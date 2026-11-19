//! Echo cancellation (issue #186; `docs/design/conch-voice.md` §3): the SDK's audio
//! processing module, WebRTC's, with the speakers' mix as its reference.
//!
//! Input: each frame the mix task hands the speakers ([`EchoCanceller::played`]), and each
//! frame the transmit task reads from the microphone ([`EchoCanceller::capture`]), both 48
//! kHz mono frames of 10 ms. Output: the captured frame, changed in place, with what of the
//! played audio the microphone picked up taken out of it. Owns: one processing module per
//! session, made only when `audio.echo_cancellation` is on ([`EchoCanceller::if_enabled`]); with it
//! off there is no module at all, and the frames are not touched.
//!
//! The module transforms the frames it is given and does nothing else: it opens no audio
//! device and sends nothing (§13 row 11). It is `webrtc::AudioProcessing` built on its own by
//! `webrtc-sys`'s `apm.cpp` (`BuiltinAudioProcessingBuilder().Build(CreateEnvironment())`):
//! no audio device module, no peer connection, no transport. Its two calls take a buffer
//! and write the result back into the same buffer.
//!
//! It is not a path to the track. The transmit task runs a captured frame through it before
//! the level detector and the gate see the frame, and the gate is still the last step
//! between the microphone and the track (`transmit.rs`): a frame the module has processed
//! goes onward only as a [`conch_voice_audio::GatedFrame`], which only the gate makes.
//!
//! What is enabled besides the echo canceller: WebRTC's high-pass filter, which takes out
//! the hum and the DC offset the canceller works badly with, and its noise suppression. Not
//! its automatic gain control: that would change the level the open microphone's threshold
//! (`audio.open_threshold_db`) is set against, and raise the quiet residue of an echo.

use std::sync::{Arc, Mutex, PoisonError};

use conch_voice_audio::{FRAME_LEN, Frame, SAMPLE_RATE};
use livekit::webrtc::native::apm::AudioProcessingModule;

/// The module's sample rate: every frame here is 48 kHz.
const RATE: i32 = SAMPLE_RATE as i32;
/// Every frame here is mono.
const CHANNELS: i32 = 1;
/// Full scale of an `i16` sample, which the module takes, for a sample in [-1, 1].
const FULL_SCALE: f32 = 32_767.0;

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

/// The module, shared by the transmit task (captured frames) and the mix task (played
/// frames). Cloning shares the one module.
#[derive(Clone)]
pub struct EchoCanceller {
    inner: Arc<Mutex<Inner>>,
}

struct Inner {
    module: AudioProcessingModule,
    /// Whether a failed call has been logged, so that it is logged once.
    failure_logged: bool,
}

impl std::fmt::Debug for EchoCanceller {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("EchoCanceller")
    }
}

impl EchoCanceller {
    /// The module for a session with `audio.echo_cancellation` set to `enabled`: `None`
    /// when it is off, and then no module is made. This is the only place one is.
    #[must_use]
    pub fn if_enabled(enabled: bool) -> Option<Self> {
        enabled.then(|| Self::with_parts(SESSION_PARTS))
    }

    fn with_parts(parts: Parts) -> Self {
        let module =
            AudioProcessingModule::new(parts.echo, parts.gain, parts.high_pass, parts.noise);
        Self {
            inner: Arc::new(Mutex::new(Inner {
                module,
                failure_logged: false,
            })),
        }
    }

    /// The speakers were given `frame`: it is what the microphone may pick up again, the
    /// module's reference. The frame itself is not changed.
    pub fn played(&self, frame: &Frame) {
        let mut samples = to_i16(frame);
        let mut inner = self.lock();
        let result = inner
            .module
            .process_reverse_stream(&mut samples, RATE, CHANNELS);
        if let Err(error) = result {
            inner.failed("played", &error.message);
        }
    }

    /// The microphone gave `frame`: what of the played audio it picked up is taken out, in
    /// place. Should the module fail, the frame is left as it was captured: the gate decides
    /// what is sent either way.
    pub fn capture(&self, frame: &mut Frame) {
        let mut samples = to_i16(frame);
        let mut inner = self.lock();
        match inner.module.process_stream(&mut samples, RATE, CHANNELS) {
            Ok(()) => from_i16(&samples, frame),
            Err(error) => inner.failed("captured", &error.message),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        // Nothing in a call can leave the module half changed in a way the next call would
        // mind; a poisoned lock is used as it is.
        self.inner.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

impl Inner {
    fn failed(&mut self, which: &str, message: &str) {
        if !self.failure_logged {
            self.failure_logged = true;
            log::warn!("echo cancellation: a {which} frame was not processed: {message}");
        }
    }
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

    /// Runs `canceller` over a call: each 10 ms the far end's frame is played, and the
    /// microphone's frame captured. Returns what was captured after processing.
    fn run(canceller: &EchoCanceller, played: &[Frame], heard: &[Frame]) -> Vec<Frame> {
        played
            .iter()
            .zip(heard)
            .map(|(far, near)| {
                canceller.played(far);
                let mut frame = *near;
                canceller.capture(&mut frame);
                frame
            })
            .collect()
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
            let processed = run(&EchoCanceller::with_parts(only_echo), &far, &heard);
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
            let unreferenced = run(&EchoCanceller::with_parts(only_echo), &silence, &heard);
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
        let Some(canceller) = EchoCanceller::if_enabled(true) else {
            panic!("the switch is on");
        };
        let processed = run(&canceller, &far, &heard);
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
        let processed = run(&EchoCanceller::with_parts(only_echo), &silence, &near);
        let tail = (seconds as usize - 3) * 100;
        let change_db = 10.0 * (energy(&near[tail..]) / energy(&processed[tail..])).log10();
        println!("own voice, nothing played: changed by {change_db:.1} dB");
        assert!(
            change_db.abs() < 3.0,
            "the voice changed by {change_db:.1} dB"
        );
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
        let Some(canceller) = EchoCanceller::if_enabled(true) else {
            panic!("the switch is on");
        };
        let started = Instant::now();
        let _ = run(&canceller, &far, &heard);
        let took = started.elapsed();
        let share = took.as_secs_f64() / f64::from(seconds) * 100.0;
        println!(
            "echo cancellation: {seconds} s of audio processed in {took:?}: {share:.2}% of one core"
        );
        assert!(took < Duration::from_secs(u64::from(seconds) / 2));
    }
}
