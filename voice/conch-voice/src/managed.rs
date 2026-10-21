//! `conch-voice managed`: one voice session run for a native client, the owner, which
//! starts this process, drives it over two private pipes and stops it
//! (`docs/design/tui-voice.md`, the helper's side, §13).
//!
//! Input: commands on standard input, one JSON object per line, in the version the start
//! was written in (`conch.voice_control.v1`, read by `conch_voice_api::parse_command`, or
//! `conch.voice_control.v2`, read by `parse_command_v2`; §20.2); the end of standard input,
//! the parent process changing, SIGTERM and SIGINT, each of which is the owner done with
//! this process; and then what `join` takes, but for the channel, the server and the login,
//! which the start names (§9).
//! Output: events on file descriptor 3, one JSON object per line, written by this module's
//! [`Events`] and by nothing else: a hello first, offering v1 and v2, then states, acks and
//! protocol errors in the session's version. Nothing else this process writes is read as
//! the protocol.
//! Owns: the event writer and its numbering; the thread that reads standard input; the
//! watch on the parent; and the watchdog that ends the process when it does not end by
//! itself in time after its owner has gone or asked it to leave. The voice session itself
//! is the session loop's (`session.rs`), the one `join` runs: the same gate, key watcher,
//! connection policy and logging.
//!
//! What it keeps:
//!
//! - **Nothing is captured before a valid start, and nothing before the identity check**
//!   (§8.1). The hello is written before anything is read, opened or asked. After the start
//!   the configuration is read, the login taken from where the start says, `conchd` asked
//!   who that login is, and only for the account the owner named are PipeWire, the
//!   speakers and the key device opened, and the session loop run. The loop opens the
//!   microphone when a session first allows publishing, as for `join`.
//! - **Standard input is the owner, not a key.** Its end is the owner gone, never "a
//!   release and no more": the gate shuts within 100 ms, the microphone and the speakers
//!   close within 500 ms, and the process exits within 3 s, with the key device being read
//!   or not (§8.3). A watchdog ends the process at 3 s whatever is still running.
//! - **No command transmits.** There is none that presses or releases the talk key; the
//!   key device is the only source of presses, and every one goes through the gate.
//! - **Every command with a usable id read before the final state gets exactly one ack,
//!   after the state its effect changed.** All of them are answered by one place in turn
//!   (the start-up, the session loop, or its leaving), so that their order is the order
//!   they were read in. An ack is written however slow the owner is (below).
//! - **A slow owner loses events, not the helper.** At most 256 events wait; past that a
//!   state or a protocol_error is dropped and the gap shows in seq, and the owner asks for
//!   a snapshot (§3). An ack, and the state that answers `get_state`, are not dropped:
//!   they wait behind the others in places of their own, enough for every command the
//!   owner may have unanswered ([`Events`]).
//! - **Nothing an event says came from a command or from `conchd`.** Every string in one
//!   is a schema name, a word of a closed vocabulary, or this binary's version. A device
//!   `set_devices` names is never repeated: `capture` and `playback` say whether it is there.
//! - **The start fixes the version** (§20.2). A v1 session is today's, byte for byte, and
//!   always push-to-talk whatever `voice.toml` says, with one log line when it says open
//!   ([`V1_PUSH_TO_TALK`]); a line of the other version is `unknown_schema`, which ends the
//!   session. A v2 session takes `set_devices` and `set_talk_mode` as well, honours
//!   `talk_mode` from `voice.toml`, and its states say `playback` and `talk_mode`. Neither
//!   command opens the gate: a microphone switch and a mode change both end a transmission
//!   at once (`session.rs`), and only a fresh press, or in open mode a fresh onset, sends
//!   again. While starting, both are kept and used once the session loop runs.
//!
//! Where the start-up has to choose: a change of the key device's state while starting is
//! part of the one start-up step, so it is first shown by the connecting state, which says
//! what was found; a change made by a command or a mute or deafen key while starting is
//! shown at once, as every other change is.

use std::fs::{File, OpenOptions};
use std::future::Future;
use std::io::{Read, Write};
use std::os::unix::fs::FileTypeExt;
use std::path::PathBuf;
use std::sync::atomic::{AtomicI32, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

pub use conch_voice_api::VoiceControlCapture as Capture;
use conch_voice_api::{
    Error as ApiError, ServerAddress, VOICE_CONTROL_EVENT_FD, VOICE_CONTROL_EVENT_QUEUE_LINES,
    VOICE_CONTROL_HELLO_TIMEOUT_MS, VOICE_CONTROL_LEAVE_EXIT_MS, VOICE_CONTROL_MAX_IN_FLIGHT,
    VOICE_CONTROL_MAX_LINE_BYTES, VOICE_CONTROL_MAX_RETRY_IN_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS,
    VOICE_CONTROL_OWNER_POLL_MS, VOICE_CONTROL_SCHEMA_V1, VOICE_CONTROL_SCHEMA_V2,
    VOICE_CONTROL_START_TIMEOUT_MS, VoiceControlAckV1, VoiceControlAckV2, VoiceControlBlock,
    VoiceControlCommand, VoiceControlCommandV2, VoiceControlCredential, VoiceControlErrorCode,
    VoiceControlEventKind, VoiceControlHelloV1, VoiceControlPhase, VoiceControlPlayback,
    VoiceControlProtocolErrorV1, VoiceControlProtocolErrorV2, VoiceControlPtt,
    VoiceControlPttProblem, VoiceControlReason, VoiceControlStartV1, VoiceControlStartV2,
    VoiceControlStateV1, VoiceControlStateV2, VoiceControlTalkMode, default_config_dir,
    parse_command, parse_command_v2, resolve_token, to_line,
};
use conch_voice_control::{
    Config, Key, KeyAction, KeyBindings, KeyEvent, LineCommand, Ptt, PttInput, PttStatus,
    ShutReason, TalkMode,
};
use tokio::sync::{mpsc, oneshot};

use crate::audio::PipeWire;
use crate::cli::{self, Environment, MicChoice, SinkChoice};
use crate::error::Error;
use crate::input::{self, Bounded, Ended, LastLine};
use crate::keydev::{self, DeviceRule, DeviceState, KeyTimings, Problem};
use crate::lines::{self, EXIT_GRACE, LineQueue};
use crate::receive::BoxedSink;
use crate::sdk::Transport;
use crate::secrets::Scrubber;
use crate::session::{
    self, Conchd, DeviceSwitch, Input, OpenMic, OpenSink, REPORT_TIMEOUT, REQUEST_TIMEOUT,
    Settings, Timings,
};

/// How long a press's final `stopped` report is given while leaving (§8.2).
pub const REPORT_LIMIT: Duration = Duration::from_secs(1);
/// How long leaving the room is given after `leave` or a signal (§8.2).
pub const ROOM_LIMIT: Duration = Duration::from_secs(2);
/// How long the report and the room are each given once the owner is gone (§8.3).
pub const OWNER_LOST_STEP: Duration = Duration::from_secs(1);
/// How long the final state is given to be written (§5, rule 11).
pub const FINAL_STATE_LIMIT: Duration = Duration::from_millis(250);

/// Why `managed` failed, in this program's words, where no other error says it.
pub const PROTOCOL_ERROR: &str = "conch sent a line conch-voice cannot act on";
const START_TIMEOUT: &str = "conch did not say what to join in time";
const IDENTITY_MISMATCH: &str = "the login found is not the account conch is signed in as";
const BAD_ENVIRONMENT: &str = "CONCH_TOKEN is set, and conch named the stored login";
const NOT_A_PERSON: &str = "voice is for people: this login is not a person's";
const VOICE_NOT_ALLOWED: &str = "this account may not use voice";

/// Which version of the protocol a session speaks: the one its start was written in, fixed
/// for the session (`docs/design/tui-voice.md` §20.2).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Version {
    /// `conch.voice_control.v1`: today's protocol, push-to-talk only.
    V1,
    /// `conch.voice_control.v2`: device switching and the open microphone besides.
    V2,
}

impl Version {
    /// The schema name its lines carry.
    #[must_use]
    pub fn schema(self) -> &'static str {
        match self {
            Version::V1 => VOICE_CONTROL_SCHEMA_V1,
            Version::V2 => VOICE_CONTROL_SCHEMA_V2,
        }
    }
}

/// A command of the owner's as the session takes it: read, checked, and numbered. It holds
/// no text but the device names of a `set_devices`, which its `Debug` does not show
/// ([`DeviceSwitch`]), so it travels with the other inputs as they do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Control {
    /// `set_mute`.
    SetMute {
        /// The command's id.
        id: i64,
        /// The state wanted.
        muted: bool,
    },
    /// `set_deafen`.
    SetDeafen {
        /// The command's id.
        id: i64,
        /// The state wanted.
        deafened: bool,
    },
    /// `get_state`.
    GetState {
        /// The command's id.
        id: i64,
    },
    /// `leave`.
    Leave {
        /// The command's id.
        id: i64,
    },
    /// `set_devices` (v2): the microphone, the speakers or both to use from now on.
    SetDevices {
        /// The command's id.
        id: i64,
        /// The sides named; one left out is left alone.
        switch: DeviceSwitch,
    },
    /// `set_talk_mode` (v2): what may open the gate from now on.
    SetTalkMode {
        /// The command's id.
        id: i64,
        /// The mode wanted.
        talk_mode: TalkMode,
    },
    /// A command refused with an ack, after which the helper goes on: of a kind it does
    /// not know, invalid, or a second start.
    Refused {
        /// The command's id.
        id: i64,
        /// Why.
        code: VoiceControlErrorCode,
    },
    /// A line that cannot be acted on: a protocol_error, and the helper fails.
    Fatal {
        /// Why.
        code: VoiceControlErrorCode,
    },
}

/// How a managed session ends: the reason its last states give, and whether it failed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Ending {
    /// `left`, `owner_lost` or `signal` for a stop; a failure's reason otherwise.
    pub reason: VoiceControlReason,
    /// Whether the session failed (exit 1) rather than stopped (exit 0).
    pub failed: bool,
}

impl Ending {
    /// A stop, for one of the reasons a stop has.
    #[must_use]
    pub fn stopped(reason: VoiceControlReason) -> Self {
        Self {
            reason,
            failed: false,
        }
    }

    /// A failure, for one of the reasons a failure has.
    #[must_use]
    pub fn failed(reason: VoiceControlReason) -> Self {
        Self {
            reason,
            failed: true,
        }
    }
}

/// What is known of the key device, for a state's `ptt`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum KeyView {
    /// No key device, or no key bound on one.
    NotConfigured,
    /// Not tried yet.
    Unknown,
    /// What its watcher last said, and whether a talk key is bound on it.
    State {
        /// The watcher's word.
        state: DeviceState,
        /// Whether a talk key is bound.
        talk: bool,
    },
}

impl KeyView {
    /// The state's `ptt` and `ptt_problem`. A device read only for its mute and deafen
    /// keys has no talk key, and is not configured for hold-to-talk; it is lost all the
    /// same once it was read and is not, since every press is then refused.
    fn ptt(self) -> (VoiceControlPtt, Option<VoiceControlPttProblem>) {
        match self {
            KeyView::NotConfigured => (VoiceControlPtt::NotConfigured, None),
            KeyView::Unknown => (VoiceControlPtt::Unknown, None),
            KeyView::State {
                state: DeviceState::Lost(problem),
                ..
            } => (VoiceControlPtt::Lost, Some(problem_word(problem))),
            KeyView::State { talk: false, .. } => (VoiceControlPtt::NotConfigured, None),
            KeyView::State {
                state: DeviceState::Ready,
                ..
            } => (VoiceControlPtt::Ready, None),
            KeyView::State {
                state: DeviceState::Missing(problem),
                ..
            } => (VoiceControlPtt::Unavailable, Some(problem_word(problem))),
        }
    }
}

