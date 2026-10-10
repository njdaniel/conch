//! Sessions and disconnects: every connection attempt begins with a session from `conchd`,
//! and every refusal and disconnect leads to what the connection policy says
//! (`docs/design/conch-voice.md` §5, both tables).

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::Error;
use conch_voice::sdk::{DisconnectReason, SdkEvent};
use conch_voice_control::LineCommand::{Down, Up};
use serde_json::{Value, json};

use support::fake::Call;
use support::stub::{Reply, Stub, join_token};
use support::{Rig, Setup, quick};

/// The `connection` objects written so far, without the ones that say `connecting`.
fn connection_changes(rig: &Rig) -> Vec<Value> {
    let mut events = rig.events_named("connection");
    events.retain(|event| event["state"] != "connecting");
    events
}

/// The last `connection` object written.
fn last_connection(out: &support::Written) -> Value {
    let mut events = out.events();
    events.retain(|event| event["event"] == "connection");
    events.pop().unwrap()
}

#[tokio::test]
async fn the_client_joins_with_the_token_of_the_grant_that_has_no_audience() {
    let conchd = Stub::start().await;
    conchd.with_net_grant();
    conchd.livekit_url("ws://livekit.example:7880");
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.connected(1).await;

    assert_eq!(rig.conchd.session_requests(), 1);
    assert_eq!(
        rig.sdk.tokens(),
        [join_token(1)],
        "the channel's own grant, not the net's that was listed first"
    );
    assert_eq!(rig.sdk.addresses(), ["ws://livekit.example:7880"]);
    let request = rig
        .conchd
        .requests()
        .into_iter()
        .find(|request| request.target.ends_with("/voice/session"))
        .unwrap();
    assert_eq!(request.method, "POST");
    assert_eq!(request.target, "/v1/channels/ops/voice/session");
}

#[tokio::test]
async fn a_deleted_room_leads_to_a_new_session_and_a_new_connection_without_a_restart() {
    for reason in [
        DisconnectReason::RoomDeleted,
        DisconnectReason::ParticipantRemoved,
    ] {
        let rig = Rig::start().await;
        rig.ready().await;
        rig.line(Down);
        rig.frames_beyond(2).await;

        rig.sdk.disconnect(reason);
        rig.connected(2).await;
        rig.ready().await;

        assert_eq!(
            rig.conchd.session_requests(),
            2,
            "a new session was asked for"
        );
        assert_eq!(
            rig.sdk.tokens(),
            [join_token(1), join_token(2)],
            "and the new connection was made with it, never with the old token"
        );
        let changes = connection_changes(&rig);
        assert_eq!(changes[0]["state"], "connected");
        assert_eq!(changes[1]["state"], "waiting");
        assert_eq!(changes[1]["retry_in_ms"], 0, "asked at once: {reason:?}");
        assert_eq!(changes[2]["state"], "connected");
        assert_eq!(
            rig.sdk.control_calls(),
            [
                Call::Connect,
                Call::Publish,
                Call::Mute,
                Call::Unmute,
                // The press held when the room went away ends there.
                Call::Mute,
                Call::Close,
                Call::Connect,
                Call::Publish,
                Call::Mute
            ]
        );
        // It does not resume in the new room by itself, and its end was reported.
        let sent = rig.sdk.frames();
        support::several_frames().await;
        assert_eq!(rig.sdk.frames(), sent);
        rig.reported(2).await;
        assert_eq!(rig.conchd.reported(), ["started", "stopped"]);

        // A new press is heard in the new room.
        rig.line(Up);
        rig.line(Down);
        rig.frames_beyond(sent).await;
    }
}

#[tokio::test]
async fn a_lost_connection_and_a_server_shutdown_are_waited_out_and_then_asked_about() {
    for (reason, name) in [
        (DisconnectReason::Lost, "connection_lost"),
        (DisconnectReason::ServerShutdown, "server_shutdown"),
    ] {
        let rig = Rig::start().await;
        rig.ready().await;
        rig.sdk.disconnect(reason);
        rig.connected(2).await;

        let changes = connection_changes(&rig);
        assert_eq!(changes[1]["state"], "waiting");
        assert_eq!(changes[1]["reason"], name);
        // The policy's first wait is 1 s less up to half; the rig divides waits by 200.
        assert_eq!(changes[1]["retry_in_ms"], 4, "{name}");
        assert_eq!(rig.conchd.session_requests(), 2);
        assert_eq!(rig.sdk.tokens(), [join_token(1), join_token(2)]);
    }
}

