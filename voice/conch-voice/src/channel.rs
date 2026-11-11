//! The events endpoint of `conch-voice managed --events-pipe <path>`: a Unix socket the
//! desktop owner listens on, which the helper connects to by path instead of taking file
//! descriptor 3 (`docs/design/linux-desktop.md` §5.2, issue #937, the helper's half of L-F2).
//! An Electron owner cannot hand over a FIFO: libuv makes every `'pipe'` stdio entry a
//! `SOCK_STREAM` socketpair, which `managed::open_event_pipe` rightly refuses (§5.1).
//!
//! **On Windows** the endpoint is a named pipe the owner created, `\\.\pipe\conch-voice-<id>`,
//! which the helper opens with tokio's safe overlapped client (`docs/design/windows-desktop.md`
//! §3.3, issue #938); there is no file descriptor 3 to take there and `FromRawHandle` is
//! `unsafe`. Everything below holds there in the same words, with these differences: the name
//! must begin `\\.\pipe\` and is checked before anything else; there is no directory to
//! examine, the pipe's DACL is the boundary between users; the open is retried every 50 ms
//! while the pipe is busy (`ERROR_PIPE_BUSY`) until the hello's time has passed; the
//! connection is used from two sides at once through one shared handle, the write side for
//! the events (a synchronous `Write` bridged with `Handle::block_on`, `BlockingWriter`)
//! and the read side for its end (`BlockingReader`), each on a thread that is not the
//! runtime's. A write that fails is owner loss as well as the read's end: on Windows
//! nothing else tells the helper its owner is gone, because there is no parent watch
//! (`managed::parent_id` is 0 there). The watch on the end depends on the runtime's I/O
//! driver being alive; if it is not, the std thread that reads standard input (its end is
//! owner loss too) and the watchdog are the backstop.
//!
//! Input: the path from the command line; the nonce from `CONCH_VOICE_ATTACH` (read by
//! `main` only when the option is given); the file system's word on the socket and its
//! directory; whatever arrives on the connection.
//! Output: the connection, with the attach line already written on it; and
//! [`Input::OwnerLost`] when the connection ends or anything is received on it.
//! Owns: the checks made before connecting, the connect, the attach write, and the thread
//! that reads the connection solely to see its end. The events themselves are written by
//! `managed::Events`, which is given the connection as it would be a pipe.
//!
//! What it keeps:
//!
//! - **Nothing is opened, read or connected before the path is known to be absolute.** An
//!   abstract name (`@name`) has no permissions to check, and a relative path means
//!   whatever the working directory makes it: both are refused. The nonce is checked next,
//!   and only then is the file system asked anything; each refusal is exit 2 with one
//!   reason on one line, and no refusal repeats the nonce.
//! - **The socket's directory is the boundary.** Before connecting, `symlink_metadata` (it
//!   never follows a link) says the socket is a socket owned by this user, and says its
//!   parent is a directory, not a symlink, owned by this user, with mode exactly `0o700`.
//!   The socket's own mode is not checked: the directory keeps other users out, and Node
//!   creates the socket with the umask's mode.
//! - **The attach line is the first thing on the connection**, written and flushed before
//!   the hello, by the thread that connected; if it cannot be written, nothing else was
//!   done. The nonce is held as a [`Secret`], registered with the scrubber, and gone from
//!   this module once the line is written: the environment given on to the session does
//!   not carry it.
//! - **The owner never writes here.** A byte received is the channel being someone else's,
//!   so it is owner loss, as is the end of the connection or an error: the thread that
//!   reads it never parses, keeps or logs what it reads.
//!
//! What it does not keep: the attach line is a spawn-to-pipe binding, defence in depth, and
//! **not authentication** of either side (`conch_voice_api::control_pipe`). The checks
//! above are made on the path and the connect is made on the same path a moment later: a
//! process of the same user can unlink the socket and bind its own in that gap (or at any
//! time before it) and then receives the attach line and every event. That same-user
//! unlink-and-rebind race is out of scope by design and named: that process could read the
//! helper's environment, and so the nonce, anyway. On Windows its twin is the same-user
//! `CreateNamedPipeW` instance hijack: a process of the same user that creates another
//! instance of the pipe's name, as the owner does, can be the instance the helper opens
//! (the owner's `ERROR_PIPE_BUSY` is the only thing that could say so); it is out of scope
//! for the same reason, and the pipe's DACL keeps other users out
//! (`docs/design/windows-desktop.md` §3.4 to §3.5). The invariant that makes it a matter of
//! confidentiality and availability and never of authority is that authority arrives only on
//! standard input. Another user cannot reach the socket: the `0o700` directory denies them
//! the search permission on the way to it.

use std::io::{Read, Write};
#[cfg(unix)]
use std::os::unix::ffi::OsStrExt;
#[cfg(unix)]
use std::os::unix::fs::{FileTypeExt, MetadataExt};
#[cfg(unix)]
use std::os::unix::net::UnixStream;
use std::path::Path;
#[cfg(any(windows, test))]
use std::sync::Arc;
use std::time::Duration;
#[cfg(any(windows, test))]
use std::time::Instant;

use conch_voice_api::{
    Secret, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_PIPE_NONCE_CHARS,
    VoiceControlPipeAttachedV1, to_line, validate_nonce,
};
use tokio::sync::mpsc;

use crate::cli::Environment;
use crate::error::Error;
use crate::managed::Watchdog;
use crate::secrets::Scrubber;
use crate::session::Input;

/// The connection to the owner, written to: a Unix stream socket.
#[cfg(unix)]
pub type Stream = UnixStream;

/// The write side of the connection, which [`attach`] has written the attach line to.
#[cfg(unix)]
pub type Events = UnixStream;

/// The read side of the connection, for [`watch_end`].
#[cfg(unix)]
pub type End = UnixStream;

/// The write side of the connection on Windows: the named pipe, through a synchronous
/// [`Write`] for `managed::Events`.
#[cfg(windows)]
pub type Events = BlockingWriter<tokio::net::windows::named_pipe::NamedPipeClient>;

/// The read side of the connection on Windows, for [`watch_end`].
#[cfg(windows)]
pub type End = BlockingReader<tokio::net::windows::named_pipe::NamedPipeClient>;

/// The connection to the owner on a platform that has neither Unix sockets nor named
/// pipes: a type with no value, so that [`attach`], which only ever answers
/// [`Error::Unsupported`] there, leaves nothing that could be written to or read from.
#[cfg(not(any(unix, windows)))]
#[derive(Debug)]
pub enum Stream {}

/// See [`Stream`].
#[cfg(not(any(unix, windows)))]
pub type Events = Stream;

/// See [`Stream`].
#[cfg(not(any(unix, windows)))]
pub type End = Stream;

#[cfg(not(any(unix, windows)))]
impl Read for Stream {
    fn read(&mut self, _buf: &mut [u8]) -> std::io::Result<usize> {
        match *self {}
    }
}

#[cfg(not(any(unix, windows)))]
impl Write for Stream {
    fn write(&mut self, _buf: &[u8]) -> std::io::Result<usize> {
        match *self {}
    }

    fn flush(&mut self) -> std::io::Result<()> {
        match *self {}
    }
}

/// The connection to the owner, as the helper holds it once it has attached.
#[derive(Debug)]
pub struct Endpoint {
    /// Where the events are written: the connection, after the attach line.
    pub events: Events,
    /// Another handle on the same connection, for [`watch_end`] to read.
    pub end: End,
}

/// What [`attach`] answers where this platform has no control channel at all:
/// [`Error::Unsupported`], never a panic. Compiled on Linux too so that a test holds it.
#[cfg(any(not(any(unix, windows)), test))]
fn unsupported() -> Error {
    Error::Unsupported {
        what: "the events endpoint (--events-pipe)",
        path: None,
    }
}

/// Whether the path is one this module will look at at all: it begins with `/`. A relative
/// path, an empty one and an abstract name (`@name`; on Linux an address whose first byte
/// is NUL, which a command line cannot hold) are refused, without anything being opened.
///
/// # Errors
///
/// [`Error::Attach`] with the reason.
#[cfg(unix)]
pub fn require_absolute(path: &Path) -> Result<(), Error> {
    if path.as_os_str().as_bytes().first() == Some(&b'/') {
        Ok(())
    } else {
        Err(Error::Attach("path not absolute"))
    }
}

/// The nonce `environment` holds, if it is one: 43 characters of canonical unpadded
/// base64url. A value of the right length is registered with `scrubber` before its shape is
/// judged, valid or not, so that no text this process writes can repeat it. One of another
/// length is not: it is no nonce, and a short one (`e`) would scrub every such letter from
/// the one line that says why it was refused. The reason for a refusal never repeats it.
///
/// # Errors
///
/// [`Error::Attach`] if there is none or it is not well formed.
pub fn nonce<'a>(environment: &'a Environment, scrubber: &Scrubber) -> Result<&'a Secret, Error> {
    let secret = environment
        .conch_voice_attach
        .as_ref()
        .filter(|secret| !secret.is_empty())
        .ok_or(Error::Attach("no attach nonce"))?;
    if secret.expose().len() == VOICE_CONTROL_PIPE_NONCE_CHARS {
        scrubber.always(secret);
    }
    validate_nonce(secret.expose()).map_err(|_| Error::Attach("attach nonce malformed"))?;
    Ok(secret)
}

/// What the file system says of the socket and its directory, for the user `uid`: the
/// socket is a socket (not a link to one) of `uid`; its parent is a directory, not a
/// symlink, of `uid`, with mode exactly `0o700`. Nothing is followed and nothing is
/// opened. The socket's own mode is not looked at.
///
/// # Errors
///
/// [`Error::Attach`] with the first reason that holds, the directory first: `parent is a
/// symlink`, `parent not a directory`, `parent not owned`, `parent mode`, then `not a
/// socket` and `not owned` (or that either could not be examined).
#[cfg(unix)]
pub fn check_endpoint(path: &Path, uid: u32) -> Result<(), Error> {
    // The directory first: it is the boundary, and what is in it is judged only once it is
    // known to be ours and private.
    let parent = path
        .parent()
        .ok_or(Error::Attach("parent not a directory"))?;
    let directory =
        std::fs::symlink_metadata(parent).map_err(|_| Error::Attach("parent cannot examine"))?;
    // Before the directory test: a symlink is not a directory to `symlink_metadata`.
    if directory.file_type().is_symlink() {
        return Err(Error::Attach("parent is a symlink"));
    }
    if !directory.file_type().is_dir() {
        return Err(Error::Attach("parent not a directory"));
    }
    if directory.uid() != uid {
        return Err(Error::Attach("parent not owned"));
    }
    if directory.mode() & 0o777 != 0o700 {
        return Err(Error::Attach("parent mode"));
    }
    let socket = std::fs::symlink_metadata(path).map_err(|_| Error::Attach("cannot examine"))?;
    if !socket.file_type().is_socket() {
        return Err(Error::Attach("not a socket"));
    }
    if socket.uid() != uid {
        return Err(Error::Attach("not owned"));
    }
    Ok(())
}

/// Connects to `path`, once: there is no retry on Linux, a socket whose listener is not
/// there is an owner that is not. The owner's backlog being full would make a blocking
/// connect wait, so it is made on a thread and given what the hello is given.
///
/// # Errors
///
/// [`Error::Attach`] if the connect fails or does not finish in time.
#[cfg(unix)]
pub fn connect(path: &Path) -> Result<UnixStream, Error> {
    use conch_voice_api::VOICE_CONTROL_HELLO_TIMEOUT_MS;
    use std::sync::mpsc as std_mpsc;
    let path = path.to_owned();
    let (done, result) = std_mpsc::channel();
    std::thread::spawn(move || {
        let _ = done.send(UnixStream::connect(path));
    });
    result
        .recv_timeout(Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS))
        .map_err(|_| Error::Attach("cannot connect in time"))?
        .map_err(|_| Error::Attach("cannot connect"))
}

/// Writes the attach line for `nonce` to `out` and flushes it: the first and only line of
/// its kind, before the hello. Nothing of the line or the nonce is kept.
///
/// # Errors
///
/// [`Error::Attach`] if the line cannot be built or written.
pub fn write_attach(out: &mut impl Write, nonce: &Secret) -> Result<(), Error> {
    let attached = VoiceControlPipeAttachedV1::new(nonce.expose())
        .map_err(|_| Error::Attach("attach nonce malformed"))?;
    let mut line = to_line(&attached)
        .map_err(|_| Error::Attach("attach line not built"))?
        .into_bytes();
    line.push(b'\n');
    out.write_all(&line)
        .and_then(|()| out.flush())
        .map_err(|_| Error::Attach("attach not written"))
}

/// Everything before the session for `--events-pipe path`, in this order: the path is
/// absolute; the nonce is there and well formed; the socket and its directory are as they
/// must be; the connection is made; the attach line is written. The first thing to fail is
/// the error, and nothing after it was done.
///
/// # Errors
///
/// [`Error::Attach`] with the reason, which never repeats the nonce.
#[cfg(unix)]
pub fn attach(
    path: &Path,
    environment: &Environment,
    scrubber: &Scrubber,
) -> Result<Endpoint, Error> {
    use crate::devices;
    use conch_voice_api::VOICE_CONTROL_HELLO_TIMEOUT_MS;
    require_absolute(path)?;
    let secret = nonce(environment, scrubber)?;
    let uid = devices::current_uid().ok_or(Error::Attach("own user unknown"))?;
    check_endpoint(path, uid)?;
    let mut events = connect(path)?;
    // The attach line is small; a peer that does not read it must not hold the helper.
    events
        .set_write_timeout(Some(Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS)))
        .map_err(|_| Error::Attach("attach not written"))?;
    write_attach(&mut events, secret)?;
    // The events are written without a limit, as to a pipe: a slow owner loses events
    // (`managed::Events`), not the helper.
    events
        .set_write_timeout(None)
        .map_err(|_| Error::Attach("attach not written"))?;
    let end = events
        .try_clone()
        .map_err(|_| Error::Attach("cannot read"))?;
    Ok(Endpoint { events, end })
}

/// [`attach`] on a platform with neither Unix sockets nor named pipes: nothing is checked,
/// opened or connected.
///
/// # Errors
///
/// Always [`Error::Unsupported`].
#[cfg(not(any(unix, windows)))]
pub fn attach(
    _path: &Path,
    _environment: &Environment,
    _scrubber: &Scrubber,
) -> Result<Endpoint, Error> {
    Err(unsupported())
}

/// Everything before the session for `--events-pipe name` on Windows, in this order: the
/// name is `\\.\pipe\` and a name; the nonce is there and well formed; the pipe is opened
/// (retried while it is busy, up to the hello's time from here); the attach line is written
/// and flushed. The first thing to fail is the error, and nothing after it was done.
///
/// It must be called with a tokio runtime entered (`Runtime::enter`), the one the events
/// are later written through: the pipe is registered with it.
///
/// # Errors
///
/// [`Error::Attach`] with the reason, which never repeats the nonce or the name.
#[cfg(windows)]
pub fn attach(
    path: &Path,
    environment: &Environment,
    scrubber: &Scrubber,
) -> Result<Endpoint, Error> {
    use conch_voice_api::VOICE_CONTROL_HELLO_TIMEOUT_MS;
    // One deadline for the retry and the attach write together: the hello's time from the
    // start of the process, so the worst case is one hello timeout.
    let deadline = process_start() + Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS);
    require_pipe_name(path)?;
    let secret = nonce(environment, scrubber)?;
    let handle = tokio::runtime::Handle::try_current().map_err(|_| Error::Attach("no runtime"))?;
    let client = open_with_retry(
        || client_options().open(path),
        deadline,
        PIPE_BUSY_RETRY,
        std::thread::sleep,
    )?;
    let half = Arc::new(client);
    let mut events = BlockingWriter::new(Arc::clone(&half), handle.clone());
    // The attach line is small; a peer that does not read it must not hold the helper.
    events.limit = Some(deadline.saturating_duration_since(Instant::now()));
    write_attach(&mut events, secret)?;
    // The events are written without a limit, as to a pipe: a slow owner loses events
    // (`managed::Events`), not the helper.
    events.limit = None;
    let end = BlockingReader::new(half, handle);
    Ok(Endpoint { events, end })
}

/// The time the process began, as far as this module knows: `main` calls this first thing
/// and it keeps that moment; a caller that never did (a test) gets the moment of its first
/// call. The events endpoint's time to attach is counted from it.
pub fn process_start() -> std::time::Instant {
    static START: std::sync::OnceLock<std::time::Instant> = std::sync::OnceLock::new();
    *START.get_or_init(std::time::Instant::now)
}

/// The Windows error for a pipe that exists and has no instance waiting for a client.
#[cfg(any(windows, test))]
const ERROR_PIPE_BUSY: i32 = 231;

/// How long the open waits between two tries while the pipe is busy.
#[cfg(any(windows, test))]
const PIPE_BUSY_RETRY: Duration = Duration::from_millis(50);

/// The prefix of the name of every named pipe on this machine.
#[cfg(any(windows, test))]
const PIPE_PREFIX: &str = r"\\.\pipe\";

/// The longest name after the prefix that is opened.
#[cfg(any(windows, test))]
const PIPE_NAME_MAX: usize = 200;

/// Whether `path` is a pipe name this module will open: `\\.\pipe\` and then one name of
/// ASCII letters, digits, `-` and `_`, 1 to 200 bytes: the owner's names are `conch-voice-`
/// and hex. A server's pipe (`\\host\pipe\x`), a relative name, a Unix path, the empty text
/// and anything with a dot, a separator, a NUL or a space in the name are refused, so that
/// nothing can leave the pipe namespace (`..`) or be read differently by the system than
/// here. Nothing is opened for any of them.
///
/// # Errors
///
/// [`Error::Attach`] with the reason.
#[cfg(any(windows, test))]
pub fn require_pipe_name(path: &Path) -> Result<(), Error> {
    const REFUSED: &str = r"pipe name is not \\.\pipe\<name>";
    let text = path.to_str().ok_or(Error::Attach(REFUSED))?;
    // Pipe names are not case sensitive, so neither is their prefix.
    let head = text
        .get(..PIPE_PREFIX.len())
        .ok_or(Error::Attach(REFUSED))?;
    if !head.eq_ignore_ascii_case(PIPE_PREFIX) {
        return Err(Error::Attach(REFUSED));
    }
    let name = &text[PIPE_PREFIX.len()..];
    let allowed = |byte: u8| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_';
    if name.is_empty() || name.len() > PIPE_NAME_MAX || !name.bytes().all(allowed) {
        return Err(Error::Attach(REFUSED));
    }
    Ok(())
}

/// The options the pipe is opened with: read and write, and nothing else. In particular
/// `security_qos_flags` is not set, so tokio's default stands (`SECURITY_SQOS_PRESENT` with
/// `SECURITY_IDENTIFICATION`) and the server cannot impersonate the helper.
#[cfg(windows)]
fn client_options() -> tokio::net::windows::named_pipe::ClientOptions {
    let mut options = tokio::net::windows::named_pipe::ClientOptions::new();
    options.read(true).write(true);
    options
}

/// Calls `open` until it answers, as long as what it says is [`ERROR_PIPE_BUSY`] and
/// `deadline` has not come: a busy pipe is an owner with no instance waiting yet, and is
/// tried again after `every` (given to `sleep`). Any other error ends it at once.
///
/// # Errors
///
/// [`Error::Attach`]: `cannot open` for any other error, `cannot open in time` for a pipe
/// that stayed busy.
#[cfg(any(windows, test))]
fn open_with_retry<T>(
    mut open: impl FnMut() -> std::io::Result<T>,
    deadline: Instant,
    every: Duration,
    mut sleep: impl FnMut(Duration),
) -> Result<T, Error> {
    loop {
        match open() {
            Ok(opened) => return Ok(opened),
            Err(error) if error.raw_os_error() == Some(ERROR_PIPE_BUSY) => {
                if Instant::now() >= deadline {
                    return Err(Error::Attach("cannot open in time"));
                }
                sleep(every);
            }
            Err(_) => return Err(Error::Attach("cannot open")),
        }
    }
}

/// One end of a connection, used from two threads at once through a shared reference: what
/// tokio's `NamedPipeClient` is. [`BlockingWriter`] and [`BlockingReader`] are the
/// synchronous faces of it, written against this so that the tests need no pipe.
///
/// `async fn` in a trait is fine here: the futures are only ever run by `block_on`, on the
/// thread that made them.
#[cfg(any(windows, test))]
#[allow(
    async_fn_in_trait,
    reason = "the futures are run by Handle::block_on on the calling thread, never spawned"
)]
pub trait Half: Send + Sync + 'static {
    /// Writes some of `buf`, waiting until the connection can take it.
    async fn write_some(&self, buf: &[u8]) -> std::io::Result<usize>;
    /// Reads some bytes, waiting until there are any: `Ok(0)` is the end.
    async fn read_some(&self, buf: &mut [u8]) -> std::io::Result<usize>;
}

#[cfg(windows)]
impl Half for tokio::net::windows::named_pipe::NamedPipeClient {
    async fn write_some(&self, buf: &[u8]) -> std::io::Result<usize> {
        loop {
            self.writable().await?;
            match self.try_write(buf) {
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                done => return done,
            }
        }
    }

    async fn read_some(&self, buf: &mut [u8]) -> std::io::Result<usize> {
        loop {
            self.readable().await?;
            match self.try_read(buf) {
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                done => return done,
            }
        }
    }
}

/// The events' [`Write`]: each write is made on the shared connection through
/// `Handle::block_on`, so it must be called from a thread that is not one of the runtime's
/// (`managed::Events`' writer thread is not; neither is the one that writes the attach
/// line). Nothing is buffered, so `flush` has nothing to do. A failed write is a
/// [`std::io::Error`], which `managed::Watched` takes as the owner gone.
#[cfg(any(windows, test))]
#[derive(Debug)]
pub struct BlockingWriter<H> {
    half: Arc<H>,
    handle: tokio::runtime::Handle,
    /// How long one write may wait; none once the attach line is written.
    limit: Option<Duration>,
}

#[cfg(any(windows, test))]
impl<H: Half> BlockingWriter<H> {
    fn new(half: Arc<H>, handle: tokio::runtime::Handle) -> Self {
        Self {
            half,
            handle,
            limit: None,
        }
    }
}

#[cfg(any(windows, test))]
impl<H: Half> Write for BlockingWriter<H> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        if buf.is_empty() {
            return Ok(0);
        }
        let write = self.half.write_some(buf);
        match self.limit {
            None => self.handle.block_on(write),
            Some(limit) => self
                .handle
                .block_on(tokio::time::timeout(limit, write))
                .unwrap_or_else(|_| Err(std::io::ErrorKind::TimedOut.into())),
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// The read side's [`Read`], for [`watch_end`]: waits through `Handle::block_on` on a
/// thread that is not one of the runtime's, and says what the connection says, `Ok(0)` for
/// its end included.
#[cfg(any(windows, test))]
#[derive(Debug)]
pub struct BlockingReader<H> {
    half: Arc<H>,
    handle: tokio::runtime::Handle,
}

#[cfg(any(windows, test))]
impl<H: Half> BlockingReader<H> {
    fn new(half: Arc<H>, handle: tokio::runtime::Handle) -> Self {
        Self { half, handle }
    }
}

#[cfg(any(windows, test))]
impl<H: Half> Read for BlockingReader<H> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if buf.is_empty() {
            return Ok(0);
        }
        self.handle.block_on(self.half.read_some(buf))
    }
}

/// Reads the connection on a thread of its own, only to see its end: the end of the
/// stream, an error other than an interrupted or would-block read, or any byte at all is
/// the owner gone. It arms the watchdog for the owner-loss bound and sends
/// [`Input::OwnerLost`], once. What was read is never parsed, kept or logged.
pub fn watch_end(
    mut end: impl Read + Send + 'static,
    inputs: mpsc::UnboundedSender<Input>,
    watchdog: Watchdog,
) {
    std::thread::spawn(move || {
        let mut sink = [0_u8; 64];
        loop {
            match end.read(&mut sink) {
                // The end, or bytes the owner never sends.
                Ok(_) => break,
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {}
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(10));
                }
                Err(_) => break,
            }
        }
        log::warn!("managed: the events endpoint ended or was written to; the owner is gone");
        watchdog.arm(Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS));
        let _ = inputs.send(Input::OwnerLost);
    });
}

#[cfg(test)]
mod unsupported_tests {
    use super::*;

    #[test]
    fn the_non_unix_answer_is_typed_names_what_is_missing_and_exits_2() {
        let error = unsupported();
        assert!(matches!(error, Error::Unsupported { .. }));
        let text = error.to_string();
        assert!(text.contains("--events-pipe"), "{text}");
        assert!(text.contains("not supported on this platform"), "{text}");
        assert_eq!(error.exit_code(), 2);
    }
}

/// The Windows half's pieces that are plain logic, run on every platform: the name check,
/// the retry, and the synchronous bridges over a fake connection. The pipe itself is run by
/// `tests/managed_windows.rs`, on Windows only.
#[cfg(test)]
mod bridge_tests {
    use std::collections::VecDeque;
    use std::io::ErrorKind;
    use std::sync::Mutex;

    use super::*;

    /// A connection that says what it is told: what was written to it, in what pieces, and
    /// what it answers to reads and writes.
    #[derive(Default)]
    struct Fake {
        written: Mutex<Vec<u8>>,
        /// The most one write takes, to see that the bridge's `write_all` goes on.
        piece: Option<usize>,
        write_error: Option<ErrorKind>,
        /// A write that never finishes.
        write_hangs: bool,
        reads: Mutex<VecDeque<std::io::Result<Vec<u8>>>>,
    }

    impl Half for Fake {
        async fn write_some(&self, buf: &[u8]) -> std::io::Result<usize> {
            if self.write_hangs {
                std::future::pending::<()>().await;
            }
            if let Some(kind) = self.write_error {
                return Err(kind.into());
            }
            let take = self.piece.map_or(buf.len(), |piece| piece.min(buf.len()));
            self.written.lock().unwrap().extend_from_slice(&buf[..take]);
            Ok(take)
        }

        async fn read_some(&self, buf: &mut [u8]) -> std::io::Result<usize> {
            let next = self.reads.lock().unwrap().pop_front();
            match next {
                Some(Ok(bytes)) => {
                    buf[..bytes.len()].copy_from_slice(&bytes);
                    Ok(bytes.len())
                }
                Some(Err(error)) => Err(error),
                None => std::future::pending().await,
            }
        }
    }

    fn handle() -> tokio::runtime::Handle {
        tokio::runtime::Handle::current()
    }

    fn refusal(result: Result<(), Error>) -> &'static str {
        match result {
            Err(Error::Attach(reason)) => reason,
            other => panic!("not an attach refusal: {other:?}"),
        }
    }

    #[test]
    fn a_name_of_200_bytes_is_the_longest_opened() {
        let long = format!(r"\\.\pipe\{}", "a".repeat(200));
        assert!(require_pipe_name(Path::new(&long)).is_ok());
        let longer = format!("{long}a");
        assert!(require_pipe_name(Path::new(&longer)).is_err());
        assert!(require_pipe_name(Path::new(r"\\.\pipe\under_score-9")).is_ok());
    }

    #[test]
    fn only_a_name_in_the_local_pipe_namespace_is_opened() {
        for good in [
            r"\\.\pipe\conch-voice-0123456789abcdef0123456789abcdef",
            r"\\.\pipe\x",
            // The namespace is not case sensitive.
            r"\\.\PIPE\x",
        ] {
            assert!(require_pipe_name(Path::new(good)).is_ok(), "{good}");
        }
        for bad in [
            "",
            r"\\.\pipe\",
            r"\\host\pipe\x",
            r"\\?\pipe\x",
            r"\\.\pipe",
            r"\.\pipe\x",
            r"pipe\x",
            "conch-voice-x",
            "/run/user/1000/conch/p/e.sock",
            "@abstract",
            r"\\.\pipe\..",
            r"\\.\pipe\..\COM1",
            "\\\\.\\pipe\\x\0",
            r"\\.\pipe\...",
            r"\\.\pipe\. ",
            r"\\.\pipe\x.",
            "//./pipe/x",
            r"\\.\pipe\x\",
            r"\\.\pipe\a\b",
            r"\\.\pipe\a/b",
            r" \\.\pipe\x",
        ] {
            assert_eq!(
                refusal(require_pipe_name(Path::new(bad))),
                r"pipe name is not \\.\pipe\<name>",
                "{bad:?}"
            );
        }
    }

    fn busy() -> std::io::Error {
        std::io::Error::from_raw_os_error(ERROR_PIPE_BUSY)
    }

    #[test]
    fn a_busy_pipe_is_tried_again_every_interval_and_any_other_error_ends_it() {
        let every = Duration::from_millis(50);
        // (what each try says, how many sleeps, the answer)
        type Case = (Vec<std::io::Result<u8>>, usize, Result<u8, &'static str>);
        let cases: Vec<Case> = vec![
            (vec![Ok(7)], 0, Ok(7)),
            (vec![Err(busy()), Ok(7)], 1, Ok(7)),
            (vec![Err(busy()), Err(busy()), Err(busy()), Ok(7)], 3, Ok(7)),
            (
                vec![Err(std::io::ErrorKind::NotFound.into())],
                0,
                Err("cannot open"),
            ),
            (
                vec![
                    Err(busy()),
                    Err(std::io::ErrorKind::PermissionDenied.into()),
                ],
                1,
                Err("cannot open"),
            ),
        ];
        for (steps, sleeps, want) in cases {
            let mut steps = steps.into_iter();
            let mut slept = Vec::new();
            let got = open_with_retry(
                || steps.next().expect("one more try than planned"),
                Instant::now() + Duration::from_secs(60),
                every,
                |wait| slept.push(wait),
            );
            match (got, want) {
                (Ok(got), Ok(want)) => assert_eq!(got, want),
                (Err(Error::Attach(got)), Err(want)) => assert_eq!(got, want),
                (got, want) => panic!("{got:?} is not {want:?}"),
            }
            assert_eq!(slept, vec![every; sleeps]);
        }
    }

    #[test]
    fn a_pipe_that_stays_busy_is_given_up_on_at_the_deadline() {
        let mut tries = 0;
        let began = Instant::now();
        let error = open_with_retry(
            || {
                tries += 1;
                Err::<(), _>(busy())
            },
            began + Duration::from_millis(120),
            PIPE_BUSY_RETRY,
            std::thread::sleep,
        )
        .unwrap_err();
        assert!(
            matches!(error, Error::Attach("cannot open in time")),
            "{error}"
        );
        assert_eq!(error.exit_code(), 2);
        assert!(tries >= 2, "{tries} tries");
        assert!(began.elapsed() >= Duration::from_millis(120));
        assert!(began.elapsed() < Duration::from_secs(5));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_write_bridge_reaches_the_half_in_order_in_whatever_pieces_it_takes() {
        let half = Arc::new(Fake {
            piece: Some(3),
            ..Fake::default()
        });
        let kept = Arc::clone(&half);
        let handle = handle();
        tokio::task::spawn_blocking(move || {
            let mut writer = BlockingWriter::new(half, handle);
            writer.write_all(b"first line\n").unwrap();
            writer.write_all(b"").unwrap();
            writer.write_all(b"second\n").unwrap();
            writer.flush().unwrap();
        })
        .await
        .unwrap();
        assert_eq!(&*kept.written.lock().unwrap(), b"first line\nsecond\n");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_error_from_the_half_is_the_writers_io_error() {
        let handle = handle();
        let kinds = tokio::task::spawn_blocking(move || {
            let mut kinds = Vec::new();
            for kind in [ErrorKind::BrokenPipe, ErrorKind::ConnectionReset] {
                let half = Arc::new(Fake {
                    write_error: Some(kind),
                    ..Fake::default()
                });
                let mut writer = BlockingWriter::new(half, handle.clone());
                kinds.push(writer.write_all(b"x").unwrap_err().kind());
            }
            kinds
        })
        .await
        .unwrap();
        assert_eq!(kinds, [ErrorKind::BrokenPipe, ErrorKind::ConnectionReset]);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_write_that_never_finishes_is_a_timeout_only_while_there_is_a_limit() {
        let half = Arc::new(Fake {
            write_hangs: true,
            ..Fake::default()
        });
        let handle = handle();
        let kind = tokio::task::spawn_blocking(move || {
            let mut writer = BlockingWriter::new(half, handle);
            writer.limit = Some(Duration::from_millis(50));
            writer.write_all(b"x").unwrap_err().kind()
        })
        .await
        .unwrap();
        assert_eq!(kind, ErrorKind::TimedOut);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_attach_line_through_the_bridge_is_one_line_and_the_nonce_is_only_in_it() {
        let half = Arc::new(Fake::default());
        let kept = Arc::clone(&half);
        let handle = handle();
        tokio::task::spawn_blocking(move || {
            let mut writer = BlockingWriter::new(half, handle);
            write_attach(&mut writer, &Secret::new(FAKE_NONCE)).unwrap();
        })
        .await
        .unwrap();
        let wrote = kept.written.lock().unwrap().clone();
        assert_eq!(wrote.iter().filter(|byte| **byte == b'\n').count(), 1);
        assert_eq!(wrote.last(), Some(&b'\n'));
        let attached = conch_voice_api::parse_attached(&wrote[..wrote.len() - 1]).unwrap();
        assert!(attached.matches(FAKE_NONCE));
    }

    /// A fake nonce from `voice-control-pipe/sensitive.json`.
    const FAKE_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

    /// What the read side makes of one answer from the connection: the owner is gone. The
    /// end, an error and a byte are each that, through the bridge and the watch.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_connections_end_an_error_or_a_byte_is_owner_loss_once() {
        let steps: [std::io::Result<Vec<u8>>; 4] = [
            Ok(Vec::new()),
            Err(ErrorKind::BrokenPipe.into()),
            Err(ErrorKind::ConnectionReset.into()),
            Ok(vec![b'x']),
        ];
        for step in steps {
            let half = Arc::new(Fake::default());
            half.reads.lock().unwrap().push_back(step);
            let (inputs, mut received) = mpsc::unbounded_channel();
            watch_end(BlockingReader::new(half, handle()), inputs, Watchdog::off());
            let got = tokio::time::timeout(Duration::from_secs(5), received.recv()).await;
            assert!(matches!(got, Ok(Some(Input::OwnerLost))), "{got:?}");
            // The sender went with the thread: nothing more can come.
            assert!(received.recv().await.is_none());
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_quiet_connection_is_not_owner_loss() {
        let half = Arc::new(Fake::default());
        let (inputs, mut received) = mpsc::unbounded_channel();
        watch_end(BlockingReader::new(half, handle()), inputs, Watchdog::off());
        let got = tokio::time::timeout(Duration::from_millis(300), received.recv()).await;
        assert!(got.is_err(), "{got:?}");
    }

    /// The second owner loss of the design (§3.6): the events cannot be written. The bridge
    /// is what `managed::Events` is given, as `lib.rs` gives it, and its failure arrives as
    /// [`Input::OwnerLost`].
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_failed_events_write_through_the_bridge_is_owner_loss() {
        for kind in [
            ErrorKind::BrokenPipe,
            ErrorKind::ConnectionReset,
            ErrorKind::Other,
        ] {
            let half = Arc::new(Fake {
                write_error: Some(kind),
                ..Fake::default()
            });
            let (inputs, mut received) = mpsc::unbounded_channel();
            let mut events = crate::managed::Events::spawn(
                Box::new(BlockingWriter::new(half, handle())),
                inputs,
                Watchdog::off(),
            );
            events.hello();
            let got = tokio::time::timeout(Duration::from_secs(5), received.recv()).await;
            assert!(
                matches!(got, Ok(Some(Input::OwnerLost))),
                "{kind:?}: {got:?}"
            );
        }
    }

    /// The options the pipe is opened with on Windows: read and write, and tokio's default
    /// for everything else, the identification-level `security_qos_flags` among it.
    #[cfg(windows)]
    #[test]
    fn the_pipe_is_opened_for_read_and_write_with_the_default_security_qos() {
        use tokio::net::windows::named_pipe::ClientOptions;
        let ours = format!("{:?}", client_options());
        let expected = format!("{:?}", ClientOptions::new().read(true).write(true));
        assert_eq!(ours, expected);
        assert!(ours.contains("generic_read: true"), "{ours}");
        assert!(ours.contains("generic_write: true"), "{ours}");
        // SECURITY_SQOS_PRESENT (0x0010_0000) | SECURITY_IDENTIFICATION (0x0001_0000): the
        // default, which nothing here overrides. A tokio that changed it would fail here.
        let flags = format!("security_qos_flags: {}", 0x0011_0000_u32);
        assert!(ours.contains(&flags), "{ours}");
    }
}

/// [`attach`] against a real named pipe, a tokio server standing in for the owner's. Run by
/// the `voice-windows` job; nothing here compiles anywhere else.
#[cfg(all(test, windows))]
mod pipe_tests {
    use std::sync::atomic::{AtomicU32, Ordering};

    use conch_voice_api::{VOICE_CONTROL_HELLO_TIMEOUT_MS, parse_attached};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::windows::named_pipe::{ClientOptions, NamedPipeServer, ServerOptions};

    use super::*;

    /// A fake nonce from `voice-control-pipe/sensitive.json`.
    const FAKE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

    fn environment() -> Environment {
        Environment {
            conch_voice_attach: Some(Secret::new(FAKE)),
            ..Environment::default()
        }
    }

    fn pipe_name() -> String {
        static NEXT: AtomicU32 = AtomicU32::new(0);
        format!(
            r"\\.\pipe\conch-voice-unit-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        )
    }

    fn server(name: &str, first: bool) -> NamedPipeServer {
        ServerOptions::new()
            .first_pipe_instance(first)
            .create(name)
            .unwrap()
    }

    /// [`attach`] on a thread that is not the runtime's, as `managed()` calls it.
    async fn attach_on_a_thread(name: String) -> Result<Endpoint, Error> {
        tokio::task::spawn_blocking(move || {
            attach(Path::new(&name), &environment(), &Scrubber::new())
        })
        .await
        .unwrap()
    }

    async fn read_line(server: &mut NamedPipeServer) -> Vec<u8> {
        let mut got = Vec::new();
        let mut byte = [0_u8; 1];
        while server.read(&mut byte).await.unwrap() == 1 {
            got.push(byte[0]);
            if byte[0] == b'\n' {
                break;
            }
        }
        got
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn attach_writes_the_attach_line_first_and_hands_back_both_sides() {
        let name = pipe_name();
        let mut owner = server(&name, true);
        let (endpoint, ()) = tokio::join!(
            async { attach_on_a_thread(name.clone()).await.unwrap() },
            async { owner.connect().await.unwrap() }
        );
        let line = read_line(&mut owner).await;
        assert!(
            parse_attached(&line[..line.len() - 1])
                .unwrap()
                .matches(FAKE)
        );
        // The events side writes after it, and the owner reads it.
        let Endpoint { mut events, end } = endpoint;
        tokio::task::spawn_blocking(move || {
            events.write_all(b"{\"kind\":\"hello\"}\n").unwrap();
            drop(end);
        })
        .await
        .unwrap();
        assert_eq!(read_line(&mut owner).await, b"{\"kind\":\"hello\"}\n");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_busy_pipe_is_retried_by_attach_until_an_instance_is_free() {
        let name = pipe_name();
        let first = server(&name, true);
        let _squatter = ClientOptions::new().open(&name).unwrap();
        first.connect().await.unwrap();
        let attaching = tokio::spawn(attach_on_a_thread(name.clone()));
        tokio::time::sleep(Duration::from_millis(300)).await;
        assert!(!attaching.is_finished(), "gave up on a busy pipe");
        let mut second = server(&name, false);
        second.connect().await.unwrap();
        assert!(attaching.await.unwrap().is_ok());
        let line = read_line(&mut second).await;
        assert!(parse_attached(&line[..line.len() - 1]).is_ok());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_pipe_that_stays_busy_is_refused_at_the_hello_time() {
        let name = pipe_name();
        let first = server(&name, true);
        let _squatter = ClientOptions::new().open(&name).unwrap();
        first.connect().await.unwrap();
        let began = Instant::now();
        let error = attach_on_a_thread(name).await.unwrap_err();
        assert!(
            matches!(error, Error::Attach("cannot open in time")),
            "{error}"
        );
        assert!(began.elapsed() >= Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS - 200));
    }

    /// The reason for tokio over `std::fs` (design §3.3): events keep flowing for a minute
    /// while a read for the owner's end is pending on the same handle, and then the owner's
    /// end closing reaches the read.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn events_flow_for_a_minute_while_a_read_for_the_end_is_pending_and_then_the_end_arrives()
    {
        let name = pipe_name();
        let mut owner = server(&name, true);
        let (endpoint, ()) = tokio::join!(
            async { attach_on_a_thread(name.clone()).await.unwrap() },
            async { owner.connect().await.unwrap() }
        );
        read_line(&mut owner).await;
        let Endpoint { mut events, end } = endpoint;
        // The read for the end, pending from now on, on its own thread.
        let (inputs, mut received) = mpsc::unbounded_channel();
        watch_end(end, inputs, Watchdog::off());
        let writer = tokio::task::spawn_blocking(move || {
            for index in 0_u32..120 {
                events.write_all(format!("{index}\n").as_bytes()).unwrap();
                std::thread::sleep(Duration::from_millis(500));
            }
        });
        let reader = tokio::spawn(async move {
            let mut lines = 0_u32;
            let mut pending = Vec::new();
            let mut chunk = [0_u8; 256];
            while lines < 120 {
                let read = owner.read(&mut chunk).await.unwrap();
                assert_ne!(read, 0);
                pending.extend_from_slice(&chunk[..read]);
                lines =
                    u32::try_from(pending.iter().filter(|byte| **byte == b'\n').count()).unwrap();
            }
            // Every write arrived, in order, while the end was being read; now it closes.
            let text = String::from_utf8(pending).unwrap();
            let want: Vec<String> = (0..120).map(|index| index.to_string()).collect();
            assert_eq!(text.lines().collect::<Vec<_>>(), want);
            drop(owner);
        });
        writer.await.unwrap();
        reader.await.unwrap();
        let lost = tokio::time::timeout(Duration::from_secs(3), received.recv()).await;
        assert!(matches!(lost, Ok(Some(Input::OwnerLost))), "{lost:?}");
    }

    /// A byte from the owner reaches the read for the end as owner loss.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_byte_written_by_the_owner_is_owner_loss() {
        let name = pipe_name();
        let mut owner = server(&name, true);
        let (endpoint, ()) = tokio::join!(
            async { attach_on_a_thread(name.clone()).await.unwrap() },
            async { owner.connect().await.unwrap() }
        );
        let (inputs, mut received) = mpsc::unbounded_channel();
        watch_end(endpoint.end, inputs, Watchdog::off());
        owner.write_all(b"x").await.unwrap();
        let lost = tokio::time::timeout(Duration::from_secs(3), received.recv()).await;
        assert!(matches!(lost, Ok(Some(Input::OwnerLost))), "{lost:?}");
        drop(endpoint.events);
    }

    /// A write to a pipe whose owner has gone fails, which `Events` takes as owner loss.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_write_to_a_pipe_the_owner_closed_is_an_error() {
        let name = pipe_name();
        let owner = server(&name, true);
        let (endpoint, ()) = tokio::join!(
            async { attach_on_a_thread(name.clone()).await.unwrap() },
            async { owner.connect().await.unwrap() }
        );
        drop(owner);
        let Endpoint { mut events, end } = endpoint;
        let failed = tokio::task::spawn_blocking(move || {
            let _keep = end;
            (0..50).any(|_| {
                std::thread::sleep(Duration::from_millis(50));
                events.write_all(b"x\n").is_err()
            })
        })
        .await
        .unwrap();
        assert!(failed, "writes to a closed pipe went on succeeding");
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::os::unix::fs::PermissionsExt;
    use std::os::unix::net::UnixListener;
    use std::path::PathBuf;

    use conch_voice_api::{VOICE_CONTROL_PIPE_SCHEMA_V1, parse_attached};

    use super::*;

    /// A fake nonce from `voice-control-pipe/sensitive.json`.
    const FAKE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

    fn environment(nonce: Option<&str>) -> Environment {
        Environment {
            conch_voice_attach: nonce.map(Secret::new),
            ..Environment::default()
        }
    }

    /// A private `0o700` directory holding a bound socket, and the user's uid.
    struct Fixture {
        dir: tempfile::TempDir,
        socket: PathBuf,
        listener: UnixListener,
        uid: u32,
    }

    fn fixture() -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o700)).unwrap();
        let socket = dir.path().join("s");
        let listener = UnixListener::bind(&socket).unwrap();
        let uid = std::fs::metadata(dir.path()).unwrap().uid();
        Fixture {
            dir,
            socket,
            listener,
            uid,
        }
    }

    fn reason(result: Result<(), Error>) -> String {
        match result {
            Err(Error::Attach(reason)) => reason.to_owned(),
            other => panic!("not an attach refusal: {other:?}"),
        }
    }

    #[test]
    fn only_an_absolute_path_is_looked_at() {
        assert!(require_absolute(Path::new("/run/user/1000/conch/p/s")).is_ok());
        for bad in ["", "@abstract", "relative/s", "./s", "s", " /s"] {
            assert_eq!(
                reason(require_absolute(Path::new(bad))),
                "path not absolute",
                "{bad:?}"
            );
        }
    }

    #[test]
    fn the_nonce_must_be_there_and_well_formed_and_is_never_repeated() {
        let scrubber = Scrubber::new();
        for (value, said) in [
            (None, "no attach nonce"),
            (Some(""), "no attach nonce"),
            (
                Some("FAKE_short_nonce_do_not_print"),
                "attach nonce malformed",
            ),
            (
                // Right length, wrong last character: not the canonical encoding.
                Some("FAKE_nonce_do_not_print_0123456789abcdefghB"),
                "attach nonce malformed",
            ),
        ] {
            let environment = environment(value);
            let error = nonce(&environment, &scrubber).unwrap_err();
            assert!(
                matches!(error, Error::Attach(r) if r == said),
                "{value:?}: {error}"
            );
            if let Some(value) = value.filter(|value| !value.is_empty()) {
                assert!(!error.to_string().contains(value), "{error}");
                assert!(!format!("{error:?}").contains(value), "{error:?}");
                // Only a value of a nonce's length is scrubbed; a short one is no nonce.
                let scrubbed = scrubber.scrub(value);
                if value.len() == VOICE_CONTROL_PIPE_NONCE_CHARS {
                    assert_eq!(scrubbed, crate::secrets::REDACTED);
                } else {
                    assert_eq!(scrubbed, value);
                }
            }
        }
        let good = environment(Some(FAKE));
        assert_eq!(nonce(&good, &scrubber).unwrap().expose(), FAKE);
        assert_eq!(scrubber.scrub(&format!("x {FAKE} y")), "x [redacted] y");
    }

    #[test]
    fn a_socket_in_a_private_directory_of_ours_passes() {
        let fixture = fixture();
        check_endpoint(&fixture.socket, fixture.uid).unwrap();
        // The socket's own mode is not the boundary.
        std::fs::set_permissions(&fixture.socket, std::fs::Permissions::from_mode(0o666)).unwrap();
        check_endpoint(&fixture.socket, fixture.uid).unwrap();
    }

    #[test]
    fn each_broken_property_is_its_own_refusal() {
        let fixture = fixture();
        // Not owned: the same files, judged for another user.
        assert_eq!(
            reason(check_endpoint(&fixture.socket, fixture.uid.wrapping_add(1))),
            "parent not owned"
        );
        // A regular file in place of the socket.
        let file = fixture.dir.path().join("file");
        std::fs::write(&file, b"").unwrap();
        assert_eq!(reason(check_endpoint(&file, fixture.uid)), "not a socket");
        // A symlink to the socket is not a socket: nothing is followed.
        let link = fixture.dir.path().join("link");
        std::os::unix::fs::symlink(&fixture.socket, &link).unwrap();
        assert_eq!(reason(check_endpoint(&link, fixture.uid)), "not a socket");
        // Nothing there.
        assert_eq!(
            reason(check_endpoint(
                &fixture.dir.path().join("gone"),
                fixture.uid
            )),
            "cannot examine"
        );
        // The directory's mode, both ways from 0o700.
        for mode in [0o750, 0o755, 0o770, 0o777, 0o500, 0o600, 0o701] {
            std::fs::set_permissions(fixture.dir.path(), std::fs::Permissions::from_mode(mode))
                .unwrap();
            assert_eq!(
                reason(check_endpoint(&fixture.socket, fixture.uid)),
                "parent mode",
                "{mode:o}"
            );
        }
        std::fs::set_permissions(fixture.dir.path(), std::fs::Permissions::from_mode(0o700))
            .unwrap();
        check_endpoint(&fixture.socket, fixture.uid).unwrap();
        // A parent that is a symlink to a good directory.
        let behind = tempfile::tempdir().unwrap();
        std::fs::set_permissions(behind.path(), std::fs::Permissions::from_mode(0o700)).unwrap();
        let socket_behind = behind.path().join("s");
        let _listener = UnixListener::bind(&socket_behind).unwrap();
        let through = fixture.dir.path().join("through");
        std::os::unix::fs::symlink(behind.path(), &through).unwrap();
        check_endpoint(&socket_behind, fixture.uid).unwrap();
        assert_eq!(
            reason(check_endpoint(&through.join("s"), fixture.uid)),
            "parent is a symlink"
        );
        // A parent that is not a directory at all.
        assert_eq!(
            reason(check_endpoint(&file.join("s"), fixture.uid)),
            "parent not a directory"
        );
        // The root has no parent.
        assert_eq!(
            reason(check_endpoint(Path::new("/"), fixture.uid)),
            "parent not a directory"
        );
    }

    /// The directory's owner is its own refusal, before its mode: a socket of ours in a
    /// directory of someone else's (`/dev/shm` is root's) is `parent not owned`, though the
    /// socket is ours. Needs a writable `/dev/shm`; where there is none the case is skipped.
    #[test]
    fn a_directory_of_someone_elses_is_refused_though_the_socket_is_ours() {
        let shm = Path::new("/dev/shm");
        let Ok(root_owned) = std::fs::metadata(shm).map(|m| m.uid()) else {
            return;
        };
        let ours = std::fs::metadata("/proc/self").unwrap().uid();
        if root_owned == ours {
            return;
        }
        let socket = shm.join(format!("conch-voice-test-{}", std::process::id()));
        let Ok(_listener) = UnixListener::bind(&socket) else {
            return;
        };
        let result = check_endpoint(&socket, ours);
        let _ = std::fs::remove_file(&socket);
        assert_eq!(reason(result), "parent not owned");
    }

    #[test]
    fn a_connect_with_nobody_listening_fails_at_once() {
        let fixture = fixture();
        let Fixture {
            socket, listener, ..
        } = fixture;
        drop(listener);
        let began = std::time::Instant::now();
        let error = connect(&socket).unwrap_err();
        assert!(matches!(error, Error::Attach("cannot connect")), "{error}");
        assert!(began.elapsed() < Duration::from_secs(2));
    }

    #[test]
    fn the_attach_line_is_one_line_the_owner_can_read_and_nothing_else() {
        let (mut ours, mut theirs) = UnixStream::pair().unwrap();
        write_attach(&mut ours, &Secret::new(FAKE)).unwrap();
        drop(ours);
        let mut got = Vec::new();
        theirs.read_to_end(&mut got).unwrap();
        assert_eq!(got.iter().filter(|byte| **byte == b'\n').count(), 1);
        assert_eq!(got.last(), Some(&b'\n'));
        let attached = parse_attached(&got[..got.len() - 1]).unwrap();
        assert_eq!(attached.schema, VOICE_CONTROL_PIPE_SCHEMA_V1);
        assert!(attached.matches(FAKE));
    }

    #[test]
    fn an_attach_line_that_cannot_be_written_is_a_refusal_that_does_not_repeat_the_nonce() {
        let (mut ours, theirs) = UnixStream::pair().unwrap();
        drop(theirs);
        let error = write_attach(&mut ours, &Secret::new(FAKE)).unwrap_err();
        assert!(
            matches!(error, Error::Attach("attach not written")),
            "{error}"
        );
        assert!(!error.to_string().contains(FAKE));
        assert!(!format!("{error:?}").contains(FAKE));
    }

    #[test]
    fn attach_attaches_with_the_line_first_and_hands_back_both_handles() {
        let fixture = fixture();
        let scrubber = Scrubber::new();
        let socket = fixture.socket.clone();
        let owner = std::thread::spawn(move || {
            let (mut peer, _) = fixture.listener.accept().unwrap();
            let mut got = Vec::new();
            let mut byte = [0_u8; 1];
            while peer.read(&mut byte).unwrap() == 1 {
                got.push(byte[0]);
                if byte[0] == b'\n' {
                    break;
                }
            }
            got
        });
        let endpoint = attach(&socket, &environment(Some(FAKE)), &scrubber).unwrap();
        let got = owner.join().unwrap();
        assert!(parse_attached(&got[..got.len() - 1]).unwrap().matches(FAKE));
        drop(endpoint);
    }

    #[test]
    fn attach_stops_at_the_first_refusal_without_connecting() {
        let fixture = fixture();
        fixture.listener.set_nonblocking(true).unwrap();
        let scrubber = Scrubber::new();
        let wrong_mode = std::fs::Permissions::from_mode(0o755);
        std::fs::set_permissions(fixture.dir.path(), wrong_mode).unwrap();
        for (path, nonce_text) in [
            (PathBuf::from("relative"), Some(FAKE)),
            (fixture.socket.clone(), None),
            (fixture.socket.clone(), Some(FAKE)),
        ] {
            let error = attach(&path, &environment(nonce_text), &scrubber).unwrap_err();
            assert!(matches!(error, Error::Attach(_)), "{error}");
            assert_eq!(error.exit_code(), 2);
            assert!(!error.to_string().contains(FAKE));
        }
        assert!(
            fixture.listener.accept().is_err(),
            "nothing connected to the listener"
        );
    }

    /// A reader that says what it is told, to see what ends the watch.
    struct Script(std::vec::IntoIter<std::io::Result<Vec<u8>>>);

    impl Read for Script {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            match self.0.next() {
                Some(Ok(bytes)) => {
                    buf[..bytes.len()].copy_from_slice(&bytes);
                    Ok(bytes.len())
                }
                Some(Err(error)) => Err(error),
                None => {
                    std::thread::sleep(Duration::from_secs(60));
                    Ok(0)
                }
            }
        }
    }

    async fn lost_after(steps: Vec<std::io::Result<Vec<u8>>>) -> Option<Input> {
        let (inputs, mut received) = mpsc::unbounded_channel();
        watch_end(Script(steps.into_iter()), inputs, Watchdog::off());
        tokio::time::timeout(Duration::from_millis(400), received.recv())
            .await
            .ok()
            .flatten()
    }

    #[tokio::test]
    async fn the_end_an_error_or_any_byte_is_the_owner_gone() {
        use std::io::ErrorKind;
        for steps in [
            vec![Ok(Vec::new())],
            vec![Err(std::io::Error::from(ErrorKind::ConnectionReset))],
            vec![Ok(vec![b'x'])],
            // An interrupted read is read again, and then the end is the end.
            vec![
                Err(std::io::Error::from(ErrorKind::Interrupted)),
                Ok(Vec::new()),
            ],
            vec![
                Err(std::io::Error::from(ErrorKind::WouldBlock)),
                Ok(vec![0]),
            ],
        ] {
            let got = lost_after(steps).await;
            assert!(matches!(got, Some(Input::OwnerLost)), "{got:?}");
        }
    }

    #[tokio::test]
    async fn an_interrupted_or_would_block_read_alone_is_not_the_owner_gone() {
        use std::io::ErrorKind;
        let got = lost_after(vec![
            Err(std::io::Error::from(ErrorKind::Interrupted)),
            Err(std::io::Error::from(ErrorKind::WouldBlock)),
        ])
        .await;
        assert!(got.is_none(), "{got:?}");
    }

    #[tokio::test]
    async fn the_loss_is_sent_once() {
        let (inputs, mut received) = mpsc::unbounded_channel();
        watch_end(
            Script(vec![Ok(vec![1, 2, 3])].into_iter()),
            inputs,
            Watchdog::off(),
        );
        assert!(matches!(received.recv().await, Some(Input::OwnerLost)));
        // The sender went with the thread: nothing more can come.
        assert!(received.recv().await.is_none());
    }
}
