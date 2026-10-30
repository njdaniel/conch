package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Message editing (issue #845, docs/design/message-editing.md). The author of
// a message, a person, changes its text within an hour of posting it, at most
// MaxMessageEdits times; every version is kept and only the author reads
// them. There is no MCP tool: agents cannot edit (§6.4).
//
// The order of the edit's checks is part of the security design (§4.2), and
// nothing before step 7 reads the message:
//
//  1. authentication off: 400 message_edit_requires_auth;
//  2. the caller is not a person: 403 human_required, before any lookup, so
//     the answer is the same for every channel and message, real or not;
//  3. the channel gate: unknown channel or not a member is the one
//     404 channel_not_found;
//  4. a token of the caller's edit bucket, whatever follows (only a 429
//     spends none, and nothing refunds one), so probing ids costs what
//     editing costs;
//  5. the message id is a positive integer, else 400 invalid_request;
//  6. the body, with the post route's bounds and answers;
//  7. the store, in one transaction (store.EditOwnMessage): membership again,
//     the author's own visible message (404 message_not_found for every
//     other case alike), editable (409 message_not_editable), unchanged text
//     (200, nothing written), the window (409 message_edit_window_closed),
//     the limit (409 message_edit_limit_reached), then the write and its
//     message_edited audit row.
//
// After a commit that changed the text, the edited MessageV2 goes to the
// /v2/ws subscribers the post reached (hub.BroadcastMessageV2 with the stored
// recipients; each socket's history gate still applies), and the events
// sockets of the channel's human members are told to send a
// conch.message_edited.v1 frame, each read with its own reader. An edit
// notifies no one, moves no read mark, reaches no v0 or v1 socket and never
// the Broadcaster seam, whose consumers would take it for a new post (§7).
//
// The revision read takes steps 1 to 3 and 5, then the same author-and-
// visibility select, and spends no token (§5).

var (
	errMessageEditRequiresAuth = &schema.Error{Code: schema.ErrorCodeMessageEditRequiresAuth, Message: "editing a message requires authentication"}
	errMessageNotEditable      = &schema.Error{Code: schema.ErrorCodeMessageNotEditable, Message: "this message cannot be edited"}
	errMessageEditWindowClosed = &schema.Error{Code: schema.ErrorCodeMessageEditWindowClosed, Message: "this message is more than an hour old and can no longer be edited"}
	errMessageEditLimitReached = &schema.Error{Code: schema.ErrorCodeMessageEditLimitReached, Message: "this message has been edited the most times allowed"}
)

// Edit limit (§9): ten a minute per person with a burst of ten, a token every
// six seconds.
const (
	messageEditBurst  = 10
	messageEditWindow = time.Minute
)

func defaultMessageEditLimit() inviteLimit {
	return inviteLimit{"message_edits_per_principal", messageEditBurst, messageEditWindow}
}

// messageEditLimiter is the per-person edit bucket. In memory only, like every
// rate limit here: a restart refills it.
type messageEditLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newMessageEditLimiter(l inviteLimit) *messageEditLimiter { return &messageEditLimiter{limit: l} }

// take spends one edit of principalID.
func (l *messageEditLimiter) take(principalID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Principals are made by operators and invites, both limited, so one
	// shared overflow bucket is enough.
	return l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.limit, now)
}

// setMessageEditLimit replaces the limit and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setMessageEditLimit(l inviteLimit) { s.messageEditLimits = newMessageEditLimiter(l) }

// writeMessageEditError answers a refusal with its status. A rate limit also
// carries Retry-After.
func writeMessageEditError(w http.ResponseWriter, serr *schema.Error) {
	status := http.StatusInternalServerError
	switch serr.Code {
	case errChannelNotFound.Code, errMessageNotFound.Code:
		status = http.StatusNotFound
	case schema.ErrorCodeHumanRequired:
		status = http.StatusForbidden
	case errMessageNotEditable.Code, errMessageEditWindowClosed.Code, errMessageEditLimitReached.Code:
		status = http.StatusConflict
	case errMessageEditRequiresAuth.Code, "invalid_request":
		status = http.StatusBadRequest
	case schema.ErrorCodeRateLimited:
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(max(serr.RetryAfterSeconds, 1)))
	}
	writeJSON(w, status, serr)
}

