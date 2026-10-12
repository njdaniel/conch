//! The mixer: the speakers' frames for one tick, added and clipped.

use std::fmt;

use crate::{Frame, SILENCE};

/// Adds the speakers' frames for one 10 ms tick.
///
/// Use: [`add`](Self::add) each speaker's frame for the tick, then [`finish`](Self::finish)
/// to take the mix.
///
/// Guarantees:
///
/// - The mix is the sample-by-sample sum of the frames added since the last `finish`, so one
///   speaker alone comes out unchanged.
/// - Every sample of the mix is in [-1, 1]: a sum beyond that is clipped to the nearest
///   bound, never wrapped, and a sum that is not a number comes out as zero.
/// - `finish` starts the next tick from nothing, so a speaker who stops being added
///   contributes nothing from then on.
/// - While deafened the mix is all zeros, whatever was added.
///
/// The mixer is a fixed-size value with no heap storage. No method allocates, locks or logs,
/// so all of them may be called next to a real-time callback.
pub struct Mixer {
    sum: Frame,
    speakers: usize,
    deafened: bool,
}

impl Mixer {
    /// A mixer that is not deafened and has nothing added.
    #[must_use]
    pub const fn new() -> Self {
        Self {
            sum: SILENCE,
            speakers: 0,
            deafened: false,
        }
    }

    /// Deafens or undeafens. It takes effect at the next [`finish`](Self::finish).
    pub fn set_deafened(&mut self, deafened: bool) {
        self.deafened = deafened;
    }

    /// Whether the mixer is deafened.
    #[must_use]
    pub fn is_deafened(&self) -> bool {
        self.deafened
    }

    /// Adds one speaker's frame to this tick.
    pub fn add(&mut self, frame: &Frame) {
        for (sum, sample) in self.sum.iter_mut().zip(frame) {
            *sum += *sample;
        }
        self.speakers = self.speakers.saturating_add(1);
    }

    /// Writes this tick's mix into `out`, starts the next tick, and returns how many frames
    /// were added to this one.
    pub fn finish(&mut self, out: &mut Frame) -> usize {
        if self.deafened {
            *out = SILENCE;
        } else {
            for (out, sum) in out.iter_mut().zip(&self.sum) {
                *out = if sum.is_nan() {
                    0.0
                } else {
                    sum.clamp(-1.0, 1.0)
                };
            }
        }
        self.sum = SILENCE;
        std::mem::take(&mut self.speakers)
    }
}

impl Default for Mixer {
    fn default() -> Self {
        Self::new()
    }
}

