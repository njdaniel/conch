package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Runs and activity (issue #266, P3 epic #263, decisions D8 and D9).
//
// A run is one piece of work an agent session reports, in one channel and
// possibly answering one message there. Who may see it is the store's rule
// (store.RunVisible, beside the message rule in audience.go): exactly the
// readers who can see the message it answers, or the channel's members when it
// answers none. Every read here asks that rule with the request's reader
// (readerFor), and a run the caller cannot see gets the answer a run that does
// not exist gets, errRunNotFound, on every route.
//
// Only an agent starts a run, on a session its own credential owns; only the
// credential that owns the run's session reports on it. Every report goes
// through the channel gate (agentManifestCheck) with runs.manage and the
// permission posting to the run's audience needs, and spends runs.manage once
// (D9). Runs need a verified caller, because they hang off a session: with
// authentication off every route here answers sessions_require_auth.
//
// The server records what the agent reports and never invents a state or an
// outcome: nothing here, and nothing in the session sweeper, changes a run
// because its session went quiet.

const (
	defaultRunLimit = 50
	maxRunLimit     = 100
)

var errRunNotFound = &schema.Error{Code: schema.ErrorCodeRunNotFound, Message: "run not found"}

// writeRunNotFound is the single response for a run that does not exist, that
// the caller may not see, or (on the reporting routes) that another
// credential owns. Every run route that names a run uses it for all of them.
func writeRunNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, errRunNotFound.Code, errRunNotFound.Message)
}

// runIDFromPath parses {id}. An id that is not a positive integer names no
// run, so it gets the not-found answer: the answer must not depend on what the
// id looks like.
func runIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeRunNotFound(w)
		return 0, false
	}
	return id, true
}

// decodeRunBody decodes a run route's JSON body and writes the 400 or 413 when
// it cannot.
func decodeRunBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeJSONBody(w, r, dst); err != nil {
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

// handleCreateRun serves POST /v1/sessions/{id}/runs. The order of the checks
// is part of the design:
//
//  1. a verified caller with a credential (sessions_require_auth otherwise);
//  2. an agent (a human gets 403 agent_required: runs are reported by agents);
//  3. a session of the caller's own credential that has not ended
//     (session_not_found otherwise, the answer every session route gives);
//  4. a well-formed body;
//  5. the channel, the caller's membership of it, the message the run
//     answers (in that channel and visible to the caller), and the manifest
//     gate. Every refusal here is the refusal a missing channel gets, so the
//     answer never says which of them failed: whether a message exists, where
//     it is and who it is for are not the caller's to learn by asking.
//
// The store checks the session, the membership and the message again inside
// the transaction that stores the run.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	if caller.Kind != store.PrincipalAgent {
		writeError(w, http.StatusForbidden, schema.ErrorCodeAgentRequired, "runs are reported by agent principals only")
		return
	}
	sessionID, ok := sessionIDFromPath(w, r)
	if !ok {
		return
	}
	switch _, err := s.store.OwnedSession(ctx, sessionID, credentialID); {
	case errors.Is(err, store.ErrNotFound):
		writeSessionNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "runs: find session failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	var req schema.CreateRunRequestV1
	if !decodeRunBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	channel, err := s.store.ChannelByName(ctx, req.Channel)
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "runs: find channel failed", "error", err)
		writeInternalError(w)
		return
	}
	member, err := s.store.IsChannelMember(ctx, channel.ID, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "runs: check membership failed", "error", err)
		writeInternalError(w)
		return
	}
	if !member {
		s.auditAgentNonMember(r, schema.CapabilityRunsManage, channel.ID)
		writeChannelNotFound(w)
		return
	}
	var audience *schema.Audience
	if req.MessageID > 0 {
		audience, err = s.store.RunAnchorAudience(ctx, channel.ID, req.MessageID, readerFor(r, apiV2))
		if errors.Is(err, store.ErrRunAnchorNotFound) {
			writeChannelNotFound(w)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "runs: find the message a run answers failed", "error", err)
			writeInternalError(w)
			return
		}
	}
	allowed, limited, err := s.runGate(ctx, caller, agentSubjectFor(r, caller), channel.ID, audience)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "runs: authorize failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	case limited != nil:
		writeRateLimited(w, limited)
		return
	case !allowed:
		writeChannelNotFound(w)
		return
	}

	run, err := s.store.CreateRun(ctx, auditActor(ctx), store.RunParams{
		SessionID: sessionID, CredentialID: credentialID, ChannelID: channel.ID, MessageID: req.MessageID,
		Task: req.Task, Link: req.Link,
	}, readerFor(r, apiV2), s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The session ended, or the credential went, since the check above.
		writeSessionNotFound(w)
		return
	case errors.Is(err, store.ErrNotChannelMember), errors.Is(err, store.ErrRunAnchorNotFound):
		// Removed from the channel, or lost sight of the message, since the
		// checks above.
		writeChannelNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "runs: create failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	s.events.notifyRun(run.ChannelID, run.ID)
	s.writeRun(w, r, http.StatusCreated, run)
}

