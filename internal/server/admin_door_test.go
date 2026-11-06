package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// doorFixture is a server with an owner (root), alice, an approval channel
// where root decides, and an AI administrator (helper) granted perms there.
type doorFixture struct {
	*authFixture
	approvalCh store.Channel
	agent      store.Principal
	agentTok   string
	db         *sql.DB
	mu         sync.Mutex
	notified   []string
}

func newDoorFixture(t *testing.T, perms ...schema.Permission) *doorFixture {
	t.Helper()
	ctx := context.Background()
	d := &doorFixture{}
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.notified = append(d.notified, r.URL.Path)
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ntfy.Close)
	d.authFixture = newAuthFixtureWithConfig(t, Config{
		AuthMode: AuthRequired,
		Ntfy: approvals.NtfyConfig{
			Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second,
		},
	})
	t.Cleanup(d.srv.approvals.Close)
	var err error
	if d.approvalCh, err = d.srv.store.CreateChannel(ctx, "ai-approvals"); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := d.srv.store.PutChannelMember(ctx, "system", d.approvalCh.ID, d.root.ID, 0, &yes); err != nil {
		t.Fatal(err)
	}
	if d.agent, err = d.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "helper"); err != nil {
		t.Fatal(err)
	}
	if _, d.agentTok, err = d.srv.store.CreateCredential(ctx, "system", d.agent.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	d.grant(t, d.agent.ID, perms...)
	path, ok := testDBPaths.Load(d.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	if d.db, err = sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.db.Close() })
	return d
}

// grant writes agentID's manifest: admin.act, read and post in the approval
// channel, and perms with the approval channel as admin's.
func (d *doorFixture) grant(t *testing.T, agentID int64, perms ...schema.Permission) {
	t.Helper()
	req := schema.PutAgentManifestRequestV1{
		DisplayName:  "helper",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityAdminAct, schema.CapabilityMessagesRead},
		Channels: []schema.ChannelGrant{{ChannelID: d.approvalCh.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		}}},
		Permissions: perms,
	}
	if len(perms) > 0 {
		req.Admin = &schema.AgentAdminV1{ApprovalChannelID: d.approvalCh.ID}
	}
	if _, _, err := d.srv.store.PutAgentManifest(context.Background(), "system", agentID, req); err != nil {
		t.Fatal(err)
	}
}

func (d *doorFixture) waitNtfy(t *testing.T, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.notified)
		d.mu.Unlock()
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never notified (got %d, want %d)", what, n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func submitBody(action schema.AdminAction, target *schema.AdminTargetV1, params, reason string) string {
	req := schema.SubmitAdminActionRequestV1{Action: action, Target: target, Reason: reason}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	b, _ := json.Marshal(req)
	return string(b)
}

func principalTarget(id int64) *schema.AdminTargetV1 {
	return &schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: id}
}

// submitGated submits a gated action as the agent and returns its approval.
func (d *doorFixture) submitGated(t *testing.T, body string) int64 {
	t.Helper()
	rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.AdminActionResultV1](t, rec)
	if err := res.Validate(); err != nil || res.Status != schema.AdminActionPendingApproval {
		t.Fatalf("submit result = %+v (%v), want a valid pending_approval", res, err)
	}
	return res.ApprovalID
}

func (d *doorFixture) decide(t *testing.T, id int64, option string) {
	t.Helper()
	rec := d.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), d.rootTok, fmt.Sprintf(`{"option_id":%q,"reason":"ok"}`, option))
	if rec.Code != http.StatusOK {
		t.Fatalf("root's decision %s = %d %s", option, rec.Code, rec.Body)
	}
}

func (d *doorFixture) run(t *testing.T, id int64, tok string) *httptest.ResponseRecorder {
	t.Helper()
	return d.do(t, "POST", fmt.Sprintf("/v1/admin/actions/%d/run", id), tok, "")
}

