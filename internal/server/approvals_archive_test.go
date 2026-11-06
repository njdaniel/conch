package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Approvals, runs and interventions in an archived channel (issue #1296). New
// ones are refused with 409 channel_archived and nothing written or sent;
// the ones already open carry on; restore puts everything back. The store
// holds the guards (store/archive_approvals_test.go); these tests ask them
// over the wire.

// ntfyRecorder is an ntfy server that keeps every published body.
type ntfyRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newNtfyRecorder(t *testing.T) *ntfyRecorder {
	t.Helper()
	n := &ntfyRecorder{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.bodies = append(n.bodies, r.URL.Path+" "+r.Header.Get("Title")+" "+string(b))
		n.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// mentioning is every published notification that contains s.
func (n *ntfyRecorder) mentioning(s string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, b := range n.bodies {
		if strings.Contains(b, s) {
			out = append(out, b)
		}
	}
	return out
}

func (n *ntfyRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.bodies)
}

// waitFor waits until at least want notifications have been published.
func (n *ntfyRecorder) waitFor(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications published, want at least %d", n.count(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// archiveChainArguments is approvalMCPArguments with its own title, so that a
// notification or an audit row can be traced to the request.
func archiveChainArguments(channelID int64, title string) map[string]any {
	args := approvalMCPArguments(channelID, time.Now().Add(time.Hour))
	args["title"] = title
	return args
}

// TestArchivedChannelApprovalFullChain is the rule-3 chain for issue #1296:
// request, notify, resolve and audit, across an archive and a restore.
//  1. The agent opens an approval in a live channel, and it is notified.
//  2. The owner archives the channel.
//  3. A second request, over MCP and over REST, is refused with
//     channel_archived: no approval, no notification, no audit row.
//  4. A member decides the first approval: the agent reads the resolution,
//     and its audit chain is complete.
//  5. The owner restores the channel, and a third request runs the whole
//     chain again.
func TestArchivedChannelApprovalFullChain(t *testing.T) {
	ctx := context.Background()
	ntfy := newNtfyRecorder(t)
	f := newAuthFixtureWithConfig(t, Config{AuthMode: AuthRequired, Ntfy: approvals.NtfyConfig{
		Server: ntfy.srv.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}})
	defer f.srv.approvals.Close()
	f.srv.cfg.MCPBearerTokens = map[string]int64{"token-1": f.bot.ID}
	ops, err := f.srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.root.ID, f.alice.ID, f.bot.ID} {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", ops.ID, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	setAgentManifest(t, f.srv, f.bot.ID, nil, general.ID, ops.ID)
	settings := func(body string) {
		t.Helper()
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/%d/settings", ops.ID), f.rootTok, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("settings %s = %d %s", body, rec.Code, rec.Body)
		}
	}
	decide := func(id int64) {
		t.Helper()
		rec := f.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), f.aliceTok, decisionBody(0))
		if rec.Code != http.StatusOK {
			t.Fatalf("alice's decision on %d = %d %s", id, rec.Code, rec.Body)
		}
		// await_decision, the agent's blocking read, answers in an
		// archived channel as in a live one.
		awaited := mcpCallInProcess[schema.AwaitDecisionOutput](t, f.srv, 9, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 5000})
		if awaited.State != schema.ApprovalStateResolved || awaited.Resolution == nil || awaited.Resolution.Outcome != schema.OutcomeApproved {
			t.Fatalf("await_decision(%d) = %+v, want resolved approved", id, awaited)
		}
	}
	chainOf := func(id int64) string {
		t.Helper()
		var chain []string
		for _, e := range f.audit(t) {
			switch e.Action {
			case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
				if e.Subject == fmt.Sprintf("approval:%d", id) {
					chain = append(chain, e.Action+" "+e.Actor)
				}
			}
		}
		return strings.Join(chain, "|")
	}
	wantChain := strings.Join([]string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.bot.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditApprovalResolved + " system",
	}, "|")

	// 1. Requested in a live channel, and notified.
	first := mcpCallInProcess[schema.RequestApprovalOutput](t, f.srv, 1, "request_approval", archiveChainArguments(ops.ID, "before the archive"))
	ntfy.waitFor(t, 1)

	// 2. Archived.
	settings(`{"archived":true}`)

	// 3. Refused over MCP and over REST, with nothing written or sent.
	before := len(f.audit(t))
	if res := mcpTool(t, f.srv, "token-1", "request_approval", archiveChainArguments(ops.ID, "while archived over MCP")); res.code != schema.ErrorCodeChannelArchived {
		t.Errorf("request_approval in an archived channel = %s, want %s", res.code, schema.ErrorCodeChannelArchived)
	}
	restBody := fmt.Sprintf(`{"channel_id":%d,"title":"while archived over REST","body":"refused","options":[{"id":"approve","kind":"approve","label":"Approve"},{"id":"reject","kind":"reject","label":"Reject"}],"deadline":%q,"quorum":1}`,
		ops.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	assertErrorBody(t, f.do(t, "POST", "/v1/approvals", f.aliceTok, restBody), http.StatusConflict, schema.ErrorCodeChannelArchived)
	if after := len(f.audit(t)); after != before {
		t.Errorf("the refused requests wrote %d audit rows", after-before)
	}

	// 4. The approval opened before the archive is decided, read back and
	// audited as in a live channel.
	decide(first.ID)
	if got := chainOf(first.ID); got != wantChain {
		t.Errorf("the in-flight approval's audit chain = %q, want %q", got, wantChain)
	}

	// 5. Restored: a new request runs the whole chain.
	settings(`{"archived":false}`)
	published := ntfy.count()
	third := mcpCallInProcess[schema.RequestApprovalOutput](t, f.srv, 3, "request_approval", archiveChainArguments(ops.ID, "after the restore"))
	ntfy.waitFor(t, published+1)
	decide(third.ID)
	if got := chainOf(third.ID); got != wantChain {
		t.Errorf("the restored channel's audit chain = %q, want %q", got, wantChain)
	}

	// Nothing of the refused requests reached ntfy or the audit log, even
	// late. By now every notification of the other two has been sent:
	// approvals.Manager.announce posts synchronously on create and resolve
	// (only its load-retry path is deferred, and a healthy store never takes
	// it), so a notifier that became asynchronous would need this check to
	// wait for the resolutions first, as the dogfood path does.
	for _, title := range []string{"while archived over MCP", "while archived over REST"} {
		if got := ntfy.mentioning(title); len(got) != 0 {
			t.Errorf("a refused request was notified: %q", got)
		}
		for _, e := range f.audit(t) {
			if strings.Contains(e.Detail, title) {
				t.Errorf("a refused request was audited: %+v", e)
			}
		}
	}
	// Each live approval was notified once when requested and once when
	// resolved, the one decided while archived included.
	for _, title := range []string{"before the archive", "after the restore"} {
		for _, kind := range []string{"Approval requested: ", "Approval resolved: "} {
			if got := ntfy.mentioning(kind + title); len(got) != 1 {
				t.Errorf("%q notifications = %d, want 1: %q", kind+title, len(got), got)
			}
		}
	}
}

