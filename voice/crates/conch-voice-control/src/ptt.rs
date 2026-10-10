//! The push-to-talk state machine (`docs/design/conch-voice.md` §3, §4 and §6 "Client side").
//!
//! [`Ptt`] is told what happened and when, and answers with what to do: commands for the
//! transmit gate, transmit reports to send, what to show, and when to quit. It does none of
//! it itself, and it reads no clock: every call is given the time.
//!
//! The rules it keeps:
//!
//! - **The gate is opened by a press and by nothing else.** No condition clearing, no
//!   reconnection and no timer opens it. A key that was held when the gate was forced shut
//!   has to be pressed again.
//! - **While any [`ShutReason`] stands, no press opens the gate.** Each reason is passed on
//!   to the gate when it starts ([`GateCommand::ForceShut`]) and when it ends
//!   ([`GateCommand::Clear`]), so the gate can refuse on its own account as well.
//! - **One `started` and one `stopped` per transmission, in that order.** `started` is
//!   produced with the command that opens the gate. `stopped` is produced when the caller
//!   says the gate has actually shut ([`PttInput::GateShut`]), because the release tail
//!   belongs to the gate and counts as part of the transmission.
//! - **The gate is never told to open while it is still shutting.** A press during the
//!   release tail waits for the gate to report shut, and then opens it as a new transmission.
//!   So the reports of two quick presses cannot run together, whatever the gate does with
//!   its tail.
//!
//! Where the caller's reports about the gate go wrong, the machine fails closed. If
//! [`PttInput::GateShut`] is never delivered after a shut, nothing opens again. If one is
//! delivered during a transmission that nothing ended, the machine ends it and commands the
//! gate shut, so the gate is not left open while the machine believes it shut.

use std::fmt;
use std::str::FromStr;
use std::time::Duration;

use crate::error::Error;
use crate::keys::{Key, KeyAction, KeyEvent};

/// Why the gate is shut whatever the talk key is doing (the note's §3).
///
/// The order is the order of importance: when several stand, the first is the one a refused
/// press is answered with.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ShutReason {
    /// There is no established connection to the room.
    NotConnected,
    /// The session's grant has `can_publish: false`, or there is no session yet.
    NoPublishGrant,
    /// There is no microphone.
    NoMicrophone,
    /// The user has deafened, which also blocks transmitting.
    Deafened,
    /// The user has muted.
    Muted,
    /// A key device that was being read has gone away. A client that never had one does not
    /// have this reason.
    KeyDeviceLost,
    /// One press lasted longer than `max_transmit_secs`: a stuck key, or something resting
    /// on the keyboard. It stands until that press is released.
    MaxTransmit,
}

impl ShutReason {
    /// Every reason, most important first.
    pub const ALL: [ShutReason; 7] = [
        ShutReason::NotConnected,
        ShutReason::NoPublishGrant,
        ShutReason::NoMicrophone,
        ShutReason::Deafened,
        ShutReason::Muted,
        ShutReason::KeyDeviceLost,
        ShutReason::MaxTransmit,
    ];

    fn bit(self) -> u8 {
        match self {
            ShutReason::NotConnected => 1 << 0,
            ShutReason::NoPublishGrant => 1 << 1,
            ShutReason::NoMicrophone => 1 << 2,
            ShutReason::Deafened => 1 << 3,
            ShutReason::Muted => 1 << 4,
            ShutReason::KeyDeviceLost => 1 << 5,
            ShutReason::MaxTransmit => 1 << 6,
        }
    }
}

/// A few words for the status line.
impl fmt::Display for ShutReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            ShutReason::NotConnected => "not connected",
            ShutReason::NoPublishGrant => "listening only: this session may not transmit",
            ShutReason::NoMicrophone => "no microphone",
            ShutReason::Deafened => "deafened",
            ShutReason::Muted => "muted",
            ShutReason::KeyDeviceLost => "key device lost",
            ShutReason::MaxTransmit => "transmit limit reached: release the key to talk again",
        })
    }
}

/// The reasons that stand at one moment.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Default)]
pub struct ShutReasons(u8);

impl ShutReasons {
    /// No reason: a press would open the gate.
    #[must_use]
    pub const fn none() -> Self {
        Self(0)
    }

    /// True if nothing stands in the way of a press.
    #[must_use]
    pub fn is_empty(self) -> bool {
        self.0 == 0
    }

    /// True if `reason` stands.
    #[must_use]
    pub fn contains(self, reason: ShutReason) -> bool {
        self.0 & reason.bit() != 0
    }

    /// The standing reasons, most important first.
    pub fn iter(self) -> impl Iterator<Item = ShutReason> {
        ShutReason::ALL
            .into_iter()
            .filter(move |&reason| self.contains(reason))
    }

    /// The most important standing reason.
    #[must_use]
    pub fn first(self) -> Option<ShutReason> {
        self.iter().next()
    }

    /// Adds a reason. True if it was not already there.
    fn insert(&mut self, reason: ShutReason) -> bool {
        let added = !self.contains(reason);
        self.0 |= reason.bit();
        added
    }

    /// Takes a reason away. True if it was there.
    fn remove(&mut self, reason: ShutReason) -> bool {
        let removed = self.contains(reason);
        self.0 &= !reason.bit();
        removed
    }
}

impl fmt::Debug for ShutReasons {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_set().entries(self.iter()).finish()
    }
}

/// A command read from standard input, one per line (the note's §4). It is how the tests
/// drive the client, and the fallback when there is no key device.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum LineCommand {
    /// `down`: the talk key went down.
    Down,
    /// `up`: the talk key came up.
    Up,
    /// `mute`: mute or unmute.
    Mute,
    /// `deafen`: deafen or undeafen.
    Deafen,
    /// `quit`: leave the room and exit.
    Quit,
}

impl FromStr for LineCommand {
    type Err = Error;

    /// One line, with or without its line ending.
    fn from_str(line: &str) -> Result<Self, Error> {
        match line.trim() {
            "down" => Ok(LineCommand::Down),
            "up" => Ok(LineCommand::Up),
            "mute" => Ok(LineCommand::Mute),
            "deafen" => Ok(LineCommand::Deafen),
            "quit" => Ok(LineCommand::Quit),
            _ => Err(Error::UnknownCommand),
        }
    }
}

/// Something that happened, for [`Ptt::handle`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PttInput {
    /// A press or release from the key decoder.
    Key(KeyEvent),
    /// A command from standard input. A `quit` here is also how the caller passes on Ctrl-C.
    Line(LineCommand),
    /// The connection to the room was established (`true`) or is gone (`false`).
    Connected(bool),
    /// Whether the current session's grant allows publishing.
    PublishGrant(bool),
    /// Whether there is a microphone.
    Microphone(bool),
    /// Whether the key device is open and readable.
    KeyDevice(bool),
    /// The gate reports that it has actually shut, after a `Shut` (when its release tail has
    /// run out) or a `ForceShut`. Deliver one for each time the gate goes from open to shut.
    GateShut,
    /// Nothing happened but time. Deliver one at [`Ptt::deadline`] at the latest.
    Tick,
}

/// A command for the transmit gate.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GateCommand {
    /// A press: start handing microphone audio on.
    Open,
    /// A release: shut after the release tail.
    Shut,
    /// This reason now stands: shut, and stay shut while it does.
    ForceShut(ShutReason),
    /// This reason no longer stands. The gate stays shut until the next `Open`.
    Clear(ShutReason),
}