fn problem_word(problem: Problem) -> VoiceControlPttProblem {
    match problem {
        Problem::CannotOpen(_) => VoiceControlPttProblem::CannotOpen,
        Problem::Refused(_) => VoiceControlPttProblem::Refused,
        Problem::ReadFailed(_) => VoiceControlPttProblem::ReadFailed,
        Problem::Ended => VoiceControlPttProblem::Ended,
        Problem::PartialRecord => VoiceControlPttProblem::PartialRecord,
    }
}

fn block_word(reason: ShutReason) -> VoiceControlBlock {
    match reason {
        ShutReason::NotConnected => VoiceControlBlock::NotConnected,
        ShutReason::NoPublishGrant => VoiceControlBlock::NoPublishGrant,
        ShutReason::NoMicrophone => VoiceControlBlock::NoMicrophone,
        ShutReason::Deafened => VoiceControlBlock::Deafened,
        ShutReason::Muted => VoiceControlBlock::Muted,
        ShutReason::KeyDeviceLost => VoiceControlBlock::KeyDeviceLost,
        ShutReason::MaxTransmit => VoiceControlBlock::MaxTransmit,
    }
}

/// A state as the owner is shown it, without its seq: two snapshots are the same state
/// when they are equal, and only a different one is written unasked.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Snapshot {
    /// Where the helper is in its life.
    pub phase: VoiceControlPhase,
    /// Why.
    pub reason: Option<VoiceControlReason>,
    /// While reconnecting for a reason: how long until `conchd` is asked.
    pub retry_in_ms: Option<i64>,
    /// The microphone stream.
    pub capture: Capture,
    /// Whether audio is leaving now.
    pub transmitting: bool,
    /// The machine's mute.
    pub muted: bool,
    /// The machine's deafen.
    pub deafened: bool,
    /// Every reason a press would do nothing, most important first.
    pub blocked: Vec<VoiceControlBlock>,
    /// Whether hold-to-talk can work.
    pub ptt: VoiceControlPtt,
    /// Why the key device is not read.
    pub ptt_problem: Option<VoiceControlPttProblem>,
    /// The speakers' stream: said in v2 only.
    pub playback: VoiceControlPlayback,
    /// The talk mode in force: said in v2 only.
    pub talk_mode: VoiceControlTalkMode,
}

impl Snapshot {
    /// The snapshot of a phase, from the push-to-talk machine's own state and what is known
    /// of the microphone and the key device. Outside `connected` a press could not
    /// transmit, and `not_connected` says so whatever the machine was told.
    #[must_use]
    pub fn new(
        phase: VoiceControlPhase,
        reason: Option<VoiceControlReason>,
        retry_in: Option<Duration>,
        capture: Capture,
        status: PttStatus,
        key: KeyView,
    ) -> Self {
        let mut blocked: Vec<VoiceControlBlock> = status.blocked.iter().map(block_word).collect();
        if phase != VoiceControlPhase::Connected
            && !blocked.contains(&VoiceControlBlock::NotConnected)
        {
            blocked.insert(0, VoiceControlBlock::NotConnected);
        }
        let (ptt, ptt_problem) = key.ptt();
        let retry_in_ms = retry_in.map(|wait| {
            i64::try_from(wait.as_millis())
                .unwrap_or(i64::MAX)
                .min(VOICE_CONTROL_MAX_RETRY_IN_MS)
        });
        Self {
            phase,
            reason,
            retry_in_ms,
            capture,
            transmitting: status.transmitting,
            muted: status.muted(),
            deafened: status.deafened(),
            blocked,
            ptt,
            ptt_problem,
            playback: VoiceControlPlayback::Off,
            talk_mode: VoiceControlTalkMode::PushToTalk,
        }
    }

    /// The same snapshot, with the speakers' state and the talk mode in force, which a v2
    /// state says. Before the speakers are opened and after they are closed (starting and
    /// the final phases) there is no stream, and before the configuration is read
    /// (starting) the mode is push-to-talk, whatever is passed (§20.4).
    #[must_use]
    pub fn with_playback_and_mode(
        mut self,
        playback: VoiceControlPlayback,
        talk_mode: TalkMode,
    ) -> Self {
        let closed = self.phase == VoiceControlPhase::Starting || self.phase.is_final();
        self.playback = if closed {
            VoiceControlPlayback::Off
        } else {
            playback
        };
        self.talk_mode = match talk_mode {
            TalkMode::Open if self.phase != VoiceControlPhase::Starting => {
                VoiceControlTalkMode::Open
            }
            TalkMode::Open | TalkMode::PushToTalk => VoiceControlTalkMode::PushToTalk,
        };
        self
    }

    /// The snapshot as an owner of `version` is shown it: a v1 state says nothing of the
    /// speakers or of the talk mode, so a change of either is no change to a v1 owner, and
    /// writes no state.
    fn seen_in(&self, version: Version) -> Snapshot {
        let mut seen = self.clone();
        if version == Version::V1 {
            seen.playback = VoiceControlPlayback::Off;
            seen.talk_mode = VoiceControlTalkMode::PushToTalk;
        }
        seen
    }

    /// Whether this state says the session is failing: failed, or stopping for a reason
    /// that is not one of a stop's.
    fn is_failing(&self) -> bool {
        match self.phase {
            VoiceControlPhase::Failed => true,
            VoiceControlPhase::Stopping => self.reason.is_some_and(|reason| !reason.is_stop()),
            _ => false,
        }
    }

    fn event(&self, seq: i64) -> VoiceControlStateV1 {
        VoiceControlStateV1 {
            schema: VOICE_CONTROL_SCHEMA_V1.to_owned(),
            kind: VoiceControlEventKind::State,
            seq,
            phase: self.phase,
            reason: self.reason,
            retry_in_ms: self.retry_in_ms,
            capture: self.capture,
            transmitting: self.transmitting,
            muted: self.muted,
            deafened: self.deafened,
            blocked: self.blocked.clone(),
            ptt: self.ptt,
            ptt_problem: self.ptt_problem,
        }
    }

    fn event_v2(&self, seq: i64) -> VoiceControlStateV2 {
        VoiceControlStateV2 {
            schema: VOICE_CONTROL_SCHEMA_V2.to_owned(),
            kind: VoiceControlEventKind::State,
            seq,
            phase: self.phase,
            reason: self.reason,
            retry_in_ms: self.retry_in_ms,
            capture: self.capture,
            playback: self.playback,
            transmitting: self.transmitting,
            muted: self.muted,
            deafened: self.deafened,
            blocked: self.blocked.clone(),
            talk_mode: self.talk_mode,
            ptt: self.ptt,
            ptt_problem: self.ptt_problem,
        }
    }
}

/// How many answers may wait beyond [`VOICE_CONTROL_EVENT_QUEUE_LINES`]: an ack for each
/// command the owner may have unanswered, and the state before it if it is a `get_state`.
///
/// Enough by the owner's contract (§5, rule 10): it has at most
/// [`VOICE_CONTROL_MAX_IN_FLIGHT`] commands written and not seen answered, and one whose
/// answer still waits here is one it has not seen answered. An owner that sends more than
/// that is past the contract, and the bound still holds: its answers past the reserve are
/// dropped as other events are, the gap shows, and it waits for an ack that does not come.
const ANSWER_LINES: usize = 2 * VOICE_CONTROL_MAX_IN_FLIGHT;

/// Whether an event may be dropped for a slow owner.
#[derive(Clone, Copy)]
enum Keep {
    /// A state that changed, or a protocol_error: dropped if the queue is full, and the gap
    /// in seq says so.
    IfThereIsRoom,
    /// An ack, or the state that answers `get_state`: never dropped within
    /// [`ANSWER_LINES`], since the owner waits for each and fails the session without
    /// it (§7), and since after a gap the answer to `get_state` is how it resynchronises.
    Answer,
}

/// The writer of the events: the only thing that writes to file descriptor 3. Each event
/// gets the next seq whether or not it is written, so a drop shows as a gap. Nothing here
/// waits for the owner but [`Events::flush`], up to its limit.
///
/// Every event joins the back of one queue, an answer too: an ack is written after every
/// event written before it, the state its command changed among them, and seq only rises.
/// When the queue is full an answer waits behind the events already in it, in a place
/// reserved for answers ([`ANSWER_LINES`]).
pub struct Events {
    queue: LineQueue,
    next: i64,
    /// The version every event but the hello is written in: v1 until a start, or a first
    /// line, names v2 ([`Events::speak`]).
    version: Version,
}

impl Events {
    /// A writer of events to `out`, on a thread of its own, with at most
    /// [`VOICE_CONTROL_EVENT_QUEUE_LINES`] waiting, and [`ANSWER_LINES`] more for answers.
    #[must_use]
    pub fn spawn(out: Box<dyn Write + Send>) -> Self {
        Self {
            queue: LineQueue::spawn_unmarked(out, VOICE_CONTROL_EVENT_QUEUE_LINES, ANSWER_LINES),
            next: 1,
            version: Version::V1,
        }
    }

    /// Writes every event after the hello in `version`, the one the owner chose.
    pub fn speak(&mut self, version: Version) {
        self.version = version;
    }

    fn write(&mut self, keep: Keep, line: impl FnOnce(i64) -> Result<String, ApiError>) {
        let seq = self.next;
        self.next = self.next.saturating_add(1);
        match line(seq) {
            Ok(line) => match keep {
                Keep::IfThereIsRoom => self.queue.push(line),
                Keep::Answer => self.queue.push_reserved(line),
            },
            // A state that does not hold together is a fault here, not something to send:
            // the owner sees the gap in seq and asks again.
            Err(error) => log::error!("managed: an event was not written: {error}"),
        }
    }

    /// The hello: the first event, written before anything is read. It is v1's, in the
    /// frozen shape, and offers both versions (§20.2).
    pub fn hello(&mut self) {
        // The first event, into an empty queue.
        self.write(Keep::IfThereIsRoom, |seq| {
            let mut hello = VoiceControlHelloV1::v1_and_v2(cli::version());
            hello.seq = seq;
            to_line(&hello)
        });
    }

    fn state(&mut self, snapshot: &Snapshot, keep: Keep) {
        match self.version {
            Version::V1 => self.write(keep, |seq| to_line(&snapshot.event(seq))),
            Version::V2 => self.write(keep, |seq| to_line(&snapshot.event_v2(seq))),
        }
    }

    fn ack(&mut self, id: i64, refusal: Option<VoiceControlErrorCode>) {
        match self.version {
            Version::V1 => self.write(Keep::Answer, |seq| {
                to_line(&VoiceControlAckV1::new(seq, id, refusal))
            }),
            Version::V2 => self.write(Keep::Answer, |seq| {
                to_line(&VoiceControlAckV2::new(seq, id, refusal))
            }),
        }
    }

    fn protocol_error(&mut self, code: VoiceControlErrorCode) {
        match self.version {
            Version::V1 => self.write(Keep::IfThereIsRoom, |seq| {
                to_line(&VoiceControlProtocolErrorV1::new(seq, code))
            }),
            Version::V2 => self.write(Keep::IfThereIsRoom, |seq| {
                to_line(&VoiceControlProtocolErrorV2::new(seq, code))
            }),
        }
    }

    /// Waits until what was written so far has reached the pipe, or `limit` has passed.
    pub async fn flush(&self, limit: Duration) -> bool {
        self.queue.flush(limit).await
    }
}

/// The owner, as the helper tells it things: the events, and the last state it was told,
/// so that only a change is written unasked.
pub struct Owner {
    events: Events,
    shown: Option<Snapshot>,
    /// Told when the session is failing, so that a process the watchdog ends exits as a
    /// failure, as the owner was told.
    watchdog: Watchdog,
}

