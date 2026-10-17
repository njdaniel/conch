//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The TUI starts against the only stored login's server, not the default, and
// when that server cannot be reached it says so in words instead of opening a
// stand-in channel (issue #522). The login is for a port nothing listens on,
// so the start-up is fast and the failure certain.
func TestTUIUsesTheOnlyStoredLogin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := `{"` + deadServer + `": {"token": "` + fakeToken + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startSession(t, true, []string{"CONCH_CONFIG_DIR=" + dir})
	if !s.waitFor("Cannot read the channel list from "+deadServer+".", 5*time.Second) {
		t.Fatalf("the TUI never said it could not read the channel list from the stored login's server; output %q", s.output())
	}
	s.send("\x1b")
	if exited, err := s.waitExit(5 * time.Second); !exited || err != nil {
		t.Fatalf("the TUI did not quit on Esc: exited = %v, err = %v", exited, err)
	}
}
