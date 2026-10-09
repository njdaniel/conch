package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestMCPEndpointPostMessageAndReadChannelParity(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	channel, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	agent, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "agent-smith")
	if err != nil {
		t.Fatalf("CreatePrincipal agent: %v", err)
	}
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	sessionID := mcpPost(t, httpSrv.URL, "", 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "conch-test", "version": "v0.0.0"},
		"capabilities":    map[string]any{},
	}, nil)

	payload := map[string]any{"schema": "leviathan.trade_signal.v1", "data": map[string]any{"symbol": "BTC", "side": "buy"}}
	var post struct {
		Result struct {
			StructuredContent schema.PostMessageResponseV1 `json:"structuredContent"`
		} `json:"result"`
	}
	mcpPost(t, httpSrv.URL, sessionID, 2, "tools/call", map[string]any{
		"name":      "post_message",
		"arguments": map[string]any{"channel": "general", "body": "buy BTC", "payload": payload},
	}, &post)
	if post.Result.StructuredContent.Message.AuthorID != agent.ID {
		t.Fatalf("MCP author ID = %d, want authenticated agent %d", post.Result.StructuredContent.Message.AuthorID, agent.ID)
	}
	if post.Result.StructuredContent.Message.ChannelID != channel.ID {
		t.Fatalf("MCP channel ID = %d, want %d", post.Result.StructuredContent.Message.ChannelID, channel.ID)
	}
	if post.Result.StructuredContent.Message.Payload == nil {
		t.Fatal("MCP payload is nil, want object payload")
	}
	if got := string(post.Result.StructuredContent.Message.Payload.Data); got != `{"side":"buy","symbol":"BTC"}` {
		t.Fatalf("MCP payload data = %s, want object payload", got)
	}

	var read struct {
		Result struct {
			StructuredContent schema.ListMessagesResponseV1 `json:"structuredContent"`
		} `json:"result"`
	}
	mcpPost(t, httpSrv.URL, sessionID, 3, "tools/call", map[string]any{
		"name":      "read_channel",
		"arguments": map[string]any{"channel": "general", "limit": 50},
	}, &read)
	if len(read.Result.StructuredContent.Messages) != 1 {
		t.Fatalf("MCP read returned %d messages, want 1", len(read.Result.StructuredContent.Messages))
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/channels/general/messages", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST read status = %d, body %s", rec.Code, rec.Body.String())
	}
	var rest schema.ListMessagesResponseV1
	if err := json.NewDecoder(rec.Body).Decode(&rest); err != nil {
		t.Fatalf("decode REST response: %v", err)
	}
	if len(rest.Messages) != 1 || rest.Messages[0].ID != read.Result.StructuredContent.Messages[0].ID {
		t.Fatalf("REST/MCP parity mismatch: REST %+v MCP %+v", rest.Messages, read.Result.StructuredContent.Messages)
	}
}