// TestAdminActionGatedFullChain is the rule 3 chain for a gated action
// (issue #1330; ai-administrators §3.5): the agent submits disable_member,
// an approval is raised in its approval channel and notified, the owner
// decides it, the agent runs it once, alice is disabled, and the trail says
// so in order: approval_created, admin_action_requested, notify_sent,
// decision_cast by the owner, approval_resolved, principal_disabled,
// admin_action_ran.
func TestAdminActionGatedFullChain(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageMembers)
	id := d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", "alice asked to leave"))
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
	for _, want := range []string{`AI admin helper: disable member "alice"`} {
		if !strings.Contains(a.Title, want) {
			t.Errorf("title %q does not say %q", a.Title, want)
		}
	}
	for _, want := range []string{"Will run:   " + wantRoute, `The agent's stated reason: "alice asked to leave"`, "Nothing else runs."} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body does not say %q:\n%s", want, a.Body)
		}
	}
	// Nothing ran yet.
	if p, _ := d.srv.store.PrincipalByID(ctx, d.alice.ID); p.DisabledAt != nil {
		t.Fatal("alice was disabled before the decision")
	}

	d.decide(t, id, "run")
	d.waitNtfy(t, 2, "approval resolution")
	rec := d.run(t, id, d.agentTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.AdminActionResultV1](t, rec)
	if res.Status != schema.AdminActionRan || res.HTTPStatus != http.StatusOK || res.Validate() != nil {
		t.Fatalf("run result = %+v", res)
	}
	if p, _ := d.srv.store.PrincipalByID(ctx, d.alice.ID); p.DisabledAt == nil {
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

func (d *doorFixture) aliceDisabled(t *testing.T) bool {
	t.Helper()
	p, err := d.srv.store.PrincipalByID(context.Background(), d.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p.DisabledAt != nil
}

func (d *doorFixture) refusedRow(t *testing.T, subject, reason string) bool {
	t.Helper()
	for _, e := range d.audit(t) {
		if e.Action == store.AuditAdminActionRefused && e.Subject == subject && strings.Contains(e.Detail, "reason="+reason) {
			return true
		}
	}
	return false
}

// TestAdminActionRunRefusals: the run enforces every check of §3.5.2, and a
// refused run disables nobody.
func TestAdminActionRunRefusals(t *testing.T) {
	disable := func(d *doorFixture) string {
		return submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", "")
	}
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, d *doorFixture, id int64) string // returns the token that runs
		status int
		code   string
		audit  bool // an admin_action_refused row names the code
	}{
		{"still pending", func(t *testing.T, d *doorFixture, id int64) string { return d.agentTok }, http.StatusConflict, schema.ErrorCodeApprovalNotApproved, true},
		{"rejected", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "refuse")
			return d.agentTok
		}, http.StatusConflict, schema.ErrorCodeApprovalNotApproved, true},
		{"expired", func(t *testing.T, d *doorFixture, id int64) string {
			if _, err := d.db.Exec("UPDATE approvals SET state = 'expired' WHERE id = ?", id); err != nil {
				t.Fatal(err)
			}
			if _, err := d.db.Exec("INSERT INTO approval_resolutions (approval_id, outcome, option_id, resolution_json, resolved_at) VALUES (?, 'expired', '', '{}', ?)", id, time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			return d.agentTok
		}, http.StatusConflict, schema.ErrorCodeApprovalNotApproved, true},
		{"after the run window", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "run")
			if _, err := d.db.Exec("UPDATE approval_resolutions SET resolved_at = ? WHERE approval_id = ?", time.Now().Add(-25*time.Hour).UnixMilli(), id); err != nil {
				t.Fatal(err)
			}
			return d.agentTok
		}, http.StatusConflict, schema.ErrorCodeApprovalRunWindowPassed, true},
		{"by another agent", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "run")
			other, err := d.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "other")
			if err != nil {
				t.Fatal(err)
			}
			_, tok, err := d.srv.store.CreateCredential(context.Background(), "system", other.ID, "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			d.grant(t, other.ID, schema.PermManageMembers)
			return tok
		}, http.StatusNotFound, "approval_not_found", false},
		{"by a person", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "run")
			return d.rootTok
		}, http.StatusNotFound, "approval_not_found", false},
		{"after the grant was revoked", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "run")
			d.grant(t, d.agent.ID)
			return d.agentTok
		}, http.StatusForbidden, "forbidden", false},
		{"after the target was renamed", func(t *testing.T, d *doorFixture, id int64) string {
			d.decide(t, id, "run")
			if rec := d.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/name", d.alice.ID), d.rootTok, `{"name":"alicia"}`); rec.Code != http.StatusOK {
				t.Fatalf("rename alice = %d %s", rec.Code, rec.Body)
			}
			return d.agentTok
		}, http.StatusConflict, schema.ErrorCodeTargetChanged, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDoorFixture(t, schema.PermManageMembers)
			id := d.submitGated(t, disable(d))
			tok := tc.before(t, d, id)
			assertErrorBody(t, d.run(t, id, tok), tc.status, tc.code)
			if d.aliceDisabled(t) {
				t.Error("a refused run disabled alice")
			}
			if tc.audit && !d.refusedRow(t, fmt.Sprintf("approval:%d", id), tc.code) {
				t.Errorf("no admin_action_refused reason=%s row", tc.code)
			}
		})
	}
}

