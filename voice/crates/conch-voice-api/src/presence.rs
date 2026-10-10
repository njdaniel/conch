//! The presence socket: `GET /v1/voice/ws?channel=`, which sends the channel's whole voice
//! state as a `conch.voice_presence.v1` document on connect and again on every change.
//!
//! The reference is `SubscribeVoicePresence` in `internal/cli/client.go`.
//!
//! **The opening handshake is made by reqwest, not by tungstenite.** tungstenite's own
//! client writes the whole upgrade request, `Authorization` header included, to the `log`
//! facade at trace level (`handshake/client.rs`: `trace!("Request: {:?}", ...)`), and the
//! binary installs a logger. Going through reqwest keeps the login token out of that code
//! altogether; reqwest marks the header sensitive and does not log requests. It also means
//! the socket shares the REST calls' TLS configuration, timeout, no-redirect rule and, for
//! a refusal before the upgrade, the same decoding of the error document: tungstenite
//! would hand back only as much of that body as arrived with the headers. tungstenite is
//! used for what follows the upgrade: the frames.

use std::fmt;
use std::time::Duration;

use futures_util::StreamExt;
use reqwest::StatusCode;
use reqwest::header::{
    CONNECTION, HeaderMap, SEC_WEBSOCKET_ACCEPT, SEC_WEBSOCKET_KEY, SEC_WEBSOCKET_VERSION, UPGRADE,
};
use tokio_tungstenite::WebSocketStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::tungstenite::error::{Error as WsError, ProtocolError};
use tokio_tungstenite::tungstenite::handshake::client::generate_key;
use tokio_tungstenite::tungstenite::handshake::derive_accept_key;
use tokio_tungstenite::tungstenite::protocol::{CloseFrame, Role, WebSocketConfig};

use crate::client::{Client, Detail, decode, transport};
use crate::error::{Error, MAX_TEXT_CHARS, bounded};
use crate::types::VoicePresenceV1;

/// The most one presence frame may hold. A snapshot is a few kilobytes; tungstenite's own
/// default would buffer 64 MiB on a server's say-so.
const MAX_FRAME_BYTES: usize = 1 << 20;

/// How long the closing handshake is given once the server has said why it is closing.
const CLOSE_GRACE: Duration = Duration::from_secs(1);

/// WebSocket close codes this client tells apart (RFC 6455 §7.4.1).
const CLOSE_GOING_AWAY: u16 = 1001;
const CLOSE_POLICY_VIOLATION: u16 = 1008;
/// What RFC 6455 says to report for a close frame that carried no code.
const CLOSE_NO_STATUS: u16 = 1005;

/// Why a presence stream ended. The connection policy treats these differently, which is
/// why they are told apart.
///
/// `reason` is the text of the server's close frame, cut to a bounded length. It is the
/// server's text: escape it before printing, and do not build behaviour on its wording.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum StreamEnd {
    /// `conchd` is shutting down (close code 1001). It may be back.
    ServerGoingAway {
        /// The close frame's text.
        reason: String,
    },
    /// `conchd` ended the subscription on purpose (close code 1008): the caller was
    /// removed from the channel, the credential was revoked or the principal disabled, or
    /// the subscriber fell too far behind. Asking for a session says which: a caller who
    /// may no longer be here is refused.
    PolicyViolation {
        /// The close frame's text.
        reason: String,
    },
    /// Any other close frame: a normal closure (1000), an internal error (1011), or a
    /// frame with no code at all (reported as 1005).
    Closed {
        /// The close code.
        code: u16,
        /// The close frame's text.
        reason: String,
    },
    /// The connection ended with no close frame: a network drop, or the server gone
    /// without a word.
    Dropped,
    /// This client stopped reading after a frame it could not accept. The call that read
    /// that frame returned the error.
    Abandoned,
}

/// What a presence stream yields.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PresenceEvent {
    /// The channel's whole voice state. It replaces the one before it.
    Snapshot(VoicePresenceV1),
    /// The stream is over, and why. Every later call gives the same answer.
    Ended(StreamEnd),
}

/// A presence socket being read.
///
/// Dropping it closes the connection. Nothing reconnects it: when to subscribe again is
/// the caller's decision.
pub struct PresenceStream {
    socket: Option<WebSocketStream<reqwest::Upgraded>>,
    end: Option<StreamEnd>,
}

