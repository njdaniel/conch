//! The transmit task: the one owner of the transmit gate and of the microphone source, and
//! the only place a frame is passed from one to the track.
//!
//! This is the property a user relies on (`docs/design/conch-voice.md` §3): microphone
//! audio leaves the machine only while the gate is open, which in push-to-talk mode is while
//! the talk key is held, and in open mode from an onset of sound until quiet. It is kept by
//! construction:
//!
//! - The microphone source and the gate live in this task and nowhere else. Nothing can
//!   read a captured frame except the loop below.
//! - The loop hands a frame onward with [`MicFeed::send`], which takes a [`GatedFrame`].
//!   That type is made only by [`TransmitGate::pass`], so the call cannot be given a frame
//!   the gate did not let through. It is the only call of `send` in the crate.
//! - The gate has its own limit on a transmission, counted in frames (`max_open_ms`), so
//!   the limit holds even if no timer anywhere fires.
//! - No frame reaches the track after it was muted. The session loop mutes the track as
//!   soon as it decides to, without waiting for this task to take its forced shut, and a
//!   frame the gate had already let through could otherwise follow the mute into the track
//!   (issue #631). The frame's hand-over and the mute both take one [`TrackLock`]: a frame
//!   is handed over before the mute, or not at all.
//! - In open mode (`docs/design/tui-voice.md` §20.6) the level detector is given each
//!   captured frame before the gate is, and hands nothing onward: it says only that sound
//!   started or quiet started ([`TxEvent::Sound`]), and the push-to-talk machine decides.
//!   The frame it judged then goes to the gate like any other, so the onset frame meets a
//!   shut gate and goes nowhere. Time counts as well as frames, from this task's pace: quiet
//!   comes once the hang has passed with no frame, or since the last frame at or above the
//!   level, so a source that stops delivering, or trickles, does not hold the gate open;
//!   and a release tail, which the gate counts in frames, is cut once it has run for its
//!   length by the clock, in push-to-talk as well (issue #686): a source that stops
//!   delivering cannot keep a released press in its tail.
//! - Every report of the detector's says how many times the detector had been restarted
//!   when it was made. A report made before the last restart the session asked for is
//!   stale, and the session does not pass it on ([`Transmitter::is_current`]): quiet and an
//!   onset on their way when a reason ends cannot open the gate on that ending.
//! - The microphone can be replaced while the task runs (a switch of device inside a
//!   session, `docs/design/tui-voice.md` §20.5): [`TxCommand::CloseMicrophone`] drops the
//!   old one, which closes its stream, and [`TxCommand::SwitchedMicrophone`] hands over the
//!   new one. What the task says of a microphone ([`TxEvent::Microphone`]) carries how many
//!   times the microphone had been changed, so that a word about one that is gone, still on
//!   its way, is not taken for a word about the one in use
//!   ([`Transmitter::is_current_microphone`]). The first read of a microphone a switch
//!   handed over is always said, whether it gave a frame or not, so that the session learns
//!   whether the new one is there before it counts it as a microphone.
//!
//! The task is told what to do with [`TxCommand`]s and says what happened with
//! [`TxEvent`]s. Commands are taken before the next frame, so a forced shut is never behind
//! audio.
//!
//! What paces the task is the microphone (`pace.rs`): one with a clock of its own, the
//! sound card's, is forwarded from as its ring fills, and a task that was late forwards
//! every frame that built up meanwhile; one without, a tone or a file, is read every 10 ms.
//! Either way a frame reaches the track only by way of the gate. A press first discards what
//! the microphone captured before it and has not handed over (`discard_waiting`), the part
//! of a frame included, so that neither catching up nor the first frame of a transmission
//! sends audio captured while the gate was shut.

use std::future::Future;
use std::pin::pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::task::Poll;
use std::time::Duration;

use conch_voice_audio::{
    AudioError, ForceShut, Frame, GateConfig, GateShut, GateStatus, Gated, LevelChange,
    LevelConfig, LevelDetector, MicRead, MicSource, OpenOutcome, Pacing, SILENCE,
    ShutReason as GateShutReason, TransmitGate,
};
use conch_voice_control::{
    DEFAULT_OPEN_HANG_MS, DEFAULT_OPEN_THRESHOLD_DB, GateCommand, ShutReason, TalkMode,
};
use tokio::sync::mpsc;
use tokio::time::Instant;

use crate::pace::Pace;
use crate::sdk::MicFeed;

/// A microphone source that can be moved into the task.
pub type BoxedMic = Box<dyn MicSource + Send>;

/// Something for the transmit task to do.
pub enum TxCommand<F> {
    /// A command for the gate, from the push-to-talk machine.
    Gate(GateCommand),
    /// The microphone, opened because a session allowed publishing, or in place of one that
    /// was closed. It is read from now until it is closed or the task ends, and taken to have
    /// audio until it says otherwise.
    Microphone(BoxedMic),
    /// A microphone opened in place of one that was closed, by a switch of device. As
    /// `Microphone`, but what its first read gives, a frame or none, is said either way
    /// ([`TxEvent::Microphone`]): the session counts it as a microphone only once it has
    /// given a frame.
    SwitchedMicrophone(BoxedMic),
    /// The microphone is dropped, which closes its stream: the session is over, or another
    /// microphone is to be used. Nothing is captured after this until the next `Microphone`.
    CloseMicrophone,
    /// The audio source of a newly published track: gated frames go here from now on.
    Attach(F),
    /// The track is gone: frames go nowhere until the next `Attach`.
    Detach,
}

/// Something the transmit task reports.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TxEvent {
    /// The gate went from open to shut: a transmission has ended. Also sent when the gate
    /// refused to open, so that whoever asked does not believe it open.
    GateShut {
        /// The gate shut itself because it had forwarded as much audio as one transmission
        /// may hold: its own cap, which needs no timer.
        limit_reached: bool,
    },
    /// The microphone source stopped having audio to give, or has it again, or, for one a
    /// switch handed over, was read for the first time, if it is the one in use
    /// ([`Transmitter::is_current_microphone`]).
    Microphone {
        /// Whether it has audio to give.
        present: bool,
        /// How many times the microphone had been changed when this was said.
        changes: u64,
    },
    /// In open mode, the level detector's word, for the push-to-talk machine as
    /// `PttInput::Sound`, if it is current ([`Transmitter::is_current`]).
    Sound {
        /// Sound started (`true`, an onset) or quiet started (`false`).
        sounding: bool,
        /// How many times the detector had been restarted when it said this.
        restarts: u64,
    },
}

/// The handle on a running transmit task.
pub struct Transmitter<F> {
    /// Commands for the task. They are taken in order. The session sends through
    /// [`Transmitter::send`], which keeps count of the commands that restart the level
    /// detector; a command sent here directly is not counted.
    pub commands: mpsc::UnboundedSender<TxCommand<F>>,
    /// What the task reports.
    pub events: mpsc::UnboundedReceiver<TxEvent>,
    /// Frames handed to a track since the task started.
    pub frames_sent: Arc<AtomicU64>,
    /// Closed before the track is muted, and reopened after it is unmuted.
    pub track_lock: TrackLock,
    /// Commands sent through [`Transmitter::send`] that restart the level detector.
    restarts_sent: u64,
    /// Commands sent through [`Transmitter::send`] that change the microphone.
    mic_changes_sent: u64,
}

impl<F> Transmitter<F> {
    /// Sends `command` to the task, counting it if it restarts the level detector.
    pub fn send(&mut self, command: TxCommand<F>) {
        if restarts_detector(&command) {
            self.restarts_sent += 1;
        }
        if changes_microphone(&command) {
            self.mic_changes_sent += 1;
        }
        // If the task is gone the session is ending; there is nothing to tell.
        let _ = self.commands.send(command);
    }

