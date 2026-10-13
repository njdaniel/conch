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
//!
//! The task is told what to do with [`TxCommand`]s and says what happened with
//! [`TxEvent`]s. Commands are taken before the next frame, so a forced shut is never behind
//! audio.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use conch_voice_audio::{
    AudioError, FRAME_MS, ForceShut, GateConfig, GateShut, Gated, MicRead, MicSource, OpenOutcome,
    SILENCE, ShutReason as GateShutReason, TransmitGate,
};
use conch_voice_control::{GateCommand, ShutReason};
use tokio::sync::mpsc;
use tokio::time::MissedTickBehavior;

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
    let task = Task {
        gate,
        mic: None,
        mic_present: false,
        feed: None,
        commands: command_rx,
        events: event_tx,
        frames_sent: Arc::clone(&frames_sent),
    };
    tokio::spawn(task.run());
    Ok(Transmitter {
        commands,
        events,
        frames_sent,
    })
}

struct Task<F> {
    gate: TransmitGate,
    mic: Option<BoxedMic>,
    /// What was last reported about the microphone.
    mic_present: bool,
    feed: Option<F>,
    commands: mpsc::UnboundedReceiver<TxCommand<F>>,
    events: mpsc::UnboundedSender<TxEvent>,
    frames_sent: Arc<AtomicU64>,
}

impl<F: MicFeed> Task<F> {
    async fn run(mut self) {
        // In this issue the microphone is a tone or a file, with no clock of its own, so a
        // timer paces it. A real microphone will pace this loop from its capture ring: a
        // frame is forwarded whenever the ring holds a whole one (the design note's §3).
        let mut tick = tokio::time::interval(Duration::from_millis(u64::from(FRAME_MS)));
        tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
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
                _ = tick.tick() => self.frame(&mut captured).await,
            }
        }
    }

    fn obey(&mut self, command: TxCommand<F>) {
        match command {
            TxCommand::Gate(GateCommand::Open) => match self.gate.open() {
                OpenOutcome::Opened | OpenOutcome::AlreadyOpen => {}
                // The machine believes the gate open and it is not. Saying that it shut
                // ends the machine's transmission, so the two agree again.
                OpenOutcome::Refused(_) => self.report(TxEvent::GateShut {
                    limit_reached: false,
                }),
            },
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
                self.mic = Some(mic);
                self.mic_present = true;
            }
            TxCommand::Attach(feed) => self.feed = Some(feed),
            TxCommand::Detach => self.feed = None,
        }
    }

    /// Reads one captured frame, offers it to the gate, and sends it if the gate lets it
    /// through.
    async fn frame(&mut self, captured: &mut conch_voice_audio::Frame) {
        let Some(mic) = self.mic.as_mut() else {
            return;
        };
        let present = match mic.read(captured) {
            MicRead::Ready => true,
            MicRead::NotYet => return,
            MicRead::NoMicrophone => false,
        };
        if present != self.mic_present {
            self.mic_present = present;
            self.report(TxEvent::Microphone(present));
        }
        if !present {
            return;
        }
        let gated = self.gate.pass(captured);
        if let (Some(frame), Some(feed)) = (gated.frame(), self.feed.as_mut()) {
            // The only call of `MicFeed::send` in this crate.
            feed.send(frame).await;
            self.frames_sent.fetch_add(1, Ordering::Relaxed);
        }
        if let Gated::Last(_, shut) = gated {
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
