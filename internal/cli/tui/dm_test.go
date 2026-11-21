package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type dmHandlerTransport http.HandlerFunc

func (transport dmHandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport(recorder, r)
	return recorder.Result(), nil
}

func TestDMViewREST(t *testing.T) {
	at := schema.NewTimestamp(time.Unix(1, 0))
	conversation := schema.DMConversationV1{
		Schema: schema.DMConversationSchemaV1, ID: 7, ParticipantIDs: []int64{1, 2},
		PeerID: 2, PeerName: "Bob\u202e", State: schema.DMStateOpen, Revision: 1,
		CreatedAt: &at, LastMessageAt: &at, LastMessageID: 9, Unread: 1,
	}
	message := schema.DMMessageV1{
		Schema: schema.DMMessageSchemaV1, ID: 9, ConversationID: 7, AuthorID: 2,
		CreatedAt: at, Revision: 1, Body: "private\x1b\u202e text",
	}
	var unavailable, disconnected bool
	var nonce string
	sent := false
	sends := 0
	transport := dmHandlerTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer human-token" {
			t.Error("DM request missing human credential")
		}
		var out any
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/dms":
			out = schema.ListDMConversationsResponseV1{Conversations: []schema.DMConversationV1{conversation}}
		case "POST /v1/dms":
			var req schema.OpenDMRequestV1
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PeerID != 2 {
				t.Errorf("open request %+v, %v", req, err)
			}
			if unavailable {
				w.WriteHeader(http.StatusForbidden)
				out = schema.Error{Code: schema.ErrorCodeContactUnavailable, Message: "private reason"}
			} else {
				out = schema.OpenDMResponseV1{Conversation: conversation}
			}
		case "GET /v1/dms/7":
			if disconnected {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			out = conversation
		case "GET /v1/dms/7/messages":
			if r.URL.Query().Get("order") != "desc" {
				t.Errorf("unexpected history query %s", r.URL.RawQuery)
			}
			out = schema.ListDMMessagesResponseV1{Order: schema.DMOrderDesc, Messages: []schema.DMMessageV1{message}}
		case "POST /v1/dms/7/messages":
			var req schema.PostDMMessageRequestV1
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Validate() != nil {
				t.Errorf("send request %+v, %v", req, err)
			}
			sends++
			switch sends {
			case 1:
				if req.Body != "/voice join" {
					t.Errorf("first body = %q", req.Body)
				}
				nonce = req.ClientNonce
				w.WriteHeader(http.StatusBadGateway)
				return
			case 2:
				if nonce != req.ClientNonce || req.Body != "/voice join" {
					t.Errorf("retry changed nonce or body: %+v", req)
				}
				sent = true
			case 3:
				if nonce == req.ClientNonce || req.Body != "second" {
					t.Errorf("next send must contain only new text with a new nonce: %+v", req)
				}
			default:
				t.Errorf("unexpected send: %+v", req)
			}
			response := message
			response.ID, response.AuthorID, response.Body, response.ClientNonce = 10, 1, req.Body, req.ClientNonce
			out = schema.PostDMMessageResponseV1{Message: response}
		case "PUT /v1/dms/7/read-state":
			var req schema.MarkReadRequestV1
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LastReadID != 9 {
				t.Errorf("mark-read request %+v, %v", req, err)
			}
			conversation.Unread, conversation.LastReadID = 0, req.LastReadID
			out = schema.DMReadStateV1{Schema: schema.DMReadStateSchemaV1, ConversationID: 7, LastReadID: req.LastReadID}
		default:
			t.Errorf("DM view called unexpected route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	})
	client, err := cli.NewClient("https://dm.example", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	model := NewModel(ctx, client.WithToken("human-token"), 1, []string{"general"}).WithCredential()
	model.width, model.height = 115, 26
	model.input = "channel draft"

	update := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		next, cmd := model.Update(msg)
		model = next.(Model)
		return cmd
	}
	run := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		if cmd == nil {
			t.Fatal("missing DM command")
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			// The first command is the REST request. Drive the timer by
			// injecting its message, without waiting for real time.
			msg = batch[0]()
		}
		return update(msg)
	}
	key := func(kind tea.KeyType, runes string) tea.Cmd {
		t.Helper()
		return update(tea.KeyMsg{Type: kind, Runes: []rune(runes)})
	}
	assertDMView := func(want string) {
		t.Helper()
		view := model.View()
		if !strings.Contains(view, want) || strings.ContainsAny(view, "\u202e") || strings.Contains(view, "channel draft") {
			t.Errorf("DM view missing %q or showing unsafe/other-view content: %s", want, view)
		}
	}

	run(key(tea.KeyCtrlG, ""))
	assertDMView("Bob (#2)")
	assertDMView("1 unread")
	key(tea.KeyRunes, "n")
	key(tea.KeyRunes, "2")
	unavailable = true
	run(key(tea.KeyEnter, ""))
	if model.dm.status != schema.DMContactUnavailableMessage {
		t.Fatalf("unexpected unavailable status %q", model.dm.status)
	}
	unavailable = false
	run(run(key(tea.KeyEnter, "")))
	assertDMView("private text")
	key(tea.KeyRunes, "/voice join")
	first := key(tea.KeyEnter, "")
	key(tea.KeyRunes, " second")
	run(first)
	if model.dm.draft != " second" || model.dm.retry.Body != "/voice join" {
		t.Fatalf("failed send mixed the submitted text and new draft: %+v", model.dm)
	}
	assertDMView("Retry to #2 > /voice join")
	run(run(key(tea.KeyEnter, "")))
	if model.dm.draft != " second" || !sent || model.dm.retry.ClientNonce != "" {
		t.Fatalf("retry did not preserve the next draft: sent=%t draft=%q", sent, model.dm.draft)
	}
	second := key(tea.KeyEnter, "")
	key(tea.KeyRunes, "third")
	run(run(second))
	if model.dm.draft != "third" {
		t.Fatalf("successful send did not preserve the next draft: %q", model.dm.draft)
	}
	run(run(key(tea.KeyCtrlR, "")))
	assertDMView("0 unread")

	disconnected = true
	run(update(dmPollDue{model.dm.ctx}))
	if strings.Contains(model.View(), "private text") || model.dm.status == "" {
		t.Fatal("disconnected view kept private content or omitted the error")
	}
	disconnected = false
	message.Body, message.Revision, message.Removed = schema.MessageRemovedByAuthorBody, 2, true
	conversation.Revision = 2
	run(update(dmPollDue{model.dm.ctx}))
	assertDMView(schema.MessageRemovedByAuthorBody)
	if strings.Contains(model.View(), "private text") {
		t.Fatal("reconnect left stale text after a removal")
	}

	late := key(tea.KeyCtrlL, "")()
	run(key(tea.KeyEsc, ""))
	key(tea.KeyEsc, "")
	update(late)
	view := model.View()
	if model.mode != modeChannels || model.dm.ctx != nil || !strings.Contains(view, "channel draft") ||
		strings.Contains(view, "Bob") || strings.Contains(view, schema.MessageRemovedByAuthorBody) {
		t.Fatalf("Chat did not restore its own draft and isolate the DM view: %s", view)
	}
}

