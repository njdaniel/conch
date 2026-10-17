//! Transmit reports: each press and release told to `conchd`, one at a time and in order
//! (`docs/design/conch-voice.md` §6, "Client side"). A press opens its track once `conchd`
//! has answered its `started`, or after a bounded wait; a release mutes before `stopped` is
//! sent. Audio never depends on the answer.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::Error;
use conch_voice::session::{REPORT_WAIT, Timings};
use conch_voice_control::LineCommand::{Down, Up};
use tokio::sync::Notify;

use support::fake::Call;
use support::stub::{Reply, Stub};
use support::{Rig, Setup, quick, several_frames};

#[tokio::test]
async fn a_press_and_release_30_ms_apart_report_started_then_stopped_every_time() {
    const PRESSES: usize = 100;
    let rig = Rig::start().await;
    rig.ready().await;

    for press in 1..=PRESSES {
        rig.line(Down);
        tokio::time::sleep(Duration::from_millis(30)).await;
        rig.line(Up);
        // The next press waits for this one's `stopped` to have arrived, so that every
        // one of the hundred is a whole press of its own.
        rig.reported(press * 2).await;
    }

    let reported = rig.conchd.reported();
    assert_eq!(
        reported.len(),
        PRESSES * 2,
        "one pair per press, none twice"
    );
    for (press, pair) in reported.chunks(2).enumerate() {
        assert_eq!(pair, ["started", "stopped"], "press {press}");
    }
    assert!(rig.events_named("report").is_empty(), "none needed a retry");
    assert!(rig.sdk.frames() >= PRESSES as u64, "and each was heard");

    let request = rig
        .conchd
        .requests()
        .into_iter()
        .find(|request| request.target.ends_with("/voice/transmit"))
        .unwrap();
    assert_eq!(request.method, "POST");
    assert_eq!(request.target, "/v1/channels/ops/voice/transmit");
    assert_eq!(request.body, r#"{"state":"started"}"#);
}

#[tokio::test]
async fn presses_faster_than_the_release_tail_are_still_reported_in_pairs_and_in_order() {
    // The key goes down again while the gate is still in its release tail: the machine
    // waits for the gate to shut, so the reports of two presses cannot run together.
    let setup = Setup {
        release_tail_ms: 60,
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    for _ in 0..10 {
        rig.line(Down);
        tokio::time::sleep(Duration::from_millis(20)).await;
        rig.line(Up);
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    rig.not_talking().await;
    rig.until("the last stopped", |rig| {
        rig.conchd.reported().last().map(String::as_str) == Some("stopped")
    })
    .await;
    several_frames().await;

    let reported = rig.conchd.reported();
    assert!(
        reported.len() >= 2 && reported.len() % 2 == 0,
        "{reported:?}"
    );
    for pair in reported.chunks(2) {
        assert_eq!(pair, ["started", "stopped"], "{reported:?}");
    }
}

#[tokio::test]
async fn a_conchd_that_never_answers_delays_audio_by_the_wait_alone_and_never_stops_it() {
    const WAIT: Duration = Duration::from_millis(100);
    let conchd = Stub::start().await;
    for _ in 0..3 {
        conchd.next_transmit(Reply::Stall);
    }
    let setup = Setup {
        // Each attempt is given half a second, far longer than the press below.
        report_timeout: Duration::from_millis(500),
        timings: Timings {
            report_wait: WAIT,
            ..quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    rig.ready().await;

    let pressed = tokio::time::Instant::now();
    rig.line(Down);
    rig.frames_beyond(4).await;
    let heard_after = pressed.elapsed();
    rig.line(Up);
    rig.not_talking().await;
    let released_after = pressed.elapsed();
    let sent = rig.sdk.frames();

    // The press waited for the answer for as long as a press waits and no longer, and was
    // over while the first report was still unanswered.
    assert!(
        heard_after >= WAIT,
        "the track opened before the wait was over: first frames after {heard_after:?}"
    );
    assert!(
        heard_after < Duration::from_millis(400) && released_after < Duration::from_millis(450),
        "audio waited for the report: first frames after {heard_after:?}, shut after {released_after:?}"
    );
    assert_eq!(
        rig.conchd.reported(),
        ["started"],
        "still the first attempt"
    );
    assert!(rig.events_named("report").is_empty());
    several_frames().await;
    assert_eq!(
        rig.sdk.frames(),
        sent,
        "and the release shut the gate all the same"
    );

    // The report is then tried three times in all, given up on, and shown as such; the
    // `stopped` that was queued behind it goes out afterwards.
    rig.reported(4).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "started", "started", "stopped"]
    );
    let problems: Vec<String> = rig
        .events_named("report")
        .iter()
        .map(|event| {
            assert_eq!(event["state"], "started");
            event["problem"].as_str().unwrap().to_owned()
        })
        .collect();
    assert_eq!(problems, ["retrying", "retrying", "gave_up"]);
    let first = &rig.events_named("report")[0];
    assert_eq!(first["attempt"], 1);
    assert_eq!(first["of"], 3);
}

#[tokio::test]
async fn a_409_and_a_429_are_not_retried_and_the_status_shows_them() {
    // The 429 is the `started`'s, which the press then goes ahead without; the 409 the
    // `stopped`'s. A 409 to a `started` drops the connection instead, below.
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::error(429, "voice_report_rate_limited"));
    conchd.next_transmit(Reply::error(409, "voice_no_session"));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.frames_beyond(2).await;
    rig.line(Up);
    rig.until("both refusals to be shown", |rig| {
        rig.events_named("report").len() == 2
    })
    .await;
    several_frames().await;

    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped"],
        "each was sent once and not again"
    );
    let shown = rig.events_named("report");
    assert_eq!(shown[0]["state"], "started");
    assert_eq!(shown[0]["problem"], "rate_limited");
    assert_eq!(shown[1]["state"], "stopped");
    assert_eq!(shown[1]["problem"], "no_session");
    assert_eq!(
        rig.connection_states(),
        ["connecting", "connected"],
        "a 409 to a `stopped` leaves the connection be"
    );

    // The queue carries on: the next press is reported as usual.
    rig.line(Down);
    rig.line(Up);
    rig.reported(4).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
}

#[tokio::test]
async fn any_other_failure_is_retried_briefly_in_place_and_keeps_the_order() {
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::error(500, "internal"));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.line(Up);
    rig.reported(3).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "started", "stopped"],
        "the retry goes before the report queued behind it"
    );
    let shown = rig.events_named("report");
    assert_eq!(shown.len(), 1);
    assert_eq!(shown[0]["problem"], "retrying");
}

