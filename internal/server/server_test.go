package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return New(Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "v1.2.3-test"}, st)
}

func TestHealthzOK(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var h schema.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if h.Status != schema.HealthOK {
		t.Errorf("status = %q, want %q", h.Status, schema.HealthOK)
	}
	if h.Version != "v1.2.3-test" {
		t.Errorf("version = %q, want v1.2.3-test", h.Version)
	}
	if h.DB != schema.HealthOK {
		t.Errorf("db = %q, want %q", h.DB, schema.HealthOK)
	}
}

func TestHealthzDegradedWhenStoreClosed(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	srv := New(Config{Version: "v0", Listen: "127.0.0.1:0"}, st)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var h schema.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if h.Status != schema.HealthDegraded {
		t.Errorf("status = %q, want %q", h.Status, schema.HealthDegraded)
	}
	if h.DB != schema.HealthDegraded {
		t.Errorf("db = %q, want %q", h.DB, schema.HealthDegraded)
	}
}

// TestServeGracefulShutdown boots the server on a real socket, hits /healthz,
// then cancels the context and asserts Serve returns cleanly without leaking
// the accept/serve goroutines.
func TestServeGracefulShutdown(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// A client that keeps no persistent connections, so the only goroutines
	// that could outlive shutdown are the server's own.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()

	// Baseline after the store and listener exist but before serving, so the
	// leak check isolates the accept/serve goroutines.
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// The listener is already bound (srv.Listen above), so the address is
	// available immediately.
	url := "http://" + srv.Addr() + "/healthz"
	resp, err := httpGetWithRetry(t, client, url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	// Simulate SIGINT/SIGTERM by cancelling the serve context.
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of shutdown")
	}

	assertNoGoroutineLeak(t, before)
}

func httpGetWithRetry(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	var lastErr error
	for range 20 {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

// assertNoGoroutineLeak polls until the goroutine count settles back to at
// most the baseline, allowing HTTP background goroutines a moment to wind down.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	for range 50 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}
