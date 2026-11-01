package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Member links on the REST API (issue #985, ADR-013, docs/design/
// community-model.md §2.1 and §3.3): the invite family's second version.
// The store (store/invites_v2.go) holds every rule of who may make, see and
// preview one; these handlers map its answers to statuses and codes and add
// what only HTTP has: rate limits, body limits and the client address.
// Redemption is POST /v2/invites/redeem, unchanged (handleRedeemInviteV2).
//
// Nothing here logs, audits or echoes an invite code. The code is in the 201
// body of a create and nowhere else, sent no-store.

// inviteKindRetiredMessage answers POST /v1/invites from a caller without
// manage_members, and a v2 request that names a v1 kind. It carries no digit, so it can never be read as an id (the
// isolation check holds every refusal to that).
const inviteKindRetiredMessage = "this route makes no personal or open invite now; make a member link instead"

// readCreateMemberLink reads the body of POST /v2/invites. A body naming a v1
// kind is answered 409 invite_kind_retired whatever else it carries (a v1
// body names channels, which v2 refuses as an unknown field, and the caller
// should be told the kind is retired rather than that a field is unknown);
// anything else is decoded strictly. It writes the refusal and returns false.
func readCreateMemberLink(w http.ResponseWriter, r *http.Request) (schema.CreateInviteRequestV2, bool) {
	var req schema.CreateInviteRequestV2
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, inviteBodyMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one valid JSON object of the documented shape")
		}
		return req, false
	}
	var peek struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &peek) == nil && schema.InviteKind(peek.Kind).Valid() {
		writeError(w, http.StatusConflict, schema.ErrorCodeInviteKindRetired, inviteKindRetiredMessage)
		return req, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return req, decodeInviteBody(w, r, &req)
}

// handleCreateMemberLink serves POST /v2/invites: the caller makes a member
// link. The code is in the 201 body and nowhere else, sent no-store.
func (s *Server) handleCreateMemberLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	if limit, ok, retry := s.inviteLimits.takeCreate(caller.ID, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, caller.ID, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	req, ok := readCreateMemberLink(w, r)
	if !ok {
		return
	}
	now := s.now()
	// The schema's messages name the rule broken, never the value sent.
	if err := req.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	resp, err := s.store.CreateMemberLink(ctx, auditActor(ctx), caller.ID, req, store.InviteOptions{Allowance: s.cfg.InviteAllowance}, now)
	// Each refusal that rests on a permission names it to a caller whose role
	// could hold it (ADR-014 design §7.4).
	var perm schema.Permission
	var pe *store.PermissionError
	if errors.As(err, &pe) {
		perm = refusedPermission(caller, pe.Permission)
	}
	switch {
	case errors.Is(err, store.ErrInviteRequestInvalid):
		// Validated above; the clock moved past a just-valid expiry.
		writeError(w, http.StatusBadRequest, "invalid_request", "the invite request is not valid")
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case errors.Is(err, store.ErrInviteNotAllowed):
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodeInviteNotAllowed, "you may not make this kind of link", perm)
	case errors.Is(err, store.ErrInviteGrantExceedsInviter):
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodeInviteGrantExceedsInviter, "you may not name a channel you are not in as a landing channel", perm)
	case errors.Is(err, store.ErrInviteAllowanceExceeded):
		s.denyForbidden(w, r, caller, schema.ErrorCodeInviteAllowanceExceeded, "you already have as many open member links as you may")
	case errors.Is(err, store.ErrNotFound):
		// Only a holder of manage_channels gets this far with a channel that
		// does not exist; anyone else is refused before any lookup.
		writeChannelNotFound(w)
	case errors.Is(err, store.ErrPrincipalNotFound):
		// The caller was resolved a moment ago; fail closed.
		writeUnauthenticated(w)
	case err != nil:
		slog.ErrorContext(ctx, "invites: create member link failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "invites: created", "principal", caller.ID, "invite", resp.Invite.ID, "kind", resp.Invite.Kind)
		writeSecretJSON(w, http.StatusCreated, resp)
	}
}

// handleListMemberLinks serves GET /v2/invites, newest first: the caller's own
// links, or every link for a holder of manage_members. It leaves out the
// legacy invites that admitted guests, which GET /v1/invites still shows,
// closed. Without manage_channels a link lists only the channels the caller
// is in and counts the rest in other_channels (ADR-014 design §7.3).
func (s *Server) handleListMemberLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	everyone, ok := s.canOrFail(w, r, caller, schema.PermManageMembers)
	if !ok {
		return
	}
	var createdBy int64
	if !everyone {
		createdBy = caller.ID
	}
	invites, err := s.store.ListMemberLinks(ctx, createdBy)
	if err != nil {
		slog.ErrorContext(ctx, "invites: list member links failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	sight, err := s.channelSightFor(ctx, caller)
	if err != nil {
		slog.ErrorContext(ctx, "invites: read channel sight failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	for i := range invites {
		invites[i] = inviteV2AsSeen(invites[i], sight)
	}
	writeJSON(w, http.StatusOK, schema.ListInvitesResponseV2{Invites: invites})
}

// inviteV2AsSeen is inv with only the channels sight shows, and the rest
// counted in OtherChannels (ADR-014 design §7.3, SF-1).
func inviteV2AsSeen(inv schema.InviteV2, sight channelSight) schema.InviteV2 {
	if sight.all {
		return inv
	}
	filter := func(in []schema.InviteChannelV1) []schema.InviteChannelV1 {
		shown := make([]schema.InviteChannelV1, 0, len(in))
		for _, c := range in {
			if sight.sees(c.ID) {
				shown = append(shown, c)
				continue
			}
			inv.OtherChannels++
		}
		return shown
	}
	inv.Channels = filter(inv.Channels)
	inv.LandingChannels = filter(inv.LandingChannels)
	return inv
}

// handleMemberLinkJoiners serves GET /v2/invites/{id}/joiners: the members
// who came through a link, to its maker or a holder of manage_members.
// Anyone else, a guest or an agent included, gets 404 invite_not_found, the
// same bytes as for an id that does not exist: no refusal that says "not
// yours", and no audit row a caller could tell from the answer. Unlike the
// other invite routes an agent is not told human_required, for that would
// tell it which ids exist.
func (s *Server) handleMemberLinkJoiners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return
	}
	id, ok := pathID(w, r, "id", "invite")
	if !ok {
		return
	}
	joiners, err := s.store.MemberLinkJoiners(ctx, caller.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
	case err != nil:
		slog.ErrorContext(ctx, "invites: member link joiners failed", "invite", id, "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, schema.InviteJoinersResponseV1{Joiners: joiners})
	}
}

// handlePreviewMemberLink serves POST /v2/invites/preview, exempt from
// authentication: what the link would offer, without using it, and nothing
// else (design §3.3: the inviter's name, the server's, the channels as they
// are now, the expiry and what the server's switches offer). While invites
// are paused it answers invites_paused whatever the code, as redeem does.
func (s *Server) handlePreviewMemberLink(w http.ResponseWriter, r *http.Request) {
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
	preview, err := s.store.PreviewMemberLink(ctx, store.HashInviteCode(req.Code), s.now())
	if err == nil {
		err = s.previewGuestJoin(ctx, &preview.Join, addr)
	}
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		writeInviteInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case err != nil:
		slog.ErrorContext(ctx, "invites: preview member link failed", "error", err)
		writeInternalError(w)
	default:
		preview.ServerName = s.serverName()
		// Not a secret, but it answers a secret: no cache keeps it.
		writeSecretJSON(w, http.StatusOK, preview)
	}
}
