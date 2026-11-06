package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The authentication gates (design §5, §6, §9.8, §9.9; issue #717 G9a), each
// over the real server and its own clock, from addresses of their own:
//
//   - A member (pia) signs out, and in on a new browser with her password;
//     proves it again and makes a device link; then an operator's reset
//     link resets her password while, at the same moment, someone signs in
//     with the old one and someone redeems the device link. Whichever of
//     them wins, nothing made before the reset acts after it: the session
//     and every socket it holds (timed), whatever the sign-in or the link
//     answered, the link itself and the reset link.
//   - The operator sets a password and a second factor through a setup token
//     the host made (conchd operator setup): a password alone answers only a
//     challenge, bound to the address it was given to; the code completes
//     the browser session; the session needs a recent proof for the
//     sensitive routes, and a code is good once; the operator's script
//     credential cannot make a proof, a device link or a sign-in link for
//     itself. Then conchd operator recover, on the host, while the session
//     holds sockets: the session and the script credential stop at once on
//     REST, and the sockets within the server's re-check interval. The
//     operator comes back through the new setup token.

// openMessaging opens, as m, the message and events socket variants on
// channel: every socket variant a member without voice can hold.
func openMessaging(m *member, channel string) ([]*feed, error) {
	var out []*feed
	for _, v := range socketVariants {
		if strings.Contains(v.path, "/voice/") {
			continue
		}
		what := fmt.Sprintf("%s's %s on %s (ticket=%v)", m.name, v.path, channel, v.ticket)
		f, r, err := dialSocket(m.api, what, v.path, url.Values{"channel": {channel}}, v.ticket)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, fmt.Errorf("%s: status %d %s", what, r.status, m.api.h.redact(string(r.body)))
		}
		out = append(out, f)
	}
	return out, nil
}

// ownLinkOpen reports whether m's open device links, as m lists them,
// include link id. The list holds open links only: one used, closed or
// expired is not in it.
func ownLinkOpen(m *member, id int64) (bool, error) {
	var mine schema.ListOwnSignInLinksResponseV1
	if err := m.api.call(http.MethodGet, "/v1/sign-in-links/mine", nil, &mine); err != nil {
		return false, err
	}
	return slices.ContainsFunc(mine.SignInLinks, func(l schema.SignInLinkV1) bool { return l.ID == id }), nil
}

// timeClosed waits for each socket to close and records, in t, how long
// after from it did: never before from (it closed for some other reason),
// never past bound, and with the close a revoked credential gets.
func timeClosed(t *timing, feeds []*feed, from time.Time, bound time.Duration, since string) error {
	for _, f := range feeds {
		if err := waitFor(f.what+" to close", bound+10*time.Second, func() (bool, string) {
			ended, _ := f.ended()
			return ended, "still open"
		}); err != nil {
			return err
		}
		_, at := f.ended()
		if at.Before(from) {
			return fmt.Errorf("%s: %s closed %s before %s", t.what, f.what, from.Sub(at).Round(time.Millisecond), since)
		}
		t.sockets = append(t.sockets, at.Sub(from))
		if at.Sub(from) > bound {
			return fmt.Errorf("%s: %s closed %s after %s, bound %s", t.what, f.what, at.Sub(from).Round(time.Millisecond), since, bound)
		}
	}
	return closedAllRevoked(feeds)
}

// closedAllRevoked is closedRevoked for every socket, not only the
// occupancy ones: each closed with 1008 and "credential no longer valid",
// the close of a credential that went.
func closedAllRevoked(feeds []*feed) error {
	return closedWith(feeds, func(*feed) []string { return []string{credentialGone} })
}

// The two reasons a socket closes with 1008 for a guest whose visit ended.
const (
	credentialGone = "credential no longer valid" //nolint:gosec // close-frame reason text, not a credential
	membershipGone = "no longer a member of this channel"
)

