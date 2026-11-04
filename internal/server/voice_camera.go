package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Camera claims (ADR-015, issue #1241; docs/design/webcam-video.md §4, §5).
//
// A join token lets its holder publish a microphone and nothing else, so a
// connection never starts with a camera. Turning a camera on is a claim, on
// the share's pattern (voice_share.go) without its one-per-room rule: a person
// in the channel-wide room claims a camera, and conchd has LiveKit let that
// one connection publish the camera source as well, through the one
// permission function (setPublishPermission), so a share's sources and a
// camera's never overwrite each other. A room holds at most voiceCameraCap
// claims. The person stops it, or a holder of manage_channels does; and it
// ends by itself when they leave or lose access, when the room is rotated, or
// when nothing has been sent for a while. Ending a claim takes the source
// away, and LiveKit unpublishes the camera for everyone.
//
// Claims are in memory, per room, as shares are, and a restart drops them.
// Every pass judges every track by its kind and its declared source together
// (§4.5, S1): a camera track nobody claimed, a camera granted to someone
// without a claim, and a track whose type disagrees with its source are
// withdrawn, audited as voice_camera_unclaimed once per track, and their
// publisher removed if LiveKit will not take the track away.
//
// Every write of a claim's state is made under the room's claims (the share's
// lock), and every path that ends one takes them too, so a claim in flight
// re-reads its entitlement under them (S3): no camera is granted after a
// revocation has answered.
//
// Nothing here puts a token, the API secret, a room name, a device name or a
// device id in a log line, an audit row or an answer.

const (
	// voiceCameraCap is how many camera claims one room holds (§4.4, O6): the
	// largest call view. A constant, not a setting and not a schema check
	// (N3): presence reports whatever was granted.
	voiceCameraCap = 9
	// voiceCameraFirstTrack is how long a claim may go without any live
	// camera track being seen before it ends as idle; voiceCameraTrackGone,
	// how long it may go on after the last pass that saw one. The share's
	// figures (§4.5).
	voiceCameraFirstTrack = 15 * time.Second
	voiceCameraTrackGone  = 5 * time.Second
	// voiceCameraAdoptWindow is how long after a rotation ended a claim
	// (reason=rotated) a request naming it may adopt it as a new claim in
	// the same channel (§4.2). A server constant, not published.
	voiceCameraAdoptWindow = 30 * time.Second
	// voiceCameraBurst and voiceCameraRefill are the bound on camera claims
	// per principal (§4.2, O12): a burst of voiceCameraBurst, refilled at one
	// every voiceCameraRefill. Every request that reaches the bound counts: a
	// new claim, a continue, an adoption and an un-named repeat alike (N5).
	// Releases are never bounded.
	voiceCameraBurst  = 10
	voiceCameraRefill = 6 * time.Second
	// voiceCameraRefusalLog is how often a principal's refusals by the bound
	// are logged. They are not audited (§4.7).
	voiceCameraRefusalLog = time.Minute
	// voiceCameraParticipantParam is the desktop phase's parameter of POST
	// (§9, issue C9): it names the companion as the publisher. Until that
	// phase exists it is refused, 400 invalid_request, so a client that
	// sends it is never granted a camera to a participant it did not mean.
	voiceCameraParticipantParam = "participant"
)

// Why a camera claim ended: the reason on voice_camera_stopped, and the one
// the person is told in their own presence (camera_ended).
const (
	voiceCameraStopSelf       = schema.VoiceCameraEndSelf
	voiceCameraStopOperator   = schema.VoiceCameraEndOperator
	voiceCameraStopLeft       = schema.VoiceCameraEndLeft
	voiceCameraStopAccessLost = schema.VoiceCameraEndAccessLost
	voiceCameraStopRotated    = schema.VoiceCameraEndRotated
	voiceCameraStopIdle       = schema.VoiceCameraEndIdle
)

// voiceCamera is one camera claim in a room: whose, since when, and what the
// passes have seen of it. Guarded by voicePoller.mu, and changed only by
// someone who also holds the room's order.
type voiceCamera struct {
	principalID int64
	startedAt   time.Time
	// pending is set from the moment the claim is recorded until its
	// voice_camera_started row is written. Nothing ends a pending claim, and
	// presence does not show it yet; the claim's own grant reads it as held.
	pending bool
	// published says a pass has seen a live camera track of the claim's,
	// and lastTrack when one last did.
	published bool
	lastTrack time.Time
}

// voiceCameraTrack is what the passes know of one track they found unclaimed
// or mismatched (voiceRoomState.camTracks).
type voiceCameraTrack struct {
	// since is the room's eviction count when the first pass to list the
	// track asked LiveKit: a withdrawal confirmed after that and before a
	// later pass's listing that still finds the track did not take it away.
	since uint64
	// ended says the track was first seen while a withdrawal owed for an
	// ended claim was not yet confirmed: the claim's own track going away.
	ended bool
	// audited says its voice_camera_unclaimed row is written.
	audited bool
	// seen is scratch for one pass: the pass listed it.
	seen bool
}

// Errors of the camera claim, each answered as the handler says.
var (
	// errVoiceCameraStale: a named request whose claim is not the caller's
	// current one, and no adoption. 409 voice_camera_stale, nothing changed.
	errVoiceCameraStale = errors.New("voice: the request names a camera claim that is no longer the caller's")
	// errVoiceCameraFull: the room holds voiceCameraCap claims and the caller
	// none. 409 voice_camera_full.
	errVoiceCameraFull = errors.New("voice: the room holds as many cameras as it may")
	// errVoiceCameraNotAllowed: the caller no longer holds use_camera, as
	// read under the room's claims. 403 permission_denied.
	errVoiceCameraNotAllowed = errors.New("voice: the caller does not hold use_camera")
	// errVoiceCameraGone: the room was rotated, or the claim being continued
	// ended, while the claim was being made. 409 voice_no_session.
	errVoiceCameraGone = errors.New("voice: the room or the camera claim went away during the claim")
	// errVoiceCameraRoomRetired: the re-read found the room retired (a
	// rotation committed in the store). 409 voice_no_session, as for any room
	// that went; a holder's claim ends rotated.
	errVoiceCameraRoomRetired = fmt.Errorf("%w: the room was retired", errVoiceCameraGone)
	// errVoiceCameraNotConnected: LiveKit has no connection of the caller's in
	// the room to grant the camera to. 409 voice_no_session.
	errVoiceCameraNotConnected = errors.New("voice: the caller is not connected to the room")
	// errVoiceCameraNotChannelRoom: a camera is claimed only in a
	// channel-wide room. 409 voice_no_session.
	errVoiceCameraNotChannelRoom = errors.New("voice: only a channel-wide room has cameras")
)

// ---------------------------------------------------------------------------
// Routes

// handleVoiceCameraStart serves POST /v1/channels/{channel}/voice/camera
// [?started_at=…]: the caller claims a camera, or continues the claim it
// names (design §4.2, checks 1 to 13, in that order).
//
// The caller is checked exactly as for a share (voiceSessionCaller: a
// verified caller, a member, a human, use_voice, voice configured), then
// use_camera, read before the room's lock so a refused caller never takes it
// (403 permission_denied), then the credential the request came with must
// hold a session for the channel's current channel-wide room (409
// voice_no_session). Then the bound (429), and only then the claim, under the
// room's claims (claimCamera). A refusal writes no row (§4.7).
func (s *Server) handleVoiceCameraStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := callerFrom(ctx); !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return
	}
	// The parameters are read before the channel, so a malformed one says
	// nothing about it.
	q := r.URL.Query()
	if _, named := q[voiceCameraParticipantParam]; named {
		writeError(w, http.StatusBadRequest, "invalid_request", "this server does not take participant: the camera is granted to your own connection")
		return
	}
	claim, ok := voiceCameraClaimParam(w, q)
	if !ok {
		return
	}
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}
	// use_camera (check 5): the role's cell, use_voice and the person's brake
	// together. The claim reads it again under the room's claims
	// (cameraEntitled). A holder who claims again after the right was taken
	// away is refused and loses the camera they held, as the re-read would
	// have done; anyone else holds nothing to end.
	holds, ok := s.canOrFail(w, r, caller, schema.PermUseCamera)
	if !ok {
		return
	}
	if !holds {
		s.voice.endCamerasOf(ctx, caller.ID, channel.ID, voiceCameraStopAccessLost)
		writeVoiceCameraNotAllowed(w, caller)
		return
	}
	credID, ok := credentialIDFrom(ctx)
	if !ok {
		writeVoiceNoSession(w)
		return
	}
	room, held, err := s.voiceHeldRoom(ctx, channel.ID, caller.ID, credID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: holder check for a camera failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	// Check 8: only the channel-wide room, in which every member publishes,
	// has cameras; a net or listen-only grant has none.
	if !held || !voiceRoomPublishes(room) {
		writeVoiceNoSession(w)
		return
	}
	// The bound, after check 8 and before the lock, so that a flood never
	// takes the room's claims (the principal's decision on #1241).
	if !s.voice.cameraClaims.take(caller.ID, s.voice.now()) {
		if s.voice.cameraClaims.logRefusal(caller.ID, s.voice.now()) {
			slog.WarnContext(ctx, "voice: camera claims over the bound are being refused", "principal", caller.ID, "channel", channel.ID)
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(voiceCameraRefill/time.Second)))
		writeError(w, http.StatusTooManyRequests, schema.ErrorCodeVoiceCameraRateLimited, "too many camera requests; wait a few seconds")
		return
	}
	camera, err := s.voice.claimCamera(ctx, room, caller.ID, claim)
	switch {
	case errors.Is(err, errVoiceCameraStale):
		writeVoiceCameraStale(w)
		return
	case errors.Is(err, errVoiceCameraFull):
		writeError(w, http.StatusConflict, schema.ErrorCodeVoiceCameraFull, "this call already has as many cameras on as it can take")
		return
	case errors.Is(err, errVoiceCameraNotAllowed):
		writeVoiceCameraNotAllowed(w, caller)
		return
	case errors.Is(err, errVoiceCameraNotConnected), errors.Is(err, errVoiceCameraGone), errors.Is(err, errVoiceCameraNotChannelRoom):
		writeVoiceNoSession(w)
		return
	case errors.Is(err, livekit.ErrUnavailable):
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable; a camera could not be granted", "channel", channel.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: a camera could not be granted", "channel", channel.ID, "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	if err := camera.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice: built an invalid camera claim", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, camera)
}

