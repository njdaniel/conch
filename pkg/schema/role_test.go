package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRoleGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"role-change-request-v1.json", func() any { return new(RoleChangeRequestV1) }},
		{"role-change-request-v1-owner.json", func() any { return new(RoleChangeRequestV1) }},
		{"role-change-response-v1.json", func() any { return new(RoleChangeResponseV1) }},
		{"role-change-response-v1-demote.json", func() any { return new(RoleChangeResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestRoleChangeRequestV1Validate(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		req     RoleChangeRequestV1
		wantErr string
	}{
		{name: "promote to operator", req: RoleChangeRequestV1{Role: RoleOperator}},
		{name: "promote to owner", req: RoleChangeRequestV1{Role: RoleOperator, Owner: &yes}},
		{name: "remove owner mark", req: RoleChangeRequestV1{Role: RoleOperator, Owner: &no}},
		{name: "demote to member", req: RoleChangeRequestV1{Role: RoleMember}},
		{name: "demote to member with owner false", req: RoleChangeRequestV1{Role: RoleMember, Owner: &no}},
		{name: "empty role", req: RoleChangeRequestV1{}, wantErr: `role must be "operator" or "member"`},
		{name: "unknown role", req: RoleChangeRequestV1{Role: "owner"}, wantErr: `role must be "operator" or "member"`},
		{name: "member with owner true", req: RoleChangeRequestV1{Role: RoleMember, Owner: &yes}, wantErr: "a member cannot be an owner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestRoleChangeResponseV1ManagesEmptyIsArray(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC))
	p := PrincipalAdminV1{
		ID:        12,
		Kind:      PrincipalHuman,
		Name:      "wren",
		Role:      RoleOperator,
		CreatedAt: at,
	}
	for _, tc := range []struct {
		name    string
		manages []int64
	}{
		{"nil slice", nil},
		{"empty slice", []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(RoleChangeResponseV1{Principal: p, Manages: tc.manages})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"manages":[]`) || strings.Contains(string(raw), `"manages":null`) {
				t.Errorf("Marshal = %s, want manages:[] and never manages:null", raw)
			}
		})
	}
}
