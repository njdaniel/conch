package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Per-principal agent rate limits (issue #80).
//
// An agent's manifest may declare, per capability, "at most Max uses in any
// window of WindowSeconds" (schema.RateLimit). This file counts uses and
// answers. It is called from the places authorization is decided, after the
// capability, membership and channel checks have passed, so a call those
// checks refuse never spends budget:
//
//   - MCP: agentScope.channel (after both gates) and authorizeScopedPost.
//   - REST and WebSocket: agentCallerAllowed and authorizeScopedPost.
//   - Webhooks: handleIngestHook, for a hook bound to an agent.
//
// Whatever checks an agent's manifest (agentManifestCheck) must go on to spend
// (spendAgentBudget); a new entry point that does one and not the other is a
// way round the limit.
//
// There is one budget per (principal, capability). Humans have none: only
// callers that reach these functions as agents are counted.
//
// The counters are in memory and nothing is persisted. A restart gives every
// agent a fresh window, so the worst case is one extra Max per restart. That
// is a deliberate trade for the single-binary, no-migration deployment.

const (
	// agentLimitAuditWindow is the window in which the refusals of one
	// (principal, capability) are thinned out by the rule of writeNth, each
	// row carrying its number (see denialCoalescer for why the count does
	// not wait for a later row).
	agentLimitAuditWindow = time.Minute

	// thinAfter is how many identical events of one window are each written
	// before thinning starts; see writeNth.
	thinAfter = 8

	// maxLimitWindow is the longest window the limiter counts over. A
	// manifest may declare more (the schema only requires a positive number
	// of seconds); it is then held to this, which is long enough to mean
	// "never again" and short enough that no arithmetic on it can overflow.
	// Without the clamp a window_seconds past about 9.2e9 would wrap to a
	// negative duration, every use would count as expired, and the limit
	// would be declared, shown and never enforced. Fifty years, not more, so
	// that the delay in whole seconds also fits an int where int is 32 bits:
	// it goes out as Retry-After and as retry_after_seconds.
	maxLimitWindow = 50 * 365 * 24 * time.Hour

	// denialCoalesceWindow is the window in which repeated identical
	// access_denied events are thinned out (issue #80, comment from the
	// review of #89); see denialCoalescer.
	denialCoalesceWindow = time.Minute
	// maxDenialKinds is how many kinds of denial the coalescer counts
	// separately for one principal in a window. Past it that principal's
	// further kinds are counted together as one, so neither the memory nor
	// the log can be grown by varying what is asked for, and nothing one
	// principal does changes how another's denials are written.
	maxDenialKinds = 256
)

type agentLimitKey struct {
	principalID int64
	capability  schema.Capability
}

type agentLimitEntry struct {
	// uses are the admitted uses still inside the window, oldest first. At
	// most Max of them, so it is bounded by the policy.
	uses   []time.Time
	window time.Duration
	// refusedAt is when the current audit window of refusals began, and
	// refusals how many there have been in it; 0 means none is running.
	refusedAt time.Time
	refusals  int
}

// agentRateLimiter is the in-memory sliding-window counter. The zero value is
// ready to use. A sliding log (not a fixed bucket) is what "at most Max in any
// window" means: no burst of 2*Max across a bucket boundary.
type agentRateLimiter struct {
	mu      sync.Mutex
	entries map[agentLimitKey]*agentLimitEntry
	pruned  time.Time
}

type limitDecision struct {
	allowed bool
	// retryAfter is when the oldest use leaves the window, rounded up to
	// whole seconds, at least 1 and never more than the window.
	retryAfter time.Duration
	// audit says this refusal is one to write a row for; count is which
	// refusal it is in its audit window (1 for the first).
	audit bool
	count int
}

// take spends one use of the budget if there is one. The clock is read by the
// caller, once, and passed as at.
func (l *agentRateLimiter) take(key agentLimitKey, limit schema.RateLimit, at time.Time) limitDecision {
	window := limitWindow(limit.WindowSeconds)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(at)
	if l.entries == nil {
		l.entries = make(map[agentLimitKey]*agentLimitEntry)
	}
	e := l.entries[key]
	if e == nil {
		e = &agentLimitEntry{}
		l.entries[key] = e
	}
	// The manifest is read afresh on every call, so a changed window applies
	// to the uses already counted.
	e.window = window
	n := 0
	for n < len(e.uses) && at.Sub(e.uses[n]) >= window {
		n++
	}
	e.uses = append(e.uses[:0], e.uses[n:]...)
	if len(e.uses) < limit.Max {
		// Callers read the clock before taking the lock, so two of them can
		// arrive here in the other order, and a clock can step back. The use
		// goes in at its own place: the uses stay oldest first, which the
		// expiry above relies on.
		i := len(e.uses)
		for i > 0 && at.Before(e.uses[i-1]) {
			i--
		}
		e.uses = slices.Insert(e.uses, i, at)
		return limitDecision{allowed: true}
	}
	// The next call is admitted once fewer than Max uses are left in the
	// window, which is when this one leaves it: the oldest, unless the
	// manifest's Max was lowered under uses already counted. (The schema
	// refuses a Max that is not positive; if one got here every call is
	// refused, for the whole window.)
	wait := window
	if limit.Max > 0 {
		wait = window - at.Sub(e.uses[len(e.uses)-limit.Max])
	}
	retry := min(max(time.Duration(math.Ceil(wait.Seconds()))*time.Second, time.Second), window)
	if e.refusals == 0 || at.Sub(e.refusedAt) >= agentLimitAuditWindow {
		e.refusedAt, e.refusals = at, 0
	}
	e.refusals++
	return limitDecision{retryAfter: retry, audit: writeNth(e.refusals), count: e.refusals}
}

// limitWindow is the window a manifest's window_seconds stands for, held to
// maxLimitWindow.
func limitWindow(seconds int) time.Duration {
	if seconds <= 0 {
		// The schema refuses this; if it ever got here, refusing every call
		// past Max is the safe reading, not allowing every call.
		return maxLimitWindow
	}
	if int64(seconds) > int64(maxLimitWindow/time.Second) {
		return maxLimitWindow
	}
	return time.Duration(seconds) * time.Second
}

// writeNth reports whether the n-th identical audit event of a window is
// written: each of the first thinAfter, and after that only the 16th, 32nd,
// 64th, ... So what happens at an ordinary rate is recorded in full, exactly
// as it always was; a flood of n costs thinAfter plus about log2(n) rows; and
// the last row written always says more than half of how many there were,
// even if the caller then stops for good.
func writeNth(n int) bool { return n > 0 && (n <= thinAfter || n&(n-1) == 0) }

// prune drops entries that hold nothing: every use expired and no audit
// window running. At most once per audit window, so the map is bounded by the
// principals and capabilities seen in the last two.
func (l *agentRateLimiter) prune(now time.Time) {
	if now.Sub(l.pruned) < agentLimitAuditWindow {
		return
	}
	l.pruned = now
	for k, e := range l.entries {
		idle := len(e.uses) == 0 || now.Sub(e.uses[len(e.uses)-1]) >= e.window
		if idle && (e.refusals == 0 || now.Sub(e.refusedAt) >= agentLimitAuditWindow) {
			delete(l.entries, k)
		}
	}
}

// rateLimitedError is a call refused by a manifest rate limit. It carries how
// long to wait; authorizeScopedPost returns it as its error so the callers,
// which already branch on an error, can answer 429 instead of 500.
type rateLimitedError struct{ retryAfter time.Duration }

func (e *rateLimitedError) Error() string { return "agent rate limit exceeded" }

// seconds is the retry delay in whole seconds, at least 1.
func (e *rateLimitedError) seconds() int { return int(e.retryAfter / time.Second) }

// schemaError is the refusal on the wire. The delay is a field of its own, so
// an MCP caller, who gets no Retry-After header, need not read it out of the
// message (issue #228).
func (e *rateLimitedError) schemaError() *schema.Error {
	return &schema.Error{
		Code:              schema.ErrorCodeRateLimited,
		Message:           fmt.Sprintf("rate limit exceeded; retry in %d seconds", e.seconds()),
		RetryAfterSeconds: e.seconds(),
	}
}

// spendAgentBudget spends one use of capability from the agent's budget, if
// its manifest m declares a limit for it. It returns nil when the call may
// proceed (no limit declared, or budget left) and a *rateLimitedError when the
// limit is used up, after auditing the refusal if it is one to write a row for.
// The audit row carries the principal, the capability and the policy; no
// credential material is ever in scope here.
func (s *Server) spendAgentBudget(ctx context.Context, principalID int64, subject string, m schema.AgentManifestV1, capability schema.Capability) *rateLimitedError {
	limit, ok := m.RateLimitFor(capability)
	if !ok {
		return nil
	}
	d := s.limits.take(agentLimitKey{principalID, capability}, limit, s.now())
	if d.allowed {
		return nil
	}
	if d.audit {
		slog.WarnContext(ctx, "authz: agent rate limit exceeded", "principal", principalID, "capability", capability)
		detail := fmt.Sprintf("capability=%s max=%d window_seconds=%d count=%d", capability, limit.Max, limit.WindowSeconds, d.count)
		// The caller may already be gone; the refusal must still be recorded.
		actx := context.WithoutCancel(ctx)
		if _, err := s.store.AppendAuditEvent(actx, fmt.Sprintf("principal:%d", principalID), store.AuditAgentRateLimited, subject, detail); err != nil {
			slog.ErrorContext(actx, "authz: audit agent_rate_limited failed", "principal", principalID, "error", err)
		}
	}
	return &rateLimitedError{retryAfter: d.retryAfter}
}

// writeRateLimited answers a REST or WebSocket request refused by a rate
// limit: 429, Retry-After in whole seconds, and the standard error body.
func writeRateLimited(w http.ResponseWriter, e *rateLimitedError) {
	w.Header().Set("Retry-After", strconv.Itoa(e.seconds()))
	writeJSON(w, http.StatusTooManyRequests, e.schemaError())
}

// denialCoalescer keeps repeated identical access_denied events from growing
// the audit log without bound (issue #80): without it one authenticated
// principal could add a row with every refused request. The requests
// themselves are not limited. The zero value is ready to use.
//
// Within a window identical denials are written by the rule of writeNth: the
// first thinAfter exactly as they always were, then the 16th, 32nd, ... each
// carrying its number. Nothing is written later on a timer: a count that
// waited for "the next row" would be lost whenever the caller simply stopped,
// which is exactly when it matters.
type denialCoalescer struct {
	mu      sync.Mutex
	entries map[denialKey]*denialEntry
	// kinds is how many separate kinds each principal has an entry for.
	kinds  map[int64]int
	pruned time.Time
}

// denialKey is one principal's denials of one kind. The empty kind is the one
// its kinds past maxDenialKinds are counted under.
type denialKey struct {
	principalID int64
	kind        string
}

type denialEntry struct {
	// start is when the window began; count is how many identical denials
	// there have been in it, this one included.
	start time.Time
	count int
}

// admit reports whether this denial is to be written, and which one it is in
// its window (1 for the first). kind says what was denied and why; it is
// never the empty string.
func (c *denialCoalescer) admit(principalID int64, kind string, now time.Time) (write bool, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[denialKey]*denialEntry)
		c.kinds = make(map[int64]int)
	}
	if now.Sub(c.pruned) >= denialCoalesceWindow {
		c.pruned = now
		for k, e := range c.entries {
			if now.Sub(e.start) < denialCoalesceWindow {
				continue
			}
			delete(c.entries, k)
			if k.kind != "" {
				if c.kinds[k.principalID]--; c.kinds[k.principalID] <= 0 {
					delete(c.kinds, k.principalID)
				}
			}
		}
	}
	key := denialKey{principalID, kind}
	e := c.entries[key]
	if e == nil && c.kinds[principalID] >= maxDenialKinds {
		key.kind = ""
		e = c.entries[key]
	}
	if e == nil {
		if key.kind != "" {
			c.kinds[principalID]++
		}
		c.entries[key] = &denialEntry{start: now, count: 1}
		return true, 1
	}
	if now.Sub(e.start) >= denialCoalesceWindow {
		e.start, e.count = now, 1
		return true, 1
	}
	e.count++
	return writeNth(e.count), e.count
}

// denialDetail is the audit detail of the count-th identical denial in its
// window: until thinning starts a row is written exactly as it always was,
// and after that it carries its number.
func denialDetail(detail string, count int) string {
	if count <= thinAfter {
		return detail
	}
	return fmt.Sprintf("%s count=%d", detail, count)
}
