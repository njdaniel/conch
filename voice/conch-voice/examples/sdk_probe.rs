//! `sdk_probe`: reports what LiveKit's Rust SDK does in the situations the voice client's
//! design relies on (issue #178; `docs/design/conch-voice.md` §13, rows 1 to 4, 10 and 12).
//!
//! Input: a mode on the command line (`publish`, `listen` or `duplex`) and options; the
//! LiveKit address in `SDK_PROBE_URL`; the join token in `SDK_PROBE_TOKEN` (never an
//! argument: arguments show in process lists); optionally the room's name in
//! `SDK_PROBE_ROOM`, used only to search captured log records for it; and one command per
//! line on standard input (`unmute`, `mute`, `mark`, `audio`, `rtp`, `quit`).
//! Output: one JSON object per line on standard output, each with `ev` and `t_ms`, a
//! monotonic time in milliseconds since the process started. The first and last lines also
//! carry the wall clock, so a reader can put several probes on one timeline. A line that is
//! not JSON is not the probe's: native code the SDK links can write to standard output.
//! It never prints a token or a room name: every string that came from the SDK goes through
//! [`Secrets::redact`] first, and the log search reports counts and places, never a record.
//!
//! It needs a LiveKit server, so it is an example and not a test. `go run ./e2e/sdkprobe`
//! starts the server, mints the tokens, arranges each situation and reads these lines.

use std::borrow::Cow;
use std::collections::BTreeMap;
use std::fmt;
use std::io::{BufRead, Write};
use std::process::ExitCode;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, PoisonError};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use conch_voice_audio::{FRAME_LEN, SAMPLE_RATE};
use futures_util::StreamExt;
use livekit::options::TrackPublishOptions;
use livekit::prelude::*;
use livekit::webrtc::audio_frame::AudioFrame;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio::time::MissedTickBehavior;

/// Peak of the tone a publisher feeds its audio source, muted or not, unless `--amp` says otherwise.
const TONE_AMP: f64 = 8000.0;
/// Peak of the louder burst a `mark` command puts in the tone for [`MARK_FRAMES`] frames.
const MARK_AMP: f64 = 24000.0;
const MARK_FRAMES: u32 = 20;
/// A received frame is "loud" above this RMS. The tone's RMS is about 5700.
const LOUD_RMS: f64 = 500.0;
/// A received frame is part of a mark above this RMS (a mark's RMS is about 17000).
const MARK_RMS: f64 = 11000.0;
/// How long the probe waits for `Room::close` before it reports that it did not return.
const CLOSE_WAIT: Duration = Duration::from_secs(60);
/// No frame from a subscribed track for this long is reported as a stall.
const STALL: Duration = Duration::from_millis(200);

// ------------------------------------------------------------------ output

static START: OnceLock<Instant> = OnceLock::new();

/// Milliseconds since the process started, from the monotonic clock.
fn now_ms() -> f64 {
    let micros = START.get_or_init(Instant::now).elapsed().as_micros();
    micros as f64 / 1000.0
}

fn unix_us() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as u64)
        .unwrap_or(0)
}

/// Writes one event that happened at `t_ms`. A reader that has gone away ends the probe.
fn emit_at(t_ms: f64, ev: &str, mut fields: Value) {
    if let Value::Object(map) = &mut fields {
        map.insert("t_ms".into(), json!((t_ms * 1000.0).round() / 1000.0));
        map.insert("ev".into(), json!(ev));
    }
    let mut out = std::io::stdout().lock();
    if writeln!(out, "{fields}")
        .and_then(|()| out.flush())
        .is_err()
    {
        std::process::exit(3);
    }
}

