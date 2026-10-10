package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
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

// TestCreateHookAuditActor: creating a hook appends exactly one hook_created
// event; under --auth required its actor is the caller, under --auth off it
// is "system".
func TestCreateHookAuditActor(t *testing.T) {
	tests := []struct {
		name      string
		mode      AuthMode
		token     func(f *authFixture) string
		wantActor func(f *authFixture) string
	}{
		{"auth required attributes the caller", AuthRequired,
			func(f *authFixture) string { return f.rootTok },
			func(f *authFixture) string { return fmt.Sprintf("principal:%d", f.root.ID) }},
		{"auth off attributes system", AuthOff,
			func(f *authFixture) string { return "" },
			func(f *authFixture) string { return "system" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, tt.mode)
			rec := f.do(t, http.MethodPost, "/v1/hooks", tt.token(f),
				fmt.Sprintf(`{"channel":"general","principal":%d,"label":"ci"}`, f.alice.ID))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body)
			}
			resp := decodeBody[schema.CreateHookResponse](t, rec)
			general, err := f.srv.store.ChannelByName(context.Background(), "general")
			if err != nil {
				t.Fatal(err)
			}
			var created []store.AuditEvent
			for _, e := range f.audit(t) {
				if e.Action == "hook_created" {
					created = append(created, e)
				}
			}
			wantDetail := fmt.Sprintf("hook=%d channel=%d label=%q", resp.ID, general.ID, "ci")
			if len(created) != 1 || created[0].Actor != tt.wantActor(f) ||
				created[0].Subject != fmt.Sprintf("principal:%d", f.alice.ID) || created[0].Detail != wantDetail {
				t.Errorf("hook_created events = %+v, want one with actor %q, subject principal:%d, detail %q",
					created, tt.wantActor(f), f.alice.ID, wantDetail)
			}
		})
	}
}

