//! The open microphone's level detector (`docs/design/tui-voice.md` §20.6): it says when
//! the microphone's sound starts and when quiet starts, and nothing else.
//!
//! It is given each captured frame before the gate, and it hands no frame onward: what it
//! decides goes to the push-to-talk machine, which alone commands the gate. The only path
//! from the microphone to the track is still [`crate::TransmitGate::pass`].

use std::time::Duration;

use crate::{AudioError, FRAME_LEN, FRAME_MS, Frame};

/// The detector's two settings: `[audio] open_threshold_db` and `open_hang_ms`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LevelConfig {
    /// The level, in dBFS, at or above which one frame is sound: 20·log10 of the frame's RMS
    /// over its samples in [-1, 1], so that a full-scale square wave is 0 dBFS and a
    /// full-scale sine about -3.
    pub threshold_db: i32,
    /// How long with no frame at or above the level before quiet starts, in milliseconds.
    /// Quiet frames count 10 ms each, and time with no frame at all counts as it passes
    /// ([`LevelDetector::no_frame_for`]).
    pub hang_ms: u32,
}

/// What a detector said changed.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum LevelChange {
    /// An onset: one frame at or above the level, after quiet.
    SoundStarted,
    /// The hang has passed with no frame at or above the level.
    QuietStarted,
}

/// The lowest threshold accepted, in dBFS. Below it the threshold would be lost in the
/// rounding of single-precision samples, and digital silence must never be sound.
const MIN_THRESHOLD_DB: i32 = -120;

/// Decides, frame by frame, when the microphone's sound starts and when quiet starts.
///
/// Two states, sounding and quiet. It becomes sounding when one frame's RMS level is at or
/// above the threshold, and quiet once the hang has passed with no such frame. **It starts
/// sounding**, and [`start_sounding`](Self::start_sounding) puts it back there: so the first
/// onset after it is made, or after that call, always follows a full hang of quiet observed
/// from that moment, however loud the microphone was. That is what keeps a command, a
/// microphone switch or a reason ending from ever being what opens the gate.
///
/// It is pure: it reads no clock, keeps no audio (a few numbers, nothing it was given), and
/// cannot fail once made. Time comes in from the caller: the time with no frame at all
/// ([`no_frame_for`](Self::no_frame_for)), so that a source that stops delivering counts as
/// quiet, and the time since the last frame at or above the level
/// ([`silent_for`](Self::silent_for)), so that one that trickles quiet frames more slowly
/// than it should cannot hold the hang open either.
#[derive(Debug, Clone)]
pub struct LevelDetector {
    /// The threshold as a mean square: 10^(dB/10), compared with the frame's mean square so
    /// that no root or logarithm is taken per frame.
    threshold_power: f64,
    hang: Duration,
    sounding: bool,
    /// Whether the last frame given was at or above the level.
    last_loud: bool,
    /// While sounding: quiet counted in frames since the last frame at or above the level
    /// (or since it was made sounding).
    quiet_frames: Duration,
    /// While sounding: the longest time reported with no frame since the last frame, or
    /// since it was made sounding.
    no_frame: Duration,
}

impl LevelDetector {
    /// A detector in its sounding state.
    ///
    /// # Errors
    ///
    /// [`AudioError::InvalidSetting`] if the threshold is above 0 dBFS (nothing could reach
    /// it) or below -120, or if the hang is shorter than one frame.
    pub fn new(config: LevelConfig) -> Result<Self, AudioError> {
        if !(MIN_THRESHOLD_DB..=0).contains(&config.threshold_db) {
            return Err(AudioError::InvalidSetting {
                what: "threshold_db",
                must_be: "from -120 to 0 dBFS",
                got: f64::from(config.threshold_db),
            });
        }
        if config.hang_ms < FRAME_MS {
            return Err(AudioError::InvalidSetting {
                what: "hang_ms",
                must_be: "at least one frame (10 ms)",
                got: f64::from(config.hang_ms),
            });
        }
        Ok(Self {
            threshold_power: 10_f64.powf(f64::from(config.threshold_db) / 10.0),
            hang: Duration::from_millis(u64::from(config.hang_ms)),
            sounding: true,
            last_loud: false,
            quiet_frames: Duration::ZERO,
            no_frame: Duration::ZERO,
        })
    }

