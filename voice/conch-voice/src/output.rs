//! What `conch-voice join` writes to standard output: plain lines for a person, or with
//! `--json` one JSON object per line for a program.
//!
//! Nothing written here holds a join token, a room name or the login token: none of the
//! values an [`Event`] carries can be one. Participants are named by principal id (`p7`),
//! as `conch voice status` names them.
//!
//! # `--json`
//!
//! Every line this program writes to standard output is one JSON object, begins with `{`
//! and has an `event` field. **A reader must skip any line that does not begin with `{`:**
//! on a machine with an NVIDIA GPU, libwebrtc prints a line of its own to standard output
//! in every process, outside any logging, and nothing here can prevent that. Fields may be
//! added; a reader should ignore those it does not know.
//!
//! **A reader that stops reading loses lines; it does not stop the client.** Lines are
//! handed to a thread that writes them (`lines.rs`), and at most 1024 wait for it. Beyond
//! that a line is dropped, and when there is room again an `output_dropped` object (in plain
//! mode a line in words) says how many were. Nothing the client does waits for a line to be
//! read: not the release of the key, not `quit`, and not the exit.
//!
//! | `event` | Written | Fields |
//! |---|---|---|
//! | `connection` | on every change of the voice connection | `state`: `connecting` (asking `conchd` for a session and joining), `connected`, `reconnecting` (the SDK is restoring the connection; not connected until it has), `waiting` (will ask `conchd` again), `stopped` (final; the exit code is not 0) or `closed` (left after `quit`). `reason`, when the state has one: `room_deleted`, `participant_removed`, `duplicate_identity`, `server_shutdown`, `connection_lost`, `reconnect_timed_out`, `connect_failed`, `server_unreachable`, `voice_unavailable`, or for `stopped` the policy's sentence. `detail`: the error's text, if there was one. `retry_in_ms`, with `waiting`: how long until `conchd` is asked; 0 means at once. |
//! | `self` | on every change of the user's own state | `transmitting`: a transmission is in progress, release tail included. `muted`, `deafened`. `blocked`: every reason a press would do nothing now, most important first, from `not_connected`, `no_publish_grant`, `no_microphone`, `deafened`, `muted`, `key_device_lost`, `max_transmit`; empty means a press would transmit. |
//! | `press_ignored` | a press did nothing | `reason`: one of the `blocked` values. |
//! | `unknown_command` | a line on standard input was not a command | none. The line is not repeated. |
//! | `presence` | on every change of who is in the channel's voice room, from `conchd` | `known`: false while `conchd`'s presence socket is not open, and then the rest is absent. `configured`, `available`: whether `conchd` has LiveKit and can reach it. `participants`: `id` (`p<principal id>`), `principal_id`, `can_publish`, `transmitting` for each, ordered by id. |
//! | `track` | a remote speaker's audio track came or went | `speaker` (`p<principal id>`), `state`: `subscribed` or `unsubscribed`. |
//! | `report` | a transmit report was not delivered at once | `state`: `started` or `stopped`. `problem`: `retrying` (with `attempt` and `of`), `gave_up`, `no_session` (409, not retried) or `rate_limited` (429, not retried). `detail`: the error's text. |
//! | `microphone` | the microphone could not be opened | `detail`: the error's text. |
//! | `output_dropped` | lines were dropped because standard output was not being read, and it is being read again | `lines`: how many objects were not written since the last one that was. |
//! | `stats` | once a second while connected | `frames_sent`: frames of this client's own audio handed to its track since the last `stats`; `frames_sent_total`: since it started. `reports_delivered`, `reports_dropped`: transmit reports since it started. `speakers`: for each remote speaker, `speaker`, and for the audio received from them since the last `stats`: `frames` (10 ms each, silent ones included), `audible_frames` (at or above -60 dB of full scale), `rms` (full scale is 1) and `dominant_hz` (the strongest of the tones `--sink` names, or `null` if none stood out or there was silence); and `frames_total` and `audible_frames_total` since the speaker was first heard. `mix`: `frames`, `audible_frames`, `rms` and `dominant_hz` of what was handed to the sink since the last `stats`; this client's own audio is never in it. |

use std::fmt;
use std::io::Write;
use std::time::Duration;

use conch_voice_api::VoiceTransmitState;
use conch_voice_control::{PttStatus, ShutReason};
use serde_json::{Value, json};

