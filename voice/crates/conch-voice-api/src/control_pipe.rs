//! The attach line of a managed helper whose events channel is an endpoint it connects to
//! (`conch.voice_control_pipe.v1`, issue #893): the twin of `pkg/schema/voice_control_pipe.go`,
//! checked against the same fixtures in `pkg/schema/testdata/voice-control-pipe/`.
//!
//! When the helper is started as `conch-voice managed --events-pipe <endpoint>` (a `\\.\pipe\`
//! name on Windows, a Unix socket path on Linux; `docs/design/windows-desktop.md` §3.4,
//! `docs/design/linux-desktop.md` §5.2) the first line it writes on that connection is one
//! `attached` object carrying the nonce the owner put in its environment
//! (`CONCH_VOICE_ATTACH`); its second line is the unchanged v1 hello. The owner reads the
//! attach line first, compares the nonce in constant time, and only then reads the connection
//! as the control protocol.
//!
//! The line is a spawn-to-pipe binding, defence in depth, not authentication of either side:
//! it tells the connection made by the spawned process apart from one made by a same-user
//! process that reached the endpoint without reading the environment, and it does nothing
//! against same-user code, which can read the environment and take over the endpoint; that
//! hijack is out of scope and named. Across users the pipe's DACL or the `0o700` runtime
//! directory is the boundary. Authority arrives only on standard input, so a stolen or failed
//! attach costs the confidentiality of state lines and the join, never the microphone.
//!
//! The nonce is the one secret-shaped value on any voice control channel: one-time, spent
//! when the owner reads it, and redacted from every diagnostic. Here that means
//! [`VoiceControlPipeAttachedV1`]'s `Debug` prints it as a placeholder and no error of this
//! module repeats it. The helper never decodes the nonce: it checks its shape by the three
//! byte rules ([`validate_nonce`]) and copies it, so no base64 crate is needed.
//!
//! The helper only writes this line ([`to_line`](crate::to_line)); [`parse_attached`] is the
//! owner's reader, here for parity with Go's `ParseVoiceControlPipeAttached` and held to the
//! same conformance cases.

use std::collections::BTreeSet;
use std::fmt;

use serde::Serialize;
use serde::de::{self, DeserializeSeed, Deserializer, IgnoredAny, MapAccess, Visitor};

use crate::control::{
    VOICE_CONTROL_MAX_LINE_BYTES, VoiceControlErrorCode, VoiceControlRefusal,
    has_lone_surrogate_escape, message, refused, vocabulary,
};
use crate::error::Error;
use crate::types::wire_object;

/// The versioned name the attach line carries in its `schema` field. It is not negotiated:
/// the line precedes the hello.
pub const VOICE_CONTROL_PIPE_SCHEMA_V1: &str = "conch.voice_control_pipe.v1";
/// The helper's option naming the endpoint to connect to; never a secret.
pub const VOICE_CONTROL_PIPE_FLAG: &str = "--events-pipe";
/// The environment variable the owner sets to the nonce. Read only when
/// [`VOICE_CONTROL_PIPE_FLAG`] is given.
pub const VOICE_CONTROL_PIPE_NONCE_ENV: &str = "CONCH_VOICE_ATTACH";
/// The nonce's entropy: 32 random bytes.
pub const VOICE_CONTROL_PIPE_NONCE_BYTES: usize = 32;
/// The nonce as written: 32 bytes in unpadded base64url are 43 characters.
pub const VOICE_CONTROL_PIPE_NONCE_CHARS: usize = 43;

/// The characters of the base64url alphabet whose two low bits are zero: the only ones a
/// 43-character encoding of 32 bytes can end with. Go checks the same rule with a strict
/// decode; the two agree.
const NONCE_LAST_BYTES: &[u8] = b"AEIMQUYcgkosw048";

vocabulary!(
    /// What a line of this schema is. v1 has one kind.
    VoiceControlPipeKind, "not a voice control pipe kind", {
    /// The one line of v1: this connection was made by the process started with this nonce.
    Attached = "attached",
});

