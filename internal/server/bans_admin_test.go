package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The AI administrator's channel-ban actions (issue #851; #849 Q4):
// list_channel_bans is direct, ban_channel_member and unban_channel_member
// are gated, all under manage_channels and targeting the channel. This is
// approval-path work (the admin catalog), so the gated chain is tested whole:
// request, notify, resolve, run, audit (CLAUDE.md rule 3).

func generalTarget(t *testing.T, d *doorFixture) *schema.AdminTargetV1 {
	t.Helper()
	general, err := d.srv.store.ChannelByName(context.Background(), "general")
	if err != nil {
		t.Fatal(err)
	}
	return &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: general.ID}
}

func (d *doorFixture) isMember(t *testing.T, channelID, principalID int64) bool {
	t.Helper()
	ok, err := d.srv.store.IsChannelMember(context.Background(), channelID, principalID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestAdminBanChannelMemberFullChain: the agent submits ban_channel_member, an
// approval is raised in its approval channel and notified, the owner decides
// it, the agent runs it once, alice is banned and out of the channel, and the
// trail says so in order. Unbanning is the same chain and restores nothing.
func TestAdminBanChannelMemberFullChain(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageChannels)
	target := generalTarget(t, d)
	params := fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID)

	id := d.submitGated(t, submitBody(schema.AdminActionBanChannelMember, target, params, "alice is spamming"))
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
	wantRoute := fmt.Sprintf("PUT /v1/channels/general/bans/%d", d.alice.ID)
	if action.Action != schema.AdminActionBanChannelMember || action.Route != wantRoute || action.Target == nil || action.Target.Name != "general" || action.AgentID != d.agent.ID {
		t.Errorf("payload = %+v, want ban_channel_member in general by the agent (route %q)", action, wantRoute)
	}
	for _, want := range []string{"Will run:   " + wantRoute, "Nothing else runs."} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body does not say %q:\n%s", want, a.Body)
		}
	}
	// Nothing ran yet.
	if !d.isMember(t, action.Target.ID, d.alice.ID) {
		t.Fatal("alice was removed before the decision")
	}
	if banned, _ := d.srv.store.IsChannelBanned(ctx, action.Target.ID, d.alice.ID); banned {
		t.Fatal("alice was banned before the decision")
	}

	d.decide(t, id, "run")
	d.waitNtfy(t, 2, "approval resolution")
	rec := d.run(t, id, d.agentTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.AdminActionResultV1](t, rec)
	if res.Status != schema.AdminActionRan || res.HTTPStatus != http.StatusNoContent || res.Validate() != nil {
		t.Fatalf("run result = %+v", res)
	}
	if banned, _ := d.srv.store.IsChannelBanned(ctx, action.Target.ID, d.alice.ID); !banned || d.isMember(t, action.Target.ID, d.alice.ID) {
		t.Fatalf("after the run: banned = %v, member = %v", banned, d.isMember(t, action.Target.ID, d.alice.ID))
	}

	agentA, rootA := fmt.Sprintf("principal:%d", d.agent.ID), fmt.Sprintf("principal:%d", d.root.ID)
	subject := fmt.Sprintf("approval:%d", id)
	chain := func() string {
		var out []string
		for _, e := range d.audit(t) {
			switch {
			case e.Subject == subject && (e.Action == store.AuditApprovalCreated || e.Action == store.AuditAdminActionRequested ||
				e.Action == "notify_sent" || e.Action == store.AuditDecisionCast || e.Action == store.AuditApprovalResolved ||
				e.Action == store.AuditAdminActionRan || e.Action == store.AuditAdminActionRefused):
				out = append(out, e.Action+" "+e.Actor)
			case e.Subject == fmt.Sprintf("channel:%d", action.Target.ID) &&
				(e.Action == store.AuditChannelBanAdded || e.Action == store.AuditChannelBanRemoved || e.Action == store.AuditMemberRemoved):
				out = append(out, e.Action+" "+e.Actor+" "+e.Detail)
			}
		}
		return strings.Join(out, "|")
	}
	aliceDetail := fmt.Sprintf("principal=%d", d.alice.ID)
	want := strings.Join([]string{
		store.AuditApprovalCreated + " " + agentA,
		store.AuditAdminActionRequested + " " + agentA,
		"notify_sent system",
		store.AuditDecisionCast + " " + rootA,
		store.AuditApprovalResolved + " system",
		"notify_sent system",
		store.AuditChannelBanAdded + " " + agentA + " " + aliceDetail,
		store.AuditMemberRemoved + " " + agentA + " " + aliceDetail,
		store.AuditAdminActionRan + " " + agentA,
	}, "|")
	if got := chain(); got != want {
		t.Errorf("audit chain:\n got %q\nwant %q", got, want)
	}

	// A second run of the spent approval does nothing.
	if rec := d.run(t, id, d.agentTok); rec.Code != http.StatusConflict {
		t.Errorf("second run = %d, want 409", rec.Code)
	}

	// The unban: the same gate, the same chain, and nothing is restored.
	listed := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionListChannelBans, target, "", ""))
	if listed.Code != http.StatusOK {
		t.Fatalf("list_channel_bans = %d %s", listed.Code, listed.Body)
	}
	lres := decodeBody[schema.AdminActionResultV1](t, listed)
	var bans schema.ListChannelBansResponseV1
	if lres.Status != schema.AdminActionRan || json.Unmarshal(lres.Result, &bans) != nil || len(bans.Bans) != 1 ||
		bans.Bans[0].PrincipalID != d.alice.ID || bans.Bans[0].CreatedBy != d.agent.ID || bans.Bans[0].Validate() != nil {
		t.Fatalf("list_channel_bans = %+v (%s)", lres, lres.Result)
	}
	uid := d.submitGated(t, submitBody(schema.AdminActionUnbanChannelMember, target, params, ""))
	if banned, _ := d.srv.store.IsChannelBanned(ctx, action.Target.ID, d.alice.ID); !banned {
		t.Fatal("the ban was lifted before the decision")
	}
	d.decide(t, uid, "run")
	if rec := d.run(t, uid, d.agentTok); rec.Code != http.StatusOK {
		t.Fatalf("unban run = %d %s", rec.Code, rec.Body)
	}
	if banned, _ := d.srv.store.IsChannelBanned(ctx, action.Target.ID, d.alice.ID); banned || d.isMember(t, action.Target.ID, d.alice.ID) {
		t.Errorf("after the unban: banned = %v, member = %v; want neither", banned, d.isMember(t, action.Target.ID, d.alice.ID))
	}
	var removed int
	for _, e := range d.audit(t) {
		if e.Action == store.AuditChannelBanRemoved && e.Actor == agentA && e.Detail == aliceDetail {
			removed++
		}
	}
	if removed != 1 {
		t.Errorf("channel_ban_removed rows by the agent = %d, want 1", removed)
	}
}

