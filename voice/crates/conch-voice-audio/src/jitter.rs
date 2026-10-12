//! One speaker's jitter buffer.

use std::fmt;

use crate::{AudioError, FRAME_MS, Frame, SILENCE};

/// How much audio a jitter buffer aims to hold, in milliseconds.
pub const JITTER_TARGET_MS: u32 = 40;
/// The most audio a jitter buffer ever holds, in milliseconds.
pub const JITTER_CAP_MS: u32 = 200;

/// The longest cap [`JitterBuffer::with_limits`] accepts, in milliseconds.
const LONGEST_CAP_MS: u32 = 10_000;

/// What [`JitterBuffer::push`] did with a frame. The frame given is always kept.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Pushed {
    /// There was room.
    Kept,
    /// The buffer was at its cap, so the oldest frame it held was dropped to make room.
    DroppedOldest,
}

/// What [`JitterBuffer::pop`] wrote.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Popped {
    /// The speaker's next frame.
    Audio,
    /// Silence: the buffer ran dry while playing. Counted as an underrun.
    Underrun,
    /// Silence: nothing is playing, and the buffer is waiting to fill to its target. This is
    /// the state before a speaker starts and after an underrun. Not counted.
    Waiting,
}

/// A jitter buffer's counters since it was made.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct JitterStats {
    /// Frames dropped, oldest first, because the buffer was at its cap.
    pub dropped_frames: u64,
    /// Times the buffer ran dry while playing.
    pub underruns: u64,
}

impl JitterStats {
    /// The audio dropped at the cap, in milliseconds.
    #[must_use]
    pub fn dropped_ms(&self) -> u64 {
        self.dropped_frames.saturating_mul(u64::from(FRAME_MS))
    }
}

/// One speaker's jitter buffer: frames go in as they arrive and come out one per tick.
///
/// Guarantees:
///
/// - Frames come out in the order they went in, each at most once and whole. Nothing is
///   repeated, reordered or blended.
/// - It never holds more than its cap (200 ms). A frame that arrives when it is full pushes
///   out the oldest frame held, which is counted, so a speaker is never heard later than the
///   cap.
/// - It starts playing once it holds its target (40 ms), so steady input comes out steady and
///   delayed by about the target. If it runs dry while playing, that tick is silence and is
///   counted as one underrun; it then waits to hold the target again before playing.
/// - Every [`pop`](Self::pop) writes a whole frame: audio, or silence.
///
/// All storage is allocated when the buffer is made. [`push`](Self::push),
/// [`pop`](Self::pop) and the accessors do not allocate, lock or log, so they may be called
/// next to a real-time callback.
pub struct JitterBuffer {
    /// As many slots as the cap; never empty.
    ring: Box<[Frame]>,
    target: usize,
    /// The slot of the oldest frame held.
    head: usize,
    held: usize,
    playing: bool,
    stats: JitterStats,
}

impl JitterBuffer {
    /// A buffer that aims to hold [`JITTER_TARGET_MS`] and never holds more than
    /// [`JITTER_CAP_MS`].
    #[must_use]
    pub fn new() -> Self {
        Self::build(JITTER_TARGET_MS, JITTER_CAP_MS)
    }

    /// A buffer with other limits.
    ///
    /// # Errors
    ///
    /// [`AudioError::InvalidSetting`] unless both are whole numbers of frames, the target is
    /// at least one frame, and the cap is at least the target and at most ten seconds.
    pub fn with_limits(target_ms: u32, cap_ms: u32) -> Result<Self, AudioError> {
        if target_ms < FRAME_MS || !target_ms.is_multiple_of(FRAME_MS) {
            return Err(AudioError::InvalidSetting {
                what: "the jitter buffer's target",
                must_be: "a whole number of 10 ms frames, at least one",
                got: f64::from(target_ms),
            });
        }
        if cap_ms < target_ms || cap_ms > LONGEST_CAP_MS || !cap_ms.is_multiple_of(FRAME_MS) {
            return Err(AudioError::InvalidSetting {
                what: "the jitter buffer's cap",
                must_be: "a whole number of 10 ms frames, from the target up to 10 s",
                got: f64::from(cap_ms),
            });
        }
        Ok(Self::build(target_ms, cap_ms))
    }

