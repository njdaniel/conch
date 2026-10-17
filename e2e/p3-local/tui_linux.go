//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// startTUI starts the terminal client of part on a pseudo-terminal, with the
// printed command: the participant's environment file and then conch. The
// terminal answers the two questions the client asks at start-up (the
// background colour and the cursor position) the way a real one does.
func (s *suite) startTUI(part string) (*tui, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("a pseudo-terminal for the TUI: %w", err)
	}
	r := s.r
	// exec, so that the process the suite signals is the client itself.
	cmd := exec.Command("sh", "-c", r.source(part+".env")+"exec "+shq(r.bin.conch)) // #nosec G204 -- the conch binary this run built, and its own environment file
	cmd.Env = r.isolatedEnv("TERM=xterm-256color", "CONCH_MOUSE=0")
	cmd.Dir = r.path("home")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, fmt.Errorf("start the TUI of %s: %w", part, err)
	}
	// The child holds the terminal now; with this end closed, reads on the
	// other end fail once the child is gone.
	_ = slave.Close()
	c := &child{name: part + "-tui", cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.done) }()
	r.mu.Lock()
	r.children = append(r.children, c)
	r.mu.Unlock()
	if err := r.record(c.name, c.pid()); err != nil {
		c.kill()
		_ = master.Close()
		return nil, err
	}
	t := &tui{s: s, part: part, master: master, c: c, scr: newScreen(ttyRows, ttyCols)}
	go t.read()
	s.cleanup(t.kill)
	if err := t.waitFor("its key hints", 20*time.Second, func(rows []string) bool { return showing(rows, "esc quit") }); err != nil {
		return nil, err
	}
	return t, nil
}

// read applies everything the client writes to the screen, and answers the
// terminal queries in it.
func (t *tui) read() {
	const colourQuery, cursorQuery = "\x1b]11;?", "\x1b[6n"
	buf := make([]byte, 4096)
	var tail string
	for {
		n, err := t.master.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.scr.write(buf[:n])
			t.mu.Unlock()
			// A query may be cut between two reads, so the end of the last read
			// is kept; a query that lies whole in it was answered then.
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
			return
		}
	}
}

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
