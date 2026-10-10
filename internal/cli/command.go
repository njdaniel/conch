package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/njdaniel/conch/pkg/schema"
)

const defaultServer = "http://127.0.0.1:8080"

// Run dispatches a conch command using the supplied standard output streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) error {
	return RunWithStdin(ctx, args, os.Stdin, stdout, stderr, version)
}

// RunWithStdin is Run with an explicit standard input, which `conch login`
// reads the token from.
func RunWithStdin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, version string) error {
	if len(args) == 0 {
		Usage(stderr)
		return errors.New("cli: no command given")
	}
	switch args[0] {
	case "send":
		return runSend(ctx, args[1:], stderr)
	case "tail":
		return runTail(ctx, args[1:], stdout, stderr)
	case "nets":
		return runNets(ctx, args[1:], stdout, stderr)
	case "voice":
		return runVoice(ctx, args[1:], stdout, stderr)
	case "approvals":
		return runApprovals(ctx, args[1:], stdout, stderr)
	case "approve":
		return runApprovalsDecision(ctx, args[1:], stdout, stderr, "approve")
	case "reject":
		return runApprovalsDecision(ctx, args[1:], stdout, stderr, "reject")
	case "login":
		return runLogin(ctx, args[1:], stdin, stdout, stderr)
	case "logout":
		return runLogout(args[1:], stderr)
	case "whoami":
		return runWhoAmI(ctx, args[1:], stdout, stderr)
	case "version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "-h", "--help", "help":
		Usage(stdout)
		return nil
	default:
		Usage(stderr)
		return fmt.Errorf("cli: unknown command %q", args[0])
	}
}

// Usage writes the command-line help text.
func Usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `conch — the Conch command-line client

Usage:
  conch send [--server <url>] [--author <id>] [--net <name> | --to <id>[,<id>...]] <channel> <text>
  conch tail [--server <url>] <channel>
  conch nets list [--server <url>] <channel>
  conch nets create [--server <url>] <channel> <name>
  conch nets archive [--server <url>] <channel> <name>
  conch nets add [--server <url>] <channel> <name> <principal-id> [--monitor]
  conch nets remove [--server <url>] <channel> <name> <principal-id>
  conch voice status [--server <url>] [--watch] <channel>
  conch approvals list [--server <url>]
  conch approve [flags] <id>
  conch reject [flags] <id>
  conch login [--server <url>]     (reads the token from stdin: conch login < tokenfile)
  conch logout [--server <url>]
  conch whoami [--server <url>]
  conch version

Scope:
  send with no flag posts to the whole channel. --net posts to the named net;
  --to whispers to the listed principals (recorded in the audit log). Giving
  both is an error. tail marks scoped messages [net:<name>] or [whisper:<id>,<id>];
  a message body that itself starts with [ is printed as \[.
  voice status shows who is connected to a channel's voice and who is talking;
  it only reads presence and never joins. One line per participant, in
  principal id order:
    <id> <name|-> <talking|quiet> <joined-at, RFC 3339 UTC>
  (a room narrower than the channel adds a fifth field naming it, net:<id>)
  The name is shown for yourself only (the API tells a non-operator no one
  else's) and is "-" when unknown; a name with spaces or odd characters is
  double-quoted and escaped. An empty room prints "nobody is connected" and
  exits 0; voice not configured or unavailable prints one line and exits
  nonzero. --watch prints "--- <time>" and then the state on every change,
  until interrupted (exit 0).

Environment:
  CONCH_SERVER  server URL (default http://127.0.0.1:8080)
  CONCH_TOKEN   bearer token; overrides the stored login
  CONCH_AUTHOR  author ID (deprecated; with a login the server decides who you are)
  CONCH_CHANNELS optional comma-separated TUI channel override (default: all channels from the server)
`)
}

// whisperNotice is printed on stderr after a whisper is sent: a whisper is
// discretion, not secrecy, and users should not mistake it for the latter.
const whisperNotice = "note: whispers are recorded in the audit log"

