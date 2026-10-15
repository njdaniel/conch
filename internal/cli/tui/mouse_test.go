package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #83. These tests drive Model.Update with synthetic mouse events; no
// terminal and no socket is involved.

func press(button tea.MouseButton, x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: button}
}

func click(x, y int) tea.MouseMsg { return press(tea.MouseButtonLeft, x, y) }

func wheel(up bool, x, y int) tea.MouseMsg {
	if up {
		return press(tea.MouseButtonWheelUp, x, y)
	}
	return press(tea.MouseButtonWheelDown, x, y)
}

// mouseModel is a channels view of channelNames with n messages in the first.
func mouseModel(width, height, n int) Model {
	m := NewModel(context.Background(), stubAPI{}, 7, []string{"general", "ops", "dev", "lab"})
	m.width, m.height = width, height
	// These tests are about the mouse; read state has its own.
	m.rs.off = true
	for i := 1; i <= n; i++ {
		m.messages["general"] = append(m.messages["general"], schema.MessageV2{ID: int64(i), AuthorID: 4, Body: fmt.Sprintf("message %02d", i)})
	}
	return m
}

func updated(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return got, cmd
}

// find is where text is first drawn in the view: its screen row and column.
func find(t *testing.T, m Model, text string) (x, y int) {
	t.Helper()
	for row, line := range strings.Split(m.View(), "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return utf8.RuneCountInString(line[:i]), row
		}
	}
	t.Fatalf("%q is not on the screen:\n%s", text, m.View())
	return 0, 0
}

func TestMouseClickSelectsChannel(t *testing.T) {
	for _, width := range []int{80, 50} {
		leftW, _ := paneWidths(width)
		tests := []struct {
			name     string
			x, y     int
			selected int
			cmd      bool
		}{
			{"first row", 2, 1, 0, false},
			{"second row", 2, 2, 1, true},
			{"last row", 2, 4, 3, true},
			{"right edge of the content", leftW - 2, 3, 2, true},
			{"below the last row", 2, 5, 0, false},
			{"bottom of the pane", 2, 10, 0, false},
			{"left border", 0, 2, 0, false},
			{"right border", leftW - 1, 2, 0, false},
			{"top border", 2, 0, 0, false},
			{"bottom border", 2, 21, 0, false},
			{"message pane", leftW + 3, 2, 0, false},
			{"input line", 2, 22, 0, false},
			{"status line", 2, 23, 0, false},
			{"outside the screen", 2, 99, 0, false},
			{"negative", -1, -1, 0, false},
		}
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%d/%s", width, tt.name), func(t *testing.T) {
				m := mouseModel(width, 24, 0)
				m, cmd := updated(t, m, click(tt.x, tt.y))
				if m.selected != tt.selected {
					t.Errorf("selected = %d, want %d", m.selected, tt.selected)
				}
				if tt.cmd != (cmd != nil) {
					t.Errorf("command returned = %v, want %v", cmd != nil, tt.cmd)
				}
				if tt.cmd {
					// The same effect as moving there with the arrow keys.
					if !m.subscribed[m.current()] || m.channelStatus != "loading…" {
						t.Errorf("subscribed = %v, status %q: not selected like the arrow keys do", m.subscribed[m.current()], m.channelStatus)
					}
				}
			})
		}
	}
}

// What is drawn is what is hit, at several sizes and after a resize.
func TestMouseHitsWhatIsDrawn(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {50, 24}, {59, 30}, {60, 20}, {120, 40}} {
		m := mouseModel(size[0], size[1], 0)
		for _, name := range m.channels {
			x, y := find(t, m, name)
			got, _ := updated(t, m, click(x, y))
			if got.current() != name {
				t.Errorf("%dx%d: clicking %q at (%d,%d) selected %q", size[0], size[1], name, x, y, got.current())
			}
		}
	}
	m := mouseModel(80, 24, 0)
	m, _ = updated(t, m, tea.WindowSizeMsg{Width: 50, Height: 30})
	x, y := find(t, m, "lab")
	if got, _ := updated(t, m, click(x, y)); got.current() != "lab" {
		t.Errorf("after a resize, clicking lab at (%d,%d) selected %q", x, y, got.current())
	}
}

