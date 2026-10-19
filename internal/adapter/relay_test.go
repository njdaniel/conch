package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const relayTestToken = "SECRET-RELAY-TOKEN"

// call is one request conchd's fake received.
type call struct {
	method, path, query, auth string
	body                      []byte
}

func (c call) String() string { return c.method + " " + c.path }

// fakeConchd is the conchd the relay forwards to: the session, run and
// intervention routes with the answers the relay reads. override answers
// first, for a test that needs conchd to refuse.
type fakeConchd struct {
	*httptest.Server
	mu       sync.Mutex
	calls    []call
	override func(w http.ResponseWriter, r *http.Request) bool
	ivs      map[int64]schema.InterventionState
	nextRun  int64
}

func newFakeConchd(t *testing.T) *fakeConchd {
	t.Helper()
	f := &fakeConchd{ivs: map[int64]schema.InterventionState{}}
	mux := http.NewServeMux()
	session := func(w http.ResponseWriter, _ *http.Request) {
		fakeJSON(w, http.StatusOK, schema.SessionV1{Schema: schema.SessionSchemaV1, ID: 7})
	}
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		fakeJSON(w, http.StatusCreated, schema.SessionV1{Schema: schema.SessionSchemaV1, ID: 7})
	})
	mux.HandleFunc("POST /v1/sessions/{id}/heartbeat", session)
	mux.HandleFunc("DELETE /v1/sessions/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/sessions/{id}/runs", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.nextRun++
		id := f.nextRun
		f.mu.Unlock()
		fakeJSON(w, http.StatusCreated, schema.RunV1{Schema: schema.RunSchemaV1, ID: id, SessionID: 7})
	})
	mux.HandleFunc("POST /v1/runs/{id}/state", func(w http.ResponseWriter, _ *http.Request) {
		fakeJSON(w, http.StatusOK, schema.RunV1{Schema: schema.RunSchemaV1, ID: 1})
	})
	mux.HandleFunc("POST /v1/runs/{id}/activity", func(w http.ResponseWriter, _ *http.Request) {
		fakeJSON(w, http.StatusCreated, schema.AgentActivityV1{Schema: schema.AgentActivitySchemaV1, ID: 1})
	})
	mux.HandleFunc("GET /v1/sessions/{id}/interventions", func(w http.ResponseWriter, r *http.Request) {
		state, after := r.URL.Query().Get("state"), int64(0)
		if v := r.URL.Query().Get("after"); v != "" {
			after, _ = strconv.ParseInt(v, 10, 64)
		}
		out := schema.ListInterventionsResponseV1{Interventions: []schema.InterventionV1{}}
		f.mu.Lock()
		for id, st := range f.ivs {
			if id > after && (state == "" || state == string(st)) {
				out.Interventions = append(out.Interventions, schema.InterventionV1{Schema: schema.InterventionSchemaV1, ID: id, RunID: 1, Kind: schema.InterventionKindCancel, State: st})
			}
		}
		f.mu.Unlock()
		sort.Slice(out.Interventions, func(i, j int) bool { return out.Interventions[i].ID < out.Interventions[j].ID })
		fakeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /v1/interventions/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var req schema.AckInterventionRequestV1
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ivs[id] != schema.InterventionStateRequested {
			fakeJSON(w, http.StatusConflict, schema.Error{Code: schema.ErrorCodeInvalidTransition, Message: "already answered"})
			return
		}
		f.ivs[id] = req.State
		fakeJSON(w, http.StatusOK, schema.InterventionV1{Schema: schema.InterventionSchemaV1, ID: id, State: req.State})
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), body})
		override := f.override
		f.mu.Unlock()
		if override != nil && override(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func fakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeConchd) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// seen returns the calls so far, or only those to path.
func (f *fakeConchd) seen(path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if path == "" || c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// request puts an intervention on conchd that is waiting for an answer.
func (f *fakeConchd) request(id int64) {
	f.mu.Lock()
	f.ivs[id] = schema.InterventionStateRequested
	f.mu.Unlock()
}

// shortDir is a private directory with a short path: a Unix socket's path has
// about a hundred bytes, which t.TempDir's does not always keep to.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func relayConfig(server, dir string) config {
	return config{
		Mode: modeRelay, Server: server, Token: relayTestToken, Channel: "ops", Runtime: "ci",
		Heartbeat: 10 * time.Second, InterventionTimeout: 60 * time.Second,
		RelaySocket: filepath.Join(dir, "r.sock"),
	}
}

// socketClient talks to the relay's socket.
func socketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}
}

// running is a relay serving on its socket.
type running struct {
	*relay
	fake   *fakeConchd
	clk    *fakeClock
	client *http.Client
	ctx    context.Context
}

