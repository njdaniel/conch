//! The transmit gate and the published track, as the session loop keeps them in step
//! (`docs/design/conch-voice.md` §3, §13 row 12). The SDK is a fake that records every call
//! and every frame it is handed; the microphone is a tone that never stops, so a frame
//! that does not arrive was stopped by the gate.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::sdk::SdkEvent;
use conch_voice::session::Input;
use conch_voice_control::LineCommand::{Deafen, Down, Mute, Quit, Up};
use serde_json::json;

use support::fake::Call;
use support::stub::Stub;
use support::{Rig, Setup, several_frames};

/// Checks the order of everything the client did to the fake: a frame is handed over only
/// between an unmute and the mute that follows it.
fn assert_frames_only_while_unmuted(calls: &[Call]) {
    let mut unmuted = false;
    for (at, call) in calls.iter().enumerate() {
        match call {
            Call::Unmute => unmuted = true,
            Call::Mute | Call::Publish | Call::Close => unmuted = false,
            Call::Frame => assert!(unmuted, "a frame at call {at} outside a press: {calls:?}"),
            Call::Connect | Call::Disable => {}
        }
    }
}

#[tokio::test]
async fn the_track_is_published_then_muted_and_only_then_is_the_client_connected() {
    let rig = Rig::start().await;
    rig.connected(1).await;
    assert_eq!(
        rig.sdk.control_calls(),
        [Call::Connect, Call::Publish, Call::Mute],
        "publish, then mute, in that order"
    );
    assert!(rig.sdk.is_muted() && !rig.sdk.is_enabled());
    rig.ready().await;
    assert_eq!(rig.mic_opened(), 1, "the microphone is opened once");

    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "nothing is sent before a press");
}

/// The microphone stream is open while joined and closed on leaving (issue #184): a real
/// microphone closes its stream when it is dropped, so what is checked is the drop.
#[tokio::test]
async fn the_microphone_is_open_while_joined_and_closed_on_leaving() {
    let rig = Rig::start().await;
    rig.connected(1).await;
    rig.ready().await;
    several_frames().await;
    assert_eq!(
        (rig.mic_opened(), rig.mic_closed()),
        (1, 0),
        "open while joined"
    );

    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    result.unwrap();
    // The close is the transmit task's, a moment behind the session's last words.
    for _ in 0..200 {
        if ended.mic_closed() == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(ended.mic_closed(), 1, "closed on leaving, and once");
}

#[tokio::test]
async fn a_press_between_the_publish_and_the_mute_is_not_connected_and_sends_nothing() {
    let mut hold = None;
    let rig = Rig::start_with(Stub::start().await, Setup::default(), |sdk| {
        hold = Some(sdk.hold_publish());
    })
    .await;
    // The track is published, enabled and not yet muted.
    rig.until("the publish", |rig| rig.sdk.count(Call::Publish) == 1)
        .await;
    assert_eq!(rig.sdk.count(Call::Mute), 0);
    assert!(rig.sdk.is_enabled());

    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        !rig.events_named("press_ignored").is_empty()
    })
    .await;
    assert_eq!(
        rig.events_named("press_ignored")[0]["reason"],
        "not_connected"
    );
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "the press opened nothing");
    assert!(
        !rig.connection_states().contains(&"connected".to_owned()),
        "not connected until the track is muted"
    );

    hold.unwrap().notify_one();
    rig.connected(1).await;
    rig.ready().await;
    assert_eq!(
        rig.sdk.control_calls(),
        [Call::Connect, Call::Publish, Call::Mute]
    );
    // The key is still held, and the press that was refused does not come to life.
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0);
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());
}

