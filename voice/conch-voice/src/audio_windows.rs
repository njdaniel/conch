//! The Windows backend's own logic: which endpoint a microphone or speaker follows, what it
//! does when one goes away or the default moves, and how audio gets between the sound card's
//! thread and the rest of the helper. Everything that needs no WASAPI is here, behind two small
//! traits ([`Audio`], [`Stream`]), so that it is compiled and tested on every platform; the
//! WASAPI side of the two traits is `audio_wasapi.rs`, which only Windows compiles
//! (`docs/design/windows-desktop.md` §2, issue #1774).
//!
//! Input: the endpoints the system has (an [`Audio`]), its notifications ([`Notice`]), and the
//! microphone or speakers asked for: the default, or an endpoint ID. Output: a [`MicSource`]
//! ([`WasapiMic`]) that gives captured audio a frame at a time, and a [`SpeakerSink`]
//! ([`WasapiSpeakers`]) that takes the mix a frame at a time, and says whether it is playing
//! ([`Speakers`]); both mono `f32` at 48 kHz in 10 ms frames, as everything inside is. Owns:
//! one worker thread for each microphone or speakers, from the moment it is opened until it is
//! dropped, and the ring between that thread and the helper.
//!
//! What runs where. The worker is the only thread that touches the sound system: it makes the
//! system's objects, waits on the stream's event, moves samples between the stream and the
//! ring, and drops them. The transmit and mix tasks are on the other side of the ring and never
//! call the system. The system's notifications are called on a thread of the system's own and
//! only [`post`] a [`Notice`] to the worker and return: a notice is sent on an unbounded
//! channel, which never blocks, and what to do about it is decided by the worker.
//!
//! The ring is a `VecDeque` behind a `Mutex`, not a lock-free ring as on Linux: this backend
//! may add no crate, and the worker is a thread of ours, not a callback of the sound card's, so
//! a lock held for the copy of a frame costs nothing the card's deadline depends on.
//!
//! What paces them. Both devices say [`Pacing::Ring`]: the card's clock sets the pace. The
//! transmit task forwards a frame whenever the capture ring holds a whole one, and the mix task
//! mixes a frame whenever the playback ring holds less than [`TARGET_FILL`] ([`frames_wanted`]).
//! A late look catches up by handling several frames; nothing is dropped and no silence is added
//! to correct for drift.
//!
//! The device rules (`docs/design/tui-voice.md` §20.5), here:
//!
//! - **A named device** is an endpoint ID. If it is not there, or goes away
//!   (`AUDCLNT_E_DEVICE_INVALIDATED`, a removal, a state other than active), the stream is
//!   dropped, the microphone says [`MicRead::NoMicrophone`] (so the gate shuts with no tail),
//!   and nothing else is opened in its place. It is opened again when it returns.
//! - **Following `default`** moves to the new default when the system says it changed. A
//!   transmission in progress ends first: the microphone says `NoMicrophone` once, whatever
//!   moved, before it says anything captured from the new device ([`Shared::epoch`]).
//! - What the worker should do is decided from what is there now ([`decide`]), not from what
//!   a notice said, so a notice that is late, doubled or missing cannot leave it following a
//!   device it should not.
//!
//! The decisions that are this file's own are small functions of plain values or of a fake
//! [`Audio`], so that they are tested without a sound card: [`decide`], [`take_frame`],
//! [`put_frame`], [`frames_wanted`], [`Worker::step`] and the conversions.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering};
use std::sync::mpsc::{self, Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use conch_voice_audio::{FRAME_LEN, FRAME_MS, Frame, MicRead, MicSource, Pacing, SpeakerSink};

use crate::devices::{DeviceEntry, DeviceTable};
use crate::error::Error;
use crate::receive::Speakers;

/// How long a listing would wait. Unused: `devices` answers `Unsupported` on Windows until
/// the device list has a schema for it (#891). Kept for the name the callers use.
pub const LISTING_WAIT: Duration = Duration::from_secs(3);

/// How much audio the capture ring holds: it has to outlast a look that is late. A stall of
/// the runtime longer than this loses the oldest audio, which [`Shared::dropped`] counts.
const CAPTURE_RING: usize = 20 * FRAME_LEN;
/// How much audio the playback ring holds. What is queued when a named speaker is unplugged
/// is played when it returns, so this is short; [`TARGET_FILL`] keeps what is in it shorter.
const PLAYBACK_RING: usize = 20 * FRAME_LEN;
/// How much the playback ring is kept filled to: a mix is made whenever it holds less. It is
/// the delay this side adds, and the room a late look has before the card finds it dry. The
/// engine's period is 10 ms by default and its buffer a few of them, so a few frames; it is
/// kept there by the card's clock, not by a count of ticks, so it does not drift.
const TARGET_FILL: usize = 4 * FRAME_LEN;

/// How long the worker waits for the stream's event, so that it looks at its notices and at
/// whether it has been dropped at least this often.
const TICK: Duration = Duration::from_millis(20);
/// How long after a device could not be started, or went away, the worker tries again by
/// itself. A notice makes it try at once; this is for a notice that never comes.
const RETRY: Duration = Duration::from_secs(1);
/// How often the worker looks at what the system has when it could not subscribe to its
/// notifications.
const POLL: Duration = Duration::from_secs(2);
/// How long opening a microphone or speakers waits for the first answer of the worker, which
/// says whether the device could be started, so that a refusal can fail the open. The open
/// runs on the session loop, so this is short rather than the open being made elsewhere: a
/// device starts in tens of milliseconds, and one that is slower is taken to be starting, its
/// refusal, if one comes, surfacing as the `Denied` or `Lost` state and a log line.
const FIRST_ANSWER: Duration = Duration::from_millis(250);

/// What Windows says to a person whose microphone is blocked. The setting is one switch for
/// all desktop apps: this program cannot be allowed on its own.
pub const PRIVACY_TEXT: &str = "Windows is not letting programs use the microphone: turn on \
    Settings > Privacy & security > Microphone > Let desktop apps access your microphone";

// ---------------------------------------------------------------------------------------------
// What the system gives.

/// Which way audio goes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// A microphone.
    Capture,
    /// Speakers.
    Render,
}

/// One audio endpoint the system lists.
#[derive(Clone, PartialEq, Eq)]
pub struct Endpoint {
    /// The endpoint ID: what the configuration names, and what Microsoft says to treat as
    /// opaque.
    pub id: String,
    /// What the endpoint calls itself, for people.
    pub name: String,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Names are never logged (tui-voice.md §20.7).
        f.debug_struct("Endpoint").finish_non_exhaustive()
    }
}

/// Why the system could not do something, sorted into what the worker does about it. It holds
/// nothing the system said about a device.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Failure {
    /// The device is not there, or has gone away (`AUDCLNT_E_DEVICE_INVALIDATED`, an ID that
    /// is no endpoint).
    Gone,
    /// Windows refused access (`E_ACCESSDENIED`): the microphone privacy setting.
    Denied,
    /// The engine would not take 48 kHz `f32` audio, in mono or in stereo.
    Unsupported,
    /// Anything else, as one line of this program's own words.
    Other(String),
}

/// Sorts an `HRESULT` into a [`Failure`].
#[must_use]
pub fn failure_from_hresult(code: i32) -> Failure {
    // The bit patterns of the codes as Microsoft writes them (audioclient.h, winerror.h).
    match code as u32 {
        // AUDCLNT_E_DEVICE_INVALIDATED, and ERROR_NOT_FOUND / E_NOTFOUND from an endpoint
        // that is no longer there (there being no default endpoint is one).
        0x8889_0004 | 0x8007_0490 => Failure::Gone,
        // E_ACCESSDENIED.
        0x8007_0005 => Failure::Denied,
        // AUDCLNT_E_UNSUPPORTED_FORMAT.
        0x8889_0008 => Failure::Unsupported,
        other => Failure::Other(format!("the sound system answered {other:#010x}")),
    }
}

/// What the system says has changed. Posted from the system's own thread.
#[derive(Clone, PartialEq, Eq)]
pub enum Notice {
    /// An endpoint was added.
    Added(String),
    /// An endpoint was removed.
    Removed(String),
    /// An endpoint changed state (active, disabled, unplugged, not present).
    State(String),
    /// The default changed: to this endpoint, or to none.
    DefaultChanged(Option<String>),
}

impl std::fmt::Debug for Notice {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The IDs are left out: names are never logged (tui-voice.md §20.7).
        f.write_str(match self {
            Notice::Added(_) => "Notice::Added",
            Notice::Removed(_) => "Notice::Removed",
            Notice::State(_) => "Notice::State",
            Notice::DefaultChanged(_) => "Notice::DefaultChanged",
        })
    }
}

impl Notice {
    /// Whether the worker has anything to look at because of this notice: the endpoint it is
    /// running on, or the one it was told to follow, changed; or it is running on none and
    /// something came; or it follows the default and the default moved.
    #[must_use]
    pub fn concerns(&self, target: &Target, running: Option<&str>) -> bool {
        let named = |id: &String| matches!(target, Target::Named(want) if want == id);
        match self {
            Notice::Removed(id) | Notice::State(id) => named(id) || running == Some(id.as_str()),
            Notice::Added(id) => named(id) || running.is_none(),
            Notice::DefaultChanged(_) => matches!(target, Target::Default),
        }
    }
}

/// How many notices wait for the worker at most.
const NOTICES: usize = 64;

/// The system's end of the notice channel. Cloning shares it.
#[derive(Clone)]
pub struct Poster {
    to: SyncSender<Notice>,
    /// Set when a notice was lost because the channel was full: the worker then looks at
    /// what the system has whatever the notices said.
    full: Arc<AtomicBool>,
}

