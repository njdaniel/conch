//! `conch-voice keys <device>`: shows the code of every key pressed on a keyboard, to find
//! the code of the key to talk with (`docs/design/conch-voice.md` §8).
//!
//! **This is the one place in the program that looks at a key it was not configured for,
//! and it is deliberate.** Everywhere else only talk, mute and deafen leave the decoder.
//! Here the user has asked to see key codes, so:
//!
//! - It says what it is about to do, and why, **before it opens anything**.
//! - It writes only to a terminal. If standard output is anything else it refuses, so that
//!   a redirection cannot put someone's keystrokes in a file by accident.
//! - A code is written to that terminal and nowhere else: nothing here logs, and nothing is
//!   kept. The read buffer is the same size as the watcher's, is reused for every read and
//!   is overwritten with zeros after each one.
//! - It opens the device through the watcher's own check (`keydev::open`): a character
//!   device under `/dev/input`, or nothing.
//!
//! Input: the device's path. Output: the warning, then one line per key press, to the
//! terminal. Owns: the open device, until Ctrl-C.

use std::io::{self, Read, Write};
use std::path::Path;

use conch_voice_control::{INPUT_EVENT_LEN, KeyCode};
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;

use crate::error::Error;
use crate::keydev::{self, BUFFER_LEN, DeviceRule, OsError, Problem};
use crate::secrets::one_line;

/// `EV_KEY` in `linux/input-event-codes.h`: the type of a key event.
const EV_KEY: u16 = 0x01;
/// The `value` of a key event for a press. Releases and auto-repeats are not shown.
const VALUE_PRESS: i32 = 1;

/// The key names `conch-voice-control` accepts in the configuration, which are the ones
/// worth showing beside a code. Only the names are here: each one's code comes from that
/// crate's own table, through `str::parse`, so the two cannot disagree. A key with no name
/// is configured by its number.
const NAMES: &[&str] = &[
    "KEY_ESC",
    "KEY_BACKSPACE",
    "KEY_TAB",
    "KEY_ENTER",
    "KEY_LEFTCTRL",
    "KEY_GRAVE",
    "KEY_LEFTSHIFT",
    "KEY_RIGHTSHIFT",
    "KEY_KPASTERISK",
    "KEY_LEFTALT",
    "KEY_SPACE",
    "KEY_CAPSLOCK",
    "KEY_F1",
    "KEY_F2",
    "KEY_F3",
    "KEY_F4",
    "KEY_F5",
    "KEY_F6",
    "KEY_F7",
    "KEY_F8",
    "KEY_F9",
    "KEY_F10",
    "KEY_NUMLOCK",
    "KEY_SCROLLLOCK",
    "KEY_KP7",
    "KEY_KP8",
    "KEY_KP9",
    "KEY_KPMINUS",
    "KEY_KP4",
    "KEY_KP5",
    "KEY_KP6",
    "KEY_KPPLUS",
    "KEY_KP1",
    "KEY_KP2",
    "KEY_KP3",
    "KEY_KP0",
    "KEY_KPDOT",
    "KEY_102ND",
    "KEY_F11",
    "KEY_F12",
    "KEY_KPENTER",
    "KEY_RIGHTCTRL",
    "KEY_KPSLASH",
    "KEY_SYSRQ",
    "KEY_RIGHTALT",
    "KEY_HOME",
    "KEY_UP",
    "KEY_PAGEUP",
    "KEY_LEFT",
    "KEY_RIGHT",
    "KEY_END",
    "KEY_DOWN",
    "KEY_PAGEDOWN",
    "KEY_INSERT",
    "KEY_DELETE",
    "KEY_MUTE",
    "KEY_PAUSE",
    "KEY_LEFTMETA",
    "KEY_RIGHTMETA",
    "KEY_COMPOSE",
    "KEY_MENU",
    "KEY_F13",
    "KEY_F14",
    "KEY_F15",
    "KEY_F16",
    "KEY_F17",
    "KEY_F18",
    "KEY_F19",
    "KEY_F20",
    "KEY_F21",
    "KEY_F22",
    "KEY_F23",
    "KEY_F24",
    "KEY_MICMUTE",
];

/// The name the configuration takes for a key code, if it has one.
#[must_use]
pub fn name_of(code: u16) -> Option<&'static str> {
    NAMES.iter().copied().find(|name| {
        name.parse::<KeyCode>()
            .is_ok_and(|named| named.code() == code)
    })
}

