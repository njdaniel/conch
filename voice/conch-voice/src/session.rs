//! The session loop: what `conch-voice join` does from start to exit.
//!
//! One loop owns every decision. It asks `conchd` for a session before each connection
//! attempt, joins the room, publishes and mutes the microphone track, feeds the
//! push-to-talk machine everything that happens, carries out what the machine says, and
//! follows the connection policy on every refusal and disconnect
//! (`docs/design/conch-voice.md` §3, §5 and §6). It is written against the traits in
//! `sdk.rs`, so all of it is tested with a fake SDK.
//!
//! What it keeps, in the order the code keeps it:
//!
//! - **Every connection attempt begins with a session from `conchd`.** There is no other
//!   source of a join token, and the token is used once, for the join it was issued for.
//! - **Publish, then mute; and "connected" begins after the mute.** The SDK enables a track
//!   whenever it publishes it, whatever its mute state (§13 row 12). So the track is muted
//!   with the statement after the publish, and the push-to-talk machine is told the
//!   connection exists only once that is done: a press that lands between the two is
//!   answered "not connected" and opens nothing.
//! - **A republish is muted and disabled before anything else.** After its own full
//!   reconnect the SDK publishes the track again. The event is handled before any other,
//!   and the SDK reports that it reconnected only after it.
//!   Should a republish ever come outside a reconnect, a press in progress is ended.
//! - **`Reconnecting` is not connected.** The gate is forced shut from that moment until
//!   `Reconnected`. If that has not come within the grace period (20 s) the loop closes the
//!   connection itself and goes back to `conchd`.
//! - **A press reports `started` before it unmutes, and a release mutes before it reports
//!   `stopped`**, as the browser client does. `conchd`'s poller compares what it sees of
//!   the track with the reports, and the other order let it see a press before its report
//!   and record an honest press as unreported (§6, "Client side").
//!   - A press queues `started` and waits for `conchd`'s answer, for at most
//!     [`REPORT_WAIT`]. Then the track is unmuted and, straight after, the gate told to
//!     open. Past the wait, or while the report is still being tried, the press goes ahead
//!     all the same: audio never depends on `conchd` being reachable.
//!   - A `started` answered 409 `voice_no_session` or 401 drops the connection instead,
//!     before the track would open, and the policy decides what follows.
//!   - A press released, or shut by a reason, while it waits never opens the track or the
//!     gate, and its transmission is over at once.
//!   - A release mutes when the gate says it has shut, release tail included, and only then
//!     does the machine produce `stopped`.
//!   - A forced shut mutes at once, without waiting for the gate; a frame the gate let
//!     through just before it is handed over before the mute or not at all
//!     (`transmit::TrackLock`).
//!
//!   So the track is unmuted for the whole of a transmission's audio and for nothing else.
//! - **No other wait for a report.** Reports are queued, in the order the machine produced
//!   them, for the task in `reports.rs`; nothing but a press's wait for its `started` is
//!   held up by one.
//! - **The machine fails closed, and so does the exit.** The machine gets a tick at its
//!   deadline. Leaving, for whatever reason, shuts the gate first, waits a bounded time for
//!   the gate to say so, and sends a final `stopped` if a press was open either way. A
//!   connection attempt that is under way is given a short time to finish, so that the
//!   room it joins is left and its track is not abandoned unmuted.
//! - **Nothing here waits for whoever reads the output.** A status line is handed to a
//!   queue with a writer thread of its own (`lines.rs`) and this loop goes on: a reader
//!   that stops reading loses lines, and the release of the key, `quit` and the exit are
//!   acted on as ever. Only the very end waits for the last lines, up to a limit.
//! - **The SDK is waited for only so long.** A join or a publish that never answers leaves
//!   the client not connected, which is safe, and would leave it so for ever: each has a
//!   limit, past which the attempt has failed and the policy decides what follows.
//! - **An error's text is scrubbed where it is put into a status line.** `conchd`'s words,
//!   or a proxy's, are shown as the `detail` of a refused session or a failed report.
//! - **A switch of device falls back to nothing** (`docs/design/tui-voice.md` §20.5,
//!   [`Input::SwitchDevices`]). A microphone switched during a transmission ends it at once,
//!   by the path every reason takes (the machine is told there is no microphone, the gate is
//!   forced shut and the track muted at once), so no frame of either microphone follows the
//!   switch, and a fresh press, or in open mode quiet and a fresh onset, is needed after it.
//!   The old stream is closed before the new one is handed over, and the new one counts as
//!   a microphone only once it has given a frame, so that one that is not there is
//!   unavailable from the switch on. Speakers are replaced in the mix and the gate is not
//!   touched. A device that cannot be opened, or is not there, is unavailable until it is:
//!   nothing else is opened, captured or played in its place. Only exactly `default` is
//!   PipeWire's default; a name `dmn-audio` would read as another (white space around it,
//!   or nothing but white space, which it takes for the default) opens nothing at all
//!   (`switched_name`). Neither an opener's error nor a name is logged.
//! - **A change of talk mode shuts the gate at once** (§20.6): the machine is told the mode
//!   ([`PttInput::TalkMode`]), cuts what was being sent with no release tail, and the track
//!   is muted at once (`mute_track`), as for a reason.
//!
//! The same loop runs a managed helper (`managed.rs`, [`serve`]). It is then given an
//! [`Owner`] to tell, and its owner's commands as inputs: once each step of the loop has
//! been applied in full the owner is sent a snapshot if anything it shows changed; a
//! command's ack follows the snapshot its effect changed; and leaving, the owner going and
//! a signal all shut the gate at once, with no release tail, before anything is said. In
//! `conch.voice_control.v2` the owner's `set_devices` is the switch above and its
//! `set_talk_mode` the change of mode, and a snapshot says `playback` and `talk_mode` too.

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use conch_voice_api::{
    Client, Error as ApiError, Secret, ServerAddress, VoiceControlErrorCode, VoiceControlPhase,
    VoiceControlPlayback, VoiceControlReason, VoiceTransmitState,
};
use conch_voice_audio::LevelConfig;
use conch_voice_control::{
    ConnectionPolicy, GateCommand, KeyEvent, LineCommand, NextStep, Outcome, Ptt, PttInput,
    PttOutput, ShutReason, TalkMode, TransmitReport,
};
use tokio::sync::{mpsc, oneshot};
use tokio::time::{Instant, MissedTickBehavior};

use crate::error::Error;
use crate::keydev::DeviceState;
use crate::managed::{
    Capture, Control, Ending, Handoff, KeyView, OWNER_LOST_STEP, Owner, PROTOCOL_ERROR,
    REPORT_LIMIT, ROOM_LIMIT, Snapshot,
};
use crate::output::{Connection, Event, Output, Stats, speaker_label};
use crate::presence::{self, PresenceTimings, Roster};
use crate::receive::{self, BoxedSink, MixCommand, MixStats, Playing};
use crate::reports::{Answer, MakeClient, ReportEvent, ReportTimings, Reports};
use crate::sdk::{DisconnectReason, MicTrack, RoomHandle, SdkError, SdkEvent, Transport};
use crate::secrets::Scrubber;
use crate::transmit::{self, BoxedMic, Transmitter, TxCommand, TxEvent};

/// Opens the microphone source with this name, a PipeWire `node.name`, or PipeWire's default
/// for `None`. Called first when a session's grant first allows publishing, and then once
/// for each switch to another microphone: a client that may only listen never opens a
/// microphone. `Ok(None)` means there is none to open.
pub type OpenMic = Box<dyn FnMut(Option<&str>) -> Result<Option<BoxedMic>, Error> + Send>;

/// Opens the speakers with this name, or PipeWire's default for `None`. Called first when
/// the session starts, so that the client hears the channel from the moment it is joined,
/// and then once for each switch to other speakers.
pub type OpenSink = Box<dyn FnMut(Option<&str>) -> Result<BoxedSink, Error> + Send>;

/// How a device is named for PipeWire's default, in the configuration and in a switch.
const DEFAULT_DEVICE: &str = "default";

/// What a managed helper says of a microphone that could not be opened. No name, and none
/// of the opener's words, which may hold one.
const MICROPHONE_NOT_OPENED: &str = "the microphone named could not be opened";

/// What is logged of speakers a switch named that could not be opened, or that cannot be
/// opened by their name. No name, and none of the opener's words.
pub const SPEAKERS_NOT_OPENED: &str = "the speakers named could not be opened: nothing is heard until other speakers are chosen, and no others are used in their place";

/// A name from the configuration (`audio.input`, `audio.output`) as the openers take it:
/// `None` for PipeWire's default, which no name, or exactly `default`, asks for; any other
/// name as it is written, as before switches existed.
pub(crate) fn configured_name(name: Option<&str>) -> Option<String> {
    name.filter(|name| *name != DEFAULT_DEVICE)
        .map(str::to_owned)
}

/// A name from a switch (an owner's `set_devices`) as the openers take it: `Some(None)` for
/// exactly `default`, `Some(Some(name))` for any other name that is opened as it is written,
/// and `None` for a name that cannot be: `dmn-audio` takes the white space off around a name
/// and reads an empty one as the default, so a name with white space around it, or nothing
/// but white space, would open another device than the one named, the default among them.
/// Nothing is opened for one (`docs/design/tui-voice.md` §20.5: no fallback).
pub(crate) fn switched_name(name: &str) -> Option<Option<String>> {
    if name == DEFAULT_DEVICE {
        Some(None)
    } else if name.is_empty() || name.trim() != name {
        None
    } else {
        Some(Some(name.to_owned()))
    }
}

/// A microphone or speakers, as the session targets them.
#[derive(Clone, PartialEq, Eq)]
enum Device {
    /// PipeWire's default.
    Default,
    /// The device of this `node.name`, as it is written.
    Named(String),
    /// A name from a switch that cannot be opened as it is written ([`switched_name`]):
    /// nothing is opened, and the side is unavailable. Kept, so that the same name again
    /// changes nothing.
    Unusable(String),
}

impl Device {
    /// A device the configuration names. The configuration refuses a name that is empty or
    /// has white space around it (`Config::validate`), so one reaching here was built by
    /// hand; it is unusable, as a switched one is, and never the default in its place.
    fn configured(name: Option<&str>) -> Self {
        match configured_name(name) {
            None => Device::Default,
            Some(name) if switched_name(&name).is_none() => Device::Unusable(name),
            Some(name) => Device::Named(name),
        }
    }

    /// A device a switch names.
    fn switched(name: &str) -> Self {
        match switched_name(name) {
            Some(None) => Device::Default,
            Some(Some(name)) => Device::Named(name),
            None => Device::Unusable(name.to_owned()),
        }
    }

    /// The name the openers are given: `Some(None)` for the default, and `None` when
    /// nothing is to be opened.
    fn opened_as(&self) -> Option<Option<&str>> {
        match self {
            Device::Default => Some(None),
            Device::Named(name) => Some(Some(name)),
            Device::Unusable(_) => None,
        }
    }
}

/// The microphone and the speakers to use from now on (`docs/design/tui-voice.md` §20.5),
/// as a managed owner's `set_devices` asks. A side that is `None` is left alone; a name is
/// a PipeWire `node.name`, or `default` for PipeWire's default.
///
/// `Debug` shows which sides are there and no name: the helper names no device in what it
/// writes.
#[derive(Clone, PartialEq, Eq)]
pub struct DeviceSwitch {
    /// The microphone.
    pub input: Option<String>,
    /// The speakers.
    pub output: Option<String>,
}

