//! `conch-voice managed` as a process (`docs/design/tui-voice.md` §2, §8): its event pipe
//! on file descriptor 3, its exit codes, and that it ends within its bounds when its owner
//! goes, by the end of its standard input or by its parent dying, with the key device
//! being read.
//!
//! The same for a v2 owner (`conch.voice_control.v2`, §20): the hello offers both
//! versions, and the start chooses the one every event after it is in.
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

use conch_voice::managed::V1_PUSH_TO_TALK;
use conch_voice_api::{
    VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_OWNER_POLL_MS, VoiceControlEvent,
    VoiceControlEventV2, VoiceControlPhase, VoiceControlReason, VoiceControlStateV2, parse_event,
    parse_event_v2,
};
use serde_json::json;

use support::keyboard::Pipe;
use support::managed::KEYS_TOML;
use support::stub::{FAKE_JOIN, FAKE_LOGIN, FAKE_ROOM, Reply, Stub, whoami};

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// The test options: no PipeWire.
const NO_DEVICES: [&str; 4] = ["--mic", "none", "--sink", "count"];

/// What the helper logs when the final state was not written in time (`Owner::finish`).
const FINAL_STATE_UNREAD: &str = "the final state was not read before leaving";

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
    spawn_env(private, fd3, args, background, &[])
}

/// `spawn`, with more of the environment: the helper's is cleared, so what it has is only
/// what is named here and below.
fn spawn_env(
    private: &Private,
    fd3: Fd3,
    args: &[&str],
    background: bool,
    envs: &[(&str, &str)],
) -> Helper {
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
        .envs(envs.iter().copied())
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

    /// Like `send`, for a helper that may already have exited: a write that
    /// fails with `BrokenPipe` (the helper is gone) is not an error. Any other
    /// failure still panics.
    fn send_to_a_helper_that_may_have_exited(&mut self, line: &str) {
        let stdin = self.stdin.as_mut().unwrap();
        let written = stdin
            .write_all(line.as_bytes())
            .and_then(|()| stdin.write_all(b"\n"))
            .and_then(|()| stdin.flush());
        if let Err(error) = written {
            assert_eq!(error.kind(), std::io::ErrorKind::BrokenPipe, "{error}");
        }
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
        // The helper exits 2 at once, so it may be gone before this write.
        helper.send_to_a_helper_that_may_have_exited("{}");
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
            // Both versions, in v1's frozen shape (docs/design/tui-voice.md §20.2).
            assert_eq!(
                hello.versions,
                ["conch.voice_control.v1", "conch.voice_control.v2"]
            );
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
    // The final state is best effort (`docs/design/tui-voice.md` §5, rule 11): the helper
    // gives it 250 ms to be written, and on a machine so loaded that its writer thread
    // is not scheduled in that time it leaves without it, and says so. The owner side
    // takes the end of the pipe as the helper ending. So a missing final state passes
    // only with that line in standard error: a helper that left it out when it could
    // have written it still fails here.
    let states = helper.states();
    let stopped = (
        VoiceControlPhase::Stopped,
        Some(VoiceControlReason::OwnerLost),
    );
    if states.last() != Some(&stopped) {
        let stderr = helper.stderr();
        assert!(
            stderr.contains(FINAL_STATE_UNREAD),
            "no final state, and the helper did not say it could not write it\nstates: {states:?}\nstderr: {stderr}"
        );
        assert_eq!(
            states.last(),
            Some(&(
                VoiceControlPhase::Stopping,
                Some(VoiceControlReason::OwnerLost)
            )),
            "stderr: {stderr}"
        );
        eprintln!("the final state was not read in time, as the helper said");
    }
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

// ------------------------------------------------------------- conch.voice_control.v2

/// The v2 events a helper wrote, read as a v2 owner reads them: the hello by v1's reader,
/// the rest by v2's (Go's `ParseVoiceControlEventV2` and its `Validate`). A line either
/// refuses fails the test.
fn events_v2(helper: &Helper) -> Vec<VoiceControlEventV2> {
    let lines = helper.events.lock().unwrap().clone();
    lines
        .iter()
        .enumerate()
        .filter_map(|(at, line)| {
            if at == 0 {
                assert!(
                    matches!(
                        parse_event(line.as_bytes()),
                        Ok(VoiceControlEvent::Hello(_))
                    ),
                    "the first event is not a hello: {line}"
                );
                return None;
            }
            Some(
                parse_event_v2(line.as_bytes())
                    .unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}")),
            )
        })
        .collect()
}

fn states_v2(helper: &Helper) -> Vec<VoiceControlStateV2> {
    events_v2(helper)
        .into_iter()
        .filter_map(|event| match event {
            VoiceControlEventV2::State(state) => Some(state),
            _ => None,
        })
        .collect()
}

/// The test options with the key device a named pipe the test holds open, and the pipe.
fn with_keys() -> (Vec<String>, Pipe, File) {
    let keyboard = Pipe::new();
    let held = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&keyboard.path)
        .unwrap();
    let mut args: Vec<String> = NO_DEVICES.iter().map(|arg| (*arg).to_owned()).collect();
    args.push("--test-key-device".to_owned());
    args.push(keyboard.path.display().to_string());
    (args, keyboard, held)
}

