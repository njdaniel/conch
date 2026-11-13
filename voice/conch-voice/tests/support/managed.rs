//! What the tests of `conch-voice managed` stand on: a [`ManagedRig`] that runs the real
//! managed helper (`conch_voice::managed::run`) between a fake SDK and a stub `conchd`, with
//! a named pipe for a keyboard, a tone for a microphone that never stops, and the owner's
//! two pipes in the test's hands: it writes commands, reads events and closes standard
//! input as `conch` would. With a [`World`], the microphones and speakers are opened by the
//! name the helper asks for, and the test says which of them are there and whether the
//! microphones hear anything: for `conch.voice_control.v2`'s device switch and open
//! microphone.
//!
//! On Unix the events go to an in-memory buffer and the keyboard is a `mkfifo`. On Windows
//! (#1766) the events go through a real named pipe: a tokio `NamedPipeServer` the rig holds
//! as the owner, reached by `channel::attach` as `conch-voice managed --events-pipe` reaches
//! it, with the attach line read and checked before the helper's hello. The keyboard on
//! Windows (#1791) is a named pipe the rig creates (`win_keys`), which the helper reads
//! through its hidden `--test-key-device` as it reads a `mkfifo` on Linux: Windows
//! push-to-talk is Raw Input, and the test seam of `keys_windows` (`PipeSource`) reads the
//! fake transitions the rig writes to it in place of Raw Input.
//!
//! No test here needs a microphone, PipeWire, a keyboard or LiveKit.

#[cfg(unix)]
use std::fs::File;
#[cfg(unix)]
use std::fs::OpenOptions;
use std::io::{PipeWriter, Write};
#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::Error;
use conch_voice::cli::Environment;
use conch_voice::livekit::ShareAudioOwner;
use conch_voice::managed::{self, Devices, Host, KeySource, Watchdog};
use conch_voice::receive::BoxedSink;
use conch_voice::secrets::Scrubber;
use conch_voice::session::Input;
use conch_voice::transmit::BoxedMic;
use conch_voice_api::{
    Secret, VoiceControlAckV2, VoiceControlEvent, VoiceControlEventV2, VoiceControlHelloV1,
    VoiceControlStateV2, parse_event, parse_event_v2,
};
use conch_voice_audio::{CountingSink, FRAME_LEN, Frame, MicRead, MicSource, ToneSource};
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio::task::JoinHandle;

use super::fake::FakeSdk;
#[cfg(unix)]
use super::keyboard::Pipe;
use super::keyboard::quick_keys;
#[cfg(unix)]
use super::keyboard::{DEAFEN, MUTE, TALK, press, release};
use super::stub::{FAKE_LOGIN, OWN_PRINCIPAL, Stub};
use super::{Reader, Written, quick};

/// The channel every managed test joins.
pub const CHANNEL: &str = "general";

/// How long a test waits for something that should happen at once before it fails.
const PATIENCE: Duration = Duration::from_secs(10);

/// What a rig's `voice.toml` binds: the three keys of `keyboard.rs`, a short release tail.
#[cfg(unix)]
pub const KEYS_TOML: &str = "[keys]\ntalk = \"KEY_F13\"\nmute = \"KEY_F14\"\ndeafen = \"KEY_F15\"\n\n[audio]\nrelease_tail_ms = 20\n";

/// What a rig's `voice.toml` binds on Windows: the same three keys, in Windows' grammar (the
/// Linux form is refused there), as the scan codes of F13, F14 and F15, which `win_keys`
/// writes. A short release tail.
#[cfg(not(unix))]
pub const KEYS_TOML: &str = "[keys]\ntalk = \"sc:0x64\"\nmute = \"sc:0x65\"\ndeafen = \"sc:0x66\"\n\n[audio]\nrelease_tail_ms = 20\n";

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
    /// Open the keyboard's end of the key device's pipe only when the test plugs it in
    /// ([`ManagedRig::plug_keyboard`]): until then the key device's watcher waits to open
    /// it, and the start-up waits for the watcher.
    pub keyboard_unplugged: bool,
    pub start_timeout: Duration,
    /// Start the helper with `--no-mute-key` (#1631).
    pub no_mute_key: bool,
    /// Microphones and speakers opened by name, as the test arranges them; `None` for the
    /// tone and the counting sink, whatever is asked for.
    pub world: Option<World>,
}

