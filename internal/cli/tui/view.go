package tui

import (
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// paneFocus is the pane of the chat view the arrow keys move. Typed text
// always goes to the input line, whatever has the focus.
type paneFocus int

const (
	// focusChannels is the channel list, and the zero value: ↑/↓ choose a
	// channel, as they always have.
	focusChannels paneFocus = iota
	// focusMessages is the message pane: ↑/↓ scroll it a line at a time.
	focusMessages
)

// View draws the screen from layout: the title bar, the panes, the input line,
// the members line and voice strip of the chat view, and the status line. It
// fills the terminal exactly, at every size from minWidth x minHeight up.
func (m Model) View() string {
	if w, h := m.size(); w < minWidth || h < minHeight {
		return tooSmallScreen(w, h)
	}
	l := m.layout()
	width := l.width
	if (m.mode == modeInbox || m.mode == modeDecision) && (width < minApprovalWidth || l.height < minApprovalHeight) {
		return tooSmall(width, l.height)
	}
	var rows []string
	if l.top > 0 {
		rows = append(rows, m.titleBar(width))
	}
	if m.help {
		rows = append(rows, m.helpPane(l))
	} else {
		rows = append(rows, m.panes(l))
	}
	rows = append(rows, m.inputRow(width))
	if l.members != "" {
		rows = append(rows, statusStyle.Width(width).Render(l.members))
	}
	if l.voice != "" {
		rows = append(rows, statusStyle.Width(width).Render(l.voice))
	}
	rows = append(rows, m.statusRow(width))
	return strings.Join(rows, "\n")
}

// panes is the panes of the view the user is in.
func (m Model) panes(l layout) string {
	switch m.mode {
	case modeInbox, modeDecision:
		inbox := paneBox("APPROVALS", m.mode == modeInbox, m.inboxLines(l.leftW-2, l.contentH), l.leftW, l.contentH, "")
		var detailsLines []string
		if d, ok := m.selectedDetails(); ok {
			detailsLines = d.lines
		}
		title := "DETAILS"
		if m.mode == modeDecision {
			title = "DECIDE"
		}
		details := paneBox(title, m.mode == modeDecision, detailsLines, l.rightW, l.contentH, "")
		return lipgloss.JoinHorizontal(lipgloss.Top, inbox, details)
	case modeAgents:
		return m.agentsPanes(l)
	case modeRuns:
		return m.runsPanes(l)
	case modeRun:
		return paneBox("RUN #"+strconv.FormatInt(m.rd.runID, 10), true, m.runLines(l.width-2, l.contentH), l.width, l.contentH, "")
	}
	return m.chatPanes(l)
}

// chatPanes is the chat view: the channel list, the messages, and in the
// wide layout the people pane.
func (m Model) chatPanes(l layout) string {
	channels := paneBox("CHANNELS", m.focus == focusChannels, m.channelWindow(l), l.leftW, l.contentH, "")
	lines, foot := m.chatLines(l)
	messages := paneBox(m.chatTitle(), m.focus == focusMessages, lines, l.rightW, l.contentH, foot)
	if l.peopleW == 0 {
		return lipgloss.JoinHorizontal(lipgloss.Top, channels, messages)
	}
	people := paneBox(m.peopleTitle(), false, m.peopleLines(l.peopleW-2, l.contentH), l.peopleW, l.contentH, "")
	return lipgloss.JoinHorizontal(lipgloss.Top, channels, messages, people)
}

// channelWindow is the part of the channel list the left pane shows: all of
// it when it fits, else the part around the selected channel. mouse finds a
// clicked channel with the same channelStart.
func (m Model) channelWindow(l layout) []string {
	lines := m.channelLines(l.leftW)
	if len(lines) <= l.contentH {
		return lines
	}
	start := m.channelStart(l.contentH)
	return lines[start : start+l.contentH]
}

// chatTitle names the conversation on screen and, when it is not the whole
// channel, where plain messages go.
func (m Model) chatTitle() string {
	channel := sanitize(m.current())
	if channel == "" {
		return "MESSAGES"
	}
	title := "# " + channel
	if m.target != nil && m.target.channel == m.current() {
		title += " › net " + sanitize(m.target.name)
	}
	return title
}

// chatLines is the message pane's content and the label of its bottom border:
// the history, or what there is instead of one.
func (m Model) chatLines(l layout) (lines []string, foot string) {
	channel := m.current()
	cols := l.rightW - 2
	switch {
	case len(m.messages[channel]) > 0:
		lines, newer, cut := m.historyView(l.rightW, l.messageRows())
		return lines, m.historyFoot(newer, cut)
	case m.channelsErr != nil:
		return m.channelsErrLines(cols), ""
	case channel == "":
		return []string{statusStyle.Render(fitCells("loading channels…", cols))}, ""
	case !m.loaded[channel]:
		return []string{statusStyle.Render(fitCells("loading history…", cols))}, ""
	}
	text := wrapWords("No messages in "+sanitize(channel)+" yet. Type below and press enter to send one.", cols, cols)
	for i, t := range text {
		text[i] = statusStyle.Render(t)
	}
	return text, ""
}

// channelsErrLines says, in a pane of the given inner width, why there are no
// channels: the server asked, its error in full, and how to choose another.
// The TUI takes no flags, so CONCH_SERVER is the way.
func (m Model) channelsErrLines(width int) []string {
	from := "the server"
	if s, ok := m.api.(interface{ Server() string }); ok {
		from = s.Server()
	}
	paragraphs := []string{"Cannot read the channel list from " + from + ".", m.channelsErr.Error()}
	if !errors.Is(m.channelsErr, cli.ErrUnauthenticated) {
		paragraphs = append(paragraphs, "To use another server, quit (esc) and start conch with CONCH_SERVER=<url>.")
	}
	var lines []string
	for i, p := range paragraphs {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, wrapCells(sanitize(p), width)...)
	}
	return lines
}

