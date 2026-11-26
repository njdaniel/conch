//! The sound of the owner's screen share, as a managed session carries it
//! (`conch.voice_control.v3`, `docs/design/linux-share-audio.md` §5.3, §7, §10, §11).
//!
//! Input: the owner's `set_share_audio` and `set_share_audio_mute`, read on the command
//! channel and nowhere else ([`ShareRequest`]); what the session knows of its connection
//! ([`Link`]: connected or not, whether `conchd` named a companion, the room); what the SDK
//! says of the share's track (published, refused, republished, unpublished by the server);
//! and what the capture engine reports (`share_capture.rs`). Output: the four share fields
//! of every v3 state ([`ShareView`] and the mute), the share's track published, muted,
//! unmuted and unpublished, and the engine started and stopped. Owns: the track, the feeder
//! that hands the mix to it, and the engine, from a command until a stop, a refusal, the
//! end of the connection or the end of the session.
//!
//! What it keeps:
//!
//! - **Nothing is captured or published but on the owner's command, in phase `connected`,
//!   with a companion.** Outside `connected` a request is refused into the state
//!   (`not_connected`) and not kept; without a companion it is `unsupported`; the
//!   `application` scope is `unsupported_scope`; a helper that cannot capture on its
//!   platform says `unsupported`. Only then is the track published, and only once it is are
//!   the captures opened: a refused publish leaves nothing open.
//! - **The state is confirmed, never hoped for.** While the track is being published, the
//!   captures opened or closed, the state the owner sees is the one before, and the
//!   command's ack waits ([`Answer::Held`]); the session answers later commands as they
//!   come and holds their acks behind it, so acks keep the order commands were read in.
//!   Every wait is bounded: the publish by [`PUBLISH_LIMIT`], the first count by
//!   [`FIRST_COUNT_LIMIT`] and closing by [`STOP_LIMIT`], together well inside the owner's
//!   5 s for an ack. Nothing here waits in the session loop: the microphone's mute and the
//!   owner going are acted on meanwhile.
//! - **The share's mute is its own.** `set_share_audio_mute` mutes the published track and
//!   does nothing else; the microphone's mute, deafen and the gate are never touched here,
//!   and none of them touches this.
//! - **Every end closes the captures before it is said.** A stop, a refusal that replaces a
//!   request, the server taking the track back, the end of the connection, a leave and the
//!   owner going each stop the engine; the state says `off` (or why not) only once it has
//!   closed every capture, or [`STOP_LIMIT`] has passed. A reconnect of the SDK closes the
//!   captures too and says `not_connected`; the request is kept only while the same room is,
//!   and the captures are opened again when it is back. A new connection needs a new
//!   command.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use conch_voice_api::{
    VOICE_CONTROL_CAPTURE_STOP_MS, VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES, VoiceControlShareAudio,
    VoiceControlShareAudioProblem, VoiceShareAudioScope,
};
use conch_voice_audio::StereoFrame;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio::time::Instant;

use crate::sdk::{RoomHandle, SdkError, ShareAudioFeed, ShareAudioTrack};
use crate::share_capture::{
    Engine, FRAME_QUEUE, Report, Reported, ShareCounters, ShareSource, StreamDecision,
};

/// How long publishing the share's track may take. Past it the publish counts as refused.
pub const PUBLISH_LIMIT: Duration = Duration::from_secs(2);
/// How long the engine is given to say what it captures, once the track is published.
pub const FIRST_COUNT_LIMIT: Duration = Duration::from_secs(1);
/// How long closing every capture may take: `VoiceControlCaptureStopMS`.
pub const STOP_LIMIT: Duration = Duration::from_millis(VOICE_CONTROL_CAPTURE_STOP_MS);
/// How long an unpublish is given.
const UNPUBLISH_LIMIT: Duration = Duration::from_secs(2);

// The owner waits 5 s for an ack; the longest a command is held is a close, a publish and a
// first count.
const _: () = assert!(
    STOP_LIMIT.as_millis() + PUBLISH_LIMIT.as_millis() + FIRST_COUNT_LIMIT.as_millis()
        < conch_voice_api::VOICE_CONTROL_ACK_TIMEOUT_MS as u128
);

/// A `set_share_audio` as the session takes it. `Debug` never shows the application name.
#[derive(Clone, PartialEq, Eq)]
pub struct ShareRequest {
    /// Capture and publish, or stop.
    pub capturing: bool,
    /// Which sound; `Some` exactly when capturing.
    pub scope: Option<VoiceShareAudioScope>,
    /// The owner's own application name, left out of the capture; `Some` exactly when
    /// capturing.
    pub application_name: Option<String>,
}

impl fmt::Debug for ShareRequest {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ShareRequest")
            .field("capturing", &self.capturing)
            .field("scope", &self.scope)
            .field(
                "application_name",
                &self.application_name.as_ref().map(|_| ".."),
            )
            .finish()
    }
}

/// The share fields of a state, but the mute.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ShareView {
    /// `share_audio`.
    pub share_audio: VoiceControlShareAudio,
    /// `share_audio_problem`.
    pub problem: Option<VoiceControlShareAudioProblem>,
    /// `share_audio_sources`.
    pub sources: i64,
}

impl ShareView {
    /// Not asked for, or stopped.
    pub const OFF: Self = Self {
        share_audio: VoiceControlShareAudio::Off,
        problem: None,
        sources: 0,
    };

    /// Published, and capturing this many streams; `no_sources` with none.
    #[must_use]
    pub fn on(sources: usize) -> Self {
        let sources = i64::try_from(sources)
            .unwrap_or(VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES)
            .min(VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES);
        Self {
            share_audio: VoiceControlShareAudio::On,
            problem: (sources == 0).then_some(VoiceControlShareAudioProblem::NoSources),
            sources,
        }
    }

    /// Asked for and not achievable now, for this reason. Nothing is captured.
    #[must_use]
    pub fn unavailable(problem: VoiceControlShareAudioProblem) -> Self {
        Self {
            share_audio: VoiceControlShareAudio::Unavailable,
            problem: Some(problem),
            sources: 0,
        }
    }
}