#[tokio::test]
async fn after_giving_up_on_a_report_the_next_one_goes_out_on_a_new_connection() {
    let conchd = Stub::start().await;
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    // One whole press first, to see that reports share one kept-alive connection: without
    // that, "a new connection" below would prove nothing.
    rig.line(Down);
    rig.line(Up);
    rig.reported(2).await;
    let reports = rig.conchd.reports();
    let kept = reports[0].0;
    assert_eq!(reports[1].0, kept, "reports reuse their connection");

    // Now `conchd` answers 500 to all three attempts of the next `started`. A 500 leaves
    // the connection usable, and the three attempts do use it.
    for _ in 0..3 {
        rig.conchd.next_transmit(Reply::error(500, "internal"));
    }
    rig.line(Down);
    rig.until("the report to be given up on", |rig| {
        rig.events_named("report")
            .iter()
            .any(|event| event["problem"] == "gave_up")
    })
    .await;
    let reports = rig.conchd.reports();
    assert_eq!(
        reports[2..5],
        [
            (kept, "started".to_owned()),
            (kept, "started".to_owned()),
            (kept, "started".to_owned())
        ]
    );

    // Giving up dropped that connection: the client closed it...
    rig.until("the old connection to be closed", |rig| {
        rig.conchd.closed().contains(&kept)
    })
    .await;
    // ...and what follows cannot be behind anything left on it.
    rig.line(Up);
    rig.reported(6).await;
    let (new, state) = rig.conchd.reports()[5].clone();
    assert_eq!(state, "stopped");
    assert_ne!(new, kept, "a new connection for what follows");
    assert!(new > kept);
}

