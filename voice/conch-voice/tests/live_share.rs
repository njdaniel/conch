//! The share's sound against a real LiveKit (issue #1586): the real SDK's events, as this
//! crate's pump passes them on, and the helper's own teardown when the server takes the
//! share's grant back. Everything else about the share is tested against the fake SDK; this
//! is the one test of what livekit 0.9.4 really reports, which the fake is built to match.
//!
//! It needs Docker and the pinned `livekit/livekit-server` image (the `Image` of
//! `e2e/internal/lkit/kit.go`, pulled if missing), `curl` for LiveKit's room API and
//! `python3` to sign its tokens (the standard library only), and the ports 7880 to 7882 on
//! loopback. So it is ignored: run it with `cargo test -p conch-voice --test live_share --
//! --ignored`. No microphone, no PipeWire (the share's sound is the synthetic test source),
//! no conchd.
#![cfg(target_os = "linux")]
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::process::Command;
use std::time::Duration;

use conch_voice::livekit::{LiveKit, LiveKitRoom, Subscription};
use conch_voice::sdk::{RoomHandle, SdkEvent, Transport};
use conch_voice::secrets::Scrubber;
use conch_voice::share_audio::{Link, ShareAudio, ShareRequest, ShareView};
use conch_voice::share_capture::{SyntheticSpec, synthetic_source};
use conch_voice_api::{Secret, VoiceShareAudioScope};
use tokio::sync::mpsc;
use tokio::time::Instant;

/// The image `e2e/internal/lkit` pins, by digest.
const IMAGE: &str = "livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3";
const CONTAINER: &str = "conch-voice-live-share-test";
const KEY: &str = "devkey";
const SECRET: &str = "conch-voice-live-share-test-secret-0001";
const URL: &str = "ws://127.0.0.1:7880";
const API: &str = "http://127.0.0.1:7880/twirp/livekit.RoomService";
const ROOM: &str = "live-share";
const IDENTITY: &str = "p1";

/// The LiveKit container, stopped (and so removed) when the test ends however it ends.
struct Server;

impl Server {
    fn start() -> Self {
        let _ = Command::new("docker")
            .args(["rm", "-f", CONTAINER])
            .output();
        let keys = format!("{KEY}: {SECRET}");
        let started = Command::new("docker")
            .args([
                "run",
                "-d",
                "--rm",
                "--name",
                CONTAINER,
                "--network",
                "host",
                IMAGE,
            ])
            .args([
                "--dev",
                "--bind",
                "127.0.0.1",
                "--node-ip",
                "127.0.0.1",
                "--keys",
                &keys,
            ])
            .output()
            .expect("docker runs");
        assert!(
            started.status.success(),
            "the LiveKit container did not start: {}",
            String::from_utf8_lossy(&started.stderr)
        );
        let server = Server;
        let deadline = std::time::Instant::now() + Duration::from_secs(30);
        loop {
            let up = Command::new("curl")
                .args([
                    "-s",
                    "-o",
                    "/dev/null",
                    "-w",
                    "%{http_code}",
                    "http://127.0.0.1:7880/",
                ])
                .output()
                .is_ok_and(|out| out.stdout == b"200");
            if up {
                return server;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "LiveKit did not come up"
            );
            std::thread::sleep(Duration::from_millis(200));
        }
    }
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = Command::new("docker").args(["stop", CONTAINER]).output();
    }
}

