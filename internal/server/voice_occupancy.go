package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice occupancy (issue #596, docs/design/voice-occupancy.md): how many
// people are in each channel's voice, for every channel the caller may see
// voice in, so that someone told "meet in voice" can find the others.
//
// It is presence reduced to a number and read across channels, so it admits
// exactly the callers presence admits: a person (an agent is refused and
// audited, ADR-004) who holds voice (issue #437), and only for the channels
// they are a member of, as GET /v1/channels lists them (operators get no
// exemption). A channel the caller may not read is absent from the document,
// never a zero. Only the channel-wide room is counted: the poller tracks no
// other (voicePoller.roomLocked), so a net's room is never in anyone's count.
//
// The socket is one per viewer, not per channel, so a tab holds one whatever
// the number of channels (design note §4). The poller wakes its subscribers
// when what a count shows changes: someone joins or leaves, a room is hidden
// or shown again, LiveKit goes or comes back. A transmit flip or a screen
// share wakes none of them, and nothing here wakes the poller's loop or calls
// LiveKit.

// voiceOccupancyReadAttempts bounds how often a read of the caller's channels
// is made again because a removal from one of them landed while it ran. A
// removal is an operator's request; this many in the time of one read means
// something is wrong, and the request or socket fails closed.
const voiceOccupancyReadAttempts = 5

// errVoiceOccupancyChannelsChanging is the refusal when the caller's channels
// kept changing under every read.
var errVoiceOccupancyChannelsChanging = errors.New("voice occupancy: the caller's channels kept changing")

// voiceOccupancySub is one occupancy socket's subscription. Like a presence
// subscription, wake holds one queued notification and the poller only ever
// sends to it without blocking, so a slow socket costs the poller nothing.
// Unlike one, it holds the viewer's channel set, which a membership removal
// changes in place (voicePoller.occupancyChannelLost) rather than closing it.
type voiceOccupancySub struct {
	p            *voicePoller
	principalID  int64
	credentialID int64 // the credential the socket was opened with; 0 for none
	wake         chan struct{}
	dropped      chan struct{}

	// Guarded by p.mu.
	//
	// channels is the viewer's channels, ascending: the rows of every frame.
	channels []int64
	// gen counts changes to channels. A read of the store notes it first and
	// is applied only if nothing changed meanwhile (setChannels), so a read
	// that began before a removal cannot put the channel back after it.
	gen  uint64
	gone bool
}

// subscribeOccupancy registers an occupancy subscriber with no channels yet.
// After closeAll it returns one that is already dropped.
func (p *voicePoller) subscribeOccupancy(principalID, credentialID int64) *voiceOccupancySub {
	sub := &voiceOccupancySub{p: p, principalID: principalID, credentialID: credentialID, wake: make(chan struct{}, 1), dropped: make(chan struct{})}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		sub.gone = true
		close(sub.dropped)
		return sub
	}
	p.occSubs[sub] = struct{}{}
	return sub
}

func (s *voiceOccupancySub) cancel() {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	delete(s.p.occSubs, s)
}

// generation is the count of changes to the subscriber's channels so far.
func (s *voiceOccupancySub) generation() uint64 {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	return s.gen
}

// setChannels replaces the subscriber's channels with ids, read from the
// store after generation returned since. It reports false, and changes
// nothing, when the channels changed since then: the read may hold a channel
// the viewer has been removed from, and the caller reads again.
func (s *voiceOccupancySub) setChannels(ids []int64, since uint64) bool {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	if s.gen != since {
		return false
	}
	s.channels = ids
	s.gen++
	return true
}

// current returns the subscriber's document and the generation of the
// channels it was built from, or ok=false when the subscription has been
// dropped. Checking and building under one lock means a document is never
// built for a subscriber after the drop that removed them returned.
func (s *voiceOccupancySub) current() (doc schema.VoiceOccupancyV1, gen uint64, ok bool) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	if s.gone {
		return schema.VoiceOccupancyV1{}, 0, false
	}
	return s.p.occupancyLocked(s.channels), s.gen, true
}

// occupancy is the document for a viewer whose channels are channelIDs.
func (p *voicePoller) occupancy(channelIDs []int64) schema.VoiceOccupancyV1 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.occupancyLocked(channelIDs)
}

// occupancyLocked builds the occupancy document for channelIDs, which are
// ascending and distinct, from the room states the passes have written. A
// count is what that channel's presence would list (snapshotLocked): nobody
// in a stale room, nothing at all while LiveKit is unavailable, and each
// principal once. Every room the poller holds is a channel-wide one.
func (p *voicePoller) occupancyLocked(channelIDs []int64) schema.VoiceOccupancyV1 {
	doc := schema.VoiceOccupancyV1{Schema: schema.VoiceOccupancySchemaV1, Channels: []schema.VoiceChannelOccupancy{}}
	if p.s.lk == nil {
		return doc
	}
	doc.Configured = true
	if !p.contacted || p.down {
		return doc
	}
	doc.Available = true
	present := make(map[int64]map[int64]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		present[id] = map[int64]struct{}{}
	}
	for _, r := range p.rooms {
		in, ok := present[r.channelID]
		if !ok || r.stale {
			continue
		}
		for pid := range r.participants {
			in[pid] = struct{}{}
		}
	}
	for _, id := range channelIDs {
		doc.Channels = append(doc.Channels, schema.VoiceChannelOccupancy{ChannelID: id, InVoice: len(present[id])})
	}
	return doc
}

