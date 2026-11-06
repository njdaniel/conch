package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func playbackStatsAt(at time.Time, sent int, frames map[string]int, mixRMS float64) voiceEvent {
	tones := map[string]int{"p7": toneAnn, "p7?screen": toneShare}
	speakers := []any{}
	for id, count := range frames {
		speakers = append(speakers, map[string]any{
			"speaker": id, "frames": float64(count), "audible_frames": float64(count), "dominant_hz": float64(tones[id]),
		})
	}
	return voiceEvent{at: at, obj: map[string]any{
		"event": "stats", "frames_sent": float64(sent), "speakers": speakers,
		"mix": map[string]any{"frames": 100.0, "audible_frames": 100.0, "rms": mixRMS},
	}}
}

func TestPlaybackHeardRejectsDoublePlayback(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(second int) time.Time { return t0.Add(time.Duration(second) * time.Second) }
	singleRMS := 0.25 / math.Sqrt2
	tests := []struct {
		name    string
		want    map[string]int
		frames  map[string]int
		mixRMS  float64
		talking bool
		wantErr string
	}{
		{"one share", map[string]int{"p7?screen": toneShare}, map[string]int{"p7?screen": 100}, singleRMS, false, ""},
		{"two share tracks with a normal per-speaker RMS", map[string]int{"p7?screen": toneShare}, map[string]int{"p7?screen": 200}, singleRMS, false, "counted 400 frames"},
		{"a doubled share mix with a normal frame count", map[string]int{"p7?screen": toneShare}, map[string]int{"p7?screen": 100}, 2 * singleRMS, false, "mix RMS"},
		{"one microphone", map[string]int{"p7": toneAnn}, map[string]int{"p7": 100}, singleRMS, true, ""},
		{"two microphone streams", map[string]int{"p7": toneAnn}, map[string]int{"p7": 200}, singleRMS, true, "counted 400 frames"},
		{"one copy of microphone and share", map[string]int{"p7": toneAnn, "p7?screen": toneShare}, map[string]int{"p7": 100, "p7?screen": 100}, math.Sqrt2 * singleRMS, true, ""},
		{"a doubled mix of microphone and share", map[string]int{"p7": toneAnn, "p7?screen": toneShare}, map[string]int{"p7": 100, "p7?screen": 100}, 2 * math.Sqrt2 * singleRMS, true, "mix RMS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listener := &voiceClient{
				h:   &harness{secrets: map[string]string{}, rooms: map[string][]string{}},
				who: &person{name: "h8", id: 8},
				events: []voiceEvent{
					playbackStatsAt(at(1), 0, tc.frames, tc.mixRMS),
					playbackStatsAt(at(2), 0, tc.frames, tc.mixRMS),
				},
			}
			publisher := &voiceClient{
				who: &person{name: "h7", id: 7},
				events: []voiceEvent{
					playbackStatsAt(at(1), 100, nil, 0),
					playbackStatsAt(at(2), 100, nil, 0),
				},
			}
			err := playbackHeard(listener, "test", t0, at(3), tc.want, publisher, tc.talking)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