/// What is said before anything is opened.
#[must_use]
pub fn warning(device: &str) -> String {
    format!(
        "conch-voice keys will show the code of EVERY key pressed on\n\
         \x20   {device}\n\
         in every application, until you stop it with Ctrl-C.\n\
         It is for finding the code of the key you want to talk with, to put in the\n\
         configuration file as `talk` under `[keys]`.\n\
         Do not type a password or anything private while it runs.\n\
         Codes are written to this terminal and nowhere else: nothing is logged or kept.\n"
    )
}

/// The code of the key a record says was pressed, if it says that. The record is 16 bytes
/// of time (not read), then a `u16` type, a `u16` code and an `i32` value, little-endian.
fn pressed(record: &[u8]) -> Option<u16> {
    let kind = u16::from_le_bytes([*record.get(16)?, *record.get(17)?]);
    let code = u16::from_le_bytes([*record.get(18)?, *record.get(19)?]);
    let value = i32::from_le_bytes([
        *record.get(20)?,
        *record.get(21)?,
        *record.get(22)?,
        *record.get(23)?,
    ]);
    (kind == EV_KEY && value == VALUE_PRESS).then_some(code)
}

/// The line for one key press.
fn line(code: u16) -> String {
    match name_of(code) {
        Some(name) => format!("key {code}: {name}   (talk = \"{name}\")"),
        None => format!("key {code}   (talk = {code})"),
    }
}

/// Reads one buffer's worth and writes a line for each key press in it. The buffer is zero
/// again when this returns, whatever happened.
fn show_one_read(
    device: &mut impl Read,
    buffer: &mut [u8; BUFFER_LEN],
    out: &mut dyn Write,
) -> Result<io::Result<()>, Problem> {
    let read = loop {
        match device.read(buffer) {
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
            other => break other,
        }
    };
    let outcome = match read {
        Ok(0) => Err(Problem::Ended),
        Ok(len) if len % INPUT_EVENT_LEN != 0 => Err(Problem::PartialRecord),
        Ok(len) => Ok(buffer
            .get(..len)
            .unwrap_or_default()
            .chunks_exact(INPUT_EVENT_LEN)
            .filter_map(pressed)
            .try_for_each(|code| writeln!(out, "{}", line(code)))
            .and_then(|()| out.flush())),
        Err(error) => Err(Problem::ReadFailed(OsError::from(&error))),
    };
    buffer.fill(0);
    std::hint::black_box(&*buffer);
    outcome
}

/// `conch-voice keys`: the warning, then the device, then a line for every key press until
/// a read fails. It does not return while the device can be read; Ctrl-C ends it.
///
/// `terminal` is whether `out` is a terminal. `rule` is [`DeviceRule::EventDevice`]
/// everywhere but in tests.
///
/// # Errors
///
/// [`Error::NotATerminal`] if `out` is not a terminal, before anything is written or
/// opened; [`Error::KeyDevice`] if the device cannot be opened, is refused, or a read of it
/// fails; [`Error::Output`] if the terminal cannot be written to.
pub fn run(
    device: &Path,
    rule: DeviceRule,
    terminal: bool,
    out: &mut dyn Write,
) -> Result<(), Error> {
    if !terminal {
        return Err(Error::NotATerminal);
    }
    let shown = one_line(&device.display().to_string());
    // First, and flushed, before the device is touched.
    out.write_all(warning(&shown).as_bytes())
        .and_then(|()| out.flush())
        .map_err(Error::Output)?;

    let failed = |problem| Error::KeyDevice {
        device: shown.clone(),
        problem,
    };
    let mut file = keydev::open(device, rule).map_err(failed)?;
    writeln!(out, "\nReading. Press the key you want to talk with.")
        .and_then(|()| out.flush())
        .map_err(Error::Output)?;
    let mut buffer = [0u8; BUFFER_LEN];
    loop {
        show_one_read(&mut file, &mut buffer, out)
            .map_err(failed)?
            .map_err(Error::Output)?;
    }
}

