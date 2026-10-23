package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Invites on the REST API (issue #437, ADR-008 §4, docs/design/private-alpha.md
// §3). The store (store/invites.go) holds every rule of who may make, see, use
// and close an invite; these handlers map its answers to statuses and codes
// and add what only HTTP has: rate limits, body limits and the client address.
//
// Two of the routes are exempt from authentication and mint credentials:
// preview and redeem. Both spend a rate-limit token (invite_limits.go) before
// the body is read, and the body is small and bounded. The code travels only
// in a POST body, never in a path or a query, so it never reaches an access
// log; the link clients build carries it in the URL fragment, which no server
// sees.
//
// Nothing here logs, audits or echoes an invite code, a redeeming name, or the
// new credential's token. The code is hashed with store.HashInviteCode before
// it is looked up, and the plaintext token appears only in the body of a
// successful redeem, sent no-store.

// inviteBodyMaxBytes bounds every invite request body. The largest, a create
// naming MaxInviteChannels channels of the longest name, is well under it.
const inviteBodyMaxBytes = 4 << 10

// denyAgentInvites is the access_denied reason of an agent on an invite route.
const denyAgentInvites = "agents_no_invites"

// inviteInvalidMessage is the one message of every invalid code. Every
// refusal of a code, whatever its reason, is these same bytes.
const inviteInvalidMessage = "this invite is not valid"

// writeInviteInvalid writes the single answer for a code that is malformed,
// unknown, expired, used up, closed, or whose maker can no longer stand
// behind it, so the answer is not an oracle for which.
func writeInviteInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodeInviteInvalid, inviteInvalidMessage)
}

func writeInvitesPaused(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused, "invites are paused on this server")
}

// decodeInviteBody reads one JSON object of at most inviteBodyMaxBytes into
// dst, rejecting unknown fields. On failure it writes 413 or 400 with a fixed
// message (the decoder's own could quote the body) and returns false.
func decodeInviteBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeBoundedBody(w, r, dst, inviteBodyMaxBytes)
}

// decodeBoundedBody is decodeInviteBody with a bound of maxBytes, for a body
// that can be larger than an invite's (the joining settings, join.go).
func decodeBoundedBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if extra := dec.Decode(&struct{}{}); !errors.Is(extra, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one valid JSON object of the documented shape")
	return false
}

// inviteCaller is the check the three authenticated invite routes start with:
// a verified human. With authentication off there is nobody to make or own an
// invite, so the answer is the 401 of whoami. Invites are for people: an
// agent is refused with human_required, audited, before anything is looked at.
func (s *Server) inviteCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return store.Principal{}, false
	}
	if caller.Kind != store.PrincipalHuman {
		s.auditAgentDenial(r.Context(), caller.ID, inviteAuditSubject(r), "", 0, denyAgentInvites)
		writeError(w, http.StatusForbidden, schema.ErrorCodeHumanRequired, "invites are for people")
		return store.Principal{}, false
	}
	return caller, true
}

