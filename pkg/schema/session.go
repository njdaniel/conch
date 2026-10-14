package schema

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Session wire shapes (issue #264; P3 epic #263, decision D1). A principal is
// the durable identity; a session is one live client or runtime connected as
// it. One principal on two clients is two sessions. Humans and agents share
// the one shape: a human session carries only what a human has, and the
// agent-only fields (runtime, model, host, declared interventions, the
// current run) are refused on it, so a reader never has to guess whether an
// absent field was hidden or never applied.
//
// Stored liveness is State (online, stale, offline) and the server flips it
// on heartbeat thresholds. Status is what a roster shows and is derived on
// read from State and the session's runs; the schema pins the vocabulary and
// the part of the derivation that never changes (a stale or offline session
// shows as nothing else), and leaves the run-dependent part to the server.

// SessionSchemaV1 is the versioned name identifying the v1 session document
// on the wire. Every SessionV1 carries it, inside a roster too, because the
// events socket streams single sessions as frames and a reader tells frames
// apart by this field alone.
const SessionSchemaV1 = "conch.session.v1"

// SessionRosterSchemaV1 is the versioned name identifying the v1 session
// roster: the whole set of sessions visible in one channel.
const SessionRosterSchemaV1 = "conch.session_roster.v1"

// Error codes returned by the session routes in Error.Code, beside the voice
// codes in voice.go and ErrorCodeRateLimited in agent_manifest.go.
const (
	// ErrorCodeSessionNotFound: no session the caller may see has that id
	// (HTTP 404). A session the caller may not see gets the same answer.
	ErrorCodeSessionNotFound = "session_not_found"
	// ErrorCodeSessionsRequireAuth: the caller is not a verified principal
	// (conchd runs with authentication off). A session is bound to a
	// credential, so there is nothing to bind it to.
	ErrorCodeSessionsRequireAuth = "sessions_require_auth"
	// ErrorCodeTooManySessions: the caller's credential already holds as
	// many live sessions as the server allows (HTTP 429). The bound is per
	// credential, not per principal: one person on two devices is two
	// credentials with a budget each.
	ErrorCodeTooManySessions = "too_many_sessions"
	// ErrorCodeAgentRequired: the call is for agent principals only, as a
	// human called it (HTTP 403). It mirrors the approval routes'
	// human_required.
	ErrorCodeAgentRequired = "agent_required"
)

// Length limits on the free-text fields of a session, in characters.
const (
	// MaxSessionClientLength bounds Client, the name of the connecting
	// program.
	MaxSessionClientLength = 64
	// MaxSessionLabelLength bounds each of Runtime, Model and Host.
	MaxSessionLabelLength = 128
)

// SessionState is a session's stored liveness. The vocabulary is closed: a
// value outside it fails validation.
//
// The server flips the state on thresholds and never deletes a row (D1). A
// heartbeat or a new events socket on a stale or offline session that has
// not ended revives the same row to online; only a session with ended_at set
// is final and never changes again.
type SessionState string

// Session states.
const (
	// SessionStateOnline: a heartbeat or an open events socket was seen
	// within the stale threshold.
	SessionStateOnline SessionState = "online"
	// SessionStateStale: nothing was heard for longer than the stale
	// threshold but less than the offline one. A heartbeat revives it.
	SessionStateStale SessionState = "stale"
	// SessionStateOffline: nothing was heard for longer than the offline
	// threshold, or the session ended. A heartbeat revives a session that has
	// not ended; an ended one (EndedAt set) stays offline for good.
	SessionStateOffline SessionState = "offline"
)

// Valid reports whether s is a recognized session state.
func (s SessionState) Valid() bool {
	switch s {
	case SessionStateOnline, SessionStateStale, SessionStateOffline:
		return true
	default:
		return false
	}
}

// SessionStatus is what a roster shows for a session: liveness for a human,
// liveness or the state of the current run for an agent. The server derives
// it on read; a client never computes it. The vocabulary is closed.
type SessionStatus string

// Session statuses.
const (
	// SessionStatusOnline: a live human session.
	SessionStatusOnline SessionStatus = "online"
	// SessionStatusIdle: a live agent session with no open run.
	SessionStatusIdle SessionStatus = "idle"
	// SessionStatusRunning: the agent's open run is queued, starting or
	// running.
	SessionStatusRunning SessionStatus = "running"
	// SessionStatusBlocked: the agent's open run is blocked.
	SessionStatusBlocked SessionStatus = "blocked"
	// SessionStatusWaiting: the agent's open run is waiting.
	SessionStatusWaiting SessionStatus = "waiting"
	// SessionStatusFailed: the agent's last run failed and none is open.
	SessionStatusFailed SessionStatus = "failed"
	// SessionStatusStale: the session's state is stale.
	SessionStatusStale SessionStatus = "stale"
	// SessionStatusOffline: the session's state is offline.
	SessionStatusOffline SessionStatus = "offline"
)

// Valid reports whether s is a recognized session status.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionStatusOnline, SessionStatusIdle, SessionStatusRunning, SessionStatusBlocked,
		SessionStatusWaiting, SessionStatusFailed, SessionStatusStale, SessionStatusOffline:
		return true
	default:
		return false
	}
}

