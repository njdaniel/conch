//! The server address: how it becomes the key a login is stored under, and how request
//! addresses are built from it.
//!
//! `conch login` stores a token under a key that `NormalizeServer` in
//! `internal/cli/credentials.go` makes from the address the user gave. This program has to
//! make the same key from the same address, or it would report "not signed in" to a user
//! who is. So the two are held together by one file of vectors,
//! `internal/cli/testdata/credentials-vectors.json`, which a Go test and the test at the
//! end of this file both run.
//!
//! The key is what Go's `net/url` makes of the address, and `net/url` is not the WHATWG
//! parser that `reqwest::Url` is: it keeps a port that is the scheme's default, keeps `..`
//! in a path, keeps an IPv6 literal as written, and does no IDNA, where the WHATWG parser
//! changes all four. A key made with `reqwest::Url` would therefore differ from Go's for
//! addresses people do type (`http://host:80`). So the parts of `net/url` that
//! `NormalizeServer` depends on are written out here by hand, function for function, from
//! Go 1.25's `net/url/url.go`. A change to Go's parser that moves a key shows up as the
//! Go half of the vectors failing.
//!
//! One thing cannot be copied from a file: lower-casing a host that has non-ASCII letters
//! uses the Unicode tables of whichever compiler built the program, Go's on one side and
//! Rust's on the other. Go 1.25 has Unicode 15.0 and Rust 1.96 a later version, which knows
//! 55 capital letters that 15.0 does not. Every code point was run through both; those 55
//! are the whole difference, and [`lower`] leaves them alone as Go does. The vectors hold
//! one from each block, so that a Go whose tables have caught up fails its half of the
//! test and this list is shortened. A letter added to Unicode after both is not covered
//! by any of this until someone adds it.

use std::fmt;

use crate::error::Error;

/// A server address `conch` would accept, parsed as Go parses it.
///
/// It holds no password: any `user:password@` in the address is dropped while parsing, as
/// it is from the key (the Go client sends its bearer token and never the userinfo).
#[derive(Clone, PartialEq, Eq)]
pub struct ServerAddress {
    /// `http` or `https`.
    scheme: &'static str,
    /// Host and optional port as they go into a URL: Go's `URL.Host`, escaped again.
    host: String,
    /// Go's `URL.EscapedPath()` with trailing slashes removed: empty, or a prefix that
    /// starts with a slash.
    prefix: String,
    /// The key the login is stored under.
    key: String,
    /// Whether the address has a query or a fragment. Neither is part of the key, and a
    /// client refuses an address that has one, as `cli.NewClient` does.
    has_query_or_fragment: bool,
}

const NOT_HTTP: Error = Error::InvalidServer {
    reason: "it must be an http:// or https:// URL with a host",
};
const MALFORMED: Error = Error::InvalidServer {
    reason: "it is not a well-formed URL",
};

impl ServerAddress {
    /// Parses a server address by the rule of `cli.NormalizeServer`: an `http` or `https`
    /// URL with a host. The error never repeats the address, which may hold a password.
    pub fn parse(raw: &str) -> Result<Self, Error> {
        // url.Parse: the fragment is cut off first and looked at last.
        let (address, fragment) = raw.split_once('#').unwrap_or((raw, ""));
        if address.bytes().any(|b| b < b' ' || b == 0x7f) {
            return Err(MALFORMED);
        }

        // getScheme. A scheme is letters, digits, '+', '-' and '.', up to the first ':'.
        // Only two are accepted and both are letters, so anything getScheme would stop at
        // before reaching one of them is refused either way.
        let (scheme, rest) = if let Some(rest) = strip_prefix_ignore_case(address, "http:") {
            ("http", rest)
        } else if let Some(rest) = strip_prefix_ignore_case(address, "https:") {
            ("https", rest)
        } else {
            return Err(NOT_HTTP);
        };

        // A lone trailing '?' is an empty query; otherwise the query starts at the first.
        let (rest, query) = match rest.strip_suffix('?') {
            Some(before) if !before.contains('?') => (before, ""),
            _ => rest.split_once('?').unwrap_or((rest, "")),
        };

        // Without "//" there is no authority: Go leaves Host empty (the rest is an opaque
        // part or a path), and NormalizeServer refuses an empty host.
        let Some(after_slashes) = rest.strip_prefix("//") else {
            return Err(NOT_HTTP);
        };
        let (authority, path) = match after_slashes.find('/') {
            Some(slash) => after_slashes.split_at(slash),
            None => (after_slashes, ""),
        };

        let host = parse_authority(authority)?;
        let escaped_path = escaped_path(path)?;
        if !fragment.is_empty() {
            unescape(fragment, Mode::Fragment)?;
        }
        if host.is_empty() {
            return Err(NOT_HTTP);
        }

        let prefix = escaped_path.trim_end_matches('/').to_owned();
        let key = format!("{scheme}://{}{prefix}", go_to_lower(&host));
        Ok(Self {
            scheme,
            host: escape(&host, Mode::Host),
            prefix,
            key,
            has_query_or_fragment: !query.is_empty() || !fragment.is_empty(),
        })
    }

