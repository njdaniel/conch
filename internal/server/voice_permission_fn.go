package server

import (
	"context"

	"github.com/njdaniel/conch/internal/server/livekit"
)

// publishInput is what a caller of setPublishPermission knows about the
// participant it writes for.
//
// canPublish is the person's own grant, never more: the room's
// (voiceRoomPublishes) where the caller knows the participant holds it, else
// what LiveKit last reported, so that a listen-only participant is never made
// a publisher by losing a share. It means nothing for a companion or a viewer,
// which the function never lets publish a microphone.
//
// canSubscribe is a companion's or viewer's own, as the pass listed it or as
// GetParticipant answered, false when unknown, so that a publish-only
// participant never gains the right to receive. It means nothing for a person,
// whose permission always receives (it is what the join token grants).
//
// share says the caller is granting the room's screen share to this
// participant (a claim), not withdrawing it. Every withdrawal and rollback
// of the share passes false: they are made because the person does not, or
// must no longer, hold the share, and a caller that is taking back a grant
// while the claim is still recorded (a room rotated under it) must not be
// handed the share again by a read of that claim. A write the camera makes (a
// camera's grant, its stop, a pass's camera withdrawal) is not about the
// share, and passes what the person holds now: shareHeldLocked, read under
// the room's claims, the one place that term is worked out for them, so a
// camera's stop never drops a share (issue #1241).
//
// A listen-only person (canPublish false) is written the join token's own
// listen-only grant: canPublish false with the microphone named, which LiveKit
// reads as nothing to publish (docs/design/webcam-video.md §4.3's "none";
// decided in #1241, so a withdrawal is exactly what the token granted).
//
// audio says the share's holder has consented to their companion publishing
// the share's sound (issue #898): the companion is granted screen_share_audio
// beside the screen. It means nothing without share, and nothing for a
// person, whose share grant always carries the sound (VoiceShareV1.audio).
// Every withdrawal passes false; the consent is read, under the room's
// claims, from the share it belongs to (voiceShare.companionAudio: a consent
// naming the companion, issue #1853), never from a
// snapshot.
type publishInput struct {
	canPublish   bool
	canSubscribe bool
	share        bool
	audio        bool
}

// publishPermission is the one place a permission conchd writes for a
// participant is worked out (ADR-015 decision 5, docs/design/webcam-video.md
// §4.3). It is pure: share and camera are what the participant holds, each
// term adds only its own sources, so the camera never takes the share's
// sources away and the share never takes the camera's.
//
//   - a person who may not publish: no share and no camera, whatever they hold
//     (the bundled form with neither is the join token's, which LiveKit reads
//     as nothing to publish when canPublish is false);
//   - a person in voice: the microphone, with the screen share's two sources
//     while they hold the share, and the camera while they hold a camera claim;
//   - a companion (p<id>.screen): the screen while the person holds the share,
//     and the share's sound as well while they have consented to it (issue
//     #898); nothing otherwise. It never gets the camera until the desktop
//     phase (§9), so camera is not consulted for it;
//   - a viewer (p<id>.view): nothing.
func publishPermission(role voiceRole, in publishInput, camera bool) livekit.Permission {
	switch role {
	case voiceRolePerson:
		if !in.canPublish {
			return livekit.Permission{}
		}
		return livekit.Permission{CanPublish: true, ScreenShare: in.share, Camera: camera}
	case voiceRoleScreen:
		switch {
		case in.share && in.audio:
			return livekit.SourcePermission(in.canSubscribe, true, livekit.SourceScreenShare, livekit.SourceScreenShareAudio)
		case in.share:
			return livekit.SourcePermission(in.canSubscribe, true, livekit.SourceScreenShare)
		}
	}
	return livekit.SourcePermission(in.canSubscribe, false)
}

// setPublishPermission writes the permission of principalID's participant in
// role to roomName: the only call of LiveKit's UpdateParticipant in this
// package (TestUpdateParticipantStaysInThePermissionFunction). LiveKit
// replaces a permission whole, so every grant, withdrawal and rollback is one
// call to this, and a change to one term cannot take another's away.
//
// The caller holds the room's claims (voiceRoomState.claims), which is what
// serializes every write of one participant's permission. The camera term is
// read here, at the moment of writing, from the person's current claims in r
// (cameraHeldLocked, issue #1241), never from a snapshot a pass carried in,
// so that a share's grant or withdrawal never takes a camera down, and one
// claimed between a pass's plan and its withdrawal stands. r is the room the
// caller holds the claims of, passed down and never looked up again by name:
// a late write for a room a rotation forgot must read that room's claims
// (none: cameraHeldLocked is false once it is retired), not its successor's.
// found is false, with a nil error, when nobody by that identity is in the
// room.
func (p *voicePoller) setPublishPermission(ctx context.Context, r *voiceRoomState, principalID int64, role voiceRole, in publishInput) (found bool, err error) {
	p.mu.Lock()
	camera := p.cameraHeldLocked(r, principalID)
	p.mu.Unlock()
	return p.s.lk.UpdateParticipant(ctx, r.name, voiceParticipantIdentity(principalID, role), publishPermission(role, in, camera))
}
