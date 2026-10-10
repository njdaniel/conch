//! The key device's watcher (`docs/design/conch-voice.md` §4): the talk, mute and deafen
//! keys reach the session loop and no other key does; every way a read can go wrong is the
//! device going away, which shuts the gate as for a release; and only an event device is
//! ever read.
//!
//! The "keyboard" is a named pipe under a temporary directory, written with records built
//! by hand. No test here opens or lists anything under `/dev/input`.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::io::Write;
use std::path::Path;
use std::time::Duration;

use conch_voice::keydev::{self, DeviceRule, DeviceState, OsError, Problem, Refusal};
use conch_voice::session::Input;
use conch_voice_control::LineCommand::{Deafen, Down, Mute, Up};
use conch_voice_control::{Key, KeyAction, KeyEvent};
use serde_json::json;
use tokio::sync::mpsc;

use support::keyboard::{
    DEAFEN, EV_KEY, EV_SYN, MUTE, Pipe, SYN_DROPPED, TALK, bindings, next, nothing_for, press,
    quick_keys, record, release, stroke, write_when_open,
};
use support::stub::Stub;
use support::{Rig, Setup, several_frames};

/// The name the session shows for the key device. Not the pipe's path, which is random.
const KEYBOARD: &str = "the-test-keyboard";

fn key(key: Key, action: KeyAction) -> Input {
    Input::Key(KeyEvent { key, action })
}

const READY: Input = Input::KeyDevice(DeviceState::Ready);

fn missing(problem: Problem) -> Input {
    Input::KeyDevice(DeviceState::Missing(problem))
}

/// A watcher on `path`, under the rule only tests may use, with what it sends.
fn watch(path: &Path) -> mpsc::UnboundedReceiver<Input> {
    let (inputs, received) = mpsc::unbounded_channel();
    keydev::spawn(
        path.to_owned(),
        bindings(),
        DeviceRule::AnyFileForTests,
        quick_keys(),
        inputs,
    )
    .unwrap();
    received
}

#[test]
fn the_configured_keys_reach_the_loop_and_no_other_key_does() {
    let pipe = Pipe::new();
    let mut inputs = watch(&pipe.path);
    let mut keyboard = pipe.writer();
    assert_eq!(next(&mut inputs), READY);

    // Other keys before, between and after; held across the talk press; and events that
    // are not keys but carry the talk key's number.
    let mut stream = Vec::new();
    stream.extend(stroke(30, 0x7001e));
    stream.extend(press(42));
    stream.extend(record(4, 4, i32::from(TALK)));
    stream.extend(press(TALK));
    stream.extend(record(EV_KEY, TALK, 2));
    stream.extend(stroke(31, 0x70016));
    stream.extend(release(TALK));
    stream.extend(release(42));
    stream.extend(press(MUTE));
    stream.extend(release(MUTE));
    stream.extend(stroke(0x2c5, 0x7a5c9));
    stream.extend(press(DEAFEN));
    stream.extend(release(DEAFEN));
    // Events were lost while the talk key was down: that is a release.
    stream.extend(press(TALK));
    stream.extend(record(EV_SYN, SYN_DROPPED, 0));
    stream.extend(stroke(32, 0x70007));
    keyboard.write_all(&stream).unwrap();

    for expected in [
        key(Key::Talk, KeyAction::Press),
        key(Key::Talk, KeyAction::Release),
        key(Key::Mute, KeyAction::Press),
        key(Key::Mute, KeyAction::Release),
        key(Key::Deafen, KeyAction::Press),
        key(Key::Deafen, KeyAction::Release),
        key(Key::Talk, KeyAction::Press),
        key(Key::Talk, KeyAction::Release),
    ] {
        assert_eq!(next(&mut inputs), expected);
    }
    nothing_for(&mut inputs, Duration::from_millis(30));
}

#[test]
fn part_of_a_record_is_the_device_going_away_and_the_decoder_starts_again() {
    let pipe = Pipe::new();
    let mut inputs = watch(&pipe.path);
    let mut keyboard = pipe.writer();
    assert_eq!(next(&mut inputs), READY);

    // A whole press of the talk key and then ten bytes of another record, in one read. An
    // event device never does this, so nothing in that read is believed: no press.
    let mut faulty = press(TALK);
    faulty.extend_from_slice(&record(EV_KEY, 30, 1)[..10]);
    keyboard.write_all(&faulty).unwrap();
    assert_eq!(next(&mut inputs), missing(Problem::PartialRecord));

    // The device is opened again. Had the ten bytes been kept, every record after them
    // would be read out of step and this press would never be seen.
    write_when_open(&mut keyboard, &press(TALK));
    assert_eq!(next(&mut inputs), READY);
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Press));

    // Again, this time with nothing whole in the read.
    keyboard.write_all(&record(EV_KEY, TALK, 0)[..23]).unwrap();
    assert_eq!(next(&mut inputs), missing(Problem::PartialRecord));
    write_when_open(&mut keyboard, &release(TALK));
    assert_eq!(next(&mut inputs), READY);
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Release));
    nothing_for(&mut inputs, Duration::from_millis(30));
}

