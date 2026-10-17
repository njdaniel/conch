package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// adminEnv is a real conchd handler with auth required, an operator and one
// ordinary member, each with a token.
type adminEnv struct {
	url           string
	operatorToken string
	memberToken   string
	memberID      int64
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	isolateConfig(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, _, operatorToken, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatalf("bootstrap operator: %v", err)
	}
	member, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "mallory")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	_, memberToken, err := st.CreateCredential(ctx, "system", member.ID, "test", nil)
	if err != nil {
		t.Fatalf("create member credential: %v", err)
	}
	srv := server.New(server.Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "vtest"}, st)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &adminEnv{url: ts.URL, operatorToken: operatorToken, memberToken: memberToken, memberID: member.ID}
}

// admin runs `conch admin <args> --server <url>` with token as CONCH_TOKEN.
func (e *adminEnv) admin(t *testing.T, token, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("CONCH_TOKEN", token)
	return runCLI(t, stdin, append([]string{"admin"}, append(args, "--server", e.url)...)...)
}

var tokenPattern = regexp.MustCompile(`^conch_[A-Za-z0-9_-]{43}$`)

const testManifest = `{"display_name":"deploy-bot","tier":"A","capabilities":["messages.read","messages.post"],"channels":[{"channel_id":1,"permissions":["read","post"]}]}`

// TestAdminCommands walks one community setup through every command. The ids
// are the ones a fresh instance hands out: the operator is 1, the member 2,
// so the new channel is 1, alice 3 and deploy-bot 4.
func TestAdminCommands(t *testing.T) {
	e := newAdminEnv(t)
	steps := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantStderr string
	}{
		{name: "channel create", args: []string{"channel", "create", "ops"}, wantStdout: "created channel ops (id 1)\n"},
		{name: "human", args: []string{"principal", "create", "--kind", "human", "alice"}, wantStdout: "created human alice (id 3)\n"},
		{name: "agent flag last", args: []string{"principal", "create", "deploy-bot", "--kind", "agent"}, wantStdout: "created agent deploy-bot (id 4)\n"},
		{name: "member add", args: []string{"member", "add", "ops", "3"}, wantStdout: "added 3 to ops\n"},
		{name: "member add agent", args: []string{"member", "add", "ops", "4"}, wantStdout: "added 4 to ops\n"},
		{name: "member decide off", args: []string{"member", "decide", "--off", "ops", "3"}, wantStdout: "3 may no longer decide approvals in ops\n"},
		{name: "member decide", args: []string{"member", "decide", "ops", "3"}, wantStdout: "3 may decide approvals in ops\n"},
		{name: "member add without the right", args: []string{"member", "add", "--no-decide", "ops", "2"}, wantStdout: "added 2 to ops, without the right to decide its approvals\n"},
		{name: "manifest put from stdin", args: []string{"manifest", "put", "4"}, stdin: testManifest, wantStdout: "stored manifest for principal 4\n"},
		{name: "manifest get", args: []string{"manifest", "get", "4"}, wantStdout: "4 tier A capabilities messages.read,messages.post channels 1:read,post deploy-bot\n"},
		{name: "member remove", args: []string{"member", "remove", "ops", "4"}, wantStdout: "removed 4 from ops\n"},
		{name: "disable", args: []string{"principal", "disable", "4"}, wantStdout: "disabled principal 4\n"},
		{name: "enable", args: []string{"principal", "enable", "4"}, wantStdout: "enabled principal 4\n"},
		{name: "rename", args: []string{"principal", "rename", "3", "Alice Liddell"}, wantStdout: "renamed principal 3 to Alice Liddell\n"},
		{name: "rename an agent", args: []string{"principal", "rename", "4", "builder"}, wantStdout: "renamed principal 4 to builder\n"},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, stderr, err := e.admin(t, e.operatorToken, st.stdin, st.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != st.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, st.wantStdout)
			}
			if stderr != st.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr, st.wantStderr)
			}
		})
	}
}