    /// Whether a report of the level detector's, made after `restarts` restarts, is
    /// current: made after the task took the last command sent that restarts it.
    ///
    /// Commands reach the task in order, and reports come back in order, but a report can
    /// be on its way when a command that restarts the detector is sent: one made before a
    /// reason ended, say. Quiet and an onset both on their way would otherwise be a fresh
    /// onset to the push-to-talk machine, and open the gate on the reason's ending itself
    /// (`docs/design/tui-voice.md` §20.6). A report made before the restart is stale, and
    /// is not passed on.
    #[must_use]
    pub fn is_current(&self, restarts: u64) -> bool {
        restarts == self.restarts_sent
    }

    /// Whether a word about the microphone, made after `changes` changes of it, is about
    /// the microphone in use: made after the task took the last command sent that closes
    /// or replaces it. One made before is about a microphone that is gone. Passed on, a
    /// word that the old one had audio again would clear "no microphone" after a switch
    /// to one that could not be opened, and one that the old one had none would block the
    /// new one until it went away and came back.
    #[must_use]
    pub fn is_current_microphone(&self, changes: u64) -> bool {
        changes == self.mic_changes_sent
    }
}

/// What puts every frame handed to the track before the mute that follows it.
///
/// The session loop mutes the track the moment a forced shut is decided, and this task
/// takes that shut only between two frames: a frame the gate let through just before it
/// would otherwise reach the track after the mute. So the hand-over of a frame (the call of
/// [`MicFeed::send`] and the first poll of what it returns, by the end of which the frame
/// is in the track) is made under this lock, and only while it is open; the session loop
/// closes it before it mutes the track. A frame is then handed over before the mute, or not
/// at all. One that is not was let through by the gate while it was open, and the track it
/// would have reached is muted: it goes nowhere.
///
/// It starts closed, as a track is muted straight after it is published.
#[derive(Clone)]
pub struct TrackLock(Arc<Mutex<bool>>);

impl TrackLock {
    fn new() -> Self {
        Self(Arc::new(Mutex::new(false)))
    }

    /// No frame is handed over from now on. If one is being handed over, this waits until
    /// it has been: no longer than the SDK's first poll of it, which hands it over and
    /// returns. Call it before muting the track.
    pub fn close(&self) {
        *self.0.lock().unwrap_or_else(PoisonError::into_inner) = false;
    }

    /// Frames may be handed over again. Call it after unmuting the track, and before the
    /// gate is told to open. Its only caller in the program is the session loop's answer
    /// to a press (`Session::on_answer`), straight after the unmute: a call anywhere else
    /// could let a frame reach a muted track again. It is public for the tests that drive
    /// a transmitter with no session.
    pub fn reopen(&self) {
        *self.0.lock().unwrap_or_else(PoisonError::into_inner) = true;
    }

    /// Hands a frame over, if the lock is open: `send` is called, and what it returns polled
    /// once, under the lock, and the rest of it is waited for after. Says whether the frame
    /// was handed over.
    async fn hand_over<S: Future<Output = ()>>(&self, send: impl FnOnce() -> S) -> bool {
        let mut send = Some(send);
        let mut sending = pin!(None::<S>);
        let first = std::future::poll_fn(|cx| {
            let open = self.0.lock().unwrap_or_else(PoisonError::into_inner);
            if !*open {
                return Poll::Ready(None);
            }
            if let Some(send) = send.take() {
                sending.set(Some(send()));
            }
            // The guard is held for this poll and dropped before anything waits.
            Poll::Ready(sending.as_mut().as_pin_mut().map(|future| future.poll(cx)))
        })
        .await;
        match (first, sending.as_pin_mut()) {
            (Some(Poll::Pending), Some(rest)) => {
                rest.await;
                true
            }
            (first, _) => first.is_some(),
        }
    }
}

/// The gate's settings for a release tail and a transmit limit. The limit becomes the
/// gate's own cap in frames (`max_open_ms`), which needs no timer to hold.
#[must_use]
pub fn gate_config(release_tail_ms: u32, max_transmit: Duration) -> GateConfig {
    let max_open_ms = u32::try_from(max_transmit.as_millis()).unwrap_or(u32::MAX);
    GateConfig {
        release_tail_ms,
        max_open_ms: Some(max_open_ms),
    }
}

/// The gate's name for one of the push-to-talk machine's reasons.
fn force(reason: ShutReason) -> ForceShut {
    match reason {
        ShutReason::NotConnected => ForceShut::NotConnected,
        ShutReason::NoPublishGrant => ForceShut::NoPublishGrant,
        ShutReason::NoMicrophone => ForceShut::NoMicrophone,
        ShutReason::Deafened => ForceShut::Deafened,
        ShutReason::Muted => ForceShut::Muted,
        ShutReason::KeyDeviceLost => ForceShut::KeyDeviceLost,
        ShutReason::MaxTransmit => ForceShut::MaxPressLength,
    }
}

/// The level detector's settings: `open_threshold_db` and `open_hang_ms`.
#[must_use]
pub fn level_config(open_threshold_db: i32, open_hang_ms: u32) -> LevelConfig {
    LevelConfig {
        threshold_db: open_threshold_db,
        hang_ms: open_hang_ms,
    }
}

/// Starts the transmit task with a shut gate, no microphone and no track, in push-to-talk
/// mode, with the configuration's default level and hang for open mode.
///
/// # Errors
///
/// [`AudioError::InvalidSetting`] if the gate refuses `config`.
pub fn spawn<F: MicFeed>(config: GateConfig) -> Result<Transmitter<F>, AudioError> {
    spawn_with_level(
        config,
        level_config(DEFAULT_OPEN_THRESHOLD_DB, DEFAULT_OPEN_HANG_MS),
    )
}

/// As [`spawn`], with this level and hang for open mode. The task starts in push-to-talk
/// mode, and runs the level detector only once the push-to-talk machine has said open mode
/// ([`GateCommand::TalkMode`]).
///
/// # Errors
///
/// [`AudioError::InvalidSetting`] if the gate refuses `config` or the detector `level`.
pub fn spawn_with_level<F: MicFeed>(
    config: GateConfig,
    level: LevelConfig,
) -> Result<Transmitter<F>, AudioError> {
    let gate = TransmitGate::new(config)?;
    let tail = Duration::from_millis(u64::from(config.release_tail_ms));
    let listen = Listen::new(level, tail)?;
    let (commands, command_rx) = mpsc::unbounded_channel();
    let (event_tx, events) = mpsc::unbounded_channel();
    let frames_sent = Arc::new(AtomicU64::new(0));
    let track_lock = TrackLock::new();
    let task = Task {
        gate,
        mic: None,
        mic_present: false,
        mic_heard: true,
        pace: Pace::new(Pacing::Timer),
        feed: None,
        commands: command_rx,
        events: event_tx,
        frames_sent: Arc::clone(&frames_sent),
        track_lock: track_lock.clone(),
        listen,
        mic_changes: 0,
    };
    tokio::spawn(task.run());
    Ok(Transmitter {
        commands,
        events,
        frames_sent,
        track_lock,
        restarts_sent: 0,
        mic_changes_sent: 0,
    })
}

struct Task<F> {
    gate: TransmitGate,
    mic: Option<BoxedMic>,
    /// What was last reported about the microphone.
    mic_present: bool,
    /// False from a switch's microphone being handed over until its first read, which is
    /// said whatever it gives.
    mic_heard: bool,
    /// What wakes the task to read the microphone: the microphone's ring, or a timer for
    /// one that has no clock.
    pace: Pace,
    feed: Option<F>,
    commands: mpsc::UnboundedReceiver<TxCommand<F>>,
    events: mpsc::UnboundedSender<TxEvent>,
    frames_sent: Arc<AtomicU64>,
    track_lock: TrackLock,
    /// The talk mode, and in open mode the level detector.
    listen: Listen,
    /// How many commands that change the microphone have been taken ([`changes_microphone`]).
    mic_changes: u64,
}

