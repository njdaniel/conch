package server

import (
	"bufio"
	"context"
	"encoding/json"
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

// Issue #1014 (ADR-014 A3a, CLAUDE.md rule 3): the full request -> notify ->
// resolve -> audit chain when an operator without manage_channels manages
// the approval's channel. The permissions document takes manage_channels
// from operators (a seeded role_permissions row). operator-1 manages
// channel-b, where an approval is open, and tries to make member-2 an
// approver (can_decide), to make member-2 a manager (manages), to add an
// agent and to remove the approver. Each is refused as a member-manager is,
// audited with permission=manage_channels. The approval then resolves with
// the quorum it had, by its one approver, and the audit export carries the
// whole chain, refusals included.
func TestRestrictedOperatorManagerCannotChangeApproversFullChain(t *testing.T) {
	// The seeded row: manage_channels off for operators, through the store.
	restrictedManagerApprovalChain(t, func(t *testing.T, f *restrictedFixture) {
		f.withhold(t, schema.PermManageChannels)
	})
}

// Issue #1015 (A3b, design §7.7): the same chain, with manage_channels cut
// by the owner through the real PUT /v1/permissions, which the approval then
// outlives unchanged.
func TestPermissionsPutCannotChangeApproversFullChain(t *testing.T) {
	restrictedManagerApprovalChain(t, func(t *testing.T, f *restrictedFixture) {
		res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermManageChannels, Role: schema.PermissionRoleOperator, Granted: false},
		}})
		if res.code != http.StatusOK {
			t.Fatalf("the owner's PUT = %d %s", res.code, res.body)
		}
	})
}

