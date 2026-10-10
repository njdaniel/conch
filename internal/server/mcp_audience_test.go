package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Audiences on the MCP tools (issue #117). The leak test for read_channel is
// the "mcp read_channel" column of TestScopedMessagesExactSets.

func mcpNet(id int64) map[string]any { return map[string]any{"kind": "net", "net_id": id} }

func mcpWhisper(ids ...int64) map[string]any {
	return map[string]any{"kind": "principals", "principal_ids": ids}
}

// mcpPost calls post_message as who and, when the call succeeded, decodes the
// message it returned as a v2 envelope.
func (f *scopedFixture) mcpPost(t *testing.T, who, body string, audience map[string]any) (mcpOutcome, schema.MessageV2) {
	t.Helper()
	args := map[string]any{"channel": "ops", "body": body}
	if audience != nil {
		args["audience"] = audience
	}
	out := f.mcpCall(t, f.p(who).token, "post_message", args)
	if out.status != http.StatusOK || out.isError {
		return out, schema.MessageV2{}
	}
	var resp struct {
		Result struct {
			Structured schema.PostMessageResponseV2 `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatalf("decode post_message %s: %v", out.body, err)
	}
	if err := resp.Result.Structured.Message.Validate(); err != nil {
		t.Fatalf("post_message returned an invalid v2 message: %v (%s)", err, out.body)
	}
	return out, resp.Result.Structured.Message
}

// mcpRead calls read_channel as who and decodes one page of v2 envelopes.
func (f *scopedFixture) mcpRead(t *testing.T, who string, after int64, limit int) ([]schema.MessageV2, int64) {
	t.Helper()
	out := f.mcpCall(t, f.p(who).token, "read_channel", map[string]any{"channel": "ops", "after": after, "limit": limit})
	if out.status != http.StatusOK || out.isError {
		t.Fatalf("%s read_channel = %s (%s)", who, out.result(), out.body)
	}
	var resp struct {
		Result struct {
			Structured schema.ListMessagesResponseV2 `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatalf("decode read_channel %s: %v", out.body, err)
	}
	for _, m := range resp.Result.Structured.Messages {
		if err := m.Validate(); err != nil {
			t.Fatalf("read_channel returned an invalid v2 message: %v (%s)", err, out.body)
		}
	}
	return resp.Result.Structured.Messages, resp.Result.Structured.NextAfter
}

// mcpRaw calls a tool and returns the raw JSON-RPC response and whether the
// call was refused, for calls the SDK itself rejects (its error text is not a
// schema.Error, which mcpCall insists on).
func (f *scopedFixture) mcpRaw(t *testing.T, who, tool string, args map[string]any) (refused bool, body string) {
	t.Helper()
	call := mustJSON(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(call))
	req.Header.Set("Authorization", "Bearer "+f.p(who).token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	var response struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s response %s: %v", tool, rec.Body, err)
	}
	return rec.Code != http.StatusOK || response.Error != nil || response.Result.IsError, rec.Body.String()
}

func messageIDs(messages []schema.MessageV2) []int64 {
	ids := []int64{}
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func (f *scopedFixture) denials(t *testing.T) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" {
			out = append(out, e)
		}
	}
	return out
}

