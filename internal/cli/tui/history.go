package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/njdaniel/conch/pkg/schema"
)

// messageHead is the start of a message as drawn: the scope badge, the time,
// the author, the agent and payload badges, and ": ".
type messageHead struct {
	// styled is the head as drawn, ending in ": ", and cells its width.
	styled string
	cells  int
	// fits is false when not even the head fits the pane; short is then as
	// much of it as does, unstyled.
	fits  bool
	short string
	// when and scope are the parts a continuation line is indented by.
	when, scope string
}

// head lays out the head of message for a pane of outer width width. It is
// what messageLine and messageBlock share, so the guarantees messageLine
// describes hold for both.
func (m Model) head(channel string, message schema.MessageV2, width int) messageHead {
	inner := width - 2 // the pane's border
	var scope, payload string
	if text := m.scopeText(channel, message.Audience); text != "" {
		scope = "[" + fitCells(text, width/3) + "]"
	}
	if message.Payload != nil {
		payload = "[" + fitCells(sanitize(message.Payload.Schema), width/3) + "]"
	}
	when := m.messageTime(message)
	name, agent := m.messageAuthor(channel, message)
	agentBadge := ""
	if agent {
		agentBadge = "[agent]"
	}
	// fixed is what the line spends on everything but the name and the body:
	// the badges, the time, the separators and ": ".
	fixed := func(when string) int {
		n := 2
		for _, part := range []string{scope, when, agentBadge, payload} {
			if part != "" {
				n += cellWidth(part) + 1
			}
		}
		return n
	}
	// The name gets what the rest leaves, at most 24 cells. When that is under
	// 4 the time goes first: the name is what says who is speaking.
	nameCells := min(24, inner-fixed(when))
	if nameCells < 4 && when != "" {
		when = ""
		nameCells = min(24, inner-fixed(when))
	}
	name = fitCells(name, nameCells)

	plain := strings.Join(nonEmpty(scope, when, name, agentBadge, payload), " ")
	if cellWidth(plain)+2 > inner {
		// Not even the head fits (a very narrow pane): keep as much of it as
		// does, unstyled, so the line is still one row.
		return messageHead{short: fitCells(plain+": ", inner), when: when, scope: scope}
	}
	var out strings.Builder
	for _, part := range []struct {
		text  string
		style func(...string) string
	}{{scope, badgeStyle.Render}, {when, statusStyle.Render}, {name, authorStyle.Render}, {agentBadge, badgeStyle.Render}, {payload, badgeStyle.Render}} {
		if part.text != "" {
			out.WriteString(part.style(part.text) + " ")
		}
	}
	// The space after the last part is the one before ": " in the format, and
	// ": " itself follows the name or badge directly.
	return messageHead{styled: strings.TrimSuffix(out.String(), " ") + ": ", cells: cellWidth(plain) + 2, fits: true, when: when, scope: scope}
}

// contMark starts every continuation line of a message, after its indent of
// at least two spaces. A message's first line starts with its time, its scope
// badge or its author, never with a space, and every other line of the
// message with spaces and this mark, so no line of a body can pass for the
// start of another message, or for a badge.
const contMark = "┊ "

// minBodyCells is the least room on a message's first line worth starting
// the body in; with less, the body starts on the next line.
const minBodyCells = 8