fn emit(ev: &str, fields: Value) {
    emit_at(now_ms(), ev, fields);
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

// ----------------------------------------------------------------- secrets

/// The values that must never be printed, each with the name it is replaced by.
struct Secrets {
    known: Mutex<Vec<(String, String)>>,
}

static SECRETS: Secrets = Secrets {
    known: Mutex::new(Vec::new()),
};

impl Secrets {
    fn add(&self, name: &str, value: &str) {
        if value.is_empty() {
            return;
        }
        let mut known = lock(&self.known);
        if !known.iter().any(|(_, v)| v == value) {
            known.push((name.to_owned(), value.to_owned()));
            // Longest first, so a whole token is replaced before one of its segments.
            known.sort_by_key(|(_, v)| std::cmp::Reverse(v.len()));
        }
    }

    fn snapshot(&self) -> Vec<(String, String)> {
        lock(&self.known).clone()
    }

    /// `text` with every known secret, and anything shaped like a JWT, replaced.
    fn redact(&self, text: &str) -> String {
        let mut out = text.to_owned();
        for (name, value) in self.snapshot() {
            out = out.replace(&value, &format!("[{name}]"));
        }
        let mut cleaned = String::with_capacity(out.len());
        let mut from = 0;
        for (start, end) in jwt_spans(&out) {
            cleaned.push_str(&out[from..start]);
            cleaned.push_str("[jwt]");
            from = end;
        }
        cleaned.push_str(&out[from..]);
        cleaned
    }
}

/// Byte ranges of `text` that look like a JWT: three runs of base64url characters joined by
/// dots, the first two starting with `eyJ` (a JSON object, base64-encoded).
fn jwt_spans(text: &str) -> Vec<(usize, usize)> {
    let bytes = text.as_bytes();
    let run = |from: usize| -> usize {
        let mut i = from;
        while i < bytes.len()
            && (bytes[i].is_ascii_alphanumeric() || matches!(bytes[i], b'-' | b'_'))
        {
            i += 1;
        }
        i
    };
    let mut spans = Vec::new();
    let mut i = 0;
    while let Some(found) = text[i..].find("eyJ") {
        let start = i + found;
        let first = run(start);
        i = first.max(start + 3);
        if bytes.get(first) != Some(&b'.') || !text[first + 1..].starts_with("eyJ") {
            continue;
        }
        let second = run(first + 1);
        if bytes.get(second) != Some(&b'.') {
            continue;
        }
        let third = run(second + 1);
        if third > second + 1 {
            spans.push((start, third));
            i = third;
        }
    }
    spans
}

// ------------------------------------------------------------- log capture

struct Captured {
    level: log::Level,
    target: String,
    site: String,
    text: String,
}

/// Keeps every record the `log` facade is given, from every target, in memory.
struct CaptureLog {
    on: AtomicBool,
    records: Mutex<Vec<Captured>>,
}

static LOGGER: CaptureLog = CaptureLog {
    on: AtomicBool::new(false),
    records: Mutex::new(Vec::new()),
};

impl log::Log for CaptureLog {
    fn enabled(&self, _: &log::Metadata<'_>) -> bool {
        self.on.load(Ordering::Relaxed)
    }

    fn log(&self, record: &log::Record<'_>) {
        if !self.on.load(Ordering::Relaxed) {
            return;
        }
        // The source file of the statement, without the part of the path that is this
        // machine's: "livekit-0.9.4/src/room/mod.rs:1593".
        let file = record.file().unwrap_or("?");
        let file = file.split_once("/registry/src/").map_or(file, |(_, rest)| {
            rest.split_once('/').map_or(rest, |(_, in_crate)| in_crate)
        });
        lock(&self.records).push(Captured {
            level: record.level(),
            target: record.target().to_owned(),
            site: format!("{file}:{}", record.line().unwrap_or(0)),
            text: record.args().to_string(),
        });
    }

    fn flush(&self) {}
}

/// Searches everything captured for the join token, its three segments, the room's name and
/// any token LiveKit sent later, and reports per level how many records held each. For a
/// record that held one it gives the level, the target and the statement's place in the
/// source, never the record.
fn log_scan(join_token: &str) -> Value {
    let mut needles: Vec<(String, String)> = Vec::new();
    needles.push(("join_token".into(), join_token.to_owned()));
    for (n, segment) in join_token.split('.').enumerate() {
        let part = ["header", "claims", "signature"]
            .get(n)
            .copied()
            .unwrap_or("extra");
        needles.push((format!("join_token_{part}"), segment.to_owned()));
    }
    for (name, value) in SECRETS.snapshot() {
        match name.as_str() {
            "room_name" => needles.push((name, value)),
            "refreshed_token" => {
                let mut segments = value.split('.').skip(1);
                if let Some(claims) = segments.next() {
                    needles.push(("refreshed_token_claims".into(), claims.to_owned()));
                }
                if let Some(signature) = segments.next() {
                    needles.push(("refreshed_token_signature".into(), signature.to_owned()));
                }
                needles.push((name, value));
            }
            _ => {}
        }
    }
    needles.retain(|(_, v)| !v.is_empty());

    let records = lock(&LOGGER.records);
    let mut bytes = 0usize;
    let mut per_level: BTreeMap<String, BTreeMap<String, u64>> = BTreeMap::new();
    let mut per_target: BTreeMap<String, BTreeMap<String, u64>> = BTreeMap::new();
    let mut hits: BTreeMap<(String, String, String, String), u64> = BTreeMap::new();
    let mut fmtp: BTreeMap<(String, String), u64> = BTreeMap::new();
    for level in ["ERROR", "WARN", "INFO", "DEBUG", "TRACE"] {
        per_level
            .entry(level.into())
            .or_default()
            .insert("records".into(), 0);
    }
    for record in records.iter() {
        bytes += record.text.len();
        if record.target.starts_with("livekit") {
            for line in fmtp_lines(&record.text) {
                *fmtp.entry((record.site.clone(), line)).or_default() += 1;
            }
        }
        let level = record.level.to_string();
        *per_level
            .entry(level.clone())
            .or_default()
            .entry("records".into())
            .or_default() += 1;
        *per_target
            .entry(record.target.clone())
            .or_default()
            .entry(level.clone())
            .or_default() += 1;
        let mut found: Vec<&str> = needles
            .iter()
            .filter(|(_, v)| record.text.contains(v.as_str()) || record.target.contains(v.as_str()))
            .map(|(name, _)| name.as_str())
            .collect();
        if !jwt_spans(&record.text).is_empty() {
            found.push("any_jwt");
        }
        for name in found {
            *per_level
                .entry(level.clone())
                .or_default()
                .entry(name.into())
                .or_default() += 1;
            let key = (
                level.clone(),
                record.target.clone(),
                record.site.clone(),
                name.to_owned(),
            );
            *hits.entry(key).or_default() += 1;
        }
    }
    let hits: Vec<Value> = hits
        .into_iter()
        .map(|((level, target, site, needle), n)| {
            json!({"level": level, "target": target, "site": site, "needle": needle, "records": n})
        })
        .collect();
    let fmtp: Vec<Value> = fmtp
        .into_iter()
        .map(|((site, line), n)| json!({"site": site, "line": line, "records": n}))
        .collect();
    json!({
        "sdp_fmtp": fmtp,
        "records": records.len(),
        "bytes": bytes,
        "searched_for": needles.iter().map(|(name, _)| name.as_str()).chain(["any_jwt"]).collect::<Vec<_>>(),
        "levels": per_level,
        "targets": per_target,
        "hits": hits,
    })
}

/// The `a=fmtp:` lines of any session description in `text`: the codec settings that were
/// offered or answered. They hold no address, key, fingerprint or token.
fn fmtp_lines(text: &str) -> Vec<String> {
    text.split("a=fmtp:")
        .skip(1)
        .map(|rest| {
            let end = rest.find(['\\', '\r', '\n', '"']).unwrap_or(rest.len());
            format!("a=fmtp:{}", &rest[..end])
        })
        .collect()
}

// ------------------------------------------------------------------ config

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Publish,
    Listen,
    Duplex,
}