impl std::fmt::Debug for DeviceSwitch {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let side = |name: &Option<String>| name.as_ref().map(|_| "..");
        f.debug_struct("DeviceSwitch")
            .field("input", &side(&self.input))
            .field("output", &side(&self.output))
            .finish()
    }
}

/// Where the speakers are, as a managed owner is told it in `playback`
/// (`docs/design/tui-voice.md` §20.4).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Playback {
    /// Their stream is open with a device, and playing.
    On,
    /// The speakers named are not there, went away, or could not be opened. Nothing else
    /// plays in their place; they play once they are there.
    Unavailable,
    /// No stream is open: the session has closed the speakers.
    Off,
}

/// Something from outside the loop: a line on standard input, its end, a signal, or what
/// the key device's watcher (`keydev.rs`) saw.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Input {
    /// One of the five commands.
    Line(LineCommand),
    /// The talk, mute or deafen key went down or came up on the key device.
    Key(KeyEvent),
    /// The key device is being read, or is not, and why. A device that is not there ends
    /// whatever was held on it.
    KeyDevice(DeviceState),
    /// A line that is not a command.
    UnknownLine,
    /// Standard input ended. It counts as `up` and then `quit`: a wrapper that dies after
    /// `down` must not leave the gate open.
    Eof,
    /// SIGINT or SIGTERM. It counts as `quit`; for a managed helper, as `leave`.
    Signal,
    /// A command of a managed helper's owner, read and checked (`managed.rs`).
    Control(Control),
    /// A managed helper's owner is gone: its control pipe ended, or it is no longer the
    /// helper's parent (`managed.rs`).
    OwnerLost,
    /// Use this microphone and these speakers from now on, as a managed owner's
    /// `set_devices` does (which reaches the loop as a [`Control`]). Nothing on standard
    /// input or the key device sends it.
    SwitchDevices(DeviceSwitch),
}

/// Every wait and limit of the loop. The defaults are the design's; tests shorten them.
#[derive(Debug, Clone)]
pub struct Timings {
    /// How long the SDK's own reconnect is allowed before the loop closes the connection
    /// and goes back to `conchd` (the design note's decision 3).
    pub reconnect_grace: Duration,
    /// How long joining a room may take once `conchd` has issued the session. An SDK that
    /// never answers would otherwise leave the client not connected for ever; past this
    /// the attempt has failed, and the policy decides what follows.
    pub connect_limit: Duration,
    /// How long publishing the microphone track may take once the room is joined. Past
    /// this the room is left, as for a publish that failed, and the attempt has failed.
    pub publish_limit: Duration,
    /// How long leaving a room may take. Closing during a reconnect was measured at 5 s.
    pub close_limit: Duration,
    /// How long a connection attempt that is in progress when the client leaves is given to
    /// finish, so that the room it joins is left and not abandoned.
    pub attempt_limit: Duration,
    /// How long, beyond the release tail, the gate is given to say it has shut when the
    /// client is leaving.
    pub gate_shut_grace: Duration,
    /// How long a press waits for `conchd`'s answer to its `started` report before the track
    /// opens all the same. [`REPORT_WAIT`] outside tests.
    pub report_wait: Duration,
    /// How long the last reports are given to be delivered when the client is leaving.
    pub report_flush: Duration,
    /// How long the last lines of output are given to be written when the client is
    /// leaving. A reader that is reading gets them; one that is not does not hold the
    /// exit up for longer than this.
    pub output_flush: Duration,
    /// How often `stats` is written while connected.
    pub stats_every: Duration,
    /// Every wait the connection policy asks for is divided by this. 1 outside tests.
    pub wait_divisor: u32,
    /// The report queue's retries.
    pub reports: ReportTimings,
    /// The presence socket's retries.
    pub presence: PresenceTimings,
    /// A random number for the policy's jitter.
    pub jitter: fn() -> u32,
}

impl Default for Timings {
    fn default() -> Self {
        Self {
            reconnect_grace: Duration::from_secs(20),
            connect_limit: Duration::from_secs(30),
            publish_limit: Duration::from_secs(30),
            close_limit: Duration::from_secs(10),
            attempt_limit: Duration::from_secs(2),
            gate_shut_grace: Duration::from_millis(500),
            report_wait: REPORT_WAIT,
            report_flush: Duration::from_secs(3),
            output_flush: Duration::from_secs(1),
            stats_every: Duration::from_secs(1),
            wait_divisor: 1,
            reports: ReportTimings::default(),
            presence: PresenceTimings::default(),
            jitter: random,
        }
    }
}

/// A random number, from the seed the standard library draws for each hash map. Good
/// enough to spread reconnecting clients apart, which is all it is used for.
fn random() -> u32 {
    use std::hash::{BuildHasher, Hasher};
    let hasher = std::collections::hash_map::RandomState::new().build_hasher();
    (hasher.finish() >> 32) as u32
}

/// What `join` was asked to do, after the command line and the configuration.
pub struct Settings {
    /// The channel to join.
    pub channel: String,
    /// How the microphone is opened.
    pub open_mic: OpenMic,
    /// How the speakers are opened.
    pub open_sink: OpenSink,
    /// `audio.input`: the microphone to open, a `node.name`; `None` or `default` for
    /// PipeWire's default.
    pub input: Option<String>,
    /// `audio.output`: the speakers to open, as `input` is named.
    pub output: Option<String>,
    /// The tones the mix is measured for in the reports.
    pub sink_tones: Vec<f32>,
    /// `audio.release_tail_ms`.
    pub release_tail_ms: u32,
    /// `audio.max_transmit_secs`.
    pub max_transmit: Duration,
    /// What opens the gate: `audio.talk_mode` for `join`. A managed helper speaking
    /// `conch.voice_control.v1` is given push-to-talk whatever the file says; one speaking
    /// v2, the file's mode, or the one its owner set while it was starting.
    pub talk_mode: TalkMode,
    /// `audio.open_threshold_db` and `audio.open_hang_ms`, for the level detector.
    pub level: LevelConfig,
    /// The key device being watched, as it is named when its state is shown. `None` when
    /// presses come from standard input only.
    pub key_device: Option<String>,
    /// Added, in brackets, to what is shown when no connection to `conchd` could be made:
    /// the stored logins, when the server is the default and they are for others
    /// (`cli::Resolved::unreachable_note`).
    pub unreachable_note: Option<String>,
    /// Waits and limits.
    pub timings: Timings,
}

/// The most remote tracks that are mixed at once. A room of people each has one; the bound
/// is what stops a room that announces thousands from costing a buffer and a task each.
pub const MAX_TRACKS: usize = 64;
/// The most tracks of one speaker that are mixed at once: the one they talk on, and one
/// more for the moment in which a track is replaced. Without it one participant who
/// publishes many tracks would use up [`MAX_TRACKS`] and nobody else would be heard.
pub const MAX_TRACKS_PER_SPEAKER: usize = 2;

/// What `join` says once when it starts in open mode: echo cancellation does not work yet
/// (#186, #566), so on speakers the open microphone sends the others' voices back to them.
pub const OPEN_MIC_WARNING: &str = "open microphone: what the microphone hears is sent when it is loud enough; use headphones (no echo cancellation yet)";

/// How long a session request or the opening of the presence socket may take.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
/// How long one attempt at a transmit report may take. Reports queue behind one another,
/// so this is short: `conchd` is normally a few milliseconds away.
pub const REPORT_TIMEOUT: Duration = Duration::from_secs(1);
/// The longest a press waits for `conchd`'s answer to its `started` report before the track
/// opens anyway: the browser client's bound too (`reportWait` in its `connection.ts`).
pub const REPORT_WAIT: Duration = Duration::from_secs(1);

/// The ways this client reaches `conchd`.
pub struct Conchd {
    /// For session requests and the presence socket.
    pub client: Client,
    /// For the report queue, which makes itself a new client whenever it gives up on a
    /// report.
    pub report_client: MakeClient,
}

impl Conchd {
    /// Clients of `server`, signed in with `token`.
    ///
    /// # Errors
    ///
    /// As [`Client::new`]: an address or a token that cannot be used.
    pub fn new(
        server: &ServerAddress,
        token: &Secret,
        request_timeout: Duration,
        report_timeout: Duration,
    ) -> Result<Self, ApiError> {
        let client = Client::new(server, token, request_timeout)?;
        // Made once here so that a token or address the report queue could not use is an
        // error now, not at the first press.
        Client::new(server, token, report_timeout)?;
        let (server, token) = (server.clone(), token.clone());
        Ok(Self {
            client,
            report_client: Box::new(move || Client::new(&server, &token, report_timeout)),
        })
    }
}

/// What the connection policy is told when `conchd` refuses a session or cannot be asked,
/// with the name `--json` gives it. `None` for an error no policy row covers and no retry
/// can change: the client ends with the error itself.
#[must_use]
pub fn session_outcome(error: &ApiError) -> Option<(Outcome, &'static str)> {
    Some(match error {
        ApiError::Unauthenticated(_) => (Outcome::NotSignedIn, "not_signed_in"),
        ApiError::VoiceRequiresAuth(_) => (Outcome::VoiceRequiresAuth, "voice_requires_auth"),
        ApiError::Forbidden(_) => (Outcome::NotAPerson, "not_a_person"),
        ApiError::ChannelNotFound(_) => (Outcome::NotAMember, "not_a_member"),
        ApiError::NotConfigured(_) => (Outcome::VoiceNotConfigured, "voice_not_configured"),
        ApiError::Unavailable(_) => (Outcome::VoiceUnavailable, "voice_unavailable"),
        ApiError::Timeout | ApiError::Connect { .. } | ApiError::Transport { .. } => {
            (Outcome::ServerUnreachable, "server_unreachable")
        }
        // Not an answer of conchd's, or conchd in trouble: a proxy in the way, a restart.
        // It may pass, so it is waited out like a server that cannot be reached.
        ApiError::UnexpectedResponse { .. }
        | ApiError::Undecodable { .. }
        | ApiError::Invalid { .. } => (Outcome::ServerUnreachable, "server_unreachable"),
        ApiError::Refused(refusal) if refusal.status >= 500 => {
            (Outcome::ServerUnreachable, "server_unreachable")
        }
        _ => return None,
    })
}

/// What the connection policy is told when the SDK reports a disconnect, with the name
/// `--json` gives it (the second table of the design note's §5).
#[must_use]
pub fn disconnect_outcome(reason: DisconnectReason) -> (Outcome, &'static str) {
    match reason {
        DisconnectReason::RoomDeleted => (Outcome::RoomDeleted, "room_deleted"),
        DisconnectReason::ParticipantRemoved => {
            (Outcome::ParticipantRemoved, "participant_removed")
        }
        DisconnectReason::DuplicateIdentity => (Outcome::DuplicateIdentity, "duplicate_identity"),
        DisconnectReason::ServerShutdown => (Outcome::ServerShutdown, "server_shutdown"),
        DisconnectReason::Lost => (Outcome::ConnectionLost, "connection_lost"),
    }
}

type Track<T> = <<T as Transport>::Room as RoomHandle>::Track;
type Feed<T> = <<T as Transport>::Room as RoomHandle>::Feed;
type Boxed<O> = Pin<Box<dyn Future<Output = O> + Send>>;

