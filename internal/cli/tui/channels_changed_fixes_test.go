package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// removedAPI is a server that has removed the person from ops: its events
// socket is refused as the retry meets it, and read state has the rows given.
type removedAPI struct {
	*channelRefreshAPI
	states []schema.ReadStateV1
	reads  int
	mu2    sync.Mutex
	// recv and ctxs record the message sockets opened, by channel.
	recv map[string]func(schema.MessageV2) error
	ctxs map[string]context.Context
}

func (a *removedAPI) SubscribeEvents(context.Context, string, int64, cli.EventHandler) error {
	return &cli.ServerError{Status: 404, Code: errChannelNotFound, Message: "channel not found"}
}

func (a *removedAPI) ListReadState(context.Context) (schema.ListReadStateResponseV1, error) {
	a.mu2.Lock()
	defer a.mu2.Unlock()
	a.reads++
	return schema.ListReadStateResponseV1{ReadState: a.states}, nil
}

func (a *removedAPI) SubscribeV2After(ctx context.Context, channel string, _ int64, opened func(), receive func(schema.MessageV2) error) error {
	a.mu2.Lock()
	if a.recv == nil {
		a.recv, a.ctxs = map[string]func(schema.MessageV2) error{}, map[string]context.Context{}
	}
	a.recv[channel], a.ctxs[channel] = receive, ctx
	a.mu2.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (a *removedAPI) socket(channel string) (context.Context, bool) {
	a.mu2.Lock()
	defer a.mu2.Unlock()
	ctx, ok := a.ctxs[channel]
	return ctx, ok
}

func instantAfter(_ time.Duration, msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }

func msgsOf[T any](msgs []tea.Msg) []T {
	var out []T
	for _, msg := range msgs {
		if v, ok := msg.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestRemovedSelectedChannelRefreshesList drives the real sequence of a
// removal from the open channel: the server closes the events socket with
// 1008 and sends no channels_changed on it, the retry meets 404
// channel_not_found, and nothing else tells the TUI. Each of the two answers
// must read the list; the second read is the one that lacks the channel.
func TestRemovedSelectedChannelRefreshesList(t *testing.T) {
	closed1008 := fmt.Errorf("cli: read events: %w", websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "no longer a member of this channel"})
	tests := []struct {
		name string
		// firstEnd is how the open socket ends; the refresh it starts still
		// sees ops (the server has not caught up), so only the retry's 404
		// reveals the removal.
		firstEnd error
		// retried says the 1008 is still retried.
		retried bool
	}{
		{name: "1008 close then 404 on the retry", firstEnd: closed1008, retried: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &channelRefreshAPI{answers: []channelListAnswer{
				{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}},
				{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}},
				{channels: []schema.ChannelV0{{ID: 1, Name: "general"}}},
			}}
			api := &removedAPI{channelRefreshAPI: base}
			m := channelRefreshModel(t, api)
			m.after = instantAfter
			m.session = 9
			gen := m.ev.gen

			m, cmd := update(m, eventsEnded{gen: gen, err: tt.firstEnd})
			msgs := runChannelRefreshCommand(m, cmd)
			loaded := msgsOf[channelsLoaded](msgs)
			if len(loaded) != 1 {
				t.Fatalf("a 1008 close of the events socket read the channel list %d times, want 1 (messages %#v)", len(loaded), msgs)
			}
			retries := msgsOf[eventsRetryDue](msgs)
			if tt.retried != (len(retries) == 1) {
				t.Fatalf("retry scheduled = %v, want %v", len(retries) == 1, tt.retried)
			}
			m, _ = update(m, loaded[0])
			if m.current() != "ops" {
				t.Fatalf("selected = %q after a list that still has ops", m.current())
			}

			// The retry opens a new socket, which the server refuses.
			m, cmd = update(m, retries[0])
			if cmd == nil {
				t.Fatal("the retry opened no socket")
			}
			cmd()
			var ended eventsEnded
			select {
			case msg := <-m.events:
				got, ok := msg.(eventsEnded)
				if !ok {
					t.Fatalf("socket reported %#v, want eventsEnded", msg)
				}
				ended = got
			case <-time.After(2 * time.Second):
				t.Fatal("the retried socket never ended")
			}
			m, cmd = update(m, ended)
			loaded = msgsOf[channelsLoaded](runChannelRefreshCommand(m, cmd))
			if len(loaded) != 1 {
				t.Fatalf("a channel_not_found refusal read the channel list %d times, want 1", len(loaded))
			}
			m, _ = update(m, loaded[0])
			if got := strings.Join(m.channels, ","); got != "general" {
				t.Errorf("channels = %q, want general", got)
			}
			if m.current() != "general" {
				t.Errorf("selected = %q, want general", m.current())
			}
			if want := "#ops is no longer available to you."; m.status() != want {
				t.Errorf("status = %q, want %q", m.status(), want)
			}
		})
	}
}

