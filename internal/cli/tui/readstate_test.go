package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// readAPI is a server's read state: the documents GET returns, what a mark
// answers, and a record of every call.
type readAPI struct {
	stubAPI
	states   []schema.ReadStateV1
	channels []schema.ChannelV0
	listErr  error
	chanErr  error
	markErr  error

	mu      sync.Mutex
	reads   int
	listed  int
	marks   []markCall
	markDoc schema.ReadStateV1
}

type markCall struct {
	channel string
	id      int64
}

func (a *readAPI) ListReadState(context.Context) (schema.ListReadStateResponseV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	if a.listErr != nil {
		return schema.ListReadStateResponseV1{}, a.listErr
	}
	return schema.ListReadStateResponseV1{ReadState: a.states}, nil
}

func (a *readAPI) ListChannels(context.Context) (schema.ListChannelsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listed++
	if a.chanErr != nil {
		return schema.ListChannelsResponse{}, a.chanErr
	}
	return schema.ListChannelsResponse{Channels: a.channels}, nil
}

func (a *readAPI) MarkRead(_ context.Context, channel string, id int64) (schema.ReadStateV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.marks = append(a.marks, markCall{channel, id})
	if a.markErr != nil {
		return schema.ReadStateV1{}, a.markErr
	}
	return a.markDoc, nil
}

func (a *readAPI) counts() (reads, listed int, marks []markCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads, a.listed, append([]markCall(nil), a.marks...)
}

func readDoc(id int64, unread int) schema.ReadStateV1 {
	return schema.ReadStateV1{Schema: schema.ReadStateSchemaV1, ChannelID: id, Unread: unread}
}

// readClock is the injected clock and the record of what was scheduled.
type readClock struct {
	now    time.Time
	timers []time.Duration
}

// readModel is a signed-in model on general and ops, at 80x24, with a clock
// the test moves.
func readModel(t *testing.T, api *readAPI) (Model, *readClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clock := &readClock{now: time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)}
	m := NewModel(ctx, api, 7, []string{"general", "ops"})
	m.now = func() time.Time { return clock.now }
	m.rs.after = func(d time.Duration, _ tea.Msg) tea.Cmd {
		clock.timers = append(clock.timers, d)
		return nil
	}
	m.width, m.height = 80, 24
	return m, clock
}

// exec runs cmd and returns what it produced. A command that waits for the
// event channel is given something to wake it.
func exec(m Model, cmd tea.Cmd) []tea.Msg {
	for i := 0; i < 8; i++ {
		m.events <- struct{}{}
	}
	return run(cmd)
}

// feed runs cmd and feeds the read-state answers back into the model.
func feedRead(m Model, cmd tea.Cmd) Model {
	for _, msg := range exec(m, cmd) {
		switch msg.(type) {
		case readStateLoaded, readMarked:
			m, _ = update(m, msg)
		}
	}
	return m
}

// threeMessages is a page of messages 1 to 3, from someone else.
func threeMessages() []schema.MessageV2 {
	var out []schema.MessageV2
	for id := int64(1); id <= 3; id++ {
		out = append(out, schema.MessageV2{ID: id, AuthorID: 9})
	}
	return out
}

// listText is the channel list as drawn.
func listText(m Model) string {
	return strings.Join(m.channelLines(m.layout().leftW), "\n")
}