/// How a publisher's track starts out.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Start {
    /// Published and left unmuted.
    Unmuted,
    /// Muted before it is published, so the request that publishes it says "muted".
    Premute,
    /// As `Premute`, and the track is disabled again once published: the SDK enables every
    /// track at the end of `publish_track`, muted or not.
    PremuteDisable,
    /// Published unmuted, then muted with the next statement.
    Postmute,
    /// Published unmuted, then only the WebRTC track is disabled: LiveKit is not told the
    /// track is muted, so the server forwards whatever the track still sends.
    DisableOnly,
}

struct Config {
    mode: Mode,
    start: Start,
    url: String,
    token: String,
    freq: f64,
    /// Peak of the tone; zero feeds digital silence.
    amp: f64,
    queue_ms: u32,
    stats_secs: u64,
    max_secs: u64,
    capture_logs: bool,
    /// Hand the audio source frames only between an `unmute` and the next `mute`, as a
    /// client with a transmit gate does; otherwise the tone is fed the whole time.
    gated: bool,
    /// Close the room as soon as the SDK reports that it is reconnecting: what an application
    /// that does not want the SDK's own resume would do.
    close_on_reconnecting: bool,
    /// Publish with Opus discontinuous transmission, which is the SDK's default.
    dtx: bool,
    /// When the SDK publishes the track again after a full reconnect and it is muted, disable
    /// the WebRTC track again.
    disable_on_republish: bool,
}

#[derive(Debug)]
enum ProbeError {
    Usage(String),
    Sdk(String),
}

impl fmt::Display for ProbeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Usage(why) => write!(
                f,
                "{why}\nusage: SDK_PROBE_URL=ws://... SDK_PROBE_TOKEN=... sdk_probe <publish|listen|duplex> \
                 [--start=unmuted|premute|premute-disable|postmute|disable-only] [--freq=HZ] [--amp=PEAK] [--queue-ms=N] \
                 [--stats-secs=N] [--max-secs=N] [--capture-logs] [--gated] \
                 [--close-on-reconnecting] [--no-dtx] [--disable-on-republish]"
            ),
            Self::Sdk(why) => write!(f, "{why}"),
        }
    }
}