#[tokio::test]
async fn the_same_identity_joining_elsewhere_stops_the_client_with_its_message() {
    let rig = Rig::start().await;
    rig.ready().await;
    rig.line(Down);
    rig.frames_beyond(2).await;
    rig.sdk.disconnect(DisconnectReason::DuplicateIdentity);

    let (result, ended) = rig.ended().await;
    let error = result.unwrap_err();
    assert!(
        matches!(error, Error::Stopped { exit_code: 1, .. }),
        "{error:?}"
    );
    assert_eq!(
        error.to_string(),
        "this account joined voice from somewhere else, so this client has stopped"
    );
    assert_eq!(error.exit_code(), 1);
    assert_eq!(
        ended.conchd.session_requests(),
        1,
        "it does not go back for a session: that would displace the other device in turn"
    );
    assert_eq!(ended.sdk.count(Call::Connect), 1);
    let last = last_connection(&ended.out);
    assert_eq!(last["state"], "stopped");
    // The press that was open is closed in conchd's record all the same.
    assert_eq!(ended.conchd.reported(), ["started", "stopped"]);
}

#[tokio::test]
async fn each_refusal_of_a_session_stops_the_client_with_its_reason_and_a_nonzero_exit() {
    for (status, code, message) in [
        (
            401,
            "unauthorized",
            "not signed in: run `conch login` first",
        ),
        (
            400,
            "voice_requires_auth",
            "voice needs a signed-in user, and this server runs without authentication",
        ),
        (
            403,
            "forbidden",
            "voice is for people: this login is not a person's",
        ),
        (
            404,
            "channel_not_found",
            "not a member of this channel, or there is no such channel",
        ),
        (
            503,
            "voice_not_configured",
            "voice is not configured on this server",
        ),
    ] {
        let conchd = Stub::start().await;
        conchd.next_session(Reply::error(status, code));
        let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
        let (result, ended) = rig.ended().await;
        let error = result.unwrap_err();
        assert_eq!(error.to_string(), message, "{status} {code}");
        assert_eq!(error.exit_code(), 1, "{status} {code}");
        assert_eq!(ended.conchd.session_requests(), 1, "asked once: {code}");
        assert_eq!(ended.sdk.calls(), [], "and nothing was joined: {code}");
        let last = last_connection(&ended.out);
        assert_eq!(last["state"], "stopped", "{code}");
        assert_eq!(last["reason"], message, "{code}");
    }
}