use crate::lines::{LineQueue, QUEUE_LINES};
use crate::presence::Roster;
use crate::receive::{Measured, MixStats};
use crate::reports::ReportProblem;
use crate::secrets::one_line;

/// The state of the voice connection, as shown.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Connection {
    /// Asking `conchd` for a session and joining the room.
    Connecting,
    /// In the room.
    Connected,
    /// The SDK is restoring the connection by itself. Not connected until it has.
    Reconnecting,
    /// Not connected; `conchd` will be asked for a session after `retry_in`.
    Waiting {
        /// Why the connection ended or was not made.
        reason: &'static str,
        /// The error's text, if there was one.
        detail: Option<String>,
        /// How long until `conchd` is asked.
        retry_in: Duration,
    },
    /// Not connected, and not trying again.
    Stopped {
        /// The connection policy's sentence.
        reason: &'static str,
    },
    /// Left the room after `quit`.
    Closed,
}

/// One thing to show.
#[derive(Debug)]
pub enum Event<'a> {
    /// The voice connection changed.
    Connection(&'a Connection),
    /// The user's own state changed.
    Own(PttStatus),
    /// A press of the talk key did nothing.
    PressIgnored(ShutReason),
    /// A line on standard input was not a command.
    UnknownCommand,
    /// Who is in the room changed.
    Presence(&'a Roster),
    /// A remote speaker's track came (`true`) or went.
    Track {
        /// The speaker, as `p<principal id>`.
        speaker: &'a str,
        /// Whether the track was subscribed to, or is gone.
        subscribed: bool,
    },
    /// A transmit report was not delivered at once.
    Report {
        /// The report.
        state: VoiceTransmitState,
        /// What became of it.
        problem: ReportProblem,
        /// The error's text.
        detail: String,
    },
    /// The microphone could not be opened.
    Microphone {
        /// The error's text.
        detail: String,
    },
    /// The once-a-second counts. Written only with `--json`.
    Stats(&'a Stats),
}

/// What a `stats` object reports.
#[derive(Debug, Clone, PartialEq)]
pub struct Stats {
    /// Frames handed to this client's track since the last one.
    pub frames_sent: u64,
    /// Frames handed to this client's track since it started.
    pub frames_sent_total: u64,
    /// Transmit reports `conchd` accepted.
    pub reports_delivered: u64,
    /// Transmit reports given up on or refused.
    pub reports_dropped: u64,
    /// What was received and mixed.
    pub received: MixStats,
}

/// The name a reason has in `--json`.
#[must_use]
pub fn reason_code(reason: ShutReason) -> &'static str {
    match reason {
        ShutReason::NotConnected => "not_connected",
        ShutReason::NoPublishGrant => "no_publish_grant",
        ShutReason::NoMicrophone => "no_microphone",
        ShutReason::Deafened => "deafened",
        ShutReason::Muted => "muted",
        ShutReason::KeyDeviceLost => "key_device_lost",
        ShutReason::MaxTransmit => "max_transmit",
    }
}

fn report_state(state: VoiceTransmitState) -> &'static str {
    match state {
        VoiceTransmitState::Started => "started",
        VoiceTransmitState::Stopped => "stopped",
    }
}

/// A level rounded to what is worth printing.
fn level(rms: f32) -> f64 {
    (f64::from(rms) * 1e5).round() / 1e5
}

fn measured(measured: &Measured) -> Value {
    json!({
        "frames": measured.frames,
        "audible_frames": measured.audible_frames,
        "rms": level(measured.rms),
        "dominant_hz": measured.dominant_hz,
    })
}

