//! A stand-in for `conchd`, written by hand on tokio's listener: enough HTTP/1.1 to make
//! every answer the client has to cope with reproducible, and a WebSocket side that plays
//! a script. It binds `127.0.0.1:0`, so tests never share a port, and it records what it
//! was sent so that tests can check the request and count the attempts.

// Each test binary uses its own part of this module.
#![allow(dead_code)]

use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice_api::{Client, Secret, ServerAddress};
use futures_util::{SinkExt, StreamExt};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::task::JoinHandle;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::tungstenite::handshake::server::{
    ErrorResponse, Request as WsRequest, Response as WsResponse,
};
use tokio_tungstenite::tungstenite::protocol::CloseFrame;

/// The login every test signs in with. Obviously not a credential.
pub const FAKE_TOKEN: &str = "conch_FAKE_login_token_do_not_print";

/// What a request has to carry for the stub to be `conchd` to it.
pub const FAKE_AUTHORIZATION: &str = "Bearer conch_FAKE_login_token_do_not_print";

/// One request as the stub received it.
#[derive(Debug, Clone)]
pub struct Request {
    pub method: String,
    /// The request target: path and query, exactly as sent.
    pub target: String,
    /// Header names in lower case.
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

impl Request {
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(n, _)| n == name)
            .map(|(_, v)| v.as_str())
    }

    /// Everything the stub was sent, as one string, for "this never appears" checks.
    pub fn everything(&self) -> String {
        format!(
            "{} {} {:?} {}",
            self.method,
            self.target,
            self.headers,
            String::from_utf8_lossy(&self.body)
        )
    }
}

/// How the stub answers one HTTP request.
#[derive(Debug, Clone)]
pub enum Reply {
    /// A status and a JSON body, as `conchd` writes them.
    Json(u16, String),
    /// A status and a body of another kind, as a proxy in front of `conchd` might write.
    Body(u16, &'static str, Vec<u8>),
    /// A status and no body.
    Empty(u16),
    /// A redirect to this location.
    Redirect(u16, String),
    /// Say nothing for this long (the slow server), then answer 200.
    Stall(Duration),
    /// Read the request and close the connection without a word.
    Hangup,
    /// Promise a body, send half of it, and close.
    Truncated(u16),
    /// These bytes, exactly, as the whole response.
    Raw(Vec<u8>),
}

impl Reply {
    /// An error document, as `conchd`'s `writeError` writes it.
    pub fn error(status: u16, code: &str, message: &str) -> Self {
        Self::Json(
            status,
            serde_json::json!({"code": code, "message": message}).to_string(),
        )
    }
}

/// One step of what the WebSocket stub does after the upgrade.
#[derive(Debug, Clone)]
pub enum Step {
    /// Send a text frame.
    Text(String),
    /// Send a text frame made from the upgrade request: a peer that reflects what it
    /// was sent.
    TextFrom(fn(&Request) -> String),
    /// Send a close frame with this code and a reason made from the upgrade request.
    CloseFrom(u16, fn(&Request) -> String),
    /// Send a binary frame.
    Binary(Vec<u8>),
    /// Send a ping.
    Ping,
    /// Wait.
    Wait(Duration),
    /// Send a close frame with this code and reason, and finish the closing handshake.
    Close(u16, &'static str),
    /// Send a close frame and then neither answer nor hang up for this long: a server
    /// that never finishes the closing handshake.
    CloseAndStall(u16, &'static str, Duration),
    /// Close the TCP connection with no close frame.
    Drop,
    /// Reset the TCP connection.
    Reset,
}

/// A running stub. Dropping it stops it.
pub struct Stub {
    addr: SocketAddr,
    requests: Arc<Mutex<Vec<Request>>>,
    task: JoinHandle<()>,
}

impl Drop for Stub {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Stub {
    /// A stub that answers every HTTP request with what `handler` says. One request per
    /// connection: every response closes it.
    pub async fn http(handler: impl Fn(&Request) -> Reply + Send + Sync + 'static) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let handler = Arc::new(handler);
        let seen = requests.clone();
        let task = tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                let (handler, seen) = (handler.clone(), seen.clone());
                tokio::spawn(async move {
                    let _ = answer(stream, &*handler, &seen).await;
                });
            }
        });
        Self {
            addr,
            requests,
            task,
        }
    }