// SessionV1 is the v1 wire representation of one session: the body of each
// entry of a roster and one kind of frame on the events socket.
//
// Interventions is always a JSON array: the kinds the session declared it
// will act on, empty for a human and for an agent that declared none. A
// client offers a reader only the kinds listed here (UX-19).
//
// CurrentRun and Task are the open run and its task, present together or not
// at all: the server fills them only for a reader who may see that run (D8),
// while Status is shown to everyone who sees the session. OutcomeUnknown is
// true when the session is stale or offline with a run still open (P3-07):
// the run keeps the state it had, nothing invents an outcome for it, and a
// reader is told not to trust that state. It is never true on an online
// session, whose runs are being reported.
type SessionV1 struct {
	// Schema is the document discriminator; always SessionSchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned session id.
	ID int64 `json:"id"`
	// PrincipalID references the principal the session belongs to.
	PrincipalID int64 `json:"principal_id"`
	// Kind mirrors the principal's kind: human or agent.
	Kind PrincipalKind `json:"kind"`
	// Name is the principal's display name, as the members list shows it.
	Name string `json:"name"`
	// Client names the connecting program, as the client reported it.
	Client string `json:"client"`
	// State is the stored liveness.
	State SessionState `json:"state"`
	// Status is the displayed status, derived by the server.
	Status SessionStatus `json:"status"`
	// StartedAt is when the session was created (UTC, ms precision).
	StartedAt Timestamp `json:"started_at"`
	// LastSeenAt is when the server last heard from the session.
	LastSeenAt Timestamp `json:"last_seen_at"`
	// EndedAt is when the session ended; omitted while it has not.
	EndedAt *Timestamp `json:"ended_at,omitempty"`
	// Runtime names the agent runtime, as reported; agents only.
	Runtime string `json:"runtime,omitempty"`
	// Model names the model behind the agent, as reported; agents only.
	Model string `json:"model,omitempty"`
	// Host names the machine the agent runs on, as reported; agents only.
	Host string `json:"host,omitempty"`
	// Interventions lists the intervention kinds the session declared.
	Interventions []InterventionKind `json:"interventions"`
	// CurrentRun is the id of the open run; zero (omitted) when there is
	// none or the reader may not see it.
	CurrentRun int64 `json:"current_run,omitempty"`
	// Task is the current run's task, one line; present exactly when
	// CurrentRun is.
	Task string `json:"task,omitempty"`
	// OutcomeUnknown is whether the session is stale or offline with a run
	// open; always false while online.
	OutcomeUnknown bool `json:"outcome_unknown"`
}

