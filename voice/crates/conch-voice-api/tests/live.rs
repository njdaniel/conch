//! This crate against a real `conchd`. Ignored by default: it needs a running server and a
//! login that the real `conch login` stored, both set up by whoever runs it.
//!
//! ```sh
//! CONCH_VOICE_LIVE_SERVER=http://127.0.0.1:8080 \
//! CONCH_VOICE_LIVE_CONFIG_DIR=/path/holding/conch/credentials.json \
//! CONCH_VOICE_LIVE_CHANNEL=ops \
//! CONCH_VOICE_LIVE_EXPECT=voice \
//!     cargo test --locked -p conch-voice-api --test live -- --ignored
//! ```
//!
//! `CONCH_VOICE_LIVE_EXPECT` says what the server is: `voice` (LiveKit configured and
//! reachable) or `no-voice` (not configured). The login must be a person who is a member of
//! the channel. Nothing is printed that came from the server's session answer.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::path::PathBuf;
use std::time::Duration;

use conch_voice_api::{Client, Error, PresenceEvent, ServerAddress, resolve_token};

struct Live {
    client: Client,
    channel: String,
    voice: bool,
}

fn live() -> Live {
    let var = |name: &str| std::env::var(name).unwrap_or_else(|_| panic!("set {name}"));
    let server = ServerAddress::parse(&var("CONCH_VOICE_LIVE_SERVER")).unwrap();
    let config_dir = PathBuf::from(var("CONCH_VOICE_LIVE_CONFIG_DIR"));
    // The stored login only: an explicit token is deliberately not passed, so that the
    // file the Go client wrote is what is being read.
    let token = resolve_token(None, &server, Some(&config_dir)).unwrap();
    let voice = match var("CONCH_VOICE_LIVE_EXPECT").as_str() {
        "voice" => true,
        "no-voice" => false,
        other => panic!("CONCH_VOICE_LIVE_EXPECT must be voice or no-voice, not {other:?}"),
    };
    Live {
        client: Client::new(&server, &token, Duration::from_secs(10)).unwrap(),
        channel: var("CONCH_VOICE_LIVE_CHANNEL"),
        voice,
    }
}

#[tokio::test]
#[ignore = "needs a running conchd and a stored login; see the top of this file"]
async fn a_session_is_issued_or_refused_as_the_server_is_configured() {
    let live = live();
    let answer = live.client.session(&live.channel).await;
    if !live.voice {
        assert!(
            matches!(answer, Err(Error::NotConfigured(_))),
            "a server without voice refuses with voice_not_configured: {answer:?}"
        );
        return;
    }
    let session = answer.unwrap();
    assert!(
        session.livekit_url.starts_with("ws://") || session.livekit_url.starts_with("wss://"),
        "the LiveKit address is a socket address"
    );
    assert!(session.identity.starts_with('p'), "{}", session.identity);
    let grant = session
        .channel_grant()
        .expect("a grant for the whole channel");
    assert!(grant.can_publish);
    assert!(!grant.expires_at.is_zero());

    // The real join token and room name stay out of every rendering.
    let shown = format!("{session:?} {session:#?}");
    assert!(!grant.token.is_empty() && !grant.room.is_empty());
    assert!(
        !shown.contains(grant.token.expose()),
        "the join token was printed"
    );
    assert!(
        !shown.contains(grant.room.expose()),
        "the room name was printed"
    );
}

#[tokio::test]
#[ignore = "needs a running conchd and a stored login; see the top of this file"]
async fn presence_says_whether_voice_is_configured_by_both_routes() {
    let live = live();
    let snapshot = live.client.presence(&live.channel).await.unwrap();
    assert_eq!(snapshot.configured, live.voice);

    let mut stream = live.client.presence_stream(&live.channel).await.unwrap();
    let first = tokio::time::timeout(Duration::from_secs(5), stream.next())
        .await
        .expect("the socket sends the current state on connect")
        .unwrap();
    match first {
        PresenceEvent::Snapshot(document) => assert_eq!(document.configured, live.voice),
        PresenceEvent::Ended(end) => panic!("the stream ended at once: {end:?}"),
    }
}

#[tokio::test]
#[ignore = "needs a running conchd and a stored login; see the top of this file"]
async fn a_channel_that_does_not_exist_is_not_found() {
    let live = live();
    let answer = live
        .client
        .session("no such channel, with a / and a space")
        .await;
    assert!(
        matches!(answer, Err(Error::ChannelNotFound(_))),
        "{answer:?}"
    );
}