// visibleMessages are the numbers of the first and last "message NN" drawn.
func visibleMessages(t *testing.T, m Model) (first, last int) {
	t.Helper()
	first, last = -1, -1
	for _, line := range strings.Split(m.View(), "\n") {
		var n int
		if i := strings.Index(line, "message "); i >= 0 {
			if _, err := fmt.Sscanf(line[i:], "message %d", &n); err == nil {
				if first < 0 {
					first = n
				}
				last = n
			}
		}
	}
	return first, last
}

func TestMouseWheelScrollsMessages(t *testing.T) {
	// 80x24: 20 content rows, 19 message lines; 40 messages, so 21 can be
	// scrolled off at most.
	const rx, ry = 40, 5
	tests := []struct {
		name        string
		events      []tea.MouseMsg
		scroll      int
		first, last int
	}{
		{"none", nil, 0, 22, 40},
		{"one notch up", []tea.MouseMsg{wheel(true, rx, ry)}, 1, 21, 39},
		{"three up", []tea.MouseMsg{wheel(true, rx, ry), wheel(true, rx, ry), wheel(true, rx, ry)}, 3, 19, 37},
		{"up then down", []tea.MouseMsg{wheel(true, rx, ry), wheel(true, rx, ry), wheel(false, rx, ry)}, 1, 21, 39},
		{"down at the newest", []tea.MouseMsg{wheel(false, rx, ry)}, 0, 22, 40},
		{"on the border", []tea.MouseMsg{wheel(true, 18, 0)}, 1, 21, 39},
		{"over the left pane", []tea.MouseMsg{wheel(true, 2, ry)}, 0, 22, 40},
		{"over the input line", []tea.MouseMsg{wheel(true, 40, 22)}, 0, 22, 40},
		{"release is ignored", []tea.MouseMsg{{X: rx, Y: ry, Action: tea.MouseActionRelease, Button: tea.MouseButtonWheelUp}}, 0, 22, 40},
		{"motion is ignored", []tea.MouseMsg{{X: rx, Y: ry, Action: tea.MouseActionMotion, Button: tea.MouseButtonWheelUp}}, 0, 22, 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mouseModel(80, 24, 40)
			for _, e := range tt.events {
				var cmd tea.Cmd
				m, cmd = updated(t, m, e)
				if cmd != nil {
					t.Fatal("a wheel event returned a command")
				}
			}
			if m.scroll != tt.scroll {
				t.Errorf("scroll = %d, want %d", m.scroll, tt.scroll)
			}
			if first, last := visibleMessages(t, m); first != tt.first || last != tt.last {
				t.Errorf("shows messages %d..%d, want %d..%d", first, last, tt.first, tt.last)
			}
		})
	}

	t.Run("clamps at the oldest", func(t *testing.T) {
		m := mouseModel(80, 24, 40)
		for range 100 {
			m, _ = updated(t, m, wheel(true, rx, ry))
		}
		if first, last := visibleMessages(t, m); m.scroll != 21 || first != 1 || last != 19 {
			t.Errorf("scroll = %d, shows %d..%d, want 21, 1..19", m.scroll, first, last)
		}
		for range 100 {
			m, _ = updated(t, m, wheel(false, rx, ry))
		}
		if first, last := visibleMessages(t, m); m.scroll != 0 || first != 22 || last != 40 {
			t.Errorf("scroll = %d, shows %d..%d, want 0, 22..40", m.scroll, first, last)
		}
	})

	t.Run("fewer messages than rows", func(t *testing.T) {
		m := mouseModel(80, 24, 5)
		m, _ = updated(t, m, wheel(true, rx, ry))
		if first, last := visibleMessages(t, m); m.scroll != 0 || first != 1 || last != 5 {
			t.Errorf("scroll = %d, shows %d..%d, want 0, 1..5", m.scroll, first, last)
		}
	})

	t.Run("narrow terminal", func(t *testing.T) {
		m := mouseModel(50, 24, 40)
		m, _ = updated(t, m, wheel(true, 30, ry))
		if m.scroll != 1 {
			t.Errorf("scroll = %d, want 1", m.scroll)
		}
		if m2, _ := updated(t, m, wheel(true, 10, ry)); m2.scroll != 1 {
			t.Errorf("a wheel over the 14-column left pane scrolled the messages: %d", m2.scroll)
		}
	})
}

