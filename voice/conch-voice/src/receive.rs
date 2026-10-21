//! The mix task: every remote speaker's audio, through a jitter buffer each, added into one
//! frame per tick for the sink (`docs/design/conch-voice.md` §3).
//!
//! Each time a frame is due it takes what has arrived from each speaker into that speaker's
//! jitter buffer, pops one frame from each buffer, adds them, and plays the result. The
//! client's own audio is never among them: LiveKit does not send a participant its own
//! track, and the session loop refuses a track under its own identity besides.
//!
//! What makes a frame due is the sink (`pace.rs`): the PipeWire speakers say how many frames
//! their ring is short of its target fill, so the sound card's clock sets the pace and a late
//! look mixes several frames; a sink with no clock is given one every 10 ms.
//!
//! The sink is whatever the caller opened: the PipeWire speakers, or with `--sink` the
//! [`conch_voice_audio::CountingSink`], which plays nothing and keeps no audio. It is given the mix and
//! nothing else. It can be replaced while the task runs ([`MixCommand::Speakers`], a switch of
//! the speakers inside a session: `docs/design/tui-voice.md` §20.5): the old one is dropped,
//! which closes its stream, and the mix goes to the new one only. The task says whether the
//! speakers it plays to are playing ([`Playing`]), so that the session can tell its owner.
//! What `--json` reports as `stats` is counted and
//! measured by this task, per speaker and for the mix, and what this task keeps per speaker
//! has a bound: a speaker is kept while a track of theirs is mixed, and the last
//! [`DEPARTED_KEPT`] who left are kept until the next report, so that their last interval
//! is still reported. (The sink could also count per speaker, but it would keep a counter
//! for every identity it was ever shown, and nothing reads them.)

use std::collections::{BTreeMap, VecDeque};

use conch_voice_audio::{
    AudioError, CountingSink, Frame, JitterBuffer, Mixer, Pacing, Popped, SILENCE, SignalMeter,
    SpeakerSink,
};
use tokio::sync::mpsc;

use crate::pace::Pace;

/// The most frames one look mixes for a sink paced by its ring. A sink asks for what its
/// ring is short of, which is a few frames; this keeps a sink that asked for more from
/// holding the task, and the commands behind it, for long.
const MAX_PER_LOOK: usize = 16;

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
    /// The speakers to play to from now on. The ones played to until now are dropped, which
    /// closes their stream: nothing is played to them after this. Sent through
    /// [`Receiver::replace_speakers`], which counts it.
    Speakers(BoxedSink),
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

/// The task's word on the speakers it plays to: whether they are playing. Said when that
/// changes, and whenever the speakers are replaced.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Playing {
    /// Whether what is played is heard: the speakers are there and their stream is up.
    pub playing: bool,
    /// How many times the speakers had been replaced when this was said.
    pub replaced: u64,
}

/// The handle on a running mix task.
pub struct Receiver {
    /// Commands for the task. Speakers are replaced through [`Receiver::replace_speakers`],
    /// which counts the replacement; one sent here directly is not counted.
    pub commands: mpsc::UnboundedSender<MixCommand>,
    /// The answer to each [`MixCommand::Stats`].
    pub stats: mpsc::UnboundedReceiver<MixStats>,
    /// Whether the speakers are playing, when it changes.
    pub playing: mpsc::UnboundedReceiver<Playing>,
    /// Replacements of the speakers sent through [`Receiver::replace_speakers`].
    replaced: u64,
}

impl Receiver {
    /// Plays to `speakers` from now on, and drops the speakers played to until now.
    pub fn replace_speakers(&mut self, speakers: BoxedSink) {
        self.replaced += 1;
        // If the task is gone the session is ending; there is nothing to play.
        let _ = self.commands.send(MixCommand::Speakers(speakers));
    }

    /// Whether `said` is about the speakers played to now: made after the task took the last
    /// replacement sent. One made before it is about speakers that are gone, and can be on
    /// its way when they are replaced.
    #[must_use]
    pub fn is_current(&self, said: Playing) -> bool {
        said.replaced == self.replaced
    }
}

/// What the mix task plays to: a sink that can say whether it is heard.
pub trait Speakers: SpeakerSink {
    /// Whether what is played is heard now: the speakers are there and their stream is up.
    /// A sink that plays nowhere real (the counting sink) is always playing.
    fn playing(&self) -> bool {
        true
    }
}

