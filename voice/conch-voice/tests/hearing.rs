//! What the client hears and shows: each remote speaker through a jitter buffer of their
//! own into one mix, the per-speaker counts `--json` reports, and presence from `conchd`'s
//! socket (`docs/design/conch-voice.md` §3 and §7).

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice_audio::{Frame, MicSource, SILENCE, ToneSource};
use conch_voice_control::LineCommand::Deafen;
use serde_json::{Value, json};
use tokio::sync::mpsc;

use support::stub::{OWN_IDENTITY, Stub, presence};
use support::{Rig, Setup};

/// Sends a tone as one speaker's audio, a frame every 10 ms, until the test ends.
fn talk(frames: mpsc::Sender<Frame>, hz: f32) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut tone = ToneSource::new(hz, 0.25).unwrap();
        let mut tick = tokio::time::interval(Duration::from_millis(10));
        loop {
            tick.tick().await;
            let mut frame = SILENCE;
            tone.read(&mut frame);
            if frames.send(frame).await.is_err() {
                return;
            }
        }
    })
}

/// The latest `stats` object that has an entry for `speaker`, and that entry.
fn heard(rig: &Rig, speaker: &str) -> Option<(Value, Value)> {
    rig.events_named("stats")
        .into_iter()
        .rev()
        .find_map(|stats| {
            let entry = stats["speakers"]
                .as_array()
                .unwrap()
                .iter()
                .find(|entry| entry["speaker"] == speaker)
                .cloned()?;
            Some((stats, entry))
        })
}

#[tokio::test]
async fn each_speaker_is_counted_by_their_own_tone_and_the_mix_holds_them_all() {
    let rig = Rig::start().await;
    rig.connected(1).await;
    let (_, first) = rig.sdk.speaker("p3");
    let (_, second) = rig.sdk.speaker("p4");
    let _talking = [talk(first, 440.0), talk(second, 880.0)];

    // A reporting interval in which both were talking throughout and both were in the mix
    // (the first 40 ms of each go to filling their jitter buffer).
    rig.until("both speakers to be heard", |rig| {
        ["p3", "p4"].iter().all(|speaker| {
            heard(rig, speaker).is_some_and(|(stats, entry)| {
                entry["frames"].as_u64().unwrap() >= 3
                    && entry["frames"] == entry["audible_frames"]
                    && stats["mix"]["audible_frames"].as_u64().unwrap() >= 3
            })
        })
    })
    .await;
    let (_, p3) = heard(&rig, "p3").unwrap();
    let (stats, p4) = heard(&rig, "p4").unwrap();
    assert_eq!(p3["dominant_hz"], 440.0);
    assert_eq!(p4["dominant_hz"], 880.0);
    for entry in [&p3, &p4] {
        // A sine at a quarter of full scale.
        let rms = entry["rms"].as_f64().unwrap();
        assert!((rms - 0.25 / 2f64.sqrt()).abs() < 0.01, "{entry}");
        assert!(entry["frames_total"].as_u64().unwrap() >= entry["frames"].as_u64().unwrap());
    }
    assert!(stats["mix"]["audible_frames"].as_u64().unwrap() > 0);
    assert!(stats["mix"]["rms"].as_f64().unwrap() > 0.1, "{stats}");
    assert_eq!(stats["frames_sent"], 0, "this client is not transmitting");

    let tracks = rig.events_named("track");
    assert_eq!(
        tracks,
        [
            json!({"event": "track", "speaker": "p3", "state": "subscribed"}),
            json!({"event": "track", "speaker": "p4", "state": "subscribed"}),
        ]
    );
}

#[tokio::test]
async fn a_speaker_who_stops_is_counted_as_silence_and_one_who_leaves_is_dropped() {
    let rig = Rig::start().await;
    rig.connected(1).await;
    let (track, frames) = rig.sdk.speaker("p3");
    let talking = talk(frames, 440.0);
    rig.until("p3 to be heard", |rig| {
        heard(rig, "p3").is_some_and(|(_, entry)| entry["dominant_hz"] == 440.0)
    })
    .await;

    // The speaker's frames stop arriving (their gate shut): nothing audible is counted,
    // and once their jitter buffer has run out the mix is silent.
    talking.abort();
    rig.until("a quiet interval", |rig| {
        heard(rig, "p3").is_some_and(|(stats, entry)| {
            entry["frames"] == 0
                && entry["audible_frames"] == 0
                && entry["dominant_hz"].is_null()
                && stats["mix"]["audible_frames"] == 0
        })
    })
    .await;

    // The track goes: the speaker is reported no more.
    let unsubscribed = conch_voice::sdk::SdkEvent::TrackUnsubscribed { track };
    assert!(rig.sdk.emit(unsubscribed));
    rig.until("p3 to be dropped", |rig| {
        let stats = rig.events_named("stats").pop().unwrap();
        stats["speakers"] == json!([])
    })
    .await;
    assert_eq!(
        rig.events_named("track").pop().unwrap(),
        json!({"event": "track", "speaker": "p3", "state": "unsubscribed"})
    );
}

