//! The crate's one error type, and what `conchd` said when it refused.
//!
//! Nothing in an [`Error`] is a secret: no token, no room name, no request header and no
//! body of a successful response. Some of it is text that `conchd` (or whatever answered in
//! its place) chose, and that text is marked as such below: it is safe to keep and to match
//! on, and it must be escaped before it is written to a terminal. The `Display` renderings
//! here already escape it.

use std::fmt;
use std::path::PathBuf;

/// The error codes the voice endpoints carry in an error document's `code`
/// (`pkg/schema/voice.go`, `ErrorCodeVoice*`), and the two general ones they share with the
/// rest of the API.
pub mod code {
    /// LiveKit is not configured on this `conchd` (503).
    pub const VOICE_NOT_CONFIGURED: &str = "voice_not_configured";
    /// LiveKit is configured but could not be reached, or the room was changing (503).
    pub const VOICE_UNAVAILABLE: &str = "voice_unavailable";
    /// `conchd` runs with authentication off; voice is never anonymous (400).
    pub const VOICE_REQUIRES_AUTH: &str = "voice_requires_auth";
    /// The caller holds no session for the channel's current room (409).
    pub const VOICE_NO_SESSION: &str = "voice_no_session";
    /// More transmit reports than the per-principal bound allows (429).
    pub const VOICE_REPORT_RATE_LIMITED: &str = "voice_report_rate_limited";
    /// No such channel, or the caller is not a member of it (404).
    pub const CHANNEL_NOT_FOUND: &str = "channel_not_found";
}

/// The most of a server-supplied `code` that is kept.
const MAX_CODE_CHARS: usize = 128;
/// The most of a server-supplied `message`, redirect target or close reason that is kept.
pub(crate) const MAX_TEXT_CHARS: usize = 512;

/// Keeps the first `max` characters of text the server supplied.
pub(crate) fn bounded(text: &str, max: usize) -> String {
    text.chars().take(max).collect()
}

/// A refusal `conchd` explained with an error document (`schema.Error`).
///
/// `code` and `message` are the server's own words, cut to a bounded length. They are not
/// secrets, but they are not this program's text either: escape them before printing. The
/// `Display` rendering does.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    /// The HTTP status of the refusal.
    pub status: u16,
    /// The error document's `code`. Empty only for a 401 that carried no error document.
    pub code: String,
    /// The error document's `message`. Server-supplied text.
    pub message: String,
}

impl Refusal {
    pub(crate) fn new(status: u16, code: &str, message: &str) -> Self {
        Self {
            status,
            code: bounded(code, MAX_CODE_CHARS),
            message: bounded(message, MAX_TEXT_CHARS),
        }
    }
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        // `{:?}` on a str escapes control characters, so a hostile message cannot
        // move the cursor or recolour the terminal.
        write!(
            f,
            "HTTP {} {:?}: {:?}",
            self.status, self.code, self.message
        )
    }
}

