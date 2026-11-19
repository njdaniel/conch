package schema

import (
	"errors"
	"fmt"
	"slices"
)

// Version 3 of the managed voice-helper control protocol (issue #1851, parent
// #772; design note docs/design/linux-share-audio.md §7, decision D-2). A v3
// session is a v2 session plus the sound of a screen share: on Linux the
// helper, not the page, captures what the other applications play and
// publishes it, on the owner's command. v3 adds
//
//   - set_share_audio: capture and publish the share's sound, or stop;
//   - set_share_audio_mute: mute the published track, which stays published;
//   - four fields of the state that say what came of it: share_audio,
//     share_audio_muted, share_audio_problem and share_audio_sources.
//
// Why a version and not additions to v2: by the rule v2 was made under
// (voice_control_v2.go), a helper refuses a command of a kind it does not
// know, which the owner treats as a broken helper and stops. The hello is
// v1's, in its frozen shape, and now offers v1, v2 and v3; the owner picks one
// by the schema of its start, and the version is fixed for the session: a v1
// or a v2 line in a v3 session is unknown_schema, and a v3 line in a v1 or a
// v2 session is too.
//
// What v1 and v2 leave out, v3 leaves out too: no secret, no free text from
// the helper (the application name the owner sends is never repeated, and the
// state names no application and no stream, only how many), and no command
// that opens the transmit gate. The share's sound is not the microphone:
// neither command touches the gate, and transmitting does not depend on any
// of the four fields.
//
// A share-audio command that cannot be carried out is refused into the state,
// never by a protocol error: a scope this platform cannot capture, a session
// with no companion, a helper that is not connected. The command is valid, its
// ack is ok, and share_audio is unavailable with the reason in
// share_audio_problem. Only a line that breaks the shape is invalid.
//
// v1 (voice_control.go) and v2 (voice_control_v2.go) are published and do not
// change. That is why v3's command kinds are a vocabulary of their own
// (VoiceControlCommandKindV3) and not two more values of
// VoiceControlCommandKind: that type is the kind field of every published v1
// and v2 command, here and in the generated TypeScript, and a value added to
// it would be a value every one of those published types newly accepts. The
// words on the wire are the same either way. The events' kinds, the error
// codes and every vocabulary of the state are shared with v1 and v2 as they
// stand; v3 lines are read with ParseVoiceControlCommandV3 and
// ParseVoiceControlEventV3, by the framing rules of the other two. The golden
// and conformance fixtures are in testdata/voice-control-v3/.

// VoiceControlSchemaV3 is the versioned name every v3 message but the hello
// carries in its schema field. The hello is always v1 (VoiceControlHelloV1).
const VoiceControlSchemaV3 = "conch.voice_control.v3"

// The bounds v3 adds to the ones in voice_control.go.
const (
	// VoiceControlMaxApplicationNameBytes is the longest application_name a
	// set_share_audio may carry.
	VoiceControlMaxApplicationNameBytes = 128
	// VoiceControlMaxShareAudioSources is the largest share_audio_sources a
	// state may carry.
	VoiceControlMaxShareAudioSources = 1024
)

// VoiceControlCommandKindV3 is what a v3 command asks for. The first seven
// words are v2's, with v2's meaning; the last two are v3's own. It is listed
// in full, as each version's kinds are (VoiceControlCommandKind.KnownIn), and
// is a type of its own so that the published v1 and v2 commands, whose kind
// is a VoiceControlCommandKind, accept nothing they did not accept before.
type VoiceControlCommandKindV3 string

// Voice control command kinds of v3.
const (
	// VoiceControlCommandV3Start is the first command and the only one
	// before it: join this channel on this server as this principal.
	VoiceControlCommandV3Start VoiceControlCommandKindV3 = "start"
	// VoiceControlCommandV3SetMute sets whether the user is muted.
	VoiceControlCommandV3SetMute VoiceControlCommandKindV3 = "set_mute"
	// VoiceControlCommandV3SetDeafen sets whether the user is deafened.
	VoiceControlCommandV3SetDeafen VoiceControlCommandKindV3 = "set_deafen"
	// VoiceControlCommandV3GetState asks for a state snapshot now.
	VoiceControlCommandV3GetState VoiceControlCommandKindV3 = "get_state"
	// VoiceControlCommandV3Leave leaves voice; the helper then exits.
	VoiceControlCommandV3Leave VoiceControlCommandKindV3 = "leave"
	// VoiceControlCommandV3SetDevices names the microphone and speakers to
	// use from now on, without leaving.
	VoiceControlCommandV3SetDevices VoiceControlCommandKindV3 = "set_devices"
	// VoiceControlCommandV3SetTalkMode chooses what may open the gate: a
	// press, or the microphone's own sound.
	VoiceControlCommandV3SetTalkMode VoiceControlCommandKindV3 = "set_talk_mode"
	// VoiceControlCommandV3SetShareAudio captures and publishes the sound of
	// the owner's screen share, or stops.
	VoiceControlCommandV3SetShareAudio VoiceControlCommandKindV3 = "set_share_audio"
	// VoiceControlCommandV3SetShareAudioMute mutes the published share-audio
	// track, or unmutes it.
	VoiceControlCommandV3SetShareAudioMute VoiceControlCommandKindV3 = "set_share_audio_mute"
)

