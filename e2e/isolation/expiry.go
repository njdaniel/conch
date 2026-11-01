package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Timed expiry (design §4.2, §9.4, §9.12), on the server's real clock and
// its real sweeper. Two guests join under the shortest bounds a visit may
// have that still let a REST request move the idle bound: 75 seconds idle
// (a request moves it at most once a minute) and 90 seconds in all. Each
// holds every socket variant, the occupancy socket and, with LiveKit, a
// participant in the plaza's room. While the rest of the guests' phase runs:
//
//   - the idle guest does nothing but hold its sockets open, which ping and
//     are pinged: they do not keep it alive, and its visit ends at its idle
//     deadline;
//   - the busy guest makes a REST request after a minute, which moves its
//     idle bound past its hard deadline: it is still in after the idle
//     guest's deadline, and its visit ends at the hard deadline, never
//     renewed past it.
//
// Each is measured from its deadline, as the server stated it when the visit
// began: sockets within the sweeper's fallback (read from the server's
// source) plus the two-second socket bound, LiveKit within one voice poller
// pass plus a room rotation. The ended visit says why it ended.

// The bounds the two guests join under.
const (
	expiryIdleSeconds     = 75
	expiryAbsoluteSeconds = 90
	// expiryRefreshAt is when the busy guest's REST request moves its idle
	// bound: after the minute the server waits between two moves, and
	// before the idle deadline.
	expiryRefreshAt = 63 * time.Second
)

// expiring is the two guests and what their timelines found.
type expiring struct {
	idle, busy         *member
	idleHeld, busyHeld holder
	sweep              time.Duration
	// idleLK and busyLK are when LiveKit stopped listing each, from its
	// deadline; busyErr how the busy guest's timeline went.
	idleLK, busyLK chan seen
	busyErr        chan error
}

// seen is when a watch saw what it waited for, or why it did not.
type seen struct {
	after time.Duration
	err   error
}

// readSweepFallback reads the guest sweeper's fallback poll,
// defaultGuestSweepInterval, out of internal/server/guests.go.
func readSweepFallback(repo string) (time.Duration, error) {
	path := filepath.Join(repo, "internal", "server", "guests.go")
	src, err := readSource(path)
	if err != nil {
		return 0, err
	}
	consts, err := durationConsts(src)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	d := consts["defaultGuestSweepInterval"]
	if d <= 0 {
		return 0, fmt.Errorf("%s: cannot read defaultGuestSweepInterval as a duration", path)
	}
	return d, nil
}

// readSource reads a file of the server's source.
func readSource(path string) (string, error) {
	src, err := os.ReadFile(path) // #nosec G304 -- the repository this run is in
	return string(src), err
}

// startExpiry lets the two guests in under the short bounds, then puts the
// default bounds back for every later visit (a visit keeps the bounds it
// began with), and starts their timelines.
func (w *world) startExpiry() (*expiring, error) {
	sweep, err := readSweepFallback(w.h.repo)
	if err != nil {
		return nil, err
	}
	e := &expiring{sweep: sweep, idleLK: make(chan seen, 1), busyLK: make(chan seen, 1), busyErr: make(chan error, 1)}
	if _, err := w.joinSettings(schema.PutJoinSettingsRequestV1{OpenJoining: ptr(schema.OpenJoiningAccount), JoinVoice: ptr(true),
		GuestIdleSeconds: ptr(int64(expiryIdleSeconds)), GuestAbsoluteSeconds: ptr(int64(expiryAbsoluteSeconds))}); err != nil {
		return nil, err
	}
	if e.idle, err = w.joinGuest("Idle", nil); err != nil {
		return nil, err
	}
	if e.busy, err = w.joinGuest("Busy", nil); err != nil {
		return nil, err
	}
	if err := w.openPlaza(); err != nil {
		return nil, err
	}
	for _, g := range []*member{e.idle, e.busy} {
		if got := g.guest.IdleExpiresAt.Time().Sub(g.guest.StartedAt.Time()); got != expiryIdleSeconds*time.Second {
			return nil, fmt.Errorf("a visit that began under a %d s idle bound has its idle deadline %s after its start", expiryIdleSeconds, got)
		}
		if got := g.guest.ExpiresAt.Time().Sub(g.guest.StartedAt.Time()); got != expiryAbsoluteSeconds*time.Second {
			return nil, fmt.Errorf("a visit that began under a %d s bound has its deadline %s after its start", expiryAbsoluteSeconds, got)
		}
	}
	if e.idleHeld, err = w.hold(e.idle, w.p.channel, ""); err != nil {
		return nil, err
	}
	if e.busyHeld, err = w.hold(e.busy, w.p.channel, ""); err != nil {
		return nil, err
	}
	if guestsHoldVoice && w.lk != nil {
		if err := w.joinVoice(e.idle, e.idleHeld.room); err != nil {
			return nil, err
		}
		if err := w.joinVoice(e.busy, e.busyHeld.room); err != nil {
			return nil, err
		}
		go func() { e.idleLK <- w.watchGone(e.idle, e.idleHeld.room, e.idle.guest.IdleExpiresAt.Time()) }()
	}
	go func() { e.busyErr <- w.busyTimeline(e) }()
	return e, nil
}

