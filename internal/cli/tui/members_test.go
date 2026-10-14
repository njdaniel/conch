package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// testNow is the clock of the roster tests; sessions are seen relative to it.
var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// sess is a session as a roster frame carries it. Status is given by the
// caller, as the server derives it; state follows from it.
func sess(id, principal int64, kind schema.PrincipalKind, name string, status schema.SessionStatus) schema.SessionV1 {
	state := schema.SessionStateOnline
	switch status {
	case schema.SessionStatusStale:
		state = schema.SessionStateStale
	case schema.SessionStatusOffline:
		state = schema.SessionStateOffline
	}
	return schema.SessionV1{
		Schema: schema.SessionSchemaV1, ID: id, PrincipalID: principal, Kind: kind, Name: name,
		Client: "test", State: state, Status: status,
		StartedAt: schema.NewTimestamp(testNow.Add(-time.Hour)), LastSeenAt: schema.NewTimestamp(testNow.Add(-3 * time.Minute)),
		Interventions: []schema.InterventionKind{},
	}
}

func rosterOf(sessions ...schema.SessionV1) schema.SessionRosterV1 {
	if sessions == nil {
		sessions = []schema.SessionV1{}
	}
	return schema.SessionRosterV1{Schema: schema.SessionRosterSchemaV1, ChannelID: 1, Sessions: sessions}
}

// sessionAPI is a fake server for the session tests. It records the calls the
// TUI makes for its own session and its events socket, and each events socket
// blocks until its context ends, like a healthy one.
type sessionAPI struct {
	stubAPI
	mu        sync.Mutex
	createErr error
	// ids are the ids the next sessions get, in order; 9 once they run out.
	ids []int64
	// gone is the sessions whose events socket is refused with session_not_found.
	gone       map[int64]bool
	created    []schema.CreateSessionRequestV1
	ended      []int64
	endCtxErrs []error
	events     []eventsCall
	started    chan eventsCall
	// afters records the after of each message socket, in order.
	afters []int64
	// page is what the first load returns; pages counts the loads.
	page  []schema.MessageV2
	pages int
	// live is delivered on the n-th message socket (from 1); the first socket
	// then returns drop, if there is one, and the others stay up.
	live map[int][]schema.MessageV2
	drop error
}

type eventsCall struct {
	channel string
	session int64
	ctx     context.Context
}

func newSessionAPI() *sessionAPI {
	return &sessionAPI{started: make(chan eventsCall, 32), live: map[int][]schema.MessageV2{}}
}

func (a *sessionAPI) CreateSession(_ context.Context, req schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.created = append(a.created, req)
	if a.createErr != nil {
		return schema.SessionV1{}, a.createErr
	}
	id := int64(9)
	if len(a.ids) > 0 {
		id, a.ids = a.ids[0], a.ids[1:]
	}
	return sess(id, 5, schema.PrincipalHuman, "alice", schema.SessionStatusOnline), nil
}

func (a *sessionAPI) EndSession(ctx context.Context, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, id)
	a.endCtxErrs = append(a.endCtxErrs, ctx.Err())
	return nil
}

func (a *sessionAPI) SubscribeEvents(ctx context.Context, channel string, session int64, _ cli.EventHandler) error {
	call := eventsCall{channel: channel, session: session, ctx: ctx}
	a.mu.Lock()
	a.events = append(a.events, call)
	a.mu.Unlock()
	a.started <- call
	a.mu.Lock()
	refused := a.gone[session]
	a.mu.Unlock()
	if refused {
		return &cli.ServerError{Status: 404, Code: schema.ErrorCodeSessionNotFound, Message: "no such session"}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *sessionAPI) ListMessagesV2(context.Context, string, int64, int) (schema.ListMessagesResponseV2, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pages++
	return schema.ListMessagesResponseV2{Messages: a.page}, nil
}

func (a *sessionAPI) SubscribeV2After(ctx context.Context, _ string, after int64, opened func(), receive func(schema.MessageV2) error) error {
	a.mu.Lock()
	a.afters = append(a.afters, after)
	n := len(a.afters)
	live, drop := a.live[n], a.drop
	a.mu.Unlock()
	if opened != nil {
		opened()
	}
	for _, message := range live {
		if err := receive(message); err != nil {
			return err
		}
	}
	if n == 1 && drop != nil {
		return drop
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *sessionAPI) eventsCalls() []eventsCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]eventsCall(nil), a.events...)
}

