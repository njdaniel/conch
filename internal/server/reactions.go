package server

import (
	"context"
	"encoding/json"
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

// Message reactions (issue #765, docs/design/message-reactions.md). A reaction
// is a durable record (message, principal, emoji); counts are derived from the
// records on every read. The REST routes below and the four MCP tools
// (mcp.go) are two fronts of the same functions here, so they cannot answer
// differently (API parity, ADR-001 D6).
//
// The order of the checks is part of the security design (§6.1) and the same
// on both fronts:
//
//  1. May the caller use this channel? Unknown channel and non-member are the
//     one channel_not_found. For an agent's write the gate is membership only
//     (as a scoped post's is): its manifest is decided in step 4 from the
//     message's audience, and its budget is spent once, there. For an agent's
//     read the gate is the read permission, and spends there.
//  2. A human's write takes a token of the reaction bucket, whether or not
//     the rest of the request is well formed. Nothing refunds it, so probing
//     message ids or keys costs what reacting costs. Only a 429 spends none.
//  3. The key is canonicalized (schema.CanonicalReactionKey), the same for
//     REST and MCP, before the store.
//  4. An agent's write: the message is probed (message_not_found for unknown,
//     other-channel and invisible alike), then the agent needs the permission
//     it would need to reply in that audience: post channel-wide, post_net on
//     a net, whisper in a whisper (forbidden, audited).
//  5. The store: one transaction that re-checks membership, probes the
//     message with the one visibility condition and the explicit
//     member-and-enabled clause, applies the net seat rule, and writes.
//
// A reaction does not post a message, notify anyone, touch a read mark, reach
// the Broadcaster, or start an agent. The one thing a committed change does
// is tell the events sockets of the channel to re-read that message's summary
// (events_ws.go), each with its own reader.

var (
	// errMessageNotFound is the one answer to a message that is unknown, in
	// another channel or not visible to the caller. It is a constant: any
	// difference between those would let a caller probe for messages scoped
	// away from them.
	errMessageNotFound      = &schema.Error{Code: schema.ErrorCodeMessageNotFound, Message: "message not found"}
	errInvalidReaction      = &schema.Error{Code: schema.ErrorCodeInvalidReaction, Message: "emoji must be one emoji of the supported Unicode Emoji set"}
	errReactionNotPermitted = &schema.Error{Code: schema.ErrorCodeReactionNotPermitted, Message: "you may not react to this message"}
	errTooManyReactions     = &schema.Error{Code: schema.ErrorCodeTooManyReactions, Message: "this message or you have reached the limit of reactions"}
	errReactionsRequireAuth = &schema.Error{Code: schema.ErrorCodeReactionsRequireAuth, Message: "reactions require authentication"}
)

// Reaction write limit (§7.4): 60 a minute per person with a burst of 20, a
// token a second. Agents are limited by their manifest's messages.post budget
// instead.
const (
	reactionWriteBurst  = 20
	reactionWriteWindow = 20 * time.Second
)

func defaultReactionLimit() inviteLimit {
	return inviteLimit{"reaction_writes_per_principal", reactionWriteBurst, reactionWriteWindow}
}

// reactionLimiter is the per-person reaction write bucket. In memory only,
// like every rate limit here: a restart refills it.
type reactionLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newReactionLimiter(l inviteLimit) *reactionLimiter { return &reactionLimiter{limit: l} }

// take spends one write of principalID.
func (l *reactionLimiter) take(principalID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Principals are made by operators and invites, both limited, so one
	// shared overflow bucket is enough.
	return l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.limit, now)
}

// setReactionLimit replaces the limit and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setReactionLimit(l inviteLimit) { s.reactionLimits = newReactionLimiter(l) }

// reactionCall is who is acting and how, for the functions both fronts share.
type reactionCall struct {
	principal store.Principal
	// subject is the audit subject of a refusal: the route pattern, or
	// "mcp:<tool>".
	subject string
	// reader is the caller as the store's visibility condition knows it:
	// readerFor(r, apiV2) for REST, the scope's reader() for MCP.
	reader store.Reader
}