// sleepUntil waits until t, or until the run is stopped.
func sleepUntil(t time.Time) error {
	select {
	case <-time.After(time.Until(t)):
		return nil
	case <-stopCtx.Done():
		return errInterrupted
	}
}

// watchGone checks that LiveKit lists m in room just before deadline, then
// watches, every 2 ms, for the moment it stops: the time from deadline.
func (w *world) watchGone(m *member, room string, deadline time.Time) seen {
	if err := sleepUntil(deadline.Add(-1500 * time.Millisecond)); err != nil {
		return seen{err: err}
	}
	if ok, _, saw := w.lk.has(room, m.id); !ok {
		return seen{err: fmt.Errorf("LiveKit stopped listing %s before its deadline: %s", m.name, saw)}
	}
	for time.Now().Before(deadline.Add(60*time.Second)) && stopCtx.Err() == nil {
		if gone, _ := w.lk.gone(room, m.id); gone {
			return seen{after: time.Since(deadline)}
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, saw := w.lk.gone(room, m.id)
	return seen{err: fmt.Errorf("LiveKit still lists %s a minute after its deadline: %s", m.name, saw)}
}

// busyTimeline is the busy guest's: a REST request after a minute; still in
// after the idle guest's deadline; with LiveKit, back in voice once the
// idle guest's end has rotated the room (it took everyone in it out); and
// then the watch to its hard deadline.
func (w *world) busyTimeline(e *expiring) error {
	b := e.busy
	start := b.guest.StartedAt.Time()
	if err := sleepUntil(start.Add(expiryRefreshAt)); err != nil {
		return err
	}
	if status, _, err := b.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
		return err
	} else if status != http.StatusOK {
		return fmt.Errorf("the busy guest's request a minute in: status %d, want 200", status)
	}
	if err := sleepUntil(e.idle.guest.IdleExpiresAt.Time().Add(e.sweep + socketBound)); err != nil {
		return err
	}
	var who schema.WhoAmIResponseV1
	if err := b.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return fmt.Errorf("past the idle bound it began with, the busy guest, whose request moved it: %w", err)
	}
	if who.Guest == nil || !who.Guest.IdleExpiresAt.Time().Equal(b.guest.ExpiresAt.Time()) {
		return fmt.Errorf("the busy guest's idle deadline is %v after its request, want its hard deadline %v: never renewed past it", who.Guest, b.guest.ExpiresAt.Time())
	}
	if status, _, err := e.idle.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
		return err
	} else if status != http.StatusUnauthorized {
		return fmt.Errorf("the idle guest's credential past its idle deadline: status %d, want 401", status)
	}
	if !guestsHoldVoice || w.lk == nil {
		return nil
	}
	if err := waitFor("LiveKit to list the idle guest nowhere", 30*time.Second, func() (bool, string) {
		n, err := w.lk.anywhere(e.idle.id)
		if err != nil {
			return false, err.Error()
		}
		return n == 0, fmt.Sprintf("in %d rooms", n)
	}); err != nil {
		return err
	}
	sess, err := w.session(b, w.p.channel)
	if err != nil {
		return err
	}
	if sess.Rooms[0].Room == e.busyHeld.room {
		return fmt.Errorf("the plaza's room was not rotated when the idle guest's visit ended")
	}
	if _, err := w.h.kit.StartLK(w.lk.lkBin, w.lk.hostPort(), sess.Rooms[0].Token, nil); err != nil {
		return fmt.Errorf("the busy guest rejoins the plaza's room: %w", err)
	}
	room := sess.Rooms[0].Room
	if err := waitFor("LiveKit to list the busy guest again", 30*time.Second, func() (bool, string) {
		ok, _, saw := w.lk.has(room, b.id)
		return ok, saw
	}); err != nil {
		return err
	}
	go func() { e.busyLK <- w.watchGone(b, room, b.guest.ExpiresAt.Time()) }()
	return nil
}

