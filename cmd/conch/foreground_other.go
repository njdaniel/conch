//go:build !linux

package main

import "os"

// inTerminalForeground cannot ask this system's terminal for its foreground
// job through the standard library, so it never refuses: the TUI starts as it
// always has, and on a system with Linux's job-control stops a background
// start is still stopped by the kernel. Issue #214 is fixed on Linux only.
func inTerminalForeground(*os.File) bool {
	return true
}
