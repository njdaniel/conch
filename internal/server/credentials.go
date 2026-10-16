package server

import (
	"context"
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
	var (
		cred  schema.CredentialV1
		token string
		err   error
	)
	if req.RenewLifetimeSeconds != nil {
		// A renewable credential (issue #439). Validate has refused one that
		// also names expires_at.
		lifetime := time.Duration(*req.RenewLifetimeSeconds) * time.Second
		cred, token, err = s.store.CreateRenewableCredential(ctx, auditActor(ctx), principalID, req.Label, lifetime)
	} else {
		var expiresAt *time.Time
		if req.ExpiresAt != nil {
			t := req.ExpiresAt.Time()
			expiresAt = &t
		}
		cred, token, err = s.store.CreateCredential(ctx, auditActor(ctx), principalID, req.Label, expiresAt)
	}
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
	// The old credential is revoked: a voice room it held a session for is
	// rotated before the response is written (issue #161).
	s.voiceCredentialRevoked(ctx)
	// Rotating revokes the old credential, which ends its sessions.
	s.events.notifyRosters()
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
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: revoke failed", "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.credentialRevoked(ctx)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeCurrentCredential serves DELETE /v1/credentials/current (issue
// #439): the caller signs itself out by revoking the credential this very
// request authenticated with. Which credential that is comes from the request
// context, never from the path or a body, so no caller can name another's.
// Any authenticated caller may; the last operator guard still applies, as to
// any revocation.
func (s *Server) handleRevokeCurrentCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := credentialIDFrom(ctx)
	if !ok {
		// AuthOff: no credential made this request, so there is none to
		// revoke. Answered as whoami answers it.
		writeUnauthenticated(w)
		return
	}
	err := s.store.RevokeOwnCredential(ctx, auditActor(ctx), id)
	switch {
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		// ErrCredentialNotFound included: the credential resolved a moment
		// ago, so its absence now is a fault, not the caller's.
		slog.ErrorContext(ctx, "credentials: self revoke failed", "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.credentialRevoked(ctx)
	w.WriteHeader(http.StatusNoContent)
}

// credentialRevoked is what follows the revocation of one credential, by an
// operator or by its holder. A voice room the credential held a session for
// is rotated before the response is written, so a token LiveKit renewed for
// it is useless (issue #161); it cannot fail the request. Revoking ended the
// credential's sessions, so rosters are told. Open sockets on the credential
// close at their next liveness re-check (defaultCredentialRecheckInterval).
func (s *Server) credentialRevoked(ctx context.Context) {
	s.voiceCredentialRevoked(ctx)
	s.events.notifyRosters()
}
