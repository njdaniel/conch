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
	subsV1 map[int64]map[*SubscriptionV1]struct{}
	subsV2 map[int64]map[*SubscriptionV2]struct{}
}

// Subscription is one subscriber's membership in a channel. Receive from
// Messages; call Cancel when done.
type Subscription struct {
	hub         *Hub
	channelID   int64
	principalID int64
	msgs        chan schema.MessageV0
}

// SubscriptionV1 is a typed-envelope channel subscription.
type SubscriptionV1 struct {
	hub         *Hub
	channelID   int64
	principalID int64
	msgs        chan schema.MessageV1
}

// SubscriptionV2 is a subscription that understands scoped messages. It is
// the only kind that can be delivered one, and only when its principal is a
// recipient.
type SubscriptionV2 struct {
	hub         *Hub
	channelID   int64
	principalID int64
	msgs        chan schema.MessageV2
}

// New returns an empty hub ready for subscriptions.
func New() *Hub {
	return &Hub{
		subs:   make(map[int64]map[*Subscription]struct{}),
		subsV1: make(map[int64]map[*SubscriptionV1]struct{}),
		subsV2: make(map[int64]map[*SubscriptionV2]struct{}),
	}
}

// SubscribeV2 registers a MessageV2 subscription held by principalID (0 when
// the connection has no authenticated caller, which then receives
// channel-wide messages only).
func (h *Hub) SubscribeV2(channelID, principalID int64, buffer int) *SubscriptionV2 {
	sub := &SubscriptionV2{hub: h, channelID: channelID, principalID: principalID, msgs: make(chan schema.MessageV2, buffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(sub.msgs)
		return sub
	}
	members := h.subsV2[channelID]
	if members == nil {
		members = make(map[*SubscriptionV2]struct{})
		h.subsV2[channelID] = members
	}
	members[sub] = struct{}{}
	return sub
}

// Messages yields MessageV2 broadcasts.
func (s *SubscriptionV2) Messages() <-chan schema.MessageV2 { return s.msgs }

// Cancel unsubscribes the V2 subscription.
func (s *SubscriptionV2) Cancel() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.dropV2Locked(s)
}

// SubscribeV1 registers a MessageV1 subscription held by principalID (0 when
// the connection has no authenticated caller).
func (h *Hub) SubscribeV1(channelID, principalID int64, buffer int) *SubscriptionV1 {
	sub := &SubscriptionV1{hub: h, channelID: channelID, principalID: principalID, msgs: make(chan schema.MessageV1, buffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(sub.msgs)
		return sub
	}
	members := h.subsV1[channelID]
	if members == nil {
		members = make(map[*SubscriptionV1]struct{})
		h.subsV1[channelID] = members
	}
	members[sub] = struct{}{}
	return sub
}

// Messages yields MessageV1 broadcasts.
func (s *SubscriptionV1) Messages() <-chan schema.MessageV1 { return s.msgs }

// Cancel unsubscribes the V1 subscription.
func (s *SubscriptionV1) Cancel() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.dropV1Locked(s)
}

// Subscribe registers a subscription held by principalID (0 when the
// connection has no authenticated caller) for messages broadcast to channelID
// from now on. buffer bounds the subscription's queue (see the slow-consumer
// policy on Hub). The hub closes the message channel when it drops the
// subscription — on overflow or hub Close; a subscription taken from a closed
// hub starts closed. Callers must Cancel the subscription when done.
func (h *Hub) Subscribe(channelID, principalID int64, buffer int) *Subscription {
	sub := &Subscription{hub: h, channelID: channelID, principalID: principalID, msgs: make(chan schema.MessageV0, buffer)}
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

// BroadcastMessageV1 delivers a typed envelope to V1 subscriptions, and to V2
// subscriptions as the equivalent channel-wide v2 envelope. A MessageV1 cannot
// carry an audience, so it is always channel-wide; this is what lets every
// existing posting path (v0, v1, MCP, webhooks) reach v2 sockets unchanged.
// Scoped messages never come through here.
func (h *Hub) BroadcastMessageV1(_ context.Context, msg schema.MessageV1) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subsV1[msg.ChannelID] {
		select {
		case sub.msgs <- msg:
		default:
			h.dropV1Locked(sub)
		}
	}
	v2 := schema.MessageV2FromV1(msg)
	for sub := range h.subsV2[msg.ChannelID] {
		select {
		case sub.msgs <- v2:
		default:
			h.dropV2Locked(sub)
		}
	}
}

