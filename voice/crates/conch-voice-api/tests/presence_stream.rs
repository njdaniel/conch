//! The presence socket against a stub `conchd`: how it is asked for, what it yields, and
//! every way it can end.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::{Duration, Instant};

use conch_voice_api::{Error, PresenceEvent, StreamEnd, VoicePresenceV1};
use serde_json::{Value, json};
use support::{FAKE_AUTHORIZATION, FAKE_TOKEN, Reply, Step, Stub};

/// A snapshot of channel 7 with these principals connected; the first one is talking.
fn snapshot(principals: &[i64]) -> Value {
    let participants: Vec<Value> = principals
        .iter()
        .enumerate()
        .map(|(i, id)| {
            json!({
                "principal_id": id,
                "can_publish": true,
                "transmitting": i == 0,
                "joined_at": "2026-10-09T12:00:31.000Z"
            })
        })
        .collect();
    json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": true,
        "available": true,
        "rooms": [{"participants": participants}]
    })
}

/// Whether an error is the one a case expects.
type Check = fn(&Error) -> bool;
/// What is wrong with a frame, the frame, and the error it must become.
type FrameCase = (&'static str, Step, Check);
/// What the stub answers the upgrade request with, the error it must become, and the
/// status and code that error must keep.
type RefusalCase = (
    &'static str,
    Reply,
    Check,
    Option<u16>,
    Option<&'static str>,
);

fn text(value: &Value) -> Step {
    Step::Text(value.to_string())
}

fn typed(value: &Value) -> VoicePresenceV1 {
    serde_json::from_value(value.clone()).unwrap()
}

fn snapshot_of(event: Result<PresenceEvent, Error>) -> VoicePresenceV1 {
    match event {
        Ok(PresenceEvent::Snapshot(presence)) => presence,
        other => panic!("expected a snapshot, got {other:?}"),
    }
}

fn end_of(event: Result<PresenceEvent, Error>) -> StreamEnd {
    match event {
        Ok(PresenceEvent::Ended(end)) => end,
        other => panic!("expected the end of the stream, got {other:?}"),
    }
}

#[tokio::test]
async fn the_socket_is_asked_for_with_the_token_in_the_header_and_the_channel_in_the_query() {
    let stub = Stub::websocket(vec![text(&snapshot(&[3]))]).await;
    let mut stream = stub
        .client()
        .presence_stream("war room/\u{3b1}")
        .await
        .unwrap();
    snapshot_of(stream.next().await);

    let request = stub.only_request();
    assert_eq!(request.method, "GET");
    // The query is encoded as Go's url.Values.Encode does: a space is a plus sign.
    assert_eq!(request.target, "/v1/voice/ws?channel=war+room%2F%CE%B1");
    assert_eq!(request.header("authorization"), Some(FAKE_AUTHORIZATION));
    // The token is in that one header and never in the address.
    assert!(!request.target.contains(FAKE_TOKEN));
    assert_eq!(request.everything().matches(FAKE_TOKEN).count(), 1);
    assert_eq!(request.header("sec-websocket-version"), Some("13"));
    assert!(
        request
            .header("upgrade")
            .unwrap()
            .eq_ignore_ascii_case("websocket")
    );
    assert!(request.header("sec-websocket-protocol").is_none());
}

#[tokio::test]
async fn the_socket_address_keeps_the_servers_path_prefix() {
    let stub = Stub::websocket(vec![text(&snapshot(&[3]))]).await;
    let client = support::client_for(&stub.url("/conch/"), Duration::from_secs(10));
    let mut stream = client.presence_stream("general").await.unwrap();
    snapshot_of(stream.next().await);
    assert_eq!(
        stub.only_request().target,
        "/conch/v1/voice/ws?channel=general"
    );
}

#[tokio::test]
async fn snapshots_arrive_in_order_and_a_shutdown_ends_the_stream_as_going_away() {
    let docs = [
        snapshot(&[3]),
        snapshot(&[3, 7]),
        snapshot(&[7]),
        snapshot(&[]),
    ];
    let mut script: Vec<Step> = docs.iter().map(text).collect();
    script.push(Step::Close(1001, "server shutting down"));
    let stub = Stub::websocket(script).await;

    let mut stream = stub.client().presence_stream("general").await.unwrap();
    assert!(stream.ended().is_none());
    for doc in &docs {
        assert_eq!(snapshot_of(stream.next().await), typed(doc));
    }
    let end = end_of(stream.next().await);
    assert_eq!(
        end,
        StreamEnd::ServerGoingAway {
            reason: "server shutting down".into()
        }
    );
    // It stays over, with the same reason, however often it is asked.
    assert_eq!(end_of(stream.next().await), end);
    assert_eq!(end_of(stream.next().await), end);
    assert_eq!(stream.ended(), Some(&end));
}

#[tokio::test]
async fn a_policy_violation_close_is_told_apart_and_keeps_the_servers_reason() {
    // The three reasons conchd closes a presence socket with 1008 (voice_ws.go).
    for reason in [
        "no longer a member of this channel",
        "credential no longer valid",
        "presence subscription closed",
    ] {
        let stub = Stub::websocket(vec![text(&snapshot(&[3])), Step::Close(1008, reason)]).await;
        let mut stream = stub.client().presence_stream("general").await.unwrap();
        snapshot_of(stream.next().await);
        assert_eq!(
            end_of(stream.next().await),
            StreamEnd::PolicyViolation {
                reason: reason.into()
            }
        );
    }
}

#[tokio::test]
async fn the_reason_is_given_even_if_the_server_never_finishes_closing() {
    let stall = Duration::from_secs(20);
    let stub = Stub::websocket(vec![
        text(&snapshot(&[3])),
        Step::CloseAndStall(1008, "no longer a member of this channel", stall),
    ])
    .await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    snapshot_of(stream.next().await);
    let started = Instant::now();
    let end = end_of(stream.next().await);
    assert_eq!(
        end,
        StreamEnd::PolicyViolation {
            reason: "no longer a member of this channel".into()
        }
    );
    // The closing handshake is given a moment, not the server's 20 seconds.
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "{:?}",
        started.elapsed()
    );
}

