package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limits of the notification routes (issue #1713, design §4.5 and §5.7).
//
// People had no account limiter (ratelimit.go is the per-agent manifest
// limiter, and "humans have none"), so these routes get one on the pattern of
// channel_write_limit.go: a token bucket per key, in memory only, and a
// refusal that is 429 rate_limited with Retry-After, answered before the
// store is touched. A restart refills every bucket, so the worst case is one
// extra burst.
//
//   - Writes (PUT preferences, and the registration writes of #1714): one
//     bucket per principal, burst 10, refilling 10 per 10 minutes. A person
//     setting up a phone never meets it; a loop does at once.
//   - Reads (GET preferences, the feed, and later GET /v1/push/config and
//     /mine): one bucket per credential, burst 30, refilling 30 per minute.
//     A wake drains at most five pages of the feed (§4.3).
//
// The buckets are keyed by id (principal, credential), never by token or
// address. #1714 reuses this limiter for its routes.
const (
	notificationWriteBurst  = 10
	notificationWriteWindow = 10 * time.Minute
	notificationReadBurst   = 30
	notificationReadWindow  = time.Minute
)

func defaultNotificationWriteLimit() inviteLimit {
	return inviteLimit{"notification_writes", notificationWriteBurst, notificationWriteWindow}
}

func defaultNotificationReadLimit() inviteLimit {
	return inviteLimit{"notification_reads", notificationReadBurst, notificationReadWindow}
}

// notificationLimiter holds the write buckets by principal and the read
// buckets by credential.
type notificationLimiter struct {
	mu            sync.Mutex
	writeLimit    inviteLimit
	readLimit     inviteLimit
	writes, reads bucketSet
}

func newNotificationLimiter(write, read inviteLimit) *notificationLimiter {
	return &notificationLimiter{writeLimit: write, readLimit: read}
}

// takeWrite spends one write of principalID. The table is bounded by
// bucketSet: past its size new keys share the overflow bucket.
func (l *notificationLimiter) takeWrite(principalID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writes.bucket(l.writeLimit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.writeLimit, now)
}

// takeRead spends one read of credentialID.
func (l *notificationLimiter) takeRead(credentialID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads.bucket(l.readLimit, strconv.FormatInt(credentialID, 10), inviteOverflowKey, now).take(l.readLimit, now)
}

// setNotificationLimits replaces the limits and forgets every bucket. For
// tests, before the server serves.
func (s *Server) setNotificationLimits(write, read inviteLimit) {
	s.notificationLimits = newNotificationLimiter(write, read)
}

// relaxNotificationLimits lifts both bounds, for a test that makes more calls
// in one window than the bursts.
func (s *Server) relaxNotificationLimits() {
	relaxed := inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour}
	s.setNotificationLimits(relaxed, relaxed)
}

// takeNotificationWriteToken spends one write of principalID; when there is
// none it answers 429 and reports false.
func (s *Server) takeNotificationWriteToken(w http.ResponseWriter, principalID int64) bool {
	ok, retry := s.notificationLimits.takeWrite(principalID, s.now())
	if ok {
		return true
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
	return false
}

// takeNotificationReadToken spends one read of credentialID; when there is
// none it answers 429 and reports false.
func (s *Server) takeNotificationReadToken(w http.ResponseWriter, credentialID int64) bool {
	ok, retry := s.notificationLimits.takeRead(credentialID, s.now())
	if ok {
		return true
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
	return false
}
