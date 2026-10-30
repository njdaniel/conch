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
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
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
	headless []*lkit.Headless
	cleanup  []func()
	kit      *lkit.Kit
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
	h := &harness{repo: repo, tmp: tmp, secrets: map[string]string{}, rooms: map[string][]string{}, jtis: map[string]bool{}}
	h.kit = lkit.New(kitHost{h}, stopCtx, "voice-check")
	return h, nil
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
	return lkit.JWT.ReplaceAllString(s, "[jwt redacted]")
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
	if lkit.JWT.MatchString(text) {
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
	participants := append([]*lkit.Headless(nil), h.headless...)
	h.mu.Unlock()
	for _, hl := range participants {
		for _, t := range hl.Relay.IssuedTokens() {
			h.secret("a LiveKit-issued token", t)
		}
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
		cmd := lkit.GroupCommand(stopCtx, "go", "build", "-o", out, pkg)
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

	bootstrap := lkit.GroupCommand(stopCtx, h.bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "voice-operator")
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
	cmd := lkit.GroupStart(h.bin.conchd, args...)
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
	_ = lkit.KillGroup(p.cmd)
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
		return 0, nil, lkit.UnwrapURLError(err)
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
		return fmt.Errorf("%s %s: status %d: %s", method, path, status, lkit.Truncate(string(data), 300))
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
	if want := identity(p.id) + ".screen"; resp.CompanionIdentity != want {
		return resp, fmt.Errorf("session for %s names companion identity %q, want %q", p.name, resp.CompanionIdentity, want)
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
	cmd := lkit.GroupCommand(ctx, u.bin, args...)
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
	cmd := lkit.GroupCommand(ctx, "go", "run", "./e2e/dogfood")
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

// ------------------------------------------------------------------- LiveKit

// kitHost is the harness as the LiveKit kit sees it (e2e/internal/lkit).
type kitHost struct{ h *harness }

func (k kitHost) Mkdir(prefix string) (string, error) { return k.h.mkdir(prefix) }
func (k kitHost) OnCleanup(f func())                  { k.h.onCleanup(f) }
func (k kitHost) Secret(label, value string)          { k.h.secret(label, value) }
func (k kitHost) Say(format string, args ...any)      { k.h.say(format, args...) }
func (k kitHost) Redact(s string) string              { return k.h.redact(s) }

// Log registers lk's output, which is scanned for secrets but may name a
// room: lk prints the room it joined.
func (k kitHost) Log(what, path string) {
	k.h.mu.Lock()
	k.h.logs = append(k.h.logs, namedFile{what, path, false})
	k.h.mu.Unlock()
}

// settingsOf is what conchd is told of a LiveKit server.
func settingsOf(s *lkit.Server) *livekitSettings {
	return &livekitSettings{url: s.WSURL, key: s.Key, secret: s.Secret}
}

// startHeadless starts lk as a participant admitted by token alone,
// publishing silence as its microphone.
func (h *harness) startHeadless(lkBin, oggPath, lkHostPort, token string) (*lkit.Headless, error) {
	return h.startLK(lkBin, lkHostPort, token, nil, "--publish", oggPath)
}

// startLK starts `lk room join` with the given options (lkit.Kit.StartLK) and
// remembers it, so that the tokens LiveKit sent it are registered as secrets
// before the final scan.
func (h *harness) startLK(lkBin, lkHostPort, token string, filter func(context.Context, []byte) []byte, options ...string) (*lkit.Headless, error) {
	hl, err := h.kit.StartLK(lkBin, lkHostPort, token, filter, options...)
	if err != nil {
		return nil, err
	}
	h.remember(hl)
	return hl, nil
}

// startLKVia is startLK through a relay already started (lkit.Kit.StartLKVia),
// which it owns from then on: stopping the participant closes it.
func (h *harness) startLKVia(lkBin string, rl *lkit.Relay, options ...string) (*lkit.Headless, error) {
	hl, err := h.kit.StartLKVia(lkBin, rl, options...)
	if err != nil {
		return nil, err
	}
	h.remember(hl)
	return hl, nil
}

// remember keeps a participant, whose LiveKit-issued tokens become secrets
// before the final scan.
func (h *harness) remember(hl *lkit.Headless) {
	h.mu.Lock()
	h.headless = append(h.headless, hl)
	h.mu.Unlock()
}