// handleCreateInvite serves POST /v1/invites: the caller makes an invite. The
// code is in the 201 body and nowhere else, sent no-store.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	if limit, ok, retry := s.inviteLimits.takeCreate(caller.ID, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, caller.ID, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	var req schema.CreateInviteRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	now := s.now()
	// The schema's messages name the rule broken, never the value sent.
	if err := req.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	resp, err := s.store.CreateInvite(ctx, auditActor(ctx), caller.ID, req, store.InviteOptions{Allowance: s.cfg.InviteAllowance}, now)
	switch {
	case errors.Is(err, store.ErrInviteRequestInvalid):
		// Validated above; the clock moved past a just-valid expiry.
		writeError(w, http.StatusBadRequest, "invalid_request", "the invite request is not valid")
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case errors.Is(err, store.ErrInviteNotAllowed):
		s.denyForbidden(w, r, caller, schema.ErrorCodeInviteNotAllowed, "you may not make this kind of invite")
	case errors.Is(err, store.ErrInviteGrantExceedsInviter):
		s.denyForbidden(w, r, caller, schema.ErrorCodeInviteGrantExceedsInviter, "an invite may grant only what its maker holds, and only an operator grants the right to invite")
	case errors.Is(err, store.ErrInviteAllowanceExceeded):
		s.denyForbidden(w, r, caller, schema.ErrorCodeInviteAllowanceExceeded, "you already have as many open personal invites as you may")
	case errors.Is(err, store.ErrNotFound):
		// Only an operator gets this far with a channel that does not exist;
		// anyone else gets invite_grant_exceeds_inviter for it.
		writeChannelNotFound(w)
	case errors.Is(err, store.ErrPrincipalNotFound):
		// The caller was resolved a moment ago; fail closed.
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "invites: create failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "invites: created", "principal", caller.ID, "invite", resp.Invite.ID, "kind", resp.Invite.Kind)
		writeSecretJSON(w, http.StatusCreated, resp)
	}
}

// handleListInvites serves GET /v1/invites, newest first. An operator sees
// every invite (issue #438); anyone else sees their own, as before. Both may
// narrow the list with ?kind= (personal, open) and ?state= (open, used,
// expired, closed); an operator also with ?created_by=<id>. A non-operator
// naming anyone but themselves there gets an empty list: none of their own
// invites was made by someone else.
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	filter, ok := inviteFilterFrom(w, r)
	if !ok {
		return
	}
	if !isOperator(caller) {
		if filter.CreatedBy != 0 && filter.CreatedBy != caller.ID {
			writeJSON(w, http.StatusOK, schema.ListInvitesResponseV1{Invites: []schema.InviteV1{}})
			return
		}
		filter.CreatedBy = caller.ID
	}
	invites, err := s.store.ListInvites(ctx, filter, s.now())
	if err != nil {
		slog.ErrorContext(ctx, "invites: list failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.ListInvitesResponseV1{Invites: invites})
}

// inviteFilterFrom reads the list filters from the query. On a value it does
// not know it writes a 400 that names the parameter, not the value, and
// returns false.
func inviteFilterFrom(w http.ResponseWriter, r *http.Request) (store.InviteFilter, bool) {
	q := r.URL.Query()
	var f store.InviteFilter
	if k := q.Get("kind"); k != "" {
		if !schema.InviteKind(k).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", "kind must be one of: "+strings.Join(schema.Vocabularies["InviteKind"], ", "))
			return f, false
		}
		f.Kind = schema.InviteKind(k)
	}
	if st := q.Get("state"); st != "" {
		switch state := store.InviteState(st); state {
		case store.InviteStateOpen, store.InviteStateUsed, store.InviteStateExpired, store.InviteStateClosed:
			f.State = state
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "state must be one of: open, used, expired, closed")
			return f, false
		}
	}
	if c := q.Get("created_by"); c != "" {
		id, err := strconv.ParseInt(c, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "created_by must be a positive integer")
			return f, false
		}
		f.CreatedBy = id
	}
	return f, true
}

