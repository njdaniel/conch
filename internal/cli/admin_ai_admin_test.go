package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// aiAdminToken is a well-formed credential token, the secret the tests watch.
const aiAdminToken = "conch_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE"

// aiAdminFake is a conchd that answers the routes `ai-admin` composes and
// records them. fail names a "METHOD path" it refuses with the given status.
type aiAdminFake struct {
	mu          sync.Mutex
	srv         *httptest.Server
	calls       []string
	bodies      map[string]string
	serverName  string
	channels    []schema.ChannelListingV1
	manifests   map[int64]schema.AgentManifestV1
	agents      []schema.PrincipalAdminV1
	failKey     string
	failStatus  int
	failMessage string
	token       string
}

func newAIAdminFake(t *testing.T) *aiAdminFake {
	t.Helper()
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "owner-token")
	t.Setenv("SHELLOPTS", "")
	f := &aiAdminFake{serverName: "Alpha Chat", bodies: map[string]string{}, manifests: map[int64]schema.AgentManifestV1{}, token: aiAdminToken}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *aiAdminFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *aiAdminFake) wrote(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *aiAdminFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	var raw strings.Builder
	if r.Body != nil {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		raw.Write(buf[:n])
	}
	f.bodies[key] = raw.String()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if key == f.failKey {
		reply(f.failStatus, schema.Error{Code: "forbidden", Message: f.failMessage})
		return
	}
	switch key {
	case "GET /v1/whoami":
		reply(200, schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator})
	case "GET /v1/join":
		reply(200, schema.JoinInfoV1{ServerName: f.serverName})
	case "POST /v0/principals":
		reply(201, schema.CreatePrincipalResponse{Principal: schema.PrincipalV0{ID: 7, Kind: schema.PrincipalAgent, Name: "alice-bot"}})
	case "GET /v1/directory/channels":
		reply(200, schema.ChannelDirectoryResponseV1{Channels: f.channels})
	case "POST /v1/channels":
		reply(201, schema.CreateChannelResponseV1{Channel: schema.ChannelV0{ID: 5, Name: "admin"}})
	case "PUT /v1/channels/admin/members/1", "PUT /v1/channels/admin/members/7":
		w.WriteHeader(http.StatusNoContent)
	case "PUT /v1/principals/7/manifest":
		reply(200, schema.PutAgentManifestResponseV1{Manifest: schema.AgentManifestV1{PrincipalID: 7}})
	case "POST /v1/principals/7/credentials":
		reply(201, schema.CreateCredentialResponseV1{Token: f.token, Credential: schema.CredentialV1{ID: 11, Renews: true}})
	case "POST /v1/principals/7/disable":
		w.WriteHeader(http.StatusNoContent)
	case "GET /v1/principals":
		reply(200, schema.ListPrincipalsResponseV1{Principals: f.agents})
	default:
		var id int64
		if strings.HasPrefix(key, "GET /v1/principals/") && strings.HasSuffix(key, "/manifest") {
			parts := strings.Split(key, "/")
			for _, c := range parts[3] {
				id = id*10 + int64(c-'0')
			}
			if m, ok := f.manifests[id]; ok {
				reply(200, schema.GetAgentManifestResponseV1{Manifest: m})
				return
			}
			reply(404, schema.Error{Code: "principal_not_found", Message: "no manifest"})
			return
		}
		reply(404, schema.Error{Code: "principal_not_found", Message: "no route " + key})
	}
}

func (f *aiAdminFake) run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCLI(t, "", append([]string{"admin", "ai-admin"}, append(args, "--server", f.srv.URL)...)...)
}

