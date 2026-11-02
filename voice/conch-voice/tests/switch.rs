//! Switching the microphone and the speakers inside a running session (issue #655,
//! `docs/design/tui-voice.md` §20.5): the session's half of a managed owner's `set_devices`,
//! driven here directly. The SDK is a fake that records every call and every frame it is
//! handed; the microphones and speakers are the test's own, opened by the name the session
//! asks for, so that a test can say which device a frame came from and what was opened.

#![allow(clippy::unwrap_used, clippy::expect_used)]

mod support;

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::Error;
use conch_voice::receive::{BoxedSink, Speakers};
use conch_voice::sdk::DisconnectReason;
use conch_voice::session::Timings;
use conch_voice::transmit::BoxedMic;
use conch_voice_audio::{FRAME_LEN, Frame, MicRead, MicSource, SpeakerSink};
use conch_voice_control::LineCommand::{Down, Mute, Up};
use conch_voice_control::TalkMode;
use serde_json::json;
use tokio::sync::Notify;

use support::fake::Call;
use support::script::Switch;
use support::stub::{Reply, Stub};
use support::{OpenNamedMic, OpenNamedSink, Rig, Setup, several_frames};

/// What the test's devices did, in order: `"<name> read"` the first time a microphone is
/// read, `"<name> dropped"` when it is dropped, which closes a real one's stream.
type Log = Arc<Mutex<Vec<String>>>;

fn logged(log: &Log) -> Vec<String> {
    log.lock().unwrap().clone()
}

/// The level of every sample of the default microphone's frames, and of the other one's:
/// the fake keeps each frame's first sample, so a test can tell whose a frame was.
const DEFAULT_LEVEL: f32 = 0.25;
const USB_LEVEL: f32 = 0.5;

/// A microphone that never stops, every sample at one level, that says when it is first
/// read and when it is dropped. Read every 10 ms, as a tone is.
struct Marked {
    name: &'static str,
    level: f32,
    log: Log,
    read: bool,
}

impl MicSource for Marked {
    fn read(&mut self, frame: &mut Frame) -> MicRead {
        if !self.read {
            self.read = true;
            self.log.lock().unwrap().push(format!("{} read", self.name));
        }
        *frame = [self.level; FRAME_LEN];
        MicRead::Ready
    }
}

impl Drop for Marked {
    fn drop(&mut self) {
        self.log
            .lock()
            .unwrap()
            .push(format!("{} dropped", self.name));
    }
}

/// A microphone stream that is open and has nothing to give: a named microphone that is
/// not there, which `dmn-audio` stands the default one in for and `audio.rs` discards.
struct Absent;

impl MicSource for Absent {
    fn read(&mut self, _frame: &mut Frame) -> MicRead {
        MicRead::NoMicrophone
    }
}

/// Opens microphones by name: the default and `usb-mic` are there; `absent` is a stream
/// with no microphone behind it; `nothing` is no microphone to open; anything else cannot
/// be opened.
fn microphones(log: &Log) -> OpenNamedMic {
    let log = Arc::clone(log);
    Arc::new(move |name| {
        let marked = |name, level| {
            let mic = Marked {
                name,
                level,
                log: Arc::clone(&log),
                read: false,
            };
            Some(Box::new(mic) as BoxedMic)
        };
        Ok(match name {
            None => marked("default", DEFAULT_LEVEL),
            Some("usb-mic") => marked("usb-mic", USB_LEVEL),
            Some("absent") => Some(Box::new(Absent) as BoxedMic),
            Some("nothing") => None,
            Some(_) => {
                return Err(Error::PipeWire {
                    doing: "open the microphone",
                    detail: "FAKE: the stream could not be made".to_owned(),
                });
            }
        })
    })
}

/// The first samples of the frames the track was handed, from `from` on.
fn levels_sent(rig: &Rig, from: usize) -> Vec<f32> {
    rig.sdk.first_samples().split_off(from)
}

/// Checks the order of everything the client did to the fake: a frame is handed over only
/// between an unmute and the mute that follows it.
fn assert_frames_only_while_unmuted(calls: &[Call]) {
    let mut unmuted = false;
    for (at, call) in calls.iter().enumerate() {
        match call {
            Call::Unmute => unmuted = true,
            Call::Mute | Call::Publish | Call::Close => unmuted = false,
            Call::Frame => assert!(unmuted, "a frame at call {at} outside a press: {calls:?}"),
            Call::Connect | Call::Disable => {}
        }
    }
}