#[tokio::test]
async fn a_call_dropped_while_the_socket_closes_does_not_lose_the_reason() {
    let stall = Duration::from_secs(20);
    let stub = Stub::websocket(vec![
        text(&snapshot(&[3])),
        Step::CloseAndStall(1001, "server shutting down", stall),
    ])
    .await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    snapshot_of(stream.next().await);
    // The caller gives up on this call while the close frame is being answered ...
    let abandoned = tokio::time::timeout(Duration::from_millis(150), stream.next()).await;
    assert!(abandoned.is_err(), "{abandoned:?}");
    // ... and the next call still says why the stream ended, at once.
    let started = Instant::now();
    let end = end_of(stream.next().await);
    assert_eq!(
        end,
        StreamEnd::ServerGoingAway {
            reason: "server shutting down".into()
        }
    );
    assert!(
        started.elapsed() < Duration::from_millis(500),
        "{:?}",
        started.elapsed()
    );
    assert_eq!(end_of(stream.next().await), end);
}

#[tokio::test]
async fn other_close_codes_are_reported_with_their_code() {
    for (code, reason) in [(1000, ""), (1011, "internal error"), (4000, "private")] {
        let stub = Stub::websocket(vec![Step::Close(code, reason)]).await;
        let mut stream = stub.client().presence_stream("general").await.unwrap();
        assert_eq!(
            end_of(stream.next().await),
            StreamEnd::Closed {
                code,
                reason: reason.into()
            }
        );
    }
}

#[tokio::test]
async fn a_socket_closed_mid_stream_with_no_close_frame_is_a_drop() {
    for last in [Step::Drop, Step::Reset] {
        let stub = Stub::websocket(vec![
            text(&snapshot(&[3])),
            text(&snapshot(&[3, 7])),
            last.clone(),
        ])
        .await;
        let mut stream = stub.client().presence_stream("general").await.unwrap();
        assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3])));
        assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3, 7])));
        assert_eq!(end_of(stream.next().await), StreamEnd::Dropped, "{last:?}");
        assert_eq!(end_of(stream.next().await), StreamEnd::Dropped, "{last:?}");
    }
}

#[tokio::test]
async fn a_socket_dropped_before_any_snapshot_is_a_drop() {
    let stub = Stub::websocket(vec![Step::Drop]).await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    assert_eq!(end_of(stream.next().await), StreamEnd::Dropped);
}

