package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Nets (issue #115, ADR-005). A net is a named subset of one channel's
// members. Operators create, archive and edit nets; everyone else can only
// list the nets they are on. A net the caller is not on is indistinguishable
// from one that does not exist: it never appears in a list, and the routes
// that address a net by name are operator-only, so there is no per-net
// response a non-participant could probe.

// writeNetNotFound is the single response for an unknown or archived net.
func writeNetNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "net_not_found", "net not found")
}

func writeInternalError(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

// netChannel resolves the {channel} path value for an operator-only net route.
// Like the membership writes it applies no membership test, since operators
// administer channels they are not in. It writes the response and returns
// false for an unknown channel or a store failure.
func (s *Server) netChannel(w http.ResponseWriter, r *http.Request) (store.Channel, bool) {
	channel, err := s.store.ChannelByName(r.Context(), r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "nets: find channel failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, false
	}
	return channel, true
}

// netName reads and validates the {net} path value. A name that can never
// name a net is a malformed request, not an unknown net.
func netName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("net")
	if err := schema.ValidateNetName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid net name")
		return "", false
	}
	return name, true
}

// netView assembles the wire form of n with its roster.
func (s *Server) netView(r *http.Request, n store.Net) (schema.NetV1, error) {
	members, err := s.store.ListNetMembers(r.Context(), n.ID)
	if err != nil {
		return schema.NetV1{}, err
	}
	v := schema.NetV1{
		ID:        n.ID,
		ChannelID: n.ChannelID,
		Name:      n.Name,
		Members:   make([]schema.NetMember, 0, len(members)),
		CreatedAt: schema.NewTimestamp(n.CreatedAt),
	}
	for _, m := range members {
		v.Members = append(v.Members, schema.NetMember{PrincipalID: m.PrincipalID, Role: m.Role})
	}
	return v, nil
}

// handleCreateNet serves POST /v1/channels/{channel}/nets (operator only).
func (s *Server) handleCreateNet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, ok := s.netChannel(w, r)
	if !ok {
		return
	}
	var req schema.CreateNetRequestV1
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
	var createdBy int64
	if caller, ok := callerFrom(ctx); ok {
		createdBy = caller.ID
	}
	n, err := s.store.CreateNet(ctx, auditActor(ctx), channel.ID, req.Name, createdBy)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "net_exists", "a net with this name already exists in the channel")
		return
	case errors.Is(err, store.ErrNotFound):
		writeChannelNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "nets: create failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	view, err := s.netView(r, n)
	if err != nil {
		slog.ErrorContext(ctx, "nets: read roster failed", "net", n.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreateNetResponseV1{Net: view})
}

// handleListNets serves GET /v1/channels/{channel}/nets. Operators and, with
// authentication off, everyone get every live net of the channel; any other
// caller must be a member of the channel (else the channel-not-found
// response) and gets only the nets they are on.
func (s *Server) handleListNets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, ok := s.netChannel(w, r)
	if !ok {
		return
	}
	caller, haveCaller := callerFrom(ctx)
	seeAll := !haveCaller || isOperator(caller)
	if !seeAll {
		member, err := s.store.IsChannelMember(ctx, channel.ID, caller.ID)
		if err != nil {
			slog.ErrorContext(ctx, "nets: check membership failed", "error", err)
			writeInternalError(w)
			return
		}
		if !member {
			s.auditAgentNonMember(r, schema.CapabilityMessagesRead, channel.ID)
			writeChannelNotFound(w)
			return
		}
	}
	// Who is on which net is channel content: an agent needs the same grant to
	// see it as to read the channel's messages (issue #79).
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, channel.ID, schema.ChannelPermissionRead) {
		return
	}
	var nets []store.Net
	var err error
	if seeAll {
		nets, err = s.store.ListNets(ctx, channel.ID)
	} else {
		nets, err = s.store.ListNetsForPrincipal(ctx, channel.ID, caller.ID)
	}
	if err != nil {
		slog.ErrorContext(ctx, "nets: list failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.ListNetsResponseV1{Nets: make([]schema.NetV1, 0, len(nets))}
	for _, n := range nets {
		view, err := s.netView(r, n)
		if err != nil {
			slog.ErrorContext(ctx, "nets: read roster failed", "net", n.ID, "error", err)
			writeInternalError(w)
			return
		}
		resp.Nets = append(resp.Nets, view)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArchiveNet serves DELETE /v1/channels/{channel}/nets/{net} (operator
// only). A net that is unknown or already archived is net_not_found.
func (s *Server) handleArchiveNet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, ok := s.netChannel(w, r)
	if !ok {
		return
	}
	name, ok := netName(w, r)
	if !ok {
		return
	}
	_, err := s.store.ArchiveNet(ctx, auditActor(ctx), channel.ID, name)
	if s.writeNetError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePutNetMember serves PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}
// (operator only). It adds the principal or changes their role; repeating the
// same request is a no-op.
func (s *Server) handlePutNetMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	channel, ok := s.netChannel(w, r)
	if !ok {
		return
	}
	name, ok := netName(w, r)
	if !ok {
		return
	}
	var req schema.PutNetMemberRequestV1
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
	var addedBy int64
	if caller, ok := callerFrom(ctx); ok {
		addedBy = caller.ID
	}
	_, err := s.store.PutNetMember(ctx, auditActor(ctx), channel.ID, name, principalID, req.Role, addedBy)
	if s.writeNetError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteNetMember serves DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}
// (operator only). Idempotent.
func (s *Server) handleDeleteNetMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principalID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	channel, ok := s.netChannel(w, r)
	if !ok {
		return
	}
	name, ok := netName(w, r)
	if !ok {
		return
	}
	_, err := s.store.RemoveNetMember(ctx, auditActor(ctx), channel.ID, name, principalID)
	if s.writeNetError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeNetError maps a store error from a net write to a response. It reports
// whether it wrote one. A store.ErrNotFound here means the net is gone: the
// handlers have already resolved the channel.
func (s *Server) writeNetError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeNetNotFound(w)
	case errors.Is(err, store.ErrNotChannelMember):
		writeError(w, http.StatusBadRequest, "not_a_channel_member", "the principal is not a member of the channel")
	default:
		slog.ErrorContext(r.Context(), "nets: write failed", "error", err)
		writeInternalError(w)
	}
	return true
}
