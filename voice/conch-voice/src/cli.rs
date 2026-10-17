//! The command line, and how it is laid over the configuration file and the environment
//! (`docs/design/conch-voice.md` §8).
//!
//! `conch-voice join [channel]` joins a channel's voice, with the microphone and the
//! speakers PipeWire has (or the ones `[audio]` in the configuration names). Its test
//! options replace hardware: a tone or a WAV file as the microphone, a sink that counts
//! instead of playing, and standard input instead of a key device. `conch-voice devices`
//! lists the audio devices and helps set the key device up; `conch-voice keys <device>`
//! helps with the keys. `conch-voice managed` is what `conch` starts to run voice inside
//! its TUI (`managed.rs`): it takes no channel, server or login on its command line, and
//! its own test options are hidden from the help and never read from anywhere but the
//! command line.
//!
//! Which keyboard is read, and for which keys, comes from `[keys]` in the configuration
//! file and from nowhere else. How a device is checked before it is read is not a setting
//! at all (`keydev::DeviceRule`).
//!
//! The order in which a value is taken, first wins: the command line, the environment
//! (`CONCH_SERVER`), the configuration file (`voice.toml` in the configuration directory: `$CONCH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/conch`), the
//! default. For the server, the stored login comes before the default, as in `conch`: when
//! exactly one login is stored and `CONCH_TOKEN` is not set, its server is used. The login
//! token is `CONCH_TOKEN`, or else what `conch login` stored for the server; this program
//! never writes that file.

use std::ffi::OsString;
use std::os::unix::fs::MetadataExt;
use std::path::PathBuf;
use std::str::FromStr;

use clap::{Arg, ArgAction, Command};
use conch_voice_api::{
    Error as ApiError, Secret, ServerAddress, TOKEN_ENV, default_config_dir, resolve_token,
    stored_servers,
};
use conch_voice_audio::{FRAME_MS, SAMPLE_RATE, SignalMeter};
use conch_voice_control::{Config, ConfigOverrides, DEFAULT_SERVER, KeyBindings};
use log::LevelFilter;

use crate::error::Error;

/// The environment variable that names the server, as for `conch`.
pub const SERVER_ENV: &str = "CONCH_SERVER";

/// How to name a server, said when the default was used while logins are stored for
/// others.
pub const CHOOSE_A_SERVER: &str =
    "choose one with --server <url>, `server` in voice.toml, or CONCH_SERVER=<url>";

/// The environment variable that moves the configuration directory, as for `conch`.
pub const CONFIG_DIR_ENV: &str = "CONCH_CONFIG_DIR";

/// The peak level of `--mic tone:<hz>`: a quarter of full scale.
pub const TONE_AMPLITUDE: f32 = 0.25;

/// What stands in for the microphone.
#[derive(Debug, Clone, PartialEq)]
pub enum MicChoice {
    /// No `--mic`: the microphone PipeWire has, or the one `audio.input` names. There is no
    /// spelling for it on the command line: it is what a user gets, and the options below
    /// are for tests.
    PipeWire,
    /// `--mic none`: no microphone.
    None,
    /// `--mic tone:<hz>`: a steady tone of this frequency.
    Tone(f32),
    /// `--mic wav:<path>`: a 48 kHz mono 16-bit WAV file, looped.
    Wav(PathBuf),
}

impl FromStr for MicChoice {
    type Err = String;

    fn from_str(text: &str) -> Result<Self, String> {
        if text == "none" {
            return Ok(MicChoice::None);
        }
        if let Some(hz) = text.strip_prefix("tone:") {
            let nyquist = SAMPLE_RATE as f32 / 2.0;
            return match hz.parse::<f32>() {
                Ok(hz) if hz > 0.0 && hz < nyquist => Ok(MicChoice::Tone(hz)),
                _ => Err(format!(
                    "a tone's frequency must be above 0 and below {nyquist} Hz"
                )),
            };
        }
        if let Some(path) = text.strip_prefix("wav:")
            && !path.is_empty()
        {
            return Ok(MicChoice::Wav(PathBuf::from(path)));
        }
        Err("expected tone:<hz>, wav:<path> or none".to_owned())
    }
}

/// What stands in for the speakers.
#[derive(Debug, Clone, PartialEq)]
pub enum SinkChoice {
    /// No `--sink`: the output PipeWire has, or the one `audio.output` names.
    PipeWire,
    /// `--sink count[:<hz>,<hz>,...]`: a sink that plays nothing and counts, looking for
    /// these tones.
    Count(Vec<f32>),
}

/// `--sink count[:<hz>,<hz>,...]`: the tones the counting sink looks for.
fn sink_tones(text: &str) -> Result<Vec<f32>, String> {
    let tones = match text {
        "count" => Vec::new(),
        _ => text
            .strip_prefix("count:")
            .ok_or("expected count or count:<hz>,<hz>,...")?
            .split(',')
            .map(|hz| {
                hz.trim()
                    .parse::<f32>()
                    .map_err(|_| format!("`{hz}` is not a frequency in Hz"))
            })
            .collect::<Result<_, _>>()?,
    };
    // The sink's own rule: 100 Hz to 23900 Hz, and at least 100 Hz apart.
    SignalMeter::new(&tones).map_err(|error| error.to_string())?;
    Ok(tones)
}

fn log_level(text: &str) -> Result<LevelFilter, String> {
    text.parse()
        .map_err(|_| "expected off, error, warn, info, debug or trace".to_owned())
}

