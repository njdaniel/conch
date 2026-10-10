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
	rooms   map[string][]string // room names (and what to search for): not secret, but never logged or audited
	jtis    map[string]bool     // jti of every join token seen
	conchds []*conchdProc
	logs    []namedFile // files to scan for secrets before the run ends
	// headless are the lk participants started, whose LiveKit-issued tokens
	// are registered as secrets before the final scan.
	headless []*headless
	cleanup  []func()
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
	return &harness{repo: repo, tmp: tmp, secrets: map[string]string{}, rooms: map[string][]string{}, jtis: map[string]bool{}}, nil
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
	needles := roomNeedles(name)
	h.mu.Lock()
	h.rooms[name] = needles
	h.mu.Unlock()
}

// hasRoom reports whether text contains a registered room name.
func (h *harness) hasRoom(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, needles := range h.rooms {
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
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
	daemons := append([]*conchdProc(nil), h.conchds...)
	h.mu.Unlock()
	for _, d := range daemons {
		if err := d.registerStoreRooms(); err != nil {
			return fmt.Errorf("read the rooms conchd stored: %w", err)
		}
	}
	h.mu.Lock()
	participants := append([]*headless(nil), h.headless...)
	h.mu.Unlock()
	for _, hl := range participants {
		hl.registerTokens(h)
	}
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
		func() {
			// One cleanup that panics must not stop the ones after it: a
			// container is removed by a cleanup that may be last in line.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintln(os.Stderr, "voice-check: a clean-up step panicked:", h.redact(fmt.Sprint(r)))
				}
			}()
			fs[i]()
		}()
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
// moment the thing it waits for is true, or a signal arrives.
func waitFor(what string, within time.Duration, fn func() (bool, string)) error {
	return waitForOrFail(what, within, func() (bool, string, error) {
		ok, saw := fn()
		return ok, saw, nil
	})
}

// waitForOrFail is waitFor for a check that can also know the wait is
// hopeless (the process that was to do the thing has exited): a non-nil error
// from fn ends the wait at once.
func waitForOrFail(what string, within time.Duration, fn func() (bool, string, error)) error {
	deadline := time.Now().Add(within)
	last := "nothing observed"
	for {
		ok, saw, err := fn()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
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
		case <-time.After(100 * time.Millisecond):
		}
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
		cmd := groupCommand(stopCtx, "go", "build", "-o", out, pkg)
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
	lk       *livekitSettings // nil when voice is not configured
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

	bootstrap := groupCommand(stopCtx, h.bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "voice-operator")
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
	cmd := groupStart(h.bin.conchd, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	p := &conchdProc{h: h, cmd: cmd, baseURL: "http://" + addr, dataDir: dataDir, logPath: logPath, done: make(chan struct{})}
	p.operator = api{h: h, baseURL: p.baseURL, token: opToken}
	p.lk = lk
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(p.done) }()
	h.onCleanup(p.stop)
	h.mu.Lock()
	h.conchds = append(h.conchds, p)
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

func (p *conchdProc) as(token string) api { return api{h: p.h, baseURL: p.baseURL, token: token} }

func (p *conchdProc) stop() {
	if p.stopped {
		return
	}
	p.stopped = true
	_ = killGroup(p.cmd)
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
	if err := p.registerStoreRooms(); err != nil {
		return fmt.Errorf("read the rooms conchd stored: %w", err)
	}
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

// api is a REST client acting as one principal. Every answer it receives is
// scanned for a token, the API secret and a room name (see scanResponse), so
// that what conchd sends to clients is checked, not only what it logs.
type api struct {
	h              *harness
	baseURL, token string
}

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
	if err != nil {
		return resp.StatusCode, data, err
	}
	if a.h != nil {
		what := method + " " + strings.SplitN(path, "?", 2)[0]
		if err := a.h.scanResponse(what, resp.Header, data, bodyCarriesCredentials(method, path, resp.StatusCode)); err != nil {
			return resp.StatusCode, data, err
		}
	}
	return resp.StatusCode, data, nil
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
	name   string
	id     int64
	token  string
	credID int64 // the credential token belongs to
	d      *conchdProc
	api    api
	cli    *conchUser
}

// personOpts varies how a principal is made.
type personOpts struct {
	kind    schema.PrincipalKind // default human
	expires *time.Time           // the credential stops being live at this time
	noCLI   bool                 // do not set up a conch configuration for them
}

func (h *harness) newPerson(d *conchdProc, name string, channels ...string) (*person, error) {
	return h.newPersonWith(d, name, personOpts{}, channels...)
}

func (h *harness) newPersonWith(d *conchdProc, name string, o personOpts, channels ...string) (*person, error) {
	kind := o.kind
	if kind == "" {
		kind = schema.PrincipalHuman
	}
	var created schema.CreatePrincipalResponse
	if err := d.operator.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &created); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	req := schema.CreateCredentialRequestV1{Label: name}
	if o.expires != nil {
		ts := schema.NewTimestamp(*o.expires)
		req.ExpiresAt = &ts
	}
	var cred schema.CreateCredentialResponseV1
	if err := d.operator.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), req, &cred); err != nil {
		return nil, fmt.Errorf("issue credential for %s: %w", name, err)
	}
	h.secret(name+"'s conch credential", cred.Token)
	p := &person{name: name, id: created.Principal.ID, token: cred.Token, credID: cred.Credential.ID, d: d, api: d.as(cred.Token)}
	for _, ch := range channels {
		if err := d.operator.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", ch, p.id), nil, nil); err != nil {
			return nil, fmt.Errorf("add %s to %s: %w", name, ch, err)
		}
	}
	if o.noCLI || kind != schema.PrincipalHuman {
		return p, nil
	}
	cli, err := h.newConchUser(d.baseURL, cred.Token)
	if err != nil {
		return nil, err
	}
	p.cli = cli
	return p, nil
}

