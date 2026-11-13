//! The Windows key source: the talk, mute and deafen keys, and the mouse's side buttons, read
//! from Raw Input in the background (`docs/design/windows-desktop.md` section 4), and handed to
//! the session loop as the same [`Input::Key`] and [`Input::KeyDevice`] the evdev watcher
//! ([`crate::keydev`]) sends, so nothing downstream knows the operating system.
//!
//! **Raw Input hands the helper every key pressed in every application, passwords included.**
//! This module is where those events enter the program, so it takes in as little as it can and
//! keeps none of it, as `keydev` does:
//!
//! - The events come from `winit`'s device events, which register for the keyboard and the
//!   mouse with `RIDEV_INPUTSINK | RIDEV_DEVNOTIFY` and nothing else (the audit at 0.30.13 is
//!   in the design note, section 4.3). A hidden window of `winit`'s own receives them; there
//!   is no window of this program's, no hook, no injection and no polling of key state *by
//!   this program*. `winit` itself contains code that polls key state (`GetAsyncKeyState`, in
//!   `keyboard.rs`'s `get_async_kbd_state`, run when one of its windows gains focus) and code
//!   that injects input (`SendInput`, in `window.rs`'s `force_window_active`, run when one of
//!   its windows is created or shown). Both are reached only through a `winit` `Window`, and
//!   **the helper never creates a `winit` `Window`** (the audit, design section 4.6): that
//!   invariant, not a feature flag, is what keeps them unreached, so it is the thing to check
//!   when `winit` is updated.
//! - [`Binder`] compares each event with the configured bindings and drops it. What leaves it
//!   is a [`KeyEvent`] (talk, mute or deafen went down or up) and a device state (the keys are
//!   being read, or are not, and why). Neither can hold a scan code, a time or a count of
//!   keys, and the raw event type, [`RawEvent`], is private to this module's thread: it has
//!   no `Display`, and its `Debug` shows no key.
//! - This module logs nothing, and an error it reports is the operating system's number and
//!   kind, as `keydev`'s is.
//!
//! **That is true of which keys are pressed, and not of when:** the thread wakes for every raw
//! input event, bound or not, and for a mouse's every movement, which `winit` produces and
//! this module drops at once. The rhythm of a person's typing is visible to anyone who can
//! read this thread's wake-up count, as it is for the evdev watcher (`keydev`'s note), and is
//! no more than the compositor's own input thread shows.
//!
//! Input: the bindings and an [`EventSource`] (`winit`'s, on Windows; a scripted one in the
//! tests, since the runner has no keyboard to press; and [`PipeSource`], the test seam of
//! #1791, which reads fake transitions from a named pipe and whose gate its own doc comment
//! sets out). Output: [`Input::Key`] and
//! [`Input::KeyDevice`] to the session loop, which owns every decision. Owns: two threads, the
//! input thread's (the event loop, which `winit` requires to be one thread, and the [`Binder`])
//! and a small monitor, and the [`Heartbeat`] between them.
//!
//! The fail-closed rules of design section 4.4, and where each is kept:
//!
//! - **Repeats.** A press while the key is down is nothing ([`Binder`]: a slot is `down`).
//! - **Unknown initial state.** Raw Input cannot be asked whether a key is down, so a binding
//!   starts unarmed and is armed by a release it has seen: a key held when the helper starts
//!   transmits nothing until it has been let go of once ([`Binder`]: `armed`). **The cost is
//!   that the first press after every start, device removal or wake gap only arms**: a tap is
//!   swallowed, and then the keys work. At the start nothing in the existing state vocabulary
//!   can say so without holding the session back until a key is tapped, so it is documented
//!   (design section 4.4, the manual). After a removal or a gap the keys are reported lost
//!   (`ptt: lost`) and are reported ready again by the first release of the talk binding (the
//!   first binding bound, if there is no talk key), so the state says what is true.
//! - **`max_transmit_secs`.** The push-to-talk machine shuts the gate on a held key and keeps
//!   holding it; the repeats that go on arriving change nothing here, so only a release and a
//!   press open it again (tested with the machine itself).
//! - **A stalled input thread.** The input thread beats a [`Heartbeat`] whenever it is woken
//!   (and `winit` is told to wake it every few hundred milliseconds); the monitor thread
//!   reports the keys lost, with `read_failed`, when a key is down and the beat is more than
//!   one `VoiceControlOwnerPollMS` old; the stall is re-checked under the reporter's lock
//!   ([`Reporter::not_ready_if`]), so a recovery cannot be followed by a stale loss. When the
//!   thread comes back it releases whatever it thought was down, forgets which keys were armed
//!   and reports the keys lost until the talk key has been released once.
//! - **Device removal.** `WM_INPUT_DEVICE_CHANGE`'s removal (`DeviceEvent::Removed`) while a key
//!   is down is a release, and the keys are lost until the talk key has been released once
//!   (an arrival alone does not arm it). `winit` does not say which device it was, so a removal is taken as the
//!   keyboard (or the mouse) being gone, which is the side that fails closed.
//! - **Focus.** A change of focus is not an input here: [`RawEvent::FocusChanged`] does nothing.
//! - **The session ends.** Every event, a tick included, ends the input thread once the session
//!   loop has gone ([`Pump::handle`]).
//!
//! **Injected input.** Raw Input delivers events that a program running as the same user sends
//! with `SendInput`, as it does those of a keyboard; this module cannot tell them apart (no
//! device is named in what `winit` hands over). Another program of the user's can therefore
//! press the bound key (design section 5).
//!
//! **One event loop per process.** `winit` allows one event loop in a process, so
//! [`spawn`] is to be called once, which it is: from `join` or `managed`, once each.

use std::fmt;
use std::io;
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use conch_voice_control::{Key, KeyAction, KeyBindings, KeyEvent, WindowsBinding};
use tokio::sync::mpsc;

use crate::keydev::{DeviceState, OsError, Problem};
use crate::session::Input;

/// What the input thread is told, in the shape of Raw Input and nothing more. It is private
/// to this module's threads: a scan code is in it, so it is not formatted, and its `Debug`
/// says only which kind it is.
#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) enum RawEvent {
    /// A key went down (`RI_KEY_MAKE`, which repeats while it is held) or up
    /// (`RI_KEY_BREAK`), by scan code with the `E0` prefix in the high byte.
    Key {
        /// The scan code.
        scan: u16,
        /// Down or up.
        down: bool,
    },
    /// A mouse button went down or up, by the number people give it: 4 or 5 for the side
    /// buttons.
    Button {
        /// The button's number.
        button: u8,
        /// Down or up.
        down: bool,
    },
    /// A device arrived (`GIDC_ARRIVAL`).
    DeviceAdded,
    /// A device was removed (`GIDC_REMOVAL`).
    DeviceRemoved,
    /// The window's focus changed. Nothing is done about it.
    FocusChanged,
    /// The event loop woke with nothing to say: a beat of the heartbeat.
    Tick,
}

impl fmt::Debug for RawEvent {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            RawEvent::Key { .. } => "RawEvent::Key",
            RawEvent::Button { .. } => "RawEvent::Button",
            RawEvent::DeviceAdded => "RawEvent::DeviceAdded",
            RawEvent::DeviceRemoved => "RawEvent::DeviceRemoved",
            RawEvent::FocusChanged => "RawEvent::FocusChanged",
            RawEvent::Tick => "RawEvent::Tick",
        })
    }
}

/// What the [`Binder`] sends on: a press or release of a configured key, or a change in
/// whether the keys are being read. The only things that leave this module.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Out {
    /// The talk, mute or deafen key went down or came up.
    Key(KeyEvent),
    /// The keys are being read again.
    Ready,
    /// The keys are not being read, and why.
    Lost(Problem),
}

/// One binding: its key, what it is, and what is known of it.
#[derive(Clone, Copy)]
struct Slot {
    key: Key,
    binding: WindowsBinding,
    /// A release of it has been seen, so a press that follows is a press and not a key that
    /// was already down.
    armed: bool,
    /// It is down, as far as this module has been told.
    down: bool,
}

/// Reduces Raw Input's events to presses and releases of the configured keys, and keeps the
/// rules of the design's section 4.4 that need no clock. It holds the bindings, which of them
/// are armed and which are down, and whether the keys are lost; it holds no event.
pub(crate) struct Binder {
    /// Talk, mute, deafen, in that order, for the ones bound. The first is the one whose
    /// release reports the keys read again.
    slots: Vec<Slot>,
    lost: bool,
}

/// Shows that there is a binder and nothing of what it is bound to or has seen.
impl fmt::Debug for Binder {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Binder").finish_non_exhaustive()
    }
}