impl Default for ManagedSetup {
    fn default() -> Self {
        Self {
            login: Login::Stored,
            voice_toml: Some(KEYS_TOML.to_owned()),
            keyboard: true,
            keyboard_unplugged: false,
            start_timeout: Duration::from_secs(10),
            no_mute_key: false,
            world: None,
        }
    }
}

/// The level of every sample of a loud frame from the microphone of this name: the
/// default's, and every named one's. A frame's first sample says which it came from
/// (`FakeSdk::first_samples`). Both are far above the default open-mode level.
pub fn level_of(name: Option<&str>) -> f32 {
    match name {
        None => 0.25,
        Some(_) => 0.5,
    }
}

/// The three keys a rig's `voice.toml` binds.
#[derive(Debug, Clone, Copy)]
enum Wanted {
    Talk,
    Mute,
    Deafen,
}

/// The bytes the helper's key source reads for `key` going down or up: `input_event` records
/// for the evdev watcher on Unix, the four-byte records of `keys_windows`'s test seam on
/// Windows. Either way the helper's expected bytes are the same.
fn transition(key: Wanted, down: bool) -> Vec<u8> {
    #[cfg(unix)]
    {
        let code = match key {
            Wanted::Talk => TALK,
            Wanted::Mute => MUTE,
            Wanted::Deafen => DEAFEN,
        };
        if down { press(code) } else { release(code) }
    }
    #[cfg(windows)]
    {
        win_keys::record(win_keys::KEY, win_keys::scan(key), down).to_vec()
    }
}

/// The test's hold on a rig's microphones and speakers: which of them are not there, by
/// name, and whether the microphones hear sound. Every name is there unless the test says
/// it is not; PipeWire's default (`None`) always is.
#[derive(Clone, Default)]
pub struct World(Arc<WorldState>);

#[derive(Default)]
struct WorldState {
    absent: Mutex<Vec<String>>,
    broken: Mutex<Vec<String>>,
    quiet: AtomicBool,
    frames: AtomicU64,
    mic_names: Mutex<Vec<Option<String>>>,
    sink_names: Mutex<Vec<Option<String>>>,
}

impl World {
    /// The device of this name is not there (unplugged, or never was).
    pub fn unplug(&self, name: &str) {
        self.0.absent.lock().unwrap().push(name.to_owned());
    }

    /// The device of this name cannot be opened: its opener fails, with an error that
    /// names it, as a PipeWire error may.
    pub fn break_device(&self, name: &str) {
        self.0.broken.lock().unwrap().push(name.to_owned());
    }

    /// The opener's error for a device that cannot be opened, if this is one.
    fn refuse(&self, doing: &'static str, name: Option<&str>) -> Result<(), Error> {
        match name {
            Some(name) if self.0.broken.lock().unwrap().iter().any(|b| b == name) => {
                Err(Error::AudioSystem {
                    doing,
                    detail: format!("FAKE: no node is named {name}"),
                })
            }
            _ => Ok(()),
        }
    }

    /// The device of this name is there.
    pub fn plug(&self, name: &str) {
        self.0
            .absent
            .lock()
            .unwrap()
            .retain(|absent| absent != name);
    }

    fn is_there(&self, name: Option<&str>) -> bool {
        name.is_none_or(|name| !self.0.absent.lock().unwrap().iter().any(|a| a == name))
    }

    /// Whether every microphone hears sound from its next frame, or silence.
    pub fn loud(&self, loud: bool) {
        self.0.quiet.store(!loud, Ordering::SeqCst);
    }

    /// Frames read from any microphone so far.
    pub fn frames(&self) -> u64 {
        self.0.frames.load(Ordering::SeqCst)
    }

    /// Waits until `count` more frames have been read: time as the level detector counts
    /// it, however late the task that reads them is.
    pub async fn frames_pass(&self, count: u64) {
        let until = self.frames() + count;
        super::until("frames read", String::new, || self.frames() >= until).await;
    }

    /// The names the microphone was opened with, in order: `None` for the default.
    pub fn mic_names(&self) -> Vec<Option<String>> {
        self.0.mic_names.lock().unwrap().clone()
    }