/// A room that was joined, with its track published and muted if it has one.
struct Established<T: Transport> {
    room: T::Room,
    events: mpsc::UnboundedReceiver<SdkEvent>,
    mic: Option<(Track<T>, Feed<T>)>,
    can_publish: bool,
    identity: String,
}

/// Why there is no room.
enum Failure {
    Session(ApiError),
    NoChannelGrant,
    Sdk(SdkError),
}

/// How long each step of a connection attempt that is the SDK's may take.
#[derive(Clone, Copy)]
struct AttemptLimits {
    connect: Duration,
    publish: Duration,
    close: Duration,
}

/// What an attempt fails with when the SDK did not answer within a limit. The text is this
/// program's own.
fn took_too_long(what: &str, limit: Duration) -> Failure {
    Failure::Sdk(SdkError::scrubbed(format!(
        "{what} took longer than {} s",
        limit.as_secs_f32()
    )))
}

/// How much longer than the sum of its own limits the SDK's part of an attempt is waited
/// for from outside, before the thread it runs on is taken to be stuck.
const STUCK_AFTER: Duration = Duration::from_secs(1);

/// One connection attempt, from the session request to a muted track.
///
/// The SDK's part of it runs on a thread of its own, not on the thread of whoever polls
/// this. The SDK's join runs native code, and on a machine with an NVIDIA GPU that code
/// prints a line to standard output where it stands (`docs/design/conch-voice.md` §5). With
/// a standard output nobody reads, that print does not return. Polled from the session
/// loop it would stop the loop there, and with it the release of the key, `quit` and every
/// signal: seen with the real binary and a full pipe. On its own thread it stops only
/// itself: the attempt is given up on from here after its limits, the loop goes on, and the
/// thread is let go whenever the print returns.
async fn establish<T: Transport>(
    client: Client,
    transport: Arc<T>,
    channel: String,
    scrubber: Arc<Scrubber>,
    limits: AttemptLimits,
) -> Result<Established<T>, Failure> {
    // A new session, immediately before the attempt it is for.
    let session = client.session(&channel).await.map_err(Failure::Session)?;
    // The room of the grant with no audience. Any other grant is ignored: nets are V5.
    let grant = session.channel_grant().ok_or(Failure::NoChannelGrant)?;
    // From here on, whatever the SDK writes is searched for these two values. They are not
    // forgotten when the connection ends: the SDK's tasks for a room go on, and may log,
    // after the room was left.
    scrubber.connection(&grant.token, &grant.room);
    // `session` is not used past this function. The join token goes to the SDK for this
    // one join; this program's only other copy is the scrubber's, which replaces it in
    // text and does nothing else with it.
    let joining = join_room(
        transport,
        session.livekit_url.clone(),
        grant.token.clone(),
        grant.can_publish,
        session.identity.clone(),
        limits,
    );
    let runtime = tokio::runtime::Handle::current();
    let (claim, claimed) = tokio::sync::oneshot::channel();
    let on_its_own_thread = move || {
        runtime.block_on(async move {
            // If nobody is waiting for the room any more (the attempt was given up on, or
            // the client is leaving), it is left here: nothing else would ever close it.
            if let Err(Ok(unclaimed)) = claim.send(joining.await) {
                let _ = tokio::time::timeout(limits.close, unclaimed.room.close()).await;
            }
        });
    };
    // A thread of the attempt's own, and not one of the runtime's: one that gets stuck is
    // then nobody else's loss, and nothing waits for it when the process ends.
    std::thread::Builder::new()
        .name("conch-voice-join".to_owned())
        .spawn(on_its_own_thread)
        .map_err(|_| {
            Failure::Sdk(SdkError::scrubbed(
                "no thread could be started for the join",
            ))
        })?;
    let stuck_after = limits.connect + limits.publish + limits.close + STUCK_AFTER;
    match tokio::time::timeout(stuck_after, claimed).await {
        Ok(Ok(joined)) => joined,
        Ok(Err(_)) => Err(Failure::Sdk(SdkError::scrubbed(
            "the join ended without an answer",
        ))),
        Err(_) => Err(took_too_long("joining the voice room", stuck_after)),
    }
}

/// The SDK's part of a connection attempt: the join, and for a grant that allows it the
/// publish and, straight after it, the mute. Each step has a limit. A client whose join
/// never answers has failed closed (it is not connected, so the gate is shut), but it must
/// not wait for ever: past the limit the attempt has failed, and the policy tries again.
async fn join_room<T: Transport>(
    transport: Arc<T>,
    livekit_url: String,
    token: Secret,
    can_publish: bool,
    identity: String,
    limits: AttemptLimits,
) -> Result<Established<T>, Failure> {
    let joining = transport.connect(&livekit_url, &token);
    let (room, events) = match tokio::time::timeout(limits.connect, joining).await {
        Ok(joined) => joined.map_err(Failure::Sdk)?,
        Err(_) => return Err(took_too_long("joining the voice room", limits.connect)),
    };
    if !can_publish {
        return Ok(Established {
            room,
            events,
            mic: None,
            can_publish,
            identity,
        });
    }
    let published = tokio::time::timeout(limits.publish, room.publish_microphone()).await;
    let failure = match published {
        Ok(Ok((track, feed))) => {
            // Straight after the publish, which left the track enabled.
            track.mute();
            return Ok(Established {
                room,
                events,
                mic: Some((track, feed)),
                can_publish,
                identity,
            });
        }
        Ok(Err(error)) => Failure::Sdk(error),
        Err(_) => took_too_long("publishing the microphone", limits.publish),
    };
    // The room was joined and has no track this client holds and could mute. It is left
    // before anything else: the next attempt joins as the same identity, and nothing else
    // would ever close this one.
    let _ = tokio::time::timeout(limits.close, room.close()).await;
    Err(failure)
}

/// A connection in use.
struct Live<T: Transport> {
    room: T::Room,
    events: mpsc::UnboundedReceiver<SdkEvent>,
    /// `None` when the grant does not allow publishing.
    track: Option<Track<T>>,
    /// This client's own identity in the room.
    identity: String,
    /// Subscribed tracks, by id, with the speaker each belongs to.
    tracks: BTreeMap<String, String>,
    /// When the SDK said `Reconnecting`, until it says `Reconnected`.
    reconnecting_since: Option<Instant>,
    /// Whether a track beyond the bounds was ignored on this connection, which is said
    /// once.
    ignored_a_track: bool,
}

/// Why a connection ended or was not made, for the policy and for the status.
#[derive(Clone)]
struct Ended {
    outcome: Outcome,
    reason: &'static str,
    detail: Option<String>,
}

/// Where the loop is with the connection.
enum Phase<T: Transport> {
    /// Ask `conchd` for a session at this time, or at once.
    Ask(Option<Instant>),
    /// A connection attempt is in progress.
    Establishing(Boxed<Result<Established<T>, Failure>>),
    /// Nothing is in progress: the client is connected (`Session::live` holds the
    /// connection), or the loop is about to decide what follows, or to end.
    Settled,
    /// A connection that ended is being left; then the policy decides.
    Closing(Boxed<()>, Ended),
}

/// What became of the phase.
enum Progress<T: Transport> {
    Ask,
    Established(Result<Established<T>, Failure>),
    Closed(Ended),
}

/// Waits for the current phase to need attention. Dropping this future loses nothing: the
/// work it waits on stays in the phase.
async fn progress<T: Transport>(phase: &mut Phase<T>) -> Progress<T> {
    match phase {
        Phase::Ask(None) => Progress::Ask,
        Phase::Ask(Some(at)) => {
            tokio::time::sleep_until(*at).await;
            Progress::Ask
        }
        Phase::Establishing(attempt) => Progress::Established(attempt.as_mut().await),
        Phase::Settled => std::future::pending().await,
        Phase::Closing(closing, ended) => {
            closing.as_mut().await;
            Progress::Closed(ended.clone())
        }
    }
}

/// The next event of the live connection; never, if there is none. `None` means the SDK
/// stopped reporting without saying that the connection ended.
async fn sdk_event<T: Transport>(live: &mut Option<Live<T>>) -> Option<SdkEvent> {
    match live {
        Some(live) => live.events.recv().await,
        None => std::future::pending().await,
    }
}

/// A press whose `started` report is out: the track opens once `conchd` has answered it,
/// or at `until`, whichever comes first.
struct Opening {
    /// The answer, once the report is queued.
    answer: Option<oneshot::Receiver<Answer>>,
    until: Instant,
}

/// What became of the report a press waits for: `None` if the wait ran out first, or the
/// report queue went away without an answer. Never, if no press waits.
async fn answered(opening: &mut Option<Opening>) -> Option<Answer> {
    let Some(opening) = opening else {
        return std::future::pending().await;
    };
    let answer = async {
        match opening.answer.as_mut() {
            Some(answer) => answer.await.ok(),
            None => std::future::pending().await,
        }
    };
    tokio::select! {
        answer = answer => answer,
        () = tokio::time::sleep_until(opening.until) => None,
    }
}

async fn sleep_until(at: Option<Instant>) {
    match at {
        Some(at) => tokio::time::sleep_until(at).await,
        None => std::future::pending().await,
    }
}

struct Session<T: Transport> {
    channel: String,
    timings: Timings,
    release_tail: Duration,
    open_mic: OpenMic,
    open_sink: OpenSink,
    /// The microphone in use, or to open when a session first allows publishing.
    input: Device,
    /// The speakers in use.
    output: Device,
    client: Client,
    transport: Arc<T>,
    scrubber: Arc<Scrubber>,
    out: Output,
    key_device: Option<String>,
    unreachable_note: Option<String>,
    inputs: mpsc::UnboundedReceiver<Input>,
    inputs_open: bool,
    /// The origin of the times the machine and the policy are given.
    start: Instant,
    ptt: Ptt,
    policy: ConnectionPolicy,
    transmitter: Transmitter<Feed<T>>,
    receiver: receive::Receiver,
    reports: Reports,
    /// A press that waits for its `started` to be answered before the track opens.
    opening: Option<Opening>,
    rosters: mpsc::UnboundedReceiver<Roster>,
    phase: Phase<T>,
    live: Option<Live<T>>,
    shown: Option<Connection>,
    frames_sent_shown: u64,
    /// Set when the loop is to end, with what `join` then returns.
    exit: Option<Result<(), Error>>,
    /// A managed helper's owner, told the state and answered (`managed.rs`). `None` for
    /// `join`, and nothing below it is used then.
    owner: Option<Owner>,
    /// Why a managed helper is ending, once it is: the reason its last states give.
    ending: Option<Ending>,
    /// What the key device's watcher last said, for the owner's `ptt`.
    key_state: Option<DeviceState>,
    /// Whether a talk key is bound on the key device, for the owner's `ptt`.
    talk_key: bool,
    /// The microphone, for the owner's `capture`.
    mic: Mic,
    /// The speakers, for the owner's `playback`.
    playback: Playback,
}

/// Where the microphone is, as a managed helper's owner is told it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Mic {
    /// Not opened: no session has allowed publishing yet.
    NotOpened,
    /// Its stream is open.
    Open,
    /// It could not be opened, or there is none to open.
    Unavailable,
    /// Its stream was closed, as the session ended.
    Closed,
}

