//! The real microphone and speakers: PipeWire, through `dmn-audio`, behind the two traits of
//! `conch-voice-audio` (`docs/design/conch-voice.md` §3, §9).
//!
//! Input: PipeWire's graph, as `dmn-audio` follows it. The microphone is a `MicCapture` and
//! the speakers a `Playback`, both mono `f32` at 48 kHz. Output: a [`MicSource`] that gives
//! the captured audio a frame at a time, and a [`SpeakerSink`] that takes the mix a frame at a
//! time. Owns: the one connection to PipeWire ([`PipeWire`]), the capture stream from the
//! moment the microphone is opened until it is dropped, and the playback stream from the
//! moment the speakers are opened until they are.
//!
//! What runs where. PipeWire's callbacks are `dmn-audio`'s: `mic_process` and
//! `playback_process`, in its `src/mic.rs` and `src/playback.rs`, each documented there call
//! by call as allocating nothing, locking nothing and logging nothing, and each only moving
//! samples between the stream's buffer and a lock-free ring. Nothing in this file runs in a
//! callback. This file is on the other side of the ring, in the transmit and mix tasks, where
//! it may allocate, lock and log, and does only the first and last sparingly.
//!
//! The decisions that are this file's own are small functions of plain values, so that they
//! are tested without PipeWire: [`availability`] (when the microphone counts as there),
//! [`take_frame`] (what a read does with the capture ring) and [`put_frame`] (what a write
//! does with the playback ring).

use std::sync::Arc;
use std::time::Duration;

use conch_voice_audio::{FRAME_LEN, Frame, MicRead, MicSource, SAMPLE_RATE, SILENCE, SpeakerSink};
use dmn_audio::{
    CaptureState, CaptureTarget, MicCapture, MicReader, MicStatus, NodeTable, PipeWireContext,
    Playback, PlaybackStatus, PlaybackWriter, SinkTarget,
};

use crate::error::Error;

/// How much audio the capture ring holds. It only has to outlast a read that is late: the
/// transmit task reads every 10 ms, and [`MAX_BACKLOG`] keeps what it holds short.
const CAPTURE_RING_MS: u32 = 200;
/// How much audio the playback ring holds. What is queued when a named speaker is unplugged
/// is played when it returns, so this is short; [`MAX_QUEUED`] keeps what is in it shorter.
const PLAYBACK_RING_MS: u32 = 200;

/// More than this much captured audio waiting means a read stalled (the tasks share their
/// threads with the SDK's), and the oldest is thrown away rather than sent late: a speaker
/// hears a press as it happens or not at all.
const MAX_BACKLOG: usize = 4 * FRAME_LEN;
/// What a read that finds more than [`MAX_BACKLOG`] leaves behind.
const KEEP_BACKLOG: usize = 2 * FRAME_LEN;

/// Silence written before the first frame, and again after the ring ran dry, so that the
/// mix task's timer (which does not run on the sound card's clock) can be a little late
/// without the speakers hearing a gap.
const PRIME_FRAMES: usize = 2;
/// A frame is dropped, not queued, when the ring already holds more than this: the sound card
/// is slower than the timer, and queuing on would delay what is heard by more each second.
const MAX_QUEUED: usize = 8 * FRAME_LEN;

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
            primed: false,
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
/// frame is handed over when there is one; and when a stalled reader has let more than
/// [`MAX_BACKLOG`] pile up, the oldest is dropped first so the audio heard is recent.
pub fn take_frame(ring: &mut impl Samples, usable: bool, frame: &mut Frame) -> MicRead {
    if !usable {
        let all = ring.queued();
        discard(ring, all);
        return MicRead::NoMicrophone;
    }
    let queued = ring.queued();
    if queued > MAX_BACKLOG {
        discard(ring, queued - KEEP_BACKLOG);
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
    capture: MicCapture,
    ring: MicReader,
    status: Arc<MicStatus>,
    /// What the last read found, so that only a change is logged.
    was: Option<Availability>,
}

impl PipeWireMic {
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
}

impl std::fmt::Debug for PipeWireMic {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PipeWireMic")
            .field("capturing", &self.capture.target())
            .finish_non_exhaustive()
    }
}

/// The ring the mix is written to. A seam for tests: a real one is a `PlaybackWriter`.
pub trait Room {
    /// Samples written and not yet played.
    fn queued(&self) -> usize;
    /// Writes as many of `samples` as fit and says how many.
    fn write(&mut self, samples: &[f32]) -> usize;
}

