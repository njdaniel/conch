//! `conch-voice managed` (`docs/design/tui-voice.md` §13), run in-process between a fake
//! SDK and a stub `conchd`, driven by the owner's side of the shared transcripts in
//! `pkg/schema/testdata/voice-control/`, with what the helper writes checked against the
//! rules of §5 as the Go reader checks the transcripts themselves; and the bounds of §3 as
//! numbers: owner loss with the key device being read, while transmitting, shuts the gate
//! within 100 ms, closes capture within 500 ms and ends within 3 s. The same for
//! `conch.voice_control.v2` (§20): each transcript in `testdata/voice-control-v2/` against
//! the real helper with test devices, the device switch and the open microphone, and the
//! bounds in open mode while the microphone hears sound.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit: the microphone is a
//! tone, the speakers count, the keyboard is a named pipe, and LiveKit is the fake.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

use conch_voice::managed::Version;
use conch_voice::sdk::DisconnectReason;
use conch_voice_api::{
    VOICE_CONTROL_EVENT_QUEUE_LINES, VOICE_CONTROL_LEAVE_EXIT_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS,
    VoiceControlCommand, VoiceControlCommandKind, VoiceControlErrorCode, VoiceControlEvent,
    VoiceControlHelloV1, VoiceControlPhase, VoiceControlReason, VoiceControlStateV1, parse_command,
    parse_command_v2, parse_event, parse_event_v2,
};
use serde_json::{Value, json};
use tokio::sync::Notify;

use support::fake::Call;
use support::managed::{KEYS_TOML, Login, ManagedRig, ManagedSetup, World, level_of};
use support::stub::{Reply, Stub, whoami};

/// `pkg/schema/testdata/voice-control`, read in place.
fn fixtures() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../pkg/schema/testdata/voice-control")
}

fn fixture(name: &str) -> Value {
    let path = fixtures().join(name);
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap()
}

/// One step of what happened, in the order the owner saw it.
#[derive(Debug, Clone)]
enum Logged {
    Owner(String),
    Close,
    Signal,
    Helper(VoiceControlEvent),
}

/// A command the owner wrote and has not seen answered, with the refusal it must get when
/// that is already known.
struct Pending {
    id: i64,
    command: Result<VoiceControlCommand, ()>,
    refusal: Option<VoiceControlErrorCode>,
}

/// The rules of §5, checked against what really happened: the port of the Go reader's
/// `checkVoiceControlTranscript`. Seq may skip (events dropped for a slow owner) but never
/// repeats or goes back.
fn check_rules(log: &[Logged], exit: i32) {
    let mut seq = 0;
    let mut last_id = 0;
    let mut hello: Option<VoiceControlHelloV1> = None;
    let (mut started, mut ended) = (false, false);
    let mut final_phase = None;
    let mut state: Option<VoiceControlStateV1> = None;
    let mut previous_was_state = false;
    let mut pending: VecDeque<Pending> = VecDeque::new();
    let mut want_protocol_error = None;
    let mut want_failed_after = false;
    let shown = || format!("{log:#?}");
    for (i, step) in log.iter().enumerate() {
        match step {
            Logged::Close | Logged::Signal => {}
            Logged::Owner(line) => {
                assert!(
                    hello
                        .as_ref()
                        .is_some_and(|h| h.offers("conch.voice_control.v1")),
                    "step {i}: the owner wrote before a hello offering v1\n{}",
                    shown()
                );
                let parsed = parse_command(line.as_bytes());
                let id = match &parsed {
                    Ok(command) => Some(command.id()),
                    Err(refusal) => refusal.id,
                };
                let is_start = matches!(parsed, Ok(VoiceControlCommand::Start(_)));
                if !started && !is_start {
                    want_protocol_error = Some(match &parsed {
                        Err(refusal) if refusal.code.is_fatal() => refusal.code,
                        _ => VoiceControlErrorCode::NotStarted,
                    });
                    continue;
                }
                let Some(id) = id else {
                    want_protocol_error = parsed.err().map(|refusal| refusal.code);
                    continue;
                };
                if id <= last_id {
                    want_protocol_error = Some(VoiceControlErrorCode::IdNotIncreasing);
                    continue;
                }
                last_id = id;
                let stopping = state
                    .as_ref()
                    .is_some_and(|s| s.phase == VoiceControlPhase::Stopping);
                let refusal = match &parsed {
                    Err(refusal) => Some(refusal.code),
                    Ok(VoiceControlCommand::Start(_)) if started => {
                        Some(VoiceControlErrorCode::AlreadyStarted)
                    }
                    Ok(VoiceControlCommand::Start(_)) => {
                        started = true;
                        None
                    }
                    Ok(VoiceControlCommand::SetMute(_) | VoiceControlCommand::SetDeafen(_))
                        if stopping =>
                    {
                        Some(VoiceControlErrorCode::Stopping)
                    }
                    Ok(_) => None,
                };
                pending.push_back(Pending {
                    id,
                    command: parsed.map_err(|_| ()),
                    refusal,
                });
            }
            Logged::Helper(event) => {
                assert!(
                    !ended,
                    "step {i}: written after the final state\n{}",
                    shown()
                );
                assert!(
                    event.seq() > seq,
                    "step {i}: seq {} after {seq}",
                    event.seq()
                );
                seq = event.seq();
                if hello.is_none() {
                    assert!(
                        matches!(event, VoiceControlEvent::Hello(_)),
                        "step {i}: the first event is not a hello"
                    );
                }
                if want_failed_after {
                    let failed = matches!(event, VoiceControlEvent::State(s)
                        if s.phase == VoiceControlPhase::Failed
                            && s.reason == Some(VoiceControlReason::ProtocolError));
                    assert!(
                        failed,
                        "step {i}: a protocol_error not followed by failed with reason \
                         protocol_error\n{}",
                        shown()
                    );
                }
                let was_state = previous_was_state;
                previous_was_state = matches!(event, VoiceControlEvent::State(_));
                match event {
                    VoiceControlEvent::Hello(h) => {
                        assert!(hello.is_none(), "step {i}: a second hello");
                        hello = Some(h.clone());
                    }
                    VoiceControlEvent::ProtocolError(e) => {
                        assert_eq!(
                            want_protocol_error,
                            Some(e.code),
                            "step {i}: protocol_error\n{}",
                            shown()
                        );
                        want_protocol_error = None;
                        want_failed_after = true;
                    }
                    VoiceControlEvent::State(s) => {
                        assert_eq!(
                            want_protocol_error, None,
                            "step {i}: a state where a protocol_error was due"
                        );
                        assert!(
                            started || want_failed_after,
                            "step {i}: a state before a start was accepted"
                        );
                        want_failed_after = false;
                        if s.phase.is_final() {
                            ended = true;
                            final_phase = Some(s.phase);
                        }
                        state = Some(s.clone());
                    }
                    VoiceControlEvent::Ack(a) => {
                        let next = pending.pop_front().unwrap_or_else(|| {
                            panic!("step {i}: an ack for {} that nothing asked for", a.id)
                        });
                        assert_eq!(next.id, a.id, "step {i}: acks out of order\n{}", shown());
                        if let Some(refusal) = next.refusal {
                            assert!(!a.ok && a.error == Some(refusal), "step {i}: {a:?}");
                            continue;
                        }
                        assert!(a.ok, "step {i}: {a:?} refused for no reason\n{}", shown());
                        let latest = state
                            .as_ref()
                            .unwrap_or_else(|| panic!("step {i}: acknowledged before any state"));
                        match next.command {
                            Ok(VoiceControlCommand::SetMute(c)) => {
                                assert_eq!(latest.muted, c.muted, "step {i}: set_mute");
                            }
                            Ok(VoiceControlCommand::SetDeafen(c)) => {
                                assert_eq!(latest.deafened, c.deafened, "step {i}: set_deafen");
                            }
                            Ok(VoiceControlCommand::Leave(_)) => assert!(
                                latest.phase == VoiceControlPhase::Stopping
                                    || latest.phase.is_final(),
                                "step {i}: leave acknowledged in {:?}",
                                latest.phase
                            ),
                            Ok(VoiceControlCommand::GetState(_)) => assert!(
                                was_state,
                                "step {i}: get_state acknowledged without a state before it"
                            ),
                            _ => {}
                        }
                    }
                    VoiceControlEvent::Unknown { .. } => {}
                }
            }
        }
    }
    let want = i32::from(final_phase == Some(VoiceControlPhase::Failed));
    assert_eq!(exit, want, "exit after a final phase of {final_phase:?}");
    if started {
        assert!(ended, "a started helper exited without a final state");
    }
    assert!(
        pending.is_empty(),
        "commands never answered: {:?}",
        pending.len()
    );
}

/// Whether the helper's event is the one a transcript's step shows, by kind, phase and
/// reason, and for a state also by what a person sees of their own state.
fn matches(event: &VoiceControlEvent, want: &Value) -> bool {
    let words = |value: &Value| value.as_str().map(str::to_owned);
    match event {
        VoiceControlEvent::Hello(_) => want["kind"] == "hello",
        VoiceControlEvent::Ack(a) => {
            want["kind"] == "ack"
                && want["id"] == a.id
                && want["ok"] == a.ok
                && words(&want["error"]) == a.error.map(|e| e.as_str().to_owned())
        }
        VoiceControlEvent::ProtocolError(e) => {
            want["kind"] == "protocol_error" && want["code"] == e.code.as_str()
        }
        VoiceControlEvent::State(s) => {
            want["kind"] == "state"
                && want["phase"] == s.phase.as_str()
                && words(&want["reason"]) == s.reason.map(|r| r.as_str().to_owned())
                && want["transmitting"] == s.transmitting
                && want["muted"] == s.muted
                && want["deafened"] == s.deafened
        }
        VoiceControlEvent::Unknown { .. } => false,
    }
}

/// The phase and reason of a state, which is what a repeated state is told apart by.
fn phase_of(event: &VoiceControlEvent) -> Option<(VoiceControlPhase, Option<VoiceControlReason>)> {
    match event {
        VoiceControlEvent::State(s) => Some((s.phase, s.reason)),
        _ => None,
    }
}

