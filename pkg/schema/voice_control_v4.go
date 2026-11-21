package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// VoiceControlV4CallIDPattern matches the canonical unpadded base64url
// encoding of the 16 bytes minted by NewPrivateCallID. Only two bits of
// the last character carry data, so its four unused bits must be zero.
const VoiceControlV4CallIDPattern = `^[A-Za-z0-9_-]{21}[AQgw]$`

var voiceControlV4CallIDRE = regexp.MustCompile(VoiceControlV4CallIDPattern)

const (
	// VoiceControlV4MinLevelDB is the floor of the reported microphone level.
	VoiceControlV4MinLevelDB = -96
	// VoiceControlV4MaxLossBasisPoints is the packet-loss value for 100 percent.
	VoiceControlV4MaxLossBasisPoints = 10000
)

// Version 4 of the managed voice-helper control protocol (issue #2039, child
// of #2013; ADR-017 Decision 13, Nick's decision 12 of 2026-11-19, and
// issue #2085). A v4 session extends v3 with a private-call destination,
// microphone diagnostics and call quality:
//
//   - the start names its destination, which is either a channel (the
//     "channel" field, as in v3) or a private call attempt (the "call"
//     field), exactly one of the two. The call field is the opaque attempt id
//     of PrivateCallV1 (private_call.go): never a room name, a token or a
//     URL. The helper asks the server for its media grant for that attempt
//     (POST /v1/calls/{call}/session) with the credential the start names,
//     and the server answers only the side that attempt selected. The
//     desktop renderer never holds the voice grant.
//   - set_diagnostics enables or disables levels and gate_event reports.
//     The state also reports which microphone processing is active.
//   - get_call_quality asks for a sample of per-participant statistics,
//     delivered as call_quality events followed by the command's ack.
//
// v4 carries over the framing, ack, protocol error and share-audio fields
// of v3. A call carries no camera or screen share, so it has no share for sound to
// belong to: set_share_audio is answered as it is for a session with no
// companion, with an ok ack and share_audio unavailable with the problem
// unsupported (VoiceControlShareAudioProblemUnsupported), never a new word.
//
// Negotiation: the hello is v1's, in its frozen shape, and offers every
// version the helper speaks. v4 is used only when the helper offers it and
// the owner writes its start in v4; an owner that does not speak v4, or a
// helper that does not offer it, keeps to v3 (or lower) exactly as before. An
// owner never offers a v4 start to a helper whose hello lacks v4, and the
// version is fixed for the session by the start's schema: a line of another
// version in a v4 session is unknown_schema, and a v4 line in a v1, v2 or v3
// session is too. The new commands, events and processing field are used
// only in a v4 session. A call start is valid in v4 alone.
//
// v1, v2 and v3 are published and do not change. The vocabularies v4 shares
// with them stay as they are: the share-audio words, the error codes and
// every vocabulary of the state. v4 has its own command and event kinds
// so the older readers keep their published vocabularies. v4 lines are read
// with ParseVoiceControlCommandV4 and ParseVoiceControlEventV4, by the framing
// rules of the others. The fixtures are in testdata/voice-control-v4/.

// VoiceControlSchemaV4 is the versioned name every v4 message but the hello
// carries in its schema field. The hello is always v1 (VoiceControlHelloV1).
const VoiceControlSchemaV4 = "conch.voice_control.v4"

// VoiceControlCommandKindV4 is what a v4 command asks for. It has v3's
// nine kinds plus the two commands for microphone diagnostics and call quality.
type VoiceControlCommandKindV4 string

// Voice control command kinds of v4.
const (
	// VoiceControlCommandV4Start is the first command and the only one
	// before it: join this channel or private call on this server as this principal.
	VoiceControlCommandV4Start VoiceControlCommandKindV4 = "start"
	// VoiceControlCommandV4SetMute sets whether the user is muted.
	VoiceControlCommandV4SetMute VoiceControlCommandKindV4 = "set_mute"
	// VoiceControlCommandV4SetDeafen sets whether the user is deafened.
	VoiceControlCommandV4SetDeafen VoiceControlCommandKindV4 = "set_deafen"
	// VoiceControlCommandV4GetState asks for a state snapshot now.
	VoiceControlCommandV4GetState VoiceControlCommandKindV4 = "get_state"
	// VoiceControlCommandV4Leave leaves voice; the helper then exits.
	VoiceControlCommandV4Leave VoiceControlCommandKindV4 = "leave"
	// VoiceControlCommandV4SetDevices names the microphone and speakers to
	// use from now on, without leaving.
	VoiceControlCommandV4SetDevices VoiceControlCommandKindV4 = "set_devices"
	// VoiceControlCommandV4SetTalkMode chooses what may open the gate: a
	// press, or the microphone's own sound.
	VoiceControlCommandV4SetTalkMode VoiceControlCommandKindV4 = "set_talk_mode"
	// VoiceControlCommandV4SetShareAudio captures and publishes the sound of
	// the owner's screen share, or stops.
	VoiceControlCommandV4SetShareAudio VoiceControlCommandKindV4 = "set_share_audio"
	// VoiceControlCommandV4SetShareAudioMute mutes the published share-audio
	// track, or unmutes it.
	VoiceControlCommandV4SetShareAudioMute VoiceControlCommandKindV4 = "set_share_audio_mute"
	// VoiceControlCommandV4SetDiagnostics enables or disables microphone diagnostics.
	VoiceControlCommandV4SetDiagnostics VoiceControlCommandKindV4 = "set_diagnostics"
	// VoiceControlCommandV4GetCallQuality requests a sample of call quality.
	VoiceControlCommandV4GetCallQuality VoiceControlCommandKindV4 = "get_call_quality"
)

