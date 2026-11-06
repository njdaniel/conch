package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The admin door over MCP (issue #1331, #1326 slice S4; ai-administrators
// §3.6): describe_admin_actions, submit_admin_action and run_approved_action
// are fronts on the functions the REST routes call (admin_handlers.go). The
// tests here hold the two fronts to the same answers, pin the capability gate
// and the single spend of the budget, drive the S3 chain over MCP, and watch
// an admin approval with await_decision until the grant goes.

// adminManifest is doorFixture.grant's manifest with more: extra
// capabilities and rate limits.
func adminManifest(d *doorFixture, caps []schema.Capability, limits []schema.RateLimit, perms ...schema.Permission) schema.PutAgentManifestRequestV1 {
	req := schema.PutAgentManifestRequestV1{
		DisplayName:  "helper",
		Tier:         schema.AgentTierA,
		Capabilities: append([]schema.Capability{schema.CapabilityAdminAct, schema.CapabilityMessagesRead}, caps...),
		Channels: []schema.ChannelGrant{{ChannelID: d.approvalCh.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		}}},
		RateLimits:  limits,
		Permissions: perms,
	}
	if len(perms) > 0 {
		req.Admin = &schema.AgentAdminV1{ApprovalChannelID: d.approvalCh.ID}
	}
	return req
}

func (d *doorFixture) putManifest(t *testing.T, req schema.PutAgentManifestRequestV1) {
	t.Helper()
	if _, _, err := d.srv.store.PutAgentManifest(context.Background(), "system", d.agent.ID, req); err != nil {
		t.Fatal(err)
	}
}

// joinApprovalChannel seats the agent in its approval channel, as the setup
// command does (design §3.5.6: it reads and posts there, and that is all):
// the manifest's channel grant is one gate, membership the other.
func (d *doorFixture) joinApprovalChannel(t *testing.T) {
	t.Helper()
	if _, err := d.srv.store.AddChannelMember(context.Background(), "system", d.approvalCh.ID, d.agent.ID, 0); err != nil {
		t.Fatal(err)
	}
}

// doorFront is one way through the door: REST, or the MCP tools. Each
// function answers "ok" or the error code, and the document when ok.
type doorFront struct {
	name     string
	describe func(t *testing.T, d *doorFixture) (string, json.RawMessage)
	submit   func(t *testing.T, d *doorFixture, action schema.AdminAction, target *schema.AdminTargetV1, params, reason string) (string, json.RawMessage)
	run      func(t *testing.T, d *doorFixture, id int64) (string, json.RawMessage)
}

var restDoor = doorFront{
	name: "rest",
	describe: func(t *testing.T, d *doorFixture) (string, json.RawMessage) {
		rec := d.do(t, "GET", "/v1/admin/actions", d.agentTok, "")
		return restCode(t, rec), rec.Body.Bytes()
	},
	submit: func(t *testing.T, d *doorFixture, action schema.AdminAction, target *schema.AdminTargetV1, params, reason string) (string, json.RawMessage) {
		rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(action, target, params, reason))
		return restCode(t, rec), rec.Body.Bytes()
	},
	run: func(t *testing.T, d *doorFixture, id int64) (string, json.RawMessage) {
		rec := d.run(t, id, d.agentTok)
		return restCode(t, rec), rec.Body.Bytes()
	},
}

var mcpDoor = doorFront{
	name: "mcp",
	describe: func(t *testing.T, d *doorFixture) (string, json.RawMessage) {
		res := mcpTool(t, d.srv, d.agentTok, "describe_admin_actions", map[string]any{})
		return res.code, res.out
	},
	submit: func(t *testing.T, d *doorFixture, action schema.AdminAction, target *schema.AdminTargetV1, params, reason string) (string, json.RawMessage) {
		args := map[string]any{"action": string(action)}
		if target != nil {
			args["target"] = map[string]any{"kind": string(target.Kind), "id": target.ID}
		}
		if params != "" {
			var obj map[string]any
			if err := json.Unmarshal([]byte(params), &obj); err != nil {
				t.Fatal(err)
			}
			args["params"] = obj
		}
		if reason != "" {
			args["reason"] = reason
		}
		res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", args)
		return res.code, res.out
	},
	run: func(t *testing.T, d *doorFixture, id int64) (string, json.RawMessage) {
		res := mcpTool(t, d.srv, d.agentTok, "run_approved_action", map[string]any{"approval_id": id})
		return res.code, res.out
	},
}

