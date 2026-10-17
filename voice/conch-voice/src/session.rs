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
//!
//! The same loop runs a managed helper (`managed.rs`, [`serve`]). It is then given an
//! [`Owner`] to tell, and its owner's commands as inputs: once each step of the loop has
//! been applied in full the owner is sent a snapshot if anything it shows changed; a
//! command's ack follows the snapshot its effect changed; and leaving, the owner going and
//! a signal all shut the gate at once, with no release tail, before anything is said.

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use conch_voice_api::{
    Client, Error as ApiError, Secret, ServerAddress, VoiceControlErrorCode, VoiceControlPhase,
    VoiceControlReason, VoiceTransmitState,
};
use conch_voice_control::{
    ConnectionPolicy, GateCommand, KeyEvent, LineCommand, NextStep, Outcome, Ptt, PttInput,
    PttOutput, ShutReason, TransmitReport,
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
use crate::receive::{self, BoxedSink, MixCommand, MixStats};
use crate::reports::{Answer, MakeClient, ReportEvent, ReportTimings, Reports};
use crate::sdk::{DisconnectReason, MicTrack, RoomHandle, SdkError, SdkEvent, Transport};
use crate::secrets::Scrubber;
use crate::transmit::{self, BoxedMic, Transmitter, TxCommand, TxEvent};

/// Opens the microphone source. Called at most once, and only when a session's grant allows
/// publishing: a client that may only listen never opens a microphone. `Ok(None)` means
/// there is none to open.
pub type OpenMic = Box<dyn FnMut() -> Result<Option<BoxedMic>, Error> + Send>;

/// Opens the speakers. Called once, when the session starts: the client hears the channel
/// from the moment it is joined.
pub type OpenSink = Box<dyn FnOnce() -> Result<BoxedSink, Error> + Send>;

/// Something from outside the loop: a line on standard input, its end, a signal, or what
/// the key device's watcher (`keydev.rs`) saw.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
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
    /// The tones the mix is measured for in the reports.
    pub sink_tones: Vec<f32>,
    /// `audio.release_tail_ms`.
    pub release_tail_ms: u32,
    /// `audio.max_transmit_secs`.
    pub max_transmit: Duration,
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
    open_mic: Option<OpenMic>,
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
            open_sink,
            sink_tones,
            release_tail_ms,
            max_transmit,
            key_device,
            unreachable_note,
            timings,
        } = settings;
        let transmitter = transmit::spawn(transmit::gate_config(release_tail_ms, max_transmit))?;
        let receiver = receive::spawn(&sink_tones, open_sink()?)?;
        let reports = Reports::spawn(
            conchd.report_client,
            channel.clone(),
            timings.reports.clone(),
        );
        let rosters = presence::spawn(conchd.client.clone(), channel.clone(), timings.presence);
        let (ptt, first) = Ptt::new(max_transmit);
        let mut session = Session {
            channel,
            release_tail: Duration::from_millis(u64::from(release_tail_ms)),
            timings,
            open_mic: Some(open_mic),
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
                    let ends = matches!(command, GateCommand::Shut | GateCommand::ForceShut(_));
                    let waited = ends && self.opening.take().is_some();
                    never_opened |= waited;
                    // The gate never opened for a press that waited, so a release has
                    // nothing to shut; a reason is passed on all the same, so that the gate
                    // holds the same reasons as the machine.
                    if !(waited && command == GateCommand::Shut) {
                        let _ = self.transmitter.commands.send(TxCommand::Gate(command));
                    }
                    if matches!(command, GateCommand::ForceShut(_)) {
                        // A forced shut has no release tail: nothing is left to send.
                        self.with_track(MicTrack::mute);
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
                    let _ = self
                        .transmitter
                        .commands
                        .send(TxCommand::Gate(GateCommand::Open));
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
            self.open_microphone();
            let _ = self.transmitter.commands.send(TxCommand::Attach(feed));
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

    /// Opens the microphone, the first time a session allows publishing and never again.
    fn open_microphone(&mut self) {
        let Some(mut open) = self.open_mic.take() else {
            return;
        };
        match open() {
            Ok(Some(mic)) => {
                let _ = self.transmitter.commands.send(TxCommand::Microphone(mic));
                self.mic = Mic::Open;
                self.tell(PttInput::Microphone(true));
            }
            Ok(None) => self.mic = Mic::Unavailable,
            Err(error) => {
                self.mic = Mic::Unavailable;
                let detail = self.shown_text(&error);
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
                self.with_track(|track| {
                    track.mute();
                    track.disable();
                });
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
        let _ = self.transmitter.commands.send(TxCommand::Detach);
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
                self.with_track(MicTrack::mute);
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
            TxEvent::Microphone(present) => self.tell(PttInput::Microphone(present)),
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
        self.with_track(MicTrack::mute);
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
        Snapshot::new(phase, reason, retry_in, capture, status, key)
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
        let _ = self.transmitter.commands.send(shut);
        let _ = self.transmitter.commands.send(TxCommand::Detach);
        // Leaving closes the microphone's stream: nothing is captured when not joined.
        let _ = self.transmitter.commands.send(TxCommand::CloseMicrophone);
        self.with_track(MicTrack::mute);

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
        let _ = self.transmitter.commands.send(shut);
        let _ = self.transmitter.commands.send(TxCommand::Detach);
        let _ = self.transmitter.commands.send(TxCommand::CloseMicrophone);
        self.mic = Mic::Closed;
        self.with_track(MicTrack::mute);
        // The mix task ends when its commands do, and the speakers close with it.
        let (none, _) = mpsc::unbounded_channel();
        drop(std::mem::replace(&mut self.receiver.commands, none));

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
        Control::SetMute { id, .. } | Control::SetDeafen { id, .. } => {
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
