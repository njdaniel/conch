package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
// agentManifestCheck for agent callers, so no path acts for an agent
// principal without passing through it.

// Denial reasons recorded in the audit log.
const (
	denyUnmappedTool      = "unmapped_tool"
	denyNoManifest        = "no_manifest"
	denyInvalidManifest   = "invalid_manifest"
	denyCapability        = "capability_not_granted"
	denyNotMember         = "not_a_member"
	denyChannelPermission = "channel_permission_not_granted"
	// denyAudienceGrant is a scoped post without the grant for that kind of
	// audience: post_net, whisper or whisper_agent (issue #117). It is its own
	// reason so the audit log tells "may not post here" from "may post here,
	// but not to that audience".
	denyAudienceGrant = "audience_not_granted"
	// denyNetMonitorOnly and denyNetNotOn are an agent's scoped post refused
	// by the net's roster rather than by its manifest (issue #149): it only
	// monitors the net, or it is not on it. "Not on it" also covers a net
	// that is archived or does not exist: the store does not tell these
	// apart, on purpose, and neither does the audit row, which carries no
	// net id (an agent walking net ids must not fill the log with numbers of
	// its choosing).
	denyNetMonitorOnly    = "net_monitor_only"
	denyNetNotOn          = "net_not_on"
	denyAgentOnHumanRoute = "agents_use_mcp"
	denyAgentVoice        = "agents_no_voice"
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
	// Repeated identical denials from one principal are thinned out; the
	// first of each kind is written at once (issue #80, denialCoalescer).
	write, count := s.denials.admit(principalID, subject+"|"+detail, s.now())
	if !write {
		return
	}
	detail = denialDetail(detail, count)
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

// agentManifestCheck is the manifest gate alone: does the agent's manifest
// grant capability, and permission in channel channelID? Denials are audited
// under subject. A store failure is reported as an error and is not an allow.
// The channel is required: a caller with no channel to name is refused rather
// than checked on capability alone.
//
// It also returns the manifest it decided on, so the caller can spend the rate
// limit from the same read (issue #80). Every caller does: a check that is not
// followed by spendAgentBudget is a way round the agent's limit.
func (s *Server) agentManifestCheck(ctx context.Context, principalID int64, subject string, capability schema.Capability, channelID int64, permission schema.ChannelPermission) (schema.AgentManifestV1, bool, error) {
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return schema.AgentManifestV1{}, false, err
	}
	switch {
	case reason != "":
	case !m.Allows(capability):
		reason = denyCapability
	case channelID <= 0 || !m.AllowsChannel(channelID, permission):
		reason = denyChannelPermission
		if isAudiencePermission(permission) {
			reason = denyAudienceGrant
		}
	}
	if reason != "" {
		s.auditAgentDenial(ctx, principalID, subject, capability, channelID, reason)
		return schema.AgentManifestV1{}, false, nil
	}
	return m, true, nil
}

// agentSelfCheck is the manifest gate for a call about the agent's own
// session: does the manifest grant capability? It checks the capability and
// nothing else. No channel is involved, so it must not be mistaken for a
// weaker agentManifestCheck: that one refuses a call that names no channel, on
// purpose, and still does (issue #265, decision D9).
//
// Because it asks so little, it may be called only for calls that act on the
// caller's own session and reach nothing of anyone else's; that is pinned by
// TestAgentSelfCheckCallers, which fails if any function but the named ones
// calls it. Denials are audited under subject like agentManifestCheck's, and a
// store failure is an error, not an allow. It returns the manifest it decided
// on, and its caller must go on to spend the capability's budget with
// spendAgentBudget, as every other check's caller does.
func (s *Server) agentSelfCheck(ctx context.Context, principalID int64, subject string, capability schema.Capability) (schema.AgentManifestV1, bool, error) {
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return schema.AgentManifestV1{}, false, err
	}
	if reason == "" && !m.Allows(capability) {
		reason = denyCapability
	}
	if reason != "" {
		s.auditAgentDenial(ctx, principalID, subject, capability, 0, reason)
		return schema.AgentManifestV1{}, false, nil
	}
	return m, true, nil
}

