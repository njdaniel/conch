package reviewpost

import (
	"bytes"
	"context"
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
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// exitNotPosted is the status the script documents for "the summary was not
// posted", whatever the reason.
const exitNotPosted = 3

// summaryBudget is SUMMARY_BUDGET in the script: the most a summary may take
// as a JSON string. serverBodyLimit is maxMessageBodyBytes in
// internal/server/messages.go, which the budget must stay under.
const (
	summaryBudget   = 30000
	serverBodyLimit = 1 << 20
)

const reviewChannel = "principal-review"

// prURL is what the stub gh prints for `gh pr create`.
const prURL = "https://github.com/njdaniel/conch/pull/4242"

var (
	repoRoot   string
	scriptPath string
	conchdBin  string
	conchBin   string
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviewpost:", err)
		return 1
	}
	repoRoot = root
	scriptPath = filepath.Join(root, "ops", "principal-review.sh")
	dir, err := os.MkdirTemp("", "reviewpost-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviewpost:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	conchdBin = filepath.Join(dir, "conchd")
	conchBin = filepath.Join(dir, "conch")
	for out, pkg := range map[string]string{conchdBin: "./cmd/conchd", conchBin: "./cmd/conch"} {
		cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- this test's own constants and temp paths
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "reviewpost: go build %s: %v\n%s", pkg, err, output)
			return 1
		}
	}
	return m.Run()
}

// ------------------------------------------------------------------ conchd

// server is one conchd on a loopback port, with authentication required (its
// default) and an operator bootstrapped offline.
type server struct {
	t             *testing.T
	cmd           *exec.Cmd
	addr          string
	baseURL       string
	dataDir       string
	logPath       string
	operatorToken string
}

func startServer(t *testing.T) *server {
	t.Helper()
	dataDir := t.TempDir()
	bootstrap := exec.Command(conchdBin, "bootstrap-operator", "--data", dataDir, "--name", "reviewpost-operator") // #nosec G204 -- a binary this test built; constant arguments
	tokenOut, err := bootstrap.Output()
	if err != nil {
		t.Fatalf("bootstrap-operator: %v", err)
	}
	token := strings.TrimSpace(string(tokenOut))
	if token == "" || strings.ContainsAny(token, " \n") {
		t.Fatalf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(tokenOut))
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	s := &server{
		t: t, addr: addr, baseURL: "http://" + addr, dataDir: dataDir,
		logPath: filepath.Join(t.TempDir(), "conchd.log"), operatorToken: token,
	}
	s.start()
	t.Cleanup(func() {
		s.stop()
		if t.Failed() {
			if out, err := os.ReadFile(s.logPath); err == nil {
				t.Logf("conchd log:\n%s", out)
			}
		}
	})
	return s
}

// start runs conchd on the server's data directory and address, and returns
// once it answers. After stop it starts the same instance again.
func (s *server) start() {
	s.t.Helper()
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(conchdBin, "serve", "--data", s.dataDir, "--listen", s.addr) // #nosec G204 -- a binary this test built; its own temp paths
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.cmd = cmd
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.baseURL + "/healthz") // #nosec G107 -- this test's own conchd
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatal("conchd did not become healthy within 10s")
}

// stop kills conchd. The data directory stays, so start can bring the same
// instance back.
func (s *server) stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	s.cmd = nil
}

// call sends one REST request with the given credential. The request must
// succeed; the response is decoded into out when out is non-nil.
func (s *server) call(token, method, path string, body, out any) {
	s.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.baseURL+path, reader) // #nosec G107 -- this test's own conchd
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			s.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
}

// channel creates a channel and makes the operator a member of it, so the
// test can read it back: operators get no read exemption.
func (s *server) channel(name string) int64 {
	s.t.Helper()
	var resp schema.CreateChannelResponse
	s.call(s.operatorToken, http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: name}, &resp)
	s.member(name, 1) // the bootstrapped operator is principal 1
	return resp.Channel.ID
}

func (s *server) member(channel string, principalID int64) {
	s.t.Helper()
	s.call(s.operatorToken, http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channel, principalID), nil, nil)
}

