package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// authzFixture is a server with two channels, one open approval in "alpha",
// and one agent per authorization situation. Each agent authenticates at /mcp
// with the static token "tok-<name>".
type authzFixture struct {
	srv        *Server
	alpha      store.Channel
	bravo      store.Channel
	approvalID int64
	agents     map[string]store.Principal
}

// The agents, by what distinguishes them in channel "alpha".
const (
	agFull          = "full"            // member; every capability; read+post
	agMessagesOnly  = "messages-only"   // member; messages.* only; read+post
	agReadOnly      = "read-only"       // member; every capability; read only
	agNoManifest    = "no-manifest"     // member; no manifest at all
	agOutsider      = "outsider"        // NOT a member; manifest would allow everything
	agMemberNoGrant = "member-no-grant" // member; every capability; no grant for alpha
)

func newAuthzFixture(t *testing.T) *authzFixture {
	t.Helper()
	ctx := context.Background()
	srv := newTestServer(t)
	f := &authzFixture{srv: srv, agents: map[string]store.Principal{}}
	var err error
	if f.alpha, err = srv.store.CreateChannel(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if f.bravo, err = srv.store.CreateChannel(ctx, "bravo"); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]int64{}
	for _, name := range []string{agFull, agMessagesOnly, agReadOnly, agNoManifest, agOutsider, agMemberNoGrant} {
		p, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, name)
		if err != nil {
			t.Fatal(err)
		}
		f.agents[name] = p
		tokens["tok-"+name] = p.ID
		if name != agOutsider {
			if _, err := srv.store.AddChannelMember(ctx, "system", f.alpha.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	srv.cfg.MCPBearerTokens = tokens

	both := []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}
	put := func(name string, caps []schema.Capability, grants ...schema.ChannelGrant) {
		t.Helper()
		req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierC, Capabilities: caps, Channels: grants}
		if _, _, err := srv.store.PutAgentManifest(ctx, "system", f.agents[name].ID, req); err != nil {
			t.Fatal(err)
		}
	}
	all := schema.Capabilities()
	put(agFull, all, schema.ChannelGrant{ChannelID: f.alpha.ID, Permissions: both})
	put(agMessagesOnly, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}, schema.ChannelGrant{ChannelID: f.alpha.ID, Permissions: both})
	put(agReadOnly, all, schema.ChannelGrant{ChannelID: f.alpha.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}})
	put(agOutsider, all, schema.ChannelGrant{ChannelID: f.alpha.ID, Permissions: both})
	put(agMemberNoGrant, all, schema.ChannelGrant{ChannelID: f.bravo.ID, Permissions: both})

	approval, err := srv.approvals.Create(ctx, store.ApprovalParams{
		RequesterID: f.agents[agFull].ID, ChannelID: f.alpha.ID, Title: "seed", Body: "seed approval",
		Options:  []schema.Option{{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove}, {ID: "reject", Label: "Reject", Kind: schema.OptionKindReject}},
		Deadline: time.Now().Add(time.Hour), Quorum: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.approvalID = approval.ID
	return f
}

// mcpOutcome is what an agent sees from one tool call.
type mcpOutcome struct {
	status  int
	isError bool
	code    string // the schema.Error code when isError
	body    string // the raw JSON-RPC response
}

// result reduces an outcome to "ok" or its error code.
func (o mcpOutcome) result() string {
	switch {
	case o.status != http.StatusOK:
		return fmt.Sprintf("http %d", o.status)
	case o.isError:
		return o.code
	default:
		return "ok"
	}
}

func (f *authzFixture) call(t *testing.T, agent, tool string, arguments map[string]any) mcpOutcome {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-"+agent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	out := mcpOutcome{status: rec.Code, body: rec.Body.String()}
	if rec.Code != http.StatusOK {
		return out
	}
	// A tool error carries its schema.Error as JSON in the first text content
	// (the SDK fills structuredContent with the tool's typed output).
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s response %s: %v", tool, rec.Body.String(), err)
	}
	out.isError = response.Result.IsError
	if out.isError && len(response.Result.Content) > 0 {
		var serr schema.Error
		if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &serr); err != nil {
			t.Fatalf("decode %s tool error %q: %v", tool, response.Result.Content[0].Text, err)
		}
		out.code = serr.Code
	}
	return out
}

