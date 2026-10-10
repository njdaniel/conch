//! The status display (`docs/design/conch-voice.md` §7): rendered to lines and to a buffer
//! and compared, for each state it shows; and that nothing in a line, an error's text
//! least of all, can move the cursor.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::time::Duration;

use conch_voice::keydev::{DeviceState, OsError, Problem, Refusal};
use conch_voice::output::{Connection, Event, Output, Stats};
use conch_voice::presence::{Participant, Roster};
use conch_voice::receive::{Measured, MixStats};
use conch_voice::reports::ReportProblem;
use conch_voice::status::{LINES, Screen, State, frame, render};
use conch_voice_api::VoiceTransmitState;
use conch_voice_control::{
    Key, KeyAction, KeyEvent, LineCommand, Ptt, PttInput, PttOutput, PttStatus, ShutReason,
};

use support::Written;

const DEVICE: &str = "/dev/input/by-id/usb-Example_Keyboard-event-kbd";

/// The user's own state after these inputs, from the real machine.
fn own(inputs: &[PttInput]) -> PttStatus {
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

/// Connected, allowed to publish, with a microphone.
const READY: [PttInput; 3] = [
    PttInput::Connected(true),
    PttInput::PublishGrant(true),
    PttInput::Microphone(true),
];

fn after_ready(more: &[PttInput]) -> PttStatus {
    own(&[&READY[..], more].concat())
}

const TALK_DOWN: PttInput = PttInput::Key(KeyEvent {
    key: Key::Talk,
    action: KeyAction::Press,
});

fn denied() -> Problem {
    Problem::CannotOpen(OsError::from(&std::io::Error::from_raw_os_error(13)))
}

fn roster() -> Roster {
    let participant = |principal_id, can_publish, transmitting| Participant {
        principal_id,
        can_publish,
        transmitting,
    };
    Roster::Known {
        configured: true,
        available: true,
        participants: vec![
            participant(3, true, true),
            participant(7, true, false),
            participant(9, false, false),
        ],
    }
}

/// The lines after these events.
fn lines(events: &[Event<'_>]) -> [String; LINES] {
    let mut state = State::default();
    for event in events {
        assert!(state.apply(event), "{event:?}");
    }
    render(&state)
}

const NOBODY_KNOWN: &str = "in voice: unknown (conchd's presence is not available)";

#[test]
fn before_anything_has_happened_the_status_says_so() {
    assert_eq!(
        render(&State::default()),
        ["voice: starting", "you: not connected", NOBODY_KNOWN, ""]
    );
}

#[test]
fn the_connection_line_shows_each_state_of_the_connection() {
    let waiting = Connection::Waiting {
        reason: "connect_failed",
        detail: Some("the server refused".into()),
        retry_in: Duration::from_millis(1500),
    };
    let asking = Connection::Waiting {
        reason: "room_deleted",
        detail: None,
        retry_in: Duration::ZERO,
    };
    for (connection, expected) in [
        (Connection::Connecting, "voice: connecting"),
        (Connection::Connected, "voice: connected"),
        (
            Connection::Reconnecting,
            "voice: not connected: reconnecting",
        ),
        (
            waiting,
            "voice: not connected: connect failed (the server refused); trying again in 1.5 s",
        ),
        (
            asking,
            "voice: not connected: room deleted; asking conchd for a new session",
        ),
        (
            Connection::Stopped {
                reason: "not a member of this channel",
            },
            "voice: stopped: not a member of this channel",
        ),
        (Connection::Closed, "voice: left voice"),
    ] {
        assert_eq!(lines(&[Event::Connection(&connection)])[0], expected);
    }
}

#[test]
fn the_own_line_shows_each_state_of_the_user() {
    let lost = [TALK_DOWN, PttInput::KeyDevice(false), PttInput::GateShut];
    let retrying = Event::Report {
        state: VoiceTransmitState::Started,
        problem: ReportProblem::Retrying { attempt: 1, of: 3 },
        detail: "conchd did not answer within the timeout".into(),
    };
    let gave_up = Event::Report {
        state: VoiceTransmitState::Stopped,
        problem: ReportProblem::GaveUp,
        detail: "conchd did not answer within the timeout".into(),
    };
    let refused = Event::Report {
        state: VoiceTransmitState::Started,
        problem: ReportProblem::NoSession,
        detail: "no voice session".into(),
    };
    let missing = Event::KeyDevice {
        device: DEVICE,
        state: DeviceState::Missing(denied()),
    };
    for (what, events, expected) in [
        (
            "not connected",
            vec![Event::Own(own(&[]))],
            "you: not connected; listening only: this session may not transmit; no microphone",
        ),
        (
            "ready",
            vec![Event::Own(after_ready(&[]))],
            "you: ready to talk",
        ),
        (
            "talking",
            vec![Event::Own(after_ready(&[TALK_DOWN]))],
            "you: talking",
        ),
        (
            "muted",
            vec![Event::Own(after_ready(&[PttInput::Line(
                LineCommand::Mute,
            )]))],
            "you: muted",
        ),
        (
            "deafened",
            vec![Event::Own(after_ready(&[PttInput::Line(
                LineCommand::Deafen,
            )]))],
            "you: deafened",
        ),
        (
            "deafened and muted",
            vec![Event::Own(after_ready(&[
                PttInput::Line(LineCommand::Mute),
                PttInput::Line(LineCommand::Deafen),
            ]))],
            "you: deafened; muted",
        ),
        (
            "no microphone",
            vec![Event::Own(after_ready(&[PttInput::Microphone(false)]))],
            "you: no microphone",
        ),
        (
            "not connected again",
            vec![Event::Own(after_ready(&[PttInput::Connected(false)]))],
            "you: not connected",
        ),
        (
            "listening only",
            vec![Event::Own(after_ready(&[PttInput::PublishGrant(false)]))],
            "you: listening only: this session may not transmit",
        ),
        (
            "the key device was lost",
            vec![Event::Own(after_ready(&lost))],
            "you: key device missing",
        ),
        (
            "the key device was lost while muted",
            vec![Event::Own(after_ready(
                &[&[PttInput::Line(LineCommand::Mute)][..], &lost[..]].concat(),
            ))],
            "you: muted; key device missing",
        ),
        (
            "the key device was never there, so a press from standard input would transmit",
            vec![Event::Own(after_ready(&[])), missing],
            "you: ready to talk; key device missing",
        ),
        (
            "a report is failing",
            vec![Event::Own(after_ready(&[TALK_DOWN])), retrying],
            "you: talking; transmit report failing (attempt 1 of 3)",
        ),
        (
            "a report was given up on",
            vec![Event::Own(after_ready(&[])), gave_up],
            "you: ready to talk; transmit report not delivered",
        ),
        (
            "a report was refused",
            vec![Event::Own(after_ready(&[])), refused],
            "you: ready to talk; transmit report refused",
        ),
    ] {
        assert_eq!(lines(&events)[1], expected, "{what}");
    }
}

#[test]
fn a_failed_report_is_forgotten_when_the_next_transmission_starts() {
    let mut state = State::default();
    state.apply(&Event::Own(after_ready(&[])));
    state.apply(&Event::Report {
        state: VoiceTransmitState::Stopped,
        problem: ReportProblem::GaveUp,
        detail: "timeout".into(),
    });
    assert_eq!(
        render(&state)[1],
        "you: ready to talk; transmit report not delivered"
    );
    // A change that is not a new press leaves it.
    state.apply(&Event::Own(after_ready(&[PttInput::Line(
        LineCommand::Mute,
    )])));
    assert_eq!(
        render(&state)[1],
        "you: muted; transmit report not delivered"
    );
    state.apply(&Event::Own(after_ready(&[TALK_DOWN])));
    assert_eq!(render(&state)[1], "you: talking");
}

#[test]
fn a_key_device_that_is_back_is_no_longer_missing() {
    let mut state = State::default();
    state.apply(&Event::Own(after_ready(&[])));
    state.apply(&Event::KeyDevice {
        device: DEVICE,
        state: DeviceState::Missing(denied()),
    });
    assert_eq!(render(&state)[1], "you: ready to talk; key device missing");
    assert_eq!(
        render(&state)[3],
        format!(
            "key device {DEVICE}: cannot open it: {}; taking down, up, mute, deafen and quit \
             from standard input, and trying again",
            std::io::Error::from_raw_os_error(13)
        )
    );
    state.apply(&Event::KeyDevice {
        device: DEVICE,
        state: DeviceState::Ready,
    });
    assert_eq!(render(&state)[1], "you: ready to talk");
    assert_eq!(render(&state)[3], format!("key device {DEVICE}: open"));

    // A device that is refused is not tried again, and the line does not say it is.
    state.apply(&Event::KeyDevice {
        device: DEVICE,
        state: DeviceState::Missing(Problem::Refused(Refusal::NotCharacterDevice)),
    });
    assert_eq!(
        render(&state)[3],
        format!(
            "key device {DEVICE}: refused: it is not a character device; taking down, up, \
             mute, deafen and quit from standard input"
        )
    );
}

#[test]
fn the_roster_line_marks_whoever_conchd_says_is_transmitting() {
    assert_eq!(
        lines(&[Event::Presence(&roster())])[2],
        "in voice: p3 (talking), p7, p9 (listening only)"
    );
    let nobody = Roster::Known {
        configured: true,
        available: true,
        participants: Vec::new(),
    };
    assert_eq!(lines(&[Event::Presence(&nobody)])[2], "in voice: nobody");
    assert_eq!(lines(&[Event::Presence(&Roster::Unknown)])[2], NOBODY_KNOWN);
}

#[test]
fn the_last_line_is_the_last_thing_that_was_said_once() {
    for (event, expected) in [
        (
            Event::PressIgnored(ShutReason::Muted),
            "press ignored: muted",
        ),
        (
            Event::UnknownCommand,
            "unknown command: expected down, up, mute, deafen or quit",
        ),
        (
            Event::Microphone {
                detail: "no such file".into(),
            },
            "no microphone: no such file",
        ),
        (
            Event::Report {
                state: VoiceTransmitState::Started,
                problem: ReportProblem::Retrying { attempt: 2, of: 3 },
                detail: "timeout".into(),
            },
            "transmit report (started) failed, attempt 2 of 3: timeout",
        ),
    ] {
        assert_eq!(lines(&[event])[3], expected);
    }
}

#[test]
fn what_the_status_does_not_show_changes_nothing() {
    let stats = Stats {
        frames_sent: 1,
        frames_sent_total: 2,
        reports_delivered: 0,
        reports_dropped: 0,
        received: MixStats {
            speakers: Vec::new(),
            mix: Measured {
                frames: 0,
                audible_frames: 0,
                rms: 0.0,
                dominant_hz: None,
            },
        },
    };
    let mut state = State::default();
    assert!(!state.apply(&Event::Stats(&stats)));
    assert!(!state.apply(&Event::Track {
        speaker: "p3",
        subscribed: true
    }));
    assert_eq!(state, State::default());
}

const ESCAPE: char = '\u{1b}';

/// What was written, with this program's own sequences taken out: what is left is the
/// text, and must hold no control character but the line ends.
fn text_of(written: &str) -> String {
    let mut text = written.to_owned();
    for own in [
        "\u{1b}7",
        "\u{1b}8",
        "\u{1b}[4A\r",
        "\u{1b}[2K",
        "\u{1b}[?7l",
        "\u{1b}[?7h",
    ] {
        text = text.replace(own, "");
    }
    text
}

#[test]
fn a_frame_is_the_lines_drawn_where_the_cursor_is_or_over_the_last_ones() {
    let lines = [
        "voice: connected".to_owned(),
        "you: talking".to_owned(),
        "in voice: p3 (talking), p7".to_owned(),
        String::new(),
    ];
    assert_eq!(
        frame(&lines, false),
        "\u{1b}[?7l\
         \u{1b}[2Kvoice: connected\n\
         \u{1b}[2Kyou: talking\n\
         \u{1b}[2Kin voice: p3 (talking), p7\n\
         \u{1b}[2K\n\
         \u{1b}[?7h"
    );
    assert_eq!(
        frame(&lines, true),
        "\u{1b}7\u{1b}[4A\r\u{1b}[?7l\
         \u{1b}[2Kvoice: connected\n\
         \u{1b}[2Kyou: talking\n\
         \u{1b}[2Kin voice: p3 (talking), p7\n\
         \u{1b}[2K\n\
         \u{1b}[?7h\u{1b}8"
    );
    assert_eq!(LINES, 4, "the distance back up is the number of lines");
}

#[test]
fn an_errors_text_cannot_move_the_cursor_or_add_a_line() {
    let hostile = "refused\r\nvoice: connected\u{1b}[2J\u{1b}[5A\u{7}\u{8}\tend\u{9b}3A";
    let waiting = Connection::Waiting {
        reason: "connect_failed",
        detail: Some(hostile.into()),
        retry_in: Duration::ZERO,
    };
    let written = Written::default();
    let mut output = Output::redrawn(Screen::new(), Box::new(written.clone()));
    output.show(&Event::Connection(&Connection::Connecting));
    output.show(&Event::Connection(&waiting));
    output.show(&Event::Microphone {
        detail: hostile.into(),
    });
    output.show(&Event::KeyDevice {
        device: "/dev/input/by-id/evil\u{1b}[2J\nname",
        state: DeviceState::Missing(denied()),
    });

    let written = written.text();
    let text = text_of(&written);
    assert!(
        text.chars().all(|c| c == '\n' || !c.is_control()),
        "a control character got through: {text:?}"
    );
    // Four frames of four lines each, and not a line more.
    assert_eq!(text.matches('\n').count(), 4 * LINES, "{text:?}");
    assert_eq!(written.matches("\u{1b}[4A\r").count(), 3, "three redraws");
    // The text is there, with each control character turned into a space.
    assert!(
        text.contains(
            "voice: not connected: connect failed (refused  voice: connected [2J [5A   end 3A); \
             asking conchd for a new session\n"
        ),
        "{text:?}"
    );
    assert!(
        text.contains("key device /dev/input/by-id/evil [2J name: cannot open it"),
        "{text:?}"
    );
}

#[test]
fn the_status_is_drawn_once_and_then_redrawn_in_place_only_when_it_changes() {
    let written = Written::default();
    let mut output = Output::redrawn(Screen::new(), Box::new(written.clone()));
    assert!(!output.is_json());

    output.show(&Event::Connection(&Connection::Connecting));
    let first = written.text();
    assert_eq!(
        first,
        frame(
            &[
                "voice: connecting".to_owned(),
                "you: not connected".to_owned(),
                NOBODY_KNOWN.to_owned(),
                String::new()
            ],
            false
        )
    );
    assert!(!first.contains("\u{1b}[4A"), "nothing to go back up to yet");

    // The same again, and events the status does not show: nothing is written.
    output.show(&Event::Connection(&Connection::Connecting));
    output.show(&Event::Track {
        speaker: "p3",
        subscribed: true,
    });
    assert_eq!(written.text(), first);

    output.show(&Event::Connection(&Connection::Connected));
    output.show(&Event::Own(after_ready(&[TALK_DOWN])));
    output.show(&Event::Presence(&roster()));
    let last = frame(
        &[
            "voice: connected".to_owned(),
            "you: talking".to_owned(),
            "in voice: p3 (talking), p7, p9 (listening only)".to_owned(),
            String::new(),
        ],
        true,
    );
    let all = written.text();
    assert!(all.ends_with(&last), "{all:?}");
    assert_eq!(
        all.matches("\u{1b}[4A\r").count(),
        3,
        "each change in place"
    );
    assert_eq!(
        all.matches(ESCAPE).count(),
        first.matches(ESCAPE).count() + 3 * 9
    );
}

#[test]
fn a_line_the_user_typed_puts_the_next_status_below_it() {
    let written = Written::default();
    let mut output = Output::redrawn(Screen::echoing(), Box::new(written.clone()));
    output.show(&Event::Connection(&Connection::Connecting));
    // The terminal echoed the line under the status, so the cursor is no longer there.
    output.typed_line();
    output.show(&Event::UnknownCommand);
    let all = written.text();
    assert!(
        !all.contains("\u{1b}[4A"),
        "drawn below the typed line, not over it: {all:?}"
    );
    assert_eq!(text_of(&all).matches('\n').count(), 2 * LINES);
    // And after that, in place again.
    output.show(&Event::Connection(&Connection::Connected));
    assert_eq!(written.text().matches("\u{1b}[4A\r").count(), 1);

    // Where nothing is echoed (standard input is not a terminal) a line moves nothing.
    let written = Written::default();
    let mut output = Output::redrawn(Screen::new(), Box::new(written.clone()));
    output.show(&Event::Connection(&Connection::Connecting));
    output.typed_line();
    output.show(&Event::UnknownCommand);
    assert_eq!(written.text().matches("\u{1b}[4A\r").count(), 1);
}

#[test]
fn off_a_terminal_and_with_json_the_output_is_line_by_line_with_no_escape_sequence() {
    // `--json` never draws, whatever standard output is.
    assert!(Screen::for_stdout(true).is_none());

    let missing = Event::KeyDevice {
        device: DEVICE,
        state: DeviceState::Missing(denied()),
    };
    for json in [true, false] {
        let written = Written::default();
        let mut output = Output::new(json, Box::new(written.clone()));
        output.typed_line();
        output.show(&Event::Connection(&Connection::Connecting));
        output.show(&missing);
        output.show(&Event::KeyDevice {
            device: DEVICE,
            state: DeviceState::Ready,
        });
        let text = written.text();
        assert!(!text.contains(ESCAPE), "{text:?}");
        assert_eq!(text.lines().count(), 3, "one line an event: {text:?}");
        if json {
            let events = written.events();
            assert_eq!(
                events[1],
                serde_json::json!({
                    "event": "key_device",
                    "device": DEVICE,
                    "state": "missing",
                    "reason": "cannot_open",
                    "detail": format!("cannot open it: {}", std::io::Error::from_raw_os_error(13)),
                    "retrying": true,
                })
            );
            assert_eq!(
                events[2],
                serde_json::json!({"event": "key_device", "device": DEVICE, "state": "open"})
            );
        } else {
            assert_eq!(
                text,
                format!(
                    "connecting\n\
                     key device {DEVICE}: cannot open it: {}; taking down, up, mute, deafen and \
                     quit from standard input, and trying again\n\
                     key device {DEVICE}: open\n",
                    std::io::Error::from_raw_os_error(13)
                )
            );
        }
    }
}