impl Binder {
    /// A binder for the bindings that are in the Windows grammar. A binding that is not (an
    /// evdev one) is left out: [`conch_voice_control::require_windows_keys`] refuses a
    /// configuration with one before it gets here.
    #[must_use]
    pub(crate) fn new(bindings: &KeyBindings) -> Self {
        let slots = [
            (Key::Talk, bindings.talk()),
            (Key::Mute, bindings.mute()),
            (Key::Deafen, bindings.deafen()),
        ]
        .into_iter()
        .filter_map(|(key, code)| {
            Some(Slot {
                key,
                binding: code?.windows_binding()?,
                armed: false,
                down: false,
            })
        })
        .collect();
        Self { slots, lost: false }
    }

    /// True while a configured key is down.
    #[must_use]
    pub(crate) fn any_down(&self) -> bool {
        self.slots.iter().any(|slot| slot.down)
    }

    /// The keys are not being read, for `problem`: whatever is down is released, nothing is
    /// armed, and they stay lost until the talk key (the first binding, without one) has been
    /// released once. Returns the releases, and the loss if it was not already one.
    pub(crate) fn mark_lost(&mut self, problem: Problem) -> Vec<Out> {
        let mut out = self.forget();
        if !self.lost {
            self.lost = true;
            out.push(Out::Lost(problem));
        }
        out
    }

    /// Releases whatever is down and forgets which keys were armed: the state of every key is
    /// unknown, as it is at the start. Returns the releases.
    pub(crate) fn forget(&mut self) -> Vec<Out> {
        let mut out = Vec::new();
        for slot in &mut self.slots {
            if slot.down {
                out.push(Out::Key(KeyEvent {
                    key: slot.key,
                    action: KeyAction::Release,
                }));
            }
            slot.down = false;
            slot.armed = false;
        }
        out
    }

    /// Takes one event and returns what leaves the module because of it, in order.
    pub(crate) fn on_event(&mut self, event: RawEvent) -> Vec<Out> {
        let (what, down) = match event {
            RawEvent::Key { scan, down } => (WindowsBinding::Scan(scan), down),
            RawEvent::Button { button, down } => (WindowsBinding::Mouse(button), down),
            // A key that was down on a device that is gone will never be released, so it is
            // released now; and nothing is armed again until a release is seen.
            RawEvent::DeviceRemoved => return self.mark_lost(Problem::Ended),
            // Neither an arrival nor anything else here says that a key is up: the keys are
            // read again by the talk key's release, which arms it.
            RawEvent::DeviceAdded | RawEvent::FocusChanged | RawEvent::Tick => {
                return Vec::new();
            }
        };
        let Some(index) = self.slots.iter().position(|slot| slot.binding == what) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        if self.lost && !down && index == 0 {
            // The key that has to be armed was let go of: it is armed now, and read again.
            self.lost = false;
            out.push(Out::Ready);
        }
        let Some(slot) = self.slots.get_mut(index) else {
            return out;
        };
        if down {
            // A press is a press only of a key known to have been up, and only once: a
            // repeat, or a key held since before anything was read, is nothing.
            if slot.armed && !slot.down {
                slot.down = true;
                out.push(Out::Key(KeyEvent {
                    key: slot.key,
                    action: KeyAction::Press,
                }));
            }
        } else {
            slot.armed = true;
            if slot.down {
                slot.down = false;
                out.push(Out::Key(KeyEvent {
                    key: slot.key,
                    action: KeyAction::Release,
                }));
            }
        }
        out
    }
}

/// The input thread's sign of life, read by the monitor. Times are those of the owner of the
/// heartbeat's clock, as durations from its start.
#[derive(Default)]
pub(crate) struct Heartbeat {
    last_ms: AtomicU64,
    held: AtomicBool,
}

/// Shows that there is a heartbeat and nothing of whether a key is down.
impl fmt::Debug for Heartbeat {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Heartbeat").finish_non_exhaustive()
    }
}

/// A duration in whole milliseconds, saturating.
fn millis(at: Duration) -> u64 {
    u64::try_from(at.as_millis()).unwrap_or(u64::MAX)
}

impl Heartbeat {
    /// The input thread was woken at `at`. Returns how long it had been since the last time.
    pub(crate) fn beat(&self, at: Duration) -> Duration {
        let before = self.last_ms.swap(millis(at), Ordering::SeqCst);
        Duration::from_millis(millis(at).saturating_sub(before))
    }

    /// Whether a configured key is down, as the input thread last saw.
    pub(crate) fn set_held(&self, held: bool) {
        self.held.store(held, Ordering::SeqCst);
    }

    /// True if a key is down and the last beat is more than `limit` before `at`.
    #[must_use]
    pub(crate) fn stalled(&self, at: Duration, limit: Duration) -> bool {
        self.held.load(Ordering::SeqCst)
            && millis(at).saturating_sub(self.last_ms.load(Ordering::SeqCst)) > millis(limit)
    }
}

/// What the session loop was last told about the keys, so that it is told once.
#[derive(Debug, Default)]
struct Told {
    /// Whether the keys have ever been reported as read, so that a later loss is a loss and
    /// not an absence (`keydev`'s distinction).
    was_ready: bool,
    /// Whether what was last said was that they are not being read.
    not_ready: bool,
    /// Whether anything was said.
    said: bool,
}

/// Tells the session loop whether the keys are being read, from either thread, once for each
/// change.
#[derive(Debug)]
pub(crate) struct Reporter {
    inputs: mpsc::UnboundedSender<Input>,
    told: Mutex<Told>,
}

impl Reporter {
    /// A reporter into the session loop's inputs.
    #[must_use]
    pub(crate) fn new(inputs: mpsc::UnboundedSender<Input>) -> Self {
        Self {
            inputs,
            told: Mutex::new(Told::default()),
        }
    }

    /// True once the session loop is gone, so nobody is reading keys for it.
    #[must_use]
    pub(crate) fn closed(&self) -> bool {
        self.inputs.is_closed()
    }

    /// Passes on a key. False if the session loop has gone away.
    pub(crate) fn key(&self, event: KeyEvent) -> bool {
        self.inputs.send(Input::Key(event)).is_ok()
    }

    /// Says the keys are being read, unless that is what was last said. False if the session
    /// loop has gone away.
    pub(crate) fn ready(&self) -> bool {
        let mut told = self.told.lock().unwrap_or_else(PoisonError::into_inner);
        if told.said && !told.not_ready {
            return !self.closed();
        }
        *told = Told {
            was_ready: true,
            not_ready: false,
            said: true,
        };
        self.inputs
            .send(Input::KeyDevice(DeviceState::Ready))
            .is_ok()
    }

    /// Says the keys are not being read, unless that is what was last said (a reason that
    /// changes while it goes on is not said again). It is a loss if they had been read and an
    /// absence if not. False if the session loop has gone away.
    pub(crate) fn not_ready(&self, problem: Problem) -> bool {
        self.not_ready_if(problem, || true)
    }

    /// As [`Reporter::not_ready`], if `still` is true, which is asked **under the lock** that
    /// every report is made under. The monitor asks whether the input thread is stalled this
    /// way: it cannot decide that, be overtaken by the thread's recovery (and its report that
    /// the keys are read again), and then report a loss that nothing will ever clear.
    pub(crate) fn not_ready_if(&self, problem: Problem, still: impl FnOnce() -> bool) -> bool {
        let mut told = self.told.lock().unwrap_or_else(PoisonError::into_inner);
        if !still() || (told.said && told.not_ready) {
            return !self.closed();
        }
        let state = if told.was_ready {
            DeviceState::Lost(problem)
        } else {
            DeviceState::Missing(problem)
        };
        told.not_ready = true;
        told.said = true;
        self.inputs.send(Input::KeyDevice(state)).is_ok()
    }
}

/// How the input thread answers its event source: told that the source has registered, and
/// handed each event.
pub(crate) trait Sink {
    /// The source is registered and will deliver events.
    fn started(&mut self);
    /// One event. [`Flow::Stop`] asks the source to end.
    fn event(&mut self, event: RawEvent) -> Flow;
}

/// Whether the source goes on.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Flow {
    /// Go on.
    Continue,
    /// End: the session loop has gone away.
    Stop,
}

/// Where the raw events come from: `winit`'s event loop on Windows, a scripted list in the
/// tests. It runs on the input thread, to its end.
pub(crate) trait EventSource: Send + 'static {
    /// Registers, calls [`Sink::started`], and then delivers events to `sink` until it says
    /// [`Flow::Stop`] or the source ends. A [`RawEvent::Tick`] is delivered at least every
    /// `tick` while nothing else happens.
    ///
    /// # Errors
    ///
    /// What the operating system said, if the source could not be set up or failed.
    fn run(self, tick: Duration, sink: &mut dyn Sink) -> Result<(), OsError>;
}

