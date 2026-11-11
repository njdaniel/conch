package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Notification preferences and the pending feed (issue #1713, Mobile M11-A;
// docs/design/mobile-notifications.md §4.3, §6.1-§6.3, §9.2). This is the O1
// path: it needs no push transport and no flag, and sends nothing anywhere. A
// client reads and writes its sign-in's preferences, and when it is opened
// fetches what it would have been told about.
//
//	GET /v1/notifications/preferences
//	PUT /v1/notifications/preferences
//	GET /v1/notifications/pending?after=<message id>
//
// The preferences are the calling credential's own: no id is in the path, so
// no caller can name another's. The feed reads messages only through
// store.ListPendingNotifications, which embeds the visibility predicate and
// the reader's live membership in its one statement; this file reads no
// message row (TestMessageReadsStayInTheVisibilityFunction holds it to
// that), and so a member learns nothing of what is said where they are not.
//
// The routes are for people with a full credential: an agent is refused with
// 403 human_required (as read state), a guest with 403 guest_not_allowed
// (membersOnly, in routes.go), a setup-scope credential by the
// authentication layer (these routes are not in setupScopeRoutes), and a
// request with no credential is 401. They are not previewable and not in the
// admin catalog. Reads and writes are rate limited per credential and per
// principal (notification_limits.go), before the store is touched. Nothing
// here logs a token, a preference's value or a message's text.

// notificationCaller is the check every notification route starts with: a
// verified human with a credential. With authentication off there is no
// credential to hold preferences, and the answer is read state's.
func notificationCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth, "notifications require authentication")
		return store.Principal{}, 0, false
	}
	if caller.Kind != store.PrincipalHuman {
		writeError(w, http.StatusForbidden, schema.ErrorCodeHumanRequired, "notifications are for human principals only")
		return store.Principal{}, 0, false
	}
	credentialID, ok := credentialIDFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth, "notifications require authentication")
		return store.Principal{}, 0, false
	}
	return caller, credentialID, true
}

// handleGetNotificationPreferences serves GET /v1/notifications/preferences:
// the calling credential's preferences, or the defaults when it has written
// none.
func (s *Server) handleGetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	_, credentialID, ok := notificationCaller(w, r)
	if !ok || !s.takeNotificationReadToken(w, credentialID) {
		return
	}
	prefs, err := s.store.GetNotificationPreferences(r.Context(), credentialID)
	if err != nil {
		slog.ErrorContext(r.Context(), "notifications: read preferences failed", "credential", credentialID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// handlePutNotificationPreferences serves PUT /v1/notifications/preferences:
// the body replaces the whole object, and the answer is what is stored,
// normalised. A body that is not valid is 400 with nothing stored. The
// refusal of an unknown policy does not repeat the value.
func (s *Server) handlePutNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	var req schema.PutNotificationPreferencesRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	// Validate, then store: the store normalises (deduplicates and orders
	// muted) and writes. A missing or null muted fails Validate, as a
	// missing policy does.
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Spent after the body is known to be valid, so a malformed request
	// spends nothing; still before the store is touched.
	if !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	prefs, err := s.store.PutNotificationPreferences(ctx, credentialID, schema.NotificationPreferencesV1(req), s.now())
	if errors.Is(err, store.ErrNotFound) {
		// The credential was revoked between authentication and the write.
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "notifications: write preferences failed", "credential", credentialID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// handleListPendingNotifications serves GET /v1/notifications/pending: one
// page of what is new for the calling credential's reader, computed now by
// the predicates the message routes use. The only parameter is after; the
// feed takes no registration id, and the credential scopes it (§4.3).
func (s *Server) handleListPendingNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, credentialID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	after, err := pendingAfter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.takeNotificationReadToken(w, credentialID) {
		return
	}
	// This credential has just read its feed: its registrations are not
	// stale (design §5.8). The update is best effort: a failure is logged,
	// with no target in it, and the feed is answered all the same.
	if err := s.store.TouchPushFetch(ctx, credentialID, s.now()); err != nil {
		slog.WarnContext(ctx, "notifications: could not record the fetch", "credential", credentialID, "error", err)
	}
	// The reader is built here, in the call, as the guard test requires.
	page, err := s.store.ListPendingNotifications(ctx, readerFor(r, apiV2), credentialID, after)
	if err != nil {
		slog.ErrorContext(ctx, "notifications: pending feed failed", "credential", credentialID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// pendingAfter reads the feed's one parameter: after, optional, a
// non-negative integer, given at most once. Any other parameter is refused,
// so a client that sends a registration id learns it is not wanted.
func pendingAfter(r *http.Request) (int64, error) {
	query := r.URL.Query()
	for name := range query {
		if name != "after" {
			return 0, errors.New("the only query parameter is after")
		}
	}
	values := query["after"]
	switch len(values) {
	case 0:
		return 0, nil
	case 1:
		after, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || after < 0 {
			return 0, errors.New("after must be a non-negative integer")
		}
		return after, nil
	default:
		return 0, errors.New("after must be given once")
	}
}