// handleCloseInvite serves DELETE /v1/invites/{id}: the caller closes an
// invite of theirs. The store decides whose it is; anyone else's is the same
// 404 as an id that does not exist. An operator may close anyone's, as the
// store allows: operators administer invites (issue #438), and closing one is
// how a leaked link is stopped. Closing a closed invite is a 204 that changes
// nothing.
func (s *Server) handleCloseInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", "invite")
	if !ok {
		return
	}
	_, err := s.store.CloseInvite(ctx, auditActor(ctx), caller.ID, id, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
	case err != nil:
		slog.ErrorContext(ctx, "invites: close failed", "principal", caller.ID, "invite", id, "error", err)
		writeInternalError(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePreviewInvite serves POST /v1/invites/preview, exempt from
// authentication: what the invite would grant, without using it. While
// invites are paused it answers invites_paused whatever the code, as redeem
// does (the store checks the pause before the code).
func (s *Server) handlePreviewInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(false, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.InvitePreviewRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeInviteInvalid(w)
		return
	}
	preview, err := s.store.PreviewInvite(ctx, store.HashInviteCode(req.Code), s.now())
	if err == nil && preview.Join != nil {
		err = s.previewGuestJoin(ctx, preview.Join, addr)
	}
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		writeInviteInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case err != nil:
		slog.ErrorContext(ctx, "invites: preview failed", "error", err)
		writeInternalError(w)
	default:
		// Not a secret, but it answers a secret: no cache keeps it.
		writeSecretJSON(w, http.StatusOK, preview)
	}
}

// previewGuestJoin applies to an invite preview's join what only the server
// knows (issue #710): with authentication off no visit can start (design
// §7.4), and the per-address cap counts the visits this caller's address
// already holds. The store has applied the switch, the mode and the
// server-wide cap.
func (s *Server) previewGuestJoin(ctx context.Context, join *schema.JoinOptionsV1, addr netip.Addr) error {
	if !s.cfg.authRequired() {
		join.Guest, join.GuestsFull = false, false
		return nil
	}
	if !join.Guest {
		return nil
	}
	full, err := s.store.AddressCapReached(ctx, s.guestAddrs.peers(limitKey(addr)))
	if err != nil {
		return err
	}
	if full {
		join.Guest, join.GuestsFull = false, true
	}
	return nil
}

// handleRedeemInvite serves POST /v1/invites/redeem, exempt from
// authentication. It made an account with no password; ADR-009 ends that
// (design §7.6 B1, issue #713): every request is now answered 409
// password_required and makes nothing. It is answered before the body is
// read, with no rate-limit token spent and no store asked, so the route says
// nothing about any code or name, and a client that predates passwords is
// told by the code that it needs one (POST /v2/invites/redeem, as the code's
// comment in pkg/schema says) rather than shown a refusal that looks like a
// bad invite.
func (s *Server) handleRedeemInvite(w http.ResponseWriter, _ *http.Request) {
	// The message names no route: it carries no digit, so it can never be
	// read as an id (the isolation check holds every refusal to that), and
	// the code is what a client acts on.
	writeError(w, http.StatusConflict, schema.ErrorCodePasswordRequired,
		"this route makes no account; an invite is now redeemed with a name and a password")
}

