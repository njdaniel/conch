//! Echo cancellation (issue #186; `docs/design/conch-voice.md` §3): the SDK's audio
//! processing module, WebRTC's, with what the speakers played as its reference.
//!
//! Input: each frame the speakers sent to the room, handed over by the mix task through an
//! [`EchoReference`], and each frame the transmit task reads from the microphone
//! ([`EchoCanceller::capture`]), both 48 kHz mono frames of 10 ms. Output: the captured frame,
//! changed in place, with what of the played audio the microphone picked up taken out of it.
//! Owns: one processing module per session, made only when `audio.echo_cancellation` is on
//! ([`EchoCanceller::if_enabled`]); with it off there is no module at all, and the frames are
//! not touched.
//!
//! The module transforms the frames it is given and does nothing else: it opens no audio
//! device and sends nothing (§13 row 11). It is `webrtc::AudioProcessing` built on its own by
//! `webrtc-sys`'s `apm.cpp` (`BuiltinAudioProcessingBuilder().Build(CreateEnvironment())`):
//! no audio device module, no peer connection, no transport. Its two calls take a buffer
//! and write the result back into the same buffer.
//!
//! One task owns it: the transmit task, which holds the [`EchoCanceller`]. The mix task holds
//! only the sending end of a bounded queue of played frames ([`EchoReference`]): it never
//! takes a lock and never waits on the module, and a frame that does not fit is dropped and
//! counted. Before each captured frame the transmit task gives the module every played frame
//! waiting, then the captured one. Neither task can hold the other up.
//!
//! It is not a path to the track. The transmit task runs a captured frame through it before
//! the level detector and the gate see the frame, and the gate is still the last step
//! between the microphone and the track (`transmit.rs`): a frame the module has processed
//! goes onward only as a [`conch_voice_audio::GatedFrame`], which only the gate makes.
//!
//! The module keeps history: a frame it puts out can still hold some of the audio it was
//! given before. Audio captured while the gate was shut must not reach the track that way,
//! so after each opening of the gate the transmit task sends silence in place of the
//! module's output for as long as it can still hold any of that audio above
//! [`FLOOR_RMS`] ([`EchoCanceller::gate_opened`], [`EchoCanceller::hide_history`]): at least
//! [`HISTORY_FRAMES`], the memory of its buffers, and longer the louder what it was given
//! last, by [`RINGING_DB_PER_FRAME`], at most [`MAX_HIDDEN_FRAMES`]. The module itself is
//! kept, not made again, so the echo path it has learnt is not lost at each press.
//!
//! What is enabled besides the echo canceller: WebRTC's high-pass filter, which takes out
//! the hum and the DC offset the canceller works badly with, and its noise suppression. Not
//! its automatic gain control: that would change the level the open microphone's threshold
//! (`audio.open_threshold_db`) is set against, and raise the quiet residue of an echo.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use conch_voice_audio::{FRAME_LEN, Frame, SAMPLE_RATE, SILENCE};
use livekit::webrtc::native::apm::AudioProcessingModule;
use tokio::sync::mpsc::{self, error::TrySendError};

/// The module's sample rate: every frame here is 48 kHz.
const RATE: i32 = SAMPLE_RATE as i32;
/// Every frame here is mono.
const CHANNELS: i32 = 1;
/// Full scale of an `i16` sample, which the module takes, for a sample in [-1, 1].
const FULL_SCALE: f32 = 32_767.0;

/// How many played frames may wait for the transmit task: 320 ms. The transmit task takes
/// them before every captured frame, so a few wait at most while the microphone delivers;
/// while it delivers nothing (none, or a stalled one) the queue fills and what is played
/// next is dropped and counted ([`EchoReference::dropped`]). The canceller finds the echo
/// path again from what follows.
pub const REFERENCE_QUEUE: usize = 32;

