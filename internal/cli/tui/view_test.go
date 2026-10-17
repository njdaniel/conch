package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #538: the layout at the documented sizes, the help, the focus, the
// history and the people pane. These tests drive Model.Update and View with no
// terminal and no socket.

// hostileBody and the other bodies are the presentation fixtures: control
// sequences, bidi overrides, wide characters, line breaks, a long address.
const (
	hostileBody = "\x1b[31mred\x1b[0m \u202eevil\u202c, a bell \a, \x1b]0;title\a and [net:alpha] a fake badge"
	wideBody    = "日本語のテキストは一文字がセル二つ分なので、バイト数でも文字数でもなくセルの幅で折り返します。"
	linesBody   = "notes:\n1. ship it behind a flag\n[whisper:2] 2. not a badge\n\n3. done"
	longBody    = "a long message that has to wrap: https://ci.example.com/jobs/4127/artifacts/a/very/long/path/that/never/breaks/at/a/space/report.html and some words after it to wrap again"
)

// richModel is the chat view of general with every kind of message, a live
// roster of two people (one with two sessions) and an agent, an offline
// member, and a voice room in which bob is talking.
func richModel(t *testing.T) Model {
	t.Helper()
	m := NewModel(context.Background(), stubAPI{}, 7, []string{"general", "ops", "movie-night"})
	m.rs.off = true
	m.userName = "alice"
	m.loc = time.UTC
	m.now = func() time.Time { return time.Date(2026, time.July, 13, 15, 30, 0, 0, time.UTC) }
	m, _ = update(m, membersLoaded{channel: "general", members: []schema.ChannelMemberV1{
		{PrincipalID: 7, Name: "alice", Kind: schema.PrincipalHuman},
		{PrincipalID: 8, Name: "bob", Kind: schema.PrincipalHuman},
		{PrincipalID: 9, Name: "rex", Kind: schema.PrincipalAgent},
		{PrincipalID: 10, Name: "carol-with-a-rather-long-display-name", Kind: schema.PrincipalHuman},
		{PrincipalID: 11, Name: "日本語の名前", Kind: schema.PrincipalHuman},
	}})
	var messages []schema.MessageV2
	for i := range 12 {
		messages = append(messages, schema.MessageV2{ID: int64(i + 1), AuthorID: 8, Body: fmt.Sprintf("earlier %02d", i), CreatedAt: at(13, 14, i)})
	}
	bodies := []string{"hello", longBody, linesBody, wideBody, hostileBody, "short again"}
	authors := []int64{7, 9, 8, 11, 8, 10}
	for i, body := range bodies {
		messages = append(messages, schema.MessageV2{ID: int64(i + 20), AuthorID: authors[i], Body: body, CreatedAt: at(13, 15, i)})
	}
	m, _ = update(m, messagesLoaded{channel: "general", messages: messages})
	m.subscribed["general"] = true
	running := sess(3, 9, schema.PrincipalAgent, "rex", schema.SessionStatusRunning)
	running.Task = "fix the flaky test"
	m.ev.gen = 1
	m, _ = update(m, eventsRosterReceived{gen: 1, roster: rosterOf(
		sess(1, 7, schema.PrincipalHuman, "alice", schema.SessionStatusIdle),
		sess(2, 8, schema.PrincipalHuman, "bob", schema.SessionStatusIdle),
		sess(4, 8, schema.PrincipalHuman, "bob", schema.SessionStatusIdle),
		running,
	)})
	m.voice.state = voiceLive
	m.voice.doc = schema.VoicePresenceV1{Configured: true, Available: true, Rooms: []schema.VoicePresenceRoom{
		{Participants: []schema.VoiceParticipant{person(8, true), person(11, false)}},
	}}
	m.width, m.height = 120, 36
	return m
}

