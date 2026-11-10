package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func sharer(id int64, quality *schema.VoiceShareQuality, sound bool) schema.VoiceParticipantV2 {
	p := person(id, false)
	started := schema.NewTimestamp(time.Unix(1700000100+id, 0))
	p.Sharing, p.SharingAudio, p.ShareStartedAt, p.ShareQuality = true, sound, &started, quality
	return p
}

func TestVoiceShareText(t *testing.T) {
	hd := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution1080p, FrameRate: 60}
	sd := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution720p, FrameRate: 30}
	src := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolutionSource, FrameRate: 15}
	tests := []struct {
		name   string
		people []schema.VoiceParticipantV2
		want   string
	}{
		{"nobody shares", []schema.VoiceParticipantV2{person(1, false)}, ""},
		{"one, nothing declared", []schema.VoiceParticipantV2{sharer(1, nil, false)}, "sam is sharing a screen"},
		{"one with quality", []schema.VoiceParticipantV2{sharer(1, hd, false)}, "sam is sharing a screen (1080p · 60 fps)"},
		{"one with sound only", []schema.VoiceParticipantV2{sharer(1, nil, true)}, "sam is sharing a screen (with sound)"},
		{"one with quality and sound", []schema.VoiceParticipantV2{sharer(1, src, true)}, "sam is sharing a screen (source · 15 fps, with sound)"},
		{"two, one with sound", []schema.VoiceParticipantV2{sharer(1, nil, false), sharer(2, nil, true)}, "sam and val are sharing their screens (val with sound)"},
		{"two, nothing declared", []schema.VoiceParticipantV2{sharer(1, nil, false), sharer(2, nil, false)}, "sam and val are sharing their screens"},
		{"two with quality", []schema.VoiceParticipantV2{sharer(1, hd, false), sharer(2, sd, true)},
			"sam and val are sharing their screens (sam 1080p · 60 fps; val 720p · 30 fps, with sound)"},
		{"earliest started first whatever the id order", []schema.VoiceParticipantV2{sharer(2, nil, false), sharer(1, nil, false)}, "sam and val are sharing their screens"},
		{"three", []schema.VoiceParticipantV2{sharer(1, nil, false), sharer(2, nil, false), sharer(3, nil, true)},
			"sam, val and kit are sharing their screens (kit with sound)"},
		{"an unnamed sharer shows as a number", []schema.VoiceParticipantV2{sharer(9, nil, false)}, "#9 is sharing a screen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := voiceModelFor(t)
			m.members["general"] = map[int64]schema.ChannelMemberV1{
				1: {PrincipalID: 1, Name: "sam"}, 2: {PrincipalID: 2, Name: "val"}, 3: {PrincipalID: 3, Name: "kit"},
			}
			m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, tt.people...)})
			if got := m.shareText(); got != tt.want {
				t.Errorf("share text = %q, want %q", got, tt.want)
			}
			strip := m.voiceLine(200)
			people := strings.Join(m.voicePeople(200), "\n")
			if tt.want == "" {
				if strings.Contains(strip, "sharing") || strings.Contains(people, "sharing") {
					t.Errorf("share text shown with no sharer: %q / %q", strip, people)
				}
				return
			}
			if !strings.Contains(strip, tt.want) {
				t.Errorf("voice line %q lacks %q", strip, tt.want)
			}
			if !strings.Contains(people, tt.want) {
				t.Errorf("people pane %q lacks %q", people, tt.want)
			}
		})
	}
}

// A share a hostile server puts in a narrower room is not the channel's.
func TestVoiceShareTextIgnoresScopedRooms(t *testing.T) {
	m, _, _ := voiceModelFor(t)
	doc := presence(true, true, person(1, false))
	doc.Rooms = append(doc.Rooms, schema.VoicePresenceRoomV2{
		Audience:     &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4},
		Participants: []schema.VoiceParticipantV2{sharer(2, nil, false)},
	})
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: doc})
	if got := m.shareText(); got != "" {
		t.Errorf("share text = %q, want none", got)
	}
}

func TestVoiceShareTextFitsWidth(t *testing.T) {
	m, _, _ := voiceModelFor(t)
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, sharer(1, nil, false), sharer(2, nil, false))})
	for _, width := range []int{10, 20, 40, 60} {
		if got := m.voiceLine(width); cellWidth(got) > width {
			t.Errorf("voice line at %d is %d cells: %q", width, cellWidth(got), got)
		}
		for _, l := range m.voicePeople(width) {
			if cellWidth(l) > width {
				t.Errorf("people line at %d is %d cells: %q", width, cellWidth(l), l)
			}
		}
	}
}
