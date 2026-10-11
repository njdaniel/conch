package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type stubAPI struct{}

func (stubAPI) ListMessagesV2(context.Context, string, int64, int) (schema.ListMessagesResponseV2, error) {
	return schema.ListMessagesResponseV2{}, nil
}
func (stubAPI) PostMessageV2(context.Context, string, int64, string, *schema.Audience) (schema.MessageV2, error) {
	return schema.MessageV2{}, nil
}
func (stubAPI) SubscribeV2(context.Context, string, func(schema.MessageV2) error) error { return nil }
func (stubAPI) ListNets(context.Context, string) (schema.ListNetsResponseV1, error) {
	return schema.ListNetsResponseV1{}, nil
}
func (stubAPI) ListChannels(context.Context) (schema.ListChannelsResponse, error) {
	return schema.ListChannelsResponse{}, nil
}
func (stubAPI) ListApprovals(context.Context) (schema.ListApprovalsResponseV1, error) {
	return schema.ListApprovalsResponseV1{}, nil
}
func (stubAPI) CastDecision(context.Context, int64, schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error) {
	return schema.CastDecisionResponseV1{}, nil
}

func (stubAPI) WhoAmI(context.Context) (schema.WhoAmIResponseV1, error) {
	return schema.WhoAmIResponseV1{}, nil
}

func TestModelUpdate(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name string
		msg  tea.Msg
		prep func(*Model)
		want func(t *testing.T, got Model)
	}{
		{name: "resize", msg: tea.WindowSizeMsg{Width: 80, Height: 24}, want: func(t *testing.T, got Model) {
			if got.width != 80 || got.height != 24 {
				t.Errorf("size = %dx%d", got.width, got.height)
			}
		}},
		{name: "type", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}, want: func(t *testing.T, got Model) {
			if got.input != "hi" {
				t.Errorf("input = %q", got.input)
			}
		}},
		{name: "type space", msg: tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, prep: func(m *Model) { m.input = "hi" }, want: func(t *testing.T, got Model) {
			if got.input != "hi " {
				t.Errorf("input = %q", got.input)
			}
		}},
		{name: "backspace unicode", msg: tea.KeyMsg{Type: tea.KeyBackspace}, prep: func(m *Model) { m.input = "a🐚" }, want: func(t *testing.T, got Model) {
			if got.input != "a" {
				t.Errorf("input = %q", got.input)
			}
		}},
		{name: "select next channel", msg: tea.KeyMsg{Type: tea.KeyDown}, want: func(t *testing.T, got Model) {
			if got.selected != 1 || got.current() != "ops" {
				t.Errorf("selected = %d (%s)", got.selected, got.current())
			}
		}},
		{name: "loaded merges duplicates", msg: messagesLoaded{channel: "general", messages: []schema.MessageV2{{ID: 2, Body: "new"}, {ID: 1, Body: "first"}}}, prep: func(m *Model) {
			m.messages["general"] = []schema.MessageV2{{ID: 2, Body: "old"}}
		}, want: func(t *testing.T, got Model) {
			messages := got.messages["general"]
			if len(messages) != 2 || messages[0].ID != 1 || messages[1].Body != "new" {
				t.Errorf("messages = %+v", messages)
			}
		}},
		{name: "load error", msg: messagesLoaded{channel: "general", err: errBoom}, want: func(t *testing.T, got Model) {
			if got.status() != "boom" {
				t.Errorf("status = %q", got.status())
			}
		}},
		{name: "send needs author", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) { m.input = "hello"; m.authorID = 0 }, want: func(t *testing.T, got Model) {
			if got.status() != "set CONCH_AUTHOR to send" || got.input != "hello" {
				t.Errorf("status/input = %q/%q", got.status(), got.input)
			}
		}},
		{name: "switch to inbox", msg: tea.KeyMsg{Type: tea.KeyTab}, want: func(t *testing.T, got Model) {
			if got.mode != modeInbox {
				t.Errorf("expected modeInbox, got %d", got.mode)
			}
		}},
		{name: "switch back to channels", msg: tea.KeyMsg{Type: tea.KeyTab}, prep: func(m *Model) { m.mode = modeInbox }, want: func(t *testing.T, got Model) {
			if got.mode != modeChannels {
				t.Errorf("expected modeChannels, got %d", got.mode)
			}
		}},
		{name: "enter decision mode", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) {
			m.mode = modeInbox
			m.approvals = []schema.ApprovalV1{{ID: 1}}
		}, want: func(t *testing.T, got Model) {
			if got.mode != modeDecision {
				t.Errorf("expected modeDecision, got %d", got.mode)
			}
		}},
		{name: "cancel decision mode", msg: tea.KeyMsg{Type: tea.KeyEsc}, prep: func(m *Model) {
			m.mode = modeDecision
		}, want: func(t *testing.T, got Model) {
			if got.mode != modeInbox {
				t.Errorf("expected modeInbox, got %d", got.mode)
			}
		}},
		{name: "cast decision needs reason", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) {
			m.mode = modeDecision
			m.approvals = []schema.ApprovalV1{{ID: 1, Options: []schema.Option{{ID: "opt1"}}}}
		}, want: func(t *testing.T, got Model) {
			if got.status() != "reason is required" {
				t.Errorf("expected 'reason is required', got %q", got.status())
			}
		}},
		// A slower ListApprovals response can land after the user has
		// already moved into modeDecision on a stale (nonempty) list — e.g.
		// re-entering the inbox re-triggers a load, the user presses enter
		// on the old list before it resolves, and the fresh response comes
		// back empty because the approval was resolved elsewhere meanwhile.
		// Regression test for a panic previously reachable this way.
		{name: "empty refresh while deciding falls back to inbox", msg: approvalsLoaded{approvals: nil}, prep: func(m *Model) {
			m.mode = modeDecision
			m.approvals = []schema.ApprovalV1{{ID: 1, Options: []schema.Option{{ID: "opt1"}}}}
			m.selApproval = 0
		}, want: func(t *testing.T, got Model) {
			if got.mode != modeInbox {
				t.Errorf("expected modeInbox after empty refresh, got %d", got.mode)
			}
		}},
		{name: "down in decision mode with no approvals does not panic", msg: tea.KeyMsg{Type: tea.KeyDown}, prep: func(m *Model) {
			m.mode = modeDecision
			m.approvals = nil
		}, want: func(t *testing.T, got Model) {
			if got.mode != modeDecision {
				t.Errorf("mode = %d", got.mode)
			}
		}},
		{name: "enter in decision mode with no approvals does not panic", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) {
			m.mode = modeDecision
			m.approvals = nil
			m.authorID = 7
			m.input = "reason"
		}, want: func(t *testing.T, got Model) {
			if got.mode != modeInbox {
				t.Errorf("expected fallback to modeInbox, got %d", got.mode)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := NewModel(context.Background(), stubAPI{}, 7, []string{"general", "ops"})
			if test.prep != nil {
				test.prep(&model)
			}
			updated, _ := model.Update(test.msg)
			got, ok := updated.(Model)
			if !ok {
				t.Fatalf("model type = %T", updated)
			}
			test.want(t, got)
		})
	}
}

// listAPI records channel-list, backfill and subscribe calls.
type listAPI struct {
	stubAPI
	resp      schema.ListChannelsResponse
	err       error
	mu        sync.Mutex
	lists     int
	listed    []string
	subscribe chan string
}

func (a *listAPI) ListChannels(context.Context) (schema.ListChannelsResponse, error) {
	a.mu.Lock()
	a.lists++
	a.mu.Unlock()
	return a.resp, a.err
}

func (a *listAPI) ListMessagesV2(_ context.Context, channel string, _ int64, _ int) (schema.ListMessagesResponseV2, error) {
	a.mu.Lock()
	a.listed = append(a.listed, channel)
	a.mu.Unlock()
	return schema.ListMessagesResponseV2{}, nil
}

func (a *listAPI) SubscribeV2(_ context.Context, channel string, _ func(schema.MessageV2) error) error {
	a.subscribe <- channel
	return nil
}

func (a *listAPI) listCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists
}

// runCmd executes cmd (flattening tea.Batch) and returns the messages produced.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestModelChannelsLoaded(t *testing.T) {
	ch := func(names ...string) []schema.ChannelV0 {
		out := make([]schema.ChannelV0, 0, len(names))
		for i, n := range names {
			out = append(out, schema.ChannelV0{ID: int64(i + 1), Name: n})
		}
		return out
	}
	tests := []struct {
		name       string
		msg        channelsLoaded
		wantList   []string
		wantStatus string
	}{
		{name: "populated", msg: channelsLoaded{channels: ch("ops", "dev")}, wantList: []string{"ops", "dev"}},
		{name: "api error", msg: channelsLoaded{err: errors.New("boom")}, wantList: []string{"general"}, wantStatus: "channel list: boom"},
		{name: "zero channels", msg: channelsLoaded{channels: []schema.ChannelV0{}}, wantList: []string{"general"}, wantStatus: "no channels on server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &listAPI{subscribe: make(chan string, 4)}
			model := NewModel(context.Background(), api, 7, nil)
			updated, cmd := model.Update(tt.msg)
			got := updated.(Model)
			if strings.Join(got.channels, ",") != strings.Join(tt.wantList, ",") || got.selected != 0 {
				t.Fatalf("channels = %v selected %d, want %v", got.channels, got.selected, tt.wantList)
			}
			if got.status() != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.status(), tt.wantStatus)
			}
			msgs := runCmd(cmd)
			var loaded messagesLoaded
			for _, m := range msgs {
				if l, ok := m.(messagesLoaded); ok {
					loaded = l
				}
			}
			if loaded.channel != tt.wantList[0] {
				t.Errorf("backfill channel = %q, want %q", loaded.channel, tt.wantList[0])
			}
			select {
			case sub := <-api.subscribe:
				if sub != tt.wantList[0] {
					t.Errorf("subscribed %q, want %q", sub, tt.wantList[0])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no subscription started")
			}
			// The notice must survive the first backfill's "connected".
			after, _ := got.Update(loaded)
			wantAfter := "connected"
			if tt.wantStatus != "" {
				wantAfter = tt.wantStatus
			}
			if s := after.(Model).status(); s != wantAfter {
				t.Errorf("status after backfill = %q, want %q", s, wantAfter)
			}
			// ...and the fallback channel's own errors, which would otherwise
			// hide why the fallback happened.
			failed, _ := got.Update(messagesLoaded{channel: tt.wantList[0], err: errors.New("channel not found")})
			wantFailed := "channel not found"
			if tt.wantStatus != "" {
				wantFailed = tt.wantStatus
			}
			if s := failed.(Model).status(); s != wantFailed {
				t.Errorf("status after failed backfill = %q, want %q", s, wantFailed)
			}
			ended, _ := got.Update(subscriptionEnded{channel: tt.wantList[0], err: errors.New("gone")})
			wantEnded := "live updates: reconnecting…"
			if tt.wantStatus != "" {
				wantEnded = tt.wantStatus
			}
			if s := ended.(Model).status(); s != wantEnded {
				t.Errorf("status after subscription end = %q, want %q", s, wantEnded)
			}
		})
	}
}

