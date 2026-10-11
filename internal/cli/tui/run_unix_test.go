//go:build unix

package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// screen collects what the program writes; the renderer writes from its own
// goroutine while the test reads.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *screen) waitFor(text string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Contains(s.String(), text) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return strings.Contains(s.String(), text)
}

// Issue #192. Run's context is the only thing that stops it from outside: a
// signal the caller has not turned into the end of that context must do
// nothing, because a second handler inside Bubble Tea is what deadlocked the
// shutdown. The context ending must then return promptly, without an error,
// and with the terminal sequences undone. Neither end is a terminal here, so
// this needs no pseudo-terminal; cmd/conch has the test of the real binary.
func TestRunStopsOnlyWithItsContext(t *testing.T) {
	const (
		leaveAltScreen = "\x1b[?1049l"
		showCursor     = "\x1b[?25h"
		hideCursor     = "\x1b[?25l"
	)
	tests := []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The test process is about to signal itself. Asking for the
			// signal here is what keeps that from killing it, and tells us
			// when the signal has been delivered.
			delivered := make(chan os.Signal, 1)
			signal.Notify(delivered, tt.sig)
			defer signal.Stop(delivered)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			keys, keyboard := io.Pipe()
			defer func() { _ = keyboard.Close() }() // ends the program's read of its input
			out := &screen{}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, stubAPI{}, 1, false, []string{"general"}, keys, out) }()
			if !out.waitFor("esc quit", 5*time.Second) {
				t.Fatalf("the TUI never drew its key hints; output %q", out.String())
			}

			if err := syscall.Kill(syscall.Getpid(), tt.sig); err != nil {
				t.Fatalf("signal this process: %v", err)
			}
			select {
			case <-delivered:
			case <-time.After(5 * time.Second):
				t.Fatalf("%v was never delivered", tt.sig)
			}
			// A handler of Bubble Tea's own would have ended the program by
			// now; that takes well under a millisecond.
			select {
			case err := <-done:
				t.Fatalf("Run returned (%v) on a signal its caller did not act on: Bubble Tea's own signal handler is on", err)
			case <-time.After(100 * time.Millisecond):
			}

			drawn := len(out.String())
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run = %v after its context ended, want nil", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run had not returned 3s after its context ended")
			}
			after := out.String()[drawn:]
			if !strings.Contains(after, leaveAltScreen) {
				t.Errorf("the alternate screen was not left; on the way out it wrote %q", after)
			}
			if shown := strings.LastIndex(after, showCursor); shown < 0 || shown < strings.LastIndex(after, hideCursor) {
				t.Errorf("the cursor was not shown again; on the way out it wrote %q", after)
			}
		})
	}
}
