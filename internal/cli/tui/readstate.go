package tui

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// markInterval is the least time between two requests of the same kind for the
// same channel: a read mark moves at most this often, and a message in another
// channel asks for the counts at most this often.
const markInterval = 2 * time.Second

// readState is what the TUI knows of the person's read state (UX-02). The
// server keeps one mark per person, shared by all their clients; the counts
// here are its last answer plus a local +1 for each message seen since.
type readState struct {
	// off is set when the server says it holds no read state for this
	// credential (an agent, or authentication off). From then on the TUI asks
	// for nothing, shows nothing and says nothing.
	off bool
	// unread is each channel's count by name; a channel at 0 has no entry.
	unread map[string]int
	// names maps the ids read state is keyed by to channel names.
	names map[int64]string
	// marked is the newest message id a mark was requested for, per channel.
	// A request that fails is not repeated until a newer message is shown.
	marked map[string]int64
	// acked is the mark the server last confirmed, per channel. A read the
	// server served before that is older than the person's own reading and
	// must not bring a count back.
	acked    map[string]int64
	markedAt map[string]time.Time
	// pending is the newest id waiting for the interval to pass, and timer says
	// the markDue that will send it is in flight.
	pending map[string]int64
	timer   map[string]bool
	// readAt is when counts were last asked for because of a message elsewhere.
	readAt time.Time
	// dropped marks channels whose message socket dropped: the counts are read
	// again once it is back.
	dropped map[string]bool
	// after schedules a markDue. It is its own field, not the model's after,
	// which the message stream's retry timers share: a test that counts those
	// must not see these.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
}

func newReadState() readState {
	return readState{
		unread: make(map[string]int), names: make(map[int64]string),
		marked: make(map[string]int64), acked: make(map[string]int64), markedAt: make(map[string]time.Time),
		pending: make(map[string]int64), timer: make(map[string]bool),
		dropped: make(map[string]bool), after: tickAfter,
	}
}

// scrolled scrolls the message pane and, when that brings the newest message
// back into view, reads it.
func (m Model) scrolled(lines int) (tea.Model, tea.Cmd) {
	was := m.scroll
	m = m.scrollMessages(lines)
	if was > 0 && m.scroll == 0 {
		return m, m.markReading()
	}
	return m, nil
}

// readStateLoaded is one read of the counts. names holds every id it could
// resolve.
type readStateLoaded struct {
	states []schema.ReadStateV1
	names  map[int64]string
	err    error
}

// readMarked is the answer to one mark request.
type readMarked struct {
	channel string
	state   schema.ReadStateV1
	err     error
}

// markDue fires when the interval that held a mark back has passed.
type markDue struct{ channel string }

// refusesReadState reports whether err is the server saying it has no read
// state for this credential.
func refusesReadState(err error) bool {
	var serverErr *cli.ServerError
	if !errors.As(err, &serverErr) {
		return false
	}
	return serverErr.Code == schema.ErrorCodeReadStateRequireAuth || serverErr.Code == schema.ErrorCodeHumanRequired
}

// loadReadState reads the counts. Read state is keyed by channel id; the
// channel list is only fetched when an id is not yet known.
func (m Model) loadReadState() tea.Cmd {
	if m.rs.off {
		return nil
	}
	api, ctx := m.api, m.ctx
	names := maps.Clone(m.rs.names)
	return func() tea.Msg {
		resp, err := api.ListReadState(ctx)
		if err != nil {
			return readStateLoaded{err: err}
		}
		states := resp.ReadState
		for _, state := range states {
			if _, ok := names[state.ChannelID]; ok {
				continue
			}
			list, err := api.ListChannels(ctx)
			if err != nil {
				// Keep what can still be named.
				states = slices.DeleteFunc(slices.Clone(states), func(s schema.ReadStateV1) bool {
					_, ok := names[s.ChannelID]
					return !ok
				})
				break
			}
			for _, channel := range list.Channels {
				names[channel.ID] = channel.Name
			}
			break
		}
		return readStateLoaded{states: states, names: names}
	}
}

// readStateLoaded replaces the counts with the server's. A failure that is not
// a refusal changes nothing and says nothing: the counts shown stay, and the
// next message elsewhere or reconnect reads again.
func (m Model) readStateLoaded(msg readStateLoaded) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if refusesReadState(msg.err) {
			m.rs.off = true
			clear(m.rs.unread)
		}
		return m, nil
	}
	maps.Copy(m.rs.names, msg.names)
	clear(m.rs.unread)
	for _, state := range msg.states {
		name := m.rs.names[state.ChannelID]
		if name != "" && state.Unread > 0 && state.LastReadID >= m.rs.acked[name] {
			m.rs.unread[name] = min(state.Unread, schema.MaxUnreadCount)
		}
	}
	// The server counted before it had our mark: what is on screen and already
	// marked is not unread.
	if m.reading() && m.rs.marked[m.current()] > 0 {
		delete(m.rs.unread, m.current())
	}
	return m, nil
}

