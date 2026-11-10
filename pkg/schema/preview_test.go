package schema

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPreviewGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		// A member under the personal preset, and a target operator (issue
		// #1514, docs/design/permission-preview.md §6.1).
		{"preview-response-v1.json", func() any { return new(PreviewResponseV1) }},
		{"preview-response-v1-operator.json", func() any { return new(PreviewResponseV1) }},
		// A role simulation (§8, issue #1519): nobody's view, marked so.
		{"preview-response-v1-simulation.json", func() any { return new(PreviewResponseV1) }},
		// The refusal of the header on a route that is not previewable (§6.2).
		{"error-preview-read-only.json", func() any { return new(Error) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The fixtures are valid documents, not merely well-formed JSON.
func TestPreviewFixturesValidate(t *testing.T) {
	for _, file := range []string{
		"preview-response-v1.json",
		"preview-response-v1-operator.json",
		"preview-response-v1-simulation.json",
	} {
		t.Run(file, func(t *testing.T) {
			var r PreviewResponseV1
			readFixture(t, file, &r)
			if err := r.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestPreviewResponseV1Keys pins the wire rule of §6.1: every field but
// simulation is always sent, owner and the brakes as false when false, and
// permissions as [] when the target holds nothing, never null. simulation is
// absent for a person's preview.
func TestPreviewResponseV1Keys(t *testing.T) {
	always := []string{"id", "kind", "name", "role", "owner", "permissions", "permissions_revision", "may_invite", "voice_allowed"}
	tests := []struct {
		name     string
		resp     PreviewResponseV1
		wantKeys []string
		wantPerm string
	}{
		{
			"a member holding nothing",
			PreviewResponseV1{ID: 5, Kind: PrincipalHuman, Name: "alice", Role: RoleMember, PermissionsRevision: 1},
			always,
			"[]",
		},
		{
			"a member holding voice",
			PreviewResponseV1{ID: 5, Kind: PrincipalHuman, Name: "alice", Role: RoleMember, Permissions: []Permission{PermUseVoice}, PermissionsRevision: 1, VoiceAllowed: true},
			always,
			`["use_voice"]`,
		},
		{
			"a role simulation",
			PreviewResponseV1{Kind: PrincipalHuman, Role: RoleMember, PermissionsRevision: 1, Simulation: PreviewSimulationRole},
			append(slices.Clone(always), "simulation"),
			"[]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatal(err)
			}
			got := slices.Sorted(func(yield func(string) bool) {
				for k := range keys {
					if !yield(k) {
						return
					}
				}
			})
			want := slices.Sorted(slices.Values(tt.wantKeys))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("keys = %v, want %v in %s", got, want, raw)
			}
			if string(keys["permissions"]) != tt.wantPerm {
				t.Errorf("permissions = %s, want %s", keys["permissions"], tt.wantPerm)
			}
		})
	}
}

// TestPreviewResponseV1Validate checks the document's own rules: a known kind
// and role, and simulation either absent or a known value.
func TestPreviewResponseV1Validate(t *testing.T) {
	good := PreviewResponseV1{ID: 5, Kind: PrincipalHuman, Name: "alice", Role: RoleMember, Permissions: []Permission{}, PermissionsRevision: 1}
	tests := []struct {
		name    string
		mutate  func(*PreviewResponseV1)
		wantErr bool
	}{
		{"a member", func(*PreviewResponseV1) {}, false},
		{"an operator", func(r *PreviewResponseV1) { r.Role = RoleOperator; r.Owner = true }, false},
		{"a role simulation", func(r *PreviewResponseV1) { r.ID, r.Name, r.Simulation = 0, "", PreviewSimulationRole }, false},
		{"an unknown permission is tolerated", func(r *PreviewResponseV1) { r.Permissions = []Permission{"fly_the_server"} }, false},
		{"an unknown kind", func(r *PreviewResponseV1) { r.Kind = "robot" }, true},
		{"an empty kind", func(r *PreviewResponseV1) { r.Kind = "" }, true},
		{"an unknown role", func(r *PreviewResponseV1) { r.Role = "owner" }, true},
		{"an empty role", func(r *PreviewResponseV1) { r.Role = "" }, true},
		{"an unknown simulation", func(r *PreviewResponseV1) { r.Simulation = "guest" }, true},
		{"a simulation in the wrong case", func(r *PreviewResponseV1) { r.Simulation = "Role" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good
			tt.mutate(&r)
			if err := r.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

// TestPreviewResponseV1OmitsAccountFields pins §6.1's "absent on purpose and
// never added": the target's account state is whoami's and a preview never
// carries it. A field added here under one of these names fails the test.
func TestPreviewResponseV1OmitsAccountFields(t *testing.T) {
	never := []string{"auth", "guest", "identity_class", "invites_remaining", "server_version", "server_mode"}
	typ := reflect.TypeOf(PreviewResponseV1{})
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if slices.Contains(never, name) {
			t.Errorf("PreviewResponseV1 has %q, which is the target's private account state and belongs to whoami alone", name)
		}
	}
}

func TestPreviewSimulationValid(t *testing.T) {
	tests := []struct {
		in   PreviewSimulation
		want bool
	}{
		{PreviewSimulationRole, true},
		{"", false},
		{"Role", false},
		{"person", false},
	}
	for _, tt := range tests {
		if got := tt.in.Valid(); got != tt.want {
			t.Errorf("PreviewSimulation(%q).Valid() = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestErrorCodePreviewReadOnly(t *testing.T) {
	if ErrorCodePreviewReadOnly != "preview_read_only" {
		t.Errorf("ErrorCodePreviewReadOnly = %q, want preview_read_only", ErrorCodePreviewReadOnly)
	}
}
