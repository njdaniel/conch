package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Password enrolment, change, reset and the recent-password proof (issue
// #708, docs/design/guests-and-accounts.md §5.2, §6.1, §7.2):
//
//   - POST /v1/password/mine: a member who predates passwords sets a first
//     one on their existing session, with an enrollment link an operator
//     made for them as the proof, since a long-lived bearer alone must not
//     set a password. On a setup-scoped credential (the host-made setup
//     flow, #709/#710) it sets or replaces an operator's.
//   - PUT /v1/password/mine: a member changes their password, proving the
//     current one. Every other session, link, pending login and proof of
//     theirs ends, and the session they changed it from is rotated.
//   - POST /v1/password/reset: open to anyone holding a reset link an
//     operator made after checking who the member is. It replaces the
//     password and ends everything else; it signs nobody in.
//   - POST /v1/reauth: a member proves their password again, and the
//     session they did it from holds a proof for ten minutes, which the
//     routes of the re-authentication class ask for (#712).
//
// An operator's password change and re-authentication need a current TOTP
// code as well (design §5.3, issue #709): a missing one is a failure before
// any password work, and the code is checked in the transaction that writes,
// which records its step, so it cannot be used twice. Every missing, wrong
// or replayed code answers auth_failed and counts in the name's window, as a
// wrong password does (review F2). Where password-only is allowed (ADR-011 item 2.1, ADR-012)
// the code is asked for only when the operator has enrolled one.
//
// Every route here takes a password, so each checks what passwordRoute
// checks first: authentication on and HTTPS (or loopback). Hashes and
// verifications run in the authenticated pool, but the reset's in the
// anonymous one (password.go). A wrong current password counts in the
// name's failure window, as a wrong sign-in does (review F2), so a stolen
// bearer cannot be used to guess the password behind it any faster than the
// sign-in route allows. Nothing here logs or audits a password, a record, a
// code or a token; a new token appears only in a response body, sent
// no-store.

// passwordFailureVia names, in a login_failed row, which route a wrong
// password was presented to, beside the sign-in's reasons.
const (
	passwordFailureViaChange = "password_change"
	passwordFailureViaReauth = "reauth"
)

// auditPasswordResetRefused records, thinned as sign_in_link_redeem_refused
// is, that a well-formed code reset nobody's password.
const auditPasswordResetRefused = "password_reset_refused"

// authChanged is what follows a committed bump of principal id's
// authentication generation (a password change, a reset or an operator's
// recovery): every socket of the principal closes, rosters learn their
// sessions ended, and they leave voice, as after a revoke-all (disable.go).
// The store has already revoked the credentials; this reaches what is live.
func (s *Server) authChanged(ctx context.Context, id int64) {
	s.hub.DropPrincipalAll(id)
	s.events.dropPrincipalAll(id)
	s.events.notifyRosters()
	s.voicePrincipalLostAccess(ctx, id, voiceReasonCredsRevoked)
}

// passwordCaller is the start of every authenticated password route: the
// transport and authentication-mode check, then the caller and the
// credential the request came with, which must be a person's. An agent is
// refused (403 human_required, audited) and so is a guest (403
// guest_not_allowed): a guest makes an account by converting.
func (s *Server) passwordCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	if !s.passwordRoute(w, r) {
		return store.Principal{}, 0, false
	}
	caller, ok := callerFrom(r.Context())
	credID, hasCred := credentialIDFrom(r.Context())
	if !ok || !hasCred {
		// Authentication is on, so the middleware set both; their absence
		// is a wiring fault, which must deny.
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	switch {
	case caller.Kind != store.PrincipalHuman:
		s.denyForbidden(w, r, caller, schema.ErrorCodeHumanRequired, "passwords are for people")
		return store.Principal{}, 0, false
	case caller.IsGuest():
		writeError(w, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed, "a guest has no password; create an account instead")
		return store.Principal{}, 0, false
	}
	return caller, credID, true
}

// passwordSubject reads the caller as the password routes need it, writing a
// 500 and reporting false when the store fails. A credential that is gone
// is answered as one that never authenticated.
func (s *Server) passwordSubject(w http.ResponseWriter, r *http.Request, caller store.Principal, credID int64) (store.PasswordSubject, bool) {
	subj, err := s.store.PasswordSubject(r.Context(), caller.ID, credID, s.now())
	switch {
	case errors.Is(err, store.ErrCredentialNotFound), errors.Is(err, store.ErrPrincipalNotFound):
		writeUnauthenticated(w)
		return store.PasswordSubject{}, false
	case err != nil:
		slog.ErrorContext(r.Context(), "passwords: read the caller failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return store.PasswordSubject{}, false
	}
	return subj, true
}

// newPassword normalizes a password to be set and holds it to the policy,
// writing the 400 and reporting false when it fails either.
func newPassword(w http.ResponseWriter, password, name string) (string, bool) {
	n, err := normalizePassword(password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return "", false
	}
	if checkPasswordPolicy(n, name) != nil {
		writeError(w, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable,
			"that password is too common or contains your name; choose another")
		return "", false
	}
	return n, true
}

// writeHashFailed answers a hash or verification that did not run: the pool
// was full or the caller left (a 429 with Retry-After), or hashing failed.
func writeHashFailed(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, errHashPoolBusy) && r.Context().Err() == nil {
		slog.ErrorContext(r.Context(), "passwords: hash the password failed", "error", err)
		writeInternalError(w)
		return
	}
	writePasswordBusy(w)
}

// writeSetupScopeRefused answers a setup-scoped credential on a route that
// is not one of the routes that finish setup (design §5.3, review F6).
func writeSetupScopeRefused(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeSetupIncomplete, "this credential is for finishing setup only")
}

