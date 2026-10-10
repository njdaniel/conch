package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// stopCtx is cancelled by the first SIGINT or SIGTERM. Every child process is
// started under it and every wait in this program watches it, so a signal
// makes the run unwind through its normal exit path, in which teardown runs
// once and after nothing is in flight. Teardown is never run from the signal
// handler, because a child being started at that moment would escape it.
var stopCtx, stopCancel = context.WithCancel(context.Background())

// errInterrupted is what a wait returns once stopCtx is cancelled.
var errInterrupted = errors.New("interrupted by a signal")

// killGroup kills cmd and everything it started: children are put in their own
// process group, so that `go run`'s child, or dogfood's conchd, cannot outlive
// the program.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// groupCommand is exec.CommandContext for a child that may have children of
// its own: it runs in its own process group, and cancelling ctx kills the
// group, not only the child.
func groupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- every caller passes a constant command or a binary this program built or pinned
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// groupStart is exec.Command in its own process group, for a long-lived child
// (conchd, lk) that teardown kills with killGroup.
func groupStart(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) // #nosec G204 -- every caller passes a binary this program built or pinned
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}
