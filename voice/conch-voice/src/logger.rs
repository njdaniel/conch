//! The process's one `log::Log`, installed before anything else runs.
//!
//! The SDK and everything it links write to the `log` facade, and what they write was
//! measured (`docs/design/conch-voice.md` §13 row 10): at trace the WebSocket library logs
//! the whole upgrade request, bearer token included, and at info and debug the SDK logs the
//! room name. So the rule here is by target, and its default is to deny:
//!
//! - A record from this workspace's own crates is written at the level the user asked for.
//! - A record from anywhere else (`livekit`, `libwebrtc`, `webrtc`, `tungstenite`,
//!   `tokio_tungstenite`, `reqwest`, `hyper`, `rustls`, `h2`, and any target this file does
//!   not know) is written only at `Warn` and `Error`, whatever level the user asked for,
//!   with its target in front.
//!
//! Every record that is written, of either kind, first has the join tokens and room names
//! of recent connections, the login token and anything shaped like a signed token replaced
//! by the [`Scrubber`], its control characters replaced by spaces, so that it is one line,
//! and is then cut to [`RECORD_CHARS`]: what answers where a session says LiveKit is decides
//! what the SDK logs about it, and how much.
//!
//! The installed logger does not write to standard error itself. A record is handed to a
//! [`LineQueue`], so a standard error nobody reads cannot stop whoever logged: the record
//! is dropped and counted instead.

use std::fmt;
use std::io::Write;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use log::{Level, LevelFilter, Log, Metadata, Record};

use crate::lines::{LineQueue, QUEUE_LINES};
use crate::secrets::Scrubber;

/// The most characters of one record that are written. An SDK record about a refused join
/// quotes the answer it got, which may be megabytes.
pub const RECORD_CHARS: usize = 2_000;

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

/// Where a logger's records go.
enum Sink {
    /// Written where they are logged, under a lock: for tests, whose writer never stalls.
    Direct(Mutex<Box<dyn Write + Send>>),
    /// Handed to a writer thread without waiting.
    Queued(LineQueue),
}

/// The logger. See the module documentation for what it writes.
pub struct Logger {
    /// The level the user asked for, as `LevelFilter as usize`.
    level: AtomicUsize,
    out: Sink,
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
    /// A logger that writes each record to `out` where it is logged, at `level`, scrubbing
    /// with `scrubber`. For tests: whoever logs waits for `out`.
    pub fn new(level: LevelFilter, out: Box<dyn Write + Send>, scrubber: Arc<Scrubber>) -> Self {
        Self {
            level: AtomicUsize::new(level as usize),
            out: Sink::Direct(Mutex::new(out)),
            scrubber,
        }
    }

    /// A logger whose records are written to `out` by a thread of their own. Whoever logs
    /// never waits for `out`: when [`QUEUE_LINES`] records are waiting, further ones are
    /// dropped, and one warning says how many when there is room again.
    pub fn queued(level: LevelFilter, out: Box<dyn Write + Send>, scrubber: Arc<Scrubber>) -> Self {
        let dropped = |records| {
            format!(
                "conch-voice: warn: {records} log records were dropped: standard error was not being read"
            )
        };
        Self {
            level: AtomicUsize::new(level as usize),
            out: Sink::Queued(LineQueue::spawn(out, QUEUE_LINES, Box::new(dropped))),
            scrubber,
        }
    }

