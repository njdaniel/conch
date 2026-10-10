//! The two commands that help set the key device up (`docs/design/conch-voice.md` §4 and
//! §8): `keys`, which says what it will show before it opens anything and writes only to a
//! terminal; and the keyboard half of `devices`, which says what is being granted before
//! it says how, grants one device, and never suggests a group.
//!
//! The "keyboards" are named pipes and files under a temporary directory. No test here
//! opens or lists anything under `/dev/input`.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::ffi::OsStr;
use std::io::Write;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::time::Duration;

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

/// Names with no business in a rule, in a command or on a terminal: what udev's rule
/// syntax gives a meaning to, control characters, a right-to-left override (which is not a
/// control character, and reorders what a terminal shows), and letters that are not ASCII.
const HOSTILE: [&str; 12] = [
    "usb-Evil\" RUN+=\"x-event-kbd",
    "usb-Esc\u{1b}[2J\nline-event-kbd",
    "usb-Dollar$devnode-event-kbd",
    "usb-Percent%k-event-kbd",
    "usb-Star*-event-kbd",
    "usb-Alt|event*-event-kbd",
    "usb-Sp ace-event-kbd",
    "usb-Hex\\x20name-event-kbd",
    "usb-Cone\u{9b}2J-event-kbd",
    "usb-Bidi\u{202e}dbk-tneve-event-kbd",
    "usb-Comma,RUN+=x-event-kbd",
    "usb-Accent\u{e9}-event-kbd",
];

/// A name that is not text at all.
const NOT_TEXT: &[u8] = b"usb-Bytes\xff\xfe-event-kbd";

/// A directory standing in for `/dev/input/by-id`: two keyboards, one of them unreadable,
/// and a mouse.
fn by_id() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    for name in [READABLE, LOCKED, "usb-Example_Mouse-event-mouse"] {
        std::fs::write(dir.path().join(name), press(97)).unwrap();
    }
    let locked = dir.path().join(LOCKED);
    std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o000)).unwrap();
    dir
}

/// That directory with every hostile name in it as well.
fn by_id_with_hostile_names() -> tempfile::TempDir {
    let dir = by_id();
    for name in HOSTILE {
        std::fs::write(dir.path().join(name), press(97)).unwrap();
    }
    std::fs::write(dir.path().join(OsStr::from_bytes(NOT_TEXT)), press(97)).unwrap();
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
        .keyboards
        .iter()
        .map(|keyboard| keyboard.name.as_str())
        .collect();
    assert_eq!(
        names,
        [READABLE, LOCKED],
        "the keyboards, by name, and not the mouse"
    );
    assert_eq!(found.unshown, 0);
    assert_eq!(found.keyboards[0].readable, Ok(()));
    if !reads_anything(dir.path()) {
        let Err(Problem::CannotOpen(error)) = found.keyboards[1].readable else {
            panic!("the locked keyboard was readable");
        };
        assert_eq!(error.kind(), std::io::ErrorKind::PermissionDenied);
    }

    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));
    let directory = dir.path().display().to_string();
    let readable = format!("{directory}/{READABLE}\n  you can read it: yes\n");
    assert!(text.contains(&readable), "{text}");
    if !reads_anything(dir.path()) {
        let locked = format!(
            "{directory}/{LOCKED}\n  you can read it: no (cannot open it: {})\n",
            std::io::Error::from_raw_os_error(13)
        );
        assert!(text.contains(&locked), "{text}");
    }
    assert!(!text.contains("Mouse"), "{text}");
    assert!(text.contains("convention"), "{text}");
    assert!(!text.contains("that are not shown"), "{text}");

    // Under the real rule nothing in a temporary directory is a key device, and the
    // listing says so in the watcher's own words: it is the same check.
    let text = report(dir.path(), DeviceRule::EventDevice, Some(1000));
    assert_eq!(
        text.matches("you can read it: no (refused: it is not under /dev/input)")
            .count(),
        2,
        "{text}"
    );
    assert_eq!(
        devices::keyboards(dir.path(), DeviceRule::EventDevice)
            .unwrap()
            .keyboards[0],
        Keyboard {
            name: READABLE.to_owned(),
            readable: Err(Problem::Refused(Refusal::OutsideDevInput)),
        }
    );
}