fn config() -> Result<Config, ProbeError> {
    let usage = |why: &str| ProbeError::Usage(why.to_owned());
    let mut args = std::env::args().skip(1);
    let mode = match args.next().as_deref() {
        Some("publish") => Mode::Publish,
        Some("listen") => Mode::Listen,
        Some("duplex") => Mode::Duplex,
        _ => return Err(usage("the first argument is the mode")),
    };
    let mut cfg = Config {
        mode,
        start: Start::Premute,
        url: std::env::var("SDK_PROBE_URL").map_err(|_| usage("SDK_PROBE_URL is not set"))?,
        token: std::env::var("SDK_PROBE_TOKEN").map_err(|_| usage("SDK_PROBE_TOKEN is not set"))?,
        freq: 440.0,
        amp: TONE_AMP,
        queue_ms: 100,
        stats_secs: 0,
        max_secs: 600,
        capture_logs: false,
        gated: false,
        close_on_reconnecting: false,
        dtx: true,
        disable_on_republish: false,
    };
    for arg in args {
        let (name, value) = arg.split_once('=').unwrap_or((arg.as_str(), ""));
        let number = || {
            value
                .parse::<u32>()
                .map_err(|_| usage(&format!("{name} needs a number")))
        };
        match name {
            "--start" => {
                cfg.start = match value {
                    "unmuted" => Start::Unmuted,
                    "premute" => Start::Premute,
                    "premute-disable" => Start::PremuteDisable,
                    "postmute" => Start::Postmute,
                    "disable-only" => Start::DisableOnly,
                    _ => return Err(usage("unknown --start")),
                }
            }
            "--freq" => cfg.freq = f64::from(number()?),
            "--amp" => cfg.amp = f64::from(number()?),
            "--queue-ms" => cfg.queue_ms = number()?,
            "--stats-secs" => cfg.stats_secs = u64::from(number()?),
            "--max-secs" => cfg.max_secs = u64::from(number()?),
            "--capture-logs" => cfg.capture_logs = true,
            "--gated" => cfg.gated = true,
            "--close-on-reconnecting" => cfg.close_on_reconnecting = true,
            "--no-dtx" => cfg.dtx = false,
            "--disable-on-republish" => cfg.disable_on_republish = true,
            _ => return Err(usage(&format!("unknown option {name}"))),
        }
    }
    if !cfg.queue_ms.is_multiple_of(10) {
        return Err(usage("--queue-ms must be a multiple of 10"));
    }
    Ok(cfg)
}

// ------------------------------------------------------------------- audio

/// The tone a publisher feeds its audio source from before the track is published until the
/// process ends, whatever the track's mute state: what a client with no transmit gate would do.
struct Tone {
    source: NativeAudioSource,
    freq: f64,
    amp: f64,
    mark_frames: AtomicU32,
    fed: AtomicU64,
    /// With `gated`, frames are handed to the source only while `open`.
    gated: bool,
    open: AtomicBool,
}

async fn feed(tone: Arc<Tone>) {
    let mut tick = tokio::time::interval(Duration::from_millis(10));
    tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
    let mut samples = vec![0i16; FRAME_LEN];
    let mut n: u64 = 0;
    let mut marking = false;
    let mut failed = false;
    loop {
        tick.tick().await;
        if tone.gated && !tone.open.load(Ordering::Relaxed) {
            continue;
        }
        let mark = tone
            .mark_frames
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |left| {
                left.checked_sub(1)
            })
            .is_ok();
        let amp = if mark { MARK_AMP } else { tone.amp };
        for sample in &mut samples {
            let t = n as f64 / f64::from(SAMPLE_RATE);
            *sample = ((t * tone.freq * std::f64::consts::TAU).sin() * amp) as i16;
            n += 1;
        }
        if mark && !marking {
            emit("mark_fed", json!({}));
        }
        marking = mark;
        let frame = AudioFrame {
            data: Cow::Borrowed(samples.as_slice()),
            sample_rate: SAMPLE_RATE,
            num_channels: 1,
            samples_per_channel: FRAME_LEN as u32,
        };
        match tone.source.capture_frame(&frame).await {
            Ok(()) => {
                tone.fed.fetch_add(1, Ordering::Relaxed);
            }
            Err(err) if !failed => {
                failed = true;
                emit(
                    "capture_error",
                    json!({"error": SECRETS.redact(&format!("{err:?}"))}),
                );
            }
            Err(_) => {}
        }
    }
}

/// What has been received from one speaker since the probe started, over every track of theirs.
#[derive(Default)]
struct Heard {
    frames: AtomicU64,
    loud: AtomicU64,
    /// Frames with any sample that is not zero.
    nonzero: AtomicU64,
    /// The largest RMS of a frame that was not loud, times 100.
    max_quiet_rms_x100: AtomicU64,
}

