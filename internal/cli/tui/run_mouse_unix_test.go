//go:build unix

package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// Issue #83: the setting reaches the terminal. Mouse reporting is a DEC
// private mode the program switches on by writing to its output.
func TestRunMouseReporting(t *testing.T) {
	const cellMotion = "\x1b[?1002h"
	tests := []struct {
		name  string
		mouse bool
	}{
		{"on", true},
		{"off", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			keys, keyboard := io.Pipe()
			defer func() { _ = keyboard.Close() }()
			out := &screen{}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, stubAPI{}, 1, false, []string{"general"}, tt.mouse, nil, keys, out) }()
			if !out.waitFor("esc quit", 5*time.Second) {
				t.Fatalf("the TUI never drew its key hints; output %q", out.String())
			}
			if got := strings.Contains(out.String(), cellMotion); got != tt.mouse {
				t.Errorf("mouse reporting switched on = %v, want %v; output %q", got, tt.mouse, out.String())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Run had not returned 3s after its context ended")
			}
		})
	}
}