func TestPageKeysScrollMessages(t *testing.T) {
	pgup, pgdown := tea.KeyMsg{Type: tea.KeyPgUp}, tea.KeyMsg{Type: tea.KeyPgDown}
	// A page is the 19 lines shown, less one for context.
	tests := []struct {
		name   string
		keys   []tea.KeyMsg
		scroll int
	}{
		{"pgup", []tea.KeyMsg{pgup}, 18},
		{"pgup twice stops at the oldest", []tea.KeyMsg{pgup, pgup}, 21},
		{"pgup, pgdown", []tea.KeyMsg{pgup, pgup, pgdown}, 3},
		{"back to the newest", []tea.KeyMsg{pgup, pgdown, pgdown}, 0},
		{"pgdown at the newest", []tea.KeyMsg{pgdown}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mouseModel(80, 24, 40)
			for _, k := range tt.keys {
				var cmd tea.Cmd
				m, cmd = updated(t, m, k)
				if cmd != nil {
					t.Fatal("a page key returned a command")
				}
			}
			if m.scroll != tt.scroll {
				t.Errorf("scroll = %d, want %d", m.scroll, tt.scroll)
			}
		})
	}
}

// PgUp and PgDn keep scrolling the approval's body in the inbox and the
// decision prompt (issue #205), and leave the message pane alone.
func TestPageKeysStillScrollApprovalBody(t *testing.T) {
	for _, md := range []mode{modeInbox, modeDecision} {
		m := approvalModel(t, md, 80, 24, testApproval(numberedBody(100), 2))
		m.messages["general"] = []schema.MessageV2{{ID: 1, Body: "x"}}
		m, _ = updated(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
		if m.bodyOffset == 0 {
			t.Errorf("mode %d: pgdown did not scroll the body", md)
		}
		if m.scroll != 0 {
			t.Errorf("mode %d: pgdown scrolled the messages: %d", md, m.scroll)
		}
		offset := m.bodyOffset
		m, _ = updated(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
		if m.bodyOffset >= offset {
			t.Errorf("mode %d: pgup did not scroll the body back: %d", md, m.bodyOffset)
		}
	}
}

func TestMessagesArrivingWhileScrolled(t *testing.T) {
	arrive := func(id int64) tea.Msg {
		return messageReceived{channel: "general", message: schema.MessageV2{ID: id, AuthorID: 4, Body: fmt.Sprintf("message %02d", id)}}
	}
	t.Run("scrolled up the view stays put", func(t *testing.T) {
		m := mouseModel(80, 24, 40)
		for range 5 {
			m, _ = updated(t, m, wheel(true, 40, 5))
		}
		first, last := visibleMessages(t, m)
		m, _ = updated(t, m, arrive(41))
		m, _ = updated(t, m, arrive(42))
		if m.scroll != 7 {
			t.Errorf("scroll = %d, want 7", m.scroll)
		}
		if f, l := visibleMessages(t, m); f != first || l != last {
			t.Errorf("shows %d..%d, was %d..%d", f, l, first, last)
		}
	})
	t.Run("at the bottom it follows", func(t *testing.T) {
		m := mouseModel(80, 24, 40)
		m, _ = updated(t, m, arrive(41))
		if first, last := visibleMessages(t, m); m.scroll != 0 || first != 23 || last != 41 {
			t.Errorf("scroll = %d, shows %d..%d, want 0, 23..41", m.scroll, first, last)
		}
	})
	t.Run("a repeat of a message moves nothing", func(t *testing.T) {
		m := mouseModel(80, 24, 40)
		m, _ = updated(t, m, wheel(true, 40, 5))
		m, _ = updated(t, m, arrive(40))
		if m.scroll != 1 {
			t.Errorf("scroll = %d, want 1", m.scroll)
		}
	})
	t.Run("another channel's message", func(t *testing.T) {
		m := mouseModel(80, 24, 40)
		m, _ = updated(t, m, wheel(true, 40, 5))
		m, _ = updated(t, m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 900, Body: "elsewhere"}})
		if m.scroll != 1 {
			t.Errorf("scroll = %d, want 1", m.scroll)
		}
	})
	t.Run("switching channels goes back to the bottom", func(t *testing.T) {
		for name, switchTo := range map[string]tea.Msg{
			"click": click(2, 2),
			"key":   tea.KeyMsg{Type: tea.KeyDown},
		} {
			m := mouseModel(80, 24, 40)
			m, _ = updated(t, m, wheel(true, 40, 5))
			m, _ = updated(t, m, switchTo)
			if m.scroll != 0 {
				t.Errorf("%s: scroll = %d after switching channels, want 0", name, m.scroll)
			}
		}
	})
}

