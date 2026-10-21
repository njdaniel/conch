package schema

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version 2 of the managed voice-helper control protocol (issue #653, parent
// #662; design note docs/design/tui-voice.md §20). It adds two things a v1
// state cannot say and a v1 helper cannot be asked for:
//
//   - switching the microphone and the speakers while in voice (set_devices),
//     with the speakers' state reported beside the microphone's (playback);
//   - an open microphone (set_talk_mode, talk_mode): the microphone's own
//     sound opens the transmit gate, level-triggered, instead of a press.
//
// Why a version and not additions to v1: a v1 reader refuses a state that
// transmits without the talk key read, and a v1 helper refuses a command of a
// kind it does not know, which the v1 owner treats as a broken helper and
// stops. The hello carries the versions a helper speaks, in v1's frozen shape,
// and the owner picks one by the schema of its start. A helper of this version
// offers both; a v1 owner uses v1 and sees today's behaviour. The version is
// fixed for the session: a line of the other version is unknown_schema.
//
// What v1 leaves out, v2 leaves out too: no secret, no free text from the
// helper (the device the owner named is never repeated: the owner knows what
// it asked for, and the state says whether capture and playback have a
// device), and no command that opens the gate. set_talk_mode chooses what may
// open it; in open mode that is the microphone's own sound, and nothing on the
// channel.
//
// v1 (voice_control.go) is published and does not change. The two versions
// share the hello, the vocabularies and the readers' framing rules; v2 lines
// are read with ParseVoiceControlCommandV2 and ParseVoiceControlEventV2. The
// golden and conformance fixtures are in testdata/voice-control-v2/.

// VoiceControlSchemaV2 is the versioned name every v2 message but the hello
// carries in its schema field. The hello is always v1 (VoiceControlHelloV1).
const VoiceControlSchemaV2 = "conch.voice_control.v2"

// VoiceControlTalkMode is what may open the transmit gate.
type VoiceControlTalkMode string

// Voice control talk modes.
const (
	// VoiceControlTalkModePushToTalk: a press of the talk key opens the gate
	// and its release shuts it. The default, and the mode of every v1
	// session.
	VoiceControlTalkModePushToTalk VoiceControlTalkMode = "push_to_talk"
	// VoiceControlTalkModeOpen: the microphone's own sound opens the gate
	// (one frame at or above the configured level after quiet) and quiet
	// shuts it. The talk key is ignored; the mute and deafen keys are not.
	VoiceControlTalkModeOpen VoiceControlTalkMode = "open"
)

// Valid reports whether m is a recognized talk mode.
func (m VoiceControlTalkMode) Valid() bool {
	switch m {
	case VoiceControlTalkModePushToTalk, VoiceControlTalkModeOpen:
		return true
	default:
		return false
	}
}

// VoiceControlPlayback is the state of the speakers' stream, as
// VoiceControlCapture is the microphone's.
type VoiceControlPlayback string

// Voice control playback states.
const (
	// VoiceControlPlaybackOff: no playback stream is open. Only before the
	// speakers are opened (starting) and after they are closed (stopped,
	// failed, and possibly stopping).
	VoiceControlPlaybackOff VoiceControlPlayback = "off"
	// VoiceControlPlaybackOn: the playback stream is open and has a device.
	// Deafened is not off: the stream stays open and plays silence.
	VoiceControlPlaybackOn VoiceControlPlayback = "on"
	// VoiceControlPlaybackUnavailable: the speakers named are not there, or
	// went away. Nothing is played in their place; playback resumes when they
	// appear. Transmitting does not need them, so nothing blocks.
	VoiceControlPlaybackUnavailable VoiceControlPlayback = "unavailable"
)

// Valid reports whether p is a recognized playback state.
func (p VoiceControlPlayback) Valid() bool {
	switch p {
	case VoiceControlPlaybackOff, VoiceControlPlaybackOn, VoiceControlPlaybackUnavailable:
		return true
	default:
		return false
	}
}

// VoiceControlStartV2 is the first command of a v2 session (owner to
// helper). Its fields are v1's: the talk mode is not carried, the helper
// reads it from voice.toml as it reads the devices, and the state reports it.
type VoiceControlStartV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
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

func (VoiceControlStartV2) voiceControlMessage() {}

// Validate reports whether the start is well-formed, by v1's rules.
func (s VoiceControlStartV2) Validate() error {
	if err := validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, s.Schema, s.Kind, VoiceControlCommandStart, s.ID); err != nil {
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

// VoiceControlSetMuteV2 is VoiceControlSetMuteV1 in v2.
type VoiceControlSetMuteV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
	Muted  bool                    `json:"muted"`
}

func (VoiceControlSetMuteV2) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetMuteV2) Validate() error {
	return validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandSetMute, c.ID)
}

