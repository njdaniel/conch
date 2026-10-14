package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// settle runs cmd the way the program would, answering the one waitEvent the
// update asks for with a message the model ignores, and feeds what the other
// commands return back into the model until nothing is left to run.
func settle(m Model, cmd tea.Cmd) Model {
	m.events <- eventsRetryDue{gen: -1}
	for _, msg := range run(cmd) {
		if _, ok := msg.(eventsRetryDue); ok {
			continue
		}
		var next tea.Cmd
		m, next = update(m, msg)
		if next != nil {
			m = settle(m, next)
		}
	}
	return m
}

// deliver takes the next message the events socket goroutines pushed, as the
// program's one reader of them would, and settles what it leads to.
func deliver(t *testing.T, m Model) Model {
	t.Helper()
	select {
	case msg := <-m.events:
		m, cmd := update(m, msg)
		return settle(m, cmd)
	case <-time.After(2 * time.Second):
		t.Fatal("no events message arrived")
		return m
	}
}

func awaitSocket(t *testing.T, a *sessionAPI, session int64) {
	t.Helper()
	select {
	case got := <-a.started:
		if got.session != session {
			t.Fatalf("events socket opened with session %d, want %d", got.session, session)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no events socket opened with session %d", session)
	}
}

func noSocket(t *testing.T, a *sessionAPI) {
	t.Helper()
	select {
	case got := <-a.started:
		t.Fatalf("an events socket was opened with session %d", got.session)
	case <-time.After(100 * time.Millisecond):
	}
}

// The server ends the TUI's session: the refused socket is answered with one
// new session, the socket reopens with its id, and the roster comes back.
func TestEventsSessionGoneRecreates(t *testing.T) {
	api := newSessionAPI()
	api.gone = map[int64]bool{9: true}
	api.ids = []int64{9, 10}
	m, api, timers := sessionModelWith(t, api)
	m = deliver(t, m)
	awaitSocket(t, api, 10)
	if len(api.created) != 2 || api.created[1].Client != "tui" {
		t.Errorf("created %+v, want a second session with client tui", api.created)
	}
	if m.session != 10 {
		t.Errorf("session = %d, want the new session 10", m.session)
	}
	if len(*timers) != 0 {
		t.Errorf("timers = %+v: the recreate is not a backoff retry", *timers)
	}
	if got := m.membersLine(80); got != "members: unavailable" {
		t.Errorf("before the new roster the line = %q, want unavailable", got)
	}
	m = frame(m, rosterOf(sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)))
	if got := m.membersLine(80); !strings.Contains(got, "worker") {
		t.Errorf("after the new roster the line = %q, want it back", got)
	}
	if len(api.ended) != 0 {
		t.Errorf("ended = %v: the session the server ended must not be ended again", api.ended)
	}
	// Quit ends the session the TUI holds, once, and not the old one.
	m.endOwnSession()
	m.endOwnSession()
	if len(api.ended) != 1 || api.ended[0] != 10 {
		t.Errorf("ended = %v, want session 10 once", api.ended)
	}
}

// A new session that is refused too costs one session, not a loop.
func TestEventsSessionGoneDoesNotLoop(t *testing.T) {
	api := newSessionAPI()
	api.gone = map[int64]bool{9: true, 10: true}
	api.ids = []int64{9, 10, 11}
	m, api, _ := sessionModelWith(t, api)
	m = deliver(t, m)
	awaitSocket(t, api, 10)
	m = deliver(t, m)
	noSocket(t, api)
	if len(api.created) != 2 {
		t.Errorf("created %d sessions, want 2: the second refusal must not create another", len(api.created))
	}
	if got := m.membersLine(80); got != "members: unavailable" {
		t.Errorf("line = %q, want unavailable", got)
	}
}

func TestEventsSessionGoneRecreateFails(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status string
	}{
		{"credential gone", &cli.ServerError{Status: 401, Code: "unauthorized", Message: "no"}, "members: session ended; sign in again"},
		{"not logged in", &cli.UnauthenticatedError{Server: "http://h:1"}, "members: session ended; sign in again"},
		{"too many", &cli.ServerError{Status: 429, Code: schema.ErrorCodeTooManySessions, Message: "no"}, "members: session ended; too many sessions"},
		{"network", errors.New("connection refused"), "members: session ended; could not start a new one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSessionAPI()
			api.gone = map[int64]bool{9: true}
			m, api, timers := sessionModelWith(t, api)
			api.mu.Lock()
			api.createErr = tt.err
			api.mu.Unlock()
			m = deliver(t, m)
			noSocket(t, api)
			if len(api.created) != 2 {
				t.Errorf("created %d sessions, want 2: one at start, one recreate attempt", len(api.created))
			}
			if got := m.status(); got != tt.status {
				t.Errorf("status = %q, want %q", got, tt.status)
			}
			if got := m.membersLine(80); got != "members: unavailable" {
				t.Errorf("members line = %q, want unavailable", got)
			}
			if got := strings.Join(plainLines(m.agentLines(60)), "|"); !strings.Contains(got, "unavailable") {
				t.Errorf("agents view = %q, want unavailable", got)
			}
			if len(*timers) != 0 {
				t.Errorf("timers = %+v: a failed recreate must not retry", *timers)
			}
			// Switching channels asks the server for nothing more.
			m, cmd := update(m, tea.KeyMsg{Type: tea.KeyDown})
			m = settle(m, cmd)
			noSocket(t, api)
			if len(api.created) != 2 || m.membersLine(80) != "members: unavailable" {
				t.Errorf("after a channel switch: %d sessions created, line %q", len(api.created), m.membersLine(80))
			}
			// The old session is already ended and there is no new one.
			m.endOwnSession()
			if len(api.ended) != 0 {
				t.Errorf("ended = %v, want nothing", api.ended)
			}
		})
	}
}

// The program quits while the new session is being created: the call that made
// it ends it, and the one the server ended is not touched.
func TestEventsSessionGoneCreatedAfterQuit(t *testing.T) {
	api := newSessionAPI()
	api.gone = map[int64]bool{9: true}
	api.ids = []int64{9, 10}
	m, api, _ := sessionModelWith(t, api)
	m, cmd := update(m, <-m.events)
	m.endOwnSession()
	m.events <- eventsRetryDue{gen: -1}
	run(cmd)
	if len(api.ended) != 1 || api.ended[0] != 10 {
		t.Errorf("ended = %v, want only the late session 10", api.ended)
	}
	noSocket(t, api)
}
