package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The events socket (issue #269, P3 epic #263, decision D4).
//
// GET /v1/events/ws?channel=<name>[&session=<id>] sends, as JSON text frames,
// the documents a client needs to follow a channel's agents live: the roster
// of its members' sessions on connect and whenever it changes, the member
// list whenever it changes (issue #515), the channel itself when it is
// renamed (issue #727; never on connect), the runs, activity and interventions
// of runs in the channel that the reader may see, and the reader's unread state
// wherever a post or shared read mark changes it. Each frame is one pkg/schema
// document, told apart by its schema field.
//
// A human socket is also sent conch.channels_changed.v1 (issue #1027,
// docs/design/membership-live.md) when its principal's own answer to
// GET /v1/channels may have changed: they were added to or removed from a
// channel, given manages, created one, or a channel they are in was renamed.
// The frame names nothing: the client re-reads its channel list and counts.
// It goes to no one but that principal, never to an agent, and changes
// within the roster floor coalesce into one trailing frame per socket.
//
// This file holds no visibility rule and no SQL. A change commits, and the
// handler that made it enqueues a reference ("the roster changed", "run 41
// changed", "activity 7 of run 41") for every subscriber of the channel. The
// subscriber's own goroutine turns each reference into a document, at the
// moment it sends, with the reader of the request that opened the socket and
// the functions the REST routes use: channelRoster for the roster,
// RunVisible, ListVisibleActivity and InterventionVisible for the rest. A
// document the reader could not fetch over REST then is not sent. A writer
// never reads on a subscriber's behalf and never waits for one.
//
// Like /v2/ws, an agent's budget is spent once, at the upgrade (messages.read
// with read in the channel, which is what each of these reads asks), not per
// frame. The manifest is asked again, without spending, before every batch of
// frames and on the interval re-check, and a manifest written later also
// closes the socket at once through the same drop that closes /v2/ws.

const (
	// eventsQueueMax bounds the references waiting for one subscriber. A
	// subscriber that falls this far behind is closed (the slow-consumer
	// reason) rather than allowed to hold memory or slow a writer; it
	// reconnects and gets a fresh roster.
	eventsQueueMax = 256
	// eventsKeepAliveInterval is how often an events socket opened with
	// &session= refreshes that session, well inside the default stale
	// threshold.
	eventsKeepAliveInterval = 10 * time.Second
	// defaultEventsRosterFloor is the least time between two roster rebuilds
	// for one subscriber (Server.eventsRosterFloor). A roster is owed after any session or run change anywhere
	// (notifyRosters, notifyRun), and each rebuild reads the store, so without
	// a floor a principal could loop cheap writes and keep every subscriber's
	// goroutine reading. Owed rosters coalesce: one is sent when the floor
	// has passed, and it is the whole state then.
	defaultEventsRosterFloor = 250 * time.Millisecond
	// eventsReadStateFloor bounds whole read-state rebuilds per human socket.
	eventsReadStateFloor = 250 * time.Millisecond
)

type eventKind uint8

const (
	eventRun eventKind = iota + 1
	eventActivity
	eventIntervention
	// eventChannel is the channel itself (issue #727): today, its name.
	eventChannel
	// eventMessage is a newly committed message for the notification client.
	eventMessage
	// eventReactions is one message whose reactions changed (issue #765): id
	// is the message and channelID its channel.
	eventReactions
	// eventMessageEdited is one message whose text its author changed (issue
	// #845): id is the message and channelID its channel.
	eventMessageEdited
)

// eventRef names a document that changed. It carries no content.
type eventRef struct {
	kind      eventKind
	runID     int64 // the run an activity entry belongs to
	id        int64 // the run, activity entry, intervention, message or channel
	channelID int64 // the channel of a message
}

// eventsHub is the registry of events sockets.
type eventsHub struct {
	mu     sync.Mutex
	subs   map[*eventsSub]struct{}
	closed bool
}

func newEventsHub() *eventsHub {
	return &eventsHub{subs: make(map[*eventsSub]struct{})}
}

// eventsSub is one socket's subscription. Everything but the channels is
// guarded by hub.mu.
type eventsSub struct {
	hub          *eventsHub
	channelID    int64
	principalID  int64
	credentialID int64 // the credential the socket was opened with
	// wake has room for one signal: the writer only ever does a non-blocking
	// send, and the subscriber takes everything queued when it wakes.
	wake    chan struct{}
	dropped chan struct{}

	roster           bool // a roster is owed: one is enough, it is the whole state
	readState        bool // read-state changes coalesce into one refresh
	readStateAll     bool
	readStateDirty   map[int64]struct{}
	readStateRemoved map[int64]struct{}
	memberships      map[int64]struct{}
	membershipDirty  bool
	readStateEpoch   uint64
	human            bool
	// channelsChanged owes a human socket one conch.channels_changed.v1
	// (notifyChannelsChanged). It is a flag, not a queue: the frame carries
	// nothing, so any number of changes owe the same one.
	channelsChanged bool
	refs            []eventRef
	gone            bool
}

// subscribe registers a subscriber, owed its first roster: that roster goes
// out through flush like every other, so it is sent only after a drop that
// committed meanwhile has been seen and membership and the manifest have been
// asked again. After closeAll it returns one that is already dropped.
func (h *eventsHub) subscribe(channelID, principalID, credentialID int64) *eventsSub {
	return h.subscribeKind(channelID, principalID, credentialID, true)
}

func (h *eventsHub) subscribeKind(channelID, principalID, credentialID int64, human bool) *eventsSub {
	sub := &eventsSub{hub: h, channelID: channelID, principalID: principalID, credentialID: credentialID, wake: make(chan struct{}, 1), dropped: make(chan struct{}), roster: true, readState: human, readStateAll: human, readStateDirty: map[int64]struct{}{}, readStateRemoved: map[int64]struct{}{}, memberships: map[int64]struct{}{}, human: human, readStateEpoch: 1}
	sub.signal()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		sub.gone = true
		close(sub.dropped)
		return sub
	}
	h.subs[sub] = struct{}{}
	return sub
}