// messageBlock renders one message in as many lines as it takes, each of at
// most width-2 cells (the pane's border): the head and the start of the body
// on the first, as messageLine draws it, and the rest of the body wrapped on
// lines that start with spaces and contMark. Line breaks in the body are
// kept; control characters are removed as for one line.
func (m Model) messageBlock(channel string, message schema.MessageV2, width int) []string {
	inner := width - 2
	h := m.head(channel, message, width)
	paras := blockParagraphs(message.Body)
	if h.fits && len(paras) == 1 && cellWidth(paras[0]) <= inner-h.cells && paras[0] == escapeBadge(sanitize(message.Body)) {
		// The common case: a one-line body that fits beside the head.
		return []string{m.messageLine(channel, message, width)}
	}

	// Never less than 2: a head never starts with a space (a name is
	// trimmed), so a line that does is never the start of a message.
	indent := 2
	if h.when != "" && h.scope == "" && inner-cellWidth(h.when)-1-cellWidth(contMark) >= minBodyCells {
		indent = cellWidth(h.when) + 1
	}
	cont := strings.Repeat(" ", indent) + statusStyle.Render(contMark)
	contW := max(1, inner-indent-cellWidth(contMark))

	first := h.short
	room := 0
	if h.fits {
		first, room = h.styled, inner-h.cells
	}
	if len(paras) == 0 {
		return []string{first}
	}
	var lines []string
	for i, p := range paras {
		if i == 0 && room >= min(minBodyCells, cellWidth(p)) && room > 0 {
			chunks := wrapWords(p, room, contW)
			lines = append(lines, first+chunks[0])
			for _, c := range chunks[1:] {
				lines = append(lines, cont+c)
			}
			continue
		}
		if i == 0 {
			lines = append(lines, first)
		}
		for _, c := range wrapWords(p, contW, contW) {
			lines = append(lines, cont+c)
		}
	}
	return lines
}

