package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// diagLogs collects the default logger's output, through LogHandler as
// conchd installs it, for the length of a test. Sockets log from their own
// goroutines, so reads and writes share a lock.
type diagLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *diagLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *diagLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// lines is every captured line that has all of the given substrings.
func (c *diagLogs) lines(has ...string) []string {
	var out []string
	for _, line := range strings.Split(c.String(), "\n") {
		ok := line != ""
		for _, h := range has {
			ok = ok && strings.Contains(line, h)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

// captureDiagLogs makes the default logger write, through LogHandler, to the
// returned capture until the test ends. Tests that use it do not run in
// parallel: the default logger is the process's.
func captureDiagLogs(t *testing.T) *diagLogs {
	t.Helper()
	c := &diagLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(LogHandler(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// withoutRequestID is h without its X-Request-Id, after checking that it has
// a well-formed one. Tests that compare two answers whole use it: the id is
// random for every request, so it differs between any two and says nothing
// about either.
func withoutRequestID(t *testing.T, h http.Header) http.Header {
	t.Helper()
	if id := h.Get(schema.RequestIDHeader); !schema.ValidRequestID(id) {
		t.Errorf("X-Request-Id = %q, want 16 hex characters", id)
	}
	out := h.Clone()
	out.Del(schema.RequestIDHeader)
	return out
}

var requestIDLine = regexp.MustCompile(`request_id=([0-9a-f]{16})`)

// Every answer carries a request id, whatever the route and whoever asks:
// exempt, authenticated, refused before authentication, unmatched. Each is
// different, and an id sent by the client is never echoed.
func TestRequestIDOnEveryResponse(t *testing.T) {
	for _, mode := range []AuthMode{AuthRequired, AuthOff} {
		f := newAuthFixture(t, mode)
		tests := []struct {
			name, method, path, token string
		}{
			{"exempt", "GET", "/healthz", ""},
			{"authenticated", "GET", "/v1/channels", f.aliceTok},
			{"no credential", "GET", "/v1/channels", ""},
			{"bad credential", "GET", "/v1/channels", "conch_nope"},
			{"operator route as a member", "GET", "/v1/reports", f.aliceTok},
			{"unmatched", "GET", "/v1/nothing-here", f.aliceTok},
			{"exempt write", "POST", "/v1/invites/preview", ""},
		}
		seen := map[string]bool{}
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/%s", mode, tt.name), func(t *testing.T) {
				req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`))
				if tt.token != "" {
					req.Header.Set("Authorization", "Bearer "+tt.token)
				}
				req.Header.Set(schema.RequestIDHeader, "0123456789abcdef")
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)
				id := rec.Header().Get(schema.RequestIDHeader)
				if !schema.ValidRequestID(id) {
					t.Fatalf("X-Request-Id = %q (status %d), want 16 hex characters", id, rec.Code)
				}
				if id == "0123456789abcdef" {
					t.Error("the client's X-Request-Id was echoed")
				}
				if seen[id] {
					t.Errorf("request id %s given twice", id)
				}
				seen[id] = true
				if n := len(rec.Header().Values(schema.RequestIDHeader)); n != 1 {
					t.Errorf("%d X-Request-Id headers, want 1", n)
				}
			})
		}
	}
}

// Every line a request logs with its context carries its id, the id in the
// header; an id the client sent is in no line.
func TestRequestIDInLogContext(t *testing.T) {
	logs := captureDiagLogs(t)
	f := newAuthFixture(t, AuthRequired)

	req := httptest.NewRequest("POST", "/v1/reports", strings.NewReader(reportBody(t)))
	req.Header.Set("Authorization", "Bearer "+f.aliceTok)
	req.Header.Set(schema.RequestIDHeader, "fedcba9876543210")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	id := rec.Header().Get(schema.RequestIDHeader)
	lines := logs.lines("reports: received")
	if len(lines) != 1 {
		t.Fatalf("log lines for the report = %q, want 1", lines)
	}
	if got := requestIDLine.FindStringSubmatch(lines[0]); got == nil || got[1] != id {
		t.Errorf("log line %q, want request_id=%s", lines[0], id)
	}
	if strings.Contains(logs.String(), "fedcba9876543210") {
		t.Error("the client's request id reached the log")
	}

	// The handler on its own: a context with an id adds it, one without does
	// not, and a logger's own attributes and groups are kept.
	var buf bytes.Buffer
	l := slog.New(LogHandler(slog.NewTextHandler(&buf, nil))).With("k", "v")
	l.InfoContext(withRequestID(context.Background(), "00112233aabbccdd"), "with")
	l.InfoContext(context.Background(), "without")
	l.WithGroup("g").InfoContext(withRequestID(context.Background(), "00112233aabbccdd"), "grouped", "x", 1)
	out := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(out) != 3 || !strings.Contains(out[0], "request_id=00112233aabbccdd") || !strings.Contains(out[0], "k=v") ||
		strings.Contains(out[1], "request_id") || !strings.Contains(out[2], "00112233aabbccdd") {
		t.Errorf("handler output:\n%s", buf.String())
	}
}

// The ring keeps each principal's last requests, with the route pattern (never
// the path, which names the channel), the status and the id; requests with
// no principal are not kept.
func TestRequestRing(t *testing.T) {
	f := newMemberFixture(t)
	var ids []string
	for i := range schema.MaxServerTraceEntries + 5 {
		path := "/v2/channels/alpha/messages"
		if i%2 == 1 {
			path = "/v2/channels/nosuch/messages"
		}
		rec := f.do(t, "GET", path, f.aliceTok, "")
		ids = append(ids, rec.Header().Get(schema.RequestIDHeader))
	}
	f.do(t, "GET", "/healthz", "", "")
	f.do(t, "GET", "/v1/channels", "", "")

	got := f.srv.diag.snapshot(f.alice.ID).Requests
	if len(got) != schema.MaxServerTraceEntries {
		t.Fatalf("%d requests kept, want %d", len(got), schema.MaxServerTraceEntries)
	}
	want := ids[len(ids)-schema.MaxServerTraceEntries:]
	for i, r := range got {
		if r.RequestID != want[i] {
			t.Errorf("request %d id = %s, want %s (oldest first)", i, r.RequestID, want[i])
		}
		if r.Route != "GET /v2/channels/{channel}/messages" {
			t.Errorf("request %d route = %q, want the pattern", i, r.Route)
		}
		if r.Status != http.StatusOK && r.Status != http.StatusNotFound {
			t.Errorf("request %d status = %d", i, r.Status)
		}
		if r.At.IsZero() {
			t.Errorf("request %d has no time", i)
		}
	}
	if s := f.srv.diag.snapshot(f.root.ID); len(s.Requests) != 0 || len(s.SocketCloses) != 0 {
		t.Errorf("root made no request but has %+v", s)
	}
	if s := f.srv.diag.snapshot(0); len(s.Requests) != 0 {
		t.Errorf("requests without a principal were kept: %+v", s.Requests)
	}
}
