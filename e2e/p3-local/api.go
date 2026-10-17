package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The scenarios observe the server through its public routes only (the table
// in internal/server/routes.go). A participant's action is done by a client
// (a TUI, the conch CLI, an adapter); the calls in this file read what the
// server shows, and make the requests no client would make, to see them
// refused.

// api is a REST client acting as one principal: every request carries its
// bearer credential, or none when token is empty.
type api struct{ base, token string }

// httpClient bounds every request, so a server that stops answering fails the
// run instead of hanging it.
var httpClient = &http.Client{Timeout: 30 * time.Second, Transport: newLoggingTransport()}

// response is what one request came back with.
type response struct {
	status int
	body   []byte
}

// send sends one request. A []byte body is sent as it is; any other non-nil
// body is sent as JSON.
func (a api) send(method, path string, body any) (response, error) {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		reader, contentType = bytes.NewReader(encoded), "application/json"
	}
	req, err := http.NewRequestWithContext(runCtx, method, a.base+path, reader) // #nosec G107 -- this run's own loopback conchd
	if err != nil {
		return response{}, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, body: data}, nil
}

// call sends a request that must succeed (2xx) and decodes the answer into out
// when out is non-nil. A failure quotes the status and the error code only,
// never the body: a body can hold message text.
func (a api) call(method, path string, body, out any) error {
	resp, err := a.send(method, path, body)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		return fmt.Errorf("%s %s: status %d (%s)", method, path, resp.status, errorCode(resp.body))
	}
	if out == nil {
		return nil
	}
	if err := decodeStrict(resp.body, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

// refused sends a request that must be refused with exactly this status and
// this error code, in the schema's error shape and nothing else.
func (a api) refused(what, method, path string, body any, status int, code string) error {
	resp, err := a.send(method, path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.status != status {
		return fmt.Errorf("%s: %s %s answered %d (%s), want %d %s", what, method, path, resp.status, errorCode(resp.body), status, code)
	}
	var e schema.Error
	if err := decodeStrict(resp.body, &e); err != nil {
		return fmt.Errorf("%s: the refusal is not a schema.Error: %w", what, err)
	}
	if e.Code != code || e.Message == "" {
		return fmt.Errorf("%s: error code %q, want %q", what, e.Code, code)
	}
	return nil
}

// decodeStrict decodes an answer and refuses a field the schema type does not
// have: the wire shapes are closed, and a lenient decoder would never notice a
// server that began sending more. The error names the field, never the
// content.
func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the answer does not fit %T: %w", out, err)
	}
	return nil
}

// errorCode is the schema.Error code of an answer body, or a marker.
func errorCode(body []byte) string {
	var e schema.Error
	if json.Unmarshal(body, &e) != nil || e.Code == "" {
		return "no error code"
	}
	return e.Code
}

// channelNotFound says whether err is the server answering that the channel
// does not exist, as it does to someone who may not know that it does.
func channelNotFound(err error) bool {
	var se *cli.ServerError
	if errors.As(err, &se) {
		return se.Status == http.StatusNotFound && se.Code == "channel_not_found"
	}
	return false
}

// describe is err in words that never quote a server's message.
func describe(err error) string {
	var se *cli.ServerError
	switch {
	case err == nil:
		return "no error"
	case errors.As(err, &se):
		return fmt.Sprintf("status %d (%s)", se.Status, se.Code)
	case errors.Is(err, cli.ErrUnauthenticated):
		return "status 401"
	}
	return err.Error()
}

// ------------------------------------------------------------ message feed

// messageFeed is a /v2/ws socket held open by the scenario: every message the
// server sent on it, in the order it sent them.
type messageFeed struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	msgs []schema.MessageV2
	err  error
}

// followMessages opens the live message socket of channel as part, from after
// (replaying what the reader may see above it, as a reconnecting client
// does), and returns once the server has accepted the socket. A refusal comes
// back as the error.
func (s *suite) followMessages(part, channel string, after int64) (*messageFeed, error) {
	client, err := s.r.client(s.token(part))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(runCtx)
	f := &messageFeed{cancel: cancel, done: make(chan struct{})}
	opened := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(f.done)
		err := client.SubscribeV2After(ctx, channel, after, func() { once.Do(func() { close(opened) }) }, func(m schema.MessageV2) error {
			f.mu.Lock()
			f.msgs = append(f.msgs, m)
			f.mu.Unlock()
			return nil
		})
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
		once.Do(func() { close(opened) })
	}()
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		cancel()
		return nil, fmt.Errorf("the message socket of %s on %s was not accepted within 15s", part, channel)
	case <-runCtx.Done():
		cancel()
		return nil, runCtx.Err()
	}
	f.mu.Lock()
	err = f.err
	f.mu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	s.cleanup(f.close)
	return f, nil
}

func (f *messageFeed) close() {
	f.cancel()
	<-f.done
}

// ids is the id of each message received so far, in order.
func (f *messageFeed) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.msgs))
	for i, m := range f.msgs {
		out[i] = m.ID
	}
	return out
}

// has reports whether a message with this id was received.
func (f *messageFeed) has(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.ID == id {
			return true
		}
	}
	return false
}

// closed is the error that ended the socket, nil while it is open.
func (f *messageFeed) closed() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// ------------------------------------------------------------- events feed

// eventFeed is a /v1/events/ws socket held open by the scenario.
type eventFeed struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	rosters []schema.SessionRosterV1
	runs    []schema.RunV1
	acts    []schema.AgentActivityV1
	ivs     []schema.InterventionV1
	err     error
}