/// The input thread's half: the [`Binder`], the heartbeat and the way out.
pub(crate) struct Pump {
    binder: Binder,
    beat: Arc<Heartbeat>,
    reporter: Arc<Reporter>,
    /// The longest gap between wake-ups that is not a stall.
    limit: Duration,
    clock: Instant,
    started: bool,
}

impl Pump {
    /// A pump for `bindings`, beating `beat` and reporting to `reporter`. `limit` is the
    /// longest the input thread may go without waking while a key is down.
    #[must_use]
    pub(crate) fn new(
        bindings: &KeyBindings,
        beat: Arc<Heartbeat>,
        reporter: Arc<Reporter>,
        limit: Duration,
    ) -> Self {
        Self {
            binder: Binder::new(bindings),
            beat,
            reporter,
            limit,
            clock: Instant::now(),
            started: false,
        }
    }

    /// Whether the source ever said it had started.
    #[must_use]
    pub(crate) fn started(&self) -> bool {
        self.started
    }

    /// One event, at `at` (from the clock the heartbeat is read against).
    pub(crate) fn handle(&mut self, at: Duration, event: RawEvent) -> Flow {
        let gap = self.beat.beat(at);
        let mut alive = true;
        let mut outs = Vec::new();
        if gap > self.limit {
            // The thread was not running for longer than a stall: any key it thought was
            // down may have come up meanwhile, so it is released and nothing is armed. The
            // keys are lost (said once, as the monitor's report is) until the talk key has
            // been released.
            outs.extend(self.binder.mark_lost(stalled_problem()));
        }
        outs.extend(self.binder.on_event(event));
        for out in outs {
            alive &= match out {
                Out::Key(key) => self.reporter.key(key),
                Out::Ready => self.reporter.ready(),
                Out::Lost(problem) => self.reporter.not_ready(problem),
            };
        }
        self.beat.set_held(self.binder.any_down());
        // A tick or an unbound key still ends the thread once the session has gone: nothing
        // else would, and winit allows one event loop in the process.
        if alive && !self.reporter.closed() {
            Flow::Continue
        } else {
            Flow::Stop
        }
    }
}

impl Sink for Pump {
    fn started(&mut self) {
        self.started = true;
        self.beat.beat(self.clock.elapsed());
        self.reporter.ready();
    }

    fn event(&mut self, event: RawEvent) -> Flow {
        self.handle(self.clock.elapsed(), event)
    }
}

/// What the monitor tells the session loop when the input thread has stopped waking: the
/// keys cannot be read. The reason is the existing one, `read_failed`.
fn stalled_problem() -> Problem {
    Problem::ReadFailed(OsError::from(&io::Error::from(io::ErrorKind::TimedOut)))
}

/// Watches the heartbeat from a thread of its own, every quarter of `limit`, until the
/// session loop has gone.
fn spawn_monitor(
    beat: Arc<Heartbeat>,
    reporter: Arc<Reporter>,
    clock: Instant,
    limit: Duration,
) -> io::Result<()> {
    std::thread::Builder::new()
        .name("conch-voice-keys-monitor".to_owned())
        .spawn(move || {
            while !reporter.closed() {
                std::thread::sleep(limit / 4);
                if beat.stalled(clock.elapsed(), limit) {
                    // Asked again under the reporter's lock: the thread may have recovered.
                    reporter
                        .not_ready_if(stalled_problem(), || beat.stalled(clock.elapsed(), limit));
                }
            }
        })
        .map(|_| ())
}

/// Starts reading `source` for `bindings`, on a thread of its own, for as long as the session
/// loop takes its inputs. `limit` is how long the input thread may go without waking while a
/// key is down: `VoiceControlOwnerPollMS` outside the tests.
///
/// # Errors
///
/// What the operating system said, if a thread could not be started.
pub(crate) fn spawn_with(
    source: impl EventSource,
    bindings: &KeyBindings,
    inputs: mpsc::UnboundedSender<Input>,
    limit: Duration,
) -> io::Result<()> {
    let reporter = Arc::new(Reporter::new(inputs));
    let beat = Arc::new(Heartbeat::default());
    let mut pump = Pump::new(bindings, Arc::clone(&beat), Arc::clone(&reporter), limit);
    spawn_monitor(Arc::clone(&beat), Arc::clone(&reporter), pump.clock, limit)?;
    std::thread::Builder::new()
        .name("conch-voice-keys".to_owned())
        .spawn(move || {
            // Wakes at least four times in a stall limit, so that a beat is never late
            // without something being wrong.
            let result = source.run(limit / 4, &mut pump);
            // It ends when the session loop does, silently; any other end is the keys being
            // gone, which the loop is told so that the gate shuts as for a release.
            if !reporter.closed() {
                let problem = match result {
                    Err(error) if !pump.started() => Problem::CannotOpen(error),
                    Err(error) => Problem::ReadFailed(error),
                    Ok(()) => Problem::Ended,
                };
                reporter.not_ready(problem);
            }
        })
        .map(|_| ())
}

/// The test seam's gate (#1791): what may be opened as the pipe the seam reads fake key
/// transitions from. A Windows named pipe, written `\\.\pipe\` and a name, and nothing else:
/// the name is letters, digits, `.`, `_` and `-` (not only dots), so that no `\`, `/`, drive
/// letter or `..` can lead out of the pipe namespace to a file, and a real keyboard is not
/// a pipe. Built on every platform, so that it is tested on every platform.
#[must_use]
pub(crate) fn is_test_pipe(path: &Path) -> bool {
    let Some(name) = path
        .to_str()
        .and_then(|text| text.strip_prefix(TEST_PIPE_PREFIX))
    else {
        return false;
    };
    !name.is_empty()
        && name.len() <= TEST_PIPE_NAME_MAX
        && name.bytes().any(|byte| byte != b'.')
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
}

/// Where every pipe the test seam reads is, in the kernel's namespace of named pipes.
const TEST_PIPE_PREFIX: &str = r"\\.\pipe\";
/// The longest name of such a pipe the seam takes; Windows allows 256.
const TEST_PIPE_NAME_MAX: usize = 200;
/// The size of one record on the test pipe.
pub(crate) const TEST_RECORD_LEN: usize = 4;
/// The kinds of record on the test pipe: its first byte.
const TEST_KEY: u8 = 1;
const TEST_BUTTON: u8 = 2;
const TEST_DEVICE_ADDED: u8 = 3;
const TEST_DEVICE_REMOVED: u8 = 4;
const TEST_FOCUS: u8 = 5;

/// One record of the test pipe as the [`RawEvent`] it stands for. A record is four bytes: the
/// kind, a little-endian `u16` (the scan code of a key, with the `E0` prefix in the high byte,
/// or the number of a mouse button, 4 or 5; zero for the other kinds) and `1` for down or `0`
/// for up. Anything else (an unknown kind, a mouse button that is not a side button) is a
/// [`RawEvent::Tick`]: it says only that the source is alive, as an event `winit` drops does.
#[must_use]
pub(crate) fn decode_test_record(record: [u8; TEST_RECORD_LEN]) -> RawEvent {
    let [kind, low, high, down] = record;
    let value = u16::from_le_bytes([low, high]);
    let down = down != 0;
    match kind {
        TEST_KEY => RawEvent::Key { scan: value, down },
        TEST_BUTTON => match u8::try_from(value) {
            Ok(button @ (4 | 5)) => RawEvent::Button { button, down },
            _ => RawEvent::Tick,
        },
        TEST_DEVICE_ADDED => RawEvent::DeviceAdded,
        TEST_DEVICE_REMOVED => RawEvent::DeviceRemoved,
        TEST_FOCUS => RawEvent::FocusChanged,
        _ => RawEvent::Tick,
    }
}

/// How the test seam opens its pipe. Each call is one attempt.
type OpenPipe = Box<dyn FnMut() -> io::Result<Box<dyn io::Read + Send>> + Send>;