    /// A stub that always gives the same answer.
    pub async fn always(reply: Reply) -> Self {
        Self::http(move |_| reply.clone()).await
    }

    /// A stub that upgrades every connection to a WebSocket and then plays `script`.
    /// After the script it keeps the socket open until the client goes away, unless the
    /// script ended it.
    pub async fn websocket(script: Vec<Step>) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let seen = requests.clone();
        let task = tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                let (script, seen) = (script.clone(), seen.clone());
                tokio::spawn(async move {
                    let _ = play(stream, script, &seen).await;
                });
            }
        });
        Self {
            addr,
            requests,
            task,
        }
    }

    /// The stub's address as a server address, with an optional path prefix.
    pub fn url(&self, prefix: &str) -> String {
        format!("http://{}{prefix}", self.addr)
    }

    /// A client of this stub, signed in with the fake token, with time to spare.
    pub fn client(&self) -> Client {
        self.client_with(Duration::from_secs(10))
    }

    pub fn client_with(&self, timeout: Duration) -> Client {
        client_for(&self.url(""), timeout)
    }

    /// Every request received so far, in order.
    pub fn requests(&self) -> Vec<Request> {
        self.requests.lock().unwrap().clone()
    }

    /// The one request the stub should have received. More than one means something
    /// was sent twice.
    pub fn only_request(&self) -> Request {
        let requests = self.requests();
        assert_eq!(
            requests.len(),
            1,
            "expected exactly one request, got {requests:#?}"
        );
        requests.into_iter().next().unwrap()
    }
}

pub fn client_for(server: &str, timeout: Duration) -> Client {
    let server = ServerAddress::parse(server).unwrap();
    Client::new(&server, &Secret::new(FAKE_TOKEN), timeout).unwrap()
}

/// An address on this machine that nothing listens on.
pub async fn closed_port() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    drop(listener);
    format!("http://{addr}")
}

/// Reads one HTTP/1.1 request: the head up to the blank line, then `Content-Length` bytes.
async fn read_request(stream: &mut TcpStream) -> std::io::Result<Request> {
    let mut buffer = Vec::new();
    let head_end = loop {
        if let Some(at) = buffer.windows(4).position(|w| w == b"\r\n\r\n") {
            break at;
        }
        let mut chunk = [0u8; 4096];
        let n = stream.read(&mut chunk).await?;
        if n == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        buffer.extend_from_slice(&chunk[..n]);
    };
    let head = String::from_utf8_lossy(&buffer[..head_end]).into_owned();
    let mut lines = head.split("\r\n");
    let mut request_line = lines.next().unwrap_or_default().split(' ');
    let method = request_line.next().unwrap_or_default().to_owned();
    let target = request_line.next().unwrap_or_default().to_owned();
    let headers: Vec<(String, String)> = lines
        .filter_map(|line| line.split_once(':'))
        .map(|(name, value)| (name.trim().to_ascii_lowercase(), value.trim().to_owned()))
        .collect();
    let length = headers
        .iter()
        .find(|(name, _)| name == "content-length")
        .and_then(|(_, value)| value.parse::<usize>().ok())
        .unwrap_or(0);
    let mut body = buffer[head_end + 4..].to_vec();
    while body.len() < length {
        let mut chunk = [0u8; 4096];
        let n = stream.read(&mut chunk).await?;
        if n == 0 {
            return Err(std::io::ErrorKind::UnexpectedEof.into());
        }
        body.extend_from_slice(&chunk[..n]);
    }
    Ok(Request {
        method,
        target,
        headers,
        body,
    })
}

