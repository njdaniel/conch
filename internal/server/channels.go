package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

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
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name must not be empty")
		return
	}

	channel, err := s.store.CreateChannel(ctx, req.Name)
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

func channelFromStore(channel store.Channel) schema.ChannelV0 {
	return schema.ChannelV0{
		ID:   channel.ID,
		Name: channel.Name,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: channel.CreatedAt.UTC(),
	}
}
