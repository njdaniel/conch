package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

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
	caller, authed := callerFrom(ctx)
	var viewer *store.Principal
	if authed {
		viewer = &caller
	}
	writeJSON(w, http.StatusOK, schema.ListChannelMembersResponseV1{Members: memberDocs(members, viewer)})
}

// memberDocs is a channel's member list as viewer sees it, nil for a caller
// with authentication off. The REST list and the events socket's members
// document (issue #515) both come from it, so neither shows more than the
// other. Every viewer sees Manages on every human member; Role (for non-guest
// humans) is shown when the viewer is an operator or a manager of the channel;
// CanDecide is shown when the viewer is an operator, a manager of the channel,
// or that member itself (issues #450, #790, ADR-010 §7.3).
func memberDocs(members []store.ChannelMember, viewer *store.Principal) []schema.ChannelMemberV1 {
	viewerManages := false
	if viewer != nil && viewer.Kind == store.PrincipalHuman && !viewer.IsGuest() && viewer.DisabledAt == nil && viewer.HiddenAt == nil {
		for _, m := range members {
			if m.PrincipalID == viewer.ID && m.Manages {
				viewerManages = true
				break
			}
		}
	}
	docs := make([]schema.ChannelMemberV1, 0, len(members))
	for _, m := range members {
		docs = append(docs, memberDoc(m, viewer, viewerManages))
	}
	return docs
}

func memberDoc(m store.ChannelMember, viewer *store.Principal, viewerManages bool) schema.ChannelMemberV1 {
	privileged := viewer == nil || isOperator(*viewer) || viewerManages
	var canDecide, manages *bool
	var role schema.PrincipalRole
	if m.Kind == store.PrincipalHuman {
		mgr := m.Manages
		manages = &mgr
		if privileged || viewer.ID == m.PrincipalID {
			can := m.CanDecide
			canDecide = &can
		}
		if privileged && m.Guest == nil {
			role = schema.PrincipalRole(m.Role)
		}
	}
	return schema.ChannelMemberV1{
		PrincipalID:  m.PrincipalID,
		AddedBy:      m.AddedBy,
		CreatedAt:    schema.NewTimestamp(m.CreatedAt),
		Name:         m.Name,
		Kind:         schema.PrincipalKind(m.Kind),
		CanDecide:    canDecide,
		Manages:      manages,
		Role:         role,
		HistoryAfter: m.HistoryAfter,
		Guest:        m.Guest,
	}
}

