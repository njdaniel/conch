package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// handleVoicePermission serves PUT /v1/principals/{id}/voice-permission
// (ADR-013 decision 4, issue #984): a holder of manage_members switches a
// person's voice off or on at any time, with immediate effect on live media.
// The route table gives the access class (manage_members) and the
// recent-authentication rule; owner protection (ADR-010) comes before any
// lookup, and the store re-checks it in the write's transaction.
//
// Off is the brake under the role's use_voice (ADR-014 design §5). After the
// commit, when the person no longer holds use_voice, the poller's immediate
// hook removes them from every room by name with their companion and viewer,
// and a share they hold goes with them. It runs before the answer and never
// fails the request; a refusal from LiveKit is left to the next pass. No room
// is rotated. It also runs on a repeat of "off" that changed nothing, so a
// removal a crash cut short is finished. Turning voice on changes nothing
// live: the person joins again.
func (s *Server) handleVoicePermission(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// With authentication off there is no caller and the server is open to
	// everyone, as for every administration route; the route table has
	// already refused an unauthenticated request when it is required.
	caller, authed := callerFrom(ctx)
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	if s.ownerProtected(w, r, id) {
		return
	}
	// The field is read as present or absent: a body without it must not be
	// taken for "off" (or for anything), so it is refused.
	var req struct {
		Allowed *bool `json:"voice_allowed"`
	}
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Allowed == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "voice_allowed is required")
		return
	}
	allowed := *req.Allowed
	changed, err := s.store.SetVoiceAllowed(ctx, auditActor(ctx), id, allowed, authed && !isOwner(caller))
	switch {
	case errors.Is(err, store.ErrOwnerProtected):
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrNotAPerson):
		writeError(w, http.StatusConflict, schema.ErrorCodeNotAPerson, "agents do not use voice")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: set voice permission failed", "principal", id, "error", err)
		writeInternalError(w)
		return
	}
	if changed {
		// A brake changes one person's permissions: they alone are told
		// (ADR-014 design §8.6).
		s.events.notifyPermissionsChanged(id)
	}
	if !allowed {
		s.voiceAfterVoiceOff(ctx, id)
	}
	writeJSON(w, http.StatusOK, schema.VoicePermissionResponseV1{PrincipalID: id, VoiceAllowed: allowed, Changed: changed})
}

// voiceAfterVoiceOff runs the poller's immediate hook for a person whose
// voice was switched off, when they no longer hold use_voice. An owner is not
// bound by the brake (design §5) and stays; a failed read of the right fails
// closed and removes them, and the next pass judges again.
func (s *Server) voiceAfterVoiceOff(ctx context.Context, id int64) {
	holds, err := s.store.Permitted(context.WithoutCancel(ctx), id, schema.PermUseVoice)
	if err != nil {
		slog.ErrorContext(ctx, "voice: read use_voice after switch-off failed; removing from voice", "principal", id, "error", err)
	} else if holds {
		return
	}
	s.voicePrincipalLostAccess(ctx, id, voiceReasonVoiceOff)
}