/// What the session knows of its connection when the share needs it.
pub struct Link<'a, R> {
    /// Phase `connected`: joined, not reconnecting, and not on the way out.
    pub connected: bool,
    /// Whether `conchd` named a companion in this connection's session.
    pub companion: bool,
    /// The joined room, while there is one.
    pub room: Option<&'a R>,
}

/// When a command's ack may be written.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Answer {
    /// Now: the state already says what came of it.
    Now,
    /// Once the share has settled ([`ShareAudio::take_resolved`]).
    Held,
}

/// What a closing of the captures leads to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum After {
    /// `off`.
    Off,
    /// `unavailable`, for this.
    Refused(VoiceControlShareAudioProblem),
    /// The SDK is reconnecting: `not_connected`, the track and the request kept.
    Interrupted,
    /// A new request came while closing: publish again.
    Start,
}

/// What publishing the share's track gives.
pub type Published<R> =
    Result<(<R as RoomHandle>::ShareTrack, <R as RoomHandle>::ShareFeed), SdkError>;

/// Where the share is.
enum Status<R: RoomHandle> {
    /// Not asked for, or stopped.
    Off,
    /// Asked for and refused, or failed: nothing is captured or published.
    Refused(VoiceControlShareAudioProblem),
    /// The track is being published.
    Publishing {
        published: oneshot::Receiver<Published<R>>,
        until: Instant,
    },
    /// The track is published and the engine started; its first count is awaited.
    Starting { until: Instant },
    /// The engine says this.
    Running(Report),
    /// The SDK is reconnecting: the captures are closed, the track and the request kept.
    Interrupted,
    /// The engine is closing every capture.
    Closing { until: Instant, then: After },
}

impl<R: RoomHandle> Status<R> {
    fn in_flight(&self) -> bool {
        matches!(
            self,
            Status::Publishing { .. } | Status::Starting { .. } | Status::Closing { .. }
        )
    }

    /// What the owner is shown once this is where the share has settled.
    fn view(&self) -> Option<ShareView> {
        Some(match self {
            Status::Off => ShareView::OFF,
            Status::Refused(problem) => ShareView::unavailable(*problem),
            Status::Running(Report::Sources(sources)) => ShareView::on(*sources),
            Status::Running(Report::NoPipewire) => {
                ShareView::unavailable(VoiceControlShareAudioProblem::NoPipewire)
            }
            Status::Running(Report::NoProcessTree) => {
                ShareView::unavailable(VoiceControlShareAudioProblem::NoProcessTree)
            }
            Status::Interrupted => {
                ShareView::unavailable(VoiceControlShareAudioProblem::NotConnected)
            }
            Status::Running(Report::Closed)
            | Status::Publishing { .. }
            | Status::Starting { .. }
            | Status::Closing { .. } => return None,
        })
    }
}

/// What woke the share.
pub enum Wake<R: RoomHandle> {
    /// The publish answered.
    Published(Published<R>),
    /// The engine said something.
    Report(Reported),
    /// A wait ran out.
    Deadline,
}

/// The share's sound of one managed session.
pub struct ShareAudio<R: RoomHandle> {
    source: ShareSource,
    status: Status<R>,
    /// The mute the owner asked for: kept whatever else happens, until it asks again.
    muted: bool,
    /// A revoke of the share's grant was seen while the track was being published: the
    /// publish's answer is not kept, whatever the permission says by then.
    revoked_while_publishing: bool,
    /// The same mute, for the feeder: while it is set no frame of the mix is handed to the
    /// track's source at all. This is the helper's own guarantee, apart from LiveKit's mute,
    /// which a full reconnect's republish does not keep (it enables the same track again).
    mute_flag: Arc<AtomicBool>,
    /// The application name of the request in force, while one is.
    wanted: Option<String>,
    track: Option<R::ShareTrack>,
    /// The way into the feeder: dropped with the track.
    frames: Option<mpsc::Sender<StereoFrame>>,
    feeder: Option<JoinHandle<()>>,
    engine: Option<Engine>,
    generation: u64,
    reports: mpsc::UnboundedReceiver<Reported>,
    reporting: mpsc::UnboundedSender<Reported>,
    counters: Arc<ShareCounters>,
    /// What the owner was last shown as settled.
    shown: ShareView,
    streams: Option<Arc<[StreamDecision]>>,
    /// Commands whose acks wait for the share to settle.
    held: usize,
    /// Of those, the ones whose wait is over.
    resolved: usize,
}

impl<R: RoomHandle> ShareAudio<R> {
    /// Nothing asked for, not muted.
    #[must_use]
    pub fn new(source: ShareSource) -> Self {
        let (reporting, reports) = mpsc::unbounded_channel();
        Self {
            source,
            status: Status::Off,
            muted: false,
            revoked_while_publishing: false,
            mute_flag: Arc::default(),
            wanted: None,
            track: None,
            frames: None,
            feeder: None,
            engine: None,
            generation: 0,
            reports,
            reporting,
            counters: Arc::default(),
            shown: ShareView::OFF,
            streams: None,
            held: 0,
            resolved: 0,
        }
    }

    /// What the owner is shown: the state the share last settled in.
    #[must_use]
    pub fn view(&self) -> ShareView {
        self.shown
    }

    /// The current engine's stream decisions, while the share sound is running.
    pub(crate) fn streams(&self) -> Option<Arc<[StreamDecision]>> {
        self.streams.clone()
    }

    /// The share's mute, as the owner asked for it.
    #[must_use]
    pub fn muted(&self) -> bool {
        self.muted
    }

    /// The captures' counters.
    #[must_use]
    pub fn counters(&self) -> Arc<ShareCounters> {
        Arc::clone(&self.counters)
    }

    /// Whether the captures are being closed.
    #[must_use]
    pub fn closing(&self) -> bool {
        matches!(self.status, Status::Closing { .. })
    }

    /// How many held commands may now be answered; they are answered in the order they were
    /// held. Zero until the share has settled.
    pub fn take_resolved(&mut self) -> usize {
        std::mem::take(&mut self.resolved)
    }

