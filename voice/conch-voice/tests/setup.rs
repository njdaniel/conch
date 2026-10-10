//! The two commands that help set the key device up (`docs/design/conch-voice.md` §4 and
//! §8): `keys`, which says what it will show before it opens anything and writes only to a
//! terminal; and the keyboard half of `devices`, which says what is being granted before
//! it says how, grants one device, and never suggests a group.
//!
//! The "keyboards" are named pipes and files under a temporary directory. No test here
//! opens or lists anything under `/dev/input`.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use conch_voice::Error;
use conch_voice::devices::{self, Keyboard};
use conch_voice::keydev::{DeviceRule, Problem, Refusal};
use conch_voice::keys;

use support::keyboard::{EV_KEY, Pipe, press, record, release, stroke};

/// `keys` on `device`, as if on a terminal or not, with what it wrote.
fn run_keys(device: &Path, rule: DeviceRule, terminal: bool) -> (Result<(), Error>, String) {
    let mut written = Vec::new();
    let result = keys::run(device, rule, terminal, &mut written);
    (result, String::from_utf8(written).unwrap())
}

#[test]
fn keys_says_what_it_will_show_before_it_tries_the_device() {
    let dir = tempfile::tempdir().unwrap();
    let absent = dir.path().join("absent-event-kbd");
    let (result, written) = run_keys(&absent, DeviceRule::EventDevice, true);

    // The device is not there, so opening it failed; and by then all of the warning, and
    // nothing else, had been written.
    let error = result.unwrap_err();
    assert!(
        matches!(
            error,
            Error::KeyDevice {
                problem: Problem::CannotOpen(_),
                ..
            }
        ),
        "{error}"
    );
    assert_eq!(written, keys::warning(&absent.display().to_string()));
    assert!(written.contains("EVERY key pressed"), "{written}");
    assert!(
        written.contains("until you stop it with Ctrl-C"),
        "{written}"
    );
    assert!(
        written.contains("finding the code of the key you want to talk with"),
        "{written}"
    );
    assert_eq!(
        error.to_string(),
        format!(
            "key device {}: cannot open it: {}",
            absent.display(),
            std::io::Error::from_raw_os_error(2)
        )
    );
    assert_eq!(error.exit_code(), 1);
}

#[test]
fn keys_refuses_when_its_output_is_not_a_terminal_and_then_opens_and_writes_nothing() {
    // A pipe with nothing writing to it: opening it would never return.
    let pipe = Pipe::new();
    for rule in [DeviceRule::EventDevice, DeviceRule::AnyFileForTests] {
        let (result, written) = run_keys(&pipe.path, rule, false);
        let error = result.unwrap_err();
        assert!(matches!(error, Error::NotATerminal), "{error}");
        assert_eq!(error.exit_code(), 2);
        assert_eq!(written, "", "not even the warning");
    }
}

#[test]
fn keys_applies_the_watchers_check_and_reads_nothing_it_refuses() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("usb-Example-event-kbd");
    std::fs::write(&file, press(97)).unwrap();
    let (result, written) = run_keys(&file, DeviceRule::EventDevice, true);
    let error = result.unwrap_err();
    assert!(
        matches!(
            error,
            Error::KeyDevice {
                problem: Problem::Refused(Refusal::OutsideDevInput),
                ..
            }
        ),
        "{error}"
    );
    assert_eq!(written, keys::warning(&file.display().to_string()));
    assert!(!written.contains("key 97"), "{written}");
}

#[test]
fn keys_shows_the_code_and_name_of_each_key_pressed_and_nothing_else() {
    let pipe = Pipe::new();
    let path = pipe.path.clone();
    let shown = std::thread::spawn(move || run_keys(&path, DeviceRule::AnyFileForTests, true));
    let mut keyboard = pipe.writer();
    // A named key, a key with no name, and what is not a press: releases, an auto-repeat,
    // scan codes.
    let mut stream = stroke(97, 0x700e4);
    stream.extend(press(30));
    stream.extend(release(30));
    stream.extend(press(183));
    keyboard.write_all(&stream).unwrap();
    // The keyboard goes away, which is what ends `keys` here; Ctrl-C does otherwise.
    drop(keyboard);
    let (result, written) = shown.join().unwrap();

    let error = result.unwrap_err();
    assert!(
        matches!(
            error,
            Error::KeyDevice {
                problem: Problem::Ended,
                ..
            }
        ),
        "{error}"
    );
    let device = pipe.path.display().to_string();
    assert_eq!(
        written,
        format!(
            "{}\nReading. Press the key you want to talk with.\n\
             key 97: KEY_RIGHTCTRL   (talk = \"KEY_RIGHTCTRL\")\n\
             key 30   (talk = 30)\n\
             key 183: KEY_F13   (talk = \"KEY_F13\")\n",
            keys::warning(&device)
        )
    );
    // The warning came first.
    assert!(written.find("EVERY key pressed").unwrap() < written.find("Reading.").unwrap());
}