impl Room for PlaybackWriter {
    fn queued(&self) -> usize {
        PlaybackWriter::queued(self)
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
    /// frame was dropped, so that nothing stale is waiting to be played when it is.
    NotPlaying,
    /// The ring held too much already: the frame was dropped.
    TooMuchQueued,
}

/// What one write of a frame does with the playback ring.
///
/// `primed` says whether the cushion of silence is in the ring; it is false at the start and
/// after the speakers were gone, and this sets it.
pub fn put_frame(ring: &mut impl Room, playing: bool, primed: &mut bool, frame: &Frame) -> Put {
    if !playing {
        *primed = false;
        return Put::NotPlaying;
    }
    if ring.queued() > MAX_QUEUED {
        return Put::TooMuchQueued;
    }
    // A ring found empty after the first frame ran dry (an underrun is silence) and needs
    // its cushion again.
    if !*primed || ring.queued() == 0 {
        for _ in 0..PRIME_FRAMES {
            ring.write(&SILENCE);
        }
        *primed = true;
    }
    ring.write(frame);
    Put::Queued
}

/// The speakers: PipeWire's default output, or the one named in the configuration. The
/// stream is open from [`PipeWire::speakers`] until it is dropped.
pub struct PipeWireSpeakers {
    // Declaration order is drop order: the stream closes before its ring goes.
    playback: Playback,
    ring: PlaybackWriter,
    status: Arc<PlaybackStatus>,
    primed: bool,
}

impl SpeakerSink for PipeWireSpeakers {
    fn play(&mut self, frame: &Frame) {
        let playing =
            self.status.state() == CaptureState::Streaming && self.status.playing_rate().is_some();
        put_frame(&mut self.ring, playing, &mut self.primed, frame);
    }
}

impl std::fmt::Debug for PipeWireSpeakers {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PipeWireSpeakers")
            .field("playing_to", &self.playback.target())
            .finish_non_exhaustive()
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

    #[test]
    fn a_reader_that_fell_behind_skips_to_recent_audio() {
        let total = MAX_BACKLOG + 3 * FRAME_LEN;
        let mut ring = ramp(0, total);
        let mut frame = SILENCE;
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
        // The newest KEEP_BACKLOG samples were kept, and the first of them was read.
        assert_eq!(frame[0], (total - KEEP_BACKLOG) as f32);
        assert_eq!(Samples::queued(&ring), KEEP_BACKLOG - FRAME_LEN);

        // Exactly at the limit is not behind.
        let mut ring = ramp(0, MAX_BACKLOG);
        assert_eq!(take_frame(&mut ring, true, &mut frame), MicRead::Ready);
        assert_eq!(frame[0], 0.0);
    }

    fn room_with(samples: usize) -> Fake {
        ramp(0, samples)
    }

    #[test]
    fn the_first_frame_is_written_behind_a_cushion_of_silence() {
        let mut ring = Fake::default();
        let mut primed = false;
        let frame = [0.5; FRAME_LEN];
        assert_eq!(put_frame(&mut ring, true, &mut primed, &frame), Put::Queued);
        assert!(primed);
        assert_eq!(Room::queued(&ring), (PRIME_FRAMES + 1) * FRAME_LEN);
        let queued: Vec<f32> = ring.samples.iter().copied().collect();
        assert!(queued[..PRIME_FRAMES * FRAME_LEN].iter().all(|s| *s == 0.0));
        assert!(queued[PRIME_FRAMES * FRAME_LEN..].iter().all(|s| *s == 0.5));

        // The next frame follows with no more silence.
        assert_eq!(put_frame(&mut ring, true, &mut primed, &frame), Put::Queued);
        assert_eq!(Room::queued(&ring), (PRIME_FRAMES + 2) * FRAME_LEN);
    }

    #[test]
    fn a_ring_that_ran_dry_gets_its_cushion_back() {
        let mut ring = Fake::default();
        let mut primed = true;
        let frame = [0.5; FRAME_LEN];
        assert_eq!(put_frame(&mut ring, true, &mut primed, &frame), Put::Queued);
        assert_eq!(Room::queued(&ring), (PRIME_FRAMES + 1) * FRAME_LEN);
    }

    #[test]
    fn nothing_is_queued_for_speakers_that_are_not_playing() {
        let mut ring = room_with(FRAME_LEN);
        let mut primed = true;
        let frame = [0.5; FRAME_LEN];
        assert_eq!(
            put_frame(&mut ring, false, &mut primed, &frame),
            Put::NotPlaying
        );
        assert!(!primed, "the cushion is wanted again when they are back");
        assert_eq!(Room::queued(&ring), FRAME_LEN, "nothing was added");
    }

    #[test]
    fn a_ring_that_holds_too_much_drops_the_frame() {
        let mut ring = room_with(MAX_QUEUED + 1);
        let mut primed = true;
        let frame = [0.5; FRAME_LEN];
        assert_eq!(
            put_frame(&mut ring, true, &mut primed, &frame),
            Put::TooMuchQueued
        );
        assert_eq!(Room::queued(&ring), MAX_QUEUED + 1);
        // At the limit it still takes one.
        let mut ring = room_with(MAX_QUEUED);
        assert_eq!(put_frame(&mut ring, true, &mut primed, &frame), Put::Queued);
    }

    #[test]
    fn the_rings_are_shorter_than_their_limits_say() {
        let ring_samples = |ms: u32| (SAMPLE_RATE * ms / 1000) as usize;
        assert!(MAX_QUEUED + (PRIME_FRAMES + 1) * FRAME_LEN <= ring_samples(PLAYBACK_RING_MS));
        assert!(MAX_BACKLOG < ring_samples(CAPTURE_RING_MS));
    }
}
