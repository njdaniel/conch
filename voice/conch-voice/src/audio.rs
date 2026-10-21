//! The real microphone and speakers: PipeWire, through `dmn-audio`, behind the two traits of
//! `conch-voice-audio` (`docs/design/conch-voice.md` §3, §9).
//!
//! Input: PipeWire's graph, as `dmn-audio` follows it. The microphone is a `MicCapture` and
//! the speakers a `Playback`, both mono `f32` at 48 kHz. Output: a [`MicSource`] that gives
//! the captured audio a frame at a time, and a [`SpeakerSink`] that takes the mix a frame at a
//! time, and says whether it is playing ([`Speakers`]). Owns: the one connection to PipeWire
//! ([`PipeWire`]), the capture stream from the moment the microphone is opened until it is
//! dropped, and the playback stream from the moment the speakers are opened until they are.
//! A switch of device inside a session opens a new stream for the new name and drops the
//! old one, so a name that is absent, or names a device of the other kind, is treated the
//! same for the first stream and for every later one: never replaced by another device.
//!
//! What runs where. PipeWire's callbacks are `dmn-audio`'s: `mic_process` and
//! `playback_process`, in its `src/mic.rs` and `src/playback.rs`, each documented there call
//! by call as allocating nothing, locking nothing and logging nothing, and each only moving
//! samples between the stream's buffer and a lock-free ring. Nothing in this file runs in a
//! callback. This file is on the other side of the ring, in the transmit and mix tasks, where
//! it may allocate, lock and log, and does only the first and last sparingly.
//!
//! What paces them. Both devices say [`Pacing::Ring`]: the sound card's clock sets the pace
//! and there is no second clock to drift against it. The transmit task forwards a frame
//! whenever the capture ring holds a whole one, and the mix task mixes a frame whenever the
//! playback ring holds less than [`TARGET_FILL`] ([`frames_wanted`]). `dmn-audio`'s rings are
//! lock-free and cannot wake a task, so the tasks look at them every
//! [`RING_LOOK`](crate::pace::RING_LOOK) (`pace.rs`); what they do on a look is decided by
//! what the ring holds, so a late look catches up by handling several frames, and nothing is
//! dropped and no silence is added to correct for drift.
//!
//! The decisions that are this file's own are small functions of plain values, so that they
//! are tested without PipeWire: [`availability`] (when the microphone counts as there),
//! [`take_frame`] (what a read does with the capture ring), [`frames_wanted`] (how many
//! frames the playback ring is short of) and [`put_frame`] (what a write does with it).

use std::sync::Arc;
use std::time::{Duration, Instant};

use conch_voice_audio::{
    FRAME_LEN, FRAME_MS, Frame, MicRead, MicSource, Pacing, SAMPLE_RATE, SILENCE, SpeakerSink,
};
use dmn_audio::{
    CaptureState, CaptureTarget, MicCapture, MicReader, MicStatus, NodeTable, PipeWireContext,
    Playback, PlaybackStatus, PlaybackWriter, SinkTarget,
};

use crate::error::Error;
use crate::receive::Speakers;

/// How much audio the capture ring holds. It has to outlast a look that is late: the
/// transmit task looks every [`RING_LOOK`](crate::pace::RING_LOOK), and a stall of the
/// runtime it shares with the SDK is made up for by forwarding what built up. Only a stall
/// longer than this loses audio, and `dmn-audio` counts it (`MicStatus::dropped`).
const CAPTURE_RING_MS: u32 = 200;
/// How much audio the playback ring holds. What is queued when a named speaker is unplugged
/// is played when it returns, so this is short; [`TARGET_FILL`] keeps what is in it shorter.
const PLAYBACK_RING_MS: u32 = 200;

/// How much the playback ring is kept filled to: a mix is made whenever it holds less. It is
/// the delay this side adds to what is heard, and the room a late look has before the sound
/// card finds the ring dry. PipeWire takes the ring a quantum at a time (10 ms is asked for,
/// and the graph's own, 21 ms by default, is what it may be given), so it has to be a few
/// quanta; it is kept there by the card's clock, not by a count of ticks, so it does not
/// drift. The ring holds at most a frame more than this.
const TARGET_FILL: usize = 4 * FRAME_LEN;

/// How long `devices` waits for PipeWire to finish listing what it has.
pub const LISTING_WAIT: Duration = Duration::from_secs(3);

