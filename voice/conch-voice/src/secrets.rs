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
//! - the join token and the room name of each of the last [`CONNECTIONS_KEPT`] connection
//!   attempts, which the session loop registers with [`Scrubber::connection`]. They are
//!   kept after the connection has ended, because the SDK's own tasks go on running, and
//!   logging, after a room was left; they are kept for this and for nothing else;
//! - the login token, registered once with [`Scrubber::always`];
//! - anything shaped like a signed token, whoever issued it: three runs of base64url
//!   characters joined by dots, the first beginning `eyJ`. That covers the tokens LiveKit
//!   sends the SDK later, which this program never sees.
//!
//! Text from outside is also made fit for one line of a terminal ([`one_line`]) and cut to
//! a length ([`cut`]), in that order after the scrub, so that a secret is never cut in half
//! and missed.

use std::borrow::Cow;
use std::collections::VecDeque;
use std::fmt;
use std::sync::{Arc, PoisonError, RwLock};

use conch_voice_api::Secret;

/// What a secret is replaced by.
pub const REDACTED: &str = "[redacted]";

/// How many connection attempts' join token and room name are remembered. A client makes
/// one attempt at a time; sixteen is every attempt of the last minutes, and a bound on
/// what a client that reconnects for days keeps.
pub const CONNECTIONS_KEPT: usize = 16;

/// What ends a text that [`cut`] shortened.
pub const CUT_MARK: &str = " [cut]";

#[derive(Default)]
struct Known {
    /// For as long as the process runs.
    always: Vec<String>,
    /// The values of the last connection attempts, oldest first: for each, its join token
    /// and its room name, less any that was empty.
    connections: VecDeque<Vec<String>>,
}

/// Replaces secrets in text. Shared by the logger, the session loop and the SDK layer.
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
            .field("connections", &known.connections.len())
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

    /// Registers the join token and the room name of one connection attempt. They stay
    /// registered when the connection ends: the SDK may still log about a room after it
    /// was left. Only the last [`CONNECTIONS_KEPT`] attempts are remembered; the oldest
    /// goes when one more arrives.
    pub fn connection(&self, token: &Secret, room: &Secret) {
        let values: Vec<String> = [token, room]
            .into_iter()
            // An empty value is in every text, so it is not searched for.
            .filter(|secret| !secret.is_empty())
            .map(|secret| secret.expose().to_owned())
            .collect();
        let mut known = self.known.write().unwrap_or_else(PoisonError::into_inner);
        while known.connections.len() >= CONNECTIONS_KEPT {
            known.connections.pop_front();
        }
        known.connections.push_back(values);
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
                .chain(known.connections.iter().flatten())
                .map(String::as_str)
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

    /// [`Scrubber::scrub`], and then [`one_line`]: text fit to be one line on a terminal. A
    /// line break in it cannot pass for a line of this program's, and neither an escape
    /// sequence nor a change of writing direction can alter what the terminal shows.
    #[must_use]
    pub fn scrub_line(&self, text: &str) -> String {
        one_line(&self.scrub(text))
    }

    /// [`Scrubber::scrub_line`], and then [`cut`] to `most` characters: for text whose
    /// length somebody else chose. The scrub comes first, so a secret that lies across the
    /// place of the cut is replaced whole and not left as a half nothing would recognise.
    #[must_use]
    pub fn scrub_line_within(&self, text: &str, most: usize) -> String {
        cut(self.scrub_line(text), most)
    }
}

/// Whether `c` changes the direction text is shown in, or separates lines or paragraphs,
/// without being a control character: U+061C, U+200E, U+200F, U+2028, U+2029, U+202A to
/// U+202E and U+2066 to U+2069. With them a line can show something other than it holds.
fn redirects(c: char) -> bool {
    matches!(
        c,
        '\u{061C}'
            | '\u{200E}'
            | '\u{200F}'
            | '\u{2028}'..='\u{202E}'
            | '\u{2066}'..='\u{2069}'
    )
}