// TestMCPAudienceAuthorizationMatrix is the audience half of
// TestMCPToolAuthorizationMatrix: post_message with each kind of audience, as
// an agent in each grant and seat situation. It checks the answer, that a
// refused call stores nothing, that each manifest or membership refusal writes
// exactly one access_denied event with the right reason, and that an allowed
// call writes none.
func TestMCPAudienceAuthorizationMatrix(t *testing.T) {
	f := newScopedFixture(t)
	type agentSpec struct {
		caps   []schema.Capability
		perms  []schema.ChannelPermission
		seat   schema.NetRole // on net1; "" for none
		member bool           // of the channel
	}
	all := []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}
	agents := map[string]agentSpec{
		"gnet":       {perms: []schema.ChannelPermission{schema.ChannelPermissionPostNet}, seat: schema.NetRoleMember, member: true},
		"gmonitor":   {perms: all, seat: schema.NetRoleMonitor, member: true},
		"goffnet":    {perms: all, member: true},
		"gwhisper":   {perms: []schema.ChannelPermission{schema.ChannelPermissionWhisper}, seat: schema.NetRoleMember, member: true},
		"gboth":      {perms: []schema.ChannelPermission{schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}, seat: schema.NetRoleMember, member: true},
		"gagentonly": {perms: []schema.ChannelPermission{schema.ChannelPermissionWhisperAgent}, seat: schema.NetRoleMember, member: true},
		"gpost":      {perms: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}, seat: schema.NetRoleMember, member: true},
		"gnocap":     {caps: []schema.Capability{schema.CapabilityMessagesRead}, perms: all, seat: schema.NetRoleMember, member: true},
		"goutside":   {perms: all},
	}
	for name, spec := range agents {
		f.add(t, name, store.PrincipalAgent, spec.member)
		f.manifest(t, name, spec.caps, spec.perms...)
		if spec.seat != "" {
			f.seat(t, "net1", name, spec.seat)
		}
	}
	f.add(t, "gnomanifest", store.PrincipalAgent, true)
	f.seat(t, "net1", "gnomanifest", schema.NetRoleMember)
	archived := f.net(t, "gone")
	f.seat(t, "gone", "goffnet", schema.NetRoleMember)
	if _, err := f.srv.store.ArchiveNet(context.Background(), "system", f.ops.ID, "gone"); err != nil {
		t.Fatal(err)
	}

	net1, net2 := mcpNet(f.net1.ID), mcpNet(f.net2.ID)
	human, agent := mcpWhisper(f.p("wes").ID), mcpWhisper(f.p("aria").ID)
	mixed := mcpWhisper(f.p("wes").ID, f.p("aria").ID)
	const ok = "ok"
	tests := []struct {
		name     string
		who      string
		audience map[string]any
		want     string // "ok" or the error code
		reason   string // the access_denied reason, "" when none is written
	}{
		{"post_net on its own net", "gnet", net1, ok, ""},
		{"a monitor may not transmit", "gmonitor", net1, "forbidden", denyNetMonitorOnly},
		{"a net the agent is not on", "goffnet", net1, "net_not_found", denyNetNotOn},
		{"a net that does not exist", "goffnet", mcpNet(999999), "net_not_found", denyNetNotOn},
		{"an archived net the agent was on", "goffnet", mcpNet(archived.ID), "net_not_found", denyNetNotOn},
		{"post_net does not reach a net the agent is not on", "gnet", net2, "net_not_found", denyNetNotOn},
		{"whisper does not allow a net post", "gwhisper", net1, "forbidden", denyAudienceGrant},
		{"channel-wide post does not allow a net post", "gpost", net1, "forbidden", denyAudienceGrant},
		{"the capability is needed for a net post", "gnocap", net1, "forbidden", denyCapability},
		{"no manifest", "gnomanifest", net1, "forbidden", denyNoManifest},
		{"not a channel member, net post", "goutside", net1, "channel_not_found", denyNotMember},

		{"whisper to a human", "gwhisper", human, ok, ""},
		{"whisper alone does not reach an agent", "gwhisper", agent, "forbidden", denyAudienceGrant},
		{"whisper alone does not reach a mixed list", "gwhisper", mixed, "forbidden", denyAudienceGrant},
		{"whisper_agent alone is not a whisper grant", "gagentonly", agent, "forbidden", denyAudienceGrant},
		{"whisper and whisper_agent reach an agent", "gboth", agent, ok, ""},
		{"whisper and whisper_agent reach a mixed list", "gboth", mixed, ok, ""},
		{"whisper_agent is not needed for a human", "gboth", human, ok, ""},
		{"post_net does not allow a whisper", "gnet", human, "forbidden", denyAudienceGrant},
		{"channel-wide post does not allow a whisper", "gpost", human, "forbidden", denyAudienceGrant},
		{"the capability is needed for a whisper", "gnocap", human, "forbidden", denyCapability},
		{"not a channel member, whisper", "goutside", human, "channel_not_found", denyNotMember},
		{"whisper to someone outside the channel", "gboth", mcpWhisper(f.p("nora").ID), "invalid_audience", ""},
		{"whisper to an agent outside the channel", "gwhisper", mcpWhisper(f.p("nomad").ID), "invalid_audience", ""},
		{"whisper to an id that is no principal", "gwhisper", mcpWhisper(999999), "invalid_audience", ""},
		{"whisper to itself only", "gboth", mcpWhisper(f.p("gboth").ID), "invalid_audience", ""},
		{"whisper naming a recipient twice", "gboth", mcpWhisper(f.p("wes").ID, f.p("wes").ID), "invalid_audience", ""},
		{"whisper at the size limit before the author is added", "gboth", mcpWhisper(manyIDs(schema.MaxAudiencePrincipals)...), "invalid_audience", ""},
		{"whisper over the size limit", "gboth", mcpWhisper(manyIDs(schema.MaxAudiencePrincipals + 1)...), "invalid_audience", ""},

		{"unknown audience kind", "gboth", map[string]any{"kind": "everyone"}, "invalid_audience", ""},
		{"net audience with principal ids", "gnet", map[string]any{"kind": "net", "net_id": f.net1.ID, "principal_ids": []int64{f.p("wes").ID}}, "invalid_audience", ""},
		{"net audience without a net", "gnet", map[string]any{"kind": "net"}, "invalid_audience", ""},
		{"whisper without recipients", "gboth", map[string]any{"kind": "principals"}, "invalid_audience", ""},
		{"an invalid audience says nothing about the channel", "goutside", map[string]any{"kind": "everyone"}, "invalid_audience", ""},

		{"post_net alone does not allow a channel-wide post", "gnet", nil, "forbidden", denyChannelPermission},
		{"post allows a channel-wide post", "gpost", nil, ok, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeDenials, beforeMsgs, beforeScoped := len(f.denials(t)), f.messageCount(t), f.auditCount(t, store.AuditMessageScoped)
			out, message := f.mcpPost(t, tt.who, "SECRET-matrix", tt.audience)
			if out.result() != tt.want {
				t.Fatalf("result = %s, want %s (body %s)", out.result(), tt.want, out.body)
			}
			events := f.denials(t)[beforeDenials:]
			wantDenials := 0
			if tt.reason != "" {
				wantDenials = 1
			}
			if len(events) != wantDenials {
				t.Fatalf("access_denied events = %d, want %d: %+v", len(events), wantDenials, events)
			}
			if wantDenials == 1 {
				e := events[0]
				wantActor := fmt.Sprintf("principal:%d", f.p(tt.who).ID)
				// A missing capability or manifest is refused before the
				// channel is looked at, so it names no target.
				wantTarget := fmt.Sprintf("target=channel:%d", f.ops.ID)
				if tt.reason == denyCapability || tt.reason == denyNoManifest {
					wantTarget = "target=none"
				}
				wantDetail := fmt.Sprintf("capability=%s %s reason=%s", schema.CapabilityMessagesPost, wantTarget, tt.reason)
				if e.Actor != wantActor || e.Subject != "mcp:post_message" || e.Detail != wantDetail {
					t.Errorf("audit event = %+v, want actor %s subject mcp:post_message detail %q", e, wantActor, wantDetail)
				}
			}
			wantMsgs, wantScoped := 0, 0
			if tt.want == ok {
				wantMsgs = 1
				if tt.audience != nil {
					wantScoped = 1
				}
				if message.AuthorID != f.p(tt.who).ID || message.ChannelID != f.ops.ID || message.Schema != schema.MessageSchemaV2 {
					t.Errorf("message = %+v, want the agent as author in ops, schema v2", message)
				}
				if (message.Audience != nil) != (tt.audience != nil) {
					t.Errorf("message audience = %+v, posted with %v", message.Audience, tt.audience)
				}
			} else if strings.Contains(out.body, "SECRET") {
				t.Errorf("a refusal echoes the body: %s", out.body)
			}
			if n := f.messageCount(t) - beforeMsgs; n != wantMsgs {
				t.Errorf("messages stored = %d, want %d", n, wantMsgs)
			}
			if n := f.auditCount(t, store.AuditMessageScoped) - beforeScoped; n != wantScoped {
				t.Errorf("message_scoped events = %d, want %d", n, wantScoped)
			}
		})
	}

	// A net the agent is not on, one that does not exist and one that is
	// archived are one answer, byte for byte; so are an unknown channel and
	// one the agent is not a member of.
	same := func(label string, a, b mcpOutcome) {
		t.Helper()
		if a.body != b.body {
			t.Errorf("%s differ:\n %s\n %s", label, a.body, b.body)
		}
	}
	notOn, _ := f.mcpPost(t, "goffnet", "x", net1)
	unknownNet, _ := f.mcpPost(t, "goffnet", "x", mcpNet(999999))
	archivedNet, _ := f.mcpPost(t, "goffnet", "x", mcpNet(archived.ID))
	same("a net the agent is not on and an unknown net", notOn, unknownNet)
	same("an archived net and an unknown net", archivedNet, unknownNet)
	// The audit rows for those three are the same too, but for id and time:
	// the log no more tells an unknown net from one the agent is not on than
	// the answer does, and carries no net id.
	rows := f.denials(t)
	last3 := rows[len(rows)-3:]
	for _, e := range last3 {
		if e.Actor != last3[0].Actor || e.Subject != last3[0].Subject || e.Detail != last3[0].Detail || !strings.HasSuffix(e.Detail, "reason="+denyNetNotOn) {
			t.Errorf("net_not_on audit rows differ: %+v vs %+v", e, last3[0])
		}
		// No net id at all: an agent walking net ids must not be able to
		// write numbers of its choosing into the log.
		if strings.Contains(e.Detail, "net=") || strings.Contains(e.Detail, "999999") || strings.Contains(e.Detail, fmt.Sprintf(":%d ", archived.ID)) {
			t.Errorf("an audit row names a net: %+v", e)
		}
	}
	notMember, _ := f.mcpPost(t, "goutside", "x", net1)
	unknownChannel := f.mcpCall(t, f.p("goutside").token, "post_message", map[string]any{"channel": "nosuch", "body": "x", "audience": net1})
	same("a channel the agent is not in and an unknown channel", notMember, unknownChannel)

	for _, e := range f.audit(t) {
		if strings.Contains(e.Actor+e.Subject+e.Detail, "SECRET") {
			t.Errorf("an audit event carries a message body: %+v", e)
		}
	}
}

