// Command temporary-check is the end-to-end check of temporary servers and
// password-only operators (issues #866 and #875,
// docs/adr/ADR-011-temporary-servers.md, docs/adr/ADR-012-optional-operator-totp.md).
// It builds conchd, runs it for real and calls the public API only, in two
// parts on one data directory and two on another:
//
//   - Temporary mode (CONCHD_MODE=temporary): /healthz says so. The first
//     operator is bootstrapped with a setup link (--link --origin), redeems
//     it, sets a password and is done: whoami says setup is complete, the
//     setup credential can no longer set a password, and Skip ends it. The
//     operator signs in with the password alone, re-authenticates with it,
//     makes a script credential and a second operator, and changes the
//     password with no code. The second operator sets a password, then takes
//     the authenticator step that stays on offer, and from then on is asked
//     for the code. Left behind on purpose: the first operator's browser
//     session with a recent-authentication proof, the second's session, a
//     pending sign-in of the second's, and an unfinished setup credential.
//   - The same data restarted in normal mode (CONCHD_MODE unset): /healthz
//     says nothing of a mode; every one of those is dead; a password-only
//     sign-in is refused as setup incomplete; the script credential still
//     works; the audit trail has the server_mode rows with the counts; and
//     the enrolled operator still signs in with password and code.
//   - A fresh server in normal mode with CONCHD_OPERATOR_TOTP=optional: no
//     banner anywhere; the operator sets up with a password alone, Skip ends
//     the setup credential, and the password alone signs in and
//     re-authenticates.
//   - The same data restarted with CONCHD_OPERATOR_TOTP=required: the
//     session is dead, the script credential works, a password-only sign-in
//     is refused, and the trail and the log count the operator left without
//     an authenticator.
//
// No credential, password, token, link or key of the run may appear in
// conchd's log. It prints one "temporary-check: PASS" line when everything
// held, and exits nonzero on the first thing that did not. A SIGINT or
// SIGTERM stops conchd and removes what the run made.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238's HMAC-SHA-1, the algorithm conchd's second factor uses (schema.TOTPAlgorithm)
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// origin is the public address the setup links are printed for. Nothing
// resolves it: the run takes the token out of the link, as the page would.
const origin = "https://temporary-check.invalid"

// The run's people. Each password passes the server's policy.
const (
	firstName      = "temp-operator"
	secondName     = "temp-second"
	firstPassword  = "a temporary operator's long passphrase"
	firstChanged   = "a temporary operator's changed passphrase"
	secondPassword = "the second operator's long passphrase"
)

var stopCtx, stopCancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	defer stopCancel()
	r, err := newRun()
	if err != nil {
		fmt.Fprintln(os.Stderr, "temporary-check: FAIL:", err)
		return 1
	}
	start := time.Now()
	err = r.check()
	r.teardown()
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "temporary-check: interrupted")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "temporary-check: FAIL:", r.redact(err.Error()))
		return 1
	}
	fmt.Printf("temporary-check: PASS (%s)\n", time.Since(start).Round(time.Second))
	return 0
}

// run is one run's state: where it builds and keeps its data, conchd's
// environment, the secrets that must never be printed or logged, and the
// running conchd.
type run struct {
	tmp, conchd, dataDir string
	env                  []string
	mu                   sync.Mutex
	secrets              map[string]string
	logs                 []string
	proc                 *conchdProc
}

func newRun() (*run, error) {
	if _, err := os.Stat("go.mod"); err != nil {
		return nil, errors.New("run this from the module root: go run ./e2e/temporary")
	}
	tmp, err := os.MkdirTemp("", "temporary-check-")
	if err != nil {
		return nil, err
	}
	r := &run{tmp: tmp, dataDir: filepath.Join(tmp, "data"), secrets: map[string]string{}}
	if err := os.MkdirAll(r.dataDir, 0o700); err != nil {
		return nil, err
	}
	// The host key operators' authenticators are sealed under: random for
	// each run, in the environment only, as in production. A temporary
	// instance has one too (ADR-011 item 2.1), or the authenticator step
	// it offers would fail.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	totpKey := base64.StdEncoding.EncodeToString(key)
	r.secret("the server's TOTP key", totpKey)
	for _, kv := range os.Environ() {
		// Nothing about who conchd is may come in from the caller.
		if strings.HasPrefix(kv, "CONCHD_") || strings.HasPrefix(kv, "CONCH_") {
			continue
		}
		r.env = append(r.env, kv)
	}
	r.env = append(r.env, "CONCHD_TOTP_KEY="+totpKey)
	for _, s := range []string{firstPassword, firstChanged, secondPassword} {
		r.secret("a password", s)
	}
	return r, nil
}

