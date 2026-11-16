//! Version 3 of the control protocol of a managed `conch-voice` (`conch.voice_control.v3`,
//! `docs/design/linux-share-audio.md` §7): Rust copies of the shapes in
//! `pkg/schema/voice_control_v3.go`, read and written by the same rules as v1's and v2's
//! (`control.rs`, `control_v2.rs`).
//!
//! A v3 session is a v2 session plus the sound of a screen share, which on Linux the
//! helper captures and publishes on its owner's command: [`VoiceControlSetShareAudioV3`]
//! asks for it or stops it, [`VoiceControlSetShareAudioMuteV3`] mutes the published track,
//! and the state says what came of it ([`VoiceControlShareAudio`],
//! [`VoiceControlShareAudioProblem`], and a count of the applications being mixed).
//!
//! What v1 and v2 leave out, v3 leaves out too: no secret, no free text from the helper
//! (the application name the owner sends is never repeated, and the state names no
//! application), and no command that opens the gate: the share's sound is not the
//! microphone, and `transmitting` does not depend on any of it.
//!
//! A share-audio command the helper cannot carry out is refused into the state, not by
//! the reader: a scope this platform cannot capture is a valid command, and its answer is
//! `unavailable` with `unsupported_scope`. Only a line that breaks the shape is `invalid`.
//!
//! The hello is v1's ([`crate::VoiceControlHelloV1::v1_v2_and_v3`]): the owner picks the
//! version by the schema of its start, and from then on every line of either side is of
//! that version. [`parse_command_v3`] and [`parse_event_v3`] apply Go's rules in Go's order
//! (`ParseVoiceControlCommandV3`, `ParseVoiceControlEventV3`): v1's framing, a line of v1
//! or v2 is `unknown_schema`, and a hello in v3 is `invalid`. An optional word that is
//! present and empty is `invalid`: a stop says "no scope" by leaving the scope out.
//!
//! v3's command kinds are a vocabulary of their own ([`VoiceControlCommandKindV3`]), as
//! in Go: [`crate::VoiceControlCommandKind`] is the kind of every published v1 and v2
//! command and does not change. The words on the wire are the same.

use std::fmt;

use serde::Serialize;

use crate::control::{
    Loose, VoiceControlBlock, VoiceControlCapture, VoiceControlCredential, VoiceControlErrorCode,
    VoiceControlEventKind, VoiceControlMessage, VoiceControlPhase, VoiceControlPtt,
    VoiceControlPttProblem, VoiceControlReason, VoiceControlRefusal, check_ack_answer,
    check_event_head_in, check_id, check_start_fields, frame, id_in_range, message, refused, typed,
    vocabulary,
};
use crate::control_v2::{
    VOICE_CONTROL_SCHEMA_V2, VoiceControlPlayback, VoiceControlStateV2, VoiceControlTalkMode,
};
use crate::error::Error;
use crate::types::wire_object;
use crate::voice_devices::is_writable_device_name;

/// The name every v3 message but the hello carries in its `schema` field. The hello is
/// always v1.
pub const VOICE_CONTROL_SCHEMA_V3: &str = "conch.voice_control.v3";

/// The longest `application_name` a `set_share_audio` may carry, in bytes.
pub const VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES: usize = 128;

/// The largest `share_audio_sources` a state may carry.
pub const VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES: i64 = 1024;

vocabulary!(
    /// What a v3 command asks for: v2's seven kinds, with v2's meaning, and the two of the
    /// share's sound. A vocabulary of its own, listed in full, so that the published v1
    /// and v2 commands accept no kind they did not accept before.
    VoiceControlCommandKindV3, "not a command kind of this version", {
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
    /// Use this microphone and these speakers from now on.
    SetDevices = "set_devices",
    /// Choose what may open the gate: a press, or the microphone's own sound.
    SetTalkMode = "set_talk_mode",
    /// Capture and publish the sound of the owner's screen share, or stop.
    SetShareAudio = "set_share_audio",
    /// Mute the published share-audio track, or unmute it.
    SetShareAudioMute = "set_share_audio_mute",
});

vocabulary!(
    /// Which sound a sharer consented to as their share's audio: Go's
    /// `VoiceShareAudioScope` (`pkg/schema/voice_companion.go`), the vocabulary of the
    /// consent recorded with `conchd`, which a `set_share_audio` repeats.
    VoiceShareAudioScope, "not a share-audio scope", {
    /// The sound of the shared application or window alone.
    Application = "application",
    /// Everything the system plays, except the call itself.
    SystemExcludingCall = "system_excluding_call",
});

vocabulary!(
    /// Whether the helper is publishing the sound of its owner's screen share.
    VoiceControlShareAudio, "not a share-audio state", {
    /// Not asked for, or stopped. Always so before the session has joined and after it
    /// has ended.
    Off = "off",
    /// The captures are open and the track is published. Muted is not off: the track
    /// stays published and is muted.
    On = "on",
    /// Asked for and not achievable now; the problem says why. Nothing is captured in its
    /// place.
    Unavailable = "unavailable",
});

vocabulary!(
    /// Why the share's sound is unavailable, or, for `no_sources` alone, a note beside a
    /// share-audio state of on.
    VoiceControlShareAudioProblem, "not a share-audio problem", {
    /// The sound system is not reachable, or a capture could not be opened.
    NoPipewire = "no_pipewire",
    /// The helper could not work out which streams are the call's own, so it captures
    /// nothing rather than everything.
    NoProcessTree = "no_process_tree",
    /// The media server refused the track. The owner may send the command again.
    PublishRefused = "publish_refused",
    /// The scope asked for is one this platform cannot capture.
    UnsupportedScope = "unsupported_scope",
    /// This session cannot carry a share's sound at all: it has no companion.
    Unsupported = "unsupported",
    /// Asked for while the helper is not connected.
    NotConnected = "not_connected",
    /// Nothing that may be captured is playing right now. Not a failure: the share's
    /// sound is on, the track is published and silent, and no source is counted.
    NoSources = "no_sources",
});

/// The first command of a v3 session (owner to helper). Its fields are v1's and v2's:
/// nothing about a share is carried.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStartV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::Start`].
    pub kind: VoiceControlCommandKindV3,
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

wire_object!(VoiceControlStartV3, "a voice control start", unknown = ignored, {
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

/// Sets whether the user is muted (owner to helper), as in v1. The microphone's mute and
/// nothing else: the share's sound has its own ([`VoiceControlSetShareAudioMuteV3`]).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetMuteV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetMute`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub muted: bool,
}

wire_object!(VoiceControlSetMuteV3, "a voice control set_mute", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    muted: required,
});

/// Sets whether the user is deafened (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetDeafenV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetDeafen`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub deafened: bool,
}

wire_object!(VoiceControlSetDeafenV3, "a voice control set_deafen", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    deafened: required,
});

/// Asks for a state snapshot now (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlGetStateV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::GetState`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlGetStateV3, "a voice control get_state", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// Leaves voice (owner to helper), as in v1. Leaving ends a share's sound with the rest
/// of the session.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlLeaveV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::Leave`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlLeaveV3, "a voice control leave", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// Names the microphone, the speakers, or both, to use from now on (owner to helper), as
/// in v2 and by its rules. A share's sound has no device.
///
/// `Debug` shows which sides are named and never a name, as v2's does.
#[derive(Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetDevicesV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetDevices`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The microphone, under the device list's rule ([`is_writable_device_name`]). `None`
    /// to leave the microphone alone.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub input: Option<String>,
    /// The speakers, under the same rule.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub output: Option<String>,
}

