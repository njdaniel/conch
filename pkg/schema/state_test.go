package schema

import "testing"

func TestApprovalStateIsTerminal(t *testing.T) {
	tests := []struct {
		state    ApprovalState
		terminal bool
	}{
		{state: ApprovalStatePending, terminal: false},
		{state: ApprovalStateEscalated, terminal: false},
		{state: ApprovalStateResolved, terminal: true},
		{state: ApprovalStateExpired, terminal: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsTerminal(); got != tt.terminal {
				t.Errorf("%q.IsTerminal() = %v, want %v", tt.state, got, tt.terminal)
			}
		})
	}
}

func TestApprovalStateValid(t *testing.T) {
	tests := []struct {
		state ApprovalState
		valid bool
	}{
		{state: ApprovalStatePending, valid: true},
		{state: ApprovalStateEscalated, valid: true},
		{state: ApprovalStateResolved, valid: true},
		{state: ApprovalStateExpired, valid: true},
		{state: "", valid: false},
		{state: "PENDING", valid: false},
		{state: "done", valid: false},
		{state: "resolving", valid: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.Valid(); got != tt.valid {
				t.Errorf("%q.Valid() = %v, want %v", tt.state, got, tt.valid)
			}
		})
	}
}
