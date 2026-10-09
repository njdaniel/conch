package server

import "net/http"

// routeTable lists every route the server serves with its authentication
// class. It is the single source for mux registration and the authentication
// policy: New builds the mux only from this table, and TestRouteTable fails
// when a route has no policy decision. Classes only take effect under
// AuthRequired.
func (s *Server) routeTable() []route {
	rt := func(pattern string, a access, h http.Handler) route {
		return route{pattern: pattern, access: a, handler: h}
	}
	hf := func(f http.HandlerFunc) http.Handler { return f }
	return []route{
		rt("GET /healthz", accessExempt, hf(s.handleHealth)),
		rt("GET /v0/ws", accessAuthenticated, hf(s.handleWS)),
		rt("GET /v1/ws", accessAuthenticated, hf(s.handleWSV1)),
		rt("GET /v1/whoami", accessAuthenticated, hf(s.handleWhoAmI)),
		rt("POST /v0/channels", accessOperator, hf(s.handleCreateChannel)),
		rt("GET /v1/channels", accessAuthenticated, hf(s.handleListChannels)),
		rt("POST /v0/principals", accessOperator, hf(s.handleCreatePrincipal)),
		rt("POST /v0/channels/{channel}/messages", accessAuthenticated, hf(s.handlePostMessage)),
		rt("GET /v0/channels/{channel}/messages", accessAuthenticated, hf(s.handleListMessages)),
		rt("POST /v1/channels/{channel}/messages", accessAuthenticated, hf(s.handlePostMessageV1)),
		rt("GET /v1/channels/{channel}/messages", accessAuthenticated, hf(s.handleListMessagesV1)),
		rt("PUT /v1/principals/{id}/manifest", accessOperator, hf(s.handlePutManifest)),
		// Operators, or the agent the manifest belongs to (checked in the handler).
		rt("GET /v1/principals/{id}/manifest", accessOperatorOrSelf, hf(s.handleGetManifest)),
		rt("POST /v1/principals/{id}/credentials", accessOperator, hf(s.handleCreateCredential)),
		rt("GET /v1/principals/{id}/credentials", accessOperator, hf(s.handleListCredentials)),
		rt("POST /v1/credentials/{credential_id}/rotate", accessOperator, hf(s.handleRotateCredential)),
		rt("DELETE /v1/credentials/{credential_id}", accessOperator, hf(s.handleRevokeCredential)),
		rt("POST /v1/hooks", accessOperator, hf(s.handleCreateHook)),
		// Webhook ingest authenticates with its own URL token.
		rt("POST /v1/hooks/{token}", accessExempt, hf(s.handleIngestHook)),
		// Approval routes need a credential under AuthRequired but still trust
		// the body's identity fields; binding them to the caller is issue #92.
		rt("POST /v1/approvals", accessAuthenticated, hf(s.handleCreateApproval)),
		rt("GET /v1/approvals", accessAuthenticated, hf(s.handleListOpenApprovals)),
		rt("POST /v1/approvals/{id}/decisions", accessAuthenticated, hf(s.handleCastDecision)),
		// /mcp has its own bearer authentication (mcp.go) and is not wrapped.
		rt("/mcp", accessExempt, s.mcpHandler()),
	}
}
