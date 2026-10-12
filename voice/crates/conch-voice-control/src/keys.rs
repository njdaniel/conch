//! Key decoding: from the bytes of a keyboard's event device to presses and releases of the
//! talk, mute and deafen keys, and nothing else.
//!
//! A keyboard's event device carries every key the user presses, in every application. This
//! module is where that stream is reduced to almost nothing, and the reduction is held by the
//! types, not by care at each call site:
//!
//! - [`KeyEvent`], the decoder's only output, names its key with [`Key`], which has three
//!   variants. There is no variant, field or type here that could carry another key's code.
//! - [`KeyCode`] holds a code the user *configured*. It is built only from the configuration
//!   or a command-line value, never from a byte that was read.
//! - [`KeyDecoder`] keeps the bindings and at most one incomplete record (23 bytes) between
//!   calls. A complete record is examined in a local and is gone when the call returns.
//!   While a fragment is pending it can hold the bytes of another key's event; it is wiped
//!   when the record completes and by [`KeyDecoder::reset`]. A keyboard's event device
//!   returns whole records, so a fragment only arises if the caller's reads are split.
//!
//! One event that is not a key is acted on: `SYN_DROPPED`, the kernel saying that it lost
//! events. A lost release of the talk key would leave the microphone open, so it is reported
//! as a release of the talk key.
//!
//! Nothing here logs, and no error or `Debug` rendering includes a byte that was read.

use std::fmt;
use std::str::FromStr;

use crate::INPUT_EVENT_LEN;
use crate::error::Error;

/// `EV_KEY` in `linux/input-event-codes.h`: the type of a key event.
const EV_KEY: u16 = 0x01;
/// `EV_SYN`: the type of a synchronisation event.
const EV_SYN: u16 = 0x00;
/// `SYN_DROPPED`: the kernel's buffer for this reader overflowed and events were lost.
const SYN_DROPPED: u16 = 3;
/// The `value` of a key event for a press.
const VALUE_PRESS: i32 = 1;
/// The `value` of a key event for a release.
const VALUE_RELEASE: i32 = 0;
// A value of 2 is an auto-repeat. It and every other value match neither constant and are
// ignored.

/// The largest key code (`KEY_MAX` in `linux/input-event-codes.h`). Code 0 is `KEY_RESERVED`
/// and is not a key.
pub const KEY_CODE_MAX: u16 = 0x2ff;

/// Names from `linux/input-event-codes.h` for the keys someone is likely to talk, mute or
/// deafen with: modifiers, function keys, navigation and the keypad. Any other key can be
/// configured by its number.
const KEY_NAMES: &[(&str, u16)] = &[
    ("KEY_ESC", 1),
    ("KEY_BACKSPACE", 14),
    ("KEY_TAB", 15),
    ("KEY_ENTER", 28),
    ("KEY_LEFTCTRL", 29),
    ("KEY_GRAVE", 41),
    ("KEY_LEFTSHIFT", 42),
    ("KEY_RIGHTSHIFT", 54),
    ("KEY_KPASTERISK", 55),
    ("KEY_LEFTALT", 56),
    ("KEY_SPACE", 57),
    ("KEY_CAPSLOCK", 58),
    ("KEY_F1", 59),
    ("KEY_F2", 60),
    ("KEY_F3", 61),
    ("KEY_F4", 62),
    ("KEY_F5", 63),
    ("KEY_F6", 64),
    ("KEY_F7", 65),
    ("KEY_F8", 66),
    ("KEY_F9", 67),
    ("KEY_F10", 68),
    ("KEY_NUMLOCK", 69),
    ("KEY_SCROLLLOCK", 70),
    ("KEY_KP7", 71),
    ("KEY_KP8", 72),
    ("KEY_KP9", 73),
    ("KEY_KPMINUS", 74),
    ("KEY_KP4", 75),
    ("KEY_KP5", 76),
    ("KEY_KP6", 77),
    ("KEY_KPPLUS", 78),
    ("KEY_KP1", 79),
    ("KEY_KP2", 80),
    ("KEY_KP3", 81),
    ("KEY_KP0", 82),
    ("KEY_KPDOT", 83),
    ("KEY_102ND", 86),
    ("KEY_F11", 87),
    ("KEY_F12", 88),
    ("KEY_KPENTER", 96),
    ("KEY_RIGHTCTRL", 97),
    ("KEY_KPSLASH", 98),
    ("KEY_SYSRQ", 99),
    ("KEY_RIGHTALT", 100),
    ("KEY_HOME", 102),
    ("KEY_UP", 103),
    ("KEY_PAGEUP", 104),
    ("KEY_LEFT", 105),
    ("KEY_RIGHT", 106),
    ("KEY_END", 107),
    ("KEY_DOWN", 108),
    ("KEY_PAGEDOWN", 109),
    ("KEY_INSERT", 110),
    ("KEY_DELETE", 111),
    ("KEY_MUTE", 113),
    ("KEY_PAUSE", 119),
    ("KEY_LEFTMETA", 125),
    ("KEY_RIGHTMETA", 126),
    ("KEY_COMPOSE", 127),
    ("KEY_MENU", 139),
    ("KEY_F13", 183),
    ("KEY_F14", 184),
    ("KEY_F15", 185),
    ("KEY_F16", 186),
    ("KEY_F17", 187),
    ("KEY_F18", 188),
    ("KEY_F19", 189),
    ("KEY_F20", 190),
    ("KEY_F21", 191),
    ("KEY_F22", 192),
    ("KEY_F23", 193),
    ("KEY_F24", 194),
    ("KEY_MICMUTE", 248),
];

