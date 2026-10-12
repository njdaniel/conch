//! The three REST calls against a stub `conchd`: what is sent, what a good answer becomes,
//! and one table per endpoint of every refusal and failure it can meet.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::future::Future;
use std::time::{Duration, Instant};

use conch_voice_api::{
    Audience, Client, Error, PresenceStream, Secret, ServerAddress, VoicePresenceV1,
    VoiceTransmitState,
};
use serde_json::{Value, json};
use support::{FAKE_AUTHORIZATION, FAKE_TOKEN, Reply, Stub, client_for, closed_port};

const FAKE_ROOM: &str = "conch-FAKE-room-k3m7q2x9v4t1b8n6";
const FAKE_JOIN_TOKEN: &str = "FAKE-join-token-not-a-real-credential";

fn session_body() -> Value {
    json!({
        "livekit_url": "wss://voice.example",
        "identity": "p7",
        "rooms": [{
            "room": FAKE_ROOM,
            "token": FAKE_JOIN_TOKEN,
            "can_publish": true,
            "expires_at": "2026-10-09T12:00:45.000Z"
        }]
    })
}

fn presence_body() -> Value {
    json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": true,
        "available": true,
        "rooms": [{
            "participants": [
                {"principal_id": 3, "can_publish": true, "transmitting": true, "joined_at": "2026-10-09T11:58:02.250Z"},
                {"principal_id": 7, "can_publish": true, "transmitting": false, "joined_at": "2026-10-09T12:00:31.000Z"}
            ]
        }]
    })
}

/// No secret this test suite uses may appear in an error, however it is formatted.
fn assert_no_secret(error: &Error, context: &str) {
    let shown = format!("{error} | {error:?} | {error:#?}");
    for secret in [FAKE_TOKEN, FAKE_ROOM, FAKE_JOIN_TOKEN] {
        assert!(!shown.contains(secret), "{context}: {shown}");
    }
}

/// One answer the stub can give and the error it must become.
struct Case {
    name: &'static str,
    reply: Reply,
    /// Whether the error is the right variant.
    is: fn(&Error) -> bool,
    status: Option<u16>,
    code: Option<&'static str>,
}

fn case(
    name: &'static str,
    reply: Reply,
    is: fn(&Error) -> bool,
    status: Option<u16>,
    code: Option<&'static str>,
) -> Case {
    Case {
        name,
        reply,
        is,
        status,
        code,
    }
}

