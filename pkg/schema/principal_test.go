package schema

import "testing"

func TestPrincipalGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "principal", file: "principal-v0.json", new: func() any { return &PrincipalV0{} }},
		{name: "create request", file: "create-principal-request-v0.json", new: func() any { return &CreatePrincipalRequest{} }},
		{name: "create response", file: "create-principal-response-v0.json", new: func() any { return &CreatePrincipalResponse{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}