/// A v2 owner, as a process: the start in v2 makes every event after the hello v2, open
/// mode from `voice.toml` is in force and said, the speakers are said, and the end of
/// standard input ends the helper within its bound with the key device being read.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_v2_owner_gets_v2_events_and_the_end_of_standard_input_ends_it_in_open_mode() {
    let conchd = refusing_stub().await;
    let toml = format!("{KEYS_TOML}talk_mode = \"open\"\n");
    let private = Private::new(Some(&toml));
    let events = private.events();
    let (args, _keyboard, _held) = with_keys();
    let mut line: serde_json::Value = serde_json::from_str(&start(&conchd)).unwrap();
    line["schema"] = json!("conch.voice_control.v2");
    let line = line.to_string();
    let (code, took, helper) = tokio::task::spawn_blocking(move || {
        let args: Vec<&str> = args.iter().map(String::as_str).collect();
        let mut helper = spawn(&private, Fd3::Pipe(events), &args, false);
        helper.send(&line);
        helper.until(
            "reconnecting in open mode with the key device read",
            |helper| {
                states_v2(helper).iter().any(|s| {
                    s.phase == VoiceControlPhase::Reconnecting
                        && s.ptt.as_str() == "ready"
                        && s.talk_mode.as_str() == "open"
                })
            },
        );
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
    eprintln!("v2, open mode, standard input ended: the helper exited {took:?} after");
    let states = states_v2(&helper);
    // Push-to-talk while starting, before the configuration is read; open after.
    assert_eq!(states[0].phase, VoiceControlPhase::Starting);
    assert_eq!(states[0].talk_mode.as_str(), "push_to_talk");
    assert_eq!(states[0].playback.as_str(), "off");
    let reconnecting = states
        .iter()
        .find(|s| s.phase == VoiceControlPhase::Reconnecting)
        .unwrap();
    assert_eq!(
        reconnecting.playback.as_str(),
        "on",
        "the counting sink plays"
    );
    let last = &states[states.len() - 2..];
    assert_eq!(
        (last[0].phase, last[0].reason),
        (
            VoiceControlPhase::Stopping,
            Some(VoiceControlReason::OwnerLost)
        )
    );
    assert_eq!(
        (last[1].phase, last[1].reason, last[1].playback.as_str()),
        (
            VoiceControlPhase::Stopped,
            Some(VoiceControlReason::OwnerLost),
            "off"
        )
    );
    let stderr = helper.stderr();
    secrets_absent(&stderr);
    assert!(!stderr.contains(V1_PUSH_TO_TALK), "{stderr}");
}

/// A v1 owner of the same helper, with the same `voice.toml`, as a process: the session is
/// v1 as it has always been, push-to-talk, and the helper's log says so in one line.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_v1_owner_with_open_mode_configured_gets_v1_and_one_line_in_the_log() {
    let conchd = refusing_stub().await;
    let toml = format!("{KEYS_TOML}talk_mode = \"open\"\n");
    let private = Private::new(Some(&toml));
    let events = private.events();
    let (args, _keyboard, _held) = with_keys();
    let line = start(&conchd);
    let (code, helper) = tokio::task::spawn_blocking(move || {
        let args: Vec<&str> = args.iter().map(String::as_str).collect();
        let mut helper = spawn(&private, Fd3::Pipe(events), &args, false);
        helper.send(&line);
        helper.until("reconnecting with the key device read", |helper| {
            helper.events().iter().any(|event| {
                matches!(event, VoiceControlEvent::State(s)
                    if s.phase == VoiceControlPhase::Reconnecting && s.ptt.as_str() == "ready")
            })
        });
        helper.close();
        let code = helper.exit_within(Duration::from_secs(10));
        (code, helper)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(0));
    // Every event after the hello is v1's, with nothing of v2's in it.
    for line in helper.events.lock().unwrap().iter().skip(1) {
        assert!(line.contains("\"conch.voice_control.v1\""), "{line}");
        assert!(
            !line.contains("talk_mode") && !line.contains("playback"),
            "{line}"
        );
    }
    assert_eq!(
        helper.states().last(),
        Some(&(
            VoiceControlPhase::Stopped,
            Some(VoiceControlReason::OwnerLost)
        ))
    );
    let stderr = helper.stderr();
    assert_eq!(stderr.matches(V1_PUSH_TO_TALK).count(), 1, "{stderr}");
}

// -------------------------------------------- the events endpoint: --events-pipe (#937)
//
// The owner is a Unix socket in a private directory under a fake `XDG_RUNTIME_DIR`, held by
// the test: a stand-in for the desktop owner. No test here needs more than the others do.

use std::net::Shutdown;
use std::os::fd::OwnedFd;
use std::os::unix::net::{UnixListener, UnixStream};

use conch_voice_api::{VOICE_CONTROL_PIPE_SCHEMA_V1, parse_attached};

/// A fake nonce from `pkg/schema/testdata/voice-control-pipe/sensitive.json`.
const FAKE_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

/// The owner's side of the endpoint: a private runtime directory with the profile's
/// directory in it, and the socket bound.
struct Listening {
    run: PathBuf,
    dir: PathBuf,
    socket: PathBuf,
    listener: UnixListener,
}

fn private_dir(path: &Path) {
    std::fs::create_dir_all(path).unwrap();
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)).unwrap();
}