// channelLines is the channel list in a pane of the given outer width.
func (m Model) channelLines(leftWidth int) []string {
	lines := make([]string, len(m.channels))
	for i, channel := range m.channels {
		badge := m.unreadBadge(channel)
		room := leftWidth - 4
		if badge != "" {
			room -= 1 + len(badge)
		}
		name := fitCells(sanitize(channel), room)
		prefix := "  "
		if i == m.selected {
			prefix = activeStyle.Render("› ")
			name = activeStyle.Render(name)
		}
		lines[i] = prefix + name
		if badge != "" {
			lines[i] += " " + badgeStyle.Render(badge)
		}
	}
	if len(lines) == 0 && m.loadingChannels {
		lines = append(lines, statusStyle.Render("loading…"))
	}
	return lines
}

// size is the terminal size, or 80x24 before the first WindowSizeMsg.
func (m Model) size() (width, height int) {
	width, height = m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	return width, height
}

// agentsPanes is the Agents view: the channel list beside the agents from
// 80x24 up, the agents alone, full width, below it.
func (m Model) agentsPanes(l layout) string {
	title := "AGENTS · " + sanitize(m.current())
	if l.width < fullWidth || l.height < fullHeight {
		lines, sel := m.agentLines(l.width - 2)
		return paneBox(title, true, agentsWindow(lines, sel, l.contentH), l.width, l.contentH, "")
	}
	channels := paneBox("CHANNELS", false, m.channelWindow(l), l.leftW, l.contentH, "")
	lines, sel := m.agentLines(l.rightW - 2)
	agents := paneBox(title, true, agentsWindow(lines, sel, l.contentH), l.rightW, l.contentH, "")
	return lipgloss.JoinHorizontal(lipgloss.Top, channels, agents)
}

// runsPanes is the Runs view: laid out as the Agents view is.
func (m Model) runsPanes(l layout) string {
	title := "RUNS · " + sanitize(m.current()) + " · " + m.runs.filter.String()
	if l.width < fullWidth || l.height < fullHeight {
		lines, sel := m.runsLines(l.width - 2)
		return paneBox(title, true, agentsWindow(lines, sel, l.contentH), l.width, l.contentH, "")
	}
	channels := paneBox("CHANNELS", false, m.channelWindow(l), l.leftW, l.contentH, "")
	lines, sel := m.runsLines(l.rightW - 2)
	runs := paneBox(title, true, agentsWindow(lines, sel, l.contentH), l.rightW, l.contentH, "")
	return lipgloss.JoinHorizontal(lipgloss.Top, channels, runs)
}