/// What every voice endpoint can answer, whichever it is: the refusals the design notes
/// document for all of them, and the answers of a proxy or a dying server.
fn common_cases() -> Vec<Case> {
    let html = b"<html><body><h1>502 Bad Gateway</h1></body></html>".to_vec();
    vec![
        case(
            "401 with an error document",
            Reply::error(401, "unauthenticated", "authentication required"),
            |e| matches!(e, Error::Unauthenticated(_)),
            Some(401),
            Some("unauthenticated"),
        ),
        case(
            "401 with no body",
            Reply::Empty(401),
            |e| matches!(e, Error::Unauthenticated(_)),
            Some(401),
            None,
        ),
        case(
            "401 from a proxy",
            Reply::Body(401, "text/html", html.clone()),
            |e| matches!(e, Error::Unauthenticated(_)),
            Some(401),
            None,
        ),
        case(
            "400 voice_requires_auth",
            Reply::error(400, "voice_requires_auth", "voice requires authentication"),
            |e| matches!(e, Error::VoiceRequiresAuth(_)),
            Some(400),
            Some("voice_requires_auth"),
        ),
        case(
            "403 forbidden",
            Reply::error(403, "forbidden", "agents do not use voice"),
            |e| matches!(e, Error::Forbidden(_)),
            Some(403),
            Some("forbidden"),
        ),
        case(
            "404 channel_not_found",
            Reply::error(404, "channel_not_found", "channel not found"),
            |e| matches!(e, Error::ChannelNotFound(_)),
            Some(404),
            Some("channel_not_found"),
        ),
        case(
            "404 that is not conchd's (no such route)",
            Reply::Body(404, "text/plain", b"404 page not found\n".to_vec()),
            |e| matches!(e, Error::UnexpectedResponse { status: 404 }),
            Some(404),
            None,
        ),
        case(
            "400 with another code",
            Reply::error(400, "invalid_request", "request body must be valid JSON"),
            |e| matches!(e, Error::Refused(_)),
            Some(400),
            Some("invalid_request"),
        ),
        case(
            "500 with an error document",
            Reply::error(500, "internal_error", "internal error"),
            |e| matches!(e, Error::Refused(_)),
            Some(500),
            Some("internal_error"),
        ),
        case(
            "500 with no body",
            Reply::Empty(500),
            |e| matches!(e, Error::UnexpectedResponse { status: 500 }),
            Some(500),
            None,
        ),
        case(
            "502 with a proxy's page",
            Reply::Body(502, "text/html", html),
            |e| matches!(e, Error::UnexpectedResponse { status: 502 }),
            Some(502),
            None,
        ),
        case(
            "503 with no body",
            Reply::Empty(503),
            |e| matches!(e, Error::UnexpectedResponse { status: 503 }),
            Some(503),
            None,
        ),
        case(
            "an error status with JSON of another shape",
            Reply::Json(500, json!({"error": "boom"}).to_string()),
            |e| matches!(e, Error::UnexpectedResponse { status: 500 }),
            Some(500),
            None,
        ),
        case(
            "an error status with JSON that is not an object",
            Reply::Json(503, "[1, 2, 3]".into()),
            |e| matches!(e, Error::UnexpectedResponse { status: 503 }),
            Some(503),
            None,
        ),
        case(
            "an error document with an empty code",
            Reply::error(500, "", "something"),
            |e| matches!(e, Error::UnexpectedResponse { status: 500 }),
            Some(500),
            None,
        ),
        case(
            "an error body cut short as JSON",
            Reply::Json(503, r#"{"code": "voice_unavailable", "mess"#.into()),
            |e| matches!(e, Error::UnexpectedResponse { status: 503 }),
            Some(503),
            None,
        ),
        case(
            "an error body larger than this client reads",
            Reply::Body(500, "text/plain", vec![b'x'; (1 << 20) + 1]),
            |e| matches!(e, Error::UnexpectedResponse { status: 500 }),
            Some(500),
            None,
        ),
        case(
            // To the stub itself, so that following it would show as a second request.
            // `no_redirect_is_followed_anywhere` covers the rest.
            "a redirect, which is never followed",
            Reply::Redirect(307, "/v1/moved".into()),
            |e| matches!(e, Error::Redirected { status: 307, location } if location == "/v1/moved"),
            Some(307),
            None,
        ),
        case(
            "the connection closed with no answer",
            Reply::Hangup,
            |e| matches!(e, Error::Transport { .. }),
            None,
            None,
        ),
        case(
            "an error whose body stops halfway",
            Reply::Truncated(503),
            |e| matches!(e, Error::Transport { .. }),
            None,
            None,
        ),
    ]
}

fn session_cases() -> Vec<Case> {
    let mut cases = common_cases();
    cases.extend([
        case(
            "503 voice_not_configured",
            Reply::error(
                503,
                "voice_not_configured",
                "voice is not configured on this server",
            ),
            |e| matches!(e, Error::NotConfigured(_)),
            Some(503),
            Some("voice_not_configured"),
        ),
        case(
            "503 voice_unavailable",
            Reply::error(503, "voice_unavailable", "voice is temporarily unavailable"),
            |e| matches!(e, Error::Unavailable(_)),
            Some(503),
            Some("voice_unavailable"),
        ),
    ]);
    cases
}

fn transmit_cases() -> Vec<Case> {
    let mut cases = common_cases();
    cases.extend([
        case(
            "409 voice_no_session",
            Reply::error(
                409,
                "voice_no_session",
                "no voice session for this channel's room",
            ),
            |e| matches!(e, Error::NoSession(_)),
            Some(409),
            Some("voice_no_session"),
        ),
        case(
            "429 voice_report_rate_limited",
            Reply::error(
                429,
                "voice_report_rate_limited",
                "too many transmit reports",
            ),
            |e| matches!(e, Error::RateLimited(_)),
            Some(429),
            Some("voice_report_rate_limited"),
        ),
    ]);
    cases
}

/// Runs one endpoint against every case: the right variant, the status and code kept,
/// no secret in the error, and exactly one request sent.
async fn run_cases<T, F, Fut>(endpoint: &str, cases: Vec<Case>, call: F)
where
    T: std::fmt::Debug,
    F: Fn(Client) -> Fut,
    Fut: Future<Output = Result<T, Error>>,
{
    assert!(cases.len() >= 20);
    for case in cases {
        let context = format!("{endpoint}: {}", case.name);
        let stub = Stub::always(case.reply.clone()).await;
        let error = match call(stub.client()).await {
            Err(error) => error,
            Ok(value) => panic!("{context}: expected an error, got {value:?}"),
        };
        assert!((case.is)(&error), "{context}: got {error:?}");
        assert_eq!(error.status(), case.status, "{context}: {error:?}");
        assert_eq!(error.code(), case.code, "{context}: {error:?}");
        if let Some(refusal) = error.refusal() {
            assert_eq!(Some(refusal.status), case.status, "{context}");
        }
        assert_no_secret(&error, &context);
        // One call, one request: nothing was tried again.
        let request = stub.only_request();
        assert_eq!(
            request.header("authorization"),
            Some(FAKE_AUTHORIZATION),
            "{context}"
        );
    }
}

#[tokio::test]
async fn session_asks_as_conch_would_and_returns_the_validated_session() {
    let stub = Stub::always(Reply::Json(200, session_body().to_string())).await;
    let session = stub.client().session("general").await.unwrap();

    assert_eq!(session.livekit_url, "wss://voice.example");
    assert_eq!(session.identity, "p7");
    assert_eq!(session.rooms.len(), 1);
    let grant = session.channel_grant().unwrap();
    assert_eq!(grant.room.expose(), FAKE_ROOM);
    assert_eq!(grant.token.expose(), FAKE_JOIN_TOKEN);
    assert!(grant.can_publish);
    assert_eq!(grant.expires_at.to_string(), "2026-10-09T12:00:45.000Z");

    let request = stub.only_request();
    assert_eq!(request.method, "POST");
    assert_eq!(request.target, "/v1/channels/general/voice/session");
    // The token is in the Authorization header and nowhere else.
    assert_eq!(request.header("authorization"), Some(FAKE_AUTHORIZATION));
    assert_eq!(request.everything().matches(FAKE_TOKEN).count(), 1);
    assert!(request.body.is_empty());
    assert!(
        request
            .header("user-agent")
            .unwrap()
            .starts_with("conch-voice/")
    );
}

#[tokio::test]
async fn session_refusals_and_failures_each_have_their_error() {
    run_cases("session", session_cases(), |client| async move {
        client.session("general").await
    })
    .await;
}

#[tokio::test]
async fn a_session_that_is_not_valid_is_refused_without_repeating_it() {
    let with = |change: fn(&mut Value)| {
        let mut body = session_body();
        change(&mut body);
        body.to_string()
    };
    // (what is wrong, the body, whether it fails at decoding or at validation)
    let table: Vec<(&str, String, bool)> = vec![
        ("no grants", with(|b| b["rooms"] = json!([])), false),
        (
            "an empty address",
            with(|b| b["livekit_url"] = json!("")),
            false,
        ),
        (
            "an empty identity",
            with(|b| b["identity"] = json!("")),
            false,
        ),
        (
            "an empty token",
            with(|b| b["rooms"][0]["token"] = json!("")),
            false,
        ),
        (
            "an empty room",
            with(|b| b["rooms"][0]["room"] = json!("")),
            false,
        ),
        (
            "no expiry",
            with(|b| b["rooms"][0]["expires_at"] = json!("0001-01-01T00:00:00.000Z")),
            false,
        ),
        (
            "two grants for the channel",
            with(|b| {
                let grant = b["rooms"][0].clone();
                b["rooms"].as_array_mut().unwrap().push(grant);
            }),
            false,
        ),
        (
            "an audience of a kind this client does not know",
            with(|b| b["rooms"][0]["audience"] = json!({"kind": "role", "role": "admin"})),
            true,
        ),
        (
            "a malformed audience",
            with(|b| b["rooms"][0]["audience"] = json!({"kind": "net", "net_id": 0})),
            false,
        ),
        (
            "a grant with no token field",
            with(|b| {
                b["rooms"][0].as_object_mut().unwrap().remove("token");
            }),
            true,
        ),
        (
            "a grant with no can_publish field",
            with(|b| {
                b["rooms"][0].as_object_mut().unwrap().remove("can_publish");
            }),
            true,
        ),
        (
            "a timestamp that is not one",
            with(|b| b["rooms"][0]["expires_at"] = json!("soon")),
            true,
        ),
        // The decoder would quote these values if it were allowed to.
        (
            "the token where a boolean belongs",
            with(|b| b["rooms"][0]["can_publish"] = json!(FAKE_JOIN_TOKEN)),
            true,
        ),
        (
            "the room where the list of rooms belongs",
            with(|b| b["rooms"] = json!(FAKE_ROOM)),
            true,
        ),
        (
            "not JSON, and holding a token",
            format!("token={FAKE_JOIN_TOKEN}"),
            true,
        ),
        (
            "JSON cut short",
            session_body().to_string()[..60].to_owned(),
            true,
        ),
        ("an empty body", String::new(), true),
        ("null", "null".to_owned(), true),
    ];
    for (name, body, at_decoding) in table {
        let stub = Stub::always(Reply::Json(200, body)).await;
        let error = stub.client().session("general").await.unwrap_err();
        if at_decoding {
            assert!(
                matches!(
                    error,
                    Error::Undecodable {
                        what: "voice session",
                        ..
                    }
                ),
                "{name}: {error:?}"
            );
        } else {
            assert!(
                matches!(
                    error,
                    Error::Invalid {
                        what: "voice session",
                        ..
                    }
                ),
                "{name}: {error:?}"
            );
        }
        assert_no_secret(&error, name);
        stub.only_request();
    }
}

#[tokio::test]
async fn a_session_body_larger_than_the_bound_is_not_read_into_memory() {
    let mut body = session_body();
    body["padding"] = json!("x".repeat(1 << 20));
    let stub = Stub::always(Reply::Json(200, body.to_string())).await;
    let error = stub.client().session("general").await.unwrap_err();
    assert!(matches!(error, Error::Undecodable { .. }), "{error:?}");
    assert_no_secret(&error, "oversized session");
}

#[tokio::test]
async fn transmit_sends_the_report_and_takes_204_as_success() {
    let stub = Stub::always(Reply::Empty(204)).await;
    let client = stub.client();
    client
        .transmit("general", VoiceTransmitState::Started, None)
        .await
        .unwrap();
    client
        .transmit("general", VoiceTransmitState::Stopped, None)
        .await
        .unwrap();
    let net = Audience::net(3);
    client
        .transmit("general", VoiceTransmitState::Started, Some(&net))
        .await
        .unwrap();

    let requests = stub.requests();
    assert_eq!(requests.len(), 3);
    let bodies: Vec<Value> = requests
        .iter()
        .map(|r| serde_json::from_slice(&r.body).unwrap())
        .collect();
    // Exactly the golden fixtures voice-transmit-report-v1-{started,stopped,net}.json.
    assert_eq!(bodies[0], json!({"state": "started"}));
    assert_eq!(bodies[1], json!({"state": "stopped"}));
    assert_eq!(
        bodies[2],
        json!({"state": "started", "audience": {"kind": "net", "net_id": 3}})
    );
    for request in &requests {
        assert_eq!(request.method, "POST");
        assert_eq!(request.target, "/v1/channels/general/voice/transmit");
        assert_eq!(request.header("content-type"), Some("application/json"));
        assert_eq!(request.header("authorization"), Some(FAKE_AUTHORIZATION));
        assert_eq!(request.everything().matches(FAKE_TOKEN).count(), 1);
    }
}

#[tokio::test]
async fn only_204_is_a_report_accepted() {
    // A success that is not conchd's answer to a report: a proxy's page, or another
    // endpoint's body. The report must not be taken as recorded.
    let html = b"<html><body>Welcome</body></html>".to_vec();
    for reply in [
        Reply::Body(200, "text/html", html),
        Reply::Json(200, "{}".into()),
        Reply::Empty(200),
        Reply::Empty(202),
    ] {
        let stub = Stub::always(reply.clone()).await;
        let result = stub
            .client()
            .transmit("general", VoiceTransmitState::Started, None)
            .await;
        assert!(
            matches!(result, Err(Error::UnexpectedResponse { status: 200 | 202 })),
            "{reply:?}: {result:?}"
        );
        stub.only_request();
    }
}

#[tokio::test]
async fn transmit_refusals_and_failures_each_have_their_error() {
    run_cases("transmit", transmit_cases(), |client| async move {
        client
            .transmit("general", VoiceTransmitState::Started, None)
            .await
    })
    .await;
}

#[tokio::test]
async fn a_report_for_a_malformed_audience_is_never_sent() {
    let stub = Stub::always(Reply::Empty(204)).await;
    let client = stub.client();
    for audience in [
        Audience::net(0),
        Audience::principals(vec![]),
        Audience::principals(vec![3, 3]),
    ] {
        let result = client
            .transmit("general", VoiceTransmitState::Started, Some(&audience))
            .await;
        assert!(
            matches!(
                result,
                Err(Error::Invalid {
                    what: "voice transmit report",
                    ..
                })
            ),
            "{audience:?}: {result:?}"
        );
    }
    assert!(stub.requests().is_empty());
}

#[tokio::test]
async fn presence_returns_the_validated_snapshot() {
    let stub = Stub::always(Reply::Json(200, presence_body().to_string())).await;
    let presence = stub.client().presence("general").await.unwrap();
    let expected: VoicePresenceV1 = serde_json::from_value(presence_body()).unwrap();
    assert_eq!(presence, expected);
    let room = presence.channel_room().unwrap();
    assert_eq!(room.participants.len(), 2);
    assert!(room.participants[0].transmitting && !room.participants[1].transmitting);

    let request = stub.only_request();
    assert_eq!(request.method, "GET");
    assert_eq!(request.target, "/v1/channels/general/voice");
    assert_eq!(request.header("authorization"), Some(FAKE_AUTHORIZATION));
}

#[tokio::test]
async fn presence_of_a_conchd_without_voice_is_a_snapshot_not_an_error() {
    let body = json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": false,
        "available": false,
        "rooms": []
    });
    let stub = Stub::always(Reply::Json(200, body.to_string())).await;
    let presence = stub.client().presence("general").await.unwrap();
    assert!(!presence.configured && !presence.available && presence.rooms.is_empty());
}

