//! The transmit task: the one owner of the transmit gate and of the microphone source, and
//! the only place a frame is passed from one to the track.
//!
//! This is the property a user relies on (`docs/design/conch-voice.md` §3): microphone
//! audio leaves the machine only while the talk key is held. It is kept by construction:
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
    AudioError, ForceShut, GateConfig, GateShut, GateStatus, Gated, MicRead, MicSource,
    OpenOutcome, Pacing, SILENCE, ShutReason as GateShutReason, TransmitGate,
};
use conch_voice_control::{GateCommand, ShutReason};
use tokio::sync::mpsc;

use crate::pace::Pace;
use crate::sdk::MicFeed;

/// A microphone source that can be moved into the task.
pub type BoxedMic = Box<dyn MicSource + Send>;

/// Something for the transmit task to do.
pub enum TxCommand<F> {
    /// A command for the gate, from the push-to-talk machine.
    Gate(GateCommand),
    /// The microphone, opened because a session allowed publishing. It is read from now
    /// until the task ends, and taken to have audio until it says otherwise.
    Microphone(BoxedMic),
    /// The session is over: the microphone is dropped, which closes its stream. Nothing is
    /// captured after this.
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
    /// The microphone source stopped having audio to give (`false`), or has it again
    /// (`true`).
    Microphone(bool),
}

/// The handle on a running transmit task.
pub struct Transmitter<F> {
    /// Commands for the task. They are taken in order.
    pub commands: mpsc::UnboundedSender<TxCommand<F>>,
    /// What the task reports.
    pub events: mpsc::UnboundedReceiver<TxEvent>,
    /// Frames handed to a track since the task started.
    pub frames_sent: Arc<AtomicU64>,
    /// Closed before the track is muted, and reopened after it is unmuted.
    pub track_lock: TrackLock,
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

/// Starts the transmit task with a shut gate, no microphone and no track.
///
/// # Errors
///
/// [`AudioError::InvalidSetting`] if the gate refuses `config`.
pub fn spawn<F: MicFeed>(config: GateConfig) -> Result<Transmitter<F>, AudioError> {
    let gate = TransmitGate::new(config)?;
    let (commands, command_rx) = mpsc::unbounded_channel();
    let (event_tx, events) = mpsc::unbounded_channel();
    let frames_sent = Arc::new(AtomicU64::new(0));
    let track_lock = TrackLock::new();
    let task = Task {
        gate,
        mic: None,
        mic_present: false,
        pace: Pace::new(Pacing::Timer),
        feed: None,
        commands: command_rx,
        events: event_tx,
        frames_sent: Arc::clone(&frames_sent),
        track_lock: track_lock.clone(),
    };
    tokio::spawn(task.run());
    Ok(Transmitter {
        commands,
        events,
        frames_sent,
        track_lock,
    })
}

struct Task<F> {
    gate: TransmitGate,
    mic: Option<BoxedMic>,
    /// What was last reported about the microphone.
    mic_present: bool,
    /// What wakes the task to read the microphone: the microphone's ring, or a timer for
    /// one that has no clock.
    pace: Pace,
    feed: Option<F>,
    commands: mpsc::UnboundedReceiver<TxCommand<F>>,
    events: mpsc::UnboundedSender<TxEvent>,
    frames_sent: Arc<AtomicU64>,
    track_lock: TrackLock,
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
                if let Some(shut) = self.gate.shut() {
                    self.shut(shut);
                }
            }
            TxCommand::Gate(GateCommand::ForceShut(reason)) => {
                if let Some(shut) = self.gate.force_shut(force(reason)) {
                    self.shut(shut);
                }
            }
            TxCommand::Gate(GateCommand::Clear(reason)) => self.gate.clear_force(force(reason)),
            TxCommand::Microphone(mic) => {
                self.pace = Pace::new(mic.pacing());
                self.mic = Some(mic);
                self.mic_present = true;
            }
            TxCommand::CloseMicrophone => {
                self.pace = Pace::new(Pacing::Timer);
                self.mic = None;
                self.mic_present = false;
            }
            TxCommand::Attach(feed) => self.feed = Some(feed),
            TxCommand::Detach => self.feed = None,
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
            MicRead::NotYet => return false,
            MicRead::NoMicrophone => false,
        };
        if present != self.mic_present {
            self.mic_present = present;
            self.report(TxEvent::Microphone(present));
        }
        if !present {
            return false;
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
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::new(AtomicU64::new(0)),
            track_lock: open_lock(),
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
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::clone(&counted),
            track_lock: open_lock(),
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
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::new(AtomicU64::new(0)),
            track_lock: open_lock(),
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
            pace: Pace::new(Pacing::Ring),
            feed: Some(record.clone()),
            commands,
            events,
            frames_sent: Arc::clone(&counted),
            track_lock: track_lock.clone(),
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
}