#[tokio::test]
async fn frames_reach_the_track_only_between_a_press_and_the_gates_shut() {
    let rig = Rig::start().await;
    rig.ready().await;
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0);

    rig.line(Down);
    rig.frames_beyond(5).await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    rig.line(Up);
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "nothing after the gate shut");
    assert_eq!(
        rig.sdk.loud_frames(),
        sent,
        "what was sent was the microphone"
    );

    // A second press, to see the order hold across presses.
    rig.line(Down);
    rig.frames_beyond(sent + 3).await;
    rig.line(Up);
    rig.not_talking().await;

    let calls = rig.sdk.calls();
    assert_frames_only_while_unmuted(&calls);
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_eq!(
        rig.sdk.control_calls(),
        [
            Call::Connect,
            Call::Publish,
            Call::Mute,
            Call::Unmute,
            Call::Mute,
            Call::Unmute,
            Call::Mute
        ],
        "a press unmutes and the gate's shut mutes"
    );
    assert_eq!(calls.last(), Some(&Call::Mute));
}

#[tokio::test]
async fn the_release_tail_is_sent_and_then_the_track_is_muted() {
    let setup = Setup {
        release_tail_ms: 100,
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(2).await;
    let at_release = rig.sdk.frames();
    let mutes = rig.sdk.count(Call::Mute);
    rig.line(Up);
    rig.not_talking().await;
    let tail = rig.sdk.frames() - at_release;
    // Ten frames of tail; one more may have been in hand when the release arrived.
    assert!((9..=11).contains(&tail), "a 100 ms tail, got {tail} frames");
    assert_eq!(rig.sdk.count(Call::Mute), mutes + 1);
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

#[tokio::test]
async fn no_frame_is_sent_in_any_state_where_the_gate_must_be_shut() {
    let rig = Rig::start().await;
    rig.ready().await;
    assert_eq!(rig.mic_opened(), 1, "the tone is running from here on");

    // Muted.
    rig.line(Mute);
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        rig.events_named("press_ignored").len() == 1
    })
    .await;
    assert_eq!(rig.events_named("press_ignored")[0]["reason"], "muted");
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "muted");
    rig.line(Up);
    rig.line(Mute);
    rig.ready().await;

    // Deafened, which also blocks transmitting.
    rig.line(Deafen);
    rig.until("deafened", |rig| rig.own()["deafened"] == true)
        .await;
    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        rig.events_named("press_ignored").len() == 2
    })
    .await;
    assert_eq!(rig.events_named("press_ignored")[1]["reason"], "deafened");
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "deafened");
    rig.line(Up);
    rig.line(Deafen);
    rig.ready().await;

    // The SDK is reconnecting.
    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    rig.until("not connected", |rig| {
        rig.own()["blocked"] == json!(["not_connected"])
    })
    .await;
    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        rig.events_named("press_ignored").len() == 3
    })
    .await;
    assert_eq!(
        rig.events_named("press_ignored")[2]["reason"],
        "not_connected"
    );
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "reconnecting");
    rig.line(Up);
    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    rig.ready().await;

    // After `up`: covered by the press itself.
    rig.line(Down);
    rig.frames_beyond(3).await;
    rig.line(Up);
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "after up");

    // Muting during a press ends it at once, with no release tail.
    rig.line(Down);
    rig.frames_beyond(sent + 3).await;
    let stops = rig.stopped();
    rig.line(Mute);
    rig.until("muted", |rig| {
        rig.own()["muted"] == true && rig.own()["transmitting"] == false
    })
    .await;
    rig.stopped_beyond(stops).await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "muted during a press");

    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn a_press_longer_than_the_transmit_limit_is_cut_off_with_the_key_still_down() {
    let setup = Setup {
        max_transmit: Duration::from_millis(100),
        release_tail_ms: 0,
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.until("the limit", |rig| {
        rig.own()["blocked"] == json!(["max_transmit"]) && rig.own()["transmitting"] == false
    })
    .await;
    // The status says not transmitting before the gate's word (#293): count after it.
    rig.reported(2).await;
    let sent = rig.sdk.frames();
    // The gate's own count allows ten frames of 10 ms and not one more, whichever of the
    // gate and the machine's timer acted first.
    assert!(sent <= 10, "100 ms is ten frames, got {sent}");
    several_frames().await;
    assert_eq!(
        rig.sdk.frames(),
        sent,
        "the key is still down and nothing is sent"
    );
    rig.reported(2).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);

    // Only a new press transmits again.
    rig.line(Up);
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(sent).await;
    rig.input(Input::Line(Up));
}