func TestModelInitChannelSource(t *testing.T) {
	t.Run("explicit list never calls ListChannels", func(t *testing.T) {
		api := &listAPI{subscribe: make(chan string, 4)}
		model := NewModel(context.Background(), api, 7, []string{"general", "ops"})
		// Batch order is backfill, subscribe, waitEvent; the subscription
		// goroutine ends immediately, which unblocks waitEvent.
		msgs := runCmd(model.Init())
		if n := api.listCalls(); n != 0 {
			t.Errorf("ListChannels called %d times, want 0", n)
		}
		var backfilled bool
		for _, m := range msgs {
			if l, ok := m.(messagesLoaded); ok && l.channel == "general" {
				backfilled = true
			}
		}
		if !backfilled {
			t.Errorf("no backfill for general in %#v", msgs)
		}
	})
	t.Run("empty list loads from API", func(t *testing.T) {
		api := &listAPI{resp: schema.ListChannelsResponse{Channels: []schema.ChannelV0{{ID: 1, Name: "ops"}}}, subscribe: make(chan string, 4)}
		model := NewModel(context.Background(), api, 7, []string{" ", ""})
		if !model.loadingChannels || len(model.channels) != 0 {
			t.Fatalf("model = %+v, want loading with no channels", model)
		}
		msg := model.loadChannels()()
		loaded, ok := msg.(channelsLoaded)
		if !ok || len(loaded.channels) != 1 || api.listCalls() != 1 {
			t.Fatalf("msg = %#v, calls = %d", msg, api.listCalls())
		}
	})
}

func TestModelEmptyChannelListIsSafe(t *testing.T) {
	keys := []tea.KeyMsg{
		{Type: tea.KeyUp}, {Type: tea.KeyDown}, {Type: tea.KeyTab}, {Type: tea.KeyTab},
		{Type: tea.KeyBackspace}, {Type: tea.KeyRunes, Runes: []rune("hi")}, {Type: tea.KeyEnter},
	}
	model := NewModel(context.Background(), stubAPI{}, 7, nil)
	var current tea.Model = model
	current, _ = current.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = current.View()
	for _, key := range keys {
		var cmd tea.Cmd
		current, cmd = current.Update(key)
		if key.Type == tea.KeyTab {
			_ = cmd // inbox load is a command; not executed here
		}
		_ = current.View()
	}
	// Send with no channels: status message, no command.
	m := model
	m.input = "hello"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("send with no channels must not issue a command")
	}
	if s := updated.(Model).status(); !strings.Contains(s, "no channel") {
		t.Errorf("status = %q, want no-channel message", s)
	}
	if !strings.Contains(m.View(), "loading") {
		t.Errorf("view should show loading:\n%s", m.View())
	}
}

func TestModelViewSmoke(t *testing.T) {
	model := NewModel(context.Background(), stubAPI{}, 7, []string{"general", "ops"})
	model.width, model.height = 80, 24
	model.messages["general"] = []schema.MessageV2{
		{ID: 1, AuthorID: 4, Body: "plain message"},
		{ID: 2, AuthorID: 5, Body: "rendered alert", Payload: &schema.Payload{Schema: "acme.alert.v1"}},
	}
	view := model.View()
	for _, want := range []string{"general", "ops", "plain message", "rendered alert", "acme.alert.v1", "> ", "enter send"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if lines := strings.Count(view, "\n") + 1; lines > 24 {
		t.Errorf("view has %d lines, want at most 24", lines)
	}
}

// identityAPI answers whoami and records the principal used to act.
type identityAPI struct {
	stubAPI
	who       schema.WhoAmIResponseV1
	whoErr    error
	mu        sync.Mutex
	sentAs    int64
	decidedAs int64
}

func (a *identityAPI) WhoAmI(context.Context) (schema.WhoAmIResponseV1, error) {
	return a.who, a.whoErr
}

func (a *identityAPI) PostMessageV2(_ context.Context, _ string, author int64, _ string, _ *schema.Audience) (schema.MessageV2, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sentAs = author
	return schema.MessageV2{}, nil
}

func (a *identityAPI) CastDecision(_ context.Context, _ int64, d schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decidedAs = d.PrincipalID
	return schema.CastDecisionResponseV1{}, nil
}

func TestModelWhoAmI(t *testing.T) {
	api := &identityAPI{who: schema.WhoAmIResponseV1{ID: 42, Kind: "human", Name: "nick", Role: schema.RoleOperator}}
	model := NewModel(context.Background(), api, 0, []string{"general"}).WithCredential()
	whoMsg := model.loadWhoAmI()()
	updated, _ := model.Update(whoMsg)
	got := updated.(Model)
	if got.authorID != 42 || got.userName != "nick" {
		t.Fatalf("author = %d name = %q, want 42 nick", got.authorID, got.userName)
	}
	got.width, got.height = 80, 24
	if view := got.View(); !strings.Contains(view, "nick") || !strings.Contains(view, "signed in as nick") {
		t.Errorf("status line missing signed-in name:\n%s", view)
	}

	// Sending uses the whoami id.
	got.input = "hello"
	_, cmd := got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(cmd)
	if api.sentAs != 42 {
		t.Errorf("sent as %d, want 42", api.sentAs)
	}

	// Deciding uses it too.
	got.mode = modeDecision
	got.approvals = []schema.ApprovalV1{{ID: 3, Options: []schema.Option{{ID: "approve", Label: "Approve"}}}}
	got.input = "because"
	_, cmd = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(cmd)
	if api.decidedAs != 42 {
		t.Errorf("decided as %d, want 42", api.decidedAs)
	}
}

func TestModelInitCallsWhoAmIOnlyWithCredential(t *testing.T) {
	for _, withCred := range []bool{true, false} {
		api := &identityAPI{who: schema.WhoAmIResponseV1{ID: 1, Name: "n"}}
		model := NewModel(context.Background(), api, 0, []string{"general"})
		if withCred {
			model = model.WithCredential()
		}
		var saw bool
		for _, m := range runCmd(model.Init()) {
			if _, ok := m.(whoAmILoaded); ok {
				saw = true
			}
		}
		if saw != withCred {
			t.Errorf("credential=%v: whoami issued = %v", withCred, saw)
		}
	}
}

func TestModelUnauthenticated(t *testing.T) {
	unauth := &cli.UnauthenticatedError{Server: "http://h:1"}
	hint := "not logged in to http://h:1: run 'conch login'"
	tests := []struct {
		name string
		mode mode // the mode the result belongs to, where the hint is visible
		msg  tea.Msg
	}{
		{"whoami", modeChannels, whoAmILoaded{err: unauth}},
		{"messages", modeChannels, messagesLoaded{channel: "general", err: unauth}},
		{"approvals", modeInbox, approvalsLoaded{err: unauth}},
		{"send", modeChannels, messageSent{err: unauth}},
		{"decision", modeInbox, decisionCast{err: unauth}},
		{"subscription", modeChannels, subscriptionEnded{channel: "general", err: unauth}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(context.Background(), stubAPI{}, 0, []string{"general"}).WithCredential()
			model.mode = tt.mode
			updated, _ := model.Update(tt.msg)
			got := updated.(Model)
			if got.status() != hint {
				t.Fatalf("status = %q, want %q", got.status(), hint)
			}
			// Still usable: keys are accepted and the view renders.
			got.mode = modeChannels
			next, _ := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
			if next.(Model).input != "x" {
				t.Error("model stopped accepting input after 401")
			}
			_ = next.View()
		})
	}

	t.Run("channel list", func(t *testing.T) {
		model := NewModel(context.Background(), stubAPI{}, 0, nil).WithCredential()
		updated, _ := model.Update(channelsLoaded{err: unauth})
		if got := updated.(Model).status(); got != hint {
			t.Errorf("status = %q, want %q", got, hint)
		}
	})

	t.Run("send before identity is known", func(t *testing.T) {
		model := NewModel(context.Background(), stubAPI{}, 0, []string{"general"}).WithCredential()
		model.input = "hi"
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil || !strings.Contains(updated.(Model).status(), "identity") {
			t.Errorf("status = %q cmd = %v", updated.(Model).status(), cmd)
		}
	})
}