/// A transmit report for the caller to queue for `conchd` (the note's §6).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum TransmitReport {
    /// The gate opened.
    Started,
    /// The gate shut.
    Stopped,
}

/// The user's own state, for the status line and for `--json`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PttStatus {
    /// A transmission is in progress: from the press until the gate has shut, release tail
    /// included.
    pub transmitting: bool,
    /// Everything that currently keeps the gate shut. Empty means a press would transmit.
    pub blocked: ShutReasons,
}

impl PttStatus {
    /// The user has muted.
    #[must_use]
    pub fn muted(&self) -> bool {
        self.blocked.contains(ShutReason::Muted)
    }

    /// The user has deafened: the caller silences playback while this is true.
    #[must_use]
    pub fn deafened(&self) -> bool {
        self.blocked.contains(ShutReason::Deafened)
    }
}

/// Something for the caller to do. [`Ptt::handle`] returns these in the order to do them:
/// gate commands first, since audio waits for nothing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PttOutput {
    /// Pass this to the transmit gate.
    Gate(GateCommand),
    /// Queue this report. Reports are sent one at a time, in the order produced.
    Report(TransmitReport),
    /// A press of the talk key did nothing, for this reason. Say so.
    PressIgnored(ShutReason),
    /// The user's state changed; show it. Produced only when it differs from the last one.
    Status(PttStatus),
    /// Leave the room and exit 0. Always the last output; nothing follows it, ever.
    Quit,
}

/// Where a transmission is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Transmission {
    /// The gate is shut and its last shut has been reported.
    Idle,
    /// The gate was told to open at `since`, and `started` was reported.
    Open { since: Duration },
    /// The gate was told to shut and has not yet said that it has. `press_pending` is a
    /// press that arrived meanwhile and has not been released.
    Shutting { press_pending: bool },
}

/// Whether the user has asked to quit.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Stage {
    Running,
    /// `quit` arrived during a transmission: waiting for the gate to shut, so that the
    /// `stopped` report is produced before [`PttOutput::Quit`].
    Quitting,
    /// [`PttOutput::Quit`] has been produced.
    Done,
}

/// The push-to-talk state machine. See the module documentation for its rules.
#[derive(Debug)]
pub struct Ptt {
    max_transmit: Duration,
    transmission: Transmission,
    reasons: ShutReasons,
    key_device_present: bool,
    stage: Stage,
    shown: PttStatus,
}

impl Ptt {
    /// A machine for a client that has just started: not connected, with no session and no
    /// microphone yet, so three reasons stand. Returned with it are the first outputs, which
    /// tell a new (shut) gate about those reasons and give the first status.
    ///
    /// `max_transmit` is the longest one press may transmit (`max_transmit_secs`).
    #[must_use]
    pub fn new(max_transmit: Duration) -> (Self, Vec<PttOutput>) {
        let mut reasons = ShutReasons::none();
        let mut outputs = Vec::new();
        for reason in [
            ShutReason::NotConnected,
            ShutReason::NoPublishGrant,
            ShutReason::NoMicrophone,
        ] {
            reasons.insert(reason);
            outputs.push(PttOutput::Gate(GateCommand::ForceShut(reason)));
        }
        let shown = PttStatus {
            transmitting: false,
            blocked: reasons,
        };
        outputs.push(PttOutput::Status(shown));
        let ptt = Self {
            max_transmit,
            transmission: Transmission::Idle,
            reasons,
            key_device_present: false,
            stage: Stage::Running,
            shown,
        };
        (ptt, outputs)
    }

    /// Takes one thing that happened, at monotonic time `now` (measured from any fixed
    /// origin), and returns what to do about it, in order.
    pub fn handle(&mut self, now: Duration, input: PttInput) -> Vec<PttOutput> {
        let mut out = Vec::new();
        if self.stage == Stage::Done {
            return out;
        }

        // Whatever else happened, a press that has gone on too long ends first.
        if let Transmission::Open { since } = self.transmission
            && now.saturating_sub(since) >= self.max_transmit
        {
            self.force_shut(ShutReason::MaxTransmit, &mut out);
        }

        match input {
            PttInput::Key(KeyEvent { key, action }) => match (key, action) {
                (Key::Talk, KeyAction::Press) => self.press(now, &mut out),
                (Key::Talk, KeyAction::Release) => self.release(&mut out),
                (Key::Mute, KeyAction::Press) => self.toggle(ShutReason::Muted, &mut out),
                (Key::Deafen, KeyAction::Press) => self.toggle(ShutReason::Deafened, &mut out),
                (Key::Mute | Key::Deafen, KeyAction::Release) => {}
            },
            PttInput::Line(command) => match command {
                LineCommand::Down => self.press(now, &mut out),
                LineCommand::Up => self.release(&mut out),
                LineCommand::Mute => self.toggle(ShutReason::Muted, &mut out),
                LineCommand::Deafen => self.toggle(ShutReason::Deafened, &mut out),
                LineCommand::Quit => self.quit(&mut out),
            },
            PttInput::Connected(connected) => {
                self.set(ShutReason::NotConnected, !connected, &mut out);
            }
            PttInput::PublishGrant(granted) => {
                self.set(ShutReason::NoPublishGrant, !granted, &mut out);
            }
            PttInput::Microphone(present) => {
                self.set(ShutReason::NoMicrophone, !present, &mut out);
            }
            PttInput::KeyDevice(present) => self.key_device(present, &mut out),
            PttInput::GateShut => self.gate_shut(now, &mut out),
            PttInput::Tick => {}
        }

        let status = self.status();
        if status != self.shown {
            self.shown = status;
            out.push(PttOutput::Status(status));
        }
        if self.stage == Stage::Quitting && self.transmission == Transmission::Idle {
            self.stage = Stage::Done;
            out.push(PttOutput::Quit);
        }
        out
    }

    /// When the caller must deliver a [`PttInput::Tick`] at the latest: the moment the
    /// current press reaches the transmit limit. `None` while nothing is being transmitted.
    #[must_use]
    pub fn deadline(&self) -> Option<Duration> {
        match self.transmission {
            Transmission::Open { since } => since.checked_add(self.max_transmit),
            Transmission::Idle | Transmission::Shutting { .. } => None,
        }
    }

    /// The user's state as of the last call.
    #[must_use]
    pub fn status(&self) -> PttStatus {
        PttStatus {
            transmitting: self.transmission != Transmission::Idle,
            blocked: self.reasons,
        }
    }

    fn press(&mut self, now: Duration, out: &mut Vec<PttOutput>) {
        if self.stage != Stage::Running {
            return;
        }
        if let Transmission::Open { .. } = self.transmission {
            // Already transmitting: a second `down` with no `up` between.
            return;
        }
        // A new press ends the one that hit the limit, if its release was never seen.
        self.clear(ShutReason::MaxTransmit, out);
        if let Some(reason) = self.reasons.first() {
            out.push(PttOutput::PressIgnored(reason));
            return;
        }
        match self.transmission {
            Transmission::Idle => self.open(now, out),
            Transmission::Shutting { .. } => {
                self.transmission = Transmission::Shutting {
                    press_pending: true,
                };
            }
            Transmission::Open { .. } => {}
        }
    }

