//! The real [`Transport`]: LiveKit's Rust SDK, and nothing decided here.
//!
//! This file translates. It turns the SDK's room events into [`SdkEvent`]s, received audio
//! into [`Frame`]s, and the two halves of a published track into [`MicTrack`] and
//! [`MicFeed`]. When to mute, what a disconnect leads to and whether a frame may be sent are
//! decided elsewhere, in code that is tested without the SDK.
//!
//! Two things here are load-bearing:
//!
//! - **[`LiveKitFeed::send`] is the only function in this crate that hands audio to the
//!   SDK**, and it takes a [`GatedFrame`]. The SDK is never asked to open a microphone
//!   itself: the track is built on a [`NativeAudioSource`], which sends what it is given and
//!   captures nothing.
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
use std::sync::{Arc, Mutex, PoisonError};

use conch_voice_api::Secret;
use conch_voice_audio::{FRAME_LEN, Frame, GatedFrame, SAMPLE_RATE, SILENCE};
use futures_util::StreamExt;
use livekit::DisconnectReason as LkReason;
use livekit::options::TrackPublishOptions;
use livekit::prelude::{
    LocalAudioTrack, LocalTrack, LocalTrackPublication, RemoteAudioTrack, RemoteTrack, Room,
    RoomEvent, RoomOptions, TrackSource,
};
use livekit::webrtc::audio_frame::AudioFrame;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use tokio::sync::mpsc;

use crate::sdk::{
    DisconnectReason, Joined, MicFeed, MicTrack, RoomHandle, SdkError, SdkEvent, Transport,
};
use crate::secrets::Scrubber;

/// How much audio the SDK's source may hold before it sends it: the value every
/// measurement in `docs/design/conch-voice.md` §13 was made with. Whatever is in it passed
/// the gate while the gate was open.
const SOURCE_QUEUE_MS: u32 = 100;

/// Frames of one speaker waiting for the mixer. The mixer takes them every 10 ms and its
/// jitter buffer holds at most 200 ms, so more than this is a mixer that has stopped.
const RECEIVE_QUEUE_FRAMES: usize = 64;

/// Full scale of the SDK's 16-bit samples.
const I16_SCALE: f32 = 32768.0;

/// LiveKit's SDK as a [`Transport`].
#[derive(Debug)]
pub struct LiveKit {
    scrubber: Arc<Scrubber>,
}

impl LiveKit {
    /// A transport whose errors are scrubbed with `scrubber`.
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

impl Transport for LiveKit {
    type Room = LiveKitRoom;

    async fn connect(&self, url: &str, token: &Secret) -> Result<Joined<LiveKitRoom>, SdkError> {
        let mut options = RoomOptions::default();
        options.auto_subscribe = true;
        // The token's one use. The SDK keeps its own copy for as long as the room lives.
        let (room, events) = Room::connect(url, token.expose(), options)
            .await
            .map_err(|error| scrubbed(&self.scrubber, "could not join the voice room", &error))?;
        let publication = Publication::default();
        let (forward, forwarded) = mpsc::unbounded_channel();
        tokio::spawn(pump(events, forward, Arc::clone(&publication)));
        let room = LiveKitRoom {
            room,
            publication,
            scrubber: Arc::clone(&self.scrubber),
        };
        Ok((room, forwarded))
    }
}

/// Translates the SDK's events until the room has no more to say.
async fn pump(
    mut events: mpsc::UnboundedReceiver<RoomEvent>,
    forward: mpsc::UnboundedSender<SdkEvent>,
    publication: Publication,
) {
    while let Some(event) = events.recv().await {
        let translated = match event {
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
            RoomEvent::Reconnected => SdkEvent::Reconnected,
            RoomEvent::LocalTrackRepublished {
                publication: republished,
                ..
            } => {
                // Before the event is passed on, so that the mute it leads to reaches the
                // publication the server now knows.
                *publication.lock().unwrap_or_else(PoisonError::into_inner) = Some(republished);
                SdkEvent::Republished
            }
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
    room: Room,
    publication: Publication,
    scrubber: Arc<Scrubber>,
}

impl RoomHandle for LiveKitRoom {
    type Track = LiveKitTrack;
    type Feed = LiveKitFeed;

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

#[cfg(test)]
mod tests {
    use super::*;

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
}
