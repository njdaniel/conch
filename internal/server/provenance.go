package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// selfMadeCredentialRoutes are the routes that require a full-scope
// credential a human member or operator made for themselves (issue #2042,
// child of #2013): direct messages and their settings, private calls and the
// per-person stream. act_as_members lets an operator mint a credential
// or a sign-in link for a non-owner. On these routes the access class runs
// first, then provenance refuses such credentials with 403
// own_sign_in_required.
//
// guard wraps every route named here with requireDMCredential, under
// AuthRequired, outside the recent-auth gate, so a credential made by
// another is told to sign in rather than asked for a proof it cannot give.
// TestSelfMadeCredentialRoutes holds each entry to a registered,
// authenticated route that is not previewable (a preview reads as the
// member, which is exactly what these routes must not allow).
//
// The rules for every route that fills it:
//
//   - A route that serves both HTTP and a WebSocket upgrade calls the check
//     (dmCredentialRefusal, or listing it here) before the upgrade, so a
//     refused credential is answered with an HTTP status and never gets a
//     socket.
//   - MCP tools bypass guard: /mcp has its own bearer authentication and
//     never passes through this check. No MCP tool may read or write a
//     person's DMs or calls.
//   - An agent can never pass: ADR-017 confirms agents reach no DM or call.
//     On routes of class accessAuthenticated, requireDMCredential refuses a
//     full-scope non-human caller before checking provenance, with 403
//     forbidden audited agents_use_mcp (ADR-017 Decision 14). An
//     own_sign_in_required audit row can come from an act-as credential or
//     a member's pre-migration credential.
//
// Every route that reads or writes a person's DMs or calls belongs here,
// sockets included.
//
// A route that serves both a person's own data and anything else (one
// socket for channels and DMs, say) cannot be listed whole without
// refusing the rest: it calls dmCredentialRefusal itself before it sends the
// person's own data.
var selfMadeCredentialRoutes = map[string]dmGuestPolicy{
	// The per-person events stream (#2040): DM frames and rings for the
	// caller alone. A ticket-opened socket is judged by its credential's
	// stored provenance (requestCredentialSelfMade).
	"GET /v1/events/person/ws": dmGuestForbidden,
	// Open, resume, list, read one and set the caller's own state of a
	// conversation (#2019). Reads are refused too.
	"POST /v1/dms":                     dmGuestForbidden,
	"GET /v1/dms":                      dmGuestForbidden,
	"GET /v1/dms/{conversation}":       dmGuestForbidden,
	"PUT /v1/dms/{conversation}/state": dmGuestForbidden,
	// Retained DM history and author sends, edits and removals (#2021).
	"GET /v1/dms/{conversation}/messages":              dmGuestForbidden,
	"POST /v1/dms/{conversation}/messages":             dmGuestForbidden,
	"PATCH /v1/dms/{conversation}/messages/{message}":  dmGuestForbidden,
	"DELETE /v1/dms/{conversation}/messages/{message}": dmGuestForbidden,
	// The caller's own contact preference and the operator's server switch (#2235).
	"GET /v1/dms/preferences": dmGuestForbidden,
	"PUT /v1/dms/preferences": dmGuestForbidden,
	"GET /v1/dms/settings":    dmGuestForbidden,
	"PUT /v1/dms/settings":    dmGuestForbidden,
	// DM recovery and the reader's own mark (#2022).
	"GET /v1/dms/snapshot":                  dmGuestForbidden,
	"GET /v1/dms/{conversation}/changes":    dmGuestForbidden,
	"PUT /v1/dms/{conversation}/read-state": dmGuestForbidden,
	// Private calls (ADR-017 Decision 7): invites, history, the reconnect
	// snapshot and actions (#2030), plus media joins (#2032).
	"POST /v1/dms/{conversation}/calls": privateCallGuestForbidden,
	"GET /v1/dms/{conversation}/calls":  privateCallGuestForbidden,
	"GET /v1/calls/current":             privateCallGuestForbidden,
	"POST /v1/calls/{call}/accept":      privateCallGuestForbidden,
	"POST /v1/calls/{call}/decline":     privateCallGuestForbidden,
	"POST /v1/calls/{call}/cancel":      privateCallGuestForbidden,
	"POST /v1/calls/{call}/hangup":      privateCallGuestForbidden,
	"POST /v1/calls/{call}/session":     privateCallGuestForbidden,
}

// dmGuestPolicy preserves the guest response each route used before sharing
// its credential check with whoami.
type dmGuestPolicy int

const (
	dmGuestForbidden dmGuestPolicy = iota
	privateCallGuestForbidden
)

// requireDMCredential enforces the credential decision and writes and audits
// refusals. The handlers check for a missing identity themselves under AuthOff.
func (s *Server) requireDMCredential(next http.Handler, guestPolicy dmGuestPolicy) http.Handler {
	if !s.cfg.authRequired() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		refusal, err := s.dmCredentialRefusal(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "provenance: read credential failed", "error", err)
			writeInternalError(w)
			return
		}
		caller, _ := callerFrom(ctx)
		switch refusal {
		case "":
			next.ServeHTTP(w, r)
		case errUnauthenticated.Code:
			writeUnauthenticated(w)
		case schema.ErrorCodeSetupIncomplete:
			writeSetupScopeRefused(w)
		case errForbidden.Code:
			subject := r.Pattern
			if subject == "" {
				subject = "<unmatched>"
			}
			s.auditAgentDenial(ctx, caller.ID, subject, "", 0, denyAgentOnHumanRoute)
			writeError(w, http.StatusForbidden, errForbidden.Code, "agents cannot use a person's direct messages or calls")
		case schema.ErrorCodeOwnSignInRequired:
			s.refuseCredentialMadeByAnother(w, r, caller)
		case schema.ErrorCodeGuestNotAllowed:
			if guestPolicy == privateCallGuestForbidden {
				w.Header().Set("Cache-Control", "no-store")
				writeError(w, http.StatusForbidden, errForbidden.Code, "private calls require a human member")
			} else {
				s.denyForbidden(w, r, caller, schema.ErrorCodeGuestNotAllowed, "a guest cannot do this")
			}
		default:
			writeInternalError(w)
		}
	})
}

// dmCredentialRefusal is the credential gate shared by whoami, DMs and
// private calls. It returns the refusal code or a store error without writing
// a response or an audit row. Authentication has checked liveness and
// already requires full scope for tickets. A setup bearer can reach whoami.
func (s *Server) dmCredentialRefusal(ctx context.Context) (string, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return errUnauthenticated.Code, nil
	}
	if cred, ok := requestCredentialFrom(ctx); ok && (cred.Scope != schema.CredentialScopeFull || cred.SetupTOTPOnly) {
		return schema.ErrorCodeSetupIncomplete, nil
	}
	if caller.Kind != store.PrincipalHuman {
		return errForbidden.Code, nil
	}
	if _, previewed := previewFrom(ctx); previewed {
		return schema.ErrorCodeOwnSignInRequired, nil
	}
	self, err := s.requestCredentialSelfMade(ctx)
	switch {
	case errors.Is(err, errNoRequestCredential), errors.Is(err, store.ErrCredentialNotFound):
		return errUnauthenticated.Code, nil
	case err != nil:
		return "", err
	case !self:
		return schema.ErrorCodeOwnSignInRequired, nil
	case caller.GuestState != "" && caller.GuestState != schema.GuestStateConverted:
		return schema.ErrorCodeGuestNotAllowed, nil
	}
	return "", nil
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
		"this action requires your own sign-in. Sign in with your password or a device link you made, then try again")
}
