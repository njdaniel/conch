//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and makes
// cancelling its context kill the whole group: the command and everything it
// started.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
