// Command p2-hardening-check is the P2 exit gate (issue #81, ROADMAP "P2 —
// Hardening"): one command that proves the hardened, authenticated,
// capability-scoped system works as one deployment. It builds conchd and
// conch, loads a database written by a P1 conchd (testdata/p1.sql), upgrades
// it, and then drives the real binaries through everything P2 added, on that
// upgraded database:
//
//   - upgrade: every P1 message, approval, decision, resolution and audit row
//     is still there and still served; the approvals P1 left open are decided
//     afterwards; the webhook URL P1 handed out still works;
//   - identity: no request is served without a credential; two agents hold
//     different manifests and credentials; a rotated or revoked credential
//     stops working at once and the principal and its history stay;
//   - capability enforcement: what a manifest grants works and nothing else
//     does; an agent can never decide an approval; the request, notify,
//     decide, await chain runs with ntfy answering and with ntfy down; one
//     agent at its rate limit does not slow another;
//   - what P2 gave people: the channel list, an approval read by id, audit
//     export, search, files, threads, and the principal-review report posted
//     by a plain command with no scheduler;
//   - operations: the audit trail and the server log hold no credential, the
//     P1 canary (e2e/dogfood) still passes, and nothing is left behind.
//
// Nonzero exit on any failed assertion; one "p2-hardening-check: PASS" line
// on success. Two steps need Linux (the TUI on a pseudo-terminal, and
// ops/principal-review.sh, which is written for GNU tools); elsewhere each
// prints a NOT RUN line, the last line counts them, and with CI set that is a
// failure. Run it from the module root: go run ./e2e/p2-hardening
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

const name = "p2-hardening-check"

// runCtx is cancelled when the run is interrupted. Every request, every child
// process and every wait hangs off it, so an interrupted run stops at once.
var runCtx = context.Background()

// pause waits for d, or returns the reason the run was interrupted.
func pause(d time.Duration) error {
	select {
	case <-runCtx.Done():
		return runCtx.Err()
	case <-time.After(d):
		return nil
	}
}

func main() {
	skipDogfood := flag.Bool("skip-dogfood", false, "do not run e2e/dogfood at the end (for a caller that runs it itself)")
	flag.Parse()

	h := &harness{skipDogfood: *skipDogfood}
	// An interrupted run still removes what it made: the server process and
	// the temporary directory, which holds signed-in CLI configs. The signal
	// cancels runCtx, so whatever the run is doing fails, run returns and its
	// own cleanup runs, with nothing else still creating files. Should the
	// run not return, it is stopped from here after a wait.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx = ctx
	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			h.cleanup()
			fmt.Fprintln(os.Stderr, name+": interrupted, and stopped by force")
			os.Exit(130)
		}
	}()
	err := run(h)
	close(finished)
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, name+": interrupted")
		os.Exit(130)
	}
	if err == nil {
		// run has cleaned up by now; a run that leaves something behind fails.
		err = h.leftovers()
	}
	// A step that could not run here is said so, on its own line and on the
	// last one. Where CI is set it is a failure: a gate that skips is green
	// for the wrong reason.
	if err == nil && len(h.notRun) > 0 && os.Getenv("CI") != "" {
		err = fmt.Errorf("%d steps could not run on this system, and CI is set: %s", len(h.notRun), strings.Join(h.notRun, "; "))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": FAIL:", h.redact(err.Error()))
		os.Exit(1)
	}
	if len(h.notRun) > 0 {
		fmt.Printf("%s: PASS, with %d steps NOT RUN on this system: %s\n", name, len(h.notRun), strings.Join(h.notRun, "; "))
		return
	}
	fmt.Println(name + ": PASS")
}

