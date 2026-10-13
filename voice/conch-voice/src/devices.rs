//! `conch-voice devices`: the microphones and speakers PipeWire has, by the names the
//! configuration takes; and the keyboards, whether this user can read each, and how to grant
//! read access to one (`docs/design/conch-voice.md` §4 and §8).
//!
//! Input: PipeWire's list of devices (a `NodeTable`, which the caller reads and this module
//! only formats), and the entries of `/dev/input/by-id/`. Output: text on standard output.
//! Owns nothing: **no audio device is opened**, and each keyboard is opened read-only to see
//! whether it can be, and closed at once; **nothing is read from any of them**.
//!
//! The audio half lists a device only under a name that can be written into the
//! configuration file as it stands: a name with a control character, a quotation mark or a
//! backslash is counted and not shown.
//!
//! What the text keeps to:
//!
//! - It says what read access to a keyboard gives **before** it says how to grant it, and
//!   all of it: every key, the means to take the keyboard from everything else, for a user
//!   and not a session, and for whatever carries that name.
//! - It grants one device to one user: a udev rule for that keyboard. It never suggests the
//!   `input` group, and says why not.
//! - It says that taking the grant back does not stop a program that has the device open.
//! - It says what can swallow a release, and that only the transmit limit ends such a
//!   transmission.
//! - A keyboard is shown, and a rule printed, only for a name made of the characters udev
//!   itself puts in such a name. Any other entry is counted and not shown: nothing a
//!   device calls itself can become part of a rule, a command or the terminal's display.
//!   Every line passes [`one_line`] as well.

use std::io::{self, Write};
use std::os::unix::fs::MetadataExt;
use std::path::Path;

use conch_voice_control::DEFAULT_MAX_TRANSMIT_SECS;
use dmn_audio::NodeTable;

use crate::keydev::{self, DeviceRule, Problem};
use crate::secrets::{cut, one_line};

/// The longest description shown; PipeWire's descriptions are a device's marketing name.
const DESCRIPTION_CHARS: usize = 80;

/// One device of the audio half.
struct Listed<'a> {
    name: &'a str,
    description: &'a str,
    is_default: bool,
}

/// Whether a `node.name` can be written between quotes in the configuration file as it is.
fn fit_for_the_config(name: &str) -> bool {
    !name.is_empty() && one_line(name) == name && !name.contains(['"', '\\'])
}

/// Adds the devices of one kind, in the order PipeWire lists them, to `lines`: those whose
/// names are fit to show, and a count of those that are not. Returns the name to use in the
/// example: the default's if it is shown, else the first.
fn audio_section<'a>(
    lines: &mut Vec<String>,
    title: &str,
    key: &str,
    devices: impl Iterator<Item = (&'a str, &'a str)>,
    default: Option<&str>,
) -> Option<String> {
    let mut shown = Vec::new();
    let mut unshown = 0_usize;
    for (name, description) in devices {
        if fit_for_the_config(name) {
            shown.push(Listed {
                name,
                description,
                is_default: default == Some(name),
            });
        } else {
            unshown += 1;
        }
    }
    lines.push(format!("{title} (`{key}`)"));
    if shown.is_empty() && unshown == 0 {
        lines.push("  none".to_owned());
    }
    for device in &shown {
        let mark = if device.is_default { "  [default]" } else { "" };
        lines.push(format!("  {}{mark}", device.name));
        let description = one_line(device.description);
        if !description.is_empty() && description != device.name {
            lines.push(format!("      {}", cut(description, DESCRIPTION_CHARS)));
        }
    }
    match unshown {
        0 => {}
        1 => lines.push("  one device whose name has characters that are not shown".to_owned()),
        more => lines.push(format!(
            "  {more} devices whose names have characters that are not shown"
        )),
    }
    shown
        .iter()
        .find(|device| device.is_default)
        .or(shown.first())
        .map(|device| device.name.to_owned())
}

