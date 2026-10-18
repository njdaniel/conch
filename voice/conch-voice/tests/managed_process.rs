//! `conch-voice managed` as a process (`docs/design/tui-voice.md` §2, §8): its event pipe
//! on file descriptor 3, its exit codes, and that it ends within its bounds when its owner
//! goes, by the end of its standard input or by its parent dying, with the key device
//! being read.
//!
//! No test here needs LiveKit, PipeWire or a keyboard: `conchd` is the stub, every session
//! it is asked for is refused for now (503 `voice_unavailable`), so the helper is
//! reconnecting by its policy; the microphone and speakers are the test options; the
//! keyboard is a named pipe the test holds open. Every run has a private `HOME`,
//! `XDG_CONFIG_HOME` and `CONCH_CONFIG_DIR`.
//!
//! File descriptor 3 is set by a shell (`sh -c '... 3>...'`), as `conch` sets it with
//! `ExtraFiles`: a test process may hold descriptors of its own (cargo's job server among
//! them), and one must never be handed to the helper by accident.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use conch_voice_api::{
    VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_OWNER_POLL_MS, VoiceControlEvent,
    VoiceControlPhase, VoiceControlReason, parse_event,
};
use serde_json::json;

use support::keyboard::Pipe;
use support::managed::KEYS_TOML;
use support::stub::{FAKE_JOIN, FAKE_LOGIN, FAKE_ROOM, Reply, Stub, whoami};

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// The test options: no PipeWire.
const NO_DEVICES: [&str; 4] = ["--mic", "none", "--sink", "count"];

/// What file descriptor 3 is when the helper starts.
enum Fd3 {
    Closed,
    /// A regular file at this path.
    File(PathBuf),
    /// A named pipe at this path, which the test reads.
    Pipe(PathBuf),
}

/// A running helper: its owner's ends of its pipes, and what it wrote.
struct Helper {
    child: Child,
    stdin: Option<ChildStdin>,
    events: Arc<Mutex<Vec<String>>>,
    /// Set when the event pipe reached its end: every writer of it has gone.
    events_ended: Arc<Mutex<Option<Instant>>>,
    /// Filled when standard error reached its end, by `stderr_reader`.
    stderr: Arc<Mutex<String>>,
    /// The thread that reads standard error; taken when it is waited for.
    stderr_reader: Mutex<Option<JoinHandle<()>>>,
}

/// The private directories one run uses, and what is in them.
struct Private {
    dir: tempfile::TempDir,
}