/// The code of a key the user configured for talk, mute or deafen.
///
/// It comes from a name in this crate's table (`KEY_RIGHTCTRL`) or from a decimal number
/// (`97`), through [`FromStr`]. There is no way to build one from an event that was read.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct KeyCode(u16);

impl KeyCode {
    /// The code as the kernel numbers it.
    #[must_use]
    pub fn code(self) -> u16 {
        self.0
    }

    /// A code given as a number, if it is in the range of key codes.
    pub(crate) fn from_number(number: u64) -> Option<Self> {
        u16::try_from(number)
            .ok()
            .filter(|code| (1..=KEY_CODE_MAX).contains(code))
            .map(Self)
    }
}

impl FromStr for KeyCode {
    type Err = Error;

    /// A key name from the table, in either case, or a decimal key code.
    fn from_str(s: &str) -> Result<Self, Error> {
        let unknown = || Error::UnknownKeyName { name: s.to_owned() };
        if !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit()) {
            let number = s.parse::<u64>().map_err(|_| unknown())?;
            return Self::from_number(number).ok_or_else(unknown);
        }
        KEY_NAMES
            .iter()
            .find(|(name, _)| name.eq_ignore_ascii_case(s))
            .map(|&(_, code)| Self(code))
            .ok_or_else(unknown)
    }
}

/// The three things a key can be bound to. This is every key the decoder can report.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Key {
    /// Hold to transmit.
    Talk,
    /// Press to mute or unmute.
    Mute,
    /// Press to deafen or undeafen.
    Deafen,
}

impl fmt::Display for Key {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Key::Talk => "talk",
            Key::Mute => "mute",
            Key::Deafen => "deafen",
        })
    }
}

/// What happened to a key. An auto-repeat is neither and is never reported.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum KeyAction {
    /// The key went down.
    Press,
    /// The key came up.
    Release,
}

/// A press or release of one of the configured keys: the only thing the decoder yields.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct KeyEvent {
    /// Which of the configured keys.
    pub key: Key,
    /// Whether it went down or came up.
    pub action: KeyAction,
}

/// The configured keys. Any of the three may be unbound; with none bound the decoder yields
/// nothing at all.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct KeyBindings {
    talk: Option<KeyCode>,
    mute: Option<KeyCode>,
    deafen: Option<KeyCode>,
}

impl KeyBindings {
    /// Binds the keys.
    ///
    /// # Errors
    ///
    /// [`Error::DuplicateKeyBinding`] if two of them are the same key: one press cannot mean
    /// two things.
    pub fn new(
        talk: Option<KeyCode>,
        mute: Option<KeyCode>,
        deafen: Option<KeyCode>,
    ) -> Result<Self, Error> {
        let pairs = [
            (Key::Talk, talk, Key::Mute, mute),
            (Key::Talk, talk, Key::Deafen, deafen),
            (Key::Mute, mute, Key::Deafen, deafen),
        ];
        for (first, a, second, b) in pairs {
            if a.is_some() && a == b {
                return Err(Error::DuplicateKeyBinding { first, second });
            }
        }
        Ok(Self { talk, mute, deafen })
    }

