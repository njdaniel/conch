package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ------------------------------------------------------- output and secrets

// harness is the state of one run: where output goes, which strings must
// never appear anywhere, and everything that must be torn down at the end.
type harness struct {
	repo string
	tmp  string // every temp directory lives under this one, removed at the end
	bin  binaries

	mu      sync.Mutex
	out     bytes.Buffer // everything this program printed
	secrets map[string]string
	rooms   map[string]bool // room names: not secret, but never logged or audited
	logs    []namedFile     // files to scan for secrets before the run ends
	cleanup []func()
}

// namedFile is a captured log. rooms is true for files that must not mention
// a room name either: conchd's own log.
type namedFile struct {
	what, path string
	rooms      bool
}

func newHarness() (*harness, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return nil, errors.New("run this from the module root: go run ./e2e/voice")
	}
	tmp, err := os.MkdirTemp("", "voice-check-")
	if err != nil {
		return nil, err
	}
	return &harness{repo: repo, tmp: tmp, secrets: map[string]string{}, rooms: map[string]bool{}}, nil
}

// say prints one line and remembers it, so the run can check at the end that
// it never printed a secret.
func (h *harness) say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	h.mu.Lock()
	h.out.WriteString(line + "\n")
	h.mu.Unlock()
	fmt.Println(line)
}

// secret registers a value that must never be printed, logged or audited.
// label says what it is, for the message when one leaks.
func (h *harness) secret(label, value string) {
	if value == "" {
		return
	}
	h.mu.Lock()
	h.secrets[value] = label
	h.mu.Unlock()
}

// room registers a room name. conchd never writes one to a log line, an audit
// row, an error or a presence document (design note §3), so the run checks.
func (h *harness) room(name string) {
	h.mu.Lock()
	h.rooms[name] = true
	h.mu.Unlock()
}

// hasRoom reports whether text contains a registered room name.
func (h *harness) hasRoom(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for r := range h.rooms {
		if strings.Contains(text, r) {
			return true
		}
	}
	return false
}

// redact replaces every registered secret in s, and anything shaped like a
// JWT, so a failure message can be printed.
func (h *harness) redact(s string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v, label := range h.secrets {
		s = strings.ReplaceAll(s, v, "["+label+" redacted]")
	}
	for r := range h.rooms {
		s = strings.ReplaceAll(s, r, "[room redacted]")
	}
	return jwtRE.ReplaceAllString(s, "[jwt redacted]")
}

// leak reports the first secret found in text, by label only.
func (h *harness) leak(text string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v, label := range h.secrets {
		if strings.Contains(text, v) {
			return label, true
		}
	}
	if jwtRE.MatchString(text) {
		return "a JWT-shaped string", true
	}
	return "", false
}

// scan checks one piece of text for secrets.
func (h *harness) scan(what, text string) error {
	if label, found := h.leak(text); found {
		return fmt.Errorf("secret leak: %s appears in %s", label, what)
	}
	return nil
}

// scanAll checks the program's own output and every captured log.
func (h *harness) scanAll() error {
	h.mu.Lock()
	printed := h.out.String()
	logs := append([]namedFile(nil), h.logs...)
	h.mu.Unlock()
	if err := h.scan("this program's output", printed); err != nil {
		return err
	}
	if h.hasRoom(printed) {
		return errors.New("a room name appears in this program's output")
	}
	for _, l := range logs {
		data, err := os.ReadFile(l.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", l.what, err)
		}
		if err := h.scan(l.what, string(data)); err != nil {
			return err
		}
		if l.rooms && h.hasRoom(string(data)) {
			return fmt.Errorf("a room name appears in %s", l.what)
		}
	}
	return nil
}

func (h *harness) onCleanup(f func()) {
	h.mu.Lock()
	h.cleanup = append(h.cleanup, f)
	h.mu.Unlock()
}

// teardown runs every cleanup in reverse order and removes the temp tree.
func (h *harness) teardown() {
	h.mu.Lock()
	fs := h.cleanup
	h.cleanup = nil
	h.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
	_ = os.RemoveAll(h.tmp)
}

func (h *harness) mkdir(prefix string) (string, error) {
	return os.MkdirTemp(h.tmp, prefix)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ----------------------------------------------------------------- waiting

// waitFor polls fn until it reports true or the deadline passes. fn returns a
// short description of what it saw, which the timeout message quotes. Nothing
// here sleeps as a way of knowing something happened: every wait ends the
// moment the thing it waits for is true.
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
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- binaries

type binaries struct {
	dir    string
	conchd string
	conch  string
}

func (h *harness) build() error {
	dir, err := h.mkdir("bin-")
	if err != nil {
		return err
	}
	h.bin = binaries{dir: dir, conchd: filepath.Join(dir, "conchd"), conch: filepath.Join(dir, "conch")}
	for out, pkg := range map[string]string{h.bin.conchd: "./cmd/conchd", h.bin.conch: "./cmd/conch"} {
		cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- out and pkg are this program's own temp paths and constants
		cmd.Dir = h.repo
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %w\n%s", pkg, err, output)
		}
	}
	return nil
}

