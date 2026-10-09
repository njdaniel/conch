package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Credential administration (issue #78). The plaintext token appears in
// exactly one place: the body of a successful create or rotate response.
// Nothing here logs a token, and no error body or log line is built from one.

// pathID parses the named path value as a positive integer, writing a 400 and
// returning false when it is not.
func pathID(w http.ResponseWriter, r *http.Request, name, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", what+" id must be a positive integer")
		return 0, false
	}
	return id, true
}

// writeSecretJSON writes a response that carries a token: it must never be
// cached by the client or an intermediary.
func writeSecretJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, body)
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	var req schema.CreateCredentialRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if err := req.Validate(time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t := req.ExpiresAt.Time()
		expiresAt = &t
	}

	cred, token, err := s.store.CreateCredential(ctx, auditActor(ctx), principalID, req.Label, expiresAt)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrPrincipalDisabled):
		writeError(w, http.StatusConflict, "principal_disabled", "principal is disabled")
		return
	case errors.Is(err, store.ErrCredentialExpiryPast):
		writeError(w, http.StatusBadRequest, "invalid_request", "credential expires_at must be in the future")
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: create failed", "principal", principalID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeSecretJSON(w, http.StatusCreated, schema.CreateCredentialResponseV1{Credential: cred, Token: token})
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	creds, err := s.store.ListCredentials(ctx, principalID)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: list failed", "principal", principalID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, schema.ListCredentialsResponseV1{Credentials: creds})
}

func (s *Server) handleRotateCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "credential_id", "credential")
	if !ok {
		return
	}
	cred, token, err := s.store.RotateCredential(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "credential_not_found", "credential not found")
		return
	case errors.Is(err, store.ErrPrincipalDisabled):
		writeError(w, http.StatusConflict, "principal_disabled", "principal is disabled")
		return
	case errors.Is(err, store.ErrCredentialRevoked):
		writeError(w, http.StatusConflict, "credential_revoked", "credential is revoked")
		return
	case errors.Is(err, store.ErrCredentialExpired):
		writeError(w, http.StatusBadRequest, "invalid_request", "credential has expired; create a new credential with a new expiry instead of rotating")
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: rotate failed", "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeSecretJSON(w, http.StatusCreated, schema.RotateCredentialResponseV1{Credential: cred, Token: token})
}

func (s *Server) handleRevokeCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "credential_id", "credential")
	if !ok {
		return
	}
	err := s.store.RevokeCredential(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "credential_not_found", "credential not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: revoke failed", "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
