package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s status = %d", method, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", method, err)
		}
	}
	if sessionID == "" {
		return resp.Header.Get("Mcp-Session-Id")
	}
	return sessionID
}
