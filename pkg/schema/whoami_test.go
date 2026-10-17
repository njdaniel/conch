package schema

import (
	"encoding/json"
	"testing"
)

func TestWhoAmIGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"whoami-response-v1.json", func() any { return new(WhoAmIResponseV1) }},
		// Voice and the remaining allowance (issue #468).
		{"whoami-response-v1-inviter.json", func() any { return new(WhoAmIResponseV1) }},
		{"whoami-response-v1-allowance-used.json", func() any { return new(WhoAmIResponseV1) }},
		{"whoami-response-v1-no-voice.json", func() any { return new(WhoAmIResponseV1) }},
		{"revoke-all-credentials-response-v1.json", func() any { return new(RevokeAllCredentialsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestWhoAmIResponseV1Keys(t *testing.T) {
	raw, err := json.Marshal(WhoAmIResponseV1{ID: 2, Kind: PrincipalAgent, Name: "bot", Role: RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "kind", "name", "role"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing key %q in %s", k, raw)
		}
	}
	if len(keys) != 4 {
		t.Errorf("keys = %v, want exactly id, kind, name, role", keys)
	}
}

func TestWhoAmIResponseV1ServerVersionKey(t *testing.T) {
	raw, err := json.Marshal(WhoAmIResponseV1{ID: 2, Kind: PrincipalAgent, Name: "bot", Role: RoleMember, ServerVersion: "v0.1.0-alpha.1"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if keys["server_version"] != "v0.1.0-alpha.1" || len(keys) != 5 {
		t.Errorf("keys = %v, want the four old keys and server_version", keys)
	}
}

// TestWhoAmIResponseV1VoiceAndAllowanceKeys checks that false and 0 are sent
// when the server sets them, since each means something there (issue #468),
// and that both keys are absent when it does not.
func TestWhoAmIResponseV1VoiceAndAllowanceKeys(t *testing.T) {
	no, zero := false, int64(0)
	tests := []struct {
		name string
		resp WhoAmIResponseV1
		want map[string]any
	}{
		{"neither set", WhoAmIResponseV1{}, map[string]any{}},
		{"voice withheld", WhoAmIResponseV1{VoiceAllowed: &no}, map[string]any{"voice_allowed": false}},
		{"allowance used up", WhoAmIResponseV1{InvitesRemaining: &zero}, map[string]any{"invites_remaining": float64(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]any
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"voice_allowed", "invites_remaining"} {
				got, has := keys[k]
				want, wantHas := tt.want[k]
				if has != wantHas || got != want {
					t.Errorf("%s = %v (present %v), want %v (present %v) in %s", k, got, has, want, wantHas, raw)
				}
			}
		})
	}
}
