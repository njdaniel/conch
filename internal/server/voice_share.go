package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Screen share (issue #443, docs/design/voice-control-plane.md §12).
//
// A join token lets its holder publish a microphone and nothing else. Sharing
// a screen is a separate, checked step: a person in the room claims the
// room's share, and conchd asks LiveKit to let that one connection publish
// the screen-share sources as well (UpdateParticipant). A room has one share
// at a time. The sharer stops it, or an operator does, member or not; and it
// ends by itself when the sharer leaves or loses access, when the room is
// rotated, or when nothing has been shared for a while. Ending a share takes
// the sources away again, and LiveKit then unpublishes the share's tracks
// for everyone at once (measured on 1.13.7).
//
// The claim lives in memory, per room, like transmit state, and a restart
// drops every claim. The passes check every participant against the room's
// claim: whoever has a screen-share source granted or published without
// holding the claim has it withdrawn, and a published one is audited as
// voice_share_unclaimed. That is also what clears whatever a restart left
// granted.
//
// Nothing here puts a token, the API secret or a room name in a log line, an
// audit row or an error.

const (
	// voiceShareFirstTrack is how long a share may go without any
	// screen-share track being seen before it ends as idle: time to choose
	// what to share in the browser's picker and start publishing.
	voiceShareFirstTrack = 15 * time.Second
	// voiceShareTrackGone is how long a share may go on after the last pass
	// that saw one of its tracks: a share whose tracks are gone has ended.
	voiceShareTrackGone = 5 * time.Second
)

// Why a share ended: the reason on voice_share_stopped.
const (
	voiceShareStopSelf       = "self"
	voiceShareStopOperator   = "operator"
	voiceShareStopLeft       = "left"
	voiceShareStopAccessLost = "access_lost"
	voiceShareStopRotated    = "rotated"
	voiceShareStopIdle       = "idle"
)

// voiceShare is a room's screen share: who holds it, since when, and what the
// passes have seen of it. Guarded by voicePoller.mu, and changed only by
// someone who also holds the room's order (voiceRoomState.order), so that its
// started and stopped rows are in the log in the order they happened.
type voiceShare struct {
	principalID int64
	startedAt   time.Time
	// pending is set from the moment the claim is recorded until its
	// voice_share_started row is written. Nothing ends a pending share: a
	// share that never started has nothing to stop. Presence does not show
	// it yet.
	pending bool
	// published says a pass has seen one of the share's tracks, and
	// lastTrack when one last did. audio is whether the last pass saw a
	// screen-share audio track.
	published bool
	lastTrack time.Time
	audio     bool
}

// Errors of the share endpoints, each answered as the handlers say.
var (
	// errVoiceShareBusy: another principal holds the room's share. 409
	// voice_share_busy.
	errVoiceShareBusy = errors.New("voice: another principal holds the room's share")
	// errVoiceShareNotConnected: the caller holds a session but LiveKit has no
	// connection of theirs in the room to grant the share to. 409
	// voice_no_session: a share is granted to a connection, and they have
	// none.
	errVoiceShareNotConnected = errors.New("voice: the caller is not connected to the room")
	// errVoiceShareRoomGone: the room was rotated, or the share being claimed
	// again ended, while the claim was being made. 409 voice_no_session.
	errVoiceShareRoomGone = errors.New("voice: the room or the share went away during the claim")
	// errVoiceShareNotYours: a caller who is neither the sharer nor an
	// operator asked to stop the share. 403.
	errVoiceShareNotYours = errors.New("voice: the share belongs to another principal")
	// errVoiceShareNotChannelRoom: a claim for a room that is not a
	// channel-wide room. Only those rooms have a share (and only in them may
	// every member publish, which withdrawing a share restores). 409
	// voice_no_session, as a transmit report for an audience with no room is.
	errVoiceShareNotChannelRoom = errors.New("voice: only a channel-wide room has a share")
)

// voiceRoomPublishes is whether every participant of room may publish a
// microphone: what its join grant is, and so what withdrawing a share leaves
// them. It is voiceAudiences' answer: a member may publish into the
// channel-wide room. Any other room (a net, V5) may hold listen-only
// participants, so it answers false, and a share is never claimed there.
func voiceRoomPublishes(room store.VoiceRoom) bool { return room.NetID == 0 }

// handleVoiceShareStart serves POST /v1/channels/{channel}/voice/share: the
// caller claims the room's screen share.
//
// The caller is checked exactly as for a session, by the same code and so in
// the same order (voiceSessionCaller): a non-member gets the unknown-channel
// 404, an agent 403, a person without voice 403 voice_not_allowed, and only
// then is a member told that voice is not configured. Then, as for a transmit
// report, the credential the request came with must hold a session for the
// channel's current room (409 voice_no_session). Only then is the room's share
// looked at: held by someone else is 409 voice_share_busy.
func (s *Server) handleVoiceShareStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}
	credID, ok := credentialIDFrom(ctx)
	if !ok {
		writeVoiceNoSession(w)
		return
	}
	room, held, err := s.voiceHeldRoom(ctx, channel.ID, caller.ID, credID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: holder check for a share failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !held {
		writeVoiceNoSession(w)
		return
	}
	share, err := s.voice.claimShare(ctx, room, caller.ID)
	switch {
	case errors.Is(err, errVoiceShareBusy):
		writeError(w, http.StatusConflict, schema.ErrorCodeVoiceShareBusy, "someone else is sharing in this room")
		return
	case errors.Is(err, errVoiceShareNotConnected), errors.Is(err, errVoiceShareRoomGone), errors.Is(err, errVoiceShareNotChannelRoom):
		writeVoiceNoSession(w)
		return
	case errors.Is(err, livekit.ErrUnavailable):
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable; a share could not be granted", "channel", channel.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: a share could not be granted", "channel", channel.ID, "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	if err := share.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice: built an invalid share", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, share)
}

