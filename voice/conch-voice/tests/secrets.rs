//! No join token, room name or login token in anything the client writes
//! (`docs/design/conch-voice.md` §5, §13 row 10).
//!
//! A whole session is run against the stub `conchd` and the fake SDK with distinctive fake
//! values for all three. The fake SDK behaves as the real one was measured to and worse: it
//! writes log records that carry the token and the room name at every level, and fails a
//! join with an error that quotes them. The session is run twice in-process, where what
//! the session wrote is examined, and twice more as a process of its own with the real
//! logger installed, where everything that reached standard output and standard error is.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::process::Command;
use std::sync::Arc;

use conch_voice::logger::Logger;
use conch_voice::sdk::{DisconnectReason, SdkEvent};
use conch_voice::secrets::{REDACTED, Scrubber};
use conch_voice_control::LineCommand::{Down, Quit, Up};
use log::{Level, LevelFilter};

use support::fake::{Call, Record};
use support::stub::{FAKE_JOIN, FAKE_LOGIN, FAKE_ROOM, Stub, join_token, presence, room_name};
use support::{Ended, Rig, Setup, until};

/// Shaped like a signed token and registered nowhere: what LiveKit sends the SDK later.
const FAKE_JWT: &str = "eyJGQUtFIjoiaGVhZGVyIn0.eyJGQUtFIjoiY2xhaW1zIn0.RkFLRS1zaWduYXR1cmU";

/// The targets the SDK and what it links log under.
const SDK_TARGETS: [&str; 9] = [
    "livekit::room",
    "livekit::rtc_engine::rtc_session",
    "libwebrtc",
    "webrtc",
    "tungstenite::handshake::client",
    "tokio_tungstenite",
    "reqwest::connect",
    "hyper::proto::h1",
    "rustls::client",
];

/// What the environment variable that turns a test into the child process is called.
const CHILD: &str = "CONCH_VOICE_SECRETS_CHILD";

/// Every secret the session below handles, and the one it never sees.
fn secrets() -> Vec<String> {
    let mut secrets = vec![
        FAKE_LOGIN.to_owned(),
        FAKE_JOIN.to_owned(),
        FAKE_ROOM.to_owned(),
        FAKE_JWT.to_owned(),
    ];
    for n in 1..=3 {
        secrets.push(join_token(n));
        secrets.push(room_name(n));
    }
    secrets
}

fn assert_no_secret(what: &str, text: &str) {
    for secret in secrets() {
        assert!(
            !text.contains(&secret),
            "{what} holds a secret (one beginning {:?}):\n{}",
            &secret[..12],
            text.replace(&secret, "<<<THE SECRET>>>")
        );
    }
}

/// Records as the SDK writes them while it joins, at every level, each carrying the token,
/// the room name, the login and a token of LiveKit's own.
fn sdk_records() -> Vec<Record> {
    let mut records = Vec::new();
    for target in SDK_TARGETS {
        for level in [
            Level::Trace,
            Level::Debug,
            Level::Info,
            Level::Warn,
            Level::Error,
        ] {
            let name = level.as_str().to_ascii_lowercase();
            records.push(Record {
                level,
                target,
                text: format!(
                    "sdk-{name}-marker GET /rtc Authorization: Bearer {{token}} room={{room}} \
                     login={FAKE_LOGIN} refreshed={FAKE_JWT}"
                ),
            });
        }
    }
    records
}

/// A whole session: a join that fails with an error quoting the secrets, a join that
/// works, a press, a reconnect with a republish, a room deletion and the join that
/// follows, and `quit`. It waits on what the fake and the stub saw, so it reads the same
/// with plain output as with JSON.
async fn whole_session(scrubber: Arc<Scrubber>, json: bool, also_stdout: bool) -> Ended {
    let conchd = Stub::start().await;
    conchd.presence(presence(&[(3, true), (7, false)]));
    let setup = Setup {
        json,
        also_stdout,
        ..Setup::default()
    };
    let rig = Rig::start_scrubbing(scrubber, conchd, setup, |sdk| {
        sdk.log_on_connect(sdk_records());
        sdk.fail_next_connect(
            "could not open wss://livekit.invalid/rtc?access_token={token} for room {room}",
        );
    })
    .await;
    // The line that says a press would transmit, and the one that says the SDK is
    // reconnecting.
    let (ready, reconnecting) = if json {
        (
            r#"{"blocked":[],"deafened":false,"event":"self","muted":false,"transmitting":false}"#,
            r#"{"event":"connection","state":"reconnecting"}"#,
        )
    } else {
        ("you: ready to talk", "not connected: reconnecting")
    };
    let shown =
        |line: &str, times: usize| rig.out.text().lines().filter(|l| *l == line).count() >= times;

    // The first join failed and the second, with a session of its own, worked.
    until("ready", || rig.out.text(), || shown(ready, 1)).await;
    assert_eq!(rig.sdk.tokens(), [join_token(1), join_token(2)]);
    // While the connection lives the scrubber knows its token and its room, and it has
    // forgotten those of the attempt that failed.
    assert_eq!(rig.scrubber.scrub(&join_token(2)), REDACTED);
    assert_eq!(rig.scrubber.scrub(&room_name(2)), REDACTED);
    assert_eq!(rig.scrubber.scrub(&room_name(1)), room_name(1));

    rig.line(Down);
    rig.frames_beyond(2).await;
    rig.line(Up);
    rig.reported(2).await;
    until(
        "ready after the press",
        || rig.out.text(),
        || shown(ready, 2),
    )
    .await;

    // The SDK's own reconnect, with a warning of the kind nobody measured.
    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    until("reconnecting", || rig.out.text(), || shown(reconnecting, 1)).await;
    log::warn!(
        target: "livekit::rtc_engine",
        "sdk-late-marker resume failed for {} with {} then {FAKE_JWT}",
        room_name(2),
        join_token(2)
    );
    rig.sdk.republish();
    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    until("ready again", || rig.out.text(), || shown(ready, 3)).await;

    // The room is rotated: a third session and a third join.
    rig.sdk.disconnect(DisconnectReason::RoomDeleted);
    until(
        "the third join",
        || rig.out.text(),
        || rig.sdk.count(Call::Connect) == 3,
    )
    .await;
    until(
        "ready in the new room",
        || rig.out.text(),
        || shown(ready, 4),
    )
    .await;
    assert_eq!(rig.scrubber.scrub(&room_name(3)), REDACTED);
    assert_eq!(
        rig.scrubber.scrub(&room_name(2)),
        room_name(2),
        "the room that was left is forgotten"
    );

    rig.line(Quit);
    let scrubber = Arc::clone(&rig.scrubber);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok(), "{result:?}");
    assert_eq!(
        scrubber.scrub(&room_name(3)),
        room_name(3),
        "and nothing of a connection is kept once the client has left"
    );
    assert_eq!(scrubber.scrub(FAKE_LOGIN), REDACTED);
    ended
}

