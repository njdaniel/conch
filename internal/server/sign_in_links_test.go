package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The sign-in link tests (issue #493) run on the invite fixture: root the
// operator, alice a member, bot an agent.

// fakeSignInLinkCode is well-formed and matches no link.
var fakeSignInLinkCode = schema.SignInLinkCodePrefix + strings.Repeat("Q", 43)

func signInLinkPath(id int64) string { return fmt.Sprintf("/v1/principals/%d/sign-in-links", id) }

// createSignInLink makes a sign-in link over REST as root and fails the test
// unless it is made.
func (f *inviteFixture) createSignInLink(t *testing.T, principalID int64, req schema.CreateSignInLinkRequestV1) schema.CreateSignInLinkResponseV1 {
	t.Helper()
	rec := f.send(t, "POST", signInLinkPath(principalID), f.rootTok, inviteJSON(t, req), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sign-in link = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
}

func (f *inviteFixture) previewSignInLink(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/sign-in-links/preview", "", inviteJSON(t, schema.SignInLinkPreviewRequestV1{Code: code}), "")
}

func (f *inviteFixture) redeemSignInLink(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/sign-in-links/redeem", "", inviteJSON(t, schema.RedeemSignInLinkRequestV1{Code: code}), "")
}

func TestCreateSignInLinkRefusals(t *testing.T) {
	inAWeek := func(f *inviteFixture, d time.Duration) string {
		ts := schema.NewTimestamp(f.srv.now().Add(d))
		return inviteJSON(t, schema.CreateSignInLinkRequestV1{ExpiresAt: &ts})
	}
	tests := []struct {
		name     string
		setup    func(*testing.T, *inviteFixture)
		who      string // root, alice, bot or "" (no credential)
		target   func(*inviteFixture) int64
		body     func(*inviteFixture) string
		want     int
		wantCode string
	}{
		{"operator, for a member", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 201, ""},
		{"operator, a week ahead", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID },
			func(f *inviteFixture) string { return inAWeek(f, schema.MaxSignInLinkLifetime) }, 201, ""},
		{"more than a week ahead", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID },
			func(f *inviteFixture) string { return inAWeek(f, schema.MaxSignInLinkLifetime+time.Minute) }, 400, "invalid_request"},
		{"in the past", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID },
			func(f *inviteFixture) string { return inAWeek(f, -time.Minute) }, 400, "invalid_request"},
		{"a member", nil, "alice", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 403, "forbidden"},
		{"an agent", nil, "bot", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 403, "forbidden"},
		{"no credential", nil, "", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 401, "unauthenticated"},
		{"for an agent", nil, "root", func(f *inviteFixture) int64 { return f.bot.ID }, nil, 400, "invalid_request"},
		{"for an operator", nil, "root", func(f *inviteFixture) int64 { return f.root.ID }, nil, 400, "invalid_request"},
		{"for nobody", nil, "root", func(*inviteFixture) int64 { return 9999 }, nil, 404, "principal_not_found"},
		{"for a disabled member", func(t *testing.T, f *inviteFixture) {
			if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.alice.ID); err != nil {
				t.Fatal(err)
			}
		}, "root", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 409, "principal_disabled"},
		{"for a hidden member", func(t *testing.T, f *inviteFixture) {
			if _, err := f.srv.store.HidePrincipal(context.Background(), "system", f.alice.ID, store.HideOptions{}); err != nil {
				t.Fatal(err)
			}
		}, "root", func(f *inviteFixture) int64 { return f.alice.ID }, nil, 409, "principal_disabled"},
		{"unknown field", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID },
			func(*inviteFixture) string { return `{"principal_id":1}` }, 400, "invalid_request"},
		{"not JSON", nil, "root", func(f *inviteFixture) int64 { return f.alice.ID },
			func(*inviteFixture) string { return "soon" }, 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			if tt.setup != nil {
				tt.setup(t, f)
			}
			body := "{}"
			if tt.body != nil {
				body = tt.body(f)
			}
			tok := map[string]string{"root": f.rootTok, "alice": f.aliceTok, "bot": f.botTok}[tt.who]
			rec := f.send(t, "POST", signInLinkPath(tt.target(f)), tok, body, "")
			if tt.want != http.StatusCreated {
				assertErrorBody(t, rec, tt.want, tt.wantCode)
				if strings.Contains(rec.Body.String(), schema.SignInLinkCodePrefix) {
					t.Errorf("a refusal carries a code: %s", rec.Body)
				}
				if n := countAudit(t, f.authFixture, store.AuditSignInLinkCreated); n != 0 {
					t.Errorf("a refusal audited %d sign_in_link_created rows", n)
				}
				return
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			resp := decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
			if schema.ValidateSignInLinkCode(resp.Code) != nil || resp.SignInLink.PrincipalID != f.alice.ID || resp.SignInLink.CreatedBy != f.root.ID {
				t.Errorf("response = %+v", resp)
			}
		})
	}
}