func (c reactionCall) isAgent() bool { return c.principal.Kind == store.PrincipalAgent }

// reactionClock is the store's clock for as_of and created_at.
func (s *Server) reactionClock() time.Time { return s.now() }

// reactionStoreError turns an error of the store's reaction functions into
// its wire error.
func reactionStoreError(ctx context.Context, err error) *schema.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotChannelMember):
		// Removed from the channel (or disabled) after the gate: the answer a
		// stranger gets.
		return errChannelNotFound
	case errors.Is(err, store.ErrMessageNotFound):
		return errMessageNotFound
	case errors.Is(err, store.ErrReactionNotPermitted):
		return errReactionNotPermitted
	case errors.Is(err, store.ErrTooManyReactions):
		return errTooManyReactions
	case errors.Is(err, schema.ErrInvalidReaction):
		return errInvalidReaction
	default:
		slog.ErrorContext(ctx, "reactions: store failed", "error", err)
		return errInternal
	}
}

// reactionSummaryDoc is the wire form of a stored summary.
func reactionSummaryDoc(sum store.ReactionSummary) schema.ReactionSummaryV1 {
	counts := make([]schema.ReactionCountV1, len(sum.Reactions))
	for i, c := range sum.Reactions {
		counts[i] = schema.ReactionCountV1{Emoji: c.Emoji, Count: c.Count, Me: c.Me}
	}
	return schema.ReactionSummaryV1{
		Schema:    schema.ReactionSummarySchemaV1,
		ChannelID: sum.ChannelID,
		MessageID: sum.MessageID,
		Reactions: counts,
		AsOf:      schema.NewTimestamp(sum.AsOf),
	}
}

// parseReactionMessageID reads a message id. One that is not a positive
// integer names no message, so it is the not-found answer, as a ghost id is.
func parseReactionMessageID(raw string) (int64, *schema.Error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errMessageNotFound
	}
	return id, nil
}

// takeReactionToken is step 2: a person's write takes a token. An agent's
// budget is spent in the audience step.
func (s *Server) takeReactionToken(c reactionCall) *schema.Error {
	if c.isAgent() {
		return nil
	}
	if ok, retry := s.reactionLimits.take(c.principal.ID, s.now()); !ok {
		return (&rateLimitedError{retryAfter: retry}).schemaError()
	}
	return nil
}

// agentReactionGate is step 4 for an agent: the permission it would need to
// reply in the audience of the message it reacts to. It is the reply
// precedent (postReply): post channel-wide, then authorizeScopedPost for a net
// or a whisper, which decides post_net or whisper and spends the budget. Each
// refusal is audited once, and the budget is spent once, last.
func (s *Server) agentReactionGate(ctx context.Context, c reactionCall, channelID int64, target store.ReactionTarget) *schema.Error {
	if target.Audience == nil {
		m, allowed, err := s.agentManifestCheck(ctx, c.principal.ID, c.subject, schema.CapabilityMessagesPost, channelID, schema.ChannelPermissionPost)
		if err != nil {
			slog.ErrorContext(ctx, "reactions: load manifest failed", "principal", c.principal.ID, "error", err)
			return errInternal
		}
		if !allowed {
			return errForbidden
		}
		if limited := s.spendAgentBudget(ctx, c.principal.ID, c.subject, m, schema.CapabilityMessagesPost); limited != nil {
			return limited.schemaError()
		}
		return nil
	}
	allowed, err := s.authorizeScopedPost(ctx, c.principal, c.subject, channelID, *target.Audience)
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		return limited.schemaError()
	}
	if err != nil {
		slog.ErrorContext(ctx, "reactions: authorize failed", "principal", c.principal.ID, "error", err)
		return errInternal
	}
	if !allowed {
		return errForbidden
	}
	return nil
}