func (r *run) secret(label, value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	r.secrets[value] = label
	r.mu.Unlock()
}

func (r *run) redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for v, label := range r.secrets {
		s = strings.ReplaceAll(s, v, "["+label+" redacted]")
	}
	return s
}

func (r *run) say(format string, args ...any) { fmt.Println(r.redact(fmt.Sprintf(format, args...))) }

func (r *run) teardown() {
	if r.proc != nil {
		r.proc.stop()
	}
	_ = os.RemoveAll(r.tmp)
}

// check is the whole run.
func (r *run) check() error {
	if err := r.build(); err != nil {
		return err
	}
	if err := r.temporaryPart(); err != nil {
		return fmt.Errorf("temporary mode: %w", err)
	}
	if err := r.normalPart(); err != nil {
		return fmt.Errorf("back to normal mode: %w", err)
	}
	if err := r.optionalPart(); err != nil {
		return fmt.Errorf("normal mode with CONCHD_OPERATOR_TOTP=optional: %w", err)
	}
	if err := r.requiredPart(); err != nil {
		return fmt.Errorf("restarted with CONCHD_OPERATOR_TOTP=required: %w", err)
	}
	return r.scanLogs()
}

// envFor is conchd's environment for this run's data directory, with extra
// settings after it.
func (r *run) envFor(extra ...string) []string {
	env := append([]string(nil), r.env...)
	env = append(env, "CONCHD_DATA="+r.dataDir)
	return append(env, extra...)
}

// left is what the temporary part leaves for the normal part to find dead,
// and alive.
var left struct {
	firstSession, secondSession, openSetup string // must be dead
	pendingChallenge                       string // must be dead
	script                                 string // must work
	firstID                                int64
	second                                 *authenticator
}

