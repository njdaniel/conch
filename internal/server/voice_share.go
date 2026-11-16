package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
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
	// voiceShareCap is how many screen shares a room holds at once (design
	// docs/design/voice-multi-share.md §4): a constant in code, not a setting
	// and not a schema check, so it can move once measured without a schema
	// change. It counts claims: a pending claim counts, and a holder asking
	// again is not a new one.
	voiceShareCap = 6
)

// capVoiceShareQuality keys the bound on quality changes in the agent
// limiter's table. It is not a manifest capability: nothing declares or
// reads it, it only keeps the bound's entries apart from the agents'.
const capVoiceShareQuality schema.Capability = "voice_share_quality"

// voiceShareQualityLimit bounds a sharer's quality changes (issue #1546):
// three in any three seconds, which is a burst of three and then one a
// second. The first change is never refused. Over it the answer is 429
// rate_limited with Retry-After.
var voiceShareQualityLimit = schema.RateLimit{Capability: capVoiceShareQuality, Max: 3, WindowSeconds: 3}

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
	// quality is what the sharer declared for the stream (issue #1483,
	// design §3): a label repeated in v2 presence, nil when none was
	// declared. Changed only under p.mu, by a claim holding the room's
	// claims.
	quality *schema.VoiceShareQuality
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
	// audioScope is the scope of the holder's recorded consent to the share's
	// sound (issue #898, voice_companion_audio.go), whoever publishes it
	// (audioPublisher), or empty when none is recorded. It is set and
	// cleared under the room's claims as well as order and voicePoller.mu,
	// and it goes with the share: every end of the share ends it, and
	// voice_share_audio_stopped is written beside voice_share_stopped.
	audioScope schema.VoiceShareAudioScope
	// audioPublisher is who the consent names as the sound's publisher
	// (issue #1853): empty means the companion, today's behaviour. Set and
	// cleared with audioScope, under the same locks. Only a consent naming
	// the companion lets the companion's sound be the share's
	// (companionAudio).
	audioPublisher schema.VoiceShareAudioPublisher
	// audioStarted says the consent's voice_share_audio_started row is in
	// the log: set under order once it is written, after LiveKit's grant for
	// a consent naming the companion, or after the row alone for one naming
	// the person (which asks LiveKit for nothing). It stays set while a
	// consent is changed.
	// Only a consent that started is stopped with a row; one whose grant was
	// still in flight when the share ended (a pass ended it idle, a rotation
	// forgot the room) has no started row to close, and its grant is taken
	// back by the consent's own undo.
	audioStarted bool
}

// publisher is the publisher the recorded consent names: the person when the
// consent says so, the companion otherwise. The caller holds p.mu.
func (c *voiceShare) publisher() schema.VoiceShareAudioPublisher {
	if c.audioPublisher == schema.VoiceShareAudioPublisherPerson {
		return schema.VoiceShareAudioPublisherPerson
	}
	return schema.VoiceShareAudioPublisherCompanion
}

// companionAudio says the share carries a consent that names the companion as
// the sound's publisher (issue #1853): the only consent that justifies the
// companion publishing screen_share_audio. A consent naming the person, like no
// consent, leaves a companion's sound unclaimed. The caller holds p.mu.
func (c *voiceShare) companionAudio() bool {
	return c != nil && c.audioScope != "" && c.publisher() == schema.VoiceShareAudioPublisherCompanion
}