// writeReaction sets (set true) or removes the caller's own reaction. The
// channel gate (step 1) has passed. It answers the message's summary after the
// write.
func (s *Server) writeReaction(ctx context.Context, c reactionCall, channel store.Channel, rawMessageID, rawKey string, set bool) (schema.ReactionSummaryV1, *schema.Error) {
	if serr := s.takeReactionToken(c); serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	key, err := schema.CanonicalReactionKey(rawKey)
	if err != nil {
		return schema.ReactionSummaryV1{}, errInvalidReaction
	}
	messageID, serr := parseReactionMessageID(rawMessageID)
	if serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	if c.isAgent() {
		target, err := s.store.ReactionTargetOf(ctx, channel.ID, c.reader, messageID)
		if err != nil {
			return schema.ReactionSummaryV1{}, reactionStoreError(ctx, err)
		}
		if serr := s.agentReactionGate(ctx, c, channel.ID, target); serr != nil {
			return schema.ReactionSummaryV1{}, serr
		}
	}
	var change store.ReactionChange
	if set {
		change, err = s.store.SetReaction(ctx, channel.ID, c.reader, messageID, key, s.reactionClock)
	} else {
		change, err = s.store.RemoveReaction(ctx, channel.ID, c.reader, messageID, key, s.reactionClock)
	}
	if serr := reactionStoreError(ctx, err); serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	if change.Changed {
		s.events.notifyReactions(channel.ID, messageID)
	}
	return reactionSummaryDoc(change.Summary), nil
}

// moderateReactions is a manager's or operator's removal (§11.5) of one
// reaction (rawKey given) or of every reaction of targetID (rawKey empty) on a
// message. The channel gate has passed. The target is any principal id.
func (s *Server) moderateReactions(ctx context.Context, c reactionCall, channel store.Channel, rawMessageID, rawKey, rawTarget string, all bool) (schema.ReactionSummaryV1, *schema.Error) {
	if serr := s.takeReactionToken(c); serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	key := ""
	if !all {
		var err error
		if key, err = schema.CanonicalReactionKey(rawKey); err != nil {
			return schema.ReactionSummaryV1{}, errInvalidReaction
		}
	}
	targetID, err := strconv.ParseInt(rawTarget, 10, 64)
	if err != nil || targetID <= 0 {
		return schema.ReactionSummaryV1{}, &schema.Error{Code: "invalid_request", Message: "principal id must be a positive integer"}
	}
	messageID, serr := parseReactionMessageID(rawMessageID)
	if serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	change, err := s.store.ModerateReactions(ctx, channel.ID, c.reader, messageID, targetID, key, s.reactionClock)
	if serr := reactionStoreError(ctx, err); serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	if change.Changed {
		s.events.notifyReactions(channel.ID, messageID)
	}
	return reactionSummaryDoc(change.Summary), nil
}

// readReaction is the single-message summary read. The channel gate has
// passed.
func (s *Server) readReaction(ctx context.Context, c reactionCall, channel store.Channel, rawMessageID string) (schema.ReactionSummaryV1, *schema.Error) {
	messageID, serr := parseReactionMessageID(rawMessageID)
	if serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	sum, err := s.store.ReactionSummaryOf(ctx, channel.ID, c.reader, messageID, s.reactionClock)
	if serr := reactionStoreError(ctx, err); serr != nil {
		return schema.ReactionSummaryV1{}, serr
	}
	return reactionSummaryDoc(sum), nil
}

// checkReactionBatch refuses a batch that names none, too many, a non-positive
// id or one twice.
func checkReactionBatch(ids []int64) *schema.Error {
	bad := func(msg string) *schema.Error { return &schema.Error{Code: "invalid_request", Message: msg} }
	if len(ids) == 0 {
		return bad("name at least one message id")
	}
	if len(ids) > schema.MaxReactionBatchMessages {
		return bad("name at most " + strconv.Itoa(schema.MaxReactionBatchMessages) + " message ids")
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return bad("message ids must be positive integers")
		}
		if _, dup := seen[id]; dup {
			return bad("message ids must be distinct")
		}
		seen[id] = struct{}{}
	}
	return nil
}