#[tokio::test]
async fn presence_refusals_and_failures_each_have_their_error() {
    run_cases("presence", common_cases(), |client| async move {
        client.presence("general").await
    })
    .await;
}

#[tokio::test]
async fn a_presence_snapshot_that_is_not_valid_is_an_error() {
    let with = |change: fn(&mut Value)| {
        let mut body = presence_body();
        change(&mut body);
        body.to_string()
    };
    let table: Vec<(&str, String, bool)> = vec![
        (
            "another schema name",
            with(|b| b["schema"] = json!("conch.message.v2")),
            false,
        ),
        ("channel zero", with(|b| b["channel_id"] = json!(0)), false),
        (
            "available but not configured",
            with(|b| b["configured"] = json!(false)),
            false,
        ),
        (
            "rooms while unavailable",
            with(|b| b["available"] = json!(false)),
            false,
        ),
        (
            "transmitting without being able to publish",
            with(|b| b["rooms"][0]["participants"][0]["can_publish"] = json!(false)),
            false,
        ),
        (
            "a principal twice",
            with(|b| b["rooms"][0]["participants"][1]["principal_id"] = json!(3)),
            false,
        ),
        (
            "an audience of a kind this client does not know",
            with(|b| b["rooms"][0]["audience"] = json!({"kind": "role"})),
            true,
        ),
        (
            "rooms missing",
            with(|b| {
                b.as_object_mut().unwrap().remove("rooms");
            }),
            true,
        ),
        (
            "a message envelope instead",
            json!({"schema": "conch.message.v2", "id": 1}).to_string(),
            true,
        ),
        ("not JSON", "presence".to_owned(), true),
        ("an empty body", String::new(), true),
    ];
    for (name, body, at_decoding) in table {
        let stub = Stub::always(Reply::Json(200, body)).await;
        let error = stub.client().presence("general").await.unwrap_err();
        if at_decoding {
            assert!(
                matches!(error, Error::Undecodable { .. }),
                "{name}: {error:?}"
            );
        } else {
            assert!(
                matches!(
                    error,
                    Error::Invalid {
                        what: "voice presence",
                        ..
                    }
                ),
                "{name}: {error:?}"
            );
        }
        stub.only_request();
    }
}

