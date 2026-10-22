package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	defaultMessageLimit = 50
	maxMessageLimit     = 100
	// What a client must be able to read back is derived from this limit
	// (schema.MaxMessageFrameBytes), which is why it is the schema's.
	maxMessageBodyBytes = schema.MaxPostRequestBytes
)

// apiVersion is the protocol generation a request speaks. It decides the
// envelope and, for reads, whether the reader may be given scoped messages:
// only v2 can express an audience.
type apiVersion int

const (
	apiV0 apiVersion = iota
	apiV1
	apiV2
)

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req schema.PostMessageRequest
	if !s.decodePostRequest(w, r, &req) || !s.bindAuthor(w, r, &req.AuthorID) {
		return
	}
	s.postMessage(w, r, req.AuthorID, req.Body, nil, apiV0)
}

func (s *Server) handlePostMessageV1(w http.ResponseWriter, r *http.Request) {
	var req schema.PostMessageRequestV1
	if !s.decodePostRequest(w, r, &req) {
		return
	}
	// Bind before validation: Validate rejects author_id <= 0, but an
	// authenticated caller may omit it.
	if !s.bindAuthor(w, r, &req.AuthorID) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.postMessage(w, r, req.AuthorID, req.Body, req.Payload, apiV1)
}

func (s *Server) decodePostRequest(w http.ResponseWriter, r *http.Request, req any) bool {
	if err := decodeJSONBody(w, r, req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return false
	}
	return true
}

