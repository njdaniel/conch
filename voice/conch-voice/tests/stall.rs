//! A reader of standard output that stops reading: a `--json` consumer that stalls, a
//! wrapper that never reads its pipe, a terminal under Ctrl-S. A write to it does not
//! return. The client loses lines; it does not lose the release of the key, `quit`, the end
//! of input or a signal, and it still exits (`docs/design/conch-voice.md` §3: audio leaves
//! only while the key is held).
//!
//! The output here goes the way the real client's does, through the queue and its writer
//! thread (`lines.rs`), to a reader the test can stop. These tests run on a runtime with a
//! single thread and need nothing to keep it awake: no thread of the runtime ever waits
//! for the reader, which is the point.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::lines::QUEUE_LINES;
use conch_voice::session::Input;
use conch_voice_control::LineCommand::{Down, Quit, Up};

use support::fake::Call;
use support::stub::Stub;
use support::{Rig, Setup, several_frames};

/// A session whose output is queued, as `conch-voice --json` writes it, with a `stats` line
/// every 50 ms.
async fn start() -> Rig {
    let setup = Setup {
        queued: true,
        ..Setup::default()
    };
    Rig::start_with(Stub::start().await, setup, |_| {}).await
}

/// The key goes down, frames flow, and then the reader stops reading: the next line the
/// client writes is not taken.
async fn talking_to_a_reader_that_stopped() -> Rig {
    let rig = start().await;
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(5).await;
    rig.reader.stop(true);
    rig.until("a line nobody reads", |rig| rig.reader.is_waited_for())
        .await;
    rig
}

#[tokio::test]
async fn a_release_is_acted_on_although_nobody_is_reading_standard_output() {
    let rig = talking_to_a_reader_that_stopped().await;

    let at_release = rig.sdk.frames();
    let released = tokio::time::Instant::now();
    rig.line(Up);
    rig.until("the release to be acted on", |rig| {
        rig.sdk.is_muted() && rig.conchd.reported() == ["started", "stopped"]
    })
    .await;
    let took = released.elapsed();
    let sent = rig.sdk.frames();

    assert!(took < Duration::from_secs(1), "the release took {took:?}");
    // The release tail is 20 ms here: two frames, and one or two that were in hand.
    assert!(
        sent - at_release <= 5,
        "{} frames after the release",
        sent - at_release
    );
    // And a second later, with the reader still not reading, not one more.
    tokio::time::sleep(Duration::from_secs(1)).await;
    assert_eq!(rig.sdk.frames(), sent, "frames went on after the release");
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert!(rig.reader.is_waited_for(), "the reader never read");

    // A press and a release in that state work as ever.
    rig.line(Down);
    rig.frames_beyond(sent + 3).await;
    rig.line(Up);
    rig.reported(4).await;

    // When the reader reads again it gets what was waiting for it, in order.
    rig.reader.stop(false);
    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok());
    let events = ended.out.events();
    assert_eq!(events.last().unwrap()["state"], "closed");
    let own: Vec<bool> = events
        .iter()
        .filter(|event| event["event"] == "self" && event["blocked"].as_array().unwrap().is_empty())
        .map(|event| event["transmitting"].as_bool().unwrap())
        .collect();
    assert_eq!(
        own,
        [false, true, false, true, false],
        "two presses, in order"
    );
}

#[tokio::test]
async fn each_way_of_leaving_ends_the_client_although_nobody_is_reading_standard_output() {
    for (way, input) in [
        ("quit", Input::Line(Quit)),
        ("the end of standard input", Input::Eof),
        ("a signal", Input::Signal),
    ] {
        let rig = talking_to_a_reader_that_stopped().await;
        let asked = tokio::time::Instant::now();
        rig.input(input);
        let (result, ended) = rig.ended().await;
        let took = asked.elapsed();

        assert!(result.is_ok(), "{way}: {result:?}");
        // The release tail and the gate, then the room and the reports, then the limit on
        // waiting for the last lines to be read, which is 200 ms in this rig: well under
        // two seconds in all.
        assert!(
            took < Duration::from_secs(2),
            "{way}: leaving took {took:?}"
        );
        assert!(ended.sdk.is_muted(), "{way}");
        assert_eq!(ended.conchd.reported(), ["started", "stopped"], "{way}");
        assert_eq!(ended.sdk.count(Call::Close), 1, "{way}: the room was left");
        let sent = ended.sdk.frames();
        several_frames().await;
        assert_eq!(
            ended.sdk.frames(),
            sent,
            "{way}: nothing is sent after leaving"
        );
        assert!(ended.reader.is_waited_for(), "{way}: the reader never read");
        // The writer thread that is still waiting is let go.
        ended.reader.stop(false);
    }
}