func TestUnreadCountsInTheChannelList(t *testing.T) {
	tests := []struct {
		name     string
		general  int
		wantLine string
		wantNone bool
	}{
		{name: "a few", general: 3, wantLine: "general 3"},
		{name: "one", general: 1, wantLine: "general 1"},
		{name: "none shows nothing", general: 0, wantNone: true},
		{name: "just under the cap", general: schema.MaxUnreadCount - 1, wantLine: "general 999"},
		{name: "the cap", general: schema.MaxUnreadCount, wantLine: "general 999+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &readAPI{
				states:   []schema.ReadStateV1{readDoc(1, tt.general), readDoc(2, 4)},
				channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}},
			}
			m, _ := readModel(t, api)
			m = feedRead(m, m.loadReadState())
			text := listText(m)
			if tt.wantNone {
				if strings.Contains(text, "general ") {
					t.Errorf("list shows a count for a channel with none:\n%s", text)
				}
			} else if !strings.Contains(text, tt.wantLine) {
				t.Errorf("list lacks %q:\n%s", tt.wantLine, text)
			}
			if !strings.Contains(text, "ops 4") {
				t.Errorf("list lacks %q:\n%s", "ops 4", text)
			}
			if !strings.Contains(m.View(), "ops 4") {
				t.Errorf("the screen lacks the count:\n%s", m.View())
			}
		})
	}
}

func TestReadStateResolvesNamesOnce(t *testing.T) {
	api := &readAPI{
		states:   []schema.ReadStateV1{readDoc(1, 3)},
		channels: []schema.ChannelV0{{ID: 1, Name: "general"}},
	}
	m, _ := readModel(t, api)
	m = feedRead(m, m.loadReadState())
	_ = feedRead(m, m.loadReadState())
	reads, listed, _ := api.counts()
	if reads != 2 || listed != 1 {
		t.Errorf("reads = %d, channel lists = %d, want 2 and 1", reads, listed)
	}
}

func TestReadStateLongNameKeepsItsCount(t *testing.T) {
	long := strings.Repeat("x", 40)
	api := &readAPI{states: []schema.ReadStateV1{readDoc(1, 12)}, channels: []schema.ChannelV0{{ID: 1, Name: long}}}
	m, _ := readModel(t, api)
	m.channels = []string{long}
	m = feedRead(m, m.loadReadState())
	line := m.channelLines(m.layout().leftW)[0]
	if !strings.HasSuffix(line, "12") {
		t.Errorf("line %q lost its count", line)
	}
	if got := cellWidth(line); got > m.layout().leftW-2 {
		t.Errorf("line is %d cells, wider than the pane", got)
	}
	// Wide runes take two cells each: the budget is in cells, not runes.
	wide := strings.Repeat("会", 30)
	api = &readAPI{states: []schema.ReadStateV1{readDoc(1, 12)}, channels: []schema.ChannelV0{{ID: 1, Name: wide}}}
	m, _ = readModel(t, api)
	m.channels = []string{wide}
	m = feedRead(m, m.loadReadState())
	line = m.channelLines(m.layout().leftW)[0]
	if !strings.HasSuffix(line, "12") || cellWidth(line) > m.layout().leftW-2 {
		t.Errorf("line %q is %d cells, want it in the pane with its count", line, cellWidth(line))
	}
}

func TestNoCountsAndNoErrorWhenRefused(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		status int
	}{
		{"an agent credential", schema.ErrorCodeHumanRequired, 403},
		{"authentication off", schema.ErrorCodeReadStateRequireAuth, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &readAPI{listErr: &cli.ServerError{Status: tt.status, Code: tt.code, Message: "no"}}
			m, _ := readModel(t, api)
			m.setStatus(modeChannels, "connected")
			m = feedRead(m, m.loadReadState())
			if !m.rs.off {
				t.Fatal("the refusal did not switch read state off")
			}
			if m.status() != "connected" || strings.Contains(listText(m), "general ") {
				t.Errorf("status %q, list:\n%s", m.status(), listText(m))
			}
			// From then on nothing is asked: no read for another channel's
			// message, no mark for a channel read to its newest message.
			m, cmd := update(m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 5, AuthorID: 9}})
			m = feedRead(m, cmd)
			m, cmd = update(m, messagesLoaded{channel: "general", messages: threeMessages()})
			m = feedRead(m, cmd)
			if reads, _, marks := api.counts(); reads != 1 || len(marks) != 0 {
				t.Errorf("reads = %d, marks = %v, want the one refused read and no mark", reads, marks)
			}
			if got := m.status(); got != "connected" {
				t.Errorf("status = %q, want it left alone", got)
			}
		})
	}
	t.Run("a mark refused", func(t *testing.T) {
		api := &readAPI{markErr: &cli.ServerError{Status: 403, Code: schema.ErrorCodeHumanRequired}}
		m, _ := readModel(t, api)
		m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
		m = feedRead(m, cmd)
		if !m.rs.off {
			t.Error("a refused mark did not switch read state off")
		}
	})
	t.Run("another failure keeps the counts and does not switch off", func(t *testing.T) {
		api := &readAPI{states: []schema.ReadStateV1{readDoc(2, 4)}, channels: []schema.ChannelV0{{ID: 2, Name: "ops"}}}
		m, _ := readModel(t, api)
		m = feedRead(m, m.loadReadState())
		api.listErr = errors.New("connection reset")
		m.setStatus(modeChannels, "connected")
		m = feedRead(m, m.loadReadState())
		if m.rs.off || !strings.Contains(listText(m), "ops 4") || m.status() != "connected" {
			t.Errorf("off = %v, status %q, list:\n%s", m.rs.off, m.status(), listText(m))
		}
	})
}