// Valid reports whether k is a command kind of v4. A kind of no version, or
// of a later one, is unknown_kind to the v4 reader.
func (k VoiceControlCommandKindV4) Valid() bool {
	switch k {
	case VoiceControlCommandV4Start, VoiceControlCommandV4SetMute, VoiceControlCommandV4SetDeafen,
		VoiceControlCommandV4GetState, VoiceControlCommandV4Leave,
		VoiceControlCommandV4SetDevices, VoiceControlCommandV4SetTalkMode,
		VoiceControlCommandV4SetShareAudio, VoiceControlCommandV4SetShareAudioMute,
		VoiceControlCommandV4SetDiagnostics, VoiceControlCommandV4GetCallQuality:
		return true
	default:
		return false
	}
}

// VoiceControlEventKindV4 is what an event reports after negotiation.
// The initial hello uses VoiceControlEventKind and VoiceControlSchemaV1.
type VoiceControlEventKindV4 string

// Voice control event kinds of v4.
const (
	VoiceControlEventV4Ack           VoiceControlEventKindV4 = "ack"
	VoiceControlEventV4State         VoiceControlEventKindV4 = "state"
	VoiceControlEventV4ProtocolError VoiceControlEventKindV4 = "protocol_error"
	VoiceControlEventV4Levels        VoiceControlEventKindV4 = "levels"
	VoiceControlEventV4GateEvent     VoiceControlEventKindV4 = "gate_event"
	VoiceControlEventV4CallQuality   VoiceControlEventKindV4 = "call_quality"
)

// Valid reports whether k is an event kind of v4.
func (k VoiceControlEventKindV4) Valid() bool {
	switch k {
	case VoiceControlEventV4Ack, VoiceControlEventV4State, VoiceControlEventV4ProtocolError,
		VoiceControlEventV4Levels, VoiceControlEventV4GateEvent, VoiceControlEventV4CallQuality:
		return true
	default:
		return false
	}
}

func validateVoiceControlEventHeadV4(schema string, kind, want VoiceControlEventKindV4, seq int64) error {
	return validateVoiceControlEventHeadIn(VoiceControlSchemaV4, schema, VoiceControlEventKind(kind), VoiceControlEventKind(want), seq)
}

// VoiceControlStartV4 is the first command of a v4 session (owner to
// helper). Its fields are v3's, except that the destination is either a
// channel or a private call attempt: exactly one of Channel and Call is
// written. Nothing about a share is carried.
type VoiceControlStartV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	// Server, PrincipalID and Credential are as in VoiceControlStartV1.
	Server string `json:"server"`
	// Channel is the channel to join, as in VoiceControlStartV1. Absent for a
	// call. When present and empty the start is invalid, as for any optional
	// string.
	Channel string `json:"channel,omitempty"`
	// Call is the opaque attempt id of the private call to join (PrivateCallV1
	// ID): 22 canonical unpadded base64url characters from NewPrivateCallID.
	// Absent for a channel. The owner takes it from the server's attempt
	// record. The session route verifies the attempt exists and that the
	// authenticated endpoint is authorized to join it.
	Call        string                 `json:"call,omitempty"`
	PrincipalID int64                  `json:"principal_id"`
	Credential  VoiceControlCredential `json:"credential"`
	// Muted and Deafened are whether to start so. Always written.
	Muted    bool `json:"muted"`
	Deafened bool `json:"deafened"`
}

func (VoiceControlStartV4) voiceControlMessage() {}

// Validate reports whether the start is well-formed: v1's rules, and exactly
// one destination, a channel under v1's channel rule or a call under the
// attempt id rule.
func (s VoiceControlStartV4) Validate() error {
	if err := validateVoiceControlCommandHeadV4(s.Schema, s.Kind, VoiceControlCommandV4Start, s.ID); err != nil {
		return err
	}
	if err := validateVoiceControlServer(s.Server); err != nil {
		return err
	}
	switch {
	case s.Channel != "" && s.Call != "":
		return errors.New("schema: voice control start must name a channel or a call, not both")
	case s.Channel != "":
		if err := validateVoiceControlChannel(s.Channel); err != nil {
			return err
		}
	case s.Call != "":
		if !voiceControlV4CallIDRE.MatchString(s.Call) {
			return errors.New("schema: voice control start call must be a canonical 22-character private call attempt ID")
		}
	default:
		return errors.New("schema: voice control start must name a channel or a call")
	}
	if s.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice control start principal_id must be positive, got %d", s.PrincipalID)
	}
	if !s.Credential.Valid() {
		return fmt.Errorf("schema: voice control start credential %q is not one of stored, env", s.Credential)
	}
	return nil
}

// VoiceControlSetMuteV4 is VoiceControlSetMuteV1 in v4. It is the
// microphone's mute and nothing else: the share's sound has its own
// (VoiceControlSetShareAudioMuteV4).
type VoiceControlSetMuteV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	Muted  bool                      `json:"muted"`
}

func (VoiceControlSetMuteV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetMuteV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetMute, c.ID)
}

// VoiceControlSetDeafenV4 is VoiceControlSetDeafenV1 in v4.
type VoiceControlSetDeafenV4 struct {
	Schema   string                    `json:"schema"`
	Kind     VoiceControlCommandKindV4 `json:"kind"`
	ID       int64                     `json:"id"`
	Deafened bool                      `json:"deafened"`
}

func (VoiceControlSetDeafenV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetDeafenV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetDeafen, c.ID)
}

// VoiceControlGetStateV4 is VoiceControlGetStateV1 in v4.
type VoiceControlGetStateV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
}