    /// The key bound to talk, if any.
    #[must_use]
    pub fn talk(&self) -> Option<KeyCode> {
        self.talk
    }

    /// The key bound to mute, if any.
    #[must_use]
    pub fn mute(&self) -> Option<KeyCode> {
        self.mute
    }

    /// The key bound to deafen, if any.
    #[must_use]
    pub fn deafen(&self) -> Option<KeyCode> {
        self.deafen
    }

    /// True if no key is bound, so there is no reason to open a key device.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.talk.is_none() && self.mute.is_none() && self.deafen.is_none()
    }

    /// Which binding a code read from the device belongs to. The code goes no further than
    /// this comparison.
    fn lookup(&self, code: u16) -> Option<Key> {
        let bound = |binding: Option<KeyCode>| binding.is_some_and(|key| key.0 == code);
        if bound(self.talk) {
            Some(Key::Talk)
        } else if bound(self.mute) {
            Some(Key::Mute)
        } else if bound(self.deafen) {
            Some(Key::Deafen)
        } else {
            None
        }
    }

    /// Reduces one whole record to a press or release of a configured key, or to nothing.
    ///
    /// The record is 16 bytes of time (not read), then a `u16` type, a `u16` code and an
    /// `i32` value, little-endian.
    fn decode(&self, record: &[u8; INPUT_EVENT_LEN]) -> Option<KeyEvent> {
        let kind = u16::from_le_bytes([record[16], record[17]]);
        let code = u16::from_le_bytes([record[18], record[19]]);
        if kind == EV_SYN && code == SYN_DROPPED {
            // Events were lost, and one of them may have been the release of the talk key.
            // The state of the keys is unknown, so fail closed: report the talk key
            // released. A press lost the same way costs the user a second press; a lost
            // release would otherwise leave the microphone open until the transmit limit.
            return self.talk.map(|_| KeyEvent {
                key: Key::Talk,
                action: KeyAction::Release,
            });
        }
        if kind != EV_KEY {
            return None;
        }
        let key = self.lookup(code)?;
        let action = match i32::from_le_bytes([record[20], record[21], record[22], record[23]]) {
            VALUE_PRESS => KeyAction::Press,
            VALUE_RELEASE => KeyAction::Release,
            _ => return None,
        };
        Some(KeyEvent { key, action })
    }
}

/// Decodes the byte stream of a keyboard's event device.
///
/// The caller reads the device and hands the bytes over as they arrive, in any sizes: a
/// record split across two reads decodes the same as one read whole. Between calls the
/// decoder holds the bindings and the bytes of at most one incomplete record. It holds no
/// event, no history, and no key code other than the bindings.
pub struct KeyDecoder {
    bindings: KeyBindings,
    /// The start of a record whose end has not been read yet: the first `partial_len` bytes.
    /// Wiped as soon as the record is complete.
    partial: [u8; INPUT_EVENT_LEN],
    partial_len: usize,
}

impl KeyDecoder {
    /// A decoder at a record boundary.
    #[must_use]
    pub fn new(bindings: KeyBindings) -> Self {
        Self {
            bindings,
            partial: [0; INPUT_EVENT_LEN],
            partial_len: 0,
        }
    }

    /// Takes the next bytes read from the device and returns the presses and releases of
    /// configured keys that they complete, in order. Everything else in them is discarded
    /// here. A trailing incomplete record is kept for the next call.
    pub fn feed(&mut self, mut bytes: &[u8]) -> Vec<KeyEvent> {
        let mut events = Vec::new();

        if self.partial_len > 0 {
            let take = (INPUT_EVENT_LEN - self.partial_len).min(bytes.len());
            let (head, rest) = bytes.split_at(take);
            self.partial[self.partial_len..self.partial_len + take].copy_from_slice(head);
            self.partial_len += take;
            bytes = rest;
            if self.partial_len < INPUT_EVENT_LEN {
                return events;
            }
            let record = self.partial;
            self.discard_partial();
            events.extend(self.bindings.decode(&record));
        }

        let mut records = bytes.chunks_exact(INPUT_EVENT_LEN);
        for record in &mut records {
            if let Ok(record) = <&[u8; INPUT_EVENT_LEN]>::try_from(record) {
                events.extend(self.bindings.decode(record));
            }
        }

        let rest = records.remainder();
        self.partial[..rest.len()].copy_from_slice(rest);
        self.partial_len = rest.len();
        events
    }