/// **The test seam (#1791): an [`EventSource`] that reads [`RawEvent`]s from a named pipe
/// instead of from Raw Input**, so that a test of the whole helper can press keys on Windows
/// as it writes `input_event` records to a `mkfifo` on Linux.
///
/// **How it is kept out of a normal run**, as the evdev watcher's `--test-key-device` is
/// (`keydev`'s `DeviceRule::NamedPipeForTests`), and no tighter: the only way to it is that
/// hidden option of `managed` (`--test-key-device`, not in `--help`), which `lib.rs` turns into
/// `KeySource::NamedPipe`, which `managed::key_source` turns into
/// `DeviceRule::NamedPipeForTests`, which `keydev::spawn` answers with [`spawn_pipe`]. Nothing
/// else names that rule: neither `join`, nor `keys`, nor `devices`, nor `voice.toml` (which has
/// nowhere to hold a pipe), nor the environment. Without the option the helper reads Raw
/// Input and nothing else ([`spawn`]). It is not left out of release builds by a `cfg`, as
/// the Linux option is not; what bounds it is [`is_test_pipe`]: a name in the kernel's pipe
/// namespace and no path, so no file, no device and no keyboard can be read through it,
/// and the records it reads are four-byte [`decode_test_record`]s, which the same user's other
/// programs could as easily send to the helper's real input with `SendInput` (design section 5).
/// Like the attach pipe, a pipe of the same user's is whatever process created it: that is not
/// authentication either.
///
/// Until the pipe can be opened (nobody has created it yet) it waits, and ticks, as the Linux
/// watcher waits on a fifo; once it is, the keys are ready. A pipe that ends is a removal of
/// the device, and opening it again is its arrival.
pub(crate) struct PipeSource {
    open: OpenPipe,
    /// How long to wait between attempts to open the pipe.
    wait: Duration,
}

/// Shows that there is a source and nothing of what it reads.
impl fmt::Debug for PipeSource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PipeSource").finish_non_exhaustive()
    }
}

impl PipeSource {
    /// A source that opens its pipe with `open`, trying again every `wait`.
    #[must_use]
    pub(crate) fn new(open: OpenPipe, wait: Duration) -> Self {
        Self { open, wait }
    }

    /// A source of the pipe at `path`, which must pass [`is_test_pipe`].
    ///
    /// # Errors
    ///
    /// [`io::ErrorKind::InvalidInput`] for a path that is not a test pipe's. The path is not
    /// in the error.
    pub(crate) fn at(path: &Path) -> io::Result<Self> {
        if !is_test_pipe(path) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "the test key pipe is not a named pipe",
            ));
        }
        let path = path.to_path_buf();
        Ok(Self::new(
            Box::new(move || {
                let file = std::fs::OpenOptions::new().read(true).open(&path)?;
                Ok(Box::new(file) as Box<dyn io::Read + Send>)
            }),
            Duration::from_millis(5),
        ))
    }
}

impl EventSource for PipeSource {
    fn run(mut self, tick: Duration, sink: &mut dyn Sink) -> Result<(), OsError> {
        let mut started = false;
        loop {
            // Waits for the pipe, for as long as the session loop is there to want it.
            let mut pipe = loop {
                match (self.open)() {
                    Ok(pipe) => break pipe,
                    Err(_) => {
                        if sink.event(RawEvent::Tick) == Flow::Stop {
                            return Ok(());
                        }
                        std::thread::sleep(self.wait.min(tick));
                    }
                }
            };
            // The read blocks, so a thread of its own does it and the source keeps its beat.
            let (bytes, arrived) = std::sync::mpsc::channel::<Vec<u8>>();
            std::thread::Builder::new()
                .name("conch-voice-keys-pipe".to_owned())
                .spawn(move || {
                    let mut buffer = [0_u8; 64];
                    while let Ok(count) = io::Read::read(&mut pipe, &mut buffer) {
                        let Some(read) = buffer.get(..count).filter(|read| !read.is_empty()) else {
                            break;
                        };
                        if bytes.send(read.to_vec()).is_err() {
                            break;
                        }
                    }
                })
                .map_err(|error| OsError::from(&error))?;
            let flow = if started {
                sink.event(RawEvent::DeviceAdded)
            } else {
                started = true;
                sink.started();
                Flow::Continue
            };
            if flow == Flow::Stop {
                return Ok(());
            }
            let mut pending: Vec<u8> = Vec::new();
            loop {
                match arrived.recv_timeout(tick) {
                    Ok(more) => {
                        pending.extend(more);
                        let whole = pending.len() - pending.len() % TEST_RECORD_LEN;
                        let records: Vec<u8> = pending.drain(..whole).collect();
                        for record in records.chunks_exact(TEST_RECORD_LEN) {
                            if let Ok(record) = <[u8; TEST_RECORD_LEN]>::try_from(record)
                                && sink.event(decode_test_record(record)) == Flow::Stop
                            {
                                return Ok(());
                            }
                        }
                    }
                    Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                        if sink.event(RawEvent::Tick) == Flow::Stop {
                            return Ok(());
                        }
                    }
                    Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                        // The pipe ended: the device is gone, and the next open is it
                        // arriving.
                        if sink.event(RawEvent::DeviceRemoved) == Flow::Stop {
                            return Ok(());
                        }
                        break;
                    }
                }
            }
        }
    }
}

/// [`spawn_with`] on the test seam's pipe at `path` ([`PipeSource`]). Only
/// `keydev::spawn`, for `DeviceRule::NamedPipeForTests`, calls it.
///
/// # Errors
///
/// [`io::ErrorKind::InvalidInput`] if `path` is not a named pipe ([`is_test_pipe`]); otherwise
/// what the operating system said, if a thread could not be started.
#[cfg(windows)]
pub(crate) fn spawn_pipe(
    path: &Path,
    bindings: &KeyBindings,
    inputs: mpsc::UnboundedSender<Input>,
) -> io::Result<()> {
    spawn_with(
        PipeSource::at(path)?,
        bindings,
        inputs,
        Duration::from_millis(conch_voice_api::VOICE_CONTROL_OWNER_POLL_MS),
    )
}

/// [`spawn_with`] on Raw Input, through `winit`. **Once per process:** `winit` allows one event
/// loop in a process, and a second would fail to be created (the keys would then be reported
/// absent, as any start failure is).
///
/// # Errors
///
/// What the operating system said, if a thread could not be started.
#[cfg(windows)]
pub(crate) fn spawn(
    bindings: &KeyBindings,
    inputs: mpsc::UnboundedSender<Input>,
) -> io::Result<()> {
    spawn_with(
        raw_input::Winit,
        bindings,
        inputs,
        Duration::from_millis(conch_voice_api::VOICE_CONTROL_OWNER_POLL_MS),
    )
}

/// `winit`'s side: its event loop, on a thread that is not the main one, with device events
/// always on and no window of this program's.
#[cfg(windows)]
mod raw_input {
    use std::io;
    use std::time::{Duration, Instant};

    use winit::application::ApplicationHandler;
    use winit::event::{DeviceEvent, DeviceId, ElementState, RawKeyEvent, WindowEvent};
    use winit::event_loop::{ActiveEventLoop, ControlFlow, DeviceEvents, EventLoop};
    use winit::platform::scancode::PhysicalKeyExtScancode;
    use winit::platform::windows::EventLoopBuilderExtWindows;
    use winit::window::WindowId;

    use super::{EventSource, Flow, RawEvent, Sink};
    use crate::keydev::OsError;

    /// The index `winit` gives the mouse's fourth button, `XBUTTON1`; the fifth is the next.
    const FIRST_SIDE_BUTTON: u32 = 3;

    /// `winit`'s Windows backend as an [`EventSource`].
    pub(crate) struct Winit;

    /// An error of the event loop, as what the operating system said. `winit`'s own text is
    /// not kept: only that it failed.
    fn failed(_: impl std::error::Error) -> OsError {
        OsError::from(&io::Error::other("the event loop failed"))
    }

    impl EventSource for Winit {
        fn run(self, tick: Duration, sink: &mut dyn Sink) -> Result<(), OsError> {
            let mut builder = EventLoop::builder();
            // The helper's main thread is tokio's: the loop runs on this one.
            builder.with_any_thread(true);
            let event_loop = builder.build().map_err(failed)?;
            // Registers for the keyboard and the mouse with RIDEV_INPUTSINK | RIDEV_DEVNOTIFY,
            // so that events arrive whichever window has the focus (and there is none here).
            event_loop.listen_device_events(DeviceEvents::Always);
            event_loop.set_control_flow(ControlFlow::WaitUntil(Instant::now() + tick));
            sink.started();
            let mut app = App { sink, tick };
            event_loop.run_app(&mut app).map_err(failed)
        }
    }

