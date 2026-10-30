package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Companion and viewer participants, the issuing side (issue #933, part B of
// #897; docs/design/voice-control-plane.md §13). The poller's side, folding
// them into the person, is voice_presence.go (issue #932).
//
// This file signs LiveKit tokens, so it is security-sensitive: the order of
// the checks is the contract, a refusal at any step issues nothing, the grants
// are fixed here and not taken from the request, and nothing here logs or
// audits a token, a room name or the API secret.

// voiceCompanionMaxBytes bounds the request body: two short fields.
const voiceCompanionMaxBytes = 1 << 10

// errVoiceCompanionNoRoom is the grant's refusal when the channel has no live
// channel-wide room that anyone holds a session for. A companion joins a
// room; it never creates one.
var errVoiceCompanionNoRoom = errors.New("voice: the channel has no room to join")

// voiceCompanionRoom finds the channel's live channel-wide room and reports
// whether anyone holds a session for it. It only reads: asking for a companion
// must not be what creates a room.
func (s *Server) voiceCompanionRoom(ctx context.Context, channelID int64) (store.VoiceRoom, bool, error) {
	rooms, err := s.store.VoiceRoomsForChannel(ctx, channelID)
	if err != nil {
		return store.VoiceRoom{}, false, err
	}
	for _, room := range rooms {
		if room.NetID != 0 {
			continue
		}
		holders, err := s.store.VoiceRoomHolders(ctx, room.ID)
		if err != nil {
			return store.VoiceRoom{}, false, err
		}
		return room, len(holders) > 0, nil
	}
	return store.VoiceRoom{}, false, nil
}

// companionSeen reports, from the poller's last pass over the room, whether
// the person's own connection p<id> is in it (a person who is there only as a
// viewer does not count) and whether their companion is. A room the poller
// does not know, has retired, or could not read on its last attempt reports
// neither. Nothing is asked of LiveKit: the answer is at most one pass gap
// old, and an eviction since the pass is already reflected.
func (p *voicePoller) companionSeen(roomName string, principalID int64) (parent, screen bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rooms[roomName]
	if r == nil || r.retired || r.stale {
		return false, false
	}
	seen, ok := r.participants[principalID]
	screen = r.companions[principalID].screen
	if at, issued := r.screenIssued[principalID]; issued && p.now().Sub(at) < voicePassGapMax {
		// A token signed less than a pass gap ago: the companion may be
		// connecting, and no pass has had the chance to list it.
		screen = true
	}
	return ok && !seen.viaViewer, screen
}

// companionIssued notes that a p<id>.screen token was just signed for room.
func (p *voicePoller) companionIssued(room store.VoiceRoom, principalID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.roomLocked(room)
	if r.screenIssued == nil {
		r.screenIssued = map[int64]time.Time{}
	}
	r.screenIssued[principalID] = p.now()
}

// companionIssuedClear forgets a note made by companionIssued.
func (p *voicePoller) companionIssuedClear(roomName string, principalID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.rooms[roomName]; r != nil {
		delete(r.screenIssued, principalID)
	}
}

// companionRemoved takes principalID's companion or viewer out of the poller's
// memory of the room once conchd has removed it, so that a request that
// follows is not refused for a participant that is gone. A pass that listed
// the participant just before the removal can put it back; the next pass
// corrects that.
func (p *voicePoller) companionRemoved(roomName string, principalID int64, role voiceRole) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rooms[roomName]
	if r == nil {
		return
	}
	if role == voiceRoleScreen {
		delete(r.screenIssued, principalID)
	}
	c, ok := r.companions[principalID]
	if !ok {
		return
	}
	switch role {
	case voiceRoleScreen:
		c.screen, c.screenSubscribes, c.orphanSince = false, false, time.Time{}
	case voiceRoleView:
		c.view = false
	}
	if !c.screen && !c.view {
		delete(r.companions, principalID)
		return
	}
	r.companions[principalID] = c
}

