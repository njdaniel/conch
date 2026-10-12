//! The crate's one error type.

use std::path::PathBuf;

/// Everything this crate can refuse.
///
/// Only constructors return it. Nothing on a per-frame path can fail: once built, the gate,
/// the jitter buffer and the mixer have no error to report.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum AudioError {
    /// A setting was outside what the type accepts.
    #[error("{what} must be {must_be}, got {got}")]
    InvalidSetting {
        /// The setting, as a caller would name it.
        what: &'static str,
        /// The accepted range, in words.
        must_be: &'static str,
        /// The value that was refused.
        got: f64,
    },

    /// The WAV file could not be read at all.
    #[error("{}: cannot read: {source}", .file.display())]
    WavRead {
        /// The file.
        file: PathBuf,
        /// What the operating system said.
        #[source]
        source: std::io::Error,
    },

    /// The WAV file is larger than a WAV source loads.
    #[error("{}: larger than the {limit} bytes a WAV source loads", .file.display())]
    WavTooLarge {
        /// The file.
        file: PathBuf,
        /// The largest size accepted, in bytes.
        limit: u64,
    },

    /// The file is not a RIFF/WAVE file, or its structure is broken.
    #[error("{}: not a usable WAV file: {what}", .file.display())]
    WavMalformed {
        /// The file.
        file: PathBuf,
        /// What was wrong.
        what: &'static str,
    },

    /// A length field in the file points past the end of the file.
    #[error(
        "{}: the '{chunk}' chunk says it is {declared} bytes long but only {available} follow it",
        .file.display()
    )]
    WavChunkLength {
        /// The file.
        file: PathBuf,
        /// The chunk whose length is wrong, with unprintable bytes replaced.
        chunk: String,
        /// The length the file declares.
        declared: u64,
        /// The bytes that are really there.
        available: u64,
    },

    /// The file has no chunk of a kind a WAV file needs.
    #[error("{}: no '{chunk}' chunk", .file.display())]
    WavMissingChunk {
        /// The file.
        file: PathBuf,
        /// The missing chunk.
        chunk: &'static str,
    },

    /// The samples are not integer PCM (for example they are floating point).
    #[error(
        "{}: the samples are {encoding} (format tag {tag}), only integer PCM is accepted",
        .file.display()
    )]
    WavEncoding {
        /// The file.
        file: PathBuf,
        /// The format tag found.
        tag: u16,
        /// That tag in words.
        encoding: &'static str,
    },

    /// The file is not mono.
    #[error("{}: has {found} channels, only mono is accepted", .file.display())]
    WavChannels {
        /// The file.
        file: PathBuf,
        /// The channel count found.
        found: u16,
    },

    /// The file is not at 48 kHz. Nothing here resamples.
    #[error("{}: sample rate is {found} Hz, only 48000 Hz is accepted", .file.display())]
    WavSampleRate {
        /// The file.
        file: PathBuf,
        /// The sample rate found.
        found: u32,
    },

    /// The samples are not 16 bits wide.
    #[error("{}: samples are {found} bits wide, only 16-bit is accepted", .file.display())]
    WavBitDepth {
        /// The file.
        file: PathBuf,
        /// The width found.
        found: u16,
    },
}
