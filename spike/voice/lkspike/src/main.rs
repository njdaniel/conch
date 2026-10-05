//! lkspike: throwaway spike for conch issue #87. Not for merge.
//!
//! Usage: lkspike <tone|listen|multi|talk> key=value ...
//!   url=ws://127.0.0.1:7880  token=<jwt>  tokens=<jwt,jwt,..> (multi)
//!   secs=10  freq=440  lead=2 (seconds of silence before the tone)
//!   perm=id1,id2 (tone: publisher-side allow list)  auto=1|0  force=1 (listen: ask for every track)
//!   ptt=always|/dev/input/eventN:KEYCODE  apm=1  play=1 (talk)

use std::collections::HashMap;
use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use futures::StreamExt;
use livekit::options::TrackPublishOptions;
use livekit::prelude::*;
use livekit::webrtc::audio_frame::AudioFrame;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use livekit::webrtc::native::apm::AudioProcessingModule;

const RATE: u32 = 48_000;
const FRAME: usize = 480; // 10 ms mono

fn now_ms() -> u128 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis()
}

fn rms(data: &[i16]) -> f64 {
    if data.is_empty() {
        return 0.0;
    }
    (data.iter().map(|s| (*s as f64).powi(2)).sum::<f64>() / data.len() as f64).sqrt()
}

struct Args(HashMap<String, String>);
impl Args {
    fn get(&self, k: &str, d: &str) -> String {
        self.0.get(k).cloned().unwrap_or_else(|| d.to_string())
    }
    fn num(&self, k: &str, d: f64) -> f64 {
        self.0.get(k).and_then(|v| v.parse().ok()).unwrap_or(d)
    }
    fn flag(&self, k: &str) -> bool {
        self.get(k, "0") == "1"
    }
}

async fn publish(room: &Room, name: &str) -> Result<(NativeAudioSource, TrackSid)> {
    let source = NativeAudioSource::new(AudioSourceOptions::default(), RATE, 1, 100);
    let track = LocalAudioTrack::create_audio_track(name, RtcAudioSource::Native(source.clone()));
    let publication = room
        .local_participant()
        .publish_track(
            LocalTrack::Audio(track),
            TrackPublishOptions { source: TrackSource::Microphone, ..Default::default() },
        )
        .await?;
    Ok((source, publication.sid()))
}

/// Counts frames and loudness for every audio track the server lets us have.
struct Counter {
    frames: AtomicU64,
    loud: AtomicU64,
    first_loud_ms: AtomicU64,
}

fn spawn_counter(track: RemoteAudioTrack, counter: Arc<Counter>, sink: Option<std::sync::mpsc::Sender<Vec<i16>>>) {
    tokio::spawn(async move {
        let mut stream = NativeAudioStream::new(track.rtc_track(), RATE as i32, 1);
        while let Some(frame) = stream.next().await {
            counter.frames.fetch_add(1, Ordering::Relaxed);
            if rms(&frame.data) > 500.0 {
                counter.loud.fetch_add(1, Ordering::Relaxed);
                let _ = counter.first_loud_ms.compare_exchange(0, now_ms() as u64, Ordering::Relaxed, Ordering::Relaxed);
            }
            if let Some(tx) = &sink {
                let _ = tx.send(frame.data.to_vec());
            }
        }
    });
}

async fn tone(a: &Args) -> Result<()> {
    let (room, mut events) = Room::connect(&a.get("url", "ws://127.0.0.1:7880"), &a.get("token", ""), RoomOptions::default()).await?;
    let me = room.local_participant().identity().to_string();
    let perm = a.get("perm", "");
    if !perm.is_empty() {
        let allowed = perm
            .split(',')
            .map(|id| livekit::participant::ParticipantTrackPermission { participant_identity: id.to_string().into(), allow_all: true, allowed_track_sids: vec![] })
            .collect();
        room.local_participant().set_track_subscription_permissions(false, allowed).await?;
        println!("{me}: publisher allow-list = [{perm}]");
    }
    let (source, sid) = publish(&room, "tone").await?;
    println!("{me}: PUBLISHED room={} track={sid}", room.name());
    tokio::spawn(async move { while events.recv().await.is_some() {} });

    let (secs, lead, freq) = (a.num("secs", 10.0), a.num("lead", 0.0), a.num("freq", 440.0));
    let start = Instant::now();
    let mut n: u64 = 0;
    let mut announced = false;
    let mut tick = tokio::time::interval(Duration::from_millis(10));
    while start.elapsed().as_secs_f64() < secs {
        tick.tick().await;
        let on = start.elapsed().as_secs_f64() >= lead;
        let data: Vec<i16> = (0..FRAME)
            .map(|i| {
                if !on {
                    return 0;
                }
                let t = (n + i as u64) as f64 / RATE as f64;
                ((t * freq * std::f64::consts::TAU).sin() * 8000.0) as i16
            })
            .collect();
        if on && !announced {
            println!("{me}: TONE_ON {}", now_ms());
            announced = true;
        }
        n += FRAME as u64;
        source.capture_frame(&AudioFrame { data: data.into(), sample_rate: RATE, num_channels: 1, samples_per_channel: FRAME as u32 }).await?;
    }
    room.close().await?;
    Ok(())
}