    struct App<'a> {
        sink: &'a mut dyn Sink,
        tick: Duration,
    }

    impl App<'_> {
        fn forward(&mut self, event_loop: &ActiveEventLoop, event: RawEvent) {
            if self.sink.event(event) == Flow::Stop {
                event_loop.exit();
            }
        }
    }

    /// The raw event a device event is, if it is one of the three kinds that matter. A key is
    /// turned into its scan code (with the `E0` prefix in the high byte) here and nowhere
    /// else; a mouse button other than the two side buttons is dropped at once.
    fn translate(event: DeviceEvent) -> Option<RawEvent> {
        let down = |state: ElementState| state == ElementState::Pressed;
        match event {
            DeviceEvent::Key(RawKeyEvent {
                physical_key,
                state,
            }) => {
                let scan = u16::try_from(physical_key.to_scancode()?).ok()?;
                Some(RawEvent::Key {
                    scan,
                    down: down(state),
                })
            }
            DeviceEvent::Button { button, state } => {
                let number = button.checked_sub(FIRST_SIDE_BUTTON)?;
                (number < 2).then(|| RawEvent::Button {
                    button: if number == 0 { 4 } else { 5 },
                    down: down(state),
                })
            }
            DeviceEvent::Added => Some(RawEvent::DeviceAdded),
            DeviceEvent::Removed => Some(RawEvent::DeviceRemoved),
            _ => None,
        }
    }

    impl ApplicationHandler for App<'_> {
        fn resumed(&mut self, _: &ActiveEventLoop) {}

        fn window_event(&mut self, event_loop: &ActiveEventLoop, _: WindowId, event: WindowEvent) {
            // Every callback is a sign of life, whatever it says.
            let raw = if matches!(event, WindowEvent::Focused(_)) {
                RawEvent::FocusChanged
            } else {
                RawEvent::Tick
            };
            self.forward(event_loop, raw);
        }

        fn device_event(&mut self, event_loop: &ActiveEventLoop, _: DeviceId, event: DeviceEvent) {
            // An event that is not one of the kinds that matter (a mouse's movement, say) is
            // dropped here, but still beats the heartbeat.
            let raw = translate(event).unwrap_or(RawEvent::Tick);
            self.forward(event_loop, raw);
        }

        fn about_to_wait(&mut self, event_loop: &ActiveEventLoop) {
            self.forward(event_loop, RawEvent::Tick);
            event_loop.set_control_flow(ControlFlow::WaitUntil(Instant::now() + self.tick));
        }
    }
}

#[cfg(test)]
mod tests {
    use conch_voice_control::{GateCommand, Ptt, PttInput, PttOutput, ShutReason};

    use super::*;

    /// A binding table for the tests: talk is the grave key, mute is the right control key
    /// (with its `E0` prefix) and deafen is Mouse 5. Written in the grammar, which is
    /// parsed on every platform.
    fn bindings() -> KeyBindings {
        let key = |text| conch_voice_control::parse_windows_binding(text).unwrap();
        KeyBindings::new(
            Some(key("sc:0x29")),
            Some(key("sc:0xE0,0x1D")),
            Some(key("mouse:5")),
        )
        .unwrap()
    }

    const TALK: u16 = 0x29;
    const MUTE: u16 = 0xE01D;
    const OTHER: u16 = 0x1E;

    fn key(scan: u16, down: bool) -> RawEvent {
        RawEvent::Key { scan, down }
    }

    fn press(key: Key) -> Out {
        Out::Key(KeyEvent {
            key,
            action: KeyAction::Press,
        })
    }

    fn release(key: Key) -> Out {
        Out::Key(KeyEvent {
            key,
            action: KeyAction::Release,
        })
    }

    /// Runs events through a binder and collects what leaves.
    fn run(binder: &mut Binder, events: &[RawEvent]) -> Vec<Out> {
        events
            .iter()
            .flat_map(|&event| binder.on_event(event))
            .collect()
    }

    #[test]
    fn a_binder_turns_the_configured_transitions_into_presses_and_releases_and_nothing_else() {
        // Each case starts on a fresh binder that has seen every key come up once (so they
        // are armed), then takes the events.
        let armed = [
            key(TALK, false),
            key(MUTE, false),
            RawEvent::Button {
                button: 5,
                down: false,
            },
        ];
        let cases: [(&str, Vec<RawEvent>, Vec<Out>); 8] = [
            (
                "talk down and up",
                vec![key(TALK, true), key(TALK, false)],
                vec![press(Key::Talk), release(Key::Talk)],
            ),
            (
                "an extended key is its own binding",
                vec![key(MUTE, true), key(MUTE, false)],
                vec![press(Key::Mute), release(Key::Mute)],
            ),
            (
                "the same scan code without the prefix is another key",
                vec![key(0x1D, true), key(0x1D, false)],
                vec![],
            ),
            (
                "a mouse side button",
                vec![
                    RawEvent::Button {
                        button: 5,
                        down: true,
                    },
                    RawEvent::Button {
                        button: 5,
                        down: false,
                    },
                ],
                vec![press(Key::Deafen), release(Key::Deafen)],
            ),
            (
                "Mouse 4 is not bound here",
                vec![RawEvent::Button {
                    button: 4,
                    down: true,
                }],
                vec![],
            ),
            (
                "a key that is not bound, down and up",
                vec![key(OTHER, true), key(OTHER, false)],
                vec![],
            ),
            (
                "a tick and a change of focus",
                vec![RawEvent::Tick, RawEvent::FocusChanged],
                vec![],
            ),
            (
                "a device that arrives while all is well",
                vec![RawEvent::DeviceAdded],
                vec![],
            ),
        ];
        for (name, events, want) in cases {
            let mut binder = Binder::new(&bindings());
            let setup = run(&mut binder, &armed);
            assert_eq!(setup, vec![], "{name}: arming says nothing");
            assert_eq!(run(&mut binder, &events), want, "{name}");
        }
    }

    #[test]
    fn a_press_while_the_key_is_down_is_nothing() {
        // The rule of repeats: Raw Input sends a make for every repeat of a held key.
        let mut binder = Binder::new(&bindings());
        let got = run(
            &mut binder,
            &[
                key(TALK, false),
                key(TALK, true),
                key(TALK, true),
                key(TALK, true),
                key(TALK, false),
            ],
        );
        assert_eq!(got, vec![press(Key::Talk), release(Key::Talk)]);
    }

    #[test]
    fn a_key_held_at_the_start_transmits_nothing_until_it_has_been_released_once() {
        let mut binder = Binder::new(&bindings());
        // Held when the helper starts: the first thing seen is a repeat of the make.
        let got = run(&mut binder, &[key(TALK, true), key(TALK, true)]);
        assert_eq!(got, vec![], "repeats of a key that was already down");
        assert!(!binder.any_down());
        // Let go: that arms it, and says nothing, since it was never a press.
        assert_eq!(run(&mut binder, &[key(TALK, false)]), vec![]);
        // Now a press is one.
        assert_eq!(
            run(&mut binder, &[key(TALK, true), key(TALK, false)]),
            vec![press(Key::Talk), release(Key::Talk)]
        );
        // And each binding is armed on its own: mute was never released.
        assert_eq!(run(&mut binder, &[key(MUTE, true)]), vec![]);
    }

    #[test]
    fn a_removal_while_a_key_is_down_is_a_release_and_the_keys_are_lost_until_a_device_is_back() {
        let mut binder = Binder::new(&bindings());
        run(&mut binder, &[key(TALK, false), key(TALK, true)]);
        assert!(binder.any_down());
        assert_eq!(
            binder.on_event(RawEvent::DeviceRemoved),
            vec![release(Key::Talk), Out::Lost(Problem::Ended)]
        );
        assert!(!binder.any_down());
        // Said once.
        assert_eq!(binder.on_event(RawEvent::DeviceRemoved), vec![]);
        assert!(binder.lost);
        // Nothing is armed: a key held on the device that stayed is not a press, and does
        // not say the keys are read again.
        assert_eq!(binder.on_event(key(TALK, true)), vec![]);
        assert!(!binder.any_down());
        // Neither does an arrival, nor the release of a binding that is not the talk key.
        assert_eq!(binder.on_event(RawEvent::DeviceAdded), vec![]);
        assert_eq!(binder.on_event(key(MUTE, false)), vec![]);
        assert!(binder.lost);
        // The talk key's release arms it and says the keys are read again; then a press is one.
        assert_eq!(binder.on_event(key(TALK, false)), vec![Out::Ready]);
        assert!(!binder.lost);
        assert_eq!(binder.on_event(key(TALK, true)), vec![press(Key::Talk)]);
    }

    #[test]
    fn without_a_talk_key_the_first_binding_is_the_one_whose_release_reads_the_keys_again() {
        let key_of = |text| conch_voice_control::parse_windows_binding(text).unwrap();
        let mut binder = Binder::new(
            &KeyBindings::new(None, Some(key_of("sc:0xE0,0x1D")), Some(key_of("mouse:5"))).unwrap(),
        );
        binder.on_event(RawEvent::DeviceRemoved);
        assert_eq!(
            binder.on_event(RawEvent::Button {
                button: 5,
                down: false
            }),
            vec![]
        );
        assert_eq!(binder.on_event(key(MUTE, false)), vec![Out::Ready]);
    }

