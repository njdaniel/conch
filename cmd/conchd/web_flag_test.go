package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
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

// TestResolveWeb: which client /app/ serves. embeddedWeb stands in for a
// binary built with -tags conchweb.
func TestResolveWeb(t *testing.T) {
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withIndex := fstest.MapFS{"index.html": {Data: []byte("x")}}
	noIndex := fstest.MapFS{"other.html": {Data: []byte("x")}}

	tests := []struct {
		name       string
		embedded   fs.FS // nil: this build embeds nothing
		webDir     string
		noWeb      bool
		wantFS     bool
		wantSource string
		wantErr    string
	}{
		{name: "nothing built in, nothing given", wantSource: "none"},
		{name: "built in, nothing given", embedded: withIndex, wantFS: true, wantSource: "embedded"},
		{name: "built in and a directory: the directory wins", embedded: withIndex, webDir: good, wantSource: "dir " + good},
		{name: "directory only", webDir: good, wantSource: "dir " + good},
		{name: "built in and --no-web", embedded: withIndex, noWeb: true, wantSource: "none (--no-web)"},
		{name: "--no-web with nothing built in", noWeb: true, wantSource: "none (--no-web)"},
		{name: "--no-web and a directory", embedded: withIndex, webDir: good, noWeb: true, wantErr: "contradict"},
		{name: "unusable directory is refused, not replaced by the built-in", embedded: withIndex, webDir: filepath.Join(good, "nope"), wantErr: "--web-dir"},
		{name: "built in without index.html", embedded: noIndex, wantErr: "no index.html"},
		{name: "built in without index.html is fine under --no-web", embedded: noIndex, noWeb: true, wantSource: "none (--no-web)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := embeddedWeb
			t.Cleanup(func() { embeddedWeb = old })
			embeddedWeb = func() (fs.FS, bool) { return tt.embedded, tt.embedded != nil }

			fsys, source, err := resolveWeb(tt.webDir, tt.noWeb)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (fsys != nil) != tt.wantFS {
				t.Errorf("embedded client served = %v, want %v", fsys != nil, tt.wantFS)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func TestParseServeArgsNoWeb(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{name: "flag", args: []string{"--no-web"}},
		{name: "env", env: "1"},
		{name: "env other than 1 is off", env: "0"},
		{name: "flag with --web-dir", args: []string{"--no-web", "--web-dir", t.TempDir()}, wantErr: "contradict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_WEB_DIR", "")
			t.Setenv("CONCHD_NO_WEB", tt.env)
			old := embeddedWeb
			t.Cleanup(func() { embeddedWeb = old })
			embeddedWeb = func() (fs.FS, bool) { return fstest.MapFS{"index.html": {Data: []byte("x")}}, true }

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
			off := tt.env != "0"
			if off && (opts.webFS != nil || opts.webDir != "") {
				t.Errorf("--no-web still serves a client: dir %q, embedded %v", opts.webDir, opts.webFS != nil)
			}
			if !off && opts.webFS == nil {
				t.Error("CONCHD_NO_WEB=0 turned the embedded client off")
			}
		})
	}
}
