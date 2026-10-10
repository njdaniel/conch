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
//! **That is true of what the keys are, and not of when they are pressed.** The watcher
//! does one blocking read for each packet the keyboard sends, so its thread wakes once for
//! every key event, bound or not. The kernel publishes how often a thread has woken
//! (`/proc/<pid>/task/<tid>/status`), and unless `/proc` is mounted with `hidepid` any
//! local user can read that count and so see the rhythm of the typing, though never a
//! key. The compositor's input thread shows the same, so this adds no exposure the machine
//! did not have; and not waking for other keys needs the device's event mask, which is an
//! `ioctl`, and the design allows none here.
//!
//! Input: the path of the device, the bindings, and the waits between attempts to open it.
//! Output: [`Input::Key`] and [`Input::KeyDevice`] to the session loop, which owns every
//! decision; this is one more source of what happened, like standard input.
//! Owns: one thread, the open device, the read buffer and the decoder.
//!
//! What it keeps:
//!
//! - **Only an event device is read.** Before it is opened the path is resolved, and it
//!   must lead to a character device named `event` and a number, directly in `/dev/input`
//!   (so not `mice`, `mouse0` or `js0`, whose records are another shape); after it is
//!   opened, the open file itself must be that same character device. Anything else is
//!   refused and not read. A refusal for what the path leads to is not tried again: it is
//!   about the configuration, not about a keyboard coming and going. A device that was
//!   replaced between the check and the open is a keyboard being plugged in at that
//!   moment, and is tried again.
//! - **Every way a read can go wrong is the device going away:** an error, a read of
//!   nothing, and a read that leaves the decoder holding part of a record (an event device
//!   returns whole records, so a fragment is a fault). The session loop is told first, so
//!   that the gate shuts as for a release; then the decoder is reset; and only then is the
//!   device opened again, after a wait that doubles up to a bound. Nothing decoded from a
//!   faulty read is passed on.
//! - **The session loop is told once.** That the device is not there is said when it is
//!   lost or first cannot be opened, not on every attempt; and once more if the watcher
//!   then gives up, so that nothing goes on saying "trying again" when nothing is. A device
//!   that opens and then fails before one whole read is not announced as open again until
//!   a read has worked, so a device that never works cannot make a line per attempt.
//! - **It says whether the device had been open.** A device that was being read and is
//!   not any more is [`DeviceState::Lost`]: the push-to-talk machine then refuses every
//!   press, from standard input too, until the device is back. One that has never been
//!   read is [`DeviceState::Missing`], and standard input works as if there were none.
//!
//! What it cannot notice: permission is checked when a device is opened, so taking the
//! permission away changes nothing for a watcher that already has the device open. It goes
//! on reading until the keyboard is unplugged or the client exits.

use std::fmt;
use std::fs::{File, Metadata};
use std::io::{self, Read};
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::time::Duration;

use conch_voice_control::{INPUT_EVENT_LEN, KeyBindings, KeyDecoder, KeyEvent};
use tokio::sync::mpsc;

use crate::session::Input;

/// Where the kernel's input devices are. Only an event device directly in it is read.
pub const DEV_INPUT: &str = "/dev/input";

/// How the kernel names an event device there: this, and then a number.
const EVENT_NAME: &str = "event";

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
    /// An event device: a character device named `event` and a number, directly in
    /// `/dev/input`. Nothing else.
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
    /// It is under `/dev/input` and is not named as an event device is: `mice`, `mouse0`
    /// and `js0` are devices of another kind, with records of another shape.
    NotAnEventDevice,
    /// It is not a character device.
    NotCharacterDevice,
    /// What was opened is not what the path named a moment before: the device was replaced
    /// in between, as when a keyboard is plugged in. The only refusal that is tried again.
    Changed,
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Refusal::OutsideDevInput => "it is not under /dev/input",
            Refusal::NotAnEventDevice => {
                "it is not an event device (/dev/input/event and a number)"
            }
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

    /// Whether the watcher goes on trying to open the device. A refusal for what the path
    /// leads to is final; one for a device that changed while it was being opened is not.
    #[must_use]
    pub fn retried(&self) -> bool {
        match self {
            Problem::Refused(refusal) => *refusal == Refusal::Changed,
            _ => true,
        }
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
    /// It is not being read and has not been since the client started, and why. Nothing
    /// was ever held on it, so presses from standard input work as if there were no device.
    Missing(Problem),
    /// It was being read and is not any more, and why. The push-to-talk machine holds that
    /// against every press, from standard input too, until the device is read again.
    Lost(Problem),
}