// principal creates a principal with a credential and membership of the
// given channels, and returns its id and token.
func (s *server) principal(kind schema.PrincipalKind, name string, channels ...string) (int64, string) {
	s.t.Helper()
	var created schema.CreatePrincipalResponse
	s.call(s.operatorToken, http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &created)
	id := created.Principal.ID
	var cred schema.CreateCredentialResponseV1
	s.call(s.operatorToken, http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", id), schema.CreateCredentialRequestV1{Label: name}, &cred)
	if cred.Token == "" {
		s.t.Fatalf("no token issued for %s", name)
	}
	for _, c := range channels {
		s.member(c, id)
	}
	return id, cred.Token
}

// messages reads a channel back through the API, as the operator.
func (s *server) messages(channel string) []schema.MessageV1 {
	s.t.Helper()
	var resp schema.ListMessagesResponseV1
	s.call(s.operatorToken, http.MethodGet, "/v1/channels/"+channel+"/messages?limit=100", nil, &resp)
	return resp.Messages
}

// bodies is the text of every message in a channel, oldest first.
func (s *server) bodies(channel string) []string {
	s.t.Helper()
	var out []string
	for _, m := range s.messages(channel) {
		out = append(out, m.Body)
	}
	return out
}

// post puts one message into a channel as the operator.
func (s *server) post(channel, body string) {
	s.t.Helper()
	s.call(s.operatorToken, http.MethodPost, "/v1/channels/"+channel+"/messages", schema.PostMessageRequestV1{Body: body}, nil)
}

// ------------------------------------------------------------- script runs

// scriptEnv is the world one test gives the script: a private HOME, conch
// configuration, log directories and a directory of stubs that comes first
// on PATH. Nothing of the developer's own identity is inherited.
type scriptEnv struct {
	t     *testing.T
	home  string
	stubs string
	// logDir is CONCH_REVIEW_LOG_DIR; agentLogDir is CONCH_AGENT_LOG_DIR,
	// where the lock shared with conch-agent lives.
	logDir      string
	agentLogDir string
	// calls is a file the stubs append one line to per invocation, which
	// gives the order things happened in across processes.
	calls string
	vars  map[string]string
}

func newScriptEnv(t *testing.T) *scriptEnv {
	t.Helper()
	base := t.TempDir()
	e := &scriptEnv{
		t: t, home: filepath.Join(base, "home"), stubs: filepath.Join(base, "stubs"),
		logDir: filepath.Join(base, "review-logs"), agentLogDir: filepath.Join(base, "agent-logs"),
		calls: filepath.Join(base, "calls"), vars: map[string]string{},
	}
	for _, dir := range []string{e.home, e.stubs, filepath.Join(base, "config")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.vars["PATH"] = e.stubs + string(os.PathListSeparator) + os.Getenv("PATH")
	e.vars["HOME"] = e.home
	e.vars["XDG_CONFIG_HOME"] = filepath.Join(base, "config")
	e.vars["CONCH_REVIEW_LOG_DIR"] = e.logDir
	e.vars["CONCH_AGENT_LOG_DIR"] = e.agentLogDir
	e.vars["CONCH_REVIEW_LOCK_WAIT"] = "0"
	e.vars["CLAUDE_BIN"] = filepath.Join(e.stubs, "claude")
	e.vars["CALLS"] = e.calls
	// git must not read the developer's configuration (hooks path, signing)
	// and must have an identity where the machine has none, as in CI.
	e.vars["GIT_CONFIG_GLOBAL"] = os.DevNull
	e.vars["GIT_CONFIG_NOSYSTEM"] = "1"
	e.vars["GIT_TERMINAL_PROMPT"] = "0"
	for _, who := range []string{"AUTHOR", "COMMITTER"} {
		e.vars["GIT_"+who+"_NAME"] = "reviewpost"
		e.vars["GIT_"+who+"_EMAIL"] = "reviewpost@example.invalid"
	}
	// The real ones must be out of reach in every test, not only in those
	// that expect a call: each of these fails and says so until a test
	// replaces it.
	for _, name := range []string{"claude", "gh", "systemctl"} {
		e.stub(name, fmt.Sprintf("printf 'UNEXPECTED %s %%s\\n' \"$*\" >>\"$CALLS\"\nexit 97\n", name))
	}
	// conch is the binary this test built, behind a wrapper that records the
	// subcommand so a test can see whether and when a post was attempted.
	e.stub("conch", fmt.Sprintf("printf 'conch %%s\\n' \"$1\" >>\"$CALLS\"\nexec %q \"$@\"\n", conchBin))
	return e
}

// stub writes an executable sh script of that name into the stub directory.
func (e *scriptEnv) stub(name, body string) {
	e.t.Helper()
	path := filepath.Join(e.stubs, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil { // #nosec G306 -- test executable in this test's private directory
		e.t.Fatal(err)
	}
}

// droppedPrefixes are the variables that must not leak in from whoever runs
// the tests: identities, tokens, locale, and a session bus a real systemctl
// could reach.
var droppedPrefixes = []string{
	"CONCH_", "CLAUDE_", "XDG_", "GIT_", "GH_", "GITHUB_", "DBUS_", "SSH_",
	"LC_", "LANG=", "LANGUAGE=", "HOME=", "PATH=", "CALLS=",
}

func (e *scriptEnv) environ() []string {
	var env []string
next:
	for _, kv := range os.Environ() {
		for _, prefix := range droppedPrefixes {
			if strings.HasPrefix(kv, prefix) {
				continue next
			}
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(e.vars))
	for k := range e.vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+e.vars[k])
	}
	return env
}

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes the script with the given arguments in dir and returns how it
// ended. A script that does not end within two minutes fails the test.
func (e *scriptEnv) run(dir string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath, args...) // #nosec G204 -- the script under test; arguments are this test's own
	cmd.Dir = dir
	cmd.Env = e.environ()
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && ctx.Err() == nil:
		res.code = exit.ExitCode()
	default:
		e.t.Fatalf("principal-review.sh %v: %v\nstderr: %s", args, err, res.stderr)
	}
	return res
}