#[test]
fn keys_stops_at_part_of_a_record() {
    let pipe = Pipe::new();
    let path = pipe.path.clone();
    let shown = std::thread::spawn(move || run_keys(&path, DeviceRule::AnyFileForTests, true));
    let mut keyboard = pipe.writer();
    let mut stream = press(97);
    stream.extend_from_slice(&record(EV_KEY, 30, 1)[..10]);
    keyboard.write_all(&stream).unwrap();
    let (result, written) = shown.join().unwrap();
    assert!(
        matches!(
            result.unwrap_err(),
            Error::KeyDevice {
                problem: Problem::PartialRecord,
                ..
            }
        ),
        "{written}"
    );
    assert!(!written.contains("key 97"), "nothing of a faulty read");
}

const READABLE: &str = "usb-Example_Keyboard-event-kbd";
const LOCKED: &str = "usb-Locked_Board-if01-event-kbd";
/// Names with no business in a rule, or on a terminal.
const QUOTED: &str = "usb-Evil\" RUN+=\"x-event-kbd";
const ESCAPED: &str = "usb-Esc\u{1b}[2J\nline-event-kbd";

/// A directory standing in for `/dev/input/by-id`: two keyboards, one of them unreadable,
/// a mouse, and two keyboards with hostile names.
fn by_id() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    for name in [
        READABLE,
        LOCKED,
        "usb-Example_Mouse-event-mouse",
        QUOTED,
        ESCAPED,
    ] {
        std::fs::write(dir.path().join(name), press(97)).unwrap();
    }
    let locked = dir.path().join(LOCKED);
    std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o000)).unwrap();
    dir
}

/// The keyboard half of `devices` for that directory.
fn report(by_id: &Path, rule: DeviceRule, uid: Option<u32>) -> String {
    let mut written = Vec::new();
    devices::report(by_id, rule, uid, &mut written).unwrap();
    String::from_utf8(written).unwrap()
}

/// True when this user can open a file nobody may read: root.
fn reads_anything(dir: &Path) -> bool {
    std::fs::File::open(dir.join(LOCKED)).is_ok()
}

#[test]
fn devices_lists_the_keyboards_by_path_and_says_which_can_be_read() {
    let dir = by_id();
    let found = devices::keyboards(dir.path(), DeviceRule::AnyFileForTests).unwrap();
    let names: Vec<&str> = found
        .iter()
        .map(|keyboard| keyboard.name.as_str())
        .collect();
    assert_eq!(
        names,
        [ESCAPED, QUOTED, READABLE, LOCKED],
        "the keyboards, by name, and not the mouse"
    );
    assert_eq!(found[2].readable, Ok(()));
    if !reads_anything(dir.path()) {
        let Err(Problem::CannotOpen(error)) = found[3].readable else {
            panic!("the locked keyboard was readable");
        };
        assert_eq!(error.kind(), std::io::ErrorKind::PermissionDenied);
    }

    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));
    let directory = dir.path().display().to_string();
    let readable = format!("{directory}/usb-Example_Keyboard-event-kbd\n  you can read it: yes\n");
    assert!(text.contains(&readable), "{text}");
    if !reads_anything(dir.path()) {
        let locked = format!(
            "{directory}/usb-Locked_Board-if01-event-kbd\n  you can read it: no (cannot open it: {})\n",
            std::io::Error::from_raw_os_error(13)
        );
        assert!(text.contains(&locked), "{text}");
    }
    assert!(!text.contains("Mouse"), "{text}");
    assert!(text.contains("convention"), "{text}");

    // Under the real rule nothing in a temporary directory is a key device, and the
    // listing says so in the watcher's own words: it is the same check.
    let text = report(dir.path(), DeviceRule::EventDevice, Some(1000));
    assert_eq!(
        text.matches("you can read it: no (refused: it is not under /dev/input)")
            .count(),
        4,
        "{text}"
    );
    assert_eq!(
        devices::keyboards(dir.path(), DeviceRule::EventDevice).unwrap()[2],
        Keyboard {
            name: READABLE.to_owned(),
            readable: Err(Problem::Refused(Refusal::OutsideDevInput)),
        }
    );
}

