//! Nothing derived from a key other than "talk, mute or deafen went down or up" is in
//! anything the client writes (`docs/design/conch-voice.md` §4).
//!
//! A whole session is run with a watcher on a pipe that carries, among the three configured
//! keys, many other keys: with their codes, their scan codes, and a time on every record.
//! Everything the session wrote is then searched for those codes, scan codes and times, in
//! decimal and in hex, and for the shapes a dump of the raw bytes would have. It is run
//! in-process, where what the session wrote is examined, and again as a process of its own
//! with the real logger installed at trace, where everything that reached standard output
//! and standard error is.
//!
//! The records are built by hand. No keyboard is read.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::io::Write;
use std::process::Command;
use std::sync::Arc;

use conch_voice::keydev::{self, DeviceRule};
use conch_voice::logger::Logger;
use conch_voice::secrets::Scrubber;
use conch_voice_control::LineCommand::Quit;
use log::LevelFilter;

use support::keyboard::{
    DEAFEN, EV_KEY, MICROSECONDS, MUTE, Pipe, SECONDS, TALK, bindings, press, quick_keys, record,
    release, stroke, write_when_open,
};
use support::stub::{Stub, presence};
use support::{Ended, Rig, Setup, until};

/// What the session shows the key device as. Not the pipe's path, which is random text
/// and could hold anything.
const KEYBOARD: &str = "the-test-keyboard";

/// What the environment variable that turns a test into the child process is called.
const CHILD: &str = "CONCH_VOICE_KEYS_CHILD";

/// Keys that are bound to nothing, with codes and scan codes unlike any other number the
/// client could write. The decoder compares a record's code with the three bindings and
/// drops it; these are dropped the same way as any letter.
const DISTINCTIVE: [(u16, i32); 8] = [
    (0x9e37, 0x07a5_c9e1),
    (0xb5c1, 0x06b3_d7f2),
    (0xd2a7, 0x05c4_e8a3),
    (0xe4f9, 0x04d5_f9b4),
    (0xa6b3, 0x03e6_a1c5),
    (0xc8d5, 0x02f7_b2d6),
    (0xf1e2, 0x01a8_c3e7),
    (0x8d4b, 0x09b9_d4f8),
];

/// Keys with the codes real keys have, which are small numbers. They are looked for as
/// whole numbers, and not in the once-a-second counts, where a count of frames could be
/// the same number by chance.
const REALISTIC: [(u16, i32); 3] = [(0x2c5, 0x7_0065), (0x2d6, 0x7_0066), (0x2e7, 0x7_0067)];

/// Typing: every other key struck once, with letters among them.
fn typing() -> Vec<u8> {
    let mut stream = Vec::new();
    for (code, scan) in DISTINCTIVE.into_iter().chain(REALISTIC) {
        stream.extend(stroke(code, scan));
    }
    for (letter, scan) in [(30, 0x7_0004), (48, 0x7_0005), (46, 0x7_0006)] {
        stream.extend(stroke(letter, scan));
    }
    assert!(stream.len() <= 4096, "one write, so one whole read");
    stream
}

/// What must appear nowhere, as text to search for.
fn needles() -> Vec<String> {
    let mut needles = Vec::new();
    for (code, scan) in DISTINCTIVE {
        let [low, high] = code.to_le_bytes();
        needles.extend([
            // The code and the scan code, in decimal and in hex.
            code.to_string(),
            format!("{code:x}"),
            scan.to_string(),
            format!("{scan:x}"),
            // The code's two bytes as a dump of the buffer would show them.
            format!("{low}, {high}"),
            format!("{low:x}, {high:x}"),
            format!("{low:#x}, {high:#x}"),
            format!("{low:02x}{high:02x}"),
            format!("{low:02x} {high:02x}"),
        ]);
    }
    // When each key was struck is the user's typing too.
    needles.extend([
        SECONDS.to_string(),
        format!("{SECONDS:x}"),
        MICROSECONDS.to_string(),
        format!("{MICROSECONDS:x}"),
    ]);
    needles
}

/// True if `token` is in `line` as a whole number or word.
fn has_token(line: &str, token: &str) -> bool {
    line.match_indices(token).any(|(at, _)| {
        let before = line[..at].chars().next_back();
        let after = line[at + token.len()..].chars().next();
        !before.is_some_and(|c| c.is_ascii_alphanumeric())
            && !after.is_some_and(|c| c.is_ascii_alphanumeric())
    })
}