    /// The key `conch login` stores this server's token under: lower-case scheme and
    /// host, the path with its case kept and trailing slashes removed, and nothing else.
    pub fn key(&self) -> &str {
        &self.key
    }

    /// Whether the address carries a query or a fragment.
    pub(crate) fn has_query_or_fragment(&self) -> bool {
        self.has_query_or_fragment
    }

    /// The path of a request for `segments` under this server, as `Client.resolve` in
    /// `internal/cli/client.go` builds it: the address's own path, then each segment
    /// escaped so that it stays one segment.
    pub(crate) fn request_path(&self, segments: &[&str]) -> String {
        let mut path = self.prefix.clone();
        for segment in segments {
            path.push('/');
            path.push_str(&escape(segment.as_bytes(), Mode::PathSegment));
        }
        path
    }

    /// Scheme, host and port, with no path and no userinfo.
    pub(crate) fn origin(&self) -> String {
        format!("{}://{}", self.scheme, self.host)
    }
}

impl fmt::Debug for ServerAddress {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_tuple("ServerAddress").field(&self.key).finish()
    }
}

/// `url.QueryEscape`: for one value in a query string.
pub(crate) fn query_escape(value: &str) -> String {
    escape(value.as_bytes(), Mode::QueryComponent)
}

fn strip_prefix_ignore_case<'a>(text: &'a str, prefix: &str) -> Option<&'a str> {
    let head = text.get(..prefix.len())?;
    head.eq_ignore_ascii_case(prefix)
        .then(|| &text[prefix.len()..])
}

/// Which part of a URL is being escaped or unescaped (`encoding` in `net/url`).
#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Path,
    PathSegment,
    Host,
    Zone,
    UserPassword,
    QueryComponent,
    Fragment,
}

/// `shouldEscape`: whether a byte must be written as `%XX` in that part of a URL.
fn should_escape(c: u8, mode: Mode) -> bool {
    if c.is_ascii_alphanumeric() {
        return false;
    }
    if matches!(mode, Mode::Host | Mode::Zone)
        && matches!(
            c,
            b'!' | b'$'
                | b'&'
                | b'\''
                | b'('
                | b')'
                | b'*'
                | b'+'
                | b','
                | b';'
                | b'='
                | b':'
                | b'['
                | b']'
                | b'<'
                | b'>'
                | b'"'
        )
    {
        return false;
    }
    match c {
        b'-' | b'_' | b'.' | b'~' => return false,
        b'$' | b'&' | b'+' | b',' | b'/' | b':' | b';' | b'=' | b'?' | b'@' => match mode {
            Mode::Path => return c == b'?',
            Mode::PathSegment => return matches!(c, b'/' | b';' | b',' | b'?'),
            Mode::UserPassword => return matches!(c, b'@' | b'/' | b'?' | b':'),
            Mode::QueryComponent => return true,
            Mode::Fragment => return false,
            Mode::Host | Mode::Zone => {}
        },
        _ => {}
    }
    if mode == Mode::Fragment && matches!(c, b'!' | b'(' | b')' | b'*') {
        return false;
    }
    true
}