/// Everything that can go wrong in this crate.
///
/// Each documented refusal of each endpoint is its own variant, because the connection
/// policy in `conch-voice-control` treats them differently: some end the program, some are
/// retried, and two answers to a transmit report are neither. [`Error::status`] and
/// [`Error::code`] give the HTTP status and the server's code wherever there is one.
///
/// Nothing here was retried. One call gives one answer or one of these.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// The server address is not one this client can use. The address is not echoed: it
    /// may contain a password.
    #[error("the server address is not usable: {reason}")]
    InvalidServer {
        /// What is wrong with it.
        reason: &'static str,
    },

    /// The channel name cannot be put in a request path.
    #[error("the channel name is not usable: {reason}")]
    InvalidChannel {
        /// What is wrong with it.
        reason: &'static str,
    },

    /// The login token is empty or cannot be sent in an HTTP header.
    #[error("the login token is empty or cannot be sent in an HTTP header")]
    InvalidToken,

    /// There is no login for this server: no `CONCH_TOKEN`, and no credentials file or no
    /// entry in it. Not a failure to read anything.
    #[error("not signed in to {server}: run 'conch login'")]
    NotSignedIn {
        /// The normalised server address the login was looked up under.
        server: String,
    },

    /// The credentials file can be read or written by other users. Refused, as `conch`
    /// refuses it.
    #[error(
        "credentials file {path} is accessible by other users (mode {mode:04o}); run 'chmod 600 {path}'",
        path = .path.display()
    )]
    CredentialsExposed {
        /// The file.
        path: PathBuf,
        /// Its permission bits.
        mode: u32,
    },

    /// The credentials file exists and could not be read.
    #[error("credentials file {path} could not be read: {source}", path = .path.display())]
    CredentialsUnreadable {
        /// The file.
        path: PathBuf,
        /// Why.
        #[source]
        source: std::io::Error,
    },

    /// The credentials file is not the JSON `conch login` writes. Only a position is
    /// given: the text around it may be a token.
    #[error("credentials file {path} is not valid: {detail}", path = .path.display())]
    CredentialsInvalid {
        /// The file.
        path: PathBuf,
        /// Where it stopped making sense.
        detail: String,
    },

    /// 401: no credential was accepted. The token is wrong, revoked or expired.
    #[error("conchd did not accept the login ({0}): run 'conch login'")]
    Unauthenticated(Refusal),

    /// 400 `voice_requires_auth`: `conchd` runs with authentication off.
    #[error("conchd runs with authentication off, and voice is never anonymous ({0})")]
    VoiceRequiresAuth(Refusal),

    /// 403: the caller is not a person. Agents have no voice.
    #[error("conchd does not allow this caller to use voice ({0})")]
    Forbidden(Refusal),

    /// 404 `channel_not_found`: no such channel, or the caller is not a member of it.
    #[error("no such channel, or not a member of it ({0})")]
    ChannelNotFound(Refusal),

    /// 409 `voice_no_session`: the caller holds no session for the channel's current room.
    /// The room was rotated or the session is otherwise gone. Not retried: go back for a
    /// session.
    #[error("conchd holds no voice session for this caller in the channel's room ({0})")]
    NoSession(Refusal),

    /// 429 `voice_report_rate_limited`: too many transmit reports. Not retried.
    #[error("too many transmit reports ({0})")]
    RateLimited(Refusal),

    /// 503 `voice_not_configured`: this `conchd` has no LiveKit.
    #[error("voice is not configured on this conchd ({0})")]
    NotConfigured(Refusal),

    /// 503 `voice_unavailable`: LiveKit could not be reached, or the room was changing.
    #[error("voice is temporarily unavailable ({0})")]
    Unavailable(Refusal),

    /// Any other refusal that came with an error document. The status says whether it is
    /// permanent (4xx) or passing (5xx).
    #[error("conchd refused the request ({0})")]
    Refused(Refusal),

    /// A 3xx. Redirects are never followed: the login token goes only where the user
    /// pointed the server address.
    #[error(
        "conchd redirected the request (HTTP {status}) to {location:?}; set the server address to the final URL"
    )]
    Redirected {
        /// The HTTP status.
        status: u16,
        /// The `Location` header, cut to a bounded length. Server-supplied text.
        location: String,
    },

    /// An answer `conchd` does not give: a status outside 2xx whose body is not an error
    /// document (empty, not JSON, too large, or JSON of another shape), or a success other
    /// than 204 to a transmit report. Typical of a proxy in front of `conchd`.
    #[error("conchd answered HTTP {status}, which is not an answer of this endpoint")]
    UnexpectedResponse {
        /// The HTTP status.
        status: u16,
    },

    /// The presence socket's address answered with a success that is not an upgrade.
    #[error("conchd answered HTTP {status} where the presence socket should have opened")]
    NotUpgraded {
        /// The HTTP status.
        status: u16,
    },

    /// No answer within the client's timeout.
    #[error("conchd did not answer within the timeout")]
    Timeout,

    /// No connection could be made: refused, unreachable, no such host, or TLS failed.
    #[error("could not connect to conchd: {detail}")]
    Connect {
        /// The causes, outermost first. No URL and no header.
        detail: String,
    },

    /// The connection was made and then failed before the answer was complete.
    #[error("the connection to conchd failed: {detail}")]
    Transport {
        /// The causes, outermost first. No URL and no header.
        detail: String,
    },

    /// The HTTP client itself could not be set up (for example, no usable TLS roots).
    #[error("the HTTP client could not be set up: {detail}")]
    Setup {
        /// The causes, outermost first.
        detail: String,
    },

    /// A body or frame that should have been a document is not JSON of that shape. For a
    /// document that carries secrets `detail` is only a position.
    #[error("conchd's {what} could not be decoded: {detail}")]
    Undecodable {
        /// Which document.
        what: &'static str,
        /// Where and, for a document with no secrets in it, why. May quote the document:
        /// server-supplied text.
        detail: String,
    },

    /// A document decoded and then failed the checks `pkg/schema` makes of it.
    #[error("{what} is not valid: {problem}")]
    Invalid {
        /// Which document.
        what: &'static str,
        /// The rule it breaks. Holds numbers and field names only, never a field's text.
        problem: String,
    },

    /// The presence socket sent a frame that is not text. Presence is JSON text frames and
    /// nothing else.
    #[error("the presence socket sent a {kind} frame where a JSON text frame was expected")]
    UnexpectedFrame {
        /// The kind of frame.
        kind: &'static str,
    },

    /// The presence socket broke the WebSocket protocol, or sent more than this client
    /// accepts in one frame.
    #[error("the presence socket broke the WebSocket protocol: {detail}")]
    SocketProtocol {
        /// What it did.
        detail: String,
    },
}

