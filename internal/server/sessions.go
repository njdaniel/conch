package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Sessions (issue #265, P3 epic #263, decisions D1 and D9).
//
// A session is one live client or runtime of a principal. It is created
// explicitly, kept alive by a heartbeat, and ended by its client or by the
// revocation of its credential. It belongs to the credential that created it:
// another credential of the same principal, and an operator, are told it does
// not exist, exactly as for an id that was never issued. The server marks a
// silent session stale and then offline (the sweeper below) and never deletes
// a row.
//
// Sessions need a verified caller, because a session is bound to a credential:
// with authentication off every route here answers sessions_require_auth, as
// voice does. An agent needs the sessions.manage capability for the three
// routes that act on its own session; those use agentSelfCheck (authz.go), and
// TestAgentSelfCheckCallers pins which functions may.

const (
	// DefaultSessionStale and DefaultSessionOffline are the heartbeat
	// thresholds when none are configured.
	DefaultSessionStale   = 30 * time.Second
	DefaultSessionOffline = 90 * time.Second

	// defaultSessionSweepInterval is how often the sweeper looks at the table.
	defaultSessionSweepInterval = 5 * time.Second

	// sessionRosterWindow is how long after its last heartbeat a session
	// stays in a roster, if it has not ended. Older ones are only history.
	sessionRosterWindow = 24 * time.Hour
)

// ValidateSessionThresholds refuses heartbeat thresholds that cannot work:
// both must be positive and stale must be shorter than offline, or no session
// could ever be seen as stale.
func ValidateSessionThresholds(stale, offline time.Duration) error {
	switch {
	case stale <= 0 || offline <= 0:
		return errors.New("the session thresholds must be positive")
	case stale >= offline:
		return fmt.Errorf("--session-stale (%s) must be shorter than --session-offline (%s)", stale, offline)
	}
	return nil
}

// sessionThresholds is the configured stale and offline thresholds, with the
// defaults for the zero value.
func (s *Server) sessionThresholds() (stale, offline time.Duration) {
	stale, offline = s.cfg.SessionStale, s.cfg.SessionOffline
	if stale == 0 {
		stale = DefaultSessionStale
	}
	if offline == 0 {
		offline = DefaultSessionOffline
	}
	return stale, offline
}

var errSessionNotFound = &schema.Error{Code: schema.ErrorCodeSessionNotFound, Message: "session not found"}

// writeSessionNotFound is the single response for a session that does not
// exist, has ended, or belongs to another credential. Every session route
// that names a session must use it for all three; do not write a separate 404
// for any of them.
func writeSessionNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, errSessionNotFound.Code, errSessionNotFound.Message)
}

// sessionCaller is the check every session route starts with: there must be a
// verified caller and the credential it used. With authentication off there is
// neither, and sessions are never anonymous. A caller without a credential id
// under authentication is a wiring bug and is refused, not allowed.
func (s *Server) sessionCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeSessionsRequireAuth, "sessions require authentication")
		return store.Principal{}, 0, false
	}
	credentialID, ok := credentialIDFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	return caller, credentialID, true
}

// ownSessionCaller is sessionCaller followed by the agent gate for a call
// about the caller's own session: an agent needs sessions.manage. Humans have
// no manifest. It writes the response and returns false when the call must
// stop. Only the handlers of the three routes that act on the caller's own
// session call it.
func (s *Server) ownSessionCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	caller, credentialID, ok := s.sessionCaller(w, r)
	if !ok {
		return store.Principal{}, 0, false
	}
	if !s.agentSelfAllowed(w, r, schema.CapabilitySessionsManage) {
		return store.Principal{}, 0, false
	}
	return caller, credentialID, true
}

// handleCreateSession serves POST /v1/sessions. The caller's kind and
// credential decide who the session belongs to; the body says only what the
// client is.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	caller, credentialID, ok := s.ownSessionCaller(w, r)
	if !ok {
		return
	}
	var req schema.CreateSessionRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	call := restCall(r, caller, credentialID)
	doc, cerr := s.createSession(r.Context(), call, req)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// createSession stores a session for the call's credential, as the client
// described itself in req, and returns it as its owner sees it. The REST
// route and the MCP tool register_session both call it, after the agent gate
// (sessions.manage) and the body.
func (s *Server) createSession(ctx context.Context, call sessionCall, req schema.CreateSessionRequestV1) (schema.SessionV1, *callError) {
	if err := req.Validate(); err != nil {
		return schema.SessionV1{}, refuse(http.StatusBadRequest, "invalid_request", err.Error())
	}
	// SessionV1.Validate refuses these fields on a human session, so storing
	// them would make every roster that holds the session invalid.
	if call.caller.Kind == store.PrincipalHuman && (req.Runtime != "" || req.Model != "" || req.Host != "" || len(req.Interventions) > 0) {
		return schema.SessionV1{}, refuse(http.StatusBadRequest, "invalid_request", "a human session carries no runtime, model, host or interventions")
	}
	sess, err := s.store.CreateSession(ctx, call.actor, store.SessionParams{
		PrincipalID:   call.caller.ID,
		CredentialID:  call.credentialID,
		Client:        req.Client,
		Runtime:       req.Runtime,
		Model:         req.Model,
		Host:          req.Host,
		Interventions: req.Interventions,
	}, s.now())
	switch {
	case errors.Is(err, store.ErrCredentialInvalid):
		// Revoked, expired or disabled between authentication and now.
		return schema.SessionV1{}, callUnauthenticated
	case errors.Is(err, store.ErrTooManySessions):
		return schema.SessionV1{}, callTooManySess
	case err != nil:
		slog.ErrorContext(ctx, "sessions: create failed", "principal", call.caller.ID, "error", err)
		return schema.SessionV1{}, callInternal
	}
	return s.ownSessionDoc(ctx, call, sess)
}

