package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The passwords of the recovery tests. Each passes the policy.
const (
	wrenPassword     = "my own long and private passphrase"
	newAlicePassword = "a brand new and private passphrase"
	annPassword      = "a long and private voice passphrase"
)

// recoveryFixture is the login fixture (alice and root have passwords) with
// wren, a member who predates passwords: a renewable browser session and no
// password, as an invite left everyone before migration 32.
type recoveryFixture struct {
	*loginFixture
	wren     store.Principal
	wrenCred int64
	wrenTok  string
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{loginFixture: newLoginFixture(t, Config{})}
	ctx := context.Background()
	var err error
	if f.wren, err = f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "wren"); err != nil {
		t.Fatal(err)
	}
	f.wrenCred, f.wrenTok = f.session(t, f.wren.ID)
	return f
}

// session issues id a renewable credential, as a browser sign-in is.
func (f *recoveryFixture) session(t *testing.T, id int64) (int64, string) {
	t.Helper()
	c, tok, err := f.srv.store.CreateRenewableCredential(context.Background(), "system", id, "browser", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, tok
}

// link has root make a link of purpose for id through the operator route,
// and returns it with its code.
func (f *recoveryFixture) link(t *testing.T, purpose schema.SignInLinkPurpose, id int64) schema.CreateSignInLinkResponseV1 {
	t.Helper()
	rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", id), f.rootTok,
		inviteJSON(t, schema.CreateSignInLinkRequestV1{Purpose: purpose}), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("make a %s link = %d %s", purpose, rec.Code, rec.Body)
	}
	return decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
}

func (f *recoveryFixture) enroll(t *testing.T, token, password, proof string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/password/mine", token, inviteJSON(t, schema.SetPasswordRequestV1{Password: password, Proof: proof}), "")
}

func (f *recoveryFixture) change(t *testing.T, token, current, password string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "PUT", "/v1/password/mine", token, inviteJSON(t, schema.ChangePasswordRequestV1{Current: current, Password: password}), "")
}

func (f *recoveryFixture) reset(t *testing.T, code, password string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/password/reset", "", inviteJSON(t, schema.ResetPasswordRequestV1{Code: code, Password: password}), "")
}

func (f *recoveryFixture) reauth(t *testing.T, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/reauth", token, inviteJSON(t, schema.ReauthRequestV1{Password: password}), "")
}

// signIn signs name in with password and returns the credential and token,
// failing the test on anything but a member's signed_in.
func (f *recoveryFixture) signIn(t *testing.T, name, password string) (int64, string) {
	t.Helper()
	rec := f.login(t, name, password, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s = %d %s", name, rec.Code, rec.Body)
	}
	resp := decodeBody[schema.LoginResponseV1](t, rec)
	return resp.Credential.ID, resp.Token
}

func (f *recoveryFixture) whoamiAuth(t *testing.T, token string) *schema.AuthStateV1 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/whoami", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.WhoAmIResponseV1](t, rec).Auth
}

// hasPassword reports whether id has a password record.
func (f *recoveryFixture) hasPassword(t *testing.T, id int64) bool {
	t.Helper()
	return f.queryString(t, fmt.Sprintf("SELECT COUNT(*) FROM passwords WHERE principal_id = %d", id)) == "1"
}

// linkSpent reports whether the link with id has been used.
func (f *recoveryFixture) linkSpent(t *testing.T, id int64) bool {
	t.Helper()
	return f.queryString(t, fmt.Sprintf("SELECT used_at IS NOT NULL OR closed_at IS NOT NULL FROM sign_in_links WHERE id = %d", id)) == "1"
}

// spentLinks counts the links a password route spent: used or closed with
// no sign_in_link_closed row, which an operator's close and a bump write and
// a spend does not.
func (f *recoveryFixture) spentLinks(t *testing.T) string {
	t.Helper()
	return f.queryString(t, `SELECT COUNT(*) FROM sign_in_links l WHERE (l.used_at IS NOT NULL OR l.closed_at IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM audit_events e WHERE e.action = 'sign_in_link_closed' AND e.detail LIKE 'sign_in_link=' || l.id || ' %')`)
}

func (f *recoveryFixture) status(t *testing.T, token string) int {
	t.Helper()
	return f.do(t, "GET", "/v1/whoami", token, "").Code
}

// actions returns, in order, every audit action whose subject or actor is
// subject.
func (f *recoveryFixture) actions(t *testing.T, subject string) []string {
	t.Helper()
	var out []string
	for _, e := range f.audit(t) {
		if e.Subject == subject || e.Actor == subject {
			out = append(out, e.Action)
		}
	}
	return out
}