    /// The names the speakers were opened with, in order.
    pub fn sink_names(&self) -> Vec<Option<String>> {
        self.0.sink_names.lock().unwrap().clone()
    }
}

/// A microphone of a [`World`]: read every 10 ms, a frame at its own level while the world
/// is loud and silence while it is quiet; no microphone at all while it is not there.
struct WorldMic {
    name: Option<String>,
    world: World,
}

impl MicSource for WorldMic {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        if !self.world.is_there(self.name.as_deref()) {
            return MicRead::NoMicrophone;
        }
        let quiet = self.world.0.quiet.load(Ordering::SeqCst);
        let level = if quiet {
            0.0
        } else {
            level_of(self.name.as_deref())
        };
        *frame = [level; FRAME_LEN];
        self.world.0.frames.fetch_add(1, Ordering::SeqCst);
        MicRead::Ready
    }
}

/// Speakers of a [`World`]: playing while they are there.
struct WorldSink {
    name: Option<String>,
    world: World,
    sink: CountingSink,
    closed: Arc<AtomicU32>,
}

impl conch_voice_audio::SpeakerSink for WorldSink {
    fn play(&mut self, frame: &conch_voice_audio::Frame) {
        self.sink.play(frame);
    }
}

impl conch_voice::receive::Speakers for WorldSink {
    fn playing(&self) -> bool {
        self.world.is_there(self.name.as_deref())
    }
}

impl Drop for WorldSink {
    fn drop(&mut self) {
        self.closed.fetch_add(1, Ordering::SeqCst);
    }
}

/// One event of a v2 session: the hello, which is v1, and then v2's.
#[derive(Debug, Clone)]
pub enum EventV2 {
    Hello(VoiceControlHelloV1),
    V2(VoiceControlEventV2),
}

impl EventV2 {
    pub fn seq(&self) -> i64 {
        match self {
            EventV2::Hello(hello) => hello.seq,
            EventV2::V2(event) => event.seq(),
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

impl conch_voice::receive::Speakers for Silenced {}

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
    /// The keyboard's end of the key device's pipe, if the rig reads keys and it is plugged in.
    keyboard: Option<Keyboard>,
    /// Whether the rig has a key device at all.
    reads_keys: bool,
    /// Windows: what the helper wrote to the named pipe against what the owner has read of it.
    #[cfg(windows)]
    owner: owner_pipe::Owner,
    mic_opened: Arc<AtomicU32>,
    mic_closed: Arc<AtomicU32>,
    sink_closed: Arc<AtomicU32>,
    task: Option<JoinHandle<Result<(), Error>>>,
    /// Holds the configuration directory and the pipe for as long as the rig lives.
    _dir: tempfile::TempDir,
    #[cfg(unix)]
    pipe: Pipe,
    /// Windows: the name of the key device's pipe.
    #[cfg(windows)]
    key_pipe: String,
}

/// The keyboard's end of the key device's pipe: a file on Unix, a writer on Windows.
#[cfg(unix)]
type Keyboard = File;
#[cfg(windows)]
type Keyboard = win_keys::Keyboard;

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
            #[cfg(unix)]
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        }
        if let Some(text) = &setup.voice_toml {
            let path = dir.path().join("voice.toml");
            std::fs::write(&path, text).unwrap();
            #[cfg(unix)]
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
            ..Environment::default()
        };

        // The keyboard's end is opened to read and write, so that it does not wait for the
        // watcher, and the watcher's open does not wait for it.
        let reads_keys = setup.keyboard;
        #[cfg(unix)]
        let pipe = Pipe::new();
        #[cfg(unix)]
        let keyboard = (setup.keyboard && !setup.keyboard_unplugged).then(|| {
            OpenOptions::new()
                .read(true)
                .write(true)
                .open(&pipe.path)
                .unwrap()
        });
        #[cfg(windows)]
        let key_pipe = win_keys::pipe_name();
        #[cfg(windows)]
        let keyboard = (setup.keyboard && !setup.keyboard_unplugged)
            .then(|| win_keys::Keyboard::plug(&key_pipe));