func runSend(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlagSet("send", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	author := fs.String("author", os.Getenv("CONCH_AUTHOR"), "message author ID")
	netName := fs.String("net", "", "send to the named net instead of the whole channel")
	to := fs.String("to", "", "whisper to these principal IDs (comma-separated) instead of the whole channel")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: send: %w", err)
	}
	if fs.NArg() != 2 {
		return errors.New("cli: send: expected <channel> <text>")
	}
	// Scope mistakes are the classic failure of this feature, so every flag
	// problem is rejected before the first request leaves the machine. A flag
	// counts as given even when its value is empty: `--net "$NET"` with NET
	// unset must fail, not post to the whole channel.
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var whisper []int64
	switch {
	case given["net"] && given["to"]:
		return errors.New("cli: send: --net and --to cannot be used together")
	case given["net"]:
		if err := schema.ValidateNetName(*netName); err != nil {
			return fmt.Errorf("cli: send: --net: %w", err)
		}
	case given["to"]:
		ids, err := parsePrincipalIDs(*to)
		if err != nil {
			return fmt.Errorf("cli: send: --to: %w", err)
		}
		whisper = ids
	}
	channel := fs.Arg(0)
	client, hasCredential, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	authorID, err := resolveAuthor(ctx, client, hasCredential, *author, "send", stderr)
	if err != nil {
		return err
	}
	var audience *schema.Audience
	switch {
	case given["net"]:
		netID, err := lookupNetID(ctx, client, channel, *netName)
		if err != nil {
			return err
		}
		audience = &schema.Audience{Kind: schema.AudienceKindNet, NetID: netID}
	case whisper != nil:
		audience = &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: whisper}
	}
	if _, err := client.PostMessageV2(ctx, channel, authorID, fs.Arg(1), audience); err != nil {
		return err
	}
	if whisper != nil {
		_, _ = fmt.Fprintln(stderr, whisperNotice)
	}
	return nil
}

