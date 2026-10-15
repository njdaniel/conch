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
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

// Client talks to conchd through its public REST and WebSocket API.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	token      string
	// pingInterval and pingTimeout are the socket watchdog's (watchConn);
	// zero means the defaults. Unexported so tests can shorten them.
	pingInterval, pingTimeout time.Duration
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

// dial opens one of the server's WebSockets, ready to read whatever a server
// may send on it. The library's own limit is 32 KiB a frame, well under the
// largest message conchd accepts, and reading a frame over the limit ends the
// connection: with that default, one long message disconnected every
// subscriber of its channel (issue #231). Every socket this client reads is
// opened here, so none can be left with the default.
func (c *Client) dial(ctx context.Context, endpoint string) (*websocket.Conn, *http.Response, error) {
	conn, resp, err := websocket.Dial(ctx, endpoint, c.dialOptions())
	if err != nil {
		return nil, resp, err
	}
	conn.SetReadLimit(schema.MaxMessageFrameBytes)
	c.watchConn(ctx, conn)
	return conn, resp, nil
}

const (
	// socketPingInterval and socketPingTimeout make the watchdog notice a
	// connection that died without a FIN (Wi-Fi off, packets dropped) in at
	// most their sum, 30 s, inside the 60 s of silence the protocol allows
	// (schema.HeartbeatTimeoutSeconds). The server pings every 25 s too, but
	// a client cannot count on that: a proxy may not forward it.
	socketPingInterval = 20 * time.Second
	socketPingTimeout  = 10 * time.Second
)

// watchConn pings the server on an interval and closes conn when a ping gets
// no pong in time, which ends the caller's read with an error and so starts
// its reconnect. The library answers pongs from its read loop, so every
// caller of dial is reading while this runs. It stops with ctx, or at the
// first failed ping, which is also what a connection closed by the caller
// gives.
func (c *Client) watchConn(ctx context.Context, conn *websocket.Conn) {
	interval, timeout := c.pingInterval, c.pingTimeout
	if interval <= 0 {
		interval = socketPingInterval
	}
	if timeout <= 0 {
		timeout = socketPingTimeout
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}()
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
	conn, resp, err := c.dial(ctx, endpoint.String())
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
	if err := checkSafeServer(server); err != nil {
		return nil, err
	}
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

	conn, resp, err := c.dial(ctx, endpoint.String())
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
	prefixEscaped := strings.TrimRight(base.EscapedPath(), "/")
	prefixUnescaped, err := url.PathUnescape(prefixEscaped)
	if err != nil {
		prefixUnescaped = prefixEscaped
	}
	escapedParts := make([]string, len(parts))
	for i, part := range parts {
		escapedParts[i] = url.PathEscape(part)
	}
	if len(parts) == 0 {
		base.Path = prefixUnescaped
		base.RawPath = prefixEscaped
		return &base
	}
	base.Path = prefixUnescaped + "/" + strings.Join(parts, "/")
	base.RawPath = prefixEscaped + "/" + strings.Join(escapedParts, "/")
	return &base
}

func (c *Client) decodeError(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("server redirected to %s (HTTP %d); set --server to the final URL", safeLine(resp.Header.Get("Location")), resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &UnauthenticatedError{Server: c.Server()}
	}
	var serverError schema.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&serverError); err != nil {
		return fmt.Errorf("cli: server returned %s", safeLine(resp.Status))
	}
	return &ServerError{Status: resp.StatusCode, Code: serverError.Code, Message: serverError.Message}
}