/// Plays a transcript's owner side against `rig` and waits for each of its helper steps in
/// what the helper writes, doing to the fake world what the transcript's states say
/// happened there (a press, a rotation, a duplicate). Between two steps the helper may
/// write states that repeat the phase and reason of the last one matched, with other
/// fields changed (capture, `retry_in_ms`): states are written on every change, and a
/// transcript shows the ones that matter. Returns the log and the exit code.
async fn drive(rig: &mut ManagedRig, name: &str) -> (Vec<Logged>, i32) {
    let doc = fixture(name);
    let steps = doc["steps"].as_array().unwrap().clone();
    let mut log = Vec::new();
    let mut cursor = 0;
    let mut last_state = None;
    let mut done = std::collections::BTreeSet::new();
    for step in &steps {
        if step["from"] == "owner" {
            if step["close"] == "stdin" {
                rig.close();
                log.push(Logged::Close);
            } else if step["signal"].is_string() {
                rig.signal();
                log.push(Logged::Signal);
            } else {
                let mut message = step["message"].clone();
                if message["kind"] == "start" {
                    message["server"] = json!(rig.conchd.url());
                }
                let line = message.to_string();
                rig.line(line.as_bytes());
                log.push(Logged::Owner(line));
            }
            continue;
        }
        if let Some(code) = step["exit"].as_i64() {
            let exit = rig.exit_code().await;
            let events = rig.read();
            for event in &events[cursor..] {
                assert!(
                    phase_of(event).is_some() && phase_of(event) == last_state,
                    "{name}: written after the last step: {event:?}"
                );
                log.push(Logged::Helper(event.clone()));
            }
            assert_eq!(i64::from(exit), code, "{name}: exit\n{}", rig.events.text());
            return (log, exit);
        }
        let want = &step["message"];
        // What the transcript's state says happened in the world.
        if want["kind"] == "state" {
            let action = if want["transmitting"] == true {
                Some("press")
            } else if want["phase"] == "reconnecting" && want["reason"] == "room_deleted" {
                Some("rotate")
            } else if want["reason"] == "duplicate_identity" {
                Some("duplicate")
            } else {
                None
            };
            if let Some(action) = action
                && done.insert(action)
            {
                match action {
                    "press" => rig.press_talk(),
                    "rotate" => rig.sdk.disconnect(DisconnectReason::RoomDeleted),
                    _ => rig.sdk.disconnect(DisconnectReason::DuplicateIdentity),
                }
            }
        }
        let deadline = Instant::now() + Duration::from_secs(10);
        let found = loop {
            let events = rig.read();
            let mut found = None;
            for (k, event) in events.iter().enumerate().skip(cursor) {
                if matches(event, want) {
                    found = Some((k, events.clone()));
                    break;
                }
                assert!(
                    phase_of(event).is_some() && phase_of(event) == last_state,
                    "{name}: wanted {want}, the helper wrote {event:?}\n{}",
                    rig.events.text()
                );
            }
            if let Some(found) = found {
                break found;
            }
            assert!(
                Instant::now() < deadline,
                "{name}: timed out waiting for {want}\n{}",
                rig.events.text()
            );
            tokio::time::sleep(Duration::from_millis(2)).await;
        };
        let (at, events) = found;
        for event in &events[cursor..=at] {
            log.push(Logged::Helper(event.clone()));
        }
        if let Some(phase) = phase_of(&events[at]) {
            last_state = Some(phase);
        }
        cursor = at + 1;
    }
    panic!("{name} has no exit step");
}

/// The transcripts run here, with what each needs of the world before it starts.
const DRIVEN: [&str; 8] = [
    "transcript-join-mute-leave.json",
    "transcript-owner-lost.json",
    "transcript-signal.json",
    "transcript-version-mismatch.json",
    "transcript-bad-first-command.json",
    "transcript-refused-commands.json",
    "transcript-identity-mismatch.json",
    "transcript-rotation-then-duplicate.json",
];

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn each_transcript_played_by_its_owner_gets_the_helper_side_it_shows() {
    for name in DRIVEN {
        let conchd = Stub::start().await;
        if name == "transcript-identity-mismatch.json" {
            conchd.next_whoami(whoami(8));
        }
        let mut rig = ManagedRig::start_with(conchd, ManagedSetup::default(), |sdk| {
            if name == "transcript-refused-commands.json" {
                // So that the set_mute the owner sends after leave meets a helper that is
                // still leaving, as in the transcript.
                sdk.close_takes(Duration::from_millis(300));
            }
        })
        .await;
        let (log, exit) = drive(&mut rig, name).await;
        check_rules(&log, exit);
        if name == "transcript-identity-mismatch.json"
            || name == "transcript-bad-first-command.json"
            || name == "transcript-version-mismatch.json"
        {
            // Nothing was opened and no session asked for.
            assert_eq!(rig.mic_opened(), 0, "{name}");
            assert_eq!(rig.conchd.session_requests(), 0, "{name}");
            assert!(rig.sdk.calls().is_empty(), "{name}");
        }
    }
}

#[test]
fn every_transcript_is_driven_here_or_tested_on_its_own() {
    // The gap and its resync is tested on its own below: a gap needs a slow owner.
    let mut covered: Vec<&str> = DRIVEN.to_vec();
    covered.push("transcript-gap-resync.json");
    for entry in std::fs::read_dir(fixtures()).unwrap() {
        let name = entry.unwrap().file_name().into_string().unwrap();
        if name.starts_with("transcript-") {
            assert!(covered.contains(&name.as_str()), "{name} is not run");
        }
    }
}

/// The fake secrets of `sensitive.json`: none may be on the event pipe.
fn fake_secrets() -> Vec<String> {
    fixture("sensitive.json")["fake_secrets"]
        .as_array()
        .unwrap()
        .iter()
        .map(|secret| secret.as_str().unwrap().to_owned())
        .collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn no_secret_reaches_the_event_pipe() {
    let secrets = fake_secrets();
    // The rig's login is the first, and the stub's join tokens and room names begin with
    // the second and the third.
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_LOGIN));
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_JOIN));
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_ROOM));

    let mut rig = ManagedRig::start().await;
    // A start with a field it does not declare, named token: ignored, and never repeated.
    let mut start = rig.start_command();
    start["token"] = json!(secrets[0]);
    rig.send(&start);
    rig.ready().await;
    // A refused command that holds one of them is not repeated either.
    rig.send(&ManagedRig::command(
        "set_mute",
        2,
        json!({"muted": secrets[3]}),
    ));
    assert_eq!(rig.ack(2).await.error, Some(VoiceControlErrorCode::Invalid));
    rig.send(&ManagedRig::command("leave", 3, json!({})));
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.sdk.tokens().len(), 1, "it joined, with a join token");
    let written = rig.events.text();
    for secret in &secrets {
        assert!(
            !written.contains(secret.as_str()),
            "{secret} on the event pipe:\n{written}"
        );
    }
    for fragment in ["FAKE", "do-not-print", "eyJ"] {
        assert!(!written.contains(fragment), "{fragment}:\n{written}");
    }
}

/// How long after `since` the condition first held, waiting at most `limit`.
async fn within(limit: Duration, since: Instant, met: impl Fn() -> bool) -> Duration {
    while !met() {
        assert!(since.elapsed() < limit, "not within {limit:?}");
        tokio::time::sleep(Duration::from_millis(1)).await;
    }
    since.elapsed()
}

/// Brings a rig to a transmission in progress: joined, the talk key held on the key device,
/// audio reaching the track.
async fn talking(rig: &mut ManagedRig) {
    rig.send_start();
    rig.ready().await;
    rig.press_talk();
    rig.transmitting().await;
    let before = rig.sdk.frames();
    rig.until("audio reaching the track", |rig| {
        rig.sdk.frames() > before + 3
    })
    .await;
}