// TestAdminActionRunsOnce: a second run of an approved action is refused, and
// a run whose route refuses (the permission went between decision and run)
// spends the approval without acting.
func TestAdminActionRunsOnce(t *testing.T) {
	d := newDoorFixture(t, schema.PermManageMembers)
	id := d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", ""))
	d.decide(t, id, "run")
	if rec := d.run(t, id, d.agentTok); rec.Code != http.StatusOK {
		t.Fatalf("first run = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, d.run(t, id, d.agentTok), http.StatusConflict, schema.ErrorCodeApprovalAlreadyRun)

	// The grant narrowed after the decision: the route refuses, and the
	// approval is spent.
	d2 := newDoorFixture(t, schema.PermManageMembers)
	id2 := d2.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d2.alice.ID), "", ""))
	d2.decide(t, id2, "run")
	d2.grant(t, d2.agent.ID, schema.PermManageChannels)
	rec := d2.run(t, id2, d2.agentTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.AdminActionResultV1](t, rec)
	if res.HTTPStatus != http.StatusForbidden || d2.aliceDisabled(t) {
		t.Fatalf("run without the permission = %+v, alice disabled %v; want the route's 403 and alice untouched", res, d2.aliceDisabled(t))
	}
	if !d2.refusedRow(t, fmt.Sprintf("approval:%d", id2), "") {
		t.Error("the refused dispatch was not audited admin_action_refused")
	}
	assertErrorBody(t, d2.run(t, id2, d2.agentTok), http.StatusConflict, schema.ErrorCodeApprovalAlreadyRun)
}

// TestAdminDoorRefusals: what the door refuses before anything is dispatched
// or raised.
func TestAdminDoorRefusals(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageMembers, schema.PermManageChannels, schema.PermMakeMemberLinks)
	countApprovals := func() int {
		var n int
		if err := d.db.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := countApprovals()

	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, `{"action":"make_coffee"}`), http.StatusBadRequest, schema.ErrorCodeAdminActionUnknown)
	// Not granted: export_audit needs read_audit_log.
	rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionExportAudit, nil, "", ""))
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodePermissionDenied)
	if e := decodeBody[schema.Error](t, rec); e.Permission != schema.PermReadAuditLog {
		t.Errorf("refusal names %q, want read_audit_log", e.Permission)
	}
	// Itself as target.
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionDisableMember, principalTarget(d.agent.ID), "", "")), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed)
	// Seating a decider in its approval channel, or touching it at all.
	chTarget := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: d.approvalCh.ID}
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionAddChannelMember, chTarget, fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID), "")), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed)
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionRenameChannel, chTarget, `{"name":"mine"}`, "")), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed)
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionCreateMemberLink, nil, fmt.Sprintf(`{"kind":"member","landing_channels":[%q]}`, d.approvalCh.Name), "")), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed)
	// add_channel_member never carries rights.
	general, err := d.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	gTarget := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: general.ID}
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionAddChannelMember, gTarget, fmt.Sprintf(`{"principal_id":%d,"can_decide":true}`, d.root.ID), "")), http.StatusBadRequest, "invalid_request")
	// create_member makes a person, never an agent.
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionCreateMember, nil, `{"name":"bot2","kind":"agent"}`, "")), http.StatusBadRequest, "invalid_request")
	if n := countApprovals(); n != start {
		t.Errorf("refusals raised %d approvals", n-start)
	}

	// The agent cannot decide.
	id := d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", ""))
	if rec := d.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), d.agentTok, `{"option_id":"run","reason":"self"}`); rec.Code != http.StatusForbidden {
		t.Errorf("the agent deciding its own approval = %d %s, want 403", rec.Code, rec.Body)
	}
	// The same action again names the pending one; others count to max_pending.
	rec = d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", "again"))
	assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeAdminActionPending)
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("approval %d", id)) {
		t.Errorf("admin_action_pending does not name approval %d: %s", id, rec.Body)
	}
	for i := 0; i < defaultAdminMaxPending-1; i++ {
		p, err := d.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, fmt.Sprintf("member%d", i))
		if err != nil {
			t.Fatal(err)
		}
		d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(p.ID), "", ""))
	}
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionHideMember, principalTarget(d.alice.ID), "", "")), http.StatusTooManyRequests, schema.ErrorCodeTooManyPendingAdminActions)
	if d.aliceDisabled(t) {
		t.Error("alice was disabled by a request nobody decided")
	}
}