// handleVoiceCameraStop serves DELETE /v1/channels/{channel}/voice/camera
// [?started_at=…][&principal=<id>] (design §4.2, N1, N4).
//
// Without principal, or naming the caller, it is the caller's own release,
// checked as a share stop is for anyone but a moderator
// (voicePresenceChannel): a non-member gets the uniform 404. started_at names
// the claim meant; one that is not the caller's current claim is 409
// voice_camera_stale and changes nothing. No claim is 204.
//
// With principal naming someone else it is an operator's stop. The caller's
// standing is decided before the principal is looked at: only manage_channels
// gives it (an owner always holds it), and an operator with it need not be a
// member. Anyone else is refused 403 permission_denied, audited access_denied:
// an operator without it, and a manager of the channel too, which is stricter
// than the share stop (N1, O7). A non-member without standing gets the
// uniform 404 first. With standing, an unknown principal is 204, exactly as
// one with no claim. started_at with principal naming someone else is 400:
// the operator stops the camera, not a claim.
//
// Every parameter is read, and a malformed or repeated one refused 400,
// before the channel is looked at.
func (s *Server) handleVoiceCameraStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return
	}
	q := r.URL.Query()
	claim, ok := voiceCameraClaimParam(w, q)
	if !ok {
		return
	}
	target := caller.ID
	if values, named := q[schema.VoiceCameraPrincipalParam]; named {
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
		writeError(w, http.StatusBadRequest, "invalid_request", "started_at names your own camera; it cannot be given with principal naming someone else")
		return
	}
	var channel store.Channel
	if other {
		standing, ok := s.canOrFail(w, r, caller, schema.PermManageChannels)
		if !ok {
			return
		}
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
		if !standing {
			member, err := s.callerIsMember(r, ch.ID)
			if err != nil {
				slog.ErrorContext(ctx, "voice: check membership failed", "error", err)
				writeInternalError(w)
				return
			}
			if !member {
				s.auditAgentNonMember(r, "", ch.ID)
				writeChannelNotFound(w)
				return
			}
			s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, "only an operator who can manage channels can turn off someone else's camera",
				refusedPermission(caller, schema.PermManageChannels))
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
		slog.ErrorContext(ctx, "voice: find the room of a camera failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !ok {
		// No room, so no claim: nothing to stop, and one a release names is
		// not current.
		if claim != nil {
			writeVoiceCameraStale(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := s.voice.stopCamera(ctx, room, caller.ID, target, claim); {
	case errors.Is(err, errVoiceCameraStale):
		writeVoiceCameraStale(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: stopping a camera failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// voiceCameraClaimParam reads the claim a camera request names
// (schema.VoiceCameraClaimParam): nil when it names none. A malformed or
// repeated value is answered 400 and ok is false.
func voiceCameraClaimParam(w http.ResponseWriter, q url.Values) (*schema.Timestamp, bool) {
	values, named := q[schema.VoiceCameraClaimParam]
	if !named {
		return nil, true
	}
	var started schema.Timestamp
	var err error
	if len(values) == 1 {
		started, err = schema.ParseVoiceCameraClaim(values[0])
	}
	if len(values) != 1 || err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "started_at must be the started_at of your camera, given once")
		return nil, false
	}
	return &started, true
}

// writeVoiceCameraStale answers a request naming a claim that is no longer the
// caller's. It names nobody.
func writeVoiceCameraStale(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodeVoiceCameraStale, "that camera has already been turned off")
}

// writeVoiceCameraNotAllowed answers a caller who does not hold use_camera:
// 403 permission_denied carrying the permission (ADR-014 §7.4; a new code,
// since no existing one asks whether a person may turn on a camera). It
// writes no row: a refused claim is in the answer, not the log (§4.7).
func writeVoiceCameraNotAllowed(w http.ResponseWriter, caller store.Principal) {
	writeJSON(w, http.StatusForbidden, schema.Error{
		Code: schema.ErrorCodePermissionDenied, Message: "turning on a camera is not part of your access to this server",
		Permission: refusedPermission(caller, schema.PermUseCamera),
	})
}

// ---------------------------------------------------------------------------
// The bound

// voiceCameraLimiter is the bound on camera claims: a token bucket per
// principal, voiceCameraBurst claims deep, refilled at one every
// voiceCameraRefill.
type voiceCameraLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*voiceCameraBucket
	pruned  time.Time
}

type voiceCameraBucket struct {
	// credit is the time banked, at most voiceCameraBurst*voiceCameraRefill;
	// a claim spends voiceCameraRefill of it.
	credit time.Duration
	at     time.Time
	// loggedAt is when a refusal was last logged; logged says one was.
	logged   bool
	loggedAt time.Time
}

// take spends one claim of principalID's allowance at now and reports
// whether there was one.
func (l *voiceCameraLimiter) take(principalID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	const full = voiceCameraBurst * voiceCameraRefill
	if l.buckets == nil {
		l.buckets = make(map[int64]*voiceCameraBucket)
	}
	if now.Sub(l.pruned) >= voiceCameraRefusalLog {
		l.pruned = now
		for id, b := range l.buckets {
			if now.Sub(b.at) >= full && (!b.logged || now.Sub(b.loggedAt) >= voiceCameraRefusalLog) {
				delete(l.buckets, id)
			}
		}
	}
	b := l.buckets[principalID]
	if b == nil {
		b = &voiceCameraBucket{credit: full, at: now}
		l.buckets[principalID] = b
	}
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.credit += min(elapsed, full-b.credit)
		b.at = now
	}
	if b.credit < voiceCameraRefill {
		return false
	}
	b.credit -= voiceCameraRefill
	return true
}

// logRefusal reports whether a refusal of principalID's at now is the one to
// log: the first, or the first voiceCameraRefusalLog after the last.
func (l *voiceCameraLimiter) logRefusal(principalID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[principalID]
	if b == nil || (b.logged && now.Sub(b.loggedAt) < voiceCameraRefusalLog) {
		return false
	}
	b.logged, b.loggedAt = true, now
	return true
}

// ---------------------------------------------------------------------------
// The claim

// claimCamera makes or continues principalID's camera claim in room and has
// LiveKit let their connection publish the camera (design §4.2, checks 9 to
// 13). claim is the started_at the request names, or nil.
//
//   - named, and the caller's current claim: a continue. The source is granted
//     again (the connection may be a new one) and the claim held is answered;
//     nothing is written;
//   - named, and the end record of the caller's last claim in this channel says
//     it ended rotated less than voiceCameraAdoptWindow ago: an adoption, a new
//     claim with its own started_at and row; the record is consumed;
//   - named, and anything else: errVoiceCameraStale, nothing changed;
//   - un-named while holding a claim: as a continue;
//   - un-named: a new claim, unless the room holds voiceCameraCap
//     (errVoiceCameraFull).
//
// A new claim is recorded pending before LiveKit is asked, then the
// entitlement is read again under the room's claims (cameraEntitled, S3);
// it stays pending until its voice_camera_started row is written. If LiveKit
// refuses or cannot be asked, or the re-read says no, the claim is undone. A
// room retired during the grant takes the grant back, and the claim never
// follows the channel to its new room.
func (p *voicePoller) claimCamera(ctx context.Context, room store.VoiceRoom, principalID int64, claim *schema.Timestamp) (schema.VoiceCameraV1, error) {
	if !voiceRoomPublishes(room) {
		return schema.VoiceCameraV1{}, errVoiceCameraNotChannelRoom
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

	key := voiceShareEndKey{channelID: room.ChannelID, principalID: principalID}
	r.order.Lock()
	p.mu.Lock()
	if r.retired {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceCameraV1{}, errVoiceCameraGone
	}
	held := r.cameras[principalID]
	// Check 9: a named request continues the caller's current claim, or
	// adopts one the room's rotation ended in this channel, within the
	// window (S2); anything else is stale. An adoption is a new claim in
	// every respect from here on.
	if claim != nil {
		end, ended := p.cameraEnded[key]
		current := held != nil && held.startedAt.Equal(claim.Time())
		adoption := held == nil && ended && end.Reason == voiceCameraStopRotated && end.StartedAt.Time().Equal(claim.Time()) &&
			p.now().Sub(end.EndedAt.Time()) < voiceCameraAdoptWindow
		if !current && !adoption {
			p.mu.Unlock()
			r.order.Unlock()
			return schema.VoiceCameraV1{}, errVoiceCameraStale
		}
	}
	// Check 10: the cap, which a holder's own repeat never meets.
	if held == nil && len(r.cameras) >= voiceCameraCap {
		p.mu.Unlock()
		r.order.Unlock()
		return schema.VoiceCameraV1{}, errVoiceCameraFull
	}
	mine := held
	if held == nil {
		mine = &voiceCamera{principalID: principalID, startedAt: p.now().Truncate(time.Millisecond), pending: true}
		r.cameras[principalID] = mine
		delete(r.camWithdrawing, voiceWithdrawKey{principalID, voiceRolePerson})
		// The end record of the last claim stays until this one has started
		// (check 13): a claim that is refused or undone has adopted nothing,
		// and a retry may still adopt.
	}
	p.mu.Unlock()
	r.order.Unlock()

	// Not tied to the caller: a claim LiveKit has granted is seen through,
	// whatever becomes of the request. Bounded, because the room's claims and
	// the passes' withdrawals wait for it.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	// Check 11: the entitlement read again, under the room's claims, with the
	// claim recorded. A revocation that committed since the first read either
	// finds this claim and waits for it on the claims (endCamerasOf), or
	// looked before it was recorded, in which case its commit precedes this
	// read. Either way no camera is granted once a revocation has answered. A
	// read that fails is a refusal.
	if err := p.cameraEntitled(lctx, room, principalID); err != nil {
		switch {
		case held == nil:
			// Nothing was asked of LiveKit, so nothing is owed: a withdrawal
			// marked owed here would hide the audit of the person's next
			// unclaimed camera (the security review of #1258, NIT 2).
			p.undoCamera(r, mine, false)
		case errors.Is(err, errVoiceCameraRoomRetired):
			// A holder whose room was retired loses the camera with it.
			p.endHeldCamera(ctx, r, principalID, voiceCameraStopRotated)
		default:
			// A holder who lost the right loses the camera they hold.
			p.endHeldCamera(ctx, r, principalID, voiceCameraStopAccessLost)
		}
		return schema.VoiceCameraV1{}, err
	}
	// Check 12: the grant, through the one permission function, which reads
	// the camera term from the claim just recorded; the share term is what the
	// person holds now, so a camera never takes a share away.
	p.mu.Lock()
	share := p.shareHeldLocked(r, principalID)
	p.mu.Unlock()
	found, err := p.setPublishPermission(lctx, r, principalID, voiceRolePerson, publishInput{canPublish: true, share: share})
	if err != nil || !found {
		if held == nil {
			// LiveKit may have granted it before failing to say so: owed.
			p.undoCamera(r, mine, true)
		}
		if err != nil {
			return schema.VoiceCameraV1{}, err
		}
		return schema.VoiceCameraV1{}, errVoiceCameraNotConnected
	}
	if held != nil {
		p.mu.Lock()
		ended := r.retired || r.cameras[principalID] != held
		p.mu.Unlock()
		if ended {
			// A pass ended the claim (idle, say) while it was granted again:
			// what was just granted is taken back.
			p.withdrawCamera(ctx, r, principalID)
			return schema.VoiceCameraV1{}, errVoiceCameraGone
		}
		if gone := p.roomRotatedUnder(ctx, lctx, r, room, principalID, true); gone {
			return schema.VoiceCameraV1{}, errVoiceCameraGone
		}
		return schema.VoiceCameraV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(held.startedAt)}, nil
	}

	// Check 13: the row, and only then is the claim no longer pending.
	r.order.Lock()
	p.mu.Lock()
	if r.retired || r.cameras[principalID] != mine {
		// Rotated while LiveKit was being asked: the claim never started.
		// What was granted is taken back at once (cameraHeldLocked and
		// shareHeldLocked read a retired room as holding nothing).
		p.mu.Unlock()
		r.order.Unlock()
		p.withdrawCamera(ctx, r, principalID)
		return schema.VoiceCameraV1{}, errVoiceCameraGone
	}
	p.mu.Unlock()
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	err = p.writeEvent(wctx, voiceEvent{
		actor: fmt.Sprintf("principal:%d", principalID), action: store.AuditVoiceCameraStarted, channelID: room.ChannelID,
		detail: fmt.Sprintf("channel=%d", room.ChannelID), at: mine.startedAt,
	})
	wcancel()
	if err != nil {
		p.mu.Lock()
		if r.cameras[principalID] == mine {
			delete(r.cameras, principalID)
			r.camWithdrawing[voiceWithdrawKey{principalID, voiceRolePerson}] = 0
		}
		p.mu.Unlock()
		r.order.Unlock()
		p.withdrawCamera(ctx, r, principalID)
		return schema.VoiceCameraV1{}, fmt.Errorf("voice: audit voice_camera_started: %w", err)
	}
	p.mu.Lock()
	mine.pending = false
	// A new claim makes the end of the last one old news; an adoption
	// consumes it, so the same claim is adopted at most once.
	delete(p.cameraEnded, key)
	p.mu.Unlock()
	r.order.Unlock()
	p.notify(room.ChannelID)

	// As for a share: a rotation committed between the holder check and the
	// claim, and not yet forgotten, is read from the store, and the grant is
	// taken back before the claims are let go.
	if gone := p.roomRotatedUnder(ctx, lctx, r, room, principalID, true); gone {
		return schema.VoiceCameraV1{}, errVoiceCameraGone
	}
	return schema.VoiceCameraV1{PrincipalID: principalID, StartedAt: schema.NewTimestamp(mine.startedAt)}, nil
}

// cameraEntitled reads, under the room's claims, everything a camera in room
// rests on (check 11, S3), the camera's shareEntitled:
//   - use_camera, from the permissions document and the person's brakes (the
//     store's permittedTx, which ANDs in use_voice): errVoiceCameraNotAllowed
//     when it is not held;
//   - the person's place in the room, as the passes read it
//     (voiceEntitlement), and the room still live: errVoiceCameraGone.
//
// A read that fails is an error too, never a yes.
func (p *voicePoller) cameraEntitled(ctx context.Context, room store.VoiceRoom, principalID int64) error {
	ok, err := p.s.store.Permitted(ctx, principalID, schema.PermUseCamera)
	if err != nil {
		return fmt.Errorf("voice: read use_camera: %w", err)
	}
	if !ok {
		return errVoiceCameraNotAllowed
	}
	_, reason, err := p.s.voiceEntitlement(ctx, voiceIdentity(principalID), room.ChannelID)
	switch {
	case err != nil:
		return fmt.Errorf("voice: read the camera holder's place: %w", err)
	case reason == voiceReasonVoiceOff:
		return errVoiceCameraNotAllowed
	case reason != "":
		return errVoiceCameraGone
	}
	live, err := p.s.store.VoiceRoomLive(ctx, room.ID)
	switch {
	case err != nil:
		return fmt.Errorf("voice: read the room: %w", err)
	case !live:
		return errVoiceCameraRoomRetired
	}
	return nil
}

// undoCamera takes back a claim that was never granted. owe says LiveKit may
// have granted it before failing to say so: the passes then withdraw it
// without auditing it (camWithdrawing). A claim refused before LiveKit was
// asked owes nothing.
func (p *voicePoller) undoCamera(r *voiceRoomState, mine *voiceCamera, owe bool) {
	r.order.Lock()
	p.mu.Lock()
	if r.cameras[mine.principalID] == mine {
		delete(r.cameras, mine.principalID)
		if owe {
			r.camWithdrawing[voiceWithdrawKey{mine.principalID, voiceRolePerson}] = 0
		}
	}
	p.mu.Unlock()
	r.order.Unlock()
}

// ---------------------------------------------------------------------------
// Ending a claim

// stopCamera ends target's claim in room for callerID: their own (target is
// the caller, reason=self), or, the handler having checked the caller's
// standing, someone else's (reason=operator, the caller the row's actor). A
// room or a person with no claim is left as it is. The claim leaves presence
// and its row is written before LiveKit is asked to withdraw the source; a
// withdrawal that fails is retried by the passes, and the stop is never
// refused for it.
//
// claim, when not nil, is the started_at of the caller's own claim: unless it
// is the current one, nothing is changed and errVoiceCameraStale is returned.
func (p *voicePoller) stopCamera(ctx context.Context, room store.VoiceRoom, callerID, target int64, claim *schema.Timestamp) error {
	r := p.roomNamed(room.RoomName)
	if r == nil {
		if claim != nil {
			return errVoiceCameraStale
		}
		return nil
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	r.order.Lock()
	p.mu.Lock()
	c := r.cameras[target]
	if claim != nil && (r.retired || c == nil || !c.startedAt.Equal(claim.Time())) {
		p.mu.Unlock()
		r.order.Unlock()
		return errVoiceCameraStale
	}
	if r.retired || c == nil {
		p.mu.Unlock()
		r.order.Unlock()
		return nil
	}
	reason, actor := voiceCameraStopSelf, int64(0)
	if target != callerID {
		reason, actor = voiceCameraStopOperator, callerID
	}
	events := p.endCameraLocked(r, target, reason, actor)
	p.mu.Unlock()
	p.writeEvents(ctx, events...)
	r.order.Unlock()
	p.notify(r.channelID)
	p.withdrawCamera(ctx, r, target)
	return nil
}

// endCameraLocked ends principalID's claim in r with reason, if they hold one
// that has started: the claim is dropped, its withdrawal is owed, the person's
// own presence says why from now on, and the voice_camera_stopped row is
// returned for the caller to write. actor is the operator for
// reason=operator; the row's actor is otherwise the person for reason=self
// and the system for every end conchd decided. The caller holds r.order and
// p.mu.
func (p *voicePoller) endCameraLocked(r *voiceRoomState, principalID int64, reason schema.VoiceCameraEndReason, actor int64) []voiceEvent {
	c := r.cameras[principalID]
	if c == nil || c.pending {
		return nil
	}
	delete(r.cameras, principalID)
	r.camWithdrawing[voiceWithdrawKey{principalID, voiceRolePerson}] = 0
	ended := p.now()
	if ended.Before(c.startedAt) {
		ended = c.startedAt // a clock stepped back says no earlier end than the start
	}
	p.cameraEnded[voiceShareEndKey{channelID: r.channelID, principalID: principalID}] = schema.VoiceCameraEnded{
		Reason: reason, StartedAt: schema.NewTimestamp(c.startedAt), EndedAt: schema.NewTimestamp(ended),
	}
	who := "system"
	switch reason {
	case voiceCameraStopSelf:
		who = fmt.Sprintf("principal:%d", principalID)
	case voiceCameraStopOperator:
		who = fmt.Sprintf("principal:%d", actor)
	}
	return []voiceEvent{{
		actor: who, action: store.AuditVoiceCameraStopped, channelID: r.channelID,
		detail: fmt.Sprintf("channel=%d principal=%d reason=%s", r.channelID, principalID, reason),
	}}
}

// endHeldCamera ends principalID's claim in r, if they hold one, as a stop
// does: out of presence and its row written, then the source withdrawn. The
// caller holds r's claims. It reports whether it ended a claim.
func (p *voicePoller) endHeldCamera(ctx context.Context, r *voiceRoomState, principalID int64, reason schema.VoiceCameraEndReason) bool {
	r.order.Lock()
	p.mu.Lock()
	events := p.endCameraLocked(r, principalID, reason, 0)
	p.mu.Unlock()
	p.writeEvents(ctx, events...)
	r.order.Unlock()
	if len(events) == 0 {
		return false
	}
	p.notify(r.channelID)
	p.withdrawCamera(ctx, r, principalID)
	return true
}

// endCamerasOf is the camera's stop path for every revocation of use_camera
// or of a place in a room (the permissions PUT, a role change, the voice
// switch, disabling, membership loss), the twin of endSharesOf: it ends
// principalID's claim in every room the poller holds one or a running claim
// for them, only channelID's when that is not 0, with reason. It takes each
// room's claims, so a claim in flight is waited for and then ended, and a
// claim recorded after this has looked meets the revocation, already
// committed, in its own re-read (cameraEntitled). Call it after the
// revocation is committed and before it is answered, with no lock held.
//
//nolint:unparam // every revocation ends a camera as access_lost today; the reason is the stop path's own (voice_camera_stopped), kept as endSharesOf keeps it
func (p *voicePoller) endCamerasOf(ctx context.Context, principalID, channelID int64, reason schema.VoiceCameraEndReason) {
	p.mu.Lock()
	var rooms []*voiceRoomState
	for _, r := range p.rooms {
		theirs := r.cameras[principalID] != nil || r.claimer == principalID
		if theirs && (channelID == 0 || r.channelID == channelID) {
			rooms = append(rooms, r)
		}
	}
	// A retired room with a claim of theirs still running in it: it is no
	// longer in rooms, but the claim, pending and so not among the ones a
	// forget ends, may be inside LiveKit's answer to its grant. Taking the
	// room's claims waits for it; it finds the room retired and takes its
	// grant back before it lets them go.
	for r, pid := range p.retiring {
		if pid == principalID && (channelID == 0 || r.channelID == channelID) && !slices.Contains(rooms, r) {
			rooms = append(rooms, r)
		}
	}
	p.mu.Unlock()
	if p.endCamerasSeam != nil {
		p.endCamerasSeam(len(rooms))
	}
	ctx = context.WithoutCancel(ctx)
	slices.SortFunc(rooms, func(a, b *voiceRoomState) int {
		return cmp.Or(cmp.Compare(a.channelID, b.channelID), cmp.Compare(a.name, b.name))
	})
	for _, r := range rooms {
		r.claims.Lock()
		p.endHeldCamera(ctx, r, principalID, reason)
		r.claims.Unlock()
	}
	// A room a rotation forgot, whose ended camera of theirs is still being
	// withdrawn (retireRoom, or a running claim that was handed the debt):
	// there is no claim left to end in it, but the revocation must not
	// answer while the grant is in flight (ADR-015 decision 6). It is looked
	// for now, after the rooms above, so that a room forgotten after the
	// first look (whose claims this took before retireRoom did, finding
	// nothing to end) is found too: a forget that ends a camera keeps the
	// room in retiring, with withdrawDone open, until the withdrawal is made
	// (forgetRoom, retireRoom, claimRunning). Its done channels are waited
	// for, as waitRunningClaims waits for them.
	var waits []chan struct{}
	p.mu.Lock()
	for r := range p.retiring {
		if !slices.Contains(r.retiredCameras, principalID) || (channelID != 0 && r.channelID != channelID) {
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
	if p.endCamerasWaitSeam != nil {
		p.endCamerasWaitSeam(len(waits))
	}
	for _, d := range waits {
		<-d
	}
}

// withdrawCamera takes the camera source away from principalID's own
// connection to r, leaving it the microphone and, if they hold it now, the
// share (shareHeldLocked): a camera's stop never drops a share. The camera
// term is read at the write (setPublishPermission), so a claim of theirs
// still held keeps it. Camera claims are made only in a channel-wide room,
// where every member may publish. The caller holds r's claims. A failure is
// logged and left to the passes, which owe the withdrawal until LiveKit
// confirms it (camWithdrawing).
func (p *voicePoller) withdrawCamera(ctx context.Context, r *voiceRoomState, principalID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	p.mu.Lock()
	share := p.shareHeldLocked(r, principalID)
	p.mu.Unlock()
	if _, err := p.setPublishPermission(ctx, r, principalID, voiceRolePerson, publishInput{canPublish: true, share: share}); err != nil {
		slog.WarnContext(ctx, "voice: withdrawing a camera failed; passes retry", "channel", r.channelID, "principal", principalID, "error", err)
		return
	}
	p.camWithdrawConfirmed(r, voiceWithdrawKey{principalID, voiceRolePerson})
}

// camWithdrawConfirmed is withdrawConfirmed for the camera source.
func (p *voicePoller) camWithdrawConfirmed(r *voiceRoomState, key voiceWithdrawKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, owed := r.camWithdrawing[key]; owed {
		r.seq++
		r.camWithdrawing[key] = r.seq
	}
}

// shareHeldLocked is the share term of every write the camera makes (a
// camera's grant, its stop, a pass's camera withdrawal from a person or a
// companion): whether principalID holds r's share at this moment, as started.
// It is the one place that term is worked out for them (the C4a review),
// read under the room's claims at the write. It answers as endShareLocked
// would end a share: a pending share is not one, and a retired room holds
// none, so a write made while a rotation is forgetting the room never grants
// a share back. The caller holds p.mu, and the room's claims.
func (p *voicePoller) shareHeldLocked(r *voiceRoomState, principalID int64) bool {
	return !r.retired && r.share != nil && !r.share.pending && r.share.principalID == principalID
}

// cameraHeldLocked is the camera term of every write: whether principalID
// holds a camera claim in r, pending or started (a pending one is the claim
// whose grant is being written). A retired room holds none. The caller holds
// p.mu, and the room's claims.
func (p *voicePoller) cameraHeldLocked(r *voiceRoomState, principalID int64) bool {
	return !r.retired && r.cameras[principalID] != nil
}

// sortedCameraHolders is r's claim holders in id order. The caller holds p.mu.
func sortedCameraHolders(r *voiceRoomState) []int64 {
	pids := make([]int64, 0, len(r.cameras))
	for pid := range r.cameras {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids
}

// cameraPrincipals is everyone the poller holds a camera claim or a running
// claim for, in a live room or a retiring one, each once, in id order: whom a
// change that may withhold use_camera from a whole column asks again
// (endWithheldCameras). It reads p.mu only.
func (p *voicePoller) cameraPrincipals() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[int64]bool{}
	for _, r := range p.rooms {
		for pid := range r.cameras {
			seen[pid] = true
		}
		if r.claimer != 0 {
			seen[r.claimer] = true
		}
	}
	for r, pid := range p.retiring {
		if pid != 0 {
			seen[pid] = true
		}
		// Whose camera a rotation ended there and may still be withdrawing.
		for _, c := range r.retiredCameras {
			seen[c] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

// camerasWithheld ends r's claims whose holders no longer hold use_camera:
// the per-pass half of the right (design §4.5), the camera twin of
// sharesWithheld, so a change that missed its hook is found within one pass.
// It ends them through the stop path (endCamerasOf). A read that fails ends
// the claim too: the right is not known, and the person can claim again. A
// pass that was cancelled decides nothing. Nothing is held when it is called.
func (p *voicePoller) camerasWithheld(ctx context.Context, r *voiceRoomState) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	var holders []int64
	for _, pid := range sortedCameraHolders(r) {
		if !r.cameras[pid].pending {
			holders = append(holders, pid)
		}
	}
	p.mu.Unlock()
	for _, pid := range holders {
		ok, err := p.s.store.Permitted(ctx, pid, schema.PermUseCamera)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.ErrorContext(ctx, "voice: read use_camera failed; ending the camera", "channel", r.channelID, "principal", pid, "error", err)
		case ok:
			continue
		}
		p.endCamerasOf(ctx, pid, r.channelID, voiceCameraStopAccessLost)
	}
}

// ---------------------------------------------------------------------------
// The pass

// cameraWithdrawal is one participant a pass found with a camera it holds no
// claim for (granted or published), or with a track whose kind and source
// disagree. published says a track the withdrawal would take away is
// published: if the withdrawal fails, they are removed instead. evict says
// a track no withdrawal can take away is published (a mismatched track under
// a source they keep), or one survived a confirmed withdrawal: they are
// removed whatever the withdrawal does. reason is the removal's.
type cameraWithdrawal struct {
	principalID int64
	role        voiceRole
	joinedAt    time.Time
	published   bool
	evict       bool
	// mismatch says a track that is never a claim's is among what was
	// found (a kind that disagrees with its source, or a second camera under
	// one claim): its publisher is removed even if they hold a claim by the
	// time the withdrawal is made.
	mismatch bool
	// granted says LiveKit listed the camera granted to a participant without
	// a claim: a removal is preceded by the withdrawal, so that a token
	// LiveKit sends it does not carry the camera into a rejoin.
	granted      bool
	reason       string
	canSubscribe bool
}

// voiceCameraTrackKey names one track of one connection for camTracks: the
// connection (its identity and join time) and the track (LiveKit's id, empty
// when it gave none, and its declared source and type), so that a rejoin, a
// republish or a second track is a new one.
func voiceCameraTrackKey(part livekit.Participant, t livekit.Track) string {
	return strings.Join([]string{part.Identity, strconv.FormatInt(part.JoinedAt.UnixMilli(), 10), t.SID, t.Source, t.Type}, "\x00")
}

// judgeCamerasLocked is a pass's part in the cameras, under r.order and p.mu,
// at now; its arguments are judgeShareLocked's. It ends each claim whose
// holder has gone (left), lost access (access_lost) or let it idle, notes the
// live camera tracks of the rest, and judges every participant's tracks by
// kind and source together (§4.5, S1): a camera granted or published without
// a claim, a companion's or a viewer's camera, and a mismatched track are to
// be withdrawn, a published one audited once per track, and a track that no
// withdrawal can take away has its publisher removed. It returns the rows,
// the withdrawals, and whether presence changed.
//
// A claim is its holder's and is kept alive by p<id> alone: a companion is
// never granted a camera until the desktop phase (§4.8), so a camera from it
// is unclaimed whatever the claim says.
func (p *voicePoller) judgeCamerasLocked(r *voiceRoomState, now time.Time, seen map[int64]livekit.Participant, fold voiceFold, keep map[int64]struct{}, lost map[int64]bool, since uint64) ([]voiceEvent, []cameraWithdrawal, bool) {
	var events []voiceEvent
	changed := false
	for _, pid := range sortedCameraHolders(r) {
		c := r.cameras[pid]
		if c.pending {
			continue
		}
		part, here := seen[pid]
		here = here && fold.direct[pid]
		_, undecided := keep[pid]
		var ended []voiceEvent
		switch {
		case undecided:
		case lost[pid]:
			ended = p.endCameraLocked(r, pid, voiceCameraStopAccessLost, 0)
		case !here:
			ended = p.endCameraLocked(r, pid, voiceCameraStopLeft, 0)
		default:
			// A muted camera sends nothing, and is not a camera on. A claim
			// covers one camera track, the first LiveKit lists; any other is
			// judged below as one nobody claimed (the security review of
			// #1258: one claim must not carry the cap's worth of cameras).
			live := part.CameraLive()
			if live {
				c.published, c.lastTrack = true, now
			}
			if (!c.published && now.Sub(c.startedAt) >= voiceCameraFirstTrack) ||
				(c.published && !live && now.Sub(c.lastTrack) >= voiceCameraTrackGone) {
				ended = p.endCameraLocked(r, pid, voiceCameraStopIdle, 0)
			}
		}
		if len(ended) > 0 {
			events = append(events, ended...)
			changed = true
		}
	}

	for _, t := range r.camTracks {
		t.seen = false
	}
	var withdraw []cameraWithdrawal
	consider := func(pid int64, role voiceRole, part livekit.Participant) {
		key := voiceWithdrawKey{pid, role}
		// The person's own connection is the only one a claim covers, pending
		// or started.
		holder := role == voiceRolePerson && r.cameras[pid] != nil
		stamp, owed := r.camWithdrawing[key]
		// Still owed, or confirmed after this pass listed the room: what it
		// saw is a claim's camera being taken away, not a new one.
		quiet := owed && (stamp == 0 || stamp > since)
		w := cameraWithdrawal{principalID: pid, role: role, joinedAt: part.JoinedAt, canSubscribe: part.CanSubscribe, reason: voiceReasonCameraUnclaimed, granted: !holder && part.CameraGranted}
		// owes says a withdrawal of this participant's is owed and confirmed
		// (camWithdrawing): for anyone without a claim. A holder's only
		// withdrawable track would be a camera, which its claim keeps.
		owes := !holder
		// mismatched is a track that is never anyone's claim: its kind and
		// source disagree, or it is a second camera under one claim.
		note := func(t livekit.Track, mismatched bool) {
			tk := voiceCameraTrackKey(part, t)
			known := r.camTracks[tk]
			if known == nil {
				known = &voiceCameraTrack{since: since, ended: quiet && !mismatched}
				r.camTracks[tk] = known
			}
			known.seen = true
			if !known.audited && (mismatched || !quiet) {
				known.audited = true
				detail := fmt.Sprintf("channel=%d source=observed track_source=%s track_type=%s", r.channelID, t.Source, t.Type)
				if role != voiceRolePerson {
					detail += " participant=" + role.label()
				}
				events = append(events, voiceEvent{
					actor: fmt.Sprintf("principal:%d", pid), action: store.AuditVoiceCameraUnclaimed, channelID: r.channelID, detail: detail,
				})
			}
			// Withdrawn and confirmed after it was first listed, before this
			// pass listed it, and still there: no withdrawal takes it away.
			if owed && stamp != 0 && stamp > known.since && stamp <= since {
				w.evict = true
				w.mismatch = w.mismatch || mismatched
				if known.ended {
					w.reason = voiceReasonCameraNotWithdrawn
				}
			}
			// Withdrawn first only where the withdrawal is known to take the
			// track away: one declared as the camera, from a participant who
			// holds no claim (the camera's withdrawal, K3; LiveKit enforces a
			// track's declared source, K8). Every other mismatched track is
			// never anyone's and its publisher is removed on this pass:
			// waiting a pass on a withdrawal would only leave it published
			// for one more (the review of #1258). That is the microphone, the
			// claim's own camera source, the share's sound (shared or not), a
			// source LiveKit does not name, and a companion's or a viewer's,
			// whose per-source withdrawal is not relied on to cover a source
			// it never granted.
			withdrawable := t.Source == "camera" && !holder
			switch {
			case mismatched && !withdrawable:
				w.evict, w.mismatch = true, true
			default:
				// Withdrawn; if it is still listed once LiveKit has
				// confirmed the withdrawal, its publisher is removed on a
				// later pass (above).
				w.published = true
				w.mismatch = w.mismatch || mismatched
				if known.ended && !mismatched {
					w.reason = voiceReasonCameraNotWithdrawn
				}
			}
		}
		cameras := part.Cameras
		if holder && len(cameras) > 0 {
			// The claim's own track is the first; every other is extra.
			for _, t := range cameras[1:] {
				note(t, true)
			}
			cameras = nil
		}
		for _, t := range cameras {
			note(t, false)
		}
		for _, t := range part.Mismatched {
			note(t, true)
		}
		grantedWithout := !holder && part.CameraGranted
		if !grantedWithout && !w.published && !w.evict {
			if owes && (!quiet || stamp == 0) {
				delete(r.camWithdrawing, key) // nothing left to withdraw
			}
			return
		}
		if owes && (!quiet || stamp == 0) {
			// Owed again until LiveKit confirms. A stamp later than this
			// pass's listing stands: that confirmation is newer.
			r.camWithdrawing[key] = 0
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
	// A track no pass lists any more is gone, unless its publisher could not
	// be read on this pass; whoever is gone from the room has nothing left
	// to withdraw.
	for tk, t := range r.camTracks {
		if t.seen {
			continue
		}
		pid, _, _ := parseVoiceParticipant(strings.SplitN(tk, "\x00", 2)[0])
		if _, undecided := keep[pid]; !undecided {
			delete(r.camTracks, tk)
		}
	}
	for key := range r.camWithdrawing {
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
			delete(r.camWithdrawing, key)
		}
	}
	return events, withdraw, changed
}

// withdrawCameras makes a pass's camera withdrawals in r, under the room's
// claims, so that none takes away a grant a claim has just made: each is
// checked again, under the lock. A person who holds a claim by then is let
// be, unless what was found is a track that is never a claim's (mismatch),
// as withdrawShares lets a holder of the share be; the camera term is read at
// the write besides, so a write never drops a held camera. A person keeps the
// microphone (every member publishes in a channel-wide room, the only rooms
// with cameras) and, if they hold it, the share; a companion keeps the
// screen while its person holds the share; a viewer keeps nothing. A
// withdrawal that fails with a track published, or a track that no
// withdrawal takes away, removes the publisher (fail closed). It reports
// whether the room needs another pass.
func (p *voicePoller) withdrawCameras(ctx context.Context, r *voiceRoomState, list []cameraWithdrawal) bool {
	if len(list) == 0 {
		return false
	}
	r.claims.Lock()
	defer r.claims.Unlock()
	again := false
	for _, w := range list {
		p.mu.Lock()
		share := p.shareHeldLocked(r, w.principalID)
		holds := w.role == voiceRolePerson && p.cameraHeldLocked(r, w.principalID)
		gone := r.retired
		p.mu.Unlock()
		if gone || (holds && !w.mismatch) {
			continue
		}
		// Removed at once: what the pass found is to be removed, or a
		// mismatched track whose owner has claimed a camera since the plan
		// (a withdrawal now would keep the claim's camera source, and with
		// it a mismatched track declared under it). A holder is not written
		// first: the write would only grant again what the removal takes.
		// Anyone else with the camera granted is, so that the refreshed token
		// LiveKit sends with the withdrawal is the one a rejoin carries.
		remove := w.evict || (holds && w.mismatch)
		key, done := voiceWithdrawKey{w.principalID, w.role}, false
		if !remove || (w.granted && !holds) {
			lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
			// Not what LiveKit listed: a listing that lost the person's
			// canPublish must not turn the withdrawal into a listen-only write.
			in := publishInput{canSubscribe: w.canSubscribe, share: share, canPublish: w.role == voiceRolePerson}
			if _, err := p.setPublishPermission(lctx, r, w.principalID, w.role, in); err != nil {
				slog.WarnContext(lctx, "voice: withdrawing a camera nobody claimed failed; passes retry", "channel", r.channelID, "principal", w.principalID, "participant", w.role.label(), "error", err)
			} else {
				done = true
				p.camWithdrawConfirmed(r, key)
			}
			cancel()
			if !done {
				again = true
			}
		}
		if !remove && (done || !w.published) {
			continue
		}
		again = true
		part := livekit.Participant{Identity: voiceParticipantIdentity(w.principalID, w.role), JoinedAt: w.joinedAt}
		if !p.removeObserved(ctx, r, part, w.principalID, w.reason) || w.role != voiceRolePerson {
			continue
		}
		// A person removed holding a claim has left with it: the claim ends
		// now (reason=left, camera_ended in their presence), so its place
		// under the cap is free and a reconnect cannot continue it.
		r.order.Lock()
		p.mu.Lock()
		events := p.endCameraLocked(r, w.principalID, voiceCameraStopLeft, 0)
		p.mu.Unlock()
		p.writeEvents(ctx, events...)
		r.order.Unlock()
		if len(events) > 0 {
			p.notify(r.channelID)
		}
	}
	return again
}
