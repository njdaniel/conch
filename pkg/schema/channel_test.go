package schema

import (
	"encoding/json"
	"testing"
)

func TestChannelGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "channel", file: "channel-v0.json", new: func() any { return &ChannelV0{} }},
		{name: "create request", file: "create-channel-request-v0.json", new: func() any { return &CreateChannelRequest{} }},
		{name: "create response", file: "create-channel-response-v0.json", new: func() any { return &CreateChannelResponse{} }},
		{name: "list response", file: "list-channels-response.json", new: func() any { return &ListChannelsResponse{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestListChannelsResponseEmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ListChannelsResponse{Channels: []ChannelV0{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"channels":[]}` {
		t.Fatalf("got %s", got)
	}
}
