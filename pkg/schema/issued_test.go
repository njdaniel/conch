package schema

import (
	"encoding/json"
	"testing"
)

func TestIssuedGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"disable-principal-response-v1.json", func() any { return new(DisablePrincipalResponseV1) }},
		{"disable-principal-response-v1-none.json", func() any { return new(DisablePrincipalResponseV1) }},
		{"revoke-issued-response-v1.json", func() any { return new(RevokeIssuedResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The list is omitted, not null, when there is nothing to list: a disable
// that found nothing issued, and a demotion the same, answer without it.
func TestIssuedToOthersOmittedWhenEmpty(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"disable nil", DisablePrincipalResponseV1{}},
		{"disable empty", DisablePrincipalResponseV1{IssuedToOthers: []IssuedByOperatorV1{}}},
		{"role nil", RoleChangeResponseV1{}},
		{"role empty", RoleChangeResponseV1{IssuedToOthers: []IssuedByOperatorV1{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if _, ok := m["issued_to_others"]; ok {
				t.Errorf("issued_to_others present in %s", raw)
			}
		})
	}
}
