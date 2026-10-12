//! The audio logic of `conch-voice`, with no devices, no network and no native code.
//!
//! Input: 48 kHz mono `f32` frames of 10 ms (480 samples) from a microphone source and from
//! each remote speaker, and commands to open and shut the transmit gate.
//! Output: the frames the gate lets through, and one mixed frame per tick for the speakers.
//! Owns: the transmit gate (microphone audio is handed onward only while it is open), one
//! jitter buffer per speaker, and the mixer. Nothing here writes audio to disk.
//!
//! The edges are two traits, [`MicSource`] and [`SpeakerSink`], so the binary can put real
//! devices behind them. This crate ships the test implementations: [`ToneSource`] and
//! [`WavSource`] for the microphone, [`CountingSink`] for the speakers.
//!
//! A captured frame is meant to reach the network only as a [`GatedFrame`], and the only way
//! to get one is [`TransmitGate::pass`]: whatever feeds the track should take that type and
//! nothing else.
//!
//! Design: `docs/design/conch-voice.md` §3.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

mod error;
mod gate;
mod jitter;
mod mixer;
mod sink;
mod source;
mod wav;

pub use error::AudioError;
pub use gate::{
    ForceShut, GateConfig, GateShut, GateStatus, Gated, GatedFrame, OpenOutcome, ShutReason,
    TransmitGate,
};
pub use jitter::{JITTER_CAP_MS, JITTER_TARGET_MS, JitterBuffer, JitterStats, Popped, Pushed};
pub use mixer::Mixer;
pub use sink::{CountingSink, SignalMeter, SpeakerSink};
pub use source::{MicRead, MicSource, ToneSource};
pub use wav::WavSource;

/// Sample rate of every frame in this crate, in Hz.
pub const SAMPLE_RATE: u32 = 48_000;
/// Length of a frame, in milliseconds.
pub const FRAME_MS: u32 = 10;
/// Samples in a frame: 48 kHz × 10 ms.
pub const FRAME_LEN: usize = (SAMPLE_RATE * FRAME_MS / 1000) as usize;

/// One frame: 10 ms of 48 kHz mono audio, each sample nominally in [-1, 1].
pub type Frame = [f32; FRAME_LEN];

/// A frame of silence.
pub const SILENCE: Frame = [0.0; FRAME_LEN];

#[cfg(test)]
mod testutil {
    //! A small deterministic generator for property-style tests: the same seed always gives
    //! the same sequence, so a failure can be replayed.

    /// A linear congruential generator (Knuth's MMIX constants).
    pub struct Lcg(u64);

    impl Lcg {
        pub fn new(seed: u64) -> Self {
            Self(seed.wrapping_mul(0x9E37_79B9_7F4A_7C15) ^ 0xD1B5_4A32_D192_ED03)
        }

        pub fn next_u32(&mut self) -> u32 {
            self.0 = self
                .0
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            (self.0 >> 32) as u32
        }

        /// A value in `0..n`; `n` must not be zero.
        pub fn below(&mut self, n: u32) -> u32 {
            self.next_u32() % n
        }

        pub fn byte(&mut self) -> u8 {
            (self.next_u32() >> 24) as u8
        }
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn a_frame_is_480_samples() {
        assert_eq!(super::FRAME_LEN, 480);
    }
}
