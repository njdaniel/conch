//! Version 2 of the control protocol of a managed `conch-voice` (`conch.voice_control.v2`,
//! `docs/design/tui-voice.md` §20): Rust copies of the shapes in
//! `pkg/schema/voice_control_v2.go`, read and written by the same rules as v1's
//! (`control.rs`).
//!
//! v2 adds two things a v1 state cannot say and a v1 helper cannot be asked for: switching
//! the microphone and the speakers while in voice ([`VoiceControlSetDevicesV2`]), with the
//! speakers' state beside the microphone's ([`VoiceControlPlayback`]); and an open
//! microphone ([`VoiceControlSetTalkModeV2`], [`VoiceControlTalkMode`]), where the
//! microphone's own sound opens the transmit gate instead of a press.
//!
//! What v1 leaves out, v2 leaves out too: no secret, no free text from the helper (a device
//! the owner named is never repeated in an event), and no command that opens the gate.
//!
//! The hello is v1's ([`crate::VoiceControlHelloV1::v1_and_v2`]): the owner picks the
//! version by the schema of its start, and from then on every line of either side is of
//! that version. [`parse_command_v2`] and [`parse_event_v2`] apply Go's rules in Go's order
//! (`ParseVoiceControlCommandV2`, `ParseVoiceControlEventV2`), which are v1's framing rules
//! with two of their own: a line of v1 is `unknown_schema`, and a hello in v2 is `invalid`.
//! A side of `set_devices` that is left out is left alone; one that is present and empty
//! is `invalid`, so that no reader can take an empty name for "leave it".

use std::fmt;

use serde::Serialize;

use crate::control::{
    Loose, VoiceControlBlock, VoiceControlCapture, VoiceControlCommandKind, VoiceControlCredential,
    VoiceControlErrorCode, VoiceControlEventKind, VoiceControlPhase, VoiceControlPtt,
    VoiceControlPttProblem, VoiceControlReason, VoiceControlRefusal, check_ack_answer,
    check_blocked, check_command_head_in, check_event_head_in, check_id, check_ptt_problem,
    check_reason, check_start_fields, frame, id_in_range, message, refused, typed, vocabulary,
};
use crate::types::wire_object;
use crate::voice_devices::is_writable_device_name;

/// The name every v2 message but the hello carries in its `schema` field. The hello is
/// always v1.
pub const VOICE_CONTROL_SCHEMA_V2: &str = "conch.voice_control.v2";

/// The name that means PipeWire's default device in `set_devices`, as it does in
/// `voice.toml`.
pub const VOICE_CONTROL_DEVICE_DEFAULT: &str = "default";

vocabulary!(
    /// What may open the transmit gate.
    VoiceControlTalkMode, "not a talk mode", {
    /// A press of the talk key opens the gate and its release shuts it. The default, and
    /// the mode of every v1 session.
    PushToTalk = "push_to_talk",
    /// The microphone's own sound opens the gate (one frame at or above the configured
    /// level after quiet) and quiet shuts it. The talk key is ignored; the mute and deafen
    /// keys are not.
    Open = "open",
});

vocabulary!(
    /// The state of the speakers' stream, as [`VoiceControlCapture`] is the microphone's.
    VoiceControlPlayback, "not a playback state", {
    /// No playback stream is open: before the speakers are opened (starting) and after
    /// they are closed.
    Off = "off",
    /// The playback stream is open and has a device. Deafened is not off: the stream stays
    /// open and plays silence.
    On = "on",
    /// The speakers named are not there, or went away. Nothing is played in their place;
    /// playback resumes when they appear. Transmitting does not need them.
    Unavailable = "unavailable",
});

/// The first command of a v2 session (owner to helper). Its fields are v1's: the talk mode
/// is not carried, the helper reads it from `voice.toml`, and the state reports it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStartV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
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

wire_object!(VoiceControlStartV2, "a voice control start", unknown = ignored, {
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

/// Sets whether the user is muted (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetMuteV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetMute`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub muted: bool,
}

wire_object!(VoiceControlSetMuteV2, "a voice control set_mute", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    muted: required,
});

/// Sets whether the user is deafened (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetDeafenV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetDeafen`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The state wanted.
    pub deafened: bool,
}