#[tokio::test]
async fn a_slow_server_is_a_timeout_and_the_request_is_not_sent_again() {
    let stub = Stub::always(Reply::Stall(Duration::from_secs(5))).await;
    let client = stub.client_with(Duration::from_millis(200));
    assert_eq!(client.timeout(), Duration::from_millis(200));

    let started = Instant::now();
    let session = client.session("general").await;
    let report = client
        .transmit("general", VoiceTransmitState::Started, None)
        .await;
    let presence = client.presence("general").await;
    let waited = started.elapsed();

    assert!(matches!(session, Err(Error::Timeout)), "{session:?}");
    assert!(matches!(report, Err(Error::Timeout)), "{report:?}");
    assert!(matches!(presence, Err(Error::Timeout)), "{presence:?}");
    assert!(
        waited >= Duration::from_millis(600),
        "gave up early: {waited:?}"
    );
    assert!(
        waited < Duration::from_secs(4),
        "waited for the server: {waited:?}"
    );
    assert_eq!(Error::Timeout.status(), None);
    // Three calls, three requests.
    assert_eq!(stub.requests().len(), 3);
}

#[tokio::test]
async fn nothing_listening_is_a_connection_failure() {
    let client = client_for(&closed_port().await, Duration::from_secs(5));
    let session = client.session("general").await;
    let report = client
        .transmit("general", VoiceTransmitState::Stopped, None)
        .await;
    let presence = client.presence("general").await;
    let stream = client.presence_stream("general").await;
    for (name, error) in [
        ("session", session.unwrap_err()),
        ("transmit", report.unwrap_err()),
        ("presence", presence.unwrap_err()),
        ("presence stream", stream.unwrap_err()),
    ] {
        assert!(matches!(error, Error::Connect { .. }), "{name}: {error:?}");
        assert_eq!((error.status(), error.code()), (None, None));
        assert_no_secret(&error, name);
        // The request's address is not part of the error: it says why, not where to.
        let shown = format!("{error} {error:?}");
        for part in ["/v1/", "general", "channels", "http://"] {
            assert!(!shown.contains(part), "{name}: {part} in {shown}");
        }
    }
}

