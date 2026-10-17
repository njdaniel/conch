package voicehelper

import (
	"slices"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Phase is where voice is: idle when no helper has been asked for (or after
// Leave or Close), otherwise one of the phases the helper reports.
type Phase string

// Phases. All but PhaseIdle are the wire phases of conch.voice_control.v1.
const (
	PhaseIdle         Phase = "idle"
	PhaseStarting     Phase = Phase(schema.VoiceControlPhaseStarting)
	PhaseConnecting   Phase = Phase(schema.VoiceControlPhaseConnecting)
	PhaseConnected    Phase = Phase(schema.VoiceControlPhaseConnected)
	PhaseReconnecting Phase = Phase(schema.VoiceControlPhaseReconnecting)
	PhaseStopping     Phase = Phase(schema.VoiceControlPhaseStopping)
	PhaseStopped      Phase = Phase(schema.VoiceControlPhaseStopped)
	PhaseFailed       Phase = Phase(schema.VoiceControlPhaseFailed)
)

// Reasons the supervisor gives for a failure it saw itself. They never cross
// the wire; each comes with PhaseFailed. Every other reason in a Status is a
// wire reason (schema.VoiceControlReason).
const (
	ReasonHelperNotFound     = "helper_not_found"
	ReasonHelperUntrusted    = "helper_untrusted"
	ReasonHelperIncompatible = "helper_incompatible"
	ReasonSpawnFailed        = "spawn_failed"
	ReasonHelloTimeout       = "hello_timeout"
	ReasonAckTimeout         = "ack_timeout"
	ReasonBadEvent           = "bad_event"
	ReasonHelperExited       = "helper_exited"
	ReasonHelperCrashed      = "helper_crashed"
)

// retryableReasons are the failures a new attempt could fix (design note
// §15). The rest need the person to do something else first: sign in again,
// fix voice.toml, install a matching conch-voice.
var retryableReasons = []string{
	string(schema.VoiceControlReasonDuplicateIdentity),
	string(schema.VoiceControlReasonServerUnreachable),
	string(schema.VoiceControlReasonVoiceUnavailable),
	string(schema.VoiceControlReasonAudioUnavailable),
	string(schema.VoiceControlReasonStartTimeout),
	string(schema.VoiceControlReasonInternalError),
	string(schema.VoiceControlReasonProtocolError),
	ReasonHelloTimeout, ReasonAckTimeout, ReasonBadEvent, ReasonHelperExited, ReasonHelperCrashed, ReasonSpawnFailed,
}

// Retryable reports whether a failure for reason may be tried again with
// Retry.
func Retryable(reason string) bool {
	return slices.Contains(retryableReasons, reason)
}

// Status is what voice is doing, made only from what the helper confirmed and
// what the supervisor saw itself, never from what was asked for.
type Status struct {
	// Gen says which helper process this is about: 1 for the first started,
	// and one more for each after it; 0 before the first.
	Gen uint64
	// Channel is the channel the helper was started for.
	Channel string
	// Phase is idle, then the helper's phases.
	Phase Phase
	// Reason is a wire reason or one of the supervisor's Reason constants.
	Reason string
	// Retryable is whether Retry may be offered: only for PhaseFailed.
	Retryable bool
	// RetryIn is, while reconnecting for a reason, when the helper asks
	// conchd again.
	RetryIn       time.Duration
	Capture       schema.VoiceControlCapture
	Transmitting  bool
	Muted         bool
	Deafened      bool
	Blocked       []schema.VoiceControlBlock
	PTT           schema.VoiceControlPTT
	PTTProblem    schema.VoiceControlPTTProblem
	HelperVersion string
}

// clone copies s, so that what a caller holds never shares the supervisor's
// slice.
func (s Status) clone() Status {
	s.Blocked = slices.Clone(s.Blocked)
	return s
}

// fromState is the status a helper's state snapshot says.
func fromState(gen uint64, channel, helperVersion string, st *schema.VoiceControlStateV1) Status {
	out := Status{
		Gen:           gen,
		Channel:       channel,
		Phase:         Phase(st.Phase),
		Reason:        string(st.Reason),
		Capture:       st.Capture,
		Transmitting:  st.Transmitting,
		Muted:         st.Muted,
		Deafened:      st.Deafened,
		Blocked:       slices.Clone(st.Blocked),
		PTT:           st.PTT,
		PTTProblem:    st.PTTProblem,
		HelperVersion: helperVersion,
	}
	if st.RetryInMS != nil {
		out.RetryIn = time.Duration(*st.RetryInMS) * time.Millisecond
	}
	out.Retryable = out.Phase == PhaseFailed && Retryable(out.Reason)
	return out
}

// failed turns s into a failure for reason, keeping what the helper last
// confirmed about mute, deafen and the talk key.
func (s Status) failed(reason string) Status {
	s.Phase = PhaseFailed
	s.Reason = reason
	s.Retryable = Retryable(reason)
	s.RetryIn = 0
	return s
}

// ended is s once the process is gone: nothing can be captured or sent, and
// nothing is connected.
func (s Status) ended() Status {
	s.Capture = schema.VoiceControlCaptureOff
	s.Transmitting = false
	s.RetryIn = 0
	if !slices.Contains(s.Blocked, schema.VoiceControlBlockNotConnected) {
		s.Blocked = append([]schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}, s.Blocked...)
	}
	return s
}