func (s *Server) postMessage(
	w http.ResponseWriter, r *http.Request, authorID int64, body string, payload *schema.Payload, version apiVersion,
) {
	ctx := r.Context()
	// Membership is enforced here: a non-member gets the unknown-channel 404.
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesPost, schema.ChannelPermissionPost)
	if !ok {
		return
	}

	// Attaching a file publishes something only its uploader could read, so
	// it takes the uploader's own verified credential (issue #23). Two posts
	// arrive here without one. A webhook posts as its principal without being
	// that principal: whoever holds the hook's token could otherwise attach
	// the principal's unattached uploads to a channel-wide message and read
	// them. And with authentication off the author is whatever the body says.
	// Neither may attach; the store's "uploaded by the author" check cannot
	// tell them from the author.
	if payload != nil && payload.Schema == schema.FileAttachmentV1Name {
		if _, verified := callerFrom(ctx); !verified {
			writeError(w, http.StatusBadRequest, errFilesNeedAuth.Code, "attaching a file requires the uploader's own credential")
			return
		}
	}

	if authorID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "author_id must be positive")
		return
	}
	if strings.TrimSpace(body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	author, err := s.store.PrincipalByID(ctx, authorID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "author_not_found", "author not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "messages: find author failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	stored, err := s.store.InsertMessageV1(ctx, channel.ID, authorID, body, payload)
	if errors.Is(err, store.ErrFileAttachment) {
		// Nothing was stored, and the answer is the same whichever file was
		// at fault (issue #23).
		writeError(w, http.StatusBadRequest, errInvalidFileAttachment.Code, errInvalidFileAttachment.Message)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: insert failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	s.metrics.messagePosted(author.Kind)
	messageV1 := messageV1FromStore(stored)
	messageV2 := messageV2FromStore(stored)
	// Persist-then-broadcast: the message is durable before anyone hears
	// about it, so a crash here loses delivery, never data. A guest's post
	// carries its attribution (issue #705), which v1 cannot: v2 sockets get
	// the v2 envelope itself, as for a reply.
	if stored.GuestAuthor != nil {
		s.hub.BroadcastReply(ctx, messageV1, messageV2)
	} else {
		s.hub.BroadcastMessageV1(ctx, messageV1)
	}
	messageV0 := messageV0FromStore(stored)
	s.hub.BroadcastMessage(ctx, messageV0)
	s.broadcaster.BroadcastMessage(ctx, messageV0)
	switch version {
	case apiV2:
		writeJSON(w, http.StatusCreated, schema.PostMessageResponseV2{Message: messageV2})
	case apiV1:
		writeJSON(w, http.StatusCreated, schema.PostMessageResponseV1{Message: messageV1})
	default:
		writeJSON(w, http.StatusCreated, schema.PostMessageResponse{Message: messageV0})
	}
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	s.listMessages(w, r, apiV0)
}

func (s *Server) handleListMessagesV1(w http.ResponseWriter, r *http.Request) {
	s.listMessages(w, r, apiV1)
}

func (s *Server) handleListMessagesV2(w http.ResponseWriter, r *http.Request) {
	s.listMessages(w, r, apiV2)
}

// readerFor is the visibility reader for a request: the verified caller, and
// whether the protocol it speaks can express an audience. With no caller
// (authentication off) there is no verified reader, so it sees channel-wide
// messages only whatever the version.
func readerFor(r *http.Request, version apiVersion) store.Reader {
	reader := store.Reader{Scoped: version == apiV2}
	if caller, ok := callerFrom(r.Context()); ok {
		reader.PrincipalID = caller.ID
	}
	return reader
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, version apiVersion) {
	ctx := r.Context()
	// Membership is enforced here: a non-member gets the unknown-channel 404.
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}

	// v0 has no order: it ignores order and before, as it always has.
	var page store.MessagePage
	var err error
	if version == apiV0 {
		page.After, err = nonNegativeQueryInt64(r, "after", 0)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	} else if page, ok = messageOrder(w, r); !ok {
		return
	}
	parsedLimit, err := nonNegativeQueryInt64(r, "limit", defaultMessageLimit)
	if err != nil || parsedLimit == 0 || parsedLimit > maxMessageLimit {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		return
	}
	limit := int(parsedLimit)

	// ?thread=<root id> narrows a v2 listing to one thread (issue #25). v0 and v1
	// have no threads and ignore it.
	thread := int64(0)
	if version == apiV2 && r.URL.Query().Has("thread") {
		thread, err = strconv.ParseInt(r.URL.Query().Get("thread"), 10, 64)
		if err != nil || thread <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "thread must be a positive integer")
			return
		}
		// A thread is the root and then its replies, in order.
		if page.NewestFirst {
			writeError(w, http.StatusBadRequest, "invalid_request", "thread cannot be read with order=desc")
			return
		}
	}

	// Fetch one extra row so the response only advertises a next page when one
	// is currently available.
	var stored []store.Message
	if thread > 0 {
		stored, err = s.store.ListThreadMessages(ctx, channel.ID, readerFor(r, version), thread, page.After, limit+1)
	} else {
		stored, err = s.store.ListVisibleMessagesPage(ctx, channel.ID, readerFor(r, version), page, limit+1)
	}
	if errors.Is(err, store.ErrThreadNotFound) {
		writeThreadNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	nextAfter := int64(0)
	if len(stored) > limit {
		stored = stored[:limit]
		// next_after is for the ascending read; a newest-first client pages
		// back by the smallest id it holds.
		if !page.NewestFirst {
			nextAfter = stored[len(stored)-1].ID
		}
	}
	switch version {
	case apiV2:
		messages := make([]schema.MessageV2, len(stored))
		for i, message := range stored {
			messages[i] = messageV2FromStore(message)
		}
		writeJSON(w, http.StatusOK, schema.ListMessagesResponseV2{Messages: messages, NextAfter: nextAfter})
		return
	case apiV1:
		messages := make([]schema.MessageV1, len(stored))
		for i, message := range stored {
			messages[i] = messageV1FromStore(message)
		}
		writeJSON(w, http.StatusOK, schema.ListMessagesResponseV1{Messages: messages, NextAfter: nextAfter})
		return
	}
	messages := make([]schema.MessageV0, len(stored))
	for i, message := range stored {
		messages[i] = messageV0FromStore(message)
	}
	writeJSON(w, http.StatusOK, schema.ListMessagesResponse{
		Messages:  messages,
		NextAfter: nextAfter,
	})
}