fn response(status: u16, content_type: Option<&str>, extra: &str, body: &[u8]) -> Vec<u8> {
    let mut out = format!("HTTP/1.1 {status} Stub\r\nConnection: close\r\n{extra}");
    if let Some(content_type) = content_type {
        out.push_str(&format!("Content-Type: {content_type}\r\n"));
    }
    out.push_str(&format!("Content-Length: {}\r\n\r\n", body.len()));
    let mut out = out.into_bytes();
    out.extend_from_slice(body);
    out
}

async fn answer(
    mut stream: TcpStream,
    handler: &(dyn Fn(&Request) -> Reply + Send + Sync),
    seen: &Mutex<Vec<Request>>,
) -> std::io::Result<()> {
    let request = read_request(&mut stream).await?;
    let reply = handler(&request);
    seen.lock().unwrap().push(request);
    let bytes = match reply {
        Reply::Json(status, body) => {
            response(status, Some("application/json"), "", body.as_bytes())
        }
        Reply::Body(status, content_type, body) => response(status, Some(content_type), "", &body),
        Reply::Empty(status) => response(status, None, "", b""),
        Reply::Redirect(status, location) => {
            response(status, None, &format!("Location: {location}\r\n"), b"")
        }
        Reply::Stall(wait) => {
            tokio::time::sleep(wait).await;
            response(200, Some("application/json"), "", b"{}")
        }
        Reply::Hangup => return Ok(()),
        Reply::Truncated(status) => {
            let whole = response(status, Some("application/json"), "", &[b' '; 64]);
            whole[..whole.len() - 32].to_vec()
        }
        Reply::Raw(bytes) => bytes,
    };
    stream.write_all(&bytes).await?;
    stream.shutdown().await
}

async fn play(
    stream: TcpStream,
    script: Vec<Step>,
    seen: &Mutex<Vec<Request>>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    // The upgrade request is recorded like any other, so tests can check how the
    // socket was asked for. The closure's type is the one tungstenite's Callback asks for.
    #[allow(clippy::result_large_err)]
    let record = |request: &WsRequest, response: WsResponse| -> Result<WsResponse, ErrorResponse> {
        seen.lock().unwrap().push(Request {
            method: request.method().to_string(),
            target: request.uri().to_string(),
            headers: request
                .headers()
                .iter()
                .map(|(name, value)| {
                    (
                        name.as_str().to_owned(),
                        String::from_utf8_lossy(value.as_bytes()).into_owned(),
                    )
                })
                .collect(),
            body: Vec::new(),
        });
        Ok(response)
    };
    let mut socket = tokio_tungstenite::accept_hdr_async(stream, record).await?;
    let request = seen.lock().unwrap().last().cloned().unwrap();
    for step in script {
        match step {
            Step::Text(text) => socket.send(Message::text(text)).await?,
            Step::TextFrom(make) => socket.send(Message::text(make(&request))).await?,
            Step::CloseFrom(code, make) => {
                socket
                    .close(Some(CloseFrame {
                        code: code.into(),
                        reason: make(&request).into(),
                    }))
                    .await?;
                break;
            }
            Step::Binary(bytes) => socket.send(Message::binary(bytes)).await?,
            Step::Ping => socket.send(Message::Ping(Vec::new().into())).await?,
            Step::Wait(wait) => tokio::time::sleep(wait).await,
            Step::Close(code, reason) => {
                socket
                    .close(Some(CloseFrame {
                        code: code.into(),
                        reason: reason.into(),
                    }))
                    .await?;
                break;
            }
            Step::CloseAndStall(code, reason, wait) => {
                let frame = CloseFrame {
                    code: code.into(),
                    reason: reason.into(),
                };
                socket.send(Message::Close(Some(frame))).await?;
                tokio::time::sleep(wait).await;
                return Ok(());
            }
            Step::Drop => return Ok(()),
            Step::Reset => {
                socket.get_ref().set_zero_linger()?;
                return Ok(());
            }
        }
    }
    // Until the client answers the close frame or goes away.
    while let Some(Ok(_)) = socket.next().await {}
    Ok(())
}
