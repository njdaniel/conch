package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The operator host commands (issue #709, docs/design/guests-and-accounts.md
// §5.3): offline, on the data directory, by whoever can run conchd there.
// Each prints a setup token, alone on stdout so a script can capture it, and
// what to do with it on stderr. The token is good once, for ten minutes, and
// only for the restricted setup flow: POST /v1/setup/redeem answers a
// fifteen-minute credential that reaches whoami, the password enrolment,
// the TOTP enrolment and its own sign-out, and nothing else. There is no
// route that does what these do: an operator's authentication is reset only
// on the host.
//
//	conchd operator bootstrap --data <dir> [--keep-existing-credentials] [--server-name <name>] [--link --origin https://<host>] <name>
//	conchd operator setup     --data <dir> [--link --origin https://<host>] <principal id>
//	conchd operator recover   --data <dir> <principal id>
//
// With --link, bootstrap and setup print a setup link,
// https://<host>/app/#/setup/<token>, in place of the bare token (ADR-011
// item 3): once, on stdout, as the token would be. --origin names the
// server's public address, which conchd is never told otherwise. recover
// refuses --link: it is the command that revokes, and its token is typed.
//
// bootstrap makes the first operator as bootstrap-operator does, with a setup
// token in place of a script credential, and like it refuses the server's
// name (--server-name, issue #798). setup gives an existing operator
// whose setup is incomplete (one made by bootstrap-operator, say) a setup
// token and revokes nothing, so their script credentials keep working.
// recover is for an operator who lost a password or an authenticator: it
// revokes every credential of theirs, removes the second factor, ends every
// pending sign-in, link and proof of theirs, and audits operator_recovered,
// then makes the token. A running server notices the revocations at once on
// REST and within its re-check interval on open sockets.

func operatorUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  conchd operator bootstrap --data <dir> [--keep-existing-credentials] [--server-name <name>] [--link --origin https://<host>] <name>
  conchd operator setup     --data <dir> [--link --origin https://<host>] <principal id>
  conchd operator recover   --data <dir> <principal id>

--link prints https://<host>/app/#/setup/<token> in place of the bare token,
once, on stdout; --origin, required with it, is the server's public HTTPS
address. recover does not take --link. The link needs a web client from
v0.1.0-alpha.2 or later; with an older one, give the bare token on the setup page.
`)
}

func runOperator(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		operatorUsage(stderr)
		return errors.New("operator: no command given")
	}
	switch args[0] {
	case "bootstrap":
		return runOperatorBootstrap(args[1:], stdout, stderr)
	case "setup", "recover":
		return runOperatorSetup(args[0], args[1:], stdout, stderr)
	case "-h", "--help", "help":
		operatorUsage(stdout)
		return nil
	default:
		operatorUsage(stderr)
		return fmt.Errorf("operator: unknown command %q", args[0])
	}
}

// parseInterleaved parses fs from args, letting flags come before or after
// the positional arguments, and returns the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// openExisting opens the database in dataDir, refusing to make one: a
// mistyped --data must not look like success.
func openExisting(ctx context.Context, cmd, dataDir string) (*store.Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("%s: --data (or CONCHD_DATA) is required", cmd)
	}
	dbPath := filepath.Join(dataDir, "conch.db")
	if _, err := os.Stat(dbPath); err != nil { // #nosec G703 -- trusted operator path
		return nil, fmt.Errorf("%s: no database at %s; check --data", cmd, dbPath)
	}
	return store.Open(ctx, dbPath)
}

// printSetupToken writes the token alone on stdout and what to do with it on
// stderr. The token appears nowhere else. With an origin (--link) it writes
// the setup link in its place, once, the same way (ADR-011 item 3). Whether
// the authenticator is needed is the server's setting, which these offline
// commands are not told, so the text says both (ADR-012).
func printSetupToken(stdout, stderr io.Writer, name string, t store.SetupToken, origin string) {
	if origin != "" {
		_, _ = fmt.Fprintln(stdout, setupLink(origin, t.Token))
		_, _ = fmt.Fprintf(stderr, "setup link for operator %q (principal %d) printed on stdout; it will not be shown again.\n", name, t.PrincipalID)
		_, _ = fmt.Fprintf(stderr, "It works once, until %s (%d minutes), over HTTPS only: open it in a browser and press the button to redeem it, then set a password and enrol an authenticator app.\n",
			t.ExpiresAt.UTC().Format(time.RFC3339), schema.SetupTokenLifetimeSeconds/60)
		_, _ = fmt.Fprintln(stderr, "The link needs a web client from v0.1.0-alpha.2 or later; with an older one, paste the part after #/setup/ on the setup page as the token.")
	} else {
		_, _ = fmt.Fprintln(stdout, t.Token)
		_, _ = fmt.Fprintf(stderr, "setup token for operator %q (principal %d) printed on stdout; it will not be shown again.\n", name, t.PrincipalID)
		_, _ = fmt.Fprintf(stderr, "It works once, until %s (%d minutes), over HTTPS only: redeem it on the server's setup page, or with POST /v1/setup/redeem, then set a password and enrol an authenticator app.\n",
			t.ExpiresAt.UTC().Format(time.RFC3339), schema.SetupTokenLifetimeSeconds/60)
	}
	_, _ = fmt.Fprintln(stderr, "The authenticator is required unless serve runs with CONCHD_OPERATOR_TOTP=optional or CONCHD_MODE=temporary; there the password completes setup, and the authenticator step may be skipped.")
}

// setupLink is the web client's setup address for token at origin. The token
// is in the fragment, which a browser never sends to a server, and the page
// takes it from the address and clears it at once (ADR-011 item 3).
func setupLink(origin, token string) string { return origin + "/app/#/setup/" + token }

// linkFlags are --link and --origin, for the commands that offer a link.
type linkFlags struct {
	link   *bool
	origin *string
}

func addLinkFlags(fs *flag.FlagSet) linkFlags {
	return linkFlags{
		link:   fs.Bool("link", false, "print a setup link (https://<host>/app/#/setup/<token>) in place of the bare token; needs --origin and a web client from v0.1.0-alpha.2 or later"),
		origin: fs.String("origin", "", "the server's public address for --link, https://<host>[:port]"),
	}
}

// resolve is the origin the link is printed at, "" for no link. --origin
// without --link, or --link without --origin, is refused: conchd has no
// public address of its own to fall back on, and CONCHD_LISTEN is where it
// binds, not where people reach it.
func (l linkFlags) resolve(cmd string) (string, error) {
	switch {
	case !*l.link && *l.origin == "":
		return "", nil
	case !*l.link:
		return "", fmt.Errorf("%s: --origin is only for --link; nothing changed", cmd)
	case *l.origin == "":
		return "", fmt.Errorf("%s: --link needs --origin https://<host>, the server's public address; nothing changed", cmd)
	}
	origin, err := parseSetupOrigin(*l.origin)
	if err != nil {
		return "", fmt.Errorf("%s: --origin: %w; nothing changed", cmd, err)
	}
	return origin, nil
}

// parseSetupOrigin checks an --origin: https, a host, an optional port, and
// nothing else (no user, path, query or fragment; one trailing slash is
// dropped). A setup token is accepted over HTTPS only, so a link to plain
// HTTP would only fail later.
func parseSetupOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	switch {
	case err != nil:
		return "", errors.New("not an address")
	case u.Scheme != "https":
		return "", errors.New("must start with https://")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("names no host")
	case u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "?#"):
		return "", errors.New("must be https://<host>[:port] and nothing more")
	}
	return "https://" + u.Host, nil
}

func runOperatorBootstrap(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("operator bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	keep := fs.Bool("keep-existing-credentials", false, "do not revoke credentials or delete webhook hooks that already exist")
	serverNameFlag := fs.String("server-name", os.Getenv("CONCHD_SERVER_NAME"), "the server's name, which the operator may not take (default "+server.DefaultServerName+")")
	linkOpts := addLinkFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("operator bootstrap: --data (or CONCHD_DATA) is required")
	}
	origin, err := linkOpts.resolve("operator bootstrap")
	if err != nil {
		return err
	}
	serverName, err := offlineServerName("operator bootstrap", *serverNameFlag)
	if err != nil {
		return err
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return errors.New("operator bootstrap: give the operator's name, once")
	}
	name := positional[0]
	if err := schema.ValidateDisplayName("operator name", name, schema.MaxPrincipalNameLength); err != nil {
		return err
	}
	if err := refuseUnfinishedRestore("operator bootstrap", *dataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(*dataDir, 0o750); err != nil { // #nosec G301,G703 -- trusted operator path
		return fmt.Errorf("operator bootstrap: create data dir: %w", err)
	}
	ctx := context.Background()
	dbPath := filepath.Join(*dataDir, "conch.db")
	_, statErr := os.Stat(dbPath) // #nosec G703 -- trusted operator path
	createdDB := errors.Is(statErr, os.ErrNotExist)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	st.SetServerName(serverName)
	res, err := st.BootstrapOperatorWith(ctx, name, store.BootstrapOptions{KeepExistingCredentials: *keep, Setup: true})
	if err := bootstrapError("operator bootstrap", name, serverName, err); err != nil {
		return err
	}
	if createdDB {
		_, _ = fmt.Fprintf(stderr, "note: no database existed at %s; a new one was created. If you meant an existing instance, check --data\n", dbPath)
	}
	if res.Revoked > 0 {
		_, _ = fmt.Fprintf(stderr, "revoked %d existing credential(s); pass --keep-existing-credentials to keep them\n", res.Revoked)
	}
	if res.RevokedHooks > 0 {
		_, _ = fmt.Fprintf(stderr, "deleted %d existing webhook hook(s); pass --keep-existing-credentials to keep them\n", res.RevokedHooks)
	}
	if res.Manifests > 0 {
		_, _ = fmt.Fprintf(stderr, "note: %d agent manifest(s) already exist and were kept; they were writable by anyone until now, so review them\n", res.Manifests)
	}
	_, _ = fmt.Fprintf(stderr, "operator %q (principal %d) created, with no credential yet\n", res.Principal.Name, res.Principal.ID)
	printSetupToken(stdout, stderr, res.Principal.Name, res.SetupToken, origin)
	return nil
}

// bootstrapError is what a refused bootstrap says; nil when err is nil.
func bootstrapError(cmd, name, serverName string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrOperatorExists):
		return fmt.Errorf("%s: an operator already exists; nothing changed", cmd)
	case errors.Is(err, store.ErrGuestNameReserved):
		return fmt.Errorf("%s: names beginning with %s are reserved for guests; nothing changed", cmd, schema.ReservedGuestNameKeyPrefix)
	case errors.Is(err, store.ErrServerNameReserved):
		return serverNameRefusal(cmd, serverName)
	case errors.Is(err, store.ErrNameReserved):
		return fmt.Errorf("%s: names beginning with %s are reserved for hidden accounts; nothing changed", cmd, schema.HiddenPrincipalNamePrefix)
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("%s: a principal named %q, or a name that looks the same, already exists; nothing changed", cmd, name)
	case errors.Is(err, store.ErrNameInvalid):
		return fmt.Errorf("%s: the name has no character a reader can see; nothing changed", cmd)
	default:
		return fmt.Errorf("%s: %w", cmd, err)
	}
}

// runOperatorSetup serves both setup and recover: one names an operator by
// id and gets a setup token, the other also resets what they had.
func runOperatorSetup(cmd string, args []string, stdout, stderr io.Writer) error {
	label := "operator " + cmd
	fs := flag.NewFlagSet(label, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	linkOpts := addLinkFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	// Refused before the store is opened: recover revokes, and a refusal
	// must leave everything as it was.
	if cmd == "recover" && (*linkOpts.link || *linkOpts.origin != "") {
		return fmt.Errorf("%s: --link is not offered for recover; run it without --link and give the printed token on the setup page; nothing changed", label)
	}
	origin, err := linkOpts.resolve(label)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("%s: give the operator's principal id, once (conch admin principal list shows ids)", label)
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("%s: %q is not a principal id", label, positional[0])
	}
	// A setup token made before conchd restore has finished would be
	// deleted by it (issue #716).
	if *dataDir != "" {
		if err := refuseUnfinishedRestore(label, *dataDir); err != nil {
			return err
		}
	}
	ctx := context.Background()
	st, err := openExisting(ctx, label, *dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// The name is only for the messages; the store's checks below decide.
	p, err := st.PrincipalByID(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s: %w", label, err)
	}
	var (
		token   store.SetupToken
		revoked int
	)
	if cmd == "recover" {
		var rec store.Recovery
		rec, err = st.RecoverOperator(ctx, id, time.Now())
		token, revoked = rec.Setup, rec.Revoked
	} else {
		token, err = st.IssueSetupToken(ctx, id, time.Now())
	}
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		return fmt.Errorf("%s: no principal has id %d; nothing changed", label, id)
	case errors.Is(err, store.ErrNotAnOperator):
		return fmt.Errorf("%s: principal %d is not an operator; nothing changed (a member recovers through a reset link, docs/deploy.md)", label, id)
	case errors.Is(err, store.ErrPrincipalDisabled):
		return fmt.Errorf("%s: operator %d is disabled; enable them first; nothing changed", label, id)
	case errors.Is(err, store.ErrPrincipalHidden):
		return fmt.Errorf("%s: operator %d is hidden; nothing changed", label, id)
	case errors.Is(err, store.ErrSetupComplete):
		return fmt.Errorf("%s: operator %d already has a password and an authenticator; to replace them use: conchd operator recover; nothing changed", label, id)
	case err != nil:
		return fmt.Errorf("%s: %w", label, err)
	}
	if cmd == "recover" {
		_, _ = fmt.Fprintf(stderr, "operator %q (principal %d) recovered: %d credential(s) revoked, the authenticator removed, every pending sign-in and link ended\n", p.Name, id, revoked)
	}
	printSetupToken(stdout, stderr, p.Name, token, origin)
	if cmd == "setup" {
		_, _ = fmt.Fprintln(stderr, "If a setup token was already spent and setup finished, conchd operator setup refuses: conchd operator recover is the command then.")
	}
	return nil
}