/// Writes the audio half of `conch-voice devices`: the microphones and the speakers in
/// `table`, by the names `input` and `output` under `[audio]` take, the defaults marked.
/// Nothing is opened.
///
/// # Errors
///
/// What the operating system said, if `out` cannot be written to.
pub fn audio_report(table: &NodeTable, out: &mut dyn Write) -> io::Result<()> {
    let mut lines: Vec<String> = vec![
        "Audio devices (PipeWire)".to_owned(),
        "  Name one under [audio] in the configuration file, or leave `input` and `output`"
            .to_owned(),
        "  out, or write \"default\", to follow PipeWire's default as it changes.".to_owned(),
    ];
    if !table.synced {
        lines.push(
            "  PipeWire had not finished listing its devices; this list may be incomplete."
                .to_owned(),
        );
    }
    lines.push(String::new());
    let input = audio_section(
        &mut lines,
        "Microphones",
        "input",
        table
            .sources()
            .map(|node| (node.name.as_str(), node.description.as_str())),
        table.default_source.as_deref(),
    );
    lines.push(String::new());
    let output = audio_section(
        &mut lines,
        "Speakers",
        "output",
        table
            .sinks()
            .map(|node| (node.name.as_str(), node.description.as_str())),
        table.default_sink.as_deref(),
    );
    if input.is_some() || output.is_some() {
        lines.push(String::new());
        lines.push("  For example, in $XDG_CONFIG_HOME/conch/voice.toml:".to_owned());
        lines.push(String::new());
        lines.push("    [audio]".to_owned());
        if let Some(name) = input {
            lines.push(format!("    input = \"{name}\""));
        }
        if let Some(name) = output {
            lines.push(format!("    output = \"{name}\""));
        }
    }
    lines.push(String::new());
    lines.push(
        "  A microphone named there that is not plugged in is not replaced by the default:"
            .to_owned(),
    );
    lines.push(
        "  nothing is transmitted until it is. A speaker named there that is not plugged in"
            .to_owned(),
    );
    lines.push("  is not replaced either; nothing is heard until it is.".to_owned());
    lines.push(String::new());
    for line in lines {
        writeln!(out, "{}", one_line(&line))?;
    }
    out.flush()
}

/// Writes the audio half when PipeWire cannot be reached, so that the keyboards are still
/// listed.
///
/// # Errors
///
/// What the operating system said, if `out` cannot be written to.
pub fn audio_unavailable(why: &str, out: &mut dyn Write) -> io::Result<()> {
    writeln!(out, "Audio devices (PipeWire)")?;
    writeln!(out, "  not listed: {}", one_line(why))?;
    writeln!(out)?;
    out.flush()
}

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

/// What the directory holds that claims to be a keyboard.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Found {
    /// The keyboards whose names are fit to show, by name.
    pub keyboards: Vec<Keyboard>,
    /// How many more entries end in `-kbd` under a name that is not shown. Nothing is done
    /// with them: they are not opened, and no path, rule or command is printed for them.
    pub unshown: usize,
}

/// The keyboards in `by_id`, by name: every entry whose name ends in `-kbd`. Each one whose
/// name is fit to show is opened read-only, by the same check the watcher applies, and
/// closed at once without a read.
///
/// # Errors
///
/// What the operating system said, if the directory cannot be listed.
pub fn keyboards(by_id: &Path, rule: DeviceRule) -> io::Result<Found> {
    let mut found = Found::default();
    for entry in std::fs::read_dir(by_id)? {
        let entry = entry?;
        let name = entry.file_name();
        if !name
            .as_encoded_bytes()
            .ends_with(KEYBOARD_SUFFIX.as_bytes())
        {
            continue;
        }
        match name.into_string() {
            Ok(name) if fit_for_a_rule(&name) => {
                // Opened and dropped: this is the whole of what is done with the device.
                let readable = keydev::open(&entry.path(), rule).map(drop);
                found.keyboards.push(Keyboard { name, readable });
            }
            _ => found.unshown += 1,
        }
    }
    found.keyboards.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(found)
}