// followEvents opens the events socket of channel as part, with no session of
// its own, and returns once the first roster is in, which is how the server
// says the socket is open. A refusal comes back as the error.
func (s *suite) followEvents(part, channel string) (*eventFeed, error) {
	client, err := s.r.client(s.token(part))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(runCtx)
	f := &eventFeed{cancel: cancel, done: make(chan struct{})}
	first := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(f.done)
		err := client.SubscribeEvents(ctx, channel, 0, cli.EventHandler{
			Roster: func(r schema.SessionRosterV1) error {
				f.mu.Lock()
				f.rosters = append(f.rosters, r)
				f.mu.Unlock()
				once.Do(func() { close(first) })
				return nil
			},
			Run: func(r schema.RunV1) error {
				f.mu.Lock()
				f.runs = append(f.runs, r)
				f.mu.Unlock()
				return nil
			},
			Activity: func(a schema.AgentActivityV1) error {
				f.mu.Lock()
				f.acts = append(f.acts, a)
				f.mu.Unlock()
				return nil
			},
			Intervention: func(iv schema.InterventionV1) error {
				f.mu.Lock()
				f.ivs = append(f.ivs, iv)
				f.mu.Unlock()
				return nil
			},
		})
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
		once.Do(func() { close(first) })
	}()
	select {
	case <-first:
	case <-time.After(15 * time.Second):
		cancel()
		return nil, fmt.Errorf("the events socket of %s on %s sent no roster within 15s", part, channel)
	case <-runCtx.Done():
		cancel()
		return nil, runCtx.Err()
	}
	f.mu.Lock()
	err = f.err
	f.mu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	s.cleanup(f.close)
	return f, nil
}

// closed is the error that ended the socket, nil while it is open.
func (f *eventFeed) closed() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *eventFeed) close() {
	f.cancel()
	<-f.done
}

// runsSeen is a copy of every run frame so far.
func (f *eventFeed) runsSeen() []schema.RunV1 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]schema.RunV1(nil), f.runs...)
}

// activitySeen is a copy of every activity frame so far.
func (f *eventFeed) activitySeen() []schema.AgentActivityV1 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]schema.AgentActivityV1(nil), f.acts...)
}

// anyRoster reports whether some roster frame satisfies ok.
func (f *eventFeed) anyRoster(ok func(schema.SessionRosterV1) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rosters {
		if ok(r) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------- ticket sockets

// ticketSocket is a WebSocket opened the way a browser opens one: with a
// single-use ticket in the URL, because a browser cannot set a header on a
// WebSocket.
type ticketSocket struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	frames int
	err    error
}

// wsURL is the address of a socket route, with the query.
func (s *suite) wsURL(path string, query url.Values) string {
	u := url.URL{Scheme: "ws", Host: s.r.addr, Path: path, RawQuery: query.Encode()}
	return u.String()
}

// newTicket asks for a ticket as part, the way a browser does over REST.
func (s *suite) newTicket(part string) (string, error) {
	var t schema.WSTicketV1
	if err := s.as(part).call(http.MethodPost, "/v1/ws-tickets", nil, &t); err != nil {
		return "", err
	}
	return t.Ticket, nil
}

// dialTicket opens path with ticket. The answer's status comes back with an
// error when the upgrade was refused.
func (s *suite) dialTicket(path string, query url.Values, ticket string) (*websocket.Conn, int, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("ticket", ticket)
	ctx, cancel := context.WithTimeout(runCtx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, s.wsURL(path, q), &websocket.DialOptions{HTTPClient: httpClient})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		return nil, status, err
	}
	conn.SetReadLimit(schema.MaxMessageFrameBytes)
	return conn, status, nil
}

// holdTicketSocket opens path as part with a fresh ticket and reads it in the
// background, counting frames, until it closes.
func (s *suite) holdTicketSocket(part, path string, query url.Values) (*ticketSocket, error) {
	ticket, err := s.newTicket(part)
	if err != nil {
		return nil, err
	}
	conn, status, err := s.dialTicket(path, query, ticket)
	if err != nil {
		return nil, fmt.Errorf("open %s as %s with a ticket: status %d: %w", path, part, status, err)
	}
	return s.holdConn(conn), nil
}

// holdConn reads conn in the background, counting frames, until it closes.
func (s *suite) holdConn(conn *websocket.Conn) *ticketSocket {
	ctx, cancel := context.WithCancel(runCtx)
	t := &ticketSocket{conn: conn, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		for {
			_, _, err := conn.Read(ctx)
			t.mu.Lock()
			if err != nil {
				t.err = err
				t.mu.Unlock()
				return
			}
			t.frames++
			t.mu.Unlock()
		}
	}()
	s.cleanup(t.close)
	return t
}

func (t *ticketSocket) close() {
	t.cancel()
	_ = t.conn.CloseNow()
	<-t.done
}

// framesSeen is how many frames the socket has received.
func (t *ticketSocket) framesSeen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames
}

// closeStatus is the close code the server ended the socket with, and whether
// it has ended at all.
func (t *ticketSocket) closeStatus() (websocket.StatusCode, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		return 0, false
	}
	return websocket.CloseStatus(t.err), true
}

// closeCode is the close code in err, or -1 when err is not a close.
func closeCode(err error) websocket.StatusCode {
	if err == nil {
		return -1
	}
	return websocket.CloseStatus(err)
}
