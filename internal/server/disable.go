package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Disabling a principal and revoking all of its credentials (issue #101).
// Disable and enable are operator-only under AuthRequired (route table);
// revoke-all is for operators or the principal itself (issue #439). All three
// are open with no caller under AuthOff, like the other administrative
// endpoints. Nothing here reads, logs, or audits a token.

// handleDisablePrincipal serves POST /v1/principals/{id}/disable.
//
// The store call sets disabled_at, revokes every live credential, and audits
// it all in one transaction; the principal's open WebSockets are closed after
// it commits. It answers 200 with DisablePrincipalResponseV1: what the
// principal issued to others and is still live, since disabling ends none
// of it (ADR-014 O13, issue #1024), for the owner to revoke. Idempotent: a
// repeat answers 200 with an empty body and changes nothing.
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
	if s.ownerProtected(w, r, id) {
		return
	}
	caller, authed := callerFrom(ctx)
	changed, err := s.store.DisablePrincipal(ctx, auditActor(ctx), id, authed && !isOwner(caller))
	switch {
	case errors.Is(err, store.ErrOwnerProtected):
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
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
	s.principalDisabled(ctx, id)
	var resp schema.DisablePrincipalResponseV1
	if changed {
		s.directoryPrincipalChanged(ctx, id, nil)
		// What the principal issued to others stays live (design §7.5,
		// O13): listed for the owner, as the demotion and the PUT that
		// turns act_as_members off list it. Read after the commit, as
		// the PUT does; the principal is disabled and can issue no more.
		issued, err := s.store.IssuedToOthersBy(ctx, id, s.now())
		if err != nil {
			// The disable is done; the list is a help to the owner, so
			// the answer goes without it and says so in the log.
			slog.ErrorContext(ctx, "principals: list what the disabled principal issued to others failed", "principal", id, "error", err)
		} else if len(issued) > 0 {
			resp.IssuedToOthers = issued
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRevokeIssued serves POST /v1/principals/{id}/issued/revoke-all
// (owner only, recent authentication; ADR-014 O13, issue #1024): the one
// confirmed step that revokes every credential and hook principal {id}
// issued to principals other than itself, what the demotion, disable and
// act_as_members answers listed. An owner as {id} is refused: its issues are
// never listed, and the owner mark is cleared first if they are to end.
// Each revoked credential's sockets close after the commit, as a single
// revoke's do.
func (s *Server) handleRevokeIssued(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	// Under AuthOff there is no caller and the system acts, as on the
	// other administrative routes; under AuthRequired the owner class has
	// already refused anyone else.
	caller, authed := callerFrom(ctx)
	var actorID int64
	if authed {
		actorID = caller.ID
	}
	res, err := s.store.RevokeIssued(ctx, auditActor(ctx), actorID, id)
	switch {
	case errors.Is(err, store.ErrNotOwner):
		s.denyForbidden(w, r, caller, "forbidden", "owner required")
		return
	case errors.Is(err, store.ErrOwnerProtected):
		s.denyForbidden(w, r, caller, "forbidden", "what an owner issued is not revoked this way; clear the owner mark first")
		return
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "credentials: revoke issued failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	for _, credID := range res.Credentials {
		s.credentialRevoked(ctx, credID)
	}
	writeJSON(w, http.StatusOK, schema.RevokeIssuedResponseV1{CredentialsRevoked: res.CredentialsRevoked, HooksRevoked: res.HooksRevoked})
}

// principalDisabled does what follows a committed Store.DisablePrincipal:
// the principal's open sockets close, rosters learn their sessions ended, and
// they leave voice. Every route that disables someone calls it, one principal
// at a time (issue #438).
func (s *Server) principalDisabled(ctx context.Context, id int64) {
	s.hub.DropPrincipalAll(id)
	s.events.dropPrincipalAll(id)
	// Disabling ends the principal's sessions.
	s.events.notifyRosters()
	s.voicePrincipalLostAccess(ctx, id, voiceReasonPrincipalOff)
}

// handleEnablePrincipal serves POST /v1/principals/{id}/enable. It clears
// disabled_at and revives no credential: new ones must be issued.
func (s *Server) handleEnablePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	if s.ownerProtected(w, r, id) {
		return
	}
	caller, authed := callerFrom(ctx)
	changed, err := s.store.EnablePrincipal(ctx, auditActor(ctx), id, authed && !isOwner(caller))
	switch {
	case errors.Is(err, store.ErrOwnerProtected):
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: enable failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if changed {
		s.directoryPrincipalChanged(ctx, id, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeAllCredentials serves
// POST /v1/principals/{id}/credentials/revoke-all: the principal stays enabled
// and can be issued new credentials, but every live one is revoked atomically
// and its open WebSockets are closed after the transaction commits.
//
// An operator may do it to anyone; anyone else only to themselves, which is
// "sign out everywhere" (issue #439). The check comes before any lookup, so a
// refused caller learns nothing of the principal named. The last operator
// guard applies to a self revoke-all as to any other.
func (s *Server) handleRevokeAllCredentials(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	caller, authed := callerFrom(ctx)
	if authed && caller.ID != id {
		// Someone else's needs manage_members (ADR-014 design §7.2).
		allowed, ok := s.canOrFail(w, r, caller, schema.PermManageMembers)
		if !ok {
			return
		}
		if !allowed {
			s.denyForbiddenFor(w, r, caller, "forbidden", "operator role or the principal itself required",
				refusedPermission(caller, schema.PermManageMembers))
			return
		}
	}
	if s.ownerProtected(w, r, id) {
		return
	}
	// The store decides the standing again under its write lock, so a
	// caller that lost manage_members since the check above signs only
	// itself out (ADR-014 design §7.2, ADR-010 D7).
	var actorID int64
	if authed {
		actorID = caller.ID
	}
	n, err := s.store.RevokeAllCredentialsAs(ctx, auditActor(ctx), actorID, id, authed && !isOwner(caller))
	switch {
	case errors.Is(err, store.ErrManageMembersRequired):
		s.denyForbiddenFor(w, r, caller, "forbidden", "operator role or the principal itself required",
			refusedPermission(caller, schema.PermManageMembers))
		return
	case errors.Is(err, store.ErrOwnerProtected):
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
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
	s.events.dropPrincipalAll(id)
	// Revoking every credential ends the principal's sessions.
	s.events.notifyRosters()
	s.voicePrincipalLostAccess(ctx, id, voiceReasonCredsRevoked)
	writeJSON(w, http.StatusOK, schema.RevokeAllCredentialsResponseV1{Revoked: n})
}

// lastOperatorMessage explains a 409 last_operator: the action would leave no
// enabled owner able to sign in (issue #790, ADR-010 §5.5).
const lastOperatorMessage = "this would leave no owner able to sign in; give another owner a credential first"
