package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

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
	var mu sync.Mutex
	var unavailable, disconnected bool
	var nonce string
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
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
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Validate() != nil || req.Body != "/voice join" {
				t.Errorf("send request %+v, %v", req, err)
			}
			if nonce == "" {
				nonce = req.ClientNonce
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if nonce != req.ClientNonce {
				t.Errorf("send retry used a different nonce: %q != %q", nonce, req.ClientNonce)
			}
			sent = true
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
	}))
	t.Cleanup(srv.Close)
	client, err := cli.NewClient(srv.URL, srv.Client())
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
	mu.Lock()
	unavailable = true
	mu.Unlock()
	run(key(tea.KeyEnter, ""))
	if model.dm.status != schema.DMContactUnavailableMessage {
		t.Fatalf("unexpected unavailable status %q", model.dm.status)
	}
	mu.Lock()
	unavailable = false
	mu.Unlock()
	run(run(key(tea.KeyEnter, "")))
	assertDMView("private text")
	key(tea.KeyRunes, "/voice join")
	run(key(tea.KeyEnter, ""))
	if model.dm.draft != "/voice join" {
		t.Errorf("failed send lost the DM draft: %q", model.dm.draft)
	}
	run(run(key(tea.KeyEnter, "")))
	mu.Lock()
	didSend := sent
	mu.Unlock()
	if model.dm.draft != "" || !didSend {
		t.Errorf("retry did not send and clear the DM draft: sent=%t draft=%q", didSend, model.dm.draft)
	}
	run(run(key(tea.KeyCtrlR, "")))
	assertDMView("0 unread")

	mu.Lock()
	disconnected = true
	mu.Unlock()
	run(update(dmPollDue{model.dm.ctx}))
	if strings.Contains(model.View(), "private text") || model.dm.status == "" {
		t.Fatal("disconnected view kept private content or omitted the error")
	}
	mu.Lock()
	disconnected = false
	message.Body, message.Revision, message.Removed = schema.MessageRemovedByAuthorBody, 2, true
	conversation.Revision = 2
	mu.Unlock()
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
