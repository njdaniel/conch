package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Personal blocks (issue #1832, docs/design/blocks.md).
//
//	GET    /v1/blocks                  the caller's list
//	PUT    /v1/blocks/{principal_id}   block (idempotent)
//	DELETE /v1/blocks/{principal_id}   unblock (idempotent)
//
// The list is the calling principal's own: no route names an owner, so no
// caller, operator or owner of the server included, can read or change
// another's. Every caller gets the same kind of answer about their own list
// whoever else blocks them, so nothing here tells the blocked principal
// anything: they read an empty list of their own, and a block changes no
// other route, frame, roster or count (blocks_isolation_test.go).
//
// The routes are for people with a full credential: an agent is refused with
// 403 human_required, a guest with 403 guest_not_allowed (membersOnly, in
// routes.go; a guest is short-lived and keeps no list), and a request with no
// credential is 401. They are not previewable, not in the admin catalog, and
// write no audit row. Reads and writes share the notification preferences'
// limits (notification_limits.go): reads per credential, writes per
// principal, spent before the store is touched. Nothing here logs whom
// anyone blocks.
//
// A change that took effect sends conch.blocks_changed.v1 to the caller's own
// events sockets, so their other devices re-read the list.

// blockCaller is the check every block route starts with: a verified human
// with a credential.
func blockCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		// Authentication is off: there is no principal to hold a list. The
		// answer is read state's and the notification routes'.
		writeError(w, http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth, "blocks require authentication")
		return store.Principal{}, 0, false
	}
	if caller.Kind != store.PrincipalHuman {
		writeError(w, http.StatusForbidden, schema.ErrorCodeHumanRequired, "blocks are for human principals only")
		return store.Principal{}, 0, false
	}
	credentialID, ok := credentialIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth, "blocks require authentication")
		return store.Principal{}, 0, false
	}
	return caller, credentialID, true
}

// handleListBlocks serves GET /v1/blocks: the caller's list, oldest first.
func (s *Server) handleListBlocks(w http.ResponseWriter, r *http.Request) {
	caller, credentialID, ok := blockCaller(w, r)
	if !ok || !s.takeNotificationReadToken(w, credentialID) {
		return
	}
	blocks, err := s.store.ListBlocks(r.Context(), caller.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "blocks: list failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.ListBlocksResponseV1{Blocks: blocks, Limit: schema.MaxBlocks})
}

// handlePutBlock serves PUT /v1/blocks/{principal_id}: 200 with the entry,
// new or as it was. The caller themself is 400; an id that is no principal,
// a hidden one, or one the caller's directory would not show them is 404
// principal_not_found, all alike; a full list is 409 block_limit_reached.
func (s *Server) handlePutBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, _, ok := blockCaller(w, r)
	if !ok {
		return
	}
	targetID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	// Whom the caller may name is the directory's rule: everyone with
	// see_member_directory, otherwise channel-mates and their own joiners.
	// Anyone else is 404 exactly as an id that is nobody, so a PUT is no
	// probe for principals or their names.
	seeAll, ok := s.canOrFail(w, r, caller, schema.PermSeeMemberDirectory)
	if !ok {
		return
	}
	if !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	entry, created, err := s.store.PutBlock(ctx, caller.ID, targetID, seeAll, s.now())
	switch {
	case errors.Is(err, store.ErrBlockSelf):
		writeError(w, http.StatusBadRequest, "invalid_request", "you cannot block yourself")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrBlockLimit):
		writeError(w, http.StatusConflict, schema.ErrorCodeBlockLimitReached, "your block list is full; unblock someone first")
		return
	case err != nil:
		slog.ErrorContext(ctx, "blocks: put failed", "error", err)
		writeInternalError(w)
		return
	}
	if created {
		s.events.notifyBlocksChanged(caller.ID)
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleDeleteBlock serves DELETE /v1/blocks/{principal_id}: 204 whether or
// not the principal was blocked, and whether or not they exist.
func (s *Server) handleDeleteBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, _, ok := blockCaller(w, r)
	if !ok {
		return
	}
	targetID, ok := pathID(w, r, "principal_id", "principal")
	if !ok {
		return
	}
	if !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	removed, err := s.store.DeleteBlock(ctx, caller.ID, targetID)
	if err != nil {
		slog.ErrorContext(ctx, "blocks: delete failed", "error", err)
		writeInternalError(w)
		return
	}
	if removed {
		s.events.notifyBlocksChanged(caller.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}