func TestMCPEntryName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "conch-conch"},
		{"conch", "conch-conch"},
		{"Alpha", "conch-alpha"},
		{"Alpha Chat", "conch-alpha-chat"},
		{"gank.bears_01", "conch-gank-bears-01"},
		{"a-b", "conch-a-b"},
		{"Ünï", "conch--n-"},
		{"x\"y';$(id)", "conch-x-y----id-"},
	}
	for _, tc := range tests {
		if got := mcpEntryName(tc.in); got != tc.want {
			t.Errorf("mcpEntryName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAIAdminCreate(t *testing.T) {
	f := newAIAdminFake(t)
	stdout, stderr, err := f.run(t, "create", "alice-bot")
	if err != nil {
		t.Fatalf("create: %v (stderr %q)", err, stderr)
	}
	// stdout is exactly the JSON entry: it parses, nothing else is there.
	var doc struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not the JSON entry: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Errorf("stdout has more than the entry: %q", stdout)
	}
	entry, ok := doc.MCPServers["conch-alpha-chat"]
	if len(doc.MCPServers) != 1 || !ok {
		t.Fatalf("entries = %v, want only conch-alpha-chat", doc.MCPServers)
	}
	if entry.Type != "http" || entry.URL != f.srv.URL+"/mcp" || entry.Headers["Authorization"] != "Bearer "+aiAdminToken {
		t.Errorf("entry = %+v", entry)
	}
	if strings.Contains(stderr, aiAdminToken) {
		t.Errorf("stderr carries the token: %q", stderr)
	}
	if !strings.Contains(stderr, "--scope user") || !strings.Contains(stderr, "revokes nothing") {
		t.Errorf("stderr lacks the advice: %q", stderr)
	}

	want := []string{
		"GET /v1/whoami", "GET /v1/join", "POST /v0/principals",
		"GET /v1/directory/channels", "POST /v1/channels",
		"PUT /v1/channels/admin/members/1", "PUT /v1/channels/admin/members/7",
		"PUT /v1/principals/7/manifest", "POST /v1/principals/7/credentials",
	}
	if got := f.called(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls =\n%v\nwant\n%v", got, want)
	}
	// The manifest: the design's capabilities, the grant, the approval
	// channel, the admin.act rate limit.
	var req schema.PutAgentManifestRequestV1
	if err := json.Unmarshal([]byte(f.wrote("PUT /v1/principals/7/manifest")), &req); err != nil {
		t.Fatal(err)
	}
	if req.Tier != schema.AgentTierA || len(req.Permissions) != 3 || req.Permissions[0] != schema.PermManageMembers {
		t.Errorf("manifest = %+v", req)
	}
	if req.Admin == nil || req.Admin.ApprovalChannelID != 5 || len(req.Channels) != 1 || req.Channels[0].ChannelID != 5 {
		t.Errorf("manifest channel = %+v / %+v", req.Admin, req.Channels)
	}
	if len(req.RateLimits) != 1 || req.RateLimits[0].Capability != schema.CapabilityAdminAct || req.RateLimits[0].Max != 60 || req.RateLimits[0].WindowSeconds != 3600 {
		t.Errorf("rate limits = %+v", req.RateLimits)
	}
	for _, c := range req.Capabilities {
		if c == schema.CapabilityApprovalsRequest {
			t.Error("approvals.request must not be granted")
		}
	}
	if body := f.wrote("PUT /v1/channels/admin/members/7"); !strings.Contains(body, `"can_decide":false`) {
		t.Errorf("agent membership body = %q, want can_decide false", body)
	}
	if body := f.wrote("PUT /v1/channels/admin/members/1"); !strings.Contains(body, `"can_decide":true`) {
		t.Errorf("owner membership body = %q, want can_decide true", body)
	}
	var cred schema.CreateCredentialRequestV1
	if err := json.Unmarshal([]byte(f.wrote("POST /v1/principals/7/credentials")), &cred); err != nil {
		t.Fatal(err)
	}
	if cred.RenewLifetimeSeconds == nil || *cred.RenewLifetimeSeconds != 90*86400 {
		t.Errorf("credential = %+v, want a 90 day renewable one", cred)
	}
}

func TestAIAdminCreateExistingChannelAndFlags(t *testing.T) {
	f := newAIAdminFake(t)
	f.channels = []schema.ChannelListingV1{{ID: 5, Name: "admin", Visibility: schema.ChannelPrivate, Joined: true}}
	_, _, err := f.run(t, "create", "alice-bot", "--grant", "manage_members,read_audit_log", "--ttl", "2h", "--run-window", "30m",
		"--quorum", "2", "--max-pending", "4", "--require-approval", "disable_member", "--credential-days", "30")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.called() {
		if c == "POST /v1/channels" {
			t.Error("created a channel that exists")
		}
	}
	var req schema.PutAgentManifestRequestV1
	if err := json.Unmarshal([]byte(f.wrote("PUT /v1/principals/7/manifest")), &req); err != nil {
		t.Fatal(err)
	}
	a := req.Admin
	if len(req.Permissions) != 2 || a.ApprovalTTLSeconds != 7200 || a.RunWindowSeconds != 1800 || a.Quorum != 2 || a.MaxPending != 4 ||
		len(a.RequireApproval) != 1 || a.RequireApproval[0] != schema.AdminActionDisableMember {
		t.Errorf("manifest = %+v admin %+v", req, a)
	}
}

func TestAIAdminCreateClients(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		serverName string
		want       string
	}{
		{"claude code one-liner", []string{"--client", "claude-code"}, "alpha",
			"claude mcp add --transport http --scope user conch-alpha SERVER/mcp --header 'Authorization: Bearer " + aiAdminToken + "'\n"},
		{"entry name override", []string{"--client", "claude-code", "--entry-name", "chat2"}, "alpha",
			"claude mcp add --transport http --scope user chat2 SERVER/mcp --header 'Authorization: Bearer " + aiAdminToken + "'\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAIAdminFake(t)
			f.serverName = tc.serverName
			stdout, _, err := f.run(t, append([]string{"create", "alice-bot"}, tc.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.ReplaceAll(tc.want, "SERVER", f.srv.URL); stdout != want {
				t.Errorf("stdout = %q\nwant %q", stdout, want)
			}
			if strings.Count(stdout, "\n") != 1 {
				t.Errorf("the one-liner is more than one line: %q", stdout)
			}
		})
	}
}

func TestAIAdminCreateNonOwnerIsRolledBack(t *testing.T) {
	f := newAIAdminFake(t)
	f.failKey = "PUT /v1/principals/7/manifest"
	f.failStatus = http.StatusForbidden
	f.failMessage = "owner required: only an owner grants an agent permissions"
	stdout, stderr, err := f.run(t, "create", "alice-bot")
	if err == nil {
		t.Fatal("a non-owner's create succeeded")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(err.Error(), "owner required") || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error = %q, want the refusal and the rollback", err)
	}
	calls := f.called()
	if calls[len(calls)-1] != "POST /v1/principals/7/disable" {
		t.Errorf("calls = %v, want the principal disabled last", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "credentials") {
			t.Errorf("a credential was issued: %v", calls)
		}
	}
	if strings.Contains(stderr, aiAdminToken) || strings.Contains(err.Error(), aiAdminToken) {
		t.Error("the token leaked")
	}
}

func TestAIAdminCreateRollbackFailureSaysHow(t *testing.T) {
	f := newAIAdminFake(t)
	f.failKey = "POST /v1/principals/7/credentials"
	f.failStatus = http.StatusForbidden
	f.failMessage = "no"
	// The disable fails too: the error must say what to run.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/principals/7/disable" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: "internal", Message: "boom"})
			return
		}
		f.serve(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, _, err := runCLI(t, "", "admin", "ai-admin", "create", "alice-bot", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "conch admin principal disable 7") {
		t.Fatalf("error = %v, want the manual disable command", err)
	}
}

func TestAIAdminCreateRefusesBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad client", []string{"--client", "cursor"}, "--client"},
		{"bad grant", []string{"--grant", "act_as_members"}, "act_as_members"},
		{"empty grant", []string{"--grant", ","}, "no permission"},
		{"bad action", []string{"--require-approval", "nuke"}, "nuke"},
		{"bad ttl", []string{"--ttl", "soon"}, "--ttl"},
		{"ttl below floor", []string{"--ttl", "1s"}, "approval_ttl_seconds"},
		{"credential days", []string{"--credential-days", "0"}, "--credential-days"},
		{"entry name", []string{"--entry-name", "a b"}, "--entry-name"},
		{"json", []string{"--json"}, "--json"},
		{"channel", []string{"--channel", ""}, "--channel"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAIAdminFake(t)
			_, _, err := f.run(t, append([]string{"create", "alice-bot"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
			if calls := f.called(); len(calls) != 0 {
				t.Errorf("requests made before the refusal: %v", calls)
			}
		})
	}
}

func TestAIAdminCreateRefusesUnderShellTrace(t *testing.T) {
	f := newAIAdminFake(t)
	t.Setenv("SHELLOPTS", "braceexpand:hashall:xtrace")
	_, _, err := f.run(t, "create", "alice-bot")
	if err == nil || !strings.Contains(err.Error(), "set -x") {
		t.Fatalf("error = %v, want a refusal under a trace", err)
	}
	if len(f.called()) != 0 {
		t.Errorf("requests made under a trace: %v", f.called())
	}
}

// failWriter is a stdout that refuses to be written.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestAIAdminCreateNeverLeaksTheToken is the hostile case: whatever goes wrong
// once the token is minted, it reaches neither stderr nor an error.
func TestAIAdminCreateNeverLeaksTheToken(t *testing.T) {
	t.Run("stdout closed", func(t *testing.T) {
		f := newAIAdminFake(t)
		var errOut strings.Builder
		err := RunWithStdin(context.Background(), []string{"admin", "ai-admin", "create", "alice-bot", "--server", f.srv.URL},
			strings.NewReader(""), failWriter{}, &errOut, "vtest")
		if err == nil {
			t.Fatal("create succeeded with nowhere to print")
		}
		if strings.Contains(err.Error(), aiAdminToken) || strings.Contains(errOut.String(), aiAdminToken) {
			t.Errorf("token leaked: err %q stderr %q", err, errOut.String())
		}
		calls := f.called()
		if calls[len(calls)-1] != "POST /v1/principals/7/disable" {
			t.Errorf("the credential was left alive: %v", calls)
		}
	})
	t.Run("malformed token", func(t *testing.T) {
		f := newAIAdminFake(t)
		f.token = "conch_short;rm -rf"
		stdout, stderr, err := f.run(t, "create", "alice-bot")
		if err == nil || stdout != "" {
			t.Fatalf("stdout %q, err %v", stdout, err)
		}
		if strings.Contains(err.Error()+stderr, "rm -rf") {
			t.Errorf("a malformed token was echoed: %v / %q", err, stderr)
		}
	})
	t.Run("scrub", func(t *testing.T) {
		got := scrub(errors.New("server said "+aiAdminToken+" twice "+aiAdminToken), aiAdminToken)
		if strings.Contains(got.Error(), aiAdminToken) {
			t.Errorf("scrub left the token: %v", got)
		}
	})
	t.Run("every stream of a successful run", func(t *testing.T) {
		f := newAIAdminFake(t)
		stdout, stderr, err := f.run(t, "create", "alice-bot", "--client", "claude-code")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, aiAdminToken) || strings.Count(stdout, aiAdminToken) != 1 {
			t.Errorf("token placement wrong: stdout %q stderr %q", stdout, stderr)
		}
	})
}

