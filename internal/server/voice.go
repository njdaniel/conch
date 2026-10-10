package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice sessions (issue #126, docs/design/voice-control-plane.md §3, §4).
//
// A human member of a channel asks conchd for a voice session and gets a
// short-lived LiveKit join token for the channel's room. conchd is the only
// authority on who may join; LiveKit only moves audio. Nothing in this file
// logs or audits a token or the API secret.

// voiceTokenLifetime is how long a join token is valid after it is issued.
// LiveKit adds 60 seconds of leeway to the expiry and the not-before time
// (design note §10, finding 6), so a token can start a connection for about
// 75 seconds; the not-before time is deliberately not backdated.
const voiceTokenLifetime = 15 * time.Second

// voiceIdentity is the LiveKit participant identity of a principal: one
// principal, one connection per room.
func voiceIdentity(principalID int64) string { return "p" + strconv.FormatInt(principalID, 10) }

// handleVoiceSession serves POST /v1/channels/{channel}/voice/session.
//
// The order of the checks is part of the contract. Everything that depends on
// who is asking comes first, so a caller who is not a member of the channel
// gets the unknown-channel 404 whether or not voice is configured or LiveKit
// is up, and learns nothing about voice. Only then do the checks that depend
// on conchd's own setup run. A refusal issues nothing: no room row, no
// LiveKit call, no audit event (an agent's denial is the one audit row, and
// it records the denial, not a session).
func (s *Server) handleVoiceSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Voice needs a verified caller; with authentication off the identity is
	// whatever the request says. Refused before the channel is looked at.
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return
	}

	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "voice: find channel failed", "error", err)
		writeInternalError(w)
		return
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: check membership failed", "error", err)
		writeInternalError(w)
		return
	}
	if !member {
		// Operators get no exemption. A non-member agent is audited as such.
		s.auditAgentNonMember(r, "", channel.ID)
		writeChannelNotFound(w)
		return
	}
	// Humans only: agents in voice are deferred (ADR-004), and any kind of
	// principal added later has no voice until someone decides it does.
	// Checked after membership, so a non-member learns nothing about the
	// channel from this answer.
	if caller.Kind != store.PrincipalHuman {
		s.auditAgentDenial(ctx, caller.ID, r.Pattern, "", channel.ID, denyAgentVoice)
		writeError(w, http.StatusForbidden, errForbidden.Code, "agents do not use voice")
		return
	}

	if s.lk == nil {
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceNotConfigured, "voice is not configured on this server")
		return
	}

	resp, grants, err := s.voiceSession(ctx, caller, channel)
	switch {
	case errors.Is(err, errVoiceNoLongerEntitled):
		// Removed, disabled or signed out while LiveKit was being asked: the
		// same answer as if it had happened before the request.
		writeChannelNotFound(w)
		return
	case errors.Is(err, errVoiceRoomChanging):
		// The room was rotated away on every attempt; asking again later
		// gets the new one. Nothing was issued.
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case errors.Is(err, livekit.ErrUnavailable):
		// A caller who hung up cancels the LiveKit call; that is not an
		// outage and must not look like one in the log. Otherwise the error
		// text names the LiveKit method and the transport failure; it carries
		// no token and no secret.
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable", "channel", channel.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: build session failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}

	// The audit row is written before the response, and a failure to write it
	// withholds the tokens: a session conchd cannot account for is not issued.
	detail := fmt.Sprintf("channel=%d identity=%s grants=%s", channel.ID, resp.Identity, strings.Join(grants, ","))
	if _, err := s.store.AppendAuditEvent(context.WithoutCancel(ctx), auditActor(ctx),
		store.AuditVoiceSessionIssued, fmt.Sprintf("channel:%d", channel.ID), detail); err != nil {
		slog.ErrorContext(ctx, "voice: audit voice_session_issued failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	slog.InfoContext(ctx, "voice: session issued", "principal", caller.ID, "channel", channel.ID)
	// The body carries bearer tokens: never cached.
	writeSecretJSON(w, http.StatusOK, resp)
}

// voicePresenceChannel resolves the channel for a presence request and applies
// the checks every presence reader shares, in the order the session endpoint
// uses: a verified caller, then the unknown-channel 404 for a non-member, then
// the refusal of agents. It writes the response and returns false when the
// request must stop. With authentication off there is no verified caller, and
// voice is never anonymous, so presence is refused as the session endpoint is
// (the design note is silent; one rule keeps one answer for all voice routes).
//
// For the socket, afterLookup runs between the channel lookup and the
// membership check: the caller subscribes there, so a removal that lands
// during the checks closes the subscription (see handleVoiceWS).
func (s *Server) voicePresenceChannel(w http.ResponseWriter, r *http.Request, name string, afterLookup func(store.Channel, store.Principal) bool) (store.Channel, store.Principal, bool) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeVoiceRequiresAuth, "voice requires authentication")
		return store.Channel{}, store.Principal{}, false
	}
	channel, err := s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return store.Channel{}, store.Principal{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "voice: find channel failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, store.Principal{}, false
	}
	if afterLookup != nil && !afterLookup(channel, caller) {
		return store.Channel{}, store.Principal{}, false
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: check membership failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, store.Principal{}, false
	}
	if !member {
		s.auditAgentNonMember(r, "", channel.ID)
		writeChannelNotFound(w)
		return store.Channel{}, store.Principal{}, false
	}
	// Whether an agent may see who is talking is part of the deferred
	// decision on agents in voice (ADR-004): refused whatever its manifest
	// says, and audited.
	if caller.Kind != store.PrincipalHuman {
		s.auditAgentDenial(ctx, caller.ID, r.Pattern, "", channel.ID, denyAgentVoice)
		writeError(w, http.StatusForbidden, errForbidden.Code, "agents do not use voice")
		return store.Channel{}, store.Principal{}, false
	}
	return channel, caller, true
}