wire_object!(VoiceControlSetDevicesV3, "a voice control set_devices", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    input: maybe,
    output: maybe,
});

impl fmt::Debug for VoiceControlSetDevicesV3 {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let side = |name: &Option<String>| name.as_ref().map(|_| "..");
        f.debug_struct("VoiceControlSetDevicesV3")
            .field("schema", &self.schema)
            .field("kind", &self.kind)
            .field("id", &self.id)
            .field("input", &side(&self.input))
            .field("output", &side(&self.output))
            .finish()
    }
}

/// Chooses what may open the gate (owner to helper), as in v2.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetTalkModeV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetTalkMode`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The mode wanted.
    pub talk_mode: VoiceControlTalkMode,
}

wire_object!(VoiceControlSetTalkModeV3, "a voice control set_talk_mode", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    talk_mode: required,
});

/// Asks the helper to capture and publish the sound of the owner's screen share, or to
/// stop (owner to helper). A desired state, like `set_mute`, and idempotent. The owner
/// sends it only after the sharer's consent to that sound is recorded with `conchd`;
/// nothing here records or widens a consent, and nothing here opens the transmit gate.
///
/// What the helper cannot do it refuses into the state, with an ack that is ok.
///
/// `Debug` shows whether an application name is there and never the name: the helper
/// does not repeat it in anything it writes.
#[derive(Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetShareAudioV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetShareAudio`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The state wanted: true to capture and publish, false to stop.
    pub capturing: bool,
    /// Which sound: required with `capturing`, and `None` without it, so that a scope
    /// left over from an earlier command is never read as a consent.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub scope: Option<VoiceShareAudioScope>,
    /// The name the owner's own application plays sound under, which the helper leaves
    /// out of the capture: 1 to [`VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES`] bytes of
    /// printable ASCII. Required with `capturing`, `None` without it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub application_name: Option<String>,
}

wire_object!(VoiceControlSetShareAudioV3, "a voice control set_share_audio", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    capturing: required,
    scope: maybe,
    application_name: maybe,
});

impl fmt::Debug for VoiceControlSetShareAudioV3 {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("VoiceControlSetShareAudioV3")
            .field("schema", &self.schema)
            .field("kind", &self.kind)
            .field("id", &self.id)
            .field("capturing", &self.capturing)
            .field("scope", &self.scope)
            .field(
                "application_name",
                &self.application_name.as_ref().map(|_| ".."),
            )
            .finish()
    }
}

/// Mutes the published share-audio track, or unmutes it (owner to helper). A desired
/// state, like `set_mute`, kept whether or not a share's sound is being published. The
/// track stays published and the captures stay open. Independent of `set_mute` (the
/// microphone) and of `set_deafen`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetShareAudioMuteV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlCommandKindV3::SetShareAudioMute`].
    pub kind: VoiceControlCommandKindV3,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub muted: bool,
}

wire_object!(VoiceControlSetShareAudioMuteV3, "a voice control set_share_audio_mute", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    muted: required,
});

/// The answer to one command (helper to owner), as in v1. A share-audio command the
/// helper cannot carry out is not refused here: its ack is ok and the state says why not.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlAckV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
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

wire_object!(VoiceControlAckV3, "a voice control ack", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    id: required,
    ok: required,
    error: maybe,
});

impl VoiceControlAckV3 {
    /// The answer to command `id`: carried out if `refusal` is `None`.
    #[must_use]
    pub fn new(seq: i64, id: i64, refusal: Option<VoiceControlErrorCode>) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V3.to_owned(),
            kind: VoiceControlEventKind::Ack,
            seq,
            id,
            ok: refusal.is_none(),
            error: refusal,
        }
    }
}

/// A whole-state snapshot (helper to owner): every field of [`VoiceControlStateV2`],
/// unchanged and by the same rules, and four about the sound of the owner's screen share.
/// It names no device, no application and no stream.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStateV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
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
    /// The microphone stream. It says nothing of a share's sound.
    pub capture: VoiceControlCapture,
    /// The speakers' stream.
    pub playback: VoiceControlPlayback,
    /// Whether the microphone's audio is leaving now, release tail included. A share's
    /// sound is not counted.
    pub transmitting: bool,
    /// Whether the user is muted.
    pub muted: bool,
    /// Whether the user is deafened.
    pub deafened: bool,
    /// Every reason a press, or in open mode an onset, would do nothing now, most
    /// important first. Nothing about a share's sound is ever among them.
    pub blocked: Vec<VoiceControlBlock>,
    /// The talk mode in force.
    pub talk_mode: VoiceControlTalkMode,
    /// Whether hold-to-talk can work.
    pub ptt: VoiceControlPtt,
    /// Why the key device is not being read: present exactly when `ptt` is unavailable or
    /// lost.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ptt_problem: Option<VoiceControlPttProblem>,
    /// Whether the sound of the owner's screen share is being captured and published.
    pub share_audio: VoiceControlShareAudio,
    /// The mute `set_share_audio_mute` asked for: a desired state, true in any phase and
    /// whatever `share_audio` is.
    pub share_audio_muted: bool,
    /// Why `share_audio` is unavailable, or `no_sources` beside on when nothing that may
    /// be captured is playing. `None` otherwise.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub share_audio_problem: Option<VoiceControlShareAudioProblem>,
    /// How many applications' streams are being mixed into the share's sound now: 0 to
    /// [`VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES`]. A count and nothing else.
    pub share_audio_sources: i64,
}

wire_object!(VoiceControlStateV3, "a voice control state", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    phase: required,
    reason: maybe,
    retry_in_ms: maybe,
    capture: required,
    playback: required,
    transmitting: required,
    muted: required,
    deafened: required,
    blocked: required,
    talk_mode: required,
    ptt: required,
    ptt_problem: maybe,
    share_audio: required,
    share_audio_muted: required,
    share_audio_problem: maybe,
    share_audio_sources: required,
});

impl VoiceControlStateV3 {
    /// Whether `block` is among the reasons a press, or an onset, would do nothing.
    #[must_use]
    pub fn blocks(&self, block: VoiceControlBlock) -> bool {
        self.blocked.contains(&block)
    }

    /// The snapshot's v2 fields as a v2 state, for the rules the two versions share.
    fn v2(&self) -> VoiceControlStateV2 {
        VoiceControlStateV2 {
            schema: VOICE_CONTROL_SCHEMA_V2.to_owned(),
            kind: self.kind,
            seq: self.seq,
            phase: self.phase,
            reason: self.reason,
            retry_in_ms: self.retry_in_ms,
            capture: self.capture,
            playback: self.playback,
            transmitting: self.transmitting,
            muted: self.muted,
            deafened: self.deafened,
            blocked: self.blocked.clone(),
            talk_mode: self.talk_mode,
            ptt: self.ptt,
            ptt_problem: self.ptt_problem,
        }
    }
}

/// A line the helper cannot act on (helper to owner), as in v1; a failed state follows.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlProtocolErrorV3 {
    /// Always [`VOICE_CONTROL_SCHEMA_V3`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::ProtocolError`].
    pub kind: VoiceControlEventKind,
    /// The event's seq.
    pub seq: i64,
    /// What was wrong: a code that ends a session.
    pub code: VoiceControlErrorCode,
}

wire_object!(VoiceControlProtocolErrorV3, "a voice control protocol_error", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    code: required,
});

impl VoiceControlProtocolErrorV3 {
    /// The protocol_error for `code`.
    #[must_use]
    pub fn new(seq: i64, code: VoiceControlErrorCode) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V3.to_owned(),
            kind: VoiceControlEventKind::ProtocolError,
            seq,
            code,
        }
    }
}