func awaitEvents(t *testing.T, a *sessionAPI, channel string) {
	t.Helper()
	select {
	case got := <-a.started:
		if got.channel != channel || got.session != 9 {
			t.Fatalf("events socket for %q session %d, want %q with the TUI's session 9", got.channel, got.session, channel)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no events socket opened for %q", channel)
	}
}

// sessionModel is a model on "general" and "ops" with its own session created
// (id 9) and its first events socket open, a recording clock and a fixed now.
func sessionModel(t *testing.T) (Model, *sessionAPI, *[]recordedTimer) {
	t.Helper()
	return sessionModelWith(t, newSessionAPI())
}

func sessionModelWith(t *testing.T, api *sessionAPI) (Model, *sessionAPI, *[]recordedTimer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewModel(ctx, api, 5, []string{"general", "ops"})
	m.userName = "alice"
	m.now = func() time.Time { return testNow }
	timers := &[]recordedTimer{}
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		*timers = append(*timers, recordedTimer{d, msg})
		return func() tea.Msg { return nil }
	}
	started, ok := m.startSession()().(sessionStarted)
	if !ok || started.err != nil {
		t.Fatalf("session did not start: %+v", started)
	}
	m, cmd := update(m, started)
	run(cmd)
	awaitEvents(t, api, "general")
	return m, api, timers
}

func frame(m Model, roster schema.SessionRosterV1) Model {
	m, _ = update(m, eventsRosterReceived{gen: m.ev.gen, roster: roster})
	return m
}

func TestOwnSessionStarts(t *testing.T) {
	m, api, _ := sessionModel(t)
	if len(api.created) != 1 || api.created[0].Client != "tui" {
		t.Errorf("created %+v, want one session with client tui", api.created)
	}
	if m.session != 9 {
		t.Errorf("session = %d, want 9", m.session)
	}
	calls := api.eventsCalls()
	if len(calls) != 1 || calls[0].channel != "general" || calls[0].session != 9 {
		t.Errorf("events sockets = %+v, want one for general with session 9", calls)
	}
}

func TestOwnSessionCreateFails(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"auth off", &cli.ServerError{Status: 400, Code: schema.ErrorCodeSessionsRequireAuth, Message: "no"}},
		{"too many", &cli.ServerError{Status: 429, Code: schema.ErrorCodeTooManySessions, Message: "no"}},
		{"network", errors.New("connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			api := newSessionAPI()
			api.createErr = tt.err
			m := NewModel(ctx, api, 5, []string{"general"})
			started := m.startSession()().(sessionStarted)
			m, cmd := update(m, started)
			if cmd != nil {
				t.Errorf("a failed session opened a socket: %v", run(cmd))
			}
			if got := m.membersLine(80); got != "members: unavailable" {
				t.Errorf("members line = %q, want unavailable", got)
			}
			// Messaging is not affected.
			m.authorID = 5
			m.input = "hello"
			if _, cmd := update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
				t.Error("sending did not work without a session")
			}
			if strings.Contains(m.status(), "session") {
				t.Errorf("status = %q: a failed session must not touch the status line", m.status())
			}
		})
	}
}

// The program can quit while its session is still being created: the session
// is then ended by the call that made it, and Run has nothing to end.
func TestOwnSessionCreatedAfterQuit(t *testing.T) {
	api := newSessionAPI()
	m := NewModel(context.Background(), api, 5, []string{"general"})
	m.endOwnSession()
	if len(api.ended) != 0 {
		t.Fatalf("ended %v before any session existed", api.ended)
	}
	started := m.startSession()().(sessionStarted)
	if !errors.Is(started.err, context.Canceled) {
		t.Errorf("err = %v, want canceled", started.err)
	}
	if len(api.ended) != 1 || api.ended[0] != 9 {
		t.Errorf("ended = %v, want the late session 9", api.ended)
	}
}

func TestEndOwnSessionEndsOnceWithALiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := newSessionAPI()
	m := NewModel(ctx, api, 5, []string{"general"})
	m.startSession()()
	cancel() // a signal ends the program's context
	m.endOwnSession()
	m.endOwnSession()
	if len(api.ended) != 1 || api.ended[0] != 9 {
		t.Fatalf("ended = %v, want session 9 once", api.ended)
	}
	if api.endCtxErrs[0] != nil {
		t.Errorf("the end call's context was already %v; it needs one of its own", api.endCtxErrs[0])
	}
}

func TestMembersLine(t *testing.T) {
	human := func(id, principal int64, name string, status schema.SessionStatus) schema.SessionV1 {
		return sess(id, principal, schema.PrincipalHuman, name, status)
	}
	agent := func(id, principal int64, name string, status schema.SessionStatus) schema.SessionV1 {
		return sess(id, principal, schema.PrincipalAgent, name, status)
	}
	tests := []struct {
		name     string
		sessions []schema.SessionV1
		want     string
	}{
		{"nobody", nil, "online: nobody"},
		{"the example of the design",
			[]schema.SessionV1{
				human(1, 5, "nick", schema.SessionStatusOnline), human(2, 5, "nick", schema.SessionStatusOnline),
				human(3, 6, "guest", schema.SessionStatusOnline),
				agent(4, 7, "worker-test", schema.SessionStatusRunning), agent(5, 8, "observer-test", schema.SessionStatusOffline),
			}, "online: nick(2) guest · agents: worker-test ● observer-test ○"},
		{"two sessions of one principal are one name", []schema.SessionV1{
			human(1, 5, "nick", schema.SessionStatusOnline), human(2, 5, "nick", schema.SessionStatusOnline),
		}, "online: nick(2)"},
		{"a stale human is not online", []schema.SessionV1{human(1, 5, "nick", schema.SessionStatusStale)}, "online: nobody"},
		{"running", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusRunning)}, "online: nobody · agents: a ●"},
		{"idle", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusIdle)}, "online: nobody · agents: a ●"},
		{"blocked", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusBlocked)}, "online: nobody · agents: a ●"},
		{"waiting", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusWaiting)}, "online: nobody · agents: a ●"},
		{"failed", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusFailed)}, "online: nobody · agents: a ●"},
		{"stale", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusStale)}, "online: nobody · agents: a ◐"},
		{"offline", []schema.SessionV1{agent(1, 7, "a", schema.SessionStatusOffline)}, "online: nobody · agents: a ○"},
		{"the best session of an agent wins", []schema.SessionV1{
			agent(1, 7, "a", schema.SessionStatusOffline), agent(2, 7, "a", schema.SessionStatusIdle),
		}, "online: nobody · agents: a ●"},
		{"two live sessions of an agent", []schema.SessionV1{
			agent(1, 7, "a", schema.SessionStatusIdle), agent(2, 7, "a", schema.SessionStatusRunning),
		}, "online: nobody · agents: a(2) ●"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := sessionModel(t)
			m = frame(m, rosterOf(tt.sessions...))
			if got := m.membersLine(80); got != tt.want {
				t.Errorf("members line = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMembersLineFits(t *testing.T) {
	m, _, _ := sessionModel(t)
	m = frame(m, rosterOf(
		sess(1, 5, schema.PrincipalHuman, "a-very-long-human-name", schema.SessionStatusOnline),
		sess(2, 7, schema.PrincipalAgent, "a-very-long-agent-name", schema.SessionStatusRunning),
	))
	for _, width := range []int{80, 40, 20, 5} {
		if got := m.membersLine(width); cellWidth(got) > width {
			t.Errorf("width %d: line %q is %d cells", width, got, cellWidth(got))
		}
	}
}

// Never a stale roster: the views read unavailable the moment the socket goes
// and until a new roster frame arrives, however many retries fail between.
func TestRosterNeverStale(t *testing.T) {
	m, api, timers := sessionModel(t)
	m = frame(m, rosterOf(sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)))
	if !strings.Contains(m.membersLine(80), "worker") {
		t.Fatalf("members line = %q, want the roster", m.membersLine(80))
	}
	m, _ = update(m, eventsEnded{gen: m.ev.gen, err: errors.New("eof")})
	for _, line := range []string{m.membersLine(80), strings.Join(plainLines(m.agentLines(60)), "|")} {
		if !strings.Contains(line, "unavailable") || strings.Contains(line, "worker") {
			t.Errorf("after the drop a view reads %q, want unavailable and no roster", line)
		}
	}
	if len(*timers) != 1 || (*timers)[0].d != minResubscribeDelay {
		t.Fatalf("timers = %+v, want one retry after %v", *timers, minResubscribeDelay)
	}
	// The retry reopens the socket for the same channel and session; until it
	// speaks, the views still read unavailable.
	m, cmd := update(m, (*timers)[0].msg)
	run(cmd)
	awaitEvents(t, api, "general")
	if got := m.membersLine(80); got != "members: unavailable" {
		t.Errorf("during the retry the line = %q, want unavailable", got)
	}
	// Failing again waits twice as long.
	m, _ = update(m, eventsEnded{gen: m.ev.gen, err: errors.New("refused")})
	if len(*timers) != 2 || (*timers)[1].d != 2*minResubscribeDelay {
		t.Errorf("timers = %+v, want the second retry after %v", *timers, 2*minResubscribeDelay)
	}
	// A roster frame is the state again, whatever the old one said.
	m, cmd = update(m, (*timers)[1].msg)
	run(cmd)
	awaitEvents(t, api, "general")
	m = frame(m, rosterOf(sess(2, 8, schema.PrincipalAgent, "other", schema.SessionStatusIdle)))
	if got := m.membersLine(80); !strings.Contains(got, "other") || strings.Contains(got, "worker") {
		t.Errorf("after a new roster the line = %q", got)
	}
}

func TestEventsEndedRetry(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		retry bool
	}{
		{"dropped", errors.New("eof"), true},
		{"closed cleanly", nil, true},
		{"too slow (1008)", websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "subscriber too slow"}, true},
		{"server busy", &cli.ServerError{Status: 429, Code: schema.ErrorCodeRateLimited}, true},
		{"server error", &cli.ServerError{Status: 500, Code: "internal"}, true},
		{"not a member", &cli.ServerError{Status: 404, Code: "channel_not_found"}, false},
		{"session gone", &cli.ServerError{Status: 404, Code: schema.ErrorCodeSessionNotFound}, false},
		{"not logged in", &cli.UnauthenticatedError{Server: "http://h:1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, timers := sessionModel(t)
			m = frame(m, rosterOf(sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)))
			m, _ = update(m, eventsEnded{gen: m.ev.gen, err: tt.err})
			if got := m.membersLine(80); got != "members: unavailable" {
				t.Errorf("line = %q, want unavailable", got)
			}
			if (len(*timers) == 1) != tt.retry {
				t.Errorf("timers = %d, want retry=%v", len(*timers), tt.retry)
			}
		})
	}
}