// closedAllExpired is closedAllRevoked for a guest whose visit expired.
// Expiry both invalidates the credential and removes the guest from its
// channels, so its events socket is closed by whichever path runs first:
// both reasons are valid there (both 1008). Every other socket keeps the
// single reason.
func closedAllExpired(feeds []*feed) error {
	return closedWith(feeds, func(f *feed) []string {
		if strings.Contains(f.what, "/v1/events/ws") {
			return []string{credentialGone, membershipGone}
		}
		return []string{credentialGone}
	})
}

// closedWith requires that each socket closed with 1008 and one of the
// reasons that reasons gives for it.
func closedWith(feeds []*feed, reasons func(*feed) []string) error {
	for _, f := range feeds {
		f.mu.Lock()
		err := f.err
		f.mu.Unlock()
		want := reasons(f)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || !slices.Contains(want, ce.Reason) {
			return fmt.Errorf("%s ended with %w, want 1008 %q", f.what, err, want)
		}
	}
	return nil
}

// signIn signs name in with password from a, and returns the member's
// session.
func (w *world) signIn(a api, name string, id int64, password string) (*member, error) {
	var login schema.LoginResponseV1
	if err := a.call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: password}, &login); err != nil {
		return nil, err
	}
	w.h.secret(name+"'s sign-in", login.Token)
	if login.Next != schema.LoginNextSignedIn || login.Principal == nil || login.Principal.ID != id || login.Credential == nil {
		return nil, fmt.Errorf("%s signing in answered %s", name, login.Next)
	}
	m := &member{name: name, id: id, token: login.Token, credID: login.Credential.ID, password: true, addr: a.addr}
	m.api = w.as(m)
	return m, nil
}

// refusedAs requires that a's request is answered want. These refusals are
// about the caller's own account, and a message may name a route
// ("POST /v1/reauth"), so its numbers are not held against it.
func refusedAs(a api, method, path string, body any, want answer, what string) error {
	status, data, err := a.do(method, path, body)
	if err != nil {
		return err
	}
	if _, err := checkAnswer(result{status: status, body: data}, want); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// signInLinkTargetPrefix starts the refusal of a sign-in link for a
// principal that may not have one (the server's signInLinkTargetMessage):
// the specific reason, not any 400.
const signInLinkTargetPrefix = "a sign-in link is for a member"

// refusedForTarget requires that an operator's sign-in link for principal
// id is refused with that reason.
func refusedForTarget(a api, id int64, purpose schema.SignInLinkPurpose, what string) error {
	status, data, err := a.do(http.MethodPost, fmt.Sprintf("/v1/principals/%d/sign-in-links", id), schema.CreateSignInLinkRequestV1{Purpose: purpose})
	if err != nil {
		return err
	}
	e, err := checkAnswer(result{status: status, body: data}, answer{http.StatusBadRequest, "invalid_request"})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !strings.HasPrefix(e.Message, signInLinkTargetPrefix) {
		return fmt.Errorf("%s: refused with %q, not as a link for someone who may not have one", what, e.Message)
	}
	return nil
}

// dead requires that each token no longer authenticates.
func (w *world) dead(what string, tokens ...string) error {
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		a := api{h: w.h, base: w.d.baseURL, token: tok, who: what}
		if status, _, err := a.do(http.MethodGet, "/v1/whoami", nil); err != nil {
			return err
		} else if status != http.StatusUnauthorized {
			return fmt.Errorf("%s still authenticates (status %d)", what, status)
		}
	}
	return nil
}

// raceRefill is how long the server-wide redemption limit (twenty a minute)
// takes to give back the two places the reset race needs.
const raceRefill = 2 * time.Minute / 20

// resetPassword is the password pia chooses at her reset.
const resetPassword = "a passphrase chosen at the reset"