// parsePrincipalIDs reads a comma-separated list of positive, distinct
// principal ids. Spaces around an id are allowed ("3, 5"); an empty element
// is not.
func parsePrincipalIDs(list string) ([]int64, error) {
	var ids []int64
	seen := make(map[int64]bool)
	for _, field := range strings.Split(list, ",") {
		field = strings.TrimSpace(field)
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a positive principal id", field)
		}
		if seen[id] {
			return nil, fmt.Errorf("principal id %d is listed twice", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	audience := schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}
	if err := audience.Validate(); err != nil {
		return nil, err
	}
	return ids, nil
}

// lookupNetID resolves a net name to its id from the caller's net list.
func lookupNetID(ctx context.Context, client *Client, channel, name string) (int64, error) {
	nets, err := client.ListNets(ctx, channel)
	if err != nil {
		return 0, err
	}
	for _, n := range nets.Nets {
		if n.Name == name {
			return n.ID, nil
		}
	}
	return 0, fmt.Errorf("cli: send: no net %q in channel %q", name, channel)
}

// scopeMarker is the prefix that makes a message's audience visible on every
// output line; a channel-wide message has none. Net names are resolved from
// names where the caller could, and fall back to the net id. Only a marker
// may open the text with "[": tailBody escapes a body that does.
func scopeMarker(audience *schema.Audience, names map[int64]string) string {
	if audience == nil {
		return ""
	}
	switch audience.Kind {
	case schema.AudienceKindNet:
		if name, ok := names[audience.NetID]; ok {
			return "[net:" + name + "] "
		}
		return "[net:" + strconv.FormatInt(audience.NetID, 10) + "] "
	case schema.AudienceKindPrincipals:
		ids := make([]string, len(audience.PrincipalIDs))
		for i, id := range audience.PrincipalIDs {
			ids[i] = strconv.FormatInt(id, 10)
		}
		return "[whisper:" + strings.Join(ids, ",") + "] "
	default:
		// An audience kind this client does not know is still scoped; never
		// print it as if it were channel-wide.
		return "[" + string(audience.Kind) + "] "
	}
}

// tailBody is a message body as tail prints it: on one line, and never
// starting with a bare "[". A body is written by its author, and without the
// escape a channel-wide message "[whisper:3,7] ..." would read as a whisper.
// Backslash is already the escape character here, so "\[" is unambiguous.
func tailBody(body string) string {
	body = strings.NewReplacer("\\", "\\\\", "\r", "\\r", "\n", "\\n").Replace(body)
	if strings.HasPrefix(body, "[") {
		return "\\" + body
	}
	return body
}

func runTail(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("tail", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: tail: %w", err)
	}
	if fs.NArg() != 1 {
		return errors.New("cli: tail: expected <channel>")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	// Net names are resolved once; a net the caller cannot list, or one
	// created after the tail started, is shown by id.
	names := make(map[int64]string)
	nets, err := client.ListNets(ctx, fs.Arg(0))
	if errors.Is(err, ErrUnauthenticated) {
		return err
	}
	if err == nil {
		for _, n := range nets.Nets {
			names[n.ID] = n.Name
		}
	}
	err = client.SubscribeV2(ctx, fs.Arg(0), func(message schema.MessageV2) error {
		_, writeErr := fmt.Fprintf(stdout, "%s %d %s%s\n", message.CreatedAt.Time().Format(time.RFC3339Nano), message.AuthorID, scopeMarker(message.Audience, names), tailBody(message.Body))
		return writeErr
	})
	if websocket.CloseStatus(err) == websocket.StatusGoingAway {
		_, _ = fmt.Fprintln(stderr, "conch: server shutting down")
		return nil
	}
	// Interrupting a tail is how a user stops it; that is not a failure.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runNets(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: nets requires a subcommand (list, create, archive, add, remove)")
	}
	switch args[0] {
	case "list", "create", "archive", "add", "remove":
		return runNetsSubcommand(ctx, args[0], args[1:], stdout, stderr)
	default:
		return fmt.Errorf("cli: unknown nets subcommand %q", args[0])
	}
}

// netsArity names the positional arguments of each nets subcommand.
var netsArity = map[string][]string{
	"list":    {"channel"},
	"create":  {"channel", "name"},
	"archive": {"channel", "name"},
	"add":     {"channel", "name", "principal-id"},
	"remove":  {"channel", "name", "principal-id"},
}

func runNetsSubcommand(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("nets "+verb, stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	var monitor *bool
	if verb == "add" {
		monitor = fs.Bool("monitor", false, "add as a monitor (listens only) instead of a member")
	}
	// Flags may follow the positionals (`nets add c n 5 --monitor`), which the
	// flag package alone does not allow.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("cli: nets %s: %w", verb, err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	want := netsArity[verb]
	if len(positional) != len(want) {
		return fmt.Errorf("cli: nets %s: expected <%s>", verb, strings.Join(want, "> <"))
	}
	channel := positional[0]
	var principalID int64
	if len(want) == 3 {
		id, err := strconv.ParseInt(positional[2], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("cli: nets %s: principal-id must be a positive integer", verb)
		}
		principalID = id
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	switch verb {
	case "list":
		nets, err := client.ListNets(ctx, channel)
		if err != nil {
			return err
		}
		for _, n := range nets.Nets {
			_, _ = fmt.Fprintln(stdout, formatNet(n))
		}
		return nil
	case "create":
		n, err := client.CreateNet(ctx, channel, positional[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "created net %s (id %d) in %s\n", n.Name, n.ID, channel)
		return err
	case "archive":
		if err := client.ArchiveNet(ctx, channel, positional[1]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "archived net %s in %s\n", positional[1], channel)
		return err
	case "add":
		role := schema.NetRoleMember
		if *monitor {
			role = schema.NetRoleMonitor
		}
		if err := client.PutNetMember(ctx, channel, positional[1], principalID, role); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "added %d to net %s as %s\n", principalID, positional[1], role)
		return err
	default: // remove
		if err := client.RemoveNetMember(ctx, channel, positional[1], principalID); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "removed %d from net %s\n", principalID, positional[1])
		return err
	}
}

// formatNet renders a net as `<name>  <id>:<role> ...`, members in the order
// the server sent them.
func formatNet(n schema.NetV1) string {
	if len(n.Members) == 0 {
		return n.Name + "  (empty)"
	}
	members := make([]string, len(n.Members))
	for i, m := range n.Members {
		members[i] = strconv.FormatInt(m.PrincipalID, 10) + ":" + string(m.Role)
	}
	return n.Name + "  " + strings.Join(members, " ")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func serverEnvOr() string {
	if value := os.Getenv("CONCH_SERVER"); value != "" {
		return value
	}
	return defaultServer
}

func runApprovals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: approvals requires a subcommand (e.g. list)")
	}
	switch args[0] {
	case "list":
		return runApprovalsList(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("cli: unknown approvals subcommand %q", args[0])
	}
}

func runApprovalsList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("approvals list", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: approvals list: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: approvals list takes no positional arguments")
	}

	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}

	resp, err := client.ListApprovals(ctx)
	if err != nil {
		return err
	}

	if len(resp.Approvals) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(stdout, "%-6s | %-12s | %-20s | %-20s | %s\n", "ID", "STATE", "DEADLINE", "REQUESTER", "TITLE")
	_, _ = fmt.Fprintln(stdout, strings.Repeat("-", 90))
	for _, a := range resp.Approvals {
		deadline := a.Deadline.Time().Format(time.RFC3339)
		_, _ = fmt.Fprintf(stdout, "%-6d | %-12s | %-20s | %-20d | %s\n", a.ID, a.State, deadline, a.RequesterID, a.Title)
	}
	return nil
}

