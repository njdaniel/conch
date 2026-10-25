package server

import (
	"context"
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

// Issue #789 (CLAUDE.md rule 3): full request -> notify -> resolve -> audit
// chain when a channel manager adds members via PutChannelMember and
// AddChannelMemberByName. Each manager-added member receives can_decide = 0,
// sees the channel's approval as read-only, is refused on decision
// (403 decision_not_permitted, audited access_denied), and the approval
// resolves by the channel's existing approver with its full audit chain intact.
func TestManagerAddedMemberCannotDecideFullChain(t *testing.T) {
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

	// mgr manages general (without can_decide) and shares lobby with two
	// plain human members (byID and byName).
	mgr, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	byID, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "added-by-id")
	if err != nil {
		t.Fatal(err)
	}
	_, byIDTok, err := f.srv.store.CreateCredential(ctx, "system", byID.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	byName, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "added-by-name")
	if err != nil {
		t.Fatal(err)
	}
	_, byNameTok, err := f.srv.store.CreateCredential(ctx, "system", byName.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	decideFalse := false
	managesTrue := true
	if _, err := f.srv.store.PutChannelMember(ctx, fmt.Sprintf("principal:%d", f.root.ID), general.ID, mgr.ID, f.root.ID, &decideFalse, &managesTrue); err != nil {
		t.Fatalf("grant mgr on general: %v", err)
	}
	for _, pid := range []int64{mgr.ID, byID.ID, byName.ID} {
		if _, err := f.srv.store.PutChannelMember(ctx, fmt.Sprintf("principal:%d", f.root.ID), lobby.ID, pid, f.root.ID, &decideFalse); err != nil {
			t.Fatalf("seed lobby member %d: %v", pid, err)
		}
	}

	// Manager adds byID via PutChannelMember and byName via AddChannelMemberByName.
	mgrActor := fmt.Sprintf("principal:%d", mgr.ID)
	if added, err := f.srv.store.PutChannelMember(ctx, mgrActor, general.ID, byID.ID, mgr.ID, nil); err != nil || !added {
		t.Fatalf("manager PutChannelMember(byID) = %v, %v; want true, nil", added, err)
	}
	if cm, added, err := f.srv.store.AddChannelMemberByName(ctx, mgrActor, general.ID, mgr.ID, byName.Name); err != nil || !added || cm.CanDecide || cm.Manages {
		t.Fatalf("manager AddChannelMemberByName(byName) = %+v, %v, %v", cm, added, err)
	}

	// Request an approval in general.
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)

	// Notify: wait for request notification.
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

	// Both manager-added members see the approval marked read-only and are
	// refused when attempting to decide it, with access_denied audited.
	for _, tc := range []struct {
		name string
		p    store.Principal
		tok  string
	}{
		{"by-id member", byID, byIDTok},
		{"by-name member", byName, byNameTok},
	} {
		list := decodeBody[schema.ListApprovalsResponseV1](t, f.do(t, "GET", "/v1/approvals", tc.tok, ""))
		if len(list.Approvals) != 1 || list.Approvals[0].ID != id || len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
			t.Fatalf("%s list = %+v, want approval %d marked read-only", tc.name, list, id)
		}
		assertErrorBody(t, f.do(t, "POST", path, tc.tok, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
		if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
			t.Fatalf("after %s refusal approval = %+v, %v; want pending", tc.name, a, err)
		}
		refused := false
		actor := fmt.Sprintf("principal:%d", tc.p.ID)
		for _, e := range f.audit(t) {
			if e.Action == "access_denied" && e.Actor == actor && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
				refused = true
			}
			if e.Action == store.AuditDecisionCast && e.Actor == actor {
				t.Errorf("%s refused decision was recorded as cast", tc.name)
			}
		}
		if !refused {
			t.Errorf("%s refusal was not audited", tc.name)
		}
	}

	// Resolve: alice (who holds can_decide = 1) decides the approval.
	rec = f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}
	waitNtfy(2, "approval resolution")

	// Audit chain: created -> decision_cast by alice -> approval_resolved.
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
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}

// Issue #789 (CLAUDE.md rule 3): under a quorum of two, a channel manager's
// attempt to remove an approver who has cast a decision (or an approver who
// has yet to decide) is refused by the store (ErrManagerRefused), so a
// manager cannot drop a cast decision or strip a channel's approvers. The
// second approver's concurring decision resolves the approval with its
// notifications and audit chain intact.
func TestManagerCannotRemoveApproverOrAlterQuorumFullChain(t *testing.T) {
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
	carol, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "carol")
	if err != nil {
		t.Fatal(err)
	}
	_, carolTok, err := f.srv.store.CreateCredential(ctx, "system", carol.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", carol.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add carol = %d %s", rec.Code, rec.Body)
	}

	mgr, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	decideFalse := false
	managesTrue := true
	if _, err := f.srv.store.PutChannelMember(ctx, fmt.Sprintf("principal:%d", f.root.ID), general.ID, mgr.ID, f.root.ID, &decideFalse, &managesTrue); err != nil {
		t.Fatalf("grant mgr on general: %v", err)
	}

	// Request an approval with quorum 2.
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
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)

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

	// Alice casts the first decision; state remains pending under quorum 2.
	if code := f.do(t, "POST", path, f.aliceTok, decisionBody(0)).Code; code != http.StatusOK {
		t.Fatalf("alice's decision = %d", code)
	}

	// Manager attempts to remove alice (who has cast a decision) and carol
	// (who holds can_decide): both are refused with ErrManagerRefused.
	mgrActor := fmt.Sprintf("principal:%d", mgr.ID)
	for _, approver := range []store.Principal{f.alice, carol} {
		removed, err := f.srv.store.RemoveChannelMember(ctx, mgrActor, general.ID, approver.ID, mgr.ID)
		if removed || !errors.Is(err, store.ErrManagerRefused) {
			t.Fatalf("manager RemoveChannelMember(%s) = %v, %v; want false, ErrManagerRefused", approver.Name, removed, err)
		}
	}

	// Carol casts the second decision: because alice's membership and
	// can_decide could not be touched by the manager, quorum 2 is met and
	// the approval resolves.
	rec = f.do(t, "POST", path, carolTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("carol's decision = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.CastDecisionResponseV1](t, rec)
	if resp.State != schema.ApprovalStateResolved || resp.Resolution == nil || len(resp.Resolution.Decisions) != 2 {
		t.Fatalf("after carol = %+v, want resolved with 2 decisions", resp)
	}
	waitNtfy(2, "approval resolution")

	// Audit chain: created -> decision_cast by alice -> decision_cast by carol -> approval_resolved.
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
