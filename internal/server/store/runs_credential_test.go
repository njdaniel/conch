package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// credentialFill starts MaxOpenRunsPerSession runs on each of
// MaxLiveSessionsPerCredential fresh sessions of f's credential and returns
// the sessions. It is the whole of the credential's budget (issue #402).
func (f *runFixture) credentialFill(t *testing.T) []int64 {
	t.Helper()
	sessions := make([]int64, 0, MaxLiveSessionsPerCredential)
	for range MaxLiveSessionsPerCredential {
		sess := f.freshSession(t)
		sessions = append(sessions, sess)
		for range MaxOpenRunsPerSession {
			if _, err := f.startOn(sess); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
	}
	return sessions
}

// openRuns is how many runs have not ended, over the whole table.
func (f *runFixture) openRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM runs WHERE ended_at IS NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The loop of the issue: fill every live session, retire the sessions (by
// going quiet or by ending them), open new ones and fill those. The open runs
// of a credential never pass MaxOpenRunsPerCredential, and a credential that
// ends its old sessions can start runs again.
func TestCreateRunCredentialBound(t *testing.T) {
	ctx := context.Background()
	if MaxOpenRunsPerCredential != 512 {
		t.Fatalf("MaxOpenRunsPerCredential = %d, want 512", MaxOpenRunsPerCredential)
	}
	tests := []struct {
		name string
		// retire takes the filled sessions out of use; it reports whether
		// their runs are still open afterwards.
		retire   func(t *testing.T, f *runFixture, sessions []int64)
		stillOpn bool
	}{
		{"offline", func(t *testing.T, f *runFixture, _ []int64) {
			if got, err := f.s.SweepSessions(ctx, runT0.Add(time.Hour), 30*time.Second, 90*time.Second); err != nil || got.Offline < MaxLiveSessionsPerCredential {
				t.Fatalf("sweep = %+v, %v; want the sessions offline", got, err)
			}
		}, true},
		{"ended", func(t *testing.T, f *runFixture, sessions []int64) {
			for _, id := range sessions {
				if err := f.s.EndSession(ctx, "system", id, f.agtCred, runT0); err != nil {
					t.Fatal(err)
				}
			}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			// Start from nothing open: the fixture's own session ends with its runs.
			if err := f.s.EndSession(ctx, "system", f.sess.ID, f.agtCred, runT0); err != nil {
				t.Fatal(err)
			}
			first := f.credentialFill(t)
			if n := f.openRuns(t); n != MaxOpenRunsPerCredential {
				t.Fatalf("open runs after the first batch = %d, want %d", n, MaxOpenRunsPerCredential)
			}
			tc.retire(t, f, first)

			var next int64
			if tc.stillOpn {
				// Runs of offline sessions still count, so the 513th is refused
				// on a brand-new session.
				if n := f.openRuns(t); n != MaxOpenRunsPerCredential {
					t.Fatalf("open runs after going offline = %d, want %d", n, MaxOpenRunsPerCredential)
				}
				next = f.freshSession(t)
			} else {
				if n := f.openRuns(t); n != 0 {
					t.Fatalf("open runs after ending the sessions = %d, want 0", n)
				}
				// The credential may fill again, and no further. With every live
				// session full the per-session bound answers first.
				second := f.credentialFill(t)
				next = second[0]
			}
			runs, audit := f.count(t, "runs"), f.count(t, "audit_events")
			if _, err := f.startOn(next); !errors.Is(err, ErrTooManyCredentialRuns) && (tc.stillOpn || !errors.Is(err, ErrTooManyRuns)) {
				t.Fatalf("run %d: err = %v, want ErrTooManyCredentialRuns", MaxOpenRunsPerCredential+1, err)
			}
			if f.count(t, "runs") != runs || f.count(t, "audit_events") != audit {
				t.Error("the refused create wrote a row")
			}
			if n := f.openRuns(t); n != MaxOpenRunsPerCredential {
				t.Errorf("open runs = %d, want %d", n, MaxOpenRunsPerCredential)
			}
			// Another credential of the principal has its own budget.
			other, err := f.s.CreateSession(ctx, "system", SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred2, Client: "adapter"}, runT0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.CreateRun(ctx, "system", RunParams{SessionID: other.ID, CredentialID: f.agtCred2, ChannelID: f.ch.ID, Task: "t"}, f.agtReader(), runT0); err != nil {
				t.Errorf("a run on another credential: %v", err)
			}

			// Recovery: end every session of the credential (offline ones
			// too), and it can start a run again.
			rows, err := f.s.db.Query("SELECT id FROM sessions WHERE credential_id = ? AND ended_at IS NULL", f.agtCred)
			if err != nil {
				t.Fatal(err)
			}
			var live []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				live = append(live, id)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			for _, id := range live {
				if err := f.s.EndSession(ctx, "system", id, f.agtCred, runT0.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			fresh := f.freshSession(t)
			if _, err := f.startOn(fresh); err != nil {
				t.Errorf("a run after ending every session: %v", err)
			}
		})
	}
}

// Ending a session fails its open runs with outcome "session ended", one
// run_ended event each, whichever way it ends; going offline does not.
func TestEndingASessionEndsItsRuns(t *testing.T) {
	ctx := context.Background()
	type world struct {
		f                *runFixture
		cred2Sess, expSe int64
	}
	// Runs: the fixture's five on f.sess (credential agtCred), one on a
	// session of agtCred2 and one on a session of an expiring credential.
	tests := []struct {
		name string
		act  func(t *testing.T, w world)
		// ended reports which of the three sessions' runs the act ends.
		fixtureEnds, cred2Ends, expEnds bool
	}{
		{"client", func(t *testing.T, w world) {
			if err := w.f.s.EndSession(ctx, "system", w.f.sess.ID, w.f.agtCred, runT0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}, true, false, false},
		{"revoke", func(t *testing.T, w world) {
			if err := w.f.s.RevokeCredential(ctx, "op", w.f.agtCred); err != nil {
				t.Fatal(err)
			}
		}, true, false, false},
		{"disable", func(t *testing.T, w world) {
			if _, err := w.f.s.DisablePrincipal(ctx, "op", w.f.agt.ID); err != nil {
				t.Fatal(err)
			}
		}, true, true, true},
		{"expiry", func(t *testing.T, w world) {
			if _, err := w.f.s.SweepSessions(ctx, runT0.Add(25*time.Hour), 30*time.Second, 90*time.Second); err != nil {
				t.Fatal(err)
			}
		}, false, false, true},
		{"offline only", func(t *testing.T, w world) {
			if _, err := w.f.s.SweepSessions(ctx, runT0.Add(time.Hour), 30*time.Second, 90*time.Second); err != nil {
				t.Fatal(err)
			}
		}, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			cred2Sess, err := f.s.CreateSession(ctx, "system", SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred2, Client: "adapter"}, runT0)
			if err != nil {
				t.Fatal(err)
			}
			// The expiry is checked against credentialNow, so pin it to the
			// test's own clock; otherwise runT0+24h is in the past from the
			// day after runT0 on (issue #413).
			now := runT0
			setCredentialClock(t, &now)
			expires := runT0.Add(24 * time.Hour)
			expCred, _, err := f.s.CreateCredential(ctx, "system", f.agt.ID, "expiring", &expires)
			if err != nil {
				t.Fatal(err)
			}
			expSess, err := f.s.CreateSession(ctx, "system", SessionParams{PrincipalID: f.agt.ID, CredentialID: expCred.ID, Client: "adapter"}, runT0)
			if err != nil {
				t.Fatal(err)
			}
			byCred := map[int64]int64{}
			for _, c := range []struct{ sess, cred int64 }{{cred2Sess.ID, f.agtCred2}, {expSess.ID, expCred.ID}} {
				r, err := f.s.CreateRun(ctx, "system", RunParams{SessionID: c.sess, CredentialID: c.cred, ChannelID: f.ch.ID, Task: "t"}, f.agtReader(), runT0)
				if err != nil {
					t.Fatal(err)
				}
				byCred[c.sess] = r.ID
			}
			tc.act(t, world{f: f, cred2Sess: cred2Sess.ID, expSe: expSess.ID})

			var ids []int64
			rows, err := f.s.db.Query("SELECT id FROM runs ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			_ = rows.Close()
			if len(ids) != 7 {
				t.Fatalf("runs = %d, want 7", len(ids))
			}
			wantEnded := map[int64]bool{}
			for _, id := range ids {
				var sess int64
				if err := f.s.db.QueryRow("SELECT session_id FROM runs WHERE id = ?", id).Scan(&sess); err != nil {
					t.Fatal(err)
				}
				switch sess {
				case f.sess.ID:
					wantEnded[id] = tc.fixtureEnds
				case cred2Sess.ID:
					wantEnded[id] = tc.cred2Ends
				case expSess.ID:
					wantEnded[id] = tc.expEnds
				}
			}
			var ended int
			for _, id := range ids {
				var state, outcome string
				var endedAt, startedAt sql.NullInt64
				if err := f.s.db.QueryRow("SELECT state, outcome, ended_at, started_at FROM runs WHERE id = ?", id).Scan(&state, &outcome, &endedAt, &startedAt); err != nil {
					t.Fatal(err)
				}
				if wantEnded[id] {
					ended++
					if state != "failed" || outcome != RunOutcomeSessionEnded || !endedAt.Valid || endedAt.Int64 < startedAt.Int64 {
						t.Errorf("run %d = %s %q ended_at=%v, want failed %q with ended_at", id, state, outcome, endedAt, RunOutcomeSessionEnded)
					}
				} else if state != "queued" || outcome != "" || endedAt.Valid {
					t.Errorf("run %d = %s %q ended_at=%v, want it untouched", id, state, outcome, endedAt)
				}
			}
			events, err := f.s.ListAuditEvents(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for _, e := range events {
				if e.Action == AuditRunEnded {
					seen[e.Subject]++
					if !strings.HasPrefix(e.Detail, "state=failed cause=session_ended session=") {
						t.Errorf("run_ended detail = %q", e.Detail)
					}
				}
			}
			if len(seen) != ended {
				t.Errorf("run_ended subjects = %v, want %d", seen, ended)
			}
			for id, want := range wantEnded {
				if got := seen[fmt.Sprintf("run:%d", id)]; (got == 1) != want || got > 1 {
					t.Errorf("run:%d has %d run_ended rows, want ended=%v", id, got, want)
				}
			}
		})
	}
}

// Runs left open by a session that ended before #402 (when ending a session
// left its runs alone) are not counted: nothing could ever end them, so
// counting them would lock the credential out for good.
func TestRunsOfAnEndedSessionDoNotCount(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	if err := f.s.EndSession(ctx, "system", f.sess.ID, f.agtCred, runT0); err != nil {
		t.Fatal(err)
	}
	first := f.credentialFill(t)
	// End the sessions as the old code did: the session ends, its runs stay.
	for _, id := range first {
		if _, err := f.s.db.Exec("UPDATE sessions SET ended_at = ?, state = 'offline' WHERE id = ?", runT0.UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.openRuns(t); n != MaxOpenRunsPerCredential {
		t.Fatalf("open runs left behind = %d, want %d", n, MaxOpenRunsPerCredential)
	}
	if _, err := f.startOn(f.freshSession(t)); err != nil {
		t.Fatalf("a run beside runs left open by ended sessions: %v", err)
	}
}
