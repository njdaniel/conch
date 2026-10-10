//! The key device: the configured keyboard's event device, read for the talk, mute and
//! deafen keys (`docs/design/conch-voice.md` §4).
//!
//! **Read access to a keyboard's event device exposes every key pressed on it**, in every
//! application, passwords included. This module is where those bytes enter the program, so
//! it takes in as little as it can and keeps none of it:
//!
//! - What is read goes into one fixed buffer, is handed straight to the decoder of
//!   `conch-voice-control`, which discards everything but the configured keys as it reads,
//!   and the buffer is then overwritten with zeros. Nothing here copies it, formats it or
//!   logs it, and this module logs nothing at all.
//! - The only things that leave are a [`KeyEvent`] (talk, mute or deafen went down or up)
//!   and a [`DeviceState`] (the device is open, or why it is not). Neither type can hold a
//!   key code, a scan code, a time or a count of keys.
//!
//! Input: the path of the device, the bindings, and the waits between attempts to open it.
//! Output: [`Input::Key`] and [`Input::KeyDevice`] to the session loop, which owns every
//! decision; this is one more source of what happened, like standard input.
//! Owns: one thread, the open device, the read buffer and the decoder.
//!
//! What it keeps:
//!
//! - **Only an event device is read.** Before it is opened the path is resolved, and it
//!   must be a character device under `/dev/input`; after it is opened, the open file
//!   itself must be that same character device. Anything else is refused and not read, and
//!   a refusal is not tried again: it is about the configuration, not about a keyboard
//!   coming and going.
//! - **Every way a read can go wrong is the device going away:** an error, a read of
//!   nothing, and a read that leaves the decoder holding part of a record (an event device
//!   returns whole records, so a fragment is a fault). The session loop is told first, so
//!   that the gate shuts as for a release; then the decoder is reset; and only then is the
//!   device opened again, after a wait that doubles up to a bound. Nothing decoded from a
//!   faulty read is passed on.
//! - **The session loop is told once.** That the device is not there is said when it is
//!   lost or first cannot be opened, not on every attempt. A device that opens and then
//!   fails before one whole read is not announced as open again until a read has worked,
//!   so a device that never works cannot make a line per attempt.

use std::fmt;
use std::fs::{File, Metadata};
use std::io::{self, Read};
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::time::Duration;

use conch_voice_control::{INPUT_EVENT_LEN, KeyBindings, KeyDecoder, KeyEvent};
use tokio::sync::mpsc;

use crate::session::Input;

/// Where the kernel's input devices are. Only a character device under it is read.
pub const DEV_INPUT: &str = "/dev/input";

/// How many records one read can take. The kernel hands over as many whole records as fit.
const RECORDS_PER_READ: usize = 64;

/// The size of the read buffer: a whole number of records.
pub const BUFFER_LEN: usize = RECORDS_PER_READ * INPUT_EVENT_LEN;

/// What may be opened as a key device.
///
/// The binary only ever passes [`DeviceRule::EventDevice`]: it is written out where `join`,
/// `keys` and `devices` are started (`lib.rs`), and no option on the command line and no
/// key of the configuration file can choose another, because neither `cli::JoinArgs` nor
/// `conch_voice_control::Config` has anywhere to hold one. (The configuration separately
/// refuses a `keys.device` that is not written as a path under `/dev/input/`; this rule is
/// about where the path leads and what was opened.)
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DeviceRule {
    /// A character device under `/dev/input`, and nothing else.
    EventDevice,
    /// Anything that can be opened. For tests, which read named pipes and temporary files
    /// and never a keyboard. No code outside this file's own match on the rule names it.
    AnyFileForTests,
}

/// Why something is not accepted as a key device.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Refusal {
    /// The path leads somewhere that is not under `/dev/input`.
    OutsideDevInput,
    /// It is not a character device.
    NotCharacterDevice,
    /// What was opened is not what the path named a moment before.
    Changed,
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Refusal::OutsideDevInput => "it is not under /dev/input",
            Refusal::NotCharacterDevice => "it is not a character device",
            Refusal::Changed => "it changed while it was being opened",
        })
    }
}

/// What the operating system said when the device could not be opened or read: the error's
/// number and kind, with nothing of what was read. It is `Copy`, so that it can travel in
/// an [`Input`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OsError {
    code: Option<i32>,
    kind: io::ErrorKind,
}

impl OsError {
    /// The kind of error.
    #[must_use]
    pub fn kind(&self) -> io::ErrorKind {
        self.kind
    }
}

