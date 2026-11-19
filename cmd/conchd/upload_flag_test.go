package main

import (
	"testing"

	"github.com/njdaniel/conch/internal/server"
)

func TestParseServeArgsMaxUploadBytes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    int64
		wantErr bool
	}{
		{"default", nil, "", server.DefaultMaxUploadBytes, false},
		{"flag", []string{"--max-upload-bytes", "1000"}, "", 1000, false},
		{"env", nil, "2048", 2048, false},
		{"flag beats env", []string{"--max-upload-bytes=5"}, "2048", 5, false},
		{"zero disables", []string{"--max-upload-bytes", "0"}, "", 0, false},
		{"env zero disables", nil, "0", 0, false},
		{"negative flag", []string{"--max-upload-bytes", "-1"}, "", 0, true},
		{"negative env", nil, "-1", 0, true},
		{"garbage env", nil, "ten", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_MAX_UPLOAD_BYTES", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && opts.maxUploadBytes != tt.want {
				t.Errorf("maxUploadBytes = %d, want %d", opts.maxUploadBytes, tt.want)
			}
		})
	}
}

func TestParseServeArgsAgentUploadsPerHour(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    int
		wantErr bool
	}{
		{"default", nil, "", server.DefaultAgentUploadsPerHour, false},
		{"flag", []string{"--agent-uploads-per-hour", "5"}, "", 5, false},
		{"env", nil, "60", 60, false},
		{"flag beats env", []string{"--agent-uploads-per-hour=3"}, "60", 3, false},
		{"zero lets no agent upload", []string{"--agent-uploads-per-hour", "0"}, "", 0, false},
		{"env zero", nil, "0", 0, false},
		{"negative flag", []string{"--agent-uploads-per-hour", "-1"}, "", 0, true},
		{"negative env", nil, "-1", 0, true},
		{"garbage env", nil, "twenty", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_AGENT_UPLOADS_PER_HOUR", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && opts.agentUploads != tt.want {
				t.Errorf("agentUploads = %d, want %d", opts.agentUploads, tt.want)
			}
		})
	}
}