/// A LiveKit token with these video grants, HS256 with the test's key.
fn token(identity: &str, grants: &str) -> String {
    const SIGN: &str = r#"
import base64, hashlib, hmac, json, sys, time
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
key, secret, identity, grants = sys.argv[1:5]
now = int(time.time())
head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
body = b64(json.dumps({"iss": key, "sub": identity, "nbf": now - 10, "exp": now + 600, "video": json.loads(grants)}).encode())
sig = hmac.new(secret.encode(), f"{head}.{body}".encode(), hashlib.sha256).digest()
print(f"{head}.{body}.{b64(sig)}")
"#;
    let out = Command::new("python3")
        .args(["-I", "-c", SIGN, KEY, SECRET, identity, grants])
        .output()
        .expect("python3 runs");
    assert!(
        out.status.success(),
        "{}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8(out.stdout).unwrap().trim().to_owned()
}

/// One call of LiveKit's room API, with an admin token for the room.
fn api(method: &str, body: &str) -> serde_json::Value {
    let admin = token(
        "admin",
        &format!(r#"{{"roomAdmin": true, "roomList": true, "room": "{ROOM}"}}"#),
    );
    let out = Command::new("curl")
        .args(["-s", "-X", "POST", "-H", "Content-Type: application/json"])
        .args(["-H", &format!("Authorization: Bearer {admin}")])
        .args(["--data", body, &format!("{API}/{method}")])
        .output()
        .expect("curl runs");
    serde_json::from_slice(&out.stdout).unwrap_or_default()
}

/// The sources of the tracks the server holds for the participant.
fn server_sources() -> Vec<String> {
    let listed = api("ListParticipants", &format!(r#"{{"room": "{ROOM}"}}"#));
    listed["participants"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|p| p["identity"] == IDENTITY)
        .flat_map(|p| p["tracks"].as_array().cloned().unwrap_or_default())
        .map(|t| t["source"].as_str().unwrap_or_default().to_owned())
        .collect()
}

/// Sets the participant's publish sources, as conchd does when a share's grant changes.
fn set_sources(sources: &str) {
    let answer = api(
        "UpdateParticipant",
        &format!(
            r#"{{"room": "{ROOM}", "identity": "{IDENTITY}", "permission": {{"can_subscribe": true, "can_publish": true, "can_publish_data": true, "can_publish_sources": [{sources}]}}}}"#
        ),
    );
    assert_eq!(answer["identity"], IDENTITY, "UpdateParticipant: {answer}");
}

/// Runs the share until `done`, at most ten seconds.
async fn settle(
    share: &mut ShareAudio<LiveKitRoom>,
    link: &Link<'_, LiveKitRoom>,
    what: &str,
    done: impl Fn(&ShareAudio<LiveKitRoom>) -> bool,
) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !done(share) {
        match tokio::time::timeout_at(deadline, share.wake()).await {
            Ok(wake) => share.on_wake(wake, link),
            Err(_) => panic!("{what}: not within ten seconds"),
        }
    }
}

/// The next permission change the pump passes on, at most ten seconds away.
async fn next_permission(events: &mut mpsc::UnboundedReceiver<SdkEvent>) -> bool {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match tokio::time::timeout_at(deadline, events.recv()).await {
            Ok(Some(SdkEvent::PermissionChanged { allowed })) => return allowed,
            Ok(Some(_)) => {}
            Ok(None) | Err(_) => panic!("no permission change was passed on"),
        }
    }
}

/// Published on its owner's command; the server takes the share's grant back, which the real
/// SDK reports only as a permission change (the server drops the track, the client keeps its
/// publication); the pump passes it on saying not allowed, and the helper unpublishes the
/// track itself and says off. Given again, the pump says allowed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs Docker and the pinned livekit-server image, curl and python3, and the ports 7880 to 7882 on loopback"]
async fn the_real_sdk_reports_a_grant_taken_back_and_the_helper_takes_its_track_back() {
    let _server = Server::start();
    let participant = token(
        IDENTITY,
        &format!(
            r#"{{"roomJoin": true, "room": "{ROOM}", "canPublish": true, "canSubscribe": true, "canPublishSources": ["microphone", "screen_share_audio"]}}"#
        ),
    );
    let (room, mut events) = LiveKit::new(Scrubber::new())
        .connect(URL, &Secret::new(participant), &Subscription::new(None))
        .await
        .expect("joined the room");
    let spec: SyntheticSpec = "tone:440".parse().unwrap();
    let mut share: ShareAudio<LiveKitRoom> = ShareAudio::new(synthetic_source(&spec));
    let link = Link {
        connected: true,
        companion: true,
        room: Some(&room),
    };
    share.request(
        ShareRequest {
            capturing: true,
            scope: Some(VoiceShareAudioScope::SystemExcludingCall),
            application_name: Some("Conch".to_owned()),
        },
        &link,
    );
    settle(&mut share, &link, "on", |share| {
        share.view() == ShareView::on(1)
    })
    .await;
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    while !server_sources().iter().any(|s| s == "SCREEN_SHARE_AUDIO") {
        assert!(
            std::time::Instant::now() < deadline,
            "the server never held the share's track"
        );
        std::thread::sleep(Duration::from_millis(100));
    }

    // conchd takes the share's grant back: the microphone only.
    set_sources(r#""MICROPHONE""#);
    let allowed = next_permission(&mut events).await;
    assert!(!allowed, "the pump said the share is still allowed");
    share.permission_changed(allowed);
    settle(&mut share, &link, "off", |share| {
        share.view() == ShareView::OFF
    })
    .await;
    assert_eq!(
        share
            .counters()
            .open
            .load(std::sync::atomic::Ordering::SeqCst),
        0
    );
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    while server_sources().iter().any(|s| s == "SCREEN_SHARE_AUDIO") {
        assert!(
            std::time::Instant::now() < deadline,
            "the server still holds the share's track"
        );
        std::thread::sleep(Duration::from_millis(100));
    }

    // Given again: the pump says so.
    set_sources(r#""MICROPHONE", "SCREEN_SHARE_AUDIO""#);
    assert!(
        next_permission(&mut events).await,
        "the pump said not allowed after the re-grant"
    );

    share.leave();
    share.until_closed(Duration::from_secs(1)).await;
    room.close().await;
}