// notifyOccupancy wakes every occupancy subscriber: a count may have changed.
// It never blocks.
func (p *voicePoller) notifyOccupancy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.occSubs {
		wakeOccupancy(sub)
	}
}

func wakeOccupancy(sub *voiceOccupancySub) {
	select {
	case sub.wake <- struct{}{}:
	default: // one wake-up is already queued
	}
}

// occupancyChannelLost takes channelID out of principalID's occupancy
// subscriptions and wakes them: they were removed from the channel. It runs
// in the removal hook, before the removal is answered, so the next frame they
// are sent has no row for it. The generation moves on even when the channel
// was not in the set yet, so a read of the store already in flight is read
// again rather than applied.
func (p *voicePoller) occupancyChannelLost(channelID, principalID int64) {
	if principalID == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.occSubs {
		if sub.principalID != principalID {
			continue
		}
		sub.channels = slices.DeleteFunc(slices.Clone(sub.channels), func(id int64) bool { return id == channelID })
		sub.gen++
		wakeOccupancy(sub)
	}
}

// dropOccupancyLocked closes an occupancy subscription.
func (p *voicePoller) dropOccupancyLocked(sub *voiceOccupancySub) {
	delete(p.occSubs, sub)
	if !sub.gone {
		sub.gone = true
		close(sub.dropped)
	}
}

// voiceOccupancyCaller applies the checks of the caller that both occupancy
// routes share, in the order voicePresenceChannel applies them, with no
// channel to look up: a verified caller, then the refusal of agents
// (audited), then the refusal of a person without voice (audited, as on every
// voice route). It writes the response and returns false when the request
// must stop. No refusal names a channel. With authentication off there is no
// verified caller and voice is never anonymous: refused before anything is
// read.
func (s *Server) voiceOccupancyCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return store.Principal{}, false
	}
	// Humans only, as on presence: whether an agent may see who is in voice
	// is part of the decision that defers agents in voice (ADR-004), and a
	// count is the same information, smaller.
	if caller.Kind != store.PrincipalHuman {
		s.auditAgentDenial(ctx, caller.ID, r.Pattern, "", 0, denyAgentVoice)
		writeError(w, http.StatusForbidden, errForbidden.Code, "agents do not use voice")
		return store.Principal{}, false
	}
	if !caller.VoiceAllowed {
		s.denyForbidden(w, r, caller, schema.ErrorCodeVoiceNotAllowed, "voice is not part of your access to this server")
		return store.Principal{}, false
	}
	return caller, true
}

