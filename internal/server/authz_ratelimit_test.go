package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Per-principal agent rate limits, end to end (issue #80): the manifest's
// RateLimits enforced through REST, WebSocket and MCP with a clock the test
// moves.

// rateFixture is the scoped fixture with a hand-moved clock.
type rateFixture struct {
	*scopedFixture
	clock *manualClock
}

func newRateFixture(t *testing.T) *rateFixture {
	t.Helper()
	f := &rateFixture{scopedFixture: newScopedFixture(t), clock: newManualClock()}
	f.srv.now = f.clock.Now
	return f
}

// limit rewrites an agent's manifest: every capability, read and post in ops,
// and the given limits.
func (f *rateFixture) limit(t *testing.T, name string, limits ...schema.RateLimit) {
	t.Helper()
	req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}}},
		RateLimits: limits}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.p(name).ID, req); err != nil {
		t.Fatal(err)
	}
}

func rl(c schema.Capability, maxUses, windowSeconds int) schema.RateLimit {
	return schema.RateLimit{Capability: c, Max: maxUses, WindowSeconds: windowSeconds}
}

func (f *rateFixture) rateLimitedRows(t *testing.T) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditAgentRateLimited {
			out = append(out, e)
		}
	}
	return out
}

func (f *rateFixture) restPost(t *testing.T, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "POST", "/v2/channels/ops/messages", f.p(who).token, body)
}

func (f *rateFixture) restRead(t *testing.T, who string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "GET", "/v1/channels/ops/messages", f.p(who).token, "")
}

func errCodeOf(t *testing.T, body string) string {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Code
}

func TestRESTRateLimitedPostIs429(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 2, 60))

	for i := range 2 {
		if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
			t.Fatalf("post %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.restPost(t, "aria", `{"body":"x"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third post = %d %s, want 429", rec.Code, rec.Body)
	}
	if got := errCodeOf(t, rec.Body.String()); got != schema.ErrorCodeRateLimited {
		t.Errorf("code = %q, want %q", got, schema.ErrorCodeRateLimited)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Errorf("Retry-After = %q, want whole seconds in [1, 60]", rec.Header().Get("Retry-After"))
	}
	if n := f.messageCount(t); n != 2 {
		t.Errorf("messages stored = %d, want 2: a refused post stores nothing", n)
	}

	// The advice is honest: waiting that long admits the next post, and a
	// second less does not.
	f.clock.Advance(time.Duration(ra)*time.Second - time.Second)
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post just before Retry-After = %d, want 429", rec.Code)
	}
	f.clock.Advance(time.Second)
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post after Retry-After = %d %s, want 201", rec.Code, rec.Body)
	}
}

// The same over a real socket, to see the status line and header as a client
// does.
func TestRESTRateLimitOverRealHTTP(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 1, 30))
	ts := httptest.NewServer(f.srv.Handler())
	t.Cleanup(ts.Close)

	post := func() (*http.Response, string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/channels/ops/messages", strings.NewReader(fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.p("aria").ID)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.p("aria").token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var sb strings.Builder
		buf := make([]byte, 512)
		for {
			n, rerr := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		return resp, sb.String()
	}
	if resp, body := post(); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post = %d %s", resp.StatusCode, body)
	}
	resp, body := post()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second post = %d %s, want 429", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if got := errCodeOf(t, body); got != schema.ErrorCodeRateLimited {
		t.Errorf("code = %q", got)
	}
}