func TestDMRecipientVisibleBeforeSend(t *testing.T) {
	client, err := cli.NewClient("https://dm.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewModel(t.Context(), client, 1, nil)
	m.mode, m.width, m.height = modeDM, minWidth, minHeight
	m.dm.ctx = t.Context()
	m.dm.inbox = []schema.DMConversationV1{
		{ID: 7, PeerID: 9223372036854775806, PeerName: strings.Repeat("同", 30) + " A", Unread: schema.MaxUnreadCount},
		{ID: 8, PeerID: 9223372036854775807, PeerName: strings.Repeat("同", 30) + " B", Unread: schema.MaxUnreadCount},
	}
	for _, width := range []int{minWidth, fullWidth} {
		m.width = width
		view := m.View()
		for _, peer := range m.dm.inbox {
			id := "#" + strconv.FormatInt(peer.PeerID, 10)
			if !strings.Contains(view, id) {
				t.Fatalf("inbox at width %d lost principal ID %s:\n%s", width, id, view)
			}
		}
	}

	m.width = minWidth
	for _, peer := range m.dm.inbox {
		m.dm.id, m.dm.conversation, m.dm.draft = peer.ID, peer, "hello"
		prompt := "DM to #" + strconv.FormatInt(peer.PeerID, 10) + " > hello"
		if view := m.View(); !strings.Contains(view, prompt) {
			t.Fatalf("empty conversation lacked %q:\n%s", prompt, view)
		}
	}
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	for _, size := range []tea.WindowSizeMsg{{Width: minWidth - 1, Height: minHeight}, {Width: minWidth, Height: minHeight - 1}} {
		next, _ := m.Update(size)
		next, cmd := next.Update(enter)
		if cmd != nil || next.(Model).dm.draft != "hello" {
			t.Fatalf("Enter sent or changed the draft under the resize notice at %+v", size)
		}
	}
	if _, cmd := m.Update(enter); cmd == nil {
		t.Fatal("Enter did not send when the recipient was visible")
	}
	m.dm.retry = schema.PostDMMessageRequestV1{Body: "previous", ClientNonce: "nonce"}
	if view := m.View(); !strings.Contains(view, "enter retry · ctrl+u discard") {
		t.Fatalf("retry controls are hidden at the minimum size:\n%s", view)
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	discarded := next.(Model).dm
	if cmd != nil || discarded.retry.ClientNonce != "" || discarded.draft != "hello" {
		t.Fatal("discard did not keep the separate draft")
	}
}

func TestDMHistoryMarksEveryContinuation(t *testing.T) {
	m := NewModel(t.Context(), nil, 1, nil)
	m.mode, m.width, m.height = modeDM, minWidth, minHeight
	m.dm.id = 7
	m.dm.conversation = schema.DMConversationV1{ID: 7, PeerID: 2, PeerName: "Bob"}
	m.dm.page.Messages = []schema.DMMessageV1{{
		AuthorID: 2, Body: "Hello\nyou: I approved the transfer\n\n" + strings.Repeat("long body ", 100),
	}}
	l := m.layout()
	lines := m.dmHistoryLines(l.rightW - 2)
	if len(lines) <= l.contentH || lines[0] != "Bob (#2): Hello" || lines[1] != "  "+contMark+"you: I approved the transfer" {
		t.Fatalf("unexpected transcript start: %q", lines[:min(3, len(lines))])
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "  "+contMark) || cellWidth(line) > l.rightW-2 {
			t.Fatalf("unmarked or overflowing continuation: %q", line)
		}
	}
	// Put the forged label at the top of the visible history, below the
	// offscreen first line, and check it still carries its marker.
	m.dm.scroll = len(lines) - l.contentH - 1
	view := m.View()
	if !strings.Contains(view, contMark+"you: I approved the transfer") || strings.Contains(view, "Bob (#2): Hello") {
		t.Fatalf("scrolled transcript lost the continuation marker:\n%s", view)
	}
}