fn pipewire_error(doing: &'static str, detail: impl std::fmt::Display) -> Error {
    Error::PipeWire {
        doing,
        detail: detail.to_string(),
    }
}

/// The connection to PipeWire, shared by the microphone and the speakers. Cloning shares it;
/// the connection closes when the last clone and everything opened through it are gone.
#[derive(Clone)]
pub struct PipeWire {
    context: Arc<PipeWireContext>,
}

impl PipeWire {
    /// Connects to the PipeWire server this user's session runs.
    ///
    /// # Errors
    ///
    /// [`Error::PipeWire`] if there is none to reach.
    pub fn connect() -> Result<Self, Error> {
        let context = PipeWireContext::connect()
            .map_err(|error| pipewire_error("connect to PipeWire", error))?;
        Ok(Self {
            context: Arc::new(context),
        })
    }

    /// What PipeWire offers, once it has finished listing it or `wait` has passed. The
    /// table says whether it finished (`synced`).
    #[must_use]
    pub fn nodes(&self, wait: Duration) -> NodeTable {
        self.context.wait_for_sync(wait)
    }

    /// Opens the microphone: `configured` is a `node.name` as `devices` lists it, or `None`
    /// (or `default`) for PipeWire's default. The stream is open until the result is dropped.
    ///
    /// # Errors
    ///
    /// [`Error::PipeWire`] if the stream cannot be created.
    pub fn microphone(&self, configured: Option<&str>) -> Result<PipeWireMic, Error> {
        let target = CaptureTarget::mic(configured.unwrap_or_default());
        let mut capture = MicCapture::new(
            Arc::clone(&self.context),
            target,
            SAMPLE_RATE,
            CAPTURE_RING_MS,
        )
        .map_err(|error| pipewire_error("open the microphone", error))?;
        let status = capture.status();
        let ring = capture
            .start()
            .map_err(|error| pipewire_error("open the microphone", error))?;
        Ok(PipeWireMic {
            capture,
            ring,
            status,
            was: None,
        })
    }

    /// Opens the speakers: `configured` is a `node.name` as `devices` lists it, or `None`
    /// (or `default`) for PipeWire's default, which the session manager moves the stream to
    /// whenever it changes. The stream is open until the result is dropped.
    ///
    /// # Errors
    ///
    /// [`Error::PipeWire`] if the stream cannot be created.
    pub fn speakers(&self, configured: Option<&str>) -> Result<PipeWireSpeakers, Error> {
        let target = SinkTarget::from_config(configured.unwrap_or_default());
        let mut playback = Playback::new(
            Arc::clone(&self.context),
            target,
            SAMPLE_RATE,
            PLAYBACK_RING_MS,
        )
        .map_err(|error| pipewire_error("open the speakers", error))?;
        let status = playback.status();
        let ring = playback
            .start()
            .map_err(|error| pipewire_error("open the speakers", error))?;
        Ok(PipeWireSpeakers {
            playback,
            ring,
            status,
            idle: IdleClock::default(),
        })
    }
}

/// Whether the microphone can be transmitted from at this moment, and if not, why.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Availability {
    /// The configured microphone, or the default one, is delivering audio.
    Streaming,
    /// A microphone was named in the configuration, it is not there, and the default one is
    /// being captured in its place. Not a microphone to transmit from: see [`availability`].
    StandIn,
    /// Nothing is being delivered: the stream is in this state (`connecting` while the
    /// session manager moves it after the default microphone was unplugged, `disconnected`
    /// for a named one that was).
    Waiting(&'static str),
}