/// Runs `conch-voice join` until `quit`, the end of input, a signal, or a stop.
///
/// # Errors
///
/// [`Error::Stopped`] when the connection policy says to stop, with its message and exit
/// code; another [`Error`] for a setting that was refused or an answer of `conchd` that no
/// retry can change.
pub async fn join<T: Transport>(
    settings: Settings,
    conchd: Conchd,
    transport: T,
    scrubber: Arc<Scrubber>,
    inputs: mpsc::UnboundedReceiver<Input>,
    out: Output,
) -> Result<(), Error> {
    let mut out = out;
    // Said once, first: with speakers, what the others say is sent back to them. With
    // `--json` no object carries it, and it goes to the log instead.
    if settings.talk_mode == TalkMode::Open && !out.note(OPEN_MIC_WARNING) {
        log::warn!("{OPEN_MIC_WARNING}");
    }
    let mut session = Session::new(settings, conchd, transport, scrubber, inputs, out)?;
    session.run().await;
    session.leave().await
}

/// Runs a managed helper's session (`managed.rs`) from the moment its identity, its
/// configuration, its audio and its key device have been set up, until it has left: for
/// leave, the owner going, a signal, or a stop. The owner is told every state and answered
/// every command, and its last state is the final one. Nothing is shown on standard output.
///
/// # Errors
///
/// As [`join`]: what the owner was told in a failed state. An error before the loop began
/// (the speakers could not be opened) is told as a failed state too.
pub async fn serve<T: Transport>(
    settings: Settings,
    conchd: Conchd,
    transport: T,
    scrubber: Arc<Scrubber>,
    inputs: mpsc::UnboundedReceiver<Input>,
    handoff: Handoff,
) -> Result<(), Error> {
    // Standard output carries a managed helper's native noise, which its owner discards;
    // nothing of this program's goes there.
    let out = Output::new(false, Box::new(std::io::sink()));
    let mut session = match Session::new(settings, conchd, transport, scrubber, inputs, out) {
        Ok(session) => session,
        Err(error) => {
            handoff.fail(&error).await;
            return Err(error);
        }
    };
    let Handoff {
        owner,
        muted,
        deafened,
        key_state,
        talk_key,
        switch,
    } = handoff;
    session.owner = Some(owner);
    session.key_state = key_state;
    session.talk_key = talk_key;
    // The machine as the start, and keys pressed while starting, left it.
    if muted {
        session.tell(PttInput::Line(LineCommand::Mute));
    }
    if deafened {
        session.tell(PttInput::Line(LineCommand::Deafen));
    }
    match key_state {
        Some(DeviceState::Ready) => session.tell(PttInput::KeyDevice(true)),
        Some(DeviceState::Lost(_)) => {
            session.tell(PttInput::KeyDevice(true));
            session.tell(PttInput::KeyDevice(false));
        }
        Some(DeviceState::Missing(_)) | None => {}
    }
    // The devices the owner named while starting, as a switch: the microphone is not open
    // yet, so its name is kept for when it is; speakers other than those opened are opened
    // in their place.
    session.switch_devices(switch);
    // The first attempt now, so that the first state the owner is told is the connecting
    // one, with the key device already tried.
    session.on_progress(Progress::Ask);
    session.settle(false);
    session.run().await;
    session.leave_managed().await
}

impl<T: Transport> Session<T> {
    /// Starts the tasks a session needs and gives the machine its first words. The
    /// speakers are opened here.
    fn new(
        settings: Settings,
        conchd: Conchd,
        transport: T,
        scrubber: Arc<Scrubber>,
        inputs: mpsc::UnboundedReceiver<Input>,
        out: Output,
    ) -> Result<Self, Error> {
        let Settings {
            channel,
            open_mic,
            mut open_sink,
            input,
            output,
            sink_tones,
            release_tail_ms,
            max_transmit,
            talk_mode,
            level,
            key_device,
            unreachable_note,
            timings,
        } = settings;
        let transmitter = transmit::spawn_with_level(
            transmit::gate_config(release_tail_ms, max_transmit),
            level,
        )?;
        let output = Device::configured(output.as_deref());
        let speakers = match output.opened_as() {
            Some(name) => open_sink(name)?,
            None => Box::new(receive::NoSpeakers),
        };
        let playback = if speakers.playing() {
            Playback::On
        } else {
            Playback::Unavailable
        };
        let receiver = receive::spawn(&sink_tones, speakers)?;
        let reports = Reports::spawn(
            conchd.report_client,
            channel.clone(),
            timings.reports.clone(),
        );
        let rosters = presence::spawn(conchd.client.clone(), channel.clone(), timings.presence);
        let (ptt, first) = Ptt::with_talk_mode(max_transmit, talk_mode);
        let mut out = out;
        out.set_talk_mode(talk_mode);
        let mut session = Session {
            channel,
            release_tail: Duration::from_millis(u64::from(release_tail_ms)),
            timings,
            open_mic,
            open_sink,
            input: Device::configured(input.as_deref()),
            output,
            client: conchd.client,
            transport: Arc::new(transport),
            scrubber,
            out,
            key_device,
            unreachable_note,
            inputs,
            inputs_open: true,
            start: Instant::now(),
            ptt,
            policy: ConnectionPolicy::new(),
            transmitter,
            receiver,
            reports,
            opening: None,
            rosters,
            phase: Phase::Ask(None),
            live: None,
            shown: None,
            frames_sent_shown: 0,
            exit: None,
            owner: None,
            ending: None,
            key_state: None,
            talk_key: true,
            mic: Mic::NotOpened,
            playback,
        };
        // The machine's first words: the gate is told why it is shut, and the first status.
        session.obey(first);
        Ok(session)
    }

    fn now(&self) -> Duration {
        self.start.elapsed()
    }

    async fn run(&mut self) {
        let mut stats = tokio::time::interval_at(
            Instant::now() + self.timings.stats_every,
            self.timings.stats_every,
        );
        stats.set_missed_tick_behavior(MissedTickBehavior::Skip);
        while self.exit.is_none() {
            let tick_at = self.ptt.deadline().map(|at| self.start + at);
            let grace_ends = self
                .live
                .as_ref()
                .and_then(|live| live.reconnecting_since)
                .map(|since| since + self.timings.reconnect_grace);
            tokio::select! {
                // What the SDK says about the connection comes first, so a republished
                // track is muted before anything else is done; then what the gate says.
                biased;
                event = sdk_event(&mut self.live) => self.on_sdk(event),
                Some(event) = self.transmitter.events.recv() => self.on_transmit(event),
                progress = progress(&mut self.phase) => self.on_progress(progress),
                input = self.inputs.recv(), if self.inputs_open => self.on_input(input),
                // After the inputs: a release that is already waiting ends the press before
                // its track would open.
                answer = answered(&mut self.opening) => self.on_answer(answer),
                // The machine's own deadline: a press that has reached the transmit limit.
                () = sleep_until(tick_at) => self.tell(PttInput::Tick),
                () = sleep_until(grace_ends) => self.disconnected(Ended {
                    outcome: Outcome::ConnectionLost,
                    reason: "reconnect_timed_out",
                    detail: None,
                }),
                Some(roster) = self.rosters.recv() => self.out.show(&Event::Presence(&roster)),
                Some(report) = self.reports.events.recv() => self.on_report(&report),
                Some(received) = self.receiver.stats.recv() => self.on_stats(received),
                Some(said) = self.receiver.playing.recv() => self.on_playing(said),
                // Asked for whether connected or not, so that each answer covers one
                // interval; it is shown only while connected.
                _ = stats.tick(), if self.out.is_json() => {
                    let _ = self.receiver.commands.send(MixCommand::Stats);
                }
            }
            // The step has been applied in full: a managed owner is told what changed.
            self.settle(false);
        }
    }

    /// Gives the push-to-talk machine one input and carries out what it says.
    fn tell(&mut self, input: PttInput) {
        let outputs = self.ptt.handle(self.now(), input);
        self.obey(outputs);
    }

    fn obey(&mut self, outputs: Vec<PttOutput>) {
        // A press that ended while it waited for its answer, and so never opened the gate.
        let mut never_opened = false;
        for output in outputs {
            match output {
                PttOutput::Gate(GateCommand::Open) => {
                    // Not yet: the gate opens once `conchd` has answered the `started` that
                    // the machine produces with this, or once the wait for that is over.
                    self.opening = Some(Opening {
                        answer: None,
                        until: Instant::now() + self.timings.report_wait,
                    });
                }
                PttOutput::Gate(command) => {
                    // What shuts the gate at once, with no release tail: a reason, and a
                    // change of talk mode.
                    let at_once = matches!(
                        command,
                        GateCommand::ForceShut(_) | GateCommand::TalkMode(_)
                    );
                    let ends = at_once || command == GateCommand::Shut;
                    let waited = ends && self.opening.take().is_some();
                    never_opened |= waited;
                    // The gate never opened for a press that waited, so a release has
                    // nothing to shut; a reason, or the mode, is passed on all the same, so
                    // that the gate holds the same reasons as the machine and runs the
                    // level detector in the mode the machine is in.
                    if !(waited && command == GateCommand::Shut) {
                        self.transmitter.send(TxCommand::Gate(command));
                    }
                    if at_once {
                        // Nothing is left to send.
                        self.mute_track();
                    }
                }
                PttOutput::Report(TransmitReport::Started) => match self.opening.as_mut() {
                    Some(opening) => {
                        let (answered, answer) = oneshot::channel();
                        self.reports
                            .push_answered(VoiceTransmitState::Started, answered);
                        opening.answer = Some(answer);
                    }
                    None => self.reports.push(VoiceTransmitState::Started),
                },
                PttOutput::Report(TransmitReport::Stopped) => {
                    self.reports.push(VoiceTransmitState::Stopped);
                }
                PttOutput::PressIgnored(reason) => self.out.show(&Event::PressIgnored(reason)),
                PttOutput::Status(status) => {
                    self.out.show(&Event::Own(status));
                    let _ = self
                        .receiver
                        .commands
                        .send(MixCommand::Deafen(status.deafened()));
                }
                PttOutput::Quit => {
                    self.exit.get_or_insert(Ok(()));
                }
            }
        }
        if never_opened {
            // The gate will never say it shut, since it never opened: the press is over now,
            // and is ended as one the gate shut, mute first and `stopped` after. Told once
            // the machine's outputs above are carried out, so they are shown in order.
            self.on_transmit(TxEvent::GateShut {
                limit_reached: false,
            });
        }
    }

    /// `conchd` answered the `started` a press waits for, or the wait is over.
    fn on_answer(&mut self, answer: Option<Answer>) {
        let refused = match answer {
            Some(Answer::NoSession) => Ended {
                // As for a room that was deleted: `conchd` decides what follows.
                outcome: Outcome::RoomDeleted,
                reason: "no_session",
                detail: None,
            },
            Some(Answer::Unauthorized) => Ended {
                outcome: Outcome::NotSignedIn,
                reason: "not_signed_in",
                detail: None,
            },
            Some(Answer::Delivered | Answer::Settled) | None => {
                if self.opening.take().is_some() {
                    // Before the gate opens, so that no frame meets a muted track.
                    self.with_track(MicTrack::unmute);
                    self.transmitter.track_lock.reopen();
                    self.transmitter.send(TxCommand::Gate(GateCommand::Open));
                }
                return;
            }
        };
        // `conchd` would not take this transmission as reported. The connection is dropped
        // before the track opens: the machine is told it is not connected, which ends the
        // press that waited.
        self.disconnected(refused);
        if self.opening.take().is_some() {
            // There was no connection to drop, which cannot be while a press waits. The
            // press is ended all the same, and its track never opens.
            self.on_transmit(TxEvent::GateShut {
                limit_reached: false,
            });
        }
    }