// BroadcastMessageV2 delivers msg to V2 subscriptions only. recipients is the
// set of principal ids resolved when the message was posted. A message with an
// audience, or with any recipients, is scoped: it goes only to a subscription
// whose principal id is in recipients, so an unidentified subscription
// (principal 0), a v0 or v1 subscription, and every principal outside the
// audience receive nothing, not a redacted placeholder. A scoped message with
// no recipients is delivered to no one (fail closed). Only a message with
// neither an audience nor recipients is channel-wide. The recipient list is
// used for the decision and is never put in a frame.
func (h *Hub) BroadcastMessageV2(_ context.Context, msg schema.MessageV2, recipients []int64) {
	scoped := msg.Audience != nil || len(recipients) > 0
	var allowed map[int64]struct{}
	if scoped {
		allowed = make(map[int64]struct{}, len(recipients))
		for _, r := range recipients {
			if r > 0 {
				allowed[r] = struct{}{}
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subsV2[msg.ChannelID] {
		if scoped {
			if _, ok := allowed[sub.principalID]; !ok {
				continue
			}
		}
		select {
		case sub.msgs <- msg:
		default:
			h.dropV2Locked(sub)
		}
	}
}

// DropPrincipal closes every subscription, v0, v1 and v2, that principalID holds
// on channelID, and reports how many it closed. Subscribers observe the close
// exactly as for a slow-consumer drop. Because the drop and every broadcast
// serialize on the hub lock, a message broadcast after DropPrincipal returns
// is never delivered to a dropped subscription. A zero principalID (no
// caller) matches nothing: unauthenticated subscriptions are never targeted.
func (h *Hub) DropPrincipal(channelID, principalID int64) int {
	if principalID == 0 {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for sub := range h.subs[channelID] {
		if sub.principalID == principalID {
			h.dropLocked(sub)
			n++
		}
	}
	for sub := range h.subsV1[channelID] {
		if sub.principalID == principalID {
			h.dropV1Locked(sub)
			n++
		}
	}
	for sub := range h.subsV2[channelID] {
		if sub.principalID == principalID {
			h.dropV2Locked(sub)
			n++
		}
	}
	return n
}

// DropPrincipalAll closes every subscription, v0, v1 and v2, that principalID holds
// on any channel, and reports how many it closed. It is the hub half of
// disabling a principal or revoking all its credentials. As with
// DropPrincipal, a message broadcast after it returns is never delivered to a
// dropped subscription, and a zero principalID (no caller) is a no-op.
func (h *Hub) DropPrincipalAll(principalID int64) int {
	if principalID == 0 {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, members := range h.subs {
		for sub := range members {
			if sub.principalID == principalID {
				h.dropLocked(sub)
				n++
			}
		}
	}
	for _, members := range h.subsV1 {
		for sub := range members {
			if sub.principalID == principalID {
				h.dropV1Locked(sub)
				n++
			}
		}
	}
	for _, members := range h.subsV2 {
		for sub := range members {
			if sub.principalID == principalID {
				h.dropV2Locked(sub)
				n++
			}
		}
	}
	return n
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
	for _, members := range h.subsV1 {
		for sub := range members {
			close(sub.msgs)
		}
	}
	for _, members := range h.subsV2 {
		for sub := range members {
			close(sub.msgs)
		}
	}
	h.subs = make(map[int64]map[*Subscription]struct{})
	h.subsV1 = make(map[int64]map[*SubscriptionV1]struct{})
	h.subsV2 = make(map[int64]map[*SubscriptionV2]struct{})
}

func (h *Hub) dropV2Locked(sub *SubscriptionV2) {
	members := h.subsV2[sub.channelID]
	if _, ok := members[sub]; !ok {
		return
	}
	delete(members, sub)
	if len(members) == 0 {
		delete(h.subsV2, sub.channelID)
	}
	close(sub.msgs)
}

func (h *Hub) dropV1Locked(sub *SubscriptionV1) {
	members := h.subsV1[sub.channelID]
	if _, ok := members[sub]; !ok {
		return
	}
	delete(members, sub)
	if len(members) == 0 {
		delete(h.subsV1, sub.channelID)
	}
	close(sub.msgs)
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
