package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Account notices (issue #2042, child of #2013; Nick's decision B of
// 2026-11-19): GET /v1/notices/mine lists what the caller has not dismissed
// and DELETE /v1/notices/mine/{id} dismisses one. The store writes a notice
// when a password is set through an enrolment or reset link another
// principal made (store/account_notices.go).
//
// The notices are the member's own and no one else's. Every read and every
// dismissal names the caller's principal id, so another member's notice is
// never listed and its id is a 404. And the caller must be signed in on a
// credential they made themselves (provenance.go): a credential an operator
// issued, or a link an operator made, sees an empty list and cannot dismiss,
// so the operator who set a password cannot read the notice about it or
// clear it by signing in as the member through a link. A previewed request
// (Conch-Preview-As) reads as the member and is refused the same way. Both
// answers are the same as for a member with no notices, so nothing tells
// them apart; none is audited, since the web client asks on every page load.

// ownNoticeCaller returns the principal whose notices this request may read
// or dismiss, or ok false when it has none: authentication off, no
// credential, an agent, a guest, a previewed request or a credential made by
// another. A store error has been answered (500) and also returns ok false,
// with answered true.
func (s *Server) ownNoticeCaller(w http.ResponseWriter, r *http.Request) (caller store.Principal, ok, answered bool) {
	ctx := r.Context()
	if !s.cfg.authRequired() {
		return store.Principal{}, false, false
	}
	caller, has := callerFrom(ctx)
	if !has {
		writeUnauthenticated(w)
		return store.Principal{}, false, true
	}
	if _, previewed := previewFrom(ctx); previewed || caller.Kind != store.PrincipalHuman || caller.IsGuest() {
		return store.Principal{}, false, false
	}
	self, err := s.requestCredentialSelfMade(ctx)
	switch {
	case errors.Is(err, errNoRequestCredential), errors.Is(err, store.ErrCredentialNotFound):
		writeUnauthenticated(w)
		return store.Principal{}, false, true
	case err != nil:
		slog.ErrorContext(ctx, "notices: read credential failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return store.Principal{}, false, true
	}
	return caller, self, false
}

// handleListAccountNotices serves GET /v1/notices/mine.
func (s *Server) handleListAccountNotices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok, answered := s.ownNoticeCaller(w, r)
	if answered {
		return
	}
	notices := []schema.AccountNoticeV1{}
	if ok {
		var err error
		if notices, err = s.store.ListAccountNotices(ctx, caller.ID); err != nil {
			slog.ErrorContext(ctx, "notices: list failed", "principal", caller.ID, "error", err)
			writeInternalError(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, schema.ListAccountNoticesResponseV1{Notices: notices})
}

// handleDismissAccountNotice serves DELETE /v1/notices/mine/{id}: 204 for
// the caller's own notice (one already dismissed too), 404 for anything else
// (another's id, an id never issued, or a caller who may not read notices).
func (s *Server) handleDismissAccountNotice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok, answered := s.ownNoticeCaller(w, r)
	if answered {
		return
	}
	id, idOK := pathID(w, r, "id", "notice")
	if !idOK {
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, schema.ErrorCodeNoticeNotFound, "notice not found")
		return
	}
	switch err := s.store.DismissAccountNotice(ctx, caller.ID, id, s.now()); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeNoticeNotFound, "notice not found")
	case err != nil:
		slog.ErrorContext(ctx, "notices: dismiss failed", "principal", caller.ID, "notice", id, "error", err)
		writeInternalError(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
