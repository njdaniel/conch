package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #790 (ADR-010, docs/design/delegated-administration.md §4–§7): the
// owner and channel-manager route classes, the role route, owner protection
// and the per-viewer member fields, over HTTP.

// adminFixture is an authFixture (root the owner, alice a member of
// "general", bot an agent in it) with every other standing the capability
// matrix names:
//   - op2, an operator without the owner mark;
//   - own2, a second owner;
//   - mgr, a human who manages "general";
//   - guest, a guest visiting "general";
//   - outsider, a human in no channel the routes name.
//
// "lobby" is a second channel mgr shares with cand, a plain member mgr may
// add to "general".
type adminFixture struct {
	*authFixture
	general, lobby store.Channel

	op2, own2, mgr, outsider, cand       store.Principal
	op2Tok, own2Tok, mgrTok, outsiderTok string
	candTok, guestTok                    string
	guest                                store.Principal
	own2CredID                           int64
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	ctx := context.Background()
	f := &adminFixture{authFixture: newAuthFixture(t, AuthRequired)}
	st := f.srv.store
	var err error
	if f.general, err = st.ChannelByName(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if f.lobby, err = st.CreateChannel(ctx, "lobby"); err != nil {
		t.Fatal(err)
	}
	human := func(name string) store.Principal {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatalf("CreatePrincipal %s: %v", name, err)
		}
		return p
	}
	cred := func(id int64) (string, int64) {
		t.Helper()
		c, tok, err := st.CreateCredential(ctx, "system", id, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return tok, c.ID
	}
	no, yes := false, true

	f.op2 = human("op2")
	if _, err := st.SetRole(ctx, "system", f.root.ID, f.op2.ID, store.RoleOperator, &no); err != nil {
		t.Fatalf("SetRole op2: %v", err)
	}
	f.op2Tok, _ = cred(f.op2.ID)
	if f.op2, err = st.PrincipalByID(ctx, f.op2.ID); err != nil {
		t.Fatal(err)
	}

	f.own2 = human("own2")
	if _, err := st.SetRole(ctx, "system", f.root.ID, f.own2.ID, store.RoleOperator, &yes); err != nil {
		t.Fatalf("SetRole own2: %v", err)
	}
	// The mark revokes what the target held, so its credential comes after.
	f.own2Tok, f.own2CredID = cred(f.own2.ID)
	if f.own2, err = st.PrincipalByID(ctx, f.own2.ID); err != nil {
		t.Fatal(err)
	}

	f.mgr = human("mgr")
	f.mgrTok, _ = cred(f.mgr.ID)
	if _, err := st.PutChannelMember(ctx, "system", f.general.ID, f.mgr.ID, 0, &no, &yes); err != nil {
		t.Fatalf("grant mgr: %v", err)
	}
	f.cand = human("cand")
	f.candTok, _ = cred(f.cand.ID)
	for _, id := range []int64{f.mgr.ID, f.cand.ID} {
		if _, err := st.AddChannelMember(ctx, "system", f.lobby.ID, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	f.outsider = human("outsider")
	f.outsiderTok, _ = cred(f.outsider.ID)

	g := newGuest(t, f.srv, "visitor", time.Hour, 2*time.Hour, f.general.ID)
	f.guest, f.guestTok = g.Principal, g.Token
	return f
}

// deniedRows counts the access_denied rows written for caller.
func (f *adminFixture) deniedRows(t *testing.T, callerID int64) int {
	t.Helper()
	n := 0
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", callerID) {
			n++
		}
	}
	return n
}

// TestAdministrationRouteMatrix sends each route whose class issue #790
// changed or added as every standing of §4, with a request that changes
// nothing even where it is let through, and checks the status, the code and
// whether the refusal was audited. A non-member's refusal on the manager
// class is byte-identical to the answer for a channel that never existed,
// and writes no access_denied row.
func TestAdministrationRouteMatrix(t *testing.T) {
	f := newAdminFixture(t)
	type want struct {
		status  int
		code    string
		audited bool
	}
	forbidden := want{http.StatusForbidden, "forbidden", true}
	hidden := want{http.StatusNotFound, "channel_not_found", false}
	bad := want{http.StatusBadRequest, "invalid_request", false}
	ok := want{http.StatusOK, "", false}

	callers := []struct {
		who   string
		id    int64
		token string
	}{
		{"owner", f.root.ID, f.rootTok},
		{"operator", f.op2.ID, f.op2Tok},
		{"manager", f.mgr.ID, f.mgrTok},
		{"member", f.alice.ID, f.aliceTok},
		{"guest", f.guest.ID, f.guestTok},
		{"agent", f.bot.ID, f.botTok},
		{"non-member", f.outsider.ID, f.outsiderTok},
	}
	operatorRoute := func(allowed want) [7]want {
		return [7]want{allowed, allowed, forbidden, forbidden, forbidden, forbidden, forbidden}
	}
	ownerRoute := func(allowed want) [7]want {
		return [7]want{allowed, forbidden, forbidden, forbidden, forbidden, forbidden, forbidden}
	}
	selfRoute := func(allowed want) [7]want {
		return [7]want{allowed, allowed, forbidden, forbidden, forbidden, forbidden, forbidden}
	}
	// An owner's alone by default, by a permission (ADR-014, #1015): an
	// operator is told which permission it lacks; everyone else's answer is
	// the owner route's, unchanged.
	ownerByDefaultRoute := func(allowed want) [7]want {
		denied := want{http.StatusForbidden, schema.ErrorCodePermissionDenied, true}
		return [7]want{allowed, denied, forbidden, forbidden, forbidden, forbidden, forbidden}
	}
	rows := []struct {
		name, method, path, body string
		// nosuch is the same request on a channel that never existed, for
		// the rows of the manager class; empty for the rest.
		nosuch string
		want   [7]want
	}{
		{"set a role", "PUT", fmt.Sprintf("/v1/principals/%d/role", f.cand.ID), `{"role":"bogus"}`, "", ownerRoute(bad)},
		{"export the audit log", "GET", "/v1/audit?limit=1", "", "", ownerByDefaultRoute(ok)},
		{"change the joining settings", "PUT", "/v1/join/settings", `{}`, "", ownerByDefaultRoute(bad)},
		{"add a member by id", "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), `{"bogus":true}`,
			fmt.Sprintf("/v1/channels/nosuch/members/%d", f.cand.ID),
			[7]want{bad, bad, bad, forbidden, forbidden, forbidden, hidden}},
		{"add a member by name", "POST", "/v1/channels/general/members", `{}`, "/v1/channels/nosuch/members",
			[7]want{bad, bad, bad, forbidden, forbidden, forbidden, hidden}},
		{"remove a member", "DELETE", "/v1/channels/general/members/0", "", "/v1/channels/nosuch/members/0",
			[7]want{bad, bad, bad, forbidden, forbidden, forbidden, hidden}},
		{"create a channel", "POST", "/v0/channels", `{}`, "", operatorRoute(bad)},
		{"create a principal", "POST", "/v0/principals", `{}`, "", operatorRoute(bad)},
		{"rename a principal", "PUT", fmt.Sprintf("/v1/principals/%d/name", f.cand.ID), `{}`, "", selfRoute(bad)},
		{"create a net", "POST", "/v1/channels/general/nets", `{}`, "", operatorRoute(bad)},
		{"archive a net", "DELETE", "/v1/channels/general/nets/nosuch", "", "", operatorRoute(want{http.StatusNotFound, "net_not_found", false})},
		{"put a net member", "PUT", fmt.Sprintf("/v1/channels/general/nets/nosuch/members/%d", f.cand.ID), `{"role":"member"}`, "", operatorRoute(want{http.StatusNotFound, "net_not_found", false})},
		{"remove a net member", "DELETE", fmt.Sprintf("/v1/channels/general/nets/nosuch/members/%d", f.cand.ID), "", "", operatorRoute(want{http.StatusNotFound, "net_not_found", false})},
		{"put a manifest", "PUT", fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID), `{}`, "", operatorRoute(want{http.StatusBadRequest, "invalid_manifest", false})},
		{"get a manifest", "GET", fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID), "", "",
			[7]want{ok, ok, forbidden, forbidden, forbidden, ok, forbidden}},
		{"issue a credential", "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.cand.ID), `{}`, "", operatorRoute(bad)},
		{"list credentials", "GET", fmt.Sprintf("/v1/principals/%d/credentials", f.cand.ID), "", "", operatorRoute(ok)},
		{"rotate a credential", "POST", "/v1/credentials/999999/rotate", "", "", operatorRoute(want{http.StatusNotFound, "credential_not_found", false})},
		{"revoke a credential", "DELETE", "/v1/credentials/999999", "", "", operatorRoute(want{http.StatusNotFound, "credential_not_found", false})},
		{"disable a principal", "POST", "/v1/principals/999999/disable", "", "", operatorRoute(want{http.StatusNotFound, "principal_not_found", false})},
		{"enable a principal", "POST", "/v1/principals/999999/enable", "", "", operatorRoute(want{http.StatusNotFound, "principal_not_found", false})},
		{"hide a principal", "POST", "/v1/principals/999999/hide", "", "", operatorRoute(want{http.StatusNotFound, "principal_not_found", false})},
		{"bulk hide principals", "POST", "/v1/principals/hide", `{}`, "", operatorRoute(bad)},
		{"list principals", "GET", "/v1/principals", "", "", operatorRoute(ok)},
		{"revoke all credentials", "POST", "/v1/principals/999999/credentials/revoke-all", "", "", selfRoute(want{http.StatusNotFound, "principal_not_found", false})},
		{"create a hook", "POST", "/v1/hooks", `{}`, "", operatorRoute(bad)},
		{"list hooks", "GET", "/v1/hooks", "", "", operatorRoute(ok)},
		{"revoke a hook", "DELETE", "/v1/hooks/999999", "", "", operatorRoute(want{http.StatusNotFound, "hook_not_found", false})},
		{"list invite joiners", "GET", "/v1/invites/999999/principals", "", "", operatorRoute(want{http.StatusNotFound, "invite_not_found", false})},
		{"disable invite joiners", "POST", "/v1/invites/999999/disable-joiners", `{}`, "", operatorRoute(want{http.StatusNotFound, "invite_not_found", false})},
		{"disable invitees", "POST", fmt.Sprintf("/v1/principals/%d/disable-invitees", f.cand.ID), `{"dry_run":true}`, "", operatorRoute(ok)},
		{"set invite permission", "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", f.cand.ID), `{}`, "", operatorRoute(want{http.StatusNoContent, "", false})},
		{"get invite settings", "GET", "/v1/invites/settings", "", "", operatorRoute(ok)},
		{"put invite settings", "PUT", "/v1/invites/settings", `{"bogus":true}`, "", operatorRoute(bad)},
		{"create a sign-in link", "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.cand.ID), `{}`, "", operatorRoute(want{http.StatusCreated, "", false})},
		{"close a sign-in link", "DELETE", "/v1/sign-in-links/999999", "", "", operatorRoute(want{http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound, false})},
		{"list reports", "GET", "/v1/reports", "", "", operatorRoute(ok)},
		{"get a report", "GET", "/v1/reports/999999", "", "", operatorRoute(want{http.StatusNotFound, "problem_report_not_found", false})},
		{"resolve a report", "PUT", "/v1/reports/999999", `{}`, "", operatorRoute(bad)},
		{"list another channel's members", "GET", "/v1/channels/lobby/members", "", "/v1/channels/nosuch/members",
			[7]want{ok, ok, ok, hidden, hidden, {http.StatusNotFound, "channel_not_found", true}, hidden}},
	}
	for _, row := range rows {
		for i, c := range callers {
			t.Run(row.name+"/"+c.who, func(t *testing.T) {
				w := row.want[i]
				before := f.deniedRows(t, c.id)
				rec := f.do(t, row.method, row.path, c.token, row.body)
				if rec.Code != w.status {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, w.status, rec.Body)
				}
				if w.code != "" {
					assertErrorBody(t, rec, w.status, w.code)
				}
				if got := f.deniedRows(t, c.id) - before; (got > 0) != w.audited {
					t.Errorf("access_denied rows written = %d, want audited %v", got, w.audited)
				}
				if row.nosuch != "" && w.code == "channel_not_found" {
					ref := f.do(t, row.method, row.nosuch, c.token, row.body)
					if ref.Code != rec.Code || ref.Body.String() != rec.Body.String() {
						t.Errorf("non-member answer differs from an unknown channel's:\n got  %d %s\n want %d %s", rec.Code, rec.Body, ref.Code, ref.Body)
					}
				}
			})
		}
	}
}