// A member who predates passwords sets a first one with an operator's
// enrollment link: until then whoami says setup is incomplete; the link is a
// proof for that one member only, has no landing page and signs nobody in;
// the session it was used on is rotated, nothing else of the member's ends,
// and the member can then sign in with the password anywhere.
func TestEnrollFirstPassword(t *testing.T) {
	f := newRecoveryFixture(t)
	if got := f.whoamiAuth(t, f.wrenTok); got == nil || got.PasswordSet || got.SetupComplete || got.Scope != schema.CredentialScopeFull {
		t.Fatalf("whoami auth before = %+v, want full scope, no password, setup incomplete", got)
	}
	_, otherTok := f.session(t, f.wren.ID)

	link := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID)
	if link.SignInLink.Purpose != schema.SignInLinkPurposeEnrollment {
		t.Errorf("link purpose = %q, want enrollment", link.SignInLink.Purpose)
	}
	if got, want := link.SignInLink.ExpiresAt.Time(), f.srv.now().Add(schema.DefaultRestrictedLinkLifetime).Truncate(time.Millisecond); !got.Equal(want) {
		t.Errorf("expires = %v, want ten minutes from now (%v)", got, want)
	}
	// No landing page, and no sign-in.
	assertErrorBody(t, f.previewSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if f.linkSpent(t, link.SignInLink.ID) {
		t.Fatal("a refused redeem spent the link")
	}

	rec := f.enroll(t, f.wrenTok, wrenPassword, link.Code)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("enroll = %d (Cache-Control %q) %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	resp := decodeBody[schema.PasswordUpdatedResponseV1](t, rec)
	if resp.Credential == nil || resp.Token == "" || resp.Revoked != 0 || resp.Credential.Label != "browser" || !resp.Credential.Renews {
		t.Fatalf("enroll response = %s, want the session rotated (label and renewal kept) and nothing else revoked", rec.Body)
	}
	if got := f.status(t, f.wrenTok); got != http.StatusUnauthorized {
		t.Errorf("the session it was sent on = %d after, want 401: it is rotated", got)
	}
	if got := f.status(t, otherTok); got != http.StatusOK {
		t.Errorf("wren's other session = %d after, want 200: a first password ends nothing", got)
	}
	if got := f.whoamiAuth(t, resp.Token); got == nil || !got.PasswordSet || !got.SetupComplete {
		t.Errorf("whoami auth after = %+v, want password set and setup complete", got)
	}
	if !f.linkSpent(t, link.SignInLink.ID) {
		t.Error("the enrollment link is not spent")
	}
	f.signIn(t, "wren", wrenPassword)

	subject := fmt.Sprintf("principal:%d", f.wren.ID)
	var trail []string
	for _, a := range f.actions(t, subject) {
		switch a {
		case store.AuditEnrollmentIssued, store.AuditPasswordSet, "credential_rotated", store.AuditAuthGenerationBumped:
			trail = append(trail, a)
		}
	}
	if want := []string{store.AuditEnrollmentIssued, store.AuditPasswordSet, "credential_rotated"}; strings.Join(trail, " ") != strings.Join(want, " ") {
		t.Errorf("audit trail = %v, want %v", trail, want)
	}
	for _, e := range auditRows(t, f.authFixture, store.AuditPasswordSet) {
		if e.Subject == subject && e.Detail != fmt.Sprintf("via=enrollment replaced=false sign_in_link=%d", link.SignInLink.ID) {
			t.Errorf("password_set detail = %q", e.Detail)
		}
	}
}

// Every proof that is not a usable enrollment link made for the caller sets
// no password and spends nothing, and neither does any caller the route is
// not for.
func TestEnrollRefusals(t *testing.T) {
	tests := []struct {
		name string
		// do makes the request; it may prepare the fixture first.
		do     func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"no proof", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, wrenPassword, "")
		}, http.StatusForbidden, schema.ErrorCodeReauthRequired},
		{"a proof made for another member", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			kit, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "kit")
			if err != nil {
				t.Fatal(err)
			}
			return f.enroll(t, f.wrenTok, wrenPassword, f.link(t, schema.SignInLinkPurposeEnrollment, kit.ID).Code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a reset link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, wrenPassword, f.link(t, schema.SignInLinkPurposeReset, f.wren.ID).Code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a sign-in link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, wrenPassword, f.link(t, schema.SignInLinkPurposeSignIn, f.wren.ID).Code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a malformed proof", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, wrenPassword, "conch_sil_short")
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an unknown proof", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, wrenPassword, schema.SignInLinkCodePrefix+strings.Repeat("A", 43))
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an expired proof", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code
			f.advance(schema.DefaultRestrictedLinkLifetime + time.Second)
			return f.enroll(t, f.wrenTok, wrenPassword, code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a proof made before a sign-out everywhere", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code
			if rec := f.send(t, "POST", f.revokeAllPath(f.wren.ID), f.wrenTok, "", ""); rec.Code != http.StatusOK {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
			_, tok := f.session(t, f.wren.ID)
			return f.enroll(t, tok, wrenPassword, code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a proof whose maker is no longer an operator", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code
			f.exec(t, "UPDATE principals SET role = 'member' WHERE id = ?", f.root.ID)
			return f.enroll(t, f.wrenTok, wrenPassword, code)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an unacceptable password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, "correct horse battery staple", f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code)
		}, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"a password too short", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.wrenTok, "short", f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code)
		}, http.StatusBadRequest, "invalid_request"},
		{"an operator on an ordinary credential", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			f.exec(t, "DELETE FROM passwords WHERE principal_id = ?", f.root.ID)
			return f.enroll(t, f.rootTok, rootPassword, "")
		}, http.StatusForbidden, schema.ErrorCodeSetupOnHost},
		{"a member who has a password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.aliceTok, newAlicePassword, f.link(t, schema.SignInLinkPurposeEnrollment, f.alice.ID).Code)
		}, http.StatusConflict, schema.ErrorCodePasswordAlreadySet},
		{"an agent", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.enroll(t, f.botTok, wrenPassword, "")
		}, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"a guest", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			g := newGuest(t, f.srv, "", time.Hour, 2*time.Hour)
			return f.enroll(t, g.Token, wrenPassword, "")
		}, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"plain HTTP", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.do(t, "POST", "/v1/password/mine", f.wrenTok,
				inviteJSON(t, schema.SetPasswordRequestV1{Password: wrenPassword, Proof: f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code}))
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			assertErrorBody(t, tt.do(t, f), tt.status, tt.code)
			if f.hasPassword(t, f.wren.ID) {
				t.Error("wren has a password after a refusal")
			}
			if n := f.spentLinks(t); n != "0" {
				t.Errorf("%s links spent by a refusal", n)
			}
		})
	}
}

