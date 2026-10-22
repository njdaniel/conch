package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// inviteFixture is the auth fixture (root the operator, alice a human member,
// bot an agent, all in "general") with two more channels, "red" (alice is a
// member) and "blue" (nobody is), a clock the test moves, and invite limits
// loose enough not to matter unless a test sets its own.
type inviteFixture struct {
	*authFixture
	red, blue store.Channel
	mu        sync.Mutex
	clock     time.Time
}

func looseInviteLimits() inviteLimits {
	loose := inviteLimit{"loose", 1_000_000, time.Minute}
	return inviteLimits{previewPerAddress: loose, redeemPerAddress: loose, redeemGlobal: loose, createPerPrincipal: loose}
}

func newInviteFixture(t *testing.T, cfg Config) *inviteFixture {
	t.Helper()
	cfg.AuthMode = AuthRequired
	f := &inviteFixture{authFixture: newAuthFixtureWithConfig(t, cfg), clock: time.Now()}
	f.srv.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}
	f.srv.setInviteLimits(looseInviteLimits())
	f.srv.setLoginLimits(looseLoginLimits())
	ctx := context.Background()
	var err error
	if f.red, err = f.srv.store.CreateChannel(ctx, "red"); err != nil {
		t.Fatal(err)
	}
	if f.blue, err = f.srv.store.CreateChannel(ctx, "blue"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.red.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *inviteFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

// send makes a request from a client address, with optional X-Forwarded-For
// headers. It arrives as over HTTPS, as a deployment's requests do, so the
// routes that take a password take it (secureTransport): over TLS, or,
// with X-Forwarded-For, with the X-Forwarded-Proto a proxy adds. No other
// route reads either. The tests of the transport rule itself build their
// requests by hand.
func (f *inviteFixture) send(t *testing.T, method, path, token, body, remote string, xff ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.TLS = &tls.ConnectionState{}
	if remote != "" {
		req.RemoteAddr = remote
	}
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	if len(xff) > 0 {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func inviteJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// createInvite makes an invite over REST and fails the test unless it is made.
func (f *inviteFixture) createInvite(t *testing.T, token string, req schema.CreateInviteRequestV1) schema.CreateInviteResponseV1 {
	t.Helper()
	rec := f.send(t, "POST", "/v1/invites", token, inviteJSON(t, req), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.CreateInviteResponseV1](t, rec)
}

func (f *inviteFixture) preview(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/invites/preview", "", inviteJSON(t, schema.InvitePreviewRequestV1{Code: code}), "")
}

func (f *inviteFixture) redeem(t *testing.T, code, name string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: name}), "")
}

// testJoinPassword is the password every joiner of the v2 tests chooses: it
// passes the policy for every name they use.
const testJoinPassword = "a passphrase long enough to pass"

// redeemJSON is the body of a v2 redemption of code as name, with
// testJoinPassword.
func redeemJSON(t *testing.T, code, name string) string {
	t.Helper()
	return inviteJSON(t, schema.RedeemInviteRequestV2{Code: code, Name: name, Password: testJoinPassword})
}

// redeemWithPassword joins through POST /v2/invites/redeem (issue #707).
func (f *inviteFixture) redeemWithPassword(t *testing.T, code, name string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v2/invites/redeem", "", redeemJSON(t, code, name), "")
}

func personal(channels ...string) schema.CreateInviteRequestV1 {
	return schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: channels}
}

func openLink(channels ...string) schema.CreateInviteRequestV1 {
	return schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: channels}
}

func boolPtr(b bool) *bool { return &b }

// fakeInviteCode is well-formed and matches no invite.
var fakeInviteCode = schema.InviteCodePrefix + strings.Repeat("Q", 43)