wire_object!(VoiceControlSetDeafenV2, "a voice control set_deafen", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    deafened: required,
});

/// Asks for a state snapshot now (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlGetStateV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::GetState`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlGetStateV2, "a voice control get_state", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// Leaves voice (owner to helper), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlLeaveV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::Leave`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
}

wire_object!(VoiceControlLeaveV2, "a voice control leave", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
});

/// Names the microphone, the speakers, or both, to use from now on (owner to helper). A
/// desired state, like `set_mute`: the session targets the device named whether or not it
/// is there, and the state says whether it is. A side left out is left alone.
///
/// `Debug` shows which sides are named and never a name: the helper does not repeat a name
/// in anything it writes (`docs/design/tui-voice.md` §20.7).
#[derive(Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetDevicesV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetDevices`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The microphone: a PipeWire `node.name` under the device list's rule
    /// ([`is_writable_device_name`]), or [`VOICE_CONTROL_DEVICE_DEFAULT`]. `None` to leave
    /// the microphone alone.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub input: Option<String>,
    /// The speakers, under the same rule.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub output: Option<String>,
}

wire_object!(VoiceControlSetDevicesV2, "a voice control set_devices", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    input: maybe,
    output: maybe,
});

impl fmt::Debug for VoiceControlSetDevicesV2 {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let side = |name: &Option<String>| name.as_ref().map(|_| "..");
        f.debug_struct("VoiceControlSetDevicesV2")
            .field("schema", &self.schema)
            .field("kind", &self.kind)
            .field("id", &self.id)
            .field("input", &side(&self.input))
            .field("output", &side(&self.output))
            .finish()
    }
}

/// Chooses what may open the gate (owner to helper): a press, or the microphone's own
/// sound. A desired state, like `set_mute`. Nothing here opens the gate.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlSetTalkModeV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlCommandKind::SetTalkMode`].
    pub kind: VoiceControlCommandKind,
    /// The command's id.
    pub id: i64,
    /// The mode wanted.
    pub talk_mode: VoiceControlTalkMode,
}

wire_object!(VoiceControlSetTalkModeV2, "a voice control set_talk_mode", unknown = ignored, {
    schema: required,
    kind: required,
    id: required,
    talk_mode: required,
});

/// The answer to one command (helper to owner), as in v1.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlAckV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
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

wire_object!(VoiceControlAckV2, "a voice control ack", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    id: required,
    ok: required,
    error: maybe,
});

impl VoiceControlAckV2 {
    /// The answer to command `id`: carried out if `refusal` is `None`.
    #[must_use]
    pub fn new(seq: i64, id: i64, refusal: Option<VoiceControlErrorCode>) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V2.to_owned(),
            kind: VoiceControlEventKind::Ack,
            seq,
            id,
            ok: refusal.is_none(),
            error: refusal,
        }
    }
}

/// A whole-state snapshot (helper to owner): v1's fields, the speakers' state beside the
/// microphone's, and the talk mode in force. It names no device.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlStateV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
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
    /// The speakers' stream.
    pub playback: VoiceControlPlayback,
    /// Whether audio is leaving now, release tail included.
    pub transmitting: bool,
    /// Whether the user is muted.
    pub muted: bool,
    /// Whether the user is deafened.
    pub deafened: bool,
    /// Every reason a press, or in open mode an onset, would do nothing now, most
    /// important first.
    pub blocked: Vec<VoiceControlBlock>,
    /// The talk mode in force: push-to-talk until the configuration has been read.
    pub talk_mode: VoiceControlTalkMode,
    /// Whether hold-to-talk can work. Reported in open mode too: the key device is still
    /// read for the mute and deafen keys.
    pub ptt: VoiceControlPtt,
    /// Why the key device is not being read: present exactly when `ptt` is unavailable or
    /// lost.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ptt_problem: Option<VoiceControlPttProblem>,
}

wire_object!(VoiceControlStateV2, "a voice control state", unknown = ignored, {
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
});

impl VoiceControlStateV2 {
    /// Whether `block` is among the reasons a press, or an onset, would do nothing.
    #[must_use]
    pub fn blocks(&self, block: VoiceControlBlock) -> bool {
        self.blocked.contains(&block)
    }
}

