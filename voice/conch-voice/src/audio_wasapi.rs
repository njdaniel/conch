//! The Windows backend's contact with the sound system: WASAPI, through the `wasapi` crate,
//! behind the two traits of `audio_windows.rs` ([`Audio`], [`Stream`]). Compiled only under
//! `cfg(windows)`; every decision that is not a call of the crate is in `audio_windows.rs`,
//! which is compiled and tested everywhere.
//!
//! Input: the system's audio endpoints and its notifications. Output: [`Wasapi`], which opens
//! a [`WasapiMic`] and [`WasapiSpeakers`], each on a worker thread of its own, and lists the
//! endpoints as a neutral table. Owns: the COM apartment of each worker thread, from the
//! moment the thread makes its [`System`] until the thread ends, and every object made in it.
//!
//! What it asks of the engine. Shared mode, event-driven, at the device's default period (10 ms
//! on a normal device), 48 kHz `f32`, mono: the engine converts the rate and the channels
//! (`AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM`, which `wasapi` exposes as `autoconvert`), so no
//! resampler is needed and none is in the tree. A driver that refuses mono is asked for
//! stereo, and the helper mixes the channels (`decode_mono`, `encode_mono`: no rate change).
//! One that refuses both is [`Failure::Unsupported`] and the open fails with words saying so.
//! Whether real devices accept this, at what period, is E3 of the design, still to be done on
//! hardware (#903); the `#[ignore]` test at the end of this file is for that run.
//!
//! COM. `wasapi`'s objects are COM interfaces; an `IMMDevice` and what is made from it belong
//! to the apartment of the thread that made them. So a [`System`] is made in the worker's own
//! thread, never moves, and is dropped there, its COM objects before the apartment goes.
//! The notification handlers run on a thread of the system's own and only [`post`].

use std::time::Duration;

use wasapi::{
    AudioCaptureClient, AudioClient, AudioRenderClient, BufferFlags, DeviceEnumerator,
    DeviceEventCallbacks, DeviceEventRegistration, Direction, Handle, Role, SampleType, StreamMode,
    WasapiError, WaveFormat, deinitialize, initialize_mta,
};

use super::windows::{
    Audio, Endpoint, Failure, Kind, Notice, Poster, Stream, Target, WasapiMic, WasapiSpeakers,
    decode_mono, default_changed, encode_mono, failure_from_hresult, list, post,
};
use conch_voice_audio::SAMPLE_RATE;

use crate::devices::DeviceTable;
use crate::error::Error;

/// What the engine is asked for: 32-bit `f32` at the helper's rate.
fn format(channels: usize) -> WaveFormat {
    WaveFormat::new(
        32,
        32,
        &SampleType::Float,
        SAMPLE_RATE as usize,
        channels,
        None,
    )
}

fn direction(kind: Kind) -> Direction {
    match kind {
        Kind::Capture => Direction::Capture,
        Kind::Render => Direction::Render,
    }
}

fn kind_of(direction: Direction) -> Kind {
    match direction {
        Direction::Capture => Kind::Capture,
        Direction::Render => Kind::Render,
    }
}

/// Sorts what `wasapi` said into what the worker does about it. Nothing the system said about
/// a device is kept: the failures hold none of it.
fn classify(error: &WasapiError) -> Failure {
    match error {
        WasapiError::Windows(error) => failure_from_hresult(error.code().0),
        WasapiError::DeviceNotFound(_) => Failure::Gone,
        WasapiError::UnsupportedFormat => Failure::Unsupported,
        _ => Failure::Other("the sound system failed".to_owned()),
    }
}

/// The calling thread's membership of the multi-threaded apartment, which `wasapi`'s objects
/// need. Left when this is dropped.
struct Com;

impl Com {
    fn join() -> Result<Self, Failure> {
        initialize_mta()
            .ok()
            .map_err(|error| failure_from_hresult(error.code().0))?;
        Ok(Self)
    }
}

impl Drop for Com {
    fn drop(&mut self) {
        deinitialize();
    }
}

/// The sound system, as one worker thread uses it.
pub(crate) struct System {
    // Declaration order is drop order: what lives in the apartment goes before it does.
    registration: Option<DeviceEventRegistration>,
    enumerator: DeviceEnumerator,
    _com: Com,
}

impl System {
    /// Joins the apartment and makes the enumerator. Call it in the thread that will use it.
    pub(crate) fn new() -> Result<Self, Failure> {
        let com = Com::join()?;
        let enumerator = DeviceEnumerator::new().map_err(|error| classify(&error))?;
        Ok(Self {
            registration: None,
            enumerator,
            _com: com,
        })
    }
}

impl Audio for System {
    type Stream = Running;