func TestCreateInviteRefusals(t *testing.T) {
	grantAlice := func(t *testing.T, f *inviteFixture) {
		t.Helper()
		if _, err := f.srv.store.SetMayInvite(context.Background(), "system", f.alice.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		cfg      Config
		setup    func(*testing.T, *inviteFixture)
		who      string // root, alice, bot or "" (no credential)
		body     string
		want     int
		wantCode string
	}{
		{"operator, personal", Config{}, nil, "root", inviteJSON(t, personal("general")), 201, ""},
		{"operator, open link", Config{}, nil, "root", inviteJSON(t, openLink("red", "blue")), 201, ""},
		{"operator grants the right to invite", Config{}, nil, "root", inviteJSON(t, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}, MayInvite: boolPtr(true)}), 201, ""},
		{"member with the right, personal", Config{}, grantAlice, "alice", inviteJSON(t, personal("red")), 201, ""},
		{"member without the right", Config{}, nil, "alice", inviteJSON(t, personal("red")), 403, schema.ErrorCodeInviteNotAllowed},
		{"member with the right, open link", Config{}, grantAlice, "alice", inviteJSON(t, openLink("red")), 403, schema.ErrorCodeInviteNotAllowed},
		{"member passes the right on", Config{}, grantAlice, "alice", inviteJSON(t, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"red"}, MayInvite: boolPtr(true)}), 403, schema.ErrorCodeInviteGrantExceedsInviter},
		{"member names a channel they are not in", Config{}, grantAlice, "alice", inviteJSON(t, personal("blue")), 403, schema.ErrorCodeInviteGrantExceedsInviter},
		{"member names a channel that does not exist", Config{}, grantAlice, "alice", inviteJSON(t, personal("nosuch")), 403, schema.ErrorCodeInviteGrantExceedsInviter},
		{"operator names a channel that does not exist", Config{}, nil, "root", inviteJSON(t, personal("nosuch")), 404, "channel_not_found"},
		{"agent", Config{}, nil, "bot", inviteJSON(t, personal("general")), 403, schema.ErrorCodeHumanRequired},
		{"no credential", Config{}, nil, "", inviteJSON(t, personal("general")), 401, "unauthenticated"},
		{"paused", Config{}, func(t *testing.T, f *inviteFixture) {
			if _, err := f.srv.store.SetInvitesPaused(context.Background(), "system", true); err != nil {
				t.Fatal(err)
			}
		}, "root", inviteJSON(t, personal("general")), 503, schema.ErrorCodeInvitesPaused},
		{"allowance used up", Config{InviteAllowance: 1}, func(t *testing.T, f *inviteFixture) {
			grantAlice(t, f)
			f.createInvite(t, f.aliceTok, personal("red"))
		}, "alice", inviteJSON(t, personal("red")), 403, schema.ErrorCodeInviteAllowanceExceeded},
		{"unknown kind", Config{}, nil, "root", `{"kind":"forever","channels":["general"]}`, 400, "invalid_request"},
		{"no channels", Config{}, nil, "root", `{"kind":"personal","channels":[]}`, 400, "invalid_request"},
		{"unknown field", Config{}, nil, "root", `{"kind":"personal","channels":["general"],"role":"operator"}`, 400, "invalid_request"},
		{"two objects", Config{}, nil, "root", `{"kind":"personal","channels":["general"]}{}`, 400, "invalid_request"},
		{"too large", Config{}, nil, "root", `{"kind":"personal","channels":["` + strings.Repeat("x", inviteBodyMaxBytes) + `"]}`, 413, "request_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, tt.cfg)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			tok := map[string]string{"root": f.rootTok, "alice": f.aliceTok, "bot": f.botTok}[tt.who]
			rec := f.send(t, "POST", "/v1/invites", tok, tt.body, "")
			if tt.want != http.StatusCreated {
				assertErrorBody(t, rec, tt.want, tt.wantCode)
				if strings.Contains(rec.Body.String(), schema.InviteCodePrefix) {
					t.Errorf("a refusal carries a code: %s", rec.Body)
				}
				return
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			resp := decodeBody[schema.CreateInviteResponseV1](t, rec)
			if schema.ValidateInviteCode(resp.Code) != nil {
				t.Errorf("code %q is not a well-formed invite code", resp.Code)
			}
		})
	}
}

// The code is in the create response and nowhere else: not in the list, not
// in the audit trail, not in a log line.
func TestInviteCodeShownOnce(t *testing.T) {
	logs := captureLogs(t)
	f := newInviteFixture(t, Config{})
	resp := f.createInvite(t, f.rootTok, personal("general"))
	hash := store.HashInviteCode(resp.Code)

	list := f.send(t, "GET", "/v1/invites", f.rootTok, "", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d %s", list.Code, list.Body)
	}
	got := decodeBody[schema.ListInvitesResponseV1](t, list)
	if len(got.Invites) != 1 || got.Invites[0].ID != resp.Invite.ID {
		t.Fatalf("list = %+v, want the one invite", got)
	}
	for where, text := range map[string]string{"list": list.Body.String(), "logs": logs.buf.String(), "audit": auditText(t, f.authFixture)} {
		if strings.Contains(text, resp.Code) || strings.Contains(text, hash) {
			t.Errorf("the code or its hash is in the %s", where)
		}
	}
}