// ServerError is a refusal the server explained in its error body. Its text
// is the same one-line form every client error has always had; the fields let
// a caller tell a permanent refusal (a 4xx) from a passing one.
type ServerError struct {
	// Status is the HTTP status of the refusal.
	Status int
	// Code and Message are the server's own words and are not safe to print
	// unescaped.
	Code    string
	Message string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("cli: server error %s: %s", safeLine(e.Code), safeLine(e.Message))
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

// MessagesPage says which page of messages ListMessagesV2 asks for. The zero
// value is the first page, ascending. After is the largest id held (ascending
// reads); NewestFirst asks for the newest messages first and Before, the
// smallest id held, pages back from there (zero is from the newest). The server
// refuses After with NewestFirst and Before without it.
type MessagesPage struct {
	After, Before int64
	NewestFirst   bool
}

// ListMessagesV2 returns one forward page of the v2 messages in channel that
// the caller may see.
func (c *Client) ListMessagesV2(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	return c.ListMessagesV2Page(ctx, channel, MessagesPage{After: after}, limit)
}

// ListMessagesV2Page returns one page of the v2 messages in channel that the
// caller may see: ascending by id from page.After, or newest first below
// page.Before.
func (c *Client) ListMessagesV2Page(ctx context.Context, channel string, page MessagesPage, limit int) (schema.ListMessagesResponseV2, error) {
	endpoint := c.resolve("v2", "channels", channel, "messages")
	query := endpoint.Query()
	if page.NewestFirst {
		query.Set("order", "desc")
		if page.Before > 0 {
			query.Set("before", strconv.FormatInt(page.Before, 10))
		}
	} else {
		query.Set("after", strconv.FormatInt(page.After, 10))
	}
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
	return c.subscribeV2(ctx, channel, nil, nil, receive)
}

// SubscribeV2After is SubscribeV2 that first sends every visible message with
// an id greater than after, then carries on live, so a reader that holds
// messages up to after misses none (after=0 sends the whole channel). opened,
// if not nil, is called once the server has accepted the socket: a resumed
// stream may have nothing to send for a long time, and this is how the caller
// learns it is back.
func (c *Client) SubscribeV2After(ctx context.Context, channel string, after int64, opened func(), receive func(schema.MessageV2) error) error {
	return c.subscribeV2(ctx, channel, &after, opened, receive)
}

func (c *Client) subscribeV2(ctx context.Context, channel string, after *int64, opened func(), receive func(schema.MessageV2) error) error {
	endpoint := c.resolve("v2", "ws")
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	query := endpoint.Query()
	query.Set("channel", channel)
	if after != nil {
		query.Set("after", strconv.FormatInt(*after, 10))
	}
	endpoint.RawQuery = query.Encode()
	conn, resp, err := c.dial(ctx, endpoint.String())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
		}
		return fmt.Errorf("cli: connect v2 subscription: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if opened != nil {
		opened()
	}
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

// VoicePresence returns one snapshot of who is connected to channel's voice
// and who is transmitting. A document that fails its own Validate is an error:
// presence is read to be believed, so a malformed one is never shown.
func (c *Client) VoicePresence(ctx context.Context, channel string) (schema.VoicePresenceV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "voice")
	var result schema.VoicePresenceV1
	if err := c.roundTrip(ctx, "voice presence", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.VoicePresenceV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.VoicePresenceV1{}, fmt.Errorf("cli: invalid voice presence: %w", err)
	}
	return result, nil
}

// SubscribeVoicePresence follows channel's voice presence socket and calls
// receive with the whole-state document the server sends on connect and on
// every change. A document that fails Validate ends the subscription with an
// error. A server shutdown is reported through websocket.StatusGoingAway, and
// removal from the channel (or a disabled or signed-out credential) through
// websocket.StatusPolicyViolation.
func (c *Client) SubscribeVoicePresence(ctx context.Context, channel string, receive func(schema.VoicePresenceV1) error) error {
	endpoint := c.resolve("v1", "voice", "ws")
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	query := endpoint.Query()
	query.Set("channel", channel)
	endpoint.RawQuery = query.Encode()
	conn, resp, err := c.dial(ctx, endpoint.String())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
		}
		return fmt.Errorf("cli: connect voice presence: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		var doc schema.VoicePresenceV1
		if err := wsjson.Read(ctx, conn, &doc); err != nil {
			return fmt.Errorf("cli: read voice presence: %w", err)
		}
		if err := doc.Validate(); err != nil {
			return fmt.Errorf("cli: invalid voice presence: %w", err)
		}
		if err := receive(doc); err != nil {
			return fmt.Errorf("cli: receive voice presence: %w", err)
		}
	}
}

// The methods below are the operator's setup calls behind `conch admin`. Each
// is one request to a public route; the caller's ordinary credential decides
// whether the server allows it.

// ListReadState returns the caller's read state in every channel they are in:
// how far they have read and how many messages they may see after that. It is
// for people only; an agent gets human_required and a server with
// authentication off gets read_state_require_auth.
func (c *Client) ListReadState(ctx context.Context) (schema.ListReadStateResponseV1, error) {
	var result schema.ListReadStateResponseV1
	if err := c.roundTrip(ctx, "list read state", http.MethodGet, c.resolve("v1", "read-state"), nil, &result); err != nil {
		return schema.ListReadStateResponseV1{}, err
	}
	return result, nil
}