func (sub *eventsSub) cancel() {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	delete(sub.hub.subs, sub)
}

// take returns what is waiting and empties it, or live=false when the
// subscription was dropped.
func (sub *eventsSub) take() (roster, readState, channelsChanged bool, refs []eventRef, live bool) {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	if sub.gone {
		return false, false, false, nil, false
	}
	roster, readState, channelsChanged, refs = sub.roster, sub.readState, sub.channelsChanged, sub.refs
	sub.roster, sub.readState, sub.channelsChanged, sub.refs = false, false, false, nil
	return roster, readState, channelsChanged, refs, true
}

func (sub *eventsSub) signal() {
	select {
	case sub.wake <- struct{}{}:
	default: // one signal is already queued
	}
}

func (sub *eventsSub) readStateVersion() uint64 {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	return sub.readStateEpoch
}

// invalidateReadState discards an in-flight build and owes a fresh one. The
// hub lock is already held by a membership or credential change.
func (sub *eventsSub) invalidateReadState() {
	sub.readStateEpoch++
	sub.readStateAll = true
	sub.readState = true
	sub.signal()
}

func (sub *eventsSub) setMemberships(channels []store.Channel) {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	sub.memberships = make(map[int64]struct{}, len(channels))
	for _, channel := range channels {
		sub.memberships[channel.ID] = struct{}{}
	}
}

func (sub *eventsSub) setMembershipsIfCurrent(channels []store.Channel, epoch uint64) bool {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	if sub.readStateEpoch != epoch {
		return false
	}
	sub.memberships = make(map[int64]struct{}, len(channels))
	for _, channel := range channels {
		sub.memberships[channel.ID] = struct{}{}
	}
	return true
}

func (sub *eventsSub) hasMembership(channelID int64) bool {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	_, ok := sub.memberships[channelID]
	return ok
}

func (sub *eventsSub) readStateTargets() (all bool, ids, removed []int64) {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	all = sub.readStateAll || sub.membershipDirty
	for id := range sub.readStateDirty {
		ids = append(ids, id)
	}
	for id := range sub.readStateRemoved {
		removed = append(removed, id)
	}
	sub.readStateAll = false
	sub.membershipDirty = false
	sub.readStateDirty = map[int64]struct{}{}
	sub.readStateRemoved = map[int64]struct{}{}
	return all, ids, removed
}

// push queues ref, unless the same document is already waiting (a run or an
// intervention is read whole, so one pending reference says it all). A full
// queue drops the subscriber. h.mu must be held.
func (sub *eventsSub) push(ref eventRef) {
	if ref.kind != eventActivity {
		for _, queued := range sub.refs {
			if queued == ref {
				sub.signal()
				return
			}
		}
	}
	if len(sub.refs) >= eventsQueueMax {
		sub.hub.dropLocked(sub)
		return
	}
	sub.refs = append(sub.refs, ref)
	sub.signal()
}

func (h *eventsHub) dropLocked(sub *eventsSub) {
	delete(h.subs, sub)
	if !sub.gone {
		sub.gone = true
		sub.refs = nil
		close(sub.dropped)
	}
}

// notifyRosters tells every subscriber that rosters changed, for changes that
// do not say which channels they touch (a session's life, a run's status, a
// revocation). A subscriber rebuilds its roster and sends it only if it
// differs from the last it sent, so a channel the change did not touch hears
// nothing.
func (h *eventsHub) notifyRosters() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.roster = true
		sub.signal()
	}
}

// notifyChannelRoster is notifyRosters for a change that touches one channel:
// its membership.
func (h *eventsHub) notifyChannelRoster(channelID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.human {
			sub.membershipDirty = true
			sub.readState = true
		}
		if sub.channelID == channelID {
			sub.roster = true
		}
		sub.signal()
	}
}