func run(h *harness) (err error) {
	defer h.cleanup()
	// On a failure, the end of the server's log is the first thing anyone
	// will want. It is read before cleanup removes it.
	defer func() {
		if err != nil && runCtx.Err() == nil {
			h.showServerLog()
		}
	}()

	root, err := os.MkdirTemp("", "p2-hardening-")
	if err != nil {
		return err
	}
	h.root = root
	h.onCleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []string{"bin", "home", "tmp", "users"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return err
		}
	}
	if h.bin, err = buildBinaries(filepath.Join(root, "bin")); err != nil {
		return fmt.Errorf("build binaries: %w", err)
	}

	phases := []struct {
		title string
		run   func() error
	}{
		{"upgrade from a P1 database", h.upgrade},
		{"identity", h.identity},
		{"capability enforcement", h.capabilities},
		{"the webhook P1 handed out", h.webhook},
		{"the approval chain", h.approvalChain},
		{"rate limits", h.rateLimits},
		{"channel list", h.channelList},
		{"search", h.search},
		{"files", h.files},
		{"threads", h.threads},
		{"principal-review report", h.principalReview},
		{"audit export and credential hygiene", h.auditAndHygiene},
	}
	for _, p := range phases {
		fmt.Printf("== %s ==\n", p.title)
		if err := p.run(); err != nil {
			return fmt.Errorf("%s: %w", p.title, err)
		}
	}

	// The server is stopped before the P1 canary runs: that program starts
	// its own, and this one's work is done.
	h.proc.halt()
	if h.skipDogfood {
		fmt.Println("== P1 canary (e2e/dogfood): skipped on request ==")
		return nil
	}
	fmt.Println("== P1 canary (e2e/dogfood) ==")
	return h.dogfood()
}

// ------------------------------------------------------------------ harness

type harness struct {
	skipDogfood bool

	// root holds every file this run makes: binaries, the data directory,
	// each CLI user's home, and the temporary files of every child process.
	root string
	bin  binaries
	proc *conchdProc
	ntfy *fakeNtfy

	secretsMu sync.Mutex
	secrets   []string

	// notRun names the steps this system cannot run (they need Linux).
	notRun []string

	p1 p1Database

	// The principals of the run. nick, ana, leviathan and scout were created
	// by P1 and came through the upgrade; the rest are made here.
	nick, ana, outsider, reviewer *person
	leviathan, scout, courier     *person
	opsID, labID, operatorID      int64
	// pendingForChain is the approval the capability phase raises and leaves
	// pending for the approval chain to decide.
	pendingForChain int64
	// degraded is the approval raised and decided while ntfy was down: the
	// only one conchd's log may hold errors about.
	degraded int64

	cleanupMu sync.Mutex
	cleanups  []func()
}

// person is one principal and the ways it acts: a REST credential, the conch
// CLI signed in as it (people), an MCP session (agents).
type person struct {
	name  string
	id    int64
	token string
	cli   *conchUser
	mcp   *mcpclient.Client
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

// secret records a credential, so that no failure message prints it and the
// last phase can look for it where it must never be.
func (h *harness) secret(t string) {
	if t == "" {
		return
	}
	h.secretsMu.Lock()
	h.secrets = append(h.secrets, t)
	h.secretsMu.Unlock()
}

func (h *harness) allSecrets() []string {
	h.secretsMu.Lock()
	defer h.secretsMu.Unlock()
	return append([]string(nil), h.secrets...)
}

// redact hides every credential this run knows from a failure message.
func (h *harness) redact(s string) string {
	for _, t := range h.allSecrets() {
		s = strings.ReplaceAll(s, t, "[token]")
	}
	return s
}

func (h *harness) op() api { return api{baseURL: h.proc.baseURL, token: h.proc.operatorToken} }

func (h *harness) as(p *person) api { return api{baseURL: h.proc.baseURL, token: p.token} }

func (h *harness) anonymous() api { return api{baseURL: h.proc.baseURL} }

// step logs a passed step.
func step(format string, args ...any) {
	fmt.Printf("ok  %s\n", fmt.Sprintf(format, args...))
}

// skip records a step this system cannot run, and says so where the step's
// line would be.
func (h *harness) skip(what, why string) {
	h.notRun = append(h.notRun, what)
	fmt.Printf("NOT RUN  %s: %s\n", what, why)
}

// leftovers reports anything the run should have removed and did not: its
// directory, its server process, or something still listening on its port.
func (h *harness) leftovers() error {
	if h.root != "" {
		switch _, err := os.Stat(h.root); {
		case err == nil:
			return fmt.Errorf("the run's directory %s is still there", h.root)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("cannot tell whether the run's directory is gone: %w", err)
		}
	}
	if h.proc == nil {
		return nil
	}
	if h.proc.cmd != nil {
		return errors.New("conchd is still running after cleanup")
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.proc.baseURL, "http://"), time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("something still listens on %s after cleanup", h.proc.baseURL)
	}
	step("nothing left behind: the run's directory is gone, conchd has exited and its port is closed")
	return nil
}