    /// Makes a logger that writes to standard error, through a queue, the process's logger.
    /// Call it first in `main`: until it is installed a record goes nowhere, and once it is
    /// nothing can install another.
    ///
    /// Returns `None` if some other logger was installed already; then this one is not in
    /// use and nothing may be assumed about what is written.
    pub fn install(level: LevelFilter, scrubber: Arc<Scrubber>) -> Option<&'static Self> {
        let logger: &'static Self = Box::leak(Box::new(Self::queued(
            level,
            Box::new(std::io::stderr()),
            scrubber,
        )));
        log::set_logger(logger).ok()?;
        log::set_max_level(level);
        Some(logger)
    }

    /// Waits until every record logged so far has been written, or `limit` has passed.
    /// True if they were written. It blocks the calling thread: it is for `main`, before
    /// the process exits, and a flush that times out is not an error.
    pub fn flush_within(&self, limit: Duration) -> bool {
        match &self.out {
            Sink::Direct(out) => {
                let mut out = out.lock().unwrap_or_else(PoisonError::into_inner);
                out.flush().is_ok()
            }
            Sink::Queued(queue) => queue.flush_blocking(limit),
        }
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
        // Whoever wrote it, the record is scrubbed and kept to one line. This workspace's
        // own code holds its secrets where they cannot be printed, but its records quote
        // what `conchd` and the SDK said, and a mistake here must not be the one way out.
        let text = if is_own(record.target()) {
            record.args().to_string()
        } else {
            // Not this program's text: its target is shown, and scrubbed with it.
            format!("{}: {}", record.target(), record.args())
        };
        // Scrubbed first and cut second: a secret across the place of the cut is replaced
        // whole, not left as a half that nothing would recognise.
        let text = self.scrubber.scrub_line_within(&text, RECORD_CHARS);
        let line = format!("conch-voice: {level}: {text}");
        match &self.out {
            Sink::Direct(out) => {
                let mut out = out.lock().unwrap_or_else(PoisonError::into_inner);
                // A logger has nowhere to report that it could not write.
                let _ = writeln!(out, "{line}");
                let _ = out.flush();
            }
            // Never waited for: this runs in the session loop and in the SDK's threads.
            Sink::Queued(queue) => queue.push(line),
        }
        // On a terminal the record went under the status, which is then drawn anew.
        crate::status::disturbed();
    }

    /// The `log` facade's flush. A queued logger does nothing here, because this must not
    /// wait for standard error either; [`Logger::flush_within`] is the flush with a limit.
    fn flush(&self) {
        if let Sink::Direct(out) = &self.out {
            let mut out = out.lock().unwrap_or_else(PoisonError::into_inner);
            let _ = out.flush();
        }
    }
}

#[cfg(test)]
mod tests {
    use conch_voice_api::Secret;

