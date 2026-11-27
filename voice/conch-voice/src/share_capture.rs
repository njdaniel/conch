//! The captures of a screen share's sound and their mix (`docs/design/linux-share-audio.md`
//! §5.1, §5.3, §11): one capture per eligible application output stream, by its
//! `object.serial`, mixed every 10 ms into one 48 kHz stereo frame for the share's track.
//!
//! Input: the audio system's graph ([`Graph`]: the application streams it lists, and a
//! capture of one of them by its serial), the Conch process tree ([`ReadTree`]), and the
//! application name of the owner's command. Output: one [`StereoFrame`] every 10 ms on a
//! bounded channel, for the track's audio source, and a [`Report`] whenever what is being
//! captured changes, with bounded stream decisions or a failure reason. Owns: an [`Engine`]'s thread,
//! every capture it opened, and their closing.
//!
//! What it keeps:
//!
//! - **One application stream per capture, and nothing else.** The only thing a capture is
//!   asked for is a [`CaptureSpec`]: a serial, 48 kHz, two channels and a ring. There is no
//!   field for a sink, a monitor or a fallback, so no spec this module builds can name one.
//!   On Linux the graph is `dmn-audio`'s application-stream capture and nothing else
//!   (`share_capture_linux.rs`).
//! - **The rule is evaluated on every change of the registry** (`share_eligibility.rs`), and
//!   every [`REEVALUATE_EVERY`] besides: a stream that appears is captured, one that goes is
//!   closed, and the count follows. The tree is read again with it, from `/proc`; when it
//!   cannot be, every capture is closed.
//! - **Fail closed, whole.** A tree that cannot be read is [`Report::NoProcessTree`]; an audio
//!   system that is gone, a capture that cannot be opened, or more eligible streams than
//!   [`MAX_CAPTURES`] is [`Report::NoPipewire`]. Each closes every capture: the share is never
//!   some applications silently. The mix goes on, silent, and the engine tries again every
//!   [`RETRY_EVERY`] while it runs; when the audio system is back it is reached afresh,
//!   since a connection that saw it go may never hear from it again.
//! - **Never in the microphone's way.** The engine is a thread of its own. It hands each mix
//!   to a bounded queue ([`FRAME_QUEUE`]) and drops a frame, counted, when the queue is full;
//!   a capture whose ring has more than [`BACKLOG_FRAMES`] waiting has the oldest dropped,
//!   counted. Mixing uses fixed arrays and [`StereoMixer`] without allocating or taking
//!   locks. Reporting can inspect the audio system's error when a capture fails. Logs carry
//!   counts and reasons without names, serials or pids. The PipeWire callbacks are `dmn-audio`'s.
//! - **Closing is bounded and complete.** Captures are closed as a group, each on a short
//!   thread of its own, within [`STOP_BUDGET`] (inside `VoiceControlCaptureStopMS`), not one
//!   after another. A capture whose close is late is still closed by its thread, counted
//!   ([`ShareCounters::late_stops`]), and never left open: [`ShareCounters::open`] goes back
//!   to zero only when every capture has closed. An engine that is dropped stops.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, mpsc as std_mpsc};
use std::time::{Duration, Instant};

pub use conch_voice_api::VoiceControlShareAudioStreamV3 as StreamDecision;
use conch_voice_api::{
    VOICE_CONTROL_MAX_SHARE_AUDIO_LABEL_BYTES, VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES,
    VOICE_CONTROL_MAX_SHARE_AUDIO_STREAMS, VoiceControlShareAudioStreamReason as StreamReason,
};
use conch_voice_audio::{
    FRAME_LEN, FRAME_MS, SAMPLE_RATE, STEREO_SILENCE, StereoFrame, StereoMixer,
};
use tokio::sync::mpsc;

use crate::share_eligibility::{
    Deferral, Exclusion, NoTree, ProcessTree, Rule, StreamFacts, Verdict, judge,
};

/// PipeWire's graph (Linux only).
#[cfg(target_os = "linux")]
#[path = "share_capture_linux.rs"]
pub mod linux;

/// The channels of a share's sound and of every capture of it.
pub const SHARE_CHANNELS: u32 = 2;
/// How much audio one capture's ring holds: the engine takes a frame every 10 ms.
pub const CAPTURE_RING_MS: u32 = 200;
/// The most streams captured at once: the most the state can count. Beyond it nothing is
/// captured (`no_pipewire`), rather than some applications and not others. At this bound
/// the rings hold 1024 × 200 ms of stereo audio, about 75 MiB, which is the deliberate cost
/// of never leaving an application out unannounced.
pub const MAX_CAPTURES: usize = VOICE_CONTROL_MAX_SHARE_AUDIO_SOURCES as usize;
/// How often the registry is looked at for a change.
pub const LOOK_EVERY: Duration = Duration::from_millis(50);
/// How often a failure (no audio system, no tree, a capture that would not open) is tried
/// again while the share's sound is still wanted.
pub const RETRY_EVERY: Duration = Duration::from_secs(1);
/// How often the rule is evaluated again whether or not the registry says it changed: the
/// process tree and a deferred stream's owning client can change without a new generation.
pub const REEVALUATE_EVERY: Duration = Duration::from_secs(1);
/// How long a stream whose `application.process.id` is not known yet waits before it is
/// captured. PipeWire lists a stream before its properties are all in, and that advisory pid
/// is how a stream played through `pipewire-pulse` (whose client pid is the proxy's) is known
/// to be Conch's own; a stream that still has none after this is captured. Well inside the
/// first count's limit (`share_audio::FIRST_COUNT_LIMIT`, 1 s).
pub const APP_PID_GRACE: Duration = Duration::from_millis(150);
/// The longest the count is held back while streams wait for their application pid: then it
/// is said anyway, so that streams that keep coming without one never hold it for good.
pub const MAX_COUNT_HOLD: Duration = Duration::from_millis(300);
const _: () = assert!(MAX_COUNT_HOLD.as_millis() == 2 * APP_PID_GRACE.as_millis());
/// How long closing every capture is given, together: within the protocol's 500 ms.
pub const STOP_BUDGET: Duration = Duration::from_millis(400);
/// Frames a capture may have waiting before the oldest are dropped.
pub const BACKLOG_FRAMES: usize = 4;
/// Mixed frames that may wait for the track's audio source.
pub const FRAME_QUEUE: usize = 8;
/// The most frames one wake of the engine makes up for after it was late.
const CATCH_UP_FRAMES: u32 = 5;
/// The stack of a thread that closes one capture.
const STOPPER_STACK: usize = 256 * 1024;

