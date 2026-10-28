//! The configuration file (`docs/design/conch-voice.md` §8).
//!
//! The caller reads `voice.toml` from the configuration directory (`CONCH_CONFIG_DIR`, else
//! `$XDG_CONFIG_HOME/conch`) and hands the text to
//! [`Config::parse`]; a file that does not exist is [`Config::default`]. Values from the
//! environment and the command line are the caller's to collect, into a [`ConfigOverrides`]
//! per layer, applied with [`Config::apply`] in the order it wants them to win.
//!
//! [`set_audio_value`] gives the text back with one `[audio]` device, or the talk mode,
//! changed, for `conch-voice config set` to save. It changes nothing else in the text.

use std::fmt;
use std::ops::Range;
use std::time::Duration;

use serde::Deserialize;
use serde::de::{self, Deserializer, Visitor};
use toml::de::{DeTable, DeValue};

use crate::error::Error;
use crate::keys::{KeyBindings, KeyCode};
use crate::ptt::TalkMode;

/// The server address when neither the command line, `CONCH_SERVER` nor the file gives one,
/// and no single stored login stands in for them ([`Config::server_or`]): the same default
/// as `conch`.
pub const DEFAULT_SERVER: &str = "http://127.0.0.1:8080";
/// How long the gate stays open after a release, so the last syllable is not cut.
pub const DEFAULT_RELEASE_TAIL_MS: u32 = 100;
/// The longest a single press may transmit. It catches a stuck key.
pub const DEFAULT_MAX_TRANSMIT_SECS: u32 = 120;
/// The level, in dBFS, at or above which one frame is sound in open mode
/// (`docs/design/tui-voice.md` §20.6).
pub const DEFAULT_OPEN_THRESHOLD_DB: i32 = -40;
/// How long, in open mode, with no frame at or above the level before the gate shuts. The
/// browser's open mode holds for as long.
pub const DEFAULT_OPEN_HANG_MS: u32 = 400;

/// The whole configuration, with defaults filled in where the design note states one.
///
/// A field that is `None` was not set anywhere and has no fixed default: the caller decides
/// (for `server`, the only stored login's server and then [`DEFAULT_SERVER`], as
/// [`Config::server_or`] says; for a device, PipeWire's default; for a key, not bound).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Config {
    /// `server`: the address of `conchd`.
    pub server: Option<String>,
    /// `channel`: the channel `join` uses when none is named.
    pub channel: Option<String>,
    /// `[keys]`.
    pub keys: KeysConfig,
    /// `[audio]`.
    pub audio: AudioConfig,
}

/// `[keys]`: the keyboard to read and what its keys do.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct KeysConfig {
    /// `device`: the keyboard's stable path under `/dev/input/by-id/`.
    pub device: Option<String>,
    /// `talk`: held to transmit.
    pub talk: Option<KeyCode>,
    /// `mute`: pressed to mute or unmute.
    pub mute: Option<KeyCode>,
    /// `deafen`: pressed to deafen or undeafen.
    pub deafen: Option<KeyCode>,
}

/// Why a device name that is empty or padded with white space is refused.
const DEVICE_NAME_MUST_BE: &str = "must be the exact name of a device, or `default`, with no white space around it: an empty or padded name would be read as the default device";

/// `[audio]`: devices and the transmit gate's timing.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioConfig {
    /// `input`: the microphone, by PipeWire name. `None` is PipeWire's default.
    pub input: Option<String>,
    /// `output`: the speakers, by PipeWire name. `None` is PipeWire's default.
    pub output: Option<String>,
    /// `echo_cancellation`: off unless set (the note's decision 8).
    pub echo_cancellation: bool,
    /// `release_tail_ms`: see [`DEFAULT_RELEASE_TAIL_MS`]. The gate applies it.
    pub release_tail_ms: u32,
    /// `max_transmit_secs`: see [`DEFAULT_MAX_TRANSMIT_SECS`]. At least 1. The push-to-talk
    /// state machine applies it, to each press and, in open mode, to each opening.
    pub max_transmit_secs: u32,
    /// `talk_mode`: what may open the gate, `"push_to_talk"` (the default) or `"open"`.
    pub talk_mode: TalkMode,
    /// `open_threshold_db`: see [`DEFAULT_OPEN_THRESHOLD_DB`]. From
    /// [`MIN_OPEN_THRESHOLD_DB`] to [`MAX_OPEN_THRESHOLD_DB`]. The level detector applies it.
    pub open_threshold_db: i32,
    /// `open_hang_ms`: see [`DEFAULT_OPEN_HANG_MS`]. From [`MIN_OPEN_HANG_MS`] to
    /// [`MAX_OPEN_HANG_MS`]. The level detector applies it.
    pub open_hang_ms: u32,
}

impl Default for AudioConfig {
    fn default() -> Self {
        Self {
            input: None,
            output: None,
            echo_cancellation: false,
            release_tail_ms: DEFAULT_RELEASE_TAIL_MS,
            max_transmit_secs: DEFAULT_MAX_TRANSMIT_SECS,
            talk_mode: TalkMode::PushToTalk,
            open_threshold_db: DEFAULT_OPEN_THRESHOLD_DB,
            open_hang_ms: DEFAULT_OPEN_HANG_MS,
        }
    }
}

/// The longest release tail accepted, in milliseconds. The gate stays open for the tail
/// after the key is released, and nothing else bounds it.
pub const MAX_RELEASE_TAIL_MS: u32 = 2_000;
/// The longest transmit limit accepted, in seconds. Beyond this it would not be a limit.
pub const MAX_MAX_TRANSMIT_SECS: u32 = 600;
/// The lowest open-mode level accepted, in dBFS. Lower, a quiet room's own hiss would be
/// sound.
pub const MIN_OPEN_THRESHOLD_DB: i32 = -80;
/// The highest open-mode level accepted, in dBFS. Higher, speech would not be.
pub const MAX_OPEN_THRESHOLD_DB: i32 = -10;
/// The shortest open-mode hang accepted, in milliseconds. Shorter would cut words at each
/// pause, and send a pair of transmit reports for each piece.
pub const MIN_OPEN_HANG_MS: u32 = 100;
/// The longest open-mode hang accepted, in milliseconds. The gate stays open this long
/// after the sound stops, as it does for the longest release tail after a release.
pub const MAX_OPEN_HANG_MS: u32 = 2_000;

/// What a talk mode may be, for an error.
const TALK_MODE_MUST_BE: &str = "must be \"push_to_talk\" or \"open\"";

/// One layer of values to put over a [`Config`]: the environment, or the command line. A
/// field left `None` leaves the configuration as it was.
///
/// Key names from such a source are turned into a [`KeyCode`] with `str::parse`, which
/// gives the same error as a bad name in the file.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ConfigOverrides {
    /// Replaces `server`.
    pub server: Option<String>,
    /// Replaces `channel`.
    pub channel: Option<String>,
    /// Replaces `keys.device`.
    pub key_device: Option<String>,
    /// Replaces `keys.talk`.
    pub talk: Option<KeyCode>,
    /// Replaces `keys.mute`.
    pub mute: Option<KeyCode>,
    /// Replaces `keys.deafen`.
    pub deafen: Option<KeyCode>,
    /// Replaces `audio.input`.
    pub input: Option<String>,
    /// Replaces `audio.output`.
    pub output: Option<String>,
    /// Replaces `audio.echo_cancellation`.
    pub echo_cancellation: Option<bool>,
    /// Replaces `audio.release_tail_ms`.
    pub release_tail_ms: Option<u32>,
    /// Replaces `audio.max_transmit_secs`.
    pub max_transmit_secs: Option<u32>,
    /// Replaces `audio.talk_mode`.
    pub talk_mode: Option<TalkMode>,
    /// Replaces `audio.open_threshold_db`.
    pub open_threshold_db: Option<i32>,
    /// Replaces `audio.open_hang_ms`.
    pub open_hang_ms: Option<u32>,
}