// messageOrder reads order, after and before for a v1 or v2 message list into
// the store's page, as runOrder does for runs: order is asc (the default) or
// desc; after goes only with asc and before only with desc, so any other mix,
// both cursors included, is a 400, and desc with no before is the newest page.
// An empty value is no value. It writes the 400 and returns false when the
// parameters are bad.
func messageOrder(w http.ResponseWriter, r *http.Request) (store.MessagePage, bool) {
	query := r.URL.Query()
	var page store.MessagePage
	switch query.Get("order") {
	case "", "asc":
	case "desc":
		page.NewestFirst = true
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "order must be asc or desc")
		return page, false
	}
	if page.NewestFirst && query.Get("after") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "after goes with order=asc; page back with before")
		return page, false
	}
	if !page.NewestFirst && query.Get("before") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "before goes with order=desc")
		return page, false
	}
	var err error
	if page.NewestFirst {
		page.Before, err = nonNegativeQueryInt64(r, "before", 0)
	} else {
		page.After, err = nonNegativeQueryInt64(r, "after", 0)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return page, false
	}
	return page, true
}

// maxSearchQueryBytes bounds the q of a message search.
const maxSearchQueryBytes = 256

// handleSearchMessagesV2 serves GET /v2/channels/{channel}/messages/search
// (issue #22): the messages of one channel that contain every word of q, that
// the caller could list, newest first. v2 only, because only v2 can carry a
// scoped message.
//
// The caller is authorised by channelForCaller exactly as for listing, so a
// non-member gets the unknown-channel answer, and the reader is readerFor's,
// so the search sees what the list sees. The store applies visibility in the
// same statement as the match; nothing here counts or filters after it, so the
// page and next_before are the same with or without a message the caller
// cannot see.
func (s *Server) handleSearchMessagesV2(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	words := strings.Fields(q)
	if len(words) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "q is required")
		return
	}
	if len(q) > maxSearchQueryBytes {
		writeError(w, http.StatusBadRequest, "invalid_request", "q must be at most 256 bytes")
		return
	}
	before, err := nonNegativeQueryInt64(r, "before", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	parsedLimit, err := nonNegativeQueryInt64(r, "limit", defaultMessageLimit)
	if err != nil || parsedLimit == 0 || parsedLimit > maxMessageLimit {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		return
	}
	limit := int(parsedLimit)

	// One extra row, as for listing, so next_before is advertised only when a
	// further match is currently available to this caller.
	stored, err := s.store.SearchVisibleMessages(ctx, channel.ID, readerFor(r, apiV2), words, before, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "messages: search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	nextBefore := int64(0)
	if len(stored) > limit {
		stored = stored[:limit]
		nextBefore = stored[len(stored)-1].ID
	}
	messages := make([]schema.MessageV2, len(stored))
	for i, message := range stored {
		messages[i] = messageV2FromStore(message)
	}
	writeJSON(w, http.StatusOK, schema.SearchMessagesResponseV2{Messages: messages, NextBefore: nextBefore})
}

func messageV0FromStore(message store.Message) schema.MessageV0 {
	return schema.MessageV0{
		ID:        message.ID,
		ChannelID: message.ChannelID,
		AuthorID:  message.AuthorID,
		Body:      message.Body,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: message.CreatedAt.UTC(),
	}
}

func messageV1FromStore(message store.Message) schema.MessageV1 {
	return schema.MessageV1{Schema: schema.MessageSchemaV1, ID: message.ID, ChannelID: message.ChannelID,
		AuthorID: message.AuthorID, Body: message.Body, Payload: message.Payload,
		CreatedAt: schema.NewTimestamp(message.CreatedAt)}
}

// messageV2FromStore is the v2 wire form. The audience of a net message is
// only the net id; a whisper's is its normalized principal list. The resolved
// recipients of a net message are never on the wire.
func messageV2FromStore(message store.Message) schema.MessageV2 {
	m := schema.MessageV2FromV1(messageV1FromStore(message))
	if message.Audience != nil {
		a := *message.Audience
		a.PrincipalIDs = append([]int64(nil), a.PrincipalIDs...)
		m.Audience = &a
	}
	m.ThreadRootID = message.ThreadRootID
	if message.GuestAuthor != nil {
		a := *message.GuestAuthor
		m.GuestAuthor = &a
	}
	return m
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("server: request body must contain one JSON object")
	}
	return nil
}

func nonNegativeQueryInt64(r *http.Request, name string, defaultValue int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return value, nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, schema.Error{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("server: encode response failed", "error", err)
	}
}