func (r *run) temporaryPart() error {
	link, err := r.host("operator", "bootstrap", "--link", "--origin", origin, firstName)
	if err != nil {
		return err
	}
	setupToken, err := tokenOfLink(link)
	if err != nil {
		return fmt.Errorf("operator bootstrap --link: %w", err)
	}
	r.secret("a setup link", link)
	r.secret("a setup token", setupToken)
	r.say("ok   operator bootstrap --link printed one link, %s/app/#/setup/<token>, alone on stdout", origin)

	if err := r.start("CONCHD_MODE=temporary"); err != nil {
		return err
	}
	if err := r.healthMode(schema.ServerModeTemporary); err != nil {
		return err
	}
	anon := r.api("")

	// Setup with a password alone.
	var redeemed schema.RedeemSetupTokenResponseV1
	if err := anon.call(http.MethodPost, "/v1/setup/redeem", schema.RedeemSetupTokenRequestV1{Token: setupToken}, &redeemed); err != nil {
		return err
	}
	r.secret("a setup credential", redeemed.Token)
	left.firstID = redeemed.Principal.ID
	setup := r.api(redeemed.Token)
	if err := setup.whoami(func(w schema.WhoAmIResponseV1) error {
		if w.Auth == nil || w.Auth.Scope != schema.CredentialScopeSetup || w.Auth.SetupComplete || w.ServerMode != schema.ServerModeTemporary {
			return fmt.Errorf("whoami before the password = %+v", w)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setup.call(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: firstPassword}, nil); err != nil {
		return err
	}
	if err := setup.whoami(func(w schema.WhoAmIResponseV1) error {
		if w.Auth == nil || !w.Auth.PasswordSet || w.Auth.TOTPEnrolled || !w.Auth.SetupComplete {
			return fmt.Errorf("whoami after the password = %+v, want setup complete", w.Auth)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setup.refused(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: "another long passphrase here"},
		http.StatusForbidden, schema.ErrorCodeSetupIncomplete); err != nil {
		return err
	}
	if err := setup.refused(http.MethodGet, "/v1/channels", nil, http.StatusForbidden, schema.ErrorCodeSetupIncomplete); err != nil {
		return err
	}
	if err := setup.call(http.MethodDelete, "/v1/credentials/current", nil, nil); err != nil {
		return fmt.Errorf("skip: %w", err)
	}
	if err := setup.refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return err
	}
	r.say("ok   set up with a password alone: setup complete, the setup credential refused a second password, and Skip ended it")

	// Sign in, re-authenticate, change the password: no code anywhere.
	first, err := r.passwordSignIn(firstName, firstPassword)
	if err != nil {
		return err
	}
	// That session is the only owner's only credential. The password signs
	// them in again, so signing out of it is not refused as the last way in.
	if err := first.call(http.MethodDelete, "/v1/credentials/current", nil, nil); err != nil {
		return fmt.Errorf("the only owner signing out of their only session: %w", err)
	}
	if first, err = r.passwordSignIn(firstName, firstPassword); err != nil {
		return err
	}
	if err := first.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: firstPassword}, nil); err != nil {
		return err
	}
	var made schema.CreatePrincipalResponse
	if err := first.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: secondName}, &made); err != nil {
		return err
	}
	if err := first.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/role", made.Principal.ID),
		schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
		return fmt.Errorf("the role route (ADR-010) after a password-only proof: %w", err)
	}
	var changed schema.PasswordUpdatedResponseV1
	if err := first.call(http.MethodPut, "/v1/password/mine", schema.ChangePasswordRequestV1{Current: firstPassword, Password: firstChanged}, &changed); err != nil {
		return err
	}
	r.secret("a browser session", changed.Token)
	if err := first.refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return fmt.Errorf("the session the change rotated: %w", err)
	}
	first = r.api(changed.Token)
	if err := first.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: firstChanged}, nil); err != nil {
		return err
	}
	if err := first.whoami(func(w schema.WhoAmIResponseV1) error {
		if w.Auth == nil || w.Auth.RecentAuthUntil == nil || w.Auth.IdleExpiresAt == nil {
			return fmt.Errorf("whoami of the operator's session = %+v, want a recent-auth proof and an idle bound", w.Auth)
		}
		return nil
	}); err != nil {
		return err
	}
	// Made after the change, which ends every other credential of the
	// operator's, as a change always does.
	var script schema.CreateCredentialResponseV1
	if err := first.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", left.firstID),
		schema.CreateCredentialRequestV1{Label: "temporary-check script"}, &script); err != nil {
		return fmt.Errorf("a route of the re-authentication class after a password-only proof: %w", err)
	}
	r.secret("the script credential", script.Token)
	left.script = script.Token
	left.firstSession = changed.Token
	r.say("ok   signed in, signed out of the only session and in again, re-authenticated (then made an operator), changed the password and made a script credential, all with no code")

	// The second operator takes the authenticator step after the password.
	link, err = r.host("operator", "setup", "--link", "--origin", origin, fmt.Sprint(made.Principal.ID))
	if err != nil {
		return err
	}
	secondToken, err := tokenOfLink(link)
	if err != nil {
		return fmt.Errorf("operator setup --link: %w", err)
	}
	r.secret("a setup link", link)
	r.secret("a setup token", secondToken)
	if err := anon.call(http.MethodPost, "/v1/setup/redeem", schema.RedeemSetupTokenRequestV1{Token: secondToken}, &redeemed); err != nil {
		return err
	}
	r.secret("a setup credential", redeemed.Token)
	setup = r.api(redeemed.Token)
	if err := setup.call(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: secondPassword}, nil); err != nil {
		return err
	}
	var enrolment schema.TOTPEnrollmentV1
	if err := setup.call(http.MethodPost, "/v1/totp/enroll", nil, &enrolment); err != nil {
		return fmt.Errorf("the authenticator step after the password: %w", err)
	}
	r.secret("an authenticator secret", enrolment.Secret)
	secret, err := decodeTOTPSecret(enrolment.Secret)
	if err != nil {
		return err
	}
	left.second = &authenticator{secret: secret}
	code, err := left.second.code()
	if err != nil {
		return err
	}
	if err := setup.call(http.MethodPost, "/v1/totp/confirm", schema.ConfirmTOTPRequestV1{Code: code}, nil); err != nil {
		return err
	}
	if err := setup.refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return fmt.Errorf("the setup credential after the enrolment: %w", err)
	}
	second, err := r.codeSignIn(secondName, secondPassword, left.second)
	if err != nil {
		return err
	}
	left.secondSession = second.token
	if err := second.refused(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: secondPassword}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed); err != nil {
		return fmt.Errorf("an enrolled operator's re-authentication with no code: %w", err)
	}
	if left.pendingChallenge, err = r.challenge(secondName, secondPassword); err != nil {
		return err
	}
	r.say("ok   a second operator set a password, enrolled an authenticator on the same setup credential, and is asked for the code")

	// An unfinished setup credential.
	tok, err := r.host("operator", "setup", fmt.Sprint(left.firstID))
	if err != nil {
		return err
	}
	r.secret("a setup token", tok)
	if err := anon.call(http.MethodPost, "/v1/setup/redeem", schema.RedeemSetupTokenRequestV1{Token: tok}, &redeemed); err != nil {
		return err
	}
	r.secret("a setup credential", redeemed.Token)
	left.openSetup = redeemed.Token
	for what, tok := range map[string]string{
		"the first operator's session": left.firstSession, "the second operator's session": left.secondSession,
		"the open setup credential": left.openSetup, "the script credential": left.script,
	} {
		if status, _, err := r.api(tok).do(http.MethodGet, "/v1/whoami", nil); err != nil || status != http.StatusOK {
			return statusErr(what+" before the restart", status, err)
		}
	}
	return nil
}