impl Config {
    /// Parses the text of a configuration file. Every key is optional, and empty text is
    /// [`Config::default`]. `file` is the name errors give for where the text came from; it is
    /// not opened.
    ///
    /// # Errors
    ///
    /// - [`Error::UnknownConfigKey`] for a key this version does not know, so that a typo is
    ///   not silently ignored.
    /// - [`Error::InvalidConfigValue`] for a value of the wrong type, a key name that is not
    ///   in the table, two bindings to one key, or a value [`Config::validate`] refuses.
    /// - [`Error::InvalidConfig`] for text that is not TOML.
    ///
    /// All three name `file`, and the first two name the key.
    pub fn parse(text: &str, file: &str) -> Result<Self, Error> {
        let raw: RawConfig = toml::from_str(text).map_err(|error| locate(text, file, &error))?;

        let invalid = |key: &str, message: String| Error::InvalidConfigValue {
            file: file.to_owned(),
            key: key.to_owned(),
            message,
        };
        let key_code = |key: &str, raw: Option<RawKey>| -> Result<Option<KeyCode>, Error> {
            raw.map(|raw| {
                raw.resolve()
                    .map_err(|error| invalid(key, error.to_string()))
            })
            .transpose()
        };

        let keys = KeysConfig {
            device: raw.keys.device,
            talk: key_code("keys.talk", raw.keys.talk)?,
            mute: key_code("keys.mute", raw.keys.mute)?,
            deafen: key_code("keys.deafen", raw.keys.deafen)?,
        };
        let defaults = AudioConfig::default();
        let talk_mode = match raw.audio.talk_mode {
            None => defaults.talk_mode,
            Some(name) => TalkMode::from_name(&name)
                .ok_or_else(|| invalid("audio.talk_mode", TALK_MODE_MUST_BE.to_owned()))?,
        };
        let audio = AudioConfig {
            input: raw.audio.input,
            output: raw.audio.output,
            echo_cancellation: raw
                .audio
                .echo_cancellation
                .unwrap_or(defaults.echo_cancellation),
            release_tail_ms: raw
                .audio
                .release_tail_ms
                .unwrap_or(defaults.release_tail_ms),
            max_transmit_secs: raw
                .audio
                .max_transmit_secs
                .unwrap_or(defaults.max_transmit_secs),
            talk_mode,
            open_threshold_db: raw
                .audio
                .open_threshold_db
                .unwrap_or(defaults.open_threshold_db),
            open_hang_ms: raw.audio.open_hang_ms.unwrap_or(defaults.open_hang_ms),
        };
        let config = Self {
            server: raw.server,
            channel: raw.channel,
            keys,
            audio,
        };
        config.validate(file)?;
        Ok(config)
    }

    /// Checks the values that have a range. [`Config::parse`] and [`Config::apply`] both end
    /// here, so no layer can put in what another would have refused. `source` is the name
    /// errors give for where the values came from.
    ///
    /// The two times are bounded because each keeps the microphone open: the release tail
    /// after the key is up (nothing else limits it), and the longest single press, which is
    /// the only thing that ends a transmission whose release was lost. The open-mode hang
    /// keeps it open after the sound stops and is bounded as the tail is; the open-mode
    /// level is bounded so that neither a quiet room nor nothing at all counts as sound.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidConfigValue`] naming the key.
    pub fn validate(&self, source: &str) -> Result<(), Error> {
        let invalid = |key: &str, message: String| Error::InvalidConfigValue {
            file: source.to_owned(),
            key: key.to_owned(),
            message,
        };
        // dmn-audio trims a device name and reads an empty one as PipeWire's default, so a
        // name that is empty or has white space around it would open another device than
        // the one written, the default among them. Refused, as an unknown key is: the
        // exact `default`, and no key, are the ways to ask for the default.
        for (key, name) in [
            ("audio.input", &self.audio.input),
            ("audio.output", &self.audio.output),
        ] {
            if name
                .as_deref()
                .is_some_and(|name| name.is_empty() || name.trim() != name)
            {
                return Err(invalid(key, DEVICE_NAME_MUST_BE.to_owned()));
            }
        }
        if self.audio.release_tail_ms > MAX_RELEASE_TAIL_MS {
            return Err(invalid(
                "audio.release_tail_ms",
                format!(
                    "must be at most {MAX_RELEASE_TAIL_MS}: the microphone stays open this long after the key is released"
                ),
            ));
        }
        if !(1..=MAX_MAX_TRANSMIT_SECS).contains(&self.audio.max_transmit_secs) {
            return Err(invalid(
                "audio.max_transmit_secs",
                format!(
                    "must be from 1 to {MAX_MAX_TRANSMIT_SECS}: 0 would shut the gate on every press, and more would be no limit on a stuck key"
                ),
            ));
        }
        let threshold = self.audio.open_threshold_db;
        if !(MIN_OPEN_THRESHOLD_DB..=MAX_OPEN_THRESHOLD_DB).contains(&threshold) {
            return Err(invalid(
                "audio.open_threshold_db",
                format!(
                    "must be from {MIN_OPEN_THRESHOLD_DB} to {MAX_OPEN_THRESHOLD_DB} (dBFS): lower would count a quiet room as sound, higher would miss speech"
                ),
            ));
        }
        if !(MIN_OPEN_HANG_MS..=MAX_OPEN_HANG_MS).contains(&self.audio.open_hang_ms) {
            return Err(invalid(
                "audio.open_hang_ms",
                format!(
                    "must be from {MIN_OPEN_HANG_MS} to {MAX_OPEN_HANG_MS}: in open mode the microphone stays open this long after the sound stops"
                ),
            ));
        }
        if let Some(server) = &self.server {
            // The value itself is not put in the message: it may hold a password.
            let rest = server
                .strip_prefix("http://")
                .or_else(|| server.strip_prefix("https://"));
            let authority = rest.map(|rest| rest.split(['/', '?', '#']).next().unwrap_or(""));
            match authority {
                None | Some("") => {
                    return Err(invalid(
                        "server",
                        "must be an http:// or https:// address with a host".to_owned(),
                    ));
                }
                Some(authority) if authority.contains('@') => {
                    return Err(invalid(
                        "server",
                        "must not contain a user name or password: sign in with `conch login`"
                            .to_owned(),
                    ));
                }
                Some(_) => {}
            }
        }
        if let Err(error) = KeyBindings::new(self.keys.talk, self.keys.mute, self.keys.deafen) {
            let key = match &error {
                Error::DuplicateKeyBinding { second, .. } => format!("keys.{second}"),
                _ => "keys".to_owned(),
            };
            return Err(invalid(&key, error.to_string()));
        }
        if let Some(device) = &self.keys.device
            && (!device.starts_with("/dev/input/")
                || device.split('/').any(|part| part == ".." || part == "."))
        {
            return Err(invalid(
                "keys.device",
                "must be a path under /dev/input/, such as /dev/input/by-id/...-event-kbd"
                    .to_owned(),
            ));
        }
        Ok(())
    }

    /// Puts one layer of overrides on top, then checks the result as [`Config::validate`]
    /// does. Apply the environment's and then the command line's for the usual order.
    /// `source` names the layer in an error ("the command line").
    ///
    /// # Errors
    ///
    /// [`Error::InvalidConfigValue`] if the result is out of range. The configuration is
    /// then left as it was.
    pub fn apply(&mut self, overrides: ConfigOverrides, source: &str) -> Result<(), Error> {
        let mut next = self.clone();
        next.put(overrides);
        next.validate(source)?;
        *self = next;
        Ok(())
    }

    fn put(&mut self, overrides: ConfigOverrides) {
        fn put<T>(slot: &mut Option<T>, value: Option<T>) {
            if value.is_some() {
                *slot = value;
            }
        }
        put(&mut self.server, overrides.server);
        put(&mut self.channel, overrides.channel);
        put(&mut self.keys.device, overrides.key_device);
        put(&mut self.keys.talk, overrides.talk);
        put(&mut self.keys.mute, overrides.mute);
        put(&mut self.keys.deafen, overrides.deafen);
        put(&mut self.audio.input, overrides.input);
        put(&mut self.audio.output, overrides.output);
        if let Some(value) = overrides.echo_cancellation {
            self.audio.echo_cancellation = value;
        }
        if let Some(value) = overrides.release_tail_ms {
            self.audio.release_tail_ms = value;
        }
        if let Some(value) = overrides.max_transmit_secs {
            self.audio.max_transmit_secs = value;
        }
        if let Some(value) = overrides.talk_mode {
            self.audio.talk_mode = value;
        }
        if let Some(value) = overrides.open_threshold_db {
            self.audio.open_threshold_db = value;
        }
        if let Some(value) = overrides.open_hang_ms {
            self.audio.open_hang_ms = value;
        }
    }

    /// The server address, or [`DEFAULT_SERVER`] if no layer set one.
    #[must_use]
    pub fn server_or_default(&self) -> &str {
        self.server_or(None)
    }

    /// The server address by `conch`'s rule (`serverEnvOr` in `internal/cli/command.go`):
    /// the one a layer set, else `only_login`, else [`DEFAULT_SERVER`].
    ///
    /// `only_login` is the server of the stored login when exactly one is stored, and the
    /// caller passes it only when `CONCH_TOKEN` is not set: that token is not the stored
    /// login, and choosing the login's server would send it somewhere it was never meant
    /// for. With several logins none is preferred, as in `conch`.
    #[must_use]
    pub fn server_or<'a>(&'a self, only_login: Option<&'a str>) -> &'a str {
        self.server
            .as_deref()
            .or(only_login)
            .unwrap_or(DEFAULT_SERVER)
    }