impl DeviceState {
    /// True while the device is being read.
    #[must_use]
    pub fn is_ready(&self) -> bool {
        matches!(self, DeviceState::Ready)
    }

    /// Why the device is not being read, if it is not.
    #[must_use]
    pub fn problem(&self) -> Option<Problem> {
        match self {
            DeviceState::Ready => None,
            DeviceState::Missing(problem) | DeviceState::Lost(problem) => Some(*problem),
        }
    }

    /// True if a press is refused, from standard input too, for as long as this lasts.
    #[must_use]
    pub fn presses_refused(&self) -> bool {
        matches!(self, DeviceState::Lost(_))
    }

    /// The state in words, for the one line that says it: why, and what the client does
    /// about it. It says what is true of presses, which differs between a device that was
    /// never open and one that was.
    #[must_use]
    pub fn describe(&self) -> String {
        match self {
            DeviceState::Ready => "open".to_owned(),
            DeviceState::Missing(problem) => {
                let then = if problem.retried() {
                    ", and trying again"
                } else {
                    ""
                };
                format!(
                    "{problem}; taking down, up, mute, deafen and quit from standard input{then}"
                )
            }
            DeviceState::Lost(problem) => {
                let (until, then) = if problem.retried() {
                    ("it is open again or ", "trying again")
                } else {
                    ("", "not trying again")
                };
                format!(
                    "{problem}; a press is refused, from standard input too, until {until}\
                     conch-voice is started again with --stdin-keys; {then}"
                )
            }
        }
    }
}

/// True for `event` followed by one or more digits: the name of an event device.
fn is_event_name(name: &str) -> bool {
    name.strip_prefix(EVENT_NAME).is_some_and(|number| {
        !number.is_empty() && number.bytes().all(|digit| digit.is_ascii_digit())
    })
}

/// The rule itself, on facts that need nothing opened: where the resolved path is, and
/// whether a character device is there.
///
/// # Errors
///
/// The [`Refusal`] for a path that is not strictly under [`DEV_INPUT`]; for one that is
/// not `event` and a number directly in it; or for something that is not a character
/// device.
pub fn permitted(canonical: &Path, is_char_device: bool) -> Result<(), Refusal> {
    let dev_input = Path::new(DEV_INPUT);
    // By components, so `/dev/inputs/x` is outside; and the directory itself is not a device.
    if !canonical.starts_with(dev_input) || canonical == dev_input {
        return Err(Refusal::OutsideDevInput);
    }
    // Directly in the directory, under the name the kernel gives an event device. The
    // links under `by-id` and `by-path` resolve to such a name; nothing else is one.
    let named_as_event = canonical.parent() == Some(dev_input)
        && canonical
            .file_name()
            .and_then(|name| name.to_str())
            .is_some_and(is_event_name);
    if !named_as_event {
        return Err(Refusal::NotAnEventDevice);
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
/// unless it leads to an event device, a character device named `event` and a number
/// directly in `/dev/input`; what was then opened is checked again from the open file
/// itself, so that the thing checked is the thing read.
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
    /// What the session loop was last told. `None` before it has been told anything.
    told: Option<Told>,
    /// Whether the session loop has ever been told the device is being read. From then on
    /// a device that is not being read is one that was lost.
    was_ready: bool,
}

/// What the session loop was last told about the device.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Told {
    /// It is being read.
    Ready,
    /// It is not, and whether the watcher goes on trying to open it.
    NotReady { retried: bool },
}

impl Watcher {
    /// Tells the session loop the device is being read, unless that is what it was last
    /// told.
    fn ready(&mut self) -> Result<(), LoopGone> {
        if self.told == Some(Told::Ready) {
            return Ok(());
        }
        self.told = Some(Told::Ready);
        self.was_ready = true;
        self.inputs
            .send(Input::KeyDevice(DeviceState::Ready))
            .map_err(|_| LoopGone)
    }

