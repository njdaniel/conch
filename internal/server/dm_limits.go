package server

import (
	"strconv"
	"sync"
	"time"
)

// The new-conversations bucket of direct messages (issue #2019, design §5):
// 20 a day per person, spent on every open of a pair the caller cannot yet
// see, refused attempts included, so probing for people costs tokens. A
// resume of a conversation the caller already sees spends nothing. The
// first message in a conversation also spends from this bucket. Sends, edits
// and removals each have their own per-person bucket.
const dmNewBurst = 20

func defaultDMNewLimit() inviteLimit {
	return inviteLimit{"dm_new_conversations", dmNewBurst, 24 * time.Hour}
}

// dmWriteLimiter holds the in-memory buckets for one kind of write, by
// principal. A restart refills them.
type dmWriteLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newDMWriteLimiter(l inviteLimit) *dmWriteLimiter { return &dmWriteLimiter{limit: l} }

// take spends one token for principalID or returns the wait time. Past the
// bucket table's size limit, new keys share the overflow bucket.
func (l *dmWriteLimiter) take(principalID int64, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok, retry := l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.limit, now)
	if !ok {
		return &rateLimitedError{retryAfter: retry}
	}
	return nil
}

// setDMNewLimit replaces the limit and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setDMNewLimit(l inviteLimit) { s.dmNewLimits = newDMWriteLimiter(l) }

// takeDMNewToken spends one new-conversation token or returns the wait time.
func (s *Server) takeDMNewToken(principalID int64, now time.Time) error {
	return s.dmNewLimits.take(principalID, now)
}