// TestHookRedaction mirrors TestCredentialRedaction for hooks: across success
// and failure creates, an ingest and a store outage, the hook's token and its
// hash appear in no audit event, no log line and no response body other than
// the create response's token field.
func TestHookRedaction(t *testing.T) {
	logs := captureServerLogs(t)
	f := newAuthFixture(t, AuthRequired)

	created := f.do(t, http.MethodPost, "/v1/hooks", f.rootTok,
		fmt.Sprintf(`{"channel":"general","principal":%d,"label":"redact"}`, f.alice.ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	// The create body is the one place the token may appear; it is not scanned.
	token := decodeBody[schema.CreateHookResponse](t, created).Token
	secrets := []string{token, sha256Hex(token)}

	var texts []string
	record := func(rec *httptest.ResponseRecorder) { texts = append(texts, rec.Body.String()) }

	// Failure cases: unknown channel, unknown principal, malformed body, too
	// long a label; and the list and the ingest of the live hook.
	record(f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, `{"channel":"missing","principal":1}`))
	record(f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, `{"channel":"general","principal":9999}`))
	record(f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, `{"channel":"general"`))
	record(f.do(t, http.MethodPost, "/v1/hooks", f.rootTok,
		fmt.Sprintf(`{"channel":"general","principal":%d,"label":%q}`, f.alice.ID, strings.Repeat("x", schema.MaxHookLabelLength+1))))
	record(f.do(t, http.MethodGet, "/v1/hooks", f.rootTok, ""))
	record(ingest(f, t, token))

	for _, e := range f.audit(t) {
		texts = append(texts, e.Actor+e.Action+e.Subject+e.Detail)
	}

	// A store outage exercises the 500 paths and their log lines.
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodPost, "/v1/hooks", f.rootTok,
		fmt.Sprintf(`{"channel":"general","principal":%d}`, f.alice.ID))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("create during outage = %d, want 500", rec.Code)
	}
	record(rec)

	if logs.Len() == 0 {
		t.Fatal("no log output captured; the outage case should have logged")
	}
	texts = append(texts, logs.String())
	for i, text := range texts {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("output %d contains hook token material %q...: %.200s", i, secret[:6], text)
			}
		}
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
			if _, err := srv.store.CreateHook(context.Background(), "system", token, channel.ID, principal.ID); err != nil {
				t.Fatalf("CreateHook: %v", err)
			}
			sub := srv.hub.SubscribeV1(channel.ID, 0, 1)
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
			select {
			case broadcast, ok := <-sub.Messages():
				if !ok {
					t.Fatal("subscription closed, want a message")
				}
				if broadcast.ID != message.ID || broadcast.AuthorID != principal.ID {
					t.Errorf("broadcast = %+v, want posted message %+v", broadcast, message)
				}
			default:
				t.Fatal("no message buffered, want one")
			}
			events, err := srv.store.ListAuditEvents(context.Background(), 0, 10)
			if err != nil {
				t.Fatalf("ListAuditEvents: %v", err)
			}
			var posted []store.AuditEvent
			for _, e := range events {
				if e.Action == "message.post" {
					posted = append(posted, e)
				}
			}
			wantActor := fmt.Sprintf("principal:%d", principal.ID)
			if len(posted) != 1 || posted[0].Actor != wantActor ||
				posted[0].Subject != fmt.Sprintf("message:%d", message.ID) {
				t.Errorf("message.post audit events = %+v", posted)
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
			if _, err := srv.store.CreateHook(context.Background(), "system", "hook-token", channel.ID, principal.ID); err != nil {
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

// captureServerLogs routes the default slog logger into a buffer for the test.
func captureServerLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&sb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &sb
}

func ingest(f *authFixture, t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+token, strings.NewReader(`{"body":"from hook"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHookRevokeEndToEnd drives the whole chain over HTTP: create a hook,
// post through it, list it, revoke it, and check that the dead URL is
// indistinguishable from one that never existed, the revocation is audited
// with the caller as actor, and the token appears nowhere it should not.
func TestHookRevokeEndToEnd(t *testing.T) {
	logs := captureServerLogs(t)
	f := newAuthFixture(t, AuthRequired)

	created := f.do(t, "POST", "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"general","principal":%d,"label":"ci builds"}`, f.alice.ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	if got := created.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("create response Cache-Control = %q, want no-store", got)
	}
	resp := decodeBody[schema.CreateHookResponse](t, created)
	if resp.Token == "" || resp.ID <= 0 {
		t.Fatalf("create response = %+v", resp)
	}
	token := resp.Token

	if rec := ingest(f, t, token); rec.Code != http.StatusCreated {
		t.Fatalf("ingest through live hook: %d %s", rec.Code, rec.Body)
	}

	listed := f.do(t, "GET", "/v1/hooks", f.rootTok, "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listed.Code, listed.Body)
	}
	list := decodeBody[schema.ListHooksResponseV1](t, listed)
	if len(list.Hooks) != 1 || list.Hooks[0].ID != resp.ID || list.Hooks[0].Channel != "general" ||
		list.Hooks[0].Principal != f.alice.ID || list.Hooks[0].Label != "ci builds" || list.Hooks[0].RevokedAt != nil {
		t.Fatalf("list = %+v", list)
	}

	unknown := ingest(f, t, "never-existed-"+token)
	revokePath := fmt.Sprintf("/v1/hooks/%d", resp.ID)
	if rec := f.do(t, "DELETE", revokePath, f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	// Idempotent: a second revoke succeeds and audits nothing more.
	if rec := f.do(t, "DELETE", revokePath, f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("second revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "DELETE", "/v1/hooks/99999", f.rootTok, ""); rec.Code != http.StatusNotFound {
		t.Errorf("revoke unknown id: %d", rec.Code)
	}
	if rec := f.do(t, "DELETE", "/v1/hooks/"+token, f.rootTok, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("revoke by token must be rejected, got %d", rec.Code)
	}

	// Unknown, revoked and malformed tokens answer byte for byte alike.
	revokedIngest := ingest(f, t, token)
	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"revoked", revokedIngest},
		{"malformed", ingest(f, t, "%00%ff..%2f")},
		{"short", ingest(f, t, "x")},
		{"hash-shaped", ingest(f, t, strings.Repeat("a", 64))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rec.Code != unknown.Code || tc.rec.Body.String() != unknown.Body.String() {
				t.Errorf("response = %d %q, want %d %q", tc.rec.Code, tc.rec.Body, unknown.Code, unknown.Body)
			}
			if !reflect.DeepEqual(tc.rec.Header(), unknown.Header()) {
				t.Errorf("headers = %v, want %v", tc.rec.Header(), unknown.Header())
			}
		})
	}

	list = decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.rootTok, ""))
	if len(list.Hooks) != 1 || list.Hooks[0].RevokedAt == nil {
		t.Errorf("list after revoke = %+v, want the hook with revoked_at set", list)
	}

	var revoked []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "hook_revoked" {
			revoked = append(revoked, e)
		}
	}
	if len(revoked) != 1 || revoked[0].Actor != fmt.Sprintf("principal:%d", f.root.ID) ||
		revoked[0].Subject != fmt.Sprintf("principal:%d", f.alice.ID) {
		t.Fatalf("hook_revoked events = %+v, want exactly one with the operator as actor", revoked)
	}

	// Redaction: the token is in the create response only.
	for _, e := range f.audit(t) {
		if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, token) {
			t.Errorf("audit event carries the hook token: %+v", e)
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("logs contain the hook token:\n%s", logs)
	}
	for _, rec := range []*httptest.ResponseRecorder{unknown, listed, revokedIngest} {
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("a response body carries the hook token: %s", rec.Body)
		}
	}
}

