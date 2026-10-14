package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/pkg/schema"
)

// agentModel is a model in the Agents view of "general" with a live roster
// and the given agents as members.
func agentModel(t *testing.T, members []schema.ChannelMemberV1, sessions ...schema.SessionV1) Model {
	t.Helper()
	m, _, _ := sessionModel(t)
	byID := map[int64]schema.ChannelMemberV1{}
	for _, member := range members {
		byID[member.PrincipalID] = member
	}
	m.members["general"] = byID
	m = frame(m, rosterOf(sessions...))
	m.mode = modeAgents
	m.width, m.height = 100, 30
	return m
}

func agentMember(id int64, name string) schema.ChannelMemberV1 {
	return schema.ChannelMemberV1{PrincipalID: id, Name: name, Kind: schema.PrincipalAgent}
}

func TestAgentLines(t *testing.T) {
	running := sess(1, 7, schema.PrincipalAgent, "worker-test", schema.SessionStatusRunning)
	running.Runtime, running.Model = "claude-code", "opus"
	running.CurrentRun, running.Task = 41, "refactor the parser"
	stale := sess(2, 8, schema.PrincipalAgent, "stale-test", schema.SessionStatusStale)
	stale.CurrentRun, stale.Task, stale.OutcomeUnknown = 42, "wait for CI", true
	offline := sess(3, 9, schema.PrincipalAgent, "gone-test", schema.SessionStatusOffline)
	offline.OutcomeUnknown = true
	offline.CurrentRun, offline.Task = 43, "deploy"
	// Every agent status of the vocabulary, one agent each.
	var all []schema.SessionV1
	var allWant []string
	for i, status := range []schema.SessionStatus{
		schema.SessionStatusIdle, schema.SessionStatusRunning, schema.SessionStatusBlocked, schema.SessionStatusWaiting,
		schema.SessionStatusFailed, schema.SessionStatusStale, schema.SessionStatusOffline,
	} {
		all = append(all, sess(int64(i+1), int64(100+i), schema.PrincipalAgent, "a"+string(status), status))
		allWant = append(allWant, "[agent] a"+string(status)+"  "+string(status))
	}

	tests := []struct {
		name     string
		members  []schema.ChannelMemberV1
		sessions []schema.SessionV1
		want     []string // substrings, one per line, in order
	}{
		{"every status", nil, all, allWant},
		{"detail of a running agent", nil, []schema.SessionV1{running}, []string{
			"[agent] worker-test  running · task: refactor the parser · 3m ago"}},
		{"an agent with no session is offline", []schema.ChannelMemberV1{agentMember(7, "idle-member")}, nil, []string{
			"[agent] idle-member  offline"}},
		{"members first, then agents only the roster knows", []schema.ChannelMemberV1{agentMember(9, "member-agent")},
			[]schema.SessionV1{running}, []string{"[agent] member-agent  offline", "[agent] worker-test  running"}},
		{"a human member is not an agent", []schema.ChannelMemberV1{{PrincipalID: 4, Name: "nick", Kind: schema.PrincipalHuman}}, nil, []string{
			"No agents in this channel"}},
		{"a stale session with an unknown outcome", nil, []schema.SessionV1{stale}, []string{
			"[agent] stale-test  stale · outcome unknown · task: wait for CI · 3m ago"}},
		{"an offline session with an unknown outcome", nil, []schema.SessionV1{offline}, []string{
			"[agent] gone-test  offline · outcome unknown · task: deploy"}},
		{"two sessions show the one that needs a person", nil, []schema.SessionV1{
			sess(1, 7, schema.PrincipalAgent, "twin", schema.SessionStatusIdle),
			sess(2, 7, schema.PrincipalAgent, "twin", schema.SessionStatusBlocked),
		}, []string{"[agent] twin  blocked (2 sessions)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := agentModel(t, tt.members, tt.sessions...)
			lines, _ := m.agentLines(98)
			if len(lines) != len(tt.want) {
				t.Fatalf("lines = %q, want %d", lines, len(tt.want))
			}
			for i, want := range tt.want {
				if !strings.Contains(lines[i], want) {
					t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestAgentsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		prep func(*Model)
		want string
	}{
		{"socket dropped", func(m *Model) { *m, _ = update(*m, eventsEnded{gen: m.ev.gen, err: errNoSessions}) }, "agents: unavailable"},
		{"waiting for the first roster", func(m *Model) { m.ev = eventsModel{gen: m.ev.gen} }, "agents: connecting…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := agentModel(t, []schema.ChannelMemberV1{agentMember(7, "worker")},
				sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning))
			tt.prep(&m)
			lines, _ := m.agentLines(60)
			if len(lines) != 1 || !strings.Contains(lines[0], tt.want) {
				t.Errorf("lines = %q, want %q alone: no agent is listed from a roster that may be stale", lines, tt.want)
			}
			if strings.Contains(m.View(), "worker") {
				t.Errorf("the view lists an agent while the roster is unavailable:\n%s", m.View())
			}
		})
	}
}

func TestAgentsViewKeys(t *testing.T) {
	a := sess(1, 7, schema.PrincipalAgent, "alpha", schema.SessionStatusRunning)
	a.Runtime = "rt"
	a.CurrentRun, a.Task = 1, "task a"
	b1 := sess(2, 8, schema.PrincipalAgent, "beta", schema.SessionStatusIdle)
	b2 := sess(3, 8, schema.PrincipalAgent, "beta", schema.SessionStatusOffline)
	m := agentModel(t, []schema.ChannelMemberV1{agentMember(7, "alpha"), agentMember(8, "beta"), agentMember(9, "gamma")}, a, b1, b2)
	press := func(k tea.KeyType) { m, _ = update(m, tea.KeyMsg{Type: k}) }
	selected := func() string {
		rows := m.agentRows()
		return rows[m.selectedAgent(rows)].name
	}
	count := func() int { l, _ := m.agentLines(98); return len(l) }

	if selected() != "alpha" || count() != 3 {
		t.Fatalf("start: selected %q, %d lines", selected(), count())
	}
	press(tea.KeyDown)
	if selected() != "beta" {
		t.Errorf("down: selected %q, want beta", selected())
	}
	press(tea.KeyEnter) // expand beta
	if count() != 5 {
		t.Errorf("expanded beta: %d lines, want 3 agents and 2 sessions", count())
	}
	lines, _ := m.agentLines(98)
	if !strings.Contains(lines[2], "#2 idle") || !strings.Contains(lines[3], "#3 offline") {
		t.Errorf("session lines = %q", lines[2:4])
	}
	// A roster refresh does not move the selection or fold what is open.
	m = frame(m, rosterOf(a, b1, b2, sess(4, 5, schema.PrincipalHuman, "alice", schema.SessionStatusOnline)))
	if selected() != "beta" || count() != 5 {
		t.Errorf("after a roster: selected %q, %d lines", selected(), count())
	}
	press(tea.KeyEnter) // fold
	if count() != 3 {
		t.Errorf("folded: %d lines, want 3", count())
	}
	press(tea.KeyDown)
	press(tea.KeyDown)
	if selected() != "gamma" {
		t.Errorf("selected %q, want gamma (the end of the list)", selected())
	}
	press(tea.KeyUp)
	press(tea.KeyUp)
	press(tea.KeyUp)
	if selected() != "alpha" {
		t.Errorf("selected %q, want alpha (the start of the list)", selected())
	}
	// Typing, backspace and the mouse leave the channel's draft alone.
	m.input = "draft"
	press(tea.KeyBackspace)
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("xyz")})
	m, _ = update(m, tea.MouseMsg{X: 5, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.input != "draft" {
		t.Errorf("input = %q, want the draft untouched", m.input)
	}
}

func TestTabCycle(t *testing.T) {
	m, _, _ := sessionModel(t)
	var seen []mode
	var cmds []tea.Cmd
	for range 4 {
		var cmd tea.Cmd
		m, cmd = update(m, tea.KeyMsg{Type: tea.KeyTab})
		seen = append(seen, m.mode)
		cmds = append(cmds, cmd)
	}
	want := []mode{modeInbox, modeAgents, modeChannels, modeInbox}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("modes after tab = %v, want %v", seen, want)
		}
	}
	if cmds[1] != nil {
		t.Error("entering the Agents view started a request: it reads the socket's roster")
	}
	m.mode = modeAgents
	if got := m.status(); got != "agents: general" {
		t.Errorf("status = %q, want the mode and channel named", got)
	}
	m.width, m.height = 100, 30
	if view := m.View(); !strings.Contains(view, "agents: general") || !strings.Contains(view, "enter sessions") {
		t.Errorf("status line does not name the mode:\n%s", view)
	}
}