impl Speakers for CountingSink {}

/// What the mix is played to when the speakers asked for could not be opened: nothing. They
/// are never playing, and no other speakers play in their place. A frame is still mixed for
/// them every 10 ms and dropped, so that each speaker's jitter buffer is drained as it would
/// be by speakers that are there.
#[derive(Debug, Default, Clone, Copy)]
pub struct NoSpeakers;

impl SpeakerSink for NoSpeakers {
    fn play(&mut self, _frame: &Frame) {}
}

impl Speakers for NoSpeakers {
    fn playing(&self) -> bool {
        false
    }
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
    playing_tx: mpsc::UnboundedSender<Playing>,
    /// By track id.
    slots: BTreeMap<String, Slot>,
    /// By speaker: everyone with a track in `slots`, and those in `departed`.
    heard: BTreeMap<String, Heard>,
    /// Speakers in `heard` whose last track is gone, in the order they left. Never more
    /// than [`DEPARTED_KEPT`]: the one who left longest ago is forgotten for the next.
    departed: VecDeque<String>,
    mixer: Mixer,
    sink: BoxedSink,
    /// What makes a frame due: the sink's pace.
    pace: Pace,
    /// Whether the sink was playing when last looked at.
    playing: bool,
    /// How many times the sink has been replaced.
    replaced: u64,
    mix_window: Window,
    /// A meter that has measured nothing, copied for each new window.
    blank: SignalMeter,
}

/// Speakers that can be moved into the task.
pub type BoxedSink = Box<dyn Speakers + Send>;

/// Starts the mix task, playing to `sink`. What the reports measure is measured by the task
/// itself, looking for `tones`.
///
/// # Errors
///
/// [`AudioError::InvalidSetting`] if the meter refuses the tones (see
/// [`SignalMeter::new`]).
pub fn spawn(tones: &[f32], sink: BoxedSink) -> Result<Receiver, AudioError> {
    let blank = SignalMeter::new(tones)?;
    let (commands, command_rx) = mpsc::unbounded_channel();
    let (stats_tx, stats) = mpsc::unbounded_channel();
    let (playing_tx, playing) = mpsc::unbounded_channel();
    let task = Task {
        commands: command_rx,
        stats: stats_tx,
        playing_tx,
        slots: BTreeMap::new(),
        heard: BTreeMap::new(),
        departed: VecDeque::new(),
        mixer: Mixer::new(),
        pace: Pace::new(sink.pacing()),
        playing: sink.playing(),
        replaced: 0,
        sink,
        mix_window: Window {
            meter: blank.clone(),
            audible: 0,
        },
        blank,
    };
    tokio::spawn(task.run());
    Ok(Receiver {
        commands,
        stats,
        playing,
        replaced: 0,
    })
}

impl Task {
    async fn run(mut self) {
        // What wakes this loop is the sink's pace (`pace.rs`). A sink on the sound card's
        // clock is looked at often and asked how many frames its ring is short of its
        // target fill; a late look mixes that many and drops none. A sink with no clock
        // (the counting sink) is given one frame every 10 ms.
        loop {
            tokio::select! {
                biased;
                command = self.commands.recv() => match command {
                    Some(command) => self.obey(command),
                    None => return,
                },
                () = self.pace.wait() => self.tick(),
            }
        }
    }

    /// One wake-up: a frame for a sink paced by a timer, and for one paced by its ring as
    /// many as it says it wants. And whether the sink is playing, if that changed.
    fn tick(&mut self) {
        let frames = match self.pace.pacing() {
            Pacing::Timer => 1,
            Pacing::Ring => self.sink.frames_wanted().min(MAX_PER_LOOK),
        };
        for _ in 0..frames {
            self.mix();
        }
        let playing = self.sink.playing();
        if playing != self.playing {
            self.playing = playing;
            self.say_playing();
        }
    }

