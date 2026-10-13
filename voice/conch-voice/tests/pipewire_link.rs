//! Whether the binary can call PipeWire at all (issue #184; design note §13 row 7).
//!
//! The prebuilt libwebrtc the SDK links carries stand-ins for some of PipeWire's functions
//! (`pw_init`, `pw_context_new`, `pw_stream_*` and a few more) as weak definitions, and the
//! linker takes them for `dmn-audio`'s calls into PipeWire. Nothing fills in their table, so
//! the first call, `pw_init`, jumps to a null pointer and the process dies with SIGSEGV.
//! `scripts/voice-toolchain.sh` therefore removes that one member from the archive before
//! anything is linked, and `scripts/voice-pins.sh` pins the result. This test is what fails
//! if the stand-ins ever come back, with a new libwebrtc or a changed link.
//!
//! It needs no PipeWire server, no device and no keyboard: it runs `conch-voice devices`
//! with `PIPEWIRE_REMOTE` naming a socket that is not there. A binary that can call
//! PipeWire says that it cannot connect, on one line, and still lists the keyboards; one
//! that cannot dies on a signal before it prints anything.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::os::unix::process::ExitStatusExt;
use std::process::{Command, Stdio};

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

#[test]
fn the_binary_can_call_pipewire_and_says_when_there_is_no_server() {
    let private = tempfile::tempdir().unwrap();
    let output = Command::new(BIN)
        .env_clear()
        .env("HOME", private.path().join("home"))
        .env("XDG_CONFIG_HOME", private.path().join("config"))
        // Where PipeWire looks for its socket, and a socket that is not there.
        .env("XDG_RUNTIME_DIR", private.path())
        .env("PIPEWIRE_REMOTE", "conch-voice-test-no-such-socket")
        .stdin(Stdio::null())
        .arg("devices")
        .output()
        .unwrap();
    assert_eq!(
        output.status.signal(),
        None,
        "the binary was killed by a signal when it called PipeWire: {:?}",
        output.status
    );
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.starts_with("Audio devices (PipeWire)\n  not listed: "),
        "{stdout}"
    );
    assert!(output.status.success(), "{:?}", output.status);
    // The keyboards are listed all the same.
    assert!(stdout.contains("What granting access means"), "{stdout}");
}
