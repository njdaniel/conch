package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// WebSocket tickets (issue #270, decision D2 of the P3 epic).
//
// A browser cannot set an Authorization header on a WebSocket, and the bearer
// token must never be in a URL. So a client that holds a bearer asks for a
// ticket (POST /v1/ws-tickets) and opens the socket with ?ticket=. A ticket
// is 32 random bytes, valid for wsTicketLifetime, spendable once, and bound to
// the credential that asked for it.
//
// A ticket is not a second kind of session. Spending one only names a
// credential; the request then goes through the same checks a bearer does
// (the credential is live, its principal is not disabled), carries the same
// caller and credential id in its context, and the socket it opens re-checks
// that credential every 30 seconds like any other. What a ticket can do is
// therefore never more than its credential could do at the moment it is spent.
//
// Nothing here logs or audits a ticket, and no log line anywhere may carry a
// request URL, because the ticket is in it.

const (
	wsTicketLifetime = schema.WSTicketLifetimeSeconds * time.Second

	// wsTicketSweepEvery is how often a redemption sweeps expired tickets
	// out of the map. Issuing always sweeps, so the bound it enforces is
	// exact; a redemption is open to anyone, so it must not be made to walk
	// the map on every request.
	wsTicketSweepEvery = time.Second
)

var errTooManyWSTickets = errors.New("server: too many unspent tickets for this credential")

// wsTicketEntry is what the server keeps of a ticket: whose it is and when it
// stops working. The ticket itself is not kept, only its hash is the key.
type wsTicketEntry struct {
	credentialID int64
	principalID  int64
	expires      time.Time
}

// wsTicketStore holds the unspent tickets, in memory. A restart forgets them
// all, which costs a client one new request. The zero value is ready to use.
// Time is passed in, so a test controls it through Server.now.
//
// It is keyed by the SHA-256 of the ticket. A lookup is a map access on a
// hash, so no secret is compared byte by byte, and a memory dump or a core
// file does not hold a ticket that can still be spent.
type wsTicketStore struct {
	mu     sync.Mutex
	byHash map[[sha256.Size]byte]wsTicketEntry
	held   map[int64]int // unspent tickets per credential id
	swept  time.Time
}

// issue creates a ticket for credentialID, held by principalID. It returns the
// ticket and its expiry, or errTooManyWSTickets when the credential already
// holds schema.MaxWSTicketsPerCredential unexpired ones.
func (t *wsTicketStore) issue(principalID, credentialID int64, now time.Time) (string, time.Time, error) {
	raw := make([]byte, schema.WSTicketRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("server: generate ws ticket: %w", err)
	}
	ticket := schema.WSTicketPrefix + base64.RawURLEncoding.EncodeToString(raw)
	expires := now.Add(wsTicketLifetime)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byHash == nil {
		t.byHash = make(map[[sha256.Size]byte]wsTicketEntry)
		t.held = make(map[int64]int)
	}
	t.sweepLocked(now)
	if t.held[credentialID] >= schema.MaxWSTicketsPerCredential {
		return "", time.Time{}, errTooManyWSTickets
	}
	t.byHash[sha256.Sum256([]byte(ticket))] = wsTicketEntry{credentialID: credentialID, principalID: principalID, expires: expires}
	t.held[credentialID]++
	return ticket, expires, nil
}

// redeem spends ticket. The ticket is removed on this first lookup, before its
// expiry or anything else is looked at, so no failure after it can be retried
// with the same value. ok is false for a ticket that is unknown, spent or
// expired, and the three cannot be told apart.
func (t *wsTicketStore) redeem(ticket string, now time.Time) (wsTicketEntry, bool) {
	// A value of the wrong length cannot be a ticket; nothing is hashed or
	// looked up for it. The URL can be long and is chosen by the caller.
	if len(ticket) != schema.WSTicketLength {
		return wsTicketEntry{}, false
	}
	key := sha256.Sum256([]byte(ticket))

	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.swept) >= wsTicketSweepEvery {
		t.sweepLocked(now)
	}
	e, ok := t.byHash[key]
	if !ok {
		return wsTicketEntry{}, false
	}
	t.removeLocked(key, e)
	if !now.Before(e.expires) {
		return wsTicketEntry{}, false
	}
	return e, true
}

// sweepLocked drops every expired ticket. t.mu must be held.
func (t *wsTicketStore) sweepLocked(now time.Time) {
	t.swept = now
	for key, e := range t.byHash {
		if !now.Before(e.expires) {
			t.removeLocked(key, e)
		}
	}
}

func (t *wsTicketStore) removeLocked(key [sha256.Size]byte, e wsTicketEntry) {
	delete(t.byHash, key)
	if t.held[e.credentialID]--; t.held[e.credentialID] <= 0 {
		delete(t.held, e.credentialID)
	}
}

// handleCreateWSTicket serves POST /v1/ws-tickets: a ticket for the
// credential the request authenticated with, humans and agents alike. It has
// no body. A request that was itself authenticated by a ticket cannot reach
// it, because this route does not accept tickets, so a ticket cannot be
// exchanged for more of them.
func (s *Server) handleCreateWSTicket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, hasCaller := callerFrom(ctx)
	credentialID, hasCredential := credentialIDFrom(ctx)
	if !hasCaller || !hasCredential {
		if !s.cfg.authRequired() {
			writeError(w, http.StatusBadRequest, schema.ErrorCodeTicketsRequireAuth, "tickets need authentication; conchd is running with authentication off")
			return
		}
		// The middleware sets both on every non-exempt route; their absence
		// is a wiring bug, which must deny rather than allow.
		writeUnauthenticated(w)
		return
	}
	ticket, expires, err := s.tickets.issue(caller.ID, credentialID, s.now())
	switch {
	case errors.Is(err, errTooManyWSTickets):
		writeError(w, http.StatusTooManyRequests, schema.ErrorCodeTooManyTickets, "too many unspent tickets; use one or wait for them to expire")
		return
	case err != nil:
		slog.ErrorContext(ctx, "tickets: issue failed", "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// The response carries a secret: no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, schema.WSTicketV1{Ticket: ticket, ExpiresAt: schema.NewTimestamp(expires)})
}

// redeemWSTicket authenticates r by its ?ticket= parameter. The middleware
// calls it only for a route that accepts tickets, when the request has no
// Authorization header and does have a ticket parameter. On success it returns
// the principal and credential id exactly as the bearer path does. On any
// failure it has written the response, and the caller must stop: a missing,
// repeated, used, expired or unknown ticket, and one whose credential has
// since been revoked or has expired or whose principal is disabled, all get
// the one 401 a bad bearer gets.
func (s *Server) redeemWSTicket(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	ctx := r.Context()
	values := r.URL.Query()["ticket"]
	if len(values) != 1 {
		// Two parameters is a request nobody honest sends, and which of them
		// a proxy or a log would show is not worth guessing at.
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	entry, ok := s.tickets.redeem(values[0], s.now())
	if !ok {
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	// The ticket is spent. From here the credential is judged afresh, as it
	// would be for a bearer presented now.
	live, err := s.store.CredentialLive(ctx, entry.credentialID)
	if err != nil {
		slog.ErrorContext(ctx, "tickets: check credential failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return store.Principal{}, 0, false
	}
	if !live {
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	// The principal is read now, not kept from when the ticket was issued, so
	// a role changed in between applies.
	principal, err := s.store.PrincipalByID(ctx, entry.principalID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	case err != nil:
		slog.ErrorContext(ctx, "tickets: find principal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return store.Principal{}, 0, false
	case principal.DisabledAt != nil:
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	return principal, entry.credentialID, true
}