impl Owner {
    fn new(events: Events, watchdog: Watchdog) -> Self {
        Self {
            events,
            shown: None,
            watchdog,
        }
    }

    /// Tells the owner the state if it differs from the last one it was told, or whatever
    /// it is if `force`. `force` is how `get_state` is answered, and only that: the state
    /// it writes is kept as an ack is, so that the answer is a state and then the ack
    /// whatever the queue holds (§5, rule 8).
    pub fn state(&mut self, snapshot: &Snapshot, force: bool) {
        if snapshot.is_failing() {
            self.watchdog.failed();
        }
        // What the owner's version can say: a v1 owner is not told of a change it cannot
        // see, as before v2 existed.
        let snapshot = snapshot.seen_in(self.events.version);
        if force {
            self.events.state(&snapshot, Keep::Answer);
            self.shown = Some(snapshot);
        } else if self.shown.as_ref() != Some(&snapshot) {
            self.events.state(&snapshot, Keep::IfThereIsRoom);
            self.shown = Some(snapshot);
        }
    }

    /// Answers command `id`: carried out, or refused for `refusal`.
    pub fn ack(&mut self, id: i64, refusal: Option<VoiceControlErrorCode>) {
        self.events.ack(id, refusal);
    }

    /// Says that a line cannot be acted on. The session fails for it.
    pub fn protocol_error(&mut self, code: VoiceControlErrorCode) {
        self.watchdog.failed();
        self.events.protocol_error(code);
    }

    /// Tells the owner the final state, and gives it [`FINAL_STATE_LIMIT`] to be written.
    /// Nothing is written after it: this is the end of the owner.
    pub async fn finish(mut self, last: &Snapshot) {
        self.events.state(last, Keep::IfThereIsRoom);
        if !self.events.flush(FINAL_STATE_LIMIT).await {
            log::warn!("managed: the final state was not read before leaving");
        }
    }
}

/// What the start-up hands the session loop.
pub struct Handoff {
    /// The owner, told the starting state and the start's ack.
    pub owner: Owner,
    /// Whether the machine starts muted: as the start said, or as the commands and keys
    /// left it while starting.
    pub muted: bool,
    /// Whether it starts deafened.
    pub deafened: bool,
    /// What the key device's watcher said while starting; `None` with no key device.
    pub key_state: Option<DeviceState>,
    /// Whether a talk key is bound on the key device.
    pub talk_key: bool,
    /// The devices the owner named while starting (v2), used once the session loop runs:
    /// a side left out is left alone.
    pub switch: DeviceSwitch,
}

impl Handoff {
    /// The session loop could not begin (the speakers would not open): a failed state,
    /// with the machine as it was handed over.
    pub async fn fail(self, error: &Error) {
        let reason = match error {
            Error::PipeWire { .. } | Error::Audio(_) => VoiceControlReason::AudioUnavailable,
            _ => VoiceControlReason::InternalError,
        };
        let key = match self.key_state {
            Some(state) => KeyView::State {
                state,
                talk: self.talk_key,
            },
            None => KeyView::NotConfigured,
        };
        let status = fresh_status(self.muted, self.deafened);
        let last = Snapshot::new(
            VoiceControlPhase::Failed,
            Some(reason),
            None,
            Capture::Off,
            status,
            key,
        );
        self.owner.finish(&last).await;
    }
}

/// A push-to-talk machine that has just started, muted and deafened as given.
fn fresh_machine(muted: bool, deafened: bool) -> Ptt {
    let (mut ptt, _) = Ptt::new(Duration::MAX);
    if muted {
        ptt.handle(Duration::ZERO, PttInput::Line(LineCommand::Mute));
    }
    if deafened {
        ptt.handle(Duration::ZERO, PttInput::Line(LineCommand::Deafen));
    }
    ptt
}

fn fresh_status(muted: bool, deafened: bool) -> PttStatus {
    fresh_machine(muted, deafened).status()
}

/// Ends the process when it has not ended by itself in time: after its owner has gone,
/// after `leave`, and after a signal. It is the `lines::exit` that `main` uses, begun early
/// enough that the process is gone at the bound. Off in the library's tests, which are not
/// a process of their own.
///
/// It exits with the code the session has come to: 0 for a stop, 1 once the owner has been
/// told the session is failing ([`Watchdog::failed`]), so that the owner never reads a
/// failure as a stop.
#[derive(Clone)]
pub struct Watchdog {
    armed: Option<Arc<Mutex<Option<std::time::Instant>>>>,
    code: Arc<AtomicI32>,
}

impl Watchdog {
    /// The watchdog of this process.
    #[must_use]
    pub fn process() -> Self {
        Self {
            armed: Some(Arc::default()),
            code: Arc::default(),
        }
    }

    /// A watchdog that never fires.
    #[must_use]
    pub fn off() -> Self {
        Self {
            armed: None,
            code: Arc::default(),
        }
    }

    /// The session is failing: if the watchdog ends the process, it exits 1.
    pub fn failed(&self) {
        self.code.store(1, Ordering::SeqCst);
    }

    /// The code the process exits with if the watchdog ends it now.
    #[must_use]
    pub fn exit_code(&self) -> i32 {
        self.code.load(Ordering::SeqCst)
    }

    /// Makes sure the process has exited `within` from now. A later bound than one already
    /// set changes nothing.
    pub fn arm(&self, within: Duration) {
        if !self.sooner(within) {
            return;
        }
        let code = Arc::clone(&self.code);
        std::thread::spawn(move || {
            // The ordinary exit is tried first, and is itself given EXIT_GRACE.
            std::thread::sleep(within.saturating_sub(EXIT_GRACE));
            log::warn!("managed: the helper did not exit in time; ending it");
            lines::exit(code.load(Ordering::SeqCst));
        });
    }

    /// Whether `within` from now is sooner than any bound already set, and if so, sets it.
    fn sooner(&self, within: Duration) -> bool {
        let Some(armed) = &self.armed else {
            return false;
        };
        let at = std::time::Instant::now() + within;
        let mut armed = armed.lock().unwrap_or_else(PoisonError::into_inner);
        if armed.is_some_and(|earlier| earlier <= at) {
            return false;
        }
        *armed = Some(at);
        true
    }
}

/// A valid start, in the version it was written in, which the session then speaks. It
/// carries no token (§9).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Start {
    /// The version the start was written in.
    pub version: Version,
    /// The command's id.
    pub id: i64,
    /// The `conchd` address, as the owner's credential store keys it.
    pub server: String,
    /// The channel whose voice to join.
    pub channel: String,
    /// The owner's user.
    pub principal_id: i64,
    /// Where the login token is.
    pub credential: VoiceControlCredential,
    /// Whether to start muted.
    pub muted: bool,
    /// Whether to start deafened.
    pub deafened: bool,
}

impl From<VoiceControlStartV1> for Start {
    fn from(start: VoiceControlStartV1) -> Self {
        Self {
            version: Version::V1,
            id: start.id,
            server: start.server,
            channel: start.channel,
            principal_id: start.principal_id,
            credential: start.credential,
            muted: start.muted,
            deafened: start.deafened,
        }
    }
}

impl From<VoiceControlStartV2> for Start {
    fn from(start: VoiceControlStartV2) -> Self {
        Self {
            version: Version::V2,
            id: start.id,
            server: start.server,
            channel: start.channel,
            principal_id: start.principal_id,
            credential: start.credential,
            muted: start.muted,
            deafened: start.deafened,
        }
    }
}

/// What the first line of the control pipe came to.
#[derive(Debug)]
pub enum First {
    /// A valid start.
    Start(Start),
    /// Anything else: the code its protocol_error gives (§5, rule 5), and the version that
    /// error and the failed state after it are written in ([`version_named`]).
    Bad(VoiceControlErrorCode, Version),
    /// The end of input before a whole line.
    End,
}

/// The first line: a valid start, or the code that refuses it. It is read with the reader
/// of the version it names, since the hello offered both (§20.2), and so fixes the version:
/// a line of v2 by v2's reader, any other by v1's, which refuses a third version as
/// `unknown_schema`. A line refused for a code that ends a session keeps that code; anything
/// else that is not a start is not_started.
///
/// # Errors
///
/// The code the line's protocol_error gives.
pub fn first_line(line: Bounded<'_>) -> Result<Start, VoiceControlErrorCode> {
    let Bounded::Line(bytes) = line else {
        return Err(VoiceControlErrorCode::TooLong);
    };
    let refused = |code: VoiceControlErrorCode| {
        if code.is_fatal() {
            code
        } else {
            VoiceControlErrorCode::NotStarted
        }
    };
    match parse_command_v2(bytes) {
        Ok(VoiceControlCommandV2::Start(start)) => Ok(start.into()),
        Ok(_) => Err(VoiceControlErrorCode::NotStarted),
        Err(refusal) if refusal.code != VoiceControlErrorCode::UnknownSchema => {
            Err(refused(refusal.code))
        }
        // Not a line of v2: read as v1, as before v2 existed.
        Err(_) => match parse_command(bytes) {
            Ok(VoiceControlCommand::Start(start)) => Ok(start.into()),
            Ok(_) => Err(VoiceControlErrorCode::NotStarted),
            Err(refusal) => Err(refused(refusal.code)),
        },
    }
}

/// The version a first line that is not a valid start is answered in: v2 when it is a
/// line of v2 (one JSON object whose schema is v2's), so that the owner that wrote it reads
/// the answer; v1 otherwise, the version of the hello, which any owner reads.
#[must_use]
pub fn version_named(line: Bounded<'_>) -> Version {
    use VoiceControlErrorCode as Code;
    let Bounded::Line(bytes) = line else {
        return Version::V1;
    };
    match parse_command_v2(bytes) {
        Err(refusal)
            if matches!(
                refusal.code,
                Code::TooLong | Code::Malformed | Code::UnknownSchema
            ) =>
        {
            Version::V1
        }
        Ok(_) | Err(_) => Version::V2,
    }
}

/// One command read after the start, in either version, before its id is checked.
enum Command {
    Start,
    SetMute(bool),
    SetDeafen(bool),
    GetState,
    Leave,
    SetDevices(DeviceSwitch),
    SetTalkMode(TalkMode),
}

impl From<VoiceControlCommand> for Command {
    fn from(command: VoiceControlCommand) -> Self {
        match command {
            VoiceControlCommand::Start(_) => Command::Start,
            VoiceControlCommand::SetMute(c) => Command::SetMute(c.muted),
            VoiceControlCommand::SetDeafen(c) => Command::SetDeafen(c.deafened),
            VoiceControlCommand::GetState(_) => Command::GetState,
            VoiceControlCommand::Leave(_) => Command::Leave,
        }
    }
}

impl From<VoiceControlCommandV2> for Command {
    fn from(command: VoiceControlCommandV2) -> Self {
        match command {
            VoiceControlCommandV2::Start(_) => Command::Start,
            VoiceControlCommandV2::SetMute(c) => Command::SetMute(c.muted),
            VoiceControlCommandV2::SetDeafen(c) => Command::SetDeafen(c.deafened),
            VoiceControlCommandV2::GetState(_) => Command::GetState,
            VoiceControlCommandV2::Leave(_) => Command::Leave,
            VoiceControlCommandV2::SetDevices(c) => Command::SetDevices(DeviceSwitch {
                input: c.input,
                output: c.output,
            }),
            VoiceControlCommandV2::SetTalkMode(c) => Command::SetTalkMode(talk_mode(c.talk_mode)),
        }
    }
}

/// The machine's talk mode for the protocol's word.
fn talk_mode(word: VoiceControlTalkMode) -> TalkMode {
    match word {
        VoiceControlTalkMode::PushToTalk => TalkMode::PushToTalk,
        VoiceControlTalkMode::Open => TalkMode::Open,
    }
}

/// The lines after the start, read in order with the reader of the session's version: each
/// id must be greater than the one before.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Later {
    last_id: i64,
    version: Version,
}

