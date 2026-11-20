package schema

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// VoiceControlV4RoomNamePattern reserves the room namespace on the control
// pipe. Conch generates room names with a configurable lower-case prefix
// ending in '-' and a 26-character lower-case base32 suffix (store/voice.go).
// This also covers rooms with a prefix from a previous configuration.
const VoiceControlV4RoomNamePattern = `^[a-z0-9-]{0,31}-[a-z2-7]{26}$`

var voiceControlV4RoomName = regexp.MustCompile(VoiceControlV4RoomNamePattern)

// Version 4 of the managed voice-helper control protocol (issue #2039, child
// of #2013; ADR-017 Decision 13, Nick's decision 12 of 2026-11-19). A v4
// session is a v3 session that may be about a private call instead of a
// channel. The desktop renderer never holds the voice grant, and the helper
// joins only through the server's session route; a private call has no
// channel, so v3's start, which names one, cannot tell the helper to join a
// call. v4 changes exactly that one thing:
//
//   - the start names its destination, which is either a channel (the
//     "channel" field, as in v3) or a private call attempt (the "call"
//     field), exactly one of the two. The call field is the opaque attempt id
//     of PrivateCallV1 (private_call.go): never a room name, a token or a
//     URL. The helper asks the server for its media grant for that attempt
//     (POST /v1/calls/{call}/session) with the credential the start names,
//     and the server answers only the side that attempt selected.
//
// Everything else is v3 carried over unchanged: the other six commands, the
// ack, the state with its four share-audio fields, the protocol error, the
// framing, the bounds and the rules of every field. A call carries no camera
// and no screen share, so a call session has no share for a share's sound to
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
// session is too. A channel start is valid in v4 and not in v3 only in that
// its schema is v4; a call start is valid in v4 alone.
//
// v1, v2 and v3 are published and do not change. The vocabularies v4 shares
// with them stay as they are: the share-audio words, the events' kinds, the
// error codes and every vocabulary of the state. Only the command kinds are a
// vocabulary of their own (VoiceControlCommandKindV4), for the reason v3's
// are (voice_control_v3.go). v4 lines are read with ParseVoiceControlCommandV4
// and ParseVoiceControlEventV4, by the framing rules of the others. The golden
// and conformance fixtures are in testdata/voice-control-v4/.

// VoiceControlSchemaV4 is the versioned name every v4 message but the hello
// carries in its schema field. The hello is always v1 (VoiceControlHelloV1).
const VoiceControlSchemaV4 = "conch.voice_control.v4"

// VoiceControlCommandKindV4 is what a v4 command asks for. They are v3's nine
// words, with v3's meaning; v4 adds no kind, only the start's destination. It is listed
// in full, as each version's kinds are (VoiceControlCommandKind.KnownIn), and
// is a type of its own so that the published v1 and v2 commands, whose kind
// is a VoiceControlCommandKind, accept nothing they did not accept before.
type VoiceControlCommandKindV4 string

// Voice control command kinds of v4.
const (
	// VoiceControlCommandV4Start is the first command and the only one
	// before it: join this channel on this server as this principal.
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
)