func TestAdminManifestPutFromFile(t *testing.T) {
	e := newAdminEnv(t)
	for _, args := range [][]string{
		{"channel", "create", "ops"},
		{"principal", "create", "--kind", "agent", "bot"},
	} {
		if _, _, err := e.admin(t, e.operatorToken, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(testManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := e.admin(t, e.operatorToken, "", "manifest", "put", "3", "--file", path)
	if err != nil {
		t.Fatalf("manifest put --file: %v", err)
	}
	if want := "stored manifest for principal 3\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestAdminCredentials(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "principal", "create", "--kind", "human", "alice"); err != nil {
		t.Fatal(err)
	}
	// alice is principal 3.
	t.Run("issue prints the token alone", func(t *testing.T) {
		stdout, stderr, err := e.admin(t, e.operatorToken, "", "credential", "issue", "3", "--label", "alice laptop")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		token := strings.TrimSuffix(stdout, "\n")
		if !tokenPattern.MatchString(token) || strings.Count(stdout, "\n") != 1 {
			t.Fatalf("stdout = %q, want one token line", stdout)
		}
		if strings.Contains(stderr, token) {
			t.Errorf("stderr contains the token: %q", stderr)
		}
		if want := "credential 3 does not expire\n"; stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
		// The token works: it is alice's own.
		t.Setenv("CONCH_TOKEN", token)
		who, _, err := runCLI(t, "", "whoami", "--server", e.url)
		if err != nil || !strings.Contains(who, "alice") {
			t.Errorf("whoami with the issued token = %q, %v; want alice", who, err)
		}
	})
	t.Run("issue with expiry", func(t *testing.T) {
		expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
		stdout, stderr, err := e.admin(t, e.operatorToken, "", "credential", "issue", "--expires-at", expires.Format(time.RFC3339), "3", "--label", "short")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if token := strings.TrimSpace(stdout); strings.Contains(stderr, token) {
			t.Errorf("stderr contains the token")
		}
		if want := "credential 4 expires " + expires.Format("2006-01-02T15:04:05.000Z") + "\n"; stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})
	t.Run("list", func(t *testing.T) {
		stdout, stderr, err := e.admin(t, e.operatorToken, "", "credential", "list", "3")
		if err != nil || stderr != "" {
			t.Fatalf("run: %v, stderr %q", err, stderr)
		}
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("stdout = %q, want two lines", stdout)
		}
		for i, want := range []string{"4 active ", "3 active "} {
			if !strings.HasPrefix(lines[i], want) {
				t.Errorf("line %d = %q, want prefix %q", i, lines[i], want)
			}
		}
		if !strings.HasSuffix(lines[0], " short") || !strings.HasSuffix(lines[1], " - alice laptop") {
			t.Errorf("labels or expiry columns wrong: %q", lines)
		}
	})
	t.Run("revoke then list shows revoked", func(t *testing.T) {
		stdout, _, err := e.admin(t, e.operatorToken, "", "credential", "revoke", "3")
		if err != nil || stdout != "revoked credential 3\n" {
			t.Fatalf("revoke = %q, %v", stdout, err)
		}
		list, _, err := e.admin(t, e.operatorToken, "", "credential", "list", "3")
		if err != nil || !strings.Contains(list, "3 revoked ") {
			t.Errorf("list = %q, %v; want credential 3 revoked", list, err)
		}
	})
}

// TestAdminJSON decodes each --json output strictly into the schema type the
// route documents.
func TestAdminJSON(t *testing.T) {
	e := newAdminEnv(t)
	run := func(stdin string, args ...string) string {
		t.Helper()
		stdout, _, err := e.admin(t, e.operatorToken, stdin, append(args, "--json")...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout
	}
	strict := func(t *testing.T, raw string, into any) {
		t.Helper()
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if dec.More() {
			t.Fatalf("extra data after the document in %q", raw)
		}
	}
	tests := []struct {
		name  string
		args  []string
		stdin string
		check func(t *testing.T, out string)
	}{
		{"channel create", []string{"channel", "create", "ops"}, "", func(t *testing.T, out string) {
			var r schema.CreateChannelResponse
			strict(t, out, &r)
			if r.Channel.Name != "ops" || r.Channel.ID != 1 {
				t.Errorf("channel = %+v", r.Channel)
			}
		}},
		{"principal create", []string{"principal", "create", "--kind", "agent", "bot"}, "", func(t *testing.T, out string) {
			var r schema.CreatePrincipalResponse
			strict(t, out, &r)
			if r.Principal.Name != "bot" || r.Principal.Kind != schema.PrincipalAgent {
				t.Errorf("principal = %+v", r.Principal)
			}
		}},
		{"credential issue", []string{"credential", "issue", "3", "--label", "bot"}, "", func(t *testing.T, out string) {
			var r schema.CreateCredentialResponseV1
			strict(t, out, &r)
			if !tokenPattern.MatchString(r.Token) || r.Credential.PrincipalID != 3 {
				t.Errorf("response = %+v", r.Credential)
			}
		}},
		{"credential list", []string{"credential", "list", "3"}, "", func(t *testing.T, out string) {
			var r schema.ListCredentialsResponseV1
			strict(t, out, &r)
			if len(r.Credentials) != 1 {
				t.Errorf("credentials = %+v", r.Credentials)
			}
		}},
		{"manifest put", []string{"manifest", "put", "3"}, testManifest, func(t *testing.T, out string) {
			var r schema.PutAgentManifestResponseV1
			strict(t, out, &r)
			if err := r.Manifest.Validate(); err != nil || r.Manifest.PrincipalID != 3 {
				t.Errorf("manifest = %+v, %v", r.Manifest, err)
			}
		}},
		{"manifest get", []string{"manifest", "get", "3"}, "", func(t *testing.T, out string) {
			var r schema.GetAgentManifestResponseV1
			strict(t, out, &r)
			if r.Manifest.DisplayName != "deploy-bot" {
				t.Errorf("manifest = %+v", r.Manifest)
			}
		}},
		{"principal rename", []string{"principal", "rename", "3", "builder"}, "", func(t *testing.T, out string) {
			var r schema.RenamePrincipalResponseV1
			strict(t, out, &r)
			if r.Principal.ID != 3 || r.Principal.Name != "builder" {
				t.Errorf("principal = %+v", r.Principal)
			}
		}},
		{"member add has no document", []string{"member", "add", "ops", "3"}, "", func(t *testing.T, out string) {
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, run(tt.stdin, tt.args...))
		})
	}
}

// TestAdminMemberRefused: the CLI makes no authorization decision, so every
// operator command run by a member is refused by the server, with its code.
func TestAdminMemberRefused(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"channel create", []string{"channel", "create", "x"}, ""},
		{"principal create", []string{"principal", "create", "--kind", "human", "x"}, ""},
		{"principal disable", []string{"principal", "disable", "2"}, ""},
		{"principal enable", []string{"principal", "enable", "2"}, ""},
		{"principal rename", []string{"principal", "rename", "1", "x"}, ""},
		{"credential issue", []string{"credential", "issue", "2", "--label", "x"}, ""},
		{"credential list", []string{"credential", "list", "2"}, ""},
		{"credential revoke", []string{"credential", "revoke", "1"}, ""},
		{"member add", []string{"member", "add", "ops", "2"}, ""},
		{"member remove", []string{"member", "remove", "ops", "2"}, ""},
		{"manifest put", []string{"manifest", "put", "2"}, testManifest},
		{"manifest get", []string{"manifest", "get", "1"}, ""},
		{"principal list", []string{"principal", "list"}, ""},
		{"principal hide", []string{"principal", "hide", "2"}, ""},
		{"principal hide by prefix", []string{"principal", "hide", "--prefix", "mal", "--yes"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, e.memberToken, tt.stdin, tt.args...)
			if err == nil {
				t.Fatalf("a member ran %v without error", tt.args)
			}
			if !strings.Contains(err.Error(), "forbidden") {
				t.Errorf("error = %q, want the server's forbidden code", err)
			}
			if strings.Contains(ErrorLine(err), "\n") {
				t.Errorf("error is not one line: %q", err)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestAdminServerErrorCodeInText(t *testing.T) {
	e := newAdminEnv(t)
	_, _, err := e.admin(t, e.operatorToken, "", "principal", "disable", "999")
	if err == nil || !strings.Contains(err.Error(), "principal_not_found") {
		t.Errorf("error = %v, want principal_not_found", err)
	}
}

// --renew-days issues a credential that renews by being used (issue #439).
func TestAdminCredentialIssueRenewable(t *testing.T) {
	e := newAdminEnv(t)
	before := time.Now()
	stdout, stderr, err := e.admin(t, e.operatorToken, "", "credential", "issue", "--renew-days", "90", "2", "--label", "browser")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !tokenPattern.MatchString(strings.TrimSpace(stdout)) {
		t.Errorf("stdout is not a token alone")
	}
	m := regexp.MustCompile(`^credential 3 expires (\S+), renewed by use\n$`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr = %q, want the expiry and that it is renewed by use", stderr)
	}
	exp, err := time.Parse("2006-01-02T15:04:05.000Z07:00", m[1])
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi := before.Add(90*24*time.Hour-time.Second), time.Now().Add(90*24*time.Hour+time.Second); exp.Before(lo) || exp.After(hi) {
		t.Errorf("expires %v, want about 90 days from now", exp)
	}

	out, _, err := e.admin(t, e.operatorToken, "", "credential", "list", "--json", "2")
	if err != nil {
		t.Fatal(err)
	}
	var list schema.ListCredentialsResponseV1
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("list --json: %v: %q", err, out)
	}
	if len(list.Credentials) != 2 || !list.Credentials[0].Renews || list.Credentials[1].Renews {
		t.Errorf("credentials = %+v, want the new one renewable and the old one not", list.Credentials)
	}
}

func TestAdminUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{"admin"}, "usage: conch admin <channel|principal|credential|member|manifest|invite|reports>"},
		{"unknown group", []string{"admin", "frob"}, `unknown subcommand "frob"`},
		{"group without verb", []string{"admin", "member"}, "usage: conch admin member <add|decide|remove>"},
		{"unknown verb", []string{"admin", "member", "frob"}, `unknown subcommand "frob"`},
		{"channel create without name", []string{"admin", "channel", "create"}, "usage: conch admin channel create [--server <url>] [--json] <name>"},
		{"channel create extra", []string{"admin", "channel", "create", "a", "b"}, "expected <name>"},
		{"principal create without kind", []string{"admin", "principal", "create", "alice"}, "--kind is required"},
		{"principal create bad kind", []string{"admin", "principal", "create", "--kind", "robot", "alice"}, `kind must be "human" or "agent"`},
		{"principal disable without id", []string{"admin", "principal", "disable"}, "expected <id>"},
		{"principal disable bad id", []string{"admin", "principal", "disable", "abc"}, "id must be a positive integer"},
		{"principal rename without name", []string{"admin", "principal", "rename", "3"}, "expected <id> <name>"},
		{"principal rename bad id", []string{"admin", "principal", "rename", "bob", "robert"}, "id must be a positive integer"},
		{"credential issue without label", []string{"admin", "credential", "issue", "3"}, "--label is required"},
		{"credential issue without id", []string{"admin", "credential", "issue", "--label", "x"}, "expected <principal-id>"},
		{"credential issue bad expiry", []string{"admin", "credential", "issue", "3", "--label", "x", "--expires-at", "tomorrow"}, "--expires-at must be an RFC 3339 time"},
		{"credential issue past expiry", []string{"admin", "credential", "issue", "3", "--label", "x", "--expires-at", "2001-01-01T00:00:00Z"}, "must be in the future"},
		{"credential issue renew days not a number", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "ninety"}, "must be a whole number of days"},
		{"credential issue renew one day", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "1"}, "--renew-days must be between 2 and 365"},
		{"credential issue renew too long", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "366"}, "--renew-days must be between 2 and 365"},
		{"credential issue renew huge", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "9223372036854775807"}, "--renew-days must be between 2 and 365"},
		{"credential issue renew negative", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "-90"}, "--renew-days must be between 2 and 365"},
		{"credential issue renew and expiry", []string{"admin", "credential", "issue", "3", "--label", "x", "--renew-days", "90", "--expires-at", "2099-01-01T00:00:00Z"}, "cannot both be given"},
		{"credential revoke zero", []string{"admin", "credential", "revoke", "0"}, "credential-id must be a positive integer"},
		{"member add missing id", []string{"admin", "member", "add", "ops"}, "expected <channel> <principal-id>"},
		{"unknown flag", []string{"admin", "channel", "create", "--frob", "x"}, "flag provided but not defined"},
		{"manifest put without id", []string{"admin", "manifest", "put"}, "expected <principal-id>"},
		{"principal hide with nothing", []string{"admin", "principal", "hide"}, "give an id, or --prefix or --invite"},
		{"principal hide with both", []string{"admin", "principal", "hide", "3", "--prefix", "test-"}, "not both"},
		{"principal hide yes with an id", []string{"admin", "principal", "hide", "3", "--yes"}, "not both"},
		{"principal hide short prefix", []string{"admin", "principal", "hide", "--prefix", "te"}, "3 to 100 characters"},
		{"principal hide bad kind", []string{"admin", "principal", "hide", "--prefix", "test-", "--kind", "robot"}, "kind"},
		{"principal hide bad id", []string{"admin", "principal", "hide", "abc"}, "id must be a positive integer"},
		{"principal hide two ids", []string{"admin", "principal", "hide", "3", "4"}, "expected <id>"},
		{"principal list bad state", []string{"admin", "principal", "list", "--state", "gone"}, "--state must be active, disabled or hidden"},
		{"principal list bad kind", []string{"admin", "principal", "list", "--kind", "robot"}, "--kind must be"},
		{"principal list stray argument", []string{"admin", "principal", "list", "3"}, "usage: conch admin principal list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			stdout, stderr, err := runCLI(t, "", append(tt.args, "--server", "http://127.0.0.1:1")...)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("error is more than one line: %q", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout %q, stderr %q; want both empty (main prints the error)", stdout, stderr)
			}
		})
	}
}

