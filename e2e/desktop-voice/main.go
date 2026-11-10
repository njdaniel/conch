// Command desktop-voice is the live same-machine voice test of the desktop
// shell (issue #1383, D3-12 of #771; docs/design/linux-desktop.md): the
// desktop's VoiceService and Supervisor (desktop/main/voice) drive the real
// conch-voice into a real LiveKit, while a second, separate principal listens
// through a counting sink.
//
// This program is the world around the owner. It starts the pinned LiveKit
// image in Docker (e2e/internal/lkit, as e2e/voice does), a real conchd, and
// makes the principals: A, whom the desktop owns, C, a second owner, and B, a
// listener. B runs `conch-voice join` with `--sink count` and this program
// keeps what B counts. It then runs the owner side, desktop/tests/e2e/
// voice-live.test.ts under Vitest, and gives that its facts in its
// environment and, for what only this program can see or do, a small HTTP
// observer on loopback (guarded by a bearer secret):
//
//	GET    /heard?speaker=p<id>&since=<unix ms>  what B's stats lines counted of that speaker
//	GET    /participants                         LiveKit's own list of the room
//	POST   /second-session                       a standalone `conch-voice join` as A
//	GET    /second-session                       whether it is still running
//	DELETE /second-session                       stops it
//
// A fourth principal, V, is a browser (issue #1583, D4-06): conchd serves the
// web client's build (make web-build) at /app/, and the owner side's second
// file, companion-live.test.ts, drives V in a Chromium beside the desktop's
// own screen companion (desktop/main/voice/companion.ts) running A's share
// and watch. V's credential and the app's address are in its environment.
//
// Everything is synthetic: microphones are `--mic tone:<hz>`, the key is a
// named pipe of hand-built input_event records (`--test-key-device`), the
// speakers `--sink count`. No microphone, PipeWire, keyboard or compositor.
// The screens are Chromium's fake screen (launch flags the test gives the
// browsers it starts). This proves the owner's lifecycle, the ceiling, the
// gate and one microphone publisher on one machine, and the companion's share
// and watch between a desktop and a browser; it proves nothing about a real keyboard, real
// audio or Electron's own process tree (D3-13, D3-14).
//
// With no Docker, no network for the image or lk, or no pinned Rust
// toolchain, it prints SKIPPED with the reason and exits 0, unless CI is set:
// then any skip is a failure. CONCH_VOICE_BIN names a conch-voice already
// built (CI uses the one the rust job's cargo test built). No token or secret
// appears in anything printed: everything the owner side writes goes through
// the redaction list.
package main

import (
	"bufio"
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	channel = "squad"
	toneA   = 440 // principal A's microphone
	toneC   = 880 // principal C's microphone
)

var stopCtx, stopCancel = context.WithCancel(context.Background())

func main() { os.Exit(realMain()) }

func inCI() bool {
	v := strings.ToLower(os.Getenv("CI"))
	return v != "" && v != "false" && v != "0"
}

func realMain() int {
	h, err := newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "desktop-voice: FAIL:", err)
		return 1
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "desktop-voice: interrupted; cleaning up")
		stopCancel()
	}()
	start := time.Now()
	skipped, err := guarded(h)
	h.teardown()
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "desktop-voice: interrupted")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "desktop-voice: FAIL:", h.redact(err.Error()))
		return 1
	case skipped != "":
		if inCI() {
			fmt.Fprintln(os.Stderr, "desktop-voice: FAIL: cannot run in CI:", skipped)
			return 1
		}
		fmt.Printf("desktop-voice: SKIPPED %s\n", skipped)
		return 0
	}
	fmt.Printf("desktop-voice: PASS in %s\n", time.Since(start).Round(time.Second))
	return 0
}

func guarded(h *harness) (skipped string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	skipped, err = run(stopCtx, h)
	if err == nil && skipped == "" {
		err = h.scanAll()
	}
	return skipped, err
}

// ------------------------------------------------------------------ harness

type harness struct {
	repo, tmp string
	kit       *lkit.Kit

	mu      sync.Mutex
	out     bytes.Buffer
	secrets map[string]string
	rooms   map[string]bool
	logs    []string // files scanned for secrets at the end
	clean   []func()
}

