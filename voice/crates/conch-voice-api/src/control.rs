//! The control protocol of a managed `conch-voice` (`conch.voice_control.v1`): Rust copies
//! of the shapes in `pkg/schema/voice_control.go`, read and written by the rules of
//! `docs/design/tui-voice.md` §3 and §4.
//!
//! A native client, the owner, starts `conch-voice managed` and drives it over two pipes:
//! commands on the helper's standard input, events on its file descriptor 3. Every message
//! is one JSON object on one line. [`parse_command`] is how the helper reads a line of its
//! owner's; [`parse_event`] is how an owner reads one of the helper's, and is here so that
//! the tests read what the helper wrote by the rules the Go owner applies; [`to_line`]
//! writes either, and refuses what a reader would refuse.
//!
//! What the protocol leaves out is part of it: no field can hold a token, an event carries
//! no free text (every string is a schema name, a word of a closed vocabulary, or the
//! helper's version, which is short printable ASCII), and there is no command that presses
//! or releases the talk key.
//!
//! The readers apply Go's rules, in Go's order (`ParseVoiceControlCommand`,
//! `ParseVoiceControlEvent`), so that the two languages accept and refuse the same lines
//! with the same codes:
//!
//! - **too_long**: more than [`VOICE_CONTROL_MAX_LINE_BYTES`] bytes.
//! - **malformed**: not UTF-8, a line feed inside, not exactly one JSON object (white space
//!   around it is allowed), or a key given twice.
//! - **unknown_schema**: `schema` missing, not a string, or not this version.
//! - **invalid** without an id: `id` (for an event, `seq`) missing or not a whole number
//!   from 1 to [`VOICE_CONTROL_MAX_ID`]; for an event also `kind` missing or empty.
//! - **unknown_kind** with the id: a command kind this version does not know. (An event of
//!   a kind this version does not know is not refused: it is skipped.)
//! - **invalid** with the id: a field of the kind missing, `null` or of the wrong type, or
//!   the message breaking a rule of the schema's `Validate`.
//!
//! Field names are matched exactly, case included, and a field the kind does not declare
//! is ignored. The typed decoding is `wire_object!`'s, as for the REST types.
//!
//! Where serde's JSON reader is stricter than Go's, a line Go would accept is refused here
//! as malformed or invalid; no line Go refuses is accepted. Two such corners are known, and
//! no owner writes either: a number too large for a float (`1e400`) in `schema`, `kind`,
//! `id` or `seq`, and a string holding half of a surrogate pair (`"\ud800"`), which Go reads
//! as U+FFFD.

use std::collections::BTreeSet;
use std::fmt;

use serde::de::{self, DeserializeOwned, IgnoredAny, MapAccess, SeqAccess, Visitor};
use serde::{Deserialize, Deserializer, Serialize, Serializer};

use crate::error::Error;
use crate::types::wire_object;

/// The name every v1 message carries in its `schema` field. A hello is always written in
/// this version, so that an owner of any version can read it.
pub const VOICE_CONTROL_SCHEMA_V1: &str = "conch.voice_control.v1";
/// The helper's subcommand: the owner runs `conch-voice managed`.
pub const VOICE_CONTROL_SUBCOMMAND: &str = "managed";
/// The file descriptor the helper writes events on.
pub const VOICE_CONTROL_EVENT_FD: i32 = 3;
/// The longest line, in bytes and without its line feed, either side writes or accepts.
pub const VOICE_CONTROL_MAX_LINE_BYTES: usize = 4096;
/// The largest command id or event seq: 2^53-1, which any JSON reader holds exactly.
pub const VOICE_CONTROL_MAX_ID: i64 = (1 << 53) - 1;
/// The most commands the owner may have written and not yet seen acknowledged.
pub const VOICE_CONTROL_MAX_IN_FLIGHT: usize = 8;
/// The most events the helper holds for a reader that is slow; past it a state or
/// protocol_error is dropped, and the gap shows in seq. Acks, and the state that answers
/// `get_state`, wait in places of their own (`docs/design/tui-voice.md` §3).
pub const VOICE_CONTROL_EVENT_QUEUE_LINES: usize = 256;
/// The most versions a hello may offer.
pub const VOICE_CONTROL_MAX_VERSIONS: usize = 8;
/// The longest `helper_version`.
pub const VOICE_CONTROL_MAX_HELPER_VERSION_BYTES: usize = 64;
/// The longest `server` in a start.
pub const VOICE_CONTROL_MAX_SERVER_BYTES: usize = 1024;
/// The longest `channel` in a start: 100 characters of up to four bytes.
pub const VOICE_CONTROL_MAX_CHANNEL_BYTES: usize = 400;
/// The largest `retry_in_ms`.
pub const VOICE_CONTROL_MAX_RETRY_IN_MS: i64 = 600_000;
/// Owner: from starting the process to reading its hello.
pub const VOICE_CONTROL_HELLO_TIMEOUT_MS: u64 = 5_000;
/// Helper: from writing its hello to reading start.
pub const VOICE_CONTROL_START_TIMEOUT_MS: u64 = 10_000;
/// Owner: from writing a command to reading its ack.
pub const VOICE_CONTROL_ACK_TIMEOUT_MS: u64 = 5_000;
/// Helper: how often it checks that its owner is still its parent.
pub const VOICE_CONTROL_OWNER_POLL_MS: u64 = 250;
/// Helper: from noticing owner loss, or reading leave, to the last microphone frame handed
/// to the track.
pub const VOICE_CONTROL_GATE_SHUT_MS: u64 = 100;
/// Helper: from the same moment to the microphone stream closed and playback stopped.
pub const VOICE_CONTROL_CAPTURE_STOP_MS: u64 = 500;
/// Helper: from noticing owner loss to the process having exited.
pub const VOICE_CONTROL_OWNER_LOSS_EXIT_MS: u64 = 3_000;
/// Helper: from reading leave, standard input still open, to the process having exited.
pub const VOICE_CONTROL_LEAVE_EXIT_MS: u64 = 5_000;
/// Owner: from the start of a stop to SIGTERM.
pub const VOICE_CONTROL_STOP_TERM_AFTER_MS: u64 = 4_000;
/// Owner: from the start of a stop to SIGKILL.
pub const VOICE_CONTROL_STOP_KILL_AFTER_MS: u64 = 5_000;

