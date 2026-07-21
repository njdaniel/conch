// Package mcpclient provides Conch's small streamable-HTTP MCP client.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// Client calls a conchd MCP endpoint.
type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	sessionID string
	nextID    int
	mu        sync.Mutex
}

// New constructs a client using http.DefaultClient.
func New(baseURL, token string) *Client {
	return &Client{baseURL: baseURL, token: token, http: http.DefaultClient}
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context, clientName string) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": clientName, "version": "1"},
		"capabilities":    map[string]any{},
	})
	return err
}

// CallTool calls any named MCP tool and returns its structured content.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (json.RawMessage, error) {
	return c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
}

// ReadChannel reads one page from the read_channel tool.
func (c *Client) ReadChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV1, error) {
	arguments := map[string]any{"channel": channel}
	if after != 0 {
		arguments["after"] = after
	}
	if limit != 0 {
		arguments["limit"] = limit
	}
	raw, err := c.CallTool(ctx, "read_channel", arguments)
	if err != nil {
		return schema.ListMessagesResponseV1{}, err
	}
	return Decode[schema.ListMessagesResponseV1](raw)
}

// PostMessage posts an untyped message through the post_message tool.
func (c *Client) PostMessage(ctx context.Context, channel, body string) (schema.PostMessageResponseV1, error) {
	raw, err := c.CallTool(ctx, "post_message", map[string]any{"channel": channel, "body": body})
	if err != nil {
		return schema.PostMessageResponseV1{}, err
	}
	return Decode[schema.PostMessageResponseV1](raw)
}

// Decode unmarshals a tool's structured content into a schema type.
func Decode[T any](raw json.RawMessage) (T, error) {
	var out T
	err := json.Unmarshal(raw, &out)
	return out, err
}

func (c *Client) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	sessionID := c.sessionID
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp %s status %d: %s", method, resp.StatusCode, respBody)
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode mcp %s response: %w (body=%s)", method, err, respBody)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("mcp %s rpc error: %s", method, envelope.Error.Message)
	}
	if envelope.Result.IsError {
		var text string
		if len(envelope.Result.Content) > 0 {
			text = envelope.Result.Content[0].Text
		}
		return nil, fmt.Errorf("mcp %s tool error: %s", method, text)
	}
	return envelope.Result.StructuredContent, nil
}