#[test]
fn a_read_of_nothing_is_the_device_going_away_and_it_is_opened_again_when_it_is_back() {
    let pipe = Pipe::new();
    let mut inputs = watch(&pipe.path);
    let mut keyboard = pipe.writer();
    assert_eq!(next(&mut inputs), READY);
    keyboard.write_all(&press(TALK)).unwrap();
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Press));

    // The keyboard goes away with the key held: the read returns nothing.
    drop(keyboard);
    assert_eq!(next(&mut inputs), missing(Problem::Ended));
    // Said once, however many times the watcher tries to open it.
    nothing_for(&mut inputs, Duration::from_millis(60));

    // It is back, and it worked before, so it is said to be open as soon as it is.
    let mut keyboard = pipe.writer();
    assert_eq!(next(&mut inputs), READY);
    keyboard.write_all(&release(TALK)).unwrap();
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Release));
}

#[test]
fn a_read_error_is_the_device_going_away_and_is_said_once_however_often_it_recurs() {
    // A directory can be opened, and every read of it fails.
    let dir = tempfile::tempdir().unwrap();
    let mut inputs = watch(dir.path());
    assert_eq!(next(&mut inputs), READY);
    let Input::KeyDevice(DeviceState::Missing(Problem::ReadFailed(error))) = next(&mut inputs)
    else {
        panic!("not a read error");
    };
    assert_eq!(error.kind(), std::io::ErrorKind::IsADirectory);
    // It opens and fails again every few milliseconds, and is not announced again: a
    // device that has never given one whole read is not said to be open a second time.
    nothing_for(&mut inputs, Duration::from_millis(60));
}

#[test]
fn a_device_that_cannot_be_opened_is_said_once_and_opened_when_it_appears() {
    let pipe = Pipe::unmade();
    let mut inputs = watch(&pipe.path);
    let Input::KeyDevice(DeviceState::Missing(Problem::CannotOpen(error))) = next(&mut inputs)
    else {
        panic!("not a failure to open");
    };
    assert_eq!(error.kind(), std::io::ErrorKind::NotFound);
    assert!(Problem::CannotOpen(error).retried());
    nothing_for(&mut inputs, Duration::from_millis(60));

    pipe.make();
    let mut keyboard = pipe.writer();
    assert_eq!(next(&mut inputs), READY);
    keyboard.write_all(&press(TALK)).unwrap();
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Press));
}

#[test]
fn a_regular_file_and_a_pipe_are_refused_by_the_real_rule_and_nothing_is_read_from_them() {
    let pipe = Pipe::new();
    let file = pipe.dir.path().join("file");
    // A file that holds a press of the talk key: if it were read, the press would be seen.
    std::fs::write(&file, press(TALK)).unwrap();
    let link = pipe.dir.path().join("link-event-kbd");
    std::os::unix::fs::symlink(&file, &link).unwrap();

    let outside = Problem::Refused(Refusal::OutsideDevInput);
    for path in [&file, &link, &pipe.path] {
        // The pipe has no writer: opening it would never return, so this also shows that
        // what is refused is not opened.
        assert_eq!(
            keydev::open(path, DeviceRule::EventDevice).map(drop),
            Err(outside),
            "{}",
            path.display()
        );
    }
    // A character device that is no keyboard, and is not under /dev/input.
    assert_eq!(
        keydev::open(Path::new("/dev/null"), DeviceRule::EventDevice).map(drop),
        Err(outside)
    );

    // The watcher under the real rule: one line's worth, then nothing, and no retry.
    let (inputs, mut received) = mpsc::unbounded_channel();
    keydev::spawn(
        file.clone(),
        bindings(),
        DeviceRule::EventDevice,
        quick_keys(),
        inputs,
    )
    .unwrap();
    assert_eq!(next(&mut received), missing(outside));
    assert!(!outside.retried());
    nothing_for(&mut received, Duration::from_millis(60));
    // The refusal ended the watcher: it holds the channel no longer.
    assert!(matches!(
        received.try_recv(),
        Err(mpsc::error::TryRecvError::Disconnected)
    ));

    // The same file under the rule only tests may use is read: that rule is the only way
    // past the check.
    let mut inputs = watch(&file);
    assert_eq!(next(&mut inputs), READY);
    assert_eq!(next(&mut inputs), key(Key::Talk, KeyAction::Press));
}

