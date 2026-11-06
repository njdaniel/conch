package schema

import (
	"encoding/json"
	"testing"
)

// TestHealthServerMode checks the wire rule of ADR-011 item 2.3 (issue #865)
// on /healthz: normal mode omits server_mode, so the answer every existing
// server gives is unchanged; temporary mode says "temporary"; and nothing
// else is a mode on the wire, "normal" included. Health has no golden
// fixture (it is an operational shape), so the bytes are checked here.
func TestHealthServerMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    ServerMode
		want    string
		wantErr bool
	}{
		{"normal mode omits the field", "", `{"status":"ok","version":"v1","db":"ok","voice":"not_configured"}`, false},
		{"temporary", ServerModeTemporary, `{"status":"ok","version":"v1","db":"ok","voice":"not_configured","server_mode":"temporary"}`, false},
		{"normal is never sent", "normal", `{"status":"ok","version":"v1","db":"ok","voice":"not_configured","server_mode":"normal"}`, true},
		{"unknown mode", "ephemeral", `{"status":"ok","version":"v1","db":"ok","voice":"not_configured","server_mode":"ephemeral"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Health{Status: HealthOK, Version: "v1", DB: HealthOK, Voice: HealthVoiceNotConfigured, ServerMode: tt.mode}
			if err := h.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, want error %v", err, tt.wantErr)
			}
			raw, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.want {
				t.Errorf("health = %s, want %s", raw, tt.want)
			}
			var back Health
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if back != h {
				t.Errorf("round trip = %+v, want %+v", back, h)
			}
		})
	}
}

// TestServerModeValid pins the vocabulary: only "temporary" is a mode on the
// wire. The empty value is the field's absence, which the carrying struct's
// Validate accepts and Valid does not.
func TestServerModeValid(t *testing.T) {
	tests := []struct {
		mode ServerMode
		want bool
	}{
		{ServerModeTemporary, true},
		{"", false},
		{"normal", false},
		{"Temporary", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if got := tt.mode.Valid(); got != tt.want {
				t.Errorf("ServerMode(%q).Valid() = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
