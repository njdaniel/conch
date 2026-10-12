package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A stand-in cargo makes the build inexpensive while exercising the actual
// shell command, metadata decoding and executable copy. In particular, the
// default target directory must work without CARGO_TARGET_DIR (issue #178).
func TestBuildProbeTargetDirectory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		custom bool
	}{
		{name: "default without CARGO_TARGET_DIR"},
		{name: "configured directory with spaces", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			target := filepath.Join(repo, "voice", "target")
			if tc.custom {
				target = filepath.Join(repo, "configured target")
			}
			for _, dir := range []string{"scripts", "voice", "tools"} {
				if err := os.MkdirAll(filepath.Join(repo, dir), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "scripts", "voice-env.sh"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(target, "release", "examples", "sdk_probe")
			if err := os.MkdirAll(filepath.Dir(bin), 0o750); err != nil {
				t.Fatal(err)
			}
			want := []byte("stand-in probe executable")
			if err := os.WriteFile(bin, want, 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(map[string]string{"target_directory": target})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PROBE_TEST_METADATA", string(metadata))
			t.Setenv("CARGO_TARGET_DIR", "")
			t.Setenv("PATH", filepath.Join(repo, "tools")+string(os.PathListSeparator)+os.Getenv("PATH"))
			script := `#!/bin/sh
case "$*" in
  'build --locked --release -p conch-voice --example sdk_probe') exit 0 ;;
  'metadata --locked --no-deps --format-version 1') printf '%s' "$PROBE_TEST_METADATA" ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(repo, "tools", "cargo"), []byte(script), 0o700); err != nil { // #nosec G306 -- test executable in this test's private directory
				t.Fatal(err)
			}
			r := &run{repo: repo, tmp: t.TempDir(), secrets: map[string]string{}}
			if err := r.buildProbe(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(r.probeBin) // #nosec G304 -- private test directory
			if err != nil || string(got) != string(want) {
				t.Fatalf("copied probe = %q, %v; want %q", got, err, want)
			}
			info, err := os.Stat(r.probeBin)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("copied probe must be a private executable: %v, %v", info, err)
			}
		})
	}
}
