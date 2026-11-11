package push

import (
	"sync"
	"time"
)

// limiter holds the limits of §5.6 and §5.7, in memory and bounded:
//
//   - coalescing: at most one wake per registration per CoalesceWindow, unless
//     the registration has fetched since its last wake. A fetch lifts this
//     window and nothing else;
//   - the hourly cap: at most HourlyCap wakes per registration in any hour,
//     whatever it fetches in between. A member's own fetches are cheap for them
//     (the feed read limit is about 30 a minute), so if a fetch cleared the
//     count a member could aim about 30 wakes a minute at an endpoint they
//     registered (T11) and spend the budget of everyone else;
//   - the per-server budget: a token bucket of maxPerMinute tokens refilling
//     evenly, so a flood of posts cannot become a flood of requests;
//   - the per-principal share of that budget: no person's registrations take
//     more than 1/PrincipalShare of it in a minute (a token bucket of that
//     size), so one person cannot use all of it however many sign-ins and
//     endpoints they hold. 1/8 leaves at least seven other people's worth of
//     the budget whatever one person does, and at the default 600 a minute it
//     is 75, far above what a person's own messages make (the 30 s coalescing
//     alone holds a registration to 2 a minute, and a person holds at most
//     16 registrations);
//   - a failure backoff: after a timeout or another network failure a
//     registration is skipped for 1 minute, then 2, doubling to at most an
//     hour, until a wake succeeds. A hanging endpoint costs a worker 2 s once
//     per interval and not once per post.
//
// The state is forgotten an hour after a registration's last wake (its window
// has passed), unless it is still backing off, and holds MaxTracked at most;
// with that many live entries a new registration is refused (capped) rather
// than tracked without bound. A restart forgets the counts and the backoff,
// but the registration's last_wake_at, which the source gives, keeps the
// coalescing.
type limiter struct {
	now func() time.Time

	mu         sync.Mutex
	regs       map[string]*regState
	principals map[int64]*bucket
	global     bucket
	share      int // the principal bucket's size
}

// bucket is a token bucket.
type bucket struct {
	rate, burst, tokens float64
	updated             time.Time
}

func (b *bucket) take(now time.Time) bool {
	if elapsed := now.Sub(b.updated).Seconds(); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.updated = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (b *bucket) refund() { b.tokens = min(b.burst, b.tokens+1) }

type regState struct {
	lastWake time.Time
	// wakes are the times of the wakes of the last hour, oldest first: at most
	// HourlyCap of them.
	wakes []time.Time
	// failures is the run of network failures with no success between, and
	// until the time before which the registration is skipped.
	failures     int
	backoffUntil time.Time
}

// The backoff after a network failure: the first interval and the longest.
const (
	BackoffFirst = time.Minute
	BackoffMax   = time.Hour
	// PrincipalShare is the divisor of the budget a person may use in a
	// minute: 1/PrincipalShare of it.
	PrincipalShare = 8
)

func newLimiter(maxPerMinute int, now func() time.Time) *limiter {
	share := max(1, maxPerMinute/PrincipalShare)
	return &limiter{
		now:        now,
		regs:       map[string]*regState{},
		principals: map[int64]*bucket{},
		global:     bucket{rate: float64(maxPerMinute) / 60, burst: float64(maxPerMinute), tokens: float64(maxPerMinute), updated: now()},
		share:      share,
	}
}

// admit decides whether t gets a wake now: "" when it does, else the outcome
// that counts why not. Only a wake that passes every check changes any state
// and spends a token, so a wake that is refused costs nothing.
func (l *limiter) admit(t Target) string {
	out, _ := l.admitUndo(t)
	return out
}

// admitUndo is admit and, for a wake that was admitted, the function that
// takes it back (the wake was then dropped before it was sent: the queue was
// full), so that a wake that never went out is not counted against anything.
func (l *limiter) admitUndo(t Target) (string, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.regs[t.RegistrationID]
	if st == nil {
		if len(l.regs) >= MaxTracked {
			l.sweep(now)
			if len(l.regs) >= MaxTracked {
				return OutcomeCapped, nil
			}
		}
		st = &regState{lastWake: t.LastWakeAt}
		l.regs[t.RegistrationID] = st
	}
	if now.Before(st.backoffUntil) {
		return OutcomeBackoff, nil
	}
	fetched := !t.LastFetchAt.IsZero() && t.LastFetchAt.After(st.lastWake)
	if !fetched && !st.lastWake.IsZero() && now.Sub(st.lastWake) < CoalesceWindow {
		return OutcomeCoalesced, nil
	}
	st.trim(now)
	if len(st.wakes) >= HourlyCap {
		return OutcomeCapped, nil
	}
	pb := l.principals[t.PrincipalID]
	if pb == nil {
		if len(l.principals) >= MaxTracked {
			l.sweepPrincipals(now)
		}
		if len(l.principals) >= MaxTracked {
			return OutcomeOverBudget, nil
		}
		pb = &bucket{rate: float64(l.share) / 60, burst: float64(l.share), tokens: float64(l.share), updated: now}
		l.principals[t.PrincipalID] = pb
	}
	if !pb.take(now) {
		return OutcomeOverBudget, nil
	}
	if !l.global.take(now) {
		pb.refund()
		return OutcomeOverBudget, nil
	}
	prev := st.lastWake
	st.lastWake = now
	st.wakes = append(st.wakes, now)
	undo := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.global.refund()
		pb.refund()
		if n := len(st.wakes); n > 0 && st.wakes[n-1].Equal(now) {
			st.wakes = st.wakes[:n-1]
		}
		if st.lastWake.Equal(now) {
			st.lastWake = prev
		}
	}
	return "", undo
}

// trim drops the wakes older than an hour.
func (st *regState) trim(now time.Time) {
	i := 0
	for i < len(st.wakes) && now.Sub(st.wakes[i]) >= time.Hour {
		i++
	}
	st.wakes = st.wakes[i:]
}

// sweep forgets every registration whose last wake is over an hour old and
// that is not backing off. The caller holds the lock.
func (l *limiter) sweep(now time.Time) {
	for id, st := range l.regs {
		if now.Sub(st.lastWake) >= time.Hour && !now.Before(st.backoffUntil) {
			delete(l.regs, id)
		}
	}
}

// sweepPrincipals forgets the buckets that have refilled to full, which are
// indistinguishable from a new one. The caller holds the lock.
func (l *limiter) sweepPrincipals(now time.Time) {
	for id, b := range l.principals {
		if b.tokens+now.Sub(b.updated).Seconds()*b.rate >= b.burst {
			delete(l.principals, id)
		}
	}
}

// sent notes that a wake to id went out at now, which is what coalescing
// counts from, and ends any backoff.
func (l *limiter) sent(id string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.regs[id]; st != nil {
		if now.After(st.lastWake) {
			st.lastWake = now
		}
		st.failures, st.backoffUntil = 0, time.Time{}
	}
}

// networkFailed starts or lengthens the backoff of id after a timeout or
// another network failure: 1 minute, doubling to BackoffMax.
func (l *limiter) networkFailed(id string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.regs[id]
	if st == nil {
		return
	}
	d := BackoffMax
	if st.failures < 12 { // 1 min << 12 is past the cap; no overflow
		d = min(BackoffMax, BackoffFirst<<st.failures)
	}
	st.failures++
	st.backoffUntil = now.Add(d)
}

// forget drops the state of a registration that ended.
func (l *limiter) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.regs, id)
}
