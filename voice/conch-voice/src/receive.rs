//! The mix task: every remote speaker's audio, through a jitter buffer each, added into one
//! frame per tick for the sink (`docs/design/conch-voice.md` §3).
//!
//! Each tick it takes what has arrived from each speaker into that speaker's jitter buffer,
//! pops one frame from each buffer, adds them, and plays the result. The client's own audio
//! is never among them: LiveKit does not send a participant its own track, and the session
//! loop refuses a track under its own identity besides.
//!
//! In this issue the sink is the [`CountingSink`], which plays nothing and keeps no audio:
//! it and this task count and measure, and that is what `--json` reports as `stats`. Real
//! speakers arrive with issue #184, behind the same `SpeakerSink` trait.

use std::collections::BTreeMap;
use std::time::Duration;

use conch_voice_audio::{
    AudioError, CountingSink, FRAME_MS, Frame, JitterBuffer, Mixer, Popped, SILENCE, SignalMeter,
    SpeakerSink,
};
use tokio::sync::mpsc;
use tokio::time::MissedTickBehavior;

/// A frame counts as audible at or above this RMS: -60 dB of full scale. What a receiver
/// makes up when nothing arrives (comfort noise, the tail of concealment) is below it; a
/// test tone at a quarter of full scale is far above.
pub const AUDIBLE_RMS: f32 = 1e-3;

/// Something for the mix task to do.
pub enum MixCommand {
    /// A remote track was subscribed to: give it a jitter buffer and mix it.
    Add {
        /// The participant, as `p<principal id>`.
        speaker: String,
        /// The track's id.
        track: String,
        /// The track's audio as it arrives.
        frames: mpsc::Receiver<Frame>,
    },
    /// A track is gone.
    Remove {
        /// The track's id.
        track: String,
    },
    /// The connection is over: every track is gone.
    Clear,
    /// The user deafened or undeafened: while deafened the mix is silence.
    Deafen(bool),
    /// Report what was received since the last report.
    Stats,
}

/// What was measured of one stream of frames since the last report.
#[derive(Debug, Clone, PartialEq)]
pub struct Measured {
    /// Frames, silent ones included.
    pub frames: u64,
    /// Frames at or above [`AUDIBLE_RMS`].
    pub audible_frames: u64,
    /// The RMS level of all of them, where full scale is 1.
    pub rms: f32,
    /// The strongest of the frequencies the sink was told to look for, if one stood out.
    pub dominant_hz: Option<f32>,
}

/// What was received from one speaker.
#[derive(Debug, Clone, PartialEq)]
pub struct SpeakerStats {
    /// The participant, as `p<principal id>`.
    pub speaker: String,
    /// Since the last report.
    pub window: Measured,
    /// Frames since the client started.
    pub frames_total: u64,
    /// Audible frames since the client started.
    pub audible_frames_total: u64,
}

/// One report: every speaker heard from, and the mix that was played.
#[derive(Debug, Clone, PartialEq)]
pub struct MixStats {
    /// One entry per speaker, in name order.
    pub speakers: Vec<SpeakerStats>,
    /// The mix handed to the sink.
    pub mix: Measured,
}

/// The handle on a running mix task.
pub struct Receiver {
    /// Commands for the task.
    pub commands: mpsc::UnboundedSender<MixCommand>,
    /// The answer to each [`MixCommand::Stats`].
    pub stats: mpsc::UnboundedReceiver<MixStats>,
}

/// A meter over one reporting interval, with the count the meter itself does not keep.
struct Window {
    meter: SignalMeter,
    audible: u64,
}

impl Window {
    fn push(&mut self, frame: &Frame) -> bool {
        self.meter.push(frame);
        let audible = is_audible(frame);
        if audible {
            self.audible += 1;
        }
        audible
    }

    /// What was measured, leaving the window empty for the next interval.
    fn take(&mut self, blank: &SignalMeter) -> Measured {
        let measured = Measured {
            frames: self.meter.frames(),
            audible_frames: self.audible,
            rms: self.meter.rms(),
            dominant_hz: self.meter.dominant(),
        };
        self.meter = blank.clone();
        self.audible = 0;
        measured
    }
}

fn is_audible(frame: &Frame) -> bool {
    let energy: f32 = frame.iter().map(|sample| sample * sample).sum();
    (energy / frame.len() as f32).sqrt() >= AUDIBLE_RMS
}

/// One subscribed track.
struct Slot {
    speaker: String,
    frames: mpsc::Receiver<Frame>,
    jitter: JitterBuffer,
}