// One budget per principal and capability: exhausting one never throttles
// another capability, another principal, or a human.
func TestRateLimitIsolation(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 1, 60))
	f.limit(t, "arlo", rl(schema.CapabilityMessagesPost, 1, 60))
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("aria first post = %d", rec.Code)
	}

	tests := []struct {
		name string
		do   func() int
		want int
	}{
		{"aria is exhausted for posting", func() int { return f.restPost(t, "aria", `{"body":"x"}`).Code }, http.StatusTooManyRequests},
		{"aria is exhausted for posting over MCP too", func() int {
			if out, _ := f.mcpPost(t, "aria", "x", nil); out.code == schema.ErrorCodeRateLimited {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		}, http.StatusTooManyRequests},
		{"aria's reads are unaffected", func() int { return f.restRead(t, "aria").Code }, http.StatusOK},
		{"aria's approval requests are unaffected", func() int {
			out := f.mcpCall(t, f.p("aria").token, "request_approval", approvalMCPArguments(f.ops.ID, time.Now().Add(time.Hour)))
			if out.isError {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		}, http.StatusOK},
		{"another agent has its own budget", func() int { return f.restPost(t, "arlo", `{"body":"x"}`).Code }, http.StatusCreated},
		{"a human is never limited", func() int {
			for range 10 {
				if rec := f.restPost(t, "ann", `{"body":"x"}`); rec.Code != http.StatusCreated {
					return rec.Code
				}
			}
			return http.StatusCreated
		}, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.do(); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// A limit on an approval request is spent by creating approvals only, and by
// every one of them.
func TestMCPRateLimitedApprovalRequests(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityApprovalsRequest, 2, 120))
	args := approvalMCPArguments(f.ops.ID, time.Now().Add(time.Hour))
	call := func() mcpOutcome { return f.mcpCall(t, f.p("aria").token, "request_approval", args) }

	for i := range 2 {
		if out := call(); out.isError {
			t.Fatalf("request %d refused: %s", i, out.body)
		}
	}
	out := call()
	if !out.isError || out.code != schema.ErrorCodeRateLimited {
		t.Fatalf("third request = %s, want %s (%s)", out.result(), schema.ErrorCodeRateLimited, out.body)
	}
	// The gap the dispatcher should know about: the code travels in the tool
	// error, but the delay can only be in the message text.
	if !strings.Contains(out.body, "retry in 120 seconds") {
		t.Errorf("refusal does not say when to retry: %s", out.body)
	}
	// Reads and posts are other budgets.
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
		t.Errorf("post after approvals exhausted = %d", rec.Code)
	}
	f.clock.Advance(120 * time.Second)
	if out := call(); out.isError {
		t.Errorf("request after the window refused: %s", out.body)
	}
}

// await_decision polls the approval over and over; the call is one use.
func TestMCPAwaitDecisionSpendsOnce(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityApprovalsCheck, 3, 60), rl(schema.CapabilityApprovalsAwait, 1, 60))
	out := f.mcpCall(t, f.p("aria").token, "request_approval", approvalMCPArguments(f.ops.ID, time.Now().Add(time.Hour)))
	if out.isError {
		t.Fatalf("request_approval: %s", out.body)
	}
	var created struct {
		Result struct {
			Structured struct {
				ID int64 `json:"id"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Result.Structured.ID

	// A wait long enough for several polls costs one use of approvals.await.
	if out := f.mcpCall(t, f.p("aria").token, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 1500}); out.isError {
		t.Fatalf("first await refused: %s", out.body)
	}
	if out := f.mcpCall(t, f.p("aria").token, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 1}); out.code != schema.ErrorCodeRateLimited {
		t.Fatalf("second await = %s, want rate_limited", out.result())
	}
	// check_decision has its own budget of 3.
	for i := range 3 {
		if out := f.mcpCall(t, f.p("aria").token, "check_decision", map[string]any{"approval_id": id}); out.isError {
			t.Fatalf("check %d refused: %s", i, out.body)
		}
	}
	if out := f.mcpCall(t, f.p("aria").token, "check_decision", map[string]any{"approval_id": id}); out.code != schema.ErrorCodeRateLimited {
		t.Fatalf("fourth check = %s, want rate_limited", out.result())
	}
}

// A call refused by any other gate spends nothing: the budget is for calls
// that were allowed.
func TestRefusedCallsSpendNoBudget(t *testing.T) {
	f := newRateFixture(t)
	// nomad holds every capability but is not in the channel.
	req := schema.PutAgentManifestRequestV1{DisplayName: "nomad", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}},
		RateLimits: []schema.RateLimit{rl(schema.CapabilityMessagesPost, 1, 3600), rl(schema.CapabilityMessagesRead, 1, 3600)}}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.p("nomad").ID, req); err != nil {
		t.Fatal(err)
	}
	// And arlo may post but not read.
	req2 := schema.PutAgentManifestRequestV1{DisplayName: "arlo", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost}}},
		RateLimits: []schema.RateLimit{rl(schema.CapabilityMessagesRead, 1, 3600)}}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.p("arlo").ID, req2); err != nil {
		t.Fatal(err)
	}

	for i := range 5 {
		if rec := f.restPost(t, "nomad", `{"body":"x"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("non-member post %d = %d, want 404 and never 429", i, rec.Code)
		}
		if out, _ := f.mcpPost(t, "nomad", "x", nil); out.code != "channel_not_found" {
			t.Fatalf("non-member MCP post %d = %s", i, out.result())
		}
		if rec := f.restRead(t, "arlo"); rec.Code != http.StatusForbidden {
			t.Fatalf("no-read-permission read %d = %d, want 403 and never 429", i, rec.Code)
		}
	}
	// Once allowed in, nomad still has its whole budget.
	f.join(t, "nomad")
	if rec := f.restPost(t, "nomad", `{"body":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post after joining = %d %s, want 201: refusals must not have spent the budget", rec.Code, rec.Body)
	}
	if rec := f.restPost(t, "nomad", `{"body":"x"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second post = %d, want 429", rec.Code)
	}
	if rows := f.rateLimitedRows(t); len(rows) != 1 {
		t.Errorf("agent_rate_limited rows = %d, want 1", len(rows))
	}
}

// The scoped-post path (net and whisper) draws on the same messages.post
// budget as a channel-wide post, over REST and MCP alike, and spends it once
// however many manifest checks it runs.
func TestScopedPostsShareThePostBudget(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 3, 60))
	whisper := `{"body":"x",` + whisperAudience(f.p("wes").ID, f.p("arlo").ID) + `}` // needs whisper and whisper_agent: two manifest checks

	steps := []struct {
		name string
		do   func() string
		want string
	}{
		{"REST whisper to an agent: one use", func() string { return strconv.Itoa(f.restPost(t, "aria", whisper).Code) }, "201"},
		{"MCP whisper: one use", func() string {
			out, _ := f.mcpPost(t, "aria", "x", mcpWhisper(f.p("wes").ID))
			return out.result()
		}, "ok"},
		{"REST channel-wide: the third use", func() string { return strconv.Itoa(f.restPost(t, "aria", `{"body":"x"}`).Code) }, "201"},
		{"MCP channel-wide is refused", func() string { out, _ := f.mcpPost(t, "aria", "x", nil); return out.result() }, schema.ErrorCodeRateLimited},
		{"MCP whisper is refused", func() string {
			out, _ := f.mcpPost(t, "aria", "x", mcpWhisper(f.p("wes").ID))
			return out.result()
		}, schema.ErrorCodeRateLimited},
		{"REST whisper is refused", func() string {
			rec := f.restPost(t, "aria", whisper)
			if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
				return "429 without Retry-After"
			}
			return strconv.Itoa(rec.Code)
		}, "429"},
	}
	for _, s := range steps {
		if got := s.do(); got != s.want {
			t.Fatalf("%s: got %s, want %s", s.name, got, s.want)
		}
	}
}

