package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVoicePermissionGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"principal-voice-permission-request-v1.json", func() any { return new(VoicePermissionRequestV1) }},
		{"principal-voice-permission-response-v1.json", func() any { return new(VoicePermissionResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The request decodes strictly: it has one field, and the per-role cell
// (use_voice, ADR-014) is not set through it.
func TestVoicePermissionRequestV1RefusesUnknownFields(t *testing.T) {
	for _, raw := range []string{
		`{"voice_allowed":true,"share_screen":true}`,
		`{"voice_allowed":true,"use_voice":true}`,
		`{"voice_allowed":true,"principal_id":3}`,
	} {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var r VoicePermissionRequestV1
		if err := dec.Decode(&r); err == nil {
			t.Errorf("%s decoded into VoicePermissionRequestV1", raw)
		}
	}
}

func TestVoicePermissionErrorCode(t *testing.T) {
	if ErrorCodeNotAPerson != "not_a_person" {
		t.Errorf("ErrorCodeNotAPerson = %q", ErrorCodeNotAPerson)
	}
	// The same word the voice helper uses for the same condition.
	if string(VoiceControlReasonNotAPerson) != ErrorCodeNotAPerson {
		t.Errorf("VoiceControlReasonNotAPerson = %q, ErrorCodeNotAPerson = %q", VoiceControlReasonNotAPerson, ErrorCodeNotAPerson)
	}
}
