package schema

import (
	"errors"
	"fmt"
	"strings"
)

// Intervention wire shapes (issue #264; P3 epic #263, decision D10). An
// intervention is a person asking a run to do something: take a message,
// cancel, pause or resume. The server records the request and what the
// adapter said about it, and nothing more: an intervention is applied when
// the adapter reports it applied, never because a button was pressed (UX-19).
//
// A session declares the kinds it will act on (SessionV1.Interventions); a
// request for any other kind is refused at once as unsupported. Who may
// intervene is whoever can see the run (D8, D10); interventions inherit the
// run's visibility.

// InterventionSchemaV1 is the versioned name identifying the v1 intervention
// document on the wire. Every InterventionV1 carries it, so the events
// socket's reader can tell an intervention frame from the others.
const InterventionSchemaV1 = "conch.intervention.v1"

// Error codes returned by the intervention routes in Error.Code.
const (
	// ErrorCodeInterventionNotFound: no intervention the caller may see has
	// that id (HTTP 404). An invisible one gets the same answer.
	ErrorCodeInterventionNotFound = "intervention_not_found"
	// ErrorCodeInterventionUnsupported: the run's session did not declare
	// the requested kind, so nothing would act on it (HTTP 409).
	ErrorCodeInterventionUnsupported = "intervention_unsupported"
	// ErrorCodeTooManyInterventions: the run already has as many
	// interventions waiting for an answer as the server allows (HTTP 429).
	ErrorCodeTooManyInterventions = "too_many_interventions"
	// ErrorCodeHumanRequired: only a person may do this, and an agent asked
	// (HTTP 403). The approval routes have answered with this code since P1.
	ErrorCodeHumanRequired = "human_required"
)

// Length limits on the free-text fields of an intervention, in characters.
const (
	// MaxInterventionBodyLength bounds Body, the text of a message
	// intervention.
	MaxInterventionBodyLength = 4000
	// MaxInterventionReasonLength bounds Reason, the adapter's explanation
	// in its answer.
	MaxInterventionReasonLength = 512
)

// InterventionKind is what the person asks the run to do. The vocabulary is
// closed: an adapter declares the kinds it understands from this list, and a
// kind outside it fails validation.
type InterventionKind string

// Intervention kinds.
const (
	// InterventionKindMessage: deliver Body to the run as input.
	InterventionKindMessage InterventionKind = "message"
	// InterventionKindCancel: stop the run.
	InterventionKindCancel InterventionKind = "cancel"
	// InterventionKindPause: hold the run where it is.
	InterventionKindPause InterventionKind = "pause"
	// InterventionKindResume: let a paused run continue.
	InterventionKindResume InterventionKind = "resume"
)

// Valid reports whether k is a recognized intervention kind.
func (k InterventionKind) Valid() bool {
	switch k {
	case InterventionKindMessage, InterventionKindCancel, InterventionKindPause, InterventionKindResume:
		return true
	default:
		return false
	}
}

// interventionKindNames renders the vocabulary for error messages.
func interventionKindNames() string {
	return "message, cancel, pause, resume"
}

// InterventionState is where an intervention is between the request and the
// adapter's answer. The vocabulary is closed. Requested is the only state the
// server sets on its own; the other three are the adapter's answers.
type InterventionState string

// Intervention states.
const (
	// InterventionStateRequested: recorded; the adapter has not answered.
	InterventionStateRequested InterventionState = "requested"
	// InterventionStateAcknowledged: the adapter has seen it and is acting.
	InterventionStateAcknowledged InterventionState = "acknowledged"
	// InterventionStateApplied: the adapter did what was asked.
	InterventionStateApplied InterventionState = "applied"
	// InterventionStateFailed: the adapter could not do it; Reason says why.
	InterventionStateFailed InterventionState = "failed"
)

// Valid reports whether s is a recognized intervention state.
func (s InterventionState) Valid() bool {
	switch s {
	case InterventionStateRequested, InterventionStateAcknowledged, InterventionStateApplied, InterventionStateFailed:
		return true
	default:
		return false
	}
}

// interventionStateNames renders the vocabulary for error messages.
func interventionStateNames() string {
	return "requested, acknowledged, applied, failed"
}

// InterventionV1 is the v1 wire representation of one intervention: an entry
// of an interventions list and one kind of frame on the events socket.
//
// AnsweredAt and Reason belong to the adapter's answer: AnsweredAt is present
// exactly when the state is no longer requested, and a Reason is only ever
// part of an answer. Body belongs to a message intervention, which is nothing
// without one; cancel, pause and resume take none (#287), so a body on them
// is refused rather than carried where nothing reads it.
type InterventionV1 struct {
	// Schema is the document discriminator; always InterventionSchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned intervention id.
	ID int64 `json:"id"`
	// RunID references the run the intervention is for.
	RunID int64 `json:"run_id"`
	// Kind is what the person asked the run to do.
	Kind InterventionKind `json:"kind"`
	// Body is the text of a message intervention; omitted on every other
	// kind.
	Body string `json:"body,omitempty"`
	// State is where the intervention is between request and answer.
	State InterventionState `json:"state"`
	// RequestedBy references the human principal who asked.
	RequestedBy int64 `json:"requested_by"`
	// RequestedAt is when the server recorded the request (UTC, ms).
	RequestedAt Timestamp `json:"requested_at"`
	// AnsweredAt is when the adapter last answered; omitted while requested.
	AnsweredAt *Timestamp `json:"answered_at,omitempty"`
	// Reason is the adapter's explanation, usually of a failure; omitted when
	// it gave none.
	Reason string `json:"reason,omitempty"`
}