func TestMCPRejectsMissingBearer(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{}`)))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMCPApprovalFullChainAwaitAndCheck(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, human := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}

	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", approvalMCPArguments(channel.ID, time.Now().Add(time.Hour)))
	awaited := make(chan schema.AwaitDecisionOutput, 1)
	go func() {
		awaited <- mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, 2, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 5000})
	}()
	time.Sleep(300 * time.Millisecond)
	decideApprovalREST(t, srv, created.ID, human.ID)
	waitResult := <-awaited
	checked := mcpCallInProcess[schema.CheckDecisionOutput](t, srv, 3, "check_decision", map[string]any{"approval_id": created.ID})
	if waitResult.State != schema.ApprovalStateResolved || waitResult.Resolution == nil {
		t.Fatalf("await result = %+v, want resolved with resolution", waitResult)
	}
	if !reflect.DeepEqual(waitResult.Resolution, checked.Resolution) {
		t.Fatalf("await/check resolutions differ: await=%+v check=%+v", waitResult.Resolution, checked.Resolution)
	}
	clamped := mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, 4, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 999999})
	if clamped.EffectiveTimeoutMS != 60000 || !reflect.DeepEqual(clamped.Resolution, checked.Resolution) {
		t.Fatalf("clamped terminal await = %+v, want timeout 60000 and identical resolution", clamped)
	}

	events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, event := range events {
		if event.Subject == fmt.Sprintf("approval:%d", created.ID) {
			actions = append(actions, event.Action)
		}
	}
	want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("audit chain = %v, want %v", actions, want)
	}
}

func TestMCPAwaitDecisionTimeoutThenCheck(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, human := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}
	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", approvalMCPArguments(channel.ID, time.Now().Add(time.Hour)))

	started := time.Now()
	waited := mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, 2, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 100})
	elapsed := time.Since(started)
	if waited.State != schema.ApprovalStatePending || waited.Resolution != nil || waited.EffectiveTimeoutMS != 100 {
		t.Fatalf("timeout result = %+v, want pending without resolution and effective timeout 100", waited)
	}
	if elapsed < 80*time.Millisecond || elapsed > time.Second {
		t.Fatalf("await elapsed = %s, want approximately 100ms", elapsed)
	}

	decideApprovalREST(t, srv, created.ID, human.ID)
	checked := mcpCallInProcess[schema.CheckDecisionOutput](t, srv, 3, "check_decision", map[string]any{"approval_id": created.ID})
	if checked.State != schema.ApprovalStateResolved || checked.Resolution == nil {
		t.Fatalf("post-resolution check = %+v, want resolved with resolution", checked)
	}
}

// request_approval must accept custom-kind options (approval-object.md §1
// documents e.g. "approve with size X") alongside the required approve/reject
// pair — it must not be more restrictive than the REST path for the same
// operation (CLAUDE.md rule 4, API parity).
func TestMCPRequestApprovalAllowsCustomOption(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, _ := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}

	args := approvalMCPArguments(channel.ID, time.Now().Add(time.Hour))
	args["options"] = []map[string]any{
		{"id": "approve", "kind": "approve", "label": "Approve"},
		{"id": "reject", "kind": "reject", "label": "Reject"},
		{"id": "approve-half-size", "kind": "custom", "label": "Approve at half size"},
	}
	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", args)
	if created.ID == 0 {
		t.Fatalf("request_approval with a custom option failed: %+v", created)
	}
}

func TestMCPAwaitDecisionConcurrentConsistency(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, human := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}
	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", approvalMCPArguments(channel.ID, time.Now().Add(time.Hour)))

	const awaiters = 8
	results := make([]schema.AwaitDecisionOutput, awaiters)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, i+2, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 5000})
		}(i)
	}
	close(start)
	time.Sleep(300 * time.Millisecond)
	decideApprovalREST(t, srv, created.ID, human.ID)
	wg.Wait()
	for i := range results {
		if results[i].State != schema.ApprovalStateResolved || results[i].Resolution == nil {
			t.Fatalf("awaiter %d result = %+v, want resolved", i, results[i])
		}
		if !reflect.DeepEqual(results[0].Resolution, results[i].Resolution) {
			t.Fatalf("awaiter resolutions differ: first=%+v waiter %d=%+v", results[0].Resolution, i, results[i].Resolution)
		}
	}
}

func approvalMCPArguments(channelID int64, deadline time.Time) map[string]any {
	return map[string]any{
		"channel_id": channelID, "title": "Enter BTC long", "body": "Signal fired; approve to place the order.",
		"options":  []map[string]any{{"id": "approve", "kind": "approve", "label": "Approve"}, {"id": "reject", "kind": "reject", "label": "Reject"}},
		"deadline": deadline.Format(time.RFC3339Nano), "quorum": 1,
	}
}

func decideApprovalREST(t *testing.T, srv *Server, approvalID, humanID int64) {
	t.Helper()
	body := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"risk is fine"}`, humanID)
	rec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", approvalID), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func mcpCallInProcess[T any](t *testing.T, srv *Server, id int, name string, arguments map[string]any) T {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("MCP %s status = %d, body = %s", name, rec.Code, rec.Body.String())
	}
	var response struct {
		Result struct {
			StructuredContent T    `json:"structuredContent"`
			IsError           bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP %s response %s: %v", name, rec.Body.String(), err)
	}
	t.Logf("MCP %s response: %s", name, rec.Body.String())
	if response.Result.IsError {
		t.Fatalf("MCP %s returned tool error: %s", name, rec.Body.String())
	}
	return response.Result.StructuredContent
}

