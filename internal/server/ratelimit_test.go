package server

import (
	"math"
	"math/bits"
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

// newSteppingClock is a clock that is step later on every read.
func newSteppingClock(step time.Duration) func() time.Time {
	c := newManualClock()
	return func() time.Time {
		c.Advance(step)
		return c.Now()
	}
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
			// Of r concurrent refusals the 1st, 2nd, 4th, ... are rows: exactly
			// bits.Len(r) of them, however the goroutines interleave.
			if want := int64(bits.Len(uint(tt.callers - int(want)))); audits.Load() != want {
				t.Errorf("refusals to audit = %d, want %d", audits.Load(), want)
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
		{"in a window the 1st, 2nd, 4th and 8th are written, each with its number", []step{
			{"a", 0, true, 1},
			{"a", time.Second, true, 2},
			{"a", 2 * time.Second, false, 3},
			{"a", 3 * time.Second, true, 4},
			{"a", 4 * time.Second, false, 5},
			{"a", 5 * time.Second, false, 6},
			{"a", 6 * time.Second, false, 7},
			{"a", denialCoalesceWindow - time.Nanosecond, true, 8},
		}},
		{"a new window starts the count again", []step{
			{"a", 0, true, 1},
			{"a", time.Second, true, 2},
			{"a", 2 * time.Second, false, 3},
			{"a", denialCoalesceWindow, true, 1},
			{"a", denialCoalesceWindow + time.Second, true, 2},
			{"a", 10 * denialCoalesceWindow, true, 1},
		}},
		{"a different kind is its own first denial", []step{
			{"a", 0, true, 1},
			{"a", time.Second, true, 2},
			{"b", 2 * time.Second, true, 1},
			{"a", 3 * time.Second, false, 3},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c denialCoalescer
			for i, s := range tt.steps {
				write, count := c.admit(s.key, start.Add(s.at))
				if write != s.wantWrite || count != s.wantCount {
					t.Errorf("step %d: admit(%q, +%v) = %v, %d; want %v, %d", i, s.key, s.at, write, count, s.wantWrite, s.wantCount)
				}
			}
		})
	}

	// A flood costs about log2(n) rows, and the last row written says at
	// least half of how many there were, whenever the caller stops.
	t.Run("a flood of n writes log2(n)+1 rows and loses less than half the count", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{1, 2, 3, 49, 1000, 100000} {
			var c denialCoalescer
			rows, last := 0, 0
			for range n {
				if write, count := c.admit("a", start); write {
					rows++
					last = count
				}
			}
			if wantRows := bits.Len(uint(n)); rows != wantRows {
				t.Errorf("n=%d: %d rows, want %d", n, rows, wantRows)
			}
			if 2*last <= n {
				t.Errorf("n=%d: the last row says %d, less than half", n, last)
			}
		}
	})
}

func TestDenialCoalescerBounded(t *testing.T) {
	t.Parallel()
	var c denialCoalescer
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxDenialKeys + 10 {
		if write, _ := c.admit(string(rune('a'+i%26))+"-"+time.Duration(i).String(), at); !write {
			t.Fatalf("first denial of kind %d was not written", i)
		}
	}
	if n := len(c.entries); n > maxDenialKeys {
		t.Errorf("remembered %d kinds, bound is %d", n, maxDenialKeys)
	}
}

// Refusals are thinned out as denials are: within the audit window the 1st,
// 2nd, 4th, 8th, ... is the one to write a row for, with its number, so the
// log says how hard an agent pushed even if it never calls again.
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
	if want := []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512}; !reflect.DeepEqual(written, want) {
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
	for _, seconds := range []int{10_000_000_000, 1 << 62, math.MaxInt64, int(maxLimitWindow/time.Second) + 1} {
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
	if got := limitWindow(0); got != maxLimitWindow {
		t.Errorf("limitWindow(0) = %v: a window that is not positive must refuse, not allow", got)
	}
	if got := limitWindow(10); got != 10*time.Second {
		t.Errorf("limitWindow(10) = %v", got)
	}
}
