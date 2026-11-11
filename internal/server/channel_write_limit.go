package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Bound on a channel's name and topic changes (issue #1667, from the
// security review of #1531).
//
// Each change writes an audit row and makes every member's client re-read its
// channel list (channels_changed). Rename has a daily cap per person (the
// store's), but it is per day and across channels, and the topic had no bound
// at all, so a manager in a loop, or a buggy script, could fill the audit log
// and churn every member's client. One token bucket per (principal, channel)
// is shared by PUT /v1/channels/{id}/name and PUT /v1/channels/{id}/topic: a
// burst of channelWriteBurst, then one change per channelWriteRefill. A
// person fixing a typo or setting up a new room never meets it; a loop
// does at once.
//
// A refusal is 429 rate_limited with Retry-After, answered before the store is
// touched: it writes no audit row and sends no channels_changed. The token is
// spent after the body is read and valid, so a malformed request spends
// nothing; a same-string no-op does spend one. In memory only, like every
// limit here: a restart refills it, so the worst case is one extra burst.
const (
	channelWriteBurst  = 5
	channelWriteRefill = 10 * time.Second
)

func defaultChannelWriteLimit() inviteLimit {
	return inviteLimit{"channel_name_topic_writes", channelWriteBurst, channelWriteBurst * channelWriteRefill}
}

// channelWriteLimiter holds the buckets, by principal and channel.
type channelWriteLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newChannelWriteLimiter(l inviteLimit) *channelWriteLimiter {
	return &channelWriteLimiter{limit: l}
}

// take spends one change of channelID by principalID. The table is bounded by
// bucketSet: past its size new keys share the overflow bucket.
func (l *channelWriteLimiter) take(principalID, channelID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buckets.bucket(l.limit, fmt.Sprintf("%d/%d", principalID, channelID), inviteOverflowKey, now).take(l.limit, now)
}

// setChannelWriteLimit replaces the limit and forgets every bucket. For
// tests, before the server serves.
func (s *Server) setChannelWriteLimit(l inviteLimit) {
	s.channelWriteLimits = newChannelWriteLimiter(l)
}

// relaxChannelWriteLimit lifts the bound for a test of some other limit that
// makes more changes in one window than the burst, such as the daily rename
// caps.
func (s *Server) relaxChannelWriteLimit() {
	s.setChannelWriteLimit(inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour})
}

// takeChannelWriteToken spends one name or topic change; when there is none
// it answers 429 and reports false.
func (s *Server) takeChannelWriteToken(w http.ResponseWriter, principalID, channelID int64) bool {
	ok, retry := s.channelWriteLimits.take(principalID, channelID, s.now())
	if ok {
		return true
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
	return false
}
