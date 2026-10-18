package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// Connection logs (issue #446). Every socket handler opens its socket through
// acceptSocket, which writes one line when the socket opens and one when it
// closes: the route pattern, the principal and credential ids, the request id
// (from the context, see requestid.go), and on close how long it lasted, why it
// closed and how many frames it sent. Nothing else: the route is the pattern,
// so no channel name; no ticket, token or message text is in scope here.
//
// Why a socket closed is said where it is closed, with closeWS (or, for the
// keepalive, noteClose): the first reason said wins, because what follows a
// close (the reader ending, the handler's loop seeing its context end) would
// otherwise say "client" for every socket.
//
// A client that reconnects in a loop must not fill the log. Each credential
// gets connLogFull sockets logged in full per connLogWindow; past that its
// sockets are counted, and the counts are written as one summary line by the
// first open or close after the window ends.

const (
	// connLogFull is how many sockets of one credential are logged in full
	// in a window.
	connLogFull = 5
	// connLogWindow is the window, and so how often at most a storm is
	// summarised.
	connLogWindow = time.Minute
)

// socketTrace is one open socket's record. It lives in the socket's context.
type socketTrace struct {
	route       string
	principalID int64
	credID      int64
	opened      time.Time // monotonic, for the duration
	full        bool      // logged in full, open and close
	frames      atomic.Int64
	mu          sync.Mutex
	reason      schema.SocketCloseReason
}

type socketTraceKey struct{}

func socketTraceFrom(ctx context.Context) *socketTrace {
	t, _ := ctx.Value(socketTraceKey{}).(*socketTrace)
	return t
}

// noteClose records why the socket of ctx is closing, unless a reason was
// already recorded.
func noteClose(ctx context.Context, reason schema.SocketCloseReason) {
	t := socketTraceFrom(ctx)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reason == "" {
		t.reason = reason
	}
}

// closeWS closes conn with code and text, after recording reason for the
// connection log. Every close of a socket goes through it.
func closeWS(ctx context.Context, conn *websocket.Conn, reason schema.SocketCloseReason, code websocket.StatusCode, text string) {
	// A write or a read that failed because the reader had already seen the
	// peer go is the client leaving, not a fault.
	if reason == schema.SocketCloseError && ctx.Err() != nil {
		reason = schema.SocketCloseClient
	}
	noteClose(ctx, reason)
	_ = conn.Close(code, text)
}

// countFrame counts a frame written to the socket of ctx when err says it was
// written, and returns err.
func countFrame(ctx context.Context, err error) error {
	if err == nil {
		if t := socketTraceFrom(ctx); t != nil {
			t.frames.Add(1)
		}
	}
	return err
}

