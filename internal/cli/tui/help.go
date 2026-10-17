package tui

import (
	"strconv"
)

// The help overlay. F1 opens it everywhere, and so does "?" in a view where
// nothing is being typed, and "/help" in the chat view. While it is open it
// takes every key: esc, F1, "?" or enter close it, the arrow and page keys
// scroll it, ctrl+c closes it and then does what it does in the view (cancels
// a decision, quits elsewhere), and nothing else reaches the view under it,
// so nothing typed while reading it is sent, decided or asked. The mouse is
// ignored too.

// helpEntry is one line of the help: the keys and what they do.
type helpEntry struct{ keys, does string }

// helpTopic is what the help overlay shows.
type helpTopic int

const (
	// topicKeys is the key map of the view on screen: F1, ?, /help.
	topicKeys helpTopic = iota
	// topicVoice is the voice commands, the talk key and the SSH note:
	// /voice help.
	topicVoice
	// topicVoiceStatus is everything voice is doing: /voice status.
	topicVoiceStatus
)

// helpName is the name of the view the help is about, as the tabs name it.
func (m Model) helpName() string {
	switch m.helpTopic {
	case topicVoice:
		return "Voice"
	case topicVoiceStatus:
		return "Voice status"
	}
	switch m.mode {
	case modeInbox:
		return "Inbox"
	case modeDecision:
		return "Decide"
	case modeAgents:
		return "Agents"
	case modeRuns:
		return "Runs"
	case modeRun:
		return "Run detail"
	}
	return "Chat"
}

// helpEntries is the key map of the view the user is in, the keys every view
// shares last.
func (m Model) helpEntries() []helpEntry {
	if m.helpTopic == topicVoice {
		return voiceHelpEntries()
	}
	var entries []helpEntry
	switch m.mode {
	case modeChannels:
		entries = []helpEntry{
			{"type, enter", "write a message and send it to the audience the prompt names"},
			{"backspace", "delete the last character of the draft"},
			{"/net NAME", "send plain messages to net NAME; /net alone: the whole channel"},
			{"/w IDS TEXT", "whisper TEXT once to the principal ids IDS (3,5)"},
			{"//TEXT", "send TEXT that starts with /"},
		}
		if m.vc.owner != nil {
			entries = append(entries, helpEntry{"/voice …", "join, leave, mute and deafen voice; /voice help says how"})
		}
		entries = append(entries, []helpEntry{
			{"/help", "this help"},
			{"↑/↓", "focus on channels: open the channel above or below"},
			{"", "focus on messages: scroll the history a line"},
			{"←/→", "move the focus between the channel list and the messages"},
			{"pgup/pgdn", "scroll the history a page"},
			{"home/end", "the oldest message held / back to the newest"},
			{"tab", "Inbox"},
		}...)
	case modeInbox:
		entries = []helpEntry{
			{"↑/↓", "choose an approval"},
			{"pgup/pgdn", "scroll its body"},
			{"enter", "decide it: choose an option, type a reason, enter"},
			{"tab", "Agents"},
		}
	case modeDecision:
		entries = []helpEntry{
			{"↑/↓", "choose an option"},
			{"type", "the reason, which is required"},
			{"enter", "cast the decision with that option and reason"},
			{"pgup/pgdn", "scroll the body"},
			{"esc", "cancel and go back to the inbox"},
		}
	case modeAgents:
		entries = []helpEntry{
			{"↑/↓", "choose an agent, a session or a run"},
			{"enter", "unfold an agent's sessions, or open a run"},
			{"r", "the channel's runs"},
			{"tab", "Chat"},
		}
	case modeRuns:
		entries = []helpEntry{
			{"↑/↓ pgup/pgdn", "choose a run"},
			{"enter", "open it"},
			{"f", "show all, open or ended runs"},
			{"a", "back to Agents"},
			{"tab", "Chat"},
		}
	case modeRun:
		entries = []helpEntry{
			{"↑/↓ pgup/pgdn", "move through the timeline"},
			{"enter", "unfold or fold an entry's data"},
			{"g", "go to the message the run answers"},
			{"m x p r", "message, cancel, pause, resume: only what the session supports"},
			{"esc", "back to the view the run was opened from"},
		}
	}
	if m.mode != modeDecision && m.mode != modeRun {
		entries = append(entries, helpEntry{"shift+tab", "the view before"}, helpEntry{"esc", "quit"})
	}
	ctrlC := helpEntry{"ctrl+c", "quit"}
	switch m.mode {
	case modeDecision:
		ctrlC.does = "cancel the decision"
	case modeRun:
		ctrlC.does = "quit; in the message or the cancel question, drop it"
	}
	return append(entries,
		helpEntry{"F1", "open or close this help; where the terminal keeps F1 for itself, /help in Chat and ? elsewhere"},
		ctrlC,
	)
}