impl Event<'_> {
    /// The event as a `--json` object. The fields are documented at the top of this file.
    #[must_use]
    pub fn json(&self) -> Value {
        match self {
            Event::Connection(connection) => match connection {
                Connection::Connecting => json!({"event": "connection", "state": "connecting"}),
                Connection::Connected => json!({"event": "connection", "state": "connected"}),
                Connection::Reconnecting => {
                    json!({"event": "connection", "state": "reconnecting"})
                }
                Connection::Waiting {
                    reason,
                    detail,
                    retry_in,
                } => {
                    let mut object = json!({
                        "event": "connection",
                        "state": "waiting",
                        "reason": reason,
                        "retry_in_ms": u64::try_from(retry_in.as_millis()).unwrap_or(u64::MAX),
                    });
                    if let (Some(detail), Value::Object(fields)) = (detail, &mut object) {
                        fields.insert("detail".into(), json!(detail));
                    }
                    object
                }
                Connection::Stopped { reason } => {
                    json!({"event": "connection", "state": "stopped", "reason": reason})
                }
                Connection::Closed => json!({"event": "connection", "state": "closed"}),
            },
            Event::Own(status) => json!({
                "event": "self",
                "transmitting": status.transmitting,
                "muted": status.muted(),
                "deafened": status.deafened(),
                "blocked": status.blocked.iter().map(reason_code).collect::<Vec<_>>(),
            }),
            Event::PressIgnored(reason) => {
                json!({"event": "press_ignored", "reason": reason_code(*reason)})
            }
            Event::UnknownCommand => json!({"event": "unknown_command"}),
            Event::Presence(Roster::Unknown) => json!({"event": "presence", "known": false}),
            Event::Presence(Roster::Known {
                configured,
                available,
                participants,
            }) => json!({
                "event": "presence",
                "known": true,
                "configured": configured,
                "available": available,
                "participants": participants.iter().map(|participant| json!({
                    "id": format!("p{}", participant.principal_id),
                    "principal_id": participant.principal_id,
                    "can_publish": participant.can_publish,
                    "transmitting": participant.transmitting,
                })).collect::<Vec<_>>(),
            }),
            Event::Track {
                speaker,
                subscribed,
            } => json!({
                "event": "track",
                "speaker": speaker,
                "state": if *subscribed { "subscribed" } else { "unsubscribed" },
            }),
            Event::Report {
                state,
                problem,
                detail,
            } => {
                let mut object = json!({
                    "event": "report",
                    "state": report_state(*state),
                    "detail": detail,
                });
                if let Value::Object(fields) = &mut object {
                    let name = match problem {
                        ReportProblem::Retrying { attempt, of } => {
                            fields.insert("attempt".into(), json!(attempt));
                            fields.insert("of".into(), json!(of));
                            "retrying"
                        }
                        ReportProblem::GaveUp => "gave_up",
                        ReportProblem::NoSession => "no_session",
                        ReportProblem::RateLimited => "rate_limited",
                    };
                    fields.insert("problem".into(), json!(name));
                }
                object
            }
            Event::Microphone { detail } => json!({"event": "microphone", "detail": detail}),
            Event::Stats(stats) => json!({
                "event": "stats",
                "frames_sent": stats.frames_sent,
                "frames_sent_total": stats.frames_sent_total,
                "reports_delivered": stats.reports_delivered,
                "reports_dropped": stats.reports_dropped,
                "speakers": stats.received.speakers.iter().map(|speaker| {
                    let mut object = measured(&speaker.window);
                    if let Value::Object(fields) = &mut object {
                        fields.insert("speaker".into(), json!(speaker.speaker));
                        fields.insert("frames_total".into(), json!(speaker.frames_total));
                        fields.insert(
                            "audible_frames_total".into(),
                            json!(speaker.audible_frames_total),
                        );
                    }
                    object
                }).collect::<Vec<_>>(),
                "mix": measured(&stats.received.mix),
            }),
        }
    }

    /// The event as a plain line, or `None` for an event that is only for `--json`.
    #[must_use]
    pub fn plain(&self) -> Option<String> {
        Some(match self {
            Event::Connection(connection) => match connection {
                Connection::Connecting => "connecting".to_owned(),
                Connection::Connected => "connected".to_owned(),
                Connection::Reconnecting => "not connected: reconnecting".to_owned(),
                Connection::Waiting {
                    reason,
                    detail,
                    retry_in,
                } => {
                    let why = match detail {
                        Some(detail) => format!("{} ({detail})", reason.replace('_', " ")),
                        None => reason.replace('_', " "),
                    };
                    if retry_in.is_zero() {
                        format!("not connected: {why}; asking conchd for a new session")
                    } else {
                        format!(
                            "not connected: {why}; trying again in {:.1} s",
                            retry_in.as_secs_f64()
                        )
                    }
                }
                Connection::Stopped { reason } => format!("stopped: {reason}"),
                Connection::Closed => "left voice".to_owned(),
            },
            Event::Own(status) => {
                if status.transmitting {
                    "you: talking".to_owned()
                } else if status.blocked.is_empty() {
                    "you: ready to talk".to_owned()
                } else {
                    let reasons: Vec<String> = status
                        .blocked
                        .iter()
                        .map(|reason| reason.to_string())
                        .collect();
                    format!("you: {}", reasons.join("; "))
                }
            }
            Event::PressIgnored(reason) => format!("press ignored: {reason}"),
            Event::UnknownCommand => {
                "unknown command: expected down, up, mute, deafen or quit".to_owned()
            }
            Event::Presence(Roster::Unknown) => {
                "in voice: unknown (conchd's presence is not available)".to_owned()
            }
            Event::Presence(Roster::Known {
                configured: false, ..
            }) => "in voice: nobody (voice is not configured on this server)".to_owned(),
            Event::Presence(Roster::Known {
                available: false, ..
            }) => "in voice: unknown (conchd cannot reach LiveKit)".to_owned(),
            Event::Presence(Roster::Known { participants, .. }) => {
                if participants.is_empty() {
                    "in voice: nobody".to_owned()
                } else {
                    let names: Vec<String> = participants
                        .iter()
                        .map(|participant| {
                            let mark = if participant.transmitting {
                                " (talking)"
                            } else if participant.can_publish {
                                ""
                            } else {
                                " (listening only)"
                            };
                            format!("p{}{mark}", participant.principal_id)
                        })
                        .collect();
                    format!("in voice: {}", names.join(", "))
                }
            }
            Event::Report {
                state,
                problem,
                detail,
            } => {
                let state = report_state(*state);
                match problem {
                    ReportProblem::Retrying { attempt, of } => format!(
                        "transmit report ({state}) failed, attempt {attempt} of {of}: {detail}"
                    ),
                    ReportProblem::GaveUp => {
                        format!("transmit report ({state}) given up on: {detail}")
                    }
                    ReportProblem::NoSession | ReportProblem::RateLimited => {
                        format!("transmit report ({state}) refused: {detail}")
                    }
                }
            }
            Event::Microphone { detail } => format!("no microphone: {detail}"),
            Event::Track { .. } | Event::Stats(_) => return None,
        })
    }
}

