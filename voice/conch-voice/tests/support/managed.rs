//! What the tests of `conch-voice managed` stand on: a [`ManagedRig`] that runs the real
//! managed helper (`conch_voice::managed::run`) between a fake SDK and a stub `conchd`, with
//! a named pipe for a keyboard, a tone for a microphone that never stops, and the owner's
//! two pipes in the test's hands: it writes commands, reads events and closes standard
//! input as `conch` would.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit.

use std::fs::{File, OpenOptions};
use std::io::{PipeWriter, Write};
use std::os::unix::fs::PermissionsExt;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::Error;
use conch_voice::cli::Environment;
use conch_voice::managed::{self, Devices, Host, KeySource, Watchdog};
use conch_voice::receive::BoxedSink;
use conch_voice::secrets::Scrubber;
use conch_voice::session::Input;
use conch_voice::transmit::BoxedMic;
use conch_voice_api::{Secret, VoiceControlEvent, parse_event};
use conch_voice_audio::{CountingSink, MicSource, ToneSource};
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio::task::JoinHandle;

use super::fake::FakeSdk;
use super::keyboard::{DEAFEN, MUTE, Pipe, TALK, press, quick_keys, release};
use super::stub::{FAKE_LOGIN, OWN_PRINCIPAL, Stub};
use super::{Reader, Written, quick};

/// The channel every managed test joins.
pub const CHANNEL: &str = "general";

/// How long a test waits for something that should happen at once before it fails.
const PATIENCE: Duration = Duration::from_secs(10);

/// What a rig's `voice.toml` binds: the three keys of `keyboard.rs`, a short release tail.
pub const KEYS_TOML: &str = "[keys]\ntalk = \"KEY_F13\"\nmute = \"KEY_F14\"\ndeafen = \"KEY_F15\"\n\n[audio]\nrelease_tail_ms = 20\n";

/// Where the helper is told its login is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Login {
    /// `credentials.json` holds it, and `CONCH_TOKEN` is not set.
    Stored,
    /// `CONCH_TOKEN` holds it, and there is no `credentials.json`.
    Env,
    /// `credentials.json` holds it and `CONCH_TOKEN` is set too.
    Both,
    /// Neither.
    Nowhere,
}

/// How a rig differs from the usual one.
pub struct ManagedSetup {
    pub login: Login,
    /// The text of `voice.toml`; `None` for no file.
    pub voice_toml: Option<String>,
    /// Read keys from the named pipe (`--test-key-device`), with the bindings of `voice_toml`.
    pub keyboard: bool,
    pub start_timeout: Duration,
}

impl Default for ManagedSetup {
    fn default() -> Self {
        Self {
            login: Login::Stored,
            voice_toml: Some(KEYS_TOML.to_owned()),
            keyboard: true,
            start_timeout: Duration::from_secs(10),
        }
    }
}

/// A microphone that counts when it is dropped.
struct Closing<M>(M, Arc<AtomicU32>);

impl<M: MicSource> MicSource for Closing<M> {
    fn read(&mut self, frame: &mut conch_voice_audio::Frame) -> conch_voice_audio::MicRead {
        self.0.read(frame)
    }
}

impl<M> Drop for Closing<M> {
    fn drop(&mut self) {
        self.1.fetch_add(1, Ordering::SeqCst);
    }
}

/// A sink that counts when it is dropped, which is what stops a real one playing.
struct Silenced(CountingSink, Arc<AtomicU32>);

impl conch_voice_audio::SpeakerSink for Silenced {
    fn play(&mut self, frame: &conch_voice_audio::Frame) {
        self.0.play(frame);
    }
}

impl Drop for Silenced {
    fn drop(&mut self) {
        self.1.fetch_add(1, Ordering::SeqCst);
    }
}

