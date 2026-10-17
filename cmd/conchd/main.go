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
	"io/fs"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/buildinfo"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/internal/web"
	"github.com/njdaniel/conch/pkg/schema"
)

// version is set by the linker (-X main.version=...) and is empty in a build
// that was not stamped; buildVersion is what the binary reports.
var version string

// buildVersion is the stamped version, or the commit the binary was built from.
func buildVersion() string { return buildinfo.Version(version) }

// embeddedWeb is the web client built into this binary; a variable so a test
// can stand in for a binary built with -tags conchweb.
var embeddedWeb = web.Files

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
		fmt.Println(buildVersion())
		fmt.Println("web: " + embeddedWebName())
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
                     [--web-dir <dir> | --no-web] [--allowed-origins <origin,...>]
                     [--trusted-proxies <cidr,...>] [--invite-allowance <n>]
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
  --max-upload-bytes     largest file an upload may carry, in bytes; 0 disables uploads
                         (env CONCHD_MAX_UPLOAD_BYTES, default 10485760). Files are stored
                         under <data>/files, not in the database.
  --session-stale        how long without a heartbeat before a session shows as stale
                         (env CONCHD_SESSION_STALE, default 30s)
  --session-offline      how long without a heartbeat before a session shows as offline
                         (env CONCHD_SESSION_OFFLINE, default 90s). Must be longer than
                         --session-stale; serve refuses to start otherwise.
  --mcp-token            token=principal_id mapping for MCP bearer auth; comma-separate (env CONCHD_MCP_TOKENS)
  --ntfy-server          ntfy server URL (env CONCHD_NTFY_SERVER)
  --ntfy-topic           topic for new and resolved approvals (env CONCHD_NTFY_TOPIC)
  --ntfy-urgent-topic    topic for escalations (env CONCHD_NTFY_URGENT_TOPIC)
                         With --ntfy-server, set both: what a missing topic
                         would carry is not sent.
  --livekit-url          ws:// or wss:// LiveKit address given to clients (env CONCHD_LIVEKIT_URL)
  --livekit-api-url      http:// or https:// LiveKit address conchd calls (env CONCHD_LIVEKIT_API_URL,
                         default: --livekit-url with ws->http, wss->https)
  --livekit-room-prefix  start of the name of every new voice room (env CONCHD_LIVEKIT_ROOM_PREFIX,
                         default conch-). Lower-case letters, digits and '-', at most 32
                         characters, ending in '-'. Rooms already stored keep their names.
  --web-dir              a built web client (a directory holding index.html) to serve at /app/ on
                         the API's own origin; / then redirects to /app/ (env CONCHD_WEB_DIR).
                         Unset, the web client built into this binary is served instead, if there
                         is one (conchd version says "web: embedded" or "web: none"); with neither
                         there is no /app/. serve refuses to start if the directory is unusable.
  --no-web               do not serve /app/ at all, even when a client is built in
                         (env CONCHD_NO_WEB=1). Not valid together with --web-dir.
  --allowed-origins      comma-separated extra Origins the WebSocket upgrade accepts, such as
                         http://localhost:5173 (env CONCHD_ALLOWED_ORIGINS). It exists only so a
                         Vite dev server on another port can connect; unset keeps the same-origin
                         check. Each entry is one exact origin; wildcards are refused.
  --trusted-proxies      comma-separated CIDRs of reverse proxies whose X-Forwarded-For is
                         believed, such as 127.0.0.1/32,::1/128 for a proxy on this host
                         (env CONCHD_TRUSTED_PROXIES, default none). The client address is
                         used only for the invite routes' rate limits. Unset, the header is
                         never read and every request counts as coming from its connection's
                         peer: behind a proxy, from the proxy.
  --invite-allowance     how many open personal invites one person who may invite can hold
                         at once; operators have no allowance (env CONCHD_INVITE_ALLOWANCE,
                         default 5)