fn listening(private: &Private) -> Listening {
    let run = private.path().join("run");
    let dir = run.join("conch").join("profile");
    private_dir(&run);
    private_dir(&run.join("conch"));
    private_dir(&dir);
    let socket = dir.join("e.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    Listening {
        run,
        dir,
        socket,
        listener,
    }
}

/// What the owner has received, and the owner's end of the connection.
struct Wire {
    raw: Arc<Mutex<Vec<u8>>>,
    peer: Arc<Mutex<Option<UnixStream>>>,
}

impl Wire {
    /// The first line received: the attach line.
    fn attach_line(&self) -> Option<String> {
        let raw = self.raw.lock().unwrap();
        let end = raw.iter().position(|byte| *byte == b'\n')?;
        Some(String::from_utf8_lossy(&raw[..end]).into_owned())
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.raw.lock().unwrap()).into_owned()
    }

    fn peer(&self) -> UnixStream {
        self.peer
            .lock()
            .unwrap()
            .as_ref()
            .unwrap()
            .try_clone()
            .unwrap()
    }
}

/// Accepts the helper's connection on a thread, and files what arrives: the first line as
/// the attach line, every line after it as an event of `helper`'s.
fn serve(listener: UnixListener, helper: &Helper) -> Wire {
    let wire = Wire {
        raw: Arc::default(),
        peer: Arc::default(),
    };
    let (raw, peer) = (Arc::clone(&wire.raw), Arc::clone(&wire.peer));
    let (events, ended) = (Arc::clone(&helper.events), Arc::clone(&helper.events_ended));
    std::thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        *peer.lock().unwrap() = Some(stream.try_clone().unwrap());
        let mut chunk = [0_u8; 4096];
        loop {
            let read = match stream.read(&mut chunk) {
                Ok(0) | Err(_) => break,
                Ok(read) => read,
            };
            let mut raw = raw.lock().unwrap();
            raw.extend_from_slice(&chunk[..read]);
            let text = String::from_utf8_lossy(&raw);
            let mut lines: Vec<&str> = text.split('\n').collect();
            // The last piece is a line still being written, or nothing.
            lines.pop();
            *events.lock().unwrap() = lines
                .iter()
                .skip(1)
                .map(|line| (*line).to_owned())
                .collect();
        }
        *ended.lock().unwrap() = Some(Instant::now());
    });
    wire
}