func TestModelCredentialIgnoresSuppliedAuthor(t *testing.T) {
	api := &identityAPI{}
	model := NewModel(context.Background(), api, 7, []string{"general"}).WithCredential()
	if model.authorID != 0 {
		t.Fatalf("authorID = %d, want 0 after WithCredential", model.authorID)
	}
	model.input = "hello"
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("a command was issued before whoami returned")
	}
	if api.sentAs != 0 {
		t.Errorf("sent as %d before whoami returned; want nothing sent", api.sentAs)
	}
}

func TestModelIdentityStatusStates(t *testing.T) {
	hint := "not logged in to http://h:1: run 'conch login'"
	tests := []struct {
		name string
		who  *whoAmILoaded // nil = still pending
		want string
	}{
		{"pending", nil, "waiting for your identity"},
		{"401", &whoAmILoaded{err: &cli.UnauthenticatedError{Server: "http://h:1"}}, hint},
		{"other error", &whoAmILoaded{err: errors.New("boom")}, "whoami: boom"},
	}
	for _, tt := range tests {
		for _, md := range []mode{modeChannels, modeDecision} {
			t.Run(fmt.Sprintf("%s/mode%d", tt.name, md), func(t *testing.T) {
				var cur tea.Model = NewModel(context.Background(), stubAPI{}, 0, []string{"general"}).WithCredential()
				if tt.who != nil {
					cur, _ = cur.Update(*tt.who)
				}
				m := cur.(Model)
				m.mode = md
				m.input = "x"
				m.approvals = []schema.ApprovalV1{{ID: 1, Options: []schema.Option{{ID: "approve", Label: "A"}}}}
				updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				if cmd != nil {
					t.Error("nothing should be sent")
				}
				if got := updated.(Model).status(); !strings.Contains(got, tt.want) {
					t.Errorf("status = %q, want containing %q", got, tt.want)
				}
			})
		}
	}
}

// resubAPI counts backfills and subscriptions. Subscribe blocks until the
// context ends, like a healthy live stream.
type resubAPI struct {
	stubAPI
	mu         sync.Mutex
	backfills  map[string]int
	subscribes map[string]int
	started    chan string
}

func newResubAPI() *resubAPI {
	return &resubAPI{backfills: map[string]int{}, subscribes: map[string]int{}, started: make(chan string, 16)}
}

func (a *resubAPI) ListMessagesV2(_ context.Context, channel string, _ int64, _ int) (schema.ListMessagesResponseV2, error) {
	a.mu.Lock()
	a.backfills[channel]++
	a.mu.Unlock()
	return schema.ListMessagesResponseV2{}, nil
}

func (a *resubAPI) SubscribeV2(ctx context.Context, channel string, _ func(schema.MessageV2) error) error {
	a.mu.Lock()
	a.subscribes[channel]++
	a.mu.Unlock()
	a.started <- channel
	<-ctx.Done()
	return ctx.Err()
}

// run executes cmd (flattening batches) and returns the messages it produced.
// Only for commands that do not include waitEvent, which would block.
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

type recordedTimer struct {
	d   time.Duration
	msg tea.Msg
}

func resubModel(t *testing.T) (Model, *resubAPI, *[]recordedTimer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := newResubAPI()
	m := NewModel(ctx, api, 7, []string{"general", "ops"})
	timers := &[]recordedTimer{}
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		*timers = append(*timers, recordedTimer{d, msg})
		return func() tea.Msg { return nil }
	}
	return m, api, timers
}

func (a *resubAPI) counts(channel string) (backfills, subscribes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backfills[channel], a.subscribes[channel]
}

func awaitStart(t *testing.T, a *resubAPI, want string) {
	t.Helper()
	select {
	case got := <-a.started:
		if got != want {
			t.Fatalf("subscribed to %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no subscription started for %q", want)
	}
}

func update(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestSubscriptionEndedClearsFlag(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantFlag  bool
		wantTimer bool
		wantState string
	}{
		{"error", errors.New("eof"), false, true, "live updates: reconnecting…"},
		{"clean close", nil, false, true, "live updates: reconnecting…"},
		{"canceled", context.Canceled, true, false, ""},
		{"unauthenticated", &cli.UnauthenticatedError{Server: "http://h:1"}, false, false, "not logged in to http://h:1: run 'conch login'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, timers := resubModel(t)
			m, _ = update(m, subscriptionEnded{channel: "general", err: tt.err})
			if m.subscribed["general"] != tt.wantFlag {
				t.Errorf("subscribed = %v, want %v", m.subscribed["general"], tt.wantFlag)
			}
			if (len(*timers) == 1) != tt.wantTimer {
				t.Errorf("timers = %d, want timer=%v", len(*timers), tt.wantTimer)
			}
			if m.status() != tt.wantState {
				t.Errorf("status = %q, want %q", m.status(), tt.wantState)
			}
		})
	}
}

func TestResubscribeBackoff(t *testing.T) {
	m, _, timers := resubModel(t)
	var got []time.Duration
	// Each round: the subscription drops, the timer fires, and the
	// resubscription immediately drops again.
	for i := 0; i < 8; i++ {
		m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
		got = append(got, (*timers)[len(*timers)-1].d)
		m, _ = update(m, (*timers)[len(*timers)-1].msg)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Fatalf("delays = %v, want seconds %v", got, want)
		}
	}
}

func TestResubscribeStartsOneSubscriptionAndBackfills(t *testing.T) {
	m, api, timers := resubModel(t)
	m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
	if n, _ := api.counts("general"); n != 0 {
		t.Fatalf("backfilled before the delay elapsed: %d", n)
	}
	m, cmd := update(m, (*timers)[0].msg)
	run(cmd)
	awaitStart(t, api, "general")
	if b, s := api.counts("general"); b != 1 || s != 1 {
		t.Errorf("backfills=%d subscribes=%d, want 1 and 1", b, s)
	}
	if !m.subscribed["general"] || m.retryPending["general"] {
		t.Errorf("subscribed=%v pending=%v", m.subscribed["general"], m.retryPending["general"])
	}
	// A duplicate timer for the same drop is stale and must do nothing.
	_, cmd = update(m, (*timers)[0].msg)
	if cmd != nil {
		t.Error("stale resubscribeDue produced a command")
	}
}

func TestBackoffResetsAfterSuccessfulSubscription(t *testing.T) {
	tests := []struct {
		name  string
		heal  func(m Model) Model
		wantD time.Duration
	}{
		{"no recovery keeps growing", func(m Model) Model { return m }, 4 * time.Second},
		{"message received", func(m Model) Model {
			m, _ = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 1}})
			return m
		}, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, timers := resubModel(t)
			for i := 0; i < 2; i++ {
				m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
				m, _ = update(m, (*timers)[len(*timers)-1].msg)
			}
			m = tt.heal(m)
			update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
			if d := (*timers)[len(*timers)-1].d; d != tt.wantD {
				t.Errorf("delay = %v, want %v", d, tt.wantD)
			}
		})
	}
	t.Run("long-lived subscription", func(t *testing.T) {
		m, _, timers := resubModel(t)
		for i := 0; i < 2; i++ {
			m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
			m, _ = update(m, (*timers)[len(*timers)-1].msg)
		}
		update(m, subscriptionEnded{channel: "general", err: errors.New("eof"), lived: time.Minute})
		if d := (*timers)[len(*timers)-1].d; d != time.Second {
			t.Errorf("delay = %v, want 1s", d)
		}
	})
}

func TestNonSelectedChannelResubscribesOnSelect(t *testing.T) {
	m, api, timers := resubModel(t)
	m, cmd := update(m, subscriptionEnded{channel: "ops", err: errors.New("eof")})
	_ = cmd
	if len(*timers) != 0 {
		t.Fatalf("timer scheduled for a channel nobody is viewing")
	}
	if m.subscribed["ops"] {
		t.Fatal("flag not cleared")
	}
	m, cmd = update(m, tea.KeyMsg{Type: tea.KeyDown})
	run(cmd)
	awaitStart(t, api, "ops")
	if _, s := api.counts("ops"); s != 1 {
		t.Errorf("subscribes = %d, want 1", s)
	}
	// Moving away and back must not start another one.
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyUp})
	_, cmd = update(m, tea.KeyMsg{Type: tea.KeyDown})
	run(cmd)
	if _, s := api.counts("ops"); s != 1 {
		t.Errorf("subscribes after re-select = %d, want 1", s)
	}
}

func TestNoSecondSubscriptionWhileRetryPending(t *testing.T) {
	tests := []struct {
		name        string
		fireWhileOn bool
	}{
		{"timer fires after returning", true},
		{"timer fires while away", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, api, timers := resubModel(t)
			m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown})
			if !tt.fireWhileOn {
				m, _ = update(m, (*timers)[0].msg)
			}
			m, cmd := update(m, tea.KeyMsg{Type: tea.KeyUp})
			run(cmd)
			if tt.fireWhileOn {
				if _, s := api.counts("general"); s != 0 {
					t.Fatalf("selectChannel started a subscription under a pending timer")
				}
				_, cmd = update(m, (*timers)[0].msg)
				run(cmd)
			}
			awaitStart(t, api, "general")
			if _, s := api.counts("general"); s != 1 {
				t.Errorf("subscribes = %d, want exactly 1", s)
			}
		})
	}
}

