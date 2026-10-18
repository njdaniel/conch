package main

import "testing"

// TestNamesParticipant: an lk log line names a member only under a key that
// names a participant, with the member's identity as the whole value. The
// lines are lk v2.19.0's, as a run of the check captured them.
func TestNamesParticipant(t *testing.T) {
	const prefix = "2026-01-01T00:00:00.000Z\tINFO\tlk\t"
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"a room name ending in p5", prefix + `lk/room.go:984	connected to room	{"room": "conch-2cgyc7lizkmiv3z2c5oyi2tqp5"}`, false},
		{"a room name ending in p5 and a quote", prefix + `lk/room.go:984	connected to room	{"room": "p5"}`, false},
		{"participant connected", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_crg2eCg8DAuE", "participant": "p5"}`, true},
		{"track subscribed", prefix + `v2@v2.19.0/remoteparticipant.go:122	track subscribed	{"participant": "p5", "trackID": "TR_AMLFHibASgJqYp", "kind": "audio"}`, true},
		{"another member", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_crg2eCg8DAuE", "participant": "p6"}`, false},
		{"a longer identity", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_crg2eCg8DAuE", "participant": "p55"}`, false},
		{"a SID that ends in p5", prefix + `room.go:1024	could not find participant, deferring track update	{"participantID": "PA_crg2eCg8DAp5"}`, false},
		{"a track id that ends in p5", prefix + `lk/room.go:895	track subscribed	{"kind": "audio", "trackID": "TR_AMLFHibASgJqp5", "source": "MICROPHONE", "participant": "p6"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namesParticipant(tc.line, 5); got != tc.want {
				t.Errorf("namesParticipant(line, 5) = %v, want %v\nline: %s", got, tc.want, tc.line)
			}
		})
	}
}
