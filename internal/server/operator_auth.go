package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Operator sign-in, the second factor and offline setup (issue #709,
// docs/design/guests-and-accounts.md §5.3, §7.2):
//
//   - POST /v1/login/totp completes the challenge an operator's password
//     earned at POST /v1/login with a TOTP code, from the address it was
//     issued to, and answers the operator's browser session: twelve hours,
//     an hour idle. A wrong code counts in the name's failure window with
//     wrong passwords (review F2), and against the challenge, which is spent
//     at its fifth.
//   - POST /v1/setup/redeem spends a setup token that conchd made on the host
//     (bootstrap or recovery) for a fifteen-minute credential of scope setup,
//     which the authentication middleware lets reach only whoami, the
//     password enrolment and the two TOTP routes (review F6).
//   - POST /v1/totp/enroll and POST /v1/totp/confirm, on that credential and
//     no other (review F7), make a secret, show it once, and confirm it with
//     a code.
//   - The recent-auth gate: a browser session of an operator needs a proof
//     of password and code no older than ten minutes for every route of the
//     re-authentication class (recentAuthRoutes, routes.go; review F8). A
//     script credential is outside it (the S15 boundary).
//
// Every route here takes a code or a token, so each checks what
// passwordRoute checks first: authentication on, HTTPS or loopback. Nothing
// here logs or audits a secret, a code, a challenge or a token; a secret
// appears once, in the enrolment's response, sent no-store.

// Reasons of a login_failed row beside the sign-in's (login.go).
const (
	loginReasonWrongCode     = "wrong_code"
	loginReasonMissingCode   = "missing_code"
	loginReasonWrongAddress  = "wrong_address"
	loginReasonChallengeGone = "challenge_invalid"
	// passwordFailureViaConfirm names, in a login_failed row, a wrong code
	// presented to confirm an enrolment.
	passwordFailureViaConfirm = "totp_confirm"
)

// auditSetupTokenRefused records, thinned as the other code refusals are,
// that a well-formed setup token redeemed nothing.
const auditSetupTokenRefused = "setup_token_redeem_refused"

// totpUnavailable reports whether err is a TOTP check that could not run:
// no key, or a key that does not open the stored secret. It fails closed.
func totpUnavailable(err error) bool {
	return errors.Is(err, errTOTPKeyMissing) || errors.Is(err, errTOTPUnsealable)
}

// logTOTPUnavailable says, once per occurrence, why an operator's code could
// not be checked. It names the principal and never the secret or the code.
func logTOTPUnavailable(ctx context.Context, principalID int64, err error) {
	slog.ErrorContext(ctx, "totp: an operator's code cannot be checked: the server's TOTP key (CONCHD_TOTP_KEY) is missing or is not the key the second factor was enrolled under; restore the key, or recover the operator with conchd operator recover",
		"principal", principalID, "error", err)
}

// totpCheck is the store's check of code for principalID, at the server's
// clock: it opens the sealed secret with the host key, matches the code
// within the window after the last accepted step, and forgets the secret.
func (s *Server) totpCheck(principalID int64, code string) store.TOTPCheck {
	return func(sealed []byte, lastStep int64) (int64, error) {
		secret, err := s.cfg.TOTPKey.openTOTP(principalID, sealed)
		if err != nil {
			return 0, err
		}
		defer clear(secret)
		step, ok := matchTOTP(secret, code, s.now(), lastStep)
		if !ok {
			return 0, store.ErrSecondFactorWrong
		}
		return step, nil
	}
}

// handleLoginTOTP serves POST /v1/login/totp. The order is the sign-in's: the
// transport check, the address budget before the body is read, a malformed
// body a 400 that says nothing about any account; then the challenge, which
// names the account whose window the attempt counts in; that window, 429 at
// once when spent; and the store, which checks the code in the transaction
// that records it and issues the session.
func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.logins.takeAddress(addr, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, addr)
		return
	}
	var req schema.TOTPLoginRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the challenge or code is malformed")
		return
	}
	hash := store.HashChallenge(req.Challenge)
	pending, err := s.store.PendingLoginOf(ctx, hash, s.now())
	switch {
	case errors.Is(err, store.ErrChallengeInvalid):
		// A challenge that names nobody has no window to count in; the
		// address's budget keeps the attempt.
		s.auditLoginFailed(r, fmt.Sprintf("name_key=%q reason=%s", loginReasonUnknown, loginReasonChallengeGone),
			loginReasonChallengeGone+"|"+loginReasonUnknown)
		writeAuthFailed(w)
		return
	case err != nil:
		s.logins.refundAddress(addr, s.now())
		slog.ErrorContext(ctx, "login: read the challenge failed", "error", err)
		writeInternalError(w)
		return
	}
	key := names.KeysOf(pending.Name).Folded
	if limit, ok, retry := s.logins.takeName(key, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, addr)
		return
	}
	sameAddress := s.pendingAddressMatches(pending.AddressKey, addr)
	issued, err := s.store.CompleteTOTPLogin(ctx, store.TOTPLogin{
		ChallengeHash: hash, AddressMatches: sameAddress, Check: s.totpCheck(pending.PrincipalID, req.Code),
	}, s.now())
	switch {
	case errors.Is(err, store.ErrSecondFactorWrong):
		reason := loginReasonWrongCode
		if !sameAddress {
			reason = loginReasonWrongAddress
		}
		s.loginFailed(w, r, key, true, reason)
	case errors.Is(err, store.ErrChallengeInvalid):
		s.loginFailed(w, r, key, true, loginReasonChallengeGone)
	case errors.Is(err, store.ErrLoginStale):
		s.loginFailed(w, r, key, true, loginReasonStale)
	case totpUnavailable(err):
		// No code was looked at: nothing is counted, and the answer is the
		// one for a code that does not work.
		s.logins.nameCancelled(key, s.now())
		s.logins.refundAddress(addr, s.now())
		logTOTPUnavailable(ctx, pending.PrincipalID, err)
		writeAuthFailed(w)
	case err != nil:
		s.logins.nameCancelled(key, s.now())
		s.logins.refundAddress(addr, s.now())
		slog.ErrorContext(ctx, "login: complete the sign-in failed", "principal", pending.PrincipalID, "error", err)
		writeInternalError(w)
	default:
		s.logins.nameSucceeded(key, s.now())
		s.logins.refundAddress(addr, s.now())
		slog.InfoContext(ctx, "login: operator signed in", "principal", issued.Principal.ID, "credential", issued.Credential.ID)
		principal := principalFromStore(issued.Principal)
		writeSecretJSON(w, http.StatusOK, schema.LoginResponseV1{
			Next: schema.LoginNextSignedIn, Principal: &principal, Credential: &issued.Credential, Token: issued.Token,
		})
	}
}