// Validate reports whether the session is structurally well-formed: the v1
// schema name, positive ids, a known kind, state and status, a non-blank name
// and client within their limits, an interventions list that is a list (empty
// is fine, null is not), a one-line task, timestamps in order, and the
// cross-field rules below. It cannot check that the principal exists or that
// Status is the right derivation for the session's runs; those are the
// server's.
//
// A stale or offline state shows as that status and nothing else, and an
// online session shows as neither; this much of the status derivation never
// depends on runs, so a document breaking it is a bug, not a state. An ended
// session is offline. An online session never has an unknown outcome: its
// runs are being reported. A human session has no runtime, model or host,
// declares no interventions, has no run and so no task and no unknown
// outcome, and shows only its liveness. Task and CurrentRun come together or
// not at all.
func (s SessionV1) Validate() error {
	if s.Schema != SessionSchemaV1 {
		return fmt.Errorf("schema: session schema must be %q, got %q", SessionSchemaV1, s.Schema)
	}
	if s.ID <= 0 {
		return fmt.Errorf("schema: session id must be positive, got %d", s.ID)
	}
	if s.PrincipalID <= 0 {
		return fmt.Errorf("schema: session principal_id must be positive, got %d", s.PrincipalID)
	}
	if s.Kind != PrincipalHuman && s.Kind != PrincipalAgent {
		return fmt.Errorf("schema: session kind %q is not one of human, agent", s.Kind)
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("schema: session name is required")
	}
	// Length only, not ValidateDisplayName: the name is the principal's, and
	// names stored before the character rule existed are returned as they
	// are. Nothing stored is longer than the limit, which has always held.
	if n := utf8.RuneCountInString(s.Name); n > MaxPrincipalNameLength {
		return fmt.Errorf("schema: session name must be at most %d characters, got %d", MaxPrincipalNameLength, n)
	}
	if err := validateSessionLabels(s.Client, s.Runtime, s.Model, s.Host); err != nil {
		return err
	}
	if !s.State.Valid() {
		return fmt.Errorf("schema: session state %q is not one of online, stale, offline", s.State)
	}
	if !s.Status.Valid() {
		return fmt.Errorf("schema: session status %q is not one of online, idle, running, blocked, waiting, failed, stale, offline", s.Status)
	}
	if err := validateSessionStatus(s.State, s.Status); err != nil {
		return err
	}
	if s.OutcomeUnknown && s.State == SessionStateOnline {
		return errors.New("schema: online session must not report outcome_unknown")
	}
	if s.StartedAt.IsZero() {
		return errors.New("schema: session started_at is required")
	}
	if s.LastSeenAt.IsZero() {
		return errors.New("schema: session last_seen_at is required")
	}
	if s.LastSeenAt.Time().Before(s.StartedAt.Time()) {
		return errors.New("schema: session last_seen_at must not be before started_at")
	}
	if s.EndedAt != nil {
		if s.EndedAt.IsZero() {
			return errors.New("schema: session ended_at must be omitted or set")
		}
		if s.EndedAt.Time().Before(s.StartedAt.Time()) {
			return errors.New("schema: session ended_at must not be before started_at")
		}
		if s.State != SessionStateOffline {
			return fmt.Errorf("schema: session with ended_at must be offline, got state %q", s.State)
		}
	}
	if s.Interventions == nil {
		return errors.New("schema: session interventions must be a list")
	}
	if err := validateInterventionKinds(s.Interventions); err != nil {
		return err
	}
	if s.CurrentRun < 0 {
		return fmt.Errorf("schema: session current_run must be omitted or positive, got %d", s.CurrentRun)
	}
	if (s.CurrentRun > 0) != (s.Task != "") {
		return errors.New("schema: session current_run and task must be present together or omitted together")
	}
	if err := validateLine("session task", s.Task, MaxRunTaskLength); err != nil {
		return err
	}
	if s.Kind == PrincipalHuman {
		return validateHumanSession(s)
	}
	return nil
}

// validateSessionStatus checks the part of the status derivation that does
// not depend on runs: liveness wins. The run-dependent statuses are only ever
// shown on an online session.
func validateSessionStatus(state SessionState, status SessionStatus) error {
	switch state {
	case SessionStateStale:
		if status != SessionStatusStale {
			return fmt.Errorf("schema: stale session must have status %q, got %q", SessionStatusStale, status)
		}
	case SessionStateOffline:
		if status != SessionStatusOffline {
			return fmt.Errorf("schema: offline session must have status %q, got %q", SessionStatusOffline, status)
		}
	default:
		if status == SessionStatusStale || status == SessionStatusOffline {
			return fmt.Errorf("schema: online session must not have status %q", status)
		}
	}
	return nil
}

// validateHumanSession refuses everything only an agent session carries. A
// human has no runtime, model or host, declares no interventions and has no
// runs, so it has no current run, no task, no unknown outcome and no
// run-derived status.
func validateHumanSession(s SessionV1) error {
	if s.Runtime != "" || s.Model != "" || s.Host != "" {
		return errors.New("schema: human session must not carry runtime, model or host")
	}
	if len(s.Interventions) != 0 {
		return errors.New("schema: human session must not declare interventions")
	}
	if s.CurrentRun != 0 {
		return errors.New("schema: human session must not carry current_run or task")
	}
	if s.OutcomeUnknown {
		return errors.New("schema: human session must not report outcome_unknown")
	}
	switch s.Status {
	case SessionStatusOnline, SessionStatusStale, SessionStatusOffline:
		return nil
	default:
		return fmt.Errorf("schema: human session status must be one of online, stale, offline, got %q", s.Status)
	}
}

// validateSessionLabels checks the short labels a client reports about
// itself. Client is required; the other three are optional and agents-only
// (the caller checks which kind of session they are on). All four end up in
// rosters and other people's terminals, so they are held to the display-name
// rule: no control characters, no bidi overrides, no invisible marks.
func validateSessionLabels(client, runtime, model, host string) error {
	if strings.TrimSpace(client) == "" {
		return errors.New("schema: session client is required")
	}
	if err := ValidateDisplayName("session client", client, MaxSessionClientLength); err != nil {
		return err
	}
	for _, l := range []struct{ field, value string }{
		{"session runtime", runtime},
		{"session model", model},
		{"session host", host},
	} {
		if l.value == "" {
			continue
		}
		if err := ValidateDisplayName(l.field, l.value, MaxSessionLabelLength); err != nil {
			return err
		}
	}
	return nil
}