/// Everything in `text` that came from a key.
fn leaks(text: &str) -> Vec<String> {
    let lower = text.to_ascii_lowercase();
    let mut found: Vec<String> = needles()
        .into_iter()
        .filter(|needle| lower.contains(needle))
        .collect();
    for line in lower
        .lines()
        .filter(|line| !line.contains(r#""event":"stats""#))
    {
        for (code, scan) in REALISTIC {
            for token in [
                code.to_string(),
                format!("{code:x}"),
                format!("{code:#x}"),
                scan.to_string(),
                format!("{scan:x}"),
                format!("{scan:#x}"),
            ] {
                if has_token(line, &token) {
                    found.push(format!("{token} in: {line}"));
                }
            }
        }
    }
    found
}

fn assert_no_key_data(what: &str, text: &str) {
    let found = leaks(text);
    assert!(found.is_empty(), "{what} holds key data {found:?}:\n{text}");
}

#[test]
fn the_search_finds_a_code_a_scan_code_a_time_and_a_dump_of_the_bytes() {
    // The search is only worth something if it finds what it looks for.
    for leaked in [
        "read key 40503".to_owned(),
        "code=0x9E37".to_owned(),
        "scan 128305633".to_owned(),
        "MSC_SCAN 7a5c9e1".to_owned(),
        "at 73588229205.703710".to_owned(),
        "key 709 pressed".to_owned(),
        "key 0x2c5 pressed".to_owned(),
        "{\"event\":\"key\",\"code\":743}".to_owned(),
        "scan=458853".to_owned(),
        format!("{:?}", typing()),
        format!("{:x?}", typing()),
        format!("{:?}", record(EV_KEY, 0xb5c1, 1)),
        typing().iter().map(|byte| format!("{byte:02x}")).collect(),
    ] {
        assert!(!leaks(&leaked).is_empty(), "not found in {leaked:.60}");
    }
    // And it does not cry wolf at numbers that only contain a small code, or at a count.
    for clean in [
        "listening on 127.0.0.1:40709",
        "retry in 1709 ms; attempt 1 of 3; os error 19",
        r#"{"event":"stats","frames_sent":709,"frames_sent_total":743}"#,
        "key device the-test-keyboard: open",
    ] {
        assert!(leaks(clean).is_empty(), "{clean}: {:?}", leaks(clean));
    }
    assert!(needles().iter().all(|needle| needle.len() >= 4));
}

/// A whole session driven from the "keyboard", with typing before, during and after
/// everything: a transmission, a mute, a deafen, a refused press, the device going away in
/// the middle of another key's record, coming back, and going away for good.
async fn whole_session(json: bool, also_stdout: bool) -> Ended {
    let pipe = Pipe::new();
    let conchd = Stub::start().await;
    conchd.presence(presence(&[(3, true), (7, false)]));
    let setup = Setup {
        json,
        also_stdout,
        key_device: Some(KEYBOARD.to_owned()),
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    keydev::spawn(
        pipe.path.clone(),
        bindings(),
        DeviceRule::AnyFileForTests,
        quick_keys(),
        rig.inputs(),
    )
    .unwrap();
    let mut keyboard = pipe.writer();

    // The lines to wait for, which read the same way in either mode.
    let (ready, muted, deafened, refused) = if json {
        (
            r#"{"blocked":[],"deafened":false,"event":"self","muted":false,"transmitting":false}"#,
            r#"{"blocked":["muted"],"deafened":false,"event":"self","muted":true,"transmitting":false}"#,
            r#"{"blocked":["deafened"],"deafened":true,"event":"self","muted":false,"transmitting":false}"#,
            r#"{"event":"press_ignored","reason":"muted"}"#,
        )
    } else {
        (
            "you: ready to talk",
            "you: muted",
            "you: deafened",
            "press ignored: muted",
        )
    };
    let shown =
        |line: &str, times: usize| rig.out.text().lines().filter(|l| *l == line).count() >= times;
    let key_device = |state: &str, times: usize| {
        rig.out
            .text()
            .lines()
            .filter(|line| line.contains("key_device") || line.starts_with("key device "))
            .filter(|line| match state {
                "open" => line.contains(r#""state":"open""#) || line.ends_with(": open"),
                _ => line.contains(r#""state":"missing""#) || line.contains("standard input"),
            })
            .count()
            >= times
    };

    until("ready", || rig.out.text(), || shown(ready, 1)).await;
    until("the device", || rig.out.text(), || key_device("open", 1)).await;

    // Typing, then the talk key held while more is typed.
    keyboard.write_all(&typing()).unwrap();
    keyboard.write_all(&press(TALK)).unwrap();
    rig.frames_beyond(2).await;
    keyboard.write_all(&typing()).unwrap();
    keyboard.write_all(&release(TALK)).unwrap();
    rig.reported(2).await;
    until("ready again", || rig.out.text(), || shown(ready, 2)).await;

    // Mute, a press that is refused, unmute; deafen, undeafen.
    keyboard.write_all(&press(MUTE)).unwrap();
    keyboard.write_all(&release(MUTE)).unwrap();
    until("muted", || rig.out.text(), || shown(muted, 1)).await;
    keyboard.write_all(&typing()).unwrap();
    keyboard.write_all(&press(TALK)).unwrap();
    keyboard.write_all(&release(TALK)).unwrap();
    until("the refusal", || rig.out.text(), || shown(refused, 1)).await;
    keyboard.write_all(&press(MUTE)).unwrap();
    keyboard.write_all(&release(MUTE)).unwrap();
    until("unmuted", || rig.out.text(), || shown(ready, 3)).await;
    keyboard.write_all(&press(DEAFEN)).unwrap();
    keyboard.write_all(&release(DEAFEN)).unwrap();
    until("deafened", || rig.out.text(), || shown(deafened, 1)).await;
    keyboard.write_all(&press(DEAFEN)).unwrap();
    keyboard.write_all(&release(DEAFEN)).unwrap();
    until("undeafened", || rig.out.text(), || shown(ready, 4)).await;

    // The device "goes away" in the middle of another key's record: for a moment the
    // decoder holds 23 bytes of it, code included, and the fault is reported.
    let (code, _) = DISTINCTIVE[0];
    keyboard.write_all(&record(EV_KEY, code, 1)[..23]).unwrap();
    until("the fault", || rig.out.text(), || key_device("missing", 1)).await;
    until(
        "the device again",
        || rig.out.text(),
        || key_device("open", 2),
    )
    .await;
    write_when_open(&mut keyboard, &typing());
    write_when_open(&mut keyboard, &press(TALK));
    rig.frames_beyond(rig.sdk.frames() + 1).await;
    keyboard.write_all(&typing()).unwrap();

    // And for good, with the talk key held.
    drop(keyboard);
    until("the loss", || rig.out.text(), || key_device("missing", 2)).await;
    rig.reported(4).await;

    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok(), "{result:?}");
    assert_eq!(
        ended.conchd.reported(),
        ["started", "stopped", "started", "stopped"],
        "the keys did what they are for"
    );
    ended
}

#[tokio::test]
async fn nothing_a_session_writes_holds_another_keys_code_in_json_or_in_plain_lines() {
    for json in [true, false] {
        let ended = whole_session(json, false).await;
        let written = ended.out.text();
        assert_no_key_data("the session's output", &written);
        // The session said what happened to the device, which is all it may say of it.
        assert!(written.contains(KEYBOARD), "{written}");
        assert!(
            written.contains("part of an event record"),
            "the fault was reported: {written}"
        );
        assert!(written.contains("p3"), "presence was shown: {written}");
    }
}

/// Not a test of its own: the session, run as a process with the real logger installed at
/// its most talkative, for the test below to read the output of. Without the environment
/// variable it does nothing.
#[tokio::test]
async fn child_process_runs_a_whole_session_with_the_real_logger_at_trace() {
    let Some(mode) = std::env::var_os(CHILD) else {
        return;
    };
    let scrubber = Scrubber::new();
    Logger::install(LevelFilter::Trace, Arc::clone(&scrubber)).expect("no logger yet");
    log::trace!(target: "conch_voice::keydev", "own-trace-marker");
    whole_session(mode == "json", true).await;
    std::io::stdout().flush().unwrap();
}

/// Runs this test binary again as the child above, and returns what it wrote.
fn run_child(mode: &str) -> (String, String) {
    let output = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "child_process_runs_a_whole_session_with_the_real_logger_at_trace",
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
        "the child failed:\n{stdout}\n{stderr}"
    );
    (stdout, stderr)
}

#[test]
fn nothing_a_whole_process_writes_holds_another_keys_code_even_at_trace() {
    for mode in ["json", "plain"] {
        let (stdout, stderr) = run_child(mode);
        assert_no_key_data("standard output", &stdout);
        assert_no_key_data("standard error", &stderr);

        // The session really ran and wrote where a terminal would see it, and the logger
        // really was at trace.
        let open = if mode == "json" {
            format!(r#"{{"device":"{KEYBOARD}","event":"key_device","state":"open"}}"#)
        } else {
            format!("key device {KEYBOARD}: open")
        };
        assert_eq!(
            stdout.lines().filter(|line| **line == open).count(),
            2,
            "{mode}: {stdout}"
        );
        assert!(
            stderr.contains("conch-voice: trace: own-trace-marker"),
            "{mode}: {stderr}"
        );
    }
}