// Valid reports whether k is a command kind of v3. A kind of no version, or
// of a later one, is unknown_kind to the v3 reader.
func (k VoiceControlCommandKindV3) Valid() bool {
	switch k {
	case VoiceControlCommandV3Start, VoiceControlCommandV3SetMute, VoiceControlCommandV3SetDeafen,
		VoiceControlCommandV3GetState, VoiceControlCommandV3Leave,
		VoiceControlCommandV3SetDevices, VoiceControlCommandV3SetTalkMode,
		VoiceControlCommandV3SetShareAudio, VoiceControlCommandV3SetShareAudioMute:
		return true
	default:
		return false
	}
}

// VoiceControlShareAudio is whether the helper is publishing the sound of the
// owner's screen share.
type VoiceControlShareAudio string

// Voice control share-audio states.
const (
	// VoiceControlShareAudioOff: not asked for, or stopped. Always so before
	// the session has joined (starting) and after it has ended (stopped,
	// failed).
	VoiceControlShareAudioOff VoiceControlShareAudio = "off"
	// VoiceControlShareAudioOn: the captures are open and the track is
	// published. Muted is not off: the track stays published and is muted.
	VoiceControlShareAudioOn VoiceControlShareAudio = "on"
	// VoiceControlShareAudioUnavailable: asked for and not achievable now.
	// share_audio_problem says why. Nothing is captured in its place.
	VoiceControlShareAudioUnavailable VoiceControlShareAudio = "unavailable"
)

// Valid reports whether a is a recognized share-audio state.
func (a VoiceControlShareAudio) Valid() bool {
	switch a {
	case VoiceControlShareAudioOff, VoiceControlShareAudioOn, VoiceControlShareAudioUnavailable:
		return true
	default:
		return false
	}
}

// VoiceControlShareAudioProblem is why the share's sound is unavailable, or,
// for no_sources alone, a note beside a share-audio state of on.
type VoiceControlShareAudioProblem string

// Voice control share-audio problems.
const (
	// VoiceControlShareAudioProblemNoPipeWire: the sound system is not
	// reachable, or a capture could not be opened. The helper tries again
	// when the sound system reports a change.
	VoiceControlShareAudioProblemNoPipeWire VoiceControlShareAudioProblem = "no_pipewire"
	// VoiceControlShareAudioProblemNoProcessTree: the helper could not work
	// out which streams are the call's own, so it captures nothing rather
	// than everything.
	VoiceControlShareAudioProblemNoProcessTree VoiceControlShareAudioProblem = "no_process_tree"
	// VoiceControlShareAudioProblemPublishRefused: the media server refused
	// the track (the share's grant is not there yet), or the publish did not
	// resolve within the helper's own bound (see VoiceControlSetShareAudioV3).
	// The owner may send the command again.
	VoiceControlShareAudioProblemPublishRefused VoiceControlShareAudioProblem = "publish_refused"
	// VoiceControlShareAudioProblemUnsupportedScope: the scope asked for is
	// one this platform cannot capture (application, on Linux).
	VoiceControlShareAudioProblemUnsupportedScope VoiceControlShareAudioProblem = "unsupported_scope"
	// VoiceControlShareAudioProblemUnsupported: this session cannot carry a
	// share's sound at all: it has no companion, so there is no share of the
	// owner's for the sound to belong to.
	VoiceControlShareAudioProblemUnsupported VoiceControlShareAudioProblem = "unsupported"
	// VoiceControlShareAudioProblemNotConnected: asked for while the helper
	// is not connected.
	VoiceControlShareAudioProblemNotConnected VoiceControlShareAudioProblem = "not_connected"
	// VoiceControlShareAudioProblemNoSources: nothing that may be captured is
	// playing right now. Not a failure: share_audio is on, the track is
	// published and silent, and share_audio_sources is 0.
	VoiceControlShareAudioProblemNoSources VoiceControlShareAudioProblem = "no_sources"
)

