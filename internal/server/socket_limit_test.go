package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The per-credential cap on open WebSockets (issue #309). Every socket read
// has a deadline. A socket ending is seen by the server a moment after the
// client ends it, so a test that wants the count back polls it against a
// deadline (waitSockets) instead of sleeping a guessed time.

// socketRoutes are the six routes the cap spans, as paths on channel alpha.
// The occupancy socket (issue #596) names no channel: it is the caller's
// across every channel they are in.
var socketRoutes = []struct {
	name string
	path string
}{
	{"v0", "/v0/ws?channel=alpha"},
	{"v1", "/v1/ws?channel=alpha"},
	{"v2", "/v2/ws?channel=alpha"},
	{"events", "/v1/events/ws?channel=alpha"},
	{"voice", "/v1/voice/ws?channel=alpha"},
	{"voice occupancy", "/v1/voice/occupancy/ws"},
}

// perChannel reports whether a socket route of socketRoutes is opened on one
// channel, and so is refused to a non-member and closed by a removal.
func perChannel(path string) bool { return strings.HasSuffix(path, "?channel=alpha") }

func capDial(base, path, token string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hdr http.Header
	if token != "" {
		hdr = http.Header{"Authorization": {"Bearer " + token}}
	}
	return websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: hdr}) //nolint:bodyclose // closed by the caller when refused
}

// capOpen dials and fails the test unless the upgrade succeeds. The socket
// is closed with the test.
func capOpen(t *testing.T, base, path, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := capDial(base, path, token)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial %s = %v (status %d)", path, err, status)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	// A server-side close waits for the client's close frame, which only a
	// reading client sends; drain until the socket ends.
	go func() {
		ctx := context.Background()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	return conn
}

// capRefused dials and fails the test unless the upgrade is refused with
// the status and error code, and returns nothing upgraded.
func capRefused(t *testing.T, base, path, token string, status int, code string) {
	t.Helper()
	conn, resp, err := capDial(base, path, token)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatalf("dial %s: upgraded, want %d %s", path, status, code)
	}
	if resp == nil {
		t.Fatalf("dial %s: no response (%v)", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != status {
		t.Fatalf("dial %s: status = %d, want %d", path, resp.StatusCode, status)
	}
	var e schema.Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("dial %s: decode error body: %v", path, err)
	}
	if e.Code != code {
		t.Fatalf("dial %s: code = %q, want %q", path, e.Code, code)
	}
}

// waitSockets waits until the credential's count is want, failing at a
// deadline.
func waitSockets(t *testing.T, srv *Server, credID int64, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for srv.sockets.count(credID) != want {
		select {
		case <-deadline.C:
			t.Fatalf("socket count = %d, want %d", srv.sockets.count(credID), want)
		case <-tick.C:
		}
	}
}

func TestSocketCounter(t *testing.T) {
	tests := []struct {
		name string
		do   func(c *socketCounter) (granted []bool)
		want []bool
		open map[int64]int
	}{
		{
			"refuses at the cap and counts nothing for the refusal",
			func(c *socketCounter) []bool {
				var got []bool
				for i := 0; i <= maxSocketsPerCredential; i++ {
					got = append(got, c.acquire(1))
				}
				return got[maxSocketsPerCredential-1:]
			},
			[]bool{true, false},
			map[int64]int{1: maxSocketsPerCredential},
		},
		{
			"release at the cap lets one more in",
			func(c *socketCounter) []bool {
				for i := 0; i < maxSocketsPerCredential; i++ {
					c.acquire(1)
				}
				c.release(1)
				return []bool{c.acquire(1), c.acquire(1)}
			},
			[]bool{true, false},
			map[int64]int{1: maxSocketsPerCredential},
		},
		{
			"credentials count apart",
			func(c *socketCounter) []bool {
				for i := 0; i < maxSocketsPerCredential; i++ {
					c.acquire(1)
				}
				return []bool{c.acquire(2)}
			},
			[]bool{true},
			map[int64]int{1: maxSocketsPerCredential, 2: 1},
		},
		{
			"a credential at zero is deleted",
			func(c *socketCounter) []bool {
				c.acquire(1)
				c.acquire(2)
				c.release(1)
				c.release(2)
				return nil
			},
			nil,
			map[int64]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c socketCounter
			got := tt.do(&c)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
			if len(c.open) != len(tt.open) {
				t.Fatalf("open = %v, want %v", c.open, tt.open)
			}
			for k, v := range tt.open {
				if c.open[k] != v {
					t.Fatalf("open = %v, want %v", c.open, tt.open)
				}
			}
		})
	}
}

