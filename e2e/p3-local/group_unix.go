//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcessGroup starts cmd in a process group of its own, so that a signal
// to the group reaches the command and everything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// ownProcessGroup is setProcessGroup, and makes cancelling the command's
// context kill the whole group. Only for a command made with CommandContext.
func ownProcessGroup(cmd *exec.Cmd) {
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// signalProcess sends sig to pid, and to its whole process group when pid
// leads one: that is how this program starts everything, and it reaches a
// fixture script an adapter started. A process that is already gone is not an
// error.
func signalProcess(pid int, termination bool) error {
	sig := syscall.SIGKILL
	if termination {
		sig = syscall.SIGTERM
	}
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		target = -pid
	}
	if err := syscall.Kill(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// processRunning reports whether pid is a live process. A zombie is not: it
// has exited and only waits for its parent to say so.
func processRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- a /proc path built from a number
	if err != nil {
		return true // no /proc here: Kill answered, so it exists
	}
	// The state is the field after the command name, which is in brackets and
	// may itself contain spaces.
	if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] != 'Z'
	}
	return true
}