// cleanEnv is the caller's environment without anything that would change
// who conch or conchd are, or which LiveKit conchd talks to.
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

// -------------------------------------------------------------------- conchd

type livekitSettings struct {
	url    string // ws://host:port given to clients
	key    string
	secret string
}

type conchdProc struct {
	h        *harness
	cmd      *exec.Cmd
	baseURL  string
	dataDir  string
	logPath  string
	operator api
	done     chan struct{}
	stopped  bool
}

// startConchd bootstraps an operator into a fresh data directory and starts
// conchd on it. Everything lives in temp directories, HOME and XDG included,
// so nothing of the real user's is read or written. A nil lk starts conchd
// with no LiveKit settings at all.
func (h *harness) startConchd(name string, lk *livekitSettings) (*conchdProc, error) {
	root, err := h.mkdir("conchd-" + name + "-")
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(root, "data")
	home := filepath.Join(root, "home")
	for _, d := range []string{dataDir, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	env := append(cleanEnv(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"))

	bootstrap := exec.Command(h.bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "voice-operator") // #nosec G204 -- binary built by this program
	bootstrap.Env = env
	tokenOut, err := bootstrap.Output()
	if err != nil {
		return nil, fmt.Errorf("bootstrap-operator: %w", err)
	}
	opToken := strings.TrimSpace(string(tokenOut))
	if opToken == "" || strings.ContainsAny(opToken, " \n") {
		return nil, fmt.Errorf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(tokenOut))
	}
	h.secret("operator credential", opToken)

	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--data", dataDir, "--listen", addr}
	if lk != nil {
		args = append(args, "--livekit-url", lk.url)
		env = append(env, "CONCHD_LIVEKIT_API_KEY="+lk.key, "CONCHD_LIVEKIT_API_SECRET="+lk.secret)
	}
	logPath := filepath.Join(root, "conchd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(h.bin.conchd, args...) // #nosec G204 -- binary built by this program; args are its own
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	p := &conchdProc{h: h, cmd: cmd, baseURL: "http://" + addr, dataDir: dataDir, logPath: logPath, done: make(chan struct{})}
	p.operator = api{baseURL: p.baseURL, token: opToken}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(p.done) }()
	h.onCleanup(p.stop)
	h.mu.Lock()
	h.logs = append(h.logs, namedFile{"conchd log (" + name + ")", logPath, true})
	h.mu.Unlock()
	if err := waitFor("conchd to answer /healthz", 15*time.Second, func() (bool, string) {
		resp, err := http.Get(p.baseURL + "/healthz") // #nosec G107 -- this harness's own conchd
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode)
	}); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *conchdProc) as(token string) api { return api{baseURL: p.baseURL, token: token} }

func (p *conchdProc) stop() {
	if p.stopped {
		return
	}
	p.stopped = true
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.done
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

// audit returns the whole audit log. It also scans it for secrets.
func (p *conchdProc) audit() ([]store.AuditEvent, error) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(p.dataDir, "conch.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	var all []store.AuditEvent
	var after int64
	for {
		page, err := st.ListAuditEvents(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		after = page[len(page)-1].ID
	}
}

// auditRows returns the audit rows with the given action.
func (p *conchdProc) auditRows(action string) ([]store.AuditEvent, error) {
	all, err := p.audit()
	if err != nil {
		return nil, err
	}
	var rows []store.AuditEvent
	for _, e := range all {
		if e.Action == action {
			rows = append(rows, e)
		}
	}
	return rows, nil
}

// scanAudit checks every audit row for a secret or a room name.
func (p *conchdProc) scanAudit(name string) error {
	all, err := p.audit()
	if err != nil {
		return err
	}
	for _, e := range all {
		text := e.Actor + "\n" + e.Action + "\n" + e.Subject + "\n" + e.Detail
		if err := p.h.scan(fmt.Sprintf("audit log row %d (%s, %s)", e.ID, e.Action, name), text); err != nil {
			return err
		}
		if p.h.hasRoom(text) {
			return fmt.Errorf("a room name appears in audit log row %d (%s)", e.ID, e.Action)
		}
	}
	return nil
}

// ---------------------------------------------------------------------- REST

type api struct{ baseURL, token string }

func (a api) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.baseURL+path, reader) // #nosec G107 -- this harness's own conchd
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, unwrapURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (a api) call(method, path string, body, out any) error {
	status, data, err := a.do(method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, status, truncate(string(data), 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// refusal sends a request and returns the status and error code of the
// answer, for asserting what a caller is told.
func (a api) refusal(method, path string) (int, string, error) {
	status, data, err := a.do(method, path, nil)
	if err != nil {
		return 0, "", err
	}
	var e schema.Error
	_ = json.Unmarshal(data, &e)
	return status, e.Code, nil
}

// person is a human with a credential and their own `conch` configuration.
type person struct {
	name  string
	id    int64
	token string
	api   api
	cli   *conchUser
}

func (h *harness) newPerson(d *conchdProc, name string, channels ...string) (*person, error) {
	var created schema.CreatePrincipalResponse
	if err := d.operator.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}, &created); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	var cred schema.CreateCredentialResponseV1
	if err := d.operator.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), schema.CreateCredentialRequestV1{Label: name}, &cred); err != nil {
		return nil, fmt.Errorf("issue credential for %s: %w", name, err)
	}
	h.secret(name+"'s conch credential", cred.Token)
	p := &person{name: name, id: created.Principal.ID, token: cred.Token, api: d.as(cred.Token)}
	for _, ch := range channels {
		if err := d.operator.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", ch, p.id), nil, nil); err != nil {
			return nil, fmt.Errorf("add %s to %s: %w", name, ch, err)
		}
	}
	cli, err := h.newConchUser(d.baseURL, cred.Token)
	if err != nil {
		return nil, err
	}
	p.cli = cli
	return p, nil
}

