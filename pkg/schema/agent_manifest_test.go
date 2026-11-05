package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// validator is the subset of manifest wire shapes that carry a Validate
// method; the golden test validates each decoded fixture through it.
type validator interface {
	Validate() error
}

func TestAgentManifestGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"agent-manifest-v1.json", func() any { return new(AgentManifestV1) }},
		{"agent-manifest-v1-deny-all.json", func() any { return new(AgentManifestV1) }},
		{"agent-manifest-v1-nets.json", func() any { return new(AgentManifestV1) }},
		{"agent-manifest-v1-sessions.json", func() any { return new(AgentManifestV1) }},
		{"agent-manifest-v1-admin.json", func() any { return new(AgentManifestV1) }},
		{"put-agent-manifest-request-v1.json", func() any { return new(PutAgentManifestRequestV1) }},
		{"put-agent-manifest-request-v1-admin.json", func() any { return new(PutAgentManifestRequestV1) }},
		{"put-agent-manifest-response-v1.json", func() any { return new(PutAgentManifestResponseV1) }},
		{"get-agent-manifest-response-v1.json", func() any { return new(GetAgentManifestResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)

			// Decode again and validate: a published fixture must be a value
			// the schema itself accepts.
			raw, err := os.ReadFile(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixture name comes from this test's own table
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			value := tt.new()
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(value); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			var target validator
			switch v := value.(type) {
			case *PutAgentManifestResponseV1:
				target = v.Manifest
			case *GetAgentManifestResponseV1:
				target = v.Manifest
			case validator:
				target = v
			default:
				t.Fatalf("fixture type %T has no Validate", value)
			}
			if err := target.Validate(); err != nil {
				t.Errorf("fixture does not validate: %v", err)
			}
		})
	}
}

func validAgentManifest() AgentManifestV1 {
	return AgentManifestV1{
		Schema:      AgentManifestSchemaV1,
		PrincipalID: 3,
		DisplayName: "Leviathan",
		Tier:        AgentTierA,
		Capabilities: []Capability{
			CapabilityMessagesRead,
			CapabilityMessagesPost,
			CapabilityApprovalsRequest,
			CapabilityApprovalsAwait,
			CapabilityApprovalsCheck,
		},
		Channels: []ChannelGrant{
			{ChannelID: 7, Permissions: []ChannelPermission{ChannelPermissionRead, ChannelPermissionPost}},
			{ChannelID: 9, Permissions: []ChannelPermission{ChannelPermissionRead}},
		},
		RateLimits: []RateLimit{
			{Capability: CapabilityMessagesPost, Max: 60, WindowSeconds: 60},
			{Capability: CapabilityApprovalsRequest, Max: 10, WindowSeconds: 3600},
		},
		CreatedAt: NewTimestamp(time.Date(2026, 10, 5, 12, 34, 56, 789e6, time.UTC)),
		UpdatedAt: NewTimestamp(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)),
	}
}