// TestArchivedChannelRefusesNewRunsAndInterventions: starting a run (REST and
// MCP) and asking for an intervention answer 409 channel_archived and write
// nothing; the run already going changes state, reports and has its
// intervention answered; restore lets new ones start.
func TestArchivedChannelRefusesNewRunsAndInterventions(t *testing.T) {
	ctx := context.Background()
	f := newInterventionFixture(t)
	going := f.start(t, f.botTok, f.sess, f.private.Name, 0, "already going")
	f.mustState(t, f.botTok, going.ID, schema.RunStateRunning)
	asked := f.mustAsk(t, f.aliceTok, going.ID, schema.InterventionKindCancel)
	archive := func(archived bool) {
		t.Helper()
		if _, err := f.srv.store.ChannelSettings(ctx, "system", 0, f.private.ID, store.ChannelSettingsChange{Archived: &archived}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	archive(true)

	audit := len(f.audit(t))
	assertErrorBody(t, f.do(t, "POST", sessionPath(f.sess, "/runs"), f.botTok, runBody(f.private.Name, 0, "late")), http.StatusConflict, schema.ErrorCodeChannelArchived)
	if res := f.tool(t, f.botTok, "start_run", map[string]any{"session_id": f.sess, "channel": f.private.Name, "task": "late"}); res.code != schema.ErrorCodeChannelArchived {
		t.Errorf("start_run in an archived channel = %s, want %s", res.code, schema.ErrorCodeChannelArchived)
	}
	assertErrorBody(t, f.ask(t, f.aliceTok, going.ID, schema.InterventionKindMessage, "and the linter"), http.StatusConflict, schema.ErrorCodeChannelArchived)
	if got := len(f.audit(t)); got != audit {
		t.Errorf("the refusals wrote %d audit rows", got-audit)
	}

	// The run already going finishes its work.
	if rec := f.do(t, "POST", runPath(going.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Errorf("activity on a run in an archived channel = %d %s", rec.Code, rec.Body)
	}
	if rec := f.ack(t, f.botTok, asked.ID, schema.InterventionStateAcknowledged, "stopping"); rec.Code != http.StatusOK {
		t.Errorf("answering an intervention in an archived channel = %d %s", rec.Code, rec.Body)
	}
	f.mustState(t, f.botTok, going.ID, schema.RunStateCancelled)

	archive(false)
	again := f.start(t, f.botTok, f.sess, f.private.Name, 0, "after the restore")
	f.mustState(t, f.botTok, again.ID, schema.RunStateRunning)
	f.mustAsk(t, f.aliceTok, again.ID, schema.InterventionKindCancel)
}