func (f *authzFixture) denials(t *testing.T) []store.AuditEvent {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, e := range events {
		if e.Action == "access_denied" {
			out = append(out, e)
		}
	}
	return out
}

// toolCalls returns, per MCP tool, the arguments that address channel alpha
// (or the seeded approval in it) and the arguments that address something
// that does not exist.
func (f *authzFixture) toolCalls() map[string][2]map[string]any {
	deadline := time.Now().Add(time.Hour)
	return map[string][2]map[string]any{
		"post_message":     {{"channel": "alpha", "body": "hi"}, {"channel": "nosuch", "body": "hi"}},
		"read_channel":     {{"channel": "alpha"}, {"channel": "nosuch"}},
		"request_approval": {approvalMCPArguments(f.alpha.ID, deadline), approvalMCPArguments(9999, deadline)},
		"check_decision":   {{"approval_id": f.approvalID}, {"approval_id": 9999}},
		"await_decision":   {{"approval_id": f.approvalID, "timeout_ms": 20}, {"approval_id": 9999, "timeout_ms": 20}},
	}
}

// TestMCPToolAuthorizationMatrix runs every MCP tool as every kind of agent:
// allowed, missing the capability, missing the channel permission, without a
// manifest, and not a member. It checks the answer, that nothing is written
// by a refused call, that a non-member sees exactly what an unknown target
// looks like, and that every denial is audited.
func TestMCPToolAuthorizationMatrix(t *testing.T) {
	f := newAuthzFixture(t)
	const (
		ok       = "ok"
		deny     = "forbidden"
		noChan   = "channel_not_found"
		noApprov = "approval_not_found"
	)
	tools := []string{"post_message", "read_channel", "request_approval", "check_decision", "await_decision"}
	want := map[string][5]string{
		agFull:          {ok, ok, ok, ok, ok},
		agMessagesOnly:  {ok, ok, deny, deny, deny},
		agReadOnly:      {deny, ok, deny, ok, ok},
		agNoManifest:    {deny, deny, deny, deny, deny},
		agOutsider:      {noChan, noChan, noChan, noApprov, noApprov},
		agMemberNoGrant: {deny, deny, deny, deny, deny},
	}
	wantReason := map[string][5]string{
		agMessagesOnly:  {"", "", denyCapability, denyCapability, denyCapability},
		agReadOnly:      {denyChannelPermission, "", denyChannelPermission, "", ""},
		agNoManifest:    {denyNoManifest, denyNoManifest, denyNoManifest, denyNoManifest, denyNoManifest},
		agOutsider:      {denyNotMember, denyNotMember, denyNotMember, denyNotMember, denyNotMember},
		agMemberNoGrant: {denyChannelPermission, denyChannelPermission, denyChannelPermission, denyChannelPermission, denyChannelPermission},
	}
	calls := f.toolCalls()
	agents := make([]string, 0, len(want))
	for name := range want {
		agents = append(agents, name)
	}
	sort.Strings(agents)

	// What an unknown target looks like to an agent allowed everything.
	unknown := map[string]string{}
	for _, tool := range tools {
		got := f.call(t, agFull, tool, calls[tool][1])
		if got.result() != noChan && got.result() != noApprov {
			t.Fatalf("%s on an unknown target = %s, want a not-found error", tool, got.result())
		}
		unknown[tool] = got.body
	}
	if n := len(f.denials(t)); n != 0 {
		t.Fatalf("unknown targets wrote %d access_denied events, want 0", n)
	}

	wantDenials := 0
	for _, agent := range agents {
		for i, tool := range tools {
			t.Run(agent+"/"+tool, func(t *testing.T) {
				before := len(f.denials(t))
				got := f.call(t, agent, tool, calls[tool][0])
				if got.result() != want[agent][i] {
					t.Fatalf("result = %s, want %s (body %s)", got.result(), want[agent][i], got.body)
				}
				events := f.denials(t)[before:]
				if want[agent][i] == ok {
					if len(events) != 0 {
						t.Errorf("an allowed call wrote an access_denied event: %+v", events)
					}
					return
				}
				wantDenials++
				if agent == agOutsider && got.body != unknown[tool] {
					t.Errorf("a non-member's response differs from an unknown target's\n non-member: %s\n unknown:    %s", got.body, unknown[tool])
				}
				if len(events) != 1 {
					t.Fatalf("access_denied events = %d, want 1", len(events))
				}
				e := events[0]
				capability, _ := schema.MCPToolCapability(tool)
				wantActor := fmt.Sprintf("principal:%d", f.agents[agent].ID)
				if e.Actor != wantActor || e.Subject != "mcp:"+tool ||
					!strings.Contains(e.Detail, "capability="+string(capability)) ||
					!strings.Contains(e.Detail, "reason="+wantReason[agent][i]) {
					t.Errorf("audit event = %+v, want actor %s subject mcp:%s capability %s reason %s", e, wantActor, tool, capability, wantReason[agent][i])
				}
			})
		}
	}
	if got := len(f.denials(t)); got != wantDenials {
		t.Errorf("access_denied events = %d, want %d", got, wantDenials)
	}

	// Refused calls wrote nothing: only the two agents allowed to post did,
	// and only the one allowed to request raised an approval beside the seed.
	ctx := context.Background()
	messages, err := f.srv.store.ListMessages(ctx, f.alpha.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var authors []string
	for _, m := range messages {
		for name, p := range f.agents {
			if p.ID == m.AuthorID {
				authors = append(authors, name)
			}
		}
	}
	sort.Strings(authors)
	if got := strings.Join(authors, ","); got != agFull+","+agMessagesOnly {
		t.Errorf("message authors = %s, want only the two agents allowed to post", got)
	}
	open, err := f.srv.store.ListOpenApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("open approvals = %d, want the seed plus one from the agent allowed to request", len(open))
	}
	for _, e := range f.denials(t) {
		if strings.Contains(e.Actor+e.Subject+e.Detail, "tok-") {
			t.Errorf("an audit event contains a bearer token: %+v", e)
		}
	}
}

