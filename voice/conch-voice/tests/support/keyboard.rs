//! A stand-in for a keyboard's event device: a named pipe under a temporary directory,
//! written with `input_event` records built by hand.
//!
//! **No test reads a real keyboard, or opens or lists anything under `/dev/input`.** The
//! records here are made up, never recorded: a recording of a keyboard is someone's
//! keystrokes.

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::PathBuf;
use std::process::Command;
use std::time::{Duration, Instant};

use conch_voice::keydev::KeyTimings;
use conch_voice::session::Input;
use conch_voice_control::{INPUT_EVENT_LEN, KeyBindings};
use tokio::sync::mpsc;

/// `EV_SYN`, `EV_KEY` and `EV_MSC` in `linux/input-event-codes.h`.
pub const EV_SYN: u16 = 0x00;
pub const EV_KEY: u16 = 0x01;
pub const EV_MSC: u16 = 0x04;
/// `SYN_DROPPED`: the kernel lost events.
pub const SYN_DROPPED: u16 = 3;
/// `MSC_SCAN`: the event that carries a key's scan code.
pub const MSC_SCAN: u16 = 4;

/// The keys the tests bind: `KEY_F13`, `KEY_F14` and `KEY_F15`.
pub const TALK: u16 = 183;
pub const MUTE: u16 = 184;
pub const DEAFEN: u16 = 185;

/// The seconds and microseconds every record here carries. They are nothing like a real
/// time, so that they can be searched for in what a client wrote.
pub const SECONDS: i64 = 0x11_2233_4455;
pub const MICROSECONDS: i64 = 703_710;

pub fn bindings() -> KeyBindings {
    KeyBindings::new(
        Some("KEY_F13".parse().unwrap()),
        Some("KEY_F14".parse().unwrap()),
        Some("KEY_F15".parse().unwrap()),
    )
    .unwrap()
}

/// One `input_event` record of 64-bit Linux: 16 bytes of time, then type, code and value,
/// little-endian.
pub fn record(kind: u16, code: u16, value: i32) -> [u8; INPUT_EVENT_LEN] {
    let mut record = [0u8; INPUT_EVENT_LEN];
    record[..8].copy_from_slice(&SECONDS.to_le_bytes());
    record[8..16].copy_from_slice(&MICROSECONDS.to_le_bytes());
    record[16..18].copy_from_slice(&kind.to_le_bytes());
    record[18..20].copy_from_slice(&code.to_le_bytes());
    record[20..24].copy_from_slice(&value.to_le_bytes());
    record
}

/// A key going down, as a keyboard reports it: the key event and the report that ends it.
pub fn press(code: u16) -> Vec<u8> {
    [record(EV_KEY, code, 1), record(EV_SYN, 0, 0)].concat()
}

/// A key coming up.
pub fn release(code: u16) -> Vec<u8> {
    [record(EV_KEY, code, 0), record(EV_SYN, 0, 0)].concat()
}

/// A key struck, with its scan code and an auto-repeat, as a keyboard reports a key that
/// was held a moment.
pub fn stroke(code: u16, scan: i32) -> Vec<u8> {
    [
        record(EV_MSC, MSC_SCAN, scan),
        record(EV_KEY, code, 1),
        record(EV_SYN, 0, 0),
        record(EV_KEY, code, 2),
        record(EV_SYN, 0, 0),
        record(EV_MSC, MSC_SCAN, scan),
        record(EV_KEY, code, 0),
        record(EV_SYN, 0, 0),
    ]
    .concat()
}

/// Waits between attempts to open the device, shortened to milliseconds.
pub fn quick_keys() -> KeyTimings {
    KeyTimings {
        first_wait: Duration::from_millis(2),
        longest_wait: Duration::from_millis(10),
    }
}

/// A named pipe under a temporary directory.
pub struct Pipe {
    /// Holds the directory for as long as the pipe is in use.
    pub dir: tempfile::TempDir,
    pub path: PathBuf,
}

impl Pipe {
    /// A new pipe that nothing has open.
    pub fn new() -> Self {
        let pipe = Self::unmade();
        pipe.make();
        pipe
    }

    /// The place for a pipe, with no pipe there yet.
    pub fn unmade() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("keyboard");
        Self { dir, path }
    }

    /// Makes the pipe.
    pub fn make(&self) {
        let made = Command::new("mkfifo").arg(&self.path).status().unwrap();
        assert!(made.success(), "mkfifo failed");
    }

    /// The keyboard's end. Opening it waits until the watcher has the pipe open to read,
    /// which is at once or after its next wait.
    pub fn writer(&self) -> File {
        OpenOptions::new().write(true).open(&self.path).unwrap()
    }
}

/// How long a test waits for something that should happen at once before it fails.
const PATIENCE: Duration = Duration::from_secs(10);

/// Writes to a pipe whose reader has just closed it and is about to open it again: a
/// watcher after a fault. Until it has, a write fails whole, and is tried again.
pub fn write_when_open(keyboard: &mut File, bytes: &[u8]) {
    let deadline = Instant::now() + PATIENCE;
    loop {
        match keyboard.write_all(bytes) {
            Ok(()) => return,
            Err(error) if error.kind() == std::io::ErrorKind::BrokenPipe => {
                assert!(Instant::now() < deadline, "the watcher did not reopen");
                std::thread::sleep(Duration::from_millis(1));
            }
            Err(error) => panic!("writing to the pipe: {error}"),
        }
    }
}

/// The next thing a watcher sent, for a test that reads the watcher's channel itself.
pub fn next(inputs: &mut mpsc::UnboundedReceiver<Input>) -> Input {
    let deadline = Instant::now() + PATIENCE;
    loop {
        if let Ok(input) = inputs.try_recv() {
            return input;
        }
        assert!(
            Instant::now() < deadline,
            "timed out waiting for the watcher"
        );
        std::thread::sleep(Duration::from_millis(1));
    }
}

/// Asserts that the watcher sends nothing for `time`.
pub fn nothing_for(inputs: &mut mpsc::UnboundedReceiver<Input>, time: Duration) {
    let deadline = Instant::now() + time;
    while Instant::now() < deadline {
        if let Ok(input) = inputs.try_recv() {
            panic!("the watcher sent {input:?} when it should have sent nothing");
        }
        std::thread::sleep(Duration::from_millis(1));
    }
}