        let mic_opened = Arc::new(AtomicU32::new(0));
        let mic_closed = Arc::new(AtomicU32::new(0));
        let sink_closed = Arc::new(AtomicU32::new(0));
        let (opened, closed, silenced) = (
            Arc::clone(&mic_opened),
            Arc::clone(&mic_closed),
            Arc::clone(&sink_closed),
        );
        let devices = match setup.world.clone() {
            None => Devices::Given {
                // A tone that never stops: whatever does not reach the fake's audio source
                // was stopped by the gate.
                open_mic: Box::new(move |_| {
                    opened.fetch_add(1, Ordering::SeqCst);
                    let tone = ToneSource::new(440.0, 0.25)?;
                    Ok(Some(
                        Box::new(Closing(tone, Arc::clone(&closed))) as BoxedMic
                    ))
                }),
                open_sink: Box::new(move |_| {
                    Ok(Box::new(Silenced(
                        CountingSink::new(&[440.0])?,
                        Arc::clone(&silenced),
                    )) as BoxedSink)
                }),
                sink_tones: vec![440.0],
            },
            Some(world) => {
                let sinks = world.clone();
                Devices::Given {
                    open_mic: Box::new(move |name| {
                        opened.fetch_add(1, Ordering::SeqCst);
                        world.refuse("open the microphone", name)?;
                        let name = name.map(str::to_owned);
                        world.0.mic_names.lock().unwrap().push(name.clone());
                        let mic = WorldMic {
                            name,
                            world: world.clone(),
                        };
                        Ok(Some(Box::new(Closing(mic, Arc::clone(&closed))) as BoxedMic))
                    }),
                    open_sink: Box::new(move |name| {
                        sinks.refuse("open the speakers", name)?;
                        let name = name.map(str::to_owned);
                        sinks.0.sink_names.lock().unwrap().push(name.clone());
                        Ok(Box::new(WorldSink {
                            name,
                            world: sinks.clone(),
                            sink: CountingSink::new(&[440.0])?,
                            closed: Arc::clone(&silenced),
                        }) as BoxedSink)
                    }),
                    sink_tones: vec![440.0],
                }
            }
        };
        let mut timings = quick();
        // A wait the policy asks for is a few milliseconds, as in the other tests.
        timings.wait_divisor = 200;
        #[cfg(unix)]
        let keys = if setup.keyboard {
            KeySource::NamedPipe(pipe.path.clone())
        } else {
            KeySource::Configured
        };
        #[cfg(windows)]
        let keys = if setup.keyboard {
            KeySource::NamedPipe(std::path::PathBuf::from(&key_pipe))
        } else {
            KeySource::Configured
        };
        let host = Host {
            environment,
            transport: sdk.clone(),
            scrubber: Arc::clone(&scrubber),
            devices,
            keys,
            share_audio_owner: ShareAudioOwner::Helper,
            transmit_ceiling_secs: None,
            no_mute_key: setup.no_mute_key,
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
        // Unix: the owner's reader is the sink. Windows: a real named pipe is, and a thread
        // of the rig reads it into the reader.
        #[cfg(unix)]
        let out: Box<dyn Write + Send> = Box::new(reader.clone());
        #[cfg(windows)]
        let (out, owner) =
            owner_pipe::attach(&reader, &scrubber, &inputs, host.watchdog.clone()).await;
        let task = tokio::spawn(managed::run(host, stdin, out, inputs.clone(), received));
        Self {
            sdk,
            conchd,
            scrubber,
            events,
            reader,
            stdin: Some(writer),
            inputs,
            keyboard,
            reads_keys,
            #[cfg(windows)]
            owner,
            mic_opened,
            mic_closed,
            sink_closed,
            task: Some(task),
            _dir: dir,
            #[cfg(unix)]
            pipe,
            #[cfg(windows)]
            key_pipe,
        }
    }

    /// The keyboard goes away: its end of the key device's pipe is closed, and the
    /// watcher reads the end of it.
    pub fn unplug_keyboard(&mut self) {
        self.keyboard = None;
    }

    /// The keyboard is there (again): its end of the pipe is opened.
    pub fn plug_keyboard(&mut self) {
        #[cfg(unix)]
        {
            self.keyboard = Some(
                OpenOptions::new()
                    .read(true)
                    .write(true)
                    .open(&self.pipe.path)
                    .unwrap(),
            );
        }
        #[cfg(windows)]
        {
            self.keyboard = Some(win_keys::Keyboard::plug(&self.key_pipe));
        }
    }

