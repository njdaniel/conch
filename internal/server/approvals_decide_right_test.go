package server

import (
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
	guest, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "invitee")
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

	// The member list carries the right for people and not for agents, and
	// shows it to the operator for everyone but to a member only about
	// themselves.
	for _, viewer := range []struct {
		name  string
		token string
		self  int64
		all   bool
	}{{"operator", f.rootTok, f.root.ID, true}, {"member", f.aliceTok, f.alice.ID, false}} {
		members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", viewer.token, ""))
		for _, m := range members.Members {
			shown := viewer.all || m.PrincipalID == viewer.self
			switch {
			case m.Kind == schema.PrincipalAgent && m.CanDecide != nil:
				t.Errorf("%s sees agent %d carry can_decide", viewer.name, m.PrincipalID)
			case m.Kind == schema.PrincipalHuman && shown && (m.CanDecide == nil || !*m.CanDecide):
				t.Errorf("%s sees person %d can_decide = %v, want true", viewer.name, m.PrincipalID, m.CanDecide)
			case m.Kind == schema.PrincipalHuman && !shown && m.CanDecide != nil:
				t.Errorf("%s sees another member's (%d) can_decide", viewer.name, m.PrincipalID)
			}
		}
	}

	// Adding someone without the right says so in the audit trail.
	ghost, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if code := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", ghost.ID), f.rootTok, `{"can_decide":false}`).Code; code != http.StatusNoContent {
		t.Fatalf("add ghost without the right = %d", code)
	}
	found := false
	for _, e := range f.audit(t) {
		if e.Action == store.AuditMemberAdded && e.Detail == fmt.Sprintf("principal=%d can_decide=0", ghost.ID) {
			found = true
		}
	}
	if !found {
		t.Error("the add without the right is not marked can_decide=0 in the audit trail")
	}

	// A body that is not exactly one object is refused.
	for _, body := range []string{`null`, `{"can_decide":true}}`, `{"can_decide":true} {}`} {
		if code := put(f.rootTok, body); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, code)
		}
	}
}

// Issue #453, the whole chain (CLAUDE.md rule 3): request, notify, resolve,
// audit. Under a quorum of two, a decision cast by a member whose right is
// then revoked stays on the record but no longer counts: the operator's
// concurring decision leaves the approval pending, and it resolves only when
// a second member who holds the right concurs. Every decision is in the
// resolution and the audit chain.
func TestRevokedDecisionNoLongerCountsFullChain(t *testing.T) {
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

	// Request, with a quorum of two.
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

	// Notify: the request is announced once.
	notifications := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(notified)
	}
	waitNotified := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for notifications() < want {
			if time.Now().After(deadline) {
				t.Fatalf("%s was never notified", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitNotified(1, "the approval")

	// Alice decides, then loses the right.
	if code := f.do(t, "POST", path, f.aliceTok, decisionBody(0)).Code; code != http.StatusOK {
		t.Fatalf("alice's decision = %d", code)
	}
	if code := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.alice.ID), f.rootTok, `{"can_decide":false}`).Code; code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	// The revoke itself resolves nothing and reopens nothing.
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the revoke approval = %+v, %v; want pending", a, err)
	}

	// The operator concurs: one decision of a current holder, short of two.
	rec = f.do(t, "POST", path, f.rootTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator's decision = %d %s", rec.Code, rec.Body)
	}
	if state := decodeBody[schema.CastDecisionResponseV1](t, rec).State; state != schema.ApprovalStatePending {
		t.Fatalf("state after the operator = %s; alice's decision no longer counts, so want pending", state)
	}
	// Nothing resolved, so nothing more is announced.
	time.Sleep(200 * time.Millisecond)
	if n := notifications(); n != 1 {
		t.Fatalf("%d notifications while the approval is still pending, want only the request's", n)
	}

	// Resolve: carol, who holds the right, concurs.
	rec = f.do(t, "POST", path, carolTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("carol's decision = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.CastDecisionResponseV1](t, rec)
	if resp.State != schema.ApprovalStateResolved || resp.Resolution == nil {
		t.Fatalf("after carol = %+v, want resolved", resp)
	}
	// The record keeps every decision, alice's included.
	if n := len(resp.Resolution.Decisions); n != 3 {
		t.Errorf("resolution holds %d decisions, want all 3", n)
	}
	// Notify: the resolution is announced.
	waitNotified(2, "the resolution")

	// Audit: created, three decisions, resolved once, in order.
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
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.root.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", carol.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}

// A member who leaves the channel after deciding is counted out the same way.
func TestDecisionOfAMemberWhoLeftNoLongerCounts(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, AuthRequired)
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
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
	if code := f.do(t, "POST", path, f.aliceTok, decisionBody(0)).Code; code != http.StatusOK {
		t.Fatalf("alice's decision = %d", code)
	}
	if code := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.alice.ID), f.rootTok, "").Code; code != http.StatusNoContent {
		t.Fatalf("remove alice = %d", code)
	}
	rec = f.do(t, "POST", path, f.rootTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator's decision = %d %s", rec.Code, rec.Body)
	}
	if state := decodeBody[schema.CastDecisionResponseV1](t, rec).State; state != schema.ApprovalStatePending {
		t.Errorf("state = %s; the decision of a member who left no longer counts, so want pending", state)
	}
}

// Someone who joins through an invite (issue #436) is a member of the
// channels it names but never holds the right to decide their approvals:
// they see the approval read-only and a decision is refused.
func TestInviteJoinerCannotDecide(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, AuthRequired)
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := f.srv.store.CreateInvite(ctx, "system", f.root.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, store.InviteOptions{}, time.Now())
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	joined, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(inv.Code), "joiner", time.Now())
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	if member, err := f.srv.store.IsChannelMember(ctx, general.ID, joined.Principal.ID); err != nil || !member {
		t.Fatalf("the joiner is not a member of general: %v, %v", member, err)
	}
	if can, err := f.srv.store.CanDecideApprovals(ctx, general.ID, joined.Principal.ID); err != nil || can {
		t.Fatalf("the joiner can decide = %v, %v; want false", can, err)
	}

	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	list := decodeBody[schema.ListApprovalsResponseV1](t, f.do(t, "GET", "/v1/approvals", joined.Token, ""))
	if len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
		t.Errorf("the joiner's list marks %v read-only, want [%d]", list.ReadOnly, id)
	}
	assertErrorBody(t, f.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), joined.Token, decisionBody(0)),
		http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Errorf("after the joiner's refusal approval = %+v, %v; want pending", a, err)
	}
}

// A revoke that commits after the decision handler's own check, and before
// the deciding transaction, still stops the decision (issue #453): the store
// checks again, the answer is the same audited 403, and nothing is written.
func TestRevokeInTheGapStopsTheDecision(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, AuthRequired)
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	f.srv.beforeDecide = func() {
		if _, err := f.srv.store.SetChannelMemberCanDecide(ctx, "system", general.ID, f.alice.ID, false); err != nil {
			t.Error(err)
		}
	}
	assertErrorBody(t, f.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), f.aliceTok, decisionBody(0)),
		http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("approval = %+v, %v; want pending", a, err)
	}
	refused := false
	for _, e := range f.audit(t) {
		if e.Action == store.AuditDecisionCast {
			t.Error("a decision was written in the gap")
		}
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", f.alice.ID) && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
			refused = true
		}
	}
	if !refused {
		t.Error("the refusal was not audited")
	}
}