#[tokio::test]
async fn a_report_given_up_on_while_unanswered_leaves_no_connection_that_could_deliver_it_late() {
    let conchd = Stub::start().await;
    for _ in 0..3 {
        conchd.next_transmit(Reply::Stall);
    }
    let setup = Setup {
        report_timeout: Duration::from_millis(40),
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.line(Up);
    rig.reported(4).await;
    let reports = rig.conchd.reports();
    assert_eq!(
        rig.conchd.reported(),
        ["started", "started", "started", "stopped"]
    );
    let stalled: Vec<u64> = reports[..3].iter().map(|(conn, _)| *conn).collect();
    let after = reports[3].0;
    assert!(
        !stalled.contains(&after),
        "the next report is on a connection none of the attempts used"
    );
    // Every connection an unanswered attempt was written to has been closed by the client.
    rig.until("the stalled connections to be closed", |rig| {
        let closed = rig.conchd.closed();
        stalled.iter().all(|conn| closed.contains(conn))
    })
    .await;
}

/// What `conchd` says when it refuses a report is shown as the `detail` of the status line,
/// and is scrubbed first, like its words about a session.
#[tokio::test]
async fn what_conchd_says_about_a_failed_report_is_scrubbed_before_it_is_shown() {
    use support::stub::{FAKE_LOGIN, room_name};
    const FAKE_JWT: &str = "eyJGQUtFIjoiaGVhZGVyIn0.eyJGQUtFIjoiY2xhaW1zIn0.RkFLRS1zaWduYXR1cmU";

    let conchd = Stub::start().await;
    let message = format!(
        "no session in {} for Bearer {FAKE_LOGIN} or {FAKE_JWT}",
        room_name(1)
    );
    let refusal = serde_json::json!({"code": "voice_no_session", "message": message});
    conchd.next_transmit(Reply::Json(409, refusal.to_string()));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.until("the refusal to be shown", |rig| {
        !rig.events_named("report").is_empty()
    })
    .await;
    rig.line(Up);

    let shown = &rig.events_named("report")[0];
    assert_eq!(shown["problem"], "no_session");
    let detail = shown["detail"].as_str().unwrap();
    assert!(
        detail.contains("no session in [redacted] for <redacted> or [redacted]"),
        "{}",
        detail.replace("FAKE", "F4KE")
    );
    let written = rig.out.text();
    for secret in [room_name(1), FAKE_LOGIN.to_owned(), FAKE_JWT.to_owned()] {
        assert!(!written.contains(&secret), "a secret was written");
    }
}

/// What was true of the track at the moment `conchd` received each report.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Heard {
    state: String,
    unmutes: usize,
    muted: bool,
}

/// Has the stub note, for each report it receives, what the fake SDK had been told by then.
fn note_what_conchd_heard(rig: &Rig) -> Arc<Mutex<Vec<Heard>>> {
    let heard = Arc::new(Mutex::new(Vec::new()));
    let (sdk, notes) = (rig.sdk.clone(), Arc::clone(&heard));
    rig.conchd.on_report(move |state| {
        notes.lock().unwrap().push(Heard {
            state: state.to_owned(),
            unmutes: sdk.count(Call::Unmute),
            muted: sdk.is_muted(),
        });
    });
    heard
}

