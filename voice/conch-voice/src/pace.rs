//! What wakes the transmit and mix tasks (`docs/design/conch-voice.md` §3).
//!
//! Input: the [`Pacing`] of a microphone source or of a speaker sink. Output: a future that
//! completes when the task should look at its source or sink again. Owns: the timer behind
//! that future.
//!
//! A source or sink with a clock of its own, which is the sound card's through PipeWire,
//! sets the pace: the task acts on what the ring holds, however much that is, and a late
//! look catches up instead of dropping anything. `dmn-audio`'s rings are lock-free and
//! cannot wake a task, so "looking" is a short timer, [`RING_LOOK`], and the decision is
//! the ring's. A source or sink with no clock (a tone, a WAV file, the counting sink) is
//! paced by a timer of one frame, as it always was.

use std::time::Duration;

use conch_voice_audio::{FRAME_MS, Pacing};
use tokio::time::{Interval, MissedTickBehavior};

/// How often a ring is looked at: a fifth of a frame. This bounds what looking adds to the
/// delay in either direction, and it is a bound, not a rate: a look that finds nothing costs
/// a few atomic loads, and one that finds more than a frame handles all of it.
pub const RING_LOOK: Duration = Duration::from_millis(2);

/// The interval a task with this pacing looks at its source or sink on.
#[must_use]
pub fn look_every(pacing: Pacing) -> Duration {
    match pacing {
        Pacing::Timer => Duration::from_millis(u64::from(FRAME_MS)),
        Pacing::Ring => RING_LOOK,
    }
}

/// The wake-ups of one task.
pub struct Pace {
    pacing: Pacing,
    tick: Interval,
    /// The last look found a whole frame, so the next one is not waited for.
    more: bool,
}

impl Pace {
    /// A pace for a source or sink with `pacing`. Must be made inside a tokio runtime.
    #[must_use]
    pub fn new(pacing: Pacing) -> Self {
        let mut tick = tokio::time::interval(look_every(pacing));
        // A look that is late is not made up for with a burst of looks: the ring holds what
        // arrived meanwhile, and the one look handles all of it.
        tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
        Self {
            pacing,
            tick,
            more: false,
        }
    }

    /// What paces this task.
    #[must_use]
    pub fn pacing(&self) -> Pacing {
        self.pacing
    }

    /// Says whether the look just made found something to act on. For a ring-paced task,
    /// the next look is then made at once (after letting other tasks run), so that a ring
    /// holding several frames is emptied in a row. A timer-paced task ignores it: its
    /// source is read once a frame.
    pub fn found(&mut self, something: bool) {
        self.more = something && self.pacing == Pacing::Ring;
    }

    /// Completes when the source or sink is to be looked at again.
    pub async fn wait(&mut self) {
        if self.more {
            // Cleared only once the yield is over: if a command wins the `select!` meanwhile
            // and this future is dropped, the look that was owed is still owed.
            tokio::task::yield_now().await;
            self.more = false;
        } else {
            self.tick.tick().await;
        }
    }
}

#[cfg(test)]
mod tests {
    use std::time::Instant;

    use super::*;

    #[test]
    fn a_timer_looks_once_a_frame_and_a_ring_looks_far_more_often() {
        assert_eq!(look_every(Pacing::Timer), Duration::from_millis(10));
        assert_eq!(look_every(Pacing::Ring), RING_LOOK);
        assert!(RING_LOOK * 2 <= look_every(Pacing::Timer));
    }

    #[tokio::test]
    async fn after_a_look_that_found_something_the_next_is_not_waited_for() {
        let mut pace = Pace::new(Pacing::Ring);
        pace.wait().await; // The first tick is immediate.
        let begun = Instant::now();
        for _ in 0..50 {
            pace.found(true);
            pace.wait().await;
        }
        // Waited for, fifty looks take fifty times `RING_LOOK`. Not waited for they take
        // well under a millisecond; the bound leaves a loaded machine most of the difference.
        assert!(
            begun.elapsed() < RING_LOOK * 40,
            "fifty looks in a row took {:?}",
            begun.elapsed()
        );
    }

    #[tokio::test]
    async fn a_timer_paced_task_waits_a_frame_whatever_it_found() {
        let mut pace = Pace::new(Pacing::Timer);
        pace.wait().await; // The first tick is immediate.
        pace.found(true);
        let begun = Instant::now();
        pace.wait().await;
        assert!(
            begun.elapsed() >= Duration::from_millis(5),
            "{:?}",
            begun.elapsed()
        );
    }
}