// readReactionBatch is the batch read: one summary for each named message the
// caller can see that has a reaction.
func (s *Server) readReactionBatch(ctx context.Context, c reactionCall, channel store.Channel, ids []int64) (schema.ReactionSummariesResponseV1, *schema.Error) {
	if serr := checkReactionBatch(ids); serr != nil {
		return schema.ReactionSummariesResponseV1{}, serr
	}
	sums, err := s.store.ReactionSummariesOf(ctx, channel.ID, c.reader, ids, s.reactionClock)
	if serr := reactionStoreError(ctx, err); serr != nil {
		return schema.ReactionSummariesResponseV1{}, serr
	}
	out := schema.ReactionSummariesResponseV1{Summaries: make([]schema.ReactionSummaryV1, len(sums))}
	for i, sum := range sums {
		out.Summaries[i] = reactionSummaryDoc(sum)
	}
	return out, nil
}

// readReactors is one page of the principals holding one emoji on a message.
func (s *Server) readReactors(ctx context.Context, c reactionCall, channel store.Channel, rawMessageID, rawKey string, after int64, limit int) (schema.ListReactorsResponseV1, *schema.Error) {
	key, err := schema.CanonicalReactionKey(rawKey)
	if err != nil {
		return schema.ListReactorsResponseV1{}, errInvalidReaction
	}
	if after < 0 {
		return schema.ListReactorsResponseV1{}, &schema.Error{Code: "invalid_request", Message: "after must be a non-negative integer"}
	}
	if limit < 1 || limit > schema.MaxReactorLimit {
		return schema.ListReactorsResponseV1{}, &schema.Error{Code: "invalid_request", Message: "limit must be between 1 and " + strconv.Itoa(schema.MaxReactorLimit)}
	}
	messageID, serr := parseReactionMessageID(rawMessageID)
	if serr != nil {
		return schema.ListReactorsResponseV1{}, serr
	}
	page, err := s.store.ListReactorsOf(ctx, channel.ID, c.reader, messageID, key, after, limit)
	if serr := reactionStoreError(ctx, err); serr != nil {
		return schema.ListReactorsResponseV1{}, serr
	}
	out := schema.ListReactorsResponseV1{Reactors: make([]schema.ReactionV1, len(page.Reactors)), NextAfter: page.NextAfter}
	for i, r := range page.Reactors {
		out.Reactors[i] = schema.ReactionV1{PrincipalID: r.PrincipalID, Emoji: r.Emoji, CreatedAt: schema.NewTimestamp(r.CreatedAt), GuestReactor: r.Guest}
	}
	return out, nil
}

// REST ------------------------------------------------------------------

// writeReactionError answers a refusal with its status. A rate limit also
// carries Retry-After.
func writeReactionError(w http.ResponseWriter, serr *schema.Error) {
	status := http.StatusInternalServerError
	switch serr.Code {
	case errChannelNotFound.Code, errMessageNotFound.Code:
		status = http.StatusNotFound
	case errForbidden.Code, errReactionNotPermitted.Code, schema.ErrorCodeGuestNotAllowed:
		status = http.StatusForbidden
	case errTooManyReactions.Code:
		status = http.StatusConflict
	case errInvalidReaction.Code, errReactionsRequireAuth.Code, "invalid_request":
		status = http.StatusBadRequest
	case schema.ErrorCodeRateLimited:
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(max(serr.RetryAfterSeconds, 1)))
	}
	writeJSON(w, status, serr)
}

