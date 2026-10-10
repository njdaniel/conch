//! `conch-voice`: the Conch voice client (ADR-006). This library is the whole of the
//! binary but its `main`, so that all of it can be tested.
//!
//! Input: the command line; the configuration file `$XDG_CONFIG_HOME/conch/voice.toml`;
//! `CONCH_SERVER` and `CONCH_TOKEN`, or the login `conch login` stored; `conchd`'s answers
//! (voice sessions, the presence socket); the audio and events of the channel's LiveKit
//! room; the configured keyboard's event device, for the talk, mute and deafen keys and
//! nothing else ([`keydev`]); and standard input, one command per line, `down`, `up`,
//! `mute`, `deafen`, `quit`, where the end of input counts as `up`, and then as `quit`
//! unless a key device is watched.
//!
//! Output: on standard output, plain lines or, with `--json`, one JSON object per line
//! ([`output`] documents the fields; a reader must skip any line that does not begin with
//! `{`, because libwebrtc prints one of its own on a machine with an NVIDIA GPU). On
//! standard error, log records and the one line that says why the client stopped. To
//! `conchd`, a transmit report for each press and release. To LiveKit, a microphone track
//! that carries audio only while the transmit gate is open. No join token, room name or
//! login token is in anything this program writes.
//!
//! Owns: the connection to the channel's voice room, for as long as `join` runs; the
//! process's logger ([`logger`]); and the tasks that hold the transmit gate ([`transmit`]),
//! the mix ([`receive`]), the report queue ([`reports`]) and the presence socket
//! ([`presence`]). It keeps no file and stores no token.
//!
//! No audio hardware yet: the microphone is a tone or a WAV file and the speakers are a
//! sink that counts. Real audio devices are issue #184.
//!
//! - [`session`]: the loop that decides everything, written against the traits in [`sdk`].
//! - [`livekit`]: those traits over LiveKit's SDK, kept thin.
//! - [`secrets`] and [`logger`]: what keeps tokens and room names out of the output.
//! - [`cli`]: the command line and the layers of configuration.
//! - [`keydev`]: the key device's watcher. [`keys`] and [`devices`]: the two commands that
//!   help set it up. [`status`]: the status redrawn in place on a terminal.
//!
//! Design: `docs/design/conch-voice.md` §3, §5 to §8.

// CLAUDE.md: no unwrap/expect outside tests and main.
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod cli;
pub mod devices;
pub mod error;
pub mod input;
pub mod keydev;
pub mod keys;
pub mod livekit;
pub mod logger;
pub mod output;
pub mod presence;
pub mod receive;
pub mod reports;
pub mod sdk;
pub mod secrets;
pub mod session;
pub mod status;
pub mod transmit;

use std::io::IsTerminal;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use conch_voice_audio::{ToneSource, WavSource};
use tokio::sync::mpsc;

use crate::cli::{Environment, JoinArgs, MicChoice, TONE_AMPLITUDE};
pub use crate::error::Error;
use crate::input::AtEnd;
use crate::keydev::{DeviceRule, KeyTimings};
use crate::livekit::LiveKit;
use crate::output::Output;
use crate::secrets::Scrubber;
use crate::session::{Conchd, OpenMic, REPORT_TIMEOUT, REQUEST_TIMEOUT, Settings, Timings};
use crate::transmit::BoxedMic;

/// What opens the microphone `--mic` chose. Nothing is opened until this is called, which
/// is when a session first allows publishing.
fn mic_opener(choice: MicChoice) -> OpenMic {
    Box::new(move || {
        Ok(match &choice {
            MicChoice::None => None,
            MicChoice::Tone(hz) => {
                Some(Box::new(ToneSource::new(*hz, TONE_AMPLITUDE)?) as BoxedMic)
            }
            MicChoice::Wav(path) => Some(Box::new(WavSource::open(path)?) as BoxedMic),
        })
    })
}

/// `conch-voice join`: resolves the configuration and the login, starts the runtime, and
/// runs the session over LiveKit until it ends. It is for `main` to call once, and to exit
/// the process with what it returns: the runtime and the SDK's threads are left running.
///
/// # Errors
///
/// Whatever ended the client other than `quit`, the end of input or a signal. See
/// [`Error::exit_code`].
pub fn join(
    args: &JoinArgs,
    environment: &Environment,
    scrubber: Arc<Scrubber>,
) -> Result<(), Error> {
    let resolved = cli::resolve(args, environment)?;
    // The SDK never sees the login token, and its output is searched for it all the same.
    scrubber.always(&resolved.token);
    let conchd = Conchd::new(
        &resolved.server,
        &resolved.token,
        REQUEST_TIMEOUT,
        REPORT_TIMEOUT,
    )?;
    // The keyboard to read, if any. The rule for what may be opened as one is written
    // out below and is not a setting: only an event device under /dev/input.
    let key_device = cli::key_device(&resolved.config, args.stdin_keys)?;
    let settings = Settings {
        channel: resolved.channel,
        open_mic: mic_opener(args.mic.clone()),
        sink_tones: args.sink_tones.clone(),
        release_tail_ms: resolved.config.audio.release_tail_ms,
        max_transmit: Duration::from_secs(u64::from(resolved.config.audio.max_transmit_secs)),
        key_device: key_device
            .as_ref()
            .map(|(device, _)| device.display().to_string()),
        timings: Timings::default(),
    };
    let out = Output::stdout(args.json);

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(Error::Runtime)?;
    let result = runtime.block_on(async {
        let (inputs, received) = mpsc::unbounded_channel();
        input::signals(inputs.clone()).map_err(Error::Runtime)?;
        match key_device {
            Some((device, bindings)) => {
                // With a key device, a client started with no standard input runs on.
                input::stdin_lines(inputs.clone(), AtEnd::Release);
                keydev::spawn(
                    device,
                    bindings,
                    DeviceRule::EventDevice,
                    KeyTimings::default(),
                    inputs,
                )
                .map_err(Error::Runtime)?;
            }
            None => input::stdin_lines(inputs, AtEnd::Quit),
        }
        let transport = LiveKit::new(Arc::clone(&scrubber));
        session::join(settings, conchd, transport, scrubber, received, out).await
    });
    // The runtime is left running, not shut down: the caller exits the process with what
    // this returns. Shutting it down would cancel the SDK's own tasks wherever they stand,
    // and the SDK panics when one is cancelled under it. That was seen against a real
    // LiveKit after a `DUPLICATE_IDENTITY` disconnect, whose teardown the SDK was still
    // finishing: the panic would have replaced the reason the client stopped for.
    std::mem::forget(runtime);
    result
}

/// `conch-voice devices`: the keyboards under `/dev/input/by-id`, whether this user can
/// read each, and the udev rule that would grant one. Nothing is read from any of them.
///
/// # Errors
///
/// [`Error::Output`] if standard output cannot be written to.
pub fn devices() -> Result<(), Error> {
    devices::report(
        Path::new(devices::BY_ID),
        DeviceRule::EventDevice,
        devices::current_uid(),
        &mut std::io::stdout().lock(),
    )
    .map_err(Error::Output)
}

/// `conch-voice keys <device>`: says that it will show every key pressed on the device,
/// and then does, on the terminal, until Ctrl-C.
///
/// # Errors
///
/// [`Error::NotATerminal`] if standard output is not a terminal; [`Error::KeyDevice`] if
/// the device cannot be opened, is not an event device, or stops being readable.
pub fn keys(device: &Path) -> Result<(), Error> {
    keys::until_interrupted(
        device,
        DeviceRule::EventDevice,
        std::io::stdout().is_terminal(),
    )
}