    fn build(target_ms: u32, cap_ms: u32) -> Self {
        let cap = ((cap_ms / FRAME_MS) as usize).max(1);
        let target = ((target_ms / FRAME_MS) as usize).clamp(1, cap);
        Self {
            ring: vec![SILENCE; cap].into_boxed_slice(),
            target,
            head: 0,
            held: 0,
            playing: false,
            stats: JitterStats::default(),
        }
    }

    /// Takes a frame that has arrived from the speaker. If the buffer is at its cap the
    /// oldest frame held is dropped to make room, and counted.
    pub fn push(&mut self, frame: &Frame) -> Pushed {
        let cap = self.ring.len();
        let outcome = if self.held == cap {
            self.head = (self.head + 1) % cap;
            self.held -= 1;
            self.stats.dropped_frames = self.stats.dropped_frames.saturating_add(1);
            Pushed::DroppedOldest
        } else {
            Pushed::Kept
        };
        let at = (self.head + self.held) % cap;
        if let Some(slot) = self.ring.get_mut(at) {
            *slot = *frame;
            self.held += 1;
        }
        if self.held >= self.target {
            self.playing = true;
        }
        outcome
    }

    /// Writes this tick's frame for the speaker into `out`: the oldest frame held, or silence.
    pub fn pop(&mut self, out: &mut Frame) -> Popped {
        if self.playing && self.held > 0 {
            *out = self.ring.get(self.head).copied().unwrap_or(SILENCE);
            self.head = (self.head + 1) % self.ring.len();
            self.held -= 1;
            return Popped::Audio;
        }
        *out = SILENCE;
        if self.playing {
            self.playing = false;
            self.stats.underruns = self.stats.underruns.saturating_add(1);
            Popped::Underrun
        } else {
            Popped::Waiting
        }
    }

    /// The audio held now, in milliseconds.
    #[must_use]
    pub fn held_ms(&self) -> u32 {
        // At most the cap, which is at most ten seconds.
        (self.held as u32).saturating_mul(FRAME_MS)
    }

    /// The counters since the buffer was made.
    #[must_use]
    pub fn stats(&self) -> JitterStats {
        self.stats
    }
}

impl Default for JitterBuffer {
    fn default() -> Self {
        Self::new()
    }
}

