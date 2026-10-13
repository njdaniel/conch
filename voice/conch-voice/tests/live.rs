//! Two real `conch-voice` processes in one channel, against a real `conchd` and a real
//! LiveKit. Ignored by default: it needs both running (LiveKit is the pinned image in
//! Docker, as `e2e/voice` starts it) and two people signed in with the real `conch login`,
//! all set up by whoever runs it. The scenario that starts them itself, with three
//! clients, is the end-to-end issue's (#188).
//!
//! ```sh
//! CONCH_VOICE_LIVE_SERVER=http://127.0.0.1:8080 \
//! CONCH_VOICE_LIVE_CHANNEL=ops \
//! CONCH_VOICE_LIVE_CONFIG_DIR_A=/path/a \
//! CONCH_VOICE_LIVE_CONFIG_DIR_B=/path/b \
//!     cargo test --locked -p conch-voice --test live -- --ignored
//! ```
//!
//! Each `CONFIG_DIR` is an `XDG_CONFIG_HOME` holding `conch/credentials.json` for the
//! server; the two logins must be different people who are members of the channel, and
//! nobody else may be talking there. Each client's "microphone" is a tone that never stops:
//! what is asserted is that the other hears that tone while the key is down and nothing
//! while it is up. No token, room name or login is printed.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::io::{BufRead, BufReader, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::Value;

const BIN: &str = env!("CARGO_BIN_EXE_conch-voice");

/// How long one `stats` object covers.
const INTERVAL: Duration = Duration::from_secs(1);

fn var(name: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| panic!("set {name}; see the top of this file"))
}

/// One running client, with every object it has written and when.
struct Client {
    name: &'static str,
    child: Child,
    written: Arc<Mutex<Vec<(Instant, Value)>>>,
    everything: Arc<Mutex<String>>,
    /// The client's private home directory, removed with the client.
    _home: tempfile::TempDir,
}

