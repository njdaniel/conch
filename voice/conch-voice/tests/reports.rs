//! Transmit reports: each press and release told to `conchd`, one at a time and in order,
//! without audio ever waiting for it (`docs/design/conch-voice.md` §6, "Client side").

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice_control::LineCommand::{Down, Up};

use support::stub::{Reply, Stub};
use support::{Rig, Setup, several_frames};

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
async fn a_conchd_that_never_answers_neither_delays_nor_stops_audio() {
    let conchd = Stub::start().await;
    for _ in 0..3 {
        conchd.next_transmit(Reply::Stall);
    }
    let setup = Setup {
        // Each attempt is given half a second, far longer than the press below.
        report_timeout: Duration::from_millis(500),
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

    // The whole press was over while the first report was still unanswered.
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
    let conchd = Stub::start().await;
    conchd.next_transmit(Reply::error(409, "voice_no_session"));
    conchd.next_transmit(Reply::error(429, "voice_report_rate_limited"));
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
    assert_eq!(shown[0]["problem"], "no_session");
    assert_eq!(shown[1]["state"], "stopped");
    assert_eq!(shown[1]["problem"], "rate_limited");

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
