package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Agent authorization (issue #79, ADR-000 D10, docs/design/agent-manifest.md).
//
// An agent may act only where two independent gates both allow it:
//
//  1. Channel membership (issue #90) — the visibility boundary for every
//     principal. An agent that is not a member of a channel is told the
//     channel (or approval) does not exist, exactly as for an unknown one.
//  2. Its capability manifest (issue #77) — a further restriction on agents.
//     The manifest must grant the capability, and grant the needed permission
//     in that channel. A member refused by its manifest gets "forbidden".
//
// Everything here fails closed: no manifest, an invalid stored manifest, a
// tool with no capability mapping, or a store error all deny. Every denial is
// audited (actor, capability, target, reason) and none of it ever touches
// credential material.
//
// This is the only place the policy lives. The MCP tools reach channels and
// approvals only through agentScope, and the REST/WebSocket handlers call
// agentManifestAllows for agent callers, so no path acts for an agent
// principal without passing through it.

// Denial reasons recorded in the audit log.
const (
	denyUnmappedTool      = "unmapped_tool"
	denyNoManifest        = "no_manifest"
	denyInvalidManifest   = "invalid_manifest"
	denyCapability        = "capability_not_granted"
	denyNotMember         = "not_a_member"
	denyChannelPermission = "channel_permission_not_granted"
	denyAgentOnHumanRoute = "agents_use_mcp"
)

var (
	errForbidden       = &schema.Error{Code: "forbidden", Message: "the agent's manifest does not permit this"}
	errChannelNotFound = &schema.Error{Code: "channel_not_found", Message: "channel not found"}
	errApprovalMissing = &schema.Error{Code: "approval_not_found", Message: "approval not found"}
	errInternal        = &schema.Error{Code: "internal_error", Message: "internal server error"}
	errUnauthenticated = &schema.Error{Code: "unauthenticated", Message: "the credential is no longer valid"}
)

// auditAgentDenial records one denied agent action. A failure to write the
// audit row is logged; the action stays denied either way.
func (s *Server) auditAgentDenial(ctx context.Context, principalID int64, subject string, capability schema.Capability, channelID int64, reason string) {
	target := "none"
	if channelID > 0 {
		target = fmt.Sprintf("channel:%d", channelID)
	}
	named := string(capability)
	if named == "" {
		named = "n/a"
	}
	detail := fmt.Sprintf("capability=%s target=%s reason=%s", named, target, reason)
	// The caller may already be gone; the denial must still be recorded.
	actx := context.WithoutCancel(ctx)
	if _, err := s.store.AppendAuditEvent(actx, fmt.Sprintf("principal:%d", principalID), "access_denied", subject, detail); err != nil {
		slog.ErrorContext(actx, "authz: audit access_denied failed", "principal", principalID, "error", err)
	}
}

// agentManifest loads the agent's manifest. It returns a denial reason when
// there is none or the stored one is invalid, and an error only for a store
// failure (which callers must also treat as a denial).
func (s *Server) agentManifest(ctx context.Context, principalID int64) (schema.AgentManifestV1, string, error) {
	m, err := s.store.AgentManifestByPrincipal(ctx, principalID)
	switch {
	case err == nil:
		return m, "", nil
	case errors.Is(err, store.ErrNotFound):
		return schema.AgentManifestV1{}, denyNoManifest, nil
	case errors.Is(err, store.ErrInvalidManifest):
		return schema.AgentManifestV1{}, denyInvalidManifest, nil
	default:
		return schema.AgentManifestV1{}, "", err
	}
}

// agentManifestAllows is the manifest gate alone: does the agent's manifest
// grant capability, and permission in channel channelID? Denials are audited
// under subject. A store failure is reported as an error and is not an allow.
// The channel is required: a caller with no channel to name is refused rather
// than checked on capability alone.
func (s *Server) agentManifestAllows(ctx context.Context, principalID int64, subject string, capability schema.Capability, channelID int64, permission schema.ChannelPermission) (bool, error) {
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return false, err
	}
	switch {
	case reason != "":
	case !m.Allows(capability):
		reason = denyCapability
	case channelID <= 0 || !m.AllowsChannel(channelID, permission):
		reason = denyChannelPermission
	}
	if reason != "" {
		s.auditAgentDenial(ctx, principalID, subject, capability, channelID, reason)
		return false, nil
	}
	return true, nil
}

// agentScope authorizes one MCP tool call for one agent. It is created by the
// tool wrapper after the tool's capability has been checked, and it is the
// only way a tool reaches a channel or an approval: the lookups below return
// grantedChannel and grantedApproval values, and reading or writing content
// is a method on those, so a tool body never touches the store.
type agentScope struct {
	s          *Server
	identity   mcpIdentity
	subject    string // "mcp:<tool>", the audit subject
	tool       string
	capability schema.Capability
	manifest   schema.AgentManifestV1
}

