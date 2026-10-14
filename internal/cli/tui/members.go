package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// sessionClient is the name the TUI gives its own session: what a roster
// reader sees as the program behind it.
const sessionClient = "tui"

// endSessionTimeout bounds the DELETE of the TUI's own session on the way
// out. The caller's context has usually ended by then (that is how a signal
// stops the program), so the call has its own, short one.
const endSessionTimeout = 2 * time.Second

// ownSession is the TUI's own session, shared by every copy of the model. It
// lives outside the model because it must be ended by Run after the program
// is gone, and because the call that creates it can finish after the quit.
type ownSession struct {
	mu   sync.Mutex
	id   int64
	done bool
}

// set records the created session. It reports false when the TUI has already
// quit, in which case the caller owns ending the session.
func (o *ownSession) set(id int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return false
	}
	o.id = id
	return true
}

// finish marks the TUI as quit and returns the session to end, 0 for none.
func (o *ownSession) finish() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done = true
	id := o.id
	o.id = 0
	return id
}

// endSession ends session id with a context of its own. A failure is not
// reported: the terminal is already given back, and the server ends a session
// nobody keeps alive by itself.
func endSession(api API, id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), endSessionTimeout)
	defer cancel()
	_ = api.EndSession(ctx, id)
}

// endOwnSession ends the TUI's own session, once, and stops a session that is
// still being created from being kept. Run calls it after the program has
// returned, whether the user quit or a signal ended the context.
func (m Model) endOwnSession() {
	if id := m.own.finish(); id > 0 {
		endSession(m.api, id)
	}
}

// sessionStarted is the result of creating the TUI's own session.
type sessionStarted struct {
	id  int64
	err error
}

// errNoSession is what a server that answers a session without an id gets.
var errNoSession = errors.New("the server returned no session")

// startSession creates the TUI's own session. Everything else works without
// it: a failure only leaves the roster unavailable.
func (m Model) startSession() tea.Cmd {
	api, own, ctx := m.api, m.own, m.ctx
	return func() tea.Msg {
		doc, err := api.CreateSession(ctx, schema.CreateSessionRequestV1{Client: sessionClient})
		if err == nil && doc.ID <= 0 {
			err = errNoSession
		}
		if err != nil {
			return sessionStarted{err: err}
		}
		if !own.set(doc.ID) {
			// The program quit while the session was being created. This
			// runs on the command's goroutine, which cmd/conch does not wait
			// for: the process may exit first, and then the server's
			// staleness ends the session instead.
			endSession(api, doc.ID)
			return sessionStarted{err: context.Canceled}
		}
		return sessionStarted{id: doc.ID}
	}
}

// eventsState is what the TUI knows about the events socket of the channel on
// screen.
type eventsState int

const (
	// eventsConnecting is the state before the first roster of this socket,
	// and the zero value: nothing has been said yet, so nothing is shown.
	eventsConnecting eventsState = iota
	// eventsLive means roster is the latest the server sent.
	eventsLive
	// eventsDown means the socket dropped or was refused. The roster is not
	// trusted, and the views say so until a new one arrives.
	eventsDown
)

// eventsModel is the events socket of the selected channel and the roster it
// last sent. Only the newest socket's results are kept.
type eventsModel struct {
	gen    int
	cancel context.CancelFunc
	state  eventsState
	roster schema.SessionRosterV1
	// settled is set once the socket has said anything, a roster or a failure.
	// Until then the members line is not drawn, as the voice line is not
	// before its first document; after it the line keeps its row through
	// every reconnect and channel switch.
	settled bool
	// backoff is the delay the next drop will wait; zero means the minimum.
	backoff      time.Duration
	retryPending bool
}

// eventsRosterReceived carries one roster frame. gen tells which socket sent
// it, so a late frame from a channel the user has left is dropped.
type eventsRosterReceived struct {
	gen    int
	roster schema.SessionRosterV1
}