// The whole chain over REST: a member signs out, so their credential no
// longer works; an operator makes them a link; whoever opens it sees whose
// account it is and gets back into that same account with a new, lasting
// credential; the link then works no more.
func TestSignInLinkEndToEnd(t *testing.T) {
	f := newInviteFixture(t, Config{})
	if rec := f.send(t, "DELETE", "/v1/credentials/current", f.aliceTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(t, "GET", "/v1/whoami", f.aliceTok, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after sign-out = %d, want 401", rec.Code)
	}

	link := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
	if got, want := link.SignInLink.ExpiresAt.Time(), f.srv.now().Add(24*time.Hour).Truncate(time.Millisecond); !got.Equal(want) {
		t.Errorf("expires = %v, want a day from now (%v)", got, want)
	}

	rec := f.previewSignInLink(t, link.Code)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview = %d (Cache-Control %q) %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	// The name, and what the link is for (issue #708): nothing else.
	if len(raw) != 2 || string(raw["name"]) != `"alice"` || string(raw["purpose"]) != `"sign_in"` {
		t.Errorf("preview = %s, want the name and the purpose alone", rec.Body)
	}

	rec = f.redeemSignInLink(t, link.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("redeem Cache-Control = %q, want no-store", got)
	}
	res := decodeBody[schema.RedeemSignInLinkResponseV1](t, rec)
	if res.Principal.ID != f.alice.ID || res.Principal.Name != "alice" || res.Credential.PrincipalID != f.alice.ID || !res.Credential.Renews {
		t.Fatalf("redeem = %+v, want a renewable credential for alice", res)
	}
	who := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", res.Token, "", ""))
	if who.ID != f.alice.ID || who.Name != "alice" || who.Role != schema.RoleMember {
		t.Errorf("whoami with the new credential = %+v, want alice", who)
	}
	// Her channels are hers, as before.
	chans := decodeBody[schema.ListChannelsResponse](t, f.send(t, "GET", "/v1/channels", res.Token, "", ""))
	var names []string
	for _, c := range chans.Channels {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "general,red" {
		t.Errorf("channels = %v, want general and red", names)
	}

	// Used: one answer for preview and redeem alike.
	assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	assertErrorBody(t, f.previewSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)

	counts := map[string]int{}
	var created, redeemed store.AuditEvent
	for _, e := range f.audit(t) {
		counts[e.Action]++
		switch e.Action {
		case store.AuditSignInLinkCreated:
			created = e
		case store.AuditSignInLinkRedeemed:
			redeemed = e
		}
	}
	if counts[store.AuditSignInLinkCreated] != 1 || counts[store.AuditSignInLinkRedeemed] != 1 || counts[auditSignInLinkRedeemRefused] != 1 {
		t.Errorf("audit counts = %v", counts)
	}
	if created.Actor != fmt.Sprintf("principal:%d", f.root.ID) || created.Subject != fmt.Sprintf("principal:%d", f.alice.ID) {
		t.Errorf("sign_in_link_created = %+v, want the operator about alice", created)
	}
	if redeemed.Subject != fmt.Sprintf("principal:%d", f.alice.ID) || redeemed.Actor != fmt.Sprintf("sign_in_link:%d", link.SignInLink.ID) {
		t.Errorf("sign_in_link_redeemed = %+v", redeemed)
	}
}

// Every code that does not work gets the same bytes, from preview and from
// redeem.
func TestInvalidSignInLinkCodesAreIndistinguishable(t *testing.T) {
	f := newInviteFixture(t, Config{})
	ctx := context.Background()
	soon := schema.NewTimestamp(f.srv.now().Add(time.Hour))
	expired := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{ExpiresAt: &soon}).Code
	used := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{}).Code
	if rec := f.redeemSignInLink(t, used); rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	other, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "other")
	if err != nil {
		t.Fatal(err)
	}
	disabled := f.createSignInLink(t, other.ID, schema.CreateSignInLinkRequestV1{}).Code
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", other.ID); err != nil {
		t.Fatal(err)
	}
	invite := f.createInvite(t, f.rootTok, openLink("general")).Code
	f.advance(2 * time.Hour)

	codes := map[string]string{
		"unknown":          fakeSignInLinkCode,
		"expired":          expired,
		"used":             used,
		"person disabled":  disabled,
		"an invite's code": invite,
		"malformed":        "conch_sil_short",
		"empty":            "",
	}
	for _, route := range []string{"preview", "redeem"} {
		var want []byte
		for name, code := range codes {
			var rec *httptest.ResponseRecorder
			if route == "preview" {
				rec = f.previewSignInLink(t, code)
			} else {
				rec = f.redeemSignInLink(t, code)
			}
			assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
			if want == nil {
				want = rec.Body.Bytes()
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Errorf("%s %s: body %q differs from %q", route, name, rec.Body.Bytes(), want)
			}
		}
	}
	// None of them issued a credential: alice has her fixture one and the one
	// the first use gave.
	creds, err := f.srv.store.ListCredentials(ctx, f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Errorf("alice has %d credentials, want 2", len(creds))
	}
}