// handlePostMessageV2 serves POST /v2/channels/{channel}/messages. Without an
// audience it is the v1 post with the v2 envelope in the response; with one it
// is a scoped post (postScopedMessage).
func (s *Server) handlePostMessageV2(w http.ResponseWriter, r *http.Request) {
	var req schema.PostMessageRequestV2
	if !s.decodePostRequest(w, r, &req) {
		return
	}
	if req.ThreadRootID != 0 && req.Audience != nil {
		// A reply has its root's audience and cannot choose its own, so this is
		// a malformed request whatever the root is; nothing is looked up.
		writeError(w, http.StatusBadRequest, errThreadAudience.Code, errThreadAudience.Message)
		return
	}
	if req.Audience != nil {
		// A scoped post needs a verified author, and with authentication off
		// the author is whatever the body says. Refuse before anything else
		// so nothing about the channel or the audience is looked at.
		caller, ok := callerFrom(r.Context())
		if !ok {
			writeError(w, http.StatusBadRequest, "audience_requires_auth", "a message with an audience requires authentication")
			return
		}
		// A guest does not start private conversations (issue #706, design
		// §4.3): a whisper is refused before the channel or the audience is
		// looked at. A net it was put on by an operator is not one it creates.
		if caller.IsGuest() && req.Audience.Kind == schema.AudienceKindPrincipals {
			s.denyForbidden(w, r, caller, schema.ErrorCodeGuestNotAllowed, "a guest cannot whisper")
			return
		}
		if err := req.Audience.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_audience", err.Error())
			return
		}
	}
	if !s.bindAuthor(w, r, &req.AuthorID) {
		return
	}
	if req.Audience != nil && req.Audience.Kind == schema.AudienceKindPrincipals && len(req.Audience.Normalize(req.AuthorID).PrincipalIDs) < 2 {
		writeError(w, http.StatusBadRequest, "invalid_audience", "a whisper needs at least one recipient other than the author")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ThreadRootID > 0 {
		s.postReply(w, r, req)
		return
	}
	if req.Audience == nil {
		s.postMessage(w, r, req.AuthorID, req.Body, req.Payload, apiV2)
		return
	}
	s.postScopedMessage(w, r, req)
}

// postScopedMessage stores a message with an audience and delivers it to its
// recipients only. The caller is verified (handlePostMessageV2 checked), and
// the author is bound to it.
//
// The order of the checks is part of the security design. Channel membership
// comes first, answered with the unknown-channel 404. An agent's manifest
// comes next and can only be answered from the agent's own manifest. Only then
// is the net resolved, together with the caller's seat on it, in one store
// transaction: an unknown net, an archived net and a net the caller is not on
// are all net_not_found, so a net's existence is learnt only by those on it,
// and only a monitor of the net is told it may not transmit.
//
// Nothing is delivered through the v0/v1 broadcasts or the Broadcaster seam:
// those cannot carry an audience.
func (s *Server) postScopedMessage(w http.ResponseWriter, r *http.Request, req schema.PostMessageRequestV2) {
	ctx := r.Context()
	caller, _ := callerFrom(ctx)
	channel, ok := s.channelForScopedPost(w, r)
	if !ok {
		return
	}
	audience := req.Audience.Normalize(req.AuthorID)
	allowed, err := s.authorizeScopedPost(ctx, caller, r.Pattern, channel.ID, audience)
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		writeRateLimited(w, limited)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: authorize scoped post failed", "error", err)
		writeInternalError(w)
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	// Only an agent's refusals are audited as access_denied; for a human
	// the subject is empty and nothing is written.
	agentSubject := agentSubjectFor(r, caller)
	message, recipients, serr := s.storeScopedPost(ctx, store.ScopedPost{
		ChannelID: channel.ID, AuthorID: req.AuthorID, Body: req.Body, Payload: req.Payload, Audience: audience,
	}, agentSubject)
	if serr != nil {
		writeError(w, scopedPostStatus(serr.Code), serr.Code, serr.Message)
		return
	}
	// Persist-then-broadcast, to the recipients' v2 sockets only.
	s.hub.BroadcastMessageV2(ctx, message, recipients)
	writeJSON(w, http.StatusCreated, schema.PostMessageResponseV2{Message: message})
}

