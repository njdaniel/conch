package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseServeArgsSessionThresholds(t *testing.T) {
	tests := []struct {
		name                   string
		args                   []string
		envStale, envOffline   string
		wantStale, wantOffline time.Duration
		wantErr                string
	}{
		{"defaults", nil, "", "", 30 * time.Second, 90 * time.Second, ""},
		{"flags", []string{"--session-stale", "2s", "--session-offline", "4s"}, "", "", 2 * time.Second, 4 * time.Second, ""},
		{"env", nil, "10s", "1m", 10 * time.Second, time.Minute, ""},
		{"flag beats env", []string{"--session-stale=3s"}, "10s", "1m", 3 * time.Second, time.Minute, ""},
		{"stale equals offline", []string{"--session-stale=5s", "--session-offline=5s"}, "", "", 0, 0, "shorter"},
		{"stale past offline", []string{"--session-stale=2m"}, "", "", 0, 0, "shorter"},
		{"env stale past the default offline", nil, "2m", "", 0, 0, "shorter"},
		{"zero", []string{"--session-stale=0"}, "", "", 0, 0, "positive"},
		{"negative", []string{"--session-stale=-1s"}, "", "", 0, 0, "positive"},
		{"garbage env", nil, "soon", "", 0, 0, "CONCHD_SESSION_STALE"},
		{"garbage offline env", nil, "", "later", 0, 0, "CONCHD_SESSION_OFFLINE"},
		{"garbage flag", []string{"--session-offline=later"}, "", "", 0, 0, "session-offline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_SESSION_STALE", tt.envStale)
			t.Setenv("CONCHD_SESSION_OFFLINE", tt.envOffline)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.sessionStale != tt.wantStale || opts.sessionOffline != tt.wantOffline {
				t.Errorf("thresholds = %s/%s, want %s/%s", opts.sessionStale, opts.sessionOffline, tt.wantStale, tt.wantOffline)
			}
		})
	}
}

// The flags are in `conchd serve --help` (printed by the flag set from the
// flags it defines, to stderr) and in conchd's own usage text.
func TestHelpListsSessionFlags(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	_, parseErr := parseServeArgs([]string{"--help"})
	os.Stderr = old
	_ = w.Close()
	help, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(parseErr, flag.ErrHelp) {
		t.Fatalf("serve --help returned %v, want flag.ErrHelp", parseErr)
	}
	for _, name := range []string{"-session-stale", "-session-offline"} {
		if !strings.Contains(string(help), name) {
			t.Errorf("serve --help does not list %s:\n%s", name, help)
		}
	}

	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	usage(f)
	text, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--session-stale", "CONCHD_SESSION_STALE", "--session-offline", "CONCHD_SESSION_OFFLINE"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}