// reduceResult folds an AdminActionResultV1 into one comparable line: the
// status, and for a ran action the route's status and error code. Any other
// document (the catalog) reduces to the code alone.
func reduceResult(t *testing.T, code string, doc json.RawMessage) string {
	t.Helper()
	if code != "ok" {
		return code
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	if probe.Status == "" {
		return code
	}
	var res schema.AdminActionResultV1
	if err := json.Unmarshal(doc, &res); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result %s is not valid: %v", doc, err)
	}
	if res.Status != schema.AdminActionRan {
		return "ok " + string(res.Status)
	}
	line := fmt.Sprintf("ok ran %d", res.HTTPStatus)
	if res.HTTPStatus >= 400 {
		var e schema.Error
		_ = json.Unmarshal(res.Result, &e)
		line += " " + e.Code
	}
	return line
}

func approvalIDOf(t *testing.T, doc json.RawMessage) int64 {
	t.Helper()
	var res schema.AdminActionResultV1
	if err := json.Unmarshal(doc, &res); err != nil || res.Status != schema.AdminActionPendingApproval || res.ApprovalID == 0 {
		t.Fatalf("submit answered %s, want pending_approval with an approval id", doc)
	}
	return res.ApprovalID
}

// TestMCPAdminToolsParity: each tool answers what its REST route answers, on
// the same fixture state, with the same result status and error codes, for
// what the door allows and for every kind of refusal of its own and of the
// dispatched route.
func TestMCPAdminToolsParity(t *testing.T) {
	all := func() []schema.Permission {
		var out []schema.Permission
		for _, p := range schema.Permissions {
			if p.AgentGrantable() {
				out = append(out, p)
			}
		}
		return out
	}
	alice := func(d *doorFixture) *schema.AdminTargetV1 { return principalTarget(d.alice.ID) }
	gated := func(t *testing.T, d *doorFixture, f doorFront) int64 {
		t.Helper()
		_, doc := f.submit(t, d, schema.AdminActionDisableMember, alice(d), "", "alice asked")
		return approvalIDOf(t, doc)
	}
	cases := []struct {
		name  string
		perms []schema.Permission
		do    func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage)
	}{
		{"describe: the catalog", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.describe(t, d)
		}},
		{"describe: no admin grant", nil, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.describe(t, d)
		}},
		{"submit: a direct action runs", all(), func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionListMembers, nil, `{"limit":5}`, "")
		}},
		{"submit: a gated action pends", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionDisableMember, alice(d), "", "alice asked")
		}},
		{"submit: the same action again while it pends", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			gated(t, d, f)
			return f.submit(t, d, schema.AdminActionDisableMember, alice(d), "", "again")
		}},
		{"submit: an unknown action", all(), func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminAction("launch_rockets"), nil, "", "")
		}},
		{"submit: a permission not held", []schema.Permission{schema.PermManageChannels}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionDisableMember, alice(d), "", "")
		}},
		{"submit: the agent itself", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionDisableMember, principalTarget(d.agent.ID), "", "")
		}},
		{"submit: a target that does not exist", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionDisableMember, principalTarget(999), "", "")
		}},
		{"submit: a target the action does not take", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionListMembers, alice(d), "", "")
		}},
		{"submit: the owner as target", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			_, doc := f.submit(t, d, schema.AdminActionRenameMember, principalTarget(d.root.ID), `{"name":"boss"}`, "")
			// A direct action: the dispatched route's refusal is the answer.
			return "ok", doc
		}},
		{"submit: no admin grant", nil, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.submit(t, d, schema.AdminActionListMembers, nil, "", "")
		}},
		{"run: still pending", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.run(t, d, gated(t, d, f))
		}},
		{"run: an approval that does not exist", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.run(t, d, 999)
		}},
		{"run: approved", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			id := gated(t, d, f)
			d.decide(t, id, "run")
			code, doc := f.run(t, d, id)
			if !d.aliceDisabled(t) {
				t.Errorf("%s: alice was not disabled by the approved run", f.name)
			}
			return code, doc
		}},
		{"run: a second time", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			id := gated(t, d, f)
			d.decide(t, id, "run")
			f.run(t, d, id)
			return f.run(t, d, id)
		}},
		{"run: rejected", []schema.Permission{schema.PermManageMembers}, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			id := gated(t, d, f)
			d.decide(t, id, "refuse")
			code, doc := f.run(t, d, id)
			if d.aliceDisabled(t) {
				t.Errorf("%s: alice was disabled after a refusal", f.name)
			}
			return code, doc
		}},
		{"run: no admin grant", nil, func(t *testing.T, d *doorFixture, f doorFront) (string, json.RawMessage) {
			return f.run(t, d, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := map[string]string{}
			docs := map[string]json.RawMessage{}
			for _, f := range []doorFront{restDoor, mcpDoor} {
				d := newDoorFixture(t, tc.perms...)
				code, doc := tc.do(t, d, f)
				answers[f.name] = reduceResult(t, code, doc)
				docs[f.name] = doc
			}
			if answers["rest"] != answers["mcp"] {
				t.Fatalf("REST answered %q, MCP %q\nREST %s\nMCP %s", answers["rest"], answers["mcp"], docs["rest"], docs["mcp"])
			}
			if strings.HasPrefix(tc.name, "describe") && answers["rest"] == "ok" {
				var rest, mcp schema.AdminCatalogResponseV1
				if err := json.Unmarshal(docs["rest"], &rest); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(docs["mcp"], &mcp); err != nil {
					t.Fatal(err)
				}
				if err := mcp.Validate(); err != nil {
					t.Errorf("the MCP catalog is not valid: %v", err)
				}
				if !reflect.DeepEqual(rest, mcp) {
					t.Errorf("catalogs differ:\nREST %+v\nMCP  %+v", rest, mcp)
				}
			}
		})
	}
}