// A backfill that completes while the selected channel's subscription is down
// and a retry is pending must not report "connected": the history loaded, the
// live feed did not. Once the retry has resubscribed, it may.
func TestBackfillDoesNotClaimConnectedWhileReconnecting(t *testing.T) {
	m, _, _ := resubModel(t)
	// The subscription fails at once; the first backfill is still in flight.
	m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
	if m.status() != statusReconnecting {
		t.Fatalf("status after the drop = %q", m.status())
	}
	m, _ = update(m, messagesLoaded{channel: "general", messages: []schema.MessageV2{{ID: 1, Body: "a"}}})
	if m.status() != statusReconnecting {
		t.Errorf("status after a backfill during the outage = %q, want %q", m.status(), statusReconnecting)
	}
	if len(m.messages["general"]) != 1 {
		t.Errorf("the backfill was not merged: %+v", m.messages["general"])
	}
	// The retry fires and resubscribes; its backfill may now say connected.
	m, _ = update(m, resubscribeDue{channel: "general"})
	m, _ = update(m, messagesLoaded{channel: "general"})
	if m.status() != "connected" {
		t.Errorf("status after the reconnect's backfill = %q, want connected", m.status())
	}
}

func TestStaleBackfillKeepsStatus(t *testing.T) {
	tests := []struct {
		name string
		msg  messagesLoaded
		want string
	}{
		{"stale success", messagesLoaded{channel: "general", messages: []schema.MessageV2{{ID: 1, Body: "a"}}}, "loading…"},
		{"stale error", messagesLoaded{channel: "general", err: errors.New("boom")}, "loading…"},
		{"selected success", messagesLoaded{channel: "ops"}, "connected"},
		{"selected error", messagesLoaded{channel: "ops", err: errors.New("boom")}, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := resubModel(t)
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown}) // ops, status "loading…"
			m, _ = update(m, tt.msg)
			if m.status() != tt.want {
				t.Errorf("status = %q, want %q", m.status(), tt.want)
			}
			if len(tt.msg.messages) > 0 && len(m.messages["general"]) != 1 {
				t.Errorf("stale messages not merged: %+v", m.messages["general"])
			}
		})
	}
}

// A result that belongs to one mode must not change the status line of
// another, and the mode must show its current state when the user returns.
func TestStatusBelongsToMode(t *testing.T) {
	key := func(k tea.KeyType) tea.Msg { return tea.KeyMsg{Type: k} }
	apps := approvalsLoaded{approvals: []schema.ApprovalV1{{ID: 1}}}
	eof := errors.New("eof")
	// A model with the "ops" backfill outstanding, as after selecting it.
	opsSelected := func(t *testing.T) Model {
		m, _, _ := resubModel(t)
		m, _ = update(m, key(tea.KeyDown))
		return m
	}
	noChannels := func(t *testing.T) Model {
		return NewModel(context.Background(), stubAPI{}, 7, nil)
	}
	// toDecision enters the inbox and opens a decision prompt.
	toDecision := []tea.Msg{key(tea.KeyTab), apps, key(tea.KeyEnter)}
	tab := []tea.Msg{key(tea.KeyTab)}

	tests := []struct {
		name  string
		setup func(t *testing.T) Model
		enter []tea.Msg // moves the user out of channels
		msg   tea.Msg   // the background result
		away  string    // visible status after msg, unchanged from before it
		back  []tea.Msg // returns the user to channels
		want  string    // status after the user returns to the mode (case 3: re-enters the inbox)
	}{
		{"1 messagesLoaded in inbox", opsSelected, tab,
			messagesLoaded{channel: "ops"}, "loading approvals…", tab, "connected"},
		{"1 messagesLoaded error in inbox", opsSelected, tab,
			messagesLoaded{channel: "ops", err: errors.New("boom")}, "loading approvals…", tab, "boom"},
		{"1 messagesLoaded in decision", opsSelected, toDecision,
			messagesLoaded{channel: "ops"}, "type reason to decide", []tea.Msg{key(tea.KeyEsc), key(tea.KeyTab)}, "connected"},
		{"2 subscriptionEnded in inbox", opsSelected, tab,
			subscriptionEnded{channel: "ops", err: eof}, "loading approvals…", tab, statusReconnecting},
		{"2 subscriptionEnded in decision", opsSelected, toDecision,
			subscriptionEnded{channel: "ops", err: eof}, "type reason to decide", []tea.Msg{key(tea.KeyEsc), key(tea.KeyTab)}, statusReconnecting},
		{"2 backfill during an outage keeps reconnecting", opsSelected,
			[]tea.Msg{subscriptionEnded{channel: "ops", err: eof}, key(tea.KeyTab)},
			messagesLoaded{channel: "ops"}, "loading approvals…", tab, statusReconnecting},
		{"3 approvalsLoaded after returning to channels", opsSelected,
			[]tea.Msg{key(tea.KeyTab), key(tea.KeyTab)},
			apps, "loading…", tab, "loading approvals…"},
		{"3 approvalsLoaded error after returning to channels", opsSelected,
			[]tea.Msg{key(tea.KeyTab), key(tea.KeyTab)},
			approvalsLoaded{err: errors.New("boom")}, "loading…", tab, "loading approvals…"},
		{"4 channelsLoaded error in inbox", noChannels, tab,
			channelsLoaded{err: errors.New("boom")}, "loading approvals…", tab, "channel list: boom"},
		{"4 channelsLoaded empty in inbox", noChannels, tab,
			channelsLoaded{channels: []schema.ChannelV0{}}, "loading approvals…", tab, "no channels on server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.setup(t)
			for _, msg := range tt.enter {
				m, _ = update(m, msg)
			}
			// In case 3 the user is already back in channels when the result lands.
			before := m.status()
			m, _ = update(m, tt.msg)
			if got := m.status(); got != tt.away || got != before {
				t.Errorf("visible status after the result = %q (before %q), want %q", got, before, tt.away)
			}
			for _, msg := range tt.back {
				m, _ = update(m, msg)
			}
			if got := m.status(); got != tt.want {
				t.Errorf("status after returning = %q, want %q", got, tt.want)
			}
		})
	}
}

// The inbox keeps its own last status while channel results land.
func TestInboxStatusSurvivesChannelResults(t *testing.T) {
	m, _, _ := resubModel(t)
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = update(m, approvalsLoaded{approvals: []schema.ApprovalV1{{ID: 1}}})
	if got := m.status(); got != "inbox loaded" {
		t.Fatalf("inbox status = %q", got)
	}
	m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
	m, _ = update(m, messagesLoaded{channel: "general"})
	if got := m.status(); got != "inbox loaded" {
		t.Errorf("inbox status after channel results = %q, want inbox loaded", got)
	}
}

// The decision prompt shares the inbox's status, so a slow approvals refresh
// must not replace the prompt the user is answering. An empty refresh still
// ends the decision, and a failed one is still shown.
func TestApprovalsRefreshKeepsTheDecisionPrompt(t *testing.T) {
	open := []schema.ApprovalV1{{ID: 1, Title: "deploy", Options: []schema.Option{{ID: "approve", Label: "Approve"}}}}
	tests := []struct {
		name     string
		msg      approvalsLoaded
		wantMode mode
		want     string
	}{
		{"refresh with approvals", approvalsLoaded{approvals: open}, modeDecision, "type reason to decide"},
		{"refresh came back empty", approvalsLoaded{}, modeInbox, "no open approvals"},
		{"refresh failed", approvalsLoaded{err: errors.New("boom")}, modeDecision, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := resubModel(t)
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyTab})
			m, _ = update(m, approvalsLoaded{approvals: open})
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyEnter})
			if m.mode != modeDecision || m.status() != "type reason to decide" {
				t.Fatalf("setup: mode %v status %q", m.mode, m.status())
			}
			m, _ = update(m, tt.msg)
			if m.mode != tt.wantMode || m.status() != tt.want {
				t.Errorf("mode %v status %q, want mode %v status %q", m.mode, m.status(), tt.wantMode, tt.want)
			}
		})
	}
}

// The real client must satisfy the TUI's API, v2 methods included.
var _ API = (*cli.Client)(nil)

// postCall is one PostMessageV2 the fake saw.
type postCall struct {
	channel  string
	author   int64
	body     string
	audience *schema.Audience
}

// scopeAPI is a fake server for the scoped-send tests. It records every call
// by method name; the v1 message methods are not on API at all, so a call to
// them cannot compile.
type scopeAPI struct {
	stubAPI
	mu      sync.Mutex
	calls   []string
	posts   []postCall
	roster  []schema.NetV1
	netsErr error
	postErr error
	nextID  int64
}

func (a *scopeAPI) record(name string) {
	a.mu.Lock()
	a.calls = append(a.calls, name)
	a.mu.Unlock()
}

func (a *scopeAPI) count(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (a *scopeAPI) ListNets(context.Context, string) (schema.ListNetsResponseV1, error) {
	a.record("ListNets")
	a.mu.Lock()
	defer a.mu.Unlock()
	return schema.ListNetsResponseV1{Nets: a.roster}, a.netsErr
}

func (a *scopeAPI) ListMessagesV2(context.Context, string, int64, int) (schema.ListMessagesResponseV2, error) {
	a.record("ListMessagesV2")
	return schema.ListMessagesResponseV2{}, nil
}

func (a *scopeAPI) SubscribeV2(context.Context, string, func(schema.MessageV2) error) error {
	a.record("SubscribeV2")
	return nil
}

func (a *scopeAPI) PostMessageV2(_ context.Context, channel string, author int64, body string, audience *schema.Audience) (schema.MessageV2, error) {
	a.record("PostMessageV2")
	a.mu.Lock()
	defer a.mu.Unlock()
	a.posts = append(a.posts, postCall{channel, author, body, audience})
	if a.postErr != nil {
		return schema.MessageV2{}, a.postErr
	}
	a.nextID++
	return schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 100 + a.nextID, AuthorID: author, Body: body, Audience: audience}, nil
}