/// A helper started to attach to `endpoint`, with the nonce in its environment as asked.
fn spawn_endpoint(
    private: &Private,
    run: &Path,
    endpoint: &str,
    nonce: Option<&str>,
    fd3: Fd3,
    args: &[&str],
) -> Helper {
    let mut line = vec!["--events-pipe", endpoint];
    line.extend(args);
    let run = run.display().to_string();
    let mut envs = vec![("XDG_RUNTIME_DIR", run.as_str())];
    if let Some(nonce) = nonce {
        envs.push(("CONCH_VOICE_ATTACH", nonce));
    }
    spawn_env(private, fd3, &line, false, &envs)
}

fn socket_str(socket: &Path) -> String {
    socket.display().to_string()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_attach_line_comes_first_then_the_hello_and_the_nonce_is_on_the_wire_exactly_once() {
    let conchd = refusing_stub().await;
    let private = Private::new(None);
    let owner = listening(&private);
    let line = start(&conchd);
    let (code, helper, wire) = tokio::task::spawn_blocking(move || {
        let mut args = NO_DEVICES.to_vec();
        args.extend(["--log-level", "trace"]);
        let mut helper = spawn_endpoint(
            &private,
            &owner.run,
            &socket_str(&owner.socket),
            Some(FAKE_NONCE),
            Fd3::Closed,
            &args,
        );
        let wire = serve(owner.listener, &helper);
        helper.send(&line);
        helper.until("reconnecting", |helper| {
            helper
                .states()
                .iter()
                .any(|(phase, _)| *phase == VoiceControlPhase::Reconnecting)
        });
        helper.close();
        let code = helper.exit_within(Duration::from_secs(10));
        // The connection reached its end: what the scan below reads includes the final
        // stopping and stopped lines.
        helper.until("the events to end", |helper| {
            helper.events_ended.lock().unwrap().is_some()
        });
        (code, helper, wire)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(0));
    // The first line on the connection is the attach line, and it is the owner's to read.
    let first = wire.attach_line().expect("an attach line");
    let attached = parse_attached(first.as_bytes()).unwrap();
    assert_eq!(attached.schema, VOICE_CONTROL_PIPE_SCHEMA_V1);
    assert!(attached.matches(FAKE_NONCE));
    // The second is the hello, seq 1; after it the session as on file descriptor 3.
    let events = helper.events();
    match &events[0] {
        VoiceControlEvent::Hello(hello) => assert_eq!(hello.seq, 1),
        other => panic!("the event after the attach line is {other:?}"),
    }
    let text = wire.text();
    assert_eq!(text.lines().next(), Some(first.as_str()));
    assert!(
        text.lines().nth(1).unwrap().contains("\"kind\":\"hello\""),
        "{text}"
    );
    assert_eq!(
        helper.states().last(),
        Some(&(
            VoiceControlPhase::Stopped,
            Some(VoiceControlReason::OwnerLost)
        ))
    );
    // The nonce leaves once, in line 1, and is nowhere else: not in an event, not in
    // standard error at the most talkative level.
    assert_eq!(text.matches(FAKE_NONCE).count(), 1, "{text}");
    for event in helper.events.lock().unwrap().iter() {
        assert!(
            !event.contains(FAKE_NONCE) && !event.contains("nonce"),
            "{event}"
        );
    }
    let stderr = helper.stderr();
    assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
    secrets_absent(&stderr);
}

/// What `--events-pipe` refuses before it connects, one reason each. Not owned (a socket of
/// another user's) needs a second user and is a unit test of `channel::check_endpoint`.
#[test]
fn each_precondition_failure_is_exit_2_with_its_own_reason_and_nothing_connected() {
    struct Case {
        name: &'static str,
        reason: &'static str,
        nonce: Option<&'static str>,
        /// Arranges the directory and says what to pass as the endpoint.
        endpoint: fn(&Listening) -> String,
    }
    let cases = [
        Case {
            name: "a relative path, and no nonce read first",
            reason: "path not absolute",
            nonce: None,
            endpoint: |_| "e.sock".to_owned(),
        },
        Case {
            name: "an abstract name",
            reason: "path not absolute",
            nonce: Some(FAKE_NONCE),
            endpoint: |_| "@conch-abstract".to_owned(),
        },
        Case {
            name: "an empty string",
            reason: "path not absolute",
            nonce: Some(FAKE_NONCE),
            endpoint: |_| String::new(),
        },
        Case {
            name: "no nonce",
            reason: "no attach nonce",
            nonce: None,
            endpoint: |owner| socket_str(&owner.socket),
        },
        Case {
            name: "an empty nonce",
            reason: "no attach nonce",
            nonce: Some(""),
            endpoint: |owner| socket_str(&owner.socket),
        },
        Case {
            name: "a nonce of the wrong length",
            reason: "attach nonce malformed",
            nonce: Some("FAKE_short_nonce_do_not_print"),
            endpoint: |owner| socket_str(&owner.socket),
        },
        Case {
            name: "a nonce that is not canonical",
            reason: "attach nonce malformed",
            nonce: Some("FAKE_nonce_do_not_print_0123456789abcdefghB"),
            endpoint: |owner| socket_str(&owner.socket),
        },
        Case {
            name: "a regular file in place of the socket",
            reason: "not a socket",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                let file = owner.dir.join("file");
                std::fs::write(&file, b"").unwrap();
                socket_str(&file)
            },
        },
        Case {
            name: "a symlink to the socket",
            reason: "not a socket",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                let link = owner.dir.join("link");
                std::os::unix::fs::symlink(&owner.socket, &link).unwrap();
                socket_str(&link)
            },
        },
        Case {
            name: "a directory others can search",
            reason: "parent mode",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                std::fs::set_permissions(&owner.dir, std::fs::Permissions::from_mode(0o750))
                    .unwrap();
                socket_str(&owner.socket)
            },
        },
        Case {
            name: "a directory the owner cannot search",
            reason: "parent mode",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                // 0o600 hides the socket from the helper's lstat: the mode is judged first.
                std::fs::set_permissions(&owner.dir, std::fs::Permissions::from_mode(0o600))
                    .unwrap();
                socket_str(&owner.socket)
            },
        },
        Case {
            name: "a parent that is a symlink",
            reason: "parent is a symlink",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                let through = owner.run.join("through");
                std::os::unix::fs::symlink(&owner.dir, &through).unwrap();
                socket_str(&through.join("e.sock"))
            },
        },
        Case {
            name: "a parent that is a file",
            reason: "parent not a directory",
            nonce: Some(FAKE_NONCE),
            endpoint: |owner| {
                let file = owner.dir.join("file");
                std::fs::write(&file, b"").unwrap();
                socket_str(&file.join("e.sock"))
            },
        },
    ];
    for case in cases {
        let private = Private::new(None);
        let owner = listening(&private);
        let endpoint = (case.endpoint)(&owner);
        let mut helper = spawn_endpoint(
            &private,
            &owner.run,
            &endpoint,
            case.nonce,
            Fd3::Closed,
            &NO_DEVICES,
        );
        helper.send_to_a_helper_that_may_have_exited("{}");
        assert_eq!(
            helper.exit_within(Duration::from_secs(10)),
            Some(2),
            "{}",
            case.name
        );
        let stderr = helper.stderr();
        assert_eq!(stderr.lines().count(), 1, "{}: {stderr}", case.name);
        assert!(stderr.contains(case.reason), "{}: {stderr}", case.name);
        assert!(
            stderr.contains("events endpoint"),
            "{}: {stderr}",
            case.name
        );
        assert!(!stderr.contains(FAKE_NONCE), "{}: {stderr}", case.name);
        secrets_absent(&stderr);
        owner.listener.set_nonblocking(true).unwrap();
        assert!(
            matches!(owner.listener.accept(), Err(e) if e.kind() == std::io::ErrorKind::WouldBlock),
            "{}: the helper connected",
            case.name
        );
    }
}