// secondFactorMissing answers an operator's change or re-authentication that
// came without a TOTP code: a failure like any other, counted in the name's
// window (review F2), and answered before any password work.
func (s *Server) secondFactorMissing(w http.ResponseWriter, r *http.Request, caller store.Principal, via string) {
	key := names.KeysOf(caller.Name).Folded
	if limit, ok, retry := s.logins.takeName(key, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	s.secondFactorFailed(w, r, key, loginReasonMissingCode, via)
}

// secondFactorFailed settles a reserved attempt as a failure in the name's
// window, audits login_failed with via, and answers auth_failed.
func (s *Server) secondFactorFailed(w http.ResponseWriter, r *http.Request, key, reason, via string) {
	s.logins.nameFailed(key, s.now())
	s.auditLoginFailed(r, fmt.Sprintf("name_key=%q reason=%s via=%s", key, reason, via), reason+"|"+via+"|"+key)
	writeSecondFactorRequired(w)
}

// writeSecondFactorRequired is the answer to an operator's change or
// re-authentication whose code is missing, wrong, replayed or cannot be
// checked: the one auth_failed.
func writeSecondFactorRequired(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, schema.ErrorCodeAuthFailed,
		"the password or code is not right; an operator proves a password with a current TOTP code as well")
}

// setupCaller is the start of the TOTP routes: what passwordCaller checks,
// then a setup-scoped credential of an operator. On anything else the answer
// is 403 setup_on_host: an operator's second factor is enrolled only through
// a setup token made on the host (review F7). It returns what the store knew
// of the caller.
func (s *Server) setupCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, store.PasswordSubject, bool) {
	caller, credID, ok := s.passwordCaller(w, r)
	if !ok {
		return store.Principal{}, 0, store.PasswordSubject{}, false
	}
	subj, ok := s.passwordSubject(w, r, caller, credID)
	if !ok {
		return store.Principal{}, 0, store.PasswordSubject{}, false
	}
	if subj.Scope != schema.CredentialScopeSetup || !isOperator(caller) {
		writeSetupOnHost(w)
		return store.Principal{}, 0, store.PasswordSubject{}, false
	}
	return caller, credID, subj, true
}