func TestLayoutSizes(t *testing.T) {
	a := sess(1, 7, schema.PrincipalAgent, "worker-test", schema.SessionStatusRunning)
	a.CurrentRun, a.Task = 1, "a task"
	tests := []struct {
		name          string
		width, height int
		mode          mode
		tooSmall      bool
		channelList   bool // the channel list is drawn beside the content
	}{
		{"agents full 80x24", 80, 24, modeAgents, false, true},
		{"agents full 120x40", 120, 40, modeAgents, false, true},
		{"agents one pane, 79 wide", 79, 24, modeAgents, false, false},
		{"agents one pane, 23 high", 80, 23, modeAgents, false, false},
		{"agents one pane at the smallest", 40, 10, modeAgents, false, false},
		{"agents too narrow", 39, 24, modeAgents, true, false},
		{"agents too short", 80, 9, modeAgents, true, false},
		{"channels full", 80, 24, modeChannels, false, true},
		{"channels at the smallest", 40, 10, modeChannels, false, true},
		{"channels too narrow", 39, 24, modeChannels, true, false},
		{"channels too short", 80, 9, modeChannels, true, false},
		{"inbox too narrow", 39, 24, modeInbox, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := agentModel(t, []schema.ChannelMemberV1{agentMember(7, "worker-test")}, a)
			m.mode = tt.mode
			m.width, m.height = tt.width, tt.height
			view := m.View()
			lines := strings.Split(view, "\n")
			if tooSmall := strings.Contains(view, "terminal too small"); tooSmall != tt.tooSmall {
				t.Fatalf("terminal too small = %v, want %v:\n%s", tooSmall, tt.tooSmall, view)
			}
			if len(lines) != tt.height {
				t.Errorf("view has %d lines, want exactly %d", len(lines), tt.height)
			}
			for i, line := range lines {
				if w := cellWidth(line); w > tt.width {
					t.Errorf("line %d is %d cells, want at most %d: %q", i, w, tt.width, line)
				}
			}
			if tt.tooSmall {
				return
			}
			// "ops" is the other channel: it is on screen when the list is.
			if has := strings.Contains(view, "ops"); has != tt.channelList {
				t.Errorf("channel list shown = %v, want %v:\n%s", has, tt.channelList, view)
			}
			switch tt.mode {
			case modeAgents:
				if !strings.Contains(view, "worker-test") || !strings.Contains(view, "running") {
					t.Errorf("the agent is not shown with its status:\n%s", view)
				}
			case modeChannels:
				if !strings.Contains(view, "online:") {
					t.Errorf("the members line is not shown:\n%s", view)
				}
			}
		})
	}
}

func TestAgo(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "just now"}, {4 * time.Second, "just now"}, {5 * time.Second, "5s ago"}, {59 * time.Second, "59s ago"},
		{time.Minute, "1m ago"}, {59 * time.Minute, "59m ago"}, {time.Hour, "1h ago"}, {47 * time.Hour, "47h ago"},
		{48 * time.Hour, "2d ago"}, {-time.Minute, "just now"},
	}
	for _, tt := range tests {
		if got := ago(testNow, testNow.Add(-tt.d)); got != tt.want {
			t.Errorf("ago(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