func (a *scopeAPI) postCalls() []postCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]postCall(nil), a.posts...)
}

func (a *scopeAPI) callNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// scopeRoster has alpha (viewer 7 transmits) and bravo (viewer 7 monitors).
func scopeRoster() []schema.NetV1 {
	return []schema.NetV1{
		{ID: 3, Name: "alpha", Members: []schema.NetMember{{PrincipalID: 7, Role: schema.NetRoleMember}, {PrincipalID: 9, Role: schema.NetRoleMember}}},
		{ID: 4, Name: "bravo", Members: []schema.NetMember{{PrincipalID: 7, Role: schema.NetRoleMonitor}, {PrincipalID: 9, Role: schema.NetRoleMember}}},
	}
}

// scopeModel is a model on channel "ops" (then "general") as principal 7, with
// the roster already loaded when loaded is true.
func scopeModel(api *scopeAPI, loaded bool) Model {
	m := NewModel(context.Background(), api, 7, []string{"ops", "general"})
	if loaded {
		m.nets["ops"] = api.roster
	}
	return m
}

// enter types text into the input and presses enter, then feeds every result
// of the resulting command back into the model, like the runtime would.
func enter(m Model, text string) Model {
	m.input = text
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	for _, msg := range run(cmd) {
		m, _ = update(m, msg)
	}
	return m
}

func TestTargetStateMachine(t *testing.T) {
	type step struct {
		input      string // typed then entered; empty with key set instead
		key        tea.KeyType
		wantPrompt string
		wantStatus string // substring
	}
	tests := []struct {
		name   string
		roster []schema.NetV1 // the server's; the model starts with it unless stale
		stale  bool           // the model's roster starts empty
		netErr error
		steps  []step
	}{
		{name: "net sets the target, bare /net clears it", roster: scopeRoster(), steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >", wantStatus: "transmitting to net alpha"},
			{input: "/net", wantPrompt: "ops >", wantStatus: "whole channel"},
		}},
		{name: "channel switch resets the target", roster: scopeRoster(), steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >"},
			{key: tea.KeyDown, wantPrompt: "general >"},
			{key: tea.KeyUp, wantPrompt: "ops >"},
		}},
		{name: "whisper leaves the target unchanged", roster: scopeRoster(), steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >"},
			{input: "/w 9 psst", wantPrompt: "ops/alpha >"},
			{input: "hello", wantPrompt: "ops/alpha >"},
		}},
		{name: "unknown net leaves the target unchanged", roster: scopeRoster(), steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >"},
			{input: "/net nope", wantPrompt: "ops/alpha >", wantStatus: `no net "nope"`},
		}},
		{name: "unknown net from the channel stays on the channel", roster: scopeRoster(), steps: []step{
			{input: "/net nope", wantPrompt: "ops >", wantStatus: `no net "nope"`},
		}},
		{name: "a net you only monitor cannot be a target", roster: scopeRoster(), steps: []step{
			{input: "/net bravo", wantPrompt: "ops >", wantStatus: "only monitor net bravo"},
		}},
		{name: "monitor refusal keeps the previous target", roster: scopeRoster(), steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >"},
			{input: "/net bravo", wantPrompt: "ops/alpha >", wantStatus: "only monitor"},
		}},
		{name: "a net created after startup is found by reloading", roster: scopeRoster(), stale: true, steps: []step{
			{input: "/net alpha", wantPrompt: "ops/alpha >", wantStatus: "transmitting to net alpha"},
		}},
		{name: "roster load failure leaves the target unchanged", netErr: errors.New("boom"), stale: true, steps: []step{
			{input: "/net alpha", wantPrompt: "ops >", wantStatus: "boom"},
		}},
		{name: "invalid net name is rejected without a lookup", roster: scopeRoster(), steps: []step{
			{input: "/net Bad!", wantPrompt: "ops >", wantStatus: "/net:"},
			{input: "/net a b", wantPrompt: "ops >", wantStatus: "usage"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &scopeAPI{roster: tt.roster, netsErr: tt.netErr}
			m := scopeModel(api, !tt.stale)
			for i, s := range tt.steps {
				if s.key != 0 {
					m, _ = update(m, tea.KeyMsg{Type: s.key})
				} else {
					m = enter(m, s.input)
				}
				got := m.prompt()
				if got != s.wantPrompt {
					t.Errorf("step %d (%q): prompt = %q, want %q", i, s.input, got, s.wantPrompt)
				}
				if strings.TrimSpace(got) == "" || strings.TrimSpace(got) == ">" {
					t.Errorf("step %d: prompt is blank: %q", i, got)
				}
				if s.wantStatus != "" && !strings.Contains(m.status(), s.wantStatus) {
					t.Errorf("step %d (%q): status = %q, want containing %q", i, s.input, m.status(), s.wantStatus)
				}
			}
		})
	}
}

func TestWhisperKeepsTargetOnTheNextSend(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "/net alpha")
	m = enter(m, "/w 9 psst")
	enter(m, "hello")
	posts := api.postCalls()
	if len(posts) != 2 || posts[0].audience.Kind != schema.AudienceKindPrincipals || posts[1].audience.Kind != schema.AudienceKindNet {
		t.Errorf("posts = %+v", posts)
	}
}

func TestStaleRosterReloadsOnce(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, false)
	m = enter(m, "/net alpha")
	if n := api.count("ListNets"); n != 1 {
		t.Errorf("ListNets calls = %d, want 1", n)
	}
	// Now cached: no second lookup.
	m = enter(m, "/net")
	enter(m, "/net alpha")
	if n := api.count("ListNets"); n != 1 {
		t.Errorf("ListNets calls after cached lookup = %d, want 1", n)
	}
}

func TestPromptShownInView(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m.width, m.height = 80, 24
	if !strings.Contains(m.View(), "ops > ") {
		t.Errorf("channel prompt missing:\n%s", m.View())
	}
	m = enter(m, "/net alpha")
	m.input = "typed"
	if v := m.View(); !strings.Contains(v, "ops/alpha > typed") {
		t.Errorf("net prompt missing:\n%s", v)
	}
	empty := NewModel(context.Background(), api, 7, nil)
	if got := empty.prompt(); strings.TrimSpace(got) == "" || got == ">" {
		t.Errorf("prompt with no channel = %q", got)
	}
}

func TestSendAudience(t *testing.T) {
	tests := []struct {
		name  string
		setup string // a command entered first
		input string
		want  *schema.Audience
		body  string
	}{
		{"channel", "", "hello", nil, "hello"},
		{"net", "/net alpha", "hello", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}, "hello"},
		{"whisper", "", "/w 9,11 hi there", &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{9, 11}}, "hi there"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := scopeModel(api, true)
			if tt.setup != "" {
				m = enter(m, tt.setup)
			}
			m = enter(m, tt.input)
			posts := api.postCalls()
			if len(posts) != 1 {
				t.Fatalf("posts = %d, want exactly 1", len(posts))
			}
			p := posts[0]
			if p.channel != "ops" || p.author != 7 || p.body != tt.body || fmt.Sprint(p.audience) != fmt.Sprint(tt.want) {
				t.Errorf("post = %+v (audience %+v), want body %q audience %+v", p, p.audience, tt.body, tt.want)
			}
			if m.input != "" {
				t.Errorf("input after accepted send = %q", m.input)
			}
			if got := len(m.messages["ops"]); got != 1 {
				t.Fatalf("messages after send = %d, want 1", got)
			}
			// The server also pushes the message on the subscription: shown once.
			echoed := m.messages["ops"][0]
			m, _ = update(m, messageReceived{channel: "ops", message: echoed})
			if got := len(m.messages["ops"]); got != 1 {
				t.Errorf("messages after echo = %d, want 1", got)
			}
		})
	}
}

func TestSentMessageCarriesItsBadge(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "/net alpha")
	m = enter(m, "on the net")
	line := m.messageLine("ops", m.messages["ops"][0], 60)
	if !strings.HasPrefix(line, "[net:alpha] 7") || !strings.Contains(line, "on the net") {
		t.Errorf("line = %q", line)
	}
}

func TestRefusedSend(t *testing.T) {
	for _, code := range []string{"forbidden", "net_not_found", "invalid_audience"} {
		for _, input := range []string{"typed text", "/w 9 typed text"} {
			t.Run(code+" "+input, func(t *testing.T) {
				api := &scopeAPI{roster: scopeRoster(), postErr: fmt.Errorf("cli: server error %s: nope", code)}
				m := scopeModel(api, true)
				m = enter(m, "/net alpha")
				m = enter(m, input)
				posts := api.postCalls()
				if len(posts) != 1 {
					t.Fatalf("posts = %d, want exactly 1 (no fallback, no retry)", len(posts))
				}
				if posts[0].audience == nil {
					t.Errorf("a scoped send went out channel-wide")
				}
				if m.input != input {
					t.Errorf("input = %q, want it kept as %q", m.input, input)
				}
				if !strings.Contains(m.status(), code) {
					t.Errorf("status = %q, want the server's error", m.status())
				}
				if len(m.messages["ops"]) != 0 {
					t.Errorf("a refused message was displayed: %+v", m.messages["ops"])
				}
				if m.sending {
					t.Error("still marked as sending after the refusal")
				}
				if m.prompt() != "ops/alpha >" {
					t.Errorf("prompt = %q, want the target unchanged", m.prompt())
				}
			})
		}
	}
}