/// What one capture is asked to be: an application output stream, by its `object.serial`,
/// at 48 kHz in two channels. It has no field for a sink, a sink's monitor or a fallback,
/// so none can be asked for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CaptureSpec {
    /// The application stream's `object.serial`.
    pub serial: u64,
    /// Always [`SAMPLE_RATE`].
    pub rate: u32,
    /// Always [`SHARE_CHANNELS`].
    pub channels: u32,
    /// Always [`CAPTURE_RING_MS`].
    pub ring_ms: u32,
}

impl CaptureSpec {
    /// The one way a spec is made: the stream's serial, and the share's format.
    #[must_use]
    pub const fn of_stream(serial: u64) -> Self {
        Self {
            serial,
            rate: SAMPLE_RATE,
            channels: SHARE_CHANNELS,
            ring_ms: CAPTURE_RING_MS,
        }
    }
}

/// The application streams the audio system lists now.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Registry {
    /// Changes whenever the registry does.
    pub generation: u64,
    /// Every application output stream.
    pub streams: Vec<StreamFacts>,
}

/// What a look at the graph found.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Look {
    /// Nothing changed since the generation given.
    Unchanged,
    /// The registry now.
    Changed(Registry),
    /// The audio system is gone.
    Lost,
}

/// A capture that could not be opened. Why is not kept: it may name a stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OpenFailed;

/// The audio system's graph, as the engine uses it. On Linux, PipeWire through `dmn-audio`
/// (`share_capture_linux.rs`); in the tests and behind `--test-share-source`, a synthetic one.
pub trait Graph: Send {
    /// The registry, if it changed since `since` (always, for `None`).
    fn look(&mut self, since: Option<u64>) -> Look;

    /// Opens a capture of one application stream.
    ///
    /// # Errors
    ///
    /// [`OpenFailed`] if the audio system would not open it.
    fn open_capture(&mut self, spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed>;
}

/// How a capture is doing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Health {
    /// Connecting, or the format is not settled yet.
    Waiting,
    /// Delivering 48 kHz stereo.
    Streaming,
    /// It reports delivery at another rate or channel count.
    WrongFormat,
    /// The audio system rejected the negotiated format.
    FormatNotSupported,
    /// The stream it captured is gone, or the capture failed.
    Ended,
}

impl Health {
    fn reason(self) -> StreamReason {
        match self {
            Self::Streaming | Self::Waiting => StreamReason::Captured,
            Self::WrongFormat => StreamReason::WrongFormat,
            Self::FormatNotSupported => StreamReason::FormatNotSupported,
            Self::Ended => StreamReason::StreamEnded,
        }
    }
}

/// What one take from a capture gave.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Taken {
    /// Whether a whole frame was written.
    pub frame: bool,
    /// Frames dropped first because too many were waiting.
    pub dropped: u64,
}

/// One open capture of one application stream.
pub trait Capture: Send {
    /// How it is doing.
    fn health(&self) -> Health;
    /// Writes the next whole frame into `frame` if the capture is streaming in the share's
    /// format and a frame is waiting.
    fn take(&mut self, frame: &mut StereoFrame) -> Taken;
    /// Closes it. May wait for the audio system; called on a thread of its own.
    fn stop(self: Box<Self>);
}

/// Opens the graph: `true` after the audio system was lost, when a connection that saw it go
/// must not be used again. `None` when it cannot be reached.
pub type OpenGraph = Arc<dyn Fn(bool) -> Option<Box<dyn Graph>> + Send + Sync>;

/// Reads the Conch process tree now.
pub type ReadTree = Arc<dyn Fn() -> Result<ProcessTree, NoTree> + Send + Sync>;

/// Where a share's sound can be captured from.
#[derive(Clone)]
pub struct Captures {
    /// The audio system's graph.
    pub graph: OpenGraph,
    /// The Conch process tree.
    pub tree: ReadTree,
    /// This process's pid, whose playback is never captured.
    pub own_pid: Option<u32>,
}

/// How this helper can capture a share's sound, fixed when it starts.
#[derive(Clone)]
pub enum ShareSource {
    /// Not on this platform: a request is `unavailable` with `unsupported`.
    Unsupported,
    /// From this graph.
    Captures(Captures),
}

impl fmt::Debug for ShareSource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ShareSource::Unsupported => f.write_str("Unsupported"),
            ShareSource::Captures(_) => f.write_str("Captures(..)"),
        }
    }
}

/// What the engine says of what it captures.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Report {
    /// This many captures are waiting or streaming in the share's format.
    Sources(usize),
    /// The audio system cannot be reached, or a capture could not be opened: nothing is
    /// captured, and the mix is silence.
    NoPipewire,
    /// What to leave out cannot be worked out: nothing is captured, and the mix is silence.
    NoProcessTree,
    /// Every capture is closed and the engine has ended.
    Closed,
}

/// A report, with the engine it came from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reported {
    /// The engine's number.
    pub generation: u64,
    /// What it said.
    pub report: Report,
    /// The local view of the streams, while the capture route is running.
    pub streams: Option<Arc<[StreamDecision]>>,
}

fn display_label(label: Option<&str>) -> String {
    let label = label.unwrap_or_default();
    if label.contains(['/', '\\']) {
        return String::new();
    }
    let mut bytes = 0;
    label
        .trim()
        .chars()
        .filter(|c| {
            !c.is_control() && !matches!(c, '\u{202a}'..='\u{202e}' | '\u{2066}'..='\u{2069}')
        })
        .take_while(|c| {
            bytes += c.len_utf8() + usize::from(*c == '"');
            bytes <= VOICE_CONTROL_MAX_SHARE_AUDIO_LABEL_BYTES
        })
        .collect()
}

struct Candidate {
    application: String,
    media: String,
    verdict: Verdict,
}

impl Candidate {
    fn reason(&self, capture_reasons: &[(u64, StreamReason)]) -> StreamReason {
        match self.verdict {
            Verdict::Excluded(Exclusion::OwnPlayback) => StreamReason::CallPlayback,
            Verdict::Excluded(Exclusion::ClientInTree) => StreamReason::ConchClient,
            Verdict::Excluded(Exclusion::ApplicationInTree) => StreamReason::ConchApplication,
            Verdict::Excluded(Exclusion::ApplicationName) => StreamReason::ConchName,
            Verdict::Deferred(Deferral::NoClientPid) => StreamReason::UnknownOwner,
            Verdict::Deferred(Deferral::NoSerial) => StreamReason::MissingTarget,
            Verdict::Eligible(serial) => capture_reasons
                .binary_search_by_key(&serial, |&(serial, _)| serial)
                .map_or(StreamReason::AwaitingApplication, |index| {
                    capture_reasons[index].1
                }),
        }
    }
}

/// What the captures have done, for the log and the tests. Counts only.
#[derive(Debug, Default)]
pub struct ShareCounters {
    /// Captures open now.
    pub open: AtomicUsize,
    /// Captures opened, ever.
    pub opened: AtomicU64,
    /// Frames mixed.
    pub mixed: AtomicU64,
    /// Frames dropped: from a ring that had too many waiting, or for a full queue.
    pub dropped: AtomicU64,
    /// Captures whose close took longer than [`STOP_BUDGET`].
    pub late_stops: AtomicU64,
}

