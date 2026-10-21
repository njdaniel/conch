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
    #[error("{path:?}: cannot read the configuration: {source}")]
    ConfigRead {
        /// The file.
        path: PathBuf,
        /// What the operating system said.
        #[source]
        source: std::io::Error,
    },

    /// The configuration file is not a regular file (a FIFO, a device, a directory). It is
    /// refused before it is opened, so that a FIFO cannot make the program wait.
    #[error("{path:?}: the configuration must be a regular file")]
    ConfigNotRegular {
        /// The file.
        path: PathBuf,
    },

    /// Others could change the configuration file, and its `server` decides where the login
    /// token is sent.
    #[error(
        "{path:?}: the configuration is writable by other users (mode {mode:04o}); run 'chmod go-w {path:?}'"
    )]
    ConfigWritable {
        /// The file.
        path: PathBuf,
        /// Its permission bits.
        mode: u32,
    },

    /// The configuration file, or a value laid over it, was refused.
    #[error(transparent)]
    Config(#[from] conch_voice_control::Error),

    /// `config set` found no configuration directory to save `voice.toml` in. It never
    /// creates one: `conch login` does, private.
    #[error("no configuration directory: run `conch login` first")]
    NoConfigDir,

    /// `config set` found a symbolic link where `voice.toml` is. Saving through it would
    /// write wherever it points, and saving over it would replace the link.
    #[error("{path:?} is a link: change it by hand")]
    ConfigLink {
        /// The link.
        path: PathBuf,
    },

    /// `config set` was asked to change a file that is not a configuration this program
    /// reads. The file is left as it is.
    #[error("{0}; fix voice.toml first")]
    ConfigToFix(#[source] conch_voice_control::Error),

    /// `config set` could not write the new `voice.toml`. The old one is as it was.
    #[error("{path:?}: cannot save the configuration: {source}")]
    ConfigWrite {
        /// The file.
        path: PathBuf,
        /// What the operating system said.
        #[source]
        source: std::io::Error,
    },

    /// Something else changed `voice.toml` while `config set` was saving it. What the other
    /// writer wrote is kept.
    #[error("{path:?} changed while saving: try again")]
    ConfigChanged {
        /// The file.
        path: PathBuf,
    },

    /// The login, the server address, or an answer of `conchd` that no retry can change.
    #[error(transparent)]
    Api(#[from] conch_voice_api::Error),

    /// No login for the default server, which nothing named, while logins are stored for
    /// other servers: the person most likely meant one of those, and "not signed in" alone
    /// would not tell them so.
    #[error("{error} ({note})")]
    DefaultServer {
        /// What the login said: not signed in to the default.
        error: conch_voice_api::Error,
        /// The stored servers and how to choose one ([`crate::cli::stored_elsewhere`]).
        note: String,
    },

    /// A test microphone or the counting sink was refused.
    #[error(transparent)]
    Audio(#[from] conch_voice_audio::AudioError),

    /// PipeWire could not be reached, or refused a stream. The detail is `dmn-audio`'s own
    /// words and holds nothing from the server or the login.
    #[error("cannot {doing}: {detail}")]
    PipeWire {
        /// What was being done.
        doing: &'static str,
        /// Why not.
        detail: String,
    },

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

    /// `managed` was started without its event pipe: file descriptor 3 is not open, or is
    /// not a pipe (`docs/design/tui-voice.md` §2). Nothing else was done.
    #[error("managed: file descriptor 3 {0}; conch-voice managed is started by conch, not by hand")]
    EventPipe(&'static str),

    /// `managed` failed for a reason it told its owner, as a failed state, and that no
    /// other error here describes. The sentence is this program's own.
    #[error("managed: {0}")]
    Managed(&'static str),
}

impl Error {
    /// The exit code `main` ends with: 2 for a mistake on the command line or a `managed`
    /// with no event pipe, the policy's code for a stop, and 1 otherwise.
    #[must_use]
    pub fn exit_code(&self) -> u8 {
        match self {
            Error::Usage(_) | Error::NoChannel | Error::NotATerminal | Error::EventPipe(_) => 2,
            Error::Stopped { exit_code, .. } => *exit_code,
            _ => EXIT_STOPPED,
        }
    }
}