/// [`run`] on standard output, until Ctrl-C or SIGTERM, which end it cleanly. For `main`.
///
/// # Errors
///
/// As [`run`]; and [`Error::Runtime`] if the signal handlers could not be installed.
pub fn until_interrupted(device: &Path, rule: DeviceRule, terminal: bool) -> Result<(), Error> {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(Error::Runtime)?;
    runtime.block_on(async {
        let mut interrupt = signal(SignalKind::interrupt()).map_err(Error::Runtime)?;
        let mut terminate = signal(SignalKind::terminate()).map_err(Error::Runtime)?;
        let (ended, mut end) = mpsc::unbounded_channel();
        let device = device.to_owned();
        // The reads block, so they are on a thread of their own.
        std::thread::spawn(move || {
            let _ = ended.send(run(&device, rule, terminal, &mut io::stdout()));
        });
        tokio::select! {
            _ = interrupt.recv() => Ok(()),
            _ = terminate.recv() => Ok(()),
            result = end.recv() => result.unwrap_or(Ok(())),
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn record(kind: u16, code: u16, value: i32) -> [u8; INPUT_EVENT_LEN] {
        let mut record = [0u8; INPUT_EVENT_LEN];
        record[16..18].copy_from_slice(&kind.to_le_bytes());
        record[18..20].copy_from_slice(&code.to_le_bytes());
        record[20..24].copy_from_slice(&value.to_le_bytes());
        record
    }

    #[test]
    fn every_name_shown_is_one_the_configuration_accepts() {
        for name in NAMES {
            let code = name.parse::<KeyCode>().unwrap().code();
            assert_eq!(name_of(code), Some(*name));
        }
        assert_eq!(name_of(97), Some("KEY_RIGHTCTRL"));
        assert_eq!(name_of(30), None, "a letter has no name in the table");
        assert_eq!(name_of(0), None);
    }

    #[test]
    fn only_a_press_of_a_key_is_shown() {
        assert_eq!(pressed(&record(EV_KEY, 97, 1)), Some(97));
        assert_eq!(pressed(&record(EV_KEY, 97, 0)), None, "a release");
        assert_eq!(pressed(&record(EV_KEY, 97, 2)), None, "an auto-repeat");
        assert_eq!(pressed(&record(4, 4, 1)), None, "not a key event");
        assert_eq!(pressed(&[0u8; 10]), None, "not a whole record");
        assert_eq!(
            line(97),
            "key 97: KEY_RIGHTCTRL   (talk = \"KEY_RIGHTCTRL\")"
        );
        assert_eq!(line(30), "key 30   (talk = 30)");
    }

    #[test]
    fn a_read_is_shown_and_then_the_buffer_is_zero_again() {
        let mut stream = record(EV_KEY, 97, 1).to_vec();
        stream.extend_from_slice(&record(EV_KEY, 0x2c5, 1));
        let mut buffer = [0u8; BUFFER_LEN];
        let mut shown = Vec::new();
        show_one_read(&mut stream.as_slice(), &mut buffer, &mut shown)
            .unwrap()
            .unwrap();
        assert_eq!(
            String::from_utf8(shown).unwrap(),
            "key 97: KEY_RIGHTCTRL   (talk = \"KEY_RIGHTCTRL\")\nkey 709   (talk = 709)\n"
        );
        assert!(buffer.iter().all(|&byte| byte == 0));

        // And after a read that was a fault, of which nothing is shown.
        let mut shown = Vec::new();
        let fragment = &stream[..30];
        assert_eq!(
            show_one_read(&mut &*fragment, &mut buffer, &mut shown).unwrap_err(),
            Problem::PartialRecord
        );
        assert!(shown.is_empty());
        assert!(buffer.iter().all(|&byte| byte == 0));
        assert_eq!(
            show_one_read(&mut io::empty(), &mut buffer, &mut shown).unwrap_err(),
            Problem::Ended
        );
    }

    #[test]
    fn the_warning_names_the_device_and_what_will_be_shown() {
        let warning = warning("/dev/input/by-id/usb-Example-event-kbd");
        assert!(warning.contains("EVERY key pressed"), "{warning}");
        assert!(
            warning.contains("/dev/input/by-id/usb-Example-event-kbd"),
            "{warning}"
        );
        assert!(warning.contains("Ctrl-C"), "{warning}");
        assert!(
            warning.contains("the key you want to talk with"),
            "{warning}"
        );
        assert!(warning.contains("nothing is logged or kept"), "{warning}");
    }
}