/// A line the helper cannot act on (helper to owner), as in v1; a failed state follows.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlProtocolErrorV2 {
    /// Always [`VOICE_CONTROL_SCHEMA_V2`].
    pub schema: String,
    /// Always [`VoiceControlEventKind::ProtocolError`].
    pub kind: VoiceControlEventKind,
    /// The event's seq.
    pub seq: i64,
    /// What was wrong: a code that ends a session.
    pub code: VoiceControlErrorCode,
}

wire_object!(VoiceControlProtocolErrorV2, "a voice control protocol_error", unknown = ignored, {
    schema: required,
    kind: required,
    seq: required,
    code: required,
});

impl VoiceControlProtocolErrorV2 {
    /// The protocol_error for `code`.
    #[must_use]
    pub fn new(seq: i64, code: VoiceControlErrorCode) -> Self {
        Self {
            schema: VOICE_CONTROL_SCHEMA_V2.to_owned(),
            kind: VoiceControlEventKind::ProtocolError,
            seq,
            code,
        }
    }
}

/// One v2 command, as [`parse_command_v2`] read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlCommandV2 {
    /// `start`.
    Start(VoiceControlStartV2),
    /// `set_mute`.
    SetMute(VoiceControlSetMuteV2),
    /// `set_deafen`.
    SetDeafen(VoiceControlSetDeafenV2),
    /// `get_state`.
    GetState(VoiceControlGetStateV2),
    /// `leave`.
    Leave(VoiceControlLeaveV2),
    /// `set_devices`.
    SetDevices(VoiceControlSetDevicesV2),
    /// `set_talk_mode`.
    SetTalkMode(VoiceControlSetTalkModeV2),
}

impl VoiceControlCommandV2 {
    /// The command's kind.
    #[must_use]
    pub fn kind(&self) -> VoiceControlCommandKind {
        match self {
            Self::Start(_) => VoiceControlCommandKind::Start,
            Self::SetMute(_) => VoiceControlCommandKind::SetMute,
            Self::SetDeafen(_) => VoiceControlCommandKind::SetDeafen,
            Self::GetState(_) => VoiceControlCommandKind::GetState,
            Self::Leave(_) => VoiceControlCommandKind::Leave,
            Self::SetDevices(_) => VoiceControlCommandKind::SetDevices,
            Self::SetTalkMode(_) => VoiceControlCommandKind::SetTalkMode,
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
        }
    }
}

/// One v2 event, as [`parse_event_v2`] read it. There is no hello: the hello is v1, read
/// with [`crate::parse_event`] before the version is chosen.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VoiceControlEventV2 {
    /// `ack`.
    Ack(VoiceControlAckV2),
    /// `state`.
    State(VoiceControlStateV2),
    /// `protocol_error`.
    ProtocolError(VoiceControlProtocolErrorV2),
    /// An event of a kind this version does not know, which a reader skips.
    Unknown {
        /// Its seq.
        seq: i64,
        /// Its kind, as written.
        kind: String,
    },
}

impl VoiceControlEventV2 {
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
    VoiceControlStartV2,
    VoiceControlSetMuteV2,
    VoiceControlSetDeafenV2,
    VoiceControlGetStateV2,
    VoiceControlLeaveV2,
    VoiceControlSetDevicesV2,
    VoiceControlSetTalkModeV2,
    VoiceControlAckV2,
    VoiceControlStateV2,
    VoiceControlProtocolErrorV2,
);

