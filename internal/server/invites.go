package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, inviteBodyMaxBytes))
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

// handleListInvites serves GET /v1/invites: the caller's own invites, newest
// first, operators included (seeing everyone's is operator administration,
// issue #438).
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	invites, err := s.store.ListInvites(ctx, store.InviteFilter{CreatedBy: caller.ID}, s.now())
	if err != nil {
		slog.ErrorContext(ctx, "invites: list failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.ListInvitesResponseV1{Invites: invites})
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

// handleRedeemInvite serves POST /v1/invites/redeem, exempt from
// authentication: it uses the invite to make a new human member and returns
// its first credential. The order is part of the contract: the per-address
// limit before the body is read; a malformed code the same 404 as any invalid
// one; a bad name a 400 that says nothing about the code; the server-wide
// limit last, so only a redemption that reaches the store can spend it, and
// one that fails there gives it back.
func (s *Server) handleRedeemInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.RedeemInviteRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if schema.ValidateInviteCode(req.Code) != nil {
		writeInviteInvalid(w)
		return
	}
	// The schema's messages name the rule broken, never the name sent.
	if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: req.Name}).Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	res, err := s.store.RedeemInvite(ctx, store.HashInviteCode(req.Code), req.Name, s.now())
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		s.auditRedeemRefused(r, schema.ErrorCodeInviteInvalid)
		writeInviteInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case errors.Is(err, store.ErrDuplicate):
		// The store found the name taken only after the code proved good, and
		// rolled back: the invite is not used up.
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, "that name is taken; choose another")
	case errors.Is(err, store.ErrInviteRequestInvalid):
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