// notifyChannelsChanged owes every human events socket of each of
// principalIDs one conch.channels_changed.v1 (issue #1027,
// membership-live.md §2): that principal's answer to GET /v1/channels may
// have changed. A handler calls it after the change commits, and only for a
// change that took effect. A zero id matches nothing, and an agent's socket
// is never owed it: an agent learns its memberships over MCP.
func (h *eventsHub) notifyChannelsChanged(principalIDs ...int64) {
	if len(principalIDs) == 0 {
		return
	}
	want := make(map[int64]struct{}, len(principalIDs))
	for _, id := range principalIDs {
		if id != 0 {
			want[id] = struct{}{}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if _, ok := want[sub.principalID]; ok && sub.human {
			sub.channelsChanged = true
			sub.signal()
		}
	}
}

// notifyRun is for a run created or changed in channelID: its subscribers are
// owed the run, and every roster may show a different status for its agent.
func (h *eventsHub) notifyRun(channelID, runID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.roster = true
		if sub.channelID == channelID {
			sub.push(eventRef{kind: eventRun, id: runID})
		} else {
			sub.signal()
		}
	}
}

// notifyActivity is for activity entry id appended to runID in channelID.
func (h *eventsHub) notifyActivity(channelID, runID, id int64) {
	h.notifyChannel(channelID, eventRef{kind: eventActivity, runID: runID, id: id})
}

// notifyIntervention is for intervention id created or answered in channelID.
func (h *eventsHub) notifyIntervention(channelID, id int64) {
	h.notifyChannel(channelID, eventRef{kind: eventIntervention, id: id})
}

func (h *eventsHub) notifyChannel(channelID int64, ref eventRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.channelID == channelID {
			sub.push(ref)
		}
	}
}

// notifyMessage queues a new message for each human who may read the channel,
// except its author. Visibility is checked again with the subscriber's reader.
func (h *eventsHub) notifyMessage(channelID, messageID, authorID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		_, member := sub.memberships[channelID]
		if sub.human && member && sub.principalID != authorID {
			sub.push(eventRef{kind: eventMessage, id: messageID, channelID: channelID})
		}
	}
}

// notifyReactions queues, for every subscriber of channelID, the re-reading of
// one message's reactions after a committed change to them (issue #765,
// message-reactions.md §8.1). Humans and agents alike, and the actor's own
// other sockets too. The reference names the message and nothing more: each
// subscriber's goroutine reads the summary, at the moment it sends, with its
// own reader and the explicit member-and-enabled clause, so a subscriber who
// cannot see the message, or has just been removed from the channel, is sent
// nothing. References for one message coalesce in a queue (push), so a burst of
// reactions costs one read when the socket drains.
func (h *eventsHub) notifyReactions(channelID, messageID int64) {
	h.notifyChannel(channelID, eventRef{kind: eventReactions, id: messageID, channelID: channelID})
}

// notifyMessageEdited queues an edited message for every human who holds a
// membership of channelID, the author's own other sockets included
// (docs/design/message-editing.md §7.2). Agents are not sent it, as they are
// not sent a new message. The reference names the message and nothing more:
// each subscriber's goroutine reads it, when it sends, with its own reader,
// and sends conch.message_edited.v1 or, if it may not see it, nothing.
// References for one message coalesce in the queue (push). An edit is not a
// new message: no read state is owed.
func (h *eventsHub) notifyMessageEdited(channelID, messageID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if _, member := sub.memberships[channelID]; sub.human && member {
			sub.push(eventRef{kind: eventMessageEdited, id: messageID, channelID: channelID})
		}
	}
}

// notifyReadState owes every other reader the count in a channel with a new
// message. The subscriber checks membership and scoped visibility when sending.
func (h *eventsHub) notifyReadState(channelID int64, authorID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		_, member := sub.memberships[channelID]
		if sub.human && member && sub.principalID != authorID {
			sub.readStateDirty[channelID] = struct{}{}
			sub.readState = true
			sub.signal()
		}
	}
}

// notifyReadMarked owes all of one reader's sockets their shared mark and count.
func (h *eventsHub) notifyReadMarked(channelID int64, principalID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		_, member := sub.memberships[channelID]
		if sub.human && member && sub.principalID == principalID {
			sub.readStateDirty[channelID] = struct{}{}
			sub.readState = true
			sub.signal()
		}
	}
}

// dropPrincipal closes principalID's events sockets on channelID. A zero
// principal matches nothing.
func (h *eventsHub) dropPrincipal(channelID, principalID int64) {
	if principalID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.principalID == principalID {
			delete(sub.memberships, channelID)
			sub.readStateDirty[channelID] = struct{}{}
			sub.readStateRemoved[channelID] = struct{}{}
			sub.invalidateReadState()
			if sub.channelID == channelID {
				h.dropLocked(sub)
			}
		}
	}
}

// dropPrincipalAll closes principalID's events sockets on every channel.
func (h *eventsHub) dropPrincipalAll(principalID int64) {
	if principalID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.principalID == principalID {
			sub.invalidateReadState()
			h.dropLocked(sub)
		}
	}
}

