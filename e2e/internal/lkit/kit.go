// Package lkit is the LiveKit side of the end-to-end programs that need a real
// media server: e2e/voice (the voice control plane) and e2e/isolation (one
// group cannot reach another's media).
//
// Its input is a Host, the program using it: where temporary directories go,
// which strings are secret, what to tear down at the end. Its output is a
// running LiveKit from the pinned image in Docker, the pinned lk tool, and the
// clients that stand in for people: lk participants admitted on a token conchd
// issued (through a relay that swaps the token in), bare signalling
// connections, and LiveKit's admin API for ground truth that does not pass
// through conchd. It owns the pins (the image by digest, lk by checksum), the
// container (named and labelled by the run, removed at teardown) and the skip
// rule's facts: whether Docker and the network are there.
package lkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"time"
)

// Host is what the kit needs from the program that uses it.
type Host interface {
	// Mkdir makes a temporary directory under the run's own, removed at the
	// end of the run.
	Mkdir(prefix string) (string, error)
	// OnCleanup registers f to run at teardown, last registered first.
	OnCleanup(f func())
	// Secret registers a value that must never be printed, logged or audited.
	Secret(label, value string)
	// Say prints one line of the run's output.
	Say(format string, args ...any)
	// Redact replaces every secret in s, for a message that quotes output.
	Redact(s string) string
	// Log registers a file the run scans for secrets before it ends.
	Log(what, path string)
}

// Kit starts LiveKit and its clients for one run.
type Kit struct {
	host Host
	stop context.Context
	// name names the run's container, label, API key and secret, so that two
	// programs on one host never touch each other's: "voice-check" gives the
	// container conch-voice-check-<random>, labelled
	// conch-voice-check=$VOICE_CHECK_RUN.
	name string
}

// New makes a kit for a run called name ("voice-check", "isolation-check").
// stop is cancelled by a signal: every child and every wait watches it.
func New(host Host, stop context.Context, name string) *Kit {
	return &Kit{host: host, stop: stop, name: name}
}

// errInterrupted is what a wait returns once the run's stop context is
// cancelled.
var errInterrupted = errors.New("interrupted by a signal")

// ---------------------------------------------------------------------- pins

const (
	// Image is pinned by version and by digest. The digest is that of the
	// multi-architecture index for v1.13.7.
	Image = "livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3"

	// LKVersion is the pinned livekit-cli release.
	LKVersion = "2.19.0"
)

// lkSHA256 are the SHA-256 sums of the lk release archives, from the
// checksums.txt published with livekit-cli v2.19.0.
var lkSHA256 = map[string]string{
	"linux_amd64":  "601ce74299db7e538014426644ab23b2a1ba171c506967197a259c3cd762d418",
	"linux_arm64":  "64e8f68f26eb1de81f0696a9a0fca7564c4e7e551de6cc98efc327ca02b56b52",
	"darwin_amd64": "9f7f23ebc5b146e9b3bcba63254e1792f40598247a468a415ef051e97b31ff57",
	"darwin_arm64": "f5e2413303d91814b0dfe9a0b6a284d8f095b3cb2558ad702970f653cfe1dfa9",
}

// ------------------------------------------------------------------ waiting

// waitForOrFail polls fn until it reports true, the deadline passes, or the
// run is stopped. A non-nil error from fn ends the wait at once.
func (k *Kit) waitForOrFail(what string, within time.Duration, fn func() (bool, string, error)) error {
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
		case <-k.stop.Done():
			return fmt.Errorf("waiting for %s: %w", what, errInterrupted)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- prepare

// Prepare checks what the live half of a run needs and fetches it: Docker,
// the pinned image, and the pinned lk, extracted into a temporary directory.
// It returns a reason to skip when the machine cannot run the live half (not
// Linux, no Docker, no network), and an error when it could but something is
// wrong: a pulled image or a downloaded lk that is not the pinned one is not a
// missing network, and fails everywhere.
func (k *Kit) Prepare(ctx context.Context) (lkBin, skip string, err error) {
	if goos := runtime.GOOS; goos != "linux" {
		return "", "the live half runs LiveKit on the Docker host network, which only Linux has (this is " + goos + ")", nil
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := Docker(pctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return "", "Docker is not available: " + OneLine(out, err), nil
	}
	ictx, icancel := context.WithTimeout(ctx, 5*time.Minute)
	defer icancel()
	if out, err := Docker(ictx, "pull", "--quiet", Image); err != nil {
		if ctx.Err() != nil {
			return "", "", errInterrupted
		}
		if LooksOffline(out) {
			return "", "cannot pull the LiveKit image (no network?): " + OneLine(out, err), nil
		}
		return "", "", fmt.Errorf("cannot pull the pinned LiveKit image %s: %s", Image, OneLine(out, err))
	}
	dir, err := k.host.Mkdir("lkbin-")
	if err != nil {
		return "", "", err
	}
	lkBin, err = FetchLK(ictx, dir)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", errInterrupted
		}
		if errors.Is(err, ErrUnreachable) {
			return "", "cannot download lk " + LKVersion + " (no network?): " + err.Error(), nil
		}
		return "", "", fmt.Errorf("lk %s: %w", LKVersion, err)
	}
	return lkBin, "", nil
}

