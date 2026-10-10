//! The scrubber: what replaces a secret in text that did not come from this program.
//!
//! Three things must never reach output (`docs/design/conch-voice.md` §5): the login token,
//! a join token and a room name. This crate's own code cannot print them, because they are
//! held in [`Secret`], which has no `Display` and a placeholder for `Debug`. Text written by
//! the SDK and what it links is another matter: it was measured clean at warning and error
//! level in four situations (§13 row 10), which is not every error path. So every such text
//! (a log record, an error's message) goes through [`Scrubber::scrub`] before it is written,
//! which replaces:
//!
//! - the join token and the room name of the connection in progress, which the session loop
//!   registers with [`Scrubber::connection`] and which are forgotten when the returned guard
//!   is dropped;
//! - the login token, registered once with [`Scrubber::always`];
//! - anything shaped like a signed token, whoever issued it: three runs of base64url
//!   characters joined by dots, the first beginning `eyJ`. That covers the tokens LiveKit
//!   sends the SDK later, which this program never sees.

use std::borrow::Cow;
use std::fmt;
use std::sync::{Arc, PoisonError, RwLock};

use conch_voice_api::Secret;

/// What a secret is replaced by.
pub const REDACTED: &str = "[redacted]";

#[derive(Default)]
struct Known {
    /// For as long as the process runs.
    always: Vec<String>,
    /// For one connection: the guard's number, and the value.
    connection: Vec<(u64, String)>,
    next_guard: u64,
}

/// Replaces secrets in text. Shared by the logger and the SDK layer.
#[derive(Default)]
pub struct Scrubber {
    known: RwLock<Known>,
}

/// Shows how many values are known and none of them.
impl fmt::Debug for Scrubber {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let known = self.known.read().unwrap_or_else(PoisonError::into_inner);
        f.debug_struct("Scrubber")
            .field("always", &known.always.len())
            .field("connection", &known.connection.len())
            .finish()
    }
}

impl Scrubber {
    /// A scrubber that knows no value yet. It still replaces anything shaped like a token.
    #[must_use]
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// Registers a value for as long as the process runs: the login token.
    pub fn always(&self, secret: &Secret) {
        if secret.is_empty() {
            return;
        }
        let mut known = self.known.write().unwrap_or_else(PoisonError::into_inner);
        known.always.push(secret.expose().to_owned());
    }

    /// Registers the join token and the room name of one connection. They are forgotten when
    /// the guard is dropped, which is when the connection has ended or the attempt failed.
    #[must_use = "the values are forgotten when the guard is dropped"]
    pub fn connection(self: &Arc<Self>, token: &Secret, room: &Secret) -> ConnectionSecrets {
        let mut known = self.known.write().unwrap_or_else(PoisonError::into_inner);
        let guard = known.next_guard;
        known.next_guard = known.next_guard.wrapping_add(1);
        for secret in [token, room] {
            // An empty value is in every text, so it is not searched for.
            if !secret.is_empty() {
                known.connection.push((guard, secret.expose().to_owned()));
            }
        }
        ConnectionSecrets {
            scrubber: Arc::clone(self),
            guard,
        }
    }

    /// `text` with every registered value, and anything shaped like a signed token, replaced
    /// by [`REDACTED`]. Text that holds none comes back as it was, without a copy.
    #[must_use]
    pub fn scrub<'a>(&self, text: &'a str) -> Cow<'a, str> {
        let mut out = Cow::Borrowed(text);
        {
            let known = self.known.read().unwrap_or_else(PoisonError::into_inner);
            let mut values: Vec<&str> = known
                .always
                .iter()
                .map(String::as_str)
                .chain(known.connection.iter().map(|(_, value)| value.as_str()))
                .collect();
            // Longest first, so a whole token goes before a value that is part of it.
            values.sort_by_key(|value| std::cmp::Reverse(value.len()));
            for value in values {
                if out.contains(value) {
                    out = Cow::Owned(out.replace(value, REDACTED));
                }
            }
        }
        let spans = token_spans(&out);
        if spans.is_empty() {
            return out;
        }
        let mut cleaned = String::with_capacity(out.len());
        let mut from = 0;
        for (start, end) in spans {
            cleaned.push_str(out.get(from..start).unwrap_or_default());
            cleaned.push_str(REDACTED);
            from = end;
        }
        cleaned.push_str(out.get(from..).unwrap_or_default());
        Cow::Owned(cleaned)
    }

    /// [`Scrubber::scrub`], and then every control character replaced by a space: text fit
    /// to be one line on a terminal. A line break in it cannot pass for a line of this
    /// program's, and an escape sequence cannot drive the terminal.
    #[must_use]
    pub fn scrub_line(&self, text: &str) -> String {
        one_line(&self.scrub(text))
    }
}

/// `text` with every control character replaced by a space.
#[must_use]
pub fn one_line(text: &str) -> String {
    text.chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .collect()
}

/// The join token and room name of one connection, registered with the scrubber. Dropping
/// it makes the scrubber forget them.
pub struct ConnectionSecrets {
    scrubber: Arc<Scrubber>,
    guard: u64,
}

impl Drop for ConnectionSecrets {
    fn drop(&mut self) {
        let mut known = self
            .scrubber
            .known
            .write()
            .unwrap_or_else(PoisonError::into_inner);
        known.connection.retain(|(guard, _)| *guard != self.guard);
    }
}

/// Shows nothing of what it guards.
impl fmt::Debug for ConnectionSecrets {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ConnectionSecrets").finish_non_exhaustive()
    }
}

fn is_base64url(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_')
}

