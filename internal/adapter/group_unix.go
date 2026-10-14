//go:build unix

package adapter

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ownProcessGroup starts cmd in a process group of its own and makes
// cancelling its context kill the whole group: the command and everything it
// started, not only the process the adapter launched.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// waitGroupGone waits, for at most limit, until no process is left in the
// group led by pid, so that a kill has completed and not only been sent.
func waitGroupGone(pid int, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
