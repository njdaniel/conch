package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Authentication for the REST and WebSocket surface (issue #89, ADR-003).
//
// Under AuthRequired one middleware wraps the whole mux: every route that is
// not explicitly exempt in the route table must present a live bearer
// credential in the Authorization header, and the principal it resolves to is
// carried in the request context. Nothing here ever logs, audits, or echoes a
// token. Under AuthOff none of this runs and handlers behave as they did
// before: no credential is read and no principal is in the context.

// AuthMode selects whether REST and WebSocket requests must authenticate.
type AuthMode string

const (
	// AuthOff leaves REST and WebSocket open, as before issue #89 (default).
	AuthOff AuthMode = "off"
	// AuthRequired requires a bearer credential on every non-exempt route.
	AuthRequired AuthMode = "required"
)

// ParseAuthMode validates a --auth / CONCHD_AUTH value. Anything other than
// "off" or "required" is an error.
func ParseAuthMode(s string) (AuthMode, error) {
	switch m := AuthMode(s); m {
	case AuthOff, AuthRequired:
		return m, nil
	}
	return "", fmt.Errorf("invalid auth mode %q, want %q or %q", s, AuthOff, AuthRequired)
}

// authRequired reports whether cfg demands authentication. It fails closed:
// only the empty value (unset) and "off" disable it, so an unrecognized mode
// that slipped past ParseAuthMode still requires credentials.
func (c Config) authRequired() bool {
	return c.AuthMode != "" && c.AuthMode != AuthOff
}

// access is the authorization class of a route under AuthRequired. The zero
// value is the strictest non-exempt class short of operator, so a route added
// without a decision is never accidentally open.
type access int

const (
	// accessAuthenticated: any valid credential, human or agent, any role.
	accessAuthenticated access = iota
	// accessOperator: valid credential whose principal has the operator role.
	accessOperator
	// accessOperatorOrSelf: authenticated; the handler additionally requires
	// the operator role or that the caller is the principal named in the path.
	accessOperatorOrSelf
	// accessExempt: the middleware does not authenticate the route. Only
	// routes that carry their own authentication (or none by design) may use it.
	accessExempt
)

// route is one entry of the server's route table, the single source of truth
// for both mux registration and the authentication policy.
type route struct {
	pattern string
	access  access
	handler http.Handler
}

type callerKey struct{}

// withCaller returns ctx carrying the authenticated principal.
func withCaller(ctx context.Context, p store.Principal) context.Context {
	return context.WithValue(ctx, callerKey{}, p)
}

// callerFrom returns the authenticated principal, if any. It reports false
// under AuthOff and on exempt routes.
func callerFrom(ctx context.Context) (store.Principal, bool) {
	p, ok := ctx.Value(callerKey{}).(store.Principal)
	return p, ok
}

// auditActor is the audit actor for an administrative write made in ctx: the
// authenticated caller as "principal:<id>", or "system" when there is no
// caller (AuthOff).
func auditActor(ctx context.Context) string {
	if caller, ok := callerFrom(ctx); ok {
		return fmt.Sprintf("principal:%d", caller.ID)
	}
	return "system"
}

func isOperator(p store.Principal) bool { return p.Role == store.RoleOperator }

// writeUnauthenticated writes the single 401 response used for every
// credential failure, so a client cannot tell a missing credential from a
// malformed, unknown, expired or revoked one.
func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="conch"`)
	writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
}

// bearerToken extracts the token from an Authorization header of the form
// "Bearer <token>". It is the only credential transport: query parameters and
// cookies are never consulted.
func bearerToken(r *http.Request) (string, bool) {
	const scheme = "bearer "
	raw := r.Header.Get("Authorization")
	if len(raw) <= len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) {
		return "", false
	}
	return raw[len(scheme):], true
}

// authMiddleware authenticates every request that is not routed to an exempt
// route. The mux is consulted for the matched pattern, using the same matching
// the mux itself applies on dispatch, so the exemption cannot diverge from the
// handler that actually runs. Unmatched requests (404/405) are not exempt.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		if rt, ok := s.routeByPattern[pattern]; ok && rt.access == accessExempt {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeUnauthenticated(w)
			return
		}
		principal, err := s.store.ResolveCredential(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrCredentialInvalid):
			writeUnauthenticated(w)
			return
		case err != nil:
			// Fail closed: a broken store never authenticates anyone. The
			// error is wrapped by the store and never contains the token.
			slog.ErrorContext(r.Context(), "auth: resolve credential failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), principal)))
	})
}

// guard returns the handler to register for rt. Under AuthOff it is the bare
// handler, so behaviour is unchanged. Under AuthRequired operator routes are
// wrapped with the role check.
func (s *Server) guard(rt route) http.Handler {
	if !s.cfg.authRequired() || rt.access != accessOperator {
		return rt.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerFrom(r.Context())
		if !ok {
			// The middleware always sets a caller on non-exempt routes; its
			// absence is a wiring bug, which must deny rather than allow.
			writeUnauthenticated(w)
			return
		}
		if !isOperator(caller) {
			s.denyForbidden(w, r, caller, "forbidden", "operator role required")
			return
		}
		rt.handler.ServeHTTP(w, r)
	})
}

// denyForbidden writes a 403 with the given error code and appends an
// access_denied audit event: actor principal:<id>, subject the route pattern,
// detail the error code. The event carries no request content or credential
// material. If the audit write fails the request is still denied.
func (s *Server) denyForbidden(w http.ResponseWriter, r *http.Request, caller store.Principal, code, message string) {
	// The client may already be gone; the denial must still be recorded.
	ctx := context.WithoutCancel(r.Context())
	subject := r.Pattern
	if subject == "" {
		subject = r.Method + " " + r.URL.Path
	}
	if _, err := s.store.AppendAuditEvent(ctx, fmt.Sprintf("principal:%d", caller.ID), "access_denied", subject, code); err != nil {
		slog.ErrorContext(ctx, "auth: audit access_denied failed", "principal", caller.ID, "error", err)
	}
	writeError(w, http.StatusForbidden, code, message)
}

// bindAuthor enforces author binding on a message post. Without a caller
// (AuthOff, or the exempt webhook ingest route) it does nothing. With one, an
// absent or zero author_id becomes the caller; the caller's own id is accepted;
// anything else is a 403 author_mismatch. It reports whether the request may
// proceed.
func (s *Server) bindAuthor(w http.ResponseWriter, r *http.Request, authorID *int64) bool {
	caller, ok := callerFrom(r.Context())
	if !ok {
		return true
	}
	switch *authorID {
	case 0:
		*authorID = caller.ID
	case caller.ID:
	default:
		s.denyForbidden(w, r, caller, "author_mismatch", "author_id does not match the authenticated principal")
		return false
	}
	return true
}

// handleWhoAmI serves GET /v1/whoami. With no caller (AuthOff) there is no
// identity to report, so it answers 401 like any unauthenticated request.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.WhoAmIResponseV1{
		ID:   caller.ID,
		Kind: schema.PrincipalKind(caller.Kind),
		Name: caller.Name,
		Role: schema.PrincipalRole(caller.Role),
	})
}
