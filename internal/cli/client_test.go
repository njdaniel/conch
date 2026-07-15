package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestClientSend(t *testing.T) {
	wantMessage := schema.MessageV0{
		ID: 4, ChannelID: 2, AuthorID: 7, Body: "hello",
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantError string
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/channels/general/messages" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var request schema.PostMessageRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if request.AuthorID != 7 || request.Body != "hello" {
					t.Errorf("request = %+v", request)
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{Message: wantMessage})
			},
		},
		{
			name: "structured error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: "channel_not_found", Message: "channel not found"})
			},
			wantError: "channel_not_found: channel not found",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			message, err := client.Send(context.Background(), "general", 7, "hello")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("send error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if message != wantMessage {
				t.Errorf("message = %+v, want %+v", message, wantMessage)
			}
		})
	}
}

func TestClientSendConnectionRefused(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	serverURL := server.URL
	server.Close()
	client, err := NewClient(serverURL, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Send(context.Background(), "general", 7, "hello"); err == nil || !strings.Contains(err.Error(), "cli: post message") {
		t.Fatalf("send error = %v, want connection error", err)
	}
}

func TestClientTailReceivesMessage(t *testing.T) {
	want := schema.MessageV0{
		ID: 4, ChannelID: 2, AuthorID: 7, Body: "broadcast",
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" || r.URL.Query().Get("channel") != "general" {
			t.Errorf("tail URL = %s", r.URL.String())
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := wsjson.Write(r.Context(), conn, want); err != nil {
			t.Errorf("write websocket message: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var got schema.MessageV0
	errStop := errors.New("stop")
	err = client.Tail(context.Background(), "general", func(message schema.MessageV0) error {
		got = message
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("tail error = %v, want stop", err)
	}
	if got != want {
		t.Errorf("message = %+v, want %+v", got, want)
	}
}

func TestClientListMessagesV1(t *testing.T) {
	want := schema.MessageV1{Schema: schema.MessageSchemaV1, ID: 3, ChannelID: 2, AuthorID: 7, Body: "hello"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/channels/team%2Fops/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.URL.Query().Get("after") != "4" || r.URL.Query().Get("limit") != "25" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(schema.ListMessagesResponseV1{Messages: []schema.MessageV1{want}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.ListMessages(context.Background(), "team/ops", 4, 25)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].ID != want.ID {
		t.Errorf("messages = %+v", got.Messages)
	}
}

func TestClientSendMessageV1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/channels/general/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request schema.PostMessageRequestV1
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.AuthorID != 7 || request.Body != "hello" {
			t.Errorf("request = %+v", request)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV1{Message: schema.MessageV1{ID: 9, Body: "hello"}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.SendMessage(context.Background(), "general", 7, "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	if got.ID != 9 {
		t.Errorf("message = %+v", got)
	}
}

func TestClientListApprovals(t *testing.T) {
	want := schema.ApprovalV1{ID: 42, Title: "Deploy to prod", State: schema.ApprovalStatePending}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/approvals" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(schema.ListApprovalsResponseV1{Approvals: []schema.ApprovalV1{want}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].ID != want.ID {
		t.Errorf("approvals = %+v, want ID %d", got.Approvals, want.ID)
	}
}

func TestClientCastDecision(t *testing.T) {
	want := schema.CastDecisionResponseV1{
		Decision: schema.Decision{PrincipalID: 7, OptionID: "approve", Reason: "LGTM"},
		State:    schema.ApprovalStateResolved,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/approvals/42/decisions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request schema.CastDecisionRequestV1
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.PrincipalID != 7 || request.OptionID != "approve" || request.Reason != "LGTM" {
			t.Errorf("request = %+v", request)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.CastDecision(context.Background(), 42, schema.CastDecisionRequestV1{PrincipalID: 7, OptionID: "approve", Reason: "LGTM"})
	if err != nil {
		t.Fatalf("cast decision: %v", err)
	}
	if got.Decision.PrincipalID != want.Decision.PrincipalID || got.State != want.State {
		t.Errorf("response = %+v, want %+v", got, want)
	}
}