func runApprovalsDecision(ctx context.Context, args []string, stdout, stderr io.Writer, defaultOption string) error {
	fs := newFlagSet(defaultOption, stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	author := fs.String("author", os.Getenv("CONCH_AUTHOR"), "human principal ID")
	reason := fs.String("reason", "", "reason for decision")
	optionID := fs.String("option", defaultOption, "option ID to select")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: %s: %w", defaultOption, err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("cli: %s: expected <id>", defaultOption)
	}

	approvalID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || approvalID <= 0 {
		return fmt.Errorf("cli: %s: id must be a positive integer", defaultOption)
	}

	if *reason == "" {
		return fmt.Errorf("cli: %s: --reason is required", defaultOption)
	}

	client, hasCredential, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	principalID, err := resolveAuthor(ctx, client, hasCredential, *author, defaultOption, stderr)
	if err != nil {
		return err
	}

	resp, err := client.CastDecision(ctx, approvalID, schema.CastDecisionRequestV1{
		PrincipalID: principalID,
		OptionID:    *optionID,
		Reason:      *reason,
	})
	if err != nil {
		// Rewrite into a single clear message rather than writing to stderr
		// directly: cmd/conch's Run wrapper already prints the returned
		// error, so writing here as well would print it twice.
		if strings.Contains(err.Error(), "invalid_state") || strings.Contains(err.Error(), "terminal") {
			return fmt.Errorf("approval %d is no longer open", approvalID)
		}
		if strings.Contains(err.Error(), "approval_not_found") || strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("approval %d not found", approvalID)
		}
		return err
	}

	if resp.Resolution != nil {
		_, _ = fmt.Fprintf(stdout, "conch: approval %d resolved (%s)\n", approvalID, resp.State)
	} else {
		_, _ = fmt.Fprintf(stdout, "conch: approval %d decision recorded (state: %s)\n", approvalID, resp.State)
	}

	return nil
}