/// The open microphone's part of the task: which talk mode is in force, and the level
/// detector that runs in open mode only. It sees captured frames and keeps none of them.
struct Listen {
    mode: TalkMode,
    detector: LevelDetector,
    /// How many times the detector has been put back in its sounding state
    /// ([`restarts_detector`]). Every report carries it, so that one made before the latest
    /// restart is known for stale ([`Transmitter::is_current`]).
    restarts: u64,
    /// Where a stretch with no frame is measured from: the last frame the detector was
    /// given, or the moment it was last put back in its sounding state, whichever is later.
    since: Instant,
    /// Where the time with no sound is measured from: the last frame at or above the level,
    /// or the moment the detector was last put back in its sounding state.
    sound_at: Instant,
    /// The release tail, as time: in open mode it ends by the clock as well as by frames,
    /// since a source that delivers slowly, or not at all, would otherwise make it last.
    tail: Duration,
    /// When the gate was last told to shut after its tail, in open mode.
    tail_from: Option<Instant>,
}

impl Listen {
    fn new(level: LevelConfig, tail: Duration) -> Result<Self, AudioError> {
        let now = Instant::now();
        Ok(Self {
            mode: TalkMode::PushToTalk,
            detector: LevelDetector::new(level)?,
            restarts: 0,
            since: now,
            sound_at: now,
            tail,
            tail_from: None,
        })
    }

    fn is_open(&self) -> bool {
        self.mode == TalkMode::Open
    }

    /// The detector starts sounding again, with no quiet counted: a fresh onset, after
    /// quiet observed from now, is what opens the gate next. Counted in either mode, as
    /// the session counts the commands that do it.
    fn start_sounding(&mut self) {
        self.detector.start_sounding();
        self.restarts += 1;
        let now = Instant::now();
        self.since = now;
        self.sound_at = now;
    }

    /// A report of the detector's: sound started (`true`) or quiet started.
    fn report(&self, change: LevelChange) -> TxEvent {
        TxEvent::Sound {
            sounding: change == LevelChange::SoundStarted,
            restarts: self.restarts,
        }
    }

    /// A captured frame, before the gate: what the detector says of it, in open mode.
    fn frame(&mut self, frame: &Frame) -> Option<TxEvent> {
        if !self.is_open() {
            return None;
        }
        let now = Instant::now();
        self.since = now;
        let change = self.detector.frame(frame);
        if self.detector.last_frame_loud() {
            self.sound_at = now;
        }
        // Quiet frames that trickle in can each be too late to count, and still too early
        // for the stretch with none to reach the hang: the time since the last sound ends it.
        let change = change.or_else(|| self.detector.silent_for(self.sound_at.elapsed()));
        change.map(|change| self.report(change))
    }

    /// The task looked and there was no frame: what the detector says of the time, in open
    /// mode.
    fn no_frame(&mut self) -> Option<TxEvent> {
        if !self.is_open() {
            return None;
        }
        let change = self
            .detector
            .no_frame_for(self.since.elapsed())
            .or_else(|| self.detector.silent_for(self.sound_at.elapsed()));
        change.map(|change| self.report(change))
    }

    /// The gate was told to shut after its tail.
    fn tail_began(&mut self) {
        self.tail_from = Some(Instant::now());
    }

    /// Whether the release tail has run for as long as it lasts, by the clock.
    fn tail_over(&self) -> bool {
        self.tail_from
            .is_some_and(|from| from.elapsed() >= self.tail)
    }
}

/// Whether the transmit task puts the level detector back in its sounding state on this
/// command: when a reason ends (but the transmit limit, which in open mode ends with quiet),
/// when the talk mode changes, and when a new microphone is opened. The task and
/// [`Transmitter::send`] both ask this one function, so their counts cannot drift apart.
fn restarts_detector<F>(command: &TxCommand<F>) -> bool {
    match command {
        TxCommand::Gate(GateCommand::Clear(reason)) => *reason != ShutReason::MaxTransmit,
        TxCommand::Gate(GateCommand::TalkMode(_))
        | TxCommand::Microphone(_)
        | TxCommand::SwitchedMicrophone(_) => true,
        TxCommand::Gate(_)
        | TxCommand::CloseMicrophone
        | TxCommand::Attach(_)
        | TxCommand::Detach => false,
    }
}

/// Whether this command closes or replaces the microphone. The task and
/// [`Transmitter::send`] both ask this one function, so their counts cannot drift apart.
fn changes_microphone<F>(command: &TxCommand<F>) -> bool {
    matches!(
        command,
        TxCommand::Microphone(_) | TxCommand::SwitchedMicrophone(_) | TxCommand::CloseMicrophone
    )
}

impl<F: MicFeed> Task<F> {
    async fn run(mut self) {
        // What wakes this loop is the microphone's pace (`pace.rs`): a source with a clock of
        // its own, the sound card's, is looked at often and a frame is forwarded whenever
        // its ring holds one, however many have built up; one without (a tone, a file) is
        // read once every 10 ms.
        let mut captured = SILENCE;
        loop {
            tokio::select! {
                // Commands first: a shut that is waiting is acted on before another frame.
                biased;
                command = self.commands.recv() => match command {
                    Some(command) => self.obey(command),
                    // Whoever held the commands is gone, and with it every reason to send.
                    None => return,
                },
                () = self.pace.wait() => {
                    let found = self.frame(&mut captured).await;
                    self.pace.found(found);
                }
            }
        }
    }

    /// Throws away what the microphone has captured and not yet handed over, while the gate
    /// is shut: whole frames a stalled task let build up, and the part of a frame that is
    /// always waiting. A press must not send what was captured before it, and the gate
    /// judges a frame when it is passed, not when it was captured. Nothing here reaches the
    /// track; the samples go nowhere.
    fn discard_waiting(&mut self) {
        if !matches!(self.gate.status(), GateStatus::Shut(_)) {
            return;
        }
        if let Some(mic) = self.mic.as_mut() {
            mic.forget_waiting();
        }
    }

    fn obey(&mut self, command: TxCommand<F>) {
        // Asked before the command is taken apart: the same question the session's count
        // asks of what it sends.
        let restart = restarts_detector(&command);
        if changes_microphone(&command) {
            self.mic_changes += 1;
        }
        match command {
            TxCommand::Gate(GateCommand::Open) => {
                self.discard_waiting();
                match self.gate.open() {
                    OpenOutcome::Opened | OpenOutcome::AlreadyOpen => {}
                    // The machine believes the gate open and it is not. Saying that it shut
                    // ends the machine's transmission, so the two agree again.
                    OpenOutcome::Refused(_) => self.report(TxEvent::GateShut {
                        limit_reached: false,
                    }),
                }
            }
            TxCommand::Gate(GateCommand::Shut) => {
                let was_open = self.gate.status() == GateStatus::Open;
                if let Some(shut) = self.gate.shut() {
                    self.shut(shut);
                } else if was_open {
                    self.listen.tail_began();
                }
            }
            TxCommand::Gate(GateCommand::ForceShut(reason)) => {
                if let Some(shut) = self.gate.force_shut(force(reason)) {
                    self.shut(shut);
                }
            }
            // Once a reason ends, quiet and a fresh onset are needed, observed from now:
            // unmuting during speech does not send the rest of it (the restart below).
            TxCommand::Gate(GateCommand::Clear(reason)) => self.gate.clear_force(force(reason)),
            TxCommand::Gate(GateCommand::TalkMode(mode)) => {
                // The person changed what transmission means: what was being sent ends now,
                // with no release tail, as for a reason.
                if let Some(shut) = self.gate.cut() {
                    self.shut(shut);
                }
                self.listen.mode = mode;
            }
            // A microphone just opened is heard from quiet (the restart below): however loud
            // it begins, it is no onset.
            TxCommand::Microphone(mic) => {
                self.pace = Pace::new(mic.pacing());
                self.mic = Some(mic);
                self.mic_present = true;
            }
            TxCommand::SwitchedMicrophone(mic) => {
                self.pace = Pace::new(mic.pacing());
                self.mic = Some(mic);
                self.mic_present = true;
                self.mic_heard = false;
            }
            TxCommand::CloseMicrophone => {
                self.pace = Pace::new(Pacing::Timer);
                self.mic = None;
                self.mic_present = false;
            }
            TxCommand::Attach(feed) => self.feed = Some(feed),
            TxCommand::Detach => self.feed = None,
        }
        if restart {
            self.listen.start_sounding();
        }
    }

