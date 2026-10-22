package main

import (
	"strings"
	"testing"
)

// --server-name (issue #710): the front door's label. Unset is the server's
// default; a name the schema would not show is refused at start.
func TestParseServeArgsServerName(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    string
		wantErr bool
	}{
		{"default", nil, "", "", false},
		{"flag", []string{"--server-name", "Harbour Club"}, "", "Harbour Club", false},
		{"env", nil, "conch.example", "conch.example", false},
		{"flag beats env", []string{"--server-name=Harbour"}, "conch.example", "Harbour", false},
		{"blank", []string{"--server-name", "   "}, "", "", true},
		{"too long", []string{"--server-name", strings.Repeat("x", 65)}, "", "", true},
		{"a control character", []string{"--server-name", "a\tb"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_SERVER_NAME", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && opts.serverName != tt.want {
				t.Errorf("serverName = %q, want %q", opts.serverName, tt.want)
			}
		})
	}
}