// newAgent makes an agent that is a member of channel and holds a manifest
// granting every capability there, so that anything it is refused is refused
// for being an agent and not for lacking a grant.
func (h *harness) newAgent(d *conchdProc, name, channel string, channelID int64) (*person, error) {
	a, err := h.newPersonWith(d, name, personOpts{kind: schema.PrincipalAgent}, channel)
	if err != nil {
		return nil, err
	}
	req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierA, Capabilities: schema.Capabilities()}
	req.Channels = append(req.Channels, schema.ChannelGrant{
		ChannelID:   channelID,
		Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost},
	})
	if err := d.operator.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", a.id), req, nil); err != nil {
		return nil, fmt.Errorf("write %s's manifest: %w", name, err)
	}
	return a, nil
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
	if err := decodeStrict(data, &resp); err != nil {
		return resp, fmt.Errorf("decode session response: %w", err)
	}
	for _, g := range resp.Rooms {
		h.secret("a voice join token", g.Token)
		h.room(g.Room)
	}
	if err := resp.Validate(); err != nil {
		return resp, fmt.Errorf("session response for %s does not satisfy the schema: %w", p.name, err)
	}
	if resp.Identity != identity(p.id) {
		return resp, fmt.Errorf("session for %s names identity %q, want %q", p.name, resp.Identity, identity(p.id))
	}
	if p.d.lk != nil {
		for _, g := range resp.Rooms {
			if err := h.checkJoinToken(g.Token, p.d.lk, identity(p.id), g.Room, g.CanPublish, g.ExpiresAt.Time()); err != nil {
				return resp, fmt.Errorf("%s's join token for %s: %w", p.name, channel, err)
			}
		}
	}
	return resp, nil
}