// VoiceControlSetDeafenV2 is VoiceControlSetDeafenV1 in v2.
type VoiceControlSetDeafenV2 struct {
	Schema   string                  `json:"schema"`
	Kind     VoiceControlCommandKind `json:"kind"`
	ID       int64                   `json:"id"`
	Deafened bool                    `json:"deafened"`
}

func (VoiceControlSetDeafenV2) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetDeafenV2) Validate() error {
	return validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandSetDeafen, c.ID)
}

// VoiceControlGetStateV2 is VoiceControlGetStateV1 in v2.
type VoiceControlGetStateV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
}

func (VoiceControlGetStateV2) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlGetStateV2) Validate() error {
	return validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandGetState, c.ID)
}

// VoiceControlLeaveV2 is VoiceControlLeaveV1 in v2.
type VoiceControlLeaveV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
}

func (VoiceControlLeaveV2) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlLeaveV2) Validate() error {
	return validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandLeave, c.ID)
}

// VoiceControlSetDevicesV2 names the microphone, the speakers, or both, to
// use from now on (owner to helper). A desired state, like set_mute: the
// session targets the device named whether or not it is there, and the state
// says whether it is (capture, playback). A side left out is left alone. The
// helper writes nothing to voice.toml; the owner saves the choice itself.
//
// Switching the microphone during a transmission shuts the gate at once, with
// no release tail, and the transmission is reported stopped; a fresh press,
// or in open mode a fresh onset, is needed after. Switching the speakers does
// not touch the gate. A name that is the one in force changes nothing.
type VoiceControlSetDevicesV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
	// Input is the microphone: a PipeWire node.name under the rule of
	// VoiceDevice.Name (1 to VoiceDevicesMaxNameBytes bytes, no control
	// character, no quotation mark or backslash), or "default" for
	// PipeWire's default, followed as it changes. Absent to leave the
	// microphone alone; an empty string is refused.
	Input string `json:"input,omitempty"`
	// Output is the speakers, under the same rule.
	Output string `json:"output,omitempty"`
}

func (VoiceControlSetDevicesV2) voiceControlMessage() {}

// VoiceControlDeviceDefault is the name that means PipeWire's default device
// in set_devices, as it does in voice.toml.
const VoiceControlDeviceDefault = "default"

