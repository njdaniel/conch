//! The three REST calls voice needs, and what `conchd` answers them with.
//!
//! The reference is `internal/cli/client.go`. Like it, this client sends the login token
//! as a bearer credential in the `Authorization` header and nowhere else, builds each
//! request path by escaping every segment, and never follows a redirect, so the token goes
//! only where the user pointed the server address.
//!
//! One call gives one answer or one [`Error`]. Nothing here retries: when to try again is
//! the connection policy's decision (`conch-voice-control`), and for a transmit report a
//! second delivery would be a second audit row. reqwest's own retries are switched off.
//! What remains underneath is not a retry of anything delivered: a request that could not
//! be written at all, because the kept-alive connection it was given had already been
//! closed, is written to a new connection.

use std::fmt;
use std::time::Duration;

use reqwest::StatusCode;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderValue, LOCATION};
use serde::de::DeserializeOwned;

use crate::error::{Error, MAX_TEXT_CHARS, Refusal, bounded, causes};
use crate::server::{ServerAddress, query_escape};
use crate::types::{
    Audience, ErrorBody, Secret, VoicePresenceV1, VoiceSessionResponseV1, VoiceTransmitReportV1,
    VoiceTransmitState,
};

/// The most of any response body that is read: the Go client's bound on an error body,
/// applied here to every body. A session or a presence snapshot is a few kilobytes.
const MAX_BODY_BYTES: usize = 1 << 20;

/// TCP keep-alive on every connection, the presence socket's included: probing starts
/// after this long with nothing received, repeats this often, and gives up after this
/// many unanswered probes. It is what turns a link that died without a word into an
/// error, after about a minute, instead of a socket that waits for ever. These are
/// reqwest's own defaults, written out so that an upgrade of reqwest cannot change them
/// unnoticed.
const KEEPALIVE_IDLE: Duration = Duration::from_secs(15);
const KEEPALIVE_INTERVAL: Duration = Duration::from_secs(15);
const KEEPALIVE_PROBES: u32 = 3;

/// Whether decoding errors of a document may say what they found. For a document that
/// carries secrets they may not: serde's message can quote a value.
#[derive(Clone, Copy)]
pub(crate) enum Detail {
    /// The decoder's own message.
    Full,
    /// Only the kind of problem and its position.
    PositionOnly,
}

/// A client of one `conchd`, signed in as one principal.
///
/// Cheap to clone; clones share connections. Its `Debug` shows the server and the timeout
/// and never the token.
#[derive(Clone)]
pub struct Client {
    http: reqwest::Client,
    server: ServerAddress,
    /// `Bearer <token>`, marked sensitive so that nothing underneath prints it.
    authorization: HeaderValue,
    timeout: Duration,
}

impl Client {
    /// A client for `server` that signs in with `token` and gives every request `timeout`
    /// to be answered in full.
    ///
    /// The address must have no query and no fragment, as for `conch`. Proxy settings in
    /// the environment are not used: the presence socket could not follow them, and a
    /// client that reached `conchd` two different ways would be harder to reason about
    /// than one that reaches it directly.
    pub fn new(server: &ServerAddress, token: &Secret, timeout: Duration) -> Result<Self, Error> {
        if server.has_query_or_fragment() {
            return Err(Error::InvalidServer {
                reason: "it must not contain a query or a fragment",
            });
        }
        if token.is_empty() {
            return Err(Error::InvalidToken);
        }
        let mut authorization = HeaderValue::from_str(&format!("Bearer {}", token.expose()))
            .map_err(|_| Error::InvalidToken)?;
        authorization.set_sensitive(true);

        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .no_proxy()
            .timeout(timeout)
            .tcp_keepalive(KEEPALIVE_IDLE)
            .tcp_keepalive_interval(KEEPALIVE_INTERVAL)
            .tcp_keepalive_retries(KEEPALIVE_PROBES)
            .user_agent(concat!("conch-voice/", env!("CARGO_PKG_VERSION")))
            .build()
            .map_err(|e| Error::Setup {
                detail: causes(&e.without_url()),
            })?;
        let client = Self {
            http,
            server: server.clone(),
            authorization,
            timeout,
        };
        // An address reqwest cannot use is refused now, not at the first call.
        client
            .url(&["v1"], None)
            .map_err(|_| Error::InvalidServer {
                reason: "its host or path cannot be used in a request",
            })?;
        Ok(client)
    }

    /// The key of the server this client talks to ([`ServerAddress::key`]).
    pub fn server(&self) -> &str {
        self.server.key()
    }

    /// The time every request is given.
    pub fn timeout(&self) -> Duration {
        self.timeout
    }

