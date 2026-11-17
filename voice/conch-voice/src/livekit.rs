//! The real [`Transport`]: LiveKit's Rust SDK, and nothing decided here.
//!
//! This file translates. It turns the SDK's room events into [`SdkEvent`]s, received audio
//! into [`Frame`]s, and the two halves of a published track into [`MicTrack`] and
//! [`MicFeed`]. When to mute, what a disconnect leads to and whether a frame may be sent are
//! decided elsewhere, in code that is tested without the SDK.
//!
//! Two things here are load-bearing:
//!
//! - **[`LiveKitFeed::send`] is the only function in this crate that hands the microphone's
//!   audio to the SDK**, and it takes a [`GatedFrame`]. The SDK is never asked to open a
//!   microphone itself: the track is built on a [`NativeAudioSource`], which sends what it is
//!   given and captures nothing.
//! - **A screen share's sound is a second track, never the microphone's.** It is published
//!   only when the session asks ([`RoomHandle::publish_share_audio`]): a 48 kHz stereo
//!   [`NativeAudioSource`], source `ScreenshareAudio`, DTX off. Each share track keeps its own
//!   publication, apart from the microphone's, and a republish the SDK reports is routed by
//!   the publication's source and the id it had, so that one never replaces the other: a
//!   share republished never becomes what the microphone's mute acts on. A share track the
//!   SDK republishes and nobody holds is unpublished at once. [`LiveKitShareFeed::send`]
//!   hands it the mix of the captures and nothing else.
//! - **The SDK's `LocalTrackUnpublished` is always this client's own doing** (livekit 0.9.4
//!   sends it only from its own `unpublish_track`: a stop, `close`, a full reconnect's
//!   republish). It is not passed on. The server taking a share's grant back is seen only as
//!   a change of this participant's permission, which is passed on as
//!   [`SdkEvent::PermissionChanged`], with whether that permission allows the share.
//! - **Every text the SDK produces is scrubbed, put on one line and cut to a length before
//!   it leaves this file** (`secrets.rs`): an error's message may hold an address or a
//!   header on a path nobody measured, and when a join is refused it quotes the answer,
//!   whose words and length whatever answered chose.
//!
//! What of this file a test can reach without a room is tested here: how a disconnect's
//! reason is translated, and that `disable` disables the track. `mute`, `unmute` and the
//! publication the SDK hands over when it republishes need a joined room; nothing here
//! pretends to test them, and they stay with the end-to-end scenario (#188) and the real
//! runs behind `docs/design/conch-voice.md` §13.

use std::borrow::Cow;
use std::sync::{Arc, Mutex, PoisonError, Weak};

use conch_voice_api::{Secret, VoiceSessionResponseV1};
use conch_voice_audio::{
    FRAME_LEN, Frame, GatedFrame, SAMPLE_RATE, SILENCE, STEREO_FRAME_LEN, StereoFrame,
};
use futures_util::StreamExt;
use livekit::DisconnectReason as LkReason;
use livekit::options::TrackPublishOptions;
use livekit::prelude::{
    LocalAudioTrack, LocalTrack, LocalTrackPublication, Participant, RemoteAudioTrack, RemoteTrack,
    RemoteTrackPublication, Room, RoomEvent, RoomOptions, TrackKind, TrackSource,
};
use livekit::webrtc::audio_frame::AudioFrame;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use tokio::sync::mpsc;

use crate::sdk::{
    DisconnectReason, Joined, MicFeed, MicTrack, RoomHandle, SdkError, SdkEvent, ShareAudioFeed,
    ShareAudioTrack, Transport,
};
use crate::secrets::Scrubber;
use crate::share_capture::SHARE_CHANNELS;

/// How much audio the SDK's source may hold before it sends it: the value every
/// measurement in `docs/design/conch-voice.md` §13 was made with. Whatever is in it passed
/// the gate while the gate was open.
const SOURCE_QUEUE_MS: u32 = 100;

// `MicFeed::send` must hand the frame over in its first poll (`transmit::TrackLock`). The
// SDK's `capture_frame` copies the frame in chunks of the source's queue, each chunk before
// it first waits: with a queue of at least one frame, a frame is one chunk and is in the
// source by the end of the first poll. A smaller queue would hand part of it over after the
// lock is let go, so it is refused here (issue #631).
const _: () = assert!(SOURCE_QUEUE_MS >= conch_voice_audio::FRAME_MS);

/// Frames of one speaker waiting for the mixer. The mixer takes them every 10 ms and its
/// jitter buffer holds at most 200 ms, so more than this is a mixer that has stopped.
const RECEIVE_QUEUE_FRAMES: usize = 64;

/// Full scale of the SDK's 16-bit samples.
const I16_SCALE: f32 = 32768.0;

/// The companion a desktop session may have: a second LiveKit participant of the same
/// person that publishes the screen (`docs/design/windows-desktop.md` §7.2).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Companion {
    identity: String,
    owns_share_playback: bool,
}

impl Companion {
    /// A companion with this LiveKit identity. `owns_share_playback` is true when the
    /// companion plays other participants' share audio itself (B2), false when it receives
    /// nothing and the helper plays it (B1).
    #[must_use]
    pub fn new(identity: impl Into<String>, owns_share_playback: bool) -> Self {
        Self {
            identity: identity.into(),
            owns_share_playback,
        }
    }
}

/// Who plays other participants' share audio in a session that has a companion: a static
/// fact of whoever launched the helper, given as `--share-audio-owner` (issue #934).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum ShareAudioOwner {
    /// This helper plays it (B1). The default, and what the TUI is.
    #[default]
    Helper,
    /// The companion plays it (B2): this helper subscribes to nothing but voice.
    Companion,
}

