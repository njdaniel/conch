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

// Issue #720, the whole chain (CLAUDE.md rule 3) with a guest in the
// approval's channel and a quorum of two: request, notify, resolve, audit.
// The guest has no decision right: its decision is refused over REST as every
// guest's approval action is (guest_not_allowed, audited), and the deciding
// transaction would refuse it decision_not_permitted (ErrNoDecideRight)
// whatever its membership row said. Its membership counts nothing toward
// quorum: one member's decision of two leaves the approval pending, and it
// resolves on the second member's. The guests' other refusals are #706's;
// the store-level quorum rule is store/approvals_guest_test.go's.
func TestGuestInTheChannelQuorumFullChain(t *testing.T) {
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
	guestActor := fmt.Sprintf("principal:%d", g.Principal.ID)

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

	// Notify.
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

	// The guest cannot see, read or decide the approval, and no notification
	// or preview is addressed to it: ntfy goes to the operator's topics and
	// no socket carries approvals. Each refusal is audited, and the approval
	// stays pending.
	for _, r := range []struct {
		name, method, path, body string
	}{
		{"list approvals", "GET", "/v1/approvals", ""},
		{"read the approval", "GET", fmt.Sprintf("/v1/approvals/%d", id), ""},
		{"decide the approval", "POST", path, decisionBody(0)},
	} {
		assertErrorBody(t, f.do(t, r.method, r.path, g.Token, r.body), http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)
	}
	// The deciding transaction's own refusal is decision_not_permitted, what
	// a member without the right is answered: a guest is never one, so the
	// REST refusal above is guest_not_allowed, but the store's answer is the
	// same one's, whatever the membership row says.
	if _, res, err := f.srv.store.CastMemberDecision(ctx, id, g.Principal.ID, "approve", "fine"); !errors.Is(err, store.ErrNoDecideRight) || res != nil {
		t.Fatalf("the guest's decision in the transaction = %v, %v; want ErrNoDecideRight and no resolution", res, err)
	}
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the guest's refusals approval = %+v, %v; want pending", a, err)
	}

	// One member's decision of a quorum of two, with a guest in the channel,
	// resolves nothing: the guest's membership counts nothing.
	rec = f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if state := decodeBody[schema.CastDecisionResponseV1](t, rec).State; state != schema.ApprovalStatePending {
		t.Fatalf("state after one member's decision = %s, want pending (quorum 2)", state)
	}

	// Resolve: the operator's is the second member's decision.
	rec = f.do(t, "POST", path, f.rootTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("the operator's decision = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.CastDecisionResponseV1](t, rec)
	if resp.State != schema.ApprovalStateResolved || resp.Resolution == nil {
		t.Fatalf("after the operator = %+v, want resolved", resp)
	}
	if n := len(resp.Resolution.Decisions); n != 2 {
		t.Errorf("the resolution holds %d decisions, want the two members'", n)
	}
	waitNotified(2, "the resolution")

	// Audit: created, the two members' decisions, resolved, in that order;
	// the guest's refusals are on record and none of them cast a decision.
	var chain []string
	refused := 0
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", id) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
			if e.Action == store.AuditDecisionCast && e.Actor == guestActor {
				t.Error("the guest's refused decision was cast")
			}
		case "access_denied":
			if e.Actor == guestActor && strings.Contains(e.Detail, schema.ErrorCodeGuestNotAllowed) {
				refused++
			}
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.root.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
	if refused != 3 {
		t.Errorf("the guest's refusals audited = %d, want 3", refused)
	}
}

// Issue #720, the whole chain for a converted member, read back from the
// audit export: conversion grants no right, the member's decision is refused
// (decision_not_permitted, the approval still pending) until an operator
// grants the right through the separate route; then the member decides, the
// chain completes, and the export carries the refusal, the grant and the
// whole approval chain. #711's test reads the store's audit; this one reads
// the operator's export.
func TestConvertedMemberGrantExportedFullChain(t *testing.T) {
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
	f.mustPutJoin(t, openAccounts("general"))
	g := f.seedGuest(t, "Pat")
	rec := secureDo(t, f, "POST", "/v1/guest/convert", g.Token, inviteJSON(t, schema.ConvertGuestRequestV1{Name: "patricia", Password: testJoinPassword}))
	if rec.Code != http.StatusOK {
		t.Fatalf("convert = %d %s", rec.Code, rec.Body)
	}
	member := decodeBody[schema.ConvertGuestResponseV1](t, rec)
	actor := fmt.Sprintf("principal:%d", g.Principal.ID)

	// Request; notify.
	rec = f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)
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

	// The converted member's decision is refused, and the approval stays
	// pending.
	assertErrorBody(t, f.do(t, "POST", path, member.Token, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after the refusal approval = %+v, %v; want pending", a, err)
	}

	// The operator grants the right, separately; the member decides; the
	// chain resolves.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", g.Principal.ID), f.rootTok, `{"can_decide":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, "POST", path, member.Token, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("the granted member's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notified)
		mu.Unlock()
		if n > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resolution was never notified")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The export carries the refusal, the grant and the whole chain.
	rec = f.do(t, "GET", "/v1/audit", f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d %s", rec.Code, rec.Body)
	}
	var chain []string
	var refused, granted bool
	for _, e := range parseJSONL(t, rec.Body.String()) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", id) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
		case "access_denied":
			refused = refused || (e.Actor == actor && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted))
		case store.AuditMemberDecideGranted:
			granted = granted || (e.Actor == fmt.Sprintf("principal:%d", f.root.ID) && e.Detail == fmt.Sprintf("principal=%d", g.Principal.ID))
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + actor,
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("exported audit chain = %q, want %q", chain, want)
	}
	if !refused {
		t.Error("the export holds no record of the member's refused decision")
	}
	if !granted {
		t.Error("the export holds no record of the operator's grant")
	}
}

// Issue #720: an agent-requested (MCP) approval in a channel with a guest
// behaves as before for members — the guest is refused over REST and the
// members' chain runs request, notify, resolve, audit unchanged.
func TestMCPApprovalFullChainWithAGuestInTheChannel(t *testing.T) {
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
	f.srv.cfg.MCPBearerTokens = map[string]int64{"token-1": f.bot.ID}
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	g := newGuest(t, f.srv, "Pat", time.Hour, 2*time.Hour, general.ID)

	// The agent requests over MCP; notify.
	created := mcpCallInProcess[schema.RequestApprovalOutput](t, f.srv, 1, "request_approval", approvalMCPArguments(general.ID, time.Now().Add(time.Hour)))
	path := fmt.Sprintf("/v1/approvals/%d/decisions", created.ID)
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

	// The guest is refused over REST, as on any approval.
	assertErrorBody(t, f.do(t, "POST", path, g.Token, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)

	// A member decides; the agent reads the resolution back over MCP.
	rec := f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	checked := mcpCallInProcess[schema.CheckDecisionOutput](t, f.srv, 2, "check_decision", map[string]any{"approval_id": created.ID})
	if checked.State != schema.ApprovalStateResolved || checked.Resolution == nil {
		t.Fatalf("check = %+v, want resolved with resolution", checked)
	}

	// Audit: created by the agent, decided by the member, resolved, in that
	// order; the guest cast nothing.
	var chain []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", created.ID) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
			if e.Action == store.AuditDecisionCast && e.Actor == fmt.Sprintf("principal:%d", g.Principal.ID) {
				t.Error("the guest's refused decision was cast")
			}
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.bot.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