func TestMarkMovesOnOpenAndAtTheBottomButNotScrolledUp(t *testing.T) {
	api := &readAPI{}
	m, clock := readModel(t, api)
	m.rs.unread["general"] = 3

	// Opening: the page arrives, the newest message is on screen.
	m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 1 || marks[0] != (markCall{"general", 3}) {
		t.Fatalf("marks after opening = %v, want general at 3", marks)
	}
	if got := m.rs.unread["general"]; got != 0 {
		t.Errorf("unread after opening = %d, want 0 at once", got)
	}

	// Scrolled up: a message arrives and nothing moves.
	m.scroll = 2
	clock.now = clock.now.Add(3 * time.Second)
	m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 4, AuthorID: 9}})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 1 {
		t.Errorf("marks while scrolled up = %v, want still one", marks)
	}
	if got := m.rs.unread["general"]; got == 0 {
		t.Error("a message that was not shown did not count as unread")
	}

	// Back at the bottom the next message that arrives is shown, and marks
	// everything above it.
	m.scroll = 0
	clock.now = clock.now.Add(3 * time.Second)
	m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 5, AuthorID: 9}})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 2 || marks[1] != (markCall{"general", 5}) {
		t.Errorf("marks after scrolling back = %v, want general at 5 second", marks)
	}
	if got := m.rs.unread["general"]; got != 0 {
		t.Errorf("unread after scrolling back = %d, want 0", got)
	}

	// A message while the bottom is shown moves it again (after the interval).
	clock.now = clock.now.Add(3 * time.Second)
	m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 6, AuthorID: 9}})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 3 || marks[2] != (markCall{"general", 6}) {
		t.Errorf("marks for a message at the bottom = %v, want general at 6 third", marks)
	}
}

func TestMarkNotMovedOutsideTheChannelsView(t *testing.T) {
	api := &readAPI{}
	m, _ := readModel(t, api)
	m.mode = modeInbox
	m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 0 {
		t.Errorf("marks from the inbox = %v, want none", marks)
	}
	// A message that arrives meanwhile is not shown either.
	m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 4, AuthorID: 9}})
	feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 0 {
		t.Errorf("marks for a message not on screen = %v, want none", marks)
	}
}