// blockParagraphs is a body as lines without control characters: its own
// line breaks kept, trailing ones dropped. A body that starts with "[" is
// shown with a leading backslash, as on one line.
func blockParagraphs(body string) []string {
	body = strings.TrimRight(sanitizeMultiline(body), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(escapeBadge(body), "\n")
}

// escapeBadge is a body with a leading backslash when it starts with "[", so
// that it cannot read as a badge.
func escapeBadge(body string) string {
	if strings.HasPrefix(body, "[") {
		return "\\" + body
	}
	return body
}

// wrapWords cuts s into lines, the first of at most first cells and the rest
// of at most rest: at the last space that fits when there is one, the space
// itself dropped, and inside a word (a long address, a run of wide
// characters) only when it is longer than a line. "" is one empty line.
func wrapWords(s string, first, rest int) []string {
	width := max(first, 1)
	var lines []string
	for cellWidth(s) > width {
		used, cut, space := 0, 0, -1
		for i, r := range s {
			w := runeCells(r)
			if used+w > width {
				cut = i
				break
			}
			used += w
			if r == ' ' {
				space = i
			}
		}
		switch {
		case space > 0:
			lines = append(lines, s[:space])
			s = s[space+1:]
		case cut > 0:
			lines = append(lines, s[:cut])
			s = s[cut:]
		default:
			// Not one character fits: give it a line of its own rather than
			// loop. Only a width under a wide character's two cells does this.
			_, size := utf8.DecodeRuneInString(s)
			lines = append(lines, s[:size])
			s = s[size:]
		}
		width = max(rest, 1)
	}
	if s == "" && len(lines) > 0 {
		// The last character took a line of its own.
		return lines
	}
	return append(lines, s)
}

// historyOwners is the selected channel's history laid out for a pane of
// outer width width: for each line, oldest first, the index of its message.
func (m Model) historyOwners(width int) []int {
	channel := m.current()
	var owner []int
	for i, message := range m.messages[channel] {
		for range m.messageBlock(channel, message, width) {
			owner = append(owner, i)
		}
	}
	return owner
}

// historyHeight is how many lines messages take in the message pane.
func (m Model) historyHeight(channel string, messages []schema.MessageV2) int {
	width := m.chatLayout().rightW
	n := 0
	for _, message := range messages {
		n += len(m.messageBlock(channel, message, width))
	}
	return n
}

// historyView is what the message pane shows: the rows lines that end scroll
// lines above the newest, and how many messages are wholly below them. Only
// the messages it needs are laid out, newest first. scroll is clamped to the
// oldest line.
func (m Model) historyView(width, rows int) (lines []string, newer int, cut bool) {
	channel := m.current()
	messages := m.messages[channel]
	var blocks [][]string
	need := m.scroll + rows
	got := 0
	i := len(messages) - 1
	for ; i >= 0 && got < need; i-- {
		block := m.messageBlock(channel, messages[i], width)
		blocks = append(blocks, block)
		got += len(block)
	}
	scroll := m.scroll
	if i < 0 {
		scroll = min(scroll, max(0, got-rows))
	}
	all := make([]string, 0, got)
	owner := make([]int, 0, got)
	for k := len(blocks) - 1; k >= 0; k-- {
		all = append(all, blocks[k]...)
		for range blocks[k] {
			owner = append(owner, len(messages)-1-k)
		}
	}
	end := len(all) - scroll
	start := max(0, end-rows)
	if end <= 0 {
		return nil, 0, false
	}
	bottom := owner[end-1]
	return all[start:end], len(messages) - 1 - bottom, end < len(all) && owner[end] == bottom
}

// historyFoot is what the bottom border of the message pane says while it is
// scrolled up: how many messages are below and the key back to the newest.
func (m Model) historyFoot(newer int, cut bool) string {
	if m.scroll <= 0 {
		return ""
	}
	switch {
	case newer > 0:
		return "↓ " + strconv.Itoa(newer) + " newer · end: latest"
	case cut:
		return "↓ more below · end: latest"
	}
	return ""
}

// scrollMessages moves the message pane up by lines (down when negative),
// stopping at the oldest and at the newest line.
//
// The scroll is counted in lines as the history is laid out now, and only an
// arrival or a resize moves it to match. Anything else that changes how many
// lines a message below the pane takes leaves what is on screen off by that
// much: the member list loading names in place of numbers, or the day
// changing, so that "HH:MM" becomes "MM-DD HH:MM". That is a line or two, once,
// and never a jump to the bottom.
func (m Model) scrollMessages(lines int) Model {
	channel := m.current()
	top := max(0, m.historyHeight(channel, m.messages[channel])-m.chatLayout().messageRows())
	m.scroll = min(max(min(m.scroll, top)+lines, 0), top)
	return m
}

// scrollAnchor is the message at the bottom of the message pane and how many
// of its lines are below the bottom row: what a resize keeps on screen.
func (m Model) scrollAnchor() (message, below int, ok bool) {
	if m.scroll <= 0 {
		return 0, 0, false
	}
	owner := m.historyOwners(m.chatLayout().rightW)
	bottom := len(owner) - 1 - m.scroll
	if bottom < 0 || bottom >= len(owner) {
		return 0, 0, false
	}
	message = owner[bottom]
	last := bottom
	for last+1 < len(owner) && owner[last+1] == message {
		last++
	}
	return message, last - bottom, true
}

// scrollTo sets the scroll so that the line below lines of message is the
// bottom row of the pane, as near as the history allows.
func (m Model) scrollTo(message, below int) Model {
	owner := m.historyOwners(m.chatLayout().rightW)
	last := -1
	for i, o := range owner {
		if o == message {
			last = i
		}
	}
	if last < 0 {
		return m
	}
	first := last
	for first > 0 && owner[first-1] == message {
		first--
	}
	bottom := max(first, last-below)
	m.scroll = min(max(len(owner)-1-bottom, 0), max(0, len(owner)-m.chatLayout().messageRows()))
	return m
}

// centreOn scrolls the message pane so that message at is in the middle of
// it, or as near as the history allows.
func (m Model) centreOn(at int) Model {
	owner := m.historyOwners(m.chatLayout().rightW)
	first := -1
	for i, o := range owner {
		if o == at {
			first = i
			break
		}
	}
	if first < 0 {
		return m
	}
	rows := m.chatLayout().messageRows()
	m.scroll = min(max(len(owner)-1-(first+rows/2), 0), max(0, len(owner)-rows))
	return m
}
