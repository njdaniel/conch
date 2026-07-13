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
	// Broadcaster receives each message after it has been persisted. When nil,
	// message posting uses a no-op broadcaster.
	Broadcaster Broadcaster
}

// Broadcaster is the realtime delivery seam used after a message is persisted.
// The P0 REST server supplies a no-op implementation; the WebSocket hub can
// implement this interface without changing the message handlers.
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
	broadcaster Broadcaster
	http        *http.Server
	ln          net.Listener
}

// New builds a Server for cfg backed by st. It does not bind a socket; call
// Listen (or Serve, which binds lazily) to start accepting connections.
func New(cfg Config, st *store.Store) *Server {
	broadcaster := cfg.Broadcaster
	if broadcaster == nil {
		broadcaster = noopBroadcaster{}
	}
	s := &Server{cfg: cfg, store: st, broadcaster: broadcaster}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v0/channels", s.handleCreateChannel)
	mux.HandleFunc("POST /v0/principals", s.handleCreatePrincipal)
	mux.HandleFunc("POST /v0/channels/{channel}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /v0/channels/{channel}/messages", s.handleListMessages)
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

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