/// After `since`: no frame reaches the track once the gate's bound has passed.
async fn gate_shut_within_its_bound(rig: &ManagedRig, since: Instant) {
    let bound = Duration::from_millis(conch_voice_api::VOICE_CONTROL_GATE_SHUT_MS);
    tokio::time::sleep_until((since + bound).into()).await;
    let at_bound = rig.sdk.frames();
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(
        rig.sdk.frames(),
        at_bound,
        "frames reached the track after the gate's bound"
    );
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert!(rig.sdk.is_muted());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_owner_going_while_the_talk_key_is_held_stops_everything_within_the_bounds() {
    let mut rig = ManagedRig::start().await;
    talking(&mut rig).await;

    // The key device is still being read, and the key is still held on it.
    let gone = Instant::now();
    rig.close();
    let capture = Duration::from_millis(conch_voice_api::VOICE_CONTROL_CAPTURE_STOP_MS);
    let mic = within(capture, gone, || rig.mic_closed() >= 1).await;
    let speakers = within(capture, gone, || rig.sink_closed() >= 1).await;
    gate_shut_within_its_bound(&rig, gone).await;
    let code = rig.exit_code().await;
    let exited = gone.elapsed();
    assert_eq!(code, 0);
    assert!(
        exited < Duration::from_millis(conch_voice_api::VOICE_CONTROL_OWNER_LOSS_EXIT_MS),
        "ended {exited:?} after the owner went"
    );
    eprintln!("owner lost: microphone closed {mic:?}, speakers {speakers:?}, ended {exited:?}");

    // What the owner was told, had it been there: stopping and stopped, for owner_lost,
    // with the gate shut in both; and conchd was told the press is over.
    let states: Vec<_> = rig
        .read()
        .into_iter()
        .filter_map(|event| match event {
            VoiceControlEvent::State(state) => Some(state),
            _ => None,
        })
        .collect();
    let last_two: Vec<_> = states.iter().rev().take(2).rev().collect();
    assert_eq!(last_two[0].phase, VoiceControlPhase::Stopping);
    assert_eq!(last_two[0].reason, Some(VoiceControlReason::OwnerLost));
    assert!(!last_two[0].transmitting);
    assert_eq!(last_two[1].phase, VoiceControlPhase::Stopped);
    assert_eq!(last_two[1].reason, Some(VoiceControlReason::OwnerLost));
    assert_eq!(last_two[1].capture.as_str(), "off");
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert!(
        rig.sdk
            .control_calls()
            .contains(&support::fake::Call::Close)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn leave_while_the_talk_key_is_held_has_no_release_tail_and_ends_within_its_bound() {
    let mut rig = ManagedRig::start().await;
    talking(&mut rig).await;

    let asked = Instant::now();
    rig.send(&ManagedRig::command("leave", 2, json!({})));
    let capture = Duration::from_millis(conch_voice_api::VOICE_CONTROL_CAPTURE_STOP_MS);
    within(capture, asked, || rig.mic_closed() >= 1).await;
    within(capture, asked, || rig.sink_closed() >= 1).await;
    gate_shut_within_its_bound(&rig, asked).await;
    // Standard input stays open: the helper leaves by itself.
    assert!(rig.is_open());
    let code = rig.exit_code().await;
    assert_eq!(code, 0);
    assert!(asked.elapsed() < Duration::from_millis(conch_voice_api::VOICE_CONTROL_LEAVE_EXIT_MS));
    let ack = rig.ack(2).await;
    assert!(ack.ok);
    let last = rig.state().unwrap();
    assert_eq!(last.phase, VoiceControlPhase::Stopped);
    assert_eq!(last.reason, Some(VoiceControlReason::Left));
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
}

/// Issue #631: under load, the thread handing a frame to the track can be taken away after
/// the gate let the frame through and before the SDK took it, while the session loop
/// leaves and mutes the track. That frame reaches the track before the mute or not at all.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_frame_on_its_way_into_the_track_at_leave_never_follows_the_mute() {
    let mut rig = ManagedRig::start().await;
    talking(&mut rig).await;
    let hold = rig.sdk.hold_next_send();
    // From here until the frame is let go the test waits on its own thread, and not on the
    // runtime: one of the runtime's two threads is held by the frame, and the other may be
    // held by the mute that waits for it.
    let armed = Instant::now();
    while !hold.reached() {
        assert!(
            armed.elapsed() < Duration::from_secs(10),
            "no frame was held"
        );
        std::thread::sleep(Duration::from_millis(1));
    }
    rig.send(&ManagedRig::command("leave", 2, json!({})));
    let asked = Instant::now();
    while !rig.sdk.is_muted() && asked.elapsed() < Duration::from_millis(300) {
        std::thread::sleep(Duration::from_millis(1));
    }
    hold.release();

    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert!(rig.sdk.is_muted());
    let last = rig.state().unwrap();
    assert_eq!(
        (last.phase, last.reason),
        (VoiceControlPhase::Stopped, Some(VoiceControlReason::Left))
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_signal_while_the_talk_key_is_held_leaves_as_leave_does() {
    let mut rig = ManagedRig::start().await;
    talking(&mut rig).await;
    let signalled = Instant::now();
    rig.signal();
    gate_shut_within_its_bound(&rig, signalled).await;
    assert_eq!(rig.exit_code().await, 0);
    let last = rig.state().unwrap();
    assert_eq!(
        (last.phase, last.reason),
        (VoiceControlPhase::Stopped, Some(VoiceControlReason::Signal))
    );
    assert!(rig.mic_closed() >= 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn setting_mute_or_deafen_twice_changes_nothing_the_second_time() {
    let mut rig = ManagedRig::start().await;
    rig.send_start();
    rig.ready().await;
    let states = |rig: &ManagedRig| {
        rig.read()
            .iter()
            .filter(|event| matches!(event, VoiceControlEvent::State(_)))
            .count()
    };
    rig.send(&ManagedRig::command("set_mute", 2, json!({"muted": true})));
    assert!(rig.ack(2).await.ok);
    assert!(rig.state().unwrap().muted);
    let after_one = states(&rig);
    rig.send(&ManagedRig::command("set_mute", 3, json!({"muted": true})));
    assert!(rig.ack(3).await.ok);
    assert_eq!(states(&rig), after_one, "a second set_mute wrote a state");
    // Unmuted already? Then not deafened changes nothing either.
    rig.send(&ManagedRig::command(
        "set_deafen",
        4,
        json!({"deafened": false}),
    ));
    assert!(rig.ack(4).await.ok);
    assert_eq!(states(&rig), after_one);
    // Deafened and undeafened: the mute stays as it was (UX-11).
    rig.send(&ManagedRig::command(
        "set_deafen",
        5,
        json!({"deafened": true}),
    ));
    rig.ack(5).await;
    rig.send(&ManagedRig::command(
        "set_deafen",
        6,
        json!({"deafened": false}),
    ));
    rig.ack(6).await;
    let state = rig.state().unwrap();
    assert!(state.muted && !state.deafened);
    // The mute key toggles the same machine; a set_mute that agrees with it then changes
    // nothing, and one that does not, does.
    rig.strike_mute();
    rig.until("unmuted by the key", |rig| !rig.state().unwrap().muted)
        .await;
    let after_key = states(&rig);
    rig.send(&ManagedRig::command("set_mute", 7, json!({"muted": false})));
    assert!(rig.ack(7).await.ok);
    assert_eq!(states(&rig), after_key);

    // Nothing a command did opened the gate.
    assert_eq!(rig.sdk.frames(), 0);
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn malformed_and_oversized_lines_after_the_start_fail_the_session_and_close_capture() {
    let long = std::fs::read(fixtures().join("lines/command-too-long.txt")).unwrap();
    // The longest line there is: the fixture's get_state, with an id after the start's,
    // still exactly at the limit.
    let at_limit =
        String::from_utf8(std::fs::read(fixtures().join("lines/command-at-limit.txt")).unwrap())
            .unwrap()
            .replacen("\"id\":1,", "\"id\":2,", 1)
            .into_bytes();
    assert_eq!(
        at_limit.len(),
        conch_voice_api::VOICE_CONTROL_MAX_LINE_BYTES
    );
    for (line, code) in [
        (b"{\"schema\":".to_vec(), VoiceControlErrorCode::Malformed),
        (b"down".to_vec(), VoiceControlErrorCode::Malformed),
        (long.clone(), VoiceControlErrorCode::TooLong),
        (vec![b'x'; 64 * 1024], VoiceControlErrorCode::TooLong),
        (
            br#"{"schema":"conch.voice_control.v2","kind":"leave","id":9}"#.to_vec(),
            VoiceControlErrorCode::UnknownSchema,
        ),
        (
            br#"{"schema":"conch.voice_control.v1","kind":"leave"}"#.to_vec(),
            VoiceControlErrorCode::Invalid,
        ),
        (
            br#"{"schema":"conch.voice_control.v1","kind":"get_state","id":1}"#.to_vec(),
            VoiceControlErrorCode::IdNotIncreasing,
        ),
    ] {
        let mut rig = ManagedRig::start().await;
        rig.send_start();
        rig.ready().await;
        // The longest line there is is read.
        rig.line(&at_limit);
        let at_limit_id = parse_command(&at_limit).unwrap().id();
        assert!(rig.ack(at_limit_id).await.ok);
        let sent = Instant::now();
        rig.line(&line);
        assert_eq!(rig.exit_code().await, 1, "{code}");
        let events = rig.read();
        // The protocol_error, and then the failed state and nothing else.
        let at = events
            .iter()
            .position(|event| matches!(event, VoiceControlEvent::ProtocolError(_)))
            .unwrap_or_else(|| panic!("{code}: no protocol_error in {events:?}"));
        assert!(matches!(&events[at], VoiceControlEvent::ProtocolError(e) if e.code == code));
        assert_eq!(at + 2, events.len(), "{code}: {events:?}");
        let last = rig.state().unwrap();
        assert_eq!(
            (last.phase, last.reason),
            (
                VoiceControlPhase::Failed,
                Some(VoiceControlReason::ProtocolError)
            )
        );
        // The helper ends having asked for both to close, and they close a moment later,
        // within the bound that leaving has.
        let capture = Duration::from_millis(conch_voice_api::VOICE_CONTROL_CAPTURE_STOP_MS);
        within(capture, sent, || rig.mic_closed() >= 1).await;
        within(capture, sent, || rig.sink_closed() >= 1).await;
        assert_eq!(rig.mic_closed(), 1, "{code}: capture was not closed");
        assert_eq!(rig.sink_closed(), 1, "{code}: playback was not stopped");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_version_mismatch_or_a_bad_first_command_exits_having_captured_nothing() {
    // The owner reads the hello, speaks no version of it, and closes standard input.
    let mut rig = ManagedRig::start().await;
    rig.until("the hello", |rig| !rig.read().is_empty()).await;
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.lines().len(), 1, "nothing after the hello");

    for first in [
        json!({"schema": "conch.voice_control.v1", "kind": "get_state", "id": 1}),
        json!({"schema": "conch.voice_control.v1", "kind": "press", "id": 1}),
        json!({"schema": "conch.voice_control.v1", "kind": "start", "id": 1}),
        json!("start"),
    ] {
        let mut rig = ManagedRig::start().await;
        rig.send(&first);
        assert_eq!(rig.exit_code().await, 1, "{first}");
        assert_eq!(rig.mic_opened(), 0);
        assert!(
            rig.conchd.requests().is_empty(),
            "{first}: conchd was asked"
        );
        let events = rig.read();
        assert_eq!(events.len(), 3, "hello, protocol_error, failed: {events:?}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_last_line_with_no_line_feed_is_not_acted_on() {
    // A whole start, but for its line feed, and then the end of the pipe: the helper,
    // never started, exits 0 having written nothing but its hello and asked nothing.
    let mut rig = ManagedRig::start().await;
    rig.write_partial(rig.start_command().to_string().as_bytes());
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.lines().len(), 1, "only the hello");
    assert!(rig.conchd.requests().is_empty());
    assert_eq!(rig.mic_opened(), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn commands_while_starting_are_answered_and_the_mute_carries_into_the_session() {
    let conchd = Stub::start().await;
    let held = Arc::new(Notify::new());
    conchd.next_whoami(Reply::Held(Arc::clone(&held), Box::new(whoami(7))));
    let mut rig = ManagedRig::start_with(conchd, ManagedSetup::default(), |_| {}).await;
    rig.send_start();
    assert!(rig.ack(1).await.ok);
    rig.until("whoami asked", |rig| rig.conchd.whoami_requests() == 1)
        .await;
    rig.send(&ManagedRig::command("set_mute", 2, json!({"muted": true})));
    assert!(rig.ack(2).await.ok);
    let starting = rig.state().unwrap();
    assert_eq!(starting.phase, VoiceControlPhase::Starting);
    assert!(starting.muted);
    rig.send(&ManagedRig::command("get_state", 3, json!({})));
    assert!(rig.ack(3).await.ok);
    // Nothing is opened while starting.
    assert_eq!(rig.mic_opened(), 0);
    assert_eq!(rig.conchd.session_requests(), 0);

    held.notify_one();
    rig.joined().await;
    let state = rig.state().unwrap();
    assert!(state.muted, "{state:?}");
    rig.send(&ManagedRig::command("leave", 4, json!({})));
    assert_eq!(rig.exit_code().await, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn leave_or_the_owner_going_while_starting_stops_before_anything_is_opened() {
    for leave in [true, false] {
        let conchd = Stub::start().await;
        let held = Arc::new(Notify::new());
        conchd.next_whoami(Reply::Held(Arc::clone(&held), Box::new(whoami(7))));
        let mut rig = ManagedRig::start_with(conchd, ManagedSetup::default(), |_| {}).await;
        rig.send_start();
        rig.until("whoami asked", |rig| rig.conchd.whoami_requests() == 1)
            .await;
        let reason = if leave {
            rig.send(&ManagedRig::command("leave", 2, json!({})));
            VoiceControlReason::Left
        } else {
            rig.close();
            VoiceControlReason::OwnerLost
        };
        assert_eq!(rig.exit_code().await, 0);
        let states: Vec<_> = rig
            .read()
            .into_iter()
            .filter_map(|event| match event {
                VoiceControlEvent::State(s) => Some((s.phase, s.reason)),
                _ => None,
            })
            .collect();
        assert_eq!(
            states,
            [
                (VoiceControlPhase::Starting, None),
                (VoiceControlPhase::Stopping, Some(reason)),
                (VoiceControlPhase::Stopped, Some(reason)),
            ]
        );
        assert_eq!(rig.mic_opened(), 0);
        assert_eq!(rig.conchd.session_requests(), 0);
        assert!(rig.sdk.calls().is_empty());
        held.notify_one();
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn no_start_in_time_fails_with_start_timeout() {
    let setup = ManagedSetup {
        start_timeout: Duration::from_millis(100),
        ..ManagedSetup::default()
    };
    let mut rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
    assert_eq!(rig.exit_code().await, 1);
    let last = rig.state().unwrap();
    assert_eq!(
        (last.phase, last.reason),
        (
            VoiceControlPhase::Failed,
            Some(VoiceControlReason::StartTimeout)
        )
    );
    assert!(rig.conchd.requests().is_empty());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_login_comes_from_where_the_start_says_and_nowhere_else() {
    // (how the login is kept, credential named, the reason it fails with, if it does)
    let cases = [
        (Login::Stored, "stored", None),
        (Login::Env, "env", None),
        (
            Login::Both,
            "stored",
            Some(VoiceControlReason::BadEnvironment),
        ),
        (Login::Both, "env", None),
        // CONCH_TOKEN is set: not the login the owner meant.
        (
            Login::Env,
            "stored",
            Some(VoiceControlReason::BadEnvironment),
        ),
        (Login::Stored, "env", Some(VoiceControlReason::NotSignedIn)),
        (
            Login::Nowhere,
            "stored",
            Some(VoiceControlReason::NotSignedIn),
        ),
    ];
    for (login, credential, fails) in cases {
        let setup = ManagedSetup {
            login,
            ..ManagedSetup::default()
        };
        let mut rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
        let mut start = rig.start_command();
        start["credential"] = json!(credential);
        rig.send(&start);
        match fails {
            None => {
                rig.ready().await;
                rig.close();
                assert_eq!(rig.exit_code().await, 0, "{login:?} {credential}");
            }
            Some(reason) => {
                assert_eq!(rig.exit_code().await, 1, "{login:?} {credential}");
                let last = rig.state().unwrap();
                assert_eq!(
                    (last.phase, last.reason),
                    (VoiceControlPhase::Failed, Some(reason)),
                    "{login:?} {credential}"
                );
                assert_eq!(rig.conchd.session_requests(), 0);
                assert_eq!(rig.mic_opened(), 0);
            }
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn whoami_decides_before_anything_is_opened() {
    let cases = [
        (whoami(8), VoiceControlReason::IdentityMismatch),
        (
            Reply::Json(
                200,
                json!({"id": 7, "kind": "agent", "name": "bot", "role": "member"}).to_string(),
            ),
            VoiceControlReason::NotAPerson,
        ),
        (
            Reply::Json(
                200,
                json!({"id": 7, "kind": "human", "name": "x", "role": "member", "voice_allowed": false})
                    .to_string(),
            ),
            VoiceControlReason::VoiceNotAllowed,
        ),
        (Reply::error(401, "unauthorized"), VoiceControlReason::NotSignedIn),
        (Reply::Empty(502), VoiceControlReason::ServerUnreachable),
    ];
    for (reply, reason) in cases {
        let conchd = Stub::start().await;
        conchd.next_whoami(reply);
        let mut rig = ManagedRig::start_with(conchd, ManagedSetup::default(), |_| {}).await;
        rig.send_start();
        assert_eq!(rig.exit_code().await, 1, "{reason}");
        let last = rig.state().unwrap();
        assert_eq!(
            (last.phase, last.reason),
            (VoiceControlPhase::Failed, Some(reason))
        );
        assert_eq!(rig.conchd.session_requests(), 0, "{reason}");
        assert_eq!(rig.mic_opened(), 0, "{reason}");
        assert_eq!(rig.sink_closed(), 0, "{reason}: the speakers were opened");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn without_a_key_device_hold_to_talk_is_not_configured_and_the_rest_works() {
    let setup = ManagedSetup {
        voice_toml: None,
        keyboard: false,
        ..ManagedSetup::default()
    };
    let mut rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.send_start();
    rig.until("connected", |rig| {
        rig.state()
            .is_some_and(|s| s.phase == VoiceControlPhase::Connected)
    })
    .await;
    let state = rig.state().unwrap();
    assert_eq!(state.ptt.as_str(), "not_configured");
    assert_eq!(state.capture.as_str(), "on");
    rig.send(&ManagedRig::command(
        "set_deafen",
        2,
        json!({"deafened": true}),
    ));
    assert!(rig.ack(2).await.ok);
    assert!(rig.state().unwrap().deafened);
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_slow_owner_loses_events_sees_the_gap_and_resynchronises() {
    const STRIKES: i64 = 400;
    let mut rig = ManagedRig::start().await;
    rig.send_start();
    rig.ready().await;
    let base = rig.read().last().unwrap().seq();
    let track_muted = rig.sdk.count(Call::Mute);
    // The owner stops reading; meanwhile the mute key is struck far more often than the
    // queue holds states. Each strike is one change, and so one state, and one seq.
    rig.reader.stop(true);
    for _ in 0..STRIKES {
        rig.strike_mute();
    }
    rig.until("the writer waiting on the owner", |rig| {
        rig.reader.is_waited_for()
    })
    .await;
    // The owner reads again only once the helper has taken the strikes, all but perhaps
    // the last: each strike that mutes mutes the track as well. That is more states than
    // the helper holds for an owner who does not read, so some were dropped.
    rig.until("the strikes taken", |rig| {
        rig.sdk.count(Call::Mute) >= track_muted + STRIKES as usize / 2
    })
    .await;
    rig.reader.stop(false);

    // The owner reads first what the helper held for it: the event in its writer's hands
    // and a full queue. Only then does it ask, as `conch` asks only once it has read an
    // event after the gap. (Asked sooner, the answer waits behind the full queue and still
    // comes: the test after this one.)
    rig.until("the events held for the owner", |rig| {
        rig.read().iter().filter(|event| event.seq() > base).count()
            > VOICE_CONTROL_EVENT_QUEUE_LINES
    })
    .await;

    // Then it asks for the state, whose seq shows the gap, until the answer comes after
    // every strike: then it is the whole state, unmuted after an even number.
    let mut id = 2;
    let resynced = loop {
        rig.send(&ManagedRig::command("get_state", id, json!({})));
        let ack = rig.ack(id).await;
        assert!(ack.ok);
        let events = rig.read();
        let at = events
            .iter()
            .position(|event| matches!(event, VoiceControlEvent::Ack(a) if a.id == id))
            .unwrap();
        let state = match &events[at - 1] {
            VoiceControlEvent::State(state) => state.clone(),
            other => panic!("get_state answered without a state just before: {other:?}"),
        };
        if state.seq > base + STRIKES {
            break state;
        }
        id += 1;
        tokio::time::sleep(Duration::from_millis(20)).await;
    };
    assert!(!resynced.muted);
    let seqs: Vec<i64> = rig.read().iter().map(VoiceControlEvent::seq).collect();
    assert!(
        seqs.windows(2).all(|pair| pair[1] > pair[0]),
        "seq repeats or goes back"
    );
    assert!(
        seqs.windows(2).any(|pair| pair[1] > pair[0] + 1),
        "no gap: nothing was dropped"
    );
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// Commands read while the owner is not reading and the queue is full are still answered,
/// each exactly once and in order, once the owner reads again: an ack, and the state that
/// answers `get_state`, are never dropped (§5, rules 8 and 10; #571).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn commands_sent_to_an_owner_that_is_not_reading_are_answered_once_it_reads() {
    // Odd, so that the last strike mutes and the track says when it has been taken.
    const STRIKES: i64 = 401;
    let mut rig = ManagedRig::start().await;
    rig.send_start();
    rig.ready().await;
    let track_muted = rig.sdk.count(Call::Mute);
    // The owner stops reading, and the mute key is struck far more often than the queue
    // holds states: when every strike has been taken, the queue is full.
    rig.reader.stop(true);
    for _ in 0..STRIKES {
        rig.strike_mute();
    }
    rig.until("the writer waiting on the owner", |rig| {
        rig.reader.is_waited_for()
    })
    .await;
    rig.until("every strike taken", |rig| {
        rig.sdk.count(Call::Mute) >= track_muted + (STRIKES as usize).div_ceil(2)
    })
    .await;

    // Still not reading, the owner asks for the state, unmutes, and mutes again. The last
    // mutes the track: by then the session loop has answered the two before it, with the
    // queue as full as the strikes left it.
    rig.send(&ManagedRig::command("get_state", 2, json!({})));
    rig.send(&ManagedRig::command("set_mute", 3, json!({"muted": false})));
    rig.send(&ManagedRig::command("set_mute", 4, json!({"muted": true})));
    rig.until("the last command taken", |rig| {
        rig.sdk.count(Call::Mute) > track_muted + (STRIKES as usize).div_ceil(2)
    })
    .await;
    rig.reader.stop(false);

    rig.ack(4).await;
    let events = rig.read();
    let acks: Vec<(i64, bool)> = events
        .iter()
        .filter_map(|event| match event {
            VoiceControlEvent::Ack(ack) if ack.id > 1 => Some((ack.id, ack.ok)),
            _ => None,
        })
        .collect();
    assert_eq!(
        acks,
        [(2, true), (3, true), (4, true)],
        "each answered once"
    );
    // get_state is answered by a state and then its ack, whatever the queue held: the
    // state as every strike left it, muted after an odd number.
    let at = events
        .iter()
        .position(|event| matches!(event, VoiceControlEvent::Ack(a) if a.id == 2))
        .unwrap();
    let VoiceControlEvent::State(answer) = &events[at - 1] else {
        panic!("get_state answered without a state just before: {events:?}");
    };
    assert_eq!(answer.seq + 1, events[at].seq(), "the state, then the ack");
    assert!(answer.muted, "{answer:?}");
    // Seq only rises, and what was dropped still shows as a gap, before the answers.
    let seqs: Vec<i64> = events.iter().map(VoiceControlEvent::seq).collect();
    assert!(
        seqs.windows(2).all(|pair| pair[1] > pair[0]),
        "seq repeats or goes back: {seqs:?}"
    );
    assert!(
        seqs[..at].windows(2).any(|pair| pair[1] > pair[0] + 1),
        "no gap before the answers: nothing was dropped"
    );
    // And the session goes on: the state after the last command is muted.
    rig.send(&ManagedRig::command("get_state", 5, json!({})));
    assert!(rig.ack(5).await.ok);
    assert!(rig.state().unwrap().muted);
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// The lines that follow the start in one write are read with it; each is answered, in
/// order, however the start and they are taken from the reader.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_start_and_the_commands_written_with_it_are_all_answered_in_order() {
    for _ in 0..20 {
        let mut rig = ManagedRig::start().await;
        let mut written = rig.start_command().to_string();
        for command in [
            ManagedRig::command("set_mute", 2, json!({"muted": true})),
            ManagedRig::command("get_state", 3, json!({})),
            ManagedRig::command("set_deafen", 4, json!({"deafened": true})),
        ] {
            written.push('\n');
            written.push_str(&command.to_string());
        }
        written.push('\n');
        rig.write_partial(written.as_bytes());
        rig.ack(4).await;
        let acks: Vec<(i64, bool)> = rig
            .read()
            .into_iter()
            .filter_map(|event| match event {
                VoiceControlEvent::Ack(ack) => Some((ack.id, ack.ok)),
                _ => None,
            })
            .collect();
        assert_eq!(acks, [(1, true), (2, true), (3, true), (4, true)]);
        rig.joined().await;
        let state = rig.state().unwrap();
        assert!(state.muted && state.deafened, "{state:?}");
        rig.close();
        assert_eq!(rig.exit_code().await, 0);
    }
}

/// A leave, or a line that cannot be acted on, written with the start is not lost: the
/// helper ends as it asks, with a final state, and a command after the leave is answered.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_leave_or_a_bad_line_written_with_the_start_ends_the_helper_as_it_asks() {
    for _ in 0..10 {
        let mut rig = ManagedRig::start().await;
        let written = format!(
            "{}\n{}\n{}\n",
            rig.start_command(),
            ManagedRig::command("leave", 2, json!({})),
            ManagedRig::command("set_mute", 3, json!({"muted": true})),
        );
        rig.write_partial(written.as_bytes());
        assert_eq!(rig.exit_code().await, 0);
        assert!(rig.ack(2).await.ok);
        assert_eq!(
            rig.ack(3).await.error,
            Some(VoiceControlErrorCode::Stopping)
        );
        let last = rig.state().unwrap();
        assert_eq!(
            (last.phase, last.reason),
            (VoiceControlPhase::Stopped, Some(VoiceControlReason::Left))
        );
        assert_eq!(rig.mic_opened(), 0);

        let mut rig = ManagedRig::start().await;
        let written = format!("{}\nleave\n", rig.start_command());
        rig.write_partial(written.as_bytes());
        assert_eq!(rig.exit_code().await, 1);
        let events = rig.read();
        let at = events
            .iter()
            .position(|event| matches!(event, VoiceControlEvent::ProtocolError(_)))
            .unwrap();
        assert_eq!(at + 2, events.len(), "{events:?}");
        let last = rig.state().unwrap();
        assert_eq!(
            (last.phase, last.reason),
            (
                VoiceControlPhase::Failed,
                Some(VoiceControlReason::ProtocolError)
            )
        );
    }
}

/// An invite that withheld voice: a managed owner is told voice_not_allowed, and `join`'s
/// output is what it was before managed mode existed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_withheld_voice_is_named_to_the_owner_and_join_says_what_it_always_said() {
    let conchd = Stub::start().await;
    conchd.next_session(Reply::error(403, "voice_not_allowed"));
    let mut rig = ManagedRig::start_with(conchd, ManagedSetup::default(), |_| {}).await;
    rig.send_start();
    assert_eq!(rig.exit_code().await, 1);
    let last = rig.state().unwrap();
    assert_eq!(
        (last.phase, last.reason),
        (
            VoiceControlPhase::Failed,
            Some(VoiceControlReason::VoiceNotAllowed)
        )
    );

    let conchd = Stub::start().await;
    conchd.next_session(Reply::error(403, "voice_not_allowed"));
    let rig = support::Rig::start_with(conchd, support::Setup::default(), |_| {}).await;
    let (result, ended) = rig.ended().await;
    assert_eq!(
        result.unwrap_err().to_string(),
        "voice is for people: this login is not a person's"
    );
    let written = ended.out.text();
    let stopped = written
        .lines()
        .map(|line| serde_json::from_str::<Value>(line).unwrap())
        .find(|event| event["event"] == "connection" && event["state"] == "stopped")
        .unwrap_or_else(|| panic!("no stop in {written}"));
    assert_eq!(
        stopped,
        json!({
            "event": "connection",
            "state": "stopped",
            "reason": "voice is for people: this login is not a person's",
        })
    );
    assert!(!written.contains("voice_not_allowed"), "{written}");
}

// ------------------------------------------------------------------ the open microphone

/// What the helper logs, from the moment the one test that reads it first asks. The log
/// is the process's, so it holds the other tests' records too; that test looks for its
/// own line only.
fn helper_log() -> &'static Captured {
    use conch_voice::logger::Logger;
    use conch_voice::secrets::Scrubber;
    use log::LevelFilter;
    use std::sync::OnceLock;

    static LOG: OnceLock<Captured> = OnceLock::new();
    LOG.get_or_init(|| {
        let captured = Captured::default();
        let logger = Logger::new(
            LevelFilter::Warn,
            Box::new(captured.clone()),
            Scrubber::new(),
        );
        log::set_logger(Box::leak(Box::new(logger))).unwrap();
        log::set_max_level(LevelFilter::Warn);
        captured
    })
}

/// Log records, as they were written.
#[derive(Clone, Default)]
struct Captured(Arc<std::sync::Mutex<Vec<u8>>>);

impl Captured {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.0.lock().unwrap()).into_owned()
    }
}

impl std::io::Write for Captured {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// A managed helper speaks only `conch.voice_control.v1`, whose state cannot say "open
/// microphone": with `talk_mode = "open"` in `voice.toml` it runs push-to-talk, and says
/// so in one log line (`docs/design/tui-voice.md` §20.6). The talk key transmits, as it
/// would not in open mode.
#[tokio::test]
async fn a_v1_helper_with_open_mode_configured_runs_push_to_talk_and_logs_one_line() {
    use conch_voice::managed::V1_PUSH_TO_TALK;

    let log = helper_log();
    let setup = ManagedSetup {
        voice_toml: Some(format!(
            "{}talk_mode = \"open\"\n",
            support::managed::KEYS_TOML
        )),
        ..ManagedSetup::default()
    };
    let mut rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.send_start();
    rig.ready().await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(rig.sdk.frames(), 0, "nothing transmits by itself");

    // The talk key, which open mode would ignore, transmits.
    rig.press_talk();
    rig.transmitting().await;
    let before = rig.sdk.frames();
    rig.until("audio reaching the track", |rig| {
        rig.sdk.frames() > before + 3
    })
    .await;
    rig.release_talk();
    rig.until("the press over", |rig| {
        rig.state().is_some_and(|state| !state.transmitting)
    })
    .await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);

    let written = log.text();
    assert_eq!(
        written.matches(V1_PUSH_TO_TALK).count(),
        1,
        "one line, once:\n{written}"
    );
}

// ------------------------------------------------------------- conch.voice_control.v2

/// `pkg/schema/testdata/voice-control-v2`, read in place.
fn fixtures_v2() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../pkg/schema/testdata/voice-control-v2")
}

fn fixture_v2(name: &str) -> Value {
    let path = fixtures_v2().join(name);
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap()
}

/// `open_hang_ms` in the v2 tests' `voice.toml`, the least there is, in frames: a hang of
/// quiet is waited for as frames read, so a test does not wait longer than it must.
const HANG_FRAMES: u64 = 10;

/// What the v2 tests' `voice.toml` adds to the keys of [`KEYS_TOML`]: the shortest hang.
const QUICK_HANG: &str = "open_hang_ms = 100\n";

/// A rig with microphones and speakers opened by name ([`World`]), loud from the start,
/// with `extra` added to the `[audio]` of the usual `voice.toml`.
async fn world_rig(extra: &str, arrange: impl FnOnce(&World)) -> (ManagedRig, World) {
    let world = World::default();
    arrange(&world);
    let setup = ManagedSetup {
        voice_toml: Some(format!("{KEYS_TOML}{QUICK_HANG}{extra}")),
        world: Some(world.clone()),
        ..ManagedSetup::default()
    };
    let rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
    (rig, world)
}

/// Every line the helper wrote, read by the reader of the owner's version (the hello by
/// v1's), as the JSON it is. A line that reader refuses fails the test: Go's readers, and
/// their `Validate`, are the specification of the owner's reading.
fn helper_values(rig: &ManagedRig, version: Option<Version>) -> Vec<Value> {
    rig.lines()
        .iter()
        .enumerate()
        .map(|(at, line)| {
            let read = match (at, version) {
                (0, _) | (_, None | Some(Version::V1)) => parse_event(line.as_bytes()).map(|_| ()),
                (_, Some(Version::V2)) => parse_event_v2(line.as_bytes()).map(|_| ()),
            };
            read.unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}"));
            serde_json::from_str(line).unwrap()
        })
        .collect()
}

/// One step of what happened in a v2 session, in the order the owner saw it.
#[derive(Debug, Clone)]
enum Step {
    Owner(String),
    Close,
    Signal,
    Helper(Value),
}

/// What a transcript's helper step needs done to the world first: what its state shows
/// happened there.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Act {
    /// The talk key released, if held, and pressed: a fresh press.
    Press,
    Release,
    /// Quiet for longer than the hang, then sound: an onset.
    Onset,
    Quiet,
    /// This device is there now.
    Plug(&'static str),
    /// The keyboard is unplugged, and plugged in again.
    KeyboardGone,
    KeyboardBack,
}

async fn act(rig: &mut ManagedRig, world: Option<&World>, act: Act) {
    let world = || world.expect("the transcript needs a world");
    match act {
        Act::Press => {
            rig.ready_v2().await;
            rig.release_talk();
            rig.press_talk();
        }
        Act::Release => rig.release_talk(),
        Act::Onset => {
            world().loud(false);
            world().frames_pass(HANG_FRAMES + 10).await;
            world().loud(true);
        }
        Act::Quiet => world().loud(false),
        Act::Plug(name) => world().plug(name),
        Act::KeyboardGone => rig.unplug_keyboard(),
        Act::KeyboardBack => rig.plug_keyboard(),
    }
}

/// The block reasons of a state, but `no_microphone`, which a microphone switch may pass
/// through (below).
fn blocks_but_the_microphone(state: &Value) -> Vec<Value> {
    state["blocked"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|block| *block != "no_microphone")
        .cloned()
        .collect()
}

/// Whether the helper's event is the one a transcript's step shows: by kind, phase and
/// reason, and for a state by every field the step has, with one allowance: the design
/// leaves it to the helper whether capture passes through `unavailable` (with
/// `no_microphone`) while a switch moves it to another microphone (§20.5), and this helper
/// does until the new one has given a frame. While a switch the owner asked for is moving
/// it (`moving`), a step that shows `capture: on` therefore also matches `unavailable`; one
/// that shows `unavailable` matches only that, and outside a switch capture is exact.
fn matches_v2(event: &Value, want: &Value, moving: bool) -> bool {
    if event["kind"] != want["kind"] {
        return false;
    }
    let same = |field: &str| event.get(field) == want.get(field);
    match want["kind"].as_str().unwrap() {
        "hello" => true,
        "ack" => same("id") && same("ok") && same("error"),
        "protocol_error" => same("code"),
        "state" => {
            let shown = ["playback", "talk_mode"]
                .into_iter()
                .all(|field| want.get(field).is_none() || same(field));
            let capture = same("capture")
                || (moving && want["capture"] == "on" && event["capture"] == "unavailable");
            shown
                && capture
                && [
                    "phase",
                    "reason",
                    "transmitting",
                    "muted",
                    "deafened",
                    "ptt",
                    "ptt_problem",
                ]
                .into_iter()
                .all(same)
                && blocks_but_the_microphone(event) == blocks_but_the_microphone(want)
        }
        other => panic!("a step of kind {other}"),
    }
}

/// What tells two states apart for the driver: a state the helper writes between two steps
/// must repeat these of the last one matched (it may differ in capture, while a switch moves
/// it, in `retry_in_ms` and the like), so that a transmission a transcript does not show,
/// or a change of phase or mode, fails the run.
fn state_key(event: &Value) -> Option<[Value; 4]> {
    (event["kind"] == "state").then(|| {
        [
            event["phase"].clone(),
            event.get("reason").cloned().unwrap_or(Value::Null),
            event["transmitting"].clone(),
            event.get("talk_mode").cloned().unwrap_or(Value::Null),
        ]
    })
}

/// Plays a v2 transcript's owner side against `rig`, doing to the world before each helper
/// step what `acts` says for that step's seq, and waits for each helper step in what the
/// helper writes, read by the owner's reader. Returns the log and the exit code.
async fn drive_v2(
    rig: &mut ManagedRig,
    world: Option<&World>,
    name: &str,
    acts: &[(i64, Act)],
) -> (Vec<Step>, i32) {
    let doc = fixture_v2(name);
    let steps = doc["steps"].as_array().unwrap().clone();
    let mut log = Vec::new();
    let mut version = None;
    let mut cursor = 0;
    let mut last_state = None;
    // From a switch of microphone until a state shows it capturing.
    let mut moving = false;
    for step in &steps {
        if step["from"] == "owner" {
            if step["close"] == "stdin" {
                rig.close();
                log.push(Step::Close);
            } else if step["signal"].is_string() {
                rig.signal();
                log.push(Step::Signal);
            } else {
                let mut message = step["message"].clone();
                if message["kind"] == "start" {
                    message["server"] = json!(rig.conchd.url());
                    if version.is_none() {
                        version = Some(match message["schema"].as_str() {
                            Some("conch.voice_control.v2") => Version::V2,
                            _ => Version::V1,
                        });
                    }
                }
                if message["kind"] == "set_devices" && message.get("input").is_some() {
                    moving = true;
                }
                let line = message.to_string();
                rig.line(line.as_bytes());
                log.push(Step::Owner(line));
            }
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
                    act(rig, world, *what).await;
                }
            }
        }
        let deadline = Instant::now() + Duration::from_secs(20);
        let found = loop {
            let events = helper_values(rig, version);
            let mut found = None;
            for (k, event) in events.iter().enumerate().skip(cursor) {
                if matches_v2(event, want, moving) {
                    found = Some((k, events.clone()));
                    break;
                }
                assert!(
                    state_key(event).is_some() && state_key(event) == last_state,
                    "{name}: wanted {want}, the helper wrote {event}\n{}",
                    rig.events.text()
                );
            }
            if let Some(found) = found {
                break found;
            }
            assert!(
                Instant::now() < deadline,
                "{name}: timed out waiting for {want}\n{}",
                rig.events.text()
            );
            tokio::time::sleep(Duration::from_millis(2)).await;
        };
        let (at, events) = found;
        for event in &events[cursor..=at] {
            log.push(Step::Helper(event.clone()));
        }
        if let Some(key) = state_key(&events[at]) {
            last_state = Some(key);
        }
        if events[at]["capture"] == "on" {
            moving = false;
        }
        cursor = at + 1;
    }
    panic!("{name} has no exit step");
}

/// One of the owner's lines, as the helper reads it: with the reader of the version the
/// session speaks, or for the first line, the one the line names.
struct Read {
    kind: Option<VoiceControlCommandKind>,
    id: Option<i64>,
    refusal: Option<VoiceControlErrorCode>,
}

fn read_owner_line(line: &str, version: Option<Version>) -> Read {
    let v1 = |line: &str| match parse_command(line.as_bytes()) {
        Ok(command) => Read {
            kind: Some(command.kind()),
            id: Some(command.id()),
            refusal: None,
        },
        Err(refusal) => Read {
            kind: None,
            id: refusal.id,
            refusal: Some(refusal.code),
        },
    };
    let v2 = |line: &str| match parse_command_v2(line.as_bytes()) {
        Ok(command) => Read {
            kind: Some(command.kind()),
            id: Some(command.id()),
            refusal: None,
        },
        Err(refusal) => Read {
            kind: None,
            id: refusal.id,
            refusal: Some(refusal.code),
        },
    };
    match version {
        Some(Version::V1) => v1(line),
        Some(Version::V2) => v2(line),
        None => match v2(line) {
            Read {
                refusal: Some(VoiceControlErrorCode::UnknownSchema),
                ..
            } => v1(line),
            read => read,
        },
    }
}

/// A command with a usable id that has not been answered, the answer it must get when that
/// is already known, its line, and whether the latest state transmitted when it was read.
struct PendingV2 {
    id: i64,
    kind: Option<VoiceControlCommandKind>,
    refusal: Option<VoiceControlErrorCode>,
    line: Value,
    was_transmitting: bool,
}

/// The rules of §5 and v2's own, checked against what really happened: the port of the Go
/// reader's `checkVoiceControlV2Transcript`. The hello offers both versions; the start fixes
/// the version; seq may skip (events dropped for a slow owner) but never repeats or goes
/// back; a state a command changed comes before its ack, a microphone switch or a mode
/// switch during a transmission ends it before the ack, and the mode set is the one the
/// state says.
fn check_rules_v2(log: &[Step], exit: i32) {
    use VoiceControlCommandKind as Kind;
    let mut seq = 0;
    let mut last_id = 0;
    let mut version: Option<Version> = None;
    let mut hello = false;
    let (mut started, mut ended) = (false, false);
    let mut final_phase: Option<String> = None;
    let mut state: Option<Value> = None;
    let mut previous_was_state = false;
    let mut pending: VecDeque<PendingV2> = VecDeque::new();
    let mut want_protocol_error = None;
    let mut want_failed_after = false;
    let shown = || format!("{log:#?}");
    for (i, step) in log.iter().enumerate() {
        match step {
            Step::Close | Step::Signal => {}
            Step::Owner(line) => {
                assert!(hello, "step {i}: the owner wrote before the hello");
                let read = read_owner_line(line, version);
                let value: Value = serde_json::from_str(line).unwrap_or(Value::Null);
                let is_start = read.kind == Some(Kind::Start);
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
                let refusal = match (read.refusal, read.kind) {
                    (Some(code), _) => Some(code),
                    (None, Some(Kind::Start)) if started => {
                        Some(VoiceControlErrorCode::AlreadyStarted)
                    }
                    (None, Some(Kind::Start)) => {
                        started = true;
                        version = Some(if value["schema"] == "conch.voice_control.v2" {
                            Version::V2
                        } else {
                            Version::V1
                        });
                        None
                    }
                    (
                        None,
                        Some(
                            Kind::SetMute | Kind::SetDeafen | Kind::SetDevices | Kind::SetTalkMode,
                        ),
                    ) if stopping => Some(VoiceControlErrorCode::Stopping),
                    _ => None,
                };
                pending.push_back(PendingV2 {
                    id,
                    kind: read.kind,
                    refusal,
                    line: value,
                    was_transmitting: state.as_ref().is_some_and(|s| s["transmitting"] == true),
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
                        ["conch.voice_control.v1", "conch.voice_control.v2"],
                        "step {i}: the hello offers both versions"
                    );
                    hello = true;
                    previous_was_state = false;
                    continue;
                }
                if want_failed_after {
                    assert!(
                        event["kind"] == "state"
                            && event["phase"] == "failed"
                            && event["reason"] == "protocol_error",
                        "step {i}: a protocol_error not followed by failed with reason \
                         protocol_error\n{}",
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
                        want_protocol_error = None;
                        want_failed_after = true;
                    }
                    "state" => {
                        assert_eq!(
                            want_protocol_error, None,
                            "step {i}: a state where a protocol_error was due"
                        );
                        assert!(
                            started || want_failed_after,
                            "step {i}: a state before a start was accepted"
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
                            panic!(
                                "step {i}: an ack for {} that nothing asked for",
                                event["id"]
                            )
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
                            "step {i}: {event} refused for no reason\n{}",
                            shown()
                        );
                        let latest = state
                            .as_ref()
                            .unwrap_or_else(|| panic!("step {i}: acknowledged before any state"));
                        let line = &next.line;
                        match next.kind {
                            Some(Kind::SetMute) => {
                                assert_eq!(latest["muted"], line["muted"], "step {i}: set_mute");
                            }
                            Some(Kind::SetDeafen) => {
                                assert_eq!(
                                    latest["deafened"], line["deafened"],
                                    "step {i}: set_deafen"
                                );
                            }
                            Some(Kind::Leave) => assert!(
                                ["stopping", "stopped", "failed"]
                                    .contains(&latest["phase"].as_str().unwrap()),
                                "step {i}: leave acknowledged in {}",
                                latest["phase"]
                            ),
                            Some(Kind::GetState) => assert!(
                                was_state,
                                "step {i}: get_state acknowledged without a state before it"
                            ),
                            Some(Kind::SetDevices) => assert!(
                                line.get("input").is_none()
                                    || !next.was_transmitting
                                    || latest["transmitting"] == false,
                                "step {i}: the microphone was switched during a transmission \
                                 and the latest state still transmits"
                            ),
                            Some(Kind::SetTalkMode) => {
                                assert_eq!(
                                    latest["talk_mode"], line["talk_mode"],
                                    "step {i}: set_talk_mode"
                                );
                                assert!(
                                    !next.was_transmitting || latest["transmitting"] == false,
                                    "step {i}: the talk mode was switched during a \
                                     transmission and the latest state still transmits"
                                );
                            }
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

/// The v2 transcripts, with the `voice.toml` each runs with beyond the keys of
/// [`KEYS_TOML`] (`None` for no key device, and the line given instead), the devices that
/// are not there when it starts, and what is done to the world before each of the helper's
/// steps, by the step's seq.
struct Transcript {
    name: &'static str,
    toml: Option<&'static str>,
    no_keys: Option<&'static str>,
    unplugged: &'static [&'static str],
    acts: &'static [(i64, Act)],
}

const USB_MIC: &str = "alsa_input.usb-Generic_USB_Audio-00.mono-fallback";
const USB_SPEAKERS: &str = "alsa_output.usb-Generic_USB_Audio-00.analog-stereo";

const TRANSCRIPTS_V2: [Transcript; 9] = [
    Transcript {
        name: "transcript-negotiation-v2-owner.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[],
    },
    // A v1 owner. (With open mode in voice.toml a v1 session is push-to-talk all the same,
    // and says so in its log: the test of that counts the line in this process's log, and
    // so is the only one here that makes it.)
    Transcript {
        name: "transcript-negotiation-v1-owner.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[],
    },
    Transcript {
        name: "transcript-version-fixed.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[],
    },
    Transcript {
        name: "transcript-refused-commands.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[],
    },
    Transcript {
        name: "transcript-microphone-switch-while-transmitting.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[(6, Act::Press), (10, Act::Press), (11, Act::Release)],
    },
    Transcript {
        name: "transcript-device-missing-after-switch.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[USB_MIC, USB_SPEAKERS],
        acts: &[(8, Act::Plug(USB_MIC)), (11, Act::Plug(USB_SPEAKERS))],
    },
    // No key device, and open mode from voice.toml; one opening may last a second.
    Transcript {
        name: "transcript-open-mode.json",
        toml: None,
        no_keys: Some(
            "[audio]\nrelease_tail_ms = 20\ntalk_mode = \"open\"\nopen_hang_ms = 100\nmax_transmit_secs = 1\n",
        ),
        unplugged: &[],
        acts: &[
            (6, Act::Onset),
            (7, Act::Quiet),
            (8, Act::Onset),
            (13, Act::Onset),
            (15, Act::Quiet),
            (16, Act::Onset),
        ],
    },
    Transcript {
        name: "transcript-mode-switch-while-transmitting.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[
            (6, Act::Press),
            (9, Act::Onset),
            (10, Act::KeyboardGone),
            (11, Act::Quiet),
            (14, Act::KeyboardBack),
            (15, Act::Press),
        ],
    },
    Transcript {
        name: "transcript-open-mode-entered-loud.json",
        toml: Some(""),
        no_keys: None,
        unplugged: &[],
        acts: &[(10, Act::Onset), (15, Act::Onset), (16, Act::Quiet)],
    },
];

/// Every v2 transcript, its owner side played against the real helper with test devices,
/// gets the helper side it shows, and what the helper wrote keeps §5's rules and v2's.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn each_v2_transcript_played_by_its_owner_gets_the_helper_side_it_shows() {
    for transcript in &TRANSCRIPTS_V2 {
        let name = transcript.name;
        let world = World::default();
        for device in transcript.unplugged {
            world.unplug(device);
        }
        let setup = ManagedSetup {
            voice_toml: Some(match (transcript.toml, transcript.no_keys) {
                (Some(extra), _) => format!("{KEYS_TOML}{QUICK_HANG}{extra}"),
                (None, Some(alone)) => alone.to_owned(),
                (None, None) => unreachable!("{name}"),
            }),
            keyboard: transcript.no_keys.is_none(),
            world: Some(world.clone()),
            ..ManagedSetup::default()
        };
        let mut rig = ManagedRig::start_with(Stub::start().await, setup, |sdk| {
            if name == "transcript-refused-commands.json" {
                // So that the commands sent after leave meet a helper still leaving.
                sdk.close_takes(Duration::from_millis(300));
            }
        })
        .await;
        let (log, exit) = drive_v2(&mut rig, Some(&world), name, transcript.acts).await;
        check_rules_v2(&log, exit);
        assert_eq!(rig.sdk.frames_while_muted(), 0, "{name}");
        // Nothing but what was named was opened.
        for opened in world.mic_names().into_iter().chain(world.sink_names()) {
            assert!(
                opened.is_none()
                    || [USB_MIC, USB_SPEAKERS, "speech_out"].contains(&opened.as_deref().unwrap()),
                "{name}: {opened:?} was opened"
            );
        }
        eprintln!("{name}: {} steps", log.len());
    }
}

#[test]
fn every_v2_transcript_is_driven_here() {
    let driven: Vec<&str> = TRANSCRIPTS_V2.iter().map(|t| t.name).collect();
    for entry in std::fs::read_dir(fixtures_v2()).unwrap() {
        let name = entry.unwrap().file_name().into_string().unwrap();
        if name.starts_with("transcript-") {
            assert!(driven.contains(&name.as_str()), "{name} is not run");
        }
    }
}

/// The hello is v1's frozen shape, offering exactly v1 and v2, whichever the owner then
/// chooses; the start fixes the version, and every event after it is in it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_hello_offers_both_and_the_start_chooses_the_version_of_every_event_after_it() {
    for v2 in [false, true] {
        let mut rig = ManagedRig::start().await;
        rig.until("the hello", |rig| !rig.lines().is_empty()).await;
        let hello: Value = serde_json::from_str(&rig.lines()[0]).unwrap();
        let mut golden = fixture_v2("event-hello-v1-and-v2.json");
        golden["helper_version"] = hello["helper_version"].clone();
        assert_eq!(hello, golden, "the frozen shape, offering both");
        if v2 {
            rig.send_start_v2();
        } else {
            rig.send_start();
        }
        rig.until("connected", |rig| {
            rig.lines()
                .iter()
                .any(|line| line.contains("\"connected\""))
        })
        .await;
        rig.send(&if v2 {
            ManagedRig::command_v2("leave", 2, json!({}))
        } else {
            ManagedRig::command("leave", 2, json!({}))
        });
        assert_eq!(rig.exit_code().await, 0);
        let schema = if v2 {
            "conch.voice_control.v2"
        } else {
            "conch.voice_control.v1"
        };
        let lines = rig.lines();
        for line in &lines[1..] {
            let event: Value = serde_json::from_str(line).unwrap();
            assert_eq!(event["schema"], schema, "{line}");
            assert_eq!(
                event.get("talk_mode").is_some(),
                v2 && event["kind"] == "state",
                "{line}"
            );
        }
        if v2 {
            // Read by v2's reader, whose Validate is Go's.
            rig.read_v2();
        } else {
            rig.read();
        }
    }
}

/// A v1 session is today's: a line of v2 in it is unknown_schema, which ends it, and a v2
/// command written in v1 is a kind v1 does not know, refused and nothing more.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn in_a_v1_session_a_v2_command_is_unknown_and_a_v2_line_ends_it() {
    let mut rig = ManagedRig::start().await;
    rig.send_start();
    rig.ready().await;
    for (id, kind, fields) in [
        (2, "set_devices", json!({"input": "speech_out"})),
        (3, "set_talk_mode", json!({"talk_mode": "open"})),
    ] {
        rig.send(&ManagedRig::command(kind, id, fields));
        assert_eq!(
            rig.ack(id).await.error,
            Some(VoiceControlErrorCode::UnknownKind),
            "{kind}"
        );
    }
    assert_eq!(rig.mic_opened(), 1, "no microphone was switched to");
    rig.send(&ManagedRig::command_v2(
        "set_devices",
        4,
        json!({"input": "speech_out"}),
    ));
    assert_eq!(rig.exit_code().await, 1);
    let events = rig.read();
    let at = events
        .iter()
        .position(|event| matches!(event, VoiceControlEvent::ProtocolError(_)))
        .unwrap();
    assert!(
        matches!(&events[at], VoiceControlEvent::ProtocolError(e) if e.code == VoiceControlErrorCode::UnknownSchema)
    );
    assert_eq!(at + 2, events.len(), "{events:?}");
    let last = rig.state().unwrap();
    assert_eq!(
        (last.phase, last.reason),
        (
            VoiceControlPhase::Failed,
            Some(VoiceControlReason::ProtocolError)
        )
    );
}

/// The first samples of the frames that reached the track, from `from` on: which
/// microphone each came from ([`level_of`]).
fn sent_levels(rig: &ManagedRig, from: u64) -> Vec<f32> {
    rig.sdk
        .first_samples()
        .split_off(usize::try_from(from).unwrap())
}

/// `set_devices` during a transmission: it ends with one `stopped`; no frame of either
/// microphone reaches the track after the switch began; the state before the ack has
/// stopped transmitting; the next states show the new microphone's capture; and only a
/// fresh press sends again, from the new microphone.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_microphone_switch_during_a_transmission_ends_it_and_a_fresh_press_is_needed() {
    let (mut rig, world) = world_rig("", |_| {}).await;
    rig.send_start_v2();
    rig.ready_v2().await;
    rig.press_talk();
    rig.transmitting_v2(true).await;
    let before = rig.sdk.frames();
    rig.until("audio reaching the track", |rig| {
        rig.sdk.frames() > before + 3
    })
    .await;

    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"input": "speech_out"}),
    ));
    let ack = rig.ack_v2(2).await;
    assert!(ack.ok);
    // The state before the ack says the transmission is over.
    let states = rig.states_v2();
    let before_ack = states.iter().rfind(|state| state.seq < ack.seq).unwrap();
    assert!(!before_ack.transmitting, "{before_ack:?}");
    let at_ack = rig.sdk.frames();
    // The new microphone is opened and captures; the key is still held.
    rig.until_v2("the new microphone capturing", |state| {
        state.capture.as_str() == "on" && state.blocked.is_empty()
    })
    .await;
    world.frames_pass(20).await;
    assert_eq!(rig.sdk.frames(), at_ack, "nothing after the switch");
    assert!(
        sent_levels(&rig, 0)
            .iter()
            .all(|level| *level == level_of(None)),
        "nothing of the new microphone was sent"
    );
    assert_eq!(rig.conchd.reported(), ["started", "stopped"], "one stopped");
    assert!(rig.sdk.is_muted());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_eq!(world.mic_names(), [None, Some("speech_out".to_owned())]);
    assert_eq!(rig.mic_closed(), 1, "the old stream is closed");

    // A fresh press sends again, from the new microphone only.
    rig.release_talk();
    rig.press_talk();
    rig.transmitting_v2(true).await;
    rig.until("audio from the new microphone", |rig| {
        rig.sdk.frames() > at_ack + 3
    })
    .await;
    assert!(
        sent_levels(&rig, at_ack)
            .iter()
            .all(|level| *level == level_of(Some("speech_out")))
    );
    rig.release_talk();
    rig.transmitting_v2(false).await;
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// A device that is not there after `set_devices` is acknowledged `ok`: the state before
/// the ack says `capture: unavailable` with `no_microphone`, or `playback: unavailable`,
/// nothing is captured or played from another device meanwhile, and a press sends nothing.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_absent_device_after_set_devices_is_unavailable_and_none_other_is_used() {
    let (mut rig, world) = world_rig("", |world| {
        world.unplug(USB_MIC);
        world.unplug(USB_SPEAKERS);
    })
    .await;
    rig.send_start_v2();
    rig.ready_v2().await;

    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"input": USB_MIC, "output": USB_SPEAKERS}),
    ));
    let ack = rig.ack_v2(2).await;
    assert!(ack.ok);
    let states = rig.states_v2();
    let before_ack = states.iter().rfind(|state| state.seq < ack.seq).unwrap();
    assert_eq!(before_ack.capture.as_str(), "unavailable");
    assert!(before_ack.blocks(conch_voice_api::VoiceControlBlock::NoMicrophone));
    assert_eq!(before_ack.playback.as_str(), "unavailable");

    // A press does nothing: there is no microphone, and no other is used. (No microphone
    // gives frames now, so the wait is a time.)
    rig.press_talk();
    tokio::time::sleep(Duration::from_millis(200)).await;
    rig.release_talk();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let state = rig.state_v2().unwrap();
    assert_eq!(
        (state.capture.as_str(), state.playback.as_str()),
        ("unavailable", "unavailable")
    );
    assert_eq!(rig.sdk.frames(), 0);
    assert!(rig.conchd.reported().is_empty());
    assert_eq!(world.mic_names(), [None, Some(USB_MIC.to_owned())]);
    assert_eq!(world.sink_names(), [None, Some(USB_SPEAKERS.to_owned())]);

    // They appear: capture and playback resume, on them.
    world.plug(USB_MIC);
    world.plug(USB_SPEAKERS);
    rig.until_v2("capture and playback back", |state| {
        state.capture.as_str() == "on" && state.playback.as_str() == "on"
    })
    .await;
    assert_eq!(world.mic_names().len(), 2, "nothing else was opened");
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// Waits for `quiet` frames of silence, longer than the hang, and then makes the
/// microphones loud: an onset.
async fn onset(world: &World) {
    world.loud(false);
    world.frames_pass(HANG_FRAMES + 10).await;
    world.loud(true);
}

/// `set_talk_mode open`, then sound: a `transmitting` state with `talk_mode: open`. Mute
/// sends nothing, and after the unmute nothing is sent until a fresh onset.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn set_talk_mode_open_then_sound_transmits_and_muting_sends_nothing() {
    let (mut rig, world) = world_rig("", |_| {}).await;
    rig.send_start_v2();
    rig.ready_v2().await;
    assert_eq!(rig.state_v2().unwrap().talk_mode.as_str(), "push_to_talk");

    rig.send(&ManagedRig::command_v2(
        "set_talk_mode",
        2,
        json!({"talk_mode": "open"}),
    ));
    assert!(rig.ack_v2(2).await.ok);
    assert_eq!(rig.state_v2().unwrap().talk_mode.as_str(), "open");
    // Loud since before the command: no onset, nothing sent.
    world.frames_pass(HANG_FRAMES * 3).await;
    assert_eq!(rig.sdk.frames(), 0, "the command opened nothing");

    onset(&world).await;
    rig.until_v2("transmitting in open mode", |state| {
        state.transmitting && state.talk_mode.as_str() == "open"
    })
    .await;
    rig.until("audio reaching the track", |rig| rig.sdk.frames() > 3)
        .await;

    rig.send(&ManagedRig::command_v2(
        "set_mute",
        3,
        json!({"muted": true}),
    ));
    assert!(rig.ack_v2(3).await.ok);
    assert!(!rig.state_v2().unwrap().transmitting);
    let at_mute = rig.sdk.frames();
    world.frames_pass(20).await;
    assert_eq!(rig.sdk.frames(), at_mute, "muting sends nothing");
    rig.send(&ManagedRig::command_v2(
        "set_mute",
        4,
        json!({"muted": false}),
    ));
    assert!(rig.ack_v2(4).await.ok);
    // Still loud: the rest of the utterance is not sent.
    world.frames_pass(HANG_FRAMES * 3).await;
    assert_eq!(
        rig.sdk.frames(),
        at_mute,
        "unmuting during sound sends nothing"
    );
    onset(&world).await;
    rig.transmitting_v2(true).await;
    rig.until("audio again", |rig| rig.sdk.frames() > at_mute + 3)
        .await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started"],
        "an opening each"
    );
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// Issue #654 left the session's mute on a mode change without a session-level test: a
/// change of talk mode in the middle of a transmission, either way, sends no frame after
/// it, and the state before the ack already says `transmitting: false`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_mode_change_mid_transmission_sends_nothing_after_it_and_says_so_at_once() {
    let (mut rig, world) = world_rig("", |_| {}).await;
    rig.send_start_v2();
    rig.ready_v2().await;
    let mut id = 1;
    for (from_press, to) in [(true, "open"), (false, "push_to_talk")] {
        if from_press {
            rig.press_talk();
        } else {
            onset(&world).await;
        }
        rig.transmitting_v2(true).await;
        let before = rig.sdk.frames();
        rig.until("audio reaching the track", |rig| {
            rig.sdk.frames() > before + 3
        })
        .await;
        id += 1;
        rig.send(&ManagedRig::command_v2(
            "set_talk_mode",
            id,
            json!({"talk_mode": to}),
        ));
        let ack = rig.ack_v2(id).await;
        assert!(ack.ok);
        let states = rig.states_v2();
        let before_ack = states.iter().rfind(|state| state.seq < ack.seq).unwrap();
        assert!(!before_ack.transmitting, "{to}: {before_ack:?}");
        assert_eq!(before_ack.talk_mode.as_str(), to);
        let at_ack = rig.sdk.frames();
        // Loud, and in push-to-talk the key still held: nothing.
        world.frames_pass(HANG_FRAMES * 3).await;
        assert_eq!(
            rig.sdk.frames(),
            at_ack,
            "{to}: a frame after the mode change"
        );
        assert!(rig.sdk.is_muted(), "{to}");
        rig.release_talk();
    }
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    rig.until("both stopped reported", |rig| {
        rig.conchd.reported().len() == 4
    })
    .await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// In open mode, while the microphone hears sound and the gate is open: the owner going,
/// leave, and a signal each shut the gate within `VoiceControlGateShutMS`, close capture
/// and playback within `VoiceControlCaptureStopMS`, and end the helper within its bound.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn in_open_mode_while_sound_plays_the_owner_going_leave_and_a_signal_stop_within_the_bounds()
{
    for how in ["owner lost", "leave", "signal"] {
        let (mut rig, world) = world_rig("talk_mode = \"open\"\n", |_| {}).await;
        rig.send_start_v2();
        rig.ready_v2().await;
        assert_eq!(rig.state_v2().unwrap().talk_mode.as_str(), "open");
        onset(&world).await;
        rig.transmitting_v2(true).await;
        rig.until("audio reaching the track", |rig| rig.sdk.frames() > 3)
            .await;

        let since = Instant::now();
        let (reason, bound) = match how {
            "owner lost" => {
                rig.close();
                (
                    VoiceControlReason::OwnerLost,
                    VOICE_CONTROL_OWNER_LOSS_EXIT_MS,
                )
            }
            "leave" => {
                rig.send(&ManagedRig::command_v2("leave", 2, json!({})));
                (VoiceControlReason::Left, VOICE_CONTROL_LEAVE_EXIT_MS)
            }
            _ => {
                rig.signal();
                (VoiceControlReason::Signal, VOICE_CONTROL_LEAVE_EXIT_MS)
            }
        };
        let capture = Duration::from_millis(conch_voice_api::VOICE_CONTROL_CAPTURE_STOP_MS);
        within(capture, since, || rig.mic_closed() >= 1).await;
        within(capture, since, || rig.sink_closed() >= 1).await;
        gate_shut_within_its_bound(&rig, since).await;
        assert_eq!(rig.exit_code().await, 0, "{how}");
        let took = since.elapsed();
        assert!(
            took < Duration::from_millis(bound),
            "{how}: ended {took:?} after"
        );
        eprintln!("open mode, {how}: ended {took:?} after");
        let last = rig.state_v2().unwrap();
        assert_eq!(
            (last.phase, last.reason, last.playback.as_str()),
            (VoiceControlPhase::Stopped, Some(reason), "off"),
            "{how}"
        );
        assert_eq!(rig.conchd.reported(), ["started", "stopped"], "{how}");
    }
}

/// `set_devices` and `set_talk_mode` sent while the helper is still starting are kept and
/// used once the session runs: the ack has no state before it, the starting state still says
/// push-to-talk, the microphone opened is the one named, and the speakers opened are only
/// the ones named, never those `voice.toml` names.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn devices_and_the_mode_set_while_starting_are_used_once_the_session_runs() {
    let world = World::default();
    let setup = ManagedSetup {
        voice_toml: Some(format!(
            "{KEYS_TOML}{QUICK_HANG}input = \"configured_mic\"\noutput = \"configured_speakers\"\n"
        )),
        world: Some(world.clone()),
        ..ManagedSetup::default()
    };
    let conchd = Stub::start().await;
    let held = Arc::new(Notify::new());
    conchd.next_whoami(Reply::Held(Arc::clone(&held), Box::new(whoami(7))));
    let mut rig = ManagedRig::start_with(conchd, setup, |_| {}).await;
    rig.send_start_v2();
    assert!(rig.ack_v2(1).await.ok);
    rig.until("whoami asked", |rig| rig.conchd.whoami_requests() == 1)
        .await;
    let states_before = rig.states_v2().len();
    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"input": "speech_out", "output": "headset"}),
    ));
    rig.send(&ManagedRig::command_v2(
        "set_talk_mode",
        3,
        json!({"talk_mode": "open"}),
    ));
    assert!(rig.ack_v2(2).await.ok);
    assert!(rig.ack_v2(3).await.ok);
    assert_eq!(
        rig.states_v2().len(),
        states_before,
        "no state before the acks"
    );
    assert_eq!(rig.state_v2().unwrap().talk_mode.as_str(), "push_to_talk");
    assert!(world.sink_names().is_empty() && world.mic_names().is_empty());

    held.notify_one();
    rig.until_v2("connected in open mode", |state| {
        state.phase.as_str() == "connected" && state.talk_mode.as_str() == "open"
    })
    .await;
    rig.ready_v2().await;
    assert_eq!(world.mic_names(), [Some("speech_out".to_owned())]);
    assert_eq!(world.sink_names(), [Some("headset".to_owned())]);
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// Speakers named after the start-up has opened the ones `voice.toml` names, but before the
/// session runs, are switched to as the session starts: the configured ones are closed, and
/// the speakers playing are the ones named.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn speakers_named_after_the_start_up_opened_others_are_switched_to_when_the_session_runs() {
    let world = World::default();
    let setup = ManagedSetup {
        voice_toml: Some(format!("{KEYS_TOML}output = \"configured_speakers\"\n")),
        world: Some(world.clone()),
        // The start-up waits for the key device, after it has opened the speakers.
        keyboard_unplugged: true,
        ..ManagedSetup::default()
    };
    let mut rig = ManagedRig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.send_start_v2();
    rig.until("the configured speakers opened", |_| {
        world.sink_names() == [Some("configured_speakers".to_owned())]
    })
    .await;
    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"output": "headset"}),
    ));
    assert!(rig.ack_v2(2).await.ok);
    rig.plug_keyboard();
    rig.ready_v2().await;
    assert_eq!(
        world.sink_names(),
        [
            Some("configured_speakers".to_owned()),
            Some("headset".to_owned())
        ]
    );
    assert_eq!(rig.sink_closed(), 1, "the configured speakers were closed");
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
}