#[test]
fn each_rule_devices_prints_names_that_one_device_and_this_user() {
    let dir = by_id_with_hostile_names();
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
        "one rule for each keyboard, naming it and no other; none for a name that is not fit"
    );
    for rule in &rules {
        assert_eq!(rule.matches("-event-kbd").count(), 1, "{rule}");
        assert_eq!(rule.matches("u:1000:r ").count(), 1, "read, for one user");
    }
    // The trial command is for the same one device and user.
    let trial = format!(
        "    sudo setfacl -m u:1000:r {}/{READABLE}\n",
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
fn the_names_a_rule_may_hold_are_exactly_the_ones_udev_itself_gives() {
    // Every character from U+0000 to U+2FFF, and a few beyond, alone in an otherwise
    // plain name. The set that is let into a rule is pinned, so that it cannot be widened
    // by a character at a time: ASCII letters and digits and `#+-.:=@_`, and nothing else.
    let beyond = ['\u{202e}', '\u{feff}', '\u{ff21}', '\u{1d7d8}', '\u{e0041}'];
    let mut accepted = String::new();
    for c in (0u32..=0x2fff).filter_map(char::from_u32).chain(beyond) {
        if devices::udev_rule(&format!("usb-A{c}B-event-kbd"), 1000).is_some() {
            accepted.push(c);
        }
    }
    assert_eq!(
        accepted,
        "#+-.0123456789:=@ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"
    );
    // What udev's rule syntax and its matching give a meaning to, and what a shell does.
    for special in [
        '"', '\\', '$', '%', '*', '?', '[', ']', '|', ',', ' ', '\'', '\n', '\t', '/', '{', '}',
        '!', ';', '&', '(', ')', '<', '>', '`', '~', '^',
    ] {
        assert_eq!(
            devices::udev_rule(&format!("usb-A{special}B-event-kbd"), 1000),
            None,
            "{special:?}"
        );
    }
    // Letters and digits that are not ASCII are not udev's either.
    for foreign in ['\u{e9}', '\u{3a9}', '\u{663}', '\u{430}'] {
        assert_eq!(
            devices::udev_rule(&format!("usb-A{foreign}B-event-kbd"), 1000),
            None,
            "{foreign:?}"
        );
    }
    assert_eq!(devices::udev_rule("", 1000), None);
}

#[test]
fn a_keyboard_whose_name_is_not_fit_for_a_rule_is_counted_and_not_shown() {
    let dir = by_id_with_hostile_names();
    let found = devices::keyboards(dir.path(), DeviceRule::AnyFileForTests).unwrap();
    assert_eq!(found.keyboards.len(), 2);
    assert_eq!(found.unshown, HOSTILE.len() + 1);

    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));
    assert!(
        text.contains("\n\n  13 entries whose names have characters that are not shown\n"),
        "{text}"
    );
    // Nothing of any such name is printed: no path, no rule, no command, not a character.
    for part in [
        "Evil",
        "Esc",
        "Dollar",
        "Percent",
        "Star",
        "Alt",
        "Sp ace",
        "Hex",
        "Cone",
        "Bidi",
        "dbk-tneve",
        "Comma",
        "Accent",
        "Bytes",
        "[2J",
        "$devnode-",
        "%k",
        "RUN+=x",
    ] {
        assert!(!text.contains(part), "{part}:\n{text}");
    }
    assert!(
        text.is_ascii(),
        "nothing but ASCII reaches the terminal: {text:?}"
    );
    assert!(
        text.chars().all(|c| c == '\n' || !c.is_control()),
        "{text:?}"
    );
    // The two plain keyboards have everything, and nothing else has anything.
    assert_eq!(text.matches("SYMLINK==").count(), 2, "{text}");
    assert_eq!(text.matches("sudo setfacl -m").count(), 2, "{text}");
    assert_eq!(text.matches("sudo setfacl -x").count(), 2, "{text}");
    assert_eq!(text.matches("device = \"").count(), 2, "{text}");
    assert_eq!(text.matches("you can read it:").count(), 2, "{text}");

    // One such entry, and no keyboard besides: it is said, and "none" is not.
    let alone = tempfile::tempdir().unwrap();
    std::fs::write(alone.path().join(HOSTILE[9]), b"").unwrap();
    let text = report(alone.path(), DeviceRule::AnyFileForTests, Some(1000));
    assert!(
        text.contains("\n\n  one entry whose name has characters that are not shown\n"),
        "{text}"
    );
    assert!(!text.contains("none."), "{text}");
    assert!(!text.contains('\u{202e}') && text.is_ascii(), "{text:?}");
    assert!(!text.contains("SYMLINK=="), "{text}");
}