enum Command {
    Rename(String),
}

/// The captures of one share's sound and their mix, on a thread of its own.
pub struct Engine {
    commands: std_mpsc::Sender<Command>,
    stop: Arc<AtomicBool>,
    generation: u64,
}

impl Engine {
    /// Starts capturing: the rule is evaluated at once, and then on every change.
    ///
    /// # Errors
    ///
    /// What the operating system said if the thread could not be started; nothing was
    /// opened then.
    pub fn spawn(
        captures: &Captures,
        application_name: String,
        generation: u64,
        frames: mpsc::Sender<StereoFrame>,
        reports: mpsc::UnboundedSender<Reported>,
        counters: Arc<ShareCounters>,
    ) -> std::io::Result<Self> {
        let (commands, received) = std_mpsc::channel();
        let stop = Arc::new(AtomicBool::new(false));
        let worker = Worker {
            open_graph: Arc::clone(&captures.graph),
            tree: Arc::clone(&captures.tree),
            own_pid: captures.own_pid,
            name: application_name,
            graph: None,
            lost: false,
            seen: None,
            captures: BTreeMap::new(),
            capture_reasons: Vec::new(),
            candidates: Vec::new(),
            pending: BTreeMap::new(),
            held_since: None,
            problem: None,
            sent: None,
            generation,
            frames,
            reports,
            counters,
            mixer: StereoMixer::new(),
            scratch: STEREO_SILENCE,
            out: STEREO_SILENCE,
        };
        let stopping = Arc::clone(&stop);
        std::thread::Builder::new()
            .name("conch-voice-share".to_owned())
            .spawn(move || worker.run(&received, &stopping))?;
        Ok(Self {
            commands,
            stop,
            generation,
        })
    }

    /// The engine's number, which its reports carry.
    #[must_use]
    pub fn generation(&self) -> u64 {
        self.generation
    }

    /// Leaves out this application name from now on: the rule is evaluated again.
    pub fn rename(&self, application_name: String) {
        let _ = self.commands.send(Command::Rename(application_name));
    }

    /// Closes every capture and ends; [`Report::Closed`] says when it has.
    pub fn stop(&self) {
        self.stop.store(true, Ordering::SeqCst);
    }
}

impl Drop for Engine {
    fn drop(&mut self) {
        self.stop();
    }
}

impl fmt::Debug for Engine {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Engine")
            .field("generation", &self.generation)
            .finish_non_exhaustive()
    }
}

/// Closes `captures` together, each on a thread of its own, and waits at most `budget` for
/// all of them. Says how many were late; their threads close them all the same.
pub fn stop_group(
    captures: Vec<Box<dyn Capture>>,
    budget: Duration,
    counters: &Arc<ShareCounters>,
) -> usize {
    let count = captures.len();
    if count == 0 {
        return 0;
    }
    let (done, finished) = std_mpsc::channel();
    for capture in captures {
        let closing = Closing {
            capture: Some(capture),
            counters: Arc::clone(counters),
            done: done.clone(),
        };
        // If no thread can be had, the closure is dropped here, and `Closing` closes the
        // capture where it stands.
        let _ = std::thread::Builder::new()
            .name("conch-voice-share-stop".to_owned())
            .stack_size(STOPPER_STACK)
            .spawn(move || drop(closing));
    }
    drop(done);
    let deadline = Instant::now() + budget;
    let mut closed = 0;
    while closed < count {
        let left = deadline.saturating_duration_since(Instant::now());
        match finished.recv_timeout(left) {
            Ok(()) => closed += 1,
            Err(_) => break,
        }
    }
    let late = count - closed;
    if late > 0 {
        counters
            .late_stops
            .fetch_add(late as u64, Ordering::Relaxed);
        log::warn!(
            "share audio: {late} of {count} captures took longer than {} ms to close; they are closing",
            budget.as_millis()
        );
    }
    late
}

/// One capture being closed: closed when this is dropped, on whatever thread drops it.
struct Closing {
    capture: Option<Box<dyn Capture>>,
    counters: Arc<ShareCounters>,
    done: std_mpsc::Sender<()>,
}

impl Drop for Closing {
    fn drop(&mut self) {
        if let Some(capture) = self.capture.take() {
            capture.stop();
            self.counters.open.fetch_sub(1, Ordering::SeqCst);
        }
        let _ = self.done.send(());
    }
}

struct Worker {
    open_graph: OpenGraph,
    tree: ReadTree,
    own_pid: Option<u32>,
    name: String,
    graph: Option<Box<dyn Graph>>,
    /// The audio system was lost: the next graph is reached afresh.
    lost: bool,
    /// The registry's generation last evaluated.
    seen: Option<u64>,
    captures: BTreeMap<u64, Box<dyn Capture>>,
    /// A reusable snapshot in capture serial order, shared by the count and the list.
    capture_reasons: Vec<(u64, StreamReason)>,
    candidates: Vec<Candidate>,
    /// Eligible streams whose application pid is not known yet, with when each was first
    /// seen so: captured once [`APP_PID_GRACE`] has passed without one (`reconcile`).
    pending: BTreeMap<u64, Instant>,
    /// Since when the count has been held back for `pending`, while it is.
    held_since: Option<Instant>,
    /// Why nothing is captured, while that is so.
    problem: Option<Report>,
    /// The report last sent.
    sent: Option<Reported>,
    generation: u64,
    frames: mpsc::Sender<StereoFrame>,
    reports: mpsc::UnboundedSender<Reported>,
    counters: Arc<ShareCounters>,
    mixer: StereoMixer,
    scratch: StereoFrame,
    out: StereoFrame,
}

impl Worker {
    fn run(mut self, commands: &std_mpsc::Receiver<Command>, stop: &AtomicBool) {
        let frame = Duration::from_millis(u64::from(FRAME_MS));
        let mut next_tick = Instant::now();
        let mut next_look = Instant::now();
        let mut next_full = Instant::now() + REEVALUATE_EVERY;
        let mut force = true;
        while !stop.load(Ordering::SeqCst) {
            while let Ok(Command::Rename(name)) = commands.try_recv() {
                self.name = name;
                force = true;
                next_look = Instant::now();
            }
            if Instant::now() >= next_full {
                force = true;
                next_full = Instant::now() + REEVALUATE_EVERY;
            }
            if Instant::now() >= next_look {
                self.look(force, stop);
                // A stream waiting for its application pid is looked at again on the next
                // look, whether or not the registry changes.
                force = self.problem.is_some() || !self.pending.is_empty();
                next_look = Instant::now()
                    + if self.problem.is_some() {
                        RETRY_EVERY
                    } else {
                        LOOK_EVERY
                    };
                self.report();
            }
            let now = Instant::now();
            let mut made = 0;
            while next_tick <= now && made < CATCH_UP_FRAMES {
                self.mix_one();
                next_tick += frame;
                made += 1;
            }
            if next_tick <= now {
                // Too far behind: the rest is not made up for.
                next_tick = now + frame;
            }
            self.report();
            let wake = next_tick.min(next_look);
            std::thread::sleep(wake.saturating_duration_since(Instant::now()));
        }
        self.close_all();
        log::info!(
            "share audio: stopped after {} frames, {} dropped",
            self.counters.mixed.load(Ordering::Relaxed),
            self.counters.dropped.load(Ordering::Relaxed)
        );
        let _ = self.reports.send(Reported {
            generation: self.generation,
            report: Report::Closed,
            streams: None,
        });
    }