// acceptSocket is how every socket handler opens its socket, after its last
// refusal: it counts the socket against its credential (admitSocket), upgrades
// the connection and logs the open. It returns the connection, the context the
// socket's work must use (it carries the socket's trace), and done, which the
// handler defers: it gives the count back and logs the close. When ok is
// false the answer has been written and the handler returns.
func (s *Server) acceptSocket(w http.ResponseWriter, r *http.Request) (conn *websocket.Conn, ctx context.Context, done func(), ok bool) {
	release, ok := s.admitSocket(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	conn, err := websocket.Accept(w, r, s.acceptOptions())
	if err != nil {
		// Accept has already written the HTTP error response.
		release()
		slog.DebugContext(r.Context(), "ws: accept failed", "route", r.Pattern, "error", err)
		return nil, nil, nil, false
	}
	t := &socketTrace{route: r.Pattern, opened: time.Now()}
	if caller, has := callerFrom(r.Context()); has {
		t.principalID = caller.ID
	}
	t.credID, _ = credentialIDFrom(r.Context())
	ctx = context.WithValue(r.Context(), socketTraceKey{}, t)
	s.socketOpened(ctx, t)
	return conn, ctx, func() {
		// Logged before the count is given back, so that once the count
		// shows the socket gone its line is written.
		s.socketClosed(ctx, t)
		release()
	}, true
}

func (s *Server) socketOpened(ctx context.Context, t *socketTrace) {
	s.metrics.socketOpened(t.route)
	full, summary := s.connLogs.open(t.credID, t.principalID, s.now())
	t.full = full
	summary.log(ctx)
	if full {
		slog.InfoContext(ctx, "ws: socket opened", "route", t.route, "principal", t.principalID, "credential", t.credID)
	}
}

func (s *Server) socketClosed(ctx context.Context, t *socketTrace) {
	t.mu.Lock()
	reason := t.reason
	t.mu.Unlock()
	if reason == "" {
		// Every close goes through closeWS; a path that does not is a bug
		// here, and "error" is the honest word for it.
		reason = schema.SocketCloseError
	}
	s.metrics.socketClosed(t.route, reason)
	lasted := time.Since(t.opened)
	frames := t.frames.Load()
	id, _ := requestIDFrom(ctx)
	s.diag.recordClose(t.principalID, schema.ServerSocketCloseV1{
		At: schema.NewTimestamp(s.now()), RequestID: id, Route: t.route, Reason: reason,
		DurationSeconds: durationSeconds(lasted), Frames: frames,
	})
	summary := s.connLogs.close(t.credID, t.principalID, reason, t.full, s.now())
	summary.log(ctx)
	if t.full {
		slog.InfoContext(ctx, "ws: socket closed", "route", t.route, "principal", t.principalID, "credential", t.credID,
			"duration", lasted.Round(time.Millisecond), "reason", string(reason), "frames", frames)
	}
}

// connLogLimiter decides which sockets are logged in full, per credential, and
// counts the rest. The zero value is ready to use.
type connLogLimiter struct {
	mu      sync.Mutex
	entries map[int64]*connLogEntry
	pruned  time.Time
}

// connLogEntry is one credential's window.
type connLogEntry struct {
	start       time.Time
	principalID int64
	// opened is every socket opened in the window; quietOpened and
	// quietClosed are the ones not logged in full since the last summary.
	opened      int
	quietOpened int
	quietClosed map[schema.SocketCloseReason]int
}

// connSummary is a storm's summary line; nil when there is none to write.
type connSummary struct {
	credID, principalID int64
	window              time.Duration
	opened              int
	closed              map[schema.SocketCloseReason]int
}

func (c *connSummary) log(ctx context.Context) {
	if c == nil {
		return
	}
	total := 0
	reasons := make([]string, 0, len(c.closed))
	for r, n := range c.closed {
		total += n
		reasons = append(reasons, fmt.Sprintf("%s=%d", r, n))
	}
	slices.Sort(reasons)
	// Written with the context of whichever socket ended the window; its
	// request_id is that socket's, and says so.
	slog.InfoContext(ctx, "ws: sockets summarised", "principal", c.principalID, "credential", c.credID,
		"window", c.window.Round(time.Second), "opened", c.opened, "closed", total, "reasons", strings.Join(reasons, " "))
}

// roll starts a new window for e when its window has passed, and returns the
// summary of what went unlogged in the old one, if anything did.
func (e *connLogEntry) roll(credID int64, now time.Time) *connSummary {
	if now.Sub(e.start) < connLogWindow {
		return nil
	}
	var summary *connSummary
	if e.quietOpened > 0 || len(e.quietClosed) > 0 {
		summary = &connSummary{credID: credID, principalID: e.principalID, window: now.Sub(e.start), opened: e.quietOpened, closed: e.quietClosed}
	}
	e.start, e.opened, e.quietOpened, e.quietClosed = now, 0, 0, nil
	return summary
}

func (l *connLogLimiter) entry(credID, principalID int64, now time.Time) *connLogEntry {
	if l.entries == nil {
		l.entries = make(map[int64]*connLogEntry)
	}
	// Idle entries go at most once a window, so the map holds only the
	// credentials that used a socket recently.
	if now.Sub(l.pruned) >= connLogWindow {
		l.pruned = now
		for k, e := range l.entries {
			if now.Sub(e.start) >= connLogWindow && e.quietOpened == 0 && len(e.quietClosed) == 0 {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[credID]
	if e == nil {
		e = &connLogEntry{start: now, principalID: principalID}
		l.entries[credID] = e
	}
	return e
}

// open counts a socket opened by credID and reports whether it is logged in
// full, with the summary of an earlier window to write first, if any.
func (l *connLogLimiter) open(credID, principalID int64, now time.Time) (bool, *connSummary) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(credID, principalID, now)
	summary := e.roll(credID, now)
	e.opened++
	if e.opened <= connLogFull {
		return true, summary
	}
	e.quietOpened++
	return false, summary
}

// close counts the close of a socket that was not logged in full, and
// returns the summary of an earlier window to write, if any.
func (l *connLogLimiter) close(credID, principalID int64, reason schema.SocketCloseReason, full bool, now time.Time) *connSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(credID, principalID, now)
	summary := e.roll(credID, now)
	if !full {
		if e.quietClosed == nil {
			e.quietClosed = make(map[schema.SocketCloseReason]int)
		}
		e.quietClosed[reason]++
	}
	return summary
}
