//! The sound of a screen share (#1586, `docs/design/linux-share-audio.md` §5, §7, §10,
//! §11, §14.2), in `conch.voice_control.v3`, run in-process between a fake SDK and a stub
//! `conchd`: each v3 transcript in `pkg/schema/testdata/voice-control-v3/` driven by its owner
//! side against the real helper, what the helper writes checked against the protocol's rules
//! with every line read by v3's reader (Go's `Validate`), and each rule of the share's
//! lifecycle on its own.
//!
//! The captures are a scripted graph: applications that play tones, among them the helper's
//! own playback, a renderer of the Conch tree and the owner's own application name, which
//! the rule must leave out. The process tree is a fake one.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit, but the one marked
//! `#[ignore]`, which says what it needs.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use conch_voice::sdk::SdkEvent;
use conch_voice::share_capture::{
    Capture, CaptureSpec, Captures, Graph, Health, Look, OpenFailed, Registry, ShareSource, Taken,
    ToneCapture,
};
use conch_voice::share_eligibility::{ProcessTree, StreamFacts};
use conch_voice_api::{
    VOICE_CONTROL_CAPTURE_STOP_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VoiceControlAckV3,
    VoiceControlErrorCode, VoiceControlEventV3, VoiceControlStateV3, parse_command,
    parse_command_v2, parse_command_v3, parse_event, parse_event_v2, parse_event_v3,
};
use conch_voice_audio::StereoFrame;
use serde_json::{Value, json};

use support::fake::Call;
use support::managed::{ManagedRig, ManagedSetup};
use support::stub::Stub;

/// The companion `conchd` names for the desktop's sessions.
const COMPANION: &str = "p7.screen";
/// The application name the owner's commands carry.
const NAME: &str = "Conch";

/// The fake process tree: this helper, its parent (the desktop's main process) and a
/// renderer under it.
const OWN: u32 = 4100;
const PARENT: u32 = 4000;
const RENDERER: u32 = 4001;

/// The streams the rule must leave out, whatever else is playing.
const OWN_PLAYBACK: u64 = 1;
const RENDERER_STREAM: u64 = 2;
const NAMED_STREAM: u64 = 3;
const UNKNOWN_CLIENT: u64 = 4;

/// `pkg/schema/testdata/voice-control-v3`, read in place.
fn fixtures_v3() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../pkg/schema/testdata/voice-control-v3")
}

fn fixture_v3(name: &str) -> Value {
    let path = fixtures_v3().join(name);
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap()
}

// ------------------------------------------------------------------- the scripted graph

/// The applications playing, as the test arranges them, and what was captured of them.
#[derive(Clone, Default)]
struct ShareWorld(Arc<WorldState>);

#[derive(Default)]
struct WorldState {
    /// The registry's generation and its streams.
    streams: Mutex<(u64, Vec<StreamFacts>)>,
    /// Captures open now.
    open: Arc<AtomicUsize>,
    /// Every serial a capture was opened for, in order.
    opened: Mutex<Vec<u64>>,
    /// Every spec a capture was asked for.
    specs: Mutex<Vec<CaptureSpec>>,
    /// While set, the process tree cannot be read.
    no_tree: AtomicBool,
    /// Frames taken from the captures, all together: the mix running.
    taken: Arc<AtomicU64>,
}

impl ShareWorld {
    /// `others` applications of other people playing, and always the three that must be
    /// left out and one whose owning client is not known yet.
    fn play(&self, others: usize) {
        let mut streams = vec![
            StreamFacts {
                serial: Some(OWN_PLAYBACK),
                sec_pid: Some(OWN),
                application_process_id: Some(OWN),
                application_name: Some("conch-voice".to_owned()),
                media_name: None,
            },
            StreamFacts {
                serial: Some(RENDERER_STREAM),
                sec_pid: Some(900),
                application_process_id: Some(RENDERER),
                application_name: Some("Chromium".to_owned()),
                media_name: None,
            },
            StreamFacts {
                serial: Some(NAMED_STREAM),
                sec_pid: Some(900),
                application_process_id: Some(9999),
                application_name: Some(NAME.to_owned()),
                media_name: None,
            },
            StreamFacts {
                serial: Some(UNKNOWN_CLIENT),
                sec_pid: None,
                application_process_id: None,
                application_name: None,
                media_name: None,
            },
        ];
        for at in 0..others {
            let at = u32::try_from(at).unwrap();
            streams.push(StreamFacts {
                serial: Some(100 + u64::from(at)),
                sec_pid: Some(7000 + at),
                application_process_id: Some(7000 + at),
                application_name: Some(format!("player-{at}")),
                media_name: None,
            });
        }
        let mut state = self.0.streams.lock().unwrap();
        state.0 += 1;
        state.1 = streams;
    }

    /// Frames the captures have given the mix so far.
    fn mixed_frames(&self) -> u64 {
        self.0.taken.load(Ordering::SeqCst)
    }

    fn open(&self) -> usize {
        self.0.open.load(Ordering::SeqCst)
    }

    fn opened(&self) -> Vec<u64> {
        self.0.opened.lock().unwrap().clone()
    }

    fn source(&self) -> ShareSource {
        let graph = self.clone();
        let tree = self.clone();
        ShareSource::Captures(Captures {
            graph: Arc::new(move |_| Some(Box::new(WorldGraph(graph.clone())) as Box<dyn Graph>)),
            tree: Arc::new(move || {
                if tree.0.no_tree.load(Ordering::SeqCst) {
                    Err(conch_voice::share_eligibility::NoTree::Unreadable)
                } else {
                    ProcessTree::build(
                        Some(OWN),
                        PARENT,
                        &[(OWN, PARENT), (RENDERER, PARENT), (PARENT, 1)],
                    )
                }
            }),
            own_pid: Some(OWN),
        })
    }

    /// Never a capture of what must be left out, and every spec one stream's at 48 kHz in
    /// two channels.
    fn assert_never_captured_the_call(&self) {
        for serial in self.opened() {
            assert!(
                ![OWN_PLAYBACK, RENDERER_STREAM, NAMED_STREAM, UNKNOWN_CLIENT].contains(&serial),
                "stream {serial} was captured: {:?}",
                self.opened()
            );
        }
        for spec in self.0.specs.lock().unwrap().iter() {
            assert_eq!(*spec, CaptureSpec::of_stream(spec.serial));
            assert_eq!((spec.rate, spec.channels), (48_000, 2));
        }
    }
}

struct WorldGraph(ShareWorld);

impl Graph for WorldGraph {
    fn look(&mut self, since: Option<u64>) -> Look {
        let state = self.0.0.streams.lock().unwrap();
        if since == Some(state.0) {
            return Look::Unchanged;
        }
        Look::Changed(Registry {
            generation: state.0,
            streams: state.1.clone(),
        })
    }

    fn open_capture(&mut self, spec: CaptureSpec) -> Result<Box<dyn Capture>, OpenFailed> {
        self.0.0.specs.lock().unwrap().push(spec);
        self.0.0.opened.lock().unwrap().push(spec.serial);
        self.0.0.open.fetch_add(1, Ordering::SeqCst);
        Ok(Box::new(WorldCapture {
            tone: ToneCapture::new(200.0 + spec.serial as f32, 0.1),
            open: Arc::clone(&self.0.0.open),
            taken: Arc::clone(&self.0.0.taken),
        }))
    }
}

/// A tone that counts itself closed.
struct WorldCapture {
    tone: ToneCapture,
    open: Arc<AtomicUsize>,
    taken: Arc<AtomicU64>,
}

