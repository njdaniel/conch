//! The stereo mixer: the frames of a screen share's sources for one tick, added and clipped
//! (`docs/design/linux-share-audio.md` §5.1, §15 D-5).
//!
//! The share's sound is 48 kHz stereo; the speakers' mix ([`crate::Mixer`]) is mono and
//! stays as it is. This is the same rule at twice the width: a frame of 10 ms holds
//! [`FRAME_LEN`] samples of each channel, interleaved left then right.

use std::fmt;

use crate::FRAME_LEN;

/// Samples in a stereo frame: [`FRAME_LEN`] of each channel, interleaved (`L R L R ...`).
pub const STEREO_FRAME_LEN: usize = 2 * FRAME_LEN;

/// One stereo frame: 10 ms of 48 kHz audio, two channels interleaved, left first. Each
/// sample is nominally in [-1, 1].
pub type StereoFrame = [f32; STEREO_FRAME_LEN];

/// A stereo frame of silence.
pub const STEREO_SILENCE: StereoFrame = [0.0; STEREO_FRAME_LEN];

/// Adds the sources' stereo frames for one 10 ms tick.
///
/// Use: [`add`](Self::add) each source's frame for the tick, then [`finish`](Self::finish)
/// to take the mix.
///
/// Guarantees, those of [`crate::Mixer`] but for deafening, which a share's sound has
/// nothing to do with:
///
/// - The mix is the sample-by-sample sum of the frames added since the last `finish`, each
///   channel with itself (the left of one source never reaches the right of the mix), so one
///   source alone comes out unchanged.
/// - Every sample of the mix is in [-1, 1]: a sum beyond that is clipped to the nearest
///   bound, never wrapped, and a sum that is not a number comes out as zero.
/// - `finish` starts the next tick from nothing, so a source that stops being added
///   contributes nothing from then on, and a tick with no source is silence.
///
/// The mixer is a fixed-size value with no heap storage. No method allocates, locks or logs,
/// so all of them may be called next to a real-time callback.
pub struct StereoMixer {
    sum: StereoFrame,
    sources: usize,
}

impl StereoMixer {
    /// A mixer with nothing added.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            sum: STEREO_SILENCE,
            sources: 0,
        }
    }

    /// Adds one source's frame to this tick.
    pub fn add(&mut self, frame: &StereoFrame) {
        for (sum, sample) in self.sum.iter_mut().zip(frame) {
            *sum += *sample;
        }
        self.sources = self.sources.saturating_add(1);
    }

    /// How many frames have been added to this tick so far.
    #[must_use]
    pub fn sources(&self) -> usize {
        self.sources
    }

    /// Writes this tick's mix into `out`, starts the next tick, and returns how many frames
    /// were added to this one. With none, `out` is silence.
    pub fn finish(&mut self, out: &mut StereoFrame) -> usize {
        for (out, sum) in out.iter_mut().zip(&self.sum) {
            *out = if sum.is_nan() {
                0.0
            } else {
                sum.clamp(-1.0, 1.0)
            };
        }
        self.sum = STEREO_SILENCE;
        std::mem::take(&mut self.sources)
    }
}

impl Default for StereoMixer {
    fn default() -> Self {
        Self::new()
    }
}