// TestChannelNotFoundOnMessageSocketRefreshes covers the other two places the
// removal shows: the selected channel's page load and its subscription.
func TestChannelNotFoundOnMessageSocketRefreshes(t *testing.T) {
	notFound := &cli.ServerError{Status: 404, Code: errChannelNotFound}
	tests := []struct {
		name     string
		msg      func(m Model) tea.Msg
		wantRead bool
	}{
		{"page load refused", func(Model) tea.Msg { return messagesLoaded{channel: "ops", following: true, err: notFound} }, true},
		{"subscription refused", func(m Model) tea.Msg {
			return subscriptionEnded{channel: "ops", gen: m.subs.byName["ops"].gen, err: notFound}
		}, true},
		{"other refusal", func(Model) tea.Msg {
			return subscriptionEnded{channel: "ops", err: &cli.ServerError{Status: 500, Code: "internal"}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &channelRefreshAPI{answers: []channelListAnswer{{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}}, {channels: []schema.ChannelV0{{ID: 1, Name: "general"}}}}}
			m := channelRefreshModel(t, api)
			m.after = instantAfter
			m, cmd := update(m, tt.msg(m))
			loaded := msgsOf[channelsLoaded](runChannelRefreshCommand(m, cmd))
			if (len(loaded) == 1) != tt.wantRead {
				t.Fatalf("channel list reads = %d, want read = %v", len(loaded), tt.wantRead)
			}
		})
	}
}

// TestRenamedChannelIsTornDown renames ops to staff, then makes a new channel
// named ops: nothing of the old socket may reach it.
func TestRenamedChannelIsTornDown(t *testing.T) {
	base := &channelRefreshAPI{answers: []channelListAnswer{
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}},
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}}},
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}, {ID: 3, Name: "ops"}}},
	}}
	api := &removedAPI{channelRefreshAPI: base}
	m := channelRefreshModel(t, api)
	m.subscribed["ops"] = true
	runCmd(m.startSubscription("ops"))
	oldGen := m.subs.byName["ops"].gen
	oldCtx := func() context.Context {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ctx, ok := api.socket("ops"); ok {
				return ctx
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the ops socket never opened")
		return nil
	}()
	m.messages["ops"] = []schema.MessageV2{{ID: 5, Body: "kept"}}
	m.loaded["ops"], m.streamed["ops"] = true, 5
	m.rs.unread["ops"], m.rs.marked["ops"], m.rs.acked["ops"] = 3, 5, 5
	m.backoff["ops"] = time.Second

	m, _ = update(m, eventsChannelsReceived{gen: m.ev.gen})
	m, _ = update(m, channelsLoaded{refresh: true, channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}}})

	if oldCtx.Err() == nil {
		t.Error("the renamed-away channel's socket was not cancelled")
	}
	for name, held := range map[string]bool{
		"subscribed": m.subscribed["ops"], "messages": len(m.messages["ops"]) > 0, "loaded": m.loaded["ops"],
		"streamed": m.streamed["ops"] != 0, "unread": m.rs.unread["ops"] != 0, "marked": m.rs.marked["ops"] != 0,
		"acked": m.rs.acked["ops"] != 0, "backoff": m.backoff["ops"] != 0,
	} {
		if held {
			t.Errorf("%s still held under the old name", name)
		}
	}
	if m.current() != "staff" {
		t.Fatalf("selected = %q, want staff", m.current())
	}
	if !m.subscribed["staff"] {
		t.Error("the renamed channel did not open fresh under its new name")
	}

	// Another channel takes the name; the person opens it.
	m, _ = update(m, eventsChannelsReceived{gen: m.ev.gen})
	m, _ = update(m, channelsLoaded{refresh: true, channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}, {ID: 3, Name: "ops"}}})
	m.selected = 2
	m, _ = m.openSelected()
	if !m.subscribed["ops"] {
		t.Fatal("the new ops did not subscribe: a stale flag was found")
	}
	if got := m.subs.byName["ops"].gen; got == 0 || got == oldGen {
		t.Fatalf("new ops socket generation = %d, old = %d", got, oldGen)
	}

	// The old socket delivers a late message and a late end.
	late := schema.MessageV2{ID: 99, Body: "staff secret"}
	m, _ = update(m, messageReceived{channel: "ops", gen: oldGen, message: late})
	m, _ = update(m, subscriptionEnded{channel: "ops", gen: oldGen, err: fmt.Errorf("boom")})
	for _, msg := range m.messages["ops"] {
		if msg.ID == late.ID {
			t.Error("a late message of the old socket landed in the new ops")
		}
	}
	if !m.subscribed["ops"] {
		t.Error("a late end of the old socket cleared the new ops subscription")
	}
	// The current socket still delivers.
	m, _ = update(m, messageReceived{channel: "ops", gen: m.subs.byName["ops"].gen, message: schema.MessageV2{ID: 100, Body: "mine"}})
	if len(m.messages["ops"]) == 0 || m.messages["ops"][len(m.messages["ops"])-1].ID != 100 {
		t.Error("the new ops socket's message was dropped")
	}
}