    /// How many bytes of an incomplete record are being kept: 0 to 23.
    #[must_use]
    pub fn pending_len(&self) -> usize {
        self.partial_len
    }

    /// Forgets an incomplete record. Call it when the device is closed or reopened: a newly
    /// opened device starts at a record boundary.
    ///
    /// It reports nothing. A release that happened while the device was closed is never
    /// read, so the caller must also tell the push-to-talk machine that the device went
    /// away (`PttInput::KeyDevice(false)`), which ends whatever was held on it.
    pub fn reset(&mut self) {
        self.discard_partial();
    }

    fn discard_partial(&mut self) {
        self.partial = [0; INPUT_EVENT_LEN];
        self.partial_len = 0;
    }
}

/// Shows the bindings and how many bytes are pending, never the bytes themselves.
impl fmt::Debug for KeyDecoder {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("KeyDecoder")
            .field("bindings", &self.bindings)
            .field("pending_len", &self.partial_len)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TALK: Key = Key::Talk;
    const MUTE: Key = Key::Mute;
    const DEAFEN: Key = Key::Deafen;
    const PRESS: KeyAction = KeyAction::Press;
    const RELEASE: KeyAction = KeyAction::Release;

    fn event(key: Key, action: KeyAction) -> KeyEvent {
        KeyEvent { key, action }
    }

    fn code(name: &str) -> KeyCode {
        name.parse().unwrap()
    }

    /// The bindings the fixture was written for.
    fn bindings() -> KeyBindings {
        KeyBindings::new(
            Some(code("KEY_RIGHTCTRL")),
            Some(code("KEY_F13")),
            Some(code("KEY_F14")),
        )
        .unwrap()
    }

    /// Builds one record by hand: `sec` and `usec` fill the 16 bytes of time.
    fn record(sec: u64, usec: u64, kind: u16, code: u16, value: i32) -> [u8; INPUT_EVENT_LEN] {
        let mut bytes = [0u8; INPUT_EVENT_LEN];
        bytes[0..8].copy_from_slice(&sec.to_le_bytes());
        bytes[8..16].copy_from_slice(&usec.to_le_bytes());
        bytes[16..18].copy_from_slice(&kind.to_le_bytes());
        bytes[18..20].copy_from_slice(&code.to_le_bytes());
        bytes[20..24].copy_from_slice(&value.to_le_bytes());
        bytes
    }

    /// Reads `testdata/key-events.hex`: hex bytes, with `#` starting a comment.
    fn fixture() -> Vec<u8> {
        let text = include_str!("../testdata/key-events.hex");
        let digits: Vec<u8> = text
            .lines()
            .flat_map(|line| line.split('#').next().unwrap_or("").bytes())
            .filter(|b| !b.is_ascii_whitespace())
            .collect();
        assert_eq!(
            digits.len() % 2,
            0,
            "the fixture has an odd number of hex digits"
        );
        digits
            .chunks(2)
            .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
            .collect()
    }

    /// What the fixture holds for the bindings above. See the comments in the file.
    fn fixture_events() -> Vec<KeyEvent> {
        vec![
            event(TALK, PRESS),
            event(MUTE, PRESS),
            event(MUTE, RELEASE),
            event(DEAFEN, PRESS),
            event(DEAFEN, RELEASE),
            event(TALK, RELEASE),
            // Record 38 is a SYN_DROPPED: the talk key is reported released again.
            event(TALK, RELEASE),
        ]
    }

    #[test]
    fn the_fixture_is_whole_records_and_mostly_other_keys_and_event_types() {
        let bytes = fixture();
        assert_eq!(bytes.len() % INPUT_EVENT_LEN, 0);
        let records = bytes.len() / INPUT_EVENT_LEN;
        // Discarding is only exercised if most of the fixture is not ours.
        assert!(records >= 4 * fixture_events().len(), "{records} records");
        let kinds: std::collections::BTreeSet<u16> = bytes
            .chunks(INPUT_EVENT_LEN)
            .map(|r| u16::from_le_bytes([r[16], r[17]]))
            .collect();
        assert!(kinds.len() >= 4, "event types in the fixture: {kinds:?}");
    }

