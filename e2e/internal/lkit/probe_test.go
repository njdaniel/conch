package lkit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker puts a docker script on PATH; "" leaves no docker at all.
func fakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if script != "" {
		// #nosec G306 -- a test script has to be executable
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestProbeDocker(t *testing.T) {
	// The first call stays silent (exec, so killing it frees the pipe) and the
	// second answers: a daemon that is slow to answer its first call. The
	// sleep is found by absolute path because PATH holds only the fake.
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	slowThenOK := `if [ -e "$0.seen" ]; then echo 24.0.0; exit 0; fi
: > "$0.seen"
exec ` + sleep + ` 30
`
	tests := []struct {
		name    string
		script  string // "" means no docker binary on PATH
		wantErr bool
		wantOut string
		within  time.Duration
	}{
		{"answers after a slow first call", slowThenOK, false, "24.0.0", 3 * time.Second},
		{"no docker binary fails at once", "", true, "executable file not found", time.Second},
		{"daemon that refuses fails at once", "echo 'Cannot connect to the Docker daemon' >&2\nexit 1\n", true, "Cannot connect", time.Second},
		{"daemon that never answers fails within the bound", "exec " + sleep + " 30\n", true, "", 3 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDocker(t, tc.script)
			start := time.Now()
			out, err := probeDocker(context.Background(), 300*time.Millisecond, 1200*time.Millisecond, 50*time.Millisecond)
			if el := time.Since(start); el > tc.within {
				t.Errorf("took %v, want under %v", el, tc.within)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			msg := out
			if err != nil {
				msg += " " + err.Error()
			}
			if !strings.Contains(msg, tc.wantOut) {
				t.Errorf("output %q err %v, want %q", out, err, tc.wantOut)
			}
		})
	}
}
