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