/// The worker's end of the notice channel.
pub struct Inbox {
    from: Receiver<Notice>,
    full: Arc<AtomicBool>,
}

/// A bounded channel for notices: the system's thread never waits on it, and a flood of
/// notices costs one look and not memory.
#[must_use]
pub fn notice_channel() -> (Poster, Inbox) {
    let (to, from) = mpsc::sync_channel(NOTICES);
    let full = Arc::new(AtomicBool::new(false));
    (
        Poster {
            to,
            full: Arc::clone(&full),
        },
        Inbox { from, full },
    )
}

/// Posts a notice to the worker and returns. It never waits: a full channel sets the flag
/// that makes the worker look anyway, and a worker that is gone is not an error for the
/// system's thread.
pub fn post(to: &Poster, notice: Notice) {
    match to.to.try_send(notice) {
        Ok(()) => {}
        Err(TrySendError::Full(_)) => to.full.store(true, Ordering::Release),
        Err(TrySendError::Disconnected(_)) => {}
    }
}

/// The notice for a default that changed, if it is one the side follows: the kind of audio is
/// the one the side is, and the role is the console's, the one Windows calls the default.
#[must_use]
pub fn default_changed(
    side: Kind,
    flow: Kind,
    console_role: bool,
    id: Option<String>,
) -> Option<Notice> {
    (side == flow && console_role).then_some(Notice::DefaultChanged(id))
}

/// The sound system, as one worker thread uses it. Not `Send`: it is made in the worker's
/// thread, where the system's objects live their whole life.
pub trait Audio {
    /// The stream this system gives.
    type Stream: Stream;

    /// The active endpoints of a kind.
    ///
    /// # Errors
    ///
    /// The system could not be asked.
    fn endpoints(&mut self, kind: Kind) -> Result<Vec<Endpoint>, Failure>;

    /// The IDs of the active endpoints of a kind, and nothing else about them: what the
    /// worker looks at whenever something changed, which has no use for a friendly name.
    ///
    /// # Errors
    ///
    /// The system could not be asked.
    fn active_ids(&mut self, kind: Kind) -> Result<Vec<String>, Failure>;

    /// The ID of the default endpoint of a kind, if there is one.
    ///
    /// # Errors
    ///
    /// The system could not be asked.
    fn default_id(&mut self, kind: Kind) -> Result<Option<String>, Failure>;

    /// Starts a shared, event-driven stream of 48 kHz audio on an endpoint.
    ///
    /// # Errors
    ///
    /// Why it could not be started.
    fn start(&mut self, kind: Kind, id: &str) -> Result<Self::Stream, Failure>;

    /// Has the system post its notices to `to`, until this is dropped.
    ///
    /// # Errors
    ///
    /// The system would not take the subscription.
    fn subscribe(&mut self, kind: Kind, to: Poster) -> Result<(), Failure>;
}

/// A running stream: the sound card's side of a ring.
pub trait Stream {
    /// Waits up to `wait` for the stream's event, then hands over whatever has been captured,
    /// as mono samples.
    ///
    /// # Errors
    ///
    /// The stream failed: [`Failure::Gone`] if its device went away.
    fn capture(&mut self, wait: Duration, deliver: &mut dyn FnMut(&[f32])) -> Result<(), Failure>;

    /// Waits up to `wait` for the stream's event, then asks `supply` to fill what room the
    /// stream has with mono samples, and plays as many as it gave (it says how many).
    ///
    /// # Errors
    ///
    /// The stream failed: [`Failure::Gone`] if its device went away.
    fn render(
        &mut self,
        wait: Duration,
        supply: &mut dyn FnMut(&mut [f32]) -> usize,
    ) -> Result<(), Failure>;
}

/// The endpoints and the defaults of both kinds as the neutral table `devices` formats. An
/// endpoint whose ID cannot be written into `voice.toml` is in the table all the same: the
/// rule that counts it as unshown and leaves it out of the lists is `devices.rs`'s, the one
/// PipeWire's names go through.
///
/// # Errors
///
/// The system could not be asked.
pub fn list<A: Audio>(audio: &mut A) -> Result<DeviceTable, Failure> {
    let entries = |endpoints: Vec<Endpoint>| -> Vec<DeviceEntry> {
        endpoints
            .into_iter()
            .map(|endpoint| DeviceEntry {
                name: endpoint.id,
                description: endpoint.name,
            })
            .collect()
    };
    Ok(DeviceTable {
        synced: true,
        sources: entries(audio.endpoints(Kind::Capture)?),
        sinks: entries(audio.endpoints(Kind::Render)?),
        default_source: audio.default_id(Kind::Capture)?,
        default_sink: audio.default_id(Kind::Render)?,
    })
}

// ---------------------------------------------------------------------------------------------
// Which endpoint.

/// What a microphone or speakers were asked to follow.
#[derive(Clone, PartialEq, Eq)]
pub enum Target {
    /// The system's default, as it changes.
    Default,
    /// This endpoint ID, and no other.
    Named(String),
}

impl std::fmt::Debug for Target {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The ID is left out: names are never logged (tui-voice.md §20.7).
        f.write_str(match self {
            Target::Default => "Target::Default",
            Target::Named(_) => "Target::Named",
        })
    }
}

impl Target {
    /// What a configured name asks for: nothing, an empty name or `default` is the default.
    #[must_use]
    pub fn from_config(configured: Option<&str>) -> Self {
        match configured {
            None | Some("" | "default") => Target::Default,
            Some(id) => Target::Named(id.to_owned()),
        }
    }
}

/// The endpoint to be running on, given what is there. A named endpoint is wanted only while
/// it is listed as active: if it is not, nothing is wanted, and in particular not the default.
/// The default is wanted only if the system lists it.
#[must_use]
pub fn decide(target: &Target, active: &[String], default: Option<&str>) -> Option<String> {
    let listed = |id: &str| active.iter().any(|listed| listed == id);
    match target {
        Target::Named(id) => listed(id).then(|| id.clone()),
        Target::Default => default.filter(|id| listed(id)).map(str::to_owned),
    }
}

// ---------------------------------------------------------------------------------------------
// The ring and what is shared with the worker.

/// What the stream is doing, as the microphone and speakers see it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    /// The worker has not yet found out.
    Starting,
    /// The stream is up.
    Streaming,
    /// There is no such device now: the one that was named is not plugged in, or there is no
    /// default.
    NoDevice,
    /// Windows refused access.
    Denied,
    /// The system failed in some other way, and the worker is trying again.
    Lost,
}

impl State {
    fn code(self) -> u8 {
        match self {
            State::Starting => 0,
            State::Streaming => 1,
            State::NoDevice => 2,
            State::Denied => 3,
            State::Lost => 4,
        }
    }

    fn from_code(code: u8) -> Self {
        match code {
            1 => State::Streaming,
            2 => State::NoDevice,
            3 => State::Denied,
            4 => State::Lost,
            _ => State::Starting,
        }
    }

    /// Words for a log line: nothing about which device.
    fn why(self) -> &'static str {
        match self {
            State::Starting => "starting",
            State::Streaming => "streaming",
            State::NoDevice => "the device is not there",
            State::Denied => "Windows refused access",
            State::Lost => "the sound system failed",
        }
    }
}

/// Samples between the worker and the helper.
#[derive(Debug)]
struct Ring {
    samples: VecDeque<f32>,
    capacity: usize,
}

impl Ring {
    fn new(capacity: usize) -> Self {
        Self {
            samples: VecDeque::with_capacity(capacity),
            capacity,
        }
    }

    fn queued(&self) -> usize {
        self.samples.len()
    }

    fn vacant(&self) -> usize {
        self.capacity.saturating_sub(self.samples.len())
    }

    fn clear(&mut self) {
        self.samples.clear();
    }

    /// Appends `samples`, dropping the oldest to make room, and says how many it dropped.
    fn push_overwriting(&mut self, samples: &[f32]) -> usize {
        self.samples.extend(samples.iter().copied());
        let over = self.samples.len().saturating_sub(self.capacity);
        self.samples.drain(..over);
        over
    }

    /// Appends all of `samples` or none of them.
    fn push_whole(&mut self, samples: &[f32]) -> bool {
        if self.vacant() < samples.len() {
            return false;
        }
        self.samples.extend(samples.iter().copied());
        true
    }

    /// Moves up to `out.len()` samples into `out`, oldest first, and says how many.
    fn pop(&mut self, out: &mut [f32]) -> usize {
        let mut n = 0;
        for slot in out.iter_mut() {
            match self.samples.pop_front() {
                Some(sample) => *slot = sample,
                None => break,
            }
            n += 1;
        }
        n
    }
}

/// What a microphone or speakers share with their worker.
#[derive(Debug)]
struct Shared {
    state: AtomicU8,
    /// Counts the streams that were dropped. A microphone that sees it change says
    /// `NoMicrophone` once before anything else: the device moved or went away, and a
    /// transmission in progress ends. It never goes back.
    epoch: AtomicU64,
    /// Set by the handle's drop: the worker ends.
    stop: AtomicBool,
    /// Captured samples lost because the ring was full.
    dropped: AtomicU64,
    ring: Mutex<Ring>,
}

impl Shared {
    fn new(capacity: usize) -> Self {
        Self {
            state: AtomicU8::new(State::Starting.code()),
            epoch: AtomicU64::new(0),
            stop: AtomicBool::new(false),
            dropped: AtomicU64::new(0),
            ring: Mutex::new(Ring::new(capacity)),
        }
    }