// do sends one request to the relay and returns the status, the Retry-After
// header and the body.
func (r *running) do(t *testing.T, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://relay"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// startRelay registers a relay against a fake conchd and serves its handler on
// a real socket made the way the program makes it. It does not run the
// background loops: a test fires them itself with expire and discover.
func startRelay(t *testing.T, mutate func(*config)) *running {
	t.Helper()
	fake := newFakeConchd(t)
	cfg := relayConfig(fake.URL, shortDir(t))
	if mutate != nil {
		mutate(&cfg)
	}
	clk := newFakeClock()
	r := newRelay(cfg, newUpstream(cfg.Server, cfg.Token), clk)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.register(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err := listenRelay(ctx, cfg.RelaySocket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: r.handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &running{relay: r, fake: fake, clk: clk, client: socketClient(cfg.RelaySocket), ctx: ctx}
}

// startRun opens a run through the relay and returns its id.
func (r *running) startRun(t *testing.T) int64 {
	t.Helper()
	status, _, body := r.do(t, "POST", "/v1/runs", `{"channel":"ops","task":"nightly build"}`)
	var run schema.RunV1
	if status != http.StatusCreated || json.Unmarshal(body, &run) != nil || run.ID == 0 {
		t.Fatalf("start run = %d %s", status, body)
	}
	return run.ID
}

func decodeError(t *testing.T, body []byte) schema.Error {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body %q is no Error: %v", body, err)
	}
	return e
}

func TestRelaySocketAndDirectoryModes(t *testing.T) {
	r := startRelay(t, nil)
	sock := r.cfg.RelaySocket
	info, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", info.Mode())
	}
	if !ownedByUs(info) {
		t.Errorf("the socket is not owned by this user")
	}
	dir, err := os.Lstat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0o700 || !ownedByUs(dir) {
		t.Errorf("socket directory mode = %v, want 0700 and ours", dir.Mode())
	}
	if status, _, body := r.do(t, "GET", "/v1/session", ""); status != http.StatusOK || !strings.Contains(string(body), `"id":7`) {
		t.Errorf("GET /v1/session = %d %s", status, body)
	}
}

func TestRelayRefusesSocketPaths(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the socket path
		want  string
		// registered is whether a session was asked for before the refusal.
		registered bool
	}{
		{"a directory open to others", func(t *testing.T, dir string) string {
			if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the case under test
				t.Fatal(err)
			}
			return filepath.Join(dir, "r.sock")
		}, "want 0700", false},
		{"a directory that is not there", func(t *testing.T, dir string) string { return filepath.Join(dir, "missing", "r.sock") }, "socket directory", false},
		{"a path that is a file", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "r.sock")
			if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "is not a socket", false},
		{"a path that is a directory", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "r.sock")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		}, "is not a socket", false},
		{"a socket someone is listening on", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "r.sock")
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			return path
		}, "running", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeConchd(t)
			dir := shortDir(t)
			cfg := relayConfig(fake.URL, dir)
			cfg.RelaySocket = tt.setup(t, dir)
			err := runRelay(context.Background(), cfg, newFakeClock())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runRelay = %v, want an error with %q", err, tt.want)
			}
			if got := len(fake.seen("/v1/sessions")) > 0; got != tt.registered {
				t.Errorf("a session was asked for = %v, want %v", got, tt.registered)
			}
			if data, err := os.ReadFile(cfg.RelaySocket); err == nil && string(data) != "" && string(data) != "precious" { // #nosec G304 -- a test file
				t.Errorf("the file at the socket path changed: %q", data)
			}
		})
	}

	t.Run("a stale socket is replaced", func(t *testing.T) {
		fake := newFakeConchd(t)
		dir := shortDir(t)
		cfg := relayConfig(fake.URL, dir)
		ln, err := net.Listen("unix", cfg.RelaySocket)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close() // the file stays, nothing listens
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runRelay(ctx, cfg, newFakeClock()) }()
		waitForSocket(t, cfg.RelaySocket)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("runRelay = %v", err)
		}
	})
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listens on %s", path)
}

func TestRelayRefusals(t *testing.T) {
	big := `{"kind":"command_started","summary":"` + strings.Repeat("x", schema.MaxPostRequestBytes) + `"}`
	tests := []struct {
		name         string
		method, path string
		body         string
		status       int
		code         string
	}{
		{"an oversized body", "POST", "/v1/runs/1/activity", big, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"an oversized start", "POST", "/v1/runs", `{"channel":"ops","task":"` + strings.Repeat("x", schema.MaxPostRequestBytes) + `"}`, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"a malformed body", "POST", "/v1/runs/1/state", `{"state":`, http.StatusBadRequest, "invalid_request"},
		{"an empty body", "POST", "/v1/runs/1/state", ``, http.StatusBadRequest, "invalid_request"},
		{"an unknown field", "POST", "/v1/runs/1/state", `{"state":"running","bogus":1}`, http.StatusBadRequest, "invalid_request"},
		{"two objects", "POST", "/v1/runs/1/state", `{"state":"running"}{"state":"running"}`, http.StatusBadRequest, "invalid_request"},
		{"a value the type refuses", "POST", "/v1/runs/1/state", `{"state":"sleeping"}`, http.StatusBadRequest, "invalid_request"},
		{"an http link", "POST", "/v1/runs", `{"channel":"ops","task":"t","link":"http://ci.example.com"}`, http.StatusBadRequest, "invalid_request"},
		{"a run the relay did not create", "POST", "/v1/runs/999/state", `{"state":"running"}`, http.StatusNotFound, "run_not_found"},
		{"a run id that is no number", "POST", "/v1/runs/one/activity", `{"kind":"command_started","summary":"s"}`, http.StatusNotFound, "run_not_found"},
		{"a run id of zero", "POST", "/v1/runs/0/state", `{"state":"running"}`, http.StatusNotFound, "run_not_found"},
		{"another channel", "POST", "/v1/runs", `{"channel":"other","task":"t"}`, http.StatusNotFound, "channel_not_found"},
		{"a channel in other letters", "POST", "/v1/runs", `{"channel":"OPS","task":"t"}`, http.StatusNotFound, "channel_not_found"},
		{"an intervention the relay did not return", "POST", "/v1/interventions/51/ack", `{"state":"applied"}`, http.StatusNotFound, "intervention_not_found"},
		{"an intervention id that is no number", "POST", "/v1/interventions/x/ack", `{"state":"applied"}`, http.StatusNotFound, "intervention_not_found"},
	}
	r := startRelay(t, func(c *config) { c.Interventions = []schema.InterventionKind{schema.InterventionKindCancel} })
	if id := r.startRun(t); id != 1 {
		t.Fatalf("the first run is %d, want 1", id)
	}
	r.fake.request(51) // exists on conchd; no poll has returned it
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := r.fake.count()
			status, _, body := r.do(t, tt.method, tt.path, tt.body)
			if status != tt.status {
				t.Fatalf("status = %d %s, want %d", status, body, tt.status)
			}
			if got := decodeError(t, body).Code; got != tt.code {
				t.Errorf("code = %q, want %q", got, tt.code)
			}
			// No refusal of the relay's own is a request to conchd.
			if after := r.fake.count(); after != before {
				t.Errorf("conchd received %d request(s): %v", after-before, r.fake.calls[before:])
			}
		})
	}
}