/// Which of the remote tracks in a room this helper subscribes to: one playback owner per
/// audio source. With no companion every remote audio track is played, as it always was.
/// With one, its own tracks never are (that sound is the person's own application), and,
/// when it owns share playback, neither is other participants' share audio.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Subscription {
    companion: Option<Companion>,
}

impl Subscription {
    /// The rule for a session with this companion, or none.
    #[must_use]
    pub fn new(companion: Option<Companion>) -> Self {
        Self { companion }
    }

    /// The rule for a session `conchd` answered with: the companion it names, if any, and
    /// whoever the launcher said plays share audio. A session that names none keeps every
    /// remote audio track, whatever `owner` is: a companion that never connects is
    /// excluded by nothing, and there is nothing to share playback with.
    #[must_use]
    pub fn for_session(session: &VoiceSessionResponseV1, owner: ShareAudioOwner) -> Self {
        Self::new(
            session
                .companion_identity()
                .map(|identity| Companion::new(identity, owner == ShareAudioOwner::Companion)),
        )
    }

    /// Whether the helper subscribes to a track of `identity` of this kind and source.
    /// Video is never wanted: the helper plays voice and share audio, and shows nothing.
    fn wants(&self, identity: &str, kind: TrackKind, source: TrackSource) -> bool {
        if kind != TrackKind::Audio {
            return false;
        }
        match &self.companion {
            None => true,
            Some(companion) if companion.identity == identity => false,
            Some(companion) => {
                !(companion.owns_share_playback && source != TrackSource::Microphone)
            }
        }
    }
}

/// A remote publication as the subscription rule sees it. The SDK's is one; a test's is
/// another, since the SDK's cannot be made without a room.
trait Offered {
    fn identity(&self) -> String;
    fn kind(&self) -> TrackKind;
    fn source(&self) -> TrackSource;
    fn set_subscribed(&self, subscribed: bool);
}

/// A publication together with the identity of the participant that owns it.
struct SdkOffer {
    identity: String,
    publication: RemoteTrackPublication,
}

impl Offered for SdkOffer {
    fn identity(&self) -> String {
        self.identity.clone()
    }

    fn kind(&self) -> TrackKind {
        self.publication.kind()
    }

    fn source(&self) -> TrackSource {
        self.publication.source()
    }

    fn set_subscribed(&self, subscribed: bool) {
        self.publication.set_subscribed(subscribed);
    }
}

/// Subscribes to the publication if the rule wants it. Used for the publications present at
/// join and for those published later alike, so that the two cannot differ.
fn offer(rule: &Subscription, offered: &impl Offered) {
    if rule.wants(&offered.identity(), offered.kind(), offered.source()) {
        offered.set_subscribed(true);
    }
}

/// What the subscription rule needs to hear of a room, apart from the SDK's own types.
enum Seen<O> {
    /// The room is joined; these publications were already in it.
    Connected(Vec<O>),
    /// One more publication appeared.
    Published(O),
    /// The SDK restarted its session.
    Reconnected,
}

/// What the rule does for one thing seen in the room. `current` lists the publications the
/// room has now; it is asked for only on a reconnect.
///
/// After a full restart the SDK rebuilds the participants and announces their tracks
/// (`TrackPublished`) before the replacement session is installed (livekit 0.9.4,
/// `handle_signal_restarted` and `handle_restarted`), so a subscription made then can go to
/// the session that is gone; and with `auto_subscribe` off the new join subscribes to
/// nothing. Once it is `Reconnected` the rule is therefore applied to every publication
/// again, and `set_subscribed(true)` is sent again even to one already subscribed: the SDK
/// sends the update whatever the flag was.
fn subscribe_on<O: Offered>(rule: &Subscription, seen: Seen<O>, current: impl FnOnce() -> Vec<O>) {
    match seen {
        Seen::Connected(present) => present.iter().for_each(|o| offer(rule, o)),
        Seen::Published(published) => offer(rule, &published),
        Seen::Reconnected => current().iter().for_each(|o| offer(rule, o)),
    }
}

/// Every remote publication the room has now.
fn current_publications(room: &Room) -> Vec<SdkOffer> {
    room.remote_participants()
        .into_values()
        .flat_map(|participant| {
            let identity = participant.identity().to_string();
            participant
                .track_publications()
                .into_values()
                .map(move |publication| SdkOffer {
                    identity: identity.clone(),
                    publication,
                })
        })
        .collect()
}

/// LiveKit's SDK as a [`Transport`].
#[derive(Debug)]
pub struct LiveKit {
    scrubber: Arc<Scrubber>,
}

impl LiveKit {
    /// A transport whose errors are scrubbed with `scrubber`. Which tracks it subscribes
    /// to is given with each `connect`, from the session that connection is for.
    #[must_use]
    pub fn new(scrubber: Arc<Scrubber>) -> Self {
        Self { scrubber }
    }
}

/// The most characters of an SDK error's text that are kept. It becomes the `detail` of a
/// status line, on every attempt.
pub const ERROR_CHARS: usize = 512;

/// An error of the SDK's as one of ours: its text scrubbed, on one line, and then cut.
fn scrubbed(scrubber: &Scrubber, what: &str, error: &dyn std::fmt::Display) -> SdkError {
    let text = format!("{what}: {error}");
    SdkError::scrubbed(scrubber.scrub_line_within(&text, ERROR_CHARS))
}