// TestHookAdminRequiresOperator spot-checks the two new routes as each kind of
// caller (the exhaustive matrix lives in TestRoleMatrix).
func TestHookAdminRequiresOperator(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name, method, path, token string
		want                      int
	}{
		{"list as operator", "GET", "/v1/hooks", f.rootTok, http.StatusOK},
		{"list as member", "GET", "/v1/hooks", f.aliceTok, http.StatusForbidden},
		{"list as agent", "GET", "/v1/hooks", f.botTok, http.StatusForbidden},
		{"list unauthenticated", "GET", "/v1/hooks", "", http.StatusUnauthorized},
		{"revoke as member", "DELETE", "/v1/hooks/1", f.aliceTok, http.StatusForbidden},
		{"revoke as agent", "DELETE", "/v1/hooks/1", f.botTok, http.StatusForbidden},
		{"revoke unauthenticated", "DELETE", "/v1/hooks/1", "", http.StatusUnauthorized},
		{"revoke bad id", "DELETE", "/v1/hooks/abc", f.rootTok, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := f.do(t, tt.method, tt.path, tt.token, ""); rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestCreateHookRejectsLongLabel(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	body := fmt.Sprintf(`{"channel":"general","principal":%d,"label":%q}`, f.alice.ID, strings.Repeat("x", schema.MaxHookLabelLength+1))
	if rec := f.do(t, "POST", "/v1/hooks", f.rootTok, body); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHookAdminAuthOff: with authentication off the admin endpoints are open
// like the other admin endpoints, and the revocation is attributed to "system".
func TestHookAdminAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	created := f.do(t, "POST", "/v1/hooks", "", fmt.Sprintf(`{"channel":"general","principal":%d}`, f.alice.ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	id := decodeBody[schema.CreateHookResponse](t, created).ID
	if rec := f.do(t, "GET", "/v1/hooks", "", ""); rec.Code != http.StatusOK {
		t.Errorf("list: %d", rec.Code)
	}
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/hooks/%d", id), "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("revoke: %d", rec.Code)
	}
	n := 0
	for _, e := range f.audit(t) {
		if e.Action == "hook_revoked" {
			n++
			if e.Actor != "system" {
				t.Errorf("actor = %q, want system", e.Actor)
			}
		}
	}
	if n != 1 {
		t.Errorf("hook_revoked events = %d, want 1", n)
	}
}