// readCalls drives one refresh answer through the model and its commands.
func refreshWithReadState(t *testing.T, m Model, next []schema.ChannelV0) (Model, []tea.Msg) {
	t.Helper()
	m.loadingChannels = true
	m, cmd := update(m, channelsLoaded{refresh: true, channels: next})
	msgs := runCmd(cmd)
	for _, rs := range msgsOf[readStateLoaded](msgs) {
		m, _ = update(m, rs)
	}
	return m, msgs
}

func TestRenameReplacesReadStateNames(t *testing.T) {
	tests := []struct {
		name       string
		next       []schema.ChannelV0
		states     []schema.ReadStateV1
		wantUnread map[string]int
	}{
		{
			name:       "renamed channel keeps its counts under the new name only",
			next:       []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}},
			states:     []schema.ReadStateV1{{ChannelID: 2, Unread: 3}},
			wantUnread: map[string]int{"staff": 3},
		},
		{
			name:       "a new channel taking the old name inherits nothing",
			next:       []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "staff"}, {ID: 3, Name: "ops"}},
			states:     []schema.ReadStateV1{{ChannelID: 2, Unread: 3}},
			wantUnread: map[string]int{"staff": 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &channelRefreshAPI{answers: []channelListAnswer{{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}}}}
			api := &removedAPI{channelRefreshAPI: base, states: tt.states}
			m := channelRefreshModel(t, api)
			m.rs.names = map[int64]string{1: "general", 2: "ops"}
			m.rs.unread["ops"] = 3
			m, _ = refreshWithReadState(t, m, tt.next)
			if len(m.rs.unread) != len(tt.wantUnread) {
				t.Errorf("unread = %v, want %v", m.rs.unread, tt.wantUnread)
			}
			for name, n := range tt.wantUnread {
				if m.rs.unread[name] != n {
					t.Errorf("unread[%q] = %d, want %d (all %v)", name, m.rs.unread[name], n, m.rs.unread)
				}
			}
			if m.rs.names[2] != "staff" {
				t.Errorf("read-state name for id 2 = %q, want staff", m.rs.names[2])
			}
		})
	}
}

// TestRefreshMakesNoSecondListRead: a refresh that finds a channel read state
// does not know yet must name it from its own answer, not read the list again
// beside the queued refresh.
func TestRefreshMakesNoSecondListRead(t *testing.T) {
	base := &channelRefreshAPI{answers: []channelListAnswer{
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}}},
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}, {ID: 4, Name: "help"}}},
		{channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}, {ID: 4, Name: "help"}}},
	}}
	api := &removedAPI{channelRefreshAPI: base, states: []schema.ReadStateV1{{ChannelID: 4, Unread: 2}}}
	m := channelRefreshModel(t, api)
	m.loadingChannels = true
	m.channelsAgain = true // a burst: one more read is queued
	m, cmd := update(m, channelsLoaded{refresh: true, channels: []schema.ChannelV0{{ID: 1, Name: "general"}, {ID: 2, Name: "ops"}, {ID: 4, Name: "help"}}})
	msgs := runCmd(cmd)
	// Initial load, plus exactly the one queued refresh.
	if got := base.listCalls(); got != 2 {
		t.Errorf("ListChannels calls = %d, want 2 (initial and the queued refresh)", got)
	}
	if n := len(msgsOf[channelsLoaded](msgs)); n != 1 {
		t.Errorf("queued refreshes = %d, want 1", n)
	}
	for _, rs := range msgsOf[readStateLoaded](msgs) {
		m, _ = update(m, rs)
	}
	if m.rs.unread["help"] != 2 {
		t.Errorf("unread = %v, want help named from the refresh's own answer", m.rs.unread)
	}
}
