package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #982 (community model I3b, design §2.6 rows 3 and 4, §5): the
// channel directory, and a member joining and leaving channels themselves.

// handleChannelDirectory serves GET /v1/directory/channels. A member sees
// every public channel and the private channels they are in, each with
// joined, default, visibility and (when joined) manages, and no counts and
// no names of members. ?scope=all, for a holder of manage_channels (R4),
// lists every channel with its member count, so an operator can find a
// channel nobody can reach; it reads no channel's content. Agents and
// guests are refused before the handler runs.
func (s *Server) handleChannelDirectory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var all bool
	switch scope := r.URL.Query().Get("scope"); scope {
	case "":
	case "all":
		all = true
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "scope must be all, or left out")
		return
	}
	caller, authed := callerFrom(ctx)
	var viewerID int64
	if authed {
		viewerID = caller.ID
		if all {
			allowed, ok := s.canOrFail(w, r, caller, schema.PermManageChannels)
			if !ok {
				return
			}
			if !allowed {
				// A member is refused as the operator routes refuse one; a
				// restricted operator is told which permission it lacks.
				perm := refusedPermission(caller, schema.PermManageChannels)
				code := "forbidden"
				if perm != "" {
					code = schema.ErrorCodePermissionDenied
				}
				s.denyForbiddenFor(w, r, caller, code, "listing every channel needs manage_channels", perm)
				return
			}
		}
	}
	// With authentication off there is no viewer to hold a membership, and
	// nothing is withheld: every channel, counted only on ?scope=all.
	listing, err := s.store.ChannelDirectory(ctx, viewerID, all || !authed)
	if err != nil {
		slog.ErrorContext(ctx, "directory: list channels failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	resp := schema.ChannelDirectoryResponseV1{Channels: make([]schema.ChannelListingV1, 0, len(listing))}
	for _, l := range listing {
		row := schema.ChannelListingV1{
			ID:         l.ID,
			Name:       l.Name,
			CreatedAt:  schema.NewTimestamp(l.CreatedAt),
			Visibility: l.Visibility,
			Default:    l.IsDefault,
			Joined:     l.Joined,
			Manages:    l.Joined && l.Manages,
		}
		if all {
			n := l.Members
			row.Members = &n
		}
		resp.Channels = append(resp.Channels, row)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleJoinChannel serves POST /v1/channels/{channel}/join: a member joins
// a public channel themselves, with can_decide = 0 and no history bound
// (ADR-013 O12), or gets their membership back unchanged if they are in it
// already (200; a new membership is 201). A guest or an agent is refused
// with one 403 channel_not_joinable before the channel is looked up, so the
// refusal says nothing of which channels exist; a private channel the caller
// is not in is the same 404 channel_not_found as one that never existed.
func (s *Server) handleJoinChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, authed := callerFrom(ctx)
	if authed && (caller.Kind != store.PrincipalHuman || caller.IsGuest()) {
		s.denyForbidden(w, r, caller, schema.ErrorCodeChannelNotJoinable, "only a member of the community may join a channel")
		return
	}
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: find channel to join failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if !authed {
		// Authentication off: there is nobody to make a member.
		writeError(w, http.StatusBadRequest, "invalid_request", "joining a channel needs a signed-in member")
		return
	}
	res, err := s.store.JoinChannel(ctx, channel.ID, caller.ID, s.now())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
		return
	case errors.Is(err, store.ErrChannelNotJoinable):
		s.denyForbidden(w, r, caller, schema.ErrorCodeChannelNotJoinable, "you may not join this channel")
		return
	default:
		slog.ErrorContext(ctx, "channels: join failed", "channel", channel.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	status := http.StatusOK
	if res.Added {
		status = http.StatusCreated
		// The new member's sessions join the channel's roster, and their
		// own channel list gained the channel (issue #1027).
		s.events.notifyChannelRoster(channel.ID)
		s.events.notifyChannelsChanged(caller.ID)
	}
	writeJSON(w, status, schema.JoinChannelResponseV1{
		Channel: channelDoc(res.Channel),
		Member:  memberDoc(res.Member, &caller, false),
	})
}

// handleLeaveChannel serves POST /v1/channels/{channel}/leave: any member
// of the channel leaves it, managers and operators included, losing a
// manager grant with the membership. The last member of a private channel
// may not (409 channel_last_member); the last member of a public channel,
// and a member of a default channel, may. A channel the caller is not in is
// the one 404 channel_not_found, whether it exists or not. Agents and guests
// are refused before the handler runs: a guest ends its visit instead.
func (s *Server) handleLeaveChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, authed := callerFrom(ctx)
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: find channel to leave failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if !authed {
		// Authentication off: there is nobody whose membership to end.
		writeError(w, http.StatusBadRequest, "invalid_request", "leaving a channel needs a signed-in member")
		return
	}
	err = s.store.LeaveChannel(ctx, channel.ID, caller.ID, s.now())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
		return
	case errors.Is(err, store.ErrChannelLastMember):
		writeError(w, http.StatusConflict, schema.ErrorCodeChannelLastMember, "you are the last member of this private channel: someone else must be added before you leave")
		return
	default:
		slog.ErrorContext(ctx, "channels: leave failed", "channel", channel.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// What a removal does after it commits (handleDeleteChannelMember): no
	// subscription outlives the membership, the roster and the leaver's
	// other sockets are told, and voice lets them go before the answer.
	s.hub.DropPrincipal(channel.ID, caller.ID)
	s.events.dropPrincipal(channel.ID, caller.ID)
	s.events.notifyChannelRoster(channel.ID)
	s.events.notifyChannelsChanged(caller.ID)
	s.voiceMemberRemoved(ctx, channel.ID, caller.ID)
	w.WriteHeader(http.StatusNoContent)
}
