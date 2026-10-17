package server

import (
	"net/http"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The bounds on open runs and on activity are the same refusal over REST and
// MCP, each stores nothing, and ending a run frees a slot on either.
func TestMCPRunBoundsMatchREST(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a run with 10,000 activity entries")
	}
	f := newInterventionFixture(t)
	sess := f.session(t, f.botTok)
	var ids []int64
	for i := 0; i < store.MaxOpenRunsPerSession; i++ {
		// Alternate the fronts: they count together.
		if i%2 == 0 {
			ids = append(ids, f.start(t, f.botTok, sess, "general", 0, "t").ID)
			continue
		}
		run := mustTool[schema.RunV1](t, f, f.botTok, "start_run", map[string]any{"session_id": sess, "channel": "general", "task": "t"})
		ids = append(ids, run.ID)
	}
	startArgs := map[string]any{"session_id": sess, "channel": "general", "task": "one too many"}
	f.checkParity(t, []parityCase{
		{"the 33rd open run", f.botTok, "POST", sessionPath(sess, "/runs"), runBody("general", 0, "one too many"), "start_run", startArgs, schema.ErrorCodeTooManyRuns},
	})
	assertErrorBody(t, f.do(t, "POST", sessionPath(sess, "/runs"), f.botTok, runBody("general", 0, "one too many")), http.StatusTooManyRequests, schema.ErrorCodeTooManyRuns)
	if got := len(f.listedRunIDs(t, f.botTok, "general")); got != store.MaxOpenRunsPerSession+1 {
		t.Errorf("runs listed = %d, want %d (the 32 and the fixture's own)", got, store.MaxOpenRunsPerSession+1)
	}
	f.mustState(t, f.botTok, ids[0], schema.RunStateCancelled)
	if res := f.tool(t, f.botTok, "start_run", startArgs); res.code != "ok" {
		t.Errorf("start_run after ending one = %s", res.code)
	}

	// The activity half fills 10,000 entries one transaction at a time: seconds
	// normally, minutes under the race detector, which pushed this package past
	// go test's ten-minute limit in CI (#340). Under -race it is skipped; the
	// bound itself is proved fast in store (TestAppendRunActivityBound, a bulk
	// fill), the REST and MCP refusal share calls.go with the run bound above,
	// and make check runs this half without -race.
	if raceEnabled {
		return
	}
	run := f.start(t, f.botTok, f.session(t, f.botTok), "general", 0, "chatty")
	f.fillActivity(t, run.ID, store.MaxActivityPerRun)
	f.checkParity(t, []parityCase{
		{"the 10,001st entry", f.botTok, "POST", runPath(run.ID, "/activity"), `{"kind":"command_started","summary":"s"}`, "post_activity",
			map[string]any{"run_id": run.ID, "kind": "command_started", "summary": "s"}, schema.ErrorCodeTooMuchActivity},
	})
	assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody), http.StatusTooManyRequests, schema.ErrorCodeTooMuchActivity)
	if res := f.tool(t, f.botTok, "post_activity", map[string]any{"run_id": ids[1], "kind": "command_started", "summary": "s"}); res.code != "ok" {
		t.Errorf("post_activity on another run = %s", res.code)
	}
	// A run that has ended is still told so, not that it is full.
	f.mustState(t, f.botTok, run.ID, schema.RunStateCancelled)
	f.checkParity(t, []parityCase{
		{"an entry on an ended full run", f.botTok, "POST", runPath(run.ID, "/activity"), `{"kind":"command_started","summary":"s"}`, "post_activity",
			map[string]any{"run_id": run.ID, "kind": "command_started", "summary": "s"}, schema.ErrorCodeInvalidTransition},
	})
}
