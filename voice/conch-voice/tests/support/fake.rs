//! A fake of LiveKit's SDK, in the place of `conch_voice::livekit`: it records everything
//! the client does to it, in order, and lets a test say what happens to the room.
//!
//! It keeps the two quirks of the real SDK that the client's order of calls is built
//! around (`docs/design/conch-voice.md` §13 row 12): publishing leaves the track enabled
//! whatever its mute state, and muting a track that is muted already does nothing, so a
//! republished track stays enabled until it is disabled.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use conch_voice::sdk::{
    DisconnectReason, Joined, MicFeed, MicTrack, RoomHandle, SdkError, SdkEvent, Transport,
};
use conch_voice::secrets::Scrubber;
use conch_voice_api::Secret;
use conch_voice_audio::{Frame, GatedFrame};
use tokio::sync::{Notify, mpsc};

/// One thing the client did to the SDK.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Call {
    Connect,
    Publish,
    Mute,
    Unmute,
    Disable,
    /// One frame handed to the track's audio source.
    Frame,
    Close,
}

/// What a log record of the SDK's would be: written through the `log` facade while the
/// fake connects, as the real SDK writes while it does.
#[derive(Debug, Clone)]
pub struct Record {
    pub level: log::Level,
    pub target: &'static str,
    /// `{token}` is replaced by the join token of the attempt, and `{room}` by the name of
    /// the room that token is for (see [`room_of`]).
    pub text: String,
}

/// The room a join token of the stub `conchd` is for. The real SDK learns the room's name
/// when it joins; the fake is told only the token, and the stub numbers the two alike
/// (`FAKE-join-token-…-2` is for `FAKE-room-name-…-2`).
pub fn room_of(token: &str) -> String {
    token.replace(super::stub::FAKE_JOIN, super::stub::FAKE_ROOM)
}

#[derive(Default)]
struct Inner {
    calls: Vec<Call>,
    /// The address and join token of every connection attempt.
    connects: Vec<(String, String)>,
    /// The events of the room that is joined now.
    events: Option<mpsc::UnboundedSender<SdkEvent>>,
    /// What the next connection attempts fail with; when empty they succeed.
    connect_failures: VecDeque<String>,
    /// While set, `publish_microphone` waits here after it has published.
    publish_hold: Option<Arc<Notify>>,
    /// While set, handing over a frame never returns.
    sends_hang: bool,
    close_takes: Duration,
    records: Vec<Record>,
    muted: bool,
    enabled: bool,
    frames: u64,
    /// Frames with a sample that is not zero.
    loud_frames: u64,
    /// Frames handed over while the track was muted or disabled.
    frames_while_muted: u64,
    tracks: u64,
}

/// The fake. Cloning it gives another handle on the same one.
#[derive(Clone)]
pub struct FakeSdk {
    inner: Arc<Mutex<Inner>>,
    scrubber: Arc<Scrubber>,
}

impl FakeSdk {
    /// A fake whose errors are scrubbed with `scrubber`, as the real layer's are.
    pub fn new(scrubber: Arc<Scrubber>) -> Self {
        Self {
            inner: Arc::default(),
            scrubber,
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap()
    }

    // ------------------------------------------------------------ what a test arranges

    /// The next connection attempt fails with this text, in which `{token}` stands for the
    /// join token it was given and `{room}` for that token's room: an SDK error on a path
    /// nobody measured.
    pub fn fail_next_connect(&self, text: &str) {
        self.lock().connect_failures.push_back(text.to_owned());
    }

    /// Makes `publish_microphone` stop after it has published and before it returns, until
    /// the returned handle is notified: the moment between the publish and the mute.
    pub fn hold_publish(&self) -> Arc<Notify> {
        let hold = Arc::new(Notify::new());
        self.lock().publish_hold = Some(Arc::clone(&hold));
        hold
    }

    /// From now on handing a frame to the track never returns: an SDK that has hung.
    pub fn hang_sends(&self) {
        self.lock().sends_hang = true;
    }

    /// Leaving a room takes this long.
    pub fn close_takes(&self, time: Duration) {
        self.lock().close_takes = time;
    }

    /// The fake writes these through the `log` facade at every connection attempt.
    pub fn log_on_connect(&self, records: Vec<Record>) {
        self.lock().records = records;
    }

    /// Reports an event of the joined room. False if no room is joined.
    pub fn emit(&self, event: SdkEvent) -> bool {
        match self.lock().events.as_ref() {
            Some(events) => events.send(event).is_ok(),
            None => false,
        }
    }

    /// The SDK's own full reconnect: it publishes the track again, which enables it
    /// whatever its mute state, and says so.
    pub fn republish(&self) {
        self.lock().enabled = true;
        assert!(self.emit(SdkEvent::Republished), "no room is joined");
    }

    /// The room is over, for this reason.
    pub fn disconnect(&self, reason: DisconnectReason) {
        assert!(
            self.emit(SdkEvent::Disconnected(reason)),
            "no room is joined"
        );
    }

    /// A remote speaker's track is subscribed to. Frames sent on what is returned arrive
    /// as that speaker's audio.
    pub fn speaker(&self, identity: &str) -> (String, mpsc::Sender<Frame>) {
        let track = {
            let mut inner = self.lock();
            inner.tracks += 1;
            format!("TR_fake{}", inner.tracks)
        };
        let (frames, received) = mpsc::channel(64);
        let subscribed = SdkEvent::TrackSubscribed {
            speaker: identity.to_owned(),
            track: track.clone(),
            frames: received,
        };
        assert!(self.emit(subscribed), "no room is joined");
        (track, frames)
    }

    // ------------------------------------------------------------- what a test observes

    /// Everything the client did, in order.
    pub fn calls(&self) -> Vec<Call> {
        self.lock().calls.clone()
    }

    /// Everything the client did other than hand over frames, in order.
    pub fn control_calls(&self) -> Vec<Call> {
        let mut calls = self.calls();
        calls.retain(|call| *call != Call::Frame);
        calls
    }

    /// How many times the client did this.
    pub fn count(&self, call: Call) -> usize {
        self.lock().calls.iter().filter(|c| **c == call).count()
    }

    /// Frames handed to the track's audio source so far.
    pub fn frames(&self) -> u64 {
        self.lock().frames
    }

    /// Of those, the ones that were not silence.
    pub fn loud_frames(&self) -> u64 {
        self.lock().loud_frames
    }

    /// Of those, the ones handed over while the track was muted or disabled.
    pub fn frames_while_muted(&self) -> u64 {
        self.lock().frames_while_muted
    }

    /// The join token of every connection attempt, in order.
    pub fn tokens(&self) -> Vec<String> {
        self.lock()
            .connects
            .iter()
            .map(|(_, token)| token.clone())
            .collect()
    }

    /// The address of every connection attempt, in order.
    pub fn addresses(&self) -> Vec<String> {
        self.lock()
            .connects
            .iter()
            .map(|(url, _)| url.clone())
            .collect()
    }

    /// Whether the track is muted, as LiveKit was told.
    pub fn is_muted(&self) -> bool {
        self.lock().muted
    }

    /// Whether the track would send what it is handed.
    pub fn is_enabled(&self) -> bool {
        self.lock().enabled
    }
}

impl Transport for FakeSdk {
    type Room = FakeRoom;