// TestAdminPayloadIsTheDoors (F1 of #1328's review): no client authors the
// door's payload, and an approval the door did not raise never runs.
func TestAdminPayloadIsTheDoors(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageMembers)
	forged := schema.AdminActionV1{
		Action: schema.AdminActionDisableMember, Route: fmt.Sprintf("POST /v1/principals/%d/disable", d.alice.ID),
		Target: principalTarget(d.alice.ID), AgentID: d.agent.ID, RequestedAt: schema.NewTimestamp(time.Now()),
	}
	data, _ := json.Marshal(forged)
	var req schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(d.approvalCh.ID, 0)), &req); err != nil {
		t.Fatal(err)
	}
	req.Payload = &schema.Payload{Schema: schema.AdminActionV1Name, Data: data}
	raw, _ := json.Marshal(req)
	assertErrorBody(t, d.do(t, "POST", "/v1/approvals", d.rootTok, string(raw)), http.StatusBadRequest, "invalid_request")

	// Written underneath every route, approved: still not the door's.
	res, err := d.db.Exec(`INSERT INTO approvals (requester_id, channel_id, title, body, payload_schema, payload_json, options_json, deadline, grace_deadline, quorum, state, created_at)
		VALUES (?, ?, 't', 'b', ?, ?, '[]', ?, ?, 1, 'resolved', ?)`, d.agent.ID, d.approvalCh.ID, schema.AdminActionV1Name, string(data),
		time.Now().Add(time.Hour).UnixMilli(), time.Now().Add(2*time.Hour).UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := d.db.Exec("INSERT INTO approval_resolutions (approval_id, outcome, option_id, resolution_json, resolved_at) VALUES (?, 'approved', 'run', '{}', ?)", id, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, d.run(t, id, d.agentTok), http.StatusConflict, schema.ErrorCodeApprovalNotAdminAction)
	if d.aliceDisabled(t) {
		t.Error("a forged approval disabled alice")
	}
	if _, err := d.srv.store.CreateApproval(ctx, store.ApprovalParams{RequesterID: d.agent.ID, ChannelID: d.approvalCh.ID, Title: "t", Body: "b",
		Payload: &schema.Payload{Schema: schema.AdminActionV1Name, Data: data}, Options: []schema.Option{{ID: "run", Label: "Run", Kind: schema.OptionKindApprove}},
		Deadline: time.Now().Add(time.Hour), GraceDeadline: time.Now().Add(2 * time.Hour), Quorum: 1}); !errors.Is(err, store.ErrAdminPayloadReserved) {
		t.Errorf("store create with the door's payload and no request: err = %v, want ErrAdminPayloadReserved", err)
	}
}

// parityFixture is a door fixture with a non-owner operator (opal), a side
// channel, and the agent granted every agent-grantable permission.
type parityFixture struct {
	*doorFixture
	opal    store.Principal
	opalTok string
	side    store.Channel
	general store.Channel
	aliceCr int64
}

func newParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	ctx := context.Background()
	var grantable []schema.Permission
	for _, p := range schema.Permissions {
		if p.AgentGrantable() {
			grantable = append(grantable, p)
		}
	}
	p := &parityFixture{doorFixture: newDoorFixture(t, grantable...)}
	var err error
	if p.opal, err = p.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "opal"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", p.opal.ID); err != nil {
		t.Fatal(err)
	}
	if _, p.opalTok, err = p.srv.store.CreateCredential(ctx, "system", p.opal.ID, "operator", nil); err != nil {
		t.Fatal(err)
	}
	if p.side, err = p.srv.store.CreateChannel(ctx, "side"); err != nil {
		t.Fatal(err)
	}
	if p.general, err = p.srv.store.ChannelByName(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	creds, err := p.srv.store.ListCredentials(ctx, p.alice.ID)
	if err != nil || len(creds) == 0 {
		t.Fatalf("alice's credentials = %v, %v", creds, err)
	}
	p.aliceCr = creds[0].ID
	// Operators hold what the agent was granted: the two operator cells off
	// by default are turned on, so the comparison is like for like.
	if rec := p.do(t, "PUT", "/v1/permissions", p.rootTok, `{"revision":1,"changes":[{"permission":"change_server_settings","role":"operator","granted":true},{"permission":"read_audit_log","role":"operator","granted":true}]}`); rec.Code != http.StatusOK {
		t.Fatalf("grant operators = %d %s", rec.Code, rec.Body)
	}
	return p
}