async fn listen_room(url: String, token: String, auto: bool, force: bool, secs: f64) -> Result<String> {
    let t0 = Instant::now();
    let (room, mut events) = Room::connect(&url, &token, { let mut o = RoomOptions::default(); o.auto_subscribe = auto; o }).await?;
    let connect_ms = t0.elapsed().as_millis();
    let me = room.local_participant().identity().to_string();
    let rname = room.name();
    let counter = Arc::new(Counter { frames: 0.into(), loud: 0.into(), first_loud_ms: 0.into() });
    let (mut subscribed, mut failed) = (0, 0);
    if force {
        for (_, p) in room.remote_participants() {
            for (_, publication) in p.track_publications() {
                publication.set_subscribed(true);
            }
        }
    }
    let deadline = tokio::time::sleep(Duration::from_secs_f64(secs));
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            _ = &mut deadline => break,
            ev = events.recv() => match ev {
                Some(RoomEvent::TrackPublished { publication, participant }) => {
                    println!("{me}@{rname}: sees track {} from {}", publication.sid(), participant.identity());
                    if force { publication.set_subscribed(true); println!("{me}@{rname}: requested subscribe"); }
                }
                Some(RoomEvent::TrackSubscribed { track: RemoteTrack::Audio(t), participant, .. }) => {
                    subscribed += 1;
                    println!("{me}@{rname}: SUBSCRIBED to {}", participant.identity());
                    spawn_counter(t, counter.clone(), None);
                }
                Some(RoomEvent::TrackSubscriptionFailed { error, track_sid, .. }) => {
                    failed += 1;
                    println!("{me}@{rname}: SUBSCRIPTION FAILED {track_sid}: {error:?}");
                }
                Some(_) => {}
                None => break,
            }
        }
    }
    room.close().await?;
    Ok(format!(
        "RESULT identity={me} room={rname} connect_ms={connect_ms} subscribed={subscribed} failed={failed} frames={} loud_frames={} first_loud_ms={}",
        counter.frames.load(Ordering::Relaxed),
        counter.loud.load(Ordering::Relaxed),
        counter.first_loud_ms.load(Ordering::Relaxed)
    ))
}

fn self_stats() -> String {
    let status = std::fs::read_to_string("/proc/self/status").unwrap_or_default();
    let rss = status.lines().find(|l| l.starts_with("VmRSS")).unwrap_or("").replace('\t', " ");
    let stat = std::fs::read_to_string("/proc/self/stat").unwrap_or_default();
    let f: Vec<&str> = stat.rsplit(')').next().unwrap_or("").split_whitespace().collect();
    let ticks: u64 = f.get(11).and_then(|v| v.parse().ok()).unwrap_or(0) + f.get(12).and_then(|v| v.parse().ok()).unwrap_or(0);
    let threads = status.lines().find(|l| l.starts_with("Threads")).unwrap_or("").replace('\t', " ");
    format!("{rss} | {threads} | cpu_ticks={ticks} (100/s)")
}

async fn multi(a: &Args) -> Result<()> {
    let url = a.get("url", "ws://127.0.0.1:7880");
    let secs = a.num("secs", 10.0);
    let mut tasks = vec![];
    for token in a.get("tokens", "").split(',').filter(|t| !t.is_empty()) {
        tasks.push(tokio::spawn(listen_room(url.clone(), token.to_string(), true, false, secs)));
    }
    let n = tasks.len();
    for t in tasks {
        println!("{}", t.await??);
    }
    println!("MULTI rooms={n} secs={secs} {}", self_stats());
    Ok(())
}

/// Watches one evdev key and mirrors its state into `held`. Reads the raw
/// input_event struct (24 bytes on x86_64) so the spike needs no extra crate.
fn watch_key(spec: &str, held: Arc<AtomicBool>) -> Result<()> {
    let (path, code) = spec.rsplit_once(':').ok_or_else(|| anyhow!("ptt=/dev/input/eventN:KEYCODE"))?;
    let code: u16 = code.parse()?;
    let mut f = std::fs::File::open(path).map_err(|e| anyhow!("open {path}: {e} (evdev needs read access, usually the 'input' group)"))?;
    std::thread::spawn(move || {
        let mut buf = [0u8; 24];
        while f.read_exact(&mut buf).is_ok() {
            let (ty, c, val) = (u16::from_ne_bytes([buf[16], buf[17]]), u16::from_ne_bytes([buf[18], buf[19]]), i32::from_ne_bytes([buf[20], buf[21], buf[22], buf[23]]));
            if ty == 1 && c == code && val != 2 {
                held.store(val == 1, Ordering::Relaxed);
                println!("PTT {} at {}", if val == 1 { "DOWN" } else { "UP" }, now_ms());
            }
        }
    });
    Ok(())
}

