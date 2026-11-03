package tui

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The community member view (ADR-013 decision 3, issue #995): everyone on the
// server, from GET /v1/directory/members, with agents marked and roles shown.
// It lists no guests: the server sends none. To an operator each person's row
// carries the voice switch, which v flips.

// communityPageLimit is how many members one request asks for, and
// maxCommunityPages bounds the pages followed so a cursor that never ends
// cannot keep the TUI reading.
const (
	communityPageLimit = schema.MaxListPrincipalsLimit
	maxCommunityPages  = 100
)

// communityView is the member view.
type communityView struct {
	members         []schema.ServerMemberV1
	loaded, loading bool
	busy            bool
	sel             int64
	note            string
}

// communityLoaded is the community, every page of it.
type communityLoaded struct {
	members []schema.ServerMemberV1
	err     error
}

// voiceSwitched is the answer to an operator's voice switch.
type voiceSwitched struct {
	id   int64
	resp schema.VoicePermissionResponseV1
	err  error
}

var errCommunityPages = errors.New("the server's pages do not end")

func (m Model) enterCommunity() (tea.Model, tea.Cmd) {
	if m.comm == nil {
		m.setStatus(modeChannels, statusNoCommunity)
		return m, nil
	}
	m.mode = modeCommunity
	m.community.note = ""
	m.community.loading = true
	return m, m.loadCommunity()
}

// loadCommunity reads the community, following the server's pages to the end.
func (m Model) loadCommunity() tea.Cmd {
	api, ctx := m.comm, m.ctx
	return func() tea.Msg {
		var all []schema.ServerMemberV1
		var after int64
		for page := 0; ; page++ {
			if page == maxCommunityPages {
				return communityLoaded{err: errCommunityPages}
			}
			resp, err := api.DirectoryMembers(ctx, after, communityPageLimit)
			if err != nil {
				return communityLoaded{err: err}
			}
			all = append(all, resp.Members...)
			if resp.After == 0 {
				return communityLoaded{members: all}
			}
			if resp.After <= after {
				return communityLoaded{err: errCommunityPages}
			}
			after = resp.After
		}
	}
}

func (m Model) communityLoaded(msg communityLoaded) (tea.Model, tea.Cmd) {
	p := m.community
	p.loading = false
	if msg.err != nil {
		if line := m.communityError(cli.CommunityOpMembers, msg.err); line != "" {
			p.note = line
		}
		m.community = p
		return m, nil
	}
	p.members, p.loaded = msg.members, true
	if p.selected() < 0 && len(p.members) > 0 {
		p.sel = p.members[0].ID
	}
	m.community = p
	return m, nil
}

func (p communityView) selected() int {
	for i, mem := range p.members {
		if mem.ID == p.sel {
			return i
		}
	}
	return -1
}

func (m Model) moveCommunity(delta int) Model {
	p := m.community
	if len(p.members) == 0 {
		return m
	}
	i := max(p.selected(), 0)
	p.sel = p.members[min(max(i+delta, 0), len(p.members)-1)].ID
	m.community = p
	return m
}

// canManageMembers reports whether whoami says the caller may manage members,
// as the web's canDo does: an owner always may; a server that sends no
// permissions leaves it to the role; otherwise the permission decides. It only
// decides what is offered: the server decides every request.
func (m Model) canManageMembers() bool {
	switch {
	case m.role == schema.RoleOperator && m.owner:
		return true
	case !m.permsKnown:
		return m.role == schema.RoleOperator
	}
	return slices.Contains(m.permissions, schema.PermManageMembers)
}

// communityKey handles every key in the member view.
func (m Model) communityKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if !m.community.busy {
		m.community.note = ""
	}
	switch key {
	case "esc", "tab", "shift+tab":
		m.mode = modeChannels
		return m, m.markReading()
	case "up":
		return m.moveCommunity(-1), nil
	case "down":
		return m.moveCommunity(1), nil
	case "pgup":
		return m.moveCommunity(-max(1, m.layout().contentH-1)), nil
	case "pgdown":
		return m.moveCommunity(max(1, m.layout().contentH-1)), nil
	case "r":
		m.community.loading = true
		return m, m.loadCommunity()
	case "v":
		return m.switchVoice()
	}
	return m, nil
}