#[tokio::test]
async fn nothing_a_session_writes_holds_a_token_or_a_room_name_in_json_or_in_plain_lines() {
    for json in [true, false] {
        let ended = whole_session(Scrubber::new(), json, false).await;
        let written = ended.out.text();
        assert_no_secret("the session's output", &written);
        // The failed join's error was shown, with both values replaced: so they were
        // there to be replaced, and the scrubber knew them when it mattered.
        let failure = format!(
            "could not open wss://livekit.invalid/rtc?access_token={REDACTED} for room {REDACTED}"
        );
        assert!(written.contains(&failure), "{written}");
        // The session did what it was meant to: three joins, one press.
        assert_eq!(ended.sdk.count(Call::Connect), 3);
        assert_eq!(ended.conchd.reported(), ["started", "stopped"]);
        assert!(written.contains("p3"), "presence was shown: {written}");
    }
}

/// Not a test of its own: the session, run as a process with the real logger installed,
/// for the test below to read the output of. Without the environment variable it does
/// nothing.
#[tokio::test]
async fn child_process_runs_a_whole_session_with_the_real_logger() {
    let Some(mode) = std::env::var_os(CHILD) else {
        return;
    };
    let scrubber = Scrubber::new();
    // As `main` does, first, and at the most talkative level a user can ask for.
    Logger::install(LevelFilter::Trace, Arc::clone(&scrubber)).expect("no logger yet");
    log::info!(target: "conch_voice::session", "own-info-marker");
    whole_session(scrubber, mode == "json", true).await;
}

/// Runs this test binary again as the child above, and returns what it wrote.
fn run_child(mode: &str) -> (String, String) {
    let output = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "child_process_runs_a_whole_session_with_the_real_logger",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(CHILD, mode)
        .output()
        .unwrap();
    let stdout = String::from_utf8(output.stdout).unwrap();
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(
        output.status.success(),
        "the child failed:\n{}\n{}",
        stdout.replace("FAKE", "F4KE"),
        stderr.replace("FAKE", "F4KE")
    );
    (stdout, stderr)
}

#[test]
fn nothing_a_whole_process_writes_to_standard_output_or_standard_error_holds_a_secret() {
    for mode in ["json", "plain"] {
        let (stdout, stderr) = run_child(mode);
        assert_no_secret("standard output", &stdout);
        assert_no_secret("standard error", &stderr);

        // The session really ran, and really wrote where a terminal would see it.
        let connected = if mode == "json" {
            r#"{"event":"connection","state":"connected"}"#
        } else {
            "connected"
        };
        assert!(
            stdout.lines().filter(|line| *line == connected).count() >= 3,
            "{mode}: {stdout}"
        );

        // The logger: every SDK target's warnings and errors got through, scrubbed...
        for target in SDK_TARGETS {
            for level in ["warn", "error"] {
                let line = format!(
                    "conch-voice: {level}: {target}: sdk-{level}-marker GET /rtc Authorization: \
                     Bearer {REDACTED} room={REDACTED} login={REDACTED} refreshed={REDACTED}"
                );
                // Once for each of the three joins.
                assert_eq!(
                    stderr.lines().filter(|written| *written == line).count(),
                    3,
                    "{mode}: {line}\n{stderr}"
                );
            }
        }
        assert!(
            stderr.contains(&format!(
                "conch-voice: warn: livekit::rtc_engine: sdk-late-marker resume failed for \
                 {REDACTED} with {REDACTED} then {REDACTED}"
            )),
            "{mode}: {stderr}"
        );
        // ...nothing of theirs below a warning did, though the level asked for is trace...
        for level in ["trace", "debug", "info"] {
            assert!(
                !stderr.contains(&format!("sdk-{level}-marker")),
                "{mode}: {stderr}"
            );
        }
        // ...and this program's own records at that level did.
        assert!(
            stderr.contains("conch-voice: info: own-info-marker"),
            "{mode}: {stderr}"
        );
    }
}
