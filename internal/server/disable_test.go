package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

const reasonRevoked = "credential no longer valid"

func (f *authFixture) disablePath(id int64) string {
	return fmt.Sprintf("/v1/principals/%d/disable", id)
}
func (f *authFixture) enablePath(id int64) string { return fmt.Sprintf("/v1/principals/%d/enable", id) }
func (f *authFixture) revokeAllPath(id int64) string {
	return fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", id)
}

// newCred issues a credential for principalID through the REST API as root.
func (f *authFixture) newCred(t *testing.T, principalID int64) (int64, string) {
	t.Helper()
	rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", principalID), f.rootTok, `{"label":"extra"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create credential = %d %s", rec.Code, rec.Body)
	}
	r := decodeBody[schema.CreateCredentialResponseV1](t, rec)
	return r.Credential.ID, r.Token
}

func closeErrContains(t *testing.T, err error, status websocket.StatusCode, reason string) {
	t.Helper()
	if websocket.CloseStatus(err) != status {
		t.Errorf("close status = %v (%v), want %v", websocket.CloseStatus(err), err, status)
	}
	if reason != "" && (err == nil || !strings.Contains(err.Error(), reason)) {
		t.Errorf("close error = %v, want reason %q", err, reason)
	}
}

func TestDisableEndpointsAccess(t *testing.T) {
	tests := []struct {
		name   string
		path   func(f *authFixture) string
		setup  func(t *testing.T, f *authFixture)
		wantOp int
	}{
		{"disable", func(f *authFixture) string { return f.disablePath(f.alice.ID) }, nil, http.StatusNoContent},
		{"enable", func(f *authFixture) string { return f.enablePath(f.alice.ID) }, nil, http.StatusNoContent},
		{"revoke-all", func(f *authFixture) string { return f.revokeAllPath(f.alice.ID) }, nil, http.StatusOK},
	}
	callers := []struct {
		who  string
		tok  func(f *authFixture) string
		want func(op int) int
	}{
		{"unauthenticated", func(*authFixture) string { return "" }, func(int) int { return 401 }},
		{"member", func(f *authFixture) string { return f.botTok }, func(int) int { return 403 }},
		{"operator", func(f *authFixture) string { return f.rootTok }, func(op int) int { return op }},
	}
	for _, tt := range tests {
		for _, c := range callers {
			t.Run(tt.name+"/"+c.who, func(t *testing.T) {
				f := newAuthFixture(t, AuthRequired)
				rec := f.do(t, "POST", tt.path(f), c.tok(f), "")
				if rec.Code != c.want(tt.wantOp) {
					t.Fatalf("status = %d, want %d; %s", rec.Code, c.want(tt.wantOp), rec.Body)
				}
				if c.who != "operator" {
					// Denied callers change nothing: alice still authenticates.
					if r := f.do(t, "GET", "/v1/whoami", f.aliceTok, ""); r.Code != 200 {
						t.Errorf("alice whoami after denied call = %d", r.Code)
					}
				}
				if c.who == "member" && countAction(f.audit(t), "access_denied") != 1 {
					t.Errorf("member denial not audited")
				}
			})
		}
	}
}