// A tool with no capability mapping has no access, whatever the manifest says.
func TestUnmappedToolFailsClosed(t *testing.T) {
	f := newAuthzFixture(t)
	scope, serr := f.srv.newAgentScope(context.Background(), f.agents[agFull].ID, "delete_everything")
	if scope != nil || serr == nil || serr.Code != "forbidden" {
		t.Fatalf("newAgentScope for an unmapped tool = %v, %v; want nil and forbidden", scope, serr)
	}
	events := f.denials(t)
	if len(events) != 1 || events[0].Subject != "mcp:delete_everything" || !strings.Contains(events[0].Detail, "reason="+denyUnmappedTool) {
		t.Fatalf("audit = %+v, want one unmapped_tool denial", events)
	}
}

// Every tool the MCP server offers must have a capability mapping, and every
// mapped name must be a real tool.
func TestEveryMCPToolIsMapped(t *testing.T) {
	f := newAuthzFixture(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-"+agFull)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list = %d %s", rec.Code, rec.Body)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var offered, mapped []string
	for _, tool := range response.Result.Tools {
		offered = append(offered, tool.Name)
	}
	for name := range schema.MCPToolCapabilities() {
		mapped = append(mapped, name)
	}
	sort.Strings(offered)
	sort.Strings(mapped)
	if len(offered) == 0 || strings.Join(offered, ",") != strings.Join(mapped, ",") {
		t.Fatalf("tools offered = %v, tools mapped to a capability = %v; they must be the same set", offered, mapped)
	}
}

// The MCP tools must reach channels and approvals only through agentScope,
// and register only through addAgentTool. This pins that structurally: a
// direct store lookup or a bare mcp.AddTool in mcp.go would bypass the policy.
func TestMCPToolsOnlyUseTheAuthorizationScope(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("mcp.go"))
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, forbidden := range []string{"s.store.ChannelByName(", "s.store.ChannelByID(", "s.store.ApprovalByID(", "s.store.IsChannelMember(", "s.store.AgentManifestByPrincipal("} {
		if strings.Contains(code, forbidden) {
			t.Errorf("mcp.go calls %s directly; channels, approvals, membership and manifests must go through agentScope (authz.go)", forbidden)
		}
	}
	if n := strings.Count(code, "mcp.AddTool("); n != 1 {
		t.Errorf("mcp.go has %d mcp.AddTool calls, want exactly 1 (inside addAgentTool)", n)
	}
}

