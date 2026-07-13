// Package hub fans persisted messages out to per-channel subscribers. It is
// the realtime half of the P0 spike: the REST layer persists a message, then
// hands it to the hub, which delivers it to every live subscription on that
// channel.
package hub

import (
	"context"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// Hub routes broadcast messages to channel subscriptions.
//
// Slow-consumer policy: every subscription has a bounded buffer fixed at
// Subscribe time. A broadcast never blocks on a subscriber — when it finds a
// subscription's buffer full, the hub drops that subscription and closes its
// message channel instead of stalling the hub or queueing without bound. The
// subscriber observes the close and is expected to disconnect.
type Hub struct {
	mu     sync.Mutex
	closed bool
	subs   map[int64]map[*Subscription]struct{}
}

// Subscription is one subscriber's membership in a channel. Receive from
// Messages; call Cancel when done.
type Subscription struct {
	hub       *Hub
	channelID int64
	msgs      chan schema.MessageV0
}

// New returns an empty hub ready for subscriptions.
func New() *Hub {
	return &Hub{subs: make(map[int64]map[*Subscription]struct{})}
}

// Subscribe registers a subscription for messages broadcast to channelID from
// now on. buffer bounds the subscription's queue (see the slow-consumer
// policy on Hub). The hub closes the message channel when it drops the
// subscription — on overflow or hub Close; a subscription taken from a closed
// hub starts closed. Callers must Cancel the subscription when done.
func (h *Hub) Subscribe(channelID int64, buffer int) *Subscription {
	sub := &Subscription{hub: h, channelID: channelID, msgs: make(chan schema.MessageV0, buffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(sub.msgs)
		return sub
	}
	members := h.subs[channelID]
	if members == nil {
		members = make(map[*Subscription]struct{})
		h.subs[channelID] = members
	}
	members[sub] = struct{}{}
	return sub
}

// Messages yields every message broadcast to the subscription's channel. The
// hub closes it when the subscription is dropped (overflow, Cancel, or Close).
func (s *Subscription) Messages() <-chan schema.MessageV0 {
	return s.msgs
}

// Cancel unsubscribes. It is idempotent and safe to call after the hub has
// already dropped the subscription.
func (s *Subscription) Cancel() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.dropLocked(s)
}

// BroadcastMessage delivers msg to every subscription on msg.ChannelID,
// dropping any subscriber whose buffer is full. It implements the server's
// Broadcaster seam and never blocks.
func (h *Hub) BroadcastMessage(_ context.Context, msg schema.MessageV0) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[msg.ChannelID] {
		select {
		case sub.msgs <- msg:
		default:
			h.dropLocked(sub)
		}
	}
}

// Closed reports whether Close has been called, letting subscribers
// distinguish hub shutdown from a slow-consumer drop after their message
// channel closes.
func (h *Hub) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Close drops every subscription and marks the hub closed; subsequent
// broadcasts deliver to no one and subsequent Subscribes start closed.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, members := range h.subs {
		for sub := range members {
			close(sub.msgs)
		}
	}
	h.subs = make(map[int64]map[*Subscription]struct{})
}

// dropLocked removes sub from the hub and closes its channel exactly once;
// the map membership check is what makes Cancel/overflow/Close race-free.
// Callers must hold h.mu.
func (h *Hub) dropLocked(sub *Subscription) {
	members := h.subs[sub.channelID]
	if _, ok := members[sub]; !ok {
		return
	}
	delete(members, sub)
	if len(members) == 0 {
		delete(h.subs, sub.channelID)
	}
	close(sub.msgs)
}
