// Command sdkprobe measures what LiveKit's Rust SDK does in the situations the
// voice client's design relies on (issue #178; docs/design/conch-voice.md §13,
// rows 1 to 4, 10 and 12), against a real LiveKit server.
//
// It starts the pinned livekit/livekit-server image in Docker (the image and
// settings e2e/voice uses: localhost only, automatic room creation off), with
// an API secret made up for this run; builds the Rust probe
// (voice/conch-voice/examples/sdk_probe.rs); creates rooms and mints 15-second
// join tokens with internal/server/livekit, as conchd does; runs probe
// processes, doing to the server what each measurement needs (deleting a room,
// removing a participant, freezing and stopping the container); and prints
// what the probes reported.
//
//	go run ./e2e/sdkprobe                  everything (about 30 minutes)
//	go run ./e2e/sdkprobe -only mute,logs  some of it: mute, sent, reasons, resume, cost, logs
//	go run ./e2e/sdkprobe -reps 2          fewer repetitions (default 5)
//	go run ./e2e/sdkprobe -raw DIR         also keep every probe's lines in DIR
//
// It is a measurement, not a check: it is not part of `make check` or CI, and
// it exits 0 when it ran, whatever it measured. It exits 1 when it could not
// run, when a container of its own is left behind, or when a token, the API
// secret or a room name turned up in anything a probe wrote.
//
// Tokens reach a probe in its environment, never on a command line. Nothing
// here prints a token, the secret or a room name: every line passes through
// redact, and every line a probe writes is searched for them first. A SIGINT
// or SIGTERM stops the run and removes the container.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
)

// stopCtx is cancelled by the first SIGINT or SIGTERM. Every child runs under
// it and every wait watches it, so a signal makes the run unwind on its normal
// path and teardown runs once, after nothing is in flight.
var stopCtx, stopCancel = context.WithCancel(context.Background())

var errInterrupted = errors.New("interrupted by a signal")

// tokenLifetime is the lifetime conchd gives a join token (internal/server/voice.go).
const tokenLifetime = 15 * time.Second

// run is the state of one run.
type run struct {
	repo     string
	tmp      string // every file this run writes lives under it; removed at the end
	probeBin string
	rawDir   string // when set, each probe's lines are also written here
	reps     int
	clkTck   float64 // clock ticks per second, for /proc/self/stat

	srv *server
	api *livekit.Client

	mu         sync.Mutex
	secrets    map[string]string // value -> what it is
	probes     []*probe
	probeCount int
	lines      int            // lines read from probes
	leaks      []string       // what leaked and where, by name only
	stray      map[string]int // lines on a probe's standard output that the probe did not write
	maxDriftMS float64        // largest disagreement between a probe's monotonic and wall clocks
	serial     int
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	only := flag.String("only", "", "comma-separated measurements to run: mute, sent, reasons, resume, cost, logs (default: all)")
	reps := flag.Int("reps", 5, "repetitions of each measurement")
	raw := flag.String("raw", "", "directory to also write each probe's lines to (they hold no token, secret or room name)")
	flag.Parse()
	if *reps < 1 {
		fmt.Fprintln(os.Stderr, "sdkprobe: -reps must be at least 1")
		return 2
	}
	want, err := selection(*only)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdkprobe:", err)
		return 2
	}

	r, err := newRun(*reps, *raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdkprobe: FAIL:", err)
		return 1
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for n := 1; ; n++ {
			<-sigs
			switch n {
			case 1:
				fmt.Fprintln(os.Stderr, "sdkprobe: interrupted; cleaning up")
				stopCancel()
			case 2:
				fmt.Fprintln(os.Stderr, "sdkprobe: still cleaning up; signal again to give up (a container may be left behind)")
			default:
				fmt.Fprintln(os.Stderr, "sdkprobe: giving up on clean-up")
				os.Exit(130)
			}
		}
	}()

	start := time.Now()
	err = r.guarded(want)
	r.teardown()
	left := r.leftover()

	r.say("")
	r.say("== housekeeping ==")
	r.say("probe processes run: %d; lines read from them: %d", r.probeCount, r.lines)
	r.say("largest disagreement between a probe's monotonic clock and the wall clock over its life: %.3f ms", r.maxDriftMS)
	if len(r.stray) == 0 {
		r.say("lines on a probe's standard output that the probe did not write: none")
	}
	for line, n := range r.stray {
		r.say("lines on a probe's standard output that the probe did not write (native code, going round the log facade): %d times %q", n, line)
	}
	if len(r.leaks) == 0 {
		r.say("tokens, the API secret and room names found in anything a probe wrote: none")
	} else {
		r.say("LEAK: found in probe output: %s", strings.Join(r.leaks, "; "))
	}
	if left == "" {
		r.say("containers left with this run's label: none")
	} else {
		r.say("LEFT BEHIND: a container with this run's label is still there: %s", left)
	}
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "sdkprobe: interrupted")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "sdkprobe: FAIL:", r.redact(err.Error()))
		return 1
	case len(r.leaks) > 0 || left != "":
		return 1
	}
	r.say("sdkprobe: done in %s", time.Since(start).Round(time.Second))
	return 0
}