impl Capture for WorldCapture {
    fn health(&self) -> Health {
        self.tone.health()
    }
    fn take(&mut self, frame: &mut StereoFrame) -> Taken {
        self.taken.fetch_add(1, Ordering::SeqCst);
        self.tone.take(frame)
    }
    fn stop(self: Box<Self>) {
        self.open.fetch_sub(1, Ordering::SeqCst);
    }
}

// ------------------------------------------------------------------------- the rig

/// A rig whose session names a companion (or not), capturing from `world`.
async fn share_rig(world: &ShareWorld, companion: bool) -> ManagedRig {
    share_rig_with(world, companion, ManagedSetup::default(), |_| {}).await
}

async fn share_rig_with(
    world: &ShareWorld,
    companion: bool,
    setup: ManagedSetup,
    arrange: impl FnOnce(&support::fake::FakeSdk),
) -> ManagedRig {
    let conchd = Stub::start().await;
    if companion {
        conchd.companion_identity(Some(COMPANION));
    }
    let setup = ManagedSetup {
        share: Some(world.source()),
        ..setup
    };
    ManagedRig::start_with(conchd, setup, arrange).await
}

fn v3(kind: &str, id: i64, fields: Value) -> Value {
    let mut command = ManagedRig::command(kind, id, fields);
    command["schema"] = json!("conch.voice_control.v3");
    command
}

fn send_start_v3(rig: &mut ManagedRig) {
    let mut start = rig.start_command();
    start["schema"] = json!("conch.voice_control.v3");
    rig.send(&start);
}

fn share_on(id: i64) -> Value {
    v3(
        "set_share_audio",
        id,
        json!({"capturing": true, "scope": "system_excluding_call", "application_name": NAME}),
    )
}

fn share_off(id: i64) -> Value {
    v3("set_share_audio", id, json!({"capturing": false}))
}

/// Every event of a v3 session, read by the owner's rules: the hello by v1's reader and
/// every line after it by v3's, which is Go's `ParseVoiceControlEventV3` and its `Validate`.
/// A line either would refuse fails the test: every state the helper writes holds together.
fn events_v3(rig: &ManagedRig) -> Vec<VoiceControlEventV3> {
    rig.lines()
        .iter()
        .enumerate()
        .skip(1)
        .map(|(_, line)| {
            parse_event_v3(line.as_bytes())
                .unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}"))
        })
        .collect()
}

fn states_v3(rig: &ManagedRig) -> Vec<VoiceControlStateV3> {
    events_v3(rig)
        .into_iter()
        .filter_map(|event| match event {
            VoiceControlEventV3::State(state) => Some(state),
            _ => None,
        })
        .collect()
}

fn state_v3(rig: &ManagedRig) -> Option<VoiceControlStateV3> {
    states_v3(rig).pop()
}

async fn until_v3(rig: &ManagedRig, what: &str, met: impl Fn(&VoiceControlStateV3) -> bool) {
    rig.until(what, |rig| state_v3(rig).is_some_and(|state| met(&state)))
        .await;
}

fn ack_of(rig: &ManagedRig, id: i64) -> Option<VoiceControlAckV3> {
    events_v3(rig).into_iter().find_map(|event| match event {
        VoiceControlEventV3::Ack(ack) if ack.id == id => Some(ack),
        _ => None,
    })
}

async fn ack_v3(rig: &ManagedRig, id: i64) -> VoiceControlAckV3 {
    rig.until(&format!("the ack of {id}"), |rig| ack_of(rig, id).is_some())
        .await;
    ack_of(rig, id).unwrap()
}

/// The position of the ack of `id` among the events, and of the last state before it.
fn ack_and_state_before(rig: &ManagedRig, id: i64) -> (usize, VoiceControlStateV3) {
    let events = events_v3(rig);
    let at = events
        .iter()
        .position(|event| matches!(event, VoiceControlEventV3::Ack(ack) if ack.id == id))
        .unwrap();
    let state = events[..at]
        .iter()
        .rev()
        .find_map(|event| match event {
            VoiceControlEventV3::State(state) => Some(state.clone()),
            _ => None,
        })
        .unwrap();
    (at, state)
}

async fn connected_v3(rig: &ManagedRig) {
    until_v3(rig, "connected", |state| {
        state.phase.as_str() == "connected" && state.capture.as_str() == "on"
    })
    .await;
}

fn share_is(state: &VoiceControlStateV3, audio: &str, problem: Option<&str>, sources: i64) -> bool {
    state.share_audio.as_str() == audio
        && state.share_audio_problem.map(|p| p.as_str()) == problem
        && state.share_audio_sources == sources
}

async fn wait_for(what: &str, met: impl Fn() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !met() {
        assert!(Instant::now() < deadline, "never: {what}");
        tokio::time::sleep(Duration::from_millis(2)).await;
    }
}

// ------------------------------------------------------------- the v3 transcripts

/// One step of a session, in the order the owner saw it.
#[derive(Debug, Clone)]
enum Step {
    Owner(String),
    Close,
    Helper(Value),
}

/// What a transcript's helper step needs done to the applications first.
#[derive(Debug, Clone, Copy)]
enum Act {
    /// This many applications of others are playing now.
    Play(usize),
}

/// The version a session speaks, by the schema of its start.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Version {
    V1,
    V2,
    V3,
}

fn version_of(schema: &str) -> Version {
    match schema {
        "conch.voice_control.v3" => Version::V3,
        "conch.voice_control.v2" => Version::V2,
        _ => Version::V1,
    }
}

/// Every line the helper wrote, read by the reader of the owner's version (the hello by
/// v1's), as JSON.
fn helper_values(rig: &ManagedRig, version: Option<Version>) -> Vec<Value> {
    rig.lines()
        .iter()
        .enumerate()
        .map(|(at, line)| {
            let read = match (at, version) {
                (0, _) | (_, None | Some(Version::V1)) => parse_event(line.as_bytes()).map(|_| ()),
                (_, Some(Version::V2)) => parse_event_v2(line.as_bytes()).map(|_| ()),
                (_, Some(Version::V3)) => parse_event_v3(line.as_bytes()).map(|_| ()),
            };
            read.unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}"));
            serde_json::from_str(line).unwrap()
        })
        .collect()
}

/// Whether the helper's event matches the shared contract fields in the transcript.
fn matches_step(event: &Value, want: &Value) -> bool {
    let strip = |value: &Value| {
        let mut value = value.clone();
        if let Value::Object(fields) = &mut value {
            fields.remove("seq");
            // This desktop diagnostic has its own fake-graph test.
            fields.remove("share_audio_streams");
            if fields.get("kind") == Some(&json!("hello")) {
                // This binary's version is its own.
                fields.remove("helper_version");
            }
        }
        value
    };
    strip(event) == strip(want)
}

/// What a state the transcript does not show must repeat of the last one matched: the
/// helper may write a state more (a capture or a key device settling) but never a change of
/// phase, of transmission, or of the share's sound the transcript does not show.
fn state_key(event: &Value) -> Option<Value> {
    (event["kind"] == "state").then(|| {
        json!([
            event["phase"],
            event.get("reason"),
            event["transmitting"],
            event.get("share_audio"),
            event.get("share_audio_muted"),
            event.get("share_audio_problem"),
            event.get("share_audio_sources"),
        ])
    })
}

