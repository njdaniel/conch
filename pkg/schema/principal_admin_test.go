package schema

import (
	"strings"
	"testing"
)

func TestPrincipalAdminGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"principal-admin-v1.json", func() any { return new(PrincipalAdminV1) }},
		{"principal-admin-v1-owner.json", func() any { return new(PrincipalAdminV1) }},
		{"principal-admin-v1-hidden.json", func() any { return new(PrincipalAdminV1) }},
		{"principal-admin-v1-guest.json", func() any { return new(PrincipalAdminV1) }},
		// The voice switch and the may_invite brake (ADR-013, issue #978).
		{"principal-admin-v1-voice.json", func() any { return new(PrincipalAdminV1) }},
		{"list-principals-response-v1.json", func() any { return new(ListPrincipalsResponseV1) }},
		{"list-principals-response-v1-empty.json", func() any { return new(ListPrincipalsResponseV1) }},
		{"hide-principal-response-v1.json", func() any { return new(HidePrincipalResponseV1) }},
		{"hide-principals-request-v1.json", func() any { return new(HidePrincipalsRequestV1) }},
		{"hide-principals-request-v1-invite.json", func() any { return new(HidePrincipalsRequestV1) }},
		{"hide-principals-response-v1.json", func() any { return new(HidePrincipalsResponseV1) }},
		{"hide-principals-response-v1-dry-run.json", func() any { return new(HidePrincipalsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// A bulk hide must say whether it is a dry run, and whom: an invite or a name
// prefix long enough not to sweep up half the server. Kind only narrows.
func TestHidePrincipalsRequestValidate(t *testing.T) {
	dry, real := true, false
	tests := []struct {
		name    string
		req     HidePrincipalsRequestV1
		wantErr string
	}{
		{"prefix", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "test-"}, ""},
		{"three characters", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "abc"}, ""},
		{"three letters of another script", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "жжж"}, ""},
		{"invite", HidePrincipalsRequestV1{DryRun: &dry, InviteID: 3}, ""},
		{"both, and a kind", HidePrincipalsRequestV1{DryRun: &dry, InviteID: 3, NamePrefix: "bot", Kind: PrincipalAgent}, ""},
		{"not a dry run", HidePrincipalsRequestV1{DryRun: &real, NamePrefix: "test-"}, ""},
		{"dry_run left out", HidePrincipalsRequestV1{NamePrefix: "test-"}, "dry_run is required"},
		{"nothing", HidePrincipalsRequestV1{DryRun: &dry}, "give invite_id or name_prefix"},
		{"kind alone", HidePrincipalsRequestV1{DryRun: &dry, Kind: PrincipalHuman}, "give invite_id or name_prefix"},
		{"short prefix", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "te"}, "3 to 100 characters"},
		{"two characters, more bytes", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "жж"}, "3 to 100 characters"},
		{"long prefix", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: strings.Repeat("a", MaxPrincipalNameLength+1)}, "3 to 100 characters"},
		{"blank prefix", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "   "}, "blank"},
		{"invalid UTF-8", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "ab\xff"}, "UTF-8"},
		{"negative invite", HidePrincipalsRequestV1{DryRun: &dry, InviteID: -1}, "positive"},
		{"unknown kind", HidePrincipalsRequestV1{DryRun: &dry, NamePrefix: "test-", Kind: "robot"}, "kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}