Voice (LiveKit) is all or nothing: set none of the LiveKit settings and voice is
off; set some but not all and serve refuses to start. The signing key pair is
read only from the environment, never a flag: CONCHD_LIVEKIT_API_KEY and
CONCHD_LIVEKIT_API_SECRET. Startup does not wait for LiveKit: conchd starts and
serves whether or not LiveKit answers, and looks at it once straight away.
`)
}

// serveOptions is what `conchd serve` resolved from its flags and environment.
type serveOptions struct {
	dataDir           string
	listen            string
	authMode          server.AuthMode
	mcpTokens         map[string]int64
	ntfyServer        string
	ntfyTopic         string
	ntfyUrgentTopic   string
	livekit           livekit.Config
	livekitRoomPrefix string
	maxUploadBytes    int64
	webDir            string
	webFS             fs.FS  // the embedded client, when it is the one served
	webSource         string // what the start-up line names: dir <path>, embedded or none
	allowedOrigins    []string
	sessionStale      time.Duration
	sessionOffline    time.Duration
	trustedProxies    []netip.Prefix
	inviteAllowance   int
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
	livekitRoomPrefix := fs.String("livekit-room-prefix", envOr(livekit.EnvRoomPrefix, store.DefaultVoiceRoomPrefix), "start of the name of every new voice room")
	webDir := fs.String("web-dir", os.Getenv("CONCHD_WEB_DIR"), "built web client directory to serve at /app/")
	// An off switch that is set to something it cannot read must not be
	// taken as unset: that would serve the client the operator meant to
	// turn off.
	noWebDefault, err := envBool("CONCHD_NO_WEB")
	if err != nil {
		return serveOptions{}, err
	}
	noWeb := fs.Bool("no-web", noWebDefault, "do not serve /app/, even when a web client is built in")
	originsRaw := fs.String("allowed-origins", os.Getenv("CONCHD_ALLOWED_ORIGINS"), "extra Origins the WebSocket upgrade accepts (Vite dev server only)")
	// The environment seeds the default of each; a flag given on the command
	// line wins because the flag set parses it afterwards.
	staleDefault, err := envDuration("CONCHD_SESSION_STALE", server.DefaultSessionStale)
	if err != nil {
		return serveOptions{}, err
	}
	offlineDefault, err := envDuration("CONCHD_SESSION_OFFLINE", server.DefaultSessionOffline)
	if err != nil {
		return serveOptions{}, err
	}
	sessionStale := fs.Duration("session-stale", staleDefault, "how long without a heartbeat before a session shows as stale")
	sessionOffline := fs.Duration("session-offline", offlineDefault, "how long without a heartbeat before a session shows as offline")
	trustedProxiesRaw := fs.String("trusted-proxies", os.Getenv("CONCHD_TRUSTED_PROXIES"), "comma-separated CIDRs of reverse proxies whose X-Forwarded-For is believed")
	inviteAllowance := fs.Int("invite-allowance", store.DefaultPersonalInviteAllowance, "open personal invites one inviter may hold at once")
	if raw := os.Getenv("CONCHD_INVITE_ALLOWANCE"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return serveOptions{}, fmt.Errorf("serve: CONCHD_INVITE_ALLOWANCE: %q is not a whole number", raw)
		}
		*inviteAllowance = v
	}
	maxUpload := fs.Int64("max-upload-bytes", server.DefaultMaxUploadBytes, "largest file an upload may carry, in bytes; 0 disables uploads")
	// The environment seeds the default; a flag given on the command line
	// wins because the flag set parses it afterwards.
	if raw := os.Getenv("CONCHD_MAX_UPLOAD_BYTES"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return serveOptions{}, fmt.Errorf("serve: CONCHD_MAX_UPLOAD_BYTES: %q is not a whole number of bytes", raw)
		}
		*maxUpload = v
	}
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if *dataDir == "" {
		return serveOptions{}, errors.New("serve: --data (or CONCHD_DATA) is required")
	}
	if *maxUpload < 0 {
		return serveOptions{}, errors.New("serve: --max-upload-bytes must not be negative (0 disables uploads)")
	}
	// Zero would read as "nobody may invite" but mean the default to the
	// store, so it is refused rather than reinterpreted.
	if *inviteAllowance < 1 {
		return serveOptions{}, errors.New("serve: --invite-allowance must be at least 1")
	}
	trustedProxies, err := server.ParseTrustedProxies(*trustedProxiesRaw)
	if err != nil {
		return serveOptions{}, fmt.Errorf("serve: --trusted-proxies: %w", err)
	}
	if err := server.ValidateSessionThresholds(*sessionStale, *sessionOffline); err != nil {
		return serveOptions{}, fmt.Errorf("serve: %w", err)
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
	if err := store.ValidateVoiceRoomPrefix(*livekitRoomPrefix); err != nil {
		return serveOptions{}, fmt.Errorf("serve: --livekit-room-prefix: %w", err)
	}
	origins, err := server.ParseAllowedOrigins(*originsRaw)
	if err != nil {
		return serveOptions{}, fmt.Errorf("serve: --allowed-origins: %w", err)
	}
	webFS, webSource, err := resolveWeb(*webDir, *noWeb)
	if err != nil {
		return serveOptions{}, err
	}
	return serveOptions{
		webDir: *webDir, webFS: webFS, webSource: webSource, allowedOrigins: origins,
		livekit: lk, livekitRoomPrefix: *livekitRoomPrefix, maxUploadBytes: *maxUpload,
		sessionStale: *sessionStale, sessionOffline: *sessionOffline,
		trustedProxies: trustedProxies, inviteAllowance: *inviteAllowance,
		dataDir: *dataDir, listen: *listen, authMode: authMode, mcpTokens: mcpTokens,
		ntfyServer: *ntfyServer, ntfyTopic: *ntfyTopic, ntfyUrgentTopic: *ntfyUrgentTopic,
	}, nil
}

// resolveWeb decides which web client /app/ serves. A --web-dir wins and is
// checked here, as always; with none, the client built into the binary, if
// any; --no-web turns all of it off. Naming both a directory and --no-web
// contradicts itself and is refused rather than guessed at.
func resolveWeb(webDir string, noWeb bool) (fsys fs.FS, source string, err error) {
	switch {
	case webDir != "" && noWeb:
		return nil, "", errors.New("serve: --web-dir and --no-web contradict each other")
	case noWeb:
		return nil, "none (--no-web)", nil
	case webDir != "":
		if err := server.CheckWebDir(webDir); err != nil {
			return nil, "", fmt.Errorf("serve: --web-dir: %w", err)
		}
		return nil, "dir " + webDir, nil
	}
	if fsys, ok := embeddedWeb(); ok {
		if err := server.CheckWebFS(fsys); err != nil {
			return nil, "", fmt.Errorf("serve: %w", err)
		}
		return fsys, "embedded", nil
	}
	return nil, "none", nil
}

// embeddedWebName is what `conchd version` says about the build.
func embeddedWebName() string {
	if _, ok := embeddedWeb(); ok {
		return "embedded"
	}
	return "none"
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
	if err := st.SetVoiceRoomPrefix(opts.livekitRoomPrefix); err != nil {
		return err
	}

	srv := server.New(server.Config{
		DataDir:         opts.dataDir,
		Listen:          opts.listen,
		Version:         buildVersion(),
		MCPBearerTokens: opts.mcpTokens,
		AuthMode:        opts.authMode,
		LiveKit:         opts.livekit,
		MaxUploadBytes:  opts.maxUploadBytes,
		WebDir:          opts.webDir,
		WebFS:           opts.webFS,
		AllowedOrigins:  opts.allowedOrigins,
		SessionStale:    opts.sessionStale,
		SessionOffline:  opts.sessionOffline,
		TrustedProxies:  opts.trustedProxies,
		InviteAllowance: opts.inviteAllowance,
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

	fmt.Printf("conchd %s listening on %s (data %s)\n", buildVersion(), srv.Addr(), opts.dataDir)
	fmt.Printf("conchd web: %s\n", opts.webSource)
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
	case errors.Is(err, store.ErrNameReserved):
		return fmt.Errorf("bootstrap-operator: names beginning with %s are reserved for hidden accounts; nothing changed", schema.HiddenPrincipalNamePrefix)
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("bootstrap-operator: a principal named %q, or a name that looks the same, already exists; nothing changed", *name)
	case errors.Is(err, store.ErrNameInvalid):
		return errors.New("bootstrap-operator: --name has no character a reader can see; nothing changed")
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

// envDuration reads a duration from the environment variable key, or returns
// fallback when it is unset or empty. A value that is not a duration is an
// error, not a silent fallback.
func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("serve: %s: %q is not a duration such as 30s", key, raw)
	}
	return d, nil
}

// envBool reads key as a boolean (1, t, true, 0, f, false and the like):
// unset is false, and anything else that is not a boolean is an error.
func envBool(key string) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("serve: %s: %q is not a boolean such as 1 or 0", key, raw)
	}
	return b, nil
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
