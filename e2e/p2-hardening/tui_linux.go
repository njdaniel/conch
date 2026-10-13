//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// tuiChannelList starts the real TUI on a pseudo-terminal, signed in as who,
// and checks the channels it draws: every one of want, none of unwanted. The
// TUI gets its list from GET /v1/channels (#57).
func (h *harness) tuiChannelList(who *person, want, unwanted []string) error {
	master, slave, err := openPTY()
	if err != nil {
		return fmt.Errorf("a pseudo-terminal for the TUI: %w", err)
	}
	defer func() { _ = master.Close() }()

	cmd := exec.CommandContext(runCtx, who.cli.bin) // #nosec G204 -- this program's own just-built conch binary
	cmd.Env = append(append([]string(nil), who.cli.env...), "TERM=xterm-256color")
	cmd.Dir = who.cli.home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		return fmt.Errorf("start the TUI: %w", err)
	}
	// The child holds the terminal now; with this end closed, the read below
	// ends when the child does.
	_ = slave.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Killed on every way out, an interrupted run included.
	var stop sync.Once
	kill := func() {
		stop.Do(func() {
			_ = cmd.Process.Kill()
			<-exited
		})
	}
	h.onCleanup(kill)
	defer kill()

	// Everything the TUI draws, with the two questions a terminal is asked
	// at start-up answered the way a real one answers them.
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		const colourQuery, cursorQuery = "\x1b]11;?", "\x1b[6n"
		buf := make([]byte, 4096)
		var colours, cursors int
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				screen.Write(buf[:n])
				seen := screen.String()
				mu.Unlock()
				for ; colours < strings.Count(seen, colourQuery); colours++ {
					_, _ = master.WriteString("\x1b]11;rgb:0000/0000/0000\x1b\\")
				}
				for ; cursors < strings.Count(seen, cursorQuery); cursors++ {
					_, _ = master.WriteString("\x1b[1;1R")
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// What was drawn, as text: the escape sequences that colour and place
	// it are removed, so a name is matched as a word and not inside one.
	shown := func() string {
		mu.Lock()
		defer mu.Unlock()
		return escapeSequence.ReplaceAllString(screen.String(), " ")
	}
	lists := func(s, channel string) bool {
		return regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(channel) + `($|[^\w-])`).MatchString(s)
	}
	drawn := func() bool {
		s := shown()
		if !strings.Contains(s, "esc quit") {
			return false
		}
		for _, channel := range want {
			if !lists(s, channel) {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(20 * time.Second)
	for !drawn() {
		if time.Now().After(deadline) {
			return fmt.Errorf("the TUI as %s never drew its key hints and the channels %v", who.name, want)
		}
		if err := pause(20 * time.Millisecond); err != nil {
			return err
		}
	}
	// One more moment for anything it would still draw.
	if err := pause(300 * time.Millisecond); err != nil {
		return err
	}
	for _, channel := range unwanted {
		if lists(shown(), channel) {
			return fmt.Errorf("the TUI as %s shows the channel %q, which %s is not in", who.name, channel, who.name)
		}
	}
	if _, err := master.WriteString("\x1b"); err != nil {
		return err
	}
	select {
	case err := <-exited:
		exited <- err // for kill, which waits for it
		if err != nil {
			return fmt.Errorf("the TUI did not quit cleanly on Esc: %w", err)
		}
	case <-time.After(10 * time.Second):
		return errors.New("the TUI did not quit on Esc")
	}
	if len(unwanted) == 0 {
		step("the TUI, signed in as %s, lists %s", who.name, strings.Join(want, ", "))
	} else {
		step("the TUI, signed in as %s, lists %s and not %s", who.name, strings.Join(want, ", "), strings.Join(unwanted, ", "))
	}
	return nil
}

// escapeSequence matches the terminal control sequences the TUI writes: CSI
// (colour, cursor movement) and OSC (the background-colour question).
var escapeSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?|\x1b[=>()][0-9A-Za-z]?")

// openPTY returns the two ends of a new pseudo-terminal, 30 rows by 100
// columns. The slave end is what a program sees as its terminal.
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
	size := [4]uint16{30, 100, 0, 0}
	if err := ioctl(slave, syscall.TIOCSWINSZ, unsafe.Pointer(&size)); err != nil { // #nosec G103 -- ioctl argument
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