/// What a reason the SDK gives for a disconnect is to this client (the second table of the
/// design note's §5). Every reason the SDK has is named, with no catch-all, so that one it
/// gains is decided here and not by default: the match stops compiling until it is.
fn reason(reason: LkReason) -> DisconnectReason {
    match reason {
        LkReason::RoomDeleted => DisconnectReason::RoomDeleted,
        LkReason::ParticipantRemoved => DisconnectReason::ParticipantRemoved,
        LkReason::DuplicateIdentity => DisconnectReason::DuplicateIdentity,
        LkReason::ServerShutdown => DisconnectReason::ServerShutdown,
        // No reason at all is what follows a `Reconnecting` the SDK gave up on. None of the
        // rest is treated differently from a connection that was lost: `conchd` is asked
        // again after a wait, and says what is still allowed.
        LkReason::UnknownReason
        | LkReason::ClientInitiated
        | LkReason::StateMismatch
        | LkReason::JoinFailure
        | LkReason::Migration
        | LkReason::SignalClose
        | LkReason::RoomClosed
        | LkReason::UserUnavailable
        | LkReason::UserRejected
        | LkReason::SipTrunkFailure
        | LkReason::ConnectionTimeout
        | LkReason::MediaFailure
        | LkReason::AgentError => DisconnectReason::Lost,
    }
}

/// The publication the SDK currently has for the microphone track. The SDK replaces it when
/// it republishes the track after a full reconnect, so both the event pump and the track's
/// handle need it. It is touched on a mute, an unmute and a republish: never per frame.
type Publication = Arc<Mutex<Option<LocalTrackPublication>>>;

/// One share track's publication cell, per track: a republish puts the new publication in
/// the cell that held the old one.
type ShareCell = Arc<Mutex<Option<LocalTrackPublication>>>;

/// Every share track's cell, held weakly: a track that is gone leaves nothing to follow.
type ShareCells = Arc<Mutex<Vec<Weak<Mutex<Option<LocalTrackPublication>>>>>>;

/// The publications a republish can replace, each in a place of its own.
struct Published {
    microphone: Publication,
    share: ShareCells,
}

/// The protocol's wire value of `TrackSource::SCREEN_SHARE_AUDIO` (livekit-protocol 0.8.0,
/// which the SDK does not re-export by name).
const SCREEN_SHARE_AUDIO: i32 = 4;

/// Whether a participant with this permission (`can_publish`, `can_publish_sources`) may
/// publish a share's sound. An empty source list with `can_publish` means every source. No
/// permission known: allowed here, and the server decides when the track is published.
fn share_allowed(permission: Option<(bool, &[i32])>) -> bool {
    match permission {
        None => true,
        Some((can_publish, sources)) => {
            can_publish && (sources.is_empty() || sources.contains(&SCREEN_SHARE_AUDIO))
        }
    }
}

/// Puts `next` in the live cell whose publication had the id `previous`, and forgets the cells
/// whose track is gone on the way. `Err(next)` when no cell held it: nobody owns that track.
fn follow<P>(
    cells: &mut Vec<Weak<Mutex<Option<P>>>>,
    previous: &str,
    next: P,
    id: impl Fn(&P) -> String,
) -> Result<(), P> {
    cells.retain(|cell| cell.strong_count() > 0);
    for cell in cells.iter().filter_map(Weak::upgrade) {
        let mut held = cell.lock().unwrap_or_else(PoisonError::into_inner);
        if held.as_ref().map(&id).as_deref() == Some(previous) {
            *held = Some(next);
            return Ok(());
        }
    }
    Err(next)
}

impl Transport for LiveKit {
    type Room = LiveKitRoom;

    async fn connect(
        &self,
        url: &str,
        token: &Secret,
        subscription: &Subscription,
    ) -> Result<Joined<LiveKitRoom>, SdkError> {
        let mut options = RoomOptions::default();
        // Nothing is subscribed to unless `Subscription` wants it (`offer`).
        options.auto_subscribe = false;
        // The token's one use. The SDK keeps its own copy for as long as the room lives.
        let (room, events) = Room::connect(url, token.expose(), options)
            .await
            .map_err(|error| scrubbed(&self.scrubber, "could not join the voice room", &error))?;
        let room = Arc::new(room);
        let publication = Publication::default();
        let share = ShareCells::default();
        let (forward, forwarded) = mpsc::unbounded_channel();
        tokio::spawn(pump(
            Arc::clone(&room),
            events,
            forward,
            Published {
                microphone: Arc::clone(&publication),
                share: Arc::clone(&share),
            },
            subscription.clone(),
        ));
        let room = LiveKitRoom {
            room,
            publication,
            share,
            scrubber: Arc::clone(&self.scrubber),
        };
        Ok((room, forwarded))
    }
}