/// A one-character value is no nonce and is not scrubbed as one: the single refusal line
/// stays readable and says what is wrong with it.
#[test]
fn a_one_character_nonce_is_refused_in_one_readable_line() {
    let private = Private::new(None);
    let owner = listening(&private);
    let mut helper = spawn_endpoint(
        &private,
        &owner.run,
        &socket_str(&owner.socket),
        Some("e"),
        Fd3::Closed,
        &NO_DEVICES,
    );
    helper.send_to_a_helper_that_may_have_exited("{}");
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(2));
    let stderr = helper.stderr();
    assert_eq!(stderr.lines().count(), 1, "{stderr}");
    assert!(stderr.contains("nonce"), "{stderr}");
    assert!(stderr.contains("events endpoint"), "{stderr}");
    assert!(!stderr.contains("[redacted]"), "{stderr}");
}

/// The owner reads nothing: it accepts, shuts down its reading half and keeps the
/// descriptor open, so the helper's write fails with EPIPE. The helper's write can come
/// before the owner's shutdown (the connection is queued before it is accepted), so the
/// owner spins on a non-blocking accept to be first, and a run in which the write won is
/// tried again: the helper is exit 2 in the run that loses it, having done nothing else.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_attach_line_that_cannot_be_written_is_exit_2_having_done_nothing_else() {
    for attempt in 1..=40 {
        let conchd = refusing_stub().await;
        let line = start(&conchd);
        let (code, helper) = tokio::task::spawn_blocking(move || {
            let private = Private::new(None);
            let owner = listening(&private);
            owner.listener.set_nonblocking(true).unwrap();
            let mut helper = spawn_endpoint(
                &private,
                &owner.run,
                &socket_str(&owner.socket),
                Some(FAKE_NONCE),
                Fd3::Closed,
                &NO_DEVICES,
            );
            let deadline = Instant::now() + Duration::from_secs(10);
            let _held = loop {
                match owner.listener.accept() {
                    Ok((stream, _)) => {
                        stream.shutdown(Shutdown::Read).unwrap();
                        break stream;
                    }
                    Err(_) if Instant::now() < deadline => std::hint::spin_loop(),
                    Err(error) => panic!("the helper never connected: {error}"),
                }
            };
            helper.send_to_a_helper_that_may_have_exited(&line);
            // A run that lost the race has a session going: its owner ends it.
            std::thread::sleep(Duration::from_millis(300));
            if helper.child.try_wait().unwrap().is_none() {
                helper.close();
            }
            let code = helper.exit_within(Duration::from_secs(10));
            (code, helper)
        })
        .await
        .unwrap();
        if code != Some(2) {
            eprintln!("attempt {attempt}: the write won the race; trying again");
            continue;
        }
        let stderr = helper.stderr();
        assert_eq!(stderr.lines().count(), 1, "{stderr}");
        assert!(stderr.contains("attach not written"), "{stderr}");
        assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
        assert_eq!(conchd.whoami_requests(), 0, "no request to conchd");
        assert_eq!(conchd.session_requests(), 0, "no request to conchd");
        eprintln!("the attach write failed on attempt {attempt}");
        return;
    }
    panic!("the helper's attach write never failed in 40 tries");
}