// handleSetPassword serves POST /v1/password/mine: the caller's first
// password, or on a setup-scoped credential an operator's new one. The
// checks that cost nothing come first, so a refused request does no Argon2
// work: the body; the caller; on a full-scope credential an operator (403
// setup_on_host), a password already set (409 password_already_set) and no
// proof (403 reauth_required); then the proof, which must be a usable
// enrollment link made for the caller (else 404 sign_in_link_invalid, the
// answer for any code that does not work); the policy; the hash; and the
// store, which re-checks all of it in the transaction that writes.
func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, ok := s.passwordCaller(w, r)
	if !ok {
		return
	}
	var req schema.SetPasswordRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	// The password's rule is a 400; a malformed proof is a code that does
	// not work, like any other.
	if err := schema.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Proof != "" && schema.ValidateSignInLinkCode(req.Proof) != nil {
		writeSignInLinkInvalid(w)
		return
	}
	subj, ok := s.passwordSubject(w, r, caller, credID)
	if !ok {
		return
	}
	if subj.Scope != schema.CredentialScopeSetup {
		switch {
		case isOperator(caller):
			writeError(w, http.StatusForbidden, schema.ErrorCodeSetupOnHost,
				"an operator's password is set through a setup token made on the host (conchd), not on this credential")
			return
		case subj.Record != "":
			writePasswordAlreadySet(w)
			return
		case req.Proof == "":
			writeEnrollmentProofRequired(w)
			return
		}
	}
	proofHash := ""
	if req.Proof != "" {
		proofHash = store.HashSignInLinkCode(req.Proof)
		switch err := s.store.CheckEnrollmentProof(ctx, proofHash, caller.ID, s.now()); {
		case errors.Is(err, store.ErrSignInLinkInvalid):
			writeSignInLinkInvalid(w)
			return
		case err != nil:
			slog.ErrorContext(ctx, "passwords: check the enrolment proof failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
			return
		}
	}
	password, ok := newPassword(w, req.Password, caller.Name)
	if !ok {
		return
	}
	record, err := s.passwords.hashIn(ctx, s.passwords.authenticated, password)
	if err != nil {
		writeHashFailed(w, r, err)
		return
	}
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	res, err := s.store.EnrollPassword(ctx, auditActor(ctx), store.PasswordEnrollment{
		PrincipalID: caller.ID, CredentialID: credID, Record: record, ProofHash: proofHash,
	}, s.now())
	switch {
	case errors.Is(err, store.ErrSignInLinkInvalid):
		writeSignInLinkInvalid(w)
	case errors.Is(err, store.ErrPasswordAlreadySet):
		writePasswordAlreadySet(w)
	case errors.Is(err, store.ErrEnrollmentProofRequired):
		writeEnrollmentProofRequired(w)
	case errors.Is(err, store.ErrSetupOnHost):
		writeError(w, http.StatusForbidden, schema.ErrorCodeSetupOnHost,
			"an operator's password is set through a setup token made on the host (conchd), not on this credential")
	case errors.Is(err, store.ErrSetupCredentialRestricted):
		// Another request on this credential set the password first, on a
		// server that allows password-only: it now reaches only the
		// authenticator routes, as the middleware answers (ADR-011 item 2.1).
		writeSetupScopeRefused(w)
	case errors.Is(err, store.ErrLoginStale), errors.Is(err, store.ErrPrincipalDisabled), errors.Is(err, store.ErrPrincipalHidden),
		errors.Is(err, store.ErrPrincipalNotFound):
		// The credential or the person was ended while the password was
		// hashed: the request no longer has anyone to act for.
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "passwords: store the password failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		s.passwordUpdated(ctx, res)
		slog.InfoContext(ctx, "passwords: set", "principal", caller.ID, "rotated", res.Rotated)
		writePasswordUpdated(w, res)
	}
}

