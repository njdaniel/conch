package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Authentication for the /mcp endpoint (issues #97, #79). This file is the
// only part of the MCP front-end that touches the store directly; the tools in
// mcp.go reach data only through agentScope (authz.go).

// mcpIdentity is who an /mcp request authenticated as, and by which means.
type mcpIdentity struct {
	principalID int64
	// credentialID is the stored credential the request presented, or 0 when
	// it authenticated through a deprecated static --mcp-token mapping.
	credentialID int64
}

// mcpIdentityKey carries the authenticated identity from the /mcp wrapper to
// the per-request server factory, so a request is authenticated exactly once.
type mcpIdentityKey struct{}

func (s *Server) mcpHandler() http.Handler {
	if n := len(s.cfg.MCPBearerTokens); n > 0 {
		slog.Warn("mcp: static --mcp-token mappings are deprecated; issue credentials with POST /v1/principals/{id}/credentials instead", "mappings", n)
	}
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		identity, ok := r.Context().Value(mcpIdentityKey{}).(mcpIdentity)
		if !ok {
			return nil
		}
		return s.mcpServerFor(identity)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := s.authenticateMCP(r)
		if !ok {
			// One response for every failure: missing, malformed, unknown,
			// expired, revoked, wrong principal kind, disabled, or a store error.
			w.Header().Set("WWW-Authenticate", `Bearer realm="conch-mcp"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpIdentityKey{}, identity)))
	})
}

// authenticateMCP resolves the request's bearer token to an agent principal.
// Stored credentials (issue #78) are tried first; the static --mcp-token map
// is a deprecated fallback. It never logs the token.
func (s *Server) authenticateMCP(r *http.Request) (mcpIdentity, bool) {
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return mcpIdentity{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return mcpIdentity{}, false
	}
	ctx := r.Context()

	principal, credentialID, err := s.store.ResolveCredentialDetail(ctx, token)
	switch {
	case err == nil:
		// MCP is the agent front-end: a human's credential is not accepted here.
		if principal.Kind != store.PrincipalAgent {
			return mcpIdentity{}, false
		}
		return mcpIdentity{principalID: principal.ID, credentialID: credentialID}, true
	case !errors.Is(err, store.ErrCredentialInvalid):
		// The store failed. Fail closed rather than fall through to the map.
		slog.ErrorContext(ctx, "mcp: resolve credential failed", "error", err)
		return mcpIdentity{}, false
	}

	// A token shaped like a stored credential is never honoured through the
	// static map, so configuration cannot bring a revoked or expired
	// credential back to life.
	if strings.HasPrefix(token, schema.CredentialTokenPrefix) {
		return mcpIdentity{}, false
	}
	principalID, ok := s.cfg.MCPBearerTokens[token]
	if !ok || principalID <= 0 {
		return mcpIdentity{}, false
	}
	live, err := s.staticAgentUsable(ctx, principalID)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: authenticate principal failed", "error", err)
		return mcpIdentity{}, false
	}
	if !live {
		return mcpIdentity{}, false
	}
	return mcpIdentity{principalID: principalID}, true
}

// staticAgentUsable reports whether a principal reached through a static
// --mcp-token mapping may use MCP: it must exist, be an agent, and not be
// disabled. A static mapping is configuration, not a credential row, so
// disabling the principal (issue #101) revokes nothing there; this is the
// explicit check that keeps a disabled agent out.
func (s *Server) staticAgentUsable(ctx context.Context, principalID int64) (bool, error) {
	p, err := s.store.PrincipalByID(ctx, principalID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.Kind == store.PrincipalAgent && p.DisabledAt == nil, nil
}

// mcpStillAuthenticated re-checks an identity that authenticated earlier. A
// long-running tool (await_decision) calls it on every poll, so that revoking
// or expiring the credential, or disabling the principal, ends the call
// instead of letting it run to its timeout.
func (s *Server) mcpStillAuthenticated(ctx context.Context, identity mcpIdentity) (bool, error) {
	if identity.credentialID > 0 {
		return s.store.CredentialLive(ctx, identity.credentialID)
	}
	return s.staticAgentUsable(ctx, identity.principalID)
}
