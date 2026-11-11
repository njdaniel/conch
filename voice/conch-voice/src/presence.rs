//! Presence: who is connected and who is talking, as `conchd` has decided and logged it
//! (`docs/design/conch-voice.md` §7). It comes from `conchd`'s presence socket, not from
//! LiveKit, and it carries no token and no room name.
//!
//! One task follows the socket for as long as the client runs, whatever the voice
//! connection is doing. When the socket ends it is opened again after a wait that doubles
//! from one second to thirty. A refusal that no retry can change ends the task: the session
//! request meets the same refusal, and the connection policy stops the client with the
//! reason. One end is news in itself: `conchd` closing the socket for a moderator's
//! voice-only removal (`voice_removed`, issue #1409), which the task passes on as
//! [`Presence::VoiceRemoved`] and then stops, since the session stops on it.

use std::time::Duration;

use conch_voice_api::{Client, Error as ApiError, PresenceEvent, StreamEnd, VoicePresenceV2};
use tokio::sync::mpsc;

/// What the presence task tells the session.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Presence {
    /// What is known about the room changed.
    Roster(Roster),
    /// A moderator removed this person from voice only. They are still a member, so
    /// `conchd` would grant the next session; the client stops instead of asking.
    VoiceRemoved,
}

/// One participant of the channel's room, as shown.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Participant {
    /// The principal's id. It is shown as `p<id>`.
    pub principal_id: i64,
    /// Whether the participant may transmit.
    pub can_publish: bool,
    /// Whether `conchd` has the participant as transmitting.
    pub transmitting: bool,
}

/// What is known about the channel's voice room.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Roster {
    /// `conchd`'s latest snapshot.
    Known {
        /// Whether this `conchd` has LiveKit configured at all.
        configured: bool,
        /// Whether LiveKit answered `conchd` on its last pass.
        available: bool,
        /// Everyone connected to the channel's room, ordered by id.
        participants: Vec<Participant>,
    },
    /// The presence socket is not open, so nothing is known at present.
    Unknown,
}

impl Roster {
    fn from_snapshot(snapshot: &VoicePresenceV2) -> Self {
        // V4 shows the channel's own room and no other (nets are V5).
        let participants = snapshot
            .channel_room()
            .map(|room| {
                room.participants
                    .iter()
                    .map(|participant| Participant {
                        principal_id: participant.principal_id,
                        can_publish: participant.can_publish,
                        transmitting: participant.transmitting,
                    })
                    .collect()
            })
            .unwrap_or_default();
        Roster::Known {
            configured: snapshot.configured,
            available: snapshot.available,
            participants,
        }
    }
}

/// How long to wait before opening the socket again.
#[derive(Debug, Clone, Copy)]
pub struct PresenceTimings {
    /// The first wait.
    pub first_wait: Duration,
    /// The longest wait.
    pub longest_wait: Duration,
}

impl Default for PresenceTimings {
    fn default() -> Self {
        Self {
            first_wait: Duration::from_secs(1),
            longest_wait: Duration::from_secs(30),
        }
    }
}

/// Starts the task that follows `channel`'s presence, read as v2 (several sharers are
/// listed whole; the helper shows no share text, so it keeps only who is there and who is
/// talking).
///
/// Each change of the roster is sent once; the task ends when the receiver is dropped, or after it has said
/// [`Presence::VoiceRemoved`].
#[must_use]
pub fn spawn(
    client: Client,
    channel: String,
    timings: PresenceTimings,
) -> mpsc::UnboundedReceiver<Presence> {
    let (rosters, receiver) = mpsc::unbounded_channel();
    tokio::spawn(follow(client, channel, timings, rosters));
    receiver
}

/// Whether trying again could change the answer.
fn is_final(error: &ApiError) -> bool {
    matches!(
        error,
        ApiError::Unauthenticated(_)
            | ApiError::VoiceRequiresAuth(_)
            | ApiError::Forbidden(_)
            | ApiError::ChannelNotFound(_)
            | ApiError::InvalidChannel { .. }
            | ApiError::InvalidServer { .. }
            | ApiError::Redirected { .. }
    )
}

async fn follow(
    client: Client,
    channel: String,
    timings: PresenceTimings,
    rosters: mpsc::UnboundedSender<Presence>,
) {
    let mut last: Option<Roster> = None;
    let mut wait = timings.first_wait;
    loop {
        let mut say = |roster: Roster| {
            if last.as_ref() == Some(&roster) {
                return true;
            }
            last = Some(roster.clone());
            rosters.send(Presence::Roster(roster)).is_ok()
        };
        match client.presence_stream(&channel).await {
            Ok(mut stream) => loop {
                let event = tokio::select! {
                    event = stream.next() => event,
                    () = rosters.closed() => return,
                };
                match event {
                    Ok(PresenceEvent::Snapshot(snapshot)) => {
                        wait = timings.first_wait;
                        if !say(Roster::from_snapshot(&snapshot)) {
                            return;
                        }
                    }
                    Ok(PresenceEvent::Ended(StreamEnd::VoiceRemoved)) => {
                        // Not a loss of the socket to be made good: the session stops on
                        // this, and nothing here subscribes again on its own.
                        log::debug!("the presence socket ended: removed from voice");
                        say(Roster::Unknown);
                        let _ = rosters.send(Presence::VoiceRemoved);
                        return;
                    }
                    Ok(PresenceEvent::Ended(end)) => {
                        log::debug!("the presence socket ended: {end:?}");
                        break;
                    }
                    Err(error) => {
                        log::debug!("the presence socket failed: {error}");
                        break;
                    }
                }
            },
            Err(error) if is_final(&error) => {
                log::debug!("presence is not available: {error}");
                say(Roster::Unknown);
                return;
            }
            Err(error) => log::debug!("the presence socket could not be opened: {error}"),
        }
        if !say(Roster::Unknown) {
            return;
        }
        tokio::select! {
            () = tokio::time::sleep(wait) => {}
            () = rosters.closed() => return,
        }
        wait = wait.saturating_mul(2).min(timings.longest_wait);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The v2 document of two sharers, `pkg/schema/testdata/voice-presence-v2.json`'s
    /// room: the roster lists everyone, sharers or not, by id, and is the same as it
    /// would be with nobody sharing.
    #[test]
    fn a_roster_from_a_room_with_two_sharers_lists_everyone() {
        let text = include_str!("../../../pkg/schema/testdata/voice-presence-v2.json");
        let snapshot: VoicePresenceV2 = serde_json::from_str(text).unwrap();
        snapshot.validate().unwrap();
        assert_eq!(
            Roster::from_snapshot(&snapshot),
            Roster::Known {
                configured: true,
                available: true,
                participants: vec![
                    Participant {
                        principal_id: 3,
                        can_publish: true,
                        transmitting: false
                    },
                    Participant {
                        principal_id: 7,
                        can_publish: true,
                        transmitting: true
                    },
                    Participant {
                        principal_id: 9,
                        can_publish: true,
                        transmitting: false
                    },
                ],
            }
        );
    }
}
