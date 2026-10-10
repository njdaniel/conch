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
//! - **`Reconnecting` is not connected.** The gate is forced shut from that moment until
//!   `Reconnected`. If that has not come within the grace period (20 s) the loop closes the
//!   connection itself and goes back to `conchd`.
//! - **A press unmutes and a release mutes, and neither waits.** The unmute is called
//!   before the gate is told to open, and the mute when the gate says it has shut, so the
//!   track is unmuted for the whole of a transmission and for nothing else.
//! - **Reports never hold up audio.** They are queued, in the order the machine produced
//!   them, for the task in `reports.rs`.
//! - **The machine fails closed, and so does the exit.** The machine gets a tick at its
//!   deadline. Leaving, for whatever reason, shuts the gate first, waits a bounded time for
//!   the gate to say so, and sends a final `stopped` if a press was open either way. A
//!   connection attempt that is under way is given a short time to finish, so that the
//!   room it joins is left and its track is not abandoned unmuted.

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use conch_voice_api::{Client, Error as ApiError, Secret, ServerAddress, VoiceTransmitState};
use conch_voice_audio::AudioError;
use conch_voice_control::{
    ConnectionPolicy, GateCommand, KeyEvent, LineCommand, NextStep, Outcome, Ptt, PttInput,
    PttOutput, TransmitReport,
};
use tokio::sync::mpsc;
use tokio::time::{Instant, MissedTickBehavior};

use crate::error::Error;
use crate::keydev::DeviceState;
use crate::output::{Connection, Event, Output, Stats, speaker_label};
use crate::presence::{self, PresenceTimings, Roster};
use crate::receive::{self, MixCommand, MixStats};
use crate::reports::{MakeClient, ReportEvent, ReportTimings, Reports};
use crate::sdk::{DisconnectReason, MicTrack, RoomHandle, SdkError, SdkEvent, Transport};
use crate::secrets::{ConnectionSecrets, Scrubber};
use crate::transmit::{self, BoxedMic, Transmitter, TxCommand, TxEvent};

/// Opens the microphone source. Called at most once, and only when a session's grant allows
/// publishing: a client that may only listen never opens a microphone. `Ok(None)` means
/// there is none to open.
pub type OpenMic = Box<dyn FnMut() -> Result<Option<BoxedMic>, AudioError> + Send>;

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
    /// SIGINT or SIGTERM. It counts as `quit`.
    Signal,
}

/// Every wait and limit of the loop. The defaults are the design's; tests shorten them.
#[derive(Debug, Clone)]
pub struct Timings {
    /// How long the SDK's own reconnect is allowed before the loop closes the connection
    /// and goes back to `conchd` (the design note's decision 3).
    pub reconnect_grace: Duration,
    /// How long leaving a room may take. Closing during a reconnect was measured at 5 s.
    pub close_limit: Duration,
    /// How long a connection attempt that is in progress when the client leaves is given to
    /// finish, so that the room it joins is left and not abandoned.
    pub attempt_limit: Duration,
    /// How long, beyond the release tail, the gate is given to say it has shut when the
    /// client is leaving.
    pub gate_shut_grace: Duration,
    /// How long the last reports are given to be delivered when the client is leaving.
    pub report_flush: Duration,
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
            close_limit: Duration::from_secs(10),
            attempt_limit: Duration::from_secs(2),
            gate_shut_grace: Duration::from_millis(500),
            report_flush: Duration::from_secs(3),
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
    /// The tones the counting sink looks for.
    pub sink_tones: Vec<f32>,
    /// `audio.release_tail_ms`.
    pub release_tail_ms: u32,
    /// `audio.max_transmit_secs`.
    pub max_transmit: Duration,
    /// The key device being watched, as it is named when its state is shown. `None` when
    /// presses come from standard input only.
    pub key_device: Option<String>,
    /// Waits and limits.
    pub timings: Timings,
}

/// How long a session request or the opening of the presence socket may take.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
/// How long one attempt at a transmit report may take. Reports queue behind one another,
/// so this is short: `conchd` is normally a few milliseconds away.
pub const REPORT_TIMEOUT: Duration = Duration::from_secs(1);

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
    secrets: ConnectionSecrets,
}

/// Why there is no room.
enum Failure {
    Session(ApiError),
    NoChannelGrant,
    Sdk(SdkError),
}

