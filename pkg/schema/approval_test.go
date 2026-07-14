package schema

import (
	"testing"
	"time"
)

func TestApprovalGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "option", file: "approval-option-v1.json", new: func() any { return &Option{} }},
		{name: "decision", file: "approval-decision-v1.json", new: func() any { return &Decision{} }},
		{name: "approval", file: "conch-approval-v1.json", new: func() any { return &ApprovalV1{} }},
		{name: "resolution approved", file: "approval-resolution-v1.json", new: func() any { return &ApprovalResolutionV1{} }},
		{name: "resolution expired", file: "approval-resolution-v1-expired.json", new: func() any { return &ApprovalResolutionV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestApprovalRegisteredPayloads(t *testing.T) {
	for _, name := range []string{ApprovalV1Name, ApprovalResolutionV1Name} {
		if _, ok := LookupPayload(name); !ok {
			t.Errorf("LookupPayload(%q) not found — payload not registered", name)
		}
	}
}

func validApproval() ApprovalV1 {
	return ApprovalV1{
		ID:          42,
		RequesterID: 3,
		ChannelID:   7,
		Title:       "Approve trade",
		Body:        "buy 0.5 BTC-USD",
		Options: []Option{
			{ID: "approve", Label: "Approve", Kind: OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: OptionKindReject},
		},
		Deadline:  NewTimestamp(time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)),
		Quorum:    1,
		State:     ApprovalStatePending,
		CreatedAt: NewTimestamp(time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)),
	}
}

func TestApprovalValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ApprovalV1)
		wantErr bool
	}{
		{name: "valid", mutate: func(*ApprovalV1) {}, wantErr: false},
		{name: "valid with custom option", mutate: func(a *ApprovalV1) {
			a.Options = append(a.Options, Option{ID: "half", Label: "Approve half", Kind: OptionKindCustom})
		}, wantErr: false},
		{name: "valid with ntfy escalation", mutate: func(a *ApprovalV1) {
			a.EscalationTarget = &EscalationTarget{Kind: EscalationTargetNtfyTopic, Topic: "urgent"}
		}, wantErr: false},
		{name: "valid with principal escalation", mutate: func(a *ApprovalV1) {
			a.EscalationTarget = &EscalationTarget{Kind: EscalationTargetPrincipal, PrincipalID: 9}
		}, wantErr: false},
		{name: "escalated state", mutate: func(a *ApprovalV1) { a.State = ApprovalStateEscalated }, wantErr: false},
		{name: "non-positive id", mutate: func(a *ApprovalV1) { a.ID = 0 }, wantErr: true},
		{name: "non-positive requester", mutate: func(a *ApprovalV1) { a.RequesterID = 0 }, wantErr: true},
		{name: "non-positive channel", mutate: func(a *ApprovalV1) { a.ChannelID = 0 }, wantErr: true},
		{name: "empty title", mutate: func(a *ApprovalV1) { a.Title = "" }, wantErr: true},
		{name: "empty body", mutate: func(a *ApprovalV1) { a.Body = "" }, wantErr: true},
		{name: "empty options", mutate: func(a *ApprovalV1) { a.Options = nil }, wantErr: true},
		{name: "options missing approve", mutate: func(a *ApprovalV1) {
			a.Options = []Option{{ID: "reject", Label: "Reject", Kind: OptionKindReject}}
		}, wantErr: true},
		{name: "options missing reject", mutate: func(a *ApprovalV1) {
			a.Options = []Option{{ID: "approve", Label: "Approve", Kind: OptionKindApprove}}
		}, wantErr: true},
		{name: "duplicate option id", mutate: func(a *ApprovalV1) {
			a.Options = []Option{
				{ID: "approve", Label: "Approve", Kind: OptionKindApprove},
				{ID: "approve", Label: "Reject", Kind: OptionKindReject},
			}
		}, wantErr: true},
		{name: "invalid option", mutate: func(a *ApprovalV1) { a.Options[0].Label = "" }, wantErr: true},
		{name: "missing deadline", mutate: func(a *ApprovalV1) { a.Deadline = Timestamp{} }, wantErr: true},
		{name: "quorum zero", mutate: func(a *ApprovalV1) { a.Quorum = 0 }, wantErr: true},
		{name: "quorum negative", mutate: func(a *ApprovalV1) { a.Quorum = -1 }, wantErr: true},
		{name: "quorum above one", mutate: func(a *ApprovalV1) { a.Quorum = 2 }, wantErr: false},
		{name: "invalid state", mutate: func(a *ApprovalV1) { a.State = "done" }, wantErr: true},
		{name: "empty state", mutate: func(a *ApprovalV1) { a.State = "" }, wantErr: true},
		{name: "missing created_at", mutate: func(a *ApprovalV1) { a.CreatedAt = Timestamp{} }, wantErr: true},
		{name: "invalid escalation target", mutate: func(a *ApprovalV1) {
			a.EscalationTarget = &EscalationTarget{Kind: EscalationTargetNtfyTopic}
		}, wantErr: true},
		{name: "invalid payload", mutate: func(a *ApprovalV1) {
			a.Payload = &Payload{Schema: "not a valid name", Data: []byte(`{}`)}
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validApproval()
			tt.mutate(&a)
			if gotErr := a.Validate() != nil; gotErr != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", a.Validate(), tt.wantErr)
			}
		})
	}
}

func TestEscalationTargetValidate(t *testing.T) {
	tests := []struct {
		name    string
		target  EscalationTarget
		wantErr bool
	}{
		{name: "principal ok", target: EscalationTarget{Kind: EscalationTargetPrincipal, PrincipalID: 9}, wantErr: false},
		{name: "ntfy ok", target: EscalationTarget{Kind: EscalationTargetNtfyTopic, Topic: "urgent"}, wantErr: false},
		{name: "principal missing id", target: EscalationTarget{Kind: EscalationTargetPrincipal}, wantErr: true},
		{name: "principal with topic", target: EscalationTarget{Kind: EscalationTargetPrincipal, PrincipalID: 9, Topic: "x"}, wantErr: true},
		{name: "ntfy missing topic", target: EscalationTarget{Kind: EscalationTargetNtfyTopic}, wantErr: true},
		{name: "ntfy with principal", target: EscalationTarget{Kind: EscalationTargetNtfyTopic, Topic: "u", PrincipalID: 9}, wantErr: true},
		{name: "unknown kind", target: EscalationTarget{Kind: "email"}, wantErr: true},
		{name: "empty kind", target: EscalationTarget{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotErr := tt.target.Validate() != nil; gotErr != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", tt.target.Validate(), tt.wantErr)
			}
		})
	}
}