// NewAuthClient builds a client for server that carries the stored or
// CONCH_TOKEN credential when there is one. The bool reports whether a
// credential is in use.
func NewAuthClient(server string) (*Client, bool, error) {
	client, err := NewClient(server, nil)
	if err != nil {
		return nil, false, err
	}
	token, err := LoadToken(server)
	if err != nil {
		return nil, false, err
	}
	client.WithToken(token)
	return client, token != "", nil
}

// resolveAuthor decides which principal an action is performed as. With a
// credential the server's whoami answer is authoritative and --author may only
// repeat it; without one the legacy --author / CONCH_AUTHOR rules apply.
func resolveAuthor(ctx context.Context, client *Client, hasCredential bool, flagValue, command string, stderr io.Writer) (int64, error) {
	var asserted int64
	if flagValue != "" {
		id, err := strconv.ParseInt(flagValue, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("cli: %s: author must be a positive integer", command)
		}
		asserted = id
	}
	if !hasCredential {
		if asserted == 0 {
			return 0, fmt.Errorf("cli: %s: --author (or CONCH_AUTHOR) is required", command)
		}
		return asserted, nil
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return 0, err
	}
	if asserted != 0 {
		if asserted != who.ID {
			return 0, fmt.Errorf("cli: %s: --author %d does not match your login (principal %d)", command, asserted, who.ID)
		}
		_, _ = fmt.Fprintln(stderr, "warning: --author is deprecated; identity comes from your login")
	}
	return who.ID, nil
}

func runLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("login", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: login: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: login takes no positional arguments; the token is read from stdin")
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	token, err := readToken(ctx, stdin, stderr)
	if err != nil {
		return err
	}
	who, err := client.WithToken(token).WhoAmI(ctx)
	if errors.Is(err, ErrUnauthenticated) {
		return errors.New("login failed: token rejected (or the server is not running with --auth required)")
	}
	if err != nil {
		return err
	}
	if err := SaveToken(*server, token); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "logged in to %s as %s (%s)\n", client.Server(), who.Name, who.Role)
	return err
}

// Seams for the terminal branch of readToken, so tests can drive it without a
// real terminal. isTerminalFD reports whether fd is a terminal; readSecret
// reads one line from it with echo off, and must restore the terminal before it
// returns.
var (
	isTerminalFD = term.IsTerminal
	readSecret   = readSecretNoEcho
)

// readSecretNoEcho reads a line from the terminal fd with echo disabled.
// term.ReadPassword restores the terminal when it returns, but it does not
// notice an interrupt: main turns SIGINT into a context cancel, so Ctrl-C would
// be swallowed and the prompt would hang. On cancel the saved state is put back
// here and the blocked read is abandoned; the process is about to exit, so the
// goroutine does not outlive anything that matters.
//
// The abandoned goroutine can still write the terminal state once: turning
// echo off is ReadPassword's first act. If the cancel arrived before that
// happened, restoring at once would be undone a moment later and the user's
// shell would be left without echo. So the cancel branch waits until the
// goroutine has reached ReadPassword, allows it a moment to apply the change,
// and only then restores. A context that is already cancelled never starts
// the read at all.
func readSecretNoEcho(ctx context.Context, fd int) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		secret, err := term.ReadPassword(fd)
		done <- result{secret, err}
	}()
	select {
	case r := <-done:
		return r.secret, r.err
	case <-ctx.Done():
		select {
		case <-started:
		case <-time.After(echoSettle * 10):
		}
		time.Sleep(echoSettle)
		_ = term.Restore(fd, state)
		return nil, ctx.Err()
	}
}

// echoSettle is how long the cancel branch of readSecretNoEcho lets the reader
// goroutine apply its echo-off before restoring the terminal over it. The
// change is a single ioctl made immediately after the goroutine starts.
const echoSettle = 20 * time.Millisecond

