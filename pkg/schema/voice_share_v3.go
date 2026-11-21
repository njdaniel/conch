package schema

import (
	"fmt"
	"io"
)

// VoicePresenceSchemaV3 identifies the presence snapshot with explicit 480p
// and 1440p share qualities. Only clients that negotiate it read these labels.
const VoicePresenceSchemaV3 = "conch.voice_presence.v3"

type VoiceShareResolutionV3 string

const (
	VoiceShareV3Resolution480p   VoiceShareResolutionV3 = "480p"
	VoiceShareV3Resolution720p   VoiceShareResolutionV3 = "720p"
	VoiceShareV3Resolution1080p  VoiceShareResolutionV3 = "1080p"
	VoiceShareV3Resolution1440p  VoiceShareResolutionV3 = "1440p"
	VoiceShareV3ResolutionSource VoiceShareResolutionV3 = "source"
)

type VoiceShareQualityV3 struct {
	Resolution VoiceShareResolutionV3 `json:"resolution"`
	FrameRate  int                    `json:"frame_rate"`
}

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

func (p VoiceParticipantV3) Validate() error {
	if p.ShareQuality != nil {
		if err := p.ShareQuality.Validate(); err != nil {
			return err
		}
	}
	return p.Legacy().Validate()
}

type VoicePresenceRoomV3 struct {
	Audience     *Audience            `json:"audience,omitempty"`
	Participants []VoiceParticipantV3 `json:"participants"`
}

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

type VoicePresenceV3 struct {
	Schema      string                `json:"schema"`
	ChannelID   int64                 `json:"channel_id"`
	Configured  bool                  `json:"configured"`
	Available   bool                  `json:"available"`
	Rooms       []VoicePresenceRoomV3 `json:"rooms"`
	ShareEnded  *VoiceShareEnded      `json:"share_ended,omitempty"`
	CameraEnded *VoiceCameraEnded     `json:"camera_ended,omitempty"`
}

func (p VoicePresenceV3) Validate() error {
	return validatePresence(p.Schema, VoicePresenceSchemaV3, p.ChannelID, p.Configured, p.Available, p.Rooms, p.ShareEnded, p.CameraEnded)
}

// VoiceShareRequestV2 is the optional claim body when the caller requests v3.
type VoiceShareRequestV2 struct {
	Quality *VoiceShareQualityV3 `json:"quality,omitempty"`
}

func (r VoiceShareRequestV2) Validate() error {
	if r.Quality != nil {
		return r.Quality.Validate()
	}
	return nil
}

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

func (s VoiceShareV3) Validate() error {
	if err := (VoiceShareV1{PrincipalID: s.PrincipalID, StartedAt: s.StartedAt, Audio: s.Audio}).Validate(); err != nil {
		return err
	}
	return s.Quality.Validate()
}