func (r *run) normalPart() error {
	r.proc.stop()
	r.proc = nil
	if err := r.start(); err != nil {
		return err
	}
	if err := r.healthMode(""); err != nil {
		return err
	}
	for what, tok := range map[string]string{
		"the first operator's browser session (with its proof)":   left.firstSession,
		"the second operator's browser session (an enrolled one)": left.secondSession,
		"the unfinished setup credential":                         left.openSetup,
	} {
		if err := r.api(tok).refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
			return fmt.Errorf("%s after the restart: %w", what, err)
		}
	}
	// The pending sign-in, with a right code, is refused: it was spent.
	code, err := left.second.peek()
	if err != nil {
		return err
	}
	if err := r.api("").refused(http.MethodPost, "/v1/login/totp",
		schema.TOTPLoginRequestV1{Challenge: left.pendingChallenge, Code: code}, http.StatusUnauthorized, schema.ErrorCodeAuthFailed); err != nil {
		return fmt.Errorf("the pending sign-in after the restart: %w", err)
	}
	r.say("ok   restarted in normal mode: both operators' browser sessions, the proof, the pending sign-in and the setup credential are dead")

	if err := r.api("").refused(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: firstName, Password: firstChanged},
		http.StatusUnauthorized, schema.ErrorCodeAuthFailed); err != nil {
		return fmt.Errorf("a password-only sign-in on a normal server: %w", err)
	}
	script := r.api(left.script)
	if err := script.whoami(func(w schema.WhoAmIResponseV1) error {
		if w.Role != schema.RoleOperator || w.ServerMode != "" || w.Auth == nil || w.Auth.SetupComplete {
			return fmt.Errorf("whoami of the script credential = %+v (auth %+v), want an operator whose setup is incomplete on a normal server", w, w.Auth)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("the script credential after the restart: %w", err)
	}
	events, err := script.audit()
	if err != nil {
		return err
	}
	var modes, refusals []string
	for _, e := range events {
		switch {
		case e.Action == "server_mode":
			modes = append(modes, e.Detail)
		case e.Action == "login_failed" && strings.Contains(e.Detail, "reason=setup_incomplete"):
			refusals = append(refusals, e.Detail)
		}
	}
	wantModes := []string{
		"mode=temporary password_only=true first_start=true",
		"mode=temporary->normal password_only=true->false sessions_revoked=2 proofs_cleared=1 pending_logins_spent=1 setup_credentials_revoked=1 operators_without_authenticator=1",
	}
	if strings.Join(modes, "\n") != strings.Join(wantModes, "\n") {
		return fmt.Errorf("server_mode rows %q, want %q", modes, wantModes)
	}
	if len(refusals) != 1 {
		return fmt.Errorf("login_failed setup_incomplete rows %q, want one", refusals)
	}
	r.say("ok   a password-only sign-in is refused as setup incomplete, the script credential works, and the trail says %q", wantModes[1])

	if _, err := r.codeSignIn(secondName, secondPassword, left.second); err != nil {
		return fmt.Errorf("the enrolled operator on a normal server: %w", err)
	}
	r.say("ok   the enrolled operator signs in with password and code as on any normal server")
	return nil
}

// optional is what the optional part leaves for the required part.
var optional struct {
	session, script string
}

// optionalPart is ADR-012 on a fresh, normal server: password-only setup,
// sign-in, re-authentication and Skip with CONCHD_OPERATOR_TOTP=optional,
// and no banner anywhere.
func (r *run) optionalPart() error {
	r.proc.stop()
	r.proc = nil
	r.dataDir = filepath.Join(r.tmp, "data-optional")
	if err := os.MkdirAll(r.dataDir, 0o700); err != nil {
		return err
	}
	link, err := r.host("operator", "bootstrap", "--link", "--origin", origin, firstName)
	if err != nil {
		return err
	}
	setupToken, err := tokenOfLink(link)
	if err != nil {
		return fmt.Errorf("operator bootstrap --link: %w", err)
	}
	r.secret("a setup link", link)
	r.secret("a setup token", setupToken)
	if err := r.start("CONCHD_OPERATOR_TOTP=optional"); err != nil {
		return err
	}
	if err := r.healthMode(""); err != nil {
		return err
	}
	var redeemed schema.RedeemSetupTokenResponseV1
	if err := r.api("").call(http.MethodPost, "/v1/setup/redeem", schema.RedeemSetupTokenRequestV1{Token: setupToken}, &redeemed); err != nil {
		return err
	}
	r.secret("a setup credential", redeemed.Token)
	setup := r.api(redeemed.Token)
	if err := setup.call(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: firstPassword}, nil); err != nil {
		return err
	}
	if err := setup.whoami(func(w schema.WhoAmIResponseV1) error {
		if w.Auth == nil || !w.Auth.SetupComplete || w.Auth.TOTPEnrolled || w.Auth.Scope != schema.CredentialScopeSetup || w.ServerMode != "" {
			return fmt.Errorf("whoami after the password = %+v (auth %+v), want setup complete on a setup credential and no server_mode", w, w.Auth)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := setup.refused(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: "another long passphrase here"},
		http.StatusForbidden, schema.ErrorCodeSetupIncomplete); err != nil {
		return err
	}
	if err := setup.call(http.MethodDelete, "/v1/credentials/current", nil, nil); err != nil {
		return fmt.Errorf("skip: %w", err)
	}
	if err := setup.refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return err
	}
	first, err := r.passwordSignIn(firstName, firstPassword)
	if err != nil {
		return err
	}
	if err := first.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: firstPassword}, nil); err != nil {
		return err
	}
	var script schema.CreateCredentialResponseV1
	if err := first.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", redeemed.Principal.ID),
		schema.CreateCredentialRequestV1{Label: "temporary-check script"}, &script); err != nil {
		return err
	}
	r.secret("the script credential", script.Token)
	status, body, err := first.do(http.MethodGet, "/v1/whoami", nil)
	if err != nil || status != http.StatusOK {
		return statusErr("whoami of the session", status, err)
	}
	if bytes.Contains(body, []byte("server_mode")) {
		return fmt.Errorf("whoami of the session: %s, want no server_mode", r.redact(string(body)))
	}
	optional.session, optional.script = first.token, script.Token
	r.say("ok   normal mode, CONCHD_OPERATOR_TOTP=optional: no banner; set up with a password alone, Skip, signed in and re-authenticated with no code")
	return nil
}