// countArrival is a message streamed for channel: +1 locally, since the server
// is only asked again at the interval. A message in another channel also asks
// for the counts. Your own messages are not unread.
func (m *Model) countArrival(channel string, message schema.MessageV2) tea.Cmd {
	if m.rs.off || (m.authorID > 0 && message.AuthorID == m.authorID) {
		return nil
	}
	m.rs.unread[channel] = min(m.rs.unread[channel]+1, schema.MaxUnreadCount)
	now := m.now()
	if channel == m.current() || (!m.rs.readAt.IsZero() && now.Sub(m.rs.readAt) < markInterval) {
		return nil
	}
	m.rs.readAt = now
	return m.loadReadState()
}

// reading is true while the newest message of the selected channel is on the
// terminal's screen: the channels view, not scrolled up, big enough to draw.
func (m Model) reading() bool {
	if m.mode != modeChannels || m.scroll > 0 || m.current() == "" {
		return false
	}
	w, h := m.size()
	return w >= minWidth && h >= minHeight
}

// markReading clears the selected channel's count and asks for its mark to
// move when the newest message is on screen and the mark is behind it. It is
// called when a channel is opened (selected, or its page arrives) and when a
// message arrives. It is also called when the newest message comes back into view: scrolling down to
// it, and returning to the channels view.
func (m *Model) markReading() tea.Cmd {
	if m.rs.off || !m.reading() {
		return nil
	}
	channel := m.current()
	delete(m.rs.unread, channel)
	held := m.messages[channel]
	if len(held) == 0 {
		return nil
	}
	newest := held[len(held)-1].ID
	if newest <= m.rs.marked[channel] {
		return nil
	}
	now := m.now()
	last, sent := m.rs.markedAt[channel]
	if !sent || now.Sub(last) >= markInterval {
		return m.sendMark(channel, newest, now)
	}
	// Too soon: the newest id wins when the interval is over.
	m.rs.pending[channel] = newest
	if m.rs.timer[channel] {
		return nil
	}
	m.rs.timer[channel] = true
	return m.rs.after(last.Add(markInterval).Sub(now), markDue{channel: channel})
}

// sendMark asks the server to move channel's mark to id.
func (m *Model) sendMark(channel string, id int64, now time.Time) tea.Cmd {
	m.rs.marked[channel] = id
	m.rs.markedAt[channel] = now
	delete(m.rs.pending, channel)
	api, ctx := m.api, m.ctx
	return func() tea.Msg {
		state, err := api.MarkRead(ctx, channel, id)
		return readMarked{channel: channel, state: state, err: err}
	}
}

// markDue sends what the interval held back.
func (m Model) markDue(msg markDue) (tea.Model, tea.Cmd) {
	delete(m.rs.timer, msg.channel)
	if !m.socketUp(msg.channel) {
		// The server is not answering: nothing is sent on a timer of ours.
		// The mark waits in pending until the socket is back (flushMark).
		return m, nil
	}
	return m, m.flushMark(msg.channel)
}

// socketUp is true while channel's message socket is open or opening, that is
// not waiting out a retry after a drop.
func (m Model) socketUp(channel string) bool {
	return m.subscribed[channel] && !m.retryPending[channel]
}

// flushMark sends the mark that was held back for channel, if any.
func (m *Model) flushMark(channel string) tea.Cmd {
	id := m.rs.pending[channel]
	if m.rs.off || id <= m.rs.marked[channel] {
		return nil
	}
	return m.sendMark(channel, id, m.now())
}

// readMarked takes the server's count after a mark as the channel's count.
func (m Model) readMarked(msg readMarked) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if refusesReadState(msg.err) {
			m.rs.off = true
			clear(m.rs.unread)
		}
		return m, nil
	}
	if m.rs.off {
		return m, nil
	}
	if msg.state.LastReadID > m.rs.acked[msg.channel] {
		m.rs.acked[msg.channel] = msg.state.LastReadID
	}
	if msg.channel == m.current() && m.reading() && m.rs.pending[msg.channel] > msg.state.LastReadID {
		// A newer mark is waiting: this count is already out of date.
		return m, nil
	}
	if msg.state.Unread > 0 {
		m.rs.unread[msg.channel] = min(msg.state.Unread, schema.MaxUnreadCount)
	} else {
		delete(m.rs.unread, msg.channel)
	}
	return m, nil
}

// unreadBadge is the count shown after a channel's name, "" for none.
func (m Model) unreadBadge(channel string) string {
	n := m.rs.unread[channel]
	switch {
	case m.rs.off || n <= 0:
		return ""
	case n >= schema.MaxUnreadCount:
		return strconv.Itoa(schema.MaxUnreadCount-1) + "+"
	}
	return strconv.Itoa(n)
}