    /// Tells the session loop the device is not being read: when it stops being read, and
    /// once more if the watcher then gives up. It is not said for each attempt, nor for a
    /// reason that changes while the attempts go on.
    fn not_ready(&mut self, problem: Problem) -> Result<(), LoopGone> {
        let now = Told::NotReady {
            retried: problem.retried(),
        };
        if self.told == Some(now) {
            return Ok(());
        }
        self.told = Some(now);
        let state = if self.was_ready {
            DeviceState::Lost(problem)
        } else {
            DeviceState::Missing(problem)
        };
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
                    self.ready()?;
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
                        self.ready()?;
                    }
                    let (problem, read_any) = self.read_to_fault(&mut file)?;
                    // The machine first, so the gate shuts as for a release; then the
                    // decoder; and the device is closed before it is opened again.
                    self.not_ready(problem)?;
                    self.reader.reset();
                    drop(file);
                    announce_on_open = read_any;
                    if read_any {
                        wait = self.timings.first_wait;
                    }
                }
                Err(problem) => {
                    self.not_ready(problem)?;
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
        was_ready: false,
    };
    std::thread::Builder::new()
        .name("conch-voice-keys".to_owned())
        .spawn(move || {
            // It ends when the session loop does, or on a refusal that is final; either way
            // silently.
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
    fn the_rule_takes_an_event_device_directly_under_dev_input_and_nothing_else() {
        let not_event = Err(Refusal::NotAnEventDevice);
        // Paths that need no opening: the rule is about where a path leads and what is there.
        for (path, is_char_device, expected) in [
            ("/dev/input/event3", true, Ok(())),
            ("/dev/input/event0", true, Ok(())),
            ("/dev/input/event127", true, Ok(())),
            ("/dev/input/event3", false, Err(Refusal::NotCharacterDevice)),
            // Character devices under /dev/input that are not event devices: their records
            // are another shape.
            ("/dev/input/mice", true, not_event),
            ("/dev/input/mouse0", true, not_event),
            ("/dev/input/js0", true, not_event),
            // Not `event` and then nothing but digits.
            ("/dev/input/event", true, not_event),
            ("/dev/input/event1x", true, not_event),
            ("/dev/input/eventx", true, not_event),
            ("/dev/input/event-1", true, not_event),
            ("/dev/input/event 1", true, not_event),
            ("/dev/input/event\u{0663}", true, not_event),
            ("/dev/input/Event3", true, not_event),
            ("/dev/input/xevent3", true, not_event),
            // Not directly in the directory: a name under a subdirectory that resolves to
            // itself is not what the kernel made. (A link there resolves to `eventN`.)
            ("/dev/input/by-id/event3", true, not_event),
            ("/dev/input/by-id/usb-Example-event-kbd", true, not_event),
            (
                "/dev/input/by-path/platform-i8042-serio-0-event-kbd",
                true,
                not_event,
            ),
            ("/dev/input/event3/event4", true, not_event),
            // Never what resolving a path gives, and refused all the same.
            ("/dev/input/..", true, not_event),
            ("/dev/input/event3/..", true, not_event),
            ("/dev/input", true, Err(Refusal::OutsideDevInput)),
            ("/dev/input/", true, Err(Refusal::OutsideDevInput)),
            ("/dev/INPUT/event3", true, Err(Refusal::OutsideDevInput)),
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
                Problem::Refused(Refusal::NotAnEventDevice),
                "refused",
                false,
            ),
            (
                Problem::Refused(Refusal::NotCharacterDevice),
                "refused",
                false,
            ),
            // Replaced between the check and the open: a keyboard being plugged in, not a
            // wrong configuration, so it is tried again.
            (Problem::Refused(Refusal::Changed), "refused", true),
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
        assert_eq!(
            Problem::Refused(Refusal::NotAnEventDevice).to_string(),
            "refused: it is not an event device (/dev/input/event and a number)"
        );
        let unnumbered = OsError::from(&io::Error::from(io::ErrorKind::UnexpectedEof));
        assert_eq!(
            unnumbered.to_string(),
            io::ErrorKind::UnexpectedEof.to_string()
        );
    }

    /// A watcher that is never run, to see what it tells the session loop.
    fn idle_watcher() -> (Watcher, mpsc::UnboundedReceiver<Input>) {
        let (inputs, received) = mpsc::unbounded_channel();
        let watcher = Watcher {
            device: PathBuf::from("unused"),
            rule: DeviceRule::AnyFileForTests,
            timings: KeyTimings::default(),
            inputs,
            reader: Reader::new(bindings()),
            told: None,
            was_ready: false,
        };
        (watcher, received)
    }

    /// Everything the watcher has told the session loop since the last call.
    fn told(received: &mut mpsc::UnboundedReceiver<Input>) -> Vec<DeviceState> {
        let mut states = Vec::new();
        while let Ok(input) = received.try_recv() {
            let Input::KeyDevice(state) = input else {
                panic!("not a device state: {input:?}");
            };
            states.push(state);
        }
        states
    }

    #[test]
    fn a_loss_is_said_once_and_giving_up_after_it_is_said_too() {
        let gone = Problem::CannotOpen(OsError::from(&io::Error::from_raw_os_error(2)));
        let changed = Problem::Refused(Refusal::Changed);
        let not_event = Problem::Refused(Refusal::NotAnEventDevice);

        let (mut watcher, mut received) = idle_watcher();
        watcher.ready().ok().unwrap();
        watcher.ready().ok().unwrap();
        assert_eq!(told(&mut received), [DeviceState::Ready], "said once");

        // It was being read, so from here on it is lost, not merely missing.
        watcher.not_ready(Problem::Ended).ok().unwrap();
        assert_eq!(told(&mut received), [DeviceState::Lost(Problem::Ended)]);
        // Attempts that fail, for whatever reason that is tried again: nothing is said.
        watcher.not_ready(gone).ok().unwrap();
        watcher.not_ready(changed).ok().unwrap();
        watcher.not_ready(gone).ok().unwrap();
        assert_eq!(told(&mut received), []);
        // Then what is there turns out to be something that is refused for good: said, so
        // that nothing goes on claiming the client is trying.
        watcher.not_ready(not_event).ok().unwrap();
        watcher.not_ready(not_event).ok().unwrap();
        let said = told(&mut received);
        assert_eq!(said, [DeviceState::Lost(not_event)]);
        assert!(said[0].presses_refused() && !not_event.retried());
    }

    #[test]
    fn a_device_that_was_never_read_is_missing_and_one_that_was_is_lost() {
        let gone = Problem::CannotOpen(OsError::from(&io::Error::from_raw_os_error(2)));
        let outside = Problem::Refused(Refusal::OutsideDevInput);

        // Never opened: missing, and giving up is said as well.
        let (mut watcher, mut received) = idle_watcher();
        watcher.not_ready(gone).ok().unwrap();
        watcher.not_ready(gone).ok().unwrap();
        watcher.not_ready(outside).ok().unwrap();
        let said = told(&mut received);
        assert_eq!(
            said,
            [DeviceState::Missing(gone), DeviceState::Missing(outside)]
        );
        assert!(said.iter().all(|state| !state.presses_refused()));

        // Missing, then there, then gone: the last is a loss.
        let (mut watcher, mut received) = idle_watcher();
        watcher.not_ready(gone).ok().unwrap();
        watcher.ready().ok().unwrap();
        watcher.not_ready(Problem::PartialRecord).ok().unwrap();
        watcher.ready().ok().unwrap();
        watcher.not_ready(gone).ok().unwrap();
        assert_eq!(
            told(&mut received),
            [
                DeviceState::Missing(gone),
                DeviceState::Ready,
                DeviceState::Lost(Problem::PartialRecord),
                DeviceState::Ready,
                DeviceState::Lost(gone),
            ]
        );
    }

    #[test]
    fn the_line_says_what_is_true_of_presses_for_each_state() {
        let gone = Problem::CannotOpen(OsError::from(&io::Error::from_raw_os_error(2)));
        let why = format!("cannot open it: {}", io::Error::from_raw_os_error(2));
        assert_eq!(DeviceState::Ready.describe(), "open");
        assert_eq!(DeviceState::Ready.problem(), None);
        // Never open: standard input is the talk key, as if there were no device.
        assert_eq!(
            DeviceState::Missing(gone).describe(),
            format!(
                "{why}; taking down, up, mute, deafen and quit from standard input, and trying \
                 again"
            )
        );
        assert_eq!(
            DeviceState::Missing(Problem::Refused(Refusal::NotAnEventDevice)).describe(),
            "refused: it is not an event device (/dev/input/event and a number); taking down, \
             up, mute, deafen and quit from standard input"
        );
        // It was open: the machine refuses every press until it is back.
        assert_eq!(
            DeviceState::Lost(Problem::Ended).describe(),
            "it ended: a read returned nothing; a press is refused, from standard input too, \
             until it is open again or conch-voice is started again with --stdin-keys; trying \
             again"
        );
        assert_eq!(
            DeviceState::Lost(Problem::Refused(Refusal::OutsideDevInput)).describe(),
            "refused: it is not under /dev/input; a press is refused, from standard input too, \
             until conch-voice is started again with --stdin-keys; not trying again"
        );
        assert_eq!(DeviceState::Lost(gone).problem(), Some(gone));
        assert!(!DeviceState::Missing(gone).is_ready() && !DeviceState::Lost(gone).is_ready());
    }
}
