//! Secrets never reach output: the login token, join tokens, room names, and a password
//! typed into the server address are absent from every `Debug` and `Display` rendering of
//! every public type of this crate, and from every error the crate makes out of input that
//! holds one.
//!
//! A public type added to the crate gets a line in `every_public_type_can_be_printed`.
//! The secrets here are obviously fake.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::fmt::{Debug, Display};
use std::time::Duration;

use conch_voice_api::{
    Audience, AudienceKind, Client, Error, ErrorBody, PresenceEvent, Refusal, Secret,
    ServerAddress, StreamEnd, Timestamp, VoiceParticipant, VoicePresenceRoom, VoicePresenceV1,
    VoiceRoomGrant, VoiceSessionResponseV1, VoiceTransmitReportV1, VoiceTransmitState,
};
use serde_json::json;
use support::{FAKE_TOKEN, Reply, Step, Stub, client_for, closed_port};

const FAKE_ROOM: &str = "conch-FAKEROOM-zq81-do-not-print";
const FAKE_NET_ROOM: &str = "conch-FAKENETROOM-7h2k-do-not-print";
const FAKE_JOIN: &str = "FAKEJOIN.eyJ2aWRlbyI6e319.do-not-print";
const FAKE_NET_JOIN: &str = "FAKENETJOIN.eyJ2aWRlbyI6e319.do-not-print";
const FAKE_PASSWORD: &str = "FAKEPASSWORD-hunter2-do-not-print";

const SECRETS: [&str; 6] = [
    FAKE_TOKEN,
    FAKE_ROOM,
    FAKE_NET_ROOM,
    FAKE_JOIN,
    FAKE_NET_JOIN,
    FAKE_PASSWORD,
];

/// Every way a value can be formatted for output.
struct Shown {
    name: &'static str,
    text: String,
}

fn debug(name: &'static str, value: &impl Debug) -> Shown {
    Shown {
        name,
        text: format!("{value:?}\n{value:#?}"),
    }
}

fn debug_and_display(name: &'static str, value: &(impl Debug + Display)) -> Shown {
    Shown {
        name,
        text: format!("{value:?}\n{value:#?}\n{value}\n{value:#}"),
    }
}

fn assert_clean(shown: &Shown) {
    for secret in SECRETS {
        assert!(
            !shown.text.contains(secret),
            "{} shows a secret:\n{}",
            shown.name,
            shown.text
        );
    }
    // Not even a recognisable part of one.
    for fragment in ["FAKE", "hunter2", "do-not-print"] {
        assert!(
            !shown.text.contains(fragment),
            "{} shows part of a secret:\n{}",
            shown.name,
            shown.text
        );
    }
}

fn stamp() -> Timestamp {
    Timestamp::parse("2026-10-09T12:00:45.000Z").unwrap()
}

fn session() -> VoiceSessionResponseV1 {
    serde_json::from_value(session_json()).unwrap()
}

fn session_json() -> serde_json::Value {
    json!({
        "livekit_url": "wss://voice.example",
        "identity": "p7",
        "rooms": [
            {"room": FAKE_ROOM, "token": FAKE_JOIN, "can_publish": true, "expires_at": "2026-10-09T12:00:45.000Z"},
            {
                "room": FAKE_NET_ROOM,
                "token": FAKE_NET_JOIN,
                "can_publish": false,
                "expires_at": "2026-10-09T12:00:45.000Z",
                "audience": {"kind": "net", "net_id": 3}
            }
        ]
    })
}

fn presence() -> VoicePresenceV1 {
    VoicePresenceV1 {
        schema: "conch.voice_presence.v1".into(),
        channel_id: 7,
        configured: true,
        available: true,
        rooms: vec![VoicePresenceRoom {
            audience: None,
            participants: vec![VoiceParticipant {
                principal_id: 3,
                can_publish: true,
                transmitting: true,
                joined_at: stamp(),
            }],
        }],
    }
}