// A password the policy refuses leaves the link as it was, so the person
// tries another with the same link.
func TestRefusedPasswordKeepsTheLink(t *testing.T) {
	f := newRecoveryFixture(t)
	enroll := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code
	assertErrorBody(t, f.enroll(t, f.wrenTok, "correct horse battery staple", enroll), http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable)
	if rec := f.enroll(t, f.wrenTok, wrenPassword, enroll); rec.Code != http.StatusOK {
		t.Errorf("enroll after a refused password = %d %s", rec.Code, rec.Body)
	}
	reset := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
	assertErrorBody(t, f.reset(t, reset, "alice alice alice alice"), http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable)
	if rec := f.reset(t, reset, newAlicePassword); rec.Code != http.StatusOK {
		t.Errorf("reset after a refused password = %d %s", rec.Code, rec.Body)
	}
}

// whoami's setup_complete is a member's password, and an operator's password
// and confirmed second factor together.
func TestWhoAmISetupComplete(t *testing.T) {
	f := newRecoveryFixture(t)
	if got := f.whoamiAuth(t, f.aliceTok); got == nil || !got.PasswordSet || !got.SetupComplete || got.TOTPEnrolled {
		t.Errorf("alice = %+v, want password set and setup complete", got)
	}
	if got := f.whoamiAuth(t, f.rootTok); got == nil || !got.PasswordSet || got.TOTPEnrolled || got.SetupComplete {
		t.Errorf("root with a password only = %+v, want setup incomplete", got)
	}
	f.enrollTOTP(t, f.root.ID)
	if got := f.whoamiAuth(t, f.rootTok); got == nil || !got.TOTPEnrolled || !got.SetupComplete {
		t.Errorf("root with both = %+v, want setup complete", got)
	}
	if got := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", f.botTok, "")).Auth; got != nil {
		t.Errorf("an agent's whoami auth = %+v, want none", got)
	}
	g := newGuest(t, f.srv, "", time.Hour, 2*time.Hour)
	if got := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", g.Token, "")).Auth; got != nil {
		t.Errorf("a guest's whoami auth = %+v, want none", got)
	}
}

// A change with the current password stores the new one and ends everything
// else of the member's: every other credential, open link and
// recent-password proof; live sockets close (TestPasswordChangeEndsLiveAccess).
// The session it was made from is rotated and keeps its label and lifetime.
func TestChangePassword(t *testing.T) {
	f := newRecoveryFixture(t)
	credA, tokA := f.signIn(t, "alice", alicePassword)
	_, tokB := f.signIn(t, "alice", alicePassword)
	open := f.link(t, schema.SignInLinkPurposeSignIn, f.alice.ID)
	if rec := f.reauth(t, tokA, alicePassword); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}

	rec := f.change(t, tokA, alicePassword, newAlicePassword)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.PasswordUpdatedResponseV1](t, rec)
	// The fixture's credential and B.
	if resp.Credential == nil || resp.Token == "" || resp.Revoked != 2 || resp.Credential.Label != "password sign-in" || !resp.Credential.Renews {
		t.Fatalf("change response = %s, want A rotated and two others revoked", rec.Body)
	}
	for name, tok := range map[string]string{"A": tokA, "B": tokB, "the fixture's": f.aliceTok} {
		if got := f.status(t, tok); got != http.StatusUnauthorized {
			t.Errorf("credential %s = %d after the change, want 401", name, got)
		}
	}
	if got := f.status(t, resp.Token); got != http.StatusOK {
		t.Errorf("the rotated credential = %d, want 200", got)
	}
	assertErrorBody(t, f.redeemSignInLink(t, open.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if reason := f.queryString(t, fmt.Sprintf(
		"SELECT detail FROM audit_events WHERE action = 'sign_in_link_closed' AND detail LIKE 'sign_in_link=%d %%'", open.SignInLink.ID)); !strings.HasSuffix(reason, "reason="+store.SignInLinkCloseAuthChanged) {
		t.Errorf("the open link's close = %q, want reason %s", reason, store.SignInLinkCloseAuthChanged)
	}
	for _, id := range []int64{credA, resp.Credential.ID} {
		if ok, err := f.srv.store.RecentAuthStands(context.Background(), id, f.srv.now()); err != nil || ok {
			t.Errorf("credential %d holds a recent-password proof after the change: %v, %v", id, ok, err)
		}
	}
	assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	f.signIn(t, "alice", newAlicePassword)

	var trail []string
	for _, a := range f.actions(t, fmt.Sprintf("principal:%d", f.alice.ID)) {
		switch a {
		case store.AuditPasswordChanged, store.AuditAuthGenerationBumped, "credential_rotated":
			trail = append(trail, a)
		}
	}
	if want := []string{store.AuditPasswordChanged, store.AuditAuthGenerationBumped, "credential_rotated"}; strings.Join(trail, " ") != strings.Join(want, " ") {
		t.Errorf("audit trail = %v, want %v", trail, want)
	}
}

// Every change that is refused leaves the password as it was.
func TestChangePasswordRefusals(t *testing.T) {
	tests := []struct {
		name   string
		do     func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"a wrong current password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, f.aliceTok, "not alice's password at all, no", newAlicePassword)
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed},
		{"the same password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, f.aliceTok, alicePassword, alicePassword)
		}, http.StatusBadRequest, "invalid_request"},
		{"an unacceptable new password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, f.aliceTok, alicePassword, "correct horse battery staple")
		}, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"no password yet", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, f.wrenTok, wrenPassword, newAlicePassword)
		}, http.StatusConflict, schema.ErrorCodePasswordNotSet},
		// The second factor cannot be checked in this build (#709): an
		// operator's change fails closed, the right password included.
		{"an operator", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			f.enrollTOTP(t, f.root.ID)
			return f.send(t, "PUT", "/v1/password/mine", f.rootTok,
				inviteJSON(t, schema.ChangePasswordRequestV1{Current: rootPassword, Password: newAlicePassword, Code: "123456"}), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed},
		{"an agent", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, f.botTok, alicePassword, newAlicePassword)
		}, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"a guest", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.change(t, newGuest(t, f.srv, "", time.Hour, 2*time.Hour).Token, alicePassword, newAlicePassword)
		}, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"plain HTTP", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.do(t, "PUT", "/v1/password/mine", f.aliceTok,
				inviteJSON(t, schema.ChangePasswordRequestV1{Current: alicePassword, Password: newAlicePassword}))
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			before := f.queryString(t, "SELECT group_concat(hash, '|') FROM (SELECT hash FROM passwords ORDER BY principal_id)")
			assertErrorBody(t, tt.do(t, f), tt.status, tt.code)
			if after := f.queryString(t, "SELECT group_concat(hash, '|') FROM (SELECT hash FROM passwords ORDER BY principal_id)"); after != before {
				t.Error("a refused change changed a password")
			}
			if got := f.status(t, f.aliceTok); got != http.StatusOK {
				t.Errorf("alice's credential = %d after a refused change, want 200", got)
			}
		})
	}
}