// manifestPolicyCases are the validation rules shared by the manifest and its
// PUT request body. Each case mutates a valid manifest; the request test
// copies the mutated policy fields across, so both shapes are held to the
// same table.
var manifestPolicyCases = []struct {
	name    string
	mutate  func(*AgentManifestV1)
	wantErr string
}{
	{name: "valid", mutate: func(*AgentManifestV1) {}},
	{name: "valid tier C", mutate: func(m *AgentManifestV1) { m.Tier = AgentTierC }},
	{name: "valid tier H", mutate: func(m *AgentManifestV1) { m.Tier = AgentTierH }},
	{
		name: "valid deny-all: no capabilities, channels, or rate limits",
		mutate: func(m *AgentManifestV1) {
			m.Capabilities, m.Channels, m.RateLimits = nil, nil, nil
		},
	},
	{
		name:   "valid zero-value rate-limit policy: nil means not rate limited",
		mutate: func(m *AgentManifestV1) { m.RateLimits = nil },
	},
	{
		name:   "valid zero-value rate-limit policy: empty means not rate limited",
		mutate: func(m *AgentManifestV1) { m.RateLimits = []RateLimit{} },
	},
	{
		name: "valid rate limit for a capability that is not granted",
		mutate: func(m *AgentManifestV1) {
			m.Capabilities = []Capability{CapabilityMessagesRead}
		},
	},

	{name: "empty display name", mutate: func(m *AgentManifestV1) { m.DisplayName = "" }, wantErr: "display_name is required"},
	{name: "blank display name", mutate: func(m *AgentManifestV1) { m.DisplayName = " \t" }, wantErr: "display_name is required"},

	{name: "empty tier", mutate: func(m *AgentManifestV1) { m.Tier = "" }, wantErr: `tier "" is not one of C, A, H`},
	{name: "unknown tier", mutate: func(m *AgentManifestV1) { m.Tier = "B" }, wantErr: `tier "B" is not one of C, A, H`},
	{name: "lowercase tier", mutate: func(m *AgentManifestV1) { m.Tier = "a" }, wantErr: `tier "a" is not one of C, A, H`},

	{
		name:    "unknown capability",
		mutate:  func(m *AgentManifestV1) { m.Capabilities = append(m.Capabilities, "nets.transmit") },
		wantErr: `capability "nets.transmit" is not a known capability`,
	},
	{
		name:    "empty capability",
		mutate:  func(m *AgentManifestV1) { m.Capabilities = append(m.Capabilities, "") },
		wantErr: `capability "" is not a known capability`,
	},
	{
		name:    "wildcard capability",
		mutate:  func(m *AgentManifestV1) { m.Capabilities = []Capability{"*"} },
		wantErr: `capability "*" is not a known capability`,
	},
	{
		name:    "tool name is not a capability",
		mutate:  func(m *AgentManifestV1) { m.Capabilities = []Capability{"post_message"} },
		wantErr: `capability "post_message" is not a known capability`,
	},
	{
		name:    "duplicate capability",
		mutate:  func(m *AgentManifestV1) { m.Capabilities = append(m.Capabilities, CapabilityMessagesPost) },
		wantErr: `lists capability "messages.post" more than once`,
	},

	{
		name:    "zero channel id",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].ChannelID = 0 },
		wantErr: "channel_id must be positive, got 0",
	},
	{
		name:    "negative channel id",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].ChannelID = -7 },
		wantErr: "channel_id must be positive, got -7",
	},
	{
		name:    "duplicate channel",
		mutate:  func(m *AgentManifestV1) { m.Channels[1].ChannelID = 7 },
		wantErr: "lists channel 7 more than once",
	},
	{
		name:    "channel grant with no permissions",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].Permissions = nil },
		wantErr: "must list at least one permission",
	},
	{
		name: "valid scoped-speaking permissions (ADR-005)",
		mutate: func(m *AgentManifestV1) {
			m.Channels[0].Permissions = []ChannelPermission{
				ChannelPermissionRead, ChannelPermissionPost,
				ChannelPermissionPostNet, ChannelPermissionWhisper, ChannelPermissionWhisperAgent,
			}
		},
	},
	{
		name:   "valid post_net alone",
		mutate: func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{ChannelPermissionPostNet} },
	},
	{
		name:   "valid whisper alone",
		mutate: func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{ChannelPermissionWhisper} },
	},
	{
		name: "valid whisper_agent alone",
		mutate: func(m *AgentManifestV1) {
			m.Channels[0].Permissions = []ChannelPermission{ChannelPermissionWhisperAgent}
		},
	},
	{
		name:    "unknown channel permission",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{"admin"} },
		wantErr: `channel permission "admin" is not one of read, post, post_net, whisper, whisper_agent`,
	},
	{
		name:    "empty channel permission",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{""} },
		wantErr: `channel permission "" is not one of read, post, post_net, whisper, whisper_agent`,
	},
	{
		name:    "wildcard channel permission",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{"*"} },
		wantErr: `channel permission "*" is not one of read, post, post_net, whisper, whisper_agent`,
	},
	{
		name:    "capability-style spelling is not a channel permission",
		mutate:  func(m *AgentManifestV1) { m.Channels[0].Permissions = []ChannelPermission{"nets.post"} },
		wantErr: `channel permission "nets.post" is not one of read, post, post_net, whisper, whisper_agent`,
	},
	{
		name: "duplicate scoped-speaking permission",
		mutate: func(m *AgentManifestV1) {
			m.Channels[0].Permissions = []ChannelPermission{ChannelPermissionWhisper, ChannelPermissionWhisper}
		},
		wantErr: `lists permission "whisper" more than once`,
	},
	{
		name: "duplicate channel permission",
		mutate: func(m *AgentManifestV1) {
			m.Channels[0].Permissions = []ChannelPermission{ChannelPermissionRead, ChannelPermissionRead}
		},
		wantErr: `lists permission "read" more than once`,
	},

	{
		name:    "zero-value rate limit entry",
		mutate:  func(m *AgentManifestV1) { m.RateLimits = []RateLimit{{}} },
		wantErr: `rate limit capability "" is not a known capability`,
	},
	{
		name:    "rate limit for unknown capability",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[0].Capability = "voice.publish" },
		wantErr: `rate limit capability "voice.publish" is not a known capability`,
	},
	{
		name:    "rate limit max zero",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[0].Max = 0 },
		wantErr: `max for "messages.post" must be positive, got 0`,
	},
	{
		name:    "rate limit max negative",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[0].Max = -1 },
		wantErr: `max for "messages.post" must be positive, got -1`,
	},
	{
		name:    "rate limit window zero",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[0].WindowSeconds = 0 },
		wantErr: `window_seconds for "messages.post" must be positive, got 0`,
	},
	{
		name:    "rate limit window negative",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[0].WindowSeconds = -60 },
		wantErr: `window_seconds for "messages.post" must be positive, got -60`,
	},
	{
		name:    "duplicate rate limit",
		mutate:  func(m *AgentManifestV1) { m.RateLimits[1].Capability = CapabilityMessagesPost },
		wantErr: `lists a rate limit for "messages.post" more than once`,
	},
}

func assertValidation(t *testing.T, err error, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("Validate() = nil, want error containing %q", wantErr)
	}
	if !strings.HasPrefix(err.Error(), "schema: ") {
		t.Errorf("Validate() error %q lacks the schema: prefix", err)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Validate() = %q, want error containing %q", err, wantErr)
	}
}