    fn state(&self) -> State {
        State::from_code(self.state.load(Ordering::Acquire))
    }

    fn set_state(&self, state: State) {
        self.state.store(state.code(), Ordering::Release);
    }

    fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    fn ring(&self) -> MutexGuard<'_, Ring> {
        // A panic while the lock was held cannot leave the ring in a state that matters:
        // it holds samples and nothing else.
        self.ring.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

// ---------------------------------------------------------------------------------------------
// The worker.

/// A stream and the endpoint it runs on.
struct Running<S> {
    id: String,
    stream: S,
}

/// What a step of the worker found.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stepped {
    /// A stream is up and was waited on.
    Pumped,
    /// There is no stream: the caller should wait before it steps again.
    Idle,
}

/// The one thread that touches the sound system for one microphone or speakers.
struct Worker<A: Audio> {
    // Declaration order is drop order: the stream goes before the system it came from.
    stream: Option<Running<A::Stream>>,
    audio: A,
    kind: Kind,
    target: Target,
    shared: Arc<Shared>,
    notices: Inbox,
    /// Look at what the system has at the next step.
    dirty: bool,
    /// How many streams in a row went away at once after being started.
    gone_streak: u32,
    /// When to look again by itself, while there is no stream.
    retry_at: Option<Instant>,
    /// When to look again by itself because no notice can come, if that is so.
    poll_at: Option<Instant>,
    /// The answer to what opened this: sent once, by the first thing the worker settles.
    first: Option<SyncSender<Result<(), Failure>>>,
}

impl<A: Audio> Worker<A> {
    fn new(audio: A, kind: Kind, target: Target, shared: Arc<Shared>, notices: Inbox) -> Self {
        Self {
            stream: None,
            audio,
            kind,
            target,
            shared,
            notices,
            dirty: true,
            gone_streak: 0,
            retry_at: None,
            poll_at: None,
            first: None,
        }
    }

    /// Takes what the system posted, and notes whether any of it needs a look.
    fn take_notices(&mut self) {
        let running = self.stream.as_ref().map(|running| running.id.as_str());
        let mut concerned = false;
        while let Ok(notice) = self.notices.from.try_recv() {
            concerned |= notice.concerns(&self.target, running);
        }
        // Notices were lost: look, whatever they were.
        concerned |= self.notices.full.swap(false, Ordering::AcqRel);
        self.dirty |= concerned;
    }

    /// Drops the stream, if there is one. Whoever reads the microphone next is told once.
    fn drop_stream(&mut self) {
        if self.stream.take().is_some() {
            // Under the ring's lock, which a read of the microphone holds while it looks at
            // the state and the epoch: it sees both changed or neither.
            let _ring = self.shared.ring();
            self.shared.set_state(State::Starting);
            self.shared.epoch.fetch_add(1, Ordering::AcqRel);
        }
    }

    fn answer(&mut self, answer: Result<(), Failure>) {
        if let Some(first) = self.first.take() {
            let _ = first.try_send(answer);
        }
    }

    /// Makes what runs match what is wanted now.
    fn reconcile(&mut self, now: Instant) {
        let kind = self.kind;
        let looked = self.audio.active_ids(kind).and_then(|active| {
            let default = match self.target {
                Target::Default => self.audio.default_id(kind)?,
                Target::Named(_) => None,
            };
            Ok((active, default))
        });
        let (active, default) = match looked {
            Ok(found) => found,
            Err(failure) => {
                self.lose(&failure, now);
                return;
            }
        };
        let want = decide(&self.target, &active, default.as_deref());
        let running = self.stream.as_ref().map(|running| running.id.as_str());
        if want.is_some() && want.as_deref() == running {
            return;
        }
        self.drop_stream();
        let Some(id) = want else {
            self.shared.set_state(State::NoDevice);
            self.answer(Ok(()));
            self.retry_at = Some(now + RETRY);
            return;
        };
        match self.audio.start(kind, &id) {
            Ok(stream) => {
                self.stream = Some(Running { id, stream });
                // What was captured before is not for this stream's listener. Emptying the
                // ring and saying the stream is up are one step to a reader of the microphone,
                // so that nothing of the new device reaches a transmission of the old one.
                {
                    let mut ring = self.shared.ring();
                    if kind == Kind::Capture {
                        ring.clear();
                    }
                    self.shared.set_state(State::Streaming);
                }
                self.answer(Ok(()));
            }
            Err(failure) => self.lose(&failure, now),
        }
    }

    /// Records that the system failed, and when to try again.
    fn lose(&mut self, failure: &Failure, now: Instant) {
        self.drop_stream();
        let (state, tell) = match failure {
            Failure::Gone => (State::NoDevice, false),
            Failure::Denied => (State::Denied, true),
            Failure::Unsupported | Failure::Other(_) => (State::Lost, true),
        };
        self.shared.set_state(state);
        if tell {
            self.answer(Err(failure.clone()));
        } else {
            self.answer(Ok(()));
        }
        self.retry_at = Some(now + RETRY);
    }

    /// One turn of the worker: takes its notices, looks at what the system has if it needs to,
    /// and, with a stream up, waits up to `wait` on it and moves what is ready. `now` is the
    /// time, so that a test can say what it is.
    fn step(&mut self, now: Instant, wait: Duration) -> Stepped {
        self.take_notices();
        if self.poll_at.is_some_and(|at| now >= at) {
            self.dirty = true;
            self.poll_at = Some(now + POLL);
        }
        let retry = self.stream.is_none() && self.retry_at.is_some_and(|at| now >= at);
        if self.dirty || retry {
            self.dirty = false;
            self.retry_at = None;
            self.reconcile(now);
        }
        let shared = &self.shared;
        let kind = self.kind;
        let Some(running) = self.stream.as_mut() else {
            return Stepped::Idle;
        };
        let moved = match kind {
            Kind::Capture => running.stream.capture(wait, &mut |samples| {
                let lost = shared.ring().push_overwriting(samples);
                if lost > 0 {
                    shared.dropped.fetch_add(lost as u64, Ordering::Relaxed);
                }
            }),
            Kind::Render => running
                .stream
                .render(wait, &mut |out| shared.ring().pop(out)),
        };
        match moved {
            Ok(()) => self.gone_streak = 0,
            Err(failure) => {
                // A device that went away is looked for at once: it may be listed still, or
                // another may be wanted. But one that goes away again as soon as it is
                // started waits a while, so that this never spins. Anything else is tried
                // again in a while.
                self.drop_stream();
                if failure == Failure::Gone {
                    self.gone_streak += 1;
                    self.shared.set_state(State::NoDevice);
                    if self.gone_streak >= 2 {
                        self.retry_at = Some(now + RETRY);
                    } else {
                        self.dirty = true;
                    }
                } else {
                    self.lose(&failure, now);
                }
            }
        }
        Stepped::Pumped
    }

    /// Steps until the handle is dropped.
    fn run(mut self) {
        while !self.shared.stop.load(Ordering::Acquire) {
            if self.step(Instant::now(), TICK) == Stepped::Idle {
                std::thread::sleep(TICK);
            }
        }
    }
}

/// What a person is told when opening failed, by the failure. Nothing in it names a device.
fn error_for(doing: &'static str, kind: Kind, failure: Failure) -> Error {
    let detail = match failure {
        Failure::Denied if kind == Kind::Capture => PRIVACY_TEXT.to_owned(),
        Failure::Denied => "Windows refused access to the sound device".to_owned(),
        Failure::Gone => "the device went away".to_owned(),
        Failure::Unsupported => "the sound system would not take 48 kHz audio".to_owned(),
        Failure::Other(line) => line,
    };
    Error::AudioSystem { doing, detail }
}

/// What a worker leaves behind whichever way it ends: the state is `Lost` and the epoch has
/// moved, so a reader is told the stream is gone, once, and is not left with `Streaming` for
/// good.
struct Ends(Arc<Shared>);

impl Drop for Ends {
    fn drop(&mut self) {
        let _ring = self.0.ring();
        self.0.set_state(State::Lost);
        self.0.epoch.fetch_add(1, Ordering::AcqRel);
    }
}

/// Starts the worker for one microphone or speakers, with a ring of `capacity` samples, and
/// waits a moment for its first answer, so that a refusal fails the open.
///
/// `make` runs in the worker's thread and makes the system's objects there.
fn spawn<A: Audio>(
    make: impl FnOnce() -> Result<A, Failure> + Send + 'static,
    kind: Kind,
    target: Target,
    capacity: usize,
) -> Result<Arc<Shared>, Error> {
    let (doing, name) = match kind {
        Kind::Capture => ("open the microphone", "conch-voice-capture"),
        Kind::Render => ("open the speakers", "conch-voice-playback"),
    };
    let shared = Arc::new(Shared::new(capacity));
    let (first_tx, first_rx) = mpsc::sync_channel(1);
    let for_worker = Arc::clone(&shared);
    let started = std::thread::Builder::new()
        .name(name.to_owned())
        .spawn(move || {
            // However this thread ends, a panic included, the microphone stops saying the
            // stream is up.
            let _ends = Ends(Arc::clone(&for_worker));
            let audio = match make() {
                Ok(audio) => audio,
                Err(failure) => {
                    let _ = first_tx.try_send(Err(failure));
                    return;
                }
            };
            let (post_to, notices) = notice_channel();
            let mut audio = audio;
            let subscribed = audio.subscribe(kind, post_to).is_ok();
            let mut worker = Worker::new(audio, kind, target, for_worker, notices);
            worker.first = Some(first_tx);
            if !subscribed {
                // No notice will come: look at what the system has from time to time instead.
                worker.poll_at = Some(Instant::now() + POLL);
            }
            worker.run();
        });
    if started.is_err() {
        return Err(Error::AudioSystem {
            doing,
            detail: "no thread could be started".to_owned(),
        });
    }
    match first_rx.recv_timeout(FIRST_ANSWER) {
        Ok(Ok(())) | Err(mpsc::RecvTimeoutError::Timeout) => Ok(shared),
        Ok(Err(failure)) => {
            shared.stop.store(true, Ordering::Release);
            Err(error_for(doing, kind, failure))
        }
        Err(mpsc::RecvTimeoutError::Disconnected) => {
            shared.stop.store(true, Ordering::Release);
            Err(Error::AudioSystem {
                doing,
                detail: "the audio thread stopped".to_owned(),
            })
        }
    }
}

