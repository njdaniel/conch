package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/hub"
	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Config configures a Server. It is populated from conchd's flags/environment.
type Config struct {
	// DataDir is the directory holding the SQLite database. It is provided for
	// operator context and future endpoints/logging; the store itself is opened
	// by the caller.
	DataDir string
	// Listen is the TCP address the HTTP server binds to (e.g. ":8080").
	Listen string
	// Version is the build version, reported by /healthz.
	Version string
	// Broadcaster, when set, receives each message after it has been persisted
	// and delivered to the server's own WebSocket hub — an additional tap for
	// tests and future integrations (e.g. ntfy), not a replacement for the hub.
	Broadcaster Broadcaster
	// MCPBearerTokens maps bearer tokens to agent principal IDs for the MCP endpoint.
	MCPBearerTokens map[string]int64
	// Ntfy configures optional approval lifecycle push notifications. When
	// unconfigured, notification hooks are silent and append no audit rows.
	Ntfy approvals.NtfyConfig
	// AuthMode selects REST/WebSocket authentication. Unset means
	// AuthRequired; only an explicit AuthOff opens the server. See auth.go.
	AuthMode AuthMode
	// LiveKit configures optional voice. The zero value means voice is not
	// configured. Building the server contacts nothing; when voice is
	// configured, Serve looks at LiveKit once at start (the presence
	// poller's first sweep) without waiting for the answer.
	LiveKit livekit.Config
	// MaxUploadBytes is the largest file an upload may carry. Zero or less
	// disables uploads (fetching existing files still works). conchd defaults
	// it to DefaultMaxUploadBytes; the zero value here is off.
	MaxUploadBytes int64
	// WebDir, when set, is a built web client served at /app/ (see
	// static.go). It wins over WebFS. With neither, no /app/ routes exist.
	WebDir string
	// WebFS, when set and WebDir is not, is the web client embedded in the
	// binary (internal/web), rooted at the directory holding index.html.
	WebFS fs.FS
	// AllowedOrigins are extra origins the WebSocket upgrade accepts besides
	// the server's own, so a dev server on another port can connect. Empty
	// keeps the same-origin check. See ParseAllowedOrigins.
	AllowedOrigins []string
	// SessionStale and SessionOffline are how long a session may go without
	// a heartbeat before it shows as stale and then as offline (issue #265).
	// Zero means DefaultSessionStale and DefaultSessionOffline. Serve refuses
	// to start unless stale is shorter than offline.
	SessionStale   time.Duration
	SessionOffline time.Duration
	// TrustedProxies are the reverse proxies whose X-Forwarded-For conchd
	// believes, for the invite routes' per-address rate limits (see
	// clientaddr.go). Empty, the header is never read and the client is the
	// connection's peer.
	TrustedProxies []netip.Prefix
	// InviteAllowance is how many outstanding personal invites one inviter
	// who is not an operator may hold. Zero or less means
	// store.DefaultPersonalInviteAllowance.
	InviteAllowance int
	// MetricsListen, when set, is the address of a second listener that
	// serves Prometheus metrics at /metrics and nothing else (issue #562,
	// metrics.go). Empty, the default, means no such listener. /metrics is
	// never served on Listen.
	MetricsListen string
	// MetricsAllowRemote lets MetricsListen be an address that is not
	// loopback. Without it such an address stops the server at start.
	MetricsAllowRemote bool
	// TestRelaxInviteLimits raises the per-address rate limits of the invite and
	// sign-in-link routes (invite_limits.go) so that a browser suite, which
	// redeems everything from one address, does not share one budget across
	// its specs (issue #609). It is for test servers only: Listen refuses it
	// unless every address the server binds is loopback and no trusted proxy
	// is set, and logs a warning when it is on. Off, the default, the limits are the production ones.
	TestRelaxInviteLimits bool
	// ServerName is the label the front door shows (GET /v1/join) and a name
	// no guest label may take (issue #710). Empty means DefaultServerName.
	// conchd checks it with ValidateServerName.
	ServerName string
	// TOTPKey is the host key operators' TOTP secrets are sealed under
	// (issue #709, totp.go): kept outside the database and its backups,
	// and backed up on its own. Nil, operators cannot enrol a second factor
	// or sign in with a password, and every TOTP check fails closed;
	// everything else works. conchd reads it from CONCHD_TOTP_KEY only.
	TOTPKey *TOTPKey
}