// A wrong current password counts in the name's failure window with wrong
// sign-ins (review F2): a stolen bearer guesses no faster than the sign-in
// route allows, and the window it fills is the one the sign-in uses.
func TestChangeAndReauthShareTheNameWindow(t *testing.T) {
	for _, route := range []string{"change", "reauth"} {
		t.Run(route, func(t *testing.T) {
			f := newRecoveryFixture(t)
			l := looseLoginLimits()
			l.failuresPerName = inviteLimit{"login_failures_per_name", 3, time.Hour}
			f.srv.setLoginLimits(l)
			_, tok := f.signIn(t, "alice", alicePassword)
			try := func(password string) *httptest.ResponseRecorder {
				if route == "change" {
					return f.change(t, tok, password, newAlicePassword)
				}
				return f.reauth(t, tok, password)
			}
			for i := 0; i < 3; i++ {
				assertErrorBody(t, try("a wrong guess at alice's password"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			}
			rec := try(alicePassword)
			assertErrorBody(t, rec, http.StatusTooManyRequests, "rate_limited")
			if rec.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After")
			}
			assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusTooManyRequests, "rate_limited")
		})
	}
}

// A change loses to whatever commits while its password is verified: the
// store re-checks, and the change answers auth_failed and writes nothing.
func TestChangePasswordRacesLose(t *testing.T) {
	tests := []struct {
		name string
		race func(t *testing.T, f *recoveryFixture)
	}{
		{"another change", func(t *testing.T, f *recoveryFixture) {
			f.setPassword(t, f.alice.ID, "a password set by somebody else meanwhile")
		}},
		{"a sign-out everywhere", func(t *testing.T, f *recoveryFixture) {
			if _, err := f.srv.store.RevokeAllCredentials(context.Background(), "system", f.alice.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"a disable", func(t *testing.T, f *recoveryFixture) {
			if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.alice.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"a reset", func(t *testing.T, f *recoveryFixture) {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			n, err := normalizePassword("a password a reset set meanwhile")
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.srv.passwords.hasher.hash(n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.srv.store.ResetPassword(context.Background(), store.HashSignInLinkCode(code), record, f.srv.now()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			_, tok := f.signIn(t, "alice", alicePassword)
			var once sync.Once
			f.srv.beforePasswordWrite = func() { once.Do(func() { tt.race(t, f) }) }
			before := f.queryString(t, fmt.Sprintf("SELECT COUNT(*) FROM audit_events WHERE action = '%s'", store.AuditPasswordChanged))
			assertErrorBody(t, f.change(t, tok, alicePassword, newAlicePassword), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if after := f.queryString(t, fmt.Sprintf("SELECT COUNT(*) FROM audit_events WHERE action = '%s'", store.AuditPasswordChanged)); after != before {
				t.Error("the losing change was audited")
			}
			f.srv.beforePasswordWrite = nil
			assertErrorBody(t, f.login(t, "alice", newAlicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
		})
	}
}

// A reset link lets the member it was made for replace a forgotten password,
// once, and does nothing else: it previews as a reset, signs nobody in,
// issues no credential, and ends every credential, link and proof of the
// member's. Normal sign-in follows with the new password.
func TestResetPassword(t *testing.T) {
	f := newRecoveryFixture(t)
	credA, tokA := f.signIn(t, "alice", alicePassword)
	if rec := f.reauth(t, tokA, alicePassword); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	open := f.link(t, schema.SignInLinkPurposeSignIn, f.alice.ID)
	reset := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)

	rec := f.previewSignInLink(t, reset.Code)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	if p := decodeBody[schema.SignInLinkPreviewV1](t, rec); p.Name != "alice" || p.Purpose != schema.SignInLinkPurposeReset {
		t.Errorf("preview = %+v, want alice's reset", p)
	}
	assertErrorBody(t, f.redeemSignInLink(t, reset.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)

	rec = f.reset(t, reset.Code, newAlicePassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || string(raw["revoked"]) != "2" {
		t.Errorf("reset response = %s, want the count alone (A and the fixture's), and no credential", rec.Body)
	}
	for name, tok := range map[string]string{"A": tokA, "the fixture's": f.aliceTok} {
		if got := f.status(t, tok); got != http.StatusUnauthorized {
			t.Errorf("credential %s = %d after the reset, want 401", name, got)
		}
	}
	if ok, err := f.srv.store.RecentAuthStands(context.Background(), credA, f.srv.now()); err != nil || ok {
		t.Errorf("A's proof after the reset = %v, %v", ok, err)
	}
	assertErrorBody(t, f.redeemSignInLink(t, open.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	// Used once, it is good for nothing more.
	assertErrorBody(t, f.reset(t, reset.Code, "and a third password, never set"), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	f.signIn(t, "alice", newAlicePassword)

	actor := fmt.Sprintf("sign_in_link:%d", reset.SignInLink.ID)
	var trail []string
	for _, e := range f.audit(t) {
		if e.Actor == actor && !strings.HasPrefix(e.Action, "credential_") {
			trail = append(trail, e.Action+" "+e.Detail)
		}
	}
	want := []string{
		fmt.Sprintf("%s sign_in_link=%d replaced=true", store.AuditPasswordReset, reset.SignInLink.ID),
		fmt.Sprintf("%s cause=password_reset generation=1", store.AuditAuthGenerationBumped),
		fmt.Sprintf("%s sign_in_link=%d reason=%s", store.AuditSignInLinkClosed, open.SignInLink.ID, store.SignInLinkCloseAuthChanged),
	}
	if strings.Join(trail, "\n") != strings.Join(want, "\n") {
		t.Errorf("the reset's audit trail:\n%s\nwant:\n%s", strings.Join(trail, "\n"), strings.Join(want, "\n"))
	}
	if n := len(auditRows(t, f.authFixture, store.AuditPasswordResetIssued)); n != 1 {
		t.Errorf("password_reset_issued rows = %d, want 1", n)
	}
}

// A member who predates passwords and lost their browser too is recovered
// the same way: the reset sets a first password, and they sign in with it.
func TestResetALegacyMember(t *testing.T) {
	f := newRecoveryFixture(t)
	reset := f.link(t, schema.SignInLinkPurposeReset, f.wren.ID)
	if rec := f.reset(t, reset.Code, wrenPassword); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	if got := f.status(t, f.wrenTok); got != http.StatusUnauthorized {
		t.Errorf("wren's old session = %d, want 401", got)
	}
	f.signIn(t, "wren", wrenPassword)
}

// Every reset that is refused changes no password and revokes nothing.
func TestResetRefusals(t *testing.T) {
	tests := []struct {
		name   string
		do     func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"a malformed code", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reset(t, "conch_sil_short", newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an unknown code", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reset(t, schema.SignInLinkCodePrefix+strings.Repeat("B", 43), newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an expired link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			f.advance(schema.DefaultRestrictedLinkLifetime + time.Second)
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a closed link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			l := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)
			if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/sign-in-links/%d", l.SignInLink.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("close = %d", rec.Code)
			}
			return f.reset(t, l.Code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a sign-in link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reset(t, f.link(t, schema.SignInLinkPurposeSignIn, f.alice.ID).Code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an enrollment link", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reset(t, f.link(t, schema.SignInLinkPurposeEnrollment, f.alice.ID).Code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a link made before a sign-out everywhere", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			if _, err := f.srv.store.BumpAuthGeneration(context.Background(), "system", f.alice.ID, "revoke_all", time.Now()); err != nil {
				t.Fatal(err)
			}
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a member disabled since", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			f.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", f.alice.ID)
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a member hidden since", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			f.exec(t, "UPDATE principals SET disabled_at = 1, hidden_at = 1 WHERE id = ?", f.alice.ID)
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a member made an operator since", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			f.exec(t, "UPDATE principals SET role = 'operator' WHERE id = ?", f.alice.ID)
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"a maker no longer an operator", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
			f.exec(t, "UPDATE principals SET role = 'member' WHERE id = ?", f.root.ID)
			return f.reset(t, code, newAlicePassword)
		}, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
		{"an unacceptable password", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reset(t, f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code, "alice alice alice alice")
		}, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"plain HTTP", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.do(t, "POST", "/v1/password/reset", "",
				inviteJSON(t, schema.ResetPasswordRequestV1{Code: f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code, Password: newAlicePassword}))
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			before := f.queryString(t, fmt.Sprintf("SELECT hash FROM passwords WHERE principal_id = %d", f.alice.ID))
			assertErrorBody(t, tt.do(t, f), tt.status, tt.code)
			if after := f.queryString(t, fmt.Sprintf("SELECT hash FROM passwords WHERE principal_id = %d", f.alice.ID)); after != before {
				t.Error("a refused reset changed the password")
			}
			if n := f.spentLinks(t); n != "0" {
				t.Errorf("%s links spent by a refused reset", n)
			}
			if n := len(auditRows(t, f.authFixture, store.AuditPasswordReset)); n != 0 {
				t.Errorf("password_reset rows = %d after a refusal", n)
			}
		})
	}
}

// A reset or an enrollment link is refused for the targets a sign-in link is
// refused for.
func TestResetAndEnrollmentLinkTargets(t *testing.T) {
	for _, purpose := range []schema.SignInLinkPurpose{schema.SignInLinkPurposeReset, schema.SignInLinkPurposeEnrollment} {
		tests := []struct {
			name   string
			target func(t *testing.T, f *recoveryFixture) int64
			status int
			code   string
		}{
			{"an operator", func(t *testing.T, f *recoveryFixture) int64 { return f.root.ID }, http.StatusBadRequest, "invalid_request"},
			{"an agent", func(t *testing.T, f *recoveryFixture) int64 { return f.bot.ID }, http.StatusBadRequest, "invalid_request"},
			{"a guest", func(t *testing.T, f *recoveryFixture) int64 {
				return newGuest(t, f.srv, "", time.Hour, 2*time.Hour).Principal.ID
			}, http.StatusBadRequest, "invalid_request"},
			{"a disabled member", func(t *testing.T, f *recoveryFixture) int64 {
				if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.wren.ID); err != nil {
					t.Fatal(err)
				}
				return f.wren.ID
			}, http.StatusConflict, "principal_disabled"},
			{"a hidden member", func(t *testing.T, f *recoveryFixture) int64 {
				if _, err := f.srv.store.HidePrincipal(context.Background(), "system", f.wren.ID, store.HideOptions{}); err != nil {
					t.Fatal(err)
				}
				return f.wren.ID
			}, http.StatusConflict, "principal_disabled"},
		}
		for _, tt := range tests {
			t.Run(string(purpose)+"/"+tt.name, func(t *testing.T) {
				f := newRecoveryFixture(t)
				id := tt.target(t, f)
				rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", id), f.rootTok,
					inviteJSON(t, schema.CreateSignInLinkRequestV1{Purpose: purpose}), "")
				assertErrorBody(t, rec, tt.status, tt.code)
			})
		}
	}
	// The longest a restricted link may live is an hour.
	f := newRecoveryFixture(t)
	far := schema.NewTimestamp(f.srv.now().Add(2 * time.Hour))
	rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.alice.ID), f.rootTok,
		inviteJSON(t, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset, ExpiresAt: &far}), "")
	assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
}

// Two uses of one reset link at once: exactly one replaces the password.
func TestResetConcurrently(t *testing.T) {
	f := newRecoveryFixture(t)
	code := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code
	const n = 4
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- f.reset(t, code, fmt.Sprintf("racing replacement password number %d", i)).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	won, lost := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			won++
		case http.StatusNotFound:
			lost++
		default:
			t.Errorf("a racing reset answered %d", c)
		}
	}
	if won != 1 || lost != n-1 {
		t.Errorf("won %d, lost %d; want exactly one winner", won, lost)
	}
	if rows := len(auditRows(t, f.authFixture, store.AuditPasswordReset)); rows != 1 {
		t.Errorf("password_reset rows = %d, want 1", rows)
	}
}

// The recent-password proof: issued only for the right password, on the
// session that proved it and no other, good for ten minutes, and ended by
// every change of the member's authentication.
func TestReauthProof(t *testing.T) {
	f := newRecoveryFixture(t)
	credA, tokA := f.signIn(t, "alice", alicePassword)
	credB, tokB := f.signIn(t, "alice", alicePassword)
	ctx := context.Background()

	assertErrorBody(t, f.reauth(t, tokA, "not alice's password at all, no"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if ok, _ := f.srv.store.RecentAuthStands(ctx, credA, f.srv.now()); ok {
		t.Fatal("a wrong password left a proof")
	}

	issued := f.srv.now()
	rec := f.reauth(t, tokA, alicePassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	until := decodeBody[schema.ReauthResponseV1](t, rec).Until.Time()
	if want := issued.Add(schema.RecentAuthLifetimeSeconds * time.Second).Truncate(time.Millisecond); !until.Equal(want) {
		t.Errorf("until = %v, want ten minutes on (%v)", until, want)
	}
	if got := f.whoamiAuth(t, tokA); got == nil || got.RecentAuthUntil == nil || !got.RecentAuthUntil.Time().Equal(until) {
		t.Errorf("whoami of A = %+v, want the proof", got)
	}
	if got := f.whoamiAuth(t, tokB); got == nil || got.RecentAuthUntil != nil {
		t.Errorf("whoami of B = %+v, want no proof: it is bound to A", got)
	}
	for _, c := range []struct {
		name string
		cred int64
		at   time.Duration
		want bool
	}{
		{"A, at once", credA, 0, true},
		{"A, a moment before it ends", credA, schema.RecentAuthLifetimeSeconds*time.Second - time.Millisecond, true},
		{"A, when it ends", credA, schema.RecentAuthLifetimeSeconds * time.Second, false},
		{"A, after", credA, schema.RecentAuthLifetimeSeconds*time.Second + time.Minute, false},
		{"B, at once", credB, 0, false},
	} {
		if got, err := f.srv.store.RecentAuthStands(ctx, c.cred, issued.Add(c.at)); err != nil || got != c.want {
			t.Errorf("%s: stands = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if n := len(auditRows(t, f.authFixture, store.AuditReauth)); n != 1 {
		t.Errorf("reauth rows = %d, want 1", n)
	}
}

// Each change of the member's authentication ends a standing proof.
func TestReauthProofInvalidation(t *testing.T) {
	tests := []struct {
		name string
		// end ends it; credA is the session holding the proof, tokB another.
		end func(t *testing.T, f *recoveryFixture, credA int64, tokA, tokB string)
	}{
		{"a password change from another session", func(t *testing.T, f *recoveryFixture, _ int64, _, tokB string) {
			if rec := f.change(t, tokB, alicePassword, newAlicePassword); rec.Code != http.StatusOK {
				t.Fatalf("change = %d %s", rec.Code, rec.Body)
			}
		}},
		{"a password reset", func(t *testing.T, f *recoveryFixture, _ int64, _, _ string) {
			if rec := f.reset(t, f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code, newAlicePassword); rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body)
			}
		}},
		{"an authentication generation bump", func(t *testing.T, f *recoveryFixture, _ int64, _, _ string) {
			if _, err := f.srv.store.BumpAuthGeneration(context.Background(), "system", f.alice.ID, "test", time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"a sign-out everywhere", func(t *testing.T, f *recoveryFixture, _ int64, _, tokB string) {
			if rec := f.send(t, "POST", f.revokeAllPath(f.alice.ID), tokB, "", ""); rec.Code != http.StatusOK {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
		}},
		{"a disable", func(t *testing.T, f *recoveryFixture, _ int64, _, _ string) {
			if rec := f.send(t, "POST", f.disablePath(f.alice.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("disable = %d %s", rec.Code, rec.Body)
			}
		}},
		{"signing out of that session", func(t *testing.T, f *recoveryFixture, _ int64, tokA, _ string) {
			if rec := f.send(t, "DELETE", "/v1/credentials/current", tokA, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			credA, tokA := f.signIn(t, "alice", alicePassword)
			_, tokB := f.signIn(t, "alice", alicePassword)
			if rec := f.reauth(t, tokA, alicePassword); rec.Code != http.StatusOK {
				t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
			}
			tt.end(t, f, credA, tokA, tokB)
			if ok, err := f.srv.store.RecentAuthStands(context.Background(), credA, f.srv.now()); err != nil || ok {
				t.Errorf("the proof stands after %s: %v, %v", tt.name, ok, err)
			}
		})
	}
}

// Who may not have a proof, and what they are told.
func TestReauthRefusals(t *testing.T) {
	tests := []struct {
		name   string
		do     func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder
		status int
		code   string
	}{
		// The S15 boundary: a script credential is outside the class.
		{"a script credential", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reauth(t, f.aliceTok, alicePassword)
		}, http.StatusForbidden, "forbidden"},
		{"no password yet", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reauth(t, f.wrenTok, wrenPassword)
		}, http.StatusConflict, schema.ErrorCodePasswordNotSet},
		{"an operator", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			f.enrollTOTP(t, f.root.ID)
			return f.send(t, "POST", "/v1/reauth", f.rootTok, inviteJSON(t, schema.ReauthRequestV1{Password: rootPassword, Code: "123456"}), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed},
		{"an agent", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reauth(t, f.botTok, alicePassword)
		}, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"a guest", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			return f.reauth(t, newGuest(t, f.srv, "", time.Hour, 2*time.Hour).Token, alicePassword)
		}, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"plain HTTP", func(t *testing.T, f *recoveryFixture) *httptest.ResponseRecorder {
			_, tok := f.signIn(t, "alice", alicePassword)
			return f.do(t, "POST", "/v1/reauth", tok, inviteJSON(t, schema.ReauthRequestV1{Password: alicePassword}))
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			assertErrorBody(t, tt.do(t, f), tt.status, tt.code)
			if n := f.queryString(t, "SELECT COUNT(*) FROM credentials WHERE recent_auth_until IS NOT NULL"); n != "0" {
				t.Errorf("%s credentials hold a proof after a refusal", n)
			}
		})
	}
}

// Nothing the password routes write or log holds a password, a record, a
// link code or a token.
func TestPasswordRoutesSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newRecoveryFixture(t)
	enroll := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID)
	rec := f.enroll(t, f.wrenTok, wrenPassword, enroll.Code)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d", rec.Code)
	}
	wrenNew := decodeBody[schema.PasswordUpdatedResponseV1](t, rec).Token
	_, tok := f.signIn(t, "alice", alicePassword)
	f.reauth(t, tok, alicePassword)
	f.reauth(t, tok, "a wrong password for the secrets test")
	rec = f.change(t, tok, alicePassword, newAlicePassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("change = %d", rec.Code)
	}
	aliceNew := decodeBody[schema.PasswordUpdatedResponseV1](t, rec).Token
	reset := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)
	f.reset(t, reset.Code, "a third password, set by a reset")
	f.reset(t, reset.Code, "a fourth password, refused by now")

	secrets := []string{wrenPassword, alicePassword, newAlicePassword, "a third password, set by a reset", "a fourth password, refused by now",
		"a wrong password for the secrets test", enroll.Code, reset.Code, tok, wrenNew, aliceNew, f.wrenTok,
		store.HashSignInLinkCode(enroll.Code), store.HashSignInLinkCode(reset.Code)}
	for _, e := range f.audit(t) {
		for _, s := range secrets {
			if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, s) {
				t.Errorf("audit %s holds a secret", e.Action)
			}
		}
		if strings.Contains(e.Detail, "$argon2id$") {
			t.Errorf("audit %s holds a password record", e.Action)
		}
	}
	text := logs.buf.String()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Error("the log holds a secret")
		}
	}
	if strings.Contains(text, "$argon2id$") {
		t.Error("the log holds a password record")
	}
}