// approvalsModel is the inbox with n approvals titled A00, A01, ...
func approvalsModel(t *testing.T, md mode, width, n int) Model {
	t.Helper()
	m := approvalModel(t, md, width, 24, testApproval("body", 4))
	m.approvals = nil
	for i := range n {
		app := testApproval("body", 4)
		app.ID, app.Title = int64(i+1), fmt.Sprintf("A%02d", i)
		m.approvals = append(m.approvals, app)
	}
	return m
}

func TestMouseInbox(t *testing.T) {
	for _, width := range []int{80, 50} {
		leftW, _ := paneWidths(width)
		t.Run(fmt.Sprintf("%d/click", width), func(t *testing.T) {
			m := approvalsModel(t, modeInbox, width, 4)
			for i := range 4 {
				x, y := find(t, m, fmt.Sprintf("A%02d", i))
				got, cmd := updated(t, m, click(x, y))
				if got.selApproval != i || cmd != nil || got.mode != modeInbox {
					t.Errorf("click on A%02d: selected %d, command %v, mode %d", i, got.selApproval, cmd != nil, got.mode)
				}
			}
			for name, at := range map[string][2]int{"below the list": {2, 8}, "border": {0, 2}, "top border": {2, 0}, "details": {leftW + 3, 2}, "input": {2, 22}, "status": {2, 23}} {
				got, cmd := updated(t, m, click(at[0], at[1]))
				if got.selApproval != 0 || cmd != nil {
					t.Errorf("%s: selected %d, command %v", name, got.selApproval, cmd != nil)
				}
			}
		})
		t.Run(fmt.Sprintf("%d/long list", width), func(t *testing.T) {
			// 30 approvals do not fit: there are mark lines to miss.
			m := approvalsModel(t, modeInbox, width, 30)
			if got, _ := updated(t, m, click(2, 1)); got.selApproval != 0 {
				t.Errorf("a click on the mark line selected %d", got.selApproval)
			}
			for range 25 {
				m, _ = updated(t, m, wheel(false, 2, 5))
			}
			if m.selApproval != 25 {
				t.Fatalf("selected %d after 25 notches down, want 25", m.selApproval)
			}
			for _, i := range []int{m.selApproval - 3, m.selApproval - 1, m.selApproval} {
				x, y := find(t, m, fmt.Sprintf("A%02d", i))
				if got, _ := updated(t, m, click(x, y)); got.selApproval != i {
					t.Errorf("click on A%02d selected %d", i, got.selApproval)
				}
			}
			if got, _ := updated(t, m, click(2, 20)); got.selApproval != 25 {
				t.Errorf("a click on the lower mark line selected %d", got.selApproval)
			}
		})
	}

	wheels := []struct {
		name   string
		start  int
		events []tea.MouseMsg
		want   int
	}{
		{"down", 0, []tea.MouseMsg{wheel(false, 2, 3)}, 1},
		{"down to the last", 0, []tea.MouseMsg{wheel(false, 2, 3), wheel(false, 2, 3), wheel(false, 2, 3), wheel(false, 2, 3), wheel(false, 2, 3)}, 3},
		{"up at the first", 0, []tea.MouseMsg{wheel(true, 2, 3)}, 0},
		{"up", 2, []tea.MouseMsg{wheel(true, 2, 3)}, 1},
		{"on the left border", 1, []tea.MouseMsg{wheel(false, 0, 3)}, 2},
		{"over the details pane", 1, []tea.MouseMsg{wheel(false, 40, 3)}, 1},
	}
	for _, tt := range wheels {
		t.Run("wheel/"+tt.name, func(t *testing.T) {
			m := approvalsModel(t, modeInbox, 80, 4)
			m.selApproval, m.bodyOffset = tt.start, 3
			for _, e := range tt.events {
				var cmd tea.Cmd
				m, cmd = updated(t, m, e)
				if cmd != nil {
					t.Fatal("a wheel event returned a command")
				}
			}
			if m.selApproval != tt.want {
				t.Errorf("selected %d, want %d", m.selApproval, tt.want)
			}
			if m.selApproval != tt.start && m.bodyOffset != 0 {
				t.Errorf("body offset %d after the selection moved, want 0", m.bodyOffset)
			}
		})
	}

	t.Run("empty inbox", func(t *testing.T) {
		m := approvalsModel(t, modeInbox, 80, 0)
		for _, e := range []tea.MouseMsg{click(2, 1), wheel(false, 2, 1), wheel(true, 2, 1)} {
			if got, cmd := updated(t, m, e); got.selApproval != 0 || cmd != nil {
				t.Errorf("%v: selected %d, command %v", e, got.selApproval, cmd != nil)
			}
		}
	})
	t.Run("too small", func(t *testing.T) {
		m := approvalsModel(t, modeInbox, 30, 4)
		if got, _ := updated(t, m, click(2, 2)); got.selApproval != 0 {
			t.Errorf("selected %d on a terminal showing only the too-small notice", got.selApproval)
		}
	})
}