// dropCredential closes the events sockets opened with credentialID, on
// every channel: that one credential was revoked (issue #519). The
// principal's sockets through its other credentials stay. A zero credential
// matches nothing.
func (h *eventsHub) dropCredential(credentialID int64) {
	if credentialID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.credentialID == credentialID {
			h.dropLocked(sub)
		}
	}
}

// closeAll drops every subscription and refuses new ones: shutdown.
func (h *eventsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		h.dropLocked(sub)
	}
}

func (h *eventsHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// handleEventsWS serves GET /v1/events/ws?channel=<name>[&session=<id>]. The
// checks before the upgrade are the roster route's (a verified caller, the
// channel gate: a missing channel and one the caller is not a member of are
// the same 404, an agent needs messages.read with read in it), and with
// &session= the own-session check: the session must be the caller
// credential's and not ended, otherwise 404 session_not_found. Nothing is
// upgraded for a refusal.
//
// It follows ws.go: the subscription is registered before the last checks, so
// a removal that commits in between closes it.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel query parameter is required")
		return
	}
	channel, ok := s.channelForCaller(w, r, name, schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	var sessionID int64
	if raw := r.URL.Query().Get("session"); raw != "" {
		// An id that is not a positive integer names no session, so it is
		// the not-found answer, as on the session routes.
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeSessionNotFound(w)
			return
		}
		// The same check as the session routes: an agent needs sessions.manage
		// (and spends it, once, here; the keep-alive does not spend).
		if _, _, ok := s.ownSessionCaller(w, r); !ok {
			return
		}
		switch _, err := s.store.OwnedSession(ctx, id, credID); {
		case errors.Is(err, store.ErrNotFound):
			writeSessionNotFound(w)
			return
		case err != nil:
			slog.ErrorContext(ctx, "events ws: find session failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
			return
		}
		sessionID = id
	}

	// Subscribe before the last checks: DropPrincipal-style drops close only
	// what is registered, so a removal, a disable or a revoke of this
	// credential that committed after the checks above has either been seen
	// below or closes this subscription.
	sub := s.events.subscribeKind(channel.ID, caller.ID, credID, caller.Kind == store.PrincipalHuman)
	defer sub.cancel()
	memberships, err := s.store.ListChannelsForPrincipal(ctx, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "events ws: list memberships failed", "error", err)
		writeInternalError(w)
		return
	}
	sub.setMemberships(memberships)
	live, err := s.store.CredentialLive(ctx, credID)
	if err != nil {
		slog.ErrorContext(ctx, "events ws: check credential failed", "error", err)
		writeInternalError(w)
		return
	}
	if !live {
		writeUnauthenticated(w)
		return
	}
	member, err := s.store.IsChannelMember(ctx, channel.ID, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "events ws: check membership failed", "error", err)
		writeInternalError(w)
		return
	}
	if !member {
		writeChannelNotFound(w)
		return
	}

	conn, ctx, done, ok := s.acceptSocket(w, r)
	if !ok {
		return
	}
	defer done()
	ticker := time.NewTicker(s.credRecheckInterval)
	defer ticker.Stop()
	es := &eventsStream{s: s, r: r, conn: conn, sub: sub, channel: channel, principalID: caller.ID, credID: credID, sessionID: sessionID}
	es.run(ctx, ticker.C)
}

// eventsStream is one open events socket.
type eventsStream struct {
	s           *Server
	r           *http.Request // the request that opened the socket; its reader is the subscriber's
	conn        *websocket.Conn
	sub         *eventsSub
	channel     store.Channel
	principalID int64
	credID      int64
	sessionID   int64  // the session to keep alive; 0 for none
	lastRoster  []byte // the roster frame last sent
	// lastMembers is the member list frame last sent, or, before any was,
	// the list as it stood before the first roster was written: that one is
	// not sent.
	lastMembers []byte
	// rosterBuilt is when a roster was last built, for the roster floor;
	// rosterLater fires when one owed meanwhile may be built, and is nil
	// while none is owed.
	rosterBuilt time.Time
	rosterLater <-chan time.Time
	// channelsLater fires when an owed conch.channels_changed.v1 goes, one
	// roster floor after the first change that owed it; nil while none is
	// owed (trailing-edge coalescing, membership-live.md §3).
	channelsLater    <-chan time.Time
	readStateBuilt   time.Time
	readStateLater   <-chan time.Time
	lastReadStates   map[int64][]byte
	readStateWriting uint64
}