/// The attach line (helper to owner): the first line on an events endpoint, written once,
/// before the hello. Its field list is exact: nothing but the nonce rides on it.
///
/// `Debug` prints the nonce as a placeholder, as [`Secret`](crate::Secret) does; the field is
/// a plain `String` because the helper copies it onto the line and then forgets it.
///
/// The derived `==` compares the nonce with an early exit and is for tests only: an owner
/// compares the nonce it generated with [`VoiceControlPipeAttachedV1::matches`], which takes
/// the same time whatever the two values are.
#[derive(Clone, PartialEq, Eq, Serialize)]
pub struct VoiceControlPipeAttachedV1 {
    /// Always [`VOICE_CONTROL_PIPE_SCHEMA_V1`].
    pub schema: String,
    /// Always [`VoiceControlPipeKind::Attached`].
    pub kind: VoiceControlPipeKind,
    /// The value of `CONCH_VOICE_ATTACH` as the helper found it: 43 characters of canonical
    /// unpadded base64url ([`validate_nonce`]).
    pub nonce: String,
}

wire_object!(VoiceControlPipeAttachedV1, "a voice control pipe attach line", unknown = ignored, {
    schema: required,
    kind: required,
    nonce: required,
});

impl fmt::Debug for VoiceControlPipeAttachedV1 {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("VoiceControlPipeAttachedV1")
            .field("schema", &self.schema)
            .field("kind", &self.kind)
            .field("nonce", &format_args!("<redacted>"))
            .finish()
    }
}

impl VoiceControlPipeAttachedV1 {
    /// The attach line for `nonce`, which must pass [`validate_nonce`].
    ///
    /// # Errors
    ///
    /// [`Error::Invalid`] for a nonce that is not 43 characters of canonical unpadded
    /// base64url; the error never repeats the value.
    pub fn new(nonce: &str) -> Result<Self, Error> {
        validate_nonce(nonce)?;
        Ok(Self {
            schema: VOICE_CONTROL_PIPE_SCHEMA_V1.to_owned(),
            kind: VoiceControlPipeKind::Attached,
            nonce: nonce.to_owned(),
        })
    }

    /// Whether the line's nonce is `nonce`, the value the owner generated: Go's `Matches`.
    /// The comparison is a fold over every byte of both values, at least
    /// [`VOICE_CONTROL_PIPE_NONCE_CHARS`] of them, that never returns early, so the time it
    /// takes does not depend on where the two first differ; the lengths are not secret and
    /// are folded in. A line with no nonce never matches. No new crate: the fold is a dozen
    /// lines, and `std::hint::black_box` keeps it from being shortened.
    pub fn matches(&self, nonce: &str) -> bool {
        let (ours, theirs) = (self.nonce.as_bytes(), nonce.as_bytes());
        if ours.is_empty() {
            return false;
        }
        let span = ours
            .len()
            .max(theirs.len())
            .max(VOICE_CONTROL_PIPE_NONCE_CHARS);
        let mut diff = u8::from(ours.len() != theirs.len());
        for at in 0..span {
            let a = ours.get(at).copied().unwrap_or(0);
            let b = theirs.get(at).copied().unwrap_or(0);
            diff = std::hint::black_box(diff | (a ^ b));
        }
        diff == 0
    }

    fn check(&self) -> Result<(), String> {
        if self.schema != VOICE_CONTROL_PIPE_SCHEMA_V1 {
            return Err(format!(
                "voice control pipe attached schema must be {VOICE_CONTROL_PIPE_SCHEMA_V1:?}"
            ));
        }
        if self.kind != VoiceControlPipeKind::Attached {
            return Err(format!(
                "voice control pipe attached kind must be {:?}",
                VoiceControlPipeKind::Attached
            ));
        }
        check_nonce(&self.nonce)
    }
}

message!(VoiceControlPipeAttachedV1);

/// Whether `nonce` is well-formed: [`VOICE_CONTROL_PIPE_NONCE_CHARS`] bytes of the base64url
/// alphabet (`A-Z`, `a-z`, `0-9`, `-`, `_`), unpadded and canonical, so that the value is
/// the encoding of exactly [`VOICE_CONTROL_PIPE_NONCE_BYTES`] bytes with the two trailing
/// bits zero. It says nothing about whether the nonce is the owner's.
///
/// # Errors
///
/// [`Error::Invalid`] naming the rule broken, by position and count only: the text never
/// repeats the value.
pub fn validate_nonce(nonce: &str) -> Result<(), Error> {
    check_nonce(nonce).map_err(|problem| Error::Invalid {
        what: "voice control pipe nonce",
        problem,
    })
}