func writeSetupOnHost(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeSetupOnHost,
		"an operator's second factor is set up through a setup token made on the host (conchd operator), not on this credential")
}

func writeTOTPAlreadyEnrolled(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodeTOTPAlreadyEnrolled,
		"this operator already has a second factor; only recovery on the host replaces it")
}

// writeTOTPKeyMissing answers an enrolment the server cannot seal or check:
// it was started without a TOTP key, or with one that does not open what is
// stored. It fails closed and says what to fix.
func writeTOTPKeyMissing(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "internal_error",
		"this server cannot keep a second factor: its TOTP key (CONCHD_TOTP_KEY) is not set or is not the right one")
}

// handleEnrollTOTP serves POST /v1/totp/enroll, on a setup-scoped credential:
// a fresh secret, sealed under the host key and kept pending, and shown in
// this response and never again. A second enrolment replaces a pending one.
func (s *Server) handleEnrollTOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, subj, ok := s.setupCaller(w, r)
	if !ok {
		return
	}
	if subj.TOTPEnrolled {
		writeTOTPAlreadyEnrolled(w)
		return
	}
	if s.cfg.TOTPKey == nil {
		logTOTPUnavailable(ctx, caller.ID, errTOTPKeyMissing)
		writeTOTPKeyMissing(w)
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		slog.ErrorContext(ctx, "totp: make a secret failed", "error", err)
		writeInternalError(w)
		return
	}
	defer clear(secret)
	sealed, err := s.cfg.TOTPKey.sealTOTP(caller.ID, secret)
	if err != nil {
		slog.ErrorContext(ctx, "totp: seal the secret failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	switch err := s.store.BeginTOTPEnrollment(ctx, caller.ID, credID, sealed, s.now()); {
	case errors.Is(err, store.ErrTOTPAlreadyEnrolled):
		writeTOTPAlreadyEnrolled(w)
	case errors.Is(err, store.ErrSetupOnHost):
		writeSetupOnHost(w)
	case errors.Is(err, store.ErrLoginStale), errors.Is(err, store.ErrPrincipalDisabled), errors.Is(err, store.ErrPrincipalHidden),
		errors.Is(err, store.ErrPrincipalNotFound):
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "totp: record the enrolment failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "totp: enrolment started", "principal", caller.ID)
		writeSecretJSON(w, http.StatusOK, totpEnrollment(secret, totpAccount(caller.Name, r.Host)))
	}
}