func TestRelayUnknownRoutesAndMethods(t *testing.T) {
	r := startRelay(t, nil)
	before := r.fake.count()
	for _, tt := range []struct {
		method, path string
	}{
		{"GET", "/v1/sessions"}, {"DELETE", "/v1/session"}, {"POST", "/v1/session"}, {"GET", "/v1/runs"}, {"GET", "/v1/runs/1"},
		{"DELETE", "/v1/runs/1/state"}, {"POST", "/v1/sessions/7/runs"}, {"GET", "/v1/approvals"}, {"POST", "/v1/messages"},
	} {
		if status, _, _ := r.do(t, tt.method, tt.path, ""); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404 or 405", tt.method, tt.path, status)
		}
	}
	if after := r.fake.count(); after != before {
		t.Errorf("conchd received %d request(s) for routes the socket does not serve", after-before)
	}
}

// conchd's refusals and its Retry-After come back as they went.
func TestRelayPassesConchdAnswersUnchanged(t *testing.T) {
	tests := []struct {
		name         string
		method, path string
		body         string
		answer       func(w http.ResponseWriter)
		status       int
		retryAfter   string
		raw          string
	}{
		{
			name: "a manifest without runs.manage", method: "POST", path: "/v1/runs", body: `{"channel":"ops","task":"t"}`,
			answer: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"code":"channel_not_found","message":"channel not found"}`+"\n")
			},
			status: http.StatusNotFound, raw: `{"code":"channel_not_found","message":"channel not found"}` + "\n",
		},
		{
			name: "runs.manage taken away from a run that is open", method: "POST", path: "/v1/runs/1/state", body: `{"state":"running"}`,
			answer: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"code":"forbidden","message":"not allowed"}`)
			},
			status: http.StatusForbidden, raw: `{"code":"forbidden","message":"not allowed"}`,
		},
		{
			name: "a rate limit on a state report", method: "POST", path: "/v1/runs/1/state", body: `{"state":"running"}`,
			answer: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":"rate_limited","message":"slow down","retry_after_seconds":7}`)
			},
			status: http.StatusTooManyRequests, retryAfter: "7", raw: `{"code":"rate_limited","message":"slow down","retry_after_seconds":7}`,
		},
		{
			name: "a rate limit on activity", method: "POST", path: "/v1/runs/1/activity", body: `{"kind":"command_started","summary":"s"}`,
			answer: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":"rate_limited","message":"slow down","retry_after_seconds":30}`)
			},
			status: http.StatusTooManyRequests, retryAfter: "30", raw: `{"code":"rate_limited","message":"slow down","retry_after_seconds":30}`,
		},
		{
			name: "a rate limit on polling", method: "GET", path: "/v1/interventions?state=requested", answer: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":"rate_limited","message":"slow down","retry_after_seconds":3}`)
			},
			status: http.StatusTooManyRequests, retryAfter: "3", raw: `{"code":"rate_limited","message":"slow down","retry_after_seconds":3}`,
		},
		{
			name: "a transition conchd refuses", method: "POST", path: "/v1/runs/1/state", body: `{"state":"queued"}`,
			answer: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"invalid_transition","message":"no"}`)
			},
			status: http.StatusConflict, raw: `{"code":"invalid_transition","message":"no"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := startRelay(t, nil)
			r.startRun(t)
			r.fake.mu.Lock()
			r.fake.override = func(w http.ResponseWriter, req *http.Request) bool {
				if req.URL.Path == "/v1/sessions/7/runs" && tt.path != "/v1/runs" {
					return false
				}
				if strings.HasPrefix(req.URL.Path, "/v1/sessions/") && !strings.HasSuffix(req.URL.Path, "/runs") && !strings.HasSuffix(req.URL.Path, "/interventions") {
					return false
				}
				tt.answer(w)
				return true
			}
			r.fake.mu.Unlock()
			status, header, body := r.do(t, tt.method, tt.path, tt.body)
			if status != tt.status || string(body) != tt.raw || header.Get("Retry-After") != tt.retryAfter {
				t.Errorf("got %d %q Retry-After=%q; want %d %q Retry-After=%q", status, body, header.Get("Retry-After"), tt.status, tt.raw, tt.retryAfter)
			}
		})
	}

	t.Run("a run conchd refused is not a run of the relay's", func(t *testing.T) {
		r := startRelay(t, nil)
		r.fake.mu.Lock()
		r.fake.override = func(w http.ResponseWriter, req *http.Request) bool {
			if !strings.HasSuffix(req.URL.Path, "/runs") {
				return false
			}
			fakeJSON(w, http.StatusNotFound, schema.Error{Code: "channel_not_found", Message: "channel not found"})
			return true
		}
		r.fake.mu.Unlock()
		if status, _, _ := r.do(t, "POST", "/v1/runs", `{"channel":"ops","task":"t"}`); status != http.StatusNotFound {
			t.Fatalf("start = %d", status)
		}
		if status, _, body := r.do(t, "POST", "/v1/runs/1/state", `{"state":"running"}`); status != http.StatusNotFound || decodeError(t, body).Code != "run_not_found" {
			t.Errorf("state on a run that was never made = %d %s", status, body)
		}
	})

	t.Run("conchd cannot be reached", func(t *testing.T) {
		r := startRelay(t, nil)
		r.fake.Close()
		status, _, body := r.do(t, "POST", "/v1/runs", `{"channel":"ops","task":"t"}`)
		if status != http.StatusBadGateway || decodeError(t, body).Code != "internal_error" {
			t.Errorf("= %d %s, want 502 internal_error", status, body)
		}
	})
}