#[tokio::test]
async fn the_clients_own_audio_is_never_in_its_own_mix() {
    let rig = Rig::start().await;
    rig.connected(1).await;
    // A track under this client's own identity, which LiveKit would not send; if it ever
    // did, it is refused.
    let (_, own) = rig.sdk.speaker(OWN_IDENTITY);
    let (_, other) = rig.sdk.speaker("p3");
    let _talking = [talk(own, 880.0), talk(other, 440.0)];

    rig.until("p3 to be heard", |rig| {
        heard(rig, "p3").is_some_and(|(_, entry)| entry["frames"].as_u64().unwrap() >= 3)
    })
    .await;
    for stats in rig.events_named("stats") {
        for entry in stats["speakers"].as_array().unwrap() {
            assert_eq!(entry["speaker"], "p3", "only the other speaker: {stats}");
        }
        let dominant = &stats["mix"]["dominant_hz"];
        assert!(
            dominant.is_null() || *dominant == 440.0,
            "the mix holds p3's tone and never this client's own: {stats}"
        );
    }
    assert_eq!(rig.events_named("track").len(), 1);
}

#[tokio::test]
async fn deafening_silences_the_mix_while_the_speakers_are_still_counted() {
    let rig = Rig::start().await;
    rig.ready().await;
    let (_, frames) = rig.sdk.speaker("p3");
    let _talking = talk(frames, 440.0);
    rig.until("p3 in the mix", |rig| {
        rig.events_named("stats")
            .pop()
            .is_some_and(|stats| stats["mix"]["dominant_hz"] == 440.0)
    })
    .await;

    rig.line(Deafen);
    rig.until("a silent mix", |rig| {
        let stats = rig.events_named("stats").pop().unwrap();
        stats["mix"]["audible_frames"] == 0 && stats["mix"]["frames"].as_u64().unwrap() > 0
    })
    .await;
    let (_, entry) = heard(&rig, "p3").unwrap();
    assert!(
        entry["audible_frames"].as_u64().unwrap() > 0,
        "still received"
    );
    assert_eq!(rig.own()["deafened"], true);
}

#[tokio::test]
async fn presence_is_shown_from_conchds_socket_as_principal_ids() {
    let conchd = Stub::start().await;
    conchd.presence(presence(&[(3, false), (7, false)]));
    let rig = Rig::start_with(conchd, Setup::default(), |_| {}).await;
    rig.until("the first roster", |rig| {
        !rig.events_named("presence").is_empty()
    })
    .await;
    let first = &rig.events_named("presence")[0];
    assert_eq!(first["known"], true);
    assert_eq!(
        first["participants"],
        json!([
            {"id": "p3", "principal_id": 3, "can_publish": true, "transmitting": false},
            {"id": "p7", "principal_id": 7, "can_publish": true, "transmitting": false},
        ])
    );
    let request = rig
        .conchd
        .requests()
        .into_iter()
        .find(|request| request.method == "GET")
        .unwrap();
    assert_eq!(request.target, "/v1/voice/ws?channel=ops");

    // Someone starts talking: a new line. The same document again: no line.
    rig.conchd.presence(presence(&[(3, true), (7, false)]));
    rig.until("the change", |rig| rig.events_named("presence").len() == 2)
        .await;
    assert_eq!(
        rig.events_named("presence")[1]["participants"][0]["transmitting"],
        true
    );
    rig.conchd.presence(presence(&[(3, true), (7, false)]));
    rig.conchd.presence(presence(&[(7, false)]));
    rig.until("the next change", |rig| {
        rig.events_named("presence").len() == 3
    })
    .await;
    assert_eq!(
        rig.events_named("presence")[2]["participants"],
        json!([{"id": "p7", "principal_id": 7, "can_publish": true, "transmitting": false}])
    );
}

#[tokio::test]
async fn in_plain_mode_presence_is_one_line_per_change() {
    let conchd = Stub::start().await;
    conchd.presence(presence(&[(3, true), (7, false)]));
    let setup = Setup {
        json: false,
        ..Setup::default()
    };
    let rig = Rig::start_with(conchd, setup, |_| {}).await;
    let line = "in voice: p3 (talking), p7";
    support::until(
        "the roster",
        || rig.out.text(),
        || rig.out.text().lines().any(|written| written == line),
    )
    .await;
    // Plain mode has no stats lines, however long it runs.
    tokio::time::sleep(Duration::from_millis(120)).await;
    assert!(!rig.out.text().contains("stats"), "{}", rig.out.text());
}