    /// The start of a v2 session: the usual one, written in `conch.voice_control.v2`.
    pub fn start_command_v2(&self) -> Value {
        let mut start = self.start_command();
        start["schema"] = json!("conch.voice_control.v2");
        start
    }

    /// Writes the v2 start.
    pub fn send_start_v2(&mut self) {
        let start = self.start_command_v2();
        self.send(&start);
    }

    /// A v2 command of `kind` with `id`, and these fields besides.
    pub fn command_v2(kind: &str, id: i64, fields: Value) -> Value {
        let mut command = Self::command(kind, id, fields);
        command["schema"] = json!("conch.voice_control.v2");
        command
    }

    /// Every event so far of a v2 session, read by the owner's rules: the hello by v1's
    /// reader, everything after it by v2's, which is Go's `ParseVoiceControlEventV2` and
    /// its `Validate`. A line either would refuse fails the test.
    pub fn read_v2(&self) -> Vec<EventV2> {
        self.lines()
            .iter()
            .enumerate()
            .map(|(at, line)| {
                let refused = |refusal: conch_voice_api::VoiceControlRefusal| -> ! {
                    panic!("the owner refuses {line}: {refusal}")
                };
                if at == 0 {
                    match parse_event(line.as_bytes()).unwrap_or_else(|r| refused(r)) {
                        VoiceControlEvent::Hello(hello) => EventV2::Hello(hello),
                        other => panic!("the first event is not a hello: {other:?}"),
                    }
                } else {
                    EventV2::V2(parse_event_v2(line.as_bytes()).unwrap_or_else(|r| refused(r)))
                }
            })
            .collect()
    }

    /// Every v2 state so far.
    pub fn states_v2(&self) -> Vec<VoiceControlStateV2> {
        self.read_v2()
            .into_iter()
            .filter_map(|event| match event {
                EventV2::V2(VoiceControlEventV2::State(state)) => Some(state),
                _ => None,
            })
            .collect()
    }

    /// The latest v2 state, if there is one.
    pub fn state_v2(&self) -> Option<VoiceControlStateV2> {
        self.states_v2().pop()
    }

    /// Waits until v2 command `id` is answered, and gives the answer.
    pub async fn ack_v2(&self, id: i64) -> VoiceControlAckV2 {
        let found = Arc::new(Mutex::new(None));
        let kept = Arc::clone(&found);
        self.until(&format!("the ack of {id}"), move |rig| {
            let ack = rig.read_v2().into_iter().find_map(|event| match event {
                EventV2::V2(VoiceControlEventV2::Ack(ack)) if ack.id == id => Some(ack),
                _ => None,
            });
            let met = ack.is_some();
            *kept.lock().unwrap() = ack;
            met
        })
        .await;
        found.lock().unwrap().take().unwrap()
    }

    /// Waits until the latest v2 state has `met` true of it.
    pub async fn until_v2(&self, what: &str, met: impl Fn(&VoiceControlStateV2) -> bool) {
        self.until(what, |rig| rig.state_v2().is_some_and(|state| met(&state)))
            .await;
    }

    /// Waits until, in a v2 session, a press or an onset would transmit: connected,
    /// capturing, nothing in the way.
    pub async fn ready_v2(&self) {
        self.until_v2("ready to transmit", |state| {
            state.phase.as_str() == "connected"
                && state.blocked.is_empty()
                && state.capture.as_str() == "on"
                && !state.transmitting
        })
        .await;
    }

    /// Waits until the latest v2 state says audio is leaving, or that it is not.
    pub async fn transmitting_v2(&self, transmitting: bool) {
        self.until_v2(&format!("transmitting {transmitting}"), |state| {
            state.transmitting == transmitting
        })
        .await;
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
        #[cfg(unix)]
        keyboard.write_all(bytes).unwrap();
        #[cfg(windows)]
        keyboard.write(bytes);
    }

    pub fn press_talk(&mut self) {
        self.keys(&transition(Wanted::Talk, true));
    }

    pub fn release_talk(&mut self) {
        self.keys(&transition(Wanted::Talk, false));
    }

