package schema

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Run wire shapes (issue #264; P3 epic #263, decision D8). A run is one piece
// of work an agent session does: it has a stable id, a task, a lifecycle of
// closed states with a fixed transition table, and an outcome the adapter
// reports and the server never invents.
//
// A run belongs to a channel and may answer one message in it. That pair is
// the run's audience: a run with a message_id is visible to exactly the
// readers who can see that message, one without to the channel's members.
// Activity and interventions inherit the run's visibility, which is why the
// run, not they, names the channel.

// RunSchemaV1 is the versioned name identifying the v1 run document on the
// wire. Every RunV1 carries it, so the events socket's reader can tell a run
// frame from a session or an activity frame.
const RunSchemaV1 = "conch.run.v1"

// Error codes returned by the run routes in Error.Code.
const (
	// ErrorCodeRunNotFound: no run the caller may see has that id (HTTP 404).
	// An invisible run gets the same answer as a missing one (D8).
	ErrorCodeRunNotFound = "run_not_found"
	// ErrorCodeInvalidTransition: the requested state is not reachable from
	// the run's current state under RunTransitionAllowed (HTTP 409).
	ErrorCodeInvalidTransition = "invalid_transition"
)

// Length limits on the free-text fields of a run, in characters.
const (
	// MaxRunTaskLength bounds Task, the one-line statement of the work.
	MaxRunTaskLength = 512
	// MaxRunLinkLength bounds Link, the optional https URL to the work.
	MaxRunLinkLength = 512
	// MaxRunOutcomeLength bounds Outcome, the adapter's closing statement.
	MaxRunOutcomeLength = 512
)

// RunState is where a run is in its lifecycle. The vocabulary is closed and
// the transitions between states are fixed (RunTransitionAllowed), so the
// server and every client agree on what can follow what.
type RunState string

// Run states. Completed, failed and cancelled are terminal: a run in one of
// them never changes state again.
const (
	// RunStateQueued: created and not yet picked up.
	RunStateQueued RunState = "queued"
	// RunStateStarting: picked up; the runtime is spinning up.
	RunStateStarting RunState = "starting"
	// RunStateRunning: doing the work.
	RunStateRunning RunState = "running"
	// RunStateBlocked: stopped on something outside the run, such as an
	// approval it requested.
	RunStateBlocked RunState = "blocked"
	// RunStateWaiting: stopped on input from a person.
	RunStateWaiting RunState = "waiting"
	// RunStateCompleted: finished as intended. Terminal.
	RunStateCompleted RunState = "completed"
	// RunStateFailed: finished with an error. Terminal.
	RunStateFailed RunState = "failed"
	// RunStateCancelled: stopped by a cancel intervention or by the adapter.
	// Terminal.
	RunStateCancelled RunState = "cancelled"
)

// Valid reports whether s is a recognized run state.
func (s RunState) Valid() bool {
	switch s {
	case RunStateQueued, RunStateStarting, RunStateRunning, RunStateBlocked,
		RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled:
		return true
	default:
		return false
	}
}

// RunStateTerminal reports whether s is one of the three states a run never
// leaves: completed, failed or cancelled. An unknown state is not terminal,
// and is not anything else either; callers validate first.
func RunStateTerminal(s RunState) bool {
	switch s {
	case RunStateCompleted, RunStateFailed, RunStateCancelled:
		return true
	default:
		return false
	}
}

// runTransitions is the run lifecycle: from each state, the states it may
// move to. A state absent from the map (the three terminal states) has no
// exits. Anything not listed is illegal, including staying put: a transition
// to the current state is not a transition. This is the only copy of the
// table; the server enforces it and clients use it to grey out controls.
var runTransitions = map[RunState][]RunState{
	RunStateQueued:   {RunStateStarting, RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateStarting: {RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateRunning:  {RunStateBlocked, RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled},
	RunStateBlocked:  {RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateWaiting:  {RunStateRunning, RunStateCompleted, RunStateFailed, RunStateCancelled},
}

// RunTransitionAllowed reports whether a run in state from may move to state
// to. It is false for any pair not in the lifecycle table, for every pair
// leaving a terminal state, for from == to, and for any unknown state.
func RunTransitionAllowed(from, to RunState) bool {
	for _, next := range runTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// runStateNames renders the vocabulary for error messages.
func runStateNames() string {
	return "queued, starting, running, blocked, waiting, completed, failed, cancelled"
}

// RunV1 is the v1 wire representation of one run: an entry of a runs list
// and one kind of frame on the events socket.
//
// ChannelID references the channel the run belongs to, by id as every other
// document (a message, a net, presence) names its channel. MessageID is the
// message the run answers, which must be a message in that channel (the
// server checks; the schema cannot). Together they decide who may see the
// run (D8). The create request names the channel the way a caller does, by
// name; the server resolves it and the stored run carries the id.
//
// EndedAt is present exactly when State is terminal: the server stamps it on
// the transition and never before. Outcome is whatever the adapter said when
// it changed the state, kept short; the activity stream holds the detail.
type RunV1 struct {
	// Schema is the document discriminator; always RunSchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned run id, stable for the run's life.
	ID int64 `json:"id"`
	// SessionID references the session doing the work.
	SessionID int64 `json:"session_id"`
	// PrincipalID references the agent principal the session belongs to.
	PrincipalID int64 `json:"principal_id"`
	// ChannelID references the channel the run belongs to.
	ChannelID int64 `json:"channel_id"`
	// MessageID references the message the run answers; zero (omitted) for
	// a run that answers no message and is visible to the whole channel.
	MessageID int64 `json:"message_id,omitempty"`
	// State is where the run is in its lifecycle.
	State RunState `json:"state"`
	// Task is the one-line statement of the work: no control characters.
	Task string `json:"task"`
	// Link is an optional https URL to the work (a pull request, a log).
	Link string `json:"link,omitempty"`
	// StartedAt is when the run was created (UTC, ms precision).
	StartedAt Timestamp `json:"started_at"`
	// EndedAt is when the run reached a terminal state; omitted before.
	EndedAt *Timestamp `json:"ended_at,omitempty"`
	// Outcome is the adapter's closing statement; omitted when it gave none.
	Outcome string `json:"outcome,omitempty"`
}

// Validate reports whether the run is structurally well-formed: the v1 schema
// name, positive ids including channel_id, a known state, a one-line task
// within its limit, a link that is an https URL when present, started_at set, and
// ended_at present exactly when the state is terminal and not before
// started_at. It cannot check that the session, channel or message exist or
// that the message is in the channel; those are the server's.
func (r RunV1) Validate() error {
	if r.Schema != RunSchemaV1 {
		return fmt.Errorf("schema: run schema must be %q, got %q", RunSchemaV1, r.Schema)
	}
	if r.ID <= 0 {
		return fmt.Errorf("schema: run id must be positive, got %d", r.ID)
	}
	if r.SessionID <= 0 {
		return fmt.Errorf("schema: run session_id must be positive, got %d", r.SessionID)
	}
	if r.PrincipalID <= 0 {
		return fmt.Errorf("schema: run principal_id must be positive, got %d", r.PrincipalID)
	}
	if r.ChannelID <= 0 {
		return fmt.Errorf("schema: run channel_id must be positive, got %d", r.ChannelID)
	}
	if err := validateRunWork(r.MessageID, r.Task, r.Link); err != nil {
		return err
	}
	if !r.State.Valid() {
		return fmt.Errorf("schema: run state %q is not one of %s", r.State, runStateNames())
	}
	if r.StartedAt.IsZero() {
		return errors.New("schema: run started_at is required")
	}
	switch terminal := RunStateTerminal(r.State); {
	case terminal && (r.EndedAt == nil || r.EndedAt.IsZero()):
		return fmt.Errorf("schema: run in state %q must carry ended_at", r.State)
	case !terminal && r.EndedAt != nil:
		return fmt.Errorf("schema: run in state %q must not carry ended_at", r.State)
	case terminal && r.EndedAt.Time().Before(r.StartedAt.Time()):
		return errors.New("schema: run ended_at must not be before started_at")
	}
	return validateText("run outcome", r.Outcome, MaxRunOutcomeLength)
}

// validateRunWork checks the caller-supplied description of the work, shared
// by RunV1 and CreateRunRequestV1 so a request that is accepted is a run that
// validates on read. The channel is not here: the request names it and the
// stored run carries its id, so each type checks its own.
func validateRunWork(messageID int64, task, link string) error {
	if messageID < 0 {
		return fmt.Errorf("schema: run message_id must be omitted or positive, got %d", messageID)
	}
	if strings.TrimSpace(task) == "" {
		return errors.New("schema: run task is required")
	}
	if err := validateLine("run task", task, MaxRunTaskLength); err != nil {
		return err
	}
	return validateRunLink(link)
}

// validateRunLink checks an optional link: within its length; one token with
// no whitespace, control character, bidi control or other invisible
// formatting character (the classes ValidateDisplayName refuses, with no
// exceptions, since a URL has no use for a joiner or a directional mark); and
// an absolute https URL with a host and no userinfo (the scheme is
// case-insensitive, as in RFC 3986). Nothing else is accepted, because a
// client renders the link as something to open and a reader should not be
// handed an http, file or javascript URL by an agent, nor one whose
// "user@" part reads as the host ("https://github.com@evil.example/"). The
// whitespace rule is explicit because net/url is lenient about a space in a
// path. The error never echoes the link.
func validateRunLink(link string) error {
	if link == "" {
		return nil
	}
	if !utf8.ValidString(link) {
		return errors.New("schema: run link must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(link); n > MaxRunLinkLength {
		return fmt.Errorf("schema: run link must be at most %d characters, got %d", MaxRunLinkLength, n)
	}
	if strings.ContainsFunc(link, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("schema: run link must not contain whitespace or control characters")
	}
	if strings.ContainsFunc(link, func(r rune) bool { return isStrongBidiControl(r) || unicode.Is(unicode.Cf, r) }) {
		return errors.New("schema: run link must not contain bidi controls or invisible formatting characters")
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("schema: run link must be an https URL")
	}
	if u.User != nil {
		return errors.New("schema: run link must not carry userinfo")
	}
	return nil
}

// CreateRunRequestV1 is the request body of POST /v1/sessions/{id}/runs. The
// session comes from the URL, the principal from the session, and the id,
// state (queued) and timestamps from the server; a request carries none of
// them. Like PostMessageRequestV1 this is an API shape, not a persisted or
// registered payload.
//
// Channel is the channel's name, as a caller names a channel in every route
// and query parameter; the server resolves it to the id the stored RunV1
// carries, and an unknown or unreadable name gets the channel-not-found
// refusal.
type CreateRunRequestV1 struct {
	// Channel is the name of the channel the run belongs to.
	Channel string `json:"channel"`
	// MessageID references the message the run answers; zero (omitted) for
	// a run visible to the whole channel.
	MessageID int64 `json:"message_id,omitempty"`
	// Task is the one-line statement of the work.
	Task string `json:"task"`
	// Link is an optional https URL to the work.
	Link string `json:"link,omitempty"`
}

// Validate reports whether the request is structurally well-formed: a
// non-blank channel name, and exactly RunV1's rules for the message, task and
// link. The name is not held to the display-name rule because names stored
// before that rule existed keep working.
func (r CreateRunRequestV1) Validate() error {
	if strings.TrimSpace(r.Channel) == "" {
		return errors.New("schema: run channel is required")
	}
	return validateRunWork(r.MessageID, r.Task, r.Link)
}

// SetRunStateRequestV1 is the request body of the route that moves a run to a
// new state (POST /v1/runs/{id}/state). The schema checks that the state is
// one of the vocabulary; whether it is reachable from the run's current state
// is the server's to decide with RunTransitionAllowed, because only the
// server knows the current state.
type SetRunStateRequestV1 struct {
	// State is the state to move the run to.
	State RunState `json:"state"`
	// Outcome is the adapter's statement about the change; omitted when it
	// has none.
	Outcome string `json:"outcome,omitempty"`
}

// Validate reports whether the request is structurally well-formed: a known
// state and an outcome within its limit.
func (r SetRunStateRequestV1) Validate() error {
	if !r.State.Valid() {
		return fmt.Errorf("schema: run state %q is not one of %s", r.State, runStateNames())
	}
	return validateText("run outcome", r.Outcome, MaxRunOutcomeLength)
}

// ListRunsResponseV1 is one forward page of the runs the caller may see
// (GET /v1/runs?channel=&session=&after=&limit=), ordered by id. NextAfter is
// the run id to pass as the next request's after parameter; zero (omitted)
// means there is no known subsequent page, as on a message page. Runs is
// always a JSON array; a channel with no runs is an empty array. The type has
// no Validate, so that is the server's promise: it always sends an array,
// never null.
type ListRunsResponseV1 struct {
	Runs      []RunV1 `json:"runs"`
	NextAfter int64   `json:"next_after,omitempty"`
}