// keyMsg is the key a terminal sends for name.
func keyMsg(name string) tea.KeyMsg {
	types := map[string]tea.KeyType{
		"enter": tea.KeyEnter, "esc": tea.KeyEsc, "tab": tea.KeyTab, "shift+tab": tea.KeyShiftTab,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
		"f1": tea.KeyF1, "ctrl+c": tea.KeyCtrlC, "backspace": tea.KeyBackspace,
	}
	if k, ok := types[name]; ok {
		return tea.KeyMsg{Type: k}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

func keys(t *testing.T, m Model, names ...string) (Model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, name := range names {
		m, cmd = updated(t, m, keyMsg(name))
	}
	return m, cmd
}

// checkScreen fails unless view fills width x height exactly: no line wider
// than the terminal (measured in cells), and no more or fewer lines.
func checkScreen(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != height {
		t.Fatalf("%dx%d: view has %d lines:\n%s", width, height, len(lines), view)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("%dx%d: line %d is %d cells: %q", width, height, i, w, line)
		}
	}
}

// The documented breakpoints (docs/design/tui-polish.md): wide from 120x30
// with the people pane, a title bar from 80x24, compact below it, and the
// too-small screen below 40x10. Every view fills the terminal exactly at every
// size, and the input line, its audience and the way out stay on screen.
func TestLayoutBreakpoints(t *testing.T) {
	tests := []struct {
		width, height int
		title, people bool
		tooSmall      bool
	}{
		{160, 48, true, true, false},
		{120, 36, true, true, false},
		{120, 30, true, true, false},
		{120, 29, true, false, false},
		{119, 36, true, false, false},
		{100, 30, true, false, false},
		{80, 24, true, false, false},
		{79, 24, false, false, false},
		{80, 23, false, false, false},
		{60, 18, false, false, false},
		{40, 10, false, false, false},
		{39, 10, false, false, true},
		{40, 9, false, false, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%dx%d", tt.width, tt.height), func(t *testing.T) {
			m := richModel(t)
			m, _ = updated(t, m, tea.WindowSizeMsg{Width: tt.width, Height: tt.height})
			view := m.View()
			checkScreen(t, view, tt.width, tt.height)
			lines := strings.Split(view, "\n")
			if tt.tooSmall {
				if !strings.Contains(view, "terminal too small") || !strings.Contains(view, "esc: quit") {
					t.Errorf("no too-small screen:\n%s", view)
				}
				return
			}
			if got := strings.HasPrefix(lines[0], "CONCH"); got != tt.title {
				t.Errorf("title bar = %v, want %v:\n%s", got, tt.title, view)
			}
			if got := strings.Contains(view, "PEOPLE"); got != tt.people {
				t.Errorf("people pane = %v, want %v:\n%s", got, tt.people, view)
			}
			// The members line is there exactly when the people pane is not.
			if got := strings.Contains(view, "online: "); got == tt.people {
				t.Errorf("members line = %v with the people pane %v", got, tt.people)
			}
			if !strings.Contains(view, "general > ") {
				t.Errorf("the input line lost its audience:\n%s", view)
			}
			if !strings.Contains(lines[len(lines)-1], "esc quit") {
				t.Errorf("the status line lost the way out: %q", lines[len(lines)-1])
			}
			// Every other view fills the same terminal exactly too.
			for _, md := range []mode{modeInbox, modeAgents, modeRuns} {
				other := m
				other.mode = md
				checkScreen(t, other.View(), tt.width, tt.height)
			}
			help := m
			help.help = true
			checkScreen(t, help.View(), tt.width, tt.height)
		})
	}
}

// Styling adds escape sequences and nothing else: the plain screen (NO_COLOR,
// a pipe, a monochrome terminal) has the same text, and what the colours say
// is in it as text: the selection, the focus, the view and the badges.
func TestPlainAndStyledShowTheSameText(t *testing.T) {
	profile := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(profile)
	// termenv.ANSI and termenv.Ascii, named through the type lipgloss returns:
	// the module has termenv only indirectly.
	ansi, ascii := sameType(profile, 2), sameType(profile, 3)
	for _, md := range []mode{modeChannels, modeInbox, modeAgents} {
		m := richModel(t)
		m.mode = md
		m.approvals = []schema.ApprovalV1{testApproval("body", 2)}
		lipgloss.SetColorProfile(ansi)
		styled := m.View()
		lipgloss.SetColorProfile(ascii)
		plain := m.View()
		if !strings.Contains(styled, "\x1b[") {
			t.Fatalf("mode %d: the styled screen has no styling", md)
		}
		if strings.Contains(plain, "\x1b") {
			t.Fatalf("mode %d: the plain screen has escape sequences", md)
		}
		if stripANSI(styled) != plain {
			t.Errorf("mode %d: styling changed the text:\nstyled:\n%s\nplain:\n%s", md, stripANSI(styled), plain)
		}
	}
	m := richModel(t)
	lipgloss.SetColorProfile(ascii)
	plain := m.View()
	for _, mark := range []string{"› general", "▸ CHANNELS", "[Chat]", "[agent]", "● bob", "talking", "● text live"} {
		if !strings.Contains(plain, mark) {
			t.Errorf("the plain screen lacks %q:\n%s", mark, plain)
		}
	}
}

// sameType is v as a value of the type of like.
func sameType[P ~int](_ P, v int) P { return P(v) }

func TestWrapWords(t *testing.T) {
	tests := []struct {
		name        string
		s           string
		first, rest int
		want        []string
	}{
		{"fits", "hello world", 20, 20, []string{"hello world"}},
		{"at a space", "hello wide world", 11, 11, []string{"hello wide", "world"}},
		{"first line narrower", "one two three four", 4, 10, []string{"one", "two three", "four"}},
		{"a word longer than a line", "abcdefghij kl", 4, 4, []string{"abcd", "efgh", "ij", "kl"}},
		{"wide characters by cells", "日本語です", 4, 4, []string{"日本", "語で", "す"}},
		{"a wide character never split", "日本", 3, 3, []string{"日", "本"}},
		{"too narrow for a wide character", "日本", 1, 1, []string{"日", "本"}},
		{"empty", "", 5, 5, []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapWords(tt.s, tt.first, tt.rest)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("wrapWords(%q, %d, %d) = %q, want %q", tt.s, tt.first, tt.rest, got, tt.want)
			}
		})
	}
}