func (VoiceControlGetStateV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlGetStateV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4GetState, c.ID)
}

// VoiceControlLeaveV4 is VoiceControlLeaveV1 in v4. Leaving ends a share's
// sound with the rest of the session.
type VoiceControlLeaveV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
}

func (VoiceControlLeaveV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlLeaveV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4Leave, c.ID)
}

// VoiceControlSetDevicesV4 is VoiceControlSetDevicesV2 in v4, by the same
// rules. The devices are the microphone and the speakers: a share's sound has
// no device, its captures follow the applications that play.
type VoiceControlSetDevicesV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	// Input and Output are as in VoiceControlSetDevicesV2: absent to leave a
	// side alone; an empty string is refused.
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

func (VoiceControlSetDevicesV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed: at least one side
// named, each under the name rule of VoiceControlSetDevicesV2.
func (c VoiceControlSetDevicesV4) Validate() error {
	if err := validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetDevices, c.ID); err != nil {
		return err
	}
	if c.Input == "" && c.Output == "" {
		return errors.New("schema: voice control set_devices must name input, output or both")
	}
	if c.Input != "" {
		if err := validateVoiceControlDeviceName("input", c.Input); err != nil {
			return err
		}
	}
	if c.Output != "" {
		if err := validateVoiceControlDeviceName("output", c.Output); err != nil {
			return err
		}
	}
	return nil
}

// VoiceControlSetTalkModeV4 is VoiceControlSetTalkModeV2 in v4.
type VoiceControlSetTalkModeV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	// TalkMode is the mode wanted.
	TalkMode VoiceControlTalkMode `json:"talk_mode"`
}

func (VoiceControlSetTalkModeV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetTalkModeV4) Validate() error {
	if err := validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetTalkMode, c.ID); err != nil {
		return err
	}
	if !c.TalkMode.Valid() {
		return fmt.Errorf("schema: voice control set_talk_mode talk_mode %q is not one of push_to_talk, open", c.TalkMode)
	}
	return nil
}

// VoiceControlSetShareAudioV4 asks the helper to capture and publish the
// sound of the owner's screen share, or to stop (owner to helper). A desired
// state, like set_mute, and idempotent: asking again for what is in force
// changes nothing.
//
// The owner sends it only after the sharer's consent to that sound is
// recorded with conchd; the command carries the scope consented to, and
// nothing here records or widens a consent. What the helper cannot do it
// refuses into the state, with an ack that is ok: share_audio is unavailable
// and share_audio_problem says why (unsupported_scope for a scope this
// platform cannot capture, unsupported for a session with no companion,
// not_connected when the helper is not connected). Nothing here opens the
// transmit gate: the share's sound is a track of its own.
//
// The ack of a set_share_audio with capturing true follows the state that
// says how it went (tui-voice §5 rule 7), so it waits on the publish. The
// helper therefore bounds the publish well under VoiceControlAckTimeoutMS
// (an owner treats a later ack as a broken helper and the call drops): a
// publish still pending at its bound is given up and reported as
// share_audio unavailable with publish_refused. Commands that arrive while
// that ack is held are applied at once; only their acks wait, in order
// (rule 10), so a set_mute is never delayed by a slow publish.
type VoiceControlSetShareAudioV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	// Capturing is the state wanted: true to capture and publish, false to
	// stop. Always written.
	Capturing bool `json:"capturing"`
	// Scope is which sound: the vocabulary of the consent
	// (VoiceCompanionAudioRequestV1). Required when Capturing. Absent
	// otherwise: a stop names no scope, so that a scope left over from an
	// earlier command can never be read as a consent.
	Scope VoiceShareAudioScope `json:"scope,omitempty"`
	// ApplicationName is the name the owner's own application plays sound
	// under, which the helper leaves out of the capture so that the call is
	// not shared back into itself: 1 to VoiceControlMaxApplicationNameBytes
	// bytes of printable ASCII. Required when Capturing, absent otherwise.
	// The helper never repeats it in anything it writes.
	ApplicationName string `json:"application_name,omitempty"`
}

func (VoiceControlSetShareAudioV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed: with capturing, a
// known scope and an application name under its rule; without, neither. A
// scope that is known but that the helper's platform cannot capture is
// well-formed: the helper says so in the state.
func (c VoiceControlSetShareAudioV4) Validate() error {
	if err := validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetShareAudio, c.ID); err != nil {
		return err
	}
	if !c.Capturing {
		if c.Scope != "" {
			return errors.New("schema: voice control set_share_audio with capturing false must carry no scope")
		}
		if c.ApplicationName != "" {
			return errors.New("schema: voice control set_share_audio with capturing false must carry no application_name")
		}
		return nil
	}
	if !c.Scope.Valid() {
		return fmt.Errorf("schema: voice control set_share_audio scope %q is not one of application, system_excluding_call", c.Scope)
	}
	return validateVoiceControlApplicationName(c.ApplicationName)
}

// VoiceControlSetShareAudioMuteV4 mutes the published share-audio track, or
// unmutes it (owner to helper). A desired state, like set_mute, and kept
// whether or not a share's sound is being published: it applies to the track
// when there is one. The track stays published and the captures stay open;
// viewers see it muted. It is independent of set_mute (the microphone) and of
// set_deafen, and neither of those changes it.
type VoiceControlSetShareAudioMuteV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	// Muted is the state wanted. Always written.
	Muted bool `json:"muted"`
}

func (VoiceControlSetShareAudioMuteV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetShareAudioMuteV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetShareAudioMute, c.ID)
}

// VoiceControlSetDiagnosticsV4 sets whether the helper emits microphone
// levels and gate transitions. These reports are off at the start of a session.
type VoiceControlSetDiagnosticsV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
	On     bool                      `json:"on"`
}