// memberResetRace is the member's half. It returns the reset's timing.
func (w *world) memberResetRace() (timing, error) {
	p := w.p
	pia := p.pia
	// Sign out, then in on a new browser with the password.
	if err := pia.api.call(http.MethodDelete, "/v1/credentials/current", nil, nil); err != nil {
		return timing{}, err
	}
	if err := w.dead("pia's signed-out credential", pia.token); err != nil {
		return timing{}, err
	}
	s1, err := w.signIn(w.newcomer("pia's new browser"), pia.name, pia.id, joinPassword)
	if err != nil {
		return timing{}, err
	}
	// A device link needs a recent proof of the password on this session.
	if err := refusedAs(s1.api, http.MethodPost, "/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "pia's phone"},
		answer{http.StatusForbidden, schema.ErrorCodeReauthRequired}, "a device link before the proof"); err != nil {
		return timing{}, err
	}
	if err := s1.api.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: joinPassword}, nil); err != nil {
		return timing{}, err
	}
	var device schema.CreateSignInLinkResponseV1
	if err := s1.api.call(http.MethodPost, "/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "pia's phone"}, &device); err != nil {
		return timing{}, err
	}
	w.h.secret("pia's device link", device.Code)
	// A second device link that nobody races and nobody uses: whether it
	// works after the reset is the reset's doing alone.
	var untouched schema.CreateSignInLinkResponseV1
	if err := s1.api.call(http.MethodPost, "/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "pia's laptop"}, &untouched); err != nil {
		return timing{}, err
	}
	w.h.secret("pia's untouched device link", untouched.Code)
	if open, err := ownLinkOpen(s1, untouched.SignInLink.ID); err != nil {
		return timing{}, err
	} else if !open {
		return timing{}, fmt.Errorf("the device link pia just made is not among her open links")
	}
	sockets, err := openMessaging(s1, p.channel)
	if err != nil {
		return timing{}, err
	}
	var reset schema.CreateSignInLinkResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/sign-in-links", pia.id), schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset}, &reset); err != nil {
		return timing{}, err
	}
	w.h.secret("pia's reset link", reset.Code)

	// The reset, a sign-in with the old password and the device link's
	// redemption, at once. A reset that succeeds spends the server-wide
	// redemption allowance, which this run's newcomers have been spending,
	// so it may be told to wait (entry): the sockets are timed from the
	// request that was answered.
	t := timing{what: "a member's password reset, racing a sign-in with the old password and a device link"}
	// The redemption and the reset each spend a place under the server-wide
	// limit when they succeed, which the phase's joins have just used up. It
	// gives back one place every three seconds (twenty a minute,
	// invite_limits.go), so after raceRefill both have one and race at once
	// rather than one after the other; a wait there is said all the same.
	if err := sleepUntil(time.Now().Add(raceRefill)); err != nil {
		return timing{}, err
	}
	var racers sync.WaitGroup
	var loginTok, linkTok string
	var loginStatus, linkStatus int
	var loginBody, linkBody []byte
	var linkWaited time.Duration
	var loginErr, linkErr error
	start := make(chan struct{})
	oldPassword, deviceHolder, resetPage := w.newcomer("someone with pia's old password"), w.newcomer("someone with pia's device link"), w.newcomer("pia at the reset page")
	racers.Add(2)
	go func() {
		defer racers.Done()
		<-start
		loginStatus, loginBody, loginErr = oldPassword.do(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: pia.name, Password: joinPassword})
		loginTok = tokenOf(loginBody)
	}()
	go func() {
		defer racers.Done()
		<-start
		linkStatus, linkBody, _, linkWaited, linkErr = w.entryAt(deviceHolder, http.MethodPost, "/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: device.Code})
		linkTok = tokenOf(linkBody)
	}()
	close(start)
	status, body, sent, resetWaited, err := w.entryAt(resetPage, http.MethodPost, "/v1/password/reset", schema.ResetPasswordRequestV1{Code: reset.Code, Password: resetPassword})
	racers.Wait()
	w.h.secret("a racing sign-in", loginTok)
	w.h.secret("a racing device sign-in", linkTok)
	switch {
	case err != nil:
		return t, err
	case loginErr != nil:
		return t, loginErr
	case linkErr != nil:
		return t, linkErr
	case status != http.StatusOK:
		return t, fmt.Errorf("the reset: status %d %s", status, w.h.redact(string(body)))
	case tokenOf(body) != "":
		return t, fmt.Errorf("a reset answered a credential: it signs nobody in")
	}
	// Each racer won (a credential) or lost to the reset (the one refusal
	// its route has); nothing else is an answer.
	if loginStatus != http.StatusOK {
		if _, err := checkAnswer(result{status: loginStatus, body: loginBody}, answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed}); err != nil {
			return t, fmt.Errorf("the sign-in racing the reset: %w", err)
		}
	}
	if linkStatus != http.StatusCreated {
		if _, err := checkAnswer(result{status: linkStatus, body: linkBody}, answer{http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid}); err != nil {
			return t, fmt.Errorf("the device link racing the reset: %w", err)
		}
	}
	if err := timeClosed(&t, sockets, sent, socketBound, "the reset was sent"); err != nil {
		return t, err
	}
	// Whoever won, nothing made before the reset acts after it. Which won
	// is the scheduler's; say it, and say when a racer or the reset waited
	// for the server-wide limit, since then they did not race.
	t.what += fmt.Sprintf(" (the sign-in answered %d, the device link %d)", loginStatus, linkStatus)
	if linkWaited > 0 || resetWaited > 0 {
		t.what += fmt.Sprintf(" (not a race this run: the device link waited %s and the reset %s for the server-wide limit)", linkWaited, resetWaited)
	}
	if err := w.dead("a credential made before pia's reset", s1.token, loginTok, linkTok); err != nil {
		return t, err
	}
	for _, c := range []struct {
		what string
		path string
		body any
	}{
		{"the device link that raced the reset", "/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: device.Code}},
		{"the device link made before the reset and never used", "/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: untouched.Code}},
		{"the reset link, again", "/v1/password/reset", schema.ResetPasswordRequestV1{Code: reset.Code, Password: anotherPassword}},
	} {
		status, body, err := w.entry(w.newcomer("someone with "+c.what), http.MethodPost, c.path, c.body)
		if err != nil {
			return t, err
		}
		if err := checkRefusal(result{status: status, body: body}, answer{http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid}, asked{}); err != nil {
			return t, fmt.Errorf("%s: %w", c.what, err)
		}
	}
	if err := refusedAs(w.newcomer("pia's old password"), http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: pia.name, Password: joinPassword},
		answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed}, "the old password after the reset"); err != nil {
		return t, err
	}
	s2, err := w.signIn(w.newcomer("pia after the reset"), pia.name, pia.id, resetPassword)
	if err != nil {
		return t, err
	}
	// The untouched link is not among her open links any more: the reset
	// closed it (it would otherwise be open for ten minutes yet).
	if open, err := ownLinkOpen(s2, untouched.SignInLink.ID); err != nil {
		return t, err
	} else if open {
		return t, fmt.Errorf("the device link made before the reset and never used is still open after it")
	}
	// A link is for one purpose: a reset link does not sign in, and a
	// device link does not reset.
	var reset2 schema.CreateSignInLinkResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/sign-in-links", pia.id), schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset}, &reset2); err != nil {
		return t, err
	}
	w.h.secret("pia's second reset link", reset2.Code)
	if err := s2.api.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: resetPassword}, nil); err != nil {
		return t, err
	}
	var device2 schema.CreateSignInLinkResponseV1
	if err := s2.api.call(http.MethodPost, "/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "pia's tablet"}, &device2); err != nil {
		return t, err
	}
	w.h.secret("pia's second device link", device2.Code)
	for _, c := range []struct {
		what string
		path string
		body any
	}{
		{"a reset link presented to sign in", "/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: reset2.Code}},
		{"a device link presented as a reset", "/v1/password/reset", schema.ResetPasswordRequestV1{Code: device2.Code, Password: anotherPassword}},
	} {
		status, body, err := w.entry(w.newcomer("someone with "+c.what), http.MethodPost, c.path, c.body)
		if err != nil {
			return t, err
		}
		if err := checkRefusal(result{status: status, body: body}, answer{http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid}, asked{}); err != nil {
			return t, fmt.Errorf("%s: %w", c.what, err)
		}
	}
	if err := s2.api.call(http.MethodDelete, fmt.Sprintf("/v1/sign-in-links/mine/%d", device2.SignInLink.ID), nil, nil); err != nil {
		return t, err
	}
	if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/sign-in-links/%d", reset2.SignInLink.ID), nil, nil); err != nil {
		return t, err
	}
	// pia goes on with the session she signed in with after the reset.
	p.pia = s2
	return t, nil
}

