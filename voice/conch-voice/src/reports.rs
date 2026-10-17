//! The transmit reports: each press and release, told to `conchd`
//! (`docs/design/conch-voice.md` §6, "Client side").
//!
//! One task serves one queue. It sends one report at a time, in the order they were queued,
//! each after the answer to the one before, because `conchd` takes reports in the order
//! they arrive: two in flight at once could arrive reversed and leave its state wrong.
//!
//! - A failed report is tried again a few times, in place, and each failure is reported to
//!   the session loop to be shown.
//! - Two answers are not tried again: 409 `voice_no_session` (the room was rotated or the
//!   session is gone, and the connection policy is already on its way back to `conchd`)
//!   and 429 (the bound on reports was hit).
//! - When the task gives up on a report it drops its client, and with it every connection
//!   that client held, and makes a new one. A request that was given up on can then not be
//!   written later by a connection that came back to life, behind a report queued after it.
//!
//! The queue has no bound and is filled from the session loop without waiting; nothing here
//! is called from the transmit task. One thing waits for a report, and only for a while: a
//! press reports `started` and opens the track once `conchd` has answered it, or after at
//! most a second (`session.rs`). [`Reports::push_answered`] is how it learns of the answer.
//! Nothing else waits for a report, and audio never depends on one: past that second the
//! press goes ahead whatever became of it.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use conch_voice_api::{Client, Error as ApiError, VoiceTransmitState};
use tokio::sync::{mpsc, oneshot};

/// How a report that was not delivered at once fared.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReportProblem {
    /// This attempt failed and another follows.
    Retrying {
        /// The attempt that failed, from 1.
        attempt: u32,
        /// How many attempts a report gets.
        of: u32,
    },
    /// Every attempt failed. The report was dropped, and the connection with it.
    GaveUp,
    /// `conchd` answered 409 `voice_no_session`. Not tried again.
    NoSession,
    /// `conchd` answered 429. Not tried again.
    RateLimited,
}

/// A report that did not go through at once, for the status display.
#[derive(Debug)]
pub struct ReportEvent {
    /// The report.
    pub state: VoiceTransmitState,
    /// What became of it.
    pub problem: ReportProblem,
    /// Why, in the API crate's words, which hold no secret.
    pub error: ApiError,
}

/// How the queue retries.
#[derive(Debug, Clone)]
pub struct ReportTimings {
    /// The waits between attempts. A report gets one attempt more than there are waits.
    pub waits: Vec<Duration>,
}

impl Default for ReportTimings {
    /// Three attempts over about a second and a half.
    fn default() -> Self {
        Self {
            waits: vec![Duration::from_millis(500), Duration::from_secs(1)],
        }
    }
}

/// What `conchd` made of a report, as far as a press that waits for its `started` needs
/// to know. Given once, at the first answer that settles it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Answer {
    /// `conchd` answered 204: the report is recorded.
    Delivered,
    /// `conchd` answered 409 `voice_no_session`: it holds no session for this client in the
    /// channel's room, and would record what follows as unreported.
    NoSession,
    /// `conchd` answered 401: it did not accept the login. Given at the first such answer,
    /// although the report is tried again as any other failure is.
    Unauthorized,
    /// Settled some other way: answered 429, or given up on.
    Settled,
}

/// Makes the task's client: a new one, with a connection pool of its own, each time.
pub type MakeClient = Box<dyn Fn() -> Result<Client, ApiError> + Send>;

enum Item {
    /// A report, and who is to be told what `conchd` made of it, if anyone is.
    Report(VoiceTransmitState, Option<oneshot::Sender<Answer>>),
    /// Answered when everything queued before it has been dealt with.
    Flush(oneshot::Sender<()>),
}

/// The handle on the report queue.
pub struct Reports {
    queue: mpsc::UnboundedSender<Item>,
    /// Reports that did not go through at once.
    pub events: mpsc::UnboundedReceiver<ReportEvent>,
    /// Reports `conchd` answered 204.
    pub delivered: Arc<AtomicU64>,
    /// Reports that were dropped: given up on, or answered 409 or 429.
    pub dropped: Arc<AtomicU64>,
}

impl Reports {
    /// Starts the task that serves the queue, for reports about `channel`.
    #[must_use]
    pub fn spawn(make_client: MakeClient, channel: String, timings: ReportTimings) -> Self {
        let (queue, items) = mpsc::unbounded_channel();
        let (event_tx, events) = mpsc::unbounded_channel();
        let delivered = Arc::new(AtomicU64::new(0));
        let dropped = Arc::new(AtomicU64::new(0));
        let task = Task {
            items,
            events: event_tx,
            make_client,
            client: None,
            channel,
            timings,
            delivered: Arc::clone(&delivered),
            dropped: Arc::clone(&dropped),
        };
        tokio::spawn(task.run());
        Self {
            queue,
            events,
            delivered,
            dropped,
        }
    }

