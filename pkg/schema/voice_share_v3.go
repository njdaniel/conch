package schema

import (
	"fmt"
	"io"
)

// VoicePresenceSchemaV3 identifies the presence snapshot with explicit 480p
// and 1440p share qualities. Only clients that negotiate it read these labels.
const VoicePresenceSchemaV3 = "conch.voice_presence.v3"

// VoiceShareResolutionV3 is the declared resolution in v3 presence and claims.
type VoiceShareResolutionV3 string

// Voice share resolutions available to v3 clients.
const (
	VoiceShareV3Resolution480p   VoiceShareResolutionV3 = "480p"
	VoiceShareV3Resolution720p   VoiceShareResolutionV3 = "720p"
	VoiceShareV3Resolution1080p  VoiceShareResolutionV3 = "1080p"
	VoiceShareV3Resolution1440p  VoiceShareResolutionV3 = "1440p"
	VoiceShareV3ResolutionSource VoiceShareResolutionV3 = "source"
)

// VoiceShareQualityV3 is the declared resolution and frame rate of a share.
type VoiceShareQualityV3 struct {
	Resolution VoiceShareResolutionV3 `json:"resolution"`
	FrameRate  int                    `json:"frame_rate"`
}

// Validate checks the v3 resolution and the frame rate of 15, 30 or 60 fps.
func (q VoiceShareQualityV3) Validate() error {
	switch q.Resolution {
	case VoiceShareV3Resolution480p, VoiceShareV3Resolution720p, VoiceShareV3Resolution1080p,
		VoiceShareV3Resolution1440p, VoiceShareV3ResolutionSource:
		return q.Legacy().Validate()
	default:
		return fmt.Errorf("schema: unknown v3 voice share resolution %q", q.Resolution)
	}
}

// Legacy is the quality label sent to readers of v2 presence. The older
// vocabulary has no exact equivalent for either new resolution.
func (q VoiceShareQualityV3) Legacy() VoiceShareQuality {
	resolution := VoiceShareResolution(q.Resolution)
	if !resolution.Valid() {
		resolution = VoiceShareResolutionSource
	}
	return VoiceShareQuality{Resolution: resolution, FrameRate: q.FrameRate}
}

// VoiceParticipantV3 is a connected principal with a v3 quality label when declared.
type VoiceParticipantV3 struct {
	PrincipalID    int64                `json:"principal_id"`
	CanPublish     bool                 `json:"can_publish"`
	Transmitting   bool                 `json:"transmitting"`
	JoinedAt       Timestamp            `json:"joined_at"`
	Sharing        bool                 `json:"sharing,omitempty"`
	SharingAudio   bool                 `json:"sharing_audio,omitempty"`
	ShareStartedAt *Timestamp           `json:"share_started_at,omitempty"`
	ShareQuality   *VoiceShareQualityV3 `json:"share_quality,omitempty"`
	Camera         bool                 `json:"camera,omitempty"`
}

// Legacy converts the participant's share quality for v2 presence.
func (p VoiceParticipantV3) Legacy() VoiceParticipantV2 {
	part := VoiceParticipantV2{
		PrincipalID: p.PrincipalID, CanPublish: p.CanPublish, Transmitting: p.Transmitting,
		JoinedAt: p.JoinedAt, Sharing: p.Sharing, SharingAudio: p.SharingAudio,
		ShareStartedAt: p.ShareStartedAt, Camera: p.Camera,
	}
	if p.ShareQuality != nil {
		quality := p.ShareQuality.Legacy()
		part.ShareQuality = &quality
	}
	return part
}

// Validate checks the participant's presence fields and any declared v3 quality.
func (p VoiceParticipantV3) Validate() error {
	if p.ShareQuality != nil {
		if err := p.ShareQuality.Validate(); err != nil {
			return err
		}
	}
	return p.Legacy().Validate()
}

// VoicePresenceRoomV3 is a room's audience and participants as v3 presence shows them.
type VoicePresenceRoomV3 struct {
	Audience     *Audience            `json:"audience,omitempty"`
	Participants []VoiceParticipantV3 `json:"participants"`
}

// Validate checks the audience and that each participant is valid and unique.
func (r VoicePresenceRoomV3) Validate() error {
	if r.Audience != nil {
		if err := r.Audience.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[int64]struct{}, len(r.Participants))
	for _, p := range r.Participants {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seen[p.PrincipalID]; dup {
			return fmt.Errorf("schema: voice room lists principal %d more than once", p.PrincipalID)
		}
		seen[p.PrincipalID] = struct{}{}
	}
	return nil
}

func (r VoicePresenceRoomV3) roomAudience() *Audience { return r.Audience }

// VoicePresenceV3 is the channel's voice snapshot with v3 share quality labels.
type VoicePresenceV3 struct {
	Schema      string                `json:"schema"`
	ChannelID   int64                 `json:"channel_id"`
	Configured  bool                  `json:"configured"`
	Available   bool                  `json:"available"`
	Rooms       []VoicePresenceRoomV3 `json:"rooms"`
	ShareEnded  *VoiceShareEnded      `json:"share_ended,omitempty"`
	CameraEnded *VoiceCameraEnded     `json:"camera_ended,omitempty"`
}

// Validate checks the snapshot's version, channel, rooms and end notices.
func (p VoicePresenceV3) Validate() error {
	return validatePresence(p.Schema, VoicePresenceSchemaV3, p.ChannelID, p.Configured, p.Available, p.Rooms, p.ShareEnded, p.CameraEnded)
}

// VoiceShareRequestV2 is the optional body of POST /v1/channels/{channel}/voice/share
// with ?schema=conch.voice_presence.v3.
type VoiceShareRequestV2 struct {
	Quality *VoiceShareQualityV3 `json:"quality,omitempty"`
}

// Validate checks the declared quality when present.
func (r VoiceShareRequestV2) Validate() error {
	if r.Quality != nil {
		return r.Quality.Validate()
	}
	return nil
}

// DecodeVoiceShareRequestV2 applies the same strict decoding as
// DecodeVoiceShareRequestV1 with the v3 quality vocabulary.
func DecodeVoiceShareRequestV2(r io.Reader) (VoiceShareRequestV2, error) {
	return decodeVoiceShareRequest[VoiceShareRequestV2](r)
}

// VoiceShareV3 is the answer to a v3 claim with a declared quality.
type VoiceShareV3 struct {
	PrincipalID int64               `json:"principal_id"`
	StartedAt   Timestamp           `json:"started_at"`
	Audio       bool                `json:"audio"`
	Quality     VoiceShareQualityV3 `json:"quality"`
}

// Validate checks the holder, start time and declared v3 quality.
func (s VoiceShareV3) Validate() error {
	if err := (VoiceShareV1{PrincipalID: s.PrincipalID, StartedAt: s.StartedAt, Audio: s.Audio}).Validate(); err != nil {
		return err
	}
	return s.Quality.Validate()
}