// MarkRead moves the caller's read mark in channel forward to lastReadID and
// returns the stored state. The server never moves it back, and takes an id
// above the newest message the caller may see as that message.
func (c *Client) MarkRead(ctx context.Context, channel string, lastReadID int64) (schema.ReadStateV1, error) {
	var result schema.ReadStateV1
	endpoint := c.resolve("v1", "channels", channel, "read-state")
	if err := c.roundTrip(ctx, "mark read", http.MethodPut, endpoint, schema.MarkReadRequestV1{LastReadID: lastReadID}, &result); err != nil {
		return schema.ReadStateV1{}, err
	}
	return result, nil
}

// CreateChannel creates a channel. The caller becomes its only member.
func (c *Client) CreateChannel(ctx context.Context, name string) (schema.ChannelV0, error) {
	var result schema.CreateChannelResponse
	if err := c.roundTrip(ctx, "create channel", http.MethodPost, c.resolve("v0", "channels"), schema.CreateChannelRequest{Name: name}, &result); err != nil {
		return schema.ChannelV0{}, err
	}
	return result.Channel, nil
}

// CreatePrincipal creates a principal of the given kind. It has no role: the
// route takes a kind and a name only.
func (c *Client) CreatePrincipal(ctx context.Context, kind schema.PrincipalKind, name string) (schema.PrincipalV0, error) {
	var result schema.CreatePrincipalResponse
	if err := c.roundTrip(ctx, "create principal", http.MethodPost, c.resolve("v0", "principals"), schema.CreatePrincipalRequest{Kind: kind, Name: name}, &result); err != nil {
		return schema.PrincipalV0{}, err
	}
	return result.Principal, nil
}

// DisablePrincipal switches a principal off and revokes its credentials.
func (c *Client) DisablePrincipal(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(id, 10), "disable")
	return c.roundTrip(ctx, "disable principal", http.MethodPost, endpoint, nil, nil)
}

// EnablePrincipal switches a principal back on. It revives no credential.
func (c *Client) EnablePrincipal(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(id, 10), "enable")
	return c.roundTrip(ctx, "enable principal", http.MethodPost, endpoint, nil, nil)
}

// CreateCredential issues a credential for a principal. The response carries
// the plaintext token, which the server shows only once.
func (c *Client) CreateCredential(ctx context.Context, principalID int64, request schema.CreateCredentialRequestV1) (schema.CreateCredentialResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "credentials")
	var result schema.CreateCredentialResponseV1
	if err := c.roundTrip(ctx, "create credential", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.CreateCredentialResponseV1{}, err
	}
	return result, nil
}

// ListCredentials returns a principal's credentials, newest first.
func (c *Client) ListCredentials(ctx context.Context, principalID int64) (schema.ListCredentialsResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "credentials")
	var result schema.ListCredentialsResponseV1
	if err := c.roundTrip(ctx, "list credentials", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListCredentialsResponseV1{}, err
	}
	return result, nil
}

// RevokeCredential revokes one credential.
func (c *Client) RevokeCredential(ctx context.Context, credentialID int64) error {
	endpoint := c.resolve("v1", "credentials", strconv.FormatInt(credentialID, 10))
	return c.roundTrip(ctx, "revoke credential", http.MethodDelete, endpoint, nil, nil)
}

// PutChannelMember adds principalID to channel. It is idempotent.
func (c *Client) PutChannelMember(ctx context.Context, channel string, principalID int64) error {
	endpoint := c.resolve("v1", "channels", channel, "members", strconv.FormatInt(principalID, 10))
	return c.roundTrip(ctx, "add channel member", http.MethodPut, endpoint, nil, nil)
}

// RemoveChannelMember takes principalID off channel. It is idempotent.
func (c *Client) RemoveChannelMember(ctx context.Context, channel string, principalID int64) error {
	endpoint := c.resolve("v1", "channels", channel, "members", strconv.FormatInt(principalID, 10))
	return c.roundTrip(ctx, "remove channel member", http.MethodDelete, endpoint, nil, nil)
}

// PutManifest creates or replaces an agent's manifest.
func (c *Client) PutManifest(ctx context.Context, principalID int64, request schema.PutAgentManifestRequestV1) (schema.AgentManifestV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "manifest")
	var result schema.PutAgentManifestResponseV1
	if err := c.roundTrip(ctx, "put manifest", http.MethodPut, endpoint, request, &result); err != nil {
		return schema.AgentManifestV1{}, err
	}
	return result.Manifest, nil
}