async fn talk(a: &Args) -> Result<()> {
    let (room, mut events) = Room::connect(&a.get("url", "ws://127.0.0.1:7880"), &a.get("token", ""), RoomOptions::default()).await?;
    let me = room.local_participant().identity().to_string();
    let (source, sid) = publish(&room, "mic").await?;
    println!("{me}: PUBLISHED mic track={sid}");

    let held = Arc::new(AtomicBool::new(false));
    let ptt = a.get("ptt", "always");
    if ptt == "always" {
        held.store(true, Ordering::Relaxed);
    } else {
        watch_key(&ptt, held.clone())?;
    }

    // Playback: every remote frame goes to one pw-play process (naive sum-free
    // hand-off; real mixing is V4 work).
    let (ptx, prx) = std::sync::mpsc::channel::<Vec<i16>>();
    let (reftx, refrx) = std::sync::mpsc::channel::<Vec<i16>>();
    let use_apm = a.flag("apm");
    if a.get("play", "1") == "1" {
        let mut child = Command::new("pw-play").args(["--raw", "--rate", "48000", "--channels", "1", "--format", "s16", "--latency", "20ms", "--volume", &a.get("vol", "1.0"), "-"]).stdin(Stdio::piped()).spawn()?;
        let mut stdin = child.stdin.take().unwrap();
        std::thread::spawn(move || {
            for frame in prx {
                if use_apm {
                    let _ = reftx.send(frame.clone());
                }
                let bytes: Vec<u8> = frame.iter().flat_map(|s| s.to_le_bytes()).collect();
                if stdin.write_all(&bytes).is_err() {
                    break;
                }
            }
        });
    }

    // Capture: pw-record -> 10 ms frames -> (optional APM) -> LiveKit, gated by PTT.
    let (ctx, mut crx) = tokio::sync::mpsc::channel::<Vec<i16>>(50);
    let mut rec = Command::new("pw-record").args(["--raw", "--rate", "48000", "--channels", "1", "--format", "s16", "--latency", "10ms", "-"]).stdout(Stdio::piped()).spawn()?;
    let mut out = rec.stdout.take().unwrap();
    std::thread::spawn(move || {
        let mut buf = [0u8; FRAME * 2];
        while out.read_exact(&mut buf).is_ok() {
            let frame: Vec<i16> = buf.chunks_exact(2).map(|b| i16::from_le_bytes([b[0], b[1]])).collect();
            if ctx.blocking_send(frame).is_err() {
                break;
            }
        }
    });

    let counter = Arc::new(Counter { frames: 0.into(), loud: 0.into(), first_loud_ms: 0.into() });
    let secs = a.num("secs", 30.0);
    let deadline = tokio::time::sleep(Duration::from_secs_f64(secs));
    tokio::pin!(deadline);
    let mut apm = use_apm.then(|| AudioProcessingModule::new(true, true, true, true));
    let (mut sent, mut gated) = (0u64, 0u64);
    loop {
        tokio::select! {
            _ = &mut deadline => break,
            Some(mut frame) = crx.recv() => {
                if let Some(apm) = apm.as_mut() {
                    while let Ok(mut far) = refrx.try_recv() {
                        if far.len() == FRAME { let _ = apm.process_reverse_stream(&mut far, RATE as i32, 1); }
                    }
                    apm.process_stream(&mut frame, RATE as i32, 1).map_err(|e| anyhow!("apm: {e:?}"))?;
                }
                if held.load(Ordering::Relaxed) {
                    sent += 1;
                    source.capture_frame(&AudioFrame { data: frame.into(), sample_rate: RATE, num_channels: 1, samples_per_channel: FRAME as u32 }).await?;
                } else {
                    gated += 1;
                }
            }
            ev = events.recv() => match ev {
                Some(RoomEvent::TrackSubscribed { track: RemoteTrack::Audio(t), participant, .. }) => {
                    println!("{me}: hearing {}", participant.identity());
                    spawn_counter(t, counter.clone(), Some(ptx.clone()));
                }
                Some(_) => {}
                None => break,
            }
        }
    }
    println!("RESULT identity={me} mic_frames_sent={sent} mic_frames_gated={gated} remote_frames={} apm={use_apm} {}", counter.frames.load(Ordering::Relaxed), self_stats());
    let _ = rec.kill();
    room.close().await?;
    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    let mut argv = std::env::args().skip(1);
    let mode = argv.next().unwrap_or_default();
    let a = Args(argv.filter_map(|kv| kv.split_once('=').map(|(k, v)| (k.to_string(), v.to_string()))).collect());
    match mode.as_str() {
        "tone" => tone(&a).await,
        "listen" => {
            let r = listen_room(a.get("url", "ws://127.0.0.1:7880"), a.get("token", ""), a.get("auto", "1") == "1", a.flag("force"), a.num("secs", 10.0)).await?;
            println!("{r}");
            Ok(())
        }
        "multi" => multi(&a).await,
        "talk" => talk(&a).await,
        _ => Err(anyhow!("usage: lkspike <tone|listen|multi|talk> key=value ...")),
    }
}
