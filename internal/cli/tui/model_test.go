package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type stubAPI struct{}

func (stubAPI) ListMessages(context.Context, string, int64, int) (schema.ListMessagesResponseV1, error) {
	return schema.ListMessagesResponseV1{}, nil
}
func (stubAPI) SendMessage(context.Context, string, int64, string) (schema.MessageV1, error) {
	return schema.MessageV1{}, nil
}
func (stubAPI) Subscribe(context.Context, string, func(schema.MessageV1) error) error { return nil }
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
		{name: "loaded merges duplicates", msg: messagesLoaded{channel: "general", messages: []schema.MessageV1{{ID: 2, Body: "new"}, {ID: 1, Body: "first"}}}, prep: func(m *Model) {
			m.messages["general"] = []schema.MessageV1{{ID: 2, Body: "old"}}
		}, want: func(t *testing.T, got Model) {
			messages := got.messages["general"]
			if len(messages) != 2 || messages[0].ID != 1 || messages[1].Body != "new" {
				t.Errorf("messages = %+v", messages)
			}
		}},
		{name: "load error", msg: messagesLoaded{channel: "general", err: errBoom}, want: func(t *testing.T, got Model) {
			if got.status != "boom" {
				t.Errorf("status = %q", got.status)
			}
		}},
		{name: "send needs author", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) { m.input = "hello"; m.authorID = 0 }, want: func(t *testing.T, got Model) {
			if got.status != "set CONCH_AUTHOR to send" || got.input != "hello" {
				t.Errorf("status/input = %q/%q", got.status, got.input)
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
			if got.status != "reason is required" {
				t.Errorf("expected 'reason is required', got %q", got.status)
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

func (a *listAPI) ListMessages(_ context.Context, channel string, _ int64, _ int) (schema.ListMessagesResponseV1, error) {
	a.mu.Lock()
	a.listed = append(a.listed, channel)
	a.mu.Unlock()
	return schema.ListMessagesResponseV1{}, nil
}

func (a *listAPI) Subscribe(_ context.Context, channel string, _ func(schema.MessageV1) error) error {
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
			if got.status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.status, tt.wantStatus)
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
			if s := after.(Model).status; s != wantAfter {
				t.Errorf("status after backfill = %q, want %q", s, wantAfter)
			}
			// ...and the fallback channel's own errors, which would otherwise
			// hide why the fallback happened.
			failed, _ := got.Update(messagesLoaded{channel: tt.wantList[0], err: errors.New("channel not found")})
			wantFailed := "channel not found"
			if tt.wantStatus != "" {
				wantFailed = tt.wantStatus
			}
			if s := failed.(Model).status; s != wantFailed {
				t.Errorf("status after failed backfill = %q, want %q", s, wantFailed)
			}
			ended, _ := got.Update(subscriptionEnded{channel: tt.wantList[0], err: errors.New("gone")})
			wantEnded := "live updates: gone"
			if tt.wantStatus != "" {
				wantEnded = tt.wantStatus
			}
			if s := ended.(Model).status; s != wantEnded {
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
	if s := updated.(Model).status; !strings.Contains(s, "no channel") {
		t.Errorf("status = %q, want no-channel message", s)
	}
	if !strings.Contains(m.View(), "loading") {
		t.Errorf("view should show loading:\n%s", m.View())
	}
}

func TestModelViewSmoke(t *testing.T) {
	model := NewModel(context.Background(), stubAPI{}, 7, []string{"general", "ops"})
	model.width, model.height = 80, 24
	model.messages["general"] = []schema.MessageV1{
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

func (a *identityAPI) SendMessage(_ context.Context, _ string, author int64, _ string) (schema.MessageV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sentAs = author
	return schema.MessageV1{}, nil
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
		msg  tea.Msg
	}{
		{"whoami", whoAmILoaded{err: unauth}},
		{"messages", messagesLoaded{channel: "general", err: unauth}},
		{"approvals", approvalsLoaded{err: unauth}},
		{"send", messageSent{err: unauth}},
		{"decision", decisionCast{err: unauth}},
		{"subscription", subscriptionEnded{channel: "general", err: unauth}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(context.Background(), stubAPI{}, 0, []string{"general"}).WithCredential()
			updated, _ := model.Update(tt.msg)
			got := updated.(Model)
			if got.status != hint {
				t.Fatalf("status = %q, want %q", got.status, hint)
			}
			// Still usable: keys are accepted and the view renders.
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
		if got := updated.(Model).status; got != hint {
			t.Errorf("status = %q, want %q", got, hint)
		}
	})

	t.Run("send before identity is known", func(t *testing.T) {
		model := NewModel(context.Background(), stubAPI{}, 0, []string{"general"}).WithCredential()
		model.input = "hi"
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil || !strings.Contains(updated.(Model).status, "identity") {
			t.Errorf("status = %q cmd = %v", updated.(Model).status, cmd)
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
				if got := updated.(Model).status; !strings.Contains(got, tt.want) {
					t.Errorf("status = %q, want containing %q", got, tt.want)
				}
			})
		}
	}
}
