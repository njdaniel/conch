//! Proxy settings in the environment are not used (`Client::new` says why): with every
//! proxy variable pointing at a listener, the client makes no connection to it.
//!
//! The variables cannot be set in this process: tests run as threads and share one
//! environment, and setting it while another thread reads it is unsound. So the test runs
//! this same test binary again as a child process with the variables set, asking it for
//! the one test below that makes the calls, and then counts the connections its own
//! listener received.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::io::ErrorKind;
use std::net::TcpListener;
use std::process::Command;
use std::time::Duration;

use conch_voice_api::{Error, VoiceTransmitState};
use serde_json::json;
use support::{Reply, Stub, client_for, closed_port};

/// Set, to anything, in the child process only.
const CHILD: &str = "CONCH_VOICE_TEST_PROXY_CHILD";
/// What the child prints when it has made its calls, so that a child that ran no test at
/// all (a renamed test, a changed harness flag) cannot pass for one that did.
const CHILD_DONE: &str = "the child made its calls with the proxy variables set";

/// Every spelling a proxy is configured under.
const PROXY_VARIABLES: [&str; 6] = [
    "HTTP_PROXY",
    "http_proxy",
    "HTTPS_PROXY",
    "https_proxy",
    "ALL_PROXY",
    "all_proxy",
];

#[test]
fn no_connection_is_made_to_a_proxy_named_in_the_environment() {
    let proxy = TcpListener::bind("127.0.0.1:0").unwrap();
    proxy.set_nonblocking(true).unwrap();
    let proxy_url = format!("http://{}", proxy.local_addr().unwrap());

    let mut child = Command::new(std::env::current_exe().unwrap());
    child.args([
        "--exact",
        "the_calls_a_child_process_makes_with_proxy_variables_set",
        "--nocapture",
        "--test-threads=1",
    ]);
    for name in PROXY_VARIABLES {
        child.env(name, &proxy_url);
    }
    // An exemption for loopback in the developer's environment would let the test pass
    // without showing anything.
    child.env_remove("NO_PROXY").env_remove("no_proxy");
    child.env(CHILD, "1");
    let output = child.output().unwrap();
    let said = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );

    let mut connections = 0;
    loop {
        match proxy.accept() {
            Ok(_) => connections += 1,
            Err(e) if e.kind() == ErrorKind::WouldBlock => break,
            Err(e) => panic!("the proxy listener failed: {e}"),
        }
    }
    assert_eq!(
        connections, 0,
        "the client connected to the proxy named in the environment.\nchild output:\n{said}"
    );
    assert!(output.status.success(), "the child failed:\n{said}");
    assert!(said.contains(CHILD_DONE), "the child ran no calls:\n{said}");
}

/// Not a test of its own: the calls the child process makes for the test above. Without
/// the marker in the environment it does nothing.
#[tokio::test]
async fn the_calls_a_child_process_makes_with_proxy_variables_set() {
    if std::env::var_os(CHILD).is_none() {
        return;
    }
    for name in PROXY_VARIABLES {
        assert!(
            std::env::var_os(name).is_some(),
            "{name} is not set in the child"
        );
    }
    for name in ["NO_PROXY", "no_proxy"] {
        assert!(
            std::env::var_os(name).is_none(),
            "{name} is set in the child"
        );
    }

    let presence = json!({
        "schema": "conch.voice_presence.v1",
        "channel_id": 7,
        "configured": true,
        "available": true,
        "rooms": []
    });
    let stub = Stub::http(move |request| {
        if request.target.ends_with("/voice/transmit") {
            Reply::Empty(204)
        } else {
            Reply::Json(200, presence.to_string())
        }
    })
    .await;
    // Short: with a proxy in the way these would wait on a listener that never answers.
    let client = stub.client_with(Duration::from_secs(2));

    // Over plain HTTP, REST and the socket's upgrade: each reaches the stub itself.
    client.presence("general").await.unwrap();
    client
        .transmit("general", VoiceTransmitState::Started, None)
        .await
        .unwrap();
    let session = client.session("general").await;
    assert!(
        matches!(session, Err(Error::Undecodable { .. })),
        "{session:?}"
    );
    let socket = client.presence_stream("general").await.map(|_| ());
    assert!(
        matches!(socket, Err(Error::NotUpgraded { status: 200 })),
        "{socket:?}"
    );
    assert_eq!(stub.requests().len(), 4, "{:#?}", stub.requests());

    // Over HTTPS, to a port nothing listens on: refused there, not tunnelled.
    let secure = closed_port().await.replace("http://", "https://");
    let client = client_for(&secure, Duration::from_secs(2));
    let rest = client.presence("general").await.map(|_| ());
    assert!(matches!(rest, Err(Error::Connect { .. })), "{rest:?}");
    let socket = client.presence_stream("general").await.map(|_| ());
    assert!(matches!(socket, Err(Error::Connect { .. })), "{socket:?}");

    println!("{CHILD_DONE}");
}