    fn endpoints(&mut self, kind: Kind) -> Result<Vec<Endpoint>, Failure> {
        let collection = self
            .enumerator
            .get_device_collection(&direction(kind))
            .map_err(|error| classify(&error))?;
        let count = collection
            .get_nbr_devices()
            .map_err(|error| classify(&error))?;
        let mut found = Vec::new();
        for index in 0..count {
            // An endpoint that cannot be read is one that is going: leave it out.
            let Ok(device) = collection.get_device_at_index(index) else {
                continue;
            };
            let Ok(id) = device.get_id() else {
                continue;
            };
            let name = device.get_friendlyname().unwrap_or_default();
            found.push(Endpoint { id, name });
        }
        Ok(found)
    }

    fn active_ids(&mut self, kind: Kind) -> Result<Vec<String>, Failure> {
        // IDs only: a friendly name is read when a device is listed for a person, not on
        // every look the worker takes.
        let collection = self
            .enumerator
            .get_device_collection(&direction(kind))
            .map_err(|error| classify(&error))?;
        let count = collection
            .get_nbr_devices()
            .map_err(|error| classify(&error))?;
        Ok((0..count)
            .filter_map(|index| collection.get_device_at_index(index).ok()?.get_id().ok())
            .collect())
    }

    fn default_id(&mut self, kind: Kind) -> Result<Option<String>, Failure> {
        // The console role: the one Windows calls the default (`default_changed`).
        let device = match self
            .enumerator
            .get_default_device_for_role(&direction(kind), &Role::Console)
        {
            Ok(device) => device,
            // No such device: there is no default.
            Err(error) if classify(&error) == Failure::Gone => return Ok(None),
            Err(error) => return Err(classify(&error)),
        };
        device.get_id().map(Some).map_err(|error| classify(&error))
    }

    fn start(&mut self, kind: Kind, id: &str) -> Result<Running, Failure> {
        // An ID that is no endpoint, for whatever reason, is a device that is not there.
        let device = self.enumerator.get_device(id).map_err(|_| Failure::Gone)?;
        // An endpoint of the other kind is not this one's: `wasapi` turns a capture client
        // on a render endpoint into loopback, which would capture everything the PC plays.
        if device.get_direction() != direction(kind) {
            return Err(Failure::Gone);
        }
        // Mono first, which the engine converts; stereo if the driver will not.
        let mut refused = Failure::Unsupported;
        for channels in [1_usize, 2] {
            let mut client = device
                .get_iaudioclient()
                .map_err(|error| classify(&error))?;
            let (period, _minimum) = client
                .get_device_period()
                .map_err(|error| classify(&error))?;
            let mode = StreamMode::EventsShared {
                autoconvert: true,
                buffer_duration_hns: period,
            };
            match client.initialize_client(&format(channels), &direction(kind), &mode) {
                Ok(()) => return Running::begin(client, kind, channels),
                Err(error) => match classify(&error) {
                    failure @ (Failure::Unsupported | Failure::Other(_)) => refused = failure,
                    other => return Err(other),
                },
            }
        }
        Err(refused)
    }

    fn subscribe(&mut self, kind: Kind, to: Poster) -> Result<(), Failure> {
        let mut callbacks = DeviceEventCallbacks::new();
        let added = to.clone();
        callbacks.set_device_added_callback(move |id| post(&added, Notice::Added(id)));
        let removed = to.clone();
        callbacks.set_device_removed_callback(move |id| post(&removed, Notice::Removed(id)));
        let state = to.clone();
        callbacks.set_device_state_callback(move |id, _state| post(&state, Notice::State(id)));
        callbacks.set_default_device_callback(move |flow, role, id| {
            if let Some(notice) = default_changed(kind, kind_of(flow), role == Role::Console, id) {
                post(&to, notice);
            }
        });
        let registration = self
            .enumerator
            .register_notification_callback(callbacks)
            .map_err(|error| classify(&error))?;
        self.registration = Some(registration);
        Ok(())
    }
}

/// Which end of the stream the helper holds.
enum Io {
    Capture(AudioCaptureClient),
    Render(AudioRenderClient),
}

/// A shared-mode, event-driven stream, started.
pub(crate) struct Running {
    // Declaration order is drop order: the ends and the event go before the client.
    io: Io,
    event: Handle,
    client: AudioClient,
    channels: usize,
    /// Bytes per frame of what the engine is given: four a channel.
    block: usize,
    bytes: Vec<u8>,
    samples: Vec<f32>,
}

