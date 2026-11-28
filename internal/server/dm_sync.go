package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// dmSnapshot builds the same viewer-specific recovery document for REST and
// for a newly subscribed person socket.
func (s *Server) dmSnapshot(ctx context.Context, reader int64) (personDMSnapshot, error) {
	rows, enabled, err := s.store.DMSnapshotFor(ctx, reader)
	if err != nil {
		return personDMSnapshot{}, err
	}
	more := len(rows) > schema.MaxDMSnapshotConversations
	if more {
		rows = rows[:schema.MaxDMSnapshotConversations]
	}
	snap := personDMSnapshot{
		doc: schema.DMSnapshotV1{
			Schema:        schema.DMSnapshotSchemaV1,
			Conversations: make([]schema.DMConversationV1, 0, len(rows)),
			More:          more,
			TakenAt:       schema.NewTimestamp(s.now()),
			DMsEnabled:    enabled,
		},
		emptyViewers: make(map[int64]int64),
	}
	for _, c := range rows {
		snap.doc.Conversations = append(snap.doc.Conversations, dmConversationDoc(c))
		if c.CreatedAt.IsZero() {
			// A conversation without messages needs the reader's own viewer row.
			snap.emptyViewers[c.ID] = reader
		}
	}
	return snap, nil
}

func (s *Server) handleDMSnapshot(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	snap, err := s.personSnapshot(r.Context(), caller.ID)
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleListDMChanges(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	after, err := nonNegativeQueryInt64(r, "after", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "after must be a nonnegative revision")
		return
	}
	requested, err := nonNegativeQueryInt64(r, "limit", schema.DefaultDMPageSize)
	if err != nil || requested == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
		return
	}
	limit := int(min(requested, schema.MaxDMChangesPage))
	rows, err := s.store.ListDMChanges(r.Context(), caller.ID, id, after, limit+1)
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	resp := schema.ListDMChangesResponseV1{}
	if len(rows) > limit {
		rows = rows[:limit]
		resp.NextAfter = rows[limit-1].Revision
	}
	resp.Messages = make([]schema.DMMessageV1, len(rows))
	for i, msg := range rows {
		resp.Messages[i] = dmMessageFromStore(msg)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMarkDMRead(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	var req schema.MarkReadRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "last_read_id must be a nonnegative integer")
		return
	}
	if err := s.dmReadLimits.take(caller.ID, s.now()); err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	ctx := r.Context()
	credential, _ := credentialIDFrom(ctx)
	mark, changed, err := s.store.MarkDMReadFromCredential(ctx, caller.ID, credential, id, req.LastReadID, s.now)
	if err != nil {
		writeDMMessageError(w, r, err)
		return
	}
	if changed {
		s.publishDMRead(ctx, caller.ID, mark.ConversationID)
	}
	resp := schema.DMReadStateV1{
		Schema:         schema.DMReadStateSchemaV1,
		ConversationID: mark.ConversationID,
		LastReadID:     mark.LastReadID,
		Unread:         mark.Unread,
	}
	if mark.LastReadID > 0 {
		at := schema.NewTimestamp(mark.UpdatedAt)
		resp.UpdatedAt = &at
	}
	writeJSON(w, http.StatusOK, resp)
}

// A mark only changes the reader's own projection. Check live access before
// enqueuing the signal, and again on the socket before delivery.
func (s *Server) publishDMRead(ctx context.Context, reader, convID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsRecheckTimeout)
	defer cancel()
	allowed, err := s.store.DMFrameAllowed(ctx, reader, convID)
	if err != nil {
		slog.ErrorContext(ctx, "dms: check read signal access", "error", err)
		return
	}
	if allowed {
		s.publishDMFrame(ctx, reader, readerDMChange{
			DMChangedV1: schema.DMChangedV1{
				Schema:         schema.DMChangedSchemaV1,
				ConversationID: convID,
				Kind:           schema.DMChangeRead,
			},
			readerID: reader,
		})
	}
}

// publishDMMessage runs after the write commits, even if its HTTP client
// disconnected. The store checks both viewers before anything is enqueued.
func (s *Server) publishDMMessage(ctx context.Context, msg store.DMMessage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsRecheckTimeout)
	defer cancel()
	readers, err := s.store.DMMessageFrameRecipients(ctx, msg.AuthorID, msg.ConversationID)
	if err != nil {
		slog.ErrorContext(ctx, "dms: find message frame recipients", "error", err)
		return
	}
	for _, reader := range readers {
		doc := dmMessageFromStore(msg)
		if reader != msg.AuthorID {
			doc.ClientNonce = ""
		}
		var frame personDoc = doc
		switch {
		case doc.Removed:
			frame = schema.DMMessageRemovedV1{Schema: schema.DMMessageRemovedSchemaV1, Message: doc}
		case doc.EditedAt != nil:
			frame = schema.DMMessageEditedV1{Schema: schema.DMMessageEditedSchemaV1, Message: doc}
		}
		s.publishDMFrame(ctx, reader, frame)
	}
}

func (s *Server) publishDMFrame(ctx context.Context, reader int64, frame personDoc) {
	if err := s.person.publish(reader, frame); err != nil {
		slog.ErrorContext(ctx, "dms: publish person stream frame", "error", err)
	}
}