fn address_with_password() -> ServerAddress {
    ServerAddress::parse(&format!("http://nick:{FAKE_PASSWORD}@127.0.0.1:8080/conch")).unwrap()
}

#[tokio::test]
async fn every_public_type_can_be_printed_without_showing_a_secret() {
    let session = session();
    let grant: VoiceRoomGrant = session.rooms[1].clone();
    let address = address_with_password();
    let client = Client::new(&address, &Secret::new(FAKE_TOKEN), Duration::from_secs(5)).unwrap();

    let stub = Stub::websocket(vec![Step::Text(
        serde_json::to_string(&presence()).unwrap(),
    )])
    .await;
    let mut stream = stub.client().presence_stream("general").await.unwrap();
    let open_stream = debug("PresenceStream (open)", &stream);
    let event: PresenceEvent = stream.next().await.unwrap();

    let refusal = Refusal {
        status: 503,
        code: "voice_unavailable".into(),
        message: "voice is temporarily unavailable".into(),
    };
    let report = VoiceTransmitReportV1 {
        state: VoiceTransmitState::Started,
        audience: Some(Audience::net(3)),
    };

    let all = [
        debug("Secret", &Secret::new(FAKE_TOKEN)),
        debug("Secret (a room)", &Secret::new(FAKE_ROOM)),
        debug("VoiceRoomGrant", &grant),
        debug("VoiceSessionResponseV1", &session),
        debug("Vec<VoiceRoomGrant>", &session.rooms),
        debug("Option<&VoiceRoomGrant>", &session.channel_grant()),
        debug("ServerAddress", &address),
        debug("Client", &client),
        debug("Client (clone)", &client.clone()),
        open_stream,
        debug("PresenceStream", &stream),
        debug("PresenceEvent", &event),
        debug(
            "StreamEnd",
            &StreamEnd::PolicyViolation {
                reason: "no longer a member of this channel".into(),
            },
        ),
        debug_and_display("Refusal", &refusal),
        debug_and_display("Error", &Error::Unavailable(refusal.clone())),
        debug_and_display("Timestamp", &stamp()),
        debug("Audience", &Audience::principals(vec![3, 7])),
        debug("AudienceKind", &AudienceKind::Net),
        debug(
            "ErrorBody",
            &ErrorBody {
                code: "forbidden".into(),
                message: "agents do not use voice".into(),
            },
        ),
        debug("VoiceParticipant", &presence().rooms[0].participants[0]),
        debug("VoicePresenceRoom", &presence().rooms[0]),
        debug("VoicePresenceV1", &presence()),
        debug("VoiceTransmitReportV1", &report),
        debug("VoiceTransmitState", &VoiceTransmitState::Stopped),
    ];
    for shown in &all {
        assert!(!shown.text.is_empty(), "{}", shown.name);
        assert_clean(shown);
    }

    // The renderings are not clean by being empty: what is not secret is there to read.
    let session_shown = format!("{session:?}");
    for visible in [
        "wss://voice.example",
        "p7",
        "can_publish: false",
        "2026-10-09T12:00:45.000Z",
        "net_id: 3",
    ] {
        assert!(
            session_shown.contains(visible),
            "{visible} missing from {session_shown}"
        );
    }
    assert_eq!(
        session_shown.matches("<redacted>").count(),
        4,
        "{session_shown}"
    );
    let client_shown = format!("{client:?}");
    assert!(
        client_shown.contains("http://127.0.0.1:8080/conch"),
        "{client_shown}"
    );
    assert!(client_shown.contains("5s"), "{client_shown}");
    assert_eq!(
        format!("{address:?}"),
        r#"ServerAddress("http://127.0.0.1:8080/conch")"#
    );

    // And the secrets are still there for the one caller that needs them.
    assert_eq!(session.rooms[0].room.expose(), FAKE_ROOM);
    assert_eq!(session.rooms[0].token.expose(), FAKE_JOIN);
    assert_eq!(grant.token.expose(), FAKE_NET_JOIN);
}