// operatorPassword is the operator's password, set through the setup token.
const operatorPassword = "the operator's long passphrase"

// recheckBound is how soon a socket notices a credential revoked behind the
// server's back (by a host command, in the database): the re-check
// interval plus the re-check's own timeout, read from internal/server/ws.go.
func recheckBound(repo string) (time.Duration, error) {
	path := filepath.Join(repo, "internal", "server", "ws.go")
	src, err := readSource(path)
	if err != nil {
		return 0, err
	}
	consts, err := durationConsts(src)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	every, timeout := consts["defaultCredentialRecheckInterval"], consts["wsRecheckTimeout"]
	if every <= 0 || timeout <= 0 {
		return 0, fmt.Errorf("%s: cannot read defaultCredentialRecheckInterval and wsRecheckTimeout as durations", path)
	}
	return every + timeout, nil
}

// operatorSetup redeems a setup token from a, sets the password and enrols
// a second factor; auth is the operator's authenticator, which keeps the
// last step it gave a code for across enrolments.
func (w *world) operatorSetup(a api, token string, auth *authenticator) error {
	var setup schema.RedeemSetupTokenResponseV1
	if err := a.call(http.MethodPost, "/v1/setup/redeem", schema.RedeemSetupTokenRequestV1{Token: token}, &setup); err != nil {
		return err
	}
	w.h.secret("a setup credential", setup.Token)
	if setup.Credential.Scope != schema.CredentialScopeSetup {
		return fmt.Errorf("a setup token answered a credential of scope %q", setup.Credential.Scope)
	}
	s := a
	s.token, s.who = setup.Token, "the operator's setup credential"
	// It reaches whoami and setup, and nothing else.
	if err := refusedAs(s, http.MethodGet, "/v1/channels", nil, answer{http.StatusForbidden, schema.ErrorCodeSetupIncomplete}, "the setup credential lists channels"); err != nil {
		return err
	}
	if err := refusedAs(s, http.MethodGet, "/v1/guests", nil, answer{http.StatusForbidden, schema.ErrorCodeSetupIncomplete}, "the setup credential lists visits"); err != nil {
		return err
	}
	if err := s.call(http.MethodPost, "/v1/password/mine", schema.SetPasswordRequestV1{Password: operatorPassword}, nil); err != nil {
		return err
	}
	var enrol schema.TOTPEnrollmentV1
	if err := s.call(http.MethodPost, "/v1/totp/enroll", nil, &enrol); err != nil {
		return err
	}
	w.h.secret("the operator's TOTP secret", enrol.Secret)
	if enrol.Digits != schema.TOTPDigits || enrol.PeriodSeconds != schema.TOTPPeriodSeconds || enrol.Algorithm != schema.TOTPAlgorithm {
		return fmt.Errorf("the enrolment's parameters are %s, %d digits, %d s", enrol.Algorithm, enrol.Digits, enrol.PeriodSeconds)
	}
	secret, err := decodeTOTPSecret(enrol.Secret)
	if err != nil {
		return err
	}
	auth.secret = secret
	code, err := auth.code()
	if err != nil {
		return err
	}
	return s.call(http.MethodPost, "/v1/totp/confirm", schema.ConfirmTOTPRequestV1{Code: code}, nil)
}