/// What `join` was given on the command line.
#[derive(Debug, Clone, PartialEq)]
pub struct JoinArgs {
    /// The channel, if one was named.
    pub channel: Option<String>,
    /// `--server`.
    pub server: Option<String>,
    /// `--json`.
    pub json: bool,
    /// `--mic`.
    pub mic: MicChoice,
    /// `--sink`.
    pub sink: SinkChoice,
    /// `--log-level`.
    pub log_level: LevelFilter,
    /// `--stdin-keys`: take commands from standard input only, and open no key device.
    pub stdin_keys: bool,
}

/// What `managed` was given on the command line. Every field but the log level is for
/// tests: `conch` passes none of them.
#[derive(Debug, Clone, PartialEq)]
pub struct ManagedArgs {
    /// `--log-level`.
    pub log_level: LevelFilter,
    /// `--mic`, test only.
    pub mic: MicChoice,
    /// `--sink`, test only.
    pub sink: SinkChoice,
    /// `--test-key-device`, test only: a named pipe of `input_event` records read instead
    /// of the key device `voice.toml` names, with the keys it binds.
    pub test_key_device: Option<PathBuf>,
}

/// What the command line asked for.
#[derive(Debug, Clone, PartialEq)]
pub enum Invocation {
    /// `conch-voice join`.
    Join(JoinArgs),
    /// `conch-voice managed`.
    Managed(ManagedArgs),
    /// `conch-voice devices`.
    Devices,
    /// `conch-voice keys <device>`.
    Keys {
        /// The keyboard's event device.
        device: PathBuf,
    },
}

/// One line: this binary's version and the audio frame it is built for.
#[must_use]
pub fn version_line() -> String {
    format!("conch-voice {}", version())
}

/// This binary's version and the audio frame it is built for, as a managed helper's hello
/// gives it: printable ASCII, well under the protocol's 64 bytes.
#[must_use]
pub fn version() -> String {
    format!(
        "{} ({SAMPLE_RATE} Hz, {FRAME_MS} ms frames)",
        conch_voice_api::VERSION
    )
}

/// The command line's definition.
#[must_use]
pub fn command() -> Command {
    // clap keeps the version for as long as the command lives, which is the process.
    let version: &'static str = Box::leak(version().into_boxed_str());
    let join = Command::new("join")
        .about("Join a channel's voice and run until quit, the end of input, or Ctrl-C")
        .arg(Arg::new("channel").help(
            "The channel to join [default: `channel` in the configuration file]",
        ))
        .arg(
            Arg::new("server")
                .long("server")
                .value_name("URL")
                .help("The address of conchd [default: $CONCH_SERVER, then the configuration file, then the server of the stored login when exactly one is stored and CONCH_TOKEN is not set, then http://127.0.0.1:8080]"),
        )
        .arg(
            Arg::new("json")
                .long("json")
                .action(ArgAction::SetTrue)
                .help("Write one JSON object per line; skip any line that does not begin with `{`"),
        )
        .arg(
            Arg::new("mic")
                .long("mic")
                .value_name("SOURCE")
                .value_parser(MicChoice::from_str)
                .help("Test microphone instead of PipeWire's: tone:<hz>, wav:<path> (48 kHz mono 16-bit) or none"),
        )
        .arg(
            Arg::new("sink")
                .long("sink")
                .value_name("SINK")
                .value_parser(sink_tones)
                .help("Test speakers instead of PipeWire's: count, or count:<hz>,<hz>,... to name the tones to look for"),
        )
        .arg(
            Arg::new("stdin-keys")
                .long("stdin-keys")
                .action(ArgAction::SetTrue)
                .help("Take down, up, mute, deafen and quit from standard input only, one per line: do not open the key device the configuration file names"),
        )
        .arg(
            Arg::new("log-level")
                .long("log-level")
                .value_name("LEVEL")
                .value_parser(log_level)
                .default_value("warn")
                .help("off, error, warn, info, debug or trace; the SDK's own records are written at warn and error only, whatever is asked"),
        );
    let managed = Command::new("managed")
        .about("Run voice for conch's TUI, which starts this itself and drives it over private pipes")
        .long_about(
            "Runs one voice session for the conch TUI, which starts this as its own child \
             process and drives it over its standard input and file descriptor 3 \
             (docs/design/tui-voice.md). It is not for running by hand: without file \
             descriptor 3 it exits at once. The channel, the server and the login come from \
             conch; the key device and the audio devices from voice.toml, as for join.",
        )
        .arg(
            Arg::new("log-level")
                .long("log-level")
                .value_name("LEVEL")
                .value_parser(log_level)
                .default_value("warn")
                .help("off, error, warn, info, debug or trace, written to standard error"),
        )
        .arg(
            Arg::new("mic")
                .long("mic")
                .value_name("SOURCE")
                .value_parser(MicChoice::from_str)
                .hide(true)
                .help("Test only: tone:<hz>, wav:<path> or none instead of PipeWire's microphone"),
        )
        .arg(
            Arg::new("sink")
                .long("sink")
                .value_name("SINK")
                .value_parser(sink_tones)
                .hide(true)
                .help("Test only: count or count:<hz>,... instead of PipeWire's speakers"),
        )
        .arg(
            Arg::new("test-key-device")
                .long("test-key-device")
                .value_name("PIPE")
                .value_parser(clap::value_parser!(PathBuf))
                .hide(true)
                .help("Test only: read key records from this named pipe instead of the key device voice.toml names"),
        );
    let devices = Command::new("devices")
        .about("List microphones and speakers by the names the configuration takes, and keyboards")
        .long_about(
            "Lists the microphones and the speakers PipeWire has, by the names `input` and \
             `output` under [audio] in the configuration file take, and marks the defaults. \
             Then lists the keyboards under /dev/input/by-id, says for each whether you can \
             read it, and prints a udev rule that would grant you read access to that one \
             device. It opens each keyboard to see whether it can, and reads nothing from any \
             of them; it opens no audio device.",
        );
    let keys = Command::new("keys")
        .about("Show the code of EVERY key pressed on a keyboard until Ctrl-C, to find the key to talk with")
        .long_about(
            "Shows the code of EVERY key pressed on the device, in every application, until \
             Ctrl-C. It is for finding the code of the key you want to talk with. It says so \
             before it opens the device, and it writes only to a terminal: if standard output \
             is redirected it refuses, so that keystrokes are not put in a file by accident. \
             It keeps nothing itself; a terminal that is being recorded keeps what it is shown.",
        )
        .arg(
            Arg::new("device")
                .required(true)
                .value_name("DEVICE")
                .value_parser(clap::value_parser!(PathBuf))
                .help("The keyboard's event device, by its path under /dev/input/by-id"),
        );
    Command::new("conch-voice")
        .about("The Conch voice client: push-to-talk voice for a channel")
        .after_help(
            "Environment:\n  \
             CONCH_CONFIG_DIR  The directory that holds voice.toml and the credentials.json \
             `conch login` wrote, used exactly as given (no `conch` is added); it must be an \
             absolute path. Default: $XDG_CONFIG_HOME/conch, else $HOME/.config/conch. \
             Use it to run two logins on one machine; `conch` reads the same variable.\n  \
             CONCH_SERVER      The address of conchd. Without it, --server or `server` in \
             voice.toml, the server of the stored login when exactly one is stored and \
             CONCH_TOKEN is not set, else http://127.0.0.1:8080.\n  \
             CONCH_TOKEN       A login token, used instead of the stored one.",
        )
        .version(version)
        .subcommand_required(true)
        .arg_required_else_help(true)
        .subcommand(join)
        .subcommand(managed)
        .subcommand(devices)
        .subcommand(keys)
}