func TestDisableEndpointBehaviour(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)

	// Unknown principal and bad ids.
	for _, p := range []string{f.disablePath(9999), f.enablePath(9999), f.revokeAllPath(9999)} {
		assertErrorBody(t, f.do(t, "POST", p, f.rootTok, ""), 404, "principal_not_found")
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/principals/abc/disable", f.rootTok, ""), 400, "invalid_request")

	// Last operator.
	assertErrorBody(t, f.do(t, "POST", f.disablePath(f.root.ID), f.rootTok, ""), 409, "last_operator")
	if r := f.do(t, "GET", "/v1/whoami", f.rootTok, ""); r.Code != 200 {
		t.Fatalf("root disabled despite 409: %d", r.Code)
	}

	// Disable alice: 204, token dead, audit attributed to root.
	_, tok2 := f.newCred(t, f.alice.ID)
	if rec := f.do(t, "POST", f.disablePath(f.alice.ID), f.rootTok, ""); rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("disable = %d %q", rec.Code, rec.Body)
	}
	for _, tok := range []string{f.aliceTok, tok2} {
		assertErrorBody(t, f.do(t, "GET", "/v1/whoami", tok, ""), 401, "unauthenticated")
	}
	before := len(f.audit(t))
	if rec := f.do(t, "POST", f.disablePath(f.alice.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatalf("repeat disable = %d", rec.Code)
	}
	if after := len(f.audit(t)); after != before {
		t.Errorf("repeat disable wrote %d audit events", after-before)
	}
	var disabledEvents, revokedEvents int
	for _, e := range f.audit(t) {
		switch e.Action {
		case "principal_disabled":
			disabledEvents++
			if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) || e.Subject != fmt.Sprintf("principal:%d", f.alice.ID) {
				t.Errorf("principal_disabled = %+v", e)
			}
		case "credential_revoked":
			if strings.Contains(e.Detail, "reason=principal disabled") {
				revokedEvents++
				if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) {
					t.Errorf("credential_revoked actor = %q", e.Actor)
				}
			}
		}
	}
	if disabledEvents != 1 || revokedEvents != 2 {
		t.Errorf("principal_disabled = %d (want 1), credential_revoked by disable = %d (want 2)", disabledEvents, revokedEvents)
	}

	// No credentials for a disabled principal.
	assertErrorBody(t, f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID), f.rootTok, `{"label":"x"}`), 409, "principal_disabled")
	creds := decodeBody[schema.ListCredentialsResponseV1](t, f.do(t, "GET", fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID), f.rootTok, ""))
	assertErrorBody(t, f.do(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", creds.Credentials[0].ID), f.rootTok, ""), 409, "principal_disabled")

	// Enable: 204, idempotent, old tokens stay dead, a new one works.
	if rec := f.do(t, "POST", f.enablePath(f.alice.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatalf("enable = %d", rec.Code)
	}
	before = len(f.audit(t))
	if rec := f.do(t, "POST", f.enablePath(f.alice.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatalf("repeat enable = %d", rec.Code)
	}
	if after := len(f.audit(t)); after != before {
		t.Errorf("repeat enable wrote audit events")
	}
	for _, tok := range []string{f.aliceTok, tok2} {
		assertErrorBody(t, f.do(t, "GET", "/v1/whoami", tok, ""), 401, "unauthenticated")
	}
	_, fresh := f.newCred(t, f.alice.ID)
	if rec := f.do(t, "GET", "/v1/whoami", fresh, ""); rec.Code != 200 {
		t.Errorf("fresh credential after enable = %d", rec.Code)
	}
	if countAction(f.audit(t), "principal_enabled") != 1 {
		t.Errorf("principal_enabled events != 1")
	}

	// Revoke-all: count, body shape, principal stays enabled.
	_, extra := f.newCred(t, f.alice.ID)
	rec := f.do(t, "POST", f.revokeAllPath(f.alice.ID), f.rootTok, "")
	if rec.Code != 200 {
		t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.RevokeAllCredentialsResponseV1](t, rec); got.Revoked != 2 {
		t.Errorf("revoked = %d, want 2", got.Revoked)
	}
	for _, tok := range []string{fresh, extra} {
		assertErrorBody(t, f.do(t, "GET", "/v1/whoami", tok, ""), 401, "unauthenticated")
	}
	rec = f.do(t, "POST", f.revokeAllPath(f.alice.ID), f.rootTok, "")
	if got := decodeBody[schema.RevokeAllCredentialsResponseV1](t, rec); rec.Code != 200 || got.Revoked != 0 {
		t.Errorf("second revoke-all = %d %+v", rec.Code, got)
	}
	_, again := f.newCred(t, f.alice.ID)
	if r := f.do(t, "GET", "/v1/whoami", again, ""); r.Code != 200 {
		t.Errorf("credential after revoke-all = %d", r.Code)
	}
}

// TestDisabledTokenIndistinguishable: a disabled principal's token gets the
// byte-identical response an unknown token gets, on REST and WebSocket.
func TestDisabledTokenIndistinguishable(t *testing.T) {
	f := newMemberFixture(t)
	base := wsTestServer(t, f.srv)
	if rec := f.do(t, "POST", f.disablePath(f.alice.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	unknown := schema.CredentialTokenPrefix + strings.Repeat("Z", 43)
	rest := []struct{ method, path string }{
		{"GET", "/v1/whoami"}, {"GET", "/v1/channels"}, {"GET", "/v1/channels/alpha/messages"},
	}
	for _, r := range rest {
		got := f.callREST(t, r.method, r.path, f.aliceTok, "")
		want := f.callREST(t, r.method, r.path, unknown, "")
		if got != want || got.status != 401 {
			t.Errorf("%s: disabled %+v != unknown %+v", r.path, got, want)
		}
	}
	for _, p := range []string{"/v0/ws?channel=alpha", "/v1/ws?channel=alpha"} {
		got := callWS(t, base, p, f.aliceTok)
		want := callWS(t, base, p, unknown)
		if got != want || got.status != 401 {
			t.Errorf("%s: disabled %+v != unknown %+v", p, got, want)
		}
	}
}

func TestDisableClosesSocketsEndToEnd(t *testing.T) {
	for _, version := range []string{"v0", "v1"} {
		for _, action := range []string{"disable", "revoke-all"} {
			t.Run(version+"/"+action, func(t *testing.T) {
				f := newMemberFixture(t)
				base := wsTestServer(t, f.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if _, err := f.srv.store.AddChannelMember(ctx, "system", f.alpha.ID, f.root.ID, 0); err != nil {
					t.Fatal(err)
				}
				ws := func(ch string) string { return "/" + version + "/ws?channel=" + ch }
				a1 := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.aliceTok))
				a2 := startReader(ctx, dialAs(t, ctx, base, ws("general"), f.aliceTok))
				rootReader := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.rootTok))
				warm := f.postAs(t, f.rootTok, "alpha", "warmup")
				a1.waitForID(t, warm)
				rootReader.waitForID(t, warm)

				path := f.disablePath(f.alice.ID)
				if action == "revoke-all" {
					path = f.revokeAllPath(f.alice.ID)
				}
				if rec := f.do(t, "POST", path, f.rootTok, ""); rec.Code != 204 && rec.Code != 200 {
					t.Fatalf("%s = %d", action, rec.Code)
				}
				for _, r := range []*wsReader{a1, a2} {
					closeErrContains(t, r.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)
				}
				after := f.postAs(t, f.rootTok, "alpha", "after")
				f.postAs(t, f.rootTok, "general", "after-general")
				rootReader.waitForID(t, after)
				for _, r := range []*wsReader{a1, a2} {
					for _, id := range r.seen() {
						if id >= after {
							t.Errorf("closed socket received message %d posted after the action", id)
						}
					}
				}
				assertErrorBody(t, f.do(t, "GET", "/v1/whoami", f.aliceTok, ""), 401, "unauthenticated")
				if action == "revoke-all" {
					// Still enabled: a new credential works and can open a socket.
					_, tok := f.newCred(t, f.alice.ID)
					dialAs(t, ctx, base, ws("alpha"), tok)
					if rec := f.do(t, "GET", "/v1/whoami", tok, ""); rec.Code != 200 {
						t.Errorf("new credential = %d", rec.Code)
					}
				}
			})
		}
	}
}

// TestSocketRechecksItsCredential: a socket closes within the (shortened)
// interval after its own credential alone is revoked or expires, while a
// second socket of the same principal on another credential stays open.
func TestSocketRechecksItsCredential(t *testing.T) {
	for _, version := range []string{"v0", "v1"} {
		for _, cause := range []string{"revoked", "expired"} {
			t.Run(version+"/"+cause, func(t *testing.T) {
				f := newMemberFixture(t)
				f.srv.credRecheckInterval = 25 * time.Millisecond
				base := wsTestServer(t, f.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()

				var credA int64
				var tokA string
				if cause == "revoked" {
					credA, tokA = f.newCred(t, f.alice.ID)
				} else {
					exp := time.Now().Add(400 * time.Millisecond)
					c, tok, err := f.srv.store.CreateCredential(ctx, "system", f.alice.ID, "short", &exp)
					if err != nil {
						t.Fatal(err)
					}
					credA, tokA = c.ID, tok
				}
				_, tokB := f.newCred(t, f.alice.ID)
				path := "/" + version + "/ws?channel=alpha"
				a := startReader(ctx, dialAs(t, ctx, base, path, tokA))
				b := startReader(ctx, dialAs(t, ctx, base, path, tokB))
				if cause == "revoked" {
					time.Sleep(100 * time.Millisecond) // several ticks: still open
					if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", credA), f.rootTok, ""); rec.Code != 204 {
						t.Fatalf("revoke = %d", rec.Code)
					}
				}
				closeErrContains(t, a.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)
				// B is unaffected and still receives.
				id := f.postAs(t, tokB, "alpha", "still here")
				b.waitForID(t, id)
				select {
				case <-b.done:
					t.Errorf("socket on the other credential closed: %v", b.err)
				default:
				}
			})
		}
	}
}

// TestSocketRecheckFailsClosedOnStoreError: if the liveness read fails the
// socket closes, with a generic reason that does not name the cause.
func TestSocketRecheckFailsClosedOnStoreError(t *testing.T) {
	f := newMemberFixture(t)
	f.srv.credRecheckInterval = 25 * time.Millisecond
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := startReader(ctx, dialAs(t, ctx, base, "/v1/ws?channel=alpha", f.aliceTok))
	time.Sleep(60 * time.Millisecond)
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	err := r.waitClosed(t)
	closeErrContains(t, err, websocket.StatusInternalError, "credential check failed")
}

// TestAuthOffStartsNoRecheck: with no caller there is no credential to check;
// a socket stays open across many (shortened) intervals and endpoints work.
func TestAuthOffStartsNoRecheck(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	f.srv.credRecheckInterval = 10 * time.Millisecond
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, base+"/v1/ws?channel=general", nil) //nolint:bodyclose // upgraded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	r := startReader(ctx, conn)
	time.Sleep(150 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatalf("auth-off socket closed: %v", r.err)
	default:
	}

	// The endpoints work with no caller and audit as system.
	if rec := f.do(t, "POST", f.disablePath(f.alice.ID), "", ""); rec.Code != 204 {
		t.Fatalf("disable = %d", rec.Code)
	}
	if rec := f.do(t, "POST", f.enablePath(f.alice.ID), "", ""); rec.Code != 204 {
		t.Fatalf("enable = %d", rec.Code)
	}
	if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID), "", `{"label":"x"}`); rec.Code != 201 {
		t.Fatalf("create credential = %d", rec.Code)
	}
	rec := f.do(t, "POST", f.revokeAllPath(f.alice.ID), "", "")
	if got := decodeBody[schema.RevokeAllCredentialsResponseV1](t, rec); rec.Code != 200 || got.Revoked != 1 {
		t.Fatalf("revoke-all = %d %+v", rec.Code, got)
	}
	for _, e := range f.audit(t) {
		if (e.Action == "principal_disabled" || e.Action == "principal_enabled") && e.Actor != "system" {
			t.Errorf("%s actor = %q", e.Action, e.Actor)
		}
	}
	// The unauthenticated socket was not dropped (principal 0 is never targeted).
	f.srv.hub.DropPrincipalAll(0)
	select {
	case <-r.done:
		t.Fatalf("auth-off socket closed by disable: %v", r.err)
	default:
	}
}

// TestDisableNoTokenLeaks: nothing the new paths log, return, or audit
// contains a token.
func TestDisableNoTokenLeaks(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newAuthFixture(t, AuthRequired)
	_, extra := f.newCred(t, f.alice.ID)
	secrets := []string{f.rootTok, f.aliceTok, f.botTok, extra}
	var out []string
	for _, step := range []struct{ path, tok string }{
		{f.disablePath(f.alice.ID), f.rootTok},
		{f.disablePath(f.alice.ID), f.aliceTok},
		{f.enablePath(f.alice.ID), f.rootTok},
		{f.revokeAllPath(f.alice.ID), f.rootTok},
		{f.revokeAllPath(f.alice.ID), f.botTok},
		{fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID), f.rootTok},
	} {
		rec := f.do(t, "POST", step.path, step.tok, `{"label":"x"}`)
		out = append(out, rec.Body.String())
		for k, v := range rec.Header() {
			out = append(out, k+": "+strings.Join(v, ","))
		}
	}
	for _, e := range f.audit(t) {
		out = append(out, e.Actor+" "+e.Action+" "+e.Subject+" "+e.Detail)
	}
	out = append(out, logs.buf.String())
	for _, text := range out {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("token leaked into %q", text)
			}
		}
	}
}