impl From<&io::Error> for OsError {
    fn from(error: &io::Error) -> Self {
        Self {
            code: error.raw_os_error(),
            kind: error.kind(),
        }
    }
}

impl fmt::Display for OsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.code {
            Some(code) => write!(f, "{}", io::Error::from_raw_os_error(code)),
            None => write!(f, "{}", self.kind),
        }
    }
}

/// Why there is no key device.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Problem {
    /// It could not be opened: not there, or not readable by this user.
    CannotOpen(OsError),
    /// It is not something this program reads keys from.
    Refused(Refusal),
    /// A read failed.
    ReadFailed(OsError),
    /// A read returned nothing: the device ended.
    Ended,
    /// A read returned part of a record.
    PartialRecord,
}

impl Problem {
    /// The name `--json` gives it.
    #[must_use]
    pub fn code(&self) -> &'static str {
        match self {
            Problem::CannotOpen(_) => "cannot_open",
            Problem::Refused(_) => "refused",
            Problem::ReadFailed(_) => "read_failed",
            Problem::Ended => "ended",
            Problem::PartialRecord => "partial_record",
        }
    }

    /// Whether the watcher goes on trying to open the device. A refusal is final.
    #[must_use]
    pub fn retried(&self) -> bool {
        !matches!(self, Problem::Refused(_))
    }
}

impl fmt::Display for Problem {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Problem::CannotOpen(error) => write!(f, "cannot open it: {error}"),
            Problem::Refused(refusal) => write!(f, "refused: {refusal}"),
            Problem::ReadFailed(error) => write!(f, "reading it failed: {error}"),
            Problem::Ended => f.write_str("it ended: a read returned nothing"),
            Problem::PartialRecord => f.write_str(
                "a read returned part of an event record, which an event device never does",
            ),
        }
    }
}

/// Whether the key device is being read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DeviceState {
    /// It is open, and what it is was checked.
    Ready,
    /// It is not being read, and why.
    Missing(Problem),
}

impl DeviceState {
    /// True while the device is being read.
    #[must_use]
    pub fn is_ready(&self) -> bool {
        matches!(self, DeviceState::Ready)
    }
}

/// The rule itself, on facts that need nothing opened: where the resolved path is, and
/// whether a character device is there.
///
/// # Errors
///
/// The [`Refusal`] for a path that is not strictly under [`DEV_INPUT`], or for something
/// that is not a character device.
pub fn permitted(canonical: &Path, is_char_device: bool) -> Result<(), Refusal> {
    let dev_input = Path::new(DEV_INPUT);
    // By components, so `/dev/inputs/x` is outside; and the directory itself is not a device.
    if !canonical.starts_with(dev_input) || canonical == dev_input {
        return Err(Refusal::OutsideDevInput);
    }
    if !is_char_device {
        return Err(Refusal::NotCharacterDevice);
    }
    Ok(())
}

/// What makes a device node the one it is: the file system it is on, its inode, and the
/// device it stands for.
fn identity(metadata: &Metadata) -> (u64, u64, u64) {
    (metadata.dev(), metadata.ino(), metadata.rdev())
}

/// Checks the file that was opened, not the path: it must be a character device, and the
/// very one the path named when the rule was applied to it.
fn confirm(named: &Metadata, opened: &Metadata) -> Result<(), Refusal> {
    if !opened.file_type().is_char_device() {
        return Err(Refusal::NotCharacterDevice);
    }
    if identity(named) != identity(opened) {
        return Err(Refusal::Changed);
    }
    Ok(())
}

/// Opens a key device read-only, with ordinary file I/O, and checks what was opened before
/// anything is read from it.
///
/// Under [`DeviceRule::EventDevice`] the path is resolved first and nothing is opened
/// unless it leads to a character device under `/dev/input`; what was then opened is
/// checked again from the open file itself, so that the thing checked is the thing read.
///
/// # Errors
///
/// [`Problem::CannotOpen`] with what the operating system said, or [`Problem::Refused`].
pub fn open(device: &Path, rule: DeviceRule) -> Result<File, Problem> {
    let cannot = |error: io::Error| Problem::CannotOpen(OsError::from(&error));
    match rule {
        DeviceRule::AnyFileForTests => File::open(device).map_err(cannot),
        DeviceRule::EventDevice => {
            let canonical = std::fs::canonicalize(device).map_err(cannot)?;
            // Of the resolved path itself: a link put there since is not followed.
            let named = std::fs::symlink_metadata(&canonical).map_err(cannot)?;
            permitted(&canonical, named.file_type().is_char_device()).map_err(Problem::Refused)?;
            let file = File::open(&canonical).map_err(cannot)?;
            let opened = file.metadata().map_err(cannot)?;
            confirm(&named, &opened).map_err(Problem::Refused)?;
            Ok(file)
        }
    }
}

