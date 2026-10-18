package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The managed voice-helper control protocol (issue #539, parent #537; design
// note docs/design/tui-voice.md). A native client (the TUI) starts
// `conch-voice managed` as a child process and drives it over two private,
// inherited pipes: commands go to the helper's standard input, and events
// come back on file descriptor 3, which nothing but the helper's protocol
// writer touches. There is no socket and no listener.
//
// Every message is one JSON object on one line, with a schema name and a
// kind. The helper speaks first (hello, naming the versions it offers), the
// owner answers with start (in the version it picked), and from then on each
// command carries an id that the helper acknowledges once. The helper reports
// what is really happening as whole-state snapshots (state), so a reader that
// missed one loses nothing.
//
// What the protocol leaves out is part of the contract:
//
//   - No secret crosses it in either direction. The login token travels the
//     way it always has (the stored login in the configuration directory, or
//     CONCH_TOKEN in the child's environment); join tokens and room names
//     stay inside the helper. voice_control_test.go asserts that no field of
//     these types could hold one.
//   - No free text crosses it from the helper. Every string an event carries
//     is a schema name, a closed vocabulary or the helper's version, which is
//     checked to be short printable ASCII. A reader can show any of it on a
//     terminal without escaping, and none of it repeats anything a command
//     or conchd said.
//   - No press or release. The talk key is read by the helper from the key
//     device; nothing on this channel can open the transmit gate.
//
// These are new types and change nothing published. Unlike the REST bodies
// this protocol is read with ParseVoiceControlCommand and
// ParseVoiceControlEvent, which apply the framing rules (size, one object,
// no duplicate keys, exact case-sensitive field names) the Rust side applies
// too, so the two languages accept and refuse the same lines. The golden and
// conformance fixtures are in testdata/voice-control/.

// VoiceControlSchemaV1 is the versioned name every v1 message carries in its
// schema field. A hello is always written in this version, whatever else the
// helper offers, so that an owner of any version can read it.
const VoiceControlSchemaV1 = "conch.voice_control.v1"

// The numbers both sides are held to. They are not wire shapes, but each side
// can only keep its part if the other keeps its own, so they live with the
// protocol, as the heartbeat's do. Times are in milliseconds so that the Rust
// copy is a plain integer too.
const (
	// VoiceControlSubcommand is the helper's command line, after the path of
	// the executable: `conch-voice managed`.
	VoiceControlSubcommand = "managed"
	// VoiceControlEventFD is the file descriptor the helper writes events on.
	VoiceControlEventFD = 3

	// VoiceControlMaxLineBytes is the longest line, in bytes and not counting
	// its line feed, either side writes or accepts. A longer one is refused
	// with too_long; its bytes are discarded as they are read, up to the line
	// feed, so a reader never holds more than this.
	VoiceControlMaxLineBytes = 4096
	// VoiceControlMaxID is the largest command id or event seq: 2^53-1, so
	// that any JSON reader holds it exactly.
	VoiceControlMaxID = 1<<53 - 1
	// VoiceControlMaxInFlight is the most commands the owner may have written
	// and not yet seen acknowledged.
	VoiceControlMaxInFlight = 8
	// VoiceControlEventQueueLines is the most events the helper holds for a
	// reader that is slow. Past it a state or protocol_error is dropped, never
	// waited for; the gap shows in seq. Acks, and the state that answers
	// get_state, wait in places of their own (docs/design/tui-voice.md §3).
	VoiceControlEventQueueLines = 256
	// VoiceControlMaxVersions is the most versions a hello may offer.
	VoiceControlMaxVersions = 8
	// VoiceControlMaxHelperVersionBytes is the longest helper_version.
	VoiceControlMaxHelperVersionBytes = 64
	// VoiceControlMaxServerBytes is the longest server address in a start.
	// With the longest channel, and every character of both written as a
	// two-byte escape, a start still fits in VoiceControlMaxLineBytes.
	VoiceControlMaxServerBytes = 1024
	// VoiceControlMaxChannelBytes is the longest channel name in a start:
	// MaxChannelNameLength characters of up to four bytes each.
	VoiceControlMaxChannelBytes = 4 * MaxChannelNameLength
	// VoiceControlMaxRetryInMS is the largest retry_in_ms a state may carry.
	VoiceControlMaxRetryInMS = 600_000

	// VoiceControlHelloTimeoutMS is how long the owner waits, from starting
	// the process, for its hello. Past it the process is killed; it can have
	// captured nothing, since it has not been told what to join.
	VoiceControlHelloTimeoutMS = 5_000
	// VoiceControlStartTimeoutMS is how long the helper waits, from writing
	// its hello, for start. Past it the helper fails with start_timeout and
	// exits, having opened no device.
	VoiceControlStartTimeoutMS = 10_000
	// VoiceControlAckTimeoutMS is how long the owner waits for the ack of a
	// command it wrote. Past it the helper is treated as broken and stopped.
	VoiceControlAckTimeoutMS = 5_000
	// VoiceControlOwnerPollMS is how often the helper checks that the process
	// that started it is still its parent.
	VoiceControlOwnerPollMS = 250
	// VoiceControlGateShutMS bounds the time from the helper noticing that
	// its owner is gone (or reading leave) to the last microphone frame
	// handed to the track.
	VoiceControlGateShutMS = 100
	// VoiceControlCaptureStopMS bounds the time from the same moment to the
	// microphone stream being closed and playback stopped.
	VoiceControlCaptureStopMS = 500
	// VoiceControlOwnerLossExitMS bounds the time from the helper noticing
	// that its owner is gone to the process having exited.
	VoiceControlOwnerLossExitMS = 3_000
	// VoiceControlLeaveExitMS bounds the time from the helper reading leave
	// to the process having exited, with the control pipe still open.
	VoiceControlLeaveExitMS = 5_000
	// VoiceControlStopTermAfterMS is when, counted from the start of a stop
	// (leave written and standard input closed), the owner sends SIGTERM to a
	// helper that has not exited.
	VoiceControlStopTermAfterMS = 4_000
	// VoiceControlStopKillAfterMS is when, counted from the same start, the
	// owner sends SIGKILL to a helper that has still not exited. The owner
	// then waits for it and reaps it.
	VoiceControlStopKillAfterMS = 5_000
)

// VoiceControlCommandKind is what a command asks for.
type VoiceControlCommandKind string

// Voice control command kinds.
const (
	// VoiceControlCommandStart is the first command and the only one before
	// it: join this channel on this server as this principal.
	VoiceControlCommandStart VoiceControlCommandKind = "start"
	// VoiceControlCommandSetMute sets whether the user is muted.
	VoiceControlCommandSetMute VoiceControlCommandKind = "set_mute"
	// VoiceControlCommandSetDeafen sets whether the user is deafened.
	VoiceControlCommandSetDeafen VoiceControlCommandKind = "set_deafen"
	// VoiceControlCommandGetState asks for a state snapshot now.
	VoiceControlCommandGetState VoiceControlCommandKind = "get_state"
	// VoiceControlCommandLeave leaves voice; the helper then exits.
	VoiceControlCommandLeave VoiceControlCommandKind = "leave"
)

// Valid reports whether k is a recognized command kind.
func (k VoiceControlCommandKind) Valid() bool {
	switch k {
	case VoiceControlCommandStart, VoiceControlCommandSetMute, VoiceControlCommandSetDeafen,
		VoiceControlCommandGetState, VoiceControlCommandLeave:
		return true
	default:
		return false
	}
}

// VoiceControlEventKind is what an event reports.
type VoiceControlEventKind string

// Voice control event kinds.
const (
	// VoiceControlEventHello is the helper's first line: who it is and which
	// versions it speaks.
	VoiceControlEventHello VoiceControlEventKind = "hello"
	// VoiceControlEventAck answers one command.
	VoiceControlEventAck VoiceControlEventKind = "ack"
	// VoiceControlEventState is a whole-state snapshot.
	VoiceControlEventState VoiceControlEventKind = "state"
	// VoiceControlEventProtocolError says the helper read a line it could not
	// act on and is stopping because of it.
	VoiceControlEventProtocolError VoiceControlEventKind = "protocol_error"
)

// Valid reports whether k is a recognized event kind. A reader ignores an
// event of a kind it does not know (ParseVoiceControlEvent).
func (k VoiceControlEventKind) Valid() bool {
	switch k {
	case VoiceControlEventHello, VoiceControlEventAck, VoiceControlEventState, VoiceControlEventProtocolError:
		return true
	default:
		return false
	}
}

// VoiceControlCredential says where the helper takes the login token from.
// The token itself never crosses the control channel.
type VoiceControlCredential string

// Voice control credential sources.
const (
	// VoiceControlCredentialStored is the login `conch login` stored for the
	// server in the configuration directory (CONCH_CONFIG_DIR). CONCH_TOKEN
	// must then be absent from the helper's environment.
	VoiceControlCredentialStored VoiceControlCredential = "stored"
	// VoiceControlCredentialEnv is CONCH_TOKEN in the helper's environment:
	// the owner was itself started with CONCH_TOKEN and passes the same value
	// on, by the mechanism that already existed.
	VoiceControlCredentialEnv VoiceControlCredential = "env"
)

// Valid reports whether c is a recognized credential source.
func (c VoiceControlCredential) Valid() bool {
	switch c {
	case VoiceControlCredentialStored, VoiceControlCredentialEnv:
		return true
	default:
		return false
	}
}

// VoiceControlPhase is where the helper is in its life.
type VoiceControlPhase string

// Voice control phases.
const (
	// VoiceControlPhaseStarting: start was accepted; the helper is reading
	// its configuration, checking the login and setting up audio. Nothing is
	// captured.
	VoiceControlPhaseStarting VoiceControlPhase = "starting"
	// VoiceControlPhaseConnecting: asking conchd for a session and joining
	// the room.
	VoiceControlPhaseConnecting VoiceControlPhase = "connecting"
	// VoiceControlPhaseConnected: in the room, with the track (if any)
	// published and muted. A press transmits unless something blocks it.
	VoiceControlPhaseConnected VoiceControlPhase = "connected"
	// VoiceControlPhaseReconnecting: not connected, and trying again by the
	// helper's own policy.
	VoiceControlPhaseReconnecting VoiceControlPhase = "reconnecting"
	// VoiceControlPhaseStopping: leaving. The gate is shut.
	VoiceControlPhaseStopping VoiceControlPhase = "stopping"
	// VoiceControlPhaseStopped: left, as asked or because the owner is gone.
	// Final; the helper exits 0.
	VoiceControlPhaseStopped VoiceControlPhase = "stopped"
	// VoiceControlPhaseFailed: stopped for a reason trying again by itself
	// cannot fix. Final; the helper exits 1.
	VoiceControlPhaseFailed VoiceControlPhase = "failed"
)

// Valid reports whether p is a recognized phase.
func (p VoiceControlPhase) Valid() bool {
	switch p {
	case VoiceControlPhaseStarting, VoiceControlPhaseConnecting, VoiceControlPhaseConnected,
		VoiceControlPhaseReconnecting, VoiceControlPhaseStopping, VoiceControlPhaseStopped,
		VoiceControlPhaseFailed:
		return true
	default:
		return false
	}
}

// Final reports whether p is the last phase a helper reports.
func (p VoiceControlPhase) Final() bool {
	return p == VoiceControlPhaseStopped || p == VoiceControlPhaseFailed
}

// VoiceControlReason is why the helper is in its phase. Which reasons a phase
// may carry is in VoiceControlStateV1.Validate and the design note §6.
type VoiceControlReason string

// Voice control reasons. The first group are the connection policy's reasons
// to try again (docs/design/conch-voice.md §5), with the names `join --json`
// already uses; the second are reasons to stop; the third are reasons to
// fail.
const (
	VoiceControlReasonRoomDeleted        VoiceControlReason = "room_deleted"
	VoiceControlReasonParticipantRemoved VoiceControlReason = "participant_removed"
	VoiceControlReasonConnectionLost     VoiceControlReason = "connection_lost"
	VoiceControlReasonReconnectTimedOut  VoiceControlReason = "reconnect_timed_out"
	VoiceControlReasonConnectFailed      VoiceControlReason = "connect_failed"
	VoiceControlReasonServerUnreachable  VoiceControlReason = "server_unreachable"
	VoiceControlReasonVoiceUnavailable   VoiceControlReason = "voice_unavailable"
	VoiceControlReasonNoSession          VoiceControlReason = "no_session"
	VoiceControlReasonServerShutdown     VoiceControlReason = "server_shutdown"

	// VoiceControlReasonLeft: the owner sent leave.
	VoiceControlReasonLeft VoiceControlReason = "left"
	// VoiceControlReasonOwnerLost: the control pipe ended or the owner is no
	// longer the helper's parent.
	VoiceControlReasonOwnerLost VoiceControlReason = "owner_lost"
	// VoiceControlReasonSignal: the helper got SIGTERM or SIGINT.
	VoiceControlReasonSignal VoiceControlReason = "signal"

	VoiceControlReasonNotSignedIn        VoiceControlReason = "not_signed_in"
	VoiceControlReasonIdentityMismatch   VoiceControlReason = "identity_mismatch"
	VoiceControlReasonBadEnvironment     VoiceControlReason = "bad_environment"
	VoiceControlReasonBadConfig          VoiceControlReason = "bad_config"
	VoiceControlReasonVoiceRequiresAuth  VoiceControlReason = "voice_requires_auth"
	VoiceControlReasonNotAPerson         VoiceControlReason = "not_a_person"
	VoiceControlReasonVoiceNotAllowed    VoiceControlReason = "voice_not_allowed"
	VoiceControlReasonNotAMember         VoiceControlReason = "not_a_member"
	VoiceControlReasonVoiceNotConfigured VoiceControlReason = "voice_not_configured"
	VoiceControlReasonDuplicateIdentity  VoiceControlReason = "duplicate_identity"
	VoiceControlReasonAudioUnavailable   VoiceControlReason = "audio_unavailable"
	VoiceControlReasonStartTimeout       VoiceControlReason = "start_timeout"
	VoiceControlReasonProtocolError      VoiceControlReason = "protocol_error"
	VoiceControlReasonInternalError      VoiceControlReason = "internal_error"
)

// Valid reports whether r is a recognized reason.
func (r VoiceControlReason) Valid() bool {
	switch r {
	case VoiceControlReasonRoomDeleted, VoiceControlReasonParticipantRemoved, VoiceControlReasonConnectionLost,
		VoiceControlReasonReconnectTimedOut, VoiceControlReasonConnectFailed, VoiceControlReasonServerUnreachable,
		VoiceControlReasonVoiceUnavailable, VoiceControlReasonNoSession, VoiceControlReasonServerShutdown,
		VoiceControlReasonLeft, VoiceControlReasonOwnerLost, VoiceControlReasonSignal,
		VoiceControlReasonNotSignedIn, VoiceControlReasonIdentityMismatch, VoiceControlReasonBadEnvironment,
		VoiceControlReasonBadConfig, VoiceControlReasonVoiceRequiresAuth, VoiceControlReasonNotAPerson,
		VoiceControlReasonVoiceNotAllowed, VoiceControlReasonNotAMember, VoiceControlReasonVoiceNotConfigured,
		VoiceControlReasonDuplicateIdentity, VoiceControlReasonAudioUnavailable, VoiceControlReasonStartTimeout,
		VoiceControlReasonProtocolError, VoiceControlReasonInternalError:
		return true
	default:
		return false
	}
}

// voiceControlRetryReasons may accompany reconnecting.
var voiceControlRetryReasons = []VoiceControlReason{
	VoiceControlReasonRoomDeleted, VoiceControlReasonParticipantRemoved, VoiceControlReasonConnectionLost,
	VoiceControlReasonReconnectTimedOut, VoiceControlReasonConnectFailed, VoiceControlReasonServerUnreachable,
	VoiceControlReasonVoiceUnavailable, VoiceControlReasonNoSession, VoiceControlReasonServerShutdown,
}

// voiceControlStopReasons may accompany stopped.
var voiceControlStopReasons = []VoiceControlReason{
	VoiceControlReasonLeft, VoiceControlReasonOwnerLost, VoiceControlReasonSignal,
}

// voiceControlFailReasons may accompany failed.
var voiceControlFailReasons = []VoiceControlReason{
	VoiceControlReasonNotSignedIn, VoiceControlReasonIdentityMismatch, VoiceControlReasonBadEnvironment,
	VoiceControlReasonBadConfig, VoiceControlReasonVoiceRequiresAuth, VoiceControlReasonNotAPerson,
	VoiceControlReasonVoiceNotAllowed, VoiceControlReasonNotAMember, VoiceControlReasonVoiceNotConfigured,
	VoiceControlReasonDuplicateIdentity, VoiceControlReasonAudioUnavailable, VoiceControlReasonServerUnreachable,
	VoiceControlReasonStartTimeout, VoiceControlReasonProtocolError, VoiceControlReasonInternalError,
}

// VoiceControlCapture is the state of the microphone stream, which is not the
// state of transmission: the stream stays open while joined with a grant to
// publish, and the gate decides what leaves (docs/design/conch-voice.md §3).
type VoiceControlCapture string

// Voice control capture states.
const (
	// VoiceControlCaptureOff: no microphone stream is open.
	VoiceControlCaptureOff VoiceControlCapture = "off"
	// VoiceControlCaptureOn: the microphone stream is open and has a device.
	// The desktop's "microphone in use" indicator is on.
	VoiceControlCaptureOn VoiceControlCapture = "on"
	// VoiceControlCaptureUnavailable: the stream could not be opened, or its
	// device has gone; no_microphone is among the blocked reasons.
	VoiceControlCaptureUnavailable VoiceControlCapture = "unavailable"
)

// Valid reports whether c is a recognized capture state.
func (c VoiceControlCapture) Valid() bool {
	switch c {
	case VoiceControlCaptureOff, VoiceControlCaptureOn, VoiceControlCaptureUnavailable:
		return true
	default:
		return false
	}
}

// VoiceControlBlock is one reason a press would do nothing now. The values and
// their order are the `blocked` of `conch-voice join --json`, most important
// first; a state lists them in this order.
type VoiceControlBlock string

// Voice control block reasons, in order of importance.
const (
	VoiceControlBlockNotConnected   VoiceControlBlock = "not_connected"
	VoiceControlBlockNoPublishGrant VoiceControlBlock = "no_publish_grant"
	VoiceControlBlockNoMicrophone   VoiceControlBlock = "no_microphone"
	VoiceControlBlockDeafened       VoiceControlBlock = "deafened"
	VoiceControlBlockMuted          VoiceControlBlock = "muted"
	VoiceControlBlockKeyDeviceLost  VoiceControlBlock = "key_device_lost"
	VoiceControlBlockMaxTransmit    VoiceControlBlock = "max_transmit"
)

// Valid reports whether b is a recognized block reason.
func (b VoiceControlBlock) Valid() bool {
	switch b {
	case VoiceControlBlockNotConnected, VoiceControlBlockNoPublishGrant, VoiceControlBlockNoMicrophone,
		VoiceControlBlockDeafened, VoiceControlBlockMuted, VoiceControlBlockKeyDeviceLost,
		VoiceControlBlockMaxTransmit:
		return true
	default:
		return false
	}
}

// voiceControlBlockOrder is the order a state lists block reasons in.
var voiceControlBlockOrder = []VoiceControlBlock{
	VoiceControlBlockNotConnected, VoiceControlBlockNoPublishGrant, VoiceControlBlockNoMicrophone,
	VoiceControlBlockDeafened, VoiceControlBlockMuted, VoiceControlBlockKeyDeviceLost,
	VoiceControlBlockMaxTransmit,
}

// VoiceControlPTT is whether hold-to-talk can work: whether the key device the
// configuration names is being read.
type VoiceControlPTT string

// Voice control push-to-talk states.
const (
	// VoiceControlPTTUnknown: the key device has not been tried yet. Only in a
	// state written before the configuration was read (starting, or a
	// failure or stop that came before it); the helper tries the key device
	// before it first connects.
	VoiceControlPTTUnknown VoiceControlPTT = "unknown"
	// VoiceControlPTTReady: the key device is open and a talk key is bound.
	VoiceControlPTTReady VoiceControlPTT = "ready"
	// VoiceControlPTTNotConfigured: voice.toml names no key device or binds
	// no talk key. Nothing can transmit; listening, mute, deafen and leave
	// work.
	VoiceControlPTTNotConfigured VoiceControlPTT = "not_configured"
	// VoiceControlPTTUnavailable: a key device is configured and has not been
	// read since the helper started; ptt_problem says why.
	VoiceControlPTTUnavailable VoiceControlPTT = "unavailable"
	// VoiceControlPTTLost: the key device was being read and is not any more;
	// every press is refused until it is read again (key_device_lost).
	VoiceControlPTTLost VoiceControlPTT = "lost"
)

// Valid reports whether p is a recognized push-to-talk state.
func (p VoiceControlPTT) Valid() bool {
	switch p {
	case VoiceControlPTTUnknown, VoiceControlPTTReady, VoiceControlPTTNotConfigured, VoiceControlPTTUnavailable,
		VoiceControlPTTLost:
		return true
	default:
		return false
	}
}

// VoiceControlPTTProblem is why the key device is not being read. The values
// are the `reason` of a `key_device` line of `conch-voice join --json`.
type VoiceControlPTTProblem string

// Voice control key device problems.
const (
	VoiceControlPTTProblemCannotOpen    VoiceControlPTTProblem = "cannot_open"
	VoiceControlPTTProblemRefused       VoiceControlPTTProblem = "refused"
	VoiceControlPTTProblemReadFailed    VoiceControlPTTProblem = "read_failed"
	VoiceControlPTTProblemEnded         VoiceControlPTTProblem = "ended"
	VoiceControlPTTProblemPartialRecord VoiceControlPTTProblem = "partial_record"
)

// Valid reports whether p is a recognized key device problem.
func (p VoiceControlPTTProblem) Valid() bool {
	switch p {
	case VoiceControlPTTProblemCannotOpen, VoiceControlPTTProblemRefused, VoiceControlPTTProblemReadFailed,
		VoiceControlPTTProblemEnded, VoiceControlPTTProblemPartialRecord:
		return true
	default:
		return false
	}
}

// VoiceControlErrorCode is why a line was refused: in an ack that refuses a
// command, in a protocol_error, and in the error ParseVoiceControlCommand and
// ParseVoiceControlEvent return.
type VoiceControlErrorCode string

// Voice control error codes.
const (
	// VoiceControlErrorTooLong: the line is longer than
	// VoiceControlMaxLineBytes.
	VoiceControlErrorTooLong VoiceControlErrorCode = "too_long"
	// VoiceControlErrorMalformed: the line is not valid UTF-8 holding exactly
	// one JSON object with no key given twice.
	VoiceControlErrorMalformed VoiceControlErrorCode = "malformed"
	// VoiceControlErrorUnknownSchema: the schema field is missing or is not
	// the version in use. A message of another version is not read as this
	// one.
	VoiceControlErrorUnknownSchema VoiceControlErrorCode = "unknown_schema"
	// VoiceControlErrorUnknownKind: a command of a kind this version does not
	// know. The command is refused and the helper goes on.
	VoiceControlErrorUnknownKind VoiceControlErrorCode = "unknown_kind"
	// VoiceControlErrorInvalid: a field is missing, of the wrong type or out
	// of range.
	VoiceControlErrorInvalid VoiceControlErrorCode = "invalid"
	// VoiceControlErrorIDNotIncreasing: a command's id is not greater than
	// the one before it.
	VoiceControlErrorIDNotIncreasing VoiceControlErrorCode = "id_not_increasing"
	// VoiceControlErrorNotStarted: the first command was not a valid start.
	VoiceControlErrorNotStarted VoiceControlErrorCode = "not_started"
	// VoiceControlErrorAlreadyStarted: a second start. It is refused and the
	// helper goes on.
	VoiceControlErrorAlreadyStarted VoiceControlErrorCode = "already_started"
	// VoiceControlErrorStopping: set_mute or set_deafen after leaving began.
	VoiceControlErrorStopping VoiceControlErrorCode = "stopping"
)

// Valid reports whether c is a recognized error code.
func (c VoiceControlErrorCode) Valid() bool {
	switch c {
	case VoiceControlErrorTooLong, VoiceControlErrorMalformed, VoiceControlErrorUnknownSchema,
		VoiceControlErrorUnknownKind, VoiceControlErrorInvalid, VoiceControlErrorIDNotIncreasing,
		VoiceControlErrorNotStarted, VoiceControlErrorAlreadyStarted, VoiceControlErrorStopping:
		return true
	default:
		return false
	}
}

// AckRefusal reports whether c may be the error of an ack: a command the
// helper refuses and goes on after.
func (c VoiceControlErrorCode) AckRefusal() bool {
	switch c {
	case VoiceControlErrorUnknownKind, VoiceControlErrorInvalid, VoiceControlErrorAlreadyStarted,
		VoiceControlErrorStopping:
		return true
	default:
		return false
	}
}

// Fatal reports whether c may be the code of a protocol_error: a line the
// helper cannot act on, after which it fails with protocol_error. invalid is
// both: fatal when the line has no usable id to acknowledge, refused by an
// ack when it has one.
func (c VoiceControlErrorCode) Fatal() bool {
	switch c {
	case VoiceControlErrorTooLong, VoiceControlErrorMalformed, VoiceControlErrorUnknownSchema,
		VoiceControlErrorInvalid, VoiceControlErrorIDNotIncreasing, VoiceControlErrorNotStarted:
		return true
	default:
		return false
	}
}

// VoiceControlMessage is one of the nine message types of this file. The
// unexported method keeps any other type out of MarshalVoiceControlLine.
type VoiceControlMessage interface {
	Validate() error
	voiceControlMessage()
}

// VoiceControlStartV1 is the first command (owner to helper): join Channel on
// Server as PrincipalID, with the login named by Credential, muted and
// deafened as given. Its schema is the version the owner picked from the
// hello's offer; for this type, always VoiceControlSchemaV1.
//
// It carries no token. The helper takes the login from Credential's source,
// asks conchd who that is (GET /v1/whoami), and fails with identity_mismatch
// before opening any device if the answer is not PrincipalID, so it can never
// join as anyone but the owner's user.
type VoiceControlStartV1 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	// ID is the command's id, 1 to VoiceControlMaxID, greater than any id
	// before it. A start is normally 1.
	ID int64 `json:"id"`
	// Server is the conchd address, as the owner normalized it (lower-case
	// scheme and host, no trailing slash, no query or fragment): the key the
	// stored login is kept under. The helper uses it and nothing else.
	Server string `json:"server"`
	// Channel is the channel whose voice to join, as the owner names it.
	Channel string `json:"channel"`
	// PrincipalID is the owner's user, as conchd's whoami told the owner.
	PrincipalID int64 `json:"principal_id"`
	// Credential is where the login token is.
	Credential VoiceControlCredential `json:"credential"`
	// Muted is whether to start muted. Always written.
	Muted bool `json:"muted"`
	// Deafened is whether to start deafened. Always written.
	Deafened bool `json:"deafened"`
}