// TestAdminBanRefusals: the door refuses what the route refuses and what the
// door itself bars; nothing is raised for a request the door refuses.
func TestAdminBanRefusals(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageChannels)
	target := generalTarget(t, d)
	var before int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&before); err != nil {
		t.Fatal(err)
	}
	chTarget := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: d.approvalCh.ID}
	for _, tc := range []struct {
		name   string
		action schema.AdminAction
		target *schema.AdminTargetV1
		params string
		status int
		code   string
	}{
		{"its approval channel", schema.AdminActionBanChannelMember, chTarget, fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed},
		{"its approval channel, unban", schema.AdminActionUnbanChannelMember, chTarget, fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed},
		{"itself", schema.AdminActionBanChannelMember, target, fmt.Sprintf(`{"principal_id":%d}`, d.agent.ID), http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed},
		{"no params", schema.AdminActionBanChannelMember, target, "", http.StatusBadRequest, "invalid_request"},
		{"rights smuggled in", schema.AdminActionBanChannelMember, target, fmt.Sprintf(`{"principal_id":%d,"can_decide":true}`, d.alice.ID), http.StatusBadRequest, "invalid_request"},
		{"a reason in the params", schema.AdminActionBanChannelMember, target, fmt.Sprintf(`{"principal_id":%d,"reason":"x"}`, d.alice.ID), http.StatusBadRequest, "invalid_request"},
		{"no target", schema.AdminActionBanChannelMember, nil, fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID), http.StatusBadRequest, ""},
		{"a principal as target", schema.AdminActionBanChannelMember, principalTarget(d.alice.ID), fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID), http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(tc.action, tc.target, tc.params, ""))
			if rec.Code != tc.status {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			if tc.code != "" {
				assertErrorBody(t, rec, tc.status, tc.code)
			}
		})
	}
	var after int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&after); err != nil || after != before {
		t.Errorf("refusals raised %d approvals (%v)", after-before, err)
	}

	// Without manage_channels the agent holds nothing: not even the list.
	d.grant(t, d.agent.ID, schema.PermManageMembers)
	for _, a := range []schema.AdminAction{schema.AdminActionListChannelBans, schema.AdminActionBanChannelMember, schema.AdminActionUnbanChannelMember} {
		params := fmt.Sprintf(`{"principal_id":%d}`, d.alice.ID)
		if a == schema.AdminActionListChannelBans {
			params = ""
		}
		rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(a, target, params, ""))
		assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodePermissionDenied)
		if e := decodeBody[schema.Error](t, rec); e.Permission != schema.PermManageChannels {
			t.Errorf("%s refusal names %q, want manage_channels", a, e.Permission)
		}
	}
	d.grant(t, d.agent.ID, schema.PermManageChannels)

	// The owner is out of an AI administrator's reach: approved and run, the
	// route answers owner protection, and nothing changes.
	id := d.submitGated(t, submitBody(schema.AdminActionBanChannelMember, target, fmt.Sprintf(`{"principal_id":%d}`, d.root.ID), ""))
	d.decide(t, id, "run")
	rec := d.run(t, id, d.agentTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	if res := decodeBody[schema.AdminActionResultV1](t, rec); res.HTTPStatus != http.StatusForbidden {
		t.Errorf("banning the owner answered %d, want 403", res.HTTPStatus)
	}
	if banned, _ := d.srv.store.IsChannelBanned(ctx, target.ID, d.root.ID); banned || !d.isMember(t, target.ID, d.root.ID) {
		t.Errorf("the owner: banned = %v, member = %v", banned, d.isMember(t, target.ID, d.root.ID))
	}
}

// TestAdminBanCatalogGates: the gates the design names for the ban actions.
func TestAdminBanCatalogGates(t *testing.T) {
	for action, gate := range map[schema.AdminAction]schema.AdminGate{
		schema.AdminActionListChannelBans:    schema.AdminGateDirect,
		schema.AdminActionBanChannelMember:   schema.AdminGateApproval,
		schema.AdminActionUnbanChannelMember: schema.AdminGateApproval,
	} {
		e, ok := adminCatalogByAction[action]
		if !ok {
			t.Fatalf("%s is not in the catalog", action)
		}
		if e.gate != gate || e.permission != schema.PermManageChannels || e.target != schema.AdminTargetChannel {
			t.Errorf("%s = gate %s, permission %s, target %s; want %s, manage_channels, channel", action, e.gate, e.permission, e.target, gate)
		}
		if (action != schema.AdminActionListChannelBans) != e.changesTarget {
			t.Errorf("%s changesTarget = %v", action, e.changesTarget)
		}
	}
}
