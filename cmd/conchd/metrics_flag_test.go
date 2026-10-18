package main

import (
	"strings"
	"testing"
)

// TestParseServeArgsMetrics: metrics are off unless an address is given, by
// flag or environment; a non-loopback address is refused unless remote
// access is allowed, and allowing it without an address is a mistake.
func TestParseServeArgsMetrics(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantListen string
		wantRemote bool
		wantErr    string
	}{
		{name: "off by default"},
		{name: "loopback flag", args: []string{"--metrics-listen", "127.0.0.1:9464"}, wantListen: "127.0.0.1:9464"},
		{name: "loopback env", env: map[string]string{"CONCHD_METRICS_LISTEN": "[::1]:9464"}, wantListen: "[::1]:9464"},
		{name: "localhost", args: []string{"--metrics-listen", "localhost:9464"}, wantListen: "localhost:9464"},
		{name: "flag wins over env", args: []string{"--metrics-listen", "127.0.0.1:1"}, env: map[string]string{"CONCHD_METRICS_LISTEN": "127.0.0.1:2"}, wantListen: "127.0.0.1:1"},
		{name: "every interface", args: []string{"--metrics-listen", ":9464"}, wantErr: "--metrics-allow-remote"},
		{name: "a public address", args: []string{"--metrics-listen", "0.0.0.0:9464"}, wantErr: "not a loopback address"},
		{name: "a public address by env", env: map[string]string{"CONCHD_METRICS_LISTEN": "0.0.0.0:9464"}, wantErr: "not a loopback address"},
		{name: "allowed remote", args: []string{"--metrics-listen", "0.0.0.0:9464", "--metrics-allow-remote"}, wantListen: "0.0.0.0:9464", wantRemote: true},
		{name: "allowed remote by env", args: []string{"--metrics-listen", "0.0.0.0:9464"}, env: map[string]string{"CONCHD_METRICS_ALLOW_REMOTE": "1"}, wantListen: "0.0.0.0:9464", wantRemote: true},
		{name: "remote without an address", args: []string{"--metrics-allow-remote"}, wantErr: "needs --metrics-listen"},
		{name: "remote env unreadable", env: map[string]string{"CONCHD_METRICS_ALLOW_REMOTE": "maybe"}, wantErr: "CONCHD_METRICS_ALLOW_REMOTE"},
		{name: "no port", args: []string{"--metrics-listen", "127.0.0.1"}, wantErr: "--metrics-listen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_METRICS_LISTEN", "")
			t.Setenv("CONCHD_METRICS_ALLOW_REMOTE", "")
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
			if opts.metricsListen != tt.wantListen || opts.metricsRemote != tt.wantRemote {
				t.Errorf("metrics = %q remote %v, want %q remote %v", opts.metricsListen, opts.metricsRemote, tt.wantListen, tt.wantRemote)
			}
		})
	}
}
