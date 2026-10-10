//! The process's one `log::Log`, installed before anything else runs.
//!
//! The SDK and everything it links write to the `log` facade, and what they write was
//! measured (`docs/design/conch-voice.md` §13 row 10): at trace the WebSocket library logs
//! the whole upgrade request, bearer token included, and at info and debug the SDK logs the
//! room name. So the rule here is by target, and its default is to deny:
//!
//! - A record from this workspace's own crates is written at the level the user asked for,
//!   as it is.
//! - A record from anywhere else (`livekit`, `libwebrtc`, `webrtc`, `tungstenite`,
//!   `tokio_tungstenite`, `reqwest`, `hyper`, `rustls`, `h2`, and any target this file does
//!   not know) is written only at `Warn` and `Error`, whatever level the user asked for,
//!   and only after the [`Scrubber`] has replaced the join token and room name in use and
//!   anything shaped like a signed token.

use std::fmt;
use std::io::Write;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, PoisonError};

use log::{Level, LevelFilter, Log, Metadata, Record};

use crate::secrets::Scrubber;

/// The targets whose records are this workspace's own: each crate's module path. A target
/// is one of these, or one of these followed by `::`.
const OWN_TARGETS: [&str; 4] = [
    "conch_voice",
    "conch_voice_api",
    "conch_voice_audio",
    "conch_voice_control",
];

/// The most a record from anywhere else may be: its warnings and errors.
const FOREIGN_MOST: Level = Level::Warn;

/// Whether `target` names code of this workspace. Everything else is treated as the SDK's.
fn is_own(target: &str) -> bool {
    OWN_TARGETS.iter().any(|own| {
        target
            .strip_prefix(own)
            .is_some_and(|rest| rest.is_empty() || rest.starts_with("::"))
    })
}

/// The logger. See the module documentation for what it writes.
pub struct Logger {
    /// The level the user asked for, as `LevelFilter as usize`.
    level: AtomicUsize,
    out: Mutex<Box<dyn Write + Send>>,
    scrubber: Arc<Scrubber>,
}

/// Shows the level and nothing the scrubber knows.
impl fmt::Debug for Logger {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Logger")
            .field("level", &self.level())
            .finish_non_exhaustive()
    }
}

impl Logger {
    /// A logger that writes to `out` at `level`, scrubbing with `scrubber`.
    pub fn new(level: LevelFilter, out: Box<dyn Write + Send>, scrubber: Arc<Scrubber>) -> Self {
        Self {
            level: AtomicUsize::new(level as usize),
            out: Mutex::new(out),
            scrubber,
        }
    }

    /// Makes a logger that writes to standard error the process's logger. Call it first in
    /// `main`: until it is installed a record goes nowhere, and once it is nothing can
    /// install another.
    ///
    /// Returns `None` if some other logger was installed already; then this one is not in
    /// use and nothing may be assumed about what is written.
    pub fn install(level: LevelFilter, scrubber: Arc<Scrubber>) -> Option<&'static Self> {
        let logger: &'static Self = Box::leak(Box::new(Self::new(
            level,
            Box::new(std::io::stderr()),
            scrubber,
        )));
        log::set_logger(logger).ok()?;
        log::set_max_level(level);
        Some(logger)
    }

    /// Changes the level the user asked for, for the installed logger. It never widens what
    /// a foreign target may write.
    pub fn set_level(&self, level: LevelFilter) {
        self.level.store(level as usize, Ordering::Relaxed);
        log::set_max_level(level);
    }

    fn level(&self) -> LevelFilter {
        let stored = self.level.load(Ordering::Relaxed);
        LevelFilter::iter()
            .find(|level| *level as usize == stored)
            .unwrap_or(LevelFilter::Off)
    }

    fn writes(&self, metadata: &Metadata<'_>) -> bool {
        if metadata.level() > self.level() {
            return false;
        }
        is_own(metadata.target()) || metadata.level() <= FOREIGN_MOST
    }
}

impl Log for Logger {
    fn enabled(&self, metadata: &Metadata<'_>) -> bool {
        self.writes(metadata)
    }

