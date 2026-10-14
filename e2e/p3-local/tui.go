package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// The terminal client is driven the way a person drives it: keys go in on a
// pseudo-terminal, and what the person would read is the screen those keys and
// the server's frames have left. Nothing here reads the TUI's state any other
// way.

const (
	keyEnter = "\r"
	keyTab   = "\t"
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	keyEsc   = "\x1b"
	keyCtrlC = "\x03"
)

// ttyRows and ttyCols are the size of every TUI's terminal: wide enough for a
// message line and the members line, tall enough for the Agents view's runs.
const (
	ttyRows = 30
	ttyCols = 100
)

// tui is one terminal client on a pseudo-terminal.
type tui struct {
	s      *suite
	part   string
	master *os.File
	c      *child

	mu  sync.Mutex
	scr *screen
}

// rows is the screen as it is now, one string per row.
func (t *tui) rows() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scr.lines()
}

// press sends keys. A key that is a name from the constants above goes as the
// bytes a terminal sends for it; anything else is typed.
func (t *tui) press(keys string) error {
	if !t.c.running() {
		return fmt.Errorf("the TUI of %s has exited", t.part)
	}
	_, err := t.master.WriteString(keys)
	return err
}

// typeText types text one key at a time: a letter such as m or x is a command
// in some views, and must reach the TUI as the key it is.
func (t *tui) typeText(text string) error {
	for _, r := range text {
		if err := t.press(string(r)); err != nil {
			return err
		}
		if err := pause(2 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// waitFor polls the screen until ok holds, or fails with what the person was
// waiting for and the screen they were looking at.
func (t *tui) waitFor(what string, d time.Duration, ok func(rows []string) bool) error {
	deadline := time.Now().Add(d)
	for {
		rows := t.rows()
		if ok(rows) {
			return nil
		}
		if !t.c.running() {
			return fmt.Errorf("the TUI of %s exited while waiting for %s; the screen was:\n%s", t.part, what, dump(rows))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the TUI of %s did not show %s within %s; the screen was:\n%s", t.part, what, d, dump(rows))
		}
		if err := pause(25 * time.Millisecond); err != nil {
			return err
		}
	}
}

// dump is a screen for a failure message, blank rows left out.
func dump(rows []string) string {
	var b strings.Builder
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			b.WriteString("  | " + r + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// showing says some row contains every one of parts.
func showing(rows []string, parts ...string) bool { return countRows(rows, parts...) > 0 }

// countRows is how many rows contain every one of parts.
func countRows(rows []string, parts ...string) int {
	n := 0
rows:
	for _, r := range rows {
		for _, p := range parts {
			if !strings.Contains(r, p) {
				continue rows
			}
		}
		n++
	}
	return n
}

// channelList is the channels the left pane lists, in order, and the index of
// the one it marks (-1 when none is).
func channelList(rows []string) (names []string, marked int) {
	marked = -1
	for _, r := range rows {
		if !strings.HasPrefix(r, "│") {
			continue
		}
		pane, _, _ := strings.Cut(strings.TrimPrefix(r, "│"), "│")
		isMarked := strings.HasPrefix(pane, "› ")
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(pane, "› "), "  "))
		if name == "" {
			continue
		}
		if isMarked {
			marked = len(names)
		}
		names = append(names, name)
	}
	return names, marked
}

// channelsLoaded says the left pane lists channels and marks one, which it
// does not while it still shows "loading…".
func channelsLoaded(rows []string) bool {
	_, marked := channelList(rows)
	return marked >= 0
}

// selectedChannel is the channel the left pane marks, "" when none shows.
func selectedChannel(rows []string) string {
	names, marked := channelList(rows)
	if marked < 0 {
		return ""
	}
	return names[marked]
}

// selectChannel moves to channel with the arrow keys, one step at a time, and
// waits for the TUI to mark it.
func (t *tui) selectChannel(channel string) error {
	// A TUI that has only just started shows "loading…" in the left pane; a
	// list that has loaded without the channel fails below at once.
	if err := t.waitFor("the channel list of "+t.part+" to load", 15*time.Second, channelsLoaded); err != nil {
		return err
	}
	for range 8 {
		names, marked := channelList(t.rows())
		if marked >= 0 && names[marked] == channel {
			return t.waitFor("the prompt of "+channel, 10*time.Second, func(r []string) bool { return showing(r, channel+" >") })
		}
		target := slices.Index(names, channel)
		if marked < 0 || target < 0 {
			return fmt.Errorf("the TUI of %s does not list the channel %s; the screen was:\n%s", t.part, channel, dump(t.rows()))
		}
		key := keyDown
		if target < marked {
			key = keyUp
		}
		if err := t.press(key); err != nil {
			return err
		}
		from := names[marked]
		if err := t.waitFor("the mark to leave "+from, 10*time.Second, func(r []string) bool { return selectedChannel(r) != from }); err != nil {
			return err
		}
	}
	return fmt.Errorf("the TUI of %s did not reach %s", t.part, channel)
}

// sendLine types text at the prompt and presses enter.
func (t *tui) sendLine(text string) error {
	if err := t.typeText(text); err != nil {
		return err
	}
	return t.press(keyEnter)
}

// quit presses esc and waits for the program to end, as a person leaving does.
func (t *tui) quit() error {
	if err := t.press(keyEsc); err != nil {
		return err
	}
	select {
	case <-t.c.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("the TUI of %s did not quit on esc", t.part)
	case <-runCtx.Done():
		return runCtx.Err()
	}
}

func (t *tui) kill() {
	t.c.kill()
	_ = t.master.Close()
}