    fn with_track(&self, act: impl FnOnce(&Track<T>)) {
        if let Some(track) = self.live.as_ref().and_then(|live| live.track.as_ref()) {
            act(track);
        }
    }

    /// Mutes the track once no frame can follow the mute into it: a frame the transmit
    /// task is handing over is let finish, and none is handed over after (`TrackLock`).
    fn mute_track(&self) {
        self.transmitter.track_lock.close();
        self.with_track(MicTrack::mute);
    }

    fn show(&mut self, connection: Connection) {
        if self.shown.as_ref() != Some(&connection) {
            self.out.show(&Event::Connection(&connection));
            self.shown = Some(connection);
        }
    }

    fn on_progress(&mut self, progress: Progress<T>) {
        match progress {
            Progress::Ask => {
                self.show(Connection::Connecting);
                self.phase = Phase::Establishing(Box::pin(establish(
                    self.client.clone(),
                    Arc::clone(&self.transport),
                    self.channel.clone(),
                    Arc::clone(&self.scrubber),
                    AttemptLimits {
                        connect: self.timings.connect_limit,
                        publish: self.timings.publish_limit,
                        close: self.timings.close_limit,
                    },
                )));
            }
            Progress::Established(Ok(established)) => self.connected(established),
            Progress::Established(Err(failure)) => {
                // The attempt is over; whatever comes next replaces this.
                self.phase = Phase::Settled;
                match failure {
                    Failure::Session(error) => match session_outcome(&error) {
                        Some((outcome, reason)) => {
                            // A managed owner is told an invite that withheld voice for what
                            // it is; `join` names it as it always has.
                            if self.owner.is_some() && error.code() == Some("voice_not_allowed") {
                                self.ending.get_or_insert(Ending::failed(
                                    VoiceControlReason::VoiceNotAllowed,
                                ));
                            }
                            self.decide(Ended {
                                outcome,
                                reason,
                                detail: Some(self.session_failure_text(&error)),
                            });
                        }
                        None => {
                            self.ending
                                .get_or_insert(Ending::failed(VoiceControlReason::InternalError));
                            self.exit = Some(Err(Error::Api(error)));
                        }
                    },
                    Failure::NoChannelGrant => {
                        self.ending
                            .get_or_insert(Ending::failed(VoiceControlReason::InternalError));
                        self.exit = Some(Err(Error::NoChannelGrant));
                    }
                    Failure::Sdk(error) => self.decide(Ended {
                        outcome: Outcome::ConnectFailed,
                        reason: "connect_failed",
                        detail: Some(error.to_string()),
                    }),
                }
            }
            Progress::Closed(ended) => {
                self.phase = Phase::Settled;
                self.decide(ended);
            }
        }
    }

    /// A room was joined and its track, if it has one, is published and muted.
    fn connected(&mut self, established: Established<T>) {
        let Established {
            room,
            events,
            mic,
            can_publish,
            identity,
        } = established;
        self.tell(PttInput::PublishGrant(can_publish));
        let track = mic.map(|(track, feed)| {
            // The first time a session allows publishing, and never again: a microphone
            // opened once stays open across reconnections, and is replaced only by a switch.
            if self.mic == Mic::NotOpened {
                self.open_microphone();
            }
            self.transmitter.send(TxCommand::Attach(feed));
            track
        });
        self.policy.connected(self.now());
        self.live = Some(Live {
            room,
            events,
            track,
            identity,
            tracks: BTreeMap::new(),
            reconnecting_since: None,
            ignored_a_track: false,
        });
        self.phase = Phase::Settled;
        self.show(Connection::Connected);
        // Only now, with the track muted: until this the machine answers a press with
        // "not connected".
        self.tell(PttInput::Connected(true));
    }

    /// Opens the microphone `input` names and hands it to the transmit task. One that cannot
    /// be opened, or that there is none of, leaves the session with no microphone: nothing
    /// else is opened in its place.
    fn open_microphone(&mut self) {
        self.open_microphone_as(true);
    }

    /// As [`Session::open_microphone`]. When `present` is false the microphone opened is
    /// not yet taken to be there: the machine is told so by the transmit task's word on
    /// its first frame, or on its having none (a switch, §20.5).
    fn open_microphone_as(&mut self, present: bool) {
        let Some(name) = self.input.opened_as() else {
            // A name nothing is opened for: no microphone, and none other in its place.
            self.mic = Mic::Unavailable;
            return;
        };
        match (self.open_mic)(name) {
            Ok(Some(mic)) if present => {
                self.transmitter.send(TxCommand::Microphone(mic));
                self.mic = Mic::Open;
                self.tell(PttInput::Microphone(true));
            }
            Ok(Some(mic)) => {
                self.transmitter.send(TxCommand::SwitchedMicrophone(mic));
                self.mic = Mic::Open;
            }
            Ok(None) => self.mic = Mic::Unavailable,
            Err(error) => {
                self.mic = Mic::Unavailable;
                // A managed helper names no device in anything it writes (§20.7), and an
                // opener's error may hold the name: it is told in words of this program's
                // own. `join` shows its own person why their microphone did not open.
                let detail = if self.owner.is_some() {
                    MICROPHONE_NOT_OPENED.to_owned()
                } else {
                    self.shown_text(&error)
                };
                self.out.show(&Event::Microphone { detail });
            }
        }
    }

    /// An error's text as it is shown in the status: with the login token, the join tokens
    /// and room names of recent connections and anything shaped like a signed token
    /// replaced, and on one line. The text of an error of `conchd`'s is partly `conchd`'s
    /// own words, or those of whatever answered in its place; the API crate keeps its own
    /// secrets out of them and bounds their length, and cannot know these values.
    fn shown_text(&self, error: &dyn std::fmt::Display) -> String {
        self.scrubber.scrub_line(&error.to_string())
    }

    /// What is shown for a session request that failed: the error, and when no connection
    /// could be made at all, the note on the stored logins, as `conch` adds it.
    fn session_failure_text(&self, error: &ApiError) -> String {
        match (error, &self.unreachable_note) {
            (ApiError::Connect { .. }, Some(note)) => self.shown_text(&format!("{error} ({note})")),
            _ => self.shown_text(error),
        }
    }

    /// Asks the connection policy what follows, and does it.
    fn decide(&mut self, ended: Ended) {
        let step = self
            .policy
            .next(self.now(), ended.outcome, (self.timings.jitter)());
        match step {
            NextStep::Stop { message, exit_code } => {
                self.show(Connection::Stopped { reason: message });
                // For a managed owner: the reason, in the protocol's words.
                let reason = VoiceControlReason::from_word(ended.reason)
                    .filter(|reason| reason.is_failure())
                    .unwrap_or(VoiceControlReason::InternalError);
                self.ending.get_or_insert(Ending::failed(reason));
                self.exit = Some(Err(Error::Stopped { message, exit_code }));
            }
            NextStep::AskNow => {
                self.show(Connection::Waiting {
                    reason: ended.reason,
                    detail: ended.detail,
                    retry_in: Duration::ZERO,
                });
                self.phase = Phase::Ask(None);
            }
            NextStep::RetryAfter(wait) => {
                let wait = wait / self.timings.wait_divisor.max(1);
                self.show(Connection::Waiting {
                    reason: ended.reason,
                    detail: ended.detail,
                    retry_in: wait,
                });
                self.phase = Phase::Ask(Some(Instant::now() + wait));
            }
        }
    }

    fn on_sdk(&mut self, event: Option<SdkEvent>) {
        match event {
            Some(SdkEvent::Republished) => {
                // Before anything else: the SDK has just enabled the track again, muted or
                // not. Muting tells LiveKit about the new publication; disabling is what
                // stops a track that was muted already.
                self.mute_track();
                self.with_track(MicTrack::disable);
                // The SDK republishes inside its own reconnect, when the gate is already
                // forced shut. Should it ever do so at another time, a press in progress
                // would go on against a track that sends nothing while the status says
                // "talking". The press is ended instead, as for a lost connection: it is
                // reported as stopped and the key must be pressed again.
                let reconnecting = self
                    .live
                    .as_ref()
                    .is_some_and(|live| live.reconnecting_since.is_some());
                if !reconnecting && self.ptt.transmission_under_way() {
                    self.tell(PttInput::Connected(false));
                    self.tell(PttInput::Connected(true));
                }
            }
            Some(SdkEvent::Reconnecting) => {
                let began = match self.live.as_mut() {
                    Some(live) if live.reconnecting_since.is_none() => {
                        live.reconnecting_since = Some(Instant::now());
                        true
                    }
                    _ => false,
                };
                if began {
                    self.show(Connection::Reconnecting);
                    // Not connected from this moment: the gate is forced shut.
                    self.tell(PttInput::Connected(false));
                }
            }
            Some(SdkEvent::Reconnected) => {
                if let Some(live) = self.live.as_mut() {
                    live.reconnecting_since = None;
                    self.show(Connection::Connected);
                    // A republished track was muted and disabled before this: the SDK
                    // reports the republish first, and events are handled in order.
                    self.tell(PttInput::Connected(true));
                }
            }
            Some(SdkEvent::TrackSubscribed {
                speaker,
                track,
                frames,
            }) => {
                let Some(live) = self.live.as_mut() else {
                    return;
                };
                if speaker == live.identity {
                    // This client's own audio is never in its own mix.
                    return;
                }
                let speaker = speaker_label(&speaker);
                let of_speaker = live.tracks.values().filter(|s| **s == speaker).count();
                if live.tracks.len() >= MAX_TRACKS || of_speaker >= MAX_TRACKS_PER_SPEAKER {
                    // `frames` is dropped here, which ends the task that was reading the
                    // track.
                    if !live.ignored_a_track {
                        live.ignored_a_track = true;
                        log::warn!(
                            "ignoring further audio tracks: at most {MAX_TRACKS} are mixed, \
                             and {MAX_TRACKS_PER_SPEAKER} of one speaker"
                        );
                    }
                    return;
                }
                live.tracks.insert(track.clone(), speaker.clone());
                self.out.show(&Event::Track {
                    speaker: &speaker,
                    subscribed: true,
                });
                let _ = self.receiver.commands.send(MixCommand::Add {
                    speaker,
                    track,
                    frames,
                });
            }
            Some(SdkEvent::TrackUnsubscribed { track }) => {
                let speaker = self
                    .live
                    .as_mut()
                    .and_then(|live| live.tracks.remove(&track));
                if let Some(speaker) = speaker {
                    self.out.show(&Event::Track {
                        speaker: &speaker,
                        subscribed: false,
                    });
                    let _ = self.receiver.commands.send(MixCommand::Remove { track });
                }
            }
            Some(SdkEvent::Disconnected(reason)) => {
                let (outcome, reason) = disconnect_outcome(reason);
                self.disconnected(Ended {
                    outcome,
                    reason,
                    detail: None,
                });
            }
            None => self.disconnected(Ended {
                outcome: Outcome::ConnectionLost,
                reason: "connection_lost",
                detail: None,
            }),
        }
    }