#[tokio::test]
async fn with_a_listen_only_grant_nothing_is_published_and_the_microphone_is_never_opened() {
    let conchd = Stub::start().await;
    conchd.can_publish(false);
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.connected(1).await;
    rig.until("the own state", |rig| {
        rig.own()["blocked"] == json!(["no_publish_grant", "no_microphone"])
    })
    .await;

    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        !rig.events_named("press_ignored").is_empty()
    })
    .await;
    assert_eq!(
        rig.events_named("press_ignored")[0]["reason"],
        "no_publish_grant",
        "a press does nothing and says why"
    );
    several_frames().await;

    assert_eq!(rig.sdk.control_calls(), [Call::Connect], "no track");
    assert_eq!(rig.mic_opened(), 0, "the microphone source was never made");
    assert_eq!(rig.sdk.frames(), 0);
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());
}

#[tokio::test]
async fn in_plain_mode_a_refused_press_says_why_in_words() {
    let conchd = Stub::start().await;
    conchd.can_publish(false);
    let setup = Setup {
        json: false,
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    support::until(
        "connected",
        || rig.out.text(),
        || rig.out.text().lines().any(|line| line == "connected"),
    )
    .await;
    rig.line(Down);
    let refusal = "press ignored: listening only: this session may not transmit";
    support::until(
        "the refusal",
        || rig.out.text(),
        || rig.out.text().lines().any(|line| line == refusal),
    )
    .await;
}

/// The SDK republishes inside its own reconnect. If it ever did so while a press was in
/// progress, the track would be disabled under a client that says it is talking: the press
/// is ended and reported instead, and does not resume though the key is still down.
#[tokio::test]
async fn a_republish_during_a_press_ends_the_press() {
    let rig = Rig::start().await;
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(3).await;

    rig.sdk.republish();
    rig.not_talking().await;
    assert!(rig.sdk.is_muted() && !rig.sdk.is_enabled());
    rig.reported(2).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "the held key does not resume");
    assert_eq!(rig.sdk.frames_while_muted(), 0);

    // Released and pressed again, it transmits.
    rig.line(Up);
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(sent).await;
    assert!(!rig.sdk.is_muted() && rig.sdk.is_enabled());
    rig.line(Up);
    rig.not_talking().await;
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn a_republished_track_is_muted_and_disabled_before_the_client_is_connected_again() {
    let rig = Rig::start().await;
    rig.ready().await;

    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    rig.until("reconnecting", |rig| {
        rig.connection_states().last().unwrap() == "reconnecting"
    })
    .await;
    let before = rig.sdk.control_calls().len();

    // The SDK's full reconnect: the track is published again, enabled though muted.
    rig.sdk.republish();
    assert!(rig.sdk.is_muted() && rig.sdk.is_enabled());
    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    rig.connected(2).await;

    assert_eq!(
        rig.sdk.control_calls()[before..],
        [Call::Mute, Call::Disable],
        "muted and disabled, and nothing else, by the time it is connected again"
    );
    assert!(rig.sdk.is_muted() && !rig.sdk.is_enabled());
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0);

    // And a press still works afterwards: the unmute enables the track again.
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(3).await;
    assert!(!rig.sdk.is_muted() && rig.sdk.is_enabled());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    rig.line(Up);
    rig.not_talking().await;
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

#[tokio::test]
async fn reconnecting_forces_the_gate_shut_and_a_held_key_does_not_resume() {
    let rig = Rig::start().await;
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(3).await;

    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    rig.until("the gate forced shut", |rig| {
        rig.own()["blocked"] == json!(["not_connected"]) && rig.own()["transmitting"] == false
    })
    .await;
    assert_eq!(rig.connection_states().last().unwrap(), "reconnecting");
    assert!(rig.sdk.is_muted());
    // The status says not transmitting before the gate's word (#293): count after it.
    rig.reported(2).await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "nothing is sent while reconnecting");
    rig.reported(2).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped"],
        "the transmission ended when the connection dropped"
    );

    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    rig.ready().await;
    several_frames().await;
    assert_eq!(
        rig.sdk.frames(),
        sent,
        "the key is still held and nothing resumes by itself"
    );

    rig.line(Up);
    rig.line(Down);
    rig.frames_beyond(sent).await;
    rig.line(Up);
    rig.not_talking().await;
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

