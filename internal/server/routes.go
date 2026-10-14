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
		// The only socket that takes ?ticket= (issue #270); see tickets.go.
		{pattern: "GET /v2/ws", access: accessAuthenticated, handler: hf(s.handleWSV2), acceptsTicket: true},
		rt("POST /v1/ws-tickets", accessAuthenticated, hf(s.handleCreateWSTicket)),
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
		rt("GET /v2/channels/{channel}/messages/search", accessAuthenticated, hf(s.handleSearchMessagesV2)),
		// Files (issue #23). Humans only; an agent is refused before the
		// handler runs. Who may fetch a file is decided by the store.
		rt("POST /v2/channels/{channel}/files", accessAuthenticated, s.humansOnlyFiles(hf(s.handleUploadFile))),
		rt("GET /v2/files/{id}/meta", accessAuthenticated, s.humansOnlyFiles(hf(s.handleGetFileMeta))),
		rt("GET /v2/files/{id}", accessAuthenticated, s.humansOnlyFiles(hf(s.handleGetFile))),
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
		// Transmit reports (issue #135): a client reports each press and
		// release. Human members who hold a session; checked in the handler.
		rt("POST /v1/channels/{channel}/voice/transmit", accessAuthenticated, hf(s.handleVoiceTransmit)),
		// Voice presence (issue #127): the snapshot and its socket.
		rt("GET /v1/channels/{channel}/voice", accessAuthenticated, hf(s.handleVoicePresence)),
		rt("GET /v1/voice/ws", accessAuthenticated, hf(s.handleVoiceWS)),
		// Sessions (issue #265). Any caller with a credential; the session
		// routes act on the caller's own sessions (by credential) and the
		// list is a membership-gated read. Checked in the handlers.
		rt("POST /v1/sessions", accessAuthenticated, hf(s.handleCreateSession)),
		rt("POST /v1/sessions/{id}/heartbeat", accessAuthenticated, hf(s.handleSessionHeartbeat)),
		rt("DELETE /v1/sessions/{id}", accessAuthenticated, hf(s.handleEndSession)),
		rt("GET /v1/sessions", accessAuthenticated, hf(s.handleListSessions)),
		// Runs (issue #266). Any caller with a credential: starting a run is
		// for agents, on a session of their own credential; reporting is for
		// the credential that owns the run's session; reads are for the
		// readers who may see the run. Checked in the handlers.
		rt("POST /v1/sessions/{id}/runs", accessAuthenticated, hf(s.handleCreateRun)),
		rt("POST /v1/runs/{id}/state", accessAuthenticated, hf(s.handleSetRunState)),
		rt("POST /v1/runs/{id}/activity", accessAuthenticated, hf(s.handleAppendRunActivity)),
		rt("GET /v1/runs/{id}", accessAuthenticated, hf(s.handleGetRun)),
		rt("GET /v1/runs", accessAuthenticated, hf(s.handleListRuns)),
		rt("GET /v1/runs/{id}/activity", accessAuthenticated, hf(s.handleListRunActivity)),
		// Interventions (issue #287). A human who can see the run asks; the
		// readers who can see the run read; the credential that owns the
		// run's session polls and answers. Checked in the handlers.
		rt("POST /v1/runs/{id}/interventions", accessAuthenticated, hf(s.handleCreateIntervention)),
		rt("GET /v1/runs/{id}/interventions", accessAuthenticated, hf(s.handleListRunInterventions)),
		rt("GET /v1/interventions/{id}", accessAuthenticated, hf(s.handleGetIntervention)),
		rt("GET /v1/sessions/{id}/interventions", accessAuthenticated, hf(s.handleListSessionInterventions)),
		rt("POST /v1/interventions/{id}/ack", accessAuthenticated, hf(s.handleAckIntervention)),
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
		rt("GET /v1/audit", accessOperator, hf(s.handleExportAudit)),
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
		rt("GET /v1/approvals/{id}", accessAuthenticated, s.humansOnly(hf(s.handleGetApproval))),
		rt("POST /v1/approvals/{id}/decisions", accessAuthenticated, s.humansOnly(hf(s.handleCastDecision))),
		// /mcp has its own bearer authentication (mcp.go) and is not wrapped.
		rt("/mcp", accessExempt, s.mcpHandler()),
	}
}