func TestEventsLateResultsAreDropped(t *testing.T) {
	m, _, _ := sessionModel(t)
	oldGen := m.ev.gen
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown}) // ops
	if m.ev.gen == oldGen {
		t.Fatal("selecting a channel kept the events socket")
	}
	m, _ = update(m, eventsRosterReceived{gen: oldGen, roster: rosterOf(sess(1, 7, schema.PrincipalAgent, "ghost", schema.SessionStatusIdle))})
	m, _ = update(m, eventsEnded{gen: oldGen, err: errors.New("eof")})
	if m.ev.state == eventsLive || m.ev.retryPending {
		t.Errorf("a late result from the old channel's socket was kept: %+v", m.ev)
	}
}

func TestEventsSocketFollowsTheChannel(t *testing.T) {
	m, api, _ := sessionModel(t)
	first := api.eventsCalls()[0]
	m = frame(m, rosterOf(sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)))
	m, cmd := update(m, tea.KeyMsg{Type: tea.KeyDown})
	run(cmd)
	awaitEvents(t, api, "ops")
	if first.ctx.Err() == nil {
		t.Error("the general socket was left open after switching to ops")
	}
	if got := m.membersLine(80); got != "members: connecting…" {
		t.Errorf("on the new channel before its roster the line = %q, want connecting, not general's roster", got)
	}
}

