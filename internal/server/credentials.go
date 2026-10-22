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
	case errors.Is(err, store.ErrGuestNotAllowed):
		writeGuestCredentialRefused(w)
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
	case errors.Is(err, store.ErrGuestNotAllowed):
		writeGuestCredentialRefused(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: rotate failed", "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// Rotating revokes the old credential: what follows is what follows a
	// revoke.
	s.credentialRevoked(ctx, id)
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
	s.credentialRevoked(ctx, id)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeCurrentCredential serves DELETE /v1/credentials/current (issue
// #439): the caller signs itself out by revoking the credential this very
// request authenticated with. Which credential that is comes from the request
// context, never from the path or a body, so no caller can name another's.
// Any authenticated caller may; the last operator guard still applies, as to
// any revocation. A member's open device links close with it (issue #712,
// store.RevokeOwnCredential), so a sign-out leaves no way back in behind it.
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
	s.credentialRevoked(ctx, id)
	w.WriteHeader(http.StatusNoContent)
}

// handleListOwnCredentials serves GET /v1/credentials/mine (issue #496): the
// caller's own sign-ins, the credentials of theirs that can still sign in,
// with the one this request came with marked current. Whose they are comes
// from the request context, never from the path or a query, so no caller can
// list another's. Any authenticated caller may, a person or an agent.
func (s *Server) handleListOwnCredentials(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	currentID, hasCred := credentialIDFrom(ctx)
	if !ok || !hasCred {
		// AuthOff: nobody asked, so there is nobody's to list. Answered as
		// whoami answers it.
		writeUnauthenticated(w)
		return
	}
	creds, err := s.store.ListLiveCredentials(ctx, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "credentials: own list failed", "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	out := make([]schema.OwnCredentialV1, 0, len(creds))
	for _, c := range creds {
		out = append(out, schema.OwnCredentialV1{
			ID:        c.ID,
			Label:     c.Label,
			CreatedAt: c.CreatedAt,
			ExpiresAt: c.ExpiresAt,
			Renews:    c.Renews,
			Current:   c.ID == currentID,
		})
	}
	writeJSON(w, http.StatusOK, schema.ListOwnCredentialsResponseV1{Credentials: out})
}

// handleRevokeOwnCredential serves DELETE /v1/credentials/mine/{id} (issue
// #496): the caller ends one of their own sign-ins by its id, this one or
// another. The store revokes it only if it is the caller's: another's id is
// answered exactly as an id never issued, 404 credential_not_found, so the
// answer says nothing of whether it exists. Audited credential_revoked with
// reason=self, as signing out is; the last operator guard applies.
func (s *Server) handleRevokeOwnCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	// A caller who did not sign in with a credential has no sign-ins of
	// their own to end; checked as the list does, so this stays closed
	// however AuthOff is answered.
	if _, hasCred := credentialIDFrom(ctx); !ok || !hasCred {
		writeUnauthenticated(w)
		return
	}
	id, ok := pathID(w, r, "id", "credential")
	if !ok {
		return
	}
	err := s.store.RevokeCredentialOf(ctx, auditActor(ctx), caller.ID, id)
	switch {
	case errors.Is(err, store.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "credential_not_found", "credential not found")
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: own revoke failed", "principal", caller.ID, "credential", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.credentialRevoked(ctx, id)
	w.WriteHeader(http.StatusNoContent)
}

// writeGuestCredentialRefused is the 403 for a credential made or rotated for
// a guest (issue #705): a guest has exactly the one credential of its visit.
func writeGuestCredentialRefused(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed, "a guest has only the credential of its visit")
}

// credentialRevoked is what follows the committed revocation of credential id,
// by an operator or by its holder, or by a rotation that replaced it. Every
// socket opened with it closes at once, in every family, with the revoked
// reason (issue #519): the drops come first, before anything else here can
// owe one of them a frame, and a frame already being built is stopped before
// it is written. The principal's sockets through its other credentials stay.
// A voice room the credential held a session for is rotated before the
// response is written, so a token LiveKit renewed for it is useless (issue
// #161); it cannot fail the request. Revoking ended the credential's
// sessions, so rosters are told.
func (s *Server) credentialRevoked(ctx context.Context, id int64) {
	s.hub.DropCredential(id)
	s.events.dropCredential(id)
	s.voiceCredentialRevoked(ctx, id)
	s.events.notifyRosters()
}
