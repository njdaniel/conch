package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestApprovalAPIGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"create-approval-request-v1.json", func() any { return new(CreateApprovalRequestV1) }},
		{"create-approval-response-v1.json", func() any { return new(CreateApprovalResponseV1) }},
		{"list-approvals-response-v1.json", func() any { return new(ListApprovalsResponseV1) }},
		{"cast-decision-request-v1.json", func() any { return new(CastDecisionRequestV1) }},
		{"cast-decision-response-v1.json", func() any { return new(CastDecisionResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func validCreateApprovalRequest() CreateApprovalRequestV1 {
	return CreateApprovalRequestV1{
		RequesterID: 3,
		ChannelID:   7,
		Title:       "Approve trade",
		Body:        "Details",
		Payload:     &Payload{Schema: "leviathan.trade_signal.v1", Data: json.RawMessage(`{"side":"buy"}`)},
		Options: []Option{
			{ID: "approve", Label: "Approve", Kind: OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: OptionKindReject},
		},
		Deadline:         NewTimestamp(time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)),
		Quorum:           1,
		EscalationTarget: &EscalationTarget{Kind: EscalationTargetNtfyTopic, Topic: "conch-urgent"},
	}
}

func TestCreateApprovalRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateApprovalRequestV1)
		wantErr string
	}{
		{"valid", func(r *CreateApprovalRequestV1) {}, ""},
		{"quorum omitted means server default", func(r *CreateApprovalRequestV1) { r.Quorum = 0 }, ""},
		{"no payload or escalation", func(r *CreateApprovalRequestV1) { r.Payload = nil; r.EscalationTarget = nil }, ""},
		{"missing requester", func(r *CreateApprovalRequestV1) { r.RequesterID = 0 }, "requester_id"},
		{"missing channel", func(r *CreateApprovalRequestV1) { r.ChannelID = 0 }, "channel_id"},
		{"missing title", func(r *CreateApprovalRequestV1) { r.Title = "" }, "title"},
		{"missing body", func(r *CreateApprovalRequestV1) { r.Body = "" }, "body"},
		{"no options", func(r *CreateApprovalRequestV1) { r.Options = nil }, "at least one option"},
		{"missing reject option", func(r *CreateApprovalRequestV1) {
			r.Options = []Option{{ID: "approve", Label: "Approve", Kind: OptionKindApprove}}
		}, "approve and one reject"},
		{"duplicate option id", func(r *CreateApprovalRequestV1) {
			r.Options = append(r.Options, Option{ID: "approve", Label: "Again", Kind: OptionKindApprove})
		}, "duplicated"},
		{"zero deadline", func(r *CreateApprovalRequestV1) { r.Deadline = Timestamp{} }, "deadline"},
		{"negative quorum", func(r *CreateApprovalRequestV1) { r.Quorum = -1 }, "quorum"},
		{"malformed payload name", func(r *CreateApprovalRequestV1) { r.Payload.Schema = "Bad-Name" }, "versioned name"},
		{"invalid escalation", func(r *CreateApprovalRequestV1) { r.EscalationTarget.Topic = "" }, "topic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validCreateApprovalRequest()
			tt.mutate(&r)
			err := r.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCastDecisionRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     CastDecisionRequestV1
		wantErr string
	}{
		{"valid", CastDecisionRequestV1{PrincipalID: 5, OptionID: "approve", Reason: "ok"}, ""},
		{"missing principal", CastDecisionRequestV1{OptionID: "approve", Reason: "ok"}, "principal_id"},
		{"missing option", CastDecisionRequestV1{PrincipalID: 5, Reason: "ok"}, "option_id"},
		{"missing reason", CastDecisionRequestV1{PrincipalID: 5, OptionID: "approve"}, "reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
