//! A stand-in for `conchd`, written by hand on tokio's listener (the pattern of
//! `conch-voice-api`'s test support, copied and cut down, not imported): the three things
//! `conch-voice` asks of `conchd`, with every answer a test might need to arrange.
//!
//! - `POST /v1/channels/{channel}/voice/session`: a session whose join token and room name
//!   are numbered, so a test can tell which session a connection was made with.
//! - `POST /v1/channels/{channel}/voice/transmit`: 204, or what the test planned.
//! - `GET /v1/voice/ws`: a WebSocket that sends each presence document the test gives it.
//!
//! Connections are kept alive, as `conchd` keeps them, and every request is recorded with
//! the number of the connection it came on, so a test can tell a reused connection from a
//! new one and see when the client closed one.

use std::collections::VecDeque;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::task::JoinHandle;

/// The login every test signs in with. Obviously not a credential.
pub const FAKE_LOGIN: &str = "conch_FAKE_login_token_do_not_print";
/// What every join token the stub issues begins with. Obviously not a credential.
pub const FAKE_JOIN: &str = "FAKE-join-token-do-not-print";
/// What every room name the stub issues begins with.
pub const FAKE_ROOM: &str = "FAKE-room-name-do-not-print";
/// The identity the stub gives the caller.
pub const OWN_IDENTITY: &str = "p7";

/// One request as the stub received it.
#[derive(Debug, Clone)]
pub struct Request {
    /// The number of the connection it came on, from 1.
    pub conn: u64,
    pub method: String,
    /// The path and query, exactly as sent.
    pub target: String,
    pub body: String,
}

/// How the stub answers one request.
#[derive(Debug, Clone)]
pub enum Reply {
    /// A status and a JSON body.
    Json(u16, String),
    /// A status and no body.
    Empty(u16),
    /// Read the request and never answer: a `conchd` that has stopped responding.
    Stall,
}

impl Reply {
    /// An error document, as `conchd` writes one.
    pub fn error(status: u16, code: &str) -> Self {
        Self::Json(
            status,
            serde_json::json!({"code": code, "message": "the stub says no"}).to_string(),
        )
    }
}

/// The join token of the stub's `n`th session, from 1.
pub fn join_token(n: u32) -> String {
    format!("{FAKE_JOIN}-{n}")
}

/// The room name of the stub's `n`th session, from 1.
pub fn room_name(n: u32) -> String {
    format!("{FAKE_ROOM}-{n}")
}

/// A presence document for the channel's room with these participants: an id, and whether
/// each is transmitting.
pub fn presence(participants: &[(i64, bool)]) -> String {
    let participants: Vec<_> = participants
        .iter()
        .map(|(id, transmitting)| {
            serde_json::json!({
                "principal_id": id,
                "can_publish": true,
                "transmitting": transmitting,
                "joined_at": "2026-10-09T12:00:31.000Z",
            })
        })
        .collect();
    serde_json::json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": true,
        "available": true,
        "rooms": [{"participants": participants}],
    })
    .to_string()
}

struct State {
    requests: Vec<Request>,
    sessions_issued: u32,
    /// Answers to the next session requests; when empty, a session is issued.
    session_plan: VecDeque<Reply>,
    can_publish: bool,
    /// A grant for a net, listed before the channel's own, in every session.
    with_net_grant: bool,
    livekit_url: String,
    /// Answers to the next transmit reports; when empty, 204.
    transmit_plan: VecDeque<Reply>,
    presence: Option<String>,
    presence_sockets: Vec<mpsc::UnboundedSender<String>>,
    /// Connections the client closed, in the order it closed them.
    closed: Vec<u64>,
    connections: u64,
}

/// A running stub. Dropping it stops it.
pub struct Stub {
    addr: SocketAddr,
    state: Arc<Mutex<State>>,
    task: JoinHandle<()>,
}