    /// `POST /v1/channels/{channel}/voice/session`: asks for a voice session, and returns
    /// it only if it is one a client can connect with (see
    /// [`VoiceSessionResponseV1::validate`]).
    ///
    /// Ask immediately before every connection attempt: the tokens are short-lived on
    /// purpose. Refusals: [`Error::Unauthenticated`], [`Error::VoiceRequiresAuth`],
    /// [`Error::Forbidden`], [`Error::ChannelNotFound`], [`Error::NotConfigured`],
    /// [`Error::Unavailable`].
    pub async fn session(&self, channel: &str) -> Result<VoiceSessionResponseV1, Error> {
        Self::check_channel(channel)?;
        let url = self.url(&["v1", "channels", channel, "voice", "session"], None)?;
        let response = self.send(self.http.post(url)).await?;
        // The body holds join tokens and room names: an error about it says where, not what.
        let session: VoiceSessionResponseV1 = self
            .document(response, "voice session", Detail::PositionOnly)
            .await?;
        session.validate()?;
        Ok(session)
    }

    /// `POST /v1/channels/{channel}/voice/transmit`: reports that the caller's
    /// transmission to `audience` (`None` is the whole channel) started or stopped.
    ///
    /// Success is exactly 204: any other answer, another 2xx included, is an error, so a
    /// report is never taken as recorded on the word of something that is not `conchd`.
    ///
    /// Send reports one at a time, each after the answer to the one before: `conchd` takes
    /// them in arrival order. Refusals: those of [`Client::session`] except the two 503s,
    /// and [`Error::NoSession`] and [`Error::RateLimited`], neither of which is to be
    /// retried.
    pub async fn transmit(
        &self,
        channel: &str,
        state: VoiceTransmitState,
        audience: Option<&Audience>,
    ) -> Result<(), Error> {
        Self::check_channel(channel)?;
        let report = VoiceTransmitReportV1 {
            state,
            audience: audience.cloned(),
        };
        report.validate()?;
        let body = serde_json::to_vec(&report).map_err(|_| Error::Invalid {
            what: "voice transmit report",
            problem: "it could not be encoded".into(),
        })?;
        let url = self.url(&["v1", "channels", channel, "voice", "transmit"], None)?;
        let request = self
            .http
            .post(url)
            .header(CONTENT_TYPE, "application/json")
            .body(body);
        let response = self.send(request).await?;
        match response.status() {
            StatusCode::NO_CONTENT => Ok(()),
            status if status.is_success() => Err(Error::UnexpectedResponse {
                status: status.as_u16(),
            }),
            _ => Err(self.refusal(response).await),
        }
    }

    /// `GET /v1/channels/{channel}/voice`: one snapshot of who is connected and who is
    /// talking. A snapshot that fails [`VoicePresenceV1::validate`] is an error: presence
    /// is read to be believed.
    ///
    /// Refusals: [`Error::Unauthenticated`], [`Error::VoiceRequiresAuth`],
    /// [`Error::Forbidden`], [`Error::ChannelNotFound`]. A `conchd` with no LiveKit
    /// answers with a snapshot that says so, not with a refusal.
    pub async fn presence(&self, channel: &str) -> Result<VoicePresenceV1, Error> {
        Self::check_channel(channel)?;
        let url = self.url(&["v1", "channels", channel, "voice"], None)?;
        let response = self.send(self.http.get(url)).await?;
        let presence: VoicePresenceV1 = self
            .document(response, "voice presence", Detail::Full)
            .await?;
        presence.validate()?;
        Ok(presence)
    }

    /// Refuses a channel name that cannot be one segment of a request path. Applied to
    /// the presence socket too, though it takes the name in its query: a channel the
    /// REST calls cannot name is not one this client can join.
    pub(crate) fn check_channel(channel: &str) -> Result<(), Error> {
        match channel {
            "" => Err(Error::InvalidChannel {
                reason: "it is empty",
            }),
            "." | ".." => Err(Error::InvalidChannel {
                reason: "'.' and '..' cannot be put in a request path",
            }),
            _ => Ok(()),
        }
    }