    fn set(&mut self, status: Status<R>) {
        if !matches!(status, Status::Running(Report::Sources(_))) {
            self.streams = None;
        }
        let was = std::mem::replace(&mut self.status, status);
        if let Status::Publishing { mut published, .. } = was {
            abandon::<R>(&mut published);
        }
        if let Some(view) = self.status.view() {
            self.shown = view;
            self.resolved += std::mem::take(&mut self.held);
        }
    }

    fn answer(&mut self) -> Answer {
        if self.status.in_flight() {
            self.held += 1;
            Answer::Held
        } else {
            Answer::Now
        }
    }

    /// `set_share_audio`, in a session that is running (not starting, not stopping).
    pub fn request(&mut self, request: ShareRequest, link: &Link<'_, R>) -> Answer {
        use VoiceControlShareAudioProblem as Problem;
        let Some(name) = request.application_name.filter(|_| request.capturing) else {
            self.wanted = None;
            self.stop(After::Off, true);
            return self.answer();
        };
        if !link.connected {
            let kept = matches!(
                self.status,
                Status::Interrupted
                    | Status::Closing {
                        then: After::Interrupted,
                        ..
                    }
            );
            if kept {
                // The same room is still joined: the request stands for when it is back.
                self.wanted = Some(name);
            } else {
                // Not kept: the owner asks again once it sees `connected`.
                self.wanted = None;
                self.stop(After::Refused(Problem::NotConnected), true);
            }
            return self.answer();
        }
        let refusal = if !link.companion {
            Some(Problem::Unsupported)
        } else if request.scope != Some(VoiceShareAudioScope::SystemExcludingCall) {
            Some(Problem::UnsupportedScope)
        } else if matches!(self.source, ShareSource::Unsupported) {
            Some(Problem::Unsupported)
        } else {
            None
        };
        if let Some(problem) = refusal {
            self.wanted = None;
            self.stop(After::Refused(problem), true);
            return self.answer();
        }
        let renamed = self.wanted.as_deref() != Some(name.as_str());
        self.wanted = Some(name.clone());
        match &mut self.status {
            Status::Publishing { .. } | Status::Starting { .. } | Status::Running(_) => {
                if renamed && let Some(engine) = &self.engine {
                    engine.rename(name);
                }
            }
            // Closing for a reconnect keeps its track: the captures open again on it once
            // they are closed (`closed`), with the name now asked for. Publishing a second
            // track over the kept one would leave one nobody unpublishes.
            Status::Closing { then, .. } => {
                if !(*then == After::Interrupted && self.track.is_some()) {
                    *then = After::Start;
                }
            }
            Status::Off | Status::Refused(_) | Status::Interrupted => self.publish(link),
        }
        self.answer()
    }

    /// `set_share_audio_mute`: the published track's mute, and nothing else.
    pub fn set_mute(&mut self, muted: bool) {
        self.muted = muted;
        self.mute_flag.store(muted, Ordering::SeqCst);
        if let Some(track) = &self.track {
            if muted {
                track.mute();
            } else {
                track.unmute();
            }
        }
    }

    /// Publishes the track, on a task of its own: the session goes on meanwhile.
    fn publish(&mut self, link: &Link<'_, R>) {
        let Some(room) = link.room.filter(|_| link.connected) else {
            self.wanted = None;
            self.set(Status::Refused(VoiceControlShareAudioProblem::NotConnected));
            return;
        };
        self.revoked_while_publishing = false;
        let publishing = room.publish_share_audio();
        let (answer, published) = oneshot::channel();
        tokio::spawn(async move {
            let result = publishing.await;
            // Nobody waits any more (a stop, the publish taking too long, the session
            // ending): what was published is taken back at once.
            if let Err(Ok((track, _feed))) = answer.send(result) {
                track.mute();
                unpublish_within(track).await;
            }
        });
        self.set(Status::Publishing {
            published,
            until: Instant::now() + PUBLISH_LIMIT,
        });
    }

    /// Closes everything and goes to `then` once every capture is closed. With `unpublish`
    /// the track is unpublished; without, it is dropped with the room it belongs to.
    fn stop(&mut self, then: After, unpublish: bool) {
        // A publish still under way is let go of: its task takes back what it publishes.
        if let Some(track) = self.track.take() {
            // Silent at once, before anything else.
            track.mute();
            if unpublish {
                tokio::spawn(unpublish_within(track));
            }
        }
        self.frames = None;
        if let Some(feeder) = self.feeder.take() {
            feeder.abort();
        }
        self.close_engine(then);
    }

    fn close_engine(&mut self, then: After) {
        match &self.engine {
            Some(engine) => {
                engine.stop();
                self.set(Status::Closing {
                    until: Instant::now() + STOP_LIMIT,
                    then,
                });
            }
            None => self.after(then),
        }
    }

    fn after(&mut self, then: After) {
        match then {
            After::Off => self.set(Status::Off),
            After::Refused(problem) => self.set(Status::Refused(problem)),
            After::Interrupted => self.set(Status::Interrupted),
            // Published again by `on_wake`, which has the room.
            After::Start => self.set(Status::Off),
        }
    }

    /// Starts the engine on the published track.
    fn start_engine(&mut self) {
        let (ShareSource::Captures(captures), Some(name), Some(frames)) =
            (&self.source, self.wanted.clone(), self.frames.clone())
        else {
            self.set(Status::Refused(VoiceControlShareAudioProblem::NoPipewire));
            return;
        };
        self.generation += 1;
        match Engine::spawn(
            captures,
            name,
            self.generation,
            frames,
            self.reporting.clone(),
            Arc::clone(&self.counters),
        ) {
            Ok(engine) => {
                self.engine = Some(engine);
                self.set(Status::Starting {
                    until: Instant::now() + FIRST_COUNT_LIMIT,
                });
            }
            Err(_) => {
                log::warn!("share audio: no thread could be started for the captures");
                self.wanted = None;
                self.stop(
                    After::Refused(VoiceControlShareAudioProblem::NoPipewire),
                    true,
                );
            }
        }
    }