func TestMouseDecisionOptions(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {50, 24}, {80, 14}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := approvalModel(t, modeDecision, size[0], size[1], testApproval("body", 3))
			m.input = "because"
			for i := 2; i >= 0; i-- {
				x, y := find(t, m, fmt.Sprintf("Option %02d", i+1))
				got, cmd := updated(t, m, click(x, y))
				if got.selOption != i {
					t.Errorf("click on option %d selected %d", i+1, got.selOption)
				}
				if cmd != nil || got.mode != modeDecision || got.input != "because" || got.inboxStatus != m.inboxStatus {
					t.Errorf("click on option %d: command %v, mode %d, input %q, status %q: a click must only select", i+1, cmd != nil, got.mode, got.input, got.inboxStatus)
				}
			}
			x, y := find(t, m, "Decision Options:")
			if got, _ := updated(t, m, click(x, y)); got.selOption != 0 {
				t.Errorf("click on the Decision Options heading selected %d", got.selOption)
			}
			if got, _ := updated(t, m, click(1, y)); got.selOption != 0 {
				t.Errorf("click in the left pane selected option %d", got.selOption)
			}
		})
	}

	t.Run("windowed options", func(t *testing.T) {
		// More options than rows: the shown ones are around the selection,
		// between mark lines, and a click on a mark selects nothing.
		m := approvalModel(t, modeDecision, 80, 14, testApproval(numberedBody(30), 12))
		m.selOption = 6
		d, ok := m.selectedDetails()
		if !ok {
			t.Fatal("no details")
		}
		for line, opt := range d.optionRows {
			if !strings.Contains(d.lines[line], fmt.Sprintf("Option %02d", opt+1)) {
				t.Errorf("line %d (%q) is mapped to option %d", line, d.lines[line], opt+1)
			}
		}
		x, y := find(t, m, "Option 05")
		if got, _ := updated(t, m, click(x, y)); got.selOption != 4 {
			t.Errorf("selected %d, want 4", got.selOption)
		}
		x, y = find(t, m, "more options")
		if got, _ := updated(t, m, click(x, y)); got.selOption != 6 {
			t.Errorf("a click on a mark line selected %d", got.selOption)
		}
	})
}