func newHarness() (*harness, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return nil, errors.New("run this from the module root: go run ./e2e/desktop-voice")
	}
	tmp, err := os.MkdirTemp("", "desktop-voice-")
	if err != nil {
		return nil, err
	}
	h := &harness{repo: repo, tmp: tmp, secrets: map[string]string{}, rooms: map[string]bool{}}
	h.kit = lkit.New(kitHost{h}, stopCtx, "desktop-voice-check")
	return h, nil
}

func (h *harness) say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	h.mu.Lock()
	h.out.WriteString(line + "\n")
	h.mu.Unlock()
	fmt.Println(line)
}

func (h *harness) secret(label, value string) {
	if value == "" {
		return
	}
	h.mu.Lock()
	h.secrets[value] = label
	h.mu.Unlock()
}

func (h *harness) room(name string) {
	h.mu.Lock()
	h.rooms[name] = true
	h.mu.Unlock()
}

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

func (h *harness) leak(text string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v, label := range h.secrets {
		if strings.Contains(text, v) {
			return label, true
		}
	}
	for r := range h.rooms {
		if strings.Contains(text, r) {
			return "a room name", true
		}
	}
	return "", false
}

// scanAll checks what this program printed and every log it kept.
func (h *harness) scanAll() error {
	h.mu.Lock()
	printed := h.out.String()
	logs := append([]string(nil), h.logs...)
	h.mu.Unlock()
	if label, found := h.leak(printed); found {
		return fmt.Errorf("secret leak: %s appears in this program's output", label)
	}
	for _, path := range logs {
		data, err := os.ReadFile(path) // #nosec G304 -- a path under this program's temp dir
		if err != nil {
			return err
		}
		if label, found := h.leak(string(data)); found {
			return fmt.Errorf("secret leak: %s appears in %s", label, filepath.Base(path))
		}
	}
	return nil
}

func (h *harness) onCleanup(f func()) {
	h.mu.Lock()
	h.clean = append(h.clean, f)
	h.mu.Unlock()
}

func (h *harness) teardown() {
	h.mu.Lock()
	fs := h.clean
	h.clean = nil
	h.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintln(os.Stderr, "desktop-voice: a clean-up step panicked:", h.redact(fmt.Sprint(r)))
				}
			}()
			fs[i]()
		}()
	}
	_ = os.RemoveAll(h.tmp)
}

func (h *harness) mkdir(prefix string) (string, error) { return os.MkdirTemp(h.tmp, prefix) }

func (h *harness) keepLog(path string) {
	h.mu.Lock()
	h.logs = append(h.logs, path)
	h.mu.Unlock()
}

type kitHost struct{ h *harness }