// LooksOffline reports whether a failed pull is about reaching the registry,
// as opposed to what the registry answered.
func LooksOffline(out string) bool {
	out = strings.ToLower(out)
	for _, w := range []string{"no such host", "timeout", "timed out", "connection refused", "network is unreachable",
		"temporary failure", "tls handshake", "dial tcp", "no route to host", "i/o timeout", "could not resolve",
		"unexpected eof", "toomanyrequests", "rate limit", "deadline exceeded", "cannot connect to the docker daemon"} {
		if strings.Contains(out, w) {
			return true
		}
	}
	return false
}

// OneLine is the first line of a command's output, or the error when it
// printed nothing, cut to a length a message can carry.
func OneLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return Truncate(s, 160)
}

// ---------------------------------------------------------------------- docker

// Docker runs one docker command and returns its combined output.
func Docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- constant command; args are this program's own
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Server is the container this run started, named by this run.
type Server struct {
	kit    *Kit
	Name   string
	id     string // container id, once docker run has printed it
	APIURL string // http://127.0.0.1:port
	WSURL  string // ws://127.0.0.1:port
	Key    string
	Secret string
}

// StartLiveKit runs the pinned image on the host network, bound to localhost,
// with automatic room creation off. enable_remote_unmute lets a run stand in
// for a key press from the server side (LiveKit refuses a server-side unmute
// otherwise); conchd needs no such setting. The container is registered for
// removal before it is started, so a failure at any point still removes it.
func (k *Kit) StartLiveKit(ctx context.Context) (*Server, error) {
	// The ports are chosen by asking the kernel for free ones and letting them
	// go again, so something else on the machine can take one before LiveKit
	// binds it. LiveKit then exits saying so, and the answer is other ports.
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var srv *Server
		srv, err = k.startLiveKitOnce(ctx)
		if err == nil {
			return srv, nil
		}
		if !strings.Contains(err.Error(), "address already in use") || k.stop.Err() != nil {
			return nil, err
		}
		k.host.Say("     LiveKit could not bind a port that was free a moment before (attempt %d of 3); starting it on others", attempt)
	}
	return nil, err
}

// probeClient asks whether LiveKit is up. It gives up quickly: if LiveKit lost
// the race for its port, whatever won it may accept a connection and never
// answer, and a request with no timeout would wait for it for ever.
var probeClient = &http.Client{Timeout: 2 * time.Second}

