package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// operatorFixture is the login fixture (alice and root have passwords, root
// a script credential) on a server with a TOTP key, with the helpers an
// operator's setup and sign-in need. Codes are made at the server's clock,
// and each helper that spends a code moves the clock to the next step, as a
// person waits for the next code.
type operatorFixture struct {
	*loginFixture
}

func newOperatorFixture(t *testing.T) *operatorFixture {
	t.Helper()
	return &operatorFixture{newLoginFixture(t, Config{TOTPKey: testTOTPKey(t)})}
}

// otherAddress is a client address other than the fixture's default one.
const otherAddress = "198.51.100.7:4321"

func (f *operatorFixture) code(secret []byte) string {
	return totpCodeAt(secret, totpStep(f.srv.now()))
}

func (f *operatorFixture) nextStep() { f.advance(schema.TOTPPeriodSeconds * time.Second) }

// hostSetupToken makes id a setup token as `conchd operator setup` does.
func (f *operatorFixture) hostSetupToken(t *testing.T, id int64) string {
	t.Helper()
	tok, err := f.srv.store.IssueSetupToken(context.Background(), id, f.srv.now())
	if err != nil {
		t.Fatalf("IssueSetupToken: %v", err)
	}
	return tok.Token
}

func (f *operatorFixture) redeemSetup(t *testing.T, token string) schema.RedeemSetupTokenResponseV1 {
	t.Helper()
	rec := f.send(t, "POST", "/v1/setup/redeem", "", inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: token}), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem setup token = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.RedeemSetupTokenResponseV1](t, rec)
}

// enroll starts a TOTP enrolment on setupTok and returns the secret it shows.
func (f *operatorFixture) enroll(t *testing.T, setupTok string) []byte {
	t.Helper()
	rec := f.send(t, "POST", "/v1/totp/enroll", setupTok, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the enrolment was sent without no-store")
	}
	e := decodeBody[schema.TOTPEnrollmentV1](t, rec)
	secret, err := totpBase32.DecodeString(e.Secret)
	if err != nil || len(secret) != schema.TOTPSecretBytes {
		t.Fatalf("enrolment secret %q: %v", e.Secret, err)
	}
	return secret
}

func (f *operatorFixture) confirm(t *testing.T, setupTok, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/totp/confirm", setupTok, inviteJSON(t, schema.ConfirmTOTPRequestV1{Code: code}), "")
}

// enrolOperator runs id's setup over REST, with the password it already
// has: a setup token, its credential, the enrolment and its confirmation.
// It returns the secret.
func (f *operatorFixture) enrolOperator(t *testing.T, id int64) []byte {
	t.Helper()
	setup := f.redeemSetup(t, f.hostSetupToken(t, id))
	secret := f.enroll(t, setup.Token)
	if rec := f.confirm(t, setup.Token, f.code(secret)); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body)
	}
	f.nextStep()
	return secret
}

// rootChallenge signs root in with password from remote and returns the
// challenge, failing unless the answer is one.
func (f *operatorFixture) rootChallenge(t *testing.T, password, remote string) string {
	t.Helper()
	rec := f.login(t, "root", password, remote)
	if rec.Code != http.StatusOK {
		t.Fatalf("login root = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.LoginResponseV1](t, rec)
	if resp.Next != schema.LoginNextTOTP || resp.Token != "" {
		t.Fatalf("login root answered %+v, want a challenge", resp)
	}
	return resp.Challenge
}

func (f *operatorFixture) completeLogin(t *testing.T, challenge, code, remote string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/login/totp", "", inviteJSON(t, schema.TOTPLoginRequestV1{Challenge: challenge, Code: code}), remote)
}

// operatorSession signs root in with both factors and returns the session.
func (f *operatorFixture) operatorSession(t *testing.T, secret []byte) (schema.CredentialV1, string) {
	t.Helper()
	rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), f.code(secret), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login/totp = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.LoginResponseV1](t, rec)
	if err := resp.Validate(); err != nil || resp.Next != schema.LoginNextSignedIn {
		t.Fatalf("login/totp answered %+v: %v", resp, err)
	}
	f.nextStep()
	return *resp.Credential, resp.Token
}

func (f *operatorFixture) reauthOp(t *testing.T, token, password, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/reauth", token, inviteJSON(t, schema.ReauthRequestV1{Password: password, Code: code}), "")
}

func (f *operatorFixture) whoamiAuth(t *testing.T, token string) *schema.AuthStateV1 {
	t.Helper()
	rec := f.send(t, "GET", "/v1/whoami", token, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.WhoAmIResponseV1](t, rec).Auth
}

// status is whoami's status for token: 200 while it is live.
func (f *operatorFixture) status(t *testing.T, token string) int {
	t.Helper()
	return f.send(t, "GET", "/v1/whoami", token, "", "").Code
}

// sensitive is a route of the re-authentication class: an operator makes a
// credential for alice.
func (f *operatorFixture) sensitive(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID), token, `{"label":"made in a test"}`, "")
}

// The whole of an operator's browser authentication, as the design's flow
// runs it: a setup token from the host, its restricted credential, the
// password and the authenticator, then a sign-in with both factors that
// reaches the API, a sensitive action that needs a fresh proof, and an audit
// trail that records each step.
func TestOperatorSetupAndSignIn(t *testing.T) {
	f := newOperatorFixture(t)
	setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
	if setup.Principal.ID != f.root.ID || setup.Credential.Scope != schema.CredentialScopeSetup || setup.Auth.Scope != schema.CredentialScopeSetup ||
		!setup.Auth.PasswordSet || setup.Auth.TOTPEnrolled || setup.Auth.SetupComplete {
		t.Fatalf("redeem answered %+v", setup)
	}
	if got := f.whoamiAuth(t, setup.Token); got.Scope != schema.CredentialScopeSetup || got.SetupComplete {
		t.Errorf("whoami on the setup credential: %+v", got)
	}
	// Nothing else is in its reach.
	assertErrorBody(t, f.send(t, "GET", "/v1/channels", setup.Token, "", ""), http.StatusForbidden, schema.ErrorCodeSetupIncomplete)
	// The password, replaced through the setup credential, as recovery does.
	const newRoot = "the operator's new and private passphrase"
	if rec := f.send(t, "POST", "/v1/password/mine", setup.Token, inviteJSON(t, schema.SetPasswordRequestV1{Password: newRoot}), ""); rec.Code != http.StatusOK {
		t.Fatalf("set password = %d %s", rec.Code, rec.Body)
	}
	if got := f.status(t, f.rootTok); got != http.StatusUnauthorized {
		t.Errorf("the script credential = %d after the password was replaced, want 401", got)
	}
	secret := f.enroll(t, setup.Token)
	assertErrorBody(t, f.confirm(t, setup.Token, totpCodeAt(secret, totpStep(f.srv.now())+5)), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if rec := f.confirm(t, setup.Token, f.code(secret)); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body)
	}
	// Setup is complete: the setup credential has done its work.
	if got := f.status(t, setup.Token); got != http.StatusUnauthorized {
		t.Errorf("the setup credential = %d after setup, want 401", got)
	}
	f.nextStep()

	// The password alone is no sign-in: it is a challenge.
	ch := f.rootChallenge(t, newRoot, "")
	rec := f.completeLogin(t, ch, f.code(secret), "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login/totp = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.LoginResponseV1](t, rec)
	cred, tok := *resp.Credential, resp.Token
	now := f.srv.now().Truncate(time.Millisecond)
	if cred.ExpiresAt == nil || !cred.ExpiresAt.Time().Equal(now.Add(12*time.Hour)) || cred.IdleExpiresAt == nil ||
		!cred.IdleExpiresAt.Time().Equal(now.Add(time.Hour)) || cred.Renews {
		t.Errorf("the operator session: %+v", cred)
	}
	// The sign-in with both factors is the proof for its first ten minutes
	// (issue #1033), and the sensitive action needs nothing more.
	if got := f.whoamiAuth(t, tok); !got.SetupComplete || !got.TOTPEnrolled || got.IdleExpiresAt == nil ||
		got.RecentAuthUntil == nil || !got.RecentAuthUntil.Time().Equal(now.Add(schema.RecentAuthLifetimeSeconds*time.Second)) {
		t.Errorf("whoami on the session: %+v", got)
	}
	f.nextStep()
	if rec := f.sensitive(t, tok); rec.Code != http.StatusCreated {
		t.Fatalf("a sensitive action just after the sign-in = %d %s", rec.Code, rec.Body)
	}
	// After them, it asks for a fresh one.
	f.advance(schema.RecentAuthLifetimeSeconds * time.Second)
	assertErrorBody(t, f.sensitive(t, tok), http.StatusForbidden, schema.ErrorCodeReauthRequired)
	if rec := f.reauthOp(t, tok, newRoot, f.code(secret)); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	if rec := f.sensitive(t, tok); rec.Code != http.StatusCreated {
		t.Fatalf("a sensitive action after the proof = %d %s", rec.Code, rec.Body)
	}

	var trail []string
	for _, e := range f.audit(t) {
		if e.Subject == fmt.Sprintf("principal:%d", f.root.ID) {
			trail = append(trail, e.Action)
		}
	}
	got := strings.Join(trail, " ")
	for _, step := range []string{store.AuditSetupTokenIssued, store.AuditSetupTokenRedeemed, store.AuditPasswordSet, store.AuditTOTPEnrolled,
		store.AuditLogin, store.AuditReauth} {
		if !strings.Contains(got, step) {
			t.Errorf("the audit trail has no %s: %s", step, got)
		}
	}
	// The refusal for want of a proof is audited too, under the route.
	if rows := auditRows(t, f.authFixture, "access_denied"); len(rows) != 1 || !strings.Contains(rows[0].Detail, schema.ErrorCodeReauthRequired) {
		t.Errorf("access_denied rows %+v, want the one reauth_required", rows)
	}
}