func (k kitHost) Mkdir(prefix string) (string, error) { return k.h.mkdir(prefix) }
func (k kitHost) OnCleanup(f func())                  { k.h.onCleanup(f) }
func (k kitHost) Secret(label, value string)          { k.h.secret(label, value) }
func (k kitHost) Say(format string, args ...any)      { k.h.say(format, args...) }
func (k kitHost) Redact(s string) string              { return k.h.redact(s) }
func (k kitHost) Log(_, path string)                  { k.h.keepLog(path) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

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

func waitFor(what string, within time.Duration, fn func() (bool, string)) error {
	deadline := time.Now().Add(within)
	last := "nothing observed"
	for {
		if ok, saw := fn(); ok {
			return nil
		} else if saw != "" {
			last = saw
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s (last saw: %s)", within, what, last)
		}
		select {
		case <-stopCtx.Done():
			return errors.New("interrupted by a signal")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------- run

func run(ctx context.Context, h *harness) (string, error) {
	lkBin, reason, err := h.kit.Prepare(ctx)
	if err != nil || reason != "" {
		return reason, err
	}
	_ = lkBin // fetched and checksummed as e2e/voice does; this program's participants are conch-voice, not lk
	voiceBin, skip, err := h.voiceBinary()
	if err != nil || skip != "" {
		return skip, err
	}
	if err := h.checkDesktopDeps(); err != nil {
		return "", err
	}
	webDir, err := h.webDist()
	if err != nil {
		return "", err
	}
	conchd, err := h.buildConchd()
	if err != nil {
		return "", err
	}
	srv, err := h.kit.StartLiveKit(ctx)
	if err != nil {
		return "", err
	}
	h.say("ok   LiveKit is up on %s (container %s, auto_create off)", srv.APIURL, srv.Name)
	h.secret("LiveKit API key", srv.Key)
	h.secret("LiveKit API secret", srv.Secret)

	d, err := h.startConchd(conchd, srv, webDir)
	if err != nil {
		return "", err
	}
	a, err := d.newPrincipal("desk-a")
	if err != nil {
		return "", err
	}
	b, err := d.newPrincipal("ears-b")
	if err != nil {
		return "", err
	}
	c, err := d.newPrincipal("desk-c")
	if err != nil {
		return "", err
	}
	v, err := d.newPrincipal("view-v")
	if err != nil {
		return "", err
	}
	h.say("ok   conchd is up on %s with principals A (p%d), B (p%d, the listener), C (p%d) and V (p%d, a browser)", d.baseURL, a.id, b.id, c.id, v.id)

	ears, err := h.startListener(voiceBin, d, b)
	if err != nil {
		return "", err
	}
	obs, err := h.startObserver(srv.Admin(), ears, voiceBin, d, a)
	if err != nil {
		return "", err
	}
	h.say("ok   B is listening in %s (sink counting %d Hz and %d Hz)", channel, toneA, toneC)

	env := append(cleanEnvKeepingPath(),
		"CI="+os.Getenv("CI"),
		"CONCH_VOICE_BIN="+voiceBin,
		"CONCH_E2E_ORIGIN="+d.baseURL,
		"CONCH_E2E_OBSERVER="+obs.url,
		"CONCH_E2E_OBSERVER_TOKEN="+obs.token,
		"CONCH_E2E_A_ID="+strconv.FormatInt(a.id, 10),
		"CONCH_E2E_A_TOKEN="+a.token,
		"CONCH_E2E_C_ID="+strconv.FormatInt(c.id, 10),
		"CONCH_E2E_C_TOKEN="+c.token,
		"CONCH_E2E_V_ID="+strconv.FormatInt(v.id, 10),
		"CONCH_E2E_V_TOKEN="+v.token,
		"CONCH_E2E_APP_URL="+d.baseURL+"/app/",
		"CONCH_E2E_TONE_A="+strconv.Itoa(toneA),
		"CONCH_E2E_TONE_C="+strconv.Itoa(toneC),
		"CONCH_E2E_CHANNEL="+channel,
	)
	h.say("== the owner side: desktop/tests/e2e (voice-live.test.ts, companion-live.test.ts) ==")
	if err := h.runOwnerSide(ctx, env); err != nil {
		return "", err
	}
	if ears.exited() {
		return "", fmt.Errorf("the listener B ended during the run: %s", ears.tail())
	}
	h.say("ok   no token, secret or room name in this output, the logs, or what the owner side printed")
	return "", nil
}

func cleanEnvKeepingPath() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CONCHD_") || strings.HasPrefix(kv, "LIVEKIT_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// checkDesktopDeps fails (it does not skip) when desktop/'s dependencies are
// not installed: make desktop-voice-e2e runs npm ci first.
func (h *harness) checkDesktopDeps() error {
	if _, err := os.Stat(filepath.Join(h.repo, "desktop", "node_modules", ".bin", "vitest")); err != nil {
		return errors.New("desktop/node_modules is not installed; run make desktop-voice-e2e (it runs npm ci first)")
	}
	return nil
}

// webDist is the web client's build, which conchd serves at /app/ so that V, a
// browser, can sign in and share: make desktop-voice-e2e builds it first
// (make web-build). Missing, it fails; it does not skip.
func (h *harness) webDist() (string, error) {
	dir := filepath.Join(h.repo, "web", "dist")
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return "", errors.New("web/dist is not built; run make desktop-voice-e2e (it builds the web client first)")
	}
	return dir, nil
}

// runOwnerSide runs Vitest, its output through the redaction list line by
// line, and fails if anything it printed holds a secret.
func (h *harness) runOwnerSide(ctx context.Context, env []string) error {
	cctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	cmd := lkit.GroupCommand(cctx, "npm", "run", "--silent", "test:e2e")
	cmd.Dir = filepath.Join(h.repo, "desktop")
	cmd.Env = env
	w := &redactingWriter{h: h}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	w.flush()
	if label, found := h.leak(w.all.String()); found {
		return fmt.Errorf("secret leak: %s appears in what the owner side printed", label)
	}
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("interrupted by a signal")
		}
		return fmt.Errorf("the owner side failed: %w", err)
	}
	return nil
}

type redactingWriter struct {
	h   *harness
	mu  sync.Mutex
	buf []byte
	all bytes.Buffer
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all.Write(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		fmt.Println(w.h.redact(string(w.buf[:i])))
		w.buf = w.buf[i+1:]
	}
}

func (w *redactingWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		fmt.Println(w.h.redact(string(w.buf)))
		w.buf = nil
	}
}

