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
					playbackStatsAt(t0, 0, nil, 0),
					playbackStatsAt(at(1), 0, tc.frames, tc.mixRMS),
					playbackStatsAt(at(2), 0, tc.frames, tc.mixRMS),
				},
			}
			publisher := &voiceClient{
				who: &person{name: "h7", id: 7},
				events: []voiceEvent{
					playbackStatsAt(t0, 0, nil, 0),
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

func TestPlaybackHeardReportingIntervals(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }
	type report struct {
		seconds float64
		frames  int
	}
	tests := []struct {
		name      string
		listener  []report
		publisher []report // nil means a share, whose source rate is known
		copies    int
		wantErr   string
	}{
		{
			name: "delayed share report", copies: 1,
			listener: []report{{0, 0}, {1, 100}, {2.6, 160}},
		},
		{
			name: "delayed doubled share report", copies: 2, wantErr: "possible double playback",
			listener: []report{{0, 0}, {1, 100}, {2.6, 160}},
		},
		{
			name: "delayed receiver microphone report", copies: 1,
			listener:  []report{{0, 0}, {1, 100}, {2.6, 160}},
			publisher: []report{{0, 0}, {1, 100}, {2, 100}},
		},
		{
			name: "independent phases and shorter publisher intervals", copies: 1,
			listener:  []report{{0.1, 0}, {1.1, 100}, {2.7, 160}},
			publisher: []report{{0.2, 0}, {0.8, 60}, {1.4, 60}, {2, 60}, {2.6, 60}},
		},
		{
			name: "independent phases and delayed publisher", copies: 1,
			listener:  []report{{0.1, 0}, {1.1, 100}, {2.1, 100}},
			publisher: []report{{0.2, 0}, {1.8, 160}, {2.8, 100}},
		},
		{
			name: "independent intervals doubled microphone", copies: 2, wantErr: "possible double playback",
			listener:  []report{{0.1, 0}, {1.1, 100}, {2.1, 100}},
			publisher: []report{{0.2, 0}, {1.8, 160}, {2.8, 100}},
		},
		{
			name: "bounded delivery backlog", copies: 1,
			listener: []report{{0, 0}, {1, 120}, {2.6, 160}},
		},
		{
			name: "excess delivery backlog", copies: 1, wantErr: "possible double playback",
			listener: []report{{0, 0}, {1, 166}, {2.6, 160}},
		},
		{
			name: "no publisher baseline", copies: 1, wantErr: "no whole publisher reporting interval",
			listener:  []report{{0, 0}, {1, 100}, {2, 100}},
			publisher: []report{{2, 100}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, tone := "p7?screen", toneShare
			if tc.publisher != nil {
				id, tone = "p7", toneAnn
			}
			listener := &voiceClient{
				h:   &harness{secrets: map[string]string{}, rooms: map[string][]string{}},
				who: &person{name: "h8", id: 8},
			}
			for _, r := range tc.listener {
				listener.events = append(listener.events, playbackStatsAt(at(r.seconds), 0,
					map[string]int{id: r.frames * tc.copies}, 0.25/math.Sqrt2))
			}
			publisher := &voiceClient{who: &person{name: "h7", id: 7}}
			for _, r := range tc.publisher {
				publisher.events = append(publisher.events, playbackStatsAt(at(r.seconds), r.frames, nil, 0))
			}
			err := playbackHeard(listener, "test", t0, at(3), map[string]int{id: tone}, publisher, tc.publisher != nil)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPlaybackStatsWholeIntervals(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }
	c := &voiceClient{events: []voiceEvent{
		playbackStatsAt(at(0), 0, nil, 0),
		playbackStatsAt(at(1.6), 160, nil, 0),             // straddles from, despite ending more than a second after it
		{at: at(2), obj: map[string]any{"event": "self"}}, // does not reset the stats baseline
		playbackStatsAt(at(2.6), 100, nil, 0),
		playbackStatsAt(at(4.2), 160, nil, 0),
		playbackStatsAt(at(5.2), 100, nil, 0), // straddles to
	}}
	stats, elapsed := playbackStats(c, at(0.5), at(5))
	if len(stats) != 2 || elapsed != 2600*time.Millisecond {
		t.Fatalf("got %d reports over %s, want 2 over 2.6 s", len(stats), elapsed)
	}
	if number(stats[0]["frames_sent"]) != 100 || number(stats[1]["frames_sent"]) != 160 {
		t.Fatalf("selected wrong reporting intervals: %v", stats)
	}
}