#[tokio::test]
async fn a_reader_that_reads_again_is_told_how_many_lines_it_lost() {
    let rig = start().await;
    rig.ready().await;
    rig.reader.stop(true);
    rig.until("a line nobody reads", |rig| rig.reader.is_waited_for())
        .await;

    // Far more lines than the queue holds: each of these is answered with one.
    let unknown = QUEUE_LINES + 600;
    for _ in 0..unknown {
        rig.input(Input::UnknownLine);
    }
    // The loop is not behind any of them: a press that follows is acted on.
    rig.line(Down);
    rig.frames_beyond(3).await;
    rig.line(Up);
    rig.reported(2).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);

    rig.reader.stop(false);
    rig.until("the lost lines to be counted", |rig| {
        rig.out.text().contains("output_dropped")
    })
    .await;
    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    assert!(result.is_ok());

    // Every line that was written is still one object; the gap is said, once it is over,
    // with how many lines went into it; and no line was written twice or out of nowhere.
    let events = ended.out.events();
    let written = events
        .iter()
        .filter(|event| event["event"] == "unknown_command")
        .count();
    let lost: u64 = events
        .iter()
        .filter(|event| event["event"] == "output_dropped")
        .map(|event| event["lines"].as_u64().unwrap())
        .sum();
    assert!(lost >= 600, "{lost} lines were said to be lost");
    assert!(written <= QUEUE_LINES, "{written} were written");
    assert!(
        written as u64 + lost >= unknown as u64,
        "{written} written and {lost} lost do not account for {unknown}"
    );
    assert_eq!(events.last().unwrap()["state"], "closed");
}

/// The SDK's join runs native code, and on a machine with an NVIDIA GPU that code prints to
/// standard output where it stands. With nobody reading, the print does not return, and
/// whatever thread ran the join is held there. That thread must not be the session loop's:
/// here the join holds its thread for three seconds, and in that time a press is answered
/// and a signal ends the client.
#[tokio::test]
async fn a_join_that_blocks_its_thread_does_not_stop_the_session_loop() {
    let began = tokio::time::Instant::now();
    let rig = Rig::start_with(Stub::start().await, Setup::default(), |sdk| {
        sdk.block_next_connect(Duration::from_secs(3));
    })
    .await;
    rig.until("the join", |rig| rig.sdk.count(Call::Connect) == 1)
        .await;

    rig.line(Down);
    rig.until("the press to be refused", |rig| {
        !rig.events_named("press_ignored").is_empty()
    })
    .await;
    assert_eq!(
        rig.events_named("press_ignored")[0]["reason"],
        "not_connected"
    );
    rig.input(Input::Signal);
    let (result, ended) = rig.ended().await;
    let took = began.elapsed();

    assert!(result.is_ok(), "{result:?}");
    // Leaving gives a join in progress 300 ms in this rig; the join's thread is held for
    // three seconds. All of the above happened while it was.
    assert!(
        took < Duration::from_secs(2),
        "the loop waited for the join's thread: {took:?}"
    );
    assert_eq!(ended.sdk.frames(), 0);
    assert_eq!(ended.conchd.reported(), Vec::<String>::new());
}

/// And when such a join is not let go for longer than every limit of its own, the attempt
/// is given up on from outside and made again, with a session of its own.
#[tokio::test]
async fn a_join_whose_thread_is_stuck_is_given_up_on_and_tried_again() {
    let setup = Setup {
        timings: conch_voice::session::Timings {
            connect_limit: Duration::from_millis(50),
            publish_limit: Duration::from_millis(50),
            close_limit: Duration::from_millis(50),
            ..support::quick()
        },
        ..Setup::default()
    };
    let began = tokio::time::Instant::now();
    let rig = Rig::start_with(Stub::start().await, setup, |sdk| {
        // Held for far longer than the limits above and the second on top of them.
        sdk.block_next_connect(Duration::from_secs(4));
    })
    .await;
    rig.connected(1).await;
    let took = began.elapsed();

    assert!(
        took >= Duration::from_millis(1150) && took < Duration::from_secs(3),
        "given up on after the limits and a second, not when the thread came back: {took:?}"
    );
    assert_eq!(rig.conchd.session_requests(), 2);
    assert_eq!(
        rig.sdk.control_calls(),
        [Call::Connect, Call::Connect, Call::Publish, Call::Mute]
    );
    let waiting = rig
        .events_named("connection")
        .into_iter()
        .find(|event| event["state"] == "waiting")
        .unwrap();
    assert_eq!(waiting["reason"], "connect_failed");
    assert_eq!(
        waiting["detail"],
        "joining the voice room took longer than 1.15 s"
    );
}

/// A reader that is only slow is waited for at the very end, up to a limit, so that it has
/// the last lines before the process exits: here it reads nothing until 150 ms after `quit`.
#[tokio::test]
async fn the_last_lines_are_waited_for_when_the_reader_is_only_slow() {
    let setup = Setup {
        queued: true,
        timings: conch_voice::session::Timings {
            output_flush: Duration::from_secs(5),
            ..support::quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    rig.reader.stop(true);
    rig.until("a line nobody reads", |rig| rig.reader.is_waited_for())
        .await;

    let reader = rig.reader.clone();
    let slow = tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(150)).await;
        reader.stop(false);
    });
    let asked = tokio::time::Instant::now();
    rig.line(Quit);
    let (result, ended) = rig.ended().await;
    let took = asked.elapsed();
    slow.await.unwrap();

    assert!(result.is_ok());
    assert!(
        took >= Duration::from_millis(150) && took < Duration::from_secs(4),
        "it waited for the reader, and not for the whole limit: {took:?}"
    );
    // Everything was written by the time the session ended, the last line included.
    assert_eq!(ended.out.events().last().unwrap()["state"], "closed");
}
