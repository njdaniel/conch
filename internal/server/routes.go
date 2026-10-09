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
		rt("GET /v2/ws", accessAuthenticated, hf(s.handleWSV2)),
		rt("GET /v1/whoami", accessAuthenticated, hf(s.handleWhoAmI)),
		rt("POST /v0/channels", accessOperator, hf(s.handleCreateChannel)),
		rt("GET /v1/channels", accessAuthenticated, hf(s.handleListChannels)),
		rt("POST /v0/principals", accessOperator, hf(s.handleCreatePrincipal)),
		rt("POST /v0/channels/{channel}/messages", accessAuthenticated, hf(s.handlePostMessage)),
		rt("GET /v0/channels/{channel}/messages", accessAuthenticated, hf(s.handleListMessages)),
		rt("POST /v1/channels/{channel}/messages", accessAuthenticated, hf(s.handlePostMessageV1)),
		rt("GET /v1/channels/{channel}/messages", accessAuthenticated, hf(s.handleListMessagesV1)),
		// Scoped messages (issue #116). The v2 list and socket are the only
		// readers that can be given a message with an audience.
		rt("POST /v2/channels/{channel}/messages", accessAuthenticated, hf(s.handlePostMessageV2)),
		rt("GET /v2/channels/{channel}/messages", accessAuthenticated, hf(s.handleListMessagesV2)),
		// Members of the channel and operators (checked in the handler); anyone
		// else gets the channel-not-found response.
		rt("GET /v1/channels/{channel}/members", accessAuthenticated, hf(s.handleListChannelMembers)),
		rt("PUT /v1/channels/{channel}/members/{principal_id}", accessOperator, hf(s.handlePutChannelMember)),
		rt("DELETE /v1/channels/{channel}/members/{principal_id}", accessOperator, hf(s.handleDeleteChannelMember)),
		// Nets (issue #115). Management is operator-only; the list is open to
		// authenticated callers and filtered to the nets they are on.
		rt("POST /v1/channels/{channel}/nets", accessOperator, hf(s.handleCreateNet)),
		rt("GET /v1/channels/{channel}/nets", accessAuthenticated, hf(s.handleListNets)),
		rt("DELETE /v1/channels/{channel}/nets/{net}", accessOperator, hf(s.handleArchiveNet)),
		rt("PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}", accessOperator, hf(s.handlePutNetMember)),
		rt("DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}", accessOperator, hf(s.handleDeleteNetMember)),
		// Voice (issue #126). Human members only; checked in the handler.
		rt("POST /v1/channels/{channel}/voice/session", accessAuthenticated, hf(s.handleVoiceSession)),
		rt("PUT /v1/principals/{id}/manifest", accessOperator, hf(s.handlePutManifest)),
		// Operators, or the agent the manifest belongs to (checked in the handler).
		rt("GET /v1/principals/{id}/manifest", accessOperatorOrSelf, hf(s.handleGetManifest)),
		rt("POST /v1/principals/{id}/credentials", accessOperator, hf(s.handleCreateCredential)),
		rt("GET /v1/principals/{id}/credentials", accessOperator, hf(s.handleListCredentials)),
		rt("POST /v1/credentials/{credential_id}/rotate", accessOperator, hf(s.handleRotateCredential)),
		rt("DELETE /v1/credentials/{credential_id}", accessOperator, hf(s.handleRevokeCredential)),
		// Disabling a principal and mass revocation (issue #101).
		rt("POST /v1/principals/{id}/disable", accessOperator, hf(s.handleDisablePrincipal)),
		rt("POST /v1/principals/{id}/enable", accessOperator, hf(s.handleEnablePrincipal)),
		rt("POST /v1/principals/{id}/credentials/revoke-all", accessOperator, hf(s.handleRevokeAllCredentials)),
		rt("POST /v1/hooks", accessOperator, hf(s.handleCreateHook)),
		rt("GET /v1/hooks", accessOperator, hf(s.handleListHooks)),
		rt("DELETE /v1/hooks/{id}", accessOperator, hf(s.handleRevokeHook)),
		// Webhook ingest authenticates with its own URL token.
		rt("POST /v1/hooks/{token}", accessExempt, hf(s.handleIngestHook)),
		// Approval routes need a credential under AuthRequired but still trust
		// the body's identity fields; binding them to the caller is issue #92.
		// These are the human surface for approvals. Agents request and observe
		// approvals through MCP, where the manifest and membership gates apply,
		// so an agent credential is refused here (issue #79).
		rt("POST /v1/approvals", accessAuthenticated, s.humansOnly(hf(s.handleCreateApproval))),
		rt("GET /v1/approvals", accessAuthenticated, s.humansOnly(hf(s.handleListOpenApprovals))),
		rt("POST /v1/approvals/{id}/decisions", accessAuthenticated, s.humansOnly(hf(s.handleCastDecision))),
		// /mcp has its own bearer authentication (mcp.go) and is not wrapped.
		rt("/mcp", accessExempt, s.mcpHandler()),
	}
}