// handleConfirmTOTP serves POST /v1/totp/confirm, on a setup-scoped
// credential: a code from the pending secret makes it the operator's second
// factor. A wrong code counts in the name's window, as every factor's
// failure does; with a password already set, setup is complete and the setup
// credential ends.
func (s *Server) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, _, ok := s.setupCaller(w, r)
	if !ok {
		return
	}
	var req schema.ConfirmTOTPRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the code is malformed")
		return
	}
	key := names.KeysOf(caller.Name).Folded
	if limit, ok, retry := s.logins.takeName(key, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	res, err := s.store.ConfirmTOTP(ctx, auditActor(ctx), store.TOTPConfirmation{
		PrincipalID: caller.ID, CredentialID: credID, Check: s.totpCheck(caller.ID, req.Code),
	}, s.now())
	if !errors.Is(err, store.ErrSecondFactorWrong) {
		// Only a wrong code is a failure; anything else spends nothing.
		s.logins.nameCancelled(key, s.now())
	}
	switch {
	case errors.Is(err, store.ErrSecondFactorWrong):
		s.logins.nameFailed(key, s.now())
		s.auditLoginFailed(r, fmt.Sprintf("name_key=%q reason=%s via=%s", key, loginReasonWrongCode, passwordFailureViaConfirm),
			loginReasonWrongCode+"|"+passwordFailureViaConfirm+"|"+key)
		writeError(w, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, "the code is not right; check the authenticator's clock and try the next code")
	case errors.Is(err, store.ErrTOTPNotEnrolled):
		writeError(w, http.StatusConflict, schema.ErrorCodeTOTPNotEnrolled, "no second factor is waiting to be confirmed; enrol first")
	case errors.Is(err, store.ErrTOTPAlreadyEnrolled):
		writeTOTPAlreadyEnrolled(w)
	case errors.Is(err, store.ErrSetupOnHost):
		writeSetupOnHost(w)
	case totpUnavailable(err):
		logTOTPUnavailable(ctx, caller.ID, err)
		writeTOTPKeyMissing(w)
	case errors.Is(err, store.ErrLoginStale), errors.Is(err, store.ErrPrincipalDisabled), errors.Is(err, store.ErrPrincipalHidden),
		errors.Is(err, store.ErrPrincipalNotFound):
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "totp: confirm the enrolment failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		if res.SetupEnded != 0 {
			s.credentialRevoked(ctx, res.SetupEnded)
		}
		slog.InfoContext(ctx, "totp: enrolled", "principal", caller.ID, "setup_complete", res.SetupEnded != 0)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleRedeemSetupToken serves POST /v1/setup/redeem, exempt from
// authentication: the setup token a host command printed, for a setup-scoped
// credential. The order is the reset's: the transport check; the per-address
// limit before the body is read; a malformed token the same 404 as any that
// does not work; the server-wide limit, given back if the store refuses; and
// the store, which spends the token in the transaction that issues the
// credential. The per-address limit is its own (setupLimiter).
func (s *Server) handleRedeemSetupToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.setupLimits.take(addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.RedeemSetupTokenRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeSetupTokenInvalid(w)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	res, err := s.store.RedeemSetupToken(ctx, store.HashSetupToken(req.Token), s.now())
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrSetupTokenInvalid):
		s.auditCodeRefused(r, auditSetupTokenRefused, schema.ErrorCodeSetupTokenInvalid)
		writeSetupTokenInvalid(w)
	case err != nil:
		// The store's errors carry no token or hash.
		slog.ErrorContext(ctx, "setup: redeem the token failed", "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "setup: token redeemed", "principal", res.Principal.ID, "credential", res.Credential.ID)
		writeSecretJSON(w, http.StatusCreated, schema.RedeemSetupTokenResponseV1{
			Principal:  principalFromStore(res.Principal),
			Credential: res.Credential,
			Token:      res.Token,
			Auth: schema.AuthStateV1{
				Scope:        schema.CredentialScopeSetup,
				PasswordSet:  res.PasswordSet,
				TOTPEnrolled: res.TOTPEnrolled,
				ExpiresAt:    res.Credential.ExpiresAt,
			},
		})
	}
}

// setupLimiter is the per-address limit of POST /v1/setup/redeem: invite
// redemption's policy (design §7.2), ten tries in ten minutes from one
// address, success or not, in a bucket of its own, so a setup token is not
// refused because the same address redeemed invites, nor the other way
// round. A setup token is 256 random bits; the limit bounds the work, not the
// odds of a guess. Like the invite limits it is in memory only, and the
// test-only relaxed limits raise it (Config.TestRelaxInviteLimits).
type setupLimiter struct {
	mu     sync.Mutex
	limit  inviteLimit
	byAddr bucketSet
}

func newSetupLimiter(cfg Config) *setupLimiter {
	l := inviteLimit{"setup_redeem_per_address", defaultRedeemPerAddress, defaultAddressWindow}
	if cfg.TestRelaxInviteLimits {
		l.burst = testRelaxedPerAddressBurst
	}
	return &setupLimiter{limit: l}
}

// take spends one of addr's tries.
func (l *setupLimiter) take(addr netip.Addr, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok, retry := l.byAddr.bucket(l.limit, limitKey(addr), overflowKey(addr), now).take(l.limit, now)
	return l.limit, ok, retry
}

func writeSetupTokenInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodeSetupTokenInvalid,
		"this setup token is not valid: it may be mistyped, used, expired or replaced; make a new one on the host")
}

