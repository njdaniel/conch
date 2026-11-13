package schema

import (
	"errors"
	"fmt"
	"strings"
)

// AgentManifestSchemaV1 is the versioned name identifying the v1 agent
// capability manifest on the wire. Every AgentManifestV1 carries it in its
// Schema field so a decoder can recognize the version without out-of-band
// context.
const AgentManifestSchemaV1 = "conch.agent_manifest.v1"

// AgentTier is the manifest's tier tag (ADR-000 D10). It is a label carried
// for operators, audit, and later policy; this schema attaches no permission
// to it. Nothing is granted by tier: an agent's permissions are exactly its
// declared capabilities and channel grants.
type AgentTier string

// Agent tiers.
const (
	AgentTierC AgentTier = "C"
	AgentTierA AgentTier = "A"
	AgentTierH AgentTier = "H"
)

// Valid reports whether t is a recognized tier tag.
func (t AgentTier) Valid() bool {
	switch t {
	case AgentTierC, AgentTierA, AgentTierH:
		return true
	default:
		return false
	}
}

// Capability names one server-side permission an agent principal may be
// granted (ADR-000 D10). The vocabulary is closed: a value outside it fails
// validation. It grows only by adding constants (agent-manifest.md §6).
//
// Names describe the permission, not the MCP tool that exercises it; the tool
// to capability mapping is MCPToolCapability. There is no wildcard and no
// capability that implies another.
type Capability string

// Capabilities. Message and approval capabilities are independent: holding
// one never implies the other, and each approval operation is its own grant.
const (
	// CapabilityMessagesRead permits reading channel message history.
	CapabilityMessagesRead Capability = "messages.read"
	// CapabilityMessagesPost permits posting messages to a channel.
	CapabilityMessagesPost Capability = "messages.post"
	// CapabilityApprovalsRequest permits raising an approval.
	CapabilityApprovalsRequest Capability = "approvals.request"
	// CapabilityApprovalsAwait permits blocking on an approval's resolution.
	CapabilityApprovalsAwait Capability = "approvals.await"
	// CapabilityApprovalsCheck permits polling an approval's current state.
	CapabilityApprovalsCheck Capability = "approvals.check"
	// CapabilitySessionsManage permits an agent to register, keep alive and
	// end its own sessions and to read the interventions on its own runs
	// (P3, decision D9). It is the one capability checked without a channel.
	CapabilitySessionsManage Capability = "sessions.manage"
	// CapabilityRunsManage permits an agent to start runs, change their
	// state, append activity and answer interventions. Each of those also
	// needs the channel permission that posting to the run's audience needs.
	CapabilityRunsManage Capability = "runs.manage"
	// CapabilityAdminAct permits an agent to use the admin door over MCP
	// (issue #1326, docs/design/ai-administrators.md §3.6): to read the
	// action catalog, submit an action and run an approved one. It says only
	// that the agent may use the door; what it may do through it is the
	// Permissions of its manifest, and a manifest with neither grants no
	// administration. Like sessions.manage it is checked without a channel.
	CapabilityAdminAct Capability = "admin.act"
	// CapabilityFilesUpload permits an agent to upload an image to a channel
	// (issue #1798): over REST, or as an attachment of post_message. It needs
	// the channel permission attach there as well, and membership. It grants
	// no reading: an agent is refused on every route that fetches a file, with
	// or without it.
	CapabilityFilesUpload Capability = "files.upload"
)

// Valid reports whether c is a recognized capability.
func (c Capability) Valid() bool {
	switch c {
	case CapabilityMessagesRead, CapabilityMessagesPost,
		CapabilityApprovalsRequest, CapabilityApprovalsAwait, CapabilityApprovalsCheck,
		CapabilitySessionsManage, CapabilityRunsManage, CapabilityAdminAct,
		CapabilityFilesUpload:
		return true
	default:
		return false
	}
}

// Capabilities returns the full capability vocabulary in declaration order.
// The slice is freshly allocated; callers may modify it.
func Capabilities() []Capability {
	return []Capability{
		CapabilityMessagesRead,
		CapabilityMessagesPost,
		CapabilityApprovalsRequest,
		CapabilityApprovalsAwait,
		CapabilityApprovalsCheck,
		CapabilitySessionsManage,
		CapabilityRunsManage,
		CapabilityAdminAct,
		CapabilityFilesUpload,
	}
}