/// A closed vocabulary: an enum of words that is written as its word and read from a JSON
/// string that is exactly one of them, and from nothing else.
macro_rules! vocabulary {
    (
        $(#[$meta:meta])*
        $name:ident, $what:literal,
        { $($(#[$vmeta:meta])* $variant:ident = $word:literal),+ $(,)? }
    ) => {
        $(#[$meta])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
        pub enum $name {
            $($(#[$vmeta])* $variant),+
        }

        impl $name {
            /// Every word, in the schema's order.
            pub const ALL: &'static [$name] = &[$($name::$variant),+];

            /// The word as it is written.
            pub fn as_str(self) -> &'static str {
                match self {
                    $($name::$variant => $word),+
                }
            }

            /// The value a word names, if it is one of this vocabulary's.
            pub fn from_word(word: &str) -> Option<Self> {
                match word {
                    $($word => Some($name::$variant),)+
                    _ => None,
                }
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str(self.as_str())
            }
        }

        impl Serialize for $name {
            fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
                serializer.serialize_str(self.as_str())
            }
        }

        impl<'de> Deserialize<'de> for $name {
            fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
                let word = String::deserialize(deserializer)?;
                Self::from_word(&word).ok_or_else(|| de::Error::custom($what))
            }
        }
    };
}

vocabulary!(
    /// What a command asks for.
    VoiceControlCommandKind, "not a command kind of this version", {
    /// The first command, and the only one before it: join this channel as this principal.
    Start = "start",
    /// Set whether the user is muted.
    SetMute = "set_mute",
    /// Set whether the user is deafened.
    SetDeafen = "set_deafen",
    /// Write a state snapshot now.
    GetState = "get_state",
    /// Leave voice; the helper then exits.
    Leave = "leave",
});

vocabulary!(
    /// What an event reports.
    VoiceControlEventKind, "not an event kind of this version", {
    /// The helper's first line: which versions it speaks.
    Hello = "hello",
    /// The answer to one command.
    Ack = "ack",
    /// A whole-state snapshot.
    State = "state",
    /// A line the helper cannot act on; it then fails.
    ProtocolError = "protocol_error",
});

vocabulary!(
    /// Where the helper takes the login token from. The token never crosses the protocol.
    VoiceControlCredential, "not a credential source", {
    /// The login `conch login` stored for the server. `CONCH_TOKEN` must then be absent.
    Stored = "stored",
    /// `CONCH_TOKEN` in the helper's environment.
    Env = "env",
});

vocabulary!(
    /// Where the helper is in its life.
    VoiceControlPhase, "not a phase", {
    /// Start was accepted: configuration, login and audio are being set up. Nothing is
    /// captured.
    Starting = "starting",
    /// Asking `conchd` for a session and joining the room.
    Connecting = "connecting",
    /// In the room.
    Connected = "connected",
    /// Not connected, and trying again by the helper's own policy.
    Reconnecting = "reconnecting",
    /// Leaving. The gate is shut.
    Stopping = "stopping",
    /// Left. Final; the helper exits 0.
    Stopped = "stopped",
    /// Stopped for a reason trying again by itself cannot fix. Final; the helper exits 1.
    Failed = "failed",
});

impl VoiceControlPhase {
    /// Whether this is the last phase a helper reports.
    pub fn is_final(self) -> bool {
        matches!(self, VoiceControlPhase::Stopped | VoiceControlPhase::Failed)
    }
}

vocabulary!(
    /// Why the helper is in its phase.
    VoiceControlReason, "not a reason", {
    /// The room was deleted: `conchd` rotated it.
    RoomDeleted = "room_deleted",
    /// `conchd` removed this participant.
    ParticipantRemoved = "participant_removed",
    /// The connection was lost.
    ConnectionLost = "connection_lost",
    /// The SDK's own reconnect did not finish in time.
    ReconnectTimedOut = "reconnect_timed_out",
    /// Joining the room failed.
    ConnectFailed = "connect_failed",
    /// `conchd` could not be reached.
    ServerUnreachable = "server_unreachable",
    /// `conchd` cannot reach LiveKit at the moment.
    VoiceUnavailable = "voice_unavailable",
    /// `conchd` holds no session for a press's report.
    NoSession = "no_session",
    /// LiveKit is shutting down.
    ServerShutdown = "server_shutdown",
    /// The owner sent leave.
    Left = "left",
    /// The control pipe ended, or the owner is no longer the helper's parent.
    OwnerLost = "owner_lost",
    /// SIGTERM or SIGINT.
    Signal = "signal",
    /// No login, or `conchd` answered 401.
    NotSignedIn = "not_signed_in",
    /// The login is not the principal the owner named.
    IdentityMismatch = "identity_mismatch",
    /// The stored login was named and `CONCH_TOKEN` is set.
    BadEnvironment = "bad_environment",
    /// `voice.toml` or the configuration directory was refused.
    BadConfig = "bad_config",
    /// `conchd` runs without authentication.
    VoiceRequiresAuth = "voice_requires_auth",
    /// The login is an agent's.
    NotAPerson = "not_a_person",
    /// The account may not use voice.
    VoiceNotAllowed = "voice_not_allowed",
    /// Not a member of the channel, or no such channel.
    NotAMember = "not_a_member",
    /// `conchd` has no LiveKit.
    VoiceNotConfigured = "voice_not_configured",
    /// The same account joined voice elsewhere.
    DuplicateIdentity = "duplicate_identity",
    /// PipeWire or the speakers could not be opened.
    AudioUnavailable = "audio_unavailable",
    /// No start in time.
    StartTimeout = "start_timeout",
    /// After a protocol_error.
    ProtocolError = "protocol_error",
    /// Anything else that ended the helper.
    InternalError = "internal_error",
});

impl VoiceControlReason {
    /// Whether this reason may come with `reconnecting`.
    pub fn is_retry(self) -> bool {
        use VoiceControlReason::*;
        matches!(
            self,
            RoomDeleted
                | ParticipantRemoved
                | ConnectionLost
                | ReconnectTimedOut
                | ConnectFailed
                | ServerUnreachable
                | VoiceUnavailable
                | NoSession
                | ServerShutdown
        )
    }

    /// Whether this reason may come with `stopped`.
    pub fn is_stop(self) -> bool {
        matches!(
            self,
            VoiceControlReason::Left | VoiceControlReason::OwnerLost | VoiceControlReason::Signal
        )
    }

    /// Whether this reason may come with `failed`.
    pub fn is_failure(self) -> bool {
        use VoiceControlReason::*;
        matches!(
            self,
            NotSignedIn
                | IdentityMismatch
                | BadEnvironment
                | BadConfig
                | VoiceRequiresAuth
                | NotAPerson
                | VoiceNotAllowed
                | NotAMember
                | VoiceNotConfigured
                | DuplicateIdentity
                | AudioUnavailable
                | ServerUnreachable
                | StartTimeout
                | ProtocolError
                | InternalError
        )
    }
}

vocabulary!(
    /// The state of the microphone stream, which is not the state of transmission.
    VoiceControlCapture, "not a capture state", {
    /// No microphone stream is open.
    Off = "off",
    /// The microphone stream is open and has a device.
    On = "on",
    /// The stream could not be opened, or its device has gone.
    Unavailable = "unavailable",
});

vocabulary!(
    /// One reason a press would do nothing now. The order is the order of importance, and
    /// the order a state lists them in.
    VoiceControlBlock, "not a block reason", {
    /// Not connected to the room.
    NotConnected = "not_connected",
    /// The session may not publish.
    NoPublishGrant = "no_publish_grant",
    /// No microphone.
    NoMicrophone = "no_microphone",
    /// Deafened.
    Deafened = "deafened",
    /// Muted.
    Muted = "muted",
    /// The key device was lost.
    KeyDeviceLost = "key_device_lost",
    /// One press reached the transmit limit.
    MaxTransmit = "max_transmit",
});

vocabulary!(
    /// Whether hold-to-talk can work.
    VoiceControlPtt, "not a push-to-talk state", {
    /// The key device has not been tried yet.
    Unknown = "unknown",
    /// The key device is open and a talk key is bound.
    Ready = "ready",
    /// `voice.toml` names no key device or binds no talk key.
    NotConfigured = "not_configured",
    /// Configured, and not read since the helper started.
    Unavailable = "unavailable",
    /// It was read and is not any more; every press is refused.
    Lost = "lost",
});

vocabulary!(
    /// Why the key device is not being read: the `reason` of `join --json`'s `key_device`.
    VoiceControlPttProblem, "not a key device problem", {
    /// It could not be opened.
    CannotOpen = "cannot_open",
    /// It is not something keys are read from.
    Refused = "refused",
    /// A read failed.
    ReadFailed = "read_failed",
    /// A read returned nothing.
    Ended = "ended",
    /// A read returned part of a record.
    PartialRecord = "partial_record",
});

vocabulary!(
    /// Why a line was refused: in an ack, in a protocol_error, and from the readers.
    VoiceControlErrorCode, "not an error code", {
    /// Longer than [`VOICE_CONTROL_MAX_LINE_BYTES`].
    TooLong = "too_long",
    /// Not one JSON object, not UTF-8, or a key given twice.
    Malformed = "malformed",
    /// `schema` missing or not the version in use.
    UnknownSchema = "unknown_schema",
    /// A command kind this version does not know.
    UnknownKind = "unknown_kind",
    /// A field missing, `null`, of the wrong type or out of range.
    Invalid = "invalid",
    /// An id not greater than the one before.
    IdNotIncreasing = "id_not_increasing",
    /// The first line is not a start.
    NotStarted = "not_started",
    /// A second start.
    AlreadyStarted = "already_started",
    /// set_mute or set_deafen after leaving began.
    Stopping = "stopping",
});

impl VoiceControlErrorCode {
    /// Whether the code may be an ack's: a command refused, after which the helper goes on.
    pub fn is_ack_refusal(self) -> bool {
        use VoiceControlErrorCode::*;
        matches!(self, UnknownKind | Invalid | AlreadyStarted | Stopping)
    }

    /// Whether the code may be a protocol_error's: a line after which the helper fails.
    /// `invalid` is both: fatal when the line has no usable id, an ack's refusal otherwise.
    pub fn is_fatal(self) -> bool {
        use VoiceControlErrorCode::*;
        matches!(
            self,
            TooLong | Malformed | UnknownSchema | Invalid | IdNotIncreasing | NotStarted
        )
    }
}

/// Why a reader refused a line: the code for it, and the refused command's id when the
/// line had a usable one (and so can be answered by an ack). It never holds the line.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VoiceControlRefusal {
    /// What was wrong.
    pub code: VoiceControlErrorCode,
    /// The command's id, when it had a usable one.
    pub id: Option<i64>,
}

impl fmt::Display for VoiceControlRefusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.id {
            Some(id) => write!(f, "voice control line refused: {} (id {id})", self.code),
            None => write!(f, "voice control line refused: {}", self.code),
        }
    }
}

fn refused(code: VoiceControlErrorCode, id: Option<i64>) -> VoiceControlRefusal {
    VoiceControlRefusal { code, id }
}

/// The first command (owner to helper): join `channel` on `server` as `principal_id`, with
/// the login `credential` names, muted and deafened as given. It carries no token.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStartV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::Start`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The `conchd` address, as the owner's credential store keys it.
    pub server: String,
    /// The channel whose voice to join.
    pub channel: String,
    /// The owner's user, as `conchd`'s whoami told the owner.
    pub principal_id: i64,
    /// Where the login token is.
    pub credential: VoiceControlCredential,
    /// Whether to start muted.
    pub muted: bool,
    /// Whether to start deafened.
    pub deafened: bool,
}

wire_object!(VoiceControlStartV1, "a voice control start", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    server: required,
    channel: required,
    principal_id: required,
    credential: required,
    muted: required,
    deafened: required,
});

/// Sets whether the user is muted (owner to helper): a wanted state, not a toggle.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetMuteV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetMute`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub muted: bool,
}

wire_object!(VoiceControlSetMuteV1, "a voice control set_mute", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    muted: required,
});

/// Sets whether the user is deafened (owner to helper): a wanted state, not a toggle.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetDeafenV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetDeafen`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub deafened: bool,
}

wire_object!(VoiceControlSetDeafenV1, "a voice control set_deafen", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    deafened: required,
});

/// Asks for a state snapshot now (owner to helper).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlGetStateV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::GetState`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlGetStateV1, "a voice control get_state", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// Leaves voice (owner to helper): the gate shuts at once, the room is left, the helper
/// exits 0.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlLeaveV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::Leave`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlLeaveV1, "a voice control leave", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// The helper's first line (helper to owner). Its shape is frozen: a helper of any later
/// version writes its hello exactly like this, in v1, listing every version it speaks.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlHelloV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::Hello`].
    pub kind: VoiceControlEventKind,
    /// Always 1.
    pub seq: i64,
    /// Every schema name the helper speaks.
    pub versions: Vec<String>,
    /// The helper's build: 1 to 64 bytes of printable ASCII.
    pub helper_version: String,
}

wire_object!(VoiceControlHelloV1, "a voice control hello", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    versions: required,
    helper_version: required,
});

impl VoiceControlHelloV1 {
    /// The hello of a helper that speaks v1 and nothing else.
    pub fn v1(helper_version: impl Into<String>) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V1.to_owned(),
            kind: VoiceControlEventKind::Hello,
            seq: 1,
            versions: vec![VOICE_CONTROL_SCHEMA_V1.to_owned()],
            helper_version: helper_version.into(),
        }
    }

    /// Whether the hello offers `version`.
    pub fn offers(&self, version: &str) -> bool {
        self.versions.iter().any(|offered| offered == version)
    }
}

