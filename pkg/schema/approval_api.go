package schema

import (
	"errors"
	"fmt"
)

// CreateApprovalRequestV1 is the request body for raising a v1 approval: the
// caller-supplied fields of an ApprovalV1. The server assigns the id, state,
// creation timestamp, and (when Quorum is 0) the default quorum of 1. Like
// PostMessageRequestV1 this is an API shape, not a persisted or registered
// payload — it is not entered in the payload registry.
type CreateApprovalRequestV1 struct {
	// RequesterID references the principal (usually an agent) raising it.
	RequesterID int64 `json:"requester_id"`
	// ChannelID references the channel the approval is raised in.
	ChannelID int64 `json:"channel_id"`
	// Title is the short human-rendered heading (ntfy title, inbox row).
	Title string `json:"title"`
	// Body is the human-rendered detail.
	Body string `json:"body"`
	// Payload is the optional typed machine payload being approved.
	Payload *Payload `json:"payload,omitempty"`
	// Options is the non-empty set of typed choices a decider may select; it
	// must include at least one approve and one reject option.
	Options []Option `json:"options"`
	// Deadline is the absolute time the approval must be decided by. Required.
	Deadline Timestamp `json:"deadline"`
	// Quorum is the number of concurring decisions required to resolve.
	// Omitted or 0 means the server default of 1.
	Quorum int `json:"quorum,omitempty"`
	// EscalationTarget is notified (urgent) when the deadline passes
	// unresolved; nil when none is configured.
	EscalationTarget *EscalationTarget `json:"escalation_target,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its rules
// mirror ApprovalV1's for every caller-supplied field; server-assigned fields
// have no counterpart here.
func (r CreateApprovalRequestV1) Validate() error {
	if r.RequesterID <= 0 {
		return errors.New("schema: create approval requester_id must be positive")
	}
	if r.ChannelID <= 0 {
		return errors.New("schema: create approval channel_id must be positive")
	}
	if r.Title == "" {
		return errors.New("schema: create approval title is required")
	}
	if r.Body == "" {
		return errors.New("schema: create approval body is required")
	}
	if err := validateApprovalOptions(r.Options); err != nil {
		return err
	}
	if r.Deadline.IsZero() {
		return errors.New("schema: create approval deadline is required")
	}
	if r.Quorum < 0 {
		return fmt.Errorf("schema: create approval quorum must not be negative, got %d", r.Quorum)
	}
	if r.Payload != nil {
		if err := r.Payload.Validate(); err != nil {
			return err
		}
	}
	if r.EscalationTarget != nil {
		if err := r.EscalationTarget.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// CreateApprovalResponseV1 is the response body after an approval is created.
// It embeds the full ApprovalV1 the server assigned.
type CreateApprovalResponseV1 struct {
	Approval ApprovalV1 `json:"approval"`
}

// ListApprovalsResponseV1 is the response body listing approvals — the open
// (pending or escalated) approvals endpoint returns it, and any future
// filtered listing reuses it.
type ListApprovalsResponseV1 struct {
	Approvals []ApprovalV1 `json:"approvals"`
}

// CastDecisionRequestV1 is the request body for casting one human principal's
// decision on an approval. The reason is required at the schema layer, not
// merely by the CLI (approval-object.md §3).
type CastDecisionRequestV1 struct {
	// PrincipalID references the human principal casting the decision.
	PrincipalID int64 `json:"principal_id"`
	// OptionID is the id of the approval Option being selected.
	OptionID string `json:"option_id"`
	// Reason is the required free-text justification.
	Reason string `json:"reason"`
}

// Validate reports whether the request is structurally well-formed.
func (r CastDecisionRequestV1) Validate() error {
	if r.PrincipalID <= 0 {
		return errors.New("schema: cast decision principal_id must be positive")
	}
	if r.OptionID == "" {
		return errors.New("schema: cast decision option_id is required")
	}
	if r.Reason == "" {
		return errors.New("schema: cast decision reason is required")
	}
	return nil
}

// CastDecisionResponseV1 is the response body after a decision is recorded:
// the decision as stored, the approval's state after it, and — exactly when
// this decision met quorum — the resolution event, identical to what every
// other reader of the shared resolution store sees.
type CastDecisionResponseV1 struct {
	Decision Decision `json:"decision"`
	// State is the approval's state after this decision.
	State ApprovalState `json:"state"`
	// Resolution is present only when this decision resolved the approval.
	Resolution *ApprovalResolutionV1 `json:"resolution,omitempty"`
}