// ---------------------------------------------------------------- binaries

type binaries struct {
	conchd string
	conch  string
}

func buildBinaries(dir string) (binaries, error) {
	b := binaries{conchd: filepath.Join(dir, "conchd"), conch: filepath.Join(dir, "conch")}
	if err := goBuild(b.conchd, "./cmd/conchd"); err != nil {
		return binaries{}, err
	}
	if err := goBuild(b.conch, "./cmd/conch"); err != nil {
		return binaries{}, err
	}
	return b, nil
}

func goBuild(out, pkg string) error {
	cmd := exec.CommandContext(runCtx, "go", "build", "-o", out, pkg) // #nosec G204 -- out/pkg are this program's own constants and temp paths, not external input
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s (run this program from the module root): %w\n%s", pkg, err, output)
	}
	return nil
}

// isolatedEnv is the environment of every process this program starts: the
// caller's, minus anything that says who they are, with HOME, the XDG
// directories and the temporary directory all inside the run's own.
func (h *harness) isolatedEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CONCHD_") ||
			strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home,
		"TMPDIR="+filepath.Join(h.root, "tmp"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"))
	return append(env, extra...)
}

// ------------------------------------------------------------- server proc

type conchdProc struct {
	cmd     *exec.Cmd
	baseURL string
	dataDir string
	logPath string
	logFile *os.File
	// operatorToken is the credential bootstrap-operator printed.
	operatorToken string
}

// startConchd turns the data directory into a running instance the way an
// operator upgrading from P1 does: bootstrap-operator, which also applies
// every migration, then serve with the defaults (authentication required).
// The webhook URLs P1 handed out are kept; that is the operator's choice to
// make and this run makes it, to show the migration carries them.
func (h *harness) startConchd(dataDir string) error {
	home := filepath.Join(h.root, "home")
	p := &conchdProc{dataDir: dataDir, logPath: filepath.Join(h.root, "conchd.log")}
	h.proc = p
	h.onCleanup(p.halt)

	addr, err := freeAddr()
	if err != nil {
		return err
	}
	p.baseURL = "http://" + addr

	logFile, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	p.logFile = logFile

	bootstrap := exec.CommandContext(runCtx, h.bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "gate-operator", "--keep-existing-credentials") // #nosec G204 -- h.bin.conchd is a binary this program just built into a temp dir; args are local constants
	bootstrap.Env = h.isolatedEnv(home)
	bootstrap.Stderr = logFile
	tokenOut, err := bootstrap.Output()
	if err != nil {
		return fmt.Errorf("bootstrap-operator on the P1 database: %w", err)
	}
	p.operatorToken = strings.TrimSpace(string(tokenOut))
	if p.operatorToken == "" || strings.ContainsAny(p.operatorToken, " \n") {
		return fmt.Errorf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(tokenOut))
	}
	h.secret(p.operatorToken)

	cmd := exec.CommandContext(runCtx, h.bin.conchd, "serve", "--data", dataDir, "--listen", addr, // #nosec G204 -- h.bin.conchd is a binary this program just built into a temp dir; args are local constants and this run's own addresses
		"--ntfy-server", h.ntfy.url(), "--ntfy-topic", ntfyTopic, "--ntfy-urgent-topic", ntfyUrgentTopic)
	cmd.Env = h.isolatedEnv(home)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd = cmd
	return p.waitHealthy(15 * time.Second)
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
		resp, err := (api{baseURL: p.baseURL}).send(http.MethodGet, "/healthz", nil)
		if err == nil && resp.status == http.StatusOK {
			return nil
		}
		if err := pause(50 * time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("conchd did not become healthy within %s", timeout)
}