/// Decides whether the capture can be transmitted from, from what `dmn-audio` reports.
///
/// Three of its behaviours bear on this (issue #184):
///
/// - The default microphone being unplugged is **not** reported as `Disconnected`: the
///   session manager moves the stream, and the state shows `Connecting` meanwhile. So the
///   microphone counts as there only while the state is `Streaming` and the format has
///   settled; every other state is "no microphone".
/// - A named microphone that is absent at the start is stood in for by the default one
///   (`stand_in`). **Decision: it is not transmitted from.** The user named that device on
///   purpose (a headset, an interface); the default may be the laptop's own, in the room
///   with other people. Sending from a microphone nobody chose is the worse failure; staying
///   silent with a status that says so is the safer one. Audio from the stand-in is
///   discarded, and the gate stays shut until the named microphone appears (`dmn-audio` then
///   moves the stream to it). The same reasoning is why a named microphone that goes away
///   later is not stood in for either: `dmn-audio` reports it `Disconnected`.
/// - A named speaker that is absent is never replaced by the default one, which needs no
///   decision here: playback is `Disconnected` until it returns ([`put_frame`]).
/// - A microphone switched to inside a session (`docs/design/tui-voice.md` §20.5) is a new
///   stream, started as the first one was, so the same holds for it. So does a name that is
///   not a microphone's: `dmn-audio` looks for the name among sources only, so a speaker's
///   name is a microphone that is absent, and its monitor is never captured. The speakers
///   likewise look for their name among sinks only.
#[must_use]
pub fn availability(state: CaptureState, delivering: bool, stand_in: bool) -> Availability {
    if stand_in {
        return Availability::StandIn;
    }
    match (state, delivering) {
        (CaptureState::Streaming, true) => Availability::Streaming,
        // Streaming, and PipeWire has not yet said what format: nothing is in the ring.
        (CaptureState::Streaming, false) => Availability::Waiting("negotiating"),
        (state, _) => Availability::Waiting(state.label()),
    }
}

/// The ring a microphone's samples arrive in. A seam for tests: a real one is a `MicReader`.
pub trait Samples {
    /// Samples waiting.
    fn queued(&self) -> usize;
    /// Moves up to `out.len()` waiting samples into `out`, oldest first, and says how many.
    fn pop(&mut self, out: &mut [f32]) -> usize;
}

impl Samples for MicReader {
    fn queued(&self) -> usize {
        MicReader::queued(self)
    }

    fn pop(&mut self, out: &mut [f32]) -> usize {
        MicReader::pop(self, out)
    }
}

/// Throws away the `count` oldest samples.
fn discard(ring: &mut impl Samples, mut count: usize) {
    let mut scratch = SILENCE;
    while count > 0 {
        let take = count.min(FRAME_LEN);
        let got = ring.pop(&mut scratch[..take]);
        if got == 0 {
            break;
        }
        count -= got;
    }
}

/// What one read of the microphone does with the capture ring.
///
/// While the microphone is not usable ([`Availability::Streaming`] is the only usable one)
/// everything in the ring is thrown away, so that audio from before an unplug, or from a
/// stand-in, is never the first thing sent when the microphone is back. Otherwise a whole
/// frame is handed over when there is one, oldest first. A reader that was late is not
/// skipped ahead: it reads again, and gets the next frame, until the ring has none (the
/// transmit task does, and a press discards what was waiting before it: `transmit.rs`).
pub fn take_frame(ring: &mut impl Samples, usable: bool, frame: &mut Frame) -> MicRead {
    if !usable {
        let all = ring.queued();
        discard(ring, all);
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

/// The microphone: PipeWire's default, or the one named in the configuration. It is open,
/// and PipeWire's desktop indicator shows it, from [`PipeWire::microphone`] until it is
/// dropped.
///
/// It yields captured audio and nothing more: it is for sending only after the transmit
/// gate has passed it (`transmit.rs`).
pub struct PipeWireMic {
    // Declaration order is drop order: the stream closes before its ring goes.
    #[expect(
        dead_code,
        reason = "held so the stream stays open until this is dropped"
    )]
    capture: MicCapture,
    ring: MicReader,
    status: Arc<MicStatus>,
    /// What the last read found, so that only a change is logged.
    was: Option<Availability>,
}

impl PipeWireMic {
    /// The stream's counters (`MicStatus::pushed`, `MicStatus::dropped`), shared with the
    /// stream: still readable after the microphone has been handed to the transmit task.
    #[must_use]
    pub fn status(&self) -> Arc<MicStatus> {
        Arc::clone(&self.status)
    }

    /// Logs a change in whether the microphone can be used. Never called from a PipeWire
    /// callback.
    fn note(&mut self, now: Availability) {
        if self.was == Some(now) {
            return;
        }
        let first = self.was.is_none();
        self.was = Some(now);
        match now {
            Availability::Streaming if !first => log::info!("the microphone is back"),
            Availability::Streaming => {}
            Availability::StandIn => log::warn!(
                "the microphone named in the configuration is not there: nothing is \
                 transmitted until it is, and the default microphone is not used in its place"
            ),
            Availability::Waiting(why) if first => {
                log::info!("waiting for the microphone ({why})");
            }
            Availability::Waiting(why) => {
                log::warn!("no microphone ({why}): the gate is shut until one is back");
            }
        }
    }
}