    #[test]
    fn a_removal_with_nothing_down_still_says_the_keys_are_lost() {
        let mut binder = Binder::new(&bindings());
        assert_eq!(
            binder.on_event(RawEvent::DeviceRemoved),
            vec![Out::Lost(Problem::Ended)]
        );
    }

    #[test]
    fn focus_changes_alone_do_nothing() {
        let mut binder = Binder::new(&bindings());
        let got = run(
            &mut binder,
            &[
                key(TALK, false),
                key(TALK, true),
                RawEvent::FocusChanged,
                RawEvent::FocusChanged,
                RawEvent::Tick,
                key(TALK, true),
                RawEvent::FocusChanged,
            ],
        );
        assert_eq!(got, vec![press(Key::Talk)], "held across focus changes");
        assert!(binder.any_down());
        assert_eq!(binder.on_event(key(TALK, false)), vec![release(Key::Talk)]);
    }

    #[test]
    fn a_configuration_with_no_windows_binding_binds_nothing() {
        let evdev: conch_voice_control::KeyCode = "KEY_F13".parse().unwrap();
        let binder = Binder::new(&KeyBindings::new(Some(evdev), None, None).unwrap());
        assert!(binder.slots.is_empty());
    }

    // The machine, driven by a binder: the rule of `max_transmit_secs`.

    /// A machine that is connected, may publish and has a microphone, with a 30 s limit, as
    /// a desktop session has.
    fn live_machine() -> Ptt {
        let (mut ptt, _) = Ptt::new(Duration::from_secs(30));
        for input in [
            PttInput::Connected(true),
            PttInput::PublishGrant(true),
            PttInput::Microphone(true),
            PttInput::KeyDevice(true),
        ] {
            ptt.handle(Duration::ZERO, input);
        }
        ptt
    }

    fn opens(outputs: &[PttOutput]) -> bool {
        outputs
            .iter()
            .any(|out| matches!(out, PttOutput::Gate(GateCommand::Open)))
    }

    #[test]
    fn after_max_transmit_repeats_do_not_reopen_and_a_release_and_a_press_do() {
        let mut ptt = live_machine();
        let mut binder = Binder::new(&bindings());
        let mut at = Duration::ZERO;
        let feed = |ptt: &mut Ptt, binder: &mut Binder, at: Duration, event: RawEvent| {
            binder
                .on_event(event)
                .into_iter()
                .filter_map(|out| match out {
                    Out::Key(key) => Some(ptt.handle(at, PttInput::Key(key))),
                    Out::Ready | Out::Lost(_) => None,
                })
                .flatten()
                .collect::<Vec<_>>()
        };
        feed(&mut ptt, &mut binder, at, key(TALK, false));
        at += Duration::from_secs(1);
        let opened = feed(&mut ptt, &mut binder, at, key(TALK, true));
        assert!(opens(&opened), "the press opens the gate: {opened:?}");

        // Held past the limit: the machine shuts the gate, on whatever it is next told.
        at += Duration::from_secs(30);
        let tick = ptt.handle(at, PttInput::Tick);
        assert!(
            tick.iter().any(|out| matches!(
                out,
                PttOutput::Gate(GateCommand::ForceShut(ShutReason::MaxTransmit))
            )),
            "{tick:?}"
        );
        ptt.handle(at, PttInput::GateShut);

        // The key is still held and Windows goes on sending makes: nothing reaches the
        // machine, so nothing reopens.
        for _ in 0..5 {
            at += Duration::from_millis(33);
            assert_eq!(feed(&mut ptt, &mut binder, at, key(TALK, true)), vec![]);
        }
        assert!(!ptt.transmission_under_way());

        // A release and a press: a new transmission.
        at += Duration::from_millis(100);
        feed(&mut ptt, &mut binder, at, key(TALK, false));
        at += Duration::from_millis(100);
        let again = feed(&mut ptt, &mut binder, at, key(TALK, true));
        assert!(opens(&again), "{again:?}");
    }

    // The heartbeat, the reporter, the pump and the monitor.

    fn at(ms: u64) -> Duration {
        Duration::from_millis(ms)
    }

    #[test]
    fn the_heartbeat_is_stalled_only_when_a_key_is_down_and_the_beat_is_old() {
        let limit = at(250);
        for (name, held, beat_at, now, want) in [
            ("up, old beat", false, 0, 10_000, false),
            ("down, fresh beat", true, 1_000, 1_100, false),
            ("down, exactly the limit", true, 1_000, 1_250, false),
            ("down, beyond the limit", true, 1_000, 1_251, true),
            ("down, a long time", true, 0, 60_000, true),
        ] {
            let heartbeat = Heartbeat::default();
            heartbeat.beat(at(beat_at));
            heartbeat.set_held(held);
            assert_eq!(heartbeat.stalled(at(now), limit), want, "{name}");
        }
    }

    #[test]
    fn beating_says_how_long_it_had_been() {
        let heartbeat = Heartbeat::default();
        assert_eq!(heartbeat.beat(at(100)), at(100));
        assert_eq!(heartbeat.beat(at(150)), at(50));
        assert_eq!(heartbeat.beat(at(150)), at(0));
    }

    fn drain(received: &mut mpsc::UnboundedReceiver<Input>) -> Vec<Input> {
        let mut got = Vec::new();
        while let Ok(input) = received.try_recv() {
            got.push(input);
        }
        got
    }

    fn pump_with(
        limit: Duration,
    ) -> (
        Pump,
        Arc<Heartbeat>,
        Arc<Reporter>,
        mpsc::UnboundedReceiver<Input>,
    ) {
        let (inputs, received) = mpsc::unbounded_channel();
        let reporter = Arc::new(Reporter::new(inputs));
        let beat = Arc::new(Heartbeat::default());
        let pump = Pump::new(&bindings(), Arc::clone(&beat), Arc::clone(&reporter), limit);
        (pump, beat, reporter, received)
    }

    #[test]
    fn the_reporter_says_ready_and_lost_once_each_and_a_loss_only_after_a_ready() {
        let (inputs, mut received) = mpsc::unbounded_channel();
        let reporter = Reporter::new(inputs);
        // Never read: an absence, not a loss.
        assert!(reporter.not_ready(Problem::Ended));
        assert!(reporter.not_ready(stalled_problem()));
        assert_eq!(
            drain(&mut received),
            vec![Input::KeyDevice(DeviceState::Missing(Problem::Ended))]
        );
        assert!(reporter.ready());
        assert!(reporter.ready());
        assert!(reporter.not_ready(Problem::Ended));
        assert!(reporter.not_ready(Problem::Ended));
        assert_eq!(
            drain(&mut received),
            vec![
                Input::KeyDevice(DeviceState::Ready),
                Input::KeyDevice(DeviceState::Lost(Problem::Ended)),
            ]
        );
        drop(received);
        assert!(!reporter.ready(), "the loop has gone: nobody to tell");
        assert!(reporter.closed());
    }

