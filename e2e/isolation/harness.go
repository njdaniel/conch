package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// stopCtx is cancelled by the first SIGINT or SIGTERM. Every child and every
// wait watches it, so a signal unwinds the run through its normal exit path
// and teardown runs once, after nothing is in flight.
var stopCtx, stopCancel = context.WithCancel(context.Background())

var errInterrupted = errors.New("interrupted by a signal")

// harness is the state of one run: its output, the secrets that must never be
// printed, the requests it made, and what to tear down.
type harness struct {
	repo   string
	tmp    string
	conchd string // the binary this run built
	kit    *lkit.Kit

	mu      sync.Mutex
	secrets map[string]string
	// private are values redacted from what the run prints but not searched
	// for in logs: a room name, which lk prints by design.
	private map[string]bool
	calls   map[string]bool // "METHOD /path" of every request made
	logs    []namedFile
	cleanup []func()
}

type namedFile struct{ what, path string }

func newHarness() (*harness, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return nil, errors.New("run this from the module root: go run ./e2e/isolation")
	}
	tmp, err := os.MkdirTemp("", "isolation-check-")
	if err != nil {
		return nil, err
	}
	h := &harness{repo: repo, tmp: tmp, secrets: map[string]string{}, private: map[string]bool{}, calls: map[string]bool{}}
	h.kit = lkit.New(kitHost{h}, stopCtx, "isolation-check")
	return h, nil
}

func (h *harness) say(format string, args ...any) {
	fmt.Println(h.redact(fmt.Sprintf(format, args...)))
}

func (h *harness) secret(label, value string) {
	if value == "" {
		return
	}
	h.mu.Lock()
	h.secrets[value] = label
	h.mu.Unlock()
}

// hide registers a value that is redacted from the run's output but may
// appear in a log.
func (h *harness) hide(label, value string) {
	if value == "" {
		return
	}
	h.mu.Lock()
	h.secrets[value] = label
	h.private[value] = true
	h.mu.Unlock()
}

// redact replaces every secret in s, and anything shaped like a JWT.
func (h *harness) redact(s string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v, label := range h.secrets {
		s = strings.ReplaceAll(s, v, "["+label+" redacted]")
	}
	return lkit.JWT.ReplaceAllString(s, "[jwt redacted]")
}

func (h *harness) mkdir(prefix string) (string, error) { return os.MkdirTemp(h.tmp, prefix) }

func (h *harness) onCleanup(f func()) {
	h.mu.Lock()
	h.cleanup = append(h.cleanup, f)
	h.mu.Unlock()
}

// teardown runs every cleanup, last registered first, and removes the temp tree.
func (h *harness) teardown() {
	h.mu.Lock()
	fs := h.cleanup
	h.cleanup = nil
	h.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintln(os.Stderr, "isolation-check: a clean-up step panicked:", h.redact(fmt.Sprint(r)))
				}
			}()
			fs[i]()
		}()
	}
	_ = os.RemoveAll(h.tmp)
}

// called records a request, for the check that the run used the public
// routes only.
func (h *harness) called(method, path string) {
	h.mu.Lock()
	h.calls[method+" "+strings.SplitN(path, "?", 2)[0]] = true
	h.mu.Unlock()
}