impl Client {
    /// `GET /v1/voice/ws?channel=`: follows the channel's voice presence.
    ///
    /// The first [`PresenceStream::next`] gives the state at connection; `conchd` then
    /// sends a snapshot only when something changes, so a quiet channel is a quiet
    /// socket. The client's timeout bounds the opening of the socket, not its life.
    ///
    /// Refusals before the socket opens are those of [`Client::presence`]:
    /// [`Error::Unauthenticated`], [`Error::VoiceRequiresAuth`], [`Error::Forbidden`],
    /// [`Error::ChannelNotFound`].
    pub async fn presence_stream(&self, channel: &str) -> Result<PresenceStream, Error> {
        Self::check_channel(channel)?;
        let url = self.url(&["v1", "voice", "ws"], Some(("channel", channel)))?;
        let key = generate_key();
        let request = self
            .http_get(url)
            .header(CONNECTION, "Upgrade")
            .header(UPGRADE, "websocket")
            .header(SEC_WEBSOCKET_VERSION, "13")
            .header(SEC_WEBSOCKET_KEY, &key);
        let response = self.send(request).await?;

        let status = response.status();
        if status != StatusCode::SWITCHING_PROTOCOLS {
            if status.is_success() {
                return Err(Error::NotUpgraded {
                    status: status.as_u16(),
                });
            }
            return Err(self.refusal(response).await);
        }
        check_upgrade(response.headers(), &key)?;

        // The 101 is in hand, so this only takes the connection over; it is bounded all
        // the same so that no await here is open-ended.
        let upgraded = tokio::time::timeout(self.timeout(), response.upgrade())
            .await
            .map_err(|_| Error::Timeout)?
            .map_err(transport)?;
        let config = WebSocketConfig::default()
            .max_message_size(Some(MAX_FRAME_BYTES))
            .max_frame_size(Some(MAX_FRAME_BYTES));
        let socket = WebSocketStream::from_raw_socket(upgraded, Role::Client, Some(config)).await;
        Ok(PresenceStream {
            socket: Some(socket),
            end: None,
        })
    }
}

/// Checks that a 101 answers this upgrade request, as RFC 6455 §4.1 requires of a client.
fn check_upgrade(headers: &HeaderMap, key: &str) -> Result<(), Error> {
    let problem = |detail: &str| Error::SocketProtocol {
        detail: detail.to_owned(),
    };
    let has_token = |name, token: &str| {
        headers.get_all(name).iter().any(|value| {
            value.to_str().is_ok_and(|v| {
                v.split(',')
                    .any(|part| part.trim().eq_ignore_ascii_case(token))
            })
        })
    };
    if !has_token(UPGRADE, "websocket") {
        return Err(problem("the upgrade answer is not for a WebSocket"));
    }
    if !has_token(CONNECTION, "upgrade") {
        return Err(problem("the upgrade answer does not switch the connection"));
    }
    let accept = headers
        .get(SEC_WEBSOCKET_ACCEPT)
        .map(|value| value.as_bytes());
    if accept != Some(derive_accept_key(key.as_bytes()).as_bytes()) {
        return Err(problem(
            "the upgrade answer does not match the request's key",
        ));
    }
    Ok(())
}

impl PresenceStream {
    /// The next thing the socket has to say.
    ///
    /// - `Ok(Snapshot)`: a document that decoded and passed
    ///   [`VoicePresenceV1::validate`]. Documents come in the order `conchd` sent them and
    ///   none is skipped.
    /// - `Ok(Ended)`: the stream is over, with the reason. It stays over.
    /// - `Err`: a frame this client could not accept: not text, not JSON of the presence
    ///   shape, a document that is not valid, or a breach of the protocol. Such a frame is
    ///   never passed over in silence. The stream is abandoned: the next call gives
    ///   [`StreamEnd::Abandoned`].
    ///
    /// Waits as long as the channel is quiet. A link that dies without a word ends the
    /// wait too, as [`StreamEnd::Dropped`], once TCP keep-alive has given up on it: about
    /// a minute (see `KEEPALIVE_IDLE` in `client.rs`). Dropping the future before it
    /// finishes loses nothing: a frame is either returned or still to be read.
    pub async fn next(&mut self) -> Result<PresenceEvent, Error> {
        // A close frame was read by a call that was dropped before it returned.
        if let (Some(end), true) = (self.end.clone(), self.socket.is_some()) {
            return Ok(self.finish(end));
        }
        loop {
            let Some(socket) = self.socket.as_mut() else {
                let end = self.end.clone().unwrap_or(StreamEnd::Abandoned);
                return Ok(PresenceEvent::Ended(end));
            };
            let failure = match socket.next().await {
                Some(Ok(Message::Text(text))) => match snapshot(text.as_bytes()) {
                    Ok(presence) => return Ok(PresenceEvent::Snapshot(presence)),
                    Err(error) => error,
                },
                // tungstenite answers a ping by itself; neither carries anything to pass on.
                Some(Ok(Message::Ping(_) | Message::Pong(_))) => continue,
                Some(Ok(Message::Close(frame))) => {
                    // Kept before anything else is awaited: if this call is dropped while
                    // the handshake finishes, the next one still knows why it ended.
                    let end = close_reason(frame);
                    self.end = Some(end.clone());
                    // Let the closing handshake finish, so the server is not left waiting
                    // for an answer to its close frame.
                    let _ = tokio::time::timeout(CLOSE_GRACE, async {
                        while let Some(Ok(_)) = socket.next().await {}
                    })
                    .await;
                    return Ok(self.finish(end));
                }
                Some(Ok(Message::Binary(_))) => Error::UnexpectedFrame { kind: "binary" },
                Some(Ok(Message::Frame(_))) => Error::UnexpectedFrame { kind: "raw" },
                // The connection went away underneath, and nobody said why.
                None
                | Some(Err(
                    WsError::ConnectionClosed
                    | WsError::AlreadyClosed
                    | WsError::Io(_)
                    | WsError::Protocol(ProtocolError::ResetWithoutClosingHandshake),
                )) => return Ok(self.finish(StreamEnd::Dropped)),
                Some(Err(error)) => Error::SocketProtocol {
                    detail: error.to_string(),
                },
            };
            self.finish(StreamEnd::Abandoned);
            return Err(failure);
        }
    }

