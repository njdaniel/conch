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

// TestNamesSID: an lk log line names a participant by SID only with the SID
// as a whole JSON string value. The lines are lk v2.19.0's, as a run of the
// check captured them.
func TestNamesSID(t *testing.T) {
	const (
		prefix = "2026-01-01T00:00:00.000Z\tINFO\tlk\t"
		sid    = "PA_VU7QtJsbjHJE"
	)
	for _, tc := range []struct {
		name string
		line string
		sid  string
		want bool
	}{
		{"participant connected", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_VU7QtJsbjHJE", "participant": "p5"}`, sid, true},
		{"deferring a track update", prefix + `room.go:1024	could not find participant, deferring track update	{"participantID": "PA_VU7QtJsbjHJE"}`, sid, true},
		{"under a key not seen yet", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantSid": "PA_VU7QtJsbjHJE", "participant": "p5"}`, sid, true},
		{"a different SID", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_wW6imzTmCvfS", "participant": "p6"}`, sid, false},
		{"a SID that only shares a suffix", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_wW6QtJsbjHJE", "participant": "p6"}`, sid, false},
		{"the SID as the end of a longer value", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_xPA_VU7QtJsbjHJE", "participant": "p6"}`, sid, false},
		{"the SID as the start of a longer value", prefix + `lk/room.go:866	participant connected	{"kind": 0, "participantID": "PA_VU7QtJsbjHJE0", "participant": "p6"}`, sid, false},
		{"a track subscribed line", prefix + `lk/room.go:895	track subscribed	{"kind": "audio", "trackID": "TR_AMdnfGyHrdLtvW", "source": "MICROPHONE", "participant": "p5"}`, sid, false},
		{"no SID to look for", prefix + `room.go:1024	could not find participant, deferring track update	{"participantID": ""}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namesSID(tc.line, tc.sid); got != tc.want {
				t.Errorf("namesSID(line, %q) = %v, want %v\nline: %s", tc.sid, got, tc.want, tc.line)
			}
		})
	}
}