// storeScopedPost stores a message with an audience and turns each refusal of
// the store into its wire error. The REST post and the MCP post_message tool
// both end here, so the two cannot answer differently: an unknown net, an
// archived net and a net the author is not on are all net_not_found; only a
// monitor of the net learns it may not transmit.
//
// agentSubject is the audit subject ("mcp:<tool>" or the route pattern) when
// the author is an agent, and empty otherwise. An agent's refusal by the net's
// roster is then recorded as access_denied, once, here, so the two front ends
// record it alike (issue #149). The answer to the caller does not change.
func (s *Server) storeScopedPost(ctx context.Context, post store.ScopedPost, agentSubject string) (schema.MessageV2, []int64, *schema.Error) {
	stored, recipients, err := s.store.InsertScopedMessage(ctx, post)
	if serr := s.scopedPostRefusal(ctx, err, post.ChannelID, post.AuthorID, agentSubject); serr != nil {
		return schema.MessageV2{}, nil, serr
	}
	s.countScopedPost(agentSubject)
	return messageV2FromStore(stored), recipients, nil
}

// countScopedPost counts a stored scoped message for the metrics by its
// author's kind (metrics.go). A scoped post always has an authenticated
// author, and agentSubject is set exactly when that author is an agent.
func (s *Server) countScopedPost(agentSubject string) {
	if agentSubject != "" {
		s.metrics.messagePosted(store.PrincipalAgent)
		return
	}
	s.metrics.messagePosted(store.PrincipalHuman)
}

// scopedPostRefusal turns an error of a scoped insert (a post or a reply) into
// its wire error, or nil for none. See storeScopedPost.
func (s *Server) scopedPostRefusal(ctx context.Context, err error, channelID, authorID int64, agentSubject string) *schema.Error {
	denied := func(reason string) {
		if agentSubject != "" {
			s.auditAgentDenial(ctx, authorID, agentSubject, schema.CapabilityMessagesPost, channelID, reason)
		}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNetNotFound):
		denied(denyNetNotOn)
		return &schema.Error{Code: "net_not_found", Message: "net not found"}
	case errors.Is(err, store.ErrNetMonitorOnly):
		denied(denyNetMonitorOnly)
		return &schema.Error{Code: errForbidden.Code, Message: "a monitor of a net may listen but not transmit"}
	case errors.Is(err, store.ErrInvalidAudience):
		return &schema.Error{Code: "invalid_audience", Message: "every recipient must be a member of the channel, and a whisper needs a recipient other than the author"}
	case errors.Is(err, store.ErrFileAttachment):
		return errInvalidFileAttachment
	case errors.Is(err, store.ErrThreadNotFound):
		// The root became unusable between the check and the insert.
		return errThreadNotFound
	case errors.Is(err, store.ErrNotChannelMember):
		// Removed from the channel after the caller's membership check.
		return errChannelNotFound
	default:
		slog.ErrorContext(ctx, "messages: insert scoped failed", "error", err)
		return errInternal
	}
}

// agentSubjectFor is the audit subject ("<route pattern>") when the author is
// an agent, and empty otherwise. Only an agent's refusals are audited as
// access_denied; for a human the subject is empty and nothing is written.
func agentSubjectFor(r *http.Request, caller store.Principal) string {
	if caller.Kind != store.PrincipalAgent {
		return ""
	}
	// Never empty for an agent: empty means "not an agent".
	if r.Pattern == "" {
		return "<unmatched>"
	}
	return r.Pattern
}

var (
	// errThreadNotFound is the one answer to every reply or thread read whose
	// root the caller cannot use: unknown, in another channel, itself a reply,
	// or not visible to them. Message ids are sequential, so any difference
	// between those would let a caller probe for scoped messages.
	errThreadNotFound = &schema.Error{Code: "thread_not_found", Message: "thread not found"}
	// errThreadAudience refuses a post that names both a thread and an audience.
	errThreadAudience = &schema.Error{Code: "thread_audience_conflict", Message: "a reply takes its root's audience; thread_root_id and audience cannot be combined"}
)

func writeThreadNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, errThreadNotFound.Code, errThreadNotFound.Message)
}

