package server

import (
	"math"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// manualClock is a clock the tests move by hand: no test sleeps to cross a
// window.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestAgentRateLimiterWindow(t *testing.T) {
	t.Parallel()
	limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 3, WindowSeconds: 10}
	key := agentLimitKey{principalID: 1, capability: schema.CapabilityMessagesPost}

	type step struct {
		at        time.Duration // since the start
		wantAllow bool
		wantRetry time.Duration // checked only for a refusal
		wantAudit bool
		wantCount int // which refusal of its audit window; checked only for a refusal
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"max uses are admitted, the next is refused until the oldest leaves", []step{
			{0, true, 0, false, 0},
			{0, true, 0, false, 0},
			{0, true, 0, false, 0},
			{0, false, 10 * time.Second, true, 1},
			{9*time.Second + 500*time.Millisecond, false, time.Second, true, 2}, // 0.5s left, rounded up to 1s
			{10 * time.Second, true, 0, false, 0},                               // the boundary: exactly one window old has left
		}},
		{"just inside the window is still counted", []step{
			{0, true, 0, false, 0},
			{0, true, 0, false, 0},
			{0, true, 0, false, 0},
			{10*time.Second - time.Nanosecond, false, time.Second, true, 1},
		}},
		{"the window slides: no double burst across a boundary", []step{
			{0, true, 0, false, 0},
			{4 * time.Second, true, 0, false, 0},
			{8 * time.Second, true, 0, false, 0},
			{10 * time.Second, true, 0, false, 0},               // the use at 0 left
			{10 * time.Second, false, 4 * time.Second, true, 1}, // next to leave is the one at 4
			{14 * time.Second, true, 0, false, 0},               // the use at 4 left
			{14 * time.Second, false, 4 * time.Second, true, 2}, // next is the one at 8; the second refusal of the audit window
			{18 * time.Second, true, 0, false, 0},               // the use at 8 left
			{70 * time.Second, true, 0, false, 0},               // long after: all gone
			{70 * time.Second, true, 0, false, 0},
			{70 * time.Second, true, 0, false, 0},
			{70 * time.Second, false, 10 * time.Second, true, 1}, // a minute since the refusal at 10s: a new audit window, counted from 1
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var l agentRateLimiter
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i, s := range tt.steps {
				d := l.take(key, limit, start.Add(s.at))
				if d.allowed != s.wantAllow {
					t.Fatalf("step %d at %v: allowed = %v, want %v", i, s.at, d.allowed, s.wantAllow)
				}
				if s.wantAllow {
					continue
				}
				if d.retryAfter != s.wantRetry {
					t.Errorf("step %d at %v: retryAfter = %v, want %v", i, s.at, d.retryAfter, s.wantRetry)
				}
				if d.retryAfter < time.Second || d.retryAfter > 10*time.Second {
					t.Errorf("step %d: retryAfter %v outside [1s, window]", i, d.retryAfter)
				}
				if d.audit != s.wantAudit {
					t.Errorf("step %d at %v: audit = %v, want %v", i, s.at, d.audit, s.wantAudit)
				}
				if d.count != s.wantCount {
					t.Errorf("step %d at %v: count = %d, want %d", i, s.at, d.count, s.wantCount)
				}
			}
		})
	}
}

func TestAgentRateLimiterRetryAfterBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		window int
		wait   time.Duration // how far into the window the refusal is
		want   time.Duration
	}{
		{"a one-second window never advises less than 1s", 1, 999 * time.Millisecond, time.Second},
		{"never more than the window", 5, 0, 5 * time.Second},
		{"rounds up", 60, 30*time.Second + time.Millisecond, 30 * time.Second},
		{"whole seconds stay whole", 60, 30 * time.Second, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var l agentRateLimiter
			limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 1, WindowSeconds: tt.window}
			key := agentLimitKey{1, schema.CapabilityMessagesPost}
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if !l.take(key, limit, start).allowed {
				t.Fatal("first use refused")
			}
			d := l.take(key, limit, start.Add(tt.wait))
			if d.allowed {
				t.Fatal("second use admitted")
			}
			if d.retryAfter != tt.want {
				t.Errorf("retryAfter = %v, want %v", d.retryAfter, tt.want)
			}
		})
	}
}

