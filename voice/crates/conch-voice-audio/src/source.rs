//! Where captured audio comes from: the microphone trait, and a tone for tests.

use crate::{AudioError, Frame, SAMPLE_RATE};

/// What a [`MicSource`] had for one read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MicRead {
    /// The frame was filled with the next 10 ms of captured audio.
    Ready,
    /// There is not a whole frame yet. The frame was left as it was; ask again later.
    NotYet,
    /// There is no microphone at present. The frame was left as it was.
    NoMicrophone,
}

/// A microphone: something that yields captured audio one frame at a time.
///
/// What it yields is captured audio and nothing more. It is not for sending until it has
/// been through [`TransmitGate::pass`](crate::TransmitGate::pass).
pub trait MicSource {
    /// Fills `frame` with the next 10 ms of captured audio, if there is a whole frame.
    /// Never blocks.
    fn read(&mut self, frame: &mut Frame) -> MicRead;
}

/// A microphone for tests: a steady sine tone, always [`MicRead::Ready`].
///
/// The tone is continuous from one frame to the next, at the frequency and peak level asked
/// for, so a listener can recognise whose "microphone" it hears by its frequency.
#[derive(Debug, Clone)]
pub struct ToneSource {
    /// Cycles per sample.
    step: f64,
    /// Where in the cycle the next sample is, in [0, 1).
    phase: f64,
    amplitude: f64,
}

impl ToneSource {
    /// A tone of `hz` at a peak level of `amplitude`.
    ///
    /// # Errors
    ///
    /// [`AudioError::InvalidSetting`] unless `hz` is above zero and below half the sample
    /// rate, and `amplitude` is above zero and at most 1.
    pub fn new(hz: f32, amplitude: f32) -> Result<Self, AudioError> {
        let nyquist = SAMPLE_RATE as f32 / 2.0;
        if !(hz > 0.0 && hz < nyquist) {
            return Err(AudioError::InvalidSetting {
                what: "the tone's frequency",
                must_be: "above 0 Hz and below 24000 Hz",
                got: f64::from(hz),
            });
        }
        if !(amplitude > 0.0 && amplitude <= 1.0) {
            return Err(AudioError::InvalidSetting {
                what: "the tone's amplitude",
                must_be: "above 0 and at most 1",
                got: f64::from(amplitude),
            });
        }
        Ok(Self {
            step: f64::from(hz) / f64::from(SAMPLE_RATE),
            phase: 0.0,
            amplitude: f64::from(amplitude),
        })
    }
}

impl MicSource for ToneSource {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        for sample in frame.iter_mut() {
            *sample = (self.amplitude * (std::f64::consts::TAU * self.phase).sin()) as f32;
            self.phase += self.step;
            if self.phase >= 1.0 {
                self.phase -= 1.0;
            }
        }
        MicRead::Ready
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::SILENCE;

    #[test]
    fn a_tone_has_the_frequency_and_level_asked_for_and_no_seam_between_frames() {
        let mut tone = ToneSource::new(440.0, 0.5).unwrap();
        let mut frame = SILENCE;
        let mut n = 0_u32;
        // Ten seconds, compared sample by sample with the sine it should be.
        for _ in 0..1000 {
            assert_eq!(tone.read(&mut frame), MicRead::Ready);
            for sample in frame {
                let t = f64::from(n) / f64::from(SAMPLE_RATE);
                let want = 0.5 * (std::f64::consts::TAU * 440.0 * t).sin();
                assert!(
                    (f64::from(sample) - want).abs() < 1e-6,
                    "sample {n}: {sample} is not {want}"
                );
                n += 1;
            }
        }
    }

    #[test]
    fn a_tone_is_never_all_zeros() {
        let mut tone = ToneSource::new(1000.0, 0.25).unwrap();
        let mut frame = SILENCE;
        for _ in 0..100 {
            tone.read(&mut frame);
            let peak = frame.iter().fold(0.0_f32, |peak, s| peak.max(s.abs()));
            assert!((peak - 0.25).abs() < 1e-3, "peak {peak}");
        }
    }

    #[test]
    fn a_tone_that_cannot_be_played_is_refused() {
        struct Case {
            hz: f32,
            amplitude: f32,
            what: &'static str,
        }
        let cases = [
            Case {
                hz: 0.0,
                amplitude: 0.5,
                what: "frequency",
            },
            Case {
                hz: -440.0,
                amplitude: 0.5,
                what: "frequency",
            },
            Case {
                hz: 24_000.0,
                amplitude: 0.5,
                what: "frequency",
            },
            Case {
                hz: f32::NAN,
                amplitude: 0.5,
                what: "frequency",
            },
            Case {
                hz: f32::INFINITY,
                amplitude: 0.5,
                what: "frequency",
            },
            Case {
                hz: 440.0,
                amplitude: 0.0,
                what: "amplitude",
            },
            Case {
                hz: 440.0,
                amplitude: 1.5,
                what: "amplitude",
            },
            Case {
                hz: 440.0,
                amplitude: f32::NAN,
                what: "amplitude",
            },
        ];
        for case in cases {
            let err = ToneSource::new(case.hz, case.amplitude).unwrap_err();
            assert!(
                err.to_string().contains(case.what),
                "{} Hz at {}: {err}",
                case.hz,
                case.amplitude
            );
        }
    }
}