// ----------------------------------------------------------------- binaries

// voiceBinary is the conch-voice to run: CONCH_VOICE_BIN if it names one,
// else a build with the pinned toolchain (scripts/voice-env.sh), whose absence
// is a skip.
func (h *harness) voiceBinary() (bin, skip string, err error) {
	if p := os.Getenv("CONCH_VOICE_BIN"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", "", fmt.Errorf("CONCH_VOICE_BIN names %s, which is not there", abs)
		}
		return abs, "", nil
	}
	ctx, cancel := context.WithTimeout(stopCtx, 30*time.Minute)
	defer cancel()
	check := lkit.GroupCommand(ctx, "bash", "-c", ". ./scripts/voice-env.sh")
	check.Dir = h.repo
	if out, err := check.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", "", errors.New("interrupted by a signal")
		}
		return "", "the pinned Rust toolchain is not set up: " + lkit.OneLine(string(out), err), nil
	}
	build := lkit.GroupCommand(ctx, "bash", "-c", ". ./scripts/voice-env.sh && cd voice && cargo build --locked -p conch-voice")
	build.Dir = h.repo
	if out, err := build.CombinedOutput(); err != nil {
		text := string(out)
		if len(text) > 3000 {
			text = text[len(text)-3000:]
		}
		return "", "", fmt.Errorf("cargo build -p conch-voice: %w\n%s", err, text)
	}
	return filepath.Join(h.repo, "voice", "target", "debug", "conch-voice"), "", nil
}

func (h *harness) buildConchd() (string, error) {
	dir, err := h.mkdir("bin-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "conchd")
	cmd := lkit.GroupCommand(stopCtx, "go", "build", "-o", out, "./cmd/conchd")
	cmd.Dir = h.repo
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/conchd: %w\n%s", err, output)
	}
	return out, nil
}

// -------------------------------------------------------------------- conchd

type conchdProc struct {
	h        *harness
	baseURL  string
	operator string
	cmd      *exec.Cmd
	done     chan struct{}
	stopped  bool
}

func (h *harness) startConchd(bin string, srv *lkit.Server, webDir string) (*conchdProc, error) {
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
	bootstrap := lkit.GroupCommand(stopCtx, bin, "bootstrap-operator", "--data", dataDir, "--name", "desktop-voice-operator")
	bootstrap.Env = env
	tokenOut, err := bootstrap.Output()
	if err != nil {
		return nil, fmt.Errorf("bootstrap-operator: %w", err)
	}
	op := strings.TrimSpace(string(tokenOut))
	if op == "" || strings.ContainsAny(op, " \n") {
		return nil, errors.New("bootstrap-operator must print the token alone on stdout")
	}
	h.secret("operator credential", op)
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	env = append(env, "CONCHD_LIVEKIT_API_KEY="+srv.Key, "CONCHD_LIVEKIT_API_SECRET="+srv.Secret)
	logPath := filepath.Join(root, "conchd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- under this program's temp dir
	if err != nil {
		return nil, err
	}
	cmd := lkit.GroupStart(bin, "serve", "--data", dataDir, "--listen", addr, "--livekit-url", srv.WSURL, "--web-dir", webDir)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	d := &conchdProc{h: h, baseURL: "http://" + addr, operator: op, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(d.done) }()
	h.onCleanup(d.stop)
	h.keepLog(logPath)
	err = waitFor("conchd to answer /healthz", 15*time.Second, func() (bool, string) {
		resp, err := http.Get(d.baseURL + "/healthz") // #nosec G107 -- this program's own conchd
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode)
	})
	if err != nil {
		return nil, err
	}
	var ch schema.CreateChannelResponse
	if err := d.call(op, http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: channel}, &ch); err != nil {
		return nil, fmt.Errorf("create channel: %w", err)
	}
	return d, nil
}

