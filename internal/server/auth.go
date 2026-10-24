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
	// AuthOff leaves REST and WebSocket open to anyone who can reach the
	// port. It must be chosen explicitly; it is never a default.
	AuthOff AuthMode = "off"
	// AuthRequired requires a bearer credential on every non-exempt route.
	// It is what an unset mode means.
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
// only an explicit "off" disables it, so an unset mode (a zero-value Config)
// and an unrecognized one that slipped past ParseAuthMode both require
// credentials (issue #92).
func (c Config) authRequired() bool {
	return c.AuthMode != AuthOff
}

// access is the authorization class of a route under AuthRequired. The zero
// value is accessAuthenticated, so a route added without a decision still
// requires a credential and is never accidentally open.
type access int

const (
	// accessAuthenticated: any valid credential, human or agent, any role.
	accessAuthenticated access = iota
	// accessOperator: valid credential whose principal has the operator role.
	accessOperator
	// accessOwner: valid credential whose principal is an operator holding the
	// owner mark (issue #790, ADR-010 §3.1, §5.2).
	accessOwner
	// accessChannelManager: valid human credential whose principal is an
	// operator or a member of the path's {channel} holding manages = 1 (issue
	// #790, ADR-010 §3.2, §5.2).
	accessChannelManager
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
	// acceptsTicket opts the route in to ?ticket= authentication (issue
	// #270). It is false unless a route says otherwise, and on a route that
	// is false the parameter is never read.
	acceptsTicket bool
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

type credentialIDKey struct{}

// withCredentialID returns ctx carrying the id of the credential the request
// authenticated with, so later code can re-check that one credential without
// a second token lookup. The token itself is never stored in the context.
func withCredentialID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, credentialIDKey{}, id)
}