    /// Queues a report. Never waits.
    pub fn push(&self, state: VoiceTransmitState) {
        // The task ends only when this handle is dropped.
        let _ = self.queue.send(Item::Report(state, None));
    }

    /// Queues a report, as [`Reports::push`], and tells `answered` what `conchd` made of it
    /// once that is known. A report that is still being tried says nothing until it is
    /// settled: whoever waits on it bounds the wait.
    pub fn push_answered(&self, state: VoiceTransmitState, answered: oneshot::Sender<Answer>) {
        let _ = self.queue.send(Item::Report(state, Some(answered)));
    }

    /// Waits until every report queued so far has been delivered or dropped, or `limit`
    /// has passed. True if the queue was emptied in time.
    pub async fn flush(&self, limit: Duration) -> bool {
        let (done, flushed) = oneshot::channel();
        if self.queue.send(Item::Flush(done)).is_err() {
            return false;
        }
        matches!(tokio::time::timeout(limit, flushed).await, Ok(Ok(())))
    }
}

struct Task {
    items: mpsc::UnboundedReceiver<Item>,
    events: mpsc::UnboundedSender<ReportEvent>,
    make_client: MakeClient,
    /// `None` before the first report and after one was given up on.
    client: Option<Client>,
    channel: String,
    timings: ReportTimings,
    delivered: Arc<AtomicU64>,
    dropped: Arc<AtomicU64>,
}

impl Task {
    async fn run(mut self) {
        while let Some(item) = self.items.recv().await {
            match item {
                Item::Report(state, mut answered) => self.deliver(state, &mut answered).await,
                Item::Flush(done) => {
                    let _ = done.send(());
                }
            }
        }
    }

    /// Sends one report, and returns only when it has an answer or has been given up on.
    /// `answered`, if there is one, is told as soon as that answer is known.
    async fn deliver(
        &mut self,
        state: VoiceTransmitState,
        answered: &mut Option<oneshot::Sender<Answer>>,
    ) {
        let attempts = u32::try_from(self.timings.waits.len())
            .unwrap_or(u32::MAX)
            .saturating_add(1);
        let mut attempt = 1;
        loop {
            let problem = match self.attempt(state).await {
                Ok(()) => {
                    self.delivered.fetch_add(1, Ordering::Relaxed);
                    tell(answered, Answer::Delivered);
                    return;
                }
                Err(error) => error,
            };
            if let ApiError::Unauthenticated(_) = problem {
                // Trying again will not make the login good: the press that waits is told
                // now, and need not wait out the retries.
                tell(answered, Answer::Unauthorized);
            }
            let wait = self.timings.waits.get(attempt as usize - 1).copied();
            let (what, wait) = match (&problem, wait) {
                (ApiError::NoSession(_), _) => (ReportProblem::NoSession, None),
                (ApiError::RateLimited(_), _) => (ReportProblem::RateLimited, None),
                (_, Some(wait)) => (
                    ReportProblem::Retrying {
                        attempt,
                        of: attempts,
                    },
                    Some(wait),
                ),
                (_, None) => {
                    // Given up on. Dropping the client closes every connection it held, so
                    // nothing of this request can still be written; the next report gets a
                    // new client and a new connection.
                    self.client = None;
                    (ReportProblem::GaveUp, None)
                }
            };
            let _ = self.events.send(ReportEvent {
                state,
                problem: what,
                error: problem,
            });
            match wait {
                Some(wait) => tokio::time::sleep(wait).await,
                None => {
                    self.dropped.fetch_add(1, Ordering::Relaxed);
                    tell(
                        answered,
                        match what {
                            ReportProblem::NoSession => Answer::NoSession,
                            _ => Answer::Settled,
                        },
                    );
                    return;
                }
            }
            attempt += 1;
        }
    }

    async fn attempt(&mut self, state: VoiceTransmitState) -> Result<(), ApiError> {
        let client = match self.client.take() {
            Some(client) => client,
            None => (self.make_client)()?,
        };
        // No audience: V4 transmits to the whole channel.
        let answer = client.transmit(&self.channel, state, None).await;
        self.client = Some(client);
        answer
    }
}

/// Tells whoever waits for a report's answer, the first time only. One who stopped waiting
/// is not told.
fn tell(answered: &mut Option<oneshot::Sender<Answer>>, answer: Answer) {
    if let Some(answered) = answered.take() {
        let _ = answered.send(answer);
    }
}