// TestAdminManifestRefusedLocally: a manifest that fails Validate, or does not
// parse, never reaches the network.
func TestAdminManifestRefusedLocally(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bad tier", `{"display_name":"x","tier":"Z"}`, "invalid manifest"},
		{"unknown capability", `{"display_name":"x","tier":"A","capabilities":["messages.fly"]}`, "invalid manifest"},
		{"blank display name", `{"display_name":"","tier":"A"}`, "invalid manifest"},
		{"misspelled field", `{"display_name":"x","tier":"A","capabilitys":["messages.read"]}`, "not a valid manifest request"},
		{"a get response", `{"manifest":{"display_name":"x"}}`, "not a valid manifest request"},
		{"not json", `tier: A`, "not a valid manifest request"},
		{"empty", ``, "not a valid manifest request"},
		{"trailing data", testManifest + ` {}`, "data after the JSON document"},
		{"a stray closing brace", testManifest + `}`, "data after the JSON document"},
		{"a stray closing bracket", testManifest + ` ]`, "data after the JSON document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			_, _, err := runCLI(t, tt.body, "admin", "manifest", "put", "3", "--server", fake.URL)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("%d requests were sent, want none", n)
			}
		})
	}
}

func TestAdminManifestFileMissing(t *testing.T) {
	isolateConfig(t)
	_, _, err := runCLI(t, "", "admin", "manifest", "put", "3", "--file", filepath.Join(t.TempDir(), "absent.json"), "--server", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "open --file") {
		t.Errorf("error = %v, want an open --file error", err)
	}
}