impl Later {
    /// After a v1 start with this id.
    #[must_use]
    pub fn after(start_id: i64) -> Self {
        Self::after_in(start_id, Version::V1)
    }

    /// After a start with this id, written in `version`.
    #[must_use]
    pub fn after_in(start_id: i64, version: Version) -> Self {
        Self {
            last_id: start_id,
            version,
        }
    }

    /// What one line asks of the session.
    pub fn line(&mut self, line: Bounded<'_>) -> Control {
        let Bounded::Line(bytes) = line else {
            return Control::Fatal {
                code: VoiceControlErrorCode::TooLong,
            };
        };
        let parsed = match self.version {
            Version::V1 => {
                parse_command(bytes).map(|command| (command.id(), Command::from(command)))
            }
            Version::V2 => {
                parse_command_v2(bytes).map(|command| (command.id(), Command::from(command)))
            }
        };
        let (id, command) = match parsed {
            Ok((id, command)) => (id, Ok(command)),
            Err(refusal) => match refusal.id {
                Some(id) => (id, Err(refusal.code)),
                // No id to answer: the line ends the session.
                None => {
                    return Control::Fatal { code: refusal.code };
                }
            },
        };
        if id <= self.last_id {
            return Control::Fatal {
                code: VoiceControlErrorCode::IdNotIncreasing,
            };
        }
        self.last_id = id;
        match command {
            Err(code) => Control::Refused { id, code },
            Ok(Command::Start) => Control::Refused {
                id,
                code: VoiceControlErrorCode::AlreadyStarted,
            },
            Ok(Command::SetMute(muted)) => Control::SetMute { id, muted },
            Ok(Command::SetDeafen(deafened)) => Control::SetDeafen { id, deafened },
            Ok(Command::GetState) => Control::GetState { id },
            Ok(Command::Leave) => Control::Leave { id },
            Ok(Command::SetDevices(switch)) => Control::SetDevices { id, switch },
            Ok(Command::SetTalkMode(talk_mode)) => Control::SetTalkMode { id, talk_mode },
        }
    }
}

/// Reads the control pipe on a thread of its own, for as long as it lasts: the first line
/// to `first`, every later one to `inputs` as a [`Control`], and its end as
/// [`Input::OwnerLost`]. A partial line at the end is discarded. `leave`, a line that ends
/// the session, and the end of the pipe arm the watchdog.
pub fn spawn_reader(
    stdin: impl Read + Send + 'static,
    first: oneshot::Sender<First>,
    inputs: mpsc::UnboundedSender<Input>,
    watchdog: Watchdog,
) {
    std::thread::spawn(move || {
        let mut first = Some(first);
        let mut later = Later::after(0);
        let ended = input::read_bounded(
            stdin,
            VOICE_CONTROL_MAX_LINE_BYTES,
            LastLine::Discarded,
            |line| {
                if let Some(first) = first.take() {
                    return match first_line(line) {
                        Ok(start) => {
                            later = Later::after_in(start.id, start.version);
                            first.send(First::Start(start)).is_ok()
                        }
                        Err(code) => {
                            let _ = first.send(First::Bad(code, version_named(line)));
                            false
                        }
                    };
                }
                let control = later.line(line);
                let fatal = matches!(control, Control::Fatal { .. });
                if fatal || matches!(control, Control::Leave { .. }) {
                    watchdog.arm(Duration::from_millis(VOICE_CONTROL_LEAVE_EXIT_MS));
                }
                inputs.send(Input::Control(control)).is_ok() && !fatal
            },
        );
        if ended == Ended::Input {
            match first.take() {
                Some(first) => {
                    let _ = first.send(First::End);
                }
                None => {
                    watchdog.arm(Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS));
                    let _ = inputs.send(Input::OwnerLost);
                }
            }
        }
    });
}

/// Looks every [`VOICE_CONTROL_OWNER_POLL_MS`] at whether the process that started this one
/// is still its parent, and says the owner is lost when it is not: a dead owner's children
/// are given to another parent, which catches a death the control pipe misses (§8.3).
pub fn watch_parent(parent: u32, inputs: mpsc::UnboundedSender<Input>, watchdog: Watchdog) {
    std::thread::spawn(move || {
        loop {
            std::thread::sleep(Duration::from_millis(VOICE_CONTROL_OWNER_POLL_MS));
            if inputs.is_closed() {
                return;
            }
            if std::os::unix::process::parent_id() != parent {
                watchdog.arm(Duration::from_millis(VOICE_CONTROL_OWNER_LOSS_EXIT_MS));
                let _ = inputs.send(Input::OwnerLost);
                return;
            }
        }
    });
}

/// Turns SIGTERM and SIGINT into [`Input::Signal`], which a managed session takes as
/// `leave` (§8.4), and arms the watchdog for a leave's bound. Must be called inside the
/// runtime.
///
/// # Errors
///
/// What the operating system said, if a handler could not be installed.
pub fn signals(inputs: mpsc::UnboundedSender<Input>, watchdog: Watchdog) -> std::io::Result<()> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut interrupt = signal(SignalKind::interrupt())?;
    let mut terminate = signal(SignalKind::terminate())?;
    tokio::spawn(async move {
        loop {
            tokio::select! {
                _ = interrupt.recv() => {}
                _ = terminate.recv() => {}
            }
            watchdog.arm(Duration::from_millis(VOICE_CONTROL_LEAVE_EXIT_MS));
            if inputs.send(Input::Signal).is_err() {
                return;
            }
        }
    });
    Ok(())
}

/// Opens the event pipe the owner handed over as file descriptor 3, without `unsafe`: the
/// same pipe opened again by its name under `/proc/self/fd`, checked to be a pipe before
/// and after. The inherited descriptor stays open beside it. Opening a pipe to write waits
/// for a reader; the owner reads it from the start, and one that is gone already is not
/// waited for longer than it would itself have waited for the hello.
///
/// # Errors
///
/// [`Error::EventPipe`] if file descriptor 3 is not open, is not a pipe, or has no reader.
pub fn open_event_pipe() -> Result<File, Error> {
    let path = PathBuf::from(format!("/proc/self/fd/{VOICE_CONTROL_EVENT_FD}"));
    let named = std::fs::metadata(&path).map_err(|_| Error::EventPipe("is not open"))?;
    if !named.file_type().is_fifo() {
        return Err(Error::EventPipe("is not a pipe"));
    }
    let (opened, open) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = opened.send(OpenOptions::new().write(true).open(&path));
    });
    let file = open
        .recv_timeout(Duration::from_millis(VOICE_CONTROL_HELLO_TIMEOUT_MS))
        .map_err(|_| Error::EventPipe("has nobody reading it"))?
        .map_err(|_| Error::EventPipe("cannot be opened for writing"))?;
    let is_pipe = file
        .metadata()
        .is_ok_and(|opened| opened.file_type().is_fifo());
    if !is_pipe {
        return Err(Error::EventPipe("is not a pipe"));
    }
    Ok(file)
}

/// How the microphone and the speakers are opened, once the identity has been checked.
pub enum Devices {
    /// PipeWire's, or with `managed`'s test options a tone, a WAV file and a counting sink.
    Choices {
        /// `--mic`.
        mic: MicChoice,
        /// `--sink`.
        sink: SinkChoice,
    },
    /// Opened by the caller: for the library's tests.
    Given {
        /// Opens the microphone.
        open_mic: OpenMic,
        /// Opens the speakers.
        open_sink: OpenSink,
        /// The tones the mix is measured for.
        sink_tones: Vec<f32>,
    },
}

/// Where key events come from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KeySource {
    /// The key device `voice.toml` names, read as an event device.
    Configured,
    /// `--test-key-device`: a named pipe of records, with the keys `voice.toml` binds.
    NamedPipe(PathBuf),
}

/// Everything a managed helper runs with beyond its pipes.
pub struct Host<T: Transport> {
    /// The process's environment, as it was read once.
    pub environment: Environment,
    /// LiveKit, or a fake of it.
    pub transport: T,
    /// What keeps the login, join tokens and room names out of what is written.
    pub scrubber: Arc<Scrubber>,
    /// The microphone and the speakers.
    pub devices: Devices,
    /// Where key events come from.
    pub keys: KeySource,
    /// The session loop's waits.
    pub timings: Timings,
    /// The key device's waits.
    pub key_timings: KeyTimings,
    /// How long to wait for start ([`VOICE_CONTROL_START_TIMEOUT_MS`]).
    pub start_timeout: Duration,
    /// What ends the process when it does not end in time.
    pub watchdog: Watchdog,
}

impl<T: Transport> Host<T> {
    /// The host of the real process: its environment, LiveKit, the start timeout of the
    /// protocol, and a watchdog that can end it.
    pub fn new(
        environment: Environment,
        transport: T,
        scrubber: Arc<Scrubber>,
        devices: Devices,
        keys: KeySource,
    ) -> Self {
        Self {
            environment,
            transport,
            scrubber,
            devices,
            keys,
            timings: Timings::default(),
            key_timings: KeyTimings::default(),
            start_timeout: Duration::from_millis(VOICE_CONTROL_START_TIMEOUT_MS),
            watchdog: Watchdog::process(),
        }
    }
}

/// Why the start-up did not hand over to the session loop.
enum Halt {
    /// The owner asked it to stop, went, or sent a signal.
    Stop(VoiceControlReason),
    /// It failed, for this reason, with this error.
    Fail(VoiceControlReason, Error),
}

/// What the start-up waits for first.
enum Begin {
    /// The start, and whatever the session's inputs held after it when it was taken: the
    /// lines read after it among them, which are answered in order.
    Start(Start, Vec<Input>),
    Bad(VoiceControlErrorCode, Version),
    Timeout,
    /// The owner went, or a signal came, before a start: nothing more is written.
    Gone,
}

/// Runs `conch-voice managed` over `stdin` and the event writer `out`: the hello, the
/// start, the start-up, and the session, until the final state. `inputs` and `received`
/// are the two ends of the session loop's inputs; the caller may have given `inputs` to a
/// signal handler and the watch on the parent already.
///
/// # Errors
///
/// What the owner was told in a failed state: the error, or [`Error::Managed`].
pub async fn run<T: Transport>(
    host: Host<T>,
    stdin: impl Read + Send + 'static,
    out: Box<dyn Write + Send>,
    inputs: mpsc::UnboundedSender<Input>,
    mut received: mpsc::UnboundedReceiver<Input>,
) -> Result<(), Error> {
    let mut events = Events::spawn(out);
    // Before anything is read, opened or asked.
    events.hello();
    let (first, first_read) = oneshot::channel();
    spawn_reader(stdin, first, inputs.clone(), host.watchdog.clone());
    let watchdog = host.watchdog.clone();
    let (start, early) = match wait_for_start(first_read, &mut received, host.start_timeout).await {
        Begin::Start(start, early) => (start, early),
        Begin::Gone => {
            // Never started: nothing more is written (§5, rule 3).
            events.flush(FINAL_STATE_LIMIT).await;
            return Ok(());
        }
        Begin::Bad(code, version) => {
            events.speak(version);
            let mut owner = Owner::new(events, watchdog);
            owner.protocol_error(code);
            let boot = Boot::new(owner, received, false, false);
            boot.finish(VoiceControlPhase::Failed, VoiceControlReason::ProtocolError)
                .await;
            return Err(Error::Managed(PROTOCOL_ERROR));
        }
        Begin::Timeout => {
            let boot = Boot::new(Owner::new(events, watchdog), received, false, false);
            boot.finish(VoiceControlPhase::Failed, VoiceControlReason::StartTimeout)
                .await;
            return Err(Error::Managed(START_TIMEOUT));
        }
    };

    // From the start on, every event is in the version it was written in.
    events.speak(start.version);
    let mut boot = Boot::new(
        Owner::new(events, watchdog),
        received,
        start.muted,
        start.deafened,
    );
    boot.version = start.version;
    boot.tell();
    boot.owner.ack(start.id, None);
    // What was read after the start before it was taken is answered now, in order; if
    // one of them ends the start-up, the rest are answered with the final state.
    let mut early = early.into_iter();
    let halted = early.by_ref().find_map(|input| boot.on_input(Some(input)));
    boot.pending = early.collect();
    let Host {
        environment,
        transport,
        scrubber,
        devices,
        keys,
        timings,
        key_timings,
        ..
    } = host;
    let setup = Setup {
        environment: &environment,
        scrubber: &scrubber,
        keys: &keys,
        key_timings,
        inputs: &inputs,
    };
    let prepared = match halted {
        Some(halt) => Err(halt),
        None => boot.prepare(&setup, &start, devices, timings).await,
    };
    match prepared {
        Ok((settings, conchd)) => {
            let status = boot.ptt.status();
            let (key_state, talk_key) = match boot.key {
                KeyView::State { state, talk } => (Some(state), talk),
                KeyView::NotConfigured | KeyView::Unknown => (None, true),
            };
            let handoff = Handoff {
                owner: boot.owner,
                muted: status.muted(),
                deafened: status.deafened(),
                key_state,
                talk_key,
                switch: boot.switch,
            };
            session::serve(
                settings,
                conchd,
                transport,
                scrubber,
                boot.received,
                handoff,
            )
            .await
        }
        Err(Halt::Stop(reason)) => {
            if boot.finish(VoiceControlPhase::Stopped, reason).await {
                // A line read before the final state could not be acted on.
                return Err(Error::Managed(PROTOCOL_ERROR));
            }
            Ok(())
        }
        Err(Halt::Fail(reason, error)) => {
            boot.finish(VoiceControlPhase::Failed, reason).await;
            Err(error)
        }
    }
}

