package schema

import "testing"

func TestMessageGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "message", file: "message-v0.json", new: func() any { return &MessageV0{} }},
		{name: "post request", file: "post-message-request-v0.json", new: func() any { return &PostMessageRequest{} }},
		{name: "post response", file: "post-message-response-v0.json", new: func() any { return &PostMessageResponse{} }},
		{name: "list response", file: "list-messages-response-v0.json", new: func() any { return &ListMessagesResponse{} }},
		{name: "error", file: "error.json", new: func() any { return &Error{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}
