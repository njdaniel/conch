package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The final leak scan must see the entire stdout stream, including bytes
// Scanner already consumed when it rejects an oversized line. A native log
// line on exit can otherwise lose a secret before any remaining stats wait.
func TestVoiceClientCapturesAllOutputForLeakScan(t *testing.T) {
	const marker = "FAKE_SECRET_IN_STDOUT_PREFIX"
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"ordinary line", marker + "\n"},
		{"line beyond scanner limit", marker + strings.Repeat("x", 5*1024*1024) + "\ntrailing output\n"},
		{"unterminated final line", marker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{tmp: t.TempDir(), secrets: map[string]string{marker: "test stdout secret"}, rooms: map[string][]string{}}
			defer h.teardown()
			fixture := filepath.Join(h.tmp, "output")
			if err := os.WriteFile(fixture, []byte(tc.output), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(h.tmp, "voice-output")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec cat \"$VOICE_TEST_STDOUT\"\n"), 0o700); err != nil { // #nosec G306 -- stand-in test executable in the test's private directory
				t.Fatal(err)
			}
			p := &person{name: "test-speaker", cli: &conchUser{env: append(os.Environ(), "VOICE_TEST_STDOUT="+fixture)}}
			l := &live{h: h, voiceBin: bin}
			c, err := l.startVoiceClient(p, "test-channel", "none", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.stdin.Close() }()
			select {
			case <-c.done:
			case <-time.After(10 * time.Second):
				t.Fatal("stdout capture did not drain the client")
			}
			if c.exit != nil {
				t.Fatalf("stand-in voice client: %v", c.exit)
			}
			if err := h.scanAll(); err == nil || !strings.Contains(err.Error(), "test stdout secret") {
				t.Fatalf("final leak scan must find the secret in stdout: %v", err)
			}
			h.mu.Lock()
			logs := append([]namedFile(nil), h.logs...)
			h.mu.Unlock()
			var captured []byte
			for _, log := range logs {
				if strings.HasPrefix(log.what, "conch-voice's standard output") {
					captured, err = os.ReadFile(log.path) // #nosec G304 -- a captured log in this test's private directory
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if !bytes.Equal(captured, []byte(tc.output)) {
				t.Fatalf("stdout capture changed or lost bytes: got %d, want %d", len(captured), len(tc.output))
			}
		})
	}
}