/// Everything in `src/` outside its unit tests.
fn sources() -> Vec<(String, String)> {
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    std::fs::read_dir(src)
        .unwrap()
        .map(|entry| {
            let path = entry.unwrap().path();
            let text = std::fs::read_to_string(&path).unwrap();
            // A file's unit tests are its last part.
            let code = text.split("#[cfg(test)]").next().unwrap().to_owned();
            (
                path.file_name().unwrap().to_string_lossy().into_owned(),
                code,
            )
        })
        .collect()
}

#[test]
fn the_rule_for_tests_is_named_nowhere_but_where_it_is_defined() {
    let sources = sources();
    assert!(sources.len() >= 15, "src/ was read");
    for (file, code) in &sources {
        let named = code.matches("AnyFileForTests").count();
        if file == "keydev.rs" {
            // Its definition and the one match on the rule.
            assert_eq!(named, 2, "{file}");
        } else {
            assert_eq!(named, 0, "{file} names the rule that only tests may pass");
        }
    }
    // What starts the watcher and the two commands passes the real rule, written out.
    let lib = &sources.iter().find(|(file, _)| file == "lib.rs").unwrap().1;
    assert_eq!(lib.matches("DeviceRule::EventDevice").count(), 3);
    assert_eq!(lib.matches("keydev::spawn(").count(), 1);
    // And neither the command line nor the configuration can name a rule at all.
    let cli = &sources.iter().find(|(file, _)| file == "cli.rs").unwrap().1;
    assert!(!cli.contains("DeviceRule::"), "cli.rs");
    let main = &sources
        .iter()
        .find(|(file, _)| file == "main.rs")
        .unwrap()
        .1;
    assert!(!main.contains("DeviceRule"), "main.rs");
}

#[test]
fn nothing_under_src_reads_a_key_device_but_the_watcher_and_the_keys_command() {
    for (file, code) in sources() {
        let opens = code.contains("keydev::open(") || code.contains("fn open(");
        let expected = matches!(file.as_str(), "keydev.rs" | "keys.rs" | "devices.rs");
        assert_eq!(opens, expected, "{file}");
        // No crate for input devices, no ioctl, no unsafe: plain file I/O.
        for forbidden in ["unsafe ", "ioctl(", "libc::", " nix::", "evdev::"] {
            assert!(!code.contains(forbidden), "{file}: {forbidden}");
        }
    }
}

/// A session with a watcher on a pipe, and the keyboard's end of the pipe.
async fn rig_with_keyboard() -> (Rig, Pipe, std::fs::File) {
    let pipe = Pipe::new();
    let setup = Setup {
        key_device: Some(KEYBOARD.to_owned()),
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    keydev::spawn(
        pipe.path.clone(),
        bindings(),
        DeviceRule::AnyFileForTests,
        quick_keys(),
        rig.inputs(),
    )
    .unwrap();
    let keyboard = pipe.writer();
    rig.until("the key device", |rig| {
        rig.events_named("key_device").len() == 1
    })
    .await;
    rig.ready().await;
    (rig, pipe, keyboard)
}

fn key_device_states(rig: &Rig) -> Vec<String> {
    rig.events_named("key_device")
        .iter()
        .map(|event| {
            assert_eq!(event["device"], KEYBOARD);
            match event["state"].as_str().unwrap() {
                "open" => "open".to_owned(),
                _ => format!("missing: {}", event["reason"].as_str().unwrap()),
            }
        })
        .collect()
}

#[tokio::test]
async fn the_talk_key_opens_the_gate_and_its_release_shuts_it() {
    let (rig, _pipe, mut keyboard) = rig_with_keyboard().await;
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "nothing before a press");

    // Other keys do nothing at all.
    keyboard.write_all(&stroke(30, 0x7001e)).unwrap();
    keyboard.write_all(&press(42)).unwrap();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0);
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());

    keyboard.write_all(&press(TALK)).unwrap();
    rig.frames_beyond(5).await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    keyboard.write_all(&release(TALK)).unwrap();
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "nothing after the release");
    rig.reported(2).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert_eq!(key_device_states(&rig), ["open"]);
}