impl MicSource for PipeWireMic {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        let now = availability(
            self.status.state(),
            self.status.delivered_rate().is_some(),
            self.status.fallback(),
        );
        self.note(now);
        take_frame(&mut self.ring, now == Availability::Streaming, frame)
    }

    /// The sound card's clock paces the microphone: it is read when the ring has a frame.
    fn pacing(&self) -> Pacing {
        Pacing::Ring
    }

    /// Empties the capture ring, down to the last sample: what is left of a frame would
    /// otherwise be the start of the first frame sent after a press.
    fn forget_waiting(&mut self) {
        let all = self.ring.queued();
        discard(&mut self.ring, all);
    }
}

impl std::fmt::Debug for PipeWireMic {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The device's name is left out: names are never logged (tui-voice.md §20.7).
        f.debug_struct("PipeWireMic").finish_non_exhaustive()
    }
}

/// The ring the mix is written to. A seam for tests: a real one is a `PlaybackWriter`.
pub trait Room {
    /// Samples written and not yet played.
    fn queued(&self) -> usize;
    /// Samples the ring can still take.
    fn vacant(&self) -> usize;
    /// Writes as many of `samples` as fit and says how many.
    fn write(&mut self, samples: &[f32]) -> usize;
}

impl Room for PlaybackWriter {
    fn queued(&self) -> usize {
        PlaybackWriter::queued(self)
    }

    fn vacant(&self) -> usize {
        PlaybackWriter::vacant(self)
    }

    fn write(&mut self, samples: &[f32]) -> usize {
        PlaybackWriter::write(self, samples)
    }
}

/// What a write of one frame did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Put {
    /// The frame was queued.
    Queued,
    /// Nothing is playing (a named speaker is unplugged, or the stream is not up yet): the
    /// frame was dropped, so that nothing more piles up behind what the ring already held
    /// (at most [`TARGET_FILL`], which is played when the speaker is back).
    NotPlaying,
    /// The ring could not take a whole frame. [`frames_wanted`] asks for a frame only when
    /// the ring holds less than [`TARGET_FILL`], which is far below its size, so this is
    /// the ring being full of something else: the frame was dropped.
    NoRoom,
}

/// What one write of a frame does with the playback ring: it queues it. Nothing is added
/// before it or dropped for it: how much the ring holds is kept by how many frames are asked
/// for ([`frames_wanted`]), not by correcting afterwards.
pub fn put_frame(ring: &mut impl Room, playing: bool, frame: &Frame) -> Put {
    if !playing {
        return Put::NotPlaying;
    }
    // Whole or not at all: part of a frame followed by the next frame is a click.
    if ring.vacant() < FRAME_LEN || ring.write(frame) < FRAME_LEN {
        return Put::NoRoom;
    }
    Put::Queued
}

/// The frames owed to the speakers when they are not playing (a named speaker is unplugged,
/// or the stream is not up yet). The ring does not drain then, so it cannot say when a frame
/// is due; without something that does, the mix would stop and the speakers' jitter buffers
/// would fill and drop. This is a clock of one frame, for as long as they are not playing.
/// The frames it is owed for are dropped by [`put_frame`].
#[derive(Debug, Default)]
pub struct IdleClock {
    since: Option<Instant>,
}

impl IdleClock {
    /// The most frames owed at once: a clock that was not asked for longer than this starts
    /// again instead of paying out a burst.
    const MAX_OWED: u128 = 4;

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
        if owed > Self::MAX_OWED {
            self.since = Some(now);
            return Self::MAX_OWED as usize;
        }
        // At most MAX_OWED, so the conversions cannot fail.
        let owed_frames = u32::try_from(owed).unwrap_or(0);
        self.since = Some(since + frame * owed_frames);
        owed_frames as usize
    }
}