// Errors of the share endpoints, each answered as the handlers say.
var (
	// errVoiceShareFull: the room already holds voiceShareCap shares and the
	// caller holds none. 409 voice_share_full, naming nobody.
	errVoiceShareFull = errors.New("voice: the room holds as many shares as it may")
	// errVoiceShareNotConnected: the caller holds a session but LiveKit has no
	// connection of theirs in the room to grant the share to. 409
	// voice_no_session: a share is granted to a connection, and they have
	// none.
	errVoiceShareNotConnected = errors.New("voice: the caller is not connected to the room")
	// errVoiceShareRoomGone: the room was rotated, or the share being claimed
	// again ended, while the claim was being made. 409 voice_no_session.
	errVoiceShareRoomGone = errors.New("voice: the room or the share went away during the claim")
	// errVoiceShareStale: a release named a share that is not the room's
	// current share, or not the caller's. 409 voice_share_stale, and nothing
	// changes.
	errVoiceShareStale = errors.New("voice: the release names a share that is no longer the caller's")
	// errVoiceShareNotChannelRoom: a claim for a room that is not a
	// channel-wide room. Only those rooms have a share (and only in them may
	// every member publish, which withdrawing a share restores). 409
	// voice_no_session, as a transmit report for an audience with no room is.
	errVoiceShareNotChannelRoom = errors.New("voice: only a channel-wide room has a share")
	// errVoiceShareNotAllowed: the caller no longer holds share_screen, as
	// read under the room's claims. 403 voice_not_allowed, the answer the
	// up-front gate (voicePresenceChannel) gives a person without voice.
	errVoiceShareNotAllowed = errors.New("voice: the caller does not hold share_screen")
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
// looked at: a room that holds voiceShareCap shares is 409 voice_share_full to a caller who holds none.
func (s *Server) handleVoiceShareStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceClaimCaller(w, r)
	if !ok {
		return
	}
	// share_screen, on top of use_voice (which voiceSessionCaller read): the
	// role's cell and the person's brake together (design §7.3). The claim
	// reads it again under the room's claims (shareEntitled). A holder who
	// claims again after the right was taken away is refused and loses the
	// share they held, as the claim's own re-read would have done.
	holds, ok := s.canOrFail(w, r, caller, schema.PermShareScreen)
	if !ok {
		return
	}
	if !holds {
		s.endWithheldShare(ctx, caller.ID)
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodeVoiceNotAllowed, "voice is not part of your access to this server",
			refusedPermission(caller, schema.PermShareScreen))
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
	// The body is read after the caller's checks and before the lock: a
	// malformed one is 400, and says nothing to a caller who was refused
	// above. No body declares nothing.
	r.Body = http.MaxBytesReader(w, r.Body, voiceCompanionMaxBytes)
	req, err := schema.DecodeVoiceShareRequestV1(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		// A fixed message: the decoder's own names what the client sent.
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be empty or a voice share request with an optional quality")
		return
	}
	share, err := s.voice.claimShare(ctx, room, caller.ID, req.Quality)
	var limited *rateLimitedError
	switch {
	case errors.Is(err, errVoiceShareFull):
		writeError(w, http.StatusConflict, schema.ErrorCodeVoiceShareFull, fmt.Sprintf("this call already has %d screen shares", voiceShareCap))
		return
	case errors.Is(err, errVoiceShareNotAllowed):
		// The same refusal, word for word, as the up-front checks give; it
		// names use_voice when that is what was taken away since, else
		// share_screen.
		perm := schema.PermShareScreen
		if holds, err := s.can(ctx, caller, schema.PermUseVoice); err != nil || !holds {
			perm = schema.PermUseVoice
		}
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodeVoiceNotAllowed, "voice is not part of your access to this server",
			refusedPermission(caller, perm))
		return
	case errors.As(err, &limited):
		writeRateLimited(w, limited)
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
	// A claim that declared a quality is answered with it (VoiceShareV2); one
	// that did not gets VoiceShareV1, exactly as before, so a client from
	// before the quality never sees a field it does not know.
	var answer interface{ Validate() error } = share
	if req.Quality != nil {
		answer = schema.VoiceShareV2{PrincipalID: share.PrincipalID, StartedAt: share.StartedAt, Audio: share.Audio, Quality: *req.Quality}
	}
	if err := answer.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice: built an invalid share", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleVoiceShareStop serves DELETE /v1/channels/{channel}/voice/share
// [?started_at=…][&principal=<id>]: the sharer stops their own share, or an
// operator or channel manager stops someone's by naming them with principal
// (design docs/design/voice-multi-share.md §5.2). Without principal it is
// always the caller's own stream; there is no "whoever shares" any more.
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
	q := r.URL.Query()
	var claim *schema.Timestamp
	if values, named := q[schema.VoiceShareClaimParam]; named {
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
	// principal names whose stream to stop (design §5.2), on the camera's
	// pattern: absent or the caller's own id is the caller's own stop. It is
	// read, like started_at, before the channel is looked at.
	target := caller.ID
	if values, named := q[schema.VoiceSharePrincipalParam]; named {
		var id int64
		var err error
		if len(values) == 1 {
			id, err = strconv.ParseInt(values[0], 10, 64)
		}
		if len(values) != 1 || err != nil || id <= 0 || values[0] != strconv.FormatInt(id, 10) {
			writeError(w, http.StatusBadRequest, "invalid_request", "principal must be a principal id, given once")
			return
		}
		target = id
	}
	other := target != caller.ID
	if other && claim != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "started_at names your own share; it cannot be given with principal naming someone else")
		return
	}
	var channel store.Channel
	// A moderator holds manage_channels or manages the channel (ADR-014
	// design §7.3).
	moderator, ok := s.canOrFail(w, r, caller, schema.PermManageChannels)
	if !ok {
		return
	}
	// Agents do not moderate voice (issue #1546, voice-control-plane.md
	// §12): an agent that holds manage_channels is checked as a member, so
	// it gets what a plain member gets.
	if caller.Kind != store.PrincipalHuman {
		moderator = false
	}
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
	if other && !moderator {
		// Someone else's stream is a moderator's to stop. A caller who got
		// here is a member of the channel (a non-member was refused 404 above,
		// so this says nothing about other channels), and the answer does not
		// depend on whether the person they named shares, or exists.
		s.denyForbidden(w, r, caller, errForbidden.Code, "only a manager of the channel or an operator can stop someone else's share")
		return
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
	switch err := s.voice.stopShare(ctx, room, caller.ID, target, claim); {
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

// claimRunning records that a claim of principalID's (a share's or a
// camera's, issue #1241: both are made under the room's claims, one at a
// time) is running in r, and returns what the claim defers, to run before it
// lets the claims go. While it runs, a rotation keeps the room findable
// (retiring) and a revocation's endSharesOf or endCamerasOf waits for it. The
// caller holds r's claims.
func (p *voicePoller) claimRunning(ctx context.Context, r *voiceRoomState, principalID int64) func() {
	p.mu.Lock()
	r.claimer = principalID
	done := make(chan struct{})
	r.claimDone = done
	p.mu.Unlock()
	return func() {
		// A withdrawal retireRoom left owing (issue #1087) is paid here, with
		// only the claims held, while claimer is still set, so nothing can
		// record a debt after the last look; claimDone closes after it. A
		// second withdrawal for a principal the claim already withdrew is
		// harmless: withdrawAll is idempotent. Claims are made only in a
		// channel-wide room, where every member may publish.
		paid := false
		for {
			p.mu.Lock()
			owed := r.owedWithdraw
			r.owedWithdraw = nil
			if len(owed) == 0 {
				r.claimer = 0
				r.claimDone = nil
				close(done)
				// Only a claim that was handed the debt closes withdrawDone,
				// and only after paying it: for any other claim it belongs
				// to the retireRoom that opened it, which withdraws after
				// this claim has let go (a claim that finds the room retired
				// refuses at once). Likewise the retiring entry: a claim's
				// own (a principal's id) goes, the sentinel 0 is retireRoom's.
				if paid && r.withdrawDone != nil {
					close(r.withdrawDone)
					r.withdrawDone = nil
				}
				// A withdrawal still owed by someone else (forgetRoom ended a
				// share or cameras while this claim ran, and retireRoom has
				// yet to withdraw them) keeps the room in retiring, as
				// retireRoom's.
				if pid, ok := p.retiring[r]; ok && pid != 0 {
					if r.withdrawDone != nil {
						p.retiring[r] = 0
					} else {
						delete(p.retiring, r)
					}
				}
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()
			for _, pid := range owed {
				p.withdrawAll(context.WithoutCancel(ctx), r, pid, true)
			}
			paid = true
		}
	}
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
func (p *voicePoller) claimShare(ctx context.Context, room store.VoiceRoom, principalID int64, quality *schema.VoiceShareQuality) (schema.VoiceShareV1, error) {
	// The grant below lets the connection publish a microphone as well as
	// the share, which is right only where every member may.
	if !voiceRoomPublishes(room) {
		return schema.VoiceShareV1{}, errVoiceShareNotChannelRoom
	}
	if p.claimSeam != nil {
		p.claimSeam("enter")
	}
	p.mu.Lock()
	r := p.roomLocked(room)
	p.mu.Unlock()
	if p.claimSeam != nil {
		p.claimSeam("lock")
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	defer p.claimRunning(ctx, r, principalID)()

	if p.claimSeam != nil {
		p.claimSeam("claimed")
	}
	r.order.Lock()
	p.mu.Lock()
	if r.retired {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	held := r.shares[principalID]
	if held == nil && len(r.shares) >= voiceShareCap {
		p.mu.Unlock()
		r.order.Unlock()
		// The room is full. The cap is read after the entitlement, so a
		// caller whose right was taken away is told that, not that the room
		// is full; nothing is recorded, written or asked of LiveKit.
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
		defer fcancel()
		if err := p.shareEntitled(fctx, room, principalID); err != nil {
			return schema.VoiceShareV1{}, err
		}
		return schema.VoiceShareV1{}, errVoiceShareFull
	}
	// A holder's repeat that changes the quality fans a presence frame out to
	// every watcher, so it is bounded per principal (issue #1546). It is read
	// before anything is granted or changed, and a refused change leaves the
	// quality as it was. The first change is never refused: the allowance
	// starts full.
	if held != nil && quality != nil && (held.quality == nil || *held.quality != *quality) {
		if d := p.qualityChanges.take(agentLimitKey{principalID, capVoiceShareQuality}, voiceShareQualityLimit, p.now()); !d.allowed {
			p.mu.Unlock()
			r.order.Unlock()
			return schema.VoiceShareV1{}, &rateLimitedError{retryAfter: d.retryAfter}
		}
	}
	mine := held
	if held == nil {
		mine = &voiceShare{principalID: principalID, startedAt: p.now().Truncate(time.Millisecond), pending: true}
		if quality != nil {
			q := *quality
			mine.quality = &q
		}
		r.shares[principalID] = mine
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
	// The entitlement is read again here, under the room's claims, with the
	// claim already recorded (ADR-014, ADR-015 decision 6): the caller's
	// authority was first read before the lock was taken, and a revocation
	// that committed since either finds this claim and waits for it on the
	// claims (endSharesOf), or looked before it was recorded, in which case
	// its commit precedes this read. Either way no grant is written once a
	// revocation has answered. A read that fails is a refusal, not a guess.
	if err := p.shareEntitled(lctx, room, principalID); err != nil {
		if held == nil {
			p.undoClaim(r, mine)
		} else {
			// A holder who lost the right loses the share they hold.
			p.endHeldShare(ctx, r, room, principalID, voiceShareStopAccessLost)
		}
		return schema.VoiceShareV1{}, err
	}
	found, err := p.setPublishPermission(lctx, r, principalID, voiceRolePerson, publishInput{canPublish: true, share: true})
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
		ended := r.retired || r.shares[principalID] != held
		p.mu.Unlock()
		if ended {
			// A pass ended the share (it went idle, say) while it was
			// being granted again: what was just granted is taken back,
			// and the caller is told, as for any share that is gone, to
			// start again.
			p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
			return schema.VoiceShareV1{}, errVoiceShareRoomGone
		}
		// As for a first claim below: a rotation committed while the grant
		// was in flight, and not yet forgotten, is read from the store, and
		// what was just granted is taken back before the claims are let go,
		// so a revocation waiting on them (waitRunningClaims) answers after.
		if gone := p.roomRotatedUnder(ctx, lctx, r, room, principalID, false); gone {
			return schema.VoiceShareV1{}, errVoiceShareRoomGone
		}
		// A holder's repeat with a body changes the declared quality: a
		// presence frame, no row (design §3.1).
		if quality != nil {
			q := *quality
			p.mu.Lock()
			changed := held.quality == nil || *held.quality != q
			held.quality = &q
			p.mu.Unlock()
			if changed {
				p.notify(room.ChannelID)
			}
		}
		return schema.VoiceShareV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(held.startedAt), Audio: true}, nil
	}

	r.order.Lock()
	p.mu.Lock()
	if r.retired || r.shares[principalID] != mine {
		// Rotated while LiveKit was being asked: the room's deletion took
		// the grant with it, and the share never started.
		p.mu.Unlock()
		r.order.Unlock()
		// What was just granted is taken back at once, the person's own
		// grant as well as the companion's: a DeleteRoom that failed leaves
		// the room, and the grant in it, until the sweep. A revocation that
		// rotated the room waits for this in endSharesOf (retiring).
		p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	p.mu.Unlock()
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	err = p.writeEvent(wctx, voiceEvent{
		actor: fmt.Sprintf("principal:%d", principalID), action: store.AuditVoiceShareStarted, channelID: room.ChannelID,
		detail: shareStartedDetail(room.ChannelID, mine.quality), at: mine.startedAt,
	})
	wcancel()
	if err != nil {
		p.mu.Lock()
		if r.shares[principalID] == mine {
			delete(r.shares, principalID)
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
	if gone := p.roomRotatedUnder(ctx, lctx, r, room, principalID, false); gone {
		return schema.VoiceShareV1{}, errVoiceShareRoomGone
	}
	return schema.VoiceShareV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(mine.startedAt), Audio: true}, nil
}

// shareStartedDetail is the detail of a voice_share_started row: the channel,
// the sound the grant allows, and the quality the sharer declared, when they
// did (design §5.6).
func shareStartedDetail(channelID int64, quality *schema.VoiceShareQuality) string {
	detail := fmt.Sprintf("channel=%d audio=true", channelID)
	if quality != nil {
		detail += fmt.Sprintf(" quality=%s/%d", quality.Resolution, quality.FrameRate)
	}
	return detail
}

// roomRotatedUnder reads from the store whether room was rotated while a
// claim's grant was in flight. If it was, what was granted is taken back and
// the room is forgotten (its share ends, reason=rotated), and it reports
// true. A read that fails is a refusal, as for shareEntitled: it cannot be
// told whether the room was rotated, so what was granted is taken back and
// the claim is answered as for a room that went (errVoiceShareRoomGone),
// never 200; the room is left to the sweep. camera says the claim is a
// camera's (issue #1241), not a share's: a read that fails ends only the kind
// being claimed. The caller holds r's claims.
func (p *voicePoller) roomRotatedUnder(ctx, lctx context.Context, r *voiceRoomState, room store.VoiceRoom, principalID int64, camera bool) bool {
	var live bool
	var err error
	if p.liveReadSeam != nil {
		live, err = p.liveReadSeam(lctx, room.ID)
	} else {
		live, err = p.s.store.VoiceRoomLive(lctx, room.ID)
	}
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "voice: could not tell whether a room was retired after a claim; the grant is withdrawn and the sweep checks", "channel", room.ChannelID, "error", err)
		// What the claim recorded ends with the grant (reason=rotated: the
		// room could not be shown live), so nothing is held that has no
		// sources; the withdrawal is made whether or not it was found. Only
		// the kind being claimed: a camera's claim leaves the person's share
		// as it is, and a share's claim their camera.
		if camera {
			if !p.endHeldCamera(ctx, r, principalID, voiceCameraStopRotated) {
				p.withdrawCamera(ctx, r, principalID)
			}
		} else if !p.endHeldShare(ctx, r, room, principalID, voiceShareStopRotated) {
			p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
		}
		p.sweepSoon()
		return true
	case !live:
		// The room is forgotten first, so that it is retired before anything
		// is written: the camera term then reads nothing held, and no
		// withdrawal below grants again what the rotation ends (issue #1241).
		// Forgetting ends every claim in the room (reason=rotated) and names
		// whose grants are left; the claimer's is taken back with theirs, under
		// the claims this claim holds. If another path forgot the room first,
		// what it owes is this claim's to pay (claimRunning).
		forgot, holders := p.forgetRoom(ctx, room.RoomName)
		if !slices.Contains(holders, principalID) {
			holders = append(holders, principalID)
		}
		for _, pid := range holders {
			p.withdrawAll(ctx, r, pid, voiceRoomPublishes(room))
		}
		if forgot != nil {
			// This claim's own forget made the withdrawals: what it opened
			// for them (forgetRoom, ended cameras) is closed.
			p.withdrawFinished(r)
		}
		return true
	}
	return false
}

// shareEntitled reads, under the room's claims, everything a share in room
// rests on (issue #1061), so a revocation that committed before this read is
// seen by it:
//   - share_screen, from the permissions document and the person's brakes
//     (the store's permittedTx, as can does for a handler):
//     errVoiceShareNotAllowed when it is not held;
//   - the person's place in the room, as the passes read it
//     (voiceEntitlement: disabled, a guest whose visit ended, voice off, not
//     a channel member) and the room still live (a rotation commits its
//     retirement before forgetting the room): errVoiceShareRoomGone, the
//     answer a claim in a room that has gone gets.
//
// A read that fails is an error too, never a yes.
func (p *voicePoller) shareEntitled(ctx context.Context, room store.VoiceRoom, principalID int64) error {
	ok, err := p.s.store.Permitted(ctx, principalID, schema.PermShareScreen)
	if err != nil {
		return fmt.Errorf("voice: read share_screen: %w", err)
	}
	if !ok {
		return errVoiceShareNotAllowed
	}
	_, reason, err := p.s.voiceEntitlement(ctx, voiceIdentity(principalID), room.ChannelID)
	switch {
	case err != nil:
		return fmt.Errorf("voice: read the sharer's place: %w", err)
	case reason == voiceReasonVoiceOff:
		return errVoiceShareNotAllowed
	case reason != "":
		return errVoiceShareRoomGone
	}
	live, err := p.s.store.VoiceRoomLive(ctx, room.ID)
	switch {
	case err != nil:
		return fmt.Errorf("voice: read the room: %w", err)
	case !live:
		return errVoiceShareRoomGone
	}
	return nil
}

// sharesWithheld ends r's share when its holder no longer holds share_screen:
// the per-pass half of the right (design §7.3, should-fix 4), which reads the
// document on every pass as voiceEntitlement reads use_voice, so a change
// that missed its hook (a restart, a failed call) is still found within one
// pass. It ends the share through the stop path (endSharesOf: under the
// room's claims, audited access_lost, the sources withdrawn). A read that
// fails ends it too: the right is not known, and a sharer can claim again. A
// pass that was cancelled decides nothing. Nothing is held when it is
// called.
func (p *voicePoller) sharesWithheld(ctx context.Context, r *voiceRoomState) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	holders := sortedShareHolders(r)
	p.mu.Unlock()
	// Every holder is read on its own: one person's loss ends their share
	// and no other's.
	for _, holder := range holders {
		ok, err := p.s.store.Permitted(ctx, holder, schema.PermShareScreen)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.ErrorContext(ctx, "voice: read share_screen failed; ending the share", "channel", r.channelID, "principal", holder, "error", err)
		case ok:
			continue
		}
		p.endSharesOf(ctx, holder, r.channelID, voiceShareStopAccessLost)
	}
}

// sortedShareHolders is r's share holders, started or pending, in id order.
// The caller holds p.mu.
func sortedShareHolders(r *voiceRoomState) []int64 {
	pids := make([]int64, 0, len(r.shares))
	for pid := range r.shares {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids
}

// endSharesOf is the stop path of every revocation of share_screen or of a
// place in a room (the permissions PUT, the voice switch, membership loss,
// disabling): it ends principalID's share in every room the poller holds one
// for them, only channelID's when that is not 0, with reason: the
// voice_share_stopped row is written and the sources are withdrawn. It takes
// each room's claims, so a claim in flight is waited for and then ended, and a
// claim recorded after this has looked meets the revocation, already
// committed, in its own re-read (shareEntitled). It then waits for the
// withdrawal of any share of theirs a rotation ended in a room still
// retiring (retiredShare). Call it after the revocation is committed and
// before it is answered, with no lock held.
//
// Lock order: the room's claims, then its order, then p.mu, as every claim
// and stop takes them. Nothing here is called with any of them held, the
// rooms are taken one at a time in channel order, and the waits hold none.
//
//nolint:unparam // every revocation ends a share as access_lost today; the reason is the stop path's own (voice_share_stopped), kept for its other ends
func (p *voicePoller) endSharesOf(ctx context.Context, principalID, channelID int64, reason schema.VoiceShareEndReason) {
	p.mu.Lock()
	var rooms []*voiceRoomState
	for _, r := range p.rooms {
		theirs := r.shares[principalID] != nil || r.claimer == principalID
		if theirs && (channelID == 0 || r.channelID == channelID) {
			rooms = append(rooms, r)
		}
	}
	// A retired room with a claim of theirs still running: it is no longer in
	// rooms, but the claim may be inside LiveKit's answer, and is waited for
	// too.
	for r, pid := range p.retiring {
		if pid == principalID && (channelID == 0 || r.channelID == channelID) && !slices.Contains(rooms, r) {
			rooms = append(rooms, r)
		}
	}
	p.mu.Unlock()
	if p.endSharesSeam != nil {
		p.endSharesSeam(len(rooms))
	}
	// A revocation is not cancelled by a caller who hangs up.
	ctx = context.WithoutCancel(ctx)
	slices.SortFunc(rooms, func(a, b *voiceRoomState) int {
		return cmp.Or(cmp.Compare(a.channelID, b.channelID), cmp.Compare(a.name, b.name))
	})
	for _, r := range rooms {
		r.claims.Lock()
		p.endHeldShare(ctx, r, store.VoiceRoom{ChannelID: r.channelID, RoomName: r.name}, principalID, reason)
		r.claims.Unlock()
	}
	// A room a rotation forgot, whose ended share of theirs is still being
	// withdrawn (retireRoom, or a running claim that was handed the debt):
	// there is no share left to end in it, but the revocation must not
	// answer while the grant is in flight (S3, issue #1265). It is looked for
	// now, after the rooms above, so that a room forgotten after the first
	// look (whose claims this took before retireRoom did, finding nothing to
	// end) is found too: a forget that ends a share keeps the room in
	// retiring, with withdrawDone open, until the withdrawal is made
	// (forgetRoom, retireRoom, claimRunning). Its done channels are waited
	// for, as waitRunningClaims waits for them, each closed by a withdrawal
	// bounded by LiveKit's call timeouts.
	var waits []chan struct{}
	p.mu.Lock()
	for r := range p.retiring {
		if !slices.Contains(r.retiredShares, principalID) || (channelID != 0 && r.channelID != channelID) {
			continue
		}
		if r.withdrawDone != nil {
			waits = append(waits, r.withdrawDone)
		}
		if r.claimDone != nil {
			waits = append(waits, r.claimDone)
		}
	}
	p.mu.Unlock()
	for _, d := range waits {
		<-d
	}
}

// endHeldShare ends r's share if principalID holds it, as a stop does: the
// share leaves presence and its row is written before LiveKit is asked to
// withdraw the sources (a failure is retried by the passes, which remove the
// sharer if the share is still published). room is r's channel-wide room. The
// caller holds r's claims, so there is no pending claim to skip. It reports
// whether it ended a share.
func (p *voicePoller) endHeldShare(ctx context.Context, r *voiceRoomState, room store.VoiceRoom, principalID int64, reason schema.VoiceShareEndReason) bool {
	r.order.Lock()
	p.mu.Lock()
	events := p.endShareLocked(r, principalID, reason)
	p.mu.Unlock()
	p.writeEvents(ctx, events...)
	r.order.Unlock()
	if len(events) == 0 {
		return false
	}
	p.notify(r.channelID)
	p.withdrawAll(ctx, r, principalID, voiceRoomPublishes(room))
	return true
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
	// The share's sound only with the consent the share carries (issue
	// #898): a claim restores the screen by itself, never the audio. A
	// holder claiming again keeps the consent they recorded, so their
	// companion (perhaps a new connection) is restored to what they hold.
	sh := r.shares[principalID]
	audio := sh.companionAudio()
	p.mu.Unlock()
	if !c.screen {
		return
	}
	// Its own canSubscribe, as the pass listed it: LiveKit replaces the
	// permission whole, and a publish-only companion must not be handed the
	// right to receive everyone's voice by being given its screen.
	if _, err := p.setPublishPermission(ctx, r, principalID, voiceRoleScreen, publishInput{canSubscribe: c.screenSubscribes, share: true, audio: audio}); err != nil {
		slog.WarnContext(ctx, "voice: granting the screen to a companion failed; the passes retry what it may still hold", "channel", room.ChannelID, "principal", principalID, "error", err)
		if !audio {
			// The write may have been refused with an earlier grant's
			// sound still on the companion (a claim just dropped the
			// withdrawal the last share's end owed): that is owed again,
			// and the passes retry it quietly until LiveKit confirms.
			p.mu.Lock()
			if r.shares[principalID] == sh && !r.retired {
				r.withdrawing[voiceWithdrawKey{principalID, voiceRoleScreen}] = 0
			}
			p.mu.Unlock()
		}
	}
}

// withdrawCompanion takes everything away from principalID's participant in
// role (a companion or a viewer): canPublish off and no sources, and
// canSubscribe as the participant has it (canSubscribe), never more. It is not
// the microphone-only bundle withdrawLocked leaves a person, which would let
// the companion publish a microphone. Only a companion or a viewer is passed:
// for a person, publishPermission would write the microphone bundle, not a
// per-source permission, so a caller must use withdrawLocked for one. The caller
// holds the room's claims. It reports whether LiveKit answered.
func (p *voicePoller) withdrawCompanion(ctx context.Context, r *voiceRoomState, principalID int64, role voiceRole, canSubscribe bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	if _, err := p.setPublishPermission(ctx, r, principalID, role, publishInput{canSubscribe: canSubscribe}); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing a companion's screen-share sources failed; passes retry", "channel", r.channelID, "principal", principalID, "participant", role.label(), "error", err)
		return false
	}
	return true
}

// withdrawCompanionNow revokes principalID's companion, p<id>.screen, whether
// or not any pass has listed it: one that connected and began publishing since
// the last pass must not go on streaming after a stop has been answered. It
// looks the one participant up first, so that the revocation leaves its
// canSubscribe as it is (LiveKit replaces a permission whole):
//
//   - nobody by that name: nothing to revoke, and nothing is audited;
//   - found: publishing is revoked with its current canSubscribe; a revoke
//     LiveKit answers not_found counts as done;
//   - the lookup failed: publishing is revoked with canSubscribe false, the
//     conservative choice;
//   - the revoke failed: if the companion is known to have published, or its
//     state could not be learned, it is removed from the room (fail closed,
//     as the passes do), and that is audited as a removal; otherwise the
//     passes, which still owe the withdrawal, retry.
//
// The caller holds the room's claims.
func (p *voicePoller) withdrawCompanionNow(ctx context.Context, r *voiceRoomState, principalID int64) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	identity := voiceParticipantIdentity(principalID, voiceRoleScreen)
	part, found, err := p.s.lk.GetParticipant(lctx, r.name, identity)
	if err == nil && !found {
		return
	}
	known := err == nil
	if !known {
		slog.WarnContext(lctx, "voice: looking up a companion to revoke it failed; revoking without its subscribe permission", "channel", r.channelID, "principal", principalID, "error", err)
	}
	if p.withdrawCompanion(ctx, r, principalID, voiceRoleScreen, known && part.CanSubscribe) {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRoleScreen})
		return
	}
	if known && !part.Sharing() {
		return // nothing is being streamed; the passes retry the withdrawal
	}
	if !known {
		part = livekit.Participant{Identity: identity}
	}
	p.removeObserved(ctx, r, part, principalID, voiceReasonShareNotWithdrawn)
}

