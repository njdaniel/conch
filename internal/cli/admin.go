package cli

import (
	"bytes"
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

	"github.com/njdaniel/conch/pkg/schema"
)

// maxManifestInput caps what `manifest put` reads, as the server caps a
// request body, so a pipe that never ends cannot fill memory.
const maxManifestInput = 1 << 20

// adminOpts holds the flags of every admin command. A command binds only its
// own.
type adminOpts struct {
	server    string
	asJSON    bool
	kind      string
	label     string
	expiresAt string
	file      string
}

// adminRun is what a command's run function gets once its arguments are
// checked and the client is built.
type adminRun struct {
	client *Client
	opts   *adminOpts
	// pos are the positional arguments; id is the one named by idAt, parsed.
	pos    []string
	id     int64
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// adminCommand is one `conch admin <group> <verb>`.
type adminCommand struct {
	group, verb string
	// args names the positional arguments, in order.
	args []string
	// idAt is the 1-based position of the argument that must be a positive
	// integer id, or 0 for none.
	idAt int
	// flags is the command's own flags as the usage line shows them.
	flags string
	// required lists the flags that must be given.
	required []string
	bind     func(fs *flag.FlagSet, o *adminOpts)
	run      func(ctx context.Context, r *adminRun) error
}

// adminCommands is the whole of `conch admin`, in the order the usage line
// lists the groups.
var adminCommands = []adminCommand{
	{group: "channel", verb: "create", args: []string{"name"}, run: adminChannelCreate},
	{group: "principal", verb: "create", args: []string{"name"}, flags: "--kind human|agent ", required: []string{"kind"},
		bind: func(fs *flag.FlagSet, o *adminOpts) { fs.StringVar(&o.kind, "kind", "", "human or agent") },
		run:  adminPrincipalCreate},
	{group: "principal", verb: "disable", args: []string{"id"}, idAt: 1, run: adminPrincipalDisable},
	{group: "principal", verb: "enable", args: []string{"id"}, idAt: 1, run: adminPrincipalEnable},
	{group: "credential", verb: "issue", args: []string{"principal-id"}, idAt: 1, flags: "--label <label> [--expires-at <RFC3339>] ", required: []string{"label"},
		bind: func(fs *flag.FlagSet, o *adminOpts) {
			fs.StringVar(&o.label, "label", "", "what the credential is for")
			fs.StringVar(&o.expiresAt, "expires-at", "", "RFC 3339 time after which the credential stops working")
		},
		run: adminCredentialIssue},
	{group: "credential", verb: "list", args: []string{"principal-id"}, idAt: 1, run: adminCredentialList},
	{group: "credential", verb: "revoke", args: []string{"credential-id"}, idAt: 1, run: adminCredentialRevoke},
	{group: "member", verb: "add", args: []string{"channel", "principal-id"}, idAt: 2, run: adminMemberAdd},
	{group: "member", verb: "remove", args: []string{"channel", "principal-id"}, idAt: 2, run: adminMemberRemove},
	{group: "manifest", verb: "put", args: []string{"principal-id"}, idAt: 1, flags: "[--file <path>] ",
		bind: func(fs *flag.FlagSet, o *adminOpts) {
			fs.StringVar(&o.file, "file", "", "read the manifest from this file instead of standard input")
		},
		run: adminManifestPut},
	{group: "manifest", verb: "get", args: []string{"principal-id"}, idAt: 1, run: adminManifestGet},
}

const adminGroups = "channel|principal|credential|member|manifest"

// usage is the one line shown for a malformed command.
func (c adminCommand) usage() string {
	line := "conch admin " + c.group + " " + c.verb + " [--server <url>] [--json] " + c.flags
	for _, a := range c.args {
		line += "<" + a + "> "
	}
	return strings.TrimSpace(line)
}

func runAdmin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("cli: admin: usage: conch admin <%s> <subcommand>", adminGroups)
	}
	group := args[0]
	var verbs []string
	for _, c := range adminCommands {
		if c.group == group {
			verbs = append(verbs, c.verb)
		}
	}
	if len(verbs) == 0 {
		return fmt.Errorf("cli: admin: unknown subcommand %q (usage: conch admin <%s> ...)", group, adminGroups)
	}
	if len(args) == 1 {
		return fmt.Errorf("cli: admin %s: usage: conch admin %s <%s>", group, group, strings.Join(verbs, "|"))
	}
	for _, c := range adminCommands {
		if c.group == group && c.verb == args[1] {
			return runAdminCommand(ctx, c, args[2:], stdin, stdout, stderr)
		}
	}
	return fmt.Errorf("cli: admin %s: unknown subcommand %q (usage: conch admin %s <%s> ...)", group, args[1], group, strings.Join(verbs, "|"))
}

