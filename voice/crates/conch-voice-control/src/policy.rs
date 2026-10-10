//! The connection policy (`docs/design/conch-voice.md` §5): what to do after each way a
//! session request can be refused, a connection attempt can fail, or a connection can end.
//!
//! The caller maps what it saw (an HTTP status and error code from `conchd`, a disconnect
//! reason from the SDK) onto an [`Outcome`], and [`ConnectionPolicy::next`] answers with a
//! [`NextStep`]: stop, ask `conchd` for a session now, or wait and then ask. It never says
//! "reconnect": every connection attempt begins with a new session from `conchd`.
//!
//! Waits double from 1 s to 30 s and start again after a connection has lasted 30 s. Each is
//! shortened by a random amount, so that clients cut off together do not all come back
//! together: the wait is between half the nominal value and the whole of it. The randomness
//! and the time are the caller's, passed in.

use std::time::Duration;

/// The first wait after a failure, before jitter.
pub const BACKOFF_MIN: Duration = Duration::from_secs(1);
/// The longest wait, before jitter.
pub const BACKOFF_MAX: Duration = Duration::from_secs(30);
/// How long a connection must last for the waits to start again from [`BACKOFF_MIN`].
pub const STABLE_AFTER: Duration = Duration::from_secs(30);
/// The exit code of every [`NextStep::Stop`]: the same as `conch` uses for a failed command.
pub const EXIT_STOPPED: u8 = 1;

/// Doubling [`BACKOFF_MIN`] this many times passes [`BACKOFF_MAX`].
const MAX_DOUBLINGS: u32 = 5;

/// What happened: every row of the note's §5 table and every refusal the session endpoint
/// documents (`docs/design/voice-control-plane.md` §4).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Outcome {
    /// The session request was answered 401: there is no valid login.
    NotSignedIn,
    /// The session request was answered 400 `voice_requires_auth`: the server runs with
    /// `--auth off`, and voice is never anonymous.
    VoiceRequiresAuth,
    /// The session request was answered 403: the login is an agent's, not a person's.
    NotAPerson,
    /// The session request was answered 404: not a member of the channel, or no such
    /// channel. The server does not say which.
    NotAMember,
    /// The session request was answered 503 `voice_not_configured`.
    VoiceNotConfigured,
    /// The session request was answered 503 `voice_unavailable`: `conchd` cannot reach
    /// LiveKit at the moment.
    VoiceUnavailable,
    /// The session request got no answer: `conchd` could not be reached.
    ServerUnreachable,
    /// A session was issued, and connecting to LiveKit with it failed.
    ConnectFailed,
    /// Disconnected because the room was deleted: `conchd` rotated the channel's room.
    RoomDeleted,
    /// Disconnected because the same identity joined from elsewhere.
    DuplicateIdentity,
    /// Disconnected because `conchd` removed this participant from the room by name.
    ParticipantRemoved,
    /// Disconnected because the connection was lost and the SDK gave up resuming it.
    ConnectionLost,
    /// Disconnected because LiveKit is shutting down.
    ServerShutdown,
}

/// What to do next.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NextStep {
    /// Print `message` (one line, no secrets) and exit with `exit_code`, which is not 0.
    Stop {
        /// The reason, for the user.
        message: &'static str,
        /// The process's exit code.
        exit_code: u8,
    },
    /// Ask `conchd` for a session at once, and connect with it.
    AskNow,
    /// Wait this long, then ask `conchd` for a session and connect with it.
    RetryAfter(Duration),
}

