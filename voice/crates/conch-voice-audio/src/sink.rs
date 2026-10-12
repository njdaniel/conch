//! Where audio to be heard goes: the speaker trait, and a sink for tests that counts and
//! measures what it is given and keeps none of it.

use std::collections::BTreeMap;

use crate::{AudioError, FRAME_LEN, Frame, SAMPLE_RATE};

/// The speakers: something that takes the mixed audio one frame at a time.
pub trait SpeakerSink {
    /// Takes the next 10 ms to be heard. Never blocks.
    fn play(&mut self, frame: &Frame);
}

/// Below this RMS a signal counts as silence (-80 dB of full scale).
const SILENCE_RMS: f32 = 1e-4;
/// A candidate is dominant only if it carries at least this share of the signal's energy.
const DOMINANT_SHARE: f64 = 0.25;
/// What one frame can resolve: 100 Hz.
const RESOLUTION_HZ: f32 = SAMPLE_RATE as f32 / FRAME_LEN as f32;

#[derive(Debug, Clone)]
struct Candidate {
    hz: f32,
    /// The Goertzel coefficient for `hz`.
    coeff: f64,
    /// Power found at `hz`, summed over every frame.
    power: f64,
}

/// Measures a stream of frames and keeps none of them: how many there were, how loud they
/// were, and which of a given set of frequencies was strongest.
///
/// It holds a few numbers per candidate frequency and nothing else, so nothing it is given
/// can be played back or written anywhere.
///
/// Frequencies are told apart frame by frame, so candidates must be at least 100 Hz apart;
/// multiples of 100 Hz are measured exactly, and a tone within 50 Hz of a candidate may be
/// taken for it.
#[derive(Debug, Clone)]
pub struct SignalMeter {
    candidates: Box<[Candidate]>,
    frames: u64,
    /// The sum of every sample squared.
    energy: f64,
}

impl SignalMeter {
    /// A meter that looks for these frequencies. The set may be empty.
    ///
    /// # Errors
    ///
    /// [`AudioError::InvalidSetting`] unless every candidate is from 100 Hz to 23900 Hz and
    /// no two are closer than 100 Hz.
    pub fn new(candidates_hz: &[f32]) -> Result<Self, AudioError> {
        let highest = SAMPLE_RATE as f32 / 2.0 - RESOLUTION_HZ;
        for (i, &hz) in candidates_hz.iter().enumerate() {
            if !(hz >= RESOLUTION_HZ && hz <= highest) {
                return Err(AudioError::InvalidSetting {
                    what: "a candidate frequency",
                    must_be: "from 100 Hz to 23900 Hz",
                    got: f64::from(hz),
                });
            }
            let too_close = candidates_hz
                .iter()
                .take(i)
                .any(|&other| (other - hz).abs() < RESOLUTION_HZ);
            if too_close {
                return Err(AudioError::InvalidSetting {
                    what: "a candidate frequency",
                    must_be: "at least 100 Hz from every other candidate",
                    got: f64::from(hz),
                });
            }
        }
        let candidates = candidates_hz
            .iter()
            .map(|&hz| Candidate {
                hz,
                coeff: 2.0 * (std::f64::consts::TAU * f64::from(hz) / f64::from(SAMPLE_RATE)).cos(),
                power: 0.0,
            })
            .collect();
        Ok(Self {
            candidates,
            frames: 0,
            energy: 0.0,
        })
    }

    /// Measures one frame. A sample that is not a finite number counts as zero.
    pub fn push(&mut self, frame: &Frame) {
        let clean = |sample: &f32| {
            if sample.is_finite() {
                f64::from(*sample)
            } else {
                0.0
            }
        };
        self.frames = self.frames.saturating_add(1);
        self.energy += frame.iter().map(clean).map(|x| x * x).sum::<f64>();
        for candidate in &mut self.candidates {
            // Goertzel: the power of this one frequency in this one frame.
            let (mut s1, mut s2) = (0.0_f64, 0.0_f64);
            for x in frame.iter().map(clean) {
                let s0 = x + candidate.coeff * s1 - s2;
                s2 = s1;
                s1 = s0;
            }
            candidate.power += s1 * s1 + s2 * s2 - candidate.coeff * s1 * s2;
        }
    }