func (d *conchdProc) createChannel(name string) (int64, error) {
	var resp schema.CreateChannelResponse
	if err := d.operator.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: name}, &resp); err != nil {
		return 0, fmt.Errorf("create channel %s: %w", name, err)
	}
	return resp.Channel.ID, nil
}

// session asks for a voice session as p and registers every token in it as a
// secret.
func (h *harness) session(p *person, channel string) (schema.VoiceSessionResponseV1, error) {
	var resp schema.VoiceSessionResponseV1
	status, data, err := p.api.do(http.MethodPost, "/v1/channels/"+channel+"/voice/session", nil)
	if err != nil {
		return resp, err
	}
	if status != http.StatusOK {
		var e schema.Error
		_ = json.Unmarshal(data, &e)
		return resp, fmt.Errorf("%s asked for a voice session in %s: status %d, code %q", p.name, channel, status, e.Code)
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return resp, fmt.Errorf("decode session response: %w", err)
	}
	for _, g := range resp.Rooms {
		h.secret("a voice join token", g.Token)
	}
	if err := resp.Validate(); err != nil {
		return resp, fmt.Errorf("session response for %s does not satisfy the schema: %w", p.name, err)
	}
	return resp, nil
}

func (p *person) presence(channel string) (schema.VoicePresenceV1, error) {
	var doc schema.VoicePresenceV1
	err := p.api.call(http.MethodGet, "/v1/channels/"+channel+"/voice", nil, &doc)
	return doc, err
}

// ----------------------------------------------------------------- conch CLI

type conchUser struct {
	bin string
	env []string
}

func (h *harness) newConchUser(serverURL, token string) (*conchUser, error) {
	dir, err := h.mkdir("conch-user-")
	if err != nil {
		return nil, err
	}
	env := append(cleanEnv(), "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "CONCH_SERVER="+serverURL)
	u := &conchUser{bin: h.bin.conch, env: env}
	if _, err := u.run(token+"\n", "login"); err != nil {
		return nil, fmt.Errorf("conch login: %w", err)
	}
	return u, nil
}

// run executes one conch subcommand and returns its combined output.
func (u *conchUser) run(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, u.bin, args...) // #nosec G204 -- binary built by this program; args are its own
	cmd.Env = u.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("conch %s: %w", args[0], err)
	}
	return string(out), nil
}

// ------------------------------------------------------------------- dogfood