    #[test]
    fn a_stalled_thread_with_a_key_down_is_read_failed_and_presses_are_refused_until_it_reports() {
        let limit = at(250);
        let (mut pump, beat, reporter, mut received) = pump_with(limit);
        pump.started = true;
        reporter.ready();
        pump.handle(at(0), key(TALK, false));
        pump.handle(at(10), key(TALK, true));
        assert_eq!(
            drain(&mut received),
            vec![
                Input::KeyDevice(DeviceState::Ready),
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Press
                }),
            ]
        );
        // The thread stops waking; the monitor's check at 400 ms says so.
        assert!(beat.stalled(at(400), limit));
        reporter.not_ready(stalled_problem());
        let lost = drain(&mut received);
        let [Input::KeyDevice(state)] = lost.as_slice() else {
            panic!("{lost:?}");
        };
        assert!(
            matches!(state, DeviceState::Lost(Problem::ReadFailed(_))),
            "{state:?}"
        );
        assert_eq!(
            state.problem().map(|problem| problem.code()),
            Some("read_failed")
        );
        assert!(state.presses_refused());

        // The machine shuts the gate on it: the same state, as the session loop passes it.
        let mut ptt = live_machine();
        ptt.handle(
            at(10),
            PttInput::Key(KeyEvent {
                key: Key::Talk,
                action: KeyAction::Press,
            }),
        );
        let shut = ptt.handle(at(400), PttInput::KeyDevice(state.is_ready()));
        assert!(
            shut.iter().any(|out| matches!(
                out,
                PttOutput::Gate(GateCommand::ForceShut(ShutReason::KeyDeviceLost))
            )),
            "{shut:?}"
        );

        // The thread wakes, 5 s later, with the key now up and then pressed again. It
        // released what it thought was down and forgot what was armed; the loss is already
        // said, so it is not said twice, and the keys are read again only when the talk key
        // has been released (which arms it), and only the press after that counts.
        pump.handle(at(5_000), RawEvent::Tick);
        let back = drain(&mut received);
        assert_eq!(
            back,
            vec![Input::Key(KeyEvent {
                key: Key::Talk,
                action: KeyAction::Release
            })]
        );
        pump.handle(at(5_010), key(TALK, true));
        assert_eq!(drain(&mut received), vec![], "armed by nothing yet");
        pump.handle(at(5_020), key(TALK, false));
        assert_eq!(
            drain(&mut received),
            vec![Input::KeyDevice(DeviceState::Ready)]
        );
        pump.handle(at(5_030), key(TALK, true));
        assert_eq!(
            drain(&mut received),
            vec![Input::Key(KeyEvent {
                key: Key::Talk,
                action: KeyAction::Press
            })]
        );
    }

    /// S1: the monitor decides the thread is stalled, the thread recovers (and arms, and says
    /// the keys are read again), and only then does the monitor report. The report is asked
    /// again under the reporter's lock and is not made, so nothing stale is left standing.
    #[test]
    fn a_loss_the_monitor_decided_on_before_a_recovery_is_not_reported_after_it() {
        let limit = at(250);
        let (mut pump, beat, reporter, mut received) = pump_with(limit);
        pump.started = true;
        reporter.ready();
        pump.handle(at(0), key(TALK, false));
        pump.handle(at(10), key(TALK, true));
        drain(&mut received);

        // The monitor's look, at 400 ms: stalled. It has not yet reported.
        assert!(beat.stalled(at(400), limit));

        // The thread comes back, is lost until the talk key is released, and that happens.
        pump.handle(at(5_000), RawEvent::Tick);
        pump.handle(at(5_010), key(TALK, false));
        assert_eq!(
            drain(&mut received)
                .iter()
                .filter(|input| matches!(input, Input::KeyDevice(_)))
                .count(),
            2,
            "lost, then ready"
        );

        // The monitor now reports what it decided, with the question put again under the lock.
        reporter.not_ready_if(stalled_problem(), || beat.stalled(at(5_020), limit));
        assert_eq!(drain(&mut received), vec![], "no loss after the recovery");
        // And a monitor that did not ask again would have left the keys lost for good.
        assert!(reporter.not_ready(stalled_problem()));
        assert_eq!(drain(&mut received).len(), 1);
    }

    /// S2: the input thread does not outlive the session, on a tick or an unbound key.
    #[test]
    fn once_the_session_has_gone_every_event_stops_the_input_thread() {
        for (name, event) in [
            ("a tick", RawEvent::Tick),
            ("an unbound key", key(OTHER, true)),
            ("a change of focus", RawEvent::FocusChanged),
            ("a bound key", key(TALK, false)),
        ] {
            let (mut pump, _beat, _reporter, received) = pump_with(at(250));
            assert_eq!(pump.handle(at(1), RawEvent::Tick), Flow::Continue, "{name}");
            drop(received);
            assert_eq!(pump.handle(at(2), event), Flow::Stop, "{name}");
        }
    }

    #[test]
    fn the_monitor_thread_reports_a_stall_without_the_input_thread() {
        let limit = at(40);
        let (_pump, beat, reporter, mut received) = pump_with(limit);
        reporter.ready();
        let clock = Instant::now();
        beat.beat(Duration::ZERO);
        beat.set_held(true);
        spawn_monitor(Arc::clone(&beat), Arc::clone(&reporter), clock, limit).unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut seen = Vec::new();
        while Instant::now() < deadline && seen.len() < 2 {
            std::thread::sleep(Duration::from_millis(10));
            seen.extend(drain(&mut received));
        }
        assert_eq!(seen.len(), 2, "{seen:?}");
        assert_eq!(seen[0], Input::KeyDevice(DeviceState::Ready));
        assert!(
            matches!(
                seen[1],
                Input::KeyDevice(DeviceState::Lost(Problem::ReadFailed(_)))
            ),
            "{seen:?}"
        );
    }

    /// A source that hands over a script of events and then ends, as a fake of what the
    /// runner cannot do: press keys.
    struct Script(Vec<RawEvent>);

    impl EventSource for Script {
        fn run(self, _: Duration, sink: &mut dyn Sink) -> Result<(), OsError> {
            sink.started();
            for event in self.0 {
                if sink.event(event) == Flow::Stop {
                    return Ok(());
                }
            }
            Ok(())
        }
    }

    /// A source that fails to set up.
    struct Refuses;

    impl EventSource for Refuses {
        fn run(self, _: Duration, _: &mut dyn Sink) -> Result<(), OsError> {
            Err(OsError::from(&io::Error::from(
                io::ErrorKind::PermissionDenied,
            )))
        }
    }

    fn wait_for(received: &mut mpsc::UnboundedReceiver<Input>, count: usize) -> Vec<Input> {
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut seen = Vec::new();
        while Instant::now() < deadline && seen.len() < count {
            std::thread::sleep(Duration::from_millis(5));
            seen.extend(drain(received));
        }
        seen
    }

    #[test]
    fn a_scripted_source_reaches_the_session_loop_as_the_evdev_watcher_does() {
        let (inputs, mut received) = mpsc::unbounded_channel();
        let script = Script(vec![
            key(OTHER, true),
            key(TALK, false),
            key(TALK, true),
            key(TALK, false),
        ]);
        spawn_with(script, &bindings(), inputs, Duration::from_secs(5)).unwrap();
        let seen = wait_for(&mut received, 4);
        assert_eq!(
            seen,
            vec![
                Input::KeyDevice(DeviceState::Ready),
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Press
                }),
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Release
                }),
                // The script ended: the keys are no longer read, which is a loss.
                Input::KeyDevice(DeviceState::Lost(Problem::Ended)),
            ]
        );
    }

    /// A record of the test pipe, as the rig writes it.
    fn record(kind: u8, value: u16, down: bool) -> [u8; TEST_RECORD_LEN] {
        let [low, high] = value.to_le_bytes();
        [kind, low, high, u8::from(down)]
    }

    #[test]
    fn the_test_pipe_is_a_name_in_the_pipe_namespace_and_no_path() {
        let accepted = [
            r"\\.\pipe\conch-voice-rig-keys-1234-0",
            r"\\.\pipe\a",
            r"\\.\pipe\a.b_c-D",
        ];
        for path in accepted {
            assert!(is_test_pipe(Path::new(path)), "{path}");
        }
        let refused = [
            // The things a keyboard, a file or another pipe namespace would be.
            "",
            "keys",
            "/dev/input/event3",
            "/tmp/pipe",
            r"C:\Windows\System32\config\SAM",
            r"\\.\C:\file",
            r"\\.\HID#VID_1234",
            r"\\.\PhysicalDrive0",
            r"\\?\C:\file",
            r"\\?\pipe\x",
            r"\\server\pipe\x",
            r"\pipe\x",
            // Nothing after the prefix, or something that leaves the namespace.
            r"\\.\pipe\",
            r"\\.\pipe\..",
            r"\\.\pipe\.",
            r"\\.\pipe\..\..\C:\file",
            r"\\.\pipe\a\b",
            r"\\.\pipe\a/b",
            r"\\.\pipe\a:b",
            "\\\\.\\pipe\\a\0b",
            r"\\.\pipe\a b",
        ];
        for path in refused {
            assert!(!is_test_pipe(Path::new(path)), "{path:?}");
        }
        assert!(!is_test_pipe(Path::new(&format!(
            "{TEST_PIPE_PREFIX}{}",
            "x".repeat(TEST_PIPE_NAME_MAX + 1)
        ))));
    }

    /// A normal run cannot use the seam: the source of a path that is not a pipe is not made,
    /// and what the error says does not hold the path.
    #[test]
    fn a_path_that_is_not_a_pipe_is_refused_without_being_named() {
        let error = PipeSource::at(Path::new("/dev/input/event3-FAKE")).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert!(!error.to_string().contains("FAKE"), "{error}");
        assert!(PipeSource::at(Path::new(r"\\.\pipe\fine")).is_ok());
    }

    #[test]
    fn a_record_is_one_event_and_what_is_not_a_record_is_a_tick() {
        let cases = [
            (record(1, TALK, true), key(TALK, true)),
            (record(1, MUTE, false), key(MUTE, false)),
            (
                record(2, 5, true),
                RawEvent::Button {
                    button: 5,
                    down: true,
                },
            ),
            (record(2, 3, true), RawEvent::Tick),
            (record(2, 6, false), RawEvent::Tick),
            (record(2, 0x0104, false), RawEvent::Tick),
            (record(3, 0, false), RawEvent::DeviceAdded),
            (record(4, 0, false), RawEvent::DeviceRemoved),
            (record(5, 0, true), RawEvent::FocusChanged),
            (record(0, 7, true), RawEvent::Tick),
            (record(200, 7, true), RawEvent::Tick),
        ];
        for (bytes, event) in cases {
            assert_eq!(decode_test_record(bytes), event, "{bytes:?}");
        }
    }

    /// A pipe in memory: what is sent to it is read, and dropping the sender ends it.
    struct Fed(std::sync::mpsc::Receiver<Vec<u8>>, Vec<u8>);

    impl io::Read for Fed {
        fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
            if self.1.is_empty() {
                match self.0.recv() {
                    Ok(more) => self.1 = more,
                    Err(_) => return Ok(0),
                }
            }
            let count = out.len().min(self.1.len());
            out[..count].copy_from_slice(&self.1[..count]);
            self.1.drain(..count);
            Ok(count)
        }
    }

    /// What the helper does with a keyboard that is plugged in, struck, unplugged and plugged
    /// in again, as the rig of `tests/managed.rs` does it on Windows: the source waits for
    /// the pipe, then the keys are ready; a record split in two is still one; the pipe ending
    /// is a loss, and the pipe back, with the talk key released, is ready again.
    #[test]
    fn a_pipe_that_is_plugged_struck_unplugged_and_plugged_again_is_read_as_a_keyboard() {
        let (first_in, first) = std::sync::mpsc::channel();
        let (second_in, second) = std::sync::mpsc::channel();
        let pipes = Arc::new(Mutex::new(vec![
            Fed(second, Vec::new()),
            Fed(first, Vec::new()),
        ]));
        let attempts = Arc::new(AtomicU64::new(0));
        let (opens, tries) = (Arc::clone(&pipes), Arc::clone(&attempts));
        let source = PipeSource::new(
            Box::new(move || {
                // The first two attempts find no pipe, as before the rig has made it.
                if tries.fetch_add(1, Ordering::SeqCst) < 2 {
                    return Err(io::Error::from(io::ErrorKind::NotFound));
                }
                let pipe = opens.lock().unwrap().pop().ok_or(io::ErrorKind::NotFound)?;
                Ok(Box::new(pipe) as Box<dyn io::Read + Send>)
            }),
            Duration::from_millis(1),
        );
        let (inputs, mut received) = mpsc::unbounded_channel();
        spawn_with(source, &bindings(), inputs, Duration::from_secs(5)).unwrap();

        let seen = wait_for(&mut received, 1);
        assert_eq!(seen, vec![Input::KeyDevice(DeviceState::Ready)]);
        // Arm by a release; then a press whose record arrives in two pieces; then the release.
        let arm = record(1, TALK, false);
        let down = record(1, TALK, true);
        first_in.send(arm.to_vec()).unwrap();
        first_in.send(down[..3].to_vec()).unwrap();
        first_in.send([&down[3..], &arm[..]].concat()).unwrap();
        let seen = wait_for(&mut received, 2);
        assert_eq!(
            seen,
            vec![
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Press
                }),
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Release
                }),
            ]
        );
        // Press and leave it down, and unplug: released, lost.
        first_in.send(down.to_vec()).unwrap();
        let seen = wait_for(&mut received, 1);
        assert_eq!(seen.len(), 1);
        drop(first_in);
        let seen = wait_for(&mut received, 2);
        assert_eq!(
            seen,
            vec![
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Release
                }),
                Input::KeyDevice(DeviceState::Lost(Problem::Ended)),
            ]
        );
        // Plugged in again: nothing until the talk key is released, and then ready.
        second_in.send(record(1, TALK, true).to_vec()).unwrap();
        second_in.send(arm.to_vec()).unwrap();
        let seen = wait_for(&mut received, 1);
        assert_eq!(seen, vec![Input::KeyDevice(DeviceState::Ready)]);
    }

    #[test]
    fn a_source_that_cannot_start_is_a_typed_absence() {
        let (inputs, mut received) = mpsc::unbounded_channel();
        spawn_with(Refuses, &bindings(), inputs, Duration::from_secs(5)).unwrap();
        let seen = wait_for(&mut received, 1);
        let [Input::KeyDevice(DeviceState::Missing(problem))] = seen.as_slice() else {
            panic!("{seen:?}");
        };
        assert_eq!(problem.code(), "cannot_open");
    }

    #[test]
    fn nothing_that_leaves_names_a_key_a_scan_code_or_a_count() {
        // Every Debug rendering that can reach output: the raw events, the binder and what is
        // sent on. A key bound to 0x7E, with a neighbour that is not, in every notation the
        // codes could be written in.
        let (mut pump, _beat, _reporter, mut received) = pump_with(at(250));
        let bound = conch_voice_control::parse_windows_binding("sc:0x7E").unwrap();
        pump.binder = Binder::new(&KeyBindings::new(Some(bound), None, None).unwrap());
        let mut shown = String::new();
        for event in [
            key(0x7E, false),
            key(0x7E, true),
            key(0x7D, true),
            RawEvent::Button {
                button: 4,
                down: true,
            },
            key(0x7E, false),
        ] {
            shown.push_str(&format!("{event:?}\n"));
            pump.handle(at(1), event);
        }
        shown.push_str(&format!("{:?}\n", pump.binder));
        for input in drain(&mut received) {
            shown.push_str(&format!("{input:?}\n"));
        }
        assert!(
            shown.contains("Press") && shown.contains("Release"),
            "{shown}"
        );
        for hidden in ["7E", "7e", "126", "7D", "7d", "125", "32894", "0x8000"] {
            assert!(!shown.contains(hidden), "{hidden} in {shown}");
        }
    }

    /// **E7 on Nick's Windows PC (issue #903); not run by CI, and not run when this was
    /// written.** Needs a Windows desktop session, a keyboard and a mouse with a Mouse 4 button,
    /// and a person: `cargo test -p conch-voice --lib -- --ignored --nocapture hold_a_key`.
    ///
    /// It binds talk to the grave key (`sc:0x29`) and mute to Mouse 4, starts the real Raw Input
    /// source and asks for two things. **Switch to another window, or a game, first: the point
    /// is that the helper hears them in the background.**
    ///
    /// 1. For 15 seconds, **tap** each of the grave key and Mouse 4 once, and do not hold them.
    ///    A binding is armed by a release it has seen, so a tap only arms: the test asserts
    ///    that nothing but the keys being ready came of it.
    /// 2. Then, within 60 seconds, **hold** each in turn, a second or two, and let go. Each is
    ///    exactly one press and one release (so that the repeats of a held key are nothing).
    ///
    /// It prints no key.
    #[cfg(windows)]
    #[test]
    #[ignore = "needs a Windows desktop, a keyboard and a mouse with Mouse 4, and a person pressing them in the background (E7, #903)"]
    fn hold_a_key_and_mouse_4_in_the_background() {
        let key = |text| conch_voice_control::parse_windows_binding(text).unwrap();
        let bindings = KeyBindings::new(Some(key("sc:0x29")), Some(key("mouse:4")), None).unwrap();
        let (inputs, mut received) = mpsc::unbounded_channel();
        spawn(&bindings, inputs).unwrap();

        eprintln!(
            "E7 step 1 (15 s): switch to another window, then TAP the grave key and Mouse 4 once each. Do not hold them."
        );
        let tapping = Instant::now() + Duration::from_secs(15);
        let mut seen = Vec::new();
        while Instant::now() < tapping {
            std::thread::sleep(Duration::from_millis(20));
            seen.extend(drain(&mut received));
        }
        assert_eq!(
            seen,
            vec![Input::KeyDevice(DeviceState::Ready)],
            "the first press of each only arms it: nothing but ready may come of the taps"
        );

        eprintln!(
            "E7 step 2 (60 s): now HOLD the grave key for a second or two and let go, then the same with Mouse 4."
        );
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut keys = Vec::new();
        while Instant::now() < deadline && keys.len() < 4 {
            std::thread::sleep(Duration::from_millis(20));
            keys.extend(
                drain(&mut received)
                    .into_iter()
                    .filter_map(|input| match input {
                        Input::Key(event) => Some((event.key, event.action)),
                        _ => None,
                    }),
            );
        }
        assert_eq!(
            keys,
            vec![
                (Key::Talk, KeyAction::Press),
                (Key::Talk, KeyAction::Release),
                (Key::Mute, KeyAction::Press),
                (Key::Mute, KeyAction::Release),
            ]
        );
    }
}
