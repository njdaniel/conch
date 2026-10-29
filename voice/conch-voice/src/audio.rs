//! The real microphone and speakers, one backend per platform, behind the two traits of
//! `conch-voice-audio` (`MicSource`, `SpeakerSink`; `docs/design/conch-voice.md` §3, §9;
//! `docs/design/windows-desktop.md` §2).
//!
//! Input: the platform's audio system. Output: a `MicSource` and a `SpeakerSink` (with
//! [`Speakers`](crate::receive::Speakers)) for the rest of the helper to use, and, on Linux,
//! the list of devices. Owns: the choice of backend, and nothing else: the gate, the mixer,
//! the jitter buffers and the session loop are platform-neutral and live elsewhere.
//!
//! The backends:
//!
//! - `linux` (`audio_linux.rs`; `cfg(target_os = "linux")`): PipeWire, through `dmn-audio`. Everything it
//!   defines is re-exported here, so the path `conch_voice::audio::PipeWire` and the others
//!   are what they were before the split.
//! - `unsupported` (`audio_unsupported.rs`; every other target): no backend yet. [`PipeWire::connect`] returns
//!   [`AudioError::NoBackend`](conch_voice_audio::AudioError::NoBackend) (as
//!   [`Error::Audio`](crate::error::Error::Audio)), never a panic, so the helper reports
//!   `audio_unavailable` as it does for any audio it cannot reach. A Windows WASAPI backend
//!   (#776) is a sibling module with its own `cfg`, and takes that stub's place there.
//!
//! The files sit beside this one rather than in a directory because `tests/keydev.rs` reads
//! `src/` flat.
//!
//! The stub keeps the Linux type names (`PipeWire`, the other types here) so that the callers
//! need no `cfg` of their own; renaming them to backend-neutral names is a separate change
//! made when the second real backend exists.

#[cfg(target_os = "linux")]
#[path = "audio_linux.rs"]
mod linux;
#[cfg(target_os = "linux")]
pub use linux::*;

// Compiled on Linux only for the tests, which then exercise the stub's error.
#[cfg(any(not(target_os = "linux"), test))]
#[cfg_attr(target_os = "linux", allow(dead_code))]
#[path = "audio_unsupported.rs"]
mod unsupported;
#[cfg(not(target_os = "linux"))]
pub use unsupported::*;