// TestOwnerProtection: a non-owner operator acting on an owner is refused
// on every principal-addressed route (403 forbidden, audited) and answered
// as for an id never issued on the id-addressed ones; GET /v1/hooks leaves
// the owner's hooks out. An owner acting on the same owner gets past the
// protection on each.
func TestOwnerProtection(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	target := f.own2
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, target.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateNet(ctx, "system", f.general.ID, "crew", f.root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutNetMember(ctx, "system", f.general.ID, "crew", target.ID, schema.NetRoleMember, f.root.ID); err != nil {
		t.Fatal(err)
	}
	hookRec := f.do(t, "POST", "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"general","principal":%d}`, target.ID))
	if hookRec.Code != http.StatusCreated {
		t.Fatalf("owner creates a hook for an owner = %d %s", hookRec.Code, hookRec.Body)
	}
	hookID := decodeBody[schema.CreateHookResponse](t, hookRec).ID
	// A sign-in link is for a member, so an owner's can only be one made
	// while they were a member; setting the mark closed it, and its id
	// stays.
	linkHolder, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "linked")
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.srv.store.CreateSignInLink(ctx, "system", f.root.ID, linkHolder.ID, schema.CreateSignInLinkRequestV1{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := f.srv.store.SetRole(ctx, "system", f.root.ID, linkHolder.ID, store.RoleOperator, &yes); err != nil {
		t.Fatal(err)
	}

	id := target.ID
	t.Run("store rechecks the owner mark in credential and hook transactions", func(t *testing.T) {
		if _, _, err := f.srv.store.CreateCredential(ctx, "system", id, "owner-race", nil, true); !errors.Is(err, store.ErrOwnerProtected) {
			t.Errorf("CreateCredential = %v, want ErrOwnerProtected", err)
		}
		if _, _, err := f.srv.store.CreateRenewableCredential(ctx, "system", id, "owner-race", time.Hour, true); !errors.Is(err, store.ErrOwnerProtected) {
			t.Errorf("CreateRenewableCredential = %v, want ErrOwnerProtected", err)
		}
		if _, err := f.srv.store.CreateHookWithLabel(ctx, "system", "owner-race-hook", "", f.general.ID, id, true); !errors.Is(err, store.ErrOwnerProtected) {
			t.Errorf("CreateHookWithLabel = %v, want ErrOwnerProtected", err)
		}
	})
	cases := []struct {
		name, method, path, body string
		// notFound is the route's 404 code for an id never issued; empty
		// for a principal-addressed route, which refuses with 403.
		notFound string
	}{
		{"rename", "PUT", fmt.Sprintf("/v1/principals/%d/name", id), `{"name":"renamed-owner"}`, ""},
		{"grant can_decide", "PUT", fmt.Sprintf("/v1/channels/general/members/%d", id), `{"can_decide":false}`, ""},
		{"remove a net member", "DELETE", fmt.Sprintf("/v1/channels/general/nets/crew/members/%d", id), "", ""},
		{"issue a credential", "POST", fmt.Sprintf("/v1/principals/%d/credentials", id), `{"label":"made by a test"}`, ""},
		{"list credentials", "GET", fmt.Sprintf("/v1/principals/%d/credentials", id), "", ""},
		{"rotate a credential", "POST", fmt.Sprintf("/v1/credentials/%d/rotate", f.own2CredID), "", "credential_not_found"},
		{"create a hook", "POST", "/v1/hooks", fmt.Sprintf(`{"channel":"general","principal":%d}`, id), ""},
		{"revoke a hook", "DELETE", fmt.Sprintf("/v1/hooks/%d", hookID), "", "hook_not_found"},
		{"disable invitees", "POST", fmt.Sprintf("/v1/principals/%d/disable-invitees", id), `{"dry_run":true}`, ""},
		{"set invite permission", "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", id), `{"may_invite":true}`, ""},
		{"make a sign-in link", "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", id), `{}`, ""},
		{"close a sign-in link", "DELETE", fmt.Sprintf("/v1/sign-in-links/%d", link.SignInLink.ID), "", schema.ErrorCodeSignInLinkNotFound},
		{"revoke all credentials", "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", id), "", ""},
		{"disable", "POST", fmt.Sprintf("/v1/principals/%d/disable", id), "", ""},
		{"enable", "POST", fmt.Sprintf("/v1/principals/%d/enable", id), "", ""},
		{"remove from the channel", "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", id), "", ""},
		{"revoke a credential", "DELETE", fmt.Sprintf("/v1/credentials/%d", f.own2CredID), "", "credential_not_found"},
		{"hide", "POST", fmt.Sprintf("/v1/principals/%d/hide", id), "", ""},
	}
	// The refusals first, so the owner's successes below find every target
	// as it was.
	for _, c := range cases {
		t.Run("operator/"+c.name, func(t *testing.T) {
			before := f.deniedRows(t, f.op2.ID)
			rec := f.do(t, c.method, c.path, f.op2Tok, c.body)
			if c.notFound == "" {
				assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
				if f.deniedRows(t, f.op2.ID) == before {
					t.Errorf("the refusal was not audited")
				}
				return
			}
			assertErrorBody(t, rec, http.StatusNotFound, c.notFound)
			// Byte for byte the answer for an id never issued.
			idText := fmt.Sprint(lastPathID(c.path))
			at := strings.LastIndex(c.path, "/"+idText)
			if at < 0 {
				t.Fatalf("no numeric id in %s", c.path)
			}
			never := c.path[:at] + "/999999" + c.path[at+len(idText)+1:]
			ref := f.do(t, c.method, never, f.op2Tok, c.body)
			if ref.Code != rec.Code || ref.Body.String() != rec.Body.String() {
				t.Errorf("answer differs from an unknown id's:\n got  %d %s\n want %d %s", rec.Code, rec.Body, ref.Code, ref.Body)
			}
		})
	}
	t.Run("operator/owner check precedes body validation", func(t *testing.T) {
		for _, c := range []struct{ method, path string }{
			{"PUT", fmt.Sprintf("/v1/principals/%d/name", id)},
			{"POST", fmt.Sprintf("/v1/principals/%d/credentials", id)},
			{"POST", fmt.Sprintf("/v1/principals/%d/disable-invitees", id)},
			{"PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", id)},
			{"POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", id)},
		} {
			assertErrorBody(t, f.do(t, c.method, c.path, f.op2Tok, `{`), http.StatusForbidden, "forbidden")
		}
	})
	t.Run("operator/list hooks", func(t *testing.T) {
		for _, h := range decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.op2Tok, "")).Hooks {
			if h.Principal == id {
				t.Errorf("a non-owner operator was shown an owner's hook: %+v", h)
			}
		}
		found := false
		for _, h := range decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.rootTok, "")).Hooks {
			found = found || h.Principal == id
		}
		if !found {
			t.Errorf("the owner was not shown the owner's hook")
		}
	})
	// Nothing the refusals touched changed: the owner still holds its
	// credential, can_decide and name.
	if rec := f.do(t, "GET", "/v1/whoami", f.own2Tok, ""); rec.Code != http.StatusOK || decodeBody[schema.WhoAmIResponseV1](t, rec).Name != "own2" {
		t.Fatalf("the owner after the refusals: %d %s", rec.Code, rec.Body)
	}

	for _, c := range cases {
		t.Run("owner/"+c.name, func(t *testing.T) {
			rec := f.do(t, c.method, c.path, f.rootTok, c.body)
			if c.name == "make a sign-in link" {
				// Past the protection, the route's own rule answers: a
				// sign-in link is for a member.
				if rec.Code == http.StatusForbidden || rec.Code >= 500 {
					t.Fatalf("status = %d %s, want the target rule's answer", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code >= 300 {
				t.Fatalf("status = %d %s, want success", rec.Code, rec.Body)
			}
		})
	}
	target, err = f.srv.store.PrincipalByID(ctx, target.ID)
	if err != nil || target.HiddenAt == nil || !target.IsOwner() {
		t.Fatalf("target is not a hidden owner: %+v, %v", target, err)
	}
	for _, c := range cases {
		t.Run("operator/hidden owner/"+c.name, func(t *testing.T) {
			rec := f.do(t, c.method, c.path, f.op2Tok, c.body)
			if c.notFound == "" {
				assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			} else {
				assertErrorBody(t, rec, http.StatusNotFound, c.notFound)
			}
		})
	}
}

// lastPathID is the last numeric segment of path.
func lastPathID(path string) int64 {
	var id int64
	for _, seg := range strings.Split(path, "/") {
		var n int64
		if _, err := fmt.Sscan(seg, &n); err == nil && fmt.Sprint(n) == seg {
			id = n
		}
	}
	return id
}

// TestSetRoleRoute walks PUT /v1/principals/{id}/role through §6's table.
func TestSetRoleRoute(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	path := func(id int64) string { return fmt.Sprintf("/v1/principals/%d/role", id) }
	set := func(t *testing.T, token string, id int64, body string) *httptestRecorder {
		t.Helper()
		return f.do(t, "PUT", path(id), token, body)
	}

	t.Run("promote a member to operator", func(t *testing.T) {
		rec := set(t, f.rootTok, f.cand.ID, `{"role":"operator"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		resp := decodeBody[schema.RoleChangeResponseV1](t, rec)
		if resp.Principal.Role != schema.RoleOperator || resp.Principal.Owner {
			t.Errorf("principal = %+v, want an operator without the mark", resp.Principal)
		}
		if n := countAction(f.audit(t), store.AuditPrincipalRoleChanged); n == 0 {
			t.Errorf("no principal_role_changed row")
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		before := countAction(f.audit(t), store.AuditPrincipalRoleChanged)
		rec := set(t, f.rootTok, f.cand.ID, `{"role":"operator","owner":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		if resp := decodeBody[schema.RoleChangeResponseV1](t, rec); resp.CredentialsRevoked != 0 || resp.InvitesClosed != 0 {
			t.Errorf("a no-op did something: %+v", resp)
		}
		if after := countAction(f.audit(t), store.AuditPrincipalRoleChanged); after != before {
			t.Errorf("a no-op was audited: %d rows, was %d", after, before)
		}
	})
	t.Run("set the owner mark closes the target's sockets", func(t *testing.T) {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, f.cand.ID, 0); err != nil {
			t.Fatal(err)
		}
		base := wsTestServer(t, f.srv)
		events := dialEvents(t, base, "/v1/events/ws?channel=general", f.candTok)
		rec := set(t, f.rootTok, f.cand.ID, `{"role":"operator","owner":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		resp := decodeBody[schema.RoleChangeResponseV1](t, rec)
		if !resp.Principal.Owner || resp.CredentialsRevoked == 0 || resp.LiveCredentials != 0 {
			t.Errorf("response = %+v, want the mark set and the credentials revoked", resp)
		}
		if ce := events.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
			t.Errorf("socket close = %+v, want revoked-credential policy violation", ce)
		}
		if code := f.do(t, "GET", "/v1/whoami", f.candTok, "").Code; code != http.StatusUnauthorized {
			t.Errorf("the old credential = %d, want 401", code)
		}
	})
	t.Run("clear the owner mark", func(t *testing.T) {
		rec := set(t, f.rootTok, f.cand.ID, `{"role":"operator","owner":false}`)
		if rec.Code != http.StatusOK || decodeBody[schema.RoleChangeResponseV1](t, rec).Principal.Owner {
			t.Fatalf("status = %d %s, want the mark cleared", rec.Code, rec.Body)
		}
	})
	t.Run("demote to member closes their invites", func(t *testing.T) {
		tok, err := newCredentialToken(t, f, f.cand.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.do(t, "POST", "/v1/invites", tok, `{"kind":"open","channels":["general"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("operator invite = %d %s", rec.Code, rec.Body)
		}
		rec := set(t, f.rootTok, f.cand.ID, `{"role":"member"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		if resp := decodeBody[schema.RoleChangeResponseV1](t, rec); resp.Principal.Role != schema.RoleMember || resp.InvitesClosed != 1 {
			t.Errorf("response = %+v, want a member with one invite closed", resp)
		}
	})
	t.Run("last owner", func(t *testing.T) {
		// own2 and root are the owners; with own2's credentials gone, root is
		// the last owner able to sign in.
		if _, err := f.srv.store.RevokeAllCredentials(ctx, "system", f.own2.ID); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{`{"role":"member"}`, `{"role":"operator","owner":false}`} {
			rec := set(t, f.rootTok, f.root.ID, body)
			assertErrorBody(t, rec, http.StatusConflict, "last_operator")
			if e := decodeBody[schema.Error](t, rec); e.Message != lastOperatorMessage {
				t.Errorf("message = %q, want %q", e.Message, lastOperatorMessage)
			}
		}
	})
	t.Run("refused targets and bodies", func(t *testing.T) {
		assertErrorBody(t, set(t, f.rootTok, f.bot.ID, `{"role":"operator"}`), http.StatusBadRequest, "invalid_request")
		assertErrorBody(t, set(t, f.rootTok, f.guest.ID, `{"role":"operator"}`), http.StatusBadRequest, "invalid_request")
		assertErrorBody(t, set(t, f.rootTok, f.alice.ID, `{"role":"member","owner":true}`), http.StatusBadRequest, "invalid_request")
		assertErrorBody(t, set(t, f.rootTok, 999999, `{"role":"operator"}`), http.StatusNotFound, "principal_not_found")
		assertErrorBody(t, set(t, f.rootTok, 999999, `{"role":"bogus"}`), http.StatusNotFound, "principal_not_found")
		vanished, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "vanished-r")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.HidePrincipal(ctx, "system", vanished.ID, store.HideOptions{}); err != nil {
			t.Fatal(err)
		}
		assertErrorBody(t, set(t, f.rootTok, vanished.ID, `{"role":"operator"}`), http.StatusNotFound, "principal_not_found")
		assertErrorBody(t, set(t, f.rootTok, vanished.ID, `{"role":"bogus"}`), http.StatusNotFound, "principal_not_found")
	})
	t.Run("a non-owner operator is refused before anything is read", func(t *testing.T) {
		before := f.deniedRows(t, f.op2.ID)
		assertErrorBody(t, set(t, f.op2Tok, 999999, `{"role":"operator"}`), http.StatusForbidden, "forbidden")
		if f.deniedRows(t, f.op2.ID) == before {
			t.Errorf("the refusal was not audited")
		}
		assertErrorBody(t, set(t, f.botTok, f.alice.ID, `{"role":"operator"}`), http.StatusForbidden, "forbidden")
	})
}

// httptestRecorder names the recorder f.do returns, for the helpers above.
type httptestRecorder = httptest.ResponseRecorder

// newCredentialToken issues a fresh script credential for id.
func newCredentialToken(t *testing.T, f *adminFixture, id int64) (string, error) {
	t.Helper()
	_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", id, "test", nil)
	return tok, err
}

// TestAdministrationViews: whoami's owner, the channel list's manages, and
// the member list's manages, role and can_decide per viewer (§7).
func TestAdministrationViews(t *testing.T) {
	f := newAdminFixture(t)
	t.Run("whoami", func(t *testing.T) {
		for _, c := range []struct {
			who   string
			token string
			owner bool
		}{{"owner", f.rootTok, true}, {"operator", f.op2Tok, false}, {"manager", f.mgrTok, false}, {"member", f.aliceTok, false}} {
			if got := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", c.token, "")).Owner; got != c.owner {
				t.Errorf("%s: owner = %v, want %v", c.who, got, c.owner)
			}
		}
	})
	t.Run("channel list", func(t *testing.T) {
		for _, ch := range decodeBody[schema.ListChannelsResponse](t, f.do(t, "GET", "/v1/channels", f.mgrTok, "")).Channels {
			if want := ch.Name == "general"; ch.Manages != want {
				t.Errorf("manager sees %s manages = %v, want %v", ch.Name, ch.Manages, want)
			}
		}
		for _, ch := range decodeBody[schema.ListChannelsResponse](t, f.do(t, "GET", "/v1/channels", f.aliceTok, "")).Channels {
			if ch.Manages {
				t.Errorf("a member sees manages on %s", ch.Name)
			}
		}
	})
	t.Run("member list", func(t *testing.T) {
		for _, c := range []struct {
			who        string
			id         int64
			token      string
			privileged bool
		}{{"owner", f.root.ID, f.rootTok, true}, {"manager", f.mgr.ID, f.mgrTok, true}, {"member", f.alice.ID, f.aliceTok, false}} {
			members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", c.token, "")).Members
			checkMemberView(t, c.who, c.id, c.privileged, members)
		}
	})
	t.Run("member socket", func(t *testing.T) {
		base := wsTestServer(t, f.srv)
		viewers := []struct {
			who        string
			id         int64
			token      string
			privileged bool
			events     *eventsClient
		}{{"owner", f.root.ID, f.rootTok, true, nil}, {"manager", f.mgr.ID, f.mgrTok, true, nil}, {"member", f.alice.ID, f.aliceTok, false, nil}}
		for i := range viewers {
			viewers[i].events = dialEvents(t, base, "/v1/events/ws?channel=general", viewers[i].token)
			if first := viewers[i].events.next(); first.schema != schema.SessionRosterSchemaV1 {
				t.Fatalf("%s: first frame = %v, want the roster", viewers[i].who, first)
			}
		}
		if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("trigger a member update = %d %s", rec.Code, rec.Body)
		}
		for _, c := range viewers {
			frames := c.events.until("the changed member list", isMembers(func(doc schema.ChannelMembersV1) bool {
				_, ok := listsMember(doc, f.cand.ID)
				return ok
			}))
			doc := frames[len(frames)-1].members
			if doc.ChannelID != f.general.ID {
				t.Fatalf("%s: member document channel = %d, want %d", c.who, doc.ChannelID, f.general.ID)
			}
			checkMemberView(t, c.who+" socket", c.id, c.privileged, doc.Members)
		}
	})
}

// checkMemberView holds a member list to §7.3 for a viewer.
func checkMemberView(t *testing.T, who string, viewerID int64, privileged bool, members []schema.ChannelMemberV1) {
	t.Helper()
	for _, m := range members {
		human := m.Kind == schema.PrincipalHuman
		if human != (m.Manages != nil) {
			t.Errorf("%s sees member %d manages = %v", who, m.PrincipalID, m.Manages)
		}
		wantRole := human && m.Guest == nil && privileged
		if (m.Role != "") != wantRole {
			t.Errorf("%s sees member %d role = %q, want shown %v", who, m.PrincipalID, m.Role, wantRole)
		}
		wantDecide := human && (privileged || m.PrincipalID == viewerID)
		if (m.CanDecide != nil) != wantDecide {
			t.Errorf("%s sees member %d can_decide = %v, want shown %v", who, m.PrincipalID, m.CanDecide, wantDecide)
		}
	}
}

// TestManagerAddRefusals: a manager's add, by id or by name, of anyone it
// may not add is the 404 of a principal never issued, byte for byte; each
// refusal is audited member_add_refused with no name or target; past the
// daily limit the caller is refused before anything is looked up.
func TestManagerAddRefusals(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	st := f.srv.store
	inLobby := func(name string, kind store.PrincipalKind) store.Principal {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChannelMember(ctx, "system", f.lobby.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		return p
	}
	vanished := inLobby("vanished-a", store.PrincipalHuman)
	if _, err := st.HidePrincipal(ctx, "system", vanished.ID, store.HideOptions{}); err != nil {
		t.Fatal(err)
	}
	disabled := inLobby("disabled-a", store.PrincipalHuman)
	if _, err := st.DisablePrincipal(ctx, "system", disabled.ID); err != nil {
		t.Fatal(err)
	}
	agent := inLobby("agent-a", store.PrincipalAgent)
	guest := newGuest(t, f.srv, "lobby-visitor", time.Hour, 2*time.Hour, f.lobby.ID).Principal
	if _, err := st.AddChannelMember(ctx, "system", f.lobby.ID, f.op2.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChannelMember(ctx, "system", f.lobby.ID, f.own2.ID, 0); err != nil {
		t.Fatal(err)
	}
	targets := []struct {
		name string
		p    store.Principal
	}{
		{"hidden", vanished}, {"disabled", disabled}, {"agent", agent}, {"guest", guest},
		{"operator", f.op2}, {"owner", f.own2}, {"member sharing no channel", f.outsider},
	}

	refusedRows := func() []store.AuditEvent { return auditRows(t, f.authFixture, store.AuditMemberAddRefused) }
	byID := func(id int64) *httptestRecorder {
		return f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", id), f.mgrTok, "")
	}
	byName := func(name string) *httptestRecorder {
		return f.do(t, "POST", "/v1/channels/general/members", f.mgrTok, fmt.Sprintf(`{"name":%q}`, name))
	}
	refID := byID(999999)
	refName := byName("nobody-here")
	assertErrorBody(t, refID, http.StatusNotFound, "principal_not_found")
	assertErrorBody(t, refName, http.StatusNotFound, "principal_not_found")
	refused := 2

	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			if rec := byID(tg.p.ID); rec.Code != refID.Code || rec.Body.String() != refID.Body.String() {
				t.Errorf("by id: %d %s, want %d %s", rec.Code, rec.Body, refID.Code, refID.Body)
			}
			if rec := byName(tg.p.Name); rec.Code != refName.Code || rec.Body.String() != refName.Body.String() {
				t.Errorf("by name: %d %s, want %d %s", rec.Code, rec.Body, refName.Code, refName.Body)
			}
		})
		refused += 2
	}
	rows := refusedRows()
	if len(rows) != refused {
		t.Fatalf("member_add_refused rows = %d, want %d", len(rows), refused)
	}
	for _, e := range rows {
		if e.Actor != fmt.Sprintf("principal:%d", f.mgr.ID) || e.Subject != fmt.Sprintf("channel:%d", f.general.ID) || e.Detail != "" {
			t.Errorf("row %+v names more than the caller and the channel", e)
		}
	}
	// Nobody was added.
	for _, tg := range targets {
		if in, err := st.IsChannelMember(ctx, f.general.ID, tg.p.ID); err != nil || in {
			t.Errorf("%s is in general: %v %v", tg.name, in, err)
		}
	}
	// A manager sending can_decide or manages is refused, but only after an
	// addable target is found; an unknown target keeps its uniform 404.
	for _, body := range []string{`{"can_decide":true}`, `{"manages":true}`} {
		assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), f.mgrTok, body), http.StatusForbidden, "forbidden")
	}
	if rec := f.do(t, "PUT", "/v1/channels/general/members/999999", f.mgrTok, `{"can_decide":true}`); rec.Code != refID.Code || rec.Body.String() != refID.Body.String() {
		t.Errorf("unknown target with can_decide = %d %s, want %d %s", rec.Code, rec.Body, refID.Code, refID.Body)
	}
	refused++
	for _, targetID := range []int64{999999, vanished.ID} {
		if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", targetID), f.mgrTok, `{`); rec.Code != refID.Code || rec.Body.String() != refID.Body.String() {
			t.Errorf("unaddable target %d with malformed body = %d %s, want %d %s", targetID, rec.Code, rec.Body, refID.Code, refID.Body)
		}
		refused++
	}
	// The limit: refusals up to renameAttemptsPerDay, then 429 for anything,
	// with nothing more looked up or audited.
	for refused < renameAttemptsPerDay {
		byID(999999)
		refused++
	}
	for _, rec := range []*httptestRecorder{byID(f.cand.ID), byName(f.cand.Name)} {
		assertErrorBody(t, rec, http.StatusTooManyRequests, "rate_limited")
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("no Retry-After")
		}
	}
	if n := len(refusedRows()); n != renameAttemptsPerDay {
		t.Errorf("member_add_refused rows = %d after the limit, want %d", n, renameAttemptsPerDay)
	}
	if in, _ := st.IsChannelMember(ctx, f.general.ID, f.cand.ID); in {
		t.Errorf("cand was added past the limit")
	}
}

func TestMemberAddRefusalLimitIsAtomic(t *testing.T) {
	f := newAdminFixture(t)
	attempts := renameAttemptsPerDay + 10
	results := make(chan error, attempts)
	now := f.srv.now()
	for range attempts {
		go func() {
			results <- f.srv.store.RecordMemberAddRefused(context.Background(), f.mgr.ID, f.general.ID, renameAttemptsPerDay, now)
		}()
	}
	accepted := 0
	for range attempts {
		if err := <-results; err == nil {
			accepted++
		} else {
			var limited *store.RenameLimitedError
			if !errors.As(err, &limited) {
				t.Errorf("concurrent refusal = %v, want the daily limit", err)
			}
		}
	}
	if accepted != renameAttemptsPerDay {
		t.Errorf("accepted refusals = %d, want %d", accepted, renameAttemptsPerDay)
	}
	if rows := auditRows(t, f.authFixture, store.AuditMemberAddRefused); len(rows) != renameAttemptsPerDay {
		t.Errorf("audit rows = %d, want %d", len(rows), renameAttemptsPerDay)
	}
}

func TestManagerBrowserSessionNeedsNoReauth(t *testing.T) {
	f := newAdminFixture(t)
	_, session, err := f.srv.store.CreateRenewableCredential(context.Background(), "system", f.mgr.ID, "browser", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID)
	if rec := f.do(t, "PUT", path, session, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("manager browser PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "DELETE", path, f.mgrTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup DELETE = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "POST", "/v1/channels/general/members", session, fmt.Sprintf(`{"name":%q}`, f.cand.Name)); rec.Code != http.StatusCreated {
		t.Fatalf("manager browser POST = %d %s", rec.Code, rec.Body)
	}
}

func TestOperatorAddRefusalsAreNotManagerLimited(t *testing.T) {
	f := newAdminFixture(t)
	for i := 0; i <= renameAttemptsPerDay; i++ {
		assertErrorBody(t, f.do(t, "PUT", "/v1/channels/general/members/999999", f.op2Tok, ""), http.StatusNotFound, "principal_not_found")
		assertErrorBody(t, f.do(t, "POST", "/v1/channels/general/members", f.op2Tok, `{"name":"nobody-here"}`), http.StatusNotFound, "principal_not_found")
	}
	if rows := auditRows(t, f.authFixture, store.AuditMemberAddRefused); len(rows) != 0 {
		t.Errorf("operator refusals wrote manager audit rows: %+v", rows)
	}
}

// TestManagerAddsAndRemoves: the manager's own successes, by id and by
// name, and its refused removal of an operator.
func TestManagerAddsAndRemoves(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), f.mgrTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("manager adds by id = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), f.mgrTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("manager removes = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, "POST", "/v1/channels/general/members", f.mgrTok, fmt.Sprintf(`{"name":%q}`, f.cand.Name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("manager adds by name = %d %s", rec.Code, rec.Body)
	}
	doc := decodeBody[schema.ChannelMemberV1](t, rec)
	if doc.PrincipalID != f.cand.ID || doc.CanDecide == nil || *doc.CanDecide || doc.Manages == nil || *doc.Manages {
		t.Errorf("added = %+v, want cand without can_decide or manages", doc)
	}
	if rec := f.do(t, "POST", "/v1/channels/general/members", f.mgrTok, fmt.Sprintf(`{"name":%q}`, f.cand.Name)); rec.Code != http.StatusOK {
		t.Errorf("adding again = %d %s, want 200", rec.Code, rec.Body)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, f.op2.ID, 0); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.op2.ID), f.mgrTok, ""), http.StatusForbidden, "forbidden")
	// An operator sets manages, audited.
	before := countAction(f.audit(t), store.AuditMemberManageGranted)
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.cand.ID), f.rootTok, `{"manages":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("operator sets manages = %d %s", rec.Code, rec.Body)
	}
	if countAction(f.audit(t), store.AuditMemberManageGranted) == before {
		t.Errorf("setting manages was not audited")
	}
}

// TestManagerStopsAShare: a manager of the channel stops someone else's
// screen share, including without voice access; a plain member who is not the
// sharer is still refused.
func TestManagerStopsAShare(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	f.publish(room, "ann", true, false)
	if res := f.stopShare(t, "ann2", "ops"); res.status != http.StatusForbidden {
		t.Fatalf("a member stops a share = %d %s, want 403", res.status, res.body)
	}
	no, yes := false, true
	if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann2"], 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if res := f.stopShare(t, "ann2", "ops"); res.status != http.StatusNoContent {
		t.Fatalf("a manager stops a share = %d %s, want 204", res.status, res.body)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("still sharing: %s", got)
	}

	f.mustClaim(t, "ann")
	f.joinWithoutVoice(t)
	if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, f.ids["vic"], 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if res := f.stopShare(t, "vic", "ops"); res.status != http.StatusNoContent {
		t.Fatalf("a manager without voice stops a share = %d %s, want 204", res.status, res.body)
	}
}
