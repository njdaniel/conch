//go:build linux

package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdingFixture hands the test the control of the command it runs.
type holdingFixture struct{ control chan processControl }

func (*holdingFixture) approval(context.Context, string) error { return nil }
func (h *holdingFixture) commandStarted(p processControl)      { h.control <- p }

// procState is the state letter of a process in /proc/<pid>/stat, or "" when
// it is gone.
func procState(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || len(data) < i+3 {
		return ""
	}
	return string(data[i+2])
}

func waitProcState(t *testing.T, pid int, ok func(string) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok(procState(pid)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: the command is in state %q", what, procState(pid))
}

func isStopped(s string) bool { return s == "T" }
func isRunning(s string) bool { return s == "S" || s == "R" }

// startSleeper runs `sleep` as the command and returns its pid and control.
func startSleeper(t *testing.T, ctx context.Context, runner commandRunner) (pid int, ctl *commandControl, result chan error) {
	t.Helper()
	fx := &holdingFixture{control: make(chan processControl, 1)}
	result = make(chan error, 1)
	go func() {
		_, err := runner.Reply(ctx, "p", fx)
		result <- err
	}()
	select {
	case p := <-fx.control:
		ctl = p.(*commandControl)
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not start")
	}
	ctl.mu.Lock()
	pid = ctl.pid
	ctl.mu.Unlock()
	return pid, ctl, result
}

func sleeper(t *testing.T, seconds string, timeout, maxPause time.Duration) commandRunner {
	t.Helper()
	spec, err := parseCommand("sleep "+seconds, "")
	if err != nil {
		t.Fatal(err)
	}
	return commandRunner{spec: spec, timeout: timeout, maxPause: maxPause, sleep: sleepContext}
}

// A real command is stopped (state T) by a pause and runs again after a
// resume; a cancel while it is stopped leaves nothing of its group.
func TestPauseAndResumeARealCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid, ctl, result := startSleeper(t, ctx, sleeper(t, "30", time.Minute, time.Minute))
	waitProcState(t, pid, isRunning, "before the pause")

	if err := ctl.pause(); err != nil {
		t.Fatal(err)
	}
	waitProcState(t, pid, isStopped, "after pause")
	if err := ctl.pause(); !errors.Is(err, errAlreadyPaused) {
		t.Fatalf("pause when paused = %v, want errAlreadyPaused", err)
	}

	if err := ctl.resume(); err != nil {
		t.Fatal(err)
	}
	waitProcState(t, pid, isRunning, "after resume")
	if err := ctl.resume(); !errors.Is(err, errNotPaused) {
		t.Fatalf("resume when running = %v, want errNotPaused", err)
	}

	if err := ctl.pause(); err != nil {
		t.Fatal(err)
	}
	waitProcState(t, pid, isStopped, "paused again")
	cancel() // a cancel kills a stopped group
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reply = %v, want the cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped command was not killed by the cancel")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a process of the group is left after the cancel: %v", err)
	}
	if err := ctl.pause(); !errors.Is(err, errNotRunning) {
		t.Fatalf("pause after the command ended = %v, want errNotRunning", err)
	}
}

// Time spent paused does not count against the reply timeout.
func TestPauseDoesNotUseTheReplyTimeout(t *testing.T) {
	tests := []struct {
		name     string
		command  string // seconds `sleep` runs for
		pauseFor time.Duration
		wantErr  error
	}{
		// 1 s of timeout, a command that needs about half of it, and a pause
		// longer than all of it: without the extension the timeout would
		// fire during the pause.
		{"paused for longer than the timeout", "0.5", 1200 * time.Millisecond, nil},
		{"never paused, a command that outlasts the timeout", "30", 0, errReplyTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctl, result := startSleeper(t, context.Background(), sleeper(t, tt.command, time.Second, 10*time.Second))
			if tt.pauseFor > 0 {
				if err := ctl.pause(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(tt.pauseFor)
				if err := ctl.resume(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-result:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Reply = %v, want %v", err, tt.wantErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the reply did not end")
			}
		})
	}
}

// A pause past the bound kills the command and says so.
func TestPausedTooLongKillsTheCommand(t *testing.T) {
	pid, ctl, result := startSleeper(t, context.Background(), sleeper(t, "30", time.Minute, 200*time.Millisecond))
	if err := ctl.pause(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var tooLong *pausedTooLongError
		if !errors.As(err, &tooLong) || tooLong.Error() != "paused longer than 200ms" {
			t.Fatalf("Reply = %v, want paused longer than 200ms", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the paused command was not killed")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a process of the group is left: %v", err)
	}
}

// The bound is for all the pauses of a run together: pausing and resuming
// again and again does not stretch a reply past it.
func TestPausesShareOneBound(t *testing.T) {
	// 300 ms of pause in all; five pauses of 150 ms are 750 ms.
	_, ctl, result := startSleeper(t, context.Background(), sleeper(t, "30", time.Minute, 300*time.Millisecond))
	go func() {
		for range 5 {
			if err := ctl.pause(); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
			if err := ctl.resume(); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-result:
		var tooLong *pausedTooLongError
		if !errors.As(err, &tooLong) || tooLong.Error() != "paused longer than 300ms" {
			t.Fatalf("Reply = %v, want paused longer than 300ms", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pausing again and again kept the command alive past the bound")
	}
}

// The signals reach the whole group: a process the command started is
// stopped and continued with it.
func TestPauseReachesTheWholeGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	spec, err := parseCommand(`sh -c 'sleep 300 & echo $! > `+pidFile+`; wait' sh`, "")
	if err != nil {
		t.Fatal(err)
	}
	runner := commandRunner{spec: spec, timeout: time.Minute, maxPause: time.Minute, sleep: sleepContext}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader, ctl, result := startSleeper(t, ctx, runner)
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for grandchild == 0 && time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // a file this test made in its own temp directory
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if grandchild == 0 || grandchild == leader {
		t.Fatalf("no grandchild started (pid %d)", grandchild)
	}
	waitProcState(t, grandchild, isRunning, "grandchild before the pause")

	if err := ctl.pause(); err != nil {
		t.Fatal(err)
	}
	waitProcState(t, leader, isStopped, "leader after pause")
	waitProcState(t, grandchild, isStopped, "grandchild after pause")
	if err := ctl.resume(); err != nil {
		t.Fatal(err)
	}
	waitProcState(t, grandchild, isRunning, "grandchild after resume")

	cancel()
	<-result
	if err := syscall.Kill(grandchild, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the grandchild outlived the cancel: %v", err)
	}
}