/// Parses a command line. `--help` and `--version` come back as errors of clap's, which
/// know how to print themselves and which exit code they mean.
///
/// # Errors
///
/// clap's error, to be shown with its own `exit`.
pub fn parse<I, S>(args: I) -> Result<Invocation, clap::Error>
where
    I: IntoIterator<Item = S>,
    S: Into<OsString> + Clone,
{
    let matches = command().try_get_matches_from(args)?;
    let join = match matches.subcommand() {
        Some(("join", join)) => join,
        Some(("managed", managed)) => {
            return Ok(Invocation::Managed(ManagedArgs {
                log_level: managed
                    .get_one::<LevelFilter>("log-level")
                    .copied()
                    .unwrap_or(LevelFilter::Warn),
                mic: managed
                    .get_one::<MicChoice>("mic")
                    .cloned()
                    .unwrap_or(MicChoice::PipeWire),
                sink: managed
                    .get_one::<Vec<f32>>("sink")
                    .cloned()
                    .map_or(SinkChoice::PipeWire, SinkChoice::Count),
                test_key_device: managed.get_one::<PathBuf>("test-key-device").cloned(),
            }));
        }
        Some(("devices", _)) => return Ok(Invocation::Devices),
        Some(("keys", keys)) => {
            if let Some(device) = keys.get_one::<PathBuf>("device") {
                return Ok(Invocation::Keys {
                    device: device.clone(),
                });
            }
            return Err(command().error(
                clap::error::ErrorKind::MissingRequiredArgument,
                "keys needs the device to read",
            ));
        }
        _ => {
            return Err(command().error(
                clap::error::ErrorKind::MissingSubcommand,
                "a command is required: join, managed, devices or keys",
            ));
        }
    };
    Ok(Invocation::Join(JoinArgs {
        channel: join.get_one::<String>("channel").cloned(),
        server: join.get_one::<String>("server").cloned(),
        json: join.get_flag("json"),
        mic: join
            .get_one::<MicChoice>("mic")
            .cloned()
            .unwrap_or(MicChoice::PipeWire),
        sink: join
            .get_one::<Vec<f32>>("sink")
            .cloned()
            .map_or(SinkChoice::PipeWire, SinkChoice::Count),
        log_level: join
            .get_one::<LevelFilter>("log-level")
            .copied()
            .unwrap_or(LevelFilter::Warn),
        stdin_keys: join.get_flag("stdin-keys"),
    }))
}

/// The parts of the process's environment this program reads. Collected once, in `main`,
/// so that nothing else reads the environment and a test can supply its own.
#[derive(Debug, Clone, Default)]
pub struct Environment {
    /// `CONCH_CONFIG_DIR`.
    pub conch_config_dir: Option<OsString>,
    /// `XDG_CONFIG_HOME`.
    pub xdg_config_home: Option<OsString>,
    /// `HOME`.
    pub home: Option<OsString>,
    /// `CONCH_SERVER`.
    pub server: Option<String>,
    /// `CONCH_TOKEN`. Held as a secret: its `Debug` is a placeholder.
    pub token: Option<Secret>,
}

impl Environment {
    /// Reads the process's environment.
    #[must_use]
    pub fn from_process() -> Self {
        let text = |name: &str| std::env::var(name).ok().filter(|value| !value.is_empty());
        Self {
            conch_config_dir: std::env::var_os(CONFIG_DIR_ENV),
            xdg_config_home: std::env::var_os("XDG_CONFIG_HOME"),
            home: std::env::var_os("HOME"),
            server: text(SERVER_ENV),
            token: text(TOKEN_ENV).map(Secret::new),
        }
    }
}

/// Everything `join` needs that the command line, the environment and the files decide.
#[derive(Debug)]
pub struct Resolved {
    /// The server to talk to.
    pub server: ServerAddress,
    /// The login for it.
    pub token: Secret,
    /// The channel to join.
    pub channel: String,
    /// The configuration, with every layer applied.
    pub config: Config,
    /// When the server is the default, nothing answers there, and logins are stored for
    /// other servers: what to add to the error, naming them ([`stored_elsewhere`]).
    pub unreachable_note: Option<String>,
}

