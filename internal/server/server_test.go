package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithConfig(t, Config{AuthMode: AuthOff})
}

// testDBPaths holds the database file of each server newTestServerWithConfig
// made, for the few tests that must set up what no route can, such as a
// second operator (issue #438).
var testDBPaths sync.Map // *Server -> string

func newTestServerWithConfig(t *testing.T, cfg Config) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conch.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	cfg.DataDir = t.TempDir()
	cfg.Listen = "127.0.0.1:0"
	cfg.Version = "v1.2.3-test"
	srv := New(cfg, st)
	testDBPaths.Store(srv, path)
	return srv
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

// TestHealthzVoice: /healthz says what the presence poller knows of LiveKit,
// and a voice outage never makes the server degraded or the answer 503: the
// core works without voice (issue #446, ADR-002).
func TestHealthzVoice(t *testing.T) {
	lk := livekit.Config{URL: "ws://lk.invalid", APIURL: "http://lk.invalid", APIKey: "k", APISecret: livekit.NewSecret("s")}
	tests := []struct {
		name            string
		cfg             livekit.Config
		contacted, down bool
		want            schema.HealthVoice
	}{
		{"not configured", livekit.Config{}, false, false, schema.HealthVoiceNotConfigured},
		{"configured, not answered yet", lk, false, false, schema.HealthVoiceUnavailable},
		{"configured and answering", lk, true, false, schema.HealthVoiceOK},
		{"configured and down", lk, true, true, schema.HealthVoiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, LiveKit: tt.cfg})
			srv.voice.mu.Lock()
			srv.voice.contacted, srv.voice.down = tt.contacted, tt.down
			srv.voice.mu.Unlock()

			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 whatever voice says", rec.Code)
			}
			var h schema.Health
			if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
				t.Fatal(err)
			}
			if h.Voice != tt.want || h.Status != schema.HealthOK || h.DB != schema.HealthOK {
				t.Errorf("health = %+v, want status ok, db ok, voice %s", h, tt.want)
			}
		})
	}
}

// A real outage, as the poller finds it: LiveKit's address answers nothing,
// one sweep marks voice down, and /healthz says unavailable with a 200.
func TestHealthzVoiceOutage(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, LiveKit: livekit.Config{
		URL: "ws://lk.invalid", APIURL: deadURL, APIKey: "k", APISecret: livekit.NewSecret("s"),
	}})
	srv.voice.runSweep(context.Background())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var h schema.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || h.Status != schema.HealthOK || h.Voice != schema.HealthVoiceUnavailable {
		t.Errorf("healthz = %d %+v, want 200 ok with voice unavailable", rec.Code, h)
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
	srv := New(Config{AuthMode: AuthOff, Version: "v0", Listen: "127.0.0.1:0"}, st)

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