// TestMCPAdminToolsCapabilityGate: an agent whose manifest grants everything
// but admin.act is refused forbidden by each tool, with one audit row,
// access_denied capability_not_granted under mcp:<tool>, and nothing is
// dispatched, raised or spent.
func TestMCPAdminToolsCapabilityGate(t *testing.T) {
	tools := map[string]map[string]any{
		"describe_admin_actions": {},
		"submit_admin_action":    {"action": "disable_member", "target": map[string]any{"kind": "principal", "id": 0}},
		"run_approved_action":    {"approval_id": 1},
	}
	for name, args := range tools {
		t.Run(name, func(t *testing.T) {
			d := newDoorFixture(t)
			var caps []schema.Capability
			for _, c := range schema.Capabilities() {
				if c != schema.CapabilityAdminAct {
					caps = append(caps, c)
				}
			}
			req := adminManifest(d, nil, nil, schema.PermManageMembers)
			req.Capabilities = caps
			d.putManifest(t, req)
			if a, ok := args["target"].(map[string]any); ok {
				a["id"] = d.alice.ID
			}
			before := len(d.audit(t))
			if res := mcpTool(t, d.srv, d.agentTok, name, args); res.code != "forbidden" {
				t.Fatalf("%s without admin.act = %s, want forbidden", name, res.code)
			}
			rows := d.audit(t)
			if len(rows) != before+1 {
				t.Fatalf("the refused call wrote %d audit rows, want only the denial", len(rows)-before)
			}
			last := rows[len(rows)-1]
			if last.Action != "access_denied" || last.Subject != "mcp:"+name || last.Actor != fmt.Sprintf("principal:%d", d.agent.ID) ||
				!strings.Contains(last.Detail, "capability="+string(schema.CapabilityAdminAct)) || !strings.Contains(last.Detail, "reason="+denyCapability) {
				t.Errorf("denial audited as %+v", last)
			}
			var n int
			if err := d.db.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&n); err != nil || n != 0 {
				t.Errorf("approvals after the refusal = %d, %v", n, err)
			}
			if d.aliceDisabled(t) {
				t.Error("alice was disabled by a refused call")
			}
		})
	}
}

