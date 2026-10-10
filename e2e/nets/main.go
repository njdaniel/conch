// Command nets-check is the V2 exit proof for nets and whispers (ADR-005,
// docs/design/nets-and-whispers.md): the roadmap's 2x4 squad layout in text,
// driven through the public surfaces of real conchd and conch binaries.
//
// The layout is one channel, "ops", with eight agents (a1..a4, b1..b4), a
// human "lead" and the operator, and three nets: alpha (a1..a4), bravo
// (b1..b4) and command (a1 and b1 as members, lead as a monitor). The
// program posts into every audience, then asserts, for every participant and
// every surface that participant uses, the exact set of message ids it sees:
// no extra id and no missing one. The expectations come from one table
// (scenario.go) that states, for every message, who may see it.
//
// Nonzero exit on any assertion failure; one "nets-check: PASS" line on
// success. Every credential is issued through the REST API; the conch CLI
// runs with a temporary HOME and config directory, never the user's own.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func main() {
	h := &harness{}
	// An interrupted run still removes what it made: the server process and
	// the temporary directories, one of which holds a signed-in CLI config.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupted
		h.cleanup()
		fmt.Fprintln(os.Stderr, "nets-check: interrupted")
		os.Exit(130)
	}()
	if err := run(h); err != nil {
		fmt.Fprintln(os.Stderr, "nets-check: FAIL:", h.redact(err.Error()))
		os.Exit(1)
	}
	fmt.Println("nets-check: PASS")
}

func run(h *harness) error {
	defer h.cleanup()
	bin, err := buildBinaries()
	if err != nil {
		return fmt.Errorf("build binaries: %w", err)
	}
	h.onCleanup(func() { _ = os.RemoveAll(bin.dir) })
	h.bin = bin

	proc, err := startConchd(h, bin)
	if err != nil {
		return err
	}
	h.onCleanup(func() { _ = proc.Stop() })
	h.proc = proc

	return scenario(h)
}

// onCleanup registers something to undo when the run ends, however it ends.
func (h *harness) onCleanup(fn func()) {
	h.cleanupMu.Lock()
	defer h.cleanupMu.Unlock()
	h.cleanups = append(h.cleanups, fn)
}

// cleanup undoes everything registered, last first, once.
func (h *harness) cleanup() {
	h.cleanupMu.Lock()
	fns := h.cleanups
	h.cleanups = nil
	h.cleanupMu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// step logs a passed step.
func step(format string, args ...any) {
	fmt.Printf("ok  %s\n", fmt.Sprintf(format, args...))
}

type binaries struct {
	dir    string
	conchd string
	conch  string
}

func buildBinaries() (binaries, error) {
	dir, err := os.MkdirTemp("", "nets-bin-")
	if err != nil {
		return binaries{}, err
	}
	b := binaries{dir: dir, conchd: filepath.Join(dir, "conchd"), conch: filepath.Join(dir, "conch")}
	if err := goBuild(b.conchd, "./cmd/conchd"); err != nil {
		_ = os.RemoveAll(dir)
		return binaries{}, err
	}
	if err := goBuild(b.conch, "./cmd/conch"); err != nil {
		_ = os.RemoveAll(dir)
		return binaries{}, err
	}
	return b, nil
}

func goBuild(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- out/pkg are this program's own constants and temp paths, not external input
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd // run from the module root, as every script in this repo does
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, output)
	}
	return nil
}

// ------------------------------------------------------------- server proc

type conchdProc struct {
	cmd     *exec.Cmd
	baseURL string
	dataDir string
	homeDir string
	// operatorToken is the credential bootstrap-operator printed.
	operatorToken string
}

// isolatedEnv is the environment of every process this program starts: the
// caller's, minus anything that says who they are, with HOME and the XDG
// directories pointing into a temporary directory.
func isolatedEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CONCHD_") ||
			strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"))
	return append(env, extra...)
}