// reactionRequest is the check every reaction route starts with: with
// authentication off there is no principal to hold a reaction, so every route
// refuses (reactions_require_auth), before it looks at anything else.
func (s *Server) reactionRequest(w http.ResponseWriter, r *http.Request) (reactionCall, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeReactionError(w, errReactionsRequireAuth)
		return reactionCall{}, false
	}
	return reactionCall{principal: caller, subject: agentSubjectOrPattern(r), reader: readerFor(r, apiV2)}, true
}

// agentSubjectOrPattern is the route pattern, the audit subject of a refusal.
func agentSubjectOrPattern(r *http.Request) string {
	if r.Pattern == "" {
		return "<unmatched>"
	}
	return r.Pattern
}

// handlePutReaction serves PUT /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}.
func (s *Server) handlePutReaction(w http.ResponseWriter, r *http.Request) {
	s.handleWriteReaction(w, r, true)
}

// handleDeleteReaction serves DELETE .../reactions/{emoji}: the caller's own.
func (s *Server) handleDeleteReaction(w http.ResponseWriter, r *http.Request) {
	s.handleWriteReaction(w, r, false)
}

func (s *Server) handleWriteReaction(w http.ResponseWriter, r *http.Request, set bool) {
	c, ok := s.reactionRequest(w, r)
	if !ok {
		return
	}
	// Membership only for an agent: its manifest is decided from the
	// message's audience, and its budget spent once, there.
	channel, ok := s.channelForScopedPost(w, r)
	if !ok {
		return
	}
	doc, serr := s.writeReaction(r.Context(), c, channel, r.PathValue("message_id"), r.PathValue("emoji"), set)
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleGetReactions serves GET .../messages/{message_id}/reactions.
func (s *Server) handleGetReactions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.reactionRequest(w, r)
	if !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	doc, serr := s.readReaction(r.Context(), c, channel, r.PathValue("message_id"))
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleListReactors serves GET .../reactions/{emoji}?after=&limit=.
func (s *Server) handleListReactors(w http.ResponseWriter, r *http.Request) {
	c, ok := s.reactionRequest(w, r)
	if !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	after, err := nonNegativeQueryInt64(r, "after", 0)
	if err != nil {
		writeReactionError(w, &schema.Error{Code: "invalid_request", Message: err.Error()})
		return
	}
	limit, err := nonNegativeQueryInt64(r, "limit", schema.DefaultReactorLimit)
	if err != nil || limit > schema.MaxReactorLimit {
		writeReactionError(w, &schema.Error{Code: "invalid_request", Message: "limit must be between 1 and " + strconv.Itoa(schema.MaxReactorLimit)})
		return
	}
	// 0 is refused by readReactors, with the others.
	doc, serr := s.readReactors(r.Context(), c, channel, r.PathValue("message_id"), r.PathValue("emoji"), after, int(limit))
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleBatchReactions serves GET /v2/channels/{channel}/reactions?messages=1,2,3.
func (s *Server) handleBatchReactions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.reactionRequest(w, r)
	if !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, r.PathValue("channel"), schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	var ids []int64
	if raw := r.URL.Query().Get("messages"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				writeReactionError(w, &schema.Error{Code: "invalid_request", Message: "message ids must be positive integers"})
				return
			}
			ids = append(ids, id)
			// Stop reading a list that is already too long.
			if len(ids) > schema.MaxReactionBatchMessages {
				break
			}
		}
	}
	doc, serr := s.readReactionBatch(r.Context(), c, channel, ids)
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// moderationChannel is the channel gate of the two moderation routes. The
// route class has said the caller is an operator or a manager of the channel;
// that is not membership, and an operator or manager who is not a member is
// told, as everyone is, that the channel does not exist (§11.5, review pin 1).
func (s *Server) moderationChannel(w http.ResponseWriter, r *http.Request) (store.Channel, bool) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "reactions: find channel failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, false
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "reactions: check membership failed", "error", err)
		writeInternalError(w)
		return store.Channel{}, false
	}
	if !member {
		writeChannelNotFound(w)
		return store.Channel{}, false
	}
	return channel, true
}

