package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Interventions (issue #287, P3 epic #263, decisions D9 and D10).
//
// An intervention is a person asking an agent's run to take a message or to
// stop. A human who can see the run may ask (D10: the same people who could
// prompt the agent by posting where it listens); operators get no exemption,
// and an agent may not ask, of its own run or another's. The adapter decides
// what happens and answers. The server records the request and the answer and
// never changes the run itself: a cancel that is applied does not make the
// run cancelled, only the adapter's state report does (UX-19).
//
// An intervention inherits its run's visibility, and the rule is the store's
// (store.RunVisible, audience.go): every read here is asked with the
// request's reader (readerFor), and a run the caller cannot see gets the
// answer a run that does not exist gets, errRunNotFound, on every route. The
// adapter reads what is waiting for it through its own session (D9, the
// own-session check) and answers through the credential that owns the run's
// session; anyone else is told the intervention does not exist.
//
// Interventions need a verified caller, as sessions and runs do: with
// authentication off every route here answers sessions_require_auth.

// errCodeHumanRequired is the code the approval routes already answer an agent
// with where only a person may act.
const errCodeHumanRequired = schema.ErrorCodeHumanRequired

var (
	errInterventionNotFound = &schema.Error{Code: schema.ErrorCodeInterventionNotFound, Message: "intervention not found"}
	callInterventionMissing = refuseWith(http.StatusNotFound, errInterventionNotFound)
)

// writeInterventionNotFound is the single response for an intervention that
// does not exist, that the caller may not see, or (on the answering route)
// that another credential's run owns.
func writeInterventionNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, errInterventionNotFound.Code, errInterventionNotFound.Message)
}

// interventionIDFromPath parses {id}. An id that is not a positive integer
// names no intervention, so it gets the not-found answer.
func interventionIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeInterventionNotFound(w)
		return 0, false
	}
	return id, true
}

// handleCreateIntervention serves POST /v1/runs/{id}/interventions. The order
// of the checks is part of the design:
//
//  1. a verified caller with a credential (sessions_require_auth otherwise);
//  2. a human (403 human_required for an agent, of any run: the answer must
//     not depend on whose run it is);
//  3. a run the caller may see (run_not_found otherwise), before the body is
//     read, so the answer for a run the caller may not see never depends on
//     what was sent;
//  4. a well-formed body;
//  5. the store, which decides again in one transaction: the run is still
//     visible, the session declared the kind (409 intervention_unsupported,
//     audited, nothing stored), the run has not ended (409 invalid_transition)
//     and fewer than store.MaxRequestedInterventions are waiting (429).
func (s *Server) handleCreateIntervention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, _, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	if caller.Kind != store.PrincipalHuman {
		writeError(w, http.StatusForbidden, errCodeHumanRequired, "interventions are requested by human principals only")
		return
	}
	runID, ok := runIDFromPath(w, r)
	if !ok {
		return
	}
	switch _, err := s.store.RunVisible(ctx, runID, readerFor(r, apiV2)); {
	case errors.Is(err, store.ErrRunNotFound):
		writeRunNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "interventions: read run failed", "run", runID, "error", err)
		writeInternalError(w)
		return
	}
	var req schema.CreateInterventionRequestV1
	if !decodeRunBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	iv, err := s.store.CreateIntervention(ctx, auditActor(ctx), store.InterventionParams{
		RunID: runID, Kind: req.Kind, Body: req.Body,
	}, readerFor(r, apiV2), s.now())
	switch {
	case errors.Is(err, store.ErrRunNotFound):
		// The run went out of the caller's sight since the check above.
		writeRunNotFound(w)
		return
	case errors.Is(err, store.ErrNotHuman):
		writeError(w, http.StatusForbidden, errCodeHumanRequired, "interventions are requested by human principals only")
		return
	case errors.Is(err, store.ErrInterventionUnsupported):
		writeError(w, http.StatusConflict, schema.ErrorCodeInterventionUnsupported, "the run's session does not act on that kind of intervention")
		return
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, http.StatusConflict, schema.ErrorCodeInvalidTransition, "the run, or the session it belongs to, has ended and takes no more interventions")
		return
	case errors.Is(err, store.ErrTooManyInterventions):
		writeError(w, http.StatusTooManyRequests, schema.ErrorCodeTooManyInterventions, "this run already has the most interventions waiting for an answer it may have")
		return
	case err != nil:
		slog.ErrorContext(ctx, "interventions: create failed", "principal", caller.ID, "run", runID, "error", err)
		writeInternalError(w)
		return
	}
	s.writeIntervention(w, r, http.StatusCreated, iv)
}

