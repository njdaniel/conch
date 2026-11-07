package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// issuedFixture is two things principal 7 issued to others.
func issuedFixture() []schema.IssuedByOperatorV1 {
	at := schema.NewTimestamp(time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC))
	return []schema.IssuedByOperatorV1{
		{Kind: schema.IssuedCredential, ID: 11, PrincipalID: 21, IssuedBy: 7, IssuedAt: at},
		{Kind: schema.IssuedHook, ID: 12, PrincipalID: 22, IssuedBy: 7, IssuedAt: at},
	}
}

// issuedFake answers the three routes of issue #1463 with handler's choice.
func issuedFake(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

const issuedTail = "the owner may end them with: conch admin principal revoke-issued 7 --yes\n"

func TestAdminIssuedToOthers(t *testing.T) {
	list := "2 issued to others and still live:\n" +
		"  credential 11 held by principal 21 issued 2026-11-02T09:30:00.000Z\n" +
		"  hook 12 held by principal 22 issued 2026-11-02T09:30:00.000Z\n" + issuedTail
	tests := []struct {
		name    string
		args    []string
		handler http.HandlerFunc
		want    string
		wantErr string
		calls   int32
	}{
		{"disable lists", []string{"principal", "disable", "7"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.DisablePrincipalResponseV1{IssuedToOthers: issuedFixture()})
		}, "disabled principal 7\n" + list, "", 1},
		{"disable with none", []string{"principal", "disable", "7"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.DisablePrincipalResponseV1{})
		}, "disabled principal 7\n", "", 1},
		{"disable, older server 204", []string{"principal", "disable", "7"}, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, "disabled principal 7\n", "", 1},
		{"disable --json", []string{"principal", "disable", "7", "--json"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.DisablePrincipalResponseV1{IssuedToOthers: issuedFixture()})
		}, `{"issued_to_others":[{"kind":"credential","id":11,"principal_id":21,"issued_by":7,"issued_at":"2026-11-02T09:30:00.000Z"},{"kind":"hook","id":12,"principal_id":22,"issued_by":7,"issued_at":"2026-11-02T09:30:00.000Z"}]}` + "\n", "", 1},
		{"role lists", []string{"principal", "role", "7", "member"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.RoleChangeResponseV1{Principal: schema.PrincipalAdminV1{ID: 7, Role: schema.RoleMember}, IssuedToOthers: issuedFixture()})
		}, "principal 7 role member owner false manages - invites closed 0 credentials revoked 0 hooks revoked 0 live credentials 0 hooks bound 0\n" + list, "", 1},
		{"role without a list", []string{"principal", "role", "7", "member"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.RoleChangeResponseV1{Principal: schema.PrincipalAdminV1{ID: 7, Role: schema.RoleMember}})
		}, "principal 7 role member owner false manages - invites closed 0 credentials revoked 0 hooks revoked 0 live credentials 0 hooks bound 0\n", "", 1},
		{"revoke-issued dry run", []string{"principal", "revoke-issued", "7"}, func(http.ResponseWriter, *http.Request) {},
			"would revoke every live credential and hook principal 7 issued to others; run again with --yes to do it\n", "", 0},
		{"revoke-issued --yes", []string{"principal", "revoke-issued", "7", "--yes"}, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/principals/7/issued/revoke-all" {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, schema.RevokeIssuedResponseV1{CredentialsRevoked: 3, HooksRevoked: 1})
		}, "revoked 3 credential(s) and 1 hook(s)\n", "", 1},
		{"revoke-issued --yes --json", []string{"principal", "revoke-issued", "7", "--yes", "--json"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.RevokeIssuedResponseV1{CredentialsRevoked: 3, HooksRevoked: 1})
		}, `{"credentials_revoked":3,"hooks_revoked":1}` + "\n", "", 1},
		{"revoke-issued, not the owner", []string{"principal", "revoke-issued", "7", "--yes"}, func(w http.ResponseWriter, _ *http.Request) {
			writeAPIError(w, http.StatusForbidden, "forbidden")
		}, "", "only an owner may revoke what someone issued to others", 1},
		{"revoke-issued, no password to give", []string{"principal", "revoke-issued", "7", "--yes"}, func(w http.ResponseWriter, _ *http.Request) {
			writeAPIError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired)
		}, "", "this needs your password first", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			fake, calls := issuedFake(t, tt.handler)
			stdout, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("run: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(ErrorLine(err), tt.wantErr) || strings.Count(ErrorLine(err), "\n") > 0) {
				t.Fatalf("error = %v, want one line with %q", err, tt.wantErr)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
			if got := calls.Load(); got != tt.calls {
				t.Errorf("%d requests, want %d", got, tt.calls)
			}
		})
	}
}