    use super::*;
    use crate::lines::tests::Stallable;

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
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
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
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        write(&logger, Level::Error, FAKE_ROOM, "gone");
        assert_eq!(written.text(), "conch-voice: error: [redacted]: gone\n");
    }

    #[test]
    fn this_workspaces_own_records_are_written_at_the_level_asked_for_without_a_target() {
        let (logger, written, _scrubber) = logger(LevelFilter::Trace);
        for (level, target) in [
            (Level::Trace, "conch_voice"),
            (Level::Debug, "conch_voice::session"),
            (Level::Info, "conch_voice_api::client"),
            (Level::Warn, "conch_voice_audio"),
            (Level::Error, "conch_voice_control::ptt"),
        ] {
            write(&logger, level, target, &format!("{target} says so"));
        }
        assert_eq!(
            written.text(),
            "conch-voice: trace: conch_voice says so\n\
             conch-voice: debug: conch_voice::session says so\n\
             conch-voice: info: conch_voice_api::client says so\n\
             conch-voice: warn: conch_voice_audio says so\n\
             conch-voice: error: conch_voice_control::ptt says so\n"
        );
    }

    /// This workspace's code cannot print a secret it holds, but its records quote what
    /// `conchd` and the SDK said. They are scrubbed and kept to one line like any other.
    #[test]
    fn this_workspaces_own_records_are_scrubbed_and_kept_to_one_line_too() {
        let (logger, written, scrubber) = logger(LevelFilter::Trace);
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        write(
            &logger,
            Level::Debug,
            "conch_voice::presence",
            &format!("the socket failed: {FAKE_ROOM}\n\x1b[2J{FAKE_JOIN} {FAKE_JWT}"),
        );
        assert_eq!(
            written.text(),
            "conch-voice: debug: the socket failed: [redacted]  [2J[redacted] [redacted]\n"
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
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let shown = format!("{logger:?}");
        assert!(
            !shown.contains(FAKE_JOIN) && !shown.contains(FAKE_ROOM),
            "{shown}"
        );
    }

    /// What the SDK logs about a refused join quotes the answer, and whatever answered
    /// chose its length.
    #[test]
    fn a_record_is_cut_to_a_length_after_it_is_scrubbed() {
        let (logger, written, scrubber) = logger(LevelFilter::Warn);
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let megabytes = "A".repeat(2 * 1024 * 1024);
        write(&logger, Level::Warn, "livekit_signaling", &megabytes);
        write(&logger, Level::Error, "conch_voice", &megabytes);
        let text = written.text();
        for line in text.lines() {
            let length = line.chars().count();
            assert!(
                length > RECORD_CHARS && length < RECORD_CHARS + 40,
                "a record of {length} characters"
            );
            assert!(line.ends_with("A [cut]"), "it says that it was cut");
        }
        assert_eq!(text.lines().count(), 2);

        // A secret that lies across the place of the cut is not written in part.
        let lead = "x".repeat(RECORD_CHARS - "livekit: ".len() - 15);
        let record = format!("{lead}{FAKE_JOIN} and the rest of it");
        write(&logger, Level::Warn, "livekit", &record);
        let text = written.text();
        let last = text.lines().last().unwrap();
        let end = &last[last.len() - 40..];
        assert!(!last.contains("FAKE"), "{end}");
        assert!(end.ends_with("x[redacted] and  [cut]"), "{end}");

        // A record that fits is not touched.
        write(&logger, Level::Warn, "livekit", "short");
        assert!(
            written
                .text()
                .ends_with("conch-voice: warn: livekit: short\n")
        );
    }

    #[test]
    fn a_queued_logger_writes_its_records_in_order() {
        let writer = Stallable::default();
        let logger = Logger::queued(LevelFilter::Warn, Box::new(writer.clone()), Scrubber::new());
        for n in 0..50 {
            write(&logger, Level::Warn, "livekit", &format!("record {n}"));
        }
        assert!(logger.flush_within(Duration::from_secs(10)));
        let expected: String = (0..50)
            .map(|n| format!("conch-voice: warn: livekit: record {n}\n"))
            .collect();
        assert_eq!(writer.text(), expected);
    }

    /// The logger is called from the session loop and from the SDK's threads. With a
    /// standard error nobody reads, a record is dropped: nobody waits for it.
    #[test]
    fn with_a_stalled_standard_error_a_record_is_dropped_and_not_waited_for() {
        let writer = Stallable::stalled();
        let logger = Logger::queued(LevelFilter::Warn, Box::new(writer.clone()), Scrubber::new());
        write(&logger, Level::Warn, "livekit", "in the writer's hands");
        writer.until_waiting();

        let began = std::time::Instant::now();
        for n in 0..QUEUE_LINES + 300 {
            write(&logger, Level::Error, "livekit", &format!("record {n}"));
        }
        let took = began.elapsed();
        assert!(
            took < Duration::from_secs(2),
            "logging waited for standard error: {took:?}"
        );
        // Neither does the flush wait beyond its limit, nor the facade's flush at all.
        let began = std::time::Instant::now();
        assert!(!logger.flush_within(Duration::from_millis(100)));
        Log::flush(&logger);
        assert!(began.elapsed() < Duration::from_secs(2));
        assert_eq!(writer.text(), "");

        // When standard error is read again, one warning says how many were dropped.
        writer.stall(false);
        writer.until_lines(1 + QUEUE_LINES);
        write(&logger, Level::Warn, "livekit", "after");
        assert!(logger.flush_within(Duration::from_secs(10)));
        let text = writer.text();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines.len(), 1 + QUEUE_LINES + 2, "{}", lines.len());
        assert_eq!(
            lines[0],
            "conch-voice: warn: livekit: in the writer's hands"
        );
        assert_eq!(
            lines[QUEUE_LINES],
            format!("conch-voice: error: livekit: record {}", QUEUE_LINES - 1)
        );
        assert_eq!(
            lines[QUEUE_LINES + 1],
            "conch-voice: warn: 300 log records were dropped: standard error was not being read"
        );
        assert_eq!(lines[QUEUE_LINES + 2], "conch-voice: warn: livekit: after");
    }
}