fn check_nonce(nonce: &str) -> Result<(), String> {
    let bytes = nonce.as_bytes();
    if bytes.len() != VOICE_CONTROL_PIPE_NONCE_CHARS {
        return Err(format!(
            "it must be {VOICE_CONTROL_PIPE_NONCE_CHARS} characters of unpadded base64url, got {}",
            bytes.len()
        ));
    }
    if let Some(at) = bytes.iter().position(|byte| !is_base64url(*byte)) {
        return Err(format!("character {at} is not in the base64url alphabet"));
    }
    match bytes.last() {
        Some(last) if NONCE_LAST_BYTES.contains(last) => Ok(()),
        _ => Err(format!(
            "it must be the canonical encoding of {VOICE_CONTROL_PIPE_NONCE_BYTES} bytes: its last character one of {}",
            String::from_utf8_lossy(NONCE_LAST_BYTES)
        )),
    }
}

fn is_base64url(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_'
}

/// The framing rules of the control protocol (`control::frame`), applied without reading
/// any value as a number: the length, UTF-8, one line, one JSON object with no key twice,
/// no lone surrogate escape. The control readers' `Head` reads `schema`, `kind`, `id` and
/// `seq` through a numeric visitor, and `serde_json` refuses a number it cannot hold
/// (`1e999`) while Go keeps every value as raw bytes and only asks whether it is a string.
/// The attach line has no `id` or `seq`, so here every value is skipped unread and the
/// three fields are read afterwards, each on its own ([`field`]), which gives Go's answer:
/// an undeclared field is ignored whatever it holds, and a declared one that is not a
/// string is `unknown_schema` or `invalid`, never `malformed`.
fn frame_attached(line: &[u8]) -> Result<&str, VoiceControlRefusal> {
    let malformed = refused(VoiceControlErrorCode::Malformed, None);
    if line.len() > VOICE_CONTROL_MAX_LINE_BYTES {
        return Err(refused(VoiceControlErrorCode::TooLong, None));
    }
    let text = std::str::from_utf8(line).map_err(|_| malformed)?;
    if text.contains('\n') {
        return Err(malformed);
    }
    serde_json::from_str::<Object>(text).map_err(|_| malformed)?;
    if has_lone_surrogate_escape(text) {
        return Err(malformed);
    }
    Ok(text)
}

/// One JSON object with no key given twice, every value skipped unread.
struct Object;

impl<'de> serde::Deserialize<'de> for Object {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct Keys;
        impl<'de> Visitor<'de> for Keys {
            type Value = Object;

            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a JSON object")
            }

            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Object, A::Error> {
                let mut keys = BTreeSet::new();
                while let Some(key) = map.next_key::<String>()? {
                    if !keys.insert(key) {
                        return Err(de::Error::custom("a key given twice"));
                    }
                    map.next_value::<IgnoredAny>()?;
                }
                Ok(Object)
            }
        }
        deserializer.deserialize_map(Keys)
    }
}

/// What a top-level field of a framed line holds, as far as the reader looks.
enum Field {
    /// A string.
    Text(String),
    /// No such field.
    Absent,
    /// Something other than a string, of which nothing was read: `null`, a number of any
    /// size, an array or an object. Go's `fieldString` says "not ok" of all of these.
    Other,
}

