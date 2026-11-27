package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Local direct messages: open, resume, list and the per-viewer state (issue
// #2019, child of #2013; the wire contract is pkg/schema/dm.go, ADR-017).
//
//	POST /v1/dms                       open or resume (OpenDMRequestV1 → OpenDMResponseV1)
//	GET  /v1/dms?state=&cursor=&limit= the caller's inbox (ListDMConversationsResponseV1)
//	GET  /v1/dms/{conversation}        one conversation (DMConversationV1)
//	PUT  /v1/dms/{conversation}/state  archive, hide, reopen (SetDMStateRequestV1 → DMConversationV1)
//
// Every route is in selfMadeCredentialRoutes (provenance.go), so a credential
// an operator made for the person gets 403 own_sign_in_required before any
// handler here runs, reads included. They are not previewable and write no
// audit row (ADR-017 Decision 8: no audit row names the counterparty).
//
// The store decides visibility (dm_visibility.go) and contact (dm_policy.go);
// this file maps its answers to the fixed refusal bodies of
// schema.DMRefusalError and spends the new-conversation bucket. The refusal
// order is the schema's: the caller's standing, then the write gate (dms_off,
// only for an open that introduces the pair), then the bucket (429), then the
// target and the contact rules. An empty open is private to its opener: the
// peer's list, snapshot, stream and GET by id answer as for a conversation
// that does not exist.
//
// GET /v1/dms/snapshot is not here: the snapshot source and the frame-access
// hook that open the person stream to DMs are installed together by the issue
// that wires the stream (see personSnapshot, release gate of ADR-017).

// dmListCursorKind is the first byte of an encoded list cursor.
const (
	dmCursorEmpty = 'E' // the page is past the conversations that have a message
	dmCursorMsg   = 'M' // the page is among those that have one
)

// encodeDMCursor wraps a store cursor in the opaque, URL-safe token a client
// passes back. The token is the sort key of the last row the caller was shown
// and nothing else.
func encodeDMCursor(c store.DMListCursor) string {
	var raw string
	if c.Empty {
		raw = fmt.Sprintf("%c:%d", dmCursorEmpty, c.ID)
	} else {
		raw = fmt.Sprintf("%c:%d:%d", dmCursorMsg, c.At, c.ID)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeDMCursor parses a token this server issued. Anything else, including
// a token that decodes to a non-canonical form, is an error.
func decodeDMCursor(token string) (store.DMListCursor, error) {
	if !schemaCursorOK(token) {
		return store.DMListCursor{}, errors.New("bad cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return store.DMListCursor{}, errors.New("bad cursor")
	}
	parts := strings.Split(string(raw), ":")
	var c store.DMListCursor
	switch {
	case len(parts) == 2 && parts[0] == string(rune(dmCursorEmpty)):
		c.Empty = true
		c.ID, err = strconv.ParseInt(parts[1], 10, 64)
	case len(parts) == 3 && parts[0] == string(rune(dmCursorMsg)):
		if c.At, err = strconv.ParseInt(parts[1], 10, 64); err == nil {
			c.ID, err = strconv.ParseInt(parts[2], 10, 64)
		}
	default:
		err = errors.New("bad cursor")
	}
	if err != nil || c.ID <= 0 || c.At < 0 || (!c.Empty && c.At == 0) || encodeDMCursor(c) != token {
		return store.DMListCursor{}, errors.New("bad cursor")
	}
	return c, nil
}

// schemaCursorOK is the length and alphabet bound a cursor must meet before
// it is decoded (schema.MaxDMCursorBytes, URL-safe characters).
func schemaCursorOK(token string) bool {
	if token == "" || len(token) > schema.MaxDMCursorBytes {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// writeDMRefusal answers with the fixed body of one DM refusal code.
func writeDMRefusal(w http.ResponseWriter, status int, code string) {
	body, ok := schema.DMRefusalError(code)
	if !ok {
		body = schema.Error{Code: code, Message: code}
	}
	writeJSON(w, status, body)
}

// dmCaller is the check every DM route starts with: a verified person. The
// guard has already refused a credential another made (selfMadeCredentialRoutes);
// this keeps the agent and guest refusals of every people-only route, and
// with authentication off there is no person to hold a conversation.
func (s *Server) dmCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeDMRefusal(w, http.StatusBadRequest, schema.ErrorCodeDMRequireAuth)
		return store.Principal{}, false
	}
	switch {
	case caller.Kind != store.PrincipalHuman:
		s.auditAgentDenial(r.Context(), caller.ID, r.Pattern, "", 0, denyAgentOnHumanRoute)
		writeError(w, http.StatusForbidden, errForbidden.Code, "direct messages are for people")
		return store.Principal{}, false
	case caller.IsGuest():
		s.denyForbidden(w, r, caller, schema.ErrorCodeGuestNotAllowed, "a guest cannot do this")
		return store.Principal{}, false
	}
	return caller, true
}

// dmConversationDoc is the caller's document for a store conversation. The
// peer's name is omitted for a former member; created_at only once a message
// made the conversation visible to the peer, so it never records an open.
func dmConversationDoc(c store.DMConversation) schema.DMConversationV1 {
	doc := schema.DMConversationV1{
		Schema:         schema.DMConversationSchemaV1,
		ID:             c.ID,
		ParticipantIDs: []int64{c.Lo, c.Hi},
		PeerID:         c.PeerID,
		State:          c.State,
		Revision:       c.Revision,
		LastMessageID:  c.LastMessageID,
		LastReadID:     c.LastReadID,
		Unread:         c.Unread,
	}
	if !c.PeerFormer {
		doc.PeerName = c.PeerName
	}
	if !c.CreatedAt.IsZero() {
		at := schema.NewTimestamp(c.CreatedAt)
		doc.CreatedAt = &at
	}
	if !c.LastMessageAt.IsZero() {
		at := schema.NewTimestamp(c.LastMessageAt)
		doc.LastMessageAt = &at
	}
	return doc
}

// handleOpenDM serves POST /v1/dms.
func (s *Server) handleOpenDM(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.dmCaller(w, r)
	if !ok {
		return
	}
	var req schema.OpenDMRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be {\"peer_id\": <positive integer>}")
		return
	}
	credential, _ := credentialIDFrom(ctx)
	conv, err := s.store.OpenDMFromCredential(ctx, caller.ID, credential, req.PeerID, s.now, func(now time.Time) error {
		return s.takeDMNewToken(caller.ID, now)
	})
	var limited *rateLimitedError
	switch {
	case errors.As(err, &limited):
		writeRateLimited(w, limited)
	case errors.Is(err, store.ErrDMsOff):
		writeDMRefusal(w, http.StatusForbidden, schema.ErrorCodeDMsOff)
	case errors.Is(err, store.ErrDMSelf), errors.Is(err, store.ErrDMNotHuman):
		writeError(w, http.StatusBadRequest, "invalid_request", "that person can't be messaged")
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
	case errors.Is(err, store.ErrDMContactUnavailable):
		writeDMRefusal(w, http.StatusForbidden, schema.ErrorCodeContactUnavailable)
	case errors.Is(err, store.ErrDMNoStanding):
		writeError(w, http.StatusForbidden, errForbidden.Code, "direct messages are for people")
	case err != nil:
		slog.ErrorContext(ctx, "dms: open failed", "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, schema.OpenDMResponseV1{Conversation: dmConversationDoc(conv)})
	}
}

// handleListDMs serves GET /v1/dms: only the caller's own inbox. Hidden
// conversations are in no list, and an empty conversation the caller did not
// open is not in theirs.
func (s *Server) handleListDMs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.dmCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	state := schema.DMStateOpen
	if raw := q.Get("state"); raw != "" {
		state = schema.DMState(raw)
	}
	if state != schema.DMStateOpen && state != schema.DMStateArchived {
		writeError(w, http.StatusBadRequest, "invalid_request", "state must be open or archived")
		return
	}
	limit := schema.MaxDMConversationsPage
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = min(n, schema.MaxDMConversationsPage)
	}
	var cursor store.DMListCursor
	if raw := q.Get("cursor"); raw != "" {
		var err error
		if cursor, err = decodeDMCursor(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "cursor is not one this server issued")
			return
		}
	}
	// One more than a page tells whether there may be more.
	rows, err := s.store.ListDMConversations(ctx, caller.ID, state, cursor, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "dms: list failed", "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.ListDMConversationsResponseV1{Conversations: []schema.DMConversationV1{}}
	if len(rows) > limit {
		rows = rows[:limit]
		resp.NextCursor = encodeDMCursor(rows[limit-1].CursorAfter())
	}
	for _, c := range rows {
		resp.Conversations = append(resp.Conversations, dmConversationDoc(c))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetDM serves GET /v1/dms/{conversation}. A conversation the caller
// is not part of, one that does not exist and an empty one its peer opened
// all answer the same 404 dm_not_found, whatever the caller's role.
func (s *Server) handleGetDM(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	conv, err := s.store.DMConversationFor(r.Context(), caller.ID, id)
	switch {
	case errors.Is(err, store.ErrDMNotFound):
		writeDMRefusal(w, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	case err != nil:
		slog.ErrorContext(r.Context(), "dms: read failed", "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, dmConversationDoc(conv))
	}
}

// handleSetDMState serves PUT /v1/dms/{conversation}/state: the caller's own
// archive, hide or reopen. It changes nothing for the peer, sends the peer
// nothing and needs no contact eligibility.
func (s *Server) handleSetDMState(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.dmCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "conversation", "conversation")
	if !ok {
		return
	}
	var req schema.SetDMStateRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "state must be open, archived or hidden")
		return
	}
	credential, _ := credentialIDFrom(r.Context())
	conv, err := s.store.SetDMStateFromCredential(r.Context(), caller.ID, credential, id, req.State, s.now)
	switch {
	case errors.Is(err, store.ErrDMNotFound):
		writeDMRefusal(w, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	case errors.Is(err, store.ErrDMNoStanding):
		writeError(w, http.StatusForbidden, errForbidden.Code, "direct messages are for people")
	case err != nil:
		slog.ErrorContext(r.Context(), "dms: set state failed", "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, dmConversationDoc(conv))
	}
}