// withdrawAll is the one withdrawal every stop and rollback makes: the
// person's own connection (withdrawLocked) and
// their companion (looked up, listed or not), so that none of them keeps a screen grant after the share
// is gone, with or without a pass to follow. The caller holds the room's claims.
func (p *voicePoller) withdrawAll(ctx context.Context, r *voiceRoomState, principalID int64, canPublish bool) {
	if p.withdrawLocked(ctx, r, principalID, canPublish) {
		p.withdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRolePerson})
	}
	p.withdrawCompanionNow(ctx, r, principalID)
}

// undoClaim takes back a claim that was never granted. Whatever LiveKit may
// have granted before failing to say so is left for the passes, which
// withdraw it without auditing it (withdrawing).
func (p *voicePoller) undoClaim(r *voiceRoomState, mine *voiceShare) {
	r.order.Lock()
	p.mu.Lock()
	if r.shares[mine.principalID] == mine {
		delete(r.shares, mine.principalID)
		r.owe(mine.principalID)
	}
	p.mu.Unlock()
	r.order.Unlock()
}

// stopShare ends targetID's share in room for callerID: their own (targetID
// is callerID), or, for a moderator, anyone's. Whether callerID may stop
// someone else's is the handler's to decide (handleVoiceShareStop), before
// this is called; this does not ask again. A person with no share is left
// as they are. The share leaves presence
// and its row is written before LiveKit is asked to withdraw the sources; a
// withdrawal that fails is retried by the passes, and a row that cannot be
// written is logged: a stop is not refused for the sake of its record.
//
// claim, when not nil, is the started_at of the share the caller means: their
// own (targetID is callerID then). Unless the caller's share is that one,
// nothing is changed and errVoiceShareStale is returned, whoever the caller is; a
// release that names its share is never a moderation stop.
func (p *voicePoller) stopShare(ctx context.Context, room store.VoiceRoom, callerID, targetID int64, claim *schema.Timestamp) error {
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
	c := r.shares[targetID]
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
		reason, actor = voiceShareStopOperator, callerID
	}
	delete(r.shares, c.principalID)
	r.owe(c.principalID)
	stopped := p.shareStoppedLocked(r.channelID, c, actor, reason)
	p.mu.Unlock()
	p.writeEvents(ctx, stopped...)
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
// and the rows are returned for the caller to write: voice_share_audio_stopped
// first, when the share carried a consent to the companion's sound (issue
// #898; the consent ends with the share, for the share's reason, once), then
// voice_share_stopped. actor is who stopped it, the sharer for every reason
// but operator; the rows name them, and presence never does. The caller holds
// p.mu.
func (p *voicePoller) shareStoppedLocked(channelID int64, c *voiceShare, actor int64, reason schema.VoiceShareEndReason) []voiceEvent {
	ended := p.now()
	if ended.Before(c.startedAt) {
		ended = c.startedAt // a clock stepped back says no earlier end than the start
	}
	p.shareEnded[voiceShareEndKey{channelID: channelID, principalID: c.principalID}] = schema.VoiceShareEnded{
		Reason: reason, StartedAt: schema.NewTimestamp(c.startedAt), EndedAt: schema.NewTimestamp(ended),
	}
	var events []voiceEvent
	if c.audioStarted {
		events = append(events, voiceEvent{
			actor: fmt.Sprintf("principal:%d", actor), action: store.AuditVoiceShareAudioStopped, channelID: channelID,
			detail: fmt.Sprintf("channel=%d principal=%d reason=%s", channelID, c.principalID, reason),
		})
	}
	c.audioScope, c.audioPublisher, c.audioStarted = "", "", false
	return append(events, voiceEvent{
		actor: fmt.Sprintf("principal:%d", actor), action: store.AuditVoiceShareStopped, channelID: channelID,
		detail: fmt.Sprintf("channel=%d principal=%d reason=%s", channelID, c.principalID, reason),
	})
}