// A WebSocket subscription is a use of messages.read.
func TestWebSocketRateLimited(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesRead, 1, 45))
	tok := f.p("aria").token
	if res := callWS(t, f.base, "/v1/ws?channel=ops", tok); res.status != http.StatusSwitchingProtocols {
		t.Fatalf("first subscribe = %d %s", res.status, res.body)
	}
	res := callWS(t, f.base, "/v1/ws?channel=ops", tok)
	if res.status != http.StatusTooManyRequests || errCodeOf(t, res.body) != schema.ErrorCodeRateLimited {
		t.Fatalf("second subscribe = %d %s, want 429 rate_limited", res.status, res.body)
	}
	// The REST read draws on the same messages.read budget.
	if rec := f.restRead(t, "aria"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("REST read after the subscription = %d, want 429", rec.Code)
	}
}

func TestRateLimitPolicyEdges(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria") // no entries at all
	for i := range 50 {
		if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
			t.Fatalf("unlimited post %d = %d", i, rec.Code)
		}
	}
	// A limit on a capability the agent is not granted changes nothing: the
	// capability gate refuses first and nothing is spent.
	req := schema.PutAgentManifestRequestV1{DisplayName: "arlo", Tier: schema.AgentTierC, Capabilities: []schema.Capability{schema.CapabilityMessagesRead},
		Channels:   []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost}}},
		RateLimits: []schema.RateLimit{rl(schema.CapabilityMessagesPost, 1, 60)}}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.p("arlo").ID, req); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if rec := f.restPost(t, "arlo", `{"body":"x"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("ungranted post = %d, want 403", rec.Code)
		}
	}
}

// Counters are in memory: a new server on the same store starts every window
// afresh (documented: the worst case is one extra Max per restart).
func TestRateLimitResetsOnRestart(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 1, 3600))
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first post = %d", rec.Code)
	}
	if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second post = %d, want 429", rec.Code)
	}

	restarted := New(f.srv.cfg, f.srv.store)
	restarted.now = f.clock.Now
	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v2/channels/ops/messages", strings.NewReader(`{"body":"x"}`))
		req.Header.Set("Authorization", "Bearer "+f.p("aria").token)
		rec := httptest.NewRecorder()
		restarted.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post(); got != http.StatusCreated {
		t.Fatalf("post after restart = %d, want 201 (fresh window)", got)
	}
	if got := post(); got != http.StatusTooManyRequests {
		t.Fatalf("second post after restart = %d, want 429", got)
	}
}

// Refusals are audited without flooding: one row, silence for a minute, then a
// row that counts what was left out. No credential material in any row.
func TestRateLimitAudit(t *testing.T) {
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 1, 3600))
	f.restPost(t, "aria", `{"body":"x"}`)
	f.restPost(t, "aria", `{"body":"x"}`) // first refusal

	// Refusals are thinned out: the first eight of a minute are rows, then
	// the 16th, each with its number, and a new minute starts the count again.
	steps := []struct {
		name    string
		advance time.Duration
		refuse  int
		rows    int
		last    string
	}{
		{"the first refusal wrote one row", 0, 0, 1, "count=1"},
		{"the next seven are rows too", time.Second, 7, 8, "count=8"},
		{"the ninth to fifteenth are not", time.Second, 7, 8, "count=8"},
		{"just under a minute after the first: the sixteenth is", 57*time.Second + 990*time.Millisecond, 1, 9, "count=16"},
		{"a minute after the first: the count starts again", 10 * time.Millisecond, 1, 10, "count=1"},
	}
	for _, s := range steps {
		f.clock.Advance(s.advance)
		for range s.refuse {
			if rec := f.restPost(t, "aria", `{"body":"x"}`); rec.Code != http.StatusTooManyRequests {
				t.Fatalf("%s: post = %d, want 429", s.name, rec.Code)
			}
		}
		rows := f.rateLimitedRows(t)
		if len(rows) != s.rows {
			t.Fatalf("%s: agent_rate_limited rows = %d, want %d", s.name, len(rows), s.rows)
		}
		if last := rows[len(rows)-1]; !strings.HasSuffix(last.Detail, " "+s.last) {
			t.Errorf("%s: last row detail %q does not end in %q", s.name, last.Detail, s.last)
		}
	}
	row := f.rateLimitedRows(t)[0]
	wantActor := fmt.Sprintf("principal:%d", f.p("aria").ID)
	if row.Actor != wantActor || row.Subject != "POST /v2/channels/{channel}/messages" ||
		row.Detail != "capability=messages.post max=1 window_seconds=3600 count=1" {
		t.Errorf("row = %+v", row)
	}
	for _, e := range f.audit(t) {
		for _, secret := range []string{f.p("aria").token, "Bearer"} {
			if strings.Contains(e.Actor+e.Subject+e.Detail, secret) {
				t.Errorf("audit row %d carries credential material: %+v", e.ID, e)
			}
		}
	}

	// The row is per principal and capability: another agent's first refusal
	// is written at once.
	f.limit(t, "arlo", rl(schema.CapabilityMessagesPost, 1, 3600))
	f.restPost(t, "arlo", `{"body":"x"}`)
	f.restPost(t, "arlo", `{"body":"x"}`)
	if rows := f.rateLimitedRows(t); len(rows) != 11 {
		t.Errorf("rows after a second agent was refused = %d, want 11", len(rows))
	}
}

// Repeated identical denials from one principal are one row per window with a
// count; the first of each kind is written at once.
func TestAccessDeniedIsCoalesced(t *testing.T) {
	f := newRateFixture(t)
	post := func() { f.restPost(t, "nomad", `{"body":"x"}`) } // not a member: not_a_member
	deniedRows := func() []store.AuditEvent { return f.denials(t) }

	post()
	if n := len(deniedRows()); n != 1 {
		t.Fatalf("first denial rows = %d, want 1 at once", n)
	}
	for range 49 {
		post()
	}
	// 50 identical denials: the first eight, the 16th and the 32nd are rows.
	rows := deniedRows()
	if len(rows) != 10 {
		t.Fatalf("rows after 50 identical denials = %d, want 10", len(rows))
	}
	// Until thinning starts a row is written exactly as it was before it
	// existed, and the last says more than half of how many there were.
	for _, row := range rows[:8] {
		if strings.Contains(row.Detail, "count=") {
			t.Errorf("row detail = %q, want no count on the first eight", row.Detail)
		}
	}
	if last := rows[len(rows)-1]; !strings.HasSuffix(last.Detail, " count=32") {
		t.Errorf("last row detail = %q, want it to end in count=32", last.Detail)
	}
	// A different kind of denial is its own first.
	f.restRead(t, "nomad") // a different capability and subject
	if n := len(deniedRows()); n != 11 {
		t.Fatalf("rows after a different kind = %d, want 11", n)
	}
	// Another principal's identical denial is not counted with it.
	f.add(t, "nomad2", store.PrincipalAgent, false)
	f.restPost(t, "nomad2", `{"body":"x"}`)
	if n := len(deniedRows()); n != 12 {
		t.Fatalf("rows after another principal's denial = %d, want 12", n)
	}
	// A minute on, the count starts again: a plain first row.
	f.clock.Advance(denialCoalesceWindow)
	post()
	rows = deniedRows()
	if len(rows) != 13 {
		t.Fatalf("rows after the window = %d, want 13", len(rows))
	}
	if last := rows[len(rows)-1]; strings.Contains(last.Detail, "count=") {
		t.Errorf("first row of the new window = %q, want no count", last.Detail)
	}
	// Nobody was throttled for being denied: the requests themselves are not
	// limited.
	if rec := f.restPost(t, "nomad", `{"body":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("denied request = %d, want the usual 404", rec.Code)
	}
}
