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
)

// Valid reports whether c is a recognized capability.
func (c Capability) Valid() bool {
	switch c {
	case CapabilityMessagesRead, CapabilityMessagesPost,
		CapabilityApprovalsRequest, CapabilityApprovalsAwait, CapabilityApprovalsCheck:
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
	}
}

// mcpToolCapabilities maps every MCP tool conchd exposes to the single
// capability a caller must hold to invoke it. A tool absent from this table
// has no capability and therefore cannot be authorized (fail closed).
var mcpToolCapabilities = map[string]Capability{
	"post_message":     CapabilityMessagesPost,
	"read_channel":     CapabilityMessagesRead,
	"request_approval": CapabilityApprovalsRequest,
	"await_decision":   CapabilityApprovalsAwait,
	"check_decision":   CapabilityApprovalsCheck,
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
)

// Valid reports whether p is a recognized channel permission.
func (p ChannelPermission) Valid() bool {
	switch p {
	case ChannelPermissionRead, ChannelPermissionPost,
		ChannelPermissionPostNet, ChannelPermissionWhisper, ChannelPermissionWhisperAgent:
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

// AgentManifestV1 is the v1 wire representation of an agent capability
// manifest (ADR-000 D10, docs/design/agent-manifest.md): the complete
// declaration of what one agent principal may do. There is exactly one
// manifest per agent principal, addressed by PrincipalID.
//
// The manifest fails closed by construction. Every list defaults to empty and
// an empty list grants nothing: no capabilities, no channels. There is no
// wildcard value and no field that widens access implicitly. The one list
// whose absence is permissive is RateLimits, where a capability with no entry
// is not rate limited by the manifest.
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
	// CreatedAt is when the manifest was first stored (server-assigned).
	CreatedAt Timestamp `json:"created_at"`
	// UpdatedAt is when the manifest was last replaced (server-assigned).
	UpdatedAt Timestamp `json:"updated_at"`
}

// Validate reports whether the manifest is structurally well-formed: a known
// tier, capabilities, and channel permissions, with no duplicates. It cannot
// check that the principal exists or is an agent, or that the channels exist;
// those are the store's job.
func (m AgentManifestV1) Validate() error {
	if m.Schema != AgentManifestSchemaV1 {
		return fmt.Errorf("schema: agent manifest schema must be %q, got %q", AgentManifestSchemaV1, m.Schema)
	}
	if m.PrincipalID <= 0 {
		return fmt.Errorf("schema: agent manifest principal_id must be positive, got %d", m.PrincipalID)
	}
	if err := validateManifestPolicy(m.DisplayName, m.Tier, m.Capabilities, m.Channels, m.RateLimits); err != nil {
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

// validateManifestPolicy checks the caller-supplied part of a manifest, shared
// by AgentManifestV1 and PutAgentManifestRequestV1 so a body that is accepted
// on write is one that validates on read.
func validateManifestPolicy(displayName string, tier AgentTier, caps []Capability, channels []ChannelGrant, limits []RateLimit) error {
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
}

// Validate reports whether the request is structurally well-formed. Its rules
// are exactly AgentManifestV1's for every caller-supplied field.
func (r PutAgentManifestRequestV1) Validate() error {
	return validateManifestPolicy(r.DisplayName, r.Tier, r.Capabilities, r.Channels, r.RateLimits)
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
