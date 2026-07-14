package schema

import (
	"errors"
	"fmt"
)

// OptionKind is the typed classification of an approval option. Options are
// typed, not free text (approval-object.md §1): every option declares whether
// selecting it approves, rejects, or is a custom variant (e.g. "approve with
// size X"). The kind is what drives the resolution Outcome.
type OptionKind string

// Option kinds.
const (
	OptionKindApprove OptionKind = "approve"
	OptionKindReject  OptionKind = "reject"
	OptionKindCustom  OptionKind = "custom"
)

// Valid reports whether k is a recognized option kind.
func (k OptionKind) Valid() bool {
	switch k {
	case OptionKindApprove, OptionKindReject, OptionKindCustom:
		return true
	default:
		return false
	}
}

// Option is one typed choice a decider may select on an approval
// (approval-object.md §1). Options are the only way to decide an approval:
// there is no free-text verdict. ID is stable within an approval and is what a
// Decision and the resolution event reference.
type Option struct {
	// ID is the option's identifier, unique within its approval.
	ID string `json:"id"`
	// Label is the human-rendered choice text (TUI, ntfy).
	Label string `json:"label"`
	// Kind classifies the option as approve, reject, or custom.
	Kind OptionKind `json:"kind"`
}

// Validate reports whether the option is structurally well-formed.
func (o Option) Validate() error {
	if o.ID == "" {
		return errors.New("schema: option id is required")
	}
	if o.Label == "" {
		return errors.New("schema: option label is required")
	}
	if !o.Kind.Valid() {
		return fmt.Errorf("schema: option kind %q is not one of approve, reject, custom", o.Kind)
	}
	return nil
}