/// A running `conch-voice managed`, between a fake SDK and a stub `conchd`, with the
/// owner's ends of its pipes.
pub struct ManagedRig {
    pub sdk: FakeSdk,
    pub conchd: Stub,
    pub scrubber: Arc<Scrubber>,
    /// Everything written to the event pipe.
    pub events: Written,
    /// The owner reading the event pipe, who can stop reading.
    pub reader: Reader,
    /// The owner's end of the helper's standard input; `None` once closed.
    stdin: Option<PipeWriter>,
    /// Where signals and the like reach the session.
    pub inputs: mpsc::UnboundedSender<Input>,
    /// The keyboard's end of the key device's pipe, if the rig reads keys.
    keyboard: Option<File>,
    mic_opened: Arc<AtomicU32>,
    mic_closed: Arc<AtomicU32>,
    sink_closed: Arc<AtomicU32>,
    task: Option<JoinHandle<Result<(), Error>>>,
    /// Holds the configuration directory and the pipe for as long as the rig lives.
    _dir: tempfile::TempDir,
    _pipe: Pipe,
}

impl ManagedRig {
    /// Starts a helper with the usual setup and nothing written to it yet.
    pub async fn start() -> Self {
        Self::start_with(Stub::start().await, ManagedSetup::default(), |_| {}).await
    }