    /// The connection is over, by the SDK's word or by this loop's own decision. It is left
    /// (which is quick unless the SDK was still reconnecting), and then the policy decides.
    fn disconnected(&mut self, ended: Ended) {
        if self.live.is_none() {
            return;
        }
        // While the track is still at hand: the gate is forced shut and the track muted.
        self.tell(PttInput::Connected(false));
        let Some(live) = self.live.take() else {
            return;
        };
        for speaker in live.tracks.values() {
            self.out.show(&Event::Track {
                speaker,
                subscribed: false,
            });
        }
        self.transmitter.send(TxCommand::Detach);
        let _ = self.receiver.commands.send(MixCommand::Clear);
        let limit = self.timings.close_limit;
        self.phase = Phase::Closing(
            Box::pin(async move {
                let _ = tokio::time::timeout(limit, live.room.close()).await;
            }),
            ended,
        );
    }

    fn on_transmit(&mut self, event: TxEvent) {
        match event {
            TxEvent::GateShut { limit_reached } => {
                // A release mutes: the gate has shut, release tail included.
                self.mute_track();
                if limit_reached && let Some(deadline) = self.ptt.deadline() {
                    // The gate has forwarded as much audio as one press may transmit. Its
                    // clock is the audio itself, and it runs a few milliseconds ahead of
                    // the timer behind the machine's deadline. The machine is told in its
                    // own terms, a tick at that deadline, so that it holds the reason (and
                    // shows it) exactly as when its own timer is the first to notice.
                    let outputs = self.ptt.handle(deadline.max(self.now()), PttInput::Tick);
                    self.obey(outputs);
                }
                self.tell(PttInput::GateShut);
            }
            // About the microphone in use, if it is: one about a microphone a switch closed,
            // still on its way, is about nothing the gate can send from.
            TxEvent::Microphone { present, changes } => {
                if self.transmitter.is_current_microphone(changes) {
                    self.tell(PttInput::Microphone(present));
                }
            }
            // Open mode: the level detector's word, for the machine to decide on, if it was
            // made after the detector's last restart. One made before it (quiet, or an onset,
            // on its way when a reason ended) is stale: passed on, quiet and an onset would
            // open the gate on the reason's ending itself.
            TxEvent::Sound { sounding, restarts } => {
                if self.transmitter.is_current(restarts) {
                    self.tell(PttInput::Sound(sounding));
                }
            }
        }
    }

    fn on_input(&mut self, input: Option<Input>) {
        if matches!(input, Some(Input::Line(_) | Input::UnknownLine)) {
            // On a terminal the line was echoed under the status, which is drawn anew.
            self.out.typed_line();
        }
        match input {
            Some(Input::Line(command)) => {
                self.tell(PttInput::Line(command));
                if command == LineCommand::Quit {
                    self.exit.get_or_insert(Ok(()));
                }
            }
            Some(Input::UnknownLine) => self.out.show(&Event::UnknownCommand),
            // The key device is one more source of presses for the same machine.
            Some(Input::Key(event)) => self.tell(PttInput::Key(event)),
            Some(Input::KeyDevice(state)) => {
                // The machine first: a device that went away is a release, and the gate
                // does not wait for a line to be written.
                self.tell(PttInput::KeyDevice(state.is_ready()));
                self.key_state = Some(state);
                let device = self.key_device.as_deref().unwrap_or_default();
                self.out.show(&Event::KeyDevice { device, state });
            }
            Some(Input::Signal) if self.owner.is_some() => {
                self.begin_stopping(VoiceControlReason::Signal);
            }
            Some(Input::Signal) => {
                self.exit.get_or_insert(Ok(()));
            }
            Some(Input::Control(control)) => self.on_control(control),
            Some(Input::SwitchDevices(switch)) => self.switch_devices(switch),
            Some(Input::OwnerLost) => self.begin_stopping(VoiceControlReason::OwnerLost),
            // A managed helper's inputs end only with its owner.
            Some(Input::Eof) | None if self.owner.is_some() => {
                self.inputs_open = false;
                self.begin_stopping(VoiceControlReason::OwnerLost);
            }
            Some(Input::Eof) | None => {
                self.inputs_open = false;
                self.tell(PttInput::Line(LineCommand::Up));
                self.exit.get_or_insert(Ok(()));
            }
        }
    }

    /// A command of the managed owner's. Each with an id is answered once, after the state
    /// its effect changed.
    fn on_control(&mut self, control: Control) {
        match control {
            Control::SetMute { id, muted } => self.set_state(id, ShutReason::Muted, muted),
            Control::SetDeafen { id, deafened } => {
                self.set_state(id, ShutReason::Deafened, deafened);
            }
            Control::GetState { id } => {
                self.settle(true);
                self.ack(id, None);
            }
            Control::Leave { id } => {
                self.begin_stopping(VoiceControlReason::Left);
                self.settle(false);
                self.ack(id, None);
            }
            Control::SetDevices { id, switch } => {
                if self.stopping() {
                    self.ack(id, Some(VoiceControlErrorCode::Stopping));
                    return;
                }
                self.switch_devices(switch);
                self.settle(false);
                self.ack(id, None);
            }
            Control::SetTalkMode { id, talk_mode } => {
                if self.stopping() {
                    self.ack(id, Some(VoiceControlErrorCode::Stopping));
                    return;
                }
                // The machine's own input: the same mode again changes nothing; another one
                // cuts what was being sent at once and mutes the track (`obey`), and only a
                // fresh press, or a fresh onset, sends after it.
                self.tell(PttInput::TalkMode(talk_mode));
                self.settle(false);
                self.ack(id, None);
            }
            Control::Refused { id, code } => self.ack(id, Some(code)),
            Control::Fatal { code } => {
                if let Some(owner) = self.owner.as_mut() {
                    owner.protocol_error(code);
                }
                // The owner sent what cannot be acted on: the session fails for it,
                // whatever it was doing, and the next state the owner is told is the failed
                // one (`settle` writes nothing more).
                self.ending = Some(Ending::failed(VoiceControlReason::ProtocolError));
                self.shut_now();
                self.exit = Some(Err(Error::Managed(PROTOCOL_ERROR)));
            }
        }
    }

    /// `set_mute` or `set_deafen`: the machine's own toggle, given only when its state
    /// differs from the one wanted, so that a second command changes nothing. Inside the
    /// loop, so that a key pressed at the same moment is applied in order with it.
    fn set_state(&mut self, id: i64, reason: ShutReason, wanted: bool) {
        if self.stopping() {
            self.ack(id, Some(VoiceControlErrorCode::Stopping));
            return;
        }
        if self.ptt.status().blocked.contains(reason) != wanted {
            let toggle = if reason == ShutReason::Muted {
                LineCommand::Mute
            } else {
                LineCommand::Deafen
            };
            self.tell(PttInput::Line(toggle));
        }
        self.settle(false);
        self.ack(id, None);
    }

    /// Uses this microphone and these speakers from now on (`docs/design/tui-voice.md`
    /// §20.5). A side left out, or naming the device in use, is left alone. Nothing here
    /// falls back to another device: one that cannot be opened, or is not there, is
    /// unavailable until it is there. Once the session is on its way out, nothing is opened.
    fn switch_devices(&mut self, switch: DeviceSwitch) {
        if self.stopping() || self.exit.is_some() {
            return;
        }
        if let Some(input) = switch.input {
            self.switch_microphone(Device::switched(&input));
        }
        if let Some(output) = switch.output {
            self.switch_speakers(Device::switched(&output));
        }
    }

    /// Closes the microphone and opens `name` in its place; before the microphone is first
    /// opened, only keeps the name for when it is.
    fn switch_microphone(&mut self, name: Device) {
        if name == self.input {
            return;
        }
        self.input = name;
        match self.mic {
            // Listen-only, or not yet connected: what is opened when publishing is first
            // allowed is the new name. Or closed as the session ends: nothing is opened.
            Mic::NotOpened | Mic::Closed => return,
            Mic::Open | Mic::Unavailable => {}
        }
        // What was being sent ends here, with no release tail, by the path every reason
        // takes: the machine forces the gate shut and the track is muted at once
        // (`mute_track`), so no frame of either microphone reaches the track after this.
        // Its `stopped` follows the gate's word, after the mute. Once the new microphone is
        // there the reason ends, and a held key must be pressed again, or in open mode the
        // detector, started sounding, must hear quiet and a fresh onset: the switch itself
        // opens nothing.
        self.tell(PttInput::Microphone(false));
        // The transmit task takes its commands in order: the old stream is closed before
        // the new one is handed to it. The new one is no microphone to the machine until
        // the task has had a frame of it (`TxEvent::Microphone`): one that is not there is
        // unavailable from now, in the state written before the switch is answered, and one
        // that is there is a moment later.
        self.transmitter.send(TxCommand::CloseMicrophone);
        self.mic = Mic::Unavailable;
        self.open_microphone_as(false);
    }

    /// Opens the speakers `name` names and plays the mix to them from now on; the old ones
    /// are closed. The gate is not touched.
    fn switch_speakers(&mut self, name: Device) {
        if name == self.output {
            return;
        }
        self.output = name;
        let opened = match self.output.opened_as() {
            Some(name) => (self.open_sink)(name).ok(),
            None => None,
        };
        let speakers = opened.unwrap_or_else(|| {
            // The opener's error is not repeated: it may hold the name, and no name is
            // logged (§20.7).
            log::warn!("{SPEAKERS_NOT_OPENED}");
            Box::new(receive::NoSpeakers)
        });
        let playback = if speakers.playing() {
            Playback::On
        } else {
            Playback::Unavailable
        };
        self.receiver.replace_speakers(speakers);
        self.set_playback(playback);
    }

    /// The mix task's word on whether the speakers are playing, if it is about those in use.
    fn on_playing(&mut self, said: Playing) {
        if self.receiver.is_current(said) {
            self.set_playback(if said.playing {
                Playback::On
            } else {
                Playback::Unavailable
            });
        }
    }

    fn set_playback(&mut self, now: Playback) {
        let was = std::mem::replace(&mut self.playback, now);
        match (was, now) {
            (Playback::On, Playback::Unavailable) => {
                log::info!("the speakers are not playing: nothing is heard until they are back");
            }
            (Playback::Unavailable, Playback::On) => log::info!("the speakers are playing"),
            _ => {}
        }
    }

    /// Whether a managed helper is on its way out: leaving, failing, or closing a
    /// connection that ended for a reason the policy stops on.
    fn stopping(&self) -> bool {
        self.ending.is_some()
            || matches!(&self.phase, Phase::Closing(_, ended) if ended.outcome.stops())
    }

    /// Leave, the owner gone, or a signal: the gate shuts at once, with no release tail,
    /// and the loop ends. A connection that is closing because of something the policy
    /// stops on keeps that reason: the session is failing already.
    fn begin_stopping(&mut self, reason: VoiceControlReason) {
        if self.ending.is_none() {
            match &self.phase {
                Phase::Closing(_, ended) if ended.outcome.stops() => {
                    let ended = ended.clone();
                    self.decide(ended);
                }
                _ => self.ending = Some(Ending::stopped(reason)),
            }
        }
        self.shut_now();
        self.exit.get_or_insert(Ok(()));
    }

    /// Nothing more is sent from this moment: the machine is told the connection is gone,
    /// which forces the gate shut with no release tail, and the track is muted.
    fn shut_now(&mut self) {
        self.tell(PttInput::Connected(false));
        self.mute_track();
    }