// inputRow is the input line: the prompt and the draft in the chat view, the
// reason in the decision prompt, run detail's own line, and nothing elsewhere.
// The cursor mark says where typing goes. While the help is open typing goes
// nowhere, so the draft stays on screen without it.
func (m Model) inputRow(width int) string {
	row := lipgloss.NewStyle().Width(width)
	cursor := activeStyle.Render("▌")
	if m.help {
		cursor = " "
	}
	switch m.mode {
	case modeChannels, modeDecision:
		prompt := ">"
		if m.mode == modeChannels {
			prompt = m.promptWithin(width / 2)
		}
		return row.Render(prompt + " " + clipTail(sanitize(m.input), width-cellWidth(prompt)-3) + cursor)
	case modeRun:
		return row.Render(fitCells(m.runPrompt(width), width))
	}
	return row.Render("")
}

// statusHints are the key hints of the view the user is in, in the order they
// are shown. The help lists every key. Where there is a title bar it offers
// the help; without one the status line does, when there is room.
func (m Model) statusHints() []hint {
	if m.help {
		return []hint{{"esc", "close help", 0}}
	}
	var hints []hint
	switch m.mode {
	case modeDecision:
		hints = []hint{{"↑/↓", "options", 3}, {"enter", "confirm", 1}, {"esc", "cancel", 0}}
	case modeRun:
		hints = []hint{{"↑/↓", "timeline", 3}, {"enter", "data", 4}, {"g", "message", 5}, {"esc", "back", 0}}
	case modeAgents:
		hints = []hint{{"↑/↓", "agents", 4}, {"enter", "sessions", 1}, {"r", "runs", 3}, {"tab", "channels", 2}, {"esc", "quit", 0}}
	case modeRuns:
		hints = []hint{{"↑/↓", "runs", 4}, {"enter", "open", 1}, {"f", "filter", 3}, {"a", "agents", 2}, {"esc", "quit", 0}}
	case modeInbox:
		hints = []hint{{"↑/↓", "approvals", 3}, {"enter", "decide", 1}, {"tab", "agents", 2}, {"esc", "quit", 0}}
	default:
		arrows := hint{"↑/↓", "channels", 3}
		if m.focus == focusMessages {
			arrows = hint{"↑/↓", "scroll", 3}
		}
		hints = []hint{arrows, {"←/→", "focus", 6}, {"enter", "send", 1}, {"tab", "inbox", 2}, {"esc", "quit", 0}}
		if m.scroll > 0 {
			hints = append(hints[:1], append([]hint{{"end", "latest", 4}}, hints[1:]...)...)
		}
	}
	if w, h := m.size(); !titled(w, h) {
		hints = append(hints[:len(hints)-1], hint{"F1", "help", 5}, hints[len(hints)-1])
	}
	return hints
}

// statusRow is the status line: who is signed in, the status of the view, and
// its key hints. It is one row whatever the message holds: on a narrow
// terminal the message is what is cut, then the hints that matter least,
// never the way out.
func (m Model) statusRow(width int) string {
	statusKeys := hintLine(m.statusHints(), width-max(16, width/5))
	status := m.status()
	if m.mode == modeChannels {
		status = fitCells(status, width-cellWidth(statusKeys)) + statusKeys
		if name := sanitize(m.userName); name != "" {
			// The name gets the room the status and key hints leave; when they
			// already fill the line the name goes, never the hints. lipgloss's
			// Width only pads, so without this clip a long name wraps and breaks
			// the layout.
			if room := width - cellWidth(status) - 3; room > 0 {
				status = clip(name, room) + " | " + status
			}
		}
	} else {
		if name := sanitize(m.userName); name != "" {
			status = name + " | " + status
		}
		// One line whatever it holds, so the panes are never pushed up off
		// the screen: the message is what is cut, never the key hints.
		status = fitCells(sanitize(status), width-cellWidth(statusKeys)) + statusKeys
	}
	// One row at the smallest terminal too, where the key hints alone are
	// wider than the screen.
	return statusStyle.Width(width).Render(fitCells(status, width))
}

