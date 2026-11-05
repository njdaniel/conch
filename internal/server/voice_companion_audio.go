package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The companion's share audio (issue #898, part C of #897;
// docs/design/voice-control-plane.md §13). A companion's grant never names
// screen_share_audio (linux-desktop.md §6.4): a renderer could otherwise
// publish synthesised speech as share audio with no press and no gate. The
// sound is added to the connected companion by a permission update only when
// the share's holder asks through PUT /v1/channels/{channel}/voice/companion/audio,
// which records their consent and its scope, and it is withdrawn by their
// DELETE and by every end of the share (voiceShare.audioScope goes with the
// share; shareStoppedLocked writes voice_share_audio_stopped beside
// voice_share_stopped). A companion found publishing the sound with no consent
// recorded has that one source withdrawn and is audited as unclaimed for it
// (judgeShareLocked). The scope is consent, recorded: both scopes grant the
// same LiveKit source.
//
// Nothing here puts a token, the API secret or a room name in a log line, an
// audit row or a response body.

// handleVoiceCompanionAudio serves
// PUT /v1/channels/{channel}/voice/companion/audio: the share's holder
// consents to their companion publishing the share's sound, by scope.
//
// The checks, in order: 1-5 of the companion route (voiceClaimCaller: a
// verified caller, the channel exists and the caller is a member, the caller
// is human, holds voice, voice is configured; then an archived channel is
// refused), the body (one strictly decoded request, within 1 KiB: 400
// invalid_request, 413 request_too_large), then under the room's claims: the
// caller holds the room's share (held by someone else: 409 voice_share_busy,
// naming nobody; by nobody: 409 voice_no_session) and their p<id>.screen is in
// the room as the last pass listed it (else 409 voice_no_session). Then the
// companion is granted screen_share and screen_share_audio, the
// voice_share_audio_started row is written (actor the person, detail
// channel=<id> scope=<scope>), and the consent is answered as
// VoiceCompanionAudioV1 with the share's started_at.
//
// Asking again with the scope already recorded is a no-op 200: nothing is
// written to LiveKit or the log. Asking with another scope records the new
// one and writes a row saying so; the permission is unchanged, since both
// scopes grant the same source.
func (s *Server) handleVoiceCompanionAudio(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceClaimCaller(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, voiceCompanionMaxBytes)
	req, err := schema.DecodeVoiceCompanionAudioRequestV1(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		// A fixed message: the decoder's own names what the client sent.
		writeError(w, http.StatusBadRequest, "invalid_request", `request body must be a voice companion audio request: a scope of application or system_excluding_call`)
		return
	}

	// Reads only: asking for the share's sound must not create a room.
	room, found, err := s.voiceChannelRoom(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: find the room for a companion's audio failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !found {
		writeVoiceNoSession(w)
		return
	}
	audio, err := s.voice.grantCompanionAudio(ctx, room, caller.ID, req.Scope)
	switch {
	case errors.Is(err, errVoiceShareBusy):
		writeError(w, http.StatusConflict, schema.ErrorCodeVoiceShareBusy, "someone else is sharing in this room")
		return
	case errors.Is(err, errVoiceShareNotConnected), errors.Is(err, errVoiceShareRoomGone):
		writeVoiceNoSession(w)
		return
	case errors.Is(err, livekit.ErrUnavailable):
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable; a companion's audio could not be granted", "channel", channel.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: a companion's audio could not be granted", "channel", channel.ID, "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	if err := audio.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice: built an invalid companion audio answer", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, audio)
}

// handleVoiceCompanionAudioDelete serves
// DELETE /v1/channels/{channel}/voice/companion/audio: the share's holder
// withdraws their consent. Checks 1-5 (voiceSessionCaller: a route that lets
// go keeps working in an archived channel), then the caller's companion is set
// back to screen_share alone and voice_share_audio_stopped (reason=self) is
// written, when a consent of theirs was recorded. 204 either way: a consent
// that is not there, a share that is not the caller's and a room nobody is in
// all leave nothing to withdraw. A withdrawal LiveKit will not take is retried
// by the passes, and a companion still publishing the sound is removed.
func (s *Server) handleVoiceCompanionAudioDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}
	room, found, err := s.voiceChannelRoom(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: find the room for a companion's audio failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if found {
		s.voice.stopCompanionAudio(ctx, room, caller.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// grantCompanionAudio records principalID's consent, with scope, to their
// companion publishing the sound of the share they hold in room, and has
// LiveKit let the companion publish it. Under the room's claims, like a claim
// (claimShare), so that no pass withdraws what is being granted and no stop
// runs between the check and the grant:
//
//   - the room's share is nobody's, or pending: errVoiceShareRoomGone;
//     someone else's: errVoiceShareBusy; the caller's companion is not in the
//     room as the last pass listed it: errVoiceShareNotConnected;
//   - the scope is the one recorded: nothing is done, and the consent is
//     answered;
//   - another scope: the consent is changed and a voice_share_audio_started
//     row says so; the permission already names the source;
//   - a first consent: it is recorded before LiveKit is asked (a pass sees it
//     and leaves the sound alone), the companion is granted the screen and the
//     sound with its own canSubscribe, and the row is written under the room's
//     order, before the answer. If LiveKit refuses, or cannot be asked, or the
//     companion is gone (not found), or the row cannot be written, the consent
//     is taken back and whatever LiveKit may have granted is left owed to the
//     passes, which withdraw it quietly (withdrawing). A share that ended while
//     LiveKit was being asked takes the grant with it: what was granted is
//     withdrawn and the caller is told there is no share. A rotation that
//     committed meanwhile and is not yet forgotten is read from the store
//     (roomRotatedUnder) and ends the share the same way, before any row.
//
// Registered with claimRunning (issue #1265), like a claim: a revocation's
// stop path finds it and waits, and a rotation hands it the withdrawal.
func (p *voicePoller) grantCompanionAudio(ctx context.Context, room store.VoiceRoom, principalID int64, scope schema.VoiceShareAudioScope) (schema.VoiceCompanionAudioV1, error) {
	r := p.roomNamed(room.RoomName)
	if r == nil {
		return schema.VoiceCompanionAudioV1{}, errVoiceShareRoomGone
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	// Registered as a claim (claimRunning, issue #1265): a rotation that
	// forgets the room while the grant is inside LiveKit hands this call the
	// holders' withdrawal, paid on release before the claims are let go, and
	// a revocation's endSharesOf finds the room in retiring and waits for it,
	// so no grant made here lands after a revocation has answered.
	defer p.claimRunning(ctx, r, principalID)()

	r.order.Lock()
	p.mu.Lock()
	c := r.share
	switch {
	case r.retired || c == nil || c.pending:
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceCompanionAudioV1{}, errVoiceShareRoomGone
	case c.principalID != principalID:
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceCompanionAudioV1{}, errVoiceShareBusy
	}
	comp := r.companions[principalID]
	if !comp.screen {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceCompanionAudioV1{}, errVoiceShareNotConnected
	}
	answer := schema.VoiceCompanionAudioV1{Scope: scope, StartedAt: schema.NewTimestamp(c.startedAt)}
	key := voiceWithdrawKey{principalID, voiceRoleScreen}
	row := voiceEvent{
		actor: fmt.Sprintf("principal:%d", principalID), action: store.AuditVoiceShareAudioStarted, channelID: r.channelID,
		detail: fmt.Sprintf("channel=%d scope=%s", r.channelID, scope),
	}
	switch recorded := c.audioScope; {
	case recorded == scope:
		p.mu.Unlock()
		r.order.Unlock()
		return answer, nil
	case recorded != "":
		// The consent changes; what LiveKit grants does not.
		c.audioScope = scope
		p.mu.Unlock()
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
		err := p.writeEvent(wctx, row)
		wcancel()
		if err != nil {
			p.mu.Lock()
			if r.share == c {
				c.audioScope = recorded
			}
			p.mu.Unlock()
			r.order.Unlock()
			return schema.VoiceCompanionAudioV1{}, fmt.Errorf("voice: audit voice_share_audio_started: %w", err)
		}
		r.order.Unlock()
		return answer, nil
	}
	// A first consent: recorded before LiveKit is asked, so that a pass
	// listing the sound meanwhile sees it consented to. Any withdrawal the
	// companion still owed (a DELETE just before) is superseded by the grant.
	c.audioScope = scope
	delete(r.withdrawing, key)
	p.mu.Unlock()
	r.order.Unlock()

	// Not tied to the caller: a grant LiveKit has made is seen through, and
	// bounded, because the claims wait for it.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	undo := func() {
		r.order.Lock()
		p.mu.Lock()
		if r.share == c && c.audioScope == scope {
			c.audioScope = ""
			// Whatever LiveKit may have granted is the passes' to take
			// back, without auditing it: the sound was not published by a
			// stranger.
			r.withdrawing[key] = 0
		}
		p.mu.Unlock()
		r.order.Unlock()
	}
	found, err := p.setPublishPermission(lctx, r, principalID, voiceRoleScreen, publishInput{canSubscribe: comp.screenSubscribes, share: true, audio: true})
	if err != nil || !found {
		undo()
		if err != nil {
			return schema.VoiceCompanionAudioV1{}, err
		}
		return schema.VoiceCompanionAudioV1{}, errVoiceShareNotConnected
	}

	// A rotation committed while the grant was in flight and not yet
	// forgotten is read from the store, as a claim reads it (roomRotatedUnder):
	// the room is forgotten (the share ends, reason=rotated, before any
	// consent row exists) and the holder's grants, the companion's among
	// them, are taken back before the claims are let go, so a revocation
	// waiting on them answers after.
	if gone := p.roomRotatedUnder(ctx, lctx, r, room, principalID, false); gone {
		return schema.VoiceCompanionAudioV1{}, errVoiceShareRoomGone
	}

	r.order.Lock()
	p.mu.Lock()
	if r.retired || r.share != c {
		// The share ended while LiveKit was being asked (a pass ended it
		// idle, say, or a rotation forgot the room): its end owed the
		// companion a whole withdrawal and wrote voice_share_stopped alone,
		// since no voice_share_audio_started row exists to close
		// (audioStarted was never set). The consent is void, and what was
		// just granted is taken back now rather than left to the pass.
		p.mu.Unlock()
		r.order.Unlock()
		if p.withdrawCompanion(ctx, r, principalID, voiceRoleScreen, comp.screenSubscribes) {
			p.withdrawConfirmed(r, key)
		}
		return schema.VoiceCompanionAudioV1{}, errVoiceShareRoomGone
	}
	p.mu.Unlock()
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	err = p.writeEvent(wctx, row)
	wcancel()
	if err == nil {
		// The row is in the log: from here the consent is one that started,
		// and every end of the share closes it with voice_share_audio_stopped.
		p.mu.Lock()
		c.audioStarted = true
		p.mu.Unlock()
	}
	r.order.Unlock()
	if err != nil {
		// A consent conchd cannot account for is not given: the sound is
		// taken back at once, and if LiveKit will not take that, the passes
		// retry it, quietly.
		undo()
		if p.withdrawCompanionAudio(ctx, r, principalID, comp.screenSubscribes) {
			p.withdrawConfirmed(r, key)
		}
		return schema.VoiceCompanionAudioV1{}, fmt.Errorf("voice: audit voice_share_audio_started: %w", err)
	}
	return answer, nil
}

// stopCompanionAudio ends principalID's consent to their companion's sound in
// room, if one is recorded on the share they hold: the voice_share_audio_stopped
// row (reason=self) is written under the room's order, and the companion is
// set back to the screen alone. A room with no such consent is left as it is.
// Under the room's claims, so that no claim or pass writes the companion's
// permission meanwhile. The row is written before LiveKit is asked, as a stop
// does: a withdrawal that fails is retried by the passes (withdrawing), and a
// companion still publishing the sound is removed.
func (p *voicePoller) stopCompanionAudio(ctx context.Context, room store.VoiceRoom, principalID int64) {
	r := p.roomNamed(room.RoomName)
	if r == nil {
		return
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	r.order.Lock()
	p.mu.Lock()
	c := r.share
	// A consent is always one that started here: a grant in flight holds the
	// claims this took.
	if r.retired || c == nil || c.principalID != principalID || c.audioScope == "" || !c.audioStarted {
		p.mu.Unlock()
		r.order.Unlock()
		return
	}
	c.audioScope, c.audioStarted = "", false
	key := voiceWithdrawKey{principalID, voiceRoleScreen}
	r.withdrawing[key] = 0
	row := voiceEvent{
		actor: fmt.Sprintf("principal:%d", principalID), action: store.AuditVoiceShareAudioStopped, channelID: r.channelID,
		detail: fmt.Sprintf("channel=%d principal=%d reason=%s", r.channelID, principalID, voiceShareStopSelf),
	}
	p.mu.Unlock()
	p.writeEvents(ctx, row)
	r.order.Unlock()
	p.withdrawCompanionAudioNow(ctx, r, principalID)
}

// withdrawCompanionAudio sets principalID's companion back to the screen
// alone: canPublish on, screen_share the one source, canSubscribe as the
// participant has it (canSubscribe), never more. For the holder's companion
// only; a companion whose person does not hold the share is withdrawn whole
// (withdrawCompanion). The caller holds the room's claims. It reports whether
// LiveKit answered; a participant who is not there has nothing to withdraw.
func (p *voicePoller) withdrawCompanionAudio(ctx context.Context, r *voiceRoomState, principalID int64, canSubscribe bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	if _, err := p.setPublishPermission(ctx, r, principalID, voiceRoleScreen, publishInput{canSubscribe: canSubscribe, share: true}); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing a companion's share audio failed; passes retry", "channel", r.channelID, "principal", principalID, "error", err)
		return false
	}
	return true
}

// withdrawCompanionAudioNow takes the share's sound away from principalID's
// companion whether or not any pass has listed it, as withdrawCompanionNow
// does for the whole grant: the one participant is looked up first so that
// its canSubscribe is kept (LiveKit replaces a permission whole); nobody by
// that name is nothing to withdraw; a lookup that fails withdraws with
// canSubscribe false, the conservative choice. If the withdrawal fails and the
// companion is known to be publishing the sound, or its state could not be
// learned, it is removed from the room (fail closed), audited as a removal;
// otherwise the passes, which still owe the withdrawal, retry. The caller
// holds the room's claims.
func (p *voicePoller) withdrawCompanionAudioNow(ctx context.Context, r *voiceRoomState, principalID int64) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	identity := voiceParticipantIdentity(principalID, voiceRoleScreen)
	part, found, err := p.s.lk.GetParticipant(lctx, r.name, identity)
	if err == nil && !found {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRoleScreen})
		return
	}
	known := err == nil
	if !known {
		slog.WarnContext(lctx, "voice: looking up a companion to withdraw its share audio failed; withdrawing without its subscribe permission", "channel", r.channelID, "principal", principalID, "error", err)
	}
	if p.withdrawCompanionAudio(ctx, r, principalID, known && part.CanSubscribe) {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRoleScreen})
		return
	}
	if known && !part.ScreenShareAudioPublished {
		return // no sound is being streamed; the passes retry the withdrawal
	}
	if !known {
		part = livekit.Participant{Identity: identity}
	}
	p.removeObserved(ctx, r, part, principalID, voiceReasonShareNotWithdrawn)
}