    fn ack(&mut self, id: i64, refusal: Option<VoiceControlErrorCode>) {
        if let Some(owner) = self.owner.as_mut() {
            owner.ack(id, refusal);
        }
    }

    /// Tells a managed owner the state if it changed since the owner was last told, or
    /// whatever it is if `force`.
    fn settle(&mut self, force: bool) {
        // After a protocol_error, the failed state is the next thing written (§5, rule 5).
        let failing = self
            .ending
            .is_some_and(|ending| ending.reason == VoiceControlReason::ProtocolError);
        if self.owner.is_none() || failing {
            return;
        }
        let snapshot = self.snapshot();
        if let Some(owner) = self.owner.as_mut() {
            owner.state(&snapshot, force);
        }
    }

    /// The state a managed owner is shown, as the loop has it now.
    fn snapshot(&self) -> Snapshot {
        let (phase, reason, retry_in) = self.managed_phase();
        self.snapshot_in(phase, reason, retry_in)
    }

    fn snapshot_in(
        &self,
        phase: VoiceControlPhase,
        reason: Option<VoiceControlReason>,
        retry_in: Option<Duration>,
    ) -> Snapshot {
        let status = self.ptt.status();
        let capture = match self.mic {
            Mic::Open if status.blocked.contains(ShutReason::NoMicrophone) => Capture::Unavailable,
            Mic::Open => Capture::On,
            Mic::Unavailable => Capture::Unavailable,
            Mic::NotOpened | Mic::Closed => Capture::Off,
        };
        let key = match (&self.key_device, self.key_state) {
            (None, _) => KeyView::NotConfigured,
            (Some(_), None) => KeyView::Unknown,
            (Some(_), Some(state)) => KeyView::State {
                state,
                talk: self.talk_key,
            },
        };
        let playback = match self.playback {
            Playback::On => VoiceControlPlayback::On,
            Playback::Unavailable => VoiceControlPlayback::Unavailable,
            Playback::Off => VoiceControlPlayback::Off,
        };
        Snapshot::new(phase, reason, retry_in, capture, status, key)
            .with_playback_and_mode(playback, self.ptt.talk_mode())
    }

    /// The phase a managed owner is shown, with its reason and how long until `conchd` is
    /// asked again, from what the loop is doing.
    fn managed_phase(
        &self,
    ) -> (
        VoiceControlPhase,
        Option<VoiceControlReason>,
        Option<Duration>,
    ) {
        if let Some(ending) = self.ending {
            return (VoiceControlPhase::Stopping, Some(ending.reason), None);
        }
        if let Phase::Closing(_, ended) = &self.phase {
            // The connection has ended and is being left; what follows is the policy's.
            let reason = VoiceControlReason::from_word(ended.reason);
            if ended.outcome.stops() {
                let reason = reason
                    .filter(|reason| reason.is_failure())
                    .unwrap_or(VoiceControlReason::InternalError);
                return (VoiceControlPhase::Stopping, Some(reason), None);
            }
            let reason = reason.filter(|reason| reason.is_retry());
            return (VoiceControlPhase::Reconnecting, reason, None);
        }
        match &self.shown {
            None => (VoiceControlPhase::Starting, None, None),
            Some(Connection::Connecting) => (VoiceControlPhase::Connecting, None, None),
            Some(Connection::Connected) => (VoiceControlPhase::Connected, None, None),
            Some(Connection::Reconnecting) => (VoiceControlPhase::Reconnecting, None, None),
            Some(Connection::Waiting {
                reason, retry_in, ..
            }) => match VoiceControlReason::from_word(reason).filter(|r| r.is_retry()) {
                Some(reason) => (
                    VoiceControlPhase::Reconnecting,
                    Some(reason),
                    Some(*retry_in),
                ),
                None => (VoiceControlPhase::Reconnecting, None, None),
            },
            // Not reached: an ending is set before either is shown.
            Some(Connection::Stopped { .. } | Connection::Closed) => (
                VoiceControlPhase::Stopping,
                Some(VoiceControlReason::InternalError),
                None,
            ),
        }
    }

    fn on_report(&mut self, report: &ReportEvent) {
        let detail = self.shown_text(&report.error);
        self.out.show(&Event::Report {
            state: report.state,
            problem: report.problem,
            detail,
        });
    }

    fn on_stats(&mut self, received: MixStats) {
        let total = self.transmitter.frames_sent.load(Ordering::Relaxed);
        let stats = Stats {
            frames_sent: total.saturating_sub(self.frames_sent_shown),
            frames_sent_total: total,
            reports_delivered: self.reports.delivered.load(Ordering::Relaxed),
            reports_dropped: self.reports.dropped.load(Ordering::Relaxed),
            received,
        };
        self.frames_sent_shown = total;
        if self.live.is_some() {
            self.out.show(&Event::Stats(&stats));
        }
    }

    /// Leaves: the gate is shut, a press that was open gets its `stopped`, the reports are
    /// delivered and the room is left. Every wait here has a limit, so the client exits
    /// even if the gate never says it shut, `conchd` never answers, or the SDK never
    /// closes.
    async fn leave(mut self) -> Result<(), Error> {
        let result = self.exit.take().unwrap_or(Ok(()));

        // As for `quit`, whatever the reason for leaving: the machine tells the gate to
        // shut, and produces `stopped` when the gate says it has.
        self.tell(PttInput::Line(LineCommand::Quit));
        let limit = Instant::now() + self.release_tail + self.timings.gate_shut_grace;
        while self.ptt.transmission_under_way() {
            match tokio::time::timeout_at(limit, self.transmitter.events.recv()).await {
                Ok(Some(event)) => self.on_transmit(event),
                Ok(None) | Err(_) => {
                    // The gate never said it shut. The transmission is over all the same,
                    // and `conchd` is told so.
                    log::warn!("the transmit gate did not report that it shut");
                    self.reports.push(VoiceTransmitState::Stopped);
                    break;
                }
            }
        }
        // Whatever state the gate is in, nothing more is sent.
        let shut = TxCommand::Gate(GateCommand::ForceShut(
            conch_voice_control::ShutReason::NotConnected,
        ));
        self.transmitter.send(shut);
        self.transmitter.send(TxCommand::Detach);
        // Leaving closes the microphone's stream: nothing is captured when not joined.
        self.transmitter.send(TxCommand::CloseMicrophone);
        self.mute_track();

        let live = self.live.take();
        let close_limit = self.timings.close_limit;
        let attempt_limit = self.timings.attempt_limit;
        let phase = std::mem::replace(&mut self.phase, Phase::Settled);
        let left = async {
            if let Some(live) = live {
                let _ = tokio::time::timeout(close_limit, live.room.close()).await;
            }
            match phase {
                Phase::Closing(closing, _) => {
                    let _ = tokio::time::timeout(close_limit, closing).await;
                }
                // An attempt that is dropped where it stands can leave a participant in the
                // room that nobody closes, and between its publish and its mute that
                // participant has an unmuted track: `conchd` would record a transmission
                // nobody reported. So the attempt is given a short time to finish (its
                // track is then muted), and the room it joined is left like any other.
                Phase::Establishing(attempt) => {
                    if let Ok(Ok(joined)) = tokio::time::timeout(attempt_limit, attempt).await {
                        let _ = tokio::time::timeout(close_limit, joined.room.close()).await;
                    }
                }
                Phase::Ask(_) | Phase::Settled => {}
            }
        };
        let (delivered, ()) = tokio::join!(self.reports.flush(self.timings.report_flush), left);
        if !delivered {
            log::warn!("the last transmit reports were not delivered before leaving");
        }
        if result.is_ok() {
            self.show(Connection::Closed);
        }
        // The last lines are given a moment to be written, so that a reader who is reading
        // has them before the process exits. One who is not reading does not hold the exit
        // up: past the limit the lines are left where they are.
        if !self.out.flush(self.timings.output_flush).await {
            log::debug!("the last lines of output were not read before leaving");
        }
        result
    }

    /// A managed helper leaves (`docs/design/tui-voice.md` §8.2, §8.3): the gate is shut and
    /// the microphone and the speakers closed at once, with no release tail; a press that
    /// was open gets its `stopped`, given at most [`REPORT_LIMIT`]; the room is left, given
    /// at most [`ROOM_LIMIT`], or [`OWNER_LOST_STEP`] once the owner is gone. The owner's
    /// commands are answered meanwhile, and its last state is the final one.
    async fn leave_managed(mut self) -> Result<(), Error> {
        let mut result = self.exit.take().unwrap_or(Ok(()));
        let mut ending = self.ending.unwrap_or(match result {
            Ok(()) => Ending::stopped(VoiceControlReason::Left),
            Err(_) => Ending::failed(VoiceControlReason::InternalError),
        });

        // Nothing more is sent, captured or played. The gate was shut when stopping began;
        // this makes sure of it whatever else happened, and closes what is open.
        let shut = TxCommand::Gate(GateCommand::ForceShut(ShutReason::NotConnected));
        self.transmitter.send(shut);
        self.transmitter.send(TxCommand::Detach);
        self.transmitter.send(TxCommand::CloseMicrophone);
        self.mic = Mic::Closed;
        self.mute_track();
        // The mix task ends when its commands do, and the speakers close with it.
        let (none, _) = mpsc::unbounded_channel();
        drop(std::mem::replace(&mut self.receiver.commands, none));
        self.set_playback(Playback::Off);

        // A press that was open is over: the machine produces its `stopped` once the gate
        // says it has shut, which after a forced shut it says at once.
        self.tell(PttInput::Line(LineCommand::Quit));
        let gate_said = Instant::now() + self.timings.gate_shut_grace;
        while self.ptt.transmission_under_way() {
            match tokio::time::timeout_at(gate_said, self.transmitter.events.recv()).await {
                Ok(Some(event)) => self.on_transmit(event),
                Ok(None) | Err(_) => {
                    log::warn!("the transmit gate did not report that it shut");
                    self.reports.push(VoiceTransmitState::Stopped);
                    break;
                }
            }
        }

        let room_limit = if ending.reason == VoiceControlReason::OwnerLost {
            OWNER_LOST_STEP
        } else {
            ROOM_LIMIT
        };
        let attempt_limit = self.timings.attempt_limit.min(room_limit);
        let live = self.live.take();
        let phase = std::mem::replace(&mut self.phase, Phase::Settled);
        let left = async move {
            if let Some(live) = live {
                let _ = tokio::time::timeout(room_limit, live.room.close()).await;
            }
            match phase {
                Phase::Closing(closing, _) => {
                    let _ = tokio::time::timeout(room_limit, closing).await;
                }
                // As for `join`: an attempt under way is given a moment to finish, so that
                // the room it joins is left and its track is not abandoned unmuted.
                Phase::Establishing(attempt) => {
                    if let Ok(Ok(joined)) = tokio::time::timeout(attempt_limit, attempt).await {
                        let _ = tokio::time::timeout(room_limit, joined.room.close()).await;
                    }
                }
                Phase::Ask(_) | Phase::Settled => {}
            }
        };
        // What a get_state is answered with meanwhile: on the way out, the gate shut and
        // nothing captured.
        let stopping = self.snapshot_in(VoiceControlPhase::Stopping, Some(ending.reason), None);
        let mut owner = self.owner.take();
        if ending.reason == VoiceControlReason::ProtocolError
            && let Some(owner) = owner.take()
        {
            // A protocol_error is followed by the failed state and nothing else: it is
            // written now, with the gate shut and nothing captured, and what is left to do
            // is done without a word to the owner.
            let last = self.snapshot_in(VoiceControlPhase::Failed, Some(ending.reason), None);
            owner.finish(&last).await;
        }
        let mut cutoff: Option<Instant> = None;
        {
            let work = async { tokio::join!(self.reports.flush(REPORT_LIMIT), left) };
            tokio::pin!(work);
            loop {
                tokio::select! {
                    biased;
                    (delivered, ()) = &mut work => {
                        if !delivered {
                            log::warn!("the last transmit reports were not delivered before leaving");
                        }
                        break;
                    }
                    () = sleep_until(cutoff) => {
                        log::warn!("the owner is gone: leaving without waiting any longer");
                        break;
                    }
                    input = self.inputs.recv(), if self.inputs_open => match input {
                        Some(Input::Control(control)) => answer_while_stopping(
                            owner.as_mut(),
                            control,
                            &stopping,
                            &mut ending,
                            &mut result,
                        ),
                        Some(Input::OwnerLost | Input::Eof) | None => {
                            if input.is_none() {
                                self.inputs_open = false;
                            }
                            // The tighter bounds of an owner that is gone apply from now.
                            let sooner = Instant::now() + OWNER_LOST_STEP;
                            cutoff = Some(cutoff.map_or(sooner, |at| at.min(sooner)));
                        }
                        Some(_) => {}
                    },
                }
            }
        }
        // Commands already read are answered before the last state, which nothing follows.
        while let Ok(input) = self.inputs.try_recv() {
            if let Input::Control(control) = input {
                answer_while_stopping(owner.as_mut(), control, &stopping, &mut ending, &mut result);
            }
        }
        let last = if ending.failed {
            VoiceControlPhase::Failed
        } else {
            VoiceControlPhase::Stopped
        };
        let last = self.snapshot_in(last, Some(ending.reason), None);
        if let Some(owner) = owner {
            owner.finish(&last).await;
        }
        if ending.failed && result.is_ok() {
            result = Err(Error::Managed(PROTOCOL_ERROR));
        }
        result
    }
}

