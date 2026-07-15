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
		{name: "slash unknown", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) { m.input = "/nope" }, want: func(t *testing.T, got Model) {
			if got.input != "" {
				t.Errorf("input not cleared: %q", got.input)
			}
			if !strings.HasPrefix(got.status, "unknown command:") {
				t.Errorf("status = %q", got.status)
			}
		}},
		{name: "chronicle tips no messages", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) { m.input = "/chronicle tips" }, want: func(t *testing.T, got Model) {
			if got.input != "" {
				t.Errorf("input not cleared: %q", got.input)
			}
			if !strings.HasPrefix(got.status, "chronicle:") {
				t.Errorf("status = %q", got.status)
			}
		}},
		{name: "chronicle tips with own messages", msg: tea.KeyMsg{Type: tea.KeyEnter}, prep: func(m *Model) {
			m.input = "/chronicle tips"
			m.messages["general"] = []schema.MessageV1{
				{ID: 1, AuthorID: 7, Body: "hello"},
				{ID: 2, AuthorID: 7, Body: "world"},
			}
		}, want: func(t *testing.T, got Model) {
			if !strings.HasPrefix(got.status, "chronicle:") {
				t.Errorf("status = %q", got.status)
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