type HeardMap = Arc<Mutex<BTreeMap<String, Arc<Heard>>>>;

fn rms(samples: &[i16]) -> f64 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f64 = samples.iter().map(|s| f64::from(*s).powi(2)).sum();
    (sum / samples.len() as f64).sqrt()
}

/// Reads one remote audio track for as long as it yields frames, counting them and reporting
/// when loud audio starts and stops, when a mark arrives, and when frames stop coming.
async fn hear(track: RemoteAudioTrack, speaker: String, heard: Arc<Heard>) {
    let mut stream = NativeAudioStream::new(track.rtc_track(), SAMPLE_RATE as i32, 1);
    let (mut loud, mut marked, mut stalled, mut first) = (false, false, false, true);
    let (mut last_frame, mut last_loud) = (now_ms(), 0.0);
    loop {
        let frame = match tokio::time::timeout(STALL, stream.next()).await {
            Ok(Some(frame)) => frame,
            Ok(None) => {
                emit("stream_end", json!({"speaker": speaker}));
                return;
            }
            Err(_) => {
                if !stalled {
                    stalled = true;
                    if loud {
                        loud = false;
                        emit_at(
                            last_loud,
                            "loud_end",
                            json!({"speaker": speaker, "why": "stall"}),
                        );
                    }
                    emit_at(last_frame, "stall", json!({"speaker": speaker}));
                }
                continue;
            }
        };
        let t = now_ms();
        let level = rms(&frame.data);
        if first {
            first = false;
            emit_at(
                t,
                "first_frame",
                json!({"speaker": speaker, "samples": frame.samples_per_channel, "rate": frame.sample_rate}),
            );
        }
        if stalled {
            stalled = false;
            emit_at(
                t,
                "frames_resume",
                json!({"speaker": speaker, "gap_ms": t - last_frame}),
            );
        }
        heard.frames.fetch_add(1, Ordering::Relaxed);
        if frame.data.iter().any(|s| *s != 0) {
            heard.nonzero.fetch_add(1, Ordering::Relaxed);
        }
        let is_loud = level > LOUD_RMS;
        if is_loud {
            heard.loud.fetch_add(1, Ordering::Relaxed);
            if !loud {
                emit_at(
                    t,
                    "loud_start",
                    json!({"speaker": speaker, "rms": level.round()}),
                );
            }
            last_loud = t;
        } else {
            heard
                .max_quiet_rms_x100
                .fetch_max((level * 100.0) as u64, Ordering::Relaxed);
            if loud {
                emit_at(
                    last_loud,
                    "loud_end",
                    json!({"speaker": speaker, "why": "quiet"}),
                );
            }
        }
        loud = is_loud;
        let is_mark = level > MARK_RMS;
        if is_mark && !marked {
            emit_at(
                t,
                "mark_heard",
                json!({"speaker": speaker, "rms": level.round()}),
            );
        }
        marked = is_mark;
        last_frame = t;
    }
}

/// What has been heard from each speaker so far and, for a publisher, how many frames it has
/// handed to its own track.
fn audio_snapshot(heard: &HeardMap, published: Option<&Published>) -> Value {
    let speakers: Vec<Value> = lock(heard)
        .iter()
        .map(|(speaker, h)| {
            json!({
                "speaker": speaker,
                "frames": h.frames.load(Ordering::Relaxed),
                "loud": h.loud.load(Ordering::Relaxed),
                "nonzero": h.nonzero.load(Ordering::Relaxed),
                "max_quiet_rms": h.max_quiet_rms_x100.load(Ordering::Relaxed) as f64 / 100.0,
            })
        })
        .collect();
    let fed = published.map(|p| p.tone.fed.load(Ordering::Relaxed));
    json!({"speakers": speakers, "fed": fed})
}

// ------------------------------------------------------------------- stats

/// CPU time in clock ticks (user, system) from `/proc/self/stat`, and resident memory and
/// thread count from `/proc/self/status`. The reader turns ticks into seconds.
fn stats() -> Value {
    let stat = std::fs::read_to_string("/proc/self/stat").unwrap_or_default();
    // The fields after the command name, which is in brackets and may contain spaces.
    let fields: Vec<&str> = stat
        .rsplit(')')
        .next()
        .unwrap_or("")
        .split_whitespace()
        .collect();
    let ticks = |i: usize| fields.get(i).and_then(|v| v.parse::<u64>().ok());
    let status = std::fs::read_to_string("/proc/self/status").unwrap_or_default();
    let field = |name: &str| {
        status
            .lines()
            .find_map(|line| line.strip_prefix(name))
            .and_then(|rest| rest.trim_start_matches(':').split_whitespace().next())
            .and_then(|v| v.parse::<u64>().ok())
    };
    json!({
        "utime_ticks": ticks(11),
        "stime_ticks": ticks(12),
        "rss_kb": field("VmRSS"),
        "rss_peak_kb": field("VmHWM"),
        "threads": field("Threads"),
    })
}