/// A command that arrives while a managed helper is leaving: answered, and carried out only
/// where that changes nothing. A line that cannot be acted on still fails the session.
fn answer_while_stopping(
    owner: Option<&mut Owner>,
    control: Control,
    stopping: &Snapshot,
    ending: &mut Ending,
    result: &mut Result<(), Error>,
) {
    let Some(owner) = owner else {
        return;
    };
    match control {
        Control::SetMute { id, .. }
        | Control::SetDeafen { id, .. }
        | Control::SetDevices { id, .. }
        | Control::SetTalkMode { id, .. } => {
            owner.ack(id, Some(VoiceControlErrorCode::Stopping));
        }
        Control::GetState { id } => {
            let mut now = stopping.clone();
            now.reason = Some(ending.reason);
            owner.state(&now, true);
            owner.ack(id, None);
        }
        // A second leave: what it asks for is under way.
        Control::Leave { id } => owner.ack(id, None),
        Control::Refused { id, code } => owner.ack(id, Some(code)),
        Control::Fatal { code } => {
            owner.protocol_error(code);
            *ending = Ending::failed(VoiceControlReason::ProtocolError);
            *result = Err(Error::Managed(PROTOCOL_ERROR));
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::sync::atomic::AtomicBool;

    use conch_voice_audio::{CountingSink, Frame, GatedFrame, SpeakerSink};

    use super::*;
    use crate::receive::Speakers;
    use crate::sdk::{Joined, MicFeed};

    /// A LiveKit that is never reached: the tests here drive the loop's steps by hand.
    struct Nowhere;

    impl Transport for Nowhere {
        type Room = NoRoom;

        async fn connect(&self, _url: &str, _token: &Secret) -> Result<Joined<NoRoom>, SdkError> {
            std::future::pending().await
        }
    }

    struct NoRoom;

    impl RoomHandle for NoRoom {
        type Track = NoTrack;
        type Feed = NoFeed;

        async fn publish_microphone(&self) -> Result<(NoTrack, NoFeed), SdkError> {
            std::future::pending().await
        }

        async fn close(self) {}
    }

    struct NoTrack;

    impl MicTrack for NoTrack {
        fn mute(&self) {}
        fn unmute(&self) {}
        fn disable(&self) {}
    }

    struct NoFeed;

    impl MicFeed for NoFeed {
        async fn send(&mut self, _frame: GatedFrame<'_>) {}
    }

    /// Speakers whose playing the test decides.
    struct Plugged(Arc<AtomicBool>);

    impl SpeakerSink for Plugged {
        fn play(&mut self, _frame: &Frame) {}
    }

    impl Speakers for Plugged {
        fn playing(&self) -> bool {
            self.0.load(Ordering::SeqCst)
        }
    }

    /// A session as `join` makes it, with speakers opened by `open_sink`, not yet running.
    fn session(open_sink: OpenSink) -> Session<Nowhere> {
        let server = ServerAddress::parse("http://127.0.0.1:9").unwrap();
        let conchd = Conchd::new(
            &server,
            &Secret::new("FAKE-login-token-do-not-print"),
            Duration::from_secs(1),
            Duration::from_secs(1),
        )
        .unwrap();
        let settings = Settings {
            channel: "ops".to_owned(),
            open_mic: Box::new(|_| Ok(None)),
            open_sink,
            input: None,
            output: Some("default".to_owned()),
            sink_tones: Vec::new(),
            release_tail_ms: 0,
            max_transmit: Duration::from_secs(120),
            talk_mode: TalkMode::PushToTalk,
            level: transmit::level_config(-40, 400),
            key_device: None,
            unreachable_note: None,
            timings: Timings::default(),
        };
        let (_inputs, received) = mpsc::unbounded_channel();
        let out = Output::new(false, Box::new(std::io::sink()));
        Session::new(settings, conchd, Nowhere, Scrubber::new(), received, out).unwrap()
    }

    fn switch_speakers(session: &mut Session<Nowhere>, name: &str) {
        session.on_input(Some(Input::SwitchDevices(DeviceSwitch {
            input: None,
            output: Some(name.to_owned()),
        })));
    }

    /// Takes the mix task's words until the session shows `wanted`.
    async fn until_playback(session: &mut Session<Nowhere>, wanted: Playback) {
        while session.playback != wanted {
            let said =
                tokio::time::timeout(Duration::from_secs(10), session.receiver.playing.recv())
                    .await
                    .expect("a word on the speakers")
                    .expect("the mix task is running");
            session.on_playing(said);
        }
    }

    /// Issue #655: the session says whether the speakers are playing (`playback`, #656).
    /// Speakers that are not there are unavailable until they appear; speakers that cannot
    /// be opened are unavailable, and nothing else is opened in their place; the same name
    /// again changes nothing; `default` is PipeWire's default.
    #[tokio::test]
    async fn the_speakers_are_on_or_unavailable_and_nothing_else_is_opened_in_their_place() {
        let names = Arc::new(Mutex::new(Vec::new()));
        let absent = Arc::new(AtomicBool::new(false));
        let open_sink: OpenSink = Box::new({
            let (names, absent) = (Arc::clone(&names), Arc::clone(&absent));
            move |name: Option<&str>| {
                names.lock().unwrap().push(name.map(str::to_owned));
                match name {
                    None => Ok(Box::new(CountingSink::new(&[])?) as BoxedSink),
                    Some("headset") => Ok(Box::new(Plugged(Arc::clone(&absent))) as BoxedSink),
                    Some(_) => Err(Error::PipeWire {
                        doing: "open the speakers",
                        detail: "FAKE: the stream could not be made".to_owned(),
                    }),
                }
            }
        });
        let mut session = session(open_sink);
        assert_eq!(session.playback, Playback::On);
        assert_eq!(*names.lock().unwrap(), [None], "`default` is the default");

        // Named, and not there: unavailable at once, and until they appear.
        switch_speakers(&mut session, "headset");
        assert_eq!(session.playback, Playback::Unavailable);
        absent.store(true, Ordering::SeqCst);
        until_playback(&mut session, Playback::On).await;
        // Unplugged again.
        absent.store(false, Ordering::SeqCst);
        until_playback(&mut session, Playback::Unavailable).await;

        // Speakers that cannot be opened: unavailable, and no others are opened.
        switch_speakers(&mut session, "broken");
        assert_eq!(session.playback, Playback::Unavailable);
        tokio::time::sleep(Duration::from_millis(50)).await;
        while let Ok(said) = session.receiver.playing.try_recv() {
            session.on_playing(said);
        }
        assert_eq!(session.playback, Playback::Unavailable);
        // The same name again changes nothing.
        switch_speakers(&mut session, "broken");
        let opened = |names: &Mutex<Vec<Option<String>>>| names.lock().unwrap().clone();
        let want = [None, Some("headset".to_owned()), Some("broken".to_owned())];
        assert_eq!(opened(&names), want);

        // Back to the default.
        switch_speakers(&mut session, "default");
        assert_eq!(session.playback, Playback::On);
        assert_eq!(opened(&names).last(), Some(&None));
    }

    /// Only the exact `default` is the default. A name from a switch that `dmn-audio` would
    /// read as another (white space around it, or nothing else) opens nothing; one from the
    /// configuration is passed on as written, as it was before switches existed.
    #[test]
    fn only_default_is_the_default_and_a_padded_switched_name_opens_nothing() {
        for (name, switched) in [
            ("default", Some(None)),
            ("speech_out", Some(Some("speech_out"))),
            (
                "alsa_input.usb-Mic_x-00.mono",
                Some(Some("alsa_input.usb-Mic_x-00.mono")),
            ),
            ("Default", Some(Some("Default"))),
            ("", None),
            (" ", None),
            ("   ", None),
            (" default", None),
            ("default ", None),
            ("  default  ", None),
            (" speech_out", None),
            ("speech_out ", None),
        ] {
            assert_eq!(
                switched_name(name),
                switched.map(|named: Option<&str>| named.map(str::to_owned)),
                "{name:?}"
            );
        }
        assert_eq!(configured_name(None), None);
        assert_eq!(configured_name(Some("default")), None);
        assert_eq!(configured_name(Some(" mic ")), Some(" mic ".to_owned()));
        assert_eq!(configured_name(Some("mic")), Some("mic".to_owned()));
        assert!(Device::switched(" ").opened_as().is_none());
        assert_eq!(Device::switched("default").opened_as(), Some(None));
        for name in ["", " ", " mic", "mic "] {
            assert!(
                Device::configured(Some(name)).opened_as().is_none(),
                "{name:?}"
            );
        }
        assert_eq!(Device::configured(Some("default")).opened_as(), Some(None));
        assert_eq!(Device::configured(None).opened_as(), Some(None));
        assert_eq!(
            Device::configured(Some("mic")).opened_as(),
            Some(Some("mic"))
        );
        assert!(Device::switched(" ") != Device::switched("  "));
        assert!(Device::switched(" ") == Device::switched(" "));
    }

    #[test]
    fn a_switch_shows_which_sides_it_has_and_never_a_name() {
        let switch = DeviceSwitch {
            input: Some("alsa_input.FAKE-usb-mic".to_owned()),
            output: None,
        };
        let shown = format!("{:?}", Input::SwitchDevices(switch));
        assert_eq!(
            shown,
            "SwitchDevices(DeviceSwitch { input: Some(\"..\"), output: None })"
        );
    }
}