#[test]
fn each_rule_devices_prints_names_that_one_device_and_this_user() {
    let dir = by_id();
    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));
    let rules: Vec<&str> = text
        .lines()
        .filter(|line| line.contains("SYMLINK=="))
        .map(str::trim)
        .collect();
    assert_eq!(
        rules,
        [
            "ACTION!=\"remove\", SUBSYSTEM==\"input\", KERNEL==\"event*\", \
             SYMLINK==\"input/by-id/usb-Example_Keyboard-event-kbd\", \
             RUN+=\"/usr/bin/setfacl -m u:1000:r $devnode\"",
            "ACTION!=\"remove\", SUBSYSTEM==\"input\", KERNEL==\"event*\", \
             SYMLINK==\"input/by-id/usb-Locked_Board-if01-event-kbd\", \
             RUN+=\"/usr/bin/setfacl -m u:1000:r $devnode\"",
        ],
        "one rule for each keyboard, naming it and no other; none for the name that is not fit"
    );
    for rule in &rules {
        assert_eq!(rule.matches("-event-kbd").count(), 1, "{rule}");
        assert_eq!(rule.matches("u:1000:r ").count(), 1, "read, for one user");
    }
    assert_eq!(
        text.matches("no rule is shown: this name has characters udev does not put in one")
            .count(),
        2,
        "{text}"
    );
    // A hostile name is shown, made fit for a terminal, and is in no command.
    assert!(text.contains("/usb-Esc [2J line-event-kbd\n"), "{text}");
    assert_eq!(text.matches("usb-Evil").count(), 1, "{text}");
    assert_eq!(text.matches("usb-Esc").count(), 1, "{text}");
    // The trial command is for the same one device and user.
    let trial = format!(
        "    sudo setfacl -m u:1000:r {}/usb-Example_Keyboard-event-kbd\n",
        dir.path().display()
    );
    assert!(text.contains(&trial), "{text}");
    assert!(
        text.contains("/etc/udev/rules.d/70-conch-voice.rules"),
        "{text}"
    );

    // Another user gets a rule for that user; root, or nobody known, gets none.
    let other = report(dir.path(), DeviceRule::AnyFileForTests, Some(1234));
    assert_eq!(other.matches("setfacl -m u:1234:r $devnode").count(), 2);
    assert!(!other.contains("u:1000"), "{other}");
    for uid in [Some(0), None] {
        let text = report(dir.path(), DeviceRule::AnyFileForTests, uid);
        assert!(!text.contains("SYMLINK=="), "{uid:?}: {text}");
        assert!(!text.contains("sudo setfacl"), "{uid:?}: {text}");
        assert!(text.contains("no rule is shown"), "{uid:?}: {text}");
    }
}

#[test]
fn devices_says_what_is_granted_before_how_and_never_suggests_a_group() {
    let dir = by_id();
    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));

    // What access to a keyboard exposes comes before any way of granting it.
    let what = text
        .find("lets a program see every key pressed on that")
        .unwrap();
    for how in [
        "udev",
        "setfacl",
        "SYMLINK==",
        "sudo",
        dir.path().to_str().unwrap(),
    ] {
        assert!(what < text.find(how).unwrap(), "{how} comes first:\n{text}");
    }
    assert!(text.contains("passwords included"), "{text}");
    assert!(
        text.contains("Any other program running as you gets the"),
        "{text}"
    );

    // It says why not the input group, and holds nothing that would add a user to one.
    assert!(
        text.contains(
            "Do not use the `input` group for this: being in that group grants every input device"
        ),
        "{text}"
    );
    let lower = text.to_ascii_lowercase();
    for adding in [
        "usermod",
        "gpasswd",
        "adduser",
        "useradd",
        "groupadd",
        "newgrp",
        "-ag",
        "-a -g",
        "add yourself",
        "add your user",
        "add the user",
        "join the",
        "member of the",
        "uaccess",
        "plugdev",
        "group=",
        "mode=",
        "owner=",
        "chmod",
        "chown",
        "chgrp",
    ] {
        assert!(!lower.contains(adding), "{adding}:\n{text}");
    }
    assert_eq!(
        lower.matches("group").count(),
        2,
        "the group is named only to say not to use it:\n{text}"
    );

    // What can swallow a release, and what ends such a transmission.
    for said in [
        "A program that grabs the keyboard stops every other reader seeing its events",
        "key\n  remapper",
        "virtual machine's keyboard passthrough",
        "QEMU toggles its\n  grab with both Ctrl keys",
        "A talk key released during a grab is not seen",
        "only the\n  transmit limit",
        "120 seconds",
        "Choose a talk key that such programs do not use",
    ] {
        assert!(text.contains(said), "{said:?}:\n{text}");
    }

    // Every line is fit for a terminal, whatever a device is called.
    assert!(
        text.chars().all(|c| c == '\n' || !c.is_control()),
        "{text:?}"
    );
}

#[test]
fn devices_with_no_keyboard_or_no_directory_still_says_what_matters() {
    let empty = tempfile::tempdir().unwrap();
    let text = report(empty.path(), DeviceRule::AnyFileForTests, Some(1000));
    assert!(
        text.contains("\n  none. A laptop's built-in keyboard"),
        "{text}"
    );
    assert!(text.contains("What granting access means"), "{text}");
    assert!(text.contains("What can swallow a release"), "{text}");
    assert!(!text.contains("SYMLINK=="), "{text}");

    let absent = empty.path().join("absent");
    let text = report(&absent, DeviceRule::AnyFileForTests, Some(1000));
    assert!(
        text.contains(&format!(
            "  cannot list it: {}",
            std::io::Error::from_raw_os_error(2)
        )),
        "{text}"
    );
    assert!(text.contains("What can swallow a release"), "{text}");
}

#[test]
fn devices_reads_nothing_from_a_keyboard() {
    // It opens each keyboard to see whether it can, and that is all: there is no read in
    // the module at all.
    let source =
        std::fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("src/devices.rs"))
            .unwrap();
    let code = source.split("#[cfg(test)]").next().unwrap();
    for reading in [".read(", "read_to_", "read_exact", "io::Read", "BufRead"] {
        assert!(!code.contains(reading), "{reading}");
    }
    assert!(code.contains("keydev::open(&entry.path(), rule).map(drop)"));
}
