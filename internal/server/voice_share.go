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

// Why a share ended: the reason on voice_share_stopped, and the one the sharer
// is told in their own presence (issue #487).
const (
	voiceShareStopSelf       = schema.VoiceShareEndSelf
	voiceShareStopOperator   = schema.VoiceShareEndOperator
	voiceShareStopLeft       = schema.VoiceShareEndLeft
	voiceShareStopAccessLost = schema.VoiceShareEndAccessLost
	voiceShareStopRotated    = schema.VoiceShareEndRotated
	voiceShareStopIdle       = schema.VoiceShareEndIdle
)

// voiceShareEndKey names whose share ended, and in which channel: the key of
// voicePoller.shareEnded.
type voiceShareEndKey struct {
	channelID, principalID int64
}

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
	// errVoiceShareStale: a release named a share that is not the room's
	// current share, or not the caller's. 409 voice_share_stale, and nothing
	// changes.
	errVoiceShareStale = errors.New("voice: the release names a share that is no longer the caller's")
	// errVoiceShareNotChannelRoom: a claim for a room that is not a
	// channel-wide room. Only those rooms have a share (and only in them may
	// every member publish, which withdrawing a share restores). 409
	// voice_no_session, as a transmit report for an audience with no room is.
	errVoiceShareNotChannelRoom = errors.New("voice: only a channel-wide room has a share")
)

// voiceWithdrawKey names one participant of a person in a room's withdrawing
// map: the person's own connection, their companion or their viewer each owe
// and confirm a withdrawal apart.
type voiceWithdrawKey struct {
	principalID int64
	role        voiceRole
}