#[test]
fn nobody_listening_is_exit_2_at_once_without_a_retry() {
    let private = Private::new(None);
    let owner = listening(&private);
    // The file stays, as a stale one does after a crash; the listener behind it is gone.
    drop(owner.listener);
    let began = Instant::now();
    let mut helper = spawn_endpoint(
        &private,
        &owner.run,
        &socket_str(&owner.socket),
        Some(FAKE_NONCE),
        Fd3::Closed,
        &NO_DEVICES,
    );
    helper.send_to_a_helper_that_may_have_exited("{}");
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(2));
    assert!(
        began.elapsed() < Duration::from_secs(4),
        "{:?}",
        began.elapsed()
    );
    let stderr = helper.stderr();
    assert_eq!(stderr.lines().count(), 1, "{stderr}");
    assert!(stderr.contains("cannot connect"), "{stderr}");
    assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
}

/// The owner is lost by the end of the connection or by a byte on it, with the key device
/// being read, and the helper ends within the contract's bound. The owner here keeps its
/// side's reading open, so it reads the final states.
async fn the_owner_is_lost_by(what: &'static str, act: fn(&Wire)) {
    let conchd = refusing_stub().await;
    let private = Private::new(Some(KEYS_TOML));
    let owner = listening(&private);
    let keyboard = Pipe::new();
    let _held = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&keyboard.path)
        .unwrap();
    let pipe = keyboard.path.display().to_string();
    let line = start(&conchd);
    let (code, took, helper, wire) = tokio::task::spawn_blocking(move || {
        let mut args = NO_DEVICES.to_vec();
        args.extend(["--test-key-device", &pipe]);
        let mut helper = spawn_endpoint(
            &private,
            &owner.run,
            &socket_str(&owner.socket),
            Some(FAKE_NONCE),
            Fd3::Closed,
            &args,
        );
        let wire = serve(owner.listener, &helper);
        helper.send(&line);
        helper.until("reconnecting with the key device read", |helper| {
            helper.events().iter().any(|event| {
                matches!(event, VoiceControlEvent::State(s)
                    if s.phase == VoiceControlPhase::Reconnecting && s.ptt.as_str() == "ready")
            })
        });
        let gone = Instant::now();
        act(&wire);
        let code = helper.exit_within(Duration::from_secs(10));
        (code, gone.elapsed(), helper, wire)
    })
    .await
    .unwrap();
    assert_eq!(code, Some(0), "{what}");
    assert!(
        took < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS),
        "{what}: exited {took:?} after"
    );
    eprintln!("{what}: the helper exited {took:?} after");
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
        ],
        "{what}"
    );
    assert!(
        helper.stdin.is_some(),
        "standard input was open throughout: it was the connection"
    );
    assert_eq!(wire.text().matches(FAKE_NONCE).count(), 1);
    secrets_absent(&helper.stderr());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_owner_ending_the_connection_ends_the_helper_within_its_bound_with_the_key_device_read()
{
    // Only the owner's writing half: it goes on reading, and so reads the final states.
    the_owner_is_lost_by("the owner ended its end", |wire| {
        wire.peer().shutdown(Shutdown::Write).unwrap();
    })
    .await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_single_byte_from_the_owner_ends_the_helper_within_its_bound_with_the_key_device_read() {
    the_owner_is_lost_by("the owner wrote one byte", |wire| {
        wire.peer().write_all(b"x").unwrap();
    })
    .await;
}

#[test]
fn a_descriptor_at_3_is_not_consulted_when_an_endpoint_is_given() {
    let private = Private::new(None);
    let owner = listening(&private);
    let unrelated = private.path().join("unrelated-fd3");
    std::fs::write(&unrelated, b"").unwrap();
    let mut helper = spawn_endpoint(
        &private,
        &owner.run,
        &socket_str(&owner.socket),
        Some(FAKE_NONCE),
        Fd3::File(unrelated.clone()),
        &NO_DEVICES,
    );
    let wire = serve(owner.listener, &helper);
    helper.until("the hello", |helper| !helper.events().is_empty());
    // The owner speaks no version it offers: standard input closes with nothing written.
    helper.close();
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(0));
    assert!(parse_attached(wire.attach_line().unwrap().as_bytes()).is_ok());
    assert_eq!(
        std::fs::read(&unrelated).unwrap(),
        b"",
        "fd 3 was written to"
    );
    assert_eq!(helper.events().len(), 1, "nothing after the hello");
}