// restrictedManagerApprovalChain runs the chain after cut has taken
// manage_channels from operators.
func restrictedManagerApprovalChain(t *testing.T, cut func(t *testing.T, f *restrictedFixture)) {
	ctx := context.Background()
	var mu sync.Mutex
	var notified []string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		notified = append(notified, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ntfy.Close()

	f := newRestrictedFixture(t, Config{
		AuthMode: AuthRequired,
		Ntfy: approvals.NtfyConfig{
			Server:         ntfy.URL,
			ApprovalsTopic: "approvals",
			UrgentTopic:    "urgent",
			Timeout:        time.Second,
		},
	})
	defer f.srv.approvals.Close()

	cut(t, f)
	if ok, err := f.srv.store.Permitted(ctx, f.op.ID, schema.PermManageChannels); err != nil || ok {
		t.Fatalf("operator-1 holds manage_channels = %v, %v; want not", ok, err)
	}

	// Request: member-1 asks for an approval in channel-b, which operator-1
	// manages and member-1 and the owner decide.
	rec := f.do(t, "POST", "/v1/approvals", f.memberTok, createApprovalBody(f.chanB.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval
	id := created.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)

	// Notify.
	waitNtfy := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			n := len(notified)
			mu.Unlock()
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never notified (got %d, want %d)", what, n, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitNtfy(1, "approval request")

	// operator-1's attempts, each refused and audited as a manager's are.
	member := func(p store.Principal) string { return fmt.Sprintf("/v1/channels/channel-b/members/%d", p.ID) }
	for _, try := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"make member-2 an approver", "PUT", member(f.member2), `{"can_decide":true}`, http.StatusForbidden, "forbidden"},
		{"make member-2 a manager", "PUT", member(f.member2), `{"manages":true}`, http.StatusForbidden, "forbidden"},
		{"add an agent", "PUT", member(f.agent), "", http.StatusNotFound, "principal_not_found"},
		{"remove the approver", "DELETE", member(f.member), "", http.StatusForbidden, "forbidden"},
	} {
		assertErrorBody(t, f.do(t, try.method, try.path, f.opTok, try.body), try.status, try.code)
		if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
			t.Fatalf("after %q approval = %+v, %v; want pending", try.name, a, err)
		}
	}
	for _, p := range []store.Principal{f.member2, f.agent} {
		if ok, err := f.srv.store.CanDecideApprovals(ctx, f.chanB.ID, p.ID); err != nil || ok {
			t.Fatalf("%s may decide channel-b = %v, %v; want not", p.Name, ok, err)
		}
	}
	if ok, err := f.srv.store.CanDecideApprovals(ctx, f.chanB.ID, f.member.ID); err != nil || !ok {
		t.Fatalf("member-1 may decide channel-b = %v, %v; want yes", ok, err)
	}

	// Resolve, with the quorum it had: member-1's one decision.
	rec = f.do(t, "POST", path, f.memberTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("member-1's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved || resp.Resolution == nil || len(resp.Resolution.Decisions) != created.Quorum {
		t.Fatalf("after member-1 = %+v, want resolved with %d decision(s)", resp, created.Quorum)
	}
	waitNtfy(2, "approval resolution")

	// Audit, through the export.
	rec = f.do(t, "GET", "/v1/audit", f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit export = %d %s", rec.Code, rec.Body)
	}
	var chain []string
	refusals := map[string]int{}
	op := fmt.Sprintf("principal:%d", f.op.ID)
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		var e schema.AuditEventV1
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		switch {
		case e.Subject == fmt.Sprintf("approval:%d", id) &&
			(e.Action == store.AuditApprovalCreated || e.Action == store.AuditDecisionCast || e.Action == store.AuditApprovalResolved):
			chain = append(chain, e.Action+" "+e.Actor)
		case e.Actor == op && strings.Contains(e.Detail, "permission=manage_channels"):
			refusals[e.Action]++
		case e.Actor == op && (e.Action == store.AuditMemberAdded || e.Action == "member_can_decide_changed" || e.Action == "member_manages_changed" || e.Action == "member_removed"):
			t.Errorf("operator-1's refused write left %s %s", e.Action, e.Detail)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.member.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.member.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
	// can_decide, manages and removing the approver: access_denied; the
	// agent: member_add_refused.
	if refusals["access_denied"] != 3 || refusals[store.AuditMemberAddRefused] != 1 {
		t.Errorf("refusals audited with permission=manage_channels = %v, want 3 access_denied and 1 member_add_refused", refusals)
	}
}

// TestUseCameraToggleKeepsAnOpenApprovalFullChain (webcam C3, #1240;
// CLAUDE.md rule 3): request → notify → resolve → audit, with the owner
// turning use_camera on for both columns and off again through
// PUT /v1/permissions while the approval is open. use_camera is no part of
// deciding: the approval stays pending, who may decide is unchanged, and it
// resolves with the quorum it had.
func TestUseCameraToggleKeepsAnOpenApprovalFullChain(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var notified []string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		notified = append(notified, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ntfy.Close()
	f := newRestrictedFixture(t, Config{
		AuthMode: AuthRequired,
		Ntfy:     approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second},
	})
	defer f.srv.approvals.Close()
	waitNtfy := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			n := len(notified)
			mu.Unlock()
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never notified (got %d, want %d)", what, n, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Request, and notify.
	rec := f.do(t, "POST", "/v1/approvals", f.memberTok, createApprovalBody(f.chanB.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval
	id := created.ID
	waitNtfy(1, "approval request")

	// use_camera on for both columns, then off again, while it is open.
	cell := func(role schema.PermissionRole, on bool) schema.PermissionChangeV1 {
		return schema.PermissionChangeV1{Permission: schema.PermUseCamera, Role: role, Granted: on}
	}
	for _, step := range []struct {
		name    string
		changes []schema.PermissionChangeV1
	}{
		{"on", []schema.PermissionChangeV1{cell(schema.PermissionRoleOperator, true), cell(schema.PermissionRoleMember, true)}},
		{"off", []schema.PermissionChangeV1{cell(schema.PermissionRoleMember, false), cell(schema.PermissionRoleOperator, false)}},
	} {
		res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: step.changes})
		if res.code != http.StatusOK {
			t.Fatalf("the owner's PUT (use_camera %s) = %d %s", step.name, res.code, res.body)
		}
		if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending || a.Quorum != created.Quorum {
			t.Fatalf("after use_camera %s approval = %+v, %v; want pending with quorum %d", step.name, a, err, created.Quorum)
		}
		if ok, err := f.srv.store.CanDecideApprovals(ctx, f.chanB.ID, f.member.ID); err != nil || !ok {
			t.Fatalf("after use_camera %s member-1 may decide channel-b = %v, %v; want yes", step.name, ok, err)
		}
		if ok, err := f.srv.store.CanDecideApprovals(ctx, f.chanB.ID, f.member2.ID); err != nil || ok {
			t.Fatalf("after use_camera %s member-2 may decide channel-b = %v, %v; want not", step.name, ok, err)
		}
	}

	// Resolve, with the quorum it had.
	rec = f.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), f.memberTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("member-1's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved || resp.Resolution == nil || len(resp.Resolution.Decisions) != created.Quorum {
		t.Fatalf("after member-1 = %+v, want resolved with %d decision(s)", resp, created.Quorum)
	}
	waitNtfy(2, "approval resolution")

	// Audit, through the export: the chain, and the two permission changes.
	rec = f.do(t, "GET", "/v1/audit", f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit export = %d %s", rec.Code, rec.Body)
	}
	var chain []string
	changes := 0
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		var e schema.AuditEventV1
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		switch {
		case e.Subject == fmt.Sprintf("approval:%d", id) &&
			(e.Action == store.AuditApprovalCreated || e.Action == store.AuditDecisionCast || e.Action == store.AuditApprovalResolved):
			chain = append(chain, e.Action+" "+e.Actor)
		case e.Action == store.AuditPermissionsChanged && strings.Contains(e.Detail, "use_camera"):
			changes++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.member.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.member.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
	if changes != 2 {
		t.Errorf("%d permissions_changed rows name use_camera, want 2 (on, then off)", changes)
	}
}
