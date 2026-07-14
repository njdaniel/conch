package schema

import (
	"testing"
	"time"
)

func validDecision() Decision {
	return Decision{
		PrincipalID: 5,
		OptionID:    "approve",
		Reason:      "Risk within limits.",
		At:          NewTimestamp(time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC)),
	}
}

func TestDecisionValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Decision)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Decision) {}, wantErr: false},
		{name: "non-positive principal", mutate: func(d *Decision) { d.PrincipalID = 0 }, wantErr: true},
		{name: "empty option id", mutate: func(d *Decision) { d.OptionID = "" }, wantErr: true},
		{name: "empty reason", mutate: func(d *Decision) { d.Reason = "" }, wantErr: true},
		{name: "whitespace reason is allowed by schema", mutate: func(d *Decision) { d.Reason = " " }, wantErr: false},
		{name: "missing timestamp", mutate: func(d *Decision) { d.At = Timestamp{} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDecision()
			tt.mutate(&d)
			if gotErr := d.Validate() != nil; gotErr != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", d.Validate(), tt.wantErr)
			}
		})
	}
}

func TestOptionValidate(t *testing.T) {
	tests := []struct {
		name    string
		option  Option
		wantErr bool
	}{
		{name: "valid approve", option: Option{ID: "approve", Label: "Approve", Kind: OptionKindApprove}, wantErr: false},
		{name: "valid reject", option: Option{ID: "reject", Label: "Reject", Kind: OptionKindReject}, wantErr: false},
		{name: "valid custom", option: Option{ID: "half", Label: "Approve half", Kind: OptionKindCustom}, wantErr: false},
		{name: "empty id", option: Option{ID: "", Label: "Approve", Kind: OptionKindApprove}, wantErr: true},
		{name: "empty label", option: Option{ID: "approve", Label: "", Kind: OptionKindApprove}, wantErr: true},
		{name: "invalid kind", option: Option{ID: "approve", Label: "Approve", Kind: "maybe"}, wantErr: true},
		{name: "empty kind", option: Option{ID: "approve", Label: "Approve", Kind: ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotErr := tt.option.Validate() != nil; gotErr != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", tt.option.Validate(), tt.wantErr)
			}
		})
	}
}