    /// Whether it is in its sounding state.
    #[must_use]
    pub fn is_sounding(&self) -> bool {
        self.sounding
    }

    /// Whether the last frame given to [`frame`](Self::frame) was at or above the level: for
    /// the caller to know when the last sound came, by its own clock.
    #[must_use]
    pub fn last_frame_loud(&self) -> bool {
        self.last_loud
    }

    /// Takes one captured frame. Returns [`LevelChange::SoundStarted`] if it is at or above
    /// the level after quiet, [`LevelChange::QuietStarted`] if it completes the hang.
    ///
    /// A frame is 10 ms of quiet, whenever it arrives: a source that catches up after a
    /// stall is counted by what it captured. A sample that is not a finite number counts as
    /// silence.
    pub fn frame(&mut self, frame: &Frame) -> Option<LevelChange> {
        let loud = mean_square(frame) >= self.threshold_power;
        self.last_loud = loud;
        // Frames have come, so the stretch with none is over.
        self.no_frame = Duration::ZERO;
        match (self.sounding, loud) {
            (false, true) => {
                self.start_sounding();
                Some(LevelChange::SoundStarted)
            }
            (false, false) => None,
            (true, true) => {
                self.quiet_frames = Duration::ZERO;
                None
            }
            (true, false) => {
                self.quiet_frames = self
                    .quiet_frames
                    .saturating_add(Duration::from_millis(u64::from(FRAME_MS)));
                self.quiet_if_hung()
            }
        }
    }

    /// No frame has come for `since_last_frame`, measured from the last frame given to
    /// [`frame`](Self::frame), or from the last [`start_sounding`](Self::start_sounding)
    /// (or the detector's making) if that was later. Returns
    /// [`LevelChange::QuietStarted`] if that, with the quiet frames before it, completes
    /// the hang. Saying it again, with the same time or less, changes nothing.
    ///
    /// The next frame ends the stretch: frames are counted on their own, so a source that
    /// delivers in bursts is not counted twice. A source that is late by some milliseconds
    /// can therefore make quiet come that much early; one that stops makes it come within
    /// the hang.
    pub fn no_frame_for(&mut self, since_last_frame: Duration) -> Option<LevelChange> {
        if !self.sounding {
            return None;
        }
        self.no_frame = self.no_frame.max(since_last_frame);
        self.quiet_if_hung()
    }

    /// It has been `since_sound` since the last frame at or above the level, or since the
    /// detector was last put in its sounding state (or made) if that was later, by the
    /// caller's clock. Returns [`LevelChange::QuietStarted`] once that reaches the hang,
    /// however many quiet frames came meanwhile. Saying it again changes nothing.
    ///
    /// Counting frames alone would let a source that delivers one quiet frame a little more
    /// often than once a hang hold the hang open for ever, each frame ending the stretch
    /// with none before it was long enough: this bounds quiet by the time itself. For a
    /// source that delivers on time it changes nothing, since its frames reach the hang in
    /// as long.
    pub fn silent_for(&mut self, since_sound: Duration) -> Option<LevelChange> {
        if !self.sounding || since_sound < self.hang {
            return None;
        }
        self.sounding = false;
        self.quiet_frames = Duration::ZERO;
        self.no_frame = Duration::ZERO;
        Some(LevelChange::QuietStarted)
    }

    /// Puts the detector in its sounding state, whatever it was in, with no quiet counted:
    /// on entering open mode, after a microphone switch, and whenever a reason that kept the
    /// gate shut ends. It reports nothing, since this is no onset; quiet, a full hang of it
    /// from now, is what it reports next.
    pub fn start_sounding(&mut self) {
        self.sounding = true;
        self.quiet_frames = Duration::ZERO;
        self.no_frame = Duration::ZERO;
    }

