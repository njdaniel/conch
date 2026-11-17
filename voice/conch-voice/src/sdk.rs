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
//! - A screen share's sound is a second local track of the same participant, published only
//!   on its owner's command ([`RoomHandle::publish_share_audio`],
//!   `docs/design/linux-share-audio.md` §5.1): its [`ShareAudioTrack`], muted and unmuted on
//!   its own and never by the microphone's mute, and its [`ShareAudioFeed`], which takes the
//!   stereo mix of the captures and has nothing to do with the gate. Republishing and
//!   unpublishing it are reported apart from the microphone's ([`SdkEvent::ShareRepublished`]),
//!   so that one is never taken for the other. The server taking the share's grant back is
//!   seen only as a change of what this participant may publish
//!   ([`SdkEvent::PermissionChanged`], then [`ShareAudioTrack::is_allowed`]): the SDK reports
//!   no unpublish of a local track the server closed.

use std::fmt;
use std::future::Future;

use conch_voice_api::Secret;
use conch_voice_audio::{Frame, GatedFrame, StereoFrame};

use crate::livekit::Subscription;
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
    /// The SDK published the share-audio track again, after a full reconnect: unmuted,
    /// whatever its mute was. Never the microphone's. The track keeps its handle: the
    /// handle follows the new publication (its id changes).
    ShareRepublished,
    /// The server changed what this participant may publish. `allowed` is whether the
    /// permission this event carried lets it publish a share's sound. The session also asks
    /// the track for the permission the SDK holds now ([`ShareAudioTrack::is_allowed`]), and
    /// ends the share's sound when either says no: so a revoke that a re-grant follows at
    /// once is still seen. A revoke and a re-grant that both happen while the signal
    /// connection is down cannot be seen with livekit 0.9.4: the SDK learns only the final
    /// permission when it is back.
    PermissionChanged {
        /// Whether the event's own permission allows the share's sound.
        allowed: bool,
    },
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
            SdkEvent::ShareRepublished => f.write_str("ShareRepublished"),
            SdkEvent::PermissionChanged { allowed } => f
                .debug_struct("PermissionChanged")
                .field("allowed", allowed)
                .finish(),
            SdkEvent::Disconnected(reason) => f.debug_tuple("Disconnected").field(reason).finish(),
        }
    }
}

/// A failure the SDK reported. Its text is the SDK's own with every secret already replaced
/// and no control character left (see `secrets.rs`); an implementation must scrub before it
/// makes one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SdkError {
    scrubbed: String,
}

impl SdkError {
    /// An error whose text holds no join token, no room name and no control character:
    /// what [`crate::secrets::Scrubber::scrub_line`] returns.
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

    /// Joins the room `token` is for, at `url`, subscribed to the tracks `subscription`
    /// wants. The token is used for this and kept by nothing this crate owns. The rule is
    /// built from the session this join is for, so none outlives its session.
    fn connect(
        &self,
        url: &str,
        token: &Secret,
        subscription: &Subscription,
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

    /// The published share-audio track, as the session loop sees it.
    type ShareTrack: ShareAudioTrack;
    /// The published share-audio track's audio source, as the feeder of the mix sees it.
    type ShareFeed: ShareAudioFeed;

    /// Publishes a screen share's sound on this participant: a 48 kHz stereo track whose
    /// source is `ScreenshareAudio`, with DTX off. It comes back unmuted. The future holds
    /// nothing of the caller's, so that the session loop can go on while it runs.
    fn publish_share_audio(
        &self,
    ) -> impl Future<Output = Result<(Self::ShareTrack, Self::ShareFeed), SdkError>> + Send + 'static;

    /// Leaves the room.
    fn close(self) -> impl Future<Output = ()> + Send;
}

/// The published share-audio track.
pub trait ShareAudioTrack: Send + 'static {
    /// Its id: the publication's the SDK has now, which a republish changes.
    fn id(&self) -> String;
    /// Whether the server still lets this participant publish a share's sound, as the SDK
    /// knows it now. When it does not, the server has taken the share's grant back.
    fn is_allowed(&self) -> bool;
    /// Whether this track is still among the participant's publications. After a full
    /// reconnect whose republish failed it is not, and its captures would feed nothing.
    fn is_published(&self) -> bool;
    /// Mutes it, and tells LiveKit: it stays published, and viewers see it muted.
    fn mute(&self);
    /// Unmutes it, and tells LiveKit.
    fn unmute(&self);
    /// Stops it sending what it is handed, without telling LiveKit anything: needed after a
    /// republish, when it is muted already and enabled all the same.
    fn disable(&self);
    /// Unpublishes it.
    fn unpublish(self) -> impl Future<Output = ()> + Send + 'static;
}

/// The way the share's mix reaches its published track.
pub trait ShareAudioFeed: Send + 'static {
    /// Hands one stereo frame of the mix to the track's audio source.
    fn send(&mut self, frame: &StereoFrame) -> impl Future<Output = ()> + Send;
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
    ///
    /// The frame must be handed over by the end of the first poll of the future returned;
    /// what follows may only wait for the SDK. The transmit task makes the call and that
    /// poll under the lock a mute takes (`transmit::TrackLock`), which is what keeps every
    /// frame ahead of the mute after it.
    fn send(&mut self, frame: GatedFrame<'_>) -> impl Future<Output = ()> + Send;
}