// handlePutChannelMember serves PUT /v1/channels/{channel}/members/{principal_id}
// (operator or channel manager, enforced by the route table; issue #790). Idempotent.
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
	caller, authed := callerFrom(ctx)
	manager := authed && !isOperator(caller)
	now := s.now()
	if manager {
		if err := s.store.CheckMemberAddLimit(ctx, caller.ID, renameAttemptsPerDay, now); err != nil {
			s.writeMemberAddLimitError(w, r, err, now)
			return
		}
		err = s.store.CheckManagerAddTarget(ctx, channel.ID, caller.ID, principalID)
		if errors.Is(err, store.ErrNotAddable) || errors.Is(err, store.ErrPrincipalNotFound) {
			if rerr := s.store.RecordMemberAddRefused(context.WithoutCancel(ctx), caller.ID, channel.ID, renameAttemptsPerDay, now); rerr != nil {
				s.writeMemberAddLimitError(w, r, rerr, now)
				return
			}
			writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
			return
		}
		if s.writeMemberError(w, r, err) {
			return
		}
	}
	req, ok := decodeOptionalMemberBody(w, r)
	if !ok {
		return
	}
	if manager {
		if req.CanDecide != nil || req.Manages != nil {
			s.denyForbidden(w, r, caller, "forbidden", "only an operator may set can_decide or manages")
			return
		}
		_, err = s.store.PutChannelMember(ctx, auditActor(ctx), channel.ID, principalID, caller.ID, nil, nil)
		if errors.Is(err, store.ErrNotAddable) || errors.Is(err, store.ErrPrincipalNotFound) {
			if rerr := s.store.RecordMemberAddRefused(context.WithoutCancel(ctx), caller.ID, channel.ID, renameAttemptsPerDay, now); rerr != nil {
				s.writeMemberAddLimitError(w, r, rerr, now)
				return
			}
			writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
			return
		}
	} else {
		var addedBy int64
		if authed {
			addedBy = caller.ID
		}
		_, err = s.store.PutChannelMember(ctx, auditActor(ctx), channel.ID, principalID, addedBy, req.CanDecide, req.Manages)
	}
	if s.writeMemberError(w, r, err) {
		return
	}
	// A new member's sessions join the channel's roster.
	s.events.notifyChannelRoster(channel.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleAddChannelMemberByName serves POST /v1/channels/{channel}/members
// (operator or channel manager; issue #790, ADR-010 §3.3). It adds the named
// human member with can_decide = 0 and manages = 0, returning 201 on a new
// membership and 200 when the person is already in the channel.
func (s *Server) handleAddChannelMemberByName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req schema.AddChannelMemberRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
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
	caller, authed := callerFrom(ctx)
	manager := authed && !isOperator(caller)
	now := s.now()
	var addedBy int64
	if authed {
		addedBy = caller.ID
	}
	if manager {
		if err := s.store.CheckMemberAddLimit(ctx, caller.ID, renameAttemptsPerDay, now); err != nil {
			s.writeMemberAddLimitError(w, r, err, now)
			return
		}
	}
	member, added, err := s.store.AddChannelMemberByName(ctx, auditActor(ctx), channel.ID, addedBy, req.Name)
	if errors.Is(err, store.ErrNotAddable) || errors.Is(err, store.ErrPrincipalNotFound) {
		if manager {
			if rerr := s.store.RecordMemberAddRefused(context.WithoutCancel(ctx), caller.ID, channel.ID, renameAttemptsPerDay, now); rerr != nil {
				s.writeMemberAddLimitError(w, r, rerr, now)
				return
			}
		}
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	}
	if s.writeMemberError(w, r, err) {
		return
	}
	var viewer *store.Principal
	if authed {
		viewer = &caller
	}
	doc := memberDoc(member, viewer, true)
	if added {
		s.events.notifyChannelRoster(channel.ID)
		writeJSON(w, http.StatusCreated, doc)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) writeMemberAddLimitError(w http.ResponseWriter, r *http.Request, err error, now time.Time) {
	var limited *store.RenameLimitedError
	if errors.As(err, &limited) {
		retry := max(1, int(math.Ceil(limited.Until.Sub(now).Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many refused member additions; try again later")
		return
	}
	slog.ErrorContext(r.Context(), "members: check add limit failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// handleDeleteChannelMember serves DELETE /v1/channels/{channel}/members/{principal_id}
// (operator or channel manager; issue #790). Idempotent. After the removal
// commits, every live WebSocket subscription the principal holds on the
// channel is closed, so a message posted after the removal is never delivered
// to them.
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
	var callerID int64
	if caller, ok := callerFrom(ctx); ok {
		callerID = caller.ID
	}
	_, err = s.store.RemoveChannelMember(ctx, auditActor(ctx), channel.ID, principalID, callerID)
	if s.writeMemberError(w, r, err) {
		return
	}
	// Always drop, even for an idempotent no-op: it is cheap and guarantees no
	// subscription outlives the principal's membership.
	s.hub.DropPrincipal(channel.ID, principalID)
	// The events sockets: theirs close, and the rest are told the roster
	// and the member list lost them (events_ws.go).
	s.events.dropPrincipal(channel.ID, principalID)
	s.events.notifyChannelRoster(channel.ID)
	// Voice: close their presence sockets and remove them from the channel's
	// rooms before the response is written. It cannot fail the request;
	// the poller finds anyone it could not remove (issue #127).
	s.voiceMemberRemoved(ctx, channel.ID, principalID)
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
	case errors.Is(err, store.ErrOwnerProtected), errors.Is(err, store.ErrManagerRefused):
		if caller, ok := callerFrom(r.Context()); ok {
			s.denyForbidden(w, r, caller, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		}
	case errors.Is(err, store.ErrManageNotAllowed):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
	case errors.Is(err, store.ErrGuestNotAllowed):
		// Issue #705, review F1: a guest is never given the right to decide or manage.
		writeError(w, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed, "a guest cannot be given the right to decide approvals or manage a channel")
	case errors.Is(err, store.ErrGuestEnded):
		writeError(w, http.StatusConflict, schema.ErrorCodeGuestEnded, "the guest visit has ended")
	default:
		slog.ErrorContext(r.Context(), "members: write failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
	return true
}

// maxMemberBodyBytes bounds PUT member's optional body, a two-field object.
const maxMemberBodyBytes = 1 << 10

// decodeOptionalMemberBody reads PUT member's body, which may be empty (the
// request as it has always been made) or a PutChannelMemberRequestV1 (issues
// #450, #790). It writes a 400 and returns false for anything else.
func decodeOptionalMemberBody(w http.ResponseWriter, r *http.Request) (schema.PutChannelMemberRequestV1, bool) {
	var req schema.PutChannelMemberRequestV1
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMemberBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is too large or unreadable")
		return req, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return req, true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var extra any
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || decoder.Decode(&req) != nil || !errors.Is(decoder.Decode(&extra), io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one JSON object with only can_decide and manages")
		return req, false
	}
	return req, true
}
