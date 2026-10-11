//! The mix task: every remote speaker's audio, through a jitter buffer each, added into one
//! frame per tick for the sink (`docs/design/conch-voice.md` §3).
//!
//! Each tick it takes what has arrived from each speaker into that speaker's jitter buffer,
//! pops one frame from each buffer, adds them, and plays the result. The client's own audio
//! is never among them: LiveKit does not send a participant its own track, and the session
//! loop refuses a track under its own identity besides.
//!
//! In this issue the sink is the [`CountingSink`], which plays nothing and keeps no audio.
//! It is given the mix and nothing else. What `--json` reports as `stats` is counted and
//! measured by this task, per speaker and for the mix, and what this task keeps per speaker
//! has a bound: a speaker is kept while a track of theirs is mixed, and the last
//! [`DEPARTED_KEPT`] who left are kept until the next report, so that their last interval
//! is still reported. (The sink could also count per speaker, but it would keep a counter
//! for every identity it was ever shown, and nothing reads them.) Real speakers arrive with
//! issue #184, behind the same `SpeakerSink` trait.

use std::collections::{BTreeMap, VecDeque};
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

/// How many speakers who have left are kept for the next report. Reports are asked for
/// only with `--json`; without this bound a client that is never asked would keep an entry
/// for everyone who was ever in the room.
pub const DEPARTED_KEPT: usize = 64;

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
    /// By speaker: everyone with a track in `slots`, and those in `departed`.
    heard: BTreeMap<String, Heard>,
    /// Speakers in `heard` whose last track is gone, in the order they left. Never more
    /// than [`DEPARTED_KEPT`]: the one who left longest ago is forgotten for the next.
    departed: VecDeque<String>,
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
        departed: VecDeque::new(),
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
                // A speaker who had left is back, and is no longer one to forget.
                self.departed.retain(|name| *name != speaker);
                let slot = Slot {
                    speaker,
                    frames,
                    jitter: JitterBuffer::new(),
                };
                if let Some(replaced) = self.slots.insert(track, slot) {
                    self.left(replaced.speaker);
                }
            }
            MixCommand::Remove { track } => {
                if let Some(slot) = self.slots.remove(&track) {
                    self.left(slot.speaker);
                }
            }
            MixCommand::Clear => {
                for (_, slot) in std::mem::take(&mut self.slots) {
                    self.left(slot.speaker);
                }
            }
            MixCommand::Deafen(deafened) => self.mixer.set_deafened(deafened),
            MixCommand::Stats => self.report(),
        }
    }

    /// A track of `speaker`'s is gone. If it was their last, what was heard from them is
    /// kept for the next report, unless [`DEPARTED_KEPT`] others left after them first.
    fn left(&mut self, speaker: String) {
        let still_here = self.slots.values().any(|slot| slot.speaker == speaker);
        if still_here || !self.heard.contains_key(&speaker) || self.departed.contains(&speaker) {
            return;
        }
        self.departed.push_back(speaker);
        while self.departed.len() > DEPARTED_KEPT {
            if let Some(forgotten) = self.departed.pop_front() {
                self.heard.remove(&forgotten);
            }
        }
    }

    /// One tick: what arrived goes into the jitter buffers, and one frame from each comes
    /// out, is added, and is played.
    fn mix(&mut self) {
        let mut frame = SILENCE;
        for slot in self.slots.values_mut() {
            while let Ok(arrived) = slot.frames.try_recv() {
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
        for speaker in self.departed.drain(..) {
            self.heard.remove(&speaker);
        }
        let _ = self.stats.send(MixStats { speakers, mix });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One frame that is not silence.
    fn loud() -> Frame {
        [0.25; conch_voice_audio::FRAME_LEN]
    }

    /// Waits until the mix task has taken every frame that was sent on these.
    async fn taken<'a>(senders: impl IntoIterator<Item = &'a mpsc::Sender<Frame>> + Clone) {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while senders
            .clone()
            .into_iter()
            .any(|frames| frames.capacity() < frames.max_capacity())
        {
            assert!(
                tokio::time::Instant::now() < deadline,
                "frames were not taken"
            );
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
    }

    async fn report(receiver: &mut Receiver) -> MixStats {
        assert!(receiver.commands.send(MixCommand::Stats).is_ok());
        tokio::time::timeout(Duration::from_secs(10), receiver.stats.recv())
            .await
            .expect("a report")
            .expect("the mix task is running")
    }

    /// Without `--json` nobody ever asks for a report. Speakers come and go all the same,
    /// and what is kept of those who went has a bound.
    #[tokio::test]
    async fn speakers_who_came_and_went_are_forgotten_though_no_report_is_ever_asked_for() {
        let mut receiver = spawn(&[440.0]).unwrap();
        let rounds = 8;
        let each = 50;
        for round in 0..rounds {
            let mut tracks = Vec::new();
            for n in 0..each {
                let (frames, received) = mpsc::channel(4);
                frames.try_send(loud()).unwrap();
                let track = format!("TR_{round}_{n}");
                let add = MixCommand::Add {
                    speaker: format!("p{}", round * each + n),
                    track: track.clone(),
                    frames: received,
                };
                assert!(receiver.commands.send(add).is_ok());
                tracks.push((track, frames));
            }
            // Every one of them is heard from, so every one has an entry to forget.
            taken(tracks.iter().map(|(_, frames)| frames)).await;
            if round % 2 == 0 {
                for (track, _) in &tracks {
                    let remove = MixCommand::Remove {
                        track: track.clone(),
                    };
                    assert!(receiver.commands.send(remove).is_ok());
                }
            } else {
                // As when the connection ends.
                assert!(receiver.commands.send(MixCommand::Clear).is_ok());
            }
        }

        // Four hundred speakers were heard from. The one report asked for now shows what
        // was kept: the last sixty-four to leave, and nobody else.
        let stats = report(&mut receiver).await;
        assert_eq!(stats.speakers.len(), DEPARTED_KEPT);
        for speaker in &stats.speakers {
            let id: usize = speaker.speaker[1..].parse().unwrap();
            assert!(
                id >= rounds * each - DEPARTED_KEPT,
                "{} was kept",
                speaker.speaker
            );
            assert_eq!(speaker.frames_total, 1);
            assert_eq!(speaker.audible_frames_total, 1);
        }
        // And once reported, they are gone too.
        assert_eq!(report(&mut receiver).await.speakers, []);
    }

    #[tokio::test]
    async fn a_speaker_who_left_is_reported_once_more_and_one_who_came_back_is_kept() {
        let mut receiver = spawn(&[440.0]).unwrap();
        let mut held = Vec::new();
        for (speaker, track) in [("p3", "TR_a"), ("p4", "TR_b"), ("p4", "TR_c")] {
            let (frames, received) = mpsc::channel(4);
            frames.try_send(loud()).unwrap();
            let add = MixCommand::Add {
                speaker: speaker.to_owned(),
                track: track.to_owned(),
                frames: received,
            };
            assert!(receiver.commands.send(add).is_ok());
            held.push(frames);
        }
        taken(&held).await;
        // p3 leaves; p4 loses one of two tracks and is still there.
        for track in ["TR_a", "TR_b"] {
            let remove = MixCommand::Remove {
                track: track.to_owned(),
            };
            assert!(receiver.commands.send(remove).is_ok());
        }
        let names = |stats: &MixStats| -> Vec<String> {
            stats.speakers.iter().map(|s| s.speaker.clone()).collect()
        };
        let first = report(&mut receiver).await;
        assert_eq!(names(&first), ["p3", "p4"], "the one who left, once more");
        assert_eq!(first.speakers[1].frames_total, 2);
        let second = report(&mut receiver).await;
        assert_eq!(names(&second), ["p4"]);

        // p4 leaves and comes back before the next report: kept, with what was counted.
        let remove = MixCommand::Remove {
            track: "TR_c".to_owned(),
        };
        assert!(receiver.commands.send(remove).is_ok());
        let (frames, received) = mpsc::channel(4);
        frames.try_send(loud()).unwrap();
        let add = MixCommand::Add {
            speaker: "p4".to_owned(),
            track: "TR_d".to_owned(),
            frames: received,
        };
        assert!(receiver.commands.send(add).is_ok());
        taken([&frames]).await;
        let third = report(&mut receiver).await;
        let fourth = report(&mut receiver).await;
        assert_eq!(names(&third), ["p4"]);
        assert_eq!(names(&fourth), ["p4"], "back, so not forgotten");
        assert_eq!(fourth.speakers[0].frames_total, 3);
        drop(held);
    }
}