/// Waits for the first line, a timeout, or the owner going.
///
/// The reader hands the first line over before it sends any line after it, but the two
/// travel apart: the start's own channel, and the session's inputs. An input taken from the
/// session's channel may therefore be a line read after a start that is already waiting
/// in its own; it is never dropped, and is handed back to be answered after the start.
async fn wait_for_start(
    mut first: oneshot::Receiver<First>,
    received: &mut mpsc::UnboundedReceiver<Input>,
    limit: Duration,
) -> Begin {
    let deadline = tokio::time::sleep(limit);
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            biased;
            first = &mut first => return begin(first.ok(), Vec::new()),
            () = &mut deadline => return Begin::Timeout,
            input = received.recv() => match first.try_recv() {
                // The first line came before this input: it is answered after the start.
                Ok(first) => return begin(Some(first), input.into_iter().collect()),
                // No first line yet: this came from a signal or the watch on the parent,
                // not from the reader, whose lines all follow its first.
                Err(oneshot::error::TryRecvError::Empty) => match input {
                    Some(Input::Signal | Input::OwnerLost | Input::Eof) | None => {
                        return Begin::Gone;
                    }
                    Some(_) => {}
                },
                Err(oneshot::error::TryRecvError::Closed) => return Begin::Gone,
            },
        }
    }
}

/// What the first line came to, with the inputs taken after it.
fn begin(first: Option<First>, early: Vec<Input>) -> Begin {
    match first {
        Some(First::Start(start)) => Begin::Start(start, early),
        Some(First::Bad(code, version)) => Begin::Bad(code, version),
        Some(First::End) | None => Begin::Gone,
    }
}

/// What the start-up reads its settings from.
struct Setup<'a> {
    environment: &'a Environment,
    scrubber: &'a Arc<Scrubber>,
    keys: &'a KeySource,
    key_timings: KeyTimings,
    inputs: &'a mpsc::UnboundedSender<Input>,
}

/// The start-up, between the start's ack and the session loop: it answers the owner and
/// takes the mute and deafen keys as the loop would, with a machine of its own that only
/// keeps their state.
struct Boot {
    owner: Owner,
    received: mpsc::UnboundedReceiver<Input>,
    ptt: Ptt,
    key: KeyView,
    /// With a key device being watched: whether a talk key is bound on it.
    talk: Option<bool>,
    /// Inputs taken with the start and not yet looked at when the start-up ended.
    pending: Vec<Input>,
    /// The version the start was written in.
    version: Version,
    /// The devices `set_devices` named while starting (v2): kept for the session loop,
    /// which uses them as a switch would (§20.5).
    switch: DeviceSwitch,
    /// The talk mode `set_talk_mode` chose while starting (v2), which the session runs in
    /// whatever `voice.toml` says. Not said while starting: a state then is push-to-talk.
    talk_mode: Option<TalkMode>,
}

impl Boot {
    fn new(
        owner: Owner,
        received: mpsc::UnboundedReceiver<Input>,
        muted: bool,
        deafened: bool,
    ) -> Self {
        Self {
            owner,
            received,
            ptt: fresh_machine(muted, deafened),
            key: KeyView::Unknown,
            talk: None,
            pending: Vec::new(),
            version: Version::V1,
            switch: DeviceSwitch {
                input: None,
                output: None,
            },
            talk_mode: None,
        }
    }

    fn snapshot(&self, phase: VoiceControlPhase, reason: Option<VoiceControlReason>) -> Snapshot {
        Snapshot::new(
            phase,
            reason,
            None,
            Capture::Off,
            self.ptt.status(),
            self.key,
        )
    }

    /// Tells the owner the starting state, if it changed.
    fn tell(&mut self) {
        let snapshot = self.snapshot(VoiceControlPhase::Starting, None);
        self.owner.state(&snapshot, false);
    }

    /// The final state, and the end of the owner. Commands already read are answered
    /// before it, as a session on its way out answers them; after a protocol_error the
    /// failed state comes next and nothing else. Whether the final state is a failure.
    async fn finish(
        mut self,
        mut phase: VoiceControlPhase,
        mut reason: VoiceControlReason,
    ) -> bool {
        if reason != VoiceControlReason::ProtocolError {
            let mut read = std::mem::take(&mut self.pending);
            while let Ok(input) = self.received.try_recv() {
                read.push(input);
            }
            for input in read {
                let Input::Control(control) = input else {
                    continue;
                };
                match control {
                    Control::SetMute { id, .. }
                    | Control::SetDeafen { id, .. }
                    | Control::SetDevices { id, .. }
                    | Control::SetTalkMode { id, .. } => {
                        self.owner.ack(id, Some(VoiceControlErrorCode::Stopping));
                    }
                    Control::GetState { id } => {
                        let now = self.snapshot(VoiceControlPhase::Stopping, Some(reason));
                        self.owner.state(&now, true);
                        self.owner.ack(id, None);
                    }
                    Control::Leave { id } => self.owner.ack(id, None),
                    Control::Refused { id, code } => self.owner.ack(id, Some(code)),
                    Control::Fatal { code } => {
                        self.owner.protocol_error(code);
                        phase = VoiceControlPhase::Failed;
                        reason = VoiceControlReason::ProtocolError;
                        break;
                    }
                }
            }
        }
        let last = self.snapshot(phase, Some(reason));
        self.owner.finish(&last).await;
        phase == VoiceControlPhase::Failed
    }

    fn toggle(&mut self, command: LineCommand) {
        self.ptt.handle(Duration::ZERO, PttInput::Line(command));
        self.tell();
    }

    /// Stopping before the session loop: there is nothing to shut or close yet.
    fn stopping(&mut self, reason: VoiceControlReason) -> Option<Halt> {
        let snapshot = self.snapshot(VoiceControlPhase::Stopping, Some(reason));
        self.owner.state(&snapshot, false);
        Some(Halt::Stop(reason))
    }

    /// One input while starting. A halt ends the start-up.
    fn on_input(&mut self, input: Option<Input>) -> Option<Halt> {
        match input {
            Some(Input::Control(control)) => match control {
                Control::SetMute { id, muted } => {
                    if self.ptt.status().muted() != muted {
                        self.toggle(LineCommand::Mute);
                    }
                    self.owner.ack(id, None);
                    None
                }
                Control::SetDeafen { id, deafened } => {
                    if self.ptt.status().deafened() != deafened {
                        self.toggle(LineCommand::Deafen);
                    }
                    self.owner.ack(id, None);
                    None
                }
                Control::GetState { id } => {
                    let snapshot = self.snapshot(VoiceControlPhase::Starting, None);
                    self.owner.state(&snapshot, true);
                    self.owner.ack(id, None);
                    None
                }
                Control::Leave { id } => {
                    let halt = self.stopping(VoiceControlReason::Left);
                    self.owner.ack(id, None);
                    halt
                }
                // Nothing is open yet, and a state while starting says neither: what is
                // named is kept for the session loop, which uses it as a switch would, and
                // the ack has no state before it (§20.5).
                Control::SetDevices { id, switch } => {
                    if let Some(input) = switch.input {
                        self.switch.input = Some(input);
                    }
                    if let Some(output) = switch.output {
                        self.switch.output = Some(output);
                    }
                    self.owner.ack(id, None);
                    None
                }
                Control::SetTalkMode { id, talk_mode } => {
                    self.talk_mode = Some(talk_mode);
                    self.owner.ack(id, None);
                    None
                }
                Control::Refused { id, code } => {
                    self.owner.ack(id, Some(code));
                    None
                }
                Control::Fatal { code } => {
                    self.owner.protocol_error(code);
                    Some(Halt::Fail(
                        VoiceControlReason::ProtocolError,
                        Error::Managed(PROTOCOL_ERROR),
                    ))
                }
            },
            Some(Input::Signal) => self.stopping(VoiceControlReason::Signal),
            Some(Input::OwnerLost | Input::Eof) | None => {
                self.stopping(VoiceControlReason::OwnerLost)
            }
            Some(Input::Key(KeyEvent {
                key: Key::Mute,
                action: KeyAction::Press,
            })) => {
                self.toggle(LineCommand::Mute);
                None
            }
            Some(Input::Key(KeyEvent {
                key: Key::Deafen,
                action: KeyAction::Press,
            })) => {
                self.toggle(LineCommand::Deafen);
                None
            }
            Some(Input::KeyDevice(state)) => {
                if let Some(talk) = self.talk {
                    self.key = KeyView::State { state, talk };
                }
                None
            }
            // Not connected: a press does nothing yet. Standard input is the owner's, so
            // there are no line commands. A switch of device from the owner is a
            // `Control::SetDevices`, above; nothing else sends one.
            Some(Input::Key(_) | Input::Line(_) | Input::UnknownLine | Input::SwitchDevices(_)) => {
                None
            }
        }
    }

    /// Waits for `work` while answering the owner. Pending inputs are left for the session
    /// loop once the work is done.
    async fn wait<F: Future>(&mut self, work: F) -> Result<F::Output, Halt> {
        tokio::pin!(work);
        loop {
            tokio::select! {
                biased;
                done = &mut work => return Ok(done),
                input = self.received.recv() => {
                    if let Some(halt) = self.on_input(input) {
                        return Err(halt);
                    }
                }
            }
        }
    }