/// The answer to one command (helper to owner).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlAckV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::Ack`].
    pub kind: VoiceControlEventKind,
    /// The event's seq.
    pub seq: i64,
    /// The id of the command answered.
    pub id: i64,
    /// Whether the command was carried out.
    pub ok: bool,
    /// Why not: present exactly when `ok` is false.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<VoiceControlErrorCode>,
}

wire_object!(VoiceControlAckV1, "a voice control ack", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    id: required,
    ok: required,
    error: maybe,
});

impl VoiceControlAckV1 {
    /// The answer to command `id`: carried out if `refusal` is `None`.
    pub fn new(seq: i64, id: i64, refusal: Option<VoiceControlErrorCode>) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V1.to_owned(),
            kind: VoiceControlEventKind::Ack,
            seq,
            id,
            ok: refusal.is_none(),
            error: refusal,
        }
    }
}

/// A whole-state snapshot (helper to owner): what the helper is doing and what a press
/// would do, as it is, not as it was asked to be.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStateV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::State`].
    pub kind: VoiceControlEventKind,
    /// The event's seq.
    pub seq: i64,
    /// Where the helper is in its life.
    pub phase: VoiceControlPhase,
    /// Why: required for stopping, stopped and failed, optional for reconnecting.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<VoiceControlReason>,
    /// While reconnecting for a reason: how long until `conchd` is asked again.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub retry_in_ms: Option<i64>,
    /// The microphone stream.
    pub capture: VoiceControlCapture,
    /// Whether audio is leaving now, release tail included.
    pub transmitting: bool,
    /// Whether the user is muted.
    pub muted: bool,
    /// Whether the user is deafened.
    pub deafened: bool,
    /// Every reason a press would do nothing now, most important first.
    pub blocked: Vec<VoiceControlBlock>,
    /// Whether hold-to-talk can work.
    pub ptt: VoiceControlPtt,
    /// Why the key device is not being read: present exactly when `ptt` is unavailable or
    /// lost.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ptt_problem: Option<VoiceControlPttProblem>,
}

wire_object!(VoiceControlStateV1, "a voice control state", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    phase: required,
    reason: maybe,
    retry_in_ms: maybe,
    capture: required,
    transmitting: required,
    muted: required,
    deafened: required,
    blocked: required,
    ptt: required,
    ptt_problem: maybe,
});

impl VoiceControlStateV1 {
    /// Whether `block` is among the reasons a press would do nothing.
    pub fn blocks(&self, block: VoiceControlBlock) -> bool {
        self.blocked.contains(&block)
    }
}

/// A line the helper cannot act on (helper to owner); a failed state follows.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlProtocolErrorV1 {
    /// Always [`VOICE_CONTROL_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::ProtocolError`].
    pub kind: VoiceControlEventKind,
    /// The event's seq.
    pub seq: i64,
    /// What was wrong: a code that ends a session.
    pub code: VoiceControlErrorCode,
}

wire_object!(VoiceControlProtocolErrorV1, "a voice control protocol_error", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    code: required,
});

impl VoiceControlProtocolErrorV1 {
    /// The protocol_error for `code`.
    pub fn new(seq: i64, code: VoiceControlErrorCode) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V1.to_owned(),
            kind: VoiceControlEventKind::ProtocolError,
            seq,
            code,
        }
    }
}

/// One command, as [`parse_command`] read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlCommand {
    /// `start`.
    Start(VoiceControlStartV1),
    /// `set_mute`.
    SetMute(VoiceControlSetMuteV1),
    /// `set_deafen`.
    SetDeafen(VoiceControlSetDeafenV1),
    /// `get_state`.
    GetState(VoiceControlGetStateV1),
    /// `leave`.
    Leave(VoiceControlLeaveV1),
}

impl VoiceControlCommand {
    /// The command's kind.
    pub fn kind(&self) -> VoiceControlCommandKind {
        match self {
            Self::Start(_) => VoiceControlCommandKind::Start,
            Self::SetMute(_) => VoiceControlCommandKind::SetMute,
            Self::SetDeafen(_) => VoiceControlCommandKind::SetDeafen,
            Self::GetState(_) => VoiceControlCommandKind::GetState,
            Self::Leave(_) => VoiceControlCommandKind::Leave,
        }
    }

    /// The command's id.
    pub fn id(&self) -> i64 {
        match self {
            Self::Start(c) => c.id,
            Self::SetMute(c) => c.id,
            Self::SetDeafen(c) => c.id,
            Self::GetState(c) => c.id,
            Self::Leave(c) => c.id,
        }
    }

    fn check(&self) -> Result<(), String> {
        match self {
            Self::Start(c) => c.check(),
            Self::SetMute(c) => c.check(),
            Self::SetDeafen(c) => c.check(),
            Self::GetState(c) => c.check(),
            Self::Leave(c) => c.check(),
        }
    }
}

/// One event, as [`parse_event`] read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlEvent {
    /// `hello`.
    Hello(VoiceControlHelloV1),
    /// `ack`.
    Ack(VoiceControlAckV1),
    /// `state`.
    State(VoiceControlStateV1),
    /// `protocol_error`.
    ProtocolError(VoiceControlProtocolErrorV1),
    /// An event of a kind this version does not know, which a reader skips.
    Unknown {
        /// Its seq.
        seq: i64,
        /// Its kind, as written.
        kind: String,
    },
}

impl VoiceControlEvent {
    /// The event's seq.
    pub fn seq(&self) -> i64 {
        match self {
            Self::Hello(e) => e.seq,
            Self::Ack(e) => e.seq,
            Self::State(e) => e.seq,
            Self::ProtocolError(e) => e.seq,
            Self::Unknown { seq, .. } => *seq,
        }
    }

    /// The event's kind, as written.
    pub fn kind(&self) -> &str {
        match self {
            Self::Hello(_) => VoiceControlEventKind::Hello.as_str(),
            Self::Ack(_) => VoiceControlEventKind::Ack.as_str(),
            Self::State(_) => VoiceControlEventKind::State.as_str(),
            Self::ProtocolError(_) => VoiceControlEventKind::ProtocolError.as_str(),
            Self::Unknown { kind, .. } => kind,
        }
    }
}

/// A message of this protocol: one of the nine types above.
pub trait VoiceControlMessage: Serialize {
    /// Checks the rules `pkg/schema`'s `Validate` checks for this type.
    ///
    /// # Errors
    ///
    /// [`Error::Invalid`] naming the rule broken. The problem holds field names and
    /// numbers only.
    fn validate(&self) -> Result<(), Error>;
}

macro_rules! message {
    ($($type:ident),+) => {
        $(impl VoiceControlMessage for $type {
            fn validate(&self) -> Result<(), Error> {
                self.check().map_err(|problem| Error::Invalid {
                    what: "voice control message",
                    problem,
                })
            }
        })+
    };
}

message!(
    VoiceControlStartV1,
    VoiceControlSetMuteV1,
    VoiceControlSetDeafenV1,
    VoiceControlGetStateV1,
    VoiceControlLeaveV1,
    VoiceControlHelloV1,
    VoiceControlAckV1,
    VoiceControlStateV1,
    VoiceControlProtocolErrorV1
);

/// One message as it is written: compact JSON, without its line feed. It refuses a message
/// that fails [`VoiceControlMessage::validate`], or that would be longer than
/// [`VOICE_CONTROL_MAX_LINE_BYTES`], so that a writer cannot send what a reader must refuse.
/// `<`, `>` and `&` are written as themselves, as Go's writer writes them.
///
/// # Errors
///
/// [`Error::Invalid`] for a message a reader would refuse.
pub fn to_line(message: &impl VoiceControlMessage) -> Result<String, Error> {
    message.validate()?;
    let line = serde_json::to_string(message).map_err(|_| Error::Invalid {
        what: "voice control message",
        problem: "it could not be encoded".into(),
    })?;
    if line.len() > VOICE_CONTROL_MAX_LINE_BYTES {
        return Err(Error::Invalid {
            what: "voice control message",
            problem: format!(
                "it is {} bytes, more than {VOICE_CONTROL_MAX_LINE_BYTES}",
                line.len()
            ),
        });
    }
    Ok(line)
}

/// Reads one command line, without its line feed: the helper's reader. See the module
/// documentation for what it refuses, and in which order.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not a command this version can carry out.
pub fn parse_command(line: &[u8]) -> Result<VoiceControlCommand, VoiceControlRefusal> {
    let (text, head) = frame(line)?;
    head.check_schema()?;
    let id = match head.id {
        Some(Loose::Whole(id)) if id_in_range(id) => id,
        _ => return Err(refused(VoiceControlErrorCode::Invalid, None)),
    };
    let kind = match &head.kind {
        Some(Loose::Text(word)) => VoiceControlCommandKind::from_word(word),
        _ => None,
    }
    .ok_or(refused(VoiceControlErrorCode::UnknownKind, Some(id)))?;
    let command = match kind {
        VoiceControlCommandKind::Start => VoiceControlCommand::Start(typed(text, Some(id))?),
        VoiceControlCommandKind::SetMute => VoiceControlCommand::SetMute(typed(text, Some(id))?),
        VoiceControlCommandKind::SetDeafen => {
            VoiceControlCommand::SetDeafen(typed(text, Some(id))?)
        }
        VoiceControlCommandKind::GetState => VoiceControlCommand::GetState(typed(text, Some(id))?),
        VoiceControlCommandKind::Leave => VoiceControlCommand::Leave(typed(text, Some(id))?),
    };
    command
        .check()
        .map_err(|_| refused(VoiceControlErrorCode::Invalid, Some(id)))?;
    Ok(command)
}