/// Plays a transcript's owner side against `rig`, doing to the world before each helper
/// step what `acts` says for its seq, and waits for each helper step in what the helper
/// writes. Returns the log and the exit code.
async fn drive_v3(
    rig: &mut ManagedRig,
    world: &ShareWorld,
    name: &str,
    acts: &[(i64, Act)],
) -> (Vec<Step>, i32) {
    let doc = fixture_v3(name);
    let steps = doc["steps"].as_array().unwrap().clone();
    let mut log = Vec::new();
    let mut version = None;
    let mut cursor = 0;
    let mut last_state = None;
    for step in &steps {
        if step["from"] == "owner" {
            if step["close"] == "stdin" {
                rig.close();
                log.push(Step::Close);
                continue;
            }
            let mut message = step["message"].clone();
            if message["kind"] == "start" {
                message["server"] = json!(rig.conchd.url());
                // F13: Windows owners pass the login in the environment.
                #[cfg(windows)]
                {
                    message["credential"] = json!("env");
                }
                if version.is_none() {
                    version = Some(version_of(message["schema"].as_str().unwrap_or_default()));
                }
            }
            let line = message.to_string();
            rig.line(line.as_bytes());
            log.push(Step::Owner(line));
            continue;
        }
        if let Some(code) = step["exit"].as_i64() {
            let exit = rig.exit_code().await;
            let events = helper_values(rig, version);
            for event in &events[cursor..] {
                assert!(
                    state_key(event).is_some() && state_key(event) == last_state,
                    "{name}: written after the last step: {event}"
                );
                log.push(Step::Helper(event.clone()));
            }
            assert_eq!(i64::from(exit), code, "{name}: exit\n{}", rig.events.text());
            return (log, exit);
        }
        let want = &step["message"];
        if let Some(seq) = want["seq"].as_i64() {
            for (at, what) in acts {
                if *at == seq {
                    match what {
                        Act::Play(others) => world.play(*others),
                    }
                }
            }
        }
        let deadline = Instant::now() + Duration::from_secs(20);
        let (at, events) = loop {
            let events = helper_values(rig, version);
            let mut found = None;
            for (k, event) in events.iter().enumerate().skip(cursor) {
                if matches_step(event, want) {
                    found = Some(k);
                    break;
                }
                assert!(
                    state_key(event).is_some() && state_key(event) == last_state,
                    "{name}: wanted {want}, the helper wrote {event}\n{}",
                    rig.events.text()
                );
            }
            if let Some(k) = found {
                break (k, events);
            }
            assert!(
                Instant::now() < deadline,
                "{name}: timed out waiting for {want}\n{}",
                rig.events.text()
            );
            tokio::time::sleep(Duration::from_millis(2)).await;
        };
        for event in &events[cursor..=at] {
            log.push(Step::Helper(event.clone()));
        }
        if let Some(key) = state_key(&events[at]) {
            last_state = Some(key);
        }
        cursor = at + 1;
    }
    panic!("{name} has no exit step");
}

/// One of the owner's lines, as the helper reads it: with the reader of the session's
/// version, or for the first line, the one the line names.
struct Read {
    kind: Option<String>,
    id: Option<i64>,
    refusal: Option<VoiceControlErrorCode>,
}

fn read_owner_line(line: &str, version: Option<Version>) -> Read {
    macro_rules! read_with {
        ($parse:expr) => {
            match $parse(line.as_bytes()) {
                Ok(command) => Read {
                    kind: Some(command.kind().as_str().to_owned()),
                    id: Some(command.id()),
                    refusal: None,
                },
                Err(refusal) => Read {
                    kind: None,
                    id: refusal.id,
                    refusal: Some(refusal.code),
                },
            }
        };
    }
    let unknown = |read: &Read| read.refusal == Some(VoiceControlErrorCode::UnknownSchema);
    match version {
        Some(Version::V1) => read_with!(parse_command),
        Some(Version::V2) => read_with!(parse_command_v2),
        Some(Version::V3) => read_with!(parse_command_v3),
        None => {
            let v3 = read_with!(parse_command_v3);
            if !unknown(&v3) {
                return v3;
            }
            let v2 = read_with!(parse_command_v2);
            if !unknown(&v2) {
                return v2;
            }
            read_with!(parse_command)
        }
    }
}

/// A command with a usable id that has not been answered.
struct Pending {
    id: i64,
    kind: Option<String>,
    refusal: Option<VoiceControlErrorCode>,
    line: Value,
}