// Validate reports whether the intervention is structurally well-formed: the
// v1 schema name, positive ids, a known kind and state, a body that is
// present for a message, absent for every other kind and within its limit,
// requested_at set, answered_at
// present exactly when the state is not requested and not before
// requested_at, and a reason only on an answer and within its limit. It
// cannot check that the run exists, that the requester may see it, or that
// the session declared the kind; those are the server's.
func (i InterventionV1) Validate() error {
	if i.Schema != InterventionSchemaV1 {
		return fmt.Errorf("schema: intervention schema must be %q, got %q", InterventionSchemaV1, i.Schema)
	}
	if i.ID <= 0 {
		return fmt.Errorf("schema: intervention id must be positive, got %d", i.ID)
	}
	if i.RunID <= 0 {
		return fmt.Errorf("schema: intervention run_id must be positive, got %d", i.RunID)
	}
	if err := validateInterventionAsk(i.Kind, i.Body); err != nil {
		return err
	}
	if !i.State.Valid() {
		return fmt.Errorf("schema: intervention state %q is not one of %s", i.State, interventionStateNames())
	}
	if i.RequestedBy <= 0 {
		return fmt.Errorf("schema: intervention requested_by must be positive, got %d", i.RequestedBy)
	}
	if i.RequestedAt.IsZero() {
		return errors.New("schema: intervention requested_at is required")
	}
	answered := i.State != InterventionStateRequested
	switch {
	case answered && (i.AnsweredAt == nil || i.AnsweredAt.IsZero()):
		return fmt.Errorf("schema: intervention in state %q must carry answered_at", i.State)
	case !answered && i.AnsweredAt != nil:
		return errors.New("schema: requested intervention must not carry answered_at")
	case answered && i.AnsweredAt.Time().Before(i.RequestedAt.Time()):
		return errors.New("schema: intervention answered_at must not be before requested_at")
	}
	if !answered && i.Reason != "" {
		return errors.New("schema: requested intervention must not carry reason")
	}
	return validateInterventionReason(i.State, i.Reason)
}

// validateInterventionReason checks an answer's reason, shared by
// InterventionV1 and AckInterventionRequestV1: a failed answer must say why
// (a reader is told the run did not do what was asked, and "it failed" alone
// is nothing to act on), and any reason is within its limit.
func validateInterventionReason(state InterventionState, reason string) error {
	if state == InterventionStateFailed && strings.TrimSpace(reason) == "" {
		return errors.New("schema: failed intervention must carry reason")
	}
	return validateText("intervention reason", reason, MaxInterventionReasonLength)
}

// validateInterventionAsk checks the person's side of an intervention, shared
// by InterventionV1 and CreateInterventionRequestV1: a known kind, and a body
// that a message intervention has, that no other kind has, and that is within
// its limit.
func validateInterventionAsk(kind InterventionKind, body string) error {
	if !kind.Valid() {
		return fmt.Errorf("schema: intervention kind %q is not one of %s", kind, interventionKindNames())
	}
	if kind == InterventionKindMessage && strings.TrimSpace(body) == "" {
		return errors.New("schema: message intervention body is required")
	}
	if kind != InterventionKindMessage && body != "" {
		return fmt.Errorf("schema: %s intervention must not carry body", kind)
	}
	return validateText("intervention body", body, MaxInterventionBodyLength)
}

// CreateInterventionRequestV1 is the request body of
// POST /v1/runs/{id}/interventions. The run comes from the URL, the requester
// from the caller's credential, and the id, state (requested) and time from
// the server; a request carries none of them. Like PostMessageRequestV1 this
// is an API shape, not a persisted or registered payload.
type CreateInterventionRequestV1 struct {
	// Kind is what the person asks the run to do.
	Kind InterventionKind `json:"kind"`
	// Body is the text of a message intervention; refused on the others.
	Body string `json:"body,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its rules
// are exactly InterventionV1's for every caller-supplied field. Whether the
// run's session declared the kind is the server's to check.
func (r CreateInterventionRequestV1) Validate() error {
	return validateInterventionAsk(r.Kind, r.Body)
}

// AckInterventionRequestV1 is the request body of
// POST /v1/interventions/{id}/ack: the adapter's answer. The server stamps
// answered_at, so the body is the new state and an optional reason. Requested
// is not an answer and is refused here, not by the server's transition rules.
type AckInterventionRequestV1 struct {
	// State is the adapter's answer: acknowledged, applied or failed.
	State InterventionState `json:"state"`
	// Reason is the adapter's explanation; omitted when it has none.
	Reason string `json:"reason,omitempty"`
}

// Validate reports whether the request is structurally well-formed: an answer
// state, a reason that a failed answer has, and a reason within its limit.
func (r AckInterventionRequestV1) Validate() error {
	if !r.State.Valid() {
		return fmt.Errorf("schema: intervention state %q is not one of %s", r.State, interventionStateNames())
	}
	if r.State == InterventionStateRequested {
		return errors.New("schema: intervention ack state must be one of acknowledged, applied, failed")
	}
	return validateInterventionReason(r.State, r.Reason)
}

// ListInterventionsResponseV1 is the body of GET /v1/runs/{id}/interventions
// and of GET /v1/sessions/{id}/interventions?state=: the matching
// interventions the caller may see, ordered by id. Interventions is always a
// JSON array; none matching is an empty array. The type has no Validate, so
// that is the server's promise: it always sends an array, never null.
type ListInterventionsResponseV1 struct {
	Interventions []InterventionV1 `json:"interventions"`
}