/// Translates the SDK's events until the room has no more to say, or nobody listens. The
/// pump holds the room only to list its publications after a restart, so it lets go as soon
/// as the receiver of `forward` is gone.
async fn pump(
    room: Arc<Room>,
    mut events: mpsc::UnboundedReceiver<RoomEvent>,
    forward: mpsc::UnboundedSender<SdkEvent>,
    published: Published,
    rule: Subscription,
) {
    loop {
        let event = tokio::select! {
            event = events.recv() => event,
            () = forward.closed() => return,
        };
        let Some(event) = event else { return };
        let translated = match event {
            RoomEvent::Connected {
                participants_with_tracks,
            } => {
                let present = participants_with_tracks
                    .into_iter()
                    .flat_map(|(participant, publications)| {
                        let identity = participant.identity().to_string();
                        publications.into_iter().map(move |publication| SdkOffer {
                            identity: identity.clone(),
                            publication,
                        })
                    })
                    .collect();
                subscribe_on(&rule, Seen::Connected(present), Vec::new);
                continue;
            }
            RoomEvent::TrackPublished {
                publication,
                participant,
            } => {
                let identity = participant.identity().to_string();
                let published = SdkOffer {
                    identity,
                    publication,
                };
                subscribe_on(&rule, Seen::Published(published), Vec::new);
                continue;
            }
            RoomEvent::TrackSubscribed {
                track: RemoteTrack::Audio(audio),
                publication,
                participant,
            } => {
                let (frames, received) = mpsc::channel(RECEIVE_QUEUE_FRAMES);
                tokio::spawn(receive(audio, frames));
                SdkEvent::TrackSubscribed {
                    speaker: participant.identity().to_string(),
                    track: publication.sid().to_string(),
                    frames: received,
                }
            }
            RoomEvent::TrackUnsubscribed { publication, .. } => SdkEvent::TrackUnsubscribed {
                track: publication.sid().to_string(),
            },
            RoomEvent::Reconnecting => SdkEvent::Reconnecting,
            RoomEvent::Reconnected => {
                subscribe_on(&rule, Seen::Reconnected, || current_publications(&room));
                SdkEvent::Reconnected
            }
            RoomEvent::LocalTrackRepublished {
                previous_sid,
                publication: republished,
                ..
            } if republished.source() == TrackSource::ScreenshareAudio => {
                // Before the event is passed on, so that the mute it leads to reaches the
                // publication the server now knows, in the cell of the track that had it.
                let mut cells = published
                    .share
                    .lock()
                    .unwrap_or_else(PoisonError::into_inner);
                match follow(&mut cells, previous_sid.as_str(), republished, |held| {
                    held.sid().to_string()
                }) {
                    Ok(()) => SdkEvent::ShareRepublished,
                    Err(orphan) => {
                        // A share track nobody holds is never left published.
                        log::warn!(
                            "share audio: the SDK republished a share track nobody holds; it is unpublished"
                        );
                        let room = Arc::clone(&room);
                        let sid = orphan.sid();
                        tokio::spawn(async move {
                            let _ = room.local_participant().unpublish_track(&sid).await;
                        });
                        continue;
                    }
                }
            }
            RoomEvent::LocalTrackRepublished {
                publication: republished,
                ..
            } => {
                // Before the event is passed on, so that the mute it leads to reaches the
                // publication the server now knows, and never a share's.
                *published
                    .microphone
                    .lock()
                    .unwrap_or_else(PoisonError::into_inner) = Some(republished);
                SdkEvent::Republished
            }
            // This participant's own permission: the share's grant may have been taken back.
            RoomEvent::ParticipantPermissionChanged {
                participant: Participant::Local(_),
                permission,
            } => SdkEvent::PermissionChanged {
                allowed: share_allowed(
                    permission
                        .as_ref()
                        .map(|held| (held.can_publish, held.can_publish_sources.as_slice())),
                ),
            },
            RoomEvent::Disconnected { reason: why } => SdkEvent::Disconnected(reason(why)),
            // Nothing else changes what this client does. A refreshed token in particular
            // is the SDK's own business: it is not kept, used or shown here.
            _ => continue,
        };
        if forward.send(translated).is_err() {
            return;
        }
    }
}

/// Reads one remote track for as long as it yields audio, and passes it on in frames of
/// exactly [`FRAME_LEN`] samples. If the mixer is not keeping up the newest audio is
/// dropped here; it stops when the mixer has let go of the track.
async fn receive(track: RemoteAudioTrack, frames: mpsc::Sender<Frame>) {
    let mut stream = NativeAudioStream::new(track.rtc_track(), SAMPLE_RATE as i32, 1);
    let mut frame = SILENCE;
    let mut filled = 0;
    while let Some(received) = stream.next().await {
        for sample in received.data.iter() {
            if let Some(slot) = frame.get_mut(filled) {
                *slot = f32::from(*sample) / I16_SCALE;
            }
            filled += 1;
            if filled == FRAME_LEN {
                filled = 0;
                match frames.try_send(frame) {
                    Ok(()) | Err(mpsc::error::TrySendError::Full(_)) => {}
                    Err(mpsc::error::TrySendError::Closed(_)) => return,
                }
            }
        }
    }
}

/// A joined LiveKit room.
pub struct LiveKitRoom {
    room: Arc<Room>,
    publication: Publication,
    /// Each share-audio track's publication, apart from the microphone's.
    share: ShareCells,
    scrubber: Arc<Scrubber>,
}

impl RoomHandle for LiveKitRoom {
    type Track = LiveKitTrack;
    type Feed = LiveKitFeed;
    type ShareTrack = LiveKitShareTrack;
    type ShareFeed = LiveKitShareFeed;