    /// Looks at the registry, and when it changed (or `force`), evaluates the rule again.
    fn look(&mut self, force: bool, stop: &AtomicBool) {
        if self.graph.is_none() {
            match (self.open_graph)(self.lost) {
                Some(graph) => {
                    self.graph = Some(graph);
                    self.seen = None;
                    self.lost = false;
                }
                None => {
                    self.fail(Report::NoPipewire, "the audio system cannot be reached");
                    return;
                }
            }
        }
        let since = if force { None } else { self.seen };
        let looked = match self.graph.as_mut() {
            Some(graph) => graph.look(since),
            None => return,
        };
        match looked {
            Look::Unchanged => {}
            Look::Lost => {
                self.graph = None;
                self.lost = true;
                self.fail(Report::NoPipewire, "the audio system was lost");
            }
            Look::Changed(registry) => {
                self.seen = Some(registry.generation);
                self.reconcile(&registry.streams, stop);
            }
        }
    }

    /// Opens a capture of every eligible stream not yet captured and closes every other.
    fn reconcile(&mut self, streams: &[StreamFacts], stop: &AtomicBool) {
        let (tree, own_pid) = match (self.tree)()
            .and_then(|tree| Ok((tree, self.own_pid.ok_or(NoTree::OwnPidUnknown)?)))
        {
            Ok(facts) => facts,
            Err(no) => {
                self.fail(Report::NoProcessTree, no.reason());
                return;
            }
        };
        let rule = Rule {
            own_pid,
            tree: &tree,
            application_name: &self.name,
        };
        let mut wanted = BTreeSet::new();
        self.candidates.clear();
        for stream in streams {
            let verdict = judge(stream, rule);
            if let Verdict::Eligible(serial) = verdict {
                wanted.insert(serial);
            }
            if self.candidates.len() < VOICE_CONTROL_MAX_SHARE_AUDIO_STREAMS {
                self.candidates.push(Candidate {
                    application: display_label(stream.application_name.as_deref()),
                    media: display_label(stream.media_name.as_deref()),
                    verdict,
                });
            }
        }
        if wanted.len() > MAX_CAPTURES {
            log::warn!(
                "share audio: {} streams may be captured, more than {MAX_CAPTURES}",
                wanted.len()
            );
            self.fail(Report::NoPipewire, "too many streams to capture them all");
            return;
        }
        let gone: Vec<u64> = self
            .captures
            .keys()
            .filter(|serial| !wanted.contains(serial))
            .copied()
            .collect();
        let closing: Vec<Box<dyn Capture>> = gone
            .iter()
            .filter_map(|serial| self.captures.remove(serial))
            .collect();
        stop_group(closing, STOP_BUDGET, &self.counters);
        // A stream with no application pid yet may still turn out to be Conch's own when it
        // comes: it waits, from when it was first seen so, for at most `APP_PID_GRACE`.
        let now = Instant::now();
        let waited = std::mem::take(&mut self.pending);
        let mut opening = Vec::with_capacity(wanted.len());
        for serial in wanted {
            if self.captures.contains_key(&serial) {
                continue;
            }
            let pid_known = streams.iter().any(|stream| {
                stream.serial == Some(serial) && stream.application_process_id.is_some()
            });
            let since = waited.get(&serial).copied().unwrap_or(now);
            if pid_known || now.duration_since(since) >= APP_PID_GRACE {
                opening.push(serial);
            } else {
                self.pending.insert(serial, since);
            }
        }
        for serial in opening {
            if stop.load(Ordering::SeqCst) {
                return;
            }
            let Some(graph) = self.graph.as_mut() else {
                return;
            };
            match graph.open_capture(CaptureSpec::of_stream(serial)) {
                Ok(capture) => {
                    self.counters.open.fetch_add(1, Ordering::SeqCst);
                    self.counters.opened.fetch_add(1, Ordering::Relaxed);
                    self.captures.insert(serial, capture);
                }
                Err(OpenFailed) => {
                    self.fail(Report::NoPipewire, "a capture could not be opened");
                    return;
                }
            }
        }
        if self.problem.take().is_some() {
            log::info!("share audio: capturing again");
        }
    }

    /// Nothing can be captured: every capture is closed, and the mix is silence.
    fn fail(&mut self, problem: Report, why: &str) {
        if self.problem != Some(problem) {
            log::warn!("share audio: nothing is captured: {why}");
        }
        self.problem = Some(problem);
        self.pending.clear();
        self.candidates.clear();
        self.close_all();
    }

    fn close_all(&mut self) {
        let all: Vec<Box<dyn Capture>> = std::mem::take(&mut self.captures).into_values().collect();
        stop_group(all, STOP_BUDGET, &self.counters);
    }

    /// One tick: a frame of each capture that has one, mixed, and handed on.
    fn mix_one(&mut self) {
        for capture in self.captures.values_mut() {
            let taken = capture.take(&mut self.scratch);
            if taken.dropped > 0 {
                self.counters
                    .dropped
                    .fetch_add(taken.dropped, Ordering::Relaxed);
            }
            if taken.frame {
                self.mixer.add(&self.scratch);
            }
        }
        self.mixer.finish(&mut self.out);
        self.counters.mixed.fetch_add(1, Ordering::Relaxed);
        if let Err(mpsc::error::TrySendError::Full(_)) = self.frames.try_send(self.out) {
            self.counters.dropped.fetch_add(1, Ordering::Relaxed);
        }
    }