func (VoiceControlStartV1) voiceControlMessage() {}

// Validate reports whether the start is well-formed.
func (s VoiceControlStartV1) Validate() error {
	if err := validateVoiceControlCommandHead(s.Schema, s.Kind, VoiceControlCommandStart, s.ID); err != nil {
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

// VoiceControlSetMuteV1 sets whether the user is muted (owner to helper). It
// is a desired state, not a toggle: sent twice it changes nothing the second
// time.
type VoiceControlSetMuteV1 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
	// Muted is the state wanted.
	Muted bool `json:"muted"`
}

func (VoiceControlSetMuteV1) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetMuteV1) Validate() error {
	return validateVoiceControlCommandHead(c.Schema, c.Kind, VoiceControlCommandSetMute, c.ID)
}

// VoiceControlSetDeafenV1 sets whether the user is deafened (owner to
// helper). Like set_mute, a desired state.
type VoiceControlSetDeafenV1 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
	// Deafened is the state wanted.
	Deafened bool `json:"deafened"`
}

func (VoiceControlSetDeafenV1) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlSetDeafenV1) Validate() error {
	return validateVoiceControlCommandHead(c.Schema, c.Kind, VoiceControlCommandSetDeafen, c.ID)
}

// VoiceControlGetStateV1 asks for a state snapshot (owner to helper), which
// is written before the ack whether or not anything changed.
type VoiceControlGetStateV1 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
}