// manyIDs returns n distinct principal ids that belong to nobody.
func manyIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(900000 + i)
	}
	return ids
}

// The MCP tool and the REST post refuse the same inputs with the same code.
// Both end in the same functions, but each front end validates the request
// first; this holds them to one answer for the audience shapes where the order
// of those steps could matter.
func TestMCPAndRESTRefuseAudiencesAlike(t *testing.T) {
	f := newScopedFixture(t)
	f.add(t, "gboth", store.PrincipalAgent, true)
	f.manifest(t, "gboth", nil, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent)
	self := f.p("gboth").ID
	tests := []struct {
		name     string
		audience map[string]any
	}{
		{"unknown kind", map[string]any{"kind": "everyone"}},
		{"net without an id", map[string]any{"kind": "net"}},
		{"net with principal ids", map[string]any{"kind": "net", "net_id": f.net1.ID, "principal_ids": []int64{f.p("wes").ID}}},
		{"net the agent is not on", mcpNet(f.net1.ID)},
		{"net that does not exist", mcpNet(999999)},
		{"whisper to nobody", map[string]any{"kind": "principals"}},
		{"whisper to itself", mcpWhisper(self)},
		{"whisper with a duplicate", mcpWhisper(f.p("wes").ID, f.p("wes").ID)},
		{"whisper with a non-positive id", mcpWhisper(0)},
		{"whisper to a non-member", mcpWhisper(f.p("nora").ID)},
		{"whisper to an unknown id", mcpWhisper(999999)},
		{"whisper one under the limit", mcpWhisper(manyIDs(schema.MaxAudiencePrincipals - 1)...)},
		{"whisper at the limit, author not listed", mcpWhisper(manyIDs(schema.MaxAudiencePrincipals)...)},
		{"whisper at the limit, author listed", mcpWhisper(append(manyIDs(schema.MaxAudiencePrincipals-1), self)...)},
		{"whisper over the limit", mcpWhisper(manyIDs(schema.MaxAudiencePrincipals + 1)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.messageCount(t)
			viaMCP, _ := f.mcpPost(t, "gboth", "x", tt.audience)
			rest := f.postV2(t, "gboth", string(mustJSON(t, map[string]any{"body": "x", "audience": tt.audience})))
			var restErr schema.Error
			if err := json.Unmarshal([]byte(rest.body), &restErr); err != nil {
				t.Fatalf("decode REST refusal %s: %v", rest.body, err)
			}
			if viaMCP.result() == "ok" || rest.status < 400 {
				t.Fatalf("not refused: MCP %s, REST %d %s", viaMCP.result(), rest.status, rest.body)
			}
			if viaMCP.code != restErr.Code {
				t.Errorf("MCP answers %q and REST %q (%s)", viaMCP.code, restErr.Code, rest.body)
			}
			if n := f.messageCount(t) - before; n != 0 {
				t.Errorf("refused posts stored %d messages", n)
			}
		})
	}
}