impl Running {
    /// Takes an initialised client the rest of the way: the event, the end, and, for
    /// playback, a first buffer of silence, as Microsoft's rendering guide says; then starts.
    fn begin(client: AudioClient, kind: Kind, channels: usize) -> Result<Self, Failure> {
        let event = client
            .set_get_eventhandle()
            .map_err(|error| classify(&error))?;
        let io = match kind {
            Kind::Capture => Io::Capture(
                client
                    .get_audiocaptureclient()
                    .map_err(|error| classify(&error))?,
            ),
            Kind::Render => {
                let render = client
                    .get_audiorenderclient()
                    .map_err(|error| classify(&error))?;
                let space = client
                    .get_available_space_in_frames()
                    .map_err(|error| classify(&error))? as usize;
                if space > 0 {
                    let silence = vec![0_u8; space * 4 * channels];
                    let flags = BufferFlags {
                        data_discontinuity: false,
                        silent: true,
                        timestamp_error: false,
                    };
                    render
                        .write_to_device(space, &silence, Some(flags))
                        .map_err(|error| classify(&error))?;
                }
                Io::Render(render)
            }
        };
        client.start_stream().map_err(|error| classify(&error))?;
        Ok(Self {
            io,
            event,
            client,
            channels,
            block: 4 * channels,
            bytes: Vec::new(),
            samples: Vec::new(),
        })
    }
}

/// Waits for the stream's event, at most `wait`. A timeout is not a failure: the device may
/// simply have had nothing to say, and what the stream holds is looked at all the same, which
/// is also how a device that went away is found.
fn wait_for(event: &Handle, wait: Duration) -> Result<(), Failure> {
    let milliseconds = u32::try_from(wait.as_millis()).unwrap_or(u32::MAX);
    match event.wait_for_event(milliseconds) {
        Ok(()) | Err(WasapiError::EventTimeout) => Ok(()),
        Err(error) => Err(classify(&error)),
    }
}

impl Stream for Running {
    fn capture(&mut self, wait: Duration, deliver: &mut dyn FnMut(&[f32])) -> Result<(), Failure> {
        let Io::Capture(capture) = &self.io else {
            return Err(Failure::Other("this stream does not capture".to_owned()));
        };
        wait_for(&self.event, wait)?;
        loop {
            let frames = match capture
                .get_next_packet_size()
                .map_err(|error| classify(&error))?
            {
                Some(frames) if frames > 0 => frames as usize,
                _ => return Ok(()),
            };
            self.bytes.resize(frames * self.block, 0);
            let (read, info) = capture
                .read_from_device(&mut self.bytes)
                .map_err(|error| classify(&error))?;
            let read = read as usize;
            self.samples.clear();
            if info.flags.silent {
                // The engine says what it gave is silence, whatever the buffer holds.
                self.samples.resize(read, 0.0);
            } else {
                decode_mono(
                    &self.bytes[..read * self.block],
                    self.channels,
                    &mut self.samples,
                );
            }
            if !self.samples.is_empty() {
                deliver(&self.samples);
            }
        }
    }

    fn render(
        &mut self,
        wait: Duration,
        supply: &mut dyn FnMut(&mut [f32]) -> usize,
    ) -> Result<(), Failure> {
        let Io::Render(render) = &self.io else {
            return Err(Failure::Other("this stream does not play".to_owned()));
        };
        wait_for(&self.event, wait)?;
        let space = self
            .client
            .get_available_space_in_frames()
            .map_err(|error| classify(&error))? as usize;
        if space == 0 {
            return Ok(());
        }
        self.samples.clear();
        self.samples.resize(space, 0.0);
        let given = supply(&mut self.samples).min(space);
        if given == 0 {
            return Ok(());
        }
        encode_mono(&self.samples[..given], self.channels, &mut self.bytes);
        render
            .write_to_device(given, &self.bytes, None)
            .map_err(|error| classify(&error))
    }
}

impl Drop for Running {
    fn drop(&mut self) {
        // Best effort: the client is released with the rest.
        let _ = self.client.stop_stream();
    }
}

fn wasapi_error(doing: &'static str, failure: Failure) -> Error {
    let detail = match failure {
        Failure::Denied => "Windows refused access".to_owned(),
        Failure::Gone => "the device is not there".to_owned(),
        Failure::Unsupported => "the sound system would not take 48 kHz audio".to_owned(),
        Failure::Other(line) => line,
    };
    Error::AudioSystem { doing, detail }
}

/// The connection to the Windows audio system. There is nothing to hold open: each microphone
/// and each pair of speakers has a worker thread and an apartment of its own. Cloning is free.
#[derive(Debug, Clone)]
pub struct Wasapi {
    _private: (),
}

