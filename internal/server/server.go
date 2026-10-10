package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	// configured; nothing contacts LiveKit at startup either way.
	LiveKit livekit.Config
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
	http  *http.Server
	ln    net.Listener
	// routes is the route table (routes.go); mux and routeByPattern are
	// derived from it and nothing else registers routes.
	routes         []route
	routeByPattern map[string]route
	mux            *http.ServeMux
	// credRecheckInterval is how often an authenticated WebSocket re-checks
	// its own credential. Unexported so tests can shorten it before serving.
	credRecheckInterval time.Duration
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
	}
	s := &Server{cfg: cfg, store: st, hub: hub.New(), approvals: approvals.New(st, notifier), broadcaster: broadcaster, credRecheckInterval: defaultCredentialRecheckInterval}
	s.voice = newVoicePoller(s)
	s.routes = s.routeTable()
	s.routeByPattern = make(map[string]route, len(s.routes))
	s.mux = http.NewServeMux()
	for _, rt := range s.routes {
		s.routeByPattern[rt.pattern] = rt
		s.mux.Handle(rt.pattern, s.guard(rt))
	}
	s.logAgentsWithoutManifest(context.Background())
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

// Listen binds the configured address. It is separated from Serve so tests can
// bind ":0" and read the assigned port via Addr before serving.
func (s *Server) Listen() error {
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	s.ln = ln
	return nil
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
	if s.lk != nil {
		stop := s.voice.start(ctx)
		defer stop()
	}

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
// store is reachable, 503 with status "degraded" when it is not.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := schema.Health{
		Status:  schema.HealthOK,
		Version: s.cfg.Version,
		DB:      schema.HealthOK,
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