// postReply stores a reply to a message and delivers it to exactly the
// principals who can see that message (issue #25). The order of the checks is
// part of the security design, as for postScopedMessage: channel membership
// first (the unknown-channel 404); then whether the caller can see the root,
// where unknown, other-channel, a reply and not-visible all give errThreadNotFound
// and nothing else is looked at first; only then the agent's manifest, decided
// from the root's audience (a reply to a net or a whisper is a post to that net
// or whisper, which has its own grants). The store decides visibility of the
// root again inside the insert transaction.
func (s *Server) postReply(w http.ResponseWriter, r *http.Request, req schema.PostMessageRequestV2) {
	ctx := r.Context()
	caller, _ := callerFrom(ctx)
	channel, ok := s.channelForScopedPost(w, r)
	if !ok {
		return
	}
	root, err := s.store.ReplyRootMessage(ctx, channel.ID, readerFor(r, apiV2), req.ThreadRootID)
	if errors.Is(err, store.ErrThreadNotFound) {
		writeThreadNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: find thread root failed", "error", err)
		writeInternalError(w)
		return
	}

	if root.Audience == nil {
		if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesPost, channel.ID, schema.ChannelPermissionPost) {
			return
		}
	} else {
		allowed, err := s.authorizeScopedPost(ctx, caller, r.Pattern, channel.ID, root.Audience.Normalize(req.AuthorID))
		var limited *rateLimitedError
		if errors.As(err, &limited) {
			writeRateLimited(w, limited)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "messages: authorize reply failed", "error", err)
			writeInternalError(w)
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
			return
		}
	}

	// As for any post: attaching a file takes the uploader's own credential.
	if req.Payload != nil && req.Payload.Schema == schema.FileAttachmentV1Name {
		if _, verified := callerFrom(ctx); !verified {
			writeError(w, http.StatusBadRequest, errFilesNeedAuth.Code, "attaching a file requires the uploader's own credential")
			return
		}
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	author, err := s.store.PrincipalByID(ctx, req.AuthorID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "author_not_found", "author not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "messages: find author failed", "error", err)
		writeInternalError(w)
		return
	}

	stored, recipients, err := s.store.InsertReplyMessage(ctx, store.ReplyPost{
		ChannelID: channel.ID, AuthorID: req.AuthorID, RootID: req.ThreadRootID, Body: req.Body, Payload: req.Payload,
	}, readerFor(r, apiV2))
	if serr := s.scopedPostRefusal(ctx, err, channel.ID, req.AuthorID, agentSubjectFor(r, caller)); serr != nil {
		writeError(w, scopedPostStatus(serr.Code), serr.Code, serr.Message)
		return
	}
	s.metrics.messagePosted(author.Kind)
	message := messageV2FromStore(stored)
	// Persist-then-broadcast. A scoped reply goes to its recipients' v2 sockets
	// only. A reply to a channel-wide root is an ordinary message to a v0 or v1
	// socket and carries thread_root_id on v2 (BroadcastReply, not
	// BroadcastMessageV1, which would hand v2 sockets an envelope without it).
	if stored.Audience != nil {
		s.hub.BroadcastMessageV2(ctx, message, recipients)
	} else {
		messageV0 := messageV0FromStore(stored)
		s.hub.BroadcastMessage(ctx, messageV0)
		s.hub.BroadcastReply(ctx, messageV1FromStore(stored), message)
		s.broadcaster.BroadcastMessage(ctx, messageV0)
	}
	writeJSON(w, http.StatusCreated, schema.PostMessageResponseV2{Message: message})
}

// scopedPostStatus is the HTTP status for a storeScopedPost refusal.
func scopedPostStatus(code string) int {
	switch code {
	case "net_not_found", errChannelNotFound.Code, errThreadNotFound.Code:
		return http.StatusNotFound
	case errForbidden.Code:
		return http.StatusForbidden
	case "invalid_audience", errInvalidFileAttachment.Code:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// channelForScopedPost is channelForCaller for a scoped post: the same
// unknown-channel and non-member answers, but the agent's manifest is decided
// by authorizeScopedPost, because posting to a net or a whisper needs its own
// grant and the channel permission "post" (channel-wide) grants none of them.
func (s *Server) channelForScopedPost(w http.ResponseWriter, r *http.Request) (store.Channel, bool) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: find channel failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, false
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "messages: check membership failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, false
	}
	if !member {
		s.auditAgentNonMember(r, schema.CapabilityMessagesPost, channel.ID)
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	return channel, true
}