// requiredPart restarts the optional part's data with the authenticator
// required: what password-only made is ended, and the trail and the log say
// how many operators are left without an authenticator (ADR-012 item 4).
func (r *run) requiredPart() error {
	r.proc.stop()
	r.proc = nil
	if err := r.start("CONCHD_OPERATOR_TOTP=required"); err != nil {
		return err
	}
	if err := r.api(optional.session).refused(http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return fmt.Errorf("the password-only session: %w", err)
	}
	if err := r.api("").refused(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: firstName, Password: firstPassword},
		http.StatusUnauthorized, schema.ErrorCodeAuthFailed); err != nil {
		return fmt.Errorf("a password-only sign-in: %w", err)
	}
	script := r.api(optional.script)
	events, err := script.audit()
	if err != nil {
		return fmt.Errorf("the script credential: %w", err)
	}
	var modes []string
	for _, e := range events {
		if e.Action == "server_mode" {
			modes = append(modes, e.Detail)
		}
	}
	wantModes := []string{
		"mode=normal password_only=true first_start=true",
		"mode=normal->normal password_only=true->false sessions_revoked=1 proofs_cleared=1 pending_logins_spent=0 setup_credentials_revoked=0 operators_without_authenticator=1",
	}
	if strings.Join(modes, "\n") != strings.Join(wantModes, "\n") {
		return fmt.Errorf("server_mode rows %q, want %q", modes, wantModes)
	}
	logged, err := os.ReadFile(r.logs[len(r.logs)-1]) // #nosec G304 -- a path under this run's temp dir
	if err != nil {
		return err
	}
	if !bytes.Contains(logged, []byte("operators_without_authenticator=1")) {
		return errors.New("conchd did not log how many operators are left without an authenticator")
	}
	r.say("ok   restarted with CONCHD_OPERATOR_TOTP=required: the session is dead, the script credential works, and the trail and the log count the one operator left without an authenticator")
	return nil
}