func (h *harness) allCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.calls))
	for c := range h.calls {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// kitHost is the harness as the LiveKit kit sees it.
type kitHost struct{ h *harness }

func (k kitHost) Mkdir(prefix string) (string, error) { return k.h.mkdir(prefix) }
func (k kitHost) OnCleanup(f func())                  { k.h.onCleanup(f) }
func (k kitHost) Secret(label, value string)          { k.h.secret(label, value) }
func (k kitHost) Say(format string, args ...any)      { k.h.say(format, args...) }
func (k kitHost) Redact(s string) string              { return k.h.redact(s) }
func (k kitHost) Log(what, path string) {
	k.h.mu.Lock()
	k.h.logs = append(k.h.logs, namedFile{what, path})
	k.h.mu.Unlock()
}

// scanLogs checks that no credential, ticket or token of the run is in a log
// the run kept: conchd's and lk's.
func (h *harness) scanLogs() error {
	h.mu.Lock()
	logs := append([]namedFile(nil), h.logs...)
	secrets := make(map[string]string, len(h.secrets))
	for v, l := range h.secrets {
		if !h.private[v] {
			secrets[v] = l
		}
	}
	h.mu.Unlock()
	for _, l := range logs {
		data, err := os.ReadFile(l.path) // #nosec G304 -- a path under this run's temp dir
		if err != nil {
			return fmt.Errorf("read %s: %w", l.what, err)
		}
		for v, label := range secrets {
			if bytes.Contains(data, []byte(v)) {
				return fmt.Errorf("secret leak: %s appears in %s", label, l.what)
			}
		}
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// waitFor polls fn every 20 ms until it reports true, the deadline passes or
// the run is stopped. fn describes what it saw, for the timeout message.
func waitFor(what string, within time.Duration, fn func() (bool, string)) error {
	deadline := time.Now().Add(within)
	last := "nothing observed"
	for {
		ok, saw := fn()
		if ok {
			return nil
		}
		if saw != "" {
			last = saw
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s (last saw: %s)", within, what, last)
		}
		select {
		case <-stopCtx.Done():
			return fmt.Errorf("waiting for %s: %w", what, errInterrupted)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ------------------------------------------------------------------- conchd

func (h *harness) build() error {
	dir, err := h.mkdir("bin-")
	if err != nil {
		return err
	}
	h.conchd = filepath.Join(dir, "conchd")
	cmd := lkit.GroupCommand(stopCtx, "go", "build", "-o", h.conchd, "./cmd/conchd")
	cmd.Dir = h.repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/conchd: %w\n%s", err, out)
	}
	return nil
}

// cleanEnv is the caller's environment without anything that would change who
// conchd is or which LiveKit it talks to.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CONCHD_") ||
			strings.HasPrefix(kv, "LIVEKIT_") || strings.HasPrefix(kv, "XDG_") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

type conchdProc struct {
	cmd     *exec.Cmd
	baseURL string
	op      api
	done    chan struct{}
}

// startConchd bootstraps an operator into a fresh data directory and serves
// it, configured for srv when there is one.
func (h *harness) startConchd(srv *lkit.Server) (*conchdProc, error) {
	root, err := h.mkdir("conchd-")
	if err != nil {
		return nil, err
	}
	dataDir, home := filepath.Join(root, "data"), filepath.Join(root, "home")
	for _, d := range []string{dataDir, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	env := append(cleanEnv(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"))
	boot := lkit.GroupCommand(stopCtx, h.conchd, "bootstrap-operator", "--data", dataDir, "--name", "isolation-operator")
	boot.Env = env
	out, err := boot.Output()
	if err != nil {
		return nil, fmt.Errorf("bootstrap-operator: %w", err)
	}
	opToken := strings.TrimSpace(string(out))
	if opToken == "" || strings.ContainsAny(opToken, " \n") {
		return nil, fmt.Errorf("bootstrap-operator must print the token alone, got %d bytes", len(out))
	}
	h.secret("the operator's credential", opToken)
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--data", dataDir, "--listen", addr}
	if srv != nil {
		args = append(args, "--livekit-url", srv.WSURL)
		env = append(env, "CONCHD_LIVEKIT_API_KEY="+srv.Key, "CONCHD_LIVEKIT_API_SECRET="+srv.Secret)
	}
	logPath := filepath.Join(root, "conchd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this run's temp dir
	if err != nil {
		return nil, err
	}
	cmd := lkit.GroupStart(h.conchd, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	p := &conchdProc{cmd: cmd, baseURL: "http://" + addr, done: make(chan struct{})}
	p.op = api{h: h, base: p.baseURL, token: opToken, who: "the operator"}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(p.done) }()
	h.onCleanup(func() {
		_ = lkit.KillGroup(cmd)
		<-p.done
	})
	h.mu.Lock()
	h.logs = append(h.logs, namedFile{"conchd's log", logPath})
	h.mu.Unlock()
	return p, waitFor("conchd to answer /healthz", 20*time.Second, func() (bool, string) {
		resp, err := http.Get(p.baseURL + "/healthz") // #nosec G107 -- this run's own conchd
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode)
	})
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

// --------------------------------------------------------------------- REST

// api is a REST client acting as one principal (or as nobody, with no token).
type api struct {
	h           *harness
	base, token string
	who         string
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// send makes one request with a raw body and returns the status and body.
func (a api) send(method, path, contentType string, body []byte) (int, []byte, error) {
	status, data, _, err := a.request(method, path, contentType, body)
	return status, data, err
}

// request is send, with the response's headers.
func (a api) request(method, path, contentType string, body []byte) (int, []byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(stopCtx, method, a.base+path, reader) // #nosec G107 -- this run's own conchd
	if err != nil {
		return 0, nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	a.h.called(method, req.URL.Path)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, lkit.UnwrapURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, resp.Header, err
}

// do makes one request with a JSON body (none when body is nil).
func (a api) do(method, path string, body any) (int, []byte, error) {
	if body == nil {
		return a.send(method, path, "", nil)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return a.send(method, path, "application/json", encoded)
}

// call makes a request that must succeed and decodes its answer into out.
func (a api) call(method, path string, body, out any) error {
	status, data, err := a.do(method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s as %s: %w", method, path, a.who, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s as %s: status %d: %s", method, path, a.who, status, a.h.redact(lkit.Truncate(string(data), 300)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ticket asks for a socket ticket for this credential.
func (a api) ticket() (string, error) {
	var t schema.WSTicketV1
	if err := a.call(http.MethodPost, "/v1/ws-tickets", nil, &t); err != nil {
		return "", err
	}
	a.h.secret("a socket ticket", t.Ticket)
	return t.Ticket, nil
}
