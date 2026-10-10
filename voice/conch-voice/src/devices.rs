//! `conch-voice devices`, the keyboard half: which keyboards there are, whether this user
//! can read each, and how to grant read access to one (`docs/design/conch-voice.md` §4 and
//! §8). The audio half arrives with real devices (#184).
//!
//! Input: the entries of `/dev/input/by-id/`. Output: text on standard output. Owns
//! nothing: each keyboard is opened read-only to see whether it can be, and closed at once;
//! **nothing is read from any of them**.
//!
//! What the text keeps to:
//!
//! - It says what read access to a keyboard exposes **before** it says how to grant it.
//! - It grants one device to one user: a udev rule for that keyboard. It never suggests the
//!   `input` group, and says why not.
//! - It says what can swallow a release, and that only the transmit limit ends such a
//!   transmission.
//! - A rule is printed only for a name made of the characters udev itself puts in such a
//!   name, so nothing a device calls itself can become part of a rule by surprise; and
//!   every line passes [`one_line`].

use std::io::{self, Write};
use std::os::unix::fs::MetadataExt;
use std::path::Path;

use conch_voice_control::DEFAULT_MAX_TRANSMIT_SECS;

use crate::keydev::{self, DeviceRule, Problem};
use crate::secrets::one_line;

/// Where udev links each input device under a name that does not change between boots.
pub const BY_ID: &str = "/dev/input/by-id";

/// How udev ends the name of a keyboard's event device. It is a convention of udev's
/// rules, not a guarantee: some keyboards have a second entry for their media keys, and a
/// device that is not a keyboard can claim to be one.
const KEYBOARD_SUFFIX: &str = "-kbd";

/// The file the printed rule goes in. It must sort after the rules that make the
/// `by-id` links (60), since the rule matches on one.
const RULES_FILE: &str = "/etc/udev/rules.d/70-conch-voice.rules";

/// One keyboard found in the directory.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Keyboard {
    /// The entry's name, such as `usb-Example_Keyboard-event-kbd`.
    pub name: String,
    /// Whether it could be opened for reading as a key device, and if not, why.
    pub readable: Result<(), Problem>,
}

/// The keyboards in `by_id`, by name: every entry whose name ends in `-kbd`. Each is opened
/// read-only, by the same check the watcher applies, and closed at once without a read.
///
/// # Errors
///
/// What the operating system said, if the directory cannot be listed.
pub fn keyboards(by_id: &Path, rule: DeviceRule) -> io::Result<Vec<Keyboard>> {
    let mut found = Vec::new();
    for entry in std::fs::read_dir(by_id)? {
        let entry = entry?;
        let Ok(name) = entry.file_name().into_string() else {
            continue;
        };
        if !name.ends_with(KEYBOARD_SUFFIX) {
            continue;
        }
        // Opened and dropped: this is the whole of what is done with the device.
        let readable = keydev::open(&entry.path(), rule).map(drop);
        found.push(Keyboard { name, readable });
    }
    found.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(found)
}

/// Whether a device's name is made only of what udev leaves in such a name: letters,
/// digits and `#+-.:=@_`. Anything else is not put in a rule.
fn fit_for_a_rule(name: &str) -> bool {
    !name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "#+-.:=@_".contains(c))
}

/// The udev rule that grants `uid` read access to the one keyboard `name`, and to nothing
/// else. `None` for a name that is not fit to put in a rule.
///
/// It matches the event device that carries this `by-id` link, and gives the user an ACL
/// entry to read it. It changes neither the device's owner nor its group, and grants no
/// write access.
#[must_use]
pub fn udev_rule(name: &str, uid: u32) -> Option<String> {
    fit_for_a_rule(name).then(|| {
        format!(
            "ACTION!=\"remove\", SUBSYSTEM==\"input\", KERNEL==\"event*\", \
             SYMLINK==\"input/by-id/{name}\", RUN+=\"/usr/bin/setfacl -m u:{uid}:r $devnode\""
        )
    })
}

/// The user this process runs as, from the owner of its own entry under `/proc`.
#[must_use]
pub fn current_uid() -> Option<u32> {
    std::fs::metadata("/proc/self")
        .ok()
        .map(|metadata| metadata.uid())
}

/// What read access to a keyboard exposes. Printed before any way of granting it.
const WHAT_IS_GRANTED: &str = "\
What granting access means
  Read access to a keyboard's event device lets a program see every key pressed on that
  keyboard, in every application, passwords included. conch-voice acts on the keys you
  configure for talk, mute and deafen and discards every other key as it reads; but that is
  a property of its code, not of the permission. Any other program running as you gets the
  same access. So grant it for one keyboard, to one user, and only if you accept that.

  Do not use the `input` group for this: being in that group grants every input device on
  the machine, to every program you run. The rule below grants one device.
";

/// What can keep a release from being seen.
fn what_swallows_a_release() -> String {
    format!(
        "\
What can swallow a release
  A program that grabs the keyboard stops every other reader seeing its events: a key
  remapper does, and so does a virtual machine's keyboard passthrough (QEMU toggles its
  grab with both Ctrl keys). A talk key released during a grab is not seen, and only the
  transmit limit (`max_transmit_secs` under `[audio]`, {DEFAULT_MAX_TRANSMIT_SECS} seconds unless set) ends that
  transmission. Choose a talk key that such programs do not use.
"
    )
}