// runDogfood runs the unmodified approval dogfood as a subprocess. extraEnv is
// added to the environment it (and so its conchd) inherits; that is how conchd
// ends up configured for a LiveKit that is down, or not configured at all.
func (h *harness) runDogfood(label string, extraEnv ...string) error {
	home, err := h.mkdir("dogfood-home-")
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, "dogfood.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./e2e/dogfood") // #nosec G204 -- constant command
	cmd.Dir = h.repo
	// conch and conchd under dogfood get their own temp config directories;
	// the only environment passed through is what Go itself needs, minus
	// anything that selects an identity or a LiveKit.
	cmd.Env = append(cleanEnvKeepingHome(), extraEnv...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	start := time.Now()
	if err := cmd.Run(); err != nil {
		data, _ := os.ReadFile(logPath) // #nosec G304 -- a path under this program's temp dir
		tail := string(data)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%s: go run ./e2e/dogfood failed: %w\n%s", label, err, h.redact(tail))
	}
	h.mu.Lock()
	h.logs = append(h.logs, namedFile{"dogfood output (" + label + ")", logPath, false})
	h.mu.Unlock()
	// The point of the run is the state its conchd was in. Its start-up line
	// says which, and a dogfood whose conchd never saw the LiveKit settings
	// would pass without showing anything.
	data, err := os.ReadFile(logPath) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		return err
	}
	want := "voice: not configured"
	if len(extraEnv) > 0 {
		want = "voice: configured"
	}
	if !strings.Contains(string(data), want) || !strings.Contains(string(data), "dogfood-check: PASS") {
		return fmt.Errorf("%s: dogfood exited 0, but its output lacks %q or its PASS line", label, want)
	}
	h.say("ok   dogfood passes (%s; its conchd logged %q) in %s", label, want, time.Since(start).Round(time.Second))
	return nil
}

// cleanEnvKeepingHome is cleanEnv for the Go toolchain, which needs HOME to
// find its build cache. dogfood itself never reads the real conch config.
func cleanEnvKeepingHome() []string {
	env := cleanEnv()
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "XDG_CACHE_HOME=") {
			env = append(env, kv)
		}
	}
	return env
}

// ---------------------------------------------------------------------- docker

const (
	// livekitImage is pinned by version and by digest. The digest is that of
	// the multi-architecture index for v1.13.7.
	livekitImage = "livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3"

	lkVersion = "2.19.0"
)

// lkSHA256 are the SHA-256 sums of the lk release archives, from the
// checksums.txt published with livekit-cli v2.19.0.
var lkSHA256 = map[string]string{
	"linux_amd64":  "601ce74299db7e538014426644ab23b2a1ba171c506967197a259c3cd762d418",
	"linux_arm64":  "64e8f68f26eb1de81f0696a9a0fca7564c4e7e551de6cc98efc327ca02b56b52",
	"darwin_amd64": "9f7f23ebc5b146e9b3bcba63254e1792f40598247a468a415ef051e97b31ff57",
	"darwin_arm64": "f5e2413303d91814b0dfe9a0b6a284d8f095b3cb2558ad702970f653cfe1dfa9",
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- constant command; args are this program's own
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// livekitServer is the container this run started, named by this run.
type livekitServer struct {
	h      *harness
	name   string
	apiURL string // http://127.0.0.1:port
	wsURL  string // ws://127.0.0.1:port
	key    string
	secret string
}

// startLiveKit runs the pinned image on the host network, bound to localhost,
// with automatic room creation off. enable_remote_unmute lets the harness
// stand in for a key press from the server side (LiveKit refuses a
// server-side unmute otherwise); conchd needs no such setting. The container is registered for removal
// before it is started, so a failure at any point still removes it.
func (h *harness) startLiveKit(ctx context.Context) (*livekitServer, error) {
	httpAddr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	tcpAddr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	httpPort := httpAddr[strings.LastIndex(httpAddr, ":")+1:]
	tcpPort := tcpAddr[strings.LastIndex(tcpAddr, ":")+1:]
	// 100 UDP ports for media, at a random place in the high range.
	base := 40000 + int(randByte())*100
	srv := &livekitServer{
		h:      h,
		name:   "conch-voice-check-" + randHex(4),
		apiURL: "http://127.0.0.1:" + httpPort,
		wsURL:  "ws://127.0.0.1:" + httpPort,
		key:    "voicecheckkey",
		secret: "voice-check-NOT-FOR-PRODUCTION-" + randHex(24),
	}
	h.secret("the LiveKit API secret", srv.secret)
	cfg := fmt.Sprintf(`port: %s
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: %s
  port_range_start: %d
  port_range_end: %d
  use_external_ip: false
keys:
  %s: %s
room:
  auto_create: false
  empty_timeout: 5
  departure_timeout: 5
  enable_remote_unmute: true
`, httpPort, tcpPort, base, base+99, srv.key, srv.secret)
	h.onCleanup(func() { srv.remove() })
	out, err := docker(ctx, "run", "-d", "--name", srv.name, "--label", "conch-voice-check=1", "--network", "host",
		livekitImage, "--config-body", cfg, "--node-ip", "127.0.0.1")
	if err != nil {
		return nil, fmt.Errorf("docker run: %s", h.redact(out))
	}
	if err := waitFor("LiveKit to answer on "+srv.apiURL, 60*time.Second, func() (bool, string) {
		resp, err := http.Get(srv.apiURL) // #nosec G107 -- the container this run started
		if err != nil {
			return false, unwrapURLError(err).Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode)
	}); err != nil {
		logs, _ := docker(ctx, "logs", "--tail", "20", srv.name)
		return nil, fmt.Errorf("%w\ncontainer log:\n%s", err, h.redact(logs))
	}
	return srv, nil
}

func randByte() byte {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return b[0] % 150 // keeps the range below 55000
}

func (s *livekitServer) settings() *livekitSettings {
	return &livekitSettings{url: s.wsURL, key: s.key, secret: s.secret}
}

func (s *livekitServer) admin() *lkAdmin { return newLKAdmin(s.apiURL, s.key, s.secret) }

// stop stops the container, so LiveKit is down but its address is known.
func (s *livekitServer) stop(ctx context.Context) error {
	if out, err := docker(ctx, "stop", "-t", "1", s.name); err != nil {
		return fmt.Errorf("docker stop: %s", out)
	}
	return nil
}

// remove removes the container, running or not. It is safe to call twice.
func (s *livekitServer) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = docker(ctx, "rm", "-f", s.name)
}