func auditText(t *testing.T, f *authFixture) string {
	t.Helper()
	var b strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	return b.String()
}

// Each person lists and closes their own invites; someone else's is the same
// 404 as an id that does not exist.
func TestListAndCloseOwnInvites(t *testing.T) {
	f := newInviteFixture(t, Config{})
	if _, err := f.srv.store.SetMayInvite(context.Background(), "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	rootInv := f.createInvite(t, f.rootTok, openLink("blue")).Invite
	aliceInv := f.createInvite(t, f.aliceTok, personal("red")).Invite

	// A member sees their own; an operator sees every invite (issue #438).
	for _, c := range []struct {
		tok  string
		want []int64
	}{{f.rootTok, []int64{aliceInv.ID, rootInv.ID}}, {f.aliceTok, []int64{aliceInv.ID}}} {
		got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", c.tok, "", ""))
		var ids []int64
		for _, inv := range got.Invites {
			ids = append(ids, inv.ID)
		}
		if !slices.Equal(ids, c.want) {
			t.Errorf("list = %v, want %v", ids, c.want)
		}
	}
	// A list with nothing in it is [], not null.
	other, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "other")
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := f.srv.store.CreateCredential(context.Background(), "system", other.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := f.send(t, "GET", "/v1/invites", otherTok, "", "").Body.String(); !strings.Contains(body, `"invites":[]`) {
		t.Errorf("empty list = %s, want []", body)
	}

	unknown := f.send(t, "DELETE", "/v1/invites/9999", f.aliceTok, "", "")
	someoneElses := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", rootInv.ID), f.aliceTok, "", "")
	assertErrorBody(t, unknown, http.StatusNotFound, schema.ErrorCodeInviteNotFound)
	if !bytes.Equal(unknown.Body.Bytes(), someoneElses.Body.Bytes()) || someoneElses.Code != unknown.Code {
		t.Errorf("someone else's invite = %d %s, unknown = %d %s; want the same", someoneElses.Code, someoneElses.Body, unknown.Code, unknown.Body)
	}
	assertErrorBody(t, f.send(t, "DELETE", "/v1/invites/0", f.aliceTok, "", ""), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.send(t, "DELETE", "/v1/invites/1", f.botTok, "", ""), http.StatusForbidden, schema.ErrorCodeHumanRequired)

	for range 2 { // closing a closed invite changes nothing
		if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", aliceInv.ID), f.aliceTok, "", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("close own = %d %s", rec.Code, rec.Body)
		}
	}
	got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.aliceTok, "", ""))
	if len(got.Invites) != 1 || got.Invites[0].ClosedAt == nil {
		t.Errorf("after close: %+v, want it closed", got.Invites)
	}
	if n := countAudit(t, f.authFixture, store.AuditInviteClosed); n != 1 {
		t.Errorf("invite_closed rows = %d, want 1", n)
	}
}

func countAudit(t *testing.T, f *authFixture, action string) int {
	t.Helper()
	n := 0
	for _, e := range f.audit(t) {
		if e.Action == action {
			n++
		}
	}
	return n
}

// countJoinersCreated counts the principal_created rows a redemption wrote:
// those attributed to an invite. Bootstrapping the operator writes one too
// (issue #445).
func countJoinersCreated(t *testing.T, f *authFixture) int {
	t.Helper()
	n := 0
	for _, e := range f.audit(t) {
		if e.Action == store.AuditPrincipalCreated && strings.HasPrefix(e.Actor, "invite:") {
			n++
		}
	}
	return n
}