// run sends frames until the peer leaves, the subscription is dropped, or the
// credential or membership lapses.
func (e *eventsStream) run(ctx context.Context, recheck <-chan time.Time) {
	ctx = e.s.keepAlive(ctx, e.conn, true)
	var keepAlive <-chan time.Time
	if e.sessionID != 0 {
		ticks, stop := e.s.sessionKeepAliveTicks()
		defer stop()
		keepAlive = ticks
	}
	// The first roster is owed by subscribe and goes out on the first wake,
	// through flush. A change after the subscription is queued behind it.
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, e.conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !e.stillAllowed(ctx) {
				return
			}
		case <-keepAlive:
			if e.s.keepSessionAlive(ctx, e.sessionID, e.credID) == sessionGone {
				// A revoke or a disable ends the session too; when its
				// drop has come, the close says why the credential went,
				// not that the session did.
				select {
				case <-e.sub.dropped:
					e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
					return
				default:
				}
				// The session ended (DELETE, or the sweeper ending an
				// expired credential's): nothing is left to keep alive.
				// Closing tells the client, which reconnects and is
				// refused 404 session_not_found.
				closeWS(ctx, e.conn, schema.SocketCloseSessionEnded, websocket.StatusPolicyViolation, "session ended")
				return
			}
		case <-e.sub.wake:
			if !e.flush(ctx) {
				return
			}
		case <-e.rosterLater:
			e.rosterLater = nil
			// select picks at random among ready cases: a drop that is
			// ready too (a disabled member keeps their place on the list,
			// so stillMember passes) wins, and nothing more is built.
			select {
			case <-e.sub.dropped:
				e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
				return
			default:
			}
			if !e.stillMember(ctx) || !e.sendRoster(ctx) {
				return
			}
		case <-e.readStateLater:
			e.readStateLater = nil
			select {
			case <-e.sub.dropped:
				e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
				return
			default:
			}
			if !e.stillMember(ctx) || !e.sendReadStates(ctx) {
				return
			}
		case <-e.channelsLater:
			e.channelsLater = nil
			// As for the roster: a drop that is ready too wins, and the
			// socket closes with its reason and no frame.
			select {
			case <-e.sub.dropped:
				e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
				return
			default:
			}
			if !e.stillMember(ctx) || !e.sendDoc(ctx, schema.ChannelsChangedV1{Schema: schema.ChannelsChangedSchemaV1}) {
				return
			}
		case <-e.sub.dropped:
			e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
			return
		}
	}
}

// sessionKeepAliveTicks starts the keep-alive ticker.
func (s *Server) sessionKeepAliveTicks() (<-chan time.Time, func()) {
	if s.keepAliveTicks != nil {
		return s.keepAliveTicks()
	}
	t := time.NewTicker(eventsKeepAliveInterval)
	return t.C, t.Stop
}

// keepSessionAlive refreshes session id through the heartbeat's own store call,
// so a stale or offline session is revived the same way and held to the same
// bound. It reports sessionGone once the session is gone (ended, or not the
// credential's): the store does not revive an ended session. The refresh is a
// heartbeat the socket sends for its client, not a call by the agent, so it
// does not spend the manifest's budget again.
func (s *Server) keepSessionAlive(ctx context.Context, id, credID int64) keepAliveResult {
	_, err := s.heartbeatStored(ctx, id, credID)
	switch {
	case err == nil:
		return sessionKept
	case errors.Is(err, store.ErrNotFound):
		return sessionGone
	case errors.Is(err, store.ErrTooManySessions):
		// Offline, and the credential holds as many live sessions as it may.
		// It stays offline; the next tick tries again.
		return sessionKept
	case ctx.Err() == nil:
		slog.ErrorContext(ctx, "events ws: keep session alive failed", "session", id, "error", err)
	}
	return sessionKept
}

// keepAliveResult is what a keep-alive tick found.
type keepAliveResult int

const (
	// sessionKept: refreshed, or not refreshable now; the next tick tries again.
	sessionKept keepAliveResult = iota
	// sessionGone: ended, or not the credential's; it will not come back.
	sessionGone
)

// stillAllowed is the interval re-check: the credential is live, the
// subscriber is still a member and, for an agent, the manifest still lets it
// read the channel. It has closed the socket when it reports false.
func (e *eventsStream) stillAllowed(ctx context.Context) bool {
	if !e.s.credentialStillValid(ctx, e.conn, e.credID) {
		return false
	}
	return e.stillMember(ctx)
}

// stillMember is the membership and manifest check made before every batch of
// frames and on the interval: a socket sends nothing to someone who could not
// fetch the channel over REST now. It closes the socket and reports false when
// they could not, failing closed on a store error.
func (e *eventsStream) stillMember(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	member, err := e.s.store.IsChannelMember(cctx, e.channel.ID, e.principalID)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "events ws: recheck membership failed", "error", err)
		}
		closeWS(ctx, e.conn, schema.SocketCloseError, websocket.StatusInternalError, "membership check failed")
		return false
	case !member:
		closeWS(ctx, e.conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "no longer a member of this channel")
		return false
	case e.s.agentMayNoLongerRead(cctx, e.principalID, e.channel.ID):
		closeWS(ctx, e.conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "manifest no longer permits reading this channel")
		return false
	}
	return true
}