// halt stops the server process, if it runs. The data directory goes with the
// run's root.
func (p *conchdProc) halt() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd = nil
	}
	if p.logFile != nil {
		_ = p.logFile.Close()
		p.logFile = nil
	}
}

// serverLog is everything conchd and bootstrap-operator have written so far.
func (h *harness) serverLog() (string, error) {
	if h.proc == nil {
		return "", nil
	}
	data, err := os.ReadFile(h.proc.logPath)
	return string(data), err
}

// showServerLog prints the end of the server's log, credentials hidden.
func (h *harness) showServerLog() {
	log, err := h.serverLog()
	if err != nil || log == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	fmt.Fprintln(os.Stderr, "--- the end of conchd's log ---")
	fmt.Fprintln(os.Stderr, h.redact(strings.Join(lines, "\n")))
	fmt.Fprintln(os.Stderr, "--- end ---")
}

// ---------------------------------------------------------------- fake ntfy

const (
	ntfyTopic       = "approvals"
	ntfyUrgentTopic = "approvals-urgent"
)

// fakeNtfy stands in for an ntfy server. While up it records every delivery
// and answers 200. Taken down it keeps its port, so nothing else can answer
// there, and drops every connection without an answer: to conchd that is an
// ntfy that cannot be reached.
type fakeNtfy struct {
	listener net.Listener
	srv      *http.Server
	mu       sync.Mutex
	down     bool
	hits     []ntfyHit
}

type ntfyHit struct {
	Topic    string
	Title    string
	Priority string
}

func newFakeNtfy() (*fakeNtfy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &fakeNtfy{listener: l}
	f.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(f.serve)}
	go func() { _ = f.srv.Serve(l) }()
	return f, nil
}

func (f *fakeNtfy) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	down := f.down
	if !down {
		f.hits = append(f.hits, ntfyHit{
			Topic:    strings.TrimPrefix(r.URL.Path, "/"),
			Title:    r.Header.Get("Title"),
			Priority: r.Header.Get("Priority"),
		})
	}
	f.mu.Unlock()
	if !down {
		w.WriteHeader(http.StatusOK)
		return
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

func (f *fakeNtfy) url() string { return "http://" + f.listener.Addr().String() }

func (f *fakeNtfy) close() { _ = f.srv.Close() }

func (f *fakeNtfy) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

// delivered returns the deliveries answered so far.
func (f *fakeNtfy) delivered() []ntfyHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ntfyHit(nil), f.hits...)
}

// ------------------------------------------------------------------ REST

// api is a REST client acting as one principal: every request carries its
// bearer credential (or none, when token is empty).
type api struct{ baseURL, token string }

// httpClient bounds every request, so a server that stops answering fails the
// run instead of hanging it.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// response is what one request came back with.
type response struct {
	status int
	header http.Header
	body   []byte
}

// send sends one request. A []byte or io.Reader body is sent as it is; any
// other non-nil body is sent as JSON.
func (a api) send(method, path string, body any) (response, error) {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		reader, contentType = bytes.NewReader(encoded), "application/json"
	}
	req, err := http.NewRequestWithContext(runCtx, method, a.baseURL+path, reader) // #nosec G107 -- this harness's own conchd instance, not external input
	if err != nil {
		return response{}, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: respBody}, nil
}

