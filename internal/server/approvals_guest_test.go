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

// Issue #706, the whole chain (CLAUDE.md rule 3) in the same run as the guest
// refusals: request, notify, resolve, audit, through the REST API, with a
// guest in the approval's channel. The guest is refused every member action
// over raw REST (guest_not_allowed, audited access_denied), cannot see,
// request or decide the approval, and cannot be given a durable right; a
// member decides it and the chain is the members' chain, unchanged.
func TestGuestsAreRefusedAndMembersDecideFullChain(t *testing.T) {
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
	g := newGuest(t, f.srv, "Pat", time.Hour, 2*time.Hour, general.ID)
	guestTok := g.Token
	guestActor := fmt.Sprintf("principal:%d", g.Principal.ID)

	// Request: alice, a member, asks.
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
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

	// The guest is refused every member action, whatever a page shows.
	whisper := fmt.Sprintf(`{"body":"psst","audience":{"kind":"principals","principal_ids":[%d]}}`, f.alice.ID)
	refused := []struct {
		name, method, path, body string
	}{
		{"upload a file", "POST", "/v2/channels/general/files", "x"},
		{"make an invite", "POST", "/v1/invites", `{"kind":"personal","channels":["general"],"voice":false}`},
		{"request an approval", "POST", "/v1/approvals", createApprovalBody(general.ID, 0)},
		{"list approvals", "GET", "/v1/approvals", ""},
		{"read the approval", "GET", fmt.Sprintf("/v1/approvals/%d", id), ""},
		{"decide the approval", "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), decisionBody(0)},
		{"intervene in a run", "POST", "/v1/runs/1/interventions", `{"kind":"cancel"}`},
		{"whisper", "POST", "/v2/channels/general/messages", whisper},
	}
	for _, r := range refused {
		t.Run("guest cannot "+r.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, r.method, r.path, guestTok, r.body), http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)
		})
	}
	// Channel creation is an operator's, so a guest is refused as any member is.
	assertErrorBody(t, f.do(t, "POST", "/v0/channels", guestTok, `{"name":"mine"}`), http.StatusForbidden, "forbidden")
	// The guest can still post in the channel it was admitted to.
	if rec := f.do(t, "POST", "/v2/channels/general/messages", guestTok, `{"body":"hello"}`); rec.Code != http.StatusCreated {
		t.Fatalf("guest's post in its own channel = %d %s, want 201", rec.Code, rec.Body)
	}

	// An operator cannot give the guest a durable right.
	assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", g.Principal.ID), f.rootTok, `{"may_invite":true}`),
		http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)
	assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", g.Principal.ID), f.rootTok, `{"can_decide":true}`),
		http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)

	// Every refusal of the guest is audited, and nothing it tried was done.
	denied := 0
	for _, e := range f.audit(t) {
		if e.Actor != guestActor {
			continue
		}
		switch e.Action {
		case "access_denied":
			if strings.Contains(e.Detail, schema.ErrorCodeGuestNotAllowed) {
				denied++
			}
		case store.AuditDecisionCast, store.AuditApprovalCreated:
			t.Errorf("a refused guest action was done: %s %s", e.Action, e.Subject)
		}
	}
	if denied != len(refused) {
		t.Errorf("guest refusals audited = %d, want %d", denied, len(refused))
	}
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the guest's refusals approval = %+v, %v; want pending", a, err)
	}

	// Resolve: alice decides.
	rec = f.do(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: created by alice, decided by alice, resolved, in that order.
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
