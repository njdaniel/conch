package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseServeArgsWebDirAndOrigins(t *testing.T) {
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()

	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantDir     string
		wantOrigins string
		wantErr     string
	}{
		{name: "neither set"},
		{name: "web dir flag", args: []string{"--web-dir", good}, wantDir: good},
		{name: "web dir env", env: map[string]string{"CONCHD_WEB_DIR": good}, wantDir: good},
		{name: "web dir without index", args: []string{"--web-dir", empty}, wantErr: "no index.html"},
		{name: "web dir missing", args: []string{"--web-dir", filepath.Join(empty, "nope")}, wantErr: "--web-dir"},
		{name: "origins flag", args: []string{"--allowed-origins", "http://localhost:5173,http://127.0.0.1:5173"}, wantOrigins: "http://localhost:5173,http://127.0.0.1:5173"},
		{name: "origins env", env: map[string]string{"CONCHD_ALLOWED_ORIGINS": "http://localhost:5173"}, wantOrigins: "http://localhost:5173"},
		{name: "origins star", args: []string{"--allowed-origins", "*"}, wantErr: "wildcards are not allowed"},
		{name: "origins empty entry", args: []string{"--allowed-origins", "http://a.example,"}, wantErr: "empty entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_WEB_DIR", "")
			t.Setenv("CONCHD_ALLOWED_ORIGINS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.webDir != tt.wantDir {
				t.Errorf("webDir = %q, want %q", opts.webDir, tt.wantDir)
			}
			if got := strings.Join(opts.allowedOrigins, ","); got != tt.wantOrigins {
				t.Errorf("allowedOrigins = %q, want %q", got, tt.wantOrigins)
			}
		})
	}
}