func TestArchivedTargetIsRefusedByServer(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "/net alpha")
	api.postErr = errors.New("cli: server error net_not_found: net archived")
	m = enter(m, "still there?")
	if len(api.postCalls()) != 1 || m.input != "still there?" || !strings.Contains(m.status(), "net_not_found") {
		t.Errorf("posts %d input %q status %q", len(api.postCalls()), m.input, m.status())
	}
	// The target is kept (resetting is optional); nothing went channel-wide.
	if p := api.postCalls()[0]; p.audience == nil {
		t.Error("send went channel-wide")
	}
	if m.prompt() != "ops/alpha >" {
		t.Errorf("prompt = %q", m.prompt())
	}
}

func TestWhisperParsing(t *testing.T) {
	tests := []struct {
		input   string
		wantIDs []int64
		wantBdy string
		wantErr string // substring of the status; empty means it must send
	}{
		{input: "/w 3 hello", wantIDs: []int64{3}, wantBdy: "hello"},
		{input: "/w 3,5 hello there", wantIDs: []int64{3, 5}, wantBdy: "hello there"},
		{input: "/w   3,5    spaced   out", wantIDs: []int64{3, 5}, wantBdy: "spaced   out"},
		{input: "/w", wantErr: "usage"},
		{input: "/w 3", wantErr: "usage"},
		{input: "/w hello", wantErr: "usage"},
		{input: "/w x hello", wantErr: "not a positive principal id"},
		{input: "/w 3,x hello", wantErr: "not a positive principal id"},
		{input: "/w 3,,5 hello", wantErr: "not a positive principal id"},
		{input: "/w 0 hello", wantErr: "not a positive principal id"},
		{input: "/w -2 hello", wantErr: "not a positive principal id"},
		{input: "/w 3,3 hello", wantErr: "listed twice"},
		{input: "/w 7 hello", wantErr: "someone other than you"},
		{input: "/w3 hello", wantErr: "unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := enter(scopeModel(api, true), tt.input)
			posts := api.postCalls()
			if tt.wantErr != "" {
				if len(posts) != 0 {
					t.Fatalf("sent %+v", posts)
				}
				if !strings.Contains(m.status(), tt.wantErr) || m.input != tt.input {
					t.Errorf("status %q input %q", m.status(), m.input)
				}
				return
			}
			if len(posts) != 1 || posts[0].body != tt.wantBdy || posts[0].audience.Kind != schema.AudienceKindPrincipals ||
				fmt.Sprint(posts[0].audience.PrincipalIDs) != fmt.Sprint(tt.wantIDs) {
				t.Errorf("posts = %+v", posts)
			}
		})
	}
}

func TestUnknownCommandAndLiteralSlash(t *testing.T) {
	for _, input := range []string{"/foo bar", "/nets", "/", "/NET alpha"} {
		t.Run("unknown "+input, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := enter(scopeModel(api, true), input)
			if len(api.postCalls()) != 0 {
				t.Fatalf("sent %+v", api.postCalls())
			}
			if !strings.Contains(m.status(), "unknown command") || !strings.Contains(m.status(), "//") || m.input != input {
				t.Errorf("status %q input %q", m.status(), m.input)
			}
		})
	}
	literal := []struct{ input, want string }{{"//text", "/text"}, {"//net alpha", "/net alpha"}, {"// spaced", "/ spaced"}}
	for _, tt := range literal {
		t.Run("literal "+tt.input, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := scopeModel(api, true)
			m = enter(m, "/net alpha")
			enter(m, tt.input)
			posts := api.postCalls()
			if len(posts) != 1 || posts[0].body != tt.want {
				t.Fatalf("posts = %+v, want body %q", posts, tt.want)
			}
			// A literal message still goes to the current target.
			if posts[0].audience == nil || posts[0].audience.NetID != 3 {
				t.Errorf("audience = %+v", posts[0].audience)
			}
		})
	}
}

func TestAuditLogNotice(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "hello")
	if strings.Contains(m.status(), "audit log") {
		t.Errorf("plain send shows the notice: %q", m.status())
	}
	api.postErr = errors.New("cli: server error forbidden: no")
	m = enter(m, "/w 9 refused")
	if strings.Contains(m.status(), "audit log") || m.whisperNoted {
		t.Errorf("a refused whisper used up the notice: %q", m.status())
	}
	api.postErr = nil
	m = enter(m, "/w 9 first")
	if !strings.Contains(m.status(), "note: whispers are recorded in the audit log") {
		t.Errorf("first whisper status = %q", m.status())
	}
	m = enter(m, "/w 9 second")
	if m.status() != "sent" {
		t.Errorf("second whisper status = %q", m.status())
	}
}

func TestDoubleEnterSendsOnce(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m.input = "hi"
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	_, again := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if again != nil {
		t.Error("second enter produced a command while the first was in flight")
	}
	run(cmd)
	if len(api.postCalls()) != 1 {
		t.Errorf("posts = %d", len(api.postCalls()))
	}
}

func TestInputTypedDuringSendSurvives(t *testing.T) {
	tests := []struct {
		name   string
		during string // what the input holds when the post comes back
		want   string
	}{
		{"nothing typed since", "first", ""},
		// What was sent must not stay in the box to be sent a second time.
		{"typed on after enter", "first and more", " and more"},
		{"replaced with something else", "another thought", "another thought"},
		{"edited so it no longer starts with what was sent", "firs", "firs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := scopeModel(api, true)
			m.input = "first"
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(Model)
			m.input = tt.during // typed while the post is in flight
			for _, msg := range run(cmd) {
				m, _ = update(m, msg)
			}
			if m.input != tt.want {
				t.Errorf("input = %q, want %q", m.input, tt.want)
			}
			if len(api.posts) != 1 || api.posts[0].body != "first" {
				t.Errorf("posts = %+v, want the one message as entered", api.posts)
			}
		})
	}
}

func TestMessagesUseOnlyV2(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	run(m.loadCurrent())
	run(m.startSubscription())
	deadline := time.Now().Add(2 * time.Second)
	for api.count("SubscribeV2") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	enter(m, "hi")
	for _, name := range []string{"ListMessagesV2", "SubscribeV2", "PostMessageV2"} {
		if api.count(name) != 1 {
			t.Errorf("%s calls = %d, want 1", name, api.count(name))
		}
	}
	// ListMessages, SendMessage and Subscribe are not part of API, so the
	// TUI cannot call them; the compile-time check above ties API to the
	// real client.
	for _, c := range api.callNames() {
		if !strings.HasSuffix(c, "V2") && c != "ListNets" {
			t.Errorf("unexpected call %s", c)
		}
	}
}

func TestNetsLoadedUpdatesRoster(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, false)
	var got []netsLoaded
	for _, msg := range run(m.loadNets(false, "")) {
		if nl, ok := msg.(netsLoaded); ok {
			got = append(got, nl)
		}
	}
	if len(got) != 1 || got[0].channel != "ops" || len(got[0].nets) != 2 {
		t.Fatalf("netsLoaded = %+v", got)
	}
	m, _ = update(m, got[0])
	if len(m.nets["ops"]) != 2 {
		t.Errorf("roster = %+v", m.nets)
	}
	// A failed plain refresh keeps the old roster and is silent.
	before := m.status()
	m, _ = update(m, netsLoaded{channel: "ops", err: errors.New("boom")})
	if len(m.nets["ops"]) != 2 || m.status() != before {
		t.Errorf("failed refresh changed state: %+v %q", m.nets, m.status())
	}
}

func TestScopeBadges(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	net := &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}
	whisper := schema.MessageV2{ID: 4, AuthorID: 4, Body: "x", Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{4, 7, 9}}}
	tests := []struct {
		name string
		msg  schema.MessageV2
		want string // exact prefix of the line
	}{
		{"channel-wide", schema.MessageV2{ID: 1, AuthorID: 4, Body: "plain"}, "4  plain"},
		{"net by name", schema.MessageV2{ID: 2, AuthorID: 4, Body: "x", Audience: net}, "[net:alpha] 4  x"},
		{"net unknown to the roster shows its id", schema.MessageV2{ID: 3, AuthorID: 4, Body: "x", Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 42}}, "[net:42] 4  x"},
		{"whisper lists the others", whisper, "[whisper:4,9] 4  x"},
		{"unknown kind is still scoped", schema.MessageV2{ID: 5, AuthorID: 4, Body: "x", Audience: &schema.Audience{Kind: "future"}}, "[future] 4  x"},
		{"payload badge after author", schema.MessageV2{ID: 6, AuthorID: 4, Body: "x", Payload: &schema.Payload{Schema: "acme.alert.v1"}}, "4 [acme.alert.v1]  x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.messageLine("ops", tt.msg, 70); !strings.HasPrefix(got, tt.want) {
				t.Errorf("line = %q, want prefix %q", got, tt.want)
			}
		})
	}
	// Without knowing who the viewer is, a whisper shows every id.
	m.authorID = 0
	if got := m.messageLine("ops", whisper, 70); !strings.HasPrefix(got, "[whisper:4,7,9] ") {
		t.Errorf("anonymous viewer line = %q", got)
	}
}

func TestViewOrdersMessagesAndBadges(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m.width, m.height = 100, 24
	m.messages["ops"] = mergeMessages(nil, []schema.MessageV2{
		{ID: 3, AuthorID: 9, Body: "third", Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{7, 9}}},
		{ID: 1, AuthorID: 4, Body: "first"},
		{ID: 2, AuthorID: 9, Body: "second", Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}},
	})
	view := m.View()
	i1, i2, i3 := strings.Index(view, "first"), strings.Index(view, "second"), strings.Index(view, "third")
	if i1 < 0 || i1 >= i2 || i2 >= i3 {
		t.Fatalf("order wrong (%d %d %d):\n%s", i1, i2, i3, view)
	}
	for _, want := range []string{"[net:alpha]", "[whisper:9]"} {
		if strings.Count(view, want) != 1 {
			t.Errorf("view should show %s exactly once:\n%s", want, view)
		}
	}
}