impl Error {
    /// The refusal `conchd` explained, if this is one.
    pub fn refusal(&self) -> Option<&Refusal> {
        match self {
            Self::Unauthenticated(r)
            | Self::VoiceRequiresAuth(r)
            | Self::Forbidden(r)
            | Self::ChannelNotFound(r)
            | Self::NoSession(r)
            | Self::RateLimited(r)
            | Self::NotConfigured(r)
            | Self::Unavailable(r)
            | Self::Refused(r) => Some(r),
            _ => None,
        }
    }

    /// The HTTP status, for every error that came from an HTTP answer.
    pub fn status(&self) -> Option<u16> {
        match self {
            Self::Redirected { status, .. }
            | Self::UnexpectedResponse { status }
            | Self::NotUpgraded { status } => Some(*status),
            other => other.refusal().map(|r| r.status),
        }
    }

    /// The server's error code, for every error that came with an error document.
    pub fn code(&self) -> Option<&str> {
        self.refusal()
            .map(|r| r.code.as_str())
            .filter(|code| !code.is_empty())
    }

    /// Sorts a refusal that came with an error document into its variant. A known code
    /// under a status it does not belong to is not that refusal: it stays [`Error::Refused`],
    /// which still carries both.
    pub(crate) fn from_refusal(refusal: Refusal) -> Self {
        match (refusal.status, refusal.code.as_str()) {
            (401, _) => Self::Unauthenticated(refusal),
            (400, code::VOICE_REQUIRES_AUTH) => Self::VoiceRequiresAuth(refusal),
            (403, _) => Self::Forbidden(refusal),
            (404, code::CHANNEL_NOT_FOUND) => Self::ChannelNotFound(refusal),
            (409, code::VOICE_NO_SESSION) => Self::NoSession(refusal),
            (429, code::VOICE_REPORT_RATE_LIMITED) => Self::RateLimited(refusal),
            (503, code::VOICE_NOT_CONFIGURED) => Self::NotConfigured(refusal),
            (503, code::VOICE_UNAVAILABLE) => Self::Unavailable(refusal),
            _ => Self::Refused(refusal),
        }
    }
}