// runGate is the channel gate for reporting on a run (D9): the agent's
// manifest must grant runs.manage and, in channelID, every permission posting
// to the run's audience needs (runPermissions). Refusals are audited by
// agentManifestCheck under subject. Only when every check passes is
// runs.manage spent, once; limited is then non-nil if the agent is over its
// limit. A store failure is an error and never an allow.
func (s *Server) runGate(ctx context.Context, caller store.Principal, subject string, channelID int64, audience *schema.Audience) (allowed bool, limited *rateLimitedError, err error) {
	if caller.Kind != store.PrincipalAgent {
		// Runs are an agent's; nothing else passes this gate.
		return false, nil, nil
	}
	permissions, err := s.runPermissions(ctx, caller, channelID, audience)
	if err != nil {
		return false, nil, err
	}
	var m schema.AgentManifestV1
	for _, permission := range permissions {
		granted, ok, err := s.agentManifestCheck(ctx, caller.ID, subject, schema.CapabilityRunsManage, channelID, permission)
		if err != nil || !ok {
			return false, nil, err
		}
		m = granted
	}
	if limited := s.spendAgentBudget(ctx, caller.ID, subject, m, schema.CapabilityRunsManage); limited != nil {
		return false, limited, nil
	}
	return true, nil, nil
}

// runPermissions is what posting to a run's audience needs in its channel,
// because a run's task and activity reach that audience as a reply would:
// post for a run that answers no message or a channel-wide one; post_net for
// one answering a net message; whisper for one answering a whisper, and
// whisper_agent as well when another agent who is a member of the channel is
// in the whisper, exactly as authorizeScopedPostGates decides for a post.
func (s *Server) runPermissions(ctx context.Context, caller store.Principal, channelID int64, audience *schema.Audience) ([]schema.ChannelPermission, error) {
	switch {
	case audience == nil:
		return []schema.ChannelPermission{schema.ChannelPermissionPost}, nil
	case audience.Kind == schema.AudienceKindNet:
		return []schema.ChannelPermission{schema.ChannelPermissionPostNet}, nil
	case audience.Kind != schema.AudienceKindPrincipals:
		// Never read an audience we do not understand as something wider.
		return nil, errors.New("server: a run answers a message with an unknown audience")
	}
	needed := []schema.ChannelPermission{schema.ChannelPermissionWhisper}
	for _, id := range audience.PrincipalIDs {
		if id == caller.ID {
			continue
		}
		member, err := s.store.IsChannelMember(ctx, channelID, id)
		if err != nil {
			return nil, err
		}
		if !member {
			continue
		}
		target, err := s.store.PrincipalByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if target.Kind == store.PrincipalAgent {
			return append(needed, schema.ChannelPermissionWhisperAgent), nil
		}
	}
	return needed, nil
}

// reportableRun is the check every reporting route (state, activity) starts
// with, after the verified caller: the run must be one the caller's credential
// owns and the caller may see (run_not_found otherwise, whoever asks and
// whatever they hold), and the channel gate must still allow the caller to
// report on it. It writes the response and returns false when the call must
// stop. A refusal by the manifest is 403: the caller owns the run, so the
// answer tells it nothing it does not know.
func (s *Server) reportableRun(w http.ResponseWriter, r *http.Request, caller store.Principal, credentialID int64) (store.Run, bool) {
	ctx := r.Context()
	runID, ok := runIDFromPath(w, r)
	if !ok {
		return store.Run{}, false
	}
	run, err := s.store.OwnedRun(ctx, runID, credentialID, readerFor(r, apiV2))
	if errors.Is(err, store.ErrRunNotFound) {
		writeRunNotFound(w)
		return store.Run{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "runs: find run failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return store.Run{}, false
	}
	var audience *schema.Audience
	if run.MessageID > 0 {
		audience, err = s.store.RunAnchorAudience(ctx, run.ChannelID, run.MessageID, readerFor(r, apiV2))
		if errors.Is(err, store.ErrRunAnchorNotFound) {
			// The run was visible a moment ago and its message is not now.
			writeRunNotFound(w)
			return store.Run{}, false
		}
		if err != nil {
			slog.ErrorContext(ctx, "runs: find the message a run answers failed", "error", err)
			writeInternalError(w)
			return store.Run{}, false
		}
	}
	allowed, limited, err := s.runGate(ctx, caller, agentSubjectFor(r, caller), run.ChannelID, audience)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "runs: authorize failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return store.Run{}, false
	case limited != nil:
		writeRateLimited(w, limited)
		return store.Run{}, false
	case !allowed:
		writeError(w, http.StatusForbidden, errForbidden.Code, errForbidden.Message)
		return store.Run{}, false
	}
	return run, true
}

