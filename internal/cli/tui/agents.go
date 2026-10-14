package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The Agents view is laid out in full from this size; below it only the
// agents pane is shown, full width, and tab is the way to the other views.
const (
	fullWidth  = 80
	fullHeight = 24
)

// The smallest terminal any view is drawn on. Below it View shows the
// terminal-too-small screen instead.
const (
	minWidth  = 40
	minHeight = 10
)

// tooSmallScreen fills the terminal, exactly, with a request to enlarge it.
func tooSmallScreen(width, height int) string {
	lines := make([]string, height)
	text := []string{"terminal too small", fmt.Sprintf("need at least %dx%d", minWidth, minHeight), "esc: quit"}
	for i := range lines {
		if i < len(text) {
			lines[i] = fitCells(text[i], width)
		}
	}
	return strings.Join(lines, "\n")
}

// agentRow is one agent of the channel: a member, or an agent with a session
// in the roster that the member list does not have (yet).
type agentRow struct {
	id       int64
	name     string
	sessions []schema.SessionV1
}

// agentRows lists each agent of the selected channel once. The members list is
// where an agent comes from, so one with no session is still there, and shows
// as offline; an agent that only the roster knows is added after them. Rows
// are in principal id order and then in roster order, which does not change
// as a roster is refreshed, so the selection does not move under the user.
func (m Model) agentRows() []agentRow {
	channel := m.current()
	var rows []agentRow
	index := map[int64]int{}
	members := m.members[channel]
	ids := make([]int64, 0, len(members))
	for id, member := range members {
		if member.Kind == schema.PrincipalAgent {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		name := safeName(members[id].Name)
		if name == "" {
			name = "#" + strconv.FormatInt(id, 10)
		}
		index[id] = len(rows)
		rows = append(rows, agentRow{id: id, name: name})
	}
	for _, s := range m.rosterSessions() {
		if s.Kind != schema.PrincipalAgent {
			continue
		}
		i, ok := index[s.PrincipalID]
		if !ok {
			i = len(rows)
			index[s.PrincipalID] = i
			rows = append(rows, agentRow{id: s.PrincipalID, name: sessionName(s)})
		}
		rows[i].sessions = append(rows[i].sessions, s)
	}
	return rows
}

// statusRank orders what a row shows when an agent has several sessions: what
// needs a person first. A status this client does not know ranks as online.
func statusRank(s schema.SessionStatus) int {
	switch s {
	case schema.SessionStatusBlocked:
		return 7
	case schema.SessionStatusWaiting:
		return 6
	case schema.SessionStatusRunning:
		return 5
	case schema.SessionStatusFailed:
		return 4
	case schema.SessionStatusIdle:
		return 3
	case schema.SessionStatusStale:
		return 1
	case schema.SessionStatusOffline:
		return 0
	default:
		return 2
	}
}

// lead is the session a row stands for: the first of the highest status.
func (r agentRow) lead() (schema.SessionV1, bool) {
	best := -1
	for i, s := range r.sessions {
		if best < 0 || statusRank(s.Status) > statusRank(r.sessions[best].Status) {
			best = i
		}
	}
	if best < 0 {
		return schema.SessionV1{}, false
	}
	return r.sessions[best], true
}

// statusWord is a session status as the server named it, made safe to print.
func statusWord(s schema.SessionStatus) string { return sanitize(string(s)) }

// ago is how long before now at was, as a short phrase.
func ago(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}

// sessionDetails are the facts about one session after its status: the
// runtime and model when the server returned them (and the caller wants them:
// an agent's row leaves them to its session lines, so the task keeps the
// room), that it is not known how the run ended, the task, and how long ago
// the session was last heard from. Every part is text other people and agents
// chose, so each goes through sanitize, and a label says what it is so none
// can pass for another.
func (m Model) sessionDetails(s schema.SessionV1, runtime bool) []string {
	var parts []string
	if label := runtimeModel(s); label != "" && runtime {
		parts = append(parts, label)
	}
	if s.OutcomeUnknown {
		parts = append(parts, "outcome unknown")
	}
	if s.Task != "" {
		parts = append(parts, "task: "+sanitize(s.Task))
	}
	now := m.now
	if now == nil {
		now = time.Now
	}
	if !s.LastSeenAt.IsZero() {
		parts = append(parts, ago(now(), s.LastSeenAt.Time()))
	}
	return parts
}

func runtimeModel(s schema.SessionV1) string {
	runtime, model := sanitize(s.Runtime), sanitize(s.Model)
	switch {
	case runtime != "" && model != "":
		return runtime + "/" + model
	default:
		return runtime + model
	}
}

// agentBadge is drawn before every agent name, as in the message pane.
const agentBadge = "[agent]"

// agentsNote is what the pane says instead of a list while the roster cannot
// be trusted, "" when it can.
func (m Model) agentsNote() string {
	switch m.ev.state {
	case eventsConnecting:
		return "connecting…"
	case eventsDown:
		return "unavailable"
	}
	return ""
}

// agentLines is the Agents list in lines of at most cols cells, and the line
// the selected agent starts on. An agent with no session is offline. The
// selected agent's sessions follow it when it is expanded.
func (m Model) agentLines(cols int) (lines []string, selLine int) {
	if note := m.agentsNote(); note != "" {
		return []string{statusStyle.Render(fitCells("agents: "+note, cols))}, 0
	}
	rows := m.agentRows()
	if len(rows) == 0 {
		return []string{statusStyle.Render(fitCells("No agents in this channel", cols))}, 0
	}
	sel := m.selectedAgent(rows)
	for i, row := range rows {
		prefix := "  "
		if i == sel {
			prefix = activeStyle.Render("› ")
			selLine = len(lines)
		}
		lead, has := row.lead()
		status := statusWord(schema.SessionStatusOffline)
		var detail []string
		if has {
			status = statusWord(lead.Status)
			detail = m.sessionDetails(lead, false)
		}
		rest := status
		if len(row.sessions) > 1 {
			rest += " (" + strconv.Itoa(len(row.sessions)) + " sessions)"
		}
		if len(detail) > 0 {
			rest += " · " + strings.Join(detail, " · ")
		}
		mark := "▸ "
		if m.expanded[row.id] {
			mark = "▾ "
		}
		// The badge and the name come first and are cut last, so a long task
		// never pushes who it is about off the line.
		name := fitCells(row.name, min(24, max(1, cols-cellWidth(mark)-cellWidth(agentBadge)-4)))
		room := cols - 2 - cellWidth(mark) - cellWidth(agentBadge) - 1 - cellWidth(name) - 2
		line := prefix + mark + badgeStyle.Render(agentBadge) + " " + name
		if room > 0 {
			line += "  " + fitCells(rest, room)
		}
		lines = append(lines, line)
		if !m.expanded[row.id] {
			continue
		}
		if len(row.sessions) == 0 {
			lines = append(lines, "    "+statusStyle.Render(fitCells("no sessions", cols-4)))
		}
		for _, s := range row.sessions {
			text := "#" + strconv.FormatInt(s.ID, 10) + " " + statusWord(s.Status)
			if d := m.sessionDetails(s, true); len(d) > 0 {
				text += " · " + strings.Join(d, " · ")
			}
			lines = append(lines, "    "+fitCells(text, cols-4))
		}
	}
	return lines, selLine
}

// agentsWindow is the part of lines that is shown in rows lines and keeps the
// line sel on screen.
func agentsWindow(lines []string, sel, rows int) []string {
	if rows < 1 {
		return nil
	}
	if len(lines) <= rows {
		return lines
	}
	start := 0
	if sel >= rows {
		start = min(sel-rows+1, len(lines)-rows)
	}
	return lines[start : start+rows]
}

// selectedAgent is the index in rows of the selected agent: the one the user
// chose, kept by principal id, or the first when that one is gone.
func (m Model) selectedAgent(rows []agentRow) int {
	for i, row := range rows {
		if row.id == m.selAgent {
			return i
		}
	}
	return 0
}

// moveAgent moves the selection by delta agents, stopping at the ends.
func (m Model) moveAgent(delta int) Model {
	rows := m.agentRows()
	if len(rows) == 0 {
		return m
	}
	next := min(max(m.selectedAgent(rows)+delta, 0), len(rows)-1)
	m.selAgent = rows[next].id
	return m
}

// toggleAgent expands or folds the selected agent's sessions.
func (m Model) toggleAgent() Model {
	rows := m.agentRows()
	if len(rows) == 0 {
		return m
	}
	id := rows[m.selectedAgent(rows)].id
	// The map is the model's own: a copy of the model shares it, which is
	// fine for a value only this view reads.
	if m.expanded[id] {
		delete(m.expanded, id)
	} else {
		m.expanded[id] = true
	}
	m.selAgent = id
	return m
}
