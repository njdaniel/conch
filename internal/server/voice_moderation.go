package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice-only participant removal (docs/design/moderation.md §4, issue #850).
//
// DELETE /v1/channels/{channel}/voice/participants/{principal_id} is in the
// accessChannelManager class: the guard admits an owner, an operator holding
// manage_channels, or a manager of this channel who is a member, and answers
// the uniform 404 for a channel the caller may not administer, before the
// target is read. The target is an identity, never looked up: an unknown id,
// an agent and a person who is not in voice all answer 204, so the response
// reveals only the caller's standing. Membership, credentials, the voice grant
// and history are not touched; no room is rotated.

// handleVoiceParticipantRemove serves the route above.
func (s *Server) handleVoiceParticipantRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	targetID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
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
	var callerID int64
	if caller, ok := callerFrom(ctx); ok {
		callerID = caller.ID
	}
	// The standing is read again before anything is done: a grant revoked
	// after the guard is met here (design §3.1, §4.1).
	if err := s.store.CheckChannelModerator(ctx, channel.ID, callerID); s.writeMemberError(w, r, err) {
		return
	}
	kickErr := s.voiceParticipantKick(ctx, channel.ID, targetID)
	result := "completed"
	if kickErr != nil {
		result = "failed"
		slog.ErrorContext(ctx, "voice: removing a participant failed; the moderator may retry", "channel", channel.ID, "principal", targetID, "error", kickErr)
	}
	// One row per authorized request, whatever came of it. Neither a room
	// name nor anything LiveKit said is in it.
	_, auditErr := s.store.AppendAuditEvent(context.WithoutCancel(ctx), auditActor(ctx), store.AuditVoiceParticipantKicked,
		fmt.Sprintf("channel:%d", channel.ID), fmt.Sprintf("principal=%d result=%s", targetID, result))
	if kickErr != nil {
		if auditErr != nil {
			slog.ErrorContext(ctx, "voice: audit write failed", "action", store.AuditVoiceParticipantKicked, "error", auditErr)
		}
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable; the participant may not have been removed, retry")
		return
	}
	if auditErr != nil {
		// Fail closed on the record: the removal happened and is idempotent,
		// so a retry is safe.
		slog.ErrorContext(ctx, "voice: audit write failed", "action", store.AuditVoiceParticipantKicked, "error", auditErr)
		writeInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// voiceParticipantKick removes principalID from voice in channelID and no
// further, and reports whether every current room of the channel was cleared.
//
//  1. Their presence sockets in the channel close, saying they were removed
//     from voice and not from the channel (close 1008, text
//     schema.SocketCloseVoiceRemoved); message, events and occupancy sockets
//     stay.
//  2. They leave presence in every current room of the channel and whatever
//     the transmit rule had open for them is closed, before LiveKit is asked.
//  3. LiveKit is asked to remove p<id> (and its companion and viewer) from
//     every current room, net rooms included, bounded by voiceEvictTimeout. No
//     room is rotated: they remain entitled and the others stay connected.
//
// With LiveKit unconfigured there is no media participant, and the result is
// nil. A failed room list or removal is an error; the caller answers 503.
func (s *Server) voiceParticipantKick(ctx context.Context, channelID, principalID int64) error {
	s.voice.kickPrincipal(channelID, principalID)
	if s.lk == nil {
		return nil
	}
	// A caller who hangs up must not cancel a security action.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	rooms, err := s.voice.channelRooms(ctx, channelID)
	if err != nil {
		// Presence cannot be settled for rooms that are not known. Say so.
		return fmt.Errorf("list rooms: %w", err)
	}
	type forgotten struct {
		was voiceSeen
		ok  bool
	}
	gone := make([]forgotten, len(rooms))
	for i, room := range rooms {
		gone[i].was, gone[i].ok = s.voice.forget(ctx, room.RoomName, principalID)
	}
	// A share or camera claim of theirs in flight is waited for and ended.
	s.voice.endSharesOf(ctx, principalID, channelID, voiceShareStopAccessLost)
	s.voice.endCamerasOf(ctx, principalID, channelID, voiceCameraStopAccessLost)
	identity := voiceIdentity(principalID)
	errs := make([]error, len(rooms))
	var wg sync.WaitGroup
	sem := make(chan struct{}, voicePollConcurrency)
	for i, room := range rooms {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = s.voice.evictForgotten(ctx, room, principalID, identity, voiceReasonModeratorKick, gone[i].was, gone[i].ok)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
