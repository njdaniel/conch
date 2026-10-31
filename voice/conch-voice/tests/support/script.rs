//! Microphones for the open microphone's tests: one that plays a script of tone and silence
//! and can stop delivering, and one whose sound the test switches on and off. Both are read
//! once every 10 ms, as a tone or a WAV file is, and number every frame in its first
//! sample, so that a test can say which frames reached the track (`FakeSdk::first_samples`).

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use conch_voice::transmit::{BoxedMic, TxClock};
use conch_voice_audio::{FRAME_LEN, FRAME_MS, Frame, MicRead, MicSource};

use super::MakeMic;

/// The level of every sample of a loud frame but its first: -12 dBFS, far above the
/// default level of -40.
const LOUD: f32 = 0.25;
/// One frame number in a first sample: frame `n`'s first sample is `n` · 10⁻⁹, which leaves
/// a quiet frame far below any level the configuration accepts (-80 dBFS).
const STEP: f64 = 1e-9;

/// The number of the frame whose first sample is `sample`.
#[must_use]
pub fn frame_number(sample: f32) -> u64 {
    (f64::from(sample) / STEP).round() as u64
}

/// Frame `n`: a tone (here a constant level, which is all a level detector looks at) or
/// silence, told apart from every other by its first sample.
fn numbered(n: u64, loud: bool) -> Frame {
    let mut frame = [if loud { LOUD } else { 0.0 }; FRAME_LEN];
    frame[0] = (n as f64 * STEP) as f32;
    frame
}

/// What a script does once it has played.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum After {
    /// Silence, for ever.
    Quiet,
    /// No more frames at all: a source that stopped delivering.
    Stop,
    /// One quiet frame each time this long has passed, and nothing between: a source that
    /// trickles.
    Trickle(Duration),
}

/// What a script has played so far, shared with the test.
#[derive(Debug, Default)]
pub struct Played {
    /// Frames delivered.
    pub frames: AtomicU64,
    /// When the last frame was delivered.
    pub last_at: Mutex<Option<Instant>>,
    /// When the last loud frame was delivered.
    pub last_loud_at: Mutex<Option<Instant>>,
}

impl Played {
    /// A clock for the transmit task (`Timings::transmit_clock`) that counts this script's
    /// frames, 10 ms each, and nothing else: by it the hang, measured since the last sound,
    /// passes at the same frame as the hang the detector counts in frames. On the real
    /// clock, a loaded machine that reads the script late makes the hang pass, by the clock,
    /// a few frames before the frames say so (issue #957); which frames are sent is then
    /// decided by the machine's load and not by the script. Not for a script that stops or
    /// trickles: what those test is the real clock.
    pub fn clock(self: &Arc<Self>) -> TxClock {
        let played = Arc::clone(self);
        TxClock::from_fn(move || {
            Duration::from_millis(played.frames.load(Ordering::SeqCst) * u64::from(FRAME_MS))
        })
    }
}

/// A microphone that plays `(frames, loud)` parts in order, then does as `after` says.
pub struct Script {
    parts: Vec<(u64, bool)>,
    after: After,
    played: Arc<Played>,
}

impl Script {
    /// A script, and what it has played, as a microphone for `Setup::microphone`. The
    /// script plays from its start each time the session opens a microphone.
    pub fn make(parts: &[(u64, bool)], after: After) -> (MakeMic, Arc<Played>) {
        let played = Arc::new(Played::default());
        let (parts, shared) = (parts.to_vec(), Arc::clone(&played));
        let make: MakeMic = Arc::new(move || {
            Box::new(Script {
                parts: parts.clone(),
                after,
                played: Arc::clone(&shared),
            }) as BoxedMic
        });
        (make, played)
    }

    /// Whether frame `n` is loud, or `None` past the script's end.
    fn loud(&self, n: u64) -> Option<bool> {
        let mut start = 0;
        for &(frames, loud) in &self.parts {
            if n < start + frames {
                return Some(loud);
            }
            start += frames;
        }
        None
    }
}

impl MicSource for Script {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        let n = self.played.frames.load(Ordering::SeqCst);
        let loud = match (self.loud(n), self.after) {
            (Some(loud), _) => loud,
            (None, After::Quiet) => false,
            (None, After::Stop) => return MicRead::NotYet,
            (None, After::Trickle(every)) => {
                let last = *self.played.last_at.lock().unwrap();
                if last.is_some_and(|last| last.elapsed() < every) {
                    return MicRead::NotYet;
                }
                false
            }
        };
        *frame = numbered(n, loud);
        self.played.frames.store(n + 1, Ordering::SeqCst);
        let now = Instant::now();
        *self.played.last_at.lock().unwrap() = Some(now);
        if loud {
            *self.played.last_loud_at.lock().unwrap() = Some(now);
        }
        MicRead::Ready
    }
}

/// A microphone that never stops, loud while the test says so.
pub struct Switch(Arc<Switched>);

/// The test's side of a [`Switch`].
#[derive(Debug, Default)]
pub struct Switched {
    loud: AtomicBool,
    frames: AtomicU64,
    stalled: AtomicBool,
}

impl Switched {
    /// From the next read the microphone delivers no frame (a stalled device), or again does.
    pub fn stall(&self, stalled: bool) {
        self.stalled.store(stalled, Ordering::SeqCst);
    }

    /// Loud from the next frame, or quiet.
    pub fn set(&self, loud: bool) {
        self.loud.store(loud, Ordering::SeqCst);
    }

    /// Frames read so far.
    pub fn frames(&self) -> u64 {
        self.frames.load(Ordering::SeqCst)
    }

    /// Waits until `count` more frames have been read: time as the level detector counts
    /// it, however late the task that reads them is.
    pub async fn frames_pass(&self, count: u64) {
        let until = self.frames() + count;
        super::until("frames read", String::new, || self.frames() >= until).await;
    }
}

impl Switch {
    /// A switch, quiet to begin with, and its handle, as a microphone for
    /// `Setup::microphone`.
    pub fn make() -> (MakeMic, Arc<Switched>) {
        let switched = Arc::new(Switched::default());
        let shared = Arc::clone(&switched);
        let make: MakeMic = Arc::new(move || Box::new(Switch(Arc::clone(&shared))) as BoxedMic);
        (make, switched)
    }
}

impl MicSource for Switch {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        if self.0.stalled.load(Ordering::SeqCst) {
            return MicRead::NotYet;
        }
        let n = self.0.frames.load(Ordering::SeqCst);
        *frame = numbered(n, self.0.loud.load(Ordering::SeqCst));
        self.0.frames.store(n + 1, Ordering::SeqCst);
        MicRead::Ready
    }
}
