//go:build linux

package cli

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// These tests run readSecretNoEcho against a real pseudo-terminal. The other
// login tests replace it through the readSecret seam, so without these a read
// that echoed, or a cancel that left echo off, would pass the suite.

func ptyIoctl(t *testing.T, f *os.File, req uintptr, arg unsafe.Pointer) {
	t.Helper()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg)); errno != 0 {
		t.Skipf("pseudo-terminal ioctl %#x unavailable here: %v", req, errno)
	}
}

// openPTY returns the two ends of a new pseudo-terminal. The slave end is
// what a program sees as its terminal.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	ptyIoctl(t, master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)) // #nosec G103 -- ioctl argument, test only
	var n uint32
	ptyIoctl(t, master, syscall.TIOCGPTN, unsafe.Pointer(&n)) // #nosec G103 -- ioctl argument, test only
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal's slave end: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func echoOn(t *testing.T, f *os.File) bool {
	t.Helper()
	var tio syscall.Termios
	ptyIoctl(t, f, syscall.TCGETS, unsafe.Pointer(&tio)) // #nosec G103 -- ioctl argument, test only
	return tio.Lflag&syscall.ECHO != 0
}

func waitEcho(t *testing.T, f *os.File, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for echoOn(t, f) != want {
		if time.Now().After(deadline) {
			t.Fatalf("echo did not become %v", want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// drain returns what the terminal displayed: everything the master end has to
// read within a short window.
func drain(t *testing.T, master *os.File) string {
	t.Helper()
	// Take the descriptor once: every call to Fd puts it back in blocking mode.
	fd := int(master.Fd()) //nolint:gosec // fds fit in int
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			out.Write(buf[:n])
			continue
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return out.String()
}

type secretResult struct {
	secret []byte
	err    error
}

func startRead(ctx context.Context, slave *os.File) <-chan secretResult {
	done := make(chan secretResult, 1)
	go func() {
		secret, err := readSecretNoEcho(ctx, int(slave.Fd())) //nolint:gosec // fds fit in int
		done <- secretResult{secret, err}
	}()
	return done
}

func TestReadSecretNoEchoOnARealTerminal(t *testing.T) {
	const secret = "conch_FAKE_token_for_pty_test_0123456789" // #nosec G101 -- an obviously fake token
	master, slave := openPTY(t)
	if !echoOn(t, slave) {
		t.Fatal("a fresh pseudo-terminal should echo")
	}
	done := startRead(context.Background(), slave)
	waitEcho(t, slave, false)
	if _, err := master.WriteString(secret + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || string(got.secret) != secret {
			t.Fatalf("read = %q, %v", got.secret, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read did not return")
	}
	if !echoOn(t, slave) {
		t.Error("echo was left off after a successful read")
	}
	if shown := drain(t, master); strings.Contains(shown, secret) || strings.Contains(shown, secret[:8]) {
		t.Errorf("the terminal displayed the secret: %q", shown)
	}
}

func TestReadSecretNoEchoRestoresTheTerminalOnCancel(t *testing.T) {
	master, slave := openPTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startRead(ctx, slave)
	waitEcho(t, slave, false)
	if _, err := master.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.secret != nil {
			t.Fatalf("read = %q, %v; want nothing and context.Canceled", got.secret, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read did not return after the cancel")
	}
	if !echoOn(t, slave) {
		t.Error("echo was left off after a cancelled read")
	}
	// Let the abandoned reader finish so it does not outlive the test.
	_, _ = master.WriteString("\n")
}

// A context cancelled before the read starts must not touch the terminal: an
// abandoned reader could otherwise turn echo off after the restore.
func TestReadSecretNoEchoDoesNotStartWhenAlreadyCancelled(t *testing.T) {
	_, slave := openPTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secret, err := readSecretNoEcho(ctx, int(slave.Fd())) //nolint:gosec // fds fit in int
	if !errors.Is(err, context.Canceled) || secret != nil {
		t.Fatalf("read = %q, %v; want nothing and context.Canceled", secret, err)
	}
	time.Sleep(3 * echoSettle)
	if !echoOn(t, slave) {
		t.Error("echo was turned off by a read that should never have started")
	}
}
