package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

type recordingBroadcaster struct {
	messages []schema.MessageV0
}

func (b *recordingBroadcaster) BroadcastMessage(_ context.Context, message schema.MessageV0) {
	b.messages = append(b.messages, message)
}

func createTestChannelAndPrincipal(t *testing.T, srv *Server) (store.Channel, store.Principal) {
	t.Helper()
	ctx := context.Background()
	channel, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	principal, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return channel, principal
}

func TestPostGetMessagesRoundTripAndBroadcast(t *testing.T) {
	broadcaster := &recordingBroadcaster{}
	srv := newTestServerWithConfig(t, Config{Broadcaster: broadcaster})
	channel, principal := createTestChannelAndPrincipal(t, srv)

	body := fmt.Sprintf(`{"author_id":%d,"body":"hello"}`, principal.ID)
	postReq := httptest.NewRequest(http.MethodPost, "/v0/channels/general/messages", bytes.NewBufferString(body))
	postRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want %d; body = %s", postRec.Code, http.StatusCreated, postRec.Body.String())
	}
	var posted schema.PostMessageResponse
	if err := json.Unmarshal(postRec.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if posted.Message.ID == 0 || posted.Message.ChannelID != channel.ID ||
		posted.Message.AuthorID != principal.ID || posted.Message.Body != "hello" || posted.Message.CreatedAt.IsZero() {
		t.Errorf("POST message = %+v", posted.Message)
	}
	if len(broadcaster.messages) != 1 || !messagesEqual(broadcaster.messages[0], posted.Message) {
		t.Fatalf("broadcast messages = %+v, want posted message %+v", broadcaster.messages, posted.Message)
	}
	if posted.Message.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at zone = %v, want UTC on the wire", posted.Message.CreatedAt.Location())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v0/channels/general/messages", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var listed schema.ListMessagesResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(listed.Messages) != 1 || !messagesEqual(listed.Messages[0], posted.Message) {
		t.Errorf("GET messages = %+v, want posted message %+v", listed.Messages, posted.Message)
	}
	if listed.NextAfter != 0 {
		t.Errorf("GET next_after = %d, want 0", listed.NextAfter)
	}
}

func messagesEqual(a, b schema.MessageV0) bool {
	return a.ID == b.ID && a.ChannelID == b.ChannelID && a.AuthorID == b.AuthorID &&
		a.Body == b.Body && a.CreatedAt.Equal(b.CreatedAt)
}

func TestListMessagesPagination(t *testing.T) {
	srv := newTestServer(t)
	channel, principal := createTestChannelAndPrincipal(t, srv)
	ctx := context.Background()
	for i := range 5 {
		if _, err := srv.store.InsertMessage(ctx, channel.ID, principal.ID, fmt.Sprintf("message %d", i)); err != nil {
			t.Fatalf("InsertMessage %d: %v", i, err)
		}
	}

	tests := []struct {
		name          string
		after         int64
		wantBodies    []string
		wantNextAfter bool
	}{
		{name: "first page", wantBodies: []string{"message 0", "message 1"}, wantNextAfter: true},
		{name: "second page", after: 2, wantBodies: []string{"message 2", "message 3"}, wantNextAfter: true},
		{name: "last page", after: 4, wantBodies: []string{"message 4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fmt.Sprintf("/v0/channels/general/messages?limit=2&after=%d", tt.after)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var response schema.ListMessagesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(response.Messages) != len(tt.wantBodies) {
				t.Fatalf("message count = %d, want %d", len(response.Messages), len(tt.wantBodies))
			}
			for i, want := range tt.wantBodies {
				if response.Messages[i].Body != want {
					t.Errorf("message %d body = %q, want %q", i, response.Messages[i].Body, want)
				}
			}
			if (response.NextAfter != 0) != tt.wantNextAfter {
				t.Errorf("next_after = %d, want present %v", response.NextAfter, tt.wantNextAfter)
			}
			if response.NextAfter != 0 && response.NextAfter != response.Messages[len(response.Messages)-1].ID {
				t.Errorf("next_after = %d, want last returned ID %d", response.NextAfter, response.Messages[len(response.Messages)-1].ID)
			}
		})
	}
}

func TestMessagesUnknownChannel(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name   string
		method string
		body   string
	}{
		{name: "get", method: http.MethodGet},
		{name: "post", method: http.MethodPost, body: `{"author_id":1,"body":"hello"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/v0/channels/missing/messages", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusNotFound, "channel_not_found")
		})
	}
}

func TestPostMessageMalformedBody(t *testing.T) {
	srv := newTestServer(t)
	_, principal := createTestChannelAndPrincipal(t, srv)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"author_id":`},
		{name: "unknown field", body: fmt.Sprintf(`{"author_id":%d,"body":"hello","payload":{}}`, principal.ID)},
		{name: "multiple objects", body: fmt.Sprintf(`{"author_id":%d,"body":"hello"} {}`, principal.ID)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v0/channels/general/messages", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestPostMessageBodyTooLarge(t *testing.T) {
	srv := newTestServer(t)
	_, principal := createTestChannelAndPrincipal(t, srv)

	body := fmt.Sprintf(`{"author_id":%d,"body":%q}`, principal.ID, strings.Repeat("x", maxMessageBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/v0/channels/general/messages", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assertAPIError(t, rec, http.StatusRequestEntityTooLarge, "request_too_large")
}

func TestMessagesV1PayloadRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantStatus int
		wantData   string
	}{
		{name: "with payload", payload: `,"payload":{"schema":"acme.alert.v1","data":{"level":2}}`, wantStatus: http.StatusCreated, wantData: `{"level":2}`},
		{name: "without payload", wantStatus: http.StatusCreated},
		{name: "malformed payload", payload: `,"payload":{"schema":"bad-name","data":{}}`, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			_, principal := createTestChannelAndPrincipal(t, srv)
			body := fmt.Sprintf(`{"author_id":%d,"body":"hello"%s}`, principal.ID, tt.payload)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/channels/general/messages", strings.NewReader(body)))
			if rec.Code != tt.wantStatus {
				t.Fatalf("POST status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusCreated {
				return
			}
			var posted schema.PostMessageResponseV1
			if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
				t.Fatalf("decode POST: %v", err)
			}

			get := httptest.NewRecorder()
			srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/channels/general/messages", nil))
			var listed schema.ListMessagesResponseV1
			if err := json.Unmarshal(get.Body.Bytes(), &listed); err != nil {
				t.Fatalf("decode GET: %v", err)
			}
			if len(listed.Messages) != 1 {
				t.Fatalf("GET messages = %d, want 1", len(listed.Messages))
			}
			got := listed.Messages[0]
			if got.Schema != schema.MessageSchemaV1 || got.CreatedAt.Time().Location() != time.UTC {
				t.Errorf("GET envelope = %+v", got)
			}
			if tt.wantData == "" {
				if got.Payload != nil {
					t.Errorf("payload = %+v, want nil", got.Payload)
				}
			} else if got.Payload == nil || string(got.Payload.Data) != tt.wantData {
				t.Errorf("payload = %+v, want data %s", got.Payload, tt.wantData)
			}
		})
	}
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var response schema.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Code != wantCode || response.Message == "" {
		t.Errorf("error response = %+v, want code %q and non-empty message", response, wantCode)
	}
}