// Valid reports whether k is a command kind of v4. A kind of no version, or
// of a later one, is unknown_kind to the v4 reader.
func (k VoiceControlCommandKindV4) Valid() bool {
	switch k {
	case VoiceControlCommandV4Start, VoiceControlCommandV4SetMute, VoiceControlCommandV4SetDeafen,
		VoiceControlCommandV4GetState, VoiceControlCommandV4Leave,
		VoiceControlCommandV4SetDevices, VoiceControlCommandV4SetTalkMode,
		VoiceControlCommandV4SetShareAudio, VoiceControlCommandV4SetShareAudioMute:
		return true
	default:
		return false
	}
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
	// ID): 16 to MaxPrivateCallIDBytes URL-safe characters. Absent for a
	// channel. It is an id and never a room name, a token or a URL: the
	// helper gets the media grant from the server, for its own side only.
	// The owner takes this value from the server's attempt record. Validation
	// excludes Conch's room namespace. The session route verifies the attempt
	// exists and that the authenticated endpoint is authorized to join it.
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
		if len(s.Call) < MinPrivateCallIDBytes || !validDMToken(s.Call, MaxPrivateCallIDBytes) {
			return fmt.Errorf("schema: voice control start call must be %d to %d URL-safe characters", MinPrivateCallIDBytes, MaxPrivateCallIDBytes)
		}
		if voiceControlV4RoomName.MatchString(s.Call) {
			return errors.New("schema: voice control start call matches a reserved room name")
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

// VoiceControlAckV4 is VoiceControlAckV1 in v4. The refusals are v2's.
// invalid is also the answer to a set_share_audio that breaks its shape, and
// stopping to set_share_audio and set_share_audio_mute after leaving began,
// as it is to set_mute. A share-audio command the helper cannot carry out is
// not refused here: its ack is ok and the state says why not.
type VoiceControlAckV4 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	ID     int64                 `json:"id"`
	OK     bool                  `json:"ok"`
	Error  VoiceControlErrorCode `json:"error,omitempty"`
}

func (VoiceControlAckV4) voiceControlMessage() {}

// Validate reports whether the ack is well-formed.
func (a VoiceControlAckV4) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV4, a.Schema, a.Kind, VoiceControlEventAck, a.Seq); err != nil {
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
// field of VoiceControlStateV2, unchanged and by the same rules, and four
// about the sound of the owner's screen share. It names no device, no
// application and no stream.
type VoiceControlStateV4 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
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
		Schema: VoiceControlSchemaV2, Kind: s.Kind, Seq: s.Seq,
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
// Nothing ties the four to transmitting, to blocked or to capture: the
// share's sound is not the microphone.
func (s VoiceControlStateV4) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV4, s.Schema, s.Kind, VoiceControlEventState, s.Seq); err != nil {
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
	return nil
}

// VoiceControlProtocolErrorV4 is VoiceControlProtocolErrorV1 in v4.
type VoiceControlProtocolErrorV4 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	Code   VoiceControlErrorCode `json:"code"`
}

func (VoiceControlProtocolErrorV4) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlProtocolErrorV4) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV4, e.Schema, e.Kind, VoiceControlEventProtocolError, e.Seq); err != nil {
		return err
	}
	if !e.Code.Fatal() {
		return fmt.Errorf("schema: voice control protocol_error code %q is not one that ends the session", e.Code)
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
	Unknown       VoiceControlEventKind
}

// ParseVoiceControlCommandV4 reads one v4 command line, without its line
// feed, with the framing rules and the order of refusals of
// ParseVoiceControlCommand. A v1 or a v2 line is unknown_schema: a helper
// that offers the three versions reads its first line with the reader of the
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
// The three share-audio fields that are always written are required: a state
// without one is invalid, not read as off. share_audio_problem is an optional
// word, and one that is present and empty is invalid.
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
	kind := VoiceControlEventKind(kindText)
	ev := VoiceControlEventV4{Seq: seq}
	if !kind.Valid() {
		ev.Unknown = kind
		return ev, nil
	}
	r := fieldReader{fields: fields}
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlEventHello:
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "a hello is written in %s only", VoiceControlSchemaV1)
	case VoiceControlEventAck:
		a := &VoiceControlAckV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		a.ID = r.int("id")
		a.OK = r.bool("ok")
		a.Error = VoiceControlErrorCode(r.optWord("error"))
		ev.Ack, msg = a, a
	case VoiceControlEventState:
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
		ev.State, msg = s, s
	case VoiceControlEventProtocolError:
		e := &VoiceControlProtocolErrorV4{Schema: VoiceControlSchemaV4, Kind: kind, Seq: seq}
		e.Code = VoiceControlErrorCode(r.str("code"))
		ev.ProtocolError, msg = e, e
	}
	if r.missing != "" {
		return VoiceControlEventV4{}, voiceControlErr(VoiceControlErrorInvalid, 0, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlEventV4{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return ev, nil
}