func mcpPost(t *testing.T, baseURL, sessionID string, id int, method string, params map[string]any, out any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Accept", "text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", method, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close %s response: %v", method, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s status = %d", method, resp.StatusCode)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	if out != nil {
		if err := json.Unmarshal(responseBody, out); err != nil {
			t.Fatalf("decode %s response %s: %v", method, responseBody, err)
		}
	}
	t.Logf("%s response: %s", method, responseBody)
	if sessionID == "" {
		return resp.Header.Get("Mcp-Session-Id")
	}
	return sessionID
}

// mcpAuthFixture is a server with one channel, an agent, a second agent, and
// a human, for exercising /mcp authentication (issue #97).
type mcpAuthFixture struct {
	srv                 *Server
	agent, other, human store.Principal
}

func newMCPAuthFixture(t *testing.T, cfg Config) mcpAuthFixture {
	t.Helper()
	ctx := context.Background()
	srv := newTestServerWithConfig(t, cfg)
	if _, err := srv.store.CreateChannel(ctx, "general"); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	f := mcpAuthFixture{srv: srv}
	var err error
	if f.agent, err = srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "agent-one"); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if f.other, err = srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "agent-two"); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if f.human, err = srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "nick"); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return f
}

func (f mcpAuthFixture) issue(t *testing.T, principalID int64, expiresAt *time.Time) (schema.CredentialV1, string) {
	t.Helper()
	cred, token, err := f.srv.store.CreateCredential(context.Background(), principalID, "test", expiresAt)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return cred, token
}

// post calls post_message with the given Authorization header value and
// extra tool arguments, returning the HTTP response.
func (f mcpAuthFixture) post(t *testing.T, authorization string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	arguments := map[string]any{"channel": "general", "body": "hello"}
	for k, v := range extra {
		arguments[k] = v
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "post_message", "arguments": arguments}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// authors returns the author of every message in the fixture channel.
func (f mcpAuthFixture) authors(t *testing.T) []int64 {
	t.Helper()
	ctx := context.Background()
	channel, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := f.srv.store.ListMessages(ctx, channel.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(messages))
	for i, m := range messages {
		out[i] = m.AuthorID
	}
	return out
}

func TestMCPAuthenticatesIssuedCredential(t *testing.T) {
	f := newMCPAuthFixture(t, Config{})
	_, token := f.issue(t, f.agent.ID, nil)

	rec := f.post(t, "Bearer "+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.authors(t); len(got) != 1 || got[0] != f.agent.ID {
		t.Fatalf("message authors = %v, want [%d]", got, f.agent.ID)
	}
}

func TestMCPRejectsInvalidCredentialsIdentically(t *testing.T) {
	logs := captureLogs(t)
	staticShaped := schema.CredentialTokenPrefix + strings.Repeat("B", 43)
	f := newMCPAuthFixture(t, Config{})
	f.srv.cfg.MCPBearerTokens = map[string]int64{staticShaped: f.agent.ID}

	revokedCred, revokedToken := f.issue(t, f.agent.ID, nil)
	if err := f.srv.store.RevokeCredential(context.Background(), revokedCred.ID); err != nil {
		t.Fatal(err)
	}
	rotatedCred, rotatedOld := f.issue(t, f.agent.ID, nil)
	if _, _, err := f.srv.store.RotateCredential(context.Background(), rotatedCred.ID); err != nil {
		t.Fatal(err)
	}
	_, humanToken := f.issue(t, f.human.ID, nil)
	soon := time.Now().Add(150 * time.Millisecond)
	_, expiringToken := f.issue(t, f.agent.ID, &soon)
	// Wait for the short-lived credential to expire, without sleeping blindly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := f.srv.store.ResolveCredential(context.Background(), expiringToken); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("credential did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}

	tests := []struct {
		name          string
		authorization string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic " + revokedToken},
		{"bearer with no token", "Bearer "},
		{"unknown well-formed token", "Bearer " + schema.CredentialTokenPrefix + strings.Repeat("A", 43)},
		{"malformed token", "Bearer conch_short"},
		{"unknown opaque token", "Bearer not-a-token"},
		{"revoked", "Bearer " + revokedToken},
		{"rotated away", "Bearer " + rotatedOld},
		{"expired", "Bearer " + expiringToken},
		{"human credential", "Bearer " + humanToken},
		{"credential-shaped token in the static map", "Bearer " + staticShaped},
	}
	var wantBody string
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.post(t, tt.authorization, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch-mcp"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if i == 0 {
				wantBody = rec.Body.String()
			} else if rec.Body.String() != wantBody {
				t.Errorf("body = %q, want the same body as every other failure %q", rec.Body.String(), wantBody)
			}
		})
	}
	if got := f.authors(t); len(got) != 0 {
		t.Errorf("rejected requests posted messages: authors %v", got)
	}
	for _, secret := range []string{revokedToken, rotatedOld, humanToken, expiringToken, staticShaped} {
		if strings.Contains(logs.buf.String(), secret) {
			t.Errorf("a token was written to the log")
		}
	}
}