/// How an outcome is handled, before any state is consulted.
enum Rule {
    /// Trying again cannot help.
    Stop(&'static str),
    /// `conchd` knows something the client does not: ask it at once, but only once.
    AskOnce,
    /// Something is down, and will be for an unknown time.
    BackOff,
}

impl Outcome {
    fn rule(self) -> Rule {
        match self {
            Outcome::NotSignedIn => Rule::Stop("not signed in: run `conch login` first"),
            Outcome::VoiceRequiresAuth => Rule::Stop(
                "voice needs a signed-in user, and this server runs without authentication",
            ),
            Outcome::NotAPerson => Rule::Stop("voice is for people: this login is not a person's"),
            Outcome::NotAMember => {
                Rule::Stop("not a member of this channel, or there is no such channel")
            }
            Outcome::VoiceNotConfigured => Rule::Stop("voice is not configured on this server"),
            Outcome::DuplicateIdentity => Rule::Stop(
                "this account joined voice from somewhere else, so this client has stopped",
            ),
            // The room is gone or this client was put out of it. Either way `conchd` decides
            // what happens next: a new session for the channel's new room, or a refusal that
            // says why (and then one of the rules above stops the client).
            Outcome::RoomDeleted | Outcome::ParticipantRemoved => Rule::AskOnce,
            Outcome::VoiceUnavailable
            | Outcome::ServerUnreachable
            | Outcome::ConnectFailed
            | Outcome::ConnectionLost
            | Outcome::ServerShutdown => Rule::BackOff,
        }
    }
}

/// The state behind the policy: how many waits there have been in a row, and whether the
/// one immediate request has been used. One per `join`, for as long as it runs.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ConnectionPolicy {
    /// Waits handed out since the last stable connection.
    waits: u32,
    /// An immediate request has been made since the last stable connection.
    asked_at_once: bool,
    /// When the current connection was established, if there is one.
    connected_at: Option<Duration>,
}

impl ConnectionPolicy {
    /// A policy with no failures behind it.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// The connection to the room was established at monotonic time `now`.
    pub fn connected(&mut self, now: Duration) {
        self.connected_at = Some(now);
    }

    /// Says what to do about `outcome`, which happened at monotonic time `now` (measured
    /// from the same origin as [`ConnectionPolicy::connected`]).
    ///
    /// `jitter` is a random number from the caller, uniform over all of `u32`. It decides
    /// where between half the nominal wait and the whole of it a [`NextStep::RetryAfter`]
    /// falls, to the millisecond; it is not used otherwise.
    pub fn next(&mut self, now: Duration, outcome: Outcome, jitter: u32) -> NextStep {
        // Whatever happened, the connection (if there was one) is over. If it lasted, the
        // trouble before it is forgotten.
        if let Some(since) = self.connected_at.take()
            && now.saturating_sub(since) >= STABLE_AFTER
        {
            self.waits = 0;
            self.asked_at_once = false;
        }

        match outcome.rule() {
            Rule::Stop(message) => NextStep::Stop {
                message,
                exit_code: EXIT_STOPPED,
            },
            Rule::AskOnce if !self.asked_at_once => {
                self.asked_at_once = true;
                NextStep::AskNow
            }
            Rule::AskOnce | Rule::BackOff => NextStep::RetryAfter(self.back_off(jitter)),
        }
    }