// handleVoiceShareStop serves DELETE /v1/channels/{channel}/voice/share: the
// sharer stops the room's share, or an operator stops it for them.
//
// Moderation is for operators, and an operator could add themselves to any
// channel anyway, so an operator is let through whether or not they are a
// member, after only the checks that every request makes: a verified caller
// and a channel that exists. Anyone else is checked as on every voice route
// (voicePresenceChannel), so a non-member still gets the unknown-channel 404
// and learns nothing. Stopping a share nobody holds changes nothing and
// answers 204; stopping someone else's is 403 for anyone but an operator.
func (s *Server) handleVoiceShareStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return
	}
	var channel store.Channel
	operator := isOperator(caller)
	if operator {
		ch, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
		if errors.Is(err, store.ErrNotFound) {
			writeChannelNotFound(w)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "voice: find channel failed", "error", err)
			writeInternalError(w)
			return
		}
		channel = ch
	} else {
		ch, _, ok := s.voicePresenceChannel(w, r, r.PathValue("channel"), nil)
		if !ok {
			return
		}
		channel = ch
	}
	if s.lk == nil {
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceNotConfigured, "voice is not configured on this server")
		return
	}
	room, ok, err := s.voiceChannelRoom(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: find the room of a share failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !ok {
		// No room, so no share: nothing to stop.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := s.voice.stopShare(ctx, room, caller.ID, operator); {
	case errors.Is(err, errVoiceShareNotYours):
		s.denyForbidden(w, r, caller, errForbidden.Code, "only the person sharing or an operator can stop a share")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: stopping a share failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// voiceChannelRoom finds the channel's current channel-wide room. It only
// reads: a channel nobody has asked a session for has no room, and this must
// not be what creates one.
func (s *Server) voiceChannelRoom(ctx context.Context, channelID int64) (store.VoiceRoom, bool, error) {
	rooms, err := s.store.VoiceRoomsForChannel(ctx, channelID)
	if err != nil {
		return store.VoiceRoom{}, false, err
	}
	for _, room := range rooms {
		if room.NetID == 0 {
			return room, true, nil
		}
	}
	return store.VoiceRoom{}, false, nil
}

// claimShare makes principalID the holder of room's share and has LiveKit
// let their connection publish the screen-share sources. A holder who claims
// again is granted the sources again (their connection may be a new one,
// made with a join token, which grants the microphone alone) and answered
// with the share they hold; nothing is written.
//
// Claims and stops of one room are made one at a time (voiceRoomState.claims),
// and so are the withdrawals the passes make, so none of them can take away a
// grant another has just made. The share is recorded before LiveKit is asked
// and stays pending until its voice_share_started row is written: a pass sees
// the holder as the holder throughout and does not withdraw what is being
// granted. If LiveKit refuses or cannot be asked, the claim is undone and
// whatever LiveKit may have granted is left for the passes to withdraw.
//
// The row is written before the claim is answered, and a claim whose row
// cannot be written is undone and its sources withdrawn: a share conchd
// cannot account for does not start.
func (p *voicePoller) claimShare(ctx context.Context, room store.VoiceRoom, principalID int64) (schema.VoiceShareV1, error) {
	// The grant below lets the connection publish a microphone as well as
	// the share, which is right only where every member may.
	if !voiceRoomPublishes(room) {
		return schema.VoiceShareV1{}, errVoiceShareNotChannelRoom
	}
	p.mu.Lock()
	r := p.roomLocked(room)
	p.mu.Unlock()
	r.claims.Lock()
	defer r.claims.Unlock()

	r.order.Lock()
	p.mu.Lock()
	if r.retired {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	held := r.share
	if held != nil && held.principalID != principalID {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceShareV1{}, errVoiceShareBusy
	}
	mine := held
	if held == nil {
		mine = &voiceShare{principalID: principalID, startedAt: p.now().Truncate(time.Millisecond), pending: true}
		r.share = mine
		delete(r.withdrawing, principalID)
	}
	p.mu.Unlock()
	r.order.Unlock()

	// Not tied to the caller: a claim LiveKit has granted is seen through,
	// whatever becomes of the request. Bounded, because the room's claims and
	// the passes' withdrawals wait for it.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	identity := voiceIdentity(principalID)
	found, err := p.s.lk.UpdateParticipant(lctx, room.RoomName, identity, livekit.Permission{CanPublish: true, ScreenShare: true})
	if err != nil || !found {
		if held == nil {
			p.undoClaim(r, mine)
		}
		if err != nil {
			return schema.VoiceShareV1{}, err
		}
		return schema.VoiceShareV1{}, errVoiceShareNotConnected
	}
	if held != nil {
		p.mu.Lock()
		ended := r.retired || r.share != held
		p.mu.Unlock()
		if ended {
			// A pass ended the share (it went idle, say) while it was
			// being granted again: what was just granted is taken back,
			// and the caller is told, as for any share that is gone, to
			// start again.
			p.withdrawLocked(ctx, room.RoomName, room.ChannelID, principalID, voiceRoomPublishes(room))
			return schema.VoiceShareV1{}, errVoiceShareRoomGone
		}
		return schema.VoiceShareV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(held.startedAt), Audio: true}, nil
	}

	r.order.Lock()
	p.mu.Lock()
	if r.retired || r.share != mine {
		// Rotated while LiveKit was being asked: the room's deletion took
		// the grant with it, and the share never started.
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	p.mu.Unlock()
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	err = p.writeEvent(wctx, voiceEvent{
		actor: fmt.Sprintf("principal:%d", principalID), action: store.AuditVoiceShareStarted, channelID: room.ChannelID,
		detail: fmt.Sprintf("channel=%d audio=true", room.ChannelID), at: mine.startedAt,
	})
	wcancel()
	if err != nil {
		p.mu.Lock()
		if r.share == mine {
			r.share = nil
			r.withdrawing[principalID] = 0
		}
		p.mu.Unlock()
		r.order.Unlock()
		p.withdrawLocked(ctx, room.RoomName, room.ChannelID, principalID, voiceRoomPublishes(room))
		return schema.VoiceShareV1{}, fmt.Errorf("voice: audit voice_share_started: %w", err)
	}
	p.mu.Lock()
	mine.pending = false
	p.mu.Unlock()
	r.order.Unlock()
	p.notify(room.ChannelID)

	// A rotation can land between the holder check and the claim being
	// recorded, and the claim then made state for a room nobody can be in.
	// If the room is retired, the grant is taken back at once and forgetting
	// the room ends the share (reason=rotated). Deleting the room would take
	// the grant too, but a DeleteRoom that fails leaves the room until the
	// sweep, up to 30 s, and the grant must not last that long. A rotation
	// that commits after this read forgets the room itself.
	live, err := p.s.store.VoiceRoomLive(lctx, room.ID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "voice: could not tell whether a room was retired after a share; the sweep checks", "channel", room.ChannelID, "error", err)
		p.sweepSoon()
	case !live:
		if p.withdrawLocked(ctx, room.RoomName, room.ChannelID, principalID, voiceRoomPublishes(room)) {
			p.withdrawConfirmed(r, principalID)
		}
		p.forgetRoom(ctx, room.RoomName)
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	return schema.VoiceShareV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(mine.startedAt), Audio: true}, nil
}

// undoClaim takes back a claim that was never granted. Whatever LiveKit may
// have granted before failing to say so is left for the passes, which
// withdraw it without auditing it (withdrawing).
func (p *voicePoller) undoClaim(r *voiceRoomState, mine *voiceShare) {
	r.order.Lock()
	p.mu.Lock()
	if r.share == mine {
		r.share = nil
		r.withdrawing[mine.principalID] = 0
	}
	p.mu.Unlock()
	r.order.Unlock()
}

// stopShare ends room's share for callerID: their own, or, for an operator,
// anyone's. A room with no share is left as it is. The share leaves presence
// and its row is written before LiveKit is asked to withdraw the sources; a
// withdrawal that fails is retried by the passes, and a row that cannot be
// written is logged: a stop is not refused for the sake of its record.
func (p *voicePoller) stopShare(ctx context.Context, room store.VoiceRoom, callerID int64, operator bool) error {
	r := p.roomNamed(room.RoomName)
	if r == nil {
		return nil
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	r.order.Lock()
	p.mu.Lock()
	c := r.share
	if r.retired || c == nil {
		p.mu.Unlock()
		r.order.Unlock()
		return nil
	}
	reason, actor := voiceShareStopSelf, c.principalID
	if c.principalID != callerID {
		if !operator {
			p.mu.Unlock()
			r.order.Unlock()
			return errVoiceShareNotYours
		}
		reason, actor = voiceShareStopOperator, callerID
	}
	r.share = nil
	r.withdrawing[c.principalID] = 0
	p.mu.Unlock()
	p.writeEvents(ctx, shareStoppedEvent(r.channelID, c.principalID, actor, reason))
	r.order.Unlock()
	p.notify(r.channelID)
	// If LiveKit cannot be asked, the passes try again, and remove the
	// sharer from the room if the share is still published and its sources
	// still cannot be withdrawn (withdrawShares): a stopped share does not go
	// on being watched.
	if p.withdrawLocked(ctx, r.name, r.channelID, c.principalID, voiceRoomPublishes(room)) {
		p.withdrawConfirmed(r, c.principalID)
	}
	return nil
}

// shareStoppedEvent is the voice_share_stopped row: actor is who stopped it,
// the sharer for every reason but operator.
func shareStoppedEvent(channelID, sharer, actor int64, reason string) voiceEvent {
	return voiceEvent{
		actor: fmt.Sprintf("principal:%d", actor), action: store.AuditVoiceShareStopped, channelID: channelID,
		detail: fmt.Sprintf("channel=%d principal=%d reason=%s", channelID, sharer, reason),
	}
}

// endShareLocked ends r's share with reason, if principalID holds it and it
// has started: the share is dropped, the holder's sources are marked to be
// withdrawn, and the row is returned for the caller to write. The caller holds
// r.order and p.mu.
func (p *voicePoller) endShareLocked(r *voiceRoomState, principalID int64, reason string) []voiceEvent {
	c := r.share
	if c == nil || c.pending || c.principalID != principalID {
		return nil
	}
	r.share = nil
	r.withdrawing[principalID] = 0
	return []voiceEvent{shareStoppedEvent(r.channelID, principalID, principalID, reason)}
}

// withdrawLocked asks LiveKit to take the screen-share sources away from
// principalID's connection to room, leaving it the microphone if canPublish
// and nothing to publish otherwise. canPublish is the participant's own
// grant, never more: the room's (voiceRoomPublishes) where the caller knows
// the participant holds it, else what LiveKit last reported they may do, so
// that a listen-only participant is never made a publisher by losing a
// share. The caller holds the room's claims (voiceRoomState.claims). A
// participant who is not there has nothing to withdraw. A failure is logged,
// and the passes try again. It reports whether LiveKit answered.
func (p *voicePoller) withdrawLocked(ctx context.Context, roomName string, channelID, principalID int64, canPublish bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	if _, err := p.s.lk.UpdateParticipant(ctx, roomName, voiceIdentity(principalID), livekit.Permission{CanPublish: canPublish}); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing screen-share sources failed; passes retry", "channel", channelID, "principal", principalID, "error", err)
		return false
	}
	return true
}

// withdrawConfirmed records that LiveKit has taken principalID's screen-share
// sources away, if a withdrawal of theirs was still owed: the entry is stamped
// with the room's eviction count (voiceRoomState.seq), bumped for it. A pass
// that listed the room before that moment may still have seen the share's
// tracks and says nothing of them; one that listed it after and sees a share
// published again (a client rejoining on a token LiveKit sent it during the
// share) audits it as unclaimed.
func (p *voicePoller) withdrawConfirmed(r *voiceRoomState, principalID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, owed := r.withdrawing[principalID]; owed {
		r.seq++
		r.withdrawing[principalID] = r.seq
	}
}

// shareWithdrawal is one participant a pass found with screen-share sources
// they do not hold the share for. sharing says they had a track published:
// if the sources cannot be withdrawn, they are removed from the room instead.
// unclaimed says it is also audited as a share nobody granted. canPublish is
// what LiveKit reported they may publish, which the withdrawal leaves them.
type shareWithdrawal struct {
	principalID int64
	joinedAt    time.Time
	sharing     bool
	unclaimed   bool
	canPublish  bool
}

// judgeShareLocked is a pass's part in the screen share, under r.order and
// p.mu, at now. seen is who the pass found entitled, keep whose entitlement
// it could not read, and lost who it found no longer entitled. It ends the
// room's share if its holder has gone, lost access or let it idle, notes what
// the pass saw of the share, and lists everyone else who has a screen-share
// source granted or published: their sources are to be withdrawn, and a
// published one is audited as unclaimed, once until it is gone. It returns the
// rows to write, the withdrawals to make, and whether presence changed.
func (p *voicePoller) judgeShareLocked(r *voiceRoomState, now time.Time, seen map[int64]livekit.Participant, keep map[int64]struct{}, lost map[int64]bool, since uint64) ([]voiceEvent, []shareWithdrawal, bool) {
	var events []voiceEvent
	changed := false
	if c := r.share; c != nil && !c.pending {
		pid := c.principalID
		part, here := seen[pid]
		_, undecided := keep[pid]
		switch {
		case undecided:
			// Nothing is known of them on this pass; the next decides.
		case lost[pid]:
			events = append(events, p.endShareLocked(r, pid, voiceShareStopAccessLost)...)
		case !here:
			events = append(events, p.endShareLocked(r, pid, voiceShareStopLeft)...)
		default:
			if part.Sharing() {
				c.published, c.lastTrack = true, now
			}
			if c.audio != part.ScreenShareAudioPublished {
				c.audio = part.ScreenShareAudioPublished
				changed = true
			}
			if (!c.published && now.Sub(c.startedAt) >= voiceShareFirstTrack) ||
				(c.published && !part.Sharing() && now.Sub(c.lastTrack) >= voiceShareTrackGone) {
				events = append(events, p.endShareLocked(r, pid, voiceShareStopIdle)...)
			}
		}
		if r.share == nil {
			changed = true
		}
	}

	pids := make([]int64, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	var withdraw []shareWithdrawal
	for _, pid := range pids {
		part := seen[pid]
		if r.share != nil && r.share.principalID == pid {
			continue // the holder, pending or not
		}
		stamp, owed := r.withdrawing[pid]
		// Still owed, or confirmed after this pass listed the room: what it
		// saw is the share being taken away, not a new one.
		quiet := owed && (stamp == 0 || stamp > since)
		if !part.Sharing() && !part.ShareSourcesGranted {
			if !quiet || stamp == 0 {
				delete(r.withdrawing, pid) // nothing left to withdraw
			}
			continue
		}
		w := shareWithdrawal{principalID: pid, joinedAt: part.JoinedAt, sharing: part.Sharing(), canPublish: part.CanPublish}
		if part.Sharing() && !quiet {
			// A share nobody granted: impossible through conchd, unless a
			// restart dropped the claim or the client rejoined on a token
			// LiveKit sent it while it held one, after the share ended.
			w.unclaimed = true
			events = append(events, voiceEvent{
				actor: fmt.Sprintf("principal:%d", pid), action: store.AuditVoiceShareUnclaimed, channelID: r.channelID,
				detail: fmt.Sprintf("channel=%d sources=%s source=observed", r.channelID, shareSources(part)),
			})
		}
		if !quiet || stamp == 0 {
			// Owed again until LiveKit confirms. A stamp later than this
			// pass's listing stands: that confirmation is newer than what
			// this pass saw.
			r.withdrawing[pid] = 0
		}
		withdraw = append(withdraw, w)
	}
	// Whoever is gone from the room has nothing left to withdraw; whoever
	// could not be read is looked at again next pass.
	for pid := range r.withdrawing {
		_, here := seen[pid]
		_, undecided := keep[pid]
		if !here && !undecided {
			delete(r.withdrawing, pid)
		}
	}
	return events, withdraw, changed
}

// shareSources names the screen-share sources part has published, for an
// unclaimed row.
func shareSources(part livekit.Participant) string {
	var s []string
	if part.ScreenSharePublished {
		s = append(s, "screen_share")
	}
	if part.ScreenShareAudioPublished {
		s = append(s, "screen_share_audio")
	}
	return strings.Join(s, ",")
}

// withdrawShares makes a pass's withdrawals in r, under the room's claims so
// that none takes away a grant a claim has just made: each is checked again,
// under the lock, against the room's share. A grant with nothing published
// whose withdrawal fails is retried by the next pass. A share still published
// whose sources cannot be withdrawn, unclaimed or stopped, is removed from the
// room instead (fail closed): nobody goes on watching a share conchd has
// ended or never granted. It reports whether the room needs another pass.
func (p *voicePoller) withdrawShares(ctx context.Context, r *voiceRoomState, list []shareWithdrawal) bool {
	if len(list) == 0 {
		return false
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	again := false
	for _, w := range list {
		p.mu.Lock()
		holds := r.share != nil && r.share.principalID == w.principalID
		gone := r.retired
		p.mu.Unlock()
		if holds || gone {
			continue
		}
		if p.withdrawLocked(ctx, r.name, r.channelID, w.principalID, w.canPublish) {
			p.withdrawConfirmed(r, w.principalID)
			continue
		}
		again = true
		if !w.sharing {
			continue
		}
		reason := voiceReasonShareNotWithdrawn
		if w.unclaimed {
			reason = voiceReasonShareUnclaimed
		}
		part := livekit.Participant{Identity: voiceIdentity(w.principalID), JoinedAt: w.joinedAt}
		p.removeObserved(ctx, r, part, w.principalID, reason)
	}
	return again
}
