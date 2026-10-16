package schema

import (
	"strings"
	"testing"
)

func TestPrincipalGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "principal", file: "principal-v0.json", new: func() any { return &PrincipalV0{} }},
		{name: "create request", file: "create-principal-request-v0.json", new: func() any { return &CreatePrincipalRequest{} }},
		{name: "create response", file: "create-principal-response-v0.json", new: func() any { return &CreatePrincipalResponse{} }},
		{name: "rename request", file: "rename-principal-request-v1.json", new: func() any { return &RenamePrincipalRequestV1{} }},
		{name: "rename response", file: "rename-principal-response-v1.json", new: func() any { return &RenamePrincipalResponseV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// A new name meets the rule a name meets on creation, joiners included.
func TestRenamePrincipalRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "Ann Lee", false},
		{"accented", "Zo\u00eb M\u00fcller", false},
		{"indic with a joiner", "\u0905\u0930\u094d\u200d\u091c\u0941\u0928", false},
		{"hidden account", "hidden-12", false},
		{"empty", "", true},
		{"blank", "   ", true},
		{"surrounding space", " ann", true},
		{"control character", "a\x1bb", true},
		{"bidi override", "a\u202eb", true},
		{"zero-width space", "a\u200bb", true},
		{"over length", strings.Repeat("a", MaxPrincipalNameLength+1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := (RenamePrincipalRequestV1{Name: tt.in}).Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
