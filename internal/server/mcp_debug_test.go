package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
)

func TestMCPDebug(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	_, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	agent, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "agent-smith")
	if err != nil {
		t.Fatalf("CreatePrincipal agent: %v", err)
	}
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}
	t.Logf("Agent ID: %d", agent.ID)
	t.Logf("MCPBearerTokens: %v", srv.cfg.MCPBearerTokens)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	doPost := func(sessionID string, id int, method string, params map[string]any) ([]byte, http.Header, int) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequest("POST", httpSrv.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+"token-1")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("Accept", "application/json")
		req.Header.Add("Accept", "text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.Header, resp.StatusCode
	}

	b1, h1, s1 := doPost("", 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "conch-test", "version": "v0"},
		"capabilities":    map[string]any{},
	})
	t.Logf("initialize status: %d, body: %s", s1, string(b1))
	sessionID := h1.Get("Mcp-Session-Id")
	t.Logf("Session ID: %q", sessionID)

	payload := map[string]any{"schema": "leviathan.trade_signal.v1", "data": map[string]any{"symbol": "BTC", "side": "buy"}}
	b2, _, s2 := doPost(sessionID, 2, "tools/call", map[string]any{
		"name":      "post_message",
		"arguments": map[string]any{"channel": "general", "body": "buy BTC", "payload": payload},
	})
	t.Logf("tools/call status: %d, body: %s", s2, string(b2))

	var parsed map[string]any
	json.Unmarshal(b2, &parsed)
	formatted, _ := json.MarshalIndent(parsed, "", "  ")
	fmt.Printf("post_message response:\n%s\n", string(formatted))
}