// freeAddr is a loopback address nothing listened on a moment ago.
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// startLiveKitOnce is one attempt of StartLiveKit, on ports of its own.
func (k *Kit) startLiveKitOnce(ctx context.Context) (*Server, error) {
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
	srv := &Server{
		kit:    k,
		Name:   "conch-" + k.name + "-" + randHex(4),
		APIURL: "http://127.0.0.1:" + httpPort,
		WSURL:  "ws://127.0.0.1:" + httpPort,
		Key:    strings.ReplaceAll(k.name, "-", "") + "key",
		Secret: k.name + "-NOT-FOR-PRODUCTION-" + randHex(24),
	}
	k.host.Secret("the LiveKit API secret", srv.Secret)
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
	dir, err := k.host.Mkdir("livekit-")
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
	k.host.OnCleanup(func() { srv.Remove() })
	cmd := GroupCommand(k.stop, "docker", "run", "-d", "--name", srv.Name, "--label", k.containerLabel(), "--network", "host",
		"-e", "LIVEKIT_KEYS", "-v", cfgPath+":/etc/livekit.yaml:ro",
		Image, "--config", "/etc/livekit.yaml", "--node-ip", "127.0.0.1")
	cmd.Env = append(os.Environ(), "LIVEKIT_KEYS="+srv.Key+": "+srv.Secret)
	raw, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(raw))
	// docker prints the new container's id; Remove uses it. A signal that
	// arrives while docker run is working can leave a container behind with
	// no id for us: Remove then finds it by the name and label this run gave it.
	if id := lastField(out); len(id) == 64 {
		srv.id = id
	}
	if k.stop.Err() != nil {
		return nil, errInterrupted
	}
	if err != nil {
		return nil, fmt.Errorf("docker run: %s", k.host.Redact(out))
	}
	if err := k.waitForOrFail("LiveKit to answer on "+srv.APIURL, 60*time.Second, func() (bool, string, error) {
		resp, err := probeClient.Get(srv.APIURL) // #nosec G107 -- the container this run started
		if err != nil {
			// A container that has exited will never answer.
			if state, derr := Docker(ctx, "inspect", "--format", "{{.State.Status}}", srv.Name); derr == nil && strings.TrimSpace(state) == "exited" {
				return false, "", errors.New("the LiveKit container exited")
			}
			return false, UnwrapURLError(err).Error(), nil
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode), nil
	}); err != nil {
		logs, _ := Docker(ctx, "logs", "--tail", "20", srv.Name)
		srv.Remove()
		return nil, fmt.Errorf("%w\ncontainer log:\n%s", err, k.host.Redact(logs))
	}
	return srv, nil
}

// containerLabel is the label every container of this run carries. In CI the
// job sets <NAME>_RUN (VOICE_CHECK_RUN for the run called voice-check) to
// something unique to the run, and its clean-up step removes by that label,
// so that it can never remove the container of another job on the same host.
func (k *Kit) containerLabel() string {
	run := os.Getenv(strings.ToUpper(strings.ReplaceAll(k.name, "-", "_")) + "_RUN")
	if run == "" {
		run = "local"
	}
	return "conch-" + k.name + "=" + run
}

func randByte() byte {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return b[0] % 150 // keeps the range below 55000
}

// Admin is a client of this server's admin API.
func (s *Server) Admin() *Admin { return NewAdmin(s.APIURL, s.Key, s.Secret) }

// Stop stops the container, so LiveKit is down but its address is known.
func (s *Server) Stop(ctx context.Context) error {
	if out, err := Docker(ctx, "stop", "-t", "1", s.Name); err != nil {
		return fmt.Errorf("docker stop: %s", out)
	}
	return nil
}

