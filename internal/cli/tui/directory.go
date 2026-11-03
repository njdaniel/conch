package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The community model's two views (ADR-013, issue #995): the channel directory
// here, the community's members in community.go. Both are clients of the
// public routes through CommunityAPI; the server decides who may do what, and
// a refusal is one line in the words of cli.CommunityRefusal, chosen by its
// code and never the server's text.

// CommunityAPI is the part of the client the directory and the member view
// use. A client that lacks it still runs the TUI; the two views then say they
// are not available.
type CommunityAPI interface {
	ChannelDirectory(ctx context.Context, all bool) (schema.ChannelDirectoryResponseV1, error)
	JoinChannel(ctx context.Context, channel string) (schema.JoinChannelResponseV1, error)
	LeaveChannel(ctx context.Context, channel string) error
	CreateChannelV1(ctx context.Context, request schema.CreateChannelRequestV1) (schema.CreateChannelResponseV1, error)
	DirectoryMembers(ctx context.Context, after int64, limit int) (schema.ServerMembersResponseV1, error)
	SetVoicePermission(ctx context.Context, id int64, allowed bool) (schema.VoicePermissionResponseV1, error)
}

var _ CommunityAPI = (*cli.Client)(nil)

const statusNoCommunity = "this client cannot reach the channel directory"

// directoryView is the channel directory: public channels and the caller's
// private ones. It lists no counts.
type directoryView struct {
	rows            []schema.ChannelListingV1
	loaded, loading bool
	// busy is set while a join, leave or create is in flight, so a second key
	// cannot send it twice.
	busy bool
	// sel is the selected channel by id, so the selection survives a reload.
	sel  int64
	note string
	// creating is the create-channel prompt: name is what is typed and
	// private the visibility chosen (public by default).
	creating bool
	name     string
	private  bool
	// confirm is the id of a private channel the user asked to leave: leaving
	// it cannot be undone by joining again, so it is asked once.
	confirm int64
}

// directoryLoaded is the answer to a read of the directory.
type directoryLoaded struct {
	rows []schema.ChannelListingV1
	err  error
}

// channelActed is the answer to a join, a leave or a create.
type channelActed struct {
	op      string
	channel string
	err     error
}

// communityError is a failed community call as one line. A cancelled call
// says nothing.
func (m Model) communityError(op string, err error) string {
	if errors.Is(err, context.Canceled) || m.ctx.Err() != nil {
		return ""
	}
	return sanitize(cli.CommunityRefusal(op, err).Error())
}

// enterDirectory shows the directory and reads it again.
func (m Model) enterDirectory() (tea.Model, tea.Cmd) {
	if m.comm == nil {
		m.setStatus(modeChannels, statusNoCommunity)
		return m, nil
	}
	m.mode = modeDirectory
	m.dir.note, m.dir.creating, m.dir.confirm = "", false, 0
	m.dir.loading = true
	return m, m.loadDirectory()
}

func (m Model) loadDirectory() tea.Cmd {
	api, ctx := m.comm, m.ctx
	return func() tea.Msg {
		resp, err := api.ChannelDirectory(ctx, false)
		return directoryLoaded{rows: resp.Channels, err: err}
	}
}

func (m Model) directoryLoaded(msg directoryLoaded) (tea.Model, tea.Cmd) {
	d := m.dir
	d.loading = false
	if msg.err != nil {
		if line := m.communityError(cli.CommunityOpChannels, msg.err); line != "" {
			d.note = line
		}
		m.dir = d
		return m, nil
	}
	d.rows, d.loaded = msg.rows, true
	if d.selected() < 0 && len(d.rows) > 0 {
		d.sel = d.rows[0].ID
	}
	m.dir = d
	return m, nil
}

// selected is the index of the selected row, -1 for none.
func (d directoryView) selected() int {
	for i, r := range d.rows {
		if r.ID == d.sel {
			return i
		}
	}
	return -1
}

func (m Model) moveDirectory(delta int) Model {
	d := m.dir
	if len(d.rows) == 0 {
		return m
	}
	i := max(d.selected(), 0)
	d.sel = d.rows[min(max(i+delta, 0), len(d.rows)-1)].ID
	m.dir = d
	return m
}