    #[test]
    fn only_presses_and_releases_of_the_configured_keys_come_out_of_the_fixture() {
        let mut decoder = KeyDecoder::new(bindings());
        assert_eq!(decoder.feed(&fixture()), fixture_events());
        assert_eq!(decoder.pending_len(), 0);
    }

    #[test]
    fn the_fixture_decodes_the_same_split_at_every_byte_boundary() {
        let bytes = fixture();
        for split in 0..=bytes.len() {
            let mut decoder = KeyDecoder::new(bindings());
            let mut events = decoder.feed(&bytes[..split]);
            assert_eq!(
                decoder.pending_len(),
                split % INPUT_EVENT_LEN,
                "split at {split}"
            );
            events.extend(decoder.feed(&bytes[split..]));
            assert_eq!(events, fixture_events(), "split at {split}");
            assert_eq!(decoder.pending_len(), 0, "split at {split}");
        }
    }

    #[test]
    fn the_fixture_decodes_the_same_in_reads_of_every_size() {
        let bytes = fixture();
        for size in 1..=2 * INPUT_EVENT_LEN + 1 {
            let mut decoder = KeyDecoder::new(bindings());
            let events: Vec<KeyEvent> = bytes
                .chunks(size)
                .flat_map(|chunk| decoder.feed(chunk))
                .collect();
            assert_eq!(events, fixture_events(), "reads of {size} bytes");
        }
    }

    #[test]
    fn a_trailing_fragment_is_kept_for_the_next_read_not_dropped_or_misread() {
        let press = record(1, 0, EV_KEY, 97, 1);
        for kept in 1..INPUT_EVENT_LEN {
            let mut decoder = KeyDecoder::new(bindings());
            let mut bytes = fixture();
            bytes.extend_from_slice(&press[..kept]);
            // The fragment yields nothing yet, and nothing before it is disturbed.
            assert_eq!(decoder.feed(&bytes), fixture_events(), "{kept} bytes kept");
            assert_eq!(decoder.pending_len(), kept);
            // Its remainder completes it.
            assert_eq!(decoder.feed(&press[kept..]), [event(TALK, PRESS)]);
            assert_eq!(decoder.pending_len(), 0);
        }
    }

    #[test]
    fn an_empty_read_changes_nothing() {
        let mut decoder = KeyDecoder::new(bindings());
        let press = record(1, 0, EV_KEY, 97, 1);
        assert_eq!(decoder.feed(&[]), []);
        assert_eq!(decoder.feed(&press[..5]), []);
        assert_eq!(decoder.feed(&[]), []);
        assert_eq!(decoder.pending_len(), 5);
        assert_eq!(decoder.feed(&press[5..]), [event(TALK, PRESS)]);
    }

    #[test]
    fn each_kind_of_record_yields_what_the_format_says() {
        let cases: &[(&str, [u8; INPUT_EVENT_LEN], Option<KeyEvent>)] = &[
            (
                "talk press",
                record(1, 2, 1, 97, 1),
                Some(event(TALK, PRESS)),
            ),
            (
                "talk release",
                record(1, 2, 1, 97, 0),
                Some(event(TALK, RELEASE)),
            ),
            ("talk auto-repeat", record(1, 2, 1, 97, 2), None),
            (
                "talk with a value that is none of the three",
                record(1, 2, 1, 97, 3),
                None,
            ),
            ("talk with a negative value", record(1, 2, 1, 97, -1), None),
            (
                "mute press",
                record(1, 2, 1, 183, 1),
                Some(event(MUTE, PRESS)),
            ),
            (
                "deafen release",
                record(1, 2, 1, 184, 0),
                Some(event(DEAFEN, RELEASE)),
            ),
            ("another key pressed", record(1, 2, 1, 30, 1), None),
            ("another key released", record(1, 2, 1, 30, 0), None),
            (
                "the talk code byte-swapped",
                record(1, 2, 1, 97 << 8, 1),
                None,
            ),
            ("a sync report", record(1, 2, 0, 0, 0), None),
            (
                "a scan code whose value is the talk code",
                record(1, 2, 4, 4, 97),
                None,
            ),
            (
                "a relative axis numbered like the talk key",
                record(1, 2, 2, 97, 1),
                None,
            ),
            (
                "an LED numbered like the talk key",
                record(1, 2, 0x11, 97, 1),
                None,
            ),
            (
                "the key type byte-swapped",
                record(1, 2, 0x0100, 97, 1),
                None,
            ),
            (
                "a time that reads like a talk press",
                record(0x0001_0061_0001_0001, 0x0001_0061_0001_0001, 0, 0, 0),
                None,
            ),
        ];
        for (name, bytes, want) in cases {
            let mut decoder = KeyDecoder::new(bindings());
            let got = decoder.feed(bytes);
            assert_eq!(got, want.iter().copied().collect::<Vec<_>>(), "{name}");
        }
    }