// Preview and redeem share the invite routes' per-address buckets, and
// redeem the server-wide one, so a second kind of code does not double what
// one address may try.
func TestSignInLinkRateLimits(t *testing.T) {
	t.Run("per address, shared with invites", func(t *testing.T) {
		f := newInviteFixture(t, Config{})
		limits := looseInviteLimits()
		limits.redeemPerAddress = inviteLimit{"redeem_per_address", 2, 2 * time.Minute}
		limits.previewPerAddress = inviteLimit{"preview_per_address", 2, 2 * time.Minute}
		f.srv.setInviteLimits(limits)
		const remote = "198.51.100.9:1000"
		for _, c := range []struct {
			invite, link string
			inviteBody   any
		}{
			{"/v2/invites/redeem", "/v1/sign-in-links/redeem", schema.RedeemInviteRequestV2{Code: fakeInviteCode, Name: "x", Password: testJoinPassword}},
			{"/v1/invites/preview", "/v1/sign-in-links/preview", schema.InvitePreviewRequestV1{Code: fakeInviteCode}},
		} {
			if rec := f.send(t, "POST", c.invite, "", inviteJSON(t, c.inviteBody), remote); rec.Code != http.StatusNotFound {
				t.Fatalf("%s = %d", c.invite, rec.Code)
			}
			body := inviteJSON(t, schema.RedeemSignInLinkRequestV1{Code: fakeSignInLinkCode})
			if rec := f.send(t, "POST", c.link, "", body, remote); rec.Code != http.StatusNotFound {
				t.Fatalf("%s = %d", c.link, rec.Code)
			}
			rec := f.send(t, "POST", c.link, "", body, remote)
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			if rec.Header().Get("Retry-After") != "60" {
				t.Errorf("%s Retry-After = %q, want 60", c.link, rec.Header().Get("Retry-After"))
			}
		}
		var rows []string
		for _, e := range f.audit(t) {
			if e.Action == auditInviteRateLimited {
				rows = append(rows, e.Actor+" "+e.Subject)
				if strings.Contains(e.Detail, "198.51.100") {
					t.Errorf("the row carries the address: %s", e.Detail)
				}
			}
		}
		if want := "anonymous POST /v1/sign-in-links/redeem|anonymous POST /v1/sign-in-links/preview"; strings.Join(rows, "|") != want {
			t.Errorf("invite_rate_limited rows = %q, want %q", rows, want)
		}
	})
	t.Run("server-wide, counting only uses that work", func(t *testing.T) {
		f := newInviteFixture(t, Config{})
		limits := looseInviteLimits()
		limits.redeemGlobal = inviteLimit{"redeem_server_wide", 1, time.Minute}
		f.srv.setInviteLimits(limits)
		first := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{}).Code
		second := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{}).Code
		for range 3 { // refused at the store: they give the token back
			assertErrorBody(t, f.redeemSignInLink(t, fakeSignInLinkCode), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
		}
		if rec := f.redeemSignInLink(t, first); rec.Code != http.StatusCreated {
			t.Fatalf("first = %d %s", rec.Code, rec.Body)
		}
		assertErrorBody(t, f.redeemSignInLink(t, second), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		f.advance(time.Minute)
		if rec := f.redeemSignInLink(t, second); rec.Code != http.StatusCreated {
			t.Errorf("after the refill = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("making them, per operator, shared with invites", func(t *testing.T) {
		f := newInviteFixture(t, Config{})
		limits := looseInviteLimits()
		limits.createPerPrincipal = inviteLimit{"create_per_principal", 2, time.Hour}
		f.srv.setInviteLimits(limits)
		f.createInvite(t, f.rootTok, openLink("general"))
		f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
		rec := f.send(t, "POST", signInLinkPath(f.alice.ID), f.rootTok, "{}", "")
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	})
}

// A burst of invalid codes writes a small, fixed number of
// sign_in_link_redeem_refused rows, none carrying the code.
func TestSignInLinkRefusalAuditIsBounded(t *testing.T) {
	f := newInviteFixture(t, Config{})
	body := inviteJSON(t, schema.RedeemSignInLinkRequestV1{Code: fakeSignInLinkCode})
	before := len(f.audit(t))
	for range 1000 {
		f.send(t, "POST", "/v1/sign-in-links/redeem", "", body, "203.0.113.50:1")
	}
	n := 0
	for _, e := range f.audit(t)[before:] {
		if e.Action != auditSignInLinkRedeemRefused || e.Actor != inviteAnonymousAuditActor || e.Subject != "POST /v1/sign-in-links/redeem" ||
			!strings.HasPrefix(e.Detail, "reason=sign_in_link_invalid count=") {
			t.Errorf("unexpected row %s %s %s %s", e.Actor, e.Action, e.Subject, e.Detail)
		}
		if strings.Contains(e.Detail, fakeSignInLinkCode) || strings.Contains(e.Detail, "203.0.113") {
			t.Errorf("row carries the code or the address: %s", e.Detail)
		}
		n++
	}
	if want := thinAfter + 6; n != want {
		t.Errorf("%d rows for 1,000 refusals, want %d", n, want)
	}
}

// No code, token or full client address appears in a log line, an audit row
// or an error body.
func TestSignInLinkSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newInviteFixture(t, Config{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")})
	limits := looseInviteLimits()
	limits.redeemPerAddress = inviteLimit{"redeem_per_address", 3, time.Hour}
	f.srv.setInviteLimits(limits)
	const client = "198.51.100.77"
	const proxy = "127.0.0.1:9000"

	createRec := f.send(t, "POST", signInLinkPath(f.alice.ID), f.rootTok, "{}", "")
	link := decodeBody[schema.CreateSignInLinkResponseV1](t, createRec)
	var bodies []string
	call := func(route, code string) *httptest.ResponseRecorder {
		rec := f.send(t, "POST", route, "", inviteJSON(t, schema.RedeemSignInLinkRequestV1{Code: code}), proxy, client)
		bodies = append(bodies, rec.Body.String())
		return rec
	}
	call("/v1/sign-in-links/preview", link.Code)
	ok := call("/v1/sign-in-links/redeem", link.Code)
	if ok.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", ok.Code, ok.Body)
	}
	token := decodeBody[schema.RedeemSignInLinkResponseV1](t, ok).Token
	bodies = bodies[:len(bodies)-1] // the one body that carries the token
	call("/v1/sign-in-links/redeem", link.Code)
	call("/v1/sign-in-links/redeem", fakeSignInLinkCode)
	if rec := call("/v1/sign-in-links/redeem", fakeSignInLinkCode); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth redeem = %d, want 429", rec.Code)
	}

	secrets := map[string]string{
		"code": link.Code, "code hash": store.HashSignInLinkCode(link.Code), "unknown code": fakeSignInLinkCode,
		"token": token, "token hash": sha256Hex(token), "client address": client,
	}
	places := map[string]string{"logs": logs.buf.String(), "audit": auditText(t, f.authFixture), "error bodies": strings.Join(bodies, "\n")}
	for what, secret := range secrets {
		for where, text := range places {
			if strings.Contains(text, secret) {
				t.Errorf("the %s appears in the %s", what, where)
			}
		}
	}
}

// Disabling the person closes their link, and enabling them again does not
// bring it back.
func TestSignInLinkDiesWithADisable(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
	for _, action := range []string{"disable", "enable"} {
		if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/%s", f.alice.ID, action), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("%s = %d %s", action, rec.Code, rec.Body)
		}
	}
	assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if n := countAudit(t, f.authFixture, store.AuditSignInLinkClosed); n != 1 {
		t.Errorf("sign_in_link_closed rows = %d, want 1", n)
	}
}

