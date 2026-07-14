package schema

import (
	"testing"
	"time"
)

func validResolution() ApprovalResolutionV1 {
	return ApprovalResolutionV1{
		ApprovalID: 42,
		Outcome:    OutcomeApproved,
		OptionID:   "approve",
		Decisions:  []Decision{validDecision()},
		ResolvedAt: NewTimestamp(time.Date(2026, 7, 14, 12, 30, 5, 0, time.UTC)),
	}
}

func TestResolutionValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ApprovalResolutionV1)
		wantErr bool
	}{
		{name: "valid approved", mutate: func(*ApprovalResolutionV1) {}, wantErr: false},
		{name: "valid rejected", mutate: func(r *ApprovalResolutionV1) {
			r.Outcome = OutcomeRejected
			r.OptionID = "reject"
		}, wantErr: false},
		{name: "valid custom", mutate: func(r *ApprovalResolutionV1) {
			r.Outcome = OutcomeCustom
			r.OptionID = "approve_half"
		}, wantErr: false},
		{name: "valid expired", mutate: func(r *ApprovalResolutionV1) {
			r.Outcome = OutcomeExpired
			r.OptionID = ""
			r.Decisions = []Decision{}
		}, wantErr: false},
		{name: "expired with dissenting decisions", mutate: func(r *ApprovalResolutionV1) {
			r.Outcome = OutcomeExpired
			r.OptionID = ""
			// A single decision that did not meet quorum; still recorded.
		}, wantErr: false},
		{name: "non-positive approval id", mutate: func(r *ApprovalResolutionV1) { r.ApprovalID = 0 }, wantErr: true},
		{name: "invalid outcome", mutate: func(r *ApprovalResolutionV1) { r.Outcome = "maybe" }, wantErr: true},
		{name: "empty outcome", mutate: func(r *ApprovalResolutionV1) { r.Outcome = "" }, wantErr: true},
		{name: "missing resolved_at", mutate: func(r *ApprovalResolutionV1) { r.ResolvedAt = Timestamp{} }, wantErr: true},
		{name: "expired with option id", mutate: func(r *ApprovalResolutionV1) {
			r.Outcome = OutcomeExpired
			// OptionID left set — illegal for expired.
		}, wantErr: true},
		{name: "non-expired without option id", mutate: func(r *ApprovalResolutionV1) { r.OptionID = "" }, wantErr: true},
		{name: "non-expired without decisions", mutate: func(r *ApprovalResolutionV1) { r.Decisions = nil }, wantErr: true},
		{name: "invalid decision", mutate: func(r *ApprovalResolutionV1) { r.Decisions[0].Reason = "" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validResolution()
			tt.mutate(&r)
			if gotErr := r.Validate() != nil; gotErr != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", r.Validate(), tt.wantErr)
			}
		})
	}
}

func TestOutcomeValid(t *testing.T) {
	tests := []struct {
		outcome Outcome
		valid   bool
	}{
		{outcome: OutcomeApproved, valid: true},
		{outcome: OutcomeRejected, valid: true},
		{outcome: OutcomeCustom, valid: true},
		{outcome: OutcomeExpired, valid: true},
		{outcome: "", valid: false},
		{outcome: "APPROVED", valid: false},
		{outcome: "denied", valid: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.outcome), func(t *testing.T) {
			if got := tt.outcome.Valid(); got != tt.valid {
				t.Errorf("%q.Valid() = %v, want %v", tt.outcome, got, tt.valid)
			}
		})
	}
}