// readToken reads one line from stdin. On a terminal it prompts on stderr and
// reads without echo; it never falls back to reading with echo on, because the
// token is the whole credential and would land in scrollback and recordings.
func readToken(ctx context.Context, stdin io.Reader, stderr io.Writer) (string, error) {
	var line string
	if f, ok := stdin.(*os.File); ok && isTerminalFD(int(f.Fd())) { //nolint:gosec // fds fit in int
		_, _ = fmt.Fprint(stderr, "Token: ")
		secret, err := readSecret(ctx, int(f.Fd())) //nolint:gosec // fds fit in int
		// The user's Enter was not echoed either, so end the prompt line.
		_, _ = fmt.Fprintln(stderr)
		switch {
		case errors.Is(err, io.EOF):
			// Ctrl-D on an empty line: same as an empty stdin.
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			return "", fmt.Errorf("cli: login: cannot read the token without echo (%w); use 'conch login < tokenfile'", err)
		}
		line = string(secret)
	} else {
		var err error
		line, err = bufio.NewReader(io.LimitReader(stdin, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("cli: login: read token: %w", err)
		}
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("cli: login: no token provided on stdin")
	}
	return token, nil
}

func runLogout(args []string, stderr io.Writer) error {
	fs := newFlagSet("logout", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: logout: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: logout takes no positional arguments")
	}
	return DeleteToken(*server)
}

func runWhoAmI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("whoami", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: whoami: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: whoami takes no positional arguments")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "id: %d\nkind: %s\nname: %s\nrole: %s\n", who.ID, who.Kind, who.Name, who.Role)
	return err
}

// voiceNow is the clock for the "--- <time>" line that opens each --watch
// block; a variable so tests can fix it.
var voiceNow = time.Now

func runVoice(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: voice requires a subcommand (status)")
	}
	switch args[0] {
	case "status":
		return runVoiceStatus(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("cli: unknown voice subcommand %q", args[0])
	}
}

// Sentinel refusals that voice status reports as one line and a nonzero exit.
var (
	errVoiceNotConfigured = errors.New("voice is not configured on this server")
	errVoiceUnavailable   = errors.New("voice is unavailable: the voice server cannot be reached right now")
)

// serverText is an error whose text may have come from the server. The text
// is made safe for one terminal line, and the cause stays reachable to
// errors.Is and errors.As.
type serverText struct {
	msg string
	err error
}

func (e *serverText) Error() string { return e.msg }
func (e *serverText) Unwrap() error { return e.err }

// safeLine makes text from the server fit on one line: anything that is not a
// printable character (newlines, escape and other control codes, bidi
// overrides) becomes a visible \u escape, so it can neither start another
// output line nor drive the terminal.
func safeLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteToASCII(string(r))
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// voiceName is a principal's name as a single field of a presence line:
// "-" when unknown, bare when it is plain printable text without spaces, and
// double-quoted and escaped otherwise, so a line always splits into exactly
// four fields.
func voiceName(name string) string {
	if name == "" {
		return "-"
	}
	plain := name != "-"
	for _, r := range name {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) || r == '"' || r == '\\' {
			plain = false
			break
		}
	}
	if plain {
		return name
	}
	return strconv.QuoteToASCII(name)
}

