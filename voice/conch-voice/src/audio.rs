//! The real microphone and speakers, one backend per platform, behind the two traits of
//! `conch-voice-audio` (`MicSource`, `SpeakerSink`; `docs/design/conch-voice.md` §3, §9;
//! `docs/design/windows-desktop.md` §2).
//!
//! Input: the platform's audio system. Output: a `MicSource` and a `SpeakerSink` (with
//! [`Speakers`](crate::receive::Speakers)) for the rest of the helper to use, and, on Linux,
//! the list of devices (on Windows the lister exists, but `devices` does not use it until
//! the device list has a schema for it, #891). Owns: the choice of backend, and nothing else: the gate, the mixer,
//! the jitter buffers and the session loop are platform-neutral and live elsewhere.
//!
//! The backends:
//!
//! - `linux` (`audio_linux.rs`; `cfg(target_os = "linux")`): PipeWire, through `dmn-audio`. Everything it
//!   defines is re-exported here, so the path `conch_voice::audio::AudioSystem` and the others
//!   are what they were before the split (`AudioSystem` was `PipeWire`).
//! - `windows` (`audio_windows.rs` and `audio_wasapi.rs`; `cfg(windows)`): WASAPI, through the
//!   `wasapi` crate (#1774). `audio_windows.rs` holds every decision that needs no WASAPI (which
//!   endpoint to follow, what to do when it goes away or the default moves, the rings) behind two
//!   small traits, and is compiled and tested on every platform; `audio_wasapi.rs` is the
//!   crate's side of them, Windows only. [`Wasapi`] is re-exported as `AudioSystem`, the name the
//!   callers use, which the Linux type and the stub carry too.
//! - `unsupported` (`audio_unsupported.rs`; every target with neither): no backend. [`AudioSystem::connect`]
//!   returns [`AudioError::NoBackend`](conch_voice_audio::AudioError::NoBackend) (as
//!   [`Error::Audio`](crate::error::Error::Audio)), never a panic, so the helper reports
//!   `audio_unavailable` as it does for any audio it cannot reach. It is still compiled for the
//!   tests on Linux and on Windows, and `devices` on Windows answers with it.
//!
//! The files sit beside this one rather than in a directory because `tests/keydev.rs` reads
//! `src/` flat.
//!
//! Every backend names its connection `AudioSystem` so that the callers need no `cfg` of their
//! own, and the words a user reads about it (the help, the `devices` heading, a lost
//! connection) come from [`SYSTEM_NAME`] and [`CONNECTION_LOST`], which say PipeWire only
//! where PipeWire is what runs. The ring logic of `audio_windows.rs` is a second
//! copy of `audio_linux.rs`'s on purpose: this change leaves the Linux file as it was.

#[cfg(target_os = "linux")]
#[path = "audio_linux.rs"]
mod linux;
#[cfg(target_os = "linux")]
pub use linux::*;

// Compiled on Windows, and on Linux for the tests, which then exercise its logic with a fake
// sound system. Only Windows has the part that calls WASAPI.
#[cfg(windows)]
#[path = "audio_wasapi.rs"]
mod wasapi_system;
#[cfg(any(windows, test))]
#[cfg_attr(not(windows), allow(dead_code))]
#[path = "audio_windows.rs"]
mod windows;
#[cfg(windows)]
pub use wasapi_system::{Wasapi, Wasapi as AudioSystem};
#[cfg(windows)]
pub use windows::{LISTING_WAIT, WasapiMic, WasapiSpeakers};

// Compiled on Linux only for the tests, which then exercise the stub's error. On Windows it
// is compiled too, so that its tests run there, but its names are not used.
#[cfg(any(not(target_os = "linux"), test))]
#[cfg_attr(any(target_os = "linux", windows), allow(dead_code))]
#[path = "audio_unsupported.rs"]
mod unsupported;
#[cfg(not(any(target_os = "linux", windows)))]
pub use unsupported::*;

/// What the platform's audio system is called in anything a user reads: the help, the
/// `devices` heading. PipeWire on Linux, WASAPI on Windows, neutral words elsewhere.
#[cfg(target_os = "linux")]
pub const SYSTEM_NAME: &str = "PipeWire";
/// See the Linux definition.
#[cfg(windows)]
pub const SYSTEM_NAME: &str = "WASAPI";
/// See the Linux definition.
#[cfg(not(any(target_os = "linux", windows)))]
pub const SYSTEM_NAME: &str = "the audio system";

/// What the managed helper says when its connection to the audio system is lost before it
/// finishes connecting; [`SYSTEM_NAME`]'s words, as a `&'static str` for [`Error::Managed`](crate::error::Error::Managed).
#[cfg(target_os = "linux")]
pub const CONNECTION_LOST: &str = "the connection to PipeWire was lost";
/// See the Linux definition.
#[cfg(windows)]
pub const CONNECTION_LOST: &str = "the connection to WASAPI was lost";
/// See the Linux definition.
#[cfg(not(any(target_os = "linux", windows)))]
pub const CONNECTION_LOST: &str = "the connection to the audio system was lost";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn what_a_user_reads_names_the_platforms_own_audio_system_and_no_other() {
        #[cfg(target_os = "linux")]
        assert_eq!(SYSTEM_NAME, "PipeWire");
        #[cfg(windows)]
        assert_eq!(SYSTEM_NAME, "WASAPI");
        assert!(CONNECTION_LOST.contains(SYSTEM_NAME));
        #[cfg(windows)]
        assert!(!SYSTEM_NAME.contains("PipeWire") && !CONNECTION_LOST.contains("PipeWire"));
    }
}
