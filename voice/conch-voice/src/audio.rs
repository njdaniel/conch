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
//!   defines is re-exported here, so the path `conch_voice::audio::PipeWire` and the others
//!   are what they were before the split.
//! - `windows` (`audio_windows.rs` and `audio_wasapi.rs`; `cfg(windows)`): WASAPI, through the
//!   `wasapi` crate (#1774). `audio_windows.rs` holds every decision that needs no WASAPI (which
//!   endpoint to follow, what to do when it goes away or the default moves, the rings) behind two
//!   small traits, and is compiled and tested on every platform; `audio_wasapi.rs` is the
//!   crate's side of them, Windows only. [`Wasapi`] is re-exported as `PipeWire`, the name the
//!   callers use, as the stub's is.
//! - `unsupported` (`audio_unsupported.rs`; every target with neither): no backend. [`PipeWire::connect`]
//!   returns [`AudioError::NoBackend`](conch_voice_audio::AudioError::NoBackend) (as
//!   [`Error::Audio`](crate::error::Error::Audio)), never a panic, so the helper reports
//!   `audio_unavailable` as it does for any audio it cannot reach. It is still compiled for the
//!   tests on Linux and on Windows, and `devices` on Windows answers with it.
//!
//! The files sit beside this one rather than in a directory because `tests/keydev.rs` reads
//! `src/` flat.
//!
//! The stub and the Windows backend keep the Linux type names (`PipeWire`) so that the callers
//! need no `cfg` of their own; renaming them to backend-neutral names is a separate change,
//! now that the second real backend exists. The ring logic of `audio_windows.rs` is a second
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
pub use wasapi_system::{Wasapi, Wasapi as PipeWire};
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