// ---------------------------------------------------------------------------------------------
// The microphone.

/// What one read does with the capture ring. While the microphone is not usable everything in
/// the ring is thrown away, so that audio from before a loss is never the first thing sent when
/// the microphone is back. Otherwise a whole frame is handed over when there is one, oldest
/// first.
fn take_frame(ring: &mut Ring, usable: bool, frame: &mut Frame) -> MicRead {
    if !usable {
        ring.clear();
        return MicRead::NoMicrophone;
    }
    if ring.queued() < FRAME_LEN {
        return MicRead::NotYet;
    }
    if ring.pop(frame) < FRAME_LEN {
        return MicRead::NotYet;
    }
    MicRead::Ready
}

/// The microphone: the system's default, or the endpoint named in the configuration. The
/// stream is open, and Windows shows it as in use, from the moment it is opened until it is
/// dropped; a named endpoint that is not there is waited for.
///
/// It yields captured audio and nothing more: it is for sending only after the transmit gate
/// has passed it (`transmit.rs`).
pub struct WasapiMic {
    shared: Arc<Shared>,
    /// The epoch this has told its reader about.
    epoch: u64,
    /// What the last read found, so that only a change is logged.
    was: Option<State>,
}

impl WasapiMic {
    /// Opens the microphone through `make`'s system. `target` says which.
    pub(crate) fn start<A: Audio>(
        make: impl FnOnce() -> Result<A, Failure> + Send + 'static,
        target: Target,
    ) -> Result<Self, Error> {
        let shared = spawn(make, Kind::Capture, target, CAPTURE_RING)?;
        Ok(Self {
            shared,
            epoch: 0,
            was: None,
        })
    }

    /// How many captured samples were lost because nothing read them in time.
    #[must_use]
    pub fn dropped(&self) -> u64 {
        self.shared.dropped.load(Ordering::Relaxed)
    }

    /// Logs a change in whether the microphone can be used.
    fn note(&mut self, now: State) {
        if self.was == Some(now) {
            return;
        }
        let first = self.was.is_none();
        self.was = Some(now);
        match now {
            State::Streaming if !first => log::info!("the microphone is back"),
            State::Streaming => {}
            State::Starting => log::info!("waiting for the microphone"),
            State::Denied => log::warn!("no microphone: {PRIVACY_TEXT}"),
            other => log::warn!(
                "no microphone ({}): the gate is shut until one is back",
                other.why()
            ),
        }
    }
}

impl MicSource for WasapiMic {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        // The ring's lock first: the worker changes the state, the epoch and the ring under it,
        // so what is read here is one moment and no frame of a new device can follow the old.
        let shared = Arc::clone(&self.shared);
        let mut ring = shared.ring();
        let state = shared.state();
        let epoch = shared.epoch();
        self.note(state);
        if epoch != self.epoch {
            // The stream was dropped since the last read, and may be running again already
            // on the default's new device: say so once, so that a transmission ends first.
            // What is in the ring is the new stream's, if anything: the worker emptied it
            // before it said the stream was up.
            self.epoch = epoch;
            return MicRead::NoMicrophone;
        }
        take_frame(&mut ring, state == State::Streaming, frame)
    }

    /// The sound card's clock paces the microphone: it is read when the ring has a frame.
    fn pacing(&self) -> Pacing {
        Pacing::Ring
    }

    /// Empties the capture ring, down to the last sample: what is left of a frame would
    /// otherwise be the start of the first frame sent after a press.
    fn forget_waiting(&mut self) {
        self.shared.ring().clear();
    }
}

impl Drop for WasapiMic {
    fn drop(&mut self) {
        self.shared.stop.store(true, Ordering::Release);
    }
}

impl std::fmt::Debug for WasapiMic {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The device is left out: names are never logged (tui-voice.md §20.7).
        f.debug_struct("WasapiMic").finish_non_exhaustive()
    }
}

// ---------------------------------------------------------------------------------------------
// The speakers.

/// What a write of one frame did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Put {
    /// The frame was queued.
    Queued,
    /// Nothing is playing (a named speaker is unplugged, or the stream is not up yet): the
    /// frame was dropped, so that nothing piles up behind what the ring already held.
    NotPlaying,
    /// The ring could not take a whole frame: it was dropped.
    NoRoom,
}

/// What one write of a frame does with the playback ring: it queues it, whole or not at all
/// (part of a frame followed by the next frame is a click).
fn put_frame(ring: &mut Ring, playing: bool, frame: &Frame) -> Put {
    if !playing {
        return Put::NotPlaying;
    }
    if ring.push_whole(frame) {
        Put::Queued
    } else {
        Put::NoRoom
    }
}

/// The frames owed to the speakers when they are not playing. The ring does not drain then, so
/// it cannot say when a frame is due; without something that does, the mix would stop and the
/// speakers' jitter buffers would fill and drop. This is a clock of one frame, for as long as
/// they are not playing; the frames it is owed for are dropped by [`put_frame`].
#[derive(Debug, Default)]
struct IdleClock {
    since: Option<Instant>,
}

impl IdleClock {
    /// The most frames owed at once: a clock that was not asked for longer than this starts
    /// again instead of paying out a burst.
    const MAX_OWED: u32 = 4;

    fn reset(&mut self) {
        self.since = None;
    }

    /// How many whole frames have gone by since this was last asked (none the first time).
    fn due(&mut self, now: Instant) -> usize {
        let Some(since) = self.since else {
            self.since = Some(now);
            return 0;
        };
        let frame = Duration::from_millis(u64::from(FRAME_MS));
        let owed = now.saturating_duration_since(since).as_millis() / frame.as_millis();
        if owed > u128::from(Self::MAX_OWED) {
            self.since = Some(now);
            return Self::MAX_OWED as usize;
        }
        // At most MAX_OWED, so the conversion cannot fail.
        let owed = u32::try_from(owed).unwrap_or(0);
        self.since = Some(since + frame * owed);
        owed as usize
    }
}

/// How many frames to mix now for speakers whose ring holds `queued` samples: as many as bring
/// it up to [`TARGET_FILL`], none if it is there already. The card empties the ring at its own
/// pace, so this is the card's clock setting the pace of the mix. When the speakers are not
/// playing the ring cannot say, and `idle` does.
fn frames_wanted(queued: usize, playing: bool, idle: &mut IdleClock, now: Instant) -> usize {
    if !playing {
        return idle.due(now);
    }
    idle.reset();
    TARGET_FILL.saturating_sub(queued).div_ceil(FRAME_LEN)
}

/// The speakers: the system's default output, or the endpoint named in the configuration. The
/// stream is open from the moment it is opened until it is dropped; a named endpoint that is
/// not there is waited for, and never replaced by another.
pub struct WasapiSpeakers {
    shared: Arc<Shared>,
    idle: IdleClock,
}

impl WasapiSpeakers {
    /// Opens the speakers through `make`'s system. `target` says which.
    pub(crate) fn start<A: Audio>(
        make: impl FnOnce() -> Result<A, Failure> + Send + 'static,
        target: Target,
    ) -> Result<Self, Error> {
        let shared = spawn(make, Kind::Render, target, PLAYBACK_RING)?;
        Ok(Self {
            shared,
            idle: IdleClock::default(),
        })
    }

    /// Whether the stream is up and playing the ring.
    fn playing(&self) -> bool {
        self.shared.state() == State::Streaming
    }
}

impl SpeakerSink for WasapiSpeakers {
    fn play(&mut self, frame: &Frame) {
        let playing = self.playing();
        put_frame(&mut self.shared.ring(), playing, frame);
    }

    /// The sound card's clock paces the mix: a frame is made whenever the ring is short.
    fn pacing(&self) -> Pacing {
        Pacing::Ring
    }

    fn frames_wanted(&mut self) -> usize {
        let playing = self.playing();
        let queued = self.shared.ring().queued();
        frames_wanted(queued, playing, &mut self.idle, Instant::now())
    }
}

/// Playing while the stream is up: not while it is still being set up, nor while speakers
/// named and absent are waited for, which are never replaced by others.
impl Speakers for WasapiSpeakers {
    fn playing(&self) -> bool {
        WasapiSpeakers::playing(self)
    }
}

impl Drop for WasapiSpeakers {
    fn drop(&mut self) {
        self.shared.stop.store(true, Ordering::Release);
    }
}

impl std::fmt::Debug for WasapiSpeakers {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The device is left out: names are never logged (tui-voice.md §20.7).
        f.debug_struct("WasapiSpeakers").finish_non_exhaustive()
    }
}

// ---------------------------------------------------------------------------------------------
// Format: what the helper does when the engine will not do it all.

