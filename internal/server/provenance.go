package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// selfMadeCredentialRoutes are the routes that refuse a credential its
// principal did not make for themselves (issue #2042, child of #2013;
// Nick's decision 4 of 2026-11-19): a person's direct messages, their calls
// and their per-person stream. act_as_members lets an operator mint a
// credential or a sign-in link for any member who is not an owner; on these
// routes such a credential gets 403 own_sign_in_required, so the operator
// can still sign in and post as the member but cannot read their DMs or
// join their calls. Every other route is unchanged.
//
// guard wraps every route named here with requireSelfMadeCredential, under
// AuthRequired, outside the recent-auth gate, so a credential made by
// another is told to sign in rather than asked for a proof it cannot give.
// TestSelfMadeCredentialRoutes holds each entry to a registered,
// authenticated route that is not previewable (a preview reads as the
// member, which is exactly what these routes must not allow).
//
// The rules for every route that fills it:
//
//   - A route that serves both HTTP and a WebSocket upgrade calls the check
//     (selfMadeCredential, or listing it here) before the upgrade, so a
//     refused credential is answered with an HTTP status and never gets a
//     socket.
//   - MCP tools bypass guard: /mcp has its own bearer authentication and
//     never passes through this check. No MCP tool may read or write a
//     person's DMs or calls.
//   - An agent can never pass: ADR-017 confirms agents reach no DM or call,
//     and selfMadeCredential refuses a non-human caller first, with 403
//     forbidden audited agents_use_mcp, as every human-only route does and
//     as the DM schema's refusal order says (pkg/schema/dm.go; ADR-017
//     Decision 14). An own_sign_in_required audit row can come from an
//     act-as credential or a member's pre-migration credential.
//
// The per-person stream (#2040), conversation routes (#2019) and private
// call routes (#2030) are registered here. Every route that reads or writes
// a person's DMs or calls belongs here, sockets included.
//
// A route that serves both a person's own data and anything else (one
// socket for channels and DMs, say) cannot be listed whole without
// refusing the rest: it calls selfMadeCredential itself before it sends the
// person's own data.
var selfMadeCredentialRoutes = map[string]bool{
	// The per-person events stream (#2040): DM frames and rings for the
	// caller alone. A ticket-opened socket is judged by its credential's
	// stored provenance (requestCredentialSelfMade).
	"GET /v1/events/person/ws": true,
	// Open, resume, list, read one and set the caller's own state of a
	// conversation (#2019). Reads are refused too.
	"POST /v1/dms":                     true,
	"GET /v1/dms":                      true,
	"GET /v1/dms/{conversation}":       true,
	"PUT /v1/dms/{conversation}/state": true,
	// The seven private call routes (#2030, ADR-017 Decision 7): an invite,
	// the history of a pair, the reconnect snapshot and the four actions.
	// Their handlers rely on this guard. The media-join route (#2032)
	// requires the same credential provenance.
	"POST /v1/dms/{conversation}/calls": true,
	"GET /v1/dms/{conversation}/calls":  true,
	"GET /v1/calls/current":             true,
	"POST /v1/calls/{call}/accept":      true,
	"POST /v1/calls/{call}/decline":     true,
	"POST /v1/calls/{call}/cancel":      true,
	"POST /v1/calls/{call}/hangup":      true,
	"POST /v1/calls/{call}/session":     true,
}

// requireSelfMadeCredential wraps next so it runs only for a request whose
// credential is self-made (selfMadeCredential); any other gets the answer
// selfMadeCredential writes.
func (s *Server) requireSelfMadeCredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.selfMadeCredential(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// selfMadeCredential reports whether the request's credential was made by
// its own principal (store.RequestCredential.SelfMade, provenance.go in the
// store). When it was not, it has written the answer and the caller writes
// nothing more. The order of refusals: a non-human caller first, 403
// forbidden audited as agents_use_mcp (the agent is told nothing about
// signing in, which is wording for people); then 403
// own_sign_in_required, audited as a denial, for a person's credential made
// by another; 401 when the request has no credential at
// all, which under AuthRequired is a wiring bug and denies; 500 when the
// store cannot say. A ticket-authenticated socket has its credential's id
// but not the rest, so the provenance is read from the store by that id.
//
// Under AuthOff there are no credentials and every request is the system
// actor, as for every other check guard makes: it reports true.
//
// A previewed request (Conch-Preview-As) is refused whatever the
// credential: the administrator would be reading as the member.
func (s *Server) selfMadeCredential(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.authRequired() {
		return true
	}
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return false
	}
	if caller.Kind != store.PrincipalHuman {
		subject := r.Pattern
		if subject == "" {
			subject = "<unmatched>"
		}
		s.auditAgentDenial(ctx, caller.ID, subject, "", 0, denyAgentOnHumanRoute)
		writeError(w, http.StatusForbidden, errForbidden.Code, "agents cannot use a person's direct messages or calls")
		return false
	}
	if _, previewed := previewFrom(ctx); previewed {
		s.refuseCredentialMadeByAnother(w, r, caller)
		return false
	}
	self, err := s.requestCredentialSelfMade(ctx)
	switch {
	case errors.Is(err, errNoRequestCredential), errors.Is(err, store.ErrCredentialNotFound):
		// No credential under AuthRequired is never the middleware's doing:
		// deny rather than allow.
		writeUnauthenticated(w)
		return false
	case err != nil:
		slog.ErrorContext(ctx, "provenance: read credential failed", "error", err)
		writeInternalError(w)
		return false
	}
	if !self {
		s.refuseCredentialMadeByAnother(w, r, caller)
		return false
	}
	return true
}

// errNoRequestCredential is a request that carries no credential.
var errNoRequestCredential = errors.New("server: the request has no credential")

// requestCredentialSelfMade reports whether the credential ctx's request came
// with was made by its own principal. The middleware leaves the whole
// credential in ctx for a bearer request; a socket opened with a ticket has
// only its id, and the provenance is read from the store by it. A request
// with no credential is errNoRequestCredential, a credential that no longer
// exists store.ErrCredentialNotFound.
func (s *Server) requestCredentialSelfMade(ctx context.Context) (bool, error) {
	if cred, ok := requestCredentialFrom(ctx); ok {
		return cred.SelfMade, nil
	}
	if id, ok := credentialIDFrom(ctx); ok {
		return s.store.CredentialSelfMade(ctx, id)
	}
	return false, errNoRequestCredential
}

// refuseCredentialMadeByAnother writes 403 own_sign_in_required, audited
// as every refusal of an authorization check is.
func (s *Server) refuseCredentialMadeByAnother(w http.ResponseWriter, r *http.Request, caller store.Principal) {
	s.denyForbidden(w, r, caller, schema.ErrorCodeOwnSignInRequired,
		"this is a member's own conversation or call, and this sign-in was made by someone else (an operator's credential or sign-in link, or one older than this check): sign in yourself, with your password or a device link you made, and try again")
}
