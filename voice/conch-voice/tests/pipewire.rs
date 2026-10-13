//! The microphone and the speakers against a real PipeWire session (issue #184). Every test
//! here is ignored: it needs PipeWire with a session manager, and most need `pw-loopback`
//! and `pw-link`. `make check` runs none of them.
//!
//! One command runs the ones that are safe on a desktop in use, one after the other:
//!
//! ```text
//! cargo test -p conch-voice --test pipewire -- --ignored --test-threads=1 live_
//! ```
//!
//! **Safe on a desktop in use** (names start `live_`): each makes its own virtual sink and
//! source with `pw-loopback`, names them in the client's configuration (never the defaults),
//! and before it writes a sample checks with `pw-link` that its stream is linked to its own
//! device and to nothing else. Nothing is heard, no real device is touched and the defaults
//! are not changed. Each refuses to start if a `dmn.playback` or `dmn.capture` stream
//! already exists, so that it cannot mistake another program's for its own.
//!
//! **Not safe** (names start `by_hand_`): they capture the default microphone, or change
//! the default output. Run them on a machine you are sitting at with nothing playing, and
//! one at a time by full name.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::time::{Duration, Instant};

use conch_voice::audio::PipeWire;
use conch_voice_audio::{FRAME_LEN, Frame, MicRead, MicSource, SAMPLE_RATE, SILENCE, SignalMeter};
use conch_voice_audio::{SpeakerSink, ToneSource};
use dmn_audio::{CaptureTarget, MicCapture, PipeWireContext, Playback, SinkTarget};

const TONE_HZ: f32 = 1000.0;

/// A virtual sink `NAME-sink` and a source `NAME` that carries whatever is played into the
/// sink, made by `pw-loopback`. Both go when this is dropped, a panic included.
struct Loopback(Child);

impl Loopback {
    fn new(name: &str) -> Self {
        let child = Command::new("pw-loopback")
            .args(["-n", name, "-m", "[ MONO ]"])
            .arg(format!(
                "--capture-props=media.class=Audio/Sink node.name={name}-sink"
            ))
            .arg(format!(
                "--playback-props=media.class=Audio/Source node.name={name}"
            ))
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("pw-loopback on PATH");
        Loopback(child)
    }
}

