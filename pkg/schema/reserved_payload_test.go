package schema

import "testing"

func TestReservedMessagePayload(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{ApprovalV1Name, true},
		{AdminActionV1Name, true},
		{"conch.approval.v2", true},
		{"conch.admin_action.v7", true},
		{"conch.approvals.v1", false},
		{"conch.approval", true},
		{FileAttachmentV1Name, false},
		{"acme.alert.v1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ReservedMessagePayload(tt.name); got != tt.want {
			t.Errorf("ReservedMessagePayload(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
