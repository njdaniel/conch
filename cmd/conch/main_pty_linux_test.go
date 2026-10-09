//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/njdaniel/conch/pkg/schema"

	"github.com/njdaniel/conch/internal/cli/termquiet"
)

// These tests run the built conch binary attached to a real pseudo-terminal.
// Bubble Tea's package init asks the terminal for its background colour
// (OSC 11) and the cursor position (CSI 6 n) and waits five seconds for the
// reply; the termquiet package keeps that out of the plain commands. Only a
// real terminal shows the difference, and only the built binary shows what
// Go's package initialization order actually does, so a unit test of
// termquiet alone would not pin the fix.

const (
	osc11Query = "\x1b]11;?"
	cprQuery   = "\x1b[6n"

	// A failing command with nothing to wait for takes milliseconds. The bug
	// costs five seconds, so this is well clear of both noise and the bug.
	fastLimit = 2500 * time.Millisecond

	fakeToken = "conch_FAKE_token_for_pty_test_0123456789" // #nosec G101 -- an obviously fake token

	deadServer = "http://127.0.0.1:1"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildDir  string
	buildErr  string
)

func TestMain(m *testing.M) {
	// A test binary has arguments, so termquiet's init treated it as a plain
	// command and set TERM=dumb. main never runs here, so put it back.
	termquiet.Restore()
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func conchBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "conch-pty-test-")
		if err != nil {
			buildErr = err.Error()
			return
		}
		buildDir = dir
		bin := filepath.Join(dir, "conch")
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput() // #nosec G204 -- fixed arguments
		if err != nil {
			buildErr = err.Error() + "\n" + string(out)
			return
		}
		builtBin = bin
	})
	if buildErr != "" {
		t.Fatalf("go build: %s", buildErr)
	}
	return builtBin
}

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

func setWinsize(t *testing.T, f *os.File, rows, cols uint16) {
	t.Helper()
	ws := [4]uint16{rows, cols, 0, 0}
	ptyIoctl(t, f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws)) // #nosec G103 -- ioctl argument, test only
}

// session is one conch process on a pseudo-terminal, and what it wrote there.
type session struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	cmd    *exec.Cmd
	exited chan error

	mu  sync.Mutex
	out bytes.Buffer
}

// startSession runs conch with args as the foreground job of a new terminal.
// With answer set the terminal replies to the colour and cursor queries the
// way a real one does; without it, it stays silent like the terminals in the
// bug report. The environment is fresh: a temporary HOME and config dir, and
// no inherited CONCH_* variables.
func startSession(t *testing.T, answer bool, extraEnv []string, args ...string) *session {
	t.Helper()
	bin := conchBinary(t)
	master, slave := openPTY(t)
	setWinsize(t, slave, 30, 100)
	home := t.TempDir()
	cmd := exec.Command(bin, args...) // #nosec G204 -- the binary this test just built
	cmd.Dir = home
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TERM=xterm-256color",
	}, extraEnv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start conch: %v", err)
	}
	s := &session{t: t, master: master, slave: slave, cmd: cmd, exited: make(chan error, 1)}
	go func() { s.exited <- cmd.Wait() }()
	go s.pump(answer)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
		}
	})
	return s
}

// pump records everything the program writes and, when answering, replies to
// the two start-up queries.
func (s *session) pump(answer bool) {
	buf := make([]byte, 4096)
	var answeredOSC, answeredCPR int
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.out.Write(buf[:n])
			seen := s.out.String()
			s.mu.Unlock()
			if answer {
				for ; answeredOSC < strings.Count(seen, osc11Query); answeredOSC++ {
					_, _ = s.master.WriteString("\x1b]11;rgb:0000/0000/0000\x1b\\")
				}
				for ; answeredCPR < strings.Count(seen, cprQuery); answeredCPR++ {
					_, _ = s.master.WriteString("\x1b[1;1R")
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *session) waitFor(text string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Contains(s.output(), text) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return strings.Contains(s.output(), text)
}

// waitExit waits for the process and reports whether it finished within limit.
func (s *session) waitExit(limit time.Duration) (exited bool, err error) {
	select {
	case err = <-s.exited:
		s.exited <- err // for the cleanup
		return true, err
	case <-time.After(limit):
		return false, nil
	}
}

func (s *session) send(text string) {
	s.t.Helper()
	if _, err := s.master.WriteString(text); err != nil {
		s.t.Fatalf("write to the terminal: %v", err)
	}
}

func assertNoQueries(t *testing.T, shown string) {
	t.Helper()
	if strings.Contains(shown, osc11Query) {
		t.Errorf("conch asked the terminal for its background colour (OSC 11): %q", shown)
	}
	if strings.Contains(shown, cprQuery) {
		t.Errorf("conch asked the terminal for the cursor position (CSI 6 n): %q", shown)
	}
}

// The regression: a plain command must neither query a terminal that does not
// answer nor wait for it.
func TestPlainCommandsDoNotQueryTheTerminal(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"whoami", []string{"whoami", "--server", deadServer}},
		{"version", []string{"version"}},
		{"send", []string{"send", "--server", deadServer, "--author", "1", "general", "hello"}},
		{"tail", []string{"tail", "--server", deadServer, "general"}},
		{"help", []string{"help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := startSession(t, false, nil, tt.args...)
			start := time.Now()
			exited, _ := s.waitExit(fastLimit)
			if !exited {
				t.Fatalf("still running after %v (the start-up terminal query waits five seconds); output %q", fastLimit, s.output())
			}
			t.Logf("finished in %v", time.Since(start))
			assertNoQueries(t, s.output())
		})
	}
}