/// What this client has sent as RTP so far, from WebRTC's own statistics: the packets and
/// payload bytes of every outgoing stream.
async fn sent_rtp(room: &Room) -> Value {
    let stats = match room.get_stats().await {
        Ok(stats) => stats,
        Err(err) => return json!({"error": SECRETS.redact(&err.to_string())}),
    };
    let (mut streams, mut packets, mut bytes) = (0u64, 0u64, 0u64);
    for stat in stats.publisher_stats.iter().chain(&stats.subscriber_stats) {
        if let livekit::webrtc::stats::RtcStats::OutboundRtp(out) = stat {
            streams += 1;
            packets += out.sent.packets_sent;
            bytes += out.sent.bytes_sent;
        }
    }
    json!({"streams": streams, "packets_sent": packets, "bytes_sent": bytes})
}

// ----------------------------------------------------------------- session

struct Published {
    track: LocalAudioTrack,
    publication: LocalTrackPublication,
    tone: Arc<Tone>,
}

async fn publish(room: &Room, cfg: &Config) -> Result<Published, ProbeError> {
    let source =
        NativeAudioSource::new(AudioSourceOptions::default(), SAMPLE_RATE, 1, cfg.queue_ms);
    let tone = Arc::new(Tone {
        source: source.clone(),
        freq: cfg.freq,
        amp: cfg.amp,
        mark_frames: AtomicU32::new(0),
        fed: AtomicU64::new(0),
        gated: cfg.gated,
        open: AtomicBool::new(cfg.start == Start::Unmuted),
    });
    // The tone runs from before the track exists: nothing below waits for a quiet moment.
    tokio::spawn(feed(tone.clone()));
    let track = LocalAudioTrack::create_audio_track("mic", RtcAudioSource::Native(source));
    if matches!(cfg.start, Start::Premute | Start::PremuteDisable) {
        track.mute();
    }
    let began = now_ms();
    let options = TrackPublishOptions {
        source: TrackSource::Microphone,
        dtx: cfg.dtx,
        ..Default::default()
    };
    let publication = room
        .local_participant()
        .publish_track(LocalTrack::Audio(track.clone()), options)
        .await
        .map_err(|err| ProbeError::Sdk(SECRETS.redact(&format!("publish: {err}"))))?;
    let returned = now_ms();
    emit_at(
        returned,
        "published",
        json!({
            "publish_ms": returned - began,
            "muted": publication.is_muted(),
            "rtc_enabled": track.is_enabled(),
        }),
    );
    match cfg.start {
        Start::Postmute => {
            let t = now_ms();
            publication.mute();
            emit_at(
                t,
                "mute_call",
                json!({"returned_after_ms": now_ms() - t, "first": true}),
            );
        }
        Start::PremuteDisable | Start::DisableOnly => {
            track.disable();
            emit(
                "disabled",
                json!({"muted": publication.is_muted(), "rtc_enabled": track.is_enabled()}),
            );
        }
        Start::Unmuted | Start::Premute => {}
    }
    Ok(Published {
        track,
        publication,
        tone,
    })
}