func TestAgentManifestV1Validate(t *testing.T) {
	for _, tt := range manifestPolicyCases {
		t.Run(tt.name, func(t *testing.T) {
			m := validAgentManifest()
			tt.mutate(&m)
			assertValidation(t, m.Validate(), tt.wantErr)
		})
	}

	// Fields only the stored manifest carries.
	envelope := []struct {
		name    string
		mutate  func(*AgentManifestV1)
		wantErr string
	}{
		{name: "missing schema", mutate: func(m *AgentManifestV1) { m.Schema = "" }, wantErr: `schema must be "conch.agent_manifest.v1"`},
		{name: "wrong schema version", mutate: func(m *AgentManifestV1) { m.Schema = "conch.agent_manifest.v2" }, wantErr: `schema must be "conch.agent_manifest.v1"`},
		{name: "zero principal id", mutate: func(m *AgentManifestV1) { m.PrincipalID = 0 }, wantErr: "principal_id must be positive, got 0"},
		{name: "negative principal id", mutate: func(m *AgentManifestV1) { m.PrincipalID = -3 }, wantErr: "principal_id must be positive, got -3"},
		{name: "missing created_at", mutate: func(m *AgentManifestV1) { m.CreatedAt = Timestamp{} }, wantErr: "created_at is required"},
		{name: "missing updated_at", mutate: func(m *AgentManifestV1) { m.UpdatedAt = Timestamp{} }, wantErr: "updated_at is required"},
	}
	for _, tt := range envelope {
		t.Run(tt.name, func(t *testing.T) {
			m := validAgentManifest()
			tt.mutate(&m)
			assertValidation(t, m.Validate(), tt.wantErr)
		})
	}
}

func TestPutAgentManifestRequestV1Validate(t *testing.T) {
	for _, tt := range manifestPolicyCases {
		t.Run(tt.name, func(t *testing.T) {
			m := validAgentManifest()
			tt.mutate(&m)
			r := PutAgentManifestRequestV1{
				DisplayName:  m.DisplayName,
				Tier:         m.Tier,
				Capabilities: m.Capabilities,
				Channels:     m.Channels,
				RateLimits:   m.RateLimits,
			}
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

// The zero-value manifest must not validate and must grant nothing: there is
// no way to obtain access from an uninitialized value.
func TestAgentManifestV1ZeroValueGrantsNothing(t *testing.T) {
	var m AgentManifestV1
	if err := m.Validate(); err == nil {
		t.Error("zero-value manifest validated; want an error")
	}
	for _, c := range Capabilities() {
		if m.Allows(c) {
			t.Errorf("zero-value manifest allows %q", c)
		}
	}
	for _, p := range ChannelPermissions() {
		if m.AllowsChannel(1, p) {
			t.Errorf("zero-value manifest allows %q in channel 1", p)
		}
	}
}

func TestAgentManifestV1Allows(t *testing.T) {
	m := validAgentManifest()
	m.Capabilities = []Capability{CapabilityMessagesRead, CapabilityMessagesPost}

	tests := []struct {
		capability Capability
		want       bool
	}{
		{CapabilityMessagesRead, true},
		{CapabilityMessagesPost, true},
		// Message access never implies approval access (#79).
		{CapabilityApprovalsRequest, false},
		{CapabilityApprovalsAwait, false},
		{CapabilityApprovalsCheck, false},
		{"", false},
		{"*", false},
		{"nets.transmit", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.capability), func(t *testing.T) {
			if got := m.Allows(tt.capability); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.capability, got, tt.want)
			}
		})
	}
}

func TestAgentManifestV1AllowsChannel(t *testing.T) {
	m := validAgentManifest()
	m.Channels = []ChannelGrant{
		{ChannelID: 7, Permissions: []ChannelPermission{ChannelPermissionRead, ChannelPermissionPost}},
		{ChannelID: 9, Permissions: []ChannelPermission{ChannelPermissionRead}},
		{ChannelID: 11, Permissions: []ChannelPermission{ChannelPermissionPost}},
		{ChannelID: 13, Permissions: []ChannelPermission{ChannelPermissionRead, ChannelPermissionPostNet}},
		{ChannelID: 15, Permissions: []ChannelPermission{ChannelPermissionWhisper}},
	}

	tests := []struct {
		name      string
		channelID int64
		perm      ChannelPermission
		want      bool
	}{
		{"read where both granted", 7, ChannelPermissionRead, true},
		{"post where both granted", 7, ChannelPermissionPost, true},
		{"read where read-only", 9, ChannelPermissionRead, true},
		{"post where read-only", 9, ChannelPermissionPost, false},
		{"post where post-only", 11, ChannelPermissionPost, true},
		{"post does not imply read", 11, ChannelPermissionRead, false},
		{"unlisted channel read", 8, ChannelPermissionRead, false},
		{"unlisted channel post", 8, ChannelPermissionPost, false},
		{"zero channel id", 0, ChannelPermissionRead, false},
		{"unknown permission", 7, "admin", false},
		// Scoped-speaking grants are independent of post and of each other (ADR-005).
		{"post_net where granted", 13, ChannelPermissionPostNet, true},
		{"post_net does not imply post", 13, ChannelPermissionPost, false},
		{"post does not imply post_net", 7, ChannelPermissionPostNet, false},
		{"post does not imply whisper", 7, ChannelPermissionWhisper, false},
		{"whisper where granted", 15, ChannelPermissionWhisper, true},
		{"whisper does not imply whisper_agent", 15, ChannelPermissionWhisperAgent, false},
		{"whisper does not imply read", 15, ChannelPermissionRead, false},
		{"whisper does not imply post", 15, ChannelPermissionPost, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.AllowsChannel(tt.channelID, tt.perm); got != tt.want {
				t.Errorf("AllowsChannel(%d, %q) = %v, want %v", tt.channelID, tt.perm, got, tt.want)
			}
		})
	}
}

