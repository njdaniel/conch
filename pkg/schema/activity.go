package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Agent activity wire shapes (issue #264; P3 epic #263). Activity is the
// timestamped stream of what a run did: each entry is one event of a closed
// kind with a one-line summary, and optionally a machine-readable Data body
// that is preserved byte for byte, as a message payload is, so an entry whose
// data this build does not understand survives a round trip unchanged.
//
// Activity inherits the visibility of its run (D8). An entry names the run
// and the agent so a client can file it without a second lookup, and nothing
// else: the channel is the run's.

// AgentActivitySchemaV1 is the versioned name identifying the v1 activity
// entry on the wire. Every AgentActivityV1 carries it, so the events socket's
// reader can tell an activity frame from a session or a run frame.
const AgentActivitySchemaV1 = "conch.agent.activity.v1"

// Limits on an activity entry.
const (
	// MaxActivitySummaryLength bounds Summary, in characters.
	MaxActivitySummaryLength = 512
	// MaxActivityOutcomeLength bounds Outcome, in characters.
	MaxActivityOutcomeLength = 512
	// MaxActivityDataBytes bounds Data, in bytes of its JSON encoding. It is
	// a size for a detail record, not a transport for files or transcripts.
	MaxActivityDataBytes = 8 << 10
)

// ActivityKind is what kind of event an activity entry records. The
// vocabulary is closed: a client renders each kind on purpose, and an entry
// of a kind it does not know must fail rather than render as something else.
type ActivityKind string

// Activity kinds.
const (
	// ActivityKindRunStarted: the run left queued.
	ActivityKindRunStarted ActivityKind = "run_started"
	// ActivityKindCommandStarted: the agent began a command or tool call.
	ActivityKindCommandStarted ActivityKind = "command_started"
	// ActivityKindCommandFinished: that command or tool call returned.
	ActivityKindCommandFinished ActivityKind = "command_finished"
	// ActivityKindReplyPosted: the agent posted a message.
	ActivityKindReplyPosted ActivityKind = "reply_posted"
	// ActivityKindApprovalRequested: the agent raised an approval.
	ActivityKindApprovalRequested ActivityKind = "approval_requested"
	// ActivityKindApprovalResolved: that approval was resolved.
	ActivityKindApprovalResolved ActivityKind = "approval_resolved"
	// ActivityKindInterventionReceived: the agent took an intervention.
	ActivityKindInterventionReceived ActivityKind = "intervention_received"
	// ActivityKindRunEnded: the run reached a terminal state.
	ActivityKindRunEnded ActivityKind = "run_ended"
)

// Valid reports whether k is a recognized activity kind.
func (k ActivityKind) Valid() bool {
	switch k {
	case ActivityKindRunStarted, ActivityKindCommandStarted, ActivityKindCommandFinished,
		ActivityKindReplyPosted, ActivityKindApprovalRequested, ActivityKindApprovalResolved,
		ActivityKindInterventionReceived, ActivityKindRunEnded:
		return true
	default:
		return false
	}
}

// activityKindNames renders the vocabulary for error messages.
func activityKindNames() string {
	return "run_started, command_started, command_finished, reply_posted, approval_requested, approval_resolved, intervention_received, run_ended"
}

// AgentActivityV1 is the v1 wire representation of one activity entry: an
// entry of an activity page and one kind of frame on the events socket.
//
// Data is the optional machine-readable detail, preserved verbatim. The
// schema checks that it is JSON and within MaxActivityDataBytes, never what
// it contains; its shape is the adapter's, and a client shows it as data.
type AgentActivityV1 struct {
	// Schema is the document discriminator; always AgentActivitySchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned entry id, increasing within a run.
	ID int64 `json:"id"`
	// Run references the run the entry belongs to.
	Run int64 `json:"run"`
	// Agent references the agent principal whose run it is.
	Agent int64 `json:"agent"`
	// Kind is what kind of event the entry records.
	Kind ActivityKind `json:"kind"`
	// Summary is the one-line human-readable form of the event; always
	// present.
	Summary string `json:"summary"`
	// Outcome is a short result of the event, where the kind has one (a
	// command's exit, an approval's decision); omitted otherwise.
	Outcome string `json:"outcome,omitempty"`
	// At is when the server recorded the entry (UTC, ms precision).
	At Timestamp `json:"at"`
	// Data is the optional machine-readable detail, preserved opaquely.
	Data json.RawMessage `json:"data,omitempty"`
}

// Validate reports whether the entry is structurally well-formed: the v1
// schema name, positive ids, a known kind, a non-blank summary and an outcome
// within their limits, at set, and data that is JSON within its size when
// present. It cannot check that the run exists or belongs to the agent.
func (a AgentActivityV1) Validate() error {
	if a.Schema != AgentActivitySchemaV1 {
		return fmt.Errorf("schema: agent activity schema must be %q, got %q", AgentActivitySchemaV1, a.Schema)
	}
	if a.ID <= 0 {
		return fmt.Errorf("schema: agent activity id must be positive, got %d", a.ID)
	}
	if a.Run <= 0 {
		return fmt.Errorf("schema: agent activity run must be positive, got %d", a.Run)
	}
	if a.Agent <= 0 {
		return fmt.Errorf("schema: agent activity agent must be positive, got %d", a.Agent)
	}
	if err := validateActivityBody(a.Kind, a.Summary, a.Outcome, a.Data); err != nil {
		return err
	}
	if a.At.IsZero() {
		return errors.New("schema: agent activity at is required")
	}
	return nil
}

// validateActivityBody checks the caller-supplied part of an entry, shared by
// AgentActivityV1 and AppendActivityRequestV1 so a request that is accepted is
// an entry that validates on read.
func validateActivityBody(kind ActivityKind, summary, outcome string, data json.RawMessage) error {
	if !kind.Valid() {
		return fmt.Errorf("schema: agent activity kind %q is not one of %s", kind, activityKindNames())
	}
	if strings.TrimSpace(summary) == "" {
		return errors.New("schema: agent activity summary is required")
	}
	if err := validateText("agent activity summary", summary, MaxActivitySummaryLength); err != nil {
		return err
	}
	if err := validateText("agent activity outcome", outcome, MaxActivityOutcomeLength); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxActivityDataBytes {
		return fmt.Errorf("schema: agent activity data must be at most %d bytes, got %d", MaxActivityDataBytes, len(data))
	}
	if !json.Valid(data) {
		return errors.New("schema: agent activity data is not valid JSON")
	}
	return nil
}

// AppendActivityRequestV1 is the request body of POST /v1/runs/{id}/activity.
// The run comes from the URL, the agent from the run's session, and the id
// and time from the server: the server's clock is the only one in the
// stream, so an adapter cannot back-date an entry. Like PostMessageRequestV1
// this is an API shape, not a persisted or registered payload.
type AppendActivityRequestV1 struct {
	// Kind is what kind of event the entry records.
	Kind ActivityKind `json:"kind"`
	// Summary is the one-line human-readable form of the event.
	Summary string `json:"summary"`
	// Outcome is a short result of the event; omitted when there is none.
	Outcome string `json:"outcome,omitempty"`
	// Data is the optional machine-readable detail, preserved opaquely.
	Data json.RawMessage `json:"data,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its rules
// are exactly AgentActivityV1's for every caller-supplied field.
func (r AppendActivityRequestV1) Validate() error {
	return validateActivityBody(r.Kind, r.Summary, r.Outcome, r.Data)
}

// ListActivityResponseV1 is one forward page of a run's activity
// (GET /v1/runs/{id}/activity?after=&limit=), oldest first. NextAfter is the
// entry id to pass as the next request's after parameter; zero (omitted)
// means there is no known subsequent page, as on a message page. Activity is
// always a JSON array; a run with no entries yet is an empty array.
type ListActivityResponseV1 struct {
	Activity  []AgentActivityV1 `json:"activity"`
	NextAfter int64             `json:"next_after,omitempty"`
}