    fn say_playing(&self) {
        // If nobody is listening the session is over.
        let _ = self.playing_tx.send(Playing {
            playing: self.playing,
            replaced: self.replaced,
        });
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
            MixCommand::Speakers(sink) => {
                // The old speakers are dropped here, which closes their stream; the mix goes
                // to the new ones only, at their pace.
                self.pace = Pace::new(sink.pacing());
                self.sink = sink;
                self.replaced += 1;
                self.playing = self.sink.playing();
                self.say_playing();
            }
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
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
    use std::time::Duration;

    use conch_voice_audio::CountingSink;

    use super::*;

    /// A sink on a clock of its own: it plays what the test says it wants, and says how
    /// many frames it was given.
    struct WantingSink {
        wanted: Arc<AtomicUsize>,
        played: Arc<AtomicU64>,
    }

    impl SpeakerSink for WantingSink {
        fn play(&mut self, _frame: &Frame) {
            self.played.fetch_add(1, Ordering::Relaxed);
        }

        fn pacing(&self) -> Pacing {
            Pacing::Ring
        }

        fn frames_wanted(&mut self) -> usize {
            self.wanted.swap(0, Ordering::Relaxed)
        }
    }

    impl Speakers for WantingSink {}

    /// A sink with no clock, which says only how many frames it was given.
    struct TimedSink(Arc<AtomicU64>);

    impl SpeakerSink for TimedSink {
        fn play(&mut self, _frame: &Frame) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    impl Speakers for TimedSink {}

    #[tokio::test]
    async fn a_sink_on_the_cards_clock_is_given_what_it_wants_when_it_wants_it_and_no_more() {
        let (wanted, played) = (Arc::new(AtomicUsize::new(0)), Arc::new(AtomicU64::new(0)));
        let sink = WantingSink {
            wanted: Arc::clone(&wanted),
            played: Arc::clone(&played),
        };
        let _receiver = spawn(&[440.0], Box::new(sink)).unwrap();
        // Nothing is wanted: nothing is mixed, however long the task looks.
        tokio::time::sleep(Duration::from_millis(60)).await;
        assert_eq!(played.load(Ordering::Relaxed), 0);

        // A late look is owed five frames: all five, at once, well inside the 50 ms a
        // timer of 10 ms would need.
        wanted.store(5, Ordering::Relaxed);
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert_eq!(played.load(Ordering::Relaxed), 5);

        // And a sink that asks for far more gets a bounded burst from the one look that
        // hears it ask (this sink asks once: `frames_wanted` takes what it wanted).
        wanted.store(1_000, Ordering::Relaxed);
        tokio::time::sleep(Duration::from_millis(60)).await;
        assert_eq!(played.load(Ordering::Relaxed), 5 + MAX_PER_LOOK as u64);
    }

    #[tokio::test]
    async fn a_sink_with_no_clock_is_given_a_frame_every_tick() {
        let played = Arc::new(AtomicU64::new(0));
        let _receiver = spawn(&[440.0], Box::new(TimedSink(Arc::clone(&played)))).unwrap();
        tokio::time::sleep(Duration::from_millis(200)).await;
        let n = played.load(Ordering::Relaxed);
        assert!((10..=25).contains(&n), "{n} frames in 200 ms");
    }

    fn counting(tones: &[f32]) -> BoxedSink {
        Box::new(CountingSink::new(tones).unwrap())
    }

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
        let mut receiver = spawn(&[440.0], counting(&[440.0])).unwrap();
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
        let mut receiver = spawn(&[440.0], counting(&[440.0])).unwrap();
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

    /// Speakers for the tests of a switch, as the test sees them: how many audible frames
    /// were played to them, whether they say they are playing, and whether they were dropped.
    #[derive(Clone, Default)]
    struct Played {
        audible: Arc<AtomicU64>,
        playing: Arc<AtomicBool>,
        dropped: Arc<AtomicU64>,
    }

    impl Played {
        fn playing() -> Self {
            let heard = Self::default();
            heard.playing.store(true, Ordering::SeqCst);
            heard
        }

        fn sink(&self) -> BoxedSink {
            Box::new(PlayedSink(self.clone()))
        }

        fn audible(&self) -> u64 {
            self.audible.load(Ordering::SeqCst)
        }

        fn dropped(&self) -> u64 {
            self.dropped.load(Ordering::SeqCst)
        }
    }

    struct PlayedSink(Played);

    impl SpeakerSink for PlayedSink {
        fn play(&mut self, frame: &Frame) {
            if is_audible(frame) {
                self.0.audible.fetch_add(1, Ordering::SeqCst);
            }
        }
    }

    impl Speakers for PlayedSink {
        fn playing(&self) -> bool {
            self.0.playing.load(Ordering::SeqCst)
        }
    }

    impl Drop for PlayedSink {
        fn drop(&mut self) {
            self.0.dropped.fetch_add(1, Ordering::SeqCst);
        }
    }

    async fn until(what: &str, met: impl Fn() -> bool) {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while !met() {
            assert!(tokio::time::Instant::now() < deadline, "timed out: {what}");
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
    }

    async fn next_word(receiver: &mut Receiver) -> Playing {
        tokio::time::timeout(Duration::from_secs(10), receiver.playing.recv())
            .await
            .expect("a word on the speakers")
            .expect("the mix task is running")
    }

    fn add(receiver: &Receiver, capacity: usize) -> mpsc::Sender<Frame> {
        let (frames, received) = mpsc::channel(capacity);
        let add = MixCommand::Add {
            speaker: "p3".to_owned(),
            track: "TR_a".to_owned(),
            frames: received,
        };
        assert!(receiver.commands.send(add).is_ok());
        frames
    }

    /// Issue #655: replaced speakers are dropped, which closes a real stream, and what is
    /// heard after the switch is played to the new speakers only.
    #[tokio::test]
    async fn replaced_speakers_are_dropped_and_the_mix_goes_to_the_new_ones_only() {
        let (old, new) = (Played::playing(), Played::playing());
        let mut receiver = spawn(&[440.0], old.sink()).unwrap();
        let frames = add(&receiver, 64);
        for _ in 0..10 {
            frames.send(loud()).await.unwrap();
        }
        until("the old speakers to play the speaker", || old.audible() > 0).await;

        receiver.replace_speakers(new.sink());
        until("the old speakers to be dropped", || old.dropped() == 1).await;
        let heard_before = old.audible();
        for _ in 0..10 {
            frames.send(loud()).await.unwrap();
        }
        until("the new speakers to play the speaker", || new.audible() > 0).await;
        assert_eq!(
            old.audible(),
            heard_before,
            "nothing more reached the old ones"
        );
        assert_eq!(new.dropped(), 0);
    }

    /// The task says when the speakers stop or start playing, and whenever they are
    /// replaced; a word made before a replacement is about speakers that are gone.
    #[tokio::test]
    async fn the_task_says_whether_the_speakers_play_and_a_word_about_replaced_ones_is_stale() {
        let first = Played::playing();
        let mut receiver = spawn(&[440.0], first.sink()).unwrap();
        // Unplugged.
        first.playing.store(false, Ordering::SeqCst);
        let said = next_word(&mut receiver).await;
        let not_playing = |replaced| Playing {
            playing: false,
            replaced,
        };
        assert_eq!(said, not_playing(0));
        assert!(receiver.is_current(said));

        // Replaced by speakers that are not there: never by any others.
        let absent = Played::default();
        receiver.replace_speakers(absent.sink());
        assert!(!receiver.is_current(said), "about speakers that are gone");
        let said = next_word(&mut receiver).await;
        assert_eq!(said, not_playing(1));
        assert!(receiver.is_current(said));
        // They appear: they play.
        absent.playing.store(true, Ordering::SeqCst);
        let playing = Playing {
            playing: true,
            replaced: 1,
        };
        assert_eq!(next_word(&mut receiver).await, playing);
    }

    /// Speakers that could not be opened are none: never playing, and each speaker's audio
    /// is still taken, so that nothing piles up for them.
    #[tokio::test]
    async fn with_no_speakers_nothing_plays_and_what_arrives_is_still_taken() {
        let old = Played::playing();
        let mut receiver = spawn(&[440.0], old.sink()).unwrap();
        receiver.replace_speakers(Box::new(NoSpeakers));
        let said = next_word(&mut receiver).await;
        assert!(!said.playing && receiver.is_current(said));
        until("the old speakers to be dropped", || old.dropped() == 1).await;
        let frames = add(&receiver, 4);
        for _ in 0..20 {
            frames.send(loud()).await.unwrap();
        }
        taken([&frames]).await;
        assert_eq!(old.audible(), 0);
    }
}