    /// Says what is being captured, if that changed.
    fn report(&mut self) {
        // While a stream waits for its application pid the count is not final: it is said
        // once that wait is over, within `APP_PID_GRACE` and a look, and at the latest after
        // `MAX_COUNT_HOLD` however many keep coming. A failure clears the wait (`fail`), so
        // it is never held back.
        if self.pending.is_empty() {
            self.held_since = None;
        } else if self.held_since.get_or_insert_with(Instant::now).elapsed() < MAX_COUNT_HOLD {
            return;
        }
        self.capture_reasons.clear();
        self.capture_reasons.extend(
            self.captures
                .iter()
                .map(|(&serial, capture)| (serial, capture.health().reason())),
        );
        let report = self.problem.unwrap_or_else(|| {
            Report::Sources(
                self.capture_reasons
                    .iter()
                    .filter(|(_, reason)| *reason == StreamReason::Captured)
                    .count(),
            )
        });
        let decisions = self.candidates.iter().map(|s| {
            (
                s.application.as_str(),
                s.media.as_str(),
                s.reason(&self.capture_reasons),
            )
        });
        if let Some(sent) = &self.sent
            && sent.report == report
            && decisions
                .clone()
                .eq(sent.streams.iter().flat_map(|streams| {
                    streams
                        .iter()
                        .map(|s| (s.application.as_str(), s.media.as_str(), s.reason))
                }))
        {
            return;
        }
        let streams = matches!(report, Report::Sources(_)).then(|| {
            decisions
                .map(|(application, media, reason)| StreamDecision {
                    application: application.to_owned(),
                    media: media.to_owned(),
                    reason,
                })
                .collect()
        });
        let reported = Reported {
            generation: self.generation,
            report,
            streams,
        };
        self.sent = Some(reported.clone());
        let _ = self.reports.send(reported);
    }
}

/// The amplitude of each tone of `--test-share-source`.
pub const SYNTHETIC_AMPLITUDE: f32 = 0.25;
/// The tone of the synthetic stream that stands for this helper's own playback: never in the
/// mix, which is what a test of the published track can listen for.
pub const SYNTHETIC_OWN_TONE_HZ: f32 = 1_250.0;

/// `--test-share-source tone:<hz>[,<hz>...]`: one synthetic application stream per tone, as
/// if other applications were playing them, and one more of [`SYNTHETIC_OWN_TONE_HZ`] owned
/// by this helper, which the rule must leave out. Test only: nothing on the control channel
/// can choose it.
#[derive(Debug, Clone, PartialEq)]
pub struct SyntheticSpec {
    tones: Vec<f32>,
}

impl SyntheticSpec {
    /// The tones of the other applications' streams.
    #[must_use]
    pub fn tones(&self) -> &[f32] {
        &self.tones
    }
}

impl FromStr for SyntheticSpec {
    type Err = String;

    fn from_str(text: &str) -> Result<Self, String> {
        let nyquist = SAMPLE_RATE as f32 / 2.0;
        let usage = || {
            format!("expected tone:<hz>[,<hz>...], each above 0 and below {nyquist} Hz, at most 16")
        };
        let list = text.strip_prefix("tone:").ok_or_else(usage)?;
        let tones = list
            .split(',')
            .map(|hz| match hz.trim().parse::<f32>() {
                Ok(hz) if hz > 0.0 && hz < nyquist => Ok(hz),
                _ => Err(usage()),
            })
            .collect::<Result<Vec<f32>, String>>()?;
        if tones.is_empty() || tones.len() > 16 {
            return Err(usage());
        }
        Ok(Self { tones })
    }
}

/// The pid the synthetic streams' fake parent has: not a real process's.
const SYNTHETIC_PARENT: u32 = u32::MAX - 1;

/// The source behind `--test-share-source`: a synthetic graph, a fake process tree of this
/// helper under a fake parent, and this process's own pid. The rule, the engine, the mix and
/// the track are the real ones.
#[must_use]
pub fn synthetic_source(spec: &SyntheticSpec) -> ShareSource {
    let own = std::process::id();
    let tones = spec.tones.clone();
    let graph: OpenGraph =
        Arc::new(move |_| Some(Box::new(SyntheticGraph::new(own, &tones)) as Box<dyn Graph>));
    let tree: ReadTree = Arc::new(move || {
        ProcessTree::build(Some(own), SYNTHETIC_PARENT, &[(own, SYNTHETIC_PARENT)])
    });
    ShareSource::Captures(Captures {
        graph,
        tree,
        own_pid: Some(own),
    })
}

/// A graph whose streams are tones: the other applications', and this helper's own.
pub struct SyntheticGraph {
    streams: Vec<(StreamFacts, f32)>,
}

impl SyntheticGraph {
    /// The streams of `tones`, each from a process that is not Conch's, and this helper's
    /// own at [`SYNTHETIC_OWN_TONE_HZ`].
    #[must_use]
    pub fn new(own_pid: u32, tones: &[f32]) -> Self {
        let mut streams = vec![(
            StreamFacts {
                serial: Some(1),
                sec_pid: Some(own_pid),
                application_process_id: Some(own_pid),
                application_name: Some("conch-voice".to_owned()),
                media_name: None,
            },
            SYNTHETIC_OWN_TONE_HZ,
        )];
        for (at, hz) in tones.iter().enumerate() {
            let at = u32::try_from(at).unwrap_or(0);
            let pid = SYNTHETIC_PARENT - 1 - at;
            streams.push((
                StreamFacts {
                    serial: Some(u64::from(at) + 2),
                    sec_pid: Some(pid),
                    application_process_id: Some(pid),
                    application_name: Some(format!("synthetic-{at}")),
                    media_name: None,
                },
                *hz,
            ));
        }
        Self { streams }
    }
}

impl Graph for SyntheticGraph {
    fn look(&mut self, since: Option<u64>) -> Look {
        if since == Some(1) {
            return Look::Unchanged;
        }
        Look::Changed(Registry {
            generation: 1,
            streams: self
                .streams
                .iter()
                .map(|(facts, _)| facts.clone())
                .collect(),
        })
    }

    fn open_capture(&mut self, spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed> {
        let hz = self
            .streams
            .iter()
            .find(|(facts, _)| facts.serial == Some(spec.serial))
            .map(|(_, hz)| *hz)
            .ok_or(OpenFailed)?;
        Ok(Box::new(ToneCapture::new(hz, SYNTHETIC_AMPLITUDE)))
    }
}

/// A capture that is a steady stereo tone, a frame whenever it is asked.
#[derive(Debug, Clone)]
pub struct ToneCapture {
    step: f64,
    phase: f64,
    amplitude: f32,
}

impl ToneCapture {
    /// A tone of `hz` at `amplitude`, the same on both channels.
    #[must_use]
    pub fn new(hz: f32, amplitude: f32) -> Self {
        Self {
            step: std::f64::consts::TAU * f64::from(hz) / f64::from(SAMPLE_RATE),
            phase: 0.0,
            amplitude,
        }
    }
}

impl Capture for ToneCapture {
    fn health(&self) -> Health {
        Health::Streaming
    }

