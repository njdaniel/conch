package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The AI-administrator host commands (issue #1333, docs/design/ai-administrators.md
// §4.3, §4.4, §4.5): offline, on the data directory, by whoever can run conchd
// there, as the operator commands are. Everything is audited as "system".
//
//	conchd ai-admin create --data <dir> --origin https://<host> [flags] <name>
//	conchd ai-admin revoke --data <dir> <principal id>
//
// create makes an agent principal, its approval channel (private, if it does
// not exist), the seats of the people who decide that channel's approvals, the
// manifest that grants the agent its permissions, and one renewable
// credential, then prints the MCP client entry (§4.4) alone on stdout: the
// token appears there once and nowhere else, not on stderr, not in an error.
// The origin is a flag because conchd is never told its public address.
//
// It refuses when no owner is able to sign in (nobody could decide what the
// agent asks for, and nobody could undo it), and every refusal that can be
// decided from what is already stored is made before anything is written. A
// failure after the agent exists disables it again (the design's rollback)
// and says so.
//
// A running server accepts the new credential at once: the server keeps no
// copy of credentials or manifests. Every request resolves its bearer token
// against the SQLite database (store.ResolveCredential, a read of the
// credentials row), and the store is the same file the server has open in WAL
// mode, so a committed write from this process is visible to the server's
// next read, with nothing to signal or restart.
//
// revoke is §4.5: it disables the principal, which revokes its credentials,
// clears its grant and writes principal_disabled and ai_admin_revoked.

const (
	aiAdminCredentialLabel = "ai-admin"
	// aiAdminDefaultChannel is the approval channel when --channel is not given.
	aiAdminDefaultChannel = "admin"
	// aiAdminDefaultCredentialDays is the credential's renewable lifetime.
	aiAdminDefaultCredentialDays = 90
)

// aiAdminDefaultGrant is what the agent is granted when --grant is not
// given (§3.1: the two with warnings are never granted unless named).
var aiAdminDefaultGrant = []schema.Permission{
	schema.PermManageMembers, schema.PermManageChannels, schema.PermMakeMemberLinks,
}

func aiAdminUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  conchd ai-admin create --data <dir> --origin https://<host> [--channel admin]
                     [--grant <permission,...>] [--require-approval <action,...>]
                     [--decider <principal id>]... [--ttl <duration>]
                     [--run-window <duration>] [--quorum <n>] [--max-pending <n>]
                     [--credential-days 90] [--client json|claude-code]
                     [--name <entry name>] [--server-name <name>] <name>
  conchd ai-admin revoke --data <dir> <principal id>

create makes an AI administrator: an agent that holds the listed permissions
and asks the approval channel's deciders before anything gated. It prints the
MCP client entry on stdout, once: put it in the AI's client settings. It
refuses when no owner is able to sign in. Without --decider, every owner who
can sign in decides in the approval channel.
revoke disables the agent: its credentials, sessions and grant end at once.
`)
}

func runAIAdmin(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		aiAdminUsage(stderr)
		return errors.New("ai-admin: no command given")
	}
	switch args[0] {
	case "create":
		return runAIAdminCreate(args[1:], stdout, stderr)
	case "revoke":
		return runAIAdminRevoke(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		aiAdminUsage(stdout)
		return nil
	default:
		aiAdminUsage(stderr)
		return fmt.Errorf("ai-admin: unknown command %q", args[0])
	}
}

// idListFlag collects repeated --decider values.
type idListFlag []int64

func (l *idListFlag) String() string { return fmt.Sprint([]int64(*l)) }

func (l *idListFlag) Set(v string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("%q is not a principal id", v)
	}
	*l = append(*l, id)
	return nil
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mcpServerEntry and mcpConfig are the JSON that Claude Code reads from
// .mcp.json or its settings (design §4.4). The shape is the client's, not a
// Conch wire shape; type is always printed, since an entry with a url and
// no type is read as stdio.
type mcpServerEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type mcpConfig struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

// mcpEntryName is "conch-<server name>": lowercased, anything but
// [a-z0-9-] replaced by "-" (design §4.4).
func mcpEntryName(serverName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(serverName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "conch-" + b.String()
}

// mcpSnippet renders the client entry for token: the JSON form, or the
// claude mcp add one-liner. It ends with a newline.
func mcpSnippet(client, entryName, origin, token string) (string, error) {
	url := origin + "/mcp"
	auth := "Bearer " + token
	switch client {
	case "json":
		b, err := json.MarshalIndent(mcpConfig{MCPServers: map[string]mcpServerEntry{
			entryName: {Type: "http", URL: url, Headers: map[string]string{"Authorization": auth}},
		}}, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "claude-code":
		return fmt.Sprintf("claude mcp add --transport http --scope user %s %s \\\n  --header \"Authorization: %s\"\n", entryName, url, auth), nil
	default:
		return "", fmt.Errorf("--client must be json or claude-code, got %q", client)
	}
}

func runAIAdminCreate(args []string, stdout, stderr io.Writer) error {
	const cmd = "ai-admin create"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	originFlag := fs.String("origin", "", "the server's public address, https://<host>[:port] (required)")
	channelName := fs.String("channel", aiAdminDefaultChannel, "the approval channel (created private if absent)")
	grantFlag := fs.String("grant", "", "permissions to grant, comma-separated (default manage_members,manage_channels,make_member_links)")
	requireFlag := fs.String("require-approval", "", "actions to gate for this agent, comma-separated")
	var deciders idListFlag
	fs.Var(&deciders, "decider", "principal id to seat as a decider in the approval channel (repeatable; default every owner who can sign in)")
	ttl := fs.Duration("ttl", 0, "deadline of each gated action's approval (default 24h)")
	runWindow := fs.Duration("run-window", 0, "how long after approval the action may still run (default 24h)")
	quorum := fs.Int("quorum", 0, "approvals needed (default 1)")
	maxPending := fs.Int("max-pending", 0, "most open admin approvals (default 8)")
	credDays := fs.Int("credential-days", aiAdminDefaultCredentialDays, "the credential's renewable lifetime in days")
	client := fs.String("client", "json", "client entry format: json or claude-code")
	entryFlag := fs.String("name", "", "the MCP entry's name (default conch-<server name>)")
	serverNameFlag := fs.String("server-name", os.Getenv("CONCHD_SERVER_NAME"), "the server's name, for the entry's name and the names agents may not take (default "+server.DefaultServerName+")")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return fmt.Errorf("%s: give the agent's name, once", cmd)
	}
	if tracedShell() {
		return fmt.Errorf("%s: the shell traces commands (set -x), which would echo the token; run it without; nothing changed", cmd)
	}
	name := positional[0]
	if err := schema.ValidateDisplayName("agent name", name, schema.MaxPrincipalNameLength); err != nil {
		return fmt.Errorf("%s: %w; nothing changed", cmd, err)
	}
	if *originFlag == "" {
		return fmt.Errorf("%s: --origin https://<host>, the server's public address, is required; nothing changed", cmd)
	}
	origin, err := parseSetupOrigin(*originFlag)
	if err != nil {
		return fmt.Errorf("%s: --origin: %w; nothing changed", cmd, err)
	}
	if _, err := mcpSnippet(*client, "x", origin, "x"); err != nil {
		return fmt.Errorf("%s: %w; nothing changed", cmd, err)
	}
	serverName, err := offlineServerName(cmd, *serverNameFlag)
	if err != nil {
		return err
	}
	entryName := *entryFlag
	if entryName == "" {
		entryName = mcpEntryName(serverName)
	} else if !validMCPEntryName(entryName) {
		return fmt.Errorf("%s: --name must be letters, digits, '-' and '_', got %q; nothing changed", cmd, entryName)
	}
	if schema.ValidateDisplayName("channel name", *channelName, schema.MaxChannelNameLength) != nil {
		return fmt.Errorf("%s: --channel %q is not a channel name; nothing changed", cmd, *channelName)
	}
	credLifetime := time.Duration(*credDays) * 24 * time.Hour
	if *credDays < schema.MinCredentialRenewLifetimeSeconds/86400 || *credDays > schema.MaxCredentialRenewLifetimeSeconds/86400 {
		return fmt.Errorf("%s: --credential-days must be %d to %d, got %d; nothing changed", cmd,
			schema.MinCredentialRenewLifetimeSeconds/86400, schema.MaxCredentialRenewLifetimeSeconds/86400, *credDays)
	}

	// The manifest, checked as the store will check it, with a placeholder
	// channel id, before anything is written.
	perms := aiAdminDefaultGrant
	if *grantFlag != "" {
		perms = nil
		for _, p := range splitList(*grantFlag) {
			perms = append(perms, schema.Permission(p))
		}
	}
	admin := schema.AgentAdminV1{
		ApprovalTTLSeconds: int64(*ttl / time.Second),
		RunWindowSeconds:   int64(*runWindow / time.Second),
		Quorum:             *quorum,
		MaxPending:         *maxPending,
	}
	for _, a := range splitList(*requireFlag) {
		admin.RequireApproval = append(admin.RequireApproval, schema.AdminAction(a))
	}
	req := aiAdminManifest(name, 1, perms, admin)
	if err := req.Validate(); err != nil {
		return fmt.Errorf("%s: %w; nothing changed", cmd, err)
	}

	if err := refuseUnfinishedRestore(cmd, *dataDir); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openExisting(ctx, cmd, *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	st.SetServerName(serverName)

	seats, err := aiAdminDeciders(ctx, st, deciders)
	if err != nil {
		return fmt.Errorf("%s: %w; nothing changed", cmd, err)
	}
	// An existing approval channel is used as it is; an archived one cannot
	// take members, which is known now and not after the agent exists.
	ch, err := st.ChannelByName(ctx, *channelName)
	switch {
	case err == nil && ch.Archived():
		return fmt.Errorf("%s: channel %q is archived; unarchive it or choose another with --channel; nothing changed", cmd, *channelName)
	case err == nil && ch.Visibility != schema.ChannelPrivate:
		_, _ = fmt.Fprintf(stderr, "note: channel %q is not private; the agent's approvals and their reasons are read by everyone who can read it\n", *channelName)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%s: %w", cmd, err)
	}
	snippet, agentID, err := aiAdminCreate(ctx, st, aiAdminPlan{
		cmd: cmd, name: name, channelName: *channelName, channel: ch, req: req, seats: seats,
		credLifetime: credLifetime, client: *client, entryName: entryName, origin: origin,
	}, stderr)
	if err != nil {
		return err
	}
	// The entry is the only copy of the token: if it cannot be written, the
	// agent is disabled again rather than left live with a credential nobody
	// has.
	if _, werr := fmt.Fprint(stdout, snippet); werr != nil {
		if _, derr := st.DisablePrincipal(ctx, "system", agentID); derr != nil {
			return fmt.Errorf("%s: the entry could not be written and the agent (principal %d) could not be disabled again: run conchd ai-admin revoke %d", cmd, agentID, agentID)
		}
		return fmt.Errorf("%s: the entry could not be written, so the agent (principal %d) was disabled again", cmd, agentID)
	}
	return nil
}

// tracedShell reports whether the calling shell said it traces commands
// (an exported SHELLOPTS with xtrace): the printed token would then be
// echoed to wherever the trace goes. It cannot see a shell that does not
// export SHELLOPTS; docs/deploy.md says not to run this under set -x.
func tracedShell() bool {
	for _, o := range strings.Split(os.Getenv("SHELLOPTS"), ":") {
		if o == "xtrace" {
			return true
		}
	}
	return false
}

// validMCPEntryName reports whether n is usable as an MCP entry's key.
func validMCPEntryName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// aiAdminManifest is the manifest an AI administrator is given (§4.1 step
// 4): the approval and admin capabilities, read and post in its one channel,
// the grant, and a budget of 60 admin actions an hour. approvals.request is
// not granted: the server raises its admin approvals itself.
func aiAdminManifest(name string, channelID int64, perms []schema.Permission, admin schema.AgentAdminV1) schema.PutAgentManifestRequestV1 {
	admin.ApprovalChannelID = channelID
	return schema.PutAgentManifestRequestV1{
		DisplayName: name,
		Tier:        schema.AgentTierA,
		Capabilities: []schema.Capability{
			schema.CapabilityMessagesRead, schema.CapabilityMessagesPost,
			schema.CapabilityApprovalsAwait, schema.CapabilityApprovalsCheck, schema.CapabilityAdminAct,
		},
		Channels: []schema.ChannelGrant{{
			ChannelID:   channelID,
			Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost},
		}},
		RateLimits:  []schema.RateLimit{{Capability: schema.CapabilityAdminAct, Max: 60, WindowSeconds: 3600}},
		Permissions: perms,
		Admin:       &admin,
	}
}

// aiAdminDeciders decides who is seated as a decider, read-only. It refuses
// when no owner is able to sign in, whatever --decider says: nobody could
// then answer the agent's approvals or undo its grant. Without --decider the
// seats are the owners who can sign in; with it, exactly the people named,
// each of whom must be an enabled person.
func aiAdminDeciders(ctx context.Context, st *store.Store, named []int64) ([]int64, error) {
	_, passwordOnly, err := st.StoredMode(ctx)
	if err != nil {
		return nil, err
	}
	var owners []int64
	people := map[int64]bool{}
	var before int64
	for {
		page, err := st.ListPrincipals(ctx, store.PrincipalFilter{Kind: store.PrincipalHuman, State: schema.PrincipalStateActive, Before: before, Limit: 200}, time.Now())
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			before = p.ID
			if p.GuestState != "" {
				continue
			}
			people[p.ID] = true
			if !p.Owner || p.Role != schema.RoleOperator {
				continue
			}
			// The same test the lock-out guards apply: a password, and a
			// confirmed authenticator unless the server allows a password alone.
			c, found, err := st.LoginCandidate(ctx, p.Name)
			if err != nil {
				return nil, err
			}
			if found && c.Principal.ID == p.ID && c.Record != "" && (c.TOTPEnrolled || passwordOnly) {
				owners = append(owners, p.ID)
			}
		}
		if len(page) < 200 {
			break
		}
	}
	if len(owners) == 0 {
		return nil, errors.New("no owner is able to sign in, so nobody could decide what the agent asks for; finish an owner's setup first (conchd operator setup)")
	}
	if len(named) == 0 {
		return owners, nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, id := range named {
		if !people[id] {
			return nil, fmt.Errorf("--decider %d is not an enabled person (conch admin principal list shows ids)", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// aiAdminPlan is everything create has decided before it writes.
type aiAdminPlan struct {
	cmd, name, channelName string
	// channel is the existing approval channel, or the zero Channel when
	// create makes it.
	channel      store.Channel
	req          schema.PutAgentManifestRequestV1
	seats        []int64
	credLifetime time.Duration
	client       string
	entryName    string
	origin       string
}

// aiAdminCreate writes the agent, its channel and seats, its manifest and its
// credential, in that order, so no credential exists unless everything else
// does. A failure after the agent was made disables it again. It returns the
// client entry; the token is in nothing else it returns, logs or wraps.
func aiAdminCreate(ctx context.Context, st *store.Store, pl aiAdminPlan, stderr io.Writer) (string, int64, error) {
	const actor = "system"
	agent, err := st.CreatePrincipalAs(ctx, actor, store.PrincipalAgent, pl.name)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		return "", 0, fmt.Errorf("%s: a principal named %q, or a name that looks the same, already exists; nothing changed", pl.cmd, pl.name)
	case errors.Is(err, store.ErrNameInvalid):
		return "", 0, fmt.Errorf("%s: the name has no character a reader can see; nothing changed", pl.cmd)
	case errors.Is(err, store.ErrServerNameReserved):
		return "", 0, serverNameRefusal(pl.cmd, pl.name)
	case errors.Is(err, store.ErrNameReserved):
		return "", 0, fmt.Errorf("%s: names beginning with %s are reserved for hidden accounts; nothing changed", pl.cmd, schema.HiddenPrincipalNamePrefix)
	case err != nil:
		return "", 0, fmt.Errorf("%s: %w", pl.cmd, err)
	}
	fail := func(step string, err error) (string, int64, error) {
		if _, derr := st.DisablePrincipal(ctx, actor, agent.ID); derr != nil {
			return "", 0, fmt.Errorf("%s: %s: %w; the agent %q (principal %d) was made and could not be disabled again (%w): run conchd ai-admin revoke %d",
				pl.cmd, step, err, pl.name, agent.ID, derr, agent.ID)
		}
		return "", 0, fmt.Errorf("%s: %s: %w; the agent %q (principal %d) was disabled again and its name stays taken: choose another", pl.cmd, step, err, pl.name, agent.ID)
	}
	ch := pl.channel
	if ch.ID == 0 {
		if ch, err = st.CreateUnownedChannel(ctx, actor, pl.channelName, schema.ChannelPrivate); err != nil {
			return fail("create the approval channel", err)
		}
	}
	no, yes := false, true
	if _, err := st.PutChannelMember(ctx, actor, ch.ID, agent.ID, 0, &no); err != nil {
		return fail("seat the agent", err)
	}
	for _, id := range pl.seats {
		if _, err := st.PutChannelMember(ctx, actor, ch.ID, id, 0, &yes); err != nil {
			return fail(fmt.Sprintf("seat decider %d", id), err)
		}
	}
	req := pl.req
	req.Channels = []schema.ChannelGrant{{ChannelID: ch.ID, Permissions: pl.req.Channels[0].Permissions}}
	admin := *pl.req.Admin
	admin.ApprovalChannelID = ch.ID
	req.Admin = &admin
	if _, _, err := st.PutAgentManifest(ctx, actor, agent.ID, req); err != nil {
		return fail("write the manifest", err)
	}
	_, token, err := st.CreateRenewableCredential(ctx, actor, agent.ID, aiAdminCredentialLabel, pl.credLifetime)
	if err != nil {
		return fail("issue the credential", err)
	}
	snippet, err := mcpSnippet(pl.client, pl.entryName, pl.origin, token)
	if err != nil {
		return fail("print the entry", err)
	}
	_, _ = fmt.Fprintf(stderr, "AI administrator %q (principal %d) created; approvals go to channel %q, decided by principal(s) %s.\n", pl.name, agent.ID, ch.Name, joinIDs(pl.seats))
	_, _ = fmt.Fprintf(stderr, "The MCP entry %q is on stdout and will not be shown again; its credential renews on use and lasts %d days idle. Put it in the AI's client (claude: --scope user; a project .mcp.json is a file people commit). Deleting the entry elsewhere revokes nothing: conchd ai-admin revoke %d does.\n",
		pl.entryName, int(pl.credLifetime.Hours()/24), agent.ID)
	_, _ = fmt.Fprintln(stderr, "A running server accepts it already: it reads credentials from the database on every request.")
	return snippet, agent.ID, nil
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ", ")
}

func runAIAdminRevoke(args []string, _, stderr io.Writer) error {
	const cmd = "ai-admin revoke"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("%s: give the agent's principal id, once (conch admin principal list shows ids)", cmd)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("%s: %q is not a principal id", cmd, positional[0])
	}
	if *dataDir != "" {
		if err := refuseUnfinishedRestore(cmd, *dataDir); err != nil {
			return err
		}
	}
	ctx := context.Background()
	st, err := openExisting(ctx, cmd, *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	p, err := st.PrincipalByID(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%s: no principal has id %d; nothing changed", cmd, id)
	case err != nil:
		return fmt.Errorf("%s: %w", cmd, err)
	case p.Kind != store.PrincipalAgent:
		return fmt.Errorf("%s: principal %d is not an agent; nothing changed (conchd operator recover handles an operator)", cmd, id)
	case p.DisabledAt != nil:
		return fmt.Errorf("%s: agent %d is already disabled; nothing changed", cmd, id)
	}
	// Only an AI administrator: this command is not a way to disable any agent.
	m, err := st.AgentManifestByPrincipal(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && len(m.Permissions) == 0):
		return fmt.Errorf("%s: agent %d holds no administrative grant; nothing changed (to disable an ordinary agent, conch admin principal disable)", cmd, id)
	case err != nil:
		return fmt.Errorf("%s: %w", cmd, err)
	}
	changed, err := st.DisablePrincipal(ctx, "system", id)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if !changed {
		return fmt.Errorf("%s: agent %d is already disabled; nothing changed", cmd, id)
	}
	_, _ = fmt.Fprintf(stderr, "AI administrator %q (principal %d) revoked: disabled, its credentials ended and its grant cleared. A running server stops accepting it at once on REST and within its re-check interval on open sockets.\n", p.Name, id)
	return nil
}