    /// Why the stream ended, once it has.
    pub fn ended(&self) -> Option<&StreamEnd> {
        self.end.as_ref()
    }

    fn finish(&mut self, end: StreamEnd) -> PresenceEvent {
        self.socket = None;
        self.end = Some(end.clone());
        PresenceEvent::Ended(end)
    }
}

impl fmt::Debug for PresenceStream {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PresenceStream")
            .field("open", &self.socket.is_some())
            .field("end", &self.end)
            .finish()
    }
}

/// One frame's text as a presence document that may be believed.
fn snapshot(text: &[u8]) -> Result<VoicePresenceV1, Error> {
    // Presence carries no token and no room name, so its decoding errors may say why.
    let presence: VoicePresenceV1 = decode(text, "voice presence frame", Detail::Full)?;
    presence.validate()?;
    Ok(presence)
}

fn close_reason(frame: Option<CloseFrame>) -> StreamEnd {
    let (code, reason) = match frame {
        Some(frame) => (
            u16::from(frame.code),
            bounded(frame.reason.as_str(), MAX_TEXT_CHARS),
        ),
        None => (CLOSE_NO_STATUS, String::new()),
    };
    match code {
        CLOSE_GOING_AWAY => StreamEnd::ServerGoingAway { reason },
        CLOSE_POLICY_VIOLATION => StreamEnd::PolicyViolation { reason },
        code => StreamEnd::Closed { code, reason },
    }
}

#[cfg(test)]
mod tests {
    use reqwest::header::HeaderValue;

    use super::*;

    fn headers(pairs: &[(&'static str, &str)]) -> HeaderMap {
        let mut map = HeaderMap::new();
        for (name, value) in pairs {
            map.append(*name, HeaderValue::from_str(value).unwrap());
        }
        map
    }

    #[test]
    fn an_upgrade_answer_must_match_the_request() {
        // The example of RFC 6455 §1.3.
        let key = "dGhlIHNhbXBsZSBub25jZQ==";
        let accept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=";
        let good = [
            ("upgrade", "websocket"),
            ("connection", "Upgrade"),
            ("sec-websocket-accept", accept),
        ];
        check_upgrade(&headers(&good), key).unwrap();
        check_upgrade(
            &headers(&[
                ("upgrade", "WebSocket"),
                ("connection", "keep-alive, upgrade"),
                ("sec-websocket-accept", accept),
            ]),
            key,
        )
        .unwrap();

        let bad: [&[(&str, &str)]; 6] = [
            &[("connection", "Upgrade"), ("sec-websocket-accept", accept)],
            &[
                ("upgrade", "h2c"),
                ("connection", "Upgrade"),
                ("sec-websocket-accept", accept),
            ],
            &[("upgrade", "websocket"), ("sec-websocket-accept", accept)],
            &[
                ("upgrade", "websocket"),
                ("connection", "close"),
                ("sec-websocket-accept", accept),
            ],
            &[("upgrade", "websocket"), ("connection", "Upgrade")],
            &[
                ("upgrade", "websocket"),
                ("connection", "Upgrade"),
                ("sec-websocket-accept", "AAAAAAAAAAAAAAAAAAAAAAAAAAA="),
            ],
        ];
        for pairs in bad {
            let result = check_upgrade(&headers(pairs), key);
            assert!(
                matches!(result, Err(Error::SocketProtocol { .. })),
                "{pairs:?}"
            );
        }
    }

    #[test]
    fn close_frames_are_sorted_by_code() {
        let frame = |code: u16, reason: &str| {
            Some(CloseFrame {
                code: code.into(),
                reason: reason.into(),
            })
        };
        assert_eq!(
            close_reason(frame(1001, "server shutting down")),
            StreamEnd::ServerGoingAway {
                reason: "server shutting down".into()
            }
        );
        assert_eq!(
            close_reason(frame(1008, "no longer a member of this channel")),
            StreamEnd::PolicyViolation {
                reason: "no longer a member of this channel".into()
            }
        );
        assert_eq!(
            close_reason(frame(1000, "")),
            StreamEnd::Closed {
                code: 1000,
                reason: String::new()
            }
        );
        assert_eq!(
            close_reason(frame(1011, "internal error")),
            StreamEnd::Closed {
                code: 1011,
                reason: "internal error".into()
            }
        );
        assert_eq!(
            close_reason(None),
            StreamEnd::Closed {
                code: 1005,
                reason: String::new()
            }
        );
        match close_reason(frame(1008, &"x".repeat(5000))) {
            StreamEnd::PolicyViolation { reason } => assert_eq!(reason.len(), MAX_TEXT_CHARS),
            other => panic!("{other:?}"),
        }
    }
}