#[tokio::test]
async fn channel_names_are_escaped_into_one_path_segment() {
    // (channel, the segment Go's url.PathEscape gives)
    let table = [
        ("general", "general"),
        ("war room/\u{3b1}", "war%20room%2F%CE%B1"),
        ("a b", "a%20b"),
        ("a/b", "a%2Fb"),
        ("caf\u{e9}", "caf%C3%A9"),
        ("100%", "100%25"),
        ("who?#", "who%3F%23"),
        ("a+b&c=d", "a+b&c=d"),
        ("..x", "..x"),
        ("%2e%2e", "%252e%252e"),
    ];
    for prefix in ["", "/conch", "/Conch/v1/"] {
        let stub = Stub::http(|request| {
            if request.target.ends_with("/voice/session") {
                Reply::Json(200, session_body().to_string())
            } else if request.target.ends_with("/voice/transmit") {
                Reply::Empty(204)
            } else {
                Reply::Json(200, presence_body().to_string())
            }
        })
        .await;
        let client = client_for(&stub.url(prefix), Duration::from_secs(10));
        let base = prefix.trim_end_matches('/');
        for (channel, segment) in table {
            client.session(channel).await.unwrap();
            client
                .transmit(channel, VoiceTransmitState::Started, None)
                .await
                .unwrap();
            client.presence(channel).await.unwrap();
            let requests = stub.requests();
            let sent: Vec<&str> = requests[requests.len() - 3..]
                .iter()
                .map(|r| r.target.as_str())
                .collect();
            assert_eq!(
                sent,
                [
                    format!("{base}/v1/channels/{segment}/voice/session"),
                    format!("{base}/v1/channels/{segment}/voice/transmit"),
                    format!("{base}/v1/channels/{segment}/voice"),
                ],
                "channel {channel:?} under {prefix:?}"
            );
        }
    }
}