// presenceSend makes an HTTPS request on the presence fixture with a body,
// marking the response in LiveKit's call log as doOrdered does.
func presenceSend(t *testing.T, f *presenceFixture, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.TLS = &tls.ConnectionState{}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(&orderWriter{ResponseWriter: rec, lk: f.lk}, req)
	return rec
}

// A password change or a reset ends the member's live access at once, as a
// sign-out everywhere does: every socket of theirs closes with the revoked
// reason, whichever credential opened it, another person's stays open, and
// the voice rooms they held a session for are rotated before the response is
// written.
func TestPasswordChangeEndsLiveAccess(t *testing.T) {
	actions := []struct {
		name string
		// act changes ann's password; tokA is the credential it is sent on.
		act  func(t *testing.T, f *presenceFixture, tokA string) *httptest.ResponseRecorder
		want int
	}{
		{"a change", func(t *testing.T, f *presenceFixture, tokA string) *httptest.ResponseRecorder {
			return presenceSend(t, f, "PUT", "/v1/password/mine", tokA, inviteJSON(t, schema.ChangePasswordRequestV1{Current: annPassword, Password: newAlicePassword}))
		}, http.StatusOK},
		{"a reset", func(t *testing.T, f *presenceFixture, _ string) *httptest.ResponseRecorder {
			rec := presenceSend(t, f, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.ids["ann"]), f.rootTok,
				inviteJSON(t, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset}))
			if rec.Code != http.StatusCreated {
				t.Fatalf("make the reset link = %d %s", rec.Code, rec.Body)
			}
			code := decodeBody[schema.CreateSignInLinkResponseV1](t, rec).Code
			return presenceSend(t, f, "POST", "/v1/password/reset", "", inviteJSON(t, schema.ResetPasswordRequestV1{Code: code, Password: newAlicePassword}))
		}, http.StatusOK},
	}
	families := []struct{ name, path string }{
		{"v2", "/v2/ws?channel=ops"},
		{"events", "/v1/events/ws?channel=ops"},
		{"voice", "/v1/voice/ws?channel=ops"},
	}
	for _, act := range actions {
		for _, fam := range families {
			t.Run(act.name+"/"+fam.name, func(t *testing.T) {
				f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
				f.srv.credRecheckInterval = time.Hour
				f.srv.eventsRosterFloor = 0
				ctx := context.Background()
				if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, f.root.ID, 0); err != nil {
					t.Fatal(err)
				}
				n, err := normalizePassword(annPassword)
				if err != nil {
					t.Fatal(err)
				}
				record, err := f.srv.passwords.hasher.hash(n)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.srv.store.SetPassword(ctx, "system", f.ids["ann"], record, "test", time.Now()); err != nil {
					t.Fatal(err)
				}
				// ann holds a voice session in ops on credential A.
				old := f.inUse(t, f.ops)
				f.holder(t, "ann", f.ops)
				tokA := f.tokens["ann"]
				_, tokB := f.newCred(t, f.ids["ann"])

				base := wsTestServer(t, f.srv)
				dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				a := recordFrames(dialAs(t, dctx, base, fam.path, tokA))
				b := recordFrames(dialAs(t, dctx, base, fam.path, tokB))
				root := recordFrames(dialAs(t, dctx, base, fam.path, f.rootTok))
				calls := len(f.lk.calls())

				if rec := act.act(t, f, tokA); rec.Code != act.want {
					t.Fatalf("%s = %d %s", act.name, rec.Code, rec.Body)
				}
				closeErrContains(t, a.closed(t, "A's socket"), websocket.StatusPolicyViolation, reasonRevoked)
				closeErrContains(t, b.closed(t, "B's socket"), websocket.StatusPolicyViolation, reasonRevoked)
				if !root.open() {
					t.Errorf("root's socket closed: %v", root.err)
				}

				deleted, respAt, deleteAt := false, 0, 0
				for _, c := range f.lk.calls()[calls:] {
					switch {
					case c.method == "DeleteRoom" && c.room == old:
						deleted, deleteAt = true, c.seq
					case c.method == "http-response": // the last: the reset makes its link first
						respAt = c.seq
					}
				}
				if !deleted || respAt == 0 || deleteAt > respAt {
					t.Errorf("the old room deleted = %v (seq %d), response at seq %d: want it deleted before the response", deleted, deleteAt, respAt)
				}
				if f.room(t, f.ops) == old {
					t.Error("ops's room did not rotate")
				}
			})
		}
	}
}