/// One v3 command, as [`parse_command_v3`] read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlCommandV3 {
    /// `start`.
    Start(VoiceControlStartV3),
    /// `set_mute`.
    SetMute(VoiceControlSetMuteV3),
    /// `set_deafen`.
    SetDeafen(VoiceControlSetDeafenV3),
    /// `get_state`.
    GetState(VoiceControlGetStateV3),
    /// `leave`.
    Leave(VoiceControlLeaveV3),
    /// `set_devices`.
    SetDevices(VoiceControlSetDevicesV3),
    /// `set_talk_mode`.
    SetTalkMode(VoiceControlSetTalkModeV3),
    /// `set_share_audio`.
    SetShareAudio(VoiceControlSetShareAudioV3),
    /// `set_share_audio_mute`.
    SetShareAudioMute(VoiceControlSetShareAudioMuteV3),
}

impl VoiceControlCommandV3 {
    /// The command's kind.
    #[must_use]
    pub fn kind(&self) -> VoiceControlCommandKindV3 {
        match self {
            Self::Start(_) => VoiceControlCommandKindV3::Start,
            Self::SetMute(_) => VoiceControlCommandKindV3::SetMute,
            Self::SetDeafen(_) => VoiceControlCommandKindV3::SetDeafen,
            Self::GetState(_) => VoiceControlCommandKindV3::GetState,
            Self::Leave(_) => VoiceControlCommandKindV3::Leave,
            Self::SetDevices(_) => VoiceControlCommandKindV3::SetDevices,
            Self::SetTalkMode(_) => VoiceControlCommandKindV3::SetTalkMode,
            Self::SetShareAudio(_) => VoiceControlCommandKindV3::SetShareAudio,
            Self::SetShareAudioMute(_) => VoiceControlCommandKindV3::SetShareAudioMute,
        }
    }

    /// The command's id.
    #[must_use]
    pub fn id(&self) -> i64 {
        match self {
            Self::Start(c) => c.id,
            Self::SetMute(c) => c.id,
            Self::SetDeafen(c) => c.id,
            Self::GetState(c) => c.id,
            Self::Leave(c) => c.id,
            Self::SetDevices(c) => c.id,
            Self::SetTalkMode(c) => c.id,
            Self::SetShareAudio(c) => c.id,
            Self::SetShareAudioMute(c) => c.id,
        }
    }

    fn check(&self) -> Result<(), String> {
        match self {
            Self::Start(c) => c.check(),
            Self::SetMute(c) => c.check(),
            Self::SetDeafen(c) => c.check(),
            Self::GetState(c) => c.check(),
            Self::Leave(c) => c.check(),
            Self::SetDevices(c) => c.check(),
            Self::SetTalkMode(c) => c.check(),
            Self::SetShareAudio(c) => c.check(),
            Self::SetShareAudioMute(c) => c.check(),
        }
    }
}

/// One v3 event, as [`parse_event_v3`] read it. There is no hello: the hello is v1, read
/// with [`crate::parse_event`] before the version is chosen.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlEventV3 {
    /// `ack`.
    Ack(VoiceControlAckV3),
    /// `state`.
    State(VoiceControlStateV3),
    /// `protocol_error`.
    ProtocolError(VoiceControlProtocolErrorV3),
    /// An event of a kind this version does not know, which a reader skips.
    Unknown {
        /// Its seq.
        seq: i64,
        /// Its kind, as written.
        kind: String,
    },
}

impl VoiceControlEventV3 {
    /// The event's seq.
    #[must_use]
    pub fn seq(&self) -> i64 {
        match self {
            Self::Ack(e) => e.seq,
            Self::State(e) => e.seq,
            Self::ProtocolError(e) => e.seq,
            Self::Unknown { seq, .. } => *seq,
        }
    }

    /// The event's kind, as written.
    #[must_use]
    pub fn kind(&self) -> &str {
        match self {
            Self::Ack(_) => VoiceControlEventKind::Ack.as_str(),
            Self::State(_) => VoiceControlEventKind::State.as_str(),
            Self::ProtocolError(_) => VoiceControlEventKind::ProtocolError.as_str(),
            Self::Unknown { kind, .. } => kind,
        }
    }
}

message!(
    VoiceControlStartV3,
    VoiceControlSetMuteV3,
    VoiceControlSetDeafenV3,
    VoiceControlGetStateV3,
    VoiceControlLeaveV3,
    VoiceControlSetDevicesV3,
    VoiceControlSetTalkModeV3,
    VoiceControlSetShareAudioV3,
    VoiceControlSetShareAudioMuteV3,
    VoiceControlAckV3,
    VoiceControlStateV3,
    VoiceControlProtocolErrorV3,
);

/// Reads one v3 command line, without its line feed: the helper's reader, once the start
/// was written in v3. The framing rules and the order of refusals are v1's
/// ([`crate::parse_command`]); a v1 or a v2 line is `unknown_schema`.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not a v3 command the helper can carry
/// out.
pub fn parse_command_v3(line: &[u8]) -> Result<VoiceControlCommandV3, VoiceControlRefusal> {
    let (text, head) = frame(line)?;
    head.check_schema_is(VOICE_CONTROL_SCHEMA_V3)?;
    let id = match head.id {
        Some(Loose::Whole(id)) if id_in_range(id) => id,
        _ => return Err(refused(VoiceControlErrorCode::Invalid, None)),
    };
    let some = Some(id);
    let kind = head
        .kind_word()
        .and_then(VoiceControlCommandKindV3::from_word)
        .ok_or(refused(VoiceControlErrorCode::UnknownKind, some))?;
    let command = match kind {
        VoiceControlCommandKindV3::Start => VoiceControlCommandV3::Start(typed(text, some)?),
        VoiceControlCommandKindV3::SetMute => VoiceControlCommandV3::SetMute(typed(text, some)?),
        VoiceControlCommandKindV3::SetDeafen => {
            VoiceControlCommandV3::SetDeafen(typed(text, some)?)
        }
        VoiceControlCommandKindV3::GetState => VoiceControlCommandV3::GetState(typed(text, some)?),
        VoiceControlCommandKindV3::Leave => VoiceControlCommandV3::Leave(typed(text, some)?),
        VoiceControlCommandKindV3::SetDevices => {
            VoiceControlCommandV3::SetDevices(typed(text, some)?)
        }
        VoiceControlCommandKindV3::SetTalkMode => {
            VoiceControlCommandV3::SetTalkMode(typed(text, some)?)
        }
        VoiceControlCommandKindV3::SetShareAudio => {
            VoiceControlCommandV3::SetShareAudio(typed(text, some)?)
        }
        VoiceControlCommandKindV3::SetShareAudioMute => {
            VoiceControlCommandV3::SetShareAudioMute(typed(text, some)?)
        }
    };
    command
        .check()
        .map_err(|_| refused(VoiceControlErrorCode::Invalid, some))?;
    Ok(command)
}