func TestClipByCells(t *testing.T) {
	tests := []struct {
		name, s    string
		width      int
		clip, tail string
	}{
		{"ascii fits", "hello", 5, "hello", "hello"},
		{"ascii cut", "hello world", 6, "hello…", "…world"},
		{"wide cut", "日本語のテキスト", 7, "日本語…", "…キスト"},
		{"one cell", "日本", 1, "…", "…"},
		{"none", "abc", 0, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clip(tt.s, tt.width); got != tt.clip || cellWidth(got) > max(tt.width, 0) {
				t.Errorf("clip = %q, want %q", got, tt.clip)
			}
			if got := clipTail(tt.s, tt.width); got != tt.tail || cellWidth(got) > max(tt.width, 0) {
				t.Errorf("clipTail = %q, want %q", got, tt.tail)
			}
		})
	}
}

// A message takes the lines it needs: none is wider than the pane, the first
// starts with its time and author, and every other starts with spaces and the
// continuation mark, so no line of a body can pass for another message or a
// badge, whatever it holds.
func TestMessageBlock(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		width int // the pane's outer width
		lines int // 0: more than one
		want  []string
	}{
		{"short is one line, as messageLine draws it", "hi there", 60, 1, []string{"15:00 alice: hi there"}},
		{"long wraps at spaces", longBody, 60, 0, nil},
		{"line breaks are kept", linesBody, 60, 5, []string{"15:00 alice: notes:", "      ┊ 1. ship it behind a flag", "      ┊ [whisper:2] 2. not a badge", "      ┊ ", "      ┊ 3. done"}},
		{"wide characters by cells", wideBody, 40, 0, nil},
		{"hostile text is made safe", hostileBody, 60, 0, nil},
		{"a body opening with a badge is escaped", "[net:alpha] hi", 60, 1, []string{`15:00 alice: \[net:alpha] hi`}},
		{"trailing line breaks are dropped", "hi\n\n", 60, -1, []string{"15:00 alice: hi"}},
		{"empty", "", 60, 1, []string{"15:00 alice: "}},
		{"a pane too narrow for the head", "the body goes below", 12, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := authorModel(stubAPI{}, schema.ChannelMemberV1{PrincipalID: 7, Name: "alice", Kind: schema.PrincipalHuman})
			m.loc = time.UTC
			message := schema.MessageV2{ID: 1, AuthorID: 7, Body: tt.body, CreatedAt: at(13, 15, 0)}
			lines := m.messageBlock("ops", message, tt.width)
			switch {
			case tt.lines < 0 && len(lines) != 1, tt.lines == 1 && len(lines) != 1, tt.lines > 1 && len(lines) != tt.lines, tt.lines == 0 && len(lines) < 2:
				t.Fatalf("%d lines: %q", len(lines), lines)
			}
			for i, w := range tt.want {
				if got := stripANSI(lines[i]); got != w {
					t.Errorf("line %d = %q, want %q", i, got, w)
				}
			}
			if tt.lines == 1 {
				if got, want := lines[0], m.messageLine("ops", message, tt.width); got != want {
					t.Errorf("one-line block %q differs from messageLine %q", got, want)
				}
			}
			joined := strings.Join(lines, "")
			if strings.ContainsAny(stripANSI(joined), "\x1b\a\r\n\u202e\u202c") {
				t.Errorf("a control character got through: %q", lines)
			}
			for i, line := range lines {
				plain := stripANSI(line)
				if w := cellWidth(plain); w > tt.width-2 {
					t.Errorf("line %d is %d cells, the pane holds %d: %q", i, w, tt.width-2, plain)
				}
				if i > 0 && (!strings.HasPrefix(plain, "  ") || !strings.HasPrefix(strings.TrimLeft(plain, " "), strings.TrimSpace(contMark))) {
					t.Errorf("continuation line %d does not start with spaces and the mark: %q", i, plain)
				}
				if i == 0 && strings.HasPrefix(plain, " ") {
					t.Errorf("the first line starts with a space, as a continuation does: %q", plain)
				}
			}
		})
	}
}

