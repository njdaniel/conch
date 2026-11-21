package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The new-conversations bucket of direct messages (issue #2019, design §5):
// 20 a day per person, spent on every open of a pair the caller cannot yet
// see, refused attempts included, so probing for people costs tokens. A
// resume of a conversation the caller already sees spends nothing. The first
// message of a pair that had none (#2020) shares this bucket. In memory only,
// like every limit here: a restart refills it. A refusal is 429 rate_limited
// with Retry-After, answered before the store is touched.
const dmNewBurst = 20

func defaultDMNewLimit() inviteLimit {
	return inviteLimit{"dm_new_conversations", dmNewBurst, 24 * time.Hour}
}

// dmNewLimiter holds the buckets, by principal.
type dmNewLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newDMNewLimiter(l inviteLimit) *dmNewLimiter { return &dmNewLimiter{limit: l} }

// take spends one new conversation of principalID. The table is bounded by
// bucketSet: past its size new keys share the overflow bucket.
func (l *dmNewLimiter) take(principalID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.limit, now)
}

// setDMNewLimit replaces the limit and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setDMNewLimit(l inviteLimit) { s.dmNewLimits = newDMNewLimiter(l) }

// takeDMNewToken spends one new conversation; when there is none it answers
// 429 and reports false.
func (s *Server) takeDMNewToken(w http.ResponseWriter, principalID int64) bool {
	ok, retry := s.dmNewLimits.take(principalID, s.now())
	if ok {
		return true
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
	return false
}
