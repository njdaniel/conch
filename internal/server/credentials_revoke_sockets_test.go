package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// Revoking one credential closes every socket opened with it at once, in
// every family, and none of the same principal's on another credential
// (issue #519). The re-check interval is an hour in these tests, so a socket
// that closes has been closed by the revoke itself, not by its re-check.

// frameLog records every frame one socket receives, and how it ended.
type frameLog struct {
	done chan struct{}

	mu     sync.Mutex
	frames int
	err    error
}

func recordFrames(conn *websocket.Conn) *frameLog {
	l := &frameLog{done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for {
			_, _, err := conn.Read(context.Background())
			l.mu.Lock()
			if err != nil {
				l.err = err
				l.mu.Unlock()
				return
			}
			l.frames++
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *frameLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frames
}

// waitFrames waits until the socket has received at least n frames.
func (l *frameLog) waitFrames(t *testing.T, what string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.count() >= n {
			return
		}
		select {
		case <-l.done:
			t.Fatalf("%s closed after %d frames, waiting for %d: %v", what, l.count(), n, l.err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("%s received %d frames, want at least %d", what, l.count(), n)
}

// closed waits for the socket to end and returns its close error.
func (l *frameLog) closed(t *testing.T, what string) error {
	t.Helper()
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not closed", what)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *frameLog) open() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

// TestRevokeOneCredentialClosesItsSockets: ann holds a socket of one family
// on ops with each of two credentials, A and B, and A is revoked by each
// single-credential route. A's socket closes with the revoked reason and
// receives no frame after the revoke; B's stays open and receives what
// follows.
func TestRevokeOneCredentialClosesItsSockets(t *testing.T) {
	type fixture struct {
		*presenceFixture
		room string
	}
	families := []struct {
		name string
		path string
		// warm makes the frame each socket waits for before the revoke,
		// beyond the one it gets on connect.
		warm func(t *testing.T, f fixture)
		// connect is how many frames a socket gets on connect.
		connect int
		// later makes something B receives after the revoke; nil when the
		// revoke itself does (the events roster loses A's session).
		later func(t *testing.T, f fixture)
	}{
		{name: "v0", path: "/v0/ws?channel=ops",
			warm:  func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "warmup") },
			later: func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "after") }},
		{name: "v1", path: "/v1/ws?channel=ops",
			warm:  func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "warmup") },
			later: func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "after") }},
		{name: "v2", path: "/v2/ws?channel=ops",
			warm:  func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "warmup") },
			later: func(t *testing.T, f fixture) { f.postAs(t, f.rootTok, "ops", "after") }},
		{name: "events", path: "/v1/events/ws?channel=ops", connect: 1},
		{name: "voice", path: "/v1/voice/ws?channel=ops", connect: 1,
			later: func(t *testing.T, f fixture) {
				f.lk.setRoom(f.room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000, published: true})
				f.pass(t)
			}},
	}
	revokes := []struct {
		name string
		// revoke revokes credential a; tokA and tokB are ann's tokens.
		revoke func(t *testing.T, f fixture, a int64, tokA, tokB string) *httptest.ResponseRecorder
		want   int
	}{
		{"an operator revokes it", func(t *testing.T, f fixture, a int64, _, _ string) *httptest.ResponseRecorder {
			return f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", a), f.rootTok, "")
		}, http.StatusNoContent},
		{"its holder signs out with it", func(t *testing.T, f fixture, _ int64, tokA, _ string) *httptest.ResponseRecorder {
			return f.do(t, "DELETE", "/v1/credentials/current", tokA, "")
		}, http.StatusNoContent},
		{"its holder ends it from the other", func(t *testing.T, f fixture, a int64, _, tokB string) *httptest.ResponseRecorder {
			return f.do(t, "DELETE", mineCredentialPath(a), tokB, "")
		}, http.StatusNoContent},
		{"an operator rotates it", func(t *testing.T, f fixture, a int64, _, _ string) *httptest.ResponseRecorder {
			return f.do(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", a), f.rootTok, "")
		}, http.StatusCreated},
	}
	for _, fam := range families {
		for _, rv := range revokes {
			t.Run(fam.name+"/"+rv.name, func(t *testing.T) {
				pf := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
				pf.srv.credRecheckInterval = time.Hour
				pf.srv.eventsRosterFloor = 0
				if _, err := pf.srv.store.AddChannelMember(context.Background(), "system", pf.ops.ID, pf.root.ID, 0); err != nil {
					t.Fatal(err)
				}
				f := fixture{presenceFixture: pf, room: pf.inUse(t, pf.ops)}
				credA, tokA := f.newCred(t, f.ids["ann"])
				tokB := f.tokens["ann"]
				// A holds a session, so that the revoke, which ends it,
				// changes the roster the events sockets carry.
				if rec := f.do(t, "POST", "/v1/sessions", tokA, `{"client":"conch-tui"}`); rec.Code != http.StatusCreated {
					t.Fatalf("create session = %d %s", rec.Code, rec.Body)
				}
				base := wsTestServer(t, f.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				a := recordFrames(dialAs(t, ctx, base, fam.path, tokA))
				b := recordFrames(dialAs(t, ctx, base, fam.path, tokB))
				ready := fam.connect
				if fam.warm != nil {
					fam.warm(t, f)
					ready++
				}
				a.waitFrames(t, "A's socket", ready)
				b.waitFrames(t, "B's socket", ready)
				beforeA, beforeB := a.count(), b.count()

				if rec := rv.revoke(t, f, credA, tokA, tokB); rec.Code != rv.want {
					t.Fatalf("revoke = %d %s, want %d", rec.Code, rec.Body, rv.want)
				}
				closeErrContains(t, a.closed(t, "A's socket"), websocket.StatusPolicyViolation, reasonRevoked)
				if fam.later != nil {
					fam.later(t, f)
				}
				b.waitFrames(t, "B's socket", beforeB+1)
				if got := a.count(); got != beforeA {
					t.Errorf("A's socket received %d frames after the revoke", got-beforeA)
				}
				if !b.open() {
					t.Errorf("B's socket closed: %v", b.err)
				}
			})
		}
	}
}

// TestRevokeStopsAnEventsFrameBeingBuilt: a frame the events socket is about
// to write when the revoke of its credential commits is not sent; the socket
// closes with the revoked reason instead. The revoke runs from the test seam
// just before the write, after the frame was built.
func TestRevokeStopsAnEventsFrameBeingBuilt(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.credRecheckInterval = time.Hour
	f.srv.eventsRosterFloor = 0
	credA, tokA := f.newCred(t, f.ids["ann"])
	base := wsTestServer(t, f.srv)

	var armed atomic.Bool
	revoked := make(chan int, 1)
	f.srv.eventsBeforeWrite = func() {
		if armed.CompareAndSwap(true, false) {
			revoked <- f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", credA), f.rootTok, "").Code
		}
	}
	ann := dialEvents(t, base, "/v1/events/ws?channel=ops", tokA)
	if fr := ann.next(); fr.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %s, want the roster", fr)
	}
	armed.Store(true)
	// bob joins ops: ann is owed a member list, built and about to be sent
	// when the revoke commits.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["bob"]), f.rootTok, ""); rec.Code/100 != 2 {
		t.Fatalf("add member = %d %s", rec.Code, rec.Body)
	}
	select {
	case code := <-revoked:
		if code != http.StatusNoContent {
			t.Fatalf("revoke = %d", code)
		}
	case <-time.After(eventsReadTimeout):
		t.Fatal("the socket never reached a write")
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	_, raw, err := ann.conn.Read(ctx)
	if err == nil {
		t.Fatalf("the socket sent a frame after its credential was revoked: %s", raw)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonRevoked {
		t.Errorf("the socket ended with %v, want 1008 %q", err, reasonRevoked)
	}
}