// Start starts the stopped container again and waits for LiveKit to answer.
// It is a new LiveKit process on the same address: every room is gone.
func (s *Server) Start(ctx context.Context) error {
	if out, err := Docker(ctx, "start", s.Name); err != nil {
		return fmt.Errorf("docker start: %s", out)
	}
	return s.kit.waitForOrFail("LiveKit to answer again on "+s.APIURL, 60*time.Second, func() (bool, string, error) {
		resp, err := probeClient.Get(s.APIURL) // #nosec G107 -- the container this run started
		if err != nil {
			return false, UnwrapURLError(err).Error(), nil
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("status %d", resp.StatusCode), nil
	})
}

// Remove removes the container, running or not, by its id. It is safe to call
// twice. When docker run never reported an id (it was interrupted), the id is
// looked up by the name and label this run gave the container.
func (s *Server) Remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := s.id
	if id == "" {
		out, _ := Docker(ctx, "ps", "-aq", "--no-trunc", "--filter", "name=^/"+s.Name+"$", "--filter", "label="+s.kit.containerLabel())
		id = lastField(out)
	}
	if id != "" {
		_, _ = Docker(ctx, "rm", "-f", id)
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

// ErrUnreachable marks a download that failed for want of a network: the one
// failure of FetchLK that is a reason to skip.
var ErrUnreachable = errors.New("the download could not be completed")

// ChecksumError is a downloaded file that is not the one pinned in this
// file. That is evidence of tampering or of a wrong pin, never of a missing
// network, so the run fails on it everywhere and does not skip.
type ChecksumError struct{ msg string }

func (e *ChecksumError) Error() string { return e.msg }

// FetchLK downloads the pinned lk release, checks its SHA-256 against the
// sum written in this file, and extracts the lk binary into dir.
func FetchLK(ctx context.Context, dir string) (string, error) {
	key := runtime.GOOS + "_" + runtime.GOARCH
	want, ok := lkSHA256[key]
	if !ok {
		return "", fmt.Errorf("no lk %s release is pinned for %s", LKVersion, key)
	}
	url := fmt.Sprintf("https://github.com/livekit/livekit-cli/releases/download/v%s/lk_%s_%s.tar.gz", LKVersion, LKVersion, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download lk: %w: %w", ErrUnreachable, UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("download lk: %w: HTTP %d", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download lk: the pinned release URL answered HTTP %d", resp.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("download lk: %w: %w", ErrUnreachable, err)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", &ChecksumError{fmt.Sprintf("lk %s archive has SHA-256 %s, want %s: refusing to run it", LKVersion, got, want)}
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
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700) // #nosec G302 G304 G703 -- a fixed name in a directory the caller chose (a run's temp dir, or fetch-lk's argument); the downloaded lk must be executable
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

// Headless is `lk room join`, joined through a relay that substitutes the
// token conchd issued for the one lk signed itself.
type Headless struct {
	kit     *Kit
	Relay   *Relay
	cmd     *exec.Cmd
	LogPath string
	done    chan struct{}
}

// StartHeadless starts lk as a participant admitted by token alone,
// publishing silence (the Ogg file at oggPath) as its microphone.
func (k *Kit) StartHeadless(lkBin, oggPath, lkHostPort, token string) (*Headless, error) {
	return k.StartLK(lkBin, lkHostPort, token, nil, "--publish", oggPath)
}

// StartLK starts `lk room join` with the given options, admitted by token
// alone through a relay whose filter (nil for none) sees what lk sends.
func (k *Kit) StartLK(lkBin, lkHostPort, token string, filter func(context.Context, []byte) []byte, options ...string) (*Headless, error) {
	rl, err := StartRelay(lkHostPort, token, filter)
	if err != nil {
		return nil, err
	}
	return k.StartLKVia(lkBin, rl, options...)
}

// StartLKVia is StartLK through a relay already started, which it owns from
// then on: stopping the participant closes it.
func (k *Kit) StartLKVia(lkBin string, rl *Relay, options ...string) (*Headless, error) {
	dir, err := k.host.Mkdir("lk-")
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
	args := []string{"--url", rl.URL(), "--api-key", "relay-only-key", "--api-secret", "relay-only-secret-not-known-to-livekit-0000000000",
		"room", "join", "--identity", "relay-placeholder"}
	args = append(append(args, options...), "relay-placeholder-room")
	cmd := GroupStart(lkBin, args...)
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
	hl := &Headless{kit: k, Relay: rl, cmd: cmd, LogPath: logPath, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(hl.done) }()
	k.host.OnCleanup(hl.Stop)
	k.host.Log("lk output", logPath)
	return hl, nil
}

// Stop kills lk and closes its relay. It is safe to call twice.
func (hl *Headless) Stop() {
	_ = KillGroup(hl.cmd)
	<-hl.done
	hl.Relay.Close()
}

// Done is closed when lk has ended.
func (hl *Headless) Done() <-chan struct{} { return hl.done }

// Exited reports whether lk has ended.
func (hl *Headless) Exited() bool {
	select {
	case <-hl.done:
		return true
	default:
		return false
	}
}

// Tail is the end of lk's output, redacted, for a message.
func (hl *Headless) Tail() string {
	data, _ := os.ReadFile(hl.LogPath) // #nosec G304 -- a path under this program's temp dir
	s := string(data)
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return hl.kit.host.Redact(s)
}

// LogLines returns the lines of lk's output that contain every one of the
// strings.
func (hl *Headless) LogLines(contains ...string) []string {
	data, _ := os.ReadFile(hl.LogPath) // #nosec G304 -- a path under this program's temp dir
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if ContainsAll([]string{line}, contains...) {
			out = append(out, line)
		}
	}
	return out
}