/// How many frames to mix now for speakers whose ring holds `ring.queued()` samples: as
/// many as bring it up to [`TARGET_FILL`], none if it is there already. The sound card
/// empties the ring at its own pace, so this is the card's clock setting the pace of the
/// mix: a frame is mixed when the ring has room for one, and a look that comes late is owed
/// more than one. At the start the ring is empty, so the first look mixes up to the
/// target fill, and a ring that ran dry is filled again the same way, with no silence added.
///
/// When the speakers are not playing the ring cannot say, and `idle` does ([`IdleClock`]).
pub fn frames_wanted(ring: &impl Room, playing: bool, idle: &mut IdleClock, now: Instant) -> usize {
    if !playing {
        return idle.due(now);
    }
    idle.reset();
    TARGET_FILL
        .saturating_sub(ring.queued())
        .div_ceil(FRAME_LEN)
}

/// The speakers: PipeWire's default output, or the one named in the configuration. The
/// stream is open from [`PipeWire::speakers`] until it is dropped.
pub struct PipeWireSpeakers {
    // Declaration order is drop order: the stream closes before its ring goes.
    #[expect(
        dead_code,
        reason = "held so the stream stays open until this is dropped"
    )]
    playback: Playback,
    ring: PlaybackWriter,
    status: Arc<PlaybackStatus>,
    idle: IdleClock,
}

impl PipeWireSpeakers {
    /// The stream's counters (`PlaybackStatus::played`, `silence`, `underruns`), shared with
    /// the stream: still readable after the speakers have been handed to the mix task.
    #[must_use]
    pub fn status(&self) -> Arc<PlaybackStatus> {
        Arc::clone(&self.status)
    }
}

impl PipeWireSpeakers {
    /// Whether the stream is up and playing the ring.
    fn playing(&self) -> bool {
        self.status.state() == CaptureState::Streaming && self.status.playing_rate().is_some()
    }
}

impl SpeakerSink for PipeWireSpeakers {
    fn play(&mut self, frame: &Frame) {
        let playing = self.playing();
        put_frame(&mut self.ring, playing, frame);
    }

    /// The sound card's clock paces the mix: a frame is made whenever the ring is short.
    fn pacing(&self) -> Pacing {
        Pacing::Ring
    }

    fn frames_wanted(&mut self) -> usize {
        let playing = self.playing();
        frames_wanted(&self.ring, playing, &mut self.idle, Instant::now())
    }
}

/// Playing while the stream is up and playing the ring: not while it is still being set up,
/// nor while speakers named and absent are waited for, which are never replaced by others.
impl Speakers for PipeWireSpeakers {
    fn playing(&self) -> bool {
        PipeWireSpeakers::playing(self)
    }
}

impl std::fmt::Debug for PipeWireSpeakers {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The device's name is left out: names are never logged (tui-voice.md §20.7).
        f.debug_struct("PipeWireSpeakers").finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;

    use super::*;

    /// A ring made of a queue, standing in for the real ones.
    #[derive(Default)]
    struct Fake {
        samples: VecDeque<f32>,
        /// How many samples `write` has accepted, for the ring that cannot take more.
        room: Option<usize>,
    }

    impl Samples for Fake {
        fn queued(&self) -> usize {
            self.samples.len()
        }

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

    impl Room for Fake {
        fn queued(&self) -> usize {
            self.samples.len()
        }

        fn vacant(&self) -> usize {
            self.room
                .map_or(usize::MAX, |room| room.saturating_sub(self.samples.len()))
        }

        fn write(&mut self, samples: &[f32]) -> usize {
            let fits = self.room.map_or(samples.len(), |room| {
                room.saturating_sub(self.samples.len())
            });
            let n = fits.min(samples.len());
            self.samples.extend(&samples[..n]);
            n
        }
    }

    fn ramp(from: usize, count: usize) -> Fake {
        Fake {
            samples: (from..from + count).map(|i| i as f32).collect(),
            room: None,
        }
    }