// Preview says who invited and what a joiner would get, and nothing else.
func TestPreviewInvite(t *testing.T) {
	f := newInviteFixture(t, Config{})
	resp := f.createInvite(t, f.rootTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red", "blue"}, Voice: true})
	rec := f.preview(t, resp.Code)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "channels,expires_at,inviter,kind,voice" {
		t.Errorf("preview fields = %v, want only kind, inviter, channels, voice and expires_at", keys)
	}
	p := decodeBody[schema.InvitePreviewV1](t, rec)
	if p.Kind != schema.InviteKindOpen || p.Inviter != "root" || !p.Voice || !slices.Equal(p.Channels, []string{"red", "blue"}) ||
		!p.ExpiresAt.Time().Equal(resp.Invite.ExpiresAt.Time()) {
		t.Errorf("preview = %+v", p)
	}
	// Previewing uses nothing.
	for range 3 {
		f.preview(t, resp.Code)
	}
	got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
	if got.Invites[0].Uses != 0 {
		t.Errorf("uses after previews = %d, want 0", got.Invites[0].Uses)
	}
}

// Every invalid code gets the same bytes, from preview and from redeem.
func TestInvalidInviteCodesAreIndistinguishable(t *testing.T) {
	f := newInviteFixture(t, Config{})
	ctx := context.Background()
	if _, err := f.srv.store.SetMayInvite(ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	soon := schema.NewTimestamp(f.srv.now().Add(time.Hour))
	expired := f.createInvite(t, f.rootTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"general"}, ExpiresAt: &soon}).Code
	used := f.createInvite(t, f.rootTok, personal("general")).Code
	if rec := f.redeem(t, used, "first"); rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	closedInv := f.createInvite(t, f.rootTok, openLink("red"))
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", closedInv.Invite.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d", rec.Code)
	}
	disabledInviter := f.createInvite(t, f.aliceTok, personal("red")).Code
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)

	codes := map[string]string{
		"unknown":          fakeInviteCode,
		"expired":          expired,
		"used up":          used,
		"closed":           closedInv.Code,
		"disabled inviter": disabledInviter,
		"malformed":        "conch_inv_short",
		"empty":            "",
	}
	for _, route := range []string{"preview", "redeem"} {
		var want []byte
		for name, code := range codes {
			var rec *httptest.ResponseRecorder
			if route == "preview" {
				rec = f.preview(t, code)
			} else {
				rec = f.redeem(t, code, "newcomer")
			}
			assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeInviteInvalid)
			if want == nil {
				want = rec.Body.Bytes()
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Errorf("%s %s: body %q differs from %q", route, name, rec.Body.Bytes(), want)
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("%s %s: Content-Type %q", route, name, rec.Header().Get("Content-Type"))
			}
		}
	}
	// None of them made anybody: the one principal made is the first use.
	if n := countJoinersCreated(t, f.authFixture); n != 1 {
		t.Errorf("principal_created rows = %d, want 1", n)
	}
}

// A taken name is a 409 that does not use the invite up.
func TestRedeemTakenNameKeepsTheInvite(t *testing.T) {
	f := newInviteFixture(t, Config{})
	resp := f.createInvite(t, f.rootTok, personal("general"))
	rec := f.redeem(t, resp.Code, "alice")
	assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodePrincipalExists)
	if strings.Contains(rec.Body.String(), "alice") {
		t.Errorf("the refusal echoes the name: %s", rec.Body)
	}
	if rec := f.redeem(t, resp.Code, "newcomer"); rec.Code != http.StatusCreated {
		t.Fatalf("redeem after the 409 = %d %s; the 409 used the invite up", rec.Code, rec.Body)
	}
	// A single-use invite is now used up.
	assertErrorBody(t, f.redeem(t, resp.Code, "third"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
}

func TestRedeemNameRefusals(t *testing.T) {
	f := newInviteFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	tests := []struct {
		name, body string
		want       int
		wantCode   string
	}{
		{"blank name", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: "  "}), 400, "invalid_request"},
		{"control character", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: "sneaky\u0007name"}), 400, "invalid_request"},
		{"too long", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: strings.Repeat("n", schema.MaxPrincipalNameLength+1)}), 400, "invalid_request"},
		{"not JSON", "code=" + code, 400, "invalid_request"},
		{"unknown field", `{"code":"` + code + `","name":"x","role":"operator"}`, 400, "invalid_request"},
		{"too large", `{"code":"` + code + `","name":"` + strings.Repeat("n", inviteBodyMaxBytes) + `"}`, 413, "request_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "POST", "/v1/invites/redeem", "", tt.body, "")
			assertErrorBody(t, rec, tt.want, tt.wantCode)
			if strings.Contains(rec.Body.String(), code) || strings.Contains(rec.Body.String(), "sneaky") {
				t.Errorf("the refusal echoes the request: %s", rec.Body)
			}
		})
	}
	got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
	if got.Invites[0].Uses != 0 {
		t.Errorf("uses = %d after refusals, want 0", got.Invites[0].Uses)
	}
}