// decodeStrict decodes JSON and fails on a field the type does not have, so a
// field added to a document conchd sends is a failure here, not a silent
// extra.
func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func (p *person) presence(channel string) (schema.VoicePresenceV1, error) {
	var doc schema.VoicePresenceV1
	status, data, err := p.api.do(http.MethodGet, "/v1/channels/"+channel+"/voice", nil)
	if err != nil {
		return doc, err
	}
	if status != http.StatusOK {
		return doc, fmt.Errorf("presence for %s: status %d", channel, status)
	}
	if err := decodeStrict(data, &doc); err != nil {
		return doc, fmt.Errorf("presence document does not decode strictly: %w", err)
	}
	return doc, nil
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
	ctx, cancel := context.WithTimeout(stopCtx, 30*time.Second)
	defer cancel()
	cmd := groupCommand(ctx, u.bin, args...)
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
	ctx, cancel := context.WithTimeout(stopCtx, 8*time.Minute)
	defer cancel()
	// dogfood makes temp directories of its own; pointing TMPDIR into this
	// run's tree means they go with it however the run ends.
	dtmp, err := h.mkdir("dogfood-tmp-")
	if err != nil {
		return err
	}
	cmd := groupCommand(ctx, "go", "run", "./e2e/dogfood")
	cmd.Dir = h.repo
	// conch and conchd under dogfood get their own temp config directories;
	// the only environment passed through is what Go itself needs, minus
	// anything that selects an identity or a LiveKit.
	cmd.Env = append(append(cleanEnvKeepingHome(), "TMPDIR="+dtmp), extraEnv...)
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
	id     string // container id, once docker run has printed it
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
	// The settings go in a file and the key pair in the environment of the
	// docker command, passed on by name: neither is on a command line, where
	// any local user could read the secret while the container runs.
	cfg := fmt.Sprintf(`port: %s
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: %s
  port_range_start: %d
  port_range_end: %d
  use_external_ip: false
room:
  auto_create: false
  empty_timeout: 5
  departure_timeout: 5
  enable_remote_unmute: true
`, httpPort, tcpPort, base, base+99)
	dir, err := h.mkdir("livekit-")
	if err != nil {
		return nil, err
	}
	// The directory and file are made readable beyond this user because the
	// process in the container may not run as it; they hold no secret.
	cfgPath := filepath.Join(dir, "livekit.yaml")
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- holds only the settings file below, no secret
		return nil, err
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil { // #nosec G306 -- settings only; the key pair is passed in the environment
		return nil, err
	}
	h.onCleanup(func() { srv.remove() })
	cmd := groupCommand(stopCtx, "docker", "run", "-d", "--name", srv.name, "--label", containerLabel(), "--network", "host",
		"-e", "LIVEKIT_KEYS", "-v", cfgPath+":/etc/livekit.yaml:ro",
		livekitImage, "--config", "/etc/livekit.yaml", "--node-ip", "127.0.0.1")
	cmd.Env = append(os.Environ(), "LIVEKIT_KEYS="+srv.key+": "+srv.secret)
	raw, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(raw))
	// docker prints the new container's id; remove() uses it. A signal that
	// arrives while docker run is working can leave a container behind with
	// no id for us: remove() then finds it by the name and label this run gave it.
	if id := lastField(out); len(id) == 64 {
		srv.id = id
	}
	if stopCtx.Err() != nil {
		return nil, errInterrupted
	}
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

// containerLabel is the label every container of this run carries. In CI the
// job sets VOICE_CHECK_RUN to something unique to the run, and its clean-up
// step removes by that label, so that it can never remove the container of
// another job on the same host.
func containerLabel() string {
	run := os.Getenv("VOICE_CHECK_RUN")
	if run == "" {
		run = "local"
	}
	return "conch-voice-check=" + run
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

// remove removes the container, running or not, by its id. It is safe to call
// twice. When docker run never reported an id (it was interrupted), the id is
// looked up by the name and label this run gave the container.
func (s *livekitServer) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := s.id
	if id == "" {
		out, _ := docker(ctx, "ps", "-aq", "--no-trunc", "--filter", "name=^/"+s.name+"$", "--filter", "label="+containerLabel())
		id = lastField(out)
	}
	if id != "" {
		_, _ = docker(ctx, "rm", "-f", id)
	}
}

// lastField is the last whitespace-separated word of s.
func lastField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// ---------------------------------------------------------------- lk download

// errUnreachable marks a download that failed for want of a network: the one
// failure of fetchLK that is a reason to skip.
var errUnreachable = errors.New("the download could not be completed")

// checksumError is a downloaded file that is not the one pinned in this
// file. That is evidence of tampering or of a wrong pin, never of a missing
// network, so the run fails on it everywhere and does not skip.
type checksumError struct{ msg string }

func (e *checksumError) Error() string { return e.msg }

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
		return "", fmt.Errorf("download lk: %w: %w", errUnreachable, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("download lk: %w: HTTP %d", errUnreachable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download lk: the pinned release URL answered HTTP %d", resp.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("download lk: %w: %w", errUnreachable, err)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", &checksumError{fmt.Sprintf("lk %s archive has SHA-256 %s, want %s: refusing to run it", lkVersion, got, want)}
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
	cmd := groupStart(lkBin, "--url", rl.url(), "--api-key", "relay-only-key", "--api-secret", "relay-only-secret-not-known-to-livekit-0000000000",
		"room", "join", "--identity", "relay-placeholder", "--publish", oggPath, "relay-placeholder-room")
	// lk reads livekit.toml from its working directory and LIVEKIT_* from its
	// environment. It gets an empty directory of its own and PATH, HOME and
	// TMPDIR, and nothing else of the caller's.
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir}
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
	h.headless = append(h.headless, hl)
	h.mu.Unlock()
	return hl, nil
}

func (hl *headless) stop() {
	_ = killGroup(hl.cmd)
	<-hl.done
	hl.relay.Close()
}

// registerTokens makes the tokens LiveKit sent this participant known to the
// leak scan.
func (hl *headless) registerTokens(h *harness) {
	for _, t := range hl.relay.issuedTokens() {
		h.secret("a LiveKit-issued token", t)
	}
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