// Valid reports whether p is a recognized share-audio problem.
func (p VoiceControlShareAudioProblem) Valid() bool {
	switch p {
	case VoiceControlShareAudioProblemNoPipeWire, VoiceControlShareAudioProblemNoProcessTree,
		VoiceControlShareAudioProblemPublishRefused, VoiceControlShareAudioProblemUnsupportedScope,
		VoiceControlShareAudioProblemUnsupported, VoiceControlShareAudioProblemNotConnected,
		VoiceControlShareAudioProblemNoSources:
		return true
	default:
		return false
	}
}

// VoiceControlStartV3 is the first command of a v3 session (owner to
// helper). Its fields are v1's and v2's: nothing about a share is carried, a
// share's sound starts and stops with each share, not with the session.
type VoiceControlStartV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
	// Server, Channel, PrincipalID and Credential are as in
	// VoiceControlStartV1.
	Server      string                 `json:"server"`
	Channel     string                 `json:"channel"`
	PrincipalID int64                  `json:"principal_id"`
	Credential  VoiceControlCredential `json:"credential"`
	// Muted and Deafened are whether to start so. Always written.
	Muted    bool `json:"muted"`
	Deafened bool `json:"deafened"`
}

func (VoiceControlStartV3) voiceControlMessage() {}

// Validate reports whether the start is well-formed, by v1's rules.
func (s VoiceControlStartV3) Validate() error {
	if err := validateVoiceControlCommandHeadV3(s.Schema, s.Kind, VoiceControlCommandV3Start, s.ID); err != nil {
		return err
	}
	if err := validateVoiceControlServer(s.Server); err != nil {
		return err
	}
	if err := validateVoiceControlChannel(s.Channel); err != nil {
		return err
	}
	if s.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice control start principal_id must be positive, got %d", s.PrincipalID)
	}
	if !s.Credential.Valid() {
		return fmt.Errorf("schema: voice control start credential %q is not one of stored, env", s.Credential)
	}
	return nil
}

// VoiceControlSetMuteV3 is VoiceControlSetMuteV1 in v3. It is the
// microphone's mute and nothing else: the share's sound has its own
// (VoiceControlSetShareAudioMuteV3).
type VoiceControlSetMuteV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
	Muted  bool                      `json:"muted"`
}

func (VoiceControlSetMuteV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetMuteV3) Validate() error {
	return validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetMute, c.ID)
}

// VoiceControlSetDeafenV3 is VoiceControlSetDeafenV1 in v3.
type VoiceControlSetDeafenV3 struct {
	Schema   string                    `json:"schema"`
	Kind     VoiceControlCommandKindV3 `json:"kind"`
	ID       int64                     `json:"id"`
	Deafened bool                      `json:"deafened"`
}

func (VoiceControlSetDeafenV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetDeafenV3) Validate() error {
	return validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetDeafen, c.ID)
}

// VoiceControlGetStateV3 is VoiceControlGetStateV1 in v3.
type VoiceControlGetStateV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
}

func (VoiceControlGetStateV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlGetStateV3) Validate() error {
	return validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3GetState, c.ID)
}

// VoiceControlLeaveV3 is VoiceControlLeaveV1 in v3. Leaving ends a share's
// sound with the rest of the session.
type VoiceControlLeaveV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
}

func (VoiceControlLeaveV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlLeaveV3) Validate() error {
	return validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3Leave, c.ID)
}

// VoiceControlSetDevicesV3 is VoiceControlSetDevicesV2 in v3, by the same
// rules. The devices are the microphone and the speakers: a share's sound has
// no device, its captures follow the applications that play.
type VoiceControlSetDevicesV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
	// Input and Output are as in VoiceControlSetDevicesV2: absent to leave a
	// side alone; an empty string is refused.
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