// writePasswordAlreadySet's message names no route: a refusal carries no
// digit, so the isolation check can hold every refusal to that (#713).
func writePasswordAlreadySet(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodePasswordAlreadySet,
		"this account already has a password; change it by giving the current one")
}

func writeEnrollmentProofRequired(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired,
		"setting a first password needs an enrollment link from an operator")
}

// passwordUpdated runs what follows a committed EnrollPassword,
// ChangePassword or ResetPassword: a rotated credential's sockets close as a
// revoked one's do, and a bump closes everything of the principal's that is
// live.
func (s *Server) passwordUpdated(ctx context.Context, res store.PasswordUpdate) {
	// A setup credential that completed its setup ends like any revoked one
	// (issue #709).
	if res.SetupEnded != 0 {
		s.credentialRevoked(ctx, res.SetupEnded)
	}
	if res.Bumped {
		s.authChanged(ctx, res.PrincipalID)
		return
	}
	if res.Rotated != 0 {
		s.credentialRevoked(ctx, res.Rotated)
	}
}

// writePasswordUpdated writes the 200 of a password write, no-store when it
// carries a token.
func writePasswordUpdated(w http.ResponseWriter, res store.PasswordUpdate) {
	out := schema.PasswordUpdatedResponseV1{Revoked: res.Revoked}
	if res.Token != "" {
		cred := res.Credential
		out.Credential, out.Token = &cred, res.Token
	}
	writeSecretJSON(w, http.StatusOK, out)
}

// presentedPassword runs the verification of a password the caller presents
// as their current one, after the checks that cost nothing: it takes from
// the name's windows first (429 at once when they are spent, before any
// hashing), verifies in the authenticated pool, and on a wrong password
// settles the failure, audits login_failed with via, and answers 401
// auth_failed. It returns the outcome and the name key, whose reservation
// the caller settles (nameSucceeded or nameCancelled) once the store has
// answered; false means the response is written and the reservation
// settled.
func (s *Server) presentedPassword(w http.ResponseWriter, r *http.Request, caller store.Principal, subj store.PasswordSubject, password, via string) (verifyResult, string, bool) {
	key := names.KeysOf(caller.Name).Folded
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.logins.takeName(key, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, addr)
		return verifyResult{}, "", false
	}
	presented, fits := normalizePresented(password)
	record := subj.Record
	if !fits {
		// No account has a password that long: the same work, and a miss.
		record = ""
	}
	res, err := s.passwords.verifyIn(r.Context(), s.passwords.authenticated, record, presented)
	if err != nil {
		s.logins.nameCancelled(key, s.now())
		writePasswordBusy(w)
		return verifyResult{}, "", false
	}
	if res.badRecord {
		slog.ErrorContext(r.Context(), "passwords: a stored password record cannot be verified; it must be set again", "principal", caller.ID)
	}
	if !res.ok {
		s.logins.nameFailed(key, s.now())
		reason := loginReasonWrongPassword
		if res.badRecord {
			reason = loginReasonBadRecord
		}
		s.auditLoginFailed(r, fmt.Sprintf("name_key=%q reason=%s via=%s", key, reason, via), reason+"|"+via+"|"+key)
		writeError(w, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, "the password is not right")
		return verifyResult{}, "", false
	}
	return res, key, true
}