// A revoke-issued that is asked for a proof asks for the password, then sends
// the request again.
func TestAdminRevokeIssuedReauth(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	var posts, proofs atomic.Int32
	proved := atomic.Bool{}
	fake, _ := issuedFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			writeJSON(w, schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "owner", Role: schema.RoleOperator, Owner: true})
		case "POST /v1/reauth":
			proofs.Add(1)
			proved.Store(true)
			writeJSON(w, schema.ReauthResponseV1{Until: schema.NewTimestamp(time.Now().Add(time.Hour))})
		case "POST /v1/principals/7/issued/revoke-all":
			posts.Add(1)
			if !proved.Load() {
				writeAPIError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired)
				return
			}
			writeJSON(w, schema.RevokeIssuedResponseV1{CredentialsRevoked: 2})
		default:
			http.NotFound(w, r)
		}
	})
	stdout, _, err := runCLI(t, walkPassword+"\n", "admin", "principal", "revoke-issued", "7", "--yes", "--server", fake.URL)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "revoked 2 credential(s) and 0 hook(s)\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if posts.Load() != 2 || proofs.Load() != 1 {
		t.Errorf("posts %d, proofs %d; want 2 and 1", posts.Load(), proofs.Load())
	}
}

// A hostile server cannot put control characters on the terminal through the
// lists, and cannot make the client read an unbounded body.
func TestAdminIssuedHostileServer(t *testing.T) {
	at := schema.NewTimestamp(time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC))
	hostileRows := []schema.IssuedByOperatorV1{{Kind: schema.IssuedKind(hostile), ID: 1, PrincipalID: 2, IssuedBy: 7, IssuedAt: at}}
	tests := []struct {
		name    string
		args    []string
		handler http.HandlerFunc
		wantErr string
	}{
		{"disable list", []string{"principal", "disable", "7"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.DisablePrincipalResponseV1{IssuedToOthers: hostileRows})
		}, ""},
		{"role list", []string{"principal", "role", "7", "member"}, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, schema.RoleChangeResponseV1{Principal: schema.PrincipalAdminV1{ID: 7, Role: schema.RoleMember}, IssuedToOthers: hostileRows})
		}, ""},
		{"oversized disable body", []string{"principal", "disable", "7"}, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"issued_to_others":[` + strings.Repeat(" ", maxIssuedBody+1) + `]}`))
		}, "larger than"},
		{"oversized revoke body", []string{"principal", "revoke-issued", "7", "--yes"}, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"credentials_revoked":1,` + strings.Repeat(" ", listSlack+1) + `"hooks_revoked":1}`))
		}, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			fake, _ := issuedFake(t, tt.handler)
			stdout, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			for _, bad := range []string{esc, bel, bs, nul, del, c1, rlo, lri, lsep, psep, "\r"} {
				if strings.Contains(stdout, bad) {
					t.Errorf("stdout carries %q: %q", bad, stdout)
				}
			}
			if !strings.Contains(stdout, `\x1b`) {
				t.Errorf("stdout = %q, want the escape shown as text", stdout)
			}
		})
	}
}