    #[test]
    fn only_a_streaming_microphone_that_is_not_a_stand_in_counts_as_there() {
        struct Case {
            state: CaptureState,
            delivering: bool,
            stand_in: bool,
            want: Availability,
        }
        let case = |state, delivering, stand_in, want| Case {
            state,
            delivering,
            stand_in,
            want,
        };
        let cases = [
            case(
                CaptureState::Streaming,
                true,
                false,
                Availability::Streaming,
            ),
            // The default microphone was unplugged: not `Disconnected`, but `Connecting`.
            case(
                CaptureState::Connecting,
                false,
                false,
                Availability::Waiting("connecting"),
            ),
            // A named one was.
            case(
                CaptureState::Disconnected,
                false,
                false,
                Availability::Waiting("disconnected"),
            ),
            case(
                CaptureState::Error,
                false,
                false,
                Availability::Waiting("in error"),
            ),
            case(
                CaptureState::Idle,
                false,
                false,
                Availability::Waiting("idle"),
            ),
            // The stream is up and the format has not settled: nothing to read yet.
            case(
                CaptureState::Streaming,
                false,
                false,
                Availability::Waiting("negotiating"),
            ),
            // The state a state could be stale in: a format that is gone is no microphone.
            case(
                CaptureState::Connecting,
                true,
                false,
                Availability::Waiting("connecting"),
            ),
            // A named microphone that is absent, with the default one captured instead: the
            // decision is not to transmit, whatever else the status says.
            case(CaptureState::Streaming, true, true, Availability::StandIn),
            case(CaptureState::Connecting, false, true, Availability::StandIn),
        ];
        for c in cases {
            assert_eq!(
                availability(c.state, c.delivering, c.stand_in),
                c.want,
                "{:?} delivering={} stand_in={}",
                c.state,
                c.delivering,
                c.stand_in
            );
        }
    }

    #[test]
    fn a_read_hands_over_whole_frames_in_order() {
        let mut ring = ramp(0, 2 * FRAME_LEN + 100);
        let mut frame = SILENCE;
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
        assert_eq!(frame[0], 0.0);
        assert_eq!(frame[FRAME_LEN - 1], (FRAME_LEN - 1) as f32);
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
        assert_eq!(frame[0], FRAME_LEN as f32, "no sample lost or repeated");
        // 100 samples are not a frame: asked again later, and the frame is left as it was.
        let before = frame;
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::NotYet);
        assert_eq!(frame, before);
        assert_eq!(
            Samples::queued(&ring),
            100,
            "the partial frame is kept for the next"
        );
    }

    #[test]
    fn a_microphone_that_is_not_there_gives_nothing_and_forgets_what_was_waiting() {
        let mut ring = ramp(0, 3 * FRAME_LEN);
        let mut frame = SILENCE;
        assert_eq!(
            take_frame(&mut ring, false, &mut frame),
            MicRead::NoMicrophone
        );
        assert_eq!(frame, SILENCE, "the frame was not touched");
        assert_eq!(
            Samples::queued(&ring),
            0,
            "audio from before the unplug is not what is sent when the microphone is back"
        );
        // Back: the first frame is new audio.
        ring.samples
            .extend((1000..1000 + FRAME_LEN).map(|i| i as f32));
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
        assert_eq!(frame[0], 1000.0);
    }