// owe marks the person and their companion as owing a withdrawal. A viewer is
// never owed one: it holds nothing to take away, so one found publishing is
// always a share nobody granted. The caller holds voicePoller.mu.
func (r *voiceRoomState) owe(principalID int64) {
	r.withdrawing[voiceWithdrawKey{principalID, voiceRolePerson}] = 0
	r.withdrawing[voiceWithdrawKey{principalID, voiceRoleScreen}] = 0
}

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
// sharer stops the room's share, or an operator or channel manager stops it.
//
// An operator is let through whether or not they are a member, after only the
// checks that every request makes: a verified caller and a channel that exists.
// A manager's grant is checked before the ordinary voice-access gate because
// stopping a share is moderation, not voice use. Anyone else is checked as on
// every voice route (voicePresenceChannel), so a non-member still gets the
// unknown-channel 404 and learns nothing. Stopping a share nobody holds changes
// nothing and answers 204; stopping someone else's is 403 for anyone but a
// moderator.
//
// A release may name the share it means (schema.VoiceShareClaimParam, issue
// #487): the started_at of the caller's own claim. It then ends that share
// only while it is still the room's and the caller's, and otherwise answers
// 409 voice_share_stale and changes nothing, operator or not. That is what
// keeps a release sent late, by a page whose claim lapsed while its picker was
// open, from ending somebody's newer share as a moderation stop. A request
// that names no share is the stop it always was. The parameter is read before
// the channel, so a malformed one says nothing about the channel.
func (s *Server) handleVoiceShareStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return
	}
	var claim *schema.Timestamp
	if values, named := r.URL.Query()[schema.VoiceShareClaimParam]; named {
		var started schema.Timestamp
		var err error
		if len(values) == 1 {
			started, err = schema.ParseVoiceShareClaim(values[0])
		}
		if len(values) != 1 || err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "started_at must be the started_at of your share, given once")
			return
		}
		claim = &started
	}
	var channel store.Channel
	moderator := isOperator(caller)
	if moderator {
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
		moderator, err = s.store.ManagesChannel(ctx, ch.ID, caller.ID)
		if err != nil {
			slog.ErrorContext(ctx, "voice: check channel manager failed", "channel", ch.ID, "error", err)
			writeInternalError(w)
			return
		}
		if moderator {
			channel = ch
		} else {
			ch, _, ok := s.voicePresenceChannel(w, r, r.PathValue("channel"), nil)
			if !ok {
				return
			}
			channel = ch
		}
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
		// No room, so no share: nothing to stop, and the share a release
		// names is not current.
		if claim != nil {
			writeVoiceShareStale(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := s.voice.stopShare(ctx, room, caller.ID, moderator, claim); {
	case errors.Is(err, errVoiceShareNotYours):
		s.denyForbidden(w, r, caller, errForbidden.Code, "only the person sharing, a manager of the channel or an operator can stop a share")
		return
	case errors.Is(err, errVoiceShareStale):
		writeVoiceShareStale(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: stopping a share failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeVoiceShareStale answers a release whose share is no longer the
// caller's. It names nobody: who shares now is in presence, for those who may
// read it.
func writeVoiceShareStale(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodeVoiceShareStale, "that share has already ended")
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
		delete(r.withdrawing, voiceWithdrawKey{principalID, voiceRolePerson})
		delete(r.withdrawing, voiceWithdrawKey{principalID, voiceRoleScreen})
		// A new share makes the end of the last one old news.
		delete(p.shareEnded, voiceShareEndKey{channelID: room.ChannelID, principalID: principalID})
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
	p.grantCompanion(lctx, r, room, principalID)
	if held != nil {
		p.mu.Lock()
		ended := r.retired || r.share != held
		p.mu.Unlock()
		if ended {
			// A pass ended the share (it went idle, say) while it was
			// being granted again: what was just granted is taken back,
			// and the caller is told, as for any share that is gone, to
			// start again.
			p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
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
		// The person's grant went with the room, or is the sweep's to
		// find; the companion was just granted the screen and is taken
		// back at once.
		p.withdrawListedCompanion(ctx, r, principalID)
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
			r.owe(principalID)
		}
		p.mu.Unlock()
		r.order.Unlock()
		p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
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
		p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
		p.forgetRoom(ctx, room.RoomName)
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	return schema.VoiceShareV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(mine.startedAt), Audio: true}, nil
}

// grantCompanion lets principalID's companion, p<id>.screen, publish the
// screen as well, if the last pass listed one in the room: the claim is the
// person's, and the companion is where the screen comes from. Its grant is the
// per-source form, the screen and nothing else (its sound is another step). A
// failure does not undo the claim: it is logged, and the companion is restored
// by the next claim or reconnects on a token that already names the screen. A
// companion the pass has not listed yet is not called for; it connects with
// its own grant, and while the person holds the share the passes leave it
// alone. The caller holds the room's claims. Nothing it logs is a token or a
// room name.
func (p *voicePoller) grantCompanion(ctx context.Context, r *voiceRoomState, room store.VoiceRoom, principalID int64) {
	p.mu.Lock()
	c := r.companions[principalID]
	p.mu.Unlock()
	if !c.screen {
		return
	}
	// Its own canSubscribe, as the pass listed it: LiveKit replaces the
	// permission whole, and a publish-only companion must not be handed the
	// right to receive everyone's voice by being given its screen.
	identity := voiceParticipantIdentity(principalID, voiceRoleScreen)
	perm := livekit.SourcePermission(c.screenSubscribes, true, livekit.SourceScreenShare)
	if _, err := p.s.lk.UpdateParticipant(ctx, room.RoomName, identity, perm); err != nil {
		slog.WarnContext(ctx, "voice: granting the screen to a companion failed; the next claim restores it", "channel", room.ChannelID, "principal", principalID, "error", err)
	}
}

// withdrawCompanion takes everything away from principalID's participant in
// role (a companion or a viewer): canPublish off and no sources, and
// canSubscribe as the participant has it (canSubscribe), never more. It is not
// the microphone-only bundle withdrawLocked leaves a person, which would let
// the companion publish a microphone. The caller holds the room's claims. It
// reports whether LiveKit answered.
func (p *voicePoller) withdrawCompanion(ctx context.Context, roomName string, channelID, principalID int64, role voiceRole, canSubscribe bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	identity := voiceParticipantIdentity(principalID, role)
	if _, err := p.s.lk.UpdateParticipant(ctx, roomName, identity, livekit.SourcePermission(canSubscribe, false)); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing a companion's screen-share sources failed; passes retry", "channel", channelID, "principal", principalID, "participant", role.label(), "error", err)
		return false
	}
	return true
}

// withdrawListedCompanion withdraws principalID's companion if the last pass
// listed one, and confirms it. A companion no pass has listed is the passes'.
func (p *voicePoller) withdrawListedCompanion(ctx context.Context, r *voiceRoomState, principalID int64) {
	p.mu.Lock()
	c := r.companions[principalID]
	p.mu.Unlock()
	if !c.screen {
		return
	}
	if p.withdrawCompanion(ctx, r.name, r.channelID, principalID, voiceRoleScreen, c.screenSubscribes) {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRoleScreen})
	}
}

// withdrawAll is the one withdrawal every stop and rollback makes: the
// person's own connection (withdrawLocked) and, if the last pass listed one,
// their companion, so that none of them keeps a screen grant after the share
// is gone, with or without a pass to follow. The caller holds the room's claims.
func (p *voicePoller) withdrawAll(ctx context.Context, r *voiceRoomState, principalID int64, canPublish bool) {
	if p.withdrawLocked(ctx, r.name, r.channelID, principalID, canPublish) {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRolePerson})
	}
	p.withdrawListedCompanion(ctx, r, principalID)
}