// GetManifest returns an agent's manifest.
func (c *Client) GetManifest(ctx context.Context, principalID int64) (schema.AgentManifestV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "manifest")
	var result schema.GetAgentManifestResponseV1
	if err := c.roundTrip(ctx, "get manifest", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.AgentManifestV1{}, err
	}
	return result.Manifest, nil
}

// CreateSession opens a session for the caller's credential. A session that
// fails its own Validate is an error: it is shown to be believed.
func (c *Client) CreateSession(ctx context.Context, request schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	var result schema.SessionV1
	if err := c.roundTrip(ctx, "create session", http.MethodPost, c.resolve("v1", "sessions"), request, &result); err != nil {
		return schema.SessionV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.SessionV1{}, fmt.Errorf("cli: invalid session: %w", err)
	}
	return result, nil
}

// EndSession ends the caller's own session id.
func (c *Client) EndSession(ctx context.Context, id int64) error {
	endpoint := c.resolve("v1", "sessions", strconv.FormatInt(id, 10))
	return c.roundTrip(ctx, "end session", http.MethodDelete, endpoint, nil, nil)
}

// RunsPage says which page of runs ListRuns asks for. The zero value is the
// first page, ascending. After is the largest id held (ascending reads);
// NewestFirst asks for the newest runs first and Before, the smallest id held,
// pages back from there (zero is from the newest). The server refuses After
// with NewestFirst and Before without it.
type RunsPage struct {
	After, Before int64
	NewestFirst   bool
}

// ListRuns returns one page of the runs of channel that the caller may see:
// ascending by id from page.After, or newest first below page.Before.
// Optionally only those of session (0 for all).
func (c *Client) ListRuns(ctx context.Context, channel string, session int64, page RunsPage, limit int) (schema.ListRunsResponseV1, error) {
	endpoint := c.resolve("v1", "runs")
	query := endpoint.Query()
	query.Set("channel", channel)
	if session > 0 {
		query.Set("session", strconv.FormatInt(session, 10))
	}
	if page.NewestFirst {
		query.Set("order", "desc")
		if page.Before > 0 {
			query.Set("before", strconv.FormatInt(page.Before, 10))
		}
	} else {
		query.Set("after", strconv.FormatInt(page.After, 10))
	}
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	var result schema.ListRunsResponseV1
	if err := c.roundTrip(ctx, "list runs", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListRunsResponseV1{}, err
	}
	return result, nil
}

// EventHandler is what SubscribeEvents calls for each document of the events
// socket. A nil function means the caller does not want that document: its
// frames are skipped without being decoded. Each is called with a document that
// passed its own Validate.
type EventHandler struct {
	Roster       func(schema.SessionRosterV1) error
	Run          func(schema.RunV1) error
	Activity     func(schema.AgentActivityV1) error
	Intervention func(schema.InterventionV1) error
}

// SubscribeEvents follows channel's events socket and calls the handler with
// each document it sends: the session roster (the whole state on connect and
// on every change), and the runs, activity entries and interventions the
// caller may see, each whole and possibly repeated. With session set (the
// caller's own, not ended) the server keeps that session online while the
// socket is open. A frame of any other schema is skipped, so a newer server
// may send more. A document that fails Validate ends the subscription with an
// error. There is no replay: a caller that reconnects takes the next roster as
// the state and reads runs over REST. A server shutdown is reported through
// websocket.StatusGoingAway, and loss of membership or of the credential
// through websocket.StatusPolicyViolation.
func (c *Client) SubscribeEvents(ctx context.Context, channel string, session int64, handler EventHandler) error {
	endpoint := c.resolve("v1", "events", "ws")
	if endpoint.Scheme == "http" {
		endpoint.Scheme = "ws"
	} else {
		endpoint.Scheme = "wss"
	}
	query := endpoint.Query()
	query.Set("channel", channel)
	if session > 0 {
		query.Set("session", strconv.FormatInt(session, 10))
	}
	endpoint.RawQuery = query.Encode()
	conn, resp, err := c.dial(ctx, endpoint.String())
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			return c.decodeError(resp)
		}
		return fmt.Errorf("cli: connect events: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		// Every frame is one pkg/schema document with a schema field; the
		// field says which type the frame decodes into.
		var frame json.RawMessage
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return fmt.Errorf("cli: read events: %w", err)
		}
		if err := dispatchEvent(frame, handler); err != nil {
			return err
		}
	}
}

