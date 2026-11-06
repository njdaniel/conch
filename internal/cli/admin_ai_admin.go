package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin ai-admin` (issue #1332, docs/design/ai-administrators.md §4):
// create, list, show and revoke an AI administrator, an agent principal whose
// manifest grants it operator permissions. Every verb is a composition of
// existing public routes; the server decides who may do what, and the grant
// itself is owner-only there.

const (
	// aiAdminDefaultChannel is the approval channel when --channel is not
	// given (design §4.1).
	aiAdminDefaultChannel = "admin"
	// aiAdminDefaultGrant is what `create` grants when --grant is not given.
	// change_server_settings and read_audit_log are only granted when named.
	aiAdminDefaultGrant = "manage_members,manage_channels,make_member_links"
	// aiAdminDefaultCredentialDays is the renewable credential's idle lifetime.
	aiAdminDefaultCredentialDays = 90
	// aiAdminRateMax and aiAdminRateWindow are the admin.act rate limit the
	// manifest carries: 60 per hour.
	aiAdminRateMax    = 60
	aiAdminRateWindow = 3600

	clientJSON       = "json"
	clientClaudeCode = "claude-code"
)

// aiAdminOpts holds the flags of `create`.
type aiAdminOpts struct {
	channel         string
	grant           string
	requireApproval string
	ttl             string
	runWindow       string
	quorum          int
	maxPending      int
	credentialDays  int64
	client          string
	entryName       string
}

func bindAIAdminCreate(fs *flag.FlagSet, o *adminOpts) {
	a := &o.aiAdmin
	fs.StringVar(&a.channel, "channel", aiAdminDefaultChannel, "the approval channel; created private if absent")
	fs.StringVar(&a.grant, "grant", aiAdminDefaultGrant, "comma-separated permissions to grant")
	fs.StringVar(&a.requireApproval, "require-approval", "", "comma-separated actions to gate with an approval, beyond the server's fixed set")
	fs.StringVar(&a.ttl, "ttl", "", "how long each approval stays open, such as 24h (server default 24h)")
	fs.StringVar(&a.runWindow, "run-window", "", "how long after approval the action may still be run, such as 24h (server default 24h)")
	fs.IntVar(&a.quorum, "quorum", 0, "approvals needed per gated action (server default 1)")
	fs.IntVar(&a.maxPending, "max-pending", 0, "cap on the agent's open admin approvals (server default 8)")
	fs.Int64Var(&a.credentialDays, "credential-days", aiAdminDefaultCredentialDays, "days the renewable credential lasts when unused")
	fs.StringVar(&a.client, "client", clientJSON, "the form of the MCP entry: json or claude-code")
	fs.StringVar(&a.entryName, "entry-name", "", "the MCP entry's name (default conch-<server name>)")
}

// mcpEntryName derives the MCP client entry name from the server's name
// (design §4.4): "conch-" and the name lowercased, with anything but
// [a-z0-9-] replaced by "-". An empty server name is the default, "conch".
func mcpEntryName(serverName string) string {
	if serverName == "" {
		serverName = "conch"
	}
	var b strings.Builder
	b.WriteString("conch-")
	for _, r := range strings.ToLower(serverName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// validEntryName reports whether an --entry-name override is safe to print
// into a JSON key and a shell word.
func validEntryName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !isASCIIAlnum(r) && !strings.ContainsRune("-_.", r) {
			return false
		}
	}
	return true
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// mcpEntry is the JSON entry Claude Code reads (design §4.4). It is a
// client's file format, not a conchd wire shape.
type mcpEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// renderMCPSnippet is the whole of what `create` prints on stdout.
func renderMCPSnippet(client, name, mcpURL, token string) (string, error) {
	header := "Bearer " + token
	switch client {
	case clientJSON:
		doc := struct {
			MCPServers map[string]mcpEntry `json:"mcpServers"`
		}{map[string]mcpEntry{name: {Type: "http", URL: mcpURL, Headers: map[string]string{"Authorization": header}}}}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", errors.New("render the MCP entry")
		}
		return string(out) + "\n", nil
	case clientClaudeCode:
		return "claude mcp add --transport http --scope user " + shellWord(name) + " " + shellWord(mcpURL) +
			" --header " + shellWord("Authorization: "+header) + "\n", nil
	}
	return "", fmt.Errorf("--client must be %s or %s", clientJSON, clientClaudeCode)
}

// shellWord quotes s for a POSIX shell only when it has to be.
func shellWord(s string) string {
	safe := s != ""
	for _, r := range s {
		if !isASCIIAlnum(r) && !strings.ContainsRune("-_./:@%+=", r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// aiAdminRequest builds the manifest request `create` writes, from the flags
// and the approval channel's id, and checks it as the server does.
func aiAdminRequest(o aiAdminOpts, name string, channelID int64) (schema.PutAgentManifestRequestV1, error) {
	var perms []schema.Permission
	for _, p := range splitList(o.grant) {
		perms = append(perms, schema.Permission(p))
	}
	if len(perms) == 0 {
		return schema.PutAgentManifestRequestV1{}, errors.New("cli: admin ai-admin create: --grant names no permission")
	}
	admin := schema.AgentAdminV1{ApprovalChannelID: channelID, Quorum: o.quorum, MaxPending: o.maxPending}
	for _, a := range splitList(o.requireApproval) {
		admin.RequireApproval = append(admin.RequireApproval, schema.AdminAction(a))
	}
	ttl, err := seconds("ttl", o.ttl)
	if err != nil {
		return schema.PutAgentManifestRequestV1{}, fmt.Errorf("cli: admin ai-admin create: %w", err)
	}
	if ttl != nil {
		admin.ApprovalTTLSeconds = *ttl
	}
	window, err := seconds("run-window", o.runWindow)
	if err != nil {
		return schema.PutAgentManifestRequestV1{}, fmt.Errorf("cli: admin ai-admin create: %w", err)
	}
	if window != nil {
		admin.RunWindowSeconds = *window
	}
	req := schema.PutAgentManifestRequestV1{
		DisplayName: name,
		Tier:        schema.AgentTierA,
		Capabilities: []schema.Capability{
			schema.CapabilityMessagesRead, schema.CapabilityMessagesPost,
			schema.CapabilityApprovalsAwait, schema.CapabilityApprovalsCheck, schema.CapabilityAdminAct,
		},
		Channels:    []schema.ChannelGrant{{ChannelID: channelID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}},
		RateLimits:  []schema.RateLimit{{Capability: schema.CapabilityAdminAct, Max: aiAdminRateMax, WindowSeconds: aiAdminRateWindow}},
		Permissions: perms,
		Admin:       &admin,
	}
	if err := req.Validate(); err != nil {
		return schema.PutAgentManifestRequestV1{}, fmt.Errorf("cli: admin ai-admin create: %w", err)
	}
	return req, nil
}

// shellTraceOn reports whether the caller's shell exports a trace option that
// would echo what a command is given. It can only see an exported SHELLOPTS;
// the manual says the rest is the caller's.
func shellTraceOn() bool {
	for _, opt := range strings.Split(os.Getenv("SHELLOPTS"), ":") {
		if opt == "xtrace" {
			return true
		}
	}
	return false
}

// scrub removes token from the text of err, so no error path can carry it.
func scrub(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[token]"))
}

// adminAIAdminCreate makes an AI administrator in the design's order (§4.1)
// and prints its MCP entry, alone, on standard output. Whatever fails after
// the principal exists disables it, so no half-made agent is left holding
// anything.
func adminAIAdminCreate(ctx context.Context, r *adminRun) error {
	const cmd = "cli: admin ai-admin create"
	// A broken pipe on stdout must come back as an error, not kill the
	// process between the credential and its printing: the rollback then
	// disables the agent (security review of #1342).
	signal.Ignore(syscall.SIGPIPE)
	o := r.opts.aiAdmin
	name := r.pos[0]
	if r.opts.asJSON {
		return fmt.Errorf("%s: --json is not supported; the command prints the MCP entry (see --client)", cmd)
	}
	if o.client != clientJSON && o.client != clientClaudeCode {
		return fmt.Errorf("%s: --client must be %s or %s", cmd, clientJSON, clientClaudeCode)
	}
	if o.entryName != "" && !validEntryName(o.entryName) {
		return fmt.Errorf("%s: --entry-name may hold only letters, digits, '-', '_' and '.'", cmd)
	}
	minDays, maxDays := int64(schema.MinCredentialRenewLifetimeSeconds/secondsPerDay), int64(schema.MaxCredentialRenewLifetimeSeconds/secondsPerDay)
	if o.credentialDays < minDays || o.credentialDays > maxDays {
		return fmt.Errorf("%s: --credential-days must be between %d and %d", cmd, minDays, maxDays)
	}
	if shellTraceOn() {
		return fmt.Errorf("%s: the shell is tracing commands (set -x); turn it off, the entry carries a token", cmd)
	}
	// The principal, the channel and the manifest are checked before
	// anything is made.
	pr := schema.CreatePrincipalRequest{Kind: schema.PrincipalAgent, Name: name}
	if err := pr.Validate(); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if _, err := aiAdminRequest(o, name, 1); err != nil {
		return err
	}
	chReq := schema.CreateChannelRequestV1{Name: o.channel, Visibility: schema.ChannelPrivate}
	if err := chReq.Validate(); err != nil {
		return fmt.Errorf("%s: --channel: %w", cmd, err)
	}

	me, err := r.client.WhoAmI(ctx)
	if err != nil {
		return err
	}
	serverName := ""
	if info, err := r.client.JoinInfo(ctx); err == nil {
		serverName = info.ServerName
	}
	entry := o.entryName
	if entry == "" {
		entry = mcpEntryName(serverName)
	}

	// Step 1: the principal.
	agent, err := r.client.CreatePrincipal(ctx, schema.PrincipalAgent, name)
	if err != nil {
		return err
	}
	token := ""
	rollback := func(step string, cause error) error {
		cause = scrub(cause, token)
		if derr := r.client.DisablePrincipal(ctx, agent.ID); derr != nil {
			return fmt.Errorf("%s: %s failed (%w); and disabling the new principal %d failed too (%w): run `conch admin principal disable %d`",
				cmd, step, cause, agent.ID, scrub(derr, token), agent.ID)
		}
		return fmt.Errorf("%s: %s failed (%w); the new principal %d was disabled, nothing is left holding access", cmd, step, cause, agent.ID)
	}
	if _, err := fmt.Fprintf(r.stderr, "created agent %s (id %d)\n", safeLine(agent.Name), agent.ID); err != nil {
		return rollback("reporting progress", err)
	}

	// Step 2: the approval channel, made private if absent.
	channelID, existed, err := findOrCreateChannel(ctx, r.client, o.channel)
	if err != nil {
		return rollback("the approval channel", err)
	}
	// Step 3: the owner decides in it, the agent only watches.
	yes, no := true, false
	if err := r.client.PutChannelMemberDecide(ctx, o.channel, me.ID, &yes); err != nil {
		return rollback("seating you as a decider", err)
	}
	if err := r.client.PutChannelMemberDecide(ctx, o.channel, agent.ID, &no); err != nil {
		return rollback("adding the agent to the approval channel", err)
	}
	// Step 4: the manifest. A non-owner is refused here, by the store.
	req, err := aiAdminRequest(o, name, channelID)
	if err != nil {
		return rollback("building the manifest", err)
	}
	if _, err := r.client.PutManifest(ctx, agent.ID, req); err != nil {
		return rollback("writing the manifest", err)
	}
	// Step 5: the credential, renewable, shown once.
	lifetime := o.credentialDays * secondsPerDay
	cred, err := r.client.CreateCredential(ctx, agent.ID, schema.CreateCredentialRequestV1{Label: "ai-admin", RenewLifetimeSeconds: &lifetime})
	if err != nil {
		return rollback("issuing the credential", err)
	}
	// The error says nothing of what was received.
	if !wellFormedToken(cred.Token) {
		return rollback("reading the credential", errors.New("the server returned a token in an unexpected form; it was not printed"))
	}
	token = cred.Token

	mcpURL := r.client.resolve("mcp")
	mcpURL.User = nil // a base URL's userinfo is never printed into the entry
	snippet, err := renderMCPSnippet(o.client, entry, mcpURL.String(), token)
	if err != nil {
		return rollback("rendering the MCP entry", err)
	}
	if _, err := fmt.Fprint(r.stdout, snippet); err != nil {
		return rollback("printing the MCP entry", errors.New("write standard output"))
	}
	decides := "you decide its approvals"
	if existed {
		// It keeps the deciders it had (security review of #1342).
		decides = "it existed: you and everyone who already decides there decide its approvals (review them in the channel's member list; conch admin member decide --off removes the right)"
	}
	_, err = fmt.Fprintf(r.stderr, "approval channel %s (id %d); %s\ncredential %d, renewed by use; the entry above is shown once\n"+
		"add it with --scope user: a project .mcp.json is a file people commit\n"+
		"deleting the entry on the client revokes nothing; to revoke: conch admin ai-admin revoke %d\n",
		safeLine(o.channel), channelID, decides, cred.Credential.ID, agent.ID)
	return err
}

// findOrCreateChannel returns the id of the live channel called name,
// creating it private when it is absent. It reads every channel (scope=all,
// which an owner holds by manage_channels), so a private channel of that
// name the caller is not in is found, not created twice.
func findOrCreateChannel(ctx context.Context, c *Client, name string) (id int64, existed bool, err error) {
	dir, err := c.ChannelDirectory(ctx, true)
	if err != nil {
		return 0, false, err
	}
	for _, ch := range dir.Channels {
		if ch.Name == name && ch.ArchivedAt == nil {
			return ch.ID, true, nil
		}
	}
	created, err := c.CreateChannelV1(ctx, schema.CreateChannelRequestV1{Name: name, Visibility: schema.ChannelPrivate})
	if err != nil {
		return 0, false, err
	}
	return created.Channel.ID, false, nil
}

// aiAdmin is one agent holding a grant, as `list` reads it.
type aiAdmin struct {
	principal schema.PrincipalAdminV1
	manifest  schema.AgentManifestV1
}

func permissionNames(m schema.AgentManifestV1) string {
	names := make([]string, len(m.Permissions))
	for i, p := range m.Permissions {
		names[i] = safeLine(string(p))
	}
	return strings.Join(names, ",")
}

func adminAIAdminList(ctx context.Context, r *adminRun) error {
	filter := PrincipalListFilter{Kind: string(schema.PrincipalAgent), Limit: schema.MaxListPrincipalsLimit}
	var found []aiAdmin
	for n := 0; ; n++ {
		if n == maxListPages {
			return errors.New("cli: admin ai-admin list: the server's pages do not end")
		}
		page, err := r.client.ListPrincipals(ctx, filter)
		if err != nil {
			return err
		}
		for _, p := range page.Principals {
			m, err := r.client.GetManifest(ctx, p.ID)
			if err != nil {
				if routeMissing(err) || isNotFound(err) {
					continue // an agent with no manifest holds no grant
				}
				return err
			}
			if len(m.Permissions) > 0 {
				found = append(found, aiAdmin{p, m})
			}
		}
		if page.NextBefore == 0 {
			break
		}
		if filter.Before != 0 && page.NextBefore >= filter.Before {
			return errors.New("cli: admin ai-admin list: the server's pages do not move forward")
		}
		filter.Before = page.NextBefore
	}
	if r.opts.asJSON {
		ms := make([]schema.AgentManifestV1, len(found))
		for i, a := range found {
			ms[i] = a.manifest
		}
		return r.printJSON(ms)
	}
	for _, a := range found {
		channel := "-"
		if a.manifest.Admin != nil {
			channel = strconv.FormatInt(a.manifest.Admin.ApprovalChannelID, 10)
		}
		if _, err := fmt.Fprintf(r.stdout, "%d %s approvals-in %s %s %s\n", a.principal.ID, principalState(a.principal), channel,
			permissionNames(a.manifest), safeLine(a.principal.Name)); err != nil {
			return err
		}
	}
	return nil
}

// isNotFound reports a 404 answer carrying an error document.
func isNotFound(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Status == 404
}

// holdsGrant reads the manifest of the principal and refuses one that is not
// an AI administrator, so these verbs do not act on anyone else.
func holdsGrant(ctx context.Context, r *adminRun, verb string) (schema.AgentManifestV1, error) {
	m, err := r.client.GetManifest(ctx, r.id)
	if err != nil {
		if isNotFound(err) || routeMissing(err) {
			return schema.AgentManifestV1{}, fmt.Errorf("cli: admin ai-admin %s: principal %d is not an AI administrator (no manifest)", verb, r.id)
		}
		return schema.AgentManifestV1{}, err
	}
	if len(m.Permissions) == 0 {
		return schema.AgentManifestV1{}, fmt.Errorf("cli: admin ai-admin %s: principal %d is not an AI administrator (its manifest grants no permission)", verb, r.id)
	}
	return m, nil
}

func adminAIAdminShow(ctx context.Context, r *adminRun) error {
	m, err := holdsGrant(ctx, r, "show")
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(schema.GetAgentManifestResponseV1{Manifest: m})
	}
	lines := []string{
		fmt.Sprintf("principal %d %s", m.PrincipalID, safeLine(m.DisplayName)),
		"permissions " + permissionNames(m),
	}
	if a := m.Admin; a != nil {
		lines = append(lines, fmt.Sprintf("approval channel %d ttl %ds run window %ds quorum %d max pending %d",
			a.ApprovalChannelID, a.ApprovalTTLSeconds, a.RunWindowSeconds, a.Quorum, a.MaxPending))
		if len(a.RequireApproval) > 0 {
			acts := make([]string, len(a.RequireApproval))
			for i, x := range a.RequireApproval {
				acts[i] = safeLine(string(x))
			}
			lines = append(lines, "also gated "+strings.Join(acts, ","))
		}
	}
	caps := make([]string, len(m.Capabilities))
	for i, c := range m.Capabilities {
		caps[i] = safeLine(string(c))
	}
	lines = append(lines, "capabilities "+strings.Join(caps, ","))
	for _, l := range lines {
		if _, err := fmt.Fprintln(r.stdout, l); err != nil {
			return err
		}
	}
	return nil
}

func adminAIAdminRevoke(ctx context.Context, r *adminRun) error {
	if _, err := holdsGrant(ctx, r, "revoke"); err != nil {
		return err
	}
	if err := r.client.DisablePrincipal(ctx, r.id); err != nil {
		return err
	}
	return r.say("revoked AI administrator %d: disabled, credentials revoked, sessions ended\n", r.id)
}
