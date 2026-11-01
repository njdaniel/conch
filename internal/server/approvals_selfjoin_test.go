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

// Issue #982, the whole chain (CLAUDE.md rule 3) over the real server: a
// member who joins a public channel through POST /v1/channels/{channel}/join
// holds no right to decide its approvals (O12: can_decide = 0). They see the
// request, which is notified, and their decision is refused
// decision_not_permitted; the approval resolves through the channel's
// deciders, with its quorum unchanged, and the audit log says so. A decider
// who leaves through POST /v1/channels/{channel}/leave takes their decision
// with them: it stops counting toward quorum, the approval stays pending and
// open to the deciders who remain, and it resolves when they meet quorum.
func TestSelfJoinAndLeaveKeepTheApprovalChain(t *testing.T) {
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
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	alice := fmt.Sprintf("principal:%d", f.alice.ID)

	person := func(name string) (store.Principal, string) {
		t.Helper()
		p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	carol, carolTok := person("carol")
	dave, daveTok := person("dave")

	// hall: public, the owner, carol and dave deciding.
	hall, err := f.srv.store.CreateChannelAs(ctx, owner, "hall", f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	public := schema.ChannelPublic
	if _, err := f.srv.store.ChannelSettings(ctx, owner, f.root.ID, hall.ID, store.ChannelSettingsChange{Visibility: &public}, time.Now()); err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, p := range []store.Principal{carol, dave} {
		if _, err := f.srv.store.PutChannelMember(ctx, owner, hall.ID, p.ID, f.root.ID, &yes); err != nil {
			t.Fatal(err)
		}
	}

	// alice joins hall herself: a member, not a decider.
	rec := f.do(t, http.MethodPost, "/v1/channels/hall/join", f.aliceTok, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice's join = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.JoinChannelResponseV1](t, rec).Member; m.CanDecide == nil || *m.CanDecide {
		t.Fatalf("alice's membership = %+v, want can_decide false", m)
	}

	// Request: the owner asks, quorum 2.
	var req schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(hall.ID, 0)), &req); err != nil {
		t.Fatal(err)
	}
	req.Quorum = 2
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec = f.do(t, http.MethodPost, "/v1/approvals", f.rootTok, string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	approvalPath := fmt.Sprintf("/v1/approvals/%d", id)

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

	// alice, a self-joined member, sees the request and may not decide it.
	if rec := f.do(t, http.MethodGet, approvalPath, f.aliceTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice reads the approval = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, http.MethodPost, approvalPath+"/decisions", f.aliceTok, decisionBody(0)),
		http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)

	// dave decides, then leaves: his decision stops counting, and the
	// approval is pending with its quorum of 2.
	if rec := f.do(t, http.MethodPost, approvalPath+"/decisions", daveTok, decisionBody(0)); rec.Code != http.StatusOK {
		t.Fatalf("dave's decision = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPost, "/v1/channels/hall/leave", daveTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("dave's leave = %d %s", rec.Code, rec.Body)
	}
	assertPending := func(when string) {
		t.Helper()
		if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending || a.Quorum != 2 {
			t.Fatalf("approval %s = %+v, %v; want pending, quorum 2", when, a, err)
		}
	}
	assertPending("after dave left")
	// The owner's decision is one concurring decision of a current
	// decider: dave's no longer counts, so quorum is not met.
	rec = f.do(t, http.MethodPost, approvalPath+"/decisions", f.rootTok, decisionBody(0))
	if rec.Code != http.StatusOK || decodeBody[schema.CastDecisionResponseV1](t, rec).State != schema.ApprovalStatePending {
		t.Fatalf("owner's decision = %d %s, want pending", rec.Code, rec.Body)
	}
	assertPending("after the owner decided")

	// Resolve: carol, a decider who stayed, meets quorum.
	rec = f.do(t, http.MethodPost, approvalPath+"/decisions", carolTok, decisionBody(0))
	if rec.Code != http.StatusOK || decodeBody[schema.CastDecisionResponseV1](t, rec).State != schema.ApprovalStateResolved {
		t.Fatalf("carol's decision = %d %s, want resolved", rec.Code, rec.Body)
	}

	// Audit: alice's join and dave's leave, each by the member themselves;
	// the approval created, decided three times and resolved; and no
	// decision of alice's.
	daveActor := fmt.Sprintf("principal:%d", dave.ID)
	var memberships, chain []string
	for _, e := range f.audit(t) {
		switch {
		case e.Subject == fmt.Sprintf("channel:%d", hall.ID) && (e.Actor == alice || e.Actor == daveActor):
			memberships = append(memberships, e.Action+" "+e.Actor)
		case e.Subject == fmt.Sprintf("approval:%d", id):
			switch e.Action {
			case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
				chain = append(chain, e.Action+" "+e.Actor)
			}
		}
		if e.Action == store.AuditDecisionCast && e.Actor == alice {
			t.Errorf("alice decided: %+v", e)
		}
	}
	if want := []string{store.AuditMemberAdded + " " + alice, store.AuditMemberRemoved + " " + daveActor}; strings.Join(memberships, "|") != strings.Join(want, "|") {
		t.Errorf("membership rows = %q, want %q", memberships, want)
	}
	want := []string{
		store.AuditApprovalCreated + " " + owner,
		store.AuditDecisionCast + " " + daveActor,
		store.AuditDecisionCast + " " + owner,
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", carol.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
