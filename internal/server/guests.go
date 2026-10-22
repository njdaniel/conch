package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Temporary guests on the server (issue #705, ADR-009,
// docs/design/guests-and-accounts.md §3.4, §4.2). The store holds the visit
// and decides every transition; here are End visit, the deadline sweeper and
// what follows a visit's end, whichever call ended it: the guest's sockets
// close in every family, it leaves voice, and the rosters and member lists of
// its channels are told. No route here lets anyone become a guest: the entry
// routes are #710's.

// defaultGuestSweepInterval is the sweeper's fallback poll (design §4.2: at
// most a second). It also wakes at the next deadline when that is sooner.
const defaultGuestSweepInterval = time.Second

// guestSweepFloor keeps the sweeper from spinning on a deadline it has just
// passed but not yet swept.
const guestSweepFloor = 10 * time.Millisecond

// guestsEnded does what follows the committed end of guest visits. The
// store calls it after the commit (OnGuestsEnded): the drops come first, so
// no socket of the guest is sent another frame, then voice, then the member
// lists of the channels it left and the session rosters its revoked
// credential took it off.
func (s *Server) guestsEnded(ended []store.EndedGuest) {
	ctx := context.Background()
	for _, g := range ended {
		s.hub.DropPrincipalAll(g.PrincipalID)
		s.events.dropPrincipalAll(g.PrincipalID)
	}
	for _, g := range ended {
		// Its presence and occupancy sockets close and any room it held a
		// session for rotates; then it is removed, by name, from the rooms
		// of the channels it was in, in case it is in one without a session.
		s.voicePrincipalLostAccess(ctx, g.PrincipalID, voiceReasonCredsRevoked)
		for _, ch := range g.Channels {
			s.voiceMemberRemoved(ctx, ch, g.PrincipalID)
			s.events.notifyChannelRoster(ch)
		}
		slog.InfoContext(ctx, "guests: visit ended", "principal", g.PrincipalID, "reason", string(g.Reason))
	}
	s.events.notifyRosters()
}

// handleEndVisit serves POST /v1/guest/end: a guest ends its own visit. The
// visit becomes terminal, its memberships go and its credential is revoked
// in one transaction; the answer is the visit as it now is, the server's
// confirmation that it ended (design §3.4). Anyone who is not a guest is 403
// not_a_guest; a visit already ended is 409 guest_ended. With
// authentication off there is no visit to end: 403 accounts_require_auth.
func (s *Server) handleEndVisit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.cfg.authRequired() {
		writeAccountsRequireAuth(w)
		return
	}
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return
	}
	if !caller.IsGuest() {
		writeNotAGuest(w)
		return
	}
	v, err := s.store.EndGuestVisit(ctx, auditActor(ctx), caller.ID, schema.GuestEndVisit)
	switch {
	case errors.Is(err, store.ErrNotAGuest):
		writeNotAGuest(w)
		return
	case errors.Is(err, store.ErrGuestEnded):
		writeError(w, http.StatusConflict, schema.ErrorCodeGuestEnded, "the visit has already ended")
		return
	case err != nil:
		slog.ErrorContext(ctx, "guests: end visit failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.EndVisitResponseV1{Guest: v.V1()})
}

func writeNotAGuest(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeNotAGuest, "only a guest has a visit to end")
}

// writeAccountsRequireAuth is the answer of every guest and account route
// with authentication off (design §7.4): there is no identity to join as or
// end.
func writeAccountsRequireAuth(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth, "guests and accounts need authentication on")
}

// guestWhoAmI fills whoami's identity class and, for a guest, its visit.
func (s *Server) guestWhoAmI(ctx context.Context, caller store.Principal, resp *schema.WhoAmIResponseV1) error {
	switch {
	case caller.Kind == store.PrincipalAgent:
		resp.IdentityClass = schema.IdentityAgent
	case caller.IsGuest():
		resp.IdentityClass = schema.IdentityGuest
	case isOperator(caller):
		resp.IdentityClass = schema.IdentityOperator
	default:
		resp.IdentityClass = schema.IdentityMember
	}
	if !caller.IsGuest() {
		return nil
	}
	v, err := s.store.GuestVisitOf(ctx, caller.ID)
	if err != nil {
		return err
	}
	g := v.V1()
	resp.Guest = &g
	return nil
}

// startGuestSweeper runs the deadline sweeper until ctx ends or the returned
// stop is called; stop waits for it. It sweeps at once, so a visit whose
// deadline passed while the server was down ends before anything else
// happens to it (access was refused at its deadline already), then at the
// next deadline or after guestSweepInterval, whichever is sooner.
func (s *Server) startGuestSweeper(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runGuestSweeper(ctx)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (s *Server) runGuestSweeper(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := s.store.SweepGuests(ctx); err != nil {
			if ctx.Err() == nil {
				slog.ErrorContext(ctx, "guests: sweep failed; the next pass tries again", "error", err)
			}
			// Not at the overdue deadline, which would retry at once.
			timer.Reset(s.guestSweepEvery())
			continue
		}
		timer.Reset(s.nextGuestSweep(ctx))
	}
}

// guestSweepEvery is the sweeper's fallback poll.
func (s *Server) guestSweepEvery() time.Duration {
	if s.guestSweepInterval > 0 {
		return s.guestSweepInterval
	}
	return defaultGuestSweepInterval
}

// nextGuestSweep is how long until the next sweep: until the earliest
// deadline of an active visit, at most guestSweepInterval.
func (s *Server) nextGuestSweep(ctx context.Context) time.Duration {
	wait := s.guestSweepEvery()
	next, ok, err := s.store.NextGuestDeadline(ctx)
	if err != nil || !ok {
		return wait
	}
	return max(guestSweepFloor, min(wait, time.Until(next)))
}