// titleBar is the top row from fullWidth x fullHeight up: the app and the
// server, the views with the one on screen in brackets, and on the right who
// is signed in, the state of the text connection, which is not that of voice
// (the voice strip has that), and the key to the help.
func (m Model) titleBar(width int) string {
	host := m.serverHost()
	tabs, tabsW := m.viewTabs()
	glyph, word, up := m.textLink()
	link := glyph + " text " + word
	linkStyled := warnStyle.Render(link)
	if up {
		linkStyled = okStyle.Render(link)
	}
	// Some terminals keep F1 for themselves (GNOME Terminal opens its own
	// help), so the bar also names the way that always works in this view.
	help := m.helpKeys()
	who := m.identity()
	// What gives way on a narrow terminal: the server, then who, then the
	// tabs; the connection and the way to the help stay.
	used := func() int {
		n := cellWidth("CONCH") + cellWidth(link) + 3 + cellWidth(help)
		if host != "" {
			n += 1 + cellWidth(host)
		}
		if tabs != "" {
			n += 2 + tabsW
		}
		if who != "" {
			n += cellWidth(who) + 3
		}
		return n + 1
	}
	if used() > width && host != "" {
		host = fitCells(host, max(0, cellWidth(host)-(used()-width)))
		if cellWidth(host) < 8 {
			host = ""
		}
	}
	if used() > width {
		who = ""
	}
	if used() > width {
		tabs = ""
	}
	left, leftW := titleStyle.Render("CONCH"), cellWidth("CONCH")
	if host != "" {
		left += " " + statusStyle.Render(host)
		leftW += 1 + cellWidth(host)
	}
	if tabs != "" {
		left += "  " + tabs
		leftW += 2 + tabsW
	}
	right := linkStyled + " · " + statusStyle.Render(help)
	rightW := cellWidth(link) + 3 + cellWidth(help)
	if who != "" {
		right = authorStyle.Render(who) + " · " + right
		rightW += cellWidth(who) + 3
	}
	gap := width - leftW - rightW
	if gap < 1 {
		return fitCells("CONCH "+link+" · "+help, width)
	}
	return left + strings.Repeat(" ", gap) + right
}

// viewTabs are the views tab cycles through, the one on screen in brackets so
// that it reads without colour, and their width.
func (m Model) viewTabs() (string, int) {
	// Run detail is a view of its own, the same whichever list opened it.
	active := map[mode]string{modeChannels: "Chat", modeInbox: "Inbox", modeDecision: "Inbox", modeAgents: "Agents", modeRuns: "Runs"}[m.mode]
	var parts, plain []string
	for _, name := range []string{"Chat", "Inbox", "Agents", "Runs"} {
		if name == active {
			parts = append(parts, activeStyle.Render("["+name+"]"))
			plain = append(plain, "["+name+"]")
			continue
		}
		parts = append(parts, statusStyle.Render(name))
		plain = append(plain, name)
	}
	if m.mode == modeRun {
		run := "[run #" + strconv.FormatInt(m.rd.runID, 10) + "]"
		parts = append(parts, activeStyle.Render(run))
		plain = append(plain, run)
	}
	return strings.Join(parts, " "), cellWidth(strings.Join(plain, " "))
}

// serverHost is the host of the server the client talks to, "" when the API
// does not say.
func (m Model) serverHost() string {
	s, ok := m.api.(interface{ Server() string })
	if !ok {
		return ""
	}
	raw := s.Server()
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		raw = u.Host
	}
	return sanitize(raw)
}

// identity is who the TUI acts as, in the words the user knows: the name the
// server gave, else the id, else why there is none yet.
func (m Model) identity() string {
	switch {
	case sanitize(m.userName) != "":
		return fitCells(sanitize(m.userName), 24)
	case m.authenticated && m.whoErr != nil:
		return "identity unknown"
	case m.authenticated:
		return "signing in…"
	case m.authorID > 0:
		return "#" + strconv.FormatInt(m.authorID, 10)
	}
	return "no identity"
}

// textLink is the state of the text connection of the channel on screen: a
// glyph and a word for it, and whether it is up.
func (m Model) textLink() (glyph, word string, up bool) {
	channel := m.current()
	switch {
	case m.channelsErr != nil:
		return "○", "unavailable", false
	case channel == "":
		return "◌", "connecting", false
	case m.retryPending[channel]:
		return "◐", "reconnecting", false
	case m.loaded[channel] && m.subscribed[channel]:
		return "●", "live", true
	}
	return "◌", "connecting", false
}