func TestAgentManifestV1RateLimitFor(t *testing.T) {
	m := validAgentManifest()

	got, ok := m.RateLimitFor(CapabilityMessagesPost)
	if !ok || got != (RateLimit{Capability: CapabilityMessagesPost, Max: 60, WindowSeconds: 60}) {
		t.Errorf("RateLimitFor(messages.post) = %+v, %v; want the declared limit", got, ok)
	}

	// No entry is the single spelling of "not rate limited by the manifest".
	if got, ok := m.RateLimitFor(CapabilityMessagesRead); ok || got != (RateLimit{}) {
		t.Errorf("RateLimitFor(messages.read) = %+v, %v; want zero, false", got, ok)
	}

	m.RateLimits = nil
	for _, c := range Capabilities() {
		if _, ok := m.RateLimitFor(c); ok {
			t.Errorf("RateLimitFor(%q) reported a limit on a manifest with none", c)
		}
	}
}

// currentMCPTools is the tool set conchd registers today (ADR-001 minimum
// set; internal/server/mcp.go). It is restated here, not imported, because
// pkg/schema must not depend on the server. Registering a new tool means
// adding it here and to the mapping in the same change.
var currentMCPTools = []string{
	"ack_intervention",
	"await_decision",
	"check_decision",
	"end_session",
	"heartbeat",
	"list_reactors",
	"poll_interventions",
	"post_activity",
	"post_message",
	"read_channel",
	"read_reactions",
	"register_session",
	"remove_reaction",
	"request_approval",
	"set_reaction",
	"start_run",
	"update_run",
}

