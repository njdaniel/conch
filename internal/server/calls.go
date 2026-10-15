package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Calls (issue #267). An agent reaches its sessions, runs and interventions
// through two fronts, the REST routes and the MCP tools, and both are thin:
// each builds a sessionCall, does what only it can do (read a path, decode a
// body, pass the manifest's capability gate in its own way), and calls the
// same function, which does everything else in the order the REST handler
// always has. So the same request is allowed or refused alike on both, with
// the same error code, and a rule changed in one place changes for both.

// sessionCall is who is asking, whichever front the call came through.
type sessionCall struct {
	caller store.Principal
	// credentialID is the credential the call authenticated with. Sessions
	// belong to it, so a call without one is refused before it gets here.
	credentialID int64
	// reader is the visibility reader every read is asked with.
	reader store.Reader
	// subject names the call in an access_denied audit row: the route pattern
	// for REST, "mcp:<tool>" for MCP. It is empty for a caller who is not an
	// agent, who has no manifest to be refused by.
	subject string
	// actor is the audit actor of what the call changes.
	actor string
}

// restCall is the sessionCall of a REST request whose caller and credential
// sessionCaller has established.
func restCall(r *http.Request, caller store.Principal, credentialID int64) sessionCall {
	return sessionCall{
		caller:       caller,
		credentialID: credentialID,
		reader:       readerFor(r, apiV2),
		subject:      agentSubjectFor(r, caller),
		actor:        auditActor(r.Context()),
	}
}

// sessionCall is the sessionCall of this MCP tool call. MCP authenticates
// only agents, so the caller is one; the principal is read here, through the
// scope, so a tool body never touches the store. A static --mcp-token
// identity has no credential for a session to belong to, and is refused as
// REST refuses a caller with authentication off.
func (a *agentScope) sessionCall(ctx context.Context) (sessionCall, *schema.Error) {
	if a.identity.credentialID == 0 {
		return sessionCall{}, &schema.Error{Code: schema.ErrorCodeSessionsRequireAuth, Message: "sessions require an issued credential; a static --mcp-token has none"}
	}
	caller, err := a.s.store.PrincipalByID(ctx, a.identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: read caller failed", "principal", a.identity.principalID, "error", err)
		return sessionCall{}, errInternal
	}
	return sessionCall{
		caller:       caller,
		credentialID: a.identity.credentialID,
		reader:       a.reader(),
		subject:      a.subject,
		actor:        fmt.Sprintf("principal:%d", caller.ID),
	}, nil
}

// callError is a refusal both fronts give alike. REST answers with status and
// err, or with the rate limit and its Retry-After; MCP returns err, or the
// rate limit's own error, as the tool's error.
type callError struct {
	status  int
	err     *schema.Error
	limited *rateLimitedError
}

func refuse(status int, code, message string) *callError {
	return &callError{status: status, err: &schema.Error{Code: code, Message: message}}
}

// refuseWith is refuse for an error value the routes already share.
func refuseWith(status int, err *schema.Error) *callError {
	return &callError{status: status, err: err}
}

var (
	callInternal       = refuseWith(http.StatusInternalServerError, errInternal)
	callSessionMissing = refuseWith(http.StatusNotFound, errSessionNotFound)
	callRunMissing     = refuseWith(http.StatusNotFound, errRunNotFound)
	callChannelMissing = refuseWith(http.StatusNotFound, errChannelNotFound)
	callForbidden      = refuseWith(http.StatusForbidden, errForbidden)
	// callUnauthenticated is a credential that stopped being valid during the
	// call. REST answers it as every credential failure, WWW-Authenticate and
	// all (writeUnauthenticated).
	callUnauthenticated = refuseWith(http.StatusUnauthorized, errUnauthenticated)
	callTooManyRuns     = refuse(http.StatusTooManyRequests, schema.ErrorCodeTooManyRuns, "this session already has the most open runs it may have; end one first")
	callTooMuchActivity = refuse(http.StatusTooManyRequests, schema.ErrorCodeTooMuchActivity, "this run already has the most activity entries it may have; start a new run")
	callSessionOffline  = refuse(http.StatusConflict, schema.ErrorCodeSessionOffline, "this session is offline; send a heartbeat first (ending a run is still allowed)")
	callTooManySess     = refuse(http.StatusTooManyRequests, schema.ErrorCodeTooManySessions, "this credential already has the most sessions it may have; end one first")
)

// writeCallError writes a refusal as a REST response.
func writeCallError(w http.ResponseWriter, e *callError) {
	switch {
	case e.limited != nil:
		writeRateLimited(w, e.limited)
		return
	case e.status == http.StatusUnauthorized:
		writeUnauthenticated(w)
		return
	}
	writeError(w, e.status, e.err.Code, e.err.Message)
}

// schemaError is a refusal as an MCP tool error.
func (e *callError) schemaError() *schema.Error {
	if e.limited != nil {
		return e.limited.schemaError()
	}
	return e.err
}