#[tokio::test]
async fn a_frame_that_cannot_be_accepted_is_an_error_and_never_skipped() {
    let mut other_schema = snapshot(&[3]);
    other_schema["schema"] = json!("conch.voice_presence.v2");
    let mut unconfigured_but_available = snapshot(&[]);
    unconfigured_but_available["configured"] = json!(false);
    let mut ghost_talker = snapshot(&[3]);
    ghost_talker["rooms"][0]["participants"][0]["can_publish"] = json!(false);
    let mut unknown_audience = snapshot(&[3]);
    unknown_audience["rooms"][0]["audience"] = json!({"kind": "role", "role": "admin"});
    let mut no_rooms = snapshot(&[3]);
    no_rooms.as_object_mut().unwrap().remove("rooms");

    let table: Vec<FrameCase> = vec![
        ("not JSON", Step::Text("presence".into()), |e| {
            matches!(e, Error::Undecodable { .. })
        }),
        ("an empty text frame", Step::Text(String::new()), |e| {
            matches!(e, Error::Undecodable { .. })
        }),
        (
            "JSON cut short",
            Step::Text(snapshot(&[3]).to_string()[..40].into()),
            |e| matches!(e, Error::Undecodable { .. }),
        ),
        (
            "two documents in one frame",
            Step::Text(format!("{}{}", snapshot(&[3]), snapshot(&[7]))),
            |e| matches!(e, Error::Undecodable { .. }),
        ),
        (
            "a message envelope",
            Step::Text(json!({"schema": "conch.message.v2", "id": 1, "body": "hi"}).to_string()),
            |e| matches!(e, Error::Undecodable { .. }),
        ),
        ("a field missing", text(&no_rooms), |e| {
            matches!(e, Error::Undecodable { .. })
        }),
        (
            "an audience of an unknown kind",
            text(&unknown_audience),
            |e| matches!(e, Error::Undecodable { .. }),
        ),
        ("another schema name", text(&other_schema), |e| {
            matches!(
                e,
                Error::Invalid {
                    what: "voice presence",
                    ..
                }
            )
        }),
        (
            "available but not configured",
            text(&unconfigured_but_available),
            |e| {
                matches!(
                    e,
                    Error::Invalid {
                        what: "voice presence",
                        ..
                    }
                )
            },
        ),
        (
            "talking without leave to publish",
            text(&ghost_talker),
            |e| {
                matches!(
                    e,
                    Error::Invalid {
                        what: "voice presence",
                        ..
                    }
                )
            },
        ),
        (
            "a binary frame",
            Step::Binary(snapshot(&[3]).to_string().into_bytes()),
            |e| matches!(e, Error::UnexpectedFrame { kind: "binary" }),
        ),
        (
            "a frame larger than this client accepts",
            Step::Text(" ".repeat((1 << 20) + 1)),
            |e| matches!(e, Error::SocketProtocol { .. }),
        ),
    ];
    for (name, bad, is) in table {
        // A good snapshot, the bad frame, and a good snapshot that must never be seen:
        // the stream does not step over the bad one.
        let stub = Stub::websocket(vec![text(&snapshot(&[3])), bad, text(&snapshot(&[7]))]).await;
        let mut stream = stub.client().presence_stream("general").await.unwrap();
        assert_eq!(
            snapshot_of(stream.next().await),
            typed(&snapshot(&[3])),
            "{name}"
        );
        let error = match stream.next().await {
            Err(error) => error,
            Ok(event) => panic!("{name}: expected an error, got {event:?}"),
        };
        assert!(is(&error), "{name}: {error:?}");
        assert_eq!(end_of(stream.next().await), StreamEnd::Abandoned, "{name}");
        assert_eq!(end_of(stream.next().await), StreamEnd::Abandoned, "{name}");
        assert_eq!(stream.ended(), Some(&StreamEnd::Abandoned), "{name}");
    }
}

#[tokio::test]
async fn pings_are_answered_and_are_not_events() {
    let stub = Stub::websocket(vec![
        Step::Ping,
        text(&snapshot(&[3])),
        Step::Ping,
        Step::Ping,
        text(&snapshot(&[7])),
        Step::Close(1001, "server shutting down"),
    ])
    .await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3])));
    assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[7])));
    assert!(matches!(
        end_of(stream.next().await),
        StreamEnd::ServerGoingAway { .. }
    ));
}

#[tokio::test]
async fn a_snapshot_with_fields_from_a_newer_server_is_still_read() {
    let mut newer = snapshot(&[3]);
    newer["recording"] = json!(false);
    newer["rooms"][0]["topic"] = json!("standup");
    newer["rooms"][0]["participants"][0]["device"] = json!("desk");
    let stub = Stub::websocket(vec![text(&newer)]).await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3])));
}

#[tokio::test]
async fn the_timeout_bounds_opening_the_socket_and_not_its_life() {
    // A quiet channel sends nothing for longer than the request timeout; the stream
    // waits, and the next snapshot still arrives.
    let stub = Stub::websocket(vec![
        text(&snapshot(&[3])),
        Step::Wait(Duration::from_millis(900)),
        text(&snapshot(&[3, 7])),
        Step::Close(1001, "server shutting down"),
    ])
    .await;
    let client = stub.client_with(Duration::from_millis(250));
    let started = Instant::now();
    let mut stream = client.presence_stream("general").await.unwrap();
    assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3])));
    assert_eq!(snapshot_of(stream.next().await), typed(&snapshot(&[3, 7])));
    assert!(started.elapsed() >= Duration::from_millis(900));
    assert!(matches!(
        end_of(stream.next().await),
        StreamEnd::ServerGoingAway { .. }
    ));
}