// handleSetRunState serves POST /v1/runs/{id}/state: the owner moves its run
// along the lifecycle. An illegal transition is 409 invalid_transition and
// changes nothing.
func (s *Server) handleSetRunState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	run, ok := s.reportableRun(w, r, caller, credentialID)
	if !ok {
		return
	}
	var req schema.SetRunStateRequestV1
	if !decodeRunBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	updated, err := s.store.SetRunState(ctx, auditActor(ctx), run.ID, credentialID, readerFor(r, apiV2), req.State, req.Outcome, s.now())
	switch {
	case errors.Is(err, store.ErrRunNotFound):
		writeRunNotFound(w)
		return
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, http.StatusConflict, schema.ErrorCodeInvalidTransition, "the run cannot move from its current state to that one")
		return
	case err != nil:
		slog.ErrorContext(ctx, "runs: set state failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	s.events.notifyRun(updated.ChannelID, updated.ID)
	s.writeRun(w, r, http.StatusOK, updated)
}

// handleAppendRunActivity serves POST /v1/runs/{id}/activity: the owner adds
// one entry to its run's stream. A run that has ended takes no more (409
// invalid_transition). The time is the server's.
func (s *Server) handleAppendRunActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	run, ok := s.reportableRun(w, r, caller, credentialID)
	if !ok {
		return
	}
	var req schema.AppendActivityRequestV1
	if !decodeRunBody(w, r, &req) {
		return
	}
	// "data": null is no data. Decoding keeps the literal null in a
	// RawMessage, which would be stored and sent back as "data":null on every
	// read of an entry that has none.
	if bytes.Equal(bytes.TrimSpace(req.Data), []byte("null")) {
		req.Data = nil
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var data json.RawMessage
	if len(req.Data) > 0 {
		// Kept as encoding/json writes it (compact, HTML-escaped), the form
		// the schema bounds, so what is stored is what is sent on.
		encoded, err := json.Marshal(req.Data)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "data must be valid JSON")
			return
		}
		data = encoded
	}
	entry, err := s.store.AppendRunActivity(ctx, run.ID, credentialID, readerFor(r, apiV2), store.ActivityParams{
		Kind: req.Kind, Summary: req.Summary, Outcome: req.Outcome, Data: data,
	}, s.now())
	switch {
	case errors.Is(err, store.ErrRunNotFound):
		writeRunNotFound(w)
		return
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, http.StatusConflict, schema.ErrorCodeInvalidTransition, "the run has ended and takes no more activity")
		return
	case err != nil:
		slog.ErrorContext(ctx, "runs: append activity failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	s.events.notifyActivity(run.ChannelID, run.ID, entry.ID)
	doc := activityDoc(entry)
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "runs: built an invalid activity entry", "run", run.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// handleGetRun serves GET /v1/runs/{id} to a reader who may see the run. An
// agent also needs messages.read with read in the run's channel, as for
// reading the channel's messages: a run is the channel's content.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.sessionCaller(w, r); !ok {
		return
	}
	runID, ok := runIDFromPath(w, r)
	if !ok {
		return
	}
	run, err := s.store.RunVisible(ctx, runID, readerFor(r, apiV2))
	if errors.Is(err, store.ErrRunNotFound) {
		writeRunNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "runs: read failed", "error", err)
		writeInternalError(w)
		return
	}
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, run.ChannelID, schema.ChannelPermissionRead) {
		return
	}
	s.writeRun(w, r, http.StatusOK, run)
}

// pageParams reads after and limit for the run listings, with the bounds the
// message listing has. It writes the 400 and returns false when they are bad.
func pageParams(w http.ResponseWriter, r *http.Request) (after int64, limit int, ok bool) {
	after, err := nonNegativeQueryInt64(r, "after", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return 0, 0, false
	}
	parsed, err := nonNegativeQueryInt64(r, "limit", defaultRunLimit)
	if err != nil || parsed == 0 || parsed > maxRunLimit {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		return 0, 0, false
	}
	return after, int(parsed), true
}