/// The engine converts 48 kHz mono `f32` for us (`AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM`); a
/// driver that refuses mono is given stereo and the helper mixes the channels. These are the
/// two conversions, with no sample-rate change: a rate the engine will not convert is
/// [`Failure::Unsupported`], and the helper has no resampler.
///
/// Reads interleaved little-endian `f32` frames of `channels` channels from `bytes` and
/// appends one mono sample for each, the mean of the channels, to `out`. A trailing part of a
/// frame is ignored.
pub fn decode_mono(bytes: &[u8], channels: usize, out: &mut Vec<f32>) {
    let channels = channels.max(1);
    for frame in bytes.chunks_exact(4 * channels) {
        let sum: f32 = frame
            .chunks_exact(4)
            .map(|sample| f32::from_le_bytes([sample[0], sample[1], sample[2], sample[3]]))
            .sum();
        out.push(sum / channels as f32);
    }
}

/// Writes each mono sample of `mono` as an interleaved little-endian `f32` frame of
/// `channels` channels, the same in each, to `out`, which it empties first.
pub fn encode_mono(mono: &[f32], channels: usize, out: &mut Vec<u8>) {
    out.clear();
    for sample in mono {
        let bytes = sample.to_le_bytes();
        for _ in 0..channels.max(1) {
            out.extend_from_slice(&bytes);
        }
    }
}

#[cfg(test)]
mod tests {
    use conch_voice_api::VoiceDevicesConfig;
    use conch_voice_audio::SILENCE;

    use super::*;
    use crate::devices::{ConfigView, device_list};

    // Obviously fake endpoint IDs.
    const MIC_A: &str = "FAKE-MIC-A";
    const MIC_B: &str = "FAKE-MIC-B";
    const OUT_A: &str = "FAKE-OUT-A";

    fn endpoint(id: &str) -> Endpoint {
        Endpoint {
            id: id.to_owned(),
            name: format!("{id} (fake)"),
        }
    }

    /// What the fake sound system has and has been asked.
    #[derive(Default)]
    struct World {
        capture: Vec<Endpoint>,
        render: Vec<Endpoint>,
        default_capture: Option<String>,
        default_render: Option<String>,
        /// What the next starts fail with, front first.
        start_failures: VecDeque<Failure>,
        /// What the next pump of a stream fails with.
        pump_failure: Option<Failure>,
        /// Every endpoint a stream was started on.
        started: Vec<String>,
        /// Samples the next capture pump delivers.
        to_deliver: Vec<f32>,
        /// What render pumps played.
        played: Vec<f32>,
        /// Whether `subscribe` is taken.
        can_subscribe: bool,
        /// The channel the system posts its notices to.
        posts: Option<Poster>,
        /// Whether the next start, or the next capture pump, panics.
        panic_on_start: bool,
        panic_in_capture: bool,
        /// The kind of every start, which the fake checks against the endpoint's.
        started_kinds: Vec<Kind>,
    }

    type Fake = Arc<Mutex<World>>;

    fn world() -> Fake {
        Arc::new(Mutex::new(World {
            can_subscribe: true,
            ..World::default()
        }))
    }