fn unhex(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

/// `unescape`: turns `%XX` into the byte, refusing what Go refuses. The result is bytes:
/// an escape can produce something that is not UTF-8.
fn unescape(text: &str, mode: Mode) -> Result<Vec<u8>, Error> {
    let s = text.as_bytes();
    let mut out = Vec::with_capacity(s.len());
    let mut i = 0;
    while i < s.len() {
        if s[i] != b'%' {
            if matches!(mode, Mode::Host | Mode::Zone) && s[i] < 0x80 && should_escape(s[i], mode) {
                return Err(MALFORMED);
            }
            // '+' means a space only in a query, which is never unescaped here.
            out.push(s[i]);
            i += 1;
            continue;
        }
        let (Some(high), Some(low)) = (
            s.get(i + 1).copied().and_then(unhex),
            s.get(i + 2).copied().and_then(unhex),
        ) else {
            return Err(MALFORMED);
        };
        let escaped = &s[i..i + 3];
        let byte = high << 4 | low;
        // In a host only non-ASCII bytes may be escaped, and "%25" for a zone's percent.
        if mode == Mode::Host && high < 8 && escaped != b"%25" {
            return Err(MALFORMED);
        }
        // In a zone an escape may not smuggle in a byte a host could not hold as itself.
        if mode == Mode::Zone
            && escaped != b"%25"
            && byte != b' '
            && should_escape(byte, Mode::Host)
        {
            return Err(MALFORMED);
        }
        out.push(byte);
        i += 3;
    }
    Ok(out)
}

/// `escape`: writes every byte that must be escaped as `%XX`, upper-case. The result is
/// ASCII, because every byte above 0x7f must be escaped in every mode.
fn escape(bytes: &[u8], mode: Mode) -> String {
    const UPPER_HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut out = String::with_capacity(bytes.len());
    for &c in bytes {
        if c == b' ' && mode == Mode::QueryComponent {
            out.push('+');
        } else if should_escape(c, mode) {
            out.push('%');
            out.push(char::from(UPPER_HEX[usize::from(c >> 4)]));
            out.push(char::from(UPPER_HEX[usize::from(c & 15)]));
        } else {
            out.push(char::from(c));
        }
    }
    out
}

/// `validEncoded` for a path: whether it can be used as written.
fn valid_encoded_path(path: &str) -> bool {
    path.bytes().all(|c| {
        matches!(
            c,
            b'!' | b'$'
                | b'&'
                | b'\''
                | b'('
                | b')'
                | b'*'
                | b'+'
                | b','
                | b';'
                | b'='
                | b':'
                | b'@'
                | b'['
                | b']'
                | b'%'
        ) || !should_escape(c, Mode::Path)
    })
}

/// `setPath` then `EscapedPath`: the path as written if that is a valid encoding,
/// otherwise the default encoding of what it decodes to.
fn escaped_path(path: &str) -> Result<String, Error> {
    let decoded = unescape(path, Mode::Path)?;
    if valid_encoded_path(path) {
        Ok(path.to_owned())
    } else {
        Ok(escape(&decoded, Mode::Path))
    }
}

/// `validOptionalPort`: empty, or a colon and digits (possibly none).
fn valid_optional_port(port: &str) -> bool {
    match port.strip_prefix(':') {
        Some(digits) => digits.bytes().all(|b| b.is_ascii_digit()),
        None => port.is_empty(),
    }
}

/// `parseAuthority`: checks the userinfo as Go does and then drops it; returns the host.
fn parse_authority(authority: &str) -> Result<Vec<u8>, Error> {
    let Some(at) = authority.rfind('@') else {
        return parse_host(authority);
    };
    let host = parse_host(&authority[at + 1..])?;
    let userinfo = &authority[..at];
    // validUserinfo.
    let allowed = |c: char| c.is_ascii_alphanumeric() || "-._:~!$&'()*+,;=%@".contains(c);
    if !userinfo.chars().all(allowed) {
        return Err(MALFORMED);
    }
    let (username, password) = userinfo.split_once(':').unwrap_or((userinfo, ""));
    unescape(username, Mode::UserPassword)?;
    unescape(password, Mode::UserPassword)?;
    Ok(host)
}

/// `parseHost`: a host with an optional port, as Go's `URL.Host` holds it.
fn parse_host(host: &str) -> Result<Vec<u8>, Error> {
    if host.starts_with('[') {
        // An IP literal: "[fe80::1]", "[fe80::1%25en0]", "[fe80::1]:80".
        let close = host.rfind(']').ok_or(MALFORMED)?;
        if !valid_optional_port(&host[close + 1..]) {
            return Err(MALFORMED);
        }
        // "%25" starts a zone, which may escape more than the rest of a host may.
        if let Some(zone) = host[..close].find("%25") {
            let mut out = unescape(&host[..zone], Mode::Host)?;
            out.extend(unescape(&host[zone..close], Mode::Zone)?);
            out.extend(unescape(&host[close..], Mode::Host)?);
            return Ok(out);
        }
    } else if let Some(colon) = host.rfind(':')
        && !valid_optional_port(&host[colon..])
    {
        return Err(MALFORMED);
    }
    unescape(host, Mode::Host)
}

/// `strings.ToLower` on bytes that may not be UTF-8: each character is replaced by its
/// lower-case one, and each byte that is not part of a character by U+FFFD, as ranging
/// over a Go string does.
fn go_to_lower(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len());
    let mut rest = bytes;
    loop {
        match std::str::from_utf8(rest) {
            Ok(valid) => {
                out.extend(valid.chars().map(lower));
                return out;
            }
            Err(error) => {
                let (valid, invalid) = rest.split_at(error.valid_up_to());
                // The prefix is valid by the error's own account.
                out.extend(String::from_utf8_lossy(valid).chars().map(lower));
                out.push(char::REPLACEMENT_CHARACTER);
                rest = invalid.get(1..).unwrap_or_default();
            }
        }
    }
}