// An agent waiting on an approval stops being told about it as soon as it is
// removed from the approval's channel.
func TestAwaitDecisionEndsWhenMembershipIsRemoved(t *testing.T) {
	f := newAuthzFixture(t)
	done := make(chan mcpOutcome, 1)
	go func() {
		done <- f.call(t, agFull, "await_decision", map[string]any{"approval_id": f.approvalID, "timeout_ms": 30000})
	}()
	// Let the wait start polling, then remove the agent from the channel.
	time.Sleep(50 * time.Millisecond)
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.alpha.ID, f.agents[agFull].ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.result() != "approval_not_found" {
			t.Fatalf("await after removal = %s, want approval_not_found (body %s)", got.result(), got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await_decision kept waiting after the agent was removed from the channel")
	}
}

// With authentication required, a REST or WebSocket request made with an
// agent's credential is held to the agent's manifest too.
func TestAgentRESTAndWebSocketFollowManifest(t *testing.T) {
	f := newMemberFixture(t) // bot: member of general; manifest allows general and alpha
	base := wsTestServer(t, f.srv)
	readOnly := []schema.ChannelPermission{schema.ChannelPermissionRead}
	manifest := func(caps []schema.Capability, perms []schema.ChannelPermission) {
		t.Helper()
		req := schema.PutAgentManifestRequestV1{DisplayName: "bot", Tier: schema.AgentTierC, Capabilities: caps,
			Channels: []schema.ChannelGrant{{ChannelID: f.general.ID, Permissions: perms}}}
		if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.bot.ID, req); err != nil {
			t.Fatal(err)
		}
	}
	get := func() int { return f.do(t, "GET", "/v1/channels/general/messages", f.botTok, "").Code }
	post := func() int {
		return f.do(t, "POST", "/v1/channels/general/messages", f.botTok, `{"body":"from the agent"}`).Code
	}
	ws := func() int { return callWS(t, base, "/v1/ws?channel=general", f.botTok).status }

	// Full manifest: everything works.
	if g, p, w := get(), post(), ws(); g != 200 || p != 201 || w != http.StatusSwitchingProtocols {
		t.Fatalf("full manifest: get %d post %d ws %d", g, p, w)
	}
	// Read-only in the channel: reading and subscribing work, posting is refused.
	manifest(schema.Capabilities(), readOnly)
	if g, p, w := get(), post(), ws(); g != 200 || p != http.StatusForbidden || w != http.StatusSwitchingProtocols {
		t.Fatalf("read-only grant: get %d post %d ws %d, want 200 403 101", g, p, w)
	}
	// Without the read capability: reading and subscribing are refused.
	manifest([]schema.Capability{schema.CapabilityMessagesPost}, []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost})
	if g, p, w := get(), post(), ws(); g != http.StatusForbidden || p != 201 || w != http.StatusForbidden {
		t.Fatalf("no read capability: get %d post %d ws %d, want 403 201 403", g, p, w)
	}
	// A member human is unaffected by any of this.
	if rec := f.do(t, "GET", "/v1/channels/general/messages", f.rootTok, ""); rec.Code != 200 {
		t.Fatalf("human read = %d", rec.Code)
	}

	// The refusals were audited with the route as subject.
	var reasons []string
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", f.bot.ID) {
			if !strings.HasPrefix(e.Subject, "GET /") && !strings.HasPrefix(e.Subject, "POST /") {
				t.Errorf("audit subject = %q, want a route pattern", e.Subject)
			}
			reasons = append(reasons, e.Detail[strings.Index(e.Detail, "reason=")+len("reason="):])
		}
	}
	sort.Strings(reasons)
	if got := strings.Join(reasons, ","); got != denyCapability+","+denyCapability+","+denyChannelPermission {
		t.Errorf("denial reasons = %s", got)
	}
}