/// Where an output's lines go.
enum Sink {
    /// Written where they are shown: for tests, whose writer never stalls.
    Direct(Box<dyn Write + Send>),
    /// Handed to a writer thread without waiting.
    Queued(LineQueue),
}

/// The line that stands in for lines that were dropped, with `--json`.
fn dropped_object(lines: u64) -> String {
    json!({"event": "output_dropped", "lines": lines}).to_string()
}

/// The line that stands in for lines that were dropped, in plain mode.
fn dropped_plain(lines: u64) -> String {
    format!("{lines} lines were not written: standard output was not being read")
}

/// Where the events go: standard output, one line each.
pub struct Output {
    json: bool,
    out: Sink,
}

impl fmt::Debug for Output {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Output")
            .field("json", &self.json)
            .finish_non_exhaustive()
    }
}

impl Output {
    /// An output that writes each line to `out` where it is shown: JSON objects if `json`,
    /// plain lines otherwise. For tests: whoever shows an event waits for `out`.
    #[must_use]
    pub fn new(json: bool, out: Box<dyn Write + Send>) -> Self {
        Self {
            json,
            out: Sink::Direct(out),
        }
    }

    /// An output whose lines are written to `out` by a thread of their own. Showing an
    /// event never waits for `out`: when [`QUEUE_LINES`] lines are waiting, further ones
    /// are dropped, and a line says how many when there is room again.
    #[must_use]
    pub fn queued(json: bool, out: Box<dyn Write + Send>) -> Self {
        let marker: fn(u64) -> String = if json { dropped_object } else { dropped_plain };
        Self {
            json,
            out: Sink::Queued(LineQueue::spawn(out, QUEUE_LINES, Box::new(marker))),
        }
    }

    /// An output on standard output, queued: a reader that stops reading cannot stop
    /// whoever shows an event.
    #[must_use]
    pub fn stdout(json: bool) -> Self {
        Self::queued(json, Box::new(std::io::stdout()))
    }

    /// Waits until every line shown so far has been written, or `limit` has passed. True
    /// if they were written. A flush that times out is not an error: the reader was not
    /// reading, and the client goes on to exit.
    pub async fn flush(&mut self, limit: Duration) -> bool {
        match &mut self.out {
            Sink::Direct(out) => out.flush().is_ok(),
            Sink::Queued(queue) => queue.flush(limit).await,
        }
    }