    fn quiet_if_hung(&mut self) -> Option<LevelChange> {
        if self.quiet_frames.saturating_add(self.no_frame) < self.hang {
            return None;
        }
        self.sounding = false;
        self.quiet_frames = Duration::ZERO;
        self.no_frame = Duration::ZERO;
        Some(LevelChange::QuietStarted)
    }
}

/// The mean of the frame's squared samples: its RMS level, squared. A sample that is not a
/// finite number counts as zero, so that nothing broken is ever taken for sound.
fn mean_square(frame: &Frame) -> f64 {
    let sum: f64 = frame
        .iter()
        .map(|&sample| {
            if sample.is_finite() {
                f64::from(sample) * f64::from(sample)
            } else {
                0.0
            }
        })
        .sum();
    sum / FRAME_LEN as f64
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::SILENCE;
    use crate::testutil::Lcg;

    use LevelChange::{QuietStarted, SoundStarted};

    const TEN_MS: Duration = Duration::from_millis(10);

    fn detector(threshold_db: i32, hang_ms: u32) -> LevelDetector {
        LevelDetector::new(LevelConfig {
            threshold_db,
            hang_ms,
        })
        .unwrap()
    }

    /// Every sample `value`: a frame whose RMS is `value`'s size.
    fn flat(value: f32) -> Frame {
        [value; FRAME_LEN]
    }

    /// A sine of `amplitude` at 1 kHz, ten whole cycles in the frame.
    fn sine(amplitude: f32) -> Frame {
        let mut frame = SILENCE;
        for (n, sample) in frame.iter_mut().enumerate() {
            let phase = std::f64::consts::TAU * 1000.0 * n as f64 / 48_000.0;
            *sample = (f64::from(amplitude) * phase.sin()) as f32;
        }
        frame
    }

    /// Alternating signs at `amplitude`: a square wave, whose RMS is its amplitude.
    fn square(amplitude: f32) -> Frame {
        let mut frame = SILENCE;
        for (n, sample) in frame.iter_mut().enumerate() {
            *sample = if n % 2 == 0 { amplitude } else { -amplitude };
        }
        frame
    }

    /// A detector brought to quiet, as it is once its first hang has passed.
    fn quiet(threshold_db: i32, hang_ms: u32) -> LevelDetector {
        let mut detector = detector(threshold_db, hang_ms);
        assert_eq!(
            detector.no_frame_for(Duration::from_millis(u64::from(hang_ms))),
            Some(QuietStarted)
        );
        assert!(!detector.is_sounding());
        detector
    }

    #[test]
    fn a_frame_is_sound_at_or_above_the_level_and_quiet_below_it() {
        // (what, threshold, frame, sound)
        let cases: [(&str, i32, Frame, bool); 16] = [
            ("1% above -40 dBFS", -40, flat(0.0101), true),
            ("1% below -40 dBFS", -40, flat(0.0099), false),
            ("1% above, of either sign", -40, flat(-0.0101), true),
            ("a square wave 1% above", -40, square(0.0101), true),
            ("a square wave 1% below", -40, square(0.0099), false),
            ("0.1 dB above -20 dBFS", -20, flat(0.1012), true),
            ("0.1 dB below -20 dBFS", -20, flat(0.0988), false),
            ("full scale, exactly at 0 dBFS", 0, square(1.0), true),
            ("just under full scale, at 0 dBFS", 0, square(0.999), false),
            (
                "a full-scale sine, -3.01 dBFS, against -3",
                -3,
                sine(1.0),
                false,
            ),
            ("a full-scale sine against -4 dBFS", -4, sine(1.0), true),
            (
                "a sine 1% above -40 dBFS RMS",
                -40,
                sine(0.0101 * 2_f32.sqrt()),
                true,
            ),
            (
                "a sine 1% below -40 dBFS RMS",
                -40,
                sine(0.0099 * 2_f32.sqrt()),
                false,
            ),
            ("loud at the lowest level", -80, flat(0.000_101), true),
            ("quiet at the lowest level", -80, flat(0.000_099), false),
            (
                "digital silence at the lowest threshold",
                -120,
                SILENCE,
                false,
            ),
        ];
        for (what, threshold, frame, sound) in cases {
            let mut detector = quiet(threshold, 100);
            let want = sound.then_some(SoundStarted);
            assert_eq!(detector.frame(&frame), want, "{what}");
            assert_eq!(detector.is_sounding(), sound, "{what}");
        }
    }

    #[test]
    fn digital_silence_and_broken_samples_are_never_sound() {
        for threshold in [-120, -80, -40, -10, 0] {
            let mut detector = quiet(threshold, 100);
            let mut broken = flat(f32::NAN);
            broken[1] = f32::INFINITY;
            broken[2] = f32::NEG_INFINITY;
            for frame in [SILENCE, flat(-0.0), broken] {
                assert_eq!(detector.frame(&frame), None, "{threshold}");
            }
            assert!(!detector.is_sounding());
        }
    }

    /// One loud sample is enough if it brings the frame's level up: the level is the
    /// frame's RMS, not a peak and not a majority of samples.
    #[test]
    fn the_level_is_the_frames_rms() {
        let mut one_click = SILENCE;
        // RMS of one sample of 0.5 in 480: 0.5 / sqrt(480), about -32.8 dBFS.
        one_click[100] = 0.5;
        let mut detector = quiet(-33, 100);
        assert_eq!(detector.frame(&one_click), Some(SoundStarted));
        let mut detector = quiet(-32, 100);
        assert_eq!(detector.frame(&one_click), None);
    }

    #[test]
    fn quiet_comes_after_the_hang_counted_in_frames() {
        // (hang in ms, quiet frames until quiet starts): a hang that is not a whole number
        // of frames is rounded up, so the gate never shuts before the hang.
        for (hang_ms, frames) in [(10, 1), (100, 10), (400, 40), (405, 41), (2000, 200)] {
            let mut detector = quiet(-40, hang_ms);
            assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));
            for n in 1..frames {
                assert_eq!(detector.frame(&SILENCE), None, "{hang_ms} ms, frame {n}");
            }
            assert_eq!(detector.frame(&SILENCE), Some(QuietStarted), "{hang_ms} ms");
            // Quiet goes on: nothing more is said.
            assert_eq!(detector.frame(&SILENCE), None);
            assert_eq!(detector.no_frame_for(Duration::from_secs(60)), None);
        }
    }

    #[test]
    fn a_frame_at_the_level_during_the_hang_starts_it_again_and_is_no_onset() {
        let mut detector = quiet(-40, 100);
        assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));
        for _ in 0..9 {
            assert_eq!(detector.frame(&SILENCE), None);
        }
        // A pause shorter than the hang is the same utterance.
        assert_eq!(detector.frame(&flat(0.5)), None);
        for _ in 0..9 {
            assert_eq!(detector.frame(&SILENCE), None);
        }
        assert_eq!(detector.frame(&SILENCE), Some(QuietStarted));
        // And the next sound is an onset again.
        assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));
    }

    /// A source that stops delivering counts as quiet: the stretch with no frame completes
    /// the hang, alone or with the quiet frames before it.
    #[test]
    fn a_stretch_with_no_frames_counts_as_quiet() {
        let ms = Duration::from_millis;
        // (quiet frames after the last loud one, the stretch with no frame, quiet starts)
        let cases = [
            (0, ms(399), false),
            (0, ms(400), true),
            (0, ms(10_000), true),
            (20, ms(199), false),
            (20, ms(200), true),
            (39, ms(10), true),
            (39, ms(9), false),
        ];
        for (frames, stretch, quiet_starts) in cases {
            let mut detector = quiet(-40, 400);
            detector.frame(&flat(0.5));
            for _ in 0..frames {
                assert_eq!(detector.frame(&SILENCE), None);
            }
            let want = quiet_starts.then_some(QuietStarted);
            assert_eq!(
                detector.no_frame_for(stretch),
                want,
                "{frames} frames, {stretch:?}"
            );
            assert_eq!(detector.is_sounding(), !quiet_starts);
        }
    }

    #[test]
    fn the_stretch_with_no_frame_is_measured_from_the_last_frame_and_never_counted_twice() {
        let mut detector = quiet(-40, 400);
        detector.frame(&flat(0.5));
        // Looked at often while the source is late: each look gives the whole stretch, and
        // the stretch is not added up look by look.
        for look in 1..=39 {
            assert_eq!(detector.no_frame_for(TEN_MS * look), None, "look {look}");
            assert_eq!(detector.no_frame_for(TEN_MS * look), None, "again {look}");
        }
        // Saying less than before changes nothing either.
        assert_eq!(detector.no_frame_for(TEN_MS), None);
        // The late frames arrive, loud: the stretch is over and the hang begins again.
        assert_eq!(detector.frame(&flat(0.5)), None);
        assert_eq!(detector.no_frame_for(Duration::from_millis(390)), None);
        // A quiet frame ends that stretch too: what counts now is the frame's 10 ms.
        assert_eq!(detector.frame(&SILENCE), None);
        assert_eq!(detector.no_frame_for(Duration::from_millis(389)), None);
        assert_eq!(
            detector.no_frame_for(Duration::from_millis(390)),
            Some(QuietStarted)
        );
    }

    /// A source that trickles one quiet frame a little more often than once a hang: counted
    /// in frames alone it never reaches the hang, each frame ending the stretch with none.
    /// The time since the last sound, as the caller measures it, bounds it.
    #[test]
    fn a_trickle_of_quiet_frames_goes_quiet_once_the_hang_has_passed_since_the_last_sound() {
        let ms = Duration::from_millis;
        let mut detector = quiet(-40, 400);
        assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));
        assert!(detector.last_frame_loud());
        // One quiet frame every 390 ms: neither the frames nor the stretches reach 400.
        let mut since_sound = Duration::ZERO;
        for _ in 0..20 {
            assert_eq!(detector.no_frame_for(ms(389)), None);
            since_sound += ms(390);
            assert_eq!(detector.frame(&SILENCE), None);
            assert!(!detector.last_frame_loud());
            if since_sound < ms(400) {
                assert_eq!(detector.silent_for(since_sound), None);
            } else {
                assert_eq!(detector.silent_for(since_sound), Some(QuietStarted));
                break;
            }
        }
        assert!(
            !detector.is_sounding(),
            "quiet at the second frame, 780 ms on"
        );
        // Quiet goes on: nothing more is said, and the next sound is an onset.
        assert_eq!(detector.silent_for(ms(10_000)), None);
        assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));

        // Short of the hang since the last sound, nothing, whatever came between; and
        // `start_sounding` is a new start for it, as for the rest.
        assert_eq!(detector.silent_for(ms(399)), None);
        detector.start_sounding();
        assert_eq!(detector.silent_for(ms(399)), None);
        assert_eq!(detector.silent_for(ms(400)), Some(QuietStarted));
    }

    /// The detector is made sounding, and `start_sounding` puts it back there: however loud
    /// the microphone is from the first frame, no onset comes until a full hang of quiet,
    /// frames or no frames, has passed after that moment; quiet before it does not count.
    #[test]
    fn it_starts_sounding_and_needs_a_hang_of_quiet_before_any_onset() {
        // A detector just made, fed loud frames from its first.
        let mut detector = detector(-40, 400);
        assert!(detector.is_sounding());
        for _ in 0..1000 {
            assert_eq!(detector.frame(&flat(0.5)), None);
        }
        for _ in 0..39 {
            assert_eq!(detector.frame(&SILENCE), None);
        }
        assert_eq!(
            detector.frame(&flat(0.5)),
            None,
            "39 frames are not the hang"
        );
        for _ in 0..40 {
            detector.frame(&SILENCE);
        }
        assert!(!detector.is_sounding());
        assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted));

        // Put back in the sounding state while quiet, as after a mode change, a microphone
        // switch or a reason ending: the quiet that came before does not count.
        for start in ["quiet", "sounding", "in the middle of a hang"] {
            let mut detector = quiet(-40, 400);
            match start {
                "quiet" => {}
                "sounding" => {
                    detector.frame(&flat(0.5));
                }
                _ => {
                    detector.frame(&flat(0.5));
                    for _ in 0..39 {
                        detector.frame(&SILENCE);
                    }
                    assert_eq!(detector.no_frame_for(Duration::from_millis(9)), None);
                }
            }
            detector.start_sounding();
            assert!(detector.is_sounding(), "{start}");
            // Loud at once: no onset.
            assert_eq!(detector.frame(&flat(0.5)), None, "{start}");
            for _ in 0..39 {
                assert_eq!(detector.frame(&SILENCE), None, "{start}");
            }
            assert_eq!(detector.frame(&flat(0.5)), None, "{start}");
            // A full hang of no frames, measured from after it, and then an onset.
            assert_eq!(
                detector.no_frame_for(Duration::from_millis(399)),
                None,
                "{start}"
            );
            assert_eq!(
                detector.no_frame_for(Duration::from_millis(400)),
                Some(QuietStarted),
                "{start}"
            );
            assert_eq!(detector.frame(&flat(0.5)), Some(SoundStarted), "{start}");
        }
    }

    #[test]
    fn settings_outside_their_range_are_refused() {
        for (threshold_db, hang_ms, what) in [
            (1, 400, "threshold_db"),
            (i32::MAX, 400, "threshold_db"),
            (-121, 400, "threshold_db"),
            (i32::MIN, 400, "threshold_db"),
            (-40, 9, "hang_ms"),
            (-40, 0, "hang_ms"),
        ] {
            let error = LevelDetector::new(LevelConfig {
                threshold_db,
                hang_ms,
            })
            .unwrap_err();
            assert!(
                matches!(error, AudioError::InvalidSetting { what: got, .. } if got == what),
                "{threshold_db} {hang_ms}: {error}"
            );
        }
        for (threshold_db, hang_ms) in [(0, 10), (-120, 10), (-40, u32::MAX)] {
            assert!(
                LevelDetector::new(LevelConfig {
                    threshold_db,
                    hang_ms
                })
                .is_ok()
            );
        }
    }

    /// Whatever it is given, the detector's reports alternate, an onset only ever on a frame
    /// at or above the level, and quiet only once a full hang has passed with none.
    #[test]
    fn random_input_gives_alternating_reports_each_with_its_cause() {
        let loud = flat(0.5);
        for seed in 0..200 {
            let mut random = Lcg::new(seed);
            let hang_ms = 10 * (1 + random.below(60));
            let hang = Duration::from_millis(u64::from(hang_ms));
            let mut detector = detector(-40, hang_ms);
            let mut sounding = true;
            // The test's own count of quiet since the last loud frame or since it was made
            // sounding: frames, and the longest stretch with none since the last frame.
            let (mut frames, mut stretch) = (Duration::ZERO, Duration::ZERO);
            for _ in 0..2000 {
                let said = match random.below(10) {
                    0..=2 => {
                        let said = detector.frame(&loud);
                        if sounding {
                            assert_eq!(said, None, "seed {seed}");
                        } else {
                            assert_eq!(said, Some(SoundStarted), "seed {seed}");
                        }
                        (frames, stretch) = (Duration::ZERO, Duration::ZERO);
                        sounding = true;
                        said
                    }
                    3..=6 => {
                        stretch = Duration::ZERO;
                        frames += TEN_MS;
                        detector.frame(&SILENCE)
                    }
                    7..=8 => {
                        let gap = Duration::from_millis(u64::from(random.below(700)));
                        stretch = stretch.max(gap);
                        detector.no_frame_for(gap)
                    }
                    _ => {
                        detector.start_sounding();
                        (frames, stretch) = (Duration::ZERO, Duration::ZERO);
                        sounding = true;
                        None
                    }
                };
                if said == Some(QuietStarted) {
                    assert!(sounding, "seed {seed}: quiet twice");
                    assert!(frames + stretch >= hang, "seed {seed}: quiet too soon");
                    sounding = false;
                    (frames, stretch) = (Duration::ZERO, Duration::ZERO);
                } else if sounding {
                    assert!(frames + stretch < hang, "seed {seed}: quiet never came");
                }
                assert_eq!(detector.is_sounding(), sounding, "seed {seed}");
            }
        }
    }
}