func TestAgentRateLimiterIsolation(t *testing.T) {
	t.Parallel()
	var l agentRateLimiter
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	post := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 1, WindowSeconds: 60}
	approve := schema.RateLimit{Capability: schema.CapabilityApprovalsRequest, Max: 1, WindowSeconds: 60}
	steps := []struct {
		name  string
		key   agentLimitKey
		limit schema.RateLimit
		want  bool
	}{
		{"principal 1 posts", agentLimitKey{1, schema.CapabilityMessagesPost}, post, true},
		{"principal 1 posts again: exhausted", agentLimitKey{1, schema.CapabilityMessagesPost}, post, false},
		{"principal 2 is unaffected", agentLimitKey{2, schema.CapabilityMessagesPost}, post, true},
		{"principal 1 requests an approval: its own budget", agentLimitKey{1, schema.CapabilityApprovalsRequest}, approve, true},
		{"principal 1 reads: a different capability", agentLimitKey{1, schema.CapabilityMessagesRead}, schema.RateLimit{Capability: schema.CapabilityMessagesRead, Max: 1, WindowSeconds: 60}, true},
		{"principal 1 posts: still exhausted", agentLimitKey{1, schema.CapabilityMessagesPost}, post, false},
	}
	for _, s := range steps {
		if got := l.take(s.key, s.limit, at).allowed; got != s.want {
			t.Errorf("%s: allowed = %v, want %v", s.name, got, s.want)
		}
	}
}

// A burst of concurrent calls is admitted exactly Max times (run with -race).
func TestAgentRateLimiterConcurrentBurst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		max, callers int
	}{
		{"more callers than the budget", 25, 200},
		{"as many callers as the budget", 10, 10},
		{"one use", 1, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var l agentRateLimiter
			limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: tt.max, WindowSeconds: 3600}
			key := agentLimitKey{1, schema.CapabilityMessagesPost}
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var admitted, audits atomic.Int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range tt.callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					d := l.take(key, limit, at)
					if d.allowed {
						admitted.Add(1)
					}
					if d.audit {
						audits.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			want := int64(min(tt.max, tt.callers))
			if admitted.Load() != want {
				t.Errorf("admitted = %d, want exactly %d", admitted.Load(), want)
			}
			// Of r concurrent refusals the first eight and then the 16th,
			// 32nd, ... are rows, however the goroutines interleave.
			refused := tt.callers - int(want)
			wantRows := int64(min(refused, 8))
			for nth := 16; nth <= refused; nth *= 2 {
				wantRows++
			}
			if audits.Load() != wantRows {
				t.Errorf("refusals to audit = %d of %d, want %d", audits.Load(), refused, wantRows)
			}
		})
	}
}

func TestDenialCoalescer(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type step struct {
		key       string
		at        time.Duration
		wantWrite bool
		wantCount int
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"the first denial of a kind is written at once", []step{{"a", 0, true, 1}}},
		{"in a window the first eight are written, then only the 16th", []step{
			{"a", 0, true, 1}, {"a", time.Second, true, 2}, {"a", 2 * time.Second, true, 3}, {"a", 3 * time.Second, true, 4},
			{"a", 4 * time.Second, true, 5}, {"a", 5 * time.Second, true, 6}, {"a", 6 * time.Second, true, 7}, {"a", 7 * time.Second, true, 8},
			{"a", 8 * time.Second, false, 9}, {"a", 9 * time.Second, false, 10}, {"a", 10 * time.Second, false, 11}, {"a", 11 * time.Second, false, 12},
			{"a", 12 * time.Second, false, 13}, {"a", 13 * time.Second, false, 14}, {"a", 14 * time.Second, false, 15},
			{"a", denialCoalesceWindow - time.Nanosecond, true, 16},
		}},
		{"a new window starts the count again", []step{
			{"a", 0, true, 1},
			{"a", time.Second, true, 2},
			{"a", denialCoalesceWindow, true, 1},
			{"a", denialCoalesceWindow + time.Second, true, 2},
			{"a", 10 * denialCoalesceWindow, true, 1},
		}},
		{"a different kind has its own count", []step{
			{"a", 0, true, 1},
			{"a", time.Second, true, 2},
			{"b", 2 * time.Second, true, 1},
			{"a", 3 * time.Second, true, 3},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c denialCoalescer
			for i, s := range tt.steps {
				write, count := c.admit(1, s.key, start.Add(s.at))
				if write != s.wantWrite || count != s.wantCount {
					t.Errorf("step %d: admit(%q, +%v) = %v, %d; want %v, %d", i, s.key, s.at, write, count, s.wantWrite, s.wantCount)
				}
			}
		})
	}

	// A flood costs eight rows and then about log2(n) more, and the last row
	// written says more than half of how many there were, whenever the caller
	// stops. Until thinning starts every denial is a row.
	t.Run("a flood is thinned and loses less than half the count", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct{ n, rows int }{{1, 1}, {2, 2}, {3, 3}, {8, 8}, {9, 8}, {15, 8}, {16, 9}, {49, 10}, {1000, 14}, {100000, 21}} {
			var c denialCoalescer
			rows, last := 0, 0
			for range tt.n {
				if write, count := c.admit(1, "a", start); write {
					rows++
					last = count
				}
			}
			if rows != tt.rows {
				t.Errorf("n=%d: %d rows, want %d", tt.n, rows, tt.rows)
			}
			if 2*last <= tt.n {
				t.Errorf("n=%d: the last row says %d, not more than half", tt.n, last)
			}
		}
	})
}