// handleListRunInterventions serves GET /v1/runs/{id}/interventions?after=&limit=
// to a reader who may see the run: its interventions, oldest first. An agent
// also needs messages.read with read in the run's channel, as for the run
// itself (handleGetRun). There is no next cursor on the list response: a page
// as long as the limit may have a successor, which is asked for with after set
// to the last id.
func (s *Server) handleListRunInterventions(w http.ResponseWriter, r *http.Request) {
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
		slog.ErrorContext(ctx, "interventions: read run failed", "run", runID, "error", err)
		writeInternalError(w)
		return
	}
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, run.ChannelID, schema.ChannelPermissionRead) {
		return
	}
	list, err := s.store.ListVisibleInterventions(ctx, run.ID, readerFor(r, apiV2), after, limit)
	if errors.Is(err, store.ErrRunNotFound) {
		writeRunNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "interventions: list failed", "run", run.ID, "error", err)
		writeInternalError(w)
		return
	}
	s.writeInterventionList(w, r, list)
}

// handleGetIntervention serves GET /v1/interventions/{id} to a reader who may
// see the intervention's run, under the gate handleListRunInterventions has.
func (s *Server) handleGetIntervention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := s.sessionCaller(w, r); !ok {
		return
	}
	id, ok := interventionIDFromPath(w, r)
	if !ok {
		return
	}
	iv, err := s.store.InterventionVisible(ctx, id, readerFor(r, apiV2))
	if errors.Is(err, store.ErrInterventionNotFound) {
		writeInterventionNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "interventions: read failed", "intervention", id, "error", err)
		writeInternalError(w)
		return
	}
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, iv.ChannelID, schema.ChannelPermissionRead) {
		return
	}
	s.writeIntervention(w, r, http.StatusOK, iv)
}

// handleListSessionInterventions serves
// GET /v1/sessions/{id}/interventions?state=&after=&limit=: the interventions
// on the runs of session id, oldest first, which is what an adapter polls
// (state=requested). Only the credential that owns the session may ask, and
// only for runs its principal still may see (the store's statement says so).
//
// This is the second call the own-session check, agentSelfAllowed, is for
// (decision D9; TestAgentSelfCheckCallers lists it): it names no channel, it
// asks for runs.manage alone, it acts on the caller's own session and reaches
// nothing of anyone else's, and it spends the capability's budget. A session
// that is not the caller's, has ended or never existed is session_not_found.
func (s *Server) handleListSessionInterventions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	if !s.agentSelfAllowed(w, r, schema.CapabilityRunsManage) {
		return
	}
	sessionID, ok := sessionIDFromPath(w, r)
	if !ok {
		return
	}
	state := schema.InterventionState(r.URL.Query().Get("state"))
	if state != "" && !state.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_request", "state must be one of requested, acknowledged, applied, failed")
		return
	}
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	list, cerr := s.sessionInterventions(ctx, restCall(r, caller, credentialID), sessionID, state, after, limit)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	s.writeInterventionList(w, r, list)
}

// sessionInterventions is what is waiting on session sessionID, which must be
// one of the call's credential that has not ended (session_not_found
// otherwise), in state state ("" for every state), after the id after. The
// REST route and the MCP tool poll_interventions both call it, after the
// agent gate (runs.manage, spent once) and the query.
func (s *Server) sessionInterventions(ctx context.Context, call sessionCall, sessionID int64, state schema.InterventionState, after int64, limit int) ([]store.Intervention, *callError) {
	switch _, err := s.store.OwnedSession(ctx, sessionID, call.credentialID); {
	case errors.Is(err, store.ErrNotFound):
		return nil, callSessionMissing
	case err != nil:
		slog.ErrorContext(ctx, "interventions: find session failed", "principal", call.caller.ID, "error", err)
		return nil, callInternal
	}
	list, err := s.store.ListSessionInterventions(ctx, sessionID, call.credentialID, call.reader, state, after, limit)
	if err != nil {
		slog.ErrorContext(ctx, "interventions: list session failed", "session", sessionID, "error", err)
		return nil, callInternal
	}
	return list, nil
}