// directoryKey handles every key in the directory. Quitting is ctrl+c; esc
// and tab go back to the chat.
func (m Model) directoryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	d := m.dir
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if d.creating {
		return m.createKey(msg)
	}
	confirm := d.confirm
	m.dir.confirm = 0
	m.dir.note = ""
	switch key {
	case "esc", "tab", "shift+tab":
		m.mode = modeChannels
		return m, m.markReading()
	case "up":
		return m.moveDirectory(-1), nil
	case "down":
		return m.moveDirectory(1), nil
	case "pgup":
		return m.moveDirectory(-max(1, m.layout().contentH-1)), nil
	case "pgdown":
		return m.moveDirectory(max(1, m.layout().contentH-1)), nil
	case "r":
		m.dir.loading = true
		return m, m.loadDirectory()
	case "n":
		m.dir.creating, m.dir.name, m.dir.private = true, "", false
		return m, nil
	}
	i := d.selected()
	if i < 0 {
		return m, nil
	}
	row := d.rows[i]
	switch key {
	case "enter", "j":
		if row.Joined {
			m.dir.note = "already in " + channelLabel(row.Name)
			return m, nil
		}
		return m.channelAction(cli.CommunityOpJoin, row.Name, "joining…")
	case "l":
		if !row.Joined {
			m.dir.note = "not in " + channelLabel(row.Name)
			return m, nil
		}
		if row.Visibility != schema.ChannelPublic {
			// Only y leaves a private channel; l again asks again.
			m.dir.confirm = row.ID
			m.dir.note = "leave private " + channelLabel(row.Name) + "? you cannot join it again yourself: y to leave, any other key to stay"
			return m, nil
		}
		return m.channelAction(cli.CommunityOpLeave, row.Name, "leaving…")
	case "y":
		if confirm == row.ID && row.ID != 0 {
			return m.channelAction(cli.CommunityOpLeave, row.Name, "leaving…")
		}
	}
	return m, nil
}

// createKey handles a key at the create-channel prompt.
func (m Model) createKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := &m.dir
	switch msg.String() {
	case "esc":
		d.creating, d.name = false, ""
	case "tab":
		d.private = !d.private
	case "backspace":
		if d.name != "" {
			_, size := utf8.DecodeLastRuneInString(d.name)
			d.name = d.name[:len(d.name)-size]
		}
	case "enter":
		visibility := schema.ChannelPublic
		if d.private {
			visibility = schema.ChannelPrivate
		}
		request := schema.CreateChannelRequestV1{Name: strings.TrimSpace(d.name), Visibility: visibility}
		// The check the server makes, so a name it would refuse costs no request.
		if err := request.Validate(); err != nil {
			d.note = "that name will not do: " + sanitize(err.Error())
			return m, nil
		}
		if d.busy {
			return m, nil
		}
		d.busy, d.note = true, "creating…"
		return m, m.createChannel(request)
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			d.name += string(msg.Runes)
		}
	}
	return m, nil
}

func (m Model) createChannel(request schema.CreateChannelRequestV1) tea.Cmd {
	api, ctx := m.comm, m.ctx
	return func() tea.Msg {
		resp, err := api.CreateChannelV1(ctx, request)
		return channelActed{op: cli.CommunityOpCreate, channel: resp.Channel.Name, err: err}
	}
}

// channelAction starts a join or a leave of the channel named name.
func (m Model) channelAction(op, name, doing string) (tea.Model, tea.Cmd) {
	if m.dir.busy {
		return m, nil
	}
	m.dir.busy, m.dir.note = true, doing
	api, ctx := m.comm, m.ctx
	return m, func() tea.Msg {
		if op == cli.CommunityOpJoin {
			resp, err := api.JoinChannel(ctx, name)
			if err == nil && resp.Channel.Name != "" {
				name = resp.Channel.Name
			}
			return channelActed{op: op, channel: name, err: err}
		}
		return channelActed{op: op, channel: name, err: api.LeaveChannel(ctx, name)}
	}
}