/// The track was unmuted for the one press and muted after it, and never unmuted again: a
/// forced shut mutes at once, and again when the gate says it has shut.
fn assert_muted_once_and_for_good(calls: &[Call]) {
    let (press, after) = calls.split_at(4);
    assert_eq!(
        press,
        [Call::Connect, Call::Publish, Call::Mute, Call::Unmute],
        "{calls:?}"
    );
    assert!(
        !after.is_empty() && after.iter().all(|call| *call == Call::Mute),
        "muted at the switch, and not unmuted again: {calls:?}"
    );
}

fn with_microphones(log: &Log) -> Setup {
    Setup {
        open_mic: Some(microphones(log)),
        ..Setup::default()
    }
}

/// With the microphone open, a switch closes the old stream (its source is dropped) before
/// the new one is handed over and read; the new one is what a press then sends from.
#[tokio::test]
async fn switching_the_microphone_closes_the_old_stream_before_the_new_one_is_handed_over() {
    let log = Log::default();
    let rig = Rig::start_with(Stub::start().await, with_microphones(&log), |_| {}).await;
    rig.ready().await;
    rig.until("the default microphone read", |_| {
        logged(&log).contains(&"default read".to_owned())
    })
    .await;

    rig.switch(Some("usb-mic"), None);
    rig.until("the new microphone read", |_| {
        logged(&log).contains(&"usb-mic read".to_owned())
    })
    .await;
    assert_eq!(
        logged(&log),
        ["default read", "default dropped", "usb-mic read"],
        "the old stream is closed before the new one is handed over"
    );
    assert_eq!(rig.mic_names(), [None, Some("usb-mic".to_owned())]);
    assert_eq!((rig.mic_opened(), rig.mic_closed()), (2, 1));

    // Nothing was being sent, and nothing is: a press sends from the new one only.
    rig.ready().await;
    assert_eq!(rig.sdk.frames(), 0);
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;
    assert!(levels_sent(&rig, 0).iter().all(|level| *level == USB_LEVEL));
    rig.line(Up);
    rig.not_talking().await;
    rig.reported(2).await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

/// A switch during a press: the transmission ends at once, with one `stopped` after the
/// mute; no frame of either microphone reaches the track after the switch began, though the
/// key is still held; and only a new press sends again, from the new microphone.
#[tokio::test]
async fn a_switch_during_a_press_ends_it_and_nothing_is_sent_until_a_new_press() {
    let log = Log::default();
    let rig = Rig::start_with(Stub::start().await, with_microphones(&log), |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;

    rig.switch(Some("usb-mic"), None);
    rig.stopped_beyond(0).await;
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    // The key is still down.
    several_frames().await;
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "nothing after the switch");
    assert_eq!(rig.conchd.reported(), ["started", "stopped"], "one stopped");
    assert_muted_once_and_for_good(&rig.sdk.control_calls());
    assert!(
        levels_sent(&rig, 0)
            .iter()
            .all(|level| *level == DEFAULT_LEVEL),
        "nothing of the new microphone was sent"
    );
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    rig.ready().await;

    // A new press sends again, from the new microphone only.
    rig.line(Up);
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(sent + 3).await;
    let from = usize::try_from(sent).unwrap();
    assert!(
        levels_sent(&rig, from)
            .iter()
            .all(|level| *level == USB_LEVEL)
    );
    rig.line(Up);
    rig.not_talking().await;
    rig.reported(4).await;
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

/// Issue #656 (from #655's review): a switch while a press still waits for `conchd`'s
/// answer to its `started` report. The press ends there: the track is never unmuted, nothing
/// is sent though the key is still held, the transmission has its one `stopped` once the
/// answer comes, and the answer opens nothing, since no press waits for it any more.
#[tokio::test]
async fn a_switch_while_a_press_waits_for_its_started_answer_ends_it_before_the_track_opens() {
    let log = Log::default();
    let mut timings = support::quick();
    // Long enough that the press is still waiting when the switch comes.
    timings.report_wait = Duration::from_secs(5);
    let setup = Setup {
        timings,
        report_timeout: Duration::from_secs(5),
        ..with_microphones(&log)
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    let answer = Arc::new(Notify::new());
    rig.conchd.next_transmit(Reply::Held(
        Arc::clone(&answer),
        Box::new(Reply::Empty(204)),
    ));
    rig.ready().await;
    rig.line(Down);
    rig.until("the started report at conchd", |rig| {
        rig.conchd.reported() == ["started"]
    })
    .await;
    rig.talking().await;

    rig.switch(Some("usb-mic"), None);
    rig.not_talking().await;
    rig.ready().await;
    several_frames().await;
    // The answer comes after the press was ended: it opens nothing.
    answer.notify_one();
    rig.reported(2).await;
    several_frames().await;
    several_frames().await;
    assert_eq!(rig.conchd.reported(), ["started", "stopped"], "one stopped");
    assert_eq!(
        rig.sdk.frames(),
        0,
        "nothing was sent, though the key is held"
    );
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert!(
        !rig.sdk.calls().contains(&Call::Unmute),
        "the track was never unmuted: {:?}",
        rig.sdk.calls()
    );
    assert!(rig.sdk.is_muted());

    // A fresh press sends, from the new microphone.
    rig.line(Up);
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;
    assert!(levels_sent(&rig, 0).iter().all(|level| *level == USB_LEVEL));
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

/// A switch during the release tail cuts the tail: nothing of it, or of the new
/// microphone, follows the switch, and the transmission has its one `stopped`.
#[tokio::test]
async fn a_switch_during_the_release_tail_cuts_it_and_nothing_follows() {
    let log = Log::default();
    let setup = Setup {
        // A hundred frames: the switch, sent straight after the release, lands well inside.
        release_tail_ms: 1_000,
        ..with_microphones(&log)
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;
    let at_release = rig.sdk.frames();
    rig.line(Up);
    rig.switch(Some("usb-mic"), None);
    rig.stopped_beyond(0).await;
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    several_frames().await;
    several_frames().await;
    assert_eq!(rig.sdk.frames(), sent, "nothing after the switch");
    assert!(
        sent - at_release < 50,
        "the tail was cut: {} frames of it",
        sent - at_release
    );
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    assert_muted_once_and_for_good(&rig.sdk.control_calls());
    assert!(
        levels_sent(&rig, 0)
            .iter()
            .all(|level| *level == DEFAULT_LEVEL)
    );
    assert_eq!(rig.sdk.frames_while_muted(), 0);
    assert_frames_only_while_unmuted(&rig.sdk.calls());
}

/// The hang, in frames: the configuration's default of 400 ms.
const HANG: u64 = 40;

/// In open mode a switch ends what was being sent, and the new microphone opens nothing
/// however loud it is: a whole hang of quiet heard on it, and then a fresh onset, are
/// needed. Quiet heard before a switch does not count.
#[tokio::test]
async fn in_open_mode_a_new_microphone_needs_a_whole_hang_of_quiet_and_a_fresh_onset() {
    // One sound for every microphone the session opens, loud while the test says so.
    let (make, loud) = Switch::make();
    let open: OpenNamedMic = Arc::new(move |_| Ok(Some(make())));
    // The hang is timed by the microphone's frames, not the wall clock: a loaded machine
    // that runs the transmit task late would otherwise let the clock say the hang had
    // passed before the frames did (issue #1098).
    let setup = Setup {
        talk_mode: TalkMode::Open,
        open_mic: Some(open),
        timings: Timings {
            transmit_clock: loud.clock(),
            ..support::quick()
        },
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    let longer_than_the_hang = || loud.frames_pass(HANG + 20);
    longer_than_the_hang().await;

    // Speaking, and the microphone is switched: the opening ends at once.
    loud.set(true);
    rig.talking().await;
    rig.frames_beyond(3).await;
    rig.switch(Some("usb-mic"), None);
    rig.stopped_beyond(0).await;
    rig.not_talking().await;
    let sent = rig.sdk.frames();
    // Loud on the new microphone from its first frame: nothing, for as long as it lasts.
    longer_than_the_hang().await;
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), sent, "the switch opened nothing");
    assert_eq!(rig.conchd.reported(), ["started", "stopped"]);
    // Quiet for a whole hang, then sound: a new opening.
    loud.set(false);
    longer_than_the_hang().await;
    loud.set(true);
    rig.talking().await;
    rig.frames_beyond(sent + 3).await;
    loud.set(false);
    rig.reported(4).await;
    rig.not_talking().await;

    // Quiet for longer than the hang, then a switch, and sound at once: the quiet before
    // the switch does not count, and nothing is sent.
    longer_than_the_hang().await;
    let sent = rig.sdk.frames();
    rig.switch(Some("headset-mic"), None);
    loud.set(true);
    longer_than_the_hang().await;
    longer_than_the_hang().await;
    assert_eq!(rig.sdk.frames(), sent, "a switch is no onset");
    assert_eq!(
        rig.conchd.reported(),
        ["started", "stopped", "started", "stopped"]
    );
    assert_eq!(
        rig.mic_names(),
        [
            None,
            Some("usb-mic".to_owned()),
            Some("headset-mic".to_owned())
        ]
    );
    assert_frames_only_while_unmuted(&rig.sdk.calls());
    assert_eq!(rig.sdk.frames_while_muted(), 0);
}

/// Before the microphone is first opened (here, while the grant allows listening only) a
/// switch opens nothing and keeps the name: the microphone opened once publishing is
/// allowed is the one named last.
#[tokio::test]
async fn a_switch_before_the_microphone_is_opened_opens_nothing_and_its_name_is_used_later() {
    let log = Log::default();
    let conchd = Stub::start().await;
    conchd.can_publish(false);
    let rig = Rig::start_with(conchd, with_microphones(&log), |_| {}).await;
    rig.connected(1).await;
    rig.until("the own state", |rig| {
        rig.own()["blocked"] == json!(["no_publish_grant", "no_microphone"])
    })
    .await;

    rig.switch(Some("usb-mic"), None);
    // Inputs are taken in order: once the mute is shown, the switch has been taken.
    rig.line(Mute);
    rig.until("muted", |rig| rig.own()["muted"] == true).await;
    rig.line(Mute);
    rig.until("unmuted", |rig| rig.own()["muted"] == false)
        .await;
    several_frames().await;
    assert_eq!(rig.mic_opened(), 0, "listen-only: no microphone is opened");
    assert!(logged(&log).is_empty());

    // The next session allows publishing.
    rig.conchd.can_publish(true);
    rig.sdk.disconnect(DisconnectReason::Lost);
    rig.connected(2).await;
    rig.ready().await;
    assert_eq!(rig.mic_names(), [Some("usb-mic".to_owned())]);
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;
    assert!(levels_sent(&rig, 0).iter().all(|level| *level == USB_LEVEL));
}

/// A microphone that cannot be opened, that there is none of, or that is not there leaves
/// the session running with no microphone: a press sends nothing, and no other microphone
/// is opened in its place.
#[tokio::test]
async fn a_microphone_that_cannot_be_opened_or_is_not_there_leaves_none_and_none_other_is_used() {
    let log = Log::default();
    let rig = Rig::start_with(Stub::start().await, with_microphones(&log), |_| {}).await;
    rig.ready().await;
    for (earlier, name) in ["broken", "nothing", "absent"].into_iter().enumerate() {
        rig.switch(Some(name), None);
        rig.until("no microphone", |rig| {
            rig.own()["blocked"] == json!(["no_microphone"])
        })
        .await;
        rig.line(Down);
        rig.until("the press to be refused", |rig| {
            rig.events_named("press_ignored").len() == earlier + 1
        })
        .await;
        rig.line(Up);
        several_frames().await;
        assert_eq!(rig.sdk.frames(), 0, "{name}: nothing is sent");
        assert_eq!(
            rig.connection_states().last().map(String::as_str),
            Some("connected"),
            "{name}: the session runs on"
        );
        // A microphone that is there, between each: it is used as soon as it is opened.
        rig.switch(Some("usb-mic"), None);
        rig.ready().await;
    }
    for ignored in rig.events_named("press_ignored") {
        assert_eq!(ignored["reason"], "no_microphone");
    }
    // The one that could not be opened says why, as at the start.
    let said = rig.events_named("microphone");
    assert_eq!(said.len(), 1);
    assert!(
        said[0]["detail"]
            .as_str()
            .unwrap()
            .contains("could not be made")
    );
    let named = |name: &str| Some(name.to_owned());
    assert_eq!(
        rig.mic_names(),
        [
            None,
            named("broken"),
            named("usb-mic"),
            named("nothing"),
            named("usb-mic"),
            named("absent"),
            named("usb-mic"),
        ],
        "nothing but what was named was opened"
    );
    assert_eq!(rig.conchd.reported(), Vec::<String>::new());
    assert_eq!(rig.sdk.frames(), 0);
}

/// Speakers for these tests, as the test sees them: the audible frames played to them,
/// and whether they were dropped, which closes a real stream.
#[derive(Clone, Default)]
struct Heard {
    audible: Arc<AtomicU64>,
    dropped: Arc<AtomicU64>,
}

impl Heard {
    fn audible(&self) -> u64 {
        self.audible.load(Ordering::SeqCst)
    }

    fn dropped(&self) -> u64 {
        self.dropped.load(Ordering::SeqCst)
    }
}

struct HeardSink(Heard);

impl SpeakerSink for HeardSink {
    fn play(&mut self, frame: &Frame) {
        if frame.iter().any(|sample| sample.abs() > 0.01) {
            self.0.audible.fetch_add(1, Ordering::SeqCst);
        }
    }
}

impl Speakers for HeardSink {}

impl Drop for HeardSink {
    fn drop(&mut self) {
        self.0.dropped.fetch_add(1, Ordering::SeqCst);
    }
}

/// Opens speakers by name: the default and `headset` are these; anything else cannot be
/// opened.
fn speakers(default: &Heard, headset: &Heard) -> OpenNamedSink {
    let (default, headset) = (default.clone(), headset.clone());
    Arc::new(move |name| match name {
        None => Ok(Box::new(HeardSink(default.clone())) as BoxedSink),
        Some("headset") => Ok(Box::new(HeardSink(headset.clone())) as BoxedSink),
        Some(_) => Err(Error::PipeWire {
            doing: "open the speakers",
            detail: "FAKE: the stream could not be made".to_owned(),
        }),
    })
}

/// A remote speaker who talks without a pause, until the returned task is aborted.
fn someone_talking(rig: &Rig) -> tokio::task::JoinHandle<()> {
    let (_track, frames) = rig.sdk.speaker("p3");
    tokio::spawn(async move {
        loop {
            if frames.send([0.25; FRAME_LEN]).await.is_err() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
}

/// A switch of the speakers during a press: the old speakers are dropped, what is heard
/// after the switch reaches the new ones only, and the gate is not touched: the press goes
/// on, with no mute and no report.
#[tokio::test]
async fn switching_the_speakers_reaches_the_new_ones_only_and_leaves_the_gate_alone() {
    let (default, headset) = (Heard::default(), Heard::default());
    let setup = Setup {
        open_sink: Some(speakers(&default, &headset)),
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    let talking = someone_talking(&rig);
    rig.until("the speaker heard", |_| default.audible() > 0)
        .await;
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;

    rig.switch(None, Some("headset"));
    rig.until("the old speakers dropped", |_| default.dropped() == 1)
        .await;
    let heard_before = default.audible();
    rig.until("the speaker heard on the new speakers", |_| {
        headset.audible() > 0
    })
    .await;
    several_frames().await;
    assert_eq!(default.audible(), heard_before, "the new speakers only");

    // The press went on through the switch.
    let sent = rig.sdk.frames();
    rig.frames_beyond(sent + 3).await;
    assert_eq!(rig.own()["transmitting"], true);
    assert_eq!(rig.conchd.reported(), ["started"]);
    assert_eq!(
        rig.sdk.control_calls(),
        [Call::Connect, Call::Publish, Call::Mute, Call::Unmute],
        "the gate was not touched"
    );
    assert_eq!(rig.sink_names(), [None, Some("headset".to_owned())]);
    rig.line(Up);
    rig.not_talking().await;
    talking.abort();
}

/// Speakers that cannot be opened: the old ones are dropped all the same, no others are
/// opened in their place, and the session runs on.
#[tokio::test]
async fn speakers_that_cannot_be_opened_are_none_and_the_session_runs_on() {
    let (default, headset) = (Heard::default(), Heard::default());
    let setup = Setup {
        open_sink: Some(speakers(&default, &headset)),
        ..Setup::default()
    };
    let rig = Rig::start_with(Stub::start().await, setup, |_| {}).await;
    rig.ready().await;
    let talking = someone_talking(&rig);
    rig.until("the speaker heard", |_| default.audible() > 0)
        .await;

    rig.switch(None, Some("broken"));
    rig.until("the old speakers dropped", |_| default.dropped() == 1)
        .await;
    let heard_before = default.audible();
    several_frames().await;
    assert_eq!(default.audible(), heard_before);
    assert_eq!(headset.audible(), 0);
    assert_eq!(rig.sink_names(), [None, Some("broken".to_owned())]);

    // The session runs on: a press transmits.
    rig.line(Down);
    rig.talking().await;
    rig.frames_beyond(3).await;
    rig.line(Up);
    rig.not_talking().await;
    // And speakers that can be opened are used as soon as they are named.
    rig.switch(None, Some("headset"));
    rig.until("the speaker heard on the new speakers", |_| {
        headset.audible() > 0
    })
    .await;
    talking.abort();
}
