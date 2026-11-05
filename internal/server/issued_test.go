package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// What an operator issued to others, on demotion, on disable and on the
// owner's revoke-all of it (issue #1024, ADR-014 O13).

func issuedRevokeAllPath(id int64) string {
	return fmt.Sprintf("/v1/principals/%d/issued/revoke-all", id)
}

// seedIssuedByOperator has operator-1 issue a credential to member-1, one
// to itself and a hook for member-2 in channel-a, and the owner a
// credential to member-2 (never listed). It returns the credential issued
// to member-1 (with its token), and the hook.
func seedIssuedByOperator(t *testing.T, f *restrictedFixture) (schema.CredentialV1, string, store.Hook) {
	t.Helper()
	ctx := context.Background()
	opActor := fmt.Sprintf("principal:%d", f.op.ID)
	toMember, tok, err := f.srv.store.CreateCredential(ctx, opActor, f.member.ID, "made by operator-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.store.CreateCredential(ctx, opActor, f.op.ID, "its own", nil); err != nil {
		t.Fatal(err)
	}
	hook, err := f.srv.store.CreateHook(ctx, opActor, "issued-test-hook-"+t.Name(), f.chanA.ID, f.member2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.store.CreateCredential(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.member2.ID, "the owner's", nil); err != nil {
		t.Fatal(err)
	}
	return toMember, tok, hook
}

// wantIssued checks a list is exactly operator-1's credential for member-1
// and its hook for member-2, in that order.
func wantIssued(t *testing.T, got []schema.IssuedByOperatorV1, f *restrictedFixture, cred schema.CredentialV1, hook store.Hook) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("issued_to_others = %+v, want operator-1's credential %d and hook %d", got, cred.ID, hook.ID)
	}
	if c := got[0]; c.Kind != schema.IssuedCredential || c.ID != cred.ID || c.PrincipalID != f.member.ID || c.IssuedBy != f.op.ID || !c.IssuedAt.Time().Equal(cred.CreatedAt.Time()) {
		t.Errorf("item 0 = %+v, want credential %d for member-1 by operator-1 at %v", c, cred.ID, cred.CreatedAt)
	}
	if h := got[1]; h.Kind != schema.IssuedHook || h.ID != hook.ID || h.PrincipalID != f.member2.ID || h.IssuedBy != f.op.ID {
		t.Errorf("item 1 = %+v, want hook %d for member-2 by operator-1", h, hook.ID)
	}
}