/// The transmit task alone, with no push-to-talk machine and so no timer of any kind: the
/// gate's own count of frames is what ends the transmission.
#[tokio::test]
async fn the_gates_own_cap_ends_a_transmission_when_no_timer_fires() {
    use conch_voice::sdk::{RoomHandle, Transport};
    use conch_voice::secrets::Scrubber;
    use conch_voice::transmit::{self, BoxedMic, TxCommand, TxEvent};
    use conch_voice_api::Secret;
    use conch_voice_audio::ToneSource;
    use conch_voice_control::GateCommand;
    use support::fake::{FakeFeed, FakeSdk};

    let sdk = FakeSdk::new(Scrubber::new());
    let (room, _events) = sdk
        .connect("ws://livekit.invalid", &Secret::new("FAKE-join-token"))
        .await
        .unwrap();
    let (_track, feed) = room.publish_microphone().await.unwrap();

    // max_transmit of 70 ms: seven frames.
    let config = transmit::gate_config(0, Duration::from_millis(70));
    let mut transmitter = transmit::spawn::<FakeFeed>(config).unwrap();
    // As the session loop does once the track is unmuted: frames may reach it.
    transmitter.track_lock.reopen();
    let mic = Box::new(ToneSource::new(440.0, 0.25).unwrap()) as BoxedMic;
    for command in [
        TxCommand::Microphone(mic),
        TxCommand::Attach(feed),
        TxCommand::Gate(GateCommand::Open),
    ] {
        assert!(transmitter.commands.send(command).is_ok());
    }

    let event = tokio::time::timeout(Duration::from_secs(5), transmitter.events.recv())
        .await
        .expect("the gate shuts itself")
        .unwrap();
    assert_eq!(
        event,
        TxEvent::GateShut {
            limit_reached: true
        }
    );
    assert_eq!(sdk.frames(), 7, "exactly the frames the limit allows");
    several_frames().await;
    assert_eq!(
        sdk.frames(),
        7,
        "and none after, though nothing told the gate to shut"
    );
    assert_eq!(
        transmitter
            .frames_sent
            .load(std::sync::atomic::Ordering::Relaxed),
        7
    );
}

// ------------------------------------------------------------------ the open microphone
//
// `talk_mode = "open"` (`docs/design/tui-voice.md` §20.6), with a scripted microphone read
// every 10 ms, the default level (-40 dBFS) and hang (400 ms, forty frames), and the rig's
// release tail of 20 ms (two frames).

use conch_voice_control::{Key, KeyAction, KeyEvent, TalkMode};
use support::MakeMic;
use support::script::{After, Script, Switch, frame_number};

/// The hang, in frames.
const HANG: u64 = 40;
/// The release tail, in frames.
const TAIL: u64 = 2;
/// Frames the gate may forward past the hang and tail while the session tells it quiet has
/// come: the detector's word goes to the session loop and the shut back to the transmit
/// task, between two frames of 10 ms.
const SLACK: u64 = 2;

fn open_mode(microphone: MakeMic) -> Setup {
    Setup {
        talk_mode: TalkMode::Open,
        microphone: Some(microphone),
        ..Setup::default()
    }
}

/// The numbers of the scripted frames the track was handed, in order.
fn sent(rig: &Rig) -> Vec<u64> {
    rig.sdk
        .first_samples()
        .into_iter()
        .map(frame_number)
        .collect()
}

/// Waits for `pairs` transmissions to be over and reported.
async fn transmissions(rig: &Rig, pairs: usize) {
    rig.until("the transmissions reported", |rig| {
        rig.conchd.reported().len() >= 2 * pairs && rig.own()["transmitting"] == false
    })
    .await;
}