/// Prints no audio.
impl fmt::Debug for JitterBuffer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("JitterBuffer")
            .field("held_ms", &self.held_ms())
            .field("playing", &self.playing)
            .field("stats", &self.stats)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::FRAME_LEN;
    use crate::testutil::Lcg;

    /// Frame number `n` (from 0), each sample tagged with its place in the whole stream,
    /// counted from 1 so that no sample of it is silence. Exact in `f32` up to 2^24 samples.
    fn tagged(n: u64) -> Frame {
        let mut frame = SILENCE;
        for (i, sample) in frame.iter_mut().enumerate() {
            *sample = (n * FRAME_LEN as u64 + i as u64 + 1) as f32;
        }
        frame
    }

    /// The number of the tagged frame in `frame`, checking that it is one whole frame.
    fn frame_number(frame: &Frame) -> u64 {
        let first = frame[0] as u64;
        assert_eq!(
            (first - 1) % FRAME_LEN as u64,
            0,
            "not the start of a frame"
        );
        for (i, sample) in frame.iter().enumerate() {
            assert_eq!(*sample as u64, first + i as u64, "samples out of order");
        }
        (first - 1) / FRAME_LEN as u64
    }

    /// A buffer to pop into, filled with something that is neither audio nor silence.
    fn scratch() -> Frame {
        [f32::NAN; FRAME_LEN]
    }

    #[test]
    fn steady_input_comes_out_steady_and_delayed_by_about_the_target() {
        let mut buffer = JitterBuffer::new();
        let mut out = scratch();
        for tick in 0..500_u64 {
            assert_eq!(buffer.push(&tagged(tick)), Pushed::Kept);
            let popped = buffer.pop(&mut out);
            if tick < 3 {
                // Still filling to 40 ms.
                assert_eq!(popped, Popped::Waiting, "tick {tick}");
                assert_eq!(out, SILENCE);
            } else {
                // Frame n was pushed at tick n and comes out at tick n + 3: it waited behind
                // 30 ms of audio, and the buffer holds 40 ms each time a frame arrives.
                assert_eq!(popped, Popped::Audio, "tick {tick}");
                assert_eq!(frame_number(&out), tick - 3);
                assert_eq!(buffer.held_ms(), JITTER_TARGET_MS - FRAME_MS);
            }
        }
        assert_eq!(buffer.stats(), JitterStats::default());
    }

    #[test]
    fn a_burst_beyond_the_cap_drops_the_oldest_audio_and_reports_how_much() {
        let mut buffer = JitterBuffer::new();
        // 300 ms arrives at once.
        for n in 0..30 {
            let expected = if n < 20 {
                Pushed::Kept
            } else {
                Pushed::DroppedOldest
            };
            assert_eq!(buffer.push(&tagged(n)), expected, "frame {n}");
            assert!(buffer.held_ms() <= JITTER_CAP_MS);
        }
        assert_eq!(buffer.held_ms(), JITTER_CAP_MS);
        assert_eq!(buffer.stats().dropped_frames, 10);
        assert_eq!(buffer.stats().dropped_ms(), 100);

        // The oldest 100 ms is gone; what is left comes out in order.
        let mut out = scratch();
        for n in 10..30 {
            assert_eq!(buffer.pop(&mut out), Popped::Audio);
            assert_eq!(frame_number(&out), n);
        }
        assert_eq!(buffer.held_ms(), 0);
        assert_eq!(buffer.stats().underruns, 0);
    }

    #[test]
    fn an_underrun_yields_silence_and_is_counted_once() {
        let mut buffer = JitterBuffer::new();
        let mut out = scratch();
        for n in 0..4 {
            buffer.push(&tagged(n));
        }
        for n in 0..4 {
            assert_eq!(buffer.pop(&mut out), Popped::Audio);
            assert_eq!(frame_number(&out), n);
        }

        // Dry while playing: one underrun.
        out = scratch();
        assert_eq!(buffer.pop(&mut out), Popped::Underrun);
        assert_eq!(out, SILENCE);
        assert_eq!(buffer.stats().underruns, 1);

        // Staying dry is not a new underrun each tick.
        for _ in 0..100 {
            out = scratch();
            assert_eq!(buffer.pop(&mut out), Popped::Waiting);
            assert_eq!(out, SILENCE);
        }
        assert_eq!(buffer.stats().underruns, 1);

        // It waits for the target again before playing, and loses nothing meanwhile.
        for n in 4..7 {
            buffer.push(&tagged(n));
            assert_eq!(buffer.pop(&mut out), Popped::Waiting);
            assert_eq!(out, SILENCE);
        }
        buffer.push(&tagged(7));
        for n in 4..8 {
            assert_eq!(buffer.pop(&mut out), Popped::Audio);
            assert_eq!(frame_number(&out), n);
        }
        assert_eq!(buffer.pop(&mut out), Popped::Underrun);
        assert_eq!(buffer.stats().underruns, 2);
        assert_eq!(buffer.stats().dropped_frames, 0);
    }

    #[test]
    fn limits_that_are_not_whole_frames_or_are_out_of_order_are_refused() {
        struct Case {
            target_ms: u32,
            cap_ms: u32,
            what: &'static str,
        }
        let cases = [
            Case {
                target_ms: 0,
                cap_ms: 200,
                what: "target",
            },
            Case {
                target_ms: 45,
                cap_ms: 200,
                what: "target",
            },
            Case {
                target_ms: 40,
                cap_ms: 30,
                what: "cap",
            },
            Case {
                target_ms: 40,
                cap_ms: 205,
                what: "cap",
            },
            Case {
                target_ms: 40,
                cap_ms: 4_000_000_000,
                what: "cap",
            },
        ];
        for case in cases {
            let err = JitterBuffer::with_limits(case.target_ms, case.cap_ms).unwrap_err();
            assert!(
                err.to_string().contains(case.what),
                "{} / {}: {err}",
                case.target_ms,
                case.cap_ms
            );
        }
        assert!(JitterBuffer::with_limits(10, 10).is_ok());
        assert!(JitterBuffer::with_limits(60, 10_000).is_ok());
    }

    #[test]
    fn printing_a_buffer_shows_no_audio() {
        let mut buffer = JitterBuffer::new();
        buffer.push(&[0.125; FRAME_LEN]);
        let printed = format!("{buffer:?}");
        assert!(!printed.contains("0.125"), "{printed}");
    }

    /// Random arrival patterns: bursts, gaps, more than one pop per tick. Whatever the
    /// pattern, what comes out is whole frames in strictly increasing order, so nothing is
    /// repeated or reordered, and every frame that went in is accounted for.
    #[test]
    fn random_arrivals_never_reorder_or_duplicate() {
        for seed in 0..300 {
            let mut rng = Lcg::new(seed);
            let target_frames = 1 + rng.below(8);
            let cap_frames = target_frames + rng.below(24);
            let mut buffer =
                JitterBuffer::with_limits(target_frames * FRAME_MS, cap_frames * FRAME_MS).unwrap();

            let mut pushed = 0_u64;
            let mut played = 0_u64;
            let mut dropped = 0_u64;
            let mut underruns = 0_u64;
            // Frames missing from the output so far: gaps between consecutive frames played.
            let mut skipped = 0_u64;
            let mut next_wanted = 0_u64;
            let mut out = scratch();

            for step in 0..400_u32 {
                let arrivals = match rng.below(10) {
                    0 | 1 => 0,
                    2 => 2 + rng.below(40),
                    3 => 2,
                    _ => 1,
                };
                for _ in 0..arrivals {
                    let before = buffer.stats().dropped_frames;
                    let outcome = buffer.push(&tagged(pushed));
                    pushed += 1;
                    let now = buffer.stats().dropped_frames;
                    match outcome {
                        Pushed::Kept => assert_eq!(now, before, "seed {seed} step {step}"),
                        Pushed::DroppedOldest => {
                            assert_eq!(now, before + 1, "seed {seed} step {step}");
                            dropped += 1;
                        }
                    }
                    assert!(
                        buffer.held_ms() <= cap_frames * FRAME_MS,
                        "seed {seed} step {step}"
                    );
                }

                let pops = match rng.below(10) {
                    0 => 0,
                    1 => 2 + rng.below(6),
                    _ => 1,
                };
                for _ in 0..pops {
                    out = scratch();
                    match buffer.pop(&mut out) {
                        Popped::Audio => {
                            let n = frame_number(&out);
                            assert!(
                                n >= next_wanted,
                                "seed {seed} step {step}: frame {n} after {next_wanted}"
                            );
                            skipped += n - next_wanted;
                            next_wanted = n + 1;
                            played += 1;
                        }
                        Popped::Underrun => {
                            assert_eq!(out, SILENCE, "seed {seed} step {step}");
                            underruns += 1;
                        }
                        Popped::Waiting => assert_eq!(out, SILENCE, "seed {seed} step {step}"),
                    }
                }

                let held = u64::from(buffer.held_ms() / FRAME_MS);
                assert_eq!(
                    played + dropped + held,
                    pushed,
                    "seed {seed} step {step}: a frame is unaccounted for"
                );
                // The only frames ever missing from the output are the ones counted as
                // dropped; those not yet passed over are older than everything still held.
                assert!(skipped <= dropped, "seed {seed} step {step}");
            }

            // Drain it: what is left comes out in order, and then every gap is a counted drop.
            for _ in 0..=(cap_frames + target_frames) {
                buffer.push(&tagged(pushed));
                pushed += 1;
            }
            dropped = buffer.stats().dropped_frames;
            loop {
                match buffer.pop(&mut out) {
                    Popped::Audio => {
                        let n = frame_number(&out);
                        assert!(n >= next_wanted, "seed {seed}: frame {n} out of order");
                        skipped += n - next_wanted;
                        next_wanted = n + 1;
                        played += 1;
                    }
                    Popped::Underrun => {
                        underruns += 1;
                        break;
                    }
                    Popped::Waiting => panic!("seed {seed}: a full buffer must be playing"),
                }
            }
            assert_eq!(
                next_wanted, pushed,
                "seed {seed}: the last frame never came out"
            );
            assert_eq!(skipped, dropped, "seed {seed}");
            assert_eq!(played + dropped, pushed, "seed {seed}");
            assert_eq!(buffer.stats().underruns, underruns, "seed {seed}");
        }
    }
}