/// Reads one v2 command line, without its line feed: the helper's reader, once the start
/// was written in v2. The framing rules and the order of refusals are v1's
/// ([`crate::parse_command`]); a v1 line is `unknown_schema`.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not a v2 command the helper can carry
/// out.
pub fn parse_command_v2(line: &[u8]) -> Result<VoiceControlCommandV2, VoiceControlRefusal> {
    let (text, head) = frame(line)?;
    head.check_schema_is(VOICE_CONTROL_SCHEMA_V2)?;
    let id = match head.id {
        Some(Loose::Whole(id)) if id_in_range(id) => id,
        _ => return Err(refused(VoiceControlErrorCode::Invalid, None)),
    };
    let kind = head
        .command_kind()
        .filter(|kind| kind.known_in(VOICE_CONTROL_SCHEMA_V2))
        .ok_or(refused(VoiceControlErrorCode::UnknownKind, Some(id)))?;
    let some = Some(id);
    let command = match kind {
        VoiceControlCommandKind::Start => VoiceControlCommandV2::Start(typed(text, some)?),
        VoiceControlCommandKind::SetMute => VoiceControlCommandV2::SetMute(typed(text, some)?),
        VoiceControlCommandKind::SetDeafen => VoiceControlCommandV2::SetDeafen(typed(text, some)?),
        VoiceControlCommandKind::GetState => VoiceControlCommandV2::GetState(typed(text, some)?),
        VoiceControlCommandKind::Leave => VoiceControlCommandV2::Leave(typed(text, some)?),
        VoiceControlCommandKind::SetDevices => {
            VoiceControlCommandV2::SetDevices(typed(text, some)?)
        }
        VoiceControlCommandKind::SetTalkMode => {
            VoiceControlCommandV2::SetTalkMode(typed(text, some)?)
        }
    };
    command
        .check()
        .map_err(|_| refused(VoiceControlErrorCode::Invalid, some))?;
    Ok(command)
}

/// Reads one v2 event line, without its line feed: the owner's reader, once it wrote its
/// start in v2. An event of a kind this version does not know comes back as
/// [`VoiceControlEventV2::Unknown`], to be skipped. A hello in v2 is `invalid`: the hello is
/// written in v1 only, and a reader of v2 has already read it.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not a v2 event.
pub fn parse_event_v2(line: &[u8]) -> Result<VoiceControlEventV2, VoiceControlRefusal> {
    let invalid = refused(VoiceControlErrorCode::Invalid, None);
    let (text, head) = frame(line)?;
    head.check_schema_is(VOICE_CONTROL_SCHEMA_V2)?;
    let seq = match head.seq {
        Some(Loose::Whole(seq)) if id_in_range(seq) => seq,
        _ => return Err(invalid),
    };
    let Some(word) = head.event_kind() else {
        return Err(invalid);
    };
    let Some(kind) = VoiceControlEventKind::from_word(&word) else {
        return Ok(VoiceControlEventV2::Unknown { seq, kind: word });
    };
    let (event, checked) = match kind {
        VoiceControlEventKind::Hello => return Err(invalid),
        VoiceControlEventKind::Ack => {
            let ack: VoiceControlAckV2 = typed(text, None)?;
            let checked = ack.check();
            (VoiceControlEventV2::Ack(ack), checked)
        }
        VoiceControlEventKind::State => {
            let state: VoiceControlStateV2 = typed(text, None)?;
            let checked = state.check();
            (VoiceControlEventV2::State(state), checked)
        }
        VoiceControlEventKind::ProtocolError => {
            let error: VoiceControlProtocolErrorV2 = typed(text, None)?;
            let checked = error.check();
            (VoiceControlEventV2::ProtocolError(error), checked)
        }
    };
    checked.map_err(|_| invalid)?;
    Ok(event)
}

fn command_head(
    schema: &str,
    kind: VoiceControlCommandKind,
    want: VoiceControlCommandKind,
    id: i64,
) -> Result<(), String> {
    check_command_head_in(VOICE_CONTROL_SCHEMA_V2, schema, kind, want, id)
}

fn event_head(
    schema: &str,
    kind: VoiceControlEventKind,
    want: VoiceControlEventKind,
    seq: i64,
) -> Result<(), String> {
    check_event_head_in(VOICE_CONTROL_SCHEMA_V2, schema, kind, want, seq)
}

impl VoiceControlStartV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::Start,
            self.id,
        )?;
        check_start_fields(&self.server, &self.channel, self.principal_id)
    }
}

impl VoiceControlSetMuteV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetMute,
            self.id,
        )
    }
}

impl VoiceControlSetDeafenV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetDeafen,
            self.id,
        )
    }
}

impl VoiceControlGetStateV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::GetState,
            self.id,
        )
    }
}

impl VoiceControlLeaveV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::Leave,
            self.id,
        )
    }
}

