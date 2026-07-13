package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const databaseFilename = "conch.db"

// Config controls a Server instance.
type Config struct {
	DataDir string
	Version string
}

// Server is conchd's HTTP server and embedded store.
type Server struct {
	store      *store.Store
	httpServer *http.Server
	version    string

	closeOnce sync.Once
	closeErr  error
}

// New creates the data directory, opens the embedded store, and constructs the
// HTTP handler. The caller must call Close if the server is not passed to
// Serve.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("server: data directory is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("server: create data directory: %w", err)
	}

	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, databaseFilename))
	if err != nil {
		return nil, err
	}

	s := &Server{store: db, version: cfg.Version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Handler returns the server's HTTP handler for in-process use and tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Serve serves HTTP until ctx is canceled or the listener fails. Canceling ctx
// drains active requests before closing the embedded store.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	var err error
	select {
	case err = <-serveErr:
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.httpServer.Shutdown(shutdownCtx)
		cancel()
		serverErr := <-serveErr
		if err == nil && !errors.Is(serverErr, http.ErrServerClosed) {
			err = serverErr
		}
	}

	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Close closes the embedded store. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.store.Close()
	})
	return s.closeErr
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	response := schema.Health{Version: s.version, DB: "ok"}
	status := http.StatusOK
	if err := s.store.Ping(r.Context()); err != nil {
		response.DB = "error"
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
