//! The seam between this client's logic and LiveKit's SDK.
//!
//! Everything that decides anything (when the gate opens, when the track is muted, what a
//! disconnect leads to) is written against these traits and tested with a fake in their
//! place. The real implementation, `livekit.rs`, is kept thin: it translates and decides
//! nothing.
//!
//! The shape follows what the design relies on (`docs/design/conch-voice.md` §3 and §5):
//!
//! - A room is joined with an address and a join token ([`Transport::connect`]), and from
//!   then on reports what happens to it as [`SdkEvent`]s.
//! - A microphone track is published once ([`RoomHandle::publish_microphone`]). What comes
//!   back is in two parts, for two owners: the [`MicTrack`], which the session loop mutes
//!   and unmutes, and the [`MicFeed`], which the transmit task alone holds and which takes
//!   nothing but a [`GatedFrame`].

use std::fmt;
use std::future::Future;

use conch_voice_api::Secret;
use conch_voice_audio::{Frame, GatedFrame};
use tokio::sync::mpsc;

/// Why the SDK says a connection ended (the second table of the design note's §5).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum DisconnectReason {
    /// `ROOM_DELETED`: `conchd` rotated the channel's room.
    RoomDeleted,
    /// `PARTICIPANT_REMOVED`: `conchd` removed this participant by name.
    ParticipantRemoved,
    /// `DUPLICATE_IDENTITY`: the same identity joined from elsewhere.
    DuplicateIdentity,
    /// `SERVER_SHUTDOWN`: LiveKit is shutting down.
    ServerShutdown,
    /// No reason was given, which is what follows a `Reconnecting` the SDK gave up on; or
    /// any other reason, none of which the design treats differently from a lost
    /// connection.
    Lost,
}

/// Something the SDK reported about a joined room.
pub enum SdkEvent {
    /// A remote participant's microphone track was subscribed to. `frames` yields its audio
    /// as it arrives, 10 ms at a time, until the track goes away.
    TrackSubscribed {
        /// The participant's identity, which `conchd` sets to `p<principal id>`.
        speaker: String,
        /// The track's id: what [`SdkEvent::TrackUnsubscribed`] will name.
        track: String,
        /// The track's audio.
        frames: mpsc::Receiver<Frame>,
    },
    /// A track subscribed to earlier is gone.
    TrackUnsubscribed {
        /// The track's id.
        track: String,
    },
    /// The connection was interrupted and the SDK is trying to restore it by itself.
    Reconnecting,
    /// The SDK restored the connection.
    Reconnected,
    /// The SDK published the microphone track again, after a full reconnect. The track is
    /// enabled again whatever its mute state was (§13 row 12).
    Republished,
    /// The connection is over.
    Disconnected(DisconnectReason),
}

/// Shows which event it is, and no audio.
impl fmt::Debug for SdkEvent {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            SdkEvent::TrackSubscribed { speaker, track, .. } => f
                .debug_struct("TrackSubscribed")
                .field("speaker", speaker)
                .field("track", track)
                .finish_non_exhaustive(),
            SdkEvent::TrackUnsubscribed { track } => f
                .debug_struct("TrackUnsubscribed")
                .field("track", track)
                .finish(),
            SdkEvent::Reconnecting => f.write_str("Reconnecting"),
            SdkEvent::Reconnected => f.write_str("Reconnected"),
            SdkEvent::Republished => f.write_str("Republished"),
            SdkEvent::Disconnected(reason) => f.debug_tuple("Disconnected").field(reason).finish(),
        }
    }
}

/// A failure the SDK reported. Its text is the SDK's own with every secret already replaced
/// (see `secrets.rs`); an implementation must scrub before it makes one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SdkError {
    scrubbed: String,
}

impl SdkError {
    /// An error whose text holds no join token and no room name.
    #[must_use]
    pub fn scrubbed(text: impl Into<String>) -> Self {
        Self {
            scrubbed: text.into(),
        }
    }
}

impl fmt::Display for SdkError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.scrubbed)
    }
}

/// What a successful [`Transport::connect`] gives: the room, and the events it will report.
pub type Joined<R> = (R, mpsc::UnboundedReceiver<SdkEvent>);

/// The way into a LiveKit room.
pub trait Transport: Send + Sync + 'static {
    /// A joined room.
    type Room: RoomHandle;

    /// Joins the room `token` is for, at `url`. The token is used for this and kept by
    /// nothing this crate owns.
    fn connect(
        &self,
        url: &str,
        token: &Secret,
    ) -> impl Future<Output = Result<Joined<Self::Room>, SdkError>> + Send;
}

/// A room this client has joined.
pub trait RoomHandle: Send + 'static {
    /// The published microphone track, as the session loop sees it.
    type Track: MicTrack;
    /// The published microphone track's audio source, as the transmit task sees it.
    type Feed: MicFeed;

    /// Publishes the microphone track. It comes back enabled and unmuted, whatever was done
    /// to it before (§13 row 12): the caller mutes it with the next statement.
    fn publish_microphone(
        &self,
    ) -> impl Future<Output = Result<(Self::Track, Self::Feed), SdkError>> + Send;

    /// Leaves the room.
    fn close(self) -> impl Future<Output = ()> + Send;
}

/// The published microphone track: the layer `conchd` observes. It is not the guarantee
/// that nothing is sent; the transmit gate is (§3).
pub trait MicTrack: Send + 'static {
    /// Mutes the track, and tells LiveKit.
    fn mute(&self);
    /// Unmutes the track, and tells LiveKit.
    fn unmute(&self);
    /// Stops the track sending what it is handed, without telling LiveKit anything. Needed
    /// after a republish, when the track is muted already and enabled all the same.
    fn disable(&self);
}

/// The way audio reaches the published track.
pub trait MicFeed: Send + 'static {
    /// Hands one frame to the track's audio source.
    ///
    /// It takes a [`GatedFrame`], which only [`conch_voice_audio::TransmitGate::pass`] can
    /// make, so nothing can be sent that did not pass the gate.
    fn send(&mut self, frame: GatedFrame<'_>) -> impl Future<Output = ()> + Send;
}
