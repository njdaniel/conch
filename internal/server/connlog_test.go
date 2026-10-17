package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// Connection logs (issue #446): one line when a socket opens and one when it
// closes, with why, from every socket handler; a reconnect storm summarised.

// waitLines polls the capture until it holds n lines with every substring, and
// returns them.
func waitLines(t *testing.T, logs *diagLogs, n int, has ...string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := logs.lines(has...)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d log lines with %q, want %d; the log:\n%s", len(got), has, n, logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var (
	closeLineRe = regexp.MustCompile(`msg="ws: socket closed" route="([^"]+)" principal=(\d+) credential=(\d+) duration=\S+ reason=(\w+) frames=(\d+) request_id=([0-9a-f]{16})$`)
	openLineRe  = regexp.MustCompile(`msg="ws: socket opened" route="([^"]+)" principal=(\d+) credential=(\d+) request_id=([0-9a-f]{16})$`)
)

// checkSocketLines checks that the socket of route opened by principal with
// credential has exactly one open and one close line, the close with reason,
// both with the same request id.
func checkSocketLines(t *testing.T, logs *diagLogs, route string, principal, credential int64, reason schema.SocketCloseReason) {
	t.Helper()
	closes := waitLines(t, logs, 1, `msg="ws: socket closed"`, fmt.Sprintf("credential=%d ", credential))
	opens := logs.lines(`msg="ws: socket opened"`, fmt.Sprintf("credential=%d ", credential))
	if len(opens) != 1 || len(closes) != 1 {
		t.Fatalf("open lines %q, close lines %q; want one of each", opens, closes)
	}
	o, c := openLineRe.FindStringSubmatch(opens[0]), closeLineRe.FindStringSubmatch(closes[0])
	if o == nil || c == nil {
		t.Fatalf("lines not in the expected shape:\n%s\n%s", opens[0], closes[0])
	}
	want := []string{route, fmt.Sprint(principal), fmt.Sprint(credential)}
	for i, w := range want {
		if o[i+1] != w || c[i+1] != w {
			t.Errorf("field %d: open %q, close %q, want %q", i, o[i+1], c[i+1], w)
		}
	}
	if c[4] != string(reason) {
		t.Errorf("reason = %s, want %s\n%s", c[4], reason, closes[0])
	}
	if o[4] != c[6] {
		t.Errorf("open and close request ids differ: %s, %s", o[4], c[6])
	}
}

