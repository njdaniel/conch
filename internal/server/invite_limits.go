package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Rate limits of the invite routes (issue #437, ADR-008 §4).
//
// Preview and redeem are the only routes that anyone may call without a
// credential and that can create something, so they are limited before any
// store work: per client address (clientaddr.go), and redemptions that
// succeed also server-wide, so a leaked open link cannot make accounts in
// bulk. Making invites is limited per principal, operators included.
//
// Each limit is a token bucket: Burst attempts at once, refilled evenly at
// Burst per Window. Failed and successful attempts both spend a token, except
// for the server-wide limit, which counts only redemptions that succeed. Like
// ratelimit.go the buckets are in memory only: a restart refills them, so the
// worst case is one extra burst per restart, a deliberate trade for the
// single-binary deployment.
//
// Past a limit the answer is 429 with Retry-After and rate_limited. Refusals
// are audited as invite_rate_limited, one row per limit and window thinned by
// writeNth, never with the address or a code. Invalid codes on redeem are
// audited as invite_redeem_refused the same way.

// Audit actions written by the invite routes. The store writes the rest
// (store/invites.go).
const (
	auditInviteRateLimited    = "invite_rate_limited"
	auditInviteRedeemRefused  = "invite_redeem_refused"
	inviteAnonymousAuditActor = "anonymous"
)

// inviteLimit is the policy of one token bucket.
type inviteLimit struct {
	// name is what the audit detail calls the limit.
	name   string
	burst  int
	window time.Duration
}

// rate is the refill in tokens per second.
func (l inviteLimit) rate() float64 { return float64(l.burst) / l.window.Seconds() }

// inviteLimits are the four limits. Tests replace them with
// Server.setInviteLimits.
type inviteLimits struct {
	previewPerAddress  inviteLimit
	redeemPerAddress   inviteLimit
	redeemGlobal       inviteLimit
	createPerPrincipal inviteLimit
}

// The recommended defaults of issue #437.
const (
	defaultPreviewPerAddress  = 30
	defaultRedeemPerAddress   = 10
	defaultAddressWindow      = 10 * time.Minute
	defaultRedeemGlobal       = 20
	defaultRedeemGlobalWindow = time.Minute
	defaultCreatePerPrincipal = 20
	defaultCreateWindow       = time.Hour

	// maxInviteLimitKeys bounds how many addresses (or principals) one limit
	// keeps a bucket for. Past it, until the next prune drops idle buckets, a
	// new key shares the overflow bucket its caller names: for an address,
	// the one of its wide network (overflowKey). Memory stays bounded however
	// many addresses an attacker has, and one who fills the table from a
	// single network (an IPv6 /48 holds 65,536 /64s) drains only the overflow
	// bucket of that network, not every newcomer's.
	maxInviteLimitKeys = 1 << 16
	// maxInviteOverflowKeys bounds the overflow buckets in turn. Past it,
	// every new key shares the one inviteOverflowKey bucket: reaching it
	// takes addresses in that many distinct wide networks at once.
	maxInviteOverflowKeys = 1 << 12
	// inviteOverflowKey is the overflow bucket, and the prefix of every
	// per-network one. It cannot be an address or a principal id.
	inviteOverflowKey = "overflow"
	// inviteLimitPruneEvery is how often full (idle) buckets are dropped.
	inviteLimitPruneEvery = time.Minute
)

func defaultInviteLimits() inviteLimits {
	return inviteLimits{
		previewPerAddress:  inviteLimit{"preview_per_address", defaultPreviewPerAddress, defaultAddressWindow},
		redeemPerAddress:   inviteLimit{"redeem_per_address", defaultRedeemPerAddress, defaultAddressWindow},
		redeemGlobal:       inviteLimit{"redeem_server_wide", defaultRedeemGlobal, defaultRedeemGlobalWindow},
		createPerPrincipal: inviteLimit{"create_per_principal", defaultCreatePerPrincipal, defaultCreateWindow},
	}
}

// tokenBucket is one bucket. The zero value is full: a key seen for the first
// time has its whole burst.
type tokenBucket struct {
	// spent is how many tokens are missing from a full bucket at time at; a
	// bucket stores what it lacks so that the zero value is full.
	spent float64
	at    time.Time
}

// level is how many tokens are missing at now, after the refill since at. A
// clock that steps back refills nothing.
func (b *tokenBucket) level(l inviteLimit, now time.Time) float64 {
	if b.spent <= 0 {
		return 0
	}
	elapsed := now.Sub(b.at).Seconds()
	if elapsed <= 0 {
		return b.spent
	}
	return math.Max(0, b.spent-elapsed*l.rate())
}

// take spends one token if there is one. When there is not, it reports how
// long until there is, in whole seconds, at least one.
func (b *tokenBucket) take(l inviteLimit, now time.Time) (bool, time.Duration) {
	spent := b.level(l, now)
	if b.at.IsZero() || now.After(b.at) {
		b.at = now
	}
	b.spent = spent
	if spent+1 <= float64(l.burst) {
		b.spent++
		return true, 0
	}
	wait := (spent + 1 - float64(l.burst)) / l.rate()
	// Less a microsecond, so that float error on a whole number of seconds
	// does not round it up by one.
	return false, max(time.Duration(math.Ceil(wait-1e-6))*time.Second, time.Second)
}

// refund gives back one token taken by take.
func (b *tokenBucket) refund(l inviteLimit, now time.Time) {
	b.spent = math.Max(0, b.level(l, now)-1)
	if now.After(b.at) {
		b.at = now
	}
}

// bucketSet is the buckets of one limit, by key.
type bucketSet struct {
	buckets map[string]*tokenBucket
	pruned  time.Time
}

// bucket is key's bucket, made if need be. When the set is full, a new key
// gets the bucket of overflow instead.
func (s *bucketSet) bucket(l inviteLimit, key, overflow string, now time.Time) *tokenBucket {
	if s.buckets == nil {
		s.buckets = make(map[string]*tokenBucket)
	}
	if b := s.buckets[key]; b != nil {
		return b
	}
	// By the clock only, so a full table does not make every new key walk it.
	if now.Sub(s.pruned) >= inviteLimitPruneEvery {
		s.pruned = now
		for k, b := range s.buckets {
			if b.level(l, now) == 0 {
				delete(s.buckets, k)
			}
		}
	}
	if len(s.buckets) >= maxInviteLimitKeys {
		key = overflow
		if len(s.buckets) >= maxInviteLimitKeys+maxInviteOverflowKeys {
			key = inviteOverflowKey
		}
		if b := s.buckets[key]; b != nil {
			return b
		}
	}
	b := &tokenBucket{}
	s.buckets[key] = b
	return b
}

// inviteLimiter holds every invite limit's buckets and the thinning of their
// audit rows. One lock: every operation is a few arithmetic steps.
type inviteLimiter struct {
	mu                       sync.Mutex
	limits                   inviteLimits
	preview, redeem, creates bucketSet
	global                   tokenBucket
	// audits thins invite_rate_limited and invite_redeem_refused rows, by the
	// rule of denialCoalescer. Anonymous refusals are counted under principal
	// 0, which no principal has; the kinds are a fixed handful of strings, so
	// it cannot be grown by varying a request.
	audits denialCoalescer
}

func newInviteLimiter(l inviteLimits) *inviteLimiter { return &inviteLimiter{limits: l} }

// takeAddress spends one preview or redeem attempt of addr.
func (l *inviteLimiter) takeAddress(redeem bool, addr netip.Addr, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit, set := l.limits.previewPerAddress, &l.preview
	if redeem {
		limit, set = l.limits.redeemPerAddress, &l.redeem
	}
	ok, retry := set.bucket(limit, limitKey(addr), overflowKey(addr), now).take(limit, now)
	return limit, ok, retry
}

// takeGlobal reserves one server-wide redemption. A redemption that then
// fails gives it back with refundGlobal, so only those that succeed count.
func (l *inviteLimiter) takeGlobal(now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok, retry := l.global.take(l.limits.redeemGlobal, now)
	return l.limits.redeemGlobal, ok, retry
}

func (l *inviteLimiter) refundGlobal(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.global.refund(l.limits.redeemGlobal, now)
}

// takeCreate spends one invite creation of principalID.
func (l *inviteLimiter) takeCreate(principalID int64, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits.createPerPrincipal
	// Principals are made by operators and invites, both limited, so one
	// shared overflow bucket is enough here.
	ok, retry := l.creates.bucket(limit, fmt.Sprint(principalID), inviteOverflowKey, now).take(limit, now)
	return limit, ok, retry
}

// setInviteLimits replaces the limits and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setInviteLimits(l inviteLimits) { s.inviteLimits = newInviteLimiter(l) }

// refuseInviteRateLimited answers a request refused by limit with 429 and
// Retry-After, and audits it if it is one to write a row for. principalID is
// the caller, or 0 on an anonymous route; addr is the client, logged only in
// its truncated form and never audited.
func (s *Server) refuseInviteRateLimited(w http.ResponseWriter, r *http.Request, limit inviteLimit, retry time.Duration, principalID int64, addr netip.Addr) {
	now := s.now()
	if write, count := s.inviteLimits.audits.admit(principalID, auditInviteRateLimited+"|"+limit.name, now); write {
		actor := inviteAnonymousAuditActor
		if principalID != 0 {
			actor = fmt.Sprintf("principal:%d", principalID)
			slog.WarnContext(r.Context(), "invites: rate limit reached", "limit", limit.name, "principal", principalID)
		} else {
			slog.WarnContext(r.Context(), "invites: rate limit reached", "limit", limit.name, "network", logAddr(addr))
		}
		detail := fmt.Sprintf("limit=%s max=%d window_seconds=%d count=%d", limit.name, limit.burst, int(limit.window/time.Second), count)
		// The caller may already be gone; the refusal must still be recorded.
		actx := context.WithoutCancel(r.Context())
		if _, err := s.store.AppendAuditEvent(actx, actor, auditInviteRateLimited, inviteAuditSubject(r), detail); err != nil {
			slog.ErrorContext(actx, "invites: audit invite_rate_limited failed", "error", err)
		}
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
}

// auditRedeemRefused records, thinned, that a well-formed code redeemed
// nothing. The detail is the kind of refusal, never the code.
func (s *Server) auditRedeemRefused(r *http.Request, reason string) {
	s.auditCodeRefused(r, auditInviteRedeemRefused, reason)
}

// auditCodeRefused writes action, thinned, for a well-formed code an
// anonymous route refused: an invite's, or a sign-in link's (issue #493).
func (s *Server) auditCodeRefused(r *http.Request, action, reason string) {
	write, count := s.inviteLimits.audits.admit(0, action+"|"+reason, s.now())
	if !write {
		return
	}
	actx := context.WithoutCancel(r.Context())
	if _, err := s.store.AppendAuditEvent(actx, inviteAnonymousAuditActor, action, inviteAuditSubject(r),
		fmt.Sprintf("reason=%s count=%d", reason, count)); err != nil {
		slog.ErrorContext(actx, "invites: audit refused code failed", "action", action, "error", err)
	}
}

// inviteAuditSubject is the route pattern, never the request path.
func inviteAuditSubject(r *http.Request) string {
	if r.Pattern == "" {
		return "<unmatched>"
	}
	return r.Pattern
}
