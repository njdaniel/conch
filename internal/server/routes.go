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
		// Takes ?ticket= (issue #270); see tickets.go.
		{pattern: "GET /v2/ws", access: accessAuthenticated, handler: hf(s.handleWSV2), acceptsTicket: true},
		// The events socket (issue #269) takes a ticket too: it is the one a
		// browser opens.
		{pattern: "GET /v1/events/ws", access: accessAuthenticated, handler: hf(s.handleEventsWS), acceptsTicket: true},
		rt("POST /v1/ws-tickets", accessAuthenticated, hf(s.handleCreateWSTicket)),
		rt("GET /v1/whoami", accessAuthenticated, hf(s.handleWhoAmI)),
		rt("POST /v0/channels", accessOperator, hf(s.handleCreateChannel)),
		rt("GET /v1/channels", accessAuthenticated, hf(s.handleListChannels)),
		rt("POST /v0/principals", accessOperator, hf(s.handleCreatePrincipal)),
		// Renaming (issue #441): operators rename anyone, a person themselves
		// (checked in the handler).
		rt("PUT /v1/principals/{id}/name", accessOperatorOrSelf, hf(s.handleRenamePrincipal)),
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
		// Screen share (issue #443): a member who holds a session claims the
		// room's one share; the sharer or an operator (member or not) stops
		// it. Checked in the handlers; see voice_share.go.
		rt("POST /v1/channels/{channel}/voice/share", accessAuthenticated, hf(s.handleVoiceShareStart)),
		rt("DELETE /v1/channels/{channel}/voice/share", accessAuthenticated, hf(s.handleVoiceShareStop)),
		// Voice presence (issue #127): the snapshot and its socket.
		rt("GET /v1/channels/{channel}/voice", accessAuthenticated, hf(s.handleVoicePresence)),
		// Takes ?ticket= (issue #442): a browser cannot set a header on a
		// socket. The credential and membership re-checks are unchanged.
		{pattern: "GET /v1/voice/ws", access: accessAuthenticated, handler: hf(s.handleVoiceWS), acceptsTicket: true},
		// Voice occupancy (issue #596): how many are in each channel's voice,
		// for the caller's own channels; people with voice only, checked in the
		// handlers (voice_occupancy.go). The socket takes ?ticket= as the
		// presence socket does, for the browser.
		rt("GET /v1/voice/occupancy", accessAuthenticated, hf(s.handleVoiceOccupancy)),
		{pattern: "GET /v1/voice/occupancy/ws", access: accessAuthenticated, handler: hf(s.handleVoiceOccupancyWS), acceptsTicket: true},
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
		// Read state (issue #351). People only (an agent gets 403
		// human_required, in the handler); the caller's own marks, per
		// principal. See read_state.go.
		rt("GET /v1/read-state", accessAuthenticated, hf(s.handleListReadState)),
		rt("PUT /v1/channels/{channel}/read-state", accessAuthenticated, hf(s.handlePutReadState)),
		rt("PUT /v1/principals/{id}/manifest", accessOperator, hf(s.handlePutManifest)),
		// Operators, or the agent the manifest belongs to (checked in the handler).
		rt("GET /v1/principals/{id}/manifest", accessOperatorOrSelf, hf(s.handleGetManifest)),
		rt("POST /v1/principals/{id}/credentials", accessOperator, hf(s.handleCreateCredential)),
		rt("GET /v1/principals/{id}/credentials", accessOperator, hf(s.handleListCredentials)),
		rt("POST /v1/credentials/{credential_id}/rotate", accessOperator, hf(s.handleRotateCredential)),
		rt("DELETE /v1/credentials/{credential_id}", accessOperator, hf(s.handleRevokeCredential)),
		// Signing out (issue #439): the caller revokes the credential the
		// request came with. More specific than the route above, so the mux
		// never hands "current" to it as an id.
		rt("DELETE /v1/credentials/current", accessAuthenticated, hf(s.handleRevokeCurrentCredential)),
		// Your sign-ins (issue #496): the caller lists its own live
		// credentials and ends any one of them by id; another's id is 404,
		// as one never issued (checked in the store). There is no GET of one
		// credential, and ending one has a segment more than the operator's
		// revoke above, so neither route meets another in the mux.
		rt("GET /v1/credentials/mine", accessAuthenticated, hf(s.handleListOwnCredentials)),
		// End visit (issue #705): a guest ends its own visit; anyone else is
		// not a guest (checked in the handler). The entry routes are #710's.
		rt("POST /v1/guest/end", accessAuthenticated, hf(s.handleEndVisit)),
		rt("DELETE /v1/credentials/mine/{id}", accessAuthenticated, hf(s.handleRevokeOwnCredential)),
		// Disabling a principal and mass revocation (issue #101).
		rt("POST /v1/principals/{id}/disable", accessOperator, hf(s.handleDisablePrincipal)),
		rt("POST /v1/principals/{id}/enable", accessOperator, hf(s.handleEnablePrincipal)),
		// Deleting an account is disabling and hiding it (issue #445): one
		// principal, or every principal from an invite or with a name
		// prefix. The list shows them all, hidden ones on request. The bulk
		// route has one segment fewer than the per-id ones, so the two never
		// meet in the mux; POST /v0/principals is another version.
		rt("POST /v1/principals/{id}/hide", accessOperator, hf(s.handleHidePrincipal)),
		rt("POST /v1/principals/hide", accessOperator, hf(s.handleHidePrincipals)),
		rt("GET /v1/principals", accessOperator, hf(s.handleListPrincipals)),
		// Operators, or the principal itself: "sign out everywhere" (issue
		// #439; checked in the handler).
		rt("POST /v1/principals/{id}/credentials/revoke-all", accessOperatorOrSelf, hf(s.handleRevokeAllCredentials)),
		rt("GET /v1/audit", accessOperator, hf(s.handleExportAudit)),
		rt("POST /v1/hooks", accessOperator, hf(s.handleCreateHook)),
		rt("GET /v1/hooks", accessOperator, hf(s.handleListHooks)),
		rt("DELETE /v1/hooks/{id}", accessOperator, hf(s.handleRevokeHook)),
		// Invites (issue #437). Making, listing and closing one's own are for
		// people (an agent gets 403 human_required, in the handler). Preview
		// and redeem are exempt: whoever holds a code may ask what it grants
		// and use it. They carry the code in the body only, are rate-limited
		// per client address before any store work, and the store decides
		// everything else. See invites.go.
		rt("POST /v1/invites", accessAuthenticated, hf(s.handleCreateInvite)),
		rt("GET /v1/invites", accessAuthenticated, hf(s.handleListInvites)),
		rt("DELETE /v1/invites/{id}", accessAuthenticated, hf(s.handleCloseInvite)),
		rt("POST /v1/invites/preview", accessExempt, hf(s.handlePreviewInvite)),
		// v2 (issue #707) makes the account with a password, taken with the
		// name, over HTTPS or loopback only. v1 makes one without, as before,
		// until the browser has moved to v2 (#713).
		rt("POST /v1/invites/redeem", accessExempt, hf(s.handleRedeemInvite)),
		rt("POST /v2/invites/redeem", accessExempt, hf(s.handleRedeemInviteV2)),
		// Password sign-in (issue #707): exempt, as the invite routes are,
		// and limited before any hashing; over HTTPS or loopback only. A
		// member's right password answers a credential, an operator's only a
		// challenge for the second factor. See login.go.
		rt("POST /v1/login", accessExempt, hf(s.handleLogin)),
		// Passwords (issue #708): a member's first password with an
		// operator's enrollment link, a change with the current one, and the
		// recent-password proof, for any authenticated person (the handler
		// refuses agents and guests); a reset with an operator's reset link,
		// exempt, as the other code routes are. All over HTTPS or loopback
		// only. See password_recovery.go.
		rt("POST /v1/password/mine", accessAuthenticated, hf(s.handleSetPassword)),
		rt("PUT /v1/password/mine", accessAuthenticated, hf(s.handleChangePassword)),
		rt("POST /v1/password/reset", accessExempt, hf(s.handleResetPassword)),
		rt("POST /v1/reauth", accessAuthenticated, hf(s.handleReauth)),
		// Operator administration of invites (issue #438): who joined through
		// an invite, disabling them or everyone a person invited, the right to
		// invite, and the pause switch. Operators only; GET /v1/invites and
		// DELETE /v1/invites/{id} above serve operators everyone's invites.
		rt("GET /v1/invites/{id}/principals", accessOperator, hf(s.handleInviteJoiners)),
		rt("POST /v1/invites/{id}/disable-joiners", accessOperator, hf(s.handleDisableJoiners)),
		rt("POST /v1/principals/{id}/disable-invitees", accessOperator, hf(s.handleDisableInvitees)),
		rt("PUT /v1/principals/{id}/invite-permission", accessOperator, hf(s.handleInvitePermission)),
		rt("GET /v1/invites/settings", accessOperator, hf(s.handleGetInviteSettings)),
		rt("PUT /v1/invites/settings", accessOperator, hf(s.handlePutInviteSettings)),
		// Sign-in links (issue #493): an operator makes a one-time link that
		// signs a member back into the same account. Preview and redeem are
		// exempt, as an invite's are, and limited the same way. See
		// sign_in_links.go.
		rt("POST /v1/principals/{id}/sign-in-links", accessOperator, hf(s.handleCreateSignInLink)),
		rt("POST /v1/sign-in-links/preview", accessExempt, hf(s.handlePreviewSignInLink)),
		rt("POST /v1/sign-in-links/redeem", accessExempt, hf(s.handleRedeemSignInLink)),
		// Closing one that leaked, without disabling its person.
		rt("DELETE /v1/sign-in-links/{id}", accessOperator, hf(s.handleCloseSignInLink)),
		// Problem reports (issue #446). Sending one is for people (an agent
		// gets 403 human_required, in the handler); reading and filing them
		// is for operators. conchd never sends a report anywhere.
		rt("POST /v1/reports", accessAuthenticated, hf(s.handleCreateReport)),
		rt("GET /v1/reports", accessOperator, hf(s.handleListReports)),
		rt("GET /v1/reports/{id}", accessOperator, hf(s.handleGetReport)),
		rt("PUT /v1/reports/{id}", accessOperator, hf(s.handleUpdateReport)),
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
		// Device links (issue #712): a member's own links that sign their
		// account in on another device, made under a recent-password proof.
		// Members only (the handler refuses agents, guests and operators);
		// redeemed by POST /v1/sign-in-links/redeem. See sign_in_links.go.
		rt("POST /v1/sign-in-links/mine", accessAuthenticated, hf(s.handleCreateDeviceLink)),
		rt("GET /v1/sign-in-links/mine", accessAuthenticated, hf(s.handleListOwnDeviceLinks)),
		rt("DELETE /v1/sign-in-links/mine/{id}", accessAuthenticated, hf(s.handleCloseOwnDeviceLink)),
	}
}