// mcpToolCapabilities maps every MCP tool conchd exposes to the single
// capability a caller must hold to invoke it. A tool absent from this table
// has no capability and therefore cannot be authorized (fail closed). The
// session and run tools (issue #267) front the REST routes that need the same
// capabilities.
//
// The three admin tools of issue #1326 (describe_admin_actions,
// submit_admin_action, run_approved_action) map to CapabilityAdminAct, as
// agent-manifest.md §6 requires: the server's structural tests hold this
// table and the registered tools to the same set.
//
// post_message stays one tool with one capability. A call that carries
// attachments (issue #1798) needs CapabilityFilesUpload as well, and the
// channel permission attach. The server checks both inside the tool: this
// table holds one capability for each tool, and cannot say "and another, for
// this argument".
var mcpToolCapabilities = map[string]Capability{
	"post_message":     CapabilityMessagesPost,
	"read_channel":     CapabilityMessagesRead,
	"request_approval": CapabilityApprovalsRequest,
	"await_decision":   CapabilityApprovalsAwait,
	"check_decision":   CapabilityApprovalsCheck,

	// Message reactions (issue #765, docs/design/message-reactions.md §4.2):
	// setting and removing one's own mark is a small post, reading them is a
	// read. The audience permission a write also needs (post, post_net or
	// whisper) is decided by the server from the message's audience.
	"set_reaction":    CapabilityMessagesPost,
	"remove_reaction": CapabilityMessagesPost,
	"read_reactions":  CapabilityMessagesRead,
	"list_reactors":   CapabilityMessagesRead,

	"register_session":   CapabilitySessionsManage,
	"heartbeat":          CapabilitySessionsManage,
	"end_session":        CapabilitySessionsManage,
	"start_run":          CapabilityRunsManage,
	"update_run":         CapabilityRunsManage,
	"post_activity":      CapabilityRunsManage,
	"poll_interventions": CapabilityRunsManage,
	"ack_intervention":   CapabilityRunsManage,

	// The admin door (issue #1326, docs/design/ai-administrators.md §3.6):
	// one capability says the agent may use the door; the permissions its
	// manifest grants say what it may do through it.
	"describe_admin_actions": CapabilityAdminAct,
	"submit_admin_action":    CapabilityAdminAct,
	"run_approved_action":    CapabilityAdminAct,
}

// MCPToolCapability returns the capability required to call the named MCP
// tool. ok is false for a tool with no mapping; enforcement must treat that as
// a denial, never as "no capability required".
func MCPToolCapability(tool string) (c Capability, ok bool) {
	c, ok = mcpToolCapabilities[tool]
	return c, ok
}

// MCPToolCapabilities returns a copy of the complete MCP tool name to
// capability table.
func MCPToolCapabilities() map[string]Capability {
	out := make(map[string]Capability, len(mcpToolCapabilities))
	for tool, c := range mcpToolCapabilities {
		out[tool] = c
	}
	return out
}

// ChannelPermission names one thing an agent may do in a specific channel.
// The vocabulary is closed and grows only by adding constants.
type ChannelPermission string

// Channel permissions. They are independent: none implies another. The
// scoped-speaking permissions (ADR-005) gate where an agent may transmit;
// reading a scoped message needs only read plus being in its audience.
const (
	// ChannelPermissionRead permits reading the channel's contents, including
	// scoped messages whose audience the agent is in.
	ChannelPermissionRead ChannelPermission = "read"
	// ChannelPermissionPost permits posting channel-wide: a message with no
	// audience.
	ChannelPermissionPost ChannelPermission = "post"
	// ChannelPermissionPostNet permits posting to a net in the channel that
	// the agent is a member (not merely a monitor) of.
	ChannelPermissionPostNet ChannelPermission = "post_net"
	// ChannelPermissionWhisper permits posting to an explicit list of human
	// principals in the channel.
	ChannelPermissionWhisper ChannelPermission = "whisper"
	// ChannelPermissionWhisperAgent permits the whisper list to include other
	// agents. It is a separate grant for the same reason reply-to-agent is:
	// agent-to-agent traffic nobody reads is its own risk.
	ChannelPermissionWhisperAgent ChannelPermission = "whisper_agent"
	// ChannelPermissionAttach permits uploading an image to the channel
	// (issue #1798), together with the capability files.upload. It is not a
	// way to post: the message that carries the image needs its own
	// permission (post, post_net or whisper), and attach implies none of them.
	ChannelPermissionAttach ChannelPermission = "attach"
)