func TestMCPRotationTakesEffectWithoutRestart(t *testing.T) {
	f := newMCPAuthFixture(t, Config{})
	cred, oldToken := f.issue(t, f.agent.ID, nil)
	if rec := f.post(t, "Bearer "+oldToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("before rotation: status = %d", rec.Code)
	}
	_, newToken, err := f.srv.store.RotateCredential(context.Background(), cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, "Bearer "+oldToken, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("old token after rotation: status = %d, want 401", rec.Code)
	}
	if rec := f.post(t, "Bearer "+newToken, nil); rec.Code != http.StatusOK {
		t.Errorf("new token after rotation: status = %d, want 200", rec.Code)
	}
}

// A tool input cannot choose who the call acts as: the principal comes only
// from the credential.
func TestMCPToolInputCannotOverridePrincipal(t *testing.T) {
	f := newMCPAuthFixture(t, Config{})
	_, token := f.issue(t, f.agent.ID, nil)
	for _, field := range []string{"author_id", "principal_id", "requester_id"} {
		f.post(t, "Bearer "+token, map[string]any{field: f.other.ID})
	}
	for _, author := range f.authors(t) {
		if author != f.agent.ID {
			t.Fatalf("a message was authored by principal %d, want only %d", author, f.agent.ID)
		}
	}
}

func TestMCPStoreFailureFailsClosed(t *testing.T) {
	logs := captureLogs(t)
	f := newMCPAuthFixture(t, Config{})
	_, token := f.issue(t, f.agent.ID, nil)
	f.srv.cfg.MCPBearerTokens = map[string]int64{"static-token": f.agent.ID}
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{token, "static-token"} {
		if rec := f.post(t, "Bearer "+tok, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("status with the store down = %d, want 401", rec.Code)
		}
	}
	if strings.Contains(logs.buf.String(), token) {
		t.Error("the token was written to the log")
	}
}

func TestMCPStaticTokenStillWorksAndWarnsOnce(t *testing.T) {
	logs := captureLogs(t)
	// The agent created first in the fixture has id 1.
	f := newMCPAuthFixture(t, Config{MCPBearerTokens: map[string]int64{"static-token": 1}})
	if f.agent.ID != 1 {
		t.Fatalf("fixture agent id = %d, want 1", f.agent.ID)
	}
	if rec := f.post(t, "Bearer static-token", nil); rec.Code != http.StatusOK {
		t.Fatalf("static token: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.authors(t); len(got) != 1 || got[0] != f.agent.ID {
		t.Fatalf("message authors = %v, want [%d]", got, f.agent.ID)
	}
	if n := strings.Count(logs.buf.String(), "deprecated"); n != 1 {
		t.Errorf("deprecation warnings = %d, want 1; log:\n%s", n, logs.buf.String())
	}
	if strings.Contains(logs.buf.String(), "static-token") {
		t.Error("the static token was written to the log")
	}
}

func TestMCPNoDeprecationWarningWithoutStaticTokens(t *testing.T) {
	logs := captureLogs(t)
	newMCPAuthFixture(t, Config{})
	if strings.Contains(logs.buf.String(), "deprecated") {
		t.Errorf("unexpected deprecation warning:\n%s", logs.buf.String())
	}
}
