package schema

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVoiceShareQualityV3Validate(t *testing.T) {
	tests := []struct {
		name    string
		quality VoiceShareQualityV3
		wantErr string
	}{
		{"480p", VoiceShareQualityV3{VoiceShareV3Resolution480p, 15}, ""},
		{"720p", VoiceShareQualityV3{VoiceShareV3Resolution720p, 30}, ""},
		{"1080p", VoiceShareQualityV3{VoiceShareV3Resolution1080p, 60}, ""},
		{"1440p", VoiceShareQualityV3{VoiceShareV3Resolution1440p, 60}, ""},
		{"source", VoiceShareQualityV3{VoiceShareV3ResolutionSource, 30}, ""},
		{"unknown resolution", VoiceShareQualityV3{"2160p", 30}, "resolution"},
		{"missing resolution", VoiceShareQualityV3{"", 30}, "resolution"},
		{"unsupported rate", VoiceShareQualityV3{VoiceShareV3Resolution480p, 24}, "frame_rate"},
		{"missing rate", VoiceShareQualityV3{VoiceShareV3Resolution1440p, 0}, "frame_rate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.quality.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestVoiceShareQualityV3Legacy(t *testing.T) {
	tests := []struct {
		resolution VoiceShareResolutionV3
		frameRate  int
		want       VoiceShareResolution
	}{
		{VoiceShareV3Resolution480p, 15, VoiceShareResolutionSource},
		{VoiceShareV3Resolution720p, 30, VoiceShareResolution720p},
		{VoiceShareV3Resolution1080p, 60, VoiceShareResolution1080p},
		{VoiceShareV3Resolution1440p, 60, VoiceShareResolutionSource},
		{VoiceShareV3ResolutionSource, 30, VoiceShareResolutionSource},
	}
	for _, tt := range tests {
		t.Run(string(tt.resolution), func(t *testing.T) {
			quality := VoiceShareQualityV3{tt.resolution, tt.frameRate}
			want := VoiceShareQuality{tt.want, tt.frameRate}
			if got := quality.Legacy(); got != want {
				t.Errorf("Legacy() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestVoiceParticipantV3Legacy(t *testing.T) {
	joined := NewTimestamp(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	started := NewTimestamp(joined.Time().Add(time.Second))
	tests := []struct {
		name    string
		quality *VoiceShareQualityV3
		want    *VoiceShareQuality
	}{
		{"undeclared", nil, nil},
		{"1440p", &VoiceShareQualityV3{VoiceShareV3Resolution1440p, 60}, &VoiceShareQuality{VoiceShareResolutionSource, 60}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := VoiceParticipantV3{
				PrincipalID: 3, CanPublish: true, Transmitting: true, JoinedAt: joined,
				Sharing: true, SharingAudio: true, ShareStartedAt: &started, ShareQuality: tt.quality, Camera: true,
			}
			want := VoiceParticipantV2{
				PrincipalID: 3, CanPublish: true, Transmitting: true, JoinedAt: joined,
				Sharing: true, SharingAudio: true, ShareStartedAt: &started, ShareQuality: tt.want, Camera: true,
			}
			if got := p.Legacy(); !reflect.DeepEqual(got, want) {
				t.Errorf("Legacy() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestDecodeVoiceShareRequestV2(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		quality *VoiceShareQualityV3
		wantErr string
	}{
		{name: "no body"},
		{name: "whitespace", body: " \n\t"},
		{name: "empty object", body: "{}"},
		{name: "null quality", body: `{"quality":null}`},
		{name: "480p", body: `{"quality":{"resolution":"480p","frame_rate":15}}`, quality: &VoiceShareQualityV3{VoiceShareV3Resolution480p, 15}},
		{name: "1440p", body: `{"quality":{"resolution":"1440p","frame_rate":60}}`, quality: &VoiceShareQualityV3{VoiceShareV3Resolution1440p, 60}},
		{name: "null body", body: "null", wantErr: "JSON object"},
		{name: "unknown top-level field", body: `{"principal_id":3}`, wantErr: "unknown field"},
		{name: "unknown quality field", body: `{"quality":{"resolution":"480p","frame_rate":30,"bitrate":2}}`, wantErr: "unknown field"},
		{name: "trailing object", body: "{} {}", wantErr: "one JSON object"},
		{name: "truncated body", body: `{"quality":`, wantErr: "unexpected EOF"},
		{name: "invalid resolution", body: `{"quality":{"resolution":"2160p","frame_rate":30}}`, wantErr: "resolution"},
		{name: "invalid frame rate", body: `{"quality":{"resolution":"1440p","frame_rate":24}}`, wantErr: "frame_rate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceShareRequestV2(strings.NewReader(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("DecodeVoiceShareRequestV2() = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Quality, tt.quality) {
				t.Errorf("DecodeVoiceShareRequestV2() = %+v, want quality %+v", got, tt.quality)
			}
		})
	}
}
