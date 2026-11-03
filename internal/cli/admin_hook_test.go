package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin hook list|revoke` (issue #1228) against a real conchd handler
// and a fake server that refuses or lies.

// newHook provisions a hook through the public route, as the operator, and
// returns its id and its secret token (which the commands under test must
// never print).
func (e *adminEnv) newHook(t *testing.T, channel, label string) (id int64, token string) {
	t.Helper()
	body, err := json.Marshal(schema.CreateHookRequest{Channel: channel, Principal: e.memberID, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/v1/hooks", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.operatorToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var created schema.CreateHookResponse
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&created) != nil || created.ID == 0 || created.Token == "" {
		t.Fatalf("create hook: status %d, %+v", resp.StatusCode, created)
	}
	return created.ID, created.Token
}

func TestAdminHookListAndRevoke(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	id, token := e.newHook(t, "ops", "deploy bot")

	stdout, stderr, err := e.admin(t, e.operatorToken, "", "hook", "list")
	if err != nil || stderr != "" {
		t.Fatalf("list: %v, stderr %q", err, stderr)
	}
	for _, want := range []string{"active", "channel ops", "principal 2", "deploy bot"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list = %q, want it to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, token) {
		t.Fatalf("list printed the hook's token: %q", stdout)
	}

	if stdout, _, err = e.admin(t, e.operatorToken, "", "hook", "revoke", "1"); err != nil || stdout != "revoked hook 1\n" {
		t.Fatalf("revoke = %q, %v", stdout, err)
	}
	// Revoking again succeeds, as the route does.
	if _, _, err = e.admin(t, e.operatorToken, "", "hook", "revoke", "1"); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	stdout, _, err = e.admin(t, e.operatorToken, "", "hook", "list")
	if err != nil || !strings.Contains(stdout, "revoked ") || strings.Contains(stdout, token) {
		t.Errorf("list after revoke = %q, %v", stdout, err)
	}

	// --json is the schema document, and holds no token either.
	stdout, _, err = e.admin(t, e.operatorToken, "", "hook", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc schema.ListHooksResponseV1
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Hooks) != 1 || doc.Hooks[0].ID != id || doc.Hooks[0].RevokedAt == nil {
		t.Errorf("--json = %q (%v), want the revoked hook %d", stdout, err, id)
	}
	if strings.Contains(stdout, token) || strings.Contains(stdout, "token") {
		t.Errorf("--json carries a token: %q", stdout)
	}
}

func TestAdminHookRefusals(t *testing.T) {
	e := newAdminEnv(t)
	tests := []struct {
		name  string
		token string
		args  []string
		code  string
		line  string
	}{
		{"a member may not list", e.memberToken, []string{"hook", "list"}, "forbidden", "only an operator may do this"},
		{"a member may not revoke", e.memberToken, []string{"hook", "revoke", "1"}, "forbidden", "only an operator may do this"},
		{"no such hook", e.operatorToken, []string{"hook", "revoke", "77"}, "hook_not_found", "no such hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, tt.token, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.line) {
				t.Fatalf("error = %v, want a line with %q", err, tt.line)
			}
			var ref *refusalError
			if !errors.As(err, &ref) || !IsServerCode(err, tt.code) {
				t.Errorf("error = %v, want a refusalError over the server's %s", err, tt.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestAdminHookUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"revoke needs an id", []string{"hook", "revoke"}, "expected <hook-id>"},
		{"revoke wants a number", []string{"hook", "revoke", "abc"}, "positive integer"},
		{"revoke wants a positive id", []string{"hook", "revoke", "0"}, "positive integer"},
		{"list takes no id", []string{"hook", "list", "3"}, "expected <>"},
		{"unknown verb", []string{"hook", "create"}, "hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			_, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if requests.Load() != 0 {
				t.Errorf("%d requests were sent, want none", requests.Load())
			}
		})
	}
}

// A server that sends a token anyway, and names a terminal would act on, gets
// neither echoed: the token is not in the schema type, and the names are made
// safe.
func TestAdminHookListDoesNotEchoTokens(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	secret := "hook_secret_do_not_print"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hooks":[{"id":4,"channel":"ops\u001b[31m","principal":2,"label":"x\u202e","created_at":"2026-10-13T12:00:00Z","token":"` + secret + `","url":"/v1/hooks/` + secret + `"}],"other_hooks":2}`))
	}))
	defer fake.Close()
	stdout, stderr, err := runCLI(t, "", "admin", "hook", "list", "--server", fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	if all := stdout + stderr; strings.Contains(all, secret) {
		t.Errorf("list printed the token: %q", all)
	} else if bad := firstUnsafe(all); bad != "" {
		t.Errorf("%s in %q", bad, all)
	}
	if !strings.Contains(stdout, "2 more hook") {
		t.Errorf("list = %q, want the count of hooks left out", stdout)
	}
	stdout, _, err = runCLI(t, "", "admin", "hook", "list", "--json", "--server", fake.URL)
	if err != nil || strings.Contains(stdout, secret) {
		t.Errorf("--json = %q, %v; want no token", stdout, err)
	}
}

// A refusal is in the CLI's words, by its code and permission; what the server
// wrote is never printed.
func TestAdminHookRefusalLines(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		perm   string
		verb   []string
		want   string
	}{
		{"operator without the permission", 403, schema.ErrorCodePermissionDenied, "manage_members", []string{"hook", "list"}, "you do not have the manage_members permission, which this needs"},
		{"unknown permission is not echoed", 403, schema.ErrorCodePermissionDenied, "pay evil.example", []string{"hook", "revoke", "3"}, "you do not have the permission this needs"},
		{"needs a recent password", 403, schema.ErrorCodeReauthRequired, "", []string{"hook", "revoke", "3"}, "conch reauth"},
		{"unknown code", 500, "weird", "", []string{"hook", "list"}, "the server refused this (HTTP 500)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: tt.code, Message: "pwned by evil.example", Permission: schema.Permission(tt.perm)})
			}))
			defer fake.Close()
			_, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.verb...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "evil.example") {
				t.Errorf("error = %v, want %q and none of the server's words", err, tt.want)
			}
		})
	}
}