// A scoped post over MCP is the same post as over REST: the same recipients
// resolved at post time, the same message_scoped audit event, the same v2
// envelope with the audience as stored, delivered to the same sockets.
func TestMCPScopedPostMatchesREST(t *testing.T) {
	f := newScopedFixture(t)
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	watch := map[string]*hubWatch{}
	for _, n := range []string{"ann", "nina", "mona", "wes", "olga", "aria"} {
		watch[n] = newHubWatch(f, f.p(n).ID)
	}
	recipientIDs := func(names ...string) []int64 {
		var ids []int64
		for _, n := range names {
			ids = append(ids, f.p(n).ID)
		}
		return sortedIDs(ids...)
	}
	auditDetail := func(id int64) store.AuditEvent {
		t.Helper()
		for _, e := range f.audit(t) {
			if e.Action == store.AuditMessageScoped && e.Subject == fmt.Sprintf("message:%d", id) {
				return e
			}
		}
		t.Fatalf("no message_scoped event for message %d", id)
		return store.AuditEvent{}
	}
	tests := []struct {
		name     string
		audience map[string]any
		rest     string
		want     schema.Audience
		receive  []string
	}{
		{"net", mcpNet(f.net1.ID), netAudience(f.net1.ID),
			schema.Audience{Kind: schema.AudienceKindNet, NetID: f.net1.ID},
			[]string{"ann", "nina", "mona", "aria"}},
		// The author is added to a whisper and the ids come back sorted,
		// whatever order the tool was given.
		{"whisper", mcpWhisper(f.p("wes").ID, f.p("ann").ID), whisperAudience(f.p("ann").ID, f.p("wes").ID),
			schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: recipientIDs("ann", "wes", "aria")},
			[]string{"ann", "wes", "aria"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, w := range watch {
				w.drain()
			}
			tap := len(f.rec.seen())
			out, viaMCP := f.mcpPost(t, "aria", "SECRET-"+tt.name, tt.audience)
			if out.result() != "ok" {
				t.Fatalf("post_message = %s (%s)", out.result(), out.body)
			}
			if viaMCP.Audience == nil || !reflect.DeepEqual(*viaMCP.Audience, tt.want) {
				t.Errorf("audience = %+v, want %+v", viaMCP.Audience, tt.want)
			}
			if strings.Contains(out.body, "recipient") {
				t.Errorf("the tool output names recipients: %s", out.body)
			}
			// Only the recipients' v2 subscriptions got it; no v0 or v1
			// subscription and not the Broadcaster seam.
			for name, w := range watch {
				isRecipient := false
				for _, r := range tt.receive {
					isRecipient = isRecipient || r == name
				}
				gotV2 := 0
				for drained := false; !drained; {
					select {
					case m := <-w.v2.Messages():
						if m.ID == viaMCP.ID {
							gotV2++
						}
					default:
						drained = true
					}
				}
				if want := map[bool]int{true: 1, false: 0}[isRecipient]; gotV2 != want {
					t.Errorf("%s got the message %d times on v2, want %d", name, gotV2, want)
				}
				if n := w.drain(); n != 0 {
					t.Errorf("%s got %d frames on a v0 or v1 subscription", name, n)
				}
			}
			if got := len(f.rec.seen()) - tap; got != 0 {
				t.Errorf("the Broadcaster seam saw %d messages, want 0", got)
			}
			// The same post over REST by the same agent: same audience, and
			// the audit events differ only in the message id.
			viaREST := f.mustPost(t, "aria", `{"body":"SECRET-rest",`+tt.rest+`}`)
			mcpEvent, restEvent := auditDetail(viaMCP.ID), auditDetail(viaREST)
			if mcpEvent.Detail != restEvent.Detail || mcpEvent.Actor != restEvent.Actor {
				t.Errorf("audit differs between MCP and REST:\n mcp:  %+v\n rest: %+v", mcpEvent, restEvent)
			}
			if !strings.Contains(mcpEvent.Detail, "recipients=") || mcpEvent.Actor != fmt.Sprintf("principal:%d", f.p("aria").ID) {
				t.Errorf("mcp audit = %+v", mcpEvent)
			}
		})
	}
}

