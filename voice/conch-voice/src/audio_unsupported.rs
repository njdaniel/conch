//! The audio backend of a platform that has none yet: every way of opening a device fails
//! with a typed error, and nothing is reachable that could produce a sound.
//!
//! Input: nothing. Output: [`AudioError::NoBackend`] from [`PipeWire::connect`], which is
//! where every caller starts, so the devices below it can never be built. Owns: the type
//! names the callers use, kept the same as on Linux so that they need no `cfg`.

use std::time::Duration;

use conch_voice_audio::{AudioError, Frame, MicRead, MicSource, SpeakerSink};

use crate::error::Error;
use crate::receive::Speakers;

/// How long a listing would wait. Unused here; it keeps the Linux module's name.
pub const LISTING_WAIT: Duration = Duration::from_secs(3);

/// What the platform is called in the error, as `std::env::consts::OS` has it.
fn no_backend() -> Error {
    Error::Audio(AudioError::NoBackend {
        platform: std::env::consts::OS,
    })
}

/// The connection to the platform's audio system. There is none: it is never built.
#[derive(Clone)]
pub struct PipeWire {
    _never: (),
}

impl PipeWire {
    /// Always fails: this platform has no audio backend yet.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn connect() -> Result<Self, Error> {
        Err(no_backend())
    }

    /// Always fails, as [`PipeWire::connect`] does.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn microphone(&self, _configured: Option<&str>) -> Result<NoDevice, Error> {
        Err(no_backend())
    }

    /// Always fails, as [`PipeWire::connect`] does.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn speakers(&self, _configured: Option<&str>) -> Result<NoDevice, Error> {
        Err(no_backend())
    }
}

/// A device that cannot exist: no value of this type can be made, so its methods have
/// nothing to do.
#[derive(Debug, Clone, Copy)]
pub enum NoDevice {}

impl MicSource for NoDevice {
    fn read(&mut self, _frame: &mut Frame) -> MicRead {
        match *self {}
    }
}

impl SpeakerSink for NoDevice {
    fn play(&mut self, _frame: &Frame) {
        match *self {}
    }
}

impl Speakers for NoDevice {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn connecting_says_the_platform_has_no_audio_backend() {
        let error = PipeWire::connect().err().map(|error| error.to_string());
        let error = error.unwrap_or_default();
        assert!(error.contains("no audio backend"), "{error}");
        assert!(error.contains(std::env::consts::OS), "{error}");
    }

    #[test]
    fn the_error_is_the_audio_one_so_the_helper_reports_audio_unavailable() {
        assert!(matches!(
            PipeWire::connect().err(),
            Some(Error::Audio(AudioError::NoBackend { .. }))
        ));
    }
}
