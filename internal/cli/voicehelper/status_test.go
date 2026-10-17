package voicehelper

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestRetryable pins design note §15's two lists.
func TestRetryable(t *testing.T) {
	retry := []string{
		"duplicate_identity", "server_unreachable", "voice_unavailable", "audio_unavailable", "start_timeout",
		"internal_error", "protocol_error", ReasonHelloTimeout, ReasonAckTimeout, ReasonBadEvent, ReasonHelperExited,
		ReasonHelperCrashed, ReasonSpawnFailed,
	}
	noRetry := []string{
		"not_signed_in", "identity_mismatch", "bad_environment", "bad_config", "voice_requires_auth", "not_a_person",
		"voice_not_allowed", "not_a_member", "voice_not_configured", ReasonHelperNotFound, ReasonHelperUntrusted,
		ReasonHelperIncompatible, "left", "owner_lost", "signal", "",
	}
	for _, r := range retry {
		if !Retryable(r) {
			t.Errorf("%q is not retryable", r)
		}
	}
	for _, r := range noRetry {
		if Retryable(r) {
			t.Errorf("%q is retryable", r)
		}
	}
}

func TestFromState(t *testing.T) {
	retryIn := int64(1500)
	tests := []struct {
		name  string
		state schema.VoiceControlStateV1
		want  Status
	}{
		{
			name: "connected",
			state: schema.VoiceControlStateV1{Phase: schema.VoiceControlPhaseConnected, Capture: schema.VoiceControlCaptureOn, Muted: true,
				Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}, PTT: schema.VoiceControlPTTReady},
			want: Status{Gen: 3, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn, Muted: true,
				Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}, PTT: schema.VoiceControlPTTReady, HelperVersion: "v"},
		},
		{
			name: "reconnecting with a retry time",
			state: schema.VoiceControlStateV1{Phase: schema.VoiceControlPhaseReconnecting, Reason: schema.VoiceControlReasonConnectionLost,
				RetryInMS: &retryIn, Capture: schema.VoiceControlCaptureOn, PTT: schema.VoiceControlPTTReady},
			want: Status{Gen: 3, Channel: "c", Phase: PhaseReconnecting, Reason: "connection_lost", RetryIn: 1500 * time.Millisecond,
				Capture: schema.VoiceControlCaptureOn, PTT: schema.VoiceControlPTTReady, HelperVersion: "v"},
		},
		{
			name:  "failed, retryable",
			state: schema.VoiceControlStateV1{Phase: schema.VoiceControlPhaseFailed, Reason: schema.VoiceControlReasonDuplicateIdentity, Capture: schema.VoiceControlCaptureOff},
			want: Status{Gen: 3, Channel: "c", Phase: PhaseFailed, Reason: "duplicate_identity", Retryable: true,
				Capture: schema.VoiceControlCaptureOff, HelperVersion: "v"},
		},
		{
			name:  "stopped is never retryable",
			state: schema.VoiceControlStateV1{Phase: schema.VoiceControlPhaseStopped, Reason: schema.VoiceControlReasonSignal, Capture: schema.VoiceControlCaptureOff},
			want:  Status{Gen: 3, Channel: "c", Phase: PhaseStopped, Reason: "signal", Capture: schema.VoiceControlCaptureOff, HelperVersion: "v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromState(3, "c", "v", &tt.state)
			if !statusEqual(got, tt.want) {
				t.Errorf("fromState = %+v\nwant %+v", got, tt.want)
			}
			if len(tt.state.Blocked) > 0 {
				tt.state.Blocked[0] = "changed"
				if got.Blocked[0] == "changed" {
					t.Error("the status shares the state's slice")
				}
			}
		})
	}
}

// statusEqual compares two statuses, an empty Blocked equal to none.
func statusEqual(a, b Status) bool {
	if !slices.Equal(a.Blocked, b.Blocked) {
		return false
	}
	a.Blocked, b.Blocked = nil, nil
	return reflect.DeepEqual(a, b)
}

func TestEndedStatus(t *testing.T) {
	st := Status{Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn, Transmitting: true, RetryIn: time.Second,
		Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}}
	got := st.failed(ReasonHelperCrashed).ended()
	if got.Phase != PhaseFailed || got.Reason != ReasonHelperCrashed || !got.Retryable || got.Capture != schema.VoiceControlCaptureOff ||
		got.Transmitting || got.RetryIn != 0 ||
		!slices.Equal(got.Blocked, []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected, schema.VoiceControlBlockMuted}) {
		t.Errorf("ended = %+v", got)
	}
}

// TestUpdatesLatestWins: Updates holds one status, the latest, and a reader
// that is not reading never holds the supervisor up.
func TestUpdatesLatestWins(t *testing.T) {
	s := New(Profile{}, Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 100; i++ {
			s.mu.Lock()
			s.status = Status{Gen: uint64(i), Phase: PhaseConnected}
			s.publishLocked()
			s.mu.Unlock()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing waited for a reader")
	}
	if got := <-s.Updates(); got.Gen != 100 {
		t.Fatalf("got %+v, want the latest", got)
	}
	select {
	case st := <-s.Updates():
		t.Fatalf("a second status %+v was held", st)
	default:
	}
	if cap(s.updates) != 1 {
		t.Errorf("capacity %d, want 1", cap(s.updates))
	}
}

func TestSanitizeDiag(t *testing.T) {
	long := strings.Repeat("é", 150) // 300 bytes
	tests := []struct {
		name  string
		line  string
		token string
		want  string
	}{
		{name: "plain", line: "joining", want: "joining"},
		{name: "control characters", line: "a\x1b[31mb\x07c\td\r", want: "a[31mbcd"},
		{name: "C1 and formatting characters", line: "a\u009bb\u202ec\u200bd", want: "abcd"},
		{name: "invalid UTF-8", line: "a\xffb", want: "ab"},
		{name: "the token", line: "auth tok-123 failed", token: "tok-123", want: "auth [redacted] failed"},
		{name: "the token split by a control character", line: "auth tok\x1b-123", token: "tok-123", want: "auth [redacted]"},
		{name: "cut on a character boundary", line: long, want: strings.Repeat("é", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeDiag([]byte(tt.line), tt.token); got != tt.want {
				t.Errorf("sanitizeDiag = %q, want %q", got, tt.want)
			}
		})
	}
}