// A setup-scoped credential reaches whoami, the password enrolment, the
// TOTP enrolment and its own sign-out (ADR-011 item 2.1), and every other
// route of the table answers 403 setup_incomplete (review F6), before and
// after the password it replaced.
func TestSetupScopeReachesOnlySetupRoutes(t *testing.T) {
	f := newOperatorFixture(t)
	setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
	check := func(t *testing.T) {
		t.Helper()
		for _, rt := range f.srv.routes {
			if rt.access == accessExempt {
				continue // the middleware reads no credential there
			}
			exp, ok := routeExpectations[rt.pattern]
			if !ok {
				t.Fatalf("no expectation for %s", rt.pattern)
			}
			if rt.pattern == "POST /v1/totp/enroll" || rt.pattern == "POST /v1/password/mine" || rt.pattern == "DELETE /v1/credentials/current" {
				continue // these write; they are run on their own below
			}
			rec := f.send(t, routeMethod(rt.pattern), exp.path, setup.Token, exp.body, "")
			code := ""
			if rec.Code == http.StatusForbidden {
				code = decodeBody[schema.Error](t, rec).Code
			}
			if setupScopeRoutes[rt.pattern] {
				if rec.Code == http.StatusUnauthorized || code == schema.ErrorCodeSetupIncomplete {
					t.Errorf("%s refused the setup credential: %d %s", rt.pattern, rec.Code, rec.Body)
				}
				continue
			}
			if rec.Code != http.StatusForbidden || code != schema.ErrorCodeSetupIncomplete {
				t.Errorf("%s answered the setup credential %d %s, want 403 setup_incomplete", rt.pattern, rec.Code, rec.Body)
			}
		}
		for _, c := range []struct{ method, path string }{{"GET", "/v1/nope"}, {"DELETE", "/v1/whoami"}} {
			assertErrorBody(t, f.send(t, c.method, c.path, setup.Token, "", ""), http.StatusForbidden, schema.ErrorCodeSetupIncomplete)
		}
	}
	check(t)
	if rec := f.send(t, "POST", "/v1/password/mine", setup.Token,
		inviteJSON(t, schema.SetPasswordRequestV1{Password: "a replacement and private passphrase"}), ""); rec.Code != http.StatusOK {
		t.Fatalf("set password = %d %s", rec.Code, rec.Body)
	}
	t.Run("after the password was replaced", check)
	if rec := f.send(t, "POST", "/v1/totp/enroll", setup.Token, "", ""); rec.Code != http.StatusOK {
		t.Errorf("enroll = %d %s", rec.Code, rec.Body)
	}
	// Skip: the setup credential ends itself, and is gone.
	if rec := f.send(t, "DELETE", "/v1/credentials/current", setup.Token, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("sign out = %d %s", rec.Code, rec.Body)
	}
	if code := f.status(t, setup.Token); code != http.StatusUnauthorized {
		t.Errorf("whoami after the sign-out = %d, want 401", code)
	}
}

// Every way the TOTP step can fail answers the one auth_failed, issues
// nothing, and audits a reason; a malformed body or plain HTTP is refused
// before anything is looked up.
func TestLoginTOTPRefusals(t *testing.T) {
	tests := []struct {
		name   string
		do     func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder
		status int
		code   string
		reason string
	}{
		{"a wrong code", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			return f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), totpCodeAt(secret, totpStep(f.srv.now())+3), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonWrongCode},
		{"a code already used", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			code := f.code(secret)
			if rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), code, ""); rec.Code != http.StatusOK {
				t.Fatalf("first use = %d %s", rec.Code, rec.Body)
			}
			return f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), code, "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonWrongCode},
		{"a code from another address", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			return f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), f.code(secret), otherAddress)
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonWrongAddress},
		{"an expired challenge", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			ch := f.rootChallenge(t, rootPassword, "")
			f.advance(schema.PendingLoginLifetimeSeconds * time.Second)
			return f.completeLogin(t, ch, f.code(secret), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonChallengeGone},
		{"a challenge nobody was given", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			return f.completeLogin(t, schema.PendingLoginPrefix+strings.Repeat("A", 43), f.code(secret), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonChallengeGone},
		{"an operator disabled since", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			ch := f.rootChallenge(t, rootPassword, "")
			f.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", f.root.ID)
			return f.completeLogin(t, ch, f.code(secret), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonStale},
		{"a sign-out everywhere since", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			ch := f.rootChallenge(t, rootPassword, "")
			if _, err := f.srv.store.BumpAuthGeneration(context.Background(), "system", f.root.ID, "test", f.srv.now()); err != nil {
				t.Fatal(err)
			}
			return f.completeLogin(t, ch, f.code(secret), "")
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, loginReasonChallengeGone},
		{"a malformed code", func(t *testing.T, f *operatorFixture, _ []byte) *httptest.ResponseRecorder {
			return f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), "12345a", "")
		}, http.StatusBadRequest, "invalid_request", ""},
		{"a credential's token as the challenge", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			return f.completeLogin(t, f.rootTok, f.code(secret), "")
		}, http.StatusBadRequest, "invalid_request", ""},
		{"plain HTTP", func(t *testing.T, f *operatorFixture, secret []byte) *httptest.ResponseRecorder {
			ch := f.rootChallenge(t, rootPassword, "")
			return f.do(t, "POST", "/v1/login/totp", "", inviteJSON(t, schema.TOTPLoginRequestV1{Challenge: ch, Code: f.code(secret)}))
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOperatorFixture(t)
			secret := f.enrolOperator(t, f.root.ID)
			before := f.credentialCount(t, f.root.ID)
			rec := tt.do(t, f, secret)
			assertErrorBody(t, rec, tt.status, tt.code)
			if tt.name != "a code already used" && f.credentialCount(t, f.root.ID) != before {
				t.Error("a refused completion issued a credential")
			}
			if tt.reason == "" {
				return
			}
			rows := strings.Join(loginFailedRows(t, f.authFixture), "\n")
			if !strings.Contains(rows, "reason="+tt.reason) {
				t.Errorf("login_failed rows %q, want reason=%s", rows, tt.reason)
			}
		})
	}
}

