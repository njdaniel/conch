package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Create an account during a guest visit, and the operator's conversion
// permits (issue #711, ADR-009 D3, docs/design/guests-and-accounts.md §3.3,
// §7.2).
//
//   - POST /v1/guest/convert: a live guest chooses a permanent name and a
//     password and becomes a member under the same principal id. Its
//     memberships and their history bounds stay as they are, can_decide
//     included (conversion writes no membership), and nothing is granted.
//     The guest credential is revoked, its sockets closed, and a fresh
//     member credential issued, in the store's one transaction; a refusal
//     leaves the guest as it was. A lost answer is recovered by signing in
//     with the password, never by the old guest token, which no longer
//     works.
//   - POST /v1/guests/{id}/conversion-permit: an operator lets one live
//     guest do that although the policy of its visit's origin is closed. It
//     is in the re-authentication class (it creates a permit), so an
//     operator's browser session needs a recent proof.
//
// The conversion takes a password, so it answers only over HTTPS or
// loopback (passwordRoute), and every check that costs nothing comes before
// the hash: the caller, the body, the password's policy, the visit, the
// permit or the policy, the name-probe allowance and the name. The hash runs
// in the authenticated pool, so an anonymous sign-in flood does not hold a
// guest's conversion up (review F3); and since a guest is anyone the policy
// lets in, a guest cannot hold that pool up either, which operators need for
// re-authentication: each guest has one conversion in flight at a time, and
// a few hashed conversion attempts per window (conversions_per_principal),
// taken before the hash. A hash that has run is never thrown away: the write
// and what follows it run whether or not the caller is still there. The
// name-probe allowance is the guest's own, by principal, not its address's,
// so an anonymous caller behind the same address cannot spend it. Nothing
// here logs or audits a password, a record, a permit's code or a token.

// principalSet is a set of principal ids under a lock: the guests with a
// conversion in flight.
type principalSet struct {
	mu  sync.Mutex
	ids map[int64]struct{}
}

// add puts id in the set and reports true, or reports false when it is
// there already.
func (p *principalSet) add(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.ids[id]; ok {
		return false
	}
	if p.ids == nil {
		p.ids = map[int64]struct{}{}
	}
	p.ids[id] = struct{}{}
	return true
}

func (p *principalSet) remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.ids, id)
}

// conversionBusyRetry is the Retry-After of a conversion refused because
// another of the same guest's is in flight: about one hash's time.
const conversionBusyRetry = time.Second

// accountPassword normalizes a new account's password and holds it to the
// policy, against the account's name and the server's: what every route
// that makes an account with a password checks before it hashes. It writes
// the 400 and reports false when either fails.
func (s *Server) accountPassword(w http.ResponseWriter, password, name string) (string, bool) {
	n, err := normalizePassword(password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return "", false
	}
	if checkPasswordPolicy(n, name, s.serverName()) != nil {
		writeError(w, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable,
			"that password is too common or contains your name; choose another")
		return "", false
	}
	return n, true
}

// guestV1 is v as its guest sees it, with can_convert from the policy of its
// origin as it is now. A policy that cannot be read says false: the page
// offers nothing it cannot be sure of, and the conversion decides anyway.
func (s *Server) guestV1(ctx context.Context, v store.GuestVisit) schema.GuestV1 {
	g := v.V1()
	if v.State != schema.GuestStateActive {
		return g
	}
	can, err := s.store.GuestCanConvert(ctx, v.PrincipalID, s.now())
	if err != nil {
		slog.WarnContext(ctx, "guests: read whether a visit may convert failed", "principal", v.PrincipalID, "error", err)
		return g
	}
	g.CanConvert = can
	return g
}

func writeConversionPermitInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid, "this conversion permit is not valid")
}

// writeConversionRefused answers a conversion the store refused, and reports
// whether err was one of its refusals.
func (s *Server) writeConversionRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrNotAGuest):
		writeError(w, http.StatusForbidden, schema.ErrorCodeNotAGuest, "only a guest creates an account this way")
	case errors.Is(err, store.ErrGuestEnded):
		writeError(w, http.StatusConflict, schema.ErrorCodeGuestEnded, "the visit has ended; it cannot become an account")
	case errors.Is(err, store.ErrConversionNotAllowed):
		writeError(w, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed,
			"accounts cannot be made from this visit now; an operator can allow it")
	case errors.Is(err, store.ErrConversionPermitInvalid):
		writeConversionPermitInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	default:
		return s.writeJoinAccountRefused(w, err)
	}
	return true
}