    fn with(world: &Fake) -> std::sync::MutexGuard<'_, World> {
        world.lock().unwrap()
    }

    struct FakeAudio(Fake);
    struct FakeStream(Fake);

    impl Audio for FakeAudio {
        type Stream = FakeStream;

        fn endpoints(&mut self, kind: Kind) -> Result<Vec<Endpoint>, Failure> {
            let world = with(&self.0);
            Ok(match kind {
                Kind::Capture => world.capture.clone(),
                Kind::Render => world.render.clone(),
            })
        }

        fn active_ids(&mut self, kind: Kind) -> Result<Vec<String>, Failure> {
            Ok(self
                .endpoints(kind)?
                .into_iter()
                .map(|endpoint| endpoint.id)
                .collect())
        }

        fn default_id(&mut self, kind: Kind) -> Result<Option<String>, Failure> {
            let world = with(&self.0);
            Ok(match kind {
                Kind::Capture => world.default_capture.clone(),
                Kind::Render => world.default_render.clone(),
            })
        }

        fn start(&mut self, kind: Kind, id: &str) -> Result<FakeStream, Failure> {
            let mut world = with(&self.0);
            // As the real system does: an endpoint of the other kind is not this one's.
            let of_kind = match kind {
                Kind::Capture => &world.capture,
                Kind::Render => &world.render,
            };
            if !of_kind.iter().any(|endpoint| endpoint.id == id) {
                return Err(Failure::Gone);
            }
            if std::mem::take(&mut world.panic_on_start) {
                drop(world);
                panic!("FAKE: the sound system fell over");
            }
            world.started_kinds.push(kind);
            world.started.push(id.to_owned());
            if let Some(failure) = world.start_failures.pop_front() {
                return Err(failure);
            }
            Ok(FakeStream(Arc::clone(&self.0)))
        }

        fn subscribe(&mut self, _kind: Kind, to: Poster) -> Result<(), Failure> {
            let mut world = with(&self.0);
            if !world.can_subscribe {
                return Err(Failure::Other("no".to_owned()));
            }
            world.posts = Some(to);
            Ok(())
        }
    }

    impl Stream for FakeStream {
        fn capture(
            &mut self,
            _wait: Duration,
            deliver: &mut dyn FnMut(&[f32]),
        ) -> Result<(), Failure> {
            let mut world = with(&self.0);
            if world.panic_in_capture {
                drop(world);
                panic!("FAKE: the stream fell over");
            }
            if let Some(failure) = world.pump_failure.take() {
                return Err(failure);
            }
            let samples = std::mem::take(&mut world.to_deliver);
            if !samples.is_empty() {
                deliver(&samples);
            }
            Ok(())
        }

        fn render(
            &mut self,
            _wait: Duration,
            supply: &mut dyn FnMut(&mut [f32]) -> usize,
        ) -> Result<(), Failure> {
            let mut world = with(&self.0);
            if let Some(failure) = world.pump_failure.take() {
                return Err(failure);
            }
            let mut room = [0.0_f32; 2 * FRAME_LEN];
            let n = supply(&mut room);
            world.played.extend_from_slice(&room[..n]);
            Ok(())
        }
    }

    /// A worker over the fake, the shared state it works on, and its clock.
    struct Rig {
        worker: Worker<FakeAudio>,
        shared: Arc<Shared>,
        world: Fake,
        now: Instant,
    }

    impl Rig {
        fn new(kind: Kind, target: Target, world: Fake) -> Self {
            let capacity = match kind {
                Kind::Capture => CAPTURE_RING,
                Kind::Render => PLAYBACK_RING,
            };
            let shared = Arc::new(Shared::new(capacity));
            let (post_to, notices) = notice_channel();
            let mut audio = FakeAudio(Arc::clone(&world));
            audio.subscribe(kind, post_to).unwrap();
            Self {
                worker: Worker::new(audio, kind, target, Arc::clone(&shared), notices),
                shared,
                world,
                now: Instant::now(),
            }
        }

        fn step(&mut self) -> Stepped {
            self.worker.step(self.now, Duration::ZERO)
        }

        /// The clock goes on by `by`.
        fn later(&mut self, by: Duration) {
            self.now += by;
        }

        /// The system posts a notice, as its own thread would.
        fn notify(&self, notice: Notice) {
            let world = with(&self.world);
            post(world.posts.as_ref().unwrap(), notice);
        }

        fn state(&self) -> State {
            self.shared.state()
        }

        fn started(&self) -> Vec<String> {
            with(&self.world).started.clone()
        }

        /// A microphone reading this rig's shared state, as the transmit task would.
        fn mic(&self) -> WasapiMic {
            WasapiMic {
                shared: Arc::clone(&self.shared),
                epoch: 0,
                was: None,
            }
        }
    }

    fn mics(world: &Fake, ids: &[&str], default: Option<&str>) {
        let mut world = with(world);
        world.capture = ids.iter().map(|id| endpoint(id)).collect();
        world.default_capture = default.map(str::to_owned);
    }

    #[test]
    fn what_to_follow_is_decided_from_what_is_there_and_nothing_is_substituted() {
        let active = [MIC_A.to_owned(), MIC_B.to_owned()];
        let named = |id: &str| Target::Named(id.to_owned());
        for (target, default, want, why) in [
            (Target::Default, Some(MIC_A), Some(MIC_A), "the default"),
            (
                Target::Default,
                Some(MIC_B),
                Some(MIC_B),
                "the default moved",
            ),
            (Target::Default, None, None, "no default"),
            (
                Target::Default,
                Some("FAKE-GONE"),
                None,
                "a default that is not listed",
            ),
            (
                named(MIC_B),
                Some(MIC_A),
                Some(MIC_B),
                "named, not the default",
            ),
            (
                named("FAKE-GONE"),
                Some(MIC_A),
                None,
                "named and absent: not the default",
            ),
            (
                named("FAKE-GONE"),
                None,
                None,
                "named and absent, no default",
            ),
        ] {
            assert_eq!(
                decide(&target, &active, default),
                want.map(str::to_owned),
                "{why}"
            );
        }
        assert_eq!(decide(&Target::Default, &[], Some(MIC_A)), None);
    }

    #[test]
    fn a_configured_name_is_the_default_when_it_says_so_and_else_an_endpoint() {
        for (configured, want) in [
            (None, Target::Default),
            (Some(""), Target::Default),
            (Some("default"), Target::Default),
            (Some(MIC_A), Target::Named(MIC_A.to_owned())),
        ] {
            assert_eq!(Target::from_config(configured), want, "{configured:?}");
        }
    }

    #[test]
    fn a_named_microphone_that_goes_away_shuts_the_gate_and_is_not_replaced() {
        let world = world();
        mics(&world, &[MIC_A, MIC_B], Some(MIC_B));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        with(&rig.world).to_deliver = vec![0.5; FRAME_LEN];
        assert_eq!(rig.step(), Stepped::Pumped);
        assert_eq!(mic.read(&mut frame), MicRead::Ready);
        assert_eq!(rig.state(), State::Streaming);
        // It is unplugged; the default is another microphone, which is not used.
        mics(&rig.world, &[MIC_B], Some(MIC_B));
        rig.notify(Notice::Removed(MIC_A.to_owned()));
        assert_eq!(rig.step(), Stepped::Idle);
        assert_eq!(rig.state(), State::NoDevice);
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        assert_eq!(rig.started(), [MIC_A], "nothing else was opened");
        // Not even after the retry time.
        rig.later(RETRY * 3);
        rig.step();
        assert_eq!(rig.started(), [MIC_A]);
        assert_eq!(rig.state(), State::NoDevice);
    }

    #[test]
    fn a_named_microphone_that_is_not_there_at_the_start_is_waited_for_and_then_captured() {
        let world = world();
        mics(&world, &[MIC_B], Some(MIC_B));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        assert_eq!(rig.step(), Stepped::Idle);
        assert_eq!(rig.state(), State::NoDevice);
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        assert!(rig.started().is_empty());
        // It is plugged in: the notice makes the worker look at once.
        mics(&rig.world, &[MIC_A, MIC_B], Some(MIC_B));
        rig.notify(Notice::Added(MIC_A.to_owned()));
        with(&rig.world).to_deliver = vec![0.25; FRAME_LEN];
        assert_eq!(rig.step(), Stepped::Pumped);
        assert_eq!(rig.started(), [MIC_A]);
        assert_eq!(rig.state(), State::Streaming);
        // This reader never saw a stream dropped, so it reads what the stream delivered.
        assert_eq!(mic.read(&mut frame), MicRead::Ready);
        assert_eq!(frame[0], 0.25);
    }

    #[test]
    fn following_the_default_moves_to_the_new_one_and_ends_a_transmission_first() {
        let world = world();
        mics(&world, &[MIC_A, MIC_B], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        with(&rig.world).to_deliver = vec![0.5; 2 * FRAME_LEN];
        rig.step();
        assert_eq!(mic.read(&mut frame), MicRead::Ready);
        assert_eq!(rig.started(), [MIC_A]);
        // The default moves while a frame of A is still waiting.
        with(&rig.world).default_capture = Some(MIC_B.to_owned());
        rig.notify(Notice::DefaultChanged(Some(MIC_B.to_owned())));
        with(&rig.world).to_deliver = vec![-0.5; FRAME_LEN];
        rig.step();
        assert_eq!(rig.started(), [MIC_A, MIC_B]);
        assert_eq!(rig.state(), State::Streaming);
        // The stream moved between two reads, and the reader is told before anything of B:
        // once, and nothing of A is sent after it.
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        assert_eq!(mic.read(&mut frame), MicRead::Ready);
        assert_eq!(frame[0], -0.5);
        assert_eq!(mic.read(&mut frame), MicRead::NotYet);
    }

    #[test]
    fn a_named_microphone_does_not_follow_a_change_of_default() {
        let world = world();
        mics(&world, &[MIC_A, MIC_B], Some(MIC_B));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        rig.step();
        let epoch = rig.shared.epoch();
        with(&rig.world).default_capture = Some(MIC_A.to_owned());
        rig.notify(Notice::DefaultChanged(Some(MIC_A.to_owned())));
        rig.step();
        assert_eq!(rig.started(), [MIC_A]);
        assert_eq!(rig.shared.epoch(), epoch);
    }

    #[test]
    fn a_default_that_changes_to_the_same_endpoint_restarts_nothing() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        rig.step();
        rig.notify(Notice::DefaultChanged(Some(MIC_A.to_owned())));
        rig.step();
        assert_eq!(rig.started(), [MIC_A]);
        assert_eq!(rig.shared.epoch(), 0);
    }

    #[test]
    fn no_default_at_all_is_no_microphone_until_one_appears() {
        let world = world();
        mics(&world, &[], None);
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        rig.step();
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        mics(&rig.world, &[MIC_A], Some(MIC_A));
        rig.notify(Notice::DefaultChanged(Some(MIC_A.to_owned())));
        rig.step();
        assert_eq!(rig.state(), State::Streaming);
    }

    #[test]
    fn an_invalidated_stream_is_dropped_and_the_state_says_there_is_no_microphone() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        rig.step();
        // The stream fails; the endpoint is gone from the list by the time it is looked at.
        with(&rig.world).pump_failure = Some(Failure::Gone);
        mics(&rig.world, &[], None);
        rig.step();
        assert_eq!(rig.state(), State::NoDevice);
        assert_eq!(rig.step(), Stepped::Idle);
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        assert_eq!(rig.shared.epoch(), 1);
    }

    #[test]
    fn an_invalidated_stream_whose_device_is_listed_still_is_started_again_at_once() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        rig.step();
        with(&rig.world).pump_failure = Some(Failure::Gone);
        rig.step();
        rig.step();
        assert_eq!(rig.started(), [MIC_A, MIC_A]);
        assert_eq!(rig.state(), State::Streaming);
    }

    #[test]
    fn a_microphone_windows_denies_is_a_state_that_names_the_setting_and_is_tried_again() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        with(&world).start_failures.push_back(Failure::Denied);
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        let (first_tx, first_rx) = mpsc::sync_channel(1);
        rig.worker.first = Some(first_tx);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        rig.step();
        assert_eq!(rig.state(), State::Denied);
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        // The opener is told, and its words name the Windows setting.
        let failure = first_rx.try_recv().unwrap().unwrap_err();
        let error = error_for("open the microphone", Kind::Capture, failure).to_string();
        assert!(error.contains("Privacy & security"), "{error}");
        assert!(error.contains("Microphone"), "{error}");
        assert!(error.starts_with("cannot open the microphone"), "{error}");
        // The setting is switched on: the next try, a second on, succeeds.
        rig.later(RETRY);
        rig.step();
        assert_eq!(rig.state(), State::Streaming);
        assert_eq!(rig.started(), [MIC_A, MIC_A]);
    }

    #[test]
    fn a_start_that_fails_is_tried_again_after_a_second_and_not_before() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        with(&world)
            .start_failures
            .extend([Failure::Other("x".to_owned()), Failure::Unsupported]);
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        rig.step();
        assert_eq!(rig.state(), State::Lost);
        for (after, started, state) in [
            (Duration::from_millis(500), 1, State::Lost),
            (Duration::from_millis(499), 1, State::Lost),
            (Duration::from_millis(1), 2, State::Lost),
            (Duration::from_millis(999), 2, State::Lost),
            (Duration::from_millis(1), 3, State::Streaming),
        ] {
            rig.later(after);
            rig.step();
            assert_eq!(rig.started().len(), started, "after {after:?}");
            assert_eq!(rig.state(), state, "after {after:?}");
        }
    }

    #[test]
    fn without_notifications_the_worker_looks_at_what_there_is_every_so_often() {
        let world = world();
        mics(&world, &[], None);
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        rig.worker.poll_at = Some(rig.now + POLL);
        rig.step();
        assert_eq!(rig.state(), State::NoDevice);
        mics(&rig.world, &[MIC_A], Some(MIC_A));
        rig.later(POLL);
        rig.step();
        assert_eq!(rig.state(), State::Streaming);
    }

    #[test]
    fn capture_that_nothing_reads_drops_the_oldest_and_counts_it() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        rig.step();
        with(&rig.world).to_deliver = (0..CAPTURE_RING + 10).map(|n| n as f32).collect();
        rig.step();
        assert_eq!(rig.shared.dropped.load(Ordering::Relaxed), 10);
        let mut mic = rig.mic();
        let mut frame = SILENCE;
        assert_eq!(mic.read(&mut frame), MicRead::Ready);
        assert_eq!(frame[0], 10.0, "the oldest ten went");
    }

    #[test]
    fn the_microphone_discards_what_waits_for_a_press() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Default, world);
        rig.step();
        with(&rig.world).to_deliver = vec![0.5; FRAME_LEN + 7];
        rig.step();
        let mut mic = rig.mic();
        mic.forget_waiting();
        let mut frame = SILENCE;
        assert_eq!(mic.read(&mut frame), MicRead::NotYet);
        assert_eq!(mic.pacing(), Pacing::Ring);
    }

    #[test]
    fn speakers_play_what_is_queued_on_the_cards_clock_and_are_not_replaced() {
        let world = world();
        with(&world).render = vec![endpoint(OUT_A)];
        with(&world).default_render = Some(OUT_A.to_owned());
        let mut rig = Rig::new(Kind::Render, Target::Named(OUT_A.to_owned()), world);
        let mut speakers = WasapiSpeakers {
            shared: Arc::clone(&rig.shared),
            idle: IdleClock::default(),
        };
        // Not up yet: a frame is dropped.
        assert!(!Speakers::playing(&speakers));
        speakers.play(&[0.1; FRAME_LEN]);
        assert_eq!(rig.shared.ring().queued(), 0);
        rig.step();
        assert!(Speakers::playing(&speakers));
        speakers.play(&[0.1; FRAME_LEN]);
        speakers.play(&[0.2; FRAME_LEN]);
        rig.step();
        let played = with(&rig.world).played.clone();
        assert_eq!(played.len(), 2 * FRAME_LEN);
        assert_eq!(played[0], 0.1);
        assert_eq!(played[FRAME_LEN], 0.2);
        // The speakers are unplugged: nothing is played, and the ones that are there are
        // not used.
        with(&rig.world).render = vec![endpoint("FAKE-OUT-B")];
        with(&rig.world).default_render = Some("FAKE-OUT-B".to_owned());
        rig.notify(Notice::Removed(OUT_A.to_owned()));
        rig.step();
        assert!(!Speakers::playing(&speakers));
        assert_eq!(rig.started(), [OUT_A]);
        assert_eq!(speakers.pacing(), Pacing::Ring);
    }

    #[test]
    fn speakers_that_follow_the_default_move_when_it_does() {
        let world = world();
        with(&world).render = vec![endpoint(OUT_A), endpoint("FAKE-OUT-B")];
        with(&world).default_render = Some(OUT_A.to_owned());
        let mut rig = Rig::new(Kind::Render, Target::Default, world);
        rig.step();
        with(&rig.world).default_render = Some("FAKE-OUT-B".to_owned());
        rig.notify(Notice::DefaultChanged(Some("FAKE-OUT-B".to_owned())));
        rig.step();
        assert_eq!(rig.started(), [OUT_A, "FAKE-OUT-B"]);
    }

    #[test]
    fn a_notice_concerns_the_worker_only_when_it_changes_what_it_should_do() {
        let named = Target::Named(MIC_A.to_owned());
        let id = |id: &str| id.to_owned();
        for (notice, target, running, concerns, why) in [
            (
                Notice::Removed(id(MIC_A)),
                &named,
                Some(MIC_A),
                true,
                "its device is removed",
            ),
            (
                Notice::Removed(id(MIC_B)),
                &named,
                Some(MIC_A),
                false,
                "another is removed",
            ),
            (
                Notice::State(id(MIC_A)),
                &named,
                None,
                true,
                "its device changed state",
            ),
            (
                Notice::Added(id(MIC_A)),
                &named,
                None,
                true,
                "its device came",
            ),
            (
                Notice::Added(id(MIC_B)),
                &named,
                Some(MIC_A),
                false,
                "another came",
            ),
            (
                Notice::DefaultChanged(None),
                &named,
                Some(MIC_A),
                false,
                "a named device ignores the default",
            ),
            (
                Notice::DefaultChanged(Some(id(MIC_B))),
                &Target::Default,
                Some(MIC_A),
                true,
                "the default moved",
            ),
            (
                Notice::Removed(id(MIC_A)),
                &Target::Default,
                Some(MIC_A),
                true,
                "the running device went",
            ),
            (
                Notice::Removed(id(MIC_B)),
                &Target::Default,
                Some(MIC_A),
                false,
                "another went",
            ),
            (
                Notice::Added(id(MIC_B)),
                &Target::Default,
                None,
                true,
                "running on none: anything may do",
            ),
        ] {
            assert_eq!(notice.concerns(target, running), concerns, "{why}");
        }
    }

    #[test]
    fn a_change_of_default_is_followed_only_for_the_side_and_role_that_matter() {
        let some = || Some(MIC_A.to_owned());
        for (side, flow, console, want) in [
            (Kind::Capture, Kind::Capture, true, true),
            (Kind::Capture, Kind::Render, true, false),
            (Kind::Render, Kind::Render, false, false),
            (Kind::Render, Kind::Render, true, true),
        ] {
            assert_eq!(
                default_changed(side, flow, console, some()).is_some(),
                want,
                "{side:?} {flow:?} {console}"
            );
        }
    }

    #[test]
    fn the_systems_thread_posts_and_returns_whatever_the_worker_is_doing() {
        // No receiver: nothing to wait for, and no panic.
        let (to, inbox) = notice_channel();
        drop(inbox);
        post(&to, Notice::Removed(MIC_A.to_owned()));
        // A receiver that never reads: the thousandth post returns as the first did, and
        // the channel holds no more than its bound.
        let (to, inbox) = notice_channel();
        let began = Instant::now();
        for n in 0..10_000 {
            post(&to, Notice::State(format!("FAKE-{n}")));
        }
        assert!(began.elapsed() < Duration::from_secs(5));
        assert_eq!(inbox.from.try_iter().count(), NOTICES);
        // What was lost is not forgotten: the worker is told to look anyway.
        assert!(inbox.full.load(Ordering::Acquire));
    }

    #[test]
    fn a_flood_of_notices_makes_the_worker_look_at_what_there_is() {
        let world = world();
        mics(&world, &[], None);
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        rig.step();
        assert_eq!(rig.state(), State::NoDevice);
        // The device comes, and its notice is among thousands of others about other
        // devices, most of which did not fit.
        mics(&rig.world, &[MIC_A], Some(MIC_A));
        for n in 0..1_000 {
            rig.notify(Notice::Removed(format!("FAKE-OTHER-{n}")));
        }
        rig.step();
        assert_eq!(rig.state(), State::Streaming);
    }

    #[test]
    fn a_device_that_goes_away_again_at_once_is_not_spun_on() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Named(MIC_A.to_owned()), world);
        rig.step();
        // The first time it is looked for at once; the second time it waits.
        with(&rig.world).pump_failure = Some(Failure::Gone);
        rig.step();
        assert_eq!(rig.started().len(), 1);
        with(&rig.world).pump_failure = Some(Failure::Gone);
        rig.step();
        assert_eq!(rig.started().len(), 2);
        for _ in 0..5 {
            assert_eq!(rig.step(), Stepped::Idle);
        }
        assert_eq!(rig.started().len(), 2, "no spinning");
        rig.later(RETRY);
        rig.step();
        assert_eq!(rig.started().len(), 3);
        assert_eq!(rig.state(), State::Streaming);
        // A pump that works ends the streak.
        rig.step();
        assert_eq!(rig.worker.gone_streak, 0);
    }

    #[test]
    fn a_capture_target_naming_a_render_endpoint_starts_nothing() {
        let world = world();
        with(&world).render = vec![endpoint(OUT_A)];
        with(&world).default_render = Some(OUT_A.to_owned());
        mics(&world, &[MIC_A], Some(MIC_A));
        let mut rig = Rig::new(Kind::Capture, Target::Named(OUT_A.to_owned()), world);
        rig.step();
        assert!(rig.started().is_empty());
        assert_eq!(rig.state(), State::NoDevice);
        // Nor does it fall back on the microphone that is there.
        rig.later(RETRY * 2);
        rig.step();
        assert!(with(&rig.world).started_kinds.is_empty());
        // And the system itself refuses an endpoint of the other kind.
        let mut audio = FakeAudio(Arc::clone(&rig.world));
        assert_eq!(audio.start(Kind::Capture, OUT_A).err(), Some(Failure::Gone));
        assert_eq!(audio.start(Kind::Render, MIC_A).err(), Some(Failure::Gone));
    }

    #[test]
    fn hresults_are_sorted_into_what_the_worker_does_about_them() {
        for (code, want) in [
            (0x8889_0004_u32, Failure::Gone),
            (0x8007_0490, Failure::Gone),
            (0x8007_0005, Failure::Denied),
            (0x8889_0008, Failure::Unsupported),
            (
                0x8889_0002,
                Failure::Other("the sound system answered 0x88890002".to_owned()),
            ),
        ] {
            assert_eq!(failure_from_hresult(code as i32), want, "{code:#x}");
        }
    }

    #[test]
    fn what_a_person_is_told_names_the_setting_for_a_denied_microphone_and_no_device() {
        for (kind, failure, has) in [
            (
                Kind::Capture,
                Failure::Denied,
                "Settings > Privacy & security > Microphone",
            ),
            (Kind::Capture, Failure::Unsupported, "48 kHz"),
            (Kind::Render, Failure::Gone, "went away"),
            (
                Kind::Render,
                Failure::Other("the sound system answered 0x1".to_owned()),
                "0x1",
            ),
        ] {
            let text = error_for("open the microphone", kind, failure).to_string();
            assert!(text.contains(has), "{text}");
            assert!(!text.contains("FAKE"), "{text}");
        }
    }

    #[test]
    fn a_read_of_the_capture_ring_is_a_frame_not_yet_or_no_microphone() {
        let samples = |n: usize| {
            let mut ring = Ring::new(CAPTURE_RING);
            ring.push_whole(&vec![0.5; n]);
            ring
        };
        for (usable, queued, want, left) in [
            (true, FRAME_LEN, MicRead::Ready, 0),
            (true, FRAME_LEN + 3, MicRead::Ready, 3),
            (true, FRAME_LEN - 1, MicRead::NotYet, FRAME_LEN - 1),
            (true, 0, MicRead::NotYet, 0),
            (false, 2 * FRAME_LEN, MicRead::NoMicrophone, 0),
        ] {
            let mut ring = samples(queued);
            let mut frame = SILENCE;
            assert_eq!(take_frame(&mut ring, usable, &mut frame), want, "{queued}");
            assert_eq!(ring.queued(), left, "{queued}");
        }
    }

    #[test]
    fn a_write_of_a_frame_is_queued_whole_or_dropped() {
        for (playing, queued, want) in [
            (true, 0, Put::Queued),
            (true, PLAYBACK_RING - FRAME_LEN, Put::Queued),
            (true, PLAYBACK_RING - FRAME_LEN + 1, Put::NoRoom),
            (false, 0, Put::NotPlaying),
        ] {
            let mut ring = Ring::new(PLAYBACK_RING);
            ring.push_whole(&vec![0.0; queued]);
            let before = ring.queued();
            assert_eq!(put_frame(&mut ring, playing, &[0.3; FRAME_LEN]), want);
            let grew = ring.queued() - before;
            assert_eq!(grew, if want == Put::Queued { FRAME_LEN } else { 0 });
        }
    }

    #[test]
    fn frames_are_owed_until_the_ring_holds_the_target_fill() {
        let mut idle = IdleClock::default();
        let now = Instant::now();
        for (queued, want) in [
            (0, 4),
            (1, 4),
            (FRAME_LEN, 3),
            (3 * FRAME_LEN + 1, 1),
            (TARGET_FILL, 0),
            (TARGET_FILL + FRAME_LEN, 0),
        ] {
            assert_eq!(
                frames_wanted(queued, true, &mut idle, now),
                want,
                "{queued}"
            );
        }
    }

    #[test]
    fn speakers_that_are_not_playing_are_owed_a_frame_every_ten_milliseconds() {
        let mut idle = IdleClock::default();
        let start = Instant::now();
        let frame = Duration::from_millis(u64::from(FRAME_MS));
        // The first look starts the clock; then one frame is owed per ten milliseconds, with
        // the remainder carried, and a long gap pays at most four.
        for (at, want) in [
            (Duration::ZERO, 0),
            (frame / 2, 0),
            (frame, 1),
            (frame * 2 + frame / 2, 1),
            (frame * 3, 1),
            (frame * 3 + frame / 2, 0),
            (frame * 40, 4),
            (frame * 41, 1),
        ] {
            assert_eq!(
                frames_wanted(0, false, &mut idle, start + at),
                want,
                "at {at:?}"
            );
        }
        // Playing again resets it: the next time it is not playing starts afresh.
        assert_eq!(frames_wanted(0, true, &mut idle, start), 4);
        assert_eq!(frames_wanted(0, false, &mut idle, start + frame * 50), 0);
    }

    #[test]
    fn channels_are_mixed_down_and_up_without_changing_the_rate() {
        let bytes = |samples: &[f32]| -> Vec<u8> {
            samples
                .iter()
                .flat_map(|sample| sample.to_le_bytes())
                .collect()
        };
        for (channels, input, want) in [
            (1, vec![0.5, -0.25], vec![0.5, -0.25]),
            (2, vec![1.0, 0.0, 0.5, 0.25], vec![0.5, 0.375]),
            (2, vec![1.0], vec![]),
            (0, vec![0.5], vec![0.5]),
        ] {
            let mut out = Vec::new();
            decode_mono(&bytes(&input), channels, &mut out);
            assert_eq!(out, want, "{channels} channels");
        }
        let mut out = vec![9];
        encode_mono(&[0.5, -1.0], 2, &mut out);
        assert_eq!(out, bytes(&[0.5, 0.5, -1.0, -1.0]));
        encode_mono(&[0.5], 1, &mut out);
        assert_eq!(out, bytes(&[0.5]));
    }

    #[test]
    fn the_endpoint_list_counts_an_id_the_configuration_cannot_hold_as_unshown() {
        let world = world();
        {
            let mut world = with(&world);
            world.capture = vec![
                endpoint(MIC_A),
                endpoint("FAKE\"QUOTE"),
                endpoint("FAKE\\BACKSLASH"),
                endpoint("FAKE\nLINES"),
                endpoint(&"N".repeat(256)),
            ];
            world.render = vec![endpoint(OUT_A)];
            world.default_capture = Some(MIC_A.to_owned());
            world.default_render = Some(OUT_A.to_owned());
        }
        let table = list(&mut FakeAudio(world)).unwrap();
        assert!(table.synced);
        let config = ConfigView {
            state: VoiceDevicesConfig::Missing,
            input: None,
            output: None,
        };
        let document = device_list(&table, &config);
        assert_eq!(document.microphones.len(), 1);
        assert_eq!(document.microphones[0].name, MIC_A);
        assert!(document.microphones[0].default);
        assert_eq!(document.unshown_microphones, 4);
        assert_eq!(document.speakers.len(), 1);
        assert_eq!(document.unshown_speakers, 0);
        assert!(document.validate().is_ok());
    }

    #[test]
    fn nothing_the_system_says_about_a_device_reaches_a_debug_rendering() {
        let secret = "FAKE-SECRET-ENDPOINT";
        let world = world();
        mics(&world, &[secret], Some(secret));
        let mut rig = Rig::new(Kind::Capture, Target::Named(secret.to_owned()), world);
        rig.step();
        let shown = format!(
            "{:?} {:?} {:?} {:?} {:?} {:?} {:?} {:?}",
            Notice::Added(secret.to_owned()),
            Notice::Removed(secret.to_owned()),
            Notice::State(secret.to_owned()),
            Notice::DefaultChanged(Some(secret.to_owned())),
            rig.mic(),
            Target::Named(secret.to_owned()),
            endpoint(secret),
            WasapiSpeakers {
                shared: Arc::clone(&rig.shared),
                idle: IdleClock::default()
            },
        );
        assert!(!shown.contains("SECRET"), "{shown}");
    }

    /// Waits for `done`, for at most five seconds.
    fn within_five_seconds(mut done: impl FnMut() -> bool) -> bool {
        let began = Instant::now();
        while began.elapsed() < Duration::from_secs(5) {
            if done() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        false
    }

    #[test]
    fn a_microphone_on_its_own_thread_delivers_frames_and_its_worker_ends_with_it() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let for_thread = Arc::clone(&world);
        let mut mic = WasapiMic::start(move || Ok(FakeAudio(for_thread)), Target::Default).unwrap();
        let mut frame = SILENCE;
        let mut heard = false;
        let delivered = within_five_seconds(|| {
            with(&world).to_deliver = vec![0.5; FRAME_LEN];
            heard = heard || mic.read(&mut frame) == MicRead::Ready;
            heard
        });
        assert!(delivered);
        let shared = Arc::clone(&mic.shared);
        drop(mic);
        // The worker saw the stop and ended, leaving the stream not up.
        assert!(within_five_seconds(|| shared.state() == State::Lost));
    }

    #[test]
    fn a_worker_that_panics_leaves_a_stream_that_is_not_up_and_a_microphone_that_can_be_opened_again()
     {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        let for_thread = Arc::clone(&world);
        let mut mic = WasapiMic::start(
            move || Ok(FakeAudio(for_thread)),
            Target::Named(MIC_A.to_owned()),
        )
        .unwrap();
        let mut frame = SILENCE;
        assert!(within_five_seconds(
            || mic.read(&mut frame) == MicRead::NotYet
        ));
        // The stream falls over in the worker's thread.
        with(&world).panic_in_capture = true;
        assert!(within_five_seconds(|| mic.shared.state() == State::Lost));
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone, "told once");
        assert_eq!(
            mic.read(&mut frame),
            MicRead::NoMicrophone,
            "and still none"
        );
        // The same named microphone is opened again, by whoever reopens it.
        with(&world).panic_in_capture = false;
        let for_thread = Arc::clone(&world);
        let mut again = WasapiMic::start(
            move || Ok(FakeAudio(for_thread)),
            Target::Named(MIC_A.to_owned()),
        )
        .unwrap();
        assert!(within_five_seconds(
            || again.read(&mut frame) == MicRead::NotYet
        ));
        assert_eq!(with(&world).started, [MIC_A, MIC_A]);
    }

    #[test]
    fn a_system_that_panics_while_starting_fails_the_open_and_is_not_left_streaming() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        with(&world).panic_on_start = true;
        let for_thread = Arc::clone(&world);
        let error = WasapiMic::start(move || Ok(FakeAudio(for_thread)), Target::Default)
            .err()
            .map(|error| error.to_string())
            .unwrap_or_default();
        assert_eq!(
            error,
            "cannot open the microphone: the audio thread stopped"
        );
        // Opened again, it starts.
        let mut mic = WasapiMic::start(move || Ok(FakeAudio(world)), Target::Default).unwrap();
        let mut frame = SILENCE;
        assert!(within_five_seconds(
            || mic.read(&mut frame) == MicRead::NotYet
        ));
    }

    #[test]
    fn opening_a_denied_microphone_fails_with_words_that_name_the_setting() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        with(&world).start_failures.push_back(Failure::Denied);
        let error = WasapiMic::start(move || Ok(FakeAudio(world)), Target::Default)
            .err()
            .map(|error| error.to_string())
            .unwrap_or_default();
        assert!(error.contains("Privacy & security"), "{error}");
    }

    #[test]
    fn opening_a_named_microphone_that_is_absent_succeeds_and_says_no_microphone() {
        let world = world();
        mics(&world, &[MIC_B], Some(MIC_B));
        let mut mic = WasapiMic::start(
            move || Ok(FakeAudio(world)),
            Target::Named(MIC_A.to_owned()),
        )
        .unwrap();
        let mut frame = SILENCE;
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
    }

    #[test]
    fn a_system_that_cannot_be_made_fails_the_open_and_never_panics() {
        let error = WasapiSpeakers::start(
            || -> Result<FakeAudio, Failure> { Err(Failure::Other("FAKE: no COM".to_owned())) },
            Target::Default,
        )
        .err()
        .map(|error| error.to_string())
        .unwrap_or_default();
        assert_eq!(error, "cannot open the speakers: FAKE: no COM");
    }

    #[test]
    fn a_system_that_cannot_be_subscribed_to_is_polled_and_still_opens() {
        let world = world();
        mics(&world, &[MIC_A], Some(MIC_A));
        with(&world).can_subscribe = false;
        let mut mic = WasapiMic::start(move || Ok(FakeAudio(world)), Target::Default).unwrap();
        let mut frame = SILENCE;
        // Streaming, so a read is "not yet" rather than "no microphone".
        assert!(within_five_seconds(
            || mic.read(&mut frame) == MicRead::NotYet
        ));
    }
}