// call sends a request that must succeed (2xx) and decodes the response into
// out when out is non-nil. A failure quotes the status and the error code
// only, never the body: a body can hold message text.
func (a api) call(method, path string, body, out any) error {
	resp, err := a.send(method, path, body)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		return fmt.Errorf("%s %s: status %d (%s)", method, path, resp.status, errorCode(resp.body))
	}
	if out == nil {
		return nil
	}
	if err := decodeStrict(resp.body, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

// refused sends a request that must be refused with exactly this status and
// this error code, in the schema's error shape and nothing else. It returns
// the response for a caller that checks more.
func (a api) refused(what, method, path string, body any, status int, code string) (response, error) {
	resp, err := a.send(method, path, body)
	if err != nil {
		return response{}, fmt.Errorf("%s: %w", what, err)
	}
	if resp.status != status {
		return response{}, fmt.Errorf("%s: status %d (%s), want %d %s", what, resp.status, errorCode(resp.body), status, code)
	}
	var e schema.Error
	if err := decodeStrict(resp.body, &e); err != nil {
		return response{}, fmt.Errorf("%s: the refusal is not a schema.Error: %w", what, err)
	}
	if e.Code != code || e.Message == "" {
		return response{}, fmt.Errorf("%s: error code %q with a %d-byte message, want %q and a message", what, e.Code, len(e.Message), code)
	}
	return resp, nil
}

// decodeStrict decodes a response and refuses a field the schema type does
// not have: the wire shapes are closed, and a lenient decoder would never
// notice a server that began sending more. The error names the field, never
// the content.
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

// newConchUser makes a CLI user and signs it in with token through the real
// `conch login`, which reads the token from standard input.
func (h *harness) newConchUser(who, token string) (*conchUser, error) {
	home := filepath.Join(h.root, "users", who)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	u := &conchUser{bin: h.bin.conch, home: home, env: h.isolatedEnv(home, "CONCH_SERVER="+h.proc.baseURL)}
	out, err := u.run(token+"\n", "login")
	if err != nil {
		return nil, fmt.Errorf("conch login as %s: %w", who, err)
	}
	if !strings.Contains(out, who) {
		return nil, fmt.Errorf("conch login as %s did not name the principal it signed in", who)
	}
	return u, nil
}

// run executes one conch subcommand with stdin as its standard input and
// returns its combined output.
func (u *conchUser) run(stdin string, args ...string) (string, error) {
	stdout, stderr, err := u.runSplit(stdin, args...)
	return stdout + stderr, err
}

// runSplit is run with standard output and standard error kept apart.
func (u *conchUser) runSplit(stdin string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(runCtx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, u.bin, args...) // #nosec G204 -- u.bin is this program's own just-built conch binary; args are local constants and run-time message text
	cmd.Env = u.env
	cmd.Dir = u.home
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), errOut.String(), fmt.Errorf("conch %s: %w (%s)", args[0], err, firstLine(errOut.String()))
	}
	return out.String(), errOut.String(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --------------------------------------------------------------- principals

func (h *harness) createPrincipal(kind schema.PrincipalKind, who string) (int64, error) {
	var resp schema.CreatePrincipalResponse
	if err := h.op().call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: who}, &resp); err != nil {
		return 0, fmt.Errorf("create %s: %w", who, err)
	}
	return resp.Principal.ID, nil
}

// issue creates a bearer credential for a principal through the REST API and
// returns its id and the token, which the server shows once.
func (h *harness) issue(principalID int64, label string) (int64, string, error) {
	var resp schema.CreateCredentialResponseV1
	path := fmt.Sprintf("/v1/principals/%d/credentials", principalID)
	if err := h.op().call(http.MethodPost, path, schema.CreateCredentialRequestV1{Label: label}, &resp); err != nil {
		return 0, "", fmt.Errorf("issue a credential for principal %d: %w", principalID, err)
	}
	if resp.Token == "" || resp.Credential.ID == 0 || resp.Credential.PrincipalID != principalID {
		return 0, "", fmt.Errorf("issuing a credential for principal %d returned no token, no id or another principal", principalID)
	}
	h.secret(resp.Token)
	return resp.Credential.ID, resp.Token, nil
}

func (h *harness) createChannel(channel string) (int64, error) {
	var resp schema.CreateChannelResponse
	if err := h.op().call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: channel}, &resp); err != nil {
		return 0, fmt.Errorf("create channel %s: %w", channel, err)
	}
	return resp.Channel.ID, nil
}

func (h *harness) addMember(channel string, p *person) error {
	if err := h.op().call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channel, p.id), nil, nil); err != nil {
		return fmt.Errorf("add %s to %s: %w", p.name, channel, err)
	}
	return nil
}