#[tokio::test]
async fn the_device_vanishing_while_the_talk_key_is_held_shuts_the_gate() {
    // Two ways the pipe can make a read go wrong while the key is down: the keyboard goes
    // away, and it writes part of a record.
    for (way, reason) in [("closed", "ended"), ("fragment", "partial_record")] {
        let (rig, pipe, mut keyboard) = rig_with_keyboard().await;
        keyboard.write_all(&press(TALK)).unwrap();
        rig.frames_beyond(3).await;

        let keyboard = if way == "closed" {
            drop(keyboard);
            None
        } else {
            keyboard.write_all(&record(EV_KEY, 30, 1)[..10]).unwrap();
            // Kept open, so that the pipe is not also at its end.
            Some(keyboard)
        };
        // The loss shut the gate and ended the transmission. (With the fragment the pipe
        // is still there, so the device is open again a moment later: look at what was
        // said, not only at what is said now.)
        rig.until("the gate to shut", |rig| {
            rig.events_named("self").iter().any(|own| {
                own["blocked"] == json!(["key_device_lost"]) && own["transmitting"] == false
            })
        })
        .await;
        let sent = rig.sdk.frames();
        several_frames().await;
        assert_eq!(
            rig.sdk.frames(),
            sent,
            "{way}: not one frame after the device went away, though the key was never released"
        );
        assert_eq!(rig.sdk.frames_while_muted(), 0, "{way}");
        assert!(rig.sdk.is_muted(), "{way}");
        rig.reported(2).await;
        assert_eq!(rig.conchd.reported(), ["started", "stopped"], "{way}");
        assert_eq!(
            key_device_states(&rig)[..2],
            ["open".to_owned(), format!("missing: {reason}")],
            "{way}"
        );

        // The device is back. The press that was cut off does not come back with it.
        let mut keyboard = keyboard.unwrap_or_else(|| pipe.writer());
        rig.until("the device again", |rig| {
            rig.own()["blocked"] == json!([]) && key_device_states(rig).len() == 3
        })
        .await;
        assert_eq!(
            key_device_states(&rig)[2],
            "open",
            "{way}: each change said once"
        );
        several_frames().await;
        assert_eq!(rig.sdk.frames(), sent, "{way}: only a new press transmits");

        write_when_open(&mut keyboard, &press(TALK));
        rig.frames_beyond(sent + 2).await;
        keyboard.write_all(&release(TALK)).unwrap();
        rig.not_talking().await;
        rig.reported(4).await;
        assert_eq!(
            rig.conchd.reported(),
            ["started", "stopped", "started", "stopped"],
            "{way}"
        );
    }
}

#[tokio::test]
async fn every_reason_the_watcher_gives_for_a_lost_device_shuts_the_gate() {
    let denied = OsError::from(&std::io::Error::from_raw_os_error(19));
    for problem in [
        Problem::ReadFailed(denied),
        Problem::Ended,
        Problem::PartialRecord,
    ] {
        let rig = Rig::start().await;
        rig.ready().await;
        rig.input(Input::KeyDevice(DeviceState::Ready));
        rig.input(key(Key::Talk, KeyAction::Press));
        rig.frames_beyond(3).await;

        rig.input(missing(problem));
        rig.until("the gate to shut", |rig| {
            rig.own()["blocked"] == json!(["key_device_lost"]) && rig.own()["transmitting"] == false
        })
        .await;
        let sent = rig.sdk.frames();
        several_frames().await;
        assert_eq!(rig.sdk.frames(), sent, "{problem}");
        assert!(rig.sdk.is_muted(), "{problem}");
        rig.reported(2).await;
        assert_eq!(rig.conchd.reported(), ["started", "stopped"], "{problem}");
        let said = rig.events_named("key_device").pop().unwrap();
        assert_eq!(said["reason"], problem.code());
        assert_eq!(said["detail"], problem.to_string());
        assert_eq!(said["retrying"], true);

        // A press while the device is gone is refused, and says why.
        rig.input(key(Key::Talk, KeyAction::Press));
        rig.until("the refusal", |rig| {
            !rig.events_named("press_ignored").is_empty()
        })
        .await;
        assert_eq!(
            rig.events_named("press_ignored")[0]["reason"],
            "key_device_lost"
        );
        several_frames().await;
        assert_eq!(rig.sdk.frames(), sent, "{problem}");
    }
}