    fn publish_share_audio(
        &self,
    ) -> impl std::future::Future<Output = Result<(LiveKitShareTrack, LiveKitShareFeed), SdkError>>
    + Send
    + 'static {
        let room = Arc::clone(&self.room);
        let cells = Arc::clone(&self.share);
        let scrubber = Arc::clone(&self.scrubber);
        async move {
            // A source of its own, stereo, with the SDK's audio processing off: the mix is
            // sent as it was made.
            let source = NativeAudioSource::new(
                AudioSourceOptions::default(),
                SAMPLE_RATE,
                SHARE_CHANNELS,
                SOURCE_QUEUE_MS,
            );
            let track = LocalAudioTrack::create_audio_track(
                "screen_share_audio",
                RtcAudioSource::Native(source.clone()),
            );
            // Kept, to disable it after a republish: the SDK republishes this same track.
            let kept = track.clone();
            // Music and games are not speech: no discontinuous transmission.
            let options = TrackPublishOptions {
                source: TrackSource::ScreenshareAudio,
                dtx: false,
                ..Default::default()
            };
            let published = room
                .local_participant()
                .publish_track(LocalTrack::Audio(track), options)
                .await
                .map_err(|error| {
                    scrubbed(&scrubber, "could not publish the share's sound", &error)
                })?;
            // A cell of this track's own, followed by the pump through a republish.
            let cell: ShareCell = Arc::new(Mutex::new(Some(published)));
            cells
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .push(Arc::downgrade(&cell));
            let track = LiveKitShareTrack {
                room,
                track: kept,
                publication: cell,
            };
            let feed = LiveKitShareFeed {
                source,
                samples: vec![0; STEREO_FRAME_LEN],
                scrubber,
                failed: false,
            };
            Ok((track, feed))
        }
    }

    async fn publish_microphone(&self) -> Result<(LiveKitTrack, LiveKitFeed), SdkError> {
        // The SDK's audio processing is off: a frame is sent as the gate passed it.
        let source = NativeAudioSource::new(
            AudioSourceOptions::default(),
            SAMPLE_RATE,
            1,
            SOURCE_QUEUE_MS,
        );
        let track = LocalAudioTrack::create_audio_track(
            "microphone",
            RtcAudioSource::Native(source.clone()),
        );
        let options = TrackPublishOptions {
            source: TrackSource::Microphone,
            ..Default::default()
        };
        let published = self
            .room
            .local_participant()
            .publish_track(LocalTrack::Audio(track.clone()), options)
            .await
            .map_err(|error| {
                scrubbed(&self.scrubber, "could not publish the microphone", &error)
            })?;
        *self
            .publication
            .lock()
            .unwrap_or_else(PoisonError::into_inner) = Some(published);
        let track = LiveKitTrack {
            track,
            publication: Arc::clone(&self.publication),
        };
        let feed = LiveKitFeed {
            source,
            samples: vec![0; FRAME_LEN],
            scrubber: Arc::clone(&self.scrubber),
            failed: false,
        };
        Ok((track, feed))
    }

    async fn close(self) {
        // Closing a room that is already closed is an error the SDK reports and nobody
        // needs: either way the room is left.
        let _ = self.room.close().await;
    }
}

/// The published microphone track.
pub struct LiveKitTrack {
    track: LocalAudioTrack,
    publication: Publication,
}

impl LiveKitTrack {
    fn with_publication(&self, act: impl FnOnce(&LocalTrackPublication)) {
        let publication = self
            .publication
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        if let Some(publication) = publication.as_ref() {
            act(publication);
        }
    }
}

impl MicTrack for LiveKitTrack {
    fn mute(&self) {
        self.with_publication(LocalTrackPublication::mute);
    }

    fn unmute(&self) {
        self.with_publication(LocalTrackPublication::unmute);
    }

    fn disable(&self) {
        self.track.disable();
    }
}

/// The published microphone track's audio source.
pub struct LiveKitFeed {
    source: NativeAudioSource,
    /// One frame as the SDK takes it, reused.
    samples: Vec<i16>,
    scrubber: Arc<Scrubber>,
    /// A refusal by the SDK is logged once, not a hundred times a second.
    failed: bool,
}

impl MicFeed for LiveKitFeed {
    /// The one place audio is handed to the SDK. Its only caller is the transmit task
    /// (`transmit.rs`), which has the frame from the gate.
    async fn send(&mut self, frame: GatedFrame<'_>) {
        for (out, sample) in self.samples.iter_mut().zip(frame.samples()) {
            // Truncation is the point: the product is within an i16 after the clamp.
            *out = (sample.clamp(-1.0, 1.0) * f32::from(i16::MAX)) as i16;
        }
        let frame = AudioFrame {
            data: Cow::Borrowed(self.samples.as_slice()),
            sample_rate: SAMPLE_RATE,
            num_channels: 1,
            samples_per_channel: FRAME_LEN as u32,
        };
        // The SDK copies the frame into its source before it first waits, so the frame is
        // handed over in the first poll, as `MicFeed::send` requires: checked against
        // libwebrtc 0.3.51's `capture_frame` (one chunk while `SOURCE_QUEUE_MS` holds a
        // frame). Check it again whenever `scripts/voice-pins.sh` moves the SDK.
        match self.source.capture_frame(&frame).await {
            Ok(()) => self.failed = false,
            Err(error) if !self.failed => {
                self.failed = true;
                let text = error.to_string();
                log::warn!(
                    "the SDK refused a frame of audio: {}",
                    self.scrubber.scrub_line(&text)
                );
            }
            Err(_) => {}
        }
    }
}

/// The published share-audio track. Its publication is the one the SDK has now: a republish
/// replaces it in this track's own cell (`pump`), and its id is that publication's.
pub struct LiveKitShareTrack {
    room: Arc<Room>,
    /// The track itself, the same one a republish publishes again.
    track: LocalAudioTrack,
    publication: ShareCell,
}

impl LiveKitShareTrack {
    fn with_publication<T>(&self, act: impl FnOnce(&LocalTrackPublication) -> T) -> Option<T> {
        let publication = self
            .publication
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        publication.as_ref().map(act)
    }
}

impl ShareAudioTrack for LiveKitShareTrack {
    fn id(&self) -> String {
        self.with_publication(|publication| publication.sid().to_string())
            .unwrap_or_default()
    }