    /// The keys to hand to a [`crate::KeyDecoder`], as they stand after any overrides.
    ///
    /// # Errors
    ///
    /// [`Error::DuplicateKeyBinding`] if an override bound two things to one key.
    pub fn key_bindings(&self) -> Result<KeyBindings, Error> {
        KeyBindings::new(self.keys.talk, self.keys.mute, self.keys.deafen)
    }
}

impl AudioConfig {
    /// `release_tail_ms` as a duration, for the gate.
    #[must_use]
    pub fn release_tail(&self) -> Duration {
        Duration::from_millis(u64::from(self.release_tail_ms))
    }

    /// `max_transmit_secs` as a duration, for [`crate::Ptt::new`].
    #[must_use]
    pub fn max_transmit(&self) -> Duration {
        Duration::from_secs(u64::from(self.max_transmit_secs))
    }

    /// `open_hang_ms` as a duration, for the level detector.
    #[must_use]
    pub fn open_hang(&self) -> Duration {
        Duration::from_millis(u64::from(self.open_hang_ms))
    }
}

/// A key under `[audio]` that [`set_audio_value`] writes: a device, or the talk mode. It
/// writes no other.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AudioKey {
    /// `input`: the microphone.
    Input,
    /// `output`: the speakers.
    Output,
    /// `talk_mode`: what may open the gate, so that a client can save its choice. The
    /// open-mode level and hang are changed by hand only.
    TalkMode,
}

impl AudioKey {
    /// The key's name in `[audio]`.
    #[must_use]
    pub fn name(self) -> &'static str {
        match self {
            AudioKey::Input => "input",
            AudioKey::Output => "output",
            AudioKey::TalkMode => "talk_mode",
        }
    }

    /// The key's name dotted from the top of the file, as errors and the command line give it.
    #[must_use]
    pub fn dotted(self) -> &'static str {
        match self {
            AudioKey::Input => "audio.input",
            AudioKey::Output => "audio.output",
            AudioKey::TalkMode => "audio.talk_mode",
        }
    }

    /// The device field this key names, for the tests to say by hand what an edit must
    /// give. Only the two devices have one.
    #[cfg(test)]
    fn slot(self, config: &mut Config) -> &mut Option<String> {
        match self {
            AudioKey::Input => &mut config.audio.input,
            AudioKey::Output => &mut config.audio.output,
            AudioKey::TalkMode => panic!("talk_mode is not a device"),
        }
    }
}

/// `text`, the configuration file, with `[audio]`'s `key` set to `value` and nothing else
/// changed: every comment, blank line, key and table stays as it was written, byte for byte.
/// `file` names the text in errors, as for [`Config::parse`]. For a device, `"default"` is
/// a value like any other, and is read as PipeWire's default; for the talk mode, `value` is
/// `push_to_talk` or `open`.
///
/// The edit is the smallest one there is, in this order of preference:
///
/// - the key is in a `[audio]` table: its value's text is replaced;
/// - a `[audio]` table is there without the key: one line, `key = "value"`, goes directly
///   after its header, with the header's line ending;
/// - `audio` is not in the file at all: a blank line, `[audio]` and the key's line are
///   added at the end. Empty text becomes those two lines alone.
///
/// `value` is written between double quotes as it is, so it may hold no quotation mark,
/// backslash or control character: then no escape is needed, and none is written. The
/// result is checked before it is returned: [`Config::parse`] must read it as the old
/// configuration with this one field changed, and the old text must be in it unchanged but
/// for the old value's own text.
///
/// # Errors
///
/// - What [`Config::parse`] says of `text`: a file it refuses is not edited.
/// - [`Error::InvalidConfigValue`] for a `value` that cannot be written as it is, or a talk
///   mode that does not exist.
/// - [`Error::AudioFormNotEditable`] when `audio` is an inline table or dotted keys, or the
///   key's value is not a string.
/// - [`Error::AudioEditUnchecked`] if the result does not pass the check.
pub fn set_audio_value(
    text: &str,
    file: &str,
    key: AudioKey,
    value: &str,
) -> Result<String, Error> {
    let before = Config::parse(text, file)?;
    let refuse = |message: &str| Error::InvalidConfigValue {
        file: file.to_owned(),
        key: key.dotted().to_owned(),
        message: message.to_owned(),
    };
    // What the file must read as afterwards: the old configuration with this one field set.
    let mut want = before;
    match key {
        AudioKey::Input | AudioKey::Output => {
            if value.is_empty()
                || value.trim() != value
                || value.contains(['"', '\\'])
                || value.chars().any(char::is_control)
            {
                return Err(refuse(
                    "a device name must not be empty or have white space around it, and must hold no quotation mark, backslash or control character",
                ));
            }
            let device = if key == AudioKey::Input {
                &mut want.audio.input
            } else {
                &mut want.audio.output
            };
            *device = Some(value.to_owned());
        }
        // Either name is written as it is; anything else is refused before it is written.
        AudioKey::TalkMode => {
            want.audio.talk_mode =
                TalkMode::from_name(value).ok_or_else(|| refuse(TALK_MODE_MUST_BE))?;
        }
    }
    let quoted = format!("\"{value}\"");
    let (range, replacement) = plan_audio_edit(text, file, key, &quoted)?;
    let mut edited = String::with_capacity(text.len() + replacement.len());
    edited.push_str(&text[..range.start]);
    edited.push_str(&replacement);
    edited.push_str(&text[range.end..]);

    // The edit is checked by what it gives, not by how it was made: read back by the same
    // parser that will read it, and compared byte for byte with the text it came from.
    let unchecked = || Error::AudioEditUnchecked {
        file: file.to_owned(),
        key: key.dotted().to_owned(),
    };
    let after = Config::parse(&edited, file).map_err(|_| unchecked())?;
    if after != want {
        return Err(unchecked());
    }
    // Either the old text is all there with one run added (an insertion), or what differs
    // lies within the old value's own text.
    let changed = changed_region(text, &edited);
    let within = range.start <= changed.start && changed.end <= range.end;
    if !changed.is_empty() && !within {
        return Err(unchecked());
    }
    Ok(edited)
}

/// Where in `text` the edit goes, and what replaces that range: an empty range is an
/// insertion.
fn plan_audio_edit(
    text: &str,
    file: &str,
    key: AudioKey,
    quoted: &str,
) -> Result<(Range<usize>, String), Error> {
    let not_editable = || Error::AudioFormNotEditable {
        file: file.to_owned(),
    };
    let document = DeTable::parse(text).map_err(|error| locate(text, file, &error))?;
    let audio = document
        .get_ref()
        .iter()
        .find(|(name, _)| name.get_ref() == "audio")
        .map(|(_, value)| value);
    let eol = line_ending(text);
    let line = format!("{} = {quoted}", key.name());

    let Some(audio) = audio else {
        // A file with no `audio` at all: the table goes at the end, after a blank line.
        let mut added = String::new();
        if !text.is_empty() {
            if !text.ends_with('\n') {
                added.push_str(eol);
            }
            added.push_str(eol);
        }
        added.push_str("[audio]");
        added.push_str(eol);
        added.push_str(&line);
        added.push_str(eol);
        return Ok((text.len()..text.len(), added));
    };

    // A table opened by a `[audio]` header has the header's text as its span. An inline
    // table's span starts with `{`, and a table made by dotted keys has the span of its
    // name in the first of them, which never starts with `[`.
    let span = audio.span();
    let header = text
        .get(span.clone())
        .is_some_and(|header| header.starts_with('[') && !header.starts_with("[["));
    let DeValue::Table(table) = audio.get_ref() else {
        return Err(not_editable());
    };
    if !header {
        return Err(not_editable());
    }
    let present = table
        .iter()
        .find(|(name, _)| name.get_ref() == key.name())
        .map(|(_, value)| value);
    match present {
        Some(present) if present.get_ref().is_str() => Ok((present.span(), quoted.to_owned())),
        Some(_) => Err(not_editable()),
        // Directly after the header's line, which ends with a line ending unless it is the
        // last line of the file.
        None => match text[span.end..].find('\n') {
            Some(newline) => {
                let at = span.end + newline + 1;
                let eol = if text[..at].ends_with("\r\n") {
                    "\r\n"
                } else {
                    "\n"
                };
                Ok((at..at, format!("{line}{eol}")))
            }
            None => Ok((text.len()..text.len(), format!("{eol}{line}"))),
        },
    }
}

/// The file's line ending, from its first line: CRLF if that line has one, else LF.
fn line_ending(text: &str) -> &'static str {
    match text.find('\n') {
        Some(newline) if text[..newline].ends_with('\r') => "\r\n",
        _ => "\n",
    }
}