    fn log(&self, record: &Record<'_>) {
        if !self.writes(record.metadata()) {
            return;
        }
        let level = record.level().as_str().to_ascii_lowercase();
        let line = if is_own(record.target()) {
            format!("conch-voice: {level}: {}", record.args())
        } else {
            // Not this program's text: scrubbed, target included, and kept to one line.
            let text = format!("{}: {}", record.target(), record.args());
            let text: String = self
                .scrubber
                .scrub(&text)
                .chars()
                .map(|c| if c.is_control() { ' ' } else { c })
                .collect();
            format!("conch-voice: {level}: {text}")
        };
        let mut out = self.out.lock().unwrap_or_else(PoisonError::into_inner);
        // A logger has nowhere to report that it could not write.
        let _ = writeln!(out, "{line}");
        let _ = out.flush();
    }

    fn flush(&self) {
        let mut out = self.out.lock().unwrap_or_else(PoisonError::into_inner);
        let _ = out.flush();
    }
}

#[cfg(test)]
mod tests {
    use conch_voice_api::Secret;

    use super::*;

    const FAKE_JOIN: &str = "FAKE-join-token-do-not-print-0123456789";
    const FAKE_ROOM: &str = "FAKE-room-name-do-not-print";
    /// Shaped like a signed token and registered nowhere: what LiveKit sends the SDK later.
    const FAKE_JWT: &str = "eyJGQUtFIjoiaGVhZGVyIn0.eyJGQUtFIjoiY2xhaW1zIn0.RkFLRS1zaWduYXR1cmU";

    /// Every target the SDK and what it links log under, and two nobody has heard of.
    const FOREIGN_TARGETS: [&str; 16] = [
        "livekit",
        "livekit::room",
        "livekit::rtc_engine::rtc_session",
        "livekit_api::signal_client",
        "libwebrtc",
        "webrtc",
        "webrtc_sys",
        "tungstenite::handshake::client",
        "tokio_tungstenite",
        "tokio-tungstenite",
        "reqwest::connect",
        "hyper::proto::h1",
        "rustls::client",
        "h2::codec",
        "some_crate_added_next_year",
        "conch_voice_evil",
    ];

    /// What a logger wrote, shared with the test.
    #[derive(Clone, Default)]
    struct Written(Arc<Mutex<Vec<u8>>>);

    impl Write for Written {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }

        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    impl Written {
        fn text(&self) -> String {
            String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
        }
    }

    fn logger(level: LevelFilter) -> (Logger, Written, Arc<Scrubber>) {
        let written = Written::default();
        let scrubber = Scrubber::new();
        let logger = Logger::new(level, Box::new(written.clone()), Arc::clone(&scrubber));
        (logger, written, scrubber)
    }

    fn write(logger: &Logger, level: Level, target: &str, text: &str) {
        logger.log(
            &Record::builder()
                .level(level)
                .target(target)
                .args(format_args!("{text}"))
                .build(),
        );
    }

    #[test]
    fn at_trace_no_sdk_target_is_written_below_warn_whatever_it_carries() {
        let (logger, written, _scrubber) = logger(LevelFilter::Trace);
        let secret = format!("Authorization: Bearer {FAKE_JOIN} room {FAKE_ROOM} and {FAKE_JWT}");
        for target in FOREIGN_TARGETS {
            for level in [Level::Trace, Level::Debug, Level::Info] {
                write(&logger, level, target, &secret);
                let metadata = Metadata::builder().level(level).target(target).build();
                assert!(!logger.enabled(&metadata), "{target} at {level}");
            }
        }
        assert_eq!(written.text(), "", "nothing below warn from any SDK target");
    }

