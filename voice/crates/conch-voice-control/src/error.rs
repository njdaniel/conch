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

    /// `[audio]` is written as an inline table, with dotted keys, or with a value that is not
    /// a string where a device belongs, and [`crate::set_audio_value`] edits none of those.
    #[error("{file}: `[audio]` is written in a form this command does not edit: change it by hand")]
    AudioFormNotEditable {
        /// The file the caller said the text came from.
        file: String,
    },

    /// `[keys]` is written as an inline table, with dotted keys, or with a value that is not
    /// a string where [`crate::set_keys_value`] writes one, and neither it nor
    /// [`crate::unset_keys_value`] edits those.
    #[error("{file}: `[keys]` is written in a form this command does not edit: change it by hand")]
    KeysFormNotEditable {
        /// The file the caller said the text came from.
        file: String,
    },

    /// An edit [`crate::set_audio_value`] made did not pass its own check: the result read
    /// differently, or the text changed outside the one value. Nothing is to be written.
    #[error("{file}: setting `{key}` would change more than its value: change it by hand")]
    AudioEditUnchecked {
        /// The file the caller said the text came from.
        file: String,
        /// The key, dotted from the top of the file (`audio.input`).
        key: String,
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

    /// A key binding in the form of the other platform: `sc:` or `mouse:` on Linux, a `KEY_*`
    /// name or a number on Windows.
    #[error("`{name}` is not a key binding this platform reads: use {here}")]
    WrongKeyForm {
        /// The binding as it was written.
        name: String,
        /// What this platform reads.
        here: &'static str,
    },

    /// A text in the shape of a Windows binding (`sc:` or `mouse:`) that the grammar does not
    /// take.
    #[error("`{name}` is not a Windows key binding: use sc:0x29, sc:0xE0,0x1D, mouse:4 or mouse:5")]
    InvalidWindowsBinding {
        /// The binding as it was written.
        name: String,
    },

    /// A binding in the Windows grammar that no key event ever matches (`sc:0x54`,
    /// `sc:0xE0,0x46`, `sc:0xE0,0x2A`).
    #[error(
        "`{name}` is a scan code that never fires: Alt+PrintScreen is reported as sc:0xE0,0x37, Ctrl+Pause as sc:0x45, and the shift before PrintScreen is dropped: bind the key's own code"
    )]
    UnreachableWindowsBinding {
        /// The binding as it was written.
        name: String,
    },

    /// `keys.device`, which names an evdev path, on Windows, where every keyboard is read.
    #[error(
        "{file}: `keys.device` is a Linux setting: on Windows keys are bound by scan code, with no device"
    )]
    KeyDeviceNotOnWindows {
        /// The file the caller said the text came from.
        file: String,
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