    /// Reads one captured frame, offers it to the gate, and sends it if the gate lets it
    /// through. Says whether there was a frame, so that a ring that may hold more is read
    /// again at once.
    async fn frame(&mut self, captured: &mut conch_voice_audio::Frame) -> bool {
        let Some(mic) = self.mic.as_mut() else {
            return false;
        };
        let present = match mic.read(captured) {
            MicRead::Ready => true,
            MicRead::NotYet => {
                self.no_frame();
                return false;
            }
            MicRead::NoMicrophone => false,
        };
        if present != self.mic_present || !self.mic_heard {
            self.mic_present = present;
            self.mic_heard = true;
            self.report(TxEvent::Microphone {
                present,
                changes: self.mic_changes,
            });
        }
        if !present {
            self.no_frame();
            return false;
        }
        // Open mode: the detector judges the frame before the gate does, and hands it
        // nowhere. Its word goes to the machine; the frame itself goes to the gate below,
        // which is shut for an onset frame.
        if let Some(said) = self.listen.frame(captured) {
            self.report(said);
        }
        let gated = self.gate.pass(captured);
        if let (Some(frame), Some(feed)) = (gated.frame(), self.feed.as_mut()) {
            // The only call of `MicFeed::send` in this crate, handed over under the lock a
            // mute takes.
            if self.track_lock.hand_over(|| feed.send(frame)).await {
                self.frames_sent.fetch_add(1, Ordering::Relaxed);
            }
        }
        if let Gated::Last(_, shut) = gated {
            self.shut(shut);
        }
        true
    }

    /// The task looked and the microphone had no frame. In open mode that time counts as
    /// quiet, so a source that stops delivering does not hold the gate open (in push-to-talk
    /// the detector says nothing, and only the key ends a press). And a release tail, in
    /// either mode (issue #686), which the gate counts in frames, would last for as long as frames take to come,
    /// for ever with none coming: once it has run for its length by the clock, it is cut. A
    /// source that delivers on time has sent the whole tail by then; one that is a few
    /// milliseconds late can lose the tail's last frame.
    fn no_frame(&mut self) {
        if let Some(said) = self.listen.no_frame() {
            self.report(said);
        }
        if matches!(self.gate.status(), GateStatus::Closing { .. })
            && self.listen.tail_over()
            && let Some(shut) = self.gate.cut()
        {
            self.shut(shut);
        }
    }

    /// Reports that the gate has shut.
    fn shut(&self, shut: GateShut) {
        let limit = GateShutReason::Forced(ForceShut::MaxPressLength);
        self.report(TxEvent::GateShut {
            limit_reached: shut.reason == limit,
        });
    }

    fn report(&self, event: TxEvent) {
        // If nobody is listening the session is over, and the gate is about to be dropped.
        let _ = self.events.send(event);
    }
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;
    use std::sync::Mutex;

    use conch_voice_audio::{FRAME_LEN, Frame, GatedFrame};

    use super::*;

    /// A stand-in for a capture ring: frames pushed by the test, read by the task. `part`
    /// stands for the samples short of a frame that a real ring is always holding.
    #[derive(Clone, Default)]
    struct Ring {
        frames: Arc<Mutex<VecDeque<Frame>>>,
        part: Arc<AtomicU64>,
    }

    impl Ring {
        /// Frame number `n` is `n` in every sample, so that a frame is told by its first.
        fn push(&self, n: usize) {
            self.frames.lock().unwrap().push_back([n as f32; FRAME_LEN]);
        }

        fn waiting(&self) -> usize {
            self.frames.lock().unwrap().len()
        }

        /// Samples short of a frame that are waiting.
        fn part(&self) -> u64 {
            self.part.load(Ordering::Relaxed)
        }
    }

    /// A microphone with a clock of its own: it has a frame when the ring does.
    struct RingMic(Ring);

    impl MicSource for RingMic {
        fn read(&mut self, frame: &mut Frame) -> MicRead {
            match self.0.frames.lock().unwrap().pop_front() {
                Some(next) => {
                    *frame = next;
                    MicRead::Ready
                }
                None => MicRead::NotYet,
            }
        }

        fn pacing(&self) -> Pacing {
            Pacing::Ring
        }

        fn forget_waiting(&mut self) {
            self.0.frames.lock().unwrap().clear();
            self.0.part.store(0, Ordering::Relaxed);
        }
    }

    /// A microphone with no clock that says how often it was read.
    struct CountedTone(Arc<AtomicU64>);

    impl MicSource for CountedTone {
        fn read(&mut self, frame: &mut Frame) -> MicRead {
            *frame = [0.25; FRAME_LEN];
            self.0.fetch_add(1, Ordering::Relaxed);
            MicRead::Ready
        }
    }

    /// A track that keeps the first sample of each frame it is given.
    #[derive(Clone, Default)]
    struct Record(Arc<Mutex<Vec<f32>>>);

    impl Record {
        fn heard(&self) -> Vec<f32> {
            self.0.lock().unwrap().clone()
        }
    }

    impl MicFeed for Record {
        fn send(&mut self, frame: GatedFrame<'_>) -> impl std::future::Future<Output = ()> + Send {
            self.0.lock().unwrap().push(frame.samples()[0]);
            async {}
        }
    }

    /// A lock as the session loop leaves it while the track is unmuted.
    fn open_lock() -> TrackLock {
        let lock = TrackLock::new();
        lock.reopen();
        lock
    }

    /// A task's open-microphone part as it starts: in push-to-talk mode, with the default
    /// level and hang.
    fn push_to_talk() -> Listen {
        Listen::new(level_config(-40, 400), Duration::ZERO).unwrap()
    }

    fn open(commands: &mpsc::UnboundedSender<TxCommand<Record>>) {
        assert!(commands.send(TxCommand::Gate(GateCommand::Open)).is_ok());
    }

    async fn until(what: &str, within: Duration, mut met: impl FnMut() -> bool) {
        let end = tokio::time::Instant::now() + within;
        while !met() {
            assert!(tokio::time::Instant::now() < end, "timed out: {what}");
            tokio::time::sleep(Duration::from_millis(1)).await;
        }
    }

