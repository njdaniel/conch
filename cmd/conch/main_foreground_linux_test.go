//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Issue #214: the TUI started as a background job of a terminal was stopped by
// the kernel — SIGTTOU when it set the terminal's modes — and sat stopped
// having printed nothing. The fix refuses the start instead: one line on
// standard error, exit status 1, the terminal untouched.
//
// A background job needs a terminal whose foreground job is somebody else, so
// these tests cannot use startSession, which makes its child the foreground
// job. Instead a helper — this test binary re-run with foregroundHelperEnv
// set — becomes the session leader of the pseudo-terminal, then runs the real
// conch binary in a new process group: a background job, as `conch &` makes
// one. The helper waits with WUNTRACED and reports on its own standard output
// how the child ended: "exited N", "stopped SIGTTOU", "signaled N", or
// "timeout". The child's standard error is the helper's, a pipe the test
// reads, so it stays apart from what reaches the terminal.

const (
	foregroundHelperEnv = "CONCH_TEST_FOREGROUND_HELPER"
	helperSlaveEnv      = "CONCH_TEST_SLAVE"
	// helperChildEnv carries the child's whole environment, JSON-encoded: the
	// helper's own environment is tainted by package inits (termquiet forces
	// TERM=dumb in any process with arguments), so the child's is passed whole.
	helperChildEnv = "CONCH_TEST_CHILD_ENV"

	// The helper never waits on the child longer than this; a test that
	// reaches it has found conch running happily in the background, which is
	// its own kind of failure.
	helperDeadline = 20 * time.Second
)

// runForegroundHelper is the helper's main; it never returns. The command to
// run in the background follows a "--" argument.
func runForegroundHelper() {
	fatal := func(v ...any) {
		fmt.Fprintln(os.Stderr, "foreground helper:", fmt.Sprint(v...))
		os.Exit(2)
	}
	report := func(format string, v ...any) {
		if _, err := fmt.Fprintf(os.Stdout, format+"\n", v...); err != nil {
			fatal("write the report:", err)
		}
	}
	sep := -1
	for i, a := range os.Args {
		if a == "--" {
			sep = i
		}
	}
	if sep < 0 || sep+1 >= len(os.Args) {
		fatal("no command after --")
	}
	var env []string
	if err := json.Unmarshal([]byte(os.Getenv(helperChildEnv)), &env); err != nil {
		fatal("child environment:", err)
	}
	if _, err := syscall.Setsid(); err != nil {
		fatal("setsid:", err)
	}
	// A session leader's first terminal opened without O_NOCTTY becomes its
	// controlling terminal, with the leader's process group in the foreground.
	slave, err := os.OpenFile(os.Getenv(helperSlaveEnv), os.O_RDWR, 0) // #nosec G304 G703 -- the test's own pseudo-terminal
	if err != nil {
		fatal("open the terminal:", err)
	}
	argv := os.Args[sep+1:]
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 G702 -- the binary this test just built
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, os.Stderr
	// Setpgid puts the child in a process group of its own, which is not the
	// terminal's foreground group: the child is a background job.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fatal("start the child:", err)
	}
	deadline := time.Now().Add(helperDeadline)
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED|syscall.WNOHANG, nil)
		if err != nil {
			fatal("wait4:", err)
		}
		if pid == 0 { // no change yet
			if time.Now().After(deadline) {
				report("timeout")
				_ = cmd.Process.Kill()
				_, _ = syscall.Wait4(cmd.Process.Pid, &ws, 0, nil)
				break
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		switch {
		case ws.Exited():
			report("exited %d", ws.ExitStatus())
		case ws.Signaled():
			report("signaled %v", ws.Signal())
		case ws.Stopped():
			// The bug this file tests against: report it, then put the child
			// out of its misery so the test can fail and move on.
			report("stopped %s", stopName(ws.StopSignal()))
			_ = cmd.Process.Kill()
			_, _ = syscall.Wait4(cmd.Process.Pid, &ws, 0, nil)
		}
		break
	}
	os.Exit(0)
}

// stopName names the job-control stop signals; syscall.Signal's own string is
// prose ("stopped (tty output)"), which reads badly in the helper's report.
func stopName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTTOU:
		return "SIGTTOU"
	case syscall.SIGTTIN:
		return "SIGTTIN"
	case syscall.SIGTSTP:
		return "SIGTSTP"
	case syscall.SIGSTOP:
		return "SIGSTOP"
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// backgroundRun is what came of running conch as a background job of a
// pseudo-terminal.
type backgroundRun struct {
	report string // the helper's one line: "exited N", "stopped SIGTTOU", ...
	stderr string // what conch wrote to standard error
	term   string // what reached the terminal
	took   time.Duration

	termBefore, termAfter syscall.Termios
}