// TestAdminCatalogParity: every catalog action, dispatched by the door for
// an agent granted every agent-grantable permission (approved and run, for a
// gated one), answers what its route answers a non-owner operator calling it
// directly: the same status and error code (ai-administrators §3.4).
func TestAdminCatalogParity(t *testing.T) {
	type sample struct {
		target func(p *parityFixture) *schema.AdminTargetV1
		params func(p *parityFixture) string
	}
	pr := func(p *parityFixture) *schema.AdminTargetV1 { return principalTarget(p.alice.ID) }
	ch := func(get func(p *parityFixture) store.Channel) func(p *parityFixture) *schema.AdminTargetV1 {
		return func(p *parityFixture) *schema.AdminTargetV1 {
			return &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: get(p).ID}
		}
	}
	gen := ch(func(p *parityFixture) store.Channel { return p.general })
	side := ch(func(p *parityFixture) store.Channel { return p.side })
	missing := func(k schema.AdminTargetKind) func(p *parityFixture) *schema.AdminTargetV1 {
		return func(*parityFixture) *schema.AdminTargetV1 { return &schema.AdminTargetV1{Kind: k, ID: 999} }
	}
	fixed := func(s string) func(*parityFixture) string { return func(*parityFixture) string { return s } }
	aliceIs := func(f string) func(p *parityFixture) string {
		return func(p *parityFixture) string { return fmt.Sprintf(f, p.alice.ID) }
	}
	samples := map[schema.AdminAction]sample{
		schema.AdminActionListMembers:         {params: fixed(`{"limit":5}`)},
		schema.AdminActionCreateMember:        {params: fixed(`{"name":"newbie"}`)},
		schema.AdminActionRenameMember:        {target: pr, params: fixed(`{"name":"alice2"}`)},
		schema.AdminActionSetVoicePermission:  {target: pr, params: fixed(`{"voice_allowed":false}`)},
		schema.AdminActionSetInvitePermission: {target: pr, params: fixed(`{"may_invite":true}`)},
		schema.AdminActionDisableMember:       {target: pr},
		schema.AdminActionEnableMember:        {target: pr},
		schema.AdminActionHideMember:          {target: pr},
		schema.AdminActionListCredentials:     {target: pr},
		schema.AdminActionRevokeCredential: {target: func(p *parityFixture) *schema.AdminTargetV1 {
			return &schema.AdminTargetV1{Kind: schema.AdminTargetCredential, ID: p.aliceCr}
		}},
		schema.AdminActionRevokeAllCredentials:   {target: pr},
		schema.AdminActionListHooks:              {},
		schema.AdminActionRevokeHook:             {target: missing(schema.AdminTargetHook)},
		schema.AdminActionListChannels:           {},
		schema.AdminActionCreateChannel:          {params: fixed(`{"name":"newch"}`)},
		schema.AdminActionRenameChannel:          {target: side, params: fixed(`{"name":"side2"}`)},
		schema.AdminActionSetChannelDefault:      {target: side, params: fixed(`{"default":true}`)},
		schema.AdminActionSetChannelVisibility:   {target: side, params: fixed(`{"visibility":"public"}`)},
		schema.AdminActionArchiveChannel:         {target: side},
		schema.AdminActionRestoreChannel:         {target: side},
		schema.AdminActionListChannelMembers:     {target: gen},
		schema.AdminActionAddChannelMember:       {target: side, params: aliceIs(`{"principal_id":%d}`)},
		schema.AdminActionRemoveChannelMember:    {target: gen, params: aliceIs(`{"principal_id":%d}`)},
		schema.AdminActionSetChannelMemberRights: {target: gen, params: aliceIs(`{"principal_id":%d,"manages":true}`)},
		schema.AdminActionCreateMemberLink:       {params: fixed(`{"kind":"member"}`)},
		schema.AdminActionListMemberLinks:        {},
		schema.AdminActionCloseMemberLink:        {target: missing(schema.AdminTargetInvite)},
		schema.AdminActionListLinkJoiners:        {target: missing(schema.AdminTargetInvite)},
		schema.AdminActionDisableLinkJoiners:     {target: missing(schema.AdminTargetInvite)},
		schema.AdminActionDisableInvitees:        {target: pr, params: fixed(`{"dry_run":true}`)},
		schema.AdminActionPauseInvites:           {},
		schema.AdminActionResumeInvites:          {},
		schema.AdminActionGetJoinSettings:        {},
		schema.AdminActionSetJoinSettings:        {params: fixed(`{"joining_paused":true}`)},
		schema.AdminActionGetPermissions:         {},
		schema.AdminActionSetPermissions:         {params: fixed(`{"revision":2,"changes":[{"permission":"use_voice","role":"member","granted":false}]}`)},
		schema.AdminActionListReports:            {params: fixed(`{"limit":5}`)},
		schema.AdminActionGetReport:              {target: missing(schema.AdminTargetReport)},
		schema.AdminActionUpdateReport:           {target: missing(schema.AdminTargetReport), params: fixed(`{"state":"filed","issue":"https://example.com/1"}`)},
		schema.AdminActionExportAudit:            {params: fixed(`{"limit":5}`)},
		schema.AdminActionListChannelBans:        {target: gen},
		schema.AdminActionBanChannelMember:       {target: gen, params: aliceIs(`{"principal_id":%d}`)},
		schema.AdminActionUnbanChannelMember:     {target: gen, params: aliceIs(`{"principal_id":%d}`)},
	}
	answer := func(status int, body []byte) string {
		if status < 400 {
			return fmt.Sprint(status)
		}
		var e schema.Error
		_ = json.Unmarshal(body, &e)
		return fmt.Sprintf("%d %s", status, e.Code)
	}
	for _, entry := range adminCatalog {
		s, ok := samples[entry.action]
		if !ok {
			t.Errorf("%s has no parity sample", entry.action)
			continue
		}
		t.Run(string(entry.action), func(t *testing.T) {
			ctx := context.Background()
			// The operator, directly, on a fixture of its own.
			op := newParityFixture(t)
			var target *schema.AdminTargetV1
			if s.target != nil {
				target = s.target(op)
			}
			params := ""
			if s.params != nil {
				params = s.params(op)
			}
			var raw json.RawMessage
			if params != "" {
				raw = json.RawMessage(params)
			}
			b, refusal := op.srv.buildAdminRequest(store.WithAdminDispatch(ctx), adminCaller{p: op.opal}, entry, target, raw)
			if refusal != nil {
				// An unknown target: the operator sends the route itself.
				method, path := fillPath(entry.pattern, target, 0)
				body := params
				if body == "" && (method == http.MethodPost || method == http.MethodPut) {
					body = "{}"
				}
				b.req = adminRequest{method: method, path: path, body: []byte(body)}
			}
			path := b.req.path
			if b.req.query != "" {
				path += "?" + b.req.query
			}
			direct := op.do(t, b.req.method, path, op.opalTok, string(b.req.body))
			want := answer(direct.Code, direct.Body.Bytes())

			// The agent, through the door, on a fixture of its own.
			ag := newParityFixture(t)
			if s.target != nil {
				target = s.target(ag)
			}
			rec := ag.do(t, "POST", "/v1/admin/actions", ag.agentTok, submitBody(entry.action, target, params, "parity"))
			if rec.Code >= 400 {
				// The door refused before dispatch (an unknown target).
				if got := answer(rec.Code, rec.Body.Bytes()); got != want {
					t.Errorf("door refused %s (%s), the operator's direct request %s (%s)", got, rec.Body, want, direct.Body)
				}
				return
			}
			if entry.gate == schema.AdminGateApproval {
				if rec.Code != http.StatusCreated {
					t.Fatalf("gated submit = %d %s", rec.Code, rec.Body)
				}
				id := decodeBody[schema.AdminActionResultV1](t, rec).ApprovalID
				ag.decide(t, id, "run")
				rec = ag.run(t, id, ag.agentTok)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("door = %d %s", rec.Code, rec.Body)
			}
			res := decodeBody[schema.AdminActionResultV1](t, rec)
			if got := answer(res.HTTPStatus, res.Result); got != want {
				t.Errorf("door answered %s (%s), the operator's direct request %s (%s)", got, res.Result, want, direct.Body)
			}
		})
	}
}

