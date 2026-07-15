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
	"strconv"
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

// ListMessages returns one forward page of v1 messages from channel.
func (c *Client) ListMessages(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "messages")
	query := endpoint.Query()
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return schema.ListMessagesResponseV1{}, fmt.Errorf("cli: create list messages request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return schema.ListMessagesResponseV1{}, fmt.Errorf("cli: list messages: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.ListMessagesResponseV1{}, decodeServerError(resp)
	}
	var result schema.ListMessagesResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.ListMessagesResponseV1{}, fmt.Errorf("cli: decode list messages response: %w", err)
	}
	return result, nil
}

// SendMessage posts a rendered v1 message to channel.
func (c *Client) SendMessage(ctx context.Context, channel string, authorID int64, body string) (schema.MessageV1, error) {
	requestBody, err := json.Marshal(schema.PostMessageRequestV1{AuthorID: authorID, Body: body})
	if err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: encode v1 post message request: %w", err)
	}
	endpoint := c.resolve("v1", "channels", channel, "messages")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: create v1 post message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: post v1 message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.MessageV1{}, decodeServerError(resp)
	}
	var result schema.PostMessageResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: decode v1 post message response: %w", err)
	}
	return result.Message, nil
}

// ListApprovals returns a list of open approvals.
func (c *Client) ListApprovals(ctx context.Context) (schema.ListApprovalsResponseV1, error) {
	endpoint := c.resolve("v1", "approvals")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return schema.ListApprovalsResponseV1{}, fmt.Errorf("cli: create list approvals request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return schema.ListApprovalsResponseV1{}, fmt.Errorf("cli: list approvals: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.ListApprovalsResponseV1{}, decodeServerError(resp)
	}
	var result schema.ListApprovalsResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.ListApprovalsResponseV1{}, fmt.Errorf("cli: decode list approvals response: %w", err)
	}
	return result, nil
}

// CastDecision posts a decision for an approval.
func (c *Client) CastDecision(ctx context.Context, approvalID int64, decision schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error) {
	requestBody, err := json.Marshal(decision)
	if err != nil {
		return schema.CastDecisionResponseV1{}, fmt.Errorf("cli: encode cast decision request: %w", err)
	}
	endpoint := c.resolve("v1", "approvals", strconv.FormatInt(approvalID, 10), "decisions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return schema.CastDecisionResponseV1{}, fmt.Errorf("cli: create cast decision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return schema.CastDecisionResponseV1{}, fmt.Errorf("cli: cast decision: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.CastDecisionResponseV1{}, decodeServerError(resp)
	}
	var result schema.CastDecisionResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.CastDecisionResponseV1{}, fmt.Errorf("cli: decode cast decision response: %w", err)
	}
	return result, nil
}

// Subscribe connects to channel's v1 stream and calls receive for every message.
func (c *Client) Subscribe(ctx context.Context, channel string, receive func(schema.MessageV1) error) error {
	endpoint := c.resolve("v1", "ws")
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
		return fmt.Errorf("cli: connect subscription: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		var message schema.MessageV1
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return fmt.Errorf("cli: read subscription: %w", err)
		}
		if err := receive(message); err != nil {
			return fmt.Errorf("cli: receive subscription message: %w", err)
		}
	}
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