    /// The point of pacing from the ring: a late look catches up with every frame the ring
    /// holds. A task that took one frame a 10 ms tick would need half a second for these.
    #[tokio::test]
    async fn a_ring_that_filled_while_the_task_was_late_is_forwarded_whole_and_in_order() {
        let config = gate_config(0, Duration::from_secs(60));
        let transmitter = spawn::<Record>(config).unwrap();
        let (ring, record) = (Ring::default(), Record::default());
        for command in [
            TxCommand::Microphone(Box::new(RingMic(ring.clone())) as BoxedMic),
            TxCommand::Attach(record.clone()),
        ] {
            assert!(transmitter.commands.send(command).is_ok());
        }
        transmitter.track_lock.reopen();
        open(&transmitter.commands);
        // Let the task take the commands: audio already waiting at a press is discarded.
        tokio::time::sleep(Duration::from_millis(20)).await;
        for n in 1..=50 {
            ring.push(n);
        }
        until("fifty frames", Duration::from_millis(400), || {
            record.heard().len() == 50
        })
        .await;
        let want: Vec<f32> = (1..=50).map(|n| n as f32).collect();
        assert_eq!(record.heard(), want, "none dropped, none out of order");
        assert_eq!(ring.waiting(), 0);
        assert_eq!(transmitter.frames_sent.load(Ordering::Relaxed), 50);

        // And a frame that arrives alone is forwarded without waiting for a tick of 10 ms
        // to be due: within a few looks.
        ring.push(51);
        until("one more frame", Duration::from_millis(400), || {
            record.heard().len() == 51
        })
        .await;
    }

    /// A source with no clock is read once every 10 ms, as it always was.
    #[tokio::test]
    async fn a_microphone_with_no_clock_is_read_once_a_tick() {
        let transmitter = spawn::<Record>(gate_config(0, Duration::from_secs(60))).unwrap();
        let reads = Arc::new(AtomicU64::new(0));
        let mic = Box::new(CountedTone(Arc::clone(&reads))) as BoxedMic;
        assert!(
            transmitter
                .commands
                .send(TxCommand::Microphone(mic))
                .is_ok()
        );
        tokio::time::sleep(Duration::from_millis(200)).await;
        let n = reads.load(Ordering::Relaxed);
        assert!((10..=25).contains(&n), "{n} reads in 200 ms");
    }

    /// The gate judges a frame when it is passed, so what a stalled task found waiting when
    /// a press arrived was captured before the press and is not sent.
    #[tokio::test]
    async fn audio_captured_before_a_press_is_not_sent_after_it() {
        let (events, _) = mpsc::unbounded_channel();
        let (_, commands) = mpsc::unbounded_channel();
        let (ring, record) = (Ring::default(), Record::default());
        let mut task = Task {
            gate: TransmitGate::new(gate_config(0, Duration::from_secs(60))).unwrap(),
            mic: Some(Box::new(RingMic(ring.clone()))),
            mic_present: true,
            mic_heard: true,
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::new(AtomicU64::new(0)),
            track_lock: open_lock(),
            listen: push_to_talk(),
            mic_changes: 0,
        };
        for n in 1..=5 {
            ring.push(n);
        }
        // And 120 samples short of a frame, which reading frame by frame would leave to
        // become the start of the first frame sent.
        ring.part.store(120, Ordering::Relaxed);
        task.obey(TxCommand::Gate(GateCommand::Open));
        assert_eq!(ring.waiting(), 0, "what was waiting is gone");
        assert_eq!(ring.part(), 0, "down to the last sample");
        let mut captured = SILENCE;
        assert!(!task.frame(&mut captured).await);
        // What is captured after the press goes through.
        ring.push(6);
        assert!(task.frame(&mut captured).await);
        assert_eq!(record.heard(), [6.0]);
    }

    /// The bound between a lost key device and the gate shutting, the half this test can
    /// show: every frame counted passed the gate while it was open, and once the shut is
    /// obeyed none passes, however many wait in the ring. (The other half, that `run`
    /// takes a command before it reads another frame, is its `biased` select; the keydev
    /// integration tests exercise it.)
    #[tokio::test]
    async fn a_forced_shut_lets_through_no_frame_that_comes_after_it() {
        let (events, mut said) = mpsc::unbounded_channel();
        let (_, commands) = mpsc::unbounded_channel();
        let (ring, record) = (Ring::default(), Record::default());
        let counted = Arc::new(AtomicU64::new(0));
        let mut task = Task {
            gate: TransmitGate::new(gate_config(0, Duration::from_secs(60))).unwrap(),
            mic: Some(Box::new(RingMic(ring.clone()))),
            mic_present: true,
            mic_heard: true,
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::clone(&counted),
            track_lock: open_lock(),
            listen: push_to_talk(),
            mic_changes: 0,
        };
        task.obey(TxCommand::Gate(GateCommand::Open));
        for n in 1..=4 {
            ring.push(n);
        }
        let mut captured = SILENCE;
        for _ in 0..4 {
            assert!(task.frame(&mut captured).await);
        }
        for n in 5..=9 {
            ring.push(n);
        }
        task.obey(TxCommand::Gate(GateCommand::ForceShut(
            ShutReason::KeyDeviceLost,
        )));
        assert!(matches!(
            said.try_recv(),
            Ok(TxEvent::GateShut {
                limit_reached: false
            })
        ));
        while task.frame(&mut captured).await {}
        assert_eq!(record.heard(), [1.0, 2.0, 3.0, 4.0]);
        assert_eq!(counted.load(Ordering::Relaxed), 4);
    }

    /// While the gate is open, the same waiting audio is simply sent: nothing is discarded
    /// from a transmission that is under way.
    #[tokio::test]
    async fn a_transmission_under_way_loses_nothing_to_a_second_press() {
        let (events, _) = mpsc::unbounded_channel();
        let (_, commands) = mpsc::unbounded_channel();
        let (ring, record) = (Ring::default(), Record::default());
        let mut task = Task {
            gate: TransmitGate::new(gate_config(0, Duration::from_secs(60))).unwrap(),
            mic: Some(Box::new(RingMic(ring.clone()))),
            mic_present: true,
            mic_heard: true,
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::new(AtomicU64::new(0)),
            track_lock: open_lock(),
            listen: push_to_talk(),
            mic_changes: 0,
        };
        task.obey(TxCommand::Gate(GateCommand::Open));
        for n in 1..=3 {
            ring.push(n);
        }
        ring.part.store(120, Ordering::Relaxed);
        task.obey(TxCommand::Gate(GateCommand::Open));
        assert_eq!((ring.waiting(), ring.part()), (3, 120));
        let mut captured = SILENCE;
        while task.frame(&mut captured).await {}
        assert_eq!(record.heard(), [1.0, 2.0, 3.0]);
    }

    /// Issue #631. A frame the gate has let through can be held up on its way into the track,
    /// on a thread the system took away, while the session loop mutes: the mute waits for
    /// it, and no frame is handed over once the mute has begun.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_mute_comes_after_a_frame_being_handed_over_and_before_any_other() {
        let lock = open_lock();
        let order = Arc::new(Mutex::new(Vec::new()));
        let (reached, at_sdk) = std::sync::mpsc::channel();
        let (let_go, held) = std::sync::mpsc::channel::<()>();

        // The frame's hand-over holds its thread until it is let go, as a thread the system
        // has descheduled would.
        let handing = tokio::spawn({
            let (lock, order) = (lock.clone(), Arc::clone(&order));
            async move {
                let send = || async move {
                    reached.send(()).unwrap();
                    held.recv().unwrap();
                    order.lock().unwrap().push("frame");
                };
                lock.hand_over(send).await
            }
        });
        at_sdk.recv().unwrap();
        let muting = std::thread::spawn({
            let (lock, order) = (lock.clone(), Arc::clone(&order));
            move || {
                lock.close();
                order.lock().unwrap().push("mute");
            }
        });
        std::thread::sleep(Duration::from_millis(50));
        assert!(order.lock().unwrap().is_empty(), "the mute did not wait");
        let_go.send(()).unwrap();
        muting.join().unwrap();
        assert!(handing.await.unwrap(), "the frame was handed over");
        assert_eq!(*order.lock().unwrap(), ["frame", "mute"]);