impl Drop for Stub {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Stub {
    /// Starts a stub on a port of its own.
    pub async fn start() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let state = Arc::new(Mutex::new(State {
            requests: Vec::new(),
            sessions_issued: 0,
            session_plan: VecDeque::new(),
            can_publish: true,
            with_net_grant: false,
            livekit_url: "ws://livekit.invalid:7880".to_owned(),
            transmit_plan: VecDeque::new(),
            presence: None,
            presence_sockets: Vec::new(),
            closed: Vec::new(),
            connections: 0,
        }));
        let shared = Arc::clone(&state);
        let task = tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                let conn = {
                    let mut state = shared.lock().unwrap();
                    state.connections += 1;
                    state.connections
                };
                tokio::spawn(serve(stream, conn, Arc::clone(&shared)));
            }
        });
        Self { addr, state, task }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap()
    }

    /// The stub's address, as a server address.
    pub fn url(&self) -> String {
        format!("http://{}", self.addr)
    }

    // ------------------------------------------------------------ what a test arranges

    /// The next session request gets this answer instead of a session.
    pub fn next_session(&self, reply: Reply) {
        self.lock().session_plan.push_back(reply);
    }

    /// Whether the sessions issued from now on allow publishing.
    pub fn can_publish(&self, allowed: bool) {
        self.lock().can_publish = allowed;
    }

    /// Sessions from now on also carry a grant for a net, listed first.
    pub fn with_net_grant(&self) {
        self.lock().with_net_grant = true;
    }

    /// The LiveKit address the sessions issued from now on carry.
    pub fn livekit_url(&self, url: &str) {
        self.lock().livekit_url = url.to_owned();
    }

    /// The next transmit report gets this answer instead of 204.
    pub fn next_transmit(&self, reply: Reply) {
        self.lock().transmit_plan.push_back(reply);
    }

    /// Sends a presence document to every open presence socket, and to each new one.
    pub fn presence(&self, document: String) {
        let mut state = self.lock();
        state
            .presence_sockets
            .retain(|socket| socket.send(document.clone()).is_ok());
        state.presence = Some(document);
    }

    // ------------------------------------------------------------- what a test observes

    /// Every request received so far, in order.
    pub fn requests(&self) -> Vec<Request> {
        self.lock().requests.clone()
    }

    /// How many session requests there have been.
    pub fn session_requests(&self) -> usize {
        self.requests_to("/voice/session").len()
    }

    /// The transmit reports received so far, in order: the connection each came on and
    /// the state it reported.
    pub fn reports(&self) -> Vec<(u64, String)> {
        self.requests_to("/voice/transmit")
            .into_iter()
            .map(|request| {
                let body: serde_json::Value = serde_json::from_str(&request.body).unwrap();
                (request.conn, body["state"].as_str().unwrap().to_owned())
            })
            .collect()
    }

    /// The states of the transmit reports received so far, in order.
    pub fn reported(&self) -> Vec<String> {
        self.reports().into_iter().map(|(_, state)| state).collect()
    }

    fn requests_to(&self, suffix: &str) -> Vec<Request> {
        self.requests()
            .into_iter()
            .filter(|request| request.target.ends_with(suffix))
            .collect()
    }

    /// The connections the client has closed.
    pub fn closed(&self) -> Vec<u64> {
        self.lock().closed.clone()
    }
}

fn session(state: &mut State) -> Reply {
    state.sessions_issued += 1;
    let n = state.sessions_issued;
    let mut rooms = Vec::new();
    if state.with_net_grant {
        rooms.push(serde_json::json!({
            "room": format!("{FAKE_ROOM}-net-{n}"),
            "token": format!("{FAKE_JOIN}-net-{n}"),
            "can_publish": true,
            "expires_at": "2026-10-09T12:00:45.000Z",
            "audience": {"kind": "net", "net_id": 3},
        }));
    }
    rooms.push(serde_json::json!({
        "room": room_name(n),
        "token": join_token(n),
        "can_publish": state.can_publish,
        "expires_at": "2026-10-09T12:00:45.000Z",
    }));
    Reply::Json(
        200,
        serde_json::json!({
            "livekit_url": state.livekit_url,
            "identity": OWN_IDENTITY,
            "rooms": rooms,
        })
        .to_string(),
    )
}

enum Routed {
    Reply(Reply),
    /// Upgrade to a WebSocket, answering this key.
    Socket(String),
}