// A challenge takes MaxTOTPAttemptsPerChallenge wrong codes: the right code
// after that is refused, and the operator asks for a new challenge.
func TestLoginTOTPAttemptsPerChallenge(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	ch := f.rootChallenge(t, rootPassword, "")
	wrong := totpCodeAt(secret, totpStep(f.srv.now())+4)
	for range schema.MaxTOTPAttemptsPerChallenge {
		assertErrorBody(t, f.completeLogin(t, ch, wrong, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	}
	assertErrorBody(t, f.completeLogin(t, ch, f.code(secret), ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), f.code(secret), ""); rec.Code != http.StatusOK {
		t.Errorf("a fresh challenge = %d %s", rec.Code, rec.Body)
	}
}

// One failure count for every factor (review F2): challenges spend from the
// name's window, and wrong codes count in it across challenges and
// addresses, so knowing the password buys no fresh guesses at the code. The
// next attempt, password or code, is an immediate 429; only an issued
// credential would have reset it.
func TestWrongCodesCountInTheNameWindow(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	limits := looseLoginLimits()
	limits.failuresPerName = inviteLimit{"login_failures_per_name", defaultLoginFailuresPerName, defaultLoginFailureWindow}
	f.srv.setLoginLimits(limits)
	wrong := totpCodeAt(secret, totpStep(f.srv.now())+4)
	// Five of the window, from three addresses: a challenge, a wrong code,
	// a challenge, a wrong code, and a last challenge.
	first := f.rootChallenge(t, rootPassword, "192.0.2.10:1")
	assertErrorBody(t, f.completeLogin(t, first, wrong, "192.0.2.10:1"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	second := f.rootChallenge(t, rootPassword, "192.0.2.11:1")
	assertErrorBody(t, f.completeLogin(t, second, wrong, "192.0.2.11:1"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	last := f.rootChallenge(t, rootPassword, "192.0.2.12:1")
	// The window is full: the right password, and even the right code on
	// the last challenge, are refused at once, before any work.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"the right password": f.login(t, "root", rootPassword, "192.0.2.13:1"),
		"the right code":     f.completeLogin(t, last, f.code(secret), "192.0.2.12:1"),
	} {
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s with the window full = %d %s, want an immediate 429", name, rec.Code, rec.Body)
		}
	}
	// Ten minutes on, the window has room again and the last challenge,
	// expired, is no use; a new one signs in.
	f.advance(defaultLoginFailureWindow)
	assertErrorBody(t, f.completeLogin(t, last, f.code(secret), "192.0.2.12:1"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, "192.0.2.12:1"), f.code(secret), "192.0.2.12:1"); rec.Code != http.StatusOK {
		t.Errorf("after the window = %d %s", rec.Code, rec.Body)
	}
}

// One code, sent at once to a sign-in and to a re-authentication, is
// accepted once (design §5.3).
func TestTOTPReplayAcrossConcurrentLoginAndReauth(t *testing.T) {
	for round := range 5 {
		f := newOperatorFixture(t)
		secret := f.enrolOperator(t, f.root.ID)
		_, session := f.operatorSession(t, secret)
		ch := f.rootChallenge(t, rootPassword, "")
		code := f.code(secret)
		var (
			wg    sync.WaitGroup
			codes [2]int
		)
		wg.Add(2)
		go func() { defer wg.Done(); codes[0] = f.completeLogin(t, ch, code, "").Code }()
		go func() { defer wg.Done(); codes[1] = f.reauthOp(t, session, rootPassword, code).Code }()
		wg.Wait()
		ok := 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusUnauthorized:
			default:
				t.Fatalf("round %d: statuses %v", round, codes)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: one code passed %d times (%v), want once", round, ok, codes)
		}
	}
}

// The operator's browser session ends an hour after its last REST request
// and twelve hours after it began, checked by the server whatever the
// browser keeps; a REST request moves the idle bound.
func TestOperatorSessionBounds(t *testing.T) {
	tests := []struct {
		name  string
		age   func(t *testing.T, f *operatorFixture, id int64)
		alive bool
	}{
		{"fresh", func(*testing.T, *operatorFixture, int64) {}, true},
		{"idle for an hour", func(t *testing.T, f *operatorFixture, id int64) {
			past := time.Now().Add(-time.Hour - time.Second).UnixMilli()
			f.exec(t, "UPDATE credentials SET created_at = ?, last_used_at = ? WHERE id = ?", past, past, id)
		}, false},
		{"used a minute ago", func(t *testing.T, f *operatorFixture, id int64) {
			past := time.Now().Add(-time.Hour - time.Second).UnixMilli()
			f.exec(t, "UPDATE credentials SET created_at = ?, last_used_at = ? WHERE id = ?", past, time.Now().Add(-time.Minute).UnixMilli(), id)
		}, true},
		{"twelve hours old", func(t *testing.T, f *operatorFixture, id int64) {
			f.exec(t, "UPDATE credentials SET expires_at = ?, last_used_at = ? WHERE id = ?", time.Now().UnixMilli(), time.Now().UnixMilli(), id)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOperatorFixture(t)
			cred, tok := f.operatorSession(t, f.enrolOperator(t, f.root.ID))
			tt.age(t, f, cred.ID)
			want := http.StatusUnauthorized
			if tt.alive {
				want = http.StatusOK
			}
			if got := f.status(t, tok); got != want {
				t.Errorf("whoami = %d, want %d", got, want)
			}
		})
	}
	// A REST request moves the idle bound, at most once a minute.
	f := newOperatorFixture(t)
	cred, tok := f.operatorSession(t, f.enrolOperator(t, f.root.ID))
	past := time.Now().Add(-30 * time.Minute).UnixMilli()
	f.exec(t, "UPDATE credentials SET created_at = ?, last_used_at = NULL WHERE id = ?", past, cred.ID)
	if got := f.status(t, tok); got != http.StatusOK {
		t.Fatalf("whoami = %d", got)
	}
	if last := f.queryString(t, fmt.Sprintf("SELECT COALESCE(last_used_at, 0) > %d FROM credentials WHERE id = %d", past, cred.ID)); last != "1" {
		t.Error("a REST request did not move the operator session's idle bound")
	}
}

// reauthDecision is a route's place in the re-authentication class and why.
type reauthDecision struct {
	rule reauthRule
	why  string
}

// The re-authentication class by its rule (design §5.3, review F8): every
// operator and operator-or-self route is decided here, independently of
// recentAuthRoutes, and the test fails on a route with no decision, on a
// decision that disagrees, and on a class entry for a route that is not in
// the table or is not an operator route.
var reauthDecisions = map[string]reauthDecision{
	"POST /v0/channels":                                               {reauthNone, "makes a channel: no principal, credential, link, permit or hook, no grant"},
	"PUT /v1/channels/{id}/settings":                                  {reauthNone, "changes a channel's visibility and default mark: no principal, credential, link, permit or hook, no grant"},
	"POST /v0/principals":                                             {reauthAlways, "creates a principal"},
	"PUT /v1/channels/{channel}/members/{principal_id}":               {reauthOperators, "grants access, and can_decide (approval-path); a manager's add is a member's write (issue #790)"},
	"POST /v1/channels/{channel}/members":                             {reauthOperators, "grants access, as the PUT by id; a manager's add is a member's write (issue #790)"},
	"PUT /v1/principals/{id}/role":                                    {reauthAlways, "makes or unmakes an operator or an owner (issue #790)"},
	"DELETE /v1/channels/{channel}/members/{principal_id}":            {reauthNone, "narrows access; the person stays"},
	"POST /v1/channels/{channel}/nets":                                {reauthNone, "a net, as a channel"},
	"DELETE /v1/channels/{channel}/nets/{net}":                        {reauthNone, "archives a net"},
	"PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}":    {reauthAlways, "grants access to a net"},
	"DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}": {reauthNone, "narrows access; the person stays"},
	"PUT /v1/principals/{id}/manifest":                                {reauthAlways, "grants an agent its rights (manifests)"},
	"GET /v1/principals/{id}/manifest":                                {reauthNone, "a manifest read"},
	"PUT /v1/principals/{id}/name":                                    {reauthNone, "a rename grants and ends nothing"},
	"PUT /v1/channels/{id}/name":                                      {reauthNone, "a rename grants and ends nothing"},
	"POST /v1/principals/{id}/credentials":                            {reauthAlways, "creates a credential"},
	"GET /v1/principals/{id}/credentials":                             {reauthNone, "a list"},
	"POST /v1/credentials/{credential_id}/rotate":                     {reauthAlways, "creates a credential"},
	"DELETE /v1/credentials/{credential_id}":                          {reauthAlways, "ends a person's sign-in"},
	"POST /v1/principals/{id}/disable":                                {reauthAlways, "ends a person"},
	"POST /v1/principals/{id}/enable":                                 {reauthAlways, "grants access back"},
	"POST /v1/principals/{id}/hide":                                   {reauthAlways, "removes a person"},
	"POST /v1/principals/hide":                                        {reauthAlways, "removes people in bulk"},
	"GET /v1/principals":                                              {reauthNone, "a list"},
	"POST /v1/principals/{id}/credentials/revoke-all":                 {reauthOnOthers, "bulk revoke of someone else; signing oneself out is not"},
	"GET /v1/audit":                                                   {reauthNone, "audit export"},
	"POST /v1/hooks":                                                  {reauthAlways, "creates a hook"},
	"GET /v1/hooks":                                                   {reauthNone, "a list"},
	"DELETE /v1/hooks/{id}":                                           {reauthNone, "closes a hook, as an invite close"},
	"GET /v1/invites/{id}/principals":                                 {reauthNone, "a list"},
	"POST /v1/invites/{id}/disable-joiners":                           {reauthAlways, "ends people in bulk"},
	"POST /v1/principals/{id}/disable-invitees":                       {reauthAlways, "ends people in bulk"},
	"PUT /v1/principals/{id}/invite-permission":                       {reauthAlways, "grants invite permission"},
	"GET /v1/invites/settings":                                        {reauthNone, "a settings read"},
	"PUT /v1/invites/settings":                                        {reauthAlways, "opens or closes joining through invites"},
	"POST /v1/principals/{id}/sign-in-links":                          {reauthAlways, "creates a link"},
	"DELETE /v1/sign-in-links/{id}":                                   {reauthNone, "closes a link, as an invite close"},
	"GET /v1/join/settings":                                           {reauthNone, "a settings read"},
	"PUT /v1/join/settings":                                           {reauthAlways, "opens or closes joining to guests"},
	"GET /v1/guests":                                                  {reauthNone, "a list"},
	"POST /v1/guests/{id}/end":                                        {reauthAlways, "ends a person"},
	"POST /v1/guests/end":                                             {reauthAlways, "ends people in bulk"},
	"POST /v1/guests/{id}/conversion-permit":                          {reauthAlways, "creates a permit"},
	"GET /v1/reports":                                                 {reauthNone, "reports"},
	"GET /v1/reports/{id}":                                            {reauthNone, "reports"},
	"PUT /v1/reports/{id}":                                            {reauthNone, "files a report"},

	// Every write anyone authenticated may make: in the class only where an
	// operator's use creates a principal or a link (rule: reauthOperators).
	"POST /v1/invites":                              {reauthOperators, "an operator's invite makes principals, with no allowance"},
	"DELETE /v1/invites/{id}":                       {reauthNone, "closes an invite"},
	"POST /v1/ws-tickets":                           {reauthNone, "a one-use socket ticket for the caller's own credential, no principal or link"},
	"POST /v0/channels/{channel}/messages":          {reauthNone, "a message"},
	"POST /v1/channels/{channel}/messages":          {reauthNone, "a message"},
	"POST /v2/channels/{channel}/messages":          {reauthNone, "a message"},
	"POST /v2/channels/{channel}/files":             {reauthNone, "a file"},
	"POST /v1/channels/{channel}/voice/session":     {reauthNone, "joins voice"},
	"POST /v1/channels/{channel}/voice/transmit":    {reauthNone, "a transmit report"},
	"POST /v1/channels/{channel}/voice/share":       {reauthNone, "a screen share"},
	"DELETE /v1/channels/{channel}/voice/share":     {reauthNone, "stops a share"},
	"POST /v1/channels/{channel}/voice/companion":   {reauthNone, "a companion's or viewer's join token"},
	"DELETE /v1/channels/{channel}/voice/companion": {reauthNone, "removes the caller's own companion"},
	"POST /v1/sessions":                             {reauthNone, "a session of the caller's own credential"},
	"POST /v1/sessions/{id}/heartbeat":              {reauthNone, "a heartbeat"},
	"DELETE /v1/sessions/{id}":                      {reauthNone, "ends the caller's own session"},
	"POST /v1/sessions/{id}/runs":                   {reauthNone, "an agent's run"},
	"POST /v1/runs/{id}/state":                      {reauthNone, "a run report"},
	"POST /v1/runs/{id}/activity":                   {reauthNone, "a run report"},
	"POST /v1/runs/{id}/interventions":              {reauthNone, "asks an agent to stop or change course"},
	"POST /v1/interventions/{id}/ack":               {reauthNone, "an agent's answer"},
	"PUT /v1/channels/{channel}/read-state":         {reauthNone, "the caller's read mark"},
	// Reactions (issue #765): authenticated self-writes, never an operator
	// grant, and two reduce-only moderation removals, like membership removal.
	"PUT /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}":                   {reauthNone, "the caller's own reaction"},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}":                {reauthNone, "takes back the caller's own reaction"},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}/{principal_id}": {reauthNone, "reduce-only: removes one reaction, grants nothing"},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactors/{principal_id}":          {reauthNone, "reduce-only: removes a person's reactions on one message, grants nothing"},
	"PUT /v2/channels/{channel}/messages/{message_id}/body":                                {reauthNone, "the caller's own message text (issue #845); grants nothing, and an operator has no exemption"},
	"DELETE /v1/credentials/current":                                                       {reauthNone, "signs the caller out"},
	"DELETE /v1/credentials/mine/{id}":                                                     {reauthNone, "ends one of the caller's own sign-ins"},
	"POST /v1/sign-in-links/mine":                                                          {reauthNone, "a member's own device link: the handler refuses operators and asks its own recent password proof (#712)"},
	"DELETE /v1/sign-in-links/mine/{id}":                                                   {reauthNone, "closes one of the caller's own device links"},
	"POST /v1/guest/end":                                                                   {reauthNone, "a guest ends its own visit"},
	"POST /v1/guest/convert":                                                               {reauthNone, "a guest makes its own account: no operator acts, and it proves itself with the visit's credential and, when the policy is closed, an operator's permit"},
	"POST /v1/password/mine":                                                               {reauthNone, "proves itself: a setup token or an enrolment link"},
	"PUT /v1/password/mine":                                                                {reauthNone, "proves itself: the current password, and a code for an operator"},
	"POST /v1/reauth":                                                                      {reauthNone, "is the proof"},
	"POST /v1/totp/enroll":                                                                 {reauthNone, "setup scope only"},
	"POST /v1/totp/confirm":                                                                {reauthNone, "setup scope only"},
	"POST /v1/reports":                                                                     {reauthNone, "a problem report"},
	"POST /v1/channels":                                                                    {reauthNone, "a member makes a channel: no grant beyond the creator's own membership of it"},
	"POST /v1/approvals":                                                                   {reauthNone, "an approval request (approval-path, unchanged)"},
	"POST /v1/approvals/{id}/decisions":                                                    {reauthNone, "a decision (approval-path, unchanged)"},
}

func TestRecentAuthClass(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	inTable := map[string]route{}
	for _, rt := range f.srv.routes {
		inTable[rt.pattern] = rt
		decide := rt.access == accessOperator || rt.access == accessOperatorOrSelf || rt.access == accessOwner ||
			((rt.access == accessAuthenticated || rt.access == accessChannelManager) && routeMethod(rt.pattern) != http.MethodGet)
		if !decide {
			if _, ok := recentAuthRoutes[rt.pattern]; ok {
				t.Errorf("%s is in the class but is neither an operator route nor an authenticated write", rt.pattern)
			}
			continue
		}
		d, ok := reauthDecisions[rt.pattern]
		if !ok {
			t.Errorf("route %s has no re-authentication decision: classify it by the rule of design §5.3", rt.pattern)
			continue
		}
		if (rt.access == accessAuthenticated || rt.access == accessChannelManager) && d.rule != reauthNone && d.rule != reauthOperators {
			t.Errorf("%s: an authenticated route is in the class for operators only, not %d", rt.pattern, d.rule)
		}
		if got := recentAuthRoutes[rt.pattern]; got != d.rule {
			t.Errorf("%s: class %d, decided %d (%s)", rt.pattern, got, d.rule, d.why)
		}
	}
	for p := range recentAuthRoutes {
		if _, ok := inTable[p]; !ok {
			t.Errorf("the class names %s, which the table does not have", p)
		}
	}
	for p := range reauthDecisions {
		if _, ok := inTable[p]; !ok {
			t.Errorf("a decision names %s, which the table does not have", p)
		}
	}
}

// The gate: on every route of the class an operator's browser session
// needs a standing proof (403 reauth_required, audited), a script
// credential does not (the S15 boundary), a route outside the class needs
// none, the proof runs out after ten minutes, and signing oneself out is not
// a bulk revoke.
func TestRecentAuthGate(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	_, session := f.operatorSession(t, secret)
	// The gate is held to a session with no proof: the sign-in's own
	// (issue #1033) is let run out first.
	f.advance(schema.RecentAuthLifetimeSeconds * time.Second)
	for pattern := range recentAuthRoutes {
		exp := routeExpectations[pattern]
		path := exp.path
		if pattern == "POST /v1/principals/{id}/credentials/revoke-all" {
			path = fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.alice.ID)
		}
		rec := f.send(t, routeMethod(pattern), path, session, exp.body, "")
		if rec.Code != http.StatusForbidden || decodeBody[schema.Error](t, rec).Code != schema.ErrorCodeReauthRequired {
			t.Errorf("%s on a session with no proof = %d %s, want 403 reauth_required", pattern, rec.Code, rec.Body)
		}
	}
	if rows := auditRows(t, f.authFixture, "access_denied"); len(rows) == 0 || !strings.Contains(rows[0].Detail, schema.ErrorCodeReauthRequired) {
		t.Errorf("the refusals were not audited: %+v", rows)
	}
	// The script credential is outside the class.
	if rec := f.sensitive(t, f.rootTok); rec.Code != http.StatusCreated {
		t.Errorf("the script credential = %d %s", rec.Code, rec.Body)
	}
	// Outside the class, the session needs no proof.
	if rec := f.send(t, "GET", "/v1/principals", session, "", ""); rec.Code != http.StatusOK {
		t.Errorf("a list on the session = %d", rec.Code)
	}
	// A member's invite, from a browser session, is not in the class: the
	// empty body is refused for what it is, not for want of a proof.
	ctx := context.Background()
	if _, err := f.srv.store.SetMayInvite(ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	_, aliceSession, err := f.srv.store.CreateRenewableCredential(ctx, "system", f.alice.ID, "browser", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.send(t, "POST", "/v1/invites", aliceSession, `{}`, ""), http.StatusBadRequest, "invalid_request")
	if rec := f.reauthOp(t, session, rootPassword, f.code(secret)); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	if rec := f.sensitive(t, session); rec.Code != http.StatusCreated {
		t.Errorf("with a proof = %d %s", rec.Code, rec.Body)
	}
	f.advance(schema.RecentAuthLifetimeSeconds * time.Second)
	assertErrorBody(t, f.sensitive(t, session), http.StatusForbidden, schema.ErrorCodeReauthRequired)
	// Signing oneself out everywhere is not the bulk revoke: the gate lets
	// it through with no proof. Last, since it ends the session.
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.root.ID), session, "", ""); rec.Code == http.StatusForbidden {
		t.Errorf("signing oneself out = %d %s", rec.Code, rec.Body)
	}
}