// peopleTitle is the title of the people pane: how many people are online.
func (m Model) peopleTitle() string {
	if m.ev.state != eventsLive {
		return "PEOPLE"
	}
	online := map[int64]bool{}
	for _, s := range m.rosterSessions() {
		if s.Kind != schema.PrincipalAgent && s.State == schema.SessionStateOnline {
			online[s.PrincipalID] = true
		}
	}
	return "PEOPLE · " + strconv.Itoa(len(online)) + " online"
}

// human is one person of the people pane.
type human struct {
	id    int64
	name  string
	live  int
	state schema.SessionState
}

// peopleLines is the people pane in rows lines of at most cols cells: the
// channel's people, each once, with how many sessions they have live; its
// agents with their status and task; and who is in its voice room. Every
// state is a glyph and a word. A list too long for the pane ends in how many
// are not shown.
func (m Model) peopleLines(cols, rows int) []string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	head := func(s string) { add(titleStyle.Render(fitCells(s, cols))) }
	if note := m.agentsNote(); note != "" {
		add(statusStyle.Render(fitCells("people: "+note, cols)))
	} else {
		for _, p := range m.people() {
			name := p.name
			if p.id == m.authorID && m.authorID > 0 {
				name += " (you)"
			}
			word := fitCells(sanitize(string(p.state)), cols/2)
			if p.live > 1 {
				word += " ×" + strconv.Itoa(p.live)
			}
			room := max(1, cols-3-cellWidth(word))
			add(agentGlyph(p.state) + " " + padCells(fitCells(name, room), room) + " " + statusStyle.Render(word))
		}
		if rows := m.agentRows(); len(rows) > 0 {
			add("")
			head("AGENTS")
			for _, row := range rows {
				status, state := statusWord(schema.SessionStatusOffline), schema.SessionStateOffline
				var task string
				if lead, ok := row.lead(); ok {
					status, state, task = statusWord(lead.Status), lead.State, sanitize(lead.Task)
				}
				// The glyph, the spaces and the badge take 11 cells; the name is
				// cut before the status, which says what the agent is doing.
				name := fitCells(row.name, max(4, cols-11-cellWidth(status)))
				status = fitCells(status, max(0, cols-11-cellWidth(name)))
				add(agentGlyph(state) + " " + name + " " + badgeStyle.Render(agentBadge) + " " + statusStyle.Render(status))
				if task != "" {
					add(statusStyle.Render(fitCells("  task: "+task, cols)))
				}
			}
		}
	}
	if voice := m.voicePeople(cols); len(voice) > 0 {
		add("")
		lines = append(lines, voice...)
	}
	if len(lines) > rows {
		more := len(lines) - rows + 1
		lines = append(lines[:rows-1], statusStyle.Render(fitCells("… "+strconv.Itoa(more)+" more lines (tab: Agents)", cols)))
	}
	return lines
}

// people is the channel's humans, each once: every member, and anyone the
// roster has that the member list does not have yet. Online ones come first,
// then stale, then offline, each in principal id order, which does not move
// as a roster is refreshed.
func (m Model) people() []human {
	channel := m.current()
	byID := map[int64]*human{}
	var list []*human
	for id, member := range m.members[channel] {
		if member.Kind == schema.PrincipalAgent {
			continue
		}
		name := safeName(member.Name)
		if name == "" {
			name = "#" + strconv.FormatInt(id, 10)
		}
		p := &human{id: id, name: name, state: schema.SessionStateOffline}
		byID[id] = p
		list = append(list, p)
	}
	for _, s := range m.rosterSessions() {
		if s.Kind == schema.PrincipalAgent {
			continue
		}
		p, ok := byID[s.PrincipalID]
		if !ok {
			p = &human{id: s.PrincipalID, name: sessionName(s), state: schema.SessionStateOffline}
			byID[s.PrincipalID] = p
			list = append(list, p)
		}
		if s.State == schema.SessionStateOnline {
			p.live++
		}
		if stateRank(s.State) > stateRank(p.state) {
			p.state = s.State
		}
	}
	out := make([]human, len(list))
	for i, p := range list {
		out[i] = *p
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := stateRank(out[i].state), stateRank(out[j].state); a != b {
			return a > b
		}
		return out[i].id < out[j].id
	})
	return out
}