#[tokio::test]
async fn a_channel_name_that_cannot_be_a_path_segment_is_refused_before_anything_is_sent() {
    let stub = Stub::always(Reply::Json(200, session_body().to_string())).await;
    let client = stub.client();
    for channel in ["", ".", ".."] {
        let session = client.session(channel).await;
        let report = client
            .transmit(channel, VoiceTransmitState::Started, None)
            .await;
        let presence = client.presence(channel).await;
        let stream = client.presence_stream(channel).await;
        assert!(
            matches!(session, Err(Error::InvalidChannel { .. })),
            "{channel:?}: {session:?}"
        );
        assert!(
            matches!(report, Err(Error::InvalidChannel { .. })),
            "{channel:?}: {report:?}"
        );
        assert!(
            matches!(presence, Err(Error::InvalidChannel { .. })),
            "{channel:?}: {presence:?}"
        );
        assert!(
            matches!(stream, Err(Error::InvalidChannel { .. })),
            "{channel:?}"
        );
    }
    assert!(stub.requests().is_empty());
}

#[tokio::test]
async fn a_client_is_only_built_for_an_address_and_token_it_can_use() {
    let token = Secret::new(FAKE_TOKEN);
    let timeout = Duration::from_secs(1);
    for raw in [
        "http://host:8080?x=1",
        "http://host:8080/#frag",
        "http://host/p?q",
    ] {
        let server = ServerAddress::parse(raw).unwrap();
        let result = Client::new(&server, &token, timeout);
        assert!(
            matches!(result, Err(Error::InvalidServer { .. })),
            "{raw}: {result:?}"
        );
    }
    // Addresses Go would also fail on, only later: at the first request.
    for raw in [
        "http://host:80:80",
        "http://::1",
        "http://host/a/../b",
        "http://[fe80::1%25en0]",
    ] {
        let server = ServerAddress::parse(raw).unwrap();
        let result = Client::new(&server, &token, timeout);
        assert!(
            matches!(result, Err(Error::InvalidServer { .. })),
            "{raw}: {result:?}"
        );
    }
    let server = ServerAddress::parse("http://127.0.0.1:8080").unwrap();
    for bad in ["", "line\nbreak", "nul\0"] {
        let result = Client::new(&server, &Secret::new(bad), timeout);
        assert!(
            matches!(result, Err(Error::InvalidToken)),
            "{bad:?}: {result:?}"
        );
    }
    let client = Client::new(&server, &token, timeout).unwrap();
    assert_eq!(client.server(), "http://127.0.0.1:8080");
    // Userinfo in the address is dropped, not sent and not kept.
    let server = ServerAddress::parse("http://user:FAKE-password@127.0.0.1:8080").unwrap();
    let client = Client::new(&server, &token, timeout).unwrap();
    assert_eq!(client.server(), "http://127.0.0.1:8080");
    assert!(!format!("{client:?}").contains("FAKE"));
}

