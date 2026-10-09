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
		{"put-agent-manifest-request-v1.json", func() any { return new(PutAgentManifestRequestV1) }},
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
	"await_decision",
	"check_decision",
	"post_message",
	"read_channel",
	"request_approval",
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

// Each tool needs its own capability so every approval operation is
// independently grantable (#79); two tools sharing one would couple them.
func TestMCPToolCapabilitiesAreDistinct(t *testing.T) {
	seen := map[Capability]string{}
	for tool, c := range MCPToolCapabilities() {
		if other, dup := seen[c]; dup {
			t.Errorf("tools %q and %q share capability %q", tool, other, c)
		}
		seen[c] = tool
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
