package push

import (
	"sync"
	"time"
)

// limiter holds the limits of §5.6 and §5.7, in memory and bounded:
//
//   - coalescing: at most one wake per registration per CoalesceWindow, unless
//     the registration has fetched since its last wake (a phone that read its
//     feed is ready to hear again);
//   - the hourly cap: HourlyCap wakes per registration within an hour, after
//     which its wakes are dropped until it next fetches. A fetch lifts the
//     coalescing window but does not clear the count, so the cap is what
//     bounds a phone that fetches after every wake; a fetch after the cap was
//     reached releases it;
//   - the budget: a token bucket of maxPerMinute tokens refilling evenly, for
//     the whole server, so a flood of posts cannot become a flood of requests.
//
// The state of a registration is forgotten an hour after its last wake (the
// window has passed, so it would count nothing), and holds MaxTracked at
// most; with that many live registrations a new one is refused (capped)
// rather than tracked without bound. A restart forgets the hourly count, but
// the registration's last_wake_at, which the source gives, keeps the coalescing.
type limiter struct {
	now func() time.Time

	mu      sync.Mutex
	regs    map[string]*regState
	rate    float64 // tokens per second
	burst   float64
	tokens  float64
	updated time.Time
}

type regState struct {
	lastWake    time.Time
	windowStart time.Time
	count       int
	// cappedAt is when the hourly cap was reached, the zero time while it is
	// not in force.
	cappedAt time.Time
}

func newLimiter(maxPerMinute int, now func() time.Time) *limiter {
	return &limiter{
		now:     now,
		regs:    map[string]*regState{},
		rate:    float64(maxPerMinute) / 60,
		burst:   float64(maxPerMinute),
		tokens:  float64(maxPerMinute),
		updated: now(),
	}
}

// admit decides whether t gets a wake now: "" when it does, else the outcome
// that counts why not. Only a wake that passes all three checks changes the
// registration's state and spends a token, so a wake that is refused costs
// nothing.
func (l *limiter) admit(t Target) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.regs[t.RegistrationID]
	if st == nil {
		if len(l.regs) >= MaxTracked {
			l.sweep(now)
			if len(l.regs) >= MaxTracked {
				return OutcomeCapped
			}
		}
		st = &regState{lastWake: t.LastWakeAt}
		l.regs[t.RegistrationID] = st
	}
	if !st.cappedAt.IsZero() && t.LastFetchAt.After(st.cappedAt) {
		// It fetched since the cap: released.
		st.cappedAt, st.count, st.windowStart = time.Time{}, 0, time.Time{}
	}
	if !st.cappedAt.IsZero() {
		return OutcomeCapped
	}
	fetched := !t.LastFetchAt.IsZero() && t.LastFetchAt.After(st.lastWake)
	if !fetched && !st.lastWake.IsZero() && now.Sub(st.lastWake) < CoalesceWindow {
		return OutcomeCoalesced
	}
	if st.windowStart.IsZero() || now.Sub(st.windowStart) >= time.Hour {
		st.windowStart, st.count = now, 0
	}
	if st.count >= HourlyCap {
		st.cappedAt = now
		return OutcomeCapped
	}
	if !l.take(now) {
		return OutcomeOverBudget
	}
	st.lastWake = now
	st.count++
	return ""
}

// take spends one token of the budget, refilling by the time since the last
// take. The caller holds the lock.
func (l *limiter) take(now time.Time) bool {
	if elapsed := now.Sub(l.updated).Seconds(); elapsed > 0 {
		l.tokens = min(l.burst, l.tokens+elapsed*l.rate)
	}
	l.updated = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// sweep forgets every registration whose last wake is over an hour old and
// that is not capped (a capped one waits for its fetch). The caller holds the
// lock.
func (l *limiter) sweep(now time.Time) {
	for id, st := range l.regs {
		if st.cappedAt.IsZero() && now.Sub(st.lastWake) >= time.Hour {
			delete(l.regs, id)
		}
	}
}

// sent notes that a wake to id went out at now, which is what coalescing
// counts from.
func (l *limiter) sent(id string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.regs[id]; st != nil && now.After(st.lastWake) {
		st.lastWake = now
	}
}

// forget drops the state of a registration that ended.
func (l *limiter) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.regs, id)
}
