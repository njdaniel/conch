package tui

import (
	"context"
	"slices"
)

// subscriptions holds each channel's message socket by channel name. A socket
// is bound to the channel's id on the server but its callbacks carry the name
// it was opened under, so after a rename or a removal a late callback could
// land in whatever channel later reuses the name. Each socket therefore has its
// own context, to cancel it, and a generation that is never reused, to tell
// its late callbacks from the current socket's.
type subscriptions struct {
	seq    int
	byName map[string]subscription
}

type subscription struct {
	gen    int
	cancel context.CancelFunc
}

func newSubscriptions() *subscriptions {
	return &subscriptions{byName: make(map[string]subscription)}
}

// open starts a socket's context for channel, cancelling the one it replaces.
func (s *subscriptions) open(parent context.Context, channel string) (context.Context, int) {
	if old, ok := s.byName[channel]; ok {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.seq++
	s.byName[channel] = subscription{gen: s.seq, cancel: cancel}
	return ctx, s.seq
}

// live reports whether an event of generation gen belongs to channel's
// current socket. Generation 0 is no socket's (follow never makes it): an event
// built without one is taken as current.
func (s *subscriptions) live(channel string, gen int) bool {
	return gen == 0 || gen == s.byName[channel].gen
}

// stop cancels channel's socket. Its late callbacks no longer match.
func (s *subscriptions) stop(channel string) {
	if old, ok := s.byName[channel]; ok {
		old.cancel()
		delete(s.byName, channel)
	}
}

// forgetChannel tears channel down completely: its socket is cancelled and
// every entry kept under its name is dropped, so a channel that later takes the
// name starts fresh, with its history read again from the server.
func (m *Model) forgetChannel(channel string) {
	m.subs.stop(channel)
	delete(m.subscribed, channel)
	delete(m.messages, channel)
	delete(m.loaded, channel)
	delete(m.streamed, channel)
	delete(m.retryPending, channel)
	delete(m.backoff, channel)
	delete(m.nets, channel)
	delete(m.members, channel)
	m.forgetAuthors(channel)
	// A load in flight for the old channel must not be taken for the new one's.
	m.memberRevision[channel]++
	delete(m.rs.unread, channel)
	delete(m.rs.marked, channel)
	delete(m.rs.acked, channel)
	delete(m.rs.markedAt, channel)
	delete(m.rs.pending, channel)
	delete(m.rs.timer, channel)
	delete(m.rs.dropped, channel)
}

// forgetMissing tears down every name of old that next no longer holds.
func (m *Model) forgetMissing(old, next []string) {
	for _, name := range old {
		if !slices.Contains(next, name) {
			m.forgetChannel(name)
		}
	}
}
