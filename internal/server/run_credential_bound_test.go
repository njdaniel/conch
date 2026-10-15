package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// A credential's open runs are bounded across all its sessions, live, offline
// and ended, the same refusal over REST and MCP; ending a session ends its
// runs, which is how a credential recovers (issue #402). The 512-run fill is
// kept out of -race runs for the package's time limit (#340); the store tests
// fill the same bound under it.
func TestCredentialRunBoundOverRESTAndMCP(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("fills a credential's 512 open runs")
	}
	f := newInterventionFixture(t)
	// The fixture's own session and its run end first, so the 16 below hold
	// exactly the credential's budget.
	if rec := f.do(t, "DELETE", sessionPath(f.sess, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE fixture session = %d %s", rec.Code, rec.Body)
	}
	var sessions []int64
	for len(sessions) < store.MaxLiveSessionsPerCredential {
		sessions = append(sessions, f.session(t, f.botTok))
	}
	for _, id := range sessions {
		for range store.MaxOpenRunsPerSession {
			f.start(t, f.botTok, id, "general", 0, "t")
		}
	}

	// Everyone goes quiet; a new session is live and empty, and is refused.
	f.clock.Advance(DefaultSessionOffline + time.Second)
	f.srv.sweepSessions(t.Context())
	fresh := f.session(t, f.botTok)
	args := map[string]any{"session_id": fresh, "channel": "general", "task": "t"}
	f.checkParity(t, []parityCase{
		{"the 513th open run", f.botTok, "POST", sessionPath(fresh, "/runs"), runBody("general", 0, "t"), "start_run", args, schema.ErrorCodeTooManyRuns},
	})
	assertErrorBody(t, f.do(t, "POST", sessionPath(fresh, "/runs"), f.botTok, runBody("general", 0, "t")), http.StatusTooManyRequests, schema.ErrorCodeTooManyRuns)

	// Ending the old sessions (DELETE, allowed while offline) ends their
	// runs, and the credential can start again.
	for _, id := range sessions {
		if rec := f.do(t, "DELETE", sessionPath(id, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE session %d = %d %s", id, rec.Code, rec.Body)
		}
	}
	for _, id := range f.listedRunIDs(t, f.botTok, "general") {
		rec := f.do(t, "GET", runPath(id, ""), f.botTok, "")
		if run := validRun(t, rec); run.State != schema.RunStateFailed || run.Outcome != store.RunOutcomeSessionEnded || run.EndedAt == nil {
			t.Fatalf("run %d = %s %q ended=%v, want failed %q", id, run.State, run.Outcome, run.EndedAt, store.RunOutcomeSessionEnded)
		}
	}
	if res := f.tool(t, f.botTok, "start_run", args); res.code != "ok" {
		t.Errorf("start_run after ending every session = %s", res.code)
	}
}
