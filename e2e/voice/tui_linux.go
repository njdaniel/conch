//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The managed-voice check (managed.go) drives the real `conch` TUI the way a
// person does: keys go in on a pseudo-terminal. What it shows is read from
// the text it writes, escape sequences removed: Bubble Tea writes each line
// it changes whole, so what one line says can be found in that text. Nothing
// else of the TUI's state is read.

// ttyRows and ttyCols are the size of every TUI's terminal: wide enough for
// the voice strip and the people pane.
const (
	ttyRows = 36
	ttyCols = 140
)

// tuiProc is one conch TUI on a pseudo-terminal.
type tuiProc struct {
	who    string
	cmd    *exec.Cmd
	master *os.File
	done   chan struct{}
	logf   *os.File

	mu  sync.Mutex
	raw []byte // everything the TUI wrote
}

// startTUI starts conch with env on a new pseudo-terminal of its own, as the
// leader of a session of its own, so that a signal to it reaches it alone.
func (h *harness) startTUI(who string, env []string) (*tuiProc, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("a pseudo-terminal for %s's TUI: %w", who, err)
	}
	dir, err := h.mkdir("tui-" + who + "-")
	if err != nil {
		_, _ = master.Close(), slave.Close()
		return nil, err
	}
	logPath := filepath.Join(dir, "screen")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		_, _ = master.Close(), slave.Close()
		return nil, err
	}
	cmd := exec.Command(h.bin.conch) // #nosec G204 -- the conch this run built
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_, _, _ = master.Close(), slave.Close(), logf.Close()
		return nil, fmt.Errorf("start %s's TUI: %w", who, err)
	}
	// The child holds the terminal now; with this end closed, reads on the
	// other end fail once the child is gone.
	_ = slave.Close()
	t := &tuiProc{who: who, cmd: cmd, master: master, done: make(chan struct{}), logf: logf}
	go func() { _ = cmd.Wait(); close(t.done) }()
	go t.read()
	h.onCleanup(t.kill)
	h.mu.Lock()
	h.logs = append(h.logs, namedFile{who + "'s TUI screen", logPath, true})
	h.mu.Unlock()
	if err := t.waitText("its key hints", 0, 30*time.Second, "esc quit"); err != nil {
		return nil, err
	}
	return t, nil
}

// read keeps everything the TUI writes and answers the two terminal queries
// it makes at start (the background colour, the cursor position) as a real
// terminal does.
func (t *tuiProc) read() {
	const colourQuery, cursorQuery = "\x1b]11;?", "\x1b[6n"
	buf := make([]byte, 32*1024)
	var tail string
	for {
		n, err := t.master.Read(buf)
		if n > 0 {
			_, _ = t.logf.Write(buf[:n])
			t.mu.Lock()
			t.raw = append(t.raw, buf[:n]...)
			t.mu.Unlock()
			seen := tail + string(buf[:n])
			for i := strings.Count(seen, colourQuery) - strings.Count(tail, colourQuery); i > 0; i-- {
				_, _ = t.master.WriteString("\x1b]11;rgb:0000/0000/0000\x1b\\")
			}
			for i := strings.Count(seen, cursorQuery) - strings.Count(tail, cursorQuery); i > 0; i-- {
				_, _ = t.master.WriteString("\x1b[1;1R")
			}
			tail = seen[max(0, len(seen)-len(colourQuery)+1):]
		}
		if err != nil {
			_ = t.logf.Close()
			return
		}
	}
}

// escapes matches the terminal escape sequences a TUI writes.
var escapes = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[PX^_][^\x1b]*\x1b\\|[()#][0-9A-Za-z]|[=>78DEHMNOZc])`)

// mark is where the TUI's output is now: text written after it is what it
// says from then on.
func (t *tuiProc) mark() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.raw)
}

// textSince is what the TUI wrote after mark, without escape sequences.
func (t *tuiProc) textSince(mark int) string {
	t.mu.Lock()
	raw := bytes.Clone(t.raw[min(mark, len(t.raw)):])
	t.mu.Unlock()
	return strings.ReplaceAll(escapes.ReplaceAllString(string(raw), ""), "\r", "")
}

