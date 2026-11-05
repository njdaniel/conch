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

// Issue #1329 (CLAUDE.md rule 3; ai-administrators §3.2, §3.5.4): the full
// request -> notify -> resolve -> audit chain beside an AI administrator. An
// owner grants the agent manage_members through the manifest route (a second
// operator cannot); the agent's membership carries can_decide = 1 and
// manages = 1, written raw; it is refused as a decider and, lacking
// manage_channels, as a manager; a decision row of its own, written raw,
// does not count toward a quorum of two; the two human approvers resolve the
// approval and the chain shows only them.
func TestAIAdminDecisionNeverCountsFullChain(t *testing.T) {
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

	f := newAuthFixtureWithConfig(t, Config{
		AuthMode: AuthRequired,
		Ntfy: approvals.NtfyConfig{
			Server:         ntfy.URL,
			ApprovalsTopic: "approvals",
			UrgentTopic:    "urgent",
			Timeout:        time.Second,
		},
	})
	defer f.srv.approvals.Close()

	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	lobby, err := f.srv.store.CreateChannel(ctx, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "carol")
	if err != nil {
		t.Fatal(err)
	}
	_, carolTok, err := f.srv.store.CreateCredential(ctx, "system", carol.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", carol.ID), f.rootTok, `{"can_decide":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add carol = %d %s", rec.Code, rec.Body)
	}
	opal, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "opal")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "helper")
	if err != nil {
		t.Fatal(err)
	}
	_, agentTok, err := f.srv.store.CreateCredential(ctx, "system", agent.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Underneath the routes: opal becomes an operator without the owner
	// mark, and the agent's membership of general carries the deciding and
	// managing bits no route would give it.
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "UPDATE principals SET role = 'operator' WHERE id = ?", opal.ID); err != nil {
		t.Fatal(err)
	}
	_, opalTok, err := f.srv.store.CreateCredential(ctx, "system", opal.ID, "operator", nil)
	if err != nil {
		t.Fatal(err)
	}

	manifest := func(perms ...schema.Permission) string {
		req := schema.PutAgentManifestRequestV1{
			DisplayName:  "helper",
			Tier:         schema.AgentTierA,
			Capabilities: []schema.Capability{schema.CapabilityMessagesRead},
			Channels:     []schema.ChannelGrant{{ChannelID: general.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
			Permissions:  perms,
		}
		if len(perms) > 0 {
			req.Admin = &schema.AgentAdminV1{ApprovalChannelID: general.ID}
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	manifestPath := fmt.Sprintf("/v1/principals/%d/manifest", agent.ID)

	// Grant: an operator without the owner mark is refused, the owner is not.
	assertErrorBody(t, f.do(t, "PUT", manifestPath, opalTok, manifest(schema.PermManageMembers)), http.StatusForbidden, "forbidden")
	if rec := f.do(t, "PUT", manifestPath, f.rootTok, manifest(schema.PermManageMembers)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("owner's grant = %d %s", rec.Code, rec.Body)
	}
	granted := 0
	for _, e := range f.audit(t) {
		if e.Action == store.AuditAdminGrantChanged && e.Subject == fmt.Sprintf("principal:%d", agent.ID) {
			if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) {
				t.Errorf("admin_grant_changed by %s, want the owner", e.Actor)
			}
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("admin_grant_changed rows = %d, want 1 (the refused grant writes none)", granted)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", general.ID, agent.ID, 0); err != nil {
		t.Fatalf("seed the agent in general: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE channel_members SET can_decide = 1, manages = 1 WHERE channel_id = ? AND principal_id = ?", general.ID, agent.ID); err != nil {
		t.Fatal(err)
	}

	// Manager: lacking manage_channels, the agent is refused at the route
	// and, beneath it, by the store, even in general where its row says it
	// manages.
	for _, ch := range []string{"general", "lobby"} {
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/%s/members/%d", ch, carol.ID), agentTok, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("agent's member PUT in %s = %d %s, want 403", ch, rec.Code, rec.Body)
		}
	}
	for _, ch := range []store.Channel{general, lobby} {
		if _, err := f.srv.store.PutChannelMember(ctx, fmt.Sprintf("principal:%d", agent.ID), ch.ID, opal.ID, agent.ID, nil, nil); !errors.Is(err, store.ErrManagerRefused) {
			t.Errorf("agent adding opal to %s in the store: err = %v, want ErrManagerRefused", ch.Name, err)
		}
	}

	// Request an approval in general with quorum 2.
	var req schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(general.ID, 0)), &req); err != nil {
		t.Fatal(err)
	}
	req.Quorum = 2
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, string(raw))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	decide := fmt.Sprintf("/v1/approvals/%d/decisions", id)

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

	// The agent is refused as a decider at the route and in the store.
	if rec := f.do(t, "POST", decide, agentTok, decisionBody(0)); rec.Code != http.StatusForbidden {
		t.Fatalf("agent's decision = %d %s, want 403", rec.Code, rec.Body)
	}
	if _, _, err := f.srv.store.CastMemberDecision(ctx, id, agent.ID, "approve", "self"); !errors.Is(err, store.ErrNoDecideRight) {
		t.Fatalf("agent's decision in the store: err = %v, want ErrNoDecideRight", err)
	}
	// A decision row of the agent's, written raw, sits beside alice's.
	if _, err := db.ExecContext(ctx, "INSERT INTO approval_decisions (approval_id, principal_id, option_id, reason, created_at) VALUES (?, ?, 'approve', 'raw', ?)", id, agent.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if code := f.do(t, "POST", decide, f.aliceTok, decisionBody(0)).Code; code != http.StatusOK {
		t.Fatalf("alice's decision = %d", code)
	}
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after alice and the agent's raw row, approval = %+v, %v; want pending", a, err)
	}

	// Resolve: carol's concurring decision meets the quorum.
	rec = f.do(t, "POST", decide, carolTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("carol's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}
	waitNtfy(2, "approval resolution")

	// Audit: created -> alice -> carol -> resolved; the agent cast nothing.
	var chain []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", id) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", carol.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