/// The waits between attempts to open the device.
#[derive(Debug, Clone, Copy)]
pub struct KeyTimings {
    /// The first wait.
    pub first_wait: Duration,
    /// The longest wait: each wait is double the one before, up to this.
    pub longest_wait: Duration,
}

impl Default for KeyTimings {
    fn default() -> Self {
        Self {
            first_wait: Duration::from_millis(250),
            longest_wait: Duration::from_secs(5),
        }
    }
}

/// The read buffer and the decoder: the only two places bytes from the device ever are.
struct Reader {
    decoder: KeyDecoder,
    /// One buffer, reused for every read, and zero between reads.
    buffer: [u8; BUFFER_LEN],
}

impl Reader {
    fn new(bindings: KeyBindings) -> Self {
        Self {
            decoder: KeyDecoder::new(bindings),
            buffer: [0; BUFFER_LEN],
        }
    }

    /// One read: the bytes go to the decoder and the buffer is overwritten with zeros.
    /// What comes back is the presses and releases of the configured keys that the read
    /// held, or the fault that the read was. A faulty read yields no key.
    fn read(&mut self, source: &mut impl Read) -> Result<Vec<KeyEvent>, Problem> {
        let read = loop {
            match source.read(&mut self.buffer) {
                Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
                other => break other,
            }
        };
        let outcome = match read {
            Ok(0) => Err(Problem::Ended),
            Ok(len) => {
                let seen = self.buffer.get(..len).unwrap_or(&self.buffer);
                let events = self.decoder.feed(seen);
                if self.decoder.pending_len() == 0 {
                    Ok(events)
                } else {
                    Err(Problem::PartialRecord)
                }
            }
            Err(error) => Err(Problem::ReadFailed(OsError::from(&error))),
        };
        // The decoder has seen it; nothing of it stays here.
        self.wipe();
        outcome
    }

    /// Forgets a fragment the decoder was holding, and wipes the buffer.
    fn reset(&mut self) {
        self.decoder.reset();
        self.wipe();
    }

    fn wipe(&mut self) {
        self.buffer.fill(0);
        // The zeros are written although nothing reads them before the next read.
        std::hint::black_box(&self.buffer);
    }
}

/// The session loop has gone away, so there is nobody to read keys for.
struct LoopGone;

struct Watcher {
    device: PathBuf,
    rule: DeviceRule,
    timings: KeyTimings,
    inputs: mpsc::UnboundedSender<Input>,
    reader: Reader,
    /// What the session loop was last told: that the device is being read, or that it is
    /// not. `None` before it has been told anything.
    told: Option<bool>,
}

impl Watcher {
    /// Tells the session loop the device's state, unless that is what it was last told.
    fn tell(&mut self, state: DeviceState) -> Result<(), LoopGone> {
        if self.told == Some(state.is_ready()) {
            return Ok(());
        }
        self.told = Some(state.is_ready());
        self.inputs
            .send(Input::KeyDevice(state))
            .map_err(|_| LoopGone)
    }

    /// Reads the open device until a read goes wrong. Returns the fault, and whether any
    /// read before it worked.
    fn read_to_fault(&mut self, file: &mut File) -> Result<(Problem, bool), LoopGone> {
        let mut read_any = false;
        loop {
            match self.reader.read(file) {
                Ok(events) => {
                    read_any = true;
                    // A key is passed on only while the loop knows the device is there.
                    self.tell(DeviceState::Ready)?;
                    for event in events {
                        self.inputs.send(Input::Key(event)).map_err(|_| LoopGone)?;
                    }
                }
                Err(problem) => return Ok((problem, read_any)),
            }
        }
    }