// P3-03: a roster is only a roster. Whatever it carries, the draft and the
// selected channel stay as they were.
func TestRosterUpdatesLeaveDraftAndChannelAlone(t *testing.T) {
	rosters := []schema.SessionRosterV1{
		rosterOf(),
		rosterOf(sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)),
		rosterOf(sess(1, 5, schema.PrincipalHuman, "alice", schema.SessionStatusOnline), sess(2, 7, schema.PrincipalAgent, "worker", schema.SessionStatusFailed)),
	}
	m, _, _ := sessionModel(t)
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown})
	m.input = "half a thought"
	m.scroll = 3
	status := m.status()
	for i, roster := range rosters {
		m = frame(m, roster)
		if i == 1 {
			m, _ = update(m, eventsEnded{gen: m.ev.gen, err: errors.New("eof")})
		}
		if m.input != "half a thought" || m.current() != "ops" || m.selected != 1 || m.scroll != 3 || m.status() != status {
			t.Fatalf("after roster %d: input %q channel %q scroll %d status %q", i, m.input, m.current(), m.scroll, m.status())
		}
	}
}

// Terminal safety: names and tasks are chosen by other people and agents.
func TestRosterTextIsSafe(t *testing.T) {
	evil := sess(1, 7, schema.PrincipalAgent, "wor\x1b[31mker\x1b]0;pwn\x07", schema.SessionStatusRunning)
	evil.Task = "fix \x1b[2Jthe\r\nbug\x00 \u202egnirts"
	evil.Runtime = "run\x1btime"
	evil.Model = "mo\x1bdel"
	evilHuman := sess(2, 6, schema.PrincipalHuman, "g\x1b[1;1Huest[agent]", schema.SessionStatusOnline)
	m, _, _ := sessionModel(t)
	m = frame(m, rosterOf(evil, evilHuman))
	m.members["general"] = map[int64]schema.ChannelMemberV1{7: {PrincipalID: 7, Kind: schema.PrincipalAgent, Name: "wor\x1b[31mker\x1b]0;pwn\x07"}}
	m.expanded[7] = true
	m.width, m.height = 100, 30

	var texts []string
	texts = append(texts, m.membersLine(100))
	texts = append(texts, plainLines(m.agentLines(98))...)
	m.mode = modeAgents
	texts = append(texts, m.View())
	m.mode = modeChannels
	texts = append(texts, m.View())
	for _, text := range texts {
		for _, r := range text {
			if r == 0x1b || r == 0x07 || r == 0 || r == '\r' || r == 0x202e {
				t.Fatalf("control character %U reached the terminal in %q", r, text)
			}
		}
	}
	if line := m.membersLine(100); !strings.Contains(line, `wor\[31mker]0;pwn`) || !strings.Contains(line, `g\[1;1Huest\[agent]`) {
		t.Errorf("members line = %q: names are kept as text, escapes dropped and brackets escaped", line)
	}
}