// While paused, preview and redeem answer invites_paused for any code, valid
// or not, and nothing is made.
func TestInvitesPausedOnPreviewAndRedeem(t *testing.T) {
	f := newInviteFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	if _, err := f.srv.store.SetInvitesPaused(context.Background(), "system", true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{code, fakeInviteCode} {
		assertErrorBody(t, f.preview(t, c), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
		assertErrorBody(t, f.redeem(t, c, "newcomer"), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	}
	if n := countJoinersCreated(t, f.authFixture); n != 0 {
		t.Errorf("principal_created rows = %d while paused, want 0", n)
	}
}

func TestWhoAmIMayInvite(t *testing.T) {
	f := newInviteFixture(t, Config{})
	ctx := context.Background()
	mayCode := f.createInvite(t, f.rootTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}, MayInvite: boolPtr(true)}).Code
	plainCode := f.createInvite(t, f.rootTok, openLink("red")).Code
	may := decodeBody[schema.RedeemInviteResponseV1](t, f.redeem(t, mayCode, "may"))
	plain := decodeBody[schema.RedeemInviteResponseV1](t, f.redeem(t, plainCode, "plain"))

	check := func(name, tok string, want bool) {
		t.Helper()
		got := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", tok, "", ""))
		if got.MayInvite != want {
			t.Errorf("%s: may_invite = %v, want %v", name, got.MayInvite, want)
		}
	}
	check("operator", f.rootTok, true)
	check("member", f.aliceTok, false)
	check("agent", f.botTok, false)
	check("joiner granted by an operator's invite", may.Token, true)
	check("joiner", plain.Token, false)
	if _, err := f.srv.store.SetMayInvite(ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	check("member granted the right", f.aliceTok, true)
}

func int64Ptr(n int64) *int64 { return &n }

// whoami says whether the caller holds voice and, for a person with an
// allowance, how much of it is left (issue #468).
func TestWhoAmIVoiceAndInvitesRemaining(t *testing.T) {
	f := newInviteFixture(t, Config{InviteAllowance: 3})
	ctx := context.Background()
	inviterCode := f.createInvite(t, f.rootTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}, Voice: true, MayInvite: boolPtr(true)}).Code
	plainCode := f.createInvite(t, f.rootTok, openLink("red")).Code
	inviter := decodeBody[schema.RedeemInviteResponseV1](t, f.redeem(t, inviterCode, "inviter"))
	plain := decodeBody[schema.RedeemInviteResponseV1](t, f.redeem(t, plainCode, "plain"))
	// One open personal invite counts against the allowance, and a closed one
	// does not.
	f.createInvite(t, inviter.Token, personal("red"))
	closed := f.createInvite(t, inviter.Token, personal("red"))
	if _, err := f.srv.store.CloseInvite(ctx, "system", f.root.ID, closed.Invite.ID, f.clock); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		token     string
		mayInvite bool
		voice     bool
		remaining *int64
	}{
		// Operators have no allowance, so no number.
		{"operator", f.rootTok, true, true, nil},
		{"member who may not invite", f.aliceTok, false, true, nil},
		{"member who may invite", inviter.Token, true, true, int64Ptr(2)},
		{"member without voice", plain.Token, false, false, nil},
		{"agent", f.botTok, false, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "GET", "/v1/whoami", tt.token, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("whoami = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.WhoAmIResponseV1](t, rec)
			if got.MayInvite != tt.mayInvite {
				t.Errorf("may_invite = %v, want %v", got.MayInvite, tt.mayInvite)
			}
			if got.VoiceAllowed == nil || *got.VoiceAllowed != tt.voice {
				t.Errorf("voice_allowed = %s, want %v", rec.Body, tt.voice)
			}
			if !reflect.DeepEqual(got.InvitesRemaining, tt.remaining) {
				t.Errorf("invites_remaining in %s, want %v", rec.Body, tt.remaining)
			}
		})
	}
}

