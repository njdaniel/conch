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

// Issue #981, the whole chain (CLAUDE.md rule 3) over the real server: a
// member who creates a channel through POST /v1/channels holds the right to
// decide its approvals (O9: can_decide = 1, manages = 1), and creating it
// gives them no right anywhere else. Request, notify, resolve and audit run
// through REST in the creator's channel; in a channel the creator is not in
// they cannot see the approval, in one where they are a member without the
// right they cannot decide it, and that channel's quorum is the one it was.
func TestChannelCreatorDecidesOnlyInTheirChannelFullChain(t *testing.T) {
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

	carol, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "carol")
	if err != nil {
		t.Fatal(err)
	}
	_, carolTok, err := f.srv.store.CreateCredential(ctx, "system", carol.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// ops: the owner's, alice not in it. board: alice a member without the
	// right, the owner and carol deciding.
	ops, err := f.srv.store.CreateChannelAs(ctx, owner, "ops", f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	board, err := f.srv.store.CreateChannelAs(ctx, owner, "board", f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	if _, err := f.srv.store.PutChannelMember(ctx, owner, board.ID, f.alice.ID, f.root.ID, &no); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutChannelMember(ctx, owner, board.ID, carol.ID, f.root.ID, &yes); err != nil {
		t.Fatal(err)
	}

	// alice creates her channel.
	rec := f.do(t, http.MethodPost, "/v1/channels", f.aliceTok, createChannelV1Body("alice-room", schema.ChannelPrivate))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	room := decodeBody[schema.CreateChannelResponseV1](t, rec).Channel

	// Request: carol cannot ask in alice's room; alice asks.
	if rec := f.do(t, http.MethodPost, "/v1/approvals", carolTok, createApprovalBody(room.ID, 0)); rec.Code == http.StatusCreated {
		t.Fatalf("a non-member asked in alice's room: %s", rec.Body)
	}
	rec = f.do(t, http.MethodPost, "/v1/approvals", f.aliceTok, createApprovalBody(room.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice's request = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID

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

	// No other channel: alice cannot see an approval in ops, nor decide one
	// in board, whose quorum of two is unchanged.
	rec = f.do(t, http.MethodPost, "/v1/approvals", f.rootTok, createApprovalBody(ops.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("ops request = %d %s", rec.Code, rec.Body)
	}
	opsID := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	assertErrorBody(t, f.do(t, http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", opsID), f.aliceTok, decisionBody(0)),
		http.StatusNotFound, "approval_not_found")

	var quorum2 schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(board.ID, 0)), &quorum2); err != nil {
		t.Fatal(err)
	}
	quorum2.Quorum = 2
	body, err := json.Marshal(quorum2)
	if err != nil {
		t.Fatal(err)
	}
	rec = f.do(t, http.MethodPost, "/v1/approvals", f.rootTok, string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("board request = %d %s", rec.Code, rec.Body)
	}
	boardID := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	if rec := f.do(t, http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", boardID), f.rootTok, decisionBody(0)); rec.Code != http.StatusOK {
		t.Fatalf("owner's board decision = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", boardID), f.aliceTok, decisionBody(0)),
		http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, boardID); err != nil || a.State != schema.ApprovalStatePending || a.Quorum != 2 {
		t.Fatalf("board approval after alice's attempt = %+v, %v; want pending, quorum 2", a, err)
	}
	rec = f.do(t, http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", boardID), carolTok, decisionBody(0))
	if rec.Code != http.StatusOK || decodeBody[schema.CastDecisionResponseV1](t, rec).State != schema.ApprovalStateResolved {
		t.Fatalf("carol's board decision = %d %s, want resolved", rec.Code, rec.Body)
	}
	if a, err := f.srv.store.ApprovalByID(ctx, opsID); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("ops approval = %+v, %v; want pending", a, err)
	}

	// Resolve: alice decides in her own channel.
	rec = f.do(t, http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", id), f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: the channel made by alice, alice its manager; the approval
	// created by alice, decided by alice, resolved, in that order; and no
	// decision of hers outside her channel.
	var made, chain []string
	for _, e := range f.audit(t) {
		switch {
		case e.Subject == fmt.Sprintf("channel:%d", room.ID) && e.Actor == alice:
			made = append(made, e.Action)
		case e.Subject == fmt.Sprintf("approval:%d", id):
			switch e.Action {
			case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
				chain = append(chain, e.Action+" "+e.Actor)
			}
		case e.Action == store.AuditDecisionCast && e.Actor == alice:
			t.Errorf("alice decided outside her channel: %+v", e)
		}
	}
	if want := []string{store.AuditChannelCreated, store.AuditMemberAdded, store.AuditMemberManageGranted}; strings.Join(made, "|") != strings.Join(want, "|") {
		t.Errorf("channel rows = %q, want %q", made, want)
	}
	want := []string{
		store.AuditApprovalCreated + " " + alice,
		store.AuditDecisionCast + " " + alice,
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
