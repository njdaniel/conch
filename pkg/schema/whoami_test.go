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