// What the module's output can still hold of audio it was given before. Two parts.
//
// Its buffers, a fixed length: the module splits a frame into three bands of 16 kHz and does
// its work on the lowest, so they are counted in samples of 16 kHz, 160 to a frame. Read from
// the headers of the libwebrtc conch-voice is built with (`webrtc-89d790b`,
// `include/modules/audio_processing`): [`HISTORY_FRAMES`].
//
// And a ringing that dies away: measured, since the prebuilt carries headers and not the
// `.cc` files that hold the filters' coefficients. Given a voice and then digital silence,
// the module (with any one of its parts on, the high-pass filter, the noise suppressor or
// the echo canceller, and the same with all three) puts out a low tone, 50 to 100 Hz, that
// falls by 3.3 dB a frame or more on the whole from the level of what it was last given,
// and reaches the `i16` floor 20 to 26 frames after a voice at -18 dBFS
// (`audio_captured_before_an_opening_is_hidden_until_the_module_has_let_go_of_it` prints
// it). How long is hidden follows from that: [`RINGING_DB_PER_FRAME`].

/// Samples of 16 kHz in one 10 ms frame.
const SPLIT_BAND_FRAME: usize = 160;
/// The three-band filter bank (`three_band_filter_bank.h`): `kMemorySize`, 15 samples, kept
/// by the analysis and again by the synthesis.
const FILTER_BANK_MEMORY: usize = 2 * 15;
/// The high-pass filter (`high_pass_filter.h`), a cascaded biquad: an infinite response. Its
/// coefficients are in `high_pass_filter.cc`, which the prebuilt does not carry, so this is
/// a nominal 250 samples for the fixed part; the ringing actually measured is longer, and is
/// what [`RINGING_DB_PER_FRAME`] covers.
const HIGH_PASS_RINGING: usize = 250;
/// The echo canceller, AEC3 (`aec3/aec3_common.h`, `frame_blocker.h`, `block_framer.h`): it
/// cuts 80-sample subframes into blocks of `kBlockSize`, 64, and joins them again, holding up
/// to a block back; and its suppression filter overlaps one block with the next
/// (`kFftLengthBy2`, 64).
const ECHO_CANCELLER_MEMORY: usize = 64 + 64;
/// Noise suppression (`ns/ns_common.h`): `kOverlapSize`, an FFT of `kFftSize` 256 over
/// frames of `kNsFrameSize` 160, so 96 samples of each frame are put out with the next one
/// (`process_synthesis_memory`, and `process_delay_memory` for the upper bands).
const NOISE_SUPPRESSOR_OVERLAP: usize = 256 - 160;

/// How many frames after it was given audio the module's output still holds it through its
/// buffers: the memory of each of its steps, in turn, rounded up to whole frames. 504
/// samples of 16 kHz, 31.5 ms: 4 frames. The fewest hidden after an opening.
pub const HISTORY_FRAMES: usize =
    (FILTER_BANK_MEMORY + HIGH_PASS_RINGING + ECHO_CANCELLER_MEMORY + NOISE_SUPPRESSOR_OVERLAP)
        .div_ceil(SPLIT_BAND_FRAME);

/// Below this RMS, -80 dB of full scale, a frame is taken to hold nothing: quieter than any
/// microphone's own noise, and three steps of the `i16` the module takes.
pub const FLOOR_RMS: f32 = 1e-4;

/// How fast what the module rings with is taken to die away, from the level of what it was
/// last given: 2.5 dB a frame, slower than the 3.3 dB measured (see above), so that a
/// ringing a little slower than measured is still hidden whole.
pub const RINGING_DB_PER_FRAME: f32 = 2.5;

/// The most frames hidden after an opening: 350 ms, what a frame at full scale needs at
/// [`RINGING_DB_PER_FRAME`] to fall to [`FLOOR_RMS`] (80 dB, 32 frames), and a little more.
pub const MAX_HIDDEN_FRAMES: usize = 35;

/// Which of the module's parts are on.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Parts {
    echo: bool,
    gain: bool,
    high_pass: bool,
    noise: bool,
}

/// What a session runs: the echo canceller with the high-pass filter and noise suppression,
/// and no gain control (the module doc says why).
const SESSION_PARTS: Parts = Parts {
    echo: true,
    gain: false,
    high_pass: true,
    noise: true,
};

/// The module, owned by the transmit task: it takes the played frames waiting in the queue,
/// then the captured frame.
pub struct EchoCanceller {
    module: AudioProcessingModule,
    /// What the speakers played, from the mix task.
    reference: mpsc::Receiver<Frame>,
    /// Frames still to be hidden since the gate last opened.
    hiding: usize,
    /// The loudest the module was given or put out lately, as an RMS, falling by
    /// [`RINGING_DB_PER_FRAME`] a frame: how loud what it may still ring with can be.
    recent: f32,
    /// Whether a failed call has been logged, so that it is logged once.
    failure_logged: bool,
}