// operatorSignIn signs the operator in from a: the password earns a
// challenge and nothing else; the code, from a, the session.
func (w *world) operatorSignIn(a api, name string, id int64, auth *authenticator) (*member, error) {
	var first schema.LoginResponseV1
	if err := a.call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: operatorPassword}, &first); err != nil {
		return nil, err
	}
	w.h.secret("an operator's challenge", first.Challenge)
	if first.Next != schema.LoginNextTOTP || first.Token != "" || first.Credential != nil || first.Challenge == "" {
		return nil, fmt.Errorf("the operator's password answered %s with a credential %v: a password alone is no access", first.Next, first.Credential != nil)
	}
	code, err := auth.code()
	if err != nil {
		return nil, err
	}
	var done schema.LoginResponseV1
	if err := a.call(http.MethodPost, "/v1/login/totp", schema.TOTPLoginRequestV1{Challenge: first.Challenge, Code: code}, &done); err != nil {
		return nil, err
	}
	w.h.secret("the operator's session", done.Token)
	if done.Next != schema.LoginNextSignedIn || done.Principal == nil || done.Principal.ID != id || done.Credential == nil {
		return nil, fmt.Errorf("the operator's code answered %s", done.Next)
	}
	m := &member{name: name, id: id, token: done.Token, credID: done.Credential.ID, addr: a.addr, password: true}
	m.api = w.as(m)
	return m, nil
}