    fn is_allowed(&self) -> bool {
        let permission = self.room.local_participant().permission();
        share_allowed(
            permission
                .as_ref()
                .map(|held| (held.can_publish, held.can_publish_sources.as_slice())),
        )
    }

    fn is_published(&self) -> bool {
        self.with_publication(LocalTrackPublication::sid)
            .is_some_and(|sid| {
                self.room
                    .local_participant()
                    .track_publications()
                    .contains_key(&sid)
            })
    }

    fn mute(&self) {
        self.with_publication(LocalTrackPublication::mute);
    }

    fn unmute(&self) {
        self.with_publication(LocalTrackPublication::unmute);
    }

    fn disable(&self) {
        self.track.disable();
    }

    fn unpublish(self) -> impl std::future::Future<Output = ()> + Send + 'static {
        let sid = self.with_publication(LocalTrackPublication::sid);
        let room = self.room;
        async move {
            let Some(sid) = sid else { return };
            // The SDK takes the publication off this client before anything can fail (it
            // has no wait inside). Not found is a track already gone, and nothing to do;
            // any other error is the peer connection refusing to drop the sender, which is
            // said by its reason: the track was muted before this was asked.
            match room.local_participant().unpublish_track(&sid).await {
                Ok(_) | Err(livekit::RoomError::Internal(_)) => {}
                Err(_) => log::warn!(
                    "share audio: the media server may still hold the share's track; it was muted"
                ),
            }
        }
    }
}

/// The share-audio track's audio source.
pub struct LiveKitShareFeed {
    source: NativeAudioSource,
    /// One stereo frame as the SDK takes it, reused.
    samples: Vec<i16>,
    scrubber: Arc<Scrubber>,
    /// A refusal by the SDK is logged once, not a hundred times a second.
    failed: bool,
}