#[tokio::test]
async fn no_redirect_is_followed_anywhere() {
    // The login token goes where the user pointed the server address and nowhere else.
    // `target` is another origin, a real listener that would answer: it must hear nothing.
    for status in [301u16, 302, 303, 307, 308] {
        let target = Stub::http(|request| {
            if request.target.ends_with("/voice/transmit") {
                Reply::Empty(204)
            } else if request.target.ends_with("/voice/session") {
                Reply::Json(200, session_body().to_string())
            } else {
                Reply::Json(200, presence_body().to_string())
            }
        })
        .await;
        let elsewhere = [
            format!("{}/v1/channels/general/voice", target.url("")),
            format!("{}/v1/channels/general/voice/session", target.url("")),
            format!("{}/v1/voice/ws?channel=general", target.url("")),
            target.url("/"),
        ];
        for location in elsewhere {
            let stub = Stub::always(Reply::Redirect(status, location.clone())).await;
            let client = stub.client();
            let results = [
                client.session("general").await.map(|_| ()),
                client
                    .transmit("general", VoiceTransmitState::Started, None)
                    .await,
                client.presence("general").await.map(|_| ()),
                client.presence_stream("general").await.map(|_| ()),
            ];
            for result in results {
                match result {
                    Err(Error::Redirected {
                        status: s,
                        location: l,
                    }) => {
                        assert_eq!((s, &l), (status, &location));
                    }
                    other => panic!("{status} to {location}: {other:?}"),
                }
            }
            // One request per call to the server the user named; none to the other.
            assert_eq!(stub.requests().len(), 4, "{status} to {location}");
            assert!(
                target.requests().is_empty(),
                "{status} to {location}: the redirect was followed: {:#?}",
                target.requests()
            );
        }

        // To the same origin, absolute and relative: following it would be a second
        // request to the same stub.
        let stub_url = std::sync::Arc::new(std::sync::OnceLock::<String>::new());
        let known = stub_url.clone();
        let stub = Stub::http(move |request| {
            let location = if request.target.contains("absolute") {
                format!("{}/v1/moved", known.get().unwrap())
            } else {
                "/v1/moved".to_owned()
            };
            Reply::Redirect(status, location)
        })
        .await;
        stub_url.set(stub.url("")).unwrap();
        let client = stub.client();
        for channel in ["relative", "absolute"] {
            let results = [
                client.session(channel).await.map(|_| ()),
                client
                    .transmit(channel, VoiceTransmitState::Stopped, None)
                    .await,
                client.presence(channel).await.map(|_| ()),
                client.presence_stream(channel).await.map(|_| ()),
            ];
            for result in results {
                assert!(
                    matches!(result, Err(Error::Redirected { status: s, .. }) if s == status),
                    "{status} {channel}: {result:?}"
                );
            }
        }
        let requests = stub.requests();
        assert_eq!(requests.len(), 8, "{status}: {requests:#?}");
        assert!(
            requests.iter().all(|r| !r.target.contains("moved")),
            "{status}"
        );
    }
}

#[tokio::test]
async fn an_https_address_is_never_spoken_to_in_plain_http() {
    // A plain-HTTP listener at an https address: the client must fail to connect, and
    // the listener must never see a request, least of all the token.
    let stub = Stub::always(Reply::Json(200, presence_body().to_string())).await;
    let secure = stub.url("").replace("http://", "https://");
    // The listener never answers a TLS hello, so each call ends at the timeout.
    let client = client_for(&secure, Duration::from_millis(300));
    assert!(client.server().starts_with("https://"));
    let results = [
        client.session("general").await.map(|_| ()),
        client
            .transmit("general", VoiceTransmitState::Started, None)
            .await,
        client.presence("general").await.map(|_| ()),
        client.presence_stream("general").await.map(|_| ()),
    ];
    for result in results {
        assert!(
            matches!(
                result,
                Err(Error::Timeout | Error::Connect { .. } | Error::Transport { .. })
            ),
            "{result:?}"
        );
    }
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(stub.requests().is_empty(), "{:#?}", stub.requests());
}