/// Reads one v3 event line, without its line feed: the owner's reader, once it wrote its
/// start in v3. An event of a kind this version does not know comes back as
/// [`VoiceControlEventV3::Unknown`], to be skipped. A hello in v3 is `invalid`: the hello is
/// written in v1 only, and a reader of v3 has already read it.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not a v3 event.
pub fn parse_event_v3(line: &[u8]) -> Result<VoiceControlEventV3, VoiceControlRefusal> {
    let invalid = refused(VoiceControlErrorCode::Invalid, None);
    let (text, head) = frame(line)?;
    head.check_schema_is(VOICE_CONTROL_SCHEMA_V3)?;
    let seq = match head.seq {
        Some(Loose::Whole(seq)) if id_in_range(seq) => seq,
        _ => return Err(invalid),
    };
    let Some(word) = head.event_kind() else {
        return Err(invalid);
    };
    let Some(kind) = VoiceControlEventKind::from_word(&word) else {
        return Ok(VoiceControlEventV3::Unknown { seq, kind: word });
    };
    let (event, checked) = match kind {
        VoiceControlEventKind::Hello => return Err(invalid),
        VoiceControlEventKind::Ack => {
            let ack: VoiceControlAckV3 = typed(text, None)?;
            let checked = ack.check();
            (VoiceControlEventV3::Ack(ack), checked)
        }
        VoiceControlEventKind::State => {
            let state: VoiceControlStateV3 = typed(text, None)?;
            let checked = state.check();
            (VoiceControlEventV3::State(state), checked)
        }
        VoiceControlEventKind::ProtocolError => {
            let error: VoiceControlProtocolErrorV3 = typed(text, None)?;
            let checked = error.check();
            (VoiceControlEventV3::ProtocolError(error), checked)
        }
    };
    checked.map_err(|_| invalid)?;
    Ok(event)
}

/// The head of a v3 command: its schema, its kind and its id.
fn command_head(
    schema: &str,
    kind: VoiceControlCommandKindV3,
    want: VoiceControlCommandKindV3,
    id: i64,
) -> Result<(), String> {
    if schema != VOICE_CONTROL_SCHEMA_V3 {
        return Err(format!(
            "voice control command schema must be {VOICE_CONTROL_SCHEMA_V3:?}"
        ));
    }
    if kind != want {
        return Err(format!(
            "voice control command kind must be {:?}",
            want.as_str()
        ));
    }
    check_id(id)
}

fn event_head(
    schema: &str,
    kind: VoiceControlEventKind,
    want: VoiceControlEventKind,
    seq: i64,
) -> Result<(), String> {
    check_event_head_in(VOICE_CONTROL_SCHEMA_V3, schema, kind, want, seq)
}

/// The rule of `application_name`: 1 to [`VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES`] bytes
/// of printable ASCII, as the hello's `helper_version` is.
fn is_application_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES
        && name.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
}

impl VoiceControlStartV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::Start,
            self.id,
        )?;
        check_start_fields(&self.server, &self.channel, self.principal_id)
    }
}

impl VoiceControlSetMuteV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetMute,
            self.id,
        )
    }
}

impl VoiceControlSetDeafenV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetDeafen,
            self.id,
        )
    }
}

impl VoiceControlGetStateV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::GetState,
            self.id,
        )
    }
}

impl VoiceControlLeaveV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::Leave,
            self.id,
        )
    }
}

impl VoiceControlSetDevicesV3 {
    /// At least one side named, each under the device list's name rule, as in v2. The
    /// problem never repeats a name.
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetDevices,
            self.id,
        )?;
        if self.input.is_none() && self.output.is_none() {
            return Err("voice control set_devices must name input, output or both".into());
        }
        for (field, name) in [("input", &self.input), ("output", &self.output)] {
            if name
                .as_deref()
                .is_some_and(|name| !is_writable_device_name(name))
            {
                return Err(format!(
                    "voice control set_devices {field} must be 1 to 255 bytes, with no control \
                     character, quotation mark or backslash"
                ));
            }
        }
        Ok(())
    }
}

impl VoiceControlSetTalkModeV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetTalkMode,
            self.id,
        )
    }
}

impl VoiceControlSetShareAudioV3 {
    /// `VoiceControlSetShareAudioV3.Validate`, in its order: with capturing, a scope and
    /// an application name under its rule; without, neither. A scope the platform cannot
    /// capture is well-formed. The problem never repeats the name.
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetShareAudio,
            self.id,
        )?;
        if !self.capturing {
            if self.scope.is_some() {
                return Err(
                    "voice control set_share_audio with capturing false must carry no scope".into(),
                );
            }
            if self.application_name.is_some() {
                return Err(
                    "voice control set_share_audio with capturing false must carry no application_name"
                        .into(),
                );
            }
            return Ok(());
        }
        if self.scope.is_none() {
            return Err(
                "voice control set_share_audio scope must be one of application, system_excluding_call"
                    .into(),
            );
        }
        if !self
            .application_name
            .as_deref()
            .is_some_and(is_application_name)
        {
            return Err(format!(
                "voice control set_share_audio application_name must be 1 to \
                 {VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES} bytes of printable ASCII"
            ));
        }
        Ok(())
    }
}

impl VoiceControlSetShareAudioMuteV3 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKindV3::SetShareAudioMute,
            self.id,
        )
    }
}

impl VoiceControlAckV3 {
    fn check(&self) -> Result<(), String> {
        event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::Ack,
            self.seq,
        )?;
        check_id(self.id)?;
        check_ack_answer(self.ok, self.error)
    }
}

impl VoiceControlStateV3 {
    /// `VoiceControlStateV3.Validate`, in its order: v2's rules, every one and unchanged
    /// (they are v2's own check, run on the snapshot's v2 fields), and then the rules of
    /// the four fields v3 adds.
    fn check(&self) -> Result<(), String> {
        use VoiceControlPhase as Phase;
        use VoiceControlShareAudio as Audio;
        use VoiceControlShareAudioProblem as Problem;
        event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::State,
            self.seq,
        )?;
        self.v2().validate().map_err(|error| match error {
            Error::Invalid { problem, .. } => problem,
            other => other.to_string(),
        })?;
        if !(0..=VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES).contains(&self.share_audio_sources) {
            return Err(format!(
                "voice control state share_audio_sources must be 0 to {VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES}"
            ));
        }
        if self.share_audio != Audio::Off
            && (self.phase == Phase::Starting || self.phase.is_final())
        {
            return Err(format!(
                "voice control state in phase {:?} must have share_audio off",
                self.phase.as_str()
            ));
        }
        match (self.share_audio, self.share_audio_problem) {
            (Audio::Off, Some(_)) => {
                return Err(
                    "voice control state with share_audio off must have no share_audio_problem"
                        .into(),
                );
            }
            (Audio::Unavailable, None | Some(Problem::NoSources)) => {
                return Err(
                    "voice control state with share_audio unavailable needs a share_audio_problem other than no_sources"
                        .into(),
                );
            }
            (Audio::On, Some(problem)) if problem != Problem::NoSources => {
                return Err(
                    "voice control state with share_audio on can have no share_audio_problem but no_sources"
                        .into(),
                );
            }
            (Audio::On, problem) if problem.is_some() != (self.share_audio_sources == 0) => {
                return Err(
                    "voice control state share_audio_problem no_sources must agree with share_audio_sources being 0"
                        .into(),
                );
            }
            _ => {}
        }
        if self.share_audio != Audio::On && self.share_audio_sources != 0 {
            return Err(
                "voice control state share_audio_sources must be 0 unless share_audio is on".into(),
            );
        }
        Ok(())
    }
}