    /// Whether events are written as JSON.
    #[must_use]
    pub fn is_json(&self) -> bool {
        self.json
    }

    /// Writes one event as one line, flushed, so a reader sees it when it happens. With a
    /// queued output this hands the line over and returns at once, whatever the reader is
    /// doing.
    pub fn show(&mut self, event: &Event<'_>) {
        let line = if self.json {
            // An object, so the line begins with `{`.
            event.json().to_string()
        } else {
            match event.plain() {
                // A detail in it is an error's text. Each source already makes its text
                // printable; this is the one place every line passes, so it is made sure
                // of here that a line is one line and drives no terminal.
                Some(line) => one_line(&line),
                None => return,
            }
        };
        match &mut self.out {
            Sink::Direct(out) => {
                // If the reader has gone away there is nobody to tell; the client carries
                // on, and ends when its standard input does.
                let _ = writeln!(out, "{line}");
                let _ = out.flush();
            }
            Sink::Queued(queue) => queue.push(line),
        }
    }
}

/// A participant's identity made fit to show: `conchd` sets it to `p<principal id>`, and
/// anything else LiveKit might hand over is cut down to letters, digits, `-` and `_`.
#[must_use]
pub fn speaker_label(identity: &str) -> String {
    let label: String = identity
        .chars()
        .take(32)
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '-' | '_') {
                c
            } else {
                '?'
            }
        })
        .collect();
    if label.is_empty() {
        "?".to_owned()
    } else {
        label
    }
}

#[cfg(test)]
mod tests {
    use conch_voice_control::{LineCommand, Ptt, PttInput, PttOutput};

    use super::*;
    use crate::presence::Participant;
    use crate::receive::SpeakerStats;

    fn status_after(inputs: &[PttInput]) -> PttStatus {
        let (mut ptt, _) = Ptt::new(Duration::from_secs(120));
        let mut last = ptt.status();
        for input in inputs {
            for output in ptt.handle(Duration::ZERO, *input) {
                if let PttOutput::Status(status) = output {
                    last = status;
                }
            }
        }
        last
    }

    #[test]
    fn every_event_is_one_json_object_that_names_itself() {
        let roster = Roster::Known {
            configured: true,
            available: true,
            participants: vec![Participant {
                principal_id: 3,
                can_publish: true,
                transmitting: true,
            }],
        };
        let stats = Stats {
            frames_sent: 100,
            frames_sent_total: 250,
            reports_delivered: 4,
            reports_dropped: 0,
            received: MixStats {
                speakers: vec![SpeakerStats {
                    speaker: "p3".into(),
                    window: Measured {
                        frames: 100,
                        audible_frames: 99,
                        rms: 0.176_776_7,
                        dominant_hz: Some(440.0),
                    },
                    frames_total: 300,
                    audible_frames_total: 199,
                }],
                mix: Measured {
                    frames: 100,
                    audible_frames: 0,
                    rms: 0.0,
                    dominant_hz: None,
                },
            },
        };
        let waiting = Connection::Waiting {
            reason: "connection_lost",
            detail: Some("the \"server\"\nwent away".into()),
            retry_in: Duration::from_millis(750),
        };
        let events = [
            Event::Connection(&Connection::Connecting),
            Event::Connection(&waiting),
            Event::Connection(&Connection::Stopped { reason: "why" }),
            Event::Own(status_after(&[])),
            Event::PressIgnored(ShutReason::NotConnected),
            Event::UnknownCommand,
            Event::Presence(&roster),
            Event::Presence(&Roster::Unknown),
            Event::Track {
                speaker: "p3",
                subscribed: true,
            },
            Event::Report {
                state: VoiceTransmitState::Started,
                problem: ReportProblem::Retrying { attempt: 1, of: 3 },
                detail: "conchd did not answer within the timeout".into(),
            },
            Event::Microphone {
                detail: "no such file".into(),
            },
            Event::Stats(&stats),
        ];
        for event in &events {
            let written = Buffer::default();
            Output::new(true, Box::new(written.clone())).show(event);
            let line = written.text();
            assert!(line.starts_with('{'), "{line}");
            assert!(line.ends_with("}\n"), "{line}");
            assert_eq!(line.matches('\n').count(), 1, "one line: {line}");
            let object: Value = serde_json::from_str(&line).unwrap();
            assert!(object["event"].is_string(), "{line}");
        }
    }