#[tokio::test]
async fn an_address_that_would_reach_another_host_than_its_key_names_gets_no_client() {
    let token = Secret::new(FAKE_TOKEN);
    let timeout = Duration::from_secs(1);
    // A capital dotted I lowers to a plain i in the key, as in Go, so these find the
    // login stored for istanbul.example; the request would go to xn--istanbul-o0e.example.
    for raw in [
        "https://\u{130}stanbul.example",
        "https://\u{130}STANBUL.example:8443/conch",
        "http://conch.\u{130}stanbul.example",
        "https://%C4%B0stanbul.example",
    ] {
        let server = ServerAddress::parse(raw).unwrap();
        assert!(
            server.key().contains("istanbul.example"),
            "{raw}: the key is {}",
            server.key()
        );
        let error = Client::new(&server, &token, timeout).unwrap_err();
        match &error {
            Error::InvalidServer { reason } => {
                assert!(reason.contains("cannot be used safely"), "{raw}: {reason}");
            }
            other => panic!("{raw}: {other:?}"),
        }
        // The address is not repeated, as for every other address error.
        let shown = format!("{error} {error:?}");
        assert!(
            !shown.contains("stanbul") && !shown.contains("FAKE"),
            "{shown}"
        );
    }
    // What the guard must not refuse: the plain spelling, and every ordinary address.
    for (raw, key) in [
        ("https://istanbul.example", "https://istanbul.example"),
        ("https://ISTANBUL.example", "https://istanbul.example"),
        ("http://127.0.0.1:8080", "http://127.0.0.1:8080"),
        ("http://localhost:8080/", "http://localhost:8080"),
        ("http://Host:80", "http://host:80"),
        (
            "https://Conch.Example.COM:443",
            "https://conch.example.com:443",
        ),
        (
            "https://conch.example/Conch/V1/",
            "https://conch.example/Conch/V1",
        ),
        ("http://[::1]:8080", "http://[::1]:8080"),
        (
            "http://[2001:DB8::1]:8080/pre",
            "http://[2001:db8::1]:8080/pre",
        ),
        ("https://[::1]", "https://[::1]"),
        ("http://B\u{dc}CHER.example", "http://b\u{fc}cher.example"),
        (
            "http://xn--bcher-kva.example",
            "http://xn--bcher-kva.example",
        ),
        ("http://user:FAKE-password@Host:8080", "http://host:8080"),
    ] {
        let server = ServerAddress::parse(raw).unwrap();
        let client = Client::new(&server, &token, timeout)
            .unwrap_or_else(|e| panic!("{raw} was refused: {e}"));
        assert_eq!(client.server(), key, "{raw}");
    }
}

#[tokio::test]
async fn a_bracketed_host_that_is_not_an_ipv6_address_gets_no_client() {
    // Go 1.25.0 gives these a key (the shared vectors pin that); later Go refuses them
    // outright. Whatever the key, no request is ever made to one.
    let token = Secret::new(FAKE_TOKEN);
    for raw in [
        "http://[evil.com]",
        "http://[evil.com]:8080/",
        "http://a.b[",
        "http://host[::1",
        "http://[::1]]",
        "http://[[::1]]:80",
        "http://[1.2.3.4]",
        "http://127.0.0.1[evil.com]",
    ] {
        if let Ok(server) = ServerAddress::parse(raw) {
            let result = Client::new(&server, &token, Duration::from_secs(1));
            assert!(
                matches!(result, Err(Error::InvalidServer { .. })),
                "{raw}: {result:?}"
            );
        }
    }
}

/// The binary runs these on a multi-threaded runtime and hands them between tasks. This
/// is checked by the compiler; the function only has to build.
#[test]
fn the_client_its_stream_and_its_futures_can_cross_threads() {
    fn shareable<T: Send + Sync + 'static>() {}
    fn sendable<T: Send + 'static>() {}
    fn send<T: Send>(_: &T) {}
    shareable::<Client>();
    shareable::<Error>();
    shareable::<Secret>();
    shareable::<ServerAddress>();
    sendable::<PresenceStream>();

    let client = client_for("http://127.0.0.1:1", Duration::from_secs(1));
    send(&client.session("general"));
    send(&client.transmit("general", VoiceTransmitState::Started, None));
    send(&client.presence("general"));
    send(&client.presence_stream("general"));
    fn next_is_send(stream: &mut PresenceStream) {
        send(&stream.next());
    }
    let _: fn(&mut PresenceStream) = next_is_send;
}

#[tokio::test]
async fn userinfo_in_the_address_is_not_sent() {
    let stub = Stub::always(Reply::Json(200, presence_body().to_string())).await;
    let with_userinfo = stub
        .url("")
        .replace("http://", "http://user:FAKE-password@");
    let client = client_for(&with_userinfo, Duration::from_secs(10));
    client.presence("general").await.unwrap();
    let request = stub.only_request();
    assert_eq!(request.header("authorization"), Some(FAKE_AUTHORIZATION));
    assert!(!request.everything().contains("FAKE-password"));
}