#[test]
fn without_an_endpoint_the_attach_variable_is_not_read_and_file_descriptor_3_is_the_way() {
    // A value that no nonce rule would accept, in the environment of a helper that is
    // given file descriptor 3 as ever: it is not read, so it is not refused.
    let private = Private::new(None);
    let events = private.events();
    let mut helper = spawn_env(
        &private,
        Fd3::Pipe(events),
        &NO_DEVICES,
        false,
        &[("CONCH_VOICE_ATTACH", "FAKE_not_a_nonce_do_not_print")],
    );
    helper.until("the hello", |helper| !helper.events().is_empty());
    helper.close();
    assert_eq!(helper.exit_within(Duration::from_secs(10)), Some(0));
    assert_eq!(helper.events().len(), 1);
    let stderr = helper.stderr();
    assert!(
        !stderr.contains("endpoint") && !stderr.contains("FAKE_not"),
        "{stderr}"
    );
}

/// The finding of `docs/design/linux-desktop.md` §5.1: an Electron owner's fourth stdio
/// entry is a `SOCK_STREAM` socket, not a pipe, and the helper started that way without
/// `--events-pipe` refuses it having opened nothing.
#[test]
fn a_socket_stream_on_fd_3_without_an_endpoint_is_exit_2_not_a_pipe_having_done_nothing() {
    let private = Private::new(None);
    let (ours, theirs) = UnixStream::pair().unwrap();
    // The shell makes the socket (the helper's standard output) its fd 3 and gives the
    // helper /dev/null for the rest.
    let mut child = Command::new("/bin/sh")
        .arg("-c")
        .arg(r#"exec "$0" managed "$@" 3>&1 >/dev/null"#)
        .arg(BIN)
        .args(NO_DEVICES)
        .env_clear()
        .env("PATH", "/usr/bin:/bin")
        .env("HOME", private.path().join("home"))
        .env("XDG_CONFIG_HOME", private.path().join("config"))
        .env("CONCH_CONFIG_DIR", private.path().join("conch"))
        .env("CONCH_TOKEN", FAKE_LOGIN)
        .stdin(Stdio::null())
        .stdout(Stdio::from(OwnedFd::from(theirs)))
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut stderr = String::new();
    child
        .stderr
        .take()
        .unwrap()
        .read_to_string(&mut stderr)
        .unwrap();
    assert_eq!(child.wait().unwrap().code(), Some(2), "{stderr}");
    assert_eq!(stderr.lines().count(), 1, "{stderr}");
    assert!(
        stderr.contains("file descriptor 3 is not a pipe"),
        "{stderr}"
    );
    // Nothing was written to the socket: the other end sees its end and no byte.
    let mut ours = ours;
    let mut got = Vec::new();
    ours.read_to_end(&mut got).unwrap();
    assert!(got.is_empty(), "{got:?}");
}