// agentSelfAllowed applies agentSelfCheck and the rate limit to a REST request
// made with an agent's credential, for the session routes. It reports true for
// callers that are not agents. When it reports false it has written the
// response.
func (s *Server) agentSelfAllowed(w http.ResponseWriter, r *http.Request, capability schema.Capability) bool {
	caller, ok := callerFrom(r.Context())
	if !ok || caller.Kind != store.PrincipalAgent {
		return true
	}
	subject := r.Pattern
	if subject == "" {
		subject = "<unmatched>"
	}
	m, allowed, err := s.agentSelfCheck(r.Context(), caller.ID, subject, capability)
	if err != nil {
		slog.ErrorContext(r.Context(), "authz: load manifest failed", "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
		return false
	}
	// Last, so only a call the gate allowed uses budget (issue #80).
	if limited := s.spendAgentBudget(r.Context(), caller.ID, subject, m, capability); limited != nil {
		writeRateLimited(w, limited)
		return false
	}
	return true
}

// isAudiencePermission reports whether permission grants an audience for a
// scoped post rather than access to the channel as a whole.
func isAudiencePermission(permission schema.ChannelPermission) bool {
	switch permission {
	case schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent:
		return true
	default:
		return false
	}
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
	// spent says this call has already used its rate-limit budget. A scope is
	// one tool call, and await_decision looks the approval up on every poll;
	// the call is one use, not one per poll.
	spent bool
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
	return a.spend(ctx)
}

// spend uses one of the call's budget once both gates have allowed it, so a
// refused call never costs anything (issue #80).
func (a *agentScope) spend(ctx context.Context) *schema.Error {
	if a.spent {
		return nil
	}
	if limited := a.s.spendAgentBudget(ctx, a.identity.principalID, a.subject, a.manifest, a.capability); limited != nil {
		return limited.schemaError()
	}
	a.spent = true
	return nil
}

// grantedChannel is a channel the agent has been authorized to use with one
// permission. It exists only as the result of a scope lookup.
type grantedChannel struct {
	scope   *agentScope
	channel store.Channel
	// audience is set only by channelForAudience: the audience the agent was
	// authorized to address. insertScopedMessage posts to exactly it.
	audience *schema.Audience
}

// insertMessage stores a message in the channel, authored by the agent.
func (g *grantedChannel) insertMessage(ctx context.Context, body string, payload *schema.Payload) (store.Message, error) {
	msg, err := g.scope.s.store.InsertMessageV1(ctx, g.channel.ID, g.scope.identity.principalID, body, payload)
	if errors.Is(err, store.ErrFileAttachment) {
		// A refusal of the post, not a failure of the store; the tools tell
		// the two apart by this error, without reaching into the store
		// themselves (issue #240).
		return store.Message{}, errAttachmentRefused
	}
	return msg, err
}

// errAttachmentRefused is insertMessage's error for a post whose file
// attachment cannot be honoured (an agent cannot upload, so for an agent that
// is every attachment). Nothing was stored.
var errAttachmentRefused = errors.New("server: file attachment refused")

// listMessages reads one page of the channel's messages as the agent: the
// channel-wide ones, and the scoped ones it is a recipient of. The MCP tools
// speak the v2 envelope (issue #117), so they can be given a message with an
// audience; the visibility function decides which.
func (g *grantedChannel) listMessages(ctx context.Context, after int64, limit int) ([]store.Message, error) {
	return g.scope.s.store.ListVisibleMessages(ctx, g.channel.ID, g.scope.reader(), after, limit)
}

// insertScopedMessage stores a message for the audience this grant was
// authorized for, and returns it with the recipients resolved at post time,
// for fan-out. The author is the agent and the audience is the grant's: a tool
// body cannot post as another principal or widen the audience after the check.
// The refusals are computed by storeScopedPost, the same function the REST
// post uses.
func (g *grantedChannel) insertScopedMessage(ctx context.Context, body string, payload *schema.Payload) (schema.MessageV2, []int64, *schema.Error) {
	if g.audience == nil {
		slog.ErrorContext(ctx, "authz: scoped insert on a grant without an audience")
		return schema.MessageV2{}, nil, errInternal
	}
	return g.scope.s.storeScopedPost(ctx, store.ScopedPost{
		ChannelID: g.channel.ID, AuthorID: g.scope.identity.principalID, Body: body, Payload: payload, Audience: *g.audience,
	}, g.scope.subject)
}

// createApproval raises an approval in the channel on the agent's behalf. The
// requester and the channel come from the grant, never from req, so a tool
// body cannot raise an approval as another principal or in a channel it was
// not granted.
func (g *grantedChannel) createApproval(ctx context.Context, req schema.CreateApprovalRequestV1, deadline time.Time) (store.Approval, error) {
	quorum := req.Quorum
	if quorum == 0 {
		quorum = 1
	}
	return g.scope.s.approvals.Create(ctx, store.ApprovalParams{
		RequesterID: g.scope.identity.principalID,
		ChannelID:   g.channel.ID,
		Title:       req.Title,
		Body:        req.Body,
		Payload:     req.Payload,
		Options:     req.Options,
		Deadline:    deadline,
		Quorum:      quorum,
		Escalation:  req.EscalationTarget,
	})
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

// reader is the visibility reader for this agent: its authenticated identity,
// which is the only thing a scope is ever built from, on a protocol that can
// express an audience. MCP always authenticates, so there is always one.
func (a *agentScope) reader() store.Reader {
	return store.Reader{PrincipalID: a.identity.principalID, Scoped: true}
}

// channelForAudience resolves the channel named name for a post addressed to
// aud, which must already be normalized. The order matches the REST post: an
// unknown channel and one the agent is not a member of are the same answer;
// then the agent's manifest must grant that kind of audience there
// (authorizeScopedPost, the one policy function); the net itself is resolved
// later, with the agent's seat, by the store. The capability for the tool was
// checked when the scope was made.
func (a *agentScope) channelForAudience(ctx context.Context, name string, aud schema.Audience) (*grantedChannel, *schema.Error) {
	channel, err := a.s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return nil, errInternal
	}
	member, err := a.s.store.IsChannelMember(ctx, channel.ID, a.identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: check membership failed", "error", err)
		return nil, errInternal
	}
	if !member {
		a.s.auditAgentDenial(ctx, a.identity.principalID, a.subject, a.capability, channel.ID, denyNotMember)
		return nil, errChannelNotFound
	}
	// An MCP identity is always an agent (authenticateMCP admits no other
	// kind), so the manifest rules always apply here; nothing a store row says
	// can turn them off.
	agent := store.Principal{ID: a.identity.principalID, Kind: store.PrincipalAgent}
	allowed, err := a.s.authorizeScopedPost(ctx, agent, a.subject, channel.ID, aud)
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		return nil, limited.schemaError()
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: authorize scoped post failed", "error", err)
		return nil, errInternal
	}
	if !allowed {
		return nil, errForbidden
	}
	return &grantedChannel{scope: a, channel: channel, audience: &aud}, nil
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
	m, allowed, err := s.agentManifestCheck(r.Context(), caller.ID, subject, capability, channelID, permission)
	if err != nil {
		slog.ErrorContext(r.Context(), "authz: load manifest failed", "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
		return false
	}
	// Last, so only a call every gate allowed uses budget (issue #80).
	if limited := s.spendAgentBudget(r.Context(), caller.ID, subject, m, capability); limited != nil {
		writeRateLimited(w, limited)
		return false
	}
	return true
}

// authorizeScopedPost decides whether caller may post a message with audience
// aud, already normalized, in channelID. It is the one place the policy lives,
// so the MCP tools can reuse it (issue #117).
//
// Only agents are restricted: a human who is a member of the channel may post
// to a net they are on or whisper to channel members, and the store enforces
// those rules. An agent needs the matching grant in the channel: post_net for
// a net; whisper for a whisper; and, when any target other than the agent
// itself is an agent, whisper_agent as well. The channel permission "post"
// grants none of these, because the permissions are independent. Every
// refusal is audited once under subject, and a refusal never reaches the
// store. A store failure is an error and not an allow.
func (s *Server) authorizeScopedPost(ctx context.Context, caller store.Principal, subject string, channelID int64, aud schema.Audience) (bool, error) {
	if caller.Kind != store.PrincipalAgent {
		return true, nil
	}
	m, allowed, err := s.authorizeScopedPostGates(ctx, caller, subject, channelID, aud)
	if err != nil || !allowed {
		return false, err
	}
	// Once, and last: a post that any gate refused costs no budget, and the
	// manifest checks above are one post, not several uses (issue #80). The
	// rate limit is returned as the error, a *rateLimitedError, for callers to
	// answer 429.
	if limited := s.spendAgentBudget(ctx, caller.ID, subject, m, schema.CapabilityMessagesPost); limited != nil {
		return false, limited
	}
	return true, nil
}

// authorizeScopedPostGates is the manifest and roster part of
// authorizeScopedPost, for an agent caller. It returns the manifest it decided
// on.
func (s *Server) authorizeScopedPostGates(ctx context.Context, caller store.Principal, subject string, channelID int64, aud schema.Audience) (schema.AgentManifestV1, bool, error) {
	needed := schema.ChannelPermissionPostNet
	if aud.Kind == schema.AudienceKindPrincipals {
		needed = schema.ChannelPermissionWhisper
	}
	m, allowed, err := s.agentManifestCheck(ctx, caller.ID, subject, schema.CapabilityMessagesPost, channelID, needed)
	if err != nil || !allowed {
		return schema.AgentManifestV1{}, false, err
	}
	if aud.Kind != schema.AudienceKindPrincipals {
		return m, true, nil
	}
	for _, id := range aud.PrincipalIDs {
		if id == caller.ID {
			continue
		}
		// Only a current member of the channel can be a whisper target, and
		// the store refuses any other with one answer, invalid_audience. So a
		// target who is not a member is left to the store, whatever kind of
		// principal it is. Looking at its kind first would answer "forbidden"
		// for an agent and "invalid audience" for a human or an unknown id,
		// and so tell the caller which principal ids exist and which are
		// agents, for principals it shares no channel with.
		member, err := s.store.IsChannelMember(ctx, channelID, id)
		if err != nil {
			return schema.AgentManifestV1{}, false, err
		}
		if !member {
			continue
		}
		target, err := s.store.PrincipalByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return schema.AgentManifestV1{}, false, err
		}
		if target.Kind == store.PrincipalAgent {
			return s.agentManifestCheck(ctx, caller.ID, subject, schema.CapabilityMessagesPost, channelID, schema.ChannelPermissionWhisperAgent)
		}
	}
	return m, true, nil
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
		s.events.dropPrincipalAll(agentID)
		return
	}
	for _, c := range channels {
		if !agentReadAllowed(m, c.ID) {
			s.hub.DropPrincipal(c.ID, agentID)
			s.events.dropPrincipal(c.ID, agentID)
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
	return s.humansOnlyBecause(denyAgentOnHumanRoute, "agents use the MCP endpoint for approvals", next)
}

// humansOnlyFiles refuses an agent caller on a file route (issue #23). The
// manifest has no capability for files, so an agent can neither upload nor
// fetch; the refusal comes before the handler looks at the channel or the file.
func (s *Server) humansOnlyFiles(next http.Handler) http.Handler {
	return s.humansOnlyBecause(denyAgentFiles, "agents cannot use files", next)
}

// humansOnlyBecause is humansOnly with the audit reason and the message of the
// refusal.
func (s *Server) humansOnlyBecause(reason, message string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller, ok := callerFrom(r.Context()); ok && caller.Kind == store.PrincipalAgent {
			subject := r.Pattern
			if subject == "" {
				subject = "<unmatched>"
			}
			s.auditAgentDenial(r.Context(), caller.ID, subject, "", 0, reason)
			writeError(w, http.StatusForbidden, errForbidden.Code, message)
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