    /// The address of a request: the server's own path, then `segments`, each escaped to
    /// stay one segment, then an optional `name=value` query.
    ///
    /// The address is built as Go builds it and then handed to reqwest, whose parser may
    /// read a path differently (it folds `.` and `..`). If what reqwest would request is
    /// not what was built, the request is refused instead of sent somewhere else.
    pub(crate) fn url(
        &self,
        segments: &[&str],
        query: Option<(&str, &str)>,
    ) -> Result<reqwest::Url, Error> {
        let unusable = Error::InvalidChannel {
            reason: "the request address built from it would not reach the endpoint",
        };
        let path = self.server.request_path(segments);
        let query = query.map(|(name, value)| format!("{name}={}", query_escape(value)));
        let mut text = self.server.origin();
        text.push_str(&path);
        if let Some(query) = &query {
            text.push('?');
            text.push_str(query);
        }
        let Ok(url) = reqwest::Url::parse(&text) else {
            return Err(unusable);
        };
        let same = url.path() == path
            && url.query() == query.as_deref()
            && url.fragment().is_none()
            && url.username().is_empty()
            && url.password().is_none();
        if same { Ok(url) } else { Err(unusable) }
    }

    /// A GET to build on, for the presence socket's upgrade.
    pub(crate) fn http_get(&self, url: reqwest::Url) -> reqwest::RequestBuilder {
        self.http.get(url)
    }

    /// Sends one request with the login, once.
    pub(crate) async fn send(
        &self,
        request: reqwest::RequestBuilder,
    ) -> Result<reqwest::Response, Error> {
        request
            .header(AUTHORIZATION, self.authorization.clone())
            .send()
            .await
            .map_err(transport)
    }

    /// The document a successful response carries, or the refusal an unsuccessful one is.
    async fn document<T: DeserializeOwned>(
        &self,
        response: reqwest::Response,
        what: &'static str,
        detail: Detail,
    ) -> Result<T, Error> {
        if !response.status().is_success() {
            return Err(self.refusal(response).await);
        }
        let Some(body) = read_body(response).await? else {
            return Err(Error::Undecodable {
                what,
                detail: "it is larger than 1 MiB".into(),
            });
        };
        decode(&body, what, detail)
    }

    /// What an unsuccessful response means. Reads the body, which is an error document
    /// when `conchd` itself answered.
    pub(crate) async fn refusal(&self, response: reqwest::Response) -> Error {
        let status = response.status().as_u16();
        if response.status().is_redirection() {
            let location = response
                .headers()
                .get(LOCATION)
                .map(|value| String::from_utf8_lossy(value.as_bytes()).into_owned())
                .unwrap_or_default();
            return Error::Redirected {
                status,
                location: bounded(&location, MAX_TEXT_CHARS),
            };
        }
        let body = match read_body(response).await {
            Ok(body) => body,
            // The status arrived and the body did not: the failure is the more useful
            // thing to report, except for a 401, which means the same with no body.
            Err(error) if status != 401 => return error,
            Err(_) => None,
        };
        let document = body
            .and_then(|body| serde_json::from_slice::<ErrorBody>(&body).ok())
            .filter(|document| !document.code.is_empty());
        match document {
            Some(document) => {
                Error::from_refusal(Refusal::new(status, &document.code, &document.message))
            }
            // As in the Go client, a 401 is "not signed in" whatever its body.
            None if status == 401 => Error::Unauthenticated(Refusal::new(status, "", "")),
            None => Error::UnexpectedResponse { status },
        }
    }
}

impl fmt::Debug for Client {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Client")
            .field("server", &self.server.key())
            .field("timeout", &self.timeout)
            .finish_non_exhaustive()
    }
}

/// Decodes one JSON document, keeping out of the error whatever `detail` says to.
pub(crate) fn decode<T: DeserializeOwned>(
    body: &[u8],
    what: &'static str,
    detail: Detail,
) -> Result<T, Error> {
    serde_json::from_slice(body).map_err(|e| Error::Undecodable {
        what,
        detail: match detail {
            Detail::Full => e.to_string(),
            Detail::PositionOnly => {
                let kind = match e.classify() {
                    serde_json::error::Category::Data => "it is JSON of another shape",
                    serde_json::error::Category::Eof => "it ends early",
                    _ => "it is not JSON",
                };
                format!("{kind}, at line {} column {}", e.line(), e.column())
            }
        },
    })
}

/// Reads a body up to the bound. `None` means it is larger.
async fn read_body(mut response: reqwest::Response) -> Result<Option<Vec<u8>>, Error> {
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(transport)? {
        if body.len() + chunk.len() > MAX_BODY_BYTES {
            return Ok(None);
        }
        body.extend_from_slice(&chunk);
    }
    Ok(Some(body))
}

/// A failure to exchange a request and its response, sorted the way the connection policy
/// needs it. The request's address is taken out first; no header is ever part of it.
pub(crate) fn transport(error: reqwest::Error) -> Error {
    let error = error.without_url();
    if error.is_timeout() {
        Error::Timeout
    } else if error.is_connect() {
        Error::Connect {
            detail: causes(&error),
        }
    } else {
        Error::Transport {
            detail: causes(&error),
        }
    }
}