// One principal's denials are counted separately for maxDenialKinds kinds a
// window. Its kinds past that are counted together, so it cannot grow the
// memory or the log by varying what it asks for; and none of it changes how
// another principal's denials are written.
func TestDenialCoalescerBounded(t *testing.T) {
	t.Parallel()
	var c denialCoalescer
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	kind := func(i int) string { return "kind-" + strconv.Itoa(i) }
	for i := range maxDenialKinds {
		if write, count := c.admit(1, kind(i), at); !write || count != 1 {
			t.Fatalf("first denial of kind %d: write=%v count=%d, want true, 1", i, write, count)
		}
	}
	// 300 more kinds, each new: together they are one flood.
	var written []int
	for i := range 300 {
		if write, count := c.admit(1, kind(maxDenialKinds+i), at); write {
			written = append(written, count)
		}
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 32, 64, 128, 256}; !reflect.DeepEqual(written, want) {
		t.Errorf("rows for 300 kinds past the bound = %v, want %v", written, want)
	}
	// A kind it already has an entry for is still counted on its own.
	if write, count := c.admit(1, kind(0), at); !write || count != 2 {
		t.Errorf("second denial of a tracked kind: write=%v count=%d, want true, 2", write, count)
	}
	// Another principal is not affected in any way.
	for n := 1; n <= 3; n++ {
		if write, count := c.admit(2, kind(maxDenialKinds+1), at); !write || count != n {
			t.Errorf("principal 2, denial %d: write=%v count=%d, want true, %d", n, write, count, n)
		}
	}
	if n := len(c.entries); n != maxDenialKinds+2 {
		t.Errorf("entries = %d, want %d (the kinds, principal 1's overflow, principal 2's one)", n, maxDenialKinds+2)
	}
	// A window later everything has gone and the principal starts afresh.
	later := at.Add(denialCoalesceWindow)
	if write, count := c.admit(1, kind(maxDenialKinds+5), later); !write || count != 1 {
		t.Errorf("a new kind in the next window: write=%v count=%d, want true, 1", write, count)
	}
	if n, k := len(c.entries), c.kinds[1]; n != 1 || k != 1 {
		t.Errorf("after the window: %d entries and %d kinds for principal 1, want 1 and 1", n, k)
	}
}

// Refusals are thinned out as denials are: within the audit window the first
// eight and then the 16th, 32nd, ... are the ones to write a row for, each
// with its number, so the log says how hard an agent pushed even if it never
// calls again.
func TestAgentRateLimiterRefusalRows(t *testing.T) {
	t.Parallel()
	limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 1, WindowSeconds: 3600}
	key := agentLimitKey{principalID: 1, capability: schema.CapabilityMessagesPost}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var l agentRateLimiter
	if d := l.take(key, limit, start); !d.allowed {
		t.Fatal("the first use was refused")
	}
	var written []int
	for i := range 1000 {
		d := l.take(key, limit, start.Add(time.Duration(i)*time.Millisecond))
		if d.allowed {
			t.Fatalf("refusal %d was admitted", i+1)
		}
		if d.audit {
			written = append(written, d.count)
		}
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 32, 64, 128, 256, 512}; !reflect.DeepEqual(written, want) {
		t.Errorf("rows written for 1000 refusals = %v, want %v", written, want)
	}
	// A minute after the first refusal the count starts again.
	if d := l.take(key, limit, start.Add(agentLimitAuditWindow)); !d.audit || d.count != 1 {
		t.Errorf("first refusal of the next audit window: audit=%v count=%d, want true, 1", d.audit, d.count)
	}
}