func TestRelayForwards(t *testing.T) {
	r := startRelay(t, func(c *config) { c.Interventions = []schema.InterventionKind{schema.InterventionKindCancel} })
	run := r.startRun(t)
	r.fake.request(40)

	data := `{"b":1,"a":12345678901234567890,"f":1.50,"s":"é"}`
	steps := []struct {
		name         string
		method, path string
		body         string
		status       int
		upstream     string // the path conchd should have received
		wantBody     string // what conchd should have received; "" for none
		wantQuery    string
	}{
		{"start", "POST", "/v1/runs", `{"channel":"ops","task":"nightly build","link":"https://ci.example.com/jobs/7421"}`, 201,
			"/v1/sessions/7/runs", `{"channel":"ops","task":"nightly build","link":"https://ci.example.com/jobs/7421"}`, ""},
		{"state", "POST", fmt.Sprintf("/v1/runs/%d/state", run), `{ "state" : "running" }`, 200,
			fmt.Sprintf("/v1/runs/%d/state", run), `{"state":"running"}`, ""},
		{"activity keeps its data as sent", "POST", fmt.Sprintf("/v1/runs/%d/activity", run),
			`{"kind":"command_finished","summary":"go test","outcome":"passed","data": ` + data + `}`, 201,
			fmt.Sprintf("/v1/runs/%d/activity", run), `{"kind":"command_finished","summary":"go test","outcome":"passed","data":` + data + `}`, ""},
		{"poll", "GET", "/v1/interventions?state=requested&after=3&limit=5&token=nope", "", 200,
			"/v1/sessions/7/interventions", "", "after=3&limit=5&state=requested"},
		{"ack", "POST", "/v1/interventions/40/ack", `{"state":"applied"}`, 200,
			"/v1/interventions/40/ack", `{"state":"applied"}`, ""},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			before := r.fake.count()
			if s.name == "ack" {
				// The poll before has returned 40 to the runtime.
				r.fake.request(40)
			}
			status, _, body := r.do(t, s.method, s.path, s.body)
			if status != s.status {
				t.Fatalf("status = %d %s, want %d", status, body, s.status)
			}
			calls := r.fake.calls[before:]
			if len(calls) != 1 {
				t.Fatalf("conchd received %v, want one call", calls)
			}
			c := calls[0]
			if c.path != s.upstream || c.query != s.wantQuery || string(c.body) != s.wantBody {
				t.Errorf("conchd received %s ?%s %q; want %s ?%s %q", c.path, c.query, c.body, s.upstream, s.wantQuery, s.wantBody)
			}
			if c.auth != "Bearer "+relayTestToken {
				t.Errorf("Authorization = %q", c.auth)
			}
		})
	}
}

