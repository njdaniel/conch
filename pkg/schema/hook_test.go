package schema

import "testing"

func TestHookGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "create request", file: "create-hook-request-v1.json", new: func() any { return &CreateHookRequest{} }},
		{name: "create response", file: "create-hook-response-v1.json", new: func() any { return &CreateHookResponse{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}