// endShareLocked ends r's share with reason, if principalID holds it and it
// has started: the share is dropped, the holder's sources are marked to be
// withdrawn, and the row is returned for the caller to write. The caller holds
// r.order and p.mu.
func (p *voicePoller) endShareLocked(r *voiceRoomState, principalID int64, reason schema.VoiceShareEndReason) []voiceEvent {
	c := r.shares[principalID]
	if c == nil || c.pending {
		return nil
	}
	delete(r.shares, principalID)
	r.owe(principalID)
	return p.shareStoppedLocked(r.channelID, c, principalID, reason)
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
func (p *voicePoller) withdrawLocked(ctx context.Context, r *voiceRoomState, principalID int64, canPublish bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	if _, err := p.setPublishPermission(ctx, r, principalID, voiceRolePerson, publishInput{canPublish: canPublish}); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing screen-share sources failed; passes retry", "channel", r.channelID, "principal", principalID, "error", err)
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
	// audioOnly is the holder's own companion holding or publishing the
	// share's sound without a consent naming it (issue #898): only
	// screen_share_audio is withdrawn, the screen is kept, and it is made
	// only while the person still holds the share without such a consent,
	// checked again under the claims. It is queued when the sound is
	// published (unclaimed unless owed), and, quietly and with sharing
	// false, whenever a withdrawal is owed though no sound is published
	// (issue #1859), until LiveKit confirms it.
	audioOnly bool
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
	for _, pid := range sortedShareHolders(r) {
		c := r.shares[pid]
		if c.pending {
			continue
		}
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
			// The companion's sound is the share's only under the holder's
			// consent (issue #898); without it the track is unclaimed and
			// being withdrawn (consider below), not the share's audio.
			audio := part.ScreenShareAudioPublished || (screen.ScreenShareAudioPublished && c.companionAudio())
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
		if r.shares[pid] == nil {
			changed = true
		}
	}

	var withdraw []shareWithdrawal
	// consider notes one participant (key is its entry in r.withdrawing) with
	// screen-share sources granted or published that its person does not hold
	// the share for.
	consider := func(pid int64, role voiceRole, part livekit.Participant) {
		key := voiceWithdrawKey{pid, role}
		stamp, owed := r.withdrawing[key]
		// Still owed, or confirmed after this pass listed the room: what it
		// saw is the share being taken away, not a new one.
		quiet := owed && (stamp == 0 || stamp > since)
		// The holder's own participants are let be, pending or not; a viewer
		// is never the share's, and publishing anything is unclaimed.
		if holder := r.shares[pid]; role != voiceRoleView && holder != nil {
			if role != voiceRoleScreen {
				return
			}
			// Except the holder's companion publishing the share's sound
			// with no consent recorded (issue #898): a stale token that
			// still names screen_share_audio after a withdrawal. That one
			// source is unclaimed: it is withdrawn, the screen kept, and
			// audited once until it is gone, as a whole share is. While the
			// share is held, the companion's entry in withdrawing is this
			// audio withdrawal's; a claim clears it and the share's end
			// owes a whole one.
			if holder.companionAudio() {
				if !quiet || stamp == 0 {
					delete(r.withdrawing, key) // consented
				}
				return
			}
			if !part.ScreenShareAudioPublished {
				if quiet && stamp == 0 {
					// A withdrawal conchd owes (a consent that moved to the
					// person, or was ended) that LiveKit has not confirmed:
					// the companion may still hold the audio source though
					// no sound is published, so it is retried, quietly, on
					// every pass until LiveKit takes it. Not sharing: a
					// failure removes no one, as there is no sound to stop.
					withdraw = append(withdraw, shareWithdrawal{principalID: pid, role: role, joinedAt: part.JoinedAt, canSubscribe: part.CanSubscribe, audioOnly: true})
				} else if !quiet {
					delete(r.withdrawing, key) // nothing left to withdraw
				}
				return
			}
			w := shareWithdrawal{principalID: pid, role: role, joinedAt: part.JoinedAt, sharing: true, canSubscribe: part.CanSubscribe, audioOnly: true}
			if !quiet {
				w.unclaimed = true
				events = append(events, voiceEvent{
					actor: fmt.Sprintf("principal:%d", pid), action: store.AuditVoiceShareUnclaimed, channelID: r.channelID,
					detail: fmt.Sprintf("channel=%d sources=screen_share_audio participant=%s source=observed", r.channelID, role.label()),
				})
			}
			if !quiet || stamp == 0 {
				r.withdrawing[key] = 0
			}
			withdraw = append(withdraw, w)
			return
		}
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
		sh := r.shares[w.principalID]
		holds := w.role != voiceRoleView && sh != nil
		consented := holds && sh.companionAudio()
		gone := r.retired
		p.mu.Unlock()
		if gone {
			continue
		}
		if w.audioOnly {
			// Only while the person still holds the share without a consent:
			// a share that ended since owes a whole withdrawal, and a consent
			// recorded since has granted what this would take away.
			if !holds || consented {
				continue
			}
		} else if holds {
			continue
		}
		key, done := voiceWithdrawKey{w.principalID, w.role}, false
		switch {
		case w.audioOnly:
			done = p.withdrawCompanionAudio(ctx, r, w.principalID, w.canSubscribe)
		case w.role == voiceRolePerson:
			done = p.withdrawLocked(ctx, r, w.principalID, w.canPublish)
		default:
			done = p.withdrawCompanion(ctx, r, w.principalID, w.role, w.canSubscribe)
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
