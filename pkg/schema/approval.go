package schema

import (
	"errors"
	"fmt"
)

// ApprovalV1Name is the registered versioned name of the approval object — the
// first-class approval entity (approval-object.md §1), not a message subtype.
const ApprovalV1Name = "conch.approval.v1"

// EscalationTargetKind discriminates the two things an approval may escalate to
// when its deadline passes unresolved (approval-object.md §1, D7): another
// principal, or an ntfy topic.
type EscalationTargetKind string

// Escalation target kinds.
const (
	EscalationTargetPrincipal EscalationTargetKind = "principal"
	EscalationTargetNtfyTopic EscalationTargetKind = "ntfy_topic"
)

// EscalationTarget is who or what is notified (urgent) when an approval's
// deadline passes while still pending. Exactly one of PrincipalID / Topic is
// set, selected by Kind.
type EscalationTarget struct {
	// Kind selects which field carries the target.
	Kind EscalationTargetKind `json:"kind"`
	// PrincipalID is set when Kind is principal.
	PrincipalID int64 `json:"principal_id,omitempty"`
	// Topic is the ntfy topic set when Kind is ntfy_topic.
	Topic string `json:"topic,omitempty"`
}

// Validate reports whether the escalation target is well-formed: its kind is
// recognized and exactly the matching field is populated.
func (e EscalationTarget) Validate() error {
	switch e.Kind {
	case EscalationTargetPrincipal:
		if e.PrincipalID <= 0 {
			return errors.New("schema: principal escalation target requires principal_id")
		}
		if e.Topic != "" {
			return errors.New("schema: principal escalation target must not set topic")
		}
	case EscalationTargetNtfyTopic:
		if e.Topic == "" {
			return errors.New("schema: ntfy escalation target requires topic")
		}
		if e.PrincipalID != 0 {
			return errors.New("schema: ntfy escalation target must not set principal_id")
		}
	default:
		return fmt.Errorf("schema: escalation target kind %q is not one of principal, ntfy_topic", e.Kind)
	}
	return nil
}

// validateApprovalOptions enforces the option rules shared by ApprovalV1 and
// CreateApprovalRequestV1: a non-empty set of well-formed options with unique
// ids, including at least one approve and one reject option.
func validateApprovalOptions(options []Option) error {
	if len(options) == 0 {
		return errors.New("schema: approval requires at least one option")
	}
	seen := make(map[string]struct{}, len(options))
	var hasApprove, hasReject bool
	for i, o := range options {
		if err := o.Validate(); err != nil {
			return fmt.Errorf("schema: approval option %d: %w", i, err)
		}
		if _, dup := seen[o.ID]; dup {
			return fmt.Errorf("schema: approval option id %q is duplicated", o.ID)
		}
		seen[o.ID] = struct{}{}
		switch o.Kind {
		case OptionKindApprove:
			hasApprove = true
		case OptionKindReject:
			hasReject = true
		}
	}
	if !hasApprove || !hasReject {
		return errors.New("schema: approval options must include at least one approve and one reject option")
	}
	return nil
}

// ApprovalV1 is the wire representation of an approval object
// (approval-object.md §1). It is a first-class entity with its own lifecycle:
// a message may reference an approval to render it in a channel, but the
// approval does not live inside a message.
type ApprovalV1 struct {
	// ID is the server-assigned, opaque, unique approval id.
	ID int64 `json:"id"`
	// RequesterID references the principal (usually an agent) that created it.
	RequesterID int64 `json:"requester_id"`
	// ChannelID references the channel it is raised in (scopes who sees it).
	ChannelID int64 `json:"channel_id"`
	// Title is the short human-rendered heading (ntfy title, inbox row).
	Title string `json:"title"`
	// Body is the human-rendered detail.
	Body string `json:"body"`
	// Payload is the optional typed machine payload being approved (e.g. a
	// trade signal); nil when absent.
	Payload *Payload `json:"payload,omitempty"`
	// Options is the non-empty set of typed choices a decider may select.
	Options []Option `json:"options"`
	// Deadline is the absolute time the approval must be decided by. Required —
	// an approval that can wait forever is a bug in the requester.
	Deadline Timestamp `json:"deadline"`
	// Quorum is the number of concurring decisions required to resolve. At least 1.
	Quorum int `json:"quorum"`
	// EscalationTarget is notified (urgent) when the deadline passes unresolved;
	// nil when none is configured.
	EscalationTarget *EscalationTarget `json:"escalation_target,omitempty"`
	// State is the lifecycle state.
	State ApprovalState `json:"state"`
	// CreatedAt is when the server created the approval (UTC, ms precision).
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the approval object is structurally well-formed. It
// does not require the payload's schema to be registered (forward
// compatibility), only that the payload is itself well-formed.
func (a ApprovalV1) Validate() error {
	if a.ID <= 0 {
		return fmt.Errorf("schema: approval id must be positive, got %d", a.ID)
	}
	if a.RequesterID <= 0 {
		return errors.New("schema: approval requester_id must be positive")
	}
	if a.ChannelID <= 0 {
		return errors.New("schema: approval channel_id must be positive")
	}
	if a.Title == "" {
		return errors.New("schema: approval title is required")
	}
	if a.Body == "" {
		return errors.New("schema: approval body is required")
	}
	if err := validateApprovalOptions(a.Options); err != nil {
		return err
	}
	if a.Deadline.IsZero() {
		return errors.New("schema: approval deadline is required")
	}
	if a.Quorum < 1 {
		return fmt.Errorf("schema: approval quorum must be at least 1, got %d", a.Quorum)
	}
	if a.Payload != nil {
		if err := a.Payload.Validate(); err != nil {
			return err
		}
	}
	if a.EscalationTarget != nil {
		if err := a.EscalationTarget.Validate(); err != nil {
			return err
		}
	}
	if !a.State.Valid() {
		return fmt.Errorf("schema: approval state %q is not a recognized state", a.State)
	}
	if a.CreatedAt.IsZero() {
		return errors.New("schema: approval created_at is required")
	}
	return nil
}

func init() {
	RegisterPayload(PayloadSchema{
		Name: ApprovalV1Name,
		New:  func() any { return new(ApprovalV1) },
	})
}