/// The rules of the protocol, checked against what really happened, for a session of any
/// version: the hello offers v1, v2 and v3; the start fixes the version; seq only rises;
/// every command with a usable id is answered once, in the order read, after the state its
/// effect changed; a share-audio command's state says what came of it; a session that is
/// not v3 never writes a share field; leaving refuses with `stopping`.
fn check_rules_v3(log: &[Step], exit: i32) {
    let mut seq = 0;
    let mut last_id = 0;
    let mut version: Option<Version> = None;
    let mut hello = false;
    let (mut started, mut ended) = (false, false);
    let mut final_phase: Option<String> = None;
    let mut state: Option<Value> = None;
    let mut previous_was_state = false;
    let mut pending: VecDeque<Pending> = VecDeque::new();
    let mut want_protocol_error = None;
    let mut want_failed_after = false;
    let shown = || format!("{log:#?}");
    for (i, step) in log.iter().enumerate() {
        match step {
            Step::Close => {}
            Step::Owner(line) => {
                assert!(hello, "step {i}: the owner wrote before the hello");
                let read = read_owner_line(line, version);
                let value: Value = serde_json::from_str(line).unwrap_or(Value::Null);
                let is_start = read.kind.as_deref() == Some("start");
                if !started && !is_start {
                    want_protocol_error = Some(match read.refusal {
                        Some(code) if code.is_fatal() => code,
                        _ => VoiceControlErrorCode::NotStarted,
                    });
                    continue;
                }
                let Some(id) = read.id else {
                    want_protocol_error = read.refusal;
                    continue;
                };
                if id <= last_id {
                    want_protocol_error = Some(VoiceControlErrorCode::IdNotIncreasing);
                    continue;
                }
                last_id = id;
                let stopping = state.as_ref().is_some_and(|s| s["phase"] == "stopping");
                let refusal = match (read.refusal, read.kind.as_deref()) {
                    // With a usable id, a refusal is an ack, after which the helper goes on.
                    (Some(code), _) => Some(code),
                    (None, Some("start")) if started => Some(VoiceControlErrorCode::AlreadyStarted),
                    (None, Some("start")) => {
                        started = true;
                        version = Some(version_of(value["schema"].as_str().unwrap_or_default()));
                        None
                    }
                    (
                        None,
                        Some(
                            "set_mute"
                            | "set_deafen"
                            | "set_devices"
                            | "set_talk_mode"
                            | "set_share_audio"
                            | "set_share_audio_mute",
                        ),
                    ) if stopping => Some(VoiceControlErrorCode::Stopping),
                    _ => None,
                };
                pending.push_back(Pending {
                    id,
                    kind: read.kind,
                    refusal,
                    line: value,
                });
            }
            Step::Helper(event) => {
                assert!(
                    !ended,
                    "step {i}: written after the final state\n{}",
                    shown()
                );
                let this = event["seq"].as_i64().unwrap();
                assert!(this > seq, "step {i}: seq {this} after {seq}");
                seq = this;
                if !hello {
                    assert_eq!(event["kind"], "hello", "step {i}: the first event");
                    let offers: Vec<&str> = event["versions"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .map(|v| v.as_str().unwrap())
                        .collect();
                    assert_eq!(
                        offers,
                        [
                            "conch.voice_control.v1",
                            "conch.voice_control.v2",
                            "conch.voice_control.v3"
                        ],
                        "step {i}: the hello offers every version"
                    );
                    hello = true;
                    previous_was_state = false;
                    continue;
                }
                // A session that is not v3 never says anything of a share's sound.
                if version != Some(Version::V3) {
                    let text = event.to_string();
                    assert!(!text.contains("share_audio"), "step {i}: {text}");
                    assert_ne!(event["schema"], "conch.voice_control.v3", "step {i}");
                }
                if want_failed_after {
                    assert!(
                        event["kind"] == "state"
                            && event["phase"] == "failed"
                            && event["reason"] == "protocol_error",
                        "step {i}: a protocol_error not followed by failed\n{}",
                        shown()
                    );
                }
                let was_state = previous_was_state;
                previous_was_state = event["kind"] == "state";
                match event["kind"].as_str().unwrap() {
                    "hello" => panic!("step {i}: a second hello"),
                    "protocol_error" => {
                        assert_eq!(
                            want_protocol_error.map(VoiceControlErrorCode::as_str),
                            event["code"].as_str(),
                            "step {i}: protocol_error\n{}",
                            shown()
                        );
                        assert!(pending.is_empty(), "step {i}: unanswered before it");
                        want_protocol_error = None;
                        want_failed_after = true;
                    }
                    "state" => {
                        assert_eq!(
                            want_protocol_error, None,
                            "step {i}: a protocol_error was due"
                        );
                        assert!(
                            started || want_failed_after,
                            "step {i}: a state before a start"
                        );
                        want_failed_after = false;
                        if event["phase"] == "stopped" || event["phase"] == "failed" {
                            ended = true;
                            final_phase = event["phase"].as_str().map(str::to_owned);
                        }
                        state = Some(event.clone());
                    }
                    "ack" => {
                        let next = pending.pop_front().unwrap_or_else(|| {
                            panic!("step {i}: an ack for {} nothing asked for", event["id"])
                        });
                        assert_eq!(
                            event["id"],
                            next.id,
                            "step {i}: acks out of order\n{}",
                            shown()
                        );
                        if let Some(refusal) = next.refusal {
                            assert!(
                                event["ok"] == false && event["error"] == refusal.as_str(),
                                "step {i}: {event}"
                            );
                            continue;
                        }
                        assert!(
                            event["ok"] == true,
                            "step {i}: {event} refused for no reason"
                        );
                        let latest = state
                            .as_ref()
                            .unwrap_or_else(|| panic!("step {i}: no state"));
                        let line = &next.line;
                        match next.kind.as_deref() {
                            Some("set_mute") => {
                                assert_eq!(latest["muted"], line["muted"], "step {i}")
                            }
                            Some("set_deafen") => {
                                assert_eq!(latest["deafened"], line["deafened"], "step {i}");
                            }
                            Some("leave") => assert!(
                                ["stopping", "stopped", "failed"]
                                    .contains(&latest["phase"].as_str().unwrap()),
                                "step {i}: leave acknowledged in {}",
                                latest["phase"]
                            ),
                            Some("get_state") => assert!(was_state, "step {i}: get_state"),
                            Some("set_share_audio") => {
                                let audio = latest["share_audio"].as_str().unwrap();
                                if line["capturing"] == true {
                                    // Starting: off and not kept; otherwise what came of it.
                                    assert!(
                                        latest["phase"] == "starting" || audio != "off",
                                        "step {i}: a capture asked for, and the state says off"
                                    );
                                } else {
                                    assert_eq!(audio, "off", "step {i}: a stop not said");
                                }
                            }
                            Some("set_share_audio_mute") => assert_eq!(
                                latest["share_audio_muted"], line["muted"],
                                "step {i}: set_share_audio_mute"
                            ),
                            _ => {}
                        }
                    }
                    _ => {}
                }
            }
        }
    }
    let want = i32::from(final_phase.as_deref() == Some("failed"));
    assert_eq!(exit, want, "exit after a final phase of {final_phase:?}");
    if started {
        assert!(ended, "a started helper exited without a final state");
    }
    assert!(
        pending.is_empty(),
        "commands never answered: {}",
        pending.len()
    );
}

/// A v3 transcript, how its rig is arranged, and what is played before each helper step.
struct Transcript {
    name: &'static str,
    companion: bool,
    /// Others' applications playing from the start.
    playing: usize,
    refuse_first_publish: bool,
    /// Leaving takes a moment, so that commands sent after `leave` meet a helper leaving.
    slow_leave: bool,
    acts: &'static [(i64, Act)],
}

const TRANSCRIPTS_V3: [Transcript; 10] = [
    Transcript {
        name: "transcript-negotiation-v1-owner.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-negotiation-v2-owner.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-negotiation-v3-owner.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-version-fixed.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-version-fixed-v2-session.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-refused-commands.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: true,
        acts: &[],
    },
    Transcript {
        name: "transcript-share-audio.json",
        companion: true,
        playing: 2,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[(8, Act::Play(0)), (9, Act::Play(1)), (12, Act::Play(3))],
    },
    Transcript {
        name: "transcript-share-audio-publish-refused.json",
        companion: true,
        playing: 1,
        refuse_first_publish: true,
        slow_leave: false,
        acts: &[],
    },
    Transcript {
        name: "transcript-share-audio-application-scope.json",
        companion: true,
        playing: 1,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
    // The same as a v2 owner's, without a companion: a v2 session is what it was.
    Transcript {
        name: "transcript-negotiation-v2-owner.json",
        companion: false,
        playing: 0,
        refuse_first_publish: false,
        slow_leave: false,
        acts: &[],
    },
];

/// Every v3 transcript, its owner side played against the real helper with test devices and
/// the scripted applications, gets the helper side it shows, field for field; what the
/// helper wrote keeps the protocol's rules; nothing of the call was ever captured, and
/// nothing is left open.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn each_v3_transcript_played_by_its_owner_gets_the_helper_side_it_shows() {
    for transcript in &TRANSCRIPTS_V3 {
        let name = transcript.name;
        let world = ShareWorld::default();
        world.play(transcript.playing);
        let mut rig = share_rig_with(
            &world,
            transcript.companion,
            ManagedSetup::default(),
            |sdk| {
                if transcript.refuse_first_publish {
                    sdk.fail_next_share_publish("FAKE: no share grant yet");
                }
                if transcript.slow_leave {
                    sdk.close_takes(Duration::from_millis(300));
                }
            },
        )
        .await;
        let (log, exit) = drive_v3(&mut rig, &world, name, transcript.acts).await;
        check_rules_v3(&log, exit);
        world.assert_never_captured_the_call();
        assert_eq!(world.open(), 0, "{name}: a capture was left open");
        assert_eq!(rig.sdk.frames_while_muted(), 0, "{name}");
        assert_eq!(
            rig.sdk.share_track(),
            None,
            "{name}: a share track was left published"
        );
        eprintln!("{name}: {} steps", log.len());
    }
}

#[test]
fn every_v3_transcript_is_driven_here() {
    let driven: Vec<&str> = TRANSCRIPTS_V3.iter().map(|t| t.name).collect();
    for entry in std::fs::read_dir(fixtures_v3()).unwrap() {
        let name = entry.unwrap().file_name().into_string().unwrap();
        if name.starts_with("transcript-") {
            assert!(driven.contains(&name.as_str()), "{name} is not run");
        }
    }
}

/// The hello is the signed v3 golden, whichever version the owner then picks.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_hello_is_the_v3_golden() {
    let world = ShareWorld::default();
    let mut rig = share_rig(&world, true).await;
    rig.until("the hello", |rig| !rig.lines().is_empty()).await;
    let hello: Value = serde_json::from_str(&rig.lines()[0]).unwrap();
    let mut golden = fixture_v3("event-hello-v1-v2-and-v3.json");
    golden["helper_version"] = hello["helper_version"].clone();
    assert_eq!(hello, golden);
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

// --------------------------------------------------------------- the share's rules

/// While starting the share's sound is off and a request is answered with that and not kept;
/// while connecting it is refused into the state as `not_connected` and not kept either;
/// once connected nothing was published or captured.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn before_connected_a_request_is_refused_into_the_state_and_not_kept() {
    let world = ShareWorld::default();
    world.play(2);
    // Starting: the start-up waits for a key device that is not plugged in.
    let setup = ManagedSetup {
        keyboard_unplugged: true,
        ..ManagedSetup::default()
    };
    let mut rig = share_rig_with(&world, true, setup, |sdk| sdk.hang_next_connect()).await;
    send_start_v3(&mut rig);
    rig.send(&share_on(2));
    rig.send(&v3("set_share_audio_mute", 3, json!({"muted": true})));
    assert!(ack_v3(&rig, 2).await.ok);
    assert!(ack_v3(&rig, 3).await.ok);
    for state in states_v3(&rig) {
        assert_eq!(state.phase.as_str(), "starting");
        assert!(share_is(&state, "off", None, 0), "{state:?}");
    }
    let last = state_v3(&rig).unwrap();
    assert!(
        last.share_audio_muted,
        "the share's mute is kept while starting"
    );

    // Connecting: the first join hangs.
    rig.plug_keyboard();
    until_v3(&rig, "connecting", |state| {
        state.phase.as_str() != "starting"
    })
    .await;
    rig.send(&share_on(4));
    assert!(ack_v3(&rig, 4).await.ok);
    let (_, before) = ack_and_state_before(&rig, 4);
    assert_ne!(before.phase.as_str(), "connected");
    assert!(
        share_is(&before, "unavailable", Some("not_connected"), 0),
        "{before:?}"
    );

    // Connected at last: the refusal was not kept, and nothing was asked of the room.
    until_v3(&rig, "connected", |state| {
        state.phase.as_str() == "connected"
    })
    .await;
    until_v3(&rig, "off once connected", |state| {
        state.phase.as_str() == "connected" && share_is(state, "off", None, 0)
    })
    .await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(rig.sdk.share_published(), 0);
    assert!(world.opened().is_empty());
    assert!(state_v3(&rig).unwrap().share_audio_muted, "the mute stands");
    rig.send(&v3("leave", 5, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// A session with no companion (a TUI's, or standalone) cannot carry a share's sound.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_a_companion_a_request_is_unsupported() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, false).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    assert!(ack_v3(&rig, 2).await.ok);
    let (_, before) = ack_and_state_before(&rig, 2);
    assert!(
        share_is(&before, "unavailable", Some("unsupported"), 0),
        "{before:?}"
    );
    assert_eq!(rig.sdk.share_published(), 0);
    assert!(world.opened().is_empty());
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// The share's sound: one track of its own, published on the command and nothing else, on
/// first and only then captured; the state says on with the count before the ack; nothing of
/// the Conch tree is ever captured; the mix reaches the track; a stop closes every capture
/// within 500 ms and says off before its ack. The microphone's track is untouched throughout.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_request_publishes_the_share_track_captures_and_a_stop_closes_it_within_the_bound() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(rig.sdk.share_published(), 0, "nothing without the command");
    assert!(world.opened().is_empty());
    let mic_calls = rig.sdk.control_calls();

    rig.send(&share_on(2));
    assert!(ack_v3(&rig, 2).await.ok);
    let (_, before) = ack_and_state_before(&rig, 2);
    assert!(share_is(&before, "on", None, 2), "{before:?}");
    assert_eq!(rig.sdk.share_published(), 1);
    assert_eq!(world.open(), 2);
    world.assert_never_captured_the_call();
    let calls = rig.sdk.control_calls();
    let publish = calls.iter().position(|c| *c == Call::SharePublish).unwrap();
    assert!(publish >= mic_calls.len(), "published after the command");
    assert_eq!(
        calls.iter().filter(|c| **c == Call::Publish).count(),
        1,
        "the microphone's track was not published again"
    );
    wait_for("the mix reaching the track", || {
        rig.sdk.share_loud_frames() > 5
    })
    .await;
    assert!(
        !state_v3(&rig).unwrap().transmitting,
        "the share is not the microphone"
    );

    let began = Instant::now();
    rig.send(&share_off(3));
    let ack = ack_v3(&rig, 3).await;
    let took = began.elapsed();
    assert!(ack.ok);
    assert!(
        took < Duration::from_millis(VOICE_CONTROL_CAPTURE_STOP_MS),
        "the stop took {took:?}"
    );
    let (_, before) = ack_and_state_before(&rig, 3);
    assert!(share_is(&before, "off", None, 0), "{before:?}");
    assert_eq!(world.open(), 0, "every capture closed before the ack");
    wait_for("the share's track unpublished", || {
        rig.sdk.share_track().is_none()
    })
    .await;
    assert!(rig.sdk.calls().contains(&Call::ShareUnpublish));
    rig.send(&v3("leave", 4, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// The share's mute mutes its track and nothing else; the microphone's mute and deafen leave
/// the captures and the share's track as they were.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_share_mute_and_the_microphones_mute_and_deafen_are_independent() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    let opened = world.opened();

    // The microphone's mute and deafen: the share as it was.
    let mic_mutes = rig.sdk.count(Call::Mute);
    rig.send(&v3("set_mute", 3, json!({"muted": true})));
    ack_v3(&rig, 3).await;
    rig.send(&v3("set_deafen", 4, json!({"deafened": true})));
    ack_v3(&rig, 4).await;
    let state = state_v3(&rig).unwrap();
    assert!(state.muted && state.deafened);
    assert!(share_is(&state, "on", None, 2), "{state:?}");
    assert!(!state.share_audio_muted);
    assert!(!rig.sdk.share_muted());
    assert_eq!(world.open(), 2);
    assert_eq!(world.opened(), opened, "no capture opened or closed");
    let shares: Vec<Call> = rig
        .sdk
        .calls()
        .into_iter()
        .filter(|c| matches!(c, Call::ShareMute | Call::ShareUnmute))
        .collect();
    assert!(shares.is_empty(), "{shares:?}");

    // The share's mute: its track, and nothing else.
    let mic_mutes_now = rig.sdk.count(Call::Mute);
    rig.send(&v3("set_share_audio_mute", 5, json!({"muted": true})));
    ack_v3(&rig, 5).await;
    let (_, before) = ack_and_state_before(&rig, 5);
    assert!(before.share_audio_muted);
    assert!(
        share_is(&before, "on", None, 2),
        "the track stays published"
    );
    assert!(
        before.muted && before.deafened,
        "the microphone's as they were"
    );
    assert!(rig.sdk.share_muted());
    assert_eq!(
        rig.sdk.count(Call::Mute),
        mic_mutes_now,
        "the microphone's track not touched"
    );
    assert!(mic_mutes_now >= mic_mutes);
    assert_eq!(world.open(), 2, "the captures stay open");

    // Unmuted: the same, the other way.
    rig.send(&v3("set_share_audio_mute", 6, json!({"muted": false})));
    ack_v3(&rig, 6).await;
    assert!(!rig.sdk.share_muted());
    assert!(!state_v3(&rig).unwrap().share_audio_muted);
    rig.send(&v3("set_mute", 7, json!({"muted": false})));
    ack_v3(&rig, 7).await;
    assert!(
        !rig.sdk.share_muted(),
        "the microphone's unmute is not the share's"
    );
    rig.send(&v3("leave", 8, json!({})));
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(world.open(), 0);
}

/// While the share's publish is under way the session goes on: a mute is carried out and its
/// state written at once, and its ack waits behind the share's, which comes once the state
/// says on. Acks keep the order commands were read in.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_pending_publish_holds_acks_in_order_and_blocks_nothing() {
    let world = ShareWorld::default();
    world.play(1);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    let hold = rig.sdk.hold_share_publish();
    rig.send(&share_on(2));
    rig.until("the publish under way", |rig| {
        rig.sdk.calls().contains(&Call::SharePublish)
    })
    .await;
    rig.send(&v3("set_mute", 3, json!({"muted": true})));
    rig.send(&v3("get_state", 4, json!({})));
    until_v3(&rig, "muted at once", |state| state.muted).await;
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(
        ack_of(&rig, 2).is_none() && ack_of(&rig, 3).is_none(),
        "held"
    );
    assert!(
        world.opened().is_empty(),
        "nothing captured before the publish"
    );
    hold.notify_one();
    let acks = [
        ack_v3(&rig, 2).await,
        ack_v3(&rig, 3).await,
        ack_v3(&rig, 4).await,
    ];
    assert!(acks.iter().all(|ack| ack.ok));
    let (two, before) = ack_and_state_before(&rig, 2);
    assert!(share_is(&before, "on", None, 1), "{before:?}");
    let (three, _) = ack_and_state_before(&rig, 3);
    let (four, _) = ack_and_state_before(&rig, 4);
    assert!(two < three && three < four, "in the order read");
    let events = events_v3(&rig);
    assert!(
        matches!(events[four - 1], VoiceControlEventV3::State(_)),
        "get_state is answered with a state and then its ack"
    );
    rig.send(&v3("leave", 5, json!({})));
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(world.open(), 0);
}

/// The owner going closes the captures and the track with the rest, within the owner-loss
/// bound; a publish still under way when it goes is let go of, and what it publishes is
/// taken back.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn owner_loss_closes_the_captures_and_the_track_within_the_bound() {
    // While capturing.
    let world = ShareWorld::default();
    world.play(3);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    assert_eq!(world.open(), 3);
    let began = Instant::now();
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert!(began.elapsed() < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS));
    assert_eq!(world.open(), 0);
    assert!(rig.sdk.share_muted(), "silenced at once");
    let last = state_v3(&rig).unwrap();
    assert_eq!(last.phase.as_str(), "stopped");
    assert!(share_is(&last, "off", None, 0));

    // While publishing.
    let world = ShareWorld::default();
    world.play(1);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    let hold = rig.sdk.hold_share_publish();
    rig.send(&share_on(2));
    rig.until("the publish under way", |rig| {
        rig.sdk.calls().contains(&Call::SharePublish)
    })
    .await;
    let began = Instant::now();
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert!(began.elapsed() < Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS));
    let acked = ack_of(&rig, 2).expect("every command read is answered");
    assert!(acked.ok);
    hold.notify_one();
    wait_for("the late publish taken back", || {
        rig.sdk.calls().contains(&Call::ShareUnpublish)
    })
    .await;
    assert!(world.opened().is_empty(), "nothing was ever captured");
}