// handleSessionHeartbeat serves POST /v1/sessions/{id}/heartbeat. Only the
// credential that created the session may send one.
func (s *Server) handleSessionHeartbeat(w http.ResponseWriter, r *http.Request) {
	caller, credentialID, ok := s.ownSessionCaller(w, r)
	if !ok {
		return
	}
	id, ok := sessionIDFromPath(w, r)
	if !ok {
		return
	}
	doc, cerr := s.heartbeatSession(r.Context(), restCall(r, caller, credentialID), id)
	if cerr != nil {
		writeCallError(w, cerr)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// heartbeatSession keeps session id of the call's credential alive, reviving
// it if it went stale or offline, and returns it as its owner sees it. The
// REST route and the MCP tool heartbeat both call it, after the agent gate.
func (s *Server) heartbeatSession(ctx context.Context, call sessionCall, id int64) (schema.SessionV1, *callError) {
	sess, err := s.store.HeartbeatSession(ctx, id, call.credentialID, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return schema.SessionV1{}, callSessionMissing
	case errors.Is(err, store.ErrTooManySessions):
		return schema.SessionV1{}, callTooManySess
	case err != nil:
		slog.ErrorContext(ctx, "sessions: heartbeat failed", "principal", call.caller.ID, "error", err)
		return schema.SessionV1{}, callInternal
	}
	return s.ownSessionDoc(ctx, call, sess)
}

// handleEndSession serves DELETE /v1/sessions/{id}. Only the credential that
// created the session may end it; ending one that has ended is the same 404.
func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	caller, credentialID, ok := s.ownSessionCaller(w, r)
	if !ok {
		return
	}
	id, ok := sessionIDFromPath(w, r)
	if !ok {
		return
	}
	if cerr := s.endSession(r.Context(), restCall(r, caller, credentialID), id); cerr != nil {
		writeCallError(w, cerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// endSession ends session id of the call's credential. The REST route and the
// MCP tool end_session both call it, after the agent gate.
func (s *Server) endSession(ctx context.Context, call sessionCall, id int64) *callError {
	err := s.store.EndSession(ctx, call.actor, id, call.credentialID, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return callSessionMissing
	case err != nil:
		slog.ErrorContext(ctx, "sessions: end failed", "principal", call.caller.ID, "error", err)
		return callInternal
	}
	return nil
}

// sessionIDFromPath parses {id}. An id that is not a positive integer names no
// session, so it gets the not-found answer and not a 400: the answer must not
// depend on what the id looks like.
func sessionIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeSessionNotFound(w)
		return 0, false
	}
	return id, true
}

// handleListSessions serves GET /v1/sessions?channel={name}: the live
// sessions of the channel's current members, for members of the channel.
// This is a read of other people's presence, so it is gated like the channel's
// content (channelForCaller): an unknown channel and a channel the caller is
// not a member of are the same 404, operators included, and an agent also
// needs messages.read with read on the channel, as for the members list. It
// is not one of the calls agentSelfCheck is for.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, _, ok := s.sessionCaller(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel is required")
		return
	}
	channel, ok := s.channelForCaller(w, r, name, schema.CapabilityMessagesRead, schema.ChannelPermissionRead)
	if !ok {
		return
	}
	sessions, err := s.store.ListChannelSessions(ctx, channel.ID, s.now(), sessionRosterWindow)
	if err != nil {
		slog.ErrorContext(ctx, "sessions: list failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	ids := make([]int64, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	// The runs as this reader may see them: every reader is shown each
	// session's status, and only a reader who may see its open run is shown
	// which run it is and its task (D8).
	runs, err := s.store.SessionRunViews(ctx, ids, readerFor(r, apiV2))
	if err != nil {
		slog.ErrorContext(ctx, "sessions: read runs failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	roster := schema.SessionRosterV1{
		Schema:    schema.SessionRosterSchemaV1,
		ChannelID: channel.ID,
		Sessions:  make([]schema.SessionV1, 0, len(sessions)),
	}
	for _, sess := range sessions {
		roster.Sessions = append(roster.Sessions, sessionDoc(sess, caller, runs[sess.ID]))
	}
	if err := roster.Validate(); err != nil {
		slog.ErrorContext(ctx, "sessions: built an invalid roster", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, roster)
}

// ownSessionDoc is one session as its owner sees it, with its runs as the
// owner may see them: an agent that has left a run's channel is not shown that
// run. It is checked to be a valid document.
func (s *Server) ownSessionDoc(ctx context.Context, call sessionCall, sess store.Session) (schema.SessionV1, *callError) {
	runs, err := s.store.SessionRunViews(ctx, []int64{sess.ID}, call.reader)
	if err != nil {
		slog.ErrorContext(ctx, "sessions: read runs failed", "session", sess.ID, "error", err)
		return schema.SessionV1{}, callInternal
	}
	doc := sessionDoc(sess, call.caller, runs[sess.ID])
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "sessions: built an invalid session document", "session", sess.ID, "error", err)
		return schema.SessionV1{}, callInternal
	}
	return doc, nil
}

// sessionDoc is the wire form of sess as viewer may see it, given its runs as
// the same viewer may see them (store.SessionRunViews). Runtime and host say
// where an agent runs, so they are shown only to an operator and to the
// session's own principal; the model is shown to everyone who sees the
// session. Interventions are always a list.
func sessionDoc(sess store.Session, viewer store.Principal, runs store.SessionRuns) schema.SessionV1 {
	doc := schema.SessionV1{
		Schema:        schema.SessionSchemaV1,
		ID:            sess.ID,
		PrincipalID:   sess.PrincipalID,
		Kind:          schema.PrincipalKind(sess.Kind),
		Name:          sess.Name,
		Client:        sess.Client,
		State:         sess.State,
		Status:        deriveSessionStatus(sess, runs),
		StartedAt:     schema.NewTimestamp(sess.StartedAt),
		LastSeenAt:    schema.NewTimestamp(sess.LastSeenAt),
		Model:         sess.Model,
		Interventions: append([]schema.InterventionKind{}, sess.Interventions...),
	}
	if sess.EndedAt != nil {
		ts := schema.NewTimestamp(*sess.EndedAt)
		doc.EndedAt = &ts
	}
	if isOperator(viewer) || viewer.ID == sess.PrincipalID {
		doc.Runtime = sess.Runtime
		doc.Host = sess.Host
	}
	if sess.Kind == store.PrincipalAgent {
		// Filled only when the viewer may see the open run; the store leaves
		// both empty otherwise.
		doc.CurrentRun, doc.Task = runs.CurrentRun, runs.Task
		// A run still open on a session nobody is hearing from: its state is
		// whatever was last reported, and the reader is told not to trust it
		// (P3-07). Nothing changes the run.
		quiet := sess.State == schema.SessionStateStale || sess.State == schema.SessionStateOffline
		doc.OutcomeUnknown = quiet && runs.OpenState != ""
	}
	return doc
}

// deriveSessionStatus is what a roster shows for sess, computed on read from
// its liveness and its runs. Liveness wins: a stale or offline session shows
// as that and nothing else. A live human is online. A live agent shows its
// most recent open run's state: running while it is queued, starting or
// running, blocked or waiting as it says; with no run open, failed when its
// most recent run failed, and idle otherwise.
//
// This is the one place the status is derived. It is shown to every reader of
// the roster, whether or not they may see the run it comes from.
func deriveSessionStatus(sess store.Session, runs store.SessionRuns) schema.SessionStatus {
	switch sess.State {
	case schema.SessionStateStale:
		return schema.SessionStatusStale
	case schema.SessionStateOffline:
		return schema.SessionStatusOffline
	}
	if sess.Kind != store.PrincipalAgent {
		return schema.SessionStatusOnline
	}
	switch runs.OpenState {
	case schema.RunStateQueued, schema.RunStateStarting, schema.RunStateRunning:
		return schema.SessionStatusRunning
	case schema.RunStateBlocked:
		return schema.SessionStatusBlocked
	case schema.RunStateWaiting:
		return schema.SessionStatusWaiting
	}
	if runs.LastState == schema.RunStateFailed {
		return schema.SessionStatusFailed
	}
	return schema.SessionStatusIdle
}

// sweepSessions applies the heartbeat thresholds once, at the server's clock.
// It holds no transaction between calls: the store opens and closes its own.
// A failure is logged and the next pass tries again; a pass cut short by
// shutdown is not a failure.
func (s *Server) sweepSessions(ctx context.Context) {
	stale, offline := s.sessionThresholds()
	res, err := s.store.SweepSessions(ctx, s.now(), stale, offline)
	if err != nil {
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "sessions: sweep failed", "error", err)
		}
		return
	}
	if res.Stale > 0 || res.Offline > 0 {
		slog.DebugContext(ctx, "sessions: swept", "stale", res.Stale, "offline", res.Offline)
	}
}

// runSessionSweeper sweeps on every tick until ctx is done. Ticks are a
// parameter so a test can drive it without waiting for time to pass.
func (s *Server) runSessionSweeper(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			s.sweepSessions(ctx)
		}
	}
}

// startSessionSweeper starts the sweeper and returns the function that stops
// it and waits until it has: a pass in flight is cancelled, so this is prompt.
// Serve calls it, so the sweeper runs exactly as long as the server serves.
func (s *Server) startSessionSweeper(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(s.sessionSweepInterval)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ticker.Stop()
		s.runSessionSweeper(ctx, ticker.C)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}
