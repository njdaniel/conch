package schema

import (
	"strings"
	"testing"
)

func TestHookGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "create request", file: "create-hook-request-v1.json", new: func() any { return &CreateHookRequest{} }},
		{name: "create request with label", file: "create-hook-request-v1-labeled.json", new: func() any { return &CreateHookRequest{} }},
		{name: "create response", file: "create-hook-response-v1.json", new: func() any { return &CreateHookResponse{} }},
		{name: "create response with id", file: "create-hook-response-v1-with-id.json", new: func() any { return &CreateHookResponse{} }},
		{name: "hook", file: "hook-v1.json", new: func() any { return &HookV1{} }},
		{name: "hook revoked", file: "hook-v1-revoked.json", new: func() any { return &HookV1{} }},
		{name: "list", file: "list-hooks-response-v1.json", new: func() any { return &ListHooksResponseV1{} }},
		{name: "list empty", file: "list-hooks-response-v1-empty.json", new: func() any { return &ListHooksResponseV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestCreateHookRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		wantErr bool
	}{
		{"no label", "", false},
		{"short label", "ci builds", false},
		{"max length", strings.Repeat("a", MaxHookLabelLength), false},
		{"too long", strings.Repeat("a", MaxHookLabelLength+1), true},
		{"multibyte counted in characters", strings.Repeat("é", MaxHookLabelLength), false},
		{"invalid utf-8", "bad\xff", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CreateHookRequest{Channel: "c", Principal: 1, Label: tt.label}.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