func TestRelayInterventionTimeout(t *testing.T) {
	cancelKind := []schema.InterventionKind{schema.InterventionKindCancel}
	failedAck := func(t *testing.T, r *running) (schema.AckInterventionRequestV1, bool) {
		t.Helper()
		for _, c := range r.fake.seen("/v1/interventions/40/ack") {
			var req schema.AckInterventionRequestV1
			if err := json.Unmarshal(c.body, &req); err != nil {
				t.Fatal(err)
			}
			if req.State == schema.InterventionStateFailed {
				return req, true
			}
		}
		return schema.AckInterventionRequestV1{}, false
	}
	poll := func(t *testing.T, r *running) {
		t.Helper()
		if status, _, body := r.do(t, "GET", "/v1/interventions?state=requested", ""); status != 200 || !strings.Contains(string(body), `"id":40`) {
			t.Fatalf("poll = %d %s", status, body)
		}
	}

	t.Run("a request the runtime was shown and did not take", func(t *testing.T) {
		r := startRelay(t, func(c *config) { c.Interventions = cancelKind })
		r.fake.request(40)
		poll(t, r)
		r.clk.advance(59 * time.Second)
		r.expire(r.ctx)
		if _, answered := failedAck(t, r); answered {
			t.Fatal("answered failed after 59s of 60s")
		}
		r.clk.advance(2 * time.Second)
		r.expire(r.ctx)
		req, answered := failedAck(t, r)
		if !answered || req.Reason != "not taken by ci within 60s" {
			t.Fatalf("after 61s the answer is %+v (answered=%v), want failed with the fixed reason", req, answered)
		}
		// Answered once, and not again.
		r.clk.advance(time.Minute)
		r.expire(r.ctx)
		if n := len(r.fake.seen("/v1/interventions/40/ack")); n != 1 {
			t.Errorf("%d answers, want 1", n)
		}
		// The runtime's answer, late, meets conchd's 409.
		status, _, body := r.do(t, "POST", "/v1/interventions/40/ack", `{"state":"applied"}`)
		if status != http.StatusConflict || decodeError(t, body).Code != schema.ErrorCodeInvalidTransition {
			t.Errorf("late answer = %d %s, want 409 invalid_transition", status, body)
		}
	})

	t.Run("a request the runtime answered in time", func(t *testing.T) {
		r := startRelay(t, func(c *config) { c.Interventions = cancelKind })
		r.fake.request(40)
		poll(t, r)
		r.clk.advance(30 * time.Second)
		if status, _, body := r.do(t, "POST", "/v1/interventions/40/ack", `{"state":"applied"}`); status != 200 {
			t.Fatalf("ack = %d %s", status, body)
		}
		r.clk.advance(10 * time.Minute)
		r.expire(r.ctx)
		if _, answered := failedAck(t, r); answered {
			t.Error("a request already answered was answered failed")
		}
	})

	t.Run("a request the runtime never polled for", func(t *testing.T) {
		r := startRelay(t, func(c *config) { c.Interventions = cancelKind })
		r.fake.request(40)
		r.discover(r.ctx)
		r.clk.advance(61 * time.Second)
		r.expire(r.ctx)
		if _, answered := failedAck(t, r); !answered {
			t.Error("a request nobody polled for was not answered")
		}
	})

	t.Run("the runtime cannot answer what it was not shown", func(t *testing.T) {
		r := startRelay(t, func(c *config) { c.Interventions = cancelKind })
		r.fake.request(40)
		r.discover(r.ctx)
		if status, _, body := r.do(t, "POST", "/v1/interventions/40/ack", `{"state":"applied"}`); status != 404 || decodeError(t, body).Code != "intervention_not_found" {
			t.Errorf("= %d %s, want 404 intervention_not_found", status, body)
		}
	})

	t.Run("a failed answer is tried again", func(t *testing.T) {
		r := startRelay(t, func(c *config) { c.Interventions = cancelKind })
		r.fake.request(40)
		poll(t, r)
		r.clk.advance(61 * time.Second)
		r.fake.mu.Lock()
		r.fake.override = func(w http.ResponseWriter, req *http.Request) bool {
			if !strings.HasSuffix(req.URL.Path, "/ack") {
				return false
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		r.fake.mu.Unlock()
		r.expire(r.ctx)
		r.fake.mu.Lock()
		r.fake.override = nil
		r.fake.mu.Unlock()
		r.expire(r.ctx)
		if n := len(r.fake.seen("/v1/interventions/40/ack")); n != 2 {
			t.Errorf("%d attempts, want 2", n)
		}
		if st := r.fake.ivs[40]; st != schema.InterventionStateFailed {
			t.Errorf("state = %s, want failed", st)
		}
	})
}

// The same, driven by the relay's own tickers on the fake clock.
func TestRelayRunsItsClocks(t *testing.T) {
	fake := newFakeConchd(t)
	cfg := relayConfig(fake.URL, shortDir(t))
	cfg.Interventions = []schema.InterventionKind{schema.InterventionKindCancel}
	clk := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runRelay(ctx, cfg, clk) }()
	t.Cleanup(func() { cancel(); <-done })
	// The two loops start side by side, so their tickers come in either order.
	for want := map[time.Duration]bool{cfg.Heartbeat: true, interventionSweep: true}; len(want) > 0; {
		select {
		case d := <-clk.created:
			delete(want, d)
		case <-time.After(10 * time.Second):
			t.Fatalf("tickers still to come: %v", want)
		}
	}

	// A heartbeat, and with it a look for what is waiting.
	fake.request(40)
	clk.fire(cfg.Heartbeat)
	waitFor(t, "the heartbeat and the look for interventions", func() bool {
		return len(fake.seen("/v1/sessions/7/heartbeat")) == 1 && len(fake.seen("/v1/sessions/7/interventions")) == 1
	})
	// The fake having seen the request does not mean the relay has read the
	// response and started the clock on intervention 40: that clock starts at
	// the relay's now, so a sweep (or an advance) that comes first leaves
	// nothing overdue and nothing fires the sweep again. So move the time on
	// and sweep on every poll until the answer comes; once the relay holds
	// the intervention, the next advance makes it overdue.
	waitFor(t, "the answer after the timeout", func() bool {
		clk.advance(cfg.InterventionTimeout + time.Second)
		clk.fire(interventionSweep)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.ivs[40] == schema.InterventionStateFailed
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// SIGTERM (a cancelled context here; the signal is the e2e scenario's) ends the
// session, leaves the runs as they are and takes the socket away.
func TestRelayEndsItsSessionAndLeavesRuns(t *testing.T) {
	fake := newFakeConchd(t)
	cfg := relayConfig(fake.URL, shortDir(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runRelay(ctx, cfg, newFakeClock()) }()
	waitForSocket(t, cfg.RelaySocket)

	client := socketClient(cfg.RelaySocket)
	resp, err := client.Post("http://relay/v1/runs", "application/json", strings.NewReader(`{"channel":"ops","task":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start run = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRelay = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not stop")
	}
	if n := len(fake.seen("/v1/sessions/7")); n != 1 || fake.seen("/v1/sessions/7")[0].method != "DELETE" {
		t.Errorf("calls to the session = %v, want one DELETE", fake.seen("/v1/sessions/7"))
	}
	for _, c := range fake.seen("") {
		if strings.HasPrefix(c.path, "/v1/runs/") {
			t.Errorf("the relay reported on a run when it stopped: %s", c)
		}
	}
	if _, err := os.Lstat(cfg.RelaySocket); !os.IsNotExist(err) {
		t.Errorf("the socket is still there: %v", err)
	}
}

// Neither the token nor a body is ever written to the log.
func TestRelayLogsNoSecrets(t *testing.T) {
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	r := startRelay(t, nil)
	r.startRun(t)
	r.do(t, "POST", "/v1/runs/1/state", `{"state":"running","SECRET-BODY-MARKER":"x"}`)
	r.do(t, "POST", "/v1/runs/9/state", `{"state":"running"}`)
	r.fake.Close()
	r.do(t, "POST", "/v1/runs/1/state", `{"state":"running"}`)
	r.expire(r.ctx)
	logged := buf.String()
	if logged == "" {
		t.Fatal("nothing was logged")
	}
	for _, secret := range []string{relayTestToken, "SECRET-BODY-MARKER", "nightly build"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logged)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLoadConfigRelay(t *testing.T) {
	base := map[string]string{
		"CONCH_ADAPTER_MODE": "relay", "CONCH_ADAPTER_TOKEN": "t", "CONCH_ADAPTER_CHANNEL": "ops", "XDG_RUNTIME_DIR": "/run/user/1000",
	}
	with := func(kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(out, kv[i])
			} else {
				out[kv[i]] = kv[i+1]
			}
		}
		return out
	}
	t.Run("accepted", func(t *testing.T) {
		tests := []struct {
			name  string
			env   map[string]string
			check func(config) bool
		}{
			{"defaults: no principal, no command, the socket in XDG_RUNTIME_DIR", base, func(c config) bool {
				return c.Mode == modeRelay && c.RelaySocket == "/run/user/1000/conch-adapter-relay.sock" && c.Runtime == "relay" && c.Model == "" &&
					len(c.Interventions) == 0 && c.InterventionTimeout == 60*time.Second && c.Heartbeat == 10*time.Second &&
					c.LockFile == "/run/user/1000/conch-adapter-relay.sock.lock" && c.Channel == "ops" && c.PrincipalID == 0
			}},
			{"an explicit socket wins over XDG_RUNTIME_DIR", with("CONCH_ADAPTER_RELAY_SOCKET", "/tmp/x/r.sock"), func(c config) bool { return c.RelaySocket == "/tmp/x/r.sock" }},
			{"an explicit socket needs no XDG_RUNTIME_DIR", with("CONCH_ADAPTER_RELAY_SOCKET", "/tmp/x/r.sock", "XDG_RUNTIME_DIR", ""), func(c config) bool { return c.RelaySocket == "/tmp/x/r.sock" }},
			{"a label, interventions and a timeout", with("CONCH_ADAPTER_RUNTIME", "github-actions", "CONCH_ADAPTER_INTERVENTIONS", "cancel, message", "CONCH_ADAPTER_INTERVENTION_TIMEOUT", "5s"), func(c config) bool {
				return c.Runtime == "github-actions" && len(c.Interventions) == 2 && c.Interventions[0] == schema.InterventionKindCancel &&
					c.Interventions[1] == schema.InterventionKindMessage && c.InterventionTimeout == 5*time.Second
			}},
			{"any runtime label, even one with no profile", with("CONCH_ADAPTER_RUNTIME", "my-script"), func(c config) bool { return c.Runtime == "my-script" }},
			{"the subprocess mode is the default", map[string]string{"CONCH_ADAPTER_TOKEN": "t", "CONCH_ADAPTER_PRINCIPAL_ID": "7", "CONCH_ADAPTER_CHANNEL": "ops"}, func(c config) bool { return c.Mode == modeSubprocess }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cfg, err := loadConfig(envOf(tt.env))
				if err != nil {
					t.Fatal(err)
				}
				if !tt.check(cfg) {
					t.Fatalf("unexpected config: %+v", cfg)
				}
			})
		}
	})
	t.Run("refused", func(t *testing.T) {
		tests := []struct {
			name string
			env  map[string]string
			want string
		}{
			{"no socket and no XDG_RUNTIME_DIR", with("XDG_RUNTIME_DIR", ""), "XDG_RUNTIME_DIR"},
			{"a relative socket", with("CONCH_ADAPTER_RELAY_SOCKET", "r.sock"), "absolute"},
			{"a socket path too long for a socket", with("CONCH_ADAPTER_RELAY_SOCKET", "/"+strings.Repeat("d", 120)+"/r.sock"), "longer than"},
			{"no channel", with("CONCH_ADAPTER_CHANNEL", ""), "CONCH_ADAPTER_CHANNEL"},
			{"two channels", with("CONCH_ADAPTER_CHANNEL", "a,b"), "exactly one channel"},
			{"an unknown intervention kind", with("CONCH_ADAPTER_INTERVENTIONS", "explode"), "CONCH_ADAPTER_INTERVENTIONS"},
			{"a repeated intervention kind", with("CONCH_ADAPTER_INTERVENTIONS", "cancel,cancel"), "CONCH_ADAPTER_INTERVENTIONS"},
			{"a bad timeout", with("CONCH_ADAPTER_INTERVENTION_TIMEOUT", "soon"), "CONCH_ADAPTER_INTERVENTION_TIMEOUT"},
			{"a zero timeout", with("CONCH_ADAPTER_INTERVENTION_TIMEOUT", "0s"), "CONCH_ADAPTER_INTERVENTION_TIMEOUT"},
			{"a label with a control character", with("CONCH_ADAPTER_RUNTIME", "a\x07b"), "CONCH_ADAPTER_RUNTIME"},
			{"no token", with("CONCH_ADAPTER_TOKEN", ""), "CONCH_ADAPTER_TOKEN"},
			{"an unknown mode", with("CONCH_ADAPTER_MODE", "sidecar"), "CONCH_ADAPTER_MODE"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := loadConfig(envOf(tt.env))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want one naming %s", err, tt.want)
				}
			})
		}
	})
}

// The relay's lock is the system's, so a lock file left by a relay that died
// does not keep the next one out.
func TestRelayLock(t *testing.T) {
	path := filepath.Join(shortDir(t), "r.lock")
	release, err := acquireRelayLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRelayLock(path); err == nil || !strings.Contains(err.Error(), "another instance") {
		t.Errorf("a second relay on the lock: %v, want a refusal", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("lock file = %v, %v; want mode 0600", info, err)
	}
	release()
	// The file is still there, and nobody holds it.
	again, err := acquireRelayLock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}

func TestRelayLockRefusesLinksAndOrder(t *testing.T) {
	t.Run("a link in the lock's place", func(t *testing.T) {
		dir := shortDir(t)
		target := filepath.Join(dir, "victim")
		if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "r.lock")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireRelayLock(path); err == nil {
			t.Fatal("a symlinked lock path was taken")
		}
		if data, _ := os.ReadFile(target); string(data) != "mine" { // #nosec G304 -- a test file
			t.Errorf("the link's target changed: %q", data)
		}
		// And a dangling one must not be created through.
		dangling := filepath.Join(dir, "d.lock")
		if err := os.Symlink(filepath.Join(dir, "created"), dangling); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireRelayLock(dangling); err == nil {
			t.Fatal("a dangling link was taken")
		}
		if _, err := os.Lstat(filepath.Join(dir, "created")); err == nil {
			t.Error("the relay created a file through a link")
		}
	})
	t.Run("a directory is checked before the lock is taken", func(t *testing.T) {
		dir := shortDir(t)
		if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the case under test
			t.Fatal(err)
		}
		cfg := relayConfig("http://unused", dir)
		cfg.LockFile = cfg.RelaySocket + ".lock"
		if _, err := takeRelayLock(cfg); err == nil || !strings.Contains(err.Error(), "want 0700") {
			t.Fatalf("takeRelayLock = %v, want the directory refused", err)
		}
		if _, err := os.Lstat(cfg.LockFile); err == nil {
			t.Error("the lock file was created in a directory that was refused")
		}
	})
}

func TestRelaySocketIsBornPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := shortDir(t)
	ln, err := listenRelay(context.Background(), filepath.Join(dir, "r.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := syscall.Umask(0); got != 0 {
		t.Errorf("the umask was left at %04o", got)
	}
}

// A refused answer to an overdue intervention is not asked again for ever.
func TestRelayExpireRefusals(t *testing.T) {
	answer := func(status int, retryAfter string) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, req *http.Request) bool {
			if !strings.HasSuffix(req.URL.Path, "/ack") {
				return false
			}
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			fakeJSON(w, status, schema.Error{Code: "x", Message: "no"})
			return true
		}
	}
	tests := []struct {
		name     string
		override func(http.ResponseWriter, *http.Request) bool
		// waits are advances of the clock, each followed by an expire; want is
		// the number of answers attempted after each.
		waits []time.Duration
		want  []int
	}{
		{"401 is given up on", answer(http.StatusUnauthorized, ""), []time.Duration{0, time.Second, time.Second}, []int{1, 1, 1}},
		{"403 is given up on", answer(http.StatusForbidden, ""), []time.Duration{0, time.Second}, []int{1, 1}},
		{"429 waits for Retry-After", answer(http.StatusTooManyRequests, "30"), []time.Duration{0, time.Second, 28 * time.Second, 2 * time.Second}, []int{1, 1, 1, 2}},
		{"429 without Retry-After waits the default", answer(http.StatusTooManyRequests, ""), []time.Duration{0, 4 * time.Second, 2 * time.Second}, []int{1, 1, 2}},
		{"503 is tried every sweep", answer(http.StatusServiceUnavailable, ""), []time.Duration{0, time.Second, time.Second}, []int{1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := startRelay(t, func(c *config) { c.Interventions = []schema.InterventionKind{schema.InterventionKindCancel} })
			r.fake.request(40)
			r.discover(r.ctx)
			r.clk.advance(61 * time.Second)
			r.fake.mu.Lock()
			r.fake.override = tt.override
			r.fake.mu.Unlock()
			for i, wait := range tt.waits {
				r.clk.advance(wait)
				r.expire(r.ctx)
				if got := len(r.fake.seen("/v1/interventions/40/ack")); got != tt.want[i] {
					t.Fatalf("after step %d: %d attempts, want %d", i, got, tt.want[i])
				}
			}
		})
	}
}

func TestRelayHeartbeatsAndRetriesOnSessionOffline(t *testing.T) {
	offline := func(w http.ResponseWriter) {
		fakeJSON(w, http.StatusConflict, schema.Error{Code: schema.ErrorCodeSessionOffline, Message: "session offline"})
	}
	tests := []struct {
		name         string
		method, path string
		body         string
		upstream     string
		refusals     int                         // how many times the report is refused before it is accepted
		heartbeat    func(w http.ResponseWriter) // nil for the fake's own answer
		other        func(w http.ResponseWriter) // the refusal instead of session_offline
		wantStatus   int
		wantCode     string
		wantOrder    []string
	}{
		{name: "start run", method: "POST", path: "/v1/runs", body: `{"channel":"ops","task":"t"}`, upstream: "/v1/sessions/7/runs",
			refusals: 1, wantStatus: http.StatusCreated,
			wantOrder: []string{"/v1/sessions/7/runs", "/v1/sessions/7/heartbeat", "/v1/sessions/7/runs"}},
		{name: "run state", method: "POST", path: "/v1/runs/1/state", body: `{"state":"running"}`, upstream: "/v1/runs/1/state",
			refusals: 1, wantStatus: http.StatusOK,
			wantOrder: []string{"/v1/runs/1/state", "/v1/sessions/7/heartbeat", "/v1/runs/1/state"}},
		{name: "activity", method: "POST", path: "/v1/runs/1/activity", body: `{"kind":"command_started","summary":"s"}`, upstream: "/v1/runs/1/activity",
			refusals: 1, wantStatus: http.StatusCreated,
			wantOrder: []string{"/v1/runs/1/activity", "/v1/sessions/7/heartbeat", "/v1/runs/1/activity"}},
		{name: "still offline after the heartbeat is passed back, with no second try", method: "POST", path: "/v1/runs/1/state", body: `{"state":"running"}`, upstream: "/v1/runs/1/state",
			refusals: 2, wantStatus: http.StatusConflict, wantCode: schema.ErrorCodeSessionOffline,
			wantOrder: []string{"/v1/runs/1/state", "/v1/sessions/7/heartbeat", "/v1/runs/1/state"}},
		{name: "a heartbeat that fails passes the first refusal back", method: "POST", path: "/v1/runs/1/state", body: `{"state":"running"}`, upstream: "/v1/runs/1/state",
			refusals: 1, wantStatus: http.StatusConflict, wantCode: schema.ErrorCodeSessionOffline,
			heartbeat: func(w http.ResponseWriter) {
				fakeJSON(w, http.StatusTooManyRequests, schema.Error{Code: schema.ErrorCodeTooManySessions, Message: "too many sessions"})
			},
			wantOrder: []string{"/v1/runs/1/state", "/v1/sessions/7/heartbeat"}},
		{name: "another 409 passes through", method: "POST", path: "/v1/runs/1/state", body: `{"state":"queued"}`, upstream: "/v1/runs/1/state",
			refusals: 1, wantStatus: http.StatusConflict, wantCode: schema.ErrorCodeInvalidTransition,
			other: func(w http.ResponseWriter) {
				fakeJSON(w, http.StatusConflict, schema.Error{Code: schema.ErrorCodeInvalidTransition, Message: "no"})
			},
			wantOrder: []string{"/v1/runs/1/state"}},
		{name: "a 409 whose message only says offline passes through", method: "POST", path: "/v1/runs/1/activity", body: `{"kind":"command_started","summary":"s"}`, upstream: "/v1/runs/1/activity",
			refusals: 1, wantStatus: http.StatusConflict, wantCode: schema.ErrorCodeInvalidTransition,
			other: func(w http.ResponseWriter) {
				fakeJSON(w, http.StatusConflict, schema.Error{Code: schema.ErrorCodeInvalidTransition, Message: "session_offline"})
			},
			wantOrder: []string{"/v1/runs/1/activity"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := startRelay(t, nil)
			r.startRun(t)
			before := r.fake.count()
			var mu sync.Mutex
			refused := 0
			r.fake.mu.Lock()
			r.fake.override = func(w http.ResponseWriter, req *http.Request) bool {
				switch req.URL.Path {
				case "/v1/sessions/7/heartbeat":
					if tt.heartbeat == nil {
						return false
					}
					tt.heartbeat(w)
					return true
				case tt.upstream:
					mu.Lock()
					defer mu.Unlock()
					if refused < tt.refusals {
						refused++
						if tt.other != nil {
							tt.other(w)
						} else {
							offline(w)
						}
						return true
					}
				}
				return false
			}
			r.fake.mu.Unlock()
			status, _, body := r.do(t, tt.method, tt.path, tt.body)
			if status != tt.wantStatus {
				t.Fatalf("status = %d %s, want %d", status, body, tt.wantStatus)
			}
			if tt.wantCode != "" && decodeError(t, body).Code != tt.wantCode {
				t.Errorf("body = %s, want code %s", body, tt.wantCode)
			}
			var order []string
			calls := r.fake.calls[before:]
			for _, c := range calls {
				order = append(order, c.path)
			}
			if strings.Join(order, " ") != strings.Join(tt.wantOrder, " ") {
				t.Errorf("conchd saw %v, want %v", order, tt.wantOrder)
			}
			// A retry sends the very bytes the first forward sent.
			if len(calls) == 3 && !bytes.Equal(calls[2].body, calls[0].body) {
				t.Errorf("the retry sent %s, the first forward %s", calls[2].body, calls[0].body)
			}
		})
	}
}