// TestMCPAdminToolsSpendOnce: a manifest limit of N on admin.act allows
// exactly N submitted actions, on either front, out of one budget: the tool
// body spends nothing of its own, and the core spends once per submit. A
// call the door refuses before dispatch spends nothing. The catalog read and
// the run of an approved action spend nothing on either front: the budget
// is a submitted action's, and the run is the second half of one.
func TestMCPAdminToolsSpendOnce(t *testing.T) {
	list := map[string]any{"action": "list_members", "params": map[string]any{"limit": 5}}
	t.Run("N calls, then rate_limited on both fronts", func(t *testing.T) {
		d := newDoorFixture(t, schema.PermManageMembers)
		d.putManifest(t, adminManifest(d, nil, []schema.RateLimit{{Capability: schema.CapabilityAdminAct, Max: 3, WindowSeconds: 3600}}, schema.PermManageMembers))
		for i := 1; i <= 3; i++ {
			if res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", list); res.code != "ok" {
				t.Fatalf("call %d = %s, want ok", i, res.code)
			}
		}
		if res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", list); res.code != schema.ErrorCodeRateLimited {
			t.Fatalf("call 4 over MCP = %s, want %s", res.code, schema.ErrorCodeRateLimited)
		}
		if rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionListMembers, nil, `{"limit":5}`, "")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("call 4 over REST = %d %s, want 429: the fronts share one budget", rec.Code, rec.Body)
		}
		// The catalog read answers alike on both fronts once the budget is
		// spent, whatever the core decides for it.
		rest := restCode(t, d.do(t, "GET", "/v1/admin/actions", d.agentTok, ""))
		if res := mcpTool(t, d.srv, d.agentTok, "describe_admin_actions", map[string]any{}); res.code != rest {
			t.Errorf("describe after the budget: MCP %s, REST %s", res.code, rest)
		}
	})
	t.Run("a refused call spends nothing", func(t *testing.T) {
		d := newDoorFixture(t, schema.PermManageMembers)
		d.putManifest(t, adminManifest(d, nil, []schema.RateLimit{{Capability: schema.CapabilityAdminAct, Max: 1, WindowSeconds: 3600}}, schema.PermManageMembers))
		refused := map[string]map[string]any{
			"an unknown action":     {"action": "launch_rockets"},
			"a permission not held": {"action": "rename_channel", "target": map[string]any{"kind": "channel", "id": d.approvalCh.ID}, "params": map[string]any{"name": "mine"}},
			"the agent itself":      {"action": "disable_member", "target": map[string]any{"kind": "principal", "id": d.agent.ID}},
			"a missing target":      {"action": "disable_member", "target": map[string]any{"kind": "principal", "id": 999}},
		}
		for what, args := range refused {
			if res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", args); res.code == "ok" || res.code == schema.ErrorCodeRateLimited {
				t.Fatalf("%s = %s, want a refusal of the door's own", what, res.code)
			}
		}
		if res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", list); res.code != "ok" {
			t.Fatalf("after four refusals the one allowed call = %s, want ok", res.code)
		}
		if res := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", list); res.code != schema.ErrorCodeRateLimited {
			t.Fatalf("the second allowed call = %s, want %s", res.code, schema.ErrorCodeRateLimited)
		}
	})
}

