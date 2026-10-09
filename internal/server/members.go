package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Channel membership (issue #90, ADR-003). Membership is the visibility
// boundary for channel content. It is enforced only when there is an
// authenticated caller (AuthRequired); with no caller every handler behaves
// as it did before membership existed.
//
// A non-member is told the channel does not exist, with the byte-identical
// response a genuinely unknown channel gets (writeChannelNotFound), so
// membership is never revealed. Operators get no read or post exemption: they
// manage membership and may list any channel's members, nothing more.

// writeChannelNotFound is the single response for "no such channel" and "not a
// member of that channel". Every channel-addressed handler must use it for
// both cases; do not write a separate 404 for either.
func writeChannelNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "channel_not_found", "channel not found")
}

// callerIsMember reports whether the request may use channelID's content: true
// when there is no caller (AuthOff), otherwise whether the caller is a member.
// Operators are not exempt.
func (s *Server) callerIsMember(r *http.Request, channelID int64) (bool, error) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		return true, nil
	}
	return s.store.IsChannelMember(r.Context(), channelID, caller.ID)
}

// channelForCaller resolves the channel named name for a request that reads or
// writes its content. It writes the response and returns false for an unknown
// channel, a channel the caller is not a member of (both the identical 404),
// or a store failure (500, failing closed). Every read or write path over
// channel content must go through it.
//
// An agent caller must additionally pass its manifest (issue #79): capability
// and permission say what the request does, and a member agent whose manifest
// does not allow it gets 403. Membership is checked first, so a non-member
// agent still learns nothing.
func (s *Server) channelForCaller(w http.ResponseWriter, r *http.Request, name string, capability schema.Capability, permission schema.ChannelPermission) (store.Channel, bool) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "channels: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return store.Channel{}, false
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "channels: check membership failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return store.Channel{}, false
	}
	if !member {
		s.auditAgentNonMember(r, capability, channel.ID)
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	if !s.agentCallerAllowed(w, r, capability, channel.ID, permission) {
		return store.Channel{}, false
	}
	return channel, true
}

// handleListChannelMembers serves GET /v1/channels/{channel}/members for
// members of the channel and for operators; anyone else gets the
// channel-not-found response.
func (s *Server) handleListChannelMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "members: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if caller, ok := callerFrom(ctx); ok && !isOperator(caller) {
		member, err := s.store.IsChannelMember(ctx, channel.ID, caller.ID)
		if err != nil {
			slog.ErrorContext(ctx, "members: check membership failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if !member {
			s.auditAgentNonMember(r, schema.CapabilityMessagesRead, channel.ID)
			writeChannelNotFound(w)
			return
		}
	}
	// Who else is in a channel is channel content: an agent needs the same
	// grant to see it as to read the channel's messages (issue #79).
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, channel.ID, schema.ChannelPermissionRead) {
		return
	}
	members, err := s.store.ListChannelMembers(ctx, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "members: list failed", "channel", channel.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	resp := schema.ListChannelMembersResponseV1{Members: make([]schema.ChannelMemberV1, 0, len(members))}
	for _, m := range members {
		resp.Members = append(resp.Members, schema.ChannelMemberV1{
			PrincipalID: m.PrincipalID,
			AddedBy:     m.AddedBy,
			CreatedAt:   schema.NewTimestamp(m.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePutChannelMember serves PUT /v1/channels/{channel}/members/{principal_id}
// (operator only, enforced by the route table). Idempotent.
func (s *Server) handlePutChannelMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "members: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	var addedBy int64
	if caller, ok := callerFrom(ctx); ok {
		addedBy = caller.ID
	}
	_, err = s.store.AddChannelMember(ctx, auditActor(ctx), channel.ID, principalID, addedBy)
	if s.writeMemberError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteChannelMember serves DELETE /v1/channels/{channel}/members/{principal_id}
// (operator only). Idempotent. After the removal commits, every live
// WebSocket subscription the principal holds on the channel is closed, so a
// message posted after the removal is never delivered to them.
func (s *Server) handleDeleteChannelMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "members: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	_, err = s.store.RemoveChannelMember(ctx, auditActor(ctx), channel.ID, principalID)
	if s.writeMemberError(w, r, err) {
		return
	}
	// Always drop, even for an idempotent no-op: it is cheap and guarantees no
	// subscription outlives the principal's membership.
	s.hub.DropPrincipal(channel.ID, principalID)
	w.WriteHeader(http.StatusNoContent)
}

// writeMemberError maps a store error from a membership write to a response.
// It reports whether it wrote one.
func (s *Server) writeMemberError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
	default:
		slog.ErrorContext(r.Context(), "members: write failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
	return true
}