// What whoami says is left follows what CreateInvite allows: it goes down
// with each personal invite, is 0 (and sent) exactly when the next is
// refused, comes back as invites close or expire, and goes away with the
// right to invite.
func TestWhoAmIInvitesRemainingFollowsCreate(t *testing.T) {
	f := newInviteFixture(t, Config{InviteAllowance: 2})
	ctx := context.Background()
	if _, err := f.srv.store.SetMayInvite(ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	remaining := func(t *testing.T) *int64 {
		t.Helper()
		return decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", f.aliceTok, "", "")).InvitesRemaining
	}
	steps := []struct {
		name string
		do   func(t *testing.T)
		want *int64
	}{
		{"before any", func(*testing.T) {}, int64Ptr(2)},
		{"after one", func(t *testing.T) { f.createInvite(t, f.aliceTok, personal("red")) }, int64Ptr(1)},
		{"after an operator's invite, which is not counted", func(t *testing.T) { f.createInvite(t, f.rootTok, personal("red")) }, int64Ptr(1)},
		{"after two", func(t *testing.T) { f.createInvite(t, f.aliceTok, personal("red")) }, int64Ptr(0)},
		{"after a refused third", func(t *testing.T) {
			rec := f.send(t, "POST", "/v1/invites", f.aliceTok, inviteJSON(t, personal("red")), "")
			assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeInviteAllowanceExceeded)
		}, int64Ptr(0)},
		{"after closing one", func(t *testing.T) {
			list := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.aliceTok, "", ""))
			id := list.Invites[0].ID
			if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", id), f.aliceTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("close = %d %s", rec.Code, rec.Body)
			}
		}, int64Ptr(1)},
		{"after the rest expire", func(*testing.T) { f.advance(8 * 24 * time.Hour) }, int64Ptr(2)},
		{"after the right is taken away", func(t *testing.T) {
			if _, err := f.srv.store.SetMayInvite(ctx, "system", f.alice.ID, false); err != nil {
				t.Fatal(err)
			}
		}, nil},
	}
	for _, step := range steps {
		step.do(t)
		if got := remaining(t); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: invites_remaining = %v, want %v", step.name, deref(got), deref(step.want))
		}
	}
}

// deref prints an optional count as its number, or "absent".
func deref(n *int64) string {
	if n == nil {
		return "absent"
	}
	return fmt.Sprint(*n)
}

