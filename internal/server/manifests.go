package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// manifestPrincipalID parses the {id} path value, writing a 400 and returning
// false when it is not a positive integer.
func manifestPrincipalID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "principal id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) handlePutManifest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := manifestPrincipalID(w, r)
	if !ok {
		return
	}
	var req schema.PutAgentManifestRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_manifest", err.Error())
		return
	}

	manifest, created, err := s.store.PutAgentManifest(ctx, auditActor(ctx), id, req)
	var noChannel *store.ChannelNotFoundError
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrPrincipalNotAgent):
		writeError(w, http.StatusConflict, "principal_not_agent", "principal is not an agent")
		return
	case errors.As(err, &noChannel):
		writeError(w, http.StatusBadRequest, "channel_not_found", "channel "+strconv.FormatInt(noChannel.ChannelID, 10)+" not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "manifests: put failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// A WebSocket subscription was authorized against the manifest as it was
	// when the socket opened; close the ones the new manifest no longer
	// permits, or a revoked read grant would keep streaming (issue #79).
	s.dropAgentSubscriptionsRevokedBy(ctx, id, manifest)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, schema.PutAgentManifestResponseV1{Manifest: manifest})
}

func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := manifestPrincipalID(w, r)
	if !ok {
		return
	}
	// Operators and the agent the manifest belongs to only. Checked before any
	// lookup so a forbidden caller cannot probe which manifests exist.
	if caller, authed := callerFrom(ctx); authed && !isOperator(caller) && caller.ID != id {
		s.denyForbidden(w, r, caller, "forbidden", "operator role or the manifest's own agent required")
		return
	}
	manifest, err := s.store.AgentManifestByPrincipal(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "manifest_not_found", "manifest not found")
		return
	}
	if err != nil {
		// Includes a stored row that fails validation: never served (fail closed).
		slog.ErrorContext(ctx, "manifests: read failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, schema.GetAgentManifestResponseV1{Manifest: manifest})
}