// writeVoicePresence prints the state of one presence document: one line per
// participant, in principal id order within each room, or one line saying
// nobody is connected. The first four fields are always the same. A room that
// carries a narrower audience than the channel adds a fifth, naming it
// (net:<id>), so its people can never read as being in the channel-wide room;
// V3 servers send only the channel-wide room.
func writeVoicePresence(w io.Writer, channel string, doc schema.VoicePresenceV1, names map[int64]string) error {
	lines := 0
	for _, room := range doc.Rooms {
		scope := ""
		if room.Audience != nil {
			// The marker without its brackets and trailing space: one field.
			scope = " " + strings.ReplaceAll(strings.Trim(scopeMarker(room.Audience, nil), "[] "), " ", "_")
		}
		people := append([]schema.VoiceParticipant(nil), room.Participants...)
		sort.Slice(people, func(i, j int) bool { return people[i].PrincipalID < people[j].PrincipalID })
		for _, p := range people {
			state := "quiet"
			if p.Transmitting {
				state = "talking"
			}
			if _, err := fmt.Fprintf(w, "%d %s %s %s%s\n", p.PrincipalID, voiceName(names[p.PrincipalID]), state, p.JoinedAt.Time().Format(time.RFC3339), scope); err != nil {
				return err
			}
			lines++
		}
	}
	if lines == 0 {
		_, err := fmt.Fprintf(w, "nobody is connected to voice in %s\n", safeLine(channel))
		return err
	}
	return nil
}

// voiceNames resolves principal ids to names with at most one request. The
// API offers a member only whoami (its own name): the channel member list
// carries ids alone and principal manifests are operator-or-self. Any failure
// leaves the map empty, and every participant is then shown by id.
func voiceNames(ctx context.Context, client *Client) map[int64]string {
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return nil
	}
	return map[int64]string{who.ID: who.Name}
}

func runVoiceStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("voice status", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	watch := fs.Bool("watch", false, "follow the presence socket and print on every change")
	// Flags may follow the channel (`voice status general --watch`).
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("cli: voice status: %w", err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return errors.New("cli: voice status: expected <channel>")
	}
	channel := positional[0]
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	if *watch {
		return watchVoice(ctx, client, channel, stdout, stderr)
	}
	doc, err := client.VoicePresence(ctx, channel)
	if err != nil {
		return voiceFailure(err)
	}
	switch {
	case !doc.Configured:
		return errVoiceNotConfigured
	case !doc.Available:
		return errVoiceUnavailable
	}
	var names map[int64]string
	if len(doc.Rooms) > 0 && len(doc.Rooms[0].Participants) > 0 {
		names = voiceNames(ctx, client)
	}
	return writeVoicePresence(stdout, channel, doc, names)
}

// voiceFailure makes a client error safe to print on one line.
func voiceFailure(err error) error {
	return &serverText{msg: safeLine(err.Error()), err: err}
}

// watchVoice follows the presence socket. The first document decides whether
// following makes sense: voice that is not configured will not become
// configured while connected, so that ends in the same error as the snapshot.
// Unavailable, on the other hand, comes and goes: it is printed like any other
// state and the watch carries on.
func watchVoice(ctx context.Context, client *Client, channel string, stdout, stderr io.Writer) error {
	names := voiceNames(ctx, client)
	first := true
	err := client.SubscribeVoicePresence(ctx, channel, func(doc schema.VoicePresenceV1) error {
		if first && !doc.Configured {
			return errVoiceNotConfigured
		}
		first = false
		if _, err := fmt.Fprintf(stdout, "--- %s\n", voiceNow().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		switch {
		case !doc.Configured:
			_, err := fmt.Fprintln(stdout, errVoiceNotConfigured.Error())
			return err
		case !doc.Available:
			_, err := fmt.Fprintln(stdout, errVoiceUnavailable.Error())
			return err
		}
		return writeVoicePresence(stdout, channel, doc, names)
	})
	if errors.Is(err, errVoiceNotConfigured) {
		return errVoiceNotConfigured
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure:
		// The server ended the stream in an orderly way: nothing failed.
		return nil
	case websocket.StatusGoingAway:
		_, _ = fmt.Fprintln(stderr, "conch: server shutting down")
		return nil
	case websocket.StatusPolicyViolation:
		return fmt.Errorf("voice presence for %s closed: you are no longer a member of the channel, or your credential is disabled or signed out", safeLine(channel))
	}
	// Interrupting a watch is how a user stops it; that is not a failure.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if err == nil {
		return nil
	}
	return voiceFailure(err)
}