// newHuman creates a person with a credential and a signed-in CLI, a member
// of the given channels.
func (h *harness) newHuman(who string, channels ...string) (*person, error) {
	id, err := h.createPrincipal(schema.PrincipalHuman, who)
	if err != nil {
		return nil, err
	}
	p := &person{name: who, id: id}
	if err := h.signIn(p); err != nil {
		return nil, err
	}
	for _, c := range channels {
		if err := h.addMember(c, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// signIn gives an existing person a credential and a CLI signed in with it.
func (h *harness) signIn(p *person) error {
	_, token, err := h.issue(p.id, p.name)
	if err != nil {
		return err
	}
	p.token = token
	p.cli, err = h.newConchUser(p.name, token)
	return err
}

// connect opens an MCP session for an agent with its current token.
func (h *harness) connect(p *person) error {
	p.mcp = mcpclient.New(h.proc.baseURL, p.token)
	if err := p.mcp.Initialize(runCtx, name); err != nil {
		return fmt.Errorf("mcp initialize as %s: %w", p.name, err)
	}
	return nil
}

// tool calls an MCP tool as an agent and decodes its structured content.
func tool[T any](p *person, toolName string, args map[string]any) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(runCtx, 60*time.Second)
	defer cancel()
	raw, err := p.mcp.CallTool(ctx, toolName, args)
	if err != nil {
		return zero, fmt.Errorf("%s as %s: %w", toolName, p.name, err)
	}
	out, err := mcpclient.Decode[T](raw)
	if err != nil {
		return zero, fmt.Errorf("%s as %s: decode: %w", toolName, p.name, err)
	}
	return out, nil
}

// toolRefused calls an MCP tool that must be refused with this error code.
func toolRefused(p *person, what, toolName string, args map[string]any, code string) (*mcpclient.ToolError, error) {
	ctx, cancel := context.WithTimeout(runCtx, 60*time.Second)
	defer cancel()
	_, err := p.mcp.CallTool(ctx, toolName, args)
	if err == nil {
		return nil, fmt.Errorf("%s: %s as %s succeeded, want %s", what, toolName, p.name, code)
	}
	var te *mcpclient.ToolError
	if !errors.As(err, &te) {
		return nil, fmt.Errorf("%s: %s as %s failed outside the tool (%w), want the tool error %s", what, toolName, p.name, err, code)
	}
	if te.Code != code {
		return nil, fmt.Errorf("%s: %s as %s was refused with %q, want %q", what, toolName, p.name, te.Code, code)
	}
	return te, nil
}

// putManifest writes an agent's manifest.
func (h *harness) putManifest(p *person, caps []schema.Capability, limits []schema.RateLimit, grants ...schema.ChannelGrant) error {
	req := schema.PutAgentManifestRequestV1{DisplayName: p.name, Tier: schema.AgentTierA, Capabilities: caps, Channels: grants, RateLimits: limits}
	if err := h.op().call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", p.id), req, nil); err != nil {
		return fmt.Errorf("write %s's manifest: %w", p.name, err)
	}
	return nil
}

func readPost(channelID int64) schema.ChannelGrant {
	return schema.ChannelGrant{ChannelID: channelID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}
}

// dogfood runs the P1 canary as its own process, exactly as a person would.
func (h *harness) dogfood() error {
	ctx, cancel := context.WithTimeout(runCtx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./e2e/dogfood")
	// go run starts the program, which starts servers of its own: stopping
	// it must stop all of them.
	ownProcessGroup(cmd)
	// Its temporary files land in this run's directory, so they are removed
	// with it whatever happens to that process.
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(h.root, "tmp"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go run ./e2e/dogfood: %w\n%s", err, out)
	}
	if !strings.Contains(string(out), "dogfood-check: PASS") {
		return fmt.Errorf("go run ./e2e/dogfood exited 0 without its PASS line:\n%s", out)
	}
	step("go run ./e2e/dogfood passes")
	return nil
}
