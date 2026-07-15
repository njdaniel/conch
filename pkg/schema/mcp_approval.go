package schema

// RequestApprovalOutput is the concise MCP result after creating an approval.
type RequestApprovalOutput struct {
	ID       int64         `json:"id"`
	Title    string        `json:"title"`
	State    ApprovalState `json:"state"`
	Deadline Timestamp     `json:"deadline"`
}

// CheckDecisionOutput is the persisted approval state and, when terminal, its
// canonical resolution event.
type CheckDecisionOutput struct {
	State      ApprovalState         `json:"state"`
	Resolution *ApprovalResolutionV1 `json:"resolution,omitempty"`
}

// AwaitDecisionOutput is CheckDecisionOutput plus the actual server-bounded
// wait requested. EffectiveTimeoutMS makes the 60000 ms clamp observable.
type AwaitDecisionOutput struct {
	State              ApprovalState         `json:"state"`
	Resolution         *ApprovalResolutionV1 `json:"resolution,omitempty"`
	EffectiveTimeoutMS int64                 `json:"effective_timeout_ms"`
}
