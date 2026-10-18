//! `conch-voice managed` (`docs/design/tui-voice.md` §13), run in-process between a fake
//! SDK and a stub `conchd`, driven by the owner's side of the shared transcripts in
//! `pkg/schema/testdata/voice-control/`, with what the helper writes checked against the
//! rules of §5 as the Go reader checks the transcripts themselves; and the bounds of §3 as
//! numbers: owner loss with the key device being read, while transmitting, shuts the gate
//! within 100 ms, closes capture within 500 ms and ends within 3 s.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit: the microphone is a
//! tone, the speakers count, the keyboard is a named pipe, and LiveKit is the fake.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

use conch_voice::sdk::DisconnectReason;
use conch_voice_api::{
    VOICE_CONTROL_EVENT_QUEUE_LINES, VoiceControlCommand, VoiceControlErrorCode, VoiceControlEvent,
    VoiceControlHelloV1, VoiceControlPhase, VoiceControlReason, VoiceControlStateV1, parse_command,
};
use serde_json::{Value, json};
use tokio::sync::Notify;

use support::fake::Call;
use support::managed::{Login, ManagedRig, ManagedSetup};
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