// handleChangePassword serves PUT /v1/password/mine: a member replaces their
// password with the current one, an operator with a TOTP code as well. The
// order: the body; the caller; an operator with no code is a failure at
// once (secondFactorMissing); a setup-scoped credential is refused; no
// password yet is 409 password_not_set; the new password's policy; then the
// current password (presentedPassword); the new hash; and the store, which
// re-checks the verification and the credential, checks an operator's code,
// and, in one transaction, stores the password, moves the generation on,
// revokes every other credential and rotates this one. A change that loses a
// race with another change, a reset, a sign-out everywhere, a disable or a
// hide answers unauthenticated, as the middleware does for a dead
// credential, and changes nothing: 401 auth_failed on this route is only
// ever a wrong password or code (issue #814).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, ok := s.passwordCaller(w, r)
	if !ok {
		return
	}
	var req schema.ChangePasswordRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	// The schema's messages name the rule broken, never a password.
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if isOperator(caller) && req.Code == "" && !s.cfg.passwordOnlyAllowed() {
		s.secondFactorMissing(w, r, caller, passwordFailureViaChange)
		return
	}
	subj, ok := s.passwordSubject(w, r, caller, credID)
	if !ok {
		return
	}
	// Where password-only is allowed the code is asked for only when one is enrolled
	// (ADR-011 item 2.1); a code sent by an operator who has none is not
	// looked at.
	passwordOnly := s.passwordOnlyOperator(caller, subj)
	if isOperator(caller) && req.Code == "" && !passwordOnly {
		s.secondFactorMissing(w, r, caller, passwordFailureViaChange)
		return
	}
	if subj.Scope != schema.CredentialScopeFull {
		writeSetupScopeRefused(w)
		return
	}
	if subj.Record == "" {
		writePasswordNotSet(w)
		return
	}
	password, ok := newPassword(w, req.Password, caller.Name)
	if !ok {
		return
	}
	// A rehash of the current record is moot: the change replaces it.
	_, key, ok := s.presentedPassword(w, r, caller, subj, req.Current, passwordFailureViaChange)
	if !ok {
		return
	}
	record, err := s.passwords.hashIn(ctx, s.passwords.authenticated, password)
	if err != nil {
		s.logins.nameCancelled(key, s.now())
		writeHashFailed(w, r, err)
		return
	}
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	change := store.PasswordChange{
		PrincipalID: caller.ID, CredentialID: credID, AuthGeneration: subj.AuthGeneration,
		Record: subj.Record, NewRecord: record,
	}
	if isOperator(caller) && !passwordOnly {
		change.SecondFactor = s.totpCheck(caller.ID, req.Code)
	}
	res, err := s.store.ChangePassword(ctx, auditActor(ctx), change, s.now())
	switch {
	case errors.Is(err, store.ErrSecondFactorWrong):
		s.secondFactorFailed(w, r, key, loginReasonWrongCode, passwordFailureViaChange)
	case errors.Is(err, store.ErrSecondFactorRequired):
		s.logins.nameCancelled(key, s.now())
		writeSecondFactorRequired(w)
	case totpUnavailable(err):
		// No code was looked at: nothing counts, and nothing changes.
		s.logins.nameCancelled(key, s.now())
		logTOTPUnavailable(ctx, caller.ID, err)
		writeSecondFactorRequired(w)
	case errors.Is(err, store.ErrLoginStale), errors.Is(err, store.ErrPrincipalDisabled), errors.Is(err, store.ErrPrincipalHidden),
		errors.Is(err, store.ErrPrincipalNotFound):
		// Something changed while the password was verified: another
		// change, a reset, a sign-out everywhere, a disable or a hide won,
		// and the credential the request came with is no longer live. That
		// is a sign-in to do again, not a wrong password (issue #814), so
		// it is the middleware's answer, as handleSetPassword's is.
		s.logins.nameCancelled(key, s.now())
		writeUnauthenticated(w)
	case err != nil:
		s.logins.nameCancelled(key, s.now())
		slog.ErrorContext(ctx, "passwords: change failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		// A credential was issued: the name's failure window starts again.
		s.logins.nameSucceeded(key, s.now())
		s.passwordUpdated(ctx, res)
		slog.InfoContext(ctx, "passwords: changed", "principal", caller.ID, "revoked", res.Revoked)
		writePasswordUpdated(w, res)
	}
}

func writePasswordNotSet(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodePasswordNotSet,
		"this account has no password yet; an operator can give you an enrollment link to set one")
}

