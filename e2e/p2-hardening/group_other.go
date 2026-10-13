//go:build !unix

package main

import "os/exec"

// ownProcessGroup is where a command would be given a process group of its
// own; this system has none, so cancelling stops the command alone.
func ownProcessGroup(*exec.Cmd) {}