// TestAdminTokenNeverInError: even a server that echoes a token inside a
// malformed response cannot get it into the error line.
func TestAdminTokenNeverInError(t *testing.T) {
	isolateConfig(t)
	const secret = "conch_SECRETSECRETSECRETSECRETSECRETSECRETSECR"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"credential": "` + secret + `"`))
	}))
	defer fake.Close()
	var out, errOut bytes.Buffer
	err := RunWithStdin(context.Background(), []string{"admin", "credential", "issue", "3", "--label", "x", "--server", fake.URL}, strings.NewReader(""), &out, &errOut, "vtest")
	if err == nil {
		t.Fatal("no error for a malformed response")
	}
	for name, got := range map[string]string{"error": err.Error(), "stdout": out.String(), "stderr": errOut.String()} {
		if strings.Contains(got, secret) {
			t.Errorf("%s contains the token", name)
		}
	}
}

// A token that is not shaped like one is not printed, whatever it holds, and
// the error does not repeat it.
func TestAdminIssueRefusesMalformedToken(t *testing.T) {
	good := schema.CredentialTokenPrefix + strings.Repeat("A", schema.CredentialTokenLength-len(schema.CredentialTokenPrefix))
	tests := []struct {
		name   string
		token  string
		wantOK bool
	}{
		{"well formed", good, true},
		{"empty", "", false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "A", false},
		{"wrong prefix", "other_" + good[len("other_"):], false},
		{"escape sequence", good[:20] + string(rune(0x1b)) + "[2J" + good[24:], false},
		{"padding", good[:len(good)-1] + "=", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(schema.CreateCredentialResponseV1{Token: tt.token})
			}))
			defer fake.Close()
			var out, errOut bytes.Buffer
			err := RunWithStdin(context.Background(), []string{"admin", "credential", "issue", "3", "--label", "x", "--server", fake.URL}, strings.NewReader(""), &out, &errOut, "vtest")
			if tt.wantOK {
				if err != nil || out.String() != tt.token+"\n" {
					t.Errorf("err = %v, stdout = %q; want the token", err, out.String())
				}
				return
			}
			if err == nil || out.Len() != 0 {
				t.Fatalf("err = %v, stdout = %q; want a refusal and no output", err, out.String())
			}
			if tt.token != "" && strings.Contains(err.Error()+errOut.String(), tt.token) {
				t.Errorf("the refusal repeats the token")
			}
		})
	}
}

func TestUsageListsAdmin(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	for _, want := range []string{"conch admin channel create", "conch admin credential issue", "conch admin manifest put"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// TestAdminPrincipalListAndHide walks the cleanup of test accounts (issue
// #445) against a real server: the list, a dry run that changes nothing, a
// bulk hide by prefix and kind, a hide by id, the hidden list, and the names
// free again while hidden-<id> stays reserved. Ids as in TestAdminCommands:
// root 1, mallory 2, then test-a 3, test-b 4 and keeper 5.
func TestAdminPrincipalListAndHide(t *testing.T) {
	e := newAdminEnv(t)
	steps := []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{"create a", []string{"principal", "create", "--kind", "human", "test-a"}, "created human test-a (id 3)\n"},
		{"create b", []string{"principal", "create", "--kind", "agent", "test-b"}, "created agent test-b (id 4)\n"},
		{"create keeper", []string{"principal", "create", "--kind", "human", "keeper"}, "created human keeper (id 5)\n"},
		{"list", []string{"principal", "list"}, "5 human member active creds 0 invite - keeper\n" +
			"4 agent member active creds 0 invite - test-b\n" +
			"3 human member active creds 0 invite - test-a\n" +
			"2 human member active creds 1 invite - mallory\n" +
			"1 human operator active creds 1 invite - root\n"},
		{"dry run", []string{"principal", "hide", "--prefix", "test-"}, "3 test-a\n4 test-b\nwould hide 2 principal(s); run again with --yes to do it\n"},
		{"nothing hidden by the dry run", []string{"principal", "list", "--prefix", "test-"}, "4 agent member active creds 0 invite - test-b\n" +
			"3 human member active creds 0 invite - test-a\n"},
		{"agents only", []string{"principal", "hide", "--prefix", "test-", "--kind", "agent", "--yes"}, "4 test-b\nhid 1 principal(s)\n"},
		{"by id", []string{"principal", "hide", "3"}, "hid principal 3; it is now called hidden-3\n"},
		{"hidden", []string{"principal", "list", "--state", "hidden"}, "4 agent member hidden creds 0 invite - hidden-4\n" +
			"3 human member hidden creds 0 invite - hidden-3\n"},
		{"not hidden", []string{"principal", "list", "--state", "active", "--kind", "human"}, "5 human member active creds 0 invite - keeper\n" +
			"2 human member active creds 1 invite - mallory\n" +
			"1 human operator active creds 1 invite - root\n"},
		{"nothing left", []string{"principal", "hide", "--prefix", "test-", "--yes"}, "hid 0 principal(s)\n"},
		{"the name is free", []string{"principal", "create", "--kind", "human", "test-a"}, "created human test-a (id 6)\n"},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, stderr, err := e.admin(t, e.operatorToken, "", st.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != st.wantStdout || stderr != "" {
				t.Errorf("stdout = %q, stderr %q; want %q", stdout, stderr, st.wantStdout)
			}
		})
	}

	// The hidden name, and a look-alike of it, cannot be taken.
	for _, name := range []string{"hidden-9", "HIDDEN-l2"} {
		_, _, err := e.admin(t, e.operatorToken, "", "principal", "create", "--kind", "human", name)
		if err == nil || !strings.Contains(err.Error(), schema.ErrorCodePrincipalExists) {
			t.Errorf("create %q = %v, want %s", name, err, schema.ErrorCodePrincipalExists)
		}
	}
	// Nor can the operator hide themselves.
	if _, _, err := e.admin(t, e.operatorToken, "", "principal", "hide", "1"); err == nil || !strings.Contains(err.Error(), "cannot hide themselves") {
		t.Errorf("hide self = %v", err)
	}

	// --json prints the server's documents.
	stdout, _, err := e.admin(t, e.operatorToken, "", "principal", "list", "--state", "hidden", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list schema.ListPrincipalsResponseV1
	if err := json.Unmarshal([]byte(stdout), &list); err != nil || len(list.Principals) != 2 || list.Principals[0].Name != "hidden-4" {
		t.Errorf("--json list = %q (%v)", stdout, err)
	}
	stdout, _, err = e.admin(t, e.operatorToken, "", "principal", "hide", "--prefix", "keep", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var dry schema.HidePrincipalsResponseV1
	if err := json.Unmarshal([]byte(stdout), &dry); err != nil || !dry.DryRun || dry.Count != 1 || dry.Principals[0].Name != "keeper" {
		t.Errorf("--json dry run = %q (%v)", stdout, err)
	}
}

// The list follows the server's pages to the end, passing next_before as
// before, and refuses pages that do not move.
func TestAdminPrincipalListPages(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		t.Run(fmt.Sprintf("stuck=%t", stuck), func(t *testing.T) {
			isolateConfig(t)
			var queries []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries = append(queries, r.URL.RawQuery)
				resp := schema.ListPrincipalsResponseV1{Principals: []schema.PrincipalAdminV1{{ID: 9, Kind: schema.PrincipalHuman, Name: "a", Role: schema.RoleMember}}, NextBefore: 9}
				if r.URL.Query().Get("before") == "9" && !stuck {
					resp = schema.ListPrincipalsResponseV1{Principals: []schema.PrincipalAdminV1{{ID: 4, Kind: schema.PrincipalAgent, Name: "b", Role: schema.RoleMember}}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}))
			t.Cleanup(ts.Close)
			t.Setenv("CONCH_TOKEN", "conch_"+strings.Repeat("A", 43))
			stdout, _, err := runCLI(t, "", "admin", "principal", "list", "--prefix", "x y", "--server", ts.URL)
			if stuck {
				if err == nil || !strings.Contains(err.Error(), "do not move forward") {
					t.Errorf("stuck pages: err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := "9 human member active creds 0 invite - a\n4 agent member active creds 0 invite - b\n"; stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if want := []string{"limit=200&name_prefix=x+y", "before=9&limit=200&name_prefix=x+y"}; !slices.Equal(queries, want) {
				t.Errorf("queries = %q, want %q", queries, want)
			}
		})
	}
}