// messageEditStoreError turns an error of the store's edit functions into its
// wire error.
func messageEditStoreError(r *http.Request, err error) *schema.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotChannelMember):
		// Removed from the channel (or disabled) after the gate: the answer
		// a stranger gets.
		return errChannelNotFound
	case errors.Is(err, store.ErrMessageNotFound):
		return errMessageNotFound
	case errors.Is(err, store.ErrMessageNotEditable):
		return errMessageNotEditable
	case errors.Is(err, store.ErrMessageEditWindowClosed):
		return errMessageEditWindowClosed
	case errors.Is(err, store.ErrMessageEditLimitReached):
		return errMessageEditLimitReached
	default:
		slog.ErrorContext(r.Context(), "message edit: store failed", "error", err)
		return errInternal
	}
}

// messageEditCaller is steps 1 and 2, shared by both routes: a verified
// person, decided before anything is looked up.
func messageEditCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeMessageEditError(w, errMessageEditRequiresAuth)
		return store.Principal{}, false
	}
	if caller.Kind != store.PrincipalHuman {
		writeMessageEditError(w, &schema.Error{Code: schema.ErrorCodeHumanRequired, Message: "messages are edited by people only"})
		return store.Principal{}, false
	}
	return caller, true
}

// parseEditMessageID is step 5: an id that is not a positive integer.
func parseEditMessageID(raw string) (int64, *schema.Error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, &schema.Error{Code: "invalid_request", Message: "message id must be a positive integer"}
	}
	return id, nil
}

// handleEditMessage serves PUT /v2/channels/{channel}/messages/{message_id}/body.
func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := messageEditCaller(w, r)
	if !ok {
		return
	}
	// A person's gate is membership; the capability and permission are an
	// agent's, and no agent gets here.
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesPost, schema.ChannelPermissionPost)
	if !ok {
		return
	}
	if ok, retry := s.messageEditLimits.take(caller.ID, s.now()); !ok {
		writeMessageEditError(w, (&rateLimitedError{retryAfter: retry}).schemaError())
		return
	}
	messageID, serr := parseEditMessageID(r.PathValue("message_id"))
	if serr != nil {
		writeMessageEditError(w, serr)
		return
	}
	var req schema.EditMessageRequestV1
	if !s.decodePostRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	edit, err := s.store.EditOwnMessage(ctx, channel.ID, readerFor(r, apiV2), messageID, req.Body, s.now)
	if serr := messageEditStoreError(r, err); serr != nil {
		writeMessageEditError(w, serr)
		return
	}
	message := messageV2FromStore(edit.Message)
	if edit.Changed {
		// Persist, then deliver: the frame is what the post's recipients
		// already hold, with the new text. v2 only, and never the
		// Broadcaster.
		s.hub.BroadcastMessageV2(ctx, message, edit.Recipients)
		s.events.notifyMessageEdited(channel.ID, messageID)
	}
	writeJSON(w, http.StatusOK, schema.EditMessageResponseV1{Message: message})
}

// handleListMessageRevisions serves GET
// /v2/channels/{channel}/messages/{message_id}/revisions: the author's own
// earlier versions.
func (s *Server) handleListMessageRevisions(w http.ResponseWriter, r *http.Request) {
	if _, ok := messageEditCaller(w, r); !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	messageID, serr := parseEditMessageID(r.PathValue("message_id"))
	if serr != nil {
		writeMessageEditError(w, serr)
		return
	}
	revs, err := s.store.OwnMessageRevisions(r.Context(), channel.ID, readerFor(r, apiV2), messageID)
	if serr := messageEditStoreError(r, err); serr != nil {
		writeMessageEditError(w, serr)
		return
	}
	doc := schema.ListMessageRevisionsResponseV1{ChannelID: channel.ID, MessageID: messageID, Revisions: make([]schema.MessageRevisionV1, len(revs))}
	for i, rev := range revs {
		doc.Revisions[i] = schema.MessageRevisionV1{Revision: rev.Revision, Body: rev.Body, CreatedAt: schema.NewTimestamp(rev.CreatedAt)}
	}
	if err := doc.Validate(); err != nil {
		// Fail closed: a list the schema refuses is a server bug, not an
		// answer.
		slog.ErrorContext(r.Context(), "message edit: built an invalid revision list", "channel", channel.ID, "message", messageID, "error", err)
		writeMessageEditError(w, errInternal)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