#[tokio::test]
async fn a_conchd_that_cannot_reach_livekit_or_cannot_be_reached_is_tried_again() {
    // voice_unavailable twice, then a session.
    let conchd = Stub::start().await;
    conchd.next_session(Reply::error(503, "voice_unavailable"));
    conchd.next_session(Reply::error(503, "voice_unavailable"));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.connected(1).await;
    assert_eq!(rig.conchd.session_requests(), 3);
    let waits: Vec<Value> = rig
        .events_named("connection")
        .into_iter()
        .filter(|event| event["state"] == "waiting")
        .collect();
    assert_eq!(waits.len(), 2);
    assert_eq!(waits[0]["reason"], "voice_unavailable");
    // 1 s and then 2 s, each less up to half, divided by 200: the waits double.
    assert_eq!(waits[0]["retry_in_ms"], 4);
    assert_eq!(waits[1]["retry_in_ms"], 9);

    // Nothing listening at all.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let nowhere = format!("http://{}", listener.local_addr().unwrap());
    drop(listener);
    let setup = Setup {
        server: Some(nowhere),
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.until("two waits", |rig| {
        rig.connection_states()
            .iter()
            .filter(|state| *state == "waiting")
            .count()
            >= 2
    })
    .await;
    let waiting = rig
        .events_named("connection")
        .into_iter()
        .find(|event| event["state"] == "waiting")
        .unwrap();
    assert_eq!(waiting["reason"], "server_unreachable");
    assert_eq!(rig.sdk.calls(), []);
}

/// A room that was joined and whose publish then failed is left before anything else: the
/// next attempt joins as the same identity, and nothing would ever close the first.
#[tokio::test]
async fn a_join_whose_publish_fails_leaves_the_room_and_is_tried_again() {
    let rig = Rig::start_with(Stub::start().await, Setup::default(), |sdk| {
        sdk.fail_next_publish("the track was refused");
    })
    .await;
    rig.connected(1).await;
    assert_eq!(
        rig.sdk.control_calls(),
        [
            Call::Connect,
            Call::Publish,
            Call::Close,
            Call::Connect,
            Call::Publish,
            Call::Mute
        ]
    );
    assert_eq!(rig.sdk.tokens(), [join_token(1), join_token(2)]);
    let waiting = rig
        .events_named("connection")
        .into_iter()
        .find(|event| event["state"] == "waiting")
        .unwrap();
    assert_eq!(waiting["reason"], "connect_failed");
    assert_eq!(waiting["detail"], "the track was refused");
}

/// When a connection has ended the transmit task lets go of that track's audio source at
/// once, not when the next connection replaces it: there is then nothing a frame could be
/// handed to, whatever the gate does.
#[tokio::test]
async fn a_connection_that_ended_no_longer_holds_its_audio_source() {
    let rig = Rig::start().await;
    rig.ready().await;
    assert_eq!(rig.sdk.feeds_held(), 1);

    // The next join stops inside its publish, before it has an audio source of its own.
    let hold = rig.sdk.hold_publish();
    rig.sdk.disconnect(DisconnectReason::Lost);
    rig.until("the second publish", |rig| {
        rig.sdk.count(Call::Publish) == 2
    })
    .await;
    rig.until("the first audio source to be let go", |rig| {
        rig.sdk.feeds_held() == 0
    })
    .await;

    hold.notify_one();
    rig.connected(2).await;
    assert_eq!(rig.sdk.feeds_held(), 1);
}

#[tokio::test]
async fn a_failed_join_is_retried_each_time_with_a_session_of_its_own() {
    let rig = Rig::start_with(Stub::start().await, Setup::default(), |sdk| {
        sdk.fail_next_connect("the signalling socket was refused");
        sdk.fail_next_connect("the signalling socket was refused");
    })
    .await;
    rig.connected(1).await;
    assert_eq!(rig.conchd.session_requests(), 3);
    assert_eq!(
        rig.sdk.tokens(),
        [join_token(1), join_token(2), join_token(3)],
        "no token is used for a second attempt"
    );
    let waiting = rig
        .events_named("connection")
        .into_iter()
        .find(|event| event["state"] == "waiting")
        .unwrap();
    assert_eq!(waiting["reason"], "connect_failed");
    assert_eq!(waiting["detail"], "the signalling socket was refused");
}

#[tokio::test]
async fn twenty_seconds_without_reconnected_closes_the_connection_and_asks_for_a_session() {
    // The twenty seconds are injected as 80 ms: no test waits out the real grace.
    let setup = Setup {
        timings: conch_voice::session::Timings {
            reconnect_grace: Duration::from_millis(80),
            ..quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;

    let began = tokio::time::Instant::now();
    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    rig.until("the close", |rig| rig.sdk.count(Call::Close) == 1)
        .await;
    assert!(
        began.elapsed() >= Duration::from_millis(80),
        "not before the grace period is over"
    );
    assert_eq!(
        rig.conchd.session_requests(),
        1,
        "and no session is asked for until the connection is closed"
    );
    rig.connected(2).await;
    rig.ready().await;

    assert_eq!(rig.conchd.session_requests(), 2);
    assert_eq!(rig.sdk.tokens(), [join_token(1), join_token(2)]);
    let states: Vec<Value> = connection_changes(&rig)
        .iter()
        .map(|event| json!([event["state"], event["reason"]]))
        .collect();
    assert_eq!(
        states,
        [
            json!(["connected", null]),
            json!(["reconnecting", null]),
            json!(["waiting", "reconnect_timed_out"]),
            json!(["connected", null]),
        ]
    );
}

#[tokio::test]
async fn a_reconnect_that_succeeds_within_the_grace_period_keeps_the_connection() {
    let setup = Setup {
        timings: conch_voice::session::Timings {
            reconnect_grace: Duration::from_millis(150),
            ..quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    assert!(rig.sdk.emit(SdkEvent::Reconnecting));
    rig.until("reconnecting", |rig| {
        rig.connection_states().last().unwrap() == "reconnecting"
    })
    .await;
    assert!(rig.sdk.emit(SdkEvent::Reconnected));
    rig.connected(2).await;

    // Well past where the grace period would have ended.
    tokio::time::sleep(Duration::from_millis(250)).await;
    assert_eq!(rig.sdk.count(Call::Close), 0);
    assert_eq!(rig.conchd.session_requests(), 1);
    assert_eq!(rig.sdk.count(Call::Connect), 1);
}

#[tokio::test]
async fn a_stop_is_final_whatever_happens_after_it() {
    // A room deletion, then a refusal of the session that follows: the client stops, with
    // the refusal's reason, and asks nothing more.
    let rig = Rig::start().await;
    rig.ready().await;
    rig.conchd
        .next_session(Reply::error(404, "channel_not_found"));
    rig.sdk.disconnect(DisconnectReason::RoomDeleted);
    let (result, ended) = rig.ended().await;
    assert_eq!(
        result.unwrap_err().to_string(),
        "not a member of this channel, or there is no such channel"
    );
    assert_eq!(ended.conchd.session_requests(), 2);
    assert_eq!(ended.sdk.count(Call::Connect), 1);
}