/// The field `key` of the object `text`, which [`frame_attached`] has accepted. Only that
/// field's value is decoded, and only as a string; every other value is skipped unread.
fn field(text: &str, key: &str) -> Field {
    struct Lookup<'k>(&'k str);
    impl<'de> DeserializeSeed<'de> for Lookup<'_> {
        type Value = Option<String>;

        fn deserialize<D: Deserializer<'de>>(
            self,
            deserializer: D,
        ) -> Result<Option<String>, D::Error> {
            deserializer.deserialize_map(self)
        }
    }
    impl<'de> Visitor<'de> for Lookup<'_> {
        type Value = Option<String>;

        fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str("a JSON object")
        }

        fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Option<String>, A::Error> {
            let mut found = None;
            while let Some(key) = map.next_key::<String>()? {
                if key == self.0 {
                    // A value that is not a string fails here, whatever it is, before any
                    // of it is converted: serde_json reads a string and nothing else.
                    found = Some(map.next_value::<String>()?);
                } else {
                    map.next_value::<IgnoredAny>()?;
                }
            }
            Ok(found)
        }
    }
    let mut deserializer = serde_json::Deserializer::from_str(text);
    match Lookup(key).deserialize(&mut deserializer) {
        Ok(Some(text)) => Field::Text(text),
        Ok(None) => Field::Absent,
        Err(_) => Field::Other,
    }
}