    /// What an [`Output`] wrote, shared with the test.
    #[derive(Clone, Default)]
    struct Buffer(std::sync::Arc<std::sync::Mutex<Vec<u8>>>);

    impl Buffer {
        fn text(&self) -> String {
            String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
        }
    }

    impl Write for Buffer {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }

        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn plain_mode_writes_one_line_per_change_and_nothing_for_stats_or_tracks() {
        let written = Buffer::default();
        let mut output = Output::new(false, Box::new(written.clone()));
        output.show(&Event::Connection(&Connection::Connecting));
        output.show(&Event::Track {
            speaker: "p3",
            subscribed: true,
        });
        output.show(&Event::PressIgnored(ShutReason::NotConnected));
        assert_eq!(written.text(), "connecting\npress ignored: not connected\n");
    }

    #[test]
    fn a_plain_line_is_one_line_whatever_an_errors_text_holds() {
        let written = Buffer::default();
        let mut output = Output::new(false, Box::new(written.clone()));
        output.show(&Event::Connection(&Connection::Waiting {
            reason: "connect_failed",
            detail: Some("refused\r\nconnected\x1b[2J".into()),
            retry_in: Duration::ZERO,
        }));
        output.show(&Event::Microphone {
            detail: "no\nsuch file".into(),
        });
        assert_eq!(
            written.text(),
            "not connected: connect failed (refused  connected [2J); asking conchd for a new session\n\
             no microphone: no such file\n"
        );
    }

    #[test]
    fn the_stats_object_has_the_documented_fields() {
        let stats = Stats {
            frames_sent: 0,
            frames_sent_total: 12,
            reports_delivered: 2,
            reports_dropped: 1,
            received: MixStats {
                speakers: vec![SpeakerStats {
                    speaker: "p3".into(),
                    window: Measured {
                        frames: 100,
                        audible_frames: 99,
                        rms: 0.176_776_7,
                        dominant_hz: Some(440.0),
                    },
                    frames_total: 300,
                    audible_frames_total: 199,
                }],
                mix: Measured {
                    frames: 100,
                    audible_frames: 0,
                    rms: 0.0,
                    dominant_hz: None,
                },
            },
        };
        assert_eq!(
            Event::Stats(&stats).json(),
            json!({
                "event": "stats",
                "frames_sent": 0,
                "frames_sent_total": 12,
                "reports_delivered": 2,
                "reports_dropped": 1,
                "speakers": [{
                    "speaker": "p3",
                    "frames": 100,
                    "audible_frames": 99,
                    "rms": 0.17678,
                    "dominant_hz": 440.0,
                    "frames_total": 300,
                    "audible_frames_total": 199,
                }],
                "mix": {"frames": 100, "audible_frames": 0, "rms": 0.0, "dominant_hz": null},
            })
        );
    }

    #[test]
    fn the_users_own_state_names_every_reason_a_press_would_do_nothing() {
        let fresh = Event::Own(status_after(&[])).json();
        assert_eq!(
            fresh,
            json!({
                "event": "self",
                "transmitting": false,
                "muted": false,
                "deafened": false,
                "blocked": ["not_connected", "no_publish_grant", "no_microphone"],
            })
        );
        let ready = status_after(&[
            PttInput::Connected(true),
            PttInput::PublishGrant(true),
            PttInput::Microphone(true),
        ]);
        assert_eq!(Event::Own(ready).plain().unwrap(), "you: ready to talk");
        let talking = status_after(&[
            PttInput::Connected(true),
            PttInput::PublishGrant(true),
            PttInput::Microphone(true),
            PttInput::Line(LineCommand::Down),
        ]);
        assert_eq!(Event::Own(talking).plain().unwrap(), "you: talking");
        assert_eq!(Event::Own(talking).json()["transmitting"], json!(true));
        let muted = status_after(&[
            PttInput::Connected(true),
            PttInput::PublishGrant(true),
            PttInput::Microphone(true),
            PttInput::Line(LineCommand::Mute),
        ]);
        assert_eq!(Event::Own(muted).plain().unwrap(), "you: muted");
        assert_eq!(Event::Own(muted).json()["blocked"], json!(["muted"]));
    }

