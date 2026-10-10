//! The transmit gate and the published track, as the session loop keeps them in step
//! (`docs/design/conch-voice.md` §3, §13 row 12). The SDK is a fake that records every call
//! and every frame it is handed; the microphone is a tone that never stops, so a frame
//! that does not arrive was stopped by the gate.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::sdk::SdkEvent;
use conch_voice::session::Input;
use conch_voice_control::LineCommand::{Deafen, Down, Mute, Up};
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
    rig.line(Mute);
    rig.until("muted", |rig| {
        rig.own()["muted"] == true && rig.own()["transmitting"] == false
    })
    .await;
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