    fn take(&mut self, frame: &mut StereoFrame) -> Taken {
        for pair in frame.chunks_exact_mut(2) {
            let sample = self.amplitude * self.phase.sin() as f32;
            pair[0] = sample;
            pair[1] = sample;
            self.phase = (self.phase + self.step) % std::f64::consts::TAU;
        }
        Taken {
            frame: true,
            dropped: 0,
        }
    }

    fn stop(self: Box<Self>) {}
}

// A stereo frame is two of the microphone's frames: the SDK is given it whole.
const _: () = assert!(conch_voice_audio::STEREO_FRAME_LEN == 2 * FRAME_LEN);

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use super::*;
    use crate::share_eligibility::{ProcessTree, eligible};

    const OWN: u32 = 4100;
    const PARENT: u32 = 4000;

    fn tree() -> Result<ProcessTree, NoTree> {
        ProcessTree::build(Some(OWN), PARENT, &[(OWN, PARENT), (4001, PARENT)])
    }

    /// Every spec this module builds is one stream by its serial, in the share's format.
    /// The type itself has nothing else to set: no sink, monitor or fallback.
    #[test]
    fn every_capture_spec_targets_one_stream_by_serial_at_48_khz_stereo() {
        for serial in [0, 1, 2, 77, u64::from(u32::MAX), u64::MAX] {
            let spec = CaptureSpec::of_stream(serial);
            assert_eq!(
                spec,
                CaptureSpec {
                    serial,
                    rate: 48_000,
                    channels: 2,
                    ring_ms: CAPTURE_RING_MS,
                }
            );
        }
        // A spec is these four numbers and nothing more.
        assert_eq!(std::mem::size_of::<CaptureSpec>(), 8 + 3 * 4 + 4);
    }

    #[test]
    fn the_synthetic_source_is_parsed_from_tones_and_refuses_anything_else() {
        let spec: SyntheticSpec = "tone:440,660".parse().unwrap();
        assert_eq!(spec.tones(), [440.0, 660.0]);
        let one: SyntheticSpec = "tone:300".parse().unwrap();
        assert_eq!(one.tones(), [300.0]);
        for bad in [
            "",
            "tone:",
            "tone:0",
            "tone:-5",
            "tone:24000",
            "tone:abc",
            "440",
            "wav:/x",
            "monitor",
            "tone:1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17",
        ] {
            assert!(bad.parse::<SyntheticSpec>().is_err(), "{bad:?}");
        }
    }

    /// A graph a test changes: its streams, whether it is there, and which serials refuse.
    #[derive(Clone, Default)]
    struct Scripted(Arc<Mutex<ScriptedState>>);

    #[derive(Default)]
    struct ScriptedState {
        generation: u64,
        streams: Vec<StreamFacts>,
        lost: bool,
        refuse: Vec<u64>,
        opened: Vec<CaptureSpec>,
        graphs: u32,
        capture_health: Option<Health>,
    }

    impl Scripted {
        fn set(&self, streams: Vec<StreamFacts>) {
            let mut state = self.0.lock().unwrap();
            state.generation += 1;
            state.streams = streams;
        }
    }

    impl Graph for Scripted {
        fn look(&mut self, since: Option<u64>) -> Look {
            let state = self.0.lock().unwrap();
            if state.lost {
                return Look::Lost;
            }
            if since == Some(state.generation) {
                return Look::Unchanged;
            }
            Look::Changed(Registry {
                generation: state.generation,
                streams: state.streams.clone(),
            })
        }

        fn open_capture(&mut self, spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed> {
            let mut state = self.0.lock().unwrap();
            state.opened.push(spec);
            if state.refuse.contains(&spec.serial) {
                return Err(OpenFailed);
            }
            if let Some(health) = state.capture_health {
                return Ok(Box::new(SilentCapture(health)));
            }
            Ok(Box::new(ToneCapture::new(300.0 + spec.serial as f32, 0.1)))
        }
    }

    struct SilentCapture(Health);

    impl Capture for SilentCapture {
        fn health(&self) -> Health {
            self.0
        }

        fn take(&mut self, _frame: &mut StereoFrame) -> Taken {
            Taken::default()
        }

        fn stop(self: Box<Self>) {}
    }

    fn other(serial: u64) -> StreamFacts {
        StreamFacts {
            serial: Some(serial),
            sec_pid: Some(9000 + serial as u32),
            application_process_id: None,
            application_name: Some(format!("app-{serial}")),
            media_name: None,
        }
    }

    struct Running {
        engine: Engine,
        graph: Scripted,
        frames: mpsc::Receiver<StereoFrame>,
        reports: mpsc::UnboundedReceiver<Reported>,
        counters: Arc<ShareCounters>,
        tree_ok: Arc<AtomicBool>,
    }

    fn running(streams: Vec<StreamFacts>) -> Running {
        let graph = Scripted::default();
        graph.set(streams);
        running_with_graph(graph)
    }

    fn running_with_graph(graph: Scripted) -> Running {
        let tree_ok = Arc::new(AtomicBool::new(true));
        let opened = graph.clone();
        let readable = Arc::clone(&tree_ok);
        let captures = Captures {
            graph: Arc::new(move |_| {
                let mut state = opened.0.lock().unwrap();
                if state.lost {
                    return None;
                }
                state.graphs += 1;
                Some(Box::new(opened.clone()) as Box<dyn Graph>)
            }),
            tree: Arc::new(move || {
                if readable.load(Ordering::SeqCst) {
                    tree()
                } else {
                    Err(NoTree::Unreadable)
                }
            }),
            own_pid: Some(OWN),
        };
        let (frames_tx, frames) = mpsc::channel(FRAME_QUEUE);
        let (reports_tx, reports) = mpsc::unbounded_channel();
        let counters = Arc::new(ShareCounters::default());
        let engine = Engine::spawn(
            &captures,
            "Conch".to_owned(),
            7,
            frames_tx,
            reports_tx,
            Arc::clone(&counters),
        )
        .unwrap();
        Running {
            engine,
            graph,
            frames,
            reports,
            counters,
            tree_ok,
        }
    }

    fn next_reported(reports: &mut mpsc::UnboundedReceiver<Reported>) -> Reported {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            match reports.try_recv() {
                Ok(reported) => {
                    assert_eq!(reported.generation, 7);
                    return reported;
                }
                Err(_) => {
                    assert!(Instant::now() < deadline, "no report");
                    std::thread::sleep(Duration::from_millis(2));
                }
            }
        }
    }

    fn next_report(reports: &mut mpsc::UnboundedReceiver<Reported>) -> Report {
        next_reported(reports).report
    }