// Broadcaster is the delivery seam invoked after a message is persisted.
// The server's WebSocket hub implements it; Config.Broadcaster taps it.
type Broadcaster interface {
	BroadcastMessage(context.Context, schema.MessageV0)
}

type noopBroadcaster struct{}

func (noopBroadcaster) BroadcastMessage(context.Context, schema.MessageV0) {}

// shutdownTimeout bounds how long Serve waits for in-flight requests to drain
// on shutdown before forcing connections closed.
const shutdownTimeout = 10 * time.Second

// Server is conchd's HTTP server wrapped around the store. It owns the HTTP
// listener and mux; the store's lifecycle belongs to the caller.
type Server struct {
	// awaitPolled, when set, is called after each authorized poll of
	// await_decision, before it waits for the next. Tests only (#740): it
	// lets a test withdraw the authorization once the wait is polling, not
	// after a guess at how long that takes. Nil in production, where it does
	// nothing.
	awaitPolled func()
	cfg         Config
	store       *store.Store
	hub         *hub.Hub
	approvals   *approvals.Manager
	broadcaster Broadcaster
	// lk is the LiveKit client; nil when voice is not configured. It is built
	// without contacting LiveKit (design note §2).
	lk *livekit.Client
	// voice is voice presence and enforcement (issue #127). It exists on
	// every server so presence can answer "not configured"; its goroutine
	// runs only when lk is set, started by Serve.
	voice *voicePoller
	// events is the subscribers of the events socket (events_ws.go). Like
	// voice it exists on every server.
	events *eventsHub
	http   *http.Server
	ln     net.Listener
	// metrics is the server's own registry and counts (metrics.go). It is
	// always kept, in memory; only metricsLn, bound when MetricsListen is
	// set, ever serves it.
	metrics   *serverMetrics
	metricsLn net.Listener
	// routes is the route table (routes.go); mux and routeByPattern are
	// derived from it and nothing else registers routes.
	routes         []route
	routeByPattern map[string]route
	mux            *http.ServeMux
	// credRecheckInterval is how often an authenticated WebSocket re-checks
	// its own credential. Unexported so tests can shorten it before serving.
	credRecheckInterval time.Duration
	// pingInterval and pingTimeout are the socket keepalive's (keepalive.go);
	// zero means the defaults. Unexported so tests can shorten them.
	pingInterval, pingTimeout time.Duration
	// limits counts agents' uses against their manifest rate limits and
	// denials coalesces repeated access_denied rows (issue #80). Both are in
	// memory only. now is their clock; tests replace it before serving.
	limits  agentRateLimiter
	denials denialCoalescer
	now     func() time.Time
	// sessionSweepInterval is how often the session sweeper runs. Unexported
	// so tests can shorten it before serving.
	sessionSweepInterval time.Duration
	// guestSweepInterval is the guest deadline sweeper's fallback poll
	// (guests.go); zero means defaultGuestSweepInterval.
	guestSweepInterval time.Duration
	// keepAliveTicks starts the ticker that refreshes the session an events
	// socket was opened for, and returns its channel and its stop function.
	// Nil means every eventsKeepAliveInterval; tests replace it to drive the
	// ticks by hand.
	keepAliveTicks func() (<-chan time.Time, func())
	// eventsBeforeWrite, when set by a test, runs before every frame an
	// events socket writes: a test blocks in it to be a subscriber that is
	// stuck in a write.
	eventsBeforeWrite func()
	// eventsRosterFloor is the least time between two roster rebuilds for one
	// events socket (defaultEventsRosterFloor). It is measured on the
	// monotonic clock, not now: it paces work, it is not a time anyone reads.
	// Tests set it to zero to see each roster as it is owed.
	eventsRosterFloor time.Duration
	// tickets holds the unspent WebSocket tickets (issue #270), in memory
	// only; its clock is now.
	tickets wsTicketStore
	// sockets counts the open WebSockets per credential (issue #309), in
	// memory only.
	sockets socketCounter
	// renameFile moves a finished upload into place. It is os.Rename; tests
	// replace it to fail the step between the temporary file and the final
	// name.
	renameFile func(oldpath, newpath string) error
	// replayPageSize is how many messages one page of a /v2/ws ?after= replay
	// reads; zero means the default. replayHooks lets a test act at a chosen
	// point of a replay. Both are unexported and set by tests before serving.
	replayPageSize int
	replayHooks    wsReplayHooks
	// beforeDecide, when a test sets it, runs between the decision handler's
	// own membership and right checks and the deciding transaction, so a test
	// can land a revoke in that gap (issue #453). Nil in conchd.
	beforeDecide func()
	// inviteLimits is the invite routes' rate limits (invite_limits.go), in
	// memory only; its clock is now. Tests replace it with setInviteLimits
	// before serving.
	inviteLimits *inviteLimiter
	// diag is each principal's recent request ids and socket closes, and
	// connLogs thins the connection log (issue #446). Both are in memory
	// only.
	diag     diagRing
	connLogs connLogLimiter
	// reportLimits is the problem-report rate limit (reports.go), in memory
	// only; its clock is now. Tests replace it before serving.
	reportLimits *reportLimiter
	// guestAddrs is which address each guest visit came from, for the
	// per-address cap (join.go), in memory only.
	guestAddrs guestAddressBook
	// passwords is password hashing and its two bounded pools (password.go);
	// logins is the sign-in limits (login.go), in memory only, with now as
	// their clock. Tests replace logins with setLoginLimits before serving.
	passwords *passwords
	logins    *loginLimiter
	// setupLimits is the setup-token redemption's per-address limit
	// (operator_auth.go), in memory only, with now as its clock.
	setupLimits *setupLimiter
	// addressMAC keys the HMAC a pending login keeps of its client address
	// (pendingAddressKey): random per process, never stored or logged.
	addressMAC []byte
	// beforeLoginIssue, when a test sets it, runs between a sign-in's
	// password verification and the transaction that issues what it earned,
	// so a test can land a disable or a password change in that gap. Nil in
	// conchd.
	beforeLoginIssue func()
	// beforePasswordWrite is the same seam for the password routes
	// (password_recovery.go): it runs after a password was verified or
	// hashed and before the transaction that writes. Nil in conchd.
	beforePasswordWrite func()
}

