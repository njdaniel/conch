package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// handleSetRole serves PUT /v1/principals/{id}/role (owner only, recent
// authentication required; issue #790, ADR-010 §6).
func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	caller, authed := callerFrom(ctx)
	if authed {
		target, err := s.store.PrincipalByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "principals: find role target failed", "principal", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if target.HiddenAt != nil {
			writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
			return
		}
	}
	var req schema.RoleChangeRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !authed {
		writeUnauthenticated(w)
		return
	}
	// Whether the target held use_voice before the change; read before it
	// commits. A failed read counts as held, so the check after runs.
	heldVoice, err := s.store.Permitted(ctx, id, schema.PermUseVoice)
	if err != nil {
		heldVoice = true
	}
	res, err := s.store.SetRole(ctx, auditActor(ctx), caller.ID, id, store.Role(req.Role), req.Owner)
	switch {
	case errors.Is(err, store.ErrNotOwner):
		s.denyForbidden(w, r, caller, "forbidden", "owner required")
		return
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrRoleNotAllowed):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: set role failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if res.CredentialsRevoked > 0 {
		// Setting the owner mark revoked the target's live credentials; close
		// their open message, events and voice sockets as handleRevokeAllCredentials
		// does (disable.go).
		s.hub.DropPrincipalAll(id)
		s.events.dropPrincipalAll(id)
		s.events.notifyRosters()
		s.voicePrincipalLostAccess(ctx, id, voiceReasonCredsRevoked)
	} else if res.Changed {
		s.events.notifyRosters()
	}
	if res.Changed {
		// A role is a column of the permissions document: the target's
		// permissions may have changed, and a share it may no longer hold
		// ends now, since the voice passes read the person's brake and not
		// the document (the security review of #1061, S1). The credential
		// revocation above returns before any share is ended.
		s.events.notifyPermissionsChanged(id)
		if heldVoice {
			// use_voice may have gone with the role: remove the target from
			// voice now, as PUT /v1/permissions does (#1016), not at the
			// poller's next pass. A no-op if they still hold it.
			s.voiceWithheldFor(ctx, id)
		}
		s.endShareIfWithheld(ctx, id)
	}
	writeJSON(w, http.StatusOK, res.RoleChangeResponseV1)
}