    /// How many frames were measured, silent ones included.
    #[must_use]
    pub fn frames(&self) -> u64 {
        self.frames
    }

    /// The RMS level of everything measured; 0 if nothing was.
    #[must_use]
    pub fn rms(&self) -> f32 {
        if self.frames == 0 {
            return 0.0;
        }
        let samples = self.frames as f64 * FRAME_LEN as f64;
        (self.energy / samples).sqrt() as f32
    }

    /// The candidate frequency that was strongest, if one stood out: `None` for silence (an
    /// RMS below -80 dB of full scale), and `None` unless the strongest candidate carries at
    /// least a quarter of the signal's energy, so noise and tones that are not candidates
    /// are not mistaken for one.
    #[must_use]
    pub fn dominant(&self) -> Option<f32> {
        if self.rms() < SILENCE_RMS {
            return None;
        }
        let strongest = self
            .candidates
            .iter()
            .max_by(|a, b| a.power.total_cmp(&b.power))?;
        // A pure tone of amplitude A has power (A·N/2)² in a frame of N samples, and energy
        // A²·N/2: so this is 1 when the candidate is the whole signal.
        let share = 2.0 * strongest.power / (FRAME_LEN as f64 * self.energy);
        (share >= DOMINANT_SHARE).then_some(strongest.hz)
    }
}

/// Speakers for tests: plays nothing, stores no audio, and measures what it is given.
///
/// It measures the mix it is asked to [`play`](SpeakerSink::play), and, for each speaker
/// whose frames it is shown with [`heard`](Self::heard), that speaker alone: frames received,
/// RMS, and the strongest of the candidate frequencies. A test in which each speaker's
/// microphone is a different tone can therefore tell whose audio arrived.
#[derive(Debug, Clone)]
pub struct CountingSink {
    /// A meter that has measured nothing, copied for each new speaker.
    blank: SignalMeter,
    mix: SignalMeter,
    speakers: BTreeMap<String, SignalMeter>,
}

impl CountingSink {
    /// A sink that looks for these frequencies.
    ///
    /// # Errors
    ///
    /// As [`SignalMeter::new`].
    pub fn new(candidates_hz: &[f32]) -> Result<Self, AudioError> {
        let blank = SignalMeter::new(candidates_hz)?;
        Ok(Self {
            mix: blank.clone(),
            blank,
            speakers: BTreeMap::new(),
        })
    }

    /// Measures one frame received from one speaker, before it is mixed.
    pub fn heard(&mut self, speaker: &str, frame: &Frame) {
        if let Some(meter) = self.speakers.get_mut(speaker) {
            meter.push(frame);
        } else {
            let mut meter = self.blank.clone();
            meter.push(frame);
            self.speakers.insert(speaker.to_owned(), meter);
        }
    }

    /// What was measured of the mix.
    #[must_use]
    pub fn mix(&self) -> &SignalMeter {
        &self.mix
    }

    /// What was measured of one speaker; `None` if no frame of theirs was ever shown.
    #[must_use]
    pub fn speaker(&self, speaker: &str) -> Option<&SignalMeter> {
        self.speakers.get(speaker)
    }

    /// Every speaker a frame was shown for, in name order.
    pub fn speakers(&self) -> impl Iterator<Item = (&str, &SignalMeter)> {
        self.speakers
            .iter()
            .map(|(name, meter)| (name.as_str(), meter))
    }
}