// login stores a credential for the server the way a person would: with
// `conch login`, the token on standard input.
func (e *scriptEnv) login(s *server, token string) {
	e.t.Helper()
	e.vars["CONCH_SERVER"] = s.baseURL
	cmd := exec.Command(conchBin, "login") // #nosec G204 -- a binary this test built
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("conch login: %v\n%s", err, out)
	}
}

// callLog is what the stubs recorded, one line per invocation, in order.
func (e *scriptEnv) callLog() []string {
	e.t.Helper()
	data, err := os.ReadFile(e.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// logs is everything the script has written to its log directory.
func (e *scriptEnv) logs() string {
	e.t.Helper()
	paths, err := filepath.Glob(filepath.Join(e.logDir, "review-*.log"))
	if err != nil {
		e.t.Fatal(err)
	}
	var all strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p) // #nosec G304 -- a log in this test's private directory
		if err != nil {
			e.t.Fatal(err)
		}
		all.Write(data)
	}
	return all.String()
}

// assertNoSecret fails if a credential shows up in anything the script
// printed or logged.
func (e *scriptEnv) assertNoSecret(secret string, results ...result) {
	e.t.Helper()
	for _, r := range results {
		if strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret) {
			e.t.Errorf("the script printed the stored credential")
		}
	}
	if strings.Contains(e.logs(), secret) {
		e.t.Errorf("the script logged the stored credential")
	}
}

// ---------------------------------------------------------------- helpers

// oneLine asserts that s is exactly one line and returns it without its
// newline.
func oneLine(t *testing.T, what, s string) string {
	t.Helper()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("%s must be exactly one line, got %q", what, s)
	}
	return strings.TrimSuffix(s, "\n")
}

// encodedLen is the size of body as a JSON string without its quotes, as
// conch and conchd encode it: the quantity the script budgets.
func encodedLen(t *testing.T, body string) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded) - 2
}

// sectionBody returns the lines under the last "## <heading>" line up to the
// next heading, without the blank lines around them. It is deliberately not
// the script's own parser, and knows nothing of fences: a fixture that
// repeats the heading inside one must put the real section after it.
func sectionBody(t *testing.T, report, heading string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(report, "\n"), "\n")
	start := -1
	for i, line := range lines {
		if line == "## "+heading {
			start = i + 1
		}
	}
	if start < 0 {
		t.Fatalf("no %q section in the report", heading)
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], "# ") {
			end = i
			break
		}
	}
	body := lines[start:end]
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	return body
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a fixture or a file in this test's private directory
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lockedBuffer collects the output of a process that is read while it runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eventually polls cond until it holds or ten seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
