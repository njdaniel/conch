package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestEventsPushRunsThatEndWithTheirSession: a run failed because its session
// ended reaches a subscriber as a run frame, whichever way the session ended
// (issue #405). The store changes such a run without SetRunState, so only the
// store's OnRunsEnded hook tells the events hub.
func TestEventsPushRunsThatEndWithTheirSession(t *testing.T) {
	tests := []struct {
		name string
		end  func(t *testing.T, f *runFixture, sess int64)
	}{
		{"client DELETE", func(t *testing.T, f *runFixture, sess int64) {
			if rec := f.do(t, "DELETE", sessionPath(sess, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("end session = %d %s", rec.Code, rec.Body)
			}
		}},
		{"credential revoked", func(t *testing.T, f *runFixture, sess int64) {
			if err := f.srv.store.RevokeCredential(context.Background(), "system", f.credentialID(t, f.botTok)); err != nil {
				t.Fatal(err)
			}
		}},
		{"principal disabled", func(t *testing.T, f *runFixture, sess int64) {
			if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.bot.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newInterventionFixture(t)
			base := wsTestServer(t, f.srv)
			alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
			alice.next()

			sess := f.create(t, f.botTok, interventionAgentBody).ID
			run := f.start(t, f.botTok, sess, "general", 0, "tidy the build")
			// Starting the run is its only push, and this frame is read from
			// it, so no run ref is left queued: a failed frame after the
			// session ends can come only from the store's hook. (A second push
			// before the end, such as a move to running, could be read late
			// and show the failure without the hook.)
			alice.until("the run queued", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.State == schema.RunStateQueued }))

			tc.end(t, f.runFixture, sess)
			alice.until("the run failed with its session", isRun(func(r schema.RunV1) bool {
				return r.ID == run.ID && r.State == schema.RunStateFailed && r.Outcome == "session ended"
			}))
		})
	}
}
