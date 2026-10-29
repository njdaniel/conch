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
    ServerAddress, StreamEnd, Timestamp, VoiceControlAckV1, VoiceControlAckV2, VoiceControlCommand,
    VoiceControlErrorCode, VoiceControlHelloV1, VoiceControlPipeAttachedV1, VoiceControlPlayback,
    VoiceControlProtocolErrorV1, VoiceControlProtocolErrorV2, VoiceControlRefusal,
    VoiceControlTalkMode, VoiceDevice, VoiceDevicesConfig, VoiceDevicesPipeWire, VoiceDevicesV1,
    VoiceParticipant, VoicePresenceRoom, VoicePresenceV1, VoiceRoomGrant, VoiceSessionResponseV1,
    VoiceTransmitReportV1, VoiceTransmitState, WhoAmIResponseV1, parse_command, parse_command_v2,
    parse_event, parse_event_v2,
};
use serde_json::json;
use support::{FAKE_TOKEN, Reply, Request, Step, Stub, client_for, closed_port};

const FAKE_ROOM: &str = "conch-FAKEROOM-zq81-do-not-print";
const FAKE_NET_ROOM: &str = "conch-FAKENETROOM-7h2k-do-not-print";
const FAKE_JOIN: &str = "FAKEJOIN.eyJ2aWRlbyI6e319.do-not-print";
const FAKE_NET_JOIN: &str = "FAKENETJOIN.eyJ2aWRlbyI6e319.do-not-print";
const FAKE_PASSWORD: &str = "FAKEPASSWORD-hunter2-do-not-print";
/// The attach nonce: the golden fixture's, which is `voice-control-pipe/sensitive.json`'s
/// fake secret. It is the one secret-shaped value on a control channel (issue #893).
const FAKE_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

