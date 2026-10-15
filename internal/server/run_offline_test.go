package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// An offline session takes no new run, no activity and no state change that
// leaves a run open, the same refusal over REST and MCP; it can still end its
// runs, and a heartbeat brings it back (issue #392).
func TestOfflineSessionRefusesRunsAsOnRESTAndMCP(t *testing.T) {
	f := newInterventionFixture(t)
	run := f.newRun(t)
	f.clock.Advance(DefaultSessionOffline + time.Second)
	f.srv.sweepSessions(t.Context())

	activityArgs := map[string]any{"run_id": run.ID, "kind": "command_started", "summary": "s"}
	stateArgs := func(state schema.RunState) map[string]any {
		return map[string]any{"run_id": run.ID, "state": string(state)}
	}
	f.checkParity(t, []parityCase{
		{"start_run on an offline session", f.botTok, "POST", sessionPath(f.sess, "/runs"), runBody("general", 0, "t"), "start_run",
			map[string]any{"session_id": f.sess, "channel": "general", "task": "t"}, schema.ErrorCodeSessionOffline},
		{"post_activity on an offline session", f.botTok, "POST", runPath(run.ID, "/activity"), `{"kind":"command_started","summary":"s"}`, "post_activity",
			activityArgs, schema.ErrorCodeSessionOffline},
		{"update_run to blocked on an offline session", f.botTok, "POST", runPath(run.ID, "/state"), `{"state":"blocked"}`, "update_run",
			stateArgs(schema.RunStateBlocked), schema.ErrorCodeSessionOffline},
	})
	assertErrorBody(t, f.do(t, "POST", sessionPath(f.sess, "/runs"), f.botTok, runBody("general", 0, "t")), http.StatusConflict, schema.ErrorCodeSessionOffline)

	// Ending is allowed over either front.
	other := f.newRunAfterRevive(t)
	f.clock.Advance(DefaultSessionOffline + time.Second)
	f.srv.sweepSessions(t.Context())
	f.mustState(t, f.botTok, run.ID, schema.RunStateFailed)
	args := stateArgs(schema.RunStateCancelled)
	args["run_id"] = other.ID
	if res := f.tool(t, f.botTok, "update_run", args); res.code != "ok" {
		t.Errorf("update_run to cancelled on an offline session = %s, want ok", res.code)
	}
	// An ended run is told it has ended, not that the session is offline.
	f.checkParity(t, []parityCase{
		{"post_activity on an ended run", f.botTok, "POST", runPath(run.ID, "/activity"), `{"kind":"command_started","summary":"s"}`, "post_activity",
			activityArgs, schema.ErrorCodeInvalidTransition},
	})

	// A heartbeat revives the session and everything works again.
	if rec := f.do(t, "POST", sessionPath(f.sess, "/heartbeat"), f.botTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	f.start(t, f.botTok, f.sess, "general", 0, "back")
}

// newRunAfterRevive revives the fixture's session and starts a run on it.
func (f *interventionFixture) newRunAfterRevive(t *testing.T) schema.RunV1 {
	t.Helper()
	if rec := f.do(t, "POST", sessionPath(f.sess, "/heartbeat"), f.botTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	return f.newRun(t)
}

// The sequence the issue describes: fill every live session, go quiet until
// all are offline, then open new ones. The old ones take no more runs, and
// reviving them is held to the live-session bound, so open runs never pass
// MaxLiveSessionsPerCredential * MaxOpenRunsPerSession. One run per session
// stands for the full 32: the store test fills the per-session bound itself.
func TestOfflineSessionsCannotBeFilledAgain(t *testing.T) {
	f := newInterventionFixture(t)
	first := []int64{f.sess}
	for len(first) < store.MaxLiveSessionsPerCredential {
		first = append(first, f.session(t, f.botTok))
	}
	for _, id := range first[1:] {
		f.start(t, f.botTok, id, "general", 0, "t")
	}
	f.clock.Advance(DefaultSessionOffline + time.Second)
	f.srv.sweepSessions(t.Context())

	// The credential may open a new batch, since offline sessions do not
	// count, but the old batch is shut.
	second := make([]int64, 0, len(first))
	for range first {
		second = append(second, f.session(t, f.botTok))
	}
	for _, id := range first {
		assertErrorBody(t, f.do(t, "POST", sessionPath(id, "/runs"), f.botTok, runBody("general", 0, "again")), http.StatusConflict, schema.ErrorCodeSessionOffline)
	}
	// Reviving one of them is refused while the new batch is live.
	assertErrorBody(t, f.do(t, "POST", sessionPath(first[0], "/heartbeat"), f.botTok, ""), http.StatusTooManyRequests, schema.ErrorCodeTooManySessions)
	// Every one of the new batch takes runs.
	for _, id := range second {
		f.start(t, f.botTok, id, "general", 0, "t")
	}
}