impl VoiceControlProtocolErrorV3 {
    fn check(&self) -> Result<(), String> {
        event_head(
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

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::path::{Path, PathBuf};

    use serde::Deserialize;
    use serde_json::Value;

    use super::*;
    use crate::control::{VOICE_CONTROL_SCHEMA_V1, parse_command, parse_event, to_line};
    use crate::control_v2::{parse_command_v2, parse_event_v2};
    use crate::{VoiceControlEvent, VoiceControlHelloV1};

    /// `pkg/schema`, read in place.
    fn schema_dir() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema")
    }

    /// `pkg/schema/testdata/voice-control-v3`, read in place.
    fn fixtures() -> PathBuf {
        schema_dir().join("testdata/voice-control-v3")
    }

    fn fixture_text(name: &str) -> String {
        let path = fixtures().join(name);
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()))
    }

    fn fixture(name: &str) -> Value {
        serde_json::from_str(&fixture_text(name)).unwrap_or_else(|e| panic!("parse {name}: {e}"))
    }

    /// Go's indented encoding made into the compact line Go's writer writes: the white
    /// space outside strings taken out.
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

    const COMMANDS: [&str; 14] = [
        "command-start.json",
        "command-set-mute.json",
        "command-set-deafen.json",
        "command-get-state.json",
        "command-leave.json",
        "command-set-devices.json",
        "command-set-devices-input.json",
        "command-set-devices-default.json",
        "command-set-talk-mode.json",
        "command-set-share-audio.json",
        // A valid command: a helper that cannot capture one application says so in the
        // state (event-state-share-audio-unsupported-scope.json).
        "command-set-share-audio-application.json",
        "command-set-share-audio-stop.json",
        "command-set-share-audio-mute.json",
        "command-set-share-audio-mute-off.json",
    ];

    const EVENTS: [&str; 21] = [
        "event-ack.json",
        "event-ack-refused.json",
        "event-protocol-error.json",
        // A state in each phase.
        "event-state-starting.json",
        "event-state-connecting.json",
        "event-state-connected.json",
        "event-state-reconnecting.json",
        "event-state-stopping.json",
        "event-state-stopped.json",
        "event-state-failed.json",
        // The share's sound.
        "event-state-share-audio-on.json",
        "event-state-share-audio-muted.json",
        "event-state-share-audio-no-sources.json",
        "event-state-share-audio-transmitting.json",
        "event-state-share-audio-muted-while-off.json",
        "event-state-share-audio-unsupported-scope.json",
        "event-state-share-audio-unsupported.json",
        "event-state-share-audio-no-pipewire.json",
        "event-state-share-audio-no-process-tree.json",
        "event-state-share-audio-publish-refused.json",
        "event-state-share-audio-not-connected.json",
    ];

    /// The hello, which is v1's and read by v1's reader; `control.rs` writes it.
    const HELLO: &str = "event-hello-v1-v2-and-v3.json";

    fn written(command: &VoiceControlCommandV3) -> String {
        match command {
            VoiceControlCommandV3::Start(c) => to_line(c),
            VoiceControlCommandV3::SetMute(c) => to_line(c),
            VoiceControlCommandV3::SetDeafen(c) => to_line(c),
            VoiceControlCommandV3::GetState(c) => to_line(c),
            VoiceControlCommandV3::Leave(c) => to_line(c),
            VoiceControlCommandV3::SetDevices(c) => to_line(c),
            VoiceControlCommandV3::SetTalkMode(c) => to_line(c),
            VoiceControlCommandV3::SetShareAudio(c) => to_line(c),
            VoiceControlCommandV3::SetShareAudioMute(c) => to_line(c),
        }
        .unwrap()
    }

    /// Every golden command is read by the helper's v3 reader, and written again byte for
    /// byte as Go writes it; and no v3 command is read by the v1 or the v2 reader.
    #[test]
    fn every_golden_command_decodes_and_encodes_as_go_writes_it() {
        let mut kinds = BTreeSet::new();
        for name in COMMANDS {
            let line = compact(&fixture_text(name));
            let command = parse_command_v3(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            assert_eq!(written(&command), line, "{name}");
            kinds.insert(command.kind());
            for (version, refusal) in [
                ("v1", parse_command(line.as_bytes()).unwrap_err()),
                ("v2", parse_command_v2(line.as_bytes()).unwrap_err()),
            ] {
                assert_eq!(
                    (refusal.code, refusal.id),
                    (VoiceControlErrorCode::UnknownSchema, None),
                    "{name} read as {version}"
                );
            }
        }
        // Each kind has a golden command. (The set is in the vocabulary's order.)
        assert_eq!(
            kinds.into_iter().collect::<Vec<_>>(),
            VoiceControlCommandKindV3::ALL,
            "the kinds of the golden commands"
        );
    }

    /// Every golden event is written by the helper's writer byte for byte as Go writes it,
    /// and read back by the owner's v3 reader as the same event; a state in each phase,
    /// each share-audio state and each problem is among them.
    #[test]
    fn every_golden_event_encodes_as_go_writes_it() {
        let (mut phases, mut audio, mut problems) =
            (BTreeSet::new(), BTreeSet::new(), BTreeSet::new());
        for name in EVENTS {
            let line = compact(&fixture_text(name));
            let event = parse_event_v3(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            let written = match &event {
                VoiceControlEventV3::Ack(e) => to_line(e),
                VoiceControlEventV3::State(e) => to_line(e),
                VoiceControlEventV3::ProtocolError(e) => to_line(e),
                VoiceControlEventV3::Unknown { .. } => panic!("{name} is of no known kind"),
            }
            .unwrap();
            assert_eq!(written, line, "{name}");
            assert_eq!(
                serde_json::from_str::<Value>(&written).unwrap(),
                fixture(name)
            );
            for (version, refusal) in [
                ("v1", parse_event(line.as_bytes()).unwrap_err()),
                ("v2", parse_event_v2(line.as_bytes()).unwrap_err()),
            ] {
                assert_eq!(
                    refusal.code,
                    VoiceControlErrorCode::UnknownSchema,
                    "{name} read as {version}"
                );
            }
            if let VoiceControlEventV3::State(state) = event {
                phases.insert(state.phase);
                audio.insert(state.share_audio);
                problems.extend(state.share_audio_problem);
            }
        }
        assert_eq!(phases.len(), VoiceControlPhase::ALL.len(), "{phases:?}");
        assert_eq!(audio.len(), VoiceControlShareAudio::ALL.len(), "{audio:?}");
        assert_eq!(
            problems.len(),
            VoiceControlShareAudioProblem::ALL.len(),
            "{problems:?}"
        );
    }

    /// The hello offering the three versions is v1's: written by the constructor byte for
    /// byte as Go writes it, read by v1's reader, and refused by v2's and v3's.
    #[test]
    fn the_hello_offers_the_three_versions_in_v1() {
        let golden = compact(&fixture_text(HELLO));
        let hello = VoiceControlHelloV1::v1_v2_and_v3("0.3.0 (48000 Hz, 10 ms frames)");
        assert_eq!(to_line(&hello).unwrap(), golden);
        let VoiceControlEvent::Hello(read) = parse_event(golden.as_bytes()).unwrap() else {
            panic!("{HELLO} is not a hello");
        };
        assert_eq!(read, hello);
        for version in [
            VOICE_CONTROL_SCHEMA_V1,
            VOICE_CONTROL_SCHEMA_V2,
            VOICE_CONTROL_SCHEMA_V3,
        ] {
            assert!(read.offers(version), "{version}");
        }
        assert!(!read.offers("conch.voice_control.v4"));
        assert_eq!(
            parse_event_v2(golden.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
        assert_eq!(
            parse_event_v3(golden.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
        // The hello of a helper that speaks v1 and v2 only is as it was.
        assert!(!VoiceControlHelloV1::v1_and_v2("0.2.0").offers(VOICE_CONTROL_SCHEMA_V3));
        // And one written under v3's name is not a hello: invalid.
        let in_v3 = golden.replacen(VOICE_CONTROL_SCHEMA_V1, VOICE_CONTROL_SCHEMA_V3, 1);
        assert_eq!(
            parse_event_v3(in_v3.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::Invalid
        );
    }

    #[test]
    fn every_fixture_in_the_directory_is_read_by_a_test_here_or_by_the_helpers() {
        let mut claimed: BTreeSet<String> = COMMANDS
            .iter()
            .chain(EVENTS.iter())
            .map(|name| (*name).to_owned())
            .collect();
        claimed.insert(HELLO.into());
        // The case files and lines/ below; sensitive.json and the transcripts are for the
        // helper's own tests, once it speaks v3 (#1586).
        claimed.extend(CASE_FILES.iter().map(|name| (*name).to_owned()));
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

    /// The conformance documents of the v3 readers, read as one.
    const CASE_FILES: [&str; 2] = ["cases.json", "cases-strings.json"];

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
    }

    impl Case {
        fn line(&self) -> Vec<u8> {
            match (&self.line, &self.line_hex, &self.line_file) {
                (Some(line), None, None) => line.as_bytes().to_vec(),
                (None, Some(hex), None) => (0..hex.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
                    .collect(),
                (None, None, Some(file)) => std::fs::read(fixtures().join(file)).unwrap(),
                _ => panic!("case {:?} has not exactly one form of its line", self.name),
            }
        }
    }

    fn cases_of(dir: &Path, file: &str) -> Vec<Case> {
        let text = std::fs::read_to_string(dir.join(file)).unwrap();
        serde_json::from_str::<Cases>(&text).unwrap().cases
    }

    /// Reads `line` with the v3 reader of `case`'s direction and holds the result to what
    /// the case wants: the kind, or the code and the id.
    fn check_case(case: &Case, line: &[u8]) {
        let name = format!("{}: {}", case.direction, case.name);
        let (kind, refusal) = match case.direction.as_str() {
            "command" => match parse_command_v3(line) {
                Ok(command) => (Some(command.kind().as_str().to_owned()), None),
                Err(refusal) => (None, Some(refusal)),
            },
            "event" => match parse_event_v3(line) {
                Ok(VoiceControlEventV3::Unknown { kind, .. }) => {
                    assert_eq!(case.want.result, "skip", "{name}: skipped");
                    (Some(kind), None)
                }
                Ok(event) => (Some(event.kind().to_owned()), None),
                Err(refusal) => (None, Some(refusal)),
            },
            other => panic!("{name}: direction {other}"),
        };
        match case.want.result.as_str() {
            "accept" | "skip" => {
                assert_eq!(refusal, None, "{name}: refused");
                assert_eq!(kind.as_deref(), case.want.kind.as_deref(), "{name}");
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

    /// Every conformance case is read with the same result, code and id as Go reads it:
    /// the commands by the helper's v3 reader, the events by the owner's.
    #[test]
    fn every_conformance_case_is_read_as_go_reads_it() {
        let (mut commands, mut events) = (0, 0);
        for case in CASE_FILES
            .iter()
            .flat_map(|file| cases_of(&fixtures(), file))
        {
            match case.direction.as_str() {
                "command" => commands += 1,
                _ => events += 1,
            }
            check_case(&case, &case.line());
        }
        assert!(commands >= 70, "only {commands} command cases");
        assert!(events >= 70, "only {events} event cases");
    }

    /// "A v3 session is v2 plus": every v2 conformance case that is about a message,
    /// written under v3's name (and, for a state, with the share's sound off), is read by
    /// the v3 readers with the result v2's give it, as the Go test of the same name checks
    /// of Go's. The framing cases and the cases about other versions are v3's own.
    #[test]
    fn every_v2_case_about_a_message_is_read_the_same_in_v3() {
        let v2 = schema_dir().join("testdata/voice-control-v2");
        let (mut commands, mut events) = (0, 0);
        for case in CASE_FILES.iter().flat_map(|file| cases_of(&v2, file)) {
            let about_a_message = case.want.result != "refuse"
                || matches!(case.want.code.as_deref(), Some("invalid" | "unknown_kind"));
            let Some(Ok(Value::Object(mut fields))) = case
                .line
                .as_deref()
                .filter(|_| about_a_message)
                .map(serde_json::from_str::<Value>)
            else {
                continue;
            };
            if fields.get("schema").and_then(Value::as_str) != Some(VOICE_CONTROL_SCHEMA_V2) {
                continue;
            }
            fields.insert("schema".into(), VOICE_CONTROL_SCHEMA_V3.into());
            if case.direction == "event"
                && fields.get("kind").and_then(Value::as_str) == Some("state")
            {
                fields.insert("share_audio".into(), "off".into());
                fields.insert("share_audio_muted".into(), false.into());
                fields.insert("share_audio_sources".into(), 0.into());
            }
            match case.direction.as_str() {
                "command" => commands += 1,
                _ => events += 1,
            }
            check_case(&case, Value::Object(fields).to_string().as_bytes());
        }
        assert!(commands >= 30, "only {commands} command cases of v2");
        assert!(events >= 30, "only {events} event cases of v2");
    }

    /// The vocabularies are the Go constants' words, in the Go order, and so are the
    /// schema name and the two bounds.
    #[test]
    fn the_vocabularies_the_name_and_the_bounds_are_the_schemas() {
        let read = |file: &str| std::fs::read_to_string(schema_dir().join(file)).unwrap();
        let v3 = read("voice_control_v3.go");
        let words_of = |go: &str, go_type: &str| -> Vec<String> {
            go.lines()
                .filter_map(|line| {
                    let mut parts = line.split_whitespace();
                    let _name = parts.next()?;
                    (parts.next()? == go_type && parts.next()? == "=").then_some(())?;
                    Some(parts.next()?.trim_matches('"').to_owned())
                })
                .collect()
        };
        fn ours<T: Copy>(all: &[T], word: fn(T) -> &'static str) -> Vec<String> {
            all.iter().map(|value| word(*value).to_owned()).collect()
        }
        assert_eq!(
            words_of(&v3, "VoiceControlCommandKindV3"),
            ours(
                VoiceControlCommandKindV3::ALL,
                VoiceControlCommandKindV3::as_str
            )
        );
        assert_eq!(
            words_of(&v3, "VoiceControlShareAudio"),
            ours(VoiceControlShareAudio::ALL, VoiceControlShareAudio::as_str)
        );
        assert_eq!(
            words_of(&v3, "VoiceControlShareAudioProblem"),
            ours(
                VoiceControlShareAudioProblem::ALL,
                VoiceControlShareAudioProblem::as_str
            )
        );
        assert_eq!(
            words_of(&read("voice_companion.go"), "VoiceShareAudioScope"),
            ours(VoiceShareAudioScope::ALL, VoiceShareAudioScope::as_str)
        );
        // v3's kinds are v2's, word for word and in order, and two more.
        let v2_kinds = ours(
            crate::VoiceControlCommandKind::ALL,
            crate::VoiceControlCommandKind::as_str,
        );
        let v3_kinds = ours(
            VoiceControlCommandKindV3::ALL,
            VoiceControlCommandKindV3::as_str,
        );
        assert_eq!(v3_kinds[..v2_kinds.len()], v2_kinds[..]);
        assert_eq!(
            v3_kinds[v2_kinds.len()..],
            ["set_share_audio", "set_share_audio_mute"]
        );
        for line in [
            format!("const VoiceControlSchemaV3 = \"{VOICE_CONTROL_SCHEMA_V3}\""),
            format!(
                "\tVoiceControlMaxApplicationNameBytes = {VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES}"
            ),
            format!("\tVoiceControlMaxShareAudioSources = {VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES}"),
        ] {
            assert!(v3.lines().any(|go| go == line), "{line}");
        }
    }

    /// No reader takes a line of another version, in any direction: the version is fixed
    /// by the start. A share-audio kind under an earlier version's name is `unknown_kind`
    /// there, as any kind that version does not have.
    #[test]
    fn a_line_of_one_version_is_unknown_schema_to_the_others() {
        let mute = |version: &str| {
            format!(r#"{{"schema":"{version}","kind":"set_mute","id":2,"muted":true}}"#)
        };
        let code = |refusal: VoiceControlRefusal| refusal.code;
        for version in [VOICE_CONTROL_SCHEMA_V1, VOICE_CONTROL_SCHEMA_V2] {
            assert_eq!(
                parse_command_v3(mute(version).as_bytes()).map_err(code),
                Err(VoiceControlErrorCode::UnknownSchema),
                "{version}"
            );
        }
        let v3_mute = mute(VOICE_CONTROL_SCHEMA_V3);
        assert!(matches!(
            parse_command_v3(v3_mute.as_bytes()),
            Ok(VoiceControlCommandV3::SetMute(_))
        ));
        assert_eq!(
            parse_command(v3_mute.as_bytes()).map_err(code),
            Err(VoiceControlErrorCode::UnknownSchema)
        );
        assert_eq!(
            parse_command_v2(v3_mute.as_bytes()).map_err(code),
            Err(VoiceControlErrorCode::UnknownSchema)
        );
        for dir in ["voice-control", "voice-control-v2"] {
            let state = compact(
                &std::fs::read_to_string(
                    schema_dir()
                        .join("testdata")
                        .join(dir)
                        .join("event-state-connected.json"),
                )
                .unwrap(),
            );
            assert_eq!(
                parse_event_v3(state.as_bytes()).map_err(code),
                Err(VoiceControlErrorCode::UnknownSchema),
                "{dir}"
            );
        }
        for kind in ["set_share_audio", "set_share_audio_mute"] {
            for version in [VOICE_CONTROL_SCHEMA_V1, VOICE_CONTROL_SCHEMA_V2] {
                let line = format!(
                    r#"{{"schema":"{version}","kind":"{kind}","id":7,"capturing":false,"muted":true}}"#
                );
                let refusal = if version == VOICE_CONTROL_SCHEMA_V1 {
                    parse_command(line.as_bytes()).unwrap_err()
                } else {
                    parse_command_v2(line.as_bytes()).unwrap_err()
                };
                assert_eq!(
                    (refusal.code, refusal.id),
                    (VoiceControlErrorCode::UnknownKind, Some(7)),
                    "{kind} in {version}"
                );
            }
        }
    }

    fn state() -> VoiceControlStateV3 {
        serde_json::from_value(fixture("event-state-connected.json")).unwrap()
    }

    fn on(sources: i64) -> VoiceControlStateV3 {
        VoiceControlStateV3 {
            share_audio: VoiceControlShareAudio::On,
            share_audio_sources: sources,
            share_audio_problem: (sources == 0).then_some(VoiceControlShareAudioProblem::NoSources),
            ..state()
        }
    }

    /// The writer refuses what the reader refuses: a v3 state that does not hold together,
    /// by a rule of the four fields or by one of v2's, is never written.
    #[test]
    fn a_state_that_does_not_hold_together_is_not_written() {
        use VoiceControlBlock as Block;
        use VoiceControlPhase as Phase;
        use VoiceControlShareAudio as Audio;
        use VoiceControlShareAudioProblem as Problem;
        let starting = || VoiceControlStateV3 {
            phase: Phase::Starting,
            capture: VoiceControlCapture::Off,
            playback: VoiceControlPlayback::Off,
            blocked: vec![Block::NotConnected],
            ptt: VoiceControlPtt::Unknown,
            ..state()
        };
        let stopped = || VoiceControlStateV3 {
            phase: Phase::Stopped,
            reason: Some(VoiceControlReason::Left),
            capture: VoiceControlCapture::Off,
            playback: VoiceControlPlayback::Off,
            blocked: vec![Block::NotConnected],
            ..state()
        };
        let unavailable = |problem| VoiceControlStateV3 {
            share_audio: Audio::Unavailable,
            share_audio_problem: problem,
            ..state()
        };
        let cases: Vec<(&str, VoiceControlStateV3)> = vec![
            (
                "on while starting",
                VoiceControlStateV3 {
                    share_audio: Audio::On,
                    share_audio_sources: 1,
                    ..starting()
                },
            ),
            (
                "unavailable while starting",
                VoiceControlStateV3 {
                    share_audio: Audio::Unavailable,
                    share_audio_problem: Some(Problem::NotConnected),
                    ..starting()
                },
            ),
            (
                "on when stopped",
                VoiceControlStateV3 {
                    share_audio: Audio::On,
                    share_audio_sources: 1,
                    ..stopped()
                },
            ),
            (
                "off with a problem",
                VoiceControlStateV3 {
                    share_audio_problem: Some(Problem::NotConnected),
                    ..state()
                },
            ),
            (
                "off with sources",
                VoiceControlStateV3 {
                    share_audio_sources: 2,
                    ..state()
                },
            ),
            ("unavailable with no problem", unavailable(None)),
            (
                "unavailable with no_sources",
                unavailable(Some(Problem::NoSources)),
            ),
            (
                "unavailable with sources",
                VoiceControlStateV3 {
                    share_audio_sources: 2,
                    ..unavailable(Some(Problem::NoPipewire))
                },
            ),
            (
                "on with a problem that is not no_sources",
                VoiceControlStateV3 {
                    share_audio_problem: Some(Problem::PublishRefused),
                    ..on(2)
                },
            ),
            (
                "on with no_sources and sources",
                VoiceControlStateV3 {
                    share_audio_problem: Some(Problem::NoSources),
                    ..on(2)
                },
            ),
            (
                "on with no sources and no no_sources",
                VoiceControlStateV3 {
                    share_audio_problem: None,
                    ..on(0)
                },
            ),
            (
                "sources below zero",
                VoiceControlStateV3 {
                    share_audio_sources: -1,
                    ..on(1)
                },
            ),
            (
                "one source too many",
                on(VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES + 1),
            ),
            (
                "a v2 schema",
                VoiceControlStateV3 {
                    schema: VOICE_CONTROL_SCHEMA_V2.into(),
                    ..state()
                },
            ),
            // v2's rules stand beside the share's sound.
            (
                "playback off while connected",
                VoiceControlStateV3 {
                    playback: VoiceControlPlayback::Off,
                    ..on(2)
                },
            ),
            (
                "transmitting in push_to_talk without a talk key",
                VoiceControlStateV3 {
                    transmitting: true,
                    ptt: VoiceControlPtt::NotConfigured,
                    ..on(2)
                },
            ),
            (
                "transmitting while muted",
                VoiceControlStateV3 {
                    transmitting: true,
                    muted: true,
                    blocked: vec![Block::Muted],
                    ..on(2)
                },
            ),
        ];
        for (name, state) in cases {
            assert!(to_line(&state).is_err(), "{name} was written");
        }
        // What holds together: on with and without sources, each reason for unavailable,
        // the mute in any phase, and the microphone going its own way beside all of it.
        let mut allowed = vec![
            state(),
            on(3),
            on(0),
            on(VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES),
            VoiceControlStateV3 {
                share_audio_muted: true,
                ..on(2)
            },
            VoiceControlStateV3 {
                share_audio_muted: true,
                ..state()
            },
            VoiceControlStateV3 {
                share_audio_muted: true,
                ..starting()
            },
            VoiceControlStateV3 {
                share_audio_muted: true,
                ..stopped()
            },
            VoiceControlStateV3 {
                transmitting: true,
                ..on(2)
            },
            VoiceControlStateV3 {
                muted: true,
                blocked: vec![Block::Muted],
                ..on(2)
            },
            VoiceControlStateV3 {
                capture: VoiceControlCapture::Unavailable,
                blocked: vec![Block::NoMicrophone],
                ..on(2)
            },
        ];
        allowed.extend(
            Problem::ALL
                .iter()
                .filter(|problem| **problem != Problem::NoSources)
                .map(|problem| unavailable(Some(*problem))),
        );
        for state in allowed {
            to_line(&state).unwrap_or_else(|e| panic!("{state:?}: {e}"));
        }
    }

    fn share(capturing: bool, scope: Option<&str>, name: Option<&str>) -> String {
        let mut line = serde_json::json!({
            "schema": VOICE_CONTROL_SCHEMA_V3, "kind": "set_share_audio", "id": 3,
            "capturing": capturing,
        });
        if let Some(scope) = scope {
            line["scope"] = scope.into();
        }
        if let Some(name) = name {
            line["application_name"] = name.into();
        }
        line.to_string()
    }

    /// A `set_share_audio` to try: its name, `capturing`, the scope and the application
    /// name if it carries them, and whether it is well-formed.
    type ShareCase<'a> = (&'a str, bool, Option<&'a str>, Option<&'a str>, bool);

    /// The rule of a `set_share_audio`, one case each, as Go's table has it.
    #[test]
    fn a_set_share_audio_is_held_to_its_shape() {
        const SYSTEM: Option<&str> = Some("system_excluding_call");
        let longest = "n".repeat(VOICE_CONTROL_MAX_APPLICATION_NAME_BYTES);
        let too_long = format!("{longest}n");
        let every_ascii: String = (0x20_u8..=0x7e).map(char::from).collect();
        let cases: [ShareCase<'_>; 20] = [
            ("the system's sound", true, SYSTEM, Some("Conch"), true),
            (
                "one application's sound",
                true,
                Some("application"),
                Some("Conch"),
                true,
            ),
            ("a stop", false, None, None, true),
            ("the longest name", true, SYSTEM, Some(&longest), true),
            (
                "every printable ASCII character",
                true,
                SYSTEM,
                Some(&every_ascii),
                true,
            ),
            ("no scope", true, None, Some("Conch"), false),
            (
                "a scope of no version",
                true,
                Some("system"),
                Some("Conch"),
                false,
            ),
            ("an empty scope", true, Some(""), Some("Conch"), false),
            ("no name", true, SYSTEM, None, false),
            ("an empty name", true, SYSTEM, Some(""), false),
            ("a name too long", true, SYSTEM, Some(&too_long), false),
            (
                "an escape sequence",
                true,
                SYSTEM,
                Some("\u{1b}[2JConch"),
                false,
            ),
            ("a line feed", true, SYSTEM, Some("two\nlines"), false),
            (
                "the delete character",
                true,
                SYSTEM,
                Some("Conch\u{7f}"),
                false,
            ),
            (
                "a letter outside ASCII",
                true,
                SYSTEM,
                Some("C\u{f6}nch"),
                false,
            ),
            ("a stop with a scope", false, SYSTEM, None, false),
            ("a stop with an empty scope", false, Some(""), None, false),
            ("a stop with a name", false, None, Some("Conch"), false),
            ("a stop with an empty name", false, None, Some(""), false),
            ("a stop with both", false, SYSTEM, Some("Conch"), false),
        ];
        for (name, capturing, scope, app, ok) in cases {
            let line = share(capturing, scope, app);
            match parse_command_v3(line.as_bytes()) {
                Ok(VoiceControlCommandV3::SetShareAudio(command)) => {
                    assert!(ok, "{name} was read");
                    // Written, it is read back as the same command.
                    let written = to_line(&command).unwrap();
                    assert_eq!(
                        parse_command_v3(written.as_bytes()),
                        Ok(VoiceControlCommandV3::SetShareAudio(command)),
                        "{name}"
                    );
                }
                Ok(other) => panic!("{name} was read as {other:?}"),
                Err(refusal) => {
                    assert!(!ok, "{name}: {refusal}");
                    assert_eq!(
                        (refusal.code, refusal.id),
                        (VoiceControlErrorCode::Invalid, Some(3)),
                        "{name}"
                    );
                }
            }
        }
        // And the writer refuses what the reader refuses.
        let VoiceControlCommandV3::SetShareAudio(good) =
            parse_command_v3(share(true, SYSTEM, Some("Conch")).as_bytes()).unwrap()
        else {
            panic!("not a set_share_audio");
        };
        for bad in [
            VoiceControlSetShareAudioV3 {
                scope: None,
                ..good.clone()
            },
            VoiceControlSetShareAudioV3 {
                application_name: None,
                ..good.clone()
            },
            VoiceControlSetShareAudioV3 {
                capturing: false,
                ..good.clone()
            },
            VoiceControlSetShareAudioV3 {
                schema: VOICE_CONTROL_SCHEMA_V2.into(),
                ..good.clone()
            },
            VoiceControlSetShareAudioV3 {
                kind: VoiceControlCommandKindV3::SetShareAudioMute,
                ..good.clone()
            },
        ] {
            assert!(to_line(&bad).is_err(), "{bad:?} was written");
        }
    }

    #[test]
    fn an_application_name_is_never_shown_and_never_said_in_a_refusal() {
        let line = share(
            true,
            Some("system_excluding_call"),
            Some("FAKE-app-do-not-print"),
        );
        let VoiceControlCommandV3::SetShareAudio(command) =
            parse_command_v3(line.as_bytes()).unwrap()
        else {
            panic!("not a set_share_audio");
        };
        let shown = format!("{command:?} {command:#?}");
        assert!(!shown.contains("FAKE"), "{shown}");
        assert!(shown.contains("application_name: Some(\"..\")"), "{shown}");
        assert!(shown.contains("SystemExcludingCall"), "{shown}");

        let bad = share(
            true,
            Some("system_excluding_call"),
            Some("FAKE \u{1b}[2J do-not-print"),
        );
        let refusal = parse_command_v3(bad.as_bytes()).unwrap_err();
        assert_eq!(
            (refusal.code, refusal.id),
            (VoiceControlErrorCode::Invalid, Some(3))
        );
        let shown = format!("{refusal} {refusal:?}");
        assert!(!shown.contains("FAKE"), "{shown}");

        // A device name is not shown either, as in v2.
        let devices = br#"{"schema":"conch.voice_control.v3","kind":"set_devices","id":3,"input":"alsa_input.FAKE-do-not-print"}"#;
        let shown = format!("{:?}", parse_command_v3(devices).unwrap());
        assert!(!shown.contains("FAKE"), "{shown}");
    }

    #[test]
    fn an_ack_and_a_protocol_error_are_written_in_v3() {
        assert_eq!(
            to_line(&VoiceControlAckV3::new(3, 1, None)).unwrap(),
            compact(&fixture_text("event-ack.json"))
        );
        assert_eq!(
            to_line(&VoiceControlAckV3::new(
                9,
                6,
                Some(VoiceControlErrorCode::Invalid)
            ))
            .unwrap(),
            compact(&fixture_text("event-ack-refused.json"))
        );
        assert!(
            to_line(&VoiceControlAckV3::new(
                9,
                6,
                Some(VoiceControlErrorCode::TooLong)
            ))
            .is_err()
        );
        assert_eq!(
            to_line(&VoiceControlProtocolErrorV3::new(
                8,
                VoiceControlErrorCode::UnknownSchema
            ))
            .unwrap(),
            compact(&fixture_text("event-protocol-error.json"))
        );
        assert!(
            to_line(&VoiceControlProtocolErrorV3::new(
                2,
                VoiceControlErrorCode::Stopping
            ))
            .is_err()
        );
    }
}
