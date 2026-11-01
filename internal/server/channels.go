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
	// The creator's other tabs list the new channel (issue #1027).
	s.events.notifyChannelsChanged(creatorID)
	writeJSON(w, http.StatusCreated, schema.CreateChannelResponse{Channel: channelFromStore(channel)})
}

// The daily bounds on a member's channel creations (issue #981, community
// model §5): creating is the rename limiter's pattern, as a collision is an
// existence oracle on private names. A holder of manage_channels is not
// bounded.
const (
	createsPerDay        = 10
	createAttemptsPerDay = 30
)

// handleCreateChannelV1 serves POST /v1/channels (issue #981, ADR-013
// decision 2): a community member makes a public or private channel and is
// its only member and its manager (O9). Agents and guests are refused before
// the handler runs (humansOnly, membersOnly); the store decides membership
// and the create permission again under its write lock. Every collision is
// the one 409 channel_exists, recorded without the name and counted against
// the caller's attempts.
func (s *Server) handleCreateChannelV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req schema.CreateChannelRequestV1
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
	if !authed {
		// --auth off: no member to make the manager; the channel is the
		// system's, as POST /v0/channels makes it.
		s.handleCreateChannelUnowned(w, r, req)
		return
	}
	create := store.ChannelCreate{
		CallerID:   caller.ID,
		Name:       req.Name,
		Visibility: req.Visibility,
		Limits:     store.RenameLimits{Renames: createsPerDay, Attempts: createAttemptsPerDay},
	}
	channel, err := s.store.CreateChannelV1(ctx, create, s.now())
	if errors.Is(err, store.ErrDuplicate) {
		err = s.store.RecordChannelCreateRefused(context.WithoutCancel(ctx), create, s.now())
		if err == nil {
			writeError(w, http.StatusConflict, "channel_exists", channelExistsMessage)
			return
		}
	}
	var limited *store.RenameLimitedError
	var denied *store.ChannelCreatePermissionError
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotCommunityMember):
		s.denyForbidden(w, r, caller, "forbidden", "only a member of the community may create a channel")
		return
	case errors.As(err, &denied):
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, "you may not create a "+string(req.Visibility)+" channel", refusedPermission(caller, denied.Perm))
		return
	case errors.As(err, &limited):
		s.writeChannelCreateLimitError(w, limited)
		return
	case errors.Is(err, store.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", channelNameInvisibleMessage)
		return
	default:
		slog.ErrorContext(ctx, "channels: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.events.notifyChannelsChanged(caller.ID)
	writeJSON(w, http.StatusCreated, schema.CreateChannelResponseV1{Channel: channelDoc(channel)})
}

// handleCreateChannelUnowned is POST /v1/channels with no caller (--auth
// off): the channel has no member, as POST /v0/channels makes it, with the
// visibility asked for.
func (s *Server) handleCreateChannelUnowned(w http.ResponseWriter, r *http.Request, req schema.CreateChannelRequestV1) {
	ctx := r.Context()
	channel, err := s.store.CreateChannelAs(ctx, auditActor(ctx), req.Name, 0)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "channel_exists", channelExistsMessage)
		return
	case errors.Is(err, store.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", channelNameInvisibleMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "channels: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if req.Visibility != channel.Visibility {
		res, err := s.store.ChannelSettings(ctx, auditActor(ctx), 0, channel.ID, store.ChannelSettingsChange{Visibility: &req.Visibility}, s.now())
		if err != nil {
			slog.ErrorContext(ctx, "channels: set visibility of new channel failed", "channel", channel.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		channel = res.Channel
	}
	writeJSON(w, http.StatusCreated, schema.CreateChannelResponseV1{Channel: channelFromStore(channel)})
}

func (s *Server) writeChannelCreateLimitError(w http.ResponseWriter, limited *store.RenameLimitedError) {
	seconds := max(1, int(math.Ceil(limited.Until.Sub(s.now()).Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, schema.Error{
		Code:              schema.ErrorCodeRateLimited,
		Message:           fmt.Sprintf("you may create channels at most %d times, and try at most %d times, a day", createsPerDay, createAttemptsPerDay),
		RetryAfterSeconds: seconds,
	})
}

// handleChannelSettings serves PUT /v1/channels/{id}/settings (issue #981,
// ADR-013 O5): an operator holding manage_channels changes a channel's
// visibility and default mark. The route class refuses every non-operator
// with one 403 before anything is looked up, and a restricted operator is
// refused here before the id is read, so no answer to either says whether
// a channel exists. Turning private keeps every member; unmarking the last
// default channel while open sign-up is on turns sign-up off.
func (s *Server) handleChannelSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, authed := callerFrom(ctx)
	var actorID int64
	if authed {
		allowed, ok := s.canOrFail(w, r, caller, schema.PermManageChannels)
		if !ok {
			return
		}
		if !allowed {
			s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, "changing a channel's settings needs manage_channels", refusedPermission(caller, schema.PermManageChannels))
			return
		}
		actorID = caller.ID
	}
	id, ok := pathID(w, r, "id", "channel")
	if !ok {
		return
	}
	var req schema.ChannelSettingsRequestV1
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
	res, err := s.store.ChannelSettings(ctx, auditActor(ctx), actorID, id, store.ChannelSettingsChange{Visibility: req.Visibility, Default: req.Default}, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
		return
	case errors.Is(err, store.ErrManageChannelsRequired):
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, "changing a channel's settings needs manage_channels", refusedPermission(caller, schema.PermManageChannels))
		return
	case errors.Is(err, store.ErrChannelSettingsConflict):
		writeError(w, http.StatusConflict, schema.ErrorCodeChannelSettingsConflict, "a default channel must be public: unmark it to make it private, or make it public to mark it")
		return
	case err != nil:
		slog.ErrorContext(ctx, "channels: settings failed", "channel", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if res.Changed {
		// Every open events socket of the channel is sent the channel as it
		// now is, and its members' channel lists carry the new fields.
		s.events.notifyChannel(id, eventRef{kind: eventChannel, id: id})
		s.channelRenamedLists(ctx, id)
	}
	writeJSON(w, http.StatusOK, schema.ChannelSettingsResponseV1{Channel: channelFromStore(res.Channel), OpenSignupTurnedOff: res.OpenSignupTurnedOff})
}

// channelDoc is channelFromStore with the caller's manager grant.
func channelDoc(channel store.Channel) schema.ChannelV0 {
	doc := channelFromStore(channel)
	doc.Manages = channel.Manages
	return doc
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
// An owner, and an operator holding manage_channels, are unlimited (the
// store decides the standing, ADR-014 design §7.2.1); a channel manager,
// a restricted operator included, has the same rename and attempt
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
		s.denyForbiddenFor(w, r, caller, "forbidden", "operator role or channel manager required", refusedPermission(caller, schema.PermManageChannels))
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
					s.denyForbiddenFor(w, r, caller, "forbidden", "operator role or channel manager required", refusedPermission(caller, schema.PermManageChannels))
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
		s.channelRenamedLists(ctx, id)
	}
	writeJSON(w, http.StatusOK, schema.RenameChannelResponseV1{Channel: channelFromStore(res.Channel)})
}

// channelRenamedLists tells every human member of a renamed channel that
// their channel list changed (issue #1027, membership-live.md §2): the name
// is in it. The members are read after the commit, so each one told can read
// the new name. A failed read is logged and does not fail the rename; the
// member's next re-read of the list catches up.
func (s *Server) channelRenamedLists(ctx context.Context, channelID int64) {
	members, err := s.store.ListChannelMembers(ctx, channelID)
	if err != nil {
		slog.ErrorContext(ctx, "channels: list members after rename failed", "channel", channelID, "error", err)
		return
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		if m.Kind == store.PrincipalHuman {
			ids = append(ids, m.PrincipalID)
		}
	}
	s.events.notifyChannelsChanged(ids...)
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
		resp.Channels = append(resp.Channels, channelDoc(ch))
	}
	writeJSON(w, http.StatusOK, resp)
}

func channelFromStore(channel store.Channel) schema.ChannelV0 {
	return schema.ChannelV0{
		ID:   channel.ID,
		Name: channel.Name,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt:  channel.CreatedAt.UTC(),
		Visibility: channel.Visibility,
		Default:    channel.IsDefault,
	}
}
