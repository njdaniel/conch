package server

import (
	"context"
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

// Issue #450, the whole chain (CLAUDE.md rule 3): request, notify, resolve,
// audit, through the REST API, with two human members of the approval's
// channel. A member without the right to decide its approvals sees the
// approval, read-only, and is refused when they try, audited; a member with
// the right decides it.
func TestOnlyMembersWithTheRightDecideFullChain(t *testing.T) {
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
	f := newAuthFixtureWithConfig(t, Config{AuthMode: AuthRequired, Ntfy: approvals.NtfyConfig{
		Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}})
	defer f.srv.approvals.Close()

	// guest is a member of "general" without the right, as someone who
	// joined through an invite will be; alice holds it, as every member the
	// operator adds does.
	guest, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "guest")
	if err != nil {
		t.Fatal(err)
	}
	_, guestTok, err := f.srv.store.CreateCredential(ctx, "system", guest.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", guest.ID), f.rootTok, `{"can_decide":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add guest without the right = %d %s", rec.Code, rec.Body)
	}

	// Request.
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)

	// Notify.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notified)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval was never notified")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The guest sees it, marked read-only; alice sees it with nothing marked.
	list := decodeBody[schema.ListApprovalsResponseV1](t, f.do(t, "GET", "/v1/approvals", guestTok, ""))
	if len(list.Approvals) != 1 || list.Approvals[0].ID != id || len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
		t.Fatalf("guest's list = %+v, want the approval, read-only", list)
	}
	if list := decodeBody[schema.ListApprovalsResponseV1](t, f.do(t, "GET", "/v1/approvals", f.aliceTok, "")); len(list.ReadOnly) != 0 {
		t.Errorf("alice's list marks %v read-only, want none", list.ReadOnly)
	}

	// The guest is refused, audited, and the approval stays pending.
	assertErrorBody(t, f.do(t, "POST", path, guestTok, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the guest's refusal approval = %+v, %v; want pending", a, err)
	}
	refused := false
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", guest.ID) && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
			refused = true
		}
		if e.Action == store.AuditDecisionCast && e.Actor == fmt.Sprintf("principal:%d", guest.ID) {
			t.Error("the guest's refused decision was cast")
		}
	}
	if !refused {
		t.Error("the guest's refusal was not audited")
	}

	// Resolve: alice decides.
	rec = f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: created, decided by alice, resolved, in that order.
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

// The right is the operator's to grant and revoke, over REST, audited; a
// change that changes nothing writes nothing; the member list says who holds
// it, for people only.
func TestDecideRightIsGrantedAndRevokedByTheOperator(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, AuthRequired)
	put := func(tok, body string) int {
		t.Helper()
		return f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.alice.ID), tok, body).Code
	}
	count := func(action string) int {
		n := 0
		for _, e := range f.audit(t) {
			if e.Action == action {
				n++
			}
		}
		return n
	}
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	holds := func() bool {
		t.Helper()
		can, err := f.srv.store.CanDecideApprovals(ctx, general.ID, f.alice.ID)
		if err != nil {
			t.Fatal(err)
		}
		return can
	}
	if !holds() {
		t.Fatal("a member the operator added does not hold the right")
	}
	for _, tt := range []struct {
		name           string
		token, body    string
		code           int
		want           bool
		granted, revok int
	}{
		{"revoke", f.rootTok, `{"can_decide":false}`, http.StatusNoContent, false, 0, 1},
		{"revoke again changes nothing", f.rootTok, `{"can_decide":false}`, http.StatusNoContent, false, 0, 1},
		{"no body keeps it", f.rootTok, ``, http.StatusNoContent, false, 0, 1},
		{"a member cannot grant themselves", f.aliceTok, `{"can_decide":true}`, http.StatusForbidden, false, 0, 1},
		{"an unknown field is refused", f.rootTok, `{"can_decide":true,"role":"op"}`, http.StatusBadRequest, false, 0, 1},
		{"grant", f.rootTok, `{"can_decide":true}`, http.StatusNoContent, true, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code := put(tt.token, tt.body); code != tt.code {
				t.Fatalf("PUT = %d, want %d", code, tt.code)
			}
			if got := holds(); got != tt.want {
				t.Errorf("holds the right = %v, want %v", got, tt.want)
			}
			if g, r := count(store.AuditMemberDecideGranted), count(store.AuditMemberDecideRevoked); g != tt.granted || r != tt.revok {
				t.Errorf("granted/revoked audit rows = %d/%d, want %d/%d", g, r, tt.granted, tt.revok)
			}
		})
	}

	// The member list carries the right for people and not for agents.
	members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", f.aliceTok, ""))
	for _, m := range members.Members {
		switch {
		case m.Kind == schema.PrincipalAgent && m.CanDecide != nil:
			t.Errorf("agent %d carries can_decide", m.PrincipalID)
		case m.Kind == schema.PrincipalHuman && (m.CanDecide == nil || !*m.CanDecide):
			t.Errorf("person %d can_decide = %v, want true", m.PrincipalID, m.CanDecide)
		}
	}
}