    /// Waits for what the share waits on: a publish, the engine's word, or a deadline.
    /// Never, when it waits on nothing. Dropping this loses nothing.
    pub async fn wake(&mut self) -> Wake<R> {
        let until = match &self.status {
            Status::Publishing { until, .. }
            | Status::Starting { until }
            | Status::Closing { until, .. } => Some(*until),
            _ => None,
        };
        let deadline = async {
            match until {
                Some(until) => tokio::time::sleep_until(until).await,
                None => std::future::pending().await,
            }
        };
        let status = &mut self.status;
        let reports = &mut self.reports;
        let published = async {
            match status {
                Status::Publishing { published, .. } => published.await,
                _ => std::future::pending().await,
            }
        };
        tokio::select! {
            biased;
            Some(reported) = reports.recv() => Wake::Report(reported),
            published = published => match published {
                Ok(result) => Wake::Published(result),
                // The task ended without an answer: it was refused.
                Err(_) => Wake::Published(Err(SdkError::scrubbed("the publish ended without an answer"))),
            },
            () = deadline => Wake::Deadline,
        }
    }

    /// Carries out what woke the share.
    pub fn on_wake(&mut self, wake: Wake<R>, link: &Link<'_, R>) {
        use VoiceControlShareAudioProblem as Problem;
        match wake {
            Wake::Published(Ok((track, feed))) => {
                if !matches!(self.status, Status::Publishing { .. }) || self.wanted.is_none() {
                    track.mute();
                    tokio::spawn(unpublish_within(track));
                    return;
                }
                // The grant was taken back while the publish was under way: an event said
                // so meanwhile, or the permission the SDK holds now does.
                if std::mem::take(&mut self.revoked_while_publishing) || !track.is_allowed() {
                    log::info!(
                        "share audio: the share's grant was taken back during the publish; its track is unpublished"
                    );
                    track.mute();
                    tokio::spawn(unpublish_within(track));
                    self.wanted = None;
                    self.set(Status::Refused(Problem::PublishRefused));
                    return;
                }
                // Never two: a track still held is taken back before this one is kept.
                if let Some(old) = self.track.take() {
                    old.mute();
                    tokio::spawn(unpublish_within(old));
                }
                // Straight after the publish, which left it unmuted: the mute asked for.
                if self.muted {
                    track.mute();
                }
                let (frames, feeding) = mpsc::channel(FRAME_QUEUE);
                self.feeder = Some(tokio::spawn(feed_mix(
                    feed,
                    feeding,
                    Arc::clone(&self.mute_flag),
                )));
                self.frames = Some(frames);
                self.track = Some(track);
                self.start_engine();
            }
            Wake::Published(Err(_)) => {
                // The SDK's words are not repeated: the reason is enough.
                log::warn!("share audio: the media server refused the share's track");
                self.wanted = None;
                self.set(Status::Refused(Problem::PublishRefused));
            }
            Wake::Report(Reported {
                generation,
                report,
                streams,
            }) => {
                if self.engine.as_ref().map(Engine::generation) != Some(generation) {
                    return;
                }
                match report {
                    Report::Closed => {
                        self.engine = None;
                        if let Status::Closing { then, .. } = self.status {
                            self.closed(then, link);
                        }
                    }
                    report => {
                        if matches!(self.status, Status::Starting { .. } | Status::Running(_)) {
                            self.streams = streams;
                            self.set(Status::Running(report));
                        }
                    }
                }
            }
            Wake::Deadline => match &self.status {
                Status::Publishing { .. } => {
                    log::warn!(
                        "share audio: publishing the share's track took longer than {} ms",
                        PUBLISH_LIMIT.as_millis()
                    );
                    self.wanted = None;
                    self.set(Status::Refused(Problem::PublishRefused));
                }
                Status::Starting { .. } => self.set(Status::Running(Report::NoPipewire)),
                Status::Closing { then, .. } => {
                    let then = *then;
                    log::warn!(
                        "share audio: the captures took longer than {} ms to close; they are closing",
                        STOP_LIMIT.as_millis()
                    );
                    // The engine closes them all the same; its word no longer counts.
                    self.engine = None;
                    self.closed(then, link);
                }
                _ => {}
            },
        }
    }

    /// Every capture is closed: what was to follow.
    fn closed(&mut self, then: After, link: &Link<'_, R>) {
        match then {
            After::Interrupted if link.connected && self.track.is_some() => self.resume(link),
            After::Start if self.wanted.is_some() => self.publish(link),
            then => self.after(then),
        }
    }

    /// The session is connected (again, or for the first time): a `not_connected` refusal
    /// is over.
    pub fn connected(&mut self) {
        if matches!(
            self.status,
            Status::Refused(VoiceControlShareAudioProblem::NotConnected)
        ) {
            self.set(Status::Off);
        }
    }

    /// The SDK is reconnecting: the captures close and the request is kept for this room.
    pub fn reconnecting(&mut self) {
        match &mut self.status {
            Status::Publishing { .. } => {
                self.wanted = None;
                self.set(Status::Refused(VoiceControlShareAudioProblem::NotConnected));
            }
            // The track and its feeder are kept: the same room may come back.
            Status::Starting { .. } | Status::Running(_) => {
                self.close_engine(After::Interrupted);
            }
            Status::Closing { then, .. } => {
                if *then == After::Start {
                    self.wanted = None;
                    *then = After::Refused(VoiceControlShareAudioProblem::NotConnected);
                }
            }
            Status::Off | Status::Refused(_) | Status::Interrupted => {}
        }
    }

    /// The SDK restored the connection: a request kept through it captures again.
    pub fn reconnected(&mut self, link: &Link<'_, R>) {
        if matches!(self.status, Status::Interrupted) {
            self.resume(link);
        }
    }