func TestMCPToolCapabilitiesCoverExactlyTheCurrentTools(t *testing.T) {
	mapping := MCPToolCapabilities()

	got := make([]string, 0, len(mapping))
	for tool := range mapping {
		got = append(got, tool)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(currentMCPTools, ",") {
		t.Fatalf("mapped tools = %v, want exactly %v", got, currentMCPTools)
	}

	for tool, c := range mapping {
		if !c.Valid() {
			t.Errorf("tool %q maps to %q, which is not a known capability", tool, c)
		}
	}
}

func TestMCPToolCapability(t *testing.T) {
	tests := []struct {
		tool   string
		want   Capability
		wantOK bool
	}{
		{"post_message", CapabilityMessagesPost, true},
		{"read_channel", CapabilityMessagesRead, true},
		{"set_reaction", CapabilityMessagesPost, true},
		{"remove_reaction", CapabilityMessagesPost, true},
		{"read_reactions", CapabilityMessagesRead, true},
		{"list_reactors", CapabilityMessagesRead, true},
		{"request_approval", CapabilityApprovalsRequest, true},
		{"await_decision", CapabilityApprovalsAwait, true},
		{"check_decision", CapabilityApprovalsCheck, true},
		// An unmapped tool has no capability: enforcement must deny it.
		{"delete_channel", "", false},
		{"", "", false},
		{"POST_MESSAGE", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got, ok := MCPToolCapability(tt.tool)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("MCPToolCapability(%q) = %q, %v; want %q, %v", tt.tool, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// Each message and approval tool needs its own capability so every approval
// operation is independently grantable (#79); two tools sharing one would
// couple them. The session and run tools (#267) are the exception, on
// purpose: decision D9 gives an agent's own sessions one capability and its
// runs another, and TestSessionAndRunCapabilities pins which tools have them.
// The reaction tools (issue #765, message-reactions.md §4.2 and §16.4) are the
// other: they add no capability, so reacting is governed by the grants that
// govern posting and reading, and TestReactionToolCapabilities pins them.
// The admin tools (issue #1326) are the third: one capability says the agent
// may use the door, and TestAdminActCapability pins which tools it admits.
func TestMCPToolCapabilitiesAreDistinct(t *testing.T) {
	seen := map[Capability]string{}
	for tool, c := range MCPToolCapabilities() {
		if c == CapabilitySessionsManage || c == CapabilityRunsManage || c == CapabilityAdminAct || reactionTools[tool] {
			continue
		}
		if other, dup := seen[c]; dup {
			t.Errorf("tools %q and %q share capability %q", tool, other, c)
		}
		seen[c] = tool
	}
}

// reactionTools are the four reaction tools and the capability each needs.
var reactionTools = map[string]bool{"set_reaction": true, "remove_reaction": true, "read_reactions": true, "list_reactors": true}

// A reaction is a small post and a read of reactions is a read: the writes
// need messages.post and the reads messages.read, and nothing else maps to
// either of those through a reaction tool.
func TestReactionToolCapabilities(t *testing.T) {
	want := map[string]Capability{
		"set_reaction":    CapabilityMessagesPost,
		"remove_reaction": CapabilityMessagesPost,
		"read_reactions":  CapabilityMessagesRead,
		"list_reactors":   CapabilityMessagesRead,
	}
	for tool, c := range want {
		if got, ok := MCPToolCapability(tool); !ok || got != c {
			t.Errorf("MCPToolCapability(%q) = %q, %v; want %q", tool, got, ok, c)
		}
	}
	for tool := range reactionTools {
		if _, ok := want[tool]; !ok {
			t.Errorf("reactionTools names %q, which this test does not pin", tool)
		}
	}
	if len(reactionTools) != len(want) {
		t.Errorf("reactionTools has %d tools, want %d", len(reactionTools), len(want))
	}
}

func TestMCPToolCapabilitiesReturnsACopy(t *testing.T) {
	MCPToolCapabilities()["delete_channel"] = CapabilityMessagesPost
	if _, ok := MCPToolCapability("delete_channel"); ok {
		t.Error("mutating the returned map changed the package mapping")
	}
}

func TestCapabilitiesListsTheWholeVocabulary(t *testing.T) {
	all := Capabilities()
	seen := map[Capability]struct{}{}
	for _, c := range all {
		if !c.Valid() {
			t.Errorf("Capabilities() contains %q, which is not Valid", c)
		}
		if _, dup := seen[c]; dup {
			t.Errorf("Capabilities() lists %q more than once", c)
		}
		seen[c] = struct{}{}
	}
	// Every capability reachable from a tool must be in the vocabulary list.
	for tool, c := range MCPToolCapabilities() {
		if _, ok := seen[c]; !ok {
			t.Errorf("tool %q maps to %q, which Capabilities() does not list", tool, c)
		}
	}
}

// The P3 capabilities (issue #264, decision D9) are in the vocabulary, after
// the approval ones, and a manifest may grant them. The tools that need them
// (#267) are exactly the three about the agent's own sessions and the five
// about its runs.
func TestSessionAndRunCapabilities(t *testing.T) {
	if CapabilitySessionsManage != "sessions.manage" || CapabilityRunsManage != "runs.manage" {
		t.Fatalf("capabilities = %q, %q", CapabilitySessionsManage, CapabilityRunsManage)
	}
	want := []Capability{
		CapabilityMessagesRead, CapabilityMessagesPost,
		CapabilityApprovalsRequest, CapabilityApprovalsAwait, CapabilityApprovalsCheck,
		CapabilitySessionsManage, CapabilityRunsManage, CapabilityAdminAct,
	}
	if got := Capabilities(); strings.Join(capabilityStrings(got), ",") != strings.Join(capabilityStrings(want), ",") {
		t.Errorf("Capabilities() = %v, want %v", got, want)
	}
	for _, c := range []Capability{CapabilitySessionsManage, CapabilityRunsManage} {
		if !c.Valid() {
			t.Errorf("%q.Valid() = false", c)
		}
	}
	wantTools := map[Capability][]string{
		CapabilitySessionsManage: {"end_session", "heartbeat", "register_session"},
		CapabilityRunsManage:     {"ack_intervention", "poll_interventions", "post_activity", "start_run", "update_run"},
	}
	for c, want := range wantTools {
		var got []string
		for tool, mapped := range MCPToolCapabilities() {
			if mapped == c {
				got = append(got, tool)
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("tools needing %q = %v, want %v", c, got, want)
		}
	}
	for _, c := range []Capability{"sessions", "sessions.*", "session.manage", "runs", "runs.*", "run.manage", "SESSIONS.MANAGE", "runs_manage"} {
		if c.Valid() {
			t.Errorf("capability %q should be invalid", c)
		}
	}

	m := validAgentManifest()
	m.Capabilities = []Capability{CapabilityMessagesRead, CapabilityMessagesPost, CapabilitySessionsManage, CapabilityRunsManage}
	m.RateLimits = append(m.RateLimits, RateLimit{Capability: CapabilityRunsManage, Max: 30, WindowSeconds: 60})
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest granting the P3 capabilities: Validate() = %v", err)
	}
	if !m.Allows(CapabilitySessionsManage) || !m.Allows(CapabilityRunsManage) {
		t.Error("manifest listing the P3 capabilities does not Allow them")
	}
	// Holding them implies nothing else (no capability implies another).
	if m.Allows(CapabilityApprovalsRequest) {
		t.Error("runs.manage or sessions.manage implied approvals.request")
	}
	// And a manifest without them does not hold them.
	if validAgentManifest().Allows(CapabilitySessionsManage) || validAgentManifest().Allows(CapabilityRunsManage) {
		t.Error("the pre-P3 manifest allows a P3 capability it does not list")
	}
}

func capabilityStrings(caps []Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

func TestManifestVocabularyValid(t *testing.T) {
	for _, tier := range []AgentTier{AgentTierC, AgentTierA, AgentTierH} {
		if !tier.Valid() {
			t.Errorf("tier %q should be valid", tier)
		}
	}
	for _, tier := range []AgentTier{"", "c", "B", "Tier-H", "*"} {
		if tier.Valid() {
			t.Errorf("tier %q should be invalid", tier)
		}
	}
	for _, p := range []ChannelPermission{
		ChannelPermissionRead, ChannelPermissionPost,
		ChannelPermissionPostNet, ChannelPermissionWhisper, ChannelPermissionWhisperAgent,
	} {
		if !p.Valid() {
			t.Errorf("channel permission %q should be valid", p)
		}
	}
	for _, p := range []ChannelPermission{"", "READ", "write", "admin", "*", "post-net", "whisper.agent", "WHISPER"} {
		if p.Valid() {
			t.Errorf("channel permission %q should be invalid", p)
		}
	}
	for _, c := range []Capability{"", "*", "messages.*", "messages", "post_message", "MESSAGES.POST"} {
		if c.Valid() {
			t.Errorf("capability %q should be invalid", c)
		}
	}
}

func TestChannelPermissionsListsTheWholeVocabulary(t *testing.T) {
	all := ChannelPermissions()
	want := []ChannelPermission{
		ChannelPermissionRead,
		ChannelPermissionPost,
		ChannelPermissionPostNet,
		ChannelPermissionWhisper,
		ChannelPermissionWhisperAgent,
	}
	if len(all) != len(want) {
		t.Fatalf("ChannelPermissions() = %v, want %v", all, want)
	}
	seen := map[ChannelPermission]struct{}{}
	for i, p := range all {
		if p != want[i] {
			t.Errorf("ChannelPermissions()[%d] = %q, want %q", i, p, want[i])
		}
		if !p.Valid() {
			t.Errorf("ChannelPermissions() contains %q, which is not Valid", p)
		}
		if _, dup := seen[p]; dup {
			t.Errorf("ChannelPermissions() lists %q more than once", p)
		}
		seen[p] = struct{}{}
	}
	// The returned slice is a copy.
	all[0] = "admin"
	if ChannelPermissions()[0] != ChannelPermissionRead {
		t.Error("mutating the returned slice changed the package vocabulary")
	}
}

// validAdminManifest is validAgentManifest with an administrative grant: the
// agent-grantable permissions, the admin settings and the admin.act
// capability (ai-administrators.md §3.1).
func validAdminManifest() AgentManifestV1 {
	m := validAgentManifest()
	m.Capabilities = append(m.Capabilities, CapabilityAdminAct)
	m.Channels = append(m.Channels, ChannelGrant{ChannelID: 11, Permissions: []ChannelPermission{ChannelPermissionRead, ChannelPermissionPost}})
	m.RateLimits = append(m.RateLimits, RateLimit{Capability: CapabilityAdminAct, Max: 60, WindowSeconds: 3600})
	m.Permissions = []Permission{PermManageMembers, PermManageChannels}
	m.Admin = &AgentAdminV1{
		ApprovalChannelID:  11,
		ApprovalTTLSeconds: 43200,
		RunWindowSeconds:   3600,
		Quorum:             2,
		EscalationTarget:   &EscalationTarget{Kind: EscalationTargetNtfyTopic, Topic: "conch-admin-urgent"},
		RequireApproval:    []AdminAction{AdminActionCreateMemberLink, AdminActionRenameMember},
		MaxPending:         4,
	}
	return m
}

// adminPolicyCases are the grant's validation rules, held to by the manifest
// and its PUT request alike, like manifestPolicyCases.
var adminPolicyCases = []struct {
	name    string
	mutate  func(*AgentManifestV1)
	wantErr string
}{
	{name: "valid grant", mutate: func(*AgentManifestV1) {}},
	{name: "valid grant with the defaults", mutate: func(m *AgentManifestV1) { m.Admin = &AgentAdminV1{ApprovalChannelID: 11} }},
	{name: "valid grant of every agent-grantable permission", mutate: func(m *AgentManifestV1) {
		m.Permissions = []Permission{PermManageMembers, PermManageChannels, PermMakeMemberLinks, PermChangeServerSettings, PermReadAuditLog}
	}},
	{name: "valid grant without admin.act: the door is reachable over REST", mutate: func(m *AgentManifestV1) {
		m.Capabilities = m.Capabilities[:len(m.Capabilities)-1]
		m.RateLimits = m.RateLimits[:len(m.RateLimits)-1]
	}},
	{name: "valid admin.act without a grant: the door answers an empty catalog", mutate: func(m *AgentManifestV1) {
		m.Permissions, m.Admin = nil, nil
	}},
	{name: "valid bounds at their minimum", mutate: func(m *AgentManifestV1) {
		m.Admin.ApprovalTTLSeconds, m.Admin.RunWindowSeconds, m.Admin.Quorum, m.Admin.MaxPending = MinAdminApprovalTTLSeconds, MinAdminRunWindowSeconds, 1, 1
	}},
	{name: "valid bounds at their maximum", mutate: func(m *AgentManifestV1) {
		m.Admin.ApprovalTTLSeconds, m.Admin.RunWindowSeconds, m.Admin.MaxPending = MaxAdminApprovalTTLSeconds, MaxAdminRunWindowSeconds, MaxAdminMaxPending
	}},
	{name: "valid principal escalation target", mutate: func(m *AgentManifestV1) {
		m.Admin.EscalationTarget = &EscalationTarget{Kind: EscalationTargetPrincipal, PrincipalID: 1}
	}},
	{name: "valid require_approval of an action gated anyway", mutate: func(m *AgentManifestV1) {
		m.Admin.RequireApproval = []AdminAction{AdminActionDisableMember}
	}},

	// The one power the design rules out, and the member permissions.
	{name: "act_as_members", mutate: func(m *AgentManifestV1) { m.Permissions = append(m.Permissions, PermActAsMembers) }, wantErr: `permission "act_as_members" cannot be granted to an agent`},
	{name: "create_public_channels", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermCreatePublicChannels} }, wantErr: `permission "create_public_channels" cannot be granted to an agent`},
	{name: "create_private_channels", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermCreatePrivateChannels} }, wantErr: `permission "create_private_channels" cannot be granted to an agent`},
	{name: "see_member_directory", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermSeeMemberDirectory} }, wantErr: `permission "see_member_directory" cannot be granted to an agent`},
	{name: "use_voice", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermUseVoice} }, wantErr: `permission "use_voice" cannot be granted to an agent`},
	{name: "share_screen", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermShareScreen} }, wantErr: `permission "share_screen" cannot be granted to an agent`},
	{name: "use_camera", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{PermUseCamera} }, wantErr: `permission "use_camera" cannot be granted to an agent`},
	{name: "unknown permission", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{"owner"} }, wantErr: `permission "owner" is not in the catalog`},
	{name: "empty permission", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{""} }, wantErr: `permission "" is not in the catalog`},
	{name: "wildcard permission", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{"*"} }, wantErr: `permission "*" is not in the catalog`},
	{name: "duplicate permission", mutate: func(m *AgentManifestV1) { m.Permissions = append(m.Permissions, PermManageMembers) }, wantErr: `lists permission "manage_members" more than once`},

	// The admin settings go with the grant, and only with it.
	{name: "permissions without admin", mutate: func(m *AgentManifestV1) { m.Admin = nil }, wantErr: "admin is required when permissions are granted"},
	{name: "admin without permissions", mutate: func(m *AgentManifestV1) { m.Permissions = nil }, wantErr: "admin is set but no permissions are granted"},
	{name: "admin with empty permissions", mutate: func(m *AgentManifestV1) { m.Permissions = []Permission{} }, wantErr: "admin is set but no permissions are granted"},
	{name: "zero approval channel", mutate: func(m *AgentManifestV1) { m.Admin.ApprovalChannelID = 0 }, wantErr: "approval_channel_id must be positive, got 0"},
	{name: "negative approval channel", mutate: func(m *AgentManifestV1) { m.Admin.ApprovalChannelID = -11 }, wantErr: "approval_channel_id must be positive, got -11"},
	{name: "approval ttl below the minimum", mutate: func(m *AgentManifestV1) { m.Admin.ApprovalTTLSeconds = 299 }, wantErr: "approval_ttl_seconds must be 300 to 604800, got 299"},
	{name: "approval ttl above the maximum", mutate: func(m *AgentManifestV1) { m.Admin.ApprovalTTLSeconds = 604801 }, wantErr: "approval_ttl_seconds must be 300 to 604800, got 604801"},
	{name: "negative approval ttl", mutate: func(m *AgentManifestV1) { m.Admin.ApprovalTTLSeconds = -1 }, wantErr: "approval_ttl_seconds must be 300 to 604800, got -1"},
	{name: "run window below the minimum", mutate: func(m *AgentManifestV1) { m.Admin.RunWindowSeconds = 59 }, wantErr: "run_window_seconds must be 60 to 604800, got 59"},
	{name: "run window above the maximum", mutate: func(m *AgentManifestV1) { m.Admin.RunWindowSeconds = 604801 }, wantErr: "run_window_seconds must be 60 to 604800, got 604801"},
	{name: "negative quorum", mutate: func(m *AgentManifestV1) { m.Admin.Quorum = -1 }, wantErr: "quorum must not be negative, got -1"},
	{name: "invalid escalation target", mutate: func(m *AgentManifestV1) { m.Admin.EscalationTarget = &EscalationTarget{Kind: "pager"} }, wantErr: `escalation target kind "pager" is not one of principal, ntfy_topic`},
	{name: "unknown require_approval action", mutate: func(m *AgentManifestV1) { m.Admin.RequireApproval = []AdminAction{"delete_everything"} }, wantErr: `require_approval action "delete_everything" is not in the catalog`},
	{name: "empty require_approval action", mutate: func(m *AgentManifestV1) { m.Admin.RequireApproval = []AdminAction{""} }, wantErr: `require_approval action "" is not in the catalog`},
	{name: "duplicate require_approval action", mutate: func(m *AgentManifestV1) {
		m.Admin.RequireApproval = []AdminAction{AdminActionRenameMember, AdminActionRenameMember}
	}, wantErr: `require_approval lists "rename_member" more than once`},
	{name: "negative max_pending", mutate: func(m *AgentManifestV1) { m.Admin.MaxPending = -1 }, wantErr: "max_pending must be 1 to 32, got -1"},
	{name: "max_pending above the cap", mutate: func(m *AgentManifestV1) { m.Admin.MaxPending = 33 }, wantErr: "max_pending must be 1 to 32, got 33"},
}