// signInWithCode signs root in with password and code at the fixture's clock
// and returns the session's token and the instant the code was accepted.
func (f *operatorFixture) signInWithCode(t *testing.T, secret []byte) (string, time.Time) {
	t.Helper()
	ch := f.rootChallenge(t, rootPassword, "")
	at := f.srv.now()
	rec := f.completeLogin(t, ch, f.code(secret), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login/totp = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.LoginResponseV1](t, rec).Token, at
}

// inviteNow is the operator's first invite, a route of the
// re-authentication class (audit 3 F8).
func (f *operatorFixture) inviteNow(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/invites", token, inviteJSON(t, personal("general")), "")
}

// A full sign-in with password and code is the recent proof for its first
// ten minutes (design §5.3, issue #1033, audit 3 F8): whoami shows it, the
// class admits the session within the window and asks again after it, and
// an older session of the same operator gets nothing from it.
func TestSignInIsTheRecentProof(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	_, older := f.operatorSession(t, secret)
	lifetime := schema.RecentAuthLifetimeSeconds * time.Second
	// The next sign-in comes after the older session's own proof ran out.
	f.advance(lifetime + time.Minute)
	session, signedIn := f.signInWithCode(t, secret)

	got := f.whoamiAuth(t, session).RecentAuthUntil
	if want := signedIn.Add(lifetime); got == nil || got.Time().Sub(want).Abs() > time.Second {
		t.Errorf("whoami recent_auth_until = %v, want %v ±1s", got, want)
	}
	if got := f.whoamiAuth(t, older).RecentAuthUntil; got != nil {
		t.Errorf("the older session holds a proof to %v", got)
	}
	tests := []struct {
		name  string
		at    time.Duration
		token string
		want  int
	}{
		{"the older session, a minute on", time.Minute, older, http.StatusForbidden},
		{"the new session, a minute on", time.Minute, session, http.StatusCreated},
		{"the new session, nine minutes on", 9 * time.Minute, session, http.StatusCreated},
		{"the new session, ten minutes and a second on", lifetime + time.Second, session, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.advance(signedIn.Add(tt.at).Sub(f.srv.now()))
			rec := f.inviteNow(t, tt.token)
			if tt.want == http.StatusForbidden {
				assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeReauthRequired)
				return
			}
			if rec.Code != tt.want {
				t.Errorf("invite = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
		})
	}
	// The sign-in is audited as one, not as a re-authentication.
	if n := len(auditRows(t, f.authFixture, store.AuditReauth)); n != 0 {
		t.Errorf("%d reauth rows, want none", n)
	}
}