// paneLines returns the text of the message pane (the right-hand box) for
// every row of the view that has one, borders and padding removed.
func paneLines(view string) []string {
	var out []string
	for _, line := range strings.Split(view, "\n") {
		parts := strings.Split(line, "│")
		if len(parts) >= 5 {
			out = append(out, strings.TrimSpace(parts[3]))
		}
	}
	return out
}

func TestForgedMarkersInBodies(t *testing.T) {
	forged := []struct{ name, body string }{
		{"net marker", "[net:alpha] hi"},
		{"whisper marker", "[whisper:3,7] hi"},
		{"newline then marker", "ok\n[net:alpha] hi"},
		{"carriage return", "ok\r[whisper:3,7] hi"},
		{"ansi colour", "\x1b[33m[net:alpha]\x1b[0m hi"},
		{"ansi clear and home", "\x1b[2J\x1b[Hhi"},
		{"c1 control", "\u009b31m[net:alpha] hi"},
		{"payload lookalike", "[acme.alert.v1] hi"},
	}
	api := &scopeAPI{roster: scopeRoster()}
	for _, tt := range forged {
		t.Run(tt.name, func(t *testing.T) {
			m := scopeModel(api, true)
			m.width, m.height = 100, 24
			m.messages["ops"] = []schema.MessageV2{{ID: 1, AuthorID: 4, Body: tt.body}}
			line := m.messageLine("ops", m.messages["ops"][0], 80)
			if strings.ContainsAny(line, "\n\r\x1b") || strings.ContainsRune(line, '\u009b') {
				t.Fatalf("line has a control character: %q", line)
			}
			if !strings.HasPrefix(line, "4  ") {
				t.Errorf("channel-wide line must start with the author id, got %q", line)
			}
			if strings.Contains(line, "  [") {
				t.Errorf("body can open with a bare [ marker: %q", line)
			}
			// In the whole view the message is one pane row, and no row opens with a badge.
			view := m.View()
			rows := 0
			for _, l := range paneLines(view) {
				if strings.HasPrefix(l, "[") {
					t.Errorf("pane row starts with a badge: %q", l)
				}
				if strings.HasPrefix(l, "4 ") {
					rows++
				}
			}
			if rows != 1 {
				t.Errorf("message rendered on %d pane rows, want 1:\n%s", rows, view)
			}
		})
	}
	t.Run("a real badge is distinguishable from a forged one", func(t *testing.T) {
		m := scopeModel(api, true)
		real := m.messageLine("ops", schema.MessageV2{ID: 1, AuthorID: 4, Body: "hi", Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}}, 80)
		fake := m.messageLine("ops", schema.MessageV2{ID: 2, AuthorID: 4, Body: "[net:alpha] hi"}, 80)
		if !strings.HasPrefix(real, "[net:alpha] ") || strings.HasPrefix(fake, "[") || real == fake {
			t.Errorf("real %q fake %q", real, fake)
		}
	})
	t.Run("hostile names from the server", func(t *testing.T) {
		m := scopeModel(api, true)
		m.nets["ops"] = []schema.NetV1{{ID: 3, Name: "al\x1b[31mpha\n"}}
		line := m.messageLine("ops", schema.MessageV2{ID: 1, AuthorID: 4, Body: "x", Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}}, 80)
		if strings.ContainsAny(line, "\n\x1b") {
			t.Errorf("line = %q", line)
		}
	})
}

// A prompt that has to be shortened loses channel characters, never the net:
// "ops-with-a-long-na…" would read as the whole channel.
func TestPromptKeepsTheNetWhenClipped(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "/net alpha")
	m.target.channel = strings.Repeat("c", 60)
	for _, width := range []int{200, 80, 40, 20, 12, 8, 2} {
		got := m.promptWithin(width)
		if !strings.HasSuffix(got, "/alpha >") {
			t.Errorf("width %d: prompt %q lost the net", width, got)
		}
		if width >= 12 && utf8.RuneCountInString(got) > width {
			t.Errorf("width %d: prompt %q is %d cells", width, got, utf8.RuneCountInString(got))
		}
	}
	// Without a target the prompt is just clipped.
	m.target = nil
	if got := m.promptWithin(4); utf8.RuneCountInString(got) > 4 || got == "" {
		t.Errorf("channel prompt clipped to 4 = %q", got)
	}
	// In the rendered view too.
	m = enter(m, "/net alpha")
	m.channels[m.selected] = strings.Repeat("c", 60)
	m.target.channel = m.channels[m.selected]
	m.width, m.height = 60, 24
	if v := m.View(); !strings.Contains(v, "/alpha > ") {
		t.Errorf("the view's prompt lost the net:\n%s", v)
	}
}

// Only the lookup the user last asked for may change the target. "/net alpha"
// that has to ask the server, then "/net" (or a switch, or another name)
// before the answer arrives: the late answer must not move the target.
func TestLateNetLookupDoesNotChangeTheTarget(t *testing.T) {
	late := netsLoaded{channel: "ops", nets: scopeRoster(), resolve: true, want: "alpha"}
	tests := []struct {
		name       string
		then       func(m Model) Model // what the user does before the answer arrives
		wantPrompt string
	}{
		{"nothing: the lookup completes", func(m Model) Model { return m }, "ops/alpha >"},
		{"/net back to the channel", func(m Model) Model { return enter(m, "/net") }, "ops >"},
		{"switched channel and back", func(m Model) Model {
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown})
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyUp})
			return m
		}, "ops >"},
		{"asked for another net that is known", func(m Model) Model {
			m.nets["ops"] = []schema.NetV1{{ID: 9, Name: "zulu", Members: []schema.NetMember{{PrincipalID: 7, Role: schema.NetRoleMember}}}}
			return enter(m, "/net zulu")
		}, "ops/zulu >"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &scopeAPI{roster: scopeRoster()}
			m := scopeModel(api, false) // roster not loaded: /net alpha must ask
			m.input = "/net alpha"
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // the lookup is now in flight
			m = next.(Model)
			if m.prompt() != "ops >" {
				t.Fatalf("prompt while looking up = %q", m.prompt())
			}
			m = tt.then(m)
			m, _ = update(m, late)
			if got := m.prompt(); got != tt.wantPrompt {
				t.Errorf("prompt after the late answer = %q, want %q", got, tt.wantPrompt)
			}
			// The roster is refreshed either way.
			if _, ok := findNet(m.nets["ops"], "alpha"); !ok {
				t.Error("the late answer did not refresh the roster")
			}
		})
	}
}

// A channel list that arrives while a target is set (the channel on screen can
// change under the user) resets the target, like a channel switch.
func TestChannelListResetsTheTarget(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	m = enter(m, "/net alpha")
	if m.prompt() != "ops/alpha >" {
		t.Fatalf("prompt = %q", m.prompt())
	}
	m.loadingChannels = true
	m, _ = update(m, channelsLoaded{channels: []schema.ChannelV0{{ID: 2, Name: "general"}, {ID: 1, Name: "ops"}}})
	if got := m.prompt(); strings.Contains(got, "alpha") || got != m.current()+" >" {
		t.Errorf("prompt after the channel list changed = %q on channel %q", got, m.current())
	}
}

// A scoped message always has a badge, even when its audience kind is empty or
// blank: an empty badge would render it as a message to the whole channel.
func TestScopedMessageWithoutAKindStillHasABadge(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster()}
	m := scopeModel(api, true)
	for _, tt := range []struct {
		kind schema.AudienceKind
		want string // the badge, "" for "any non-empty one"
	}{{"", "[scoped]"}, {" ", "[scoped]"}, {"\t", "[scoped]"}, {"\x1b[2J", ""}, {"\n", ""}, {"future-kind", "[future-kind]"}} {
		line := m.messageLine("ops", schema.MessageV2{ID: 1, AuthorID: 4, Body: "hi", Audience: &schema.Audience{Kind: tt.kind}}, 80)
		if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[]") || !strings.Contains(line, "] 4  hi") {
			t.Errorf("kind %q: line %q, want a badge before the author", tt.kind, line)
		}
		if tt.want != "" && !strings.HasPrefix(line, tt.want+" ") {
			t.Errorf("kind %q: line %q, want badge %s", tt.kind, line, tt.want)
		}
		if strings.ContainsAny(line, "\x1b\n\r\t") {
			t.Errorf("kind %q: line %q carries a control character", tt.kind, line)
		}
	}
}

// Error text from the server is shown in the status line without control
// characters, like everything else that comes from outside.
func TestServerErrorTextIsSanitized(t *testing.T) {
	api := &scopeAPI{roster: scopeRoster(), postErr: errors.New("cli: server error forbidden: no\x1b[2J\nsecond line")}
	m := scopeModel(api, true)
	m = enter(m, "hello")
	status := m.channelStatus
	if strings.ContainsAny(status, "\x1b\n\r") || !strings.Contains(status, "forbidden") {
		t.Errorf("status = %q", status)
	}
	if m.input != "hello" {
		t.Errorf("input = %q, want it kept after the refusal", m.input)
	}
}

