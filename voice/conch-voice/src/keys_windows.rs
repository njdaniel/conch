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
//!   is no window of this program's, no hook and no polling of key state.
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
//! tests, since the runner has no keyboard to press). Output: [`Input::Key`] and
//! [`Input::KeyDevice`] to the session loop, which owns every decision. Owns: two threads, the
//! input thread's (the event loop, which `winit` requires to be one thread, and the [`Binder`])
//! and a small monitor, and the [`Heartbeat`] between them.
//!
//! The fail-closed rules of design section 4.4, and where each is kept:
//!
//! - **Repeats.** A press while the key is down is nothing ([`Binder`]: a slot is `down`).
//! - **Unknown initial state.** Raw Input cannot be asked whether a key is down, so a binding
//!   starts unarmed and is armed by a release it has seen: a key held when the helper starts
//!   transmits nothing until it has been let go of once ([`Binder`]: `armed`).
//! - **`max_transmit_secs`.** The push-to-talk machine shuts the gate on a held key and keeps
//!   holding it; the repeats that go on arriving change nothing here, so only a release and a
//!   press open it again (tested with the machine itself).
//! - **A stalled input thread.** The input thread beats a [`Heartbeat`] whenever it is woken
//!   (and `winit` is told to wake it every few hundred milliseconds); the monitor thread
//!   reports the keys lost, with `read_failed`, when a key is down and the beat is more than
//!   one `VoiceControlOwnerPollMS` old. When the thread comes back it releases whatever it
//!   thought was down and forgets which keys were armed.
//! - **Device removal.** `WM_INPUT_DEVICE_CHANGE`'s removal (`DeviceEvent::Removed`) while a key
//!   is down is a release, and the keys are lost until a device has arrived or a bound key has
//!   been seen again. `winit` does not say which device it was, so a removal is taken as the
//!   keyboard (or the mouse) being gone, which is the side that fails closed.
//! - **Focus.** A change of focus is not an input here: [`RawEvent::FocusChanged`] does nothing.

