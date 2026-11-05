package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// TestAdminCatalogMatchesRouteTable is the structural test of the catalog
// (ai-administrators §3.3, §5.3): it lists every action exactly once, each
// on a route that exists with the class and permission the catalog says, and
// none of the routes the design keeps out of it.
func TestAdminCatalogMatchesRouteTable(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	seen := map[schema.AdminAction]bool{}
	for _, e := range adminCatalog {
		if seen[e.action] {
			t.Errorf("%s is in the catalog twice", e.action)
		}
		seen[e.action] = true
		if !e.permission.AgentGrantable() {
			t.Errorf("%s needs %s, which no agent may hold", e.action, e.permission)
		}
		rt, ok := f.srv.routeByPattern[e.pattern]
		if !ok {
			t.Errorf("%s dispatches %q, which is not in the route table", e.action, e.pattern)
			continue
		}
		switch rt.access {
		case accessPermission:
			if rt.perm != e.permission {
				t.Errorf("%s: route %q needs %s, the catalog says %s", e.action, e.pattern, rt.perm, e.permission)
			}
		case accessChannelManager:
			if e.permission != schema.PermManageChannels {
				t.Errorf("%s: a channel-manager route is the catalog's under manage_channels, not %s", e.action, e.permission)
			}
		case accessOperator, accessAuthenticated, accessOperatorOrSelf:
		default:
			t.Errorf("%s: route %q has class %d, which the door never dispatches", e.action, e.pattern, rt.access)
		}
		if strings.HasPrefix(e.pattern, "GET ") != (e.gate == schema.AdminGateDirect && strings.HasPrefix(e.pattern, "GET ")) {
			t.Errorf("%s: a read is never gated", e.action)
		}
	}
	for _, a := range schema.AdminActions {
		if !seen[a] {
			t.Errorf("schema action %s has no catalog row", a)
		}
	}
	// What is never dispatched (§3.3), pinned by route.
	for _, p := range []string{
		"PUT /v1/principals/{id}/role",
		"PUT /v1/principals/{id}/manifest",
		"GET /v1/principals/{id}/manifest",
		"POST /v1/principals/{id}/credentials",
		"POST /v1/credentials/{credential_id}/rotate",
		"POST /v1/hooks",
		"POST /v1/principals/{id}/sign-in-links",
		"POST /v1/principals/hide",
		"POST /v1/channels/{channel}/nets",
		"PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}",
		"GET /v1/guests",
		"POST /v1/guests/{id}/end",
		"POST /v1/approvals",
		"POST /v1/approvals/{id}/decisions",
		"POST /v1/admin/actions",
		"POST /v1/admin/actions/{approval_id}/run",
	} {
		if _, ok := f.srv.routeByPattern[p]; !ok {
			t.Errorf("excluded route %q is no longer in the table: update the list", p)
		}
		if adminCatalogPatterns[p] {
			t.Errorf("excluded route %q is in the catalog", p)
		}
	}
	// The gated defaults the design names stay gated.
	for _, a := range []schema.AdminAction{
		schema.AdminActionDisableMember, schema.AdminActionEnableMember, schema.AdminActionHideMember,
		schema.AdminActionRevokeCredential, schema.AdminActionRevokeAllCredentials, schema.AdminActionRevokeHook,
		schema.AdminActionSetChannelVisibility, schema.AdminActionArchiveChannel, schema.AdminActionRemoveChannelMember,
		schema.AdminActionSetChannelMemberRights, schema.AdminActionDisableLinkJoiners, schema.AdminActionDisableInvitees,
		schema.AdminActionResumeInvites, schema.AdminActionSetJoinSettings, schema.AdminActionSetPermissions,
	} {
		if adminCatalogByAction[a].gate != schema.AdminGateApproval {
			t.Errorf("%s must be gated by approval", a)
		}
	}
}

// TestAddChannelMemberBodyCarriesNoRights (security review of #1338): the
// dispatched body of add_channel_member names the member and nothing else.
func TestAddChannelMemberBodyCarriesNoRights(t *testing.T) {
	entry := adminCatalogByAction[schema.AdminActionAddChannelMember]
	ctx := context.WithValue(context.Background(), adminBuildingKey{}, entry)
	in := adminBuildInput{target: &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: 2, Name: "ops"}, params: json.RawMessage(`{"principal_id":7}`)}
	req, _, refusal := entry.build(ctx, nil, in)
	if refusal != nil {
		t.Fatal(refusal)
	}
	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["id"] != float64(7) {
		t.Errorf("dispatched body = %s, want only the member", req.body)
	}
}