// validateInterventionKinds checks a declared-interventions list: every kind
// known, none listed twice. An empty or nil list is valid and declares none.
func validateInterventionKinds(kinds []InterventionKind) error {
	seen := make(map[InterventionKind]struct{}, len(kinds))
	for _, k := range kinds {
		if !k.Valid() {
			return fmt.Errorf("schema: intervention kind %q is not one of %s", k, interventionKindNames())
		}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("schema: interventions lists kind %q more than once", k)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// validateText checks a free-text field that may be empty: valid UTF-8 and at
// most max characters. Unlike ValidateDisplayName it allows newlines and
// interior formatting, because an outcome, an intervention body or a reason
// is prose, not a label. The error names the field and never echoes the text.
func validateText(field, s string, max int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("schema: %s must be valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(s); n > max {
		return fmt.Errorf("schema: %s must be at most %d characters, got %d", field, max, n)
	}
	return nil
}

// validateLine is validateText for a field that is one line: a run's task or
// an activity summary, which rosters and timelines show on a single row. A
// control character (newline, carriage return, tab, escape, NUL) would break
// the row or, in a terminal, rewrite it, so none is allowed. Everything else
// ValidateDisplayName refuses is allowed here: a task may need interior
// punctuation and spacing a label does not.
func validateLine(field, s string, max int) error {
	if err := validateText(field, s, max); err != nil {
		return err
	}
	if strings.ContainsFunc(s, unicode.IsControl) {
		return fmt.Errorf("schema: %s must be one line with no control characters", field)
	}
	return nil
}

// SessionRosterV1 is every session visible in one channel: the body of
// GET /v1/sessions?channel= and the snapshot frame the events socket sends on
// connect and on each change. A roster is the whole state, not a delta, so a
// reader that misses one loses nothing. Sessions is always a JSON array,
// ordered by id; a channel with nobody connected is an empty array.
//
// Each entry carries its own schema field, because the same SessionV1 also
// travels alone as a frame and a reader must be able to hand either form to
// the same decoder.
type SessionRosterV1 struct {
	// Schema is the document discriminator; always SessionRosterSchemaV1.
	Schema string `json:"schema"`
	// ChannelID references the channel the roster describes, by id as every
	// other document names its channel; the request named it by name.
	ChannelID int64 `json:"channel_id"`
	// Sessions lists every session the caller may see in the channel.
	Sessions []SessionV1 `json:"sessions"`
}

// Validate reports whether the roster is structurally well-formed: the v1
// schema name, a positive channel_id, a sessions list that is a list (empty
// is fine, null is not), every session well-formed and no session id listed
// twice. Order is not checked.
func (r SessionRosterV1) Validate() error {
	if r.Schema != SessionRosterSchemaV1 {
		return fmt.Errorf("schema: session roster schema must be %q, got %q", SessionRosterSchemaV1, r.Schema)
	}
	if r.ChannelID <= 0 {
		return fmt.Errorf("schema: session roster channel_id must be positive, got %d", r.ChannelID)
	}
	if r.Sessions == nil {
		return errors.New("schema: session roster sessions must be a list")
	}
	seen := make(map[int64]struct{}, len(r.Sessions))
	for _, s := range r.Sessions {
		if err := s.Validate(); err != nil {
			return err
		}
		if _, dup := seen[s.ID]; dup {
			return fmt.Errorf("schema: session roster lists session %d more than once", s.ID)
		}
		seen[s.ID] = struct{}{}
	}
	return nil
}

// CreateSessionRequestV1 is the request body of POST /v1/sessions. The
// principal and its kind come from the caller's credential and everything
// else (id, state, status, timestamps) from the server, so a request carries
// none of them. Like PostMessageRequestV1 this is an API shape, not a
// persisted or registered payload.
//
// Runtime, Model, Host and Interventions are for agents; a human sending any
// of them is refused by the server, which knows the caller's kind. The schema
// checks only what it can see.
type CreateSessionRequestV1 struct {
	// Client names the connecting program.
	Client string `json:"client"`
	// Runtime names the agent runtime; agents only.
	Runtime string `json:"runtime,omitempty"`
	// Model names the model behind the agent; agents only.
	Model string `json:"model,omitempty"`
	// Host names the machine the agent runs on; agents only.
	Host string `json:"host,omitempty"`
	// Interventions lists the intervention kinds the session will act on;
	// omitted or empty declares none.
	Interventions []InterventionKind `json:"interventions,omitempty"`
}

// Validate reports whether the request is structurally well-formed: a client
// and any labels within their limits and free of control characters, and a
// declared-interventions list of known, distinct kinds. Its rules are exactly
// SessionV1's for every caller-supplied field.
func (r CreateSessionRequestV1) Validate() error {
	if err := validateSessionLabels(r.Client, r.Runtime, r.Model, r.Host); err != nil {
		return err
	}
	return validateInterventionKinds(r.Interventions)
}