// eventsEnded is the end of one events socket.
type eventsEnded struct {
	gen   int
	err   error
	lived time.Duration
}

// eventsRetryDue fires when a dropped events socket's backoff delay elapses.
type eventsRetryDue struct{ gen int }

// beginEvents closes the running events socket, if any, and opens one for the
// selected channel with the TUI's own session. Switching channels always goes
// through here, so the old socket never outlives its channel and the old
// roster never stays on screen for the new one. Without a session there is
// nothing to keep online and the roster is simply unavailable.
func (m *Model) beginEvents() tea.Cmd {
	e := &m.ev
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.gen++
	e.roster = schema.SessionRosterV1{}
	e.backoff = 0
	e.retryPending = false
	channel := m.current()
	switch {
	case m.session == 0 && m.sessionFailed:
		e.state = eventsDown
		e.settled = true
		return nil
	case m.session == 0 || channel == "":
		// Still waiting for the session or the channel list.
		e.state = eventsConnecting
		return nil
	}
	e.state = eventsConnecting
	ctx, cancel := context.WithCancel(m.ctx)
	e.cancel = cancel
	return m.startEvents(ctx, e.gen, channel, m.session)
}

func (m Model) startEvents(ctx context.Context, gen int, channel string, session int64) tea.Cmd {
	api, events := m.api, m.events
	return func() tea.Msg {
		go func() {
			started := time.Now()
			err := api.SubscribeEvents(ctx, channel, session, func(roster schema.SessionRosterV1) error {
				select {
				case events <- eventsRosterReceived{gen: gen, roster: roster}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			select {
			case events <- eventsEnded{gen: gen, err: err, lived: time.Since(started)}:
			case <-ctx.Done():
			}
		}()
		return nil
	}
}

// sessionStarted keeps the id of the TUI's own session and opens the socket
// that keeps it online. A failure leaves everything else as it is.
func (m Model) sessionStarted(msg sessionStarted) (tea.Model, tea.Cmd) {
	if errors.Is(msg.err, context.Canceled) || m.ctx.Err() != nil {
		// A command's own result, not an event: no waitEvent, which would add
		// a second reader of the events channel.
		return m, nil
	}
	if msg.err != nil {
		m.sessionFailed = true
	} else {
		m.session = msg.id
	}
	return m, m.beginEvents()
}

// eventsRoster takes a roster frame. It touches the roster and nothing else:
// not the draft, not the selected channel, not a status line.
func (m Model) eventsRoster(msg eventsRosterReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen {
		return m, wait
	}
	// A frame is data flowing: the socket is healthy.
	m.ev.backoff = 0
	m.ev.roster = msg.roster
	m.ev.state = eventsLive
	m.ev.settled = true
	return m, wait
}

// eventsEnded decides what the end of an events socket means. The roster is
// dropped at once, because from here on it is only a guess. A refusal that
// retrying cannot change leaves it unavailable; anything else is retried
// after a backoff, on the same schedule as the message subscription.
func (m Model) eventsEnded(msg eventsEnded) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen || errors.Is(msg.err, context.Canceled) || m.ctx.Err() != nil {
		return m, wait
	}
	e := &m.ev
	e.state = eventsDown
	e.settled = true
	e.roster = schema.SessionRosterV1{}
	var serverErr *cli.ServerError
	switch {
	case errors.Is(msg.err, cli.ErrUnauthenticated):
		return m, wait
	case errors.As(msg.err, &serverErr) && serverErr.Status >= 400 && serverErr.Status < 500 && serverErr.Status != 429:
		// Not a member, no such channel or session: asking again gets the
		// same answer. Selecting a channel asks afresh.
		return m, wait
	}
	// A 1008 close is retried too: it is also what a reader too slow to keep
	// up gets, and a removal answers the next attempt with a 404 above.
	if msg.lived >= maxResubscribeDelay {
		e.backoff = 0
	}
	delay := e.backoff
	if delay <= 0 {
		delay = minResubscribeDelay
	}
	e.backoff = delay * 2
	if e.backoff > maxResubscribeDelay {
		e.backoff = maxResubscribeDelay
	}
	e.retryPending = true
	return m, tea.Batch(wait, m.after(delay, eventsRetryDue{gen: e.gen}))
}

func (m Model) eventsRetry(msg eventsRetryDue) (tea.Model, tea.Cmd) {
	if msg.gen != m.ev.gen || !m.ev.retryPending {
		return m, nil
	}
	backoff := m.ev.backoff
	cmd := m.beginEvents()
	// A retry is the same subscription continuing: it stays unavailable until
	// the new socket's first roster, and the backoff keeps growing.
	m.ev.state, m.ev.backoff = eventsDown, backoff
	return m, cmd
}

// rosterSessions is the sessions of the latest roster, nil unless the socket
// is live: a roster that may be stale is never read.
func (m Model) rosterSessions() []schema.SessionV1 {
	if m.ev.state != eventsLive {
		return nil
	}
	return m.ev.roster.Sessions
}

// sessionName is how a session's principal is shown.
func sessionName(s schema.SessionV1) string {
	if name := safeName(s.Name); name != "" {
		return name
	}
	return "#" + strconv.FormatInt(s.PrincipalID, 10)
}

// agentGlyph is the mark after an agent in the members line: ● for any live
// session, ◐ for stale, ○ for offline. The glyph is only a hint; the Agents
// view says the same in words.
func agentGlyph(state schema.SessionState) string {
	switch state {
	case schema.SessionStateOnline:
		return "●"
	case schema.SessionStateStale:
		return "◐"
	default:
		return "○"
	}
}

// membersLine is the one line of who is connected: humans with a live session,
// then agents with a session, each once. A principal with several live
// sessions carries their number. Members with no session are not listed. It
// never exceeds width cells, it reads "unavailable" whenever the roster could
// be stale, and it is "" until the events socket has first said anything.
func (m Model) membersLine(width int) string {
	switch m.ev.state {
	case eventsConnecting:
		if !m.ev.settled {
			return ""
		}
		return fitCells("members: connecting…", width)
	case eventsDown:
		return fitCells("members: unavailable", width)
	}
	type entry struct {
		name  string
		live  int
		state schema.SessionState
	}
	var humans, agents []*entry
	byPrincipal := map[int64]*entry{}
	for _, s := range m.rosterSessions() {
		e, ok := byPrincipal[s.PrincipalID]
		if !ok {
			e = &entry{name: sessionName(s), state: schema.SessionStateOffline}
			byPrincipal[s.PrincipalID] = e
			if s.Kind == schema.PrincipalAgent {
				agents = append(agents, e)
			} else {
				humans = append(humans, e)
			}
		}
		if s.State == schema.SessionStateOnline {
			e.live++
		}
		// The best liveness among the principal's sessions is the one shown.
		if rank := stateRank(s.State); rank > stateRank(e.state) {
			e.state = s.State
		}
	}
	var online []string
	for _, h := range humans {
		if h.live == 0 {
			continue
		}
		text := h.name
		if h.live > 1 {
			text += "(" + strconv.Itoa(h.live) + ")"
		}
		online = append(online, text)
	}
	line := "online: nobody"
	if len(online) > 0 {
		line = "online: " + strings.Join(online, " ")
	}
	if len(agents) > 0 {
		parts := make([]string, len(agents))
		for i, a := range agents {
			parts[i] = a.name
			if a.live > 1 {
				parts[i] += "(" + strconv.Itoa(a.live) + ")"
			}
			parts[i] += " " + agentGlyph(a.state)
		}
		line += " · agents: " + strings.Join(parts, " ")
	}
	return fitCells(line, width)
}

// stateRank orders liveness: online over stale over offline.
func stateRank(s schema.SessionState) int {
	switch s {
	case schema.SessionStateOnline:
		return 2
	case schema.SessionStateStale:
		return 1
	default:
		return 0
	}
}