// Every change that cleared a re-authentication's proof clears the
// sign-in's (issue #1033): a password change leaves the rotated session
// without one, and a second factor change (recovery) or signing out
// everywhere ends the session with it.
func TestSignInProofClears(t *testing.T) {
	tests := []struct {
		name string
		// clear makes the change and returns the token that is left, if any.
		clear func(t *testing.T, f *operatorFixture, secret []byte, session string) string
	}{
		{"a password change", func(t *testing.T, f *operatorFixture, secret []byte, session string) string {
			rec := f.send(t, "PUT", "/v1/password/mine", session,
				inviteJSON(t, schema.ChangePasswordRequestV1{Current: rootPassword, Password: newAlicePassword, Code: f.code(secret)}), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("change = %d %s", rec.Code, rec.Body)
			}
			return decodeBody[schema.PasswordUpdatedResponseV1](t, rec).Token
		}},
		{"a second factor change by recovery", func(t *testing.T, f *operatorFixture, _ []byte, _ string) string {
			if _, err := f.srv.store.RecoverOperator(context.Background(), f.root.ID, f.srv.now()); err != nil {
				t.Fatalf("RecoverOperator = %v", err)
			}
			return ""
		}},
		{"signing out everywhere", func(t *testing.T, f *operatorFixture, _ []byte, session string) string {
			if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.root.ID), session, "", ""); rec.Code >= 300 {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
			return ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOperatorFixture(t)
			secret := f.enrolOperator(t, f.root.ID)
			session, _ := f.signInWithCode(t, secret)
			f.nextStep()
			if f.whoamiAuth(t, session).RecentAuthUntil == nil {
				t.Fatal("the sign-in made no proof")
			}
			left := tt.clear(t, f, secret, session)
			if got := f.status(t, session); got != http.StatusUnauthorized {
				t.Errorf("the signed-in session = %d after the change, want 401", got)
			}
			if left == "" {
				return
			}
			if got := f.whoamiAuth(t, left).RecentAuthUntil; got != nil {
				t.Errorf("the session left holds a proof to %v", got)
			}
			assertErrorBody(t, f.inviteNow(t, left), http.StatusForbidden, schema.ErrorCodeReauthRequired)
		})
	}
}