#[test]
fn an_entry_that_is_not_shown_is_not_opened_either() {
    // A pipe with nothing writing to it, under a name that is not shown: opening it would
    // never return, so that the listing comes back at all shows it was left alone.
    let dir = tempfile::tempdir().unwrap();
    let pipe = dir.path().join("usb-Sp ace-event-kbd");
    let made = std::process::Command::new("mkfifo")
        .arg(&pipe)
        .status()
        .unwrap();
    assert!(made.success());
    let directory = dir.path().to_owned();
    let (done, listed) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = done.send(devices::keyboards(&directory, DeviceRule::AnyFileForTests));
    });
    let found = listed
        .recv_timeout(Duration::from_secs(10))
        .expect("the listing opened an entry it does not show")
        .unwrap();
    assert!(found.keyboards.is_empty());
    assert_eq!(found.unshown, 1);
}

#[test]
fn devices_says_everything_the_grant_gives_before_how_and_never_suggests_a_group() {
    let dir = by_id();
    let text = report(dir.path(), DeviceRule::AnyFileForTests, Some(1000));

    // Where the ways of granting begin: the listing, and everything in it.
    let how = text.find("Keyboards in").unwrap();
    for way in ["setfacl", "SYMLINK==", "sudo", dir.path().to_str().unwrap()] {
        assert!(how < text.find(way).unwrap(), "{way} comes first:\n{text}");
    }
    // Everything the grant gives is said before that.
    for given in [
        // Every key.
        "lets a program see every key pressed on that\n  keyboard, in every application, passwords included",
        // More than reading: the keyboard can be taken, and changed.
        "It lets a program do more than read",
        "the kernel asks for no write access before a program takes the keyboard for itself",
        "nothing else (the desktop included) sees its keys until it lets go",
        "keyboard's key map and repeat rate until it is replugged",
        // A user, not a session.
        "The grant is to a user, not to a login session",
        "at the login screen, in another user's session and at a console",
        "On a shared machine, granting it to someone gives them the means to capture\n  what others type on that keyboard",
        // A property of the code, not of the permission.
        "Any\n  other program running as you gets the same access",
        // One name, not one piece of hardware.
        "\"One keyboard\" means one name: the rule below matches the name udev gave the device",
        "second keyboard of the same model with no serial number",
        "a device that presents the\n  same identity strings, gets the same access",
    ] {
        let at = text
            .find(given)
            .unwrap_or_else(|| panic!("not said: {given:?}\n{text}"));
        assert!(at < how, "said after the how: {given:?}\n{text}");
    }

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

    // Taking it back: the rule and the entry, and then the programs that have it open.
    for said in [
        "To take it back, delete the line and run `sudo setfacl -x u:1000 ",
        "then unplug and replug the keyboard, or stop every program that has it open.",
        "Permission is checked when a device is opened: a program that already has it open",
        "(a running conch-voice included) goes on reading until then.",
    ] {
        assert_eq!(text.matches(said).count(), 2, "{said:?}:\n{text}");
    }

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

    // Every line is fit for a terminal.
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