var measurements = []string{"mute", "sent", "reasons", "resume", "cost", "logs"}

func selection(only string) (map[string]bool, error) {
	want := map[string]bool{}
	if only == "" {
		for _, m := range measurements {
			want[m] = true
		}
		return want, nil
	}
	for _, name := range strings.Split(only, ",") {
		name = strings.TrimSpace(name)
		known := false
		for _, m := range measurements {
			known = known || m == name
		}
		if !known {
			return nil, fmt.Errorf("unknown measurement %q; the measurements are %s", name, strings.Join(measurements, ", "))
		}
		want[name] = true
	}
	return want, nil
}

func newRun(reps int, rawDir string) (*run, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return nil, errors.New("run this from the module root: go run ./e2e/sdkprobe")
	}
	tmp, err := os.MkdirTemp("", "sdk-probe-")
	if err != nil {
		return nil, err
	}
	if rawDir != "" {
		if err := os.MkdirAll(rawDir, 0o750); err != nil {
			return nil, err
		}
	}
	r := &run{repo: repo, tmp: tmp, rawDir: rawDir, reps: reps, clkTck: 100, secrets: map[string]string{}, stray: map[string]int{}}
	if out, err := exec.Command("getconf", "CLK_TCK").Output(); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil && n > 0 {
			r.clkTck = n
		}
	}
	return r, nil
}

// guarded runs the measurements, turning a panic into an error so that
// realMain still tears everything down.
func (r *run) guarded(want map[string]bool) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return r.all(want)
}

func (r *run) all(want map[string]bool) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not installed: this program needs it to run LiveKit")
	}
	r.say("== setup ==")
	if err := r.buildProbe(); err != nil {
		return err
	}
	srv, err := r.startLiveKit()
	if err != nil {
		return err
	}
	r.srv = srv
	cfg, err := livekit.ParseConfig(srv.wsURL, srv.apiURL, srv.key, srv.secret)
	if err != nil {
		return err
	}
	if r.api, err = livekit.New(cfg); err != nil {
		return err
	}
	r.say("LiveKit %s is up, signalling on 127.0.0.1 only; %d repetitions; join tokens live %s", livekitVersion, r.reps, tokenLifetime)

	steps := []struct {
		name string
		fn   func() error
	}{
		{"mute", r.measureMute},
		{"sent", r.measureSent},
		{"reasons", r.measureReasons},
		{"resume", r.measureResume},
		{"cost", r.measureCost},
		{"logs", r.measureLogs},
	}
	for _, step := range steps {
		if !want[step.name] {
			continue
		}
		began := time.Now()
		err := step.fn()
		r.stopProbes()
		if stopCtx.Err() != nil {
			return errInterrupted
		}
		if err != nil {
			// One measurement that could not be made does not stop the others.
			r.say("COULD NOT MEASURE %s: %s", step.name, err)
			if herr := r.srv.heal(); herr != nil {
				return fmt.Errorf("LiveKit did not come back after %s: %w", step.name, herr)
			}
			continue
		}
		r.say("(%s took %s)", step.name, time.Since(began).Round(time.Second))
	}
	return nil
}