impl Drop for Loopback {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn wait_for(what: &str, within: Duration, mut met: impl FnMut() -> bool) {
    let start = Instant::now();
    while !met() {
        assert!(start.elapsed() < within, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn pw_link() -> String {
    let out = Command::new("pw-link").arg("-l").output().expect("pw-link");
    String::from_utf8_lossy(&out.stdout).into_owned()
}

/// What `pw-link -l` shows linked to or from any port of the node `node`.
fn peers_of(listing: &str, node: &str) -> Vec<String> {
    let mut peers = Vec::new();
    let mut ours = false;
    for line in listing.lines() {
        if !line.starts_with(' ') {
            ours = line.split_once(':').is_some_and(|(owner, _)| owner == node);
        } else if ours {
            let line = line.trim();
            if let Some(peer) = line
                .strip_prefix("|-> ")
                .or_else(|| line.strip_prefix("|<- "))
            {
                peers.push(peer.to_owned());
            }
        }
    }
    peers
}

/// Refuses to go on, before a sample is written, unless `node` is linked to the devices
/// named `allowed` and to nothing else.
fn assert_linked_only_to(node: &str, allowed: &[&str]) {
    let peers = peers_of(&pw_link(), node);
    assert!(
        !peers.is_empty()
            && peers
                .iter()
                .all(|peer| allowed.iter().any(|a| peer.starts_with(&format!("{a}:")))),
        "{node} is linked to {peers:?}, wanted only {allowed:?}"
    );
}

fn refuse_if_streams_exist() {
    let listing = pw_link();
    for node in ["dmn.playback", "dmn.capture"] {
        assert!(
            !listing
                .lines()
                .any(|line| line.split_once(':').is_some_and(|(owner, _)| owner == node)),
            "a {node} stream already exists: it is not this test's, so this test stops"
        );
    }
}

/// A steady tone, frame by frame.
struct Tone(ToneSource);

impl Tone {
    fn new(hz: f32) -> Self {
        Tone(ToneSource::new(hz, 0.3).unwrap())
    }

    fn frame(&mut self) -> Frame {
        let mut frame = SILENCE;
        self.0.read(&mut frame);
        frame
    }
}

/// Reads the microphone every 10 ms for `secs`, into a meter.
fn listen(mic: &mut impl MicSource, secs: f32, mut each_tick: impl FnMut()) -> SignalMeter {
    let mut meter = SignalMeter::new(&[500.0, TONE_HZ, 2000.0]).unwrap();
    let mut frame = SILENCE;
    let end = Instant::now() + Duration::from_secs_f32(secs);
    while Instant::now() < end {
        each_tick();
        while mic.read(&mut frame) == MicRead::Ready {
            meter.push(&frame);
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    meter
}

/// Waits until the first whole frame arrives.
fn wait_for_a_frame(mic: &mut impl MicSource) {
    let mut frame = SILENCE;
    wait_for("the microphone to deliver", Duration::from_secs(5), || {
        mic.read(&mut frame) == MicRead::Ready
    });
}

fn stream_gone(node: &str) -> bool {
    !pw_link()
        .lines()
        .any(|line| line.split_once(':').is_some_and(|(owner, _)| owner == node))
}

/// `devices` lists what PipeWire has and opens nothing: this connects, reads the table and
/// prints it. Safe anywhere PipeWire runs.
#[test]
#[ignore = "needs a running PipeWire session; reads its list of devices and opens none"]
fn live_the_device_list_is_read_and_names_the_defaults() {
    let pw = PipeWire::connect().expect("PipeWire running");
    let table = pw.nodes(conch_voice::audio::LISTING_WAIT);
    assert!(table.synced, "PipeWire did not finish listing");
    let mut text = Vec::new();
    conch_voice::devices::audio_report(&table, &mut text).unwrap();
    let text = String::from_utf8(text).unwrap();
    println!("{text}");
    assert!(text.contains("Microphones (`input`)"), "{text}");
    assert!(text.contains("Speakers (`output`)"), "{text}");
}

/// "A tone played into a virtual source is counted by a second client": a second client
/// (a `dmn-audio` playback on a connection of its own) plays a 1 kHz tone into the virtual
/// sink; the client's microphone, configured by the virtual source's name, hears it.
#[test]
#[ignore = "needs PipeWire with a session manager, pw-loopback and pw-link; uses only a virtual device pair it makes"]
fn live_a_tone_played_into_a_virtual_source_is_counted_by_the_microphone() {
    refuse_if_streams_exist();
    let name = format!("conch-voice-test-mic-{}", std::process::id());
    let sink = format!("{name}-sink");
    let pw = PipeWire::connect().expect("PipeWire running");
    let _loopback = Loopback::new(&name);
    wait_for("the virtual devices", Duration::from_secs(5), || {
        let table = pw.nodes(Duration::from_millis(100));
        table.sinks().any(|n| n.name == sink) && table.sources().any(|n| n.name == name)
    });

    // The client under test: the microphone, by name.
    let mut mic = pw.microphone(Some(&name)).unwrap();
    wait_for_a_frame(&mut mic);
    assert_linked_only_to("dmn.capture", &[&name]);

    // The second client plays into the virtual sink and nowhere else.
    let other = Arc::new(PipeWireContext::connect().expect("PipeWire running"));
    let mut play = Playback::new(other, SinkTarget::Named(sink.clone()), SAMPLE_RATE, 200).unwrap();
    let mut writer = play.start().unwrap();
    play.wait_streaming(Duration::from_secs(3)).unwrap();
    assert_linked_only_to("dmn.playback", &[&sink]);

    let mut tone = Tone::new(TONE_HZ);
    let meter = listen(&mut mic, 1.5, || {
        while writer.queued() < 4 * FRAME_LEN {
            writer.write(&tone.frame());
        }
    });
    println!(
        "heard {} frames, rms {:.3}, strongest of 500/1000/2000 Hz: {:?}",
        meter.frames(),
        meter.rms(),
        meter.dominant()
    );
    assert!(meter.frames() > 100, "{} frames", meter.frames());
    assert!(meter.rms() > 0.05, "rms {}", meter.rms());
    assert_eq!(meter.dominant(), Some(TONE_HZ));

    // The stream is open while the microphone is, and gone when it is dropped.
    drop(play);
    drop(mic);
    wait_for(
        "the capture stream to close",
        Duration::from_secs(3),
        || stream_gone("dmn.capture"),
    );
}

/// "A client's playback is captured from the sink monitor": the client's speakers, by the
/// virtual sink's name, play a tone; a second client records the virtual source, which
/// carries what the sink plays.
#[test]
#[ignore = "needs PipeWire with a session manager, pw-loopback and pw-link; uses only a virtual device pair it makes"]
fn live_a_clients_playback_is_captured_from_the_sink_monitor() {
    refuse_if_streams_exist();
    let name = format!("conch-voice-test-spk-{}", std::process::id());
    let sink = format!("{name}-sink");
    let pw = PipeWire::connect().expect("PipeWire running");
    let _loopback = Loopback::new(&name);
    wait_for("the virtual devices", Duration::from_secs(5), || {
        let table = pw.nodes(Duration::from_millis(100));
        table.sinks().any(|n| n.name == sink) && table.sources().any(|n| n.name == name)
    });

    // The second client records the virtual source.
    let other = Arc::new(PipeWireContext::connect().expect("PipeWire running"));
    let mut record = MicCapture::new(
        other,
        CaptureTarget::Mic(Some(name.clone())),
        SAMPLE_RATE,
        2_000,
    )
    .unwrap();
    let mut heard = record.start().unwrap();
    record.wait_streaming(Duration::from_secs(3)).unwrap();

    // The client under test: the speakers, by name.
    let mut speakers = pw.speakers(Some(&sink)).unwrap();
    wait_for("the speakers to link", Duration::from_secs(5), || {
        !peers_of(&pw_link(), "dmn.playback").is_empty()
    });
    assert_linked_only_to("dmn.playback", &[&sink]);

    let mut tone = Tone::new(TONE_HZ);
    let mut meter = SignalMeter::new(&[500.0, TONE_HZ, 2000.0]).unwrap();
    let mut block = [0.0_f32; FRAME_LEN];
    let end = Instant::now() + Duration::from_millis(1500);
    while Instant::now() < end {
        speakers.play(&tone.frame());
        while heard.queued() >= FRAME_LEN {
            heard.pop(&mut block);
            meter.push(&block);
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    println!(
        "captured {} frames, rms {:.3}, strongest of 500/1000/2000 Hz: {:?}",
        meter.frames(),
        meter.rms(),
        meter.dominant()
    );
    assert!(meter.rms() > 0.02, "rms {}", meter.rms());
    assert_eq!(meter.dominant(), Some(TONE_HZ));

    drop(speakers);
    wait_for(
        "the playback stream to close",
        Duration::from_secs(3),
        || stream_gone("dmn.playback"),
    );
    record.stop().unwrap();
}

/// Unplugging a named microphone while joined: the read says there is no microphone, nothing
/// from before the unplug comes out when it is back, and capture resumes on its own.
#[test]
#[ignore = "needs PipeWire with a session manager, pw-loopback and pw-link; uses only a virtual device pair it makes"]
fn live_a_named_microphone_that_is_unplugged_gives_none_and_resumes_when_it_is_back() {
    refuse_if_streams_exist();
    let name = format!("conch-voice-test-hot-{}", std::process::id());
    let pw = PipeWire::connect().expect("PipeWire running");
    let first = Loopback::new(&name);
    wait_for("the virtual source", Duration::from_secs(5), || {
        pw.nodes(Duration::from_millis(100))
            .sources()
            .any(|n| n.name == name)
    });
    let mut mic = pw.microphone(Some(&name)).unwrap();
    wait_for_a_frame(&mut mic);
    assert_linked_only_to("dmn.capture", &[&name]);

    drop(first);
    let mut frame = SILENCE;
    wait_for("no microphone", Duration::from_secs(5), || {
        mic.read(&mut frame) == MicRead::NoMicrophone
    });
    // It stays that way: a named microphone is not replaced by the default one.
    for _ in 0..30 {
        assert_eq!(mic.read(&mut frame), MicRead::NoMicrophone);
        std::thread::sleep(Duration::from_millis(10));
    }

    let _second = Loopback::new(&name);
    wait_for("the microphone back", Duration::from_secs(10), || {
        mic.read(&mut frame) == MicRead::Ready
    });
    assert_linked_only_to("dmn.capture", &[&name]);
    println!("the microphone went and came back, and captured again without a restart");
}

/// A named microphone that is absent at the start is stood in for by the default one in
/// `dmn-audio`; `conch-voice` does not transmit from it (the decision is in
/// `audio::availability`). This captures the DEFAULT MICROPHONE while it runs, to prove
/// that nothing of it is handed on.
#[test]
#[ignore = "BY HAND: records the default microphone (and throws it away); needs PipeWire with a session manager and a default source"]
fn by_hand_a_named_microphone_that_is_absent_is_not_stood_in_for_by_the_default() {
    let pw = PipeWire::connect().expect("PipeWire running");
    let mut mic = pw
        .microphone(Some("conch-voice-test-no-such-microphone"))
        .unwrap();
    let mut frame = SILENCE;
    // Give the default microphone every chance to deliver.
    let end = Instant::now() + Duration::from_secs(3);
    while Instant::now() < end {
        assert_eq!(
            mic.read(&mut frame),
            MicRead::NoMicrophone,
            "audio of the stand-in was handed on"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Changing the default output moves playback without a restart. It CHANGES THE DEFAULT
/// OUTPUT of this machine twice, to virtual sinks of its own, and puts it back; everything
/// else playing moves with it.
#[test]
#[ignore = "BY HAND: changes the default output (to its own virtual sinks, then back); needs PipeWire, pw-loopback, pw-link and pw-metadata"]
fn by_hand_playback_to_the_default_follows_it_when_the_default_changes() {
    refuse_if_streams_exist();
    let pid = std::process::id();
    let (a, b) = (
        format!("conch-voice-test-a-{pid}"),
        format!("conch-voice-test-b-{pid}"),
    );
    let (a_sink, b_sink) = (format!("{a}-sink"), format!("{b}-sink"));
    let pw = PipeWire::connect().expect("PipeWire running");
    let _first = Loopback::new(&a);
    let _second = Loopback::new(&b);
    wait_for("both virtual sinks", Duration::from_secs(5), || {
        let table = pw.nodes(Duration::from_millis(100));
        [&a_sink, &b_sink]
            .iter()
            .all(|s| table.sinks().any(|n| &n.name == *s))
            && [&a, &b]
                .iter()
                .all(|s| table.sources().any(|n| &n.name == *s))
    });

    /// Puts the default output back as it was found, on every exit path.
    struct Restore(Option<String>);
    impl Drop for Restore {
        fn drop(&mut self) {
            let mut command = Command::new("pw-metadata");
            match &self.0 {
                Some(name) => command.args([
                    "0",
                    "default.audio.sink",
                    &format!("{{ \"name\": \"{name}\" }}"),
                    "Spa:String:JSON",
                ]),
                None => command.args(["-d", "0", "default.audio.sink"]),
            };
            let _ = command.output();
        }
    }
    let _restore = Restore(pw.nodes(Duration::from_millis(100)).default_sink);
    let set_default = |name: &str| {
        Command::new("pw-metadata")
            .args([
                "0",
                "default.audio.sink",
                &format!("{{ \"name\": \"{name}\" }}"),
                "Spa:String:JSON",
            ])
            .output()
            .expect("pw-metadata");
    };

    let other = Arc::new(PipeWireContext::connect().expect("PipeWire running"));
    let record = |name: &str| {
        let mut capture = MicCapture::new(
            Arc::clone(&other),
            CaptureTarget::Mic(Some(name.to_owned())),
            SAMPLE_RATE,
            2_000,
        )
        .unwrap();
        let reader = capture.start().unwrap();
        capture.wait_streaming(Duration::from_secs(3)).unwrap();
        (capture, reader)
    };
    let (_cap_a, mut heard_a) = record(&a);
    let (_cap_b, mut heard_b) = record(&b);

    // The default is the first virtual sink before the stream exists, so the stream is
    // never linked to a real device.
    set_default(&a_sink);
    wait_for(
        "the default to be the first sink",
        Duration::from_secs(3),
        || pw.nodes(Duration::from_millis(100)).default_sink.as_deref() == Some(a_sink.as_str()),
    );
    let mut speakers = pw.speakers(None).unwrap();
    wait_for("the speakers to link", Duration::from_secs(5), || {
        !peers_of(&pw_link(), "dmn.playback").is_empty()
    });
    assert_linked_only_to("dmn.playback", &[&a_sink]);

    let mut tone = Tone::new(TONE_HZ);
    let mut block = [0.0_f32; FRAME_LEN];
    let mut play_into = |heard: &mut dmn_audio::MicReader| {
        let mut meter = SignalMeter::new(&[500.0, TONE_HZ, 2000.0]).unwrap();
        let end = Instant::now() + Duration::from_millis(1500);
        while Instant::now() < end {
            speakers.play(&tone.frame());
            while heard.queued() >= FRAME_LEN {
                heard.pop(&mut block);
                meter.push(&block);
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        meter
    };
    let meter = play_into(&mut heard_a);
    assert_eq!(meter.dominant(), Some(TONE_HZ), "on the first default");

    set_default(&b_sink);
    wait_for(
        "the default to be the second sink",
        Duration::from_secs(3),
        || pw.nodes(Duration::from_millis(100)).default_sink.as_deref() == Some(b_sink.as_str()),
    );
    wait_for("the stream to move", Duration::from_secs(5), || {
        let peers = peers_of(&pw_link(), "dmn.playback");
        !peers.is_empty() && peers.iter().all(|p| p.starts_with(&format!("{b_sink}:")))
    });
    let _ = heard_b.pop(&mut [0.0; 48_000]);
    let meter = play_into(&mut heard_b);
    assert_eq!(
        meter.dominant(),
        Some(TONE_HZ),
        "after the default moved, without a restart"
    );
}