    fn release(&mut self, out: &mut Vec<PttOutput>) {
        self.clear(ShutReason::MaxTransmit, out);
        match self.transmission {
            Transmission::Open { .. } => {
                out.push(PttOutput::Gate(GateCommand::Shut));
                self.shutting();
            }
            Transmission::Shutting { .. } => self.shutting(),
            Transmission::Idle => {}
        }
    }

    fn toggle(&mut self, reason: ShutReason, out: &mut Vec<PttOutput>) {
        let stands = self.reasons.contains(reason);
        self.set(reason, !stands, out);
    }

    fn key_device(&mut self, present: bool, out: &mut Vec<PttOutput>) {
        if present {
            self.clear(ShutReason::KeyDeviceLost, out);
        } else if self.key_device_present {
            // Losing the device is a release of whatever was held on it: that release will
            // never be read.
            self.clear(ShutReason::MaxTransmit, out);
            self.force_shut(ShutReason::KeyDeviceLost, out);
        }
        self.key_device_present = present;
    }

    fn gate_shut(&mut self, now: Duration, out: &mut Vec<PttOutput>) {
        let press_pending = match self.transmission {
            Transmission::Idle => return,
            Transmission::Open { .. } => {
                // The gate says it shut although nothing told it to. Take its word that the
                // transmission is over, and tell it to shut all the same: if the report was
                // stale or mistaken, the gate must not be left open while this machine
                // believes it shut.
                out.push(PttOutput::Gate(GateCommand::Shut));
                false
            }
            Transmission::Shutting { press_pending } => press_pending,
        };
        self.transmission = Transmission::Idle;
        out.push(PttOutput::Report(TransmitReport::Stopped));
        if press_pending && self.stage == Stage::Running && self.reasons.is_empty() {
            self.open(now, out);
        }
    }

    fn quit(&mut self, out: &mut Vec<PttOutput>) {
        if self.stage != Stage::Running {
            return;
        }
        self.stage = Stage::Quitting;
        // As for a release. `Quit` follows the `stopped` report, once the gate has shut.
        self.release(out);
    }

    fn open(&mut self, now: Duration, out: &mut Vec<PttOutput>) {
        self.transmission = Transmission::Open { since: now };
        out.push(PttOutput::Gate(GateCommand::Open));
        out.push(PttOutput::Report(TransmitReport::Started));
    }

    /// The gate has been told to shut, or told again: no press is waiting any more.
    fn shutting(&mut self) {
        self.transmission = Transmission::Shutting {
            press_pending: false,
        };
    }

    fn set(&mut self, reason: ShutReason, stands: bool, out: &mut Vec<PttOutput>) {
        if stands {
            self.force_shut(reason, out);
        } else {
            self.clear(reason, out);
        }
    }

    /// A reason starts to stand. The gate is told even if it is already shut, so that it
    /// holds the same reasons as this machine does.
    fn force_shut(&mut self, reason: ShutReason, out: &mut Vec<PttOutput>) {
        if !self.reasons.insert(reason) {
            return;
        }
        out.push(PttOutput::Gate(GateCommand::ForceShut(reason)));
        if self.transmission != Transmission::Idle {
            self.shutting();
        }
    }