    #[test]
    fn an_unbound_key_is_never_reported() {
        let only_talk = KeyBindings::new(Some(code("KEY_RIGHTCTRL")), None, None).unwrap();
        let mut decoder = KeyDecoder::new(only_talk);
        assert_eq!(
            decoder.feed(&fixture()),
            // The second release is the fixture's SYN_DROPPED.
            [
                event(TALK, PRESS),
                event(TALK, RELEASE),
                event(TALK, RELEASE)
            ]
        );

        let mut decoder = KeyDecoder::new(KeyBindings::default());
        assert!(KeyBindings::default().is_empty());
        assert_eq!(decoder.feed(&fixture()), []);
    }

    #[test]
    fn reset_forgets_an_incomplete_record() {
        let mut decoder = KeyDecoder::new(bindings());
        let other = record(1, 0, EV_KEY, 30, 1);
        assert_eq!(decoder.feed(&other[..20]), []);
        decoder.reset();
        assert_eq!(decoder.pending_len(), 0);
        assert_eq!(
            decoder.partial, [0; INPUT_EVENT_LEN],
            "the bytes are wiped, not just their count"
        );
        // Without the reset these 24 bytes would be read 20 bytes out of step.
        assert_eq!(
            decoder.feed(&record(2, 0, EV_KEY, 97, 1)),
            [event(TALK, PRESS)]
        );
    }

    #[test]
    fn a_completed_record_leaves_no_bytes_behind() {
        let mut decoder = KeyDecoder::new(bindings());
        let other = record(u64::MAX, u64::MAX, EV_KEY, 30, 1);
        decoder.feed(&other[..23]);
        assert_ne!(decoder.partial, [0; INPUT_EVENT_LEN]);
        decoder.feed(&other[23..]);
        assert_eq!(decoder.partial, [0; INPUT_EVENT_LEN]);
        assert_eq!(decoder.pending_len(), 0);
    }

    #[test]
    fn debug_shows_no_pending_bytes() {
        let mut decoder = KeyDecoder::new(bindings());
        // 0xAB is 171 in decimal, which is how Debug would print it.
        decoder.feed(&[0xAB; 23]);
        let shown = format!("{decoder:?}");
        assert!(shown.contains("pending_len: 23"), "{shown}");
        assert!(!shown.contains("171"), "{shown}");
    }

    #[test]
    fn key_names_map_to_the_kernels_codes() {
        let cases = [
            ("KEY_RIGHTCTRL", 97),
            ("KEY_LEFTCTRL", 29),
            ("KEY_LEFTSHIFT", 42),
            ("KEY_RIGHTSHIFT", 54),
            ("KEY_LEFTALT", 56),
            ("KEY_RIGHTALT", 100),
            ("KEY_LEFTMETA", 125),
            ("KEY_RIGHTMETA", 126),
            ("KEY_CAPSLOCK", 58),
            ("KEY_SCROLLLOCK", 70),
            ("KEY_PAUSE", 119),
            ("KEY_F1", 59),
            ("KEY_F10", 68),
            ("KEY_F11", 87),
            ("KEY_F12", 88),
            ("KEY_F13", 183),
            ("KEY_F24", 194),
            ("KEY_HOME", 102),
            ("KEY_END", 107),
            ("KEY_PAGEUP", 104),
            ("KEY_PAGEDOWN", 109),
            ("KEY_INSERT", 110),
            ("KEY_DELETE", 111),
            ("KEY_UP", 103),
            ("KEY_DOWN", 108),
            ("KEY_LEFT", 105),
            ("KEY_RIGHT", 106),
            ("KEY_MICMUTE", 248),
            ("key_rightctrl", 97),
            ("97", 97),
            ("1", 1),
            ("767", 767),
            ("0097", 97),
        ];
        for (name, want) in cases {
            assert_eq!(
                name.parse::<KeyCode>().map(KeyCode::code),
                Ok(want),
                "{name}"
            );
        }
    }

