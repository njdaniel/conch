package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestCreateHook(t *testing.T) {
	srv := newTestServer(t)
	channel, principal := createTestChannelAndPrincipal(t, srv)
	body := fmt.Sprintf(`{"channel":%q,"principal":%d}`, channel.Name, principal.ID)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/hooks", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var response schema.CreateHookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Token == "" || strings.ContainsAny(response.Token, "+/=") {
		t.Errorf("token = %q, want non-empty unpadded URL-safe token", response.Token)
	}
	hook, err := srv.store.HookByToken(context.Background(), response.Token)
	if err != nil {
		t.Fatalf("HookByToken: %v", err)
	}
	if hook.ChannelID != channel.ID || hook.PrincipalID != principal.ID {
		t.Errorf("stored hook = %+v", hook)
	}
}

func TestHookIngestPostsBroadcastsAndAudits(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantBody    string
		wantPayload bool
	}{
		{name: "typed payload", contentType: "application/json", body: `{"author_id":999,"body":"alert","payload":{"schema":"acme.alert.v1","data":{"level":2}}}`, wantBody: "alert", wantPayload: true},
		{name: "plain text", contentType: "text/plain; charset=utf-8", body: "monitor recovered", wantBody: "monitor recovered"},
		{name: "v1 body only", contentType: "application/json", body: `{"body":"build passed"}`, wantBody: "build passed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			channel, principal := createTestChannelAndPrincipal(t, srv)
			const token = "hook-token"
			if _, err := srv.store.CreateHook(context.Background(), token, channel.ID, principal.ID); err != nil {
				t.Fatalf("CreateHook: %v", err)
			}
			sub := srv.hub.SubscribeV1(channel.ID, 1)
			defer sub.Cancel()

			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+token, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
			}
			var response schema.PostMessageResponseV1
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			message := response.Message
			if message.ChannelID != channel.ID || message.AuthorID != principal.ID || message.Body != tt.wantBody {
				t.Errorf("posted message = %+v", message)
			}
			if (message.Payload != nil) != tt.wantPayload {
				t.Errorf("payload = %+v, want present %v", message.Payload, tt.wantPayload)
			}
			broadcast := <-sub.Messages()
			if broadcast.ID != message.ID || broadcast.AuthorID != principal.ID {
				t.Errorf("broadcast = %+v, want posted message %+v", broadcast, message)
			}
			events, err := srv.store.ListAuditEvents(context.Background(), 0, 10)
			if err != nil {
				t.Fatalf("ListAuditEvents: %v", err)
			}
			wantActor := fmt.Sprintf("principal:%d", principal.ID)
			if len(events) != 1 || events[0].Actor != wantActor || events[0].Action != "message.post" ||
				events[0].Subject != fmt.Sprintf("message:%d", message.ID) {
				t.Errorf("audit events = %+v", events)
			}
		})
	}
}

func TestHookIngestErrors(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "unknown token", token: "missing", body: `{"body":"hello"}`, wantStatus: http.StatusNotFound, wantCode: "hook_not_found"},
		{name: "malformed JSON", token: "hook-token", body: `{"body":`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "not a v1 post shape", token: "hook-token", body: `{"text":"build passed"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "empty body", token: "hook-token", body: `{"body":""}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			channel, principal := createTestChannelAndPrincipal(t, srv)
			if _, err := srv.store.CreateHook(context.Background(), "hook-token", channel.ID, principal.ID); err != nil {
				t.Fatalf("CreateHook: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+tt.token, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			assertAPIError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}
