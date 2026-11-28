package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// dmMessageFromStore uses the store's viewer-specific projection, including the
// tombstone for a removal and the nonce only for the author.
func dmMessageFromStore(m store.DMMessage) schema.DMMessageV1 {
	doc := schema.DMMessageV1{
		Schema:         schema.DMMessageSchemaV1,
		ID:             m.ID,
		ConversationID: m.ConversationID,
		AuthorID:       m.AuthorID,
		CreatedAt:      schema.NewTimestamp(m.CreatedAt),
		Revision:       m.Revision,
		Body:           m.Body,
		FileIDs:        m.FileIDs,
		Removed:        m.Removed,
		ClientNonce:    m.ClientNonce,
	}
	if !m.EditedAt.IsZero() {
		at := schema.NewTimestamp(m.EditedAt)
		doc.EditedAt = &at
	}
	return doc
}

func (s *Server) handleListDMMessages(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	page, ok := messageOrder(w, r)
	if !ok {
		return
	}
	requested, err := nonNegativeQueryInt64(r, "limit", schema.DefaultDMPageSize)
	if err != nil || requested == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
		return
	}
	limit := int(min(requested, schema.MaxDMPageSize))
	rows, err := s.store.ListDMMessagesPage(r.Context(), caller.ID, id, page, limit+1)
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	resp := schema.ListDMMessagesResponseV1{Order: schema.DMOrderAsc}
	if page.NewestFirst {
		resp.Order = schema.DMOrderDesc
	}
	if len(rows) > limit {
		rows = rows[:limit]
		if page.NewestFirst {
			resp.NextBefore = rows[limit-1].ID
		} else {
			resp.NextAfter = rows[limit-1].ID
		}
	}
	resp.Messages = make([]schema.DMMessageV1, len(rows))
	for i, m := range rows {
		resp.Messages[i] = dmMessageFromStore(m)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePostDMMessage(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	var req schema.PostDMMessageRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid direct message body, client nonce or attachments")
		return
	}
	if err := s.takeDMContactWriteToken(r, caller.ID, s.dmSendLimits); err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	ctx := r.Context()
	credential, _ := credentialIDFrom(ctx)
	post := store.DMPost{ConversationID: id, AuthorID: caller.ID, Body: req.Body, FileIDs: req.FileIDs, ClientNonce: req.ClientNonce}
	msg, replayed, err := s.store.InsertDMMessageFromCredential(ctx, post, credential, s.now,
		func(now time.Time) error { return s.takeDMNewToken(caller.ID, now) })
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, schema.PostDMMessageResponseV1{Message: dmMessageFromStore(msg), Replayed: replayed})
}

func (s *Server) handleEditDMMessage(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	message, ok := pathID(w, r, "message", "message")
	if !ok {
		return
	}
	var req schema.EditDMMessageRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain only the new message text")
		return
	}
	if err := s.takeDMContactWriteToken(r, caller.ID, s.dmEditLimits); err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	ctx := r.Context()
	credential, _ := credentialIDFrom(ctx)
	edit, err := s.store.EditDMMessageFromCredential(ctx, caller.ID, credential, id, message, req.Body, s.now)
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, schema.EditDMMessageResponseV1{Message: dmMessageFromStore(edit.Message)})
}

func (s *Server) handleRemoveDMMessage(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	message, ok := pathID(w, r, "message", "message")
	if !ok {
		return
	}
	if err := s.dmRemoveLimits.take(caller.ID, s.now()); err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	ctx := r.Context()
	credential, _ := credentialIDFrom(ctx)
	if _, err := s.store.RemoveDMMessageFromCredential(ctx, caller.ID, credential, id, message, s.now); err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// takeDMContactWriteToken checks the DM switches before charging sends and
// edits. The store rechecks the switches under the write lock.
func (s *Server) takeDMContactWriteToken(r *http.Request, callerID int64, limit *dmWriteLimiter) error {
	enabled, err := s.store.DMsEnabledFor(r.Context(), callerID)
	if err != nil {
		return err
	}
	if !enabled {
		return store.ErrDMsOff
	}
	return limit.take(callerID, s.now())
}

func writeDMMessageError(w http.ResponseWriter, r *http.Request, err error) {
	var limited *rateLimitedError
	switch {
	case errors.Is(err, store.ErrCredentialInvalid):
		writeUnauthenticated(w)
	case errors.Is(err, store.ErrDMsOff):
		writeDMRefusal(w, http.StatusForbidden, schema.ErrorCodeDMsOff)
	case errors.As(err, &limited):
		writeRateLimited(w, limited)
	case errors.Is(err, store.ErrDMNotFound):
		writeDMRefusal(w, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	case errors.Is(err, store.ErrDMHistoryCursor):
		writeError(w, http.StatusBadRequest, "invalid_request", "cursor must identify a message in this conversation")
	case errors.Is(err, store.ErrDMContactUnavailable):
		writeDMRefusal(w, http.StatusForbidden, schema.ErrorCodeContactUnavailable)
	case errors.Is(err, store.ErrDMNonceConflict):
		writeDMRefusal(w, http.StatusConflict, schema.ErrorCodeDMNonceConflict)
	case errors.Is(err, store.ErrDMFileNotAttachable):
		writeDMRefusal(w, http.StatusConflict, schema.ErrorCodeDMFileNotAttachable)
	case errors.Is(err, store.ErrDMNotAuthor):
		writeError(w, http.StatusForbidden, errForbidden.Code, "only the author can change a direct message")
	case errors.Is(err, store.ErrDMEmptyBody):
		writeError(w, http.StatusBadRequest, "invalid_request", "direct message text must not be blank")
	case errors.Is(err, store.ErrMessageNotFound):
		writeJSON(w, http.StatusNotFound, errMessageNotFound)
	case errors.Is(err, store.ErrMessageNotEditable),
		errors.Is(err, store.ErrMessageEditWindowClosed),
		errors.Is(err, store.ErrMessageEditLimitReached):
		writeMessageEditError(w, messageEditStoreError(r, err))
	default:
		slog.ErrorContext(r.Context(), "dms: message operation failed", "method", r.Method, "error", err)
		writeInternalError(w)
	}
}