impl Client {
    fn start(name: &'static str, config_dir: &str, tone: u32) -> Self {
        // An empty directory that nothing is read from or written to: it is set so that
        // nothing can fall back to the real home directory.
        let home = tempfile::tempdir().unwrap();
        let mut child = Command::new(BIN)
            .env_clear()
            .env("XDG_CONFIG_HOME", config_dir)
            .env("HOME", home.path())
            .args(["join", &var("CONCH_VOICE_LIVE_CHANNEL")])
            .args(["--server", &var("CONCH_VOICE_LIVE_SERVER"), "--json"])
            .args(["--mic", &format!("tone:{tone}"), "--sink", "count:440,880"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let written = Arc::new(Mutex::new(Vec::new()));
        let everything = Arc::new(Mutex::new(String::new()));
        let (out, objects, all) = (
            child.stdout.take().unwrap(),
            Arc::clone(&written),
            Arc::clone(&everything),
        );
        std::thread::spawn(move || {
            for line in BufReader::new(out).lines().map_while(Result::ok) {
                all.lock().unwrap().push_str(&line);
                // A line that is not an object is not this program's: libwebrtc prints
                // one on a machine with an NVIDIA GPU.
                if line.starts_with('{')
                    && let Ok(object) = serde_json::from_str(&line)
                {
                    objects.lock().unwrap().push((Instant::now(), object));
                }
            }
        });
        let (err, all) = (child.stderr.take().unwrap(), Arc::clone(&everything));
        std::thread::spawn(move || {
            for line in BufReader::new(err).lines().map_while(Result::ok) {
                all.lock().unwrap().push_str(&line);
            }
        });
        Self {
            name,
            child,
            written,
            everything,
            _home: home,
        }
    }

    fn say(&mut self, command: &str) -> Instant {
        let stdin = self.child.stdin.as_mut().unwrap();
        writeln!(stdin, "{command}").unwrap();
        stdin.flush().unwrap();
        Instant::now()
    }

    fn objects(&self) -> Vec<(Instant, Value)> {
        self.written.lock().unwrap().clone()
    }

    /// Waits for an object that satisfies `met`, and gives it.
    fn wait_for(&self, what: &str, met: impl Fn(&Value) -> bool) -> Value {
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            if let Some((_, object)) = self.objects().into_iter().find(|(_, o)| met(o)) {
                return object;
            }
            assert!(
                Instant::now() < deadline,
                "{}: timed out waiting for {what}",
                self.name
            );
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// What this client counted of `speaker` in each `stats` interval that lies wholly
    /// between `from` and `to`.
    fn heard(&self, speaker: &str, from: Instant, to: Instant) -> Vec<Value> {
        self.objects()
            .into_iter()
            .filter(|(at, object)| {
                object["event"] == "stats" && *at - INTERVAL >= from && *at <= to
            })
            .map(|(_, stats)| {
                stats["speakers"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .find(|entry| entry["speaker"] == speaker)
                    .cloned()
                    .unwrap_or(Value::Null)
            })
            .collect()
    }

    fn quit(mut self) -> String {
        self.say("quit");
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                assert_eq!(
                    status.code(),
                    Some(0),
                    "{}: quit is a clean exit",
                    self.name
                );
                break;
            }
            assert!(Instant::now() < deadline, "{}: did not exit", self.name);
            std::thread::sleep(Duration::from_millis(20));
        }
        std::thread::sleep(Duration::from_millis(100));
        self.everything.lock().unwrap().clone()
    }
}

/// `talker` holds the key for a while and lets go; `listener` must count the talker's tone
/// while it is down, and nothing audible while it is up.
fn one_press(talker: &mut Client, listener: &Client, talker_id: &str, tone: f64) {
    // Before: the talker's tone is running and its gate is shut.
    let quiet_from = Instant::now();
    std::thread::sleep(INTERVAL * 2 + Duration::from_millis(200));
    let down = talker.say("down");
    std::thread::sleep(INTERVAL * 4);
    let up = talker.say("up");
    // The release tail, what is in flight, and the listener's jitter buffer.
    let settled = up + Duration::from_millis(600);
    std::thread::sleep(INTERVAL * 4);
    let end = Instant::now();

    let before = listener.heard(talker_id, quiet_from, down);
    let during = listener.heard(talker_id, down + Duration::from_millis(300), up);
    let after = listener.heard(talker_id, settled, end);
    println!(
        "{} hearing {} ({tone} Hz)\n  before: {before:?}\n  key down: {during:?}\n  key up: {after:?}",
        listener.name, talker.name
    );

    assert!(
        during.len() >= 2,
        "too few whole intervals with the key down"
    );
    for entry in &during {
        assert_eq!(entry["dominant_hz"], tone, "the talker's tone: {entry}");
        assert!(
            entry["audible_frames"].as_u64().unwrap() >= 90,
            "a second of it: {entry}"
        );
    }
    assert!(
        !before.is_empty() && after.len() >= 2,
        "too few quiet intervals"
    );
    for entry in before.iter().chain(&after) {
        // No entry at all is also nothing heard.
        if !entry.is_null() {
            assert_eq!(entry["audible_frames"], 0, "nothing audible: {entry}");
            assert!(entry["dominant_hz"].is_null(), "no tone: {entry}");
        }
    }

    // And the talker handed its track frames only while the key was down.
    let sent = |from: Instant, to: Instant| -> u64 {
        talker
            .objects()
            .into_iter()
            .filter(|(at, o)| o["event"] == "stats" && *at - INTERVAL >= from && *at <= to)
            .map(|(_, stats)| stats["frames_sent"].as_u64().unwrap())
            .sum()
    };
    assert!(sent(down + Duration::from_millis(300), up) >= 180);
    assert_eq!(sent(quiet_from, down), 0, "nothing before the press");
    assert_eq!(sent(settled, end), 0, "nothing after the release");
}

#[test]
#[ignore = "needs a running conchd with LiveKit and two stored logins; see the top of this file"]
fn the_listener_counts_the_talkers_tone_only_while_the_talkers_key_is_down() {
    let dir_a = var("CONCH_VOICE_LIVE_CONFIG_DIR_A");
    let dir_b = var("CONCH_VOICE_LIVE_CONFIG_DIR_B");
    let mut a = Client::start("a", &dir_a, 440);
    let mut b = Client::start("b", &dir_b, 880);

    let ready = |o: &Value| o["event"] == "self" && o["blocked"] == serde_json::json!([]);
    let subscribed = |o: &Value| o["event"] == "track" && o["state"] == "subscribed";
    a.wait_for("ready to talk", ready);
    b.wait_for("ready to talk", ready);
    // Each learns the other's identity from the track it subscribes to.
    let b_id = a.wait_for("b's track", subscribed)["speaker"]
        .as_str()
        .unwrap()
        .to_owned();
    let a_id = b.wait_for("a's track", subscribed)["speaker"]
        .as_str()
        .unwrap()
        .to_owned();
    assert_ne!(a_id, b_id, "the two logins must be different people");

    one_press(&mut a, &b, &a_id, 440.0);
    one_press(&mut b, &a, &b_id, 880.0);

    // Neither ever counted audio of its own.
    for (client, own) in [(&a, &a_id), (&b, &b_id)] {
        for (_, object) in client.objects() {
            if object["event"] == "stats" {
                let speakers = object["speakers"].as_array().unwrap();
                assert!(speakers.iter().all(|entry| entry["speaker"] != **own));
            }
        }
    }

    // No login in anything either wrote.
    let mut logins = Vec::new();
    for dir in [&dir_a, &dir_b] {
        let path = PathBuf::from(dir).join("conch").join("credentials.json");
        let stored: Value = serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap();
        for entry in stored.as_object().unwrap().values() {
            logins.push(entry["token"].as_str().unwrap().to_owned());
        }
    }
    for client in [a, b] {
        let everything = client.quit();
        for login in &logins {
            assert!(!everything.contains(login), "a login token was written");
        }
        assert!(!everything.contains("eyJ"), "something shaped like a token");
    }
}
