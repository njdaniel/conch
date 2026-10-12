//! The transmit gate: microphone audio is handed onward only while it is open.

use std::fmt;

use crate::{AudioError, FRAME_MS, Frame};

/// A condition that forces the gate shut, whatever the talk key is doing.
///
/// Each can be set with [`TransmitGate::force_shut`] and cleared with
/// [`TransmitGate::clear_force`]. While any is set the gate cannot be opened.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ForceShut {
    /// The connection to the room is not established.
    NotConnected,
    /// The session's grant does not allow publishing.
    NoPublishGrant,
    /// There is no microphone.
    NoMicrophone,
    /// The device the talk key is read from has gone away.
    KeyDeviceLost,
    /// The user has deafened.
    Deafened,
    /// The user has muted.
    Muted,
    /// A single press has lasted longer than allowed (a stuck key).
    MaxPressLength,
}

impl ForceShut {
    /// Every forcing condition, in the order [`TransmitGate::blocked_by`] reports them.
    pub const ALL: [ForceShut; 7] = [
        ForceShut::NotConnected,
        ForceShut::NoPublishGrant,
        ForceShut::NoMicrophone,
        ForceShut::KeyDeviceLost,
        ForceShut::Deafened,
        ForceShut::Muted,
        ForceShut::MaxPressLength,
    ];

    const fn bit(self) -> u8 {
        1 << (self as u8)
    }
}

/// Why the gate is shut.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ShutReason {
    /// It has not been opened since it was made.
    NeverOpened,
    /// A shut command (a release), after the release tail if there is one.
    Released,
    /// A forcing condition.
    Forced(ForceShut),
}

/// The gate's two settings.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct GateConfig {
    /// How long the gate keeps forwarding after a shut command, so the last syllable is not
    /// cut. Counted in whole frames and rounded down, so the gate never forwards for longer
    /// than this.
    pub release_tail_ms: u32,
    /// The longest a single transmission may last, counted in forwarded frames (rounded down
    /// to whole frames). When it is reached the gate shuts itself with
    /// [`ForceShut::MaxPressLength`], so the limit holds even if nothing else is timing the
    /// press. `None` leaves that to the caller, through [`TransmitGate::force_shut`].
    pub max_open_ms: Option<u32>,
}

/// What the gate is doing, for a status line. This is for display: it is not how audio gets
/// through, [`TransmitGate::pass`] is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GateStatus {
    /// Shut: a forcing condition that is set now, or else why it last shut.
    Shut(ShutReason),
    /// Open.
    Open,
    /// A shut command has arrived and the release tail is running.
    Closing {
        /// Audio still to be forwarded before it shuts.
        remaining_ms: u32,
    },
}

/// The answer to an open command.
#[must_use = "an open command can be refused"]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OpenOutcome {
    /// The gate was shut and is now open: a transmission has started. Reported once per
    /// transmission.
    Opened,
    /// The gate was already open, or in its release tail, which this cancels. The same
    /// transmission continues and nothing new is reported.
    AlreadyOpen,
    /// A forcing condition is set; the gate stays shut.
    Refused(ForceShut),
}

/// The report that a transmission has ended: the gate went from open to shut.
/// Reported once per transmission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct GateShut {
    /// Why it shut.
    pub reason: ShutReason,
    /// When it shut, as the number of frames the gate had been offered since it was made.
    /// Each is 10 ms, so this is the gate's own clock.
    pub at_frame: u64,
    /// How many frames this transmission forwarded, release tail included.
    pub forwarded_frames: u64,
}

impl GateShut {
    /// When the gate shut, in milliseconds of offered audio since it was made.
    #[must_use]
    pub fn at_ms(&self) -> u64 {
        self.at_frame.saturating_mul(u64::from(FRAME_MS))
    }

    /// The length of the transmission, in milliseconds.
    #[must_use]
    pub fn forwarded_ms(&self) -> u64 {
        self.forwarded_frames.saturating_mul(u64::from(FRAME_MS))
    }
}

/// A captured frame that the gate let through.
///
/// It cannot be made anywhere but in [`TransmitGate::pass`]: its field is private and it has
/// no constructor, so code that accepts only `GatedFrame` cannot be handed audio that did not
/// pass the gate.
///
/// ```compile_fail
/// let captured = [0.5_f32; conch_voice_audio::FRAME_LEN];
/// let forged = conch_voice_audio::GatedFrame { samples: &captured };
/// ```
#[derive(Clone, Copy)]
pub struct GatedFrame<'a> {
    samples: &'a Frame,
}

