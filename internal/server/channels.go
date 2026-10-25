package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

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
		writeError(w, http.StatusConflict, "channel_exists", channelExistsMessage)
		return
	}
	if errors.Is(err, store.ErrNameInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", channelNameInvisibleMessage)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreateChannelResponse{Channel: channelFromStore(channel)})
}

// The refusals every channel name write shares (issue #727). A channel name
// is unique by how it reads, as a principal's is, so a name that differs
// from another channel's only by case or look-alike characters is taken.
const (
	channelExistsMessage        = "a channel with this name, or one that looks the same, already exists"
	channelNameInvisibleMessage = "schema: channel name must have a character a reader can see"
	channelNameStaleMessage     = "the channel is no longer called that: it was renamed meanwhile; list the channels and try again"
)

// handleRenameChannel serves PUT /v1/channels/{id}/name (issues #727, #791).
// Operators are unlimited; a channel manager has the same rename and attempt
// bounds as a principal self-rename. The route class hides an unknown or
// non-member channel before the body is read. A duplicate-name refusal is
// audited without either name and counts toward the manager's attempt limit.
func (s *Server) handleRenameChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "channel")
	if !ok {
		return
	}
	var req schema.RenameChannelRequestV1
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

	caller, authed := callerFrom(ctx)
	var opt store.ChannelRenameOptions
	if authed {
		opt = store.ChannelRenameOptions{CallerID: caller.ID, Limits: store.RenameLimits{Renames: renamesPerDay, Attempts: renameAttemptsPerDay}}
	}
	res, err := s.store.RenameChannel(ctx, auditActor(ctx), id, req.From, req.Name, s.now(), opt)
	var limited *store.RenameLimitedError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
		return
	case errors.Is(err, store.ErrManagerRefused):
		s.denyForbidden(w, r, caller, "forbidden", "operator role or channel manager required")
		return
	case errors.Is(err, store.ErrChannelNameStale):
		writeError(w, http.StatusConflict, schema.ErrorCodeChannelNameStale, channelNameStaleMessage)
		return
	case errors.Is(err, store.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", channelNameInvisibleMessage)
		return
	case errors.Is(err, store.ErrDuplicate):
		if authed {
			if rerr := s.store.RecordChannelRenameRefused(context.WithoutCancel(ctx), caller.ID, id, opt.Limits, s.now()); rerr != nil {
				switch {
				case errors.As(rerr, &limited):
					s.writeChannelRenameLimitError(w, limited)
				case errors.Is(rerr, store.ErrNotFound):
					writeChannelNotFound(w)
				case errors.Is(rerr, store.ErrManagerRefused):
					s.denyForbidden(w, r, caller, "forbidden", "operator role or channel manager required")
				default:
					slog.ErrorContext(ctx, "channels: audit refused rename failed", "channel", id, "error", rerr)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
				return
			}
		}
		writeError(w, http.StatusConflict, "channel_exists", channelExistsMessage)
		return
	case errors.As(err, &limited):
		s.writeChannelRenameLimitError(w, limited)
		return
	case err != nil:
		slog.ErrorContext(ctx, "channels: rename failed", "channel", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if res.Changed {
		// Every open events socket of the channel is sent the channel as it
		// now is. The message and voice sockets are bound to the id and need
		// nothing; no socket is closed.
		s.events.notifyChannel(id, eventRef{kind: eventChannel, id: id})
	}
	writeJSON(w, http.StatusOK, schema.RenameChannelResponseV1{Channel: channelFromStore(res.Channel)})
}

func (s *Server) writeChannelRenameLimitError(w http.ResponseWriter, limited *store.RenameLimitedError) {
	seconds := max(1, int(math.Ceil(limited.Until.Sub(s.now()).Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, schema.Error{
		Code:              schema.ErrorCodeRateLimited,
		Message:           fmt.Sprintf("you may rename channels at most %d times, and try at most %d times, a day", renamesPerDay, renameAttemptsPerDay),
		RetryAfterSeconds: seconds,
	})
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
		doc := channelFromStore(ch)
		doc.Manages = ch.Manages
		resp.Channels = append(resp.Channels, doc)
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