impl SpeakerSink for CountingSink {
    fn play(&mut self, frame: &Frame) {
        self.mix.push(frame);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::Lcg;
    use crate::{MicSource, Mixer, SILENCE, ToneSource};

    const CANDIDATES: [f32; 3] = [400.0, 700.0, 1000.0];

    /// One second of two speakers, each a tone, received, mixed and played into a sink.
    fn two_speakers(alice: (f32, f32), bob: (f32, f32)) -> CountingSink {
        let mut sink = CountingSink::new(&CANDIDATES).unwrap();
        let mut alice = ToneSource::new(alice.0, alice.1).unwrap();
        let mut bob = ToneSource::new(bob.0, bob.1).unwrap();
        let mut mixer = Mixer::new();
        let (mut frame, mut mixed) = (SILENCE, SILENCE);
        for _ in 0..100 {
            alice.read(&mut frame);
            sink.heard("alice", &frame);
            mixer.add(&frame);
            bob.read(&mut frame);
            sink.heard("bob", &frame);
            mixer.add(&frame);
            mixer.finish(&mut mixed);
            sink.play(&mixed);
        }
        sink
    }

    #[test]
    fn the_dominant_tone_of_two_mixed_speakers_is_the_louder_one() {
        struct Case {
            alice: (f32, f32),
            bob: (f32, f32),
            dominant: f32,
        }
        let cases = [
            Case {
                alice: (400.0, 0.6),
                bob: (1000.0, 0.3),
                dominant: 400.0,
            },
            Case {
                alice: (400.0, 0.3),
                bob: (1000.0, 0.6),
                dominant: 1000.0,
            },
            Case {
                alice: (700.0, 0.2),
                bob: (400.0, 0.1),
                dominant: 700.0,
            },
            Case {
                alice: (1000.0, 0.05),
                bob: (700.0, 0.08),
                dominant: 700.0,
            },
            // Loud enough together to clip in the mixer.
            Case {
                alice: (1000.0, 0.9),
                bob: (400.0, 0.5),
                dominant: 1000.0,
            },
        ];
        for case in cases {
            let sink = two_speakers(case.alice, case.bob);
            let name = format!("{:?} with {:?}", case.alice, case.bob);
            assert_eq!(sink.mix().dominant(), Some(case.dominant), "{name}");
            assert_eq!(sink.mix().frames(), 100, "{name}");

            // And each speaker alone is known by their own tone, whatever their level.
            let alice = sink.speaker("alice").unwrap();
            assert_eq!(alice.dominant(), Some(case.alice.0), "{name}");
            assert_eq!(alice.frames(), 100, "{name}");
            let bob = sink.speaker("bob").unwrap();
            assert_eq!(bob.dominant(), Some(case.bob.0), "{name}");
            assert_eq!(bob.frames(), 100, "{name}");
        }
    }

    #[test]
    fn silence_has_no_dominant_tone_but_its_frames_are_counted() {
        let mut sink = CountingSink::new(&CANDIDATES).unwrap();
        for _ in 0..100 {
            sink.heard("alice", &SILENCE);
            sink.play(&SILENCE);
        }
        for meter in [sink.mix(), sink.speaker("alice").unwrap()] {
            assert_eq!(meter.frames(), 100);
            assert_eq!(meter.rms(), 0.0);
            assert_eq!(meter.dominant(), None);
        }
    }

    #[test]
    fn a_sink_given_nothing_reports_nothing() {
        let sink = CountingSink::new(&CANDIDATES).unwrap();
        assert_eq!(sink.mix().frames(), 0);
        assert_eq!(sink.mix().rms(), 0.0);
        assert_eq!(sink.mix().dominant(), None);
        assert!(sink.speaker("alice").is_none());
        assert_eq!(sink.speakers().count(), 0);
    }

    #[test]
    fn a_signal_too_faint_to_be_speech_counts_as_silence() {
        let mut meter = SignalMeter::new(&CANDIDATES).unwrap();
        let mut faint = ToneSource::new(400.0, 0.00005).unwrap();
        let mut frame = SILENCE;
        for _ in 0..100 {
            faint.read(&mut frame);
            meter.push(&frame);
        }
        assert!(meter.rms() > 0.0);
        assert_eq!(meter.dominant(), None);
    }

    #[test]
    fn a_tone_that_is_not_a_candidate_and_noise_are_not_taken_for_one() {
        let mut meter = SignalMeter::new(&CANDIDATES).unwrap();
        let mut stranger = ToneSource::new(550.0, 0.5).unwrap();
        let mut frame = SILENCE;
        for _ in 0..100 {
            stranger.read(&mut frame);
            meter.push(&frame);
        }
        assert!(meter.rms() > 0.3);
        assert_eq!(meter.dominant(), None);

        let mut meter = SignalMeter::new(&CANDIDATES).unwrap();
        let mut rng = Lcg::new(7);
        for _ in 0..100 {
            for sample in &mut frame {
                *sample = rng.next_u32() as f32 / u32::MAX as f32 - 0.5;
            }
            meter.push(&frame);
        }
        assert!(meter.rms() > 0.2);
        assert_eq!(meter.dominant(), None);
    }

    #[test]
    fn rms_is_the_level_of_what_was_measured() {
        let mut meter = SignalMeter::new(&[]).unwrap();
        let mut tone = ToneSource::new(400.0, 0.5).unwrap();
        let mut frame = SILENCE;
        for _ in 0..50 {
            tone.read(&mut frame);
            meter.push(&frame);
        }
        // A sine of peak 0.5 has an RMS of 0.5 / √2.
        assert!((meter.rms() - 0.353_553).abs() < 1e-4, "{}", meter.rms());
        // With no candidates there is never a dominant one.
        assert_eq!(meter.dominant(), None);

        // The same again in silence halves the mean square.
        for _ in 0..50 {
            meter.push(&SILENCE);
        }
        assert_eq!(meter.frames(), 100);
        assert!((meter.rms() - 0.25).abs() < 1e-4, "{}", meter.rms());
    }

    #[test]
    fn samples_that_are_not_numbers_do_not_spoil_the_measurement() {
        let mut meter = SignalMeter::new(&CANDIDATES).unwrap();
        let mut tone = ToneSource::new(700.0, 0.5).unwrap();
        let mut frame = SILENCE;
        for n in 0..100 {
            tone.read(&mut frame);
            if n == 10 {
                frame[3] = f32::NAN;
                frame[4] = f32::INFINITY;
            }
            meter.push(&frame);
        }
        assert!(meter.rms().is_finite());
        assert_eq!(meter.dominant(), Some(700.0));
    }

    #[test]
    fn speakers_are_measured_apart_and_listed_by_name() {
        let mut sink = CountingSink::new(&CANDIDATES).unwrap();
        let mut tone = ToneSource::new(1000.0, 0.5).unwrap();
        let mut frame = SILENCE;
        for _ in 0..30 {
            tone.read(&mut frame);
            sink.heard("carol", &frame);
        }
        for _ in 0..20 {
            sink.heard("bob", &SILENCE);
        }
        let seen: Vec<_> = sink
            .speakers()
            .map(|(name, meter)| (name, meter.frames(), meter.dominant()))
            .collect();
        assert_eq!(seen, [("bob", 20, None), ("carol", 30, Some(1000.0))]);
        // Nothing was played, so the mix has measured nothing.
        assert_eq!(sink.mix().frames(), 0);
    }

    #[test]
    fn candidates_that_cannot_be_told_apart_are_refused() {
        let cases: [&[f32]; 6] = [
            &[400.0, 450.0],
            &[400.0, 700.0, 400.0],
            &[50.0],
            &[0.0],
            &[24_000.0],
            &[f32::NAN],
        ];
        for candidates in cases {
            let err = CountingSink::new(candidates).unwrap_err();
            assert!(
                err.to_string().contains("candidate frequency"),
                "{candidates:?}: {err}"
            );
        }
        assert!(SignalMeter::new(&[100.0, 200.0, 23_900.0]).is_ok());
    }
}