// tokenOfLink is the setup token of a setup link, checking the link is
// exactly origin/app/#/setup/<token>.
func tokenOfLink(link string) (string, error) {
	tok, ok := strings.CutPrefix(link, origin+"/app/#/setup/")
	if !ok || schema.ValidateSetupToken(tok) != nil {
		return "", fmt.Errorf("stdout is not a setup link for %s alone (%d bytes)", origin, len(link))
	}
	return tok, nil
}

// passwordSignIn signs name in with a password alone and returns the
// session, which must be the operator's browser session.
func (r *run) passwordSignIn(name, password string) (api, error) {
	var resp schema.LoginResponseV1
	if err := r.api("").call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: password}, &resp); err != nil {
		return api{}, err
	}
	r.secret("a browser session", resp.Token)
	if resp.Next != schema.LoginNextSignedIn || resp.Credential == nil || resp.Credential.IdleExpiresAt == nil || resp.Credential.Renews {
		return api{}, fmt.Errorf("a password-only sign-in answered next=%s credential %+v, want the operator's browser session", resp.Next, resp.Credential)
	}
	return r.api(resp.Token), nil
}

// challenge signs name in with the password and returns the challenge the
// answer must be.
func (r *run) challenge(name, password string) (string, error) {
	var resp schema.LoginResponseV1
	if err := r.api("").call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: password}, &resp); err != nil {
		return "", err
	}
	r.secret("a sign-in challenge", resp.Challenge)
	if resp.Next != schema.LoginNextTOTP || resp.Challenge == "" || resp.Token != "" {
		return "", fmt.Errorf("an enrolled operator's sign-in answered next=%s, want a challenge", resp.Next)
	}
	return resp.Challenge, nil
}