/// An error and its causes on one line, outermost first.
pub(crate) fn causes(error: &dyn std::error::Error) -> String {
    let mut line = error.to_string();
    let mut source = error.source();
    while let Some(cause) = source {
        line.push_str(": ");
        line.push_str(&cause.to_string());
        source = cause.source();
    }
    line
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn each_documented_refusal_has_its_own_variant() {
        // (status, code, the variant's name)
        let table = [
            (401, "unauthenticated", "Unauthenticated"),
            (400, code::VOICE_REQUIRES_AUTH, "VoiceRequiresAuth"),
            (403, "forbidden", "Forbidden"),
            (404, code::CHANNEL_NOT_FOUND, "ChannelNotFound"),
            (409, code::VOICE_NO_SESSION, "NoSession"),
            (429, code::VOICE_REPORT_RATE_LIMITED, "RateLimited"),
            (503, code::VOICE_NOT_CONFIGURED, "NotConfigured"),
            (503, code::VOICE_UNAVAILABLE, "Unavailable"),
            (500, "internal_error", "Refused"),
            (400, "invalid_request", "Refused"),
        ];
        let mut seen = std::collections::BTreeSet::new();
        for (status, code, variant) in table {
            let error = Error::from_refusal(Refusal::new(status, code, "why"));
            let name = format!("{error:?}");
            assert!(
                name.starts_with(&format!("{variant}(")),
                "{status} {code} became {name}"
            );
            assert_eq!(error.status(), Some(status));
            assert_eq!(error.code(), Some(code));
            seen.insert(variant);
        }
        assert_eq!(seen.len(), 9, "eight named refusals and the general one");
    }

    #[test]
    fn a_known_code_under_the_wrong_status_is_not_that_refusal() {
        for (status, code) in [
            (500, code::VOICE_UNAVAILABLE),
            (404, code::VOICE_NO_SESSION),
            (400, code::CHANNEL_NOT_FOUND),
            (503, code::VOICE_REPORT_RATE_LIMITED),
        ] {
            let error = Error::from_refusal(Refusal::new(status, code, ""));
            assert!(matches!(error, Error::Refused(_)), "{status} {code}");
            assert_eq!(error.status(), Some(status));
            assert_eq!(error.code(), Some(code));
        }
    }

    #[test]
    fn display_escapes_what_the_server_supplied() {
        let hostile = "\u{1b}[2J\u{1b}[31mowned\r\n";
        let error = Error::from_refusal(Refusal::new(503, "voice_unavailable", hostile));
        let shown = error.to_string();
        assert!(!shown.contains('\u{1b}'), "{shown:?}");
        assert!(!shown.contains('\n') && !shown.contains('\r'), "{shown:?}");
        assert!(shown.contains("503") && shown.contains("voice_unavailable"));

        let redirected = Error::Redirected {
            status: 302,
            location: hostile.to_owned(),
        };
        assert!(!redirected.to_string().contains('\u{1b}'));
    }

    #[test]
    fn server_text_is_kept_to_a_bounded_length() {
        let refusal = Refusal::new(500, &"c".repeat(10_000), &"m".repeat(10_000));
        assert_eq!(refusal.code.len(), MAX_CODE_CHARS);
        assert_eq!(refusal.message.len(), MAX_TEXT_CHARS);
        // Cut on a character boundary, not in the middle of one.
        assert_eq!(
            bounded(&"é".repeat(600), MAX_TEXT_CHARS).chars().count(),
            512
        );
    }

    #[test]
    fn the_exposed_file_message_names_the_file_and_the_fix() {
        let error = Error::CredentialsExposed {
            path: PathBuf::from("/tmp/x/conch/credentials.json"),
            mode: 0o644,
        };
        assert_eq!(
            error.to_string(),
            "credentials file /tmp/x/conch/credentials.json is accessible by other users \
             (mode 0644); run 'chmod 600 /tmp/x/conch/credentials.json'"
        );
    }
}