// TestRequestApprovalRefusesTheDoorsPayload: request_approval over MCP
// refuses a payload of the door's schema (F1 of #1328's review), as the REST
// create route does (TestAdminPayloadIsTheDoors).
func TestRequestApprovalRefusesTheDoorsPayload(t *testing.T) {
	d := newDoorFixture(t)
	req := schema.PutAgentManifestRequestV1{
		DisplayName: "helper", Tier: schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityApprovalsRequest},
		Channels: []schema.ChannelGrant{{ChannelID: d.approvalCh.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		}}},
	}
	if _, _, err := d.srv.store.PutAgentManifest(context.Background(), "system", d.agent.ID, req); err != nil {
		t.Fatal(err)
	}
	args := approvalMCPArguments(d.approvalCh.ID, time.Now().Add(time.Hour))
	args["payload"] = map[string]any{"schema": schema.AdminActionV1Name, "data": map[string]any{
		"action": "disable_member", "route": fmt.Sprintf("POST /v1/principals/%d/disable", d.alice.ID),
		"agent_id": d.agent.ID, "requested_at": time.Now().UTC().Format(time.RFC3339Nano),
	}}
	if res := mcpTool(t, d.srv, d.agentTok, "request_approval", args); res.code != "invalid_request" {
		t.Fatalf("request_approval with the door's payload: code = %q (%s), want invalid_request", res.code, res.out)
	}
	var n int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&n); err != nil || n != 0 {
		t.Fatalf("approvals after the refusal = %d, %v", n, err)
	}
}

// TestAdminActionWithARenewableCredential (security review of #1343): the
// setup commands issue an AI administrator a renewable credential, which the
// store marks a session; the dispatched route's recent-auth gate still
// passes it, since an agent never signs in.
func TestAdminActionWithARenewableCredential(t *testing.T) {
	d := newDoorFixture(t, schema.PermManageMembers)
	_, tok, err := d.srv.store.CreateRenewableCredential(context.Background(), "system", d.agent.ID, "ai-admin", 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d.agentTok = tok
	id := d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", ""))
	d.decide(t, id, "run")
	rec := d.run(t, id, d.agentTok)
	res := decodeBody[schema.AdminActionResultV1](t, rec)
	if rec.Code != http.StatusOK || res.HTTPStatus != http.StatusOK || !d.aliceDisabled(t) {
		t.Fatalf("run with a renewable credential = %d %+v (%s), alice disabled %v; want the route's 200", rec.Code, res, res.Result, d.aliceDisabled(t))
	}
}

// TestRefusedRunsAreCoalesced (security review of #1343): a run refused over
// and over spends no budget, so its audit rows are thinned like access_denied.
func TestRefusedRunsAreCoalesced(t *testing.T) {
	d := newDoorFixture(t, schema.PermManageMembers)
	id := d.submitGated(t, submitBody(schema.AdminActionDisableMember, principalTarget(d.alice.ID), "", ""))
	for range 30 {
		assertErrorBody(t, d.run(t, id, d.agentTok), http.StatusConflict, schema.ErrorCodeApprovalNotApproved)
	}
	rows := 0
	for _, e := range d.audit(t) {
		if e.Action == store.AuditAdminActionRefused {
			rows++
		}
	}
	if rows == 0 || rows >= 30 {
		t.Errorf("admin_action_refused rows for 30 refused runs = %d; want some, thinned", rows)
	}
}