    fn run(mut self) -> Result<(), LoopGone> {
        let mut wait = self.timings.first_wait;
        // False after a device opened and failed before one whole read: it is then said
        // to be open only once a read has worked.
        let mut announce_on_open = true;
        while !self.inputs.is_closed() {
            match open(&self.device, self.rule) {
                Ok(mut file) => {
                    if announce_on_open {
                        self.tell(DeviceState::Ready)?;
                    }
                    let (problem, read_any) = self.read_to_fault(&mut file)?;
                    // The machine first, so the gate shuts as for a release; then the
                    // decoder; and the device is closed before it is opened again.
                    self.tell(DeviceState::Missing(problem))?;
                    self.reader.reset();
                    drop(file);
                    announce_on_open = read_any;
                    if read_any {
                        wait = self.timings.first_wait;
                    }
                }
                Err(problem) => {
                    self.tell(DeviceState::Missing(problem))?;
                    if !problem.retried() {
                        return Ok(());
                    }
                }
            }
            std::thread::sleep(wait);
            wait = wait.saturating_mul(2).min(self.timings.longest_wait);
        }
        Ok(())
    }
}

/// Starts watching `device` for the keys in `bindings`, on a thread of its own, for as long
/// as the session loop takes its inputs. The reads block, so the thread is not the
/// runtime's.
///
/// `rule` is [`DeviceRule::EventDevice`] everywhere but in tests.
///
/// # Errors
///
/// What the operating system said, if the thread could not be started.
pub fn spawn(
    device: PathBuf,
    bindings: KeyBindings,
    rule: DeviceRule,
    timings: KeyTimings,
    inputs: mpsc::UnboundedSender<Input>,
) -> io::Result<()> {
    let watcher = Watcher {
        device,
        rule,
        timings,
        inputs,
        reader: Reader::new(bindings),
        told: None,
    };
    std::thread::Builder::new()
        .name("conch-voice-keys".to_owned())
        .spawn(move || {
            // It ends when the session loop does, or on a refusal; either way silently.
            let _ = watcher.run();
        })
        .map(|_| ())
}

#[cfg(test)]
mod tests {
    use conch_voice_control::{Key, KeyAction};

    use super::*;

    /// A record as the kernel writes it, with a time that is nothing like a key code.
    fn record(kind: u16, code: u16, value: i32) -> [u8; INPUT_EVENT_LEN] {
        let mut record = [0u8; INPUT_EVENT_LEN];
        record[..8].copy_from_slice(&0x1122_3344_5566_i64.to_le_bytes());
        record[8..16].copy_from_slice(&0x0a_bcde_i64.to_le_bytes());
        record[16..18].copy_from_slice(&kind.to_le_bytes());
        record[18..20].copy_from_slice(&code.to_le_bytes());
        record[20..24].copy_from_slice(&value.to_le_bytes());
        record
    }

    fn bindings() -> KeyBindings {
        KeyBindings::new(
            Some("KEY_F13".parse().unwrap()),
            Some("KEY_F14".parse().unwrap()),
            Some("KEY_F15".parse().unwrap()),
        )
        .unwrap()
    }

    const TALK: u16 = 183;
    const EV_KEY: u16 = 1;

    /// A source whose reads fail.
    struct Broken;

    impl Read for Broken {
        fn read(&mut self, _: &mut [u8]) -> io::Result<usize> {
            Err(io::Error::from_raw_os_error(19))
        }
    }

    #[test]
    fn the_rule_takes_a_character_device_under_dev_input_and_nothing_else() {
        // Paths that need no opening: the rule is about where a path leads and what is there.
        for (path, is_char_device, expected) in [
            ("/dev/input/event3", true, Ok(())),
            ("/dev/input/by-id/usb-Example-event-kbd", true, Ok(())),
            ("/dev/input/event3", false, Err(Refusal::NotCharacterDevice)),
            ("/dev/input", true, Err(Refusal::OutsideDevInput)),
            ("/dev/inputs/event3", true, Err(Refusal::OutsideDevInput)),
            ("/dev/tty", true, Err(Refusal::OutsideDevInput)),
            ("/dev/null", true, Err(Refusal::OutsideDevInput)),
            ("/tmp/dev/input/event3", true, Err(Refusal::OutsideDevInput)),
            ("/home/someone/keys", false, Err(Refusal::OutsideDevInput)),
            ("input/event3", true, Err(Refusal::OutsideDevInput)),
        ] {
            assert_eq!(
                permitted(Path::new(path), is_char_device),
                expected,
                "{path}"
            );
        }
    }