func runAdminCommand(ctx context.Context, c adminCommand, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	usageErr := func(format string, a ...any) error {
		return fmt.Errorf("cli: admin %s %s: %s (usage: %s)", c.group, c.verb, fmt.Sprintf(format, a...), c.usage())
	}
	o := &adminOpts{}
	fs := newFlagSet("admin "+c.group+" "+c.verb, io.Discard)
	fs.StringVar(&o.server, "server", serverEnvOr(), "conchd HTTP URL")
	fs.BoolVar(&o.asJSON, "json", false, "print the server's response document")
	if c.bind != nil {
		c.bind(fs, o)
	}
	// Flags may follow the positionals, as they may for `nets`.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return usageErr("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, name := range c.required {
		if !given[name] {
			return usageErr("--%s is required", name)
		}
	}
	if len(pos) != len(c.args) {
		return usageErr("expected <%s>", strings.Join(c.args, "> <"))
	}
	r := &adminRun{opts: o, pos: pos, stdin: stdin, stdout: stdout, stderr: stderr}
	if c.idAt > 0 {
		id, err := strconv.ParseInt(pos[c.idAt-1], 10, 64)
		if err != nil || id <= 0 {
			return usageErr("%s must be a positive integer", c.args[c.idAt-1])
		}
		r.id = id
	}
	client, _, err := NewAuthClient(o.server)
	if err != nil {
		return err
	}
	r.client = client
	return c.run(ctx, r)
}

// printJSON writes v as the --json output: one document and a newline.
func (r *adminRun) printJSON(v any) error {
	return json.NewEncoder(r.stdout).Encode(v)
}

func adminChannelCreate(ctx context.Context, r *adminRun) error {
	ch, err := r.client.CreateChannel(ctx, r.pos[0])
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(schema.CreateChannelResponse{Channel: ch})
	}
	_, err = fmt.Fprintf(r.stdout, "created channel %s (id %d)\n", safeLine(ch.Name), ch.ID)
	return err
}

func adminPrincipalCreate(ctx context.Context, r *adminRun) error {
	req := schema.CreatePrincipalRequest{Kind: schema.PrincipalKind(r.opts.kind), Name: r.pos[0]}
	// The same check the server makes, so a typo in --kind costs no request.
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: admin principal create: %w", err)
	}
	p, err := r.client.CreatePrincipal(ctx, req.Kind, req.Name)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(schema.CreatePrincipalResponse{Principal: p})
	}
	_, err = fmt.Fprintf(r.stdout, "created %s %s (id %d)\n", safeLine(string(p.Kind)), safeLine(p.Name), p.ID)
	return err
}

// The commands that answer 204 have no response document, so --json prints
// nothing for them and the exit status is the result.

func adminPrincipalDisable(ctx context.Context, r *adminRun) error {
	if err := r.client.DisablePrincipal(ctx, r.id); err != nil {
		return err
	}
	return r.say("disabled principal %d\n", r.id)
}

func adminPrincipalEnable(ctx context.Context, r *adminRun) error {
	if err := r.client.EnablePrincipal(ctx, r.id); err != nil {
		return err
	}
	return r.say("enabled principal %d\n", r.id)
}

func adminCredentialRevoke(ctx context.Context, r *adminRun) error {
	if err := r.client.RevokeCredential(ctx, r.id); err != nil {
		return err
	}
	return r.say("revoked credential %d\n", r.id)
}

func adminMemberAdd(ctx context.Context, r *adminRun) error {
	if err := r.client.PutChannelMember(ctx, r.pos[0], r.id); err != nil {
		return err
	}
	return r.say("added %d to %s\n", r.id, safeLine(r.pos[0]))
}

func adminMemberRemove(ctx context.Context, r *adminRun) error {
	if err := r.client.RemoveChannelMember(ctx, r.pos[0], r.id); err != nil {
		return err
	}
	return r.say("removed %d from %s\n", r.id, safeLine(r.pos[0]))
}

// say prints a result line unless --json asked for documents only.
func (r *adminRun) say(format string, a ...any) error {
	if r.opts.asJSON {
		return nil
	}
	_, err := fmt.Fprintf(r.stdout, format, a...)
	return err
}

// adminCredentialIssue prints the token alone on standard output so that
// TOKEN=$(conch admin credential issue ...) works. The token goes nowhere
// else: not to standard error, not into an error.
func adminCredentialIssue(ctx context.Context, r *adminRun) error {
	req := schema.CreateCredentialRequestV1{Label: r.opts.label}
	if r.opts.expiresAt != "" {
		t, err := time.Parse(time.RFC3339, r.opts.expiresAt)
		if err != nil {
			return errors.New("cli: admin credential issue: --expires-at must be an RFC 3339 time such as 2026-12-31T00:00:00Z")
		}
		ts := schema.NewTimestamp(t)
		req.ExpiresAt = &ts
	}
	if err := req.Validate(time.Now()); err != nil {
		return fmt.Errorf("cli: admin credential issue: %w", err)
	}
	resp, err := r.client.CreateCredential(ctx, r.id, req)
	if err != nil {
		return err
	}
	// The token is the one server string printed raw, so it is checked first.
	// The error says nothing of what was received.
	if !wellFormedToken(resp.Token) {
		return errors.New("cli: admin credential issue: the server returned a token in an unexpected form; it was not printed")
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	if _, err := fmt.Fprintln(r.stdout, resp.Token); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.stderr, "credential %d %s\n", resp.Credential.ID, expiryText(resp.Credential.ExpiresAt))
	return err
}

