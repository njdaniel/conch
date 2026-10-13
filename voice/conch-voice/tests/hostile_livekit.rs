//! The real SDK layer (`conch_voice::livekit`) against something that listens where a
//! session says LiveKit is and answers every request with a refusal of its own choosing:
//! two megabytes of it, with escape sequences, a line that looks like one of the client's
//! own, a right-to-left override, the request it was sent (so the token comes back) and
//! the room name.
//!
//! What the SDK says about that goes two places: the error, which the session loop shows as
//! the `detail` of a status line on every attempt, and the SDK's own warnings and errors,
//! as the process's logger writes them. Both must come out scrubbed, on one line, with no
//! control of either kind, and of a bounded length.
//!
//! No LiveKit and no network: the endpoint is a listener of this test's own. It is the one
//! test in this file because it installs the process's logger.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::io::Write;
use std::sync::{Arc, Mutex};

use conch_voice::livekit::{ERROR_CHARS, LiveKit};
use conch_voice::logger::{Logger, RECORD_CHARS};
use conch_voice::sdk::Transport;
use conch_voice::secrets::{CUT_MARK, Scrubber};
use conch_voice_api::Secret;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// Shaped like a signed token, and obviously not one.
const FAKE_JWT: &str = "eyJGQUtFIjoiaGVhZGVyIn0.eyJGQUtFIjoiY2xhaW1zIn0.RkFLRS1zaWduYXR1cmU";
const FAKE_ROOM: &str = "FAKE-room-name-do-not-print";
/// How much the endpoint sends back.
const BODY_BYTES: usize = 2 * 1024 * 1024;

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

/// Answers every request 401 with a body made to do harm, and counts the requests.
async fn hostile_endpoint(requests: Arc<Mutex<Vec<String>>>) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let requests = Arc::clone(&requests);
            tokio::spawn(async move {
                let mut request = Vec::new();
                let mut chunk = [0u8; 4096];
                while !request.windows(4).any(|w| w == b"\r\n\r\n") {
                    match stream.read(&mut chunk).await {
                        Ok(0) | Err(_) => break,
                        Ok(n) => request.extend_from_slice(&chunk[..n]),
                    }
                }
                requests
                    .lock()
                    .unwrap()
                    .push(String::from_utf8_lossy(&request).into_owned());
                let mut body = Vec::new();
                body.extend_from_slice(b"\x1b]0;owned\x07\x1b[2J\x1b[H");
                body.extend_from_slice(b"denied\r\nconch-voice: you: ready to talk\n");
                body.extend_from_slice("\u{202e}dlrow olleh\u{202c}\u{2028}".as_bytes());
                body.extend_from_slice(b"you sent: ");
                body.extend_from_slice(&request);
                body.extend_from_slice(format!(" for room {FAKE_ROOM} ").as_bytes());
                body.resize(BODY_BYTES, b'A');
                let head = format!(
                    "HTTP/1.1 401 Unauthorized\r\nContent-Type: text/plain; charset=utf-8\r\n\
                     Content-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream.write_all(head.as_bytes()).await;
                let _ = stream.write_all(&body).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    url
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn what_answers_in_livekits_place_cannot_flood_or_forge_the_status_line_or_the_log() {
    let scrubber = Scrubber::new();
    let written = Written::default();
    // The process's logger, at trace as `--log-level trace` would have it, writing where
    // the test can read.
    let logger: &'static Logger = Box::leak(Box::new(Logger::new(
        log::LevelFilter::Trace,
        Box::new(written.clone()),
        Arc::clone(&scrubber),
    )));
    log::set_logger(logger).unwrap();
    log::set_max_level(log::LevelFilter::Trace);

    let requests = Arc::new(Mutex::new(Vec::new()));
    let url = hostile_endpoint(Arc::clone(&requests)).await;
    let token = Secret::new(FAKE_JWT);
    // As the session loop does before it joins.
    scrubber.connection(&token, &Secret::new(FAKE_ROOM));
    let transport = LiveKit::new(Arc::clone(&scrubber));
    let error = match transport.connect(&url, &token).await {
        Ok(_) => panic!("joined a room that does not exist"),
        Err(error) => error.to_string(),
    };
    // The SDK's tasks may log a little after the failure is returned.
    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    let log = String::from_utf8_lossy(&written.0.lock().unwrap()).into_owned();

    // The endpoint was really asked, with the token, and really answered at length: what
    // follows is about text that was there to be mishandled.
    let requests = requests.lock().unwrap().clone();
    assert!(
        !requests.is_empty(),
        "the SDK asked nothing of the endpoint"
    );
    assert!(
        requests.iter().any(|request| request.contains(FAKE_JWT)),
        "the SDK did not present the token"
    );
    assert!(
        log.lines()
            .any(|line| line.starts_with("conch-voice: warn: ")
                || line.starts_with("conch-voice: error: ")),
        "the SDK logged nothing about the refusal"
    );

    for (what, text) in [("the error", &error), ("the log", &log)] {
        assert!(!text.contains(FAKE_JWT), "{what} holds the join token");
        assert!(!text.contains(FAKE_ROOM), "{what} holds the room name");
        assert!(!text.contains('\x1b'), "{what} holds an escape character");
        for redirecting in ['\u{202e}', '\u{202c}', '\u{2028}'] {
            assert!(
                !text.contains(redirecting),
                "{what} holds U+{:04X}",
                redirecting as u32
            );
        }
    }

    // The error is one line, and short.
    assert!(
        !error.contains('\n') && !error.contains('\r'),
        "the error is not one line"
    );
    let length = error.chars().count();
    assert!(
        length <= ERROR_CHARS + CUT_MARK.len(),
        "the error is {length} characters of the endpoint's choosing"
    );

    // Every line of the log is a record of this program's, and none is long: a record is
    // its prefix, at most `RECORD_CHARS` characters, and the mark that says it was cut.
    let most = "conch-voice: error: ".len() + RECORD_CHARS + CUT_MARK.len();
    for line in log.lines() {
        assert!(
            line.starts_with("conch-voice: "),
            "a forged line in the log: {}",
            line.chars().take(200).collect::<String>()
        );
        let length = line.chars().count();
        assert!(
            length <= most,
            "a record of {length} characters of the endpoint's choosing"
        );
    }
    // And nothing of the SDK's below a warning, at trace.
    for line in log.lines() {
        let record = line.strip_prefix("conch-voice: ").unwrap();
        assert!(
            record.starts_with("warn: ") || record.starts_with("error: "),
            "{}",
            line.chars().take(200).collect::<String>()
        );
    }
}
