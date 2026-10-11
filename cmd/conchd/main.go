// Command conchd is the Conch server: SQLite-backed message log, REST/WS API,
// and MCP endpoint for agents. See ROADMAP.md. P0 provides config, the serve
// command, a health endpoint, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

var version = "v0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "conchd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "bootstrap-operator":
		return runBootstrapOperator(args[1:], os.Stdout, os.Stderr)
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(w *os.File) {
	_, _ = fmt.Fprint(w, `conchd — the Conch server

Usage:
  conchd serve [--data <dir>] [--listen <addr>] [--auth off|required] [--ntfy-server <url>]
                     [--livekit-url <ws-url>] [--livekit-api-url <http-url>]
  conchd bootstrap-operator --data <dir> --name <name> [--keep-existing-credentials]
  conchd version

bootstrap-operator works offline on the data directory: it creates the first
operator (a human principal) and one credential, prints the token once, and
refuses if an operator already exists. It also revokes every credential and
deletes every webhook hook created before the operator existed (they came from
open endpoints) unless --keep-existing-credentials is given. Agent manifests
are kept; it reports how many exist so you can review them.

Authentication is required by default: every REST and WebSocket request needs
a bearer credential, so run bootstrap-operator first. --auth off opens every
endpoint to anyone who can reach the port and is for local development only.

Flags for serve:
  --data    directory for the SQLite database (env CONCHD_DATA)
  --listen  HTTP listen address (env CONCHD_LISTEN, default :8080)
  --auth    REST/WebSocket authentication: required or off (env CONCHD_AUTH, default required)
  --mcp-token            token=principal_id mapping for MCP bearer auth; comma-separate (env CONCHD_MCP_TOKENS)
  --ntfy-server          ntfy server URL (env CONCHD_NTFY_SERVER)
  --ntfy-topic           normal approvals topic (env CONCHD_NTFY_TOPIC)
  --ntfy-urgent-topic    urgent escalation topic (env CONCHD_NTFY_URGENT_TOPIC)
  --livekit-url          ws:// or wss:// LiveKit address given to clients (env CONCHD_LIVEKIT_URL)
  --livekit-api-url      http:// or https:// LiveKit address conchd calls (env CONCHD_LIVEKIT_API_URL,
                         default: --livekit-url with ws->http, wss->https)

Voice (LiveKit) is all or nothing: set none of the LiveKit settings and voice is
off; set some but not all and serve refuses to start. The signing key pair is
read only from the environment, never a flag: CONCHD_LIVEKIT_API_KEY and
CONCHD_LIVEKIT_API_SECRET. Startup does not wait for LiveKit: conchd starts and
serves whether or not LiveKit answers, and looks at it once straight away.
`)
}

// serveOptions is what `conchd serve` resolved from its flags and environment.
type serveOptions struct {
	dataDir         string
	listen          string
	authMode        server.AuthMode
	mcpTokens       map[string]int64
	ntfyServer      string
	ntfyTopic       string
	ntfyUrgentTopic string
	livekit         livekit.Config
}

// parseServeArgs resolves the serve flags and their environment fallbacks.
// It is separate from runServe so the resolved values — the authentication
// mode above all — can be tested without starting a server.
func parseServeArgs(args []string) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	listen := fs.String("listen", envOr("CONCHD_LISTEN", ":8080"), "HTTP listen address")
	mcpTokensRaw := fs.String("mcp-token", os.Getenv("CONCHD_MCP_TOKENS"), "comma-separated token=agent_principal_id mappings for MCP bearer auth")
	authFlag := fs.String("auth", envOr("CONCHD_AUTH", string(defaultAuthMode)), "REST/WebSocket authentication: required or off")
	ntfyServer := fs.String("ntfy-server", os.Getenv("CONCHD_NTFY_SERVER"), "ntfy server URL")
	ntfyTopic := fs.String("ntfy-topic", os.Getenv("CONCHD_NTFY_TOPIC"), "normal approvals ntfy topic")
	ntfyUrgentTopic := fs.String("ntfy-urgent-topic", os.Getenv("CONCHD_NTFY_URGENT_TOPIC"), "urgent escalation ntfy topic")
	livekitURL := fs.String("livekit-url", os.Getenv(livekit.EnvURL), "ws:// or wss:// LiveKit address given to clients")
	livekitAPIURL := fs.String("livekit-api-url", os.Getenv(livekit.EnvAPIURL), "http:// or https:// LiveKit address conchd calls (default: derived from --livekit-url)")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if *dataDir == "" {
		return serveOptions{}, errors.New("serve: --data (or CONCHD_DATA) is required")
	}
	authMode, err := server.ParseAuthMode(*authFlag)
	if err != nil {
		return serveOptions{}, fmt.Errorf("serve: --auth: %w", err)
	}
	mcpTokens, err := parseMCPTokens(*mcpTokensRaw)
	if err != nil {
		return serveOptions{}, err
	}
	// The key pair has no flag on purpose: a flag would show in the process list.
	lk, err := livekit.ParseConfig(*livekitURL, *livekitAPIURL, os.Getenv(livekit.EnvAPIKey), os.Getenv(livekit.EnvAPISecret))
	if err != nil {
		return serveOptions{}, fmt.Errorf("serve: %w", err)
	}
	return serveOptions{
		livekit: lk,
		dataDir: *dataDir, listen: *listen, authMode: authMode, mcpTokens: mcpTokens,
		ntfyServer: *ntfyServer, ntfyTopic: *ntfyTopic, ntfyUrgentTopic: *ntfyUrgentTopic,
	}, nil
}