// startConchd bootstraps an operator into a fresh data directory, then starts
// conchd on it with its defaults: authentication required.
func startConchd(h *harness, bin binaries) (*conchdProc, error) {
	home, err := os.MkdirTemp("", "nets-home-")
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(home, "conchd-data")
	p := &conchdProc{dataDir: dataDir, homeDir: home}
	fail := func(err error) (*conchdProc, error) { _ = p.Stop(); return nil, err }
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fail(err)
	}
	addr, err := freeAddr()
	if err != nil {
		return fail(err)
	}
	bootstrap := exec.Command(bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "nets-operator") // #nosec G204 -- bin.conchd is a binary this program just built into a temp dir; args are local constants
	bootstrap.Env = isolatedEnv(home)
	bootstrap.Stderr = os.Stderr
	tokenOut, err := bootstrap.Output()
	if err != nil {
		return fail(fmt.Errorf("bootstrap-operator: %w", err))
	}
	operatorToken := strings.TrimSpace(string(tokenOut))
	if operatorToken == "" || strings.ContainsAny(operatorToken, " \n") {
		return fail(fmt.Errorf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(tokenOut)))
	}
	h.secret(operatorToken)
	p.operatorToken = operatorToken
	p.baseURL = "http://" + addr

	cmd := exec.Command(bin.conchd, "serve", "--data", dataDir, "--listen", addr) // #nosec G204 -- bin.conchd is a binary this program just built into a temp dir; args are local constants
	cmd.Env = isolatedEnv(home)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	p.cmd = cmd
	if err := p.waitHealthy(10 * time.Second); err != nil {
		return fail(err)
	}
	return p, nil
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func (p *conchdProc) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.baseURL + "/healthz") // #nosec G107 -- this harness's own conchd instance
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("conchd did not become healthy within %s", timeout)
}

// Stop kills conchd (if it runs) and removes everything it was given.
func (p *conchdProc) Stop() error {
	p.halt()
	return os.RemoveAll(p.homeDir)
}

// halt stops the server process but keeps the data directory, so the audit
// log can be read from the database, as e2e/dogfood does, until audit export
// (#21) exists.
func (p *conchdProc) halt() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd = nil
	}
}

func (p *conchdProc) auditEvents() ([]store.AuditEvent, error) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(p.dataDir, "conch.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	return st.ListAuditEvents(ctx, 0, 100000)
}

// ------------------------------------------------------------------ REST

// api is a REST client acting as one principal: every request carries its
// bearer credential.
type api struct{ baseURL, token string }

// do sends one request and returns the status and body. A non-nil body is
// sent as JSON.
func (a api) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.baseURL+path, reader) // #nosec G107 -- this harness's own conchd instance, not external input
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// call sends a request that must succeed (2xx) and decodes the response into
// out when out is non-nil. A failure quotes the status and the error code
// only, never the body: a body can hold message text.
func (a api) call(method, path string, body, out any) error {
	_, err := a.callRaw(method, path, body, out)
	return err
}

// callRaw is call, and also returns the response body as it arrived, for a
// caller that checks the wire itself.
func (a api) callRaw(method, path string, body, out any) ([]byte, error) {
	status, respBody, err := a.do(method, path, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s %s: status %d (%s)", method, path, status, errorCode(respBody))
	}
	if out == nil {
		return respBody, nil
	}
	if err := decodeStrict(respBody, out); err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return respBody, nil
}

// httpClient bounds every request, so a server that stops answering fails the
// run instead of hanging it.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// decodeStrict decodes a response or a frame and refuses a field the schema
// type does not have. The wire shapes are closed: a server that began sending,
// say, a message's resolved recipients in an extra field would be telling
// readers something the schema does not let it say, and a lenient decoder
// would never notice. The error names the field, never the content.
func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the answer does not fit %T: %w", out, err)
	}
	return nil
}

// errorCode is the schema.Error code of a response body, or a marker.
func errorCode(body []byte) string {
	var e schema.Error
	if json.Unmarshal(body, &e) != nil || e.Code == "" {
		return "no error code"
	}
	return e.Code
}

// ---------------------------------------------------------------- the CLI

// conchUser runs the conch CLI as one person: its own HOME and config
// directory (so the real user's is never touched) and no inherited identity.
type conchUser struct {
	bin  string
	home string
	env  []string
}

func newConchUser(binPath, serverURL string) (*conchUser, error) {
	dir, err := os.MkdirTemp("", "nets-conch-home-")
	if err != nil {
		return nil, err
	}
	return &conchUser{bin: binPath, home: dir, env: isolatedEnv(dir, "CONCH_SERVER="+serverURL)}, nil
}

func (u *conchUser) cleanup() { _ = os.RemoveAll(u.home) }

// run executes one conch subcommand with stdin as its standard input and
// returns its combined output.
func (u *conchUser) run(stdin string, args ...string) (string, error) {
	cmd := exec.Command(u.bin, args...) // #nosec G204 -- u.bin is this program's own just-built conch binary; args are local constants and run-time message text
	cmd.Env = u.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("conch %s: %w", args[0], err)
	}
	return string(out), nil
}