impl Wasapi {
    /// Checks that the audio system can be reached, and no more: nothing is opened.
    ///
    /// # Errors
    ///
    /// [`Error::AudioSystem`] if there is no audio system to reach (the service is stopped, or COM
    /// cannot be set up).
    pub fn connect() -> Result<Self, Error> {
        const DOING: &str = "reach the Windows audio system";
        // On a thread of its own: a thread that is already in another kind of apartment
        // cannot join this one, and the caller's is not ours to choose.
        let probed = std::thread::Builder::new()
            .name("conch-voice-wasapi-probe".to_owned())
            .spawn(|| System::new().map(drop))
            .map_err(|_| wasapi_error(DOING, Failure::Other("no thread could be started".into())))?
            .join();
        match probed {
            Ok(Ok(())) => Ok(Self { _private: () }),
            Ok(Err(failure)) => Err(wasapi_error(DOING, failure)),
            Err(_) => Err(wasapi_error(
                DOING,
                Failure::Other("the audio thread stopped".into()),
            )),
        }
    }

    /// The endpoints and the defaults, as the neutral table. `devices` does not use it on
    /// Windows yet: its document has a field that would say `pipewire` (#891).
    ///
    /// # Errors
    ///
    /// [`Error::AudioSystem`] if the system cannot be asked.
    pub fn list_devices(&self, _wait: Duration) -> Result<DeviceTable, Error> {
        const DOING: &str = "list the audio devices";
        let listed = std::thread::Builder::new()
            .name("conch-voice-wasapi-list".to_owned())
            .spawn(|| System::new().and_then(|mut system| list(&mut system)))
            .map_err(|_| wasapi_error(DOING, Failure::Other("no thread could be started".into())))?
            .join();
        match listed {
            Ok(Ok(table)) => Ok(table),
            Ok(Err(failure)) => Err(wasapi_error(DOING, failure)),
            Err(_) => Err(wasapi_error(
                DOING,
                Failure::Other("the audio thread stopped".into()),
            )),
        }
    }

    /// Opens the microphone: `configured` is an endpoint ID, or `None` (or `default`) for
    /// the default, which it follows as it changes. A named endpoint that is not there is
    /// waited for, and the default is not used in its place. The stream is open until the
    /// result is dropped.
    ///
    /// # Errors
    ///
    /// [`Error::AudioSystem`] if Windows refuses the microphone (the words name the privacy
    /// setting), or the engine would not take the format.
    pub fn microphone(&self, configured: Option<&str>) -> Result<WasapiMic, Error> {
        WasapiMic::start(System::new, Target::from_config(configured))
    }

    /// Opens the speakers: `configured` is an endpoint ID, or `None` (or `default`) for the
    /// default, which they follow as it changes. A named endpoint that is not there is
    /// waited for, and never replaced. The stream is open until the result is dropped.
    ///
    /// # Errors
    ///
    /// [`Error::AudioSystem`] if the engine would not take the format, or the system failed.
    pub fn speakers(&self, configured: Option<&str>) -> Result<WasapiSpeakers, Error> {
        WasapiSpeakers::start(System::new, Target::from_config(configured))
    }
}

#[cfg(test)]
mod tests {
    use std::time::{Duration, Instant};

    use conch_voice_audio::{FRAME_LEN, Frame, MicRead, MicSource, SILENCE, SpeakerSink};

    use super::*;

    /// Plays a tone through the default speakers for a second and records from the default
    /// microphone for a second, on a real machine. It opens real devices, so the microphone
    /// indicator lights, and what is heard depends on the room.
    ///
    /// Needs: Windows with a default microphone and default speakers, and microphone access
    /// allowed for desktop apps. Not run by CI (the runner has no audio endpoint). It is for
    /// E3 and E4 of `docs/design/windows-desktop.md`, on a real machine (#903): format
    /// negotiation, the period, and glitch-free 10 ms frames.
    #[test]
    #[ignore = "needs a real default microphone and speakers on Windows; run by hand (#903)"]
    fn the_default_speakers_play_and_the_default_microphone_records() {
        let wasapi = Wasapi::connect().expect("the audio system");
        let mut speakers = wasapi.speakers(None).expect("the speakers");
        let mut mic = wasapi.microphone(None).expect("the microphone");

        let began = Instant::now();
        let mut phase = 0.0_f32;
        let mut heard = 0_usize;
        let mut frame: Frame = SILENCE;
        while began.elapsed() < Duration::from_secs(1) {
            for _ in 0..speakers.frames_wanted() {
                for sample in &mut frame {
                    *sample = 0.1 * (phase * std::f32::consts::TAU).sin();
                    phase = (phase + 440.0 / 48_000.0).fract();
                }
                speakers.play(&frame);
            }
            while mic.read(&mut frame) == MicRead::Ready {
                heard += 1;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        // A second of 10 ms frames is a hundred; allow for the start.
        assert!(
            heard >= 50,
            "heard only {heard} frames of {FRAME_LEN} samples"
        );
    }
}