func runServe(args []string) error {
	opts, err := parseServeArgs(args)
	if err != nil {
		return err
	}

	// The data directory is an operator-supplied path by design; conchd runs
	// with the operator's own privileges, so this is configuration, not a
	// traversal vector.
	if err := os.MkdirAll(opts.dataDir, 0o750); err != nil { // #nosec G301,G703 -- trusted operator path
		return fmt.Errorf("serve: create data dir: %w", err)
	}

	// Signal-aware context: the first SIGINT/SIGTERM triggers graceful
	// shutdown. Once that fires, unregister the handler so a second signal
	// gets default handling and can force-kill a stuck drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	st, err := store.Open(ctx, filepath.Join(opts.dataDir, "conch.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	srv := server.New(server.Config{
		DataDir:         opts.dataDir,
		Listen:          opts.listen,
		Version:         version,
		MCPBearerTokens: opts.mcpTokens,
		AuthMode:        opts.authMode,
		LiveKit:         opts.livekit,
		Ntfy: approvals.NtfyConfig{
			Server:         opts.ntfyServer,
			ApprovalsTopic: opts.ntfyTopic,
			UrgentTopic:    opts.ntfyUrgentTopic,
			Timeout:        2 * time.Second,
		},
	}, st)
	if err := srv.Listen(); err != nil {
		return err
	}

	fmt.Printf("conchd %s listening on %s (data %s)\n", version, srv.Addr(), opts.dataDir)
	return srv.Serve(ctx)
}

// runBootstrapOperator creates the instance's first operator offline. The token
// goes to stdout alone, so a script can capture it; the one-line notice goes to
// stderr. On any refusal nothing is written (the store does it all in one
// transaction) and the returned error makes conchd exit nonzero.
func runBootstrapOperator(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bootstrap-operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	name := fs.String("name", "", "name of the operator principal")
	keep := fs.Bool("keep-existing-credentials", false, "do not revoke credentials or delete webhook hooks that already exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("bootstrap-operator: --data (or CONCHD_DATA) is required")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("bootstrap-operator: --name is required")
	}
	// The operator's name enters logs, audit details and every client's
	// terminal like any principal name, so the same rule applies as to
	// POST /v0/principals (issue #204).
	if err := schema.ValidateDisplayName("operator name", *name, schema.MaxPrincipalNameLength); err != nil {
		return err
	}
	if err := os.MkdirAll(*dataDir, 0o750); err != nil { // #nosec G301,G703 -- trusted operator path
		return fmt.Errorf("bootstrap-operator: create data dir: %w", err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(*dataDir, "conch.db")
	// A mistyped --data would otherwise bootstrap an empty database and look
	// like success, leaving the real instance without an operator.
	_, statErr := os.Stat(dbPath) // #nosec G703 -- trusted operator path
	createdDB := errors.Is(statErr, os.ErrNotExist)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	res, err := st.BootstrapOperatorWith(ctx, *name, store.BootstrapOptions{KeepExistingCredentials: *keep})
	switch {
	case errors.Is(err, store.ErrOperatorExists):
		return errors.New("bootstrap-operator: an operator already exists; nothing changed")
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("bootstrap-operator: a principal named %q already exists; nothing changed", *name)
	case err != nil:
		return fmt.Errorf("bootstrap-operator: %w", err)
	}
	_, _ = fmt.Fprintln(stdout, res.Token)
	p := res.Principal
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
	_, _ = fmt.Fprintf(stderr, "operator %q (principal %d) created; the token printed on stdout will not be shown again\n", p.Name, p.ID)
	return nil
}

// defaultAuthMode is what `conchd serve` uses when neither --auth nor
// CONCHD_AUTH is given. Authentication is on unless it is switched off.
const defaultAuthMode = server.AuthRequired

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseMCPTokens(raw string) (map[string]int64, error) {
	mappings := make(map[string]int64)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		token, idRaw, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(token) == "" || strings.TrimSpace(idRaw) == "" {
			return nil, fmt.Errorf("serve: invalid --mcp-token %q, want token=agent_principal_id", part)
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("serve: invalid MCP principal id %q", idRaw)
		}
		mappings[strings.TrimSpace(token)] = id
	}
	return mappings, nil
}