// A window_seconds too large for a time.Duration must not wrap round into a
// window that has always just ended: that would admit every call while the
// manifest still showed a limit. It is held to maxLimitWindow instead.
func TestAgentRateLimiterHugeWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, wide := range []int64{10_000_000_000, 1 << 62, math.MaxInt64, int64(maxLimitWindow/time.Second) + 1} {
		if wide > math.MaxInt {
			continue // where int is 32 bits a manifest cannot say this at all
		}
		seconds := int(wide)
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			t.Parallel()
			limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 2, WindowSeconds: seconds}
			if err := limit.Validate(); err != nil {
				t.Fatalf("the schema refuses this value (%v); the test no longer shows what it meant to", err)
			}
			key := agentLimitKey{principalID: 1, capability: schema.CapabilityMessagesPost}
			var l agentRateLimiter
			for i, at := range []time.Duration{0, time.Hour, 24 * time.Hour, 10 * 365 * 24 * time.Hour} {
				d := l.take(key, limit, start.Add(at))
				if wantAllow := i < 2; d.allowed != wantAllow {
					t.Fatalf("use %d at +%v: allowed = %v, want %v", i+1, at, d.allowed, wantAllow)
				}
				if !d.allowed && (d.retryAfter < time.Second || d.retryAfter > maxLimitWindow) {
					t.Errorf("use %d: retryAfter = %v, want within [1s, %v]", i+1, d.retryAfter, maxLimitWindow)
				}
			}
		})
	}
	// The longest delay there can be, in whole seconds, fits a 32-bit int: it
	// is sent as Retry-After and as retry_after_seconds, and must never wrap.
	if longest := int64(maxLimitWindow / time.Second); longest > math.MaxInt32 {
		t.Errorf("maxLimitWindow is %d seconds, more than a 32-bit int holds", longest)
	}
	if e := (&rateLimitedError{retryAfter: maxLimitWindow}); e.seconds() < 1 || e.schemaError().RetryAfterSeconds != e.seconds() {
		t.Errorf("the longest delay is sent as %d seconds (field %d)", e.seconds(), e.schemaError().RetryAfterSeconds)
	}
	if got := limitWindow(0); got != maxLimitWindow {
		t.Errorf("limitWindow(0) = %v: a window that is not positive must refuse, not allow", got)
	}
	if got := limitWindow(10); got != 10*time.Second {
		t.Errorf("limitWindow(10) = %v", got)
	}
}

// Two callers read the clock and then take the lock, so a use can arrive
// after a later one. It must still leave the window at its own time: if the
// later use were left in front of it, the earlier one would be counted until
// the later one expired.
func TestAgentRateLimiterUsesOutOfOrder(t *testing.T) {
	t.Parallel()
	limit := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 2, WindowSeconds: 10}
	key := agentLimitKey{principalID: 1, capability: schema.CapabilityMessagesPost}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var l agentRateLimiter
	for _, at := range []time.Duration{5 * time.Second, 3 * time.Second} {
		if d := l.take(key, limit, start.Add(at)); !d.allowed {
			t.Fatalf("use at +%v refused", at)
		}
	}
	if d := l.take(key, limit, start.Add(12*time.Second)); d.allowed {
		t.Fatal("a third use inside the window was admitted")
	} else if d.retryAfter != time.Second {
		t.Errorf("retryAfter = %v, want 1s: the use at +3s is the next to leave", d.retryAfter)
	}
	// The use made at +3s has left by +13.5s although it was recorded second.
	if d := l.take(key, limit, start.Add(13*time.Second+500*time.Millisecond)); !d.allowed {
		t.Error("the use at +3s was still counted after it left the window")
	}
	if d := l.take(key, limit, start.Add(14*time.Second)); d.allowed || d.retryAfter != time.Second {
		t.Errorf("at +14s: allowed=%v retryAfter=%v, want refused for 1s (the use at +5s leaves at +15s)", d.allowed, d.retryAfter)
	}
}

// When a manifest's Max is lowered under uses already counted, more than one
// of them has to leave before the next call is admitted, and Retry-After must
// say so: a caller who waits as told must then get in.
func TestAgentRateLimiterRetryAfterALoweredMax(t *testing.T) {
	t.Parallel()
	key := agentLimitKey{principalID: 1, capability: schema.CapabilityMessagesPost}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var l agentRateLimiter
	four := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 4, WindowSeconds: 10}
	for i := range 4 {
		if d := l.take(key, four, start.Add(time.Duration(i)*time.Second)); !d.allowed {
			t.Fatalf("use %d refused", i)
		}
	}
	two := schema.RateLimit{Capability: schema.CapabilityMessagesPost, Max: 2, WindowSeconds: 10}
	d := l.take(key, two, start.Add(4*time.Second))
	if d.allowed {
		t.Fatal("admitted with four uses counted against a limit of two")
	}
	// Uses at +0s, +1s and +2s must leave: the last of them at +12s.
	if d.retryAfter != 8*time.Second {
		t.Fatalf("retryAfter = %v, want 8s", d.retryAfter)
	}
	if early := l.take(key, two, start.Add(4*time.Second+d.retryAfter-time.Second)); early.allowed {
		t.Error("admitted a second before Retry-After was up")
	}
	if late := l.take(key, two, start.Add(4*time.Second+d.retryAfter)); !late.allowed {
		t.Error("refused after waiting exactly Retry-After")
	}
}