/// Reads one event line, without its line feed: the owner's reader. An event of a kind
/// this version does not know comes back as [`VoiceControlEvent::Unknown`], to be skipped.
/// Whether seq follows the one before, and whether the first event is a hello, is the
/// reader's to check: it needs the history.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not an event of this version.
pub fn parse_event(line: &[u8]) -> Result<VoiceControlEvent, VoiceControlRefusal> {
    let invalid = refused(VoiceControlErrorCode::Invalid, None);
    let (text, head) = frame(line)?;
    head.check_schema()?;
    let seq = match head.seq {
        Some(Loose::Whole(seq)) if id_in_range(seq) => seq,
        _ => return Err(invalid),
    };
    let word = match head.kind {
        Some(Loose::Text(word)) if !word.is_empty() => word,
        _ => return Err(invalid),
    };
    let Some(kind) = VoiceControlEventKind::from_word(&word) else {
        return Ok(VoiceControlEvent::Unknown { seq, kind: word });
    };
    let (event, checked) = match kind {
        VoiceControlEventKind::Hello => {
            let hello: VoiceControlHelloV1 = typed(text, None)?;
            let checked = hello.check();
            (VoiceControlEvent::Hello(hello), checked)
        }
        VoiceControlEventKind::Ack => {
            let ack: VoiceControlAckV1 = typed(text, None)?;
            let checked = ack.check();
            (VoiceControlEvent::Ack(ack), checked)
        }
        VoiceControlEventKind::State => {
            let state: VoiceControlStateV1 = typed(text, None)?;
            let checked = state.check();
            (VoiceControlEvent::State(state), checked)
        }
        VoiceControlEventKind::ProtocolError => {
            let error: VoiceControlProtocolErrorV1 = typed(text, None)?;
            let checked = error.check();
            (VoiceControlEvent::ProtocolError(error), checked)
        }
    };
    checked.map_err(|_| invalid)?;
    Ok(event)
}

/// The framing rules both directions share: the length, UTF-8, one line, one JSON object
/// with no key twice. Gives the text and what the object says of its head.
fn frame(line: &[u8]) -> Result<(&str, Head), VoiceControlRefusal> {
    let malformed = refused(VoiceControlErrorCode::Malformed, None);
    if line.len() > VOICE_CONTROL_MAX_LINE_BYTES {
        return Err(refused(VoiceControlErrorCode::TooLong, None));
    }
    let text = std::str::from_utf8(line).map_err(|_| malformed)?;
    if text.contains('\n') {
        return Err(malformed);
    }
    let head: Head = serde_json::from_str(text).map_err(|_| malformed)?;
    Ok((text, head))
}

/// The second, typed reading of a line whose framing and head were accepted: every field
/// the kind declares, of its type and not `null`. Any failure is `invalid`.
fn typed<T: DeserializeOwned>(text: &str, id: Option<i64>) -> Result<T, VoiceControlRefusal> {
    serde_json::from_str(text).map_err(|_| refused(VoiceControlErrorCode::Invalid, id))
}

fn id_in_range(id: i64) -> bool {
    (1..=VOICE_CONTROL_MAX_ID).contains(&id)
}

/// A JSON value seen only to learn whether it is a string or a whole number.
enum Loose {
    Text(String),
    Whole(i64),
    Other,
}

impl<'de> Deserialize<'de> for Loose {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct Any;
        impl<'de> Visitor<'de> for Any {
            type Value = Loose;

            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a JSON value")
            }

            fn visit_bool<E>(self, _: bool) -> Result<Loose, E> {
                Ok(Loose::Other)
            }

            fn visit_i64<E>(self, n: i64) -> Result<Loose, E> {
                Ok(Loose::Whole(n))
            }

            fn visit_u64<E>(self, n: u64) -> Result<Loose, E> {
                Ok(i64::try_from(n).map_or(Loose::Other, Loose::Whole))
            }

            fn visit_f64<E>(self, _: f64) -> Result<Loose, E> {
                Ok(Loose::Other)
            }

            fn visit_str<E>(self, text: &str) -> Result<Loose, E> {
                Ok(Loose::Text(text.to_owned()))
            }

            fn visit_string<E>(self, text: String) -> Result<Loose, E> {
                Ok(Loose::Text(text))
            }

            fn visit_unit<E>(self) -> Result<Loose, E> {
                Ok(Loose::Other)
            }

            fn visit_none<E>(self) -> Result<Loose, E> {
                Ok(Loose::Other)
            }

            fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Loose, A::Error> {
                while seq.next_element::<IgnoredAny>()?.is_some() {}
                Ok(Loose::Other)
            }

            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Loose, A::Error> {
                while map.next_entry::<IgnoredAny, IgnoredAny>()?.is_some() {}
                Ok(Loose::Other)
            }
        }
        deserializer.deserialize_any(Any)
    }
}

/// What a line's object says of the fields every message has, read before anything else so
/// that the right code can be given for what is wrong. Every key is looked at for being
/// given twice; the values of the others are only skipped over.
#[derive(Default)]
struct Head {
    schema: Option<Loose>,
    kind: Option<Loose>,
    id: Option<Loose>,
    seq: Option<Loose>,
}

impl Head {
    fn check_schema(&self) -> Result<(), VoiceControlRefusal> {
        match &self.schema {
            Some(Loose::Text(schema)) if schema == VOICE_CONTROL_SCHEMA_V1 => Ok(()),
            _ => Err(refused(VoiceControlErrorCode::UnknownSchema, None)),
        }
    }
}

impl<'de> Deserialize<'de> for Head {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct Object;
        impl<'de> Visitor<'de> for Object {
            type Value = Head;

            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a JSON object")
            }

            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Head, A::Error> {
                let mut head = Head::default();
                let mut keys = BTreeSet::new();
                while let Some(key) = map.next_key::<String>()? {
                    if !keys.insert(key.clone()) {
                        return Err(de::Error::custom("a key given twice"));
                    }
                    let slot = match key.as_str() {
                        "schema" => &mut head.schema,
                        "kind" => &mut head.kind,
                        "id" => &mut head.id,
                        "seq" => &mut head.seq,
                        _ => {
                            map.next_value::<IgnoredAny>()?;
                            continue;
                        }
                    };
                    *slot = Some(map.next_value()?);
                }
                Ok(head)
            }
        }
        deserializer.deserialize_map(Object)
    }
}

fn check_command_head(
    schema: &str,
    kind: VoiceControlCommandKind,
    want: VoiceControlCommandKind,
    id: i64,
) -> Result<(), String> {
    if schema != VOICE_CONTROL_SCHEMA_V1 {
        return Err(format!(
            "voice control command schema must be {VOICE_CONTROL_SCHEMA_V1:?}"
        ));
    }
    if kind != want {
        return Err(format!("voice control command kind must be {want:?}"));
    }
    check_id(id)
}

fn check_event_head(
    schema: &str,
    kind: VoiceControlEventKind,
    want: VoiceControlEventKind,
    seq: i64,
) -> Result<(), String> {
    if schema != VOICE_CONTROL_SCHEMA_V1 {
        return Err(format!(
            "voice control event schema must be {VOICE_CONTROL_SCHEMA_V1:?}"
        ));
    }
    if kind != want {
        return Err(format!("voice control event kind must be {want:?}"));
    }
    if !id_in_range(seq) {
        return Err(format!(
            "voice control seq must be 1 to {VOICE_CONTROL_MAX_ID}, got {seq}"
        ));
    }
    Ok(())
}

fn check_id(id: i64) -> Result<(), String> {
    if id_in_range(id) {
        Ok(())
    } else {
        Err(format!(
            "voice control id must be 1 to {VOICE_CONTROL_MAX_ID}, got {id}"
        ))
    }
}

impl VoiceControlStartV1 {
    fn check(&self) -> Result<(), String> {
        check_command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::Start,
            self.id,
        )?;
        check_server(&self.server)?;
        check_channel(&self.channel)?;
        if self.principal_id <= 0 {
            return Err(format!(
                "voice control start principal_id must be positive, got {}",
                self.principal_id
            ));
        }
        Ok(())
    }
}

impl VoiceControlSetMuteV1 {
    fn check(&self) -> Result<(), String> {
        check_command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetMute,
            self.id,
        )
    }
}

impl VoiceControlSetDeafenV1 {
    fn check(&self) -> Result<(), String> {
        check_command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetDeafen,
            self.id,
        )
    }
}

impl VoiceControlGetStateV1 {
    fn check(&self) -> Result<(), String> {
        check_command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::GetState,
            self.id,
        )
    }
}

impl VoiceControlLeaveV1 {
    fn check(&self) -> Result<(), String> {
        check_command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::Leave,
            self.id,
        )
    }
}

/// Whether `version` is shaped as a version a hello may offer: `conch.voice_control.v`
/// and a number from 1 to 9999 with no leading zero.
fn is_version_name(version: &str) -> bool {
    version
        .strip_prefix("conch.voice_control.v")
        .is_some_and(|number| {
            (1..=4).contains(&number.len())
                && !number.starts_with('0')
                && number.bytes().all(|digit| digit.is_ascii_digit())
        })
}

impl VoiceControlHelloV1 {
    fn check(&self) -> Result<(), String> {
        if self.schema != VOICE_CONTROL_SCHEMA_V1 {
            return Err(format!(
                "voice control hello schema must be {VOICE_CONTROL_SCHEMA_V1:?}"
            ));
        }
        if self.kind != VoiceControlEventKind::Hello {
            return Err("voice control hello kind must be \"hello\"".into());
        }
        if self.seq != 1 {
            return Err(format!(
                "voice control hello seq must be 1, got {}",
                self.seq
            ));
        }
        if self.versions.is_empty() || self.versions.len() > VOICE_CONTROL_MAX_VERSIONS {
            return Err(format!(
                "voice control hello must offer 1 to {VOICE_CONTROL_MAX_VERSIONS} versions, got {}",
                self.versions.len()
            ));
        }
        for (i, version) in self.versions.iter().enumerate() {
            if !is_version_name(version) {
                return Err(format!(
                    "voice control hello version {i} is not a voice control schema name"
                ));
            }
            if self.versions[..i].contains(version) {
                return Err(format!("voice control hello offers version {i} twice"));
            }
        }
        let version = self.helper_version.as_bytes();
        if version.is_empty() || version.len() > VOICE_CONTROL_MAX_HELPER_VERSION_BYTES {
            return Err(format!(
                "voice control hello helper_version must be 1 to {VOICE_CONTROL_MAX_HELPER_VERSION_BYTES} bytes"
            ));
        }
        if !version.iter().all(|byte| (0x20..=0x7e).contains(byte)) {
            return Err("voice control hello helper_version must be printable ASCII".into());
        }
        Ok(())
    }
}