// flush sends what is waiting. It reports false when the socket is closed.
func (e *eventsStream) flush(ctx context.Context) bool {
	roster, readState, channelsChanged, refs, live := e.sub.take()
	if !live {
		e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
		return false
	}
	if !e.stillMember(ctx) {
		return false
	}
	if roster && !e.rosterNowOrLater(ctx) {
		return false
	}
	if readState && !e.readStateNowOrLater(ctx) {
		return false
	}
	if channelsChanged && e.channelsLater == nil {
		e.channelsLater = time.After(e.s.eventsRosterFloor)
	}
	for _, ref := range refs {
		if !e.sendRef(ctx, ref) {
			return false
		}
	}
	return true
}

func (e *eventsStream) readStateNowOrLater(ctx context.Context) bool {
	if e.readStateLater != nil {
		return true
	}
	if wait := eventsReadStateFloor - time.Since(e.readStateBuilt); !e.readStateBuilt.IsZero() && wait > 0 {
		e.readStateLater = time.After(wait)
		return true
	}
	return e.sendReadStates(ctx)
}

// sendReadStates refreshes only dirty channels, or every current membership on
// connect and membership changes. The final write is held to the build's epoch.
func (e *eventsStream) sendReadStates(ctx context.Context) bool {
	e.readStateBuilt = time.Now()
	epoch := e.sub.readStateVersion()
	viewer, err := e.s.store.PrincipalByID(ctx, e.principalID)
	if err != nil {
		return e.failed(ctx, "read the read-state viewer", err)
	}
	if viewer.Kind != store.PrincipalHuman {
		return true
	}
	all, ids, removed := e.sub.readStateTargets()
	if all {
		channels, err := e.s.store.ListChannelsForPrincipal(ctx, e.principalID)
		if err != nil {
			return e.failed(ctx, "list read-state channels", err)
		}
		if !e.sub.setMembershipsIfCurrent(channels, epoch) {
			return true
		}
		ids = ids[:0]
		for _, channel := range channels {
			ids = append(ids, channel.ID)
		}
	}
	var marks map[int64]store.ReadMark
	if len(ids) > 0 {
		marks, err = e.s.store.ReadMarksFor(ctx, e.principalID)
		if err != nil {
			return e.failed(ctx, "list read marks", err)
		}
	}
	next := make(map[int64][]byte, len(e.lastReadStates))
	if !all {
		for id, raw := range e.lastReadStates {
			next[id] = raw
		}
	}
	for _, id := range ids {
		if !all && !e.sub.hasMembership(id) {
			delete(next, id)
			continue
		}
		doc, err := e.s.readStateDoc(e.r, id, marks[id])
		if err != nil {
			return e.failed(ctx, "count unread messages", err)
		}
		raw, ok := e.encode(ctx, doc)
		if !ok {
			return false
		}
		next[id] = raw
		previous, existed := e.lastReadStates[id]
		if existed && !bytes.Equal(previous, raw) || !existed && doc.Unread > 0 {
			if !e.writeReadState(ctx, epoch, raw) {
				return false
			}
			if e.sub.readStateVersion() != epoch {
				return true
			}
		}
	}
	for _, id := range removed {
		delete(next, id)
		raw, ok := e.encode(ctx, schema.ReadStateV1{Schema: schema.ReadStateSchemaV1, ChannelID: id})
		if !ok {
			return false
		}
		if !e.writeReadState(ctx, epoch, raw) {
			return false
		}
		if e.sub.readStateVersion() != epoch {
			return true
		}
	}
	e.lastReadStates = next
	return true
}

func (e *eventsStream) writeReadState(ctx context.Context, epoch uint64, raw []byte) bool {
	e.readStateWriting = epoch
	defer func() { e.readStateWriting = 0 }()
	return e.write(ctx, raw)
}

// rosterNowOrLater sends the owed roster now, or, when one was built less
// than the server's roster floor ago, arms rosterLater to send it once the floor has
// passed. Rosters owed meanwhile are the same roster.
func (e *eventsStream) rosterNowOrLater(ctx context.Context) bool {
	if e.rosterLater != nil {
		return true
	}
	if wait := e.s.eventsRosterFloor - time.Since(e.rosterBuilt); !e.rosterBuilt.IsZero() && wait > 0 {
		e.rosterLater = time.After(wait)
		return true
	}
	return e.sendRoster(ctx)
}