use std::fmt;
use std::io;
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
pub enum RawEvent {
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
pub enum Out {
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
pub struct Binder {
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
    pub fn new(bindings: &KeyBindings) -> Self {
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
    pub fn any_down(&self) -> bool {
        self.slots.iter().any(|slot| slot.down)
    }

    /// Releases whatever is down and forgets which keys were armed: the state of every key is
    /// unknown, as it is at the start. Returns the releases.
    pub fn forget(&mut self) -> Vec<Out> {
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

    fn slot_of(&mut self, what: WindowsBinding) -> Option<&mut Slot> {
        self.slots.iter_mut().find(|slot| slot.binding == what)
    }

    /// Takes one event and returns what leaves the module because of it, in order.
    pub fn on_event(&mut self, event: RawEvent) -> Vec<Out> {
        let (what, down) = match event {
            RawEvent::Key { scan, down } => (WindowsBinding::Scan(scan), down),
            RawEvent::Button { button, down } => (WindowsBinding::Mouse(button), down),
            RawEvent::DeviceRemoved => {
                // A key that was down on a device that is gone will never be released, so
                // it is released now; and nothing is armed again until a release is seen.
                let mut out = self.forget();
                if !self.lost {
                    self.lost = true;
                    out.push(Out::Lost(Problem::Ended));
                }
                return out;
            }
            RawEvent::DeviceAdded => {
                return if self.lost {
                    self.lost = false;
                    vec![Out::Ready]
                } else {
                    Vec::new()
                };
            }
            // Neither says anything about a key.
            RawEvent::FocusChanged | RawEvent::Tick => return Vec::new(),
        };
        let was_lost = self.lost;
        let Some(slot) = self.slot_of(what) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        if was_lost {
            // A configured key was seen, so a device is there.
            out.push(Out::Ready);
        }
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
        self.lost = false;
        out
    }
}

/// The input thread's sign of life, read by the monitor. Times are those of the owner of the
/// heartbeat's clock, as durations from its start.
#[derive(Debug, Default)]
pub struct Heartbeat {
    last_ms: AtomicU64,
    held: AtomicBool,
}

/// A duration in whole milliseconds, saturating.
fn millis(at: Duration) -> u64 {
    u64::try_from(at.as_millis()).unwrap_or(u64::MAX)
}

impl Heartbeat {
    /// The input thread was woken at `at`. Returns how long it had been since the last time.
    pub fn beat(&self, at: Duration) -> Duration {
        let before = self.last_ms.swap(millis(at), Ordering::SeqCst);
        Duration::from_millis(millis(at).saturating_sub(before))
    }

    /// Whether a configured key is down, as the input thread last saw.
    pub fn set_held(&self, held: bool) {
        self.held.store(held, Ordering::SeqCst);
    }

    /// True if a key is down and the last beat is more than `limit` before `at`.
    #[must_use]
    pub fn stalled(&self, at: Duration, limit: Duration) -> bool {
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
pub struct Reporter {
    inputs: mpsc::UnboundedSender<Input>,
    told: Mutex<Told>,
}

impl Reporter {
    /// A reporter into the session loop's inputs.
    #[must_use]
    pub fn new(inputs: mpsc::UnboundedSender<Input>) -> Self {
        Self {
            inputs,
            told: Mutex::new(Told::default()),
        }
    }

    /// True once the session loop is gone, so nobody is reading keys for it.
    #[must_use]
    pub fn closed(&self) -> bool {
        self.inputs.is_closed()
    }

    /// Passes on a key. False if the session loop has gone away.
    pub fn key(&self, event: KeyEvent) -> bool {
        self.inputs.send(Input::Key(event)).is_ok()
    }

    /// Says the keys are being read, unless that is what was last said. False if the session
    /// loop has gone away.
    pub fn ready(&self) -> bool {
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
    pub fn not_ready(&self, problem: Problem) -> bool {
        let mut told = self.told.lock().unwrap_or_else(PoisonError::into_inner);
        if told.said && told.not_ready {
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
pub trait Sink {
    /// The source is registered and will deliver events.
    fn started(&mut self);
    /// One event. [`Flow::Stop`] asks the source to end.
    fn event(&mut self, event: RawEvent) -> Flow;
}

/// Whether the source goes on.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Flow {
    /// Go on.
    Continue,
    /// End: the session loop has gone away.
    Stop,
}

/// Where the raw events come from: `winit`'s event loop on Windows, a scripted list in the
/// tests. It runs on the input thread, to its end.
pub trait EventSource: Send + 'static {
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
pub struct Pump {
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
    pub fn new(
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
    pub fn started(&self) -> bool {
        self.started
    }

    /// One event, at `at` (from the clock the heartbeat is read against).
    pub fn handle(&mut self, at: Duration, event: RawEvent) -> Flow {
        let gap = self.beat.beat(at);
        let mut alive = true;
        let mut outs = Vec::new();
        if gap > self.limit {
            // The thread was not running for longer than a stall: any key it thought was
            // down may have come up meanwhile, so it is released, nothing is armed, and the
            // keys are read again.
            outs.extend(self.binder.forget());
            outs.push(Out::Ready);
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
        if alive { Flow::Continue } else { Flow::Stop }
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
                    reporter.not_ready(stalled_problem());
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
pub fn spawn_with(
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

/// [`spawn_with`] on Raw Input, through `winit`.
///
/// # Errors
///
/// What the operating system said, if a thread could not be started.
#[cfg(windows)]
pub fn spawn(bindings: &KeyBindings, inputs: mpsc::UnboundedSender<Input>) -> io::Result<()> {
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
    pub struct Winit;

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
            if matches!(event, WindowEvent::Focused(_)) {
                self.forward(event_loop, RawEvent::FocusChanged);
            }
        }

        fn device_event(&mut self, event_loop: &ActiveEventLoop, _: DeviceId, event: DeviceEvent) {
            if let Some(raw) = translate(event) {
                self.forward(event_loop, raw);
            }
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
        // Nothing is armed: a key held on the device that stayed is not a press.
        assert_eq!(binder.on_event(key(TALK, true)), vec![Out::Ready]);
        // (The bound key was seen, so the device is there: ready, and still not a press.)
        assert!(!binder.any_down());

        // The other way back: a device arrives.
        let mut binder = Binder::new(&bindings());
        binder.on_event(RawEvent::DeviceRemoved);
        assert_eq!(binder.on_event(RawEvent::DeviceAdded), vec![Out::Ready]);
        assert_eq!(binder.on_event(RawEvent::DeviceAdded), vec![]);
        // Armed only by a release, after a removal as at the start.
        assert_eq!(run(&mut binder, &[key(TALK, true)]), vec![]);
        assert_eq!(
            run(&mut binder, &[key(TALK, false), key(TALK, true)]),
            vec![press(Key::Talk)]
        );
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
        // released what it thought was down and forgot what was armed, so the first thing
        // that follows is a release (arming it), and only the press after that counts.
        pump.handle(at(5_000), RawEvent::Tick);
        let back = drain(&mut received);
        assert_eq!(
            back,
            vec![
                Input::Key(KeyEvent {
                    key: Key::Talk,
                    action: KeyAction::Release
                }),
                Input::KeyDevice(DeviceState::Ready),
            ]
        );
        pump.handle(at(5_010), key(TALK, true));
        assert_eq!(drain(&mut received), vec![], "armed by nothing yet");
        pump.handle(at(5_020), key(TALK, false));
        pump.handle(at(5_030), key(TALK, true));
        assert_eq!(
            drain(&mut received),
            vec![Input::Key(KeyEvent {
                key: Key::Talk,
                action: KeyAction::Press
            })]
        );
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
    /// source, and waits 60 seconds for each of: the grave key held and let go, then Mouse 4
    /// held and let go. **Switch to another window, or a game, first: the point is that the
    /// helper hears them in the background.** It checks that each arrives as exactly one press
    /// and one release (so that repeats are nothing), and prints nothing of any key.
    #[cfg(windows)]
    #[test]
    #[ignore = "needs a Windows desktop, a keyboard and a mouse with Mouse 4, and a person pressing them in the background (E7, #903)"]
    fn hold_a_key_and_mouse_4_in_the_background() {
        let key = |text| conch_voice_control::parse_windows_binding(text).unwrap();
        let bindings = KeyBindings::new(Some(key("sc:0x29")), Some(key("mouse:4")), None).unwrap();
        let (inputs, mut received) = mpsc::unbounded_channel();
        spawn(&bindings, inputs).unwrap();
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut seen = Vec::new();
        // Ready, then press and release of each: five inputs.
        while Instant::now() < deadline && seen.len() < 5 {
            std::thread::sleep(Duration::from_millis(20));
            seen.extend(drain(&mut received));
        }
        let keys: Vec<(Key, KeyAction)> = seen
            .iter()
            .filter_map(|input| match input {
                Input::Key(event) => Some((event.key, event.action)),
                _ => None,
            })
            .collect();
        assert_eq!(seen.first(), Some(&Input::KeyDevice(DeviceState::Ready)));
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
