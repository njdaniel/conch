//! What the tests of `conch-voice join` stand on: a fake SDK ([`fake`]), a stub `conchd`
//! ([`stub`]), and a [`Rig`] that runs the real session loop between them, with a tone for
//! a microphone that never stops, and collects what it writes.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit, and none sleeps for
//! longer than the thing it waits for: every wait the loop makes is shortened through
//! [`Timings`], and a test waits for a condition, not for a time.

// Each test binary uses its own part of this module.
#![allow(dead_code)]

pub mod fake;
pub mod keyboard;
pub mod stub;

use std::io::Write;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::Error;
use conch_voice::output::Output;
use conch_voice::presence::PresenceTimings;
use conch_voice::reports::ReportTimings;
use conch_voice::secrets::Scrubber;
use conch_voice::session::{self, Conchd, Input, Settings, Timings};
use conch_voice::transmit::BoxedMic;
use conch_voice_api::{Secret, ServerAddress};
use conch_voice_audio::ToneSource;
use conch_voice_control::LineCommand;
use serde_json::Value;
use tokio::sync::mpsc;
use tokio::task::JoinHandle;

use self::fake::FakeSdk;
use self::stub::{FAKE_LOGIN, Stub};

/// The channel every test joins.
pub const CHANNEL: &str = "ops";

/// How long a test waits for something that should happen at once before it fails.
const PATIENCE: Duration = Duration::from_secs(10);

/// What a session wrote, shared with the test.
#[derive(Clone, Default)]
pub struct Written(Arc<Mutex<Vec<u8>>>);

impl Write for Written {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Writes to standard output as well as to what the test reads back: for a test that is
/// itself run as a process, to see what really reaches a terminal.
struct AlsoStdout(Written);

impl Write for AlsoStdout {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        std::io::stdout().write_all(bytes)?;
        self.0.write(bytes)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        std::io::stdout().flush()
    }
}

impl Written {
    /// Everything written so far.
    pub fn text(&self) -> String {
        String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
    }

    /// Every line written so far, as the JSON object it is.
    pub fn events(&self) -> Vec<Value> {
        self.text()
            .lines()
            .map(|line| {
                assert!(line.starts_with('{'), "not an object: {line}");
                serde_json::from_str(line).unwrap()
            })
            .collect()
    }
}

/// Waits until `met` is true, looking every few milliseconds.
///
/// # Panics
///
/// If it is still false after [`PATIENCE`], with `what` and whatever `context` says.
pub async fn until(what: &str, context: impl Fn() -> String, met: impl Fn() -> bool) {
    let deadline = tokio::time::Instant::now() + PATIENCE;
    while !met() {
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}\n{}",
            context()
        );
        tokio::time::sleep(Duration::from_millis(2)).await;
    }
}

/// Waits long enough for several frames to have been sent if anything were sending them:
/// the pause after which "no frame was sent" means something.
pub async fn several_frames() {
    tokio::time::sleep(Duration::from_millis(60)).await;
}

/// Every wait of the session loop, shortened to milliseconds. The reconnect grace is left
/// as the design has it; the test of it shortens that one itself.
pub fn quick() -> Timings {
    Timings {
        reconnect_grace: Duration::from_secs(20),
        close_limit: Duration::from_secs(2),
        attempt_limit: Duration::from_millis(300),
        gate_shut_grace: Duration::from_millis(150),
        report_flush: Duration::from_secs(2),
        stats_every: Duration::from_millis(50),
        // A wait of one second becomes five milliseconds.
        wait_divisor: 200,
        reports: ReportTimings {
            waits: vec![Duration::from_millis(10), Duration::from_millis(10)],
        },
        presence: PresenceTimings {
            first_wait: Duration::from_millis(20),
            longest_wait: Duration::from_millis(40),
        },
        jitter: || u32::MAX,
    }
}

/// How a rig differs from the usual one.
pub struct Setup {
    pub json: bool,
    pub timings: Timings,
    pub release_tail_ms: u32,
    pub max_transmit: Duration,
    /// How long one attempt at a transmit report may take.
    pub report_timeout: Duration,
    pub sink_tones: Vec<f32>,
    /// The server to talk to, if not the stub.
    pub server: Option<String>,
    /// Write to the process's standard output as well.
    pub also_stdout: bool,
    /// The key device the session is told it has, for the status. The test starts the
    /// watcher itself, on [`Rig::inputs`].
    pub key_device: Option<String>,
}

impl Default for Setup {
    fn default() -> Self {
        Self {
            json: true,
            timings: quick(),
            release_tail_ms: 20,
            max_transmit: Duration::from_secs(120),
            report_timeout: Duration::from_millis(200),
            sink_tones: vec![440.0, 880.0],
            server: None,
            also_stdout: false,
            key_device: None,
        }
    }
}

/// A running `conch-voice join`, between a fake SDK and a stub `conchd`.
pub struct Rig {
    pub sdk: FakeSdk,
    pub conchd: Stub,
    pub scrubber: Arc<Scrubber>,
    pub out: Written,
    inputs: mpsc::UnboundedSender<Input>,
    /// How many times the microphone source was opened.
    mic_opened: Arc<AtomicU32>,
    session: JoinHandle<Result<(), Error>>,
}

impl Rig {
    /// Starts a session with the usual setup.
    pub async fn start() -> Self {
        Self::start_with(Stub::start().await, Setup::default(), |_| {}).await
    }

    /// Starts a session against `conchd`, after `arrange` has prepared the fake SDK.
    pub async fn start_with(conchd: Stub, setup: Setup, arrange: impl FnOnce(&FakeSdk)) -> Self {
        Self::start_scrubbing(Scrubber::new(), conchd, setup, arrange).await
    }