fn route(state: &Mutex<State>, request: &Request, headers: &[(String, String)]) -> Routed {
    let header = |name: &str| {
        headers
            .iter()
            .find(|(n, _)| n == name)
            .map(|(_, value)| value.as_str())
    };
    let mut state = state.lock().unwrap();
    state.requests.push(request.clone());
    if header("authorization") != Some(&format!("Bearer {FAKE_LOGIN}")) {
        return Routed::Reply(Reply::error(401, "unauthorized"));
    }
    let path = request.target.split('?').next().unwrap_or_default();
    match (request.method.as_str(), path) {
        ("POST", path) if path.ends_with("/voice/session") => {
            let planned = state.session_plan.pop_front();
            Routed::Reply(planned.unwrap_or_else(|| session(&mut state)))
        }
        ("POST", path) if path.ends_with("/voice/transmit") => {
            Routed::Reply(state.transmit_plan.pop_front().unwrap_or(Reply::Empty(204)))
        }
        ("GET", "/v1/voice/ws") => match header("sec-websocket-key") {
            Some(key) => Routed::Socket(key.to_owned()),
            None => Routed::Reply(Reply::error(400, "bad_request")),
        },
        _ => Routed::Reply(Reply::error(404, "not_found")),
    }
}

/// Reads one HTTP/1.1 request from a connection that may carry several. `None` when the
/// client has closed it.
async fn read_request(
    stream: &mut TcpStream,
    buffer: &mut Vec<u8>,
    conn: u64,
) -> Option<(Request, Vec<(String, String)>)> {
    let head_end = loop {
        if let Some(at) = buffer.windows(4).position(|w| w == b"\r\n\r\n") {
            break at;
        }
        let mut chunk = [0u8; 4096];
        match stream.read(&mut chunk).await {
            Ok(0) | Err(_) => return None,
            Ok(n) => buffer.extend_from_slice(&chunk[..n]),
        }
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
    let body_start = head_end + 4;
    while buffer.len() < body_start + length {
        let mut chunk = [0u8; 4096];
        match stream.read(&mut chunk).await {
            Ok(0) | Err(_) => return None,
            Ok(n) => buffer.extend_from_slice(&chunk[..n]),
        }
    }
    let body = String::from_utf8_lossy(&buffer[body_start..body_start + length]).into_owned();
    buffer.drain(..body_start + length);
    let request = Request {
        conn,
        method,
        target,
        body,
    };
    Some((request, headers))
}

/// Waits until the client closes the connection, taking no notice of what it sends.
async fn until_closed(stream: &mut TcpStream) {
    let mut sink = [0u8; 1024];
    while let Ok(n) = stream.read(&mut sink).await {
        if n == 0 {
            return;
        }
    }
}

async fn serve(mut stream: TcpStream, conn: u64, state: Arc<Mutex<State>>) {
    let mut buffer = Vec::new();
    while let Some((request, headers)) = read_request(&mut stream, &mut buffer, conn).await {
        let response = match route(&state, &request, &headers) {
            Routed::Reply(Reply::Json(status, body)) => format!(
                "HTTP/1.1 {status} Stub\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                body.len()
            ),
            Routed::Reply(Reply::Empty(204)) => "HTTP/1.1 204 Stub\r\n\r\n".to_owned(),
            Routed::Reply(Reply::Empty(status)) => {
                format!("HTTP/1.1 {status} Stub\r\nContent-Length: 0\r\n\r\n")
            }
            Routed::Reply(Reply::Stall) => {
                until_closed(&mut stream).await;
                break;
            }
            Routed::Socket(key) => {
                presence_socket(&mut stream, &key, &state).await;
                break;
            }
        };
        if stream.write_all(response.as_bytes()).await.is_err() {
            break;
        }
    }
    state.lock().unwrap().closed.push(conn);
}

/// Answers the upgrade, then sends each presence document as a text frame until the client
/// goes away.
async fn presence_socket(stream: &mut TcpStream, key: &str, state: &Mutex<State>) {
    let accept = base64(&sha1(
        format!("{key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11").as_bytes(),
    ));
    let response = format!(
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: {accept}\r\n\r\n"
    );
    if stream.write_all(response.as_bytes()).await.is_err() {
        return;
    }
    let (documents, mut queued) = mpsc::unbounded_channel();
    {
        let mut state = state.lock().unwrap();
        if let Some(latest) = &state.presence {
            let _ = documents.send(latest.clone());
        }
        state.presence_sockets.push(documents);
    }
    let mut sink = [0u8; 1024];
    loop {
        tokio::select! {
            document = queued.recv() => {
                let Some(document) = document else { return };
                if stream.write_all(&text_frame(document.as_bytes())).await.is_err() {
                    return;
                }
            }
            read = stream.read(&mut sink) => match read {
                Ok(0) | Err(_) => return,
                // A pong or a close from the client: nothing here needs it.
                Ok(_) => {}
            },
        }
    }
}

/// One unmasked WebSocket text frame, as a server sends it.
fn text_frame(payload: &[u8]) -> Vec<u8> {
    let mut frame = vec![0x81];
    match payload.len() {
        n if n < 126 => frame.push(n as u8),
        n if n < 65536 => {
            frame.push(126);
            frame.extend_from_slice(&(n as u16).to_be_bytes());
        }
        n => {
            frame.push(127);
            frame.extend_from_slice(&(n as u64).to_be_bytes());
        }
    }
    frame.extend_from_slice(payload);
    frame
}

/// SHA-1, which the WebSocket handshake needs and nothing else here does. Written out
/// because this crate's tests may not add a dependency for it.
fn sha1(message: &[u8]) -> [u8; 20] {
    let mut h: [u32; 5] = [
        0x6745_2301,
        0xEFCD_AB89,
        0x98BA_DCFE,
        0x1032_5476,
        0xC3D2_E1F0,
    ];
    let mut padded = message.to_vec();
    padded.push(0x80);
    while padded.len() % 64 != 56 {
        padded.push(0);
    }
    padded.extend_from_slice(&((message.len() as u64) * 8).to_be_bytes());
    for block in padded.chunks(64) {
        let mut w = [0u32; 80];
        for (i, word) in block.chunks(4).enumerate() {
            w[i] = u32::from_be_bytes([word[0], word[1], word[2], word[3]]);
        }
        for i in 16..80 {
            w[i] = (w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16]).rotate_left(1);
        }
        let [mut a, mut b, mut c, mut d, mut e] = h;
        for (i, word) in w.iter().enumerate() {
            let (f, k) = match i {
                0..20 => ((b & c) | (!b & d), 0x5A82_7999),
                20..40 => (b ^ c ^ d, 0x6ED9_EBA1),
                40..60 => ((b & c) | (b & d) | (c & d), 0x8F1B_BCDC),
                _ => (b ^ c ^ d, 0xCA62_C1D6),
            };
            let next = a
                .rotate_left(5)
                .wrapping_add(f)
                .wrapping_add(e)
                .wrapping_add(k)
                .wrapping_add(*word);
            e = d;
            d = c;
            c = b.rotate_left(30);
            b = a;
            a = next;
        }
        for (sum, value) in h.iter_mut().zip([a, b, c, d, e]) {
            *sum = sum.wrapping_add(value);
        }
    }
    let mut digest = [0u8; 20];
    for (bytes, word) in digest.chunks_mut(4).zip(h) {
        bytes.copy_from_slice(&word.to_be_bytes());
    }
    digest
}

fn base64(bytes: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for chunk in bytes.chunks(3) {
        let b = [
            chunk[0],
            chunk.get(1).copied().unwrap_or(0),
            chunk.get(2).copied().unwrap_or(0),
        ];
        let n = (u32::from(b[0]) << 16) | (u32::from(b[1]) << 8) | u32::from(b[2]);
        for (i, shift) in [18, 12, 6, 0].into_iter().enumerate() {
            if i <= chunk.len() {
                out.push(ALPHABET[((n >> shift) & 63) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

#[test]
fn the_handshake_arithmetic_matches_the_rfcs_own_example() {
    // RFC 3174's first test vector, and RFC 6455 §1.3's handshake.
    assert_eq!(
        sha1(b"abc"),
        [
            0xA9, 0x99, 0x3E, 0x36, 0x47, 0x06, 0x81, 0x6A, 0xBA, 0x3E, 0x25, 0x71, 0x78, 0x50,
            0xC2, 0x6C, 0x9C, 0xD0, 0xD8, 0x9D
        ]
    );
    assert_eq!(
        base64(&sha1(
            b"dGhlIHNhbXBsZSBub25jZQ==258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
        )),
        "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
    );
}
