//! The audio logic of `conch-voice`, with no devices, no network and no native code.
//!
//! Input: 48 kHz mono `f32` frames of 10 ms (480 samples) from a microphone source and from
//! each remote speaker, and commands to open and shut the transmit gate.
//! Output: the frames the gate lets through, and one mixed frame per tick for the speakers.
//! Owns: the transmit gate (microphone audio is handed onward only while it is open), one
//! jitter buffer per speaker, and the mixer. Nothing here writes audio to disk.
//!
//! Design: `docs/design/conch-voice.md` §3. Empty until issue #181.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

/// Sample rate of every frame in this crate, in Hz.
pub const SAMPLE_RATE: u32 = 48_000;
/// Length of a frame, in milliseconds.
pub const FRAME_MS: u32 = 10;
/// Samples in a frame: 48 kHz × 10 ms.
pub const FRAME_LEN: usize = (SAMPLE_RATE * FRAME_MS / 1000) as usize;

#[cfg(test)]
mod tests {
    #[test]
    fn a_frame_is_480_samples() {
        assert_eq!(super::FRAME_LEN, 480);
    }
}