/// Collects one error per way the crate can fail while holding a secret.
async fn errors_made_around_secrets() -> Vec<(&'static str, Error)> {
    let mut errors = Vec::new();

    // Session bodies that hold tokens and room names and are wrong in ways a decoder
    // would like to quote.
    let mut wrong_type = session_json();
    wrong_type["rooms"][0]["can_publish"] = json!(FAKE_JOIN);
    let mut wrong_shape = session_json();
    wrong_shape["rooms"] = json!(FAKE_ROOM);
    let mut token_as_list = session_json();
    token_as_list["rooms"][0]["token"] = json!([FAKE_JOIN]);
    let mut empty_token = session_json();
    empty_token["rooms"][0]["token"] = json!("");
    let mut twice = session_json();
    let first = twice["rooms"][0].clone();
    twice["rooms"].as_array_mut().unwrap().push(first);
    let mut bad_stamp = session_json();
    bad_stamp["rooms"][0]["expires_at"] = json!(FAKE_JOIN);
    let mut bad_kind = session_json();
    bad_kind["rooms"][1]["audience"]["kind"] = json!(FAKE_NET_ROOM);
    let whole = session_json().to_string();
    let cut_short = whole[..whole.find(FAKE_ROOM).unwrap() + FAKE_ROOM.len() + 2].to_owned();
    let bodies: [(&'static str, String); 9] = [
        (
            "session: a token where a boolean belongs",
            wrong_type.to_string(),
        ),
        (
            "session: a room name where the rooms belong",
            wrong_shape.to_string(),
        ),
        ("session: a token inside a list", token_as_list.to_string()),
        (
            "session: an empty token beside real ones",
            empty_token.to_string(),
        ),
        ("session: the same grant twice", twice.to_string()),
        (
            "session: a token where a timestamp belongs",
            bad_stamp.to_string(),
        ),
        (
            "session: a room name as an audience kind",
            bad_kind.to_string(),
        ),
        ("session: cut short after the room name", cut_short),
        (
            "session: not JSON at all",
            format!("room={FAKE_ROOM}&token={FAKE_JOIN}"),
        ),
    ];
    for (name, body) in bodies {
        let stub = Stub::always(Reply::Json(200, body)).await;
        errors.push((name, stub.client().session("general").await.unwrap_err()));
    }

    // Refusals and failures of requests that carried the login token.
    let stub = Stub::always(Reply::error(
        503,
        "voice_unavailable",
        "voice is temporarily unavailable",
    ))
    .await;
    errors.push((
        "a refusal",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub = Stub::always(Reply::Empty(502)).await;
    errors.push((
        "an unexplained status",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub = Stub::always(Reply::Hangup).await;
    errors.push((
        "a hangup",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub = Stub::always(Reply::Truncated(200)).await;
    errors.push((
        "a body cut off",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub = Stub::always(Reply::Stall(Duration::from_secs(5))).await;
    let hurried = stub.client_with(Duration::from_millis(100));
    errors.push(("a timeout", hurried.presence("general").await.unwrap_err()));
    let nowhere = client_for(&closed_port().await, Duration::from_secs(5));
    errors.push((
        "a refused connection",
        nowhere.session("general").await.unwrap_err(),
    ));
    errors.push((
        "a refused socket",
        nowhere.presence_stream("general").await.unwrap_err(),
    ));
    // A server that echoes the request back, as a debugging proxy might.
    let stub =
        Stub::http(|request| Reply::Body(500, "text/plain", request.everything().into_bytes()))
            .await;
    errors.push((
        "an echo of the request",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub =
        Stub::http(|request| Reply::Body(200, "text/plain", request.everything().into_bytes()))
            .await;
    errors.push((
        "an echo of the request as a success",
        stub.client().session("general").await.unwrap_err(),
    ));
    let stub =
        Stub::http(|request| Reply::Body(200, "text/plain", request.everything().into_bytes()))
            .await;
    errors.push((
        "an echo where the socket should open",
        stub.client().presence_stream("general").await.unwrap_err(),
    ));

    // A token that cannot be a header, and an address that holds a password.
    let address = ServerAddress::parse("http://127.0.0.1:8080").unwrap();
    let unusable = Secret::new(format!("{FAKE_TOKEN}\n"));
    errors.push((
        "an unusable token",
        Client::new(&address, &unusable, Duration::from_secs(1)).unwrap_err(),
    ));
    for (name, raw) in [
        (
            "an address with another scheme",
            format!("ftp://nick:{FAKE_PASSWORD}@host"),
        ),
        (
            "an address with a bad host",
            format!("http://nick:{FAKE_PASSWORD}@ho st"),
        ),
        (
            "an address with a bad escape",
            format!("http://nick:{FAKE_PASSWORD}@host/%zz"),
        ),
        (
            "an address with no scheme",
            format!("nick:{FAKE_PASSWORD}@host"),
        ),
    ] {
        errors.push((name, ServerAddress::parse(&raw).unwrap_err()));
    }
    let with_query = ServerAddress::parse(&format!(
        "http://nick:{FAKE_PASSWORD}@host/?token={FAKE_TOKEN}"
    ))
    .unwrap();
    errors.push((
        "an address with a query",
        Client::new(
            &with_query,
            &Secret::new(FAKE_TOKEN),
            Duration::from_secs(1),
        )
        .unwrap_err(),
    ));
    errors
}

#[tokio::test]
async fn no_error_made_around_a_secret_shows_it() {
    let errors = errors_made_around_secrets().await;
    assert!(errors.len() >= 25);
    let mut variants = std::collections::BTreeSet::new();
    for (name, error) in &errors {
        assert_clean(&debug_and_display(name, error));
        // And nothing further down the chain of causes either.
        let mut source = std::error::Error::source(error);
        while let Some(cause) = source {
            assert_clean(&debug_and_display(name, &cause));
            source = cause.source();
        }
        let debugged = format!("{error:?}");
        let variant = debugged
            .split(['(', ' ', '{'])
            .next()
            .unwrap_or_default()
            .to_owned();
        variants.insert(variant);
    }
    // The errors above are of many kinds, not one kind many times.
    for expected in [
        "Undecodable",
        "Invalid",
        "Unavailable",
        "UnexpectedResponse",
        "Transport",
        "Timeout",
        "Connect",
        "NotUpgraded",
        "InvalidToken",
        "InvalidServer",
    ] {
        assert!(
            variants.contains(expected),
            "no {expected} among {variants:?}"
        );
    }
}

#[tokio::test]
async fn the_login_token_is_sent_in_the_authorization_header_and_nowhere_else() {
    let stub = Stub::http(|request| {
        if request.target.ends_with("/voice/transmit") {
            Reply::Empty(204)
        } else if request.target.ends_with("/voice/session") {
            Reply::Json(200, session_json().to_string())
        } else {
            Reply::Json(200, serde_json::to_string(&presence()).unwrap())
        }
    })
    .await;
    let client = stub.client();
    client.session("general").await.unwrap();
    client
        .transmit("general", VoiceTransmitState::Started, None)
        .await
        .unwrap();
    client.presence("general").await.unwrap();
    let socket = Stub::websocket(vec![]).await;
    let _stream = socket.client().presence_stream("general").await.unwrap();

    let mut requests = stub.requests();
    requests.extend(socket.requests());
    assert_eq!(requests.len(), 4);
    for request in requests {
        assert_eq!(
            request.header("authorization"),
            Some(format!("Bearer {FAKE_TOKEN}").as_str())
        );
        assert!(!request.target.contains(FAKE_TOKEN), "{}", request.target);
        assert!(!String::from_utf8_lossy(&request.body).contains(FAKE_TOKEN));
        assert_eq!(
            request.everything().matches(FAKE_TOKEN).count(),
            1,
            "{request:?}"
        );
        assert!(request.header("cookie").is_none());
    }
}