// An agent with a credential but no manifest can do nothing over REST, and
// the approval REST routes refuse every agent.
func TestAgentWithoutManifestAndApprovalRoutes(t *testing.T) {
	f := newMemberFixture(t)
	ctx := context.Background()
	bare, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "bare")
	if err != nil {
		t.Fatal(err)
	}
	_, bareTok, err := f.srv.store.CreateCredential(ctx, "system", bare.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, bare.ID, 0); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "GET", "/v1/channels/general/messages", bareTok, ""); rec.Code != http.StatusForbidden {
		t.Errorf("agent with no manifest: read = %d, want 403", rec.Code)
	}
	// A channel it is not in still looks like it does not exist.
	if rec := f.do(t, "GET", "/v1/channels/alpha/messages", bareTok, ""); rec.Code != http.StatusNotFound {
		t.Errorf("agent with no manifest, not a member: read = %d, want 404", rec.Code)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/approvals", ""},
		{"POST", "/v1/approvals", createApprovalBody(f.general.ID, f.bot.ID)},
		{"POST", "/v1/approvals/1/decisions", fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"x"}`, f.alice.ID)},
	} {
		for _, tok := range []string{f.botTok, bareTok} {
			rec := f.do(t, c.method, c.path, tok, c.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with an agent credential = %d %s, want 403", c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	if open, err := f.srv.store.ListOpenApprovals(ctx); err != nil || len(open) != 0 {
		t.Errorf("an agent raised an approval over REST: %d open (err %v)", len(open), err)
	}
	// A human can still use them.
	if rec := f.do(t, "GET", "/v1/approvals", f.aliceTok, ""); rec.Code != 200 {
		t.Errorf("human approvals list = %d", rec.Code)
	}
}

// A webhook hook bound to an agent posts as that agent, so under
// authentication it needs the agent's manifest to allow posting there.
func TestHookForAgentFollowsManifest(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)
	if _, err := f.srv.store.CreateHook(ctx, "agent-hook", f.general.ID, f.bot.ID); err != nil {
		t.Fatal(err)
	}
	unknown := f.do(t, "POST", "/v1/hooks/no-such-token", "", `{"body":"x"}`)
	if rec := f.do(t, "POST", "/v1/hooks/agent-hook", "", `{"body":"allowed"}`); rec.Code != http.StatusCreated {
		t.Fatalf("hook with an allowing manifest = %d %s", rec.Code, rec.Body)
	}
	setAgentManifest(t, f.srv, f.bot.ID, []schema.Capability{schema.CapabilityMessagesRead}, f.general.ID)
	rec := f.do(t, "POST", "/v1/hooks/agent-hook", "", `{"body":"refused"}`)
	if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
		t.Fatalf("hook after the manifest stopped allowing posts = %d %s, want the unknown-token response", rec.Code, rec.Body)
	}
	messages, err := f.srv.store.ListMessages(ctx, f.general.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want only the allowed one", len(messages))
	}
}

// At startup conchd says how many agents have no manifest, because upgrading
// is deny-by-default and those agents can do nothing.
func TestStartupReportsAgentsWithoutManifest(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	channel, err := st.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	with, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "with-manifest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "without-manifest"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "a-human"); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	New(Config{Listen: "127.0.0.1:0"}, st)
	if !strings.Contains(logs.buf.String(), "agents=2") {
		t.Errorf("startup log = %q, want it to report 2 agents without a manifest", logs.buf.String())
	}

	req := schema.PutAgentManifestRequestV1{DisplayName: "x", Tier: schema.AgentTierC, Channels: []schema.ChannelGrant{{ChannelID: channel.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}}}
	if _, _, err := st.PutAgentManifest(ctx, "system", with.ID, req); err != nil {
		t.Fatal(err)
	}
	logs.buf.Reset()
	New(Config{Listen: "127.0.0.1:0"}, st)
	if !strings.Contains(logs.buf.String(), "agents=1") {
		t.Errorf("startup log = %q, want it to report 1 agent without a manifest", logs.buf.String())
	}
}
