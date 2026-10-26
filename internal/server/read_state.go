package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Read state (issue #351; UX-02, UX-08). A read mark is how far a person has
// read a channel, and the unread count is how many messages after it they may
// see. The mark is the principal's, so a person's two clients share it.
//
// The count goes through store.CountVisibleUnreadMessagesAfter and the clamp inside
// store.MarkRead, both of which embed the one visibility condition of
// store/audience.go: a whisper or net message the caller is not in is never
// counted and never moves a mark. The caller's own posts are not unread. This file reads no message rows itself
// (TestReadStateHandlersCountThroughTheStore pins that).
//
// Read state is for people. An agent has no read state (its cursors are its
// own), so both routes refuse an agent with human_required. A mark is not an
// action on anyone else, so it is not audited. The events socket sends the same
// documents after posts and marks; GET /v1/read-state remains the reconnect read.

// readStateCaller is the check both routes start with: a verified human. With
// authentication off there is no one to hold a mark for.
func readStateCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth, "read state requires authentication")
		return store.Principal{}, false
	}
	if caller.Kind != store.PrincipalHuman {
		writeError(w, http.StatusForbidden, schema.ErrorCodeHumanRequired, "read state is for human principals only")
		return store.Principal{}, false
	}
	return caller, true
}

// readStateDoc builds the document for one channel from a stored mark (the
// zero ReadMark when none) and the caller's reader. The count is capped inside
// the store's statement.
func (s *Server) readStateDoc(r *http.Request, channelID int64, mark store.ReadMark) (schema.ReadStateV1, error) {
	// The reader is built here, in the call, as the guard test requires.
	unread, err := s.store.CountVisibleUnreadMessagesAfter(r.Context(), channelID, readerFor(r, apiV2), mark.LastReadID, schema.MaxUnreadCount)
	if err != nil {
		return schema.ReadStateV1{}, err
	}
	doc := schema.ReadStateV1{
		Schema:     schema.ReadStateSchemaV1,
		ChannelID:  channelID,
		LastReadID: mark.LastReadID,
		Unread:     unread,
	}
	// A mark of 0 has not moved, whatever row stores it.
	if mark.LastReadID > 0 {
		at := schema.NewTimestamp(mark.UpdatedAt)
		doc.UpdatedAt = &at
	}
	return doc, nil
}

// handleListReadState serves GET /v1/read-state: one document per channel the
// caller is a member of, with the unread count computed now. A channel the
// caller has left is simply absent.
func (s *Server) handleListReadState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := readStateCaller(w, r)
	if !ok {
		return
	}
	channels, err := s.store.ListChannelsForPrincipal(ctx, caller.ID)
	var marks map[int64]store.ReadMark
	if err == nil {
		marks, err = s.store.ReadMarksFor(ctx, caller.ID)
	}
	if err != nil {
		slog.ErrorContext(ctx, "read state: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	resp := schema.ListReadStateResponseV1{ReadState: make([]schema.ReadStateV1, 0, len(channels))}
	for _, ch := range channels {
		doc, err := s.readStateDoc(r, ch.ID, marks[ch.ID])
		if err != nil {
			slog.ErrorContext(ctx, "read state: count failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		resp.ReadState = append(resp.ReadState, doc)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePutReadState serves PUT /v1/channels/{channel}/read-state. The mark
// only moves forward and is clamped to the newest message the caller may see
// (store.MarkRead); either way the answer is the stored state. A non-member
// and an unknown channel get the same channel_not_found as every channel read.
func (s *Server) handlePutReadState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := readStateCaller(w, r)
	if !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	var req schema.MarkReadRequestV1
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
	mark, err := s.store.MarkRead(ctx, channel.ID, readerFor(r, apiV2), req.LastReadID, s.now())
	var doc schema.ReadStateV1
	if err == nil {
		doc, err = s.readStateDoc(r, channel.ID, mark)
	}
	if err != nil {
		slog.ErrorContext(ctx, "read state: mark failed", "channel", channel.ID, "principal", caller.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, doc)
	s.events.notifyReadMarked(channel.ID, caller.ID)
}