func TestMarkIsCoalescedToOneEveryTwoSeconds(t *testing.T) {
	api := &readAPI{}
	m, clock := readModel(t, api)
	m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
	m = feedRead(m, cmd)

	// Three messages within the interval: none is sent, one timer is set.
	for i, id := range []int64{4, 5, 6} {
		clock.now = clock.now.Add(500 * time.Millisecond)
		m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: id, AuthorID: 9}})
		m = feedRead(m, cmd)
		if _, _, marks := api.counts(); len(marks) != 1 {
			t.Fatalf("after message %d marks = %v, want still one", id, marks)
		}
		if len(clock.timers) != 1 {
			t.Fatalf("after message %d timers = %v, want one", i, clock.timers)
		}
	}
	if clock.timers[0] != 1500*time.Millisecond {
		t.Errorf("timer = %v, want the 1.5s left of the interval", clock.timers[0])
	}

	// The interval is over: the newest id wins.
	clock.now = clock.now.Add(500 * time.Millisecond)
	m, cmd = update(m, markDue{channel: "general"})
	m = feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 2 || marks[1] != (markCall{"general", 6}) {
		t.Errorf("marks = %v, want the second to be general at 6", marks)
	}

	// A due timer with nothing newer sends nothing.
	_, cmd = update(m, markDue{channel: "general"})
	feedRead(m, cmd)
	if _, _, marks := api.counts(); len(marks) != 2 {
		t.Errorf("marks = %v after a stale timer, want still two", marks)
	}
}

func TestMarkAnswerSetsTheCount(t *testing.T) {
	api := &readAPI{markDoc: readDoc(1, 2)}
	m, _ := readModel(t, api)
	m.mode = modeInbox
	m, _ = update(m, readMarked{channel: "ops", state: readDoc(2, 6)})
	if got := m.rs.unread["ops"]; got != 6 {
		t.Errorf("unread = %d, want the server's 6", got)
	}
	m, _ = update(m, readMarked{channel: "ops", state: readDoc(2, 0)})
	if _, ok := m.rs.unread["ops"]; ok {
		t.Error("a count of 0 is still held")
	}
}

func TestCountsAreReadAgainForOtherChannelsAndAfterReconnect(t *testing.T) {
	api := &readAPI{
		states:   []schema.ReadStateV1{readDoc(2, 5)},
		channels: []schema.ChannelV0{{ID: 2, Name: "ops"}},
	}
	m, clock := readModel(t, api)

	// A message in another channel: +1 now, and the server is asked.
	m, cmd := update(m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 9, AuthorID: 9}})
	if got := m.rs.unread["ops"]; got != 1 {
		t.Errorf("local count = %d, want 1", got)
	}
	m = feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 1 {
		t.Fatalf("reads = %d, want 1", reads)
	}
	if got := m.rs.unread["ops"]; got != 5 {
		t.Errorf("count after the read = %d, want the server's 5", got)
	}

	// Another within the interval: +1 only.
	clock.now = clock.now.Add(time.Second)
	m, cmd = update(m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 10, AuthorID: 9}})
	m = feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 1 {
		t.Errorf("reads = %d within the interval, want still 1", reads)
	}
	if got := m.rs.unread["ops"]; got != 6 {
		t.Errorf("local count = %d, want 6", got)
	}

	// After the interval it asks again.
	clock.now = clock.now.Add(2 * time.Second)
	m, cmd = update(m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 11, AuthorID: 9}})
	m = feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 2 {
		t.Errorf("reads = %d after the interval, want 2", reads)
	}

	// A message of your own is not unread.
	before := m.rs.unread["ops"]
	m, _ = update(m, messageReceived{channel: "ops", message: schema.MessageV2{ID: 12, AuthorID: 7}})
	if got := m.rs.unread["ops"]; got != before {
		t.Errorf("own message changed the count %d to %d", before, got)
	}

	// The first open of a socket reads nothing; the open after a drop does.
	m, cmd = update(m, subscriptionOpened{channel: "general"})
	m = feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 2 {
		t.Errorf("reads = %d after the first open, want 2", reads)
	}
	m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
	m, cmd = update(m, subscriptionOpened{channel: "general"})
	m = feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 3 {
		t.Errorf("reads = %d after a reconnect, want 3", reads)
	}
	_, cmd = update(m, subscriptionOpened{channel: "general"})
	feedRead(m, cmd)
	if reads, _, _ := api.counts(); reads != 3 {
		t.Errorf("reads = %d after a second open with no drop, want 3", reads)
	}
}