// ADR-005 allows an audience made only of agents. Two agents on a net with no
// human on it exchange messages, by net and by whisper, each replying with the
// audience of the message it read. A third agent in the channel, every human
// and the operator see none of it, on any path.
func TestMCPAgentsOnANetWithNoHuman(t *testing.T) {
	f := newScopedFixture(t)
	talk := []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}
	for _, name := range []string{"bot1", "bot2", "bot3"} {
		f.add(t, name, store.PrincipalAgent, true)
		f.manifest(t, name, nil, talk...)
	}
	bots := f.net(t, "bots")
	f.seat(t, "bots", "bot1", schema.NetRoleMember)
	f.seat(t, "bots", "bot2", schema.NetRoleMember)

	humans := []string{"ann", "nina", "olga", "root"}
	sockets := map[string]*socket{}
	for _, who := range append([]string{"bot3"}, humans...) {
		for _, version := range []string{"v0", "v1", "v2"} {
			s := openSocket(t, f.base, "/"+version+"/ws?channel=ops", f.p(who).token)
			if s.status != http.StatusSwitchingProtocols {
				t.Fatalf("%s %s socket: %d %s", who, version, s.status, s.body)
			}
			sockets[who+"/"+version] = s
		}
	}

	_, wide := f.mcpPost(t, "bot3", "hello everyone", nil)
	out, first := f.mcpPost(t, "bot1", "SECRET-bots-1", mcpNet(bots.ID))
	if out.result() != "ok" {
		t.Fatalf("bot1 net post = %s (%s)", out.result(), out.body)
	}

	// bot2 reads the net message and replies in kind, reusing the audience
	// object exactly as it received it.
	page, _ := f.mcpRead(t, "bot2", 0, 100)
	var received *schema.MessageV2
	for i := range page {
		if page[i].ID == first.ID {
			received = &page[i]
		}
	}
	if received == nil || received.Audience == nil || received.Body != "SECRET-bots-1" {
		t.Fatalf("bot2 did not read bot1's net message with its audience: %+v", page)
	}
	// asReceived is an audience exactly as a tool result carried it, to be
	// sent back as a tool argument.
	asReceived := func(a *schema.Audience) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(mustJSON(t, a), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	out, reply := f.mcpPost(t, "bot2", "SECRET-bots-2", asReceived(received.Audience))
	if out.result() != "ok" || reply.Audience == nil || !reflect.DeepEqual(*reply.Audience, *received.Audience) {
		t.Fatalf("bot2 reply in kind = %s, audience %+v, want %+v", out.result(), reply.Audience, received.Audience)
	}

	// A whisper between the two agents, and the reply to it.
	out, whisper := f.mcpPost(t, "bot1", "SECRET-bots-3", mcpWhisper(f.p("bot2").ID))
	if out.result() != "ok" {
		t.Fatalf("bot1 whisper = %s (%s)", out.result(), out.body)
	}
	out, whisperReply := f.mcpPost(t, "bot2", "SECRET-bots-4", asReceived(whisper.Audience))
	if out.result() != "ok" || !reflect.DeepEqual(whisperReply.Audience, whisper.Audience) {
		t.Fatalf("bot2 whisper reply = %s, audience %+v, want %+v (%s)", out.result(), whisperReply.Audience, whisper.Audience, out.body)
	}
	_, sentinel := f.mcpPost(t, "bot3", "goodbye everyone", nil)

	scoped := []int64{first.ID, reply.ID, whisper.ID, whisperReply.ID}
	everything := append([]int64{wide.ID}, append(scoped, sentinel.ID)...)
	onlyWide := []int64{wide.ID, sentinel.ID}

	for _, who := range []string{"bot1", "bot2"} {
		got, _ := f.mcpRead(t, who, 0, 100)
		if !reflect.DeepEqual(messageIDs(got), everything) {
			t.Errorf("%s read %v, want %v", who, messageIDs(got), everything)
		}
	}
	// The third agent: same channel, same grants, on no audience.
	got, _ := f.mcpRead(t, "bot3", 0, 100)
	if !reflect.DeepEqual(messageIDs(got), onlyWide) {
		t.Errorf("bot3 read %v over MCP, want only %v", messageIDs(got), onlyWide)
	}
	for _, m := range got {
		if m.Audience != nil {
			t.Errorf("bot3 was given a message with an audience: %+v", m)
		}
	}
	for _, who := range append([]string{"bot3"}, humans...) {
		for _, version := range []string{"v0", "v1", "v2"} {
			res := f.callREST(t, "GET", "/"+version+"/channels/ops/messages?limit=100", f.p(who).token, "")
			if ids, _ := listIDs(t, res.body); res.status != http.StatusOK || !reflect.DeepEqual(ids, onlyWide) || strings.Contains(res.body, "SECRET") {
				t.Errorf("%s %s list = %d %s, want only %v", who, version, res.status, res.body, onlyWide)
			}
			frames := sockets[who+"/"+version].readUntil(t, sentinel.ID)
			if ids := idsOf(t, frames); !reflect.DeepEqual(ids, onlyWide) {
				t.Errorf("%s %s socket got %v, want only %v", who, version, ids, onlyWide)
			}
			for _, frame := range frames {
				if bytes.Contains(frame, []byte("SECRET")) {
					t.Errorf("%s %s socket leaked a body: %s", who, version, frame)
				}
			}
		}
	}
	// The audit log names the agents as the only recipients.
	want := fmt.Sprintf("channel=%d audience=net net=%d recipients=%d,%d", f.ops.ID, bots.ID, f.p("bot1").ID, f.p("bot2").ID)
	found := 0
	for _, e := range f.audit(t) {
		if e.Action == store.AuditMessageScoped && e.Detail == want {
			found++
		}
	}
	if found != 2 {
		t.Errorf("message_scoped events with detail %q = %d, want 2", want, found)
	}
}