/// What one speaker has sent, over every track of theirs.
struct Heard {
    window: Window,
    frames_total: u64,
    audible_total: u64,
}

struct Task {
    commands: mpsc::UnboundedReceiver<MixCommand>,
    stats: mpsc::UnboundedSender<MixStats>,
    /// By track id.
    slots: BTreeMap<String, Slot>,
    /// By speaker.
    heard: BTreeMap<String, Heard>,
    mixer: Mixer,
    sink: CountingSink,
    mix_window: Window,
    /// A meter that has measured nothing, copied for each new window.
    blank: SignalMeter,
}

/// Starts the mix task with a counting sink that looks for `tones`.
///
/// # Errors
///
/// [`AudioError::InvalidSetting`] if the sink refuses the tones (see
/// [`SignalMeter::new`]).
pub fn spawn(tones: &[f32]) -> Result<Receiver, AudioError> {
    let blank = SignalMeter::new(tones)?;
    let sink = CountingSink::new(tones)?;
    let (commands, command_rx) = mpsc::unbounded_channel();
    let (stats_tx, stats) = mpsc::unbounded_channel();
    let task = Task {
        commands: command_rx,
        stats: stats_tx,
        slots: BTreeMap::new(),
        heard: BTreeMap::new(),
        mixer: Mixer::new(),
        sink,
        mix_window: Window {
            meter: blank.clone(),
            audible: 0,
        },
        blank,
    };
    tokio::spawn(task.run());
    Ok(Receiver { commands, stats })
}

impl Task {
    async fn run(mut self) {
        // In this issue the sink is not a device, so a timer paces the mix. Real speakers
        // will pace it from their playback ring: a frame is mixed whenever the ring falls
        // below its target fill (the design note's §3).
        let mut tick = tokio::time::interval(Duration::from_millis(u64::from(FRAME_MS)));
        tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                biased;
                command = self.commands.recv() => match command {
                    Some(command) => self.obey(command),
                    None => return,
                },
                _ = tick.tick() => self.mix(),
            }
        }
    }

    fn obey(&mut self, command: MixCommand) {
        match command {
            MixCommand::Add {
                speaker,
                track,
                frames,
            } => {
                let slot = Slot {
                    speaker,
                    frames,
                    jitter: JitterBuffer::new(),
                };
                self.slots.insert(track, slot);
            }
            MixCommand::Remove { track } => {
                self.slots.remove(&track);
            }
            MixCommand::Clear => self.slots.clear(),
            MixCommand::Deafen(deafened) => self.mixer.set_deafened(deafened),
            MixCommand::Stats => self.report(),
        }
    }

    /// One tick: what arrived goes into the jitter buffers, and one frame from each comes
    /// out, is added, and is played.
    fn mix(&mut self) {
        let mut frame = SILENCE;
        for slot in self.slots.values_mut() {
            while let Ok(arrived) = slot.frames.try_recv() {
                self.sink.heard(&slot.speaker, &arrived);
                let heard = self
                    .heard
                    .entry(slot.speaker.clone())
                    .or_insert_with(|| Heard {
                        window: Window {
                            meter: self.blank.clone(),
                            audible: 0,
                        },
                        frames_total: 0,
                        audible_total: 0,
                    });
                heard.frames_total += 1;
                if heard.window.push(&arrived) {
                    heard.audible_total += 1;
                }
                slot.jitter.push(&arrived);
            }
            if slot.jitter.pop(&mut frame) == Popped::Audio {
                self.mixer.add(&frame);
            }
        }
        self.mixer.finish(&mut frame);
        self.sink.play(&frame);
        self.mix_window.push(&frame);
    }

    fn report(&mut self) {
        let speakers = self
            .heard
            .iter_mut()
            .map(|(speaker, heard)| SpeakerStats {
                speaker: speaker.clone(),
                window: heard.window.take(&self.blank),
                frames_total: heard.frames_total,
                audible_frames_total: heard.audible_total,
            })
            .collect();
        let mix = self.mix_window.take(&self.blank);
        // A speaker who has left is reported once more, with what arrived last, and then
        // no longer.
        let present: Vec<&str> = self
            .slots
            .values()
            .map(|slot| slot.speaker.as_str())
            .collect();
        self.heard
            .retain(|speaker, _| present.contains(&speaker.as_str()));
        let _ = self.stats.send(MixStats { speakers, mix });
    }
}
