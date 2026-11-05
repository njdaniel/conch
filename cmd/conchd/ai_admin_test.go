package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// runAI runs `conchd ai-admin <args>` and returns stdout, stderr and the
// error.
func runAI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runAIAdmin(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

const aiOrigin = "https://alpha.example.org"

// aiWorld is a data directory with one owner, root, and a store handle held
// open the way a running server holds one.
type aiWorld struct {
	dir  string
	st   *store.Store
	root store.Principal
}

// newAIWorld bootstraps root. When signIn is set root gets a password and a
// confirmed authenticator underneath, as the setup flow would leave them:
// an owner able to sign in.
func newAIWorld(t *testing.T, signIn bool) *aiWorld {
	t.Helper()
	t.Setenv("CONCHD_DATA", "")
	t.Setenv("CONCHD_SERVER_NAME", "")
	dir := t.TempDir()
	if _, _, err := runOp(t, "bootstrap", "--data", dir, "root"); err != nil {
		t.Fatal(err)
	}
	w := &aiWorld{dir: dir, st: openData(t, dir)}
	ps, err := w.st.ListPrincipals(context.Background(), store.PrincipalFilter{Kind: store.PrincipalHuman, Limit: 10}, time.Now())
	if err != nil || len(ps) != 1 {
		t.Fatalf("principals = %v, %v", ps, err)
	}
	if w.root, err = w.st.PrincipalByID(context.Background(), ps[0].ID); err != nil {
		t.Fatal(err)
	}
	if signIn {
		w.finishSetup(t, w.root.ID)
	}
	return w
}

// finishSetup gives principal id a password and a confirmed authenticator.
func (w *aiWorld) finishSetup(t *testing.T, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.st.SetPassword(ctx, "system", id, "$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$x", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(w.dir, "conch.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "INSERT INTO totp (principal_id, secret, enrolled_at) VALUES (?, x'01', 1)", id); err != nil {
		t.Fatal(err)
	}
}

func (w *aiWorld) create(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return runAI(t, append([]string{"create", "--data", w.dir, "--origin", aiOrigin}, args...)...)
}

// counts is what a refusal must leave alone.
func (w *aiWorld) counts(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ps, err := w.st.ListPrincipals(ctx, store.PrincipalFilter{State: schema.PrincipalStateActive, Limit: 1000}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	chs, err := w.st.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := w.st.ListAuditEvents(ctx, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := w.st.ListPrincipals(ctx, store.PrincipalFilter{State: schema.PrincipalStateDisabled, Limit: 1000}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("principals=%d disabled=%d channels=%d audit=%d", len(ps), len(disabled), len(chs), len(evs))
}

func (w *aiWorld) agentID(t *testing.T, name string) int64 {
	t.Helper()
	ps, err := w.st.ListPrincipals(context.Background(), store.PrincipalFilter{Kind: store.PrincipalAgent, NamePrefix: name, Limit: 10}, time.Now())
	if err != nil || len(ps) != 1 {
		t.Fatalf("agent %q = %v, %v", name, ps, err)
	}
	return ps[0].ID
}

func (w *aiWorld) audit(t *testing.T) []store.AuditEvent {
	t.Helper()
	evs, err := w.st.ListAuditEvents(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// entryOf parses stdout as the JSON of design §4.4 and returns the one entry
// and its name.
func entryOf(t *testing.T, stdout string) (string, mcpServerEntry) {
	t.Helper()
	var cfg mcpConfig
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("stdout is not the MCP entry JSON: %v\n%s", err, stdout)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("want one server entry, got %v", cfg.MCPServers)
	}
	for n, e := range cfg.MCPServers {
		return n, e
	}
	return "", mcpServerEntry{}
}

func tokenOf(t *testing.T, e mcpServerEntry) string {
	t.Helper()
	auth, ok := e.Headers["Authorization"]
	tok := strings.TrimPrefix(auth, "Bearer ")
	if !ok || tok == auth || len(e.Headers) != 1 || !strings.HasPrefix(tok, schema.CredentialTokenPrefix) || len(tok) != schema.CredentialTokenLength {
		t.Fatalf("headers = %v, want one Authorization: Bearer <credential>", e.Headers)
	}
	return tok
}

// The whole of create: the entry parses and names the origin's /mcp, the
// token is on stdout alone and works against a store handle opened before
// the command ran (the running server), the grant, the channel, the seats
// and the audit trail are as designed.
func TestAIAdminCreate(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	stdout, stderr, err := w.create(t, "--server-name", "Alpha Chat", "--ttl", "2h", "--quorum", "1", "robo")
	if err != nil {
		t.Fatal(err)
	}
	name, e := entryOf(t, stdout)
	if name != "conch-alpha-chat" || e.Type != "http" || e.URL != aiOrigin+"/mcp" {
		t.Errorf("entry %q = %+v", name, e)
	}
	tok := tokenOf(t, e)
	if strings.Contains(stderr, tok) || strings.Contains(stderr, schema.CredentialTokenPrefix) || strings.Count(stdout, tok) != 1 {
		t.Errorf("the token is not on stdout alone; stderr %q", stderr)
	}
	for _, want := range []string{"will not be shown again", "conchd ai-admin revoke"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}

	// The running server's handle accepts it with no restart.
	agent, err := w.st.ResolveCredential(ctx, tok)
	if err != nil || agent.Kind != store.PrincipalAgent || agent.Name != "robo" {
		t.Fatalf("resolve = %+v, %v", agent, err)
	}

	ch, err := w.st.ChannelByName(ctx, "admin")
	if err != nil || ch.Visibility != schema.ChannelPrivate {
		t.Fatalf("channel = %+v, %v", ch, err)
	}
	m, err := w.st.AgentManifestByPrincipal(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantPerms := []schema.Permission{schema.PermManageMembers, schema.PermManageChannels, schema.PermMakeMemberLinks}
	if fmt.Sprint(m.Permissions) != fmt.Sprint(wantPerms) || !m.Allows(schema.CapabilityAdminAct) || m.Allows(schema.CapabilityApprovalsRequest) {
		t.Errorf("manifest = %+v", m)
	}
	if m.Admin == nil || m.Admin.ApprovalChannelID != ch.ID || m.Admin.ApprovalTTLSeconds != 7200 {
		t.Errorf("admin = %+v", m.Admin)
	}
	members, err := w.st.ListChannelMembers(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	decides := map[int64]bool{}
	for _, mm := range members {
		decides[mm.PrincipalID] = mm.CanDecide
	}
	if len(decides) != 2 || !decides[w.root.ID] || decides[agent.ID] {
		t.Errorf("members (principal: can_decide) = %v; want root deciding and the agent not", decides)
	}

	var sawCreated, sawGrant bool
	for _, ev := range w.audit(t) {
		if strings.Contains(ev.Detail, tok) || strings.Contains(ev.Subject, tok) {
			t.Errorf("audit event %s carries the token", ev.Action)
		}
		if ev.Actor != "system" && ev.Action != "principal_created" {
			// root's own bootstrap events are not under test
			continue
		}
		switch ev.Action {
		case store.AuditAIAdminCreated:
			sawCreated = ev.Actor == "system"
		case store.AuditAdminGrantChanged:
			sawGrant = ev.Actor == "system"
		}
	}
	if !sawCreated || !sawGrant {
		t.Errorf("audit as system: ai_admin_created=%v admin_grant_changed=%v", sawCreated, sawGrant)
	}
}

func TestAIAdminCreateClientFormats(t *testing.T) {
	w := newAIWorld(t, true)
	stdout, _, err := w.create(t, "--client", "claude-code", "--name", "conch_home", "robo")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "claude mcp add --transport http --scope user conch_home '"+aiOrigin+"/mcp' \\") ||
		!strings.HasPrefix(lines[1], `  --header "Authorization: Bearer `+schema.CredentialTokenPrefix) || !strings.HasSuffix(lines[1], `"`) {
		t.Errorf("stdout = %q", stdout)
	}
	tok := strings.TrimSuffix(strings.TrimPrefix(lines[1], `  --header "Authorization: Bearer `), `"`)
	if _, err := w.st.ResolveCredential(context.Background(), tok); err != nil {
		t.Errorf("the printed token does not resolve: %v", err)
	}
}

// An existing approval channel is used as it is; --decider seats exactly the
// people named (and still needs an owner able to sign in); --grant narrows.
func TestAIAdminCreateOptions(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	wren, err := w.st.CreatePrincipal(ctx, store.PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := w.st.CreateChannelAs(ctx, "system", "ops", wren.ID)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := w.create(t, "--channel", "ops", "--decider", fmt.Sprint(wren.ID), "--decider", fmt.Sprint(wren.ID),
		"--grant", "read_audit_log,manage_members", "--require-approval", "submit_admin_action", "robo")
	if err == nil {
		t.Fatalf("an action name that is not in the catalog was accepted: %s", stdout)
	}
	if _, _, err = w.create(t, "--channel", "ops", "--decider", fmt.Sprint(wren.ID), "--decider", fmt.Sprint(wren.ID),
		"--grant", "read_audit_log,manage_members", "robo"); err != nil {
		t.Fatal(err)
	}
	m, err := w.st.AgentManifestByPrincipal(ctx, w.agentID(t, "robo"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(m.Permissions) != fmt.Sprint([]schema.Permission{schema.PermReadAuditLog, schema.PermManageMembers}) || m.Admin.ApprovalChannelID != ops.ID {
		t.Errorf("manifest = %+v", m)
	}
	members, err := w.st.ListChannelMembers(ctx, ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range members {
		if mm.PrincipalID == w.root.ID {
			t.Errorf("root was seated though --decider named someone else: %+v", mm)
		}
	}
	if ok, err := w.st.CanDecideApprovals(ctx, ops.ID, wren.ID); err != nil || !ok {
		t.Errorf("wren decides = %v, %v", ok, err)
	}
}

// Every refusal that can be known before a write is made before it: nothing
// changes, nothing is printed on stdout, and no message carries a token.
func TestAIAdminCreateRefusals(t *testing.T) {
	signedIn := newAIWorld(t, true)
	ctx := context.Background()
	if _, err := signedIn.st.CreatePrincipal(ctx, store.PrincipalHuman, "wren"); err != nil {
		t.Fatal(err)
	}
	if _, err := signedIn.st.CreatePrincipal(ctx, store.PrincipalAgent, "taken"); err != nil {
		t.Fatal(err)
	}
	archived, err := signedIn.st.CreateChannelAs(ctx, "system", "old", signedIn.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(signedIn.dir, "conch.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "UPDATE channels SET archived_at = ? WHERE id = ?", time.Now().Unix(), archived.ID); err != nil {
		t.Fatal(err)
	}
	setupOnly := newAIWorld(t, false)
	passwordOnly := newAIWorld(t, false)
	if _, err := passwordOnly.st.SetPassword(ctx, "system", passwordOnly.root.ID, "$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$x", "test", time.Now()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		world *aiWorld
		args  []string
		want  string
	}{
		{"no owner can sign in: no password", setupOnly, []string{"robo"}, "no owner is able to sign in"},
		{"no owner can sign in: a password and no authenticator", passwordOnly, []string{"robo"}, "no owner is able to sign in"},
		{"no owner can sign in, even with a decider", setupOnly, []string{"--decider", "1", "robo"}, "no owner is able to sign in"},
		{"a name that is taken", signedIn, []string{"taken"}, "already exists"},
		{"a name that looks like a taken one", signedIn, []string{"TAKEN"}, "already exists"},
		{"a name that is a person's", signedIn, []string{"wren"}, "already exists"},
		{"no name", signedIn, nil, "agent's name"},
		{"two names", signedIn, []string{"a", "b"}, "agent's name"},
		{"a decider who is not there", signedIn, []string{"--decider", "999", "robo"}, "not an enabled person"},
		{"a decider who is an agent", signedIn, []string{"--decider", "3", "robo"}, "not an enabled person"},
		{"a decider that is not an id", signedIn, []string{"--decider", "wren", "robo"}, "not a principal id"},
		{"a permission that cannot be granted", signedIn, []string{"--grant", "act_as_members", "robo"}, "cannot be granted"},
		{"a permission that does not exist", signedIn, []string{"--grant", "rule_all", "robo"}, "not in the catalog"},
		{"an empty grant", signedIn, []string{"--grant", ",", "robo"}, "admin is set but no permissions"},
		{"a ttl below the minimum", signedIn, []string{"--ttl", "1s", "robo"}, "approval_ttl_seconds"},
		{"a negative quorum", signedIn, []string{"--quorum", "-1", "robo"}, "quorum"},
		{"too many pending", signedIn, []string{"--max-pending", "1000", "robo"}, "max_pending"},
		{"credential days too few", signedIn, []string{"--credential-days", "1", "robo"}, "--credential-days"},
		{"credential days too many", signedIn, []string{"--credential-days", "4000", "robo"}, "--credential-days"},
		{"an unknown client", signedIn, []string{"--client", "vim", "robo"}, "--client"},
		{"a bad entry name", signedIn, []string{"--name", "a b", "robo"}, "--name"},
		{"an archived approval channel", signedIn, []string{"--channel", "old", "robo"}, "archived"},
		{"a channel name with nothing in it", signedIn, []string{"--channel", " ", "robo"}, "channel"},
		{"a bad server name", signedIn, []string{"--server-name", "guest-x", "robo"}, "server-name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.world.counts(t)
			out, errOut, err := tt.world.create(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want one containing %q", err, tt.want)
			}
			if out != "" || strings.Contains(errOut+err.Error(), schema.CredentialTokenPrefix) {
				t.Errorf("a refusal printed %q / %q", out, errOut)
			}
			if after := tt.world.counts(t); after != before {
				t.Errorf("a refusal changed the store: %s -> %s", before, after)
			}
		})
	}

	flagTests := []struct {
		name string
		args []string
		want string
	}{
		{"no origin", []string{"create", "--data", signedIn.dir, "robo"}, "--origin"},
		{"an http origin", []string{"create", "--data", signedIn.dir, "--origin", "http://alpha.example.org", "robo"}, "https://"},
		{"an origin with a path", []string{"create", "--data", signedIn.dir, "--origin", "https://a.example.org/x", "robo"}, "nothing more"},
		{"no data", []string{"create", "--origin", aiOrigin, "robo"}, "--data"},
		{"a database that is not there", []string{"create", "--data", filepath.Join(signedIn.dir, "nope"), "--origin", aiOrigin, "robo"}, "no database"},
		{"no command", nil, "no command"},
		{"an unknown command", []string{"grant"}, "unknown command"},
	}
	for _, tt := range flagTests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := runAI(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || out != "" {
				t.Errorf("error %v, stdout %q, want a refusal containing %q", err, out, tt.want)
			}
		})
	}
}

// A failure after the agent exists disables it again and says so: here the
// approval channel's name looks the same as an existing one's, which only
// the store can tell. No credential is issued and nothing is printed.
func TestAIAdminCreateRollsBack(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	if _, err := w.st.CreateChannelAs(ctx, "system", "Admin", w.root.ID); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := w.create(t, "robo")
	if err == nil || !strings.Contains(err.Error(), "disabled again") || !strings.Contains(err.Error(), "create the approval channel") {
		t.Fatalf("error = %v", err)
	}
	if out != "" || strings.Contains(errOut+err.Error(), schema.CredentialTokenPrefix) {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
	id := w.agentIDAny(t, "robo")
	p, err := w.st.PrincipalByID(ctx, id)
	if err != nil || p.DisabledAt == nil {
		t.Errorf("the agent is still enabled: %+v, %v", p, err)
	}
	if _, err := w.st.AgentManifestByPrincipal(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a manifest was written: %v", err)
	}
}

func (w *aiWorld) agentIDAny(t *testing.T, name string) int64 {
	t.Helper()
	ps, err := w.st.ListPrincipals(context.Background(), store.PrincipalFilter{Kind: store.PrincipalAgent, State: schema.PrincipalStateDisabled, NamePrefix: name, Limit: 10}, time.Now())
	if err != nil || len(ps) != 1 {
		t.Fatalf("disabled agent %q = %v, %v", name, ps, err)
	}
	return ps[0].ID
}

// revoke is the disable of §4.5: the credential stops resolving on the
// running server's handle, the grant is cleared, the audit says system.
func TestAIAdminRevoke(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	stdout, _, err := w.create(t, "robo")
	if err != nil {
		t.Fatal(err)
	}
	_, e := entryOf(t, stdout)
	tok := tokenOf(t, e)
	id := w.agentID(t, "robo")
	if _, err := w.st.ResolveCredential(ctx, tok); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runAI(t, "revoke", "--data", w.dir, fmt.Sprint(id))
	if err != nil || out != "" || !strings.Contains(errOut, "revoked") {
		t.Fatalf("revoke = %q, %q, %v", out, errOut, err)
	}
	if _, err := w.st.ResolveCredential(ctx, tok); !errors.Is(err, store.ErrCredentialInvalid) {
		t.Errorf("the revoked credential still resolves: %v", err)
	}
	m, err := w.st.AgentManifestByPrincipal(ctx, id)
	if err != nil || len(m.Permissions) != 0 || m.Admin != nil {
		t.Errorf("manifest after revoke = %+v, %v", m, err)
	}
	var disabled, revoked bool
	for _, ev := range w.audit(t) {
		switch ev.Action {
		case "principal_disabled":
			disabled = ev.Actor == "system"
		case store.AuditAIAdminRevoked:
			revoked = ev.Actor == "system"
		}
	}
	if !disabled || !revoked {
		t.Errorf("audit as system: principal_disabled=%v ai_admin_revoked=%v", disabled, revoked)
	}
	// A second revoke has nothing to do.
	if _, _, err := runAI(t, "revoke", "--data", w.dir, fmt.Sprint(id)); err == nil || !strings.Contains(err.Error(), "already disabled") {
		t.Errorf("second revoke = %v", err)
	}
}

func TestAIAdminRevokeRefusals(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	plain, err := w.st.CreatePrincipal(ctx, store.PrincipalAgent, "plain")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"revoke", "--data", w.dir}, "principal id"},
		{"two ids", []string{"revoke", "--data", w.dir, "1", "2"}, "principal id"},
		{"a name", []string{"revoke", "--data", w.dir, "robo"}, "not a principal id"},
		{"nobody", []string{"revoke", "--data", w.dir, "9999"}, "no principal has id"},
		{"an operator", []string{"revoke", "--data", w.dir, fmt.Sprint(w.root.ID)}, "not an agent"},
		{"an agent with no grant", []string{"revoke", "--data", w.dir, fmt.Sprint(plain.ID)}, "no administrative grant"},
		{"no data", []string{"revoke", fmt.Sprint(plain.ID)}, "--data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := w.counts(t)
			out, _, err := runAI(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || out != "" {
				t.Errorf("error %v, stdout %q, want a refusal containing %q", err, out, tt.want)
			}
			if after := w.counts(t); after != before {
				t.Errorf("a refusal changed the store: %s -> %s", before, after)
			}
		})
	}
}

func TestMCPEntryName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"conch", "conch-conch"},
		{"Alpha", "conch-alpha"},
		{"Alpha Chat", "conch-alpha-chat"},
		{"a.b_c", "conch-a-b-c"},
		{"gank-bears", "conch-gank-bears"},
	}
	for _, tt := range tests {
		if got := mcpEntryName(tt.in); got != tt.want {
			t.Errorf("mcpEntryName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAIAdminDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runAIAdmin([]string{"help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "conchd ai-admin create") {
		t.Errorf("help = %q, %v", out.String(), err)
	}
	if err := run([]string{"ai-admin"}); err == nil || !strings.Contains(err.Error(), "no command") {
		t.Errorf("run ai-admin = %v", err)
	}
}

// TestAIAdminCreateRefusesATracedShell: an exported SHELLOPTS with xtrace
// would echo the token; nothing is written.
func TestAIAdminCreateRefusesATracedShell(t *testing.T) {
	t.Setenv("SHELLOPTS", "braceexpand:hashall:xtrace")
	var stdout, stderr strings.Builder
	err := runAIAdmin([]string{"create", "--data", t.TempDir(), "--origin", "https://conch.example", "helper"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "set -x") || stdout.Len() != 0 {
		t.Fatalf("create under xtrace: err = %v, stdout %q", err, stdout.String())
	}
}

// failWriter fails every write, as a stdout whose reader went away does once
// SIGPIPE is ignored.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestAIAdminCreateUnprintedEntryDisables (security review of #1341): when
// the entry cannot be written, the agent is disabled, so no live credential
// is left that nobody holds.
func TestAIAdminCreateUnprintedEntryDisables(t *testing.T) {
	w := newAIWorld(t, true)
	var stderr strings.Builder
	err := runAIAdmin([]string{"create", "--data", w.dir, "--origin", aiOrigin, "robo"}, failWriter{}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "disabled again") {
		t.Fatalf("create with a broken stdout: err = %v", err)
	}
	p, perr := w.st.PrincipalByID(context.Background(), w.agentID(t, "robo"))
	if perr != nil || p.DisabledAt == nil {
		t.Fatalf("the agent after an unprinted entry = %+v, %v; want disabled", p, perr)
	}
	if strings.Contains(err.Error()+stderr.String(), "conch_") {
		t.Error("a token is in the error or on stderr")
	}
}

// TestAIAdminCreateNamesExistingDeciders: an existing approval channel's
// deciders are named on stderr.
func TestAIAdminCreateNamesExistingDeciders(t *testing.T) {
	w := newAIWorld(t, true)
	ctx := context.Background()
	wren, err := w.st.CreatePrincipal(ctx, store.PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := w.st.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.AddChannelMember(ctx, "system", ch.ID, wren.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := w.create(t, "--channel", "ops", "robo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, `"wren"`) || !strings.Contains(stderr, "will decide the agent's approvals") {
		t.Errorf("stderr does not name wren as a decider:\n%s", stderr)
	}
}

// TestParseSetupOriginRefusesShellCharacters (security review of #1341).
func TestParseSetupOriginRefusesShellCharacters(t *testing.T) {
	for _, bad := range []string{"https://$(id).example", "https://a;id.example", "https://a&b.example", "https://a'b.example", "https://a\"b.example", "https://host:99999", "https://host:0"} {
		if _, err := parseSetupOrigin(bad); err == nil {
			t.Errorf("parseSetupOrigin(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"https://alpha.gankbears.com", "https://localhost:8443", "https://192.0.2.1", "https://[2001:db8::1]:8443"} {
		if _, err := parseSetupOrigin(good); err != nil {
			t.Errorf("parseSetupOrigin(%q): %v", good, err)
		}
	}
}