// Demoting an operator answers with what it issued to others; a promotion,
// an owner-mark change and a demotion with nothing to list answer without
// the field.
func TestSetRoleDemotionListsIssuedToOthers(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	cred, tok, hook := seedIssuedByOperator(t, f)
	path := fmt.Sprintf("/v1/principals/%d/role", f.op.ID)

	rec := f.do(t, "PUT", path, f.rootTok, `{"role":"member"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.RoleChangeResponseV1](t, rec)
	if resp.Principal.Role != schema.RoleMember {
		t.Fatalf("principal = %+v, want a member", resp.Principal)
	}
	wantIssued(t, resp.IssuedToOthers, f, cred, hook)
	// Demotion ended none of it.
	if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusOK {
		t.Errorf("member-1's credential after the demotion = %d, want 200", code)
	}

	// A promotion lists nothing, and neither does setting the mark.
	for _, body := range []string{`{"role":"operator"}`, `{"role":"operator","owner":true}`} {
		rec = f.do(t, "PUT", path, f.rootTok, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "issued_to_others") {
			t.Errorf("%s answered issued_to_others: %s", body, rec.Body)
		}
	}
	// Now an owner: its issues are never listed, so clearing the mark and
	// demoting answers without the field (the mark revoked nothing of
	// member-1's: the credential is member-1's, not the operator's).
	rec = f.do(t, "PUT", path, f.rootTok, `{"role":"member"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote an owner = %d %s", rec.Code, rec.Body)
	}
	// Once a member, operator-1 is no owner, so what it issued is listed
	// again: the demotion reads after the role write.
	resp = decodeBody[schema.RoleChangeResponseV1](t, rec)
	wantIssued(t, resp.IssuedToOthers, f, cred, hook)

	// With everything revoked, a demotion has nothing to list.
	if _, err := f.srv.store.SetRole(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.op.ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, "PUT", path, f.rootTok, `{"role":"member"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "issued_to_others") {
		t.Errorf("a demotion with nothing issued = %d %s, want 200 without the field", rec.Code, rec.Body)
	}
}

// Disabling a principal answers 200 with what it issued to others; a repeat,
// and a disable of someone who issued nothing, answer an empty body.
func TestDisableListsIssuedToOthers(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	cred, tok, hook := seedIssuedByOperator(t, f)

	rec := f.do(t, "POST", f.disablePath(f.op.ID), f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.DisablePrincipalResponseV1](t, rec)
	wantIssued(t, resp.IssuedToOthers, f, cred, hook)
	// The operator's own credential is gone with it; member-1's stands.
	if code := f.do(t, "GET", "/v1/whoami", f.opTok, "").Code; code != http.StatusUnauthorized {
		t.Errorf("operator-1's credential after the disable = %d, want 401", code)
	}
	if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusOK {
		t.Errorf("member-1's credential after the disable = %d, want 200", code)
	}

	// A repeat disables nothing and lists nothing.
	rec = f.do(t, "POST", f.disablePath(f.op.ID), f.rootTok, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("a repeat = %d %q, want 200 {}", rec.Code, rec.Body)
	}
	// Someone who issued nothing.
	rec = f.do(t, "POST", f.disablePath(f.member2.ID), f.rootTok, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("disable of member-2 = %d %q, want 200 {}", rec.Code, rec.Body)
	}
	// Disabled, the operator's issues are still the owner's to revoke.
	rec = f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-all after the disable = %d %s", rec.Code, rec.Body)
	}
	if res := decodeBody[schema.RevokeIssuedResponseV1](t, rec); res.CredentialsRevoked != 1 || res.HooksRevoked != 1 {
		t.Errorf("revoke-all = %+v, want 1 and 1", res)
	}
	if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusUnauthorized {
		t.Errorf("member-1's credential after the revoke-all = %d, want 401", code)
	}
}

// POST /v1/principals/{id}/issued/revoke-all is the owner's alone, refuses
// an owner as {id} and an unknown id, revokes exactly what {id} issued to
// others (sockets included), leaves its own and others' issues, and a
// repeat revokes nothing.
func TestRevokeIssuedRoute(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	cred, tok, hook := seedIssuedByOperator(t, f)
	ctx := context.Background()

	t.Run("owner only", func(t *testing.T) {
		for _, c := range []struct {
			who string
			tok string
		}{{"operator-1", f.opTok}, {"member-1", f.memberTok}, {"agent-1", f.agTok}} {
			rec := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), c.tok, "")
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s = %d %s, want 403", c.who, rec.Code, rec.Body)
			}
		}
		if code := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), "", "").Code; code != http.StatusUnauthorized {
			t.Errorf("no credential = %d, want 401", code)
		}
		if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusOK {
			t.Errorf("member-1's credential after the refusals = %d, want 200", code)
		}
	})
	t.Run("an unknown principal", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "POST", issuedRevokeAllPath(999999), f.rootTok, ""), http.StatusNotFound, "principal_not_found")
	})
	t.Run("an owner is protected", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "POST", issuedRevokeAllPath(f.root.ID), f.rootTok, ""), http.StatusForbidden, "forbidden")
		// An operator that is itself an owner, too.
		yes := true
		if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.op.ID, store.RoleOperator, &yes); err != nil {
			t.Fatal(err)
		}
		assertErrorBody(t, f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, ""), http.StatusForbidden, "forbidden")
		no := false
		if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.op.ID, store.RoleOperator, &no); err != nil {
			t.Fatal(err)
		}
		// The mark revoked operator-1's own credentials (ADR-010 D1), not
		// what it issued; give it a new one for the rest.
		var err error
		if _, f.opTok, err = f.srv.store.CreateCredential(ctx, "system", f.op.ID, "after the mark", nil); err != nil {
			t.Fatal(err)
		}
		if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusOK {
			t.Errorf("member-1's credential after the refusals = %d, want 200", code)
		}
	})
	t.Run("the owner revokes it all", func(t *testing.T) {
		// member-1 is in channel-b: a socket on operator-1's credential
		// closes with the revocation.
		base := wsTestServer(t, f.srv)
		events := dialEvents(t, base, "/v1/events/ws?channel=channel-b", tok)
		before := countAction(f.audit(t), store.AuditIssuedRevokedAll)

		rec := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
		}
		if res := decodeBody[schema.RevokeIssuedResponseV1](t, rec); res.CredentialsRevoked != 1 || res.HooksRevoked != 1 {
			t.Errorf("revoke-all = %+v, want 1 credential and 1 hook", res)
		}
		if ce := events.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
			t.Errorf("socket close = %+v, want the revoked-credential policy violation", ce)
		}
		if code := f.do(t, "GET", "/v1/whoami", tok, "").Code; code != http.StatusUnauthorized {
			t.Errorf("member-1's credential by operator-1 = %d, want 401", code)
		}
		// operator-1's own credential, and what the owner issued, stand.
		if code := f.do(t, "GET", "/v1/whoami", f.opTok, "").Code; code != http.StatusOK {
			t.Errorf("operator-1's own credential = %d, want 200", code)
		}
		live, err := f.srv.store.ListLiveCredentials(ctx, f.member2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(live); n != 2 {
			t.Errorf("member-2 holds %d live credentials, want its fixture one and the owner's", n)
		}
		if _, err := f.srv.store.HookByToken(ctx, "issued-test-hook-"+t.Name()); err == nil {
			t.Errorf("the hook still resolves after the revoke")
		}
		audit := f.audit(t)
		if n := countAction(audit, store.AuditIssuedRevokedAll); n != before+1 {
			t.Errorf("issued_revoked_all rows = %d, want %d", n, before+1)
		}
		var revokedCred, revokedHook bool
		for _, e := range audit {
			if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) || !strings.HasSuffix(e.Detail, " reason=issued revoke-all") {
				continue
			}
			switch e.Action {
			case "credential_revoked":
				revokedCred = revokedCred || strings.HasPrefix(e.Detail, fmt.Sprintf("credential=%d ", cred.ID))
			case "hook_revoked":
				revokedHook = revokedHook || strings.HasPrefix(e.Detail, fmt.Sprintf("hook=%d ", hook.ID))
			}
		}
		if !revokedCred || !revokedHook {
			t.Errorf("audit rows for the credential (%v) and the hook (%v), attributed to the owner with the reason", revokedCred, revokedHook)
		}
	})
	t.Run("a repeat revokes nothing", func(t *testing.T) {
		rec := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("repeat = %d %s", rec.Code, rec.Body)
		}
		if res := decodeBody[schema.RevokeIssuedResponseV1](t, rec); res.CredentialsRevoked != 0 || res.HooksRevoked != 0 {
			t.Errorf("repeat = %+v, want zeros", res)
		}
	})
}

// The PUT that turns act_as_members off still lists what operators issued,
// now from the columns, and the owner's revoke-all per issuer clears it.
func TestPermissionsPutIssuedToOthersFromColumns(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	cred, _, hook := seedIssuedByOperator(t, f)

	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false}}})
	if res.code != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.code, res.body)
	}
	wantIssued(t, res.document(t).IssuedToOthers, f, cred, hook)
	if rec := f.do(t, "POST", issuedRevokeAllPath(f.op.ID), f.rootTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
	}
	got, err := f.srv.store.IssuedToOthers(context.Background(), f.srv.now())
	if err != nil || len(got) != 0 {
		t.Errorf("after the revoke-all, IssuedToOthers = %+v, %v; want none", got, err)
	}
}
