package lkit

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// KillGroup kills cmd and everything it started: children are put in their own
// process group, so that `go run`'s child, or dogfood's conchd, cannot outlive
// the program.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// GroupCommand is exec.CommandContext for a child that may have children of
// its own: it runs in its own process group, and cancelling ctx kills the
// group, not only the child.
func GroupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- every caller passes a constant command or a binary this program built or pinned
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// GroupStart is exec.Command in its own process group, for a long-lived child
// (conchd, lk) that teardown kills with KillGroup.
func GroupStart(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) // #nosec G204 -- every caller passes a binary this program built or pinned
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}