// runBackground starts the built conch binary with args as a background job of
// a fresh pseudo-terminal and waits for the helper to report its end. The
// environment is fresh, as in startSession: a temporary HOME and config dir,
// no inherited CONCH_* variables.
func runBackground(t *testing.T, extraEnv []string, args ...string) backgroundRun {
	t.Helper()
	bin := conchBinary(t)
	master, slave := openPTY(t)
	setWinsize(t, slave, 30, 100)
	home := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	childEnv := append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TERM=xterm-256color",
	}, extraEnv...)
	envJSON, err := json.Marshal(childEnv)
	if err != nil {
		t.Fatalf("marshal the child's environment: %v", err)
	}
	helper := exec.Command(exe, append([]string{"--", bin}, args...)...) // #nosec G204 -- the test binary and the binary it built
	helper.Dir = home
	helper.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		foregroundHelperEnv + "=1",
		helperSlaveEnv + "=" + slave.Name(),
		helperChildEnv + "=" + string(envJSON),
	}
	var report, stderr bytes.Buffer
	helper.Stdout = &report
	helper.Stderr = &stderr

	var tioBefore syscall.Termios
	ptyIoctl(t, slave, syscall.TCGETS, unsafe.Pointer(&tioBefore)) // #nosec G103 -- ioctl argument, test only

	if err := helper.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}
	var mu sync.Mutex
	var term bytes.Buffer
	go func() { // the reader ends when the cleanup closes the master
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				term.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	start := time.Now()
	waited := make(chan error, 1)
	go func() { waited <- helper.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the helper failed: %v; it said %q", err, stderr.String())
		}
	case <-time.After(helperDeadline + 10*time.Second):
		_ = helper.Process.Kill()
		t.Fatalf("the helper did not finish; its own deadline should have come first")
	}
	took := time.Since(start)
	// The child's last writes can still be in the terminal's buffer when the
	// helper's report arrives; give the reader a moment to collect them.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var tioAfter syscall.Termios
	ptyIoctl(t, slave, syscall.TCGETS, unsafe.Pointer(&tioAfter)) // #nosec G103 -- ioctl argument, test only
	return backgroundRun{
		report:     strings.TrimSpace(report.String()),
		stderr:     stderr.String(),
		term:       term.String(),
		took:       took,
		termBefore: tioBefore,
		termAfter:  tioAfter,
	}
}

// The regression test. On main the child is stopped by SIGTTOU before it can
// say anything, the report is "stopped SIGTTOU", and this test fails; with the
// fix the TUI refuses the start: exit status 1 at once, one line on standard
// error, the terminal as it was.
func TestTUIBackgroundJobRefuses(t *testing.T) {
	r := runBackground(t, []string{"CONCH_SERVER=" + deadServer})
	t.Logf("report %q after %v; terminal got %q", r.report, r.took, r.term)
	if strings.HasPrefix(r.report, "stopped") {
		t.Fatalf("the TUI was stopped by the terminal (%s) having printed nothing — issue #214", r.report)
	}
	if r.report != "exited 1" {
		t.Fatalf("report %q, want %q", r.report, "exited 1")
	}
	if r.took > 5*time.Second {
		t.Errorf("the refusal took %v; refusing is immediate", r.took)
	}
	if want := "conch: " + tuiBackgroundError + "\n"; r.stderr != want {
		t.Errorf("standard error is %q, want exactly %q", r.stderr, want)
	}
	if strings.Contains(r.term, "\x1b[?1049") {
		t.Errorf("the alternate screen was touched from a background job; the terminal got %q", r.term)
	}
	if r.termBefore != r.termAfter {
		t.Errorf("the terminal's settings changed: before %+v, after %+v", r.termBefore, r.termAfter)
	}
}

// The plain commands do not take over the terminal and must keep running as
// background jobs, exactly as before.
func TestPlainCommandsRunInBackground(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // the report, or its prefix
	}{
		{"version", []string{"version"}, "exited 0"},
		// Against an unreachable server these fail at once with an error —
		// which is the command running normally, all this asserts.
		{"whoami", []string{"whoami", "--server", deadServer}, "exited"},
		{"send", []string{"send", "--server", deadServer, "--author", "1", "general", "hello"}, "exited"},
		{"tail", []string{"tail", "--server", deadServer, "general"}, "exited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runBackground(t, nil, tt.args...)
			t.Logf("report %q after %v; stderr %q", r.report, r.took, r.stderr)
			if !strings.HasPrefix(r.report, tt.want) {
				t.Errorf("report %q, want %q", r.report, tt.want)
			}
			if strings.Contains(r.stderr, tuiBackgroundError) {
				t.Errorf("a plain command was refused as a background job: %q", r.stderr)
			}
		})
	}
}

// Without a terminal there is no foreground job to be wrong about: the check
// must not refuse, and the behaviour is what it was — a fast exit with Bubble
// Tea's "error creating cancelreader" on standard error, status 1.
func TestTUINonTerminalNotRefused(t *testing.T) {
	bin := conchBinary(t)
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin) // #nosec G204 -- the binary this test just built
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TERM=xterm-256color",
		"CONCH_SERVER=" + deadServer,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr // stdin stays /dev/null: no terminal anywhere
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the TUI without a terminal did not exit; stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if err == nil {
		t.Fatalf("the TUI without a terminal succeeded; stdout %q", stdout.String())
	}
	if strings.Contains(stderr.String(), tuiBackgroundError) {
		t.Errorf("refused without a terminal: %q", stderr.String())
	}
	t.Logf("non-terminal behaviour, unchanged: %v, stderr %q", err, strings.TrimSpace(stderr.String()))
}