impl Private {
    fn new(voice_toml: Option<&str>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let config = dir.path().join("conch");
        std::fs::create_dir_all(&config).unwrap();
        if let Some(text) = voice_toml {
            let path = config.join("voice.toml");
            std::fs::write(&path, text).unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        Self { dir }
    }

    fn path(&self) -> &Path {
        self.dir.path()
    }

    fn events(&self) -> PathBuf {
        let path = self.path().join("events");
        let made = Command::new("mkfifo").arg(&path).status().unwrap();
        assert!(made.success());
        path
    }
}

/// The shell line that starts the helper with file descriptor 3 as asked. `$0` is the
/// binary, `$1` what fd 3 is to be, the rest the helper's options.
fn shell_line(fd3: &Fd3, background: bool) -> String {
    let redirect = match fd3 {
        Fd3::Closed => "3>&-",
        Fd3::File(_) | Fd3::Pipe(_) => "3>\"$f\"",
    };
    if background {
        // The helper's standard input is the shell's, through fd 4 (an asynchronous
        // command's own would be /dev/null). The shell then becomes a `sleep`, in the same
        // process, which the test kills: the helper's parent dies.
        format!(
            "f=$1; shift; exec 4<&0; \"$0\" managed \"$@\" {redirect} <&4 4<&- & exec 4<&-; exec sleep 600"
        )
    } else {
        format!("f=$1; shift; exec \"$0\" managed \"$@\" {redirect}")
    }
}

fn spawn(private: &Private, fd3: Fd3, args: &[&str], background: bool) -> Helper {
    let target = match &fd3 {
        Fd3::Closed => String::new(),
        Fd3::File(path) | Fd3::Pipe(path) => path.display().to_string(),
    };
    let events = Arc::new(Mutex::new(Vec::new()));
    let events_ended = Arc::new(Mutex::new(None));
    if let Fd3::Pipe(path) = &fd3 {
        let (path, kept, ended) = (path.clone(), Arc::clone(&events), Arc::clone(&events_ended));
        std::thread::spawn(move || {
            // Waits for the shell to open the pipe to write, then reads to its end.
            let pipe = File::open(path).unwrap();
            for line in BufReader::new(pipe).lines().map_while(Result::ok) {
                kept.lock().unwrap().push(line);
            }
            *ended.lock().unwrap() = Some(Instant::now());
        });
    }
    let mut child = Command::new("/bin/sh")
        .arg("-c")
        .arg(shell_line(&fd3, background))
        .arg(BIN)
        .arg(&target)
        .args(args)
        .env_clear()
        .env("PATH", "/usr/bin:/bin")
        .env("HOME", private.path().join("home"))
        .env("XDG_CONFIG_HOME", private.path().join("config"))
        .env("CONCH_CONFIG_DIR", private.path().join("conch"))
        .env("CONCH_TOKEN", FAKE_LOGIN)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let stdin = child.stdin.take();
    let stderr = Arc::new(Mutex::new(String::new()));
    let (mut err, kept) = (child.stderr.take().unwrap(), Arc::clone(&stderr));
    let stderr_reader = std::thread::spawn(move || {
        let mut text = String::new();
        let _ = err.read_to_string(&mut text);
        kept.lock().unwrap().push_str(&text);
    });
    Helper {
        child,
        stdin,
        events,
        events_ended,
        stderr,
        stderr_reader: Mutex::new(Some(stderr_reader)),
    }
}

impl Helper {
    fn send(&mut self, line: &str) {
        let stdin = self.stdin.as_mut().unwrap();
        stdin.write_all(line.as_bytes()).unwrap();
        stdin.write_all(b"\n").unwrap();
        stdin.flush().unwrap();
    }

    fn close(&mut self) {
        self.stdin = None;
    }

    fn events(&self) -> Vec<VoiceControlEvent> {
        self.events
            .lock()
            .unwrap()
            .iter()
            .map(|line| {
                parse_event(line.as_bytes())
                    .unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}"))
            })
            .collect()
    }

    fn states(&self) -> Vec<(VoiceControlPhase, Option<VoiceControlReason>)> {
        self.events()
            .into_iter()
            .filter_map(|event| match event {
                VoiceControlEvent::State(state) => Some((state.phase, state.reason)),
                _ => None,
            })
            .collect()
    }

