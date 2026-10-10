//! The status display: on a terminal, a few lines redrawn in place
//! (`docs/design/conch-voice.md` §7).
//!
//! Input: the same [`Event`]s that are written one per line when standard output is not a
//! terminal. Output: text for the terminal, with plain ANSI sequences and no terminal
//! library. Owns: what is currently shown ([`State`]), and where the cursor is relative to
//! it ([`Screen`]).
//!
//! It is in three parts, so that what is drawn can be compared in a test:
//!
//! - [`State`] is everything the display shows, and [`State::apply`] folds an event into it.
//! - [`render`] is a pure function from a state to [`LINES`] lines of text.
//! - [`frame`] turns those lines into what is written, and is the one place a drawn line
//!   passes: each goes through [`one_line`], so an error's text cannot move the cursor.
//!
//! [`Screen`] is the terminal handling, and all of it: whether standard output is a
//! terminal ([`Screen::for_stdout`]), and whether the lines are drawn over the last ones or
//! below them. The display is always [`LINES`] lines, so "over the last ones" is always the
//! same distance up.
//!
//! The cursor rests on the line under the status. When something else has written there (a
//! command the user typed, which the terminal echoed; a log record on standard error), the
//! next status is drawn below it, not over it, and the old one is left behind as history.

use std::io::IsTerminal;
use std::sync::atomic::{AtomicBool, Ordering};

use conch_voice_control::{PttStatus, ShutReason};

use crate::keydev::DeviceState;
use crate::output::{Connection, Event};
use crate::presence::Roster;
use crate::reports::ReportProblem;
use crate::secrets::one_line;

/// How many lines the status is: the connection, the user's own state, who is in voice,
/// and the last thing that was said once.
pub const LINES: usize = 4;

/// Save and restore the cursor (DECSC, DECRC), so a redraw leaves it where the user is
/// typing.
const SAVE_CURSOR: &str = "\x1b7";
const RESTORE_CURSOR: &str = "\x1b8";
/// Erase the whole line the cursor is on.
const ERASE_LINE: &str = "\x1b[2K";
/// Switch wrapping at the right margin off and on (DECAWM). While it is off a line longer
/// than the terminal is wide is cut there, so every line drawn is one row, whatever its
/// length, and the distance back up is always [`LINES`].
const WRAP_OFF: &str = "\x1b[?7l";
const WRAP_ON: &str = "\x1b[?7h";

/// Set when a log record has been written to standard error, which on a terminal moves the
/// cursor down from under the status.
static DISTURBED: AtomicBool = AtomicBool::new(false);

/// Called by the logger after it has written a record.
pub fn disturbed() {
    DISTURBED.store(true, Ordering::Relaxed);
}

/// Everything the status shows.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct State {
    /// The voice connection. `None` before the first attempt.
    pub connection: Option<Connection>,
    /// The user's own state, from the push-to-talk machine.
    pub own: Option<PttStatus>,
    /// The key device, if one is watched: being read, or not and why.
    pub key_device: Option<DeviceState>,
    /// What became of the last transmit report that was not delivered at once. Forgotten
    /// when the next transmission starts.
    pub report: Option<ReportProblem>,
    /// Who is in the channel's voice room, as `conchd` has it.
    pub roster: Option<Roster>,
    /// The last thing that is said once and is not a state: a refused press, a line that
    /// was not a command, why the key device or the microphone is not there, a report's
    /// failure.
    pub notice: Option<String>,
}

impl State {
    /// Takes one event in. Returns false for an event the status does not show.
    pub fn apply(&mut self, event: &Event<'_>) -> bool {
        match event {
            Event::Connection(connection) => self.connection = Some((*connection).clone()),
            Event::Own(status) => {
                let was_talking = self.own.is_some_and(|own| own.transmitting);
                if status.transmitting && !was_talking {
                    // A new transmission has reports of its own.
                    self.report = None;
                }
                self.own = Some(*status);
            }
            Event::Presence(roster) => self.roster = Some((*roster).clone()),
            Event::KeyDevice { state, .. } => {
                self.key_device = Some(*state);
                self.notice = event.plain();
            }
            Event::Report { problem, .. } => {
                self.report = Some(*problem);
                self.notice = event.plain();
            }
            Event::PressIgnored(_) | Event::UnknownCommand | Event::Microphone { .. } => {
                self.notice = event.plain();
            }
            Event::Track { .. } | Event::Stats(_) => return false,
        }
        true
    }
}