// handleVoiceCompanion serves POST /v1/channels/{channel}/voice/companion:
// the join token of the caller's second participant, the screen companion
// (publish screen, p<id>.screen) or the viewer (publish none, p<id>.view).
//
// The checks, in order, are the contract, so that a non-member learns nothing:
//
//  1. a verified caller; 2. the channel exists and the caller is a member;
//  3. the caller is human; 4. the caller holds voice; 5. voice is configured
//     (voiceSessionCaller, shared with the session route);
//  6. the body is one strictly decoded request;
//  7. the request carries a credential to record the holder under;
//  8. the channel has a live channel-wide room someone holds a session for;
//  9. (screen) the caller's own p<id> is in it, as the last pass saw;
//  10. (screen) no p<id>.screen of the caller is;
//  11. the holder is recorded, the room created and the caller is still
//     entitled and the room still live (voiceGrant);
//  12. the audit row is written before the body.
//
// A refusal at any step issues nothing: no holder row, no LiveKit call (except
// the idempotent CreateRoom at step 11) and no audit row (an agent's denial is
// the one audit row, and it records the denial).
func (s *Server) handleVoiceCompanion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, voiceCompanionMaxBytes)
	req, err := schema.DecodeVoiceCompanionRequestV1(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		// A fixed message: the decoder's own names what the client sent.
		writeError(w, http.StatusBadRequest, "invalid_request", `request body must be a voice companion request: a publish of screen or none, and subscribe`)
		return
	}

	// Every token is recorded against the credential it was issued under.
	credID, ok := credentialIDFrom(ctx)
	if !ok {
		slog.ErrorContext(ctx, "voice: companion request carries no credential to record", "channel", channel.ID)
		writeInternalError(w)
		return
	}

	room, held, err := s.voiceCompanionRoom(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: find the room for a companion failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !held {
		writeVoiceNoSession(w)
		return
	}
	role := voiceRoleView
	if req.Publish == schema.VoiceCompanionPublishScreen {
		role = voiceRoleScreen
		parent, screen := s.voice.companionSeen(room.RoomName, caller.ID)
		if !parent {
			// A companion joins the person's connection; they have none.
			writeVoiceNoSession(w)
			return
		}
		if screen {
			writeError(w, http.StatusConflict, schema.ErrorCodeVoiceCompanionBusy, "you already have a screen companion in this room")
			return
		}
	}
	identity := voiceParticipantIdentity(caller.ID, role)

	grant, err := s.voiceCompanionGrant(ctx, caller, channel, credID, identity, req, room.ID)
	switch {
	case errors.Is(err, errVoiceNoLongerEntitled):
		writeChannelNotFound(w)
		return
	case errors.Is(err, errVoiceCompanionNoRoom):
		writeVoiceNoSession(w)
		return
	case errors.Is(err, errVoiceRoomChanging):
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case errors.Is(err, livekit.ErrUnavailable):
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable", "channel", channel.ID, "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return
	case err != nil:
		slog.ErrorContext(ctx, "voice: build companion grant failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if role == voiceRoleScreen {
		s.voice.companionIssued(room, caller.ID)
	}
	resp := schema.VoiceCompanionResponseV1{
		LivekitURL: s.cfg.LiveKit.URL,
		Identity:   identity,
		Room:       grant.Room,
		Token:      grant.Token,
		Publish:    req.Publish,
		Subscribe:  req.Subscribe,
		ExpiresAt:  grant.ExpiresAt,
	}
	if err := resp.Validate(); err != nil {
		slog.ErrorContext(ctx, "voice: built an invalid companion response", "channel", channel.ID, "error", err)
		s.voice.companionIssuedClear(room.RoomName, caller.ID)
		writeInternalError(w)
		return
	}

	// The audit row is written before the response, and a failure to write it
	// withholds the token: a participant conchd cannot account for is not
	// issued.
	detail := fmt.Sprintf("channel=%d identity=%s grants=%s", channel.ID, identity, voiceCompanionLabel(req))
	if _, err := s.store.AppendAuditEvent(context.WithoutCancel(ctx), auditActor(ctx),
		store.AuditVoiceSessionIssued, fmt.Sprintf("channel:%d", channel.ID), detail); err != nil {
		slog.ErrorContext(ctx, "voice: audit voice_session_issued failed", "channel", channel.ID, "error", err)
		s.voice.companionIssuedClear(room.RoomName, caller.ID)
		writeInternalError(w)
		return
	}
	slog.InfoContext(ctx, "voice: companion issued", "principal", caller.ID, "channel", channel.ID, "participant", role.label())
	// The body carries a bearer token: never cached.
	writeSecretJSON(w, http.StatusOK, resp)
}

// voiceCompanionLabel names the grant in the audit detail.
func voiceCompanionLabel(req schema.VoiceCompanionRequestV1) string {
	switch {
	case req.Publish == schema.VoiceCompanionPublishNone:
		return "companion:view"
	case req.Subscribe:
		return "companion:screen:subscribe"
	default:
		return "companion:screen:publish_only"
	}
}

// voiceCompanionGrant issues the one grant, through the same code that issues
// a person's (voiceGrant): the holder is recorded before the token is signed,
// the caller is checked again after CreateRoom, and a room rotated under the
// request starts it over, at most voiceRoomAttempts times. The grant is fixed
// here from the validated request and nothing else in it.
func (s *Server) voiceCompanionGrant(ctx context.Context, p store.Principal, ch store.Channel, credID int64, identity string, req schema.VoiceCompanionRequestV1, roomID int64) (schema.VoiceRoomGrant, error) {
	a := voiceAudience{
		// canPublish stays false: it is the person's microphone right, and
		// a companion's publish right comes from companion alone.
		label: voiceCompanionLabel(req),
		companion: &livekit.CompanionJoin{
			CanSubscribe:  req.Subscribe,
			PublishScreen: req.Publish == schema.VoiceCompanionPublishScreen,
		},
		room: func(ctx context.Context) (store.VoiceRoom, error) {
			room, held, err := s.voiceCompanionRoom(ctx, ch.ID)
			if err != nil {
				return store.VoiceRoom{}, err
			}
			// The checks of the handler (a parent in the room, no companion
			// yet) were made against the room it found. A different room now
			// (rotated meanwhile) has had none of them: nothing is signed.
			if !held || room.ID != roomID {
				return store.VoiceRoom{}, errVoiceCompanionNoRoom
			}
			return room, nil
		},
	}
	return s.voiceGrant(ctx, p, ch, credID, identity, a)
}

// handleVoiceCompanionDelete serves
// DELETE /v1/channels/{channel}/voice/companion[?publish=screen|none]: the
// caller removes their own screen companion (publish=screen), viewer
// (publish=none) or both (no parameter) from the channel's room. It runs
// checks 1-5 of the POST, then reads the parameter, so a non-member learns
// nothing from a malformed one.
//
// It answers 204 whether or not anyone was removed: the caller may be a
// renderer's owner cleaning up after a crash, without knowing what LiveKit
// still lists. voice_companion_removed (reason=requested) is written for each
// participant LiveKit actually removed. It never removes p<id> or anyone
// else's participant and never ends the caller's share: a share whose tracks
// have gone ends by the idle rule. If LiveKit cannot be asked it answers 503;
// what it had already removed is audited all the same.
func (s *Server) handleVoiceCompanionDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}
	roles := []voiceRole{voiceRoleScreen, voiceRoleView}
	if values, named := r.URL.Query()["publish"]; named {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "publish must be screen or none, given once")
			return
		}
		switch schema.VoiceCompanionPublish(values[0]) {
		case schema.VoiceCompanionPublishScreen:
			roles = roles[:1]
		case schema.VoiceCompanionPublishNone:
			roles = roles[1:]
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "publish must be screen or none, given once")
			return
		}
	}

	// Reads only: a request to remove must not create a room.
	rooms, err := s.store.VoiceRoomsForChannel(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: find the room for a companion removal failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	for _, room := range rooms {
		if room.NetID != 0 {
			continue
		}
		for _, role := range roles {
			if !s.removeCompanion(ctx, w, channel, caller, room, role) {
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeCompanion removes caller's participant in role from room and audits it
// if LiveKit had it. It reports false after writing the response when the
// request must stop.
func (s *Server) removeCompanion(ctx context.Context, w http.ResponseWriter, channel store.Channel, caller store.Principal, room store.VoiceRoom, role voiceRole) bool {
	// Not tied to the caller: a removal asked for is seen through, bounded.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	removed, err := s.lk.Evict(lctx, room.RoomName, voiceParticipantIdentity(caller.ID, role))
	if err != nil {
		// The text names the LiveKit method and the transport failure; it
		// carries no token and no secret.
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: livekit unavailable; a companion was not removed", "channel", channel.ID, "participant", role.label(), "error", err)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		return false
	}
	// LiveKit has answered that the participant is not in the room, whether
	// it just removed it or it had gone by itself: either way the poller's
	// memory of it is out of date, and a request that follows is not to be
	// refused for a companion that is not there.
	s.voice.companionRemoved(room.RoomName, caller.ID, role)
	if !removed {
		return true
	}
	detail := fmt.Sprintf("channel=%d principal=%d participant=%s reason=requested", channel.ID, caller.ID, role.label())
	if _, err := s.store.AppendAuditEvent(context.WithoutCancel(ctx), auditActor(ctx),
		store.AuditVoiceCompanionRemoved, fmt.Sprintf("channel:%d", channel.ID), detail); err != nil {
		// The participant is gone either way; an unwritten row is logged.
		slog.ErrorContext(ctx, "voice: audit voice_companion_removed failed", "channel", channel.ID, "error", err)
	}
	return true
}
