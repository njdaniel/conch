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

// Issue #1206 (CLAUDE.md rule 3): the full request -> notify -> resolve ->
// audit chain when members are added through POST /v1/channels/{c}/members by
// id. A person added with can_decide true decides; one added without it is
// refused; and alice, an approver already in the channel, is "added" again
// with can_decide false and keeps deciding: the add never takes the right
// away, where PUT .../members/{id} with can_decide false does.
func TestAddIfAbsentMembersDecideFullChain(t *testing.T) {
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
	dana, danaTok := person("dana")
	erin, erinTok := person("erin")
	add := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return f.do(t, "POST", "/v1/channels/general/members", f.rootTok, body)
	}

	// dana is added as an approver, erin without the right.
	rec := add(fmt.Sprintf(`{"id":%d,"can_decide":true}`, dana.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add dana = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.ChannelMemberV1](t, rec); m.PrincipalID != dana.ID || m.CanDecide == nil || !*m.CanDecide {
		t.Fatalf("dana = %+v, want can_decide true", m)
	}
	rec = add(fmt.Sprintf(`{"id":%d}`, erin.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add erin = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.ChannelMemberV1](t, rec); m.CanDecide == nil || *m.CanDecide {
		t.Fatalf("erin = %+v, want can_decide false", m)
	}

	// alice already decides in general. An add asking can_decide false, by id
	// and by name, answers 200 with her membership unchanged and writes nothing.
	before := len(f.audit(t))
	for _, body := range []string{fmt.Sprintf(`{"id":%d,"can_decide":false}`, f.alice.ID), `{"name":"alice"}`} {
		rec = add(body)
		if rec.Code != http.StatusOK {
			t.Fatalf("add alice again %s = %d %s", body, rec.Code, rec.Body)
		}
		if m := decodeBody[schema.ChannelMemberV1](t, rec); m.CanDecide == nil || !*m.CanDecide {
			t.Fatalf("alice after an add = %+v, want can_decide still true", m)
		}
	}
	for _, e := range f.audit(t)[before:] {
		if e.Action == store.AuditMemberDecideRevoked || e.Action == store.AuditMemberAdded {
			t.Errorf("an add for alice, already a member, was audited as %s", e.Action)
		}
	}

	// Request an approval needing two decisions.
	var req schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(general.ID, 0)), &req); err != nil {
		t.Fatal(err)
	}
	req.Quorum = 2
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec = f.do(t, "POST", "/v1/approvals", f.aliceTok, string(raw))
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

	// erin, added without the right, is refused and audited.
	assertErrorBody(t, f.do(t, "POST", path, erinTok, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	refused := false
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", erin.ID) && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
			refused = true
		}
	}
	if !refused {
		t.Error("erin's refused decision was not audited")
	}

	// dana decides (pending under quorum 2), then alice resolves it.
	if rec := f.do(t, "POST", path, danaTok, decisionBody(0)); rec.Code != http.StatusOK {
		t.Fatalf("dana's decision = %d %s", rec.Code, rec.Body)
	}
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after dana approval = %+v, %v; want pending", a, err)
	}
	rec = f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}
	waitNtfy(2, "approval resolution")

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
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", dana.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}

// The add route's refusals for the id form match its name form and the PUT's
// (issue #1206).
func TestAddChannelMemberByIDRefusals(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t, AuthRequired)
	mgr, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	_, mgrTok, err := f.srv.store.CreateCredential(ctx, "system", mgr.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", mgr.ID), f.rootTok, `{"can_decide":false,"manages":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("grant mgr = %d %s", rec.Code, rec.Body)
	}
	cand, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "cand")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		token string
		body  string
		code  int
		err   string
	}{
		{"a manager sending can_decide true", mgrTok, fmt.Sprintf(`{"id":%d,"can_decide":true}`, cand.ID), http.StatusForbidden, "forbidden"},
		{"a manager sending can_decide false", mgrTok, fmt.Sprintf(`{"id":%d,"can_decide":false}`, cand.ID), http.StatusForbidden, "forbidden"},
		{"both a name and an id", f.rootTok, fmt.Sprintf(`{"id":%d,"name":"cand"}`, cand.ID), http.StatusBadRequest, "invalid_request"},
		{"neither", f.rootTok, `{}`, http.StatusBadRequest, "invalid_request"},
		{"an unknown id", f.rootTok, `{"id":999999}`, http.StatusNotFound, "principal_not_found"},
		{"an agent", f.rootTok, fmt.Sprintf(`{"id":%d}`, f.bot.ID), http.StatusNotFound, "principal_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, "POST", "/v1/channels/general/members", tc.token, tc.body), tc.code, tc.err)
		})
	}
	// Nothing above added cand.
	members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", f.rootTok, ""))
	for _, m := range members.Members {
		if m.PrincipalID == cand.ID {
			t.Errorf("a refused add made cand a member: %+v", m)
		}
	}
	// A manager may add by id, without a decide right.
	rec := f.do(t, "POST", "/v1/channels/general/members", mgrTok, fmt.Sprintf(`{"id":%d}`, cand.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("manager add by id = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.ChannelMemberV1](t, rec); m.CanDecide == nil || *m.CanDecide {
		t.Errorf("manager-added member = %+v, want can_decide false", m)
	}
}