    /// The configuration, the login, the identity check, the audio and the key device, in
    /// that order (§8.1). Nothing is opened or asked before the step that needs it.
    async fn prepare(
        &mut self,
        setup: &Setup<'_>,
        start: &Start,
        devices: Devices,
        timings: Timings,
    ) -> Result<(Settings, Conchd), Halt> {
        use VoiceControlReason as Reason;
        let environment = setup.environment;
        // The configuration: [keys] and [audio] of voice.toml. Its `server` and `channel`
        // are for `join`, and are not consulted.
        let config_dir = default_config_dir(
            environment.conch_config_dir.as_deref(),
            environment.xdg_config_home.as_deref(),
            environment.home.as_deref(),
        )
        .map_err(|error| Halt::Fail(Reason::BadConfig, Error::Api(error)))?;
        let config = match &config_dir {
            Some(dir) => cli::read_config(&dir.join("voice.toml"))
                .map_err(|error| Halt::Fail(Reason::BadConfig, error))?,
            None => Config::default(),
        };
        let keys = key_source(&config, setup.keys)
            .map_err(|error| Halt::Fail(Reason::BadConfig, error))?;
        self.key = match &keys {
            Some(_) => KeyView::Unknown,
            None => KeyView::NotConfigured,
        };

        // The login, from where the start says and nowhere else.
        let server = ServerAddress::parse(&start.server)
            .map_err(|error| Halt::Fail(Reason::InternalError, Error::Api(error)))?;
        let token = match start.credential {
            VoiceControlCredential::Stored => {
                if environment.token.is_some() {
                    return Err(Halt::Fail(
                        Reason::BadEnvironment,
                        Error::Managed(BAD_ENVIRONMENT),
                    ));
                }
                resolve_token(None, &server, config_dir.as_deref())
            }
            VoiceControlCredential::Env => resolve_token(
                environment
                    .token
                    .as_ref()
                    .map(conch_voice_api::Secret::expose),
                &server,
                None,
            ),
        }
        .map_err(|error| Halt::Fail(Reason::NotSignedIn, Error::Api(error)))?;
        setup.scrubber.always(&token);
        let conchd =
            Conchd::new(&server, &token, REQUEST_TIMEOUT, REPORT_TIMEOUT).map_err(|error| {
                match error {
                    ApiError::InvalidToken => Halt::Fail(Reason::NotSignedIn, Error::Api(error)),
                    error => Halt::Fail(Reason::InternalError, Error::Api(error)),
                }
            })?;

        // Who that login is: the account the owner named, or nothing is opened (§9).
        let whoami = self
            .wait(conchd.client.whoami())
            .await?
            .map_err(|error| Halt::Fail(whoami_reason(&error), Error::Api(error)))?;
        if whoami.id != start.principal_id {
            return Err(Halt::Fail(
                Reason::IdentityMismatch,
                Error::Managed(IDENTITY_MISMATCH),
            ));
        }
        if whoami.is_agent() {
            return Err(Halt::Fail(Reason::NotAPerson, Error::Managed(NOT_A_PERSON)));
        }
        if whoami.voice_withheld() {
            return Err(Halt::Fail(
                Reason::VoiceNotAllowed,
                Error::Managed(VOICE_NOT_ALLOWED),
            ));
        }

        // PipeWire and the speakers. The microphone is the session loop's to open.
        let audio_unavailable = |error| Halt::Fail(Reason::AudioUnavailable, error);
        let (open_mic, sink, sink_tones) = match devices {
            Devices::Choices { mic, sink } => {
                let pipewire = if mic == MicChoice::PipeWire || sink == SinkChoice::PipeWire {
                    let connecting = tokio::task::spawn_blocking(PipeWire::connect);
                    let connected = self.wait(connecting).await?.map_err(|_| {
                        Halt::Fail(
                            Reason::InternalError,
                            Error::Managed("the connection to PipeWire was lost"),
                        )
                    })?;
                    Some(connected.map_err(audio_unavailable)?)
                } else {
                    None
                };
                let sink_tones = match &sink {
                    SinkChoice::Count(tones) => tones.clone(),
                    SinkChoice::PipeWire => Vec::new(),
                };
                let open_mic = crate::mic_opener(mic, pipewire.clone());
                let open_sink = crate::sink_opener(sink, pipewire);
                (open_mic, open_sink, sink_tones)
            }
            Devices::Given {
                open_mic,
                open_sink,
                sink_tones,
            } => (open_mic, open_sink, sink_tones),
        };
        // The speakers are opened now, so that speakers that cannot be opened fail the
        // start: those the owner named already, if they can be opened by their name, or else
        // those `voice.toml` names, as written. (Speakers the owner named that cannot be
        // opened by their name are its switch's to refuse, once the session runs, and
        // nothing is opened for them.) The session's first call of the opener is given
        // these if it asks for the same speakers, as it does; for any others it opens those
        // by name, so that nothing asked for by name is ever answered with speakers it did
        // not name. A switch later opens others.
        let output = match self
            .switch
            .output
            .as_deref()
            .and_then(session::switched_name)
        {
            Some(named) => named,
            None => session::configured_name(config.audio.output.as_deref()),
        };
        let mut sink = sink;
        let speakers: BoxedSink = sink(output.as_deref()).map_err(audio_unavailable)?;
        let mut opened = Some((output.clone(), speakers));
        let open_sink: OpenSink = Box::new(move |name| match opened.take() {
            Some((with, speakers)) if with.as_deref() == name => Ok(speakers),
            // Other speakers: the ones opened here are dropped, which closes them.
            Some(_) | None => sink(name),
        });

        // The key device, tried before the first connection: the first state of the loop
        // says whether hold-to-talk can work.
        let key_device = match keys {
            Some((device, bindings, rule)) => {
                let name = device.display().to_string();
                let talk = bindings.talk().is_some();
                keydev::spawn(
                    device,
                    bindings,
                    rule,
                    setup.key_timings,
                    setup.inputs.clone(),
                )
                .map_err(|error| Halt::Fail(Reason::InternalError, Error::Runtime(error)))?;
                self.talk = Some(talk);
                self.wait_for_key().await?;
                Some(name)
            }
            None => None,
        };

        let talk_mode = match self.version {
            Version::V1 => v1_talk_mode(config.audio.talk_mode),
            // What the owner chose while starting, or else what the file says (§20.6).
            Version::V2 => self.talk_mode.unwrap_or(config.audio.talk_mode),
        };
        let settings = Settings {
            channel: start.channel.clone(),
            open_mic,
            open_sink,
            input: config.audio.input.clone(),
            output,
            sink_tones,
            release_tail_ms: config.audio.release_tail_ms,
            max_transmit: Duration::from_secs(u64::from(config.audio.max_transmit_secs)),
            talk_mode,
            level: crate::transmit::level_config(
                config.audio.open_threshold_db,
                config.audio.open_hang_ms,
            ),
            key_device,
            unreachable_note: None,
            timings,
        };
        Ok((settings, conchd))
    }

    /// Waits for the key device's watcher to say whether it is reading the device.
    async fn wait_for_key(&mut self) -> Result<(), Halt> {
        loop {
            let input = self.received.recv().await;
            let first_word = matches!(input, Some(Input::KeyDevice(_)));
            if let Some(halt) = self.on_input(input) {
                return Err(halt);
            }
            if first_word {
                return Ok(());
            }
        }
    }
}

/// The one log line of a managed session whose `voice.toml` asks for the open microphone.
pub const V1_PUSH_TO_TALK: &str = "managed: talk_mode = \"open\" in voice.toml is not used: a conch.voice_control.v1 session is push-to-talk";

/// The talk mode a `conch.voice_control.v1` session runs in. A v1 state cannot say "open
/// microphone" (`docs/design/tui-voice.md` §20.1): a v1 session is push-to-talk whatever
/// the file says, which sends less, never more (§20.6). When the file asked for open mode,
/// one log line says so, for `/voice status` to show. A v2 session honours the file.
fn v1_talk_mode(configured: TalkMode) -> TalkMode {
    if configured == TalkMode::Open {
        log::warn!("{V1_PUSH_TO_TALK}");
    }
    TalkMode::PushToTalk
}

/// What a failed whoami is told to the owner as.
fn whoami_reason(error: &ApiError) -> VoiceControlReason {
    use VoiceControlReason as Reason;
    match error {
        ApiError::Unauthenticated(_) => Reason::NotSignedIn,
        ApiError::Forbidden(refusal) if refusal.code == "voice_not_allowed" => {
            Reason::VoiceNotAllowed
        }
        ApiError::Forbidden(_) => Reason::NotAPerson,
        ApiError::VoiceRequiresAuth(_) => Reason::VoiceRequiresAuth,
        ApiError::Timeout
        | ApiError::Connect { .. }
        | ApiError::Transport { .. }
        | ApiError::UnexpectedResponse { .. }
        | ApiError::Undecodable { .. } => Reason::ServerUnreachable,
        ApiError::Refused(refusal) if refusal.status >= 500 => Reason::ServerUnreachable,
        _ => Reason::InternalError,
    }
}

