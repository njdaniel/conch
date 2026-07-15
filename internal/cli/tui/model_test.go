package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
func (stubAPI) ListApprovals(context.Context) (schema.ListApprovalsResponseV1, error) {
	return schema.ListApprovalsResponseV1{}, nil
}
func (stubAPI) CastDecision(context.Context, int64, schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error) {
	return schema.CastDecisionResponseV1{}, nil
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
