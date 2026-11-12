//! The audio backend of a platform that has none yet: every way of opening a device fails
//! with a typed error, and nothing is reachable that could produce a sound.
//!
//! Input: nothing. Output: [`AudioError::NoBackend`] from [`AudioSystem::connect`], which is
//! where every caller starts, so the devices below it can never be built. Owns: the type
//! names the callers use, kept the same as on Linux so that they need no `cfg`.

use std::time::Duration;

use conch_voice_audio::{AudioError, Frame, MicRead, MicSource, SpeakerSink};

use crate::devices::DeviceTable;
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
pub struct AudioSystem {
    _never: (),
}

impl AudioSystem {
    /// Always fails: this platform has no audio backend yet.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn connect() -> Result<Self, Error> {
        Err(no_backend())
    }

    /// Always fails, as [`AudioSystem::connect`] does: there is no list to give. The
    /// signature is the Linux one, so that `devices` needs no `cfg`.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn list_devices(&self, _wait: Duration) -> Result<DeviceTable, Error> {
        Err(no_backend())
    }

    /// Always fails, as [`AudioSystem::connect`] does.
    ///
    /// # Errors
    ///
    /// [`Error::Audio`] holding [`AudioError::NoBackend`].
    pub fn microphone(&self, _configured: Option<&str>) -> Result<NoDevice, Error> {
        Err(no_backend())
    }

    /// Always fails, as [`AudioSystem::connect`] does.
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
        let error = AudioSystem::connect().err().map(|error| error.to_string());
        let error = error.unwrap_or_default();
        assert!(error.contains("no audio backend"), "{error}");
        assert!(error.contains(std::env::consts::OS), "{error}");
    }

    #[test]
    fn listing_devices_answers_no_backend_and_never_panics() {
        let listed = AudioSystem::connect().and_then(|audio| audio.list_devices(LISTING_WAIT));
        assert!(matches!(
            listed,
            Err(Error::Audio(AudioError::NoBackend { .. }))
        ));
    }

    #[test]
    fn the_devices_command_prints_one_line_for_it_and_exits_cleanly() {
        let (mut out, mut err) = (Vec::new(), Vec::new());
        let environment = crate::cli::Environment::default();
        let scrubber = crate::secrets::Scrubber::new();
        let result = crate::devices_with(
            false,
            &environment,
            &scrubber,
            || {
                AudioSystem::connect()
                    .and_then(|audio| audio.list_devices(LISTING_WAIT))
                    .map_err(|error| error.to_string())
            },
            |_| Ok(()),
            &mut out,
            &mut err,
        );
        assert!(result.is_ok());
        let text = String::from_utf8(out).unwrap_or_default();
        assert!(text.contains("not listed: "), "{text}");
        assert!(text.contains("no audio backend"), "{text}");
    }

    #[test]
    fn the_error_is_the_audio_one_so_the_helper_reports_audio_unavailable() {
        assert!(matches!(
            AudioSystem::connect().err(),
            Some(Error::Audio(AudioError::NoBackend { .. }))
        ));
    }
}