/// One connection attempt, from the session request to a muted track.
async fn establish<T: Transport>(
    client: Client,
    transport: Arc<T>,
    channel: String,
    scrubber: Arc<Scrubber>,
) -> Result<Established<T>, Failure> {
    // A new session, immediately before the attempt it is for.
    let session = client.session(&channel).await.map_err(Failure::Session)?;
    // The room of the grant with no audience. Any other grant is ignored: nets are V5.
    let grant = session.channel_grant().ok_or(Failure::NoChannelGrant)?;
    let can_publish = grant.can_publish;
    // From here until the connection has ended, the SDK's own output is searched for these
    // two values.
    let secrets = scrubber.connection(&grant.token, &grant.room);
    let (room, events) = transport
        .connect(&session.livekit_url, &grant.token)
        .await
        .map_err(Failure::Sdk)?;
    let mic = if can_publish {
        let published = room.publish_microphone().await;
        match published {
            Ok((track, feed)) => {
                // Straight after the publish, which left the track enabled.
                track.mute();
                Some((track, feed))
            }
            Err(error) => {
                room.close().await;
                return Err(Failure::Sdk(error));
            }
        }
    } else {
        None
    };
    // `session` ends here, and the join token with it: nothing keeps one.
    Ok(Established {
        room,
        events,
        mic,
        can_publish,
        identity: session.identity.clone(),
        secrets,
    })
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
    /// Forgotten by the scrubber when the connection has been closed.
    secrets: ConnectionSecrets,
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
    inputs: mpsc::UnboundedReceiver<Input>,
    inputs_open: bool,
    /// The origin of the times the machine and the policy are given.
    start: Instant,
    ptt: Ptt,
    policy: ConnectionPolicy,
    transmitter: Transmitter<Feed<T>>,
    receiver: receive::Receiver,
    reports: Reports,
    rosters: mpsc::UnboundedReceiver<Roster>,
    phase: Phase<T>,
    live: Option<Live<T>>,
    shown: Option<Connection>,
    frames_sent_shown: u64,
    /// Set when the loop is to end, with what `join` then returns.
    exit: Option<Result<(), Error>>,
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
    let Settings {
        channel,
        open_mic,
        sink_tones,
        release_tail_ms,
        max_transmit,
        key_device,
        timings,
    } = settings;
    let transmitter = transmit::spawn(transmit::gate_config(release_tail_ms, max_transmit))?;
    let receiver = receive::spawn(&sink_tones)?;
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
        inputs,
        inputs_open: true,
        start: Instant::now(),
        ptt,
        policy: ConnectionPolicy::new(),
        transmitter,
        receiver,
        reports,
        rosters,
        phase: Phase::Ask(None),
        live: None,
        shown: None,
        frames_sent_shown: 0,
        exit: None,
    };
    // The machine's first words: the gate is told why it is shut, and the first status.
    session.obey(first);
    session.run().await;
    session.leave().await
}

impl<T: Transport> Session<T> {
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
        }
    }

    /// Gives the push-to-talk machine one input and carries out what it says.
    fn tell(&mut self, input: PttInput) {
        let outputs = self.ptt.handle(self.now(), input);
        self.obey(outputs);
    }

    fn obey(&mut self, outputs: Vec<PttOutput>) {
        for output in outputs {
            match output {
                PttOutput::Gate(command) => {
                    if command == GateCommand::Open {
                        // Before the gate opens, so that no frame meets a muted track.
                        self.with_track(MicTrack::unmute);
                    }
                    let _ = self.transmitter.commands.send(TxCommand::Gate(command));
                    if matches!(command, GateCommand::ForceShut(_)) {
                        // A forced shut has no release tail: nothing is left to send.
                        self.with_track(MicTrack::mute);
                    }
                }
                PttOutput::Report(report) => self.reports.push(match report {
                    TransmitReport::Started => VoiceTransmitState::Started,
                    TransmitReport::Stopped => VoiceTransmitState::Stopped,
                }),
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
                )));
            }
            Progress::Established(Ok(established)) => self.connected(established),
            Progress::Established(Err(failure)) => {
                // The attempt is over; whatever comes next replaces this.
                self.phase = Phase::Settled;
                match failure {
                    Failure::Session(error) => match session_outcome(&error) {
                        Some((outcome, reason)) => self.decide(Ended {
                            outcome,
                            reason,
                            detail: Some(error.to_string()),
                        }),
                        None => self.exit = Some(Err(Error::Api(error))),
                    },
                    Failure::NoChannelGrant => self.exit = Some(Err(Error::NoChannelGrant)),
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
            secrets,
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
            secrets,
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
                self.tell(PttInput::Microphone(true));
            }
            Ok(None) => {}
            Err(error) => self.out.show(&Event::Microphone {
                detail: error.to_string(),
            }),
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
                let Live { room, secrets, .. } = live;
                let _ = tokio::time::timeout(limit, room.close()).await;
                // Only now does the scrubber forget this connection's token and room.
                drop(secrets);
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
                let device = self.key_device.as_deref().unwrap_or_default();
                self.out.show(&Event::KeyDevice { device, state });
            }
            Some(Input::Signal) => {
                self.exit.get_or_insert(Ok(()));
            }
            Some(Input::Eof) | None => {
                self.inputs_open = false;
                self.tell(PttInput::Line(LineCommand::Up));
                self.exit.get_or_insert(Ok(()));
            }
        }
    }

    fn on_report(&mut self, report: &ReportEvent) {
        self.out.show(&Event::Report {
            state: report.state,
            problem: report.problem,
            detail: report.error.to_string(),
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
        while self.ptt.status().transmitting {
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
        self.with_track(MicTrack::mute);

        let live = self.live.take();
        let close_limit = self.timings.close_limit;
        let attempt_limit = self.timings.attempt_limit;
        let phase = std::mem::replace(&mut self.phase, Phase::Settled);
        let left = async {
            if let Some(live) = live {
                let _ = tokio::time::timeout(close_limit, live.room.close()).await;
                drop(live.secrets);
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
                        drop(joined.secrets);
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
        result
    }
}