const SECRETS: [&str; 7] = [
    FAKE_TOKEN,
    FAKE_ROOM,
    FAKE_NET_ROOM,
    FAKE_JOIN,
    FAKE_NET_JOIN,
    FAKE_PASSWORD,
    FAKE_NONCE,
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

fn devices() -> VoiceDevicesV1 {
    VoiceDevicesV1 {
        schema: conch_voice_api::VOICE_DEVICES_SCHEMA_V1.into(),
        pipewire: VoiceDevicesPipeWire::Ok,
        synced: true,
        microphones: vec![VoiceDevice {
            name: "speech_out".into(),
            description: String::new(),
            default: false,
        }],
        speakers: Vec::new(),
        unshown_microphones: 0,
        unshown_speakers: 0,
        config: VoiceDevicesConfig::Ok,
        configured_input: Some("speech_out".into()),
        configured_output: None,
    }
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
                sharing: false,
                sharing_audio: false,
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
        // Names and descriptions are device-supplied; the document holds nothing else.
        debug("VoiceDevicesV1", &devices()),
        debug("VoiceDevice", &devices().microphones[0]),
        debug("VoiceDevicesPipeWire", &VoiceDevicesPipeWire::Ok),
        debug("VoiceDevicesConfig", &VoiceDevicesConfig::Refused),
        debug("VoiceTransmitReportV1", &report),
        debug("VoiceTransmitState", &VoiceTransmitState::Stopped),
        debug(
            "WhoAmIResponseV1",
            &serde_json::from_value::<WhoAmIResponseV1>(
                json!({"id": 7, "kind": "human", "name": "someone", "role": "member"}),
            )
            .unwrap(),
        ),
        // The control protocol carries no secret by construction: a start that holds an
        // unknown field named token keeps none of it, and a refused line is not repeated.
        debug("VoiceControlCommand", &control_start_with_a_token()),
        debug_and_display("VoiceControlRefusal", &control_refusal()),
        debug(
            "VoiceControlEvent",
            &parse_event(control_hello().as_bytes()).unwrap(),
        ),
        debug("VoiceControlHelloV1", &VoiceControlHelloV1::v1("0.1.0")),
        debug(
            "VoiceControlAckV1",
            &VoiceControlAckV1::new(3, 1, Some(VoiceControlErrorCode::Invalid)),
        ),
        debug(
            "VoiceControlProtocolErrorV1",
            &VoiceControlProtocolErrorV1::new(2, VoiceControlErrorCode::Malformed),
        ),
        // Version 2: the same, and a device name, which is not a secret, is not shown
        // either; here it is one of the fake secrets, to show that.
        debug("VoiceControlCommandV2", &control_v2_start_with_a_token()),
        debug("VoiceControlCommandV2 (set_devices)", &control_v2_devices()),
        debug_and_display("VoiceControlRefusal (v2)", &control_v2_refusal()),
        debug(
            "VoiceControlEventV2",
            &parse_event_v2(control_v2_ack().as_bytes()).unwrap(),
        ),
        debug(
            "VoiceControlHelloV1 (v1 and v2)",
            &VoiceControlHelloV1::v1_and_v2("0.2.0"),
        ),
        debug("VoiceControlAckV2", &VoiceControlAckV2::new(3, 1, None)),
        debug(
            "VoiceControlProtocolErrorV2",
            &VoiceControlProtocolErrorV2::new(2, VoiceControlErrorCode::TooLong),
        ),
        debug("VoiceControlTalkMode", &VoiceControlTalkMode::Open),
        debug("VoiceControlPlayback", &VoiceControlPlayback::Unavailable),
        // The attach line holds the one secret-shaped value of any control channel: the
        // nonce, which its Debug prints as a placeholder.
        debug(
            "VoiceControlPipeAttachedV1",
            &VoiceControlPipeAttachedV1::new(FAKE_NONCE).unwrap(),
        ),
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

fn control_start_with_a_token() -> VoiceControlCommand {
    let line = json!({
        "schema": "conch.voice_control.v1", "kind": "start", "id": 1,
        "server": "https://conch.example.org", "channel": "general", "principal_id": 7,
        "credential": "stored", "muted": false, "deafened": false, "token": FAKE_TOKEN,
    });
    parse_command(line.to_string().as_bytes()).unwrap()
}

fn control_v2_start_with_a_token() -> conch_voice_api::VoiceControlCommandV2 {
    let line = json!({
        "schema": "conch.voice_control.v2", "kind": "start", "id": 1,
        "server": "https://conch.example.org", "channel": "general", "principal_id": 7,
        "credential": "stored", "muted": false, "deafened": false, "token": FAKE_TOKEN,
    });
    parse_command_v2(line.to_string().as_bytes()).unwrap()
}

fn control_v2_devices() -> conch_voice_api::VoiceControlCommandV2 {
    let line = json!({
        "schema": "conch.voice_control.v2", "kind": "set_devices", "id": 2,
        "input": FAKE_ROOM, "output": FAKE_JOIN,
    });
    parse_command_v2(line.to_string().as_bytes()).unwrap()
}

fn control_v2_refusal() -> VoiceControlRefusal {
    let line = json!({"schema": "conch.voice_control.v2", "kind": "set_talk_mode", "id": 4, "talk_mode": FAKE_TOKEN});
    parse_command_v2(line.to_string().as_bytes()).unwrap_err()
}

fn control_v2_ack() -> String {
    json!({"schema": "conch.voice_control.v2", "kind": "ack", "seq": 3, "id": 1,
           "ok": true, "note": FAKE_ROOM})
    .to_string()
}

fn control_refusal() -> VoiceControlRefusal {
    let line = json!({"schema": "conch.voice_control.v1", "kind": "set_mute", "id": 4, "muted": FAKE_TOKEN});
    parse_command(line.to_string().as_bytes()).unwrap_err()
}

fn control_hello() -> String {
    json!({"schema": "conch.voice_control.v1", "kind": "hello", "seq": 1,
           "versions": ["conch.voice_control.v1"], "helper_version": "0.1.0", "note": FAKE_ROOM})
    .to_string()
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
    let closed = closed_port().await;
    let nowhere = client_for(&closed.url(), Duration::from_secs(5));
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

// ---- A peer that sends the request back -------------------------------------------------
//
// Every request carries the login token, so the token is the one secret whatever answers
// certainly has. A debugging proxy, an echo endpoint or an error page that quotes headers
// sends it back, and then it is in the answer this client makes an error out of.

/// The value of the request's `Authorization` header: `Bearer <token>`.
fn authorization(request: &Request) -> String {
    request.header("authorization").unwrap().to_owned()
}

fn presence_json() -> serde_json::Value {
    json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": true,
        "available": true,
        "rooms": []
    })
}

/// One way of sending the request back as an HTTP answer.
type Reflect = fn(&Request) -> Reply;
/// One way of sending the request back as the text of a socket frame.
type ReflectedFrame = fn(&Request) -> String;

fn reflections() -> Vec<(&'static str, Reflect)> {
    vec![
        ("200, a JSON string holding the request", |r| {
            Reply::Json(200, serde_json::to_string(&r.everything()).unwrap())
        }),
        (
            "200, presence with the Authorization value where channel_id belongs",
            |r| {
                let mut doc = presence_json();
                doc["channel_id"] = json!(authorization(r));
                Reply::Json(200, doc.to_string())
            },
        ),
        ("200, presence with the token as the schema name", |r| {
            let mut doc = presence_json();
            doc["schema"] = json!(authorization(r));
            Reply::Json(200, doc.to_string())
        }),
        ("200, presence with the token as an audience kind", |r| {
            let mut doc = presence_json();
            doc["rooms"] = json!([{"audience": {"kind": authorization(r)}, "participants": []}]);
            Reply::Json(200, doc.to_string())
        }),
        (
            "200, a session with the Authorization value where can_publish belongs",
            |r| {
                let mut doc = session_json();
                doc["rooms"][0]["can_publish"] = json!(authorization(r));
                Reply::Json(200, doc.to_string())
            },
        ),
        (
            "200, a session with the token where expires_at belongs",
            |r| {
                let mut doc = session_json();
                doc["rooms"][0]["expires_at"] = json!(authorization(r));
                Reply::Json(200, doc.to_string())
            },
        ),
        ("200, an echo of the headers as JSON", |r| {
            let echo = json!({"headers": {"Authorization": authorization(r)}, "url": r.target});
            Reply::Json(200, echo.to_string())
        }),
        ("200, the request as plain text", |r| {
            Reply::Body(200, "text/plain", r.everything().into_bytes())
        }),
        ("500, an error document whose message is the request", |r| {
            Reply::error(500, "proxy_error", &r.everything())
        }),
        (
            "500, an error document whose code is the Authorization value",
            |r| Reply::error(500, &authorization(r), ""),
        ),
        ("500, an error document whose code is the bare token", |r| {
            let bare = authorization(r).replace("Bearer ", "");
            Reply::error(500, &bare, &format!("token {bare} was refused"))
        }),
        ("401, an error document that quotes the header", |r| {
            Reply::error(
                401,
                "unauthenticated",
                &format!("bad header: Authorization: {}", authorization(r)),
            )
        }),
        ("403, a named refusal that quotes the header", |r| {
            Reply::error(
                403,
                "forbidden",
                &format!("denied for {}", authorization(r)),
            )
        }),
        ("404, a named refusal that quotes the header", |r| {
            Reply::error(
                404,
                "channel_not_found",
                &format!("no channel for {}", authorization(r)),
            )
        }),
        ("409, a named refusal that quotes the header", |r| {
            Reply::error(
                409,
                "voice_no_session",
                &format!("no session for {}", authorization(r)),
            )
        }),
        ("503, a named refusal that quotes the header", |r| {
            Reply::error(
                503,
                "voice_unavailable",
                &format!("upstream saw {}", authorization(r)),
            )
        }),
        ("500, the request as plain text", |r| {
            Reply::Body(500, "text/plain", r.everything().into_bytes())
        }),
        ("302, a Location that carries the token", |r| {
            let bare = authorization(r).replace("Bearer ", "");
            Reply::Redirect(
                302,
                format!("/login?token={bare}&header={}", authorization(r)),
            )
        }),
    ]
}

/// What must hold of every error, whatever the peer sent: no secret, nothing that could
/// drive a terminal, and a bounded length.
fn assert_safe_to_print(context: &str, error: &Error) {
    let display = error.to_string();
    let debug = format!("{error:?}");
    let pretty = format!("{error:#?}");
    for text in [&display, &debug, &pretty] {
        for secret in SECRETS {
            assert!(!text.contains(secret), "{context}: shows a secret: {text}");
        }
        assert!(
            !text.contains("FAKE"),
            "{context}: shows part of a secret: {text}"
        );
        assert!(
            text.len() < 8192,
            "{context}: {} bytes of error",
            text.len()
        );
    }
    for text in [&display, &debug] {
        assert!(
            !text.chars().any(char::is_control),
            "{context}: a control character in {text:?}"
        );
    }
    let mut source = std::error::Error::source(error);
    while let Some(cause) = source {
        let text = format!("{cause} {cause:?}");
        assert!(
            !text.contains("FAKE"),
            "{context}: a cause shows a secret: {text}"
        );
        source = cause.source();
    }
}

#[tokio::test]
async fn a_peer_that_reflects_the_request_cannot_put_the_token_in_an_error() {
    let mut placeholders = 0;
    for (name, reflect) in reflections() {
        let stub = Stub::http(reflect).await;
        let client = stub.client();
        let errors = [
            (
                "session",
                client.session("general").await.map(|_| ()).unwrap_err(),
            ),
            (
                "transmit",
                client
                    .transmit("general", VoiceTransmitState::Started, None)
                    .await
                    .unwrap_err(),
            ),
            (
                "presence",
                client.presence("general").await.map(|_| ()).unwrap_err(),
            ),
            (
                "presence stream",
                client
                    .presence_stream("general")
                    .await
                    .map(|_| ())
                    .unwrap_err(),
            ),
        ];
        // The stub did receive the token each time: the peer had it to send back.
        let requests = stub.requests();
        assert_eq!(requests.len(), 4, "{name}");
        assert!(
            requests.iter().all(|r| r.everything().contains(FAKE_TOKEN)),
            "{name}"
        );
        for (call, error) in &errors {
            assert_safe_to_print(&format!("{call}: {name}"), error);
            if format!("{error:?}").contains("<redacted>") {
                placeholders += 1;
            }
        }
    }
    // The server's words were kept where they are kept, with the token taken out of
    // them: the errors are not clean merely because everything was thrown away.
    assert!(
        placeholders >= 20,
        "only {placeholders} errors kept scrubbed text"
    );
}

#[tokio::test]
async fn what_a_reflecting_peer_says_is_kept_with_the_token_replaced() {
    let stub = Stub::http(|r| {
        Reply::error(
            503,
            "voice_unavailable",
            &format!(
                "upstream saw Authorization: {} twice: {}",
                authorization(r),
                authorization(r)
            ),
        )
    })
    .await;
    let error = stub.client().session("general").await.unwrap_err();
    match &error {
        Error::Unavailable(refusal) => {
            assert_eq!(refusal.code, "voice_unavailable");
            assert_eq!(
                refusal.message,
                "upstream saw Authorization: <redacted> twice: <redacted>"
            );
        }
        other => panic!("{other:?}"),
    }

    // A code that is the token is no longer any code this client knows.
    let stub =
        Stub::http(|r| Reply::error(503, &authorization(r).replace("Bearer ", ""), "")).await;
    let error = stub.client().session("general").await.unwrap_err();
    match &error {
        Error::Refused(refusal) => assert_eq!(refusal.code, "<redacted>"),
        other => panic!("{other:?}"),
    }

    let stub =
        Stub::http(|r| Reply::Redirect(307, format!("/next?auth={}", authorization(r)))).await;
    let error = stub.client().presence("general").await.unwrap_err();
    match &error {
        Error::Redirected {
            status: 307,
            location,
        } => assert_eq!(location, "/next?auth=<redacted>"),
        other => panic!("{other:?}"),
    }
}

#[tokio::test]
async fn a_socket_that_reflects_the_request_cannot_put_the_token_in_an_error_or_a_reason() {
    let frames: [(&str, ReflectedFrame); 5] = [
        ("a JSON string holding the request", |r| {
            serde_json::to_string(&r.everything()).unwrap()
        }),
        (
            "presence with the Authorization value where channel_id belongs",
            |r| {
                let mut doc = presence_json();
                doc["channel_id"] = json!(authorization(r));
                doc.to_string()
            },
        ),
        ("presence with the token as an audience kind", |r| {
            let mut doc = presence_json();
            doc["rooms"] = json!([{"audience": {"kind": authorization(r)}, "participants": []}]);
            doc.to_string()
        }),
        ("presence with the token as the schema name", |r| {
            let mut doc = presence_json();
            doc["schema"] = json!(authorization(r));
            doc.to_string()
        }),
        ("the request as text", |r| r.everything()),
    ];
    for (name, frame) in frames {
        let socket = Stub::websocket(vec![Step::TextFrom(frame)]).await;
        let mut stream = socket.client().presence_stream("general").await.unwrap();
        let error = match stream.next().await {
            Err(error) => error,
            Ok(event) => panic!("{name}: expected an error, got {event:?}"),
        };
        assert_safe_to_print(&format!("socket frame: {name}"), &error);
        assert!(!format!("{stream:?}").contains("FAKE"), "{name}");
    }

    // A close frame is short, but long enough for a token.
    for code in [1000, 1001, 1008, 1011] {
        let socket = Stub::websocket(vec![Step::CloseFrom(code, |r| {
            format!("saw {}", authorization(r))
        })])
        .await;
        let mut stream = socket.client().presence_stream("general").await.unwrap();
        let event = stream.next().await.unwrap();
        let shown = format!("{event:?} {stream:?} {:?}", stream.ended());
        assert!(!shown.contains("FAKE"), "close {code}: {shown}");
        let reason = match event {
            PresenceEvent::Ended(
                StreamEnd::ServerGoingAway { reason }
                | StreamEnd::PolicyViolation { reason }
                | StreamEnd::Closed { reason, .. },
            ) => reason,
            other => panic!("close {code}: {other:?}"),
        };
        assert_eq!(reason, "saw <redacted>", "close {code}");
    }
}

#[tokio::test]
async fn whatever_the_server_sends_an_error_is_one_bounded_printable_line() {
    let hostile = "\u{1b}[2J\u{1b}[31mowned\r\nsecond line\0\u{7}\u{9b}";
    let huge = "x".repeat(900_000);

    let with_kind = |kind: &str| {
        let mut doc = presence_json();
        doc["rooms"] = json!([{"audience": {"kind": kind, "net_id": 1}, "participants": []}]);
        doc.to_string()
    };
    let with_channel = |channel: &str| {
        let mut doc = presence_json();
        doc["channel_id"] = json!(channel);
        doc.to_string()
    };
    let with_schema = |schema: &str| {
        let mut doc = presence_json();
        doc["schema"] = json!(schema);
        doc.to_string()
    };
    let documents = [
        (
            "an audience kind made of control characters",
            with_kind(hostile),
        ),
        (
            "control characters where a number belongs",
            with_channel(hostile),
        ),
        ("900 kB where a number belongs", with_channel(&huge)),
        (
            "a schema name made of control characters",
            with_schema(hostile),
        ),
        ("900 kB as the schema name", with_schema(&huge)),
        ("900 kB as an audience kind", with_kind(&huge)),
        (
            "a JSON string of control characters",
            serde_json::to_string(hostile).unwrap(),
        ),
        ("control characters, not JSON", hostile.to_owned()),
    ];
    for (name, document) in &documents {
        // As a REST body ...
        let stub = Stub::always(Reply::Json(200, document.clone())).await;
        let client = stub.client();
        assert_safe_to_print(
            &format!("presence: {name}"),
            &client.presence("general").await.unwrap_err(),
        );
        assert_safe_to_print(
            &format!("session: {name}"),
            &client.session("general").await.unwrap_err(),
        );
        // ... and as a socket frame.
        let socket = Stub::websocket(vec![Step::Text(document.clone())]).await;
        let mut stream = socket.client().presence_stream("general").await.unwrap();
        assert_safe_to_print(
            &format!("socket frame: {name}"),
            &stream.next().await.unwrap_err(),
        );
    }

    // Refusals: the server's code and message are kept, cut and escaped.
    let refusals = [
        Reply::error(503, "voice_unavailable", hostile),
        Reply::error(500, hostile, hostile),
        Reply::error(500, &huge[..400_000], &huge[..400_000]),
        Reply::error(401, hostile, &huge[..400_000]),
        Reply::Redirect(302, format!("/{}", "\u{e9}".repeat(4000))),
        Reply::Body(502, "text/html", hostile.as_bytes().to_vec()),
    ];
    for reply in refusals {
        let stub = Stub::always(reply.clone()).await;
        let client = stub.client();
        let label = format!("{reply:?}");
        let context: String = label.chars().take(60).collect();
        assert_safe_to_print(&context, &client.session("general").await.unwrap_err());
        assert_safe_to_print(
            &context,
            &client
                .transmit("general", VoiceTransmitState::Stopped, None)
                .await
                .unwrap_err(),
        );
        assert_safe_to_print(&context, &client.presence("general").await.unwrap_err());
        assert_safe_to_print(
            &context,
            &client
                .presence_stream("general")
                .await
                .map(|_| ())
                .unwrap_err(),
        );
    }

    // A close reason is the server's text too; it is kept bounded, and its Debug is
    // one printable line.
    let socket = Stub::websocket(vec![Step::Close(1008, "\u{1b}[31mred\r\n")]).await;
    let mut stream = socket.client().presence_stream("general").await.unwrap();
    let end = format!("{:?}", stream.next().await.unwrap());
    assert!(!end.chars().any(char::is_control), "{end:?}");
    assert!(end.len() < 2048);
}