// helpNotes are what the help says after the keys: the mouse, and what the
// TUI does not do.
func (m Model) helpNotes() []string {
	if m.helpTopic == topicVoice {
		return voiceHelpNotes()
	}
	notes := []string{"Mouse: a click selects or focuses and the wheel scrolls; a click never sends, decides or quits."}
	switch {
	case m.mode != modeChannels:
	case m.vc.owner != nil:
		notes = append(notes, "Voice: /voice join joins the voice of the channel on screen, or of one you name; browsing a channel never moves your voice. You talk with the talk key conch-voice reads, never with a key typed here (/voice help).")
	default:
		notes = append(notes, "Voice is shown, not joined: browsing a channel never moves your voice, and this terminal has no talk key.")
	}
	return notes
}

// helpPane draws the help in place of the panes, the same size, so the input
// line, the strips and the status line stay where they are. A help longer
// than the pane scrolls with the arrow and page keys.
func (m Model) helpPane(l layout) string {
	outer := helpWidth(l, m.mode)
	lines := m.helpBody(outer - 2)
	foot := "esc close"
	if len(lines) > l.contentH {
		top := min(max(m.helpTop, 0), len(lines)-l.contentH)
		below := len(lines) - top - l.contentH
		lines = lines[top : top+l.contentH]
		foot = "↑/↓ scroll · esc close"
		if below > 0 {
			foot = "↓ " + strconv.Itoa(below) + " more · " + foot
		}
	}
	return paneBox("HELP · "+m.helpName(), true, lines, outer, l.contentH, foot)
}

// helpWidth is the outer width of the help: that of the panes it stands in
// for. Run detail's one pane is the whole width, the others leave the last
// column free.
func helpWidth(l layout, md mode) int {
	if md == modeRun {
		return l.width
	}
	return l.width - 1
}

// helpBody is the whole help in lines of at most cols cells: the keys of the
// view, then the notes.
func (m Model) helpBody(cols int) []string {
	if m.helpTopic == topicVoiceStatus {
		return m.voiceStatusLines(cols)
	}
	entries := m.helpEntries()
	keyW := 0
	for _, e := range entries {
		keyW = max(keyW, cellWidth(e.keys))
	}
	keyW = min(keyW, cols/3)
	var lines []string
	for _, e := range entries {
		does := wrapWords(e.does, max(1, cols-keyW-3), max(1, cols-keyW-3))
		for i, d := range does {
			keys := ""
			if i == 0 {
				keys = e.keys
			}
			lines = append(lines, " "+activeStyle.Render(padCells(fitCells(keys, keyW), keyW))+"  "+d)
		}
	}
	for _, n := range m.helpNotes() {
		lines = append(lines, "")
		for _, w := range wrapWords(n, max(1, cols-1), max(1, cols-1)) {
			lines = append(lines, " "+statusStyle.Render(w))
		}
	}
	return lines
}

// helpKeys is how the title bar says to open the help in the view on screen.
func (m Model) helpKeys() string {
	switch {
	case m.help:
		return "help: esc closes"
	case m.mode == modeChannels:
		return "help: F1, /help"
	case m.opensHelp("?"):
		return "help: F1, ?"
	}
	return "help: F1"
}

// helpKey handles a key while the help is open: the keys that close it and the
// ones that scroll it. Every other key is dropped, except ctrl+c (handled false),
// which closes the help and then does what it does in the view under it: in
// the decision prompt it cancels the decision, elsewhere it quits.
func (m Model) helpKey(key string) (next Model, handled bool) {
	l := m.layout()
	last := max(0, len(m.helpBody(helpWidth(l, m.mode)-2))-l.contentH)
	switch key {
	case "ctrl+c":
		m.help = false
		return m, false
	case "esc", "f1", "?", "enter":
		m.help = false
	case "up":
		m.helpTop--
	case "down":
		m.helpTop++
	case "pgup":
		m.helpTop -= max(1, l.contentH-1)
	case "pgdown":
		m.helpTop += max(1, l.contentH-1)
	}
	m.helpTop = min(max(m.helpTop, 0), last)
	return m, true
}

// opensHelp reports whether key opens the help in the view on screen: F1
// always, and "?" where it would not be typed.
func (m Model) opensHelp(key string) bool {
	if key == "f1" {
		return true
	}
	if key != "?" {
		return false
	}
	switch m.mode {
	case modeInbox, modeAgents, modeRuns:
		return true
	case modeRun:
		return m.rd.input == inputNone
	}
	return false
}
