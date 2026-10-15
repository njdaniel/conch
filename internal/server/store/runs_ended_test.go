package store

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// The store reports the runs a session-ending call failed, once and only after
// the commit (issue #405). Each call below ends the fixture's session, which
// holds its five open runs.
func TestSessionEndingCallsReportTheirRuns(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		act  func(f *runFixture) error
	}{
		{"client", func(f *runFixture) error {
			return f.s.EndSession(ctx, "system", f.sess.ID, f.agtCred, runT0.Add(time.Minute))
		}},
		{"revoke", func(f *runFixture) error { return f.s.RevokeCredential(ctx, "op", f.agtCred) }},
		{"rotate", func(f *runFixture) error { _, _, err := f.s.RotateCredential(ctx, "op", f.agtCred); return err }},
		{"disable", func(f *runFixture) error { _, err := f.s.DisablePrincipal(ctx, "op", f.agt.ID); return err }},
		{"revoke all", func(f *runFixture) error { _, err := f.s.RevokeAllCredentials(ctx, "op", f.agt.ID); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			var got []EndedRun
			f.s.OnRunsEnded(func(runs []EndedRun) { got = append(got, runs...) })
			if err := tc.act(f); err != nil {
				t.Fatal(err)
			}
			want := []EndedRun{{f.runWide, f.ch.ID}, {f.runWideMsg, f.ch.ID}, {f.runNet, f.ch.ID}, {f.runWhisper, f.ch.ID}, {f.runElse, f.other.ID}}
			sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
			sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
			if len(got) != len(want) {
				t.Fatalf("reported %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("reported %v, want %v", got, want)
					break
				}
			}
		})
	}
}

// Nothing is reported for a call that changed nothing or failed.
func TestSessionEndingCallsReportNothingWithoutACommit(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	f.s.OnRunsEnded(func(runs []EndedRun) { t.Errorf("reported %v", runs) })
	if err := f.s.EndSession(ctx, "system", f.sess.ID, f.agtCred2, runT0); !errors.Is(err, ErrNotFound) { // another credential's
		t.Fatalf("EndSession = %v, want ErrNotFound", err)
	}
	// Only offline: nothing ends.
	if _, err := f.s.SweepSessions(ctx, runT0.Add(time.Hour), 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
}