/// The server taking the share's grant back is seen, as with the real SDK, only as a change
/// of this participant's permission: the helper unpublishes the track itself, closes the
/// captures, and the state says off. Nothing of the request is kept.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_grant_taken_back_unpublishes_the_track_closes_the_captures_and_says_off() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    assert!(rig.sdk.share_track().is_some());
    assert!(rig.sdk.revoke_share_grant());
    until_v3(&rig, "off", |state| share_is(state, "off", None, 0)).await;
    assert_eq!(world.open(), 0);
    wait_for("the track unpublished by the helper", || {
        rig.sdk.share_track().is_none()
    })
    .await;
    assert!(rig.sdk.calls().contains(&Call::ShareUnpublish));
    // A new stream later is not captured: the request is gone.
    world.play(3);
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(world.open(), 0);
    assert_eq!(rig.sdk.share_published(), 1);
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// A transient reconnect of the SDK closes the captures and says `not_connected`; the same
/// room back captures again, on the same track, with its mute applied again after a
/// republish. A full disconnect keeps nothing for the next connection.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_reconnect_closes_the_captures_and_the_same_room_opens_them_again() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    rig.send(&v3("set_share_audio_mute", 3, json!({"muted": true})));
    ack_v3(&rig, 3).await;

    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    until_v3(&rig, "not connected", |state| {
        state.phase.as_str() == "reconnecting"
            && share_is(state, "unavailable", Some("not_connected"), 0)
    })
    .await;
    assert_eq!(world.open(), 0);
    // As the real SDK does: the same track republished under a new id, its unpublish not
    // reported, then the reconnect.
    rig.sdk.republish_share();
    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    until_v3(&rig, "on again", |state| {
        state.phase.as_str() == "connected" && share_is(state, "on", None, 2)
    })
    .await;
    assert_eq!(world.open(), 2);
    assert_eq!(rig.sdk.share_published(), 1, "the same track");
    assert_eq!(rig.sdk.share_track().as_deref(), Some("TR_share1r1"));
    assert!(rig.sdk.share_muted(), "the share's mute applied again");
    // The republish enabled the same, still-muted track, on which LiveKit's mute is a no-op:
    // the helper disables it, and hands it nothing while muted. The mix runs meanwhile.
    let mixed = world.mixed_frames();
    wait_for("the mix running after the reconnect", || {
        world.mixed_frames() > mixed + 20
    })
    .await;
    assert!(!rig.sdk.share_enabled(), "a muted share left enabled");
    assert_eq!(
        rig.sdk.share_leaked_frames(),
        0,
        "frames reached the muted, enabled track"
    );
    // A stop reaches the republished track: none is left that nobody holds.
    rig.send(&share_off(5));
    ack_v3(&rig, 5).await;
    wait_for("the republished track unpublished", || {
        rig.sdk.share_track().is_none()
    })
    .await;
    rig.send(&share_on(6));
    ack_v3(&rig, 6).await;
    until_v3(&rig, "on", |state| share_is(state, "on", None, 2)).await;

    // The connection lost for good: the next one has no share.
    rig.sdk.disconnect(conch_voice::sdk::DisconnectReason::Lost);
    until_v3(&rig, "off", |state| share_is(state, "off", None, 0)).await;
    assert_eq!(world.open(), 0);
    until_v3(&rig, "connected again", |state| {
        state.phase.as_str() == "connected"
    })
    .await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(world.open(), 0);
    assert_eq!(
        rig.sdk.share_published(),
        2,
        "the first and the one asked after the stop"
    );
    rig.send(&v3("leave", 7, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// A revoke followed at once by a re-grant, both read after they happened, still ends the
/// share's sound: the change that said no is enough, whatever the permission says by then.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_revoke_and_an_immediate_regrant_still_end_the_share_sound() {
    let world = ShareWorld::default();
    world.play(2);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    assert!(rig.sdk.revoke_and_regrant_share());
    until_v3(&rig, "off", |state| share_is(state, "off", None, 0)).await;
    assert_eq!(world.open(), 0);
    wait_for("the track unpublished by the helper", || {
        rig.sdk.share_track().is_none()
    })
    .await;
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// After a reconnect the captures open again only on a track still allowed and still
/// published. A grant taken back during the reconnect, or a republish that failed, ends the
/// share's sound: off, nothing captured, and the track (if the client still has it)
/// unpublished.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_reconnect_reopens_no_captures_on_a_track_taken_back_or_lost() {
    for revoked in [true, false] {
        let world = ShareWorld::default();
        world.play(2);
        let mut rig = share_rig(&world, true).await;
        send_start_v3(&mut rig);
        connected_v3(&rig).await;
        rig.send(&share_on(2));
        ack_v3(&rig, 2).await;
        assert!(rig.sdk.emit(SdkEvent::Reconnecting));
        until_v3(&rig, "not connected", |state| {
            share_is(state, "unavailable", Some("not_connected"), 0)
        })
        .await;
        if revoked {
            // The permission changed while reconnecting; the event is read after.
            rig.sdk.revoke_share_grant();
        } else {
            rig.sdk.lose_share_in_republish();
        }
        assert!(rig.sdk.emit(SdkEvent::Reconnected));
        until_v3(&rig, "off", |state| {
            state.phase.as_str() == "connected" && share_is(state, "off", None, 0)
        })
        .await;
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert_eq!(world.open(), 0, "revoked {revoked}");
        assert!(
            world.opened().len() <= 2,
            "nothing opened after the reconnect"
        );
        assert!(rig.sdk.share_track().is_none(), "revoked {revoked}");
        rig.send(&v3("leave", 3, json!({})));
        assert_eq!(rig.exit_code().await, 0);
    }
}

/// What to leave out cannot be worked out: nothing is captured, and the state says why; once
/// it can be, the captures open.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_unreadable_process_tree_captures_nothing_until_it_can_be_read() {
    let world = ShareWorld::default();
    world.play(2);
    world.0.no_tree.store(true, Ordering::SeqCst);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    let (_, before) = ack_and_state_before(&rig, 2);
    assert!(
        share_is(&before, "unavailable", Some("no_process_tree"), 0),
        "{before:?}"
    );
    assert!(world.opened().is_empty());
    world.0.no_tree.store(false, Ordering::SeqCst);
    until_v3(&rig, "on", |state| share_is(state, "on", None, 2)).await;
    world.assert_never_captured_the_call();
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(world.open(), 0);
}