// waitText waits until the TUI has written want after mark.
func (t *tuiProc) waitText(what string, mark int, within time.Duration, want string) error {
	err := waitForOrFail(t.who+"'s TUI to show "+what, within, func() (bool, string, error) {
		text := t.textSince(mark)
		if strings.Contains(text, want) {
			return true, "", nil
		}
		if t.exited() {
			return false, "", fmt.Errorf("the TUI exited with status %d; the last line it wrote: %s", t.cmd.ProcessState.ExitCode(), lastLine(text))
		}
		return false, "the last voice lines written: " + voiceLines(text), nil
	})
	return err
}

// voiceLines are the last few lines of text that speak of voice: the strip
// and the status line, for a failure message.
func voiceLines(text string) string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "voice"); i >= 0 {
			found = append(found, strings.TrimSpace(line))
		}
	}
	if len(found) > 4 {
		found = found[len(found)-4:]
	}
	return strings.Join(found, " | ")
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// typeText sends keys: text is typed as it is, "\r" is enter and "\x1b" is
// esc.
func (t *tuiProc) typeText(keys string) error {
	if t.exited() {
		return fmt.Errorf("%s's TUI has exited", t.who)
	}
	_, err := t.master.WriteString(keys)
	return err
}

func (t *tuiProc) exited() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// kill ends the TUI with SIGKILL, as a crash or `kill -9` would, and waits
// for it. Only the process this program started is signalled, and only
// before it has been reaped.
func (t *tuiProc) kill() {
	if !t.exited() {
		_ = t.cmd.Process.Kill()
	}
	<-t.done
	_ = t.master.Close()
}

// helperProc is a conch-voice process a TUI started, known by its pid and its
// start time, so that a reused pid is never taken for it.
type helperProc struct {
	pid   int
	start string
}

// procStat is the parent, the command name, the state and the start time of
// pid, from /proc/<pid>/stat.
func procStat(pid int) (ppid int, comm, state, start string, err error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- /proc of a pid
	if err != nil {
		return 0, "", "", "", err
	}
	text := string(data)
	open, closing := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 0 || closing < open {
		return 0, "", "", "", errors.New("unreadable stat")
	}
	fields := strings.Fields(text[closing+1:])
	// After the name: state, ppid, ... and starttime, the 22nd field of the
	// whole line, the 20th after the name.
	if len(fields) < 20 {
		return 0, "", "", "", errors.New("short stat")
	}
	ppid, _ = strconv.Atoi(fields[1])
	return ppid, text[open+1 : closing], fields[0], fields[19], nil
}

// helperOf finds the conch-voice child of the TUI, if it has one.
func (t *tuiProc) helperOf() (helperProc, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return helperProc{}, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, comm, state, start, err := procStat(pid)
		if err == nil && ppid == t.cmd.Process.Pid && comm == "conch-voice" && state != "Z" {
			return helperProc{pid: pid, start: start}, true
		}
	}
	return helperProc{}, false
}

// alive reports whether the helper is still running: the same process, not
// yet exited. A zombie has exited.
func (p helperProc) alive() bool {
	_, _, state, start, err := procStat(p.pid)
	return err == nil && start == p.start && state != "Z"
}

// kill ends a helper left running, for teardown only.
func (p helperProc) kill() {
	if p.alive() {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
}

// mkfifo makes the named pipe that stands in for the key device.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// openPTY returns the two ends of a new pseudo-terminal of ttyRows by ttyCols.
// The slave end is what a program sees as its terminal.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	ioctl := func(f *os.File, req uintptr, arg unsafe.Pointer) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg)); errno != 0 {
			return errno
		}
		return nil
	}
	var unlock int32
	var number uint32
	if err := errors.Join(
		ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)), // #nosec G103 -- ioctl argument
		ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)),   // #nosec G103 -- ioctl argument
	); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	size := [4]uint16{ttyRows, ttyCols, 0, 0}
	if err := ioctl(slave, syscall.TIOCSWINSZ, unsafe.Pointer(&size)); err != nil { // #nosec G103 -- ioctl argument
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