// handleRedeemInviteV2 serves POST /v2/invites/redeem, exempt from
// authentication: it uses the invite to make a new human member with the
// password it chose, and returns its first credential (issue #707, design
// §3.1). The order is part of the contract: the transport check
// (passwordRoute); the per-address limit before the body is read; a
// malformed code the same 404 as any invalid one; a bad name or password a
// 400 that says nothing about the code; the code checked before any hashing,
// so an invalid one costs no Argon2 work, and a valid one that admits guests
// only answered 403 joining_closed then too; the name-probe allowance; the hash,
// in the anonymous pool and outside any transaction; the server-wide limit
// last, so only a redemption that reaches the store can spend it, and one
// that fails there gives it back. The store re-checks the invite and the
// name and stores the password with the account, or nothing.
func (s *Server) handleRedeemInviteV2(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.RedeemInviteRequestV2
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if schema.ValidateInviteCode(req.Code) != nil {
		writeInviteInvalid(w)
		return
	}
	// The schema's messages name the rule broken, never the name or the
	// password sent.
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	password, err := normalizePassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if checkPasswordPolicy(password, req.Name) != nil {
		writeError(w, http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable,
			"that password is too common or contains your name; choose another")
		return
	}
	codeHash := store.HashInviteCode(req.Code)
	switch err := s.store.CheckInviteForAccount(ctx, codeHash, s.now()); {
	case errors.Is(err, store.ErrInviteInvalid):
		s.auditRedeemRefused(r, schema.ErrorCodeInviteInvalid)
		writeInviteInvalid(w)
		return
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
		return
	case errors.Is(err, store.ErrJoiningClosed):
		writeJoiningClosed(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "invites: redeem check failed", "error", err)
		writeInternalError(w)
		return
	}
	if limit, ok, retry := s.logins.peekNameProbe(addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	record, err := s.passwords.hashIn(ctx, s.passwords.anonymous, password)
	if err != nil {
		if !errors.Is(err, errHashPoolBusy) && ctx.Err() == nil {
			slog.ErrorContext(ctx, "invites: hash the password failed", "error", err)
			writeInternalError(w)
			return
		}
		writePasswordBusy(w)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	if s.beforePasswordWrite != nil {
		s.beforePasswordWrite()
	}
	res, err := s.store.RedeemInviteWithPassword(ctx, codeHash, req.Name, record, s.now())
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	if errors.Is(err, store.ErrDuplicate) {
		// ErrNameReserved is a duplicate as well: a name probe either way.
		s.logins.spendNameProbe(addr, s.now())
	}
	s.writeRedeemed(w, r, res, err)
}

// writeJoiningClosed answers an account asked of an invite that admits guests
// only. Only a valid code gets it: an invalid one is invite_invalid first.
func writeJoiningClosed(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeJoiningClosed, "this invite admits guests only; it cannot make an account")
}

// writeRedeemed answers a redemption, v1 or v2, with what the store made of
// it.
func (s *Server) writeRedeemed(w http.ResponseWriter, r *http.Request, res store.RedeemedInvite, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, store.ErrJoiningClosed):
		writeJoiningClosed(w)
	case errors.Is(err, store.ErrInviteInvalid):
		s.auditRedeemRefused(r, schema.ErrorCodeInviteInvalid)
		writeInviteInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case errors.Is(err, store.ErrNameReserved):
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, nameReservedMessageFor(err))
	case errors.Is(err, store.ErrDuplicate):
		// The store found the name taken only after the code proved good, and
		// rolled back: the invite is not used up.
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, "that name is taken; choose another")
	case errors.Is(err, store.ErrInviteRequestInvalid), errors.Is(err, store.ErrNameInvalid):
		// ErrNameInvalid: a name of nothing a reader can see (issue #441),
		// found, like a taken one, after the code proved good.
		writeError(w, http.StatusBadRequest, "invalid_request", "the name is not acceptable")
	case err != nil:
		// The store's errors carry no code, hash or token; the only one that
		// names the name is ErrDuplicate, answered above.
		slog.ErrorContext(ctx, "invites: redeem failed", "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "invites: redeemed", "principal", res.Principal.ID, "invite", res.Principal.InviteID)
		writeSecretJSON(w, http.StatusCreated, schema.RedeemInviteResponseV1{
			Principal:  principalFromStore(res.Principal),
			Credential: res.Credential,
			Token:      res.Token,
		})
	}
}

// Operator administration of invites (issue #438). Every route below is
// operator-only in the route table, so a member or an agent is refused and
// audited before a handler runs. Each handler still starts with inviteCaller:
// with authentication off there is no operator to act, and the answer is the
// 401 the other invite routes give.

// handleInviteJoiners serves GET /v1/invites/{id}/principals: everyone who
// joined through the invite, by id, enabled or not.
func (s *Server) handleInviteJoiners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.inviteCaller(w, r); !ok {
		return
	}
	id, ok := pathID(w, r, "id", "invite")
	if !ok {
		return
	}
	joiners, err := s.store.InviteJoiners(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "invites: joiners failed", "invite", id, "error", err)
		writeInternalError(w)
		return
	}
	out := make([]schema.InviteJoinerV1, 0, len(joiners))
	for _, p := range joiners {
		out = append(out, schema.InviteJoinerV1{
			ID:        p.ID,
			Name:      p.Name,
			CreatedAt: schema.NewTimestamp(p.CreatedAt),
			Disabled:  p.DisabledAt != nil,
		})
	}
	writeJSON(w, http.StatusOK, schema.InviteJoinersResponseV1{Joiners: out})
}