// switchVoice flips the voice of the selected person. It is offered to
// operators on a person's row only; the server still decides, and asks for a
// password proof, an owner's protection and the rest, in its own refusals.
func (m Model) switchVoice() (tea.Model, tea.Cmd) {
	p := m.community
	i := p.selected()
	if i < 0 {
		return m, nil
	}
	row := p.members[i]
	switch {
	case !m.canManageMembers():
		m.community.note = "only an operator who manages members can switch a person's voice"
		return m, nil
	case row.Kind == schema.PrincipalAgent:
		m.community.note = "agents do not use voice: only a person has a voice switch"
		return m, nil
	case row.VoiceAllowed == nil:
		m.community.note = "the server did not say whether " + memberName(row) + " has voice"
		return m, nil
	case p.busy:
		return m, nil
	}
	allowed := !*row.VoiceAllowed
	m.community.busy = true
	m.community.note = "switching voice…"
	api, ctx, id := m.comm, m.ctx, row.ID
	return m, func() tea.Msg {
		resp, err := api.SetVoicePermission(ctx, id, allowed)
		return voiceSwitched{id: id, resp: resp, err: err}
	}
}

func (m Model) voiceSwitched(msg voiceSwitched) (tea.Model, tea.Cmd) {
	p := m.community
	p.busy = false
	if msg.err != nil {
		if line := m.communityError(cli.CommunityOpVoice, msg.err); line != "" {
			p.note = line
		}
		m.community = p
		if cli.IsServerCode(msg.err, "forbidden") && m.authenticated {
			// Not (or no longer) allowed: ask who we are now, so the switch
			// stops being offered.
			return m, m.loadWhoAmI()
		}
		return m, nil
	}
	p.members = append([]schema.ServerMemberV1(nil), p.members...)
	name := "that person"
	for i := range p.members {
		if p.members[i].ID == msg.id {
			allowed := msg.resp.VoiceAllowed
			p.members[i].VoiceAllowed = &allowed
			name = memberName(p.members[i])
		}
	}
	state := "off"
	if msg.resp.VoiceAllowed {
		state = "on"
	}
	if msg.resp.Changed {
		p.note = "voice is now " + state + " for " + name
	} else {
		p.note = "voice was already " + state + " for " + name + "; nothing changed"
	}
	m.community = p
	return m, nil
}

func memberName(r schema.ServerMemberV1) string {
	if name := safeName(r.Name); name != "" {
		return name
	}
	return "#" + strconv.FormatInt(r.ID, 10)
}

// memberLine is one person: the name, then what is true of them. Agents are
// marked [agent]; a human shows the role; an operator also sees owner and the
// voice switch. Words only, so nothing rests on colour.
func (m Model) memberLine(r schema.ServerMemberV1, selected bool, cols int) string {
	var marks []string
	if r.Kind == schema.PrincipalAgent {
		marks = append(marks, "agent")
	} else if r.Role != "" && r.Role.Valid() {
		marks = append(marks, string(r.Role))
	}
	if r.Owner {
		marks = append(marks, "owner")
	}
	if m.canManageMembers() && r.VoiceAllowed != nil {
		if *r.VoiceAllowed {
			marks = append(marks, "voice on")
		} else {
			marks = append(marks, "voice off")
		}
	}
	tail := ""
	if len(marks) > 0 {
		tail = " " + badgeStyle.Render("["+strings.Join(marks, " · ")+"]")
	}
	room := max(1, cols-2-cellWidth(tail))
	name := fitCells(memberName(r), room)
	prefix := "  "
	if selected {
		prefix, name = activeStyle.Render("› "), activeStyle.Render(name)
	}
	return prefix + name + tail
}

func (m Model) communityPanes(l layout) string {
	cols := l.width - 2
	var lines []string
	switch {
	case m.community.loading && !m.community.loaded:
		lines = []string{statusStyle.Render("loading the community…")}
	case len(m.community.members) == 0:
		lines = []string{statusStyle.Render(fitCells("Nobody to show.", cols))}
	default:
		sel := m.community.selected()
		for i, r := range m.community.members {
			lines = append(lines, m.memberLine(r, i == sel, cols))
		}
		lines = agentsWindow(lines, max(sel, 0), l.contentH)
	}
	return paneBox("COMMUNITY", true, lines, l.width, l.contentH, "")
}

func (m Model) communityStatus() string {
	if m.community.note != "" {
		return m.community.note
	}
	return "community (" + strconv.Itoa(len(m.community.members)) + ")"
}
