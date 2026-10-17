package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Signing out (issue #439): DELETE /v1/credentials/current revokes the
// credential the request came with, and revoke-all is open to the principal
// itself.

const currentCredentialPath = "/v1/credentials/current"

// selfRevokedRows returns the credential_revoked audit rows with reason=self.
func selfRevokedRows(t *testing.T, f *authFixture) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "credential_revoked" && strings.HasSuffix(e.Detail, " reason=self") {
			out = append(out, e)
		}
	}
	return out
}

func TestRevokeCurrentCredential(t *testing.T) {
	// Each signs out with the fixture's credential and keeps a second one.
	tests := []struct {
		name string
		who  func(f *authFixture) store.Principal
		tok  func(f *authFixture) string
	}{
		{"member", func(f *authFixture) store.Principal { return f.alice }, func(f *authFixture) string { return f.aliceTok }},
		{"agent", func(f *authFixture) store.Principal { return f.bot }, func(f *authFixture) string { return f.botTok }},
		{"operator with a second credential", func(f *authFixture) store.Principal { return f.root }, func(f *authFixture) string { return f.rootTok }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			who := tt.who(f)
			otherID, other := f.newCred(t, who.ID)
			signingOut := tt.tok(f)

			rec := f.do(t, "DELETE", currentCredentialPath, signingOut, "")
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("sign out = %d %q, want 204 and no body", rec.Code, rec.Body)
			}
			// That credential's next request is 401; the person's other one
			// still works.
			assertErrorBody(t, f.do(t, "GET", "/v1/whoami", signingOut, ""), http.StatusUnauthorized, "unauthenticated")
			if r := f.do(t, "GET", "/v1/whoami", other, ""); r.Code != http.StatusOK {
				t.Errorf("the other credential after sign out = %d", r.Code)
			}
			list, err := f.srv.store.ListCredentials(context.Background(), who.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range list {
				if (c.ID == otherID) != (c.RevokedAt == nil) {
					t.Errorf("credential %d revoked_at = %v; only the one signed out may be revoked", c.ID, c.RevokedAt)
				}
			}
			rows := selfRevokedRows(t, f)
			if len(rows) != 1 || rows[0].Actor != fmt.Sprintf("principal:%d", who.ID) || rows[0].Subject != fmt.Sprintf("principal:%d", who.ID) {
				t.Errorf("reason=self audit rows = %+v, want one by and about principal %d", rows, who.ID)
			}
			// Signed out is signed out: a repeat is a 401, not a second revoke.
			assertErrorBody(t, f.do(t, "DELETE", currentCredentialPath, signingOut, ""), http.StatusUnauthorized, "unauthenticated")
			if n := len(selfRevokedRows(t, f)); n != 1 {
				t.Errorf("reason=self rows after a repeat = %d, want 1", n)
			}
		})
	}
}

// The credential revoked is the one the request authenticated with, whatever
// the request says: there is no id in the path, and a body is not read.
func TestRevokeCurrentCredentialIgnoresTheRequest(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	rootCreds, err := f.srv.store.ListCredentials(context.Background(), f.root.ID)
	if err != nil || len(rootCreds) != 1 {
		t.Fatalf("root credentials = %+v, %v", rootCreds, err)
	}
	body := fmt.Sprintf(`{"id":%d,"credential_id":%d,"principal_id":%d}`, rootCreds[0].ID, rootCreds[0].ID, f.root.ID)
	if rec := f.do(t, "DELETE", currentCredentialPath+fmt.Sprintf("?id=%d", rootCreds[0].ID), f.aliceTok, body); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
	}
	if r := f.do(t, "GET", "/v1/whoami", f.rootTok, ""); r.Code != http.StatusOK {
		t.Errorf("root after alice signed out = %d; only alice's credential may go", r.Code)
	}
	assertErrorBody(t, f.do(t, "GET", "/v1/whoami", f.aliceTok, ""), http.StatusUnauthorized, "unauthenticated")
}

// The last operator credential cannot sign itself out, as it cannot be
// revoked any other way: nobody could sign in afterwards.
func TestRevokeCurrentCredentialKeepsTheLastOperator(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	assertErrorBody(t, f.do(t, "DELETE", currentCredentialPath, f.rootTok, ""), http.StatusConflict, "last_operator")
	if r := f.do(t, "GET", "/v1/whoami", f.rootTok, ""); r.Code != http.StatusOK {
		t.Fatalf("operator after a refused sign out = %d", r.Code)
	}
	if n := len(selfRevokedRows(t, f)); n != 0 {
		t.Errorf("reason=self rows = %d, want 0", n)
	}
}