/// Issue #654's first criterion: a microphone of 1 s silence, 1 s tone, 1 s silence and
/// 0.5 s tone is two transmissions. Frames reach the track only from each tone's onset to
/// its end, the hang and the release tail; the onset frame itself is not sent.
#[tokio::test]
async fn in_open_mode_two_utterances_are_two_transmissions_sent_from_onset_to_hang_and_tail() {
    let (mic, played) = Script::make(
        &[(100, false), (100, true), (100, false), (50, true)],
        After::Quiet,
    );
    let rig = Rig::start_with(Stub::start().await, open_mode(mic), |_| {}).await;
    rig.connected(1).await;
    transmissions(&rig, 2).await;
    several_frames().await;
    assert!(played.frames.load(std::sync::atomic::Ordering::SeqCst) > 350 + HANG);

    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    let sent = sent(&rig);
    // (onset, end): the frames of each tone.
    let tones = [(100, 200), (300, 350)];
    for &(onset, end) in &tones {
        let last = end - 1 + HANG + TAIL;
        let of_this: Vec<u64> = sent
            .iter()
            .copied()
            .filter(|n| (onset..=last + SLACK).contains(n))
            .collect();
        assert!(
            !of_this.contains(&onset),
            "the onset frame was sent: {sent:?}"
        );
        // The tone to its end, and the hang and tail after it, reached the track.
        for n in [end - 1, end - 1 + HANG, last] {
            assert!(of_this.contains(&n), "frame {n} is missing: {sent:?}");
        }
    }
    for n in &sent {
        let inside = tones
            .iter()
            .any(|&(onset, end)| *n > onset && *n <= end - 1 + HANG + TAIL + SLACK);
        assert!(inside, "frame {n} outside every transmission: {sent:?}");
    }
    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_eq!(
        rig.sdk.control_calls(),
        [
            Call::Connect,
            Call::Publish,
            Call::Mute,
            Call::Unmute,
            Call::Mute,
            Call::Unmute,
            Call::Mute
        ]
    );
}

/// Muted or deafened, nothing reaches the track in open mode, however loud the microphone.
/// Unmuting during sound sends nothing until quiet and a new onset: the rest of what was
/// being said is not sent.
#[tokio::test]
async fn in_open_mode_nothing_is_sent_muted_or_deafened_and_unmuting_needs_a_fresh_onset() {
    let (mic, loud) = Switch::make();
    let rig = Rig::start_with(Stub::start().await, open_mode(mic), |_| {}).await;
    rig.ready().await;
    let set = |on: bool| loud.set(on);
    // Sixty frames: longer than the hang of forty, counted as the detector counts.
    let longer_than_the_hang = || loud.frames_pass(HANG + 20);
    // The detector hears its first quiet.
    longer_than_the_hang().await;

    rig.line(Mute);
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    set(true);
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), 0, "muted");
    assert!(rig.events_named("press_ignored").is_empty());

    // Unmuted while the sound goes on: nothing, for as long as it goes on.
    rig.line(Mute);
    rig.ready().await;
    longer_than_the_hang().await;
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), 0, "unmuted during sound");
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());

    // Quiet, then sound: that transmits.
    set(false);
    longer_than_the_hang().await;
    set(true);
    rig.talking().await;
    rig.frames_beyond(3).await;

    // Deafened during the sound: the transmission ends at once, and nothing follows it.
    let stops = rig.stopped();
    rig.line(Deafen);
    rig.until("deafened", |rig| {
        rig.own()["deafened"] == true && rig.own()["transmitting"] == false
    })
    .await;
    rig.stopped_beyond(stops).await;
    let sent = rig.sdk.frames();
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), sent, "deafened");
    // Undeafened during the same sound: still nothing.
    rig.line(Deafen);
    rig.ready().await;
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), sent, "undeafened during sound");

    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
}