// finishExpiry waits for both visits to have run out and measures them. It
// returns the two timings.
func (w *world) finishExpiry(e *expiring, bounds voiceBounds) ([]timing, error) {
	if err := <-e.busyErr; err != nil {
		return nil, err
	}
	var out []timing
	for _, c := range []struct {
		what     string
		h        holder
		deadline time.Time
		reason   schema.GuestEndReason
		lk       chan seen
	}{
		{"idle expiry (a guest holding only sockets), from its idle deadline", e.idleHeld, e.idle.guest.IdleExpiresAt.Time(), schema.GuestEndIdle, e.idleLK},
		{"hard expiry (a guest whose requests moved its idle bound), from its deadline", e.busyHeld, e.busy.guest.ExpiresAt.Time(), schema.GuestEndExpired, e.busyLK},
	} {
		t := timing{what: c.what, bound: e.sweep + socketBound}
		for _, f := range c.h.sockets {
			if err := waitFor(f.what+" to close", time.Until(c.deadline)+30*time.Second, func() (bool, string) {
				ended, _ := f.ended()
				return ended, "still open"
			}); err != nil {
				return nil, err
			}
			_, at := f.ended()
			if at.Before(c.deadline) {
				return nil, fmt.Errorf("%s: %s closed %s before the deadline", c.what, f.what, c.deadline.Sub(at).Round(time.Millisecond))
			}
			t.sockets = append(t.sockets, at.Sub(c.deadline))
		}
		if err := closedAllExpired(c.h.sockets); err != nil {
			return nil, err
		}
		if guestsHoldVoice && w.lk != nil {
			s := <-c.lk
			if s.err != nil {
				return nil, s.err
			}
			t.livekit = append(t.livekit, s.after)
		}
		bound := e.sweep + socketBound
		for i, d := range t.sockets {
			if d > bound {
				return nil, fmt.Errorf("%s: socket %d closed %s after the deadline, bound %s (the sweeper's fallback %s plus %s)", c.what, i, d.Round(time.Millisecond), bound, e.sweep, socketBound)
			}
		}
		for _, d := range t.livekit {
			if d > bounds.total() {
				return nil, fmt.Errorf("%s: LiveKit stopped listing the guest %s after the deadline, bound %s", c.what, d.Round(time.Millisecond), bounds.total())
			}
		}
		var ended schema.ListGuestsResponseV1
		if err := w.op.call(http.MethodGet, "/v1/guests?state=ended&limit=200", nil, &ended); err != nil {
			return nil, err
		}
		var reason schema.GuestEndReason
		for _, g := range ended.Guests {
			if g.ID == c.h.m.id {
				reason = g.EndReason
			}
		}
		if reason != c.reason {
			return nil, fmt.Errorf("%s: the visit ended with reason %q, want %q", c.what, reason, c.reason)
		}
		out = append(out, t)
	}
	if err := w.expiredStayOut([]holder{e.idleHeld, e.busyHeld}, bounds); err != nil {
		return nil, fmt.Errorf("after the visits ran out: %w", err)
	}
	return out, nil
}

// expiredStayOut is staysOut for a visit that ran out: what it kept from the
// start (a ticket, a voice grant) has run out by now too, so the check is of
// the credential itself, which every way back in starts from: it opens no
// socket, gets no voice session and answers nothing; and after one more
// poller pass LiveKit lists the guest in no room.
func (w *world) expiredStayOut(holders []holder, bounds voiceBounds) error {
	for _, h := range holders {
		a := h.m.api
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/v1/whoami"},
			{http.MethodPost, "/v1/channels/" + pch(w) + "/voice/session"},
			{http.MethodPost, "/v1/ws-tickets"},
		} {
			if status, _, err := a.do(c.method, c.path, nil); err != nil {
				return err
			} else if status != http.StatusUnauthorized {
				return fmt.Errorf("%s's credential after its visit ran out: %s %s answered %d, want 401", h.m.name, c.method, c.path, status)
			}
		}
		f, r, err := dialSocket(a, h.m.name+"'s credential after its visit ran out", "/v2/ws", url.Values{"channel": {w.p.channel}}, false)
		if err != nil {
			return err
		}
		if f != nil {
			f.close()
			return fmt.Errorf("%s's credential opened a socket after its visit ran out", h.m.name)
		}
		if r.status != http.StatusUnauthorized {
			return fmt.Errorf("%s's socket after its visit ran out: status %d, want 401", h.m.name, r.status)
		}
	}
	if w.lk == nil {
		return nil
	}
	if err := sleepUntil(time.Now().Add(bounds.pass)); err != nil {
		return err
	}
	for _, h := range holders {
		if n, err := w.lk.anywhere(h.m.id); err != nil {
			return err
		} else if n != 0 {
			return fmt.Errorf("one poller pass later LiveKit lists %s in %d room(s)", h.m.name, n)
		}
	}
	return nil
}

// guestsHoldVoice is whether a guest can be in voice: it cannot since A4
// (ADR-014, #1016), because permittedTx answers false for every guest, so
// the voice legs of the visits expiry have nothing to watch. The legs stay
// for when guests return.
const guestsHoldVoice = false