// credentialIDFrom returns the authenticating credential's id. It reports
// false under AuthOff and on exempt routes.
func credentialIDFrom(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(credentialIDKey{}).(int64)
	return id, ok
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

func isOwner(p store.Principal) bool { return p.IsOwner() }

// ownerProtected refuses a non-owner operator acting on an owner principal
// (issue #790, ADR-010 §3.1, §4.1): when there is an authenticated caller who
// is not an owner and targetID belongs to an owner, it writes 403 forbidden
// (auditing access_denied) and returns true. An unknown targetID returns false
// so the handler's normal 404 path answers.
func (s *Server) ownerProtected(w http.ResponseWriter, r *http.Request, targetID int64) bool {
	caller, authed := callerFrom(r.Context())
	if !authed || isOwner(caller) {
		return false
	}
	target, err := s.store.PrincipalByID(r.Context(), targetID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: find principal failed", "principal", targetID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return true
	}
	if !target.IsOwner() {
		return false
	}
	s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
	return true
}

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
		rt, matched := s.routeByPattern[pattern]
		if matched && rt.access == accessExempt {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			// A ticket stands in for a bearer only where the route asks for
			// it, and only when the request carries no Authorization header
			// at all: a header, even a bad one, is the credential, and a
			// ticket sent with it is neither read nor spent.
			if matched && rt.acceptsTicket && len(r.Header.Values("Authorization")) == 0 && r.URL.Query().Has("ticket") {
				principal, credentialID, redeemed := s.redeemWSTicket(w, r)
				if !redeemed {
					s.metrics.authFailed("invalid_ticket")
					return
				}
				next.ServeHTTP(w, r.WithContext(withCredentialID(withCaller(r.Context(), principal), credentialID)))
				return
			}
			s.metrics.authFailed("missing_credential")
			writeUnauthenticated(w)
			return
		}
		principal, cred, err := s.store.ResolveRequestCredential(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrCredentialInvalid):
			s.metrics.authFailed("invalid_credential")
			writeUnauthenticated(w)
			return
		case err != nil:
			// Fail closed: a broken store never authenticates anyone. The
			// error is wrapped by the store and never contains the token.
			slog.ErrorContext(r.Context(), "auth: resolve credential failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		// A setup-scoped credential reaches the routes that finish setup and
		// nothing else (design §5.3, review F6), checked here once, beside
		// expiry, so no route can forget it. A scope this code does not
		// know reaches nothing.
		if cred.Scope != schema.CredentialScopeFull && (cred.Scope != schema.CredentialScopeSetup || !matched || !setupScopeRoutes[pattern]) {
			writeSetupScopeRefused(w)
			return
		}
		// A REST request moves an idle-bounded credential's idle expiry (a
		// guest's, an operator's browser session); opening a socket does
		// not, and nothing on an open socket does (issue #705, review F5),
		// so a stolen token kept alive by a ping loop still runs out.
		if cred.IdleBounded && !isSocketRoute(pattern) {
			s.store.NoteGuestUse(r.Context(), cred.ID)
		}
		ctx := withRequestCredential(withCredentialID(withCaller(r.Context(), principal), cred.ID), cred)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isSocketRoute reports whether pattern is a WebSocket route: every one of
// them ends in /ws (routes.go).
func isSocketRoute(pattern string) bool { return strings.HasSuffix(pattern, "/ws") }

// guard returns the handler to register for rt. Under AuthOff it is the bare
// handler, so behaviour is unchanged. Under AuthRequired operator, owner and
// channel-manager routes are wrapped with their class check, and the routes of
// the re-authentication class (recentAuthRoutes) with the recent-auth gate
// inside it, so a caller without the role, mark or grant is told so before
// anything about a proof (issue #790, ADR-010 §5.2, §5.3).
func (s *Server) guard(rt route) http.Handler {
	if !s.cfg.authRequired() {
		return rt.handler
	}
	handler := rt.handler
	if rule := recentAuthRoutes[rt.pattern]; rule != reauthNone {
		handler = s.recentAuthGate(rule, handler)
	}
	switch rt.access {
	case accessOperator:
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
			handler.ServeHTTP(w, r)
		})
	case accessOwner:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, ok := callerFrom(r.Context())
			if !ok {
				writeUnauthenticated(w)
				return
			}
			if !isOwner(caller) {
				s.denyForbidden(w, r, caller, "forbidden", "owner required")
				return
			}
			handler.ServeHTTP(w, r)
		})
	case accessChannelManager:
		return s.humansOnlyBecause(denyAgentOnHumanRoute, "agents cannot administer channel membership", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, ok := callerFrom(r.Context())
			if !ok {
				writeUnauthenticated(w)
				return
			}
			channel, err := s.store.ChannelByName(r.Context(), r.PathValue("channel"))
			if errors.Is(err, store.ErrNotFound) {
				writeChannelNotFound(w)
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "auth: find channel failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				return
			}
			if !isOperator(caller) {
				member, err := s.store.IsChannelMember(r.Context(), channel.ID, caller.ID)
				if err != nil {
					slog.ErrorContext(r.Context(), "auth: check channel membership failed", "error", err)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
					return
				}
				if !member {
					writeChannelNotFound(w)
					return
				}
				manages, err := s.store.ManagesChannel(r.Context(), channel.ID, caller.ID)
				if err != nil {
					slog.ErrorContext(r.Context(), "auth: check channel manager failed", "error", err)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
					return
				}
				if !manages {
					s.denyForbidden(w, r, caller, "forbidden", "operator role or channel manager required")
					return
				}
			}
			handler.ServeHTTP(w, r)
		}))
	default:
		return handler
	}
}