func (VoiceControlGetStateV1) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlGetStateV1) Validate() error {
	return validateVoiceControlCommandHead(c.Schema, c.Kind, VoiceControlCommandGetState, c.ID)
}

// VoiceControlLeaveV1 leaves voice (owner to helper): the gate shuts at once,
// with no release tail, the room is left and the helper exits 0.
type VoiceControlLeaveV1 struct {
	Schema string                  `json:"schema"`
	Kind   VoiceControlCommandKind `json:"kind"`
	ID     int64                   `json:"id"`
}

func (VoiceControlLeaveV1) voiceControlMessage() {}

// Validate reports whether the command is well-formed.
func (c VoiceControlLeaveV1) Validate() error {
	return validateVoiceControlCommandHead(c.Schema, c.Kind, VoiceControlCommandLeave, c.ID)
}

// VoiceControlHelloV1 is the helper's first line (helper to owner), written
// before it reads anything, opens any device or makes any request. Its shape
// is frozen: a helper of any later version writes its hello in exactly this
// shape and version, listing every version it speaks, so that any owner can
// read it and decide.
type VoiceControlHelloV1 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	// Seq is always 1: the hello is the first event.
	Seq int64 `json:"seq"`
	// Versions is every schema name the helper speaks, one to
	// VoiceControlMaxVersions of them, each conch.voice_control.v<N>, none
	// twice. The owner picks one and writes start in it.
	Versions []string `json:"versions"`
	// HelperVersion is the helper's build, to show in a status view: 1 to
	// VoiceControlMaxHelperVersionBytes bytes of printable ASCII.
	HelperVersion string `json:"helper_version"`
}

