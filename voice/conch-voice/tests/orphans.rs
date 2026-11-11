//! No process a test starts outlives it (#1157), and what a `join` does when its `conchd`
//! has gone and its standard input is, or is not, still held.
//!
//! The first: a test that panics while it holds the binary leaves no child behind, because
//! [`Reaped`] kills and waits for it as the stack unwinds. The second: with no key device,
//! the end of standard input is a `quit` however the connection stands (conch-voice.md,
//! "Exit codes"); with a key device it is only a release, and a client that nobody signals
//! retries for as long as it runs (§5: "Retry: wait 1 s, doubling to 30 s", with no limit),
//! which is why every test holds its client through [`Reaped`].

#![allow(clippy::unwrap_used, clippy::expect_used)]
// Unix only: It looks for a process under /proc. Windows runs the unit tests in `src/`,
// which include the answers a non-Unix platform gives, and the tests in this directory that
// need none of this (#1309).
#![cfg(unix)]

mod support;

use std::panic::{AssertUnwindSafe, catch_unwind};
use std::path::Path;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use support::reaped::Reaped;
use support::stub::FAKE_LOGIN;

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// A `join` of a `conchd` that is not there: a port that was bound and then let go of.
fn join_without_a_server(private: &Path) -> Command {
    let port = {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.local_addr().unwrap().port()
    };
    let mut command = Command::new(BIN);
    command
        .args([
            "join",
            "ops",
            "--server",
            &format!("http://127.0.0.1:{port}"),
        ])
        .args(["--mic", "none", "--sink", "count"])
        .env_clear()
        .env("HOME", private.join("home"))
        .env("XDG_CONFIG_HOME", private.join("config"))
        .env("CONCH_TOKEN", FAKE_LOGIN)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    command
}

/// Whether the kernel still has a process, or a zombie, by this pid.
fn exists(pid: u32) -> bool {
    Path::new(&format!("/proc/{pid}")).exists()
}

#[test]
fn a_test_that_panics_while_it_holds_the_binary_leaves_no_child() {
    let private = tempfile::tempdir().unwrap();
    let mut command = join_without_a_server(private.path());
    let mut pid = 0;
    let outcome = catch_unwind(AssertUnwindSafe(|| {
        let child = Reaped::spawn(&mut command).unwrap();
        pid = child.id();
        // Standard input is held open by `child`, so the client has no reason to leave.
        let began = Instant::now();
        while !exists(pid) && began.elapsed() < Duration::from_secs(5) {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(exists(pid), "the client did not start");
        panic!("a test fails with the client running");
    }));
    assert!(outcome.is_err(), "the closure was to panic");
    assert_ne!(pid, 0);
    // Reaped, not only killed: no zombie is left under this test either.
    assert!(!exists(pid), "the client {pid} outlived the panic");
}

#[test]
fn with_its_server_gone_a_join_leaves_at_the_end_of_standard_input() {
    let private = tempfile::tempdir().unwrap();
    let mut command = join_without_a_server(private.path());
    let mut child = Reaped::spawn(&mut command).unwrap();
    // Long enough for a first failed attempt and for the wait before the next to begin.
    std::thread::sleep(Duration::from_millis(1500));
    assert!(
        child.try_wait().unwrap().is_none(),
        "the client left with its standard input held: it retries until told to stop"
    );
    drop(child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(10);
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        assert!(
            Instant::now() < deadline,
            "the end of standard input did not end the client"
        );
        std::thread::sleep(Duration::from_millis(10));
    };
    assert_eq!(status.code(), Some(0), "the end of input is a clean exit");
}