func (VoiceControlSetDiagnosticsV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetDiagnosticsV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4SetDiagnostics, c.ID)
}

// VoiceControlGetCallQualityV4 asks for one call_quality event per sampled
// participant, then the existing ack for this command. An empty sample is
// valid and returns the ack alone.
//
// The owner reads the ack before sending another get_call_quality. If a
// poll arrives before the helper writes the previous poll's ack, the helper
// returns an error ack with invalid for the overlapping poll in FIFO order
// and emits no call_quality events for it.
type VoiceControlGetCallQualityV4 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV4 `json:"kind"`
	ID     int64                     `json:"id"`
}

func (VoiceControlGetCallQualityV4) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlGetCallQualityV4) Validate() error {
	return validateVoiceControlCommandHeadV4(c.Schema, c.Kind, VoiceControlCommandV4GetCallQuality, c.ID)
}

// VoiceControlAckV4 is VoiceControlAckV1 in v4. The refusals are v2's.
// invalid is also the answer to a set_share_audio that breaks its shape, and
// stopping to set_share_audio and set_share_audio_mute after leaving began,
// as it is to set_mute. A share-audio command the helper cannot carry out is
// not refused here: its ack is ok and the state says why not.
type VoiceControlAckV4 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlEventKindV4 `json:"kind"`
	Seq    int64                   `json:"seq"`
	ID     int64                   `json:"id"`
	OK     bool                    `json:"ok"`
	Error  VoiceControlErrorCode   `json:"error,omitempty"`
}

func (VoiceControlAckV4) voiceControlMessage() {}

// Validate reports whether the ack is well-formed.
func (a VoiceControlAckV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(a.Schema, a.Kind, VoiceControlEventV4Ack, a.Seq); err != nil {
		return err
	}
	if err := validateVoiceControlID(a.ID); err != nil {
		return err
	}
	switch {
	case a.OK && a.Error != "":
		return errors.New("schema: voice control ack with ok must have no error")
	case !a.OK && !a.Error.AckRefusal():
		return fmt.Errorf("schema: voice control ack error %q is not one of unknown_kind, invalid, already_started, stopping", a.Error)
	}
	return nil
}

// VoiceControlStateV4 is a whole-state snapshot (helper to owner): every
// field of VoiceControlStateV3, plus the active microphone processing.
// It names no device, application or stream.
type VoiceControlStateV4 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlEventKindV4 `json:"kind"`
	Seq    int64                   `json:"seq"`
	// Phase, Reason and RetryInMS are as in VoiceControlStateV2.
	Phase     VoiceControlPhase  `json:"phase"`
	Reason    VoiceControlReason `json:"reason,omitempty"`
	RetryInMS *int64             `json:"retry_in_ms,omitempty"`
	// Capture is the state of the microphone stream. Always written. It says
	// nothing of a share's sound, which is ShareAudio.
	Capture VoiceControlCapture `json:"capture"`
	// Playback is the state of the speakers' stream. Always written.
	Playback VoiceControlPlayback `json:"playback"`
	// Transmitting is whether the microphone's audio is leaving now, release
	// tail included. A share's sound is not counted: it is not the
	// microphone and never passes the gate.
	Transmitting bool `json:"transmitting"`
	// Muted and Deafened are as the push-to-talk machine has them.
	Muted    bool `json:"muted"`
	Deafened bool `json:"deafened"`
	// Blocked is as in VoiceControlStateV2. Always a JSON array. Nothing
	// about a share's sound is ever among the reasons.
	Blocked []VoiceControlBlock `json:"blocked"`
	// TalkMode is the mode in force, as in VoiceControlStateV2. Always
	// written.
	TalkMode VoiceControlTalkMode `json:"talk_mode"`
	// PTT and PTTProblem are as in VoiceControlStateV2.
	PTT        VoiceControlPTT        `json:"ptt"`
	PTTProblem VoiceControlPTTProblem `json:"ptt_problem,omitempty"`
	// ShareAudio is whether the sound of the owner's screen share is being
	// captured and published. Always written. off until asked for.
	ShareAudio VoiceControlShareAudio `json:"share_audio"`
	// ShareAudioMuted is the mute set_share_audio_mute asked for. Always
	// written. A desired state, like Muted: it may be true in any phase and
	// whatever ShareAudio is.
	ShareAudioMuted bool `json:"share_audio_muted"`
	// ShareAudioProblem is why ShareAudio is unavailable, or no_sources
	// beside on when nothing that may be captured is playing. Absent
	// otherwise: none is said by leaving it out, as with PTTProblem, and an
	// empty word is refused.
	ShareAudioProblem VoiceControlShareAudioProblem `json:"share_audio_problem,omitempty"`
	// ShareAudioSources is how many applications' streams are being mixed
	// into the share's sound now: 0 to VoiceControlMaxShareAudioSources.
	// Always written. A count and nothing else: which applications is never
	// said.
	ShareAudioSources int64 `json:"share_audio_sources"`
	// Processing is always written, even when the module is off.
	Processing VoiceControlProcessingV4 `json:"processing"`
}

// VoiceControlProcessingV4 reports the processing the helper is running.
// The echo module performs both echo cancellation and noise suppression.
type VoiceControlProcessingV4 struct {
	EchoCancellation bool `json:"echo_cancellation"`
	NoiseSuppression bool `json:"noise_suppression"`
}

func (VoiceControlStateV4) voiceControlMessage() {}

// Blocks reports whether b is among the state's block reasons.
func (s VoiceControlStateV4) Blocks(b VoiceControlBlock) bool {
	return slices.Contains(s.Blocked, b)
}