/// Prints no audio.
impl fmt::Debug for Mixer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Mixer")
            .field("speakers", &self.speakers)
            .field("deafened", &self.deafened)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::FRAME_LEN;

    /// A frame that is different at every sample and stays within [-`peak`, `peak`].
    fn ramp(peak: f32) -> Frame {
        let mut frame = SILENCE;
        for (i, sample) in frame.iter_mut().enumerate() {
            *sample = peak * (2.0 * i as f32 / (FRAME_LEN - 1) as f32 - 1.0);
        }
        frame
    }

    fn scratch() -> Frame {
        [f32::NAN; FRAME_LEN]
    }

    #[test]
    fn one_speaker_passes_through_unchanged() {
        let mut mixer = Mixer::new();
        let speaker = ramp(1.0);
        let mut out = scratch();
        mixer.add(&speaker);
        assert_eq!(mixer.finish(&mut out), 1);
        assert_eq!(out, speaker);
    }

    #[test]
    fn two_speakers_are_summed() {
        let mut mixer = Mixer::new();
        let a = ramp(0.25);
        let b = [0.5; FRAME_LEN];
        let mut out = scratch();
        mixer.add(&a);
        mixer.add(&b);
        assert_eq!(mixer.finish(&mut out), 2);
        for (i, ((mixed, a), b)) in out.iter().zip(&a).zip(&b).enumerate() {
            assert_eq!(*mixed, a + b, "sample {i}");
        }
    }

    #[test]
    fn a_sum_out_of_range_is_clipped_not_wrapped() {
        struct Case {
            a: f32,
            b: f32,
            want: f32,
        }
        let cases = [
            Case {
                a: 0.75,
                b: 0.75,
                want: 1.0,
            },
            Case {
                a: -0.75,
                b: -0.75,
                want: -1.0,
            },
            Case {
                a: 1.0,
                b: 1.0,
                want: 1.0,
            },
            Case {
                a: f32::MAX,
                b: f32::MAX,
                want: 1.0,
            },
            Case {
                a: f32::NEG_INFINITY,
                b: 0.5,
                want: -1.0,
            },
            Case {
                a: 0.5,
                b: 0.5,
                want: 1.0,
            },
            Case {
                a: 0.5,
                b: 0.25,
                want: 0.75,
            },
            Case {
                a: f32::NAN,
                b: 0.5,
                want: 0.0,
            },
            Case {
                a: f32::INFINITY,
                b: f32::NEG_INFINITY,
                want: 0.0,
            },
        ];
        for case in cases {
            let mut mixer = Mixer::new();
            let mut out = scratch();
            mixer.add(&[case.a; FRAME_LEN]);
            mixer.add(&[case.b; FRAME_LEN]);
            mixer.finish(&mut out);
            assert_eq!(out, [case.want; FRAME_LEN], "{} + {}", case.a, case.b);
        }
    }

    #[test]
    fn every_sample_of_a_loud_mix_stays_in_range_and_keeps_its_sign() {
        let mut mixer = Mixer::new();
        let mut out = scratch();
        let loud = ramp(1.0);
        for _ in 0..5 {
            mixer.add(&loud);
        }
        assert_eq!(mixer.finish(&mut out), 5);
        for (i, (mixed, one)) in out.iter().zip(&loud).enumerate() {
            assert!((-1.0..=1.0).contains(mixed), "sample {i}: {mixed}");
            // Wrapping would flip the sign of the loudest samples.
            assert!(mixed * one >= 0.0, "sample {i}: {mixed} from {one}");
            assert_eq!(*mixed, (5.0 * one).clamp(-1.0, 1.0), "sample {i}");
        }
    }

    #[test]
    fn a_speaker_who_stops_contributes_nothing_on_the_next_tick() {
        let mut mixer = Mixer::new();
        let staying = ramp(0.25);
        let stopping = [0.5; FRAME_LEN];
        let mut out = scratch();

        mixer.add(&staying);
        mixer.add(&stopping);
        assert_eq!(mixer.finish(&mut out), 2);
        assert_ne!(out, staying);

        mixer.add(&staying);
        assert_eq!(mixer.finish(&mut out), 1);
        assert_eq!(out, staying);

        // And when nobody is left, the tick is silence.
        out = scratch();
        assert_eq!(mixer.finish(&mut out), 0);
        assert_eq!(out, SILENCE);
    }

    #[test]
    fn a_deafened_mixer_yields_all_zeros() {
        let mut mixer = Mixer::new();
        let mut out = scratch();
        mixer.set_deafened(true);
        assert!(mixer.is_deafened());
        for _ in 0..3 {
            mixer.add(&ramp(1.0));
            mixer.add(&[0.5; FRAME_LEN]);
            out = scratch();
            mixer.finish(&mut out);
            assert_eq!(out, SILENCE);
        }

        // Deafening after the frames were added still silences that tick.
        mixer.set_deafened(false);
        mixer.add(&[0.5; FRAME_LEN]);
        mixer.set_deafened(true);
        mixer.finish(&mut out);
        assert_eq!(out, SILENCE);

        // Undeafened, nothing from the deafened ticks is left over.
        mixer.set_deafened(false);
        mixer.add(&ramp(0.5));
        mixer.finish(&mut out);
        assert_eq!(out, ramp(0.5));
    }
}