/// `unicode.ToLower`: one character to one character. Rust's `to_lowercase` gives the same
/// character for all but U+0130, which it lowers to two; Go's answer is the first of them.
fn lower(c: char) -> char {
    if newer_than_gos_tables(c) {
        return c;
    }
    c.to_lowercase().next().unwrap_or(c)
}

/// The capital letters whose lower-case form Unicode added after version 15.0, which is
/// what Go 1.25's `unicode` package holds: Go leaves them as they are. Found by running
/// every code point through `NormalizeServer` and through this module.
fn newer_than_gos_tables(c: char) -> bool {
    matches!(
        u32::from(c),
        0x1C89                  // Cyrillic capital Tje
            | 0xA7CB | 0xA7CC | 0xA7CE | 0xA7D2 | 0xA7D4 | 0xA7DA | 0xA7DC // Latin Extended-D
            | 0x10D50..=0x10D65 // Garay
            | 0x16EA0..=0x16EB8 // Beria Erfe
    )
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use serde::Deserialize;

    use super::*;

    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Vector {
        input: String,
        #[serde(default)]
        key: Option<String>,
        #[serde(default)]
        error: bool,
        #[serde(default)]
        note: String,
    }

    fn vectors() -> Vec<Vector> {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../internal/cli/testdata/credentials-vectors.json");
        let text = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
        serde_json::from_str(&text).unwrap()
    }

    #[test]
    fn the_key_matches_go_for_every_shared_vector() {
        let vectors = vectors();
        assert!(vectors.len() >= 40, "only {} vectors", vectors.len());
        let (mut keys, mut errors) = (0, 0);
        for v in &vectors {
            assert!(
                v.key.is_some() != v.error,
                "vector {:?} must have a key or error: true, not both ({})",
                v.input,
                v.note
            );
            let got = ServerAddress::parse(&v.input);
            match (&v.key, got) {
                (Some(want), Ok(address)) => {
                    assert_eq!(address.key(), want, "input {:?} ({})", v.input, v.note);
                    keys += 1;
                }
                (None, Err(Error::InvalidServer { .. })) => errors += 1,
                (Some(want), Err(e)) => {
                    panic!(
                        "input {:?} ({}): Go gives {want:?}, this gives {e}",
                        v.input, v.note
                    )
                }
                (None, Ok(address)) => panic!(
                    "input {:?} ({}): Go refuses it, this gives {:?}",
                    v.input,
                    v.note,
                    address.key()
                ),
                (None, Err(e)) => panic!("input {:?}: wrong kind of error {e:?}", v.input),
            }
        }
        assert!(keys >= 20 && errors >= 10, "{keys} keys, {errors} errors");
    }

    #[test]
    fn an_error_never_repeats_the_address() {
        for raw in [
            "ftp://user:hunter2-FAKE@host",
            "http://user:hunter2-FAKE@ho st/",
            "http://user:hunter2-FAKE@host/%zz",
            "user:hunter2-FAKE@host",
        ] {
            let error = ServerAddress::parse(raw).unwrap_err();
            let shown = format!("{error} {error:?}");
            assert!(!shown.contains("hunter2"), "{shown}");
        }
    }

    #[test]
    fn userinfo_is_dropped_from_everything_kept() {
        let address = ServerAddress::parse("http://user:hunter2-FAKE@Host:8080/Pre/").unwrap();
        assert_eq!(address.key(), "http://host:8080/Pre");
        assert_eq!(address.origin(), "http://Host:8080");
        assert_eq!(address.request_path(&["v1"]), "/Pre/v1");
        assert!(!format!("{address:?}").contains("hunter2"));
        assert_eq!(
            format!("{address:?}"),
            r#"ServerAddress("http://host:8080/Pre")"#
        );
    }

    #[test]
    fn a_query_or_fragment_is_noticed_but_is_not_part_of_the_key() {
        let table = [
            ("http://host", false),
            ("http://host/", false),
            ("http://host?", false),
            ("http://host#", false),
            ("http://host?x=1", true),
            ("http://host/?x", true),
            ("http://host#frag", true),
            ("http://host/a?b#c", true),
        ];
        for (raw, want) in table {
            let address = ServerAddress::parse(raw).unwrap();
            assert_eq!(address.has_query_or_fragment(), want, "{raw}");
            assert!(
                !address.key().contains('?') && !address.key().contains('#'),
                "{raw}"
            );
        }
    }

    #[test]
    fn request_paths_escape_each_segment_as_go_does() {
        // (channel, the segment url.PathEscape gives)
        let table = [
            ("general", "general"),
            ("war room", "war%20room"),
            ("a/b", "a%2Fb"),
            ("caf\u{e9}", "caf%C3%A9"),
            ("war room/\u{3b1}", "war%20room%2F%CE%B1"),
            ("100%", "100%25"),
            ("a?b#c", "a%3Fb%23c"),
            ("a;b,c", "a%3Bb%2Cc"),
            ("a+b&c=d:e@f$g", "a+b&c=d:e@f$g"),
            ("-_.~", "-_.~"),
            ("a\\b", "a%5Cb"),
            ("%2e%2e", "%252e%252e"),
        ];
        let root = ServerAddress::parse("http://127.0.0.1:8080").unwrap();
        let nested = ServerAddress::parse("http://127.0.0.1:8080/conch%2Fone/Two//").unwrap();
        for (channel, segment) in table {
            assert_eq!(
                root.request_path(&["v1", "channels", channel, "voice"]),
                format!("/v1/channels/{segment}/voice")
            );
            assert_eq!(
                nested.request_path(&["v1", "channels", channel, "voice"]),
                format!("/conch%2Fone/Two/v1/channels/{segment}/voice")
            );
        }
    }

    #[test]
    fn query_values_are_escaped_as_go_does() {
        let table = [
            ("general", "general"),
            ("war room", "war+room"),
            ("a/b", "a%2Fb"),
            ("war room/\u{3b1}", "war+room%2F%CE%B1"),
            ("a+b&c=d", "a%2Bb%26c%3Dd"),
            ("-_.~", "-_.~"),
            ("100%", "100%25"),
        ];
        for (value, want) in table {
            assert_eq!(query_escape(value), want);
        }
    }

    #[test]
    fn bytes_that_are_not_text_lower_as_go_ranges_over_them() {
        assert_eq!(go_to_lower(b"HOST"), "host");
        assert_eq!(go_to_lower("B\u{dc}CHER".as_bytes()), "b\u{fc}cher");
        assert_eq!(go_to_lower("\u{130}".as_bytes()), "i");
        // Letters Go 1.25's tables do not know stay as they are; their neighbours do not.
        assert_eq!(
            go_to_lower("\u{1c89}\u{a7cb}\u{10d50}\u{16ea0}".as_bytes()),
            "\u{1c89}\u{a7cb}\u{10d50}\u{16ea0}"
        );
        assert_eq!(
            go_to_lower("\u{a7c9}\u{1c90}".as_bytes()),
            "\u{a7ca}\u{10d0}"
        );
        assert_eq!(go_to_lower(b"A\xffB"), "a\u{fffd}b");
        // A truncated three-byte character is two bytes that are not text: two marks.
        assert_eq!(go_to_lower(b"\xe2\x82"), "\u{fffd}\u{fffd}");
        assert_eq!(go_to_lower(b"\xe2\x82Z\xc3"), "\u{fffd}\u{fffd}z\u{fffd}");
    }
}
