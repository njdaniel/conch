package schema

import "testing"

func TestChannelGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "channel", file: "channel-v0.json", new: func() any { return &ChannelV0{} }},
		{name: "create request", file: "create-channel-request-v0.json", new: func() any { return &CreateChannelRequest{} }},
		{name: "create response", file: "create-channel-response-v0.json", new: func() any { return &CreateChannelResponse{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}