/// The mix task's end: hands the module each frame the speakers played, without waiting.
#[derive(Clone)]
pub struct EchoReference {
    frames: mpsc::Sender<Frame>,
    dropped: Arc<AtomicU64>,
}

impl std::fmt::Debug for EchoCanceller {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("EchoCanceller")
    }
}

impl std::fmt::Debug for EchoReference {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("EchoReference")
    }
}

impl EchoCanceller {
    /// The module for a session with `audio.echo_cancellation` set to `enabled`, and the end
    /// the mix task hands it played frames through: `None` when it is off, and then no
    /// module is made. This is the only place one is.
    #[must_use]
    pub fn if_enabled(enabled: bool) -> Option<(Self, EchoReference)> {
        enabled.then(|| Self::with_parts(SESSION_PARTS))
    }

    fn with_parts(parts: Parts) -> (Self, EchoReference) {
        let module =
            AudioProcessingModule::new(parts.echo, parts.gain, parts.high_pass, parts.noise);
        let (frames, reference) = mpsc::channel(REFERENCE_QUEUE);
        let canceller = Self {
            module,
            reference,
            hiding: 0,
            recent: 0.0,
            failure_logged: false,
        };
        let sender = EchoReference {
            frames,
            dropped: Arc::new(AtomicU64::new(0)),
        };
        (canceller, sender)
    }

    /// The microphone gave `frame`: the played frames waiting are given to the module first,
    /// as its reference, and then what of them the microphone picked up is taken out of
    /// `frame`, in place. Should the module fail, the frame is left as it was captured: the
    /// gate decides what is sent either way.
    pub fn capture(&mut self, frame: &mut Frame) {
        // Bounded by the queue's size, so a mix task that keeps adding cannot hold this up.
        for _ in 0..REFERENCE_QUEUE {
            let Ok(played) = self.reference.try_recv() else {
                break;
            };
            let mut samples = to_i16(&played);
            if let Err(error) = self
                .module
                .process_reverse_stream(&mut samples, RATE, CHANNELS)
            {
                self.failed("played", &error.message);
            }
        }
        let given = rms(frame);
        let mut samples = to_i16(frame);
        match self.module.process_stream(&mut samples, RATE, CHANNELS) {
            Ok(()) => from_i16(&samples, frame),
            Err(error) => self.failed("captured", &error.message),
        }
        let falling = self.recent * ringing_per_frame();
        self.recent = falling.max(given).max(rms(frame));
    }

    /// The gate has just opened from shut: what the module puts out next can hold audio
    /// captured while it was shut, and is hidden ([`Self::hide_history`]) for as long as it
    /// can (the module doc): [`HISTORY_FRAMES`], or as many as it takes what it was given
    /// lately to die away below [`FLOOR_RMS`], at most [`MAX_HIDDEN_FRAMES`].
    pub fn gate_opened(&mut self) {
        self.hiding = frames_to_hide(self.recent);
    }

    /// `frame`, a frame the module has put out, about to be offered to the gate: silence in
    /// its place while it may still hold audio from before the gate opened.
    pub fn hide_history(&mut self, frame: &mut Frame) {
        if self.hiding > 0 {
            self.hiding -= 1;
            *frame = SILENCE;
        }
    }

    fn failed(&mut self, which: &str, message: &str) {
        if !self.failure_logged {
            self.failure_logged = true;
            log::warn!("echo cancellation: a {which} frame was not processed: {message}");
        }
    }
}

impl EchoReference {
    /// The speakers sent `frame` to the room: what the microphone may pick up again, the
    /// module's reference. Never waits: should the queue be full, the frame is dropped and
    /// counted.
    pub fn played(&self, frame: &Frame) {
        match self.frames.try_send(*frame) {
            Ok(()) => {}
            Err(TrySendError::Full(_)) => {
                if self.dropped.fetch_add(1, Ordering::Relaxed) == 0 {
                    log::debug!("echo cancellation: played frames are being dropped");
                }
            }
            // The transmit task is gone: the session is ending.
            Err(TrySendError::Closed(_)) => {}
        }
    }

