//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// setProcessGroup is where a command would be given a process group of its
// own; this system has none.
func setProcessGroup(*exec.Cmd) {}

// ownProcessGroup: cancelling stops the command alone.
func ownProcessGroup(*exec.Cmd) {}

// signalProcess stops pid alone; there are no process groups here.
func signalProcess(pid int, _ bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = p.Kill()
	return nil
}

// processRunning cannot tell here, and says no: this program is for systems
// with process groups, and elsewhere it only has to build.
func processRunning(int) bool { return false }