// undoClaim takes back a claim that was never granted. Whatever LiveKit may
// have granted before failing to say so is left for the passes, which
// withdraw it without auditing it (withdrawing).
func (p *voicePoller) undoClaim(r *voiceRoomState, mine *voiceShare) {
	r.order.Lock()
	p.mu.Lock()
	if r.share == mine {
		r.share = nil
		r.owe(mine.principalID)
	}
	p.mu.Unlock()
	r.order.Unlock()
}

// stopShare ends room's share for callerID: their own, or, for an operator,
// anyone's. A room with no share is left as it is. The share leaves presence
// and its row is written before LiveKit is asked to withdraw the sources; a
// withdrawal that fails is retried by the passes, and a row that cannot be
// written is logged: a stop is not refused for the sake of its record.
//
// claim, when not nil, is the started_at of the share the caller means: their
// own. Unless the room's share is that one, held by the caller, nothing is
// changed and errVoiceShareStale is returned, whoever the caller is; a
// release that names its share is never a moderation stop.
func (p *voicePoller) stopShare(ctx context.Context, room store.VoiceRoom, callerID int64, operator bool, claim *schema.Timestamp) error {
	r := p.roomNamed(room.RoomName)
	if r == nil {
		if claim != nil {
			return errVoiceShareStale
		}
		return nil
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	r.order.Lock()
	p.mu.Lock()
	c := r.share
	stale := claim != nil && (r.retired || c == nil || c.principalID != callerID || !c.startedAt.Equal(claim.Time()))
	if stale || r.retired || c == nil {
		p.mu.Unlock()
		r.order.Unlock()
		if stale {
			return errVoiceShareStale
		}
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
	r.owe(c.principalID)
	stopped := p.shareStoppedLocked(r.channelID, c, actor, reason)
	p.mu.Unlock()
	p.writeEvents(ctx, stopped)
	r.order.Unlock()
	p.notify(r.channelID)
	// If LiveKit cannot be asked, the passes try again, and remove the
	// sharer from the room if the share is still published and its sources
	// still cannot be withdrawn (withdrawShares): a stopped share does not go
	// on being watched.
	p.withdrawAll(ctx, r, c.principalID, voiceRoomPublishes(room))
	return nil
}

// shareStoppedLocked records that the started share c of channelID ended
// with reason: the sharer's own presence says why from now on (issue #487),
// and the voice_share_stopped row is returned for the caller to write. actor
// is who stopped it, the sharer for every reason but operator; the row names
// them, and presence never does. The caller holds p.mu.
func (p *voicePoller) shareStoppedLocked(channelID int64, c *voiceShare, actor int64, reason schema.VoiceShareEndReason) voiceEvent {
	ended := p.now()
	if ended.Before(c.startedAt) {
		ended = c.startedAt // a clock stepped back says no earlier end than the start
	}
	p.shareEnded[voiceShareEndKey{channelID: channelID, principalID: c.principalID}] = schema.VoiceShareEnded{
		Reason: reason, StartedAt: schema.NewTimestamp(c.startedAt), EndedAt: schema.NewTimestamp(ended),
	}
	return voiceEvent{
		actor: fmt.Sprintf("principal:%d", actor), action: store.AuditVoiceShareStopped, channelID: channelID,
		detail: fmt.Sprintf("channel=%d principal=%d reason=%s", channelID, c.principalID, reason),
	}
}

// endShareLocked ends r's share with reason, if principalID holds it and it
// has started: the share is dropped, the holder's sources are marked to be
// withdrawn, and the row is returned for the caller to write. The caller holds
// r.order and p.mu.
func (p *voicePoller) endShareLocked(r *voiceRoomState, principalID int64, reason schema.VoiceShareEndReason) []voiceEvent {
	c := r.share
	if c == nil || c.pending || c.principalID != principalID {
		return nil
	}
	r.share = nil
	r.owe(principalID)
	return []voiceEvent{p.shareStoppedLocked(r.channelID, c, principalID, reason)}
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

// withdrawConfirmed records that LiveKit has taken the screen-share sources of
// key's participant away, if a withdrawal of theirs was still owed: the entry is stamped
// with the room's eviction count (voiceRoomState.seq), bumped for it. A pass
// that listed the room before that moment may still have seen the share's
// tracks and says nothing of them; one that listed it after and sees a share
// published again (a client rejoining on a token LiveKit sent it during the
// share) audits it as unclaimed.
func (p *voicePoller) withdrawConfirmed(r *voiceRoomState, key voiceWithdrawKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, owed := r.withdrawing[key]; owed {
		r.seq++
		r.withdrawing[key] = r.seq
	}
}

// shareWithdrawal is one participant a pass found with screen-share sources
// they do not hold the share for. sharing says they had a track published:
// if the sources cannot be withdrawn, they are removed from the room instead.
// unclaimed says it is also audited as a share nobody granted. canPublish is
// what LiveKit reported they may publish, which the withdrawal leaves them.
type shareWithdrawal struct {
	principalID int64
	// role is which of the person's participants it is: the person, their
	// companion (whose sources are withdrawn to nothing), or a viewer.
	role         voiceRole
	joinedAt     time.Time
	sharing      bool
	unclaimed    bool
	canPublish   bool
	canSubscribe bool
}

// judgeShareLocked is a pass's part in the screen share, under r.order and
// p.mu, at now. seen is who the pass found entitled, keep whose entitlement
// it could not read, and lost who it found no longer entitled. It ends the
// room's share if its holder has gone, lost access or let it idle, notes what
// the pass saw of the share, and lists everyone else who has a screen-share
// source granted or published: their sources are to be withdrawn, and a
// published one is audited as unclaimed, once until it is gone. It returns the
// rows to write, the withdrawals to make, and whether presence changed.
//
// A share is its holder's, whichever of their participants publishes it: p<id>
// (the browser) or p<id>.screen (the desktop's companion), and it is judged
// published, with or without sound, by the tracks of either. It ends left only
// when p<id> itself is gone: a viewer does not keep it alive.
func (p *voicePoller) judgeShareLocked(r *voiceRoomState, now time.Time, seen map[int64]livekit.Participant, fold voiceFold, keep map[int64]struct{}, lost map[int64]bool, since uint64) ([]voiceEvent, []shareWithdrawal, bool) {
	var events []voiceEvent
	changed := false
	if c := r.share; c != nil && !c.pending {
		pid := c.principalID
		part, here := seen[pid]
		here = here && fold.direct[pid]
		screen := fold.screens[pid] // zero when the person has no companion
		_, undecided := keep[pid]
		switch {
		case undecided:
			// Nothing is known of them on this pass; the next decides.
		case lost[pid]:
			events = append(events, p.endShareLocked(r, pid, voiceShareStopAccessLost)...)
		case !here:
			events = append(events, p.endShareLocked(r, pid, voiceShareStopLeft)...)
		default:
			sharing := part.Sharing() || screen.Sharing()
			audio := part.ScreenShareAudioPublished || screen.ScreenShareAudioPublished
			if sharing {
				c.published, c.lastTrack = true, now
			}
			if c.audio != audio {
				c.audio = audio
				changed = true
			}
			if (!c.published && now.Sub(c.startedAt) >= voiceShareFirstTrack) ||
				(c.published && !sharing && now.Sub(c.lastTrack) >= voiceShareTrackGone) {
				events = append(events, p.endShareLocked(r, pid, voiceShareStopIdle)...)
			}
		}
		if r.share == nil {
			changed = true
		}
	}

	var withdraw []shareWithdrawal
	// consider notes one participant (key is its entry in r.withdrawing) with
	// screen-share sources granted or published that its person does not hold
	// the share for.
	consider := func(pid int64, role voiceRole, part livekit.Participant) {
		key := voiceWithdrawKey{pid, role}
		// The holder's own participants are let be, pending or not; a viewer
		// is never the share's, and publishing anything is unclaimed.
		if role != voiceRoleView && r.share != nil && r.share.principalID == pid {
			return
		}
		stamp, owed := r.withdrawing[key]
		// Still owed, or confirmed after this pass listed the room: what it
		// saw is the share being taken away, not a new one.
		quiet := owed && (stamp == 0 || stamp > since)
		if !part.Sharing() && !part.ShareSourcesGranted {
			if !quiet || stamp == 0 {
				delete(r.withdrawing, key) // nothing left to withdraw
			}
			return
		}
		w := shareWithdrawal{principalID: pid, role: role, joinedAt: part.JoinedAt, sharing: part.Sharing(), canPublish: part.CanPublish, canSubscribe: part.CanSubscribe}
		if part.Sharing() && !quiet {
			// A share nobody granted: impossible through conchd, unless a
			// restart dropped the claim or the client rejoined on a token
			// LiveKit sent it while it held one, after the share ended. A
			// companion's grant names the screen, so it is also what a
			// companion does before its person has claimed.
			w.unclaimed = true
			detail := fmt.Sprintf("channel=%d sources=%s", r.channelID, shareSources(part))
			if role != voiceRolePerson {
				detail += " participant=" + role.label()
			}
			events = append(events, voiceEvent{
				actor: fmt.Sprintf("principal:%d", pid), action: store.AuditVoiceShareUnclaimed, channelID: r.channelID,
				detail: detail + " source=observed",
			})
		}
		if !quiet || stamp == 0 {
			// Owed again until LiveKit confirms. A stamp later than this
			// pass's listing stands: that confirmation is newer than what
			// this pass saw.
			r.withdrawing[key] = 0
		}
		withdraw = append(withdraw, w)
	}
	pids := make([]int64, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	for _, pid := range pids {
		if fold.viaViewer[pid] {
			continue // a viewer, judged with the others below
		}
		consider(pid, voiceRolePerson, seen[pid])
	}
	byRole := func(m map[int64]livekit.Participant, role voiceRole) {
		list := make([]int64, 0, len(m))
		for pid := range m {
			list = append(list, pid)
		}
		slices.Sort(list)
		for _, pid := range list {
			consider(pid, role, m[pid])
		}
	}
	byRole(fold.screens, voiceRoleScreen)
	byRole(fold.views, voiceRoleView)
	// Whoever is gone from the room has nothing left to withdraw; whoever
	// could not be read is looked at again next pass.
	for key := range r.withdrawing {
		here := false
		switch key.role {
		case voiceRoleScreen:
			_, here = fold.screens[key.principalID]
		case voiceRoleView:
			_, here = fold.views[key.principalID]
		default:
			here = fold.direct[key.principalID]
		}
		_, undecided := keep[key.principalID]
		if !here && !undecided {
			delete(r.withdrawing, key)
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
		holds := w.role != voiceRoleView && r.share != nil && r.share.principalID == w.principalID
		gone := r.retired
		p.mu.Unlock()
		if holds || gone {
			continue
		}
		key, done := voiceWithdrawKey{w.principalID, w.role}, false
		if w.role == voiceRolePerson {
			done = p.withdrawLocked(ctx, r.name, r.channelID, w.principalID, w.canPublish)
		} else {
			done = p.withdrawCompanion(ctx, r.name, r.channelID, w.principalID, w.role, w.canSubscribe)
		}
		if done {
			p.withdrawConfirmed(r, key)
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
		part := livekit.Participant{Identity: voiceParticipantIdentity(w.principalID, w.role), JoinedAt: w.joinedAt}
		p.removeObserved(ctx, r, part, w.principalID, reason)
	}
	return again
}