fn on_event(event: RoomEvent, cfg: &Config, heard: &HeardMap) {
    match event {
        RoomEvent::Disconnected { reason } => {
            emit("disconnected", json!({"reason": reason.as_str_name()}));
        }
        RoomEvent::Reconnecting => emit("reconnecting", json!({})),
        RoomEvent::Reconnected => emit("reconnected", json!({})),
        RoomEvent::ConnectionStateChanged(state) => {
            emit("connection_state", json!({"state": format!("{state:?}")}));
        }
        RoomEvent::TokenRefreshed { token } => {
            SECRETS.add("refreshed_token", &token);
            emit(
                "token_refreshed",
                json!({"same_as_join_token": token == cfg.token}),
            );
        }
        RoomEvent::ParticipantConnected(p) => {
            emit(
                "participant_connected",
                json!({"who": p.identity().to_string()}),
            );
        }
        RoomEvent::ParticipantDisconnected(p) => {
            emit(
                "participant_disconnected",
                json!({"who": p.identity().to_string()}),
            );
        }
        RoomEvent::TrackPublished {
            publication,
            participant,
        } => {
            emit(
                "track_published",
                json!({"who": participant.identity().to_string(), "muted": publication.is_muted()}),
            );
        }
        RoomEvent::TrackUnpublished { participant, .. } => {
            emit(
                "track_unpublished",
                json!({"who": participant.identity().to_string()}),
            );
        }
        RoomEvent::TrackSubscribed {
            track,
            publication,
            participant,
        } => {
            let speaker = participant.identity().to_string();
            emit(
                "track_subscribed",
                json!({"who": speaker, "muted": publication.is_muted()}),
            );
            if let RemoteTrack::Audio(audio) = track {
                let counters = lock(heard).entry(speaker.clone()).or_default().clone();
                tokio::spawn(hear(audio, speaker, counters));
            }
        }
        RoomEvent::TrackUnsubscribed { participant, .. } => {
            emit(
                "track_unsubscribed",
                json!({"who": participant.identity().to_string()}),
            );
        }
        RoomEvent::TrackSubscriptionFailed {
            participant, error, ..
        } => {
            emit(
                "track_subscription_failed",
                json!({
                    "who": participant.identity().to_string(),
                    "error": SECRETS.redact(&format!("{error:?}")),
                }),
            );
        }
        RoomEvent::TrackMuted { participant, .. } => {
            emit(
                "track_muted",
                json!({"who": participant.identity().to_string()}),
            );
        }
        RoomEvent::TrackUnmuted { participant, .. } => {
            emit(
                "track_unmuted",
                json!({"who": participant.identity().to_string()}),
            );
        }
        RoomEvent::LocalTrackPublished { .. } => emit("local_track_published", json!({})),
        RoomEvent::LocalTrackUnpublished { .. } => emit("local_track_unpublished", json!({})),
        RoomEvent::LocalTrackRepublished {
            publication, track, ..
        } => {
            // The SDK publishes the track again after a full reconnect, and `publish_track`
            // enables the WebRTC track whatever its mute state.
            let enabled = track.is_enabled();
            if cfg.disable_on_republish && publication.is_muted() {
                track.disable();
            }
            emit(
                "local_track_republished",
                json!({
                    "muted": publication.is_muted(),
                    "rtc_enabled": enabled,
                    "rtc_enabled_now": track.is_enabled(),
                }),
            );
        }
        // Frequent, and nothing here measures them.
        RoomEvent::ActiveSpeakersChanged { .. }
        | RoomEvent::ConnectionQualityChanged { .. }
        | RoomEvent::ParticipantsUpdated { .. }
        | RoomEvent::RoomUpdated { .. } => {}
        other => {
            // Only the variant's name: its fields may hold a room's name.
            let debug = format!("{other:?}");
            let name: String = debug
                .chars()
                .take_while(char::is_ascii_alphanumeric)
                .collect();
            emit("other_event", json!({"name": name}));
        }
    }
}

fn on_command(command: &str, published: Option<&Published>, heard: &HeardMap) {
    match (command, published) {
        ("unmute", Some(p)) => {
            let t = now_ms();
            p.tone.open.store(true, Ordering::Relaxed);
            p.publication.unmute();
            let after = now_ms() - t;
            emit_at(
                t,
                "unmute_call",
                json!({"returned_after_ms": after, "muted": p.publication.is_muted(), "rtc_enabled": p.track.is_enabled()}),
            );
        }
        ("mute", Some(p)) => {
            let t = now_ms();
            p.tone.open.store(false, Ordering::Relaxed);
            p.publication.mute();
            let after = now_ms() - t;
            emit_at(
                t,
                "mute_call",
                json!({"returned_after_ms": after, "muted": p.publication.is_muted(), "rtc_enabled": p.track.is_enabled()}),
            );
        }
        ("mark", Some(p)) => p.tone.mark_frames.store(MARK_FRAMES, Ordering::Relaxed),
        ("audio", _) => {
            emit("audio", audio_snapshot(heard, published));
            // The loudest quiet frame is reported per stretch between two `audio` commands.
            for h in lock(heard).values() {
                h.max_quiet_rms_x100.store(0, Ordering::Relaxed);
            }
        }
        _ => emit("bad_command", json!({"command": SECRETS.redact(command)})),
    }
}

/// Reads commands from standard input on a thread of its own. The end of input is `quit`, so
/// a probe never outlives the program that started it.
fn commands() -> mpsc::UnboundedReceiver<String> {
    let (tx, rx) = mpsc::unbounded_channel();
    std::thread::spawn(move || {
        for line in std::io::stdin().lock().lines() {
            let Ok(line) = line else { break };
            if tx.send(line.trim().to_owned()).is_err() {
                return;
            }
        }
        let _ = tx.send("quit".to_owned());
    });
    rx
}