// Paging with after and next_after works across scoped messages the agent
// cannot see: every page is full until the last, no id is skipped or repeated,
// and next_after is always an id the agent was given.
func TestMCPReadChannelPagingAcrossScopedMessages(t *testing.T) {
	f := newScopedFixture(t)
	ids := f.postScopedCorpus(t)
	for i := 0; i < 4; i++ {
		f.mustPost(t, "olga", fmt.Sprintf(`{"body":"extra wide %d"}`, i))
		f.mustPost(t, "ann", `{"body":"extra net",`+netAudience(f.net1.ID)+`}`)
		f.mustPost(t, "nina", `{"body":"extra net2",`+netAudience(f.net2.ID)+`}`)
		f.mustPost(t, "ann", `{"body":"extra whisper",`+whisperAudience(f.p("wes").ID)+`}`)
	}
	f.add(t, "plain", store.PrincipalAgent, true) // on no audience at all
	f.manifest(t, "plain", nil, schema.ChannelPermissionRead)
	hidden := map[int64]string{ids[msgNet2]: msgNet2, ids[msgWhisper1]: msgWhisper1}
	for _, who := range []string{"aria", "plain"} {
		full, next := f.mcpRead(t, who, 0, 100)
		if next != 0 {
			t.Fatalf("%s: a 100-limit page still has next_after %d", who, next)
		}
		want := messageIDs(full)
		for _, id := range want {
			if label, ok := hidden[id]; ok {
				t.Errorf("%s was given %s", who, label)
			}
		}
		for _, limit := range []int{1, 2, 3, 7} {
			t.Run(fmt.Sprintf("%s/limit %d", who, limit), func(t *testing.T) {
				var got []int64
				after := int64(0)
				for pages := 0; ; pages++ {
					if pages > 200 {
						t.Fatal("paging did not terminate")
					}
					page, nextAfter := f.mcpRead(t, who, after, limit)
					got = append(got, messageIDs(page)...)
					if nextAfter == 0 {
						break
					}
					if len(page) != limit || nextAfter != page[len(page)-1].ID {
						t.Fatalf("next_after %d does not follow a full page %v", nextAfter, messageIDs(page))
					}
					after = nextAfter
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("paged %v, want %v", got, want)
				}
			})
		}
	}
	// aria is on net1 and in whisper2; plain sees channel-wide messages only.
	aria, _ := f.mcpRead(t, "aria", 0, 100)
	plain, _ := f.mcpRead(t, "plain", 0, 100)
	if len(aria) != len(plain)+1+4+1 {
		t.Errorf("aria read %d messages and plain %d; want aria to have net1, four more net1 posts and whisper2 on top", len(aria), len(plain))
	}
	for _, m := range plain {
		if m.Audience != nil {
			t.Errorf("an agent on no audience was given %+v", m)
		}
	}
}

