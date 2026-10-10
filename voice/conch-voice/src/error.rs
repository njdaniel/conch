//! The crate's one error type. `main` turns it into one line on standard error and an exit
//! code.
//!
//! Nothing in an [`Error`] is a secret: the library crates' errors carry none, and text that
//! came from the SDK was scrubbed where it was received (`livekit.rs`).

use std::path::PathBuf;

use conch_voice_control::EXIT_STOPPED;

use crate::keydev::Problem;

/// Everything that ends `conch-voice` other than `quit`.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// An option on the command line that cannot be used as given.
    #[error("{0}")]
    Usage(String),

    /// No channel was named on the command line or in the configuration file.
    #[error("no channel: name one on the command line, or set `channel` in the configuration file")]
    NoChannel,

    /// The configuration file exists and could not be read.
    #[error("{path}: cannot read the configuration: {source}", path = .path.display())]
    ConfigRead {
        /// The file.
        path: PathBuf,
        /// What the operating system said.
        #[source]
        source: std::io::Error,
    },

    /// The configuration file, or a value laid over it, was refused.
    #[error(transparent)]
    Config(#[from] conch_voice_control::Error),

    /// The login, the server address, or an answer of `conchd` that no retry can change.
    #[error(transparent)]
    Api(#[from] conch_voice_api::Error),

    /// A test microphone or the counting sink was refused.
    #[error(transparent)]
    Audio(#[from] conch_voice_audio::AudioError),

    /// `conchd` issued a session with no grant for the channel's own room. V4 joins that
    /// room and no other.
    #[error("conchd issued a voice session with no grant for the channel's own room")]
    NoChannelGrant,

    /// The connection policy said to stop (`docs/design/conch-voice.md` §5). The message is
    /// the policy's own, and holds nothing from the server.
    #[error("{message}")]
    Stopped {
        /// The reason, for the user.
        message: &'static str,
        /// The process's exit code, which is not 0.
        exit_code: u8,
    },

    /// The async runtime or the signal handlers could not be set up.
    #[error("cannot start: {0}")]
    Runtime(#[source] std::io::Error),

    /// `keys` was run with standard output going somewhere other than a terminal.
    #[error(
        "keys shows every key pressed and writes only to a terminal: standard output is not one"
    )]
    NotATerminal,

    /// `keys` could not open or read the device it was given. The problem says why in
    /// words that hold nothing that was read.
    #[error("key device {device}: {problem}")]
    KeyDevice {
        /// The device, as it was named on the command line, made fit for one line.
        device: String,
        /// Why not.
        problem: Problem,
    },

    /// `devices` or `keys` could not write to standard output.
    #[error("cannot write to standard output: {0}")]
    Output(#[source] std::io::Error),
}

impl Error {
    /// The exit code `main` ends with: 2 for a mistake on the command line, the policy's
    /// code for a stop, and 1 otherwise.
    #[must_use]
    pub fn exit_code(&self) -> u8 {
        match self {
            Error::Usage(_) | Error::NoChannel | Error::NotATerminal => 2,
            Error::Stopped { exit_code, .. } => *exit_code,
            _ => EXIT_STOPPED,
        }
    }
}
