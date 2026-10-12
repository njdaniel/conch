package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// TestApprovalChainUnderARateLimit is the approval path's end-to-end test for
// issue #80: an agent whose manifest caps approvals.request still gets the
// whole chain (request, notify, resolve, audit) for a request inside its
// budget, and a request over it is refused before anything of the chain
// exists: no approval, no notification, one agent_rate_limited row.
func TestApprovalChainUnderARateLimit(t *testing.T) {
	var delivered atomic.Int64
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ntfy.Close)
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff,
		Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "approvals-urgent"}})
	clock := newManualClock()
	srv.now = clock.Now
	channel, agent, human := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}
	const window = 300
	limited := schema.PutAgentManifestRequestV1{DisplayName: "test agent", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: channel.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}},
		RateLimits: []schema.RateLimit{{Capability: schema.CapabilityApprovalsRequest, Max: 1, WindowSeconds: window}}}
	if _, _, err := srv.store.PutAgentManifest(context.Background(), "system", agent.ID, limited); err != nil {
		t.Fatal(err)
	}
	raw := &authzFixture{srv: srv}
	args := approvalMCPArguments(channel.ID, time.Now().Add(time.Hour))

	// Inside the budget: the whole chain.
	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", args)
	// Over it: refused, and nothing of a second chain comes into being.
	refused, err := raw.callWithToken("token-1", "request_approval", args)
	if err != nil {
		t.Fatal(err)
	}
	if !refused.isError || refused.code != schema.ErrorCodeRateLimited {
		t.Fatalf("second request_approval = %s, want %s (%s)", refused.result(), schema.ErrorCodeRateLimited, refused.body)
	}

	// The limit is on asking, not on the approval already asked for: it is
	// awaited, decided and checked as ever.
	awaited := make(chan schema.AwaitDecisionOutput, 1)
	go func() {
		awaited <- mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, 2, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 5000})
	}()
	time.Sleep(300 * time.Millisecond)
	decideApprovalREST(t, srv, created.ID, human.ID)
	waitResult := <-awaited
	checked := mcpCallInProcess[schema.CheckDecisionOutput](t, srv, 3, "check_decision", map[string]any{"approval_id": created.ID})
	if waitResult.State != schema.ApprovalStateResolved || waitResult.Resolution == nil {
		t.Fatalf("await result = %+v, want resolved with resolution", waitResult)
	}
	if !reflect.DeepEqual(waitResult.Resolution, checked.Resolution) {
		t.Fatalf("await/check resolutions differ: await=%+v check=%+v", waitResult.Resolution, checked.Resolution)
	}

	events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var chain, limitedRows []string
	createdRows := 0
	for _, e := range events {
		if e.Subject == fmt.Sprintf("approval:%d", created.ID) {
			chain = append(chain, e.Action)
		}
		if e.Action == store.AuditApprovalCreated {
			createdRows++
		}
		if e.Action == store.AuditAgentRateLimited {
			limitedRows = append(limitedRows, e.Actor+" "+e.Detail)
		}
	}
	want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("audit chain = %v, want %v", chain, want)
	}
	if createdRows != 1 {
		t.Errorf("approvals created = %d, want 1: the refused request must create none", createdRows)
	}
	if got := delivered.Load(); got != 2 {
		t.Errorf("notifications that reached ntfy = %d, want 2 (created, resolved): the refused request must send none", got)
	}
	wantRow := fmt.Sprintf("principal:%d capability=approvals.request max=1 window_seconds=%d count=1", agent.ID, window)
	if !reflect.DeepEqual(limitedRows, []string{wantRow}) {
		t.Errorf("agent_rate_limited rows = %q, want one: %q", limitedRows, wantRow)
	}

	// Once the window has moved on the agent may ask again, and that request
	// starts a chain of its own.
	clock.Advance(window * time.Second)
	again := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 4, "request_approval", args)
	if again.ID == created.ID {
		t.Fatalf("the request after the window returned the first approval (%d)", again.ID)
	}
	if got := delivered.Load(); got != 3 {
		t.Errorf("notifications after the second approval = %d, want 3", got)
	}
}