// handleVoicePresence serves GET /v1/channels/{channel}/voice: the whole voice
// state of the channel. When voice is not configured the snapshot says so
// (configured and available false, no rooms) rather than failing; only a
// caller who passed the membership check sees that.
func (s *Server) handleVoicePresence(w http.ResponseWriter, r *http.Request) {
	channel, _, ok := s.voicePresenceChannel(w, r, r.PathValue("channel"), nil)
	if !ok {
		return
	}
	snap := s.voice.snapshot(channel.ID)
	if err := snap.Validate(); err != nil {
		slog.ErrorContext(r.Context(), "voice: built an invalid presence document", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// errVoiceNoLongerEntitled is voiceSession's refusal when the caller stopped
// being entitled between the handler's checks and the signing of a token.
var errVoiceNoLongerEntitled = errors.New("voice: caller is no longer entitled")

// voiceStillEntitled re-checks, immediately before a token is signed, what the
// handler checked on the way in: the caller is a member of the channel and the
// credential it used is still live (which also covers a disabled principal).
// Asking LiveKit to create the room can take seconds, and a removal that lands
// in that time must not be answered with a fresh token. A window of
// microseconds remains; closing it for good is the poller's job (issue #127),
// which compares every participant against current membership.
func (s *Server) voiceStillEntitled(ctx context.Context, p store.Principal, ch store.Channel) (bool, error) {
	member, err := s.store.IsChannelMember(ctx, ch.ID, p.ID)
	if err != nil || !member {
		return false, err
	}
	if credID, ok := credentialIDFrom(ctx); ok {
		return s.store.CredentialLive(ctx, credID)
	}
	return true, nil
}

// voiceAudience is one room a principal may join in a channel: which audience
// it carries (nil is the whole channel, as on messages), whether the principal
// may publish into it, and how its stored room is found.
type voiceAudience struct {
	audience   *schema.Audience
	canPublish bool
	// label names the grant in the audit detail, e.g. "channel:publish".
	label string
	room  func(context.Context) (store.VoiceRoom, error)
}

// voiceAudiences lists the rooms principal p may join in channel ch. In V3
// the only audience is the whole channel and a member may publish. Net rooms
// (V5) are added here: the response is built from this list and nowhere else,
// so there is no second code path.
func (s *Server) voiceAudiences(_ store.Principal, ch store.Channel) []voiceAudience {
	return []voiceAudience{{
		audience:   nil,
		canPublish: true,
		label:      "channel:publish",
		// Not cancellable: once asked for, the room's row is written or
		// found whether or not the caller is still there.
		room: func(ctx context.Context) (store.VoiceRoom, error) {
			return s.store.ChannelVoiceRoom(context.WithoutCancel(ctx), ch.ID)
		},
	}}
}

// voiceRoomAttempts bounds how often one session request starts over because
// the room it was given was rotated away before its token could be signed. A
// rotation is rare; three in a row means something is rotating the room as
// fast as it is created, and the caller is told to try again later.
const voiceRoomAttempts = 3

// errVoiceRoomChanging is voiceSession's refusal when the channel's room was
// rotated on every attempt. It is answered as voice being unavailable.
var errVoiceRoomChanging = errors.New("voice: the channel's room kept changing")

// voiceSession builds the whole response for principal p in channel ch: for
// each room p may join, it makes sure the stored room exists, records p's
// credential as a holder of it, asks LiveKit to create it, and signs a token
// for it. It returns the response and a label per grant for the audit detail.
//
// The room row is created before LiveKit is asked (the name is the durable
// thing), so a failed CreateRoom can leave a row behind. That is harmless: the
// row is the channel's room from then on and holds no token. CreateRoom runs on every request,
// before the token is signed, because LiveKit forgets empty rooms and loses
// all rooms on restart (design note §3).
func (s *Server) voiceSession(ctx context.Context, p store.Principal, ch store.Channel) (schema.VoiceSessionResponseV1, []string, error) {
	identity := voiceIdentity(p.ID)
	// Every token is recorded against the credential it was issued under, so
	// a request that carries no credential cannot be accounted for: refused.
	credID, ok := credentialIDFrom(ctx)
	if !ok {
		return schema.VoiceSessionResponseV1{}, nil, errors.New("voice: the request carries no credential to record")
	}
	resp := schema.VoiceSessionResponseV1{LivekitURL: s.cfg.LiveKit.URL, Identity: identity}
	var labels []string
	for _, a := range s.voiceAudiences(p, ch) {
		grant, err := s.voiceGrant(ctx, p, ch, credID, identity, a)
		if err != nil {
			return schema.VoiceSessionResponseV1{}, nil, err
		}
		resp.Rooms = append(resp.Rooms, grant)
		labels = append(labels, a.label)
	}
	if err := resp.Validate(); err != nil {
		return schema.VoiceSessionResponseV1{}, nil, fmt.Errorf("voice: built an invalid response: %w", err)
	}
	return resp, labels, nil
}

// voiceGrant issues one room's grant. Between finding the room and signing
// the token a rotation can retire it (a holder lost their place meanwhile).
// A token for a retired room is worthless at best and, with LiveKit's
// auto-create on, would recreate a name that must stay dead, so the request
// starts over against the channel's new room instead: the holder is recorded
// against a live room (the store refuses a retired one), and liveness is
// checked again after the slow LiveKit call and before the token exists.
//
// A request that asked LiveKit to create a room and then issues no token for
// it must not leave a retired room behind: a rotation that landed between the
// holder being recorded and CreateRoom deleted the room before this request
// created it again, and a stale token would work there until the next sweep.
// So every such exit deletes the room if it is retired (voiceDropIfRetired).
func (s *Server) voiceGrant(ctx context.Context, p store.Principal, ch store.Channel, credID int64, identity string, a voiceAudience) (schema.VoiceRoomGrant, error) {
	for range voiceRoomAttempts {
		grant, again, err := s.voiceGrantOnce(ctx, p, ch, credID, identity, a)
		if err != nil {
			return schema.VoiceRoomGrant{}, err
		}
		if !again {
			return grant, nil
		}
	}
	return schema.VoiceRoomGrant{}, errVoiceRoomChanging
}

// voiceGrantOnce is one attempt at voiceGrant. again reports that the room
// was rotated under it and the caller should start over.
func (s *Server) voiceGrantOnce(ctx context.Context, p store.Principal, ch store.Channel, credID int64, identity string, a voiceAudience) (grant schema.VoiceRoomGrant, again bool, err error) {
	// A caller who has lost their place since the handler checked is turned
	// away before a holder is recorded for them. Recorded first, the row
	// would outlive the refused request and rotate the room at the next
	// sweep, disconnecting everyone for a session that was never issued.
	// (The check after CreateRoom is the one that keeps a token from them.)
	entitled, err := s.voiceStillEntitled(ctx, p, ch)
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: check caller: %w", err)
	}
	if !entitled {
		return schema.VoiceRoomGrant{}, false, errVoiceNoLongerEntitled
	}
	room, err := a.room(ctx)
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: room for channel %d: %w", ch.ID, err)
	}
	// Recorded before anything else is done for the room, and so before
	// a token exists: whoever may hold one is always in the store. Not
	// cancellable, like the room row: a caller who hangs up now must not
	// leave a half-issued session.
	err = s.voice.recordHolder(context.WithoutCancel(ctx), room.ID, p.ID, credID)
	if errors.Is(err, store.ErrVoiceRoomRetired) {
		return schema.VoiceRoomGrant{}, true, nil
	}
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: record holder for channel %d: %w", ch.ID, err)
	}
	issued := false
	defer func() {
		if !issued {
			s.voiceDropIfRetired(ctx, room)
		}
	}()
	if err := s.lk.CreateRoom(ctx, room.RoomName); err != nil {
		return schema.VoiceRoomGrant{}, false, err
	}
	// The poller must know the room is in use before any token for it
	// exists (design note §6): a session issued in the last two minutes.
	s.voice.noteSession(room)
	entitled, err = s.voiceStillEntitled(ctx, p, ch)
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: re-check caller: %w", err)
	}
	if !entitled {
		return schema.VoiceRoomGrant{}, false, errVoiceNoLongerEntitled
	}
	live, err := s.store.VoiceRoomLive(ctx, room.ID)
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: re-check room: %w", err)
	}
	if !live {
		return schema.VoiceRoomGrant{}, true, nil
	}
	// The reported expiry is taken before signing and cut to whole
	// seconds, as the token's is, so it is never later than the token's.
	at := time.Now()
	token, err := s.lk.JoinToken(livekit.JoinParams{
		Identity:   identity,
		Room:       room.RoomName,
		CanPublish: a.canPublish,
		Lifetime:   voiceTokenLifetime,
	})
	if err != nil {
		return schema.VoiceRoomGrant{}, false, fmt.Errorf("voice: sign token: %w", err)
	}
	issued = true
	return schema.VoiceRoomGrant{
		Room:       room.RoomName,
		Token:      token,
		CanPublish: a.canPublish,
		ExpiresAt:  schema.NewTimestamp(at.Add(voiceTokenLifetime).Truncate(time.Second)),
		Audience:   a.audience,
	}, false, nil
}

// voiceDropIfRetired is called when a request asked LiveKit to create room
// and is not issuing a token for it. If the room has been retired, the poller
// forgets it and LiveKit is told to delete it, because this request may have
// created it again after the rotation deleted it. A rotation that commits
// after this check deletes the room itself, after our CreateRoom. It runs on
// a bounded context the caller cannot cancel; what it cannot do is left to
// the sweep, which is brought forward.
func (s *Server) voiceDropIfRetired(ctx context.Context, room store.VoiceRoom) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	live, err := s.store.VoiceRoomLive(ctx, room.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: could not tell whether a room was retired; the sweep checks", "channel", room.ChannelID, "error", err)
		s.voice.sweepSoon()
		return
	}
	if live {
		return
	}
	s.voice.forgetRoom(ctx, room.RoomName)
	s.voice.deleteRooms(ctx, []store.VoiceRoom{room})
}