/// With `max_transmit_secs = 1` a 3 s tone is cut at 1 s; the limit stands, and the gate does
/// not open again until quiet and a new onset.
#[tokio::test]
async fn in_open_mode_the_limit_cuts_a_long_tone_and_stands_until_quiet_and_a_new_onset() {
    let (mic, _) = Script::make(
        &[(50, false), (300, true), (60, false), (30, true)],
        After::Quiet,
    );
    let setup = Setup {
        max_transmit: Duration::from_secs(1),
        ..open_mode(mic)
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.connected(1).await;
    rig.until("the limit", |rig| {
        rig.own()["blocked"] == json!(["max_transmit"]) && rig.own()["transmitting"] == false
    })
    .await;
    transmissions(&rig, 2).await;
    several_frames().await;

    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    let sent = sent(&rig);
    let (first, second): (Vec<u64>, Vec<u64>) = sent.iter().partition(|n| **n < 410);
    // One second of the tone at most, and none of the rest of it.
    assert!(first.len() <= 100, "{} frames in the first", first.len());
    assert!(first.iter().all(|n| *n > 50), "{sent:?}");
    let (lo, hi) = (first[0], first[first.len() - 1]);
    assert!(hi - lo < 100, "the first opening ran from {lo} to {hi}");
    assert!(hi < 349, "the tone was not cut: {sent:?}");
    // The second opening: from the new onset to its hang and tail.
    assert!(
        !second.is_empty() && second.iter().all(|n| *n > 410),
        "{sent:?}"
    );
    assert!(
        second.iter().all(|n| *n <= 439 + HANG + TAIL + SLACK),
        "{sent:?}"
    );
    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// A microphone that trickles one quiet frame every 200 ms after an utterance (the review's
/// S2): counted in frames, quiet would come only after twenty of them, 4 s. The gate shuts
/// within the hang and the tail of the last loud frame, by the clock.
#[tokio::test]
async fn in_open_mode_a_trickling_microphone_shuts_the_gate_within_hang_and_tail() {
    let (mic, played) = Script::make(
        &[(50, false), (60, true)],
        After::Trickle(Duration::from_millis(200)),
    );
    let rig = Rig::start_with(Stub::start().await, open_mode(mic), |_| {}).await;
    rig.talking().await;
    rig.until("the last loud frame", |_| {
        played.frames.load(std::sync::atomic::Ordering::SeqCst) > 110
    })
    .await;
    rig.not_talking().await;
    let last_loud = played.last_loud_at.lock().unwrap().unwrap();
    let after = last_loud.elapsed();
    assert!(
        after >= Duration::from_millis(380),
        "shut before the hang: {after:?}"
    );
    assert!(
        after <= Duration::from_millis(400 + 20 + 300),
        "shut {after:?} after the last loud frame"
    );
    transmissions(&rig, 1).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert!(rig.sdk.is_muted());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// A microphone that stops delivering frames during an opening shuts the gate within the
/// hang and the tail: time with no frame is quiet, and the tail, counted in frames, is cut.
#[tokio::test]
async fn in_open_mode_a_microphone_that_stops_delivering_shuts_the_gate_within_hang_and_tail() {
    let (mic, played) = Script::make(&[(50, false), (60, true)], After::Stop);
    let rig = Rig::start_with(Stub::start().await, open_mode(mic), |_| {}).await;
    rig.talking().await;
    rig.until("the script's end", |_| {
        played.frames.load(std::sync::atomic::Ordering::SeqCst) == 110
    })
    .await;
    rig.not_talking().await;
    let last = played.last_at.lock().unwrap().unwrap();
    let after = last.elapsed();
    // The hang is 400 ms and the tail 20; the rest is the loop and this test looking.
    assert!(
        after >= Duration::from_millis(380),
        "shut before the hang: {after:?}"
    );
    assert!(
        after <= Duration::from_millis(400 + 20 + 300),
        "shut {after:?} after the last frame"
    );
    transmissions(&rig, 1).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert!(sent(&rig).iter().all(|n| *n > 50 && *n < 110));
    assert!(rig.sdk.is_muted());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// In open mode the talk key and `down`/`up` open nothing and are not answered; a lost key
/// device blocks nothing; the mute and deafen keys work.
#[tokio::test]
async fn in_open_mode_the_talk_key_opens_nothing_a_lost_key_device_blocks_nothing_and_mute_works() {
    use conch_voice::keydev::{DeviceState, Problem};

    let (mic, loud) = Switch::make();
    let setup = Setup {
        key_device: Some("the-test-keyboard".to_owned()),
        ..open_mode(mic)
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.input(Input::KeyDevice(DeviceState::Ready));
    rig.ready().await;
    // The detector hears its first quiet.
    loud.frames_pass(HANG + 20).await;
    let talk = |action| {
        Input::Key(KeyEvent {
            key: Key::Talk,
            action,
        })
    };
    for input in [
        Input::Line(Down),
        Input::Line(Up),
        talk(KeyAction::Press),
        talk(KeyAction::Release),
        Input::Line(Down),
    ] {
        rig.input(input);
    }
    several_frames().await;
    assert_eq!(rig.sdk.frames(), 0, "the talk key opened something");
    assert!(rig.events_named("press_ignored").is_empty());
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());

    // The keyboard goes away: nothing is blocked, and sound transmits.
    rig.input(Input::KeyDevice(DeviceState::Lost(Problem::Ended)));
    rig.until("the loss shown", |rig| {
        !rig.events_named("key_device").is_empty()
            && rig.events_named("key_device").last().unwrap()["state"] == "missing"
    })
    .await;
    assert_eq!(rig.own()["blocked"], json!([]));
    loud.set(true);
    rig.talking().await;
    rig.frames_beyond(3).await;
    assert!(
        rig.events_named("self").iter().all(|own| !own["blocked"]
            .as_array()
            .unwrap()
            .contains(&json!("key_device_lost"))),
        "key_device_lost is never a block in open mode"
    );

    // The mute key, from the device that came back.
    rig.input(Input::KeyDevice(DeviceState::Ready));
    let stops = rig.stopped();
    rig.input(Input::Key(KeyEvent {
        key: Key::Mute,
        action: KeyAction::Press,
    }));
    rig.until("muted", |rig| {
        rig.own()["muted"] == true && rig.own()["transmitting"] == false
    })
    .await;
    rig.stopped_beyond(stops).await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent);
    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// The transmit task alone: a change of talk mode shuts the gate at once, with no release
/// tail, and no frame follows it.
#[tokio::test]
async fn a_change_of_talk_mode_shuts_the_gate_at_once_and_no_frame_follows() {
    use conch_voice::sdk::{RoomHandle, Transport};
    use conch_voice::secrets::Scrubber;
    use conch_voice::transmit::{self, BoxedMic, TxCommand, TxEvent};
    use conch_voice_api::Secret;
    use conch_voice_audio::ToneSource;
    use conch_voice_control::GateCommand;
    use support::fake::{FakeFeed, FakeSdk};

    let sdk = FakeSdk::new(Scrubber::new());
    let (room, _events) = sdk
        .connect("ws://livekit.invalid", &Secret::new("FAKE-join-token"))
        .await
        .unwrap();
    let (_track, feed) = room.publish_microphone().await.unwrap();
    // A release tail of a whole second, which a change of mode must not run.
    let config = transmit::gate_config(1000, Duration::from_secs(60));
    let mut transmitter = transmit::spawn::<FakeFeed>(config).unwrap();
    transmitter.track_lock.reopen();
    let mic = Box::new(ToneSource::new(440.0, 0.25).unwrap()) as BoxedMic;
    for command in [
        TxCommand::Microphone(mic),
        TxCommand::Attach(feed),
        TxCommand::Gate(GateCommand::Open),
    ] {
        assert!(transmitter.commands.send(command).is_ok());
    }
    support::until("frames", String::new, || sdk.frames() >= 3).await;

    for mode in [TalkMode::Open, TalkMode::PushToTalk] {
        let changed = TxCommand::Gate(GateCommand::TalkMode(mode));
        assert!(transmitter.commands.send(changed).is_ok());
        let event = tokio::time::timeout(Duration::from_millis(500), transmitter.events.recv())
            .await
            .expect("the gate says it shut at once")
            .unwrap();
        assert_eq!(
            event,
            TxEvent::GateShut {
                limit_reached: false
            }
        );
        let at_shut = sdk.frames();
        several_frames().await;
        assert_eq!(sdk.frames(), at_shut, "a frame after the change to {mode}");
        // Nothing stands: the next open opens again.
        let open = TxCommand::Gate(GateCommand::Open);
        assert!(transmitter.commands.send(open).is_ok());
        support::until("frames again", String::new, || sdk.frames() > at_shut).await;
    }
}