// handleDisableJoiners serves POST /v1/invites/{id}/disable-joiners: it
// disables every enabled principal that joined through the invite, each
// through Store.DisablePrincipal and principalDisabled, as the single disable
// route does, and by default closes the invite first, so nobody joins through
// it while the rest is done. A dry run changes nothing and writes nothing: it
// answers with whom it would disable. Operators and the caller are never
// disabled by it.
func (s *Server) handleDisableJoiners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", "invite")
	if !ok {
		return
	}
	var req schema.DisableJoinersRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	closeIt := req.EffectiveClose()
	if !req.DryRun && closeIt {
		if _, err := s.store.CloseInvite(ctx, auditActor(ctx), caller.ID, id, s.now()); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
				return
			}
			slog.ErrorContext(ctx, "invites: close for disable-joiners failed", "invite", id, "error", err)
			writeInternalError(w)
			return
		}
	}
	joiners, err := s.store.InviteJoiners(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "invites: joiners failed", "invite", id, "error", err)
		writeInternalError(w)
		return
	}
	targets := bulkDisableTargets(caller, joiners)
	if req.DryRun {
		writeJSON(w, http.StatusOK, schema.DisableJoinersResponseV1{DryRun: true, Count: len(targets), PrincipalIDs: targets})
		return
	}
	done, err := s.disablePrincipals(ctx, targets)
	detail := fmt.Sprintf("invite=%d count=%d closed=%t", id, len(done), closeIt)
	s.finishBulkDisable(w, r, err, store.AuditJoinersDisabled, fmt.Sprintf("invite:%d", id), detail, done)
}

// handleDisableInvitees serves POST /v1/principals/{id}/disable-invitees: it
// disables the people the principal invited (depth "1", the default) or
// everyone under them (depth "all"), the same way as disable-joiners. The
// principal themselves is left alone; POST /v1/principals/{id}/disable is the
// route for them, and it never follows the tree.
func (s *Server) handleDisableInvitees(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	var req schema.DisableInviteesRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	depth := 1
	if req.EffectiveDepth() == schema.InviteeDepthAll {
		depth = 0
	}
	invitees, err := s.store.Invitees(ctx, id, depth)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "invites: invitees failed", "principal", id, "error", err)
		writeInternalError(w)
		return
	}
	targets := bulkDisableTargets(caller, invitees)
	if req.DryRun {
		writeJSON(w, http.StatusOK, schema.DisableJoinersResponseV1{DryRun: true, Count: len(targets), PrincipalIDs: targets})
		return
	}
	done, err := s.disablePrincipals(ctx, targets)
	detail := fmt.Sprintf("root=%d depth=%s count=%d", id, req.EffectiveDepth(), len(done))
	s.finishBulkDisable(w, r, err, store.AuditInviteesDisabled, fmt.Sprintf("principal:%d", id), detail, done)
}

// bulkDisableTargets is whom a bulk disable acts on, in order: the enabled
// principals among found, never an operator and never the caller. Never nil.
func bulkDisableTargets(caller store.Principal, found []store.Principal) []int64 {
	out := []int64{}
	for _, p := range found {
		if p.DisabledAt != nil || p.Role == store.RoleOperator || p.ID == caller.ID {
			continue
		}
		out = append(out, p.ID)
	}
	return out
}