func (VoiceControlSetDevicesV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed: at least one side
// named, each under the name rule of VoiceControlSetDevicesV2.
func (c VoiceControlSetDevicesV3) Validate() error {
	if err := validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetDevices, c.ID); err != nil {
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

// VoiceControlSetTalkModeV3 is VoiceControlSetTalkModeV2 in v3.
type VoiceControlSetTalkModeV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
	// TalkMode is the mode wanted.
	TalkMode VoiceControlTalkMode `json:"talk_mode"`
}

func (VoiceControlSetTalkModeV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetTalkModeV3) Validate() error {
	if err := validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetTalkMode, c.ID); err != nil {
		return err
	}
	if !c.TalkMode.Valid() {
		return fmt.Errorf("schema: voice control set_talk_mode talk_mode %q is not one of push_to_talk, open", c.TalkMode)
	}
	return nil
}

// VoiceControlSetShareAudioV3 asks the helper to capture and publish the
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
type VoiceControlSetShareAudioV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
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

func (VoiceControlSetShareAudioV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed: with capturing, a
// known scope and an application name under its rule; without, neither. A
// scope that is known but that the helper's platform cannot capture is
// well-formed: the helper says so in the state.
func (c VoiceControlSetShareAudioV3) Validate() error {
	if err := validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetShareAudio, c.ID); err != nil {
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

// validateVoiceControlApplicationName is the rule of
// VoiceControlSetShareAudioV3.ApplicationName. Printable ASCII, as the
// hello's helper_version is: the same bytes are accepted by every reader with
// no table of characters to keep in step, and the name is the owner's own
// application's, which is such a name. It does not echo the name.
func validateVoiceControlApplicationName(name string) error {
	if name == "" || len(name) > VoiceControlMaxApplicationNameBytes {
		return fmt.Errorf("schema: voice control set_share_audio application_name must be 1 to %d bytes", VoiceControlMaxApplicationNameBytes)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c > 0x7e {
			return errors.New("schema: voice control set_share_audio application_name must be printable ASCII")
		}
	}
	return nil
}

// VoiceControlSetShareAudioMuteV3 mutes the published share-audio track, or
// unmutes it (owner to helper). A desired state, like set_mute, and kept
// whether or not a share's sound is being published: it applies to the track
// when there is one. The track stays published and the captures stay open;
// viewers see it muted. It is independent of set_mute (the microphone) and of
// set_deafen, and neither of those changes it.
type VoiceControlSetShareAudioMuteV3 struct {
	Schema string                    `json:"schema"`
	Kind   VoiceControlCommandKindV3 `json:"kind"`
	ID     int64                     `json:"id"`
	// Muted is the state wanted. Always written.
	Muted bool `json:"muted"`
}

func (VoiceControlSetShareAudioMuteV3) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetShareAudioMuteV3) Validate() error {
	return validateVoiceControlCommandHeadV3(c.Schema, c.Kind, VoiceControlCommandV3SetShareAudioMute, c.ID)
}

// VoiceControlAckV3 is VoiceControlAckV1 in v3. The refusals are v2's.
// invalid is also the answer to a set_share_audio that breaks its shape, and
// stopping to set_share_audio and set_share_audio_mute after leaving began,
// as it is to set_mute. A share-audio command the helper cannot carry out is
// not refused here: its ack is ok and the state says why not.
type VoiceControlAckV3 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	ID     int64                 `json:"id"`
	OK     bool                  `json:"ok"`
	Error  VoiceControlErrorCode `json:"error,omitempty"`
}

func (VoiceControlAckV3) voiceControlMessage() {}

// Validate reports whether the ack is well-formed.
func (a VoiceControlAckV3) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV3, a.Schema, a.Kind, VoiceControlEventAck, a.Seq); err != nil {
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

// VoiceControlStateV3 is a whole-state snapshot (helper to owner): every
// field of VoiceControlStateV2, unchanged and by the same rules, and four
// about the sound of the owner's screen share. It names no device, no
// application and no stream.
type VoiceControlStateV3 struct {
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

func (VoiceControlStateV3) voiceControlMessage() {}

// Blocks reports whether b is among the state's block reasons.
func (s VoiceControlStateV3) Blocks(b VoiceControlBlock) bool {
	return slices.Contains(s.Blocked, b)
}

// v2 is the snapshot's v2 fields as a v2 state, for the rules the two
// versions share.
func (s VoiceControlStateV3) v2() VoiceControlStateV2 {
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
func (s VoiceControlStateV3) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV3, s.Schema, s.Kind, VoiceControlEventState, s.Seq); err != nil {
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

// VoiceControlProtocolErrorV3 is VoiceControlProtocolErrorV1 in v3.
type VoiceControlProtocolErrorV3 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	Code   VoiceControlErrorCode `json:"code"`
}

func (VoiceControlProtocolErrorV3) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlProtocolErrorV3) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV3, e.Schema, e.Kind, VoiceControlEventProtocolError, e.Seq); err != nil {
		return err
	}
	if !e.Code.Fatal() {
		return fmt.Errorf("schema: voice control protocol_error code %q is not one that ends the session", e.Code)
	}
	return nil
}