// TestDispatchMarkAdmitsOnlyItsRoute: every wrapper that admits the door's
// dispatch (humansOnlyBecause, the operator and permission classes,
// inviteCaller, the members gate) admits it on the route the mark names and
// no other: a request carrying the route mark of one catalog route answers,
// on any other route, what the same agent's undispatched request answers.
// The store's mark, the grant as authority, is set only with the route mark
// inside dispatch, whose request the mux must route to the catalog's own
// route (TestDispatchRoutesToItsCatalogRoute).
func TestDispatchMarkAdmitsOnlyItsRoute(t *testing.T) {
	d := newParityFixture(t)
	mark := adminCatalogByAction[schema.AdminActionListMembers]
	answer := func(rec *httptest.ResponseRecorder) string {
		var e schema.Error
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return fmt.Sprintf("%d %s", rec.Code, e.Code)
	}
	serve := func(method, path, body string, marked bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+d.agentTok)
		rec := httptest.NewRecorder()
		if !marked {
			d.srv.Handler().ServeHTTP(rec, req)
			return rec
		}
		// The agent's identity as the middleware would set it, plus the marks.
		_, cred, err := d.srv.store.ResolveRequestCredential(req.Context(), d.agentTok)
		if err != nil {
			t.Fatal(err)
		}
		ctx := withRequestCredential(withCredentialID(withCaller(req.Context(), d.agent), cred.ID), cred)
		ctx = context.WithValue(ctx, adminDispatchKey{}, adminDispatch{action: mark.action, pattern: mark.pattern, permission: mark.permission})
		d.srv.mux.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	checked := 0
	for _, rt := range d.srv.routes {
		// DELETE /v1/credentials/current ends the caller's own credential.
		if rt.pattern == mark.pattern || rt.access == accessExempt || isSocketRoute(rt.pattern) || rt.pattern == "DELETE /v1/credentials/current" {
			continue
		}
		method, path := fillPath(rt.pattern, &schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: d.alice.ID, Name: "general"}, d.alice.ID)
		if strings.Contains(path, "{") {
			continue
		}
		body := ""
		if method == http.MethodPost || method == http.MethodPut {
			body = "{}"
		}
		want := answer(serve(method, path, body, false))
		got := answer(serve(method, path, body, true))
		if got != want && !strings.HasPrefix(want, "2") {
			t.Errorf("%s: marked for %s = %s, unmarked = %s", rt.pattern, mark.pattern, got, want)
		}
		if strings.HasPrefix(got, "2") && !strings.HasPrefix(want, "2") {
			t.Errorf("%s: a mark for another route admitted the agent (%s, unmarked %s)", rt.pattern, got, want)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("checked %d routes", checked)
	}
	if p, _ := d.srv.store.PrincipalByID(context.Background(), d.alice.ID); p.DisabledAt != nil || p.HiddenAt != nil {
		t.Fatal("a mismatched mark acted on alice")
	}
}

// TestDispatchRoutesToItsCatalogRoute: every catalog row's built request is
// routed by the mux to that row's route, so the marks never reach another.
func TestDispatchRoutesToItsCatalogRoute(t *testing.T) {
	d := newParityFixture(t)
	for _, e := range adminCatalog {
		var target *schema.AdminTargetV1
		if e.target != "" {
			target = &schema.AdminTargetV1{Kind: e.target, ID: 7, Name: "general"}
		}
		method, path := fillPath(e.pattern, target, 7)
		req := httptest.NewRequest(method, path, nil)
		if _, pattern := d.srv.mux.Handler(req); pattern != e.pattern {
			t.Errorf("%s: %s %s routes to %q, not %q", e.action, method, path, pattern, e.pattern)
		}
	}
	// And dispatch refuses a request the mux would route elsewhere.
	wrong := adminCatalogByAction[schema.AdminActionListMembers]
	if _, err := d.srv.dispatch(context.Background(), wrong, 0, adminRequest{method: http.MethodPost, path: "/v1/principals/hide", body: []byte(`{}`)}); err == nil {
		t.Error("dispatch served a request the mux routes to another pattern")
	}
	_ = store.PrincipalAgent
}