// The cap holds across the six routes together, whichever route the 33rd
// arrives on: the other 32 are a mix of all six.
func TestSocketCapAcrossRoutes(t *testing.T) {
	for _, tt := range socketRoutes {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemberFixture(t)
			base := wsTestServer(t, f.srv)
			credID, tok := f.newCred(t, f.alice.ID)

			conns := make([]*websocket.Conn, 0, maxSocketsPerCredential)
			for i := 0; i < maxSocketsPerCredential; i++ {
				conns = append(conns, capOpen(t, base, socketRoutes[i%len(socketRoutes)].path, tok))
			}
			waitSockets(t, f.srv, credID, maxSocketsPerCredential)

			capRefused(t, base, tt.path, tok, http.StatusTooManyRequests, schema.ErrorCodeTooManySockets)
			if n := f.srv.sockets.count(credID); n != maxSocketsPerCredential {
				t.Fatalf("a refusal changed the count to %d", n)
			}

			// Another credential of the same principal has its own 32.
			_, otherTok := f.newCred(t, f.alice.ID)
			capOpen(t, base, tt.path, otherTok)

			// Closing one lets the next upgrade through.
			if err := conns[0].CloseNow(); err != nil {
				t.Logf("close: %v", err)
			}
			waitSockets(t, f.srv, credID, maxSocketsPerCredential-1)
			capOpen(t, base, tt.path, tok)
			waitSockets(t, f.srv, credID, maxSocketsPerCredential)
			capRefused(t, base, tt.path, tok, http.StatusTooManyRequests, schema.ErrorCodeTooManySockets)
		})
	}
}

// A socket opened with a ticket counts against the credential that asked for
// the ticket.
func TestSocketCapCountsTicketSockets(t *testing.T) {
	f := newTicketFixture(t)
	base := wsTestServer(t, f.srv)
	credID, tok := f.newCred(t, f.alice.ID)
	for i := 0; i < maxSocketsPerCredential; i++ {
		capOpen(t, base, "/v2/ws?channel=alpha&ticket="+f.ticketFor(t, tok), "")
	}
	waitSockets(t, f.srv, credID, maxSocketsPerCredential)
	capRefused(t, base, "/v2/ws?channel=alpha&ticket="+f.ticketFor(t, tok), "", http.StatusTooManyRequests, schema.ErrorCodeTooManySockets)
	capRefused(t, base, "/v1/events/ws?channel=alpha", tok, http.StatusTooManyRequests, schema.ErrorCodeTooManySockets)
}

// Many upgrades at once never get past the cap, and every one that does not
// get in is refused with 429.
func TestSocketCapConcurrent(t *testing.T) {
	f := newMemberFixture(t)
	base := wsTestServer(t, f.srv)
	credID, tok := f.newCred(t, f.alice.ID)
	const tries = 96

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
		other    []string
	)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			conn, resp, err := capDial(base, path, tok)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
				t.Cleanup(func() { _ = conn.CloseNow() })
			case resp != nil && resp.StatusCode == http.StatusTooManyRequests:
				refused++
				_ = resp.Body.Close()
			default:
				other = append(other, err.Error())
			}
		}(socketRoutes[i%len(socketRoutes)].path)
	}
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("unexpected failures: %v", other)
	}
	if accepted != maxSocketsPerCredential || refused != tries-maxSocketsPerCredential {
		t.Fatalf("accepted %d, refused %d; want %d and %d", accepted, refused, maxSocketsPerCredential, tries-maxSocketsPerCredential)
	}
	waitSockets(t, f.srv, credID, maxSocketsPerCredential)
}