#[tokio::test]
async fn a_press_unmutes_only_once_conchd_has_answered_its_started_report() {
    let conchd = Stub::start().await;
    let answer = Arc::new(Notify::new());
    conchd.next_transmit(Reply::Held(
        Arc::clone(&answer),
        Box::new(Reply::Empty(204)),
    ));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;
    let heard = note_what_conchd_heard(&rig);

    rig.line(Down);
    rig.reported(1).await;
    several_frames().await;
    // `conchd` has the report and has not answered it: the track is still muted and has
    // been handed nothing.
    assert_eq!(
        rig.sdk.count(Call::Unmute),
        0,
        "{:?}",
        rig.sdk.control_calls()
    );
    assert!(rig.sdk.is_muted());
    assert_eq!(rig.sdk.frames(), 0);

    answer.notify_one();
    rig.frames_beyond(2).await;
    assert_eq!(rig.sdk.count(Call::Unmute), 1);
    rig.line(Up);
    rig.reported(2).await;

    assert_eq!(
        rig.sdk.control_calls(),
        [
            Call::Connect,
            Call::Publish,
            Call::Mute,
            Call::Unmute,
            Call::Mute
        ]
    );
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    let heard = heard.lock().unwrap().clone();
    assert_eq!(
        heard[0],
        Heard {
            state: "started".to_owned(),
            unmutes: 0,
            muted: true
        },
        "conchd heard of the press before the track was unmuted"
    );
}

#[tokio::test]
async fn a_press_whose_answer_does_not_come_unmutes_once_the_wait_is_over() {
    const WAIT: Duration = Duration::from_millis(300);
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::Stall);
    let setup = Setup {
        // The one attempt outlasts the test: the report stays unanswered throughout.
        report_timeout: Duration::from_secs(30),
        timings: Timings {
            report_wait: WAIT,
            ..quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    rig.ready().await;

    let pressed = tokio::time::Instant::now();
    rig.line(Down);
    rig.reported(1).await;
    rig.until("the unmute", |rig| rig.sdk.count(Call::Unmute) == 1)
        .await;
    let unmuted_after = pressed.elapsed();
    rig.frames_beyond(2).await;

    assert!(
        unmuted_after >= WAIT,
        "unmuted after {unmuted_after:?}, before the wait of {WAIT:?} was over"
    );
    assert!(
        unmuted_after < WAIT + Duration::from_millis(500),
        "unmuted after {unmuted_after:?}, long after the wait of {WAIT:?}"
    );
    assert_eq!(rig.conchd.reported(), ["started"], "still unanswered");
    assert!(rig.events_named("report").is_empty(), "and not yet failed");

    rig.line(Up);
    rig.not_talking().await;
    rig.until("the mute", |rig| rig.sdk.is_muted()).await;
    let sent = rig.sdk.frames();
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "the release shut the gate as ever");
}

#[test]
fn a_press_waits_at_most_a_second_for_its_answer_as_the_browser_does() {
    assert_eq!(REPORT_WAIT, Duration::from_secs(1));
    assert_eq!(Timings::default().report_wait, REPORT_WAIT);
}