/// The range of `old` that differs from `new`: what is left once the bytes the two begin
/// with and end with alike are taken away. It is empty when `new` is `old` with one run of
/// bytes added.
fn changed_region(old: &str, new: &str) -> Range<usize> {
    let (old, new) = (old.as_bytes(), new.as_bytes());
    let prefix = old
        .iter()
        .zip(new)
        .take_while(|(left, right)| left == right)
        .count();
    let suffix = old[prefix..]
        .iter()
        .rev()
        .zip(new[prefix..].iter().rev())
        .take_while(|(left, right)| left == right)
        .count();
    prefix..old.len() - suffix
}

/// The file as serde reads it: every key optional, and no key that is not listed.
#[derive(Deserialize, Default)]
#[serde(deny_unknown_fields)]
struct RawConfig {
    server: Option<String>,
    channel: Option<String>,
    #[serde(default)]
    keys: RawKeys,
    #[serde(default)]
    audio: RawAudio,
}

#[derive(Deserialize, Default)]
#[serde(deny_unknown_fields)]
struct RawKeys {
    device: Option<String>,
    talk: Option<RawKey>,
    mute: Option<RawKey>,
    deafen: Option<RawKey>,
}

#[derive(Deserialize, Default)]
#[serde(deny_unknown_fields)]
struct RawAudio {
    input: Option<String>,
    output: Option<String>,
    echo_cancellation: Option<bool>,
    release_tail_ms: Option<u32>,
    max_transmit_secs: Option<u32>,
    talk_mode: Option<String>,
    open_threshold_db: Option<i32>,
    open_hang_ms: Option<u32>,
}

/// A key as the file gives it: a name (`"KEY_RIGHTCTRL"`), or its code as a number (`97`) or
/// as a string (`"97"`).
enum RawKey {
    Name(String),
    Number(i64),
}

impl RawKey {
    fn resolve(self) -> Result<KeyCode, Error> {
        match self {
            RawKey::Name(name) => name.parse(),
            RawKey::Number(number) => u64::try_from(number)
                .ok()
                .and_then(KeyCode::from_number)
                .ok_or(Error::UnknownKeyName {
                    name: number.to_string(),
                }),
        }
    }
}

impl<'de> Deserialize<'de> for RawKey {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct RawKeyVisitor;

        impl Visitor<'_> for RawKeyVisitor {
            type Value = RawKey;

            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a key name such as \"KEY_RIGHTCTRL\", or a key code")
            }

            fn visit_str<E: de::Error>(self, value: &str) -> Result<RawKey, E> {
                Ok(RawKey::Name(value.to_owned()))
            }

            fn visit_i64<E: de::Error>(self, value: i64) -> Result<RawKey, E> {
                Ok(RawKey::Number(value))
            }
        }

        deserializer.deserialize_any(RawKeyVisitor)
    }
}

/// How serde words a key that `deny_unknown_fields` refused. The error carries no kind, only
/// this text; `an_unknown_key_is_an_error_naming_the_key_and_the_file` fails if it changes.
const UNKNOWN_FIELD: &str = "unknown field ";

/// Turns the TOML parser's error, which knows a position, into one that names the key at
/// that position.
fn locate(text: &str, file: &str, error: &toml::de::Error) -> Error {
    let file = file.to_owned();
    let message = error.message().to_owned();
    let span = error.span().unwrap_or(0..0);

    // A second, untyped parse gives every key and value its position. It fails only if the
    // text is not TOML at all, and then there is no key to name.
    let found = DeTable::parse(text)
        .ok()
        .and_then(|table| find(table.get_ref(), &span, ""));
    match found {
        Some(key) if message.starts_with(UNKNOWN_FIELD) => Error::UnknownConfigKey { file, key },
        Some(key) => Error::InvalidConfigValue { file, key, message },
        None => {
            let (line, column) = line_and_column(text, span.start);
            Error::InvalidConfig {
                file,
                line,
                column,
                message,
            }
        }
    }
}

/// The dotted name of the innermost key whose own text, or whose value's text, holds the
/// start of `span`.
fn find(table: &DeTable<'_>, span: &Range<usize>, prefix: &str) -> Option<String> {
    for (key, value) in table {
        let name = if prefix.is_empty() {
            key.get_ref().to_string()
        } else {
            format!("{prefix}.{}", key.get_ref())
        };
        if key.span().contains(&span.start) {
            return Some(name);
        }
        if let DeValue::Table(inner) = value.get_ref()
            && let Some(found) = find(inner, span, &name)
        {
            return Some(found);
        }
        if value.span().contains(&span.start) {
            return Some(name);
        }
    }
    None
}

/// The line and column, both from 1, of a byte offset in `text`.
fn line_and_column(text: &str, offset: usize) -> (usize, usize) {
    let mut end = offset.min(text.len());
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    let before = &text[..end];
    let line = before.matches('\n').count() + 1;
    let line_start = before.rfind('\n').map_or(0, |newline| newline + 1);
    let column = before[line_start..].chars().count() + 1;
    (line, column)
}

#[cfg(test)]
mod tests {
    use super::*;

    const FILE: &str = "/home/someone/.config/conch/voice.toml";
    const SOURCE: &str = "the command line";

    /// Every key of the note's §8, each with a value that is not its default.
    const EVERY_KEY: &str = r#"
server = "https://conch.example.test"
channel = "ops"

[keys]
device = "/dev/input/by-id/usb-Example_Keyboard-event-kbd"
talk = "KEY_RIGHTCTRL"
mute = "KEY_F13"
deafen = 184

[audio]
input = "alsa_input.example-mic"
output = "alsa_output.example-headset"
echo_cancellation = true
release_tail_ms = 250
max_transmit_secs = 45
talk_mode = "open"
open_threshold_db = -55
open_hang_ms = 650
"#;

    fn key(name: &str) -> Option<KeyCode> {
        Some(name.parse().unwrap())
    }

    fn every_key() -> Config {
        Config {
            server: Some("https://conch.example.test".to_owned()),
            channel: Some("ops".to_owned()),
            keys: KeysConfig {
                device: Some("/dev/input/by-id/usb-Example_Keyboard-event-kbd".to_owned()),
                talk: key("KEY_RIGHTCTRL"),
                mute: key("KEY_F13"),
                deafen: key("KEY_F14"),
            },
            audio: AudioConfig {
                input: Some("alsa_input.example-mic".to_owned()),
                output: Some("alsa_output.example-headset".to_owned()),
                echo_cancellation: true,
                release_tail_ms: 250,
                max_transmit_secs: 45,
                talk_mode: TalkMode::Open,
                open_threshold_db: -55,
                open_hang_ms: 650,
            },
        }
    }

    #[test]
    fn every_key_is_read() {
        assert_eq!(Config::parse(EVERY_KEY, FILE), Ok(every_key()));
    }

    #[test]
    fn empty_text_is_all_defaults() {
        let want = Config {
            server: None,
            channel: None,
            keys: KeysConfig {
                device: None,
                talk: None,
                mute: None,
                deafen: None,
            },
            audio: AudioConfig {
                input: None,
                output: None,
                echo_cancellation: false,
                release_tail_ms: 100,
                max_transmit_secs: 120,
                talk_mode: TalkMode::PushToTalk,
                open_threshold_db: -40,
                open_hang_ms: 400,
            },
        };
        for text in ["", "\n", "# nothing set\n", "[keys]\n", "[audio]\n[keys]\n"] {
            assert_eq!(Config::parse(text, FILE), Ok(want.clone()), "{text:?}");
        }
        // An absent file is the caller's Config::default(), which must be the same thing.
        assert_eq!(Config::default(), want);
        assert_eq!(want.server_or_default(), "http://127.0.0.1:8080");
        assert_eq!(want.audio.release_tail(), Duration::from_millis(100));
        assert_eq!(want.audio.max_transmit(), Duration::from_secs(120));
        assert_eq!(want.key_bindings(), Ok(KeyBindings::default()));
    }

    #[test]
    fn a_key_left_out_keeps_its_default_beside_keys_that_are_set() {
        let config = Config::parse("[audio]\nrelease_tail_ms = 0\n", FILE).unwrap();
        assert_eq!(config.audio.release_tail_ms, 0);
        assert_eq!(config.audio.max_transmit_secs, DEFAULT_MAX_TRANSMIT_SECS);
        assert!(!config.audio.echo_cancellation);
    }

    #[test]
    fn a_key_may_be_a_name_a_number_or_a_number_in_a_string() {
        for text in [
            "keys.talk = \"KEY_RIGHTCTRL\"",
            "keys.talk = \"key_rightctrl\"",
            "keys.talk = 97",
            "keys.talk = \"97\"",
            "keys = { talk = 97 }",
            "[keys]\ntalk = 0x61",
        ] {
            let config = Config::parse(text, FILE).unwrap();
            assert_eq!(config.keys.talk, key("KEY_RIGHTCTRL"), "{text}");
        }
    }