/// No secret of v2's `sensitive.json` reaches the event pipe of a v2 session: not a token
/// in a field the start does not declare, not one used as a device name, and not one in a
/// refused command.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn no_secret_reaches_the_event_pipe_in_v2() {
    let secrets: Vec<String> = fixture_v2("sensitive.json")["fake_secrets"]
        .as_array()
        .unwrap()
        .iter()
        .map(|secret| secret.as_str().unwrap().to_owned())
        .collect();
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_LOGIN));
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_JOIN));
    assert!(secrets.iter().any(|s| s == support::stub::FAKE_ROOM));

    let (mut rig, _world) = world_rig("", |_| {}).await;
    let mut start = rig.start_command_v2();
    start["token"] = json!(secrets[0]);
    rig.send(&start);
    rig.ready_v2().await;
    // A device name is the owner's own text and never repeated: here, one that is a secret.
    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"input": secrets[1], "output": secrets[2]}),
    ));
    assert!(rig.ack_v2(2).await.ok);
    rig.send(&ManagedRig::command_v2(
        "set_talk_mode",
        3,
        json!({"talk_mode": secrets[3]}),
    ));
    assert_eq!(
        rig.ack_v2(3).await.error,
        Some(VoiceControlErrorCode::Invalid)
    );
    rig.send(&ManagedRig::command_v2("leave", 4, json!({})));
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    let written = rig.events.text();
    for secret in &secrets {
        assert!(!written.contains(secret.as_str()), "{secret}:\n{written}");
    }
    for fragment in ["FAKE", "do-not-print", "eyJ"] {
        assert!(!written.contains(fragment), "{fragment}:\n{written}");
    }
}