// newAgentScope checks the capability gate for tool: the tool must be mapped
// to a capability and the agent's manifest must grant it. It returns a
// *schema.Error to hand back to the caller when the call is refused.
func (s *Server) newAgentScope(ctx context.Context, identity mcpIdentity, tool string) (*agentScope, *schema.Error) {
	a := &agentScope{s: s, identity: identity, subject: "mcp:" + tool, tool: tool}
	capability, ok := schema.MCPToolCapability(tool)
	if !ok {
		// A tool registered without a capability mapping has no access.
		s.auditAgentDenial(ctx, identity.principalID, a.subject, "", 0, denyUnmappedTool)
		return nil, errForbidden
	}
	a.capability = capability
	if serr := a.loadManifest(ctx); serr != nil {
		return nil, serr
	}
	return a, nil
}

// loadManifest reads the agent's manifest afresh and applies the capability
// gate to it.
func (a *agentScope) loadManifest(ctx context.Context) *schema.Error {
	m, reason, err := a.s.agentManifest(ctx, a.identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: load manifest failed", "principal", a.identity.principalID, "error", err)
		return errInternal
	}
	if reason == "" && !m.Allows(a.capability) {
		reason = denyCapability
	}
	if reason != "" {
		a.s.auditAgentDenial(ctx, a.identity.principalID, a.subject, a.capability, 0, reason)
		return errForbidden
	}
	a.manifest = m
	return nil
}

// refresh re-establishes everything a scope decided when it was created, for
// a tool that runs long enough for it to change (await_decision calls it on
// every poll): the credential or principal must still be usable, and the
// manifest, read again, must still grant the capability. Channel membership
// and the per-channel permission are re-checked by the next lookup.
func (a *agentScope) refresh(ctx context.Context) *schema.Error {
	live, err := a.s.mcpStillAuthenticated(ctx, a.identity)
	if err != nil {
		slog.ErrorContext(ctx, "authz: re-check credential failed", "principal", a.identity.principalID, "error", err)
		return errInternal
	}
	if !live {
		return errUnauthenticated
	}
	return a.loadManifest(ctx)
}

// channel applies both remaining gates to one channel: membership first (a
// non-member learns nothing), then the manifest's per-channel permission.
func (a *agentScope) channel(ctx context.Context, channel store.Channel, permission schema.ChannelPermission, notFound *schema.Error) *schema.Error {
	member, err := a.s.store.IsChannelMember(ctx, channel.ID, a.identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: check membership failed", "principal", a.identity.principalID, "error", err)
		return errInternal
	}
	if !member {
		a.s.auditAgentDenial(ctx, a.identity.principalID, a.subject, a.capability, channel.ID, denyNotMember)
		return notFound
	}
	if !a.manifest.AllowsChannel(channel.ID, permission) {
		a.s.auditAgentDenial(ctx, a.identity.principalID, a.subject, a.capability, channel.ID, denyChannelPermission)
		return errForbidden
	}
	return nil
}

// grantedChannel is a channel the agent has been authorized to use with one
// permission. It exists only as the result of a scope lookup.
type grantedChannel struct {
	scope   *agentScope
	channel store.Channel
}

// id returns the channel's id.
func (g *grantedChannel) id() int64 { return g.channel.ID }

// insertMessage stores a message in the channel, authored by the agent.
func (g *grantedChannel) insertMessage(ctx context.Context, body string, payload *schema.Payload) (store.Message, error) {
	return g.scope.s.store.InsertMessageV1(ctx, g.channel.ID, g.scope.identity.principalID, body, payload)
}

// listMessages reads one page of the channel's messages.
func (g *grantedChannel) listMessages(ctx context.Context, after int64, limit int) ([]store.Message, error) {
	return g.scope.s.store.ListMessages(ctx, g.channel.ID, after, limit)
}

// grantedApproval is an approval the agent has been authorized to observe.
type grantedApproval struct {
	scope    *agentScope
	approval store.Approval
}

// state returns the approval's state as of the lookup.
func (g *grantedApproval) state() schema.ApprovalState { return g.approval.State }

// resolution reads the approval's resolution event.
func (g *grantedApproval) resolution(ctx context.Context) (schema.ApprovalResolutionV1, error) {
	return g.scope.s.store.ResolutionByApprovalID(ctx, g.approval.ID)
}

// channelByName resolves a channel the agent may use with permission. An
// unknown channel and a channel the agent is not a member of are the same
// answer.
func (a *agentScope) channelByName(ctx context.Context, name string, permission schema.ChannelPermission) (*grantedChannel, *schema.Error) {
	channel, err := a.s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return nil, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errChannelNotFound); serr != nil {
		return nil, serr
	}
	return &grantedChannel{scope: a, channel: channel}, nil
}

// channelByID is channelByName for a channel id.
func (a *agentScope) channelByID(ctx context.Context, id int64, permission schema.ChannelPermission) (*grantedChannel, *schema.Error) {
	channel, err := a.s.store.ChannelByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return nil, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errChannelNotFound); serr != nil {
		return nil, serr
	}
	return &grantedChannel{scope: a, channel: channel}, nil
}

