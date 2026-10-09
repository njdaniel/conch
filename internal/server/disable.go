package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Disabling a principal and revoking all of its credentials (issue #101). All
// three endpoints are operator-only under AuthRequired (route table) and open
// with no caller under AuthOff, like the other administrative endpoints.
// Nothing here reads, logs, or audits a token.

// handleDisablePrincipal serves POST /v1/principals/{id}/disable.
//
// The store call sets disabled_at, revokes every live credential, and audits
// it all in one transaction; the principal's open WebSockets are closed after
// it commits. Idempotent: a repeat answers 204 and changes nothing.
//
// Coverage note: stored credentials used at /mcp are rejected because MCP
// resolves them through store.ResolveCredential, which refuses a disabled
// principal. The deprecated static --mcp-token map is configuration, not a
// credential row, so a disabled agent that still has a static mapping keeps
// MCP access until capability enforcement (issue #79) checks it there.
func (s *Server) handleDisablePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	_, err := s.store.DisablePrincipal(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: disable failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// Dropped even when nothing changed: a repeat call is a harmless way to
	// make sure no socket survives.
	s.hub.DropPrincipalAll(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleEnablePrincipal serves POST /v1/principals/{id}/enable. It clears
// disabled_at and revives no credential: new ones must be issued.
func (s *Server) handleEnablePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	_, err := s.store.EnablePrincipal(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: enable failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeAllCredentials serves
// POST /v1/principals/{id}/credentials/revoke-all: the principal stays enabled
// and can be issued new credentials, but every live one is revoked atomically
// and its open WebSockets are closed after the transaction commits.
func (s *Server) handleRevokeAllCredentials(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	n, err := s.store.RevokeAllCredentials(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: revoke-all failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.hub.DropPrincipalAll(id)
	writeJSON(w, http.StatusOK, schema.RevokeAllCredentialsResponseV1{Revoked: n})
}

// lastOperatorMessage explains a 409 last_operator: the action would leave no
// enabled operator holding a live credential.
const lastOperatorMessage = "this would leave no operator able to sign in; give another operator a credential first"