    #[test]
    fn an_unknown_key_is_an_error_naming_the_key_and_the_file() {
        let cases = [
            ("sever = \"http://127.0.0.1:1\"", "sever"),
            ("[keys]\ntlak = \"KEY_F13\"", "keys.tlak"),
            ("[audio]\nrelase_tail_ms = 100", "audio.relase_tail_ms"),
            ("audio.echo = true", "audio.echo"),
            ("keys = { talk = 97, push = 98 }", "keys.push"),
            ("[video]\ncamera = \"none\"", "video"),
            ("[audio.advanced]\ngain = 2", "audio.advanced"),
            (
                "server = \"http://127.0.0.1:1\"\n\n[audio]\ninput = \"a\"\nvolume = 3\n",
                "audio.volume",
            ),
        ];
        for (text, key) in cases {
            let error = Config::parse(text, FILE).unwrap_err();
            assert_eq!(
                error,
                Error::UnknownConfigKey {
                    file: FILE.to_owned(),
                    key: key.to_owned()
                },
                "{text}"
            );
            assert_eq!(error.to_string(), format!("{FILE}: unknown key `{key}`"));
        }
    }

    #[test]
    fn an_empty_or_padded_device_name_is_refused_naming_the_key() {
        for name in ["", " ", "\\t", " mic", "mic ", "  default"] {
            for key in ["input", "output"] {
                let text = format!("[audio]\n{key} = \"{name}\"\n");
                let error = Config::parse(&text, FILE).unwrap_err();
                assert!(
                    matches!(&error, Error::InvalidConfigValue { key: k, .. } if *k == format!("audio.{key}")),
                    "{name:?}: {error}"
                );
            }
        }
        for (text, want) in [
            ("[audio]\ninput = \"default\"\n", Some("default")),
            ("[audio]\ninput = \"mic\"\n", Some("mic")),
            ("[audio]\n", None),
        ] {
            let config = Config::parse(text, FILE).unwrap();
            assert_eq!(config.audio.input.as_deref(), want);
        }
    }

    #[test]
    fn config_set_refuses_an_empty_or_padded_device_name() {
        for key in [AudioKey::Input, AudioKey::Output] {
            for value in ["", " ", "  ", " mic", "mic "] {
                let error = set_audio_value("", FILE, key, value).unwrap_err();
                assert!(
                    matches!(&error, Error::InvalidConfigValue { key: k, .. } if k == key.dotted()),
                    "{value:?}: {error}"
                );
            }
        }
    }

    #[test]
    fn a_value_of_the_wrong_type_is_an_error_naming_the_key_and_the_file() {
        let cases = [
            ("server = 8080", "server"),
            ("channel = [\"ops\"]", "channel"),
            ("keys = \"KEY_F13\"", "keys"),
            ("[keys]\ndevice = 3", "keys.device"),
            ("[keys]\ntalk = true", "keys.talk"),
            ("[keys]\nmute = 1.5", "keys.mute"),
            ("[keys]\ndeafen = [97]", "keys.deafen"),
            ("audio = 5", "audio"),
            ("[audio]\ninput = false", "audio.input"),
            ("[audio]\noutput = 1", "audio.output"),
            (
                "[audio]\necho_cancellation = \"yes\"",
                "audio.echo_cancellation",
            ),
            (
                "[audio]\nrelease_tail_ms = \"100\"",
                "audio.release_tail_ms",
            ),
            ("[audio]\nrelease_tail_ms = -1", "audio.release_tail_ms"),
            ("[audio]\nrelease_tail_ms = 1.5", "audio.release_tail_ms"),
            (
                "[audio]\nmax_transmit_secs = \"two minutes\"",
                "audio.max_transmit_secs",
            ),
            (
                "[audio]\nmax_transmit_secs = 4294967296",
                "audio.max_transmit_secs",
            ),
            (
                "audio = { max_transmit_secs = true }",
                "audio.max_transmit_secs",
            ),
            (
                "channel = \"ops\"\n[audio]\ninput = \"a\"\nrelease_tail_ms = true\noutput = \"b\"\n",
                "audio.release_tail_ms",
            ),
            // A table where a plain value belongs, and the other way round.
            ("[server]\nport = 1", "server"),
            ("server.port = 1", "server"),
            ("[[audio]]\ninput = \"a\"", "audio"),
            ("[keys.talk]\ncode = 97", "keys.talk"),
            (
                "[audio]\nrelease_tail_ms = { ms = 100 }",
                "audio.release_tail_ms",
            ),
            ("[keys]\ntalk = 1979-05-27", "keys.talk"),
        ];
        for (text, key) in cases {
            match Config::parse(text, FILE).unwrap_err() {
                Error::InvalidConfigValue {
                    file,
                    key: got,
                    message,
                } => {
                    assert_eq!(file, FILE, "{text}");
                    assert_eq!(got, key, "{text}");
                    assert!(
                        !message.is_empty() && !message.contains('\n'),
                        "{message:?}"
                    );
                }
                other => panic!("{text}: {other:?}"),
            }
        }
    }

    #[test]
    fn a_wrong_type_says_what_was_expected() {
        let error = Config::parse("[audio]\nrelease_tail_ms = \"soon\"", FILE).unwrap_err();
        assert_eq!(
            error.to_string(),
            format!("{FILE}: `audio.release_tail_ms`: invalid type: string \"soon\", expected u32")
        );
        let error = Config::parse("[keys]\ntalk = true", FILE).unwrap_err();
        assert_eq!(
            error.to_string(),
            format!(
                "{FILE}: `keys.talk`: invalid type: boolean `true`, \
                 expected a key name such as \"KEY_RIGHTCTRL\", or a key code"
            )
        );
    }

    #[test]
    fn an_unknown_key_name_is_an_error_naming_it_the_key_and_the_file() {
        let cases = [
            ("[keys]\ntalk = \"KEY_BOGUS\"", "keys.talk", "KEY_BOGUS"),
            ("[keys]\nmute = \"RIGHTCTRL\"", "keys.mute", "RIGHTCTRL"),
            ("[keys]\ndeafen = 0", "keys.deafen", "0"),
            ("[keys]\ntalk = 768", "keys.talk", "768"),
            ("[keys]\ntalk = -97", "keys.talk", "-97"),
            ("[keys]\ntalk = \"\"", "keys.talk", ""),
        ];
        for (text, key, name) in cases {
            let error = Config::parse(text, FILE).unwrap_err();
            assert_eq!(
                error,
                Error::InvalidConfigValue {
                    file: FILE.to_owned(),
                    key: key.to_owned(),
                    message: Error::UnknownKeyName {
                        name: name.to_owned()
                    }
                    .to_string(),
                },
                "{text}"
            );
            let shown = error.to_string();
            assert!(
                shown.starts_with(&format!("{FILE}: `{key}`: unknown key `{name}`")),
                "{shown}"
            );
        }
    }

    #[test]
    fn two_bindings_to_one_key_are_an_error_naming_the_second() {
        let error = Config::parse("[keys]\ntalk = \"KEY_F13\"\nmute = 183\n", FILE).unwrap_err();
        assert_eq!(
            error.to_string(),
            format!("{FILE}: `keys.mute`: the talk and mute keys are bound to the same key")
        );
    }