async fn run(cfg: &Config) -> Result<(), ProbeError> {
    let mut options = RoomOptions::default();
    // A probe that only publishes does not receive: it stands in for someone else's client,
    // and its cost is not what is measured.
    options.auto_subscribe = cfg.mode != Mode::Publish;
    let began = now_ms();
    emit_at(began, "connect_call", json!({}));
    let (room, mut events) = match Room::connect(&cfg.url, &cfg.token, options).await {
        Ok(connected) => connected,
        Err(err) => {
            let (display, debug) = (err.to_string(), format!("{err:?}"));
            emit(
                "connect_failed",
                json!({
                    "connect_ms": now_ms() - began,
                    "error": SECRETS.redact(&display),
                    "error_debug": SECRETS.redact(&debug),
                    "error_holds_token": display.contains(&cfg.token) || debug.contains(&cfg.token),
                }),
            );
            return Ok(());
        }
    };
    SECRETS.add("room_name", &room.name());
    emit(
        "connected",
        json!({"connect_ms": now_ms() - began, "who": room.local_participant().identity().to_string()}),
    );

    if cfg.stats_secs > 0 {
        emit("stats_connected", stats());
    }

    let heard: HeardMap = Arc::default();
    let published = if cfg.mode == Mode::Listen {
        None
    } else {
        Some(publish(&room, cfg).await?)
    };

    let mut commands = commands();
    let mut stats_tick = tokio::time::interval(Duration::from_secs(cfg.stats_secs.max(1)));
    let deadline = tokio::time::sleep(Duration::from_secs(cfg.max_secs));
    tokio::pin!(deadline);
    // The room's events can end before the probe is told to quit; it then waits for that.
    let mut events_open = true;
    loop {
        tokio::select! {
            event = events.recv(), if events_open => match event {
                Some(event) => {
                    let reconnecting = matches!(event, RoomEvent::Reconnecting);
                    on_event(event, cfg, &heard);
                    if reconnecting && cfg.close_on_reconnecting {
                        break;
                    }
                }
                None => {
                    events_open = false;
                    emit("events_closed", json!({}));
                }
            },
            command = commands.recv() => match command.as_deref() {
                Some("quit") | None => break,
                Some("rtp") => emit("rtp", sent_rtp(&room).await),
                Some(command) => on_command(command, published.as_ref(), &heard),
            },
            _ = stats_tick.tick(), if cfg.stats_secs > 0 => {
                emit("stats", stats());
                emit("audio", audio_snapshot(&heard, published.as_ref()));
            }
            () = &mut deadline => {
                emit("deadline", json!({}));
                break;
            }
        }
    }

    emit("audio", audio_snapshot(&heard, published.as_ref()));
    let closing = now_ms();
    let closed = tokio::time::timeout(CLOSE_WAIT, room.close()).await;
    let outcome = match closed {
        Ok(Ok(())) => "closed".to_owned(),
        Ok(Err(err)) => SECRETS.redact(&err.to_string()),
        Err(_) => "close timed out".to_owned(),
    };
    emit(
        "closed",
        json!({"close_ms": now_ms() - closing, "outcome": outcome}),
    );
    Ok(())
}

fn main() -> ExitCode {
    let cfg = match config() {
        Ok(cfg) => cfg,
        Err(err) => {
            eprintln!("sdk_probe: {err}");
            return ExitCode::from(2);
        }
    };
    SECRETS.add("join_token", &cfg.token);
    if let Ok(room) = std::env::var("SDK_PROBE_ROOM") {
        SECRETS.add("room_name", &room);
    }
    if cfg.capture_logs && log::set_logger(&LOGGER).is_ok() {
        LOGGER.on.store(true, Ordering::Relaxed);
        log::set_max_level(log::LevelFilter::Trace);
    }
    emit(
        "start",
        json!({"unix_us": unix_us(), "capture_logs": cfg.capture_logs}),
    );
    if cfg.stats_secs > 0 {
        emit("stats_at_start", stats());
    }

    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(err) => {
            eprintln!("sdk_probe: cannot start the runtime: {err}");
            return ExitCode::FAILURE;
        }
    };
    let result = runtime.block_on(run(&cfg));
    if let Err(err) = &result {
        emit("error", json!({"error": SECRETS.redact(&err.to_string())}));
    }
    if cfg.capture_logs {
        LOGGER.on.store(false, Ordering::Relaxed);
        emit("log_scan", log_scan(&cfg.token));
    }
    emit("exit", json!({"unix_us": unix_us()}));
    // Not a return: the runtime's shutdown would wait for the SDK's own threads.
    std::process::exit(if result.is_ok() { 0 } else { 1 });
}