// No count leaks: after every way a counted socket can end, and after an
// upgrade that fails once counted, the count is back where it was.
func TestSocketCountDoesNotLeak(t *testing.T) {
	type env struct {
		f      *memberFixture
		base   string
		credID int64
		token  string
	}
	ends := []struct {
		name string
		// run opens a socket on path, ends it and returns once it is ended
		// from the client's side.
		run func(t *testing.T, e env, path string)
	}{
		{"peer close", func(t *testing.T, e env, path string) {
			conn := capOpen(t, e.base, path, e.token)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}},
		{"peer drop", func(t *testing.T, e env, path string) {
			conn := capOpen(t, e.base, path, e.token)
			_ = conn.CloseNow()
		}},
		{"removed from the channel", func(t *testing.T, e env, path string) {
			capOpen(t, e.base, path, e.token)
			waitSockets(t, e.f.srv, e.credID, 1)
			if rec := e.f.do(t, "DELETE", fmt.Sprintf("/v1/channels/alpha/members/%d", e.f.alice.ID), e.f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("remove member = %d %s", rec.Code, rec.Body)
			}
		}},
		{"credential revoked", func(t *testing.T, e env, path string) {
			capOpen(t, e.base, path, e.token)
			waitSockets(t, e.f.srv, e.credID, 1)
			if rec := e.f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", e.credID), e.f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
			}
		}},
		{"shutdown", func(t *testing.T, e env, path string) {
			capOpen(t, e.base, path, e.token)
			waitSockets(t, e.f.srv, e.credID, 1)
			e.f.srv.hub.Close()
			e.f.srv.voice.closeAll()
			e.f.srv.events.closeAll()
		}},
	}
	for _, route := range socketRoutes {
		for _, end := range ends {
			if end.name == "removed from the channel" && !perChannel(route.path) {
				continue // a removal takes a row out of the occupancy socket and keeps it open
			}
			t.Run(route.name+"/"+end.name, func(t *testing.T) {
				f := newMemberFixture(t)
				f.srv.credRecheckInterval = 10 * time.Millisecond
				credID, tok := f.newCred(t, f.alice.ID)
				e := env{f: f, base: wsTestServer(t, f.srv), token: tok, credID: credID}
				end.run(t, e, route.path)
				waitSockets(t, f.srv, e.credID, 0)
				if n := len(f.srv.sockets.open); n != 0 {
					t.Fatalf("counter map holds %d entries after the socket ended", n)
				}
			})
		}
	}
}

// The refusals that come before the count, and the accept that fails after
// it, leave the count where it was; a 404 for a non-member stays a 404 even
// when the credential is at the cap.
func TestSocketRefusalsDoNotLeakOrCount(t *testing.T) {
	for _, route := range socketRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newMemberFixture(t)
			base := wsTestServer(t, f.srv)
			credID, tok := f.newCred(t, f.alice.ID)
			carolID, carolTok := f.newCred(t, f.carol.ID)

			// Accept fails (no upgrade headers) once counted.
			req, err := http.NewRequest(http.MethodGet, "http"+base[len("ws"):]+route.path, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusSwitchingProtocols || resp.StatusCode < 400 {
				t.Fatalf("a plain GET got %d, want an accept failure", resp.StatusCode)
			}
			waitSockets(t, f.srv, credID, 0)
			if !perChannel(route.path) {
				return // no channel to refuse; voice_occupancy_test.go has its refusals
			}

			// A non-member is 404 whatever else; the same with a channel
			// that does not exist, and neither is counted.
			path := route.path[:len(route.path)-len("alpha")]
			capRefused(t, base, path+"nosuch", carolTok, http.StatusNotFound, "channel_not_found")
			capRefused(t, base, route.path, carolTok, http.StatusNotFound, "channel_not_found")
			if n := f.srv.sockets.count(carolID); n != 0 {
				t.Fatalf("a refused upgrade left a count of %d", n)
			}

			// At the cap, a non-member still gets the 404 and not a 429.
			for i := 0; i < maxSocketsPerCredential; i++ {
				f.srv.sockets.acquire(carolID)
			}
			capRefused(t, base, route.path, carolTok, http.StatusNotFound, "channel_not_found")
			if n := f.srv.sockets.count(carolID); n != maxSocketsPerCredential {
				t.Fatalf("count = %d after a 404 at the cap", n)
			}
		})
	}
}

// With authentication off there is no credential, so nothing is counted and
// there is no cap.
func TestSocketCapAuthOff(t *testing.T) {
	srv := newTestServer(t)
	createTestChannelAndPrincipal(t, srv)
	base := wsTestServer(t, srv)
	for i := 0; i < maxSocketsPerCredential+8; i++ {
		capOpen(t, base, []string{"/v0/ws?channel=general", "/v1/ws?channel=general", "/v2/ws?channel=general"}[i%3], "")
	}
	if n := len(srv.sockets.open); n != 0 {
		t.Fatalf("counted %d credentials with authentication off", n)
	}
}
