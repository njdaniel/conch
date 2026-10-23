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

// Issue #711, the whole chain (CLAUDE.md rule 3): request, notify, resolve,
// audit, through the REST API, with a guest who converts in the approval's
// channel. Conversion changes no approval right: can_decide is 0 on every
// membership of the guest before and after it, and the converted member's
// decision is refused (decision_not_permitted, the approval still pending)
// until an operator grants the right through the existing, separate route;
// then the member decides and the chain is the ordinary one. This test only
// calls the approval routes; it changes none of them.
func TestConvertedMemberDecidesOnlyOnceGrantedFullChain(t *testing.T) {
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
	f.mustPutJoin(t, openBoth("general"))
	g := mustJoined(t, secureDo(t, f, "POST", "/v1/join/guest", "", `{"label":"Pat"}`))
	id := g.Principal.ID
	canDecide := func(stage string) {
		t.Helper()
		members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", f.rootTok, ""))
		var seen bool
		for _, m := range members.Members {
			if m.PrincipalID != id {
				continue
			}
			seen = true
			if m.CanDecide == nil || *m.CanDecide {
				t.Errorf("%s: member entry = %+v, want can_decide false", stage, m)
			}
		}
		if !seen {
			t.Fatalf("%s: the principal is not a member of general", stage)
		}
		if ok, err := f.srv.store.CanDecideApprovals(ctx, general.ID, id); err != nil || ok {
			t.Errorf("%s: CanDecideApprovals = %v, %v", stage, ok, err)
		}
	}
	canDecide("as a guest")
	rec := secureDo(t, f, "POST", "/v1/guest/convert", g.Token, inviteJSON(t, schema.ConvertGuestRequestV1{Name: "patricia", Password: testJoinPassword}))
	if rec.Code != http.StatusOK {
		t.Fatalf("convert = %d %s", rec.Code, rec.Body)
	}
	member := decodeBody[schema.ConvertGuestResponseV1](t, rec)
	canDecide("converted")

	// Request: alice asks.
	rec = f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	approvalID := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", approvalID)

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

	// The converted member's decision is refused, audited, and changes
	// nothing.
	assertErrorBody(t, f.do(t, "POST", path, member.Token, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, approvalID); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the refusal approval = %+v, %v; want pending", a, err)
	}
	canDecide("after the refused decision")

	// An operator grants the right, separately; then the member decides.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", id), f.rootTok, `{"can_decide":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, "POST", path, member.Token, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("the granted member's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: created by alice, decided by the member, resolved, in that
	// order; the member's refusal before the grant is on record.
	actor := fmt.Sprintf("principal:%d", id)
	var chain []string
	refused := false
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", approvalID) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
		case "access_denied":
			refused = refused || (e.Actor == actor && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted))
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + actor,
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
	if !refused {
		t.Error("the refused decision was not audited")
	}
}
