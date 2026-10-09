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
	// Agents in voice are deferred (ADR-004). Checked after membership, so a
	// non-member agent learns nothing about the channel from this answer.
	if caller.Kind == store.PrincipalAgent {
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
	case errors.Is(err, livekit.ErrUnavailable):
		// The error text names the LiveKit method and the transport failure;
		// it carries no token and no secret.
		slog.WarnContext(ctx, "voice: livekit unavailable", "channel", channel.ID, "error", err)
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
	writeJSON(w, http.StatusOK, resp)
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
		room:       func(ctx context.Context) (store.VoiceRoom, error) { return s.store.ChannelVoiceRoom(ctx, ch.ID) },
	}}
}

// voiceSession builds the whole response for principal p in channel ch: for
// each room p may join, it makes sure the stored room exists, asks LiveKit to
// create it, and signs a token for it. It returns the response and a label
// per grant for the audit detail.
//
// The room row is created before LiveKit is asked (the name is the durable
// thing), so a failed CreateRoom can leave a row behind. That is harmless: the
// row is kept anyway and holds no token. CreateRoom runs on every request,
// before the token is signed, because LiveKit forgets empty rooms and loses
// all rooms on restart (design note §3).
func (s *Server) voiceSession(ctx context.Context, p store.Principal, ch store.Channel) (schema.VoiceSessionResponseV1, []string, error) {
	identity := voiceIdentity(p.ID)
	resp := schema.VoiceSessionResponseV1{LivekitURL: s.cfg.LiveKit.URL, Identity: identity}
	var labels []string
	for _, a := range s.voiceAudiences(p, ch) {
		room, err := a.room(ctx)
		if err != nil {
			return schema.VoiceSessionResponseV1{}, nil, fmt.Errorf("voice: room for channel %d: %w", ch.ID, err)
		}
		if err := s.lk.CreateRoom(ctx, room.RoomName); err != nil {
			return schema.VoiceSessionResponseV1{}, nil, err
		}
		// The reported expiry is taken before signing and cut to whole
		// seconds, as the token's is, so it is never later than the token's.
		issued := time.Now()
		token, err := s.lk.JoinToken(livekit.JoinParams{
			Identity:   identity,
			Room:       room.RoomName,
			CanPublish: a.canPublish,
			Lifetime:   voiceTokenLifetime,
		})
		if err != nil {
			return schema.VoiceSessionResponseV1{}, nil, fmt.Errorf("voice: sign token: %w", err)
		}
		resp.Rooms = append(resp.Rooms, schema.VoiceRoomGrant{
			Room:       room.RoomName,
			Token:      token,
			CanPublish: a.canPublish,
			ExpiresAt:  schema.NewTimestamp(issued.Add(voiceTokenLifetime).Truncate(time.Second)),
			Audience:   a.audience,
		})
		labels = append(labels, a.label)
	}
	if err := resp.Validate(); err != nil {
		return schema.VoiceSessionResponseV1{}, nil, fmt.Errorf("voice: built an invalid response: %w", err)
	}
	return resp, labels, nil
}