// TestMCPAdminActionGatedFullChain is TestAdminActionGatedFullChain driven
// over MCP (rule 3; the S9 dogfood scenario in test form): submit over MCP,
// approval_created, admin_action_requested, notify_sent, the owner's decision
// over REST, approval_resolved, run_approved_action over MCP,
// principal_disabled, admin_action_ran, with via=mcp on the door's rows.
func TestMCPAdminActionGatedFullChain(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageMembers)
	submitted := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", map[string]any{
		"action": "disable_member", "target": map[string]any{"kind": "principal", "id": d.alice.ID}, "reason": "alice asked to leave",
	})
	if submitted.code != "ok" {
		t.Fatalf("submit = %s", submitted.code)
	}
	id := approvalIDOf(t, submitted.out)
	d.waitNtfy(t, 1, "approval request")

	a, err := d.srv.store.ApprovalByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.ChannelID != d.approvalCh.ID || a.RequesterID != d.agent.ID || a.Payload == nil || a.Payload.Schema != schema.AdminActionV1Name {
		t.Fatalf("approval = %+v, want the agent's in the approval channel with the door's payload", a)
	}
	var action schema.AdminActionV1
	if err := json.Unmarshal(a.Payload.Data, &action); err != nil || action.Validate() != nil {
		t.Fatalf("payload = %s (%v)", a.Payload.Data, err)
	}
	wantRoute := fmt.Sprintf("POST /v1/principals/%d/disable", d.alice.ID)
	if action.Action != schema.AdminActionDisableMember || action.Route != wantRoute || action.Target == nil || action.Target.Name != "alice" || action.AgentID != d.agent.ID {
		t.Errorf("payload = %+v, want disable_member of alice by the agent", action)
	}
	for _, want := range []string{"over MCP at", "Will run:   " + wantRoute, `The agent's stated reason: "alice asked to leave"`} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body does not say %q:\n%s", want, a.Body)
		}
	}
	if d.aliceDisabled(t) {
		t.Fatal("alice was disabled before the decision")
	}
	// The agent sees the approval it raised with check_decision, given the
	// capability, read in the approval channel and a seat there.
	d.putManifest(t, adminManifest(d, []schema.Capability{schema.CapabilityApprovalsCheck}, nil, schema.PermManageMembers))
	d.joinApprovalChannel(t)
	if res := mcpTool(t, d.srv, d.agentTok, "check_decision", map[string]any{"approval_id": id}); res.code != "ok" || !strings.Contains(string(res.out), `"pending"`) {
		t.Fatalf("check_decision = %s %s, want pending", res.code, res.out)
	}

	d.decide(t, id, "run")
	d.waitNtfy(t, 2, "approval resolution")
	ran := mcpTool(t, d.srv, d.agentTok, "run_approved_action", map[string]any{"approval_id": id})
	if ran.code != "ok" {
		t.Fatalf("run = %s", ran.code)
	}
	if got := reduceResult(t, ran.code, ran.out); got != "ok ran 200" {
		t.Fatalf("run result = %s (%s)", got, ran.out)
	}
	if !d.aliceDisabled(t) {
		t.Fatal("alice was not disabled by the approved run")
	}

	agentA, rootA := fmt.Sprintf("principal:%d", d.agent.ID), fmt.Sprintf("principal:%d", d.root.ID)
	subject := fmt.Sprintf("approval:%d", id)
	var chain []string
	for _, e := range d.audit(t) {
		switch {
		case e.Subject == subject && (e.Action == store.AuditApprovalCreated || e.Action == store.AuditAdminActionRequested ||
			e.Action == "notify_sent" || e.Action == store.AuditDecisionCast || e.Action == store.AuditApprovalResolved ||
			e.Action == store.AuditAdminActionRan || e.Action == store.AuditAdminActionRefused):
			chain = append(chain, e.Action+" "+e.Actor)
			if (e.Action == store.AuditAdminActionRequested || e.Action == store.AuditAdminActionRan) && !strings.Contains(e.Detail, "via=mcp") {
				t.Errorf("%s detail %q does not say via=mcp", e.Action, e.Detail)
			}
		case e.Action == "principal_disabled" && e.Subject == fmt.Sprintf("principal:%d", d.alice.ID):
			chain = append(chain, e.Action+" "+e.Actor)
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + agentA,
		store.AuditAdminActionRequested + " " + agentA,
		"notify_sent system",
		store.AuditDecisionCast + " " + rootA,
		store.AuditApprovalResolved + " system",
		"notify_sent system",
		"principal_disabled " + agentA,
		store.AuditAdminActionRan + " " + agentA,
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain:\n got %q\nwant %q", chain, want)
	}
}