// reauthRule is whether a route is in the re-authentication class (design
// §5.3, review F8) and how: always; only when an operator acts on someone
// other than themself (an operator-or-self route, such as signing everyone
// out); or only when the caller is an operator (a route anyone may use but
// that, from an operator, creates what the class covers, such as an invite,
// which makes principals; a member's use is unaffected).
type reauthRule int

const (
	reauthNone reauthRule = iota
	reauthAlways
	reauthOnOthers
	reauthOperators
)

type requestCredentialKey struct{}

// withRequestCredential returns ctx carrying what the middleware learned of
// the credential the request came with; never its token.
func withRequestCredential(ctx context.Context, c store.RequestCredential) context.Context {
	return context.WithValue(ctx, requestCredentialKey{}, c)
}

// requestCredentialFrom returns it; false under AuthOff, on exempt routes and
// for a ticket-authenticated socket.
func requestCredentialFrom(ctx context.Context) (store.RequestCredential, bool) {
	c, ok := ctx.Value(requestCredentialKey{}).(store.RequestCredential)
	return c, ok
}

// recentAuthGate wraps a route of the re-authentication class: a browser
// session must hold a recent-auth proof that has not run out, else 403
// reauth_required, audited as a denial; the client proves it with POST
// /v1/reauth and repeats the request. A script credential passes: it is
// outside the class by the S15 boundary (design §7.2), and cannot make a
// proof of its own (POST /v1/reauth refuses it). Anything the gate cannot
// read fails closed.
func (s *Server) recentAuthGate(rule reauthRule, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerFrom(r.Context())
		cred, hasCred := requestCredentialFrom(r.Context())
		if !ok || !hasCred {
			writeUnauthenticated(w)
			return
		}
		switch rule {
		case reauthOnOthers:
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if !isOperator(caller) || (err == nil && id == caller.ID) {
				next.ServeHTTP(w, r)
				return
			}
		case reauthOperators:
			if !isOperator(caller) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if !cred.Session || (cred.RecentAuthUntil != nil && s.now().Before(*cred.RecentAuthUntil)) {
			next.ServeHTTP(w, r)
			return
		}
		s.denyForbidden(w, r, caller, schema.ErrorCodeReauthRequired,
			"this action needs a recent proof of your password and code: re-authenticate with POST /v1/reauth and try again")
	})
}

// pendingLoginSweepInterval is how often spent and expired challenges are
// deleted.
const pendingLoginSweepInterval = time.Minute

// startPendingLoginSweeper deletes spent and expired pending logins every
// pendingLoginSweepInterval until ctx ends or stop is called; stop waits for
// it.
func (s *Server) startPendingLoginSweeper(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(pendingLoginSweepInterval)
		defer ticker.Stop()
		for {
			if _, err := s.store.SweepPendingLogins(ctx, s.now()); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "login: sweep spent challenges failed; the next pass tries again", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// checkTOTPKey says at start what operators' browser sign-in can do with the
// TOTP key this server has: none is a warning (no operator can enrol or sign
// in with a code), and a key that does not open a stored second factor is an
// error that names the fix. Neither stops the server: messaging and
// approvals do not depend on it, and the TOTP routes fail closed.
func (s *Server) checkTOTPKey(ctx context.Context) {
	if !s.cfg.authRequired() {
		return
	}
	id, sealed, found, err := s.store.AnyConfirmedTOTP(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "totp: read a stored second factor failed", "error", err)
		return
	}
	switch {
	case s.cfg.TOTPKey == nil && found:
		slog.ErrorContext(ctx, "totp: no TOTP key (CONCHD_TOTP_KEY) is set, but operators have a second factor: none of them can sign in with a password until the key is restored (docs/deploy.md)")
	case s.cfg.TOTPKey == nil:
		slog.InfoContext(ctx, "totp: no TOTP key (CONCHD_TOTP_KEY) is set: operators cannot enrol a second factor or sign in with a password; script credentials work as before")
	case found:
		secret, err := s.cfg.TOTPKey.openTOTP(id, sealed)
		if err != nil {
			slog.ErrorContext(ctx, "totp: the TOTP key (CONCHD_TOTP_KEY) does not open the stored second factor: it is not the key that was used; restore the right key, or recover each operator with conchd operator recover",
				"principal", id)
			return
		}
		clear(secret)
	}
}