// codeSignIn signs an enrolled operator in with both factors.
func (r *run) codeSignIn(name, password string, a *authenticator) (api, error) {
	challenge, err := r.challenge(name, password)
	if err != nil {
		return api{}, err
	}
	code, err := a.code()
	if err != nil {
		return api{}, err
	}
	var resp schema.LoginResponseV1
	if err := r.api("").call(http.MethodPost, "/v1/login/totp", schema.TOTPLoginRequestV1{Challenge: challenge, Code: code}, &resp); err != nil {
		return api{}, err
	}
	r.secret("a browser session", resp.Token)
	if resp.Next != schema.LoginNextSignedIn || resp.Token == "" {
		return api{}, fmt.Errorf("the code step answered next=%s", resp.Next)
	}
	return r.api(resp.Token), nil
}

// ------------------------------------------------------------------ conchd

func (r *run) build() error {
	r.conchd = filepath.Join(r.tmp, "conchd")
	cmd := exec.CommandContext(stopCtx, "go", "build", "-o", r.conchd, "./cmd/conchd") // #nosec G204 -- constant command; the output is this run's temp dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/conchd: %w\n%s", err, out)
	}
	return nil
}

// host runs a conchd host command on the data directory, as the operator
// would on the host, and returns its stdout, which must be one line.
func (r *run) host(args ...string) (string, error) {
	cmd := exec.CommandContext(stopCtx, r.conchd, args...) // #nosec G204 -- the binary this run built; args are this program's constants
	cmd.Env = r.envFor()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("conchd %s: %w: %s", strings.Join(args, " "), err, r.redact(stderr.String()))
	}
	if strings.Count(string(out), "\n") != 1 {
		return "", fmt.Errorf("conchd %s printed %d lines on stdout, want one", strings.Join(args, " "), strings.Count(string(out), "\n"))
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

type conchdProc struct {
	cmd     *exec.Cmd
	baseURL string
	done    chan struct{}
}

// start serves the data directory with the settings extra (none leaves
// CONCHD_MODE and CONCHD_OPERATOR_TOTP unset) and waits for /healthz.
func (r *run) start(extra ...string) error {
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	env := r.envFor(extra...)
	logPath := filepath.Join(r.tmp, fmt.Sprintf("conchd-%d.log", len(r.logs)+1))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this run's temp dir
	if err != nil {
		return err
	}
	r.logs = append(r.logs, logPath)
	cmd := exec.Command(r.conchd, "serve", "--listen", addr) // #nosec G204 -- the binary this run built
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	p := &conchdProc{cmd: cmd, baseURL: "http://" + addr, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(p.done) }()
	r.proc = p
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(p.baseURL + "/healthz") // #nosec G107 -- this run's own conchd
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("conchd exited at start; its log: %s", r.redact(tail(logPath)))
		case <-stopCtx.Done():
			return errors.New("interrupted")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("conchd did not answer /healthz within 20s; its log: %s", r.redact(tail(logPath)))
		}
	}
}