// handleModerateReaction serves DELETE .../reactions/{emoji}/{principal_id}.
func (s *Server) handleModerateReaction(w http.ResponseWriter, r *http.Request) {
	s.handleModerate(w, r, false)
}

// handleModerateReactor serves DELETE .../messages/{message_id}/reactors/{principal_id}.
func (s *Server) handleModerateReactor(w http.ResponseWriter, r *http.Request) {
	s.handleModerate(w, r, true)
}

func (s *Server) handleModerate(w http.ResponseWriter, r *http.Request, all bool) {
	c, ok := s.reactionRequest(w, r)
	if !ok {
		return
	}
	channel, ok := s.moderationChannel(w, r)
	if !ok {
		return
	}
	doc, serr := s.moderateReactions(r.Context(), c, channel, r.PathValue("message_id"), r.PathValue("emoji"), r.PathValue("principal_id"), all)
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// MCP -------------------------------------------------------------------

// The inputs of the four tools. Channels are named by name, as the other
// tools do; the server binds the agent's principal.
type mcpReactionInput struct {
	Channel   string `json:"channel" jsonschema:"channel name"`
	MessageID int64  `json:"message_id" jsonschema:"id of the message"`
	Emoji     string `json:"emoji" jsonschema:"one emoji, for example a thumbs up; any qualification of it is accepted and answered in its canonical form"`
}

type mcpReadReactionsInput struct {
	Channel    string  `json:"channel" jsonschema:"channel name"`
	MessageIDs []int64 `json:"message_ids" jsonschema:"ids of the messages, at most 100, each once"`
}

type mcpListReactorsInput struct {
	Channel   string `json:"channel" jsonschema:"channel name"`
	MessageID int64  `json:"message_id" jsonschema:"id of the message"`
	Emoji     string `json:"emoji" jsonschema:"the emoji whose reactors to list"`
	After     int64  `json:"after,omitempty" jsonschema:"return reactors with a principal id greater than this cursor"`
	Limit     *int64 `json:"limit,omitempty" jsonschema:"page size, default 50, max 100"`
}

// The outputs are the pkg/schema documents, projected through structs whose
// timestamps are strings: the SDK infers schema.Timestamp as an object.
type mcpReactionCount struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Me    bool   `json:"me,omitempty"`
}

type mcpReactionSummary struct {
	Schema    string             `json:"schema"`
	ChannelID int64              `json:"channel_id"`
	MessageID int64              `json:"message_id"`
	Reactions []mcpReactionCount `json:"reactions"`
	AsOf      string             `json:"as_of"`
}

type mcpReactionSummaries struct {
	Summaries []mcpReactionSummary `json:"summaries"`
}

type mcpReactor struct {
	PrincipalID  int64                      `json:"principal_id"`
	Emoji        string                     `json:"emoji"`
	CreatedAt    string                     `json:"created_at"`
	GuestReactor *schema.GuestAttributionV1 `json:"guest_reactor,omitempty"`
}

type mcpReactorList struct {
	Reactors  []mcpReactor `json:"reactors"`
	NextAfter int64        `json:"next_after,omitempty"`
}

// projectMCP copies a canonical document into its MCP output struct by its
// wire form.
func projectMCP[Out any](doc any) (Out, error) {
	var out Out
	wire, err := json.Marshal(doc)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(wire, &out)
	return out, err
}

// reactionScopeCall is the call both MCP fronts of a reaction share: the
// agent as a principal, the scope's subject and its reader.
func (a *agentScope) reactionCall() reactionCall {
	return reactionCall{
		principal: store.Principal{ID: a.identity.principalID, Kind: store.PrincipalAgent},
		subject:   a.subject,
		reader:    a.reader(),
	}
}

