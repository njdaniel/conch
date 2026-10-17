package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Operator administration of principals (issue #445): the list, hiding one,
// and hiding many.

func hidePath(id int64) string { return fmt.Sprintf("/v1/principals/%d/hide", id) }

func adminIDs(ps []schema.PrincipalAdminV1) []int64 {
	out := []int64{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func hiddenIDs(ps []schema.HiddenPrincipalV1) []int64 {
	out := []int64{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// Every new route: an operator gets the documented answer; a member or an
// agent is refused 403 and audited as access_denied, and nothing changes; no
// credential is a 401.
func TestPrincipalAdminRoutesAccess(t *testing.T) {
	routes := []struct {
		name, method, path, body string
		want                     int
	}{
		{"list", "GET", "/v1/principals", "", http.StatusOK},
		{"hide", "POST", "/v1/principals/{ghost}/hide", "", http.StatusOK},
		{"bulk hide", "POST", "/v1/principals/hide", `{"name_prefix":"gho","dry_run":false}`, http.StatusOK},
	}
	for _, rt := range routes {
		for _, who := range []string{"member", "agent", "unauthenticated", "operator"} {
			t.Run(rt.name+"/"+who, func(t *testing.T) {
				f := newAuthFixture(t, AuthRequired)
				ghost, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "ghost2")
				if err != nil {
					t.Fatal(err)
				}
				path := strings.ReplaceAll(rt.path, "{ghost}", fmt.Sprint(ghost.ID))
				tok := map[string]string{"member": f.aliceTok, "agent": f.botTok, "operator": f.rootTok}[who]
				denials := countAction(f.audit(t), "access_denied")
				rec := f.do(t, rt.method, path, tok, rt.body)
				switch who {
				case "operator":
					if rec.Code != rt.want {
						t.Fatalf("status = %d, want %d; %s", rec.Code, rt.want, rec.Body)
					}
					return
				case "unauthenticated":
					assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
				default:
					assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
					if got := countAction(f.audit(t), "access_denied"); got != denials+1 {
						t.Errorf("access_denied rows = %d, want %d", got, denials+1)
					}
				}
				// Nothing was hidden.
				if n := countAction(f.audit(t), store.AuditPrincipalHidden); n != 0 {
					t.Errorf("principal_hidden rows = %d after a refused call", n)
				}
				if p, err := f.srv.store.PrincipalByID(context.Background(), ghost.ID); err != nil || p.HiddenAt != nil {
					t.Errorf("ghost after a refused call = %+v, %v", p, err)
				}
			})
		}
	}
}

// Hiding one principal: it can no longer sign in, it is in no member list,
// its name is free for a new principal and its hidden name is not, and the
// answer shows it hidden.
func TestHidePrincipalRoute(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	msg := f.postAs(t, f.aliceTok, "general", "hello")

	assertErrorBody(t, f.do(t, "POST", hidePath(9999), f.rootTok, ""), http.StatusNotFound, "principal_not_found")
	assertErrorBody(t, f.do(t, "POST", "/v1/principals/abc/hide", f.rootTok, ""), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, "POST", hidePath(f.root.ID), f.rootTok, ""), http.StatusBadRequest, "invalid_request")

	rec := f.do(t, "POST", hidePath(f.alice.ID), f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.HidePrincipalResponseV1](t, rec).Principal
	hidden := fmt.Sprintf("hidden-%d", f.alice.ID)
	if got.ID != f.alice.ID || got.Name != hidden || got.DisabledAt == nil || got.HiddenAt == nil || got.LiveCredentials != 0 {
		t.Errorf("hidden principal = %+v", got)
	}
	assertErrorBody(t, f.do(t, "GET", "/v1/whoami", f.aliceTok, ""), http.StatusUnauthorized, "unauthenticated")

	members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", f.rootTok, ""))
	for _, m := range members.Members {
		if m.PrincipalID == f.alice.ID {
			t.Errorf("the hidden principal is still in general's members: %+v", m)
		}
	}
	// Her message stays.
	msgs := decodeBody[schema.ListMessagesResponseV2](t, f.do(t, "GET", "/v2/channels/general/messages", f.rootTok, ""))
	if !slices.ContainsFunc(msgs.Messages, func(m schema.MessageV2) bool { return m.ID == msg && m.AuthorID == f.alice.ID }) {
		t.Errorf("the hidden principal's message is gone: %+v", msgs.Messages)
	}

	// The name is free; the hidden name, and a look-alike of it, are not.
	if rec := f.do(t, "POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"alice"}`); rec.Code != http.StatusCreated {
		t.Errorf("create the freed name = %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{hidden, strings.ToUpper(hidden), "hidden-" + strings.Repeat("l", 3)} {
		body := fmt.Sprintf(`{"kind":"human","name":%q}`, name)
		assertErrorBody(t, f.do(t, "POST", "/v0/principals", f.rootTok, body), http.StatusConflict, schema.ErrorCodePrincipalExists)
		assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/name", f.bot.ID), f.rootTok, fmt.Sprintf(`{"name":%q}`, name)),
			http.StatusConflict, schema.ErrorCodePrincipalExists)
	}

	// A repeat answers the same and writes nothing.
	before := len(f.audit(t))
	rec = f.do(t, "POST", hidePath(f.alice.ID), f.rootTok, "")
	if rec.Code != http.StatusOK || decodeBody[schema.HidePrincipalResponseV1](t, rec).Principal.Name != hidden {
		t.Errorf("repeat hide = %d %s", rec.Code, rec.Body)
	}
	if after := len(f.audit(t)); after != before {
		t.Errorf("repeat hide wrote %d audit rows", after-before)
	}
	e, err := f.srv.store.LastAuditEvent(ctx, store.AuditPrincipalHidden)
	if err != nil || e.Actor != fmt.Sprintf("principal:%d", f.root.ID) || e.Detail != `name="alice"` {
		t.Errorf("principal_hidden = %+v, %v", e, err)
	}

	// Enable brings back only the ability to be issued a credential.
	if rec := f.do(t, "POST", f.enablePath(f.alice.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("enable = %d", rec.Code)
	}
	list := decodeBody[schema.ListPrincipalsResponseV1](t, f.do(t, "GET", "/v1/principals?state=active&name_prefix=hidden-", f.rootTok, ""))
	if len(list.Principals) != 1 || list.Principals[0].ID != f.alice.ID || list.Principals[0].HiddenAt != nil {
		t.Errorf("after enable = %+v", list.Principals)
	}
}

// A hidden account's webhooks are revoked with it: one that posted before the
// hide is refused after it, and stays refused when the account is enabled and
// put back in the channel, which would revive a hook a disable only paused.
func TestHideRevokesHooks(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	created := f.do(t, "POST", "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"general","principal":%d,"label":"ci"}`, f.alice.ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create hook = %d %s", created.Code, created.Body)
	}
	hook := decodeBody[schema.CreateHookResponse](t, created)
	if rec := ingest(f, t, hook.Token); rec.Code != http.StatusCreated {
		t.Fatalf("hook before the hide = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "POST", hidePath(f.alice.ID), f.rootTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, ingest(f, t, hook.Token), http.StatusNotFound, "hook_not_found")

	revoked := auditRows(t, f, "hook_revoked")
	want := fmt.Sprintf(`hook=%d channel=1 label="ci" reason=principal hidden`, hook.ID)
	if len(revoked) != 1 || revoked[0].Detail != want || revoked[0].Actor != fmt.Sprintf("principal:%d", f.root.ID) ||
		strings.Contains(revoked[0].Detail, hook.Token) {
		t.Errorf("hook_revoked rows = %+v, want one with %q", revoked, want)
	}

	// Enabled and back in the channel: the hook stays dead.
	if rec := f.do(t, "POST", f.enablePath(f.alice.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("enable = %d", rec.Code)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", 1, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, ingest(f, t, hook.Token), http.StatusNotFound, "hook_not_found")
	hooks := decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.rootTok, ""))
	if len(hooks.Hooks) != 1 || hooks.Hooks[0].RevokedAt == nil {
		t.Errorf("hooks = %+v, want the one, revoked", hooks.Hooks)
	}
}

// A reserved name is refused with the 409 a taken name gets, and a message
// that says why, on every route that sets a name. A person's own refused
// rename of that kind is not counted as a try.
func TestReservedNameMessage(t *testing.T) {
	f := newInviteFixture(t, Config{})
	const want = "names beginning with hidden- are reserved; choose another"
	check := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodePrincipalExists)
		if got := decodeBody[schema.Error](t, rec).Message; got != want {
			t.Errorf("%s: message = %q, want %q", name, got, want)
		}
	}
	link := f.createInvite(t, f.rootTok, openLink("red"))
	for _, name := range []string{"hidden-12", "HIDDEN-l2", "hіdden-12"} {
		check("create "+name, f.do(t, "POST", "/v0/principals", f.rootTok, fmt.Sprintf(`{"kind":"human","name":%q}`, name)))
		check("operator rename "+name, f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/name", f.bot.ID), f.rootTok, fmt.Sprintf(`{"name":%q}`, name)))
		check("own rename "+name, f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/name", f.alice.ID), f.aliceTok, fmt.Sprintf(`{"name":%q}`, name)))
		check("redeem "+name, f.redeem(t, link.Code, name))
	}
	if n := countAction(f.audit(t), store.AuditPrincipalRenameRefused); n != 0 {
		t.Errorf("principal_rename_refused rows = %d, want 0 for reserved names", n)
	}
	// A name that is merely taken keeps its own message.
	rec := f.do(t, "POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"Alice"}`)
	assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodePrincipalExists)
	if got := decodeBody[schema.Error](t, rec).Message; got != principalExistsMessage {
		t.Errorf("taken name message = %q", got)
	}
}

// With authentication off there is no caller: the single hide is open, as
// the disable route is, and still never hides the last operator.
func TestHidePrincipalAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	assertErrorBody(t, f.do(t, "POST", hidePath(f.root.ID), "", ""), http.StatusConflict, "last_operator")
	if rec := f.do(t, "POST", hidePath(f.alice.ID), "", ""); rec.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", rec.Code, rec.Body)
	}
	if e, err := f.srv.store.LastAuditEvent(context.Background(), store.AuditPrincipalHidden); err != nil || e.Actor != "system" {
		t.Errorf("principal_hidden = %+v, %v", e, err)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/principals/hide", "", `{"name_prefix":"bot","dry_run":true}`), http.StatusUnauthorized, "unauthenticated")
}

// A bulk hide by prefix: the dry run lists whom and changes nothing; the run
// hides them, never an operator or the caller, and writes one summary row.
func TestHidePrincipalsByPrefix(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	mk := func(kind store.PrincipalKind, name string) store.Principal {
		t.Helper()
		p, err := f.srv.store.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := mk(store.PrincipalHuman, "test-a")
	b := mk(store.PrincipalAgent, "test-b")
	mk(store.PrincipalHuman, "Test-c")
	// A second operator called test-op, with a credential, who runs the hide:
	// neither operator is selected.
	op2 := mk(store.PrincipalHuman, "test-op")
	if _, err := f.srv.store.AddChannelMember(ctx, "system", 1, op2.ID, 0); err != nil {
		t.Fatal(err)
	}
	op2Tok := (&inviteFixture{authFixture: f}).asOperator(t, op2.ID)

	for _, c := range []struct{ body, code string }{
		{`{"dry_run":true}`, "invalid_request"},
		// dry_run left out, or null, is refused, not taken for false: the
		// check below that nothing was hidden covers both.
		{`{"name_prefix":"test-"}`, "invalid_request"},
		{`{"name_prefix":"test-","dry_run":null}`, "invalid_request"},
		{`{"kind":"human","dry_run":true}`, "invalid_request"},
		{`{"name_prefix":"te","dry_run":true}`, "invalid_request"},
		{`{"name_prefix":"test-","kind":"robot","dry_run":true}`, "invalid_request"},
		{`{"name_prefix":"test-","dry_run":true,"extra":1}`, "invalid_request"},
	} {
		assertErrorBody(t, f.do(t, "POST", "/v1/principals/hide", op2Tok, c.body), http.StatusBadRequest, c.code)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/principals/hide", op2Tok, `{"invite_id":999,"dry_run":true}`), http.StatusNotFound, schema.ErrorCodeInviteNotFound)

	before := len(f.audit(t))
	rec := f.do(t, "POST", "/v1/principals/hide", op2Tok, `{"name_prefix":"test-","dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body)
	}
	dry := decodeBody[schema.HidePrincipalsResponseV1](t, rec)
	if !dry.DryRun || dry.Count != 2 || dry.Remaining != 0 || !slices.Equal(hiddenIDs(dry.Principals), []int64{a.ID, b.ID}) ||
		dry.Principals[0].Name != "test-a" {
		t.Errorf("dry run = %+v", dry)
	}
	if after := len(f.audit(t)); after != before {
		t.Errorf("the dry run wrote %d audit rows", after-before)
	}
	if p, _ := f.srv.store.PrincipalByID(ctx, a.ID); p.HiddenAt != nil || p.DisabledAt != nil {
		t.Errorf("the dry run hid test-a: %+v", p)
	}

	// Agents only.
	rec = f.do(t, "POST", "/v1/principals/hide", op2Tok, `{"name_prefix":"test-","kind":"agent","dry_run":false}`)
	if got := decodeBody[schema.HidePrincipalsResponseV1](t, rec); rec.Code != http.StatusOK || got.DryRun || !slices.Equal(hiddenIDs(got.Principals), []int64{b.ID}) {
		t.Fatalf("hide agents = %d %+v", rec.Code, got)
	}
	// Then the rest.
	rec = f.do(t, "POST", "/v1/principals/hide", op2Tok, `{"name_prefix":"test-","dry_run":false}`)
	got := decodeBody[schema.HidePrincipalsResponseV1](t, rec)
	if rec.Code != http.StatusOK || got.Count != 1 || !slices.Equal(hiddenIDs(got.Principals), []int64{a.ID}) || got.Principals[0].Name != "test-a" {
		t.Fatalf("hide = %d %+v", rec.Code, got)
	}
	for _, p := range []store.Principal{a, b} {
		got, err := f.srv.store.PrincipalByID(ctx, p.ID)
		if err != nil || got.HiddenAt == nil || got.Name != fmt.Sprintf("hidden-%d", p.ID) {
			t.Errorf("%s after the hide = %+v, %v", p.Name, got, err)
		}
	}
	for _, id := range []int64{f.root.ID, op2.ID} {
		if p, _ := f.srv.store.PrincipalByID(ctx, id); p.HiddenAt != nil || p.DisabledAt != nil {
			t.Errorf("operator %d was touched: %+v", id, p)
		}
	}
	var summaries []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditPrincipalsBulkHidden {
			summaries = append(summaries, e)
		}
	}
	want := []string{`prefix="test-" kind=agent count=1 remaining=0`, `prefix="test-" count=1 remaining=0`}
	if len(summaries) != 2 || summaries[0].Detail != want[0] || summaries[1].Detail != want[1] ||
		summaries[1].Actor != fmt.Sprintf("principal:%d", op2.ID) {
		t.Errorf("principals_bulk_hidden rows = %+v, want details %q", summaries, want)
	}
	// Nothing is left: the next run hides nobody, and says so.
	rec = f.do(t, "POST", "/v1/principals/hide", op2Tok, `{"name_prefix":"test-","dry_run":false}`)
	if got := decodeBody[schema.HidePrincipalsResponseV1](t, rec); got.Count != 0 || got.Principals == nil {
		t.Errorf("an empty run = %+v", got)
	}
}

// A bulk hide by invite takes everyone who joined through it.
func TestHidePrincipalsByInvite(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createInvite(t, f.rootTok, openLink("red"))
	other := f.createInvite(t, f.rootTok, openLink("red"))
	wren, _ := f.joinThrough(t, link.Code, "wren")
	piper, _ := f.joinThrough(t, link.Code, "piper")
	keep, _ := f.joinThrough(t, other.Code, "keep")
	rec := f.send(t, "POST", "/v1/principals/hide", f.rootTok, inviteJSON(t, schema.HidePrincipalsRequestV1{InviteID: link.Invite.ID, DryRun: new(bool)}), "")
	got := decodeBody[schema.HidePrincipalsResponseV1](t, rec)
	if rec.Code != http.StatusOK || !slices.Equal(hiddenIDs(got.Principals), []int64{wren.ID, piper.ID}) {
		t.Fatalf("hide by invite = %d %+v", rec.Code, got)
	}
	list := decodeBody[schema.ListPrincipalsResponseV1](t, f.send(t, "GET",
		fmt.Sprintf("/v1/principals?state=hidden&invite_id=%d", link.Invite.ID), f.rootTok, "", ""))
	if !slices.Equal(adminIDs(list.Principals), []int64{piper.ID, wren.ID}) {
		t.Errorf("hidden joiners = %v", adminIDs(list.Principals))
	}
	if p, _ := f.srv.store.PrincipalByID(context.Background(), keep.ID); p.HiddenAt != nil {
		t.Error("a joiner of another invite was hidden")
	}
	e, err := f.srv.store.LastAuditEvent(context.Background(), store.AuditPrincipalsBulkHidden)
	if err != nil || e.Subject != fmt.Sprintf("invite:%d", link.Invite.ID) || e.Detail != fmt.Sprintf("invite=%d count=2 remaining=0", link.Invite.ID) {
		t.Errorf("principals_bulk_hidden = %+v, %v", e, err)
	}
}

// One call acts on at most schema.MaxHidePrincipals; the answer says how
// many more match.
func TestHidePrincipalsCap(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	for i := range schema.MaxHidePrincipals + 1 {
		if _, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, fmt.Sprintf("cap-%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	got := decodeBody[schema.HidePrincipalsResponseV1](t, f.do(t, "POST", "/v1/principals/hide", f.rootTok, `{"name_prefix":"cap-","dry_run":true}`))
	if got.Count != schema.MaxHidePrincipals || len(got.Principals) != schema.MaxHidePrincipals || got.Remaining != 1 {
		t.Errorf("dry run over the cap: count %d, %d listed, remaining %d", got.Count, len(got.Principals), got.Remaining)
	}
}

// Hiding ends everything the principal holds, as the disable route does: an
// open /v2/ws socket and an events socket close at once, and the voice path
// runs before the response, rotating the room the principal held a session
// for. The LiveKit fake is the one the disable routes' voice tests use.
func TestHideClosesSocketsAndVoice(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.setInviteLimits(looseInviteLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, f.root.ID, 0); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "POST", "/v1/invites", f.rootTok, inviteJSON(t, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"ops"}, Voice: true}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	link := decodeBody[schema.CreateInviteResponseV1](t, rec)
	rec = f.do(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: link.Code, Name: "wren"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	joined := decodeBody[schema.RedeemInviteResponseV1](t, rec)
	f.tokens["wren"], f.ids["wren"] = joined.Token, joined.Principal.ID

	room := f.inUse(t, f.ops)
	f.holder(t, "wren", f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("wren"), joinedMs: 1_000, published: true},
		fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2,wren*" {
		t.Fatalf("before: %q", got)
	}

	base := wsTestServer(t, f.srv)
	v2 := startReader(ctx, dialAs(t, ctx, base, "/v2/ws?channel=ops", joined.Token))
	events := dialEvents(t, base, "/v1/events/ws?channel=ops", joined.Token)
	events.next()
	v2.waitForID(t, f.postAs(t, f.rootTok, "ops", "warmup"))

	base0 := len(f.lk.calls())
	req := httptest.NewRequest("POST", hidePath(joined.Principal.ID), nil)
	req.Header.Set("Authorization", "Bearer "+f.rootTok)
	out := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(&orderWriter{ResponseWriter: out, lk: f.lk}, req)
	if out.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", out.Code, out.Body)
	}

	closeErrContains(t, v2.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)
	if ce := events.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
		t.Errorf("events socket closed with %d %q, want 1008 %q", ce.Code, ce.Reason, reasonCredentialInvalid)
	}
	var deleted []string
	respAt := 0
	for _, c := range f.lk.calls()[base0:] {
		switch c.method {
		case "DeleteRoom":
			if respAt != 0 {
				t.Errorf("DeleteRoom came after the response")
			}
			deleted = append(deleted, c.room)
		case "http-response":
			respAt = c.seq
		}
	}
	if !slices.Equal(deleted, []string{room}) {
		t.Errorf("DeleteRoom = %v, want the ops room", deleted)
	}
	rotated := f.audits(t, store.AuditVoiceRoomRotated)
	if len(rotated) != 1 || rotated[0].Detail != "reason="+store.VoiceRotatePrincipalDisabled {
		t.Errorf("voice_room_rotated = %+v", rotated)
	}
	if got := f.who(t, f.ops); got != "" {
		t.Errorf("presence right after = %q, want nobody", got)
	}
	if ok, err := f.srv.store.IsChannelMember(ctx, f.ops.ID, joined.Principal.ID); err != nil || ok {
		t.Errorf("still a member of ops (%v)", err)
	}
}

// The list: filters, newest first, and pages that end.
func TestListPrincipalsRoute(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.bot.ID); err != nil {
		t.Fatal(err)
	}
	ghost, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "ghost2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.HidePrincipal(ctx, "system", ghost.ID, store.HideOptions{}); err != nil {
		t.Fatal(err)
	}
	all := decodeBody[schema.ListPrincipalsResponseV1](t, f.do(t, "GET", "/v1/principals", f.rootTok, ""))
	// root, alice, bot, the fixture's ghost; not the hidden ghost2.
	if len(all.Principals) != 4 || all.NextBefore != 0 || all.Principals[0].ID < all.Principals[3].ID {
		t.Fatalf("list = %+v", all)
	}
	root := all.Principals[len(all.Principals)-1]
	if root.ID != f.root.ID || root.Role != schema.RoleOperator || root.LiveCredentials != 1 || root.Kind != schema.PrincipalHuman {
		t.Errorf("root = %+v", root)
	}
	for _, c := range []struct {
		query string
		want  []int64
	}{
		{"state=active&kind=human&role=member", []int64{4, f.alice.ID}},
		{"state=disabled", []int64{f.bot.ID}},
		{"state=hidden", []int64{ghost.ID}},
		{"kind=agent", []int64{f.bot.ID}},
		{"role=operator", []int64{f.root.ID}},
		{"name_prefix=al", []int64{f.alice.ID}},
		{"state=&kind=&name_prefix=", adminIDs(all.Principals)},
	} {
		got := decodeBody[schema.ListPrincipalsResponseV1](t, f.do(t, "GET", "/v1/principals?"+c.query, f.rootTok, ""))
		if !slices.Equal(adminIDs(got.Principals), c.want) {
			t.Errorf("%s: %v, want %v", c.query, adminIDs(got.Principals), c.want)
		}
	}

	// Paging by id, newest first, until there is no next page.
	var paged []int64
	path := "/v1/principals?limit=3"
	for range 5 {
		page := decodeBody[schema.ListPrincipalsResponseV1](t, f.do(t, "GET", path, f.rootTok, ""))
		paged = append(paged, adminIDs(page.Principals)...)
		if page.NextBefore == 0 {
			break
		}
		path = fmt.Sprintf("/v1/principals?limit=3&before=%d", page.NextBefore)
	}
	if !slices.Equal(paged, adminIDs(all.Principals)) {
		t.Errorf("paged = %v, want %v", paged, adminIDs(all.Principals))
	}

	for _, q := range []string{"kind=robot", "role=root", "state=gone", "invite_id=0", "invite_id=x", "limit=0", "limit=201", "before=-1",
		"name_prefix=" + strings.Repeat("a", schema.MaxPrincipalNameLength+1)} {
		assertErrorBody(t, f.do(t, "GET", "/v1/principals?"+q, f.rootTok, ""), http.StatusBadRequest, "invalid_request")
	}
}

// Creating a principal or a channel through the API writes its audit row,
// attributed to the caller; bootstrapping does too (store and cmd/conchd
// tests). No route creates through the store's unaudited seeding calls.
func TestCreatesAreAudited(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	rec := f.do(t, "POST", "/v0/principals", f.rootTok, `{"kind":"agent","name":"deploy-bot"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create principal = %d %s", rec.Code, rec.Body)
	}
	p := decodeBody[schema.CreatePrincipalResponse](t, rec).Principal
	root := fmt.Sprintf("principal:%d", f.root.ID)
	if e, err := f.srv.store.LastAuditEvent(ctx, store.AuditPrincipalCreated); err != nil ||
		e.Actor != root || e.Subject != fmt.Sprintf("principal:%d", p.ID) || e.Detail != "kind=agent" {
		t.Errorf("principal_created = %+v, %v", e, err)
	}
	rec = f.do(t, "POST", "/v0/channels", f.rootTok, `{"name":"ops"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create channel = %d %s", rec.Code, rec.Body)
	}
	ch := decodeBody[schema.CreateChannelResponse](t, rec).Channel
	if e, err := f.srv.store.LastAuditEvent(ctx, store.AuditChannelCreated); err != nil ||
		e.Actor != root || e.Subject != fmt.Sprintf("channel:%d", ch.ID) || e.Detail != `name="ops"` {
		t.Errorf("channel_created = %+v, %v", e, err)
	}

	// A source check: the server's own code never calls the unaudited
	// seeding creates.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	unaudited := regexp.MustCompile(`\.(CreatePrincipal|CreateChannel)\(`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file) // #nosec G304 -- this package's own source
		if err != nil {
			t.Fatal(err)
		}
		if m := unaudited.Find(src); m != nil {
			t.Errorf("%s calls %s, which writes no audit row", file, m)
		}
	}
}