// approval resolves an approval the agent may observe: it must be a member of
// the approval's channel and its manifest must grant permission there. An
// unknown approval and one in a channel the agent is not in are the same
// answer. Being the requester grants nothing by itself: membership is the
// boundary, so an agent removed from a channel can no longer watch the
// approvals it raised there.
func (a *agentScope) approval(ctx context.Context, id int64, permission schema.ChannelPermission) (*grantedApproval, *schema.Error) {
	approval, err := a.s.store.ApprovalByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errApprovalMissing
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find approval failed", "error", err)
		return nil, errInternal
	}
	channel, err := a.s.store.ChannelByID(ctx, approval.ChannelID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: find approval channel failed", "error", err)
		return nil, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errApprovalMissing); serr != nil {
		return nil, serr
	}
	return &grantedApproval{scope: a, approval: approval}, nil
}

// auditAgentNonMember records a membership refusal for a REST or WebSocket
// request made with an agent's credential, so that an agent probing channels
// it is not in leaves the same trace there as it does over MCP. It does
// nothing for other callers.
func (s *Server) auditAgentNonMember(r *http.Request, capability schema.Capability, channelID int64) {
	caller, ok := callerFrom(r.Context())
	if !ok || caller.Kind != store.PrincipalAgent {
		return
	}
	subject := r.Pattern
	if subject == "" {
		subject = "<unmatched>"
	}
	s.auditAgentDenial(r.Context(), caller.ID, subject, capability, channelID, denyNotMember)
}

// agentCallerAllowed applies the manifest gate to a REST or WebSocket request
// made with an agent's credential, after membership has already been checked.
// It reports true for callers that are not agents (and when there is no
// caller). When it reports false it has written the response.
func (s *Server) agentCallerAllowed(w http.ResponseWriter, r *http.Request, capability schema.Capability, channelID int64, permission schema.ChannelPermission) bool {
	caller, ok := callerFrom(r.Context())
	if !ok || caller.Kind != store.PrincipalAgent {
		return true
	}
	subject := r.Pattern
	if subject == "" {
		subject = "<unmatched>"
	}
	allowed, err := s.agentManifestAllows(r.Context(), caller.ID, subject, capability, channelID, permission)
	if err != nil {
		slog.ErrorContext(r.Context(), "authz: load manifest failed", "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
		return false
	}
	return true
}

// agentReadAllowed reports whether manifest m lets an agent read channelID.
func agentReadAllowed(m schema.AgentManifestV1, channelID int64) bool {
	return m.Allows(schema.CapabilityMessagesRead) && m.AllowsChannel(channelID, schema.ChannelPermissionRead)
}

// dropAgentSubscriptionsRevokedBy closes the agent's live WebSocket
// subscriptions on every channel that manifest m no longer lets it read. It is
// called after a manifest is written. If the agent's channels cannot be
// listed, every subscription it holds is closed instead (fail closed); a
// client that reconnects is checked against the new manifest.
func (s *Server) dropAgentSubscriptionsRevokedBy(ctx context.Context, agentID int64, m schema.AgentManifestV1) {
	channels, err := s.store.ListChannelsForPrincipal(context.WithoutCancel(ctx), agentID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: list agent channels failed; closing all its subscriptions", "principal", agentID, "error", err)
		s.hub.DropPrincipalAll(agentID)
		return
	}
	for _, c := range channels {
		if !agentReadAllowed(m, c.ID) {
			s.hub.DropPrincipal(c.ID, agentID)
		}
	}
}

// agentMayNoLongerRead reports whether principalID is an agent whose manifest
// does not (or no longer) let it read channelID. It only chooses a WebSocket
// close reason, so any error answers false and nothing is audited.
func (s *Server) agentMayNoLongerRead(ctx context.Context, principalID, channelID int64) bool {
	p, err := s.store.PrincipalByID(ctx, principalID)
	if err != nil || p.Kind != store.PrincipalAgent {
		return false
	}
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return false
	}
	return reason != "" || !agentReadAllowed(m, channelID)
}

// humansOnly refuses an agent caller on a route that is the human surface for
// approvals. Agents request and observe approvals through MCP, where the
// manifest and membership gates apply; letting an agent credential use the
// REST approval routes would bypass them. With no caller (AuthOff) or a human
// caller the handler runs unchanged.
func (s *Server) humansOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller, ok := callerFrom(r.Context()); ok && caller.Kind == store.PrincipalAgent {
			subject := r.Pattern
			if subject == "" {
				subject = "<unmatched>"
			}
			s.auditAgentDenial(r.Context(), caller.ID, subject, "", 0, denyAgentOnHumanRoute)
			writeError(w, http.StatusForbidden, errForbidden.Code, "agents use the MCP endpoint for approvals")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logAgentsWithoutManifest says, once at startup, how many agent principals
// have no manifest. Upgrading is deny-by-default, so those agents can do
// nothing until an operator writes one.
func (s *Server) logAgentsWithoutManifest(ctx context.Context) {
	n, err := s.store.CountAgentsWithoutManifest(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "authz: count agents without a manifest failed", "error", err)
		return
	}
	if n > 0 {
		slog.Warn("authz: agent principals without a manifest can do nothing until one is written with PUT /v1/principals/{id}/manifest", "agents", n)
	}
}