fn readable_line(readable: &Result<(), Problem>) -> String {
    match readable {
        Ok(()) => "  you can read it: yes".to_owned(),
        Err(problem) => format!("  you can read it: no ({problem})"),
    }
}

/// How to grant read access to one keyboard, and how to take it back.
fn grant(device: &str, name: &str, uid: Option<u32>) -> Vec<String> {
    let Some(uid) = uid else {
        return vec![
            "  no rule is shown: this user's id could not be found (is /proc mounted?)".to_owned(),
        ];
    };
    if uid == 0 {
        return vec![
            "  no rule is shown: you are root, and root needs none. Run this as the user who"
                .to_owned(),
            "  will run conch-voice.".to_owned(),
        ];
    }
    let Some(rule) = udev_rule(name, uid) else {
        return vec![
            "  no rule is shown: this name has characters udev does not put in one".to_owned(),
        ];
    };
    vec![
        format!("  to let user {uid} (you) read this one keyboard, put this line, as root, in"),
        format!("  {RULES_FILE}:"),
        String::new(),
        format!("    {rule}"),
        String::new(),
        "  then run `sudo udevadm control --reload` and unplug and replug the keyboard (or"
            .to_owned(),
        "  `sudo udevadm trigger --subsystem-match=input --action=change`).".to_owned(),
        "  For a trial that lasts until the keyboard is replugged or the machine restarts:"
            .to_owned(),
        String::new(),
        format!("    sudo setfacl -m u:{uid}:r {device}"),
        String::new(),
        format!("  To take it back, delete the line and run `sudo setfacl -x u:{uid} {device}`."),
        "  Then name it in the configuration file ($XDG_CONFIG_HOME/conch/voice.toml):".to_owned(),
        String::new(),
        "    [keys]".to_owned(),
        format!("    device = \"{device}\""),
        "    talk = \"KEY_RIGHTCTRL\"".to_owned(),
        String::new(),
        format!("  `conch-voice keys {device}` shows the code of any other key."),
    ]
}

/// Writes the keyboard half of `conch-voice devices`: what access means, the keyboards in
/// `by_id` with whether each can be read and the rule that would grant it to `uid`, and
/// what can swallow a release.
///
/// `by_id` is [`BY_ID`] and `rule` is [`DeviceRule::EventDevice`] everywhere but in tests.
///
/// # Errors
///
/// What the operating system said, if `out` cannot be written to. A directory that cannot
/// be listed is not an error: the text says so and the rest is still printed.
pub fn report(
    by_id: &Path,
    rule: DeviceRule,
    uid: Option<u32>,
    out: &mut dyn Write,
) -> io::Result<()> {
    let directory = one_line(&by_id.display().to_string());
    let mut lines: Vec<String> = Vec::new();
    // What is being granted comes before any keyboard, and so before any way to grant it.
    lines.extend(WHAT_IS_GRANTED.lines().map(str::to_owned));
    lines.push(String::new());
    lines.push(format!("Keyboards in {directory}"));
    lines.push(format!(
        "  (entries whose name ends in `{KEYBOARD_SUFFIX}`: that is udev's convention for a keyboard's"
    ));
    lines.push(
        "  event device, not a guarantee that it is one or that it is the only one)".to_owned(),
    );
    match keyboards(by_id, rule) {
        Ok(found) if found.is_empty() => {
            lines.push(String::new());
            lines.push(
                "  none. A laptop's built-in keyboard has no entry there; look for one ending"
                    .to_owned(),
            );
            lines.push("  in `-kbd` under /dev/input/by-path instead.".to_owned());
        }
        Ok(found) => {
            for keyboard in found {
                let device = format!("{directory}/{}", keyboard.name);
                lines.push(String::new());
                lines.push(device.clone());
                lines.push(readable_line(&keyboard.readable));
                lines.extend(grant(&device, &keyboard.name, uid));
            }
        }
        Err(error) => {
            lines.push(String::new());
            lines.push(format!("  cannot list it: {error}"));
        }
    }
    lines.push(String::new());
    lines.extend(what_swallows_a_release().lines().map(str::to_owned));
    for line in lines {
        writeln!(out, "{}", one_line(&line))?;
    }
    out.flush()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_rule_names_one_device_and_one_user_and_grants_reading_only() {
        let rule = udev_rule("usb-Example_Keyboard-event-kbd", 1000).unwrap();
        assert_eq!(
            rule,
            "ACTION!=\"remove\", SUBSYSTEM==\"input\", KERNEL==\"event*\", \
             SYMLINK==\"input/by-id/usb-Example_Keyboard-event-kbd\", \
             RUN+=\"/usr/bin/setfacl -m u:1000:r $devnode\""
        );
        assert!(!rule.contains("GROUP"), "{rule}");
        assert!(!rule.contains("MODE"), "{rule}");
        assert!(!rule.contains("uaccess"), "{rule}");
    }

    #[test]
    fn a_name_that_could_change_the_rule_gets_no_rule() {
        for name in [
            "",
            "usb-Evil\", RUN+=\"/bin/sh-event-kbd",
            "usb-Evil $devnode-event-kbd",
            "usb-Evil\nKERNEL==\"*\"-event-kbd",
            "usb-Evil/../x-event-kbd",
            "usb-Evil\\x-event-kbd",
            "usb-Evil*-event-kbd",
        ] {
            assert_eq!(udev_rule(name, 1000), None, "{name:?}");
        }
        assert!(udev_rule("usb-0c45:7403_USB#Kbd+a=b@c.d-event-kbd", 1000).is_some());
    }
}