    /// Starts a helper against `conchd`, after `arrange` has prepared the fake SDK.
    pub async fn start_with(
        conchd: Stub,
        setup: ManagedSetup,
        arrange: impl FnOnce(&FakeSdk),
    ) -> Self {
        let scrubber = Scrubber::new();
        let sdk = FakeSdk::new(Arc::clone(&scrubber));
        arrange(&sdk);

        let dir = tempfile::tempdir().unwrap();
        if matches!(setup.login, Login::Stored | Login::Both) {
            let path = dir.path().join("credentials.json");
            std::fs::write(
                &path,
                json!({ conchd.url(): {"token": FAKE_LOGIN} }).to_string(),
            )
            .unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        if let Some(text) = &setup.voice_toml {
            let path = dir.path().join("voice.toml");
            std::fs::write(&path, text).unwrap();
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        let token =
            matches!(setup.login, Login::Env | Login::Both).then(|| Secret::new(FAKE_LOGIN));
        let environment = Environment {
            conch_config_dir: Some(dir.path().as_os_str().to_owned()),
            xdg_config_home: None,
            home: None,
            server: None,
            token,
        };

        // The keyboard's end is opened to read and write, so that it does not wait for the
        // watcher, and the watcher's open does not wait for it.
        let pipe = Pipe::new();
        let keyboard = setup.keyboard.then(|| {
            OpenOptions::new()
                .read(true)
                .write(true)
                .open(&pipe.path)
                .unwrap()
        });

        let mic_opened = Arc::new(AtomicU32::new(0));
        let mic_closed = Arc::new(AtomicU32::new(0));
        let sink_closed = Arc::new(AtomicU32::new(0));
        let (opened, closed, silenced) = (
            Arc::clone(&mic_opened),
            Arc::clone(&mic_closed),
            Arc::clone(&sink_closed),
        );
        let devices = Devices::Given {
            // A tone that never stops: whatever does not reach the fake's audio source was
            // stopped by the gate.
            open_mic: Box::new(move || {
                opened.fetch_add(1, Ordering::SeqCst);
                let tone = ToneSource::new(440.0, 0.25)?;
                Ok(Some(
                    Box::new(Closing(tone, Arc::clone(&closed))) as BoxedMic
                ))
            }),
            open_sink: Box::new(move || {
                Ok(Box::new(Silenced(CountingSink::new(&[440.0])?, silenced)) as BoxedSink)
            }),
            sink_tones: vec![440.0],
        };
        let mut timings = quick();
        // A wait the policy asks for is a few milliseconds, as in the other tests.
        timings.wait_divisor = 200;
        let host = Host {
            environment,
            transport: sdk.clone(),
            scrubber: Arc::clone(&scrubber),
            devices,
            keys: if setup.keyboard {
                KeySource::NamedPipe(pipe.path.clone())
            } else {
                KeySource::Configured
            },
            timings,
            key_timings: quick_keys(),
            start_timeout: setup.start_timeout,
            watchdog: Watchdog::off(),
        };
        let (stdin, writer) = std::io::pipe().unwrap();
        let events = Written::default();
        let reader = Reader {
            read: events.clone(),
            ..Reader::default()
        };
        let (inputs, received) = mpsc::unbounded_channel();
        let task = tokio::spawn(managed::run(
            host,
            stdin,
            Box::new(reader.clone()),
            inputs.clone(),
            received,
        ));
        Self {
            sdk,
            conchd,
            scrubber,
            events,
            reader,
            stdin: Some(writer),
            inputs,
            keyboard,
            mic_opened,
            mic_closed,
            sink_closed,
            task: Some(task),
            _dir: dir,
            _pipe: pipe,
        }
    }

    /// Writes one line to the helper's standard input, as given, with a line feed.
    pub fn line(&mut self, bytes: &[u8]) {
        let stdin = self.stdin.as_mut().expect("standard input is closed");
        let mut line = bytes.to_vec();
        line.push(b'\n');
        stdin.write_all(&line).unwrap();
    }

    /// Writes bytes with no line feed after them: a line the owner never finished.
    pub fn write_partial(&mut self, bytes: &[u8]) {
        let stdin = self.stdin.as_mut().expect("standard input is closed");
        stdin.write_all(bytes).unwrap();
    }

    /// Writes one command.
    pub fn send(&mut self, command: &Value) {
        self.line(command.to_string().as_bytes());
    }

    /// The start every test begins with: id 1, principal 7, as the stub's login is.
    pub fn start_command(&self) -> Value {
        json!({
            "schema": "conch.voice_control.v1",
            "kind": "start",
            "id": 1,
            "server": self.conchd.url(),
            "channel": CHANNEL,
            "principal_id": OWN_PRINCIPAL,
            "credential": "stored",
            "muted": false,
            "deafened": false,
        })
    }

    /// Writes the usual start.
    pub fn send_start(&mut self) {
        let start = self.start_command();
        self.send(&start);
    }

    /// A command of `kind` with `id`, and these fields besides.
    pub fn command(kind: &str, id: i64, fields: Value) -> Value {
        let mut command = json!({"schema": "conch.voice_control.v1", "kind": kind, "id": id});
        if let (Value::Object(command), Value::Object(fields)) = (&mut command, fields) {
            command.extend(fields);
        }
        command
    }

    /// Closes the helper's standard input: the owner is gone.
    pub fn close(&mut self) {
        self.stdin = None;
    }

    /// Whether standard input is still open.
    pub fn is_open(&self) -> bool {
        self.stdin.is_some()
    }

    /// SIGTERM, as the session takes it.
    pub fn signal(&self) {
        self.inputs.send(Input::Signal).unwrap();
    }

    fn keys(&mut self, bytes: &[u8]) {
        let keyboard = self.keyboard.as_mut().expect("the rig reads no keys");
        keyboard.write_all(bytes).unwrap();
    }

    pub fn press_talk(&mut self) {
        self.keys(&press(TALK));
    }

    pub fn release_talk(&mut self) {
        self.keys(&release(TALK));
    }

    /// The mute key, pressed and released: a toggle.
    pub fn strike_mute(&mut self) {
        let mut bytes = press(MUTE);
        bytes.extend(release(MUTE));
        self.keys(&bytes);
    }

    pub fn strike_deafen(&mut self) {
        let mut bytes = press(DEAFEN);
        bytes.extend(release(DEAFEN));
        self.keys(&bytes);
    }

    /// Every line written to the event pipe so far.
    pub fn lines(&self) -> Vec<String> {
        self.events.text().lines().map(str::to_owned).collect()
    }

    /// Every event so far, read by the owner's rules. A line it would refuse fails the test.
    pub fn read(&self) -> Vec<VoiceControlEvent> {
        self.lines()
            .iter()
            .map(|line| {
                parse_event(line.as_bytes())
                    .unwrap_or_else(|refusal| panic!("the owner refuses {line}: {refusal}"))
            })
            .collect()
    }

    /// The latest state, if there is one.
    pub fn state(&self) -> Option<conch_voice_api::VoiceControlStateV1> {
        self.read().into_iter().rev().find_map(|event| match event {
            VoiceControlEvent::State(state) => Some(state),
            _ => None,
        })
    }

    /// Waits for something to be true of what the helper wrote and did.
    pub async fn until(&self, what: &str, met: impl Fn(&ManagedRig) -> bool) {
        super::until(
            what,
            || {
                format!(
                    "events:\n{}\nsdk: {:?}",
                    self.events.text(),
                    self.sdk.control_calls()
                )
            },
            || met(self),
        )
        .await;
    }

    /// Waits until the latest state is in `phase` and, if given, has `transmitting`.
    pub async fn until_phase(&self, phase: &str) {
        self.until(&format!("phase {phase}"), |rig| {
            rig.state()
                .is_some_and(|state| state.phase.as_str() == phase)
        })
        .await;
    }

    /// Waits until a press would transmit: connected, capturing, the key device read.
    pub async fn ready(&self) {
        self.until("ready to talk", |rig| {
            rig.state().is_some_and(|state| {
                state.phase.as_str() == "connected"
                    && state.blocked.is_empty()
                    && state.ptt.as_str() == "ready"
                    && state.capture.as_str() == "on"
            })
        })
        .await;
    }

    /// Waits until the helper is joined, capturing, and reading the key device, whatever
    /// else blocks a press.
    pub async fn joined(&self) {
        self.until("joined", |rig| {
            rig.state().is_some_and(|state| {
                state.phase.as_str() == "connected"
                    && state.ptt.as_str() == "ready"
                    && state.capture.as_str() == "on"
            })
        })
        .await;
    }

    /// Waits until the latest state says audio is leaving.
    pub async fn transmitting(&self) {
        self.until("transmitting", |rig| {
            rig.state().is_some_and(|state| state.transmitting)
        })
        .await;
    }

    /// Waits until command `id` is answered, and gives the answer.
    pub async fn ack(&self, id: i64) -> conch_voice_api::VoiceControlAckV1 {
        let found = Arc::new(Mutex::new(None));
        let kept = Arc::clone(&found);
        self.until(&format!("the ack of {id}"), move |rig| {
            let ack = rig.read().into_iter().find_map(|event| match event {
                VoiceControlEvent::Ack(ack) if ack.id == id => Some(ack),
                _ => None,
            });
            let met = ack.is_some();
            *kept.lock().unwrap() = ack;
            met
        })
        .await;
        found.lock().unwrap().take().unwrap()
    }

    /// How many times the microphone was opened.
    pub fn mic_opened(&self) -> u32 {
        self.mic_opened.load(Ordering::SeqCst)
    }

    /// How many microphones were closed.
    pub fn mic_closed(&self) -> u32 {
        self.mic_closed.load(Ordering::SeqCst)
    }

    /// How many speakers were closed.
    pub fn sink_closed(&self) -> u32 {
        self.sink_closed.load(Ordering::SeqCst)
    }

    /// Whether the helper has ended.
    pub fn has_ended(&self) -> bool {
        self.task.as_ref().is_none_or(JoinHandle::is_finished)
    }

    /// Waits for the helper to end, and gives the exit code `main` would end with.
    pub async fn exit_code(&mut self) -> i32 {
        let task = self.task.take().expect("waited for twice");
        let result = tokio::time::timeout(PATIENCE, task)
            .await
            .unwrap_or_else(|_| panic!("the helper did not end\n{}", self.events.text()))
            .unwrap();
        match result {
            Ok(()) => 0,
            Err(error) => i32::from(error.exit_code()),
        }
    }
}