    #[test]
    fn what_was_opened_must_be_the_character_device_the_path_named() {
        // Character devices that are no keyboard: nothing is read from either.
        let null = std::fs::metadata("/dev/null").unwrap();
        let zero = std::fs::metadata("/dev/zero").unwrap();
        let opened = File::open("/dev/null").unwrap().metadata().unwrap();
        assert_eq!(confirm(&null, &opened), Ok(()));
        assert_eq!(confirm(&zero, &opened), Err(Refusal::Changed));

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("file");
        std::fs::write(&path, b"").unwrap();
        let file = File::open(&path).unwrap().metadata().unwrap();
        assert_eq!(
            confirm(&std::fs::metadata(&path).unwrap(), &file),
            Err(Refusal::NotCharacterDevice)
        );
        assert_eq!(confirm(&null, &file), Err(Refusal::NotCharacterDevice));
    }

    #[test]
    fn a_read_gives_the_configured_keys_and_leaves_the_buffer_zero() {
        let mut reader = Reader::new(bindings());
        let mut stream = Vec::new();
        stream.extend_from_slice(&record(EV_KEY, 0x2c5, 1));
        stream.extend_from_slice(&record(EV_KEY, TALK, 1));
        stream.extend_from_slice(&record(EV_KEY, 0x2c5, 0));
        let events = reader.read(&mut stream.as_slice()).unwrap();
        assert_eq!(
            events,
            [KeyEvent {
                key: Key::Talk,
                action: KeyAction::Press
            }]
        );
        assert!(
            reader.buffer.iter().all(|&byte| byte == 0),
            "the read buffer is zero after the decoder has seen it"
        );
        assert_eq!(reader.decoder.pending_len(), 0);
    }

    #[test]
    fn each_way_a_read_goes_wrong_is_a_fault_that_yields_no_key_and_leaves_nothing_behind() {
        // A read of nothing.
        let mut reader = Reader::new(bindings());
        assert_eq!(reader.read(&mut io::empty()), Err(Problem::Ended));

        // A read error.
        let failed = reader.read(&mut Broken).unwrap_err();
        assert!(matches!(failed, Problem::ReadFailed(_)), "{failed:?}");
        assert_eq!(
            failed.to_string(),
            format!("reading it failed: {}", io::Error::from_raw_os_error(19))
        );

        // A whole press of the talk key and then part of a record, in one read: the read
        // is a fault, and the press in it is not passed on.
        let mut stream = record(EV_KEY, TALK, 1).to_vec();
        stream.extend_from_slice(&record(EV_KEY, 0x2c5, 1)[..10]);
        assert_eq!(
            reader.read(&mut stream.as_slice()),
            Err(Problem::PartialRecord)
        );
        assert!(reader.buffer.iter().all(|&byte| byte == 0));
        assert_eq!(reader.decoder.pending_len(), 10, "until the reset");

        // The reset forgets the fragment, so the next record is read from its start.
        reader.reset();
        assert_eq!(reader.decoder.pending_len(), 0);
        let release = record(EV_KEY, TALK, 0);
        assert_eq!(
            reader.read(&mut release.as_slice()).unwrap(),
            [KeyEvent {
                key: Key::Talk,
                action: KeyAction::Release
            }]
        );
    }

    #[test]
    fn a_problem_says_why_in_words_that_hold_nothing_read() {
        let denied = OsError::from(&io::Error::from_raw_os_error(13));
        assert_eq!(denied.kind(), io::ErrorKind::PermissionDenied);
        for (problem, code, retried) in [
            (Problem::CannotOpen(denied), "cannot_open", true),
            (Problem::Refused(Refusal::OutsideDevInput), "refused", false),
            (
                Problem::Refused(Refusal::NotCharacterDevice),
                "refused",
                false,
            ),
            (Problem::Refused(Refusal::Changed), "refused", false),
            (Problem::ReadFailed(denied), "read_failed", true),
            (Problem::Ended, "ended", true),
            (Problem::PartialRecord, "partial_record", true),
        ] {
            assert_eq!(problem.code(), code);
            assert_eq!(problem.retried(), retried, "{problem}");
            assert!(!problem.to_string().contains('\n'));
        }
        assert_eq!(
            Problem::CannotOpen(denied).to_string(),
            format!("cannot open it: {}", io::Error::from_raw_os_error(13))
        );
        assert_eq!(
            Problem::Refused(Refusal::OutsideDevInput).to_string(),
            "refused: it is not under /dev/input"
        );
        let unnumbered = OsError::from(&io::Error::from(io::ErrorKind::UnexpectedEof));
        assert_eq!(
            unnumbered.to_string(),
            io::ErrorKind::UnexpectedEof.to_string()
        );
    }
}