/// Prints no audio.
impl fmt::Debug for StereoMixer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StereoMixer")
            .field("sources", &self.sources)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Mixer, SILENCE};

    /// A frame different at every sample, within [-`peak`, `peak`], with the right channel
    /// a scaled negation of the left so that a swap of channels shows.
    fn ramp(peak: f32) -> StereoFrame {
        let mut frame = STEREO_SILENCE;
        for (i, pair) in frame.chunks_exact_mut(2).enumerate() {
            let left = peak * (2.0 * i as f32 / (FRAME_LEN - 1) as f32 - 1.0);
            pair[0] = left;
            pair[1] = -left * 0.5;
        }
        frame
    }

    fn scratch() -> StereoFrame {
        [f32::NAN; STEREO_FRAME_LEN]
    }

    #[test]
    fn a_stereo_frame_is_ten_milliseconds_of_two_interleaved_channels() {
        assert_eq!(STEREO_FRAME_LEN, 960);
        assert_eq!(STEREO_FRAME_LEN, 2 * FRAME_LEN);
        assert_eq!(std::mem::size_of::<StereoFrame>(), 960 * 4);
        // No heap: the mixer is its frame and a count.
        assert!(std::mem::size_of::<StereoMixer>() >= std::mem::size_of::<StereoFrame>());
        assert!(
            std::mem::size_of::<StereoMixer>()
                <= std::mem::size_of::<StereoFrame>() + 2 * std::mem::size_of::<usize>()
        );
    }

    #[test]
    fn one_source_passes_through_unchanged() {
        let mut mixer = StereoMixer::new();
        let source = ramp(1.0);
        let mut out = scratch();
        mixer.add(&source);
        assert_eq!(mixer.sources(), 1);
        assert_eq!(mixer.finish(&mut out), 1);
        assert_eq!(out, source);
    }

    #[test]
    fn n_sources_are_summed_sample_by_sample() {
        for n in 1..=8_u8 {
            let mut mixer = StereoMixer::new();
            let one = ramp(0.1);
            for _ in 0..n {
                mixer.add(&one);
            }
            let mut out = scratch();
            assert_eq!(mixer.finish(&mut out), usize::from(n));
            for (i, (mixed, one)) in out.iter().zip(&one).enumerate() {
                let want = (0..n).fold(0.0_f32, |sum, _| sum + one).clamp(-1.0, 1.0);
                assert_eq!(*mixed, want, "{n} sources, sample {i}");
            }
        }
    }

    #[test]
    fn left_stays_left_and_right_stays_right() {
        let mut left_only = STEREO_SILENCE;
        let mut right_only = STEREO_SILENCE;
        for pair in left_only.chunks_exact_mut(2) {
            pair[0] = 0.5;
        }
        for pair in right_only.chunks_exact_mut(2) {
            pair[1] = -0.25;
        }
        let mut mixer = StereoMixer::new();
        mixer.add(&left_only);
        mixer.add(&right_only);
        let mut out = scratch();
        assert_eq!(mixer.finish(&mut out), 2);
        for pair in out.chunks_exact(2) {
            assert_eq!(pair, [0.5, -0.25]);
        }

        // A source on the left alone leaves the right silent.
        mixer.add(&left_only);
        mixer.finish(&mut out);
        for pair in out.chunks_exact(2) {
            assert_eq!(pair, [0.5, 0.0]);
        }
    }

    /// The clip rule is the mono mixer's, sample for sample, on either channel.
    #[test]
    fn a_sum_out_of_range_is_clipped_and_not_a_number_is_zero_as_in_the_mono_mixer() {
        let pairs = [
            (0.75, 0.75),
            (-0.75, -0.75),
            (1.0, 1.0),
            (f32::MAX, f32::MAX),
            (f32::NEG_INFINITY, 0.5),
            (0.5, 0.5),
            (0.5, 0.25),
            (f32::NAN, 0.5),
            (f32::INFINITY, f32::NEG_INFINITY),
        ];
        for (a, b) in pairs {
            let mut mono = Mixer::new();
            mono.add(&[a; FRAME_LEN]);
            mono.add(&[b; FRAME_LEN]);
            let mut want = SILENCE;
            mono.finish(&mut want);

            let mut stereo = StereoMixer::new();
            stereo.add(&[a; STEREO_FRAME_LEN]);
            stereo.add(&[b; STEREO_FRAME_LEN]);
            let mut out = scratch();
            stereo.finish(&mut out);
            for (i, sample) in out.iter().enumerate() {
                assert!((-1.0..=1.0).contains(sample), "{a} + {b}: sample {i}");
                assert_eq!(*sample, want[i / 2], "{a} + {b}: sample {i}");
            }
        }
    }

    #[test]
    fn every_sample_of_a_loud_mix_stays_in_range_and_keeps_its_sign() {
        let mut mixer = StereoMixer::new();
        let loud = ramp(1.0);
        for _ in 0..5 {
            mixer.add(&loud);
        }
        let mut out = scratch();
        assert_eq!(mixer.finish(&mut out), 5);
        for (i, (mixed, one)) in out.iter().zip(&loud).enumerate() {
            assert!((-1.0..=1.0).contains(mixed), "sample {i}: {mixed}");
            assert!(mixed * one >= 0.0, "sample {i}: {mixed} from {one}");
            assert_eq!(*mixed, (5.0 * one).clamp(-1.0, 1.0), "sample {i}");
        }
    }

    /// No source is silence, and says so: the share's `no_sources`.
    #[test]
    fn with_no_source_the_tick_is_silence_and_counts_none() {
        let mut mixer = StereoMixer::new();
        let mut out = scratch();
        assert_eq!(mixer.finish(&mut out), 0);
        assert_eq!(out, STEREO_SILENCE);

        // A source that stops contributes nothing on the next tick.
        mixer.add(&ramp(0.5));
        assert_eq!(mixer.finish(&mut out), 1);
        out = scratch();
        assert_eq!(mixer.finish(&mut out), 0);
        assert_eq!(out, STEREO_SILENCE);
    }
}