// login must reach its prompt at once and receive exactly what was typed: a
// late reply to a start-up query would otherwise be read as part of the token.
func TestLoginReadsExactlyTheTypedToken(t *testing.T) {
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "tester", Role: schema.RoleMember})
	}))
	defer srv.Close()

	s := startSession(t, false, nil, "login", "--server", srv.URL)
	start := time.Now()
	if !s.waitFor("Token: ", fastLimit) {
		t.Fatalf("no token prompt within %v; output %q", fastLimit, s.output())
	}
	t.Logf("prompt after %v", time.Since(start))
	if echoOn(t, s.slave) {
		t.Error("echo is on while the token is read")
	}
	s.send(fakeToken + "\n")
	exited, err := s.waitExit(fastLimit)
	if !exited || err != nil {
		t.Fatalf("login finished = %v, err = %v; output %q", exited, err, s.output())
	}
	mu.Lock()
	got := auth
	mu.Unlock()
	if want := "Bearer " + fakeToken; got != want {
		t.Errorf("the server received Authorization %q, want %q", got, want)
	}
	shown := s.output()
	assertNoQueries(t, shown)
	if strings.Contains(shown, fakeToken) || strings.Contains(shown, fakeToken[:8]+"_") {
		t.Errorf("the terminal displayed the token: %q", shown)
	}
	if !echoOn(t, s.slave) {
		t.Error("echo was left off after login")
	}
}

// The interrupt symptom: Ctrl-C around start-up must leave the terminal with
// the ECHO flag it had.
func TestInterruptLeavesEchoAlone(t *testing.T) {
	t.Run("at the prompt", func(t *testing.T) {
		s := startSession(t, false, nil, "login", "--server", deadServer)
		if !s.waitFor("Token: ", fastLimit) {
			t.Fatalf("no token prompt; output %q", s.output())
		}
		s.send("\x03")
		if exited, _ := s.waitExit(fastLimit); !exited {
			t.Fatalf("did not exit after Ctrl-C; output %q", s.output())
		}
		if !echoOn(t, s.slave) {
			t.Error("echo was left off after Ctrl-C at the prompt")
		}
	})
	t.Run("immediately", func(t *testing.T) {
		s := startSession(t, false, nil, "login", "--server", deadServer)
		s.send("\x03")
		if exited, _ := s.waitExit(fastLimit); !exited {
			t.Fatalf("did not exit after Ctrl-C; output %q", s.output())
		}
		if !echoOn(t, s.slave) {
			t.Error("echo was left off after an early Ctrl-C")
		}
		assertNoQueries(t, s.output())
	})
}

// The fix must not cost the TUI its start-up query: on a terminal that
// answers, conch with no arguments still asks, reaches its first frame, and
// quits on Esc.
func TestTUIStartsOnATerminalThatAnswers(t *testing.T) {
	s := startSession(t, true, []string{"CONCH_SERVER=" + deadServer})
	if !s.waitFor("esc quit", 5*time.Second) {
		t.Fatalf("the TUI never drew its key hints; output %q", s.output())
	}
	shown := s.output()
	if !strings.Contains(shown, osc11Query) || !strings.Contains(shown, cprQuery) {
		t.Errorf("the TUI no longer learns the terminal background; output %q", shown)
	}
	s.send("\x1b")
	if exited, err := s.waitExit(5 * time.Second); !exited || err != nil {
		t.Fatalf("the TUI did not quit on Esc: exited = %v, err = %v", exited, err)
	}
}
