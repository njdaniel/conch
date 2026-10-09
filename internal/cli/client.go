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
	token      string
}

// ErrUnauthenticated matches the error returned when the server answers 401.
var ErrUnauthenticated = errors.New("unauthenticated")

// UnauthenticatedError reports that the server rejected the request's
// credential, or that none was sent. Its text is the one-line login hint.
type UnauthenticatedError struct{ Server string }

func (e *UnauthenticatedError) Error() string {
	return fmt.Sprintf("not logged in to %s: run 'conch login'", e.Server)
}

// Is makes errors.Is(err, ErrUnauthenticated) true.
func (e *UnauthenticatedError) Is(target error) bool { return target == ErrUnauthenticated }

// WithToken makes the client send token as a bearer credential on every
// request, including the WebSocket upgrade. An empty token sends none.
func (c *Client) WithToken(token string) *Client {
	c.token = token
	return c
}

// Server returns the normalised server URL (scheme and host).
func (c *Client) Server() string {
	return normalizeURL(c.baseURL)
}

// do sends req with the credential, when there is one.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.httpClient.Do(req)
}

// WhoAmI returns the principal the client's credential resolves to.
func (c *Client) WhoAmI(ctx context.Context) (schema.WhoAmIResponseV1, error) {
	endpoint := c.resolve("v1", "whoami")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return schema.WhoAmIResponseV1{}, fmt.Errorf("cli: create whoami request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return schema.WhoAmIResponseV1{}, fmt.Errorf("cli: whoami: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.WhoAmIResponseV1{}, c.decodeError(resp)
	}
	var result schema.WhoAmIResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.WhoAmIResponseV1{}, fmt.Errorf("cli: decode whoami response: %w", err)
	}
	return result, nil
}

func (c *Client) dialOptions() *websocket.DialOptions {
	opts := &websocket.DialOptions{HTTPClient: c.httpClient}
	if c.token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + c.token}}
	}
	return opts
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
	resp, err := c.do(req)
	if err != nil {
		return schema.ListMessagesResponseV1{}, fmt.Errorf("cli: list messages: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.ListMessagesResponseV1{}, c.decodeError(resp)
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
	resp, err := c.do(req)
	if err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: post v1 message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.MessageV1{}, c.decodeError(resp)
	}
	var result schema.PostMessageResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.MessageV1{}, fmt.Errorf("cli: decode v1 post message response: %w", err)
	}
	return result.Message, nil
}

// ListChannels returns every channel on the server, ordered by id.
func (c *Client) ListChannels(ctx context.Context) (schema.ListChannelsResponse, error) {
	endpoint := c.resolve("v1", "channels")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return schema.ListChannelsResponse{}, fmt.Errorf("cli: create list channels request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return schema.ListChannelsResponse{}, fmt.Errorf("cli: list channels: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.ListChannelsResponse{}, c.decodeError(resp)
	}
	var result schema.ListChannelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return schema.ListChannelsResponse{}, fmt.Errorf("cli: decode list channels response: %w", err)
	}
	return result, nil
}