// operatorAuth is the operator's half. It returns the recovery's timing,
// from the host command's start, and leaves w.op the operator's new
// browser session.
func (w *world) operatorAuth() (timing, error) {
	var who schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return timing{}, err
	}
	opID, opName := who.ID, who.Name
	token, err := w.d.hostCommand(w.h, "operator", "setup", "--data", w.d.dataDir, itoa(opID))
	if err != nil {
		return timing{}, err
	}
	w.h.secret("a setup token", token)
	browser := w.newcomer("the operator's browser")
	auth := &authenticator{}
	if err := w.operatorSetup(browser, token, auth); err != nil {
		return timing{}, err
	}
	// The script credential the host made is not the setup flow.
	if err := refusedAs(w.op, http.MethodPost, "/v1/totp/enroll", nil, answer{http.StatusForbidden, schema.ErrorCodeSetupOnHost}, "the script credential enrols a second factor"); err != nil {
		return timing{}, err
	}

	// The challenge is the address's it was given to (review F14).
	var first schema.LoginResponseV1
	if err := browser.call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: opName, Password: operatorPassword}, &first); err != nil {
		return timing{}, err
	}
	w.h.secret("an operator's challenge", first.Challenge)
	if first.Next != schema.LoginNextTOTP || first.Token != "" {
		return timing{}, fmt.Errorf("the operator's password answered %s, a token %v", first.Next, first.Token != "")
	}
	code, err := auth.code()
	if err != nil {
		return timing{}, err
	}
	if err := refusedAs(w.newcomer("someone else with the operator's challenge"), http.MethodPost, "/v1/login/totp", schema.TOTPLoginRequestV1{Challenge: first.Challenge, Code: code},
		answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed}, "the challenge from another address"); err != nil {
		return timing{}, err
	}
	var signedIn schema.LoginResponseV1
	if err := browser.call(http.MethodPost, "/v1/login/totp", schema.TOTPLoginRequestV1{Challenge: first.Challenge, Code: code}, &signedIn); err != nil {
		return timing{}, err
	}
	w.h.secret("the operator's session", signedIn.Token)
	if signedIn.Next != schema.LoginNextSignedIn || signedIn.Credential == nil {
		return timing{}, fmt.Errorf("the operator's code answered %s", signedIn.Next)
	}
	sess := &member{name: opName, id: opID, token: signedIn.Token, credID: signedIn.Credential.ID, addr: browser.addr, password: true}
	sess.api = w.as(sess)
	var sw schema.WhoAmIResponseV1
	if err := sess.api.call(http.MethodGet, "/v1/whoami", nil, &sw); err != nil {
		return timing{}, err
	}
	now := time.Now()
	if sw.Auth == nil || sw.Auth.IdleExpiresAt == nil || sw.Auth.ExpiresAt == nil || !sw.Auth.SetupComplete ||
		sw.Auth.IdleExpiresAt.Time().After(now.Add(time.Hour+time.Minute)) || sw.Auth.ExpiresAt.Time().After(now.Add(12*time.Hour+time.Minute)) {
		return timing{}, fmt.Errorf("the operator's session is bounded %+v, want an hour idle and twelve in all", sw.Auth)
	}

	// The sign-in with both factors is the session's recent proof for its
	// first ten minutes (issue #1033): a sensitive route takes it at once.
	if sw.Auth.RecentAuthUntil == nil || sw.Auth.RecentAuthUntil.Time().Before(now.Add(9*time.Minute)) ||
		sw.Auth.RecentAuthUntil.Time().After(now.Add(11*time.Minute)) {
		return timing{}, fmt.Errorf("the operator's session holds a proof to %v, want ten minutes from the sign-in", sw.Auth.RecentAuthUntil)
	}
	change := schema.PutJoinSettingsRequestV1{JoinVoice: ptr(true)}
	if err := sess.api.call(http.MethodPut, "/v1/join/settings", change, nil); err != nil {
		return timing{}, fmt.Errorf("the session changes the joining policy with the sign-in's proof: %w", err)
	}
	// The proof is the signed-in credential's alone: the session rotated
	// from it has none, needs one on a sensitive route, and a code is good
	// once.
	var rotated schema.RotateCredentialResponseV1
	if err := sess.api.call(http.MethodPost, fmt.Sprintf("/v1/credentials/%d/rotate", sess.credID), nil, &rotated); err != nil {
		return timing{}, err
	}
	w.h.secret("the operator's rotated session", rotated.Token)
	sess.token, sess.credID = rotated.Token, rotated.Credential.ID
	sess.api = w.as(sess)
	if err := refusedAs(sess.api, http.MethodPut, "/v1/join/settings", change, answer{http.StatusForbidden, schema.ErrorCodeReauthRequired}, "the rotated session changes the joining policy without a proof"); err != nil {
		return timing{}, err
	}
	if err := refusedAs(sess.api, http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: operatorPassword, Code: auth.spent()},
		answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed}, "a proof with the code the sign-in spent"); err != nil {
		return timing{}, err
	}
	if code, err = auth.code(); err != nil {
		return timing{}, err
	}
	if err := sess.api.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: operatorPassword, Code: code}, nil); err != nil {
		return timing{}, err
	}
	if err := sess.api.call(http.MethodPut, "/v1/join/settings", change, nil); err != nil {
		return timing{}, err
	}

	// The script credential makes no proof (it is no session), no device
	// link and no sign-in link to the operator's account.
	for _, c := range []struct {
		what, method, path string
		body               any
		want               answer
	}{
		{"makes a proof", http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: operatorPassword, Code: "000000"}, operatorOnly},
		{"makes a device link", http.MethodPost, "/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "a laptop"}, operatorOnly},
	} {
		if err := refusedAs(w.op, c.method, c.path, c.body, c.want, "the operator's script credential "+c.what); err != nil {
			return timing{}, err
		}
	}
	if err := refusedForTarget(w.op, opID, "", "the operator's script credential makes a sign-in link to the operator's account"); err != nil {
		return timing{}, err
	}
	// What it mints for the operator is a script credential too: it does
	// not renew, has no idle bound, and makes no proof.
	var minted schema.CreateCredentialResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", opID), schema.CreateCredentialRequestV1{Label: "minted by a script"}, &minted); err != nil {
		return timing{}, err
	}
	w.h.secret("a credential the operator's script minted", minted.Token)
	if minted.Credential.Renews || minted.Credential.IdleExpiresAt != nil {
		return timing{}, fmt.Errorf("the script credential minted a session: renews %v, idle bound %v", minted.Credential.Renews, minted.Credential.IdleExpiresAt)
	}
	mintedAPI := api{h: w.h, base: w.d.baseURL, token: minted.Token, who: "a credential the operator's script minted"}
	if err := refusedAs(mintedAPI, http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: operatorPassword, Code: "000000"}, operatorOnly,
		"the credential the script minted makes a proof"); err != nil {
		return timing{}, err
	}
	var mw schema.WhoAmIResponseV1
	if err := mintedAPI.call(http.MethodGet, "/v1/whoami", nil, &mw); err != nil {
		return timing{}, err
	}
	if mw.Auth == nil || mw.Auth.RecentAuthUntil != nil || mw.Auth.IdleExpiresAt != nil {
		return timing{}, fmt.Errorf("the credential the script minted is %+v, want no proof and no idle bound", mw.Auth)
	}

	// Recovery on the host while the session holds sockets.
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(w.p.channel), opID), nil, nil); err != nil {
		return timing{}, err
	}
	sockets, err := openMessaging(sess, w.p.channel)
	if err != nil {
		return timing{}, err
	}
	bound, err := recheckBound(w.h.repo)
	if err != nil {
		return timing{}, err
	}
	script := w.op
	t := timing{what: "conchd operator recover, on the host, against the operator's open sockets", bound: bound}
	start := time.Now()
	token, err = w.d.hostCommand(w.h, "operator", "recover", "--data", w.d.dataDir, itoa(opID))
	if err != nil {
		return t, err
	}
	w.h.secret("a setup token", token)
	if err := w.dead("a credential of the operator's from before the recovery", sess.token, script.token, minted.Token); err != nil {
		return t, err
	}
	// Until setup is done again, the password signs nobody in (design §9.8:
	// an operator with a password but no confirmed second factor is
	// auth_failed): recovery removed the second factor. Where password-only
	// is allowed (a temporary server, or CONCHD_OPERATOR_TOTP=optional) the
	// recovered operator is password-only, and the password alone signs them
	// in (ADR-011 item 2.1, ADR-012 item 3); everything else here is the same.
	if passwordOnlyAllowed() {
		var in schema.LoginResponseV1
		if err := w.newcomer("the operator's password, after the recovery, where password-only is allowed").call(http.MethodPost, "/v1/login",
			schema.LoginRequestV1{Name: opName, Password: operatorPassword}, &in); err != nil {
			return t, err
		}
		w.h.secret("a browser session", in.Token)
		if in.Next != schema.LoginNextSignedIn || in.Token == "" {
			return t, fmt.Errorf("the recovered operator's password where password-only is allowed answered %s, want signed in", in.Next)
		}
	} else if err := refusedAs(w.newcomer("someone with the operator's password, after the recovery"), http.MethodPost, "/v1/login",
		schema.LoginRequestV1{Name: opName, Password: operatorPassword}, answer{http.StatusUnauthorized, schema.ErrorCodeAuthFailed},
		"the operator's password between the recovery and the new setup"); err != nil {
		return t, err
	}
	if err := timeClosed(&t, sockets, start, bound, "the recovery began (the re-check interval and its timeout)"); err != nil {
		return t, err
	}
	// Back in through the new setup token: a password and a second factor
	// afresh, then the browser session.
	again := w.newcomer("the operator's browser, after the recovery")
	if err := w.operatorSetup(again, token, auth); err != nil {
		return t, err
	}
	op, err := w.operatorSignIn(again, opName, opID, auth)
	if err != nil {
		return t, err
	}
	// With a proof the session makes the run a script credential again, as
	// an operator would for their automation: the rest of the run acts as
	// the operator with it.
	if code, err = auth.code(); err != nil {
		return t, err
	}
	if err := op.api.call(http.MethodPost, "/v1/reauth", schema.ReauthRequestV1{Password: operatorPassword, Code: code}, nil); err != nil {
		return t, err
	}
	var cred schema.CreateCredentialResponseV1
	if err := op.api.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", opID), schema.CreateCredentialRequestV1{Label: "isolation-check, after the recovery"}, &cred); err != nil {
		return t, err
	}
	w.h.secret("the operator's credential", cred.Token)
	w.op = api{h: w.h, base: w.d.baseURL, token: cred.Token, who: "the operator"}
	w.d.op = w.op
	return t, nil
}