// On threads of its own, so that the report queue can run while the session loop is in a
// mute: a `stopped` queued before the mute would then reach `conchd` while the track was
// still unmuted.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_release_mutes_the_track_before_conchd_hears_stopped() {
    const PRESSES: usize = 10;
    let rig = Rig::start().await;
    rig.ready().await;
    rig.sdk.mute_takes(Duration::from_millis(30));
    let heard = note_what_conchd_heard(&rig);

    for press in 1..=PRESSES {
        rig.line(Down);
        rig.frames_beyond(rig.sdk.frames()).await;
        rig.line(Up);
        rig.reported(press * 2).await;
    }

    let heard = heard.lock().unwrap().clone();
    assert_eq!(heard.len(), PRESSES * 2);
    for (press, pair) in heard.chunks(2).enumerate() {
        assert_eq!(
            pair[0],
            Heard {
                state: "started".to_owned(),
                unmutes: press,
                muted: true
            },
            "press {press}: conchd heard `started` before the unmute"
        );
        assert_eq!(
            pair[1],
            Heard {
                state: "stopped".to_owned(),
                unmutes: press + 1,
                muted: true
            },
            "press {press}: conchd heard `stopped` after the mute"
        );
    }
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn a_press_released_before_its_answer_never_opens_the_track() {
    let conchd = Stub::start().await;
    let answer = Arc::new(Notify::new());
    conchd.next_transmit(Reply::Held(
        Arc::clone(&answer),
        Box::new(Reply::Empty(204)),
    ));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.reported(1).await;
    rig.line(Up);
    rig.not_talking().await;
    // The press is over: `stopped` is queued behind the `started` that is still out.
    answer.notify_one();
    rig.reported(2).await;
    several_frames().await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert_eq!(
        rig.sdk.count(Call::Unmute),
        0,
        "{:?}",
        rig.sdk.control_calls()
    );
    assert_eq!(rig.sdk.frames(), 0);
    assert!(rig.sdk.is_muted());

    // The next press is a press like any other.
    rig.line(Down);
    rig.frames_beyond(2).await;
    rig.line(Up);
    rig.reported(4).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    assert_eq!(rig.sdk.count(Call::Unmute), 1);
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn a_press_shut_by_a_reason_before_its_answer_never_opens_the_track() {
    use conch_voice_control::LineCommand::Mute;

    let conchd = Stub::start().await;
    let answer = Arc::new(Notify::new());
    conchd.next_transmit(Reply::Held(
        Arc::clone(&answer),
        Box::new(Reply::Empty(204)),
    ));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.reported(1).await;
    rig.line(Mute);
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    answer.notify_one();
    rig.reported(2).await;
    several_frames().await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert_eq!(
        rig.sdk.count(Call::Unmute),
        0,
        "{:?}",
        rig.sdk.control_calls()
    );
    assert_eq!(rig.sdk.frames(), 0);

    // The key is still down, and unmuting opens nothing: a shut gate reopens only on a
    // new press.
    rig.line(Mute);
    rig.ready().await;
    several_frames().await;
    assert_eq!(rig.sdk.count(Call::Unmute), 0);
    assert_eq!(rig.sdk.frames(), 0);
}

#[tokio::test]
async fn a_started_answered_409_drops_the_connection_before_the_track_opens() {
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::error(409, "voice_no_session"));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    rig.connected(2).await;
    let waiting = rig
        .events_named("connection")
        .into_iter()
        .find(|event| event["state"] == "waiting")
        .unwrap();
    assert_eq!(waiting["reason"], "no_session");
    assert_eq!(waiting["retry_in_ms"], 0, "conchd is asked again at once");
    assert_eq!(rig.conchd.session_requests(), 2);
    assert_eq!(
        rig.sdk.count(Call::Unmute),
        0,
        "{:?}",
        rig.sdk.control_calls()
    );
    assert_eq!(rig.sdk.frames(), 0);
    assert_eq!(rig.sdk.count(Call::Close), 1, "the first room was left");

    // On the new connection a press is heard as usual.
    rig.line(Up);
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(2).await;
    rig.line(Up);
    rig.until("the second press reported", |rig| {
        let reported = rig.conchd.reported();
        reported.len() >= 4 && reported.ends_with(&["started".to_owned(), "stopped".to_owned()])
    })
    .await;
    assert_eq!(rig.sdk.count(Call::Unmute), 1);
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

#[tokio::test]
async fn a_started_answered_401_stops_the_client_before_the_track_opens() {
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::error(401, "unauthorized"));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.ready().await;

    rig.line(Down);
    let (result, ended) = rig.ended().await;
    let error = result.unwrap_err();
    assert!(matches!(error, Error::Stopped { .. }), "{error:?}");
    assert_eq!(error.to_string(), "not signed in: run `conch login` first");
    assert_eq!(
        ended.sdk.count(Call::Unmute),
        0,
        "{:?}",
        ended.sdk.control_calls()
    );
    assert_eq!(ended.sdk.frames(), 0);
    assert_eq!(ended.sdk.count(Call::Close), 1, "the room was left");
}