// handleAckIntervention serves POST /v1/interventions/{id}/ack: the owner of
// the run's session records what the adapter did with the intervention.
// Anyone else gets intervention_not_found, whatever they hold. Like a report
// on the run itself, an answer re-checks the manifest's channel gate and
// spends runs.manage; the run's state is never touched.
func (s *Server) handleAckIntervention(w http.ResponseWriter, r *http.Request) {
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	id, ok := interventionIDFromPath(w, r)
	if !ok {
		return
	}
	call := restCall(r, caller, credentialID)
	iv, cerr := s.answerableIntervention(r.Context(), call, id)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	var req schema.AckInterventionRequestV1
	if !decodeRunBody(w, r, &req) {
		return
	}
	doc, cerr := s.ackIntervention(r.Context(), call, iv, req)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// ackIntervention records req as the answer to iv, which
// answerableIntervention has allowed. The REST route and the MCP tool
// ack_intervention both call it.
func (s *Server) ackIntervention(ctx context.Context, call sessionCall, iv store.Intervention, req schema.AckInterventionRequestV1) (schema.InterventionV1, *callError) {
	if err := req.Validate(); err != nil {
		return schema.InterventionV1{}, refuse(http.StatusBadRequest, "invalid_request", err.Error())
	}
	updated, err := s.store.AckIntervention(ctx, call.actor, iv.ID, call.credentialID, call.reader, req.State, req.Reason, s.now())
	switch {
	case errors.Is(err, store.ErrInterventionNotFound):
		return schema.InterventionV1{}, callInterventionMissing
	case errors.Is(err, store.ErrInvalidTransition):
		return schema.InterventionV1{}, refuse(http.StatusConflict, schema.ErrorCodeInvalidTransition, "the intervention cannot move from its current state to that one")
	case err != nil:
		slog.ErrorContext(ctx, "interventions: answer failed", "principal", call.caller.ID, "intervention", iv.ID, "error", err)
		return schema.InterventionV1{}, callInternal
	}
	return checkedInterventionDoc(ctx, updated)
}

// answerableIntervention is the check the answering route starts with, after
// the verified caller, on either front, as reportableRunFor is for a run's
// reports: the intervention must be on a run the call's credential owns and
// its caller may see (intervention_not_found otherwise, whoever asks), and the
// channel gate must still allow the caller to report on the run: runs.manage
// and the permission posting to the run's audience needs. A refusal by the
// manifest is 403, since the caller owns the run and learns nothing from it.
func (s *Server) answerableIntervention(ctx context.Context, call sessionCall, id int64) (store.Intervention, *callError) {
	iv, err := s.store.OwnedIntervention(ctx, id, call.credentialID, call.reader)
	if errors.Is(err, store.ErrInterventionNotFound) {
		return store.Intervention{}, callInterventionMissing
	}
	if err != nil {
		slog.ErrorContext(ctx, "interventions: find intervention failed", "principal", call.caller.ID, "error", err)
		return store.Intervention{}, callInternal
	}
	if _, cerr := s.reportableRunFor(ctx, call, iv.RunID, callInterventionMissing); cerr != nil {
		return store.Intervention{}, cerr
	}
	return iv, nil
}

// writeIntervention sends one intervention, after checking it is a valid
// document.
func (s *Server) writeIntervention(w http.ResponseWriter, r *http.Request, status int, iv store.Intervention) {
	doc, cerr := checkedInterventionDoc(r.Context(), iv)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	writeJSON(w, status, doc)
}

// checkedInterventionDoc is the wire form of iv, checked to be a valid
// document.
func checkedInterventionDoc(ctx context.Context, iv store.Intervention) (schema.InterventionV1, *callError) {
	doc := interventionDoc(iv)
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "interventions: built an invalid intervention", "intervention", iv.ID, "error", err)
		return schema.InterventionV1{}, callInternal
	}
	return doc, nil
}

// checkedInterventionList is the wire form of list, always an array, each
// checked to be a valid document.
func checkedInterventionList(ctx context.Context, list []store.Intervention) (schema.ListInterventionsResponseV1, *callError) {
	resp := schema.ListInterventionsResponseV1{Interventions: make([]schema.InterventionV1, 0, len(list))}
	for _, iv := range list {
		doc, cerr := checkedInterventionDoc(ctx, iv)
		if cerr != nil {
			return schema.ListInterventionsResponseV1{}, cerr
		}
		resp.Interventions = append(resp.Interventions, doc)
	}
	return resp, nil
}

// writeInterventionList sends a list of interventions, always as an array,
// after checking each is a valid document.
func (s *Server) writeInterventionList(w http.ResponseWriter, r *http.Request, list []store.Intervention) {
	resp, cerr := checkedInterventionList(r.Context(), list)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// interventionDoc is the wire form of an intervention.
func interventionDoc(iv store.Intervention) schema.InterventionV1 {
	doc := schema.InterventionV1{
		Schema:      schema.InterventionSchemaV1,
		ID:          iv.ID,
		RunID:       iv.RunID,
		Kind:        iv.Kind,
		Body:        iv.Body,
		State:       iv.State,
		RequestedBy: iv.RequestedBy,
		RequestedAt: schema.NewTimestamp(iv.RequestedAt),
		Reason:      iv.Reason,
	}
	if iv.AnsweredAt != nil {
		ts := schema.NewTimestamp(*iv.AnsweredAt)
		doc.AnsweredAt = &ts
	}
	return doc
}
