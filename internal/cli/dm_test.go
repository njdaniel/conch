package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestDMCommands(t *testing.T) {
	at := schema.NewTimestamp(time.Unix(1, 0))
	conversation := schema.DMConversationV1{
		Schema: schema.DMConversationSchemaV1, ID: 7, ParticipantIDs: []int64{1, 2},
		PeerID: 2, PeerName: "Bo\x1bb\u202e", State: schema.DMStateOpen,
		Revision: 1, CreatedAt: &at, LastMessageAt: &at, LastMessageID: 9, Unread: 1,
	}
	message := schema.DMMessageV1{
		Schema: schema.DMMessageSchemaV1, ID: 9, ConversationID: 7, AuthorID: 2,
		CreatedAt: at, Revision: 1, Body: "private\ntext\u202e",
	}
	mux := http.NewServeMux()
	handle := func(pattern string, fn func(*http.Request) any) {
		t.Helper()
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer dm-token" || r.Header.Get(previewAsHeader) != "" {
				t.Error("DM request did not use the caller's ordinary credential")
			}
			out := fn(r)
			if out == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Error(err)
			}
		})
	}
	handle("POST /v1/dms", func(r *http.Request) any {
		var req schema.OpenDMRequestV1
		checkDMRequest(t, r, &req, "peer_id")
		if req.PeerID != 2 {
			t.Errorf("open peer ID = %d", req.PeerID)
		}
		return schema.OpenDMResponseV1{Conversation: conversation}
	})
	handle("GET /v1/dms", func(r *http.Request) any {
		if query := r.URL.Query(); query.Get("state") != "archived" || query.Get("cursor") != "first" {
			t.Errorf("unexpected list query %s", r.URL.RawQuery)
		}
		archived := conversation
		archived.State = schema.DMStateArchived
		return schema.ListDMConversationsResponseV1{Conversations: []schema.DMConversationV1{archived}, NextCursor: "next"}
	})
	handle("GET /v1/dms/7", func(*http.Request) any { return conversation })
	handle("GET /v1/dms/7/messages", func(r *http.Request) any {
		if query := r.URL.Query(); query.Get("order") != "desc" || query.Get("limit") != "3" || query.Get("before") != "10" {
			t.Errorf("unexpected history query %s", r.URL.RawQuery)
		}
		return schema.ListDMMessagesResponseV1{Order: schema.DMOrderDesc, Messages: []schema.DMMessageV1{message}, NextBefore: message.ID}
	})
	handle("POST /v1/dms/7/messages", func(r *http.Request) any {
		var req schema.PostDMMessageRequestV1
		checkDMRequest(t, r, &req, "body", "client_nonce")
		if req.Body != "@all /text" || req.Validate() != nil {
			t.Errorf("invalid dm send request: %+v", req)
		}
		sent := message
		sent.AuthorID, sent.Body, sent.ClientNonce = 1, req.Body, req.ClientNonce
		return schema.PostDMMessageResponseV1{Message: sent}
	})
	handle("PUT /v1/dms/7/read-state", func(r *http.Request) any {
		var req schema.MarkReadRequestV1
		checkDMRequest(t, r, &req, "last_read_id")
		if req.LastReadID != 9 {
			t.Errorf("read mark = %d", req.LastReadID)
		}
		return schema.DMReadStateV1{Schema: schema.DMReadStateSchemaV1, ConversationID: 7, LastReadID: 9}
	})
	handle("PATCH /v1/dms/7/messages/9", func(r *http.Request) any {
		var req schema.EditDMMessageRequestV1
		checkDMRequest(t, r, &req, "body")
		if req.Body != "fixed" {
			t.Errorf("edit body = %q", req.Body)
		}
		edited := message
		edited.Body = req.Body
		return schema.EditDMMessageResponseV1{Message: edited}
	})
	handle("DELETE /v1/dms/7/messages/9", func(*http.Request) any { return nil })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("CONCH_SERVER", srv.URL)
	t.Setenv("CONCH_TOKEN", "dm-token")

	listing := "7  Bo\\x1bb\\u202e (#2)  1 unread\n"
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"open", "2"}, listing},
		{[]string{"list", "--archived", "--cursor", "first"}, listing + "more: --cursor next\n"},
		{[]string{"send", "7", "@all /text"}, "9\n"},
		{[]string{"history", "--limit", "3", "--before", "10", "7"}, listing + "9  #2  private\\ntext\\u202e\nolder: --before 9\n"},
		{[]string{"mark-read", "7", "9"}, "read through 9 (0 unread)\n"},
		{[]string{"edit", "7", "9", "fixed"}, "9\n"},
		{[]string{"remove", "7", "9"}, ""},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			out, stderr, err := runCLI(t, "", append([]string{"dm"}, tt.args...)...)
			if err != nil || stderr != "" || out != tt.want {
				t.Fatalf("stdout = %q, stderr = %q, err = %v; want %q", out, stderr, err, tt.want)
			}
		})
	}
}

func checkDMRequest(t *testing.T, r *http.Request, target any, keys ...string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Error(err)
	}
	for key := range raw {
		if !slices.Contains(keys, key) {
			t.Errorf("unexpected request field %q", key)
		}
	}
	if err := json.Unmarshal(body, target); err != nil {
		t.Error(err)
	}
}

func TestDMCommandRefusals(t *testing.T) {
	for _, tt := range []struct {
		verb, code, want string
		status           int
		args             []string
	}{
		{"open", schema.ErrorCodeContactUnavailable, schema.DMContactUnavailableMessage, http.StatusForbidden, []string{"2"}},
		{"send", schema.ErrorCodeContactUnavailable, schema.DMContactUnavailableMessage, http.StatusForbidden, []string{"7", "text"}},
		{"open", "principal_not_found", "no such person", http.StatusNotFound, []string{"2"}},
		{"history", schema.ErrorCodeOwnSignInRequired, schema.DMOwnSignInRequiredMessage, http.StatusForbidden, []string{"7"}},
		{"history", schema.ErrorCodeDMNotFound, schema.DMNotFoundMessage, http.StatusNotFound, []string{"7"}},
	} {
		t.Run(tt.verb+"/"+tt.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				if err := json.NewEncoder(w).Encode(schema.Error{Code: tt.code, Message: "private detail\x1b"}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(srv.Close)
			t.Setenv("CONCH_SERVER", srv.URL)
			t.Setenv("CONCH_TOKEN", "dm-token")
			out, stderr, err := runCLI(t, "", append([]string{"dm", tt.verb}, tt.args...)...)
			if out != "" || stderr != "" || !IsServerCode(err, tt.code) || err.Error() != tt.want || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("stdout = %q, stderr = %q, err = %v; want %q", out, stderr, err, tt.want)
			}
		})
	}
}