func (d *conchdProc) stop() {
	if d.stopped {
		return
	}
	d.stopped = true
	_ = lkit.KillGroup(d.cmd)
	<-d.done
}

func (d *conchdProc) call(token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(stopCtx, method, d.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- this program's own conchd
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type principal struct {
	name  string
	id    int64
	token string
}

// newPrincipal makes a human, a credential, and a membership of the channel.
func (d *conchdProc) newPrincipal(name string) (*principal, error) {
	var created schema.CreatePrincipalResponse
	if err := d.call(d.operator, http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}, &created); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	var cred schema.CreateCredentialResponseV1
	if err := d.call(d.operator, http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), schema.CreateCredentialRequestV1{Label: name}, &cred); err != nil {
		return nil, fmt.Errorf("credential for %s: %w", name, err)
	}
	d.h.secret(name+"'s conch credential", cred.Token)
	if err := d.call(d.operator, http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channel, created.Principal.ID), nil, nil); err != nil {
		return nil, fmt.Errorf("add %s to %s: %w", name, channel, err)
	}
	return &principal{name: name, id: created.Principal.ID, token: cred.Token}, nil
}

// ---------------------------------------------------------- conch-voice join

// voiceJoin is one standalone `conch-voice join squad --json`: B, the
// listener, or A's second session.
type voiceJoin struct {
	cmd    *exec.Cmd
	done   chan struct{}
	exit   error
	errLog string

	mu     sync.Mutex
	events []joinEvent
}

type joinEvent struct {
	at  time.Time
	obj map[string]any
}

func (h *harness) startJoin(bin string, d *conchdProc, p *principal, label, mic, sink string) (*voiceJoin, error) {
	dir, err := h.mkdir("join-" + label + "-")
	if err != nil {
		return nil, err
	}
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		return nil, err
	}
	errPath := filepath.Join(dir, "stderr")
	errFile, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- under this program's temp dir
	if err != nil {
		return nil, err
	}
	cmd := lkit.GroupStart(bin, "join", channel, "--json", "--mic", mic, "--sink", sink)
	cmd.Env = append(cleanEnv(), "HOME="+dir, "CONCH_CONFIG_DIR="+cfg, "CONCH_SERVER="+d.baseURL, "CONCH_TOKEN="+p.token)
	cmd.Stderr = errFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = errFile.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = errFile.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = errFile.Close()
		return nil, err
	}
	j := &voiceJoin{cmd: cmd, done: make(chan struct{}), errLog: errPath}
	h.keepLog(errPath)
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "{") { // libwebrtc may print a line of its own
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line), &obj) == nil {
				j.mu.Lock()
				j.events = append(j.events, joinEvent{time.Now(), obj})
				j.mu.Unlock()
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		<-read
		j.exit = cmd.Wait()
		_ = errFile.Close()
		close(j.done)
	}()
	h.onCleanup(func() { _ = stdin.Close(); j.kill() })
	return j, nil
}

func (j *voiceJoin) kill() {
	_ = lkit.KillGroup(j.cmd)
	<-j.done
}

func (j *voiceJoin) exited() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

func (j *voiceJoin) tail() string {
	data, _ := os.ReadFile(j.errLog) // #nosec G304 -- under this program's temp dir
	s := strings.TrimSpace(string(data))
	if len(s) > 800 {
		s = s[len(s)-800:]
	}
	return s
}

func (j *voiceJoin) snapshot() []joinEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]joinEvent(nil), j.events...)
}

func (j *voiceJoin) connected() bool {
	for _, e := range j.snapshot() {
		if e.obj["event"] == "connection" && e.obj["state"] == "connected" {
			return true
		}
	}
	return false
}