    fn until_report(reports: &mut mpsc::UnboundedReceiver<Reported>, want: Report) {
        loop {
            if next_report(reports) == want {
                return;
            }
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn a_rejected_format_has_its_own_reason_and_is_not_counted() {
        use dmn_audio::{CaptureState, StreamError};

        let cases = [
            (
                StreamError::Format("need F32LE".into()),
                StreamReason::FormatNotSupported,
            ),
            (
                StreamError::RateMismatch {
                    requested: SAMPLE_RATE,
                    negotiated: 44_100,
                },
                StreamReason::FormatNotSupported,
            ),
            (
                StreamError::ChannelMismatch {
                    requested: SHARE_CHANNELS,
                    negotiated: 1,
                },
                StreamReason::FormatNotSupported,
            ),
            (
                StreamError::Stream("stream broke".into()),
                StreamReason::StreamEnded,
            ),
        ];
        for (error, reason) in cases {
            let graph = Scripted::default();
            graph.set(vec![StreamFacts {
                application_process_id: Some(9002),
                ..other(2)
            }]);
            graph.0.lock().unwrap().capture_health = Some(linux::health(
                CaptureState::Error,
                None,
                (SAMPLE_RATE, SHARE_CHANNELS),
                || Err(error),
            ));
            let mut run = running_with_graph(graph);
            let reported = next_reported(&mut run.reports);
            assert_eq!(reported.report, Report::Sources(0), "{reason:?}");
            let streams = reported.streams.unwrap();
            assert_eq!(streams.len(), 1);
            assert_eq!(streams[0].reason, reason);
            run.engine.stop();
            until_report(&mut run.reports, Report::Closed);
        }
    }

    #[test]
    fn streams_are_captured_as_they_come_and_go_and_never_the_helpers_own() {
        let own = StreamFacts {
            serial: Some(1),
            sec_pid: Some(OWN),
            application_process_id: Some(OWN),
            application_name: Some("conch-voice".into()),
            media_name: None,
        };
        let mut run = running(vec![own.clone(), other(2), other(3)]);
        until_report(&mut run.reports, Report::Sources(2));
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 2);

        // One quits, one starts, and the owner's application starts playing: still two.
        let mut conch = other(5);
        conch.application_name = Some("Conch".into());
        run.graph.set(vec![own.clone(), other(3), other(4), conch]);
        let deadline = Instant::now() + Duration::from_secs(10);
        while !run
            .graph
            .0
            .lock()
            .unwrap()
            .opened
            .iter()
            .any(|spec| spec.serial == 4)
        {
            assert!(Instant::now() < deadline, "the new stream was not captured");
            std::thread::sleep(Duration::from_millis(2));
        }
        // Nothing is playing.
        run.graph.set(vec![own]);
        until_report(&mut run.reports, Report::Sources(0));
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);

        let opened: Vec<u64> = run
            .graph
            .0
            .lock()
            .unwrap()
            .opened
            .iter()
            .map(|spec| spec.serial)
            .collect();
        assert_eq!(opened, [2, 3, 4], "the own stream and the owner's never");
        for spec in &run.graph.0.lock().unwrap().opened {
            assert_eq!(*spec, CaptureSpec::of_stream(spec.serial));
        }
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
    }

    fn was_opened(run: &Running, serial: u64) -> bool {
        run.graph
            .0
            .lock()
            .unwrap()
            .opened
            .iter()
            .any(|spec| spec.serial == serial)
    }

    /// A stream whose application pid is not known yet is not captured at once: it waits
    /// `APP_PID_GRACE`. One whose pid comes in that time and is Conch's own is never captured;
    /// one that never gets a pid is captured once the wait is over. The count is said only
    /// once nothing waits.
    #[test]
    fn a_stream_with_no_application_pid_waits_and_one_that_turns_out_ours_is_never_captured() {
        let began = Instant::now();
        let mut late = other(6);
        let mut run = running(vec![late.clone()]);
        // The pid arrives, and it is the renderer's: Conch's own playback through the proxy.
        late.application_process_id = Some(4001);
        run.graph.set(vec![late]);
        if began.elapsed() < APP_PID_GRACE {
            std::thread::sleep(APP_PID_GRACE * 4);
            assert!(!was_opened(&run, 6), "Conch's own stream was captured");
            until_report(&mut run.reports, Report::Sources(0));
        } else {
            eprintln!("too slow to test the grace: the pid came after it");
        }

        // A stream that never gets one is captured, after the wait.
        let shown = Instant::now();
        run.graph.set(vec![other(7)]);
        until_report(&mut run.reports, Report::Sources(1));
        assert!(was_opened(&run, 7));
        assert!(
            shown.elapsed() >= APP_PID_GRACE,
            "captured before its wait was over"
        );
        // One whose pid is known from the start is not held back.
        let mut known = other(8);
        known.application_process_id = Some(9008);
        run.graph.set(vec![other(7), known]);
        until_report(&mut run.reports, Report::Sources(2));
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
    }

    /// Streams that keep coming without an application pid never hold the count back for
    /// good: it is said within `MAX_COUNT_HOLD` of the first wait, while they still come.
    #[test]
    fn a_churn_of_streams_without_a_pid_holds_the_count_back_only_so_long() {
        let mut known = other(8);
        known.application_process_id = Some(9008);
        let mut run = running(vec![known.clone(), other(100)]);
        let churning = Arc::new(AtomicBool::new(true));
        let churn = {
            let graph = run.graph.clone();
            let churning = Arc::clone(&churning);
            let known = known.clone();
            std::thread::spawn(move || {
                let mut serial = 101;
                while churning.load(Ordering::SeqCst) {
                    // A new stream with no pid every 40 ms, each gone before its wait ends.
                    graph.set(vec![known.clone(), other(serial)]);
                    serial += 1;
                    std::thread::sleep(Duration::from_millis(40));
                }
            })
        };
        let began = Instant::now();
        until_report(&mut run.reports, Report::Sources(1));
        let took = began.elapsed();
        churning.store(false, Ordering::SeqCst);
        churn.join().unwrap();
        assert!(
            took < MAX_COUNT_HOLD + Duration::from_secs(1),
            "the count was held back {took:?}"
        );
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
    }

    #[test]
    fn the_mix_is_the_sources_summed_and_silence_with_none() {
        let mut run = running(vec![other(2)]);
        until_report(&mut run.reports, Report::Sources(1));
        // Frames made before the capture opened are silence; after, the tone.
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut heard = false;
        while !heard {
            assert!(Instant::now() < deadline, "the tone never reached the mix");
            if let Ok(frame) = run.frames.try_recv() {
                heard = frame.iter().any(|sample| *sample != 0.0);
                assert!(frame.iter().all(|sample| (-1.0..=1.0).contains(sample)));
                // Both channels carry the same tone.
                for pair in frame.chunks_exact(2) {
                    assert_eq!(pair[0], pair[1]);
                }
            } else {
                std::thread::sleep(Duration::from_millis(2));
            }
        }
        run.graph.set(Vec::new());
        until_report(&mut run.reports, Report::Sources(0));
        // Drain what was mixed before, then everything is silence.
        std::thread::sleep(Duration::from_millis(50));
        while run.frames.try_recv().is_ok() {}
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut silent = 0;
        while silent < 5 {
            assert!(Instant::now() < deadline);
            if let Ok(frame) = run.frames.try_recv() {
                assert_eq!(frame, STEREO_SILENCE);
                silent += 1;
            } else {
                std::thread::sleep(Duration::from_millis(2));
            }
        }
    }