/// Byte ranges of `text` shaped like a signed token: three runs of base64url characters
/// joined by dots, the first beginning `eyJ` (a JSON object, base64-encoded). Every bound is
/// next to an ASCII character, so each range can be cut out of the text.
fn token_spans(text: &str) -> Vec<(usize, usize)> {
    let bytes = text.as_bytes();
    // The end of the run of base64url characters that starts at `from`.
    let run = |from: usize| -> usize {
        bytes
            .iter()
            .skip(from)
            .position(|byte| !is_base64url(*byte))
            .map_or(bytes.len(), |length| from + length)
    };
    let mut spans = Vec::new();
    let mut at = 0;
    while let Some(found) = text.get(at..).and_then(|rest| rest.find("eyJ")) {
        let start = at + found;
        let first = run(start);
        // Whatever follows, the search goes on after this run.
        at = first.max(start + 3);
        if bytes.get(first) != Some(&b'.') {
            continue;
        }
        let second = run(first + 1);
        if second == first + 1 || bytes.get(second) != Some(&b'.') {
            continue;
        }
        let third = run(second + 1);
        if third == second + 1 {
            continue;
        }
        spans.push((start, third));
        at = third;
    }
    spans
}

#[cfg(test)]
mod tests {
    use super::*;

    const FAKE_JOIN: &str = "FAKE-join-token-do-not-print-0123456789";
    const FAKE_ROOM: &str = "FAKE-room-name-do-not-print";
    /// Shaped like a signed token, and obviously not one.
    const FAKE_JWT: &str = "eyJGQUtFIjoiaGVhZGVyIn0.eyJGQUtFIjoiY2xhaW1zIn0.RkFLRS1zaWduYXR1cmU";

    #[test]
    fn a_registered_token_and_room_are_replaced_and_forgotten_with_the_guard() {
        let scrubber = Scrubber::new();
        let text = format!("joining {FAKE_ROOM} with {FAKE_JOIN}, twice: {FAKE_JOIN}");
        assert_eq!(scrubber.scrub(&text), text, "nothing is registered yet");

        let guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let scrubbed = scrubber.scrub(&text);
        assert_eq!(
            scrubbed,
            "joining [redacted] with [redacted], twice: [redacted]"
        );

        drop(guard);
        assert_eq!(scrubber.scrub(&text), text, "forgotten with the guard");
    }

    #[test]
    fn one_guard_does_not_forget_another_connections_values() {
        let scrubber = Scrubber::new();
        let first = scrubber.connection(&Secret::new("first-FAKE-token"), &Secret::new("room"));
        let _second = scrubber.connection(&Secret::new("second-FAKE-token"), &Secret::new("room"));
        drop(first);
        assert_eq!(scrubber.scrub("second-FAKE-token"), REDACTED);
        assert_eq!(scrubber.scrub("first-FAKE-token"), "first-FAKE-token");
    }

    #[test]
    fn the_login_token_is_replaced_for_as_long_as_the_process_runs() {
        let scrubber = Scrubber::new();
        scrubber.always(&Secret::new("conch_FAKE_login_token"));
        assert_eq!(
            scrubber.scrub("Authorization: Bearer conch_FAKE_login_token"),
            "Authorization: Bearer [redacted]"
        );
    }

    #[test]
    fn anything_shaped_like_a_signed_token_is_replaced_though_never_registered() {
        let scrubber = Scrubber::new();
        let text = format!("ws://127.0.0.1:1/rtc?access_token={FAKE_JWT}&auto=1 and {FAKE_JWT}.");
        assert_eq!(
            scrubber.scrub(&text),
            "ws://127.0.0.1:1/rtc?access_token=[redacted]&auto=1 and [redacted]."
        );
    }

    #[test]
    fn text_that_only_resembles_a_token_is_left_alone() {
        let scrubber = Scrubber::new();
        for text in [
            "eyJ",
            "eyJhbGciOiJIUzI1NiJ9",
            "eyJhbGciOiJIUzI1NiJ9.",
            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwNyJ9",
            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwNyJ9.",
            "eyJhbGciOiJIUzI1NiJ9..c2ln",
            "a.b.c and 1.2.3 and file.tar.gz",
            "grüße eyJ ünïcödé",
        ] {
            assert_eq!(scrubber.scrub(text), text);
            assert!(matches!(scrubber.scrub(text), Cow::Borrowed(_)), "{text}");
        }
    }

    #[test]
    fn a_token_between_characters_that_are_not_ascii_is_cut_out_whole() {
        let scrubber = Scrubber::new();
        let text = format!("é{FAKE_JWT}é");
        assert_eq!(scrubber.scrub(&text), "é[redacted]é");
    }

    #[test]
    fn an_empty_value_is_not_searched_for_and_a_short_one_is() {
        let scrubber = Scrubber::new();
        scrubber.always(&Secret::new(""));
        let _guard = scrubber.connection(&Secret::new(""), &Secret::new("r7"));
        assert_eq!(scrubber.scrub("room r7 is full"), "room [redacted] is full");
    }

    #[test]
    fn a_scrubbed_line_has_no_control_character_and_no_secret() {
        let scrubber = Scrubber::new();
        let _guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let text = format!("refused\r\n\x1b[2Jconnected\x07 {FAKE_JOIN}\t{FAKE_ROOM}\u{85}end");
        assert_eq!(
            scrubber.scrub_line(&text),
            "refused   [2Jconnected  [redacted] [redacted] end"
        );
        assert_eq!(one_line("plain text, ünïcödé"), "plain text, ünïcödé");
    }

    #[test]
    fn debug_shows_no_registered_value() {
        let scrubber = Scrubber::new();
        scrubber.always(&Secret::new("conch_FAKE_login_token"));
        let guard = scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let shown = format!("{scrubber:?} {guard:?}");
        for secret in ["conch_FAKE_login_token", FAKE_JOIN, FAKE_ROOM] {
            assert!(!shown.contains(secret), "{shown}");
        }
    }
}