    /// How many played frames were dropped because the queue was full.
    #[must_use]
    pub fn dropped(&self) -> u64 {
        self.dropped.load(Ordering::Relaxed)
    }
}

#[cfg(test)]
impl EchoReference {
    /// A reference with no module behind it, whose frames the test reads.
    pub(crate) fn for_test(capacity: usize) -> (Self, mpsc::Receiver<Frame>) {
        let (frames, received) = mpsc::channel(capacity);
        let reference = Self {
            frames,
            dropped: Arc::new(AtomicU64::new(0)),
        };
        (reference, received)
    }
}

/// How much of what the module rings with is left a frame later ([`RINGING_DB_PER_FRAME`]).
fn ringing_per_frame() -> f32 {
    10_f32.powf(-RINGING_DB_PER_FRAME / 20.0)
}

/// How many frames to hide after an opening, when the module was lately given or put out
/// as much as `recent` (an RMS): enough for that to die away below [`FLOOR_RMS`].
fn frames_to_hide(recent: f32) -> usize {
    let above_db = 20.0 * (recent / FLOOR_RMS).log10();
    let ringing = if above_db > 0.0 {
        // At most MAX_HIDDEN_FRAMES after the clamp, so the cast neither wraps nor saturates.
        (above_db / RINGING_DB_PER_FRAME)
            .ceil()
            .min(MAX_HIDDEN_FRAMES as f32) as usize
    } else {
        0
    };
    ringing.clamp(HISTORY_FRAMES, MAX_HIDDEN_FRAMES)
}

/// The RMS level of a frame, where full scale is 1.
fn rms(frame: &Frame) -> f32 {
    let energy: f32 = frame.iter().map(|sample| sample * sample).sum();
    (energy / FRAME_LEN as f32).sqrt()
}

/// A frame as the module takes it: `i16` samples, clamped to full scale.
fn to_i16(frame: &Frame) -> [i16; FRAME_LEN] {
    let mut samples = [0_i16; FRAME_LEN];
    for (out, sample) in samples.iter_mut().zip(frame) {
        // In range after the clamp, so the cast neither wraps nor saturates.
        let scaled = (sample.clamp(-1.0, 1.0) * FULL_SCALE).round() as i16;
        *out = scaled;
    }
    samples
}

/// The module's `i16` samples back into a frame.
fn from_i16(samples: &[i16; FRAME_LEN], frame: &mut Frame) {
    for (out, sample) in frame.iter_mut().zip(samples) {
        *out = f32::from(*sample) / FULL_SCALE;
    }
}

#[cfg(test)]
mod tests {
    use std::time::{Duration, Instant};

    use super::*;

    /// A deterministic noise source, so that a failure can be replayed.
    struct Noise(u64);

    impl Noise {
        /// A sample in [-1, 1].
        fn next(&mut self) -> f32 {
            self.0 = self
                .0
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            let top = (self.0 >> 40) as u32; // 24 bits
            (top as f32 / (1 << 23) as f32) - 1.0
        }
    }

    /// Far-end speech, roughly: noise kept to the voice's band (two one-pole low-pass
    /// filters, about 1.7 kHz), shaped by a syllable-rate envelope, 4 Hz, with pauses
    /// between words. Its peaks are near a quarter of full scale.
    fn far_end(seconds: u32) -> Vec<Frame> {
        far_end_from(Noise(0x186), seconds)
    }

    fn far_end_from(mut noise: Noise, seconds: u32) -> Vec<Frame> {
        let (mut low, mut lower) = (0.0_f32, 0.0_f32);
        let frames = seconds as usize * 100;
        let mut out = Vec::with_capacity(frames);
        let mut n = 0_usize;
        for _ in 0..frames {
            let mut frame = [0.0; FRAME_LEN];
            for sample in &mut frame {
                let t = n as f32 / SAMPLE_RATE as f32;
                let envelope = (std::f32::consts::TAU * 4.0 * t).sin().max(0.0);
                // A word lasts 1.5 s and is followed by half a second of pause.
                let talking = (t % 2.0) < 1.5;
                low += 0.2 * (noise.next() - low);
                lower += 0.2 * (low - lower);
                *sample = if talking { envelope * lower } else { 0.0 };
                n += 1;
            }
            out.push(frame);
        }
        out
    }