// Losing the seat or the channel takes effect on the next call: a scoped
// message already received on a net stays readable after leaving the net
// (recipients are resolved at post time), nothing later is, and nothing at all
// after leaving the channel.
func TestMCPReadFollowsMembership(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	before := f.mustPost(t, "ann", `{"body":"SECRET-before",`+netAudience(f.net1.ID)+`}`)
	if _, err := f.srv.store.RemoveNetMember(ctx, "system", f.ops.ID, "net1", f.p("aria").ID); err != nil {
		t.Fatal(err)
	}
	after := f.mustPost(t, "ann", `{"body":"SECRET-after",`+netAudience(f.net1.ID)+`}`)
	got, _ := f.mcpRead(t, "aria", 0, 100)
	if !reflect.DeepEqual(messageIDs(got), []int64{before}) {
		t.Errorf("after leaving the net aria read %v, want only %d (not %d)", messageIDs(got), before, after)
	}
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPostNet)
	if out, _ := f.mcpPost(t, "aria", "x", mcpNet(f.net1.ID)); out.result() != "net_not_found" {
		t.Errorf("post to a net aria left = %s, want net_not_found", out.result())
	}
	if _, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.p("aria").ID); err != nil {
		t.Fatal(err)
	}
	out := f.mcpCall(t, f.p("aria").token, "read_channel", map[string]any{"channel": "ops"})
	if out.result() != "channel_not_found" || strings.Contains(out.body, "SECRET") {
		t.Errorf("read after leaving the channel = %s (%s)", out.result(), out.body)
	}
}