    /// After an interruption, the captures open again only on a track that is still
    /// published and still allowed: a grant taken back during the reconnect, or a republish
    /// that failed, ends the share's sound instead.
    fn resume(&mut self, link: &Link<'_, R>) {
        let held = self
            .track
            .as_ref()
            .map(|track| (track.is_allowed(), track.is_published()));
        match held {
            Some((true, true)) if self.wanted.is_some() && link.connected => {
                self.start_engine();
            }
            Some((false, _)) => {
                log::info!(
                    "share audio: the share's grant was taken back during the reconnect; the captures stay closed"
                );
                self.wanted = None;
                self.stop(After::Off, true);
            }
            Some((true, false)) => {
                log::warn!(
                    "share audio: the share's track was not published again after the reconnect; the captures stay closed"
                );
                self.wanted = None;
                self.stop(After::Off, true);
            }
            _ => {
                self.wanted = None;
                self.stop(After::Off, true);
            }
        }
    }

    /// The SDK published the share's track again, and enabled it whatever its mute was
    /// (the same track, whose muted flag survives, so LiveKit's mute alone is a no-op on
    /// it). The mute asked for is applied again and the track disabled, as the microphone's
    /// is (§13 row 12); the feeder hands it nothing meanwhile either way.
    pub fn republished(&mut self) {
        if self.muted
            && let Some(track) = &self.track
        {
            track.mute();
            track.disable();
        }
    }

    /// This participant's permission changed; `allowed` is what that change itself said. If
    /// the share's grant was taken back (the share ended, or conchd withdrew it), by this
    /// change or by the permission the SDK holds now, the track is unpublished here, since
    /// the SDK does not drop a local track the server closed, and the captures close: the
    /// share's sound is off and its request is over. Either one saying no is enough, so a
    /// revoke followed at once by a re-grant still ends it. A publish still under way is
    /// marked, and its answer is not kept.
    pub fn permission_changed(&mut self, allowed: bool) {
        if matches!(self.status, Status::Publishing { .. }) {
            self.revoked_while_publishing |= !allowed;
            return;
        }
        let Some(track) = &self.track else { return };
        if allowed && track.is_allowed() {
            return;
        }
        log::info!(
            "share audio: the share's grant was taken back; its track is unpublished and the captures close"
        );
        self.wanted = None;
        self.stop(After::Off, true);
    }

    /// The connection is over: nothing is kept for the next one.
    pub fn disconnected(&mut self) {
        self.wanted = None;
        self.stop(After::Off, false);
    }

    /// The session is leaving, or its owner is gone: everything closes.
    pub fn leave(&mut self) {
        self.wanted = None;
        self.stop(After::Off, false);
    }

    /// Waits, at most `limit`, until the captures are closed, as leaving does. A link with
    /// no room: nothing is started from here.
    pub async fn until_closed(&mut self, limit: Duration) {
        let none = Link {
            connected: false,
            companion: false,
            room: None,
        };
        let deadline = Instant::now() + limit;
        while self.status.in_flight() {
            match tokio::time::timeout_at(deadline, self.wake()).await {
                Ok(wake) => self.on_wake(wake, &none),
                Err(_) => {
                    if let Status::Closing { then, .. } = self.status {
                        self.engine = None;
                        self.after(then);
                    } else {
                        self.set(Status::Off);
                    }
                }
            }
        }
    }
}

/// A publish nobody will take any more: closed, so that one still under way finds nobody and
/// takes its track back itself (`publish`), and one already answered is taken back here.
fn abandon<R: RoomHandle>(published: &mut oneshot::Receiver<Published<R>>) {
    published.close();
    if let Ok(Ok((track, _feed))) = published.try_recv() {
        track.mute();
        tokio::spawn(unpublish_within(track));
    }
}

/// Unpublishes the track, and says so when that took longer than [`UNPUBLISH_LIMIT`]: the
/// track was muted before, so a slow unpublish sends nothing meanwhile.
async fn unpublish_within<T: ShareAudioTrack>(track: T) {
    if tokio::time::timeout(UNPUBLISH_LIMIT, track.unpublish())
        .await
        .is_err()
    {
        log::warn!(
            "share audio: unpublishing the share's track took longer than {} ms",
            UNPUBLISH_LIMIT.as_millis()
        );
    }
}