// stop ends conchd as systemd would, and waits for it.
func (p *conchdProc) stop() {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func tail(path string) string {
	data, err := os.ReadFile(path) // #nosec G304 -- a path under this run's temp dir
	if err != nil {
		return err.Error()
	}
	if len(data) > 2000 {
		data = data[len(data)-2000:]
	}
	return string(data)
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

// healthMode checks /healthz says want, and nothing at all of a mode when
// want is empty (ADR-011 item 2.3).
func (r *run) healthMode(want schema.ServerMode) error {
	status, body, err := r.api("").do(http.MethodGet, "/healthz", nil)
	if err != nil || status != http.StatusOK {
		return statusErr("healthz", status, err)
	}
	var h schema.Health
	if err := errors.Join(json.Unmarshal(body, &h), h.Validate()); err != nil {
		return fmt.Errorf("healthz %s: %w", body, err)
	}
	if h.ServerMode != want || bytes.Contains(body, []byte("server_mode")) != (want != "") {
		return fmt.Errorf("healthz %s, want server_mode %q", body, want)
	}
	r.say("ok   /healthz: %s", strings.TrimSpace(string(body)))
	return nil
}

// scanLogs checks that no secret of the run is in conchd's logs.
func (r *run) scanLogs() error {
	for _, path := range r.logs {
		data, err := os.ReadFile(path) // #nosec G304 -- a path under this run's temp dir
		if err != nil {
			return err
		}
		r.mu.Lock()
		for v, label := range r.secrets {
			if bytes.Contains(data, []byte(v)) {
				r.mu.Unlock()
				return fmt.Errorf("secret leak: %s appears in %s", label, filepath.Base(path))
			}
		}
		r.mu.Unlock()
	}
	r.say("ok   no credential, password, token, link or key of the run is in conchd's logs")
	return nil
}

// -------------------------------------------------------------------- REST

type api struct {
	r     *run
	base  string
	token string
}

func (r *run) api(token string) api { return api{r: r, base: r.proc.baseURL, token: token} }

var httpClient = &http.Client{Timeout: 20 * time.Second}

func (a api) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(stopCtx, method, a.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

// call makes a request that must succeed, and decodes the answer into out.
func (a api) call(method, path string, body, out any) error {
	status, data, err := a.do(method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, status, a.r.redact(string(data)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

// refused makes a request that must be answered status with error code.
func (a api) refused(method, path string, body any, status int, code string) error {
	got, data, err := a.do(method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	var e schema.Error
	_ = json.Unmarshal(data, &e)
	if got != status || e.Code != code {
		return fmt.Errorf("%s %s answered %d %s, want %d %s", method, path, got, a.r.redact(string(data)), status, code)
	}
	return nil
}

func (a api) whoami(check func(schema.WhoAmIResponseV1) error) error {
	var w schema.WhoAmIResponseV1
	if err := a.call(http.MethodGet, "/v1/whoami", nil, &w); err != nil {
		return err
	}
	if err := w.Validate(); err != nil {
		return fmt.Errorf("whoami does not validate: %w", err)
	}
	return check(w)
}

// audit reads the whole trail (GET /v1/audit, JSON Lines).
func (a api) audit() ([]schema.AuditEventV1, error) {
	status, data, err := a.do(http.MethodGet, "/v1/audit", nil)
	if err != nil || status != http.StatusOK {
		return nil, statusErr("GET /v1/audit", status, err)
	}
	var out []schema.AuditEventV1
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e schema.AuditEventV1
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("GET /v1/audit: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ----------------------------------------------------------- authenticator

// authenticator is an operator's authenticator app: RFC 6238 with the
// parameters conchd enrols (schema.TOTPAlgorithm, TOTPDigits,
// TOTPPeriodSeconds), written from the RFC rather than taken from the
// server, as e2e/isolation's is. conchd accepts each step once, so it
// remembers the last step it gave.
type authenticator struct {
	secret []byte
	last   int64
}

func totpStep(t time.Time) int64 { return t.Unix() / schema.TOTPPeriodSeconds }

func totpAt(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) // #nosec G115 -- a time step is never negative
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range schema.TOTPDigits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", schema.TOTPDigits, n%mod)
}

func decodeTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimRight(strings.TrimSpace(s), "="))
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, errors.New("the enrolment's secret is not base32")
	}
	return b, nil
}

// code is a code conchd has not seen: the current step's, or the next
// one's, which the window accepts; when both are spent it waits for the next
// step.
func (a *authenticator) code() (string, error) {
	for {
		now := totpStep(time.Now())
		for _, s := range []int64{now, now + 1} {
			if s > a.last {
				a.last = s
				return totpAt(a.secret, s), nil
			}
		}
		next := time.Unix((now+1)*schema.TOTPPeriodSeconds, 0)
		select {
		case <-time.After(time.Until(next) + 50*time.Millisecond):
		case <-stopCtx.Done():
			return "", errors.New("interrupted")
		}
	}
}

// peek is a code that would be right now, without spending it here: for a
// request that must be refused for another reason, and so takes no step.
func (a *authenticator) peek() (string, error) {
	last := a.last
	code, err := a.code()
	a.last = last
	return code, err
}

// statusErr says why a request that had to answer 200 did not: the error
// that kept it from being answered, or the status it was answered with.
func statusErr(what string, status int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: status %d", what, status)
}
