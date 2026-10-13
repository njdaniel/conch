//! The binary itself, as a process: its exit codes, the one line it stops with, and, with
//! the real SDK inside it, that nothing it writes holds a secret even at `--log-level trace`.
//!
//! No test here needs LiveKit. Where the real SDK is made to join, it joins a listener of
//! this test's own that refuses it, which is enough to make the SDK present the join token
//! and log what it logs. Every run has a private `HOME` and `XDG_CONFIG_HOME`, so no test
//! can read or write the real user's files.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::io::{BufRead, BufReader, Read, Write};
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

use support::stub::{FAKE_JOIN, FAKE_LOGIN, FAKE_ROOM, Reply, Stub, join_token, room_name};

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// The binary with nothing of the real user's in its environment.
fn conch_voice(private: &Path) -> Command {
    let mut command = Command::new(BIN);
    command
        .env_clear()
        .env("HOME", private.join("home"))
        .env("XDG_CONFIG_HOME", private.join("config"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    command
}

/// Runs the binary to its end with nothing on standard input.
fn run(command: &mut Command) -> (Option<i32>, String, String) {
    let output = command.stdin(Stdio::null()).output().unwrap();
    (
        output.status.code(),
        String::from_utf8(output.stdout).unwrap(),
        String::from_utf8(output.stderr).unwrap(),
    )
}

#[test]
fn the_version_is_still_printed() {
    let private = tempfile::tempdir().unwrap();
    let (code, stdout, stderr) = run(conch_voice(private.path()).arg("--version"));
    assert_eq!(code, Some(0));
    assert_eq!(
        stdout,
        format!(
            "conch-voice {} (48000 Hz, 10 ms frames)\n",
            env!("CARGO_PKG_VERSION")
        )
    );
    assert_eq!(stderr, "");
}

#[test]
fn a_mistake_on_the_command_line_exits_2() {
    let private = tempfile::tempdir().unwrap();
    for args in [
        &["join", "--mic", "pipewire"][..],
        &["join", "--sink", "count:440,450"],
        // Mistakes that are caught before anything is listed or opened.
        &["devices", "extra"],
        &["keys"],
        &["listen"],
        &[],
        // No channel on the command line, and no configuration file to name one.
        &["join"],
    ] {
        let mut command = conch_voice(private.path());
        let (code, stdout, stderr) = run(command.args(args).env("CONCH_TOKEN", FAKE_LOGIN));
        assert_eq!(code, Some(2), "{args:?}: {stderr}");
        assert_eq!(stdout, "", "{args:?}");
        assert!(!stderr.is_empty(), "{args:?}");
        assert!(!stderr.contains(FAKE_LOGIN), "{args:?}");
    }
}

#[test]
fn without_a_login_it_says_to_sign_in_and_exits_1() {
    let private = tempfile::tempdir().unwrap();
    let (code, stdout, stderr) = run(conch_voice(private.path()).args(["join", "ops"]));
    assert_eq!(code, Some(1));
    assert_eq!(stdout, "");
    assert_eq!(
        stderr,
        "conch-voice: not signed in to http://127.0.0.1:8080: run 'conch login'\n"
    );
}

#[test]
fn a_configuration_file_with_a_typo_is_refused_by_name() {
    let private = tempfile::tempdir().unwrap();
    let directory = private.path().join("config").join("conch");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::write(directory.join("voice.toml"), "chanel = \"ops\"\n").unwrap();
    let mut command = conch_voice(private.path());
    let (code, _, stderr) = run(command.args(["join", "ops"]).env("CONCH_TOKEN", FAKE_LOGIN));
    assert_eq!(code, Some(1));
    assert!(stderr.contains("unknown key `chanel`"), "{stderr}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_refused_session_stops_it_with_one_line_naming_the_reason_and_exit_1() {
    let private = tempfile::tempdir().unwrap();
    let conchd = Stub::start().await;
    conchd.next_session(Reply::error(404, "channel_not_found"));
    let mut command = conch_voice(private.path());
    command
        .args(["join", "ops", "--server", &conchd.url()])
        .env("CONCH_TOKEN", FAKE_LOGIN);
    // Standard input stays open: its end would be a `quit`, and this is about a stop.
    let (code, stdout, stderr) =
        tokio::task::spawn_blocking(move || Running::start(&mut command).exit())
            .await
            .unwrap();
    assert_eq!(code, Some(1));
    assert_eq!(
        stderr,
        "conch-voice: not a member of this channel, or there is no such channel\n"
    );
    assert!(
        stdout.contains("stopped: not a member of this channel, or there is no such channel\n"),
        "{stdout}"
    );
    assert_eq!(conchd.session_requests(), 1);
}

/// Something that listens where a session says LiveKit is, takes each request the SDK
/// makes of it, keeps it, and answers 404.
struct NotLiveKit {
    url: String,
    received: Arc<Mutex<String>>,
}

impl NotLiveKit {
    async fn start() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let received = Arc::new(Mutex::new(String::new()));
        let kept = Arc::clone(&received);
        tokio::spawn(async move {
            loop {
                let Ok((mut stream, _)) = listener.accept().await else {
                    return;
                };
                let kept = Arc::clone(&kept);
                tokio::spawn(async move {
                    let mut request = Vec::new();
                    let mut chunk = [0u8; 4096];
                    while !request.windows(4).any(|w| w == b"\r\n\r\n") {
                        match stream.read(&mut chunk).await {
                            Ok(0) | Err(_) => break,
                            Ok(n) => request.extend_from_slice(&chunk[..n]),
                        }
                    }
                    kept.lock()
                        .unwrap()
                        .push_str(&String::from_utf8_lossy(&request));
                    let answer =
                        "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
                    let _ = stream.write_all(answer.as_bytes()).await;
                });
            }
        });
        Self { url, received }
    }

    fn received(&self) -> String {
        self.received.lock().unwrap().clone()
    }
}

/// Collects a child's output on threads of their own, so that neither pipe fills.
struct Running {
    child: Child,
    stdout: Arc<Mutex<String>>,
    stderr: Arc<Mutex<String>>,
}

impl Running {
    fn start(command: &mut Command) -> Self {
        let mut child = command.spawn().unwrap();
        let stdout = Arc::new(Mutex::new(String::new()));
        let stderr = Arc::new(Mutex::new(String::new()));
        let (out, kept) = (child.stdout.take().unwrap(), Arc::clone(&stdout));
        std::thread::spawn(move || {
            for line in BufReader::new(out).lines().map_while(Result::ok) {
                let mut kept = kept.lock().unwrap();
                kept.push_str(&line);
                kept.push('\n');
            }
        });
        let (mut err, kept) = (child.stderr.take().unwrap(), Arc::clone(&stderr));
        std::thread::spawn(move || {
            let mut chunk = [0u8; 4096];
            while let Ok(n) = err.read(&mut chunk) {
                if n == 0 {
                    break;
                }
                kept.lock()
                    .unwrap()
                    .push_str(&String::from_utf8_lossy(&chunk[..n]));
            }
        });
        Self {
            child,
            stdout,
            stderr,
        }
    }

    fn stdout(&self) -> String {
        self.stdout.lock().unwrap().clone()
    }

    fn stderr(&self) -> String {
        self.stderr.lock().unwrap().clone()
    }

    /// Waits until `met` is true of what was written to standard output so far.
    fn until(&mut self, what: &str, within: Duration, met: impl Fn(&str) -> bool) {
        let deadline = Instant::now() + within;
        while !met(&self.stdout()) {
            let exited = self.child.try_wait().unwrap();
            assert!(
                exited.is_none() && Instant::now() < deadline,
                "waiting for {what}: exited {exited:?}\nstdout:\n{}\nstderr:\n{}",
                self.stdout().replace("FAKE", "F4KE"),
                self.stderr().replace("FAKE", "F4KE")
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    /// Ends standard input and waits for the process to exit.
    fn end_input(mut self) -> (Option<i32>, String, String) {
        drop(self.child.stdin.take());
        self.exit()
    }

    /// Waits for the process to exit by itself, with its standard input left as it is.
    fn exit(mut self) -> (Option<i32>, String, String) {
        let deadline = Instant::now() + Duration::from_secs(30);
        let code = loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                break status.code();
            }
            if Instant::now() > deadline {
                self.child.kill().unwrap();
                panic!("the process did not exit");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        // The reader threads see the end of the pipes just after the exit.
        std::thread::sleep(Duration::from_millis(100));
        (code, self.stdout(), self.stderr())
    }
}

/// How many `connection` lines say the join failed, in either mode.
fn failed_joins(stdout: &str) -> usize {
    stdout
        .lines()
        .filter(|line| line.contains("connect_failed") || line.contains("connect failed"))
        .count()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn with_the_real_sdk_at_trace_level_nothing_written_holds_a_token_or_a_room_name() {
    for json in [true, false] {
        let private = tempfile::tempdir().unwrap();
        let conchd = Stub::start().await;
        let not_livekit = NotLiveKit::start().await;
        conchd.livekit_url(&not_livekit.url);

        let mut command = conch_voice(private.path());
        command
            .args(["join", "ops", "--server", &conchd.url()])
            .args(["--mic", "tone:440", "--log-level", "trace"])
            .env("CONCH_TOKEN", FAKE_LOGIN);
        if json {
            command.arg("--json");
        }
        let (code, stdout, stderr) = tokio::task::spawn_blocking(move || {
            let mut running = Running::start(&mut command);
            // Two joins that fail: the second with a session, and a token, of its own.
            running.until("two failed joins", Duration::from_secs(60), |stdout| {
                failed_joins(stdout) >= 2
            });
            // A press while not connected, for good measure, and then the end of input.
            let stdin = running.child.stdin.as_mut().unwrap();
            stdin.write_all(b"down\nnonsense typed here\n").unwrap();
            stdin.flush().unwrap();
            running.until(
                "the press to be refused",
                Duration::from_secs(10),
                |stdout| stdout.contains("press_ignored") || stdout.contains("press ignored"),
            );
            running.end_input()
        })
        .await
        .unwrap();

        assert_eq!(code, Some(0), "the end of input is a clean exit: {stderr}");

        // The SDK really was given the tokens, and really presented them where the session
        // said LiveKit was: this is the traffic that its trace logging describes.
        let presented = not_livekit.received();
        assert!(conchd.session_requests() >= 2);
        for n in [1, 2] {
            assert!(
                presented.contains(&join_token(n)),
                "the SDK did not present the token of session {n}"
            );
        }

        for (what, text) in [("standard output", &stdout), ("standard error", &stderr)] {
            for secret in [FAKE_LOGIN, FAKE_JOIN, FAKE_ROOM] {
                assert!(
                    !text.contains(secret),
                    "json {json}: {what} holds a secret:\n{}",
                    text.replace("FAKE", "F4KE")
                );
            }
            for n in [1, 2] {
                assert!(!text.contains(&room_name(n)), "json {json}: {what}");
            }
            assert!(!text.contains("nonsense typed here"), "json {json}: {what}");
        }
        // The SDK's warnings and errors about the refused joins got through, so the logger
        // is the one in use; nothing of the SDK's below a warning did, though trace was
        // asked for and the SDK logs the whole upgrade request, token and all, at trace.
        assert!(
            stderr.contains("conch-voice: warn: livekit"),
            "json {json}: {stderr}"
        );
        for line in stderr.lines() {
            let record = line
                .strip_prefix("conch-voice: ")
                .unwrap_or_else(|| panic!("a line on standard error that is not a record: {line}"));
            let ours = [
                "livekit",
                "tungstenite",
                "webrtc",
                "reqwest",
                "hyper",
                "rustls",
            ]
            .iter()
            .all(|theirs| !record.contains(theirs));
            assert!(
                ours || record.starts_with("warn: ") || record.starts_with("error: "),
                "json {json}: an SDK record below warn: {line}"
            );
            assert!(
                !record.to_ascii_lowercase().contains("authorization"),
                "json {json}: a request header was logged: {line}"
            );
        }
        if json {
            for line in stdout.lines().filter(|line| line.starts_with('{')) {
                let object: serde_json::Value = serde_json::from_str(line).unwrap();
                assert!(object["event"].is_string(), "{line}");
            }
            assert!(stdout.contains(r#""state":"waiting""#), "{stdout}");
            assert!(
                stdout.contains(r#"{"event":"unknown_command"}"#),
                "{stdout}"
            );
            assert!(stdout.contains(r#"{"event":"connection","state":"closed"}"#));
        }
    }
}

/// A keyboard that is not there, where the configuration is allowed to name one. The name
/// is made up and nothing is at it, so the client finds nothing to open: no test here
/// opens, reads or lists a device under `/dev/input`.
const NO_SUCH_KEYBOARD: &str = "/dev/input/by-id/conch-voice-test-no-such-keyboard-event-kbd";

/// A configuration file that names a key device and a talk key.
fn configure_keys(private: &Path, device: &Path) {
    let directory = private.join("config").join("conch");
    std::fs::create_dir_all(&directory).unwrap();
    std::fs::write(
        directory.join("voice.toml"),
        format!(
            "[keys]\ndevice = \"{}\"\ntalk = \"KEY_F13\"\n",
            device.display()
        ),
    )
    .unwrap();
}

/// The lines that are about the key device, in either mode.
fn key_device_lines(stdout: &str) -> Vec<&str> {
    stdout
        .lines()
        .filter(|line| line.starts_with("key device ") || line.contains(r#""event":"key_device""#))
        .collect()
}

/// A `join` that stays up: `conchd` issues sessions, and what it calls LiveKit refuses the
/// SDK, so the client waits and tries again for as long as it is left to.
async fn joining(private: &Path, extra: &[&str]) -> (Command, Stub, NotLiveKit) {
    let conchd = Stub::start().await;
    let not_livekit = NotLiveKit::start().await;
    conchd.livekit_url(&not_livekit.url);
    let mut command = conch_voice(private);
    command
        .args(["join", "ops", "--server", &conchd.url()])
        .args(extra)
        .env("CONCH_TOKEN", FAKE_LOGIN);
    (command, conchd, not_livekit)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_key_device_that_cannot_be_opened_is_said_once_and_the_client_runs_on_with_no_input() {
    for json in [false, true] {
        let private = tempfile::tempdir().unwrap();
        let absent = Path::new(NO_SUCH_KEYBOARD);
        assert!(!absent.exists(), "the made-up keyboard must not exist");
        configure_keys(private.path(), absent);
        let extra: &[&str] = if json { &["--json"] } else { &[] };
        let (mut command, _conchd, _not_livekit) = joining(private.path(), extra).await;
        // No standard input at all: with a key device to watch, its end is not a `quit`.
        command.stdin(Stdio::null());

        let (code, stdout, stderr) = tokio::task::spawn_blocking(move || {
            let mut running = Running::start(&mut command);
            // Two failed joins later (more than a second, and several attempts to open
            // the device) the client is still running: `until` fails if it has exited.
            running.until("two failed joins", Duration::from_secs(60), |stdout| {
                failed_joins(stdout) >= 2
            });
            // SIGTERM, as a service manager would send it.
            let ended = Command::new("kill")
                .args(["-TERM", &running.child.id().to_string()])
                .status()
                .unwrap();
            assert!(ended.success());
            running.exit()
        })
        .await
        .unwrap();

        assert_eq!(
            code,
            Some(0),
            "json {json}: a signal is a clean exit: {stderr}"
        );
        let said = key_device_lines(&stdout);
        assert_eq!(
            said.len(),
            1,
            "json {json}: said once, not once an attempt: {stdout}"
        );
        let why = format!("cannot open it: {}", std::io::Error::from_raw_os_error(2));
        if json {
            let object: serde_json::Value = serde_json::from_str(said[0]).unwrap();
            assert_eq!(
                object,
                serde_json::json!({
                    "event": "key_device",
                    "device": absent.display().to_string(),
                    "state": "missing",
                    "reason": "cannot_open",
                    "detail": why,
                    "retrying": true,
                    "presses_refused": false,
                })
            );
        } else {
            assert_eq!(
                said[0],
                format!(
                    "key device {}: {why}; taking down, up, mute, deafen and quit from \
                     standard input, and trying again",
                    absent.display()
                )
            );
            // Line by line, as before: standard output is not a terminal.
            assert!(stdout.contains("you: not connected; "), "{stdout}");
            assert!(stdout.contains("\nconnecting\n"), "{stdout}");
            assert!(!stdout.contains("voice: "), "{stdout}");
        }
        assert!(!stdout.contains('\u{1b}'), "json {json}: {stdout:?}");
    }
}

#[test]
fn the_configuration_cannot_name_a_key_device_that_is_not_under_dev_input() {
    let private = tempfile::tempdir().unwrap();
    // A file that holds a press of the talk key, where the configuration says the keyboard
    // is; and a path that only starts under /dev/input.
    let file = private.path().join("usb-Example-event-kbd");
    let mut press = [0u8; 24];
    press[16] = 1;
    press[18] = 183;
    press[20] = 1;
    std::fs::write(&file, press).unwrap();
    let climbing = Path::new("/dev/input/..").join(file.strip_prefix("/").unwrap());
    for device in [&file, &climbing] {
        configure_keys(private.path(), device);
        let mut command = conch_voice(private.path());
        let (code, stdout, stderr) =
            run(command.args(["join", "ops"]).env("CONCH_TOKEN", FAKE_LOGIN));
        assert_eq!(code, Some(1), "{}", device.display());
        assert_eq!(stdout, "");
        assert!(
            stderr.ends_with(
                "`keys.device`: must be a path under /dev/input/, such as \
                 /dev/input/by-id/...-event-kbd\n"
            ),
            "{stderr}"
        );
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn with_stdin_keys_no_key_device_is_opened_and_the_end_of_input_still_quits() {
    let private = tempfile::tempdir().unwrap();
    configure_keys(private.path(), Path::new(NO_SUCH_KEYBOARD));
    let (mut command, _conchd, _not_livekit) = joining(private.path(), &["--stdin-keys"]).await;

    let (code, stdout, stderr) = tokio::task::spawn_blocking(move || {
        let mut running = Running::start(&mut command);
        running.until("a failed join", Duration::from_secs(60), |stdout| {
            failed_joins(stdout) >= 1
        });
        running.end_input()
    })
    .await
    .unwrap();

    assert_eq!(code, Some(0), "the end of input is a clean exit: {stderr}");
    assert!(key_device_lines(&stdout).is_empty(), "{stdout}");
    assert!(stdout.ends_with("left voice\n"), "{stdout}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn with_a_key_device_the_end_of_standard_input_is_a_release_and_the_client_runs_on() {
    let private = tempfile::tempdir().unwrap();
    configure_keys(private.path(), Path::new(NO_SUCH_KEYBOARD));
    let (mut command, _conchd, _not_livekit) = joining(private.path(), &[]).await;

    let (code, stdout, stderr) = tokio::task::spawn_blocking(move || {
        let mut running = Running::start(&mut command);
        running.until("a failed join", Duration::from_secs(60), |stdout| {
            failed_joins(stdout) >= 1
        });
        {
            let stdin = running.child.stdin.as_mut().unwrap();
            stdin.write_all(b"down\n").unwrap();
            stdin.flush().unwrap();
        }
        // The end of standard input: with a key device watched it is a release and no
        // more, so a wrapper that dies after `down` cannot leave the gate open, and the
        // client runs on, driven by the device.
        drop(running.child.stdin.take());
        assert!(
            exit_within(&mut running.child, Duration::from_secs(2)).is_none(),
            "the end of standard input was taken as a quit"
        );
        // A signal is still a clean exit.
        signal(&running.child, "TERM");
        running.exit()
    })
    .await
    .unwrap();

    assert_eq!(code, Some(0), "a signal is a clean exit: {stderr}");
    assert!(stdout.ends_with("left voice\n"), "{stdout}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_line_that_is_not_text_is_not_a_command_and_the_reading_goes_on() {
    let private = tempfile::tempdir().unwrap();
    let (mut command, _conchd, _not_livekit) = joining(private.path(), &[]).await;

    let (code, stdout, stderr) = tokio::task::spawn_blocking(move || {
        let mut running = Running::start(&mut command);
        running.until("a failed join", Duration::from_secs(60), |stdout| {
            failed_joins(stdout) >= 1
        });
        {
            let stdin = running.child.stdin.as_mut().unwrap();
            // No UTF-8 here: the line is not a command, and it is not repeated. The
            // reading does not end for it: the `quit` after it is still heard.
            stdin.write_all(b"\xff\xfe\x00\nquit\n").unwrap();
            stdin.flush().unwrap();
        }
        running.exit()
    })
    .await
    .unwrap();

    assert_eq!(code, Some(0), "quit was heard after it: {stderr}");
    assert!(stdout.contains("unknown command"), "{stdout}");
    assert!(stdout.ends_with("left voice\n"), "{stdout}");
}

#[test]
fn keys_refuses_when_standard_output_is_not_a_terminal() {
    let private = tempfile::tempdir().unwrap();
    // Not a keyboard, and not there: nothing could be read even if the refusal failed.
    let absent = private.path().join("absent-event-kbd");
    let mut command = conch_voice(private.path());
    let (code, stdout, stderr) = run(command.arg("keys").arg(&absent));
    assert_eq!(code, Some(2));
    assert_eq!(stdout, "", "not even the warning goes into a pipe");
    assert_eq!(
        stderr,
        "conch-voice: keys shows every key pressed and writes only to a terminal: standard \
         output is not one\n"
    );
}

/// One word of a shell command line.
fn quoted(text: &str) -> String {
    format!("'{}'", text.replace('\'', "'\\''"))
}

/// How long a run on a terminal may take before it is killed and the test fails.
const TERMINAL_BOUND: Duration = Duration::from_secs(60);

/// Runs a shell command line under script(1) from util-linux, which gives it a terminal
/// for standard input, output and error (the line may then redirect any of them), and
/// returns its exit code and everything written to that terminal, with the terminal's
/// carriage returns taken out.
///
/// # Panics
///
/// If the command has not ended within [`TERMINAL_BOUND`]: a binary that blocks, on a pipe
/// it should never have opened for one, fails the test and does not hang it.
fn on_a_terminal_line(private: &Path, line: &str, token: bool) -> (Option<i32>, String) {
    let mut command = Command::new("script");
    command
        .args(["--quiet", "--return", "--command", line, "/dev/null"])
        .env_clear()
        .env("HOME", private.join("home"))
        .env("XDG_CONFIG_HOME", private.join("config"))
        .env("SHELL", "/bin/sh")
        // Held open and never written to: the end of input would be a `quit`.
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if token {
        command.env("CONCH_TOKEN", FAKE_LOGIN);
    }
    let mut child = command
        .spawn()
        .expect("script(1) from util-linux is needed to give the binary a terminal");
    let stdin = child.stdin.take();
    let mut terminal = child.stdout.take().unwrap();
    let reader = std::thread::spawn(move || {
        let mut written = Vec::new();
        let _ = terminal.read_to_end(&mut written);
        written
    });
    let deadline = Instant::now() + TERMINAL_BOUND;
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break Some(status);
        }
        if Instant::now() > deadline {
            // With script gone its terminal is gone, and the command under it is hung up.
            child.kill().unwrap();
            child.wait().unwrap();
            break None;
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    drop(stdin);
    let written = String::from_utf8_lossy(&reader.join().unwrap()).replace('\r', "");
    let Some(status) = status else {
        panic!("`{line}` did not end within {TERMINAL_BOUND:?}; it wrote:\n{written}");
    };
    (status.code(), written)
}

/// Runs the binary with these arguments on a terminal, as [`on_a_terminal_line`] does.
fn on_a_terminal(private: &Path, args: &[&str], token: bool) -> (Option<i32>, String) {
    let line = std::iter::once(BIN)
        .chain(args.iter().copied())
        .map(quoted)
        .collect::<Vec<_>>()
        .join(" ");
    on_a_terminal_line(private, &line, token)
}

#[test]
fn on_a_terminal_keys_says_what_it_will_show_and_only_then_tries_the_device() {
    let private = tempfile::tempdir().unwrap();
    let absent = private.path().join("absent-event-kbd");
    let device = absent.display().to_string();
    let (code, written) = on_a_terminal(private.path(), &["keys", &device], false);

    let failure = format!(
        "conch-voice: key device {device}: cannot open it: {}\n",
        std::io::Error::from_raw_os_error(2)
    );
    // The whole warning, and after it the failure to open the device, which is not there.
    assert_eq!(
        written,
        format!("{}{failure}", conch_voice::keys::warning(&device))
    );
    assert!(written.contains("EVERY key pressed"), "{written}");
    assert_eq!(code, Some(1));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn on_a_terminal_the_status_is_redrawn_in_place_and_with_json_it_is_not() {
    let reason = "not a member of this channel, or there is no such channel";
    for json in [false, true] {
        let private = tempfile::tempdir().unwrap();
        let conchd = Stub::start().await;
        conchd.next_session(Reply::error(404, "channel_not_found"));
        let url = conchd.url();
        let directory = private.path().to_owned();
        let (code, written) = tokio::task::spawn_blocking(move || {
            let mut args = vec!["join", "ops", "--server", &url];
            if json {
                args.push("--json");
            }
            on_a_terminal(&directory, &args, true)
        })
        .await
        .unwrap();
        assert_eq!(code, Some(1), "{written:?}");
        assert!(
            written.ends_with(&format!("conch-voice: {reason}\n")),
            "{written:?}"
        );

        if json {
            assert!(!written.contains('\u{1b}'), "{written:?}");
            assert!(
                written.contains(r#"{"event":"connection","state":"connecting"}"#),
                "{written:?}"
            );
            continue;
        }
        // Drawn once where the cursor was, and then over itself, four lines up each time.
        assert!(
            written.contains("\u{1b}[?7l\u{1b}[2Kvoice: starting\n"),
            "{written:?}"
        );
        assert!(
            written.contains("\u{1b}7\u{1b}[4A\u{1b}[?7l\u{1b}[2Kvoice: connecting\n"),
            "{written:?}"
        );
        let last = format!(
            "\u{1b}7\u{1b}[4A\u{1b}[?7l\u{1b}[2Kvoice: stopped: {reason}\n\
             \u{1b}[2Kyou: not connected; listening only: this session may not transmit; \
             no microphone\n"
        );
        assert!(written.contains(&last), "{written:?}");
        // No line of the line-by-line output is there.
        assert!(!written.contains("\nconnecting\n"), "{written:?}");
        assert!(
            !written.contains(&format!("\nstopped: {reason}\n")),
            "{written:?}"
        );
    }
}

/// Sends a signal to a child with the system's `kill`: the standard library can only kill.
fn signal(child: &Child, name: &str) {
    let status = Command::new("kill")
        .args([&format!("-{name}"), &child.id().to_string()])
        .status()
        .unwrap();
    assert!(status.success(), "kill -{name} failed");
}

/// Waits for a child to exit by itself, for at most `within`. `None` if it did not.
fn exit_within(child: &mut Child, within: Duration) -> Option<(Option<i32>, Duration)> {
    let began = Instant::now();
    while began.elapsed() < within {
        if let Some(status) = child.try_wait().unwrap() {
            return Some((status.code(), began.elapsed()));
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    None
}

/// The real process, with a standard output that is a pipe nobody reads. The client fills
/// it (each line that is not a command is answered with one, and thirty thousand of them
/// are far more than a pipe and the queue behind it hold). It must still leave on a
/// signal, and on the end of its standard input, within a few seconds, and cleanly.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_standard_output_nobody_reads_cannot_keep_the_process_from_leaving() {
    for way in ["TERM", "INT", "the end of standard input"] {
        let private = tempfile::tempdir().unwrap();
        let conchd = Stub::start().await;
        let not_livekit = NotLiveKit::start().await;
        conchd.livekit_url(&not_livekit.url);

        let mut command = conch_voice(private.path());
        command
            .args(["join", "ops", "--server", &conchd.url(), "--json"])
            .args(["--mic", "none"])
            .env("CONCH_TOKEN", FAKE_LOGIN)
            // What it logs about the joins that fail is not what this is about.
            .stderr(Stdio::null());
        let (exited, unread) = tokio::task::spawn_blocking(move || {
            let mut child = command.spawn().unwrap();
            let mut stdin = child.stdin.take().unwrap();
            stdin.write_all(&b"junk\n".repeat(30_000)).unwrap();
            stdin.flush().unwrap();
            // Long enough for the client to have read all of it and filled the pipe.
            std::thread::sleep(Duration::from_millis(1500));
            assert!(
                child.try_wait().unwrap().is_none(),
                "the client left by itself"
            );

            if way == "the end of standard input" {
                drop(stdin);
            } else {
                signal(&child, way);
                // Standard input stays open: it is the signal that is being tried.
                std::mem::forget(stdin);
            }
            let exited = exit_within(&mut child, Duration::from_secs(8));
            if exited.is_none() {
                child.kill().unwrap();
                child.wait().unwrap();
            }
            // What was in the pipe all along, read only now that the client is gone.
            let mut unread = Vec::new();
            child
                .stdout
                .take()
                .unwrap()
                .read_to_end(&mut unread)
                .unwrap();
            (exited, unread)
        })
        .await
        .unwrap();

        let (code, took) = exited.unwrap_or_else(|| {
            panic!("{way}: still running 8 s later, with nobody reading its standard output")
        });
        assert_eq!(code, Some(0), "{way}: a clean exit");
        assert!(took < Duration::from_secs(8), "{way}: {took:?}");
        // The pipe really was full: it holds what a pipe holds, and the client had more to
        // say than that.
        assert!(
            unread.len() >= 60_000,
            "{way}: only {} bytes were waiting in the pipe",
            unread.len()
        );
        let unread = String::from_utf8_lossy(&unread);
        assert!(unread.contains(r#"{"event":"unknown_command"}"#), "{way}");
        assert!(!unread.contains(FAKE_LOGIN), "{way}");
    }
}

/// What `keys` says when it refuses for want of a terminal.
const NOT_A_TERMINAL: &str = "conch-voice: keys shows every key pressed and writes only to a \
                              terminal: standard output is not one\n";

#[test]
fn keys_looks_at_standard_output_and_at_nothing_else_to_decide_whether_to_write() {
    let private = tempfile::tempdir().unwrap();
    // Not a keyboard, and not there: nothing could be read whatever `keys` decided.
    let absent = private.path().join("absent-event-kbd");
    let device = absent.display().to_string();
    let keys = format!("{} keys {}", quoted(BIN), quoted(&device));
    let failure = format!(
        "conch-voice: key device {device}: cannot open it: {}\n",
        std::io::Error::from_raw_os_error(2)
    );
    let warning = conch_voice::keys::warning(&device);

    // Standard input and standard error are a terminal, and standard output is a file:
    // it refuses, and nothing at all is put in the file.
    let file = private.path().join("redirected.txt");
    let (code, written) = on_a_terminal_line(
        private.path(),
        &format!("{keys} > {}", quoted(file.to_str().unwrap())),
        false,
    );
    assert_eq!(code, Some(2), "{written}");
    assert_eq!(written, NOT_A_TERMINAL);
    assert_eq!(std::fs::read(&file).unwrap(), b"", "not a byte in the file");

    // The same through a pipe to a program that writes to the terminal.
    let (code, written) = on_a_terminal_line(private.path(), &format!("{keys} | /bin/cat"), false);
    assert_eq!(written, NOT_A_TERMINAL);
    // The shell reports the last command of the pipe, which is `cat`.
    assert_eq!(code, Some(0));

    // Standard output is a terminal and standard error is a file: it runs. What it looks
    // at is where the keys would be written, and nothing else.
    let errors = private.path().join("errors.txt");
    let (code, written) = on_a_terminal_line(
        private.path(),
        &format!("{keys} 2> {}", quoted(errors.to_str().unwrap())),
        false,
    );
    assert_eq!(code, Some(1), "{written}");
    assert_eq!(written, warning);
    assert_eq!(std::fs::read_to_string(&errors).unwrap(), failure);

    // Standard output is a terminal and standard input is not: it runs.
    let (code, written) = on_a_terminal_line(private.path(), &format!("{keys} < /dev/null"), false);
    assert_eq!(code, Some(1), "{written}");
    assert_eq!(written, format!("{warning}{failure}"));
}

#[test]
fn on_a_terminal_keys_refuses_a_file_and_a_pipe_itself_and_reads_nothing_from_them() {
    use std::fs::OpenOptions;
    let private = tempfile::tempdir().unwrap();
    // A press of KEY_RIGHTCTRL, which `keys` would show as `key 97` if it read it.
    let mut press = [0u8; 24];
    press[16] = 1;
    press[18] = 97;
    press[20] = 1;

    // An existing file that holds the press.
    let file = private.path().join("usb-File-event-kbd");
    std::fs::write(&file, press).unwrap();
    // An existing pipe that holds the press. This test has it open for reading and for
    // writing, so that nothing here can block on it: a binary that opened it would get it
    // open at once, read the press, show it, and then wait for ever for the next one.
    let pipe = private.path().join("usb-Pipe-event-kbd");
    let made = Command::new("mkfifo").arg(&pipe).status().unwrap();
    assert!(made.success());
    let mut held = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&pipe)
        .unwrap();
    held.write_all(&press).unwrap();

    for device in [&file, &pipe] {
        let device = device.display().to_string();
        // Within a bound: a binary that blocks on the pipe fails this, and does not hang it.
        let (code, written) = on_a_terminal(private.path(), &["keys", &device], false);
        // The refusal is the binary's own, in its own words, after the whole warning.
        assert_eq!(
            written,
            format!(
                "{}conch-voice: key device {device}: refused: it is not under /dev/input\n",
                conch_voice::keys::warning(&device)
            )
        );
        assert_eq!(code, Some(1));
        assert!(!written.contains("key 97"), "{written}");
        assert!(!written.contains("Reading."), "{written}");
    }

    // Nothing was taken from the pipe: the press is still in it, in front of this marker.
    let marker = [0xa5u8; 24];
    held.write_all(&marker).unwrap();
    let mut left = [0u8; 96];
    let count = held.read(&mut left).unwrap();
    assert_eq!(left[..count], [press, marker].concat()[..]);
    // And no option asks for another rule: there is none
    // to ask for.
    for option in ["--any-file", "--rule=any", "--no-check"] {
        let mut command = conch_voice(private.path());
        let (code, stdout, _) = run(command.args(["keys", option]).arg(&file));
        assert_eq!(code, Some(2), "{option}");
        assert_eq!(stdout, "", "{option}");
    }
}
