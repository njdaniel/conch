package server

import (
	"net/http"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// maxSocketsPerCredential is how many WebSockets one credential may hold open
// at once, across /v0/ws, /v1/ws, /v2/ws, /v1/events/ws and /v1/voice/ws
// together (issue #309). Each open socket costs a goroutine, its buffers and
// work on every change, and the events socket reads SQLite to rebuild a roster;
// without a bound one principal could multiply that by as many sockets as it
// cares to open. 32 is a TUI, several browser tabs and a voice client with room
// to spare. It is a constant, not a flag.
const maxSocketsPerCredential = 32

// socketCounter counts open sockets per credential, in memory: a restart
// forgets them, and so do the sockets. The zero value is ready to use.
type socketCounter struct {
	mu   sync.Mutex
	open map[int64]int
}

// acquire counts one more socket for the credential and reports whether it
// was under the cap. A refusal counts nothing.
func (c *socketCounter) acquire(credentialID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[credentialID] >= maxSocketsPerCredential {
		return false
	}
	if c.open == nil {
		c.open = make(map[int64]int)
	}
	c.open[credentialID]++
	return true
}

// release gives back what acquire counted. A credential at zero is deleted, so
// the map holds only credentials with a socket open and cannot grow without
// bound.
func (c *socketCounter) release(credentialID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[credentialID]--; c.open[credentialID] <= 0 {
		delete(c.open, credentialID)
	}
}

// count is how many sockets the credential holds open.
func (c *socketCounter) count(credentialID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open[credentialID]
}

// admitSocket counts the request's socket against its credential. Every socket
// handler calls it once, through acceptSocket (connlog.go), after its last
// refusal and immediately before websocket.Accept, and defers the release it
// returns, so a socket is counted
// from the upgrade until its handler returns, whatever ends it (accept
// failure, close, drop, slow consumer, shutdown). Calling it that late means a
// 404 for a non-member is never turned into a 429 and no refusal has a count
// to give back.
//
// Being last also means a refusal at the cap comes after what the earlier
// gates spend: an agent's manifest budget, sessions.manage for &session=, a
// ticket redeemed by the middleware. That is the caller's own, and moving the
// count earlier would skip the manifest's audited refusal for an agent at
// the cap, which is worse (security review of #315).
//
// At the cap it has written 429 too_many_sockets and the handler must return.
// A request with no credential (authentication off) is not counted and gets a
// release that does nothing.
func (s *Server) admitSocket(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	credID, has := credentialIDFrom(r.Context())
	if !has {
		return func() {}, true
	}
	if !s.sockets.acquire(credID) {
		writeError(w, http.StatusTooManyRequests, schema.ErrorCodeTooManySockets, "too many open sockets for this credential; close one first")
		return nil, false
	}
	return func() { s.sockets.release(credID) }, true
}
