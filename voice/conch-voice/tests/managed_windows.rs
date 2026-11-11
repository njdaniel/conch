//! `conch-voice managed --events-pipe \\.\pipe\<name>` as a process on Windows (issue #938,
//! `docs/design/windows-desktop.md` §3): the owner is a named pipe server of tokio's, held
//! by the test, in place of the desktop owner's libuv listener. Whether libuv's listener
//! and the helper agree (`ERROR_PIPE_BUSY` against Node's instances, the effective DACL)
//! is the hardware run of #903, not here.
//!
//! Run only by the `voice-windows` job. No test needs LiveKit, a microphone or a keyboard:
//! no session is started, so `conchd` is not asked for anything; the hello is written
//! before the helper reads a start, and that is as far as these go. The nonces and the pipe
//! names are obviously fake.

#![allow(clippy::unwrap_used, clippy::expect_used)]
// Windows only: the named pipes are the Windows ones. The Linux half is `managed_process.rs`.
#![cfg(windows)]

use std::io::Read;
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use conch_voice_api::{
    VOICE_CONTROL_HELLO_TIMEOUT_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VoiceControlEvent,
    parse_attached, parse_event,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::windows::named_pipe::{ClientOptions, NamedPipeServer, ServerOptions};

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// A fake nonce from `pkg/schema/testdata/voice-control-pipe/sensitive.json`.
const FAKE_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

/// How long a test waits for something that should happen at once.
const PATIENCE: Duration = Duration::from_secs(15);

/// A pipe name no other test, and no other run, uses.
fn pipe_name() -> String {
    static NEXT: AtomicU32 = AtomicU32::new(0);
    format!(
        r"\\.\pipe\conch-voice-test-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::SeqCst)
    )
}

fn server(name: &str, first: bool) -> NamedPipeServer {
    ServerOptions::new()
        .first_pipe_instance(first)
        .reject_remote_clients(true)
        .create(name)
        .unwrap()
}

/// A helper process, killed and waited for when the test lets go of it.
struct Helper {
    child: Child,
    stdin: Option<ChildStdin>,
    stderr: Arc<Mutex<String>>,
    reader: Option<std::thread::JoinHandle<()>>,
    _config: tempfile::TempDir,
}

impl Helper {
    fn spawn(events_pipe: &str, nonce: Option<&str>) -> Self {
        let config = tempfile::tempdir().unwrap();
        let mut command = Command::new(BIN);
        command
            .args(["managed", "--mic", "none", "--sink", "count"])
            .args(["--log-level", "trace", "--events-pipe", events_pipe])
            .env("CONCH_CONFIG_DIR", config.path())
            .env_remove("CONCH_TOKEN")
            .env_remove("CONCH_VOICE_ATTACH")
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped());
        if let Some(nonce) = nonce {
            command.env("CONCH_VOICE_ATTACH", nonce);
        }
        let mut child = command.spawn().unwrap();
        let stdin = child.stdin.take();
        let stderr = Arc::new(Mutex::new(String::new()));
        let (mut pipe, kept) = (child.stderr.take().unwrap(), Arc::clone(&stderr));
        let reader = std::thread::spawn(move || {
            let mut text = String::new();
            let mut chunk = [0_u8; 1024];
            while let Ok(read) = pipe.read(&mut chunk) {
                if read == 0 {
                    break;
                }
                text.push_str(&String::from_utf8_lossy(&chunk[..read]));
                *kept.lock().unwrap() = text.clone();
            }
        });
        Self {
            child,
            stdin,
            stderr,
            reader: Some(reader),
            _config: config,
        }
    }

    /// The end of standard input: the owner gone, by the other channel.
    fn close_stdin(&mut self) {
        self.stdin = None;
    }

    /// The exit code, if the process ended within `limit`.
    async fn exit_within(&mut self, limit: Duration) -> Option<i32> {
        let began = Instant::now();
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                return status.code();
            }
            if began.elapsed() >= limit {
                return None;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    fn stderr(&mut self) -> String {
        // The end of the pipe: the process has gone, or is killed.
        let _ = self.child.kill();
        let _ = self.child.wait();
        if let Some(reader) = self.reader.take() {
            let _ = reader.join();
        }
        self.stderr.lock().unwrap().clone()
    }
}

impl Drop for Helper {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

/// Reads from the owner's end until `lines` complete lines have arrived, or fails.
async fn read_lines(server: &mut NamedPipeServer, lines: usize) -> Vec<u8> {
    let mut got = Vec::new();
    let mut chunk = [0_u8; 1024];
    while got.iter().filter(|byte| **byte == b'\n').count() < lines {
        let read = tokio::time::timeout(PATIENCE, server.read(&mut chunk))
            .await
            .expect("the helper wrote nothing in time")
            .unwrap();
        assert_ne!(read, 0, "the helper's end closed: {got:?}");
        got.extend_from_slice(&chunk[..read]);
    }
    got
}

async fn connected(server: &NamedPipeServer) {
    tokio::time::timeout(PATIENCE, server.connect())
        .await
        .expect("the helper did not connect in time")
        .unwrap();
}

/// Reads the owner's end to its end: what else arrives until the helper lets go.
async fn read_to_end(server: &mut NamedPipeServer, mut got: Vec<u8>) -> Vec<u8> {
    let mut chunk = [0_u8; 1024];
    loop {
        match tokio::time::timeout(PATIENCE, server.read(&mut chunk)).await {
            Ok(Ok(read)) if read > 0 => got.extend_from_slice(&chunk[..read]),
            _ => return got,
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_attach_line_comes_first_then_the_hello_and_the_nonce_is_on_the_wire_exactly_once() {
    let name = pipe_name();
    let mut owner = server(&name, true);
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    connected(&owner).await;
    let got = read_lines(&mut owner, 2).await;
    helper.close_stdin();
    assert_eq!(helper.exit_within(PATIENCE).await, Some(0));
    let all = read_to_end(&mut owner, got).await;
    let text = String::from_utf8(all).unwrap();
    let mut lines = text.lines();
    // The first line is the attach line, the second the v1 hello, seq 1.
    let attached = parse_attached(lines.next().unwrap().as_bytes()).unwrap();
    assert!(attached.matches(FAKE_NONCE));
    match parse_event(lines.next().unwrap().as_bytes()).unwrap() {
        VoiceControlEvent::Hello(hello) => assert_eq!(hello.seq, 1),
        other => panic!("the line after the attach line is {other:?}"),
    }
    // The nonce left once, in line 1, and is in no event and not in standard error.
    assert_eq!(text.matches(FAKE_NONCE).count(), 1, "{text}");
    for event in text.lines().skip(1) {
        assert!(!event.contains("nonce"), "{event}");
    }
    let stderr = helper.stderr();
    assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_owner_closing_its_end_is_owner_loss_within_the_bound() {
    let name = pipe_name();
    let mut owner = server(&name, true);
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    connected(&owner).await;
    read_lines(&mut owner, 2).await;
    // Standard input stays open: the pipe alone says the owner is gone.
    let began = Instant::now();
    drop(owner);
    let code = helper
        .exit_within(Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS * 2))
        .await;
    let took = began.elapsed();
    assert!(code.is_some(), "the helper outlived its owner's end");
    assert!(
        took < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS),
        "{took:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_byte_from_the_owner_is_owner_loss_within_the_bound() {
    let name = pipe_name();
    let mut owner = server(&name, true);
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    connected(&owner).await;
    read_lines(&mut owner, 2).await;
    let began = Instant::now();
    // The owner never writes: one byte is the channel being someone else's.
    owner.write_all(b"x").await.unwrap();
    let code = helper
        .exit_within(Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS * 2))
        .await;
    let took = began.elapsed();
    assert!(
        code.is_some(),
        "the helper went on after a byte was written to it"
    );
    assert!(
        took < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS),
        "{took:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_name_outside_the_local_pipe_namespace_is_exit_2_with_one_line_before_anything_else() {
    for bad in [
        r"\\host\pipe\conch-voice-x",
        r"\\?\pipe\conch-voice-x",
        "conch-voice-x",
        r"pipe\conch-voice-x",
        "/tmp/conch-voice-x.sock",
        r"\\.\pipe\..\conch-voice-x",
        "",
    ] {
        let mut helper = Helper::spawn(bad, Some(FAKE_NONCE));
        assert_eq!(helper.exit_within(PATIENCE).await, Some(2), "{bad:?}");
        let stderr = helper.stderr();
        assert_eq!(stderr.lines().count(), 1, "{bad:?}: {stderr}");
        assert!(stderr.contains("events endpoint"), "{stderr}");
        assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_missing_or_malformed_nonce_is_exit_2_before_the_pipe_is_opened() {
    for nonce in [None, Some(""), Some("FAKE_short_nonce_do_not_print")] {
        let name = pipe_name();
        let owner = server(&name, true);
        let mut helper = Helper::spawn(&name, nonce);
        assert_eq!(helper.exit_within(PATIENCE).await, Some(2), "{nonce:?}");
        // Nobody connected: the one instance is still waiting for a client.
        let connect = tokio::time::timeout(Duration::from_millis(200), owner.connect()).await;
        assert!(connect.is_err(), "{nonce:?}: the helper connected");
        let stderr = helper.stderr();
        assert_eq!(stderr.lines().count(), 1, "{nonce:?}: {stderr}");
        if let Some(nonce) = nonce.filter(|nonce| !nonce.is_empty()) {
            assert!(!stderr.contains(nonce), "{stderr}");
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_busy_pipe_is_retried_until_an_instance_is_free() {
    let name = pipe_name();
    // The only instance is taken by another client: a new one finds ERROR_PIPE_BUSY.
    let first = server(&name, true);
    let _squatter = ClientOptions::new().open(&name).unwrap();
    connected(&first).await;
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    tokio::time::sleep(Duration::from_millis(400)).await;
    assert_eq!(
        helper.exit_within(Duration::from_millis(1)).await,
        None,
        "the helper gave up on a busy pipe"
    );
    // A second instance is created: the next try gets it.
    let mut second = server(&name, false);
    connected(&second).await;
    let got = read_lines(&mut second, 2).await;
    let text = String::from_utf8(got).unwrap();
    assert!(parse_attached(text.lines().next().unwrap().as_bytes()).is_ok());
    helper.close_stdin();
    assert_eq!(helper.exit_within(PATIENCE).await, Some(0));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_pipe_that_stays_busy_is_exit_2_at_the_hello_time_and_names_neither_pipe_nor_nonce() {
    let name = pipe_name();
    let first = server(&name, true);
    let _squatter = ClientOptions::new().open(&name).unwrap();
    connected(&first).await;
    let began = Instant::now();
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    let code = helper
        .exit_within(Duration::from_millis(
            VOICE_CONTROL_HELLO_TIMEOUT_MS + 5_000,
        ))
        .await;
    let took = began.elapsed();
    assert_eq!(code, Some(2));
    assert!(
        took >= Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS - 500),
        "gave up early: {took:?}"
    );
    let stderr = helper.stderr();
    assert_eq!(stderr.lines().count(), 1, "{stderr}");
    assert!(!stderr.contains(FAKE_NONCE), "{stderr}");
    assert!(!stderr.contains(&name), "{stderr}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn any_other_open_error_is_exit_2_at_once() {
    // Nothing listens on this name: not busy, absent.
    let name = pipe_name();
    let began = Instant::now();
    let mut helper = Helper::spawn(&name, Some(FAKE_NONCE));
    assert_eq!(helper.exit_within(PATIENCE).await, Some(2));
    assert!(began.elapsed() < Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS));
    let stderr = helper.stderr();
    assert_eq!(stderr.lines().count(), 1, "{stderr}");
}