// ListApprovals returns a list of open approvals.
func (c *Client) ListApprovals(ctx context.Context) (schema.ListApprovalsResponseV1, error) {
	endpoint := c.resolve("v1", "approvals")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return schema.ListApprovalsResponseV1{}, fmt.Errorf("cli: create list approvals request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return schema.ListApprovalsResponseV1{}, fmt.Errorf("cli: list approvals: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.ListApprovalsResponseV1{}, c.decodeError(resp)
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
	resp, err := c.do(req)
	if err != nil {
		return schema.CastDecisionResponseV1{}, fmt.Errorf("cli: cast decision: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.CastDecisionResponseV1{}, c.decodeError(resp)
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
	conn, resp, err := websocket.Dial(ctx, endpoint.String(), c.dialOptions())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
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
	// Never follow redirects: the bearer token must only go where the user
	// pointed --server. A caller-supplied client is copied, not trusted.
	if httpClient == nil {
		httpClient = &http.Client{}
	} else {
		clone := *httpClient
		httpClient = &clone
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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

	resp, err := c.do(req)
	if err != nil {
		return schema.MessageV0{}, fmt.Errorf("cli: post message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return schema.MessageV0{}, c.decodeError(resp)
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

	conn, resp, err := websocket.Dial(ctx, endpoint.String(), c.dialOptions())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
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

func (c *Client) decodeError(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("server redirected to %s (HTTP %d); set --server to the final URL", resp.Header.Get("Location"), resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &UnauthenticatedError{Server: c.Server()}
	}
	var serverError schema.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&serverError); err != nil {
		return fmt.Errorf("cli: server returned %s", resp.Status)
	}
	return fmt.Errorf("cli: server error %s: %s", serverError.Code, serverError.Message)
}

// roundTrip sends one JSON request and, when out is non-nil, decodes the
// response into it. Any status outside 2xx becomes the server's error. The
// nets and v2 message methods share it so each stays a few lines.
func (c *Client) roundTrip(ctx context.Context, what, method string, endpoint *url.URL, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("cli: encode %s request: %w", what, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("cli: create %s request: %w", what, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("cli: %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return c.decodeError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("cli: decode %s response: %w", what, err)
	}
	return nil
}

// PostMessageV2 posts a message to channel through the v2 route. A nil
// audience posts channel-wide; otherwise the message is scoped to a net or to
// an explicit list of principals. The server binds the author when the client
// has a credential.
func (c *Client) PostMessageV2(ctx context.Context, channel string, authorID int64, body string, audience *schema.Audience) (schema.MessageV2, error) {
	request := schema.PostMessageRequestV2{AuthorID: authorID, Body: body, Audience: audience}
	var result schema.PostMessageResponseV2
	endpoint := c.resolve("v2", "channels", channel, "messages")
	if err := c.roundTrip(ctx, "post v2 message", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.MessageV2{}, err
	}
	return result.Message, nil
}

// ListMessagesV2 returns one forward page of the v2 messages in channel that
// the caller may see.
func (c *Client) ListMessagesV2(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	endpoint := c.resolve("v2", "channels", channel, "messages")
	query := endpoint.Query()
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	var result schema.ListMessagesResponseV2
	if err := c.roundTrip(ctx, "list v2 messages", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListMessagesResponseV2{}, err
	}
	return result, nil
}

// SubscribeV2 connects to channel's v2 stream and calls receive for every
// message the caller may see, scoped ones included. A server shutdown is
// reported through websocket.StatusGoingAway.
func (c *Client) SubscribeV2(ctx context.Context, channel string, receive func(schema.MessageV2) error) error {
	endpoint := c.resolve("v2", "ws")
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	query := endpoint.Query()
	query.Set("channel", channel)
	endpoint.RawQuery = query.Encode()
	conn, resp, err := websocket.Dial(ctx, endpoint.String(), c.dialOptions())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
		}
		return fmt.Errorf("cli: connect v2 subscription: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		var message schema.MessageV2
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return fmt.Errorf("cli: read v2 subscription: %w", err)
		}
		if err := receive(message); err != nil {
			return fmt.Errorf("cli: receive v2 subscription message: %w", err)
		}
	}
}

// ListNets returns the nets of channel that the caller may see, with rosters.
func (c *Client) ListNets(ctx context.Context, channel string) (schema.ListNetsResponseV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "nets")
	var result schema.ListNetsResponseV1
	if err := c.roundTrip(ctx, "list nets", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListNetsResponseV1{}, err
	}
	return result, nil
}

// CreateNet creates the named net in channel.
func (c *Client) CreateNet(ctx context.Context, channel, name string) (schema.NetV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "nets")
	var result schema.CreateNetResponseV1
	if err := c.roundTrip(ctx, "create net", http.MethodPost, endpoint, schema.CreateNetRequestV1{Name: name}, &result); err != nil {
		return schema.NetV1{}, err
	}
	return result.Net, nil
}

// ArchiveNet archives the named net of channel.
func (c *Client) ArchiveNet(ctx context.Context, channel, name string) error {
	endpoint := c.resolve("v1", "channels", channel, "nets", name)
	return c.roundTrip(ctx, "archive net", http.MethodDelete, endpoint, nil, nil)
}

// PutNetMember adds principalID to the named net, or changes its role there.
func (c *Client) PutNetMember(ctx context.Context, channel, name string, principalID int64, role schema.NetRole) error {
	endpoint := c.resolve("v1", "channels", channel, "nets", name, "members", strconv.FormatInt(principalID, 10))
	return c.roundTrip(ctx, "put net member", http.MethodPut, endpoint, schema.PutNetMemberRequestV1{Role: role}, nil)
}

// RemoveNetMember takes principalID off the named net.
func (c *Client) RemoveNetMember(ctx context.Context, channel, name string, principalID int64) error {
	endpoint := c.resolve("v1", "channels", channel, "nets", name, "members", strconv.FormatInt(principalID, 10))
	return c.roundTrip(ctx, "remove net member", http.MethodDelete, endpoint, nil, nil)
}