// No mouse event, anywhere on the screen, casts a decision, sends or quits:
// in the inbox and the decision prompt it returns no command at all, and in
// the channels view only the loads of a selected channel.
func TestMouseNeverActs(t *testing.T) {
	buttons := []tea.MouseButton{tea.MouseButtonLeft, tea.MouseButtonRight, tea.MouseButtonMiddle, tea.MouseButtonWheelUp, tea.MouseButtonWheelDown, tea.MouseButtonNone}
	actions := []tea.MouseAction{tea.MouseActionPress, tea.MouseActionRelease, tea.MouseActionMotion}
	for _, md := range []mode{modeInbox, modeDecision} {
		m := approvalsModel(t, md, 80, 4)
		m.input, m.authorID = "a reason", 7
		for y := -1; y < 26; y++ {
			for x := -1; x < 82; x++ {
				for _, b := range buttons {
					for _, a := range actions {
						got, cmd := updated(t, m, tea.MouseMsg{X: x, Y: y, Button: b, Action: a})
						if cmd != nil || got.mode != md || got.input != "a reason" || got.sending {
							t.Fatalf("mode %d, %v %v at (%d,%d): command %v, mode %d, input %q", md, b, a, x, y, cmd != nil, got.mode, got.input)
						}
					}
				}
			}
		}
	}

	m := mouseModel(80, 24, 40)
	m.input, m.authorID = "hello", 7
	for y := -1; y < 26; y++ {
		for x := -1; x < 82; x++ {
			for _, b := range buttons {
				got, cmd := updated(t, m, tea.MouseMsg{X: x, Y: y, Button: b, Action: tea.MouseActionPress})
				if got.sending || got.input != "hello" || got.mode != modeChannels {
					t.Fatalf("%v at (%d,%d): sending %v, input %q", b, x, y, got.sending, got.input)
				}
				if onChannelRow := b == tea.MouseButtonLeft && x >= 1 && x <= 16 && y >= 2 && y <= 4; cmd != nil && !onChannelRow {
					t.Fatalf("%v at (%d,%d) returned a command", b, x, y)
				}
			}
		}
	}
}

// The voice line is under the panes, not in the left pane; a click on it does
// nothing, and its row takes one from the panes without moving a hit.
func TestMouseVoiceRoster(t *testing.T) {
	m := mouseModel(80, 24, 0)
	m.voice.state = voiceReconnecting
	l := m.layout()
	if l.voice == "" || l.contentH != 19 {
		t.Fatalf("voice line %q, content rows %d: the voice line should take a row from the panes", l.voice, l.contentH)
	}
	x, y := find(t, m, "voice: reconnecting")
	for _, at := range [][2]int{{x, y}, {x + 3, y}, {2, y}, {79, y}} {
		got, cmd := updated(t, m, click(at[0], at[1]))
		if got.selected != 0 || cmd != nil {
			t.Errorf("click on the voice line at %v: selected %d, command %v", at, got.selected, cmd != nil)
		}
	}
	x, y = find(t, m, "ops")
	if got, _ := updated(t, m, click(x, y)); got.current() != "ops" {
		t.Errorf("with the voice line shown, clicking ops selected %q", got.current())
	}
	m.messages["general"] = nil
	for i := 1; i <= 40; i++ {
		m.messages["general"] = append(m.messages["general"], schema.MessageV2{ID: int64(i), Body: fmt.Sprintf("message %02d", i)})
	}
	m, _ = updated(t, m, wheel(true, 40, 5))
	if first, last := visibleMessages(t, m); first != 22 || last != 39 {
		t.Errorf("shows %d..%d, want 22..39 (18 lines, one scrolled)", first, last)
	}
}

func TestProgramOptionsMouse(t *testing.T) {
	ctx := context.Background()
	off := programOptions(ctx, strings.NewReader(""), &strings.Builder{}, false)
	on := programOptions(ctx, strings.NewReader(""), &strings.Builder{}, true)
	if len(on) != len(off)+1 {
		t.Errorf("%d options with the mouse on and %d with it off, want exactly one more", len(on), len(off))
	}
}