// sendRoster builds the roster for this reader now and sends it unless it is
// the one last sent, then does the same for the member list.
func (e *eventsStream) sendRoster(ctx context.Context) bool {
	e.rosterBuilt = time.Now()
	viewer, err := e.s.store.PrincipalByID(ctx, e.principalID)
	if err != nil {
		return e.failed(ctx, "read the viewer", err)
	}
	// What the viewer holds, read once per roster from the viewer just read
	// (ADR-014 design §7.3): manage_members shows every session's runtime
	// and host, manage_channels the member list's roles and every
	// can_decide.
	held, err := e.s.store.HeldBy(ctx, viewer, schema.PermManageMembers, schema.PermManageChannels)
	if err != nil {
		return e.failed(ctx, "read the viewer's permissions", err)
	}
	seesHosts, seesAll := held[0], held[1]
	roster, err := e.s.channelRoster(ctx, e.r, e.channel, viewer, seesHosts)
	if err != nil {
		return e.failed(ctx, "build a roster", err)
	}
	raw, ok := e.encode(ctx, roster)
	if !ok {
		return false
	}
	// The list this reader is measured against is read before the first
	// roster goes out (issue #528). The client reads the list over REST once
	// it has that roster, so a change that commits after the write is one it
	// may have missed: read afterwards, the baseline would already hold it,
	// and the change would never be sent.
	if e.lastMembers == nil {
		if e.lastMembers, ok = e.membersFrame(ctx, viewer, seesAll); !ok {
			return false
		}
	}
	if !bytes.Equal(raw, e.lastRoster) {
		if !e.write(ctx, raw) {
			return false
		}
		e.lastRoster = raw
	}
	return e.sendMembers(ctx, viewer, seesAll)
}

// sendMembers builds the member list for this reader now and sends it if it
// changed since the last one (issue #515). Everything that changes the list
// already owes a roster (a member added or removed, hidden or renamed), so the
// list is rebuilt with each roster, under the same floor; the roster alone
// would miss a member who had no session. The list as it stood before the
// first roster was written is kept but not sent (sendRoster): a client reads
// the list over REST once the socket is live, as it always has, and learns
// here only what changed after.
// A channel the change did not touch hears nothing.
func (e *eventsStream) sendMembers(ctx context.Context, viewer store.Principal, seesAll bool) bool {
	raw, ok := e.membersFrame(ctx, viewer, seesAll)
	if !ok {
		return false
	}
	if bytes.Equal(raw, e.lastMembers) {
		return true
	}
	if !e.write(ctx, raw) {
		return false
	}
	e.lastMembers = raw
	return true
}

// membersFrame is the encoded member list as viewer sees it now; seesAll is
// whether viewer holds manage_channels (memberDocs).
func (e *eventsStream) membersFrame(ctx context.Context, viewer store.Principal, seesAll bool) ([]byte, bool) {
	members, err := e.s.store.ListChannelMembers(ctx, e.channel.ID)
	if err != nil {
		return nil, e.failed(ctx, "list members", err)
	}
	doc := schema.ChannelMembersV1{Schema: schema.ChannelMembersSchemaV1, ChannelID: e.channel.ID, Members: memberDocs(members, &viewer, seesAll)}
	return e.encode(ctx, doc)
}

// sendRef reads the document ref names with this reader's gates and sends it,
// or sends nothing when they do not let the reader see it.
func (e *eventsStream) sendRef(ctx context.Context, ref eventRef) bool {
	s := e.s
	switch ref.kind {
	case eventRun:
		run, err := s.store.RunVisible(ctx, ref.id, readerFor(e.r, apiV2))
		if errors.Is(err, store.ErrRunNotFound) || (err == nil && run.ChannelID != e.channel.ID) {
			return true
		}
		if err != nil {
			return e.failed(ctx, "read a run", err)
		}
		return e.sendDoc(ctx, runDoc(run))
	case eventActivity:
		// The entry is read as a page of one that starts just before it, by
		// the listing REST uses; it is sent only if that page is the entry.
		entries, err := s.store.ListVisibleActivity(ctx, ref.runID, readerFor(e.r, apiV2), ref.id-1, 1)
		if errors.Is(err, store.ErrRunNotFound) {
			return true
		}
		if err != nil {
			return e.failed(ctx, "read activity", err)
		}
		if len(entries) == 0 || entries[0].ID != ref.id {
			return true
		}
		return e.sendDoc(ctx, activityDoc(entries[0]))
	case eventIntervention:
		iv, err := s.store.InterventionVisible(ctx, ref.id, readerFor(e.r, apiV2))
		if errors.Is(err, store.ErrInterventionNotFound) || (err == nil && iv.ChannelID != e.channel.ID) {
			return true
		}
		if err != nil {
			return e.failed(ctx, "read an intervention", err)
		}
		return e.sendDoc(ctx, interventionDoc(iv))
	case eventMessage:
		if !e.sub.hasMembership(ref.channelID) {
			return true
		}
		epoch := e.sub.readStateVersion()
		messages, err := s.store.ListVisibleMessages(ctx, ref.channelID, readerFor(e.r, apiV2), ref.id-1, 1)
		if err != nil {
			return e.failed(ctx, "read a notification message", err)
		}
		if len(messages) != 1 || messages[0].ID != ref.id || !e.sub.hasMembership(ref.channelID) {
			return true
		}
		raw, ok := e.encode(ctx, messageV2FromStore(messages[0]))
		return ok && e.writeReadState(ctx, epoch, raw)
	case eventMessageEdited:
		// The message as this reader may see it now, read as eventMessage
		// reads one; nothing if the reader cannot, or is no longer a member.
		// It never touches read state: an edit changes no count.
		if !e.sub.hasMembership(ref.channelID) {
			return true
		}
		messages, err := s.store.ListVisibleMessages(ctx, ref.channelID, readerFor(e.r, apiV2), ref.id-1, 1)
		if err != nil {
			return e.failed(ctx, "read an edited message", err)
		}
		if len(messages) != 1 || messages[0].ID != ref.id || messages[0].EditedAt.IsZero() || !e.sub.hasMembership(ref.channelID) {
			return true
		}
		return e.sendDoc(ctx, schema.MessageEditedV1{Schema: schema.MessageEditedSchemaV1, Message: messageV2FromStore(messages[0])})
	case eventReactions:
		// The message's whole reaction state for this reader, or nothing: no
		// row when the message is not visible to the reader, or when the reader
		// is no longer a current, enabled member (the store's explicit clause,
		// for a channel-wide message as for a scoped one). A frame is never
		// computed once and fanned out.
		if ref.channelID != e.channel.ID || !e.sub.hasMembership(ref.channelID) {
			return true
		}
		if hook := s.reactionBeforeRead; hook != nil {
			hook()
		}
		sum, err := s.store.ReactionSummaryOf(ctx, ref.channelID, readerFor(e.r, apiV2), ref.id, s.reactionClock)
		if errors.Is(err, store.ErrMessageNotFound) || errors.Is(err, store.ErrNotChannelMember) {
			return true
		}
		if err != nil {
			return e.failed(ctx, "read reactions", err)
		}
		return e.sendDoc(ctx, reactionSummaryDoc(ctx, sum))
	case eventChannel:
		// The channel as it is now, read by the id the socket is bound to:
		// flush has just checked that this reader is still a member. Two
		// renames in quick succession send the current state twice, which
		// a reader takes as it takes one.
		ch, err := s.store.ChannelByID(ctx, e.channel.ID)
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		if err != nil {
			return e.failed(ctx, "read the channel", err)
		}
		e.channel = ch
		return e.sendDoc(ctx, schema.ChannelDocV1{
			Schema:     schema.ChannelSchemaV1,
			ChannelID:  ch.ID,
			Name:       ch.Name,
			CreatedAt:  schema.NewTimestamp(ch.CreatedAt),
			Visibility: ch.Visibility,
			Default:    ch.IsDefault,
		})
	}
	return true
}