    /// What the microphone hears of `far`: the same audio `delay_frames` later, at
    /// `gain`, through a short echo path (a direct sound and one reflection).
    fn echo_of(far: &[Frame], delay_frames: usize, gain: f32) -> Vec<Frame> {
        let flat: Vec<f32> = far.iter().flatten().copied().collect();
        let delay = delay_frames * FRAME_LEN;
        let reflection = 96; // 2 ms later
        let mut heard = vec![0.0_f32; flat.len()];
        for (i, out) in heard.iter_mut().enumerate() {
            let direct = i.checked_sub(delay).map_or(0.0, |j| flat[j]);
            let bounced = i.checked_sub(delay + reflection).map_or(0.0, |j| flat[j]);
            *out = gain * (direct + 0.4 * bounced);
        }
        heard
            .chunks_exact(FRAME_LEN)
            .map(|chunk| {
                let mut frame = [0.0; FRAME_LEN];
                frame.copy_from_slice(chunk);
                frame
            })
            .collect()
    }

    fn energy(frames: &[Frame]) -> f64 {
        frames
            .iter()
            .flatten()
            .map(|s| f64::from(*s) * f64::from(*s))
            .sum()
    }

    /// Runs a module over a call: each 10 ms the far end's frame is played, and the
    /// microphone's frame captured. Returns what was captured after processing.
    fn run(
        (canceller, reference): &mut (EchoCanceller, EchoReference),
        played: &[Frame],
        heard: &[Frame],
    ) -> Vec<Frame> {
        played
            .iter()
            .zip(heard)
            .map(|(far, near)| {
                reference.played(far);
                let mut frame = *near;
                canceller.capture(&mut frame);
                frame
            })
            .collect()
    }

    fn session_module() -> (EchoCanceller, EchoReference) {
        let Some(module) = EchoCanceller::if_enabled(true) else {
            panic!("the switch is on");
        };
        module
    }

    /// With the switch off there is no module; with it on there is one.
    #[test]
    fn with_the_switch_off_no_module_is_made() {
        assert!(EchoCanceller::if_enabled(false).is_none());
        assert!(EchoCanceller::if_enabled(true).is_some());
    }

    /// The point of the module: the far end's voice, played and picked up again at half its
    /// level 60, 150 or 250 ms later (what a sound card's buffers and the room add), is
    /// mostly gone from what is captured once the canceller has converged. Only the echo
    /// canceller is on here, so that the noise suppressor cannot take the credit; and with a
    /// module given silence as its reference, the same capture keeps its energy, so it is
    /// the reference that does it.
    #[test]
    fn the_played_audio_is_taken_out_of_what_is_captured() {
        let seconds = 10;
        let far = far_end(seconds);
        let only_echo = Parts {
            echo: true,
            gain: false,
            high_pass: false,
            noise: false,
        };
        // The last 4 s, after the canceller has had 6 s to find the echo path.
        let tail = (seconds as usize - 4) * 100;
        for delay_frames in [6, 15, 25] {
            let heard = echo_of(&far, delay_frames, 0.5);
            let processed = run(&mut EchoCanceller::with_parts(only_echo), &far, &heard);
            let before = energy(&heard[tail..]);
            let after = energy(&processed[tail..]);
            let reduction_db = 10.0 * (before / after.max(f64::MIN_POSITIVE)).log10();
            println!(
                "echo {}0 ms late: reduced by {reduction_db:.1} dB",
                delay_frames
            );
            assert!(
                reduction_db >= 20.0,
                "echo {delay_frames}0 ms late: reduced by {reduction_db:.1} dB only"
            );

            let silence = vec![[0.0; FRAME_LEN]; far.len()];
            let unreferenced = run(&mut EchoCanceller::with_parts(only_echo), &silence, &heard);
            let kept_db = 10.0 * (before / energy(&unreferenced[tail..])).log10();
            println!(
                "echo {}0 ms late, no reference: changed by {kept_db:.1} dB",
                delay_frames
            );
            assert!(
                kept_db.abs() < 3.0,
                "with no reference the capture changed by {kept_db:.1} dB"
            );
        }
    }