// denyForbidden writes a 403 with the given error code and appends an
// access_denied audit event: actor principal:<id>, subject the route pattern,
// detail the error code. The event carries no request content or credential
// material. If the audit write fails the request is still denied.
func (s *Server) denyForbidden(w http.ResponseWriter, r *http.Request, caller store.Principal, code, message string) {
	// The client may already be gone; the denial must still be recorded.
	ctx := context.WithoutCancel(r.Context())
	// The mux sets r.Pattern on every dispatched request. Never fall back to
	// the request path: it is attacker-controlled and must not reach the
	// audit log.
	subject := r.Pattern
	if subject == "" {
		subject = "<unmatched>"
	}
	// Repeated identical denials from one principal are thinned out, so a
	// caller cannot grow the audit log with every refused request; the first
	// of each kind is written at once (issue #80, denialCoalescer).
	write, count := s.denials.admit(caller.ID, subject+"|"+code, s.now())
	if !write {
		writeError(w, http.StatusForbidden, code, message)
		return
	}
	if _, err := s.store.AppendAuditEvent(ctx, fmt.Sprintf("principal:%d", caller.ID), "access_denied", subject, denialDetail(code, count)); err != nil {
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
	return s.bindCallerID(w, r, authorID, "author_mismatch", "author_id does not match the authenticated principal")
}

// bindCallerID binds an identity field of a request body to the authenticated
// caller. Without a caller (AuthOff, or an exempt route) it does nothing, so
// the body is trusted as before. With one, an absent or zero id becomes the
// caller's; the caller's own id is accepted; anything else is a 403 with the
// given error code, audited. It reports whether the request may proceed. Call
// it before validating the body, because validation rejects a zero id.
func (s *Server) bindCallerID(w http.ResponseWriter, r *http.Request, id *int64, code, message string) bool {
	caller, ok := callerFrom(r.Context())
	if !ok {
		return true
	}
	switch *id {
	case 0:
		*id = caller.ID
	case caller.ID:
	default:
		s.denyForbidden(w, r, caller, code, message)
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
	voice := voiceAllowed(caller)
	resp := schema.WhoAmIResponseV1{
		ID:            caller.ID,
		Kind:          schema.PrincipalKind(caller.Kind),
		Name:          caller.Name,
		Role:          schema.PrincipalRole(caller.Role),
		Owner:         caller.IsOwner(),
		ServerVersion: s.cfg.Version,
		MayInvite:     mayInvite(caller),
		VoiceAllowed:  &voice,
	}
	// What the caller is, and a guest's visit (issue #705). Only with
	// authentication on, where there is a caller to classify.
	if err := s.guestWhoAmI(r.Context(), caller, &resp); err != nil {
		slog.ErrorContext(r.Context(), "whoami: read guest visit failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	// How the caller is authenticated, and whether setup is complete
	// (issue #708).
	if credID, ok := credentialIDFrom(r.Context()); ok {
		if err := s.authWhoAmI(r.Context(), caller, credID, &resp); err != nil {
			slog.ErrorContext(r.Context(), "whoami: read authentication state failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
			return
		}
	}
	// Only a person with an allowance is told what is left of it (issue
	// #468): operators have none, and nobody else may invite.
	if resp.MayInvite && !isOperator(caller) {
		ctx := r.Context()
		n, err := s.store.InvitesRemaining(ctx, caller.ID, store.InviteOptions{Allowance: s.cfg.InviteAllowance}, s.now())
		if err != nil {
			slog.ErrorContext(ctx, "whoami: count invites remaining failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
			return
		}
		remaining := int64(n)
		resp.InvitesRemaining = &remaining
	}
	writeJSON(w, http.StatusOK, resp)
}

// voiceAllowed is what whoami says about voice (issue #468): what the
// person's access grants, and false for an agent, which every voice route
// refuses whatever its grant says (voicePresenceChannel).
func voiceAllowed(p store.Principal) bool {
	return p.Kind == store.PrincipalHuman && p.VoiceAllowed
}

// mayInvite is what whoami says about making invites (issue #437): true for
// an operator and for a person granted the right. Only people make invites,
// so an agent is told false whatever its role; the store refuses it anyway.
func mayInvite(p store.Principal) bool {
	return p.Kind == store.PrincipalHuman && (isOperator(p) || p.MayInvite)
}