// Each close reason, from a real socket, as the one close line says it.
func TestSocketCloseReasons(t *testing.T) {
	routeOf := func(path string) string {
		p, _, _ := strings.Cut(path, "?")
		return "GET " + p
	}
	type setup struct {
		f      *memberFixture
		base   string
		credID int64
		tok    string
	}
	tests := []struct {
		name   string
		path   string
		before func(s *setup)
		// open opens the socket; nil is capOpen with the credential.
		open  func(t *testing.T, s *setup) *websocket.Conn
		cause func(t *testing.T, s *setup, conn *websocket.Conn)
		want  schema.SocketCloseReason
	}{
		{name: "client closes", path: "/v2/ws?channel=alpha",
			cause: func(t *testing.T, _ *setup, conn *websocket.Conn) { _ = conn.Close(websocket.StatusNormalClosure, "") },
			want:  schema.SocketCloseClient},
		{name: "client vanishes", path: "/v1/events/ws?channel=alpha",
			cause: func(t *testing.T, _ *setup, conn *websocket.Conn) { _ = conn.CloseNow() },
			want:  schema.SocketCloseClient},
		{name: "server shutdown, messages", path: "/v0/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) { s.f.srv.hub.Close() },
			want:  schema.SocketCloseServerShutdown},
		{name: "server shutdown, events", path: "/v1/events/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) { s.f.srv.events.closeAll() },
			want:  schema.SocketCloseServerShutdown},
		{name: "server shutdown, voice", path: "/v1/voice/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) { s.f.srv.voice.closeAll() },
			want:  schema.SocketCloseServerShutdown},
		{name: "keepalive timeout", path: "/v1/ws?channel=alpha",
			before: func(s *setup) { s.f.srv.pingInterval, s.f.srv.pingTimeout = 30*time.Millisecond, 60*time.Millisecond },
			open: func(t *testing.T, s *setup) *websocket.Conn {
				// Dialled and never read, so no ping is answered.
				conn, _, err := capDial(s.base, "/v1/ws?channel=alpha", s.tok)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.CloseNow() })
				return conn
			},
			cause: func(*testing.T, *setup, *websocket.Conn) {},
			want:  schema.SocketCloseKeepaliveTimeout},
		{name: "credential revoked", path: "/v2/ws?channel=alpha",
			before: func(s *setup) { s.f.srv.credRecheckInterval = 20 * time.Millisecond },
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) {
				if err := s.f.srv.store.RevokeCredential(context.Background(), "system", s.credID); err != nil {
					t.Fatal(err)
				}
			},
			want: schema.SocketCloseRevoked},
		{name: "principal disabled", path: "/v1/voice/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) {
				if rec := s.f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", s.f.alice.ID), s.f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("disable = %d %s", rec.Code, rec.Body)
				}
			},
			want: schema.SocketCloseRevoked},
		{name: "removed from the channel", path: "/v2/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) {
				if rec := s.f.do(t, "DELETE", fmt.Sprintf("/v1/channels/alpha/members/%d", s.f.alice.ID), s.f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("remove = %d %s", rec.Code, rec.Body)
				}
			},
			want: schema.SocketCloseRemoved},
		{name: "slow consumer", path: "/v1/events/ws?channel=alpha",
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) {
				// What a full queue does (eventsSub.push): the subscriber is
				// dropped for nothing else.
				h := s.f.srv.events
				h.mu.Lock()
				for sub := range h.subs {
					h.dropLocked(sub)
				}
				h.mu.Unlock()
			},
			want: schema.SocketCloseSlowConsumer},
		{name: "store failure", path: "/v2/ws?channel=alpha",
			before: func(s *setup) { s.f.srv.credRecheckInterval = 20 * time.Millisecond },
			cause: func(t *testing.T, s *setup, _ *websocket.Conn) {
				// The recheck cannot read the store; the socket fails closed.
				if err := s.f.srv.store.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: schema.SocketCloseError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureDiagLogs(t)
			s := &setup{f: newMemberFixture(t)}
			if tt.before != nil {
				tt.before(s)
			}
			s.base = wsTestServer(t, s.f.srv)
			s.credID, s.tok = s.f.newCred(t, s.f.alice.ID)
			var conn *websocket.Conn
			if tt.open != nil {
				conn = tt.open(t, s)
			} else {
				conn = capOpen(t, s.base, tt.path, s.tok)
			}
			waitLines(t, logs, 1, `msg="ws: socket opened"`, fmt.Sprintf("credential=%d ", s.credID))
			tt.cause(t, s, conn)
			checkSocketLines(t, logs, routeOf(tt.path), s.f.alice.ID, s.credID, tt.want)

			// The reporter's ring has the close too.
			closes := s.f.srv.diag.snapshot(s.f.alice.ID).SocketCloses
			if len(closes) != 1 || closes[0].Reason != tt.want || closes[0].Route != routeOf(tt.path) || !schema.ValidRequestID(closes[0].RequestID) {
				t.Errorf("ring closes = %+v", closes)
			}
		})
	}
}

// The events socket an ended session kept alive says so.
func TestSocketCloseReasonSessionEnded(t *testing.T) {
	logs := captureDiagLogs(t)
	f := newSessionFixture(t)
	ticks := make(chan time.Time)
	f.srv.keepAliveTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	base := wsTestServer(t, f.srv)
	credID, tok := f.newCred(t, f.alice.ID)
	mine := f.create(t, tok, humanBody)
	c := dialEvents(t, base, fmt.Sprintf("/v1/events/ws?channel=general&session=%d", mine.ID), tok)
	c.next()
	if rec := f.do(t, "DELETE", sessionPath(mine.ID, ""), tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end session = %d %s", rec.Code, rec.Body)
	}
	select {
	case ticks <- f.clock.Now():
	case <-time.After(eventsReadTimeout):
		t.Fatal("the socket took no keep-alive tick")
	}
	_ = c.closed()
	checkSocketLines(t, logs, "GET /v1/events/ws", f.alice.ID, credID, schema.SocketCloseSessionEnded)
	// It sent frames (the roster at least), and the line says how many.
	line := logs.lines(`msg="ws: socket closed"`)[0]
	if m := closeLineRe.FindStringSubmatch(line); m == nil || m[5] == "0" {
		t.Errorf("frames not counted: %s", line)
	}
}

