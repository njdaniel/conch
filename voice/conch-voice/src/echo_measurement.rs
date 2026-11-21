//! Measures the part of a microphone signal that matches a known speaker sweep (#2081).
//!
//! Input: 48 kHz mono frames of the sweep, the raw capture, and optionally the same capture
//! after echo cancellation. Output: a raw echo level and an optional echo return loss
//! enhancement (ERLE), or no measurement when the sweep was not detected. Owns no audio
//! devices and keeps no state or samples between measurements.

use conch_voice_audio::{FRAME_LEN, FRAME_MS, Frame};

use crate::echo::FLOOR_RMS;

const MAX_DELAY_SAMPLES: usize = 500 / FRAME_MS as usize * FRAME_LEN;
// The coarse search samples the sweep at 6 kHz and tests every fourth possible delay.
// The finer delay spacing keeps the nearest candidate within two samples of the echo.
const COARSE_SAMPLE_STEP: usize = 8;
const COARSE_DELAY_STEP: usize = 4;
// Require at least 1% of microphone power to match the sweep. This keeps an unrelated
// sound in the room from counting as echo and raises the reporting floor in a noisy room.
const MIN_MATCH_SHARE: f64 = 0.01;

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
/// The strongest delayed match estimates the power of the coherent echo. The two captures
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
    let before = strongest_match(reference, reference_power, raw.as_flattened())?;
    if before.echo_power <= before.floor {
        return None;
    }
    let reduction = if let Some(frames) = processed {
        let after = strongest_match(reference, reference_power, frames.as_flattened())?;
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

fn strongest_match(reference: &[f32], reference_power: f64, capture: &[f32]) -> Option<Match> {
    if capture.iter().any(|sample| !sample.is_finite()) {
        return None;
    }
    let last_delay = MAX_DELAY_SAMPLES.min(capture.len() - reference.len());
    let window = |delay| &capture[delay..delay + reference.len()];

    // Every candidate uses the whole reference and the same number of captured samples.
    // Use the coarse grid to avoid scanning a multi-second sweep at every sample offset.
    let mut coarse_delay = 0;
    let mut coarse_peak = 0.0;
    for delay in (0..=last_delay).step_by(COARSE_DELAY_STEP) {
        let peak = dot(reference, window(delay), COARSE_SAMPLE_STEP).abs();
        if peak > coarse_peak {
            coarse_delay = delay;
            coarse_peak = peak;
        }
    }

    let mut best_delay = coarse_delay;
    let mut peak = 0.0;
    let first = coarse_delay.saturating_sub(COARSE_DELAY_STEP - 1);
    let last = last_delay.min(coarse_delay + COARSE_DELAY_STEP - 1);
    for delay in first..=last {
        let score = dot(reference, window(delay), 1).abs();
        if score > peak {
            best_delay = delay;
            peak = score;
        }
    }

    // Squared projection onto the reference, expressed as mean square per sample.
    let mean_product = peak / reference.len() as f64;
    Some(Match {
        echo_power: mean_product.powi(2) / reference_power,
        floor: (MIN_MATCH_SHARE * power(window(best_delay))).max(f64::from(FLOOR_RMS).powi(2)),
    })
}

fn dot(a: &[f32], b: &[f32], step: usize) -> f64 {
    a.iter()
        .zip(b)
        .step_by(step)
        .map(|(&a, &b)| f64::from(a) * f64::from(b))
        .sum()
}

fn power(samples: &[f32]) -> f64 {
    dot(samples, samples, 1) / samples.len() as f64
}

#[cfg(test)]
mod tests {
    use super::*;
    use conch_voice_audio::{SAMPLE_RATE, SILENCE};
    use std::f32::consts::TAU;

    fn sweep() -> Vec<Frame> {
        let mut frames = vec![SILENCE; 1000 / FRAME_MS as usize];
        for (i, sample) in frames.as_flattened_mut().iter_mut().enumerate() {
            let seconds = i as f32 / SAMPLE_RATE as f32;
            // A 400 Hz to 2.4 kHz linear sweep over one second.
            *sample = 0.08 * (TAU * seconds * (400.0 + 1000.0 * seconds)).sin();
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