impl VoiceControlAckV1 {
    fn check(&self) -> Result<(), String> {
        check_event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::Ack,
            self.seq,
        )?;
        check_id(self.id)?;
        match (self.ok, self.error) {
            (true, Some(_)) => Err("voice control ack with ok must have no error".into()),
            (false, error) if !error.is_some_and(VoiceControlErrorCode::is_ack_refusal) => Err(
                "voice control ack error is not one of unknown_kind, invalid, already_started, stopping"
                    .into(),
            ),
            _ => Ok(()),
        }
    }
}

impl VoiceControlStateV1 {
    fn check(&self) -> Result<(), String> {
        use VoiceControlBlock as Block;
        use VoiceControlPhase as Phase;
        check_event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::State,
            self.seq,
        )?;
        if matches!(
            self.phase,
            Phase::Connecting | Phase::Connected | Phase::Reconnecting
        ) && self.ptt == VoiceControlPtt::Unknown
        {
            return Err(format!(
                "voice control state in phase {:?} must say whether the key device is read",
                self.phase.as_str()
            ));
        }
        let mut last = None;
        for block in &self.blocked {
            if last.is_some_and(|last| last >= *block) {
                return Err(
                    "voice control state block reasons must be listed once each, most important first"
                        .into(),
                );
            }
            last = Some(*block);
        }
        self.check_reason()?;
        let problem = matches!(
            self.ptt,
            VoiceControlPtt::Unavailable | VoiceControlPtt::Lost
        );
        if problem && self.ptt_problem.is_none() {
            return Err(format!(
                "voice control state with ptt {:?} needs a ptt_problem",
                self.ptt.as_str()
            ));
        }
        if !problem && self.ptt_problem.is_some() {
            return Err(format!(
                "voice control state with ptt {:?} must have no ptt_problem",
                self.ptt.as_str()
            ));
        }
        if self.muted != self.blocks(Block::Muted) {
            return Err("voice control state muted must agree with the muted block reason".into());
        }
        if self.deafened != self.blocks(Block::Deafened) {
            return Err(
                "voice control state deafened must agree with the deafened block reason".into(),
            );
        }
        if (self.ptt == VoiceControlPtt::Lost) != self.blocks(Block::KeyDeviceLost) {
            return Err(
                "voice control state ptt lost must agree with the key_device_lost block reason"
                    .into(),
            );
        }
        if self.phase != Phase::Connected && !self.blocks(Block::NotConnected) {
            return Err(format!(
                "voice control state in phase {:?} must block with not_connected",
                self.phase.as_str()
            ));
        }
        if self.capture == VoiceControlCapture::On && self.blocks(Block::NoMicrophone) {
            return Err(
                "voice control state cannot have capture on and block with no_microphone".into(),
            );
        }
        if self.capture == VoiceControlCapture::Unavailable && !self.blocks(Block::NoMicrophone) {
            return Err(
                "voice control state with capture unavailable must block with no_microphone".into(),
            );
        }
        if self.capture != VoiceControlCapture::Off
            && (self.phase == Phase::Starting || self.phase.is_final())
        {
            return Err(format!(
                "voice control state in phase {:?} must have capture off",
                self.phase.as_str()
            ));
        }
        if self.transmitting {
            if !self.blocked.is_empty() {
                return Err(
                    "voice control state cannot be transmitting while a reason blocks it".into(),
                );
            }
            if self.capture != VoiceControlCapture::On {
                return Err("voice control state cannot be transmitting without capture on".into());
            }
            if self.ptt != VoiceControlPtt::Ready {
                return Err("voice control state cannot be transmitting without ptt ready".into());
            }
        }
        Ok(())
    }

    fn check_reason(&self) -> Result<(), String> {
        use VoiceControlPhase as Phase;
        let reason = self.reason;
        let fits = match self.phase {
            Phase::Starting | Phase::Connecting | Phase::Connected => reason.is_none(),
            Phase::Reconnecting => reason.is_none_or(VoiceControlReason::is_retry),
            Phase::Stopping => reason.is_some_and(|r| r.is_stop() || r.is_failure()),
            Phase::Stopped => reason.is_some_and(VoiceControlReason::is_stop),
            Phase::Failed => reason.is_some_and(VoiceControlReason::is_failure),
        };
        if !fits {
            return Err(format!(
                "voice control state reason {:?} does not fit phase {:?}",
                reason.map_or("", VoiceControlReason::as_str),
                self.phase.as_str()
            ));
        }
        if let Some(retry_in_ms) = self.retry_in_ms {
            if self.phase != Phase::Reconnecting || reason.is_none() {
                return Err(
                    "voice control state retry_in_ms is only for reconnecting with a reason".into(),
                );
            }
            if !(0..=VOICE_CONTROL_MAX_RETRY_IN_MS).contains(&retry_in_ms) {
                return Err(format!(
                    "voice control state retry_in_ms must be 0 to {VOICE_CONTROL_MAX_RETRY_IN_MS}"
                ));
            }
        }
        Ok(())
    }
}

impl VoiceControlProtocolErrorV1 {
    fn check(&self) -> Result<(), String> {
        check_event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::ProtocolError,
            self.seq,
        )?;
        if !self.code.is_fatal() {
            return Err(format!(
                "voice control protocol_error code {:?} is not one that ends the session",
                self.code.as_str()
            ));
        }
        Ok(())
    }
}

/// Accepts what `validateVoiceControlServer` accepts: an http or https address as the
/// owner's credential store keys it, lower-case host, no user, query or fragment, no
/// trailing slash. The address is looked at as Go's `url.Parse` reads it, step for step,
/// since that is the reader the rule was written against. It does not echo the address.
fn check_server(server: &str) -> Result<(), String> {
    if server.is_empty() || server.len() > VOICE_CONTROL_MAX_SERVER_BYTES {
        return Err(format!(
            "voice control start server must be 1 to {VOICE_CONTROL_MAX_SERVER_BYTES} bytes"
        ));
    }
    let not_a_url = || "voice control start server is not a URL".to_owned();
    let parsed = GoUrl::parse(server).ok_or_else(not_a_url)?;
    if parsed.scheme != "http" && parsed.scheme != "https" {
        return Err("voice control start server must be an http or https URL in lower case".into());
    }
    let host_lower = std::str::from_utf8(&parsed.host).is_ok_and(|host| {
        host.chars()
            .all(|c| c.to_lowercase().eq(std::iter::once(c)))
    });
    if parsed.host.is_empty() || !host_lower {
        return Err("voice control start server must have a host, in lower case".into());
    }
    if parsed.user
        || !parsed.query.is_empty()
        || parsed.force_query
        || !parsed.fragment.is_empty()
        || !parsed.opaque.is_empty()
    {
        return Err("voice control start server must have no user, query or fragment".into());
    }
    if parsed.path.ends_with(b"/") {
        return Err("voice control start server must not end with a slash".into());
    }
    Ok(())
}

/// What `check_server` needs of what Go's `url.Parse` makes of an address.
struct GoUrl<'a> {
    /// In lower case, as Go leaves it.
    scheme: String,
    opaque: &'a str,
    user: bool,
    /// Unescaped, as Go's `URL.Host` is.
    host: Vec<u8>,
    /// Unescaped, as Go's `URL.Path` is.
    path: Vec<u8>,
    query: &'a str,
    force_query: bool,
    /// Unescaped, as Go's `URL.Fragment` is.
    fragment: Vec<u8>,
}

/// Which part of an address is being unescaped: Go's `encoding` modes, the ones that
/// matter here.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Part {
    Host,
    Zone,
    Path,
    Fragment,
}

impl<'a> GoUrl<'a> {
    /// `url.Parse`: `None` where it returns an error.
    fn parse(raw: &'a str) -> Option<Self> {
        let (rest, fragment) = match raw.split_once('#') {
            Some((rest, fragment)) => (rest, fragment),
            None => (raw, ""),
        };
        let mut url = Self::parse_without_fragment(rest)?;
        if !fragment.is_empty() {
            url.fragment = unescape(fragment, Part::Fragment)?;
        }
        Some(url)
    }

    /// `parse(rawURL, false)`.
    fn parse_without_fragment(raw: &'a str) -> Option<Self> {
        if raw.bytes().any(|byte| byte < 0x20 || byte == 0x7f) {
            return None;
        }
        let mut url = Self {
            scheme: String::new(),
            opaque: "",
            user: false,
            host: Vec::new(),
            path: Vec::new(),
            query: "",
            force_query: false,
            fragment: Vec::new(),
        };
        if raw == "*" {
            url.path = b"*".to_vec();
            return Some(url);
        }
        let (scheme, rest) = scheme_of(raw)?;
        url.scheme = scheme.to_ascii_lowercase();
        let mut rest = rest;
        if rest.ends_with('?') && rest.matches('?').count() == 1 {
            url.force_query = true;
            rest = &rest[..rest.len() - 1];
        } else if let Some((before, query)) = rest.split_once('?') {
            rest = before;
            url.query = query;
        }
        if !rest.starts_with('/') {
            if !url.scheme.is_empty() {
                url.opaque = rest;
                return Some(url);
            }
            let segment = rest.split('/').next().unwrap_or_default();
            if segment.contains(':') {
                return None;
            }
        }
        if (!url.scheme.is_empty() || !rest.starts_with("///")) && rest.starts_with("//") {
            let authority_and_path = &rest[2..];
            let (authority, path) = match authority_and_path.find('/') {
                Some(at) => authority_and_path.split_at(at),
                None => (authority_and_path, ""),
            };
            // Any `@` is user information, or makes the address unparseable: either way
            // the address is refused, so what follows it is not looked at further.
            if let Some(at) = authority.rfind('@') {
                url.user = true;
                url.host = parse_host(&authority[at + 1..])?;
            } else {
                url.host = parse_host(authority)?;
            }
            rest = path;
        }
        url.path = unescape(rest, Part::Path)?;
        Some(url)
    }
}

