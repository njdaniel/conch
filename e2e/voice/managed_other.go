//go:build !linux

package main

import "context"

// runManaged needs a Linux pseudo-terminal, /proc and conch-voice, which is
// built for Linux only.
func runManaged(context.Context, *harness) (string, error) {
	return "the managed-voice check: it runs on Linux only", nil
}