    async fn connect(&self, url: &str, token: &Secret) -> Result<Joined<FakeRoom>, SdkError> {
        let (failure, records) = {
            let mut inner = self.lock();
            inner.calls.push(Call::Connect);
            inner
                .connects
                .push((url.to_owned(), token.expose().to_owned()));
            (inner.connect_failures.pop_front(), inner.records.clone())
        };
        let fill = |text: &str| {
            text.replace("{token}", token.expose())
                .replace("{room}", &room_of(token.expose()))
        };
        for record in records {
            let text = fill(&record.text);
            log::log!(target: record.target, record.level, "{text}");
        }
        if let Some(text) = failure {
            // As the real layer does: the SDK's text, scrubbed before it becomes an error.
            let text = fill(&text);
            return Err(SdkError::scrubbed(self.scrubber.scrub(&text).into_owned()));
        }
        let (events, received) = mpsc::unbounded_channel();
        self.lock().events = Some(events);
        let room = FakeRoom {
            inner: Arc::clone(&self.inner),
        };
        Ok((room, received))
    }
}

/// A room the fake has "joined".
pub struct FakeRoom {
    inner: Arc<Mutex<Inner>>,
}

impl RoomHandle for FakeRoom {
    type Track = FakeTrack;
    type Feed = FakeFeed;

    async fn publish_microphone(&self) -> Result<(FakeTrack, FakeFeed), SdkError> {
        let hold = {
            let mut inner = self.inner.lock().unwrap();
            inner.calls.push(Call::Publish);
            // Published: enabled and not muted, whatever came before.
            inner.muted = false;
            inner.enabled = true;
            inner.publish_hold.take()
        };
        if let Some(hold) = hold {
            hold.notified().await;
        }
        let track = FakeTrack {
            inner: Arc::clone(&self.inner),
        };
        let feed = FakeFeed {
            inner: Arc::clone(&self.inner),
        };
        Ok((track, feed))
    }

    async fn close(self) {
        let takes = {
            let mut inner = self.inner.lock().unwrap();
            inner.calls.push(Call::Close);
            inner.events = None;
            inner.close_takes
        };
        if !takes.is_zero() {
            tokio::time::sleep(takes).await;
        }
    }
}

/// The fake's published track.
pub struct FakeTrack {
    inner: Arc<Mutex<Inner>>,
}

impl MicTrack for FakeTrack {
    fn mute(&self) {
        let mut inner = self.inner.lock().unwrap();
        inner.calls.push(Call::Mute);
        // As the SDK: muting a muted track changes nothing, an enabled one included.
        if !inner.muted {
            inner.muted = true;
            inner.enabled = false;
        }
    }

    fn unmute(&self) {
        let mut inner = self.inner.lock().unwrap();
        inner.calls.push(Call::Unmute);
        if inner.muted {
            inner.muted = false;
            inner.enabled = true;
        }
    }

    fn disable(&self) {
        let mut inner = self.inner.lock().unwrap();
        inner.calls.push(Call::Disable);
        inner.enabled = false;
    }
}

/// The fake's audio source: it counts what it is handed and keeps none of it.
pub struct FakeFeed {
    inner: Arc<Mutex<Inner>>,
}

impl MicFeed for FakeFeed {
    async fn send(&mut self, frame: GatedFrame<'_>) {
        let hang = {
            let mut inner = self.inner.lock().unwrap();
            if !inner.sends_hang {
                inner.calls.push(Call::Frame);
                inner.frames += 1;
                if frame.samples().iter().any(|sample| *sample != 0.0) {
                    inner.loud_frames += 1;
                }
                if inner.muted || !inner.enabled {
                    inner.frames_while_muted += 1;
                }
            }
            inner.sends_hang
        };
        if hang {
            std::future::pending::<()>().await;
        }
    }
}