/// `getScheme`: the scheme and the rest, or `None` for "missing protocol scheme".
fn scheme_of(raw: &str) -> Option<(&str, &str)> {
    for (i, byte) in raw.bytes().enumerate() {
        match byte {
            b'a'..=b'z' | b'A'..=b'Z' => {}
            b'0'..=b'9' | b'+' | b'-' | b'.' if i == 0 => return Some(("", raw)),
            b'0'..=b'9' | b'+' | b'-' | b'.' => {}
            b':' if i == 0 => return None,
            b':' => return Some((&raw[..i], &raw[i + 1..])),
            _ => return Some(("", raw)),
        }
    }
    Some(("", raw))
}

/// `parseHost`: the host and port, unescaped, or `None` where Go returns an error.
fn parse_host(host: &str) -> Option<Vec<u8>> {
    if host.starts_with('[') {
        let close = host.rfind(']')?;
        if !valid_optional_port(&host[close + 1..]) {
            return None;
        }
        if let Some(zone) = host[..close].find("%25") {
            let mut whole = unescape(&host[..zone], Part::Host)?;
            whole.extend(unescape(&host[zone..close], Part::Zone)?);
            whole.extend(unescape(&host[close..], Part::Host)?);
            return Some(whole);
        }
    } else if let Some(colon) = host.rfind(':')
        && !valid_optional_port(&host[colon..])
    {
        return None;
    }
    unescape(host, Part::Host)
}

/// `validOptionalPort`: empty, or a colon and digits.
fn valid_optional_port(port: &str) -> bool {
    port.is_empty()
        || port
            .strip_prefix(':')
            .is_some_and(|digits| digits.bytes().all(|digit| digit.is_ascii_digit()))
}

/// `shouldEscape` for the parts of an address `unescape` checks bytes of.
fn should_escape(byte: u8, part: Part) -> bool {
    if byte.is_ascii_alphanumeric() {
        return false;
    }
    if matches!(part, Part::Host | Part::Zone)
        && matches!(
            byte,
            b'!' | b'$'
                | b'&'
                | b'\''
                | b'('
                | b')'
                | b'*'
                | b'+'
                | b','
                | b';'
                | b'='
                | b':'
                | b'['
                | b']'
                | b'<'
                | b'>'
                | b'"'
        )
    {
        return false;
    }
    !matches!(byte, b'-' | b'_' | b'.' | b'~')
}

/// `unescape`: the bytes `%XX` escapes stand for, or `None` where Go returns an error.
fn unescape(text: &str, part: Part) -> Option<Vec<u8>> {
    let bytes = text.as_bytes();
    let hex = |byte: u8| {
        char::from(byte)
            .to_digit(16)
            .and_then(|d| u8::try_from(d).ok())
    };
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'%' => {
                let high = bytes.get(i + 1).copied().and_then(hex)?;
                let low = bytes.get(i + 2).copied().and_then(hex)?;
                let escaped = &text[i..i + 3];
                if part == Part::Host && high < 8 && escaped != "%25" {
                    return None;
                }
                let value = high << 4 | low;
                if part == Part::Zone
                    && escaped != "%25"
                    && value != b' '
                    && should_escape(value, Part::Host)
                {
                    return None;
                }
                out.push(value);
                i += 3;
            }
            byte => {
                if matches!(part, Part::Host | Part::Zone)
                    && byte < 0x80
                    && should_escape(byte, part)
                {
                    return None;
                }
                out.push(byte);
                i += 1;
            }
        }
    }
    Some(out)
}