    #[test]
    fn a_transmit_limit_of_zero_is_refused() {
        let error = Config::parse("[audio]\nmax_transmit_secs = 0\n", FILE).unwrap_err();
        match error {
            Error::InvalidConfigValue { file, key, .. } => {
                assert_eq!(
                    (file.as_str(), key.as_str()),
                    (FILE, "audio.max_transmit_secs")
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn text_that_is_not_toml_is_an_error_naming_the_file_and_the_place() {
        let error = Config::parse("server = \"a\"\nchannel = \n", FILE).unwrap_err();
        match &error {
            Error::InvalidConfig {
                file,
                line,
                column,
                message,
            } => {
                assert_eq!(file, FILE);
                assert_eq!((*line, *column), (2, 11));
                assert!(
                    !message.is_empty() && !message.contains('\n'),
                    "{message:?}"
                );
            }
            other => panic!("{other:?}"),
        }
        assert!(
            error
                .to_string()
                .starts_with(&format!("{FILE}: line 2, column 11: "))
        );

        // A key given twice is not TOML either.
        let error = Config::parse("channel = \"a\"\nchannel = \"b\"\n", FILE).unwrap_err();
        assert!(
            matches!(error, Error::InvalidConfig { line: 2, .. }),
            "{error:?}"
        );
    }

    #[test]
    fn line_and_column_count_from_one_and_in_characters() {
        let text = "ab\nçd = 1\n";
        assert_eq!(line_and_column(text, 0), (1, 1));
        assert_eq!(line_and_column(text, 2), (1, 3));
        assert_eq!(line_and_column(text, 3), (2, 1));
        // 'ç' is two bytes: offset 5 is the 'd' after it, the second character of line 2.
        assert_eq!(line_and_column(text, 5), (2, 2));
        // Inside 'ç' rounds down to it, and past the end is the end.
        assert_eq!(line_and_column(text, 4), (2, 1));
        assert_eq!(line_and_column(text, 999), (3, 1));
        assert_eq!(line_and_column("", 0), (1, 1));
    }

    #[test]
    fn overrides_replace_only_what_they_set() {
        let mut config = every_key();
        config.apply(ConfigOverrides::default(), SOURCE).unwrap();
        assert_eq!(config, every_key());

        config
            .apply(
                ConfigOverrides {
                    channel: Some("general".to_owned()),
                    talk: key("KEY_F15"),
                    max_transmit_secs: Some(30),
                    ..ConfigOverrides::default()
                },
                SOURCE,
            )
            .unwrap();
        let mut want = every_key();
        want.channel = Some("general".to_owned());
        want.keys.talk = key("KEY_F15");
        want.audio.max_transmit_secs = 30;
        assert_eq!(config, want);
    }

    #[test]
    fn every_field_can_be_overridden() {
        let mut config = Config::default();
        config
            .apply(
                ConfigOverrides {
                    server: Some("https://conch.example.test".to_owned()),
                    channel: Some("ops".to_owned()),
                    key_device: Some("/dev/input/by-id/usb-Example_Keyboard-event-kbd".to_owned()),
                    talk: key("KEY_RIGHTCTRL"),
                    mute: key("KEY_F13"),
                    deafen: key("KEY_F14"),
                    input: Some("alsa_input.example-mic".to_owned()),
                    output: Some("alsa_output.example-headset".to_owned()),
                    echo_cancellation: Some(true),
                    release_tail_ms: Some(250),
                    max_transmit_secs: Some(45),
                    talk_mode: Some(TalkMode::Open),
                    open_threshold_db: Some(-55),
                    open_hang_ms: Some(650),
                },
                SOURCE,
            )
            .unwrap();
        assert_eq!(config, every_key());
        assert_eq!(config.server_or_default(), "https://conch.example.test");
    }

    #[test]
    fn layers_apply_in_the_order_given() {
        let mut config = Config::parse("channel = \"from-file\"\n", FILE).unwrap();
        let environment = ConfigOverrides {
            server: Some("http://from-env.test".to_owned()),
            channel: Some("from-env".to_owned()),
            ..ConfigOverrides::default()
        };
        let command_line = ConfigOverrides {
            channel: Some("from-flag".to_owned()),
            ..ConfigOverrides::default()
        };
        config.apply(environment, SOURCE).unwrap();
        config.apply(command_line, SOURCE).unwrap();
        assert_eq!(config.channel.as_deref(), Some("from-flag"));
        assert_eq!(config.server.as_deref(), Some("http://from-env.test"));
    }

    #[test]
    fn a_server_any_layer_set_wins_over_the_only_login_and_that_over_the_default() {
        // (the file's server, the only stored login's server, the server used)
        let table = [
            (None, None, DEFAULT_SERVER),
            (None, Some("http://login.test:1"), "http://login.test:1"),
            (Some("http://file.test"), None, "http://file.test"),
            (
                Some("http://file.test"),
                Some("http://login.test:1"),
                "http://file.test",
            ),
        ];
        for (file, only_login, want) in table {
            let text = file.map_or(String::new(), |server| format!("server = {server:?}\n"));
            let config = Config::parse(&text, FILE).unwrap();
            assert_eq!(
                config.server_or(only_login),
                want,
                "{file:?} {only_login:?}"
            );
            // server_or_default is the rule with no login to stand in.
            assert_eq!(config.server_or_default(), file.unwrap_or(DEFAULT_SERVER));
        }
        // A layer over the file wins over the login the same way.
        let mut config = Config::default();
        config
            .apply(
                ConfigOverrides {
                    server: Some("http://from-env.test".to_owned()),
                    ..ConfigOverrides::default()
                },
                SOURCE,
            )
            .unwrap();
        assert_eq!(
            config.server_or(Some("http://login.test:1")),
            "http://from-env.test"
        );
    }

    #[test]
    fn overrides_that_collide_are_refused_and_change_nothing() {
        let mut config = every_key();
        let error = config
            .apply(
                ConfigOverrides {
                    deafen: key("KEY_RIGHTCTRL"),
                    ..ConfigOverrides::default()
                },
                SOURCE,
            )
            .unwrap_err();
        assert!(
            matches!(&error, Error::InvalidConfigValue { file, key, .. } if file == SOURCE && key == "keys.deafen"),
            "{error:?}"
        );
        assert_eq!(config, every_key(), "a refused layer leaves nothing behind");
        assert_eq!(
            every_key().key_bindings().unwrap().talk(),
            key("KEY_RIGHTCTRL")
        );
    }

    /// The two times that keep the microphone open are bounded, and the bound holds for
    /// the file and for every layer over it.
    #[test]
    fn the_times_that_keep_the_microphone_open_are_bounded_in_every_layer() {
        let in_file = [
            ("[audio]\nrelease_tail_ms = 2001\n", "audio.release_tail_ms"),
            (
                "[audio]\nrelease_tail_ms = 4294967295\n",
                "audio.release_tail_ms",
            ),
            (
                "[audio]\nmax_transmit_secs = 0\n",
                "audio.max_transmit_secs",
            ),
            (
                "[audio]\nmax_transmit_secs = 601\n",
                "audio.max_transmit_secs",
            ),
            (
                "[audio]\nmax_transmit_secs = 4294967295\n",
                "audio.max_transmit_secs",
            ),
        ];
        for (text, want) in in_file {
            let error = Config::parse(text, FILE).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == want),
                "{text:?}: {error:?}"
            );
        }
        for (text, tail, limit) in [
            (
                "[audio]\nrelease_tail_ms = 2000\nmax_transmit_secs = 600\n",
                2000,
                600,
            ),
            (
                "[audio]\nrelease_tail_ms = 0\nmax_transmit_secs = 1\n",
                0,
                1,
            ),
        ] {
            let config = Config::parse(text, FILE).unwrap();
            assert_eq!(
                (config.audio.release_tail_ms, config.audio.max_transmit_secs),
                (tail, limit)
            );
        }

        let layers = [
            (
                ConfigOverrides {
                    release_tail_ms: Some(u32::MAX),
                    ..ConfigOverrides::default()
                },
                "audio.release_tail_ms",
            ),
            (
                ConfigOverrides {
                    max_transmit_secs: Some(0),
                    ..ConfigOverrides::default()
                },
                "audio.max_transmit_secs",
            ),
            (
                ConfigOverrides {
                    max_transmit_secs: Some(u32::MAX),
                    ..ConfigOverrides::default()
                },
                "audio.max_transmit_secs",
            ),
        ];
        for (layer, want) in layers {
            let mut config = Config::default();
            let error = config.apply(layer, SOURCE).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == want),
                "{error:?}"
            );
            assert_eq!(config, Config::default());
        }
    }