// v2 is the snapshot's v2 fields as a v2 state, for the rules the two
// versions share.
func (s VoiceControlStateV4) v2() VoiceControlStateV2 {
	return VoiceControlStateV2{
		Schema: VoiceControlSchemaV2, Kind: VoiceControlEventKind(s.Kind), Seq: s.Seq,
		Phase: s.Phase, Reason: s.Reason, RetryInMS: s.RetryInMS,
		Capture: s.Capture, Playback: s.Playback, Transmitting: s.Transmitting,
		Muted: s.Muted, Deafened: s.Deafened, Blocked: s.Blocked,
		TalkMode: s.TalkMode, PTT: s.PTT, PTTProblem: s.PTTProblem,
	}
}

// Validate reports whether the snapshot is well-formed and consistent. The
// rules are VoiceControlStateV2.Validate's, every one and unchanged, and
// these about the four fields v3 adds:
//
//   - share_audio is off in starting and in the final phases: nothing is
//     published before the session has joined or after it has ended;
//   - share_audio_problem is there exactly when share_audio is unavailable,
//     and is then a problem other than no_sources; the one exception is
//     no_sources, which goes with on and with nothing else;
//   - share_audio_sources is 0 unless share_audio is on, and with on it is 0
//     exactly when the problem is no_sources: the two say the same thing and
//     must agree, as muted and its block reason must;
//   - share_audio_muted may be true whatever the rest says.
//
// The share-audio fields are independent of transmitting, blocked and capture.
// The processing flags agree because the echo module performs both jobs.
func (s VoiceControlStateV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(s.Schema, s.Kind, VoiceControlEventV4State, s.Seq); err != nil {
		return err
	}
	if err := s.v2().Validate(); err != nil {
		return err
	}
	if !s.ShareAudio.Valid() {
		return fmt.Errorf("schema: voice control state share_audio %q is not one of off, on, unavailable", s.ShareAudio)
	}
	if s.ShareAudioProblem != "" && !s.ShareAudioProblem.Valid() {
		return fmt.Errorf("schema: voice control state share_audio_problem %q is not recognized", s.ShareAudioProblem)
	}
	if s.ShareAudioSources < 0 || s.ShareAudioSources > VoiceControlMaxShareAudioSources {
		return fmt.Errorf("schema: voice control state share_audio_sources must be 0 to %d", VoiceControlMaxShareAudioSources)
	}
	if s.ShareAudio != VoiceControlShareAudioOff &&
		(s.Phase == VoiceControlPhaseStarting || s.Phase.Final()) {
		return fmt.Errorf("schema: voice control state in phase %q must have share_audio off", s.Phase)
	}
	switch s.ShareAudio {
	case VoiceControlShareAudioOff:
		if s.ShareAudioProblem != "" {
			return errors.New("schema: voice control state with share_audio off must have no share_audio_problem")
		}
	case VoiceControlShareAudioUnavailable:
		if s.ShareAudioProblem == "" || s.ShareAudioProblem == VoiceControlShareAudioProblemNoSources {
			return errors.New("schema: voice control state with share_audio unavailable needs a share_audio_problem other than no_sources")
		}
	case VoiceControlShareAudioOn:
		if s.ShareAudioProblem != "" && s.ShareAudioProblem != VoiceControlShareAudioProblemNoSources {
			return errors.New("schema: voice control state with share_audio on can have no share_audio_problem but no_sources")
		}
		if (s.ShareAudioProblem == VoiceControlShareAudioProblemNoSources) != (s.ShareAudioSources == 0) {
			return errors.New("schema: voice control state share_audio_problem no_sources must agree with share_audio_sources being 0")
		}
	}
	if s.ShareAudio != VoiceControlShareAudioOn && s.ShareAudioSources != 0 {
		return errors.New("schema: voice control state share_audio_sources must be 0 unless share_audio is on")
	}
	if s.Processing.EchoCancellation != s.Processing.NoiseSuppression {
		return errors.New("schema: voice control state echo_cancellation and noise_suppression must agree")
	}
	return nil
}

// VoiceControlProtocolErrorV4 is VoiceControlProtocolErrorV1 in v4.
type VoiceControlProtocolErrorV4 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlEventKindV4 `json:"kind"`
	Seq    int64                   `json:"seq"`
	Code   VoiceControlErrorCode   `json:"code"`
}

func (VoiceControlProtocolErrorV4) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlProtocolErrorV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(e.Schema, e.Kind, VoiceControlEventV4ProtocolError, e.Seq); err != nil {
		return err
	}
	if !e.Code.Fatal() {
		return fmt.Errorf("schema: voice control protocol_error code %q is not one that ends the session", e.Code)
	}
	return nil
}

// VoiceControlLevelsV4 reports microphone diagnostics. While enabled, the
// helper sends these events at least 100 ms apart. Levels and thresholds are
// integer dBFS, with the measured level clamped to -96 through 0.
type VoiceControlLevelsV4 struct {
	Schema           string                  `json:"schema"`
	Kind             VoiceControlEventKindV4 `json:"kind"`
	Seq              int64                   `json:"seq"`
	LevelDB          int64                   `json:"level_db"`
	OpenThresholdDB  int64                   `json:"open_threshold_db"`
	CloseThresholdDB int64                   `json:"close_threshold_db"`
	GateOpen         bool                    `json:"gate_open"`
}

func (VoiceControlLevelsV4) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlLevelsV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(e.Schema, e.Kind, VoiceControlEventV4Levels, e.Seq); err != nil {
		return err
	}
	if e.LevelDB < VoiceControlV4MinLevelDB || e.LevelDB > 0 {
		return fmt.Errorf("schema: voice control levels level_db must be %d to 0", VoiceControlV4MinLevelDB)
	}
	return nil
}