func TestInitReadsTheCounts(t *testing.T) {
	api := &readAPI{states: []schema.ReadStateV1{readDoc(2, 5)}, channels: []schema.ChannelV0{{ID: 2, Name: "ops"}}}
	m, _ := readModel(t, api)
	var loaded bool
	for _, msg := range exec(m, m.Init()) {
		if _, ok := msg.(readStateLoaded); ok {
			loaded = true
		}
	}
	if !loaded {
		t.Error("Init did not read the counts")
	}
}

// A read the server answered before it had our mark must not bring back the
// count of a channel that has since been left.
func TestStaleReadDoesNotResurrectAReadChannel(t *testing.T) {
	api := &readAPI{
		states:   []schema.ReadStateV1{readDoc(1, 3)}, // served before the PUT
		channels: []schema.ChannelV0{{ID: 1, Name: "general"}},
		markDoc:  schema.ReadStateV1{Schema: schema.ReadStateSchemaV1, ChannelID: 1, LastReadID: 3},
	}
	m, _ := readModel(t, api)
	m.rs.unread["general"] = 3
	m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
	m = feedRead(m, cmd) // the PUT is answered: acknowledged at 3
	m, cmd = update(m, tea.KeyMsg{Type: tea.KeyDown})
	m = feedRead(m, cmd)
	// The GET that was served before the PUT lands now.
	m = feedRead(m, m.loadReadState())
	if got := m.rs.unread["general"]; got != 0 {
		t.Errorf("unread after a stale read = %d, want 0", got)
	}
	// A read that is not older than the mark still counts.
	api.states = []schema.ReadStateV1{{Schema: schema.ReadStateSchemaV1, ChannelID: 1, LastReadID: 3, Unread: 2}}
	m = feedRead(m, m.loadReadState())
	if got := m.rs.unread["general"]; got != 2 {
		t.Errorf("unread after a current read = %d, want 2", got)
	}
}

// The newest message coming back into view is read, by scrolling down and by
// returning to the channels view.
func TestMarkWhenTheNewestMessageComesBackIntoView(t *testing.T) {
	tests := []struct {
		name string
		away func(*Model)
		back tea.Msg
	}{
		{"scrolling down", func(m *Model) { m.scroll = 2 }, tea.KeyMsg{Type: tea.KeyPgDown}},
		{"returning to the channels view", func(m *Model) { m.mode = modeAgents }, tea.KeyMsg{Type: tea.KeyTab}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &readAPI{}
			m, clock := readModel(t, api)
			m, cmd := update(m, messagesLoaded{channel: "general", messages: threeMessages()})
			m = feedRead(m, cmd)
			tt.away(&m)
			clock.now = clock.now.Add(3 * time.Second)
			m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 4, AuthorID: 9}})
			m = feedRead(m, cmd)
			if _, _, marks := api.counts(); len(marks) != 1 {
				t.Fatalf("marks while away = %v, want one", marks)
			}
			if m.rs.unread["general"] != 1 {
				t.Fatalf("unread while away = %d, want 1", m.rs.unread["general"])
			}
			m, cmd = update(m, tt.back)
			m = feedRead(m, cmd)
			if _, _, marks := api.counts(); len(marks) != 2 || marks[1] != (markCall{"general", 4}) {
				t.Errorf("marks after coming back = %v, want general at 4 second", marks)
			}
			if got := m.rs.unread["general"]; got != 0 {
				t.Errorf("unread after coming back = %d, want 0", got)
			}
		})
	}
}

func TestReadStateKeepsNamedChannelsWhenTheChannelListFails(t *testing.T) {
	api := &readAPI{states: []schema.ReadStateV1{readDoc(1, 3), readDoc(2, 4)}, channels: []schema.ChannelV0{{ID: 1, Name: "general"}}}
	m, _ := readModel(t, api)
	m.rs.names[1] = "general"
	api.chanErr = errors.New("boom")
	m = feedRead(m, m.loadReadState())
	if got := m.rs.unread["general"]; got != 3 {
		t.Errorf("unread = %d, want the named channel's 3 kept", got)
	}
}