    /// The next wait: 1 s, 2 s, 4 s, 8 s, 16 s, then 30 s every time, each shortened by up
    /// to half.
    fn back_off(&mut self, jitter: u32) -> Duration {
        let nominal = BACKOFF_MIN
            .saturating_mul(1 << self.waits.min(MAX_DOUBLINGS))
            .min(BACKOFF_MAX);
        self.waits = self.waits.saturating_add(1);

        // In whole milliseconds, so that the result is exact: half, plus `jitter / 2^32` of
        // the other half. A half is at most 15 000 ms, so the product fits in a u64.
        let half_ms = u64::try_from(nominal.as_millis() / 2).unwrap_or(u64::MAX);
        let extra_ms = half_ms.saturating_mul(u64::from(jitter)) >> 32;
        Duration::from_millis(half_ms.saturating_add(extra_ms))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// No jitter taken off: the longest wait for a nominal value is 1 ms short of it.
    const LATEST: u32 = u32::MAX;
    /// All of the jitter taken off: exactly half the nominal value.
    const EARLIEST: u32 = 0;
    /// Exactly in the middle: three quarters of the nominal value.
    const MIDDLE: u32 = 1 << 31;

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    fn ms(n: u64) -> Duration {
        Duration::from_millis(n)
    }

    /// What the table below expects of an outcome that arrives on a new policy.
    #[derive(Debug, PartialEq)]
    enum First {
        Stop(&'static str),
        AskNow,
        BackOff,
    }

    /// One case per outcome. The match has no wildcard, so a new outcome does not compile
    /// until it has a row here.
    fn expected(outcome: Outcome) -> First {
        match outcome {
            // Refusals of the session request.
            Outcome::NotSignedIn => First::Stop("not signed in: run `conch login` first"),
            Outcome::VoiceRequiresAuth => First::Stop(
                "voice needs a signed-in user, and this server runs without authentication",
            ),
            Outcome::NotAPerson => First::Stop("voice is for people: this login is not a person's"),
            Outcome::NotAMember => {
                First::Stop("not a member of this channel, or there is no such channel")
            }
            Outcome::VoiceNotConfigured => First::Stop("voice is not configured on this server"),
            Outcome::VoiceUnavailable => First::BackOff,
            Outcome::ServerUnreachable => First::BackOff,
            // The connection attempt.
            Outcome::ConnectFailed => First::BackOff,
            // Disconnects.
            Outcome::RoomDeleted => First::AskNow,
            Outcome::DuplicateIdentity => First::Stop(
                "this account joined voice from somewhere else, so this client has stopped",
            ),
            Outcome::ParticipantRemoved => First::AskNow,
            Outcome::ConnectionLost => First::BackOff,
            Outcome::ServerShutdown => First::BackOff,
        }
    }

    const EVERY_OUTCOME: [Outcome; 13] = [
        Outcome::NotSignedIn,
        Outcome::VoiceRequiresAuth,
        Outcome::NotAPerson,
        Outcome::NotAMember,
        Outcome::VoiceNotConfigured,
        Outcome::VoiceUnavailable,
        Outcome::ServerUnreachable,
        Outcome::ConnectFailed,
        Outcome::RoomDeleted,
        Outcome::DuplicateIdentity,
        Outcome::ParticipantRemoved,
        Outcome::ConnectionLost,
        Outcome::ServerShutdown,
    ];

    #[test]
    fn every_outcome_has_its_next_step() {
        let mut distinct = std::collections::HashSet::new();
        for outcome in EVERY_OUTCOME {
            assert!(distinct.insert(outcome), "{outcome:?} is listed twice");
            let mut policy = ConnectionPolicy::new();
            let step = policy.next(secs(100), outcome, LATEST);
            match expected(outcome) {
                First::Stop(message) => {
                    assert_eq!(
                        step,
                        NextStep::Stop {
                            message,
                            exit_code: 1
                        },
                        "{outcome:?}"
                    );
                    assert!(
                        !message.is_empty() && !message.contains('\n'),
                        "{outcome:?}"
                    );
                }
                First::AskNow => assert_eq!(step, NextStep::AskNow, "{outcome:?}"),
                First::BackOff => {
                    assert_eq!(step, NextStep::RetryAfter(ms(999)), "{outcome:?}");
                }
            }
        }
        assert_eq!(distinct.len(), 13);
    }

    #[test]
    fn every_stop_has_a_message_of_its_own_and_a_nonzero_exit_code() {
        let mut messages = std::collections::HashSet::new();
        let mut stops = 0;
        for outcome in EVERY_OUTCOME {
            let step = ConnectionPolicy::new().next(secs(1), outcome, MIDDLE);
            if let NextStep::Stop { message, exit_code } = step {
                stops += 1;
                assert_ne!(exit_code, 0, "{outcome:?}");
                assert!(messages.insert(message), "{outcome:?} shares its message");
            }
        }
        // 401, 400, 403, 404, 503 voice_not_configured, and a duplicate identity.
        assert_eq!(stops, 6);
    }

    #[test]
    fn a_stop_is_a_stop_whatever_came_before() {
        let mut policy = ConnectionPolicy::new();
        for _ in 0..4 {
            policy.next(secs(1), Outcome::ConnectionLost, LATEST);
        }
        let before = policy.clone();
        assert!(matches!(
            policy.next(secs(2), Outcome::DuplicateIdentity, LATEST),
            NextStep::Stop { .. }
        ));
        assert_eq!(policy, before, "a stop changes nothing");
    }

    #[test]
    fn waits_double_from_one_second_to_thirty_and_stay_there() {
        let mut policy = ConnectionPolicy::new();
        let mut now = secs(0);
        // With all of the jitter taken off, each wait is exactly half its nominal value.
        let halves_ms = [
            500, 1_000, 2_000, 4_000, 8_000, 15_000, 15_000, 15_000, 15_000,
        ];
        for (attempt, half) in halves_ms.into_iter().enumerate() {
            let step = policy.next(now, Outcome::ServerUnreachable, EARLIEST);
            assert_eq!(step, NextStep::RetryAfter(ms(half)), "attempt {attempt}");
            now += ms(half);
        }
    }

    #[test]
    fn jitter_keeps_every_wait_between_half_the_nominal_value_and_all_of_it() {
        let nominal_ms: [u64; 8] = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000];
        // The jitter value, and the wait in milliseconds it gives for each nominal value: half
        // of it, plus that fraction of the other half.
        let cases: [(u32, [u64; 8]); 5] = [
            (
                EARLIEST,
                [500, 1_000, 2_000, 4_000, 8_000, 15_000, 15_000, 15_000],
            ),
            (
                1 << 30,
                [625, 1_250, 2_500, 5_000, 10_000, 18_750, 18_750, 18_750],
            ),
            (
                MIDDLE,
                [750, 1_500, 3_000, 6_000, 12_000, 22_500, 22_500, 22_500],
            ),
            (
                3 << 30,
                [875, 1_750, 3_500, 7_000, 14_000, 26_250, 26_250, 26_250],
            ),
            (
                LATEST,
                [999, 1_999, 3_999, 7_999, 15_999, 29_999, 29_999, 29_999],
            ),
        ];
        for (jitter, waits_ms) in cases {
            let mut policy = ConnectionPolicy::new();
            for (nominal, want) in nominal_ms.into_iter().zip(waits_ms) {
                let step = policy.next(secs(0), Outcome::ConnectionLost, jitter);
                assert_eq!(step, NextStep::RetryAfter(ms(want)), "jitter {jitter}");
                // The bounds, stated on their own: never under half, never the whole.
                assert!(nominal / 2 <= want && want < nominal, "jitter {jitter}");
                assert!(ms(want) < BACKOFF_MAX);
            }
        }
    }

    #[test]
    fn the_outcomes_that_back_off_share_one_sequence() {
        let mut policy = ConnectionPolicy::new();
        let steps = [
            (Outcome::ServerUnreachable, 500),
            (Outcome::VoiceUnavailable, 1_000),
            (Outcome::ConnectFailed, 2_000),
            (Outcome::ConnectionLost, 4_000),
            (Outcome::ServerShutdown, 8_000),
            (Outcome::ServerUnreachable, 15_000),
        ];
        for (outcome, want) in steps {
            assert_eq!(
                policy.next(secs(0), outcome, EARLIEST),
                NextStep::RetryAfter(ms(want)),
                "{outcome:?}"
            );
        }
    }

    #[test]
    fn the_waits_start_again_after_thirty_seconds_connected() {
        let mut policy = ConnectionPolicy::new();
        for _ in 0..6 {
            policy.next(secs(10), Outcome::ConnectFailed, EARLIEST);
        }
        // Connected at t=100 and lost at t=130, which is exactly long enough.
        policy.connected(secs(100));
        assert_eq!(
            policy.next(secs(130), Outcome::ConnectionLost, EARLIEST),
            NextStep::RetryAfter(ms(500))
        );
        assert_eq!(
            policy.next(secs(131), Outcome::ConnectFailed, EARLIEST),
            NextStep::RetryAfter(ms(1_000))
        );
    }

    #[test]
    fn a_connection_shorter_than_thirty_seconds_does_not_start_the_waits_again() {
        let mut policy = ConnectionPolicy::new();
        for want in [500, 1_000, 2_000] {
            assert_eq!(
                policy.next(secs(10), Outcome::ConnectFailed, EARLIEST),
                NextStep::RetryAfter(ms(want))
            );
        }
        // Connected at t=100 and lost one millisecond short of thirty seconds later.
        policy.connected(secs(100));
        assert_eq!(
            policy.next(secs(130) - ms(1), Outcome::ConnectionLost, EARLIEST),
            NextStep::RetryAfter(ms(4_000))
        );
        // A connection that keeps dropping after a few seconds keeps backing off.
        policy.connected(secs(140));
        assert_eq!(
            policy.next(secs(145), Outcome::ConnectionLost, EARLIEST),
            NextStep::RetryAfter(ms(8_000))
        );
    }

    #[test]
    fn only_time_connected_counts_toward_starting_again() {
        let mut policy = ConnectionPolicy::new();
        policy.next(secs(0), Outcome::ConnectFailed, EARLIEST);
        policy.next(secs(1), Outcome::ConnectFailed, EARLIEST);
        // An hour of failing to connect is not an hour connected.
        assert_eq!(
            policy.next(secs(3_600), Outcome::ConnectFailed, EARLIEST),
            NextStep::RetryAfter(ms(2_000))
        );
        // A connection that ended is not counted again by a later failure.
        policy.connected(secs(4_000));
        assert_eq!(
            policy.next(secs(4_001), Outcome::ConnectionLost, EARLIEST),
            NextStep::RetryAfter(ms(4_000))
        );
        assert_eq!(
            policy.next(secs(9_000), Outcome::ConnectFailed, EARLIEST),
            NextStep::RetryAfter(ms(8_000))
        );
    }

    #[test]
    fn a_deleted_room_asks_at_once_the_first_time_and_backs_off_if_that_fails() {
        let mut policy = ConnectionPolicy::new();
        policy.connected(secs(0));
        // The room is rotated after ten minutes: ask conchd for the new one at once.
        assert_eq!(
            policy.next(secs(600), Outcome::RoomDeleted, EARLIEST),
            NextStep::AskNow
        );
        // That request fails: conchd cannot reach LiveKit. Now it is a backoff, from 1 s.
        assert_eq!(
            policy.next(secs(600), Outcome::VoiceUnavailable, EARLIEST),
            NextStep::RetryAfter(ms(500))
        );
        assert_eq!(
            policy.next(secs(601), Outcome::VoiceUnavailable, EARLIEST),
            NextStep::RetryAfter(ms(1_000))
        );
        // It connects, and the new room is deleted five seconds later. Not at once again.
        policy.connected(secs(602));
        assert_eq!(
            policy.next(secs(607), Outcome::RoomDeleted, EARLIEST),
            NextStep::RetryAfter(ms(2_000))
        );
    }

    #[test]
    fn a_room_deleted_again_before_the_connection_has_lasted_backs_off() {
        let mut policy = ConnectionPolicy::new();
        policy.connected(secs(0));
        assert_eq!(
            policy.next(secs(60), Outcome::RoomDeleted, EARLIEST),
            NextStep::AskNow
        );
        policy.connected(secs(61));
        assert_eq!(
            policy.next(secs(62), Outcome::RoomDeleted, EARLIEST),
            NextStep::RetryAfter(ms(500))
        );
        policy.connected(secs(63));
        assert_eq!(
            policy.next(secs(64), Outcome::RoomDeleted, EARLIEST),
            NextStep::RetryAfter(ms(1_000))
        );
    }

    #[test]
    fn a_deleted_room_asks_at_once_again_after_a_connection_that_lasted() {
        let mut policy = ConnectionPolicy::new();
        policy.connected(secs(0));
        assert_eq!(
            policy.next(secs(60), Outcome::RoomDeleted, EARLIEST),
            NextStep::AskNow
        );
        policy.connected(secs(61));
        assert_eq!(
            policy.next(secs(91), Outcome::RoomDeleted, EARLIEST),
            NextStep::AskNow
        );
    }

    #[test]
    fn a_deleted_room_whose_member_is_no_longer_entitled_ends_in_a_stop() {
        // The note's §5: "if not, the request is refused and the client stops".
        let mut policy = ConnectionPolicy::new();
        policy.connected(secs(0));
        assert_eq!(
            policy.next(secs(60), Outcome::RoomDeleted, EARLIEST),
            NextStep::AskNow
        );
        assert_eq!(
            policy.next(secs(60), Outcome::NotAMember, EARLIEST),
            NextStep::Stop {
                message: "not a member of this channel, or there is no such channel",
                exit_code: 1
            }
        );
    }

    #[test]
    fn being_removed_and_a_deleted_room_share_the_one_immediate_request() {
        let mut policy = ConnectionPolicy::new();
        policy.connected(secs(0));
        assert_eq!(
            policy.next(secs(60), Outcome::ParticipantRemoved, EARLIEST),
            NextStep::AskNow
        );
        policy.connected(secs(61));
        assert_eq!(
            policy.next(secs(62), Outcome::RoomDeleted, EARLIEST),
            NextStep::RetryAfter(ms(500))
        );
    }

    #[test]
    fn a_clock_that_runs_backwards_does_not_panic_or_start_the_waits_again() {
        let mut policy = ConnectionPolicy::new();
        policy.next(secs(50), Outcome::ConnectFailed, EARLIEST);
        policy.connected(secs(100));
        assert_eq!(
            policy.next(secs(40), Outcome::ConnectionLost, EARLIEST),
            NextStep::RetryAfter(ms(1_000))
        );
    }

    #[test]
    fn the_waits_never_overflow_however_long_the_outage() {
        let mut policy = ConnectionPolicy::new();
        policy.waits = u32::MAX - 1;
        for _ in 0..3 {
            assert_eq!(
                policy.next(secs(0), Outcome::ServerUnreachable, LATEST),
                NextStep::RetryAfter(ms(29_999))
            );
        }
        assert_eq!(policy.waits, u32::MAX);
    }
}
