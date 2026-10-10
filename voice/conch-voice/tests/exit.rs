//! Leaving: `quit`, the end of standard input and a signal each end the client cleanly, with
//! a final `stopped` if a press was open, and the client exits even if the gate never says
//! that it shut (`docs/design/conch-voice.md` §4 and §6).

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::session::Input;
use conch_voice_control::LineCommand::{Down, Quit};

use support::fake::Call;
use support::stub::{Reply, Stub};
use support::{Rig, Setup, quick};

/// Every way of asking the client to leave.
const WAYS: [(&str, Input); 3] = [
    ("quit", Input::Line(Quit)),
    ("the end of standard input", Input::Eof),
    ("a signal", Input::Signal),
];

#[tokio::test]
async fn each_way_of_leaving_sends_a_final_stopped_leaves_the_room_and_ends_cleanly() {
    for (way, input) in WAYS {
        let rig = Rig::start().await;
        rig.ready().await;
        rig.line(Down);
        rig.frames_beyond(2).await;

        rig.input(input);
        let (result, ended) = rig.ended().await;
        assert!(result.is_ok(), "{way}: {result:?}");
        assert_eq!(
            ended.conchd.reported(),
            ["started", "stopped"],
            "{way}: the press that was open is closed"
        );
        let calls = ended.sdk.control_calls();
        assert_eq!(
            calls[calls.len() - 2..],
            [Call::Mute, Call::Close],
            "{way}: muted, then the room is left: {calls:?}"
        );
        assert!(ended.sdk.is_muted(), "{way}");
        let states: Vec<String> = ended
            .out
            .events()
            .iter()
            .filter(|event| event["event"] == "connection")
            .map(|event| event["state"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(states.last().unwrap(), "closed", "{way}");
        let sent = ended.sdk.frames();
        support::several_frames().await;
        assert_eq!(
            ended.sdk.frames(),
            sent,
            "{way}: nothing is sent after leaving"
        );
    }
}

#[tokio::test]
async fn leaving_with_no_press_open_reports_nothing() {
    for (way, input) in WAYS {
        let rig = Rig::start().await;
        rig.ready().await;
        rig.input(input);
        let (result, ended) = rig.ended().await;
        assert!(result.is_ok(), "{way}");
        assert_eq!(ended.conchd.reported(), Vec::<String>::new(), "{way}");
        assert_eq!(ended.sdk.count(Call::Close), 1, "{way}");
        assert_eq!(ended.sdk.frames(), 0, "{way}");
    }
}

#[tokio::test]
async fn the_end_of_input_before_the_room_is_joined_ends_the_client_too() {
    let mut hold = None;
    let rig = Rig::start_with(Stub::start().await, Setup::default(), |sdk| {
        hold = Some(sdk.hold_publish());
    })
    .await;
    rig.until("the publish", |rig| rig.sdk.count(Call::Publish) == 1)
        .await;
    rig.input(Input::Eof);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok());
    assert_eq!(ended.sdk.frames(), 0);
    assert_eq!(ended.conchd.reported(), Vec::<String>::new());
    drop(hold);
}

#[tokio::test]
async fn the_client_exits_even_if_the_gate_never_reports_that_it_shut() {
    for (way, input) in WAYS {
        let rig = Rig::start().await;
        rig.ready().await;
        // The SDK hangs on the next frame it is handed: the transmit task is stuck inside
        // it, so the gate can be told nothing and can report nothing.
        rig.sdk.hang_sends();
        rig.line(Down);
        rig.talking().await;
        rig.reported(1).await;
        tokio::time::sleep(Duration::from_millis(30)).await;

        let asked = tokio::time::Instant::now();
        rig.input(input);
        let (result, ended) = rig.ended().await;
        let took = asked.elapsed();
        assert!(result.is_ok(), "{way}: {result:?}");
        // The rig gives the gate its 20 ms release tail and 150 ms more.
        assert!(
            took >= Duration::from_millis(150) && took < Duration::from_secs(2),
            "{way}: after a short timeout, not at once and not never: {took:?}"
        );
        assert_eq!(
            ended.conchd.reported(),
            ["started", "stopped"],
            "{way}: the final stopped is sent all the same"
        );
        assert!(ended.sdk.is_muted(), "{way}: and the track is muted");
        assert_eq!(ended.sdk.count(Call::Close), 1, "{way}");
        assert_eq!(
            ended.sdk.frames(),
            0,
            "{way}: nothing got through the hung SDK"
        );
    }
}

#[tokio::test]
async fn the_client_exits_even_if_conchd_never_answers_the_last_report() {
    let conchd = Stub::start().await;
    let setup = Setup {
        timings: conch_voice::session::Timings {
            report_flush: Duration::from_millis(100),
            ..quick()
        },
        report_timeout: Duration::from_secs(5),
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.reported(1).await;
    rig.conchd.next_transmit(Reply::Stall);

    let asked = tokio::time::Instant::now();
    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok());
    assert!(
        asked.elapsed() < Duration::from_secs(2),
        "{:?}",
        asked.elapsed()
    );
    assert_eq!(ended.conchd.reported(), ["started", "stopped"]);
    assert_eq!(
        ended.sdk.count(Call::Close),
        1,
        "the room is left all the same"
    );
}

#[tokio::test]
async fn the_client_exits_even_if_the_sdk_never_finishes_leaving_the_room() {
    let setup = Setup {
        timings: conch_voice::session::Timings {
            close_limit: Duration::from_millis(100),
            ..quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |sdk| {
        sdk.close_takes(Duration::from_secs(600));
    })
    .await;
    rig.ready().await;
    let asked = tokio::time::Instant::now();
    rig.line(Quit);
    let (result, _ended) = rig.ended().await;
    assert!(result.is_ok());
    assert!(
        asked.elapsed() < Duration::from_secs(2),
        "{:?}",
        asked.elapsed()
    );
}

#[tokio::test]
async fn commands_after_quit_do_nothing() {
    let rig = Rig::start().await;
    rig.ready().await;
    rig.line(Quit);
    rig.line(Down);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok());
    assert_eq!(ended.sdk.frames(), 0);
    assert_eq!(ended.conchd.reported(), Vec::<String>::new());
}