// An operator closes a leaked link; the person keeps their account and the
// link gets the one invalid answer. A member is refused and audited; a used
// or closed link is a 204 that changes nothing; an unknown id is a 404.
func TestCloseSignInLinkRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
	path := fmt.Sprintf("/v1/sign-in-links/%d", link.SignInLink.ID)

	assertErrorBody(t, f.send(t, "DELETE", path, f.aliceTok, "", ""), http.StatusForbidden, "forbidden")
	denied := 0
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Subject == "DELETE /v1/sign-in-links/{id}" && e.Actor == fmt.Sprintf("principal:%d", f.alice.ID) {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("access_denied rows for the member = %d, want 1", denied)
	}
	assertErrorBody(t, f.send(t, "DELETE", path, f.botTok, "", ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.send(t, "DELETE", path, "", "", ""), http.StatusUnauthorized, "unauthenticated")
	if n := countAudit(t, f.authFixture, store.AuditSignInLinkClosed); n != 0 {
		t.Fatalf("refusals closed the link: %d rows", n)
	}

	for range 2 { // closing a closed link changes nothing
		if rec := f.send(t, "DELETE", path, f.rootTok, "", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("close = %d %s", rec.Code, rec.Body)
		}
	}
	assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if n := countAudit(t, f.authFixture, store.AuditSignInLinkClosed); n != 1 {
		t.Errorf("sign_in_link_closed rows = %d, want 1", n)
	}
	// alice keeps her account and her credential.
	if rec := f.send(t, "GET", "/v1/whoami", f.aliceTok, "", ""); rec.Code != http.StatusOK {
		t.Errorf("alice whoami = %d", rec.Code)
	}

	used := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
	if rec := f.redeemSignInLink(t, used.Code); rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d", rec.Code)
	}
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/sign-in-links/%d", used.SignInLink.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("close a used link = %d, want 204", rec.Code)
	}
	if n := countAudit(t, f.authFixture, store.AuditSignInLinkClosed); n != 1 {
		t.Errorf("closing a used link wrote a row: %d rows", n)
	}
	assertErrorBody(t, f.send(t, "DELETE", "/v1/sign-in-links/9999", f.rootTok, "", ""), http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound)
	assertErrorBody(t, f.send(t, "DELETE", "/v1/sign-in-links/x", f.rootTok, "", ""), http.StatusBadRequest, "invalid_request")
}

// With authentication off there is no operator to close a link.
func TestCloseSignInLinkAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	assertErrorBody(t, f.do(t, "DELETE", "/v1/sign-in-links/1", "", ""), http.StatusUnauthorized, "unauthenticated")
}