// Validate reports whether the command is well-formed: at least one side
// named, each under the name rule.
func (c VoiceControlSetDevicesV2) Validate() error {
	if err := validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandSetDevices, c.ID); err != nil {
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

// validateVoiceControlDeviceName is the rule of VoiceDevice.Name
// (validateVoiceDeviceName in voice_devices.go), kept equal to it by
// voice_control_v2_test.go: a name the device list shows is a name set_devices
// takes, and the other way round. It does not echo the name.
func validateVoiceControlDeviceName(field, name string) error {
	if name == "" || len(name) > VoiceDevicesMaxNameBytes {
		return fmt.Errorf("schema: voice control set_devices %s must be 1 to %d bytes", field, VoiceDevicesMaxNameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("schema: voice control set_devices %s must be valid UTF-8", field)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("schema: voice control set_devices %s must not contain control characters", field)
	}
	if strings.ContainsAny(name, `"\`) {
		return fmt.Errorf("schema: voice control set_devices %s must not contain a quotation mark or a backslash", field)
	}
	return nil
}

// VoiceControlSetTalkModeV2 chooses what may open the gate (owner to helper):
// a press, or the microphone's own sound. A desired state, like set_mute. A
// change shuts the gate at once, with no release tail: what was transmitting
// is reported stopped, and a fresh press or a fresh onset is needed after.
// Nothing here opens the gate. The helper writes nothing to voice.toml.
type VoiceControlSetTalkModeV2 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
	// TalkMode is the mode wanted.
	TalkMode VoiceControlTalkMode `json:"talk_mode"`
}

func (VoiceControlSetTalkModeV2) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetTalkModeV2) Validate() error {
	if err := validateVoiceControlCommandHeadIn(VoiceControlSchemaV2, c.Schema, c.Kind, VoiceControlCommandSetTalkMode, c.ID); err != nil {
		return err
	}
	if !c.TalkMode.Valid() {
		return fmt.Errorf("schema: voice control set_talk_mode talk_mode %q is not one of push_to_talk, open", c.TalkMode)
	}
	return nil
}

// VoiceControlAckV2 is VoiceControlAckV1 in v2. The refusals are v1's, with
// stopping now also the answer to set_devices and set_talk_mode after leaving
// began, and invalid to a device name that breaks the rule.
type VoiceControlAckV2 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	ID     int64                 `json:"id"`
	OK     bool                  `json:"ok"`
	Error  VoiceControlErrorCode `json:"error,omitempty"`
}

func (VoiceControlAckV2) voiceControlMessage() {}

// Validate reports whether the ack is well-formed.
func (a VoiceControlAckV2) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV2, a.Schema, a.Kind, VoiceControlEventAck, a.Seq); err != nil {
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

// VoiceControlStateV2 is a whole-state snapshot (helper to owner): v1's
// fields, the speakers' state beside the microphone's, and the talk mode in
// force. It names no device: the owner knows what it asked for.
type VoiceControlStateV2 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	// Phase, Reason and RetryInMS are as in VoiceControlStateV1.
	Phase     VoiceControlPhase  `json:"phase"`
	Reason    VoiceControlReason `json:"reason,omitempty"`
	RetryInMS *int64             `json:"retry_in_ms,omitempty"`
	// Capture is the state of the microphone stream. Always written.
	Capture VoiceControlCapture `json:"capture"`
	// Playback is the state of the speakers' stream. Always written.
	Playback VoiceControlPlayback `json:"playback"`
	// Transmitting is whether audio is leaving now, release tail included.
	Transmitting bool `json:"transmitting"`
	// Muted and Deafened are as the push-to-talk machine has them.
	Muted    bool `json:"muted"`
	Deafened bool `json:"deafened"`
	// Blocked is every reason a press, or in open mode an onset, would do
	// nothing now, in the order of the VoiceControlBlock constants. Always a
	// JSON array. key_device_lost is never among them in open mode, where
	// the talk key is ignored; max_transmit in open mode stands until quiet.
	Blocked []VoiceControlBlock `json:"blocked"`
	// TalkMode is the mode in force. Always written. push_to_talk before the
	// configuration is read (starting), which is also the mode that sends
	// less; open only once voice.toml, or set_talk_mode, said so.
	TalkMode VoiceControlTalkMode `json:"talk_mode"`
	// PTT and PTTProblem are whether the key device is read, as in v1. They
	// are reported in open mode too (the mute and deafen keys need it), but
	// transmitting does not depend on them there.
	PTT        VoiceControlPTT        `json:"ptt"`
	PTTProblem VoiceControlPTTProblem `json:"ptt_problem,omitempty"`
}

func (VoiceControlStateV2) voiceControlMessage() {}

// Blocks reports whether b is among the state's block reasons.
func (s VoiceControlStateV2) Blocks(b VoiceControlBlock) bool {
	return slices.Contains(s.Blocked, b)
}

// Validate reports whether the snapshot is well-formed and consistent. The
// rules are v1's, changed in three places and with two added:
//
//   - transmitting needs ptt ready only in push_to_talk; in open mode it
//     needs capture on and nothing blocking, whatever the key device;
//   - key_device_lost is among the blocks exactly when ptt is lost in
//     push_to_talk, and never in open mode, where nothing held can be lost;
//   - playback is off in starting and the final phases, on or unavailable in
//     connecting, connected and reconnecting, and anything in stopping;
//   - talk_mode is push_to_talk in starting: the configuration has not been
//     read, and a state never claims open mode before it has;
//   - everything else as VoiceControlStateV1.Validate.
func (s VoiceControlStateV2) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV2, s.Schema, s.Kind, VoiceControlEventState, s.Seq); err != nil {
		return err
	}
	if !s.Phase.Valid() {
		return fmt.Errorf("schema: voice control state phase %q is not recognized", s.Phase)
	}
	if !s.Capture.Valid() {
		return fmt.Errorf("schema: voice control state capture %q is not one of off, on, unavailable", s.Capture)
	}
	if !s.Playback.Valid() {
		return fmt.Errorf("schema: voice control state playback %q is not one of off, on, unavailable", s.Playback)
	}
	if !s.TalkMode.Valid() {
		return fmt.Errorf("schema: voice control state talk_mode %q is not one of push_to_talk, open", s.TalkMode)
	}
	if !s.PTT.Valid() {
		return fmt.Errorf("schema: voice control state ptt %q is not one of unknown, ready, not_configured, unavailable, lost", s.PTT)
	}
	switch s.Phase {
	case VoiceControlPhaseConnecting, VoiceControlPhaseConnected, VoiceControlPhaseReconnecting:
		if s.PTT == VoiceControlPTTUnknown {
			return fmt.Errorf("schema: voice control state in phase %q must say whether the key device is read", s.Phase)
		}
		if s.Playback == VoiceControlPlaybackOff {
			return fmt.Errorf("schema: voice control state in phase %q must have playback on or unavailable", s.Phase)
		}
	case VoiceControlPhaseStarting:
		if s.TalkMode != VoiceControlTalkModePushToTalk {
			return errors.New("schema: voice control state in phase starting must have talk_mode push_to_talk")
		}
	}
	if s.Playback != VoiceControlPlaybackOff &&
		(s.Phase == VoiceControlPhaseStarting || s.Phase.Final()) {
		return fmt.Errorf("schema: voice control state in phase %q must have playback off", s.Phase)
	}
	if err := validateVoiceControlBlocked(s.Blocked); err != nil {
		return err
	}
	if err := validateVoiceControlReason(s.Phase, s.Reason, s.RetryInMS); err != nil {
		return err
	}
	switch problem := s.PTT == VoiceControlPTTUnavailable || s.PTT == VoiceControlPTTLost; {
	case problem && !s.PTTProblem.Valid():
		return fmt.Errorf("schema: voice control state with ptt %q needs a recognized ptt_problem, got %q", s.PTT, s.PTTProblem)
	case !problem && s.PTTProblem != "":
		return fmt.Errorf("schema: voice control state with ptt %q must have no ptt_problem", s.PTT)
	}
	if s.Muted != s.Blocks(VoiceControlBlockMuted) {
		return errors.New("schema: voice control state muted must agree with the muted block reason")
	}
	if s.Deafened != s.Blocks(VoiceControlBlockDeafened) {
		return errors.New("schema: voice control state deafened must agree with the deafened block reason")
	}
	switch s.TalkMode {
	case VoiceControlTalkModePushToTalk:
		if (s.PTT == VoiceControlPTTLost) != s.Blocks(VoiceControlBlockKeyDeviceLost) {
			return errors.New("schema: voice control state ptt lost must agree with the key_device_lost block reason")
		}
	case VoiceControlTalkModeOpen:
		if s.Blocks(VoiceControlBlockKeyDeviceLost) {
			return errors.New("schema: voice control state in open mode cannot block with key_device_lost: the talk key is ignored")
		}
	}
	if s.Phase != VoiceControlPhaseConnected && !s.Blocks(VoiceControlBlockNotConnected) {
		return fmt.Errorf("schema: voice control state in phase %q must block with not_connected", s.Phase)
	}
	if s.Capture == VoiceControlCaptureOn && s.Blocks(VoiceControlBlockNoMicrophone) {
		return errors.New("schema: voice control state cannot have capture on and block with no_microphone")
	}
	if s.Capture == VoiceControlCaptureUnavailable && !s.Blocks(VoiceControlBlockNoMicrophone) {
		return errors.New("schema: voice control state with capture unavailable must block with no_microphone")
	}
	if s.Capture != VoiceControlCaptureOff &&
		(s.Phase == VoiceControlPhaseStarting || s.Phase.Final()) {
		return fmt.Errorf("schema: voice control state in phase %q must have capture off", s.Phase)
	}
	if s.Transmitting {
		switch {
		case len(s.Blocked) != 0:
			return errors.New("schema: voice control state cannot be transmitting while a reason blocks it")
		case s.Capture != VoiceControlCaptureOn:
			return errors.New("schema: voice control state cannot be transmitting without capture on")
		case s.TalkMode == VoiceControlTalkModePushToTalk && s.PTT != VoiceControlPTTReady:
			return errors.New("schema: voice control state cannot be transmitting in push_to_talk without ptt ready")
		}
	}
	return nil
}

// VoiceControlProtocolErrorV2 is VoiceControlProtocolErrorV1 in v2.
type VoiceControlProtocolErrorV2 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	Code   VoiceControlErrorCode `json:"code"`
}

func (VoiceControlProtocolErrorV2) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlProtocolErrorV2) Validate() error {
	if err := validateVoiceControlEventHeadIn(VoiceControlSchemaV2, e.Schema, e.Kind, VoiceControlEventProtocolError, e.Seq); err != nil {
		return err
	}
	if !e.Code.Fatal() {
		return fmt.Errorf("schema: voice control protocol_error code %q is not one that ends the session", e.Code)
	}
	return nil
}

// VoiceControlCommandV2 is one v2 command as ParseVoiceControlCommandV2 read
// it: exactly one field is set. It is not a wire shape.
type VoiceControlCommandV2 struct {
	Start       *VoiceControlStartV2
	SetMute     *VoiceControlSetMuteV2
	SetDeafen   *VoiceControlSetDeafenV2
	GetState    *VoiceControlGetStateV2
	Leave       *VoiceControlLeaveV2
	SetDevices  *VoiceControlSetDevicesV2
	SetTalkMode *VoiceControlSetTalkModeV2
}

// VoiceControlEventV2 is one v2 event as ParseVoiceControlEventV2 read it:
// Seq, and exactly one of the message fields, or none and Unknown set to the
// kind of an event this version does not know, which a reader skips. There is
// no hello: the hello is v1, read with ParseVoiceControlEvent before the
// version is chosen. It is not a wire shape.
type VoiceControlEventV2 struct {
	Seq           int64
	Ack           *VoiceControlAckV2
	State         *VoiceControlStateV2
	ProtocolError *VoiceControlProtocolErrorV2
	Unknown       VoiceControlEventKind
}

// ParseVoiceControlCommandV2 reads one v2 command line, without its line
// feed, with the framing rules and the order of refusals of
// ParseVoiceControlCommand. A v1 line is unknown_schema: a helper that offers
// both versions reads its first line with the reader of the version that
// line names, and from then on with that reader alone.
//
// The fields of set_devices have one rule of their own: a side that is absent
// is left alone, and a side that is present and empty is invalid, so that a
// reader cannot take an empty name for "leave it".
func ParseVoiceControlCommandV2(line []byte) (VoiceControlCommandV2, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlCommandV2{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV2); err != nil {
		return VoiceControlCommandV2{}, err
	}
	id, ok := fieldInt(fields, "id")
	if !ok || validateVoiceControlID(id) != nil {
		return VoiceControlCommandV2{}, voiceControlErr(VoiceControlErrorInvalid, 0, "id must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, _ := fieldString(fields, "kind")
	kind := VoiceControlCommandKind(kindText)
	if !kind.KnownIn(VoiceControlSchemaV2) {
		return VoiceControlCommandV2{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV2)
	}
	r := fieldReader{fields: fields}
	var cmd VoiceControlCommandV2
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlCommandStart:
		s := &VoiceControlStartV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id}
		s.Server = r.str("server")
		s.Channel = r.str("channel")
		s.PrincipalID = r.int("principal_id")
		s.Credential = VoiceControlCredential(r.str("credential"))
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		cmd.Start, msg = s, s
	case VoiceControlCommandSetMute:
		c := &VoiceControlSetMuteV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetMute, msg = c, c
	case VoiceControlCommandSetDeafen:
		c := &VoiceControlSetDeafenV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id, Deafened: r.bool("deafened")}
		cmd.SetDeafen, msg = c, c
	case VoiceControlCommandGetState:
		c := &VoiceControlGetStateV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id}
		cmd.GetState, msg = c, c
	case VoiceControlCommandLeave:
		c := &VoiceControlLeaveV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id}
		cmd.Leave, msg = c, c
	case VoiceControlCommandSetDevices:
		c := &VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id}
		c.Input = r.optName("input")
		c.Output = r.optName("output")
		cmd.SetDevices, msg = c, c
	case VoiceControlCommandSetTalkMode:
		c := &VoiceControlSetTalkModeV2{Schema: VoiceControlSchemaV2, Kind: kind, ID: id}
		c.TalkMode = VoiceControlTalkMode(r.str("talk_mode"))
		cmd.SetTalkMode, msg = c, c
	default:
		// KnownIn lists v2's kinds in full, so this is unreachable unless
		// the two fall out of step; refuse rather than read a nil message.
		return VoiceControlCommandV2{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV2)
	}
	if r.missing != "" {
		return VoiceControlCommandV2{}, voiceControlErr(VoiceControlErrorInvalid, id, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlCommandV2{}, &VoiceControlError{Code: VoiceControlErrorInvalid, ID: id, detail: err.Error()}
	}
	return cmd, nil
}

// optName reads an optional string that, when present, must not be empty: a
// device name whose absence means "leave it", where an empty string could be
// mistaken for the same.
func (r *fieldReader) optName(key string) string {
	s := r.optStr(key)
	if _, present := r.fields[key]; present && s == "" && r.missing == "" {
		r.missing = key
	}
	return s
}

// Kind is the kind of the command held.
func (c VoiceControlCommandV2) Kind() VoiceControlCommandKind {
	switch {
	case c.Start != nil:
		return VoiceControlCommandStart
	case c.SetMute != nil:
		return VoiceControlCommandSetMute
	case c.SetDeafen != nil:
		return VoiceControlCommandSetDeafen
	case c.GetState != nil:
		return VoiceControlCommandGetState
	case c.Leave != nil:
		return VoiceControlCommandLeave
	case c.SetDevices != nil:
		return VoiceControlCommandSetDevices
	case c.SetTalkMode != nil:
		return VoiceControlCommandSetTalkMode
	}
	return ""
}

// ID is the id of the command held, or 0 if none is.
func (c VoiceControlCommandV2) ID() int64 {
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
	}
	return 0
}

// ParseVoiceControlEventV2 reads one v2 event line, without its line feed,
// with the framing rules and the order of refusals of ParseVoiceControlEvent.
// A v1 line is unknown_schema. A hello in v2 is invalid: the hello is written
// in v1 only, and a reader of v2 has already read it. An event of a kind this
// version does not know comes back with Seq and Unknown set, to skip.
func ParseVoiceControlEventV2(line []byte) (VoiceControlEventV2, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlEventV2{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlSchemaV2); err != nil {
		return VoiceControlEventV2{}, err
	}
	seq, ok := fieldInt(fields, "seq")
	if !ok || seq < 1 || seq > VoiceControlMaxID {
		return VoiceControlEventV2{}, voiceControlErr(VoiceControlErrorInvalid, 0, "seq must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, ok := fieldString(fields, "kind")
	if !ok || kindText == "" {
		return VoiceControlEventV2{}, voiceControlErr(VoiceControlErrorInvalid, 0, "kind must be a string")
	}
	kind := VoiceControlEventKind(kindText)
	ev := VoiceControlEventV2{Seq: seq}
	if !kind.Valid() {
		ev.Unknown = kind
		return ev, nil
	}
	r := fieldReader{fields: fields}
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlEventHello:
		return VoiceControlEventV2{}, voiceControlErr(VoiceControlErrorInvalid, 0, "a hello is written in %s only", VoiceControlSchemaV1)
	case VoiceControlEventAck:
		a := &VoiceControlAckV2{Schema: VoiceControlSchemaV2, Kind: kind, Seq: seq}
		a.ID = r.int("id")
		a.OK = r.bool("ok")
		a.Error = VoiceControlErrorCode(r.optStr("error"))
		ev.Ack, msg = a, a
	case VoiceControlEventState:
		s := &VoiceControlStateV2{Schema: VoiceControlSchemaV2, Kind: kind, Seq: seq}
		s.Phase = VoiceControlPhase(r.str("phase"))
		s.Reason = VoiceControlReason(r.optStr("reason"))
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
		s.PTTProblem = VoiceControlPTTProblem(r.optStr("ptt_problem"))
		ev.State, msg = s, s
	case VoiceControlEventProtocolError:
		e := &VoiceControlProtocolErrorV2{Schema: VoiceControlSchemaV2, Kind: kind, Seq: seq}
		e.Code = VoiceControlErrorCode(r.str("code"))
		ev.ProtocolError, msg = e, e
	}
	if r.missing != "" {
		return VoiceControlEventV2{}, voiceControlErr(VoiceControlErrorInvalid, 0, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlEventV2{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return ev, nil
}