// voiceOccupancyChannels is the ids of principalID's channels, ascending: the
// read behind GET /v1/channels, so that the document and the channel list
// agree.
func (s *Server) voiceOccupancyChannels(ctx context.Context, principalID int64) ([]int64, error) {
	channels, err := s.store.ListChannelsForPrincipal(ctx, principalID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(channels))
	for _, ch := range channels {
		ids = append(ids, ch.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// loadOccupancyChannels reads the subscriber's channels from the store and
// sets them, reading again when a removal landed while it read.
func (s *Server) loadOccupancyChannels(ctx context.Context, sub *voiceOccupancySub) error {
	for range voiceOccupancyReadAttempts {
		since := sub.generation()
		ids, err := s.voiceOccupancyChannels(ctx, sub.principalID)
		if err != nil {
			return err
		}
		if sub.setChannels(ids, since) {
			return nil
		}
	}
	return errVoiceOccupancyChannelsChanging
}

// handleVoiceOccupancy serves GET /v1/voice/occupancy: the caller's voice
// occupancy, once. When voice is not configured the document says so
// (configured and available false, no channels) rather than failing.
func (s *Server) handleVoiceOccupancy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.voiceOccupancyCaller(w, r)
	if !ok {
		return
	}
	ids, err := s.voiceOccupancyChannels(ctx, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice occupancy: list channels failed", "error", err)
		writeInternalError(w)
		return
	}
	doc := s.voice.occupancy(ids)
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice occupancy: built an invalid document", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleVoiceOccupancyWS serves GET /v1/voice/occupancy/ws: it upgrades to a
// WebSocket and sends the caller's voice occupancy as a
// conch.voice_occupancy.v1 document on connect and again whenever it changes,
// and nothing otherwise.
//
// It follows handleVoiceWS: refusals before the upgrade are plain HTTP, the
// subscription is registered before the credential is checked and the
// channels are read (a revoke, a disable or a removal that commits meanwhile
// then reaches it), and an open socket re-checks its credential on an
// interval and is closed at once when the principal is disabled or signed
// out, or the credential it was opened with is revoked. Losing a channel does
// not close it: the channel leaves the next frame.
func (s *Server) handleVoiceOccupancyWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.voiceOccupancyCaller(w, r)
	if !ok {
		return
	}
	// The subscription carries the credential, so that revoking that one
	// credential closes it at once (issue #519).
	credID, hasCred := credentialIDFrom(ctx)
	sub := s.voice.subscribeOccupancy(caller.ID, credID)
	defer sub.cancel()
	// A disable or a revoke that committed after the middleware resolved the
	// token but before the subscription registered has run its drop already.
	if hasCred {
		live, err := s.store.CredentialLive(ctx, credID)
		if err != nil {
			slog.ErrorContext(ctx, "voice occupancy ws: check credential failed", "error", err)
			writeInternalError(w)
			return
		}
		if !live {
			writeUnauthenticated(w)
			return
		}
	}
	if err := s.loadOccupancyChannels(ctx, sub); err != nil {
		slog.ErrorContext(ctx, "voice occupancy ws: list channels failed", "error", err)
		writeInternalError(w)
		return
	}

	conn, ctx, done, ok := s.acceptSocket(w, r)
	if !ok {
		return
	}
	defer done()
	var recheck <-chan time.Time
	if hasCred {
		ticker := time.NewTicker(s.credRecheckInterval)
		defer ticker.Stop()
		recheck = ticker.C
	}
	s.streamOccupancy(ctx, conn, sub, credID, recheck)
}

// streamOccupancy writes occupancy until the peer leaves, the subscription is
// dropped, or the credential lapses. Each wake-up rebuilds the whole document
// and sends it only if it differs from the last one sent. Writes carry a
// deadline; the poller never waits for this loop.
func (s *Server) streamOccupancy(ctx context.Context, conn *websocket.Conn, sub *voiceOccupancySub, credID int64, recheck <-chan time.Time) {
	ctx = s.keepAlive(ctx, conn, false)
	var last []byte
	send := func() bool {
		for {
			doc, gen, live := sub.current()
			if !live {
				s.closeOccupancyDropped(ctx, conn, credID)
				return false
			}
			if err := doc.Validate(); err != nil {
				slog.ErrorContext(ctx, "voice occupancy ws: built an invalid document", "error", err)
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
				return false
			}
			raw, err := json.Marshal(doc)
			if err != nil {
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
				return false
			}
			if bytes.Equal(raw, last) {
				return true
			}
			// A drop that came while the frame was built (a revoke, a
			// disable) stops it here, as on the presence socket.
			select {
			case <-sub.dropped:
				s.closeOccupancyDropped(ctx, conn, credID)
				return false
			default:
			}
			// So does a removal from one of the channels: the frame is built
			// again without it.
			if sub.generation() != gen {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = countFrame(ctx, conn.Write(wctx, websocket.MessageText, raw))
			cancel()
			if err != nil {
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return false
			}
			last = raw
			return true
		}
	}
	if !send() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !s.credentialStillValid(ctx, conn, credID) || !s.occupancyStillReadable(ctx, conn, sub) || !send() {
				return
			}
		case <-sub.wake:
			if !send() {
				return
			}
		case <-sub.dropped:
			s.closeOccupancyDropped(ctx, conn, credID)
			return
		}
	}
}

// occupancyStillReadable re-reads the subscriber's channels on the interval
// re-check, which is how a channel they were added to appears. It reports
// true when the stream may continue; otherwise it has closed conn, failing
// closed on a store error.
func (s *Server) occupancyStillReadable(ctx context.Context, conn *websocket.Conn, sub *voiceOccupancySub) bool {
	cctx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	if err := s.loadOccupancyChannels(cctx, sub); err != nil {
		slog.ErrorContext(ctx, "voice occupancy ws: recheck channels failed", "error", err)
		closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "membership check failed")
		return false
	}
	return true
}

// closeOccupancyDropped closes conn after its subscription was dropped,
// saying why: shutdown, or a revoked credential or disabled principal, with
// the codes the presence socket uses.
func (s *Server) closeOccupancyDropped(ctx context.Context, conn *websocket.Conn, credID int64) {
	if s.voice.isClosed() {
		closeWS(ctx, conn, schema.SocketCloseServerShutdown, websocket.StatusGoingAway, "server shutting down")
		return
	}
	if credID != 0 {
		if live, err := s.store.CredentialLive(context.WithoutCancel(ctx), credID); err == nil && !live {
			closeWS(ctx, conn, schema.SocketCloseRevoked, websocket.StatusPolicyViolation, reasonCredentialInvalid)
			return
		}
	}
	closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "occupancy subscription closed")
}