// A user name from whoami is sanitized before drawing in the status line:
// escape sequences, newlines, and bidi overrides are stripped or replaced
// so they cannot drive the terminal or break line layout.
func TestUserNameIsSanitizedInView(t *testing.T) {
	hostile := "\x1b[31muser\n\u202ename"
	api := &identityAPI{who: schema.WhoAmIResponseV1{ID: 42, Kind: "human", Name: hostile, Role: schema.RoleOperator}}
	model := NewModel(context.Background(), api, 0, []string{"general"}).WithCredential()
	whoMsg := model.loadWhoAmI()()
	updated, _ := model.Update(whoMsg)
	m := updated.(Model)
	m.width, m.height = 120, 24

	view := m.View()
	// Assert against the hostile sequence with its context, not a bare ESC:
	// lipgloss legitimately emits escape codes when styling is on.
	if strings.Contains(view, "\x1b[31muser") {
		t.Errorf("rendered view contains the raw escape sequence from the name:\n%s", view)
	}
	if strings.Contains(view, "\u202e") {
		t.Errorf("rendered view contains raw bidi override:\n%s", view)
	}

	lines := strings.Split(view, "\n")
	if len(lines) != 24 {
		t.Errorf("view has %d lines, want 24 (line layout broken):\n%s", len(lines), view)
	}

	statusLine := lines[len(lines)-1]
	if !strings.Contains(statusLine, "[31muser ↵ name | signed in as [31muser ↵ name") {
		t.Errorf("status line = %q, want sanitized username and status", statusLine)
	}

	// Compare with clean baseline: hostile username preserves line count.
	cleanModel := NewModel(context.Background(), &identityAPI{who: schema.WhoAmIResponseV1{ID: 42, Kind: "human", Name: "clean", Role: schema.RoleOperator}}, 0, []string{"general"}).WithCredential()
	cleanWho := cleanModel.loadWhoAmI()()
	updatedClean, _ := cleanModel.Update(cleanWho)
	cm := updatedClean.(Model)
	cm.width, cm.height = 120, 24
	if len(lines) != len(strings.Split(cm.View(), "\n")) {
		t.Errorf("hostile model has %d lines, clean model has %d lines", len(lines), len(strings.Split(cm.View(), "\n")))
	}

	// Direct assignment to model.userName is also sanitized in View.
	m2 := NewModel(context.Background(), stubAPI{}, 42, []string{"general"})
	m2.userName = hostile
	m2.width, m2.height = 120, 24
	v2 := m2.View()
	if strings.Contains(v2, "\x1b[31muser") || strings.Contains(v2, "\u202e") {
		t.Errorf("direct userName rendered view contains raw escape or bidi override:\n%s", v2)
	}
	lines2 := strings.Split(v2, "\n")
	if len(lines2) != 24 {
		t.Errorf("direct userName view has %d lines, want 24:\n%s", len(lines2), v2)
	}
	if !strings.Contains(lines2[len(lines2)-1], "[31muser ↵ name | ") {
		t.Errorf("direct userName status line = %q", lines2[len(lines2)-1])
	}
}

// A name longer than the terminal is clipped rather than wrapped: the status
// line never exceeds the width, so the layout holds.
func TestLongUserNameIsClippedInStatusLine(t *testing.T) {
	m := NewModel(context.Background(), stubAPI{}, 42, []string{"general"})
	m.userName = strings.Repeat("a", 300)
	m.width, m.height = 80, 24
	lines := strings.Split(m.View(), "\n")
	if len(lines) != 24 {
		t.Fatalf("view has %d lines, want 24:\n%s", len(lines), m.View())
	}
	statusLine := lines[len(lines)-1]
	if utf8.RuneCountInString(statusLine) > 80 {
		t.Errorf("status line is %d runes, want at most 80: %q", utf8.RuneCountInString(statusLine), statusLine)
	}
}

func TestApprovalFieldsAreSanitizedInView(t *testing.T) {
	app := schema.ApprovalV1{
		ID:          1,
		RequesterID: 42,
		Title:       "hostile\x1b[31m\ntitle\u202e",
		Body:        "body line 1\x1b[2J\nbody line 2\u202e",
		Payload:     &schema.Payload{Schema: "schema\x1b[32m\u202e.v1"},
		Options: []schema.Option{
			{ID: "opt1", Label: "opt\x1b[33m\nlabel\u202e"},
		},
		Deadline:  schema.NewTimestamp(time.Now().Add(time.Hour)),
		CreatedAt: schema.NewTimestamp(time.Now()),
		Quorum:    1,
		State:     schema.ApprovalStatePending,
	}

	api := stubAPI{}
	m := NewModel(context.Background(), api, 42, []string{"general"})
	m.mode = modeInbox
	m.approvals = []schema.ApprovalV1{app}
	m.selApproval = 0
	m.width, m.height = 80, 24

	inboxView := m.View()
	// Assert against the hostile sequences specifically, not a bare ESC:
	// lipgloss legitimately emits escape codes when styling is on. The palette
	// (colors 8, 6, 3) never produces [31m, [32m, or [2J.
	for _, hostile := range []string{"\x1b[2J", "\x1b[31m", "\x1b[32m"} {
		if strings.Contains(inboxView, hostile) {
			t.Errorf("inbox view contains the raw hostile sequence %q:\n%s", hostile, inboxView)
		}
	}
	if strings.Contains(inboxView, "\u202e") {
		t.Errorf("inbox view contains raw bidi override:\n%s", inboxView)
	}
	inboxLines := strings.Split(inboxView, "\n")
	if len(inboxLines) != 24 {
		t.Errorf("inbox view has %d lines, want 24:\n%s", len(inboxLines), inboxView)
	}
	// Positive pins: the sanitized text is what is drawn. These fail if a
	// newline in Title or Body survives, which the line count alone cannot see.
	for _, want := range []string{"hostile[31m ↵ title", "body line 1[2J", "body line 2", "schema[32m.v1", "  - opt[33m ↵ label"} {
		if !strings.Contains(inboxView, want) {
			t.Errorf("inbox view missing sanitized %q:\n%s", want, inboxView)
		}
	}

	m.mode = modeDecision
	decisionView := m.View()
	for _, hostile := range []string{"\x1b[2J", "\x1b[31m", "\x1b[32m"} {
		if strings.Contains(decisionView, hostile) {
			t.Errorf("decision view contains the raw hostile sequence %q:\n%s", hostile, decisionView)
		}
	}
	if strings.Contains(decisionView, "\u202e") {
		t.Errorf("decision view contains raw bidi override:\n%s", decisionView)
	}
	decisionLines := strings.Split(decisionView, "\n")
	if len(decisionLines) != 24 {
		t.Errorf("decision view has %d lines, want 24:\n%s", len(decisionLines), decisionView)
	}
	for _, want := range []string{"hostile[31m ↵ title", "opt[33m ↵ label"} {
		if !strings.Contains(decisionView, want) {
			t.Errorf("decision view missing sanitized %q:\n%s", want, decisionView)
		}
	}
}

// A body that keeps its line breaks takes one pane row per line, so the
// pane's truncation accounts for it and the layout never overflows.
func TestMultilineBodyCountsItsLines(t *testing.T) {
	body := strings.Repeat("body line\n", 30)
	app := schema.ApprovalV1{
		ID:          1,
		RequesterID: 42,
		Title:       "long body",
		Body:        body,
		Options:     []schema.Option{{ID: "opt1", Label: "approve"}},
		Deadline:    schema.NewTimestamp(time.Now().Add(time.Hour)),
		CreatedAt:   schema.NewTimestamp(time.Now()),
		Quorum:      1,
		State:       schema.ApprovalStatePending,
	}
	m := NewModel(context.Background(), stubAPI{}, 42, []string{"general"})
	m.mode = modeDecision
	m.approvals = []schema.ApprovalV1{app}
	m.width, m.height = 80, 24
	if lines := strings.Split(m.View(), "\n"); len(lines) != 24 {
		t.Errorf("view has %d lines, want 24 (multiline body overflowed the pane):\n%s", len(lines), m.View())
	}
}

// setStatus sanitizes error text independently when called directly, ensuring
// escape sequences, newlines, and bidi overrides cannot leak into status lines.
func TestSetStatusSanitizesError(t *testing.T) {
	var m Model
	err := errors.New("network error: \x1b[31mfailure\x1b[0m\r\nsubtext\u202e")
	m.setStatus(modeChannels, err.Error())
	if strings.Contains(m.channelStatus, "\x1b") || strings.Contains(m.channelStatus, "\u202e") || strings.ContainsAny(m.channelStatus, "\r\n") {
		t.Errorf("channelStatus carries control chars or bidi overrides: %q", m.channelStatus)
	}
	want := "network error: [31mfailure[0m ↵ subtext"
	if m.channelStatus != want {
		t.Errorf("channelStatus = %q, want %q", m.channelStatus, want)
	}

	m.setStatus(modeInbox, err.Error())
	if m.inboxStatus != want {
		t.Errorf("inboxStatus = %q, want %q", m.inboxStatus, want)
	}
}

func TestSanitizeMultiline(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "tabs map to spaces",
			input: "col1\tcol2\t\tcol3",
			want:  "col1 col2  col3",
		},
		{
			name:  "lone carriage return becomes newline",
			input: "line1\rline2\rline3",
			want:  "line1\nline2\nline3",
		},
		{
			name:  "crlf becomes newline",
			input: "line1\r\nline2",
			want:  "line1\nline2",
		},
		{
			name:  "unicode line separator U+2028 becomes newline",
			input: "first\u2028second",
			want:  "first\nsecond",
		},
		{
			name:  "unicode paragraph separator U+2029 becomes newline",
			input: "para1\u2029para2",
			want:  "para1\npara2",
		},
		{
			name:  "control characters and bidi overrides stripped",
			input: "hello\x1b[31m \u202eworld\u200e\a\b",
			want:  "hello[31m world",
		},
		{
			name:  "combined multiline with tabs lone cr and separators",
			input: "heading\tvalue\rbody line 1\r\nbody\tline\t2\u2028subline\u2029footer\x1b[0m",
			want:  "heading value\nbody line 1\nbody line 2\nsubline\nfooter[0m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeMultiline(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeMultiline(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
