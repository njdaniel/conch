package schema

import "errors"

// Decision is a single verdict cast by a human principal on an approval
// (approval-object.md §3). Decisions are cast only by humans via conch, never
// by agents on their own approvals. Each decision selects a typed Option by id
// and MUST carry a free-text reason; an empty reason is rejected at the schema
// layer, not merely by the CLI.
//
// Decision is the element type of a resolution event's Decisions list, which
// records every concurring and dissenting decision.
type Decision struct {
	// PrincipalID references the human principal that cast the decision.
	PrincipalID int64 `json:"principal_id"`
	// OptionID is the id of the Option the principal selected.
	OptionID string `json:"option_id"`
	// Reason is the required free-text justification. Never empty.
	Reason string `json:"reason"`
	// At is when the decision was cast (UTC, ms precision).
	At Timestamp `json:"at"`
}

// Validate reports whether the decision is structurally well-formed. An empty
// reason is a validation error (approval-object.md §3).
func (d Decision) Validate() error {
	if d.PrincipalID <= 0 {
		return errors.New("schema: decision principal_id must be positive")
	}
	if d.OptionID == "" {
		return errors.New("schema: decision option_id is required")
	}
	if d.Reason == "" {
		return errors.New("schema: decision reason is required")
	}
	if d.At.IsZero() {
		return errors.New("schema: decision at is required")
	}
	return nil
}
