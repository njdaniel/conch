//! Measures the part of a microphone signal that matches a known speaker sweep (#2081).
//!
//! Input: 48 kHz mono frames of the sweep, the raw capture, and optionally the same capture
//! after echo cancellation. Output: a raw echo level and an optional echo return loss
//! enhancement (ERLE), or no measurement when the sweep was not detected. Owns no audio
//! devices and keeps no state or samples between measurements.

use std::f64::consts::TAU;

use conch_voice_audio::{FRAME_LEN, FRAME_MS, Frame, SAMPLE_RATE};

use crate::echo::FLOOR_RMS;

const MAX_DELAY_SAMPLES: usize = 500 / FRAME_MS as usize * FRAME_LEN;
// The coarse search uses 8 kHz windows, leaving a transition band above the sweep.
const COARSE_SAMPLE_STEP: usize = 6;
const LOW_PASS_CUTOFF_HZ: f64 = 3200.0;
const LOW_PASS_RADIUS: usize = 128;
const REFINE_RADIUS: usize = 4;
// Require normalized correlation across the whole sweep. Even a perfectly matching
// 20 ms excerpt of a one-second sweep has correlation of only about 0.14.
const MIN_CORRELATION: f64 = 0.20;

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct EchoMeasurement {
    /// Level of the coherent echo at the microphone, in dB relative to digital full scale.
    pub raw_echo_dbfs: f64,
    /// Echo reduction, present when the capture was processed.
    pub reduction: Option<EchoReduction>,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct EchoReduction {
    pub db: f64,
    /// The residual was below the measurement floor. `db` uses that floor as its residual.
    pub lower_bound: bool,
}

/// Estimate raw echo level and ERLE from finite samples of a broadband sweep below 3 kHz.
/// Pass the part of the sweep to score after any canceller warm-up. The captures start at
/// the same point on the playback timeline and extend at least as far as `played`. Extra
/// capture frames allow for delays up to 500 ms, including any delay introduced by
/// processing. When supplied, `processed` covers the same capture interval as `raw`.
///
/// The strongest delayed match estimates the power of the coherent echo. Detection uses
/// normalized correlation over the whole sweep in its frequency band. The two captures
/// are aligned independently so a delay introduced by the canceller does not count as
/// attenuation. This estimates the strongest path, so diffuse reflections and distortion
/// can escape measurement.
#[must_use]
pub fn measure(
    played: &[Frame],
    raw: &[Frame],
    processed: Option<&[Frame]>,
) -> Option<EchoMeasurement> {
    if played.is_empty()
        || raw.len() < played.len()
        || processed.is_some_and(|frames| frames.len() != raw.len())
    {
        return None;
    }
    let reference = played.as_flattened();
    let reference_power = power(reference);
    if !reference_power.is_finite() || reference_power <= f64::from(FLOOR_RMS).powi(2) {
        return None;
    }
    let taps = low_pass_taps();
    let coarse_reference = low_pass(reference, &taps, COARSE_SAMPLE_STEP);
    let find_echo = |frames: &[Frame]| {
        strongest_match(
            reference,
            reference_power,
            &coarse_reference,
            &taps,
            frames.as_flattened(),
        )
    };
    let before = find_echo(raw)?;
    if before.echo_power <= before.floor {
        return None;
    }
    let reduction = if let Some(frames) = processed {
        let after = find_echo(frames)?;
        Some(EchoReduction {
            db: 10.0 * (before.echo_power / after.echo_power.max(after.floor)).log10(),
            lower_bound: after.echo_power <= after.floor,
        })
    } else {
        None
    };
    Some(EchoMeasurement {
        raw_echo_dbfs: 10.0 * before.echo_power.log10(),
        reduction,
    })
}

struct Match {
    echo_power: f64,
    floor: f64,
}

fn strongest_match(
    reference: &[f32],
    reference_power: f64,
    coarse_reference: &[f64],
    taps: &[f64],
    capture: &[f32],
) -> Option<Match> {
    if capture.iter().any(|sample| !sample.is_finite()) {
        return None;
    }
    let last_delay = MAX_DELAY_SAMPLES.min(capture.len() - reference.len());
    let window = |delay| &capture[delay..delay + reference.len()];

    // Filter the capture before taking any of the decimated windows. Keep every
    // integer delay eligible because the old delay grid could favor a sidelobe.
    let filter_end = capture
        .len()
        .min(last_delay + reference.len() + LOW_PASS_RADIUS);
    let filtered = low_pass(&capture[..filter_end], taps, 1);
    let coarse_window = |delay| {
        filtered[delay..delay + reference.len()]
            .iter()
            .copied()
            .step_by(COARSE_SAMPLE_STEP)
    };
    let coarse_dot = |delay| {
        coarse_reference
            .iter()
            .zip(coarse_window(delay))
            .map(|(&played, captured)| played * captured)
            .sum::<f64>()
    };
    let mut coarse_delay = 0;
    let mut coarse_peak = 0.0;
    for delay in 0..=last_delay {
        let peak = coarse_dot(delay).abs();
        if peak > coarse_peak {
            coarse_delay = delay;
            coarse_peak = peak;
        }
    }

    let mut best_delay = coarse_delay;
    let mut peak = 0.0;
    let first = coarse_delay.saturating_sub(REFINE_RADIUS);
    let last = last_delay.min(coarse_delay + REFINE_RADIUS);
    for delay in first..=last {
        let score = dot(reference, window(delay)).abs();
        if score > peak {
            best_delay = delay;
            peak = score;
        }
    }

    // Test the squared, normalized correlation in the sweep band. A short tone
    // can line up with a small part of the chirp and still fail this test.
    let capture_energy: f64 = coarse_window(best_delay)
        .map(|sample| sample * sample)
        .sum();
    let reference_energy: f64 = coarse_reference.iter().map(|sample| sample * sample).sum();
    let min_match_energy = MIN_CORRELATION.powi(2) * capture_energy;
    let coherent = coarse_dot(best_delay).powi(2) > min_match_energy * reference_energy;

    // Report the full-rate projection and use in-band capture power for the floor.
    let mean_product = peak / reference.len() as f64;
    Some(Match {
        echo_power: if coherent {
            mean_product.powi(2) / reference_power
        } else {
            0.0
        },
        floor: (min_match_energy / coarse_reference.len() as f64).max(f64::from(FLOOR_RMS).powi(2)),
    })
}

fn low_pass_taps() -> [f64; 2 * LOW_PASS_RADIUS + 1] {
    let cutoff = TAU * LOW_PASS_CUTOFF_HZ / f64::from(SAMPLE_RATE);
    // Symmetric Blackman-windowed sinc, normalized for unit gain at DC.
    let mut taps = std::array::from_fn(|index| {
        let offset = index as f64 - LOW_PASS_RADIUS as f64;
        let phase = TAU * offset / (2 * LOW_PASS_RADIUS) as f64;
        let window = 0.42 + 0.5 * phase.cos() + 0.08 * (2.0 * phase).cos();
        let sinc = if index == LOW_PASS_RADIUS {
            cutoff
        } else {
            (cutoff * offset).sin() / offset
        };
        window * sinc
    });
    let sum: f64 = taps.iter().sum();
    for tap in &mut taps {
        *tap /= sum;
    }
    taps
}

fn low_pass(samples: &[f32], taps: &[f64], step: usize) -> Vec<f64> {
    // Zero padding keeps the reference and capture on their original timelines.
    let mut padded = vec![0.0; samples.len() + taps.len() - 1];
    let start = taps.len() / 2;
    padded[start..start + samples.len()].copy_from_slice(samples);
    padded
        .windows(taps.len())
        .step_by(step)
        .map(|window| {
            window
                .iter()
                .zip(taps)
                .map(|(&sample, &tap)| f64::from(sample) * tap)
                .sum()
        })
        .collect()
}

fn dot(a: &[f32], b: &[f32]) -> f64 {
    a.iter()
        .zip(b)
        .map(|(&a, &b)| f64::from(a) * f64::from(b))
        .sum()
}

fn power(samples: &[f32]) -> f64 {
    dot(samples, samples) / samples.len() as f64
}

#[cfg(test)]
mod tests {
    use super::*;
    use conch_voice_audio::SILENCE;
    use std::f32::consts::TAU;

    fn sweep() -> Vec<Frame> {
        sweep_between(400.0, 2400.0)
    }

    fn sweep_between(start_hz: f32, end_hz: f32) -> Vec<Frame> {
        let mut frames = vec![SILENCE; 1000 / FRAME_MS as usize];
        for (i, sample) in frames.as_flattened_mut().iter_mut().enumerate() {
            let seconds = i as f32 / SAMPLE_RATE as f32;
            let phase = TAU * seconds * (start_hz + (end_hz - start_hz) * 0.5 * seconds);
            *sample = 0.08 * phase.sin();
        }
        frames
    }

    fn delayed(played: &[Frame], delay: usize, gain: f32) -> Vec<Frame> {
        let mut frames = vec![SILENCE; played.len() + MAX_DELAY_SAMPLES / FRAME_LEN];
        for (&sample, out) in played
            .as_flattened()
            .iter()
            .zip(&mut frames.as_flattened_mut()[delay..])
        {
            *out = gain * sample;
        }
        frames
    }

    #[test]
    fn measures_delayed_attenuated_copies_before_and_after_processing() {
        let played = sweep();
        // Neither arrival is on the coarse grid. Processing adds a delay and reduces
        // amplitude tenfold. The acoustic path can also invert the signal's polarity.
        let raw = delayed(&played, 1733, -0.25);
        let processed = delayed(&played, 3219, -0.025);
        let result = measure(&played, &raw, Some(&processed)).unwrap();
        let expected_raw = 10.0 * (power(played.as_flattened()) * 0.25_f64.powi(2)).log10();
        assert!((result.raw_echo_dbfs - expected_raw).abs() < 0.01);
        let reduction = result.reduction.unwrap();
        assert!((reduction.db - 20.0).abs() < 0.01);
        assert!(!reduction.lower_bound);

        let off = measure(&played, &raw, None).unwrap();
        assert_eq!(off.raw_echo_dbfs, result.raw_echo_dbfs);
        assert_eq!(off.reduction, None);
    }

    #[test]
    fn out_of_band_sweep_does_not_hide_unchanged_echo() {
        let played = sweep();
        let raw = delayed(&played, 1732, 0.25);
        // Exercise the review input and a sweep that would alias at the new 8 kHz rate.
        for (start_hz, end_hz) in [(5600.0, 3600.0), (7600.0, 5600.0)] {
            let interferer = delayed(&sweep_between(start_hz, end_hz), 12000, 1.0);
            let mut processed = raw.clone();
            for (sample, &interference) in processed
                .as_flattened_mut()
                .iter_mut()
                .zip(interferer.as_flattened())
            {
                *sample += interference;
            }

            let reduction = measure(&played, &raw, Some(&processed))
                .unwrap()
                .reduction
                .unwrap();
            assert!(reduction.db.abs() < 0.01, "{start_hz} Hz: {reduction:?}");
            assert!(!reduction.lower_bound);
        }
    }

    #[test]
    fn a_two_sample_delay_change_does_not_reduce_echo() {
        // Near the top of the sweep band, the old delay grid can prefer a sidelobe.
        let played = sweep_between(1600.0, 2900.0);
        let raw = delayed(&played, 1732, 0.25);
        let processed = delayed(&played, 1734, 0.25);
        let reduction = measure(&played, &raw, Some(&processed))
            .unwrap()
            .reduction
            .unwrap();
        assert!(reduction.db.abs() < 0.01, "{reduction:?}");
        assert!(!reduction.lower_bound);
    }

    #[test]
    fn a_short_tone_crossing_the_sweep_does_not_count_as_echo() {
        let played = sweep();
        let mut capture = vec![SILENCE; played.len()];
        let tone_len = (SAMPLE_RATE * 20 / 1000) as usize;
        let start = (capture.as_flattened().len() - tone_len) / 2;
        for (i, sample) in capture.as_flattened_mut()[start..start + tone_len]
            .iter_mut()
            .enumerate()
        {
            *sample = 0.08 * (TAU * 1400.0 * i as f32 / SAMPLE_RATE as f32).sin();
        }

        assert_eq!(measure(&played, &capture, None), None);
    }

    #[test]
    fn silence_and_unrelated_capture_give_no_measurement() {
        let played = sweep();
        let silent = vec![SILENCE; played.len()];
        assert_eq!(measure(&played, &silent, Some(&silent)), None);
        assert_eq!(measure(&silent, &played, None), None);
        let unrelated = vec![[0.03; FRAME_LEN]; played.len()];
        assert_eq!(measure(&played, &unrelated, None), None);
    }

    #[test]
    fn a_silent_residual_reports_a_finite_lower_bound() {
        let played = sweep();
        let raw = delayed(&played, MAX_DELAY_SAMPLES, 0.5);
        let silent = vec![SILENCE; raw.len()];
        let result = measure(&played, &raw, Some(&silent)).unwrap();
        let reduction = result.reduction.unwrap();
        assert!(reduction.lower_bound);
        let floor_dbfs = 20.0 * f64::from(FLOOR_RMS).log10();
        assert!((reduction.db - (result.raw_echo_dbfs - floor_dbfs)).abs() < 0.01);
    }

    #[test]
    fn invalid_capture_gives_no_measurement() {
        let played = sweep();
        assert_eq!(measure(&played, &played, Some(&played[1..])), None);
        let mut invalid = played.clone();
        invalid[0][0] = f32::INFINITY;
        assert_eq!(measure(&played, &played, Some(&invalid)), None);
        assert_eq!(measure(&invalid, &played, None), None);
    }
}