#[tokio::test]
async fn a_server_slow_to_open_the_socket_is_a_timeout_and_is_not_asked_again() {
    let stub = Stub::always(Reply::Stall(Duration::from_secs(5))).await;
    let client = stub.client_with(Duration::from_millis(200));
    let started = Instant::now();
    let result = client.presence_stream("general").await;
    assert!(matches!(result, Err(Error::Timeout)), "{result:?}");
    assert!(started.elapsed() < Duration::from_secs(4));
    stub.only_request();
}

#[tokio::test]
async fn refusals_before_the_socket_opens_each_have_their_error() {
    let html = b"<html><body><h1>502 Bad Gateway</h1></body></html>".to_vec();
    let table: Vec<RefusalCase> = vec![
        (
            "401 with an error document",
            Reply::error(401, "unauthenticated", "authentication required"),
            |e| matches!(e, Error::Unauthenticated(_)),
            Some(401),
            Some("unauthenticated"),
        ),
        ("401 with no body", Reply::Empty(401), |e| matches!(e, Error::Unauthenticated(_)), Some(401), None),
        (
            "400 voice_requires_auth",
            Reply::error(400, "voice_requires_auth", "voice requires authentication"),
            |e| matches!(e, Error::VoiceRequiresAuth(_)),
            Some(400),
            Some("voice_requires_auth"),
        ),
        (
            "400 invalid_request (no channel)",
            Reply::error(400, "invalid_request", "channel query parameter is required"),
            |e| matches!(e, Error::Refused(_)),
            Some(400),
            Some("invalid_request"),
        ),
        (
            "403 forbidden",
            Reply::error(403, "forbidden", "agents do not use voice"),
            |e| matches!(e, Error::Forbidden(_)),
            Some(403),
            Some("forbidden"),
        ),
        (
            "404 channel_not_found",
            Reply::error(404, "channel_not_found", "channel not found"),
            |e| matches!(e, Error::ChannelNotFound(_)),
            Some(404),
            Some("channel_not_found"),
        ),
        (
            "500 with an error document",
            Reply::error(500, "internal_error", "internal error"),
            |e| matches!(e, Error::Refused(_)),
            Some(500),
            Some("internal_error"),
        ),
        ("500 with no body", Reply::Empty(500), |e| matches!(e, Error::UnexpectedResponse { status: 500 }), Some(500), None),
        (
            "502 with a proxy's page",
            Reply::Body(502, "text/html", html),
            |e| matches!(e, Error::UnexpectedResponse { status: 502 }),
            Some(502),
            None,
        ),
        (
            "a redirect, which is never followed",
            Reply::Redirect(301, "https://elsewhere.example/v1/voice/ws"),
            |e| matches!(e, Error::Redirected { status: 301, .. }),
            Some(301),
            None,
        ),
        (
            "a success that is not an upgrade",
            Reply::Json(200, snapshot(&[3]).to_string()),
            |e| matches!(e, Error::NotUpgraded { status: 200 }),
            Some(200),
            None,
        ),
        (
            "an upgrade that does not answer this request's key",
            Reply::Raw(
                b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\
                  Sec-WebSocket-Accept: AAAAAAAAAAAAAAAAAAAAAAAAAAA=\r\n\r\n"
                    .to_vec(),
            ),
            |e| matches!(e, Error::SocketProtocol { .. }),
            None,
            None,
        ),
        ("the connection closed with no answer", Reply::Hangup, |e| matches!(e, Error::Transport { .. }), None, None),
    ];
    for (name, reply, is, status, code) in table {
        let stub = Stub::always(reply).await;
        let error = match stub.client().presence_stream("general").await {
            Err(error) => error,
            Ok(stream) => panic!("{name}: the stream opened: {stream:?}"),
        };
        assert!(is(&error), "{name}: {error:?}");
        assert_eq!(
            (error.status(), error.code()),
            (status, code),
            "{name}: {error:?}"
        );
        assert!(!format!("{error} {error:?}").contains(FAKE_TOKEN), "{name}");
        let request = stub.only_request();
        assert_eq!(
            request.header("authorization"),
            Some(FAKE_AUTHORIZATION),
            "{name}"
        );
    }
}
