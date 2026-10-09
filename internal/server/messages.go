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
	maxMessageBodyBytes = 1 << 20
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

	if authorID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "author_id must be positive")
		return
	}
	if strings.TrimSpace(body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	if _, err := s.store.PrincipalByID(ctx, authorID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "author_not_found", "author not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "messages: find author failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	stored, err := s.store.InsertMessageV1(ctx, channel.ID, authorID, body, payload)
	if err != nil {
		slog.ErrorContext(ctx, "messages: insert failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	messageV1 := messageV1FromStore(stored)
	// Persist-then-broadcast: the message is durable before anyone hears
	// about it, so a crash here loses delivery, never data.
	s.hub.BroadcastMessageV1(ctx, messageV1)
	messageV0 := messageV0FromStore(stored)
	s.hub.BroadcastMessage(ctx, messageV0)
	s.broadcaster.BroadcastMessage(ctx, messageV0)
	switch version {
	case apiV2:
		writeJSON(w, http.StatusCreated, schema.PostMessageResponseV2{Message: schema.MessageV2FromV1(messageV1)})
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

	after, err := nonNegativeQueryInt64(r, "after", 0)
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

	// Fetch one extra row so the response only advertises a next page when one
	// is currently available.
	stored, err := s.store.ListVisibleMessages(ctx, channel.ID, readerFor(r, version), after, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "messages: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	nextAfter := int64(0)
	if len(stored) > limit {
		stored = stored[:limit]
		nextAfter = stored[len(stored)-1].ID
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
	if req.Audience != nil {
		// A scoped post needs a verified author, and with authentication off
		// the author is whatever the body says. Refuse before anything else
		// so nothing about the channel or the audience is looked at.
		if _, ok := callerFrom(r.Context()); !ok {
			writeError(w, http.StatusBadRequest, "audience_requires_auth", "a message with an audience requires authentication")
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
	agentSubject := ""
	if caller.Kind == store.PrincipalAgent {
		// Never empty for an agent: empty means "not an agent" below.
		if agentSubject = r.Pattern; agentSubject == "" {
			agentSubject = "<unmatched>"
		}
	}
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
	denied := func(reason string) {
		if agentSubject != "" {
			s.auditAgentDenial(ctx, post.AuthorID, agentSubject, schema.CapabilityMessagesPost, post.ChannelID, reason)
		}
	}
	switch {
	case errors.Is(err, store.ErrNetNotFound):
		denied(denyNetNotOn)
		return schema.MessageV2{}, nil, &schema.Error{Code: "net_not_found", Message: "net not found"}
	case errors.Is(err, store.ErrNetMonitorOnly):
		denied(denyNetMonitorOnly)
		return schema.MessageV2{}, nil, &schema.Error{Code: errForbidden.Code, Message: "a monitor of a net may listen but not transmit"}
	case errors.Is(err, store.ErrInvalidAudience):
		return schema.MessageV2{}, nil, &schema.Error{Code: "invalid_audience", Message: "every recipient must be a member of the channel, and a whisper needs a recipient other than the author"}
	case errors.Is(err, store.ErrNotChannelMember):
		// Removed from the channel after the caller's membership check.
		return schema.MessageV2{}, nil, errChannelNotFound
	case err != nil:
		slog.ErrorContext(ctx, "messages: insert scoped failed", "error", err)
		return schema.MessageV2{}, nil, errInternal
	}
	return messageV2FromStore(stored), recipients, nil
}

// scopedPostStatus is the HTTP status for a storeScopedPost refusal.
func scopedPostStatus(code string) int {
	switch code {
	case "net_not_found", errChannelNotFound.Code:
		return http.StatusNotFound
	case errForbidden.Code:
		return http.StatusForbidden
	case "invalid_audience":
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
