package server

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The community member directory (ADR-013 decision 1, issue #983,
// docs/design/community-model.md §3.5 and §5).

// directoryRoute wraps the directory handler: an agent is refused (it asks
// who is on the server through MCP, or not at all), and so is a guest, active
// or ended (guest_not_allowed; no route admits a guest since #1005, so this
// guards the kept guest tables and an ended guest's replayed token).
func (s *Server) directoryRoute(next http.Handler) http.Handler {
	return s.humansOnlyBecause(denyAgentOnHumanRoute, "agents cannot read the member directory",
		s.membersOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if caller, ok := callerFrom(r.Context()); ok && caller.GuestState == schema.GuestStateEnded {
				s.denyForbidden(w, r, caller, schema.ErrorCodeGuestNotAllowed, "a guest cannot do this")
				return
			}
			next.ServeHTTP(w, r)
		})))
}

// handleListDirectoryMembers serves GET /v1/directory/members: one page of
// the community's members and enabled, non-hidden agents, ordered by id,
// as ServerMemberV1. Every viewer sees id, kind, name, role and created_at;
// a viewer holding manage_members also sees the operator fields. Whether the
// list is everyone or only the people the viewer may know is decided by
// see_member_directory, through the store's one may-know entry point.
func (s *Server) handleListDirectoryMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	after, err := nonNegativeQueryInt64(r, "after", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := nonNegativeQueryInt64(r, "limit", schema.DefaultListPrincipalsLimit)
	if err != nil || limit == 0 || limit > schema.MaxListPrincipalsLimit {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("limit must be between 1 and %d", schema.MaxListPrincipalsLimit))
		return
	}
	// With authentication off there is no viewer: the server is open, and
	// the whole directory is shown as every operator route is.
	viewer := store.DirectoryViewer{SeeAll: true, Operator: true}
	if caller, ok := callerFrom(ctx); ok {
		viewer = store.DirectoryViewer{ID: caller.ID}
		var ok bool
		if viewer.SeeAll, ok = s.canOrFail(w, r, caller, schema.PermSeeMemberDirectory); !ok {
			return
		}
		if viewer.Operator, ok = s.canOrFail(w, r, caller, schema.PermManageMembers); !ok {
			return
		}
	}
	page, err := s.store.ListDirectory(ctx, viewer, after, int(limit))
	if err != nil {
		slog.ErrorContext(ctx, "directory: list members failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