// ---------------------------------------------------------------- lk download

// fetchLK downloads the pinned lk release, checks its SHA-256 against the
// sum written in this file, and extracts the lk binary into dir.
func fetchLK(ctx context.Context, dir string) (string, error) {
	key := runtime.GOOS + "_" + runtime.GOARCH
	want, ok := lkSHA256[key]
	if !ok {
		return "", fmt.Errorf("no lk %s release is pinned for %s", lkVersion, key)
	}
	url := fmt.Sprintf("https://github.com/livekit/livekit-cli/releases/download/v%s/lk_%s_%s.tar.gz", lkVersion, lkVersion, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download lk: %w", unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download lk: HTTP %d", resp.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("download lk: %w", err)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("lk %s archive has SHA-256 %s, want %s: refusing to run it", lkVersion, got, want)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("lk archive has no lk binary")
		}
		if err != nil {
			return "", err
		}
		if hdr.Name != "lk" && hdr.Name != "./lk" {
			continue
		}
		path := filepath.Join(dir, "lk")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700) // #nosec G302 G304 -- a path under this program's temp dir; the downloaded lk must be executable
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, 300<<20)); err != nil { // #nosec G110 -- bounded
			_ = f.Close()
			return "", err
		}
		return path, f.Close()
	}
}

// ------------------------------------------------------------ headless client

// headless is `lk room join` publishing silence, joined through a relay that
// substitutes the token conchd issued for the one lk signed itself.
type headless struct {
	relay   *relay
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
}

// startHeadless starts lk as a participant admitted by token alone.
func (h *harness) startHeadless(lkBin, oggPath, lkHostPort, token string) (*headless, error) {
	rl, err := startRelay(lkHostPort, token)
	if err != nil {
		return nil, err
	}
	dir, err := h.mkdir("lk-")
	if err != nil {
		rl.Close()
		return nil, err
	}
	logPath := filepath.Join(dir, "lk.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		rl.Close()
		return nil, err
	}
	// A throwaway key pair LiveKit has never heard of: lk signs its own token
	// with it, and the relay replaces that token before LiveKit sees it.
	cmd := exec.Command(lkBin, "--url", rl.url(), "--api-key", "relay-only-key", "--api-secret", "relay-only-secret-not-known-to-livekit-0000000000", // #nosec G204 -- the pinned lk binary this program downloaded
		"room", "join", "--identity", "relay-placeholder", "--publish", oggPath, "relay-placeholder-room")
	cmd.Env = append(cleanEnv(), "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		rl.Close()
		return nil, err
	}
	hl := &headless{relay: rl, cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(hl.done) }()
	h.onCleanup(hl.stop)
	h.mu.Lock()
	h.logs = append(h.logs, namedFile{"lk output", logPath, false})
	h.mu.Unlock()
	return hl, nil
}

func (hl *headless) stop() {
	if hl.cmd.Process != nil {
		_ = hl.cmd.Process.Kill()
	}
	<-hl.done
	hl.relay.Close()
}

func (hl *headless) exited() bool {
	select {
	case <-hl.done:
		return true
	default:
		return false
	}
}

func (hl *headless) tail(h *harness) string {
	data, _ := os.ReadFile(hl.logPath) // #nosec G304 -- a path under this program's temp dir
	s := string(data)
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return h.redact(s)
}