    /// A late read is not skipped ahead: it gets every frame the ring built up, oldest first,
    /// in as many reads as there are frames.
    #[test]
    fn a_reader_that_fell_behind_catches_up_without_dropping_a_sample() {
        let frames = 15;
        let mut ring = ramp(0, frames * FRAME_LEN + 7);
        let mut frame = SILENCE;
        for n in 0..frames {
            assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
            assert_eq!(frame[0], (n * FRAME_LEN) as f32, "frame {n}, in order");
            assert_eq!(frame[FRAME_LEN - 1], ((n + 1) * FRAME_LEN - 1) as f32);
        }
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::NotYet);
        assert_eq!(Samples::queued(&ring), 7, "and the partial frame is kept");
    }

    fn room_with(samples: usize) -> Fake {
        ramp(0, samples)
    }

    /// The speakers' side of the same: a ring is filled to its target and no further, by
    /// asking for frames, however the card drains it.
    #[test]
    fn frames_are_wanted_while_the_ring_holds_less_than_its_target_and_not_after() {
        let mut idle = IdleClock::default();
        let now = Instant::now();
        let wanted = |samples: usize, idle: &mut IdleClock| {
            frames_wanted(&room_with(samples), true, idle, now)
        };
        let target_frames = TARGET_FILL / FRAME_LEN;
        // Empty (the start, or a ring that ran dry): the whole target at once.
        assert_eq!(wanted(0, &mut idle), target_frames);
        // The card took a frame, then a bit more than that.
        assert_eq!(wanted(TARGET_FILL - FRAME_LEN, &mut idle), 1);
        assert_eq!(wanted(TARGET_FILL - FRAME_LEN - 1, &mut idle), 2);
        // A frame short by one sample is still short: writing a whole frame passes the target.
        assert_eq!(wanted(TARGET_FILL - 1, &mut idle), 1);
        // At the target, and above it: nothing, whatever the timer would have said.
        assert_eq!(wanted(TARGET_FILL, &mut idle), 0);
        assert_eq!(wanted(TARGET_FILL + FRAME_LEN, &mut idle), 0);
    }

    /// Whatever the card does, asking for what is wanted and writing it keeps the ring
    /// between the target and the target plus a frame, without a frame dropped or a sample
    /// of silence added: no drift to correct.
    #[test]
    fn a_ring_drained_at_any_rate_stays_within_a_frame_of_its_target() {
        let mut ring = Fake::default();
        let mut idle = IdleClock::default();
        let now = Instant::now();
        let frame = [0.5; FRAME_LEN];
        let (mut written, mut drained) = (0_usize, 0_usize);
        // A card slightly faster than the 10 ms the timer would have kept, then slower, with
        // drains of odd sizes (a quantum of 1024) and looks of every size in between.
        for step in 0..20_000_usize {
            let drain = match step % 3 {
                0 => 1024,
                1 => 0,
                _ => 17 + step % 5,
            };
            let got = ring.samples.len().min(drain);
            ring.samples.drain(..got);
            drained += got;
            for _ in 0..frames_wanted(&ring, true, &mut idle, now) {
                assert_eq!(put_frame(&mut ring, true, &frame), Put::Queued);
                written += FRAME_LEN;
            }
            let queued = Room::queued(&ring);
            assert!(queued >= TARGET_FILL, "step {step}: {queued} queued");
            assert!(queued < TARGET_FILL + FRAME_LEN, "step {step}: {queued}");
        }
        assert_eq!(
            written,
            drained + Room::queued(&ring),
            "no sample lost or added"
        );
    }

    #[test]
    fn nothing_is_queued_for_speakers_that_are_not_playing() {
        let mut ring = room_with(FRAME_LEN);
        let frame = [0.5; FRAME_LEN];
        assert_eq!(put_frame(&mut ring, false, &frame), Put::NotPlaying);
        assert_eq!(Room::queued(&ring), FRAME_LEN, "nothing was added");
    }

    #[test]
    fn a_ring_with_no_room_for_a_frame_says_so() {
        let mut ring = Fake {
            samples: VecDeque::new(),
            room: Some(FRAME_LEN - 1),
        };
        let frame = [0.5; FRAME_LEN];
        assert_eq!(put_frame(&mut ring, true, &frame), Put::NoRoom);
        assert_eq!(Room::queued(&ring), 0, "no part of the frame was queued");
        let mut ring = Fake::default();
        assert_eq!(put_frame(&mut ring, true, &frame), Put::Queued);
        assert_eq!(Room::queued(&ring), FRAME_LEN, "only the frame, no silence");
    }

    /// While the speakers are not playing the ring cannot say when a frame is due, so a
    /// clock of one frame does, and it does not pay out a burst after a long gap.
    #[test]
    fn speakers_that_are_not_playing_are_owed_a_frame_every_ten_milliseconds() {
        let ring = Fake::default();
        let mut idle = IdleClock::default();
        let start = Instant::now();
        let at = |ms: u64| start + Duration::from_millis(ms);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(0)), 0, "starts");
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(9)), 0);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(10)), 1);
        // A look that is 25 ms on is owed two, and the 5 ms over is kept.
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(35)), 2);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(39)), 0);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(40)), 1);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(50)), 1);
        // A long gap is not made up.
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(5_000)), 4);
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(5_005)), 0);
        // And when they play, the ring decides, and the clock starts again next time.
        assert_eq!(
            frames_wanted(&ring, true, &mut idle, at(5_010)),
            TARGET_FILL / FRAME_LEN
        );
        assert_eq!(frames_wanted(&ring, false, &mut idle, at(6_000)), 0);
    }

    #[test]
    fn the_rings_hold_what_is_asked_of_them() {
        let ring_samples = |ms: u32| (SAMPLE_RATE * ms / 1000) as usize;
        assert!(TARGET_FILL + FRAME_LEN <= ring_samples(PLAYBACK_RING_MS));
        assert!(ring_samples(PLAYBACK_RING_MS).is_multiple_of(FRAME_LEN));
        assert!(FRAME_LEN <= ring_samples(CAPTURE_RING_MS));
    }
}