    /// A reason stops standing. Nothing opens.
    fn clear(&mut self, reason: ShutReason, out: &mut Vec<PttOutput>) {
        if self.reasons.remove(reason) {
            out.push(PttOutput::Gate(GateCommand::Clear(reason)));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use GateCommand::{Clear, ForceShut, Open, Shut};
    use PttOutput::{Gate, PressIgnored, Quit, Report};
    use TransmitReport::{Started, Stopped};

    const MAX: Duration = Duration::from_secs(120);
    const DOWN: PttInput = PttInput::Line(LineCommand::Down);
    const UP: PttInput = PttInput::Line(LineCommand::Up);
    const MUTE: PttInput = PttInput::Line(LineCommand::Mute);
    const DEAFEN: PttInput = PttInput::Line(LineCommand::Deafen);
    const QUIT: PttInput = PttInput::Line(LineCommand::Quit);

    fn key(key: Key, action: KeyAction) -> PttInput {
        PttInput::Key(KeyEvent { key, action })
    }

    fn status(transmitting: bool, blocked: &[ShutReason]) -> PttOutput {
        let mut reasons = ShutReasons::none();
        for &reason in blocked {
            reasons.insert(reason);
        }
        PttOutput::Status(PttStatus {
            transmitting,
            blocked: reasons,
        })
    }

    /// A stand-in for the transmit gate of `conch-voice-audio`, as strict as a gate could
    /// be: it panics if it is commanded in a way a real gate might mishandle.
    #[derive(Debug, PartialEq)]
    enum FakeGate {
        Shut,
        Open,
        /// Told to shut and still forwarding its release tail.
        Tail,
    }

    /// Drives a [`Ptt`] with a fake clock and a fake gate, and checks on every call the
    /// properties that must hold in every scenario.
    struct Harness {
        ptt: Ptt,
        now: Duration,
        gate: FakeGate,
        gate_reasons: Vec<ShutReason>,
        /// The last report produced, to check that they alternate.
        last_report: Option<TransmitReport>,
        opens: usize,
        quit: bool,
    }

    impl Harness {
        /// A machine that has just been made, with its first outputs.
        fn new() -> (Self, Vec<PttOutput>) {
            let (ptt, first) = Ptt::new(MAX);
            let mut harness = Self {
                ptt,
                now: Duration::from_secs(1000),
                gate: FakeGate::Shut,
                gate_reasons: Vec::new(),
                last_report: None,
                opens: 0,
                quit: false,
            };
            let shut = harness.check(None, &first);
            assert!(!shut);
            (harness, first)
        }

        /// A machine with everything in place for a press to transmit, and a key device.
        fn ready() -> Self {
            let (mut harness, _) = Self::new();
            harness.send(PttInput::KeyDevice(true));
            harness.send(PttInput::Microphone(true));
            harness.send(PttInput::PublishGrant(true));
            harness.send(PttInput::Connected(true));
            assert_eq!(harness.ptt.status().blocked, ShutReasons::none());
            harness
        }

        /// A machine in the middle of a transmission.
        fn talking() -> Self {
            let mut harness = Self::ready();
            assert_eq!(
                harness.send(DOWN),
                [Gate(Open), Report(Started), status(true, &[])]
            );
            harness
        }

        fn advance(&mut self, by: Duration) {
            self.now += by;
        }

        /// Delivers one input. If the fake gate is forced shut while open it reports that at
        /// once, as a real gate would, and the outputs of that report follow in the result.
        fn send(&mut self, input: PttInput) -> Vec<PttOutput> {
            let mut outputs = self.ptt.handle(self.now, input);
            if self.check(Some(input), &outputs) {
                let more = self.ptt.handle(self.now, PttInput::GateShut);
                assert!(!self.check(Some(PttInput::GateShut), &more));
                outputs.extend(more);
            }
            outputs
        }

        /// The fake gate's release tail runs out and it reports that it has shut.
        fn tail_ends(&mut self) -> Vec<PttOutput> {
            assert_eq!(self.gate, FakeGate::Tail, "no release tail is running");
            self.gate = FakeGate::Shut;
            self.send(PttInput::GateShut)
        }

        /// Checks one batch of outputs and applies its gate commands to the fake gate.
        /// Returns whether the gate was forced shut while it was forwarding audio.
        fn check(&mut self, input: Option<PttInput>, outputs: &[PttOutput]) -> bool {
            assert!(
                !self.quit || outputs.is_empty(),
                "output after Quit: {outputs:?}"
            );
            let mut forced_shut = false;
            let mut statuses = 0;
            for (i, output) in outputs.iter().enumerate() {
                match *output {
                    Gate(Open) => {
                        assert!(
                            self.gate_reasons.is_empty(),
                            "Open while {:?} stand",
                            self.gate_reasons
                        );
                        assert_eq!(self.gate, FakeGate::Shut, "Open before the gate had shut");
                        assert_eq!(
                            outputs.get(i + 1),
                            Some(&Report(Started)),
                            "Open is followed at once by its started report"
                        );
                        self.gate = FakeGate::Open;
                        self.opens += 1;
                    }
                    Gate(Shut) => match self.gate {
                        FakeGate::Open => self.gate = FakeGate::Tail,
                        // Shutting a gate that has just said it is shut changes nothing.
                        FakeGate::Shut => assert_eq!(input, Some(PttInput::GateShut)),
                        FakeGate::Tail => panic!("Shut twice"),
                    },
                    Gate(ForceShut(reason)) => {
                        assert!(
                            !self.gate_reasons.contains(&reason),
                            "{reason:?} forced twice"
                        );
                        self.gate_reasons.push(reason);
                        if self.gate != FakeGate::Shut {
                            self.gate = FakeGate::Shut;
                            forced_shut = true;
                        }
                    }
                    Gate(Clear(reason)) => {
                        assert!(
                            self.gate_reasons.contains(&reason),
                            "{reason:?} was not standing"
                        );
                        self.gate_reasons.retain(|&standing| standing != reason);
                    }
                    Report(Started) => {
                        assert_ne!(self.last_report, Some(Started), "two started in a row");
                        assert!(
                            i > 0 && outputs[i - 1] == Gate(Open),
                            "started without an Open"
                        );
                        self.last_report = Some(Started);
                    }
                    Report(Stopped) => {
                        assert_eq!(self.last_report, Some(Started), "stopped without a started");
                        assert_eq!(
                            input,
                            Some(PttInput::GateShut),
                            "stopped before the gate said it had shut"
                        );
                        self.last_report = Some(Stopped);
                    }
                    PressIgnored(reason) => {
                        assert_eq!(self.ptt.status().blocked.first(), Some(reason));
                    }
                    PttOutput::Status(shown) => {
                        statuses += 1;
                        assert_eq!(shown, self.ptt.status());
                    }
                    Quit => {
                        assert_eq!(i, outputs.len() - 1, "Quit is the last output");
                        assert_eq!(self.gate, FakeGate::Shut, "Quit with the gate open");
                        self.quit = true;
                    }
                }
            }
            assert!(statuses <= 1, "more than one status in {outputs:?}");
            // What the machine believes and what the gate was told agree.
            let believed: Vec<ShutReason> = self.ptt.status().blocked.iter().collect();
            let mut told = self.gate_reasons.clone();
            told.sort_by_key(|reason| reason.bit());
            assert_eq!(believed, told);
            // A machine that believes nothing is being transmitted has not left the gate
            // open (it may still be running out its release tail).
            assert!(
                self.ptt.status().transmitting || self.gate != FakeGate::Open,
                "the gate is open and the machine is idle"
            );
            // The property users rely on: with any reason standing, the gate is shut.
            assert!(
                self.gate_reasons.is_empty() || self.gate == FakeGate::Shut || forced_shut,
                "gate {:?} while {:?} stand",
                self.gate,
                self.gate_reasons
            );
            forced_shut
        }

        /// Ends the scenario: lets go of the key, lets the gate finish, and checks that
        /// every started report was followed by its stopped. Returns how many times the gate
        /// was opened.
        fn finish(mut self) -> usize {
            self.send(UP);
            if self.gate == FakeGate::Tail {
                self.tail_ends();
            }
            assert_eq!(self.gate, FakeGate::Shut);
            assert_ne!(
                self.last_report,
                Some(Started),
                "a started report was never stopped"
            );
            assert!(!self.ptt.status().transmitting);
            assert_eq!(self.ptt.deadline(), None);
            self.opens
        }
    }

    /// How a test makes each reason start and stop standing, on a machine from
    /// [`Harness::talking`] or [`Harness::ready`].
    fn cause(harness: &mut Harness, reason: ShutReason) -> Vec<PttOutput> {
        match reason {
            ShutReason::NotConnected => harness.send(PttInput::Connected(false)),
            ShutReason::NoPublishGrant => harness.send(PttInput::PublishGrant(false)),
            ShutReason::NoMicrophone => harness.send(PttInput::Microphone(false)),
            ShutReason::Deafened => harness.send(DEAFEN),
            ShutReason::Muted => harness.send(MUTE),
            ShutReason::KeyDeviceLost => harness.send(PttInput::KeyDevice(false)),
            ShutReason::MaxTransmit => {
                harness.advance(MAX);
                harness.send(PttInput::Tick)
            }
        }
    }

    fn end(harness: &mut Harness, reason: ShutReason) -> Vec<PttOutput> {
        match reason {
            ShutReason::NotConnected => harness.send(PttInput::Connected(true)),
            ShutReason::NoPublishGrant => harness.send(PttInput::PublishGrant(true)),
            ShutReason::NoMicrophone => harness.send(PttInput::Microphone(true)),
            ShutReason::Deafened => harness.send(DEAFEN),
            ShutReason::Muted => harness.send(MUTE),
            ShutReason::KeyDeviceLost => harness.send(PttInput::KeyDevice(true)),
            ShutReason::MaxTransmit => harness.send(UP),
        }
    }

    /// The acceptance criterion for one reason: it shuts the gate during a transmission,
    /// with exactly one stopped report, and the gate stays shut until a new press, even
    /// though the key was never let go and the reason has ended.
    fn shuts_the_gate_until_a_new_press(reason: ShutReason) {
        let mut harness = Harness::talking();
        harness.advance(Duration::from_secs(3));

        // The reason starts: the gate is forced shut and the transmission is reported over.
        assert_eq!(
            cause(&mut harness, reason),
            [
                Gate(ForceShut(reason)),
                status(true, &[reason]),
                Report(Stopped),
                status(false, &[reason]),
            ]
        );
        assert_eq!(harness.gate, FakeGate::Shut);

        // While it stands, time passing changes nothing and a press is refused.
        harness.advance(Duration::from_secs(10));
        assert_eq!(harness.send(PttInput::Tick), []);
        if reason != ShutReason::MaxTransmit {
            // (A new press is what ends MaxTransmit; that is the last step below.)
            assert_eq!(harness.send(DOWN), [PressIgnored(reason)]);
        }

        // The reason ends with the key still down as far as the user is concerned. The gate
        // is told, and nothing opens, however long we wait.
        if reason != ShutReason::MaxTransmit {
            assert_eq!(
                end(&mut harness, reason),
                [Gate(Clear(reason)), status(false, &[])]
            );
            harness.advance(Duration::from_secs(10));
            assert_eq!(harness.send(PttInput::Tick), []);
            assert_eq!(harness.gate, FakeGate::Shut);
            assert_eq!(harness.opens, 1);
            // The key comes up at last: still nothing.
            assert_eq!(harness.send(UP), []);
        } else {
            // For the transmit limit the reason ends when the stuck key comes up.
            assert_eq!(
                end(&mut harness, reason),
                [Gate(Clear(reason)), status(false, &[])]
            );
            assert_eq!(harness.gate, FakeGate::Shut);
            assert_eq!(harness.opens, 1);
        }

        // Only a new press opens it.
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn not_connected_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::NotConnected);
    }

    #[test]
    fn losing_the_publish_grant_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::NoPublishGrant);
    }

    #[test]
    fn a_missing_microphone_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::NoMicrophone);
    }

    #[test]
    fn deafening_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::Deafened);
    }

    #[test]
    fn muting_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::Muted);
    }

    #[test]
    fn a_lost_key_device_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::KeyDeviceLost);
    }

    #[test]
    fn the_transmit_limit_shuts_the_gate_until_a_new_press() {
        shuts_the_gate_until_a_new_press(ShutReason::MaxTransmit);
    }

    #[test]
    fn a_new_client_starts_shut_and_tells_the_gate_why() {
        let (harness, first) = Harness::new();
        assert_eq!(
            first,
            [
                Gate(ForceShut(ShutReason::NotConnected)),
                Gate(ForceShut(ShutReason::NoPublishGrant)),
                Gate(ForceShut(ShutReason::NoMicrophone)),
                status(
                    false,
                    &[
                        ShutReason::NotConnected,
                        ShutReason::NoPublishGrant,
                        ShutReason::NoMicrophone
                    ]
                ),
            ]
        );
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn the_reasons_a_client_starts_with_clear_one_by_one() {
        let (mut harness, _) = Harness::new();
        assert_eq!(
            harness.send(PttInput::Microphone(true)),
            [
                Gate(Clear(ShutReason::NoMicrophone)),
                status(
                    false,
                    &[ShutReason::NotConnected, ShutReason::NoPublishGrant]
                ),
            ]
        );
        assert_eq!(
            harness.send(PttInput::PublishGrant(true)),
            [
                Gate(Clear(ShutReason::NoPublishGrant)),
                status(false, &[ShutReason::NotConnected]),
            ]
        );
        assert_eq!(
            harness.send(PttInput::Connected(true)),
            [Gate(Clear(ShutReason::NotConnected)), status(false, &[])]
        );
        // Being told again what is already so does nothing.
        assert_eq!(harness.send(PttInput::Connected(true)), []);
        assert_eq!(harness.send(PttInput::Microphone(true)), []);
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn a_press_opens_the_gate_and_a_release_shuts_it_with_one_report_each() {
        let mut harness = Harness::ready();
        assert_eq!(
            harness.send(key(Key::Talk, KeyAction::Press)),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        harness.advance(Duration::from_secs(2));
        // The release shuts the gate; the report waits for the gate's release tail.
        assert_eq!(
            harness.send(key(Key::Talk, KeyAction::Release)),
            [Gate(Shut)]
        );
        assert!(harness.ptt.status().transmitting);
        harness.advance(Duration::from_millis(100));
        assert_eq!(harness.tail_ends(), [Report(Stopped), status(false, &[])]);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn line_commands_and_the_talk_key_are_the_same_press() {
        let mut harness = Harness::ready();
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(
            harness.send(key(Key::Talk, KeyAction::Release)),
            [Gate(Shut)]
        );
        assert_eq!(harness.tail_ends(), [Report(Stopped), status(false, &[])]);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_press_while_any_reason_stands_does_nothing_and_says_why() {
        for reason in ShutReason::ALL {
            if reason == ShutReason::MaxTransmit {
                // It stands only during a press; see the stuck-key tests.
                continue;
            }
            let mut harness = Harness::ready();
            cause(&mut harness, reason);
            assert_eq!(harness.send(DOWN), [PressIgnored(reason)], "{reason:?}");
            assert_eq!(harness.send(UP), [], "{reason:?}");
            assert_eq!(
                harness.send(key(Key::Talk, KeyAction::Press)),
                [PressIgnored(reason)],
                "{reason:?}"
            );
            assert_eq!(harness.gate, FakeGate::Shut);
            assert_eq!(harness.finish(), 0, "{reason:?}");
        }
    }

    #[test]
    fn a_press_while_not_connected_is_reported_as_not_connected() {
        let mut harness = Harness::ready();
        harness.send(PttInput::Connected(false));
        // Not connected comes first even when something else stands too.
        harness.send(MUTE);
        harness.send(PttInput::Microphone(false));
        let outputs = harness.send(DOWN);
        assert_eq!(outputs, [PressIgnored(ShutReason::NotConnected)]);
        assert_eq!(ShutReason::NotConnected.to_string(), "not connected");
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn a_key_held_when_the_connection_drops_does_not_resume_when_it_returns() {
        let mut harness = Harness::talking();
        harness.advance(Duration::from_secs(5));
        assert_eq!(
            harness.send(PttInput::Connected(false)),
            [
                Gate(ForceShut(ShutReason::NotConnected)),
                status(true, &[ShutReason::NotConnected]),
                Report(Stopped),
                status(false, &[ShutReason::NotConnected]),
            ]
        );

        // The connection returns two seconds later. The key has been down the whole time.
        harness.advance(Duration::from_secs(2));
        assert_eq!(
            harness.send(PttInput::Connected(true)),
            [Gate(Clear(ShutReason::NotConnected)), status(false, &[])]
        );
        for _ in 0..5 {
            harness.advance(Duration::from_secs(60));
            assert_eq!(harness.send(PttInput::Tick), []);
        }
        assert_eq!(harness.gate, FakeGate::Shut);
        assert_eq!(harness.ptt.deadline(), None);

        // Letting go and pressing again is what transmits.
        assert_eq!(harness.send(UP), []);
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn a_stuck_key_is_cut_off_at_the_transmit_limit() {
        let mut harness = Harness::ready();
        assert_eq!(harness.ptt.deadline(), None);
        harness.send(DOWN);
        let pressed_at = harness.now;
        assert_eq!(harness.ptt.deadline(), Some(pressed_at + MAX));

        // One millisecond short of the limit nothing happens.
        harness.advance(MAX - Duration::from_millis(1));
        assert_eq!(harness.send(PttInput::Tick), []);
        assert_eq!(harness.gate, FakeGate::Open);

        // At the limit the gate is forced shut, with a reason the status line can show.
        harness.advance(Duration::from_millis(1));
        assert_eq!(
            harness.send(PttInput::Tick),
            [
                Gate(ForceShut(ShutReason::MaxTransmit)),
                status(true, &[ShutReason::MaxTransmit]),
                Report(Stopped),
                status(false, &[ShutReason::MaxTransmit]),
            ]
        );
        assert_eq!(harness.ptt.deadline(), None);
        assert_eq!(
            ShutReason::MaxTransmit.to_string(),
            "transmit limit reached: release the key to talk again"
        );

        // The key stays stuck for an hour: nothing more happens.
        harness.advance(Duration::from_secs(3600));
        assert_eq!(harness.send(PttInput::Tick), []);

        // It comes up: the reason ends, and the next press transmits.
        assert_eq!(
            harness.send(UP),
            [Gate(Clear(ShutReason::MaxTransmit)), status(false, &[])]
        );
        harness.send(DOWN);
        assert_eq!(harness.ptt.deadline(), Some(harness.now + MAX));
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn the_transmit_limit_is_enforced_on_any_input_not_only_a_tick() {
        let mut harness = Harness::talking();
        harness.advance(MAX + Duration::from_secs(1));
        // The caller's timer was late and a mute key arrived first.
        assert_eq!(
            harness.send(key(Key::Mute, KeyAction::Press)),
            [
                Gate(ForceShut(ShutReason::MaxTransmit)),
                Gate(ForceShut(ShutReason::Muted)),
                status(true, &[ShutReason::Muted, ShutReason::MaxTransmit]),
                Report(Stopped),
                status(false, &[ShutReason::Muted, ShutReason::MaxTransmit]),
            ]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_release_that_arrives_after_the_limit_still_ends_in_one_stopped_report() {
        let mut harness = Harness::talking();
        harness.advance(MAX + Duration::from_secs(5));
        assert_eq!(
            harness.send(UP),
            [
                Gate(ForceShut(ShutReason::MaxTransmit)),
                Gate(Clear(ShutReason::MaxTransmit)),
                Report(Stopped),
                status(false, &[]),
            ]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_new_press_ends_a_limit_whose_release_was_never_seen() {
        let mut harness = Harness::talking();
        harness.advance(MAX);
        harness.send(PttInput::Tick);
        // No `up` arrives (a line-driven wrapper died, say); the next `down` is a new press.
        assert_eq!(
            harness.send(DOWN),
            [
                Gate(Clear(ShutReason::MaxTransmit)),
                Gate(Open),
                Report(Started),
                status(true, &[]),
            ]
        );
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn the_limit_counts_from_each_press_not_from_the_first() {
        let mut harness = Harness::talking();
        harness.advance(MAX - Duration::from_secs(1));
        harness.send(UP);
        harness.tail_ends();
        harness.send(DOWN);
        harness.advance(MAX - Duration::from_secs(1));
        assert_eq!(harness.send(PttInput::Tick), []);
        assert_eq!(harness.gate, FakeGate::Open);
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn mute_toggles_and_blocks_transmitting() {
        let mut harness = Harness::ready();
        assert_eq!(
            harness.send(MUTE),
            [
                Gate(ForceShut(ShutReason::Muted)),
                status(false, &[ShutReason::Muted])
            ]
        );
        assert!(harness.ptt.status().muted());
        assert_eq!(harness.send(DOWN), [PressIgnored(ShutReason::Muted)]);
        assert_eq!(harness.send(UP), []);
        assert_eq!(
            harness.send(MUTE),
            [Gate(Clear(ShutReason::Muted)), status(false, &[])]
        );
        assert!(!harness.ptt.status().muted());
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn deafen_toggles_and_blocks_transmitting() {
        let mut harness = Harness::ready();
        assert_eq!(
            harness.send(DEAFEN),
            [
                Gate(ForceShut(ShutReason::Deafened)),
                status(false, &[ShutReason::Deafened])
            ]
        );
        assert!(harness.ptt.status().deafened());
        assert_eq!(harness.send(DOWN), [PressIgnored(ShutReason::Deafened)]);
        assert_eq!(harness.send(UP), []);
        assert_eq!(
            harness.send(DEAFEN),
            [Gate(Clear(ShutReason::Deafened)), status(false, &[])]
        );
        assert!(!harness.ptt.status().deafened());
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn the_mute_and_deafen_keys_toggle_on_press_and_ignore_release() {
        let mut harness = Harness::ready();
        for (toggle_key, reason) in [
            (Key::Mute, ShutReason::Muted),
            (Key::Deafen, ShutReason::Deafened),
        ] {
            assert_eq!(
                harness.send(key(toggle_key, KeyAction::Press)),
                [Gate(ForceShut(reason)), status(false, &[reason])]
            );
            assert_eq!(harness.send(key(toggle_key, KeyAction::Release)), []);
            assert_eq!(
                harness.send(key(toggle_key, KeyAction::Press)),
                [Gate(Clear(reason)), status(false, &[])]
            );
            assert_eq!(harness.send(key(toggle_key, KeyAction::Release)), []);
        }
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn mute_and_deafen_are_independent() {
        let mut harness = Harness::ready();
        harness.send(MUTE);
        harness.send(DEAFEN);
        assert_eq!(
            harness.ptt.status().blocked.iter().collect::<Vec<_>>(),
            [ShutReason::Deafened, ShutReason::Muted]
        );
        // Undeafening leaves the mute as it was, and transmitting stays blocked.
        assert_eq!(
            harness.send(DEAFEN),
            [
                Gate(Clear(ShutReason::Deafened)),
                status(false, &[ShutReason::Muted])
            ]
        );
        assert_eq!(harness.send(DOWN), [PressIgnored(ShutReason::Muted)]);
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn a_session_that_may_not_publish_never_opens_the_gate() {
        let (mut harness, _) = Harness::new();
        harness.send(PttInput::Microphone(true));
        harness.send(PttInput::Connected(true));
        harness.send(PttInput::PublishGrant(false));
        assert_eq!(
            harness.send(DOWN),
            [PressIgnored(ShutReason::NoPublishGrant)]
        );
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn a_press_during_the_release_tail_waits_for_the_gate_and_is_a_new_transmission() {
        let mut harness = Harness::talking();
        assert_eq!(harness.send(UP), [Gate(Shut)]);
        harness.advance(Duration::from_millis(40));
        // Pressed again 40 ms later, inside the tail: nothing is commanded yet.
        assert_eq!(harness.send(DOWN), []);
        assert_eq!(harness.ptt.deadline(), None);
        harness.advance(Duration::from_millis(60));
        // The gate shuts: the first transmission is reported over, then the second begins.
        assert_eq!(
            harness.tail_ends(),
            [Report(Stopped), Gate(Open), Report(Started)]
        );
        assert_eq!(harness.ptt.deadline(), Some(harness.now + MAX));
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn a_press_and_release_both_inside_the_release_tail_open_nothing() {
        let mut harness = Harness::talking();
        harness.send(UP);
        assert_eq!(harness.send(DOWN), []);
        assert_eq!(harness.send(UP), []);
        assert_eq!(harness.tail_ends(), [Report(Stopped), status(false, &[])]);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_press_waiting_on_the_release_tail_is_dropped_if_a_reason_starts() {
        for reason in ShutReason::ALL {
            if reason == ShutReason::MaxTransmit {
                continue;
            }
            let mut harness = Harness::talking();
            harness.send(UP);
            assert_eq!(harness.send(DOWN), []);
            // The reason cuts the tail short and the waiting press with it.
            assert_eq!(
                cause(&mut harness, reason),
                [
                    Gate(ForceShut(reason)),
                    status(true, &[reason]),
                    Report(Stopped),
                    status(false, &[reason]),
                ],
                "{reason:?}"
            );
            // It ends with the key still down: nothing opens.
            assert_eq!(
                end(&mut harness, reason),
                [Gate(Clear(reason)), status(false, &[])]
            );
            assert_eq!(harness.gate, FakeGate::Shut);
            assert_eq!(harness.finish(), 1, "{reason:?}");
        }
    }

    #[test]
    fn a_reason_that_starts_during_the_release_tail_still_gives_one_stopped_report() {
        let mut harness = Harness::talking();
        assert_eq!(harness.send(UP), [Gate(Shut)]);
        assert_eq!(
            harness.send(PttInput::Connected(false)),
            [
                Gate(ForceShut(ShutReason::NotConnected)),
                status(true, &[ShutReason::NotConnected]),
                Report(Stopped),
                status(false, &[ShutReason::NotConnected]),
            ]
        );
        // A gate that also reported the end of its tail would be telling us twice.
        assert_eq!(harness.send(PttInput::GateShut), []);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn two_reasons_at_once_force_the_gate_once_each_and_report_one_stop() {
        let mut harness = Harness::talking();
        assert_eq!(
            harness.send(PttInput::Connected(false)),
            [
                Gate(ForceShut(ShutReason::NotConnected)),
                status(true, &[ShutReason::NotConnected]),
                Report(Stopped),
                status(false, &[ShutReason::NotConnected]),
            ]
        );
        assert_eq!(
            harness.send(PttInput::PublishGrant(false)),
            [
                Gate(ForceShut(ShutReason::NoPublishGrant)),
                status(
                    false,
                    &[ShutReason::NotConnected, ShutReason::NoPublishGrant]
                ),
            ]
        );
        // Both must end before a press transmits.
        harness.send(UP);
        harness.send(PttInput::Connected(true));
        assert_eq!(
            harness.send(DOWN),
            [PressIgnored(ShutReason::NoPublishGrant)]
        );
        harness.send(UP);
        harness.send(PttInput::PublishGrant(true));
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 2);
    }

    #[test]
    fn a_lost_key_device_is_a_release_of_the_key_held_on_it() {
        let mut harness = Harness::ready();
        harness.send(key(Key::Talk, KeyAction::Press));
        harness.advance(MAX);
        harness.send(PttInput::Tick);
        // The key is still held, past the limit, when the keyboard is unplugged. Its release
        // will never be read, so the limit's reason ends here.
        assert_eq!(
            harness.send(PttInput::KeyDevice(false)),
            [
                Gate(Clear(ShutReason::MaxTransmit)),
                Gate(ForceShut(ShutReason::KeyDeviceLost)),
                status(false, &[ShutReason::KeyDeviceLost]),
            ]
        );
        // Plugged back in: ready for a new press, with nothing left over.
        assert_eq!(
            harness.send(PttInput::KeyDevice(true)),
            [Gate(Clear(ShutReason::KeyDeviceLost)), status(false, &[])]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_client_that_never_had_a_key_device_is_not_blocked_by_its_absence() {
        let (mut harness, _) = Harness::new();
        harness.send(PttInput::Microphone(true));
        harness.send(PttInput::PublishGrant(true));
        harness.send(PttInput::Connected(true));
        // The caller may say there is none; that is not a loss.
        assert_eq!(harness.send(PttInput::KeyDevice(false)), []);
        assert_eq!(
            harness.send(DOWN),
            [Gate(Open), Report(Started), status(true, &[])]
        );
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_key_device_lost_while_idle_is_shown_and_blocks_until_it_returns() {
        let mut harness = Harness::ready();
        assert_eq!(
            harness.send(PttInput::KeyDevice(false)),
            [
                Gate(ForceShut(ShutReason::KeyDeviceLost)),
                status(false, &[ShutReason::KeyDeviceLost])
            ]
        );
        assert_eq!(harness.send(PttInput::KeyDevice(false)), []);
        assert_eq!(
            harness.send(DOWN),
            [PressIgnored(ShutReason::KeyDeviceLost)]
        );
        assert_eq!(
            harness.send(PttInput::KeyDevice(true)),
            [Gate(Clear(ShutReason::KeyDeviceLost)), status(false, &[])]
        );
        assert_eq!(harness.finish(), 0);
    }

    #[test]
    fn inputs_that_change_nothing_produce_nothing() {
        let mut harness = Harness::ready();
        assert_eq!(harness.send(UP), [], "a release with nothing held");
        assert_eq!(
            harness.send(PttInput::GateShut),
            [],
            "a shut that was not ours"
        );
        assert_eq!(harness.send(PttInput::Tick), []);
        harness.send(DOWN);
        assert_eq!(harness.send(DOWN), [], "a second down with no up between");
        assert_eq!(harness.send(key(Key::Talk, KeyAction::Press)), []);
        assert_eq!(harness.send(PttInput::Tick), []);
        assert_eq!(harness.send(PttInput::Connected(true)), []);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_gate_that_shuts_by_itself_ends_the_transmission_and_does_not_resume() {
        let mut harness = Harness::talking();
        // The gate reports shut although nothing told it to (a safety of its own, say).
        harness.gate = FakeGate::Shut;
        assert_eq!(
            harness.send(PttInput::GateShut),
            [Gate(Shut), Report(Stopped), status(false, &[])]
        );
        assert_eq!(harness.send(PttInput::Tick), []);
        assert_eq!(harness.send(UP), []);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn a_stale_report_that_the_gate_shut_never_leaves_the_gate_open() {
        let mut harness = Harness::talking();
        // The caller delivers a shut report that does not belong to this transmission: the
        // fake gate is in fact still open. The machine cannot tell, so it shuts the gate.
        assert_eq!(harness.gate, FakeGate::Open);
        assert_eq!(
            harness.send(PttInput::GateShut),
            [Gate(Shut), Report(Stopped), status(false, &[])]
        );
        assert_eq!(harness.gate, FakeGate::Tail);
        // The key comes up, and the gate's real report arrives: nothing more to say.
        assert_eq!(harness.send(UP), []);
        assert_eq!(harness.tail_ends(), []);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn quit_while_idle_quits_at_once() {
        let mut harness = Harness::ready();
        assert_eq!(harness.send(QUIT), [Quit]);
        // Nothing follows Quit, whatever arrives.
        for input in [
            DOWN,
            UP,
            MUTE,
            QUIT,
            PttInput::Connected(false),
            PttInput::GateShut,
        ] {
            assert_eq!(harness.send(input), []);
        }
        assert_eq!(harness.opens, 0);
    }

    #[test]
    fn quit_during_a_transmission_reports_stopped_before_quitting() {
        let mut harness = Harness::talking();
        assert_eq!(harness.send(QUIT), [Gate(Shut)]);
        // While the gate finishes, a press is not a new transmission.
        assert_eq!(harness.send(DOWN), []);
        assert_eq!(harness.send(QUIT), []);
        assert_eq!(
            harness.tail_ends(),
            [Report(Stopped), status(false, &[]), Quit]
        );
        assert_eq!(harness.send(DOWN), []);
        assert_eq!(harness.last_report, Some(Stopped));
        assert_eq!(harness.opens, 1);
    }

    #[test]
    fn quit_during_the_release_tail_drops_a_waiting_press() {
        let mut harness = Harness::talking();
        harness.send(UP);
        harness.send(DOWN);
        assert_eq!(harness.send(QUIT), []);
        assert_eq!(
            harness.tail_ends(),
            [Report(Stopped), status(false, &[]), Quit]
        );
        assert_eq!(harness.opens, 1);
    }

    #[test]
    fn a_forced_shut_while_quitting_still_ends_in_stopped_then_quit() {
        let mut harness = Harness::talking();
        harness.send(QUIT);
        assert_eq!(
            harness.send(PttInput::Connected(false)),
            [
                Gate(ForceShut(ShutReason::NotConnected)),
                status(true, &[ShutReason::NotConnected]),
                Report(Stopped),
                status(false, &[ShutReason::NotConnected]),
                Quit,
            ]
        );
    }

    #[test]
    fn status_is_produced_only_when_it_changes() {
        let mut harness = Harness::ready();
        let mut statuses = 0;
        for input in [
            PttInput::Tick,
            PttInput::Connected(true),
            UP,
            DOWN, // changes: transmitting
            DOWN,
            PttInput::Tick,
            UP, // no change yet: the tail is part of the transmission
        ] {
            statuses += harness
                .send(input)
                .iter()
                .filter(|output| matches!(output, PttOutput::Status(_)))
                .count();
        }
        assert_eq!(statuses, 1);
        assert_eq!(harness.finish(), 1);
    }

    #[test]
    fn line_commands_parse_from_lines() {
        let cases = [
            ("down", Ok(LineCommand::Down)),
            ("up", Ok(LineCommand::Up)),
            ("mute", Ok(LineCommand::Mute)),
            ("deafen", Ok(LineCommand::Deafen)),
            ("quit", Ok(LineCommand::Quit)),
            ("down\n", Ok(LineCommand::Down)),
            ("up\r\n", Ok(LineCommand::Up)),
            ("  quit  ", Ok(LineCommand::Quit)),
            ("", Err(Error::UnknownCommand)),
            ("DOWN", Err(Error::UnknownCommand)),
            ("down up", Err(Error::UnknownCommand)),
            ("talk", Err(Error::UnknownCommand)),
            ("a-fake-token-typed-by-mistake", Err(Error::UnknownCommand)),
        ];
        for (line, want) in cases {
            assert_eq!(line.parse::<LineCommand>(), want, "{line:?}");
        }
        // What was typed is not echoed.
        let shown = Error::UnknownCommand.to_string();
        assert_eq!(
            shown,
            "unknown command: expected down, up, mute, deafen or quit"
        );
    }

    #[test]
    fn reasons_are_a_set_ordered_by_importance() {
        let mut reasons = ShutReasons::none();
        assert!(reasons.is_empty());
        assert_eq!(reasons.first(), None);
        assert!(reasons.insert(ShutReason::Muted));
        assert!(!reasons.insert(ShutReason::Muted));
        assert!(reasons.insert(ShutReason::NoMicrophone));
        assert!(reasons.contains(ShutReason::Muted));
        assert!(!reasons.contains(ShutReason::Deafened));
        assert_eq!(reasons.first(), Some(ShutReason::NoMicrophone));
        assert_eq!(format!("{reasons:?}"), "{NoMicrophone, Muted}");
        assert!(reasons.remove(ShutReason::NoMicrophone));
        assert!(!reasons.remove(ShutReason::NoMicrophone));
        assert_eq!(reasons.iter().collect::<Vec<_>>(), [ShutReason::Muted]);

        // Each reason has a bit of its own and words of its own.
        let mut all = ShutReasons::none();
        let mut words = std::collections::BTreeSet::new();
        for reason in ShutReason::ALL {
            assert!(all.insert(reason), "{reason:?}");
            assert!(words.insert(reason.to_string()), "{reason:?}");
        }
        assert_eq!(all.iter().collect::<Vec<_>>(), ShutReason::ALL);
    }

    /// A small deterministic generator, so that the test below needs no crate and is the
    /// same on every run.
    struct Lcg(u64);

    impl Lcg {
        fn next(&mut self, below: u64) -> u64 {
            self.0 = self
                .0
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            (self.0 >> 33) % below
        }
    }

    #[test]
    fn no_sequence_of_inputs_breaks_the_pairing_or_opens_a_blocked_gate() {
        // The properties themselves are asserted by Harness::check on every call: reports
        // alternate, started only with Open, stopped only once the gate has shut, Open only
        // with no reason standing and the gate shut, and the gate shut whenever a reason
        // stands.
        for seed in 0..200 {
            let mut random = Lcg(seed);
            let mut harness = Harness::ready();
            for _ in 0..400 {
                let input = match random.next(22) {
                    0..=4 => DOWN,
                    5..=8 => UP,
                    9 => key(Key::Talk, KeyAction::Press),
                    10 => key(Key::Talk, KeyAction::Release),
                    11 => MUTE,
                    12 => DEAFEN,
                    13 => key(Key::Mute, KeyAction::Press),
                    14 => key(Key::Deafen, KeyAction::Release),
                    15 => PttInput::Connected(random.next(3) > 0),
                    16 => PttInput::PublishGrant(random.next(3) > 0),
                    17 => PttInput::Microphone(random.next(3) > 0),
                    18 => PttInput::KeyDevice(random.next(3) > 0),
                    19 => PttInput::Tick,
                    20 => {
                        harness.advance(MAX / 3);
                        PttInput::Tick
                    }
                    _ => {
                        if harness.gate == FakeGate::Tail {
                            harness.tail_ends();
                        }
                        PttInput::Tick
                    }
                };
                harness.advance(Duration::from_millis(random.next(500)));
                harness.send(input);
            }
            // Put everything right, and the client can talk again: nothing is left stuck.
            if harness.gate == FakeGate::Tail {
                harness.tail_ends();
            }
            harness.send(UP);
            if harness.gate == FakeGate::Tail {
                harness.tail_ends();
            }
            harness.send(PttInput::Connected(true));
            harness.send(PttInput::PublishGrant(true));
            harness.send(PttInput::Microphone(true));
            harness.send(PttInput::KeyDevice(true));
            if harness.ptt.status().muted() {
                harness.send(MUTE);
            }
            if harness.ptt.status().deafened() {
                harness.send(DEAFEN);
            }
            assert_eq!(
                harness.ptt.status().blocked,
                ShutReasons::none(),
                "seed {seed}"
            );
            let before = harness.opens;
            assert_eq!(
                harness.send(DOWN),
                [Gate(Open), Report(Started), status(true, &[])],
                "seed {seed}"
            );
            assert_eq!(harness.finish(), before + 1, "seed {seed}");
        }
    }

    #[test]
    fn a_quit_at_any_point_ends_with_the_reports_paired() {
        for seed in 0..100 {
            let mut random = Lcg(seed + 1000);
            let mut harness = Harness::ready();
            for _ in 0..random.next(30) {
                let input = match random.next(8) {
                    0..=2 => DOWN,
                    3..=4 => UP,
                    5 => MUTE,
                    6 => PttInput::Connected(random.next(2) > 0),
                    _ => {
                        if harness.gate == FakeGate::Tail {
                            harness.tail_ends();
                        }
                        PttInput::Tick
                    }
                };
                harness.send(input);
            }
            harness.send(QUIT);
            if harness.gate == FakeGate::Tail {
                harness.tail_ends();
            }
            assert!(harness.quit, "seed {seed}");
            assert_ne!(harness.last_report, Some(Started), "seed {seed}");
            assert_eq!(harness.gate, FakeGate::Shut, "seed {seed}");
        }
    }
}