    /// A slow reader of the mix loses frames, counted; the engine does not wait for it.
    #[test]
    fn a_full_queue_drops_frames_and_counts_them() {
        let mut run = running(vec![other(2)]);
        until_report(&mut run.reports, Report::Sources(1));
        let deadline = Instant::now() + Duration::from_secs(10);
        while run.counters.dropped.load(Ordering::Relaxed) == 0 {
            assert!(Instant::now() < deadline, "nothing was dropped");
            std::thread::sleep(Duration::from_millis(5));
        }
        // The mix kept going all the while.
        let mixed = run.counters.mixed.load(Ordering::Relaxed);
        assert!(mixed > FRAME_QUEUE as u64);
        drop(run.frames);
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
    }

    #[test]
    fn without_a_tree_every_capture_is_closed_until_there_is_one() {
        let mut run = running(vec![other(2), other(3)]);
        until_report(&mut run.reports, Report::Sources(2));
        run.tree_ok.store(false, Ordering::SeqCst);
        // The next registry change reads the tree again.
        run.graph.set(vec![other(2), other(3)]);
        until_report(&mut run.reports, Report::NoProcessTree);
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);
        run.tree_ok.store(true, Ordering::SeqCst);
        until_report(&mut run.reports, Report::Sources(2));
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn a_capture_that_will_not_open_closes_all_and_the_lost_system_is_reached_afresh() {
        let mut run = running(vec![other(2), other(3)]);
        until_report(&mut run.reports, Report::Sources(2));

        // One more stream, whose capture is refused: nothing at all, not some.
        run.graph.0.lock().unwrap().refuse.push(4);
        run.graph.set(vec![other(2), other(3), other(4)]);
        until_report(&mut run.reports, Report::NoPipewire);
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);
        run.graph.0.lock().unwrap().refuse.clear();
        until_report(&mut run.reports, Report::Sources(3));

        // The audio system goes, and comes back.
        run.graph.0.lock().unwrap().lost = true;
        until_report(&mut run.reports, Report::NoPipewire);
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);
        run.graph.0.lock().unwrap().lost = false;
        until_report(&mut run.reports, Report::Sources(3));
        assert!(run.graph.0.lock().unwrap().graphs >= 2, "reached afresh");
        run.engine.stop();
        until_report(&mut run.reports, Report::Closed);
    }

    #[test]
    fn a_new_application_name_is_left_out_at_once() {
        let mut run = running(vec![other(2), other(3)]);
        until_report(&mut run.reports, Report::Sources(2));
        run.engine.rename("app-3".to_owned());
        until_report(&mut run.reports, Report::Sources(1));
        drop(run.engine);
        until_report(&mut run.reports, Report::Closed);
        assert_eq!(run.counters.open.load(Ordering::SeqCst), 0);
    }

    /// A capture that takes 100 ms to close, as `dmn-audio`'s may.
    struct Slow(Arc<AtomicUsize>);

    impl Capture for Slow {
        fn health(&self) -> Health {
            Health::Streaming
        }
        fn take(&mut self, _frame: &mut StereoFrame) -> Taken {
            Taken::default()
        }
        fn stop(self: Box<Self>) {
            std::thread::sleep(Duration::from_millis(100));
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// Many captures that each take 100 ms to close are closed together within the budget,
    /// not one after another.
    #[test]
    fn captures_are_closed_as_a_group_within_the_budget() {
        let closed = Arc::new(AtomicUsize::new(0));
        let counters = Arc::new(ShareCounters::default());
        let count = 64;
        counters.open.store(count, Ordering::SeqCst);
        let captures: Vec<Box<dyn Capture>> = (0..count)
            .map(|_| Box::new(Slow(Arc::clone(&closed))) as Box<dyn Capture>)
            .collect();
        let began = Instant::now();
        let late = stop_group(captures, STOP_BUDGET, &counters);
        assert_eq!(late, 0);
        assert!(
            began.elapsed() < Duration::from_millis(500),
            "{:?}",
            began.elapsed()
        );
        assert_eq!(closed.load(Ordering::SeqCst), count);
        assert_eq!(counters.open.load(Ordering::SeqCst), 0);
    }

    /// One that is late is counted, and still closed.
    #[test]
    fn a_late_close_is_counted_and_still_happens() {
        let closed = Arc::new(AtomicUsize::new(0));
        let counters = Arc::new(ShareCounters::default());
        counters.open.store(1, Ordering::SeqCst);
        let late = stop_group(
            vec![Box::new(Slow(Arc::clone(&closed)))],
            Duration::from_millis(10),
            &counters,
        );
        assert_eq!(late, 1);
        assert_eq!(counters.late_stops.load(Ordering::Relaxed), 1);
        // The close bumps `closed` inside stop() and `open` only after it
        // returns (Closing::drop): wait for both, or the read races (#1942).
        let deadline = Instant::now() + Duration::from_secs(5);
        while closed.load(Ordering::SeqCst) == 0 || counters.open.load(Ordering::SeqCst) != 0 {
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(closed.load(Ordering::SeqCst), 1);
        assert_eq!(counters.open.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn the_synthetic_graph_offers_the_tones_and_the_helpers_own_which_is_left_out() {
        let spec: SyntheticSpec = "tone:440,660".parse().unwrap();
        let ShareSource::Captures(captures) = synthetic_source(&spec) else {
            panic!("synthetic is a capture source");
        };
        let mut graph = (captures.graph)(false).unwrap();
        let Look::Changed(registry) = graph.look(None) else {
            panic!("the first look gives the registry");
        };
        assert_eq!(registry.streams.len(), 3);
        assert_eq!(graph.look(Some(registry.generation)), Look::Unchanged);
        let tree = (captures.tree)().unwrap();
        let wanted = eligible(&registry.streams, Ok(&tree), captures.own_pid, "Conch").unwrap();
        assert_eq!(wanted, [2, 3], "the helper's own tone is never captured");
        assert!(graph.open_capture(CaptureSpec::of_stream(99)).is_err());
        let mut capture = graph.open_capture(CaptureSpec::of_stream(2)).unwrap();
        let mut frame = STEREO_SILENCE;
        assert!(capture.take(&mut frame).frame);
        assert!(frame.iter().any(|sample| *sample != 0.0));
        assert!(
            frame
                .iter()
                .all(|sample| sample.abs() <= SYNTHETIC_AMPLITUDE)
        );
    }
}