/// Reads `voice.toml`: a missing file is the default configuration. The file's `server`
/// decides where the login token is sent, so a file that is not a regular file or that
/// group or others can write is refused. Looked at before it is opened, because opening a
/// FIFO waits for a writer. A symbolic link is followed: what counts is the file it names.
pub(crate) fn read_config(path: &std::path::Path) -> Result<Config, Error> {
    let meta = match std::fs::metadata(path) {
        Ok(meta) => meta,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(Config::default());
        }
        Err(source) => {
            return Err(Error::ConfigRead {
                path: path.to_owned(),
                source,
            });
        }
    };
    if !meta.is_file() {
        return Err(Error::ConfigNotRegular {
            path: path.to_owned(),
        });
    }
    let mode = meta.mode() & 0o777;
    if mode & 0o022 != 0 {
        return Err(Error::ConfigWritable {
            path: path.to_owned(),
            mode,
        });
    }
    match std::fs::read_to_string(path) {
        Ok(text) => Ok(Config::parse(&text, &format!("{path:?}"))?),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(Config::default()),
        Err(source) => Err(Error::ConfigRead {
            path: path.to_owned(),
            source,
        }),
    }
}

/// Lays the command line over the environment over the configuration file, and finds the
/// login.
///
/// # Errors
///
/// A configuration that cannot be read or is refused, no channel anywhere, a server
/// address that cannot be used, or no login for that server.
pub fn resolve(args: &JoinArgs, environment: &Environment) -> Result<Resolved, Error> {
    let config_dir = default_config_dir(
        environment.conch_config_dir.as_deref(),
        environment.xdg_config_home.as_deref(),
        environment.home.as_deref(),
    )?;
    let mut config = match &config_dir {
        Some(dir) => {
            let path = dir.join("voice.toml");
            read_config(&path)?
        }
        None => Config::default(),
    };
    config.apply(
        ConfigOverrides {
            server: environment.server.clone(),
            ..ConfigOverrides::default()
        },
        SERVER_ENV,
    )?;
    config.apply(
        ConfigOverrides {
            server: args.server.clone(),
            channel: args.channel.clone(),
            ..ConfigOverrides::default()
        },
        "the command line",
    )?;

    let channel = config
        .channel
        .clone()
        .filter(|channel| !channel.is_empty())
        .ok_or(Error::NoChannel)?;
    // With CONCH_TOKEN set the stored login is not the one in use, so it chooses nothing:
    // its server never gets that token. White space alone is not a token, as for
    // resolve_token.
    let token_set = environment
        .token
        .as_ref()
        .is_some_and(|token| !token.expose().trim().is_empty());
    let stored = stored_servers(config_dir.as_deref());
    let only_login = match stored.as_slice() {
        [only] if !token_set => Some(only.key()),
        _ => None,
    };
    let server = ServerAddress::parse(config.server_or(only_login))?;
    let elsewhere = stored_elsewhere(&server, environment, &stored);
    let token = resolve_token(
        environment.token.as_ref().map(Secret::expose),
        &server,
        config_dir.as_deref(),
    )
    .map_err(|error| match (error, &elsewhere) {
        (error @ ApiError::NotSignedIn { .. }, Some(note)) => Error::DefaultServer {
            error,
            note: note.clone(),
        },
        (error, _) => Error::Api(error),
    })?;
    let unreachable_note =
        elsewhere.map(|note| format!("nothing answered at {}; {note}", server.key()));
    Ok(Resolved {
        server,
        token,
        channel,
        config,
        unreachable_note,
    })
}

/// When `server` is the default and logins are stored for other servers, the note that
/// names them all and says how to choose one; otherwise `None`. The rule of
/// `explainUnreachable` in `internal/cli/command.go`: not when `CONCH_SERVER` is set, which
/// chose deliberately, and not when the only login is the default's own. A server named
/// on the command line or in `voice.toml` that is the default gets the note too: it is as
/// true then.
#[must_use]
pub fn stored_elsewhere(
    server: &ServerAddress,
    environment: &Environment,
    stored: &[ServerAddress],
) -> Option<String> {
    if environment.server.is_some() {
        return None;
    }
    let default = ServerAddress::parse(DEFAULT_SERVER).ok()?;
    if server.key() != default.key() || stored.iter().all(|login| login.key() == server.key()) {
        return None;
    }
    let names: Vec<&str> = stored.iter().map(ServerAddress::key).collect();
    Some(format!(
        "logins are stored for {}: {CHOOSE_A_SERVER}",
        names.join(", ")
    ))
}