// VoiceControlGateWhyV4 is the reason reported for a gate transition.
type VoiceControlGateWhyV4 string

// Gate transition reasons of v4.
const (
	VoiceControlGateWhyV4Sound     VoiceControlGateWhyV4 = "sound"
	VoiceControlGateWhyV4HangEnded VoiceControlGateWhyV4 = "hang_ended"
	VoiceControlGateWhyV4Mute      VoiceControlGateWhyV4 = "mute"
	VoiceControlGateWhyV4Lock      VoiceControlGateWhyV4 = "lock"
	VoiceControlGateWhyV4Limit     VoiceControlGateWhyV4 = "limit"
)

// Valid reports whether why is a gate transition reason of v4.
func (why VoiceControlGateWhyV4) Valid() bool {
	switch why {
	case VoiceControlGateWhyV4Sound, VoiceControlGateWhyV4HangEnded, VoiceControlGateWhyV4Mute,
		VoiceControlGateWhyV4Lock, VoiceControlGateWhyV4Limit:
		return true
	default:
		return false
	}
}

// VoiceControlGateEventV4 reports each open or close while diagnostics are
// enabled. AtMS is the time of the transition in milliseconds.
type VoiceControlGateEventV4 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlEventKindV4 `json:"kind"`
	Seq    int64                   `json:"seq"`
	AtMS   int64                   `json:"at_ms"`
	Open   bool                    `json:"open"`
	Why    VoiceControlGateWhyV4   `json:"why"`
}

func (VoiceControlGateEventV4) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlGateEventV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(e.Schema, e.Kind, VoiceControlEventV4GateEvent, e.Seq); err != nil {
		return err
	}
	if e.AtMS < 0 || e.AtMS > VoiceControlMaxID {
		return errors.New("schema: voice control gate_event at_ms must be a nonnegative safe integer")
	}
	if !e.Why.Valid() {
		return errors.New("schema: voice control gate_event why is not recognized")
	}
	return nil
}

// VoiceControlCallQualityV4 is one participant's sample, keyed by the
// principal ID in the owner's roster. Each metric is omitted when unavailable.
type VoiceControlCallQualityV4 struct {
	Schema          string                  `json:"schema"`
	Kind            VoiceControlEventKindV4 `json:"kind"`
	Seq             int64                   `json:"seq"`
	PrincipalID     int64                   `json:"principal_id"`
	LossBasisPoints *int64                  `json:"loss_basis_points,omitempty"` // From packet counter deltas.
	JitterUS        *int64                  `json:"jitter_us,omitempty"`
	RTTUS           *int64                  `json:"rtt_us,omitempty"` // Round-trip time to the SFU.
}

func (VoiceControlCallQualityV4) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlCallQualityV4) Validate() error {
	if err := validateVoiceControlEventHeadV4(e.Schema, e.Kind, VoiceControlEventV4CallQuality, e.Seq); err != nil {
		return err
	}
	if e.PrincipalID <= 0 {
		return errors.New("schema: voice control call_quality principal_id must be positive")
	}
	if e.LossBasisPoints != nil && (*e.LossBasisPoints < 0 || *e.LossBasisPoints > VoiceControlV4MaxLossBasisPoints) {
		return fmt.Errorf("schema: voice control call_quality loss_basis_points must be 0 to %d", VoiceControlV4MaxLossBasisPoints)
	}
	if e.JitterUS != nil && (*e.JitterUS < 0 || *e.JitterUS > VoiceControlMaxID) {
		return errors.New("schema: voice control call_quality jitter_us must be a nonnegative safe integer")
	}
	if e.RTTUS != nil && (*e.RTTUS < 0 || *e.RTTUS > VoiceControlMaxID) {
		return errors.New("schema: voice control call_quality rtt_us must be a nonnegative safe integer")
	}
	return nil
}

// validateVoiceControlCommandHeadV4 checks the head of a v4 command: its
// schema, its kind and its id.
func validateVoiceControlCommandHeadV4(schema string, kind, want VoiceControlCommandKindV4, id int64) error {
	if schema != VoiceControlSchemaV4 {
		return fmt.Errorf("schema: voice control command schema must be %q, got %q", VoiceControlSchemaV4, schema)
	}
	if kind != want {
		return fmt.Errorf("schema: voice control command kind must be %q, got %q", want, kind)
	}
	return validateVoiceControlID(id)
}

// VoiceControlCommandV4 is one v4 command as ParseVoiceControlCommandV4 read
// it: exactly one field is set. It is not a wire shape.
type VoiceControlCommandV4 struct {
	Start             *VoiceControlStartV4
	SetMute           *VoiceControlSetMuteV4
	SetDeafen         *VoiceControlSetDeafenV4
	GetState          *VoiceControlGetStateV4
	Leave             *VoiceControlLeaveV4
	SetDevices        *VoiceControlSetDevicesV4
	SetTalkMode       *VoiceControlSetTalkModeV4
	SetShareAudio     *VoiceControlSetShareAudioV4
	SetShareAudioMute *VoiceControlSetShareAudioMuteV4
	SetDiagnostics    *VoiceControlSetDiagnosticsV4
	GetCallQuality    *VoiceControlGetCallQualityV4
}

