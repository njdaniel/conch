package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req schema.CreateChannelRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// The creator of a channel is its only member; with no caller (AuthOff)
	// the channel starts with no members.
	var creatorID int64
	if caller, ok := callerFrom(ctx); ok {
		creatorID = caller.ID
	}
	channel, err := s.store.CreateChannelAs(ctx, auditActor(ctx), req.Name, creatorID)
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, "channel_exists", "a channel with this name already exists")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreateChannelResponse{Channel: channelFromStore(channel)})
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// With a caller, only the caller's channels (operators included); with no
	// caller (AuthOff), every channel as before.
	var channels []store.Channel
	var err error
	if caller, ok := callerFrom(ctx); ok {
		channels, err = s.store.ListChannelsForPrincipal(ctx, caller.ID)
	} else {
		channels, err = s.store.ListChannels(ctx)
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	resp := schema.ListChannelsResponse{Channels: make([]schema.ChannelV0, 0, len(channels))}
	for _, ch := range channels {
		resp.Channels = append(resp.Channels, channelFromStore(ch))
	}
	writeJSON(w, http.StatusOK, resp)
}

func channelFromStore(channel store.Channel) schema.ChannelV0 {
	return schema.ChannelV0{
		ID:   channel.ID,
		Name: channel.Name,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: channel.CreatedAt.UTC(),
	}
}
