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
)

// auditAgentDenial records one denied agent action. A failure to write the
// audit row is logged; the action stays denied either way.
func (s *Server) auditAgentDenial(ctx context.Context, principalID int64, subject string, capability schema.Capability, channelID int64, reason string) {
	target := "none"
	if channelID > 0 {
		target = fmt.Sprintf("channel:%d", channelID)
	}
	detail := fmt.Sprintf("capability=%s target=%s reason=%s", capability, target, reason)
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
// grant capability, and (when channelID is non-zero) permission in that
// channel? Denials are audited under subject. A store failure is reported as
// an error and is not an allow.
func (s *Server) agentManifestAllows(ctx context.Context, principalID int64, subject string, capability schema.Capability, channelID int64, permission schema.ChannelPermission) (bool, error) {
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return false, err
	}
	switch {
	case reason != "":
	case !m.Allows(capability):
		reason = denyCapability
	case channelID > 0 && !m.AllowsChannel(channelID, permission):
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
// only way a tool reaches a channel or an approval.
type agentScope struct {
	s           *Server
	principalID int64
	subject     string // "mcp:<tool>", the audit subject
	capability  schema.Capability
	manifest    schema.AgentManifestV1
}

// newAgentScope checks the capability gate for tool: the tool must be mapped
// to a capability and the agent's manifest must grant it. It returns a
// *schema.Error to hand back to the caller when the call is refused.
func (s *Server) newAgentScope(ctx context.Context, principalID int64, tool string) (*agentScope, *schema.Error) {
	subject := "mcp:" + tool
	capability, ok := schema.MCPToolCapability(tool)
	if !ok {
		// A tool registered without a capability mapping has no access.
		s.auditAgentDenial(ctx, principalID, subject, "", 0, denyUnmappedTool)
		return nil, errForbidden
	}
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: load manifest failed", "principal", principalID, "error", err)
		return nil, errInternal
	}
	if reason == "" && !m.Allows(capability) {
		reason = denyCapability
	}
	if reason != "" {
		s.auditAgentDenial(ctx, principalID, subject, capability, 0, reason)
		return nil, errForbidden
	}
	return &agentScope{s: s, principalID: principalID, subject: subject, capability: capability, manifest: m}, nil
}

// channel applies both gates to one channel: membership first (a non-member
// learns nothing), then the manifest's per-channel permission.
func (a *agentScope) channel(ctx context.Context, channel store.Channel, permission schema.ChannelPermission, notFound *schema.Error) *schema.Error {
	member, err := a.s.store.IsChannelMember(ctx, channel.ID, a.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: check membership failed", "principal", a.principalID, "error", err)
		return errInternal
	}
	if !member {
		a.s.auditAgentDenial(ctx, a.principalID, a.subject, a.capability, channel.ID, denyNotMember)
		return notFound
	}
	if !a.manifest.AllowsChannel(channel.ID, permission) {
		a.s.auditAgentDenial(ctx, a.principalID, a.subject, a.capability, channel.ID, denyChannelPermission)
		return errForbidden
	}
	return nil
}

// channelByName resolves a channel the agent may use with permission. An
// unknown channel and a channel the agent is not a member of are the same
// answer.
func (a *agentScope) channelByName(ctx context.Context, name string, permission schema.ChannelPermission) (store.Channel, *schema.Error) {
	channel, err := a.s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Channel{}, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return store.Channel{}, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errChannelNotFound); serr != nil {
		return store.Channel{}, serr
	}
	return channel, nil
}

// channelByID is channelByName for a channel id.
func (a *agentScope) channelByID(ctx context.Context, id int64, permission schema.ChannelPermission) (store.Channel, *schema.Error) {
	channel, err := a.s.store.ChannelByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Channel{}, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return store.Channel{}, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errChannelNotFound); serr != nil {
		return store.Channel{}, serr
	}
	return channel, nil
}

// approval resolves an approval the agent may observe: it must be a member of
// the approval's channel and its manifest must grant permission there. An
// unknown approval and one in a channel the agent is not in are the same
// answer. Being the requester grants nothing by itself: membership is the
// boundary, so an agent removed from a channel can no longer watch the
// approvals it raised there.
func (a *agentScope) approval(ctx context.Context, id int64, permission schema.ChannelPermission) (store.Approval, *schema.Error) {
	approval, err := a.s.store.ApprovalByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Approval{}, errApprovalMissing
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find approval failed", "error", err)
		return store.Approval{}, errInternal
	}
	channel, err := a.s.store.ChannelByID(ctx, approval.ChannelID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: find approval channel failed", "error", err)
		return store.Approval{}, errInternal
	}
	if serr := a.channel(ctx, channel, permission, errApprovalMissing); serr != nil {
		return store.Approval{}, serr
	}
	return approval, nil
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
