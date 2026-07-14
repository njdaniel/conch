package schema

import (
	"errors"
	"fmt"
)

// ApprovalResolutionV1Name is the registered versioned name of the resolution
// event — the single shape shared by await_decision, check_decision, and the
// audit log (approval-object.md §3). One shape, one source of truth.
const ApprovalResolutionV1Name = "approval.resolution.v1"

// Outcome is the terminal verdict of an approval. approved, rejected, and
// custom mirror the kind of the selected Option; expired is produced when the
// final deadline passes with no quorum (approval-object.md §3), so waiters
// always receive a definitive answer.
type Outcome string

// Approval outcomes.
const (
	OutcomeApproved Outcome = "approved"
	OutcomeRejected Outcome = "rejected"
	OutcomeCustom   Outcome = "custom"
	OutcomeExpired  Outcome = "expired"
)

// Valid reports whether o is a recognized outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeApproved, OutcomeRejected, OutcomeCustom, OutcomeExpired:
		return true
	default:
		return false
	}
}

// ApprovalResolutionV1 is the canonical resolution event (approval.resolution.v1,
// approval-object.md §3). The server emits exactly one per approval when quorum
// is met or the approval expires. It is what waiters receive, what the audit
// log stores, and what check_decision returns.
//
// For an expired outcome, OptionID is absent and Decisions may be empty (no
// quorum was met); for every other outcome OptionID identifies the winning
// option and Decisions carries at least the resolving decision. Decisions
// records every concurring and dissenting decision.
type ApprovalResolutionV1 struct {
	// ApprovalID references the approval this resolves.
	ApprovalID int64 `json:"approval_id"`
	// Outcome is the terminal verdict.
	Outcome Outcome `json:"outcome"`
	// OptionID is the winning option's id; absent (empty) for an expired outcome.
	OptionID string `json:"option_id,omitempty"`
	// Decisions is every decision cast, concurring and dissenting. Present
	// (possibly empty) for every resolution.
	Decisions []Decision `json:"decisions"`
	// ResolvedAt is when the approval reached this terminal state (UTC, ms).
	ResolvedAt Timestamp `json:"resolved_at"`
}

// Validate reports whether the resolution event is structurally well-formed.
func (r ApprovalResolutionV1) Validate() error {
	if r.ApprovalID <= 0 {
		return errors.New("schema: resolution approval_id must be positive")
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("schema: resolution outcome %q is not one of approved, rejected, custom, expired", r.Outcome)
	}
	if r.ResolvedAt.IsZero() {
		return errors.New("schema: resolution resolved_at is required")
	}
	if r.Outcome == OutcomeExpired {
		if r.OptionID != "" {
			return errors.New("schema: expired resolution must not carry an option_id")
		}
	} else {
		if r.OptionID == "" {
			return errors.New("schema: non-expired resolution requires an option_id")
		}
		if len(r.Decisions) == 0 {
			return errors.New("schema: non-expired resolution requires at least one decision")
		}
	}
	for i, d := range r.Decisions {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("schema: resolution decision %d: %w", i, err)
		}
	}
	return nil
}

func init() {
	RegisterPayload(PayloadSchema{
		Name: ApprovalResolutionV1Name,
		New:  func() any { return new(ApprovalResolutionV1) },
	})
}