// wellFormedToken reports whether s has the shape of a credential token: the
// prefix, then unpadded base64url to the fixed length. Anything else could
// carry terminal control sequences to the operator's screen.
func wellFormedToken(s string) bool {
	if len(s) != schema.CredentialTokenLength || !strings.HasPrefix(s, schema.CredentialTokenPrefix) {
		return false
	}
	for _, c := range s[len(schema.CredentialTokenPrefix):] {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func expiryText(expiresAt *schema.Timestamp) string {
	if expiresAt == nil {
		return "does not expire"
	}
	return "expires " + formatTimestamp(*expiresAt)
}

func formatTimestamp(ts schema.Timestamp) string {
	return ts.Time().Format("2006-01-02T15:04:05.000Z07:00")
}

// adminCredentialList prints `<id> <active|revoked> <created> <expires|-> <label>`
// per credential. The label is last because it may contain spaces.
func adminCredentialList(ctx context.Context, r *adminRun) error {
	resp, err := r.client.ListCredentials(ctx, r.id)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		if resp.Credentials == nil {
			resp.Credentials = []schema.CredentialV1{}
		}
		return r.printJSON(resp)
	}
	for _, c := range resp.Credentials {
		status, expires := "active", "-"
		if c.RevokedAt != nil {
			status = "revoked"
		}
		if c.ExpiresAt != nil {
			expires = formatTimestamp(*c.ExpiresAt)
		}
		if _, err := fmt.Fprintf(r.stdout, "%d %s %s %s %s\n", c.ID, status, formatTimestamp(c.CreatedAt), expires, safeLine(c.Label)); err != nil {
			return err
		}
	}
	return nil
}

// adminManifestPut reads a PutAgentManifestRequestV1 from --file or standard
// input. Unknown fields are refused: a misspelled "capabilitys" would
// otherwise silently grant nothing. A manifest that fails Validate is refused
// before any request is sent.
func adminManifestPut(ctx context.Context, r *adminRun) error {
	in := r.stdin
	if r.opts.file != "" {
		f, err := os.Open(r.opts.file)
		if err != nil {
			return fmt.Errorf("cli: admin manifest put: open --file: %w", err)
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	data, err := io.ReadAll(io.LimitReader(in, maxManifestInput+1))
	if err != nil {
		return fmt.Errorf("cli: admin manifest put: read manifest: %w", err)
	}
	if len(data) > maxManifestInput {
		return fmt.Errorf("cli: admin manifest put: manifest is larger than %d bytes", maxManifestInput)
	}
	var req schema.PutAgentManifestRequestV1
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Errorf("cli: admin manifest put: manifest is not a valid manifest request: %w", err)
	}
	// More reports false for a stray closing bracket, so ask for the next
	// token: only the end of the input is acceptable after the document.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("cli: admin manifest put: manifest has data after the JSON document")
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: admin manifest put: invalid manifest: %w", err)
	}
	m, err := r.client.PutManifest(ctx, r.id, req)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(schema.PutAgentManifestResponseV1{Manifest: m})
	}
	return r.say("stored manifest for principal %d\n", m.PrincipalID)
}

// adminManifestGet prints `<principal-id> tier <T> capabilities <c,...> channels <id:perm,perm ...> <name>`.
func adminManifestGet(ctx context.Context, r *adminRun) error {
	m, err := r.client.GetManifest(ctx, r.id)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(schema.GetAgentManifestResponseV1{Manifest: m})
	}
	caps := make([]string, len(m.Capabilities))
	for i, c := range m.Capabilities {
		caps[i] = safeLine(string(c))
	}
	grants := make([]string, len(m.Channels))
	for i, g := range m.Channels {
		perms := make([]string, len(g.Permissions))
		for j, p := range g.Permissions {
			perms[j] = safeLine(string(p))
		}
		grants[i] = strconv.FormatInt(g.ChannelID, 10) + ":" + strings.Join(perms, ",")
	}
	_, err = fmt.Fprintf(r.stdout, "%d tier %s capabilities %s channels %s %s\n",
		m.PrincipalID, safeLine(string(m.Tier)), dashIfEmpty(strings.Join(caps, ",")), dashIfEmpty(strings.Join(grants, " ")), safeLine(m.DisplayName))
	return err
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