#[tokio::test]
async fn a_device_that_could_never_be_opened_is_said_once_and_standard_input_still_works() {
    let setup = Setup {
        key_device: Some(KEYBOARD.to_owned()),
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    let pipe = Pipe::unmade();
    keydev::spawn(
        pipe.path.clone(),
        bindings(),
        DeviceRule::AnyFileForTests,
        quick_keys(),
        rig.inputs(),
    )
    .unwrap();
    rig.until("the key device", |rig| {
        !rig.events_named("key_device").is_empty()
    })
    .await;
    rig.ready().await;
    // Many attempts later it has been said once, and it does not block a press: there was
    // never a key held on it.
    several_frames().await;
    assert_eq!(key_device_states(&rig), ["missing: cannot_open"]);
    assert_eq!(rig.own()["blocked"], json!([]));

    rig.line(Down);
    rig.frames_beyond(3).await;
    rig.line(Up);
    rig.not_talking().await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn mute_and_deafen_work_from_keys_and_from_standard_input_through_one_machine() {
    let (rig, _pipe, mut keyboard) = rig_with_keyboard().await;

    // Muted by the key: the talk key then does nothing, and says why.
    keyboard.write_all(&press(MUTE)).unwrap();
    keyboard.write_all(&release(MUTE)).unwrap();
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    keyboard.write_all(&press(TALK)).unwrap();
    rig.until("the refusal", |rig| {
        rig.events_named("press_ignored").len() == 1
    })
    .await;
    assert_eq!(rig.events_named("press_ignored")[0]["reason"], "muted");
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "the gate obeys mute");
    keyboard.write_all(&release(TALK)).unwrap();

    // Unmuted from standard input: it is the same state, whichever source changes it.
    rig.line(Mute);
    rig.until("unmuted", |rig| rig.own()["muted"] == false)
        .await;
    // Muted from standard input, unmuted by the key.
    rig.line(Mute);
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    keyboard.write_all(&press(MUTE)).unwrap();
    keyboard.write_all(&release(MUTE)).unwrap();
    rig.until("unmuted", |rig| rig.own()["muted"] == false)
        .await;

    // Deafened by the key in the middle of a transmission: the gate shuts at once.
    keyboard.write_all(&press(TALK)).unwrap();
    rig.frames_beyond(3).await;
    keyboard.write_all(&press(DEAFEN)).unwrap();
    keyboard.write_all(&release(DEAFEN)).unwrap();
    rig.until("deafened", |rig| {
        rig.own()["deafened"] == true && rig.own()["transmitting"] == false
    })
    .await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "the gate obeys deafen");
    keyboard.write_all(&release(TALK)).unwrap();

    // Undeafened from standard input, and the talk key works again.
    rig.line(Deafen);
    rig.until("undeafened", |rig| rig.own()["blocked"] == json!([]))
        .await;
    keyboard.write_all(&press(TALK)).unwrap();
    rig.frames_beyond(sent + 2).await;
    keyboard.write_all(&release(TALK)).unwrap();
    rig.not_talking().await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[test]
fn under_the_real_rule_the_path_is_checked_before_it_is_opened_and_the_file_before_it_is_read() {
    // No test may open a character device under /dev/input, so the one path through the
    // real rule that ends with a file cannot be run here. Its parts are tested (the rule
    // on made-up paths, the check of the opened file on /dev/null); this holds their order
    // in place: resolve, apply the rule, open, check what was opened, and only then hand
    // the file over.
    let sources = sources();
    let keydev = &sources
        .iter()
        .find(|(file, _)| file == "keydev.rs")
        .unwrap()
        .1;
    let real = keydev
        .split("DeviceRule::EventDevice => {")
        .nth(1)
        .unwrap()
        .split("Ok(file)")
        .next()
        .unwrap();
    let steps = [
        "std::fs::canonicalize(device)",
        "std::fs::symlink_metadata(&canonical)",
        "permitted(&canonical, named.file_type().is_char_device()).map_err(Problem::Refused)?;",
        "File::open(&canonical)",
        "file.metadata()",
        "confirm(&named, &opened).map_err(Problem::Refused)?;",
    ];
    let mut from = 0;
    for step in steps {
        let at = real[from..]
            .find(step)
            .unwrap_or_else(|| panic!("{step} is missing or out of order"));
        from += at + step.len();
    }
    assert_eq!(real.matches("File::open(").count(), 1);
    assert!(!real.contains(".read("), "nothing is read before the check");
}
