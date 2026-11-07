package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published v1 voice shapes are closed to readers (issue #1507, PR
// #1526). The browser's generated validators (web/src/schema/validate.ts,
// object()) refuse a key the type does not have, so a v1 document that
// carried a field added later would be dropped by every open tab from before
// the field: parsePresence returns null and the frame is lost. A published v1
// shape therefore gains no field at all; a new field is a new version
// (VoicePresenceV2, VoiceShareV2).
//
// The types below are copies of the v1 shapes as main published them at
// 93453e8b (2026-11-07), frozen here on purpose and never to be edited:
// they stand for every reader built before this change. Every v1 fixture on
// disk, including the degraded two-sharer document and any fixture a later
// change adds to a v1 family, must decode into them with unknown fields
// refused and encode back byte for byte. A fixture that fails here is one an
// older browser would drop, however the live types read it.

type frozenAudienceV1 struct {
	Kind         string  `json:"kind"`
	NetID        int64   `json:"net_id,omitempty"`
	PrincipalIDs []int64 `json:"principal_ids,omitempty"`
}

type frozenVoiceParticipantV1 struct {
	PrincipalID  int64     `json:"principal_id"`
	CanPublish   bool      `json:"can_publish"`
	Transmitting bool      `json:"transmitting"`
	JoinedAt     Timestamp `json:"joined_at"`
	Sharing      bool      `json:"sharing,omitempty"`
	SharingAudio bool      `json:"sharing_audio,omitempty"`
	Camera       bool      `json:"camera,omitempty"`
}

type frozenVoicePresenceRoomV1 struct {
	Audience     *frozenAudienceV1          `json:"audience,omitempty"`
	Participants []frozenVoiceParticipantV1 `json:"participants"`
}

type frozenVoiceEndedV1 struct {
	Reason    string    `json:"reason"`
	StartedAt Timestamp `json:"started_at"`
	EndedAt   Timestamp `json:"ended_at"`
}

type frozenVoicePresenceV1 struct {
	Schema      string                      `json:"schema"`
	ChannelID   int64                       `json:"channel_id"`
	Configured  bool                        `json:"configured"`
	Available   bool                        `json:"available"`
	Rooms       []frozenVoicePresenceRoomV1 `json:"rooms"`
	ShareEnded  *frozenVoiceEndedV1         `json:"share_ended,omitempty"`
	CameraEnded *frozenVoiceEndedV1         `json:"camera_ended,omitempty"`
}

type frozenVoiceShareV1 struct {
	PrincipalID int64     `json:"principal_id"`
	StartedAt   Timestamp `json:"started_at"`
	Audio       bool      `json:"audio"`
}

type frozenVoiceCameraV1 struct {
	PrincipalID int64     `json:"principal_id"`
	StartedAt   Timestamp `json:"started_at"`
}

// TestVoiceV1FixturesReadByFrozenV1Types: every fixture of a published v1
// voice family decodes into the frozen v1 type with unknown fields refused,
// and encodes back to the same bytes. The families are matched as the web
// test matches them (<family>.json and <family>-*.json), so a fixture added
// later to a v1 family is checked without a change here. A v2 fixture is not
// in a v1 family: voice-presence-v2.json does not start with
// "voice-presence-v1-".
func TestVoiceV1FixturesReadByFrozenV1Types(t *testing.T) {
	families := []struct {
		family string
		atMost int // fixtures the family had when frozen; a guard that the glob finds them
		new    func() any
	}{
		{family: "voice-presence-v1", atMost: 7, new: func() any { return new(frozenVoicePresenceV1) }},
		{family: "voice-share-v1", atMost: 1, new: func() any { return new(frozenVoiceShareV1) }},
		{family: "voice-camera-v1", atMost: 1, new: func() any { return new(frozenVoiceCameraV1) }},
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range families {
		t.Run(fam.family, func(t *testing.T) {
			var files []string
			for _, e := range entries {
				name := e.Name()
				if name == fam.family+".json" || (strings.HasPrefix(name, fam.family+"-") && strings.HasSuffix(name, ".json")) {
					files = append(files, name)
				}
			}
			if len(files) < fam.atMost {
				t.Fatalf("found %d fixtures of %s, want at least the %d it had when frozen: %v", len(files), fam.family, fam.atMost, files)
			}
			for _, file := range files {
				t.Run(file, func(t *testing.T) {
					golden, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this package's own testdata listing
					if err != nil {
						t.Fatal(err)
					}
					value := fam.new()
					dec := json.NewDecoder(bytes.NewReader(golden))
					dec.DisallowUnknownFields()
					if err := dec.Decode(value); err != nil {
						t.Fatalf("a reader built before this change refuses %s: %v\nA published v1 shape gains no field; put it in a v2 shape instead.", file, err)
					}
					got, err := json.MarshalIndent(value, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, '\n')
					if !bytes.Equal(got, golden) {
						t.Errorf("%s read by the frozen v1 type and written again differs:\ngot:\n%s\nwant:\n%s", file, got, golden)
					}
				})
			}
		})
	}
}