// validateVoiceControlCommandHeadV3 checks the head of a v3 command: its
// schema, its kind and its id.
func validateVoiceControlCommandHeadV3(schema string, kind, want VoiceControlCommandKindV3, id int64) error {
	if schema != VoiceControlSchemaV3 {
		return fmt.Errorf("schema: voice control command schema must be %q, got %q", VoiceControlSchemaV3, schema)
	}
	if kind != want {
		return fmt.Errorf("schema: voice control command kind must be %q, got %q", want, kind)
	}
	return validateVoiceControlID(id)
}

// VoiceControlCommandV3 is one v3 command as ParseVoiceControlCommandV3 read
// it: exactly one field is set. It is not a wire shape.
type VoiceControlCommandV3 struct {
	Start             *VoiceControlStartV3
	SetMute           *VoiceControlSetMuteV3
	SetDeafen         *VoiceControlSetDeafenV3
	GetState          *VoiceControlGetStateV3
	Leave             *VoiceControlLeaveV3
	SetDevices        *VoiceControlSetDevicesV3
	SetTalkMode       *VoiceControlSetTalkModeV3
	SetShareAudio     *VoiceControlSetShareAudioV3
	SetShareAudioMute *VoiceControlSetShareAudioMuteV3
}

// VoiceControlEventV3 is one v3 event as ParseVoiceControlEventV3 read it:
// Seq, and exactly one of the message fields, or none and Unknown set to the
// kind of an event this version does not know, which a reader skips. There is
// no hello: the hello is v1, read with ParseVoiceControlEvent before the
// version is chosen. It is not a wire shape.
type VoiceControlEventV3 struct {
	Seq           int64
	Ack           *VoiceControlAckV3
	State         *VoiceControlStateV3
	ProtocolError *VoiceControlProtocolErrorV3
	Unknown       VoiceControlEventKind
}