/// The device to watch and the keys to look for on it, if `join` is to watch one at all:
/// not with `--stdin-keys`, not without a device, and not without a key to look for, since
/// a keyboard is not opened for nothing.
///
/// # Errors
///
/// [`Error::Config`] if two of talk, mute and deafen are bound to one key.
pub fn key_device(
    config: &Config,
    stdin_only: bool,
) -> Result<Option<(PathBuf, KeyBindings)>, Error> {
    let bindings = config.key_bindings()?;
    if stdin_only {
        return Ok(None);
    }
    match (&config.keys.device, bindings.is_empty()) {
        (Some(device), false) => Ok(Some((PathBuf::from(device), bindings))),
        (Some(_), true) => {
            log::warn!(
                "keys.device is set but no key is bound to talk, mute or deafen: the device is not opened"
            );
            Ok(None)
        }
        (None, false) => {
            log::warn!(
                "keys are bound but keys.device is not set: commands are taken from standard input only"
            );
            Ok(None)
        }
        (None, true) => Ok(None),
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;

    const FAKE_LOGIN: &str = "conch_FAKE_login_token_do_not_print";

    fn join(args: &[&str]) -> JoinArgs {
        let mut line = vec!["conch-voice", "join"];
        line.extend(args);
        match parse(line).unwrap() {
            Invocation::Join(args) => args,
            other => panic!("not a join: {other:?}"),
        }
    }

    fn environment(dir: &std::path::Path) -> Environment {
        Environment {
            conch_config_dir: Some(dir.as_os_str().to_owned()),
            xdg_config_home: None,
            home: None,
            server: None,
            token: Some(Secret::new(FAKE_LOGIN)),
        }
    }

    /// A private configuration directory chosen by XDG_CONFIG_HOME alone, so the `conch`
    /// directory under it is the one used.
    fn xdg_environment(dir: &std::path::Path) -> Environment {
        Environment {
            conch_config_dir: None,
            xdg_config_home: Some(dir.as_os_str().to_owned()),
            ..environment(dir)
        }
    }

    fn write_config(dir: &std::path::Path, text: &str) {
        let path = dir.join("voice.toml");
        std::fs::write(&path, text).unwrap();
        // Not left to the umask of whoever runs the tests.
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
    }

    #[test]
    fn a_configuration_others_can_write_is_refused_and_a_fifo_is_not_waited_on() {
        for mode in [0o664, 0o646, 0o666, 0o620, 0o602] {
            let dir = tempfile::tempdir().unwrap();
            write_config(dir.path(), "server = \"http://evil.example\"\n");
            let path = dir.path().join("voice.toml");
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            let error = resolve(&join(&["general"]), &environment(dir.path())).unwrap_err();
            assert!(
                matches!(error, Error::ConfigWritable { .. }),
                "{mode:o}: {error}"
            );
            let shown = error.to_string();
            assert!(
                shown.contains("chmod go-w") && !shown.contains('\n'),
                "{shown}"
            );
        }
        // Read-only for others is fine.
        let dir = tempfile::tempdir().unwrap();
        write_config(dir.path(), "channel = \"ops\"\n");
        let path = dir.path().join("voice.toml");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(resolve(&join(&[]), &environment(dir.path())).is_ok());

        // A FIFO: refused at once.
        let dir = tempfile::tempdir().unwrap();
        let made = std::process::Command::new("mkfifo")
            .arg(dir.path().join("voice.toml"))
            .status();
        if !matches!(made, Ok(status) if status.success()) {
            eprintln!("skipped: mkfifo is not available");
            return;
        }
        let path = dir.path().to_owned();
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(resolve(&join(&["general"]), &environment(&path)).map(|_| ()));
        });
        let result = rx
            .recv_timeout(std::time::Duration::from_secs(10))
            .expect("resolve waited on the FIFO");
        assert!(
            matches!(result, Err(Error::ConfigNotRegular { .. })),
            "{result:?}"
        );
    }

    #[test]
    fn a_configuration_error_is_one_line_even_if_the_directory_name_has_a_newline() {
        let parent = tempfile::tempdir().unwrap();
        let dir = parent.path().join("evil\nname");
        std::fs::create_dir(&dir).unwrap();
        write_config(&dir, "chanel = \"typo\"\n");
        let error = resolve(&join(&["general"]), &environment(&dir)).unwrap_err();
        assert!(error.to_string().contains("unknown key"), "{error}");
        assert!(!error.to_string().contains('\n'), "{error}");
        std::fs::set_permissions(
            dir.join("voice.toml"),
            std::fs::Permissions::from_mode(0o666),
        )
        .unwrap();
        let error = resolve(&join(&["general"]), &environment(&dir)).unwrap_err();
        assert!(!error.to_string().contains('\n'), "{error}");
    }

    #[test]
    fn voice_toml_and_the_login_move_with_the_configuration_directory() {
        let xdg = tempfile::tempdir().unwrap();
        let alice = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(xdg.path().join("conch")).unwrap();
        write_config(&xdg.path().join("conch"), "channel = \"xdg\"\n");
        write_config(alice.path(), "channel = \"alice\"\n");

        let args = join(&[]);
        let by_xdg = resolve(&args, &xdg_environment(xdg.path())).unwrap();
        assert_eq!(by_xdg.channel, "xdg");
        // CONCH_CONFIG_DIR wins over XDG_CONFIG_HOME and is used without `conch` added.
        let mut both = environment(alice.path());
        both.xdg_config_home = Some(xdg.path().as_os_str().to_owned());
        assert_eq!(resolve(&args, &both).unwrap().channel, "alice");
    }

    #[test]
    fn a_relative_config_dir_is_refused_naming_the_variable() {
        let mut environment = environment(std::path::Path::new("/made/up"));
        environment.conch_config_dir = Some(OsString::from("relative/FAKE_dir"));
        let error = resolve(&join(&["general"]), &environment).unwrap_err();
        assert_eq!(
            error.to_string(),
            "CONCH_CONFIG_DIR must be an absolute path"
        );
    }

    #[test]
    fn the_help_documents_the_config_dir_variable() {
        let help = command().render_help().to_string();
        assert!(help.contains("CONCH_CONFIG_DIR"), "{help}");
    }

    #[test]
    fn the_version_line_names_the_version_and_the_frame() {
        let line = version_line();
        assert!(line.starts_with("conch-voice "), "{line}");
        assert!(line.contains(conch_voice_api::VERSION), "{line}");
        assert!(line.ends_with("(48000 Hz, 10 ms frames)"), "{line}");
        let shown = parse(["conch-voice", "--version"]).unwrap_err();
        assert_eq!(shown.kind(), clap::error::ErrorKind::DisplayVersion);
        assert_eq!(shown.to_string().trim_end(), line);
    }

    #[test]
    fn join_takes_its_defaults_when_given_nothing() {
        let args = join(&[]);
        assert_eq!(
            args,
            JoinArgs {
                channel: None,
                server: None,
                json: false,
                mic: MicChoice::PipeWire,
                sink: SinkChoice::PipeWire,
                log_level: LevelFilter::Warn,
                stdin_keys: false,
            }
        );
    }

    #[test]
    fn join_takes_the_test_options() {
        let args = join(&[
            "general",
            "--server",
            "http://127.0.0.1:9",
            "--json",
            "--mic",
            "tone:440",
            "--sink",
            "count:440,880",
            "--stdin-keys",
            "--log-level",
            "trace",
        ]);
        assert_eq!(args.channel.as_deref(), Some("general"));
        assert_eq!(args.server.as_deref(), Some("http://127.0.0.1:9"));
        assert!(args.json);
        assert_eq!(args.mic, MicChoice::Tone(440.0));
        assert_eq!(args.sink, SinkChoice::Count(vec![440.0, 880.0]));
        assert_eq!(args.log_level, LevelFilter::Trace);
        assert!(args.stdin_keys);
        assert_eq!(
            join(&["--mic", "wav:/tmp/a file.wav"]).mic,
            MicChoice::Wav(PathBuf::from("/tmp/a file.wav"))
        );
        assert_eq!(join(&["--mic", "none"]).mic, MicChoice::None);
        assert_eq!(
            join(&["--sink", "count"]).sink,
            SinkChoice::Count(Vec::new())
        );
    }

    #[test]
    fn a_test_option_that_cannot_be_used_is_refused_on_the_command_line() {
        for bad in [
            &["--mic", "tone:0"][..],
            &["--mic", "tone:24000"],
            &["--mic", "tone:loud"],
            &["--mic", "wav:"],
            &["--mic", "pipewire"],
            &["--sink", "speakers"],
            &["--sink", "count:440,450"],
            &["--sink", "count:50"],
            &["--sink", "count:x"],
            &["--log-level", "loud"],
            &["one", "two"],
        ] {
            let mut line = vec!["conch-voice", "join"];
            line.extend(bad);
            let error = parse(line).unwrap_err();
            assert_eq!(error.exit_code(), 2, "{bad:?}");
        }
        assert!(parse(["conch-voice"]).is_err());
        assert!(parse(["conch-voice", "listen"]).is_err());
    }

    #[test]
    fn managed_takes_no_channel_server_or_login_and_hides_its_test_options() {
        assert_eq!(
            parse(["conch-voice", "managed"]).unwrap(),
            Invocation::Managed(ManagedArgs {
                log_level: LevelFilter::Warn,
                mic: MicChoice::PipeWire,
                sink: SinkChoice::PipeWire,
                test_key_device: None,
            })
        );
        assert_eq!(
            parse([
                "conch-voice",
                "managed",
                "--log-level",
                "debug",
                "--mic",
                "tone:440",
                "--sink",
                "count:440",
                "--test-key-device",
                "/tmp/FAKE-pipe",
            ])
            .unwrap(),
            Invocation::Managed(ManagedArgs {
                log_level: LevelFilter::Debug,
                mic: MicChoice::Tone(440.0),
                sink: SinkChoice::Count(vec![440.0]),
                test_key_device: Some(PathBuf::from("/tmp/FAKE-pipe")),
            })
        );
        // Nothing that names what to join, or where, or as whom.
        for bad in [
            &["general"][..],
            &["--server", "http://127.0.0.1:9"],
            &["--json"],
            &["--stdin-keys"],
            &["--token", "x"],
        ] {
            let mut line = vec!["conch-voice", "managed"];
            line.extend(bad);
            assert_eq!(parse(line).unwrap_err().exit_code(), 2, "{bad:?}");
        }
        let help = command()
            .find_subcommand_mut("managed")
            .unwrap()
            .render_help()
            .to_string();
        assert!(help.contains("--log-level"), "{help}");
        for hidden in ["--mic", "--sink", "--test-key-device"] {
            assert!(!help.contains(hidden), "{hidden} is shown: {help}");
        }
    }

    #[test]
    fn devices_and_keys_are_commands_and_keys_needs_its_device() {
        assert_eq!(
            parse(["conch-voice", "devices"]).unwrap(),
            Invocation::Devices
        );
        assert_eq!(
            parse([
                "conch-voice",
                "keys",
                "/dev/input/by-id/usb-Example-event-kbd"
            ])
            .unwrap(),
            Invocation::Keys {
                device: PathBuf::from("/dev/input/by-id/usb-Example-event-kbd")
            }
        );
        for bad in [
            &["keys"][..],
            &["keys", "one", "two"],
            &["devices", "extra"],
            // How a device is checked is not an option of any command.
            &["keys", "--any-file", "x"],
            &["join", "--key-device", "x"],
        ] {
            let mut line = vec!["conch-voice"];
            line.extend(bad);
            assert_eq!(parse(line).unwrap_err().exit_code(), 2, "{bad:?}");
        }
    }

    #[test]
    fn the_help_of_keys_says_what_it_shows_and_that_it_wants_a_terminal() {
        let help = command()
            .find_subcommand_mut("keys")
            .unwrap()
            .render_long_help()
            .to_string();
        assert!(help.contains("EVERY key pressed"), "{help}");
        assert!(help.contains("writes only to a terminal"), "{help}");
        assert!(help.contains("it refuses"), "{help}");
    }

    fn config(text: &str) -> Config {
        Config::parse(text, "voice.toml").unwrap()
    }

    #[test]
    fn a_key_device_is_watched_only_with_a_device_a_key_and_no_stdin_keys() {
        let both =
            config("[keys]\ndevice = \"/dev/input/by-id/x-event-kbd\"\ntalk = \"KEY_F13\"\n");
        let (device, keys) = key_device(&both, false).unwrap().unwrap();
        assert_eq!(device, PathBuf::from("/dev/input/by-id/x-event-kbd"));
        assert_eq!(keys.talk().map(|key| key.code()), Some(183));
        assert!(key_device(&both, true).unwrap().is_none(), "--stdin-keys");

        let no_key = config("[keys]\ndevice = \"/dev/input/by-id/x-event-kbd\"\n");
        assert!(key_device(&no_key, false).unwrap().is_none());
        let no_device = config("[keys]\ntalk = \"KEY_F13\"\n");
        assert!(key_device(&no_device, false).unwrap().is_none());
        assert!(key_device(&Config::default(), false).unwrap().is_none());
    }

    #[test]
    fn the_channel_and_server_come_from_the_file_when_nothing_else_names_them() {
        let dir = tempfile::tempdir().unwrap();
        write_config(
            dir.path(),
            "server = \"http://file.example:8080\"\nchannel = \"from-file\"\n",
        );
        let resolved = resolve(&join(&[]), &environment(dir.path())).unwrap();
        assert_eq!(resolved.channel, "from-file");
        assert_eq!(resolved.server.key(), "http://file.example:8080");
        assert_eq!(resolved.token.expose(), FAKE_LOGIN);
    }

    #[test]
    fn the_environment_wins_over_the_file_and_the_command_line_over_both() {
        let dir = tempfile::tempdir().unwrap();
        write_config(
            dir.path(),
            "server = \"http://file.example:8080\"\nchannel = \"from-file\"\n",
        );
        let mut environment = environment(dir.path());
        environment.server = Some("http://env.example:8080".into());
        let resolved = resolve(&join(&[]), &environment).unwrap();
        assert_eq!(resolved.server.key(), "http://env.example:8080");

        let args = join(&["named", "--server", "http://flag.example:8080"]);
        let resolved = resolve(&args, &environment).unwrap();
        assert_eq!(resolved.server.key(), "http://flag.example:8080");
        assert_eq!(resolved.channel, "named");
    }

    #[test]
    fn with_no_file_the_defaults_apply_and_a_channel_must_be_named() {
        let dir = tempfile::tempdir().unwrap();
        let resolved = resolve(&join(&["general"]), &environment(dir.path())).unwrap();
        assert_eq!(resolved.server.key(), "http://127.0.0.1:8080");
        assert_eq!(resolved.config.audio.release_tail_ms, 100);
        assert_eq!(resolved.config.audio.max_transmit_secs, 120);

        let error = resolve(&join(&[]), &environment(dir.path())).unwrap_err();
        assert!(matches!(error, Error::NoChannel), "{error}");
        assert_eq!(error.exit_code(), 2);
    }

    #[test]
    fn a_configuration_with_an_unknown_key_is_an_error_that_names_it() {
        let dir = tempfile::tempdir().unwrap();
        write_config(dir.path(), "chanel = \"typo\"\n");
        let error = resolve(&join(&["general"]), &environment(dir.path())).unwrap_err();
        assert!(
            error.to_string().contains("unknown key `chanel`"),
            "{error}"
        );
    }

    #[test]
    fn the_stored_login_is_used_when_the_environment_has_no_token() {
        let dir = tempfile::tempdir().unwrap();
        let credentials = dir.path().join("credentials.json");
        std::fs::write(
            &credentials,
            format!("{{\"http://127.0.0.1:8080\":{{\"token\":\"{FAKE_LOGIN}\"}}}}"),
        )
        .unwrap();
        std::fs::set_permissions(&credentials, std::fs::Permissions::from_mode(0o600)).unwrap();
        let mut environment = environment(dir.path());
        environment.token = None;
        let resolved = resolve(&join(&["general"]), &environment).unwrap();
        assert_eq!(resolved.token.expose(), FAKE_LOGIN);

        // And with no login anywhere, the error says to sign in and holds no token.
        std::fs::remove_file(&credentials).unwrap();
        let error = resolve(&join(&["general"]), &environment).unwrap_err();
        assert!(error.to_string().contains("conch login"), "{error}");
    }

    /// A credentials file in `dir` holding a login for each key, with the token
    /// [`fake_token`] gives it.
    fn store_logins(dir: &std::path::Path, keys: &[&str]) {
        let logins: serde_json::Map<String, serde_json::Value> = keys
            .iter()
            .map(|key| {
                (
                    (*key).to_owned(),
                    serde_json::json!({"token": fake_token(key)}),
                )
            })
            .collect();
        let path = dir.join("credentials.json");
        std::fs::write(&path, serde_json::Value::Object(logins).to_string()).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
    }

    fn fake_token(key: &str) -> String {
        format!("conch_FAKE_token_for_{key}")
    }

    /// The order the server is chosen in (issue #523), which is `conch`'s (#522) with
    /// `voice.toml` between `CONCH_SERVER` and the stored login: the command line,
    /// `CONCH_SERVER`, the file, the only stored login's server unless `CONCH_TOKEN` is set,
    /// then the default. And when the default is used while logins are stored for other
    /// servers, what is said names them.
    #[test]
    fn the_server_is_chosen_in_conchs_order_and_a_default_with_other_logins_names_them() {
        const DEFAULT: &str = "http://127.0.0.1:8080";
        const A: &str = "http://a.test:1";
        const B: &str = "http://b.test:2";
        const C: &str = "http://c.test:3";
        const ENV_TOKEN: &str = "conch_FAKE_token_from_env";
        struct Case {
            name: &'static str,
            logins: &'static [&'static str],
            flag: Option<&'static str>,
            env: Option<&'static str>,
            file: Option<&'static str>,
            /// The value of CONCH_TOKEN.
            token: Option<&'static str>,
            /// The server used.
            want: &'static str,
            /// Whether there is no login for it, so that `resolve` fails.
            not_signed_in: bool,
            /// The servers the note names; empty for no note.
            named: &'static [&'static str],
        }
        let case = |name| Case {
            name,
            logins: &[],
            flag: None,
            env: None,
            file: None,
            token: None,
            want: DEFAULT,
            not_signed_in: false,
            named: &[],
        };
        let cases = [
            Case {
                not_signed_in: true,
                ..case("nothing anywhere")
            },
            Case {
                token: Some(ENV_TOKEN),
                ..case("nothing but CONCH_TOKEN")
            },
            Case {
                logins: &[A],
                flag: Some(B),
                token: Some(ENV_TOKEN),
                want: B,
                ..case("the command line")
            },
            Case {
                logins: &[A],
                env: Some(B),
                token: Some(ENV_TOKEN),
                want: B,
                ..case("CONCH_SERVER")
            },
            Case {
                logins: &[A],
                flag: Some(C),
                env: Some(B),
                file: Some(B),
                token: Some(ENV_TOKEN),
                want: C,
                ..case("the command line over CONCH_SERVER and the file")
            },
            Case {
                logins: &[A],
                env: Some(C),
                file: Some(B),
                token: Some(ENV_TOKEN),
                want: C,
                ..case("CONCH_SERVER over the file")
            },
            Case {
                logins: &[A],
                file: Some(B),
                token: Some(ENV_TOKEN),
                want: B,
                ..case("the file over the only login")
            },
            Case {
                logins: &[A],
                want: A,
                ..case("the only login")
            },
            Case {
                logins: &[A],
                token: Some(" \t\n"),
                want: A,
                ..case("the only login, with CONCH_TOKEN only white space")
            },
            Case {
                logins: &[A],
                token: Some(ENV_TOKEN),
                named: &[A],
                ..case("the only login, with CONCH_TOKEN: not chosen, and named")
            },
            Case {
                logins: &[B, A],
                not_signed_in: true,
                named: &[A, B],
                ..case("several logins: none chosen, all named")
            },
            Case {
                logins: &[B, A],
                token: Some(ENV_TOKEN),
                named: &[A, B],
                ..case("several logins, with CONCH_TOKEN")
            },
            Case {
                logins: &[A, DEFAULT],
                named: &[DEFAULT, A],
                ..case("the default's own login among others")
            },
            Case {
                logins: &[DEFAULT],
                ..case("only the default's login")
            },
            Case {
                logins: &[A, B],
                flag: Some(B),
                want: B,
                ..case("several logins, the command line")
            },
            Case {
                logins: &[A, B],
                env: Some(A),
                want: A,
                ..case("several logins, CONCH_SERVER")
            },
            Case {
                logins: &[A, B],
                env: Some(DEFAULT),
                token: Some(ENV_TOKEN),
                ..case("several logins, CONCH_SERVER naming the default: chosen deliberately")
            },
            Case {
                logins: &[A, B],
                flag: Some(DEFAULT),
                token: Some(ENV_TOKEN),
                named: &[A, B],
                ..case("several logins, the command line naming the default: as conch does")
            },
            Case {
                logins: &[
                    A,
                    "not a url",
                    "HTTP://B.test:2/",
                    "http://\u{130}.test:4",
                    "ftp://c.test",
                ],
                want: A,
                ..case("keys conch login would not write are no logins")
            },
        ];
        for case in cases {
            let dir = tempfile::tempdir().unwrap();
            store_logins(dir.path(), case.logins);
            if let Some(file) = case.file {
                write_config(dir.path(), &format!("server = {file:?}\n"));
            }
            let mut environment = environment(dir.path());
            environment.server = case.env.map(str::to_owned);
            environment.token = case.token.map(Secret::new);
            let mut line = vec!["general"];
            if let Some(flag) = case.flag {
                line.extend(["--server", flag]);
            }
            let note = (!case.named.is_empty()).then(|| {
                format!(
                    "logins are stored for {}: choose one with --server <url>, `server` in \
                     voice.toml, or CONCH_SERVER=<url>",
                    case.named.join(", ")
                )
            });
            let name = case.name;

            let result = resolve(&join(&line), &environment);
            if case.not_signed_in {
                let error = result.unwrap_err();
                let mut want = format!("not signed in to {}: run 'conch login'", case.want);
                match (&error, &note) {
                    (Error::DefaultServer { .. }, Some(note)) => {
                        want.push_str(&format!(" ({note})"));
                    }
                    (Error::Api(conch_voice_api::Error::NotSignedIn { .. }), None) => {}
                    _ => panic!("{name}: {error:?}"),
                }
                assert_eq!(error.to_string(), want, "{name}");
                assert_eq!(error.exit_code(), 1, "{name}");
                continue;
            }
            let resolved = result.unwrap_or_else(|error| panic!("{name}: {error}"));
            assert_eq!(resolved.server.key(), case.want, "{name}");
            let token = case
                .token
                .map(str::trim)
                .filter(|token| !token.is_empty())
                .map_or_else(|| fake_token(case.want), str::to_owned);
            assert_eq!(resolved.token.expose(), token, "{name}");
            assert_eq!(
                resolved.unreachable_note,
                note.map(|note| format!("nothing answered at {DEFAULT}; {note}")),
                "{name}"
            );
        }
    }

    #[test]
    fn debug_of_what_was_resolved_shows_no_token() {
        let dir = tempfile::tempdir().unwrap();
        let environment = environment(dir.path());
        let resolved = resolve(&join(&["general"]), &environment).unwrap();
        let shown = format!("{resolved:?} {environment:?}");
        assert!(!shown.contains(FAKE_LOGIN), "{shown}");
    }
}
