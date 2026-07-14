package schema

// ApprovalState is the lifecycle state of an approval object
// (approval-object.md §2). An approval is created pending; it either reaches
// quorum (resolved) or its deadline passes (escalated, then expired if still
// undecided). resolved and expired are terminal.
type ApprovalState string

// Approval states.
const (
	// ApprovalStatePending is open for decisions.
	ApprovalStatePending ApprovalState = "pending"
	// ApprovalStateEscalated means the deadline passed while pending and the
	// urgent escalation notification fired. Still decidable — escalation is a
	// notification/priority state, not a verdict.
	ApprovalStateEscalated ApprovalState = "escalated"
	// ApprovalStateResolved means quorum was met. Terminal.
	ApprovalStateResolved ApprovalState = "resolved"
	// ApprovalStateExpired means the final deadline passed with no quorum.
	// Terminal; carries a resolution event with outcome expired.
	ApprovalStateExpired ApprovalState = "expired"
)

// Valid reports whether s is a recognized approval state.
func (s ApprovalState) Valid() bool {
	switch s {
	case ApprovalStatePending, ApprovalStateEscalated, ApprovalStateResolved, ApprovalStateExpired:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether s is a terminal state. Terminal states never
// transition; a decision against a terminal approval is a protocol error
// (approval-object.md §2). Only resolved and expired are terminal.
func (s ApprovalState) IsTerminal() bool {
	return s == ApprovalStateResolved || s == ApprovalStateExpired
}