    /// The session's module (echo, high-pass, noise suppression) does as well.
    #[test]
    fn the_session_module_takes_the_echo_out_too() {
        let seconds = 10;
        let far = far_end(seconds);
        let heard = echo_of(&far, 6, 0.5);
        let processed = run(&mut session_module(), &far, &heard);
        let tail = (seconds as usize - 4) * 100;
        let reduction_db = 10.0
            * (energy(&heard[tail..]) / energy(&processed[tail..]).max(f64::MIN_POSITIVE)).log10();
        println!("session module: echo reduced by {reduction_db:.1} dB");
        assert!(
            reduction_db >= 20.0,
            "the echo was reduced by {reduction_db:.1} dB only"
        );
    }

    /// The near end's own voice, with nothing played, comes through: the canceller takes
    /// out what was played, not what the user says.
    #[test]
    fn the_users_own_voice_comes_through_when_nothing_is_played() {
        let seconds = 6;
        let near = far_end(seconds);
        let silence = vec![[0.0; FRAME_LEN]; near.len()];
        let only_echo = Parts {
            echo: true,
            gain: false,
            high_pass: false,
            noise: false,
        };
        let processed = run(&mut EchoCanceller::with_parts(only_echo), &silence, &near);
        let tail = (seconds as usize - 3) * 100;
        let change_db = 10.0 * (energy(&near[tail..]) / energy(&processed[tail..])).log10();
        println!("own voice, nothing played: changed by {change_db:.1} dB");
        assert!(
            change_db.abs() < 3.0,
            "the voice changed by {change_db:.1} dB"
        );
    }

    /// Issue #186's review: the module keeps history, so speech captured before the gate
    /// opens can come out of it after. The session's module is given a talker's voice, at
    /// three levels (the loudest clipped), and then digital silence, the moment of the
    /// change moved a frame at a time across words, syllables and pauses: from the opening on
    /// nothing above [`FLOOR_RMS`] reaches the gate, the frames hidden included as silence.
    /// And the hiding is what does it: the first frame the module puts out after a loud
    /// change still holds the voice.
    #[test]
    fn audio_captured_before_an_opening_is_hidden_until_the_module_has_let_go_of_it() {
        let voice = far_end_from(Noise(0x5eed), 2);
        let mut leaked = 0_f64;
        let mut hidden_most = 0;
        for gain in [0.3_f32, 1.0, 4.0] {
            for change in (40..=160).step_by(3) {
                let (mut canceller, _reference) = session_module();
                for frame in &voice[..change] {
                    let mut frame = frame.map(|sample| sample * gain);
                    canceller.capture(&mut frame);
                }
                canceller.gate_opened();
                let hidden = canceller.hiding;
                hidden_most = hidden_most.max(hidden);
                let mut sent = Vec::new();
                for n in 0..50 {
                    let mut frame = SILENCE;
                    canceller.capture(&mut frame);
                    if n == 0 {
                        leaked = leaked.max(f64::from(rms(&frame)));
                    }
                    canceller.hide_history(&mut frame);
                    sent.push(rms(&frame));
                }
                for (n, level) in sent.iter().enumerate() {
                    assert!(
                        *level < FLOOR_RMS,
                        "gain {gain}, voice until frame {change}, {hidden} hidden: frame {n} \
                         after the opening holds {level}: {sent:?}"
                    );
                }
            }
        }
        println!("the most hidden: {hidden_most} frames; unhidden, the first frame held {leaked}");
        assert!(
            leaked > 1e-2,
            "the first frame out would hold the voice: {leaked}"
        );
    }