// channelForReactionWrite is the channel gate of an agent's reaction write:
// membership only, in the order of a scoped post (channelForAudience). The
// manifest is decided from the message's audience afterwards, and the budget
// spent there, once.
func (a *agentScope) channelForReactionWrite(ctx context.Context, name string) (store.Channel, *schema.Error) {
	if strings.TrimSpace(name) == "" {
		return store.Channel{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	channel, err := a.s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Channel{}, errChannelNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "authz: find channel failed", "error", err)
		return store.Channel{}, errInternal
	}
	member, err := a.s.store.IsChannelMember(ctx, channel.ID, a.identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "authz: check membership failed", "error", err)
		return store.Channel{}, errInternal
	}
	if !member {
		a.s.auditAgentDenial(ctx, a.identity.principalID, a.subject, a.capability, channel.ID, denyNotMember)
		return store.Channel{}, errChannelNotFound
	}
	return channel, nil
}

// channelForReactionRead is the channel gate of an agent's reaction read: the
// read permission in the channel, as read_channel needs, spending the budget.
func (a *agentScope) channelForReactionRead(ctx context.Context, name string) (store.Channel, *schema.Error) {
	if strings.TrimSpace(name) == "" {
		return store.Channel{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	granted, serr := a.channelByName(ctx, name, schema.ChannelPermissionRead)
	if serr != nil {
		return store.Channel{}, serr
	}
	return granted.channel, nil
}

// reactMCP is set_reaction (set true) and remove_reaction.
func (s *Server) reactMCP(ctx context.Context, scope *agentScope, in mcpReactionInput, set bool) (mcpReactionSummary, *schema.Error) {
	channel, serr := scope.channelForReactionWrite(ctx, in.Channel)
	if serr != nil {
		return mcpReactionSummary{}, serr
	}
	doc, serr := s.writeReaction(ctx, scope.reactionCall(), channel, strconv.FormatInt(in.MessageID, 10), in.Emoji, set)
	if serr != nil {
		return mcpReactionSummary{}, serr
	}
	return mustProject[mcpReactionSummary](ctx, doc)
}

// readReactionsMCP is read_reactions.
func (s *Server) readReactionsMCP(ctx context.Context, scope *agentScope, in mcpReadReactionsInput) (mcpReactionSummaries, *schema.Error) {
	channel, serr := scope.channelForReactionRead(ctx, in.Channel)
	if serr != nil {
		return mcpReactionSummaries{}, serr
	}
	doc, serr := s.readReactionBatch(ctx, scope.reactionCall(), channel, in.MessageIDs)
	if serr != nil {
		return mcpReactionSummaries{}, serr
	}
	return mustProject[mcpReactionSummaries](ctx, doc)
}

// listReactorsMCP is list_reactors.
func (s *Server) listReactorsMCP(ctx context.Context, scope *agentScope, in mcpListReactorsInput) (mcpReactorList, *schema.Error) {
	channel, serr := scope.channelForReactionRead(ctx, in.Channel)
	if serr != nil {
		return mcpReactorList{}, serr
	}
	limit := int64(schema.DefaultReactorLimit)
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit > schema.MaxReactorLimit {
		return mcpReactorList{}, &schema.Error{Code: "invalid_request", Message: "limit must be between 1 and " + strconv.Itoa(schema.MaxReactorLimit)}
	}
	doc, serr := s.readReactors(ctx, scope.reactionCall(), channel, strconv.FormatInt(in.MessageID, 10), in.Emoji, in.After, int(limit))
	if serr != nil {
		return mcpReactorList{}, serr
	}
	return mustProject[mcpReactorList](ctx, doc)
}

// mustProject projects a document built by this package, which cannot fail
// unless the types disagree; if it does, the call fails closed.
func mustProject[Out any](ctx context.Context, doc any) (Out, *schema.Error) {
	out, err := projectMCP[Out](doc)
	if err != nil {
		slog.ErrorContext(ctx, "reactions: project mcp output failed", "error", err)
		return out, errInternal
	}
	return out, nil
}