// handleConvertGuest serves POST /v1/guest/convert: Create an account during
// a live visit. Anyone who is not a guest now is 403 not_a_guest, before the
// body is read; an ended or converted visit is 409 guest_ended; a closed
// policy with no permit is 403 conversion_not_allowed, or joining_paused or
// invites_paused by the visit's origin; a permit that does not work is 404
// conversion_permit_invalid; a taken or reserved name 409 principal_exists.
// The store checks all of it again in the converting transaction. With
// authentication off there is no guest: 403 accounts_require_auth.
func (s *Server) handleConvertGuest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return
	}
	if !caller.IsGuest() {
		writeError(w, http.StatusForbidden, schema.ErrorCodeNotAGuest, "only a guest creates an account this way")
		return
	}
	var req schema.ConvertGuestRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	// The name's and the password's rules are a 400 that names the rule; a
	// malformed permit is a permit that does not work, like any other.
	permit := req.Permit
	req.Permit = ""
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if permit != "" && schema.ValidateSignInLinkCode(permit) != nil {
		writeConversionPermitInvalid(w)
		return
	}
	password, ok := s.accountPassword(w, req.Password, req.Name)
	if !ok {
		return
	}
	// One conversion per guest at a time: a second one is refused at once,
	// with the 429 and Retry-After of a password route at capacity, before
	// it reads anything or hashes. Whichever finishes, the other would have
	// found the visit converted.
	if !s.converting.add(caller.ID) {
		writeRateLimited(w, &rateLimitedError{retryAfter: conversionBusyRetry})
		return
	}
	defer s.converting.remove(caller.ID)
	permitHash := ""
	if permit != "" {
		permitHash = store.HashSignInLinkCode(permit)
	}
	if err := s.store.CheckConversion(ctx, caller.ID, permitHash, s.now()); err != nil {
		if !s.writeConversionRefused(w, err) {
			slog.ErrorContext(ctx, "guests: conversion check failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
		}
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if !s.nameIsFree(w, r, addr, caller.ID, req.Name) {
		return
	}
	if limit, ok, retry := s.logins.takeConversion(caller.ID, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, caller.ID, addr)
		return
	}
	record, err := s.passwords.hashIn(ctx, s.passwords.authenticated, password)
	if err != nil {
		// The attempt is given back only when no hash ran: the pool had no
		// room, or the caller left while it waited.
		if errors.Is(err, errHashPoolBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.logins.refundConversion(caller.ID, s.now())
		}
		writeHashFailed(w, r, err)
		return
	}
	// The hash has run: the write and its teardown finish even if the
	// caller has gone, so a dropped connection cannot make the work be done
	// again for nothing.
	ctx = context.WithoutCancel(ctx)
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	conv, err := s.store.ConvertGuest(ctx, auditActor(ctx), store.GuestConversion{
		PrincipalID: caller.ID, Name: req.Name, Record: record, PermitHash: permitHash, Now: s.now(),
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			s.logins.spendConversionNameProbe(caller.ID, addr, s.now())
		}
		if !s.writeConversionRefused(w, err) {
			slog.ErrorContext(ctx, "guests: conversion failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
		}
		return
	}
	s.guestConverted(ctx, conv)
	writeSecretJSON(w, http.StatusOK, schema.ConvertGuestResponseV1{
		Principal:  principalFromStore(conv.Principal),
		Credential: conv.Credential,
		Token:      conv.Token,
	})
}

// guestConverted does what follows a committed conversion, before the
// answer is written: every socket opened with the guest credential closes,
// in every family, as after any revocation, and a voice room it held a
// session for rotates; the address book forgets the visit, as it forgets
// one that ended; and the member lists of its channels are told, since the
// entry there is a member's now. Sockets the member opens with the new
// credential are not touched.
func (s *Server) guestConverted(ctx context.Context, c store.ConvertedGuest) {
	for _, id := range c.Revoked {
		s.credentialRevoked(ctx, id)
	}
	s.guestAddrs.forget(c.Principal.ID)
	for _, ch := range c.Channels {
		s.events.notifyChannelRoster(ch)
	}
	// The member's sockets on the new credential, if any are open yet, are
	// told their channel list changed (issue #1027).
	s.events.notifyChannelsChanged(c.Principal.ID)
	slog.InfoContext(ctx, "guests: visit converted", "principal", c.Principal.ID, "credential", c.Credential.ID)
}

// handleCreateConversionPermit serves POST /v1/guests/{id}/conversion-permit:
// an operator lets one live guest create an account although the policy of
// its visit's origin is closed. The answer is the permit, a sign-in link of
// purpose conversion whose code is in the 201 body and nowhere else, sent
// no-store; the operator passes the code to the guest. A principal that
// never was a guest is 404 not_a_guest, a visit that ended or converted 409
// guest_ended. Making one is limited per operator, in the bucket invites and
// sign-in links use.
//
// A permit stands alone, the origin's pause included: a permit outstanding
// when joining or invites are paused stays usable until it expires (at most
// an hour), since an operator made it for one principal by name. Closing it
// is ending the visit, or letting it run out.
func (s *Server) handleCreateConversionPermit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.operatorGuestCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	if limit, ok, retry := s.inviteLimits.takeCreate(caller.ID, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, caller.ID, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	var req schema.CreateConversionPermitRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	now := s.now()
	if err := req.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	resp, err := s.store.CreateConversionPermit(ctx, auditActor(ctx), caller.ID, id, req, now)
	switch {
	case errors.Is(err, store.ErrSignInLinkRequestInvalid):
		// Validated above; the clock moved past a just-valid expiry.
		writeError(w, http.StatusBadRequest, "invalid_request", "the conversion permit request is not valid")
	case errors.Is(err, store.ErrNotAGuest):
		writeError(w, http.StatusNotFound, schema.ErrorCodeNotAGuest, "no guest visit has that id")
	case errors.Is(err, store.ErrGuestEnded):
		writeError(w, http.StatusConflict, schema.ErrorCodeGuestEnded, "the visit has ended; it cannot become an account")
	case errors.Is(err, store.ErrSignInLinkNotAllowed):
		// The route table admits operators only; the caller stopped being
		// one a moment ago. Fail closed.
		s.denyForbidden(w, r, caller, "forbidden", "only an operator makes a conversion permit")
	case err != nil:
		slog.ErrorContext(ctx, "guests: create conversion permit failed", "principal", id, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "guests: conversion permit created", "operator", caller.ID, "principal", id, "sign_in_link", resp.SignInLink.ID)
		writeSecretJSON(w, http.StatusCreated, resp)
	}
}