        // Once closed, a frame is not handed over at all: its send is never polled.
        let late = || async { order.lock().unwrap().push("late frame") };
        assert!(!lock.hand_over(late).await);
        assert_eq!(*order.lock().unwrap(), ["frame", "mute"]);
        // Until the track is unmuted again.
        lock.reopen();
        let next = || async { order.lock().unwrap().push("next press") };
        assert!(lock.hand_over(next).await);
        assert_eq!(*order.lock().unwrap(), ["frame", "mute", "next press"]);
    }

    /// The task drops a frame the gate let through once the lock is closed, and counts it
    /// as nothing sent.
    #[tokio::test]
    async fn a_frame_let_through_after_the_mute_goes_nowhere() {
        let (events, _) = mpsc::unbounded_channel();
        let (_, commands) = mpsc::unbounded_channel();
        let (ring, record) = (Ring::default(), Record::default());
        let counted = Arc::new(AtomicU64::new(0));
        let track_lock = open_lock();
        let mut task = Task {
            gate: TransmitGate::new(gate_config(0, Duration::from_secs(60))).unwrap(),
            mic: Some(Box::new(RingMic(ring.clone()))),
            mic_present: true,
            mic_heard: true,
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::clone(&counted),
            track_lock: track_lock.clone(),
            listen: push_to_talk(),
            mic_changes: 0,
        };
        task.obey(TxCommand::Gate(GateCommand::Open));
        ring.push(1);
        ring.push(2);
        let mut captured = SILENCE;
        assert!(task.frame(&mut captured).await);
        // The session loop mutes before the task has taken the forced shut.
        track_lock.close();
        assert!(task.frame(&mut captured).await);
        assert_eq!(record.heard(), [1.0]);
        assert_eq!(counted.load(Ordering::Relaxed), 1);
    }

    /// The open microphone (`docs/design/tui-voice.md` §20.6), on the task alone.
    mod open_mode {
        use super::*;

        /// A frame far below any level, told by its first sample, which is `n` · 10⁻⁹.
        fn push_quiet(ring: &Ring, n: usize) {
            let mut frame = SILENCE;
            frame[0] = n as f32 * 1e-9;
            ring.frames.lock().unwrap().push_back(frame);
        }

        /// A task on a ring, with a track to hear what it is handed, `tail_ms` of release
        /// tail and `hang_ms` of hang at the default level, in push-to-talk mode as it
        /// starts; and what it reports.
        fn task(
            tail_ms: u32,
            hang_ms: u32,
        ) -> (Task<Record>, Ring, Record, mpsc::UnboundedReceiver<TxEvent>) {
            let (events, said) = mpsc::unbounded_channel();
            let (_, commands) = mpsc::unbounded_channel();
            let (ring, record) = (Ring::default(), Record::default());
            let task = Task {
                gate: TransmitGate::new(gate_config(tail_ms, Duration::from_secs(60))).unwrap(),
                mic: Some(Box::new(RingMic(ring.clone()))),
                mic_present: true,
                mic_heard: true,
                pace: Pace::new(Pacing::Ring),
                feed: Some(record.clone()),
                commands,
                events,
                frames_sent: Arc::new(AtomicU64::new(0)),
                track_lock: open_lock(),
                listen: Listen::new(
                    level_config(-40, hang_ms),
                    Duration::from_millis(u64::from(tail_ms)),
                )
                .unwrap(),
                mic_changes: 0,
            };
            (task, ring, record, said)
        }

        /// Everything the task has reported so far, the detector's reports with their count
        /// of restarts left out (the tests of it look at it themselves).
        fn reported(said: &mut mpsc::UnboundedReceiver<TxEvent>) -> Vec<TxEvent> {
            std::iter::from_fn(|| said.try_recv().ok())
                .map(|event| match event {
                    TxEvent::Sound { sounding, .. } => sound(sounding),
                    other => other,
                })
                .collect()
        }

        /// A report of the detector's, as [`reported`] gives it.
        fn sound(sounding: bool) -> TxEvent {
            TxEvent::Sound {
                sounding,
                restarts: 0,
            }
        }

        /// Reads every frame waiting in the ring.
        async fn read_all(task: &mut Task<Record>) {
            let mut captured = SILENCE;
            while task.frame(&mut captured).await {}
        }

        /// A task in open mode whose detector has heard its first quiet: the next loud frame
        /// is an onset.
        async fn listening(
            tail_ms: u32,
        ) -> (Task<Record>, Ring, Record, mpsc::UnboundedReceiver<TxEvent>) {
            let (mut task, ring, record, mut said) = task(tail_ms, 100);
            task.obey(TxCommand::Gate(GateCommand::TalkMode(TalkMode::Open)));
            for n in 0..10 {
                push_quiet(&ring, n);
            }
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(false)]);
            (task, ring, record, said)
        }

        #[tokio::test]
        async fn the_detector_judges_each_frame_before_the_gate_and_the_onset_frame_is_not_sent() {
            let (mut task, ring, record, mut said) = listening(0).await;
            // The onset: reported, and the frame itself meets the shut gate.
            ring.push(7);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(true)]);
            assert_eq!(
                record.heard(),
                Vec::<f32>::new(),
                "the onset frame is not sent"
            );

            // The machine's open, as the session passes it on: what is captured after it
            // goes through, and the quiet frames of the hang with it.
            task.obey(TxCommand::Gate(GateCommand::Open));
            ring.push(8);
            for n in 1..=10 {
                push_quiet(&ring, n);
            }
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(false)]);
            let heard = record.heard();
            assert_eq!(heard.len(), 11);
            assert_eq!(heard[0], 8.0);

            // A second utterance is a second onset.
            task.obey(TxCommand::Gate(GateCommand::Shut));
            assert_eq!(
                reported(&mut said),
                [TxEvent::GateShut {
                    limit_reached: false
                }]
            );
            ring.push(9);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(true)]);
            assert_eq!(record.heard().len(), 11, "nothing while the gate is shut");
        }

        #[tokio::test]
        async fn in_push_to_talk_mode_the_detector_does_not_run() {
            let (mut task, ring, _, mut said) = task(0, 100);
            for n in 0..20 {
                push_quiet(&ring, n);
            }
            ring.push(1);
            read_all(&mut task).await;
            std::thread::sleep(Duration::from_millis(120));
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), []);

            // And it stops when the mode goes back to push-to-talk.
            let (mut task, ring, _, mut said) = listening(0).await;
            task.obey(TxCommand::Gate(GateCommand::TalkMode(TalkMode::PushToTalk)));
            ring.push(1);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), []);
        }

        /// The coordinator's decision on #654: a change of mode shuts the gate as a reason
        /// does, with no release tail, and no frame follows it, from an open gate or from one
        /// in its tail.
        #[tokio::test]
        async fn a_change_of_mode_cuts_the_gate_at_once_and_no_frame_follows_it() {
            for in_tail in [false, true] {
                let (mut task, ring, record, mut said) = task(100, 400);
                task.obey(TxCommand::Gate(GateCommand::Open));
                for n in 1..=3 {
                    ring.push(n);
                }
                read_all(&mut task).await;
                if in_tail {
                    task.obey(TxCommand::Gate(GateCommand::Shut));
                    assert_eq!(reported(&mut said), []);
                }
                task.obey(TxCommand::Gate(GateCommand::TalkMode(TalkMode::Open)));
                assert_eq!(
                    reported(&mut said),
                    [TxEvent::GateShut {
                        limit_reached: false
                    }],
                    "in the tail: {in_tail}"
                );
                for n in 4..=40 {
                    ring.push(n);
                }
                read_all(&mut task).await;
                assert_eq!(record.heard(), [1.0, 2.0, 3.0], "in the tail: {in_tail}");
                // Nothing is left standing: the next open opens.
                task.obey(TxCommand::Gate(GateCommand::Open));
                ring.push(41);
                read_all(&mut task).await;
                assert_eq!(record.heard().last(), Some(&41.0), "in the tail: {in_tail}");
            }
        }

        /// Once a reason ends, quiet observed from that moment is needed before an onset; the
        /// transmit limit, which ends with quiet in open mode, is the exception. A new
        /// microphone is heard from quiet too.
        #[tokio::test]
        async fn a_reason_ending_or_a_new_microphone_starts_the_detector_sounding_but_the_limit_does_not()
         {
            for reason in [
                ShutReason::Muted,
                ShutReason::Deafened,
                ShutReason::NotConnected,
                ShutReason::NoPublishGrant,
                ShutReason::NoMicrophone,
            ] {
                let (mut task, ring, _, mut said) = listening(0).await;
                task.obey(TxCommand::Gate(GateCommand::ForceShut(reason)));
                task.obey(TxCommand::Gate(GateCommand::Clear(reason)));
                // Loud straight after: no onset, however long it lasts.
                for n in 1..=30 {
                    ring.push(n);
                }
                read_all(&mut task).await;
                assert_eq!(reported(&mut said), [], "{reason:?}");
                // Quiet for the hang, then sound: the onset.
                for n in 0..10 {
                    push_quiet(&ring, n);
                }
                ring.push(1);
                read_all(&mut task).await;
                assert_eq!(
                    reported(&mut said),
                    [sound(false), sound(true)],
                    "{reason:?}"
                );
            }

            let (mut task, ring, _, mut said) = listening(0).await;
            task.obey(TxCommand::Gate(GateCommand::ForceShut(
                ShutReason::MaxTransmit,
            )));
            task.obey(TxCommand::Gate(GateCommand::Clear(ShutReason::MaxTransmit)));
            ring.push(1);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(true)], "the limit");

            let (mut task, ring, _, mut said) = listening(0).await;
            let another = Ring::default();
            task.obey(TxCommand::Microphone(Box::new(RingMic(another.clone()))));
            another.push(1);
            ring.push(1);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [], "a new microphone, loud at once");
        }

        /// A source that stops delivering counts as quiet, and the release tail, which the
        /// gate counts in frames, is cut once no frame has come for as long as it had left.
        #[tokio::test]
        async fn a_microphone_that_stops_delivering_counts_as_quiet_and_its_tail_is_cut() {
            let (mut task, ring, record, mut said) = listening(50).await;
            ring.push(1);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(true)]);
            task.obey(TxCommand::Gate(GateCommand::Open));
            ring.push(2);
            read_all(&mut task).await;
            assert_eq!(record.heard(), [2.0]);

            // No frame comes. Short of the hang (100 ms) nothing is said: checked against the
            // task's own measure, taken after the look, so that a test thread the system
            // held up for longer than that does not count as the detector speaking early.
            std::thread::sleep(Duration::from_millis(40));
            read_all(&mut task).await;
            let mut quiet = reported(&mut said);
            if task.listen.since.elapsed() < Duration::from_millis(100) {
                assert_eq!(quiet, [], "quiet before the hang");
            }
            while task.listen.since.elapsed() < Duration::from_millis(110) {
                std::thread::sleep(Duration::from_millis(10));
            }
            read_all(&mut task).await;
            quiet.extend(reported(&mut said));
            assert_eq!(quiet, [sound(false)]);

            // The machine shuts the gate. Its tail of 50 ms, which the gate counts in frames,
            // would never run out with none coming: it is cut once it has run for 50 ms by
            // the clock, and not before.
            task.obey(TxCommand::Gate(GateCommand::Shut));
            assert_eq!(reported(&mut said), []);
            let mut shut = Vec::new();
            for _ in 0..3 {
                read_all(&mut task).await;
                shut.extend(reported(&mut said));
                if task.listen.tail_from.unwrap().elapsed() < Duration::from_millis(50) {
                    assert_eq!(shut, [], "the tail was cut short");
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            for _ in 0..200 {
                if task.listen.tail_over() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            read_all(&mut task).await;
            shut.extend(reported(&mut said));
            assert_eq!(
                shut,
                [TxEvent::GateShut {
                    limit_reached: false
                }]
            );
            assert!(matches!(
                task.gate.status(),
                GateStatus::Shut(GateShutReason::Cut)
            ));
            assert_eq!(record.heard(), [2.0]);
        }

        /// A source that trickles quiet frames more slowly than one every 10 ms: counted in
        /// frames (10 ms each, plus the stretch since the last), quiet comes only once
        /// 10 ms × frames + period reaches the hang, which for a period of half the hang
        /// takes hang²/40 (4 s at the default hang, 100 s at the longest). Here every 50 ms
        /// with a hang of 100: counted alone, quiet after five frames, 250 ms. Quiet comes
        /// once the hang has passed since the last sound, after at most three, and the tail
        /// is cut once it has run for its length, with at most two trickled frames sent in it
        /// (counted alone, its 50 ms would be five).
        #[tokio::test]
        async fn a_trickle_of_quiet_frames_goes_quiet_within_the_hang_and_its_tail_is_cut() {
            let trickle = Duration::from_millis(50);
            let (mut task, ring, record, mut said) = listening(50).await;
            ring.push(1);
            read_all(&mut task).await;
            assert_eq!(reported(&mut said), [sound(true)]);
            task.obey(TxCommand::Gate(GateCommand::Open));
            ring.push(2);
            read_all(&mut task).await;
            assert_eq!(record.heard(), [2.0]);

            // One quiet frame every 90 ms, looked for every 10 ms as the task's pace would.
            let mut trickled = 0;
            let mut last_push = Instant::now();
            let quiet = loop {
                if last_push.elapsed() >= trickle {
                    push_quiet(&ring, trickled);
                    trickled += 1;
                    last_push = Instant::now();
                }
                read_all(&mut task).await;
                let now = reported(&mut said);
                if !now.is_empty() {
                    break now;
                }
                assert!(trickled < 10, "no quiet after {trickled} trickled frames");
                std::thread::sleep(Duration::from_millis(10));
            };
            assert_eq!(quiet, [sound(false)]);
            assert!(trickled <= 3, "quiet only after {trickled} trickled frames");

            // The machine shuts the gate; the tail of 50 ms ends by the clock, whatever the
            // trickle.
            task.obey(TxCommand::Gate(GateCommand::Shut));
            let heard_at_shut = record.heard().len();
            let shut = loop {
                if last_push.elapsed() >= trickle {
                    push_quiet(&ring, trickled);
                    trickled += 1;
                    last_push = Instant::now();
                }
                read_all(&mut task).await;
                let now = reported(&mut said);
                if !now.is_empty() {
                    break now;
                }
                assert!(trickled < 20, "the tail never ended");
                std::thread::sleep(Duration::from_millis(10));
            };
            assert_eq!(
                shut,
                [TxEvent::GateShut {
                    limit_reached: false
                }]
            );
            assert!(
                record.heard().len() <= heard_at_shut + 2,
                "{} frames in the tail",
                record.heard().len() - heard_at_shut
            );
        }

        /// Issue #654's security review, S1. Quiet and an onset that the detector reported
        /// before a reason ended, still on their way to the session when it ends, must not
        /// open the gate on that ending: the session passes on only reports made after the
        /// detector's restart. The task, the push-to-talk machine and the session's own check
        /// ([`Transmitter::send`], [`Transmitter::is_current`]) together, the session's
        /// order of work played by hand.
        #[tokio::test]
        async fn quiet_and_an_onset_on_their_way_when_a_reason_ends_do_not_open_the_gate() {
            use conch_voice_control::{LineCommand, Ptt, PttInput, PttOutput};

            /// What the session does with the machine's outputs: gate commands to the task,
            /// through the count. Says whether the machine told the gate to open.
            fn carry_out(transmitter: &mut Transmitter<Record>, outputs: Vec<PttOutput>) -> bool {
                let mut opened = false;
                for output in outputs {
                    if let PttOutput::Gate(command) = output {
                        opened |= command == GateCommand::Open;
                        transmitter.send(TxCommand::Gate(command));
                    }
                }
                opened
            }

            /// The next report of the detector's, waited for.
            async fn next_sound(transmitter: &mut Transmitter<Record>) -> (bool, u64) {
                loop {
                    let event =
                        tokio::time::timeout(Duration::from_secs(5), transmitter.events.recv())
                            .await
                            .expect("a report in time")
                            .unwrap();
                    if let TxEvent::Sound { sounding, restarts } = event {
                        return (sounding, restarts);
                    }
                }
            }

            /// Pushes the hang's worth of quiet frames, and then `loud` frames.
            fn push(ring: &Ring, quiet: usize, loud: usize) {
                for n in 0..quiet {
                    push_quiet(ring, n);
                }
                for n in 1..=loud {
                    ring.push(n);
                }
            }

            let level = level_config(-40, 100);
            let config = gate_config(0, Duration::from_secs(60));
            let mut transmitter = spawn_with_level::<Record>(config, level).unwrap();
            transmitter.track_lock.reopen();
            let (ring, record) = (Ring::default(), Record::default());
            let now = || Duration::ZERO;
            let (mut ptt, first) = Ptt::with_talk_mode(Duration::from_secs(60), TalkMode::Open);
            assert!(!carry_out(&mut transmitter, first));
            transmitter.send(TxCommand::Microphone(Box::new(RingMic(ring.clone()))));
            transmitter.send(TxCommand::Attach(record.clone()));
            for input in [
                PttInput::Microphone(true),
                PttInput::PublishGrant(true),
                PttInput::Connected(true),
            ] {
                assert!(!carry_out(&mut transmitter, ptt.handle(now(), input)));
            }

            // The detector's first quiet, current, passed on.
            push(&ring, 10, 0);
            let (sounding, restarts) = next_sound(&mut transmitter).await;
            assert!(!sounding && transmitter.is_current(restarts));
            assert!(!carry_out(
                &mut transmitter,
                ptt.handle(now(), PttInput::Sound(false))
            ));

            // Muted. The microphone sounds, goes quiet and sounds again while it is: the
            // first onset is passed on (and opens nothing, muted); the quiet and the second
            // onset are still on their way when the user unmutes.
            let mute = PttInput::Line(LineCommand::Mute);
            assert!(!carry_out(&mut transmitter, ptt.handle(now(), mute)));
            push(&ring, 0, 3);
            let (sounding, restarts) = next_sound(&mut transmitter).await;
            assert!(sounding && transmitter.is_current(restarts));
            assert!(!carry_out(
                &mut transmitter,
                ptt.handle(now(), PttInput::Sound(true))
            ));
            push(&ring, 10, 3);
            let on_their_way = [
                next_sound(&mut transmitter).await,
                next_sound(&mut transmitter).await,
            ];
            assert_eq!(on_their_way.map(|(sounding, _)| sounding), [false, true]);

            // Unmuted: the clear goes to the task, which restarts the detector, before the
            // session reads the two reports.
            assert!(!carry_out(&mut transmitter, ptt.handle(now(), mute)));
            tokio::time::sleep(Duration::from_millis(20)).await;
            // What the session does with each: passed on only if current.
            let mut opened = false;
            for (sounding, restarts) in on_their_way {
                if transmitter.is_current(restarts) {
                    opened |= carry_out(
                        &mut transmitter,
                        ptt.handle(now(), PttInput::Sound(sounding)),
                    );
                }
            }
            assert!(
                !opened,
                "the reports on their way opened the gate on the unmute"
            );
            assert!(!ptt.status().transmitting);

            // Sound goes on from before the clear: nothing. Then quiet observed after the
            // restart, and an onset: that opens the gate.
            push(&ring, 0, 5);
            push(&ring, 10, 1);
            let mut opened = false;
            for _ in 0..2 {
                let (sounding, restarts) = next_sound(&mut transmitter).await;
                assert!(transmitter.is_current(restarts));
                opened |= carry_out(
                    &mut transmitter,
                    ptt.handle(now(), PttInput::Sound(sounding)),
                );
            }
            assert!(opened, "a fresh quiet and onset open the gate");
            assert!(record.heard().is_empty(), "nothing was sent before");
        }
    }

    /// A microphone that is not there: a named one that is absent.
    struct Absent;

    impl MicSource for Absent {
        fn read(&mut self, _frame: &mut Frame) -> MicRead {
            MicRead::NoMicrophone
        }
    }

    /// Issue #655. What the task says of a microphone can be on its way when the session
    /// closes it for a switch: it is then about a microphone that is gone, and is not
    /// current, whether the switch opened another or found none to open.
    #[tokio::test]
    async fn a_word_about_a_microphone_that_was_closed_is_not_current() {
        let mut transmitter = spawn::<Record>(gate_config(0, Duration::from_secs(60))).unwrap();
        transmitter.send(TxCommand::Microphone(Box::new(Absent)));
        let said = tokio::time::timeout(Duration::from_secs(10), transmitter.events.recv())
            .await
            .unwrap()
            .unwrap();
        let TxEvent::Microphone { present, changes } = said else {
            panic!("not about the microphone: {said:?}");
        };
        assert!(!present);
        assert!(transmitter.is_current_microphone(changes));

        transmitter.send(TxCommand::CloseMicrophone);
        assert!(
            !transmitter.is_current_microphone(changes),
            "about a microphone that was closed, with none in its place"
        );
        let reads = Arc::new(AtomicU64::new(0));
        transmitter.send(TxCommand::Microphone(Box::new(CountedTone(Arc::clone(
            &reads,
        )))));
        assert!(!transmitter.is_current_microphone(changes));
        // The new one is read, and is there: nothing more is said of it.
        while reads.load(Ordering::Relaxed) < 3 {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert!(transmitter.events.try_recv().is_err());
    }

    /// Issue #656. A microphone a switch hands over is not taken to be there: its first read
    /// is said either way, a frame or none, once, about that microphone, so that a session
    /// that switched knows whether the new one is there before it counts it as one.
    #[tokio::test]
    async fn the_first_read_of_a_switched_microphone_is_said_whatever_it_gives() {
        let mut transmitter = spawn::<Record>(gate_config(0, Duration::from_secs(60))).unwrap();
        let next = async |transmitter: &mut Transmitter<Record>| loop {
            let said = tokio::time::timeout(Duration::from_secs(10), transmitter.events.recv())
                .await
                .unwrap()
                .unwrap();
            if let TxEvent::Microphone { present, changes } = said {
                return (present, changes);
            }
        };
        let reads = Arc::new(AtomicU64::new(0));
        transmitter.send(TxCommand::SwitchedMicrophone(Box::new(CountedTone(
            Arc::clone(&reads),
        ))));
        let (present, changes) = next(&mut transmitter).await;
        assert!(present, "a microphone that gives frames is there");
        assert!(transmitter.is_current_microphone(changes));
        while reads.load(Ordering::Relaxed) < 3 {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert!(transmitter.events.try_recv().is_err(), "said once");

        transmitter.send(TxCommand::CloseMicrophone);
        transmitter.send(TxCommand::SwitchedMicrophone(Box::new(Absent)));
        let (present, changes) = next(&mut transmitter).await;
        assert!(!present, "one that gives none is not");
        assert!(transmitter.is_current_microphone(changes));
    }
}