impl<'a> GatedFrame<'a> {
    /// The samples to send.
    #[must_use]
    pub fn samples(&self) -> &'a Frame {
        self.samples
    }
}

/// Prints no audio.
impl fmt::Debug for GatedFrame<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("GatedFrame").finish_non_exhaustive()
    }
}

/// What became of one captured frame.
#[must_use = "a frame is sent only if the gate returns it"]
#[derive(Debug, Clone, Copy)]
pub enum Gated<'a> {
    /// The gate is shut. The frame goes nowhere.
    Dropped,
    /// The gate is open. Send this frame.
    Forward(GatedFrame<'a>),
    /// Send this frame; it is the last of the transmission, and the gate has now shut.
    Last(GatedFrame<'a>, GateShut),
}

impl<'a> Gated<'a> {
    /// The frame to send, if the gate let it through.
    #[must_use]
    pub fn frame(&self) -> Option<GatedFrame<'a>> {
        match *self {
            Gated::Dropped => None,
            Gated::Forward(frame) | Gated::Last(frame, _) => Some(frame),
        }
    }

    /// The report that the gate shut on this frame, if it did.
    #[must_use]
    pub fn shut(&self) -> Option<GateShut> {
        match *self {
            Gated::Dropped | Gated::Forward(_) => None,
            Gated::Last(_, shut) => Some(shut),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum State {
    Shut(ShutReason),
    Open,
    /// A shut command has arrived; this many more frames are forwarded. Never zero.
    Tail {
        remaining: u32,
    },
}

/// The transmit gate.
///
/// Guarantees:
///
/// - A captured frame comes out, as a [`GatedFrame`], only from [`pass`](Self::pass) and only
///   while the gate is open or in its release tail. While it is shut every frame is dropped.
/// - It starts shut and opens only on an [`open`](Self::open) command, and only when no
///   forcing condition is set. Clearing a condition never opens it: after any shut, forced or
///   not, the next thing that opens it is a new open command.
/// - A forced shut is immediate, with no release tail. A shut command is followed by exactly
///   the release tail and no more, however many times the command is repeated.
/// - Every transmission is reported once as opened ([`OpenOutcome::Opened`]) and once as shut
///   (a [`GateShut`]), in that order.
/// - Its time is the frames it is offered, 10 ms each; it reads no clock.
///
/// Nothing here allocates, locks or logs: every method is a few integer operations on fields
/// of fixed size, so all of them may be called next to a real-time callback.
#[derive(Debug)]
pub struct TransmitGate {
    state: State,
    /// One bit per [`ForceShut`] that is set. Not zero only while `state` is `Shut`.
    forced: u8,
    tail_frames: u32,
    max_frames: Option<u64>,
    /// Frames offered since the gate was made.
    offered: u64,
    /// Frames forwarded by the current transmission, or by the last one.
    forwarded: u64,
}

impl TransmitGate {
    /// A shut gate that has never been opened, with no forcing condition set.
    ///
    /// # Errors
    ///
    /// [`AudioError::InvalidSetting`] if `max_open_ms` is shorter than one frame.
    pub fn new(config: GateConfig) -> Result<Self, AudioError> {
        let max_frames = match config.max_open_ms {
            None => None,
            Some(ms) if ms >= FRAME_MS => Some(u64::from(ms / FRAME_MS)),
            Some(ms) => {
                return Err(AudioError::InvalidSetting {
                    what: "max_open_ms",
                    must_be: "at least one frame (10 ms)",
                    got: f64::from(ms),
                });
            }
        };
        Ok(Self {
            state: State::Shut(ShutReason::NeverOpened),
            forced: 0,
            tail_frames: config.release_tail_ms / FRAME_MS,
            max_frames,
            offered: 0,
            forwarded: 0,
        })
    }

    /// An open command: a press of the talk key.
    ///
    /// Opens the gate unless a forcing condition is set. During the release tail it cancels
    /// the tail and the same transmission goes on.
    pub fn open(&mut self) -> OpenOutcome {
        if let Some(why) = self.blocked_by() {
            return OpenOutcome::Refused(why);
        }
        match self.state {
            State::Shut(_) => {
                self.state = State::Open;
                self.forwarded = 0;
                OpenOutcome::Opened
            }
            State::Open => OpenOutcome::AlreadyOpen,
            State::Tail { .. } => {
                self.state = State::Open;
                OpenOutcome::AlreadyOpen
            }
        }
    }

    /// A shut command: a release of the talk key.
    ///
    /// With a release tail the gate goes on forwarding for exactly that long and then shuts;
    /// that is reported by [`pass`](Self::pass), on the last frame it forwards. Without one
    /// the gate shuts now and this returns the report. Repeating the command during the tail
    /// does not lengthen it, and on a shut gate it does nothing.
    #[must_use = "with no release tail this is the report that the transmission ended"]
    pub fn shut(&mut self) -> Option<GateShut> {
        match self.state {
            State::Open if self.tail_frames == 0 => Some(self.close(ShutReason::Released)),
            State::Open => {
                self.state = State::Tail {
                    remaining: self.tail_frames,
                };
                None
            }
            State::Tail { .. } | State::Shut(_) => None,
        }
    }

    /// Sets a forcing condition. If the gate was open, or in its release tail, it shuts now,
    /// with no tail, and this returns the report.
    ///
    /// The condition stays set until [`clear_force`](Self::clear_force) is called for it.
    #[must_use = "if the gate was open this is the report that the transmission ended"]
    pub fn force_shut(&mut self, why: ForceShut) -> Option<GateShut> {
        self.forced |= why.bit();
        match self.state {
            State::Open | State::Tail { .. } => Some(self.close(ShutReason::Forced(why))),
            State::Shut(_) => None,
        }
    }

    /// Clears a forcing condition. This never opens the gate: it stays shut until the next
    /// open command.
    pub fn clear_force(&mut self, why: ForceShut) {
        self.forced &= !why.bit();
    }

    /// The forcing condition that would refuse an open command now, if any. When several are
    /// set, the first in [`ForceShut::ALL`].
    #[must_use]
    pub fn blocked_by(&self) -> Option<ForceShut> {
        ForceShut::ALL
            .into_iter()
            .find(|why| self.forced & why.bit() != 0)
    }

    /// What the gate is doing, for a status line.
    #[must_use]
    pub fn status(&self) -> GateStatus {
        match self.state {
            State::Open => GateStatus::Open,
            State::Tail { remaining } => GateStatus::Closing {
                remaining_ms: remaining.saturating_mul(FRAME_MS),
            },
            State::Shut(last) => {
                GateStatus::Shut(self.blocked_by().map_or(last, ShutReason::Forced))
            }
        }
    }

    /// Offers the gate one captured frame. This is the only way a frame gets through.
    ///
    /// Shut: [`Gated::Dropped`]. Open: [`Gated::Forward`] with the same samples, unchanged.
    /// If this frame ends the release tail, or reaches the longest transmission allowed, it is
    /// still forwarded, as [`Gated::Last`], together with the report that the gate has shut.
    pub fn pass<'a>(&mut self, frame: &'a Frame) -> Gated<'a> {
        self.offered = self.offered.saturating_add(1);
        let tail_left = match self.state {
            State::Shut(_) => return Gated::Dropped,
            State::Open => None,
            State::Tail { remaining } => Some(remaining.saturating_sub(1)),
        };
        self.forwarded = self.forwarded.saturating_add(1);
        let frame = GatedFrame { samples: frame };
        if tail_left == Some(0) {
            return Gated::Last(frame, self.close(ShutReason::Released));
        }
        if self.max_frames.is_some_and(|max| self.forwarded >= max) {
            let why = ShutReason::Forced(ForceShut::MaxPressLength);
            return Gated::Last(frame, self.close(why));
        }
        if let Some(remaining) = tail_left {
            self.state = State::Tail { remaining };
        }
        Gated::Forward(frame)
    }

    fn close(&mut self, reason: ShutReason) -> GateShut {
        self.state = State::Shut(reason);
        GateShut {
            reason,
            at_frame: self.offered,
            forwarded_frames: self.forwarded,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::FRAME_LEN;
    use crate::testutil::Lcg;

    /// A frame in which every sample is non-zero.
    const LOUD: Frame = [0.5; FRAME_LEN];

    fn gate(release_tail_ms: u32, max_open_ms: Option<u32>) -> TransmitGate {
        TransmitGate::new(GateConfig {
            release_tail_ms,
            max_open_ms,
        })
        .unwrap()
    }

    /// Offers `frames` loud frames and returns how many samples came out.
    fn samples_forwarded(gate: &mut TransmitGate, frames: usize) -> usize {
        (0..frames)
            .filter_map(|_| gate.pass(&LOUD).frame())
            .map(|frame| frame.samples().len())
            .sum()
    }

    struct ShutCase {
        name: String,
        reason: ShutReason,
        /// Brings a new gate (30 ms tail, 50 ms longest transmission) to the shut state.
        arrange: Box<dyn Fn(&mut TransmitGate)>,
    }

    fn shut_cases() -> Vec<ShutCase> {
        let mut cases = vec![
            ShutCase {
                name: "never opened".into(),
                reason: ShutReason::NeverOpened,
                arrange: Box::new(|_| {}),
            },
            ShutCase {
                name: "released, after the tail".into(),
                reason: ShutReason::Released,
                arrange: Box::new(|gate| {
                    assert_eq!(gate.open(), OpenOutcome::Opened);
                    assert!(gate.shut().is_none());
                    // 30 ms of tail is three frames; the third reports the shut.
                    assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
                    assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
                    assert!(matches!(gate.pass(&LOUD), Gated::Last(..)));
                }),
            },
            ShutCase {
                name: "longest transmission reached, timed by the gate".into(),
                reason: ShutReason::Forced(ForceShut::MaxPressLength),
                arrange: Box::new(|gate| {
                    assert_eq!(gate.open(), OpenOutcome::Opened);
                    for _ in 0..4 {
                        assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
                    }
                    assert!(matches!(gate.pass(&LOUD), Gated::Last(..)));
                }),
            },
        ];
        for why in ForceShut::ALL {
            cases.push(ShutCase {
                name: format!("{why:?}, set before the press"),
                reason: ShutReason::Forced(why),
                arrange: Box::new(move |gate| {
                    assert!(gate.force_shut(why).is_none());
                    assert_eq!(gate.open(), OpenOutcome::Refused(why));
                }),
            });
            cases.push(ShutCase {
                name: format!("{why:?}, set while open"),
                reason: ShutReason::Forced(why),
                arrange: Box::new(move |gate| {
                    assert_eq!(gate.open(), OpenOutcome::Opened);
                    assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
                    assert!(gate.force_shut(why).is_some());
                }),
            });
            cases.push(ShutCase {
                name: format!("{why:?}, set during the release tail"),
                reason: ShutReason::Forced(why),
                arrange: Box::new(move |gate| {
                    assert_eq!(gate.open(), OpenOutcome::Opened);
                    assert!(gate.shut().is_none());
                    assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
                    assert!(gate.force_shut(why).is_some());
                }),
            });
            cases.push(ShutCase {
                name: format!("{why:?}, set while open and then cleared"),
                reason: ShutReason::Forced(why),
                arrange: Box::new(move |gate| {
                    assert_eq!(gate.open(), OpenOutcome::Opened);
                    assert!(gate.force_shut(why).is_some());
                    gate.clear_force(why);
                }),
            });
        }
        cases
    }

    #[test]
    fn a_shut_gate_forwards_no_sample_for_every_reason_it_can_be_shut() {
        let cases = shut_cases();
        // Every reason in the issue is covered: never opened, released, and the seven forced.
        assert!(
            cases
                .iter()
                .any(|case| case.reason == ShutReason::NeverOpened)
        );
        assert!(cases.iter().any(|case| case.reason == ShutReason::Released));
        for why in ForceShut::ALL {
            assert!(
                cases
                    .iter()
                    .any(|case| case.reason == ShutReason::Forced(why))
            );
        }

        for case in cases {
            let mut gate = gate(30, Some(50));
            (case.arrange)(&mut gate);
            assert_eq!(
                gate.status(),
                GateStatus::Shut(case.reason),
                "{}",
                case.name
            );
            // Two seconds of a signal that is never zero.
            assert_eq!(samples_forwarded(&mut gate, 200), 0, "{}", case.name);
            assert_eq!(
                gate.status(),
                GateStatus::Shut(case.reason),
                "{}",
                case.name
            );
        }
    }

    #[test]
    fn an_open_gate_forwards_the_captured_frame_unchanged() {
        let mut gate = gate(0, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        let mut captured = [0.0; FRAME_LEN];
        for (i, sample) in captured.iter_mut().enumerate() {
            *sample = i as f32 / FRAME_LEN as f32 - 0.5;
        }
        let Gated::Forward(frame) = gate.pass(&captured) else {
            panic!("an open gate must forward");
        };
        assert!(std::ptr::eq(frame.samples(), &captured));
        assert_eq!(frame.samples(), &captured);
    }

    #[test]
    fn a_forced_shut_that_clears_while_the_key_is_held_does_not_reopen_the_gate() {
        for why in ForceShut::ALL {
            let mut gate = gate(100, None);
            assert_eq!(gate.open(), OpenOutcome::Opened);
            assert_eq!(samples_forwarded(&mut gate, 5), 5 * FRAME_LEN);

            let report = gate.force_shut(why).unwrap();
            assert_eq!(report.reason, ShutReason::Forced(why));
            assert_eq!(samples_forwarded(&mut gate, 50), 0, "{why:?}");

            // The condition clears. The key is still held: no command arrives.
            gate.clear_force(why);
            assert_eq!(gate.blocked_by(), None);
            assert_eq!(samples_forwarded(&mut gate, 500), 0, "{why:?}");
            assert_eq!(gate.status(), GateStatus::Shut(ShutReason::Forced(why)));

            // Only a new open command reopens it.
            assert_eq!(gate.open(), OpenOutcome::Opened);
            assert_eq!(samples_forwarded(&mut gate, 5), 5 * FRAME_LEN);
        }
    }

    #[test]
    fn a_release_while_forced_shut_changes_nothing() {
        let mut gate = gate(100, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert!(gate.force_shut(ForceShut::Muted).is_some());
        // The key comes up after the forced shut: no second report, no tail.
        assert!(gate.shut().is_none());
        assert_eq!(samples_forwarded(&mut gate, 50), 0);
        gate.clear_force(ForceShut::Muted);
        assert_eq!(samples_forwarded(&mut gate, 50), 0);
    }

    #[test]
    fn an_open_command_is_refused_while_any_forcing_condition_is_set() {
        let mut gate = gate(0, None);
        for why in ForceShut::ALL {
            assert!(gate.force_shut(why).is_none());
        }
        // Clearing them one by one: refused, naming the first still set, until none is.
        for (i, why) in ForceShut::ALL.into_iter().enumerate() {
            assert_eq!(gate.blocked_by(), Some(why));
            assert_eq!(gate.open(), OpenOutcome::Refused(why));
            assert_eq!(samples_forwarded(&mut gate, 3), 0);
            gate.clear_force(why);
            assert_eq!(gate.blocked_by(), ForceShut::ALL.get(i + 1).copied());
        }
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(samples_forwarded(&mut gate, 3), 3 * FRAME_LEN);
    }

    #[test]
    fn setting_a_condition_twice_takes_one_clear() {
        let mut gate = gate(0, None);
        assert!(gate.force_shut(ForceShut::Deafened).is_none());
        assert!(gate.force_shut(ForceShut::Deafened).is_none());
        gate.clear_force(ForceShut::Deafened);
        assert_eq!(gate.open(), OpenOutcome::Opened);
    }

    #[test]
    fn the_release_tail_forwards_exactly_release_tail_ms_and_reports_when_it_shut() {
        struct Case {
            release_tail_ms: u32,
            tail_frames: u64,
        }
        let cases = [
            Case {
                release_tail_ms: 0,
                tail_frames: 0,
            },
            Case {
                release_tail_ms: 10,
                tail_frames: 1,
            },
            Case {
                release_tail_ms: 100,
                tail_frames: 10,
            },
            Case {
                release_tail_ms: 250,
                tail_frames: 25,
            },
            // Not a whole number of frames: never longer than asked.
            Case {
                release_tail_ms: 109,
                tail_frames: 10,
            },
            Case {
                release_tail_ms: 9,
                tail_frames: 0,
            },
        ];
        for case in cases {
            let ms = case.release_tail_ms;
            let mut gate = gate(ms, None);
            // Three dropped frames first, so the gate's clock and the transmission differ.
            assert_eq!(samples_forwarded(&mut gate, 3), 0);
            assert_eq!(gate.open(), OpenOutcome::Opened);
            assert_eq!(samples_forwarded(&mut gate, 7), 7 * FRAME_LEN);

            let mut reports = Vec::new();
            reports.extend(gate.shut());
            assert_eq!(reports.len(), usize::from(case.tail_frames == 0), "{ms} ms");

            let mut forwarded_after_release = 0_u64;
            for offered in 1..=100_u64 {
                let gated = gate.pass(&LOUD);
                if gated.frame().is_some() {
                    forwarded_after_release += 1;
                    // The tail is the frames straight after the command, with no gap.
                    assert_eq!(forwarded_after_release, offered, "{ms} ms");
                }
                reports.extend(gated.shut());
            }
            assert_eq!(forwarded_after_release, case.tail_frames, "{ms} ms");
            assert_eq!(
                reports,
                [GateShut {
                    reason: ShutReason::Released,
                    at_frame: 3 + 7 + case.tail_frames,
                    forwarded_frames: 7 + case.tail_frames,
                }],
                "{ms} ms"
            );
            assert_eq!(reports[0].at_ms(), (10 + case.tail_frames) * 10);
            assert_eq!(reports[0].forwarded_ms(), (7 + case.tail_frames) * 10);
        }
    }

    #[test]
    fn the_shut_is_reported_on_the_last_frame_of_the_tail() {
        let mut gate = gate(30, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert!(gate.shut().is_none());
        assert_eq!(gate.status(), GateStatus::Closing { remaining_ms: 30 });
        assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
        assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
        assert_eq!(gate.status(), GateStatus::Closing { remaining_ms: 10 });
        let Gated::Last(_, report) = gate.pass(&LOUD) else {
            panic!("the third frame ends a 30 ms tail");
        };
        assert_eq!(report.reason, ShutReason::Released);
        assert_eq!(gate.status(), GateStatus::Shut(ShutReason::Released));
        assert!(matches!(gate.pass(&LOUD), Gated::Dropped));
    }

    #[test]
    fn repeating_the_shut_command_does_not_lengthen_the_tail() {
        let mut gate = gate(50, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert!(gate.shut().is_none());
        let mut forwarded = 0;
        for _ in 0..100 {
            assert!(gate.shut().is_none());
            forwarded += usize::from(gate.pass(&LOUD).frame().is_some());
        }
        assert_eq!(forwarded, 5);
    }

    #[test]
    fn a_forced_shut_has_no_tail() {
        let mut gate = gate(100, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(samples_forwarded(&mut gate, 4), 4 * FRAME_LEN);
        let report = gate.force_shut(ForceShut::NotConnected).unwrap();
        assert_eq!(
            report,
            GateShut {
                reason: ShutReason::Forced(ForceShut::NotConnected),
                at_frame: 4,
                forwarded_frames: 4,
            }
        );
        assert_eq!(samples_forwarded(&mut gate, 100), 0);
    }

    #[test]
    fn a_press_during_the_tail_continues_the_same_transmission() {
        let mut gate = gate(50, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(samples_forwarded(&mut gate, 2), 2 * FRAME_LEN);
        assert!(gate.shut().is_none());
        assert_eq!(samples_forwarded(&mut gate, 3), 3 * FRAME_LEN);

        // Pressed again with 20 ms of tail left: no new report, and the tail is forgotten.
        assert_eq!(gate.open(), OpenOutcome::AlreadyOpen);
        assert_eq!(gate.status(), GateStatus::Open);
        for _ in 0..100 {
            assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
        }

        // The next release gets a whole tail and the one report covers all of it.
        assert!(gate.shut().is_none());
        let mut reports = Vec::new();
        for _ in 0..20 {
            reports.extend(gate.pass(&LOUD).shut());
        }
        assert_eq!(
            reports,
            [GateShut {
                reason: ShutReason::Released,
                at_frame: 110,
                forwarded_frames: 110,
            }]
        );
    }

    #[test]
    fn the_longest_transmission_shuts_the_gate_to_the_frame() {
        let mut gate = gate(100, Some(30));
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
        assert!(matches!(gate.pass(&LOUD), Gated::Forward(_)));
        let Gated::Last(_, report) = gate.pass(&LOUD) else {
            panic!("the third frame reaches 30 ms");
        };
        assert_eq!(
            report,
            GateShut {
                reason: ShutReason::Forced(ForceShut::MaxPressLength),
                at_frame: 3,
                forwarded_frames: 3,
            }
        );
        // The key is still down. Nothing more is forwarded, for as long as it stays down.
        assert_eq!(samples_forwarded(&mut gate, 1000), 0);
        // A release then changes nothing, and a new press starts a new, equally bounded one.
        assert!(gate.shut().is_none());
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(samples_forwarded(&mut gate, 1000), 3 * FRAME_LEN);
    }

    #[test]
    fn the_longest_transmission_also_bounds_the_release_tail() {
        let mut gate = gate(100, Some(50));
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(samples_forwarded(&mut gate, 3), 3 * FRAME_LEN);
        assert!(gate.shut().is_none());
        // Ten frames of tail were due; only two fit under the limit.
        assert_eq!(samples_forwarded(&mut gate, 100), 2 * FRAME_LEN);
        assert_eq!(
            gate.status(),
            GateStatus::Shut(ShutReason::Forced(ForceShut::MaxPressLength))
        );
    }

    #[test]
    fn a_longest_transmission_shorter_than_a_frame_is_refused() {
        for ms in [0, 1, 9] {
            let err = TransmitGate::new(GateConfig {
                release_tail_ms: 100,
                max_open_ms: Some(ms),
            })
            .unwrap_err();
            assert!(matches!(
                err,
                AudioError::InvalidSetting {
                    what: "max_open_ms",
                    ..
                }
            ));
            assert!(err.to_string().contains("max_open_ms"));
        }
    }

    #[test]
    fn the_status_names_a_condition_set_now_before_why_it_last_shut() {
        let mut gate = gate(0, None);
        assert_eq!(gate.status(), GateStatus::Shut(ShutReason::NeverOpened));
        assert_eq!(gate.open(), OpenOutcome::Opened);
        assert_eq!(gate.status(), GateStatus::Open);
        assert!(gate.shut().is_some());
        assert_eq!(gate.status(), GateStatus::Shut(ShutReason::Released));
        assert!(gate.force_shut(ForceShut::NoMicrophone).is_none());
        assert_eq!(
            gate.status(),
            GateStatus::Shut(ShutReason::Forced(ForceShut::NoMicrophone))
        );
        gate.clear_force(ForceShut::NoMicrophone);
        assert_eq!(gate.status(), GateStatus::Shut(ShutReason::Released));
    }

    #[test]
    fn a_gated_frame_prints_no_audio() {
        let mut gate = gate(0, None);
        assert_eq!(gate.open(), OpenOutcome::Opened);
        let printed = format!("{:?}", gate.pass(&LOUD));
        assert!(!printed.contains("0.5"), "{printed}");
    }

    /// Random commands and frames. The test keeps its own account, from the commands it sent
    /// and the reports it was given, of whether a transmission is under way, and checks every
    /// frame against it.
    #[test]
    fn random_commands_never_forward_outside_a_reported_transmission() {
        // How each transmission ended, over every seed: [released with no tail, released
        // after the tail, forced, timed out by the gate].
        let mut endings = [0_u32; 4];
        for seed in 0..400 {
            let mut rng = Lcg::new(seed);
            let tail_frames = rng.below(6);
            let max_frames = (rng.below(3) == 0).then(|| 1 + rng.below(40));
            let mut gate = gate(tail_frames * FRAME_MS, max_frames.map(|n| n * FRAME_MS));

            let mut set: Vec<ForceShut> = Vec::new();
            let mut transmitting = false;
            // After a shut command: the tail frames still due. An open command forgets it.
            let mut tail_due: Option<u32> = None;
            let mut forwarded = 0_u64;
            let mut offered = 0_u64;
            let (mut opened, mut shut) = (0_u32, 0_u32);

            for step in 0..3000_u32 {
                match rng.below(60) {
                    0..=3 => match gate.open() {
                        OpenOutcome::Opened => {
                            assert!(!transmitting, "seed {seed} step {step}");
                            assert!(set.is_empty(), "seed {seed} step {step}");
                            transmitting = true;
                            tail_due = None;
                            forwarded = 0;
                            opened += 1;
                        }
                        OpenOutcome::AlreadyOpen => {
                            assert!(transmitting, "seed {seed} step {step}");
                            tail_due = None;
                        }
                        OpenOutcome::Refused(why) => {
                            assert!(!transmitting, "seed {seed} step {step}");
                            assert!(set.contains(&why), "seed {seed} step {step}");
                        }
                    },
                    4..=6 => match gate.shut() {
                        Some(report) => {
                            assert!(transmitting, "seed {seed} step {step}");
                            assert_eq!(tail_frames, 0, "seed {seed} step {step}");
                            assert_eq!(
                                report,
                                GateShut {
                                    reason: ShutReason::Released,
                                    at_frame: offered,
                                    forwarded_frames: forwarded,
                                },
                                "seed {seed} step {step}"
                            );
                            transmitting = false;
                            shut += 1;
                            endings[0] += 1;
                        }
                        None if transmitting => {
                            assert!(tail_frames > 0, "seed {seed} step {step}");
                            tail_due.get_or_insert(tail_frames);
                        }
                        None => {}
                    },
                    7 => {
                        let why = ForceShut::ALL[rng.below(7) as usize];
                        let report = gate.force_shut(why);
                        if !set.contains(&why) {
                            set.push(why);
                        }
                        assert_eq!(report.is_some(), transmitting, "seed {seed} step {step}");
                        if let Some(report) = report {
                            assert_eq!(
                                report,
                                GateShut {
                                    reason: ShutReason::Forced(why),
                                    at_frame: offered,
                                    forwarded_frames: forwarded,
                                },
                                "seed {seed} step {step}"
                            );
                            transmitting = false;
                            shut += 1;
                            endings[2] += 1;
                        }
                    }
                    8..=13 => {
                        let why = ForceShut::ALL[rng.below(7) as usize];
                        gate.clear_force(why);
                        set.retain(|&other| other != why);
                    }
                    _ => {
                        let captured = [step as f32 + 1.0; FRAME_LEN];
                        offered += 1;
                        let gated = gate.pass(&captured);
                        let Some(frame) = gated.frame() else {
                            assert!(!transmitting, "seed {seed} step {step}");
                            continue;
                        };
                        assert!(transmitting, "seed {seed} step {step}");
                        assert!(set.is_empty(), "seed {seed} step {step}");
                        assert_eq!(frame.samples(), &captured, "seed {seed} step {step}");
                        forwarded += 1;
                        if let Some(due) = tail_due.as_mut() {
                            assert!(*due > 0, "seed {seed} step {step}");
                            *due -= 1;
                        }
                        if let Some(max) = max_frames {
                            assert!(forwarded <= u64::from(max), "seed {seed} step {step}");
                        }
                        match gated.shut() {
                            None => {
                                assert_ne!(tail_due, Some(0), "seed {seed} step {step}");
                                assert_ne!(
                                    max_frames.map(u64::from),
                                    Some(forwarded),
                                    "seed {seed} step {step}"
                                );
                            }
                            Some(report) => {
                                let reason = if tail_due == Some(0) {
                                    endings[1] += 1;
                                    ShutReason::Released
                                } else {
                                    endings[3] += 1;
                                    assert_eq!(
                                        max_frames.map(u64::from),
                                        Some(forwarded),
                                        "seed {seed} step {step}"
                                    );
                                    ShutReason::Forced(ForceShut::MaxPressLength)
                                };
                                assert_eq!(
                                    report,
                                    GateShut {
                                        reason,
                                        at_frame: offered,
                                        forwarded_frames: forwarded,
                                    },
                                    "seed {seed} step {step}"
                                );
                                transmitting = false;
                                shut += 1;
                            }
                        }
                    }
                }
                // Exactly one opened and one shut per transmission, in that order.
                assert_eq!(opened, shut + u32::from(transmitting), "seed {seed}");
            }
            assert_eq!(gate.blocked_by().is_some(), !set.is_empty(), "seed {seed}");
        }
        // The runs really did exercise every way a transmission can end.
        assert!(endings.iter().all(|&n| n > 100), "{endings:?}");
    }
}