// handleListRuns serves GET /v1/runs?channel={name}&session=&after=&limit=:
// the runs of the channel the caller may see, ascending by id, optionally of
// one session. It is gated like the channel's messages (channelForCaller): an
// unknown channel and one the caller is not a member of are the same 404,
// operators included. Runs the caller may not see are filtered in the store's
// statement, before the limit, so they neither fill a page nor move the
// cursor.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.sessionCaller(w, r); !ok {
		return
	}
	query := r.URL.Query()
	name := query.Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel is required")
		return
	}
	// An empty session= is no filter, as an empty after= or limit= is the
	// default.
	sessionID := int64(0)
	if raw := query.Get("session"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "session must be a positive integer")
			return
		}
		sessionID = id
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	channel, ok := s.channelForCaller(w, r, name, schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	// One extra row, so next_after is advertised only when a further visible
	// run exists now.
	runs, err := s.store.ListVisibleRuns(ctx, channel.ID, readerFor(r, apiV2), sessionID, after, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "runs: list failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.ListRunsResponseV1{Runs: make([]schema.RunV1, 0, len(runs))}
	if len(runs) > limit {
		runs = runs[:limit]
		resp.NextAfter = runs[len(runs)-1].ID
	}
	for _, run := range runs {
		doc := runDoc(run)
		if err := doc.Validate(); err != nil {
			slog.ErrorContext(ctx, "runs: built an invalid run", "run", run.ID, "error", err)
			writeInternalError(w)
			return
		}
		resp.Runs = append(resp.Runs, doc)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleListRunActivity serves GET /v1/runs/{id}/activity?after=&limit= to a
// reader who may see the run: its entries, oldest first. The gate is
// handleGetRun's.
func (s *Server) handleListRunActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.sessionCaller(w, r); !ok {
		return
	}
	runID, ok := runIDFromPath(w, r)
	if !ok {
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	run, err := s.store.RunVisible(ctx, runID, readerFor(r, apiV2))
	if errors.Is(err, store.ErrRunNotFound) {
		writeRunNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "runs: read failed", "error", err)
		writeInternalError(w)
		return
	}
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, run.ChannelID, schema.ChannelPermissionRead) {
		return
	}
	entries, err := s.store.ListVisibleActivity(ctx, run.ID, readerFor(r, apiV2), after, limit+1)
	if errors.Is(err, store.ErrRunNotFound) {
		writeRunNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "runs: list activity failed", "run", run.ID, "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.ListActivityResponseV1{Activity: make([]schema.AgentActivityV1, 0, len(entries))}
	if len(entries) > limit {
		entries = entries[:limit]
		resp.NextAfter = entries[len(entries)-1].ID
	}
	for _, entry := range entries {
		doc := activityDoc(entry)
		if err := doc.Validate(); err != nil {
			slog.ErrorContext(ctx, "runs: built an invalid activity entry", "run", run.ID, "error", err)
			writeInternalError(w)
			return
		}
		resp.Activity = append(resp.Activity, doc)
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeRun sends one run, after checking it is a valid document.
func (s *Server) writeRun(w http.ResponseWriter, r *http.Request, status int, run store.Run) {
	doc := runDoc(run)
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(r.Context(), "runs: built an invalid run", "run", run.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, status, doc)
}

// runDoc is the wire form of a run.
func runDoc(run store.Run) schema.RunV1 {
	doc := schema.RunV1{
		Schema:      schema.RunSchemaV1,
		ID:          run.ID,
		SessionID:   run.SessionID,
		PrincipalID: run.PrincipalID,
		ChannelID:   run.ChannelID,
		MessageID:   run.MessageID,
		State:       run.State,
		Task:        run.Task,
		Link:        run.Link,
		StartedAt:   schema.NewTimestamp(run.StartedAt),
		Outcome:     run.Outcome,
	}
	if run.EndedAt != nil {
		ts := schema.NewTimestamp(*run.EndedAt)
		doc.EndedAt = &ts
	}
	return doc
}

// activityDoc is the wire form of an activity entry.
func activityDoc(a store.Activity) schema.AgentActivityV1 {
	return schema.AgentActivityV1{
		Schema:      schema.AgentActivitySchemaV1,
		ID:          a.ID,
		RunID:       a.RunID,
		PrincipalID: a.PrincipalID,
		Kind:        a.Kind,
		Summary:     a.Summary,
		Outcome:     a.Outcome,
		At:          schema.NewTimestamp(a.At),
		Data:        a.Data,
	}
}