/// Issue #656's security review: a device name the v2 reader accepts that PipeWire's library
/// would read as another (white space around it, or nothing but white space, which it takes
/// for the default) opens nothing. The side is unavailable, no other device is used in its
/// place, and the opener is never called with it. Only the exact `default` is the default.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_padded_device_name_opens_nothing_and_only_default_is_the_default() {
    let (mut rig, world) = world_rig("", |_| {}).await;
    rig.send_start_v2();
    rig.ready_v2().await;
    assert_eq!(
        (world.mic_names(), world.sink_names()),
        (vec![None], vec![None])
    );
    let mut id = 1;
    for (input, output) in [
        (" ", "  default "),
        ("  default ", " "),
        ("   ", "default "),
    ] {
        id += 1;
        rig.send(&ManagedRig::command_v2(
            "set_devices",
            id,
            json!({"input": input, "output": output}),
        ));
        let ack = rig.ack_v2(id).await;
        assert!(ack.ok, "{input:?} {output:?}");
        let states = rig.states_v2();
        let before_ack = states.iter().rfind(|state| state.seq < ack.seq).unwrap();
        assert_eq!(
            (
                before_ack.capture.as_str(),
                before_ack.playback.as_str(),
                before_ack.blocked.clone()
            ),
            (
                "unavailable",
                "unavailable",
                vec![conch_voice_api::VoiceControlBlock::NoMicrophone]
            ),
            "{input:?} {output:?}"
        );
        // The same names again change nothing.
        let states = rig.states_v2().len();
        id += 1;
        rig.send(&ManagedRig::command_v2(
            "set_devices",
            id,
            json!({"input": input, "output": output}),
        ));
        assert!(rig.ack_v2(id).await.ok);
        assert_eq!(rig.states_v2().len(), states, "{input:?} {output:?}");
        // A press sends nothing: there is no microphone.
        rig.press_talk();
        tokio::time::sleep(Duration::from_millis(150)).await;
        rig.release_talk();
        assert_eq!(rig.sdk.frames(), 0, "{input:?} {output:?}");
        assert_eq!(
            rig.state_v2().unwrap().capture.as_str(),
            "unavailable",
            "{input:?}"
        );

        // Exactly `default` is the default: opened, and capturing and playing again.
        id += 1;
        rig.send(&ManagedRig::command_v2(
            "set_devices",
            id,
            json!({"input": "default", "output": "default"}),
        ));
        assert!(rig.ack_v2(id).await.ok);
        rig.until_v2("the default devices back", |state| {
            state.capture.as_str() == "on" && state.playback.as_str() == "on"
        })
        .await;
    }
    // Nothing but the default was ever opened, once at the start and once after each case.
    assert_eq!(world.mic_names(), vec![None; 4]);
    assert_eq!(world.sink_names(), vec![None; 4]);
    assert!(rig.conchd.reported().is_empty());
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    assert_eq!(rig.sdk.frames(), 0);
}

/// Issue #656's security review: a device that cannot be opened is said in this program's
/// own words; the opener's error, which may name the device, is not logged (§20.7).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_device_that_cannot_be_opened_is_logged_without_its_name() {
    let log = helper_log();
    let (mut rig, _world) = world_rig("", |world| {
        world.break_device("broken_mic_FAKE_name");
        world.break_device("broken_speakers_FAKE_name");
    })
    .await;
    rig.send_start_v2();
    rig.ready_v2().await;
    rig.send(&ManagedRig::command_v2(
        "set_devices",
        2,
        json!({"input": "broken_mic_FAKE_name", "output": "broken_speakers_FAKE_name"}),
    ));
    let ack = rig.ack_v2(2).await;
    assert!(ack.ok);
    let state = rig.state_v2().unwrap();
    assert_eq!(
        (state.capture.as_str(), state.playback.as_str()),
        ("unavailable", "unavailable")
    );
    rig.close();
    assert_eq!(rig.exit_code().await, 0);
    let written = log.text();
    assert!(
        written.contains(conch_voice::session::SPEAKERS_NOT_OPENED),
        "{written}"
    );
    assert!(!written.contains("_FAKE_name"), "{written}");
    assert!(!rig.events.text().contains("_FAKE_name"));
}
