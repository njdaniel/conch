//go:build linux

package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Issue #192: the TUI did not exit when SIGINT reached it as a signal. Ctrl-C
// typed at the terminal never showed it, because in raw mode that is a key and
// no signal is raised; `kill -INT`, `timeout -s INT` and a supervisor do raise
// one. These tests stop the built binary each way on a real pseudo-terminal
// and hold every way to the same result: gone at once, exit status 0, and the
// terminal put back.

const (
	leaveAltScreen = "\x1b[?1049l"
	showCursor     = "\x1b[?25h"
	hideCursor     = "\x1b[?25l"

	// How long a stopped TUI may take to be gone before the test fails. An
	// orderly exit takes some tens of milliseconds (most of it a pause Bubble
	// Tea makes after leaving the alternate screen) and the time it took is
	// logged; the bug never exits at all. The limit is several seconds, not
	// the one second the issue asks of the program, because a loaded CI
	// runner under the race detector can take a second over anything.
	stopLimit = 5 * time.Second
)

// signalTUI delivers each signal to the process, as kill(1) does. It does not
// go through the terminal: writing the interrupt character there would be a
// key press, which is the path that always worked.
func signalTUI(signals ...syscall.Signal) func(*testing.T, *session) {
	return func(t *testing.T, s *session) {
		t.Helper()
		for _, sig := range signals {
			// A process that is already gone has done what was asked.
			if err := s.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("send %v: %v", sig, err)
			}
		}
	}
}

func typeAtTUI(keys string) func(*testing.T, *session) {
	return func(t *testing.T, s *session) {
		t.Helper()
		s.send(keys)
	}
}

func TestTUIStopsCleanly(t *testing.T) {
	tests := []struct {
		name string
		stop func(*testing.T, *session)
	}{
		{"SIGINT", signalTUI(syscall.SIGINT)},
		{"SIGTERM", signalTUI(syscall.SIGTERM)},
		// A second signal lands while the first is still being acted on. It
		// must not cut the shutdown short and leave the terminal raw.
		{"SIGINT twice", signalTUI(syscall.SIGINT, syscall.SIGINT)},
		{"SIGINT then SIGTERM", signalTUI(syscall.SIGINT, syscall.SIGTERM)},
		{"SIGTERM then SIGINT", signalTUI(syscall.SIGTERM, syscall.SIGINT)},
		// The key, for comparison: the signals must end the same way it does.
		{"Ctrl-C typed", typeAtTUI("\x03")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := startSession(t, true, []string{"CONCH_SERVER=" + deadServer})
			if !s.waitFor("esc quit", 5*time.Second) {
				t.Fatalf("the TUI never drew its key hints; output %q", s.output())
			}
			if echoOn(t, s.slave) {
				t.Fatal("the TUI is drawn but the terminal still echoes: it is not in raw mode yet")
			}
			drawn := len(s.output())

			start := time.Now()
			tt.stop(t, s)
			exited, err := s.waitExit(stopLimit)
			if !exited {
				t.Fatalf("still running %v after being stopped; it wrote %q", stopLimit, s.output()[drawn:])
			}
			t.Logf("exited after %v", time.Since(start))
			if err != nil {
				t.Errorf("exit status: %v, want 0 as for Ctrl-C", err)
			}

			// The process is gone, so everything it wrote is in the terminal's
			// buffer; give the reader a moment to collect the last of it.
			s.waitFor(leaveAltScreen, fastLimit)
			after := s.output()[drawn:]
			left := strings.LastIndex(after, leaveAltScreen)
			if left < 0 {
				t.Fatalf("the alternate screen was not left; after being stopped it wrote %q", after)
			}
			if shown := strings.LastIndex(after, showCursor); shown < 0 || shown < strings.LastIndex(after, hideCursor) {
				t.Errorf("the cursor was not shown again; after being stopped it wrote %q", after)
			}
			if !echoOn(t, s.slave) {
				t.Error("the terminal was left in raw mode (echo is off)")
			}
			// Stopping the TUI is not a failure, so nothing is reported on the
			// screen the user returns to.
			if tail := after[left:]; strings.Contains(tail, "conch:") {
				t.Errorf("an error was printed on the way out: %q", tail)
			}
		})
	}
}