/// The key device to read, the keys to look for on it and the rule it is read under, if
/// any: the one `voice.toml` names, as for `join`, or the test option's named pipe with the
/// keys `voice.toml` binds.
fn key_source(
    config: &Config,
    keys: &KeySource,
) -> Result<Option<(PathBuf, KeyBindings, DeviceRule)>, Error> {
    match keys {
        KeySource::Configured => Ok(cli::key_device(config, false)?
            .map(|(device, bindings)| (device, bindings, DeviceRule::EventDevice))),
        KeySource::NamedPipe(pipe) => {
            let bindings = config.key_bindings()?;
            Ok((!bindings.is_empty())
                .then(|| (pipe.clone(), bindings, DeviceRule::NamedPipeForTests)))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn line(text: &str) -> Bounded<'_> {
        Bounded::Line(text.as_bytes())
    }

    const START: &str = r#"{"schema":"conch.voice_control.v1","kind":"start","id":1,"server":"https://conch.example.org","channel":"general","principal_id":7,"credential":"stored","muted":false,"deafened":false}"#;

    #[test]
    fn the_first_line_must_be_a_valid_start() {
        assert_eq!(first_line(line(START)).unwrap().id, 1);
        assert_eq!(first_line(line(START)).unwrap().version, Version::V1);
        let table = [
            ("", VoiceControlErrorCode::Malformed),
            ("leave", VoiceControlErrorCode::Malformed),
            (
                r#"{"schema":"conch.voice_control.v3","kind":"start","id":1}"#,
                VoiceControlErrorCode::UnknownSchema,
            ),
            // A version the hello offers is read by its own reader: this v2 start lacks its
            // fields.
            (
                r#"{"schema":"conch.voice_control.v2","kind":"start","id":1}"#,
                VoiceControlErrorCode::Invalid,
            ),
            (
                r#"{"schema":"conch.voice_control.v2","kind":"set_talk_mode","id":1,"talk_mode":"open"}"#,
                VoiceControlErrorCode::NotStarted,
            ),
            (
                r#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":1,"muted":true}"#,
                VoiceControlErrorCode::NotStarted,
            ),
            (
                r#"{"schema":"conch.voice_control.v1","kind":"press","id":1}"#,
                VoiceControlErrorCode::NotStarted,
            ),
            (
                r#"{"schema":"conch.voice_control.v1","kind":"start","id":1}"#,
                VoiceControlErrorCode::Invalid,
            ),
        ];
        for (text, code) in table {
            assert_eq!(first_line(line(text)).unwrap_err(), code, "{text}");
        }
        assert_eq!(
            first_line(Bounded::TooLong).unwrap_err(),
            VoiceControlErrorCode::TooLong
        );
    }

    const START_V2: &str = r#"{"schema":"conch.voice_control.v2","kind":"start","id":1,"server":"https://conch.example.org","channel":"general","principal_id":7,"credential":"stored","muted":true,"deafened":false}"#;

    /// The start fixes the version: a start in v2 makes a v2 session, and a first line that
    /// is not a valid start is answered in v2 only when it is a line of v2 (§20.2).
    #[test]
    fn a_start_in_v2_makes_a_v2_session_and_a_bad_first_line_is_answered_in_its_version() {
        let start = first_line(line(START_V2)).unwrap();
        assert_eq!(start.version, Version::V2);
        assert_eq!(
            (start.id, start.principal_id, start.muted, start.deafened),
            (1, 7, true, false)
        );
        for (text, version) in [
            (START, Version::V1),
            (START_V2, Version::V2),
            (
                r#"{"schema":"conch.voice_control.v2","kind":"start","id":1}"#,
                Version::V2,
            ),
            (
                r#"{"schema":"conch.voice_control.v2","kind":"leave"}"#,
                Version::V2,
            ),
            (
                r#"{"schema":"conch.voice_control.v3","kind":"start","id":1}"#,
                Version::V1,
            ),
            ("start", Version::V1),
        ] {
            assert_eq!(version_named(line(text)), version, "{text}");
        }
        assert_eq!(version_named(Bounded::TooLong), Version::V1);
    }

    /// After a start in v2, lines are read as v2: the two new commands, a v1 line refused
    /// as unknown_schema (which ends the session), and v1's own refusals.
    #[test]
    fn after_a_v2_start_lines_are_read_as_v2_and_a_v1_line_ends_the_session() {
        let mut later = Later::after_in(1, Version::V2);
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v2","kind":"set_devices","id":2,"input":"speech_out"}"#
            )),
            Control::SetDevices {
                id: 2,
                switch: DeviceSwitch {
                    input: Some("speech_out".to_owned()),
                    output: None,
                },
            }
        );
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v2","kind":"set_talk_mode","id":3,"talk_mode":"open"}"#
            )),
            Control::SetTalkMode {
                id: 3,
                talk_mode: TalkMode::Open,
            }
        );
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v2","kind":"set_devices","id":4,"input":""}"#
            )),
            Control::Refused {
                id: 4,
                code: VoiceControlErrorCode::Invalid,
            }
        );
        assert_eq!(
            later.line(line(START_V2.replace("\"id\":1", "\"id\":5").as_str())),
            Control::Refused {
                id: 5,
                code: VoiceControlErrorCode::AlreadyStarted,
            }
        );
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":6,"muted":true}"#
            )),
            Control::Fatal {
                code: VoiceControlErrorCode::UnknownSchema,
            }
        );
        // And in a v1 session, a kind of v2 is one v1 does not know: refused, not fatal.
        let mut later = Later::after(1);
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"set_talk_mode","id":2,"talk_mode":"open"}"#
            )),
            Control::Refused {
                id: 2,
                code: VoiceControlErrorCode::UnknownKind,
            }
        );
    }

    #[test]
    fn later_lines_are_answered_refused_or_fatal_and_ids_only_go_up() {
        let mut later = Later::after(1);
        let set_mute =
            r#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":2,"muted":true}"#;
        assert_eq!(
            later.line(line(set_mute)),
            Control::SetMute { id: 2, muted: true }
        );
        assert_eq!(
            later.line(line(START.replace("\"id\":1", "\"id\":3").as_str())),
            Control::Refused {
                id: 3,
                code: VoiceControlErrorCode::AlreadyStarted
            }
        );
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"set_volume","id":4,"level":3}"#
            )),
            Control::Refused {
                id: 4,
                code: VoiceControlErrorCode::UnknownKind
            }
        );
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"set_mute","id":5,"muted":"yes"}"#
            )),
            Control::Refused {
                id: 5,
                code: VoiceControlErrorCode::Invalid
            }
        );
        // Not greater than the one before: refused or not, it ends the session.
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"get_state","id":5}"#
            )),
            Control::Fatal {
                code: VoiceControlErrorCode::IdNotIncreasing
            }
        );
        let mut later = Later::after(1);
        assert_eq!(
            later.line(line(
                r#"{"schema":"conch.voice_control.v1","kind":"press","id":1}"#
            )),
            Control::Fatal {
                code: VoiceControlErrorCode::IdNotIncreasing
            }
        );
        for (text, code) in [
            ("{", VoiceControlErrorCode::Malformed),
            ("down", VoiceControlErrorCode::Malformed),
            (
                r#"{"kind":"set_mute","id":9,"muted":true}"#,
                VoiceControlErrorCode::UnknownSchema,
            ),
            (
                r#"{"schema":"conch.voice_control.v1","kind":"set_mute","muted":true}"#,
                VoiceControlErrorCode::Invalid,
            ),
        ] {
            assert_eq!(later.line(line(text)), Control::Fatal { code }, "{text}");
        }
        assert_eq!(
            later.line(Bounded::TooLong),
            Control::Fatal {
                code: VoiceControlErrorCode::TooLong
            }
        );
    }

    fn status(inputs: &[PttInput]) -> PttStatus {
        let (mut ptt, _) = Ptt::new(Duration::from_secs(120));
        for input in inputs {
            ptt.handle(Duration::ZERO, *input);
        }
        ptt.status()
    }

    /// Every snapshot this module assembles holds together by the schema's rules, in every
    /// phase, whatever the machine and the devices say.
    #[test]
    fn every_snapshot_holds_together() {
        let connected = [
            PttInput::Connected(true),
            PttInput::PublishGrant(true),
            PttInput::Microphone(true),
        ];
        let statuses = [
            status(&[]),
            status(&connected),
            status(&[
                PttInput::Connected(true),
                PttInput::PublishGrant(true),
                PttInput::Microphone(true),
                PttInput::KeyDevice(true),
                PttInput::Line(LineCommand::Mute),
                PttInput::Line(LineCommand::Deafen),
            ]),
            status(&[
                PttInput::Connected(true),
                PttInput::PublishGrant(true),
                PttInput::Microphone(true),
                PttInput::KeyDevice(true),
                PttInput::KeyDevice(false),
            ]),
        ];
        let keys = [
            KeyView::NotConfigured,
            KeyView::State {
                state: DeviceState::Ready,
                talk: true,
            },
            KeyView::State {
                state: DeviceState::Ready,
                talk: false,
            },
            KeyView::State {
                state: DeviceState::Missing(Problem::Ended),
                talk: true,
            },
        ];
        for status in statuses {
            let lost = status.blocked.contains(ShutReason::KeyDeviceLost);
            for key in keys {
                let key = if lost {
                    KeyView::State {
                        state: DeviceState::Lost(Problem::Ended),
                        talk: true,
                    }
                } else {
                    key
                };
                let capture = if status.blocked.contains(ShutReason::NoMicrophone) {
                    Capture::Off
                } else {
                    Capture::On
                };
                let cases = [
                    (VoiceControlPhase::Connecting, None, None, capture),
                    (VoiceControlPhase::Connected, None, None, capture),
                    (
                        VoiceControlPhase::Reconnecting,
                        Some(VoiceControlReason::RoomDeleted),
                        Some(Duration::ZERO),
                        capture,
                    ),
                    (VoiceControlPhase::Reconnecting, None, None, capture),
                    (
                        VoiceControlPhase::Stopping,
                        Some(VoiceControlReason::Left),
                        None,
                        capture,
                    ),
                    (
                        VoiceControlPhase::Stopped,
                        Some(VoiceControlReason::OwnerLost),
                        None,
                        Capture::Off,
                    ),
                    (
                        VoiceControlPhase::Failed,
                        Some(VoiceControlReason::DuplicateIdentity),
                        None,
                        Capture::Off,
                    ),
                ];
                for (phase, reason, retry, capture) in cases {
                    let snapshot = Snapshot::new(phase, reason, retry, capture, status, key);
                    to_line(&snapshot.event(5)).unwrap_or_else(|e| panic!("{snapshot:?}: {e}"));
                }
            }
        }
        // While starting, before the configuration was read.
        let starting = Snapshot::new(
            VoiceControlPhase::Starting,
            None,
            None,
            Capture::Off,
            fresh_status(true, false),
            KeyView::Unknown,
        );
        assert!(to_line(&starting.event(2)).is_ok());
        assert_eq!(
            starting.blocked,
            [
                VoiceControlBlock::NotConnected,
                VoiceControlBlock::NoPublishGrant,
                VoiceControlBlock::NoMicrophone,
                VoiceControlBlock::Muted
            ]
        );
    }

    /// Every v2 snapshot holds together by v2's rules, in either talk mode, whatever the
    /// machine, the speakers and the key device say: transmitting in open mode with no talk
    /// key or with the key device lost, speakers that are not there, and the phases where
    /// there is no stream (`docs/design/tui-voice.md` §20.4).
    #[test]
    fn every_v2_snapshot_holds_together_in_either_mode() {
        for mode in TalkMode::ALL {
            let machine = |inputs: &[PttInput]| {
                let (mut ptt, _) = Ptt::with_talk_mode(Duration::from_secs(120), mode);
                for input in inputs {
                    ptt.handle(Duration::ZERO, *input);
                }
                ptt.status()
            };
            let ready = [
                PttInput::Connected(true),
                PttInput::PublishGrant(true),
                PttInput::Microphone(true),
                PttInput::KeyDevice(true),
            ];
            let transmitting: Vec<PttInput> = match mode {
                TalkMode::PushToTalk => [&ready[..], &[PttInput::Line(LineCommand::Down)]].concat(),
                TalkMode::Open => {
                    [&ready[..], &[PttInput::Sound(false), PttInput::Sound(true)]].concat()
                }
            };
            let lost = [&transmitting[..], &[PttInput::KeyDevice(false)]].concat();
            let cases = [
                (machine(&[]), KeyView::NotConfigured),
                (machine(&ready), KeyView::NotConfigured),
                (
                    machine(&transmitting),
                    KeyView::State {
                        state: DeviceState::Ready,
                        talk: true,
                    },
                ),
                (
                    machine(&lost),
                    KeyView::State {
                        state: DeviceState::Lost(Problem::Ended),
                        talk: true,
                    },
                ),
            ];
            assert!(cases[2].0.transmitting, "{mode}: transmitting");
            assert_eq!(
                cases[3].0.transmitting,
                mode == TalkMode::Open,
                "{mode}: the key device lost ends a press, not an opening"
            );
            for (status, key) in cases {
                let capture = if status.blocked.contains(ShutReason::NoMicrophone) {
                    Capture::Off
                } else {
                    Capture::On
                };
                let phases = [
                    (VoiceControlPhase::Connecting, None, capture),
                    (VoiceControlPhase::Connected, None, capture),
                    (
                        VoiceControlPhase::Reconnecting,
                        Some(VoiceControlReason::ConnectionLost),
                        capture,
                    ),
                    (
                        VoiceControlPhase::Stopping,
                        Some(VoiceControlReason::Left),
                        capture,
                    ),
                    (
                        VoiceControlPhase::Stopped,
                        Some(VoiceControlReason::Left),
                        Capture::Off,
                    ),
                    (
                        VoiceControlPhase::Failed,
                        Some(VoiceControlReason::ProtocolError),
                        Capture::Off,
                    ),
                ];
                for (phase, reason, capture) in phases {
                    // The machine is told the connection is gone before any other phase is
                    // shown, which ends what it was sending.
                    if status.transmitting && phase != VoiceControlPhase::Connected {
                        continue;
                    }
                    for playback in [VoiceControlPlayback::On, VoiceControlPlayback::Unavailable] {
                        let snapshot = Snapshot::new(phase, reason, None, capture, status, key)
                            .with_playback_and_mode(playback, mode);
                        to_line(&snapshot.event_v2(5))
                            .unwrap_or_else(|e| panic!("{mode}: {snapshot:?}: {e}"));
                        // The v1 event of the same, in push-to-talk, as v1 has always had it.
                        if mode == TalkMode::PushToTalk {
                            to_line(&snapshot.event(5))
                                .unwrap_or_else(|e| panic!("{snapshot:?}: {e}"));
                        }
                    }
                }
            }
            // While starting the configuration has not been read: push-to-talk, and no
            // stream, whatever is passed.
            let starting = Snapshot::new(
                VoiceControlPhase::Starting,
                None,
                None,
                Capture::Off,
                fresh_status(false, false),
                KeyView::Unknown,
            )
            .with_playback_and_mode(VoiceControlPlayback::On, mode);
            assert_eq!(
                (starting.playback, starting.talk_mode),
                (VoiceControlPlayback::Off, VoiceControlTalkMode::PushToTalk)
            );
            assert!(to_line(&starting.event_v2(2)).is_ok());
        }
    }

    /// A v1 owner is not told of a change it cannot see: the speakers or the talk mode
    /// changing writes no v1 state, as before v2 existed.
    #[test]
    fn a_change_a_v1_state_cannot_say_writes_no_state_to_a_v1_owner() {
        use crate::lines::tests::Stallable;
        let connected = Snapshot::new(
            VoiceControlPhase::Connected,
            None,
            None,
            Capture::On,
            status(&[
                PttInput::Connected(true),
                PttInput::PublishGrant(true),
                PttInput::Microphone(true),
            ]),
            KeyView::NotConfigured,
        );
        for (version, written) in [(Version::V1, 1), (Version::V2, 3)] {
            let writer = Stallable::default();
            let mut events = Events::spawn(Box::new(writer.clone()));
            events.speak(version);
            let mut owner = Owner::new(events, Watchdog::off());
            for playback in [
                VoiceControlPlayback::On,
                VoiceControlPlayback::Unavailable,
                VoiceControlPlayback::Unavailable,
            ] {
                owner.state(
                    &connected
                        .clone()
                        .with_playback_and_mode(playback, TalkMode::PushToTalk),
                    false,
                );
            }
            owner.state(
                &connected
                    .clone()
                    .with_playback_and_mode(VoiceControlPlayback::Unavailable, TalkMode::Open),
                false,
            );
            writer.until_lines(written);
            std::thread::sleep(Duration::from_millis(20));
            let text = writer.text();
            assert_eq!(text.lines().count(), written, "{version:?}: {text}");
            for line in text.lines() {
                assert!(line.contains(version.schema()), "{line}");
            }
        }
    }

    #[test]
    fn a_wait_longer_than_the_protocol_takes_is_said_as_its_longest() {
        let snapshot = Snapshot::new(
            VoiceControlPhase::Reconnecting,
            Some(VoiceControlReason::ServerUnreachable),
            Some(Duration::from_secs(3600)),
            Capture::Off,
            status(&[]),
            KeyView::NotConfigured,
        );
        assert_eq!(snapshot.retry_in_ms, Some(VOICE_CONTROL_MAX_RETRY_IN_MS));
    }

    fn starting() -> Snapshot {
        Snapshot::new(
            VoiceControlPhase::Starting,
            None,
            None,
            Capture::Off,
            fresh_status(false, false),
            KeyView::Unknown,
        )
    }

    fn seqs_of(text: &str) -> Vec<i64> {
        text.lines()
            .map(|line| conch_voice_api::parse_event(line.as_bytes()).unwrap().seq())
            .collect()
    }

    /// The events are numbered from 1 and a dropped one leaves a gap, never a repeat.
    #[test]
    fn events_are_numbered_and_a_dropped_one_leaves_a_gap() {
        use crate::lines::tests::Stallable;
        let writer = Stallable::stalled();
        let mut events = Events::spawn(Box::new(writer.clone()));
        events.hello();
        writer.until_waiting();
        // The hello is in the writer's hands; this many more fit; the rest are dropped.
        // States, which may be dropped: an ack would be kept (the test below).
        for _ in 0..VOICE_CONTROL_EVENT_QUEUE_LINES + 10 {
            events.state(&starting(), Keep::IfThereIsRoom);
        }
        writer.stall(false);
        writer.until_lines(1 + VOICE_CONTROL_EVENT_QUEUE_LINES);
        events.ack(999, None);
        let lines = loop {
            let text = writer.text();
            if text.lines().count() == VOICE_CONTROL_EVENT_QUEUE_LINES + 2 {
                break text;
            }
            std::thread::sleep(Duration::from_millis(1));
        };
        let seqs = seqs_of(&lines);
        assert_eq!(seqs[0], 1);
        assert!(seqs.windows(2).all(|pair| pair[1] > pair[0]), "{seqs:?}");
        let last = seqs[seqs.len() - 1];
        assert_eq!(
            last,
            VOICE_CONTROL_EVENT_QUEUE_LINES as i64 + 12,
            "a gap of ten"
        );
    }

    /// With the queue full, an ack and the state that answers `get_state` are still
    /// written, after what waited before them, as many as the owner may have unanswered;
    /// past that they are dropped too, so the queue keeps a bound (#571).
    #[test]
    fn answers_are_kept_behind_a_full_queue_and_their_places_have_a_bound() {
        use crate::lines::tests::Stallable;
        use conch_voice_api::VoiceControlEvent;
        let writer = Stallable::stalled();
        let watchdog = Watchdog::off();
        let mut owner = Owner::new(Events::spawn(Box::new(writer.clone())), watchdog);
        owner.events.hello();
        writer.until_waiting();
        let full = VOICE_CONTROL_EVENT_QUEUE_LINES + 10;
        for _ in 0..full {
            owner.events.state(&starting(), Keep::IfThereIsRoom);
        }
        // As many commands as the owner may have unanswered, each a get_state: a state,
        // then the ack.
        for id in 1..=VOICE_CONTROL_MAX_IN_FLIGHT as i64 {
            owner.state(&starting(), true);
            owner.ack(id, None);
        }
        // Past the contract: dropped, as any event is that meets a full queue.
        owner.ack(100, None);
        owner.state(&starting(), true);
        writer.stall(false);
        let answered = 1 + VOICE_CONTROL_EVENT_QUEUE_LINES + ANSWER_LINES;
        writer.until_lines(answered);
        // Something written now shows the second gap, of the answers past the contract.
        owner.ack(101, None);
        writer.until_lines(answered + 1);

        let text = writer.text();
        let events: Vec<_> = text
            .lines()
            .map(|line| conch_voice_api::parse_event(line.as_bytes()).unwrap())
            .collect();
        let seqs = seqs_of(&text);
        assert!(seqs.windows(2).all(|pair| pair[1] > pair[0]), "{seqs:?}");
        // The hello and the states that fitted, then a gap, then every answer in order.
        let first_answer = 1 + VOICE_CONTROL_EVENT_QUEUE_LINES;
        assert_eq!(
            seqs[first_answer],
            2 + full as i64,
            "a gap of ten before them"
        );
        let acks: Vec<i64> = events
            .iter()
            .filter_map(|event| match event {
                VoiceControlEvent::Ack(ack) => Some(ack.id),
                _ => None,
            })
            .collect();
        let mut wanted: Vec<i64> = (1..=VOICE_CONTROL_MAX_IN_FLIGHT as i64).collect();
        wanted.push(101);
        assert_eq!(acks, wanted);
        // Each get_state's ack just after its state.
        for (at, event) in events.iter().enumerate().skip(first_answer) {
            let VoiceControlEvent::Ack(ack) = event else {
                continue;
            };
            if ack.id == 101 {
                continue;
            }
            let VoiceControlEvent::State(state) = &events[at - 1] else {
                panic!("the ack of {} is not just after a state", ack.id);
            };
            assert_eq!(state.seq + 1, ack.seq, "the ack of {}", ack.id);
        }
        assert_eq!(
            seqs[seqs.len() - 1] - seqs[seqs.len() - 2],
            3,
            "the two past the contract were dropped, and the gap shows"
        );
    }

    /// Not a test of its own: run as a process by the test below, it arms the watchdog and
    /// then hangs, as a helper whose session never ends would.
    #[test]
    fn child_process_hangs_with_the_watchdog_armed() {
        let Ok(how) = std::env::var("CONCH_VOICE_WATCHDOG_CHILD") else {
            return;
        };
        let watchdog = Watchdog::process();
        if how == "failed" {
            // As the owner is told of a failure (a protocol_error, a failed state).
            watchdog.failed();
        }
        watchdog.arm(Duration::from_millis(800));
        loop {
            std::thread::park();
        }
    }

    /// Runs the child above, armed after `how` ("stopped" or "failed"). Its exit code
    /// and how long it took.
    fn watched_child(how: &str) -> (Option<i32>, Duration) {
        use std::process::{Command, Stdio};
        use std::time::Instant;
        let began = Instant::now();
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "managed::tests::child_process_hangs_with_the_watchdog_armed",
                "--nocapture",
                "--test-threads=1",
            ])
            .env("CONCH_VOICE_WATCHDOG_CHILD", how)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let status = loop {
            if let Some(status) = child.try_wait().unwrap() {
                break status;
            }
            if began.elapsed() > Duration::from_secs(20) {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("the watchdog did not end the process");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        (status.code(), began.elapsed())
    }

    #[test]
    fn the_watchdog_ends_a_process_that_does_not_end_by_itself_within_its_bound() {
        // A session that was stopping exits 0; one the owner was told is failing exits 1,
        // so that the owner never reads a failure as a stop.
        for (how, code) in [("stopped", 0), ("failed", 1)] {
            let (status, took) = watched_child(how);
            assert_eq!(status, Some(code), "{how}");
            // The ordinary exit is tried EXIT_GRACE before the bound, and here it works;
            // the child would otherwise never have ended. The upper bound allows for the
            // time a test binary takes to start.
            assert!(
                took >= Duration::from_millis(800) - EXIT_GRACE,
                "{how}: {took:?}"
            );
            assert!(took < Duration::from_secs(10), "{how}: {took:?}");
        }
    }

    #[test]
    fn the_owner_told_of_a_failure_makes_the_watchdogs_code_a_failures() {
        use crate::lines::tests::Stallable;
        let snapshot = |phase, reason| {
            Snapshot::new(
                phase,
                reason,
                None,
                Capture::Off,
                status(&[]),
                KeyView::NotConfigured,
            )
        };
        let stop = [
            snapshot(VoiceControlPhase::Starting, None),
            snapshot(VoiceControlPhase::Stopping, Some(VoiceControlReason::Left)),
            snapshot(
                VoiceControlPhase::Stopped,
                Some(VoiceControlReason::OwnerLost),
            ),
        ];
        let failure = [
            snapshot(
                VoiceControlPhase::Stopping,
                Some(VoiceControlReason::DuplicateIdentity),
            ),
            snapshot(
                VoiceControlPhase::Failed,
                Some(VoiceControlReason::IdentityMismatch),
            ),
        ];
        for state in &stop {
            let watchdog = Watchdog::off();
            let mut owner = Owner::new(
                Events::spawn(Box::new(Stallable::default())),
                watchdog.clone(),
            );
            owner.state(state, false);
            assert_eq!(watchdog.exit_code(), 0, "{state:?}");
        }
        for state in &failure {
            let watchdog = Watchdog::off();
            let mut owner = Owner::new(
                Events::spawn(Box::new(Stallable::default())),
                watchdog.clone(),
            );
            owner.state(state, false);
            assert_eq!(watchdog.exit_code(), 1, "{state:?}");
        }
        let watchdog = Watchdog::off();
        let mut owner = Owner::new(
            Events::spawn(Box::new(Stallable::default())),
            watchdog.clone(),
        );
        owner.protocol_error(VoiceControlErrorCode::Malformed);
        assert_eq!(watchdog.exit_code(), 1);
    }

    #[test]
    fn a_watchdog_that_is_off_never_fires_and_a_later_bound_changes_nothing() {
        // Off: arming it at once does nothing, or this test would not get to its end.
        Watchdog::off().arm(Duration::ZERO);
        assert!(!Watchdog::off().sooner(Duration::ZERO));
        // Which bounds would start a thread: only one sooner than any set before.
        let watchdog = Watchdog::process();
        assert!(watchdog.sooner(Duration::from_secs(7200)));
        assert!(!watchdog.sooner(Duration::from_secs(10_800)));
        assert!(watchdog.sooner(Duration::from_secs(3600)));
        assert!(!watchdog.sooner(Duration::from_secs(3600)));
    }
}