/// Streams come and go during the share and the count follows; nothing playing is on, silent
/// and `no_sources`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_count_follows_the_applications_and_none_is_no_sources() {
    let world = ShareWorld::default();
    world.play(0);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    let (_, before) = ack_and_state_before(&rig, 2);
    assert!(share_is(&before, "on", Some("no_sources"), 0), "{before:?}");
    wait_for("silence reaching the track", || rig.sdk.share_frames() > 5).await;
    assert_eq!(rig.sdk.share_loud_frames(), 0);
    world.play(4);
    until_v3(&rig, "four", |state| share_is(state, "on", None, 4)).await;
    world.play(1);
    until_v3(&rig, "one", |state| share_is(state, "on", None, 1)).await;
    assert_eq!(world.open(), 1);
    world.assert_never_captured_the_call();
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_live_share_reports_bounded_stream_names_and_decisions() {
    let world = ShareWorld::default();
    world.play(1);
    {
        let mut graph = world.0.streams.lock().unwrap();
        graph.1[0].media_name = Some("/home/private/song.flac".into());
        graph.1[4].media_name = Some("A song".into());
        graph.1.extend([
            StreamFacts {
                serial: Some(9101),
                sec_pid: Some(RENDERER),
                application_name: Some("Desktop renderer".into()),
                ..StreamFacts::default()
            },
            StreamFacts {
                sec_pid: Some(9102),
                application_name: Some("/home/private/song.flac".into()),
                ..StreamFacts::default()
            },
        ]);
    }
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    rig.send(&share_on(2));
    ack_v3(&rig, 2).await;
    let current = || {
        helper_values(&rig, Some(Version::V3))
            .into_iter()
            .rfind(|event| event["kind"] == "state")
            .unwrap()
    };
    assert_eq!(
        current()["share_audio_streams"],
        json!([
            {"application": "conch-voice", "media": "", "reason": "call_playback"},
            {"application": "Chromium", "media": "", "reason": "conch_application"},
            {"application": NAME, "media": "", "reason": "conch_name"},
            {"application": "", "media": "", "reason": "unknown_owner"},
            {"application": "player-0", "media": "A song", "reason": "captured"},
            {"application": "Desktop renderer", "media": "", "reason": "conch_client"},
            {"application": "", "media": "", "reason": "missing_target"}
        ])
    );
    {
        let mut graph = world.0.streams.lock().unwrap();
        graph.0 += 1;
        graph.1[4].media_name = Some("Another song".into());
    }
    wait_for("the new media name with the same source count", || {
        current()["share_audio_streams"][4]
            == json!({"application": "player-0", "media": "Another song", "reason": "captured"})
    })
    .await;
    world.play(60);
    until_v3(&rig, "60 sources", |state| share_is(state, "on", None, 60)).await;
    assert_eq!(
        current()["share_audio_streams"].as_array().unwrap().len(),
        50
    );
    {
        let mut graph = world.0.streams.lock().unwrap();
        graph.0 += 1;
        graph.1.clear();
    }
    until_v3(&rig, "no streams", |state| {
        share_is(state, "on", Some("no_sources"), 0)
    })
    .await;
    assert!(current().get("share_audio_streams").is_none());
    rig.send(&share_off(3));
    ack_v3(&rig, 3).await;
    assert!(
        helper_values(&rig, Some(Version::V3))
            .into_iter()
            .rfind(|event| event["kind"] == "state")
            .unwrap()
            .get("share_audio_streams")
            .is_none()
    );
    rig.send(&v3("leave", 4, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

/// The request's application name is private. Stream labels come from the graph.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_application_name_is_never_repeated() {
    let world = ShareWorld::default();
    world.play(1);
    let mut rig = share_rig(&world, true).await;
    send_start_v3(&mut rig);
    connected_v3(&rig).await;
    let secret = "FAKE-application-name-do-not-print";
    rig.send(&v3(
        "set_share_audio",
        2,
        json!({"capturing": true, "scope": "system_excluding_call", "application_name": secret}),
    ));
    ack_v3(&rig, 2).await;
    rig.send(&v3("leave", 3, json!({})));
    assert_eq!(rig.exit_code().await, 0);
    let text = rig.events.text();
    assert!(!text.contains(secret), "{text}");
    assert!(
        text.contains("player-0"),
        "the graph's application is named: {text}"
    );
}

// ------------------------------------------------------------------ the structure

/// Every file under `src/`.
fn sources() -> Vec<(String, String)> {
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    std::fs::read_dir(src)
        .unwrap()
        .map(|entry| {
            let path = entry.unwrap().path();
            (
                path.file_name().unwrap().to_string_lossy().into_owned(),
                std::fs::read_to_string(&path).unwrap(),
            )
        })
        .collect()
}

/// No monitor is ever opened (`docs/design/linux-share-audio.md` §12): `dmn-audio`'s sink-
/// monitor target and its fallback are named nowhere in the helper's source, no capture asks
/// to capture a sink, and the one capture of an application stream is made in one place,
/// from a spec of a serial. A tripwire in the manner of
/// `TestUpdateParticipantStaysInThePermissionFunction`: the needles are built here, so that
/// this file does not hold them.
#[test]
fn no_monitor_target_or_fallback_is_named_in_the_helpers_source() {
    let sources = sources();
    assert!(sources.len() >= 20, "src/ was read");
    let sink_monitor = ["Sink", "Monitor"].concat();
    let fallback = ["monitor", "_", "fallback"].concat();
    let capture_sink = ["capture", ".", "sink"].concat();
    let capture_sink_field = ["capture", "_", "sink"].concat();
    // `CaptureTarget::interviewer` returns the sink monitor, or the fallback, without naming
    // either; `PipeWireSource` is the capture type that takes any target. Neither belongs
    // anywhere in the helper, not only in its share modules.
    let interviewer = ["inter", "viewer"].concat();
    let any_source = ["PipeWire", "Source"].concat();
    for (file, code) in &sources {
        for needle in [
            &sink_monitor,
            &fallback,
            &capture_sink,
            &capture_sink_field,
            &interviewer,
            &any_source,
        ] {
            assert!(!code.contains(needle.as_str()), "{file} names {needle}");
        }
    }
    // The share's captures name no capture type of `dmn-audio` but the application stream's.
    for (file, code) in sources
        .iter()
        .filter(|(file, _)| file.starts_with("share_"))
    {
        for other in [
            "CaptureTarget",
            "MicCapture",
            "PipeWireSource",
            "Playback::new",
        ] {
            assert!(!code.contains(other), "{file} names {other}");
        }
    }
    let made: Vec<&String> = sources
        .iter()
        .filter(|(_, code)| code.contains("StreamCapture::new("))
        .map(|(file, _)| file)
        .collect();
    assert_eq!(made, ["share_capture_linux.rs"]);
}

// --------------------------------------------- with PipeWire: the shadow of L-S6

/// Writes a 48 kHz stereo 16-bit WAV of a tone, `seconds` long.
#[cfg(target_os = "linux")]
fn tone_wav(path: &Path, hz: f32, seconds: u32) {
    let frames = 48_000 * seconds;
    let data_len = frames * 4;
    let mut bytes = Vec::with_capacity(44 + data_len as usize);
    bytes.extend_from_slice(b"RIFF");
    bytes.extend_from_slice(&(36 + data_len).to_le_bytes());
    bytes.extend_from_slice(b"WAVEfmt ");
    bytes.extend_from_slice(&16_u32.to_le_bytes());
    bytes.extend_from_slice(&1_u16.to_le_bytes());
    bytes.extend_from_slice(&2_u16.to_le_bytes());
    bytes.extend_from_slice(&48_000_u32.to_le_bytes());
    bytes.extend_from_slice(&(48_000_u32 * 4).to_le_bytes());
    bytes.extend_from_slice(&4_u16.to_le_bytes());
    bytes.extend_from_slice(&16_u16.to_le_bytes());
    bytes.extend_from_slice(b"data");
    bytes.extend_from_slice(&data_len.to_le_bytes());
    for n in 0..frames {
        let t = n as f32 / 48_000.0;
        let sample = (0.2 * (std::f32::consts::TAU * hz * t).sin() * 32767.0) as i16;
        bytes.extend_from_slice(&sample.to_le_bytes());
        bytes.extend_from_slice(&sample.to_le_bytes());
    }
    std::fs::write(path, bytes).unwrap();
}

/// The power of `hz` in `samples` (48 kHz), by Goertzel, per sample.
#[cfg(target_os = "linux")]
fn power(samples: &[f32], hz: f32) -> f32 {
    let k = 2.0 * (std::f32::consts::TAU * hz / 48_000.0).cos();
    let (mut s1, mut s2) = (0.0_f32, 0.0_f32);
    for sample in samples {
        let s = sample + k * s1 - s2;
        s2 = s1;
        s1 = s;
    }
    (s1 * s1 + s2 * s2 - k * s1 * s2) / samples.len().max(1) as f32
}

/// The automated shadow of L-S6 (#922 LE8), not its replacement. Two other applications play
/// tones (`pw-play`, detached so that they are not of this process's tree) and this process
/// plays a third, as the helper plays the call, all into the null sink named by
/// `CONCH_TEST_NULL_SINK`; the share's real PipeWire captures, rule and mix take what is
/// eligible, and the mix holds the two and none of the third.
///
/// Needs: a running PipeWire session, `pw-play` on the path, and a null sink the tester made
/// (`pactl load-module module-null-sink sink_name=conch-test`), named by
/// `CONCH_TEST_NULL_SINK`. The test changes no device and no default; it only plays into the
/// sink it is given and captures application streams.
#[cfg(target_os = "linux")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs PipeWire, pw-play and a null sink named by CONCH_TEST_NULL_SINK"]
async fn with_pipewire_the_mix_holds_the_other_applications_and_never_the_helpers_own_playback() {
    use conch_voice::audio::AudioSystem;
    use conch_voice::share_capture::{Engine, FRAME_QUEUE, Report, Reported, ShareCounters};
    use conch_voice_audio::{FRAME_LEN, SpeakerSink};

    let sink = std::env::var("CONCH_TEST_NULL_SINK")
        .expect("CONCH_TEST_NULL_SINK names the null sink to play into");
    let dir = tempfile::tempdir().unwrap();
    let (a, b) = (dir.path().join("a.wav"), dir.path().join("b.wav"));
    tone_wav(&a, 440.0, 20);
    tone_wav(&b, 660.0, 20);
    // Detached: `sh` exits, and `pw-play` is no descendant of this process's parent.
    let mut players = Vec::new();
    for file in [&a, &b] {
        let out = std::process::Command::new("sh")
            .arg("-c")
            .arg(format!(
                "pw-play --target '{sink}' '{}' >/dev/null 2>&1 & echo $!",
                file.display()
            ))
            .output()
            .unwrap();
        players.push(String::from_utf8(out.stdout).unwrap().trim().to_owned());
    }
    let kill = |players: &[String]| {
        for pid in players {
            let _ = std::process::Command::new("kill").arg(pid).status();
        }
    };

    // The helper's own playback of the call: a tone of this process into the same sink.
    let audio = AudioSystem::connect().unwrap();
    let mut own = audio.speakers(Some(sink.as_str())).unwrap();
    let own_tone = std::thread::spawn(move || {
        let mut phase = 0.0_f32;
        let until = Instant::now() + Duration::from_secs(8);
        while Instant::now() < until {
            for _ in 0..own.frames_wanted() {
                let mut frame = [0.0; FRAME_LEN];
                for sample in &mut frame {
                    *sample = 0.2 * phase.sin();
                    phase = (phase + std::f32::consts::TAU * 1_250.0 / 48_000.0)
                        % std::f32::consts::TAU;
                }
                own.play(&frame);
            }
            std::thread::sleep(Duration::from_millis(2));
        }
    });
    tokio::time::sleep(Duration::from_secs(1)).await;

    let ShareSource::Captures(captures) = audio.share_source() else {
        panic!("Linux captures");
    };
    let (frames_tx, mut frames) = tokio::sync::mpsc::channel(FRAME_QUEUE);
    let (reports_tx, mut reports) = tokio::sync::mpsc::unbounded_channel::<Reported>();
    let counters = Arc::new(ShareCounters::default());
    let engine = Engine::spawn(
        &captures,
        "conch-test-desktop".to_owned(),
        1,
        frames_tx,
        reports_tx,
        Arc::clone(&counters),
    )
    .unwrap();
    let mut left = Vec::new();
    let until = Instant::now() + Duration::from_secs(5);
    while Instant::now() < until {
        while let Ok(Reported { report, .. }) = reports.try_recv() {
            eprintln!("share audio: {report:?}");
        }
        if let Ok(Some(frame)) =
            tokio::time::timeout(Duration::from_millis(50), frames.recv()).await
        {
            left.extend(frame.chunks_exact(2).map(|pair| pair[0]));
        }
    }
    engine.stop();
    loop {
        match tokio::time::timeout(Duration::from_secs(2), reports.recv()).await {
            Ok(Some(Reported {
                report: Report::Closed,
                ..
            })) => break,
            Ok(Some(_)) => {}
            _ => panic!("the engine did not close"),
        }
    }
    kill(&players);
    let _ = own_tone.join();
    assert_eq!(counters.open.load(Ordering::SeqCst), 0);
    // The last three seconds, once every capture is up.
    let tail = &left[left.len().saturating_sub(3 * 48_000)..];
    let (p440, p660, p1250) = (power(tail, 440.0), power(tail, 660.0), power(tail, 1_250.0));
    eprintln!("440 Hz {p440}, 660 Hz {p660}, 1250 Hz {p1250}");
    assert!(
        p440 > 1e-3 && p660 > 1e-3,
        "the other applications are in the mix"
    );
    assert!(
        p1250 * 1_000.0 < p440.min(p660),
        "the helper's own playback is in the mix"
    );
}