// VoiceControlEventV4 is one v4 event as ParseVoiceControlEventV4 read it:
// Seq, and exactly one of the message fields, or none and Unknown set to the
// kind of an event this version does not know, which a reader skips. There is
// no hello: the hello is v1, read with ParseVoiceControlEvent before the
// version is chosen. It is not a wire shape.
type VoiceControlEventV4 struct {
	Seq           int64
	Ack           *VoiceControlAckV4
	State         *VoiceControlStateV4
	ProtocolError *VoiceControlProtocolErrorV4
	Levels        *VoiceControlLevelsV4
	GateEvent     *VoiceControlGateEventV4
	CallQuality   *VoiceControlCallQualityV4
	Unknown       VoiceControlEventKindV4
}

// ParseVoiceControlCommandV4 reads one v4 command line, without its line
// feed, with the framing rules and the order of refusals of
// ParseVoiceControlCommand. A v1, v2 or v3 line is unknown_schema: a helper
// that offers multiple versions reads its first line with the reader of the
// version that line names, and from then on with that reader alone.
//
// The optional fields have the rule of every optional string
// (fieldReader.optWord): one that is absent is absent, and one that is
// present and empty is invalid. So a set_share_audio cannot say "no scope"
// with an empty one, and a stop that carries a scope or a name, empty or not,
// is invalid.
func ParseVoiceControlCommandV4(line []byte) (VoiceControlCommandV4, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlCommandV4{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV4); err != nil {
		return VoiceControlCommandV4{}, err
	}
	id, ok := fieldInt(fields, "id")
	if !ok || validateVoiceControlID(id) != nil {
		return VoiceControlCommandV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "id must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, _ := fieldString(fields, "kind")
	kind := VoiceControlCommandKindV4(kindText)
	if !kind.Valid() {
		return VoiceControlCommandV4{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV4)
	}
	r := fieldReader{fields: fields}
	var cmd VoiceControlCommandV4
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlCommandV4Start:
		s := &VoiceControlStartV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		s.Server = r.str("server")
		s.Channel = r.optWord("channel")
		s.Call = r.optWord("call")
		s.PrincipalID = r.int("principal_id")
		s.Credential = VoiceControlCredential(r.str("credential"))
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		cmd.Start, msg = s, s
	case VoiceControlCommandV4SetMute:
		c := &VoiceControlSetMuteV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetMute, msg = c, c
	case VoiceControlCommandV4SetDeafen:
		c := &VoiceControlSetDeafenV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id, Deafened: r.bool("deafened")}
		cmd.SetDeafen, msg = c, c
	case VoiceControlCommandV4GetState:
		c := &VoiceControlGetStateV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		cmd.GetState, msg = c, c
	case VoiceControlCommandV4Leave:
		c := &VoiceControlLeaveV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		cmd.Leave, msg = c, c
	case VoiceControlCommandV4SetDevices:
		c := &VoiceControlSetDevicesV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		c.Input = r.optWord("input")
		c.Output = r.optWord("output")
		cmd.SetDevices, msg = c, c
	case VoiceControlCommandV4SetTalkMode:
		c := &VoiceControlSetTalkModeV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		c.TalkMode = VoiceControlTalkMode(r.str("talk_mode"))
		cmd.SetTalkMode, msg = c, c
	case VoiceControlCommandV4SetShareAudio:
		c := &VoiceControlSetShareAudioV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		c.Capturing = r.bool("capturing")
		c.Scope = VoiceShareAudioScope(r.optWord("scope"))
		c.ApplicationName = r.optWord("application_name")
		cmd.SetShareAudio, msg = c, c
	case VoiceControlCommandV4SetShareAudioMute:
		c := &VoiceControlSetShareAudioMuteV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetShareAudioMute, msg = c, c
	case VoiceControlCommandV4SetDiagnostics:
		c := &VoiceControlSetDiagnosticsV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id, On: r.bool("on")}
		cmd.SetDiagnostics, msg = c, c
	case VoiceControlCommandV4GetCallQuality:
		c := &VoiceControlGetCallQualityV4{Schema: VoiceControlSchemaV4, Kind: kind, ID: id}
		cmd.GetCallQuality, msg = c, c
	default:
		// Valid lists v4's kinds in full, so this is unreachable unless the
		// two fall out of step; refuse rather than read a nil message.
		return VoiceControlCommandV4{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV4)
	}
	if r.missing != "" {
		return VoiceControlCommandV4{}, voiceControlErr(VoiceControlErrorInvalid, id, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlCommandV4{}, &VoiceControlError{Code: VoiceControlErrorInvalid, ID: id, detail: err.Error()}
	}
	return cmd, nil
}

// Kind is the kind of the command held.
func (c VoiceControlCommandV4) Kind() VoiceControlCommandKindV4 {
	switch {
	case c.Start != nil:
		return VoiceControlCommandV4Start
	case c.SetMute != nil:
		return VoiceControlCommandV4SetMute
	case c.SetDeafen != nil:
		return VoiceControlCommandV4SetDeafen
	case c.GetState != nil:
		return VoiceControlCommandV4GetState
	case c.Leave != nil:
		return VoiceControlCommandV4Leave
	case c.SetDevices != nil:
		return VoiceControlCommandV4SetDevices
	case c.SetTalkMode != nil:
		return VoiceControlCommandV4SetTalkMode
	case c.SetShareAudio != nil:
		return VoiceControlCommandV4SetShareAudio
	case c.SetShareAudioMute != nil:
		return VoiceControlCommandV4SetShareAudioMute
	case c.SetDiagnostics != nil:
		return VoiceControlCommandV4SetDiagnostics
	case c.GetCallQuality != nil:
		return VoiceControlCommandV4GetCallQuality
	}
	return ""
}

