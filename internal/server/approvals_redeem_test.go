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

// The approval chain (CLAUDE.md rule 3) with members made by redemption
// (issue #986): one through a member link and one through a legacy invite.
// Request, notify, resolve, audit, through the REST API. Each joiner lands in
// the default channel red with its whole history, sees the approval there
// read-only, and is refused when it tries, audited; the existing approver
// decides, the quorum is unchanged, and the approval resolves with its audit
// rows.
func TestRedeemedMembersCannotDecideFullChain(t *testing.T) {
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
	f := newInviteFixture(t, Config{Ntfy: approvals.NtfyConfig{
		Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}})
	defer f.srv.approvals.Close()

	// red is the default channel, where alice, added by an operator, holds
	// the right to decide. A message is there before anyone joins.
	if rec := f.send(t, "PUT", fmt.Sprintf("/v1/channels/%d/settings", f.red.ID), f.rootTok, `{"visibility":"public","default":true}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("make red a default channel = %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(t, "POST", "/v2/channels/red/messages", f.aliceTok, `{"body":"before anyone joined"}`, ""); rec.Code != http.StatusCreated {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}

	// One joiner through an operator's member link, one through a legacy
	// invite to blue, which also lands in red.
	link := f.createMemberLink(t, f.rootTok, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	legacy := f.createInvite(t, f.rootTok, personal("blue"))
	joiners := map[string]schema.RedeemInviteResponseV1{}
	for name, code := range map[string]string{"by-link": link.Code, "by-legacy": legacy.Code} {
		rec := f.redeem(t, code, name)
		if rec.Code != http.StatusCreated {
			t.Fatalf("redeem %s = %d %s", name, rec.Code, rec.Body)
		}
		j := decodeBody[schema.RedeemInviteResponseV1](t, rec)
		joiners[name] = j
		if ok, err := f.srv.store.CanDecideApprovals(ctx, f.red.ID, j.Principal.ID); err != nil || ok {
			t.Fatalf("%s decides approvals in red: %v, %v", name, ok, err)
		}
		// No history bound: the joiner reads what was written before it came.
		msgs := decodeBody[schema.ListMessagesResponseV2](t, f.send(t, "GET", "/v2/channels/red/messages", j.Token, "", ""))
		if len(msgs.Messages) != 1 || msgs.Messages[0].Body != "before anyone joined" {
			t.Errorf("%s reads %+v in red, want the earlier message", name, msgs.Messages)
		}
	}

	// Request.
	rec := f.send(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(f.red.ID, 0), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval
	id, quorum := created.ID, created.Quorum
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

	for name, j := range joiners {
		// Each sees it in the default channel, read-only.
		list := decodeBody[schema.ListApprovalsResponseV1](t, f.send(t, "GET", "/v1/approvals", j.Token, "", ""))
		if len(list.Approvals) != 1 || list.Approvals[0].ID != id || len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
			t.Fatalf("%s's list = %+v, want the approval, read-only", name, list)
		}
		// Refused, audited, and the approval stays pending.
		assertErrorBody(t, f.send(t, "POST", path, j.Token, decisionBody(0), ""), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
		if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending || a.Quorum != quorum {
			t.Fatalf("after %s's refusal approval = %+v, %v; want pending with quorum %d", name, a, err, quorum)
		}
		actor := fmt.Sprintf("principal:%d", j.Principal.ID)
		refused := false
		for _, e := range f.audit(t) {
			if e.Action == "access_denied" && e.Actor == actor && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
				refused = true
			}
			if e.Action == store.AuditDecisionCast && e.Actor == actor {
				t.Errorf("%s's refused decision was cast", name)
			}
		}
		if !refused {
			t.Errorf("%s's refusal was not audited", name)
		}
	}

	// Resolve: alice decides, and one decision still meets the quorum.
	rec = f.send(t, "POST", path, f.aliceTok, decisionBody(0), "")
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