// Sign out everywhere: revoke-all is for operators and for the principal
// itself. A member acting on someone else is refused and audited, and the
// last operator guard holds for a self revoke-all too.
func TestRevokeAllCredentialsSelf(t *testing.T) {
	tests := []struct {
		name   string
		caller func(f *authFixture) (store.Principal, string)
		target func(f *authFixture) store.Principal
		want   int
		code   string
	}{
		{"member on themselves", func(f *authFixture) (store.Principal, string) { return f.alice, f.aliceTok }, func(f *authFixture) store.Principal { return f.alice }, http.StatusOK, ""},
		{"agent on itself", func(f *authFixture) (store.Principal, string) { return f.bot, f.botTok }, func(f *authFixture) store.Principal { return f.bot }, http.StatusOK, ""},
		{"operator on a member", func(f *authFixture) (store.Principal, string) { return f.root, f.rootTok }, func(f *authFixture) store.Principal { return f.alice }, http.StatusOK, ""},
		{"member on another member", func(f *authFixture) (store.Principal, string) { return f.alice, f.aliceTok }, func(f *authFixture) store.Principal { return f.bot }, http.StatusForbidden, "forbidden"},
		{"member on the operator", func(f *authFixture) (store.Principal, string) { return f.alice, f.aliceTok }, func(f *authFixture) store.Principal { return f.root }, http.StatusForbidden, "forbidden"},
		{"the last operator on themselves", func(f *authFixture) (store.Principal, string) { return f.root, f.rootTok }, func(f *authFixture) store.Principal { return f.root }, http.StatusConflict, "last_operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			caller, callerTok := tt.caller(f)
			target := tt.target(f)
			// The target holds two credentials: "their other browsers".
			_, second := f.newCred(t, target.ID)
			targetToks := []string{second}
			switch target.ID {
			case f.alice.ID:
				targetToks = append(targetToks, f.aliceTok)
			case f.bot.ID:
				targetToks = append(targetToks, f.botTok)
			case f.root.ID:
				targetToks = append(targetToks, f.rootTok)
			}
			deniedBefore := countAction(f.audit(t), "access_denied")

			rec := f.do(t, "POST", f.revokeAllPath(target.ID), callerTok, "")
			if tt.code != "" {
				assertErrorBody(t, rec, tt.want, tt.code)
			} else if rec.Code != tt.want {
				t.Fatalf("revoke-all = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}

			denied := countAction(f.audit(t), "access_denied") - deniedBefore
			if wantDenied := map[bool]int{true: 1, false: 0}[tt.want == http.StatusForbidden]; denied != wantDenied {
				t.Errorf("access_denied rows written = %d, want %d", denied, wantDenied)
			}
			for i, tok := range targetToks {
				r := f.do(t, "GET", "/v1/whoami", tok, "")
				if tt.want == http.StatusOK && r.Code != http.StatusUnauthorized {
					t.Errorf("target credential %d after revoke-all = %d, want 401", i, r.Code)
				}
				if tt.want != http.StatusOK && r.Code != http.StatusOK {
					t.Errorf("target credential %d after a refused revoke-all = %d, want 200", i, r.Code)
				}
			}
			if tt.want == http.StatusOK {
				got := decodeBody[schema.RevokeAllCredentialsResponseV1](t, rec)
				if got.Revoked != 2 {
					t.Errorf("revoked = %d, want 2", got.Revoked)
				}
				var all []store.AuditEvent
				for _, e := range f.audit(t) {
					if e.Action == "credentials_revoked_all" {
						all = append(all, e)
					}
				}
				if len(all) != 1 || all[0].Actor != fmt.Sprintf("principal:%d", caller.ID) || all[0].Subject != fmt.Sprintf("principal:%d", target.ID) {
					t.Errorf("credentials_revoked_all rows = %+v", all)
				}
			}
		})
	}
}

// A renewable credential is issued over REST with renew_lifetime_seconds and
// says so; the request is checked as the schema says.
func TestCreateRenewableCredentialOverREST(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	path := fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID)
	tests := []struct {
		name string
		body string
		want int
	}{
		{"renewable", `{"label":"browser","renew_lifetime_seconds":7776000}`, http.StatusCreated},
		{"too short to renew", `{"label":"browser","renew_lifetime_seconds":86400}`, http.StatusBadRequest},
		{"with a fixed expiry too", `{"label":"browser","renew_lifetime_seconds":7776000,"expires_at":"2099-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"fixed", `{"label":"laptop"}`, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "POST", path, f.rootTok, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("create = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if tt.want != http.StatusCreated {
				assertErrorBody(t, rec, tt.want, "invalid_request")
				return
			}
			got := decodeBody[schema.CreateCredentialResponseV1](t, rec)
			renewable := strings.Contains(tt.body, "renew_lifetime_seconds")
			if got.Credential.Renews != renewable || (got.Credential.ExpiresAt != nil) != renewable {
				t.Errorf("credential = %+v, want renews and an expiry = %v", got.Credential, renewable)
			}
			if r := f.do(t, "GET", "/v1/whoami", got.Token, ""); r.Code != http.StatusOK {
				t.Errorf("new credential whoami = %d", r.Code)
			}
		})
	}
}