/// `text` with every control character, every bidirectional control and the line and
/// paragraph separators each replaced by a space.
#[must_use]
pub fn one_line(text: &str) -> String {
    text.chars()
        .map(|c| {
            if c.is_control() || redirects(c) {
                ' '
            } else {
                c
            }
        })
        .collect()
}

/// `text` if it has at most `most` characters; otherwise its first `most` characters and
/// [`CUT_MARK`]. The cut falls between characters, never inside one.
#[must_use]
pub fn cut(text: String, most: usize) -> String {
    match text.char_indices().nth(most) {
        None => text,
        Some((at, _)) => {
            let mut kept = text;
            kept.truncate(at);
            kept.push_str(CUT_MARK);
            kept
        }
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
    fn a_registered_token_and_room_are_replaced_wherever_they_appear() {
        let scrubber = Scrubber::new();
        let text = format!("joining {FAKE_ROOM} with {FAKE_JOIN}, twice: {FAKE_JOIN}");
        assert_eq!(scrubber.scrub(&text), text, "nothing is registered yet");

        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        assert_eq!(
            scrubber.scrub(&text),
            "joining [redacted] with [redacted], twice: [redacted]"
        );
    }

    /// The SDK's tasks for a room go on running, and logging, after the room was left and
    /// while the next connection is made: nothing about an earlier connection is forgotten
    /// because a later one began.
    #[test]
    fn a_connections_values_are_still_replaced_after_later_connections_began() {
        let scrubber = Scrubber::new();
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        for n in 1..CONNECTIONS_KEPT {
            scrubber.connection(
                &Secret::new(format!("later-FAKE-token-{n}")),
                &Secret::new(format!("later-FAKE-room-{n}")),
            );
        }
        assert_eq!(
            scrubber.scrub(&format!("closing {FAKE_ROOM} ({FAKE_JOIN})")),
            "closing [redacted] ([redacted])"
        );
    }

    #[test]
    fn only_the_last_sixteen_connections_are_remembered() {
        let scrubber = Scrubber::new();
        let token = |n: usize| format!("FAKE-token-number-{n:03}");
        let room = |n: usize| format!("FAKE-room-number-{n:03}");
        for n in 0..CONNECTIONS_KEPT + 1 {
            scrubber.connection(&Secret::new(token(n)), &Secret::new(room(n)));
        }
        // The oldest pair went when the seventeenth arrived, and only that pair.
        assert_eq!(scrubber.scrub(&token(0)), token(0));
        assert_eq!(scrubber.scrub(&room(0)), room(0));
        for n in 1..CONNECTIONS_KEPT + 1 {
            assert_eq!(scrubber.scrub(&token(n)), REDACTED, "token {n}");
            assert_eq!(scrubber.scrub(&room(n)), REDACTED, "room {n}");
        }
        // However many more there are, what is kept does not grow.
        for n in 100..400 {
            scrubber.connection(&Secret::new(token(n)), &Secret::new(room(n)));
        }
        let known = scrubber.known.read().unwrap();
        assert_eq!(known.connections.len(), CONNECTIONS_KEPT);
        assert_eq!(known.connections.iter().flatten().count(), 32);
    }

    /// A value that is part of another is replaced after it, not before: otherwise the
    /// longer one would no longer be found, and what is left of it would be written.
    #[test]
    fn a_value_that_holds_another_is_replaced_whole() {
        let scrubber = Scrubber::new();
        let room = "FAKE-room";
        let token = format!("FAKE-head.{room}.FAKE-tail");
        scrubber.connection(&Secret::new(token.clone()), &Secret::new(room));
        assert_eq!(
            scrubber.scrub(&format!("token={token}")),
            "token=[redacted]"
        );
        assert_eq!(scrubber.scrub(&format!("room={room}")), "room=[redacted]");
    }

    #[test]
    fn the_login_token_is_replaced_for_as_long_as_the_process_runs() {
        let scrubber = Scrubber::new();
        scrubber.always(&Secret::new("conch_FAKE_login_token"));
        for n in 0..CONNECTIONS_KEPT * 2 {
            scrubber.connection(
                &Secret::new(format!("FAKE-token-{n}")),
                &Secret::new("room"),
            );
        }
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
        scrubber.connection(&Secret::new(""), &Secret::new("r7"));
        assert_eq!(scrubber.scrub("room r7 is full"), "room [redacted] is full");
    }

    #[test]
    fn a_scrubbed_line_has_no_control_character_and_no_secret() {
        let scrubber = Scrubber::new();
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let text = format!("refused\r\n\x1b[2Jconnected\x07 {FAKE_JOIN}\t{FAKE_ROOM}\u{85}end");
        assert_eq!(
            scrubber.scrub_line(&text),
            "refused   [2Jconnected  [redacted] [redacted] end"
        );
        assert_eq!(one_line("plain text, ünïcödé"), "plain text, ünïcödé");
    }

    /// A right-to-left override makes a terminal show `dlrow olleh` as `hello world`; a
    /// line separator starts a new line though it is no control character. None of them
    /// gets through, and nothing else is touched.
    #[test]
    fn one_line_has_no_bidirectional_control_and_no_separator() {
        let redirecting = [
            '\u{061C}', '\u{200E}', '\u{200F}', '\u{2028}', '\u{2029}', '\u{202A}', '\u{202B}',
            '\u{202C}', '\u{202D}', '\u{202E}', '\u{2066}', '\u{2067}', '\u{2068}', '\u{2069}',
        ];
        for c in redirecting {
            assert_eq!(one_line(&format!("a{c}b")), "a b", "U+{:04X}", c as u32);
        }
        assert_eq!(
            one_line("denied \u{202E}dlrow olleh\u{202C} by\u{2028}conch-voice: connected"),
            "denied  dlrow olleh  by conch-voice: connected"
        );
        // Their neighbours in the table, and text in a right-to-left script, pass.
        let kept = "\u{061B}\u{061D}\u{200D}\u{2010}\u{2027}\u{202F}\u{2065}\u{206A} שלום مرحبا";
        assert_eq!(one_line(kept), kept);
    }

    #[test]
    fn a_text_is_cut_between_characters_and_says_that_it_was() {
        assert_eq!(cut("short".into(), 5), "short");
        assert_eq!(cut("shorter".into(), 5), "short [cut]");
        assert_eq!(cut(String::new(), 0), "");
        // Counted in characters, and never cut inside one.
        assert_eq!(cut("ünïcödé".into(), 7), "ünïcödé");
        assert_eq!(cut("ünïcödé".into(), 3), "ünï [cut]");
        let long = "é".repeat(5_000);
        let kept = cut(long, 512);
        assert_eq!(kept.chars().count(), 512 + CUT_MARK.chars().count());
    }

    /// The scrub comes before the cut. Done the other way, a secret lying across the place
    /// of the cut would be cut in half first, and its first half would be written.
    #[test]
    fn a_secret_that_lies_across_the_cut_is_replaced_and_not_cut_in_half() {
        let scrubber = Scrubber::new();
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        for (secret, lead) in [(FAKE_JOIN, 500), (FAKE_ROOM, 505), (FAKE_JWT, 490)] {
            let text = format!("{}{secret} and more", "x".repeat(lead));
            let line = scrubber.scrub_line_within(&text, 512);
            assert!(
                !line.contains(&secret[..8]),
                "the start of a secret was written: {}",
                &line[lead.saturating_sub(4)..]
            );
            assert!(
                line.ends_with(CUT_MARK) || line.ends_with("and more"),
                "{line}"
            );
            assert!(line.chars().count() <= 512 + CUT_MARK.len());
        }
    }

    #[test]
    fn debug_shows_no_registered_value() {
        let scrubber = Scrubber::new();
        scrubber.always(&Secret::new("conch_FAKE_login_token"));
        scrubber.connection(&Secret::new(FAKE_JOIN), &Secret::new(FAKE_ROOM));
        let shown = format!("{scrubber:?}");
        for secret in ["conch_FAKE_login_token", FAKE_JOIN, FAKE_ROOM] {
            assert!(!shown.contains(secret), "{shown}");
        }
    }
}
