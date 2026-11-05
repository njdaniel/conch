package main

import (
	"strings"
	"testing"
	"time"
)

// statsAt is a stats line read at t: what the listener counted from one
// speaker in the second before it, and what it sent itself.
func statsAt(t time.Time, sent, frames, audible, hz int) voiceEvent {
	return voiceEvent{at: t, obj: map[string]any{
		"event":       "stats",
		"frames_sent": float64(sent),
		"speakers": []any{map[string]any{
			"speaker": "p7", "frames": float64(frames), "audible_frames": float64(audible), "dominant_hz": float64(hz),
		}},
		"mix": map[string]any{"audible_frames": float64(audible)},
	}}
}

// What heard and sending judge over a key held down (issue #1316): the
// speaker must be there in every second and audible over the window, but a
// starved machine may lose a burst of frames in one second.
func TestHeardAndSendingJudgeTheWholeWindow(t *testing.T) {
	t0 := time.Unix(1000, 0)
	two := func(a, b voiceEvent) []voiceEvent { return []voiceEvent{a, b} }
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	tests := []struct {
		name     string
		events   []voiceEvent
		wantHear string // "" for a pass, else a part of the error
		wantSend string
	}{
		{"every frame", two(statsAt(at(1), 100, 100, 100, 440), statsAt(at(2), 100, 100, 100, 440)), "", ""},
		{"the CI failure of #1316: 80 of 101 in one second, the other clean",
			two(statsAt(at(1), 100, 101, 80, 440), statsAt(at(2), 100, 100, 100, 440)), "", ""},
		{"a burst lost in both seconds, nine tenths over the window",
			two(statsAt(at(1), 79, 97, 82, 440), statsAt(at(2), 100, 101, 94, 440)), "", ""},
		{"half heard over the window",
			two(statsAt(at(1), 100, 100, 50, 440), statsAt(at(2), 100, 100, 50, 440)), "heard 100 of 200", ""},
		{"a whole second of silence",
			two(statsAt(at(1), 100, 100, 0, 440), statsAt(at(2), 100, 100, 100, 440)), "some of them audible", ""},
		{"too few frames in a second",
			two(statsAt(at(1), 100, 49, 49, 440), statsAt(at(2), 100, 100, 100, 440)), "want at least 50", ""},
		{"the wrong tone",
			two(statsAt(at(1), 100, 100, 100, 880), statsAt(at(2), 100, 100, 100, 440)), "hears 880 Hz", ""},
		{"sent too little over the window",
			two(statsAt(at(1), 70, 100, 100, 440), statsAt(at(2), 80, 100, 100, 440)), "", "in 2 seconds, want at least 160"},
		{"sent nothing in a second",
			two(statsAt(at(1), 0, 100, 100, 440), statsAt(at(2), 100, 100, 100, 440)), "", "want some"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &voiceClient{
				h:      &harness{secrets: map[string]string{}, rooms: map[string][]string{}},
				who:    &person{name: "ann", id: 1},
				events: tc.events,
			}
			check := func(err error, want string) {
				t.Helper()
				switch {
				case want == "" && err != nil:
					t.Fatalf("unexpected failure: %v", err)
				case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
					t.Fatalf("got %v, want an error containing %q", err, want)
				}
			}
			check(c.heard("test", t0, at(3), 2, map[string]int{"p7": 440}), tc.wantHear)
			check(c.sending("test", t0, at(3), true), tc.wantSend)
		})
	}
}