func (VoiceControlHelloV1) voiceControlMessage() {}

// voiceControlVersionName is the shape of a version a hello may offer.
var voiceControlVersionName = regexp.MustCompile(`^conch\.voice_control\.v[1-9][0-9]{0,3}$`)

// Validate reports whether the hello is well-formed. A hello that offers no
// version the owner speaks is well-formed; Offers says whether it can be used.
func (h VoiceControlHelloV1) Validate() error {
	if h.Schema != VoiceControlSchemaV1 {
		return fmt.Errorf("schema: voice control hello schema must be %q, got %q", VoiceControlSchemaV1, h.Schema)
	}
	if h.Kind != VoiceControlEventHello {
		return fmt.Errorf("schema: voice control hello kind must be %q, got %q", VoiceControlEventHello, h.Kind)
	}
	if h.Seq != 1 {
		return fmt.Errorf("schema: voice control hello seq must be 1, got %d", h.Seq)
	}
	if len(h.Versions) == 0 || len(h.Versions) > VoiceControlMaxVersions {
		return fmt.Errorf("schema: voice control hello must offer 1 to %d versions, got %d", VoiceControlMaxVersions, len(h.Versions))
	}
	for i, v := range h.Versions {
		if !voiceControlVersionName.MatchString(v) {
			return fmt.Errorf("schema: voice control hello version %d is not a voice control schema name", i)
		}
		if slices.Contains(h.Versions[:i], v) {
			return fmt.Errorf("schema: voice control hello offers version %d twice", i)
		}
	}
	if h.HelperVersion == "" || len(h.HelperVersion) > VoiceControlMaxHelperVersionBytes {
		return fmt.Errorf("schema: voice control hello helper_version must be 1 to %d bytes", VoiceControlMaxHelperVersionBytes)
	}
	for i := 0; i < len(h.HelperVersion); i++ {
		if c := h.HelperVersion[i]; c < 0x20 || c > 0x7e {
			return errors.New("schema: voice control hello helper_version must be printable ASCII")
		}
	}
	return nil
}

