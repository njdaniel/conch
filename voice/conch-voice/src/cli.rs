//! The command line, and how it is laid over the configuration file and the environment
//! (`docs/design/conch-voice.md` §8).
//!
//! `conch-voice join [channel]` is the one command of this issue; `devices` and `keys`
//! arrive with #184 and #185. Its test options replace hardware: a tone or a WAV file as
//! the microphone, a sink that counts instead of playing, and standard input instead of a
//! key device.
//!
//! The order in which a value is taken, first wins: the command line, the environment
//! (`CONCH_SERVER`), the configuration file (`$XDG_CONFIG_HOME/conch/voice.toml`), the
//! default. The login token is `CONCH_TOKEN`, or else what `conch login` stored for the
//! server; this program never writes that file.

use std::ffi::OsString;
use std::path::PathBuf;
use std::str::FromStr;

use clap::{Arg, ArgAction, Command};
use conch_voice_api::{Secret, ServerAddress, TOKEN_ENV, default_config_dir, resolve_token};
use conch_voice_audio::{FRAME_MS, SAMPLE_RATE, SignalMeter};
use conch_voice_control::{Config, ConfigOverrides};
use log::LevelFilter;

use crate::error::Error;

/// The environment variable that names the server, as for `conch`.
pub const SERVER_ENV: &str = "CONCH_SERVER";

/// The peak level of `--mic tone:<hz>`: a quarter of full scale.
pub const TONE_AMPLITUDE: f32 = 0.25;

/// What stands in for the microphone.
#[derive(Debug, Clone, PartialEq)]
pub enum MicChoice {
    /// `--mic none`, and the default until real devices arrive (#184): no microphone.
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
    pub sink_tones: Vec<f32>,
    /// `--log-level`.
    pub log_level: LevelFilter,
}

/// One line: this binary's version and the audio frame it is built for.
#[must_use]
pub fn version_line() -> String {
    format!("conch-voice {}", version())
}

fn version() -> String {
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
                .help("The address of conchd [default: $CONCH_SERVER, then the configuration file, then http://127.0.0.1:8080]"),
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
                .default_value("none")
                .help("Test microphone: tone:<hz>, wav:<path> (48 kHz mono 16-bit) or none"),
        )
        .arg(
            Arg::new("sink")
                .long("sink")
                .value_name("SINK")
                .value_parser(sink_tones)
                .default_value("count")
                .help("Test speakers: count, or count:<hz>,<hz>,... to name the tones to look for"),
        )
        .arg(
            Arg::new("stdin-keys")
                .long("stdin-keys")
                .action(ArgAction::SetTrue)
                .help("Take down, up, mute, deafen and quit from standard input, one per line (always on until key devices arrive)"),
        )
        .arg(
            Arg::new("log-level")
                .long("log-level")
                .value_name("LEVEL")
                .value_parser(log_level)
                .default_value("warn")
                .help("off, error, warn, info, debug or trace; the SDK's own records are written at warn and error only, whatever is asked"),
        );
    Command::new("conch-voice")
        .about("The Conch voice client: push-to-talk voice for a channel")
        .version(version)
        .subcommand_required(true)
        .arg_required_else_help(true)
        .subcommand(join)
}

/// Parses a command line. `--help` and `--version` come back as errors of clap's, which
/// know how to print themselves and which exit code they mean.
///
/// # Errors
///
/// clap's error, to be shown with its own `exit`.
pub fn parse<I, S>(args: I) -> Result<JoinArgs, clap::Error>
where
    I: IntoIterator<Item = S>,
    S: Into<OsString> + Clone,
{
    let matches = command().try_get_matches_from(args)?;
    let Some(("join", join)) = matches.subcommand() else {
        return Err(command().error(
            clap::error::ErrorKind::MissingSubcommand,
            "a command is required: join",
        ));
    };
    Ok(JoinArgs {
        channel: join.get_one::<String>("channel").cloned(),
        server: join.get_one::<String>("server").cloned(),
        json: join.get_flag("json"),
        mic: join
            .get_one::<MicChoice>("mic")
            .cloned()
            .unwrap_or(MicChoice::None),
        sink_tones: join
            .get_one::<Vec<f32>>("sink")
            .cloned()
            .unwrap_or_default(),
        log_level: join
            .get_one::<LevelFilter>("log-level")
            .copied()
            .unwrap_or(LevelFilter::Warn),
    })
}

/// The parts of the process's environment this program reads. Collected once, in `main`,
/// so that nothing else reads the environment and a test can supply its own.
#[derive(Debug, Clone, Default)]
pub struct Environment {
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
        environment.xdg_config_home.as_deref(),
        environment.home.as_deref(),
    );
    let mut config = match &config_dir {
        Some(dir) => {
            let path = dir.join("conch").join("voice.toml");
            match std::fs::read_to_string(&path) {
                Ok(text) => Config::parse(&text, &path.display().to_string())?,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => Config::default(),
                Err(source) => return Err(Error::ConfigRead { path, source }),
            }
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
    let server = ServerAddress::parse(config.server_or_default())?;
    let token = resolve_token(
        environment.token.as_ref().map(Secret::expose),
        &server,
        config_dir.as_deref(),
    )?;
    Ok(Resolved {
        server,
        token,
        channel,
        config,
    })
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;

    const FAKE_LOGIN: &str = "conch_FAKE_login_token_do_not_print";

    fn join(args: &[&str]) -> JoinArgs {
        let mut line = vec!["conch-voice", "join"];
        line.extend(args);
        parse(line).unwrap()
    }

    fn environment(dir: &std::path::Path) -> Environment {
        Environment {
            xdg_config_home: Some(dir.as_os_str().to_owned()),
            home: None,
            server: None,
            token: Some(Secret::new(FAKE_LOGIN)),
        }
    }

    fn write_config(dir: &std::path::Path, text: &str) {
        std::fs::create_dir_all(dir.join("conch")).unwrap();
        std::fs::write(dir.join("conch").join("voice.toml"), text).unwrap();
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
                mic: MicChoice::None,
                sink_tones: Vec::new(),
                log_level: LevelFilter::Warn,
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
        assert_eq!(args.sink_tones, vec![440.0, 880.0]);
        assert_eq!(args.log_level, LevelFilter::Trace);
        assert_eq!(
            join(&["--mic", "wav:/tmp/a file.wav"]).mic,
            MicChoice::Wav(PathBuf::from("/tmp/a file.wav"))
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
        assert!(parse(["conch-voice", "devices"]).is_err());
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
        std::fs::create_dir_all(dir.path().join("conch")).unwrap();
        let credentials = dir.path().join("conch").join("credentials.json");
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

    #[test]
    fn debug_of_what_was_resolved_shows_no_token() {
        let dir = tempfile::tempdir().unwrap();
        let environment = environment(dir.path());
        let resolved = resolve(&join(&["general"]), &environment).unwrap();
        let shown = format!("{resolved:?} {environment:?}");
        assert!(!shown.contains(FAKE_LOGIN), "{shown}");
    }
}