// validated is a pkg/schema document: every frame is checked before it goes.
type validated interface{ Validate() error }

func (e *eventsStream) sendDoc(ctx context.Context, doc validated) bool {
	raw, ok := e.encode(ctx, doc)
	return ok && e.write(ctx, raw)
}

func (e *eventsStream) encode(ctx context.Context, doc validated) ([]byte, bool) {
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "events ws: built an invalid document", "channel", e.channel.ID, "error", err)
		closeWS(ctx, e.conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
		return nil, false
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		slog.ErrorContext(ctx, "events ws: encode failed", "channel", e.channel.ID, "error", err)
		closeWS(ctx, e.conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
		return nil, false
	}
	return raw, true
}

func (e *eventsStream) write(ctx context.Context, raw []byte) bool {
	if hook := e.s.eventsBeforeWrite; hook != nil {
		hook()
	}
	if e.readStateWriting != 0 && e.sub.readStateVersion() != e.readStateWriting {
		e.readStateLater = time.After(0)
		return true
	}
	// The last point a drop can stop a frame (issue #519): one that was
	// being built when a revoke, a disable or a removal dropped the
	// subscription is not sent, and the socket closes saying why.
	select {
	case <-e.sub.dropped:
		e.s.closeEventsDropped(ctx, e.conn, e.channel.ID, e.principalID, e.credID)
		return false
	default:
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := countFrame(ctx, e.conn.Write(wctx, websocket.MessageText, raw)); err != nil {
		closeWS(ctx, e.conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
		_ = e.conn.CloseNow()
		return false
	}
	return true
}

// failed closes the socket after a read that failed, and reports false. A
// read cut short by the peer leaving or by shutdown is not a fault.
func (e *eventsStream) failed(ctx context.Context, what string, err error) bool {
	if ctx.Err() != nil {
		closeWS(ctx, e.conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
		return false
	}
	slog.ErrorContext(ctx, "events ws: could not "+what, "channel", e.channel.ID, "error", err)
	closeWS(ctx, e.conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
	return false
}

// closeEventsDropped closes conn after its subscription was dropped, saying
// why: shutdown, a revoked credential or disabled principal, removal from the
// channel, a manifest that no longer lets an agent read it, or, otherwise, a
// queue that filled (closeDropped's reason for that).
func (s *Server) closeEventsDropped(ctx context.Context, conn *websocket.Conn, channelID, principalID, credID int64) {
	if s.events.isClosed() {
		closeWS(ctx, conn, schema.SocketCloseServerShutdown, websocket.StatusGoingAway, "server shutting down")
		return
	}
	s.closeDropped(ctx, conn, channelID, principalID, credID)
}