    #[test]
    fn a_server_must_be_http_with_a_host_and_no_password() {
        for bad in [
            "",
            "conch.example.test",
            "ftp://conch.example.test",
            "http://",
            "https:///path",
            "https://user:FAKE-PASSWORD@conch.example.test",
            "http://user@conch.example.test/prefix",
        ] {
            let text = format!("server = {bad:?}\n");
            let error = Config::parse(&text, FILE).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == "server"),
                "{bad:?}: {error:?}"
            );
            assert!(
                !error.to_string().contains("FAKE-PASSWORD"),
                "the value is not echoed: {error}"
            );
        }
        for good in [
            "http://127.0.0.1:8080",
            "https://conch.example.test/prefix",
            "http://[::1]:8080",
        ] {
            let text = format!("server = {good:?}\n");
            assert_eq!(
                Config::parse(&text, FILE).unwrap().server.as_deref(),
                Some(good)
            );
        }
    }

    #[test]
    fn a_key_device_must_be_under_dev_input() {
        for bad in [
            "",
            "/etc/shadow",
            "../../relative",
            "/dev/input/../sda",
            "/dev/input/./event3",
            "/dev/inputs/event3",
        ] {
            let text = format!("[keys]\ndevice = {bad:?}\n");
            let error = Config::parse(&text, FILE).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == "keys.device"),
                "{bad:?}: {error:?}"
            );
        }
        let good = "/dev/input/by-id/usb-Example_Keyboard-event-kbd";
        let text = format!("[keys]\ndevice = {good:?}\n");
        assert_eq!(
            Config::parse(&text, FILE).unwrap().keys.device.as_deref(),
            Some(good)
        );
    }

    /// A file as a person keeps one: comments everywhere, blank lines, keys in no order.
    const KEPT: &str = "# conch-voice\n\
                        channel = \"ops\"   # the usual one\n\
                        \n\
                        [keys]\n\
                        talk = \"KEY_F13\"\n\
                        \n\
                        # devices\n\
                        [audio]\n\
                        release_tail_ms = 150\n\
                        # the headset\n\
                        input = \"alsa_input.old\" # chosen by hand\n\
                        # end of audio\n\
                        output = 'alsa_output.literal'\n\
                        \n\
                        # trailing comment\n";

    /// `set_audio_value`'s results, as whole texts: (what, before, key, value, after).
    #[test]
    fn setting_a_device_changes_that_value_and_nothing_else() {
        let replaced_input = KEPT.replace(
            "\"alsa_input.old\"",
            "\"alsa_input.usb-Example_Mic-00.mono-fallback\"",
        );
        let replaced_output = KEPT.replace("'alsa_output.literal'", "\"default\"");
        let cases: &[(&str, &str, AudioKey, &str, &str)] = &[
            (
                "comments before, after and on the key's line",
                KEPT,
                AudioKey::Input,
                "alsa_input.usb-Example_Mic-00.mono-fallback",
                &replaced_input,
            ),
            (
                "a single-quoted literal replaced",
                KEPT,
                AudioKey::Output,
                "default",
                &replaced_output,
            ),
            (
                "CRLF line endings, the key present",
                "[audio]\r\ninput = \"a\"\r\noutput = \"b\"\r\n",
                AudioKey::Output,
                "c",
                "[audio]\r\ninput = \"a\"\r\noutput = \"c\"\r\n",
            ),
            (
                "CRLF line endings, the key absent",
                "channel = \"ops\"\r\n[audio] # devices\r\noutput = \"b\"\r\n",
                AudioKey::Input,
                "a",
                "channel = \"ops\"\r\n[audio] # devices\r\ninput = \"a\"\r\noutput = \"b\"\r\n",
            ),
            (
                "CRLF line endings, the table absent",
                "channel = \"ops\"\r\n",
                AudioKey::Input,
                "a",
                "channel = \"ops\"\r\n\r\n[audio]\r\ninput = \"a\"\r\n",
            ),
            (
                "[ audio ] with spaces",
                "[ audio ]\noutput = \"b\"\n",
                AudioKey::Input,
                "a",
                "[ audio ]\ninput = \"a\"\noutput = \"b\"\n",
            ),
            (
                "the key absent: inserted after the header",
                "[audio]\nrelease_tail_ms = 150\n[keys]\ntalk = 97\n",
                AudioKey::Output,
                "b",
                "[audio]\noutput = \"b\"\nrelease_tail_ms = 150\n[keys]\ntalk = 97\n",
            ),
            (
                "the key absent, the header the last line with no line ending",
                "channel = \"ops\"\n[audio]",
                AudioKey::Input,
                "a",
                "channel = \"ops\"\n[audio]\ninput = \"a\"",
            ),
            (
                "the table absent",
                "server = \"http://127.0.0.1:8080\"\n\n[keys]\ntalk = 97\n",
                AudioKey::Input,
                "a",
                "server = \"http://127.0.0.1:8080\"\n\n[keys]\ntalk = 97\n\n[audio]\ninput = \"a\"\n",
            ),
            (
                "the table absent, the last line with no line ending",
                "channel = \"ops\" # last",
                AudioKey::Output,
                "b",
                "channel = \"ops\" # last\n\n[audio]\noutput = \"b\"\n",
            ),
            (
                "an empty file",
                "",
                AudioKey::Input,
                "default",
                "[audio]\ninput = \"default\"\n",
            ),
            (
                "the same value again",
                "[audio]\ninput = \"a\"\n",
                AudioKey::Input,
                "a",
                "[audio]\ninput = \"a\"\n",
            ),
            (
                "a multi-line string, a quoted key and other characters in the name",
                "[audio]\n\"input\" = \"\"\"\nold\"\"\"\n",
                AudioKey::Input,
                "alsa_input.pci-0000_00_1f.3.analog-stereo:é",
                "[audio]\n\"input\" = \"alsa_input.pci-0000_00_1f.3.analog-stereo:é\"\n",
            ),
        ];
        for (what, before, key, value, want) in cases {
            let got = set_audio_value(before, FILE, *key, value)
                .unwrap_or_else(|error| panic!("{what}: {error}"));
            assert_eq!(&got, want, "{what}");

            // The parsed configuration differs in exactly the one field.
            let mut expected = Config::parse(before, FILE).unwrap();
            *key.slot(&mut expected) = Some((*value).to_owned());
            assert_eq!(Config::parse(&got, FILE).unwrap(), expected, "{what}");
            if got != *before {
                assert_ne!(Config::parse(before, FILE).unwrap(), expected, "{what}");
            }

            // Outside one run of bytes, the text is as it was.
            let changed = changed_region(before, &got);
            let after = before.len() - changed.end;
            assert_eq!(
                got.as_bytes()[..changed.start],
                before.as_bytes()[..changed.start],
                "{what}"
            );
            assert_eq!(
                got.as_bytes()[got.len() - after..],
                before.as_bytes()[changed.end..],
                "{what}"
            );
        }
    }

    #[test]
    fn the_other_device_and_every_other_field_are_left_as_they_were() {
        let mut want = Config::parse(KEPT, FILE).unwrap();
        let got = set_audio_value(KEPT, FILE, AudioKey::Input, "x").unwrap();
        want.audio.input = Some("x".to_owned());
        let after = Config::parse(&got, FILE).unwrap();
        assert_eq!(after, want);
        assert_eq!(after.audio.output.as_deref(), Some("alsa_output.literal"));
        assert_eq!(after.channel.as_deref(), Some("ops"));
        assert_eq!(after.audio.release_tail_ms, 150);
    }

    #[test]
    fn an_inline_or_dotted_audio_or_a_value_that_is_not_a_string_is_refused() {
        let not_editable = Error::AudioFormNotEditable {
            file: FILE.to_owned(),
        };
        for text in [
            "audio = { input = \"a\" }\n",
            "audio = {}\n",
            "audio = { release_tail_ms = 150 }\n",
            "audio.input = \"a\"\n",
            "audio.release_tail_ms = 150\n",
            "channel = \"ops\"\naudio.output = \"b\"\n",
            "\"audio\".input = \"a\"\n",
        ] {
            for key in [AudioKey::Input, AudioKey::Output] {
                assert_eq!(
                    set_audio_value(text, FILE, key, "x"),
                    Err(not_editable.clone()),
                    "{text:?} {key:?}"
                );
            }
        }
        // A value that is not a string is refused before that, by the parser: a file it
        // refuses is not edited.
        for text in [
            "[audio]\ninput = false\n",
            "[audio]\ninput = [\"a\"]\n",
            "[audio]\ninput = { name = \"a\" }\n",
        ] {
            let error = set_audio_value(text, FILE, AudioKey::Input, "x").unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key.starts_with("audio.input")),
                "{text:?}: {error:?}"
            );
        }
    }

    #[test]
    fn a_file_the_parser_refuses_is_not_edited() {
        for text in [
            "chanel = \"typo\"\n",
            "[audio]\ninput = \n",
            "[audio]\nvolume = 3\n",
            "[audio]\nmax_transmit_secs = 0\n",
            "[[audio]]\ninput = \"a\"\n",
        ] {
            let parsed = Config::parse(text, FILE).unwrap_err();
            assert_eq!(
                set_audio_value(text, FILE, AudioKey::Input, "x"),
                Err(parsed),
                "{text:?}"
            );
        }
    }

    #[test]
    fn a_value_that_cannot_be_written_as_it_is_is_refused_without_being_shown() {
        for value in [
            "",
            "with \"quotes\"",
            "back\\slash",
            "new\nline",
            "carriage\rreturn",
            "tab\there",
            "escape\u{1b}[2J",
            "delete\u{7f}",
        ] {
            let error = set_audio_value("", FILE, AudioKey::Output, value).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == "audio.output"),
                "{value:?}: {error:?}"
            );
            assert!(
                value.is_empty() || !error.to_string().contains(value),
                "{error}"
            );
        }
    }

    #[test]
    fn the_changed_region_is_what_lies_between_the_common_ends() {
        assert_eq!(changed_region("abc", "abc"), 3..3);
        assert_eq!(changed_region("abc", "aXc"), 1..2);
        assert_eq!(changed_region("ac", "abc"), 1..1);
        assert_eq!(changed_region("aa", "aaa"), 2..2);
        assert_eq!(changed_region("", "x"), 0..0);
        assert_eq!(changed_region("abc", "xyz"), 0..3);
        assert_eq!(changed_region("abc", ""), 0..3);
    }

    /// The open-microphone keys (`docs/design/tui-voice.md` §20.6): each read with its
    /// default when left out, its whole range accepted, and anything else refused naming
    /// the key, in the file and in every layer over it.
    #[test]
    fn the_open_microphone_keys_have_defaults_and_ranges() {
        let defaults = Config::parse("[audio]\ninput = \"a\"\n", FILE)
            .unwrap()
            .audio;
        assert_eq!(
            (
                defaults.talk_mode,
                defaults.open_threshold_db,
                defaults.open_hang_ms
            ),
            (TalkMode::PushToTalk, -40, 400)
        );
        assert_eq!(defaults.open_hang(), Duration::from_millis(400));

        // (the text, the mode, the level, the hang)
        let accepted = [
            (
                "talk_mode = \"push_to_talk\"",
                TalkMode::PushToTalk,
                -40,
                400,
            ),
            ("talk_mode = \"open\"", TalkMode::Open, -40, 400),
            ("talk_mode = 'open'", TalkMode::Open, -40, 400),
            ("open_threshold_db = -80", TalkMode::PushToTalk, -80, 400),
            ("open_threshold_db = -10", TalkMode::PushToTalk, -10, 400),
            ("open_hang_ms = 100", TalkMode::PushToTalk, -40, 100),
            ("open_hang_ms = 2000", TalkMode::PushToTalk, -40, 2000),
            (
                "talk_mode = \"open\"\nopen_threshold_db = -60\nopen_hang_ms = 250",
                TalkMode::Open,
                -60,
                250,
            ),
        ];
        for (text, mode, level, hang) in accepted {
            let audio = Config::parse(&format!("[audio]\n{text}\n"), FILE)
                .unwrap()
                .audio;
            assert_eq!(
                (audio.talk_mode, audio.open_threshold_db, audio.open_hang_ms),
                (mode, level, hang),
                "{text}"
            );
            // The keys that were there before are untouched.
            assert_eq!(audio.release_tail_ms, DEFAULT_RELEASE_TAIL_MS, "{text}");
            assert_eq!(audio.max_transmit_secs, DEFAULT_MAX_TRANSMIT_SECS, "{text}");
        }

        let refused = [
            ("talk_mode = \"Open\"", "audio.talk_mode"),
            ("talk_mode = \"always\"", "audio.talk_mode"),
            ("talk_mode = \"\"", "audio.talk_mode"),
            ("talk_mode = \"push-to-talk\"", "audio.talk_mode"),
            ("talk_mode = \" open\"", "audio.talk_mode"),
            ("talk_mode = true", "audio.talk_mode"),
            ("talk_mode = 1", "audio.talk_mode"),
            ("talk_mode = [\"open\"]", "audio.talk_mode"),
            ("open_threshold_db = -81", "audio.open_threshold_db"),
            ("open_threshold_db = -9", "audio.open_threshold_db"),
            ("open_threshold_db = 0", "audio.open_threshold_db"),
            ("open_threshold_db = 40", "audio.open_threshold_db"),
            ("open_threshold_db = -40.5", "audio.open_threshold_db"),
            ("open_threshold_db = \"-40\"", "audio.open_threshold_db"),
            ("open_threshold_db = -2147483649", "audio.open_threshold_db"),
            ("open_hang_ms = 99", "audio.open_hang_ms"),
            ("open_hang_ms = 0", "audio.open_hang_ms"),
            ("open_hang_ms = 2001", "audio.open_hang_ms"),
            ("open_hang_ms = -400", "audio.open_hang_ms"),
            ("open_hang_ms = 4294967295", "audio.open_hang_ms"),
            ("open_hang_ms = \"400\"", "audio.open_hang_ms"),
        ];
        for (text, key) in refused {
            let error = Config::parse(&format!("[audio]\n{text}\n"), FILE).unwrap_err();
            match &error {
                Error::InvalidConfigValue {
                    file,
                    key: got,
                    message,
                } => {
                    assert_eq!((file.as_str(), got.as_str()), (FILE, key), "{text}");
                    assert!(
                        !message.is_empty() && !message.contains('\n'),
                        "{message:?}"
                    );
                }
                other => panic!("{text}: {other:?}"),
            }
        }
        let error = Config::parse("[audio]\ntalk_mode = \"always\"\n", FILE).unwrap_err();
        assert_eq!(
            error.to_string(),
            format!("{FILE}: `audio.talk_mode`: must be \"push_to_talk\" or \"open\"")
        );
        // A misspelt key is still an unknown key.
        assert_eq!(
            Config::parse("[audio]\ntalkmode = \"open\"\n", FILE),
            Err(Error::UnknownConfigKey {
                file: FILE.to_owned(),
                key: "audio.talkmode".to_owned()
            })
        );

        let layers = [
            (
                ConfigOverrides {
                    open_threshold_db: Some(-81),
                    ..ConfigOverrides::default()
                },
                "audio.open_threshold_db",
            ),
            (
                ConfigOverrides {
                    open_threshold_db: Some(i32::MAX),
                    ..ConfigOverrides::default()
                },
                "audio.open_threshold_db",
            ),
            (
                ConfigOverrides {
                    open_hang_ms: Some(0),
                    ..ConfigOverrides::default()
                },
                "audio.open_hang_ms",
            ),
            (
                ConfigOverrides {
                    open_hang_ms: Some(u32::MAX),
                    ..ConfigOverrides::default()
                },
                "audio.open_hang_ms",
            ),
        ];
        for (layer, want) in layers {
            let mut config = Config::default();
            let error = config.apply(layer, SOURCE).unwrap_err();
            assert!(
                matches!(&error, Error::InvalidConfigValue { key, .. } if key == want),
                "{error:?}"
            );
            assert_eq!(config, Config::default());
        }
        let mut config = Config::default();
        config
            .apply(
                ConfigOverrides {
                    talk_mode: Some(TalkMode::Open),
                    ..ConfigOverrides::default()
                },
                SOURCE,
            )
            .unwrap();
        assert_eq!(config.audio.talk_mode, TalkMode::Open);
        assert_eq!(config.audio.open_hang_ms, DEFAULT_OPEN_HANG_MS);
    }

    /// `config set audio.talk_mode`: the mode is saved as a device is, by changing its value
    /// alone, or adding its line after the header.
    #[test]
    fn setting_the_talk_mode_changes_that_value_and_nothing_else() {
        let with_mode = "# conch-voice\n\
                         [audio]\n\
                         # how to talk\n\
                         talk_mode = \"push_to_talk\" # for now\n\
                         input = \"alsa_input.old\"\n";
        // (what, before, value, after)
        let cases = [
            (
                "the key absent: inserted after the header",
                KEPT.to_owned(),
                "open",
                KEPT.replace("[audio]\n", "[audio]\ntalk_mode = \"open\"\n"),
            ),
            (
                "the key present, its comments kept",
                with_mode.to_owned(),
                "open",
                with_mode.replace("\"push_to_talk\"", "\"open\""),
            ),
            (
                "back to push-to-talk",
                with_mode.replace("\"push_to_talk\"", "'open'"),
                "push_to_talk",
                with_mode.to_owned(),
            ),
            (
                "the same value again",
                with_mode.to_owned(),
                "push_to_talk",
                with_mode.to_owned(),
            ),
            (
                "an empty file",
                String::new(),
                "open",
                "[audio]\ntalk_mode = \"open\"\n".to_owned(),
            ),
        ];
        for (what, before, value, want) in cases {
            let got = set_audio_value(&before, FILE, AudioKey::TalkMode, value)
                .unwrap_or_else(|error| panic!("{what}: {error}"));
            assert_eq!(got, want, "{what}");
            let mut expected = Config::parse(&before, FILE).unwrap();
            expected.audio.talk_mode = TalkMode::from_name(value).unwrap();
            assert_eq!(Config::parse(&got, FILE).unwrap(), expected, "{what}");
        }

        // Only the two names: nothing else is written, and the value is not shown.
        for value in ["", "Open", "always", "open\n", "open\" # x", "push-to-talk"] {
            let error = set_audio_value(KEPT, FILE, AudioKey::TalkMode, value).unwrap_err();
            assert_eq!(
                error,
                Error::InvalidConfigValue {
                    file: FILE.to_owned(),
                    key: "audio.talk_mode".to_owned(),
                    message: TALK_MODE_MUST_BE.to_owned(),
                },
                "{value:?}"
            );
        }
        // An `[audio]` this does not edit is refused for the mode as for a device.
        assert_eq!(
            set_audio_value(
                "audio = { input = \"a\" }\n",
                FILE,
                AudioKey::TalkMode,
                "open"
            ),
            Err(Error::AudioFormNotEditable {
                file: FILE.to_owned()
            })
        );
        assert_eq!(AudioKey::TalkMode.dotted(), "audio.talk_mode");
        assert_eq!(AudioKey::TalkMode.name(), "talk_mode");
    }
}