// ParseVoiceControlCommandV3 reads one v3 command line, without its line
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
func ParseVoiceControlCommandV3(line []byte) (VoiceControlCommandV3, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlCommandV3{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV3); err != nil {
		return VoiceControlCommandV3{}, err
	}
	id, ok := fieldInt(fields, "id")
	if !ok || validateVoiceControlID(id) != nil {
		return VoiceControlCommandV3{}, voiceControlErr(VoiceControlErrorInvalid, 0, "id must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, _ := fieldString(fields, "kind")
	kind := VoiceControlCommandKindV3(kindText)
	if !kind.Valid() {
		return VoiceControlCommandV3{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV3)
	}
	r := fieldReader{fields: fields}
	var cmd VoiceControlCommandV3
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlCommandV3Start:
		s := &VoiceControlStartV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		s.Server = r.str("server")
		s.Channel = r.str("channel")
		s.PrincipalID = r.int("principal_id")
		s.Credential = VoiceControlCredential(r.str("credential"))
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		cmd.Start, msg = s, s
	case VoiceControlCommandV3SetMute:
		c := &VoiceControlSetMuteV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetMute, msg = c, c
	case VoiceControlCommandV3SetDeafen:
		c := &VoiceControlSetDeafenV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id, Deafened: r.bool("deafened")}
		cmd.SetDeafen, msg = c, c
	case VoiceControlCommandV3GetState:
		c := &VoiceControlGetStateV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		cmd.GetState, msg = c, c
	case VoiceControlCommandV3Leave:
		c := &VoiceControlLeaveV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		cmd.Leave, msg = c, c
	case VoiceControlCommandV3SetDevices:
		c := &VoiceControlSetDevicesV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		c.Input = r.optWord("input")
		c.Output = r.optWord("output")
		cmd.SetDevices, msg = c, c
	case VoiceControlCommandV3SetTalkMode:
		c := &VoiceControlSetTalkModeV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		c.TalkMode = VoiceControlTalkMode(r.str("talk_mode"))
		cmd.SetTalkMode, msg = c, c
	case VoiceControlCommandV3SetShareAudio:
		c := &VoiceControlSetShareAudioV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id}
		c.Capturing = r.bool("capturing")
		c.Scope = VoiceShareAudioScope(r.optWord("scope"))
		c.ApplicationName = r.optWord("application_name")
		cmd.SetShareAudio, msg = c, c
	case VoiceControlCommandV3SetShareAudioMute:
		c := &VoiceControlSetShareAudioMuteV3{Schema: VoiceControlSchemaV3, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetShareAudioMute, msg = c, c
	default:
		// Valid lists v3's kinds in full, so this is unreachable unless the
		// two fall out of step; refuse rather than read a nil message.
		return VoiceControlCommandV3{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV3)
	}
	if r.missing != "" {
		return VoiceControlCommandV3{}, voiceControlErr(VoiceControlErrorInvalid, id, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlCommandV3{}, &VoiceControlError{Code: VoiceControlErrorInvalid, ID: id, detail: err.Error()}
	}
	return cmd, nil
}

// Kind is the kind of the command held.
func (c VoiceControlCommandV3) Kind() VoiceControlCommandKindV3 {
	switch {
	case c.Start != nil:
		return VoiceControlCommandV3Start
	case c.SetMute != nil:
		return VoiceControlCommandV3SetMute
	case c.SetDeafen != nil:
		return VoiceControlCommandV3SetDeafen
	case c.GetState != nil:
		return VoiceControlCommandV3GetState
	case c.Leave != nil:
		return VoiceControlCommandV3Leave
	case c.SetDevices != nil:
		return VoiceControlCommandV3SetDevices
	case c.SetTalkMode != nil:
		return VoiceControlCommandV3SetTalkMode
	case c.SetShareAudio != nil:
		return VoiceControlCommandV3SetShareAudio
	case c.SetShareAudioMute != nil:
		return VoiceControlCommandV3SetShareAudioMute
	}
	return ""
}

// ID is the id of the command held, or 0 if none is.
func (c VoiceControlCommandV3) ID() int64 {
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

// ParseVoiceControlEventV3 reads one v3 event line, without its line feed,
// with the framing rules and the order of refusals of ParseVoiceControlEvent.
// A v1 or a v2 line is unknown_schema. A hello in v3 is invalid: the hello is
// written in v1 only, and a reader of v3 has already read it. An event of a
// kind this version does not know comes back with Seq and Unknown set, to
// skip.
//
// The three share-audio fields that are always written are required: a state
// without one is invalid, not read as off. share_audio_problem is an optional
// word, and one that is present and empty is invalid.
func ParseVoiceControlEventV3(line []byte) (VoiceControlEventV3, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlEventV3{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV3); err != nil {
		return VoiceControlEventV3{}, err
	}
	seq, ok := fieldInt(fields, "seq")
	if !ok || seq < 1 || seq > VoiceControlMaxID {
		return VoiceControlEventV3{}, voiceControlErr(VoiceControlErrorInvalid, 0, "seq must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, ok := fieldString(fields, "kind")
	if !ok || kindText == "" {
		return VoiceControlEventV3{}, voiceControlErr(VoiceControlErrorInvalid, 0, "kind must be a string")
	}
	kind := VoiceControlEventKind(kindText)
	ev := VoiceControlEventV3{Seq: seq}
	if !kind.Valid() {
		ev.Unknown = kind
		return ev, nil
	}
	r := fieldReader{fields: fields}
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlEventHello:
		return VoiceControlEventV3{}, voiceControlErr(VoiceControlErrorInvalid, 0, "a hello is written in %s only", VoiceControlSchemaV1)
	case VoiceControlEventAck:
		a := &VoiceControlAckV3{Schema: VoiceControlSchemaV3, Kind: kind, Seq: seq}
		a.ID = r.int("id")
		a.OK = r.bool("ok")
		a.Error = VoiceControlErrorCode(r.optWord("error"))
		ev.Ack, msg = a, a
	case VoiceControlEventState:
		s := &VoiceControlStateV3{Schema: VoiceControlSchemaV3, Kind: kind, Seq: seq}
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
		e := &VoiceControlProtocolErrorV3{Schema: VoiceControlSchemaV3, Kind: kind, Seq: seq}
		e.Code = VoiceControlErrorCode(r.str("code"))
		ev.ProtocolError, msg = e, e
	}
	if r.missing != "" {
		return VoiceControlEventV3{}, voiceControlErr(VoiceControlErrorInvalid, 0, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlEventV3{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return ev, nil
}