// New builds a Server for cfg backed by st. It does not bind a socket; call
// Listen (or Serve, which binds lazily) to start accepting connections.
func New(cfg Config, st *store.Store) *Server {
	broadcaster := cfg.Broadcaster
	if broadcaster == nil {
		broadcaster = noopBroadcaster{}
	}
	// With no ntfy server configured (or an invalid one) there is no notifier
	// at all. The nil *NtfyNotifier must not be handed over as it is: inside
	// the Notifier interface it would not compare equal to nil, the manager
	// would take notifications to be on, and every transition would be
	// recorded as notify_sent although nothing was sent (issue #158).
	var notifier approvals.Notifier
	ntfy, err := approvals.NewNtfyNotifier(cfg.Ntfy)
	if err != nil {
		slog.Error("server: ntfy disabled by invalid configuration", "error", err)
	}
	if ntfy != nil {
		notifier = ntfy
		// Said once, here: with a topic left empty the notifications that
		// use it are never sent, and each is audited as not attempted (issue
		// #170). conchd starts all the same.
		if missing := ntfy.MissingTopics(); len(missing) > 0 {
			slog.Warn("server: an ntfy server is set but a topic is not; the notifications that use it will not be sent",
				"missing", strings.Join(missing, ", "), "not_sent", ntfyNotSent(missing))
		}
	}
	s := &Server{cfg: cfg, store: st, hub: hub.New(), approvals: approvals.New(st, notifier), broadcaster: broadcaster, credRecheckInterval: defaultCredentialRecheckInterval, sessionSweepInterval: defaultSessionSweepInterval, now: time.Now, renameFile: os.Rename, inviteLimits: newInviteLimiter(inviteLimitsFor(cfg)), reportLimits: newReportLimiter(defaultReportLimit()),
		passwords: newPasswords(), logins: newLoginLimiter(loginLimitsFor(cfg)), addressMAC: newAddressMACKey(),
		setupLimits: newSetupLimiter(cfg)}
	s.removeStaleUploads()
	s.voice = newVoicePoller(s)
	s.events = newEventsHub()
	s.metrics = newServerMetrics(s)
	// Runs that end with their session change without SetRunState, so the
	// store reports them here, after its commit (issue #405).
	st.OnRunsEnded(func(runs []store.EndedRun) {
		for _, r := range runs {
			s.events.notifyRun(r.ChannelID, r.ID)
		}
	})
	// A guest visit can end through many calls (End visit, a sweep, a
	// revoke, a disable); the store reports each after its commit (issue
	// #705), and what follows is the same whichever it was.
	st.OnGuestsEnded(s.guestsEnded)
	// A restore run on a store this server holds moves every principal's
	// authentication generation on (issue #716); what they have open closes
	// as after any other bump.
	st.OnAuthChanged(func(ids []int64) {
		for _, id := range ids {
			s.authChanged(context.Background(), id)
		}
	})
	s.eventsRosterFloor = defaultEventsRosterFloor
	s.routes = append(s.routeTable(), s.webRoutes()...)
	s.routeByPattern = make(map[string]route, len(s.routes))
	s.mux = http.NewServeMux()
	for _, rt := range s.routes {
		s.routeByPattern[rt.pattern] = rt
		s.mux.Handle(rt.pattern, traceRoute(s.guard(rt)))
	}
	s.logAgentsWithoutManifest(context.Background())
	s.checkTOTPKey(context.Background())
	if s.VoiceConfigured() {
		if s.lk, err = livekit.New(cfg.LiveKit); err != nil {
			// Unreachable: Configured() was just checked. Fail closed anyway:
			// with no client every voice endpoint answers voice_not_configured.
			slog.Error("voice: client not built", "error", err)
		} else {
			slog.Info("voice: configured", "livekit", cfg.LiveKit)
		}
	} else {
		slog.Info("voice: not configured")
	}
	var handler http.Handler = s.mux
	if cfg.authRequired() {
		handler = s.authMiddleware(handler)
	} else {
		slog.Warn("auth: authentication is OFF: every endpoint is open to anyone who can reach this port, and request bodies are trusted for identity; use this for local development only")
	}
	// Outside authentication, so every answer has a request id, a refusal
	// included (issue #446).
	handler = s.requestIDMiddleware(handler)
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// VoiceConfigured reports whether LiveKit settings were supplied. Later voice
// endpoints answer voice_not_configured when it is false.
func (s *Server) VoiceConfigured() bool { return s.cfg.LiveKit.Configured() }

// Handler returns the HTTP handler, for use with httptest and future mounts.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Listen binds the configured address, and the metrics address when one is
// set. It is separated from Serve so tests can bind ":0" and read the assigned
// port via Addr before serving. A metrics address that is refused, or cannot
// be bound, binds nothing.
func (s *Server) Listen() error {
	// A server behind a reverse proxy binds loopback too (Caddy forwards to
	// 127.0.0.1 in the deployment) yet is reachable from anywhere, so trusted
	// proxies rule the relaxed limits out as well (issue #609).
	if s.cfg.TestRelaxInviteLimits && len(s.cfg.TrustedProxies) > 0 {
		return errors.New("server: the test-only relaxed invite limits are on, but trusted proxies are set, so this server is reachable through a proxy; they are for a local test server only")
	}
	if s.ln != nil {
		return nil
	}
	if s.cfg.MetricsListen != "" {
		// Checked before the main bind, so a refused address leaves nothing
		// bound.
		if err := ValidateMetricsListen(s.cfg.MetricsListen, s.cfg.MetricsAllowRemote); err != nil {
			return fmt.Errorf("server: %w", err)
		}
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	if s.cfg.TestRelaxInviteLimits {
		if err := checkRelaxedLimitsLoopback("HTTP", ln); err != nil {
			_ = ln.Close()
			return err
		}
	}
	if s.cfg.MetricsListen != "" {
		mln, err := s.listenMetrics()
		if err != nil {
			_ = ln.Close()
			return err
		}
		if s.cfg.TestRelaxInviteLimits {
			if err := checkRelaxedLimitsLoopback("metrics", mln); err != nil {
				_ = mln.Close()
				_ = ln.Close()
				return err
			}
		}
		s.metricsLn = mln
	}
	s.ln = ln
	if s.cfg.TestRelaxInviteLimits {
		slog.Warn("invites: TEST-ONLY relaxed per-address invite and sign-in-link rate limits are ON; the brake on guessing invite codes from one address is off. Never run a real server this way", "addr", ln.Addr().String())
	}
	return nil
}

// inviteLimitsFor is the invite routes' limits cfg asks for.
func inviteLimitsFor(cfg Config) inviteLimits {
	if cfg.TestRelaxInviteLimits {
		return testRelaxedInviteLimits()
	}
	return defaultInviteLimits()
}

// checkRelaxedLimitsLoopback refuses relaxed invite limits on a listener that
// is not loopback, judged by the address it actually bound, so that ":8080",
// "0.0.0.0:0" and a name that resolves elsewhere are all refused. A relaxed
// limit on a reachable server would turn off the only brake on guessing
// invite codes from one address.
func checkRelaxedLimitsLoopback(what string, ln net.Listener) error {
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return nil
	}
	return fmt.Errorf("server: the test-only relaxed invite limits are on, but the %s listener is bound to %s, which is not loopback; they are for a local test server only", what, ln.Addr())
}

// MetricsAddr reports the metrics listener's bound address, or "" when there
// is none.
func (s *Server) MetricsAddr() string {
	if s.metricsLn == nil {
		return ""
	}
	return s.metricsLn.Addr().String()
}

// Addr reports the bound address, or "" if the server has not been bound yet.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve accepts connections until ctx is cancelled, then gracefully shuts down,
// draining in-flight requests up to shutdownTimeout. It returns nil on a clean
// shutdown. The store is left open for the caller to close.
func (s *Server) Serve(ctx context.Context) error {
	// Before anything is bound or started: thresholds that cannot work are a
	// configuration error, not something to discover when sessions misbehave.
	stale, offline := s.sessionThresholds()
	if err := ValidateSessionThresholds(stale, offline); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := s.Listen(); err != nil {
		return err
	}
	// Deadline and escalation timers survive restarts by rehydrating from the
	// store before the listener starts accepting traffic.
	if err := s.approvals.Rehydrate(ctx); err != nil {
		return fmt.Errorf("server: rehydrate approvals: %w", err)
	}
	// WebSocket connections are hijacked, so http.Server.Shutdown neither
	// waits for nor closes them; closing the hub makes every WS handler drop
	// its subscription, close its connection, and return.
	defer s.hub.Close()
	// Stop approval timers on shutdown; open approvals re-arm on next boot.
	defer s.approvals.Close()
	// Voice presence: the poller runs only when voice is configured, stops
	// with ctx, and Serve waits for it (a pass in flight is cancelled, so
	// this is prompt). Presence sockets are hijacked like message sockets, so
	// they are closed explicitly.
	defer s.voice.closeAll()
	// Events sockets are hijacked too, and are closed the same way.
	defer s.events.closeAll()
	if s.lk != nil {
		stop := s.voice.start(ctx)
		defer stop()
	}

	// Sessions: the sweeper marks silent sessions stale and then offline. It
	// stops with ctx and Serve waits for it, so no goroutine outlives Serve.
	defer s.startSessionSweeper(ctx)()

	// Guest visits: the sweeper ends each at its deadline (access is refused
	// at the deadline already) and stops with ctx, waited for like the rest.
	defer s.startGuestSweeper(ctx)()

	// Metrics, when configured, on their own listener; stopped with the rest.
	defer s.serveMetrics(ctx)()

	// Pending logins: spent and expired challenges are deleted (issue #709).
	defer s.startPendingLoginSweeper(ctx)()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(s.ln) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		// Drain the serve goroutine; ErrServerClosed is the expected result.
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: serve: %w", err)
		}
		return nil
	}
}

// handleHealth serves GET /healthz: 200 with a schema.Health body when the
// store is reachable, 503 with status "degraded" when it is not. Voice is
// reported but never decides either: it is optional, and the core works
// without it (ADR-002).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := schema.Health{
		Status:  schema.HealthOK,
		Version: s.cfg.Version,
		DB:      schema.HealthOK,
		Voice:   s.voice.health(),
	}
	code := http.StatusOK

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		// The wire body carries only the stable "degraded" marker; the
		// underlying error may embed driver detail or filesystem paths.
		slog.ErrorContext(ctx, "healthz: store ping failed", "error", err)
		h.Status = schema.HealthDegraded
		h.DB = schema.HealthDegraded
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(h)
}

// ntfyNotSent says in words which approval notifications are lost to the
// missing ntfy settings.
func ntfyNotSent(missing []string) string {
	var lost []string
	for _, flag := range missing {
		switch flag {
		case "--ntfy-topic":
			lost = append(lost, "approval created, approval resolved or expired")
		case "--ntfy-urgent-topic":
			lost = append(lost, "approval escalated")
		}
	}
	return strings.Join(lost, "; ")
}