impl VoiceControlSetDevicesV2 {
    /// At least one side named, each under the device list's name rule. The problem never
    /// repeats a name.
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetDevices,
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

impl VoiceControlSetTalkModeV2 {
    fn check(&self) -> Result<(), String> {
        command_head(
            &self.schema,
            self.kind,
            VoiceControlCommandKind::SetTalkMode,
            self.id,
        )
    }
}

impl VoiceControlAckV2 {
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

impl VoiceControlStateV2 {
    /// `VoiceControlStateV2.Validate`, in its order: v1's rules, changed in three places
    /// and with two added (`docs/design/tui-voice.md` §20.4).
    fn check(&self) -> Result<(), String> {
        use VoiceControlBlock as Block;
        use VoiceControlPhase as Phase;
        event_head(
            &self.schema,
            self.kind,
            VoiceControlEventKind::State,
            self.seq,
        )?;
        let phase = self.phase.as_str();
        match self.phase {
            Phase::Connecting | Phase::Connected | Phase::Reconnecting => {
                if self.ptt == VoiceControlPtt::Unknown {
                    return Err(format!(
                        "voice control state in phase {phase:?} must say whether the key device is read"
                    ));
                }
                if self.playback == VoiceControlPlayback::Off {
                    return Err(format!(
                        "voice control state in phase {phase:?} must have playback on or unavailable"
                    ));
                }
            }
            Phase::Starting if self.talk_mode != VoiceControlTalkMode::PushToTalk => {
                return Err(
                    "voice control state in phase starting must have talk_mode push_to_talk".into(),
                );
            }
            _ => {}
        }
        let closed = self.phase == Phase::Starting || self.phase.is_final();
        if self.playback != VoiceControlPlayback::Off && closed {
            return Err(format!(
                "voice control state in phase {phase:?} must have playback off"
            ));
        }
        check_blocked(&self.blocked)?;
        check_reason(self.phase, self.reason, self.retry_in_ms)?;
        check_ptt_problem(self.ptt, self.ptt_problem)?;
        if self.muted != self.blocks(Block::Muted) {
            return Err("voice control state muted must agree with the muted block reason".into());
        }
        if self.deafened != self.blocks(Block::Deafened) {
            return Err(
                "voice control state deafened must agree with the deafened block reason".into(),
            );
        }
        match self.talk_mode {
            VoiceControlTalkMode::PushToTalk => {
                if (self.ptt == VoiceControlPtt::Lost) != self.blocks(Block::KeyDeviceLost) {
                    return Err(
                        "voice control state ptt lost must agree with the key_device_lost block reason"
                            .into(),
                    );
                }
            }
            VoiceControlTalkMode::Open => {
                if self.blocks(Block::KeyDeviceLost) {
                    return Err(
                        "voice control state in open mode cannot block with key_device_lost: the talk key is ignored"
                            .into(),
                    );
                }
            }
        }
        if self.phase != Phase::Connected && !self.blocks(Block::NotConnected) {
            return Err(format!(
                "voice control state in phase {phase:?} must block with not_connected"
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
        if self.capture != VoiceControlCapture::Off && closed {
            return Err(format!(
                "voice control state in phase {phase:?} must have capture off"
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
            if self.talk_mode == VoiceControlTalkMode::PushToTalk
                && self.ptt != VoiceControlPtt::Ready
            {
                return Err(
                    "voice control state cannot be transmitting in push_to_talk without ptt ready"
                        .into(),
                );
            }
        }
        Ok(())
    }
}

impl VoiceControlProtocolErrorV2 {
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
    use crate::{VoiceControlCommand, VoiceControlEvent};

    /// `pkg/schema/testdata/voice-control-v2`, read in place.
    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/testdata/voice-control-v2")
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

    const COMMANDS: [&str; 9] = [
        "command-start.json",
        "command-set-mute.json",
        "command-set-deafen.json",
        "command-get-state.json",
        "command-leave.json",
        "command-set-devices.json",
        "command-set-devices-input.json",
        "command-set-devices-default.json",
        "command-set-talk-mode.json",
    ];

    const EVENTS: [&str; 14] = [
        "event-ack.json",
        "event-ack-refused.json",
        "event-protocol-error.json",
        "event-state-starting.json",
        "event-state-connected.json",
        "event-state-transmitting.json",
        "event-state-no-speakers.json",
        "event-state-no-microphone.json",
        "event-state-open-listening.json",
        "event-state-open-transmitting.json",
        "event-state-open-muted.json",
        "event-state-open-key-device-lost.json",
        "event-state-open-max-transmit.json",
        "event-state-stopped.json",
    ];

    /// The hello, which is v1's and read by v1's reader; `control.rs` writes it.
    const HELLO: &str = "event-hello-v1-and-v2.json";

    /// Every golden command is read by the helper's v2 reader, and written again byte for
    /// byte as Go writes it.
    #[test]
    fn every_golden_command_decodes_and_encodes_as_go_writes_it() {
        for name in COMMANDS {
            let line = compact(&fixture_text(name));
            let command = parse_command_v2(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            let written = match &command {
                VoiceControlCommandV2::Start(c) => to_line(c),
                VoiceControlCommandV2::SetMute(c) => to_line(c),
                VoiceControlCommandV2::SetDeafen(c) => to_line(c),
                VoiceControlCommandV2::GetState(c) => to_line(c),
                VoiceControlCommandV2::Leave(c) => to_line(c),
                VoiceControlCommandV2::SetDevices(c) => to_line(c),
                VoiceControlCommandV2::SetTalkMode(c) => to_line(c),
            }
            .unwrap();
            assert_eq!(written, line, "{name}");
            // No v2 command is read by the v1 reader.
            assert_eq!(
                parse_command(line.as_bytes()).unwrap_err().code,
                VoiceControlErrorCode::UnknownSchema,
                "{name}"
            );
        }
    }

    /// Every golden event is written by the helper's writer byte for byte as Go writes it,
    /// and read back by the owner's v2 reader as the same event.
    #[test]
    fn every_golden_event_encodes_as_go_writes_it() {
        for name in EVENTS {
            let line = compact(&fixture_text(name));
            let event = parse_event_v2(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("{name}: {refusal}"));
            let written = match &event {
                VoiceControlEventV2::Ack(e) => to_line(e),
                VoiceControlEventV2::State(e) => to_line(e),
                VoiceControlEventV2::ProtocolError(e) => to_line(e),
                VoiceControlEventV2::Unknown { .. } => panic!("{name} is of no known kind"),
            }
            .unwrap();
            assert_eq!(written, line, "{name}");
            assert_eq!(
                serde_json::from_str::<Value>(&written).unwrap(),
                fixture(name)
            );
        }
        // The hello offering both is v1's, read by v1's reader, and refused by v2's.
        let hello = compact(&fixture_text(HELLO));
        let VoiceControlEvent::Hello(read) = parse_event(hello.as_bytes()).unwrap() else {
            panic!("{HELLO} is not a hello");
        };
        assert!(read.offers(VOICE_CONTROL_SCHEMA_V1) && read.offers(VOICE_CONTROL_SCHEMA_V2));
        assert_eq!(
            parse_event_v2(hello.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
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

    /// Every conformance case is read with the same result, code and id as Go reads it:
    /// the commands by the helper's v2 reader, the events by the owner's.
    #[test]
    fn every_conformance_case_is_read_as_go_reads_it() {
        let cases: Cases = serde_json::from_str(&fixture_text("cases.json")).unwrap();
        let (mut commands, mut events) = (0, 0);
        for case in &cases.cases {
            let line = case.line();
            let name = format!("{}: {}", case.direction, case.name);
            let (kind, refusal) = match case.direction.as_str() {
                "command" => {
                    commands += 1;
                    match parse_command_v2(&line) {
                        Ok(command) => (Some(command.kind().as_str().to_owned()), None),
                        Err(refusal) => (None, Some(refusal)),
                    }
                }
                "event" => {
                    events += 1;
                    match parse_event_v2(&line) {
                        Ok(VoiceControlEventV2::Unknown { kind, .. }) => {
                            assert_eq!(case.want.result, "skip", "{name}: skipped");
                            (Some(kind), None)
                        }
                        Ok(event) => (Some(event.kind().to_owned()), None),
                        Err(refusal) => (None, Some(refusal)),
                    }
                }
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
        assert!(commands >= 40, "only {commands} command cases");
        assert!(events >= 30, "only {events} event cases");
    }

    /// The talk mode and the playback state are the Go constants' words, in the Go order,
    /// and so is the schema name.
    #[test]
    fn the_vocabularies_and_the_name_are_the_schemas() {
        let go = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/voice_control_v2.go"),
        )
        .unwrap();
        let words_of = |go_type: &str| -> Vec<String> {
            go.lines()
                .filter_map(|line| {
                    let mut parts = line.split_whitespace();
                    let _name = parts.next()?;
                    (parts.next()? == go_type && parts.next()? == "=").then_some(())?;
                    Some(parts.next()?.trim_matches('"').to_owned())
                })
                .collect()
        };
        let ours = |words: &[&str]| -> Vec<String> {
            words.iter().map(|word| (*word).to_owned()).collect()
        };
        let modes: Vec<&str> = VoiceControlTalkMode::ALL
            .iter()
            .map(|m| m.as_str())
            .collect();
        let playback: Vec<&str> = VoiceControlPlayback::ALL
            .iter()
            .map(|p| p.as_str())
            .collect();
        assert_eq!(words_of("VoiceControlTalkMode"), ours(&modes));
        assert_eq!(words_of("VoiceControlPlayback"), ours(&playback));
        assert!(
            go.contains(&format!(
                "const VoiceControlSchemaV2 = \"{VOICE_CONTROL_SCHEMA_V2}\""
            )),
            "VoiceControlSchemaV2"
        );
        assert!(
            go.contains(&format!(
                "const VoiceControlDeviceDefault = \"{VOICE_CONTROL_DEVICE_DEFAULT}\""
            )),
            "VoiceControlDeviceDefault"
        );
    }

    /// Neither reader takes a line of the other version, in either direction: the version
    /// is fixed by the start.
    #[test]
    fn a_line_of_one_version_is_unknown_schema_to_the_other() {
        let v1_mute =
            br#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":2,"muted":true}"#;
        let v2_mute =
            br#"{"schema":"conch.voice_control.v2","kind":"set_mute","id":2,"muted":true}"#;
        assert_eq!(
            parse_command_v2(v1_mute).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
        assert!(matches!(
            parse_command(v1_mute),
            Ok(VoiceControlCommand::SetMute(_))
        ));
        assert_eq!(
            parse_command(v2_mute).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
        assert!(matches!(
            parse_command_v2(v2_mute),
            Ok(VoiceControlCommandV2::SetMute(_))
        ));
        let v1_state = compact(
            &std::fs::read_to_string(
                fixtures().join("../voice-control/event-state-connected.json"),
            )
            .unwrap(),
        );
        assert_eq!(
            parse_event_v2(v1_state.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
        let v2_state = compact(&fixture_text("event-state-connected.json"));
        assert_eq!(
            parse_event(v2_state.as_bytes()).unwrap_err().code,
            VoiceControlErrorCode::UnknownSchema
        );
    }

    fn state() -> VoiceControlStateV2 {
        serde_json::from_value(fixture("event-state-connected.json")).unwrap()
    }

    /// The writer refuses what the reader refuses: a v2 state that does not hold together,
    /// by a rule v2 changed or added, is never written.
    #[test]
    fn a_state_that_does_not_hold_together_is_not_written() {
        use VoiceControlBlock as Block;
        use VoiceControlPhase as Phase;
        let starting = || VoiceControlStateV2 {
            phase: Phase::Starting,
            capture: VoiceControlCapture::Off,
            playback: VoiceControlPlayback::Off,
            blocked: vec![Block::NotConnected],
            ptt: VoiceControlPtt::Unknown,
            ..state()
        };
        let cases: Vec<(&str, VoiceControlStateV2)> = vec![
            (
                "open mode while starting",
                VoiceControlStateV2 {
                    talk_mode: VoiceControlTalkMode::Open,
                    ..starting()
                },
            ),
            (
                "playback on while starting",
                VoiceControlStateV2 {
                    playback: VoiceControlPlayback::On,
                    ..starting()
                },
            ),
            (
                "playback off while connected",
                VoiceControlStateV2 {
                    playback: VoiceControlPlayback::Off,
                    ..state()
                },
            ),
            (
                "key_device_lost in open mode",
                VoiceControlStateV2 {
                    talk_mode: VoiceControlTalkMode::Open,
                    ptt: VoiceControlPtt::Lost,
                    ptt_problem: Some(VoiceControlPttProblem::Ended),
                    blocked: vec![Block::KeyDeviceLost],
                    ..state()
                },
            ),
            (
                "ptt lost in push_to_talk without its block",
                VoiceControlStateV2 {
                    ptt: VoiceControlPtt::Lost,
                    ptt_problem: Some(VoiceControlPttProblem::Ended),
                    ..state()
                },
            ),
            (
                "transmitting in push_to_talk without a talk key",
                VoiceControlStateV2 {
                    transmitting: true,
                    ptt: VoiceControlPtt::NotConfigured,
                    ..state()
                },
            ),
            (
                "transmitting in open mode while muted",
                VoiceControlStateV2 {
                    talk_mode: VoiceControlTalkMode::Open,
                    transmitting: true,
                    muted: true,
                    blocked: vec![Block::Muted],
                    ..state()
                },
            ),
            (
                "playback on when stopped",
                VoiceControlStateV2 {
                    phase: Phase::Stopped,
                    reason: Some(VoiceControlReason::Left),
                    capture: VoiceControlCapture::Off,
                    blocked: vec![Block::NotConnected],
                    ..state()
                },
            ),
        ];
        for (name, state) in cases {
            assert!(to_line(&state).is_err(), "{name} was written");
        }
        // What v2 allows that v1 does not: transmitting in open mode with no talk key, or
        // with the key device lost; and speakers that are not there, which block nothing.
        for allowed in [
            VoiceControlStateV2 {
                talk_mode: VoiceControlTalkMode::Open,
                transmitting: true,
                ptt: VoiceControlPtt::NotConfigured,
                ..state()
            },
            VoiceControlStateV2 {
                talk_mode: VoiceControlTalkMode::Open,
                transmitting: true,
                ptt: VoiceControlPtt::Lost,
                ptt_problem: Some(VoiceControlPttProblem::Ended),
                ..state()
            },
            VoiceControlStateV2 {
                playback: VoiceControlPlayback::Unavailable,
                transmitting: true,
                ..state()
            },
        ] {
            to_line(&allowed).unwrap_or_else(|e| panic!("{allowed:?}: {e}"));
        }
    }

    #[test]
    fn a_device_name_is_never_shown_and_never_said_in_a_refusal() {
        let line = br#"{"schema":"conch.voice_control.v2","kind":"set_devices","id":3,"input":"alsa_input.FAKE-do-not-print","output":"default"}"#;
        let VoiceControlCommandV2::SetDevices(command) = parse_command_v2(line).unwrap() else {
            panic!("not a set_devices");
        };
        let shown = format!("{command:?} {command:#?}");
        assert!(!shown.contains("FAKE"), "{shown}");
        assert!(!shown.contains("default"), "{shown}");
        assert!(shown.contains("input: Some(\"..\")"), "{shown}");

        let bad = br#"{"schema":"conch.voice_control.v2","kind":"set_devices","id":3,"input":"FAKE \"do-not-print\""}"#;
        let refusal = parse_command_v2(bad).unwrap_err();
        assert_eq!(
            (refusal.code, refusal.id),
            (VoiceControlErrorCode::Invalid, Some(3))
        );
        let shown = format!("{refusal} {refusal:?}");
        assert!(!shown.contains("FAKE"), "{shown}");
    }

    #[test]
    fn an_ack_and_a_protocol_error_are_written_in_v2() {
        assert_eq!(
            to_line(&VoiceControlAckV2::new(3, 1, None)).unwrap(),
            compact(&fixture_text("event-ack.json"))
        );
        assert_eq!(
            to_line(&VoiceControlAckV2::new(
                9,
                6,
                Some(VoiceControlErrorCode::Invalid)
            ))
            .unwrap(),
            compact(&fixture_text("event-ack-refused.json"))
        );
        assert!(
            to_line(&VoiceControlAckV2::new(
                9,
                6,
                Some(VoiceControlErrorCode::TooLong)
            ))
            .is_err()
        );
        assert_eq!(
            to_line(&VoiceControlProtocolErrorV2::new(
                8,
                VoiceControlErrorCode::UnknownSchema
            ))
            .unwrap(),
            compact(&fixture_text("event-protocol-error.json"))
        );
        assert!(
            to_line(&VoiceControlProtocolErrorV2::new(
                2,
                VoiceControlErrorCode::Stopping
            ))
            .is_err()
        );
    }
}