/// Hands each mixed frame to the track's audio source, until the engine and the share let go
/// of the queue. While the share is muted nothing is handed over: the frame is taken from the
/// queue, so that the engine is never held up, and let go.
async fn feed_mix<F: ShareAudioFeed>(
    mut feed: F,
    mut frames: mpsc::Receiver<StereoFrame>,
    muted: Arc<AtomicBool>,
) {
    while let Some(frame) = frames.recv().await {
        if !muted.load(Ordering::SeqCst) {
            feed.send(&frame).await;
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicBool, Ordering};

    use conch_voice_api::VoiceShareAudioScope;

    use super::*;
    use crate::sdk::{MicFeed, MicTrack};
    use crate::share_capture::{
        Capture, CaptureSpec, Captures, Graph, Look, OpenFailed, Registry, ToneCapture,
    };
    use crate::share_eligibility::{NoTree, ProcessTree, StreamFacts};

    /// What the fake room was asked to do with the share's track.
    #[derive(Default)]
    struct Book {
        published: u32,
        refuse: u32,
        hang: bool,
        muted: bool,
        mutes: u32,
        unmutes: u32,
        unpublished: u32,
        disables: u32,
        frames: u64,
        /// The share's grant taken back: the permission the SDK holds no longer allows it.
        revoked: bool,
        /// The track no longer among the participant's publications (a failed republish).
        gone: bool,
    }

    #[derive(Clone, Default)]
    struct Room(Arc<Mutex<Book>>);

    struct Track(Arc<Mutex<Book>>);
    struct Feed(Arc<Mutex<Book>>);

    impl ShareAudioTrack for Track {
        fn id(&self) -> String {
            "TR_share".to_owned()
        }
        fn is_allowed(&self) -> bool {
            !self.0.lock().unwrap().revoked
        }
        fn is_published(&self) -> bool {
            !self.0.lock().unwrap().gone
        }
        fn mute(&self) {
            let mut book = self.0.lock().unwrap();
            book.muted = true;
            book.mutes += 1;
        }
        fn unmute(&self) {
            let mut book = self.0.lock().unwrap();
            book.muted = false;
            book.unmutes += 1;
        }
        fn disable(&self) {
            self.0.lock().unwrap().disables += 1;
        }
        fn unpublish(self) -> impl Future<Output = ()> + Send + 'static {
            self.0.lock().unwrap().unpublished += 1;
            async {}
        }
    }

    impl ShareAudioFeed for Feed {
        async fn send(&mut self, _frame: &StereoFrame) {
            self.0.lock().unwrap().frames += 1;
        }
    }

    struct NoMic;
    impl MicTrack for NoMic {
        fn mute(&self) {}
        fn unmute(&self) {}
        fn disable(&self) {}
    }
    impl MicFeed for NoMic {
        async fn send(&mut self, _frame: conch_voice_audio::GatedFrame<'_>) {}
    }

    impl RoomHandle for Room {
        type Track = NoMic;
        type Feed = NoMic;
        type ShareTrack = Track;
        type ShareFeed = Feed;

        async fn publish_microphone(&self) -> Result<(NoMic, NoMic), SdkError> {
            Ok((NoMic, NoMic))
        }

        fn publish_share_audio(
            &self,
        ) -> impl Future<Output = Result<(Track, Feed), SdkError>> + Send + 'static {
            let book = Arc::clone(&self.0);
            async move {
                let hang = {
                    let mut b = book.lock().unwrap();
                    if b.refuse > 0 {
                        b.refuse -= 1;
                        return Err(SdkError::scrubbed("FAKE: not allowed"));
                    }
                    b.published += 1;
                    b.muted = false;
                    b.hang
                };
                if hang {
                    tokio::time::sleep(Duration::from_secs(60)).await;
                }
                Ok((Track(Arc::clone(&book)), Feed(book)))
            }
        }

        async fn close(self) {}
    }

    /// A graph of streams the test sets, whose captures count themselves.
    #[derive(Clone, Default)]
    struct World(Arc<Mutex<(u64, Vec<StreamFacts>)>>, Arc<AtomicBool>);

    impl World {
        fn play(&self, serials: &[u64]) {
            let mut state = self.0.lock().unwrap();
            state.0 += 1;
            state.1 = serials
                .iter()
                .map(|serial| StreamFacts {
                    serial: Some(*serial),
                    sec_pid: Some(9000),
                    application_process_id: None,
                    application_name: None,
                    media_name: None,
                })
                .collect();
        }
    }

    impl Graph for World {
        fn look(&mut self, since: Option<u64>) -> Look {
            let state = self.0.lock().unwrap();
            if since == Some(state.0) {
                return Look::Unchanged;
            }
            Look::Changed(Registry {
                generation: state.0,
                streams: state.1.clone(),
            })
        }
        fn open_capture(&mut self, _spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed> {
            Ok(Box::new(ToneCapture::new(440.0, 0.1)))
        }
    }

    fn source(world: &World) -> ShareSource {
        let graph = world.clone();
        let tree_gone = Arc::clone(&world.1);
        ShareSource::Captures(Captures {
            graph: Arc::new(move |_| Some(Box::new(graph.clone()) as Box<dyn Graph>)),
            tree: Arc::new(move || {
                if tree_gone.load(Ordering::SeqCst) {
                    Err(NoTree::Unreadable)
                } else {
                    ProcessTree::build(Some(4100), 4000, &[(4100, 4000)])
                }
            }),
            own_pid: Some(4100),
        })
    }

    fn start(name: &str) -> ShareRequest {
        ShareRequest {
            capturing: true,
            scope: Some(VoiceShareAudioScope::SystemExcludingCall),
            application_name: Some(name.to_owned()),
        }
    }

    fn stop() -> ShareRequest {
        ShareRequest {
            capturing: false,
            scope: None,
            application_name: None,
        }
    }

    fn link(room: &Room) -> Link<'_, Room> {
        Link {
            connected: true,
            companion: true,
            room: Some(room),
        }
    }

    /// Runs the share until it has settled, and says how many held commands were resolved.
    async fn settle(share: &mut ShareAudio<Room>, room: &Room) -> usize {
        settle_on(share, &link(room)).await
    }

    async fn settle_on(share: &mut ShareAudio<Room>, link: &Link<'_, Room>) -> usize {
        let mut resolved = 0;
        while share.status.in_flight() {
            let wake = tokio::time::timeout(Duration::from_secs(10), share.wake())
                .await
                .expect("the share settles");
            share.on_wake(wake, link);
            resolved += share.take_resolved();
        }
        resolved + share.take_resolved()
    }

    /// Takes the engine's reports until the view is `want`.
    async fn until_view(share: &mut ShareAudio<Room>, room: &Room, want: ShareView) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while share.view() != want {
            let wake = tokio::time::timeout_at(deadline, share.wake())
                .await
                .unwrap_or_else(|_| panic!("never {want:?}; {:?}", share.view()));
            share.on_wake(wake, &link(room));
        }
    }

    #[tokio::test]
    async fn outside_connected_without_a_companion_or_for_one_application_it_is_refused_into_the_state()
     {
        use VoiceControlShareAudioProblem as Problem;
        let world = World::default();
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));

        let connecting = Link {
            connected: false,
            companion: true,
            room: None,
        };
        assert_eq!(share.request(start("Conch"), &connecting), Answer::Now);
        assert_eq!(share.view(), ShareView::unavailable(Problem::NotConnected));
        // Not kept: connecting clears it, and nothing is asked of the room.
        share.connected();
        assert_eq!(share.view(), ShareView::OFF);

        let alone = Link {
            connected: true,
            companion: false,
            room: Some(&room),
        };
        assert_eq!(share.request(start("Conch"), &alone), Answer::Now);
        assert_eq!(share.view(), ShareView::unavailable(Problem::Unsupported));

        let mut one = start("Conch");
        one.scope = Some(VoiceShareAudioScope::Application);
        assert_eq!(share.request(one, &link(&room)), Answer::Now);
        assert_eq!(
            share.view(),
            ShareView::unavailable(Problem::UnsupportedScope)
        );

        // A helper that cannot capture on its platform.
        let mut elsewhere: ShareAudio<Room> = ShareAudio::new(ShareSource::Unsupported);
        assert_eq!(elsewhere.request(start("Conch"), &link(&room)), Answer::Now);
        assert_eq!(
            elsewhere.view(),
            ShareView::unavailable(Problem::Unsupported)
        );

        // A stop brings each back to off.
        assert_eq!(share.request(stop(), &link(&room)), Answer::Now);
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(room.0.lock().unwrap().published, 0, "nothing was published");
    }

    #[tokio::test]
    async fn a_request_publishes_then_captures_and_its_ack_waits_for_the_count() {
        let world = World::default();
        world.play(&[2, 3]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        assert_eq!(share.request(start("Conch"), &link(&room)), Answer::Held);
        // Meanwhile the state is what it was.
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(settle(&mut share, &room).await, 1);
        assert_eq!(share.view(), ShareView::on(2));
        assert_eq!(room.0.lock().unwrap().published, 1);
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 2);

        // The same request again changes nothing and is answered at once.
        assert_eq!(share.request(start("Conch"), &link(&room)), Answer::Now);
        assert_eq!(room.0.lock().unwrap().published, 1);

        // Nothing playing: on, silent, no_sources.
        world.play(&[]);
        until_view(&mut share, &room, ShareView::on(0)).await;
        assert_eq!(
            share.view().problem,
            Some(VoiceControlShareAudioProblem::NoSources)
        );

        // The mix reaches the track.
        let deadline = Instant::now() + Duration::from_secs(10);
        while room.0.lock().unwrap().frames == 0 {
            assert!(Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(5)).await;
        }

        // Stop: closed within the bound, then off, then the ack.
        let began = Instant::now();
        assert_eq!(share.request(stop(), &link(&room)), Answer::Held);
        assert_ne!(
            share.view(),
            ShareView::OFF,
            "not off before the captures close"
        );
        assert_eq!(settle(&mut share, &room).await, 1);
        assert!(began.elapsed() <= STOP_LIMIT, "{:?}", began.elapsed());
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
        tokio::task::yield_now().await;
        let book = room.0.lock().unwrap();
        assert!(book.muted, "silenced first");
        assert_eq!(book.unpublished, 1);
    }

    #[tokio::test]
    async fn the_share_mute_mutes_the_track_and_is_kept_across_a_publish() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        // Muted before anything is published: kept, and applied straight after the publish.
        share.set_mute(true);
        assert!(share.muted());
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        assert!(room.0.lock().unwrap().muted);
        let open = share.counters().open.load(Ordering::SeqCst);
        assert_eq!(open, 1);
        share.set_mute(false);
        assert!(!room.0.lock().unwrap().muted);
        // The captures are as they were.
        assert_eq!(share.counters().open.load(Ordering::SeqCst), open);
        assert_eq!(share.view(), ShareView::on(1));
        // A republish unmutes the track; the mute asked for is applied again.
        share.set_mute(true);
        room.0.lock().unwrap().muted = false;
        share.republished();
        assert!(room.0.lock().unwrap().muted);
        share.leave();
        share.until_closed(STOP_LIMIT).await;
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
        assert!(share.muted(), "the mute is a desired state");
    }

    #[tokio::test]
    async fn a_refused_publish_opens_nothing_and_a_later_one_succeeds() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        room.0.lock().unwrap().refuse = 1;
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        assert_eq!(share.request(start("Conch"), &link(&room)), Answer::Held);
        assert_eq!(settle(&mut share, &room).await, 1);
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::PublishRefused)
        );
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
        // Granted now.
        assert_eq!(share.request(start("Conch"), &link(&room)), Answer::Held);
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::on(1));
        share.leave();
        share.until_closed(STOP_LIMIT).await;
    }

    #[tokio::test]
    async fn a_publish_that_never_answers_is_refused_within_its_bound() {
        let world = World::default();
        let room = Room::default();
        room.0.lock().unwrap().hang = true;
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        let began = Instant::now();
        assert_eq!(share.request(start("Conch"), &link(&room)), Answer::Held);
        assert_eq!(settle(&mut share, &room).await, 1);
        let took = began.elapsed();
        assert!(took >= PUBLISH_LIMIT, "{took:?}");
        assert!(
            took < Duration::from_millis(conch_voice_api::VOICE_CONTROL_ACK_TIMEOUT_MS),
            "{took:?}"
        );
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::PublishRefused)
        );
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn a_stop_or_a_leave_during_the_publish_lets_it_go_and_takes_it_back() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        // Stopped before the publish answered: off at once, and what the publish gives is
        // unpublished by its task.
        assert_eq!(share.request(stop(), &link(&room)), Answer::Now);
        assert_eq!(share.take_resolved(), 1, "the held start is answered first");
        assert_eq!(share.view(), ShareView::OFF);
        let deadline = Instant::now() + Duration::from_secs(10);
        while room.0.lock().unwrap().unpublished == 0 {
            assert!(
                Instant::now() < deadline,
                "the late publish was not taken back"
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);

        share.request(start("Conch"), &link(&room));
        share.leave();
        assert_eq!(share.take_resolved(), 1);
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
    }

    /// Waits, at most ten seconds, until the room has been asked to unpublish `n` tracks:
    /// the unpublish runs on a task of its own.
    async fn until_unpublished(room: &Room, n: u32) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while room.0.lock().unwrap().unpublished < n {
            assert!(Instant::now() < deadline, "not unpublished");
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
        assert_eq!(room.0.lock().unwrap().unpublished, n);
    }

    /// The server taking the share's grant back is seen only as a permission change (the
    /// SDK reports no unpublish of a local track the server closed): the helper unpublishes
    /// the track itself, closes the captures and says off. A change that still allows the
    /// share changes nothing.
    #[tokio::test]
    async fn the_grant_taken_back_unpublishes_the_track_closes_the_captures_and_says_off() {
        let world = World::default();
        world.play(&[2, 3]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        share.permission_changed(true);
        assert_eq!(share.view(), ShareView::on(2));
        assert_eq!(room.0.lock().unwrap().unpublished, 0);

        room.0.lock().unwrap().revoked = true;
        share.permission_changed(false);
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
        assert!(room.0.lock().unwrap().muted, "silent before the unpublish");
        // Unpublished by the helper itself.
        until_unpublished(&room, 1).await;
    }

    /// A grant taken back while the publish is under way is caught when the publish answers:
    /// the track is taken back at once and nothing is captured.
    #[tokio::test]
    async fn a_grant_taken_back_during_the_publish_takes_the_track_back_and_captures_nothing() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        // The permission event came while publishing: nothing is held yet to check.
        room.0.lock().unwrap().revoked = true;
        share.permission_changed(false);
        settle(&mut share, &room).await;
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::PublishRefused)
        );
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
        until_unpublished(&room, 1).await;
    }

    /// A revoke that a re-grant follows at once is still seen: the change that said no ends
    /// the share's sound, although the permission held by the time it is read allows it
    /// again. During the publish it is remembered, and the publish's answer is not kept.
    #[tokio::test]
    async fn a_revoke_followed_at_once_by_a_regrant_still_ends_the_sound() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        // Read after both: the SDK allows it again by now.
        share.permission_changed(false);
        share.permission_changed(true);
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
        until_unpublished(&room, 1).await;

        // The same during a publish.
        share.request(start("Conch"), &link(&room));
        share.permission_changed(false);
        share.permission_changed(true);
        settle(&mut share, &room).await;
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::PublishRefused)
        );
        until_unpublished(&room, 2).await;
        // A later publish is not held to it.
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::on(1));
    }

    /// A publish that answered before anybody took its answer (here a stop, before the
    /// session woke for it) is taken back too: its track is never left published.
    #[tokio::test]
    async fn a_publish_answered_but_not_taken_is_unpublished_when_it_is_let_go() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        // The publish task runs and answers; the session does not wake for it.
        let deadline = Instant::now() + Duration::from_secs(10);
        while room.0.lock().unwrap().published == 0 {
            assert!(Instant::now() < deadline, "the publish never ran");
            tokio::task::yield_now().await;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(room.0.lock().unwrap().unpublished, 0);
        share.request(stop(), &link(&room));
        let deadline = Instant::now() + Duration::from_secs(10);
        while room.0.lock().unwrap().unpublished == 0 {
            assert!(
                Instant::now() < deadline,
                "the answered publish was left published"
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
    }

    /// A request read while the captures close for a reconnect, once connected again, keeps
    /// the one track: the captures open again on it, and no second track is published.
    #[tokio::test]
    async fn a_request_while_closing_for_a_reconnect_keeps_the_one_track() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        share.reconnecting();
        assert!(
            share.closing(),
            "the captures are closing for the reconnect"
        );
        // Connected again before they closed, and the owner asks again.
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::on(1));
        let book = room.0.lock().unwrap();
        assert_eq!(book.published, 1, "one track, never a second");
        assert_eq!(book.unpublished, 0);
    }

    /// After a reconnect, the captures open again only on a track still allowed and still
    /// published: a grant taken back meanwhile, or a republish that failed, ends the sound.
    #[tokio::test]
    async fn a_reconnect_reopens_nothing_on_a_track_taken_back_or_not_republished() {
        for (revoked, gone) in [(true, false), (false, true)] {
            let world = World::default();
            world.play(&[2]);
            let room = Room::default();
            let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
            share.request(start("Conch"), &link(&room));
            settle(&mut share, &room).await;
            share.reconnecting();
            let reconnecting = Link {
                connected: false,
                companion: true,
                room: Some(&room),
            };
            settle_on(&mut share, &reconnecting).await;
            {
                let mut book = room.0.lock().unwrap();
                book.revoked = revoked;
                book.gone = gone;
            }
            share.reconnected(&link(&room));
            settle(&mut share, &room).await;
            assert_eq!(
                share.view(),
                ShareView::OFF,
                "revoked {revoked}, gone {gone}"
            );
            assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
            until_unpublished(&room, 1).await;
        }
    }

    #[tokio::test]
    async fn a_reconnect_closes_the_captures_and_the_same_room_opens_them_again() {
        let world = World::default();
        world.play(&[2]);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        share.reconnecting();
        let reconnecting = Link {
            connected: false,
            companion: true,
            room: Some(&room),
        };
        settle_on(&mut share, &reconnecting).await;
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::NotConnected)
        );
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
        share.reconnected(&link(&room));
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::on(1));
        assert_eq!(room.0.lock().unwrap().published, 1, "the same track");

        // A disconnect: nothing kept for the next connection.
        share.disconnected();
        settle(&mut share, &room).await;
        assert_eq!(share.view(), ShareView::OFF);
        share.connected();
        assert_eq!(share.view(), ShareView::OFF);
        assert_eq!(share.counters().open.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn an_unreadable_tree_captures_nothing_and_says_so() {
        let world = World::default();
        world.play(&[2]);
        world.1.store(true, Ordering::SeqCst);
        let room = Room::default();
        let mut share: ShareAudio<Room> = ShareAudio::new(source(&world));
        share.request(start("Conch"), &link(&room));
        settle(&mut share, &room).await;
        assert_eq!(
            share.view(),
            ShareView::unavailable(VoiceControlShareAudioProblem::NoProcessTree)
        );
        assert_eq!(share.counters().opened.load(Ordering::SeqCst), 0);
        share.leave();
        share.until_closed(STOP_LIMIT).await;
    }

    #[test]
    fn a_request_never_shows_its_application_name() {
        let shown = format!("{:?}", start("Secret App"));
        assert!(!shown.contains("Secret"), "{shown}");
        assert!(shown.contains(".."));
    }

    #[test]
    fn the_view_holds_together_by_the_v3_rules() {
        assert_eq!(ShareView::on(3).problem, None);
        assert_eq!(
            ShareView::on(0).problem,
            Some(VoiceControlShareAudioProblem::NoSources)
        );
        assert_eq!(ShareView::on(5000).sources, 1024);
        assert_eq!(
            ShareView::unavailable(VoiceControlShareAudioProblem::NoPipewire).sources,
            0
        );
    }
}