// handleResetPassword serves POST /v1/password/reset, exempt from
// authentication: a reset link's code and a new password. The order is the
// v2 invite redemption's: the transport check; the per-address limit before
// the body is read; a malformed code the same 404 as any invalid one, a
// malformed password a 400; the link checked before any hashing, so a code
// that does not work costs no Argon2 work; the policy, against the member's
// name; the hash, in the anonymous pool; the server-wide limit last, given
// back if the store refuses. The store re-checks the link and, in one
// transaction, stores the password, spends the link, moves the generation
// on and revokes every credential of the member. The answer carries no
// credential: the member signs in with the new password.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.ResetPasswordRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if schema.ValidateSignInLinkCode(req.Code) != nil {
		writeSignInLinkInvalid(w)
		return
	}
	if err := schema.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	codeHash := store.HashSignInLinkCode(req.Code)
	target, err := s.store.CheckResetLink(ctx, codeHash, s.now())
	switch {
	case errors.Is(err, store.ErrSignInLinkInvalid):
		s.auditCodeRefused(r, auditPasswordResetRefused, schema.ErrorCodeSignInLinkInvalid)
		writeSignInLinkInvalid(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "passwords: check the reset link failed", "error", err)
		writeInternalError(w)
		return
	}
	password, ok := newPassword(w, req.Password, target.Name)
	if !ok {
		return
	}
	record, err := s.passwords.hashIn(ctx, s.passwords.anonymous, password)
	if err != nil {
		writeHashFailed(w, r, err)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	res, err := s.store.ResetPassword(ctx, codeHash, record, s.now())
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrSignInLinkInvalid):
		s.auditCodeRefused(r, auditPasswordResetRefused, schema.ErrorCodeSignInLinkInvalid)
		writeSignInLinkInvalid(w)
	case err != nil:
		// The store's errors carry no code, hash or record.
		slog.ErrorContext(ctx, "passwords: reset failed", "error", err)
		writeInternalError(w)
	default:
		s.passwordUpdated(ctx, res)
		slog.InfoContext(ctx, "passwords: reset", "principal", res.PrincipalID, "revoked", res.Revoked)
		writePasswordUpdated(w, res)
	}
}

// handleReauth serves POST /v1/reauth: the caller proves their password
// again, an operator a current TOTP code too, and the credential the request
// came with, and no other, holds a recent-auth proof for
// schema.RecentAuthLifetimeSeconds. The order: the body; the caller; an
// operator with no code is a failure at once (secondFactorMissing); a
// setup-scoped credential is refused; no password is 409 password_not_set; a
// script credential, which is outside the re-authentication class (design
// §7.2, the S15 boundary), gets no proof (403 forbidden), so it can never
// make a browser session look recently authenticated; then the password
// (presentedPassword) and the store, which re-checks the verification and
// the credential and checks an operator's code in the transaction that
// records the proof. A wrong code counts in the name's window. A successful
// proof spends nothing of the name's windows and resets neither: it issues
// no credential.
func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, ok := s.passwordCaller(w, r)
	if !ok {
		return
	}
	var req schema.ReauthRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the password or code is malformed")
		return
	}
	if isOperator(caller) && req.Code == "" && !s.cfg.passwordOnlyAllowed() {
		s.secondFactorMissing(w, r, caller, passwordFailureViaReauth)
		return
	}
	subj, ok := s.passwordSubject(w, r, caller, credID)
	if !ok {
		return
	}
	// As for a change: where password-only is allowed, an operator with no
	// authenticator proves the password alone (ADR-011 item 2.1), which
	// covers ADR-010's role route and owner mark too.
	passwordOnly := s.passwordOnlyOperator(caller, subj)
	if isOperator(caller) && req.Code == "" && !passwordOnly {
		s.secondFactorMissing(w, r, caller, passwordFailureViaReauth)
		return
	}
	switch {
	case subj.Scope != schema.CredentialScopeFull:
		writeSetupScopeRefused(w)
		return
	case subj.Record == "":
		writePasswordNotSet(w)
		return
	case !subj.Session:
		writeNotASession(w)
		return
	}
	verified, key, ok := s.presentedPassword(w, r, caller, subj, req.Password, passwordFailureViaReauth)
	if !ok {
		return
	}
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	issue := store.LoginIssue{
		PrincipalID: caller.ID, AuthGeneration: subj.AuthGeneration, Record: subj.Record, Rehash: verified.rehash,
	}
	var (
		until time.Time
		err   error
	)
	switch {
	case passwordOnly:
		until, err = s.store.RecordPasswordOnlyOperatorRecentAuth(ctx, issue, credID, s.now())
	case isOperator(caller):
		until, err = s.store.RecordOperatorRecentAuth(ctx, issue, credID, s.totpCheck(caller.ID, req.Code), s.now())
	default:
		until, err = s.store.RecordRecentAuth(ctx, issue, credID, s.now())
	}
	if errors.Is(err, store.ErrSecondFactorWrong) {
		s.secondFactorFailed(w, r, key, loginReasonWrongCode, passwordFailureViaReauth)
		return
	}
	s.logins.nameCancelled(key, s.now())
	switch {
	case totpUnavailable(err):
		logTOTPUnavailable(ctx, caller.ID, err)
		writeSecondFactorRequired(w)
	case errors.Is(err, store.ErrSecondFactorRequired):
		// Only a password-only proof gets this: the operator enrolled while
		// the password was verified, or the store does not allow password-only.
		// No code was looked at, so nothing counts.
		writeSecondFactorRequired(w)
	case errors.Is(err, store.ErrNotASession):
		writeNotASession(w)
	case errors.Is(err, store.ErrLoginStale):
		// The credential or the account changed while the password was
		// verified: the credential is no longer live (issue #814).
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "passwords: record the proof failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "passwords: re-authenticated", "principal", caller.ID, "credential", credID)
		writeJSON(w, http.StatusOK, schema.ReauthResponseV1{Until: schema.NewTimestamp(until)})
	}
}