func aiAdminManifest(id int64, perms ...schema.Permission) schema.AgentManifestV1 {
	m := schema.AgentManifestV1{PrincipalID: id, DisplayName: "bot", Tier: schema.AgentTierA, Capabilities: []schema.Capability{schema.CapabilityAdminAct}, Permissions: perms}
	if len(perms) > 0 {
		m.Admin = &schema.AgentAdminV1{ApprovalChannelID: 5, Quorum: 1}
	}
	return m
}

func TestAIAdminListShowRevoke(t *testing.T) {
	f := newAIAdminFake(t)
	f.agents = []schema.PrincipalAdminV1{
		{ID: 7, Kind: schema.PrincipalAgent, Name: "alice-bot"},
		{ID: 8, Kind: schema.PrincipalAgent, Name: "ci-runner"},
		{ID: 9, Kind: schema.PrincipalAgent, Name: "no-manifest"},
	}
	f.manifests[7] = aiAdminManifest(7, schema.PermManageMembers, schema.PermReadAuditLog)
	f.manifests[8] = aiAdminManifest(8)

	t.Run("list", func(t *testing.T) {
		stdout, stderr, err := f.run(t, "list")
		if err != nil || stderr != "" {
			t.Fatalf("list: %v %q", err, stderr)
		}
		if want := "7 active approvals-in 5 manage_members,read_audit_log alice-bot\n"; stdout != want {
			t.Errorf("list = %q, want %q (only agents holding a grant)", stdout, want)
		}
	})
	t.Run("list json", func(t *testing.T) {
		stdout, _, err := f.run(t, "list", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var ms []schema.AgentManifestV1
		if err := json.Unmarshal([]byte(stdout), &ms); err != nil || len(ms) != 1 || ms[0].PrincipalID != 7 {
			t.Errorf("list --json = %q, %v", stdout, err)
		}
	})
	t.Run("show", func(t *testing.T) {
		stdout, _, err := f.run(t, "show", "7")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"principal 7 bot", "permissions manage_members,read_audit_log", "approval channel 5", "capabilities admin.act"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("show = %q, want %q", stdout, want)
			}
		}
	})
	t.Run("show refuses an agent with no grant", func(t *testing.T) {
		_, _, err := f.run(t, "show", "8")
		if err == nil || !strings.Contains(err.Error(), "not an AI administrator") {
			t.Errorf("error = %v", err)
		}
		_, _, err = f.run(t, "show", "9")
		if err == nil || !strings.Contains(err.Error(), "not an AI administrator") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		stdout, _, err := f.run(t, "revoke", "7")
		if err != nil || !strings.HasPrefix(stdout, "revoked AI administrator 7") {
			t.Fatalf("revoke = %q, %v", stdout, err)
		}
		calls := f.called()
		if calls[len(calls)-1] != "POST /v1/principals/7/disable" {
			t.Errorf("calls = %v", calls)
		}
	})
	t.Run("revoke refuses someone who is not one", func(t *testing.T) {
		before := len(f.called())
		_, _, err := f.run(t, "revoke", "8")
		if err == nil {
			t.Fatal("revoked a principal with no grant")
		}
		for _, c := range f.called()[before:] {
			if strings.HasSuffix(c, "/disable") {
				t.Errorf("disabled a principal that is not an AI administrator: %s", c)
			}
		}
	})
}