/// Whether a device's name is made only of what udev leaves in such a name: ASCII letters
/// and digits and `#+-.:=@_`. Anything else is not put in a rule, and is not shown at all:
/// a name can hold characters that reorder or hide text on a terminal without being
/// control characters.
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

/// What read access to a keyboard gives. Printed before any way of granting it.
const WHAT_IS_GRANTED: &str = "\
What granting access means
  Read access to a keyboard's event device lets a program see every key pressed on that
  keyboard, in every application, passwords included. It lets a program do more than read:
  the kernel asks for no write access before a program takes the keyboard for itself, so
  that nothing else (the desktop included) sees its keys until it lets go, or changes the
  keyboard's key map and repeat rate until it is replugged.

  The grant is to a user, not to a login session. It covers what is typed on that keyboard
  at the login screen, in another user's session and at a console, for anything running as
  that user. On a shared machine, granting it to someone gives them the means to capture
  what others type on that keyboard.

  conch-voice acts on the keys you configure for talk, mute and deafen and discards every
  other key as it reads; but that is a property of its code, not of the permission. Any
  other program running as you gets the same access. So grant it for one keyboard, to one
  user, and only if you accept that.

  \"One keyboard\" means one name: the rule below matches the name udev gave the device. A
  second keyboard of the same model with no serial number, or a device that presents the
  same identity strings, gets the same access.

  Do not use the `input` group for this: being in that group grants every input device on
  the machine, to every program you run.
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
        // Not reached from `report`, which shows only names that are fit for a rule.
        return vec!["  no rule is shown: this name is not one udev gives a device".to_owned()];
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
        format!("  To take it back, delete the line and run `sudo setfacl -x u:{uid} {device}`,"),
        "  then unplug and replug the keyboard, or stop every program that has it open.".to_owned(),
        "  Permission is checked when a device is opened: a program that already has it open"
            .to_owned(),
        "  (a running conch-voice included) goes on reading until then.".to_owned(),
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
        Ok(found) if found.keyboards.is_empty() && found.unshown == 0 => {
            lines.push(String::new());
            lines.push(
                "  none. A laptop's built-in keyboard has no entry there; look for one ending"
                    .to_owned(),
            );
            lines.push("  in `-kbd` under /dev/input/by-path instead.".to_owned());
        }
        Ok(found) => {
            for keyboard in found.keyboards {
                let device = format!("{directory}/{}", keyboard.name);
                lines.push(String::new());
                lines.push(device.clone());
                lines.push(readable_line(&keyboard.readable));
                lines.extend(grant(&device, &keyboard.name, uid));
            }
            // Counted, and nothing more: no name, no path, no rule and no command.
            match found.unshown {
                0 => {}
                1 => {
                    lines.push(String::new());
                    lines.push(
                        "  one entry whose name has characters that are not shown".to_owned(),
                    );
                }
                more => {
                    lines.push(String::new());
                    lines.push(format!(
                        "  {more} entries whose names have characters that are not shown"
                    ));
                }
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
    use dmn_audio::RegistryEvent;

    use super::*;

    fn node(id: u32, class: &str, name: &str, description: &str) -> RegistryEvent {
        RegistryEvent::Added {
            id,
            props: vec![
                ("media.class".to_owned(), class.to_owned()),
                ("node.name".to_owned(), name.to_owned()),
                ("node.description".to_owned(), description.to_owned()),
            ],
        }
    }

    fn table(events: Vec<RegistryEvent>) -> NodeTable {
        let mut table = NodeTable::default();
        for event in events {
            table.apply(event);
        }
        table
    }

    fn audio_text(table: &NodeTable) -> String {
        let mut written = Vec::new();
        audio_report(table, &mut written).unwrap();
        String::from_utf8(written).unwrap()
    }

    #[test]
    fn the_audio_half_lists_microphones_and_speakers_by_name_and_marks_the_defaults() {
        let text = audio_text(&table(vec![
            node(1, "Audio/Source", "alsa_input.usb-headset", "Headset Mono"),
            node(2, "Audio/Source", "alsa_input.builtin", "Built-in Audio"),
            node(3, "Audio/Sink", "alsa_output.builtin", "Built-in Audio"),
            node(4, "Audio/Sink", "alsa_output.usb-headset", "Headset"),
            // Neither a microphone nor a speaker.
            node(5, "Stream/Output/Audio", "firefox", "Firefox"),
            RegistryEvent::DefaultSource(Some("alsa_input.builtin".to_owned())),
            RegistryEvent::DefaultSink(Some("alsa_output.usb-headset".to_owned())),
            RegistryEvent::Synced,
        ]));
        let want = "\
Audio devices (PipeWire)
  Name one under [audio] in the configuration file, or leave `input` and `output`
  out, or write \"default\", to follow PipeWire's default as it changes.

Microphones (`input`)
  alsa_input.usb-headset
      Headset Mono
  alsa_input.builtin  [default]
      Built-in Audio

Speakers (`output`)
  alsa_output.builtin
      Built-in Audio
  alsa_output.usb-headset  [default]
      Headset

  For example, in $XDG_CONFIG_HOME/conch/voice.toml:

    [audio]
    input = \"alsa_input.builtin\"
    output = \"alsa_output.usb-headset\"

  A microphone named there that is not plugged in is not replaced by the default:
  nothing is transmitted until it is. A speaker named there that is not plugged in
  is not replaced either; nothing is heard until it is.

";
        assert_eq!(text, want);
    }

    #[test]
    fn a_machine_with_no_devices_and_one_still_listing_says_so() {
        let text = audio_text(&NodeTable::default());
        assert!(text.contains("had not finished listing"), "{text}");
        assert!(text.contains("Microphones (`input`)\n  none\n"), "{text}");
        assert!(text.contains("Speakers (`output`)\n  none\n"), "{text}");
        assert!(
            !text.contains("[audio]\n"),
            "no example without a device: {text}"
        );
    }

    #[test]
    fn a_name_that_cannot_be_written_into_the_configuration_is_counted_not_shown() {
        let text = audio_text(&table(vec![
            node(1, "Audio/Source", "ok_mic", "Fine"),
            node(2, "Audio/Source", "bad\"quote", "x"),
            node(3, "Audio/Source", "bad\\slash", "x"),
            node(4, "Audio/Source", "bad\nline\u{202E}", "x"),
            node(5, "Audio/Sink", "", "no name"),
            RegistryEvent::Synced,
        ]));
        assert!(text.contains("ok_mic"), "{text}");
        assert!(
            text.contains("3 devices whose names have characters that are not shown"),
            "{text}"
        );
        assert!(
            text.contains("Speakers (`output`)\n  one device whose name has characters"),
            "{text}"
        );
        for hidden in ["bad", "quote", "slash", "\u{202E}"] {
            assert!(!text.contains(hidden), "{hidden:?} in {text}");
        }
    }

    #[test]
    fn a_description_is_one_line_and_cut() {
        let long = format!("Line one\nline two {}", "x".repeat(200));
        let text = audio_text(&table(vec![
            node(1, "Audio/Source", "mic", &long),
            RegistryEvent::Synced,
        ]));
        let shown = text
            .lines()
            .find(|line| line.starts_with("      Line one"))
            .unwrap();
        assert!(shown.starts_with("      Line one line two x"), "{shown}");
        assert!(
            shown.chars().count() < 6 + DESCRIPTION_CHARS + 10,
            "{shown}"
        );
        assert!(shown.ends_with("[cut]"), "{shown}");
    }

    #[test]
    fn when_pipewire_cannot_be_reached_the_audio_half_says_so_on_one_line() {
        let mut written = Vec::new();
        audio_unavailable("no such\nserver", &mut written).unwrap();
        assert_eq!(
            String::from_utf8(written).unwrap(),
            "Audio devices (PipeWire)\n  not listed: no such server\n\n"
        );
    }

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
