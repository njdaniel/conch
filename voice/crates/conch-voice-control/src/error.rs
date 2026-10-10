//! The crate's one error type.

use crate::keys::Key;

/// Everything this crate can refuse. Messages are one line and are meant to be shown to the
/// user as they are.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum Error {
    /// The configuration has a key this version does not know, most often a typo.
    #[error("{file}: unknown key `{key}`")]
    UnknownConfigKey {
        /// The file the caller said the text came from.
        file: String,
        /// The key, dotted from the top of the file (`audio.relase_tail_ms`).
        key: String,
    },

    /// A known key has a value of the wrong type, or one that is out of range.
    #[error("{file}: `{key}`: {message}")]
    InvalidConfigValue {
        /// The file the caller said the text came from.
        file: String,
        /// The key, dotted from the top of the file (`keys.talk`).
        key: String,
        /// What is wrong with the value.
        message: String,
    },

    /// The configuration is not valid TOML, or is wrong in a way that belongs to no one key.
    #[error("{file}: line {line}, column {column}: {message}")]
    InvalidConfig {
        /// The file the caller said the text came from.
        file: String,
        /// Line of the problem, from 1.
        line: usize,
        /// Column of the problem, from 1, in characters.
        column: usize,
        /// What is wrong.
        message: String,
    },

    /// A key was named that this crate's table of key names does not have, or numbered outside
    /// the range of key codes.
    #[error(
        "unknown key `{name}`: use a name such as KEY_RIGHTCTRL, or a key code from 1 to {max}",
        max = crate::keys::KEY_CODE_MAX
    )]
    UnknownKeyName {
        /// The name or number as it was written in the configuration or on the command line.
        name: String,
    },

    /// Two of talk, mute and deafen are bound to the same key.
    #[error("the {first} and {second} keys are bound to the same key")]
    DuplicateKeyBinding {
        /// One of the two.
        first: Key,
        /// The other.
        second: Key,
    },

    /// A line on standard input was not one of the five commands. The line itself is not
    /// kept: nothing typed into the client is echoed back into its output.
    #[error("unknown command: expected down, up, mute, deafen or quit")]
    UnknownCommand,
}