// The tool definitions the server publishes are the shapes in code: an agent
// framework builds its calls from tools/list, and the SDK refuses an argument
// that is not in the published input schema.
func TestMCPToolDefinitionsCarryAudience(t *testing.T) {
	f := newScopedFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+f.p("aria").token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list = %d %s", rec.Code, rec.Body)
	}
	type object struct {
		Type       any                        `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Items      json.RawMessage            `json:"items"`
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name         string `json:"name"`
				InputSchema  object `json:"inputSchema"`
				OutputSchema object `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	decode := func(raw json.RawMessage) object {
		t.Helper()
		var o object
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("decode schema %s: %v", raw, err)
		}
		return o
	}
	checkAudience := func(label string, raw json.RawMessage) {
		t.Helper()
		if raw == nil {
			t.Fatalf("%s has no audience property", label)
		}
		a := decode(raw)
		for _, field := range []string{"kind", "net_id", "principal_ids"} {
			if _, ok := a.Properties[field]; !ok {
				t.Errorf("%s audience has no %s property: %s", label, field, raw)
			}
		}
		if len(a.Properties) != 3 {
			t.Errorf("%s audience has properties %v, want exactly kind, net_id, principal_ids", label, a.Properties)
		}
		if !reflect.DeepEqual(a.Required, []string{"kind"}) {
			t.Errorf("%s audience requires %v, want only kind", label, a.Required)
		}
	}
	seen := map[string]bool{}
	for _, tool := range response.Result.Tools {
		seen[tool.Name] = true
		switch tool.Name {
		case "post_message":
			checkAudience("post_message input", tool.InputSchema.Properties["audience"])
			for _, r := range tool.InputSchema.Required {
				if r == "audience" {
					t.Error("post_message requires an audience; a channel-wide post has none")
				}
			}
			checkAudience("post_message output", decode(tool.OutputSchema.Properties["message"]).Properties["audience"])
		case "read_channel":
			if _, ok := tool.InputSchema.Properties["audience"]; ok {
				t.Error("read_channel takes an audience; what an agent may read is not for it to choose")
			}
			messages := decode(tool.OutputSchema.Properties["messages"])
			checkAudience("read_channel output", decode(messages.Items).Properties["audience"])
		default:
			// The approval tools are unchanged: approvals stay channel-wide.
			if strings.Contains(string(mustJSON(t, tool.InputSchema.Properties)), "audience") {
				t.Errorf("%s takes an audience", tool.Name)
			}
		}
	}
	if !seen["post_message"] || !seen["read_channel"] {
		t.Fatalf("tools/list = %v, want post_message and read_channel", seen)
	}

	// An argument outside the published schema is refused, not ignored: a
	// misspelt audience must never become a channel-wide post.
	before := f.messageCount(t)
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	for _, args := range []map[string]any{
		{"channel": "ops", "body": "SECRET", "audiance": mcpNet(f.net1.ID)},
		{"channel": "ops", "body": "SECRET", "net_id": f.net1.ID},
		{"channel": "ops", "body": "SECRET", "audience": map[string]any{"kind": "net", "net": f.net1.ID}},
		{"channel": "ops", "body": "SECRET", "audience": "net1"},
		{"channel": "ops", "body": "SECRET", "audience": []any{mcpNet(f.net1.ID)}},
	} {
		if refused, body := f.mcpRaw(t, "aria", "post_message", args); !refused {
			t.Errorf("post_message accepted %v: %s", args, body)
		}
	}
	if n := f.messageCount(t) - before; n != 0 {
		t.Errorf("malformed audience arguments stored %d messages, want 0", n)
	}

	// An explicit null is the one spelling of "no audience" besides leaving
	// the argument out, as on REST: a channel-wide post, which needs the
	// channel-wide grant.
	out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "open", "audience": nil})
	if out.result() != "ok" || strings.Contains(out.body, `"audience"`) {
		t.Errorf("audience null = %s (%s), want a channel-wide post", out.result(), out.body)
	}
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPostNet)
	out = f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "open", "audience": nil})
	if out.result() != "forbidden" {
		t.Errorf("audience null with only post_net = %s, want forbidden: it is a channel-wide post", out.result())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The visibility reader of an MCP read is the authenticated agent and nothing
// else. The structural guard accepts g.scope.reader() as a reader, so reader()
// is pinned here.
func TestAgentScopeReaderIsTheAuthenticatedAgent(t *testing.T) {
	f := newScopedFixture(t)
	for _, name := range []string{"aria", "arlo"} {
		id := f.p(name).ID
		scope, serr := f.srv.newAgentScope(context.Background(), mcpIdentity{principalID: id}, "read_channel")
		if serr != nil {
			t.Fatalf("newAgentScope(%s): %+v", name, serr)
		}
		if got, want := scope.reader(), (store.Reader{PrincipalID: id, Scoped: true}); got != want {
			t.Errorf("%s reader = %+v, want %+v", name, got, want)
		}
	}
	// No tool argument names a reader: read_channel as aria with another
	// principal's id in any plausible argument is refused, and aria's own
	// read never returns arlo's whisper.
	f.mustPost(t, "ann", `{"body":"SECRET-for-arlo",`+whisperAudience(f.p("arlo").ID)+`}`)
	for _, extra := range []string{"principal_id", "reader", "as", "author_id"} {
		refused, body := f.mcpRaw(t, "aria", "read_channel", map[string]any{"channel": "ops", extra: f.p("arlo").ID})
		if !refused || strings.Contains(body, "SECRET") {
			t.Errorf("read_channel with %s was not refused, or leaked a whisper to another agent: %s", extra, body)
		}
	}
	if got, _ := f.mcpRead(t, "aria", 0, 100); len(got) != 0 {
		t.Errorf("aria read %+v, want nothing: the only message is a whisper to arlo", got)
	}
}