// Valid reports whether p is a recognized channel permission.
func (p ChannelPermission) Valid() bool {
	switch p {
	case ChannelPermissionRead, ChannelPermissionPost,
		ChannelPermissionPostNet, ChannelPermissionWhisper, ChannelPermissionWhisperAgent,
		ChannelPermissionAttach:
		return true
	default:
		return false
	}
}

// ChannelPermissions returns the full channel permission vocabulary in
// declaration order. The slice is freshly allocated; callers may modify it.
func ChannelPermissions() []ChannelPermission {
	return []ChannelPermission{
		ChannelPermissionRead,
		ChannelPermissionPost,
		ChannelPermissionPostNet,
		ChannelPermissionWhisper,
		ChannelPermissionWhisperAgent,
		ChannelPermissionAttach,
	}
}

// channelPermissionNames renders the vocabulary for error messages.
func channelPermissionNames() string {
	all := ChannelPermissions()
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

// ChannelGrant is an agent's permission set in one channel. A channel with no
// grant is denied entirely. Permissions are independent: post does not imply
// read.
type ChannelGrant struct {
	// ChannelID references the channel the grant applies to.
	ChannelID int64 `json:"channel_id"`
	// Permissions is the non-empty set of permissions held in that channel.
	Permissions []ChannelPermission `json:"permissions"`
}

// Validate reports whether the grant is structurally well-formed.
func (g ChannelGrant) Validate() error {
	if g.ChannelID <= 0 {
		return fmt.Errorf("schema: channel grant channel_id must be positive, got %d", g.ChannelID)
	}
	if len(g.Permissions) == 0 {
		return fmt.Errorf("schema: channel grant for channel %d must list at least one permission", g.ChannelID)
	}
	seen := make(map[ChannelPermission]struct{}, len(g.Permissions))
	for _, p := range g.Permissions {
		if !p.Valid() {
			return fmt.Errorf("schema: channel permission %q is not one of %s", p, channelPermissionNames())
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("schema: channel grant for channel %d lists permission %q more than once", g.ChannelID, p)
		}
		seen[p] = struct{}{}
	}
	return nil
}

// ErrorCodeRateLimited is the Error.Code of a call refused because the agent
// has used its RateLimit for that capability (HTTP 429 over REST). The
// refusal spends nothing: the call may be retried once the window has
// moved on.
const ErrorCodeRateLimited = "rate_limited"

// RateLimit caps how often an agent may exercise one capability: at most Max
// uses in any window of WindowSeconds. The manifest only declares the policy;
// counting and backpressure belong to the server (issue #80).
type RateLimit struct {
	// Capability is the capability this limit budgets.
	Capability Capability `json:"capability"`
	// Max is the number of uses allowed per window. Must be positive.
	Max int `json:"max"`
	// WindowSeconds is the window length in seconds. Must be positive.
	WindowSeconds int `json:"window_seconds"`
}

// Validate reports whether the limit is structurally well-formed. A zero or
// negative Max or WindowSeconds is rejected: "unlimited" is spelled only by
// omitting the entry, and "none at all" only by not granting the capability.
func (l RateLimit) Validate() error {
	if !l.Capability.Valid() {
		return fmt.Errorf("schema: rate limit capability %q is not a known capability", l.Capability)
	}
	if l.Max <= 0 {
		return fmt.Errorf("schema: rate limit max for %q must be positive, got %d", l.Capability, l.Max)
	}
	if l.WindowSeconds <= 0 {
		return fmt.Errorf("schema: rate limit window_seconds for %q must be positive, got %d", l.Capability, l.WindowSeconds)
	}
	return nil
}

// Bounds and defaults of AgentAdminV1 (docs/design/ai-administrators.md
// §3.1, §3.8). A zero field on the wire means the default; the server fills
// it in when it answers the catalog.
const (
	// DefaultAdminApprovalTTLSeconds is the deadline of a gated action's
	// approval, from its creation, when the manifest sets none: a day.
	DefaultAdminApprovalTTLSeconds int64 = 86400
	// MinAdminApprovalTTLSeconds is the shortest approval deadline: five
	// minutes.
	MinAdminApprovalTTLSeconds int64 = 300
	// MaxAdminApprovalTTLSeconds is the longest: a week.
	MaxAdminApprovalTTLSeconds int64 = 604800
	// DefaultAdminRunWindowSeconds is how long after an approval resolves
	// approved its action may still be run, when the manifest sets none: a
	// day.
	DefaultAdminRunWindowSeconds int64 = 86400
	// MinAdminRunWindowSeconds is the shortest run window: a minute.
	MinAdminRunWindowSeconds int64 = 60
	// MaxAdminRunWindowSeconds is the longest: a week.
	MaxAdminRunWindowSeconds int64 = 604800
	// DefaultAdminQuorum is the approval's quorum when the manifest sets
	// none.
	DefaultAdminQuorum = 1
	// DefaultAdminMaxPending is how many gated approvals an agent may have
	// open at once when the manifest sets none.
	DefaultAdminMaxPending = 8
	// MaxAdminMaxPending is the most a manifest may allow.
	MaxAdminMaxPending = 32
)

// AgentAdminV1 configures how an AI administrator's gated actions are
// approved (issue #1326, docs/design/ai-administrators.md §3.1). It is
// required on a manifest whose Permissions is non-empty, since an agent with
// a grant and nowhere to ask is refused on write, and refused on one whose
// Permissions is empty, so that a manifest has one spelling.
type AgentAdminV1 struct {
	// ApprovalChannelID is the channel the server raises this agent's
	// gated actions in. The agent must be a member with read there (it
	// watches its approvals through await_decision and check_decision),
	// and it may never change that channel's roster or rights (§3.5.6).
	ApprovalChannelID int64 `json:"approval_channel_id"`
	// ApprovalTTLSeconds is the deadline of each gated action's approval,
	// from its creation. Default 86400; min 300; max 604800.
	ApprovalTTLSeconds int64 `json:"approval_ttl_seconds,omitempty"`
	// RunWindowSeconds is how long after an approval resolves approved the
	// action may still be run. Default 86400; min 60; max 604800.
	RunWindowSeconds int64 `json:"run_window_seconds,omitempty"`
	// Quorum is the approval's quorum. Default 1.
	Quorum int `json:"quorum,omitempty"`
	// EscalationTarget is copied onto each approval. Optional.
	EscalationTarget *EscalationTarget `json:"escalation_target,omitempty"`
	// RequireApproval adds actions to the gated set for this agent (§3.3).
	// It can only widen the server's fixed set, never narrow it.
	RequireApproval []AdminAction `json:"require_approval,omitempty"`
	// MaxPending caps this agent's open admin approvals. Default 8; max 32.
	MaxPending int `json:"max_pending,omitempty"`
}

// Validate reports whether the settings are well-formed: a positive approval
// channel, each bound within its range or zero for the default, a valid
// escalation target when present, and known actions listed at most once. It
// cannot check that the channel exists or that the agent reads it; the store
// does.
func (a AgentAdminV1) Validate() error {
	if a.ApprovalChannelID <= 0 {
		return fmt.Errorf("schema: agent admin approval_channel_id must be positive, got %d", a.ApprovalChannelID)
	}
	if a.ApprovalTTLSeconds != 0 && (a.ApprovalTTLSeconds < MinAdminApprovalTTLSeconds || a.ApprovalTTLSeconds > MaxAdminApprovalTTLSeconds) {
		return fmt.Errorf("schema: agent admin approval_ttl_seconds must be %d to %d, got %d", MinAdminApprovalTTLSeconds, MaxAdminApprovalTTLSeconds, a.ApprovalTTLSeconds)
	}
	if a.RunWindowSeconds != 0 && (a.RunWindowSeconds < MinAdminRunWindowSeconds || a.RunWindowSeconds > MaxAdminRunWindowSeconds) {
		return fmt.Errorf("schema: agent admin run_window_seconds must be %d to %d, got %d", MinAdminRunWindowSeconds, MaxAdminRunWindowSeconds, a.RunWindowSeconds)
	}
	if a.Quorum < 0 {
		return fmt.Errorf("schema: agent admin quorum must not be negative, got %d", a.Quorum)
	}
	if a.EscalationTarget != nil {
		if err := a.EscalationTarget.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[AdminAction]struct{}, len(a.RequireApproval))
	for _, action := range a.RequireApproval {
		if !action.Valid() {
			return fmt.Errorf("schema: agent admin require_approval action %q is not in the catalog", action)
		}
		if _, dup := seen[action]; dup {
			return fmt.Errorf("schema: agent admin require_approval lists %q more than once", action)
		}
		seen[action] = struct{}{}
	}
	if a.MaxPending < 0 || a.MaxPending > MaxAdminMaxPending {
		return fmt.Errorf("schema: agent admin max_pending must be 1 to %d, got %d", MaxAdminMaxPending, a.MaxPending)
	}
	return nil
}

// EffectiveApprovalTTLSeconds is ApprovalTTLSeconds, or the default when it
// is zero.
func (a AgentAdminV1) EffectiveApprovalTTLSeconds() int64 {
	if a.ApprovalTTLSeconds == 0 {
		return DefaultAdminApprovalTTLSeconds
	}
	return a.ApprovalTTLSeconds
}

// EffectiveRunWindowSeconds is RunWindowSeconds, or the default when it is
// zero.
func (a AgentAdminV1) EffectiveRunWindowSeconds() int64 {
	if a.RunWindowSeconds == 0 {
		return DefaultAdminRunWindowSeconds
	}
	return a.RunWindowSeconds
}

// EffectiveQuorum is Quorum, or the default when it is zero.
func (a AgentAdminV1) EffectiveQuorum() int {
	if a.Quorum == 0 {
		return DefaultAdminQuorum
	}
	return a.Quorum
}

// EffectiveMaxPending is MaxPending, or the default when it is zero.
func (a AgentAdminV1) EffectiveMaxPending() int {
	if a.MaxPending == 0 {
		return DefaultAdminMaxPending
	}
	return a.MaxPending
}

// RequiresApproval reports whether the manifest moves action to the gated
// set for this agent. It says nothing about the server's own gated set,
// which it can only widen.
func (a AgentAdminV1) RequiresApproval(action AdminAction) bool {
	for _, held := range a.RequireApproval {
		if held == action {
			return true
		}
	}
	return false
}

// AgentManifestV1 is the v1 wire representation of an agent capability
// manifest (ADR-000 D10, docs/design/agent-manifest.md): the complete
// declaration of what one agent principal may do. There is exactly one
// manifest per agent principal, addressed by PrincipalID.
//
// The manifest fails closed by construction. Every list defaults to empty and
// an empty list grants nothing: no capabilities, no channels, no permissions.
// There is no wildcard value and no field that widens access implicitly. The
// one list whose absence is permissive is RateLimits, where a capability with
// no entry is not rate limited by the manifest.
type AgentManifestV1 struct {
	// Schema is the manifest version discriminator; always
	// AgentManifestSchemaV1.
	Schema string `json:"schema"`
	// PrincipalID references the agent principal the manifest governs.
	PrincipalID int64 `json:"principal_id"`
	// DisplayName is the human-rendered name of the agent.
	DisplayName string `json:"display_name"`
	// Tier is the tier tag: C, A, or H.
	Tier AgentTier `json:"tier"`
	// Capabilities is the set of capabilities granted. Omitted or empty
	// grants nothing.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Channels is the per-channel permission set, at most one grant per
	// channel. A channel not listed is denied.
	Channels []ChannelGrant `json:"channels,omitempty"`
	// RateLimits is the rate-limit policy, at most one entry per capability.
	// A capability with no entry is not rate limited by this manifest.
	RateLimits []RateLimit `json:"rate_limits,omitempty"`
	// Permissions is the subset of operator permissions this agent holds
	// (ADR-014 as amended by #1326). Omitted or empty grants nothing. Only
	// values for which Permission.AgentGrantable() is true are accepted; the
	// store refuses a write that grants any permission by an actor who is not
	// an owner (ErrNotOwner), and audits every change (admin_grant_changed).
	Permissions []Permission `json:"permissions,omitempty"`
	// Admin configures how this agent's gated actions are approved. Required
	// when Permissions is non-empty (an agent with a grant and nowhere to ask
	// is refused on write); omitted otherwise.
	Admin *AgentAdminV1 `json:"admin,omitempty"`
	// CreatedAt is when the manifest was first stored (server-assigned).
	CreatedAt Timestamp `json:"created_at"`
	// UpdatedAt is when the manifest was last replaced (server-assigned).
	UpdatedAt Timestamp `json:"updated_at"`
}

// Validate reports whether the manifest is structurally well-formed: a known
// tier, capabilities, channel permissions and agent-grantable permissions,
// with no duplicates, and admin settings exactly when permissions are
// granted. It cannot check that the principal exists or is an agent, or that
// the channels exist; those are the store's job.
func (m AgentManifestV1) Validate() error {
	if m.Schema != AgentManifestSchemaV1 {
		return fmt.Errorf("schema: agent manifest schema must be %q, got %q", AgentManifestSchemaV1, m.Schema)
	}
	if m.PrincipalID <= 0 {
		return fmt.Errorf("schema: agent manifest principal_id must be positive, got %d", m.PrincipalID)
	}
	if err := validateManifestPolicy(m.DisplayName, m.Tier, m.Capabilities, m.Channels, m.RateLimits, m.Permissions, m.Admin); err != nil {
		return err
	}
	if m.CreatedAt.IsZero() {
		return errors.New("schema: agent manifest created_at is required")
	}
	if m.UpdatedAt.IsZero() {
		return errors.New("schema: agent manifest updated_at is required")
	}
	return nil
}

// Allows reports whether the manifest grants capability c. It is the only
// sanctioned capability test: membership in the declared list, nothing else.
func (m AgentManifestV1) Allows(c Capability) bool {
	for _, held := range m.Capabilities {
		if held == c {
			return true
		}
	}
	return false
}

// AllowsChannel reports whether the manifest grants permission p in the given
// channel. A channel with no grant is denied.
func (m AgentManifestV1) AllowsChannel(channelID int64, p ChannelPermission) bool {
	for _, g := range m.Channels {
		if g.ChannelID != channelID {
			continue
		}
		for _, held := range g.Permissions {
			if held == p {
				return true
			}
		}
	}
	return false
}

// RateLimitFor returns the declared limit for capability c. ok is false when
// the manifest declares none, meaning c is not rate limited by the manifest.
func (m AgentManifestV1) RateLimitFor(c Capability) (l RateLimit, ok bool) {
	for _, l := range m.RateLimits {
		if l.Capability == c {
			return l, true
		}
	}
	return RateLimit{}, false
}

// HoldsPermission reports whether the manifest grants operator permission p
// (docs/design/ai-administrators.md §3.2, rule 1a of principalHolds): p is
// listed and agent-grantable, and nothing else. No brake, cell, dependency or
// column applies, and no permission implies another. A permission that is
// not agent-grantable is never held, whatever the list says, so a stored
// manifest that predates a narrowing of AgentGrantable fails closed.
func (m AgentManifestV1) HoldsPermission(p Permission) bool {
	if !p.AgentGrantable() {
		return false
	}
	for _, held := range m.Permissions {
		if held == p {
			return true
		}
	}
	return false
}

// validateManifestPolicy checks the caller-supplied part of a manifest, shared
// by AgentManifestV1 and PutAgentManifestRequestV1 so a body that is accepted
// on write is one that validates on read.
func validateManifestPolicy(displayName string, tier AgentTier, caps []Capability, channels []ChannelGrant, limits []RateLimit, perms []Permission, admin *AgentAdminV1) error {
	if strings.TrimSpace(displayName) == "" {
		return errors.New("schema: agent manifest display_name is required")
	}
	if !tier.Valid() {
		return fmt.Errorf("schema: agent manifest tier %q is not one of C, A, H", tier)
	}
	seenCaps := make(map[Capability]struct{}, len(caps))
	for _, c := range caps {
		if !c.Valid() {
			return fmt.Errorf("schema: agent manifest capability %q is not a known capability", c)
		}
		if _, dup := seenCaps[c]; dup {
			return fmt.Errorf("schema: agent manifest lists capability %q more than once", c)
		}
		seenCaps[c] = struct{}{}
	}
	seenChannels := make(map[int64]struct{}, len(channels))
	for _, g := range channels {
		if err := g.Validate(); err != nil {
			return err
		}
		if _, dup := seenChannels[g.ChannelID]; dup {
			return fmt.Errorf("schema: agent manifest lists channel %d more than once", g.ChannelID)
		}
		seenChannels[g.ChannelID] = struct{}{}
	}
	seenLimits := make(map[Capability]struct{}, len(limits))
	for _, l := range limits {
		if err := l.Validate(); err != nil {
			return err
		}
		if _, dup := seenLimits[l.Capability]; dup {
			return fmt.Errorf("schema: agent manifest lists a rate limit for %q more than once", l.Capability)
		}
		seenLimits[l.Capability] = struct{}{}
	}
	seenPerms := make(map[Permission]struct{}, len(perms))
	for _, p := range perms {
		if !p.Valid() {
			return fmt.Errorf("schema: agent manifest permission %q is not in the catalog", p)
		}
		if !p.AgentGrantable() {
			return fmt.Errorf("schema: agent manifest permission %q cannot be granted to an agent", p)
		}
		if _, dup := seenPerms[p]; dup {
			return fmt.Errorf("schema: agent manifest lists permission %q more than once", p)
		}
		seenPerms[p] = struct{}{}
	}
	switch {
	case len(perms) > 0 && admin == nil:
		return errors.New("schema: agent manifest admin is required when permissions are granted")
	case len(perms) == 0 && admin != nil:
		return errors.New("schema: agent manifest admin is set but no permissions are granted")
	case admin != nil:
		return admin.Validate()
	}
	return nil
}

// PutAgentManifestRequestV1 is the request body for creating or replacing the
// manifest of one agent principal (PUT /v1/principals/{id}/manifest). It is a
// full replacement, never a merge: a list omitted from the body is stored
// empty, so access not restated is access removed.
//
// The principal comes from the URL and the schema name and timestamps from the
// server; a request carries none of them. Like PostMessageRequestV1 this is
// an API shape, not a persisted or registered payload.
type PutAgentManifestRequestV1 struct {
	// DisplayName is the human-rendered name of the agent.
	DisplayName string `json:"display_name"`
	// Tier is the tier tag: C, A, or H.
	Tier AgentTier `json:"tier"`
	// Capabilities is the set of capabilities granted. Omitted or empty
	// grants nothing.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Channels is the per-channel permission set. A channel not listed is
	// denied.
	Channels []ChannelGrant `json:"channels,omitempty"`
	// RateLimits is the rate-limit policy. A capability with no entry is not
	// rate limited by this manifest.
	RateLimits []RateLimit `json:"rate_limits,omitempty"`
	// Permissions is the subset of operator permissions this agent holds
	// (ADR-014 as amended by #1326). Omitted or empty grants nothing. Only
	// values for which Permission.AgentGrantable() is true are accepted; the
	// store refuses a write that grants any permission by an actor who is not
	// an owner (ErrNotOwner), and audits every change (admin_grant_changed).
	Permissions []Permission `json:"permissions,omitempty"`
	// Admin configures how this agent's gated actions are approved. Required
	// when Permissions is non-empty (an agent with a grant and nowhere to ask
	// is refused on write); omitted otherwise.
	Admin *AgentAdminV1 `json:"admin,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its rules
// are exactly AgentManifestV1's for every caller-supplied field.
func (r PutAgentManifestRequestV1) Validate() error {
	return validateManifestPolicy(r.DisplayName, r.Tier, r.Capabilities, r.Channels, r.RateLimits, r.Permissions, r.Admin)
}

// PutAgentManifestResponseV1 is the response body after a manifest is created
// or replaced. It embeds the full AgentManifestV1 as stored.
type PutAgentManifestResponseV1 struct {
	Manifest AgentManifestV1 `json:"manifest"`
}

// GetAgentManifestResponseV1 is the response body for reading one agent
// principal's manifest (GET /v1/principals/{id}/manifest).
type GetAgentManifestResponseV1 struct {
	Manifest AgentManifestV1 `json:"manifest"`
}