    #[test]
    fn an_unknown_key_name_or_number_is_an_error_naming_it() {
        let cases = [
            "KEY_BOGUS",
            "RIGHTCTRL",
            "",
            " KEY_F1",
            "0",
            "768",
            "65536",
            "99999999999999999999999",
            "-1",
            "+97",
            "0x61",
            "97.0",
        ];
        for name in cases {
            let error = name.parse::<KeyCode>().unwrap_err();
            assert_eq!(
                error,
                Error::UnknownKeyName {
                    name: name.to_owned()
                },
                "{name:?}"
            );
            assert!(error.to_string().contains(&format!("`{name}`")), "{error}");
        }
    }

    #[test]
    fn the_name_table_is_well_formed() {
        let mut names = std::collections::BTreeSet::new();
        let mut codes = std::collections::BTreeSet::new();
        for &(name, code) in KEY_NAMES {
            assert!(name.starts_with("KEY_"), "{name}");
            assert_eq!(name, name.to_ascii_uppercase(), "{name}");
            assert!((1..=KEY_CODE_MAX).contains(&code), "{name}");
            assert!(names.insert(name), "{name} is listed twice");
            assert!(codes.insert(code), "{name} repeats code {code}");
        }
    }

    #[test]
    fn two_bindings_to_one_key_are_refused() {
        let (a, b) = (Some(code("KEY_F13")), Some(code("KEY_F14")));
        let duplicate = |first, second| Err(Error::DuplicateKeyBinding { first, second });
        assert_eq!(KeyBindings::new(a, a, b), duplicate(TALK, MUTE));
        assert_eq!(KeyBindings::new(a, b, a), duplicate(TALK, DEAFEN));
        assert_eq!(KeyBindings::new(b, a, a), duplicate(MUTE, DEAFEN));
        assert_eq!(
            KeyBindings::new(a, a, b).unwrap_err().to_string(),
            "the talk and mute keys are bound to the same key"
        );
        // Unbound twice is not a duplicate.
        assert!(KeyBindings::new(a, None, None).is_ok());
        assert!(KeyBindings::new(None, None, None).is_ok());
    }

    /// The kernel lost events: whatever was held, the talk key is reported released.
    #[test]
    fn lost_events_are_a_release_of_the_talk_key() {
        const EV_SYN: u16 = 0;
        const SYN_REPORT: u16 = 0;
        const SYN_DROPPED: u16 = 3;
        let mut decoder = KeyDecoder::new(bindings());
        let talk = code("KEY_RIGHTCTRL").code();

        let mut bytes = Vec::new();
        bytes.extend(record(1, 0, EV_KEY, talk, 1));
        bytes.extend(record(1, 0, EV_SYN, SYN_REPORT, 0));
        bytes.extend(record(2, 0, EV_SYN, SYN_DROPPED, 0));
        assert_eq!(
            decoder.feed(&bytes),
            vec![event(TALK, PRESS), event(TALK, RELEASE)],
            "an ordinary sync report yields nothing; a dropped one releases the talk key"
        );

        // Only a synchronisation event with that code means lost events: a key whose code
        // happens to be 3 does not release anything.
        let mut decoder = KeyDecoder::new(bindings());
        assert_eq!(decoder.feed(&record(3, 0, EV_KEY, SYN_DROPPED, 1)), []);
        assert_eq!(decoder.feed(&record(3, 0, EV_KEY, SYN_DROPPED, 0)), []);

        // A code is compared whole: one that matches the talk key in its low byte only is
        // another key.
        assert_eq!(decoder.feed(&record(4, 0, EV_KEY, talk + 0x100, 1)), []);

        // With no talk key bound there is nothing to release.
        let unbound = KeyBindings::new(None, Some(code("KEY_F13")), None).unwrap();
        let mut decoder = KeyDecoder::new(unbound);
        assert!(
            decoder
                .feed(&record(2, 0, EV_SYN, SYN_DROPPED, 0))
                .is_empty()
        );
    }

    #[test]
    fn bindings_report_what_was_bound() {
        let bound = bindings();
        assert_eq!(bound.talk().map(KeyCode::code), Some(97));
        assert_eq!(bound.mute().map(KeyCode::code), Some(183));
        assert_eq!(bound.deafen().map(KeyCode::code), Some(184));
        assert!(!bound.is_empty());
    }
}