    /// Before an opening the module was given only silence, or a quiet room: only the frames
    /// its buffers hold are hidden. Given a voice up to the opening, more are.
    #[test]
    fn after_quiet_only_the_buffers_frames_are_hidden_and_after_a_voice_more() {
        assert_eq!(frames_to_hide(0.0), HISTORY_FRAMES);
        assert_eq!(frames_to_hide(FLOOR_RMS), HISTORY_FRAMES);
        // A quiet room, -70.5 dBFS: 9.5 dB above the floor, 4 frames at 2.5 dB.
        assert_eq!(frames_to_hide(3.0e-4), HISTORY_FRAMES);
        // -18 dBFS, a voice: 62 dB, 25 frames.
        assert_eq!(frames_to_hide(0.125), 25);
        assert_eq!(frames_to_hide(1.0), 32);
        assert_eq!(frames_to_hide(10.0), MAX_HIDDEN_FRAMES);

        let (mut canceller, _reference) = session_module();
        for _ in 0..100 {
            let mut frame = SILENCE;
            canceller.capture(&mut frame);
        }
        canceller.gate_opened();
        assert_eq!(canceller.hiding, HISTORY_FRAMES);
        for frame in far_end(1) {
            let mut frame = frame;
            canceller.capture(&mut frame);
        }
        canceller.gate_opened();
        assert!(canceller.hiding > HISTORY_FRAMES, "{}", canceller.hiding);
    }

    /// A frame hidden is silence, and only those counted after an opening are: here, after
    /// nothing was given, [`HISTORY_FRAMES`].
    #[test]
    fn the_frames_counted_after_an_opening_are_hidden_and_no_others() {
        let (mut canceller, _reference) = session_module();
        let loud = [0.5; FRAME_LEN];
        let mut frame = loud;
        canceller.hide_history(&mut frame);
        assert_eq!(frame, loud, "nothing is hidden before an opening");
        canceller.gate_opened();
        for n in 0..HISTORY_FRAMES {
            let mut frame = loud;
            canceller.hide_history(&mut frame);
            assert_eq!(frame, SILENCE, "frame {n} is hidden");
        }
        let mut frame = loud;
        canceller.hide_history(&mut frame);
        assert_eq!(frame, loud);
        assert_eq!(HISTORY_FRAMES, 4, "504 samples of 16 kHz");
    }

    /// Issue #186's review: the mix task never waits on the module. With nothing taking
    /// played frames (no captured frame comes), the queue fills and what follows is dropped
    /// and counted, at once; the next capture takes what waited, and the queue has room again.
    #[test]
    fn played_frames_never_wait_and_those_that_do_not_fit_are_dropped_and_counted() {
        let (mut canceller, reference) = session_module();
        let started = Instant::now();
        for _ in 0..REFERENCE_QUEUE + 10 {
            reference.played(&[0.1; FRAME_LEN]);
        }
        assert!(started.elapsed() < Duration::from_secs(1));
        assert_eq!(reference.dropped(), 10);
        let mut frame = SILENCE;
        canceller.capture(&mut frame);
        reference.played(&[0.1; FRAME_LEN]);
        assert_eq!(reference.dropped(), 10, "room again once taken");
        // The module gone (the session ending), a played frame is let go quietly.
        drop(canceller);
        reference.played(&[0.1; FRAME_LEN]);
        assert_eq!(reference.dropped(), 10);
    }

    /// A sample out of range is clamped, not wrapped.
    #[test]
    fn samples_out_of_range_are_clamped() {
        let mut frame = [0.0; FRAME_LEN];
        frame[0] = 2.0;
        frame[1] = -2.0;
        frame[2] = 0.5;
        let samples = to_i16(&frame);
        assert_eq!(samples[0], i16::MAX);
        assert_eq!(samples[1], -i16::MAX);
        assert_eq!(samples[2], 16_384);
        let mut back = [0.0; FRAME_LEN];
        from_i16(&samples, &mut back);
        assert!((back[2] - 0.5).abs() < 1e-4);
    }

    /// The added cost (issue #186): a minute of one talker, played and captured through the
    /// session's module, timed. Not a pass or fail of its own beyond keeping well within
    /// real time; run with `--ignored --nocapture` to read the figure.
    #[test]
    #[ignore = "a measurement: prints the CPU time a minute of audio costs; run by hand"]
    fn cost_of_a_minute_with_one_talker() {
        let seconds = 60;
        let far = far_end(seconds);
        let heard = echo_of(&far, 6, 0.5);
        let mut module = session_module();
        let started = Instant::now();
        let _ = run(&mut module, &far, &heard);
        let took = started.elapsed();
        let share = took.as_secs_f64() / f64::from(seconds) * 100.0;
        println!(
            "echo cancellation: {seconds} s of audio processed in {took:?}: {share:.2}% of one core"
        );
        assert!(took < Duration::from_secs(u64::from(seconds) / 2));
    }
}