    #[test]
    fn presence_is_shown_as_principal_ids_with_who_is_talking() {
        let roster = Roster::Known {
            configured: true,
            available: true,
            participants: vec![
                Participant {
                    principal_id: 3,
                    can_publish: true,
                    transmitting: true,
                },
                Participant {
                    principal_id: 7,
                    can_publish: true,
                    transmitting: false,
                },
                Participant {
                    principal_id: 9,
                    can_publish: false,
                    transmitting: false,
                },
            ],
        };
        assert_eq!(
            Event::Presence(&roster).plain().unwrap(),
            "in voice: p3 (talking), p7, p9 (listening only)"
        );
        let object = Event::Presence(&roster).json();
        assert_eq!(object["participants"][0]["id"], json!("p3"));
        assert_eq!(object["participants"][0]["transmitting"], json!(true));
        assert_eq!(object["participants"][2]["can_publish"], json!(false));
    }

    #[test]
    fn stats_and_tracks_are_not_plain_lines() {
        assert!(
            Event::Track {
                speaker: "p3",
                subscribed: true
            }
            .plain()
            .is_none()
        );
    }

    #[test]
    fn an_identity_that_is_not_a_principal_id_is_cut_down_before_it_is_shown() {
        assert_eq!(speaker_label("p7"), "p7");
        assert_eq!(speaker_label("p7\u{1b}[31m red"), "p7??31m?red");
        assert_eq!(speaker_label(""), "?");
        assert_eq!(speaker_label(&"x".repeat(100)).len(), 32);
    }

    /// Shows events to a queued output whose reader is not reading, far more than the
    /// queue holds, then lets the reader read and shows one more. Returns what was written.
    async fn stalled_then_read(json: bool) -> Vec<String> {
        use crate::lines::tests::Stallable;
        let writer = Stallable::stalled();
        let mut output = Output::queued(json, Box::new(writer.clone()));
        output.show(&Event::Connection(&Connection::Connecting));
        writer.until_waiting();

        let began = std::time::Instant::now();
        for _ in 0..QUEUE_LINES + 500 {
            output.show(&Event::UnknownCommand);
        }
        assert!(
            began.elapsed() < Duration::from_secs(2),
            "showing an event waited for the reader"
        );
        assert!(!output.flush(Duration::from_millis(50)).await);
        assert_eq!(writer.text(), "");

        writer.stall(false);
        writer.until_lines(1 + QUEUE_LINES);
        output.show(&Event::Connection(&Connection::Connected));
        assert!(output.flush(Duration::from_secs(10)).await);
        writer.text().lines().map(str::to_owned).collect()
    }

    #[tokio::test]
    async fn a_queued_output_drops_lines_a_stalled_reader_does_not_take_and_then_says_so() {
        let lines = stalled_then_read(true).await;
        assert_eq!(lines.len(), 1 + QUEUE_LINES + 2);
        assert_eq!(lines[0], r#"{"event":"connection","state":"connecting"}"#);
        assert_eq!(lines[QUEUE_LINES], r#"{"event":"unknown_command"}"#);
        // The first thing written after the gap says how wide it was.
        assert_eq!(
            lines[QUEUE_LINES + 1],
            r#"{"event":"output_dropped","lines":500}"#
        );
        assert_eq!(
            lines[QUEUE_LINES + 2],
            r#"{"event":"connection","state":"connected"}"#
        );
        for line in &lines {
            assert!(line.starts_with('{'), "{line}");
        }
    }

    #[tokio::test]
    async fn in_plain_mode_the_dropped_lines_are_said_in_words() {
        let lines = stalled_then_read(false).await;
        assert_eq!(lines.len(), 1 + QUEUE_LINES + 2);
        assert_eq!(
            lines[QUEUE_LINES + 1],
            "500 lines were not written: standard output was not being read"
        );
        assert_eq!(lines[QUEUE_LINES + 2], "connected");
    }

    #[tokio::test]
    async fn a_direct_output_has_nothing_to_wait_for_when_flushed() {
        let written = Buffer::default();
        let mut output = Output::new(true, Box::new(written.clone()));
        output.show(&Event::UnknownCommand);
        assert!(output.flush(Duration::ZERO).await);
        assert_eq!(written.text(), "{\"event\":\"unknown_command\"}\n");
    }
}