/// The user's own state: talking, or every reason a press would do nothing, and then what
/// is wrong that a press does not depend on.
fn own_line(state: &State) -> String {
    let mut parts: Vec<String> = Vec::new();
    let lost = state
        .own
        .is_some_and(|own| own.blocked.contains(ShutReason::KeyDeviceLost));
    match state.own {
        Some(own) if own.transmitting => parts.push("talking".to_owned()),
        Some(own) => {
            parts.extend(
                own.blocked
                    .iter()
                    .filter(|reason| *reason != ShutReason::KeyDeviceLost)
                    .map(|reason| reason.to_string()),
            );
            if parts.is_empty() && !lost {
                parts.push("ready to talk".to_owned());
            }
        }
        None => parts.push(ShutReason::NotConnected.to_string()),
    }
    // Said the same way whether the device was lost or was never there.
    if lost || state.key_device.is_some_and(|device| !device.is_ready()) {
        parts.push("key device missing".to_owned());
    }
    match state.report {
        Some(ReportProblem::Retrying { attempt, of }) => {
            parts.push(format!(
                "transmit report failing (attempt {attempt} of {of})"
            ));
        }
        Some(ReportProblem::GaveUp) => parts.push("transmit report not delivered".to_owned()),
        Some(ReportProblem::NoSession | ReportProblem::RateLimited) => {
            parts.push("transmit report refused".to_owned());
        }
        None => {}
    }
    format!("you: {}", parts.join("; "))
}

/// The status as lines of text: a pure function of the state. The lines are as the events
/// gave them; [`frame`] is what makes each fit to draw.
#[must_use]
pub fn render(state: &State) -> [String; LINES] {
    let connection = match &state.connection {
        Some(connection) => Event::Connection(connection).plain().unwrap_or_default(),
        None => "starting".to_owned(),
    };
    let roster = state.roster.as_ref().unwrap_or(&Roster::Unknown);
    [
        format!("voice: {connection}"),
        own_line(state),
        Event::Presence(roster).plain().unwrap_or_default(),
        state.notice.clone().unwrap_or_default(),
    ]
}

/// What is written to draw `lines`: over the last status if `in_place`, where the cursor
/// is if not. Every line passes [`one_line`] here, so nothing in a line can be a control
/// character: the only escape sequences written are this function's own.
#[must_use]
pub fn frame(lines: &[String; LINES], in_place: bool) -> String {
    let mut out = String::new();
    if in_place {
        out.push_str(SAVE_CURSOR);
        // Up to the first line of the status, and to its first column.
        out.push_str(&format!("\x1b[{LINES}A\r"));
    }
    out.push_str(WRAP_OFF);
    for line in lines {
        out.push_str(ERASE_LINE);
        out.push_str(&one_line(line));
        out.push('\n');
    }
    out.push_str(WRAP_ON);
    if in_place {
        out.push_str(RESTORE_CURSOR);
    }
    out
}

/// The terminal handling: what is shown, and whether the cursor is still under it.
#[derive(Debug)]
pub struct Screen {
    state: State,
    /// The lines last drawn.
    drawn: Option<[String; LINES]>,
    /// Whether the cursor is still on the line under what was last drawn.
    in_place: bool,
    /// Whether standard input is a terminal, so that a typed line is echoed under the status.
    echo: bool,
    /// Whether standard error is a terminal, so that a log record lands under the status.
    log_shares: bool,
}

impl Screen {
    /// A screen that nothing else writes to.
    #[must_use]
    pub fn new() -> Self {
        Self {
            state: State::default(),
            drawn: None,
            in_place: false,
            echo: false,
            log_shares: false,
        }
    }

    /// As [`Screen::new`], on a terminal that echoes what the user types.
    #[must_use]
    pub fn echoing() -> Self {
        Self {
            echo: true,
            ..Self::new()
        }
    }

    /// The terminal detection: a screen if standard output is a terminal and `--json` was
    /// not given, and otherwise `None`, which leaves the output line by line.
    #[must_use]
    pub fn for_stdout(json: bool) -> Option<Self> {
        if json || !std::io::stdout().is_terminal() {
            return None;
        }
        Some(Self {
            echo: std::io::stdin().is_terminal(),
            log_shares: std::io::stderr().is_terminal(),
            ..Self::new()
        })
    }

    /// The user typed a line. If the terminal echoed it, the cursor is no longer under the
    /// status.
    pub fn typed_line(&mut self) {
        if self.echo {
            self.in_place = false;
        }
    }

    /// Takes one event in, and returns what to write to the terminal, if what is shown
    /// changed.
    pub fn show(&mut self, event: &Event<'_>) -> Option<String> {
        if !self.state.apply(event) {
            return None;
        }
        if self.log_shares && DISTURBED.swap(false, Ordering::Relaxed) {
            self.in_place = false;
        }
        let lines = render(&self.state);
        if self.drawn.as_ref() == Some(&lines) {
            return None;
        }
        let written = frame(&lines, self.in_place);
        self.drawn = Some(lines);
        self.in_place = true;
        Some(written)
    }
}

impl Default for Screen {
    fn default() -> Self {
        Self::new()
    }
}