// `conch admin manifest put` takes the grant and the admin settings, and
// `manifest get` shows them.
func TestManifestPutAcceptsTheGrant(t *testing.T) {
	f := newAIAdminFake(t)
	body := `{"display_name":"alice-bot","tier":"A","capabilities":["admin.act"],"channels":[{"channel_id":5,"permissions":["read","post"]}],` +
		`"permissions":["manage_members"],"admin":{"approval_channel_id":5,"approval_ttl_seconds":3600,"quorum":2}}`
	stdout, _, err := runCLI(t, body, "admin", "manifest", "put", "7", "--server", f.srv.URL)
	if err != nil || stdout != "stored manifest for principal 7\n" {
		t.Fatalf("put = %q, %v", stdout, err)
	}
	var sent schema.PutAgentManifestRequestV1
	if err := json.Unmarshal([]byte(f.wrote("PUT /v1/principals/7/manifest")), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Permissions) != 1 || sent.Admin == nil || sent.Admin.ApprovalTTLSeconds != 3600 || sent.Admin.Quorum != 2 {
		t.Errorf("sent = %+v", sent)
	}
	// The same checks the server makes, before a request: a grant with no
	// admin block, and a permission an agent may not hold.
	for _, bad := range []string{
		`{"display_name":"a","tier":"A","permissions":["manage_members"]}`,
		`{"display_name":"a","tier":"A","permissions":["act_as_members"],"admin":{"approval_channel_id":5}}`,
	} {
		if _, _, err := runCLI(t, bad, "admin", "manifest", "put", "7", "--server", f.srv.URL); err == nil || !strings.Contains(err.Error(), "invalid manifest") {
			t.Errorf("put %s: error = %v", bad, err)
		}
	}

	f.manifests[7] = aiAdminManifest(7, schema.PermManageChannels)
	out, _, err := runCLI(t, "", "admin", "manifest", "get", "7", "--server", f.srv.URL)
	if err != nil || !strings.Contains(out, " permissions manage_channels approvals-in 5 bot") {
		t.Errorf("get = %q, %v", out, err)
	}
}