// Offers reports whether the hello offers version, a schema name such as
// VoiceControlSchemaV1.
func (h VoiceControlHelloV1) Offers(version string) bool {
	return slices.Contains(h.Versions, version)
}

// VoiceControlAckV1 answers one command (helper to owner): accepted (ok) or
// refused with a reason. Every command with a usable id gets exactly one ack,
// in the order the commands were read, after any state its effect changed.
type VoiceControlAckV1 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	// ID is the id of the command answered.
	ID int64 `json:"id"`
	// OK is whether the command was carried out. Always written.
	OK bool `json:"ok"`
	// Error is why not; present exactly when OK is false, and then one of
	// the codes for which AckRefusal is true.
	Error VoiceControlErrorCode `json:"error,omitempty"`
}

func (VoiceControlAckV1) voiceControlMessage() {}

// Validate reports whether the ack is well-formed.
func (a VoiceControlAckV1) Validate() error {
	if err := validateVoiceControlEventHead(a.Schema, a.Kind, VoiceControlEventAck, a.Seq); err != nil {
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

// VoiceControlStateV1 is a whole-state snapshot (helper to owner): what the
// helper is doing and what a press would do, as it really is, not as it was
// asked to be. It is written on every change and in answer to get_state. A
// reader that missed one loses nothing by reading the next.
type VoiceControlStateV1 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	// Phase is where the helper is in its life.
	Phase VoiceControlPhase `json:"phase"`
	// Reason is why: required for stopping, stopped and failed, optional for
	// reconnecting, absent otherwise.
	Reason VoiceControlReason `json:"reason,omitempty"`
	// RetryInMS is, while reconnecting for a reason, how long until conchd is
	// asked for a session again; 0 means at once. Absent otherwise. A pointer
	// so that 0 is written and absence is not.
	RetryInMS *int64 `json:"retry_in_ms,omitempty"`
	// Capture is the state of the microphone stream. Always written.
	Capture VoiceControlCapture `json:"capture"`
	// Transmitting is whether audio is leaving now, release tail included.
	Transmitting bool `json:"transmitting"`
	// Muted is whether the user is muted, as the push-to-talk machine has it.
	Muted bool `json:"muted"`
	// Deafened is whether the user is deafened, as the machine has it.
	Deafened bool `json:"deafened"`
	// Blocked is every reason a press would do nothing now, in the order of
	// the VoiceControlBlock constants; an empty array means a press would
	// transmit. Always a JSON array.
	Blocked []VoiceControlBlock `json:"blocked"`
	// PTT is whether hold-to-talk can work.
	PTT VoiceControlPTT `json:"ptt"`
	// PTTProblem is why the key device is not being read; present exactly
	// when PTT is unavailable or lost.
	PTTProblem VoiceControlPTTProblem `json:"ptt_problem,omitempty"`
}

func (VoiceControlStateV1) voiceControlMessage() {}

// Blocks reports whether b is among the state's block reasons.
func (s VoiceControlStateV1) Blocks(b VoiceControlBlock) bool {
	return slices.Contains(s.Blocked, b)
}

// Validate reports whether the snapshot is well-formed and consistent. The
// rules are the ones a reader relies on to show it honestly:
//
//   - transmitting only while connected, capturing, with the talk key read
//     and nothing blocking;
//   - not_connected among the blocks in every phase but connected, so that a
//     state never says a press would transmit while it could not;
//   - no capture while starting or after the end, and the key device tried
//     before connecting (ptt unknown only outside connecting, connected and
//     reconnecting);
//   - capture on and no_microphone never together, capture unavailable and
//     no_microphone always together;
//   - muted and deafened agreeing with their block reasons, and a lost key
//     device with key_device_lost;
//   - each phase with only the reasons it may carry.
func (s VoiceControlStateV1) Validate() error {
	if err := validateVoiceControlEventHead(s.Schema, s.Kind, VoiceControlEventState, s.Seq); err != nil {
		return err
	}
	if !s.Phase.Valid() {
		return fmt.Errorf("schema: voice control state phase %q is not recognized", s.Phase)
	}
	if !s.Capture.Valid() {
		return fmt.Errorf("schema: voice control state capture %q is not one of off, on, unavailable", s.Capture)
	}
	if !s.PTT.Valid() {
		return fmt.Errorf("schema: voice control state ptt %q is not one of unknown, ready, not_configured, unavailable, lost", s.PTT)
	}
	switch s.Phase {
	case VoiceControlPhaseConnecting, VoiceControlPhaseConnected, VoiceControlPhaseReconnecting:
		if s.PTT == VoiceControlPTTUnknown {
			return fmt.Errorf("schema: voice control state in phase %q must say whether the key device is read", s.Phase)
		}
	}
	if err := validateVoiceControlBlocked(s.Blocked); err != nil {
		return err
	}
	if err := s.validateReason(); err != nil {
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
	if (s.PTT == VoiceControlPTTLost) != s.Blocks(VoiceControlBlockKeyDeviceLost) {
		return errors.New("schema: voice control state ptt lost must agree with the key_device_lost block reason")
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
		case s.PTT != VoiceControlPTTReady:
			return errors.New("schema: voice control state cannot be transmitting without ptt ready")
		}
	}
	return nil
}

// validateReason checks the reason and retry_in_ms against the phase.
func (s VoiceControlStateV1) validateReason() error {
	if s.Reason != "" && !s.Reason.Valid() {
		return fmt.Errorf("schema: voice control state reason %q is not recognized", s.Reason)
	}
	switch s.Phase {
	case VoiceControlPhaseStarting, VoiceControlPhaseConnecting, VoiceControlPhaseConnected:
		if s.Reason != "" {
			return fmt.Errorf("schema: voice control state in phase %q must have no reason", s.Phase)
		}
	case VoiceControlPhaseReconnecting:
		if s.Reason != "" && !slices.Contains(voiceControlRetryReasons, s.Reason) {
			return fmt.Errorf("schema: voice control state reason %q is not a reason to reconnect", s.Reason)
		}
	case VoiceControlPhaseStopping:
		if !slices.Contains(voiceControlStopReasons, s.Reason) && !slices.Contains(voiceControlFailReasons, s.Reason) {
			return fmt.Errorf("schema: voice control state reason %q is not a reason to stop", s.Reason)
		}
	case VoiceControlPhaseStopped:
		if !slices.Contains(voiceControlStopReasons, s.Reason) {
			return fmt.Errorf("schema: voice control state stopped needs one of left, owner_lost, signal, got %q", s.Reason)
		}
	case VoiceControlPhaseFailed:
		if !slices.Contains(voiceControlFailReasons, s.Reason) {
			return fmt.Errorf("schema: voice control state reason %q is not a reason to fail", s.Reason)
		}
	}
	if s.RetryInMS != nil {
		if s.Phase != VoiceControlPhaseReconnecting || s.Reason == "" {
			return errors.New("schema: voice control state retry_in_ms is only for reconnecting with a reason")
		}
		if *s.RetryInMS < 0 || *s.RetryInMS > VoiceControlMaxRetryInMS {
			return fmt.Errorf("schema: voice control state retry_in_ms must be 0 to %d", VoiceControlMaxRetryInMS)
		}
	}
	return nil
}

// validateVoiceControlBlocked checks that blocked is present, holds only
// recognized reasons, none twice, in their order.
func validateVoiceControlBlocked(blocked []VoiceControlBlock) error {
	if blocked == nil {
		return errors.New("schema: voice control state blocked is required (an empty array when nothing blocks)")
	}
	last := -1
	for _, b := range blocked {
		at := slices.Index(voiceControlBlockOrder, b)
		if at < 0 {
			return fmt.Errorf("schema: voice control state block reason %q is not recognized", b)
		}
		if at <= last {
			return errors.New("schema: voice control state block reasons must be listed once each, most important first")
		}
		last = at
	}
	return nil
}

// VoiceControlProtocolErrorV1 says that the helper read a line it cannot act
// on (helper to owner). It is followed by a failed state with reason
// protocol_error, and the helper exits.
type VoiceControlProtocolErrorV1 struct {
	Schema string                `json:"schema"`
	Kind   VoiceControlEventKind `json:"kind"`
	Seq    int64                 `json:"seq"`
	// Code is what was wrong, one of the codes for which Fatal is true.
	Code VoiceControlErrorCode `json:"code"`
}

func (VoiceControlProtocolErrorV1) voiceControlMessage() {}

// Validate reports whether the event is well-formed.
func (e VoiceControlProtocolErrorV1) Validate() error {
	if err := validateVoiceControlEventHead(e.Schema, e.Kind, VoiceControlEventProtocolError, e.Seq); err != nil {
		return err
	}
	if !e.Code.Fatal() {
		return fmt.Errorf("schema: voice control protocol_error code %q is not one that ends the session", e.Code)
	}
	return nil
}

func validateVoiceControlID(id int64) error {
	if id < 1 || id > VoiceControlMaxID {
		return fmt.Errorf("schema: voice control id must be 1 to %d, got %d", int64(VoiceControlMaxID), id)
	}
	return nil
}

func validateVoiceControlCommandHead(schema string, kind, want VoiceControlCommandKind, id int64) error {
	if schema != VoiceControlSchemaV1 {
		return fmt.Errorf("schema: voice control command schema must be %q, got %q", VoiceControlSchemaV1, schema)
	}
	if kind != want {
		return fmt.Errorf("schema: voice control command kind must be %q, got %q", want, kind)
	}
	return validateVoiceControlID(id)
}

func validateVoiceControlEventHead(schema string, kind, want VoiceControlEventKind, seq int64) error {
	if schema != VoiceControlSchemaV1 {
		return fmt.Errorf("schema: voice control event schema must be %q, got %q", VoiceControlSchemaV1, schema)
	}
	if kind != want {
		return fmt.Errorf("schema: voice control event kind must be %q, got %q", want, kind)
	}
	if seq < 1 || seq > VoiceControlMaxID {
		return fmt.Errorf("schema: voice control seq must be 1 to %d, got %d", int64(VoiceControlMaxID), seq)
	}
	return nil
}

// validateVoiceControlServer accepts an http(s) address in the form the
// owner's credential store keys it by: lower-case scheme and host, no user,
// query or fragment, no trailing slash. It does not echo the address.
func validateVoiceControlServer(server string) error {
	if server == "" || len(server) > VoiceControlMaxServerBytes {
		return fmt.Errorf("schema: voice control start server must be 1 to %d bytes", VoiceControlMaxServerBytes)
	}
	u, err := url.Parse(server)
	if err != nil {
		return errors.New("schema: voice control start server is not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("schema: voice control start server must be an http or https URL in lower case")
	case u.Host == "" || u.Host != strings.ToLower(u.Host):
		return errors.New("schema: voice control start server must have a host, in lower case")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return errors.New("schema: voice control start server must have no user, query or fragment")
	case strings.HasSuffix(u.Path, "/"):
		return errors.New("schema: voice control start server must not end with a slash")
	}
	return nil
}

// validateVoiceControlChannel accepts any name the server could have given a
// channel, old ones included: not blank, not too long, no control character.
func validateVoiceControlChannel(channel string) error {
	if strings.TrimSpace(channel) == "" || len(channel) > VoiceControlMaxChannelBytes {
		return fmt.Errorf("schema: voice control start channel must be 1 to %d bytes and not blank", VoiceControlMaxChannelBytes)
	}
	if !utf8.ValidString(channel) {
		return errors.New("schema: voice control start channel must be valid UTF-8")
	}
	if strings.ContainsFunc(channel, unicode.IsControl) {
		return errors.New("schema: voice control start channel must not contain control characters")
	}
	return nil
}

// VoiceControlError is why ParseVoiceControlCommand or ParseVoiceControlEvent
// refused a line. Code is the wire code for it; ID is the refused command's
// id when the line had a usable one (and so can be answered by an ack), else
// 0. The text never repeats the line.
type VoiceControlError struct {
	Code   VoiceControlErrorCode
	ID     int64
	detail string
}

func (e *VoiceControlError) Error() string {
	return "schema: voice control " + string(e.Code) + ": " + e.detail
}

func voiceControlErr(code VoiceControlErrorCode, id int64, format string, args ...any) *VoiceControlError {
	return &VoiceControlError{Code: code, ID: id, detail: fmt.Sprintf(format, args...)}
}

// VoiceControlCommand is one command as ParseVoiceControlCommand read it:
// exactly one field is set. It is not a wire shape.
type VoiceControlCommand struct {
	Start     *VoiceControlStartV1
	SetMute   *VoiceControlSetMuteV1
	SetDeafen *VoiceControlSetDeafenV1
	GetState  *VoiceControlGetStateV1
	Leave     *VoiceControlLeaveV1
}

// VoiceControlEvent is one event as ParseVoiceControlEvent read it: Seq, and
// exactly one of the message fields, or none and Unknown set to the kind of
// an event this version does not know, which a reader skips. It is not a wire
// shape.
type VoiceControlEvent struct {
	Seq           int64
	Hello         *VoiceControlHelloV1
	Ack           *VoiceControlAckV1
	State         *VoiceControlStateV1
	ProtocolError *VoiceControlProtocolErrorV1
	Unknown       VoiceControlEventKind
}

// ParseVoiceControlCommand reads one command line, without its line feed. It
// is how the helper's side is specified, and what a Go fake helper uses. It
// refuses, in this order, and with these codes:
//
//   - too_long: more than VoiceControlMaxLineBytes bytes;
//   - malformed: not valid UTF-8, a line feed inside, not exactly one JSON
//     object (white space around it is allowed), or a key given twice;
//   - unknown_schema: schema missing, not a string, or not
//     VoiceControlSchemaV1;
//   - invalid with no ID: id missing or not an integer from 1 to
//     VoiceControlMaxID;
//   - unknown_kind with the ID: kind missing, not a string, or not a command
//     kind of this version;
//   - invalid with the ID: a field of the kind missing, null or of the wrong
//     JSON type, or the command failing Validate.
//
// Field names are matched exactly, case included. A field the kind does not
// declare is ignored, so that a later owner can add one.
func ParseVoiceControlCommand(line []byte) (VoiceControlCommand, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlCommand{}, err
	}
	if err := checkVoiceControlSchema(fields); err != nil {
		return VoiceControlCommand{}, err
	}
	id, ok := fieldInt(fields, "id")
	if !ok || validateVoiceControlID(id) != nil {
		return VoiceControlCommand{}, voiceControlErr(VoiceControlErrorInvalid, 0, "id must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, _ := fieldString(fields, "kind")
	kind := VoiceControlCommandKind(kindText)
	if !kind.Valid() {
		return VoiceControlCommand{}, voiceControlErr(VoiceControlErrorUnknownKind, id, "not a command kind of %s", VoiceControlSchemaV1)
	}
	r := fieldReader{fields: fields}
	var cmd VoiceControlCommand
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlCommandStart:
		s := &VoiceControlStartV1{Schema: VoiceControlSchemaV1, Kind: kind, ID: id}
		s.Server = r.str("server")
		s.Channel = r.str("channel")
		s.PrincipalID = r.int("principal_id")
		s.Credential = VoiceControlCredential(r.str("credential"))
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		cmd.Start, msg = s, s
	case VoiceControlCommandSetMute:
		c := &VoiceControlSetMuteV1{Schema: VoiceControlSchemaV1, Kind: kind, ID: id, Muted: r.bool("muted")}
		cmd.SetMute, msg = c, c
	case VoiceControlCommandSetDeafen:
		c := &VoiceControlSetDeafenV1{Schema: VoiceControlSchemaV1, Kind: kind, ID: id, Deafened: r.bool("deafened")}
		cmd.SetDeafen, msg = c, c
	case VoiceControlCommandGetState:
		c := &VoiceControlGetStateV1{Schema: VoiceControlSchemaV1, Kind: kind, ID: id}
		cmd.GetState, msg = c, c
	case VoiceControlCommandLeave:
		c := &VoiceControlLeaveV1{Schema: VoiceControlSchemaV1, Kind: kind, ID: id}
		cmd.Leave, msg = c, c
	}
	if r.missing != "" {
		return VoiceControlCommand{}, voiceControlErr(VoiceControlErrorInvalid, id, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlCommand{}, &VoiceControlError{Code: VoiceControlErrorInvalid, ID: id, detail: err.Error()}
	}
	return cmd, nil
}

// Kind is the kind of the command held.
func (c VoiceControlCommand) Kind() VoiceControlCommandKind {
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
	}
	return ""
}

// ID is the id of the command held, or 0 if none is.
func (c VoiceControlCommand) ID() int64 {
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
	}
	return 0
}

// ParseVoiceControlEvent reads one event line, without its line feed. It is
// how the owner's side is specified. It refuses, in this order:
//
//   - too_long and malformed, as ParseVoiceControlCommand;
//   - unknown_schema: schema missing, not a string, or not
//     VoiceControlSchemaV1;
//   - invalid: seq missing or not an integer from 1 to VoiceControlMaxID;
//     kind missing or not a string; a field of a known kind missing, null or
//     of the wrong JSON type; or the event failing Validate.
//
// An event of a kind this version does not know is not an error: it comes
// back with Seq and Unknown set, for the reader to skip. Field names are
// matched exactly, and a field the kind does not declare is ignored.
//
// Whether seq follows the one before, and whether the first event is a hello,
// is the reader's to check: it needs the history.
func ParseVoiceControlEvent(line []byte) (VoiceControlEvent, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlEvent{}, err
	}
	if err := checkVoiceControlSchema(fields); err != nil {
		return VoiceControlEvent{}, err
	}
	seq, ok := fieldInt(fields, "seq")
	if !ok || seq < 1 || seq > VoiceControlMaxID {
		return VoiceControlEvent{}, voiceControlErr(VoiceControlErrorInvalid, 0, "seq must be an integer from 1 to %d", int64(VoiceControlMaxID))
	}
	kindText, ok := fieldString(fields, "kind")
	if !ok || kindText == "" {
		return VoiceControlEvent{}, voiceControlErr(VoiceControlErrorInvalid, 0, "kind must be a string")
	}
	kind := VoiceControlEventKind(kindText)
	ev := VoiceControlEvent{Seq: seq}
	if !kind.Valid() {
		ev.Unknown = kind
		return ev, nil
	}
	r := fieldReader{fields: fields}
	var msg VoiceControlMessage
	switch kind {
	case VoiceControlEventHello:
		h := &VoiceControlHelloV1{Schema: VoiceControlSchemaV1, Kind: kind, Seq: seq}
		h.Versions = r.strs("versions")
		h.HelperVersion = r.str("helper_version")
		ev.Hello, msg = h, h
	case VoiceControlEventAck:
		a := &VoiceControlAckV1{Schema: VoiceControlSchemaV1, Kind: kind, Seq: seq}
		a.ID = r.int("id")
		a.OK = r.bool("ok")
		a.Error = VoiceControlErrorCode(r.optStr("error"))
		ev.Ack, msg = a, a
	case VoiceControlEventState:
		s := &VoiceControlStateV1{Schema: VoiceControlSchemaV1, Kind: kind, Seq: seq}
		s.Phase = VoiceControlPhase(r.str("phase"))
		s.Reason = VoiceControlReason(r.optStr("reason"))
		s.RetryInMS = r.optInt("retry_in_ms")
		s.Capture = VoiceControlCapture(r.str("capture"))
		s.Transmitting = r.bool("transmitting")
		s.Muted = r.bool("muted")
		s.Deafened = r.bool("deafened")
		for _, b := range r.strs("blocked") {
			s.Blocked = append(s.Blocked, VoiceControlBlock(b))
		}
		if s.Blocked == nil && r.missing == "" {
			s.Blocked = []VoiceControlBlock{}
		}
		s.PTT = VoiceControlPTT(r.str("ptt"))
		s.PTTProblem = VoiceControlPTTProblem(r.optStr("ptt_problem"))
		ev.State, msg = s, s
	case VoiceControlEventProtocolError:
		e := &VoiceControlProtocolErrorV1{Schema: VoiceControlSchemaV1, Kind: kind, Seq: seq}
		e.Code = VoiceControlErrorCode(r.str("code"))
		ev.ProtocolError, msg = e, e
	}
	if r.missing != "" {
		return VoiceControlEvent{}, voiceControlErr(VoiceControlErrorInvalid, 0, "%s needs %s of the right type", kind, r.missing)
	}
	if err := msg.Validate(); err != nil {
		return VoiceControlEvent{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return ev, nil
}

// MarshalVoiceControlLine encodes one message as it is written: compact JSON
// and a line feed. It refuses a message that fails Validate or that would be
// longer than VoiceControlMaxLineBytes, so a writer cannot send what a reader
// must refuse. "<", ">" and "&" are written as themselves, as the Rust side
// writes them: escaping them would only lengthen a line.
func MarshalVoiceControlLine(msg VoiceControlMessage) ([]byte, error) {
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		return nil, fmt.Errorf("schema: encode voice control message: %w", err)
	}
	// Encode ends the value with a line feed of its own.
	line := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if len(line) > VoiceControlMaxLineBytes {
		return nil, fmt.Errorf("schema: voice control message is %d bytes, more than %d", len(line), VoiceControlMaxLineBytes)
	}
	return append(line, '\n'), nil
}

// decodeVoiceControlObject applies the framing rules shared by both
// directions and returns the object's fields, unparsed.
func decodeVoiceControlObject(line []byte) (map[string]json.RawMessage, error) {
	if len(line) > VoiceControlMaxLineBytes {
		return nil, voiceControlErr(VoiceControlErrorTooLong, 0, "%d bytes, more than %d", len(line), VoiceControlMaxLineBytes)
	}
	if !utf8.Valid(line) {
		return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not valid UTF-8")
	}
	if bytes.IndexByte(line, '\n') >= 0 {
		return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "a line feed inside the line")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not a JSON object")
		}
		key, ok := tok.(string)
		if !ok {
			return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not a JSON object")
		}
		if _, dup := fields[key]; dup {
			return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "a key given twice")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not a JSON object")
		}
		fields[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, voiceControlErr(VoiceControlErrorMalformed, 0, "more than one JSON value")
	}
	return fields, nil
}