/// Accepts what `validateVoiceControlChannel` accepts: any name the server could have
/// given a channel, not blank, not too long, with no control character.
fn check_channel(channel: &str) -> Result<(), String> {
    if channel.trim().is_empty() || channel.len() > VOICE_CONTROL_MAX_CHANNEL_BYTES {
        return Err(format!(
            "voice control start channel must be 1 to {VOICE_CONTROL_MAX_CHANNEL_BYTES} bytes and not blank"
        ));
    }
    if channel.chars().any(char::is_control) {
        return Err("voice control start channel must not contain control characters".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use serde_json::{Value, json};

    use super::*;

    /// `pkg/schema/testdata/voice-control`, read in place.
    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/testdata/voice-control")
    }

    fn fixture_text(name: &str) -> String {
        let path = fixtures().join(name);
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()))
    }

    fn fixture(name: &str) -> Value {
        serde_json::from_str(&fixture_text(name)).unwrap_or_else(|e| panic!("parse {name}: {e}"))
    }

    /// JSON text with the white space outside its strings taken out: Go's indented
    /// encoding, made into the compact line Go's writer writes.
    fn compact(text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        let (mut in_string, mut escaped) = (false, false);
        for c in text.chars() {
            if in_string {
                out.push(c);
                match (escaped, c) {
                    (true, _) => escaped = false,
                    (false, '\\') => escaped = true,
                    (false, '"') => in_string = false,
                    _ => {}
                }
            } else if c == '"' {
                in_string = true;
                out.push(c);
            } else if !c.is_whitespace() {
                out.push(c);
            }
        }
        out
    }

    const COMMANDS: [&str; 6] = [
        "command-start.json",
        "command-start-env.json",
        "command-set-mute.json",
        "command-set-deafen.json",
        "command-get-state.json",
        "command-leave.json",
    ];

    const EVENTS: [&str; 20] = [
        "event-hello.json",
        "event-ack.json",
        "event-ack-refused.json",
        "event-protocol-error.json",
        "event-state-starting.json",
        "event-state-connected.json",
        "event-state-transmitting.json",
        "event-state-muted-deafened.json",
        "event-state-listen-only.json",
        "event-state-no-microphone.json",
        "event-state-no-talk-key.json",
        "event-state-key-device-lost.json",
        "event-state-reconnecting.json",
        "event-state-reconnecting-backoff.json",
        "event-state-reconnecting-sdk.json",
        "event-state-stopping.json",
        "event-state-stopped.json",
        "event-state-stopped-owner-lost.json",
        "event-state-failed-duplicate-identity.json",
        "event-state-failed-identity-mismatch.json",
    ];

    /// Every golden command is read by the helper's reader, and written again byte for byte
    /// as Go writes it.
    #[test]
    fn every_golden_command_decodes_and_encodes_as_go_writes_it() {
        for name in COMMANDS {
            let line = compact(&fixture_text(name));
            let command = parse_command(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            let written = match &command {
                VoiceControlCommand::Start(c) => to_line(c),
                VoiceControlCommand::SetMute(c) => to_line(c),
                VoiceControlCommand::SetDeafen(c) => to_line(c),
                VoiceControlCommand::GetState(c) => to_line(c),
                VoiceControlCommand::Leave(c) => to_line(c),
            }
            .unwrap();
            assert_eq!(written, line, "{name}");
        }
    }

    /// Every golden event is written by the helper's writer byte for byte as Go writes it,
    /// and read back by the owner's reader as the same event.
    #[test]
    fn every_golden_event_encodes_as_go_writes_it() {
        for name in EVENTS {
            let line = compact(&fixture_text(name));
            let event =
                parse_event(line.as_bytes()).unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            let written = match &event {
                VoiceControlEvent::Hello(e) => to_line(e),
                VoiceControlEvent::Ack(e) => to_line(e),
                VoiceControlEvent::State(e) => to_line(e),
                VoiceControlEvent::ProtocolError(e) => to_line(e),
                VoiceControlEvent::Unknown { .. } => panic!("{name} is of no known kind"),
            }
            .unwrap();
            assert_eq!(written, line, "{name}");
            // And as values, whatever the order of the fields.
            assert_eq!(
                serde_json::from_str::<Value>(&written).unwrap(),
                fixture(name)
            );
        }
    }

    #[test]
    fn every_fixture_in_the_directory_is_read_by_a_test_here_or_by_the_helpers() {
        let mut claimed: BTreeSet<String> = COMMANDS
            .iter()
            .chain(EVENTS.iter())
            .map(|name| (*name).to_owned())
            .collect();
        // cases.json and lines/ below; sensitive.json and the transcripts by the
        // helper's own tests (conch-voice's tests/managed.rs).
        claimed.insert("cases.json".into());
        claimed.insert("lines".into());
        let mut unclaimed = Vec::new();
        for entry in std::fs::read_dir(fixtures()).unwrap() {
            let name = entry.unwrap().file_name().into_string().unwrap();
            let elsewhere = name == "sensitive.json" || name.starts_with("transcript-");
            if !claimed.contains(&name) && !elsewhere {
                unclaimed.push(name);
            }
        }
        assert!(unclaimed.is_empty(), "no test reads {unclaimed:?}");
    }

    #[derive(Deserialize)]
    struct Cases {
        cases: Vec<Case>,
    }

    #[derive(Deserialize)]
    struct Case {
        name: String,
        direction: String,
        line: Option<String>,
        line_hex: Option<String>,
        line_file: Option<String>,
        want: Want,
    }

    #[derive(Deserialize)]
    struct Want {
        result: String,
        kind: Option<String>,
        code: Option<String>,
        id: Option<i64>,
        offers_v1: Option<bool>,
    }

    impl Case {
        fn line(&self) -> Vec<u8> {
            match (&self.line, &self.line_hex, &self.line_file) {
                (Some(line), None, None) => line.as_bytes().to_vec(),
                (None, Some(hex), None) => (0..hex.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
                    .collect(),
                (None, None, Some(file)) if file.is_empty() => Vec::new(),
                (None, None, Some(file)) => std::fs::read(fixtures().join(file)).unwrap(),
                _ => panic!("case {:?} has not exactly one form of its line", self.name),
            }
        }
    }

    /// Every conformance case is read with the same result, code and id as Go reads it:
    /// the commands by the helper's reader, the events by the owner's.
    #[test]
    fn every_conformance_case_is_read_as_go_reads_it() {
        let cases: Cases = serde_json::from_str(&fixture_text("cases.json")).unwrap();
        let mut commands = 0;
        for case in &cases.cases {
            let line = case.line();
            let name = format!("{}: {}", case.direction, case.name);
            let (kind, refusal, offers_v1) = match case.direction.as_str() {
                "command" => {
                    commands += 1;
                    match parse_command(&line) {
                        Ok(command) => (Some(command.kind().as_str().to_owned()), None, None),
                        Err(refusal) => (None, Some(refusal), None),
                    }
                }
                "event" => match parse_event(&line) {
                    Ok(VoiceControlEvent::Unknown { kind, .. }) => {
                        assert_eq!(case.want.result, "skip", "{name}: skipped");
                        (Some(kind), None, None)
                    }
                    Ok(event) => {
                        let offers = match &event {
                            VoiceControlEvent::Hello(hello) => {
                                Some(hello.offers(VOICE_CONTROL_SCHEMA_V1))
                            }
                            _ => None,
                        };
                        (Some(event.kind().to_owned()), None, offers)
                    }
                    Err(refusal) => (None, Some(refusal), None),
                },
                other => panic!("{name}: direction {other}"),
            };
            match case.want.result.as_str() {
                "accept" | "skip" => {
                    assert_eq!(refusal, None, "{name}: refused");
                    assert_eq!(kind.as_deref(), case.want.kind.as_deref(), "{name}");
                    if let Some(want) = case.want.offers_v1 {
                        assert_eq!(offers_v1, Some(want), "{name}: offers v1");
                    }
                }
                "refuse" => {
                    let refusal = refusal.unwrap_or_else(|| panic!("{name}: accepted as {kind:?}"));
                    assert_eq!(
                        Some(refusal.code.as_str()),
                        case.want.code.as_deref(),
                        "{name}"
                    );
                    assert_eq!(refusal.id, case.want.id, "{name}: id");
                }
                other => panic!("{name}: want.result {other}"),
            }
        }
        assert!(commands >= 40, "only {commands} command cases");
    }

    /// The numbers both sides are held to are the Go constants' values.
    #[test]
    fn the_constants_are_the_go_constants() {
        let go = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/voice_control.go"),
        )
        .unwrap();
        let value_of = |name: &str| -> String {
            let line = go
                .lines()
                .map(|line| line.trim().trim_start_matches("const "))
                .find(|line| line.starts_with(&format!("{name} ")) && line.contains(" = "))
                .unwrap_or_else(|| panic!("{name} is not in voice_control.go"));
            let value = line.split(" = ").nth(1).unwrap().trim();
            // Digit separators are Go's way of writing a number, not part of it.
            if value.starts_with('"') {
                value.to_owned()
            } else {
                value.replace('_', "")
            }
        };
        let table: [(&str, String); 21] = [
            (
                "VoiceControlSchemaV1",
                format!("{VOICE_CONTROL_SCHEMA_V1:?}"),
            ),
            (
                "VoiceControlSubcommand",
                format!("{VOICE_CONTROL_SUBCOMMAND:?}"),
            ),
            ("VoiceControlEventFD", VOICE_CONTROL_EVENT_FD.to_string()),
            (
                "VoiceControlMaxLineBytes",
                VOICE_CONTROL_MAX_LINE_BYTES.to_string(),
            ),
            ("VoiceControlMaxID", "1<<53 - 1".into()),
            (
                "VoiceControlMaxInFlight",
                VOICE_CONTROL_MAX_IN_FLIGHT.to_string(),
            ),
            (
                "VoiceControlEventQueueLines",
                VOICE_CONTROL_EVENT_QUEUE_LINES.to_string(),
            ),
            (
                "VoiceControlMaxVersions",
                VOICE_CONTROL_MAX_VERSIONS.to_string(),
            ),
            (
                "VoiceControlMaxHelperVersionBytes",
                VOICE_CONTROL_MAX_HELPER_VERSION_BYTES.to_string(),
            ),
            (
                "VoiceControlMaxServerBytes",
                VOICE_CONTROL_MAX_SERVER_BYTES.to_string(),
            ),
            (
                "VoiceControlMaxChannelBytes",
                "4 * MaxChannelNameLength".into(),
            ),
            (
                "VoiceControlMaxRetryInMS",
                VOICE_CONTROL_MAX_RETRY_IN_MS.to_string(),
            ),
            (
                "VoiceControlHelloTimeoutMS",
                VOICE_CONTROL_HELLO_TIMEOUT_MS.to_string(),
            ),
            (
                "VoiceControlStartTimeoutMS",
                VOICE_CONTROL_START_TIMEOUT_MS.to_string(),
            ),
            (
                "VoiceControlAckTimeoutMS",
                VOICE_CONTROL_ACK_TIMEOUT_MS.to_string(),
            ),
            (
                "VoiceControlOwnerPollMS",
                VOICE_CONTROL_OWNER_POLL_MS.to_string(),
            ),
            (
                "VoiceControlGateShutMS",
                VOICE_CONTROL_GATE_SHUT_MS.to_string(),
            ),
            (
                "VoiceControlCaptureStopMS",
                VOICE_CONTROL_CAPTURE_STOP_MS.to_string(),
            ),
            (
                "VoiceControlOwnerLossExitMS",
                VOICE_CONTROL_OWNER_LOSS_EXIT_MS.to_string(),
            ),
            (
                "VoiceControlLeaveExitMS",
                VOICE_CONTROL_LEAVE_EXIT_MS.to_string(),
            ),
            (
                "VoiceControlStopKillAfterMS",
                VOICE_CONTROL_STOP_KILL_AFTER_MS.to_string(),
            ),
        ];
        for (name, rust) in table {
            assert_eq!(value_of(name), rust, "{name}");
        }
        assert_eq!(
            value_of("VoiceControlStopTermAfterMS"),
            VOICE_CONTROL_STOP_TERM_AFTER_MS.to_string()
        );
        assert_eq!(VOICE_CONTROL_MAX_ID, 9_007_199_254_740_991);
        // MaxChannelNameLength, in channel.go.
        assert_eq!(VOICE_CONTROL_MAX_CHANNEL_BYTES, 4 * 100);
    }

    /// The vocabularies are the Go constants' words, in the Go order.
    #[test]
    fn the_vocabularies_are_the_schemas() {
        let go = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/voice_control.go"),
        )
        .unwrap();
        // Each Go constant of a type, in the order it is declared: `Name Type = "word"`
        // or, in an aligned block, `Name    Type = "word"`.
        let words_of = |go_type: &str| -> Vec<String> {
            go.lines()
                .filter_map(|line| {
                    let mut parts = line.split_whitespace();
                    let _name = parts.next()?;
                    (parts.next()? == go_type && parts.next()? == "=").then_some(())?;
                    let word = parts.next()?;
                    Some(word.trim_matches('"').to_owned())
                })
                .collect()
        };
        fn ours<T: Copy>(all: &[T], word: fn(T) -> &'static str) -> Vec<String> {
            all.iter().map(|value| word(*value).to_owned()).collect()
        }
        let table = [
            (
                "VoiceControlCommandKind",
                ours(
                    VoiceControlCommandKind::ALL,
                    VoiceControlCommandKind::as_str,
                ),
            ),
            (
                "VoiceControlEventKind",
                ours(VoiceControlEventKind::ALL, VoiceControlEventKind::as_str),
            ),
            (
                "VoiceControlCredential",
                ours(VoiceControlCredential::ALL, VoiceControlCredential::as_str),
            ),
            (
                "VoiceControlPhase",
                ours(VoiceControlPhase::ALL, VoiceControlPhase::as_str),
            ),
            (
                "VoiceControlReason",
                ours(VoiceControlReason::ALL, VoiceControlReason::as_str),
            ),
            (
                "VoiceControlCapture",
                ours(VoiceControlCapture::ALL, VoiceControlCapture::as_str),
            ),
            (
                "VoiceControlBlock",
                ours(VoiceControlBlock::ALL, VoiceControlBlock::as_str),
            ),
            (
                "VoiceControlPTT",
                ours(VoiceControlPtt::ALL, VoiceControlPtt::as_str),
            ),
            (
                "VoiceControlPTTProblem",
                ours(VoiceControlPttProblem::ALL, VoiceControlPttProblem::as_str),
            ),
            (
                "VoiceControlErrorCode",
                ours(VoiceControlErrorCode::ALL, VoiceControlErrorCode::as_str),
            ),
        ];
        for (go_type, ours) in table {
            assert_eq!(words_of(go_type), ours, "{go_type}");
        }
    }

    #[test]
    fn the_classes_of_reasons_and_codes_are_the_schemas() {
        use VoiceControlErrorCode as Code;
        let fatal: Vec<_> = Code::ALL.iter().filter(|c| c.is_fatal()).collect();
        assert_eq!(
            fatal,
            [
                &Code::TooLong,
                &Code::Malformed,
                &Code::UnknownSchema,
                &Code::Invalid,
                &Code::IdNotIncreasing,
                &Code::NotStarted
            ]
        );
        let acks: Vec<_> = Code::ALL.iter().filter(|c| c.is_ack_refusal()).collect();
        assert_eq!(
            acks,
            [
                &Code::UnknownKind,
                &Code::Invalid,
                &Code::AlreadyStarted,
                &Code::Stopping
            ]
        );
        // Every reason belongs to at least one phase, and server_unreachable to two.
        for reason in VoiceControlReason::ALL {
            assert!(
                reason.is_retry() || reason.is_stop() || reason.is_failure(),
                "{reason}"
            );
        }
        assert!(VoiceControlReason::ServerUnreachable.is_retry());
        assert!(VoiceControlReason::ServerUnreachable.is_failure());
    }

    fn state() -> VoiceControlStateV1 {
        serde_json::from_value(fixture("event-state-connected.json")).unwrap()
    }

    /// The writer refuses what the reader refuses: a state that does not hold together is
    /// never written.
    #[test]
    fn a_state_that_does_not_hold_together_is_not_written() {
        use VoiceControlBlock as Block;
        let cases: Vec<(&str, VoiceControlStateV1)> = vec![
            (
                "transmitting while muted",
                VoiceControlStateV1 {
                    transmitting: true,
                    muted: true,
                    blocked: vec![Block::Muted],
                    ..state()
                },
            ),
            (
                "connecting without not_connected",
                VoiceControlStateV1 {
                    phase: VoiceControlPhase::Connecting,
                    ..state()
                },
            ),
            (
                "capture on while starting",
                VoiceControlStateV1 {
                    phase: VoiceControlPhase::Starting,
                    blocked: vec![Block::NotConnected],
                    ..state()
                },
            ),
            (
                "muted without its block",
                VoiceControlStateV1 {
                    muted: true,
                    ..state()
                },
            ),
            (
                "blocks out of order",
                VoiceControlStateV1 {
                    muted: true,
                    deafened: true,
                    blocked: vec![Block::Muted, Block::Deafened],
                    ..state()
                },
            ),
            (
                "ptt unknown while connected",
                VoiceControlStateV1 {
                    ptt: VoiceControlPtt::Unknown,
                    ..state()
                },
            ),
            (
                "ptt lost with no problem",
                VoiceControlStateV1 {
                    ptt: VoiceControlPtt::Lost,
                    blocked: vec![Block::KeyDeviceLost],
                    ..state()
                },
            ),
            (
                "a reason while connected",
                VoiceControlStateV1 {
                    reason: Some(VoiceControlReason::Left),
                    ..state()
                },
            ),
            (
                "stopped for a failure's reason",
                VoiceControlStateV1 {
                    phase: VoiceControlPhase::Stopped,
                    reason: Some(VoiceControlReason::DuplicateIdentity),
                    capture: VoiceControlCapture::Off,
                    blocked: vec![Block::NotConnected],
                    ..state()
                },
            ),
            (
                "retry_in_ms without a reason",
                VoiceControlStateV1 {
                    phase: VoiceControlPhase::Reconnecting,
                    retry_in_ms: Some(0),
                    blocked: vec![Block::NotConnected],
                    ..state()
                },
            ),
            (
                "capture unavailable with a microphone",
                VoiceControlStateV1 {
                    capture: VoiceControlCapture::Unavailable,
                    ..state()
                },
            ),
        ];
        for (name, state) in cases {
            assert!(to_line(&state).is_err(), "{name} was written");
        }
        assert!(to_line(&state()).is_ok());
    }

    #[test]
    fn an_ack_says_why_exactly_when_it_refuses() {
        let ok = VoiceControlAckV1::new(3, 1, None);
        assert_eq!(
            to_line(&ok).unwrap(),
            r#"{"schema":"conch.voice_control.v1","kind":"ack","seq":3,"id":1,"ok":true}"#
        );
        let refused = VoiceControlAckV1::new(9, 6, Some(VoiceControlErrorCode::UnknownKind));
        assert_eq!(
            to_line(&refused).unwrap(),
            compact(&fixture_text("event-ack-refused.json"))
        );
        // A refusal with a code that ends a session is a protocol_error, not an ack.
        assert!(
            to_line(&VoiceControlAckV1::new(
                9,
                6,
                Some(VoiceControlErrorCode::TooLong)
            ))
            .is_err()
        );
        assert!(
            to_line(&VoiceControlProtocolErrorV1::new(
                2,
                VoiceControlErrorCode::Stopping
            ))
            .is_err()
        );
        assert!(to_line(&VoiceControlAckV1::new(0, 6, None)).is_err());
    }

    #[test]
    fn the_hello_is_frozen_and_its_version_printable() {
        let hello = VoiceControlHelloV1::v1("0.1.0 (48000 Hz, 10 ms frames)");
        assert_eq!(
            to_line(&hello).unwrap(),
            compact(&fixture_text("event-hello.json"))
        );
        assert!(hello.offers(VOICE_CONTROL_SCHEMA_V1));
        for bad in ["", "\u{1b}[2J", &"x".repeat(65), "é"] {
            assert!(to_line(&VoiceControlHelloV1::v1(bad)).is_err(), "{bad:?}");
        }
        for (version, ok) in [
            ("conch.voice_control.v1", true),
            ("conch.voice_control.v9999", true),
            ("conch.voice_control.v10000", false),
            ("conch.voice_control.v0", false),
            ("conch.voice_control.v01", false),
            ("conch.voice_control.V1", false),
            ("conch.voice_control.v", false),
        ] {
            assert_eq!(is_version_name(version), ok, "{version}");
        }
    }

    #[test]
    fn a_line_longer_than_the_limit_is_never_written() {
        // A start whose channel and server are at their limits, every byte escaped, still
        // fits; nothing an owner may write is too long to write.
        let start = VoiceControlStartV1 {
            schema: VOICE_CONTROL_SCHEMA_V1.into(),
            kind: VoiceControlCommandKind::Start,
            id: VOICE_CONTROL_MAX_ID,
            server: format!("https://{}", "a".repeat(VOICE_CONTROL_MAX_SERVER_BYTES - 8)),
            channel: "\"".repeat(VOICE_CONTROL_MAX_CHANNEL_BYTES),
            principal_id: VOICE_CONTROL_MAX_ID,
            credential: VoiceControlCredential::Stored,
            muted: false,
            deafened: false,
        };
        let line = to_line(&start).unwrap();
        assert!(line.len() <= VOICE_CONTROL_MAX_LINE_BYTES);
        assert_eq!(
            parse_command(line.as_bytes()),
            Ok(VoiceControlCommand::Start(start))
        );
    }

    /// The server rule, case by case, as Go's `url.Parse` reads each address.
    #[test]
    fn a_server_is_checked_as_go_parses_it() {
        let table = [
            ("https://conch.example.org", true),
            ("http://127.0.0.1:8080", true),
            ("https://example.org/conch", true),
            ("http://[::1]:8080", true),
            ("https://conch.example.org:", true),
            // Go lowers the scheme before it is compared.
            ("HTTPS://conch.example.org", true),
            // An empty fragment is no fragment.
            ("https://conch.example.org#", true),
            ("https://conch.example.org/a%20b", true),
            ("https://Conch.example.org", false),
            ("https://conch.example.org/", false),
            ("https://conch.example.org/a%2F", false),
            ("https://someone@conch.example.org", false),
            ("https://conch.example.org?x=1", false),
            ("https://conch.example.org?", false),
            ("https://conch.example.org#top", false),
            ("ws://conch.example.org", false),
            ("conch.example.org", false),
            ("https:conch.example.org", false),
            ("https:/conch.example.org", false),
            ("https://", false),
            ("https://conch.example.org:http", false),
            ("https://conch example.org", false),
            ("https://conch.example.org/%zz", false),
            ("https://%41.example.org", false),
            ("https://[::1", false),
            ("://conch.example.org", false),
            ("https://conch.example.org\u{7f}", false),
            ("", false),
        ];
        for (server, ok) in table {
            assert_eq!(check_server(server).is_ok(), ok, "{server:?}");
        }
        assert!(check_server(&format!("https://{}", "a".repeat(1024))).is_err());
    }

    #[test]
    fn a_channel_is_any_name_but_a_blank_or_one_with_a_control_character() {
        for (channel, ok) in [
            ("general", true),
            ("café ☕", true),
            ("  spaced  ", true),
            ("", false),
            (" \t", false),
            ("\u{a0}", false),
            ("a\u{1b}[2J", false),
            ("a\u{85}b", false),
        ] {
            assert_eq!(check_channel(channel).is_ok(), ok, "{channel:?}");
        }
        assert!(check_channel(&"é".repeat(200)).is_ok());
        assert!(check_channel(&"é".repeat(201)).is_err());
    }

    #[test]
    fn a_refusal_says_its_code_and_never_the_line() {
        let line = br#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":7,"muted":"conch_FAKE_login_token_do_not_print"}"#;
        let refusal = parse_command(line).unwrap_err();
        assert_eq!(
            refusal,
            VoiceControlRefusal {
                code: VoiceControlErrorCode::Invalid,
                id: Some(7)
            }
        );
        let shown = format!("{refusal} {refusal:?}");
        assert!(!shown.contains("FAKE"), "{shown}");
        assert_eq!(
            refusal.to_string(),
            "voice control line refused: invalid (id 7)"
        );
    }

    #[test]
    fn null_is_no_value_for_any_field() {
        // An optional field of an event, given as null, is refused as Go refuses it.
        let mut ack = fixture("event-ack.json");
        ack["error"] = Value::Null;
        assert_eq!(
            parse_event(ack.to_string().as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::Invalid
        );
        let mut state = fixture("event-state-reconnecting.json");
        state["ptt_problem"] = Value::Null;
        assert!(parse_event(state.to_string().as_bytes()).is_err());
        // And the schema, the id and the kind of a command.
        for (field, code, id) in [
            ("schema", VoiceControlErrorCode::UnknownSchema, None),
            ("id", VoiceControlErrorCode::Invalid, None),
            ("kind", VoiceControlErrorCode::UnknownKind, Some(4)),
        ] {
            let mut command = fixture("command-get-state.json");
            command[field] = Value::Null;
            let refusal = parse_command(command.to_string().as_bytes()).unwrap_err();
            assert_eq!((refusal.code, refusal.id), (code, id), "{field}");
        }
        let _ = json!(null);
    }
}