// ID is the id of the command held, or 0 if none is.
func (c VoiceControlCommandV4) ID() int64 {
	switch {
	case c.Start != nil:
		return c.Start.ID
	case c.SetMute != nil:
		return c.SetMute.ID
	case c.SetDeafen != nil:
		return c.SetDeafen.ID
	case c.GetState != nil:
		return c.GetState.ID
	case c.Leave != nil:
		return c.Leave.ID
	case c.SetDevices != nil:
		return c.SetDevices.ID
	case c.SetTalkMode != nil:
		return c.SetTalkMode.ID
	case c.SetShareAudio != nil:
		return c.SetShareAudio.ID
	case c.SetShareAudioMute != nil:
		return c.SetShareAudioMute.ID
	case c.SetDiagnostics != nil:
		return c.SetDiagnostics.ID
	case c.GetCallQuality != nil:
		return c.GetCallQuality.ID
	}
	return 0
}

// ParseVoiceControlEventV4 reads one v4 event line, without its line feed,
// with the framing rules and the order of refusals of ParseVoiceControlEvent.
// A v1, v2 or v3 line is unknown_schema. A hello in v4 is invalid: the hello is
// written in v1 only, and a reader of v4 has already read it. An event of a
// kind this version does not know comes back with Seq and Unknown set, to
// skip.
//
// The three always-written share-audio fields and processing are required
// on state. share_audio_problem is an optional word, and one that is present
// and empty is invalid.
func ParseVoiceControlEventV4(line []byte) (VoiceControlEventV4, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlEventV4{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV4); err != nil {
		return VoiceControlEventV4{}, err
	}
	seq, ok := fieldInt(fields, "seq")
	if !ok || seq < 1 || seq > VoiceControlMaxID {
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "seq must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, ok := fieldString(fields, "kind")
	if !ok || kindText == "" {
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "kind must be a string")
	}
	kind := VoiceControlEventKindV4(kindText)
	ev := VoiceControlEventV4{Seq: seq}
	r := fieldReader{fields: fields}
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlEventKindV4(VoiceControlEventHello):
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "a hello is written in %s only", VoiceControlSchemaV1)
	case VoiceControlEventV4Ack:
		a := &VoiceControlAckV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		a.ID = r.int("id")
		a.OK = r.bool("ok")
		a.Error = VoiceControlErrorCode(r.optWord("error"))
		ev.Ack, msg = a, a
	case VoiceControlEventV4State:
		s := &VoiceControlStateV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		s.Phase = VoiceControlPhase(r.str("phase"))
		s.Reason = VoiceControlReason(r.optWord("reason"))
		s.RetryInMS = r.optInt("retry_in_ms")
		s.Capture = VoiceControlCapture(r.str("capture"))
		s.Playback = VoiceControlPlayback(r.str("playback"))
		s.Transmitting = r.bool("transmitting")
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		for _, b := range r.strs("blocked") {
			s.Blocked = append(s.Blocked, VoiceControlBlock(b))
		}
		if s.Blocked == nil && r.missing == "" {
			s.Blocked = []VoiceControlBlock{}
		}
		s.TalkMode = VoiceControlTalkMode(r.str("talk_mode"))
		s.PTT = VoiceControlPTT(r.str("ptt"))
		s.PTTProblem = VoiceControlPTTProblem(r.optWord("ptt_problem"))
		s.ShareAudio = VoiceControlShareAudio(r.str("share_audio"))
		s.ShareAudioMuted = r.bool("share_audio_muted")
		s.ShareAudioProblem = VoiceControlShareAudioProblem(r.optWord("share_audio_problem"))
		s.ShareAudioSources = r.int("share_audio_sources")
		s.Processing = readVoiceControlProcessingV4(&r)
		ev.State, msg = s, s
	case VoiceControlEventV4ProtocolError:
		e := &VoiceControlProtocolErrorV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		e.Code = VoiceControlErrorCode(r.str("code"))
		ev.ProtocolError, msg = e, e
	case VoiceControlEventV4Levels:
		e := &VoiceControlLevelsV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		e.LevelDB = r.int("level_db")
		e.OpenThresholdDB = r.int("open_threshold_db")
		e.CloseThresholdDB = r.int("close_threshold_db")
		e.GateOpen = r.bool("gate_open")
		ev.Levels, msg = e, e
	case VoiceControlEventV4GateEvent:
		e := &VoiceControlGateEventV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		e.AtMS = r.int("at_ms")
		e.Open = r.bool("open")
		e.Why = VoiceControlGateWhyV4(r.str("why"))
		ev.GateEvent, msg = e, e
	case VoiceControlEventV4CallQuality:
		e := &VoiceControlCallQualityV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		e.PrincipalID = r.int("principal_id")
		e.LossBasisPoints = r.optInt("loss_basis_points")
		e.JitterUS = r.optInt("jitter_us")
		e.RTTUS = r.optInt("rtt_us")
		ev.CallQuality, msg = e, e
	default:
		ev.Unknown = kind
		return ev, nil
	}
	if r.missing != "" {
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlEventV4{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return ev, nil
}

func readVoiceControlProcessingV4(r *fieldReader) VoiceControlProcessingV4 {
	var raw json.RawMessage
	if !r.take("processing", &raw, true) {
		return VoiceControlProcessingV4{}
	}
	fields, err := decodeVoiceControlObject(raw)
	if err != nil {
		if r.missing == "" {
			r.missing = "processing"
		}
		return VoiceControlProcessingV4{}
	}
	nested := fieldReader{fields: fields}
	p := VoiceControlProcessingV4{
		EchoCancellation: nested.bool("echo_cancellation"),
		NoiseSuppression: nested.bool("noise_suppression"),
	}
	if nested.missing != "" && r.missing == "" {
		r.missing = "processing." + nested.missing
	}
	return p
}
