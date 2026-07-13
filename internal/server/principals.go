package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func (s *Server) handleCreatePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req schema.CreatePrincipalRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if req.Kind != schema.PrincipalHuman && req.Kind != schema.PrincipalAgent {
		writeError(w, http.StatusBadRequest, "invalid_request", `kind must be "human" or "agent"`)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name must not be empty")
		return
	}

	principal, err := s.store.CreatePrincipal(ctx, store.PrincipalKind(req.Kind), req.Name)
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, "principal_exists", "a principal with this name already exists")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "principals: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreatePrincipalResponse{Principal: principalFromStore(principal)})
}

func principalFromStore(principal store.Principal) schema.PrincipalV0 {
	return schema.PrincipalV0{
		ID:   principal.ID,
		Kind: schema.PrincipalKind(principal.Kind),
		Name: principal.Name,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: principal.CreatedAt.UTC(),
	}
}