// A password-only operator's sign-in and a member's are not the proof
// (issue #1033 keeps them out of scope): whoami shows none.
func TestPasswordSignInsAreNotTheProof(t *testing.T) {
	tests := []struct {
		name     string
		who      string
		password string
	}{
		{"a password-only operator on a temporary server", "root", rootPassword},
		{"a member", "alice", alicePassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newModeFixture(t, temporary)
			rec := f.login(t, tt.who, tt.password, "")
			resp := decodeBody[schema.LoginResponseV1](t, rec)
			if rec.Code != http.StatusOK || resp.Next != schema.LoginNextSignedIn {
				t.Fatalf("login = %d %s", rec.Code, rec.Body)
			}
			if got := f.whoamiAuth(t, resp.Token).RecentAuthUntil; got != nil {
				t.Errorf("the sign-in made a proof to %v", got)
			}
		})
	}
}

// A full-power script credential stays powerful (the residual risk the
// design records) but cannot make itself, or anything it mints, look like a
// browser session with a fresh second factor (design §5.3, S15).
func TestScriptCredentialCannotMintASession(t *testing.T) {
	f := newOperatorFixture(t)
	f.enrolOperator(t, f.root.ID)
	assertErrorBody(t, f.reauthOp(t, f.rootTok, rootPassword, "123456"), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.send(t, "POST", "/v1/totp/enroll", f.rootTok, "", ""), http.StatusForbidden, schema.ErrorCodeSetupOnHost)
	assertErrorBody(t, f.send(t, "POST", "/v1/password/mine", f.rootTok,
		inviteJSON(t, schema.SetPasswordRequestV1{Password: "a replacement and private passphrase"}), ""), http.StatusForbidden, schema.ErrorCodeSetupOnHost)
	// What it mints for the operator is a script credential too.
	rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.root.ID), f.rootTok, `{"label":"minted"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body)
	}
	minted := decodeBody[schema.CreateCredentialResponseV1](t, rec)
	if minted.Credential.Renews || minted.Credential.IdleExpiresAt != nil {
		t.Errorf("a script credential minted a session: %+v", minted.Credential)
	}
	assertErrorBody(t, f.reauthOp(t, minted.Token, rootPassword, "123456"), http.StatusForbidden, "forbidden")
	if got := f.whoamiAuth(t, minted.Token); got.RecentAuthUntil != nil || got.IdleExpiresAt != nil {
		t.Errorf("the minted credential: %+v", got)
	}
	// And no sign-in link is made for an operator.
	rec = f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.root.ID), f.rootTok, `{}`, "")
	if rec.Code < 400 {
		t.Errorf("a sign-in link for the operator = %d %s", rec.Code, rec.Body)
	}
}

// Offline recovery end to end: everything the operator held stops working,
// the old authenticator signs nobody in, and the setup token restores both
// factors, with a new authenticator secret.
func TestOperatorRecoveryEndToEnd(t *testing.T) {
	f := newOperatorFixture(t)
	oldSecret := f.enrolOperator(t, f.root.ID)
	_, session := f.operatorSession(t, oldSecret)
	pending := f.rootChallenge(t, rootPassword, "")
	rec, err := f.srv.store.RecoverOperator(context.Background(), f.root.ID, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"script credential": f.rootTok, "browser session": session} {
		if got := f.status(t, tok); got != http.StatusUnauthorized {
			t.Errorf("the %s = %d after recovery, want 401", name, got)
		}
	}
	assertErrorBody(t, f.completeLogin(t, pending, f.code(oldSecret), ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	// The password alone: no challenge, since the second factor is gone.
	assertErrorBody(t, f.login(t, "root", rootPassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)

	setup := f.redeemSetup(t, rec.Setup.Token)
	const newRoot = "a recovered and private passphrase"
	if r := f.send(t, "POST", "/v1/password/mine", setup.Token, inviteJSON(t, schema.SetPasswordRequestV1{Password: newRoot}), ""); r.Code != http.StatusOK {
		t.Fatalf("set password = %d %s", r.Code, r.Body)
	}
	newSecret := f.enroll(t, setup.Token)
	if string(newSecret) == string(oldSecret) {
		t.Fatal("recovery kept the authenticator's secret")
	}
	if r := f.confirm(t, setup.Token, f.code(newSecret)); r.Code != http.StatusNoContent {
		t.Fatalf("confirm = %d %s", r.Code, r.Body)
	}
	f.nextStep()
	ch := f.rootChallenge(t, newRoot, "")
	assertErrorBody(t, f.completeLogin(t, ch, f.code(oldSecret), ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if r := f.completeLogin(t, ch, f.code(newSecret), ""); r.Code != http.StatusOK {
		t.Errorf("sign-in with the new authenticator = %d %s", r.Code, r.Body)
	}
	actions := map[string]bool{}
	for _, e := range f.audit(t) {
		actions[e.Action] = true
	}
	for _, want := range []string{store.AuditOperatorRecovered, store.AuditAuthGenerationBumped, store.AuditSetupTokenIssued, store.AuditSetupTokenRedeemed} {
		if !actions[want] {
			t.Errorf("no %s in the audit trail", want)
		}
	}
}

// An operator changes their password with the current one and a code; the
// session they changed it from is rotated and keeps its bounds.
func TestOperatorChangePassword(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	cred, session := f.operatorSession(t, secret)
	change := func(code string) *httptest.ResponseRecorder {
		return f.send(t, "PUT", "/v1/password/mine", session,
			inviteJSON(t, schema.ChangePasswordRequestV1{Current: rootPassword, Password: newAlicePassword, Code: code}), "")
	}
	before := f.queryString(t, fmt.Sprintf("SELECT hash FROM passwords WHERE principal_id = %d", f.root.ID))
	assertErrorBody(t, change(""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	assertErrorBody(t, change(totpCodeAt(secret, totpStep(f.srv.now())+4)), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if after := f.queryString(t, fmt.Sprintf("SELECT hash FROM passwords WHERE principal_id = %d", f.root.ID)); after != before {
		t.Fatal("a refused change changed the password")
	}
	rec := change(f.code(secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.PasswordUpdatedResponseV1](t, rec)
	if got.Token == "" || got.Credential == nil || got.Credential.IdleExpiresAt == nil ||
		got.Credential.ExpiresAt == nil || !got.Credential.ExpiresAt.Time().Equal(cred.ExpiresAt.Time()) {
		t.Errorf("the rotated session %+v", got.Credential)
	}
	if f.status(t, session) != http.StatusUnauthorized || f.status(t, got.Token) != http.StatusOK || f.status(t, f.rootTok) != http.StatusUnauthorized {
		t.Error("after the change only the rotated session should work")
	}
}

// Who may enrol and confirm, and what everyone else is told.
func TestTOTPEnrollRefusals(t *testing.T) {
	tests := []struct {
		name   string
		do     func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"an operator's script credential", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			return f.send(t, "POST", "/v1/totp/enroll", f.rootTok, "", "")
		}, http.StatusForbidden, schema.ErrorCodeSetupOnHost},
		{"a member", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			return f.send(t, "POST", "/v1/totp/enroll", f.aliceTok, "", "")
		}, http.StatusForbidden, schema.ErrorCodeSetupOnHost},
		{"an agent", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			return f.send(t, "POST", "/v1/totp/enroll", f.botTok, "", "")
		}, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"a guest", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			return f.send(t, "POST", "/v1/totp/confirm", newGuest(t, f.srv, "", time.Hour, 2*time.Hour).Token, `{"code":"123456"}`, "")
		}, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		// An operator with an authenticator and no password is mid-setup:
		// the host gives a token, and the authenticator is not replaced.
		{"an operator who has one", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			f.enrolOperator(t, f.root.ID)
			f.exec(t, "DELETE FROM passwords WHERE principal_id = ?", f.root.ID)
			setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
			return f.send(t, "POST", "/v1/totp/enroll", setup.Token, "", "")
		}, http.StatusConflict, schema.ErrorCodeTOTPAlreadyEnrolled},
		{"a confirmation with nothing pending", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
			return f.confirm(t, setup.Token, "123456")
		}, http.StatusConflict, schema.ErrorCodeTOTPNotEnrolled},
		{"a confirmation of a replaced enrolment's secret", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
			first, second := f.enroll(t, setup.Token), f.enroll(t, setup.Token)
			// Two secrets share a step's code one time in a million; enrol
			// again rather than test a code both accept.
			for f.code(first) == f.code(second) {
				second = f.enroll(t, setup.Token)
			}
			return f.confirm(t, setup.Token, f.code(first))
		}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed},
		{"plain HTTP", func(t *testing.T, f *operatorFixture) *httptest.ResponseRecorder {
			setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
			return f.do(t, "POST", "/v1/totp/enroll", setup.Token, "")
		}, http.StatusForbidden, schema.ErrorCodeHTTPSRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, tt.do(t, newOperatorFixture(t)), tt.status, tt.code)
		})
	}
	// With authentication off there is no account to set up.
	off := newAuthFixture(t, AuthOff)
	for _, path := range []string{"/v1/totp/enroll", "/v1/totp/confirm", "/v1/login/totp", "/v1/setup/redeem"} {
		assertErrorBody(t, off.do(t, "POST", path, "", `{}`), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)
	}
}

// An enrolment that is never confirmed changes nothing: the operator still
// cannot sign in with the password, the setup credential runs out, and a
// new setup token starts over.
func TestTOTPEnrollmentInterrupted(t *testing.T) {
	f := newOperatorFixture(t)
	setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
	abandoned := f.enroll(t, setup.Token)
	assertErrorBody(t, f.login(t, "root", rootPassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	f.exec(t, "UPDATE credentials SET expires_at = ? WHERE scope = 'setup'", time.Now().UnixMilli())
	if got := f.confirm(t, setup.Token, f.code(abandoned)).Code; got != http.StatusUnauthorized {
		t.Errorf("confirm on an expired setup credential = %d, want 401", got)
	}
	secret := f.enrolOperator(t, f.root.ID)
	if string(secret) == string(abandoned) {
		t.Error("the new setup reused the abandoned secret")
	}
	f.operatorSession(t, secret)
}

// A redeemed setup token is spent, and every token that does not work gets
// the one 404.
func TestRedeemSetupTokenRefusals(t *testing.T) {
	f := newOperatorFixture(t)
	tok := f.hostSetupToken(t, f.root.ID)
	f.redeemSetup(t, tok)
	for name, body := range map[string]string{
		"spent":     inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: tok}),
		"unknown":   inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: schema.SetupTokenPrefix + strings.Repeat("B", 43)}),
		"malformed": inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: "conch_setup_short"}), // #nosec G101 -- test input, not a credential
		"a challenge": inviteJSON(t, schema.RedeemSetupTokenRequestV1{
			Token: schema.PendingLoginPrefix + strings.Repeat("B", 43)}),
	} {
		t.Run(name, func(t *testing.T) {
			assertErrorBody(t, f.send(t, "POST", "/v1/setup/redeem", "", body, ""), http.StatusNotFound, schema.ErrorCodeSetupTokenInvalid)
		})
	}
}

// Nothing the operator routes log, audit or answer holds a TOTP secret, a
// code, a challenge, a setup token or a credential's token, but the one
// response that issues each.
func TestOperatorSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newOperatorFixture(t)
	setupTok := f.hostSetupToken(t, f.root.ID)
	setup := f.redeemSetup(t, setupTok)
	rec := f.send(t, "POST", "/v1/totp/enroll", setup.Token, "", "")
	e := decodeBody[schema.TOTPEnrollmentV1](t, rec)
	secret, err := totpBase32.DecodeString(e.Secret)
	if err != nil {
		t.Fatal(err)
	}
	confirmCode := f.code(secret)
	f.confirm(t, setup.Token, confirmCode)
	f.nextStep()
	ch := f.rootChallenge(t, rootPassword, "")
	wrongCode := totpCodeAt(secret, totpStep(f.srv.now())+4)
	f.completeLogin(t, ch, wrongCode, "")
	_, session := f.operatorSession(t, secret)
	var bodies []string
	for _, r := range []*httptest.ResponseRecorder{
		f.send(t, "GET", "/v1/whoami", session, "", ""),
		f.send(t, "GET", fmt.Sprintf("/v1/principals/%d/credentials", f.root.ID), f.rootTok, "", ""),
		f.send(t, "GET", "/v1/audit", f.rootTok, "", ""),
	} {
		bodies = append(bodies, r.Body.String())
	}
	var audit []string
	for _, ev := range f.audit(t) {
		audit = append(audit, ev.Actor+" "+ev.Subject+" "+ev.Detail)
	}
	sealed := f.queryString(t, fmt.Sprintf("SELECT hex(secret) FROM totp WHERE principal_id = %d", f.root.ID))
	everything := logs.buf.String() + strings.Join(bodies, "\n") + strings.Join(audit, "\n")
	for name, s := range map[string]string{
		"the secret (base32)": e.Secret, "the secret (base64)": base64.StdEncoding.EncodeToString(secret),
		"the challenge": ch, "the setup token": setupTok, "the setup credential": setup.Token, "the session": session,
		"the sealed secret": sealed,
		"a code (text)":     "code=" + confirmCode, "a code (JSON)": `"` + confirmCode + `"`,
		"a wrong code (text)": "code=" + wrongCode, "a wrong code (JSON)": `"` + wrongCode + `"`,
	} {
		if strings.Contains(everything, s) {
			t.Errorf("%s appears in a log, an answer or the audit trail", name)
		}
	}
	if strings.Contains(sealed, fmt.Sprintf("%X", secret)) {
		t.Error("the stored secret is not sealed")
	}
}

// Without the key, or with the wrong one, every code fails closed: no
// sign-in, no attempt counted, a clear log line, and enrolment refused with
// a message that names the fix. Messaging works on as before.
func TestTOTPKeyMissingOrWrong(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	other, err := ParseTOTPKey(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", TOTPKeyBytes))))
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]*TOTPKey{"no key": nil, "the wrong key": other} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			srv := New(Config{AuthMode: AuthRequired, TOTPKey: key}, f.srv.store)
			srv.now = f.srv.now
			srv.setLoginLimits(looseLoginLimits())
			srv.setInviteLimits(looseInviteLimits())
			if !strings.Contains(logs.buf.String(), "CONCHD_TOTP_KEY") {
				t.Errorf("the start-up check said nothing about the key: %s", logs.buf.String())
			}
			g := &operatorFixture{&loginFixture{&inviteFixture{authFixture: &authFixture{srv: srv, root: f.root, alice: f.alice, rootTok: f.rootTok, aliceTok: f.aliceTok}, clock: f.clock}}}
			ch := g.rootChallenge(t, rootPassword, "")
			assertErrorBody(t, g.completeLogin(t, ch, f.code(secret), ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if n := f.queryString(t, "SELECT MAX(attempts) FROM pending_logins"); n != "0" {
				t.Errorf("a code that could not be checked counted as an attempt: %s", n)
			}
			if !strings.Contains(logs.buf.String(), "cannot be checked") {
				t.Error("no log line says why the code could not be checked")
			}
			if g.status(t, f.aliceTok) != http.StatusOK {
				t.Error("a member's credential stopped working")
			}
		})
	}
	// Enrolment with no key says what to fix.
	g := &operatorFixture{newLoginFixture(t, Config{})}
	setup := g.redeemSetup(t, g.hostSetupToken(t, g.root.ID))
	rec := g.send(t, "POST", "/v1/totp/enroll", setup.Token, "", "")
	assertErrorBody(t, rec, http.StatusInternalServerError, "internal_error")
	if !strings.Contains(rec.Body.String(), "CONCHD_TOTP_KEY") {
		t.Errorf("the refusal does not name the key: %s", rec.Body)
	}
}

// Setup tokens are limited per address as invite redemption is, ten tries
// in ten minutes whatever their outcome, in a bucket of their own: the
// eleventh from one address is a 429 with Retry-After, another address is
// not held back, and invite redemption from the first keeps its own budget.
func TestRedeemSetupTokenRateLimit(t *testing.T) {
	f := newOperatorFixture(t)
	f.srv.setupLimits = newSetupLimiter(Config{})
	f.srv.setInviteLimits(defaultInviteLimits())
	unknown := inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: schema.SetupTokenPrefix + strings.Repeat("C", 43)})
	for range defaultRedeemPerAddress {
		assertErrorBody(t, f.send(t, "POST", "/v1/setup/redeem", "", unknown, ""), http.StatusNotFound, schema.ErrorCodeSetupTokenInvalid)
	}
	rec := f.send(t, "POST", "/v1/setup/redeem", "", inviteJSON(t, schema.RedeemSetupTokenRequestV1{Token: f.hostSetupToken(t, f.root.ID)}), "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("the eleventh try = %d %s, want 429 with Retry-After", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.send(t, "POST", "/v1/setup/redeem", "", unknown, otherAddress), http.StatusNotFound, schema.ErrorCodeSetupTokenInvalid)
	assertErrorBody(t, f.send(t, "POST", "/v2/invites/redeem", "", redeemJSON(t, "nope", "x"), ""),
		http.StatusNotFound, schema.ErrorCodeInviteInvalid)
}

// An operator who holds a password and a confirmed second factor can sign
// in with them, so the last-operator guard counts them as a way in: the
// sole operator, on a browser session that is their only credential, can
// sign out, end a sign-in by id, and sign out everywhere. Disabling the last
// operator who can sign in is still refused, and is allowed once another
// operator holds both factors.
func TestSoleOperatorWithBothFactorsCanSignOut(t *testing.T) {
	ctx := context.Background()
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	only := func(t *testing.T) (schema.CredentialV1, string) {
		t.Helper()
		cred, tok := f.operatorSession(t, secret)
		live, err := f.srv.store.ListLiveCredentials(ctx, f.root.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range live {
			if c.ID != cred.ID {
				if err := f.srv.store.RevokeCredential(ctx, "system", c.ID); err != nil {
					t.Fatalf("revoke %d: %v", c.ID, err)
				}
			}
		}
		return cred, tok
	}
	_, tok := only(t)
	if rec := f.send(t, "DELETE", "/v1/credentials/current", tok, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("signing out = %d %s, want 204", rec.Code, rec.Body)
	}
	cred, tok := only(t)
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/credentials/mine/%d", cred.ID), tok, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("ending the sign-in by id = %d %s, want 204", rec.Code, rec.Body)
	}
	_, tok = only(t)
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.root.ID), tok, "", ""); rec.Code >= 300 {
		t.Errorf("signing out everywhere = %d %s", rec.Code, rec.Body)
	}
	if got := f.status(t, tok); got != http.StatusUnauthorized {
		t.Errorf("the session = %d after signing out everywhere, want 401", got)
	}
	// The operator still signs in with both factors.
	f.operatorSession(t, secret)

	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.root.ID); !errors.Is(err, store.ErrLastOperator) {
		t.Errorf("disabling the last operator who can sign in = %v, want ErrLastOperator", err)
	}
	// Another owner with both factors, and no credential at all, is a way
	// in: now the disable is allowed.
	f.exec(t, "UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", f.alice.ID)
	f.exec(t, "INSERT INTO totp (principal_id, secret, enrolled_at) VALUES (?, x'01', 1)", f.alice.ID)
	f.exec(t, "UPDATE credentials SET revoked_at = 1 WHERE principal_id = ?", f.alice.ID)
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.root.ID); err != nil {
		t.Errorf("disabling root with another operator able to sign in = %v", err)
	}
}