func (h *harness) startListener(bin string, d *conchdProc, b *principal) (*voiceJoin, error) {
	j, err := h.startJoin(bin, d, b, "ears", "none", fmt.Sprintf("count:%d,%d", toneA, toneC))
	if err != nil {
		return nil, err
	}
	err = waitFor("B's conch-voice to connect", 60*time.Second, func() (bool, string) {
		if j.exited() {
			return false, "exited: " + j.tail()
		}
		return j.connected(), ""
	})
	if err != nil {
		return nil, fmt.Errorf("%w; standard error: %s", err, j.tail())
	}
	return j, nil
}

// ----------------------------------------------------------------- observer

type observer struct {
	url, token string
}

type heardLine struct {
	At      int64   `json:"at"`
	Frames  float64 `json:"frames"`
	Audible float64 `json:"audible"`
	Hz      float64 `json:"hz"`
}

type heardAnswer struct {
	Now        int64       `json:"now"`
	StatsLines int         `json:"stats_lines"`
	Lines      []heardLine `json:"lines"`
}

type participant struct {
	Identity string   `json:"identity"`
	Sources  []string `json:"sources"`
}

func num(v any) float64 { f, _ := v.(float64); return f }

// startObserver serves what the owner side asks of the world on loopback.
func (h *harness) startObserver(adm *lkit.Admin, ears *voiceJoin, bin string, d *conchdProc, a *principal) (*observer, error) {
	token := randHex(16)
	h.secret("the observer's bearer secret", token)
	var (
		mu     sync.Mutex
		second *voiceJoin
	)
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/heard", func(w http.ResponseWriter, r *http.Request) {
		speaker := r.URL.Query().Get("speaker")
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		ans := heardAnswer{Now: time.Now().UnixMilli(), Lines: []heardLine{}}
		for _, e := range ears.snapshot() {
			if e.obj["event"] != "stats" || e.at.UnixMilli() < since {
				continue
			}
			ans.StatsLines++
			speakers, _ := e.obj["speakers"].([]any)
			for _, raw := range speakers {
				s, _ := raw.(map[string]any)
				if id, _ := s["speaker"].(string); id == speaker {
					ans.Lines = append(ans.Lines, heardLine{At: e.at.UnixMilli(), Frames: num(s["frames"]), Audible: num(s["audible_frames"]), Hz: num(s["dominant_hz"])})
				}
			}
		}
		reply(w, ans)
	})
	mux.HandleFunc("/participants", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		rooms, err := adm.ListRooms(ctx)
		if err != nil {
			http.Error(w, "list rooms: "+err.Error(), http.StatusBadGateway)
			return
		}
		out := struct {
			Rooms        int           `json:"rooms"`
			Participants []participant `json:"participants"`
		}{Rooms: len(rooms), Participants: []participant{}}
		for _, room := range rooms {
			h.room(room)
			ps, err := adm.ListParticipants(ctx, room)
			if err != nil {
				http.Error(w, "list participants: "+err.Error(), http.StatusBadGateway)
				return
			}
			for _, p := range ps {
				out.Participants = append(out.Participants, participant{Identity: p.Identity, Sources: append([]string{}, p.Sources()...)})
			}
		}
		reply(w, out)
	})
	mux.HandleFunc("/second-session", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			if second != nil && !second.exited() {
				http.Error(w, "already running", http.StatusConflict)
				return
			}
			j, err := h.startJoin(bin, d, a, "second", "none", "count")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			second = j
			err = waitFor("A's second session to connect", 45*time.Second, func() (bool, string) {
				if j.exited() {
					return false, "exited"
				}
				return j.connected(), ""
			})
			if err != nil {
				http.Error(w, h.redact(err.Error()+": "+j.tail()), http.StatusBadGateway)
				return
			}
			reply(w, map[string]any{"connected": true})
		case http.MethodGet:
			reply(w, map[string]any{"running": second != nil && !second.exited(), "connected": second != nil && second.connected()})
		case http.MethodDelete:
			if second != nil {
				second.kill()
				second = nil
			}
			reply(w, map[string]any{"running": false})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: guard, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	h.onCleanup(func() { _ = srv.Close() })
	return &observer{url: "http://" + addr, token: token}, nil
}