// The chat view's two panes: ←/→ move the focus, and ↑/↓ move what has it.
// Typing always goes to the input line; a resize keeps the focus, the draft
// and the channel.
func TestChatFocus(t *testing.T) {
	m := richModel(t)
	m.height = 14 // the history is taller than the pane
	if m.focus != focusChannels || !strings.Contains(m.View(), "▸ CHANNELS") {
		t.Fatalf("the channel list does not start with the focus:\n%s", m.View())
	}
	m, cmd := keys(t, m, "down")
	if m.current() != "ops" || cmd == nil {
		t.Fatalf("down with the list focused opened %q", m.current())
	}
	m, _ = keys(t, m, "up", "right")
	if m.focus != focusMessages || !strings.Contains(m.View(), "▸ # general") {
		t.Fatalf("right did not focus the messages:\n%s", m.View())
	}
	m, _ = keys(t, m, "up", "up", "up")
	if m.current() != "general" || m.scroll != 3 {
		t.Errorf("up with the messages focused: channel %q, scroll %d; want general, 3", m.current(), m.scroll)
	}
	m, _ = keys(t, m, "down")
	if m.scroll != 2 {
		t.Errorf("down scrolled to %d, want 2", m.scroll)
	}
	m, _ = keys(t, m, "h", "i")
	if m.input != "hi" {
		t.Errorf("typing with the messages focused: input %q", m.input)
	}
	m, _ = updated(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = updated(t, m, tea.WindowSizeMsg{Width: 160, Height: 48})
	if m.focus != focusMessages || m.input != "hi" || m.current() != "general" {
		t.Errorf("a resize lost state: focus %v, input %q, channel %q", m.focus, m.input, m.current())
	}
	m, _ = keys(t, m, "left")
	if m.focus != focusChannels {
		t.Errorf("left did not give the focus back to the channels")
	}
	for _, md := range []mode{modeInbox, modeAgents, modeDecision} {
		other := m
		other.mode = md
		if got, _ := keys(t, other, "right"); got.focus != focusChannels {
			t.Errorf("mode %d: right moved the chat focus", md)
		}
	}
}

// The help: F1 everywhere, "?" where nothing is typed, /help in the chat
// view. While it is open no key reaches the view: typing, enter and tab do
// nothing, and esc closes it rather than quitting.
func TestHelpOverlay(t *testing.T) {
	m := richModel(t)
	m.input = "draft"
	if !strings.Contains(m.View(), "general > draft▌") {
		t.Fatalf("no cursor where typing goes:\n%s", m.View())
	}
	m, _ = keys(t, m, "f1")
	if v := m.View(); strings.Contains(v, "▌") || !strings.Contains(v, "general > draft") {
		t.Errorf("with the help open the draft should stay, without the cursor:\n%s", v)
	}
	if !m.help || !strings.Contains(m.View(), "HELP · Chat") || !strings.Contains(m.View(), "esc close help") {
		t.Fatalf("F1 did not open the help:\n%s", m.View())
	}
	for _, k := range []string{"x", "q", "?", "tab", "shift+tab", "up", "down", "left", "home", "pgup"} {
		got, cmd := keys(t, m, k)
		if k == "?" {
			if got.help {
				t.Errorf("? did not close the help")
			}
			continue
		}
		if !got.help || got.input != "draft" || got.mode != modeChannels || got.current() != "general" || got.scroll != 0 || cmd != nil {
			t.Errorf("%s with the help open: help %v, input %q, mode %d, channel %q, command %v", k, got.help, got.input, got.mode, got.current(), cmd != nil)
		}
	}
	got, cmd := keys(t, m, "enter")
	if got.help || got.sending || cmd != nil || got.input != "draft" {
		t.Errorf("enter with the help open: help %v, sending %v, command %v", got.help, got.sending, cmd != nil)
	}
	got, cmd = keys(t, m, "esc")
	if got.help || cmd != nil {
		t.Errorf("esc with the help open: help %v, command %v; want it closed, not quit", got.help, cmd != nil)
	}
	if got, cmd := keys(t, m, "ctrl+c"); cmd == nil || got.help {
		t.Errorf("ctrl+c with the help open did not quit")
	}
	// In the decision prompt ctrl+c cancels the decision, help or no help;
	// it does not quit and throw the reason away with the program.
	decide := approvalModel(t, modeDecision, 80, 24, testApproval("body", 2))
	decide.input = "a reason"
	decide, _ = keys(t, decide, "f1")
	if got, cmd := keys(t, decide, "ctrl+c"); got.help || got.mode != modeInbox || cmd != nil {
		t.Errorf("ctrl+c with the help open in the decision prompt: help %v, mode %d, command %v; want the decision cancelled", got.help, got.mode, cmd != nil)
	}
	for _, e := range []tea.MouseMsg{click(2, 3), wheel(true, 40, 5)} {
		if got, cmd := updated(t, m, e); !got.help || cmd != nil || got.current() != "general" || got.scroll != 0 {
			t.Errorf("a mouse event reached the view under the help: %+v", e)
		}
	}

	// "?" is typed in the chat view and the decision prompt, and opens the
	// help where nothing is typed.
	chat, _ := keys(t, richModel(t), "?")
	if chat.help || chat.input != "?" {
		t.Errorf("? in the chat view: help %v, input %q; want it typed", chat.help, chat.input)
	}
	for _, md := range []mode{modeInbox, modeAgents, modeRuns} {
		other := richModel(t)
		other.mode = md
		if got, _ := keys(t, other, "?"); !got.help || !strings.Contains(got.View(), "HELP · ") {
			t.Errorf("mode %d: ? did not open the help", md)
		}
	}
	decision := approvalModel(t, modeDecision, 80, 24, testApproval("body", 2))
	if got, _ := keys(t, decision, "?"); got.help || got.input != "?" {
		t.Errorf("? in the decision prompt: help %v, input %q; want it typed", got.help, got.input)
	}

	// On a small terminal the help scrolls, and says how much is below.
	small := richModel(t)
	small.width, small.height = 60, 18
	small, _ = keys(t, small, "f1")
	if v := stripANSI(small.View()); !strings.Contains(v, "more · ↑/↓ scroll") || strings.Contains(v, "talk key") {
		t.Errorf("a help longer than the pane does not say so:\n%s", v)
	}
	small, _ = keys(t, small, "down", "pgdown", "pgdown", "pgdown", "pgdown")
	if v := stripANSI(small.View()); !strings.Contains(v, "talk key") || strings.Contains(v, "more ·") || small.mode != modeChannels {
		t.Errorf("the end of the help is not reached:\n%s", v)
	}
	checkScreen(t, small.View(), 60, 18)
	small, _ = keys(t, small, "esc", "f1")
	if small.helpTop != 0 {
		t.Errorf("the help opened again at line %d", small.helpTop)
	}

	// /help opens it and leaves nothing in the input; nothing is sent.
	slash := richModel(t)
	slash.input = "/help"
	got, cmd = keys(t, slash, "enter")
	if !got.help || got.input != "" || got.sending || cmd != nil {
		t.Errorf("/help: help %v, input %q, sending %v", got.help, got.input, got.sending)
	}
}

// Each view's help lists its own keys.
func TestHelpListsTheKeysOfTheView(t *testing.T) {
	tests := []struct {
		mode mode
		want []string
	}{
		{modeChannels, []string{"/net NAME", "/w IDS TEXT", "←/→", "home/end", "shift+tab", "Voice is shown, not joined"}},
		{modeInbox, []string{"enter", "decide it"}},
		{modeDecision, []string{"the reason, which is required", "cancel and go back to the inbox"}},
		{modeAgents, []string{"unfold an agent's sessions", "r"}},
		{modeRuns, []string{"show all, open or ended runs"}},
		{modeRun, []string{"go to the message the run answers", "only what the session supports"}},
	}
	for _, tt := range tests {
		m := richModel(t)
		m.width, m.height = 160, 48
		m.mode, m.help = tt.mode, true
		m.approvals = []schema.ApprovalV1{testApproval("body", 2)}
		view := m.View()
		for _, w := range tt.want {
			if !strings.Contains(view, w) {
				t.Errorf("mode %d: the help lacks %q:\n%s", tt.mode, w, view)
			}
		}
	}
}

// Where a plain message goes is named twice, in the message pane's title and
// in the prompt, at every size; moving the focus does not change it.
func TestAudienceStaysVisible(t *testing.T) {
	m := richModel(t)
	m.target = &netTarget{channel: "general", id: 3, name: "alpha"}
	for _, size := range [][2]int{{160, 48}, {80, 24}, {60, 18}, {40, 10}} {
		m.width, m.height = size[0], size[1]
		for _, focus := range []string{"right", "left"} {
			got, cmd := keys(t, m, focus)
			view := stripANSI(got.View())
			if !strings.Contains(view, "general/alpha >") || cmd != nil || got.target == nil {
				t.Errorf("%dx%d after %s: the prompt lost the net:\n%s", size[0], size[1], focus, view)
			}
			if size[0] >= 60 && !strings.Contains(view, "› net alpha") {
				t.Errorf("%dx%d: the message pane's title lost the net:\n%s", size[0], size[1], view)
			}
		}
	}
}

func TestShiftTabGoesBack(t *testing.T) {
	m := richModel(t)
	steps := []mode{modeAgents, modeInbox, modeChannels}
	for _, want := range steps {
		m, _ = keys(t, m, "shift+tab")
		if m.mode != want {
			t.Fatalf("shift+tab went to mode %d, want %d", m.mode, want)
		}
	}
	m.mode = modeRuns
	if m, _ = keys(t, m, "shift+tab"); m.mode != modeAgents {
		t.Errorf("shift+tab from Runs went to mode %d, want Agents", m.mode)
	}
	m.mode = modeDecision
	if m, _ = keys(t, m, "shift+tab"); m.mode != modeDecision {
		t.Errorf("shift+tab left a decision in progress")
	}
}

// A reader scrolled up stays where they are when messages arrive, however
// many lines those take, and is shown how many are below and how to get back.
// A resize keeps the same message at the bottom of the pane.
func TestHistoryAnchoring(t *testing.T) {
	m := richModel(t)
	m.height = 14
	m, _ = keys(t, m, "home")
	top := m.scroll
	first, _, _ := m.historyView(m.layout().rightW, m.layout().messageRows())
	if top == 0 || !strings.Contains(stripANSI(first[0]), "earlier 00") {
		t.Fatalf("home did not show the oldest message: scroll %d, %q", top, first)
	}
	if foot := stripANSI(m.View()); !strings.Contains(foot, "newer · end: latest") {
		t.Errorf("no way back to the newest while scrolled up:\n%s", foot)
	}
	arrive := schema.MessageV2{ID: 50, AuthorID: 8, Body: linesBody, CreatedAt: at(13, 15, 20)}
	m, _ = updated(t, m, messageReceived{channel: "general", message: arrive})
	after, _, _ := m.historyView(m.layout().rightW, m.layout().messageRows())
	if strings.Join(after, "\n") != strings.Join(first, "\n") {
		t.Errorf("a five-line arrival moved the lines on screen:\nbefore %q\nafter %q", first, after)
	}
	if want := top + len(m.messageBlock("general", arrive, m.layout().rightW)); m.scroll != want {
		t.Errorf("scroll = %d, want %d", m.scroll, want)
	}

	// A resize: the message at the bottom of the pane stays at the bottom.
	bottomMessage := func(m Model) int {
		message, _, ok := m.scrollAnchor()
		if !ok {
			t.Fatal("not scrolled")
		}
		return message
	}
	m, _ = keys(t, m, "end", "pgup")
	want := bottomMessage(m)
	for _, size := range [][2]int{{80, 24}, {60, 18}, {160, 48}, {120, 36}} {
		m, _ = updated(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if m.scroll > 0 && bottomMessage(m) != want {
			t.Errorf("%dx%d: message %d at the bottom, want %d", size[0], size[1], bottomMessage(m), want)
		}
	}

	// End goes back to the newest; the mark goes with it.
	m, _ = keys(t, m, "end")
	if m.scroll != 0 || strings.Contains(m.View(), "end: latest") {
		t.Errorf("end left scroll %d", m.scroll)
	}
}

// longHistory is the chat view at 160x48 with n messages of about 120 cells,
// which wrap in the chat pane's width of the wide layout and not in the wider
// main pane the other views have.
func longHistory(t *testing.T, n int) Model {
	t.Helper()
	m := mouseModel(160, 48, 0)
	for i := 1; i <= n; i++ {
		m.messages["general"] = append(m.messages["general"], schema.MessageV2{ID: int64(i), AuthorID: 4,
			Body: fmt.Sprintf("message %02d %s", i, strings.Repeat("word ", 21+i%7))})
	}
	return m
}

// The history is laid out in the chat pane's width whichever view is on
// screen: messages that arrive, and a resize, while the user is in another
// view keep what the scrolled-up chat pane shows.
func TestScrollHeldInAnotherView(t *testing.T) {
	visible := func(m Model) string {
		c := m
		c.mode = modeChannels
		lines, _, _ := c.historyView(c.layout().rightW, c.layout().messageRows())
		return stripANSI(strings.Join(lines, "\n"))
	}
	t.Run("arrivals", func(t *testing.T) {
		m := longHistory(t, 60)
		m, _ = keys(t, m, "pgup")
		before := visible(m)
		m, _ = keys(t, m, "shift+tab")
		if m.mode != modeAgents {
			t.Fatalf("mode %d", m.mode)
		}
		for id := int64(61); id <= 62; id++ {
			m, _ = updated(t, m, messageReceived{channel: "general", message: schema.MessageV2{ID: id, AuthorID: 4, Body: strings.Repeat("late ", 24)}})
		}
		m, _ = keys(t, m, "tab")
		if after := visible(m); after != before {
			t.Errorf("arrivals in another view moved the history:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("resize", func(t *testing.T) {
		m := longHistory(t, 60)
		m, _ = keys(t, m, "pgup", "pgup")
		want, _, _ := m.scrollAnchor()
		m, _ = keys(t, m, "shift+tab")
		m, _ = updated(t, m, tea.WindowSizeMsg{Width: 150, Height: 48})
		m, _ = keys(t, m, "tab")
		if got, _, ok := m.scrollAnchor(); !ok || got != want {
			t.Errorf("a resize in another view moved the anchor from message %d to %d", want, got)
		}
	})
}

// What is drawn is what is hit, after resizes across every breakpoint and
// after scrolling: a click on a channel's name opens it, a click in the
// message pane focuses it, the wheel scrolls only the message pane, and the
// mark in its bottom border goes back to the newest.
func TestMouseAfterResizeAndScroll(t *testing.T) {
	m := richModel(t)
	clicked, checked := 0, 0
	for _, size := range [][2]int{{160, 48}, {120, 36}, {80, 24}, {60, 18}, {40, 10}, {120, 30}} {
		m, _ = updated(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, name := range []string{"ops", "movie-night", "general"} {
			l := m.layout()
			x, y := -1, -1
			for row, line := range strings.Split(stripANSI(m.View()), "\n") {
				if row <= l.top || row > l.top+l.contentH {
					continue
				}
				if i := strings.Index(line, "  "+name); i >= 0 && cellWidth(line[:i]) < l.leftW {
					x, y = cellWidth(line[:i])+2, row
					break
				}
			}
			if x < 0 {
				continue // the selected one is drawn with its mark
			}
			clicked++
			got, _ := updated(t, m, click(x, y))
			if got.current() != name || got.focus != focusChannels {
				t.Errorf("%dx%d: clicking %q at (%d,%d) opened %q", size[0], size[1], name, x, y, got.current())
			}
		}
		l := m.layout()
		got, cmd := updated(t, m, click(l.leftW+3, l.top+2))
		if got.focus != focusMessages || cmd != nil {
			t.Errorf("%dx%d: a click in the message pane: focus %v, command %v", size[0], size[1], got.focus, cmd != nil)
		}
		if l.peopleW > 0 {
			checked++
			if got, _ := updated(t, m, wheel(true, l.leftW+l.rightW+3, l.top+2)); got.scroll != 0 {
				t.Errorf("%dx%d: the wheel over the people pane scrolled the messages", size[0], size[1])
			}
		}
	}
	// At 40x10 the list is 14 cells wide and "movie-night" is cut, so it is not found.
	if clicked < 11 || checked < 3 {
		t.Fatalf("clicked %d channels and checked %d people panes; the test lost its targets", clicked, checked)
	}
	m, _ = updated(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	l := m.layout()
	m, _ = updated(t, m, wheel(true, l.leftW+5, l.top+3))
	m, _ = updated(t, m, wheel(true, l.leftW+5, l.top+3))
	if m.scroll != 2 {
		t.Fatalf("two notches up scrolled %d", m.scroll)
	}
	view := stripANSI(m.View())
	row := strings.Split(view, "\n")[l.top+l.contentH+1]
	i := strings.Index(row, "end: latest")
	if i < 0 {
		t.Fatalf("no mark in the bottom border: %q", row)
	}
	m, _ = updated(t, m, click(cellWidth(row[:i]), l.top+l.contentH+1))
	if m.scroll != 0 {
		t.Errorf("a click on the mark left scroll %d", m.scroll)
	}
}

// The wide layout gives the approval list more room; the details beside it,
// and the clicks on both, follow it.
func TestApprovalsInTheWideLayout(t *testing.T) {
	m := approvalsModel(t, modeInbox, 160, 4)
	m.height = 48
	l := m.layout()
	if l.leftW != inboxWideW || l.leftW+l.rightW != 159 {
		t.Fatalf("widths %d + %d", l.leftW, l.rightW)
	}
	checkScreen(t, m.View(), 160, 48)
	x, y := find(t, m, "A02")
	if got, _ := updated(t, m, click(x, y)); got.selApproval != 2 {
		t.Errorf("clicking A02 selected %d", got.selApproval)
	}
	m.mode, m.input = modeDecision, "because"
	checkScreen(t, m.View(), 160, 48)
	for i := 3; i >= 0; i-- {
		x, y := find(t, m, fmt.Sprintf("Option %02d", i+1))
		if got, cmd := updated(t, m, click(x, y)); got.selOption != i || cmd != nil {
			t.Errorf("clicking option %d selected %d", i+1, got.selOption)
		}
	}
}

// A channel list longer than its pane scrolls with the selection, and a
// click selects the channel drawn where it was clicked.
func TestLongChannelList(t *testing.T) {
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("room-%02d", i))
	}
	m := NewModel(context.Background(), stubAPI{}, 7, names)
	m.rs.off = true
	m.width, m.height = 80, 24
	for range 30 {
		m, _ = keys(t, m, "down")
	}
	if m.current() != "room-30" {
		t.Fatalf("selected %q", m.current())
	}
	view := m.View()
	checkScreen(t, view, 80, 24)
	if !strings.Contains(view, "room-30") || strings.Contains(view, "room-00") {
		t.Fatalf("the list does not follow the selection:\n%s", view)
	}
	x, y := find(t, m, "room-25")
	if got, _ := updated(t, m, click(x, y)); got.current() != "room-25" {
		t.Errorf("clicking room-25 opened %q", got.current())
	}
}

// The people pane: each principal once, how many sessions they have, a word
// beside every glyph, agents with their badge, status and task, and the voice
// room with who is talking.
func TestPeoplePane(t *testing.T) {
	m := richModel(t)
	l := m.layout()
	lines := m.peopleLines(l.peopleW-2, l.contentH)
	text := stripANSI(strings.Join(lines, "\n"))
	for _, want := range []string{"● alice (you)", "online", "● bob", "online ×2", "○ carol-with", "offline", "AGENTS", "● rex [agent] running", "task: fix the flaky test", "VOICE · general room", "● bob", "talking", "日本語の名前"} {
		if !strings.Contains(text, want) {
			t.Errorf("the people pane lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "bob") != 2 {
		t.Errorf("bob is listed %d times, want once as a person and once in voice:\n%s", strings.Count(text, "bob"), text)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > l.peopleW-2 {
			t.Errorf("line %d is %d cells: %q", i, w, line)
		}
	}
	if got := stripANSI(m.peopleTitle()); got != "PEOPLE · 2 online" {
		t.Errorf("title = %q", got)
	}
	// A roster that may be stale is not shown.
	m.ev.state = eventsDown
	if text := stripANSI(strings.Join(m.peopleLines(28, 20), "\n")); !strings.Contains(text, "people: unavailable") || strings.Contains(text, "online") {
		t.Errorf("a dropped roster is still shown:\n%s", text)
	}
	// A long roster ends in how much is not shown.
	big := richModel(t)
	members := map[int64]schema.ChannelMemberV1{}
	for i := int64(100); i < 160; i++ {
		members[i] = schema.ChannelMemberV1{PrincipalID: i, Name: fmt.Sprintf("member-%d", i), Kind: schema.PrincipalHuman}
	}
	big.members["general"] = members
	if got := big.peopleLines(28, 12); len(got) != 12 || !strings.Contains(stripANSI(got[11]), "more lines") {
		t.Errorf("a long roster: %d lines, last %q", len(got), got[len(got)-1])
	}
}

// The voice strip says only what this channel's presence shows of the person
// here, and then whose voice is in that channel's room: whether
// they are in this channel's room (from another client: this terminal never
// joins), and nothing while presence is down. It cannot see another channel's
// room, so it never says "not in voice" outright.
func TestVoiceStrip(t *testing.T) {
	tests := []struct {
		name   string
		people []schema.VoiceParticipant
		setup  func(*Model)
		want   string
	}{
		{"not in this room", []schema.VoiceParticipant{person(8, true)}, nil, "you: not in #general voice · voice: ●8 bob"},
		{"in this room from another client", []schema.VoiceParticipant{person(7, false), person(8, false)}, nil, "you: in #general voice from another client · voice: 7 alice  8 bob"},
		{"talking from another client", []schema.VoiceParticipant{person(7, true)}, nil, "you: in #general voice from another client, ● talking · voice: ●7 alice"},
		{"reconnecting: nothing about you", []schema.VoiceParticipant{person(7, true)}, func(m *Model) { m.voice.state = voiceReconnecting }, "#general voice: reconnecting"},
		{"LiveKit unavailable: nothing about you", nil, func(m *Model) { m.voice.doc = schema.VoicePresenceV1{Configured: true} }, "#general voice: unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := richModel(t)
			m.voice.doc.Rooms = []schema.VoicePresenceRoom{{Participants: tt.people}}
			if tt.setup != nil {
				tt.setup(&m)
			}
			got := m.voiceStrip(120)
			if got != tt.want {
				t.Errorf("strip = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "you: not in voice") {
				t.Errorf("the strip claims what it cannot know: %q", got)
			}
		})
	}
	m := richModel(t)
	m.voice.doc.Rooms = []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{person(11, true), person(8, false)}}}
	for _, width := range []int{80, 50, 30, 12} {
		if got := m.voiceStrip(width); cellWidth(got) > width {
			t.Errorf("width %d: strip is %d cells: %q", width, cellWidth(got), got)
		}
	}
	m.voice.state = voiceOff
	if got := m.voiceStrip(120); got != "" {
		t.Errorf("a server without voice has a strip: %q", got)
	}
}

func TestTitleBar(t *testing.T) {
	m := richModel(t)
	tests := []struct {
		name  string
		setup func(*Model)
		want  []string
	}{
		{"chat, live", func(*Model) {}, []string{"CONCH", "[Chat] Inbox Agents Runs", "alice", "● text live", "help: F1, /help"}},
		{"inbox", func(m *Model) { m.mode = modeInbox }, []string{"Chat [Inbox] Agents Runs", "help: F1, ?"}},
		{"decision", func(m *Model) { m.mode = modeDecision }, []string{"Chat [Inbox] Agents Runs", "help: F1"}},
		{"run detail", func(m *Model) { m.mode, m.rd.runID = modeRun, 41 }, []string{"Chat Inbox Agents Runs [run #41]", "help: F1, ?"}},
		{"help open", func(m *Model) { m.help = true }, []string{"help: esc closes"}},
		{"reconnecting", func(m *Model) { m.retryPending["general"] = true }, []string{"◐ text reconnecting"}},
		{"no channel list", func(m *Model) { m.channelsErr = fmt.Errorf("refused") }, []string{"○ text unavailable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := m
			m.retryPending = map[string]bool{}
			tt.setup(&m)
			bar := stripANSI(m.titleBar(120))
			for _, w := range tt.want {
				if !strings.Contains(bar, w) {
					t.Errorf("title bar %q lacks %q", bar, w)
				}
			}
			if cellWidth(bar) != 120 {
				t.Errorf("title bar is %d cells", cellWidth(bar))
			}
		})
	}
	// Narrow: the connection and the help stay.
	for _, width := range []int{80, 60, 40, 20} {
		bar := stripANSI(m.titleBar(width))
		if cellWidth(bar) > width || !strings.Contains(bar, "text") {
			t.Errorf("width %d: %q", width, bar)
		}
	}
}

// The run detail pane reached through g lands on a multi-line message as it
// does on one-line ones: centred, so it is on screen.
func TestGoToMessageWithWrappedHistory(t *testing.T) {
	m := richModel(t)
	m.height = 18
	var messages []schema.MessageV2
	for i := int64(1); i <= 40; i++ {
		messages = append(messages, schema.MessageV2{ID: i, AuthorID: 8, Body: fmt.Sprintf("msg-%02d %s", i, linesBody), CreatedAt: at(13, 15, 0)})
	}
	m.messages["general"] = messages
	m = m.centreOn(9)
	view := m.View()
	if !strings.Contains(view, "msg-10") || strings.Contains(view, "msg-40") {
		t.Errorf("not centred on msg-10:\n%s", view)
	}
}