// buildProbe builds the Rust probe with the pinned toolchain and copies it
// into this run's directory, so that another build into the shared target
// directory cannot replace it under a running measurement.
func (r *run) buildProbe() error {
	script := `. scripts/voice-env.sh && cd voice && cargo build --locked --release -p conch-voice --example sdk_probe 1>&2 && printf %s "$CARGO_TARGET_DIR"`
	cmd := exec.CommandContext(stopCtx, "bash", "-c", script) // #nosec G204 -- a constant script
	cmd.Dir = r.repo
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if stopCtx.Err() != nil {
		return errInterrupted
	}
	if err != nil {
		return fmt.Errorf("building the probe: %w\n%s", err, tail(stderr.String(), 2000))
	}
	built := filepath.Join(strings.TrimSpace(string(out)), "release", "examples", "sdk_probe")
	data, err := os.ReadFile(built) // #nosec G304 -- the binary cargo just built
	if err != nil {
		return fmt.Errorf("the probe was built but is not where cargo puts examples: %w", err)
	}
	r.probeBin = filepath.Join(r.tmp, "sdk_probe")
	if err := os.WriteFile(r.probeBin, data, 0o700); err != nil { // #nosec G306 G703 -- an executable only this user runs, in this run's own directory
		return err
	}
	r.say("built the probe (release profile)")
	return nil
}

// teardown stops every probe, removes the container and this run's files.
func (r *run) teardown() {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintln(os.Stderr, "sdkprobe: clean-up panicked:", r.redact(fmt.Sprint(p)))
		}
	}()
	r.stopProbes()
	if r.srv != nil {
		r.srv.remove()
	}
	_ = os.RemoveAll(r.tmp)
}

// leftover lists containers still carrying this run's label: empty when clean.
func (r *run) leftover() string {
	if r.srv == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := docker(ctx, "ps", "-aq", "--filter", "label="+r.srv.label)
	if err != nil {
		return "docker ps failed: " + out
	}
	return strings.Join(strings.Fields(out), " ")
}

// ------------------------------------------------------- output and secrets

// say prints one line, with anything secret in it replaced.
func (r *run) say(format string, args ...any) {
	fmt.Println(r.redact(fmt.Sprintf(format, args...)))
}

// secret registers a value that must never be printed.
func (r *run) secret(what, value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	r.secrets[value] = what
	r.mu.Unlock()
}

// redact replaces every registered secret in s.
func (r *run) redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for v, what := range r.secrets {
		s = strings.ReplaceAll(s, v, "["+what+" redacted]")
	}
	return s
}

// leak reports the first registered secret found in text, by what it is.
func (r *run) leak(text string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for v, what := range r.secrets {
		if strings.Contains(text, v) {
			return what, true
		}
	}
	return "", false
}

// noteLeak records that a probe wrote a secret, once per kind and place.
func (r *run) noteLeak(what, where string) {
	entry := what + " in " + where
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.leaks {
		if l == entry {
			return
		}
	}
	r.leaks = append(r.leaks, entry)
}

// token mints a join token as conchd does and registers it, and the two parts
// of it that are its own, as secrets. The first part of a JWT is the same for
// every token signed this way and is not registered.
func (r *run) token(identity, room string) (string, error) {
	tok, err := r.api.JoinToken(livekit.JoinParams{Identity: identity, Room: room, CanPublish: true, Lifetime: tokenLifetime})
	if err != nil {
		return "", err
	}
	r.secret("a join token", tok)
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		r.secret("a join token's claims", parts[1])
		r.secret("a join token's signature", parts[2])
	}
	return tok, nil
}

// newRoom makes up a room name, registers it as not to be printed, and
// creates the room unless create is false.
func (r *run) newRoom(create bool) (string, error) {
	name := "sdkprobe-" + randHex(10)
	r.secret("a room name", name)
	if !create {
		return name, nil
	}
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	if err := r.api.CreateRoom(ctx, name); err != nil {
		return "", fmt.Errorf("CreateRoom: %w", err)
	}
	return name, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// pause waits for d, or returns errInterrupted when a signal arrives first.
func pause(d time.Duration) error {
	select {
	case <-stopCtx.Done():
		return errInterrupted
	case <-time.After(d):
		return nil
	}
}