// disablePrincipals disables each of ids in turn, exactly as the single
// disable route does: Store.DisablePrincipal (its own transaction and its own
// principal_disabled row), then principalDisabled. It returns the ids it
// changed. A principal that is gone, or whom the last-operator guard keeps,
// is skipped; neither should be among the targets. It stops at the first
// other error and returns what it did before it.
//
// It does not stop when the operator's client goes away: a leak response cut
// short by a dropped connection would leave the rest enabled, unseen.
func (s *Server) disablePrincipals(ctx context.Context, ids []int64) ([]int64, error) {
	ctx = context.WithoutCancel(ctx)
	actor := auditActor(ctx)
	done := []int64{}
	for _, id := range ids {
		changed, err := s.store.DisablePrincipal(ctx, actor, id)
		switch {
		case errors.Is(err, store.ErrPrincipalNotFound), errors.Is(err, store.ErrLastOperator):
			continue
		case err != nil:
			return done, fmt.Errorf("disable principal %d: %w", id, err)
		}
		s.principalDisabled(ctx, id)
		if changed {
			done = append(done, id)
		}
	}
	return done, nil
}

// finishBulkDisable writes the summary audit row of a bulk disable, for what
// was done even when it stopped early, and answers. A failure of either is a
// 500: the principals already disabled stay disabled, and each has its own
// audit rows.
func (s *Server) finishBulkDisable(w http.ResponseWriter, r *http.Request, disableErr error, action, subject, detail string, done []int64) {
	// The client may be gone; what was done must still be recorded.
	ctx := context.WithoutCancel(r.Context())
	_, auditErr := s.store.AppendAuditEvent(ctx, auditActor(ctx), action, subject, detail)
	if disableErr != nil || auditErr != nil {
		slog.ErrorContext(ctx, "invites: bulk disable failed", "action", action, "subject", subject, "disabled", len(done),
			"error", errors.Join(disableErr, auditErr))
		writeInternalError(w)
		return
	}
	slog.InfoContext(ctx, "invites: bulk disable", "action", action, "subject", subject, "disabled", len(done))
	writeJSON(w, http.StatusOK, schema.DisableJoinersResponseV1{Count: len(done), PrincipalIDs: done})
}

// handleInvitePermission serves PUT /v1/principals/{id}/invite-permission:
// an operator grants or takes away a person's right to make personal invites.
// Only an operator may: the store does not check who acts (issue #457), so
// the route table's operator class is what keeps everyone else out. Taking
// the right away closes the person's open personal invites in the same
// transaction.
func (s *Server) handleInvitePermission(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.inviteCaller(w, r); !ok {
		return
	}
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	var req schema.InvitePermissionRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	_, err := s.store.SetMayInvite(ctx, auditActor(ctx), id, req.MayInvite)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
	case errors.Is(err, store.ErrInviteNotAllowed):
		writeError(w, http.StatusBadRequest, "invalid_request", "only a person may hold the right to invite")
	case errors.Is(err, store.ErrGuestNotAllowed):
		writeError(w, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed, "a guest cannot be given the right to invite")
	case err != nil:
		slog.ErrorContext(ctx, "invites: set invite permission failed", "principal", id, "error", err)
		writeInternalError(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGetInviteSettings serves GET /v1/invites/settings: whether invites
// are paused.
func (s *Server) handleGetInviteSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.inviteCaller(w, r); !ok {
		return
	}
	paused, err := s.store.InvitesPaused(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "invites: read settings failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.InviteSettingsV1{Paused: paused})
}

// handlePutInviteSettings serves PUT /v1/invites/settings: pause or resume
// every invite. The switch is stored, so it outlives a restart; while it is
// on, making, previewing and redeeming are refused by the store. Setting the
// value it already has changes and audits nothing.
func (s *Server) handlePutInviteSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.inviteCaller(w, r); !ok {
		return
	}
	var req schema.InviteSettingsV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if _, err := s.store.SetInvitesPaused(ctx, auditActor(ctx), req.Paused); err != nil {
		slog.ErrorContext(ctx, "invites: write settings failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.InviteSettingsV1{Paused: req.Paused})
}
