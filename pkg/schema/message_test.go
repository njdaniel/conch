package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
			golden, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			value := tt.new()
			decoder := json.NewDecoder(bytes.NewReader(golden))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(value); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			got, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				t.Fatalf("marshal fixture value: %v", err)
			}
			got = append(got, '\n')
			if !bytes.Equal(got, golden) {
				t.Errorf("fixture does not match schema serialization\ngot:\n%s\nwant:\n%s", got, golden)
			}
		})
	}
}