    #[test]
    fn a_warning_from_an_sdk_target_is_written_with_the_secrets_replaced() {
        let (logger, written, scrubber) = logger(LevelFilter::Trace);
        let _guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        for target in FOREIGN_TARGETS {
            for level in [Level::Warn, Level::Error] {
                write(
                    &logger,
                    level,
                    target,
                    &format!("failed: token={FAKE_JOIN} room={FAKE_ROOM}\nrefreshed={FAKE_JWT}"),
                );
            }
        }
        let text = written.text();
        assert_eq!(text.lines().count(), FOREIGN_TARGETS.len() * 2, "{text}");
        for secret in [FAKE_JOIN, FAKE_ROOM, FAKE_JWT] {
            assert!(!text.contains(secret), "{text}");
        }
        assert!(
            text.contains(
                "conch-voice: warn: livekit::room: failed: token=[redacted] room=[redacted] refreshed=[redacted]"
            ),
            "{text}"
        );
    }

    #[test]
    fn a_secret_in_an_sdk_records_target_is_replaced_too() {
        let (logger, written, scrubber) = logger(LevelFilter::Warn);
        let _guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        write(&logger, Level::Error, FAKE_ROOM, "gone");
        assert_eq!(written.text(), "conch-voice: error: [redacted]: gone\n");
    }

    #[test]
    fn this_workspaces_own_records_are_written_at_the_level_asked_for_and_not_altered() {
        let (logger, written, scrubber) = logger(LevelFilter::Trace);
        let _guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        for (level, target) in [
            (Level::Trace, "conch_voice"),
            (Level::Debug, "conch_voice::session"),
            (Level::Info, "conch_voice_api::client"),
            (Level::Warn, "conch_voice_audio"),
            (Level::Error, "conch_voice_control::ptt"),
        ] {
            // Not something this crate would write; it shows that nothing is replaced.
            write(&logger, level, target, &format!("{target} says {FAKE_JWT}"));
        }
        assert_eq!(
            written.text(),
            format!(
                "conch-voice: trace: conch_voice says {FAKE_JWT}\n\
                 conch-voice: debug: conch_voice::session says {FAKE_JWT}\n\
                 conch-voice: info: conch_voice_api::client says {FAKE_JWT}\n\
                 conch-voice: warn: conch_voice_audio says {FAKE_JWT}\n\
                 conch-voice: error: conch_voice_control::ptt says {FAKE_JWT}\n"
            )
        );
    }

    #[test]
    fn the_level_asked_for_bounds_both_kinds_of_record() {
        // `Logger::new`, not `install`: `set_level` below also sets the facade's own
        // maximum, which only matters to a logger that is installed.
        let (logger, written, _scrubber) = logger(LevelFilter::Error);
        write(&logger, Level::Warn, "livekit", "a warning");
        write(&logger, Level::Warn, "conch_voice", "a warning of our own");
        write(&logger, Level::Error, "livekit", "an error");
        assert_eq!(written.text(), "conch-voice: error: livekit: an error\n");

        logger
            .level
            .store(LevelFilter::Off as usize, Ordering::Relaxed);
        write(&logger, Level::Error, "livekit", "another");
        assert_eq!(written.text(), "conch-voice: error: livekit: an error\n");

        // Raising the level later never lets a foreign record through below warn.
        logger
            .level
            .store(LevelFilter::Trace as usize, Ordering::Relaxed);
        write(&logger, Level::Info, "livekit", "room name here");
        write(&logger, Level::Info, "conch_voice", "ours");
        assert_eq!(
            written.text(),
            "conch-voice: error: livekit: an error\nconch-voice: info: ours\n"
        );
    }

    #[test]
    fn a_target_that_only_begins_like_one_of_ours_is_foreign() {
        for target in ["conch_voice", "conch_voice::a::b", "conch_voice_api::x"] {
            assert!(is_own(target), "{target}");
        }
        for target in [
            "conch_voice_evil",
            "conch_voicex::a",
            "conch_voice:",
            "conch",
            "",
            "livekit::conch_voice",
        ] {
            assert!(!is_own(target), "{target}");
        }
    }

    #[test]
    fn debug_shows_nothing_the_scrubber_knows() {
        let (logger, _written, scrubber) = logger(LevelFilter::Warn);
        let _guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let shown = format!("{logger:?}");
        assert!(
            !shown.contains(FAKE_JOIN) && !shown.contains(FAKE_ROOM),
            "{shown}"
        );
    }
}