    fn until(&self, what: &str, met: impl Fn(&Helper) -> bool) {
        let deadline = Instant::now() + Duration::from_secs(60);
        while !met(self) {
            assert!(
                Instant::now() < deadline,
                "timed out waiting for {what}\nevents: {:?}\nstderr: {}",
                self.events.lock().unwrap(),
                self.stderr.lock().unwrap()
            );
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    /// Waits for the process to exit, for at most `within`.
    fn exit_within(&mut self, within: Duration) -> Option<i32> {
        let began = Instant::now();
        while began.elapsed() < within {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status.code();
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
        panic!(
            "the helper did not exit within {within:?}\nevents: {:?}\nstderr: {}",
            self.events.lock().unwrap(),
            self.stderr.lock().unwrap()
        );
    }

    /// Everything the helper wrote to standard error. The process having exited is not
    /// enough: the text is there once the reader has reached the end of the pipe, which it
    /// does a moment later, so it is waited for. The limit only keeps a test from hanging.
    fn stderr(&self) -> String {
        if let Some(reader) = self.stderr_reader.lock().unwrap().take() {
            let deadline = Instant::now() + Duration::from_secs(30);
            while !reader.is_finished() {
                assert!(
                    Instant::now() < deadline,
                    "standard error did not reach its end"
                );
                std::thread::sleep(Duration::from_millis(5));
            }
            reader.join().unwrap();
        }
        self.stderr.lock().unwrap().clone()
    }
}

fn start(conchd: &Stub) -> String {
    json!({
        "schema": "conch.voice_control.v1", "kind": "start", "id": 1,
        "server": conchd.url(), "channel": "general", "principal_id": 7,
        "credential": "env", "muted": false, "deafened": false,
    })
    .to_string()
}

/// A stub that refuses every session for now, so that a helper stays reconnecting.
async fn refusing_stub() -> Stub {
    let conchd = Stub::start().await;
    for _ in 0..200 {
        conchd.next_session(Reply::error(503, "voice_unavailable"));
    }
    conchd
}

fn secrets_absent(text: &str) {
    for secret in [FAKE_LOGIN, FAKE_JOIN, FAKE_ROOM] {
        assert!(!text.contains(secret), "{text}");
    }
}

#[test]
fn without_its_event_pipe_it_exits_2_having_done_nothing() {
    let private = Private::new(None);
    let file = private.path().join("not-a-pipe");
    std::fs::write(&file, b"").unwrap();
    for fd3 in [Fd3::Closed, Fd3::File(file.clone())] {
        let mut helper = spawn(&private, fd3, &NO_DEVICES, false);
        helper.send("{}");
        assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(2));
        let stderr = helper.stderr();
        assert_eq!(stderr.lines().count(), 1, "{stderr}");
        assert!(stderr.contains("file descriptor 3"), "{stderr}");
    }
    assert_eq!(
        std::fs::read(&file).unwrap(),
        b"",
        "nothing written to a file"
    );
}

#[test]
fn the_hello_comes_first_and_an_owner_that_never_starts_it_ends_it() {
    let private = Private::new(None);
    let events = private.events();
    let mut helper = spawn(&private, Fd3::Pipe(events), &NO_DEVICES, false);
    helper.until("the hello", |helper| !helper.events().is_empty());
    match &helper.events()[0] {
        VoiceControlEvent::Hello(hello) => {
            assert_eq!(hello.seq, 1);
            assert_eq!(hello.versions, ["conch.voice_control.v1"]);
            assert_eq!(
                hello.helper_version,
                format!("{} (48000 Hz, 10 ms frames)", env!("CARGO_PKG_VERSION"))
            );
        }
        other => panic!("the first event is {other:?}"),
    }
    // The owner speaks no version it offers: it closes standard input, having written
    // nothing.
    helper.close();
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(0));
    assert_eq!(helper.events().len(), 1, "nothing after the hello");
}

#[test]
fn a_bad_first_command_fails_before_anything_is_opened() {
    let private = Private::new(None);
    let events = private.events();
    let mut helper = spawn(&private, Fd3::Pipe(events), &NO_DEVICES, false);
    helper.send(r#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":1,"muted":true}"#);
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(1));
    let events = helper.events();
    assert_eq!(events.len(), 3, "{events:?}");
    assert!(
        matches!(&events[1], VoiceControlEvent::ProtocolError(e) if e.code.as_str() == "not_started")
    );
    assert_eq!(
        helper.states(),
        [(
            VoiceControlPhase::Failed,
            Some(VoiceControlReason::ProtocolError)
        )]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_identity_that_is_not_the_owners_fails_before_a_session_is_asked_for() {
    let conchd = Stub::start().await;
    conchd.next_whoami(whoami(8));
    let private = Private::new(None);
    let events = private.events();
    let line = start(&conchd);
    let (code, helper) = tokio::task::spawn_blocking(move || {
        let mut helper = spawn(&private, Fd3::Pipe(events), &NO_DEVICES, false);
        helper.send(&line);
        let code = helper.exit_within(Duration::from_secs(20));
        (code, helper)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(1));
    assert_eq!(
        helper.states(),
        [
            (VoiceControlPhase::Starting, None),
            (
                VoiceControlPhase::Failed,
                Some(VoiceControlReason::IdentityMismatch)
            ),
        ]
    );
    assert_eq!(conchd.whoami_requests(), 1);
    assert_eq!(conchd.session_requests(), 0);
    secrets_absent(&helper.stderr());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_end_of_standard_input_ends_it_within_its_bound_while_the_key_device_is_read() {
    let conchd = refusing_stub().await;
    let private = Private::new(Some(KEYS_TOML));
    let events = private.events();
    // The keyboard, held open for as long as the test runs: the watcher stays in its read.
    let keyboard = Pipe::new();
    let _held = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&keyboard.path)
        .unwrap();
    let mut args = NO_DEVICES.to_vec();
    let pipe = keyboard.path.display().to_string();
    args.extend(["--test-key-device", &pipe]);
    let args: Vec<String> = args.into_iter().map(str::to_owned).collect();
    let line = start(&conchd);
    let (code, took, helper) = tokio::task::spawn_blocking(move || {
        let args: Vec<&str> = args.iter().map(String::as_str).collect();
        let mut helper = spawn(&private, Fd3::Pipe(events), &args, false);
        helper.send(&line);
        helper.until("reconnecting with the key device read", |helper| {
            helper.events().iter().any(|event| {
                matches!(event, VoiceControlEvent::State(s)
                    if s.phase == VoiceControlPhase::Reconnecting && s.ptt.as_str() == "ready")
            })
        });
        let gone = Instant::now();
        helper.close();
        let code = helper.exit_within(Duration::from_secs(10));
        (code, gone.elapsed(), helper)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(0));
    assert!(
        took < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS),
        "exited {took:?} after its standard input ended"
    );
    eprintln!("standard input ended: the helper exited {took:?} after");
    let states = helper.states();
    assert_eq!(
        states[states.len() - 2..],
        [
            (
                VoiceControlPhase::Stopping,
                Some(VoiceControlReason::OwnerLost)
            ),
            (
                VoiceControlPhase::Stopped,
                Some(VoiceControlReason::OwnerLost)
            ),
        ]
    );
    secrets_absent(&helper.stderr());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_parent_that_dies_while_the_control_pipe_stays_open_is_noticed_and_it_ends() {
    let conchd = refusing_stub().await;
    let private = Private::new(None);
    let events = private.events();
    let line = start(&conchd);
    let (took, helper) = tokio::task::spawn_blocking(move || {
        let mut helper = spawn(&private, Fd3::Pipe(events), &NO_DEVICES, true);
        helper.send(&line);
        helper.until("reconnecting", |helper| {
            helper
                .states()
                .iter()
                .any(|(phase, _)| *phase == VoiceControlPhase::Reconnecting)
        });
        // The parent, a shell this test started, is killed. The test keeps the control
        // pipe open, as an owner's leaked descriptor would: only the parent changed.
        let killed = Instant::now();
        helper.child.kill().unwrap();
        helper.child.wait().unwrap();
        helper.until("the event pipe to end", |helper| {
            helper.events_ended.lock().unwrap().is_some()
        });
        let ended = helper.events_ended.lock().unwrap().unwrap();
        (ended.duration_since(killed), helper)
    })
    .await
    .unwrap();
    let bound =
        Duration::from_millis(VOICE_CONTROL_OWNER_POLL_MS + VOICE_CONTROL_OWNER_LOSS_EXIT_MS);
    assert!(took < bound, "ended {took:?} after its parent died");
    eprintln!("parent died: the helper ended {took:?} after");
    let states = helper.states();
    assert_eq!(
        states.last(),
        Some(&(
            VoiceControlPhase::Stopped,
            Some(VoiceControlReason::OwnerLost)
        ))
    );
    assert!(
        helper.stdin.is_some(),
        "the control pipe was open throughout"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sigterm_leaves_as_leave_does_with_reason_signal() {
    let conchd = refusing_stub().await;
    let private = Private::new(None);
    let events = private.events();
    let line = start(&conchd);
    let (code, helper) = tokio::task::spawn_blocking(move || {
        let mut helper = spawn(&private, Fd3::Pipe(events), &NO_DEVICES, false);
        helper.send(&line);
        helper.until("reconnecting", |helper| {
            helper
                .states()
                .iter()
                .any(|(phase, _)| *phase == VoiceControlPhase::Reconnecting)
        });
        // The shell exec'd the helper: the child is the helper itself.
        let status = Command::new("kill")
            .args(["-TERM", &helper.child.id().to_string()])
            .status()
            .unwrap();
        assert!(status.success());
        let code = helper.exit_within(Duration::from_secs(10));
        (code, helper)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(0));
    let states = helper.states();
    assert_eq!(
        states[states.len() - 2..],
        [
            (
                VoiceControlPhase::Stopping,
                Some(VoiceControlReason::Signal)
            ),
            (VoiceControlPhase::Stopped, Some(VoiceControlReason::Signal)),
        ]
    );
}