// A reconnect storm from one credential: the first connLogFull sockets of a
// window are logged in full, the rest counted, and the counts written once,
// by the first socket after the window.
func TestSocketLogStormSummary(t *testing.T) {
	logs := captureDiagLogs(t)
	f := newMemberFixture(t)
	clock := newManualClock()
	f.srv.now = clock.Now
	base := wsTestServer(t, f.srv)
	credID, tok := f.newCred(t, f.alice.ID)
	cred := fmt.Sprintf("credential=%d ", credID)

	const storm = connLogFull + 7
	for range storm {
		conn := capOpen(t, base, "/v2/ws?channel=alpha", tok)
		waitSockets(t, f.srv, credID, 1)
		_ = conn.Close(websocket.StatusNormalClosure, "")
		waitSockets(t, f.srv, credID, 0)
	}
	if n := len(logs.lines(`msg="ws: socket opened"`, cred)); n != connLogFull {
		t.Errorf("%d open lines in the storm, want %d", n, connLogFull)
	}
	if n := len(logs.lines(`msg="ws: socket closed"`, cred)); n != connLogFull {
		t.Errorf("%d close lines in the storm, want %d", n, connLogFull)
	}
	if n := len(logs.lines("sockets summarised")); n != 0 {
		t.Errorf("summarised before the window ended: %d", n)
	}

	clock.Advance(connLogWindow)
	conn := capOpen(t, base, "/v2/ws?channel=alpha", tok)
	waitSockets(t, f.srv, credID, 1)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitSockets(t, f.srv, credID, 0)
	sums := logs.lines("sockets summarised", cred)
	if len(sums) != 1 {
		t.Fatalf("summary lines = %q, want 1", sums)
	}
	for _, want := range []string{fmt.Sprintf("opened=%d", storm-connLogFull), fmt.Sprintf("closed=%d", storm-connLogFull), fmt.Sprintf("client=%d", storm-connLogFull), fmt.Sprintf("principal=%d", f.alice.ID)} {
		if !strings.Contains(sums[0], want) {
			t.Errorf("summary %q lacks %q", sums[0], want)
		}
	}
	// The new window logs its first socket in full again.
	if n := len(logs.lines(`msg="ws: socket opened"`, cred)); n != connLogFull+1 {
		t.Errorf("%d open lines, want %d", n, connLogFull+1)
	}
	// Another credential was never thinned by alice's storm.
	otherID, otherTok := f.newCred(t, f.alice.ID)
	c2 := capOpen(t, base, "/v2/ws?channel=alpha", otherTok)
	waitLines(t, logs, 1, `msg="ws: socket opened"`, fmt.Sprintf("credential=%d ", otherID))
	_ = c2.Close(websocket.StatusNormalClosure, "")
}

// The limiter alone, on its own clock: what is logged, what is counted, and
// that idle credentials are forgotten.
func TestConnLogLimiter(t *testing.T) {
	var l connLogLimiter
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for i := range connLogFull + 2 {
		full, sum := l.open(7, 3, t0)
		if full != (i < connLogFull) || sum != nil {
			t.Fatalf("open %d: full=%v summary=%v", i, full, sum)
		}
	}
	if sum := l.close(7, 3, schema.SocketCloseKeepaliveTimeout, false, t0); sum != nil {
		t.Fatal("summary inside the window")
	}
	// Another credential is its own.
	if full, _ := l.open(8, 3, t0); !full {
		t.Error("a second credential was thinned by the first one's storm")
	}
	sum := l.close(7, 3, schema.SocketCloseClient, true, t0.Add(connLogWindow))
	if sum == nil || sum.opened != 2 || sum.closed[schema.SocketCloseKeepaliveTimeout] != 1 || sum.principalID != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum := l.close(7, 3, schema.SocketCloseClient, true, t0.Add(connLogWindow+time.Second)); sum != nil {
		t.Errorf("a second summary for the same window: %+v", sum)
	}
	// Two windows of nothing: both credentials are pruned.
	l.open(9, 4, t0.Add(3*connLogWindow))
	if _, ok := l.entries[7]; ok {
		t.Error("an idle credential was kept")
	}
	if _, ok := l.entries[8]; ok {
		t.Error("an idle credential was kept")
	}
}

// The voice socket opened with a ticket (issue #442) logs its open and close
// like the others, and neither line, nor anything else logged, holds the
// ticket, a piece of it, or the channel's name.
func TestVoiceSocketTicketNeverLogged(t *testing.T) {
	logs := captureDiagLogs(t)
	f := newTicketFixture(t)
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var tickets []string
	for range 3 {
		tk := f.ticketFor(t, f.aliceTok)
		tickets = append(tickets, tk)
		conn, resp, err := openTicketSocket(ctx, base, "/v1/voice/ws?channel=alpha&ticket="+tk)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	lines := waitLines(t, logs, 3, `msg="ws: socket closed"`, `route="GET /v1/voice/ws"`)
	if n := len(logs.lines(`msg="ws: socket opened"`, `route="GET /v1/voice/ws"`)); n != 3 {
		t.Errorf("%d open lines, want 3", n)
	}
	all := logs.String()
	for _, tk := range tickets {
		if strings.Contains(all, tk) {
			t.Error("the log holds a ticket")
		}
		for i := len(schema.WSTicketPrefix); i+16 <= len(tk); i += 8 {
			if strings.Contains(all, tk[i:i+16]) {
				t.Error("the log holds a piece of a ticket")
			}
		}
	}
	if strings.Contains(all, schema.WSTicketPrefix) || strings.Contains(all, "ticket=") {
		t.Error("the log holds a ticket prefix or a socket address")
	}
	for _, line := range append(lines, logs.lines("ws: socket opened")...) {
		if strings.Contains(line, "alpha") {
			t.Errorf("a connection line names the channel: %s", line)
		}
	}
}