    /// The mute key, pressed and released: a toggle.
    pub fn strike_mute(&mut self) {
        let mut bytes = transition(Wanted::Mute, true);
        bytes.extend(transition(Wanted::Mute, false));
        self.keys(&bytes);
    }

    pub fn strike_deafen(&mut self) {
        let mut bytes = transition(Wanted::Deafen, true);
        bytes.extend(transition(Wanted::Deafen, false));
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
                    && rig.ptt_is_as_it_can_be(state.ptt.as_str())
                    && state.capture.as_str() == "on"
            })
        })
        .await;
    }

    /// Whether the state's push-to-talk is as good as this rig's gets: `ready` with a key
    /// device, `not_configured` without one (on Windows, and where a test asks for none).
    fn ptt_is_as_it_can_be(&self, ptt: &str) -> bool {
        ptt == "ready" || (!self.reads_keys && ptt == "not_configured")
    }

    /// Waits until the helper is joined, capturing, and reading the key device, whatever
    /// else blocks a press.
    pub async fn joined(&self) {
        self.until("joined", |rig| {
            rig.state().is_some_and(|state| {
                state.phase.as_str() == "connected"
                    && rig.ptt_is_as_it_can_be(state.ptt.as_str())
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

    /// Windows: how many threads of the owner's end are running; both end when the rig drops.
    #[cfg(windows)]
    pub fn owner_threads(&self) -> Arc<AtomicU32> {
        self.owner.threads()
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
        // The helper's last write is in the pipe; the owner has it once its reader has.
        #[cfg(windows)]
        self.owner.drained(PATIENCE).await;
        match result {
            Ok(()) => 0,
            Err(error) => i32::from(error.exit_code()),
        }
    }
}

/// The owner's end of the events endpoint on Windows: a real named pipe, as the desktop app's
/// owner holds one, in place of the in-memory buffer the Unix rig writes to.
#[cfg(windows)]
mod owner_pipe {
    use std::path::PathBuf;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
    use std::time::{Duration, Instant};

    use conch_voice::channel;
    use conch_voice::cli::Environment;
    use conch_voice::managed::Watchdog;
    use conch_voice::secrets::Scrubber;
    use conch_voice::session::Input;
    use conch_voice_api::{Secret, parse_attached};
    use tokio::io::AsyncReadExt;
    use tokio::net::windows::named_pipe::ServerOptions;
    use tokio::sync::{mpsc, oneshot};

    use super::super::Reader;
    use super::PATIENCE;

    /// A fake nonce from `pkg/schema/testdata/voice-control-pipe/sensitive.json`.
    const FAKE_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";

    /// How much the helper wrote to the pipe, and how much of it the owner has read.
    #[derive(Clone, Default)]
    struct Counts {
        written: Arc<AtomicU64>,
        read: Arc<AtomicU64>,
    }

    /// The helper's event writer, counting what reaches the pipe.
    struct Counted<W> {
        inner: W,
        written: Arc<AtomicU64>,
    }

    impl<W: std::io::Write> std::io::Write for Counted<W> {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            let wrote = self.inner.write(buf)?;
            self.written.fetch_add(wrote as u64, Ordering::SeqCst);
            Ok(wrote)
        }

        fn flush(&mut self) -> std::io::Result<()> {
            self.inner.flush()
        }
    }

    /// Counts a thread of the owner's end down when it ends, however it ends.
    struct Alive(Arc<AtomicU32>);

    impl Drop for Alive {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// The helper's end of the pipe as `watch_end` reads it, with its thread counted.
    struct Watching<R> {
        inner: R,
        _alive: Alive,
    }

    impl<R: std::io::Read> std::io::Read for Watching<R> {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            self.inner.read(buf)
        }
    }

    /// What the rig keeps of the pipe: the counts, to tell when the owner has read all, and
    /// the hold on the owner's reading thread: dropping this ends it, and with it the
    /// pipe, which ends the thread that watches the helper's end.
    pub struct Owner {
        counts: Counts,
        /// Threads of the owner's end still running: the reader and the end's watcher.
        alive: Arc<AtomicU32>,
        _cancel: oneshot::Sender<()>,
    }

    impl Owner {
        /// How many threads of the owner's end are still running; the rig's drop ends them.
        pub fn threads(&self) -> Arc<AtomicU32> {
            Arc::clone(&self.alive)
        }

        /// Waits until the owner has read everything the helper wrote to the pipe, or `limit`.
        /// Not an error to run out: the owner may be stopped, and the test sees what it sees.
        pub async fn drained(&self, limit: Duration) {
            let until = Instant::now() + limit;
            while self.counts.read.load(Ordering::SeqCst)
                < self.counts.written.load(Ordering::SeqCst)
                && Instant::now() < until
            {
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
        }
    }

    /// A pipe name no other test, and no other run, uses.
    fn pipe_name() -> String {
        static NEXT: AtomicU32 = AtomicU32::new(0);
        format!(
            r"\\.\pipe\conch-voice-rig-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        )
    }

    /// Makes the owner's pipe, attaches the helper's end to it with `channel::attach` as
    /// `managed()` does (the attach line is checked here and is not among the events), and
    /// has a thread read what the helper writes into `reader`. Returns the helper's event
    /// writer. The pipe's end is watched as `managed()` watches it.
    pub async fn attach(
        reader: &Reader,
        scrubber: &Arc<Scrubber>,
        inputs: &mpsc::UnboundedSender<Input>,
        watchdog: Watchdog,
    ) -> (Box<dyn std::io::Write + Send>, Owner) {
        let name = pipe_name();
        let mut server = ServerOptions::new()
            .first_pipe_instance(true)
            .reject_remote_clients(true)
            .create(&name)
            .unwrap();
        let environment = Environment {
            conch_voice_attach: Some(Secret::new(FAKE_NONCE)),
            ..Environment::default()
        };
        // `attach` writes the attach line through the runtime and so must not be on one of
        // its threads.
        let (path, helper_scrubber) = (PathBuf::from(&name), Arc::clone(scrubber));
        let attaching = tokio::task::spawn_blocking(move || {
            channel::attach(&path, &environment, &helper_scrubber)
        });
        tokio::time::timeout(PATIENCE, server.connect())
            .await
            .unwrap_or_else(|_| panic!("the helper's end did not connect"))
            .unwrap();
        let endpoint = attaching
            .await
            .unwrap()
            .unwrap_or_else(|error| panic!("the helper's end did not attach: {error}"));

        // The attach line, then whatever came with it.
        let mut got = Vec::new();
        let mut chunk = [0_u8; 4096];
        let line_end = loop {
            if let Some(at) = got.iter().position(|byte| *byte == b'\n') {
                break at;
            }
            let read = tokio::time::timeout(PATIENCE, server.read(&mut chunk))
                .await
                .unwrap_or_else(|_| panic!("no attach line"))
                .unwrap();
            assert_ne!(read, 0, "the helper's end closed before its attach line");
            got.extend_from_slice(&chunk[..read]);
        };
        let attached = parse_attached(&got[..line_end]).unwrap();
        assert!(attached.matches(FAKE_NONCE), "the attach line");
        assert_eq!(
            line_end + 1,
            got.len(),
            "bytes followed the attach line before the session started"
        );
        let (cancel, cancelled) = oneshot::channel::<()>();
        let alive = Arc::new(AtomicU32::new(2));

        let counts = Counts::default();
        let pumped = Arc::clone(&counts.read);
        let mut reader = reader.clone();
        let handle = tokio::runtime::Handle::current();
        let reading = Alive(Arc::clone(&alive));
        std::thread::spawn(move || {
            use std::io::Write;
            let _reading = reading;
            let read = pumped;
            let mut cancelled = cancelled;
            let mut chunk = [0_u8; 4096];
            loop {
                // The owner is dropped: `server` is dropped with this thread, which is what
                // the helper's end (watched by `watch_end`) sees end.
                let step = handle.block_on(async {
                    tokio::select! {
                        read = server.read(&mut chunk) => Some(read),
                        _ = &mut cancelled => None,
                    }
                });
                match step {
                    Some(Ok(count)) if count > 0 => {
                        if reader.write_all(&chunk[..count]).is_err() {
                            return;
                        }
                        read.fetch_add(count as u64, Ordering::SeqCst);
                    }
                    _ => return,
                }
            }
        });
        let end = Watching {
            inner: endpoint.end,
            _alive: Alive(Arc::clone(&alive)),
        };
        channel::watch_end(end, inputs.clone(), watchdog);
        let events = Counted {
            inner: endpoint.events,
            written: Arc::clone(&counts.written),
        };
        (
            Box::new(events),
            Owner {
                counts,
                alive,
                _cancel: cancel,
            },
        )
    }
}

/// The keyboard on Windows (#1791): a named pipe the rig creates, one-way, whose client is the
/// helper (`conch-voice managed --test-key-device`, through `keys_windows`'s test seam). What
/// the rig writes is that seam's four-byte records: the kind, a little-endian `u16` (a scan
/// code, or a mouse button's number) and `1` for down or `0` for up.
#[cfg(windows)]
mod win_keys {
    use std::sync::atomic::{AtomicU32, Ordering};

    use tokio::io::AsyncWriteExt;
    use tokio::net::windows::named_pipe::ServerOptions;
    use tokio::sync::{mpsc, oneshot};

    use super::Wanted;

    /// The kind of record that is a key going down or up.
    pub const KEY: u8 = 1;

    /// The scan codes of F13, F14 and F15, as [`super::KEYS_TOML`] binds them.
    pub fn scan(key: Wanted) -> u16 {
        match key {
            Wanted::Talk => 0x64,
            Wanted::Mute => 0x65,
            Wanted::Deafen => 0x66,
        }
    }

    /// One record of the seam.
    pub fn record(kind: u8, value: u16, down: bool) -> [u8; 4] {
        let [low, high] = value.to_le_bytes();
        [kind, low, high, u8::from(down)]
    }

    /// A pipe name no other test, and no other run, uses, and nobody can guess: the pipe
    /// namespace is the machine's, and whoever creates a name first serves it.
    pub fn pipe_name() -> String {
        use std::hash::{BuildHasher, Hasher};
        static NEXT: AtomicU32 = AtomicU32::new(0);
        let unguessable = std::collections::hash_map::RandomState::new()
            .build_hasher()
            .finish();
        format!(
            r"\\.\pipe\conch-voice-rig-keys-{}-{}-{unguessable:016x}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        )
    }

    /// A keyboard that is plugged in: the server end of the pipe, written by a task of the
    /// runtime. Dropping it is unplugging: what was written is read first, and then the
    /// helper's end reads the end of the pipe.
    pub struct Keyboard {
        bytes: mpsc::UnboundedSender<Vec<u8>>,
        /// Dropping this ends a task that is still waiting for the helper to open the pipe.
        _cancel: oneshot::Sender<()>,
    }

    impl Keyboard {
        /// Creates the pipe `name`, and writes the release of each key, so that each is armed
        /// (a Windows key's first press only arms it, `keys_windows`; a Linux keyboard has no
        /// such rule) and, after an unplugging, the talk key's release reads the keys again.
        /// Must be called from within the runtime.
        pub fn plug(name: &str) -> Self {
            let server = ServerOptions::new()
                .first_pipe_instance(true)
                .reject_remote_clients(true)
                .access_inbound(false)
                .create(name)
                .unwrap();
            let (bytes, mut written) = mpsc::unbounded_channel::<Vec<u8>>();
            let (cancel, mut cancelled) = oneshot::channel::<()>();
            tokio::spawn(async move {
                let mut server = server;
                tokio::select! {
                    connected = server.connect() => {
                        if connected.is_err() {
                            return;
                        }
                    }
                    _ = &mut cancelled => return,
                }
                while let Some(more) = written.recv().await {
                    if server.write_all(&more).await.is_err() {
                        return;
                    }
                }
                // The keyboard is unplugged: the helper reads what is left, and then the end.
                let _ = server.flush().await;
            });
            let keyboard = Self {
                bytes,
                _cancel: cancel,
            };
            let arm = [Wanted::Talk, Wanted::Mute, Wanted::Deafen]
                .into_iter()
                .flat_map(|key| record(KEY, scan(key), false))
                .collect::<Vec<u8>>();
            keyboard.write(&arm);
            keyboard
        }

        /// Writes to the pipe, after what was written before.
        pub fn write(&self, bytes: &[u8]) {
            self.bytes.send(bytes.to_vec()).unwrap();
        }
    }
}