// passwordOnlyOperator reports whether caller is an operator who, on a
// server that allows password-only, proves themself with a password alone:
// one with no confirmed authenticator as subj read it (ADR-011 item 2.1,
// ADR-012). Both the configuration and the stored flag, as subj read it,
// must allow it, so the two cannot disagree into a weaker answer; the store
// reads the flag and the enrolment again in the transaction that writes, and
// asks for the code if either says so.
func (s *Server) passwordOnlyOperator(caller store.Principal, subj store.PasswordSubject) bool {
	return isOperator(caller) && s.passwordOnlyAllowedFor(subj) && !subj.TOTPEnrolled
}

// passwordOnlyAllowedFor reports whether password-only is allowed for what a
// route tells the caller: the configuration says so and the store's flag,
// read with subj, agrees (ADR-012 item 3). Either saying no is no.
func (s *Server) passwordOnlyAllowedFor(subj store.PasswordSubject) bool {
	return s.cfg.passwordOnlyAllowed() && subj.PasswordOnlyAllowed
}

func writeNotASession(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden",
		"a recent-password proof is kept on a sign-in session, not on a script credential; sign in with your password")
}

// authWhoAmI fills whoami's auth (design §7.1): the credential's scope and
// bounds, whether the caller has a password and, for an operator, a
// confirmed second factor, whether setup is complete, and the
// recent-password proof when one stands. A client shows setup incomplete
// until setup_complete is true. Only for the people who have accounts: a
// guest has none to set up and an agent signs in with credentials alone.
func (s *Server) authWhoAmI(ctx context.Context, caller store.Principal, credID int64, resp *schema.WhoAmIResponseV1) error {
	if caller.Kind != store.PrincipalHuman || caller.IsGuest() {
		return nil
	}
	subj, err := s.store.PasswordSubject(ctx, caller.ID, credID, s.now())
	if err != nil {
		return err
	}
	auth := schema.AuthStateV1{
		Scope:       subj.Scope,
		PasswordSet: subj.Record != "",
		ExpiresAt:   timestampPtr(subj.ExpiresAt),
		// The idle bound is the earlier of the two, so it is never later
		// than the absolute one.
		IdleExpiresAt:   timestampPtr(subj.IdleExpiresAt),
		RecentAuthUntil: timestampPtr(subj.RecentAuthUntil),
	}
	auth.SetupComplete = auth.PasswordSet
	if isOperator(caller) {
		auth.TOTPEnrolled = subj.TOTPEnrolled
		// An authenticator is part of setup only where the mode asks for
		// one (ADR-011 item 2.1): where password-only is allowed the password
		// completes it, and the web Setup page and conch setup stop waiting.
		auth.SetupComplete = auth.PasswordSet && (subj.TOTPEnrolled || s.passwordOnlyAllowedFor(subj))
	}
	resp.Auth = &auth
	return nil
}

func timestampPtr(t *time.Time) *schema.Timestamp {
	if t == nil {
		return nil
	}
	ts := schema.NewTimestamp(*t)
	return &ts
}