func TestAgentManifestV1ValidateAdminGrant(t *testing.T) {
	for _, tt := range adminPolicyCases {
		t.Run(tt.name, func(t *testing.T) {
			m := validAdminManifest()
			tt.mutate(&m)
			assertValidation(t, m.Validate(), tt.wantErr)
		})
	}
}

func TestPutAgentManifestRequestV1ValidateAdminGrant(t *testing.T) {
	for _, tt := range adminPolicyCases {
		t.Run(tt.name, func(t *testing.T) {
			m := validAdminManifest()
			tt.mutate(&m)
			r := PutAgentManifestRequestV1{
				DisplayName:  m.DisplayName,
				Tier:         m.Tier,
				Capabilities: m.Capabilities,
				Channels:     m.Channels,
				RateLimits:   m.RateLimits,
				Permissions:  m.Permissions,
				Admin:        m.Admin,
			}
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

// A manifest with no grant holds nothing, and the fixtures that predate the
// grant decode with none: an upgrade grants no agent anything.
func TestAgentManifestV1HoldsPermission(t *testing.T) {
	for _, file := range []string{"agent-manifest-v1.json", "agent-manifest-v1-deny-all.json", "agent-manifest-v1-nets.json", "agent-manifest-v1-sessions.json"} {
		var m AgentManifestV1
		readFixture(t, file, &m)
		if m.Permissions != nil || m.Admin != nil {
			t.Errorf("%s carries a grant: %v %v", file, m.Permissions, m.Admin)
		}
		for _, p := range Permissions {
			if m.HoldsPermission(p) {
				t.Errorf("%s holds %s", file, p)
			}
		}
	}
	m := validAdminManifest()
	m.Permissions = []Permission{PermManageMembers, PermReadAuditLog}
	for _, p := range Permissions {
		want := p == PermManageMembers || p == PermReadAuditLog
		if got := m.HoldsPermission(p); got != want {
			t.Errorf("HoldsPermission(%s) = %v, want %v", p, got, want)
		}
	}
	// No dependency and no implication: manage_members does not bring
	// act_as_members, and a listed value that is not grantable is not held.
	m.Permissions = []Permission{PermManageMembers, PermActAsMembers, PermUseVoice, "owner"}
	if m.Validate() == nil {
		t.Error("a manifest listing act_as_members validated")
	}
	for _, p := range []Permission{PermActAsMembers, PermUseVoice, "owner", ""} {
		if m.HoldsPermission(p) {
			t.Errorf("HoldsPermission(%q) = true on a listed but non-grantable value", p)
		}
	}
	if !m.HoldsPermission(PermManageMembers) {
		t.Error("HoldsPermission(manage_members) = false")
	}
	var zero AgentManifestV1
	for _, p := range Permissions {
		if zero.HoldsPermission(p) {
			t.Errorf("zero-value manifest holds %s", p)
		}
	}
}

func TestAgentAdminV1Defaults(t *testing.T) {
	// The defaults and bounds are the design's (§3.1, §3.8): a day, a day,
	// one decider, eight open approvals.
	if DefaultAdminApprovalTTLSeconds != 86400 || DefaultAdminRunWindowSeconds != 86400 || DefaultAdminQuorum != 1 || DefaultAdminMaxPending != 8 {
		t.Error("the admin defaults are not the design's (§3.1)")
	}
	if MinAdminApprovalTTLSeconds != 300 || MaxAdminApprovalTTLSeconds != 604800 || MinAdminRunWindowSeconds != 60 || MaxAdminRunWindowSeconds != 604800 || MaxAdminMaxPending != 32 {
		t.Error("the admin bounds are not the design's (§3.1)")
	}
	a := AgentAdminV1{ApprovalChannelID: 11}
	if got := a.EffectiveApprovalTTLSeconds(); got != 86400 {
		t.Errorf("EffectiveApprovalTTLSeconds() = %d, want 86400", got)
	}
	if got := a.EffectiveRunWindowSeconds(); got != 86400 {
		t.Errorf("EffectiveRunWindowSeconds() = %d, want 86400", got)
	}
	if got := a.EffectiveQuorum(); got != 1 {
		t.Errorf("EffectiveQuorum() = %d, want 1", got)
	}
	if got := a.EffectiveMaxPending(); got != 8 {
		t.Errorf("EffectiveMaxPending() = %d, want 8", got)
	}
	for _, action := range AdminActions {
		if a.RequiresApproval(action) {
			t.Errorf("empty require_approval requires approval for %s", action)
		}
	}
	a = *validAdminManifest().Admin
	if a.EffectiveApprovalTTLSeconds() != 43200 || a.EffectiveRunWindowSeconds() != 3600 || a.EffectiveQuorum() != 2 || a.EffectiveMaxPending() != 4 {
		t.Errorf("the set values are not answered: %+v", a)
	}
	if !a.RequiresApproval(AdminActionRenameMember) || a.RequiresApproval(AdminActionListMembers) || a.RequiresApproval("") {
		t.Errorf("RequiresApproval answers wrongly for %v", a.RequireApproval)
	}
}

// admin.act (issue #1326) is the eighth capability: a manifest may grant and
// rate-limit it, it implies nothing, and the only tools that may ever need
// it are the three admin tools, which slice S4 maps when it registers them.
func TestAdminActCapability(t *testing.T) {
	if CapabilityAdminAct != "admin.act" || !CapabilityAdminAct.Valid() {
		t.Fatalf("CapabilityAdminAct = %q, Valid() = %v", CapabilityAdminAct, CapabilityAdminAct.Valid())
	}
	if all := Capabilities(); all[len(all)-1] != CapabilityAdminAct {
		t.Errorf("Capabilities() ends in %q, want admin.act last", all[len(all)-1])
	}
	for _, c := range []Capability{"admin", "admin.*", "admin.act ", "ADMIN.ACT", "admin_act", "admin.actions"} {
		if c.Valid() {
			t.Errorf("capability %q should be invalid", c)
		}
	}
	m := validAdminManifest()
	if !m.Allows(CapabilityAdminAct) {
		t.Error("a manifest listing admin.act does not Allow it")
	}
	if l, ok := m.RateLimitFor(CapabilityAdminAct); !ok || l.Max != 60 || l.WindowSeconds != 3600 {
		t.Errorf("RateLimitFor(admin.act) = %+v, %v", l, ok)
	}
	if validAgentManifest().Allows(CapabilityAdminAct) {
		t.Error("a manifest without admin.act allows it")
	}
	m.Capabilities = []Capability{CapabilityAdminAct}
	for _, c := range Capabilities() {
		if c != CapabilityAdminAct && m.Allows(c) {
			t.Errorf("admin.act implied %s", c)
		}
	}
	adminTools := map[string]bool{"describe_admin_actions": true, "submit_admin_action": true, "run_approved_action": true}
	for tool, c := range MCPToolCapabilities() {
		if c == CapabilityAdminAct && !adminTools[tool] {
			t.Errorf("tool %q maps to admin.act; only the three admin tools may", tool)
		}
		if adminTools[tool] && c != CapabilityAdminAct {
			t.Errorf("admin tool %q maps to %q, want admin.act", tool, c)
		}
	}
}
