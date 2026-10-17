package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Request ids (issue #446, private alpha A13). Every request gets a random id
// that is returned in X-Request-Id and carried in its context, so every log
// line written with that context says request_id=<id>, and a tester's report
// can be matched to the server's log. An incoming X-Request-Id is never read:
// a client could otherwise choose the id of another person's log lines, or put
// text of its own in the log.
//
// The ids of each principal's recent requests, and how their recent sockets
// closed, are also kept in memory (diagRing), for the server's part of a
// problem report. That memory holds the route pattern, never the path, so it
// names no channel.

type requestIDKey struct{}

// withRequestID returns ctx carrying the request's id.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFrom returns the id of the request ctx belongs to, if any.
func requestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

// newRequestID is 8 random bytes in hex: 16 characters, unguessable and
// unique enough that two requests of one report never share one.
func newRequestID() string {
	var b [schema.RequestIDLength / 2]byte
	// crypto/rand.Read never fails (it panics instead), per its documentation.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestTrace is what the route wrapper learns about a request for the
// middleware that made the id: who made it and which route served it. Both
// run on the request's goroutine, one after the other, so it needs no lock.
type requestTrace struct {
	principalID int64
	route       string
}

type requestTraceKey struct{}

func requestTraceFrom(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(requestTraceKey{}).(*requestTrace)
	return t
}

// statusWriter remembers the status a handler answered with. Unwrap lets
// http.ResponseController and the WebSocket upgrade reach the writer it wraps
// (to flush, or to hijack).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestIDMiddleware is the outermost handler: outside authentication, so a
// refused request has an id too. It sets the header before anything is
// written, and after the request records it in the caller's ring when the
// route wrapper found a caller.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		// Ignored, not trusted: dropped so nothing further in can read it.
		r.Header.Del(schema.RequestIDHeader)
		w.Header().Set(schema.RequestIDHeader, id)
		trace := &requestTrace{}
		ctx := context.WithValue(withRequestID(r.Context(), id), requestTraceKey{}, trace)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		if trace.principalID == 0 {
			return
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.diag.recordRequest(trace.principalID, schema.ServerRequestTraceV1{
			At: schema.NewTimestamp(s.now()), RequestID: id, Route: trace.route, Status: status,
		})
	})
}

// traceRoute wraps each route's handler: inside the mux, past authentication,
// it is where the caller and the matched pattern are known.
func traceRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := requestTraceFrom(r.Context()); t != nil {
			t.route = r.Pattern
			if caller, ok := callerFrom(r.Context()); ok {
				t.principalID = caller.ID
			}
		}
		next.ServeHTTP(w, r)
	})
}

// LogHandler wraps h so that every record logged with a request's context
// carries that request's id as request_id. conchd installs it as the default
// logger's handler.
// A handler that already is one is returned as it is, so the id is never
// added twice.
func LogHandler(h slog.Handler) slog.Handler {
	if _, ok := h.(requestIDHandler); ok {
		return h
	}
	return requestIDHandler{h}
}

type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := requestIDFrom(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// diagRing is each principal's last schema.MaxServerTraceEntries requests and
// socket closes, in memory only: a restart forgets them. It holds one small
// entry per principal that has made a request, and principals are made only
// by operators and invites, so it is bounded by the server's own people. The
// zero value is ready to use.
type diagRing struct {
	mu   sync.Mutex
	byID map[int64]*principalDiag
}

type principalDiag struct {
	requests ring[schema.ServerRequestTraceV1]
	closes   ring[schema.ServerSocketCloseV1]
}

// ring is a fixed ring of the last MaxServerTraceEntries values.
type ring[T any] struct {
	buf  [schema.MaxServerTraceEntries]T
	next int // where the next value goes
	n    int // how many are held
}

func (r *ring[T]) add(v T) {
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	r.n = min(r.n+1, len(r.buf))
}

// values is what the ring holds, oldest first; never nil.
func (r *ring[T]) values() []T {
	out := make([]T, 0, r.n)
	start := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := range r.n {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

func (d *diagRing) entry(principalID int64) *principalDiag {
	if d.byID == nil {
		d.byID = make(map[int64]*principalDiag)
	}
	e := d.byID[principalID]
	if e == nil {
		e = &principalDiag{}
		d.byID[principalID] = e
	}
	return e
}

func (d *diagRing) recordRequest(principalID int64, t schema.ServerRequestTraceV1) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entry(principalID).requests.add(t)
}

func (d *diagRing) recordClose(principalID int64, c schema.ServerSocketCloseV1) {
	if principalID == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entry(principalID).closes.add(c)
}

// snapshot is what the server knows of principalID's recent activity.
func (d *diagRing) snapshot(principalID int64) schema.ProblemReportServerV1 {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.byID[principalID]
	if e == nil {
		return schema.ProblemReportServerV1{Requests: []schema.ServerRequestTraceV1{}, SocketCloses: []schema.ServerSocketCloseV1{}}
	}
	return schema.ProblemReportServerV1{Requests: e.requests.values(), SocketCloses: e.closes.values()}
}

// durationSeconds rounds a socket's lifetime to whole seconds for a report.
func durationSeconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }
