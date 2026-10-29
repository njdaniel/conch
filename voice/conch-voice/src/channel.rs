//! The events endpoint of `conch-voice managed --events-pipe <path>`: a Unix socket the
//! desktop owner listens on, which the helper connects to by path instead of taking file
//! descriptor 3 (`docs/design/linux-desktop.md` §5.2, issue #937, the helper's half of L-F2).
//! An Electron owner cannot hand over a FIFO: libuv makes every `'pipe'` stdio entry a
//! `SOCK_STREAM` socketpair, which `managed::open_event_pipe` rightly refuses (§5.1).
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
//! helper's environment, and so the nonce, anyway. The invariant that makes it a matter of
//! confidentiality and availability and never of authority is that authority arrives only on
//! standard input. Another user cannot reach the socket: the `0o700` directory denies them
//! the search permission on the way to it.

use std::io::{Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::mpsc as std_mpsc;
use std::time::Duration;

use conch_voice_api::{
    Secret, VOICE_CONTROL_HELLO_TIMEOUT_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS,
    VoiceControlPipeAttachedV1, to_line, validate_nonce,
};
use tokio::sync::mpsc;

use crate::cli::Environment;
use crate::devices;
use crate::error::Error;
use crate::managed::Watchdog;
use crate::secrets::Scrubber;
use crate::session::Input;

/// The connection to the owner, as the helper holds it once it has attached.
#[derive(Debug)]
pub struct Endpoint {
    /// Where the events are written: the connection, after the attach line.
    pub events: UnixStream,
    /// Another handle on the same connection, for [`watch_end`] to read.
    pub end: UnixStream,
}

/// Whether the path is one this module will look at at all: it begins with `/`. A relative
/// path, an empty one and an abstract name (`@name`; on Linux an address whose first byte
/// is NUL, which a command line cannot hold) are refused, without anything being opened.
///
/// # Errors
///
/// [`Error::Attach`] with the reason.
pub fn require_absolute(path: &Path) -> Result<(), Error> {
    if path.as_os_str().as_bytes().first() == Some(&b'/') {
        Ok(())
    } else {
        Err(Error::Attach("path not absolute"))
    }
}

/// The nonce `environment` holds, if it is one: 43 characters of canonical unpadded
/// base64url. It is registered with `scrubber` as soon as it is seen, valid or not, so that
/// no text this process writes can repeat it. The reason for a refusal never repeats it.
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
    scrubber.always(secret);
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
pub fn connect(path: &Path) -> Result<UnixStream, Error> {
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
pub fn attach(
    path: &Path,
    environment: &Environment,
    scrubber: &Scrubber,
) -> Result<Endpoint, Error> {
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
                assert_eq!(scrubber.scrub(value), crate::secrets::REDACTED);
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