impl ShareAudioFeed for LiveKitShareFeed {
    /// The one place the share's mix is handed to the SDK.
    async fn send(&mut self, frame: &StereoFrame) {
        for (out, sample) in self.samples.iter_mut().zip(frame) {
            // Truncation is the point: the product is within an i16 after the clamp.
            *out = (sample.clamp(-1.0, 1.0) * f32::from(i16::MAX)) as i16;
        }
        let frame = AudioFrame {
            data: Cow::Borrowed(self.samples.as_slice()),
            sample_rate: SAMPLE_RATE,
            num_channels: SHARE_CHANNELS,
            samples_per_channel: FRAME_LEN as u32,
        };
        match self.source.capture_frame(&frame).await {
            Ok(()) => self.failed = false,
            Err(error) if !self.failed => {
                self.failed = true;
                let text = error.to_string();
                log::warn!(
                    "the SDK refused a frame of the share's sound: {}",
                    self.scrubber.scrub_line(&text)
                );
            }
            Err(_) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The share's source is the SDK's own, in stereo: native code, with no room.
    #[test]
    fn the_share_source_is_48_khz_stereo() {
        let source = NativeAudioSource::new(
            AudioSourceOptions::default(),
            SAMPLE_RATE,
            SHARE_CHANNELS,
            SOURCE_QUEUE_MS,
        );
        assert_eq!(source.sample_rate(), 48_000);
        assert_eq!(source.num_channels(), 2);
    }

    /// Creates the SDK's audio source, which is native code in libwebrtc: it proves that
    /// the pinned clang compiled the SDK's C++ and that the verified libwebrtc links and
    /// runs, with no LiveKit server anywhere.
    #[test]
    fn the_sdk_links_and_its_native_code_runs() {
        let source = NativeAudioSource::new(
            AudioSourceOptions::default(),
            SAMPLE_RATE,
            1,
            SOURCE_QUEUE_MS,
        );
        assert_eq!(source.sample_rate(), SAMPLE_RATE);
        assert_eq!(source.num_channels(), 1);
    }

    #[test]
    fn an_sdk_error_is_one_line_whatever_the_server_put_in_it() {
        let scrubber = Scrubber::new();
        let error = scrubbed(&scrubber, "could not join", &"401\r\nconnected\x1b[2J");
        assert_eq!(error.to_string(), "could not join: 401  connected [2J");
    }

    #[test]
    fn an_sdk_error_is_scrubbed_before_it_becomes_one_of_ours() {
        let scrubber = Scrubber::new();
        scrubber.connection(
            &Secret::new("FAKE-join-token-do-not-print"),
            &Secret::new("FAKE-room-name-do-not-print"),
        );
        let error = scrubbed(
            &scrubber,
            "could not join the voice room",
            &"ws://h/rtc?access_token=FAKE-join-token-do-not-print (FAKE-room-name-do-not-print)",
        );
        assert_eq!(
            error.to_string(),
            "could not join the voice room: ws://h/rtc?access_token=[redacted] ([redacted])"
        );
    }

    /// When a join is refused the SDK's error quotes the answer it got, and whatever
    /// answered where the session said LiveKit was chose how long that is.
    #[test]
    fn an_sdk_error_is_cut_to_a_length_after_it_is_scrubbed() {
        let scrubber = Scrubber::new();
        scrubber.connection(
            &Secret::new("FAKE-join-token-do-not-print"),
            &Secret::new("FAKE-room-name-do-not-print"),
        );
        let megabytes = "A".repeat(2 * 1024 * 1024);
        let error = scrubbed(&scrubber, "could not join", &megabytes).to_string();
        assert_eq!(error.chars().count(), ERROR_CHARS + " [cut]".len());
        assert!(error.ends_with("A [cut]"), "it says that it was cut");

        // A token that lies across the place of the cut is replaced, not cut in half.
        let lead = "x".repeat(ERROR_CHARS - "could not join: ".len() - 12);
        let text = format!("{lead}FAKE-join-token-do-not-print and the rest");
        let error = scrubbed(&scrubber, "could not join", &text).to_string();
        assert!(!error.contains("FAKE"), "{error}");
        assert!(error.ends_with("x[redacted] a [cut]"), "{error}");

        let short = scrubbed(&scrubber, "could not join", &"401").to_string();
        assert_eq!(short, "could not join: 401");
    }

    /// The same table as the design note's §5, over every reason the SDK has. The numbers
    /// are the protocol's: a reason that is added shows up here as one more that is valid.
    #[test]
    fn each_reason_the_sdk_gives_for_a_disconnect_is_translated_as_the_design_says() {
        let table = [
            (LkReason::UnknownReason, DisconnectReason::Lost),
            (LkReason::ClientInitiated, DisconnectReason::Lost),
            (
                LkReason::DuplicateIdentity,
                DisconnectReason::DuplicateIdentity,
            ),
            (LkReason::ServerShutdown, DisconnectReason::ServerShutdown),
            (
                LkReason::ParticipantRemoved,
                DisconnectReason::ParticipantRemoved,
            ),
            (LkReason::RoomDeleted, DisconnectReason::RoomDeleted),
            (LkReason::StateMismatch, DisconnectReason::Lost),
            (LkReason::JoinFailure, DisconnectReason::Lost),
            (LkReason::Migration, DisconnectReason::Lost),
            (LkReason::SignalClose, DisconnectReason::Lost),
            (LkReason::RoomClosed, DisconnectReason::Lost),
            (LkReason::UserUnavailable, DisconnectReason::Lost),
            (LkReason::UserRejected, DisconnectReason::Lost),
            (LkReason::SipTrunkFailure, DisconnectReason::Lost),
            (LkReason::ConnectionTimeout, DisconnectReason::Lost),
            (LkReason::MediaFailure, DisconnectReason::Lost),
            (LkReason::AgentError, DisconnectReason::Lost),
        ];
        for (theirs, ours) in table {
            assert_eq!(reason(theirs), ours, "{}", theirs.as_str_name());
        }
        // Every reason the SDK can decode is in the table above.
        let known: Vec<LkReason> = (0..1024)
            .filter_map(|number| LkReason::try_from(number).ok())
            .collect();
        assert_eq!(
            known.len(),
            table.len(),
            "the SDK has a reason this table lacks"
        );
        for theirs in known {
            assert!(table.iter().any(|(listed, _)| *listed == theirs));
        }
    }

    /// `disable` is what stops a republished track, which is muted already and enabled all
    /// the same. A track can be made, and read back, without a room.
    #[test]
    fn disabling_the_track_disables_it() {
        let source = NativeAudioSource::new(
            AudioSourceOptions::default(),
            SAMPLE_RATE,
            1,
            SOURCE_QUEUE_MS,
        );
        let track =
            LocalAudioTrack::create_audio_track("microphone", RtcAudioSource::Native(source));
        let ours = LiveKitTrack {
            track: track.clone(),
            // Not published: there is no room. Muting therefore does nothing here, and is
            // not what this test is about.
            publication: Publication::default(),
        };
        assert!(track.is_enabled(), "a new track is enabled");
        ours.mute();
        assert!(
            track.is_enabled(),
            "with no publication a mute reaches nothing"
        );
        ours.disable();
        assert!(!track.is_enabled());
    }

    /// A publication that counts how often it was told to subscribe.
    #[derive(Clone)]
    struct Fake {
        identity: &'static str,
        kind: TrackKind,
        source: TrackSource,
        told: std::rc::Rc<std::cell::Cell<u32>>,
    }

    impl Fake {
        fn new(identity: &'static str, kind: TrackKind, source: TrackSource) -> Self {
            Self {
                identity,
                kind,
                source,
                told: std::rc::Rc::default(),
            }
        }
    }

    impl Offered for Fake {
        fn identity(&self) -> String {
            self.identity.to_owned()
        }
        fn kind(&self) -> TrackKind {
            self.kind
        }
        fn source(&self) -> TrackSource {
            self.source
        }
        fn set_subscribed(&self, subscribed: bool) {
            assert!(subscribed, "the rule only ever subscribes");
            self.told.set(self.told.get() + 1);
        }
    }

    /// The room: another's voice, another's share audio, the companion's share audio and
    /// microphone, and another's screen video.
    fn room() -> Vec<Fake> {
        vec![
            Fake::new("bob", TrackKind::Audio, TrackSource::Microphone),
            Fake::new("bob", TrackKind::Audio, TrackSource::ScreenshareAudio),
            Fake::new(
                "alice~screen",
                TrackKind::Audio,
                TrackSource::ScreenshareAudio,
            ),
            Fake::new("alice~screen", TrackKind::Audio, TrackSource::Microphone),
            Fake::new("bob", TrackKind::Video, TrackSource::Screenshare),
        ]
    }

    /// How often each of the room's publications was told to subscribe.
    fn told(room: &[Fake]) -> Vec<u32> {
        room.iter().map(|f| f.told.get()).collect()
    }

    fn arrangements() -> [Subscription; 3] {
        [
            Subscription::default(),
            Subscription::new(Some(Companion::new("alice~screen", false))),
            Subscription::new(Some(Companion::new("alice~screen", true))),
        ]
    }

    /// For no companion, B1 and B2: how often each of the room's publications is wanted.
    const WANTED: [[u32; 5]; 3] = [[1, 1, 1, 1, 0], [1, 1, 0, 0, 0], [1, 0, 0, 0, 0]];

    #[test]
    fn publications_present_at_join_are_subscribed_to_as_the_arrangement_says() {
        for (rule, wanted) in arrangements().iter().zip(WANTED) {
            let present = room();
            subscribe_on(rule, Seen::Connected(present.clone()), || {
                panic!("a join does not list the room")
            });
            assert_eq!(told(&present), wanted, "{rule:?}");
        }
    }

    #[test]
    fn a_publication_published_later_is_subscribed_to_as_the_arrangement_says() {
        for (rule, wanted) in arrangements().iter().zip(WANTED) {
            let later = room();
            for publication in &later {
                subscribe_on(rule, Seen::Published(publication.clone()), || {
                    panic!("a publication does not list the room")
                });
            }
            assert_eq!(told(&later), wanted, "{rule:?}");
        }
    }

    /// Several sharers at once (issue #1483) change nothing in the rules of #906: with a
    /// companion that owns share playback, no other participant's share audio is wanted
    /// however many there are, the companion's own tracks are never wanted, and voices
    /// are; with a companion that does not own it, the others' share audio is wanted.
    #[test]
    fn several_sharers_leave_the_share_audio_rules_as_they_were() {
        let several = || {
            vec![
                Fake::new("bob", TrackKind::Audio, TrackSource::Microphone),
                Fake::new("bob", TrackKind::Audio, TrackSource::ScreenshareAudio),
                Fake::new("val", TrackKind::Audio, TrackSource::ScreenshareAudio),
                Fake::new("carol", TrackKind::Audio, TrackSource::ScreenshareAudio),
                Fake::new(
                    "alice~screen",
                    TrackKind::Audio,
                    TrackSource::ScreenshareAudio,
                ),
                Fake::new("val", TrackKind::Video, TrackSource::Screenshare),
            ]
        };
        let rules = [
            // No companion: every other sharer's sound is wanted, video never.
            (Subscription::default(), [1, 1, 1, 1, 1, 0]),
            // A companion that does not own share playback hears the others' sound itself.
            (
                Subscription::new(Some(Companion::new("alice~screen", false))),
                [1, 1, 1, 1, 0, 0],
            ),
            // One that owns it plays every watched stream: none of the others' is wanted.
            (
                Subscription::new(Some(Companion::new("alice~screen", true))),
                [1, 0, 0, 0, 0, 0],
            ),
        ];
        for (rule, wanted) in rules {
            let present = several();
            subscribe_on(&rule, Seen::Connected(present.clone()), || {
                panic!("a join does not list the room")
            });
            assert_eq!(told(&present), wanted, "{rule:?}");
        }
    }

    /// After a restart every wanted publication is told again, even one that was already
    /// subscribed to, and an unwanted one still is not.
    #[test]
    fn a_reconnect_subscribes_again_to_every_wanted_publication() {
        for (rule, wanted) in arrangements().iter().zip(WANTED) {
            let current = room();
            subscribe_on(rule, Seen::Connected(current.clone()), Vec::new);
            subscribe_on(rule, Seen::Reconnected, || current.clone());
            assert_eq!(told(&current), wanted.map(|n| n * 2), "{rule:?}");
        }
    }

    /// The share's grant is the permission to publish its source: an empty list with
    /// `can_publish` is every source, and nothing known leaves it to the server.
    #[test]
    fn the_share_is_allowed_by_the_permission_to_publish_its_source() {
        let camera = 1;
        let microphone = 2;
        let screen = 3;
        for (permission, allowed) in [
            (None, true),
            (Some((true, &[][..])), true),
            (Some((true, &[microphone, SCREEN_SHARE_AUDIO][..])), true),
            (Some((true, &[microphone][..])), false),
            (Some((true, &[camera, microphone, screen][..])), false),
            (Some((false, &[][..])), false),
            (Some((false, &[SCREEN_SHARE_AUDIO][..])), false),
        ] {
            assert_eq!(share_allowed(permission), allowed, "{permission:?}");
        }
    }

    /// A republish is followed into the cell of the track whose publication had the id
    /// (`previous_sid`), and only that one; one that no live track had is nobody's.
    #[test]
    fn a_republish_is_followed_into_the_cell_that_held_the_old_id() {
        let first = Arc::new(Mutex::new(Some("TR_a".to_owned())));
        let second = Arc::new(Mutex::new(Some("TR_b".to_owned())));
        let gone = Arc::new(Mutex::new(Some("TR_c".to_owned())));
        let mut cells = vec![
            Arc::downgrade(&first),
            Arc::downgrade(&second),
            Arc::downgrade(&gone),
        ];
        drop(gone);
        let id = |held: &String| held.clone();

        assert_eq!(follow(&mut cells, "TR_b", "TR_b2".to_owned(), id), Ok(()));
        assert_eq!(first.lock().unwrap().as_deref(), Some("TR_a"));
        assert_eq!(second.lock().unwrap().as_deref(), Some("TR_b2"));
        // The cell whose track was dropped is forgotten.
        assert_eq!(cells.len(), 2);

        // The old id again, or the dropped track's: nobody holds it.
        assert_eq!(
            follow(&mut cells, "TR_b", "TR_x".to_owned(), id),
            Err("TR_x".to_owned())
        );
        assert_eq!(
            follow(&mut cells, "TR_c", "TR_y".to_owned(), id),
            Err("TR_y".to_owned())
        );
        assert_eq!(second.lock().unwrap().as_deref(), Some("TR_b2"));
    }
}