// channelActed takes the answer to a join, a leave or a create: one line, and
// the chat's channel list follows what the server did.
func (m Model) channelActed(msg channelActed) (tea.Model, tea.Cmd) {
	m.dir.busy = false
	if msg.err != nil {
		if line := m.communityError(msg.op, msg.err); line != "" {
			m.dir.note = line
		}
		return m, nil
	}
	label := channelLabel(msg.channel)
	reload := m.loadDirectory()
	switch msg.op {
	case cli.CommunityOpJoin, cli.CommunityOpCreate:
		if msg.op == cli.CommunityOpCreate {
			m.dir.creating, m.dir.name = false, ""
			m.dir.note = "created " + label
		} else {
			m.dir.note = "joined " + label
		}
		if msg.channel != "" && !slices.Contains(m.channels, msg.channel) {
			// Added to the end: the channel on screen does not move.
			m.channels = append(slices.Clone(m.channels), msg.channel)
		}
		return m, reload
	default:
		m.dir.note = "left " + label
		next, cmd := m.channelLeft(msg.channel)
		return next, tea.Batch(reload, cmd)
	}
}

// channelLeft drops a channel the person has left from the chat's list. When
// it was the one on screen the chat opens a neighbour; when it was the last,
// the list is read again.
func (m Model) channelLeft(name string) (Model, tea.Cmd) {
	i := slices.Index(m.channels, name)
	if i < 0 {
		return m, nil
	}
	m.channels = slices.Delete(slices.Clone(m.channels), i, i+1)
	delete(m.subscribed, name)
	switch {
	case len(m.channels) == 0:
		m.selected, m.loadingChannels, m.target, m.pendingNet = 0, true, nil, ""
		return m, m.loadChannels()
	case i < m.selected:
		m.selected--
	case i == m.selected:
		m.selected = min(i, len(m.channels)-1)
		return m.openSelected()
	}
	return m, nil
}

// channelLabel is a channel's name as the views write it.
func channelLabel(name string) string {
	if name = safeName(name); name == "" {
		return "that channel"
	}
	return "#" + name
}

// directoryLine is one channel: its name and what is true of it. The marks are
// words, so nothing rests on colour.
func directoryLine(r schema.ChannelListingV1, selected bool, cols int) string {
	var marks []string
	if r.Joined {
		marks = append(marks, "joined")
	}
	if r.Default {
		marks = append(marks, "default")
	}
	if r.Visibility != schema.ChannelPublic {
		marks = append(marks, "private")
	}
	if r.Manages {
		marks = append(marks, "managed")
	}
	tail := ""
	if len(marks) > 0 {
		tail = " " + badgeStyle.Render("["+strings.Join(marks, " ")+"]")
	}
	room := max(1, cols-2-cellWidth(tail))
	name := fitCells(channelLabel(r.Name), room)
	prefix := "  "
	if selected {
		prefix, name = activeStyle.Render("› "), activeStyle.Render(name)
	}
	return prefix + name + tail
}

// directoryPanes is the directory: one pane, as wide as the screen.
func (m Model) directoryPanes(l layout) string {
	cols := l.width - 2
	var lines []string
	switch {
	case m.dir.loading && !m.dir.loaded:
		lines = []string{statusStyle.Render("loading channels…")}
	case len(m.dir.rows) == 0:
		lines = []string{statusStyle.Render(fitCells("No channels to show. n creates one.", cols))}
	default:
		sel := m.dir.selected()
		for i, r := range m.dir.rows {
			lines = append(lines, directoryLine(r, i == sel, cols))
		}
		lines = agentsWindow(lines, max(sel, 0), l.contentH)
	}
	return paneBox("CHANNEL DIRECTORY", true, lines, l.width, l.contentH, "")
}

// directoryStatus is the status line of the directory.
func (m Model) directoryStatus() string {
	if m.dir.note != "" {
		return m.dir.note
	}
	return "channel directory (" + strconv.Itoa(len(m.dir.rows)) + ")"
}

// directoryPrompt is the input line of the directory: the create prompt while
// it is open.
func (m Model) directoryPrompt(width int) string {
	if !m.dir.creating {
		return ""
	}
	kind := "public"
	if m.dir.private {
		kind = "private"
	}
	prompt := "new " + kind + " channel (tab: public/private):"
	return prompt + " " + clipTail(sanitize(m.dir.name), max(1, width-cellWidth(prompt)-3)) + activeStyle.Render("▌")
}