/// Reads the first line of an events endpoint, without its line feed: the owner's reader,
/// Go's `ParseVoiceControlPipeAttached` in Go's order. It refuses `too_long` and `malformed`
/// as the control readers do; `unknown_schema` for a schema missing, not a string or not
/// [`VOICE_CONTROL_PIPE_SCHEMA_V1`] (a v1 hello arriving first included); `invalid` for a kind
/// missing, null, not a string or empty, or a nonce missing, null, not a string or failing
/// [`validate_nonce`]; and `unknown_kind` for a kind other than `attached`. A field the line
/// does not declare is ignored. Whether the nonce is the owner's is the owner's to compare,
/// in constant time.
///
/// # Errors
///
/// The [`VoiceControlRefusal`] for a line that is not an attach line; its id is always `None`.
pub fn parse_attached(line: &[u8]) -> Result<VoiceControlPipeAttachedV1, VoiceControlRefusal> {
    let invalid = refused(VoiceControlErrorCode::Invalid, None);
    let text = frame_attached(line)?;
    match field(text, "schema") {
        Field::Text(schema) if schema == VOICE_CONTROL_PIPE_SCHEMA_V1 => {}
        _ => return Err(refused(VoiceControlErrorCode::UnknownSchema, None)),
    }
    let word = match field(text, "kind") {
        Field::Text(word) if !word.is_empty() => word,
        _ => return Err(invalid),
    };
    let nonce = match field(text, "nonce") {
        Field::Text(nonce) => nonce,
        _ => return Err(invalid),
    };
    check_nonce(&nonce).map_err(|_| invalid)?;
    let Some(kind) = VoiceControlPipeKind::from_word(&word) else {
        return Err(refused(VoiceControlErrorCode::UnknownKind, None));
    };
    let attached = VoiceControlPipeAttachedV1 {
        schema: VOICE_CONTROL_PIPE_SCHEMA_V1.to_owned(),
        kind,
        nonce,
    };
    attached.check().map_err(|_| invalid)?;
    Ok(attached)
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::path::{Path, PathBuf};

    use serde::Deserialize;
    use serde_json::Value;

    use super::*;
    use crate::control::{VoiceControlEvent, VoiceControlMessage, parse_event, to_line};
    use crate::control_v2::parse_event_v2;

    const GOLDEN_NONCE: &str = "FAKE_nonce_do_not_print_0123456789abcdefghA";
    const OTHER_NONCE: &str = "FAKE_other_nonce_not_the_one_0123456789abcA";

    /// `pkg/schema/testdata/voice-control-pipe`, read in place.
    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../pkg/schema/testdata/voice-control-pipe")
    }

    fn fixture_text(name: &str) -> String {
        let path = fixtures().join(name);
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()))
    }

    /// JSON text with the white space outside its strings taken out: Go's indented
    /// encoding, made into the compact line Go's writer writes (as `control.rs` does it).
    fn compact(text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        let (mut in_string, mut escaped) = (false, false);
        for c in text.chars() {
            if in_string {
                out.push(c);
                match (escaped, c) {
                    (true, _) => escaped = false,
                    (false, '\\') => escaped = true,
                    (false, '"') => in_string = false,
                    _ => {}
                }
            } else if c == '"' {
                in_string = true;
                out.push(c);
            } else if !c.is_whitespace() {
                out.push(c);
            }
        }
        out
    }

    fn golden() -> VoiceControlPipeAttachedV1 {
        VoiceControlPipeAttachedV1::new(GOLDEN_NONCE).unwrap()
    }

    /// The golden line is read by the owner's reader and written again byte for byte as Go
    /// writes it, by the helper's writer and from a value made with `new`.
    #[test]
    fn the_golden_line_is_written_as_go_writes_it() {
        let line = compact(&fixture_text("attached.json"));
        let attached = parse_attached(line.as_bytes()).unwrap_or_else(|r| panic!("{r}"));
        assert_eq!(attached, golden());
        assert_eq!(to_line(&attached).unwrap(), line);
        assert_eq!(to_line(&golden()).unwrap(), line);
        assert_eq!(
            serde_json::from_str::<Value>(&line).unwrap(),
            serde_json::from_str::<Value>(&fixture_text("attached.json")).unwrap()
        );
        assert_eq!(
            line.len(),
            112,
            "the line's length changed: a field was added?"
        );
    }

    #[test]
    fn every_fixture_in_the_directory_is_read_by_a_test_here_or_by_the_helpers() {
        let mut claimed: BTreeSet<String> = ["attached.json", "cases.json", "lines"]
            .iter()
            .map(|name| (*name).to_owned())
            .collect();
        // sensitive.json and the transcript are the helper's (conch-voice's tests, with the
        // --events-pipe transport of #937).
        let mut unclaimed = Vec::new();
        for entry in std::fs::read_dir(fixtures()).unwrap() {
            let name = entry.unwrap().file_name().into_string().unwrap();
            let elsewhere = name == "sensitive.json" || name.starts_with("transcript-");
            if !claimed.remove(&name) && !elsewhere {
                unclaimed.push(name);
            }
        }
        assert!(unclaimed.is_empty(), "no test reads {unclaimed:?}");
        assert!(claimed.is_empty(), "{claimed:?} are not in the directory");
    }

    #[derive(Deserialize)]
    struct Cases {
        cases: Vec<Case>,
    }

    #[derive(Deserialize)]
    struct Case {
        name: String,
        direction: String,
        line: Option<String>,
        line_hex: Option<String>,
        line_file: Option<String>,
        want: Want,
    }

    #[derive(Deserialize)]
    struct Want {
        result: String,
        kind: Option<String>,
        code: Option<String>,
        id: Option<i64>,
    }

    impl Case {
        fn line(&self) -> Vec<u8> {
            match (&self.line, &self.line_hex, &self.line_file) {
                (Some(line), None, None) => line.as_bytes().to_vec(),
                (None, Some(hex), None) => (0..hex.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
                    .collect(),
                (None, None, Some(file)) => std::fs::read(fixtures().join(file)).unwrap(),
                _ => panic!("case {:?} has not exactly one form of its line", self.name),
            }
        }
    }

    /// Every conformance case is read with the same result and code as Go reads it.
    #[test]
    fn every_conformance_case_is_read_as_go_reads_it() {
        let cases = serde_json::from_str::<Cases>(&fixture_text("cases.json"))
            .unwrap()
            .cases;
        assert!(cases.len() >= 40, "only {} cases", cases.len());
        let mut codes = BTreeSet::new();
        for case in &cases {
            assert_eq!(case.direction, "attached", "{}", case.name);
            let name = &case.name;
            let line = case.line();
            match (case.want.result.as_str(), parse_attached(&line)) {
                ("accept", Ok(attached)) => {
                    assert_eq!(
                        Some(attached.kind.as_str()),
                        case.want.kind.as_deref(),
                        "{name}"
                    );
                    assert!(
                        attached.validate().is_ok(),
                        "{name}: accepted but not valid"
                    );
                }
                ("accept", Err(refusal)) => panic!("{name}: refused: {refusal}"),
                ("refuse", Ok(attached)) => panic!("{name}: accepted as {attached:?}"),
                ("refuse", Err(refusal)) => {
                    assert_eq!(
                        Some(refusal.code.as_str()),
                        case.want.code.as_deref(),
                        "{name}"
                    );
                    assert_eq!(refusal.id, None, "{name}: id");
                    assert_eq!(case.want.id, Some(0), "{name}: the case wants an id");
                    codes.insert(refusal.code);
                    let shown = format!("{refusal}\n{refusal:?}");
                    assert!(
                        !shown.contains(GOLDEN_NONCE),
                        "{name}: the refusal shows the nonce"
                    );
                }
                (other, _) => panic!("{name}: want.result {other}"),
            }
        }
        for code in [
            VoiceControlErrorCode::TooLong,
            VoiceControlErrorCode::Malformed,
            VoiceControlErrorCode::UnknownSchema,
            VoiceControlErrorCode::Invalid,
            VoiceControlErrorCode::UnknownKind,
        ] {
            assert!(codes.contains(&code), "no case refuses with {code}");
        }
    }

    /// The constants are Go's, read from `voice_control_pipe.go` as `control.rs` reads v1's.
    #[test]
    fn the_constants_are_the_go_constants() {
        let go = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/voice_control_pipe.go"),
        )
        .unwrap();
        let value_of = |name: &str| -> String {
            let line = go
                .lines()
                .map(|line| line.trim().trim_start_matches("const "))
                .find(|line| line.starts_with(&format!("{name} ")) && line.contains(" = "))
                .unwrap_or_else(|| panic!("{name} is not in voice_control_pipe.go"));
            line.split(" = ").nth(1).unwrap().trim().to_owned()
        };
        let table: [(&str, String); 5] = [
            (
                "VoiceControlPipeSchemaV1",
                format!("{VOICE_CONTROL_PIPE_SCHEMA_V1:?}"),
            ),
            (
                "VoiceControlPipeFlag",
                format!("{VOICE_CONTROL_PIPE_FLAG:?}"),
            ),
            (
                "VoiceControlPipeNonceEnv",
                format!("{VOICE_CONTROL_PIPE_NONCE_ENV:?}"),
            ),
            (
                "VoiceControlPipeNonceBytes",
                VOICE_CONTROL_PIPE_NONCE_BYTES.to_string(),
            ),
            (
                "VoiceControlPipeNonceChars",
                VOICE_CONTROL_PIPE_NONCE_CHARS.to_string(),
            ),
        ];
        for (name, ours) in table {
            assert_eq!(value_of(name), ours, "{name}");
        }
        // The last characters Go names are these.
        let go_last = value_of("voiceControlPipeNonceLastChars");
        assert_eq!(
            go_last,
            format!("{:?}", String::from_utf8_lossy(NONCE_LAST_BYTES))
        );
    }

    /// The vocabulary is the schema's: one word.
    #[test]
    fn the_vocabulary_is_the_schemas() {
        let go = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pkg/schema/voice_control_pipe.go"),
        )
        .unwrap();
        let words: Vec<String> = go
            .lines()
            .filter_map(|line| {
                let mut parts = line.split_whitespace();
                let _name = parts.next()?;
                (parts.next()? == "VoiceControlPipeKind" && parts.next()? == "=").then_some(())?;
                Some(parts.next()?.trim_matches('"').to_owned())
            })
            .collect();
        let ours: Vec<String> = VoiceControlPipeKind::ALL
            .iter()
            .map(|kind| kind.as_str().to_owned())
            .collect();
        assert_eq!(words, ours);
        assert_eq!(ours, ["attached"]);
        assert_eq!(VoiceControlPipeKind::from_word("Attached"), None);
        assert_eq!(VoiceControlPipeKind::from_word(""), None);
    }

    /// The nonce rule, by the same table as Go's `TestValidateVoiceControlPipeNonce`.
    #[test]
    fn a_nonce_is_checked_by_the_three_byte_rules() {
        let short = &GOLDEN_NONCE[..42];
        let cases: [(&str, String, bool); 16] = [
            ("the golden nonce", GOLDEN_NONCE.to_owned(), true),
            ("the other fake nonce", OTHER_NONCE.to_owned(), true),
            (
                "both URL-safe characters, ending canonically",
                "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn-_0".to_owned(),
                true,
            ),
            ("42 characters", short.to_owned(), false),
            ("44 characters", format!("{GOLDEN_NONCE}A"), false),
            (
                "a last character that is not canonical",
                format!("{short}B"),
                false,
            ),
            (
                "a last character that is not canonical, upper",
                format!("{short}Z"),
                false,
            ),
            (
                "+",
                "FAKE+nonce_do_not_print_0123456789abcdefghA".to_owned(),
                false,
            ),
            (
                "/",
                "FAKE/nonce_do_not_print_0123456789abcdefghA".to_owned(),
                false,
            ),
            ("padding", format!("{short}="), false),
            ("64 hex characters", "0123456789abcdef".repeat(4), false),
            ("empty", String::new(), false),
            (
                "a space",
                "FAKE nonce_do_not_print_0123456789abcdefghA".to_owned(),
                false,
            ),
            (
                "a line feed, which a base64 decoder would skip",
                format!("{short}\n"),
                false,
            ),
            ("a carriage return", format!("{short}\r"), false),
            (
                "a character outside ASCII",
                format!("{}éA", &GOLDEN_NONCE[..40]),
                false,
            ),
        ];
        for (name, nonce, ok) in &cases {
            let result = validate_nonce(nonce);
            assert_eq!(result.is_ok(), *ok, "{name}: {result:?}");
            if let Err(error) = result {
                let shown = format!("{error}\n{error:?}");
                assert!(
                    nonce.is_empty() || !shown.contains(nonce.as_str()),
                    "{name}: the error repeats the value: {shown}"
                );
                assert!(matches!(error, Error::Invalid { .. }), "{name}: {error:?}");
            }
        }
        // The sixteen characters whose two low bits are zero are the canonical last ones;
        // every other character of the alphabet is refused there.
        let alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
        for last in alphabet.chars() {
            let accepted = validate_nonce(&format!("{short}{last}")).is_ok();
            assert_eq!(
                accepted,
                NONCE_LAST_BYTES.contains(&(last as u8)),
                "last character {last:?}"
            );
        }
        assert_eq!(NONCE_LAST_BYTES.len(), 16);
    }

    /// `Debug` prints the nonce as a placeholder, in both forms, and the rest as it is.
    #[test]
    fn debug_prints_the_nonce_as_a_placeholder() {
        let attached = golden();
        for shown in [format!("{attached:?}"), format!("{attached:#?}")] {
            assert!(!shown.contains(GOLDEN_NONCE), "{shown}");
            assert!(!shown.contains("FAKE"), "{shown}");
            assert!(shown.contains("<redacted>"), "{shown}");
            assert!(shown.contains(VOICE_CONTROL_PIPE_SCHEMA_V1), "{shown}");
            assert!(shown.contains("Attached"), "{shown}");
        }
        // The kind's own Debug is the derived one.
        assert_eq!(format!("{:?}", VoiceControlPipeKind::Attached), "Attached");
        assert_eq!(VoiceControlPipeKind::Attached.to_string(), "attached");
    }

    /// `new` refuses a malformed nonce, and the writer refuses what a reader must refuse.
    #[test]
    fn a_line_that_does_not_hold_together_is_not_written() {
        assert!(matches!(
            VoiceControlPipeAttachedV1::new(&GOLDEN_NONCE[..42]),
            Err(Error::Invalid { .. })
        ));
        let mut wrong_schema = golden();
        wrong_schema.schema = "conch.voice_control.v1".to_owned();
        let mut wrong_nonce = golden();
        wrong_nonce.nonce = format!("{}B", &GOLDEN_NONCE[..42]);
        let mut no_nonce = golden();
        no_nonce.nonce.clear();
        for (name, bad) in [
            ("the control protocol's schema", wrong_schema),
            ("a non-canonical nonce", wrong_nonce),
            ("no nonce", no_nonce),
        ] {
            let error = to_line(&bad).expect_err(name);
            assert!(matches!(error, Error::Invalid { .. }), "{name}: {error:?}");
            assert!(
                !format!("{error}\n{error:?}").contains("FAKE"),
                "{name}: the error shows the nonce"
            );
        }
    }

    /// The attach line is not an event: the control readers of both versions refuse it as
    /// `unknown_schema`, which is how a second attach line in the stream is caught.
    #[test]
    fn the_attach_line_is_not_an_event() {
        let line = to_line(&golden()).unwrap();
        assert_eq!(
            parse_event(line.as_bytes()).err().map(|r| r.code),
            Some(VoiceControlErrorCode::UnknownSchema)
        );
        assert_eq!(
            parse_event_v2(line.as_bytes()).err().map(|r| r.code),
            Some(VoiceControlErrorCode::UnknownSchema)
        );
        // And a hello is not an attach line.
        let hello = to_line(&crate::control::VoiceControlHelloV1::v1("0.1.0")).unwrap();
        assert_eq!(
            parse_attached(hello.as_bytes()).err().map(|r| r.code),
            Some(VoiceControlErrorCode::UnknownSchema)
        );
        assert!(matches!(
            parse_event(hello.as_bytes()),
            Ok(VoiceControlEvent::Hello(_))
        ));
    }

    /// The type decodes from a JSON object and from nothing else, and refuses `null` for
    /// any field, as every wire type of this crate does.
    #[test]
    fn decoding_is_from_an_object_only() {
        let decode = |text: &str| serde_json::from_str::<VoiceControlPipeAttachedV1>(text);
        assert_eq!(decode(&to_line(&golden()).unwrap()).unwrap(), golden());
        assert!(
            decode(&format!(
                r#"["conch.voice_control_pipe.v1","attached","{GOLDEN_NONCE}"]"#
            ))
            .is_err()
        );
        assert!(
            decode(r#"{"schema":"conch.voice_control_pipe.v1","kind":"attached","nonce":null}"#)
                .is_err()
        );
        assert!(decode(r#"{"schema":"conch.voice_control_pipe.v1","kind":"attached"}"#).is_err());
        assert!(
            decode(&format!(
                r#"{{"schema":"conch.voice_control_pipe.v1","kind":"attached","nonce":"{GOLDEN_NONCE}","nonce":"{GOLDEN_NONCE}"}}"#
            ))
            .is_err()
        );
        // An unknown field is ignored, as the reader ignores it.
        assert!(
            decode(&format!(
                r#"{{"schema":"conch.voice_control_pipe.v1","kind":"attached","nonce":"{GOLDEN_NONCE}","pid":4242}}"#
            ))
            .is_ok()
        );
    }

    /// `matches` by the same table as Go's `TestVoiceControlPipeMatches`: equal nonces
    /// match; one differing only in its last byte, and one of another length, do not; a
    /// line with no nonce never matches; and the answer is the one `==` on the nonce gives,
    /// which is the whole point of having a second way to ask.
    #[test]
    fn matches_mirrors_go_and_agrees_with_equality() {
        let short = &GOLDEN_NONCE[..42];
        let attached = golden();
        let cases: [(&str, String, bool); 9] = [
            ("the same nonce", GOLDEN_NONCE.to_owned(), true),
            (
                "the last byte differs, still canonical",
                format!("{short}E"),
                false,
            ),
            (
                "the first byte differs",
                format!("G{}", &GOLDEN_NONCE[1..]),
                false,
            ),
            ("one byte shorter", short.to_owned(), false),
            ("one byte longer", format!("{GOLDEN_NONCE}A"), false),
            (
                "twice as long, the nonce as a prefix",
                format!("{GOLDEN_NONCE}{GOLDEN_NONCE}"),
                false,
            ),
            ("empty", String::new(), false),
            ("the other fake nonce", OTHER_NONCE.to_owned(), false),
            (
                "a byte past the fixed length differs",
                format!("{GOLDEN_NONCE}\u{0}"),
                false,
            ),
        ];
        for (name, nonce, want) in &cases {
            assert_eq!(attached.matches(nonce), *want, "{name}");
            assert_eq!(attached.nonce == *nonce, *want, "{name}: == disagrees");
        }
        // Two values longer than the fixed length that differ only past it do not match.
        let mut long = golden();
        long.nonce = format!("{GOLDEN_NONCE}AB");
        assert!(!long.matches(&format!("{GOLDEN_NONCE}AC")));
        assert!(long.matches(&format!("{GOLDEN_NONCE}AB")));
        // A line with no nonce never matches, not even an empty one.
        let mut none = golden();
        none.nonce.clear();
        assert!(!none.matches(""));
        assert!(!none.matches(GOLDEN_NONCE));
    }
}
