//! The configuration file (`docs/design/conch-voice.md` §8).
//!
//! The caller reads `$XDG_CONFIG_HOME/conch/voice.toml` and hands the text to
//! [`Config::parse`]; a file that does not exist is [`Config::default`]. Values from the
//! environment and the command line are the caller's to collect, into a [`ConfigOverrides`]
//! per layer, applied with [`Config::apply`] in the order it wants them to win.

use std::fmt;
use std::ops::Range;
use std::time::Duration;

use serde::Deserialize;
use serde::de::{self, Deserializer, Visitor};
use toml::de::{DeTable, DeValue};

use crate::error::Error;
use crate::keys::{KeyBindings, KeyCode};

/// The server address when neither the file, `CONCH_SERVER` nor the command line gives one:
/// the same default as `conch`.
pub const DEFAULT_SERVER: &str = "http://127.0.0.1:8080";
/// How long the gate stays open after a release, so the last syllable is not cut.
pub const DEFAULT_RELEASE_TAIL_MS: u32 = 100;
/// The longest a single press may transmit. It catches a stuck key.
pub const DEFAULT_MAX_TRANSMIT_SECS: u32 = 120;

/// The whole configuration, with defaults filled in where the design note states one.
///
/// A field that is `None` was not set anywhere and has no fixed default: the caller decides
/// (for `server`, `CONCH_SERVER` and then [`DEFAULT_SERVER`]; for a device, PipeWire's
/// default; for a key, not bound).
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
    /// state machine applies it.
    pub max_transmit_secs: u32,
}

impl Default for AudioConfig {
    fn default() -> Self {
        Self {
            input: None,
            output: None,
            echo_cancellation: false,
            release_tail_ms: DEFAULT_RELEASE_TAIL_MS,
            max_transmit_secs: DEFAULT_MAX_TRANSMIT_SECS,
        }
    }
}

/// The longest release tail accepted, in milliseconds. The gate stays open for the tail
/// after the key is released, and nothing else bounds it.
pub const MAX_RELEASE_TAIL_MS: u32 = 2_000;
/// The longest transmit limit accepted, in seconds. Beyond this it would not be a limit.
pub const MAX_MAX_TRANSMIT_SECS: u32 = 600;

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
    /// the only thing that ends a transmission whose release was lost.
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
    }

    /// The server address, or [`DEFAULT_SERVER`] if no layer set one.
    #[must_use]
    pub fn server_or_default(&self) -> &str {
        self.server.as_deref().unwrap_or(DEFAULT_SERVER)
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
}