    /// As [`Rig::start_with`], with a scrubber the test already shares with a logger.
    pub async fn start_scrubbing(
        scrubber: Arc<Scrubber>,
        conchd: Stub,
        setup: Setup,
        arrange: impl FnOnce(&FakeSdk),
    ) -> Self {
        let sdk = FakeSdk::new(Arc::clone(&scrubber));
        arrange(&sdk);
        let server = setup.server.clone().unwrap_or_else(|| conchd.url());
        let server = ServerAddress::parse(&server).unwrap();
        let token = Secret::new(FAKE_LOGIN);
        scrubber.always(&token);
        let api = Conchd::new(
            &server,
            &token,
            Duration::from_secs(2),
            setup.report_timeout,
        )
        .unwrap();

        let mic_opened = Arc::new(AtomicU32::new(0));
        let opened = Arc::clone(&mic_opened);
        let settings = Settings {
            channel: CHANNEL.to_owned(),
            // A tone that is never silent and never stops: whatever does not reach the
            // fake's audio source was stopped by the gate, not by there being nothing to send.
            open_mic: Box::new(move || {
                opened.fetch_add(1, Ordering::SeqCst);
                Ok(Some(Box::new(ToneSource::new(440.0, 0.25)?) as BoxedMic))
            }),
            sink_tones: setup.sink_tones,
            release_tail_ms: setup.release_tail_ms,
            max_transmit: setup.max_transmit,
            key_device: setup.key_device,
            timings: setup.timings,
        };
        let out = Written::default();
        let writer: Box<dyn Write + Send> = if setup.also_stdout {
            Box::new(AlsoStdout(out.clone()))
        } else {
            Box::new(out.clone())
        };
        let (inputs, received) = mpsc::unbounded_channel();
        let session = tokio::spawn(session::join(
            settings,
            api,
            sdk.clone(),
            Arc::clone(&scrubber),
            received,
            Output::new(setup.json, writer),
        ));
        Self {
            sdk,
            conchd,
            scrubber,
            out,
            inputs,
            mic_opened,
            session,
        }
    }

    /// One line on "standard input".
    pub fn line(&self, command: LineCommand) {
        self.input(Input::Line(command));
    }

    pub fn input(&self, input: Input) {
        self.inputs.send(input).unwrap();
    }

    /// Where the session takes its inputs from: for a key device's watcher to write to.
    pub fn inputs(&self) -> mpsc::UnboundedSender<Input> {
        self.inputs.clone()
    }

    /// How many times the microphone source was opened.
    pub fn mic_opened(&self) -> u32 {
        self.mic_opened.load(Ordering::SeqCst)
    }

    pub fn events(&self) -> Vec<Value> {
        self.out.events()
    }

    /// The objects written so far whose `event` is `name`.
    pub fn events_named(&self, name: &str) -> Vec<Value> {
        let mut events = self.events();
        events.retain(|event| event["event"] == name);
        events
    }

    /// The latest `self` object: the user's own state.
    pub fn own(&self) -> Value {
        self.events_named("self").pop().unwrap_or(Value::Null)
    }

    /// The `state` of every `connection` object so far, in order.
    pub fn connection_states(&self) -> Vec<String> {
        self.events_named("connection")
            .iter()
            .map(|event| event["state"].as_str().unwrap().to_owned())
            .collect()
    }

    /// Waits for something to be true of what was written and done.
    pub async fn until(&self, what: &str, met: impl Fn(&Rig) -> bool) {
        until(
            what,
            || {
                format!(
                    "written:\n{}\nsdk: {:?}\nreports: {:?}",
                    self.out.text(),
                    self.sdk.control_calls(),
                    self.conchd.reported()
                )
            },
            || met(self),
        )
        .await;
    }

    /// Waits until the client has said `connected` this many times in all.
    pub async fn connected(&self, times: usize) {
        self.until("connected", |rig| {
            rig.connection_states()
                .iter()
                .filter(|state| *state == "connected")
                .count()
                >= times
        })
        .await;
    }

    /// Waits until a press would transmit: connected, allowed, with a microphone.
    pub async fn ready(&self) {
        self.until("ready to talk", |rig| {
            let own = rig.own();
            own["blocked"] == serde_json::json!([]) && own["transmitting"] == false
        })
        .await;
    }

    /// Waits until the user's own state says a transmission is in progress.
    pub async fn talking(&self) {
        self.until("talking", |rig| rig.own()["transmitting"] == true)
            .await;
    }

    /// Waits until the user's own state says no transmission is in progress.
    pub async fn not_talking(&self) {
        self.until("not talking", |rig| rig.own()["transmitting"] == false)
            .await;
    }

    /// Waits until `conchd` has received this many transmit reports.
    pub async fn reported(&self, count: usize) {
        self.until("transmit reports", |rig| {
            rig.conchd.reported().len() >= count
        })
        .await;
    }

    /// Waits until the fake's audio source has been handed more than `before` frames.
    pub async fn frames_beyond(&self, before: u64) {
        self.until("frames at the audio source", |rig| {
            rig.sdk.frames() > before
        })
        .await;
    }

    /// Waits for the session to end, and gives what `join` returned.
    pub async fn ended(self) -> (Result<(), Error>, Ended) {
        let result = tokio::time::timeout(PATIENCE, self.session)
            .await
            .unwrap_or_else(|_| panic!("the session did not end\n{}", self.out.text()))
            .unwrap();
        let ended = Ended {
            sdk: self.sdk,
            conchd: self.conchd,
            out: self.out,
        };
        (result, ended)
    }
}

/// What is left of a rig whose session has ended.
pub struct Ended {
    pub sdk: FakeSdk,
    pub conchd: Stub,
    pub out: Written,
}
