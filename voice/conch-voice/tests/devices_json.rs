//! `conch-voice devices --json` (issue #650): what it touches, and what it writes when
//! PipeWire or `voice.toml` is not as it should be. No test here needs PipeWire, a
//! microphone or a keyboard: the node table is built from registry events, and the
//! keyboards' half is a closure that fails the test if it is called.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::cell::Cell;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use conch_voice::cli::Environment;
use conch_voice::devices_with;
use conch_voice::secrets::Scrubber;
use conch_voice_api::{VoiceDevicesConfig, VoiceDevicesPipeWire, VoiceDevicesV1};
use dmn_audio::{NodeTable, RegistryEvent};

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

fn table() -> NodeTable {
    let mut table = NodeTable::default();
    for event in [
        node(1, "Audio/Source", "alsa_input.builtin", "Built-in Audio"),
        node(2, "Audio/Source", "speech_out", ""),
        node(3, "Audio/Sink", "alsa_output.builtin", "Built-in Audio"),
        RegistryEvent::DefaultSource(Some("alsa_input.builtin".to_owned())),
        RegistryEvent::DefaultSink(Some("alsa_output.builtin".to_owned())),
        RegistryEvent::Synced,
    ] {
        table.apply(event);
    }
    table
}

fn environment(dir: &Path) -> Environment {
    Environment {
        conch_config_dir: Some(dir.as_os_str().to_owned()),
        ..Environment::default()
    }
}

struct Run {
    out: String,
    err: String,
    result: Result<(), conch_voice::Error>,
    keyboards_asked: bool,
}

fn run(json: bool, environment: &Environment, connect: Result<NodeTable, String>) -> Run {
    let asked = Cell::new(false);
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let result = devices_with(
        json,
        environment,
        &Scrubber::new(),
        || connect,
        |keyboards| {
            asked.set(true);
            std::io::Write::write_all(keyboards, b"KEYBOARDS\n")
        },
        &mut out,
        &mut err,
    );
    Run {
        out: String::from_utf8(out).unwrap(),
        err: String::from_utf8(err).unwrap(),
        result,
        keyboards_asked: asked.get(),
    }
}

fn parse(out: &str) -> VoiceDevicesV1 {
    assert_eq!(out.matches('\n').count(), 1, "{out:?}");
    assert!(out.ends_with('\n'));
    let document: VoiceDevicesV1 = serde_json::from_str(out).unwrap();
    document.validate().unwrap();
    document
}

#[test]
fn json_never_asks_for_the_keyboards() {
    let private = tempfile::tempdir().unwrap();
    let run = run(true, &environment(private.path()), Ok(table()));
    run.result.unwrap();
    assert!(!run.keyboards_asked);
    assert!(!run.out.contains("KEYBOARDS"));
    assert_eq!(run.err, "");
    let document = parse(&run.out);
    assert_eq!(document.pipewire, VoiceDevicesPipeWire::Ok);
    assert_eq!(document.config, VoiceDevicesConfig::Missing);
    let names: Vec<_> = document
        .microphones
        .iter()
        .map(|d| d.name.as_str())
        .collect();
    assert_eq!(names, ["alsa_input.builtin", "speech_out"]);
    assert!(document.microphones[0].default);
    assert_eq!(document.microphones[1].description, "");
}

#[test]
fn without_json_the_text_is_written_and_the_keyboards_are_asked_for() {
    let private = tempfile::tempdir().unwrap();
    let run = run(false, &environment(private.path()), Ok(table()));
    run.result.unwrap();
    assert!(run.keyboards_asked);
    assert!(
        run.out.starts_with("Audio devices (PipeWire)\n"),
        "{}",
        run.out
    );
    assert!(run.out.ends_with("KEYBOARDS\n"));
    // And a PipeWire that cannot be reached is a line, with the keyboards all the same.
    let run = run_text_without_pipewire(private.path());
    assert!(run.keyboards_asked);
    assert!(
        run.out.contains("  not listed: no pipewire here\n"),
        "{}",
        run.out
    );
}

fn run_text_without_pipewire(dir: &Path) -> Run {
    run(false, &environment(dir), Err("no pipewire here".to_owned()))
}

#[test]
fn json_without_pipewire_is_the_unavailable_document_and_still_a_success() {
    let private = tempfile::tempdir().unwrap();
    let run = run(
        true,
        &environment(private.path()),
        Err("the reason, which is not in the document".to_owned()),
    );
    run.result.unwrap();
    assert!(!run.keyboards_asked);
    assert_eq!(run.err, "");
    assert!(!run.out.contains("reason"));
    let document = parse(&run.out);
    assert_eq!(document.pipewire, VoiceDevicesPipeWire::Unavailable);
    assert!(document.microphones.is_empty() && document.speakers.is_empty());
    assert_eq!(document.config, VoiceDevicesConfig::Missing);
}

#[test]
fn a_refused_file_is_named_on_standard_error_and_is_not_a_failure() {
    let private = tempfile::tempdir().unwrap();
    let file = private.path().join("voice.toml");
    std::fs::write(&file, "[audio]\ninput = \"speech_out\"\n").unwrap();
    std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o666)).unwrap();
    let run = run(true, &environment(private.path()), Ok(table()));
    run.result.unwrap();
    let document = parse(&run.out);
    assert_eq!(document.config, VoiceDevicesConfig::Refused);
    assert_eq!(document.configured_input, None);
    // The document holds no path; the line says which file, once, on one line.
    assert!(!run.out.contains(private.path().to_str().unwrap()));
    assert!(run.err.starts_with("conch-voice: "), "{}", run.err);
    assert_eq!(run.err.matches('\n').count(), 1, "{}", run.err);
}

#[test]
fn a_file_that_is_read_echoes_the_devices_it_names() {
    let private = tempfile::tempdir().unwrap();
    let file = private.path().join("voice.toml");
    std::fs::write(
        &file,
        "[audio]\ninput = \"speech_out\"\noutput = \"default\"\n",
    )
    .unwrap();
    std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o600)).unwrap();
    let run = run(true, &environment(private.path()), Ok(table()));
    run.result.unwrap();
    assert_eq!(run.err, "");
    let document = parse(&run.out);
    assert_eq!(document.config, VoiceDevicesConfig::Ok);
    assert_eq!(document.configured_input.as_deref(), Some("speech_out"));
    assert_eq!(document.configured_output, None);
}

#[test]
fn with_no_configuration_directory_the_document_says_none() {
    let run = run(true, &Environment::default(), Ok(table()));
    run.result.unwrap();
    assert_eq!(parse(&run.out).config, VoiceDevicesConfig::None);
}

/// A standard output that cannot be written to is the one failure.
#[test]
fn a_standard_output_that_cannot_be_written_is_an_output_error() {
    struct Broken;
    impl std::io::Write for Broken {
        fn write(&mut self, _: &[u8]) -> std::io::Result<usize> {
            Err(std::io::Error::other("closed"))
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    let result = devices_with(
        true,
        &Environment::default(),
        &Scrubber::new(),
        || Ok(table()),
        |_| panic!("--json must not ask for the keyboards"),
        &mut Broken,
        &mut Vec::new(),
    );
    let error = result.unwrap_err();
    assert!(matches!(error, conch_voice::Error::Output(_)));
    assert_eq!(error.exit_code(), 1);
}