// After a dropped message stream the TUI asks for what came after the last
// message it holds: no gap, no duplicate, and the channel is not loaded again.
func TestMessageStreamResumesAfterLastID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := newSessionAPI()
	api.page = []schema.MessageV2{{ID: 1, Body: "one"}, {ID: 2, Body: "two"}, {ID: 3, Body: "three"}}
	// The first socket delivers 4 (and a repeat of 3, as a replay may) and
	// drops; the second delivers 5 and stays up.
	api.live[1] = []schema.MessageV2{{ID: 3, Body: "three"}, {ID: 4, Body: "four"}}
	api.drop = errors.New("connection reset")
	api.live[2] = []schema.MessageV2{{ID: 5, Body: "five"}}
	m := NewModel(ctx, api, 5, []string{"general"})
	timers := &[]recordedTimer{}
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		*timers = append(*timers, recordedTimer{d, msg})
		return func() tea.Msg { return nil }
	}

	// The first load is a REST page; the socket opens after its last id.
	loaded := m.loadAndFollow()().(messagesLoaded)
	m, _ = update(m, loaded)
	next := func() tea.Msg {
		select {
		case msg := <-m.events:
			return msg
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return nil
		}
	}
	// The socket opens, delivers 3 (a repeat) and 4, and drops.
	for ended := false; !ended; {
		msg := next()
		_, ended = msg.(subscriptionEnded)
		m, _ = update(m, msg)
	}
	if got := api.afters; len(got) != 1 || got[0] != 3 {
		t.Fatalf("first socket after = %v, want [3]", got)
	}
	if len(*timers) != 1 {
		t.Fatalf("timers = %d, want one retry", len(*timers))
	}
	// The retry opens the second socket after 4 and loads no page.
	if m.status() != statusReconnecting {
		t.Fatalf("status after the drop = %q, want %q", m.status(), statusReconnecting)
	}
	// While the socket is down the user sends, and the reply carries id 10:
	// others may have posted 5 to 9 meanwhile, so the retry must still resume
	// after 4, the last id the stream gave, not after the user's own 10.
	m, _ = update(m, messageSent{channel: "general", message: schema.MessageV2{ID: 10, Body: "mine"}})
	m, cmd := update(m, (*timers)[0].msg)
	run(cmd)
	// The socket opening again ends "reconnecting", though it has nothing to
	// say yet: a quiet channel must not read as down for ever.
	for delivered := false; !delivered; {
		msg := next()
		_, delivered = msg.(messageReceived)
		m, _ = update(m, msg)
	}
	// ("sent" from the send above is fine; "reconnecting" is not.)
	if m.status() == statusReconnecting {
		t.Errorf("status after the socket opened again = %q, want it cleared", m.status())
	}
	api.mu.Lock()
	afters, pages := append([]int64(nil), api.afters...), api.pages
	api.mu.Unlock()
	if len(afters) != 2 || afters[1] != 4 {
		t.Errorf("afters = %v, want [3 4]: the resume must ask for what came after the last id held", afters)
	}
	if pages != 1 {
		t.Errorf("the channel was loaded %d times, want once: a reconnect does not reload", pages)
	}
	var ids []int64
	for _, message := range m.messages["general"] {
		ids = append(ids, message.ID)
	}
	if want := []int64{1, 2, 3, 4, 5, 10}; len(ids) != len(want) {
		t.Errorf("messages = %v, want %v with no gap and no duplicate", ids, want)
	} else {
		for i := range want {
			if ids[i] != want[i] {
				t.Errorf("messages = %v, want %v", ids, want)
				break
			}
		}
	}
}

// A first load that fails leaves no socket to follow from; it is retried on
// the same backoff, and the retry loads the page again.
func TestFailedFirstLoadRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewModel(ctx, newSessionAPI(), 5, []string{"general"})
	timers := &[]recordedTimer{}
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		*timers = append(*timers, recordedTimer{d, msg})
		return func() tea.Msg { return nil }
	}
	m, cmd := update(m, messagesLoaded{channel: "general", err: errors.New("boom"), following: true})
	run(cmd)
	if len(*timers) != 1 || (*timers)[0].d != minResubscribeDelay || m.subscribed["general"] {
		t.Fatalf("timers %+v subscribed %v, want a retry and no socket", *timers, m.subscribed["general"])
	}
	m, cmd = update(m, (*timers)[0].msg)
	if cmd == nil || !m.subscribed["general"] {
		t.Errorf("the retry did not load the page and follow")
	}
}

// plainLines is lines without their selection index, for tests that read text.
func plainLines(lines []string, _ int) []string { return lines }