// TestMCPAwaitDecisionWatchesAdminApproval: an agent with approvals.await
// and read in its approval channel watches the approval the door raised for
// it, and the wait ends with the decision, or when the grant goes (design
// §3.9): a person narrows the manifest, or revokes the agent, which is to
// disable it (§4.5).
func TestMCPAwaitDecisionWatchesAdminApproval(t *testing.T) {
	ctx := context.Background()
	caps := []schema.Capability{schema.CapabilityApprovalsAwait, schema.CapabilityApprovalsCheck}
	tests := []struct {
		name     string
		withdraw func(t *testing.T, d *doorFixture, id int64)
		want     string
	}{
		{"the owner decides", func(t *testing.T, d *doorFixture, id int64) { d.decide(t, id, "run") }, "ok"},
		{"the grant is narrowed", func(t *testing.T, d *doorFixture, _ int64) {
			req := adminManifest(d, nil, nil)
			req.Capabilities = []schema.Capability{schema.CapabilityMessagesRead}
			d.putManifest(t, req)
		}, "forbidden"},
		{"the agent is revoked", func(t *testing.T, d *doorFixture, _ int64) {
			if rec := d.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", d.agent.ID), d.rootTok, ""); rec.Code != http.StatusOK {
				t.Fatalf("disable = %d %s", rec.Code, rec.Body)
			}
		}, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDoorFixture(t, schema.PermManageMembers)
			d.putManifest(t, adminManifest(d, caps, nil, schema.PermManageMembers))
			d.joinApprovalChannel(t)
			submitted := mcpTool(t, d.srv, d.agentTok, "submit_admin_action", map[string]any{
				"action": "disable_member", "target": map[string]any{"kind": "principal", "id": d.alice.ID}, "reason": "alice asked",
			})
			if submitted.code != "ok" {
				t.Fatalf("submit = %s", submitted.code)
			}
			id := approvalIDOf(t, submitted.out)

			polled := make(chan struct{})
			var once sync.Once
			d.srv.awaitPolled = func() { once.Do(func() { close(polled) }) }
			type result struct {
				out mcpOutcome
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := mcpCallOutcome(d.srv, d.agentTok, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 30000})
				done <- result{out, err}
			}()
			select {
			case <-polled:
			case got := <-done:
				t.Fatalf("await ended before it polled: %s", got.out.result())
			case <-time.After(10 * time.Second):
				t.Fatal("await_decision never polled")
			}
			tt.withdraw(t, d, id)
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.out.result() != tt.want {
					t.Fatalf("await ended with %s, want %s (body %s)", got.out.result(), tt.want, got.out.body)
				}
				if tt.want == "ok" && !strings.Contains(got.out.body, `"resolved"`) {
					t.Errorf("the wait ended without the resolution: %s", got.out.body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("await_decision kept waiting after the grant was withdrawn")
			}
			// A withdrawn grant cannot run what was approved; a decided one
			// can, once.
			if tt.want != "ok" {
				return
			}
			if res := mcpTool(t, d.srv, d.agentTok, "run_approved_action", map[string]any{"approval_id": id}); res.code != "ok" {
				t.Fatalf("run after the decision = %s", res.code)
			}
			if p, _ := d.srv.store.PrincipalByID(ctx, d.alice.ID); p.DisabledAt == nil {
				t.Fatal("alice was not disabled")
			}
		})
	}
}

// TestMCPAdminToolsNeedAnIssuedCredential: a deprecated static --mcp-token
// identity has no credential, and a dispatched route is served under the
// caller's request credential (its recent-authentication gate reads it), so
// the door refuses it before anything is read, as the session tools do. The
// tools that need no dispatch still work for it.
func TestMCPAdminToolsNeedAnIssuedCredential(t *testing.T) {
	d := newDoorFixture(t, schema.PermManageMembers)
	d.joinApprovalChannel(t)
	d.srv.cfg.MCPBearerTokens = map[string]int64{"static-helper": d.agent.ID}
	for name, args := range map[string]map[string]any{
		"describe_admin_actions": {},
		"submit_admin_action":    {"action": "list_members"},
		"run_approved_action":    {"approval_id": 1},
	} {
		if res := mcpTool(t, d.srv, "static-helper", name, args); res.code != "forbidden" {
			t.Errorf("%s with a static token = %s, want forbidden", name, res.code)
		}
	}
	if res := mcpTool(t, d.srv, "static-helper", "read_channel", map[string]any{"channel": "ai-approvals"}); res.code != "ok" {
		t.Errorf("read_channel with a static token = %s", res.code)
	}
	if n := len(d.auditOf(t, store.AuditAdminActionRefused)) + len(d.auditOf(t, store.AuditAdminActionRan)); n != 0 {
		t.Errorf("the door wrote %d rows for a caller it refused", n)
	}
}
