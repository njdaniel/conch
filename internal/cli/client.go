package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

// Client talks to conchd through its public REST and WebSocket API.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// NewClient creates a client for server, which must be an HTTP or HTTPS URL.
func NewClient(server string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("cli: parse server URL: %w", err)
	}
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, errors.New("cli: server must be an http(s) URL")
	}
	if baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("cli: server URL must not contain a query or fragment")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}, nil
}

// Send posts a message to channel and returns the persisted message.
func (c *Client) Send(ctx context.Context, channel string, authorID int64, body string) (schema.MessageV0, error) {
	requestBody, err := json.Marshal(schema.PostMessageRequest{AuthorID: authorID, Body: body})
	if err != nil {
		return schema.MessageV0{}, fmt.Errorf("cli: encode post message request: %w", err)
	}
	endpoint := c.resolve("v0", "channels", channel, "messages")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return schema.MessageV0{}, fmt.Errorf("cli: create post message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return schema.MessageV0{}, fmt.Errorf("cli: post message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.MessageV0{}, decodeServerError(resp)
	}
	var result schema.PostMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.MessageV0{}, fmt.Errorf("cli: decode post message response: %w", err)
	}
	return result.Message, nil
}

// Tail connects to channel's live stream and calls receive for every message.
// A server shutdown is reported through websocket.StatusGoingAway so callers
// can distinguish it from failures.
func (c *Client) Tail(ctx context.Context, channel string, receive func(schema.MessageV0) error) error {
	endpoint := c.resolve("v0", "ws")
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	query := endpoint.Query()
	query.Set("channel", channel)
	endpoint.RawQuery = query.Encode()

	conn, resp, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPClient: c.httpClient})
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return decodeServerError(resp)
		}
		return fmt.Errorf("cli: connect tail: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	for {
		var message schema.MessageV0
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return fmt.Errorf("cli: read tail: %w", err)
		}
		if err := receive(message); err != nil {
			return fmt.Errorf("cli: receive tail message: %w", err)
		}
	}
}

func (c *Client) resolve(parts ...string) *url.URL {
	base := *c.baseURL
	baseEscapedPath := base.EscapedPath()
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.Join(parts, "/")
	escapedParts := make([]string, len(parts))
	for i, part := range parts {
		escapedParts[i] = url.PathEscape(part)
	}
	base.RawPath = strings.TrimRight(baseEscapedPath, "/") + "/" + strings.Join(escapedParts, "/")
	return &base
}

func decodeServerError(resp *http.Response) error {
	var serverError schema.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&serverError); err != nil {
		return fmt.Errorf("cli: server returned %s", resp.Status)
	}
	return fmt.Errorf("cli: server error %s: %s", serverError.Code, serverError.Message)
}
