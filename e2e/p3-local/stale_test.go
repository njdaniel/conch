package main

import (
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestStaleBeforeOffline(t *testing.T) {
	frame := func(state schema.SessionState, status schema.SessionStatus) schema.SessionRosterV1 {
		return schema.SessionRosterV1{Sessions: []schema.SessionV1{{ID: 7, State: state, Status: status}}}
	}
	online := frame(schema.SessionStateOnline, schema.SessionStatusOnline)
	stale := frame(schema.SessionStateStale, schema.SessionStatusStale)
	offline := frame(schema.SessionStateOffline, schema.SessionStatusOffline)
	badStatus := frame(schema.SessionStateStale, schema.SessionStatusIdle)
	other := schema.SessionRosterV1{Sessions: []schema.SessionV1{{ID: 8, State: schema.SessionStateStale}}}

	tests := []struct {
		name    string
		frames  []schema.SessionRosterV1
		wantErr bool
	}{
		{"stale then offline", []schema.SessionRosterV1{online, stale, offline}, false},
		{"straight to offline", []schema.SessionRosterV1{online, offline}, true},
		{"offline before stale", []schema.SessionRosterV1{online, offline, stale}, true},
		{"another session's stale does not count", []schema.SessionRosterV1{online, other, offline}, true},
		{"never offline", []schema.SessionRosterV1{online, stale}, true},
		{"stale with the wrong status", []schema.SessionRosterV1{online, badStatus, offline}, true},
		{"no frames", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := staleBeforeOffline(tc.frames, 7)
			if (err != nil) != tc.wantErr {
				t.Fatalf("staleBeforeOffline() = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