// dispatchEvent decodes one frame into the type its schema names and hands it
// to the handler that wants it.
func dispatchEvent(frame json.RawMessage, handler EventHandler) error {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return fmt.Errorf("cli: read events: %w", err)
	}
	switch head.Schema {
	case schema.SessionRosterSchemaV1:
		if handler.Roster == nil {
			return nil
		}
		var roster schema.SessionRosterV1
		if err := decodeEvent(frame, &roster, "session roster"); err != nil {
			return err
		}
		return receiveEvent(handler.Roster(roster))
	case schema.RunSchemaV1:
		if handler.Run == nil {
			return nil
		}
		var run schema.RunV1
		if err := decodeEvent(frame, &run, "run"); err != nil {
			return err
		}
		return receiveEvent(handler.Run(run))
	case schema.AgentActivitySchemaV1:
		if handler.Activity == nil {
			return nil
		}
		var entry schema.AgentActivityV1
		if err := decodeEvent(frame, &entry, "activity"); err != nil {
			return err
		}
		return receiveEvent(handler.Activity(entry))
	case schema.InterventionSchemaV1:
		if handler.Intervention == nil {
			return nil
		}
		var iv schema.InterventionV1
		if err := decodeEvent(frame, &iv, "intervention"); err != nil {
			return err
		}
		return receiveEvent(handler.Intervention(iv))
	}
	return nil
}

// validator is a pkg/schema document that can check itself.
type validator interface{ Validate() error }

// decodeEvent decodes frame into doc, a pointer to a document type, and
// validates it.
func decodeEvent(frame json.RawMessage, doc validator, what string) error {
	if err := json.Unmarshal(frame, doc); err != nil {
		return fmt.Errorf("cli: read events: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return fmt.Errorf("cli: invalid %s: %w", what, err)
	}
	return nil
}

func receiveEvent(err error) error {
	if err != nil {
		return fmt.Errorf("cli: receive events: %w", err)
	}
	return nil
}

// GetRun returns run id, if the caller may see it; otherwise the server's
// run_not_found.
func (c *Client) GetRun(ctx context.Context, id int64) (schema.RunV1, error) {
	endpoint := c.resolve("v1", "runs", strconv.FormatInt(id, 10))
	var result schema.RunV1
	if err := c.roundTrip(ctx, "get run", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.RunV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.RunV1{}, fmt.Errorf("cli: invalid run: %w", err)
	}
	return result, nil
}

// ListActivity returns one forward page of run's activity, oldest first,
// starting after the entry id after (0 for the first page).
func (c *Client) ListActivity(ctx context.Context, run, after int64, limit int) (schema.ListActivityResponseV1, error) {
	endpoint := c.resolve("v1", "runs", strconv.FormatInt(run, 10), "activity")
	pageQuery(endpoint, after, limit)
	var result schema.ListActivityResponseV1
	if err := c.roundTrip(ctx, "list activity", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListActivityResponseV1{}, err
	}
	for _, entry := range result.Activity {
		if err := entry.Validate(); err != nil {
			return schema.ListActivityResponseV1{}, fmt.Errorf("cli: invalid activity: %w", err)
		}
	}
	return result, nil
}

// CreateIntervention asks run to take a message or to stop. The answer is the
// intervention as stored, in state requested.
func (c *Client) CreateIntervention(ctx context.Context, run int64, request schema.CreateInterventionRequestV1) (schema.InterventionV1, error) {
	endpoint := c.resolve("v1", "runs", strconv.FormatInt(run, 10), "interventions")
	var result schema.InterventionV1
	if err := c.roundTrip(ctx, "create intervention", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.InterventionV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.InterventionV1{}, fmt.Errorf("cli: invalid intervention: %w", err)
	}
	return result, nil
}

// ListInterventions returns one forward page of run's interventions, oldest
// first, starting after the intervention id after. The response has no cursor:
// a page as long as limit may have a successor.
func (c *Client) ListInterventions(ctx context.Context, run, after int64, limit int) (schema.ListInterventionsResponseV1, error) {
	endpoint := c.resolve("v1", "runs", strconv.FormatInt(run, 10), "interventions")
	pageQuery(endpoint, after, limit)
	var result schema.ListInterventionsResponseV1
	if err := c.roundTrip(ctx, "list interventions", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListInterventionsResponseV1{}, err
	}
	for _, iv := range result.Interventions {
		if err := iv.Validate(); err != nil {
			return schema.ListInterventionsResponseV1{}, fmt.Errorf("cli: invalid intervention: %w", err)
		}
	}
	return result, nil
}

// pageQuery sets the after and limit of a forward page on endpoint.
func pageQuery(endpoint *url.URL, after int64, limit int) {
	query := endpoint.Query()
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
}