// A hook bound to an agent is a posting credential for it, so a post through
// the hook spends the agent's messages.post budget like any other post. If it
// did not, a hook would be the way round the limit.
func TestHookPostsSpendTheAgentsBudget(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 2, 60))
	hook := func(who string) string {
		t.Helper()
		rec := f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"ops","principal":%d}`, f.p(who).ID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create hook for %s = %d: %s", who, rec.Code, rec.Body.String())
		}
		var created struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Token == "" {
			t.Fatalf("hook token missing from %s (%v)", rec.Body.String(), err)
		}
		return created.Token
	}
	ingest := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+token, strings.NewReader(`{"body":"from hook"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	ariaHook, annHook, nomadHook := hook("aria"), hook("ann"), hook("nomad")

	// A hook whose agent is not a member is refused as an unknown token,
	// however often, and that costs nothing.
	for range 5 {
		if rec := ingest(nomadHook); rec.Code != http.StatusNotFound {
			t.Fatalf("non-member agent's hook = %d, want 404", rec.Code)
		}
	}

	before := f.messageCount(t)
	if rec := ingest(ariaHook); rec.Code != http.StatusCreated {
		t.Fatalf("first hook post = %d: %s", rec.Code, rec.Body.String())
	}
	// The hook and the agent's own token draw on one budget.
	if rec := f.restPost(t, "aria", `{"body":"direct"}`); rec.Code != http.StatusCreated {
		t.Fatalf("direct post = %d: %s", rec.Code, rec.Body.String())
	}
	rec := ingest(ariaHook)
	if rec.Code != http.StatusTooManyRequests || errCodeOf(t, rec.Body.String()) != schema.ErrorCodeRateLimited {
		t.Fatalf("hook post over the limit = %d %s, want 429 %s", rec.Code, rec.Body.String(), schema.ErrorCodeRateLimited)
	}
	if retry, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || retry < 1 || retry > 60 {
		t.Errorf("Retry-After = %q, want 1..60", rec.Header().Get("Retry-After"))
	}
	if rec := f.restPost(t, "aria", `{"body":"direct"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("direct post after the hook used the budget = %d, want 429", rec.Code)
	}
	if got := f.messageCount(t) - before; got != 2 {
		t.Errorf("messages stored = %d, want 2: a refused post stores nothing", got)
	}
	// The audit row names the route pattern, never the hook's token.
	for _, e := range f.rateLimitedRows(t) {
		if strings.Contains(e.Subject+e.Detail+e.Actor, ariaHook) {
			t.Errorf("audit row carries the hook token: %+v", e)
		}
	}
	// A hook bound to a human is not an agent's and is never limited.
	for i := range 5 {
		if rec := ingest(annHook); rec.Code != http.StatusCreated {
			t.Fatalf("human's hook post %d = %d", i, rec.Code)
		}
	}
	f.clock.Advance(60 * time.Second)
	if rec := ingest(ariaHook); rec.Code != http.StatusCreated {
		t.Errorf("hook post after the window = %d", rec.Code)
	}
}

// The 403 a route gives a caller without the role for it is audited, and one
// caller repeating it must not be able to grow the audit log with every
// request (the comment on issue #80, from the review of #89).
func TestRouteDenialsAreThinnedOut(t *testing.T) {
	f := newRateFixture(t)
	denied := func(method, path string) {
		t.Helper()
		if rec := f.do(t, method, path, f.p("ann").token, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as a member = %d, want 403", method, path, rec.Code)
		}
	}
	details := func() []string {
		var out []string
		for _, e := range f.denials(t) {
			out = append(out, e.Subject+" "+e.Detail)
		}
		return out
	}

	for range 50 {
		denied(http.MethodGet, "/v1/hooks")
	}
	// Written: the 1st, 2nd, 4th, 8th, 16th and 32nd. The first is exactly
	// the row there always was; the last says at least half of how many.
	want := []string{"GET /v1/hooks forbidden"}
	for _, n := range []int{2, 4, 8, 16, 32} {
		want = append(want, fmt.Sprintf("GET /v1/hooks forbidden count=%d", n))
	}
	if got := details(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after 50 identical denials:\n got %q\nwant %q", got, want)
	}
	// Another route is another kind, written at once.
	denied(http.MethodPost, "/v1/hooks")
	if got := details(); len(got) != len(want)+1 || got[len(got)-1] != "POST /v1/hooks forbidden" {
		t.Fatalf("a different route's first denial was not written at once: %q", got)
	}
	// A minute on, the count starts again.
	f.clock.Advance(denialCoalesceWindow)
	denied(http.MethodGet, "/v1/hooks")
	if got := details(); got[len(got)-1] != "GET /v1/hooks forbidden" {
		t.Errorf("first denial of a new window = %q, want it plain", got[len(got)-1])
	}
}

// Every read an agent can make spends messages.read from one budget: the two
// message reads, the member list and the net list over REST, and read_channel
// over MCP. A limit of one use therefore lets exactly one of them through,
// whichever is tried first.
func TestEveryReadSpendsTheReadBudget(t *testing.T) {
	reads := []struct {
		name string
		do   func(f *rateFixture, t *testing.T) (ok, limited bool)
	}{
		{"GET v1 messages", func(f *rateFixture, t *testing.T) (bool, bool) { return restOutcome(f.restRead(t, "aria")) }},
		{"GET v2 messages", func(f *rateFixture, t *testing.T) (bool, bool) {
			return restOutcome(f.do(t, http.MethodGet, "/v2/channels/ops/messages", f.p("aria").token, ""))
		}},
		{"GET members", func(f *rateFixture, t *testing.T) (bool, bool) {
			return restOutcome(f.do(t, http.MethodGet, "/v1/channels/ops/members", f.p("aria").token, ""))
		}},
		{"GET nets", func(f *rateFixture, t *testing.T) (bool, bool) {
			return restOutcome(f.do(t, http.MethodGet, "/v1/channels/ops/nets", f.p("aria").token, ""))
		}},
		{"MCP read_channel", func(f *rateFixture, t *testing.T) (bool, bool) {
			out := f.mcpCall(t, f.p("aria").token, "read_channel", map[string]any{"channel": "ops"})
			return !out.isError, out.code == schema.ErrorCodeRateLimited
		}},
	}
	for _, first := range reads {
		t.Run(first.name+" first", func(t *testing.T) {
			f := newRateFixture(t)
			f.limit(t, "aria", rl(schema.CapabilityMessagesRead, 1, 60))
			if ok, _ := first.do(f, t); !ok {
				t.Fatalf("%s inside the limit was refused", first.name)
			}
			for _, next := range reads {
				if ok, limited := next.do(f, t); ok || !limited {
					t.Errorf("%s after %s used the budget: ok=%v limited=%v, want refused as rate limited", next.name, first.name, ok, limited)
				}
			}
			// The post budget is another one.
			if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
				t.Errorf("post after reads were exhausted = %d", rec.Code)
			}
		})
	}
}

func restOutcome(rec *httptest.ResponseRecorder) (ok, limited bool) {
	return rec.Code == http.StatusOK, rec.Code == http.StatusTooManyRequests
}
