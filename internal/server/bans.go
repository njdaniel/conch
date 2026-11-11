package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Channel entry bans (docs/design/moderation.md §6, issue #851). The three
// routes are in the accessChannelManager class: the guard admits an owner,
// an operator holding manage_channels, or a manager of the channel who is a
// member, and answers the uniform 404 to anyone else before the target is
// read. The store re-reads the actor's standing and applies the target rules
// under its write lock (BanChannelMember, UnbanChannelMember). A ban,
// like a membership removal, is reduce-only, and it is allowed in an archived
// channel.

// handlePutChannelBan serves PUT /v1/channels/{channel}/bans/{principal_id}:
// the ban and the removal of membership and net seats are one transaction;
// then the removal's live follow-up runs whatever the ban row did, so a
// repeat that cleaned a residual row revokes its sockets and voice too.
func (s *Server) handlePutChannelBan(w http.ResponseWriter, r *http.Request) {
	s.serveChannelBan(w, r, func(c banCall) (store.BanResult, error) {
		return s.store.BanChannelMember(c.r.Context(), auditActor(c.r.Context()), c.channel.ID, c.target, c.actor)
	})
}

// handleDeleteChannelBan serves DELETE /v1/channels/{channel}/bans/{principal_id}:
// residual membership goes first, then the ban; nothing is restored.
func (s *Server) handleDeleteChannelBan(w http.ResponseWriter, r *http.Request) {
	s.serveChannelBan(w, r, func(c banCall) (store.BanResult, error) {
		return s.store.UnbanChannelMember(c.r.Context(), auditActor(c.r.Context()), c.channel.ID, c.target, c.actor)
	})
}

// banCall is what a ban write is asked.
type banCall struct {
	r       *http.Request
	channel store.Channel
	target  int64
	actor   int64
}

func (s *Server) serveChannelBan(w http.ResponseWriter, r *http.Request, write func(banCall) (store.BanResult, error)) {
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
		slog.ErrorContext(ctx, "bans: find channel failed", "error", err)
		writeInternalError(w)
		return
	}
	var callerID int64
	if caller, ok := callerFrom(ctx); ok {
		callerID = caller.ID
	}
	res, err := write(banCall{r: r, channel: channel, target: targetID, actor: callerID})
	if s.writeMemberError(w, r, err) {
		return
	}
	// The follow-up of a removal, always, as DELETE .../members does: no
	// subscription outlives the principal's access, whatever this call wrote.
	// It runs after the commit; the ban is already in force for every new
	// request, and the voice entitlement check refuses a token not yet signed.
	s.hub.DropPrincipal(channel.ID, targetID)
	s.events.dropPrincipal(channel.ID, targetID)
	s.events.notifyChannelRoster(channel.ID)
	if res.Cleaned {
		s.events.notifyChannelsChanged(targetID)
		s.directoryMembershipChanged(ctx, channel.ID, targetID)
	}
	s.voiceMemberRemoved(ctx, channel.ID, targetID)
	w.WriteHeader(http.StatusNoContent)
}

// handleListChannelBans serves GET /v1/channels/{channel}/bans, ordered by
// principal id. The route guard is the authorization; the names are read now.
func (s *Server) handleListChannelBans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "bans: find channel failed", "error", err)
		writeInternalError(w)
		return
	}
	var callerID int64
	if caller, ok := callerFrom(ctx); ok {
		callerID = caller.ID
	}
	bans, err := s.store.ListChannelBans(ctx, channel.ID, callerID)
	if err != nil {
		slog.ErrorContext(ctx, "bans: list failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	out := schema.ListChannelBansResponseV1{Bans: make([]schema.ChannelBanV1, 0, len(bans))}
	for _, b := range bans {
		out.Bans = append(out.Bans, schema.ChannelBanV1{
			PrincipalID: b.PrincipalID, Name: b.Name, CreatedBy: b.CreatedBy, CreatedAt: schema.NewTimestamp(b.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