// The whole chain over REST: an operator puts out an open link for two
// channels, two people join through it, each sees exactly those channels and
// cannot decide the approvals there (issue #450), and once its maker closes
// the link a third is turned away.
func TestInviteEndToEnd(t *testing.T) {
	f := newInviteFixture(t, Config{})
	defer f.srv.approvals.Close()
	ctx := context.Background()

	link := f.createInvite(t, f.rootTok, openLink("red", "blue"))
	if link.Invite.Kind != schema.InviteKindOpen || len(link.Invite.Channels) != 2 {
		t.Fatalf("invite = %+v", link.Invite)
	}
	preview := decodeBody[schema.InvitePreviewV1](t, f.preview(t, link.Code))
	if !slices.Equal(preview.Channels, []string{"red", "blue"}) || preview.Inviter != "root" {
		t.Fatalf("preview = %+v", preview)
	}

	var tokens []string
	for i, name := range []string{"wren", "piper"} {
		rec := f.send(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: link.Code, Name: name}),
			fmt.Sprintf("198.51.100.%d:4000", i+1))
		if rec.Code != http.StatusCreated {
			t.Fatalf("redeem %s = %d %s", name, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("redeem Cache-Control = %q, want no-store", got)
		}
		res := decodeBody[schema.RedeemInviteResponseV1](t, rec)
		if res.Principal.Name != name || res.Principal.Kind != schema.PrincipalHuman || res.Token == "" || res.Credential.ExpiresAt == nil {
			t.Fatalf("redeem %s = %+v", name, res)
		}
		tokens = append(tokens, res.Token)

		who := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", res.Token, "", ""))
		if who.ID != res.Principal.ID || who.Name != name || who.Kind != schema.PrincipalHuman || who.Role != schema.RoleMember || who.MayInvite {
			t.Errorf("whoami %s = %+v", name, who)
		}
		chans := decodeBody[schema.ListChannelsResponse](t, f.send(t, "GET", "/v1/channels", res.Token, "", ""))
		var names []string
		for _, c := range chans.Channels {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "blue,red" {
			t.Errorf("%s's channels = %v, want blue and red only", name, names)
		}
		// Nothing outside the invite: general is not theirs.
		if rec := f.send(t, "GET", "/v1/channels/general/messages", res.Token, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s reading general = %d, want 404", name, rec.Code)
		}
	}

	// A joiner cannot decide an approval in a channel they joined (#450).
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.red.ID, f.root.ID, 0); err != nil {
		t.Fatal(err)
	}
	rec := f.send(t, "POST", "/v1/approvals", f.rootTok, createApprovalBody(f.red.ID, 0), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create approval = %d %s", rec.Code, rec.Body)
	}
	approvalID := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	assertErrorBody(t, f.send(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", approvalID), tokens[0], decisionBody(0), ""),
		http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)

	// The maker closes the link; a third person is turned away with the
	// invalid-code answer.
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", link.Invite.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.redeem(t, link.Code, "third"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
	if len(got.Invites) != 1 || got.Invites[0].Uses != 2 || got.Invites[0].ClosedAt == nil {
		t.Errorf("invite after close = %+v, want 2 uses and closed", got.Invites)
	}
	// The ones who joined keep their access.
	if rec := f.send(t, "GET", "/v1/whoami", tokens[1], "", ""); rec.Code != http.StatusOK {
		t.Errorf("whoami after close = %d", rec.Code)
	}

	// The store's audit rows, attributed to the invite.
	counts := map[string]int{}
	for _, e := range f.audit(t) {
		counts[e.Action]++
	}
	if counts[store.AuditInviteCreated] != 1 || counts[store.AuditInviteRedeemed] != 2 || countJoinersCreated(t, f.authFixture) != 2 ||
		counts[store.AuditInviteClosed] != 1 || counts[auditInviteRedeemRefused] != 1 {
		t.Errorf("audit counts = %v", counts)
	}
}

// No invite code, credential token, redeeming name or full client address
// appears in a log line, an audit row or an error body, across every outcome
// of every route.
func TestInviteSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newInviteFixture(t, Config{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")})
	limits := looseInviteLimits()
	limits.redeemPerAddress = inviteLimit{"redeem_per_address", 3, time.Hour}
	f.srv.setInviteLimits(limits)
	const client = "198.51.100.77"
	const proxy = "127.0.0.1:9000"

	link := f.createInvite(t, f.rootTok, openLink("general"))
	var bodies []string
	redeem := func(code, name string) *httptest.ResponseRecorder {
		rec := f.send(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: name}), proxy, client)
		bodies = append(bodies, rec.Body.String())
		return rec
	}
	preview := f.send(t, "POST", "/v1/invites/preview", "", inviteJSON(t, schema.InvitePreviewRequestV1{Code: fakeInviteCode}), proxy, client)
	bodies = append(bodies, preview.Body.String())
	ok := redeem(link.Code, "secretive")
	if ok.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", ok.Code, ok.Body)
	}
	token := decodeBody[schema.RedeemInviteResponseV1](t, ok).Token
	bodies = bodies[:len(bodies)-1] // the one body that carries the token
	redeem(link.Code, "secretive")  // 409
	redeem(fakeInviteCode, "secretive")
	if rec := redeem(link.Code, "another"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth redeem = %d, want 429", rec.Code)
	}

	secrets := map[string]string{
		"code": link.Code, "code hash": store.HashInviteCode(link.Code), "unknown code": fakeInviteCode,
		"token": token, "token hash": sha256Hex(token), "name": "secretive", "client address": client,
	}
	places := map[string]string{"logs": logs.buf.String(), "audit": auditText(t, f.authFixture), "error bodies": strings.Join(bodies, "\n")}
	for what, secret := range secrets {
		for where, text := range places {
			if strings.Contains(text, secret) {
				t.Errorf("the %s appears in the %s", what, where)
			}
		}
	}
	// The truncated network may be logged, and is, for the refusal.
	if !strings.Contains(logs.buf.String(), "198.51.100.0/24") {
		t.Errorf("the rate-limit log line does not name the client's network: %s", logs.buf.String())
	}
}