func checkVoiceControlSchema(fields map[string]json.RawMessage) error {
	if s, ok := fieldString(fields, "schema"); !ok || s != VoiceControlSchemaV1 {
		return voiceControlErr(VoiceControlErrorUnknownSchema, 0, "schema is not %s", VoiceControlSchemaV1)
	}
	return nil
}

// isJSONNull reports whether raw is the JSON null, which no field accepts:
// encoding/json would read it as the zero value without complaint.
func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

func fieldString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok || isJSONNull(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func fieldInt(fields map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := fields[key]
	if !ok || isJSONNull(raw) {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// fieldReader takes fields out of a decoded object by exact name and
// remembers the first required one that was missing or of the wrong type.
type fieldReader struct {
	fields  map[string]json.RawMessage
	missing string
}

// take unmarshals the field into v. A field that is absent is missing when
// required and fine when not; a null or a value of the wrong type is missing
// either way.
func (r *fieldReader) take(key string, v any, required bool) bool {
	raw, ok := r.fields[key]
	if !ok {
		if required && r.missing == "" {
			r.missing = key
		}
		return false
	}
	if isJSONNull(raw) || json.Unmarshal(raw, v) != nil {
		if r.missing == "" {
			r.missing = key
		}
		return false
	}
	return true
}

func (r *fieldReader) str(key string) string {
	var s string
	r.take(key, &s, true)
	return s
}

func (r *fieldReader) optStr(key string) string {
	var s string
	r.take(key, &s, false)
	return s
}

func (r *fieldReader) int(key string) int64 {
	var n int64
	r.take(key, &n, true)
	return n
}

func (r *fieldReader) optInt(key string) *int64 {
	var n int64
	if !r.take(key, &n, false) {
		return nil
	}
	return &n
}

func (r *fieldReader) bool(key string) bool {
	var b bool
	r.take(key, &b, true)
	return b
}

func (r *fieldReader) strs(key string) []string {
	var ss []string
	if !r.take(key, &ss, true) {
		return nil
	}
	if ss == nil {
		ss = []string{}
	}
	return ss
}
