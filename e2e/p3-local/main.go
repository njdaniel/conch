// Command p3-local sets up the P3 local prototype and tears it down: one
// disposable conchd, two terminal clients as two people, a browser as a third,
// and two scripted agents, from a clean checkout (issue #283, the brief in
// docs/design/p3-local-prototype.md). Run it from the repository:
//
//	go run ./e2e/p3-local [--keep] [--reset] [--real-adapter [--runtime claude|codex]]
//	go run ./e2e/p3-local --keep --real-adapters claude,codex,ollama
//	go run ./e2e/p3-local --keep --advertise http://<host-ip>:<port>
//	go run ./e2e/p3-local --non-loopback
//
// It builds conchd, conch and conch-adapter into <root>/bin, starts conchd on
// a loopback port of its own choosing, provisions the community through
// `conch admin` and `conch nets`, reads it back through the API and compares,
// and writes one environment file per participant. With --keep it prints the
// command for each participant and returns, leaving the server running.
// Without it, it starts the server with short session thresholds, checks that
// each participant can connect and send, runs the automated P3 suite (issue
// #284: the thirteen rows of the brief's test table, with terminal clients on
// pseudo-terminals, the scripted agents as conch-adapter processes, and the API
// to look at what the server shows), and tears everything down. --reset stops
// what an earlier run started and removes its root. docs/p3-walkthrough.md is
// what to do with a kept community. With --keep --advertise the server binds to
// that address of this machine instead of loopback, every file names it, and
// the commands for a guest (a VM) are printed apart; the guest's credentials
// then travel in clear text over http://, which the run says first (issue
// #379). CONCH_P3_ROWS=P3-06,P3-07 limits the suite to those rows (and P3-01, which starts everyone) while working on them.
//
// The root is $CONCH_P3_ROOT, else $XDG_STATE_HOME/conch-p3, else
// ~/.local/state/conch-p3. This program never reads or writes the user's own
// conch configuration, and never touches a conchd it did not start.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	name       = "p3-local"
	markerName = ".conch-p3-root"
	// markerPrefix starts the marker file's one line, which ends with the run id.
	markerPrefix = "conch-p3-local run "
)

// fixtureScripts are the commands the scripted agents run in place of a model.
// They are copied into the root, so the printed commands do not depend on
// where the checkout is.
//
//go:embed fixtures/*.sh
var fixtureScripts embed.FS

// runCtx is cancelled when the run is interrupted. Every request, every child
// process and every wait hangs off it, so an interrupted run stops at once.
var runCtx = context.Background()

func main() {
	keep := flag.Bool("keep", false, "leave the server running and print the commands, instead of checking and tearing down")
	reset := flag.Bool("reset", false, "stop what an earlier run started and remove its root")
	realAdapter := flag.Bool("real-adapter", false, "write agent-a.env for the real Claude Code command instead of a fixture (it runs as you; see the warning)")
	runtimeName := flag.String("runtime", "claude", "with --real-adapter, which real command agent-a.env names: claude or codex")
	realList := flag.String("real-adapters", "", "with --keep, add an agent real-<runtime> for each of claude, codex, ollama (comma-separated); each model comes from CONCH_P3_MODEL_<RUNTIME>")
	advertiseURL := flag.String("advertise", "", "with --keep, bind conchd to this address of this machine (http://<ip>:<port>) and write it into every file, for a guest on the LAN")
	nonLoopback := flag.Bool("non-loopback", false, "without --keep, run the suite with conchd bound to this machine's own non-loopback IPv4 and every client pointed at it (CI uses it)")
	flag.Parse()
	runtimeSet := false
	flag.Visit(func(f *flag.Flag) { runtimeSet = runtimeSet || f.Name == "runtime" })
	if flag.NArg() != 0 || (*reset && (*keep || *realAdapter)) || (*runtimeName != "claude" && *runtimeName != "codex") || (runtimeSet && !*realAdapter) || (*realList != "" && (!*keep || *reset)) || (*nonLoopback && (*keep || *reset || *advertiseURL != "")) {
		fmt.Fprintln(os.Stderr, "usage: go run ./e2e/p3-local [--keep] [--real-adapter [--runtime claude|codex]] | --reset")
		fmt.Fprintln(os.Stderr, "       go run ./e2e/p3-local --keep --real-adapters claude,codex,ollama")
		fmt.Fprintln(os.Stderr, "       go run ./e2e/p3-local --keep --advertise http://<host-ip>:<port>")
		fmt.Fprintln(os.Stderr, "       go run ./e2e/p3-local --non-loopback")
		os.Exit(2)
	}
	// Before anything is built or started: a runtime without its model is
	// refused by name.
	realNames, models, err := parseRealAdapters(*realList, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": FAIL:", err)
		os.Exit(2)
	}
	adv, err := parseAdvertise(*advertiseURL, *keep, *reset, hostHasIP)
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": FAIL:", err)
		os.Exit(2)
	}
	if *nonLoopback {
		if adv, err = suiteAddress(); err != nil {
			fmt.Fprintln(os.Stderr, name+": FAIL:", err)
			os.Exit(2)
		}
		fmt.Printf("%s: --non-loopback: conchd is bound to %s, and every client is given that address\n\n", name, adv.bind)
	} else if adv != nil {
		// First of everything the run prints, and before anything is built.
		fmt.Println(adv.warning())
		fmt.Println()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	runCtx = ctx

	r, err := newRunner(*realAdapter, *runtimeName, realNames, models, adv)
	if err != nil {
		stop()
		fmt.Fprintln(os.Stderr, name+": FAIL:", err)
		os.Exit(1)
	}
	if *reset {
		err = r.reset()
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, name+": FAIL:", err)
			os.Exit(1)
		}
		return
	}

	// An interrupted run still removes what it made. The signal cancels
	// runCtx, so whatever the run is doing fails, run returns and its own
	// teardown runs. Should the run not return, it is stopped from here after
	// a wait.
	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			r.teardown()
			fmt.Fprintln(os.Stderr, name+": interrupted, and stopped by force")
			os.Exit(130)
		}
	}()
	err = r.run(*keep)
	close(finished)
	// An interrupt that lands after a successful --keep run has returned is
	// not an interrupted run: the kept server is meant to be running.
	interrupted := ctx.Err() != nil && err != nil
	stop()
	if interrupted {
		fmt.Fprintln(os.Stderr, name+": interrupted; nothing is left running")
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": FAIL:", r.redact(err.Error()))
		os.Exit(1)
	}
}

// runner is one invocation: the root it works in and what it has started.
type runner struct {
	repo        string
	root        string
	runID       string
	realAdapter bool
	// runtime is which real command --real-adapter names: claude or codex.
	runtime string
	// real are the runtimes --real-adapters asked for, each an agent of its own
	// named real-<runtime>, and models the model of each.
	realAgents []string
	models     map[string]string
	// hold is a test hook: CONCH_P3_HOLD=build|provision|check makes the run
	// wait, interruptible, at that stage, so a test can interrupt it exactly
	// there.
	hold string
	// advertise is set by --advertise: conchd binds to its address and every
	// file and printed command names its URL.
	advertise *advertise
	// webDir is the built web client conchd serves; CONCH_P3_WEB_DIR names one
	// that is not repo/web/dist, and then nothing is built for it.
	webDir    string
	ownWebDir bool

	// suite is set for a run without --keep: the server then gets the short
	// session thresholds the scenarios need (P3-07). A kept community is the
	// walkthrough's, and runs with the defaults it describes.
	suite bool

	bin    binaries
	addr   string // host:port conchd listens on
	server *child
	fx     *fixtures

	mu       sync.Mutex
	children []*child
	secrets  []string
	torn     bool
	rootMade bool
}

type binaries struct{ conchd, conch, adapter string }

func newRunner(realAdapter bool, runtimeName string, realNames []string, models map[string]string, adv *advertise) (*runner, error) {
	repo, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	root, err := rootDir()
	if err != nil {
		return nil, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	r := &runner{repo: repo, root: root, runID: hex.EncodeToString(id), realAdapter: realAdapter, runtime: runtimeName, realAgents: realNames, models: models, advertise: adv, hold: os.Getenv("CONCH_P3_HOLD")}
	r.webDir, r.ownWebDir = filepath.Join(repo, "web", "dist"), true
	if dir := os.Getenv("CONCH_P3_WEB_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return nil, errors.New("CONCH_P3_WEB_DIR must be an absolute path")
		}
		r.webDir, r.ownWebDir = dir, false
	}
	r.bin = binaries{
		conchd:  filepath.Join(root, "bin", "conchd"),
		conch:   filepath.Join(root, "bin", "conch"),
		adapter: filepath.Join(root, "bin", "conch-adapter"),
	}
	return r, nil
}

// moduleRoot finds the repository by walking up from the working directory to
// its go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod")) // #nosec G304 -- walking up from the working directory
		if err == nil && strings.Contains(string(data), "module github.com/njdaniel/conch\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run this program from inside the conch repository (go run ./e2e/p3-local)")
		}
		dir = parent
	}
}

// rootDir is where the run lives. It is always an absolute path of its own: a
// directory named conch-p3 under the state directory, never a directory the
// user keeps anything else in unless they point CONCH_P3_ROOT at it, and then
// reset's marker-file rule protects it.
func rootDir() (string, error) {
	root := os.Getenv("CONCH_P3_ROOT")
	if root == "" {
		def, err := defaultRoot()
		if err != nil {
			return "", fmt.Errorf("no home directory; set CONCH_P3_ROOT: %w", err)
		}
		root = def
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("the root must be an absolute path (CONCH_P3_ROOT, XDG_STATE_HOME)")
	}
	return filepath.Clean(root), nil
}

// ---------------------------------------------------------------- the run

// hold waits at stage when the test hook asks for it.
func (r *runner) holdAt(stage string) error {
	if r.hold != stage {
		return nil
	}
	fmt.Printf("%s: holding at %s\n", name, stage)
	<-runCtx.Done()
	return runCtx.Err()
}

func stage(title string) { fmt.Printf("== %s ==\n", title) }

// step logs a passed step.
func step(format string, args ...any) { fmt.Printf("ok  %s\n", fmt.Sprintf(format, args...)) }

func (r *runner) run(keep bool) (err error) {
	armed := true
	defer func() {
		if !armed {
			return
		}
		// On a failure, the end of the server's log is the first thing anyone
		// will want. It is read before teardown removes it.
		if err != nil && runCtx.Err() == nil {
			r.showServerLog()
		}
		r.teardown()
	}()

	if err := r.createRoot(); err != nil {
		return err
	}
	if r.realAdapter {
		fmt.Fprintln(os.Stderr, r.realAdapterWarning())
	}
	if len(r.realAgents) > 0 {
		fmt.Fprintln(os.Stderr, realAdaptersWarning)
	}

	stage("build")
	if err := r.holdAt("build"); err != nil {
		return err
	}
	if err := r.build(); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	r.suite = !keep
	stage("server")
	if err := r.startServer(); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	stage("provision")
	if err := r.holdAt("provision"); err != nil {
		return err
	}
	if err := r.provision(); err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	if err := r.writeParticipantFiles(); err != nil {
		return fmt.Errorf("participant files: %w", err)
	}
	if err := r.verifyFiles(); err != nil {
		return fmt.Errorf("participant files: %w", err)
	}

	if keep {
		if err := r.printCommands(); err != nil {
			return err
		}
		armed = false // the server stays up; reset stops it
		return nil
	}

	stage("check")
	if err := r.holdAt("check"); err != nil {
		return err
	}
	if err := r.checkParticipants(); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	stage("scenarios")
	if err := r.runSuite(); err != nil {
		return err
	}
	r.teardown()
	armed = false
	if err := r.leftovers(); err != nil {
		return err
	}
	fmt.Println(name + ": PASS")
	return nil
}

// createRoot makes the root directory and its marker file. A root that is
// already there is never reused: it may hold a running server.
func (r *runner) createRoot() error {
	if err := os.MkdirAll(filepath.Dir(r.root), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(r.root, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; run go run ./e2e/p3-local --reset first (with CONCH_P3_ROOT set the same way)", r.root)
		}
		return err
	}
	r.rootMade = true
	// The root is resolved once so that the paths written into files and
	// compared with /proc are the real ones.
	resolved, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return err
	}
	if resolved != r.root {
		r.root = resolved
		r.bin = binaries{
			conchd:  filepath.Join(resolved, "bin", "conchd"),
			conch:   filepath.Join(resolved, "bin", "conch"),
			adapter: filepath.Join(resolved, "bin", "conch-adapter"),
		}
	}
	if err := writeFile(filepath.Join(r.root, markerName), []byte(markerPrefix+r.runID+"\n"), 0o600); err != nil {
		return err
	}
	for _, dir := range []string{"bin", "data", "home", "tmp", "config", "logs", "fixtures"} {
		if err := os.Mkdir(filepath.Join(r.root, dir), 0o700); err != nil {
			return err
		}
	}
	return writePIDs(r.root, nil)
}

// ----------------------------------------------------------------- reset

// reset stops what the root's run started and removes the root. A directory
// without the marker file was not made by this program and is refused.
func (r *runner) reset() error {
	switch err := checkRootOwned(r.root); {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Printf("%s: nothing to reset: %s does not exist\n", name, r.root)
		return nil
	case err != nil:
		return err
	}
	// The run resolved symlinks when it made the root, so its processes carry
	// the real path; compare against that, or a root reached through a
	// symlinked parent would match nothing and orphan its server.
	// checkRootOwned has already refused a symlink at the root itself.
	if resolved, err := filepath.EvalSymlinks(r.root); err == nil {
		r.root = resolved
	}
	if err := stopRootProcesses(r.root, 3*time.Second); err != nil {
		return err
	}
	if err := os.RemoveAll(r.root); err != nil {
		return err
	}
	fmt.Printf("%s: reset: stopped everything under %s and removed it\n", name, r.root)
	return nil
}

// checkRootOwned succeeds only for a real directory (not a link) holding the
// marker file this program writes. fs.ErrNotExist means there is no root.
func checkRootOwned(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to reset %s: it is not a directory this program made", root)
	}
	marker, err := os.ReadFile(filepath.Join(root, markerName)) // #nosec G304 -- the marker of the root being checked
	if err != nil || !strings.HasPrefix(string(marker), markerPrefix) {
		return fmt.Errorf("refusing to reset %s: it has no %s marker file, so this program did not make it", root, markerName)
	}
	return nil
}

// teardown stops everything this run started and removes the root, once.
func (r *runner) teardown() {
	r.mu.Lock()
	if r.torn {
		r.mu.Unlock()
		return
	}
	r.torn = true
	children := r.children
	r.mu.Unlock()

	for i := len(children) - 1; i >= 0; i-- {
		children[i].kill()
	}
	if !r.rootMade {
		return
	}
	// Anything else running a binary of this root (an adapter a check
	// started has been stopped above; this is for the stragglers).
	_ = stopRootProcesses(r.root, time.Second)
	if checkRootOwned(r.root) == nil {
		_ = os.RemoveAll(r.root)
	}
}

// leftovers reports anything a finished run should have removed and did not:
// its root, a process, or something still listening on its port.
func (r *runner) leftovers() error {
	if _, err := os.Lstat(r.root); err == nil {
		return fmt.Errorf("the run's root %s is still there", r.root)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot tell whether the run's root is gone: %w", err)
	}
	for _, c := range r.snapshot() {
		if processRunning(c.pid()) {
			return fmt.Errorf("%s (pid %d) is still running after teardown", c.name, c.pid())
		}
	}
	if r.addr != "" {
		if conn, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
			_ = conn.Close()
			return fmt.Errorf("something still listens on %s after teardown", r.addr)
		}
	}
	step("P3-12 nothing left behind: the root is gone, every process has exited and the port is closed")
	return nil
}

func (r *runner) snapshot() []*child {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*child(nil), r.children...)
}

// secret records a credential so that no message prints it.
func (r *runner) secret(token string) {
	if token == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, token)
}

// redact hides every credential this run knows from a message.
func (r *runner) redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.secrets {
		s = strings.ReplaceAll(s, t, "[token]")
	}
	return s
}

// --------------------------------------------------------------- children

// child is a process this run started and keeps running after the command
// that started it returns: the server, an adapter during a check.
type child struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	log  string
}

func (c *child) pid() int { return c.cmd.Process.Pid }

// kill stops the child and everything in its process group, and waits for it.
func (c *child) kill() {
	_ = signalProcess(c.pid(), false)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// stop asks the child to stop (as an interrupt does), then makes sure.
func (c *child) stop() {
	_ = signalProcess(c.pid(), true)
	select {
	case <-c.done:
		return
	case <-time.After(5 * time.Second):
	}
	c.kill()
}

func (c *child) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// isolatedEnv is the environment of every process this program starts, other
// than the build: the caller's, minus anything that says who they are, with
// HOME, the XDG directories and the temporary directory all inside the root,
// so a program that falls back to the user's own configuration finds the
// root's instead.
func (r *runner) isolatedEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CONCHD_") ||
			strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	home := filepath.Join(r.root, "home")
	env = append(env, "HOME="+home,
		"TMPDIR="+filepath.Join(r.root, "tmp"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"CONCH_P3_RUN_ID="+r.runID)
	return append(env, extra...)
}

// spawn starts a process that outlives this call, with its output in a log
// file under the root and a process group of its own. It is not tied to
// runCtx: with --keep it has to survive this program, and teardown stops it
// otherwise.
func (r *runner) spawn(label, bin string, args, env []string) (*child, error) {
	logPath := filepath.Join(r.root, "logs", label+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- under the run's root
	if err != nil {
		return nil, err
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(bin, args...) // #nosec G204 -- a binary this program built into the root, and its own arguments
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{name: label, cmd: cmd, done: make(chan struct{}), log: logPath}
	go func() { _ = cmd.Wait(); close(c.done) }()
	r.mu.Lock()
	r.children = append(r.children, c)
	r.mu.Unlock()
	return c, r.record(label, c.pid())
}

// record adds a process to pids.json.
func (r *runner) record(label string, pid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := readPIDs(r.root)
	if err != nil {
		return err
	}
	return writePIDs(r.root, append(recs, procRecord{Name: label, PID: pid}))
}

// output runs a short command to its end and returns its standard output; its
// standard error goes into the error. It is killed, with its group, when the
// run is interrupted.
func (r *runner) output(stdin string, dir string, env []string, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(runCtx, bin, args...) // #nosec G204 -- binaries this program built, and its own arguments
	cmd.Dir, cmd.Env = dir, env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	ownProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return nil, runCtx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, tailLines(detail, 20))
	}
	return []byte(stdout.String()), nil
}

// ------------------------------------------------------------------ build

func (r *runner) build() error {
	buildEnv := append(os.Environ(), "CONCH_P3_RUN_ID="+r.runID)
	for _, b := range []struct{ out, pkg string }{
		{r.bin.conchd, "./cmd/conchd"},
		{r.bin.conch, "./cmd/conch"},
		{r.bin.adapter, "./cmd/conch-adapter"},
	} {
		if _, err := r.output("", r.repo, buildEnv, "go", "build", "-o", b.out, b.pkg); err != nil {
			return err
		}
	}
	step("built conchd, conch and conch-adapter into %s", filepath.Join(r.root, "bin"))

	if !r.ownWebDir {
		step("using the web client in %s (CONCH_P3_WEB_DIR); nothing built for it", r.webDir)
		return checkIndex(r.webDir)
	}
	stale, err := webStale(r.webDir, filepath.Join(r.repo, "web", "src"))
	if err != nil {
		return err
	}
	if !stale {
		step("web/dist is newer than web/src; not rebuilt")
		return nil
	}
	fmt.Println("building the web client (make web-build; the first time takes a while)")
	if _, err := r.output("", r.repo, buildEnv, "make", "web-build"); err != nil {
		return fmt.Errorf("%w (it needs the Node that web/.nvmrc pins on PATH or under $HOME/.nvm)", err)
	}
	step("built web/dist")
	return checkIndex(r.webDir)
}

// tailLines is the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func checkIndex(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("%s has no index.html: %w", dir, err)
	}
	return nil
}

// webStale reports whether the built client in dist is missing or older than
// the newest file under src.
func webStale(dist, src string) (bool, error) {
	built, err := os.Stat(filepath.Join(dist, "index.html"))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	stale := false
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(built.ModTime()) {
			stale = true
		}
		return nil
	})
	return stale, err
}

// ----------------------------------------------------------------- server

// listeningOn is the line conchd prints once it has its port: the loopback
// one it chose, or the address --advertise asked for.
var listeningOn = regexp.MustCompile(`(?m)^conchd \S+ listening on (\S+:\d+) `)

// startServer bootstraps the operator into a fresh data directory and starts
// conchd on a loopback port it chooses itself, or, with --advertise, on the
// address given. conchd is asked for port 0 and says which port it got:
// choosing a free port here and handing it over would leave a moment in which
// something else could take it.
func (r *runner) startServer() error {
	data := filepath.Join(r.root, "data")
	out, err := r.output("", r.repo, r.isolatedEnv(), r.bin.conchd, "bootstrap-operator", "--data", data, "--name", "p3-operator")
	if err != nil {
		return fmt.Errorf("bootstrap-operator: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \n") {
		return fmt.Errorf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(out))
	}
	r.secret(token)
	r.fx = newFixtures(token)

	listen := "127.0.0.1:0"
	if r.advertise != nil {
		listen = r.advertise.bind
	}
	c, err := r.spawn("conchd", r.bin.conchd, r.serveArgs(listen), r.isolatedEnv())
	if err != nil {
		return err
	}
	r.server = c
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !c.running() {
			return errors.New("conchd exited before it was ready")
		}
		if r.addr == "" {
			if log, err := os.ReadFile(c.log); err == nil {
				if m := listeningOn.FindSubmatch(log); m != nil {
					r.addr = string(m[1])
					if r.advertise != nil && r.addr != r.advertise.bind {
						return fmt.Errorf("conchd listens on %s, not on the advertised %s", r.addr, r.advertise.bind)
					}
				}
			}
		}
		if r.addr != "" {
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + r.addr + "/healthz") // #nosec G107 -- this run's own loopback conchd
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					if err := r.checkAdvertised(); err != nil {
						return err
					}
					step("conchd is serving on http://%s (pid %d)", r.addr, c.pid())
					return nil
				}
			}
		}
		if err := pause(50 * time.Millisecond); err != nil {
			return err
		}
	}
	return errors.New("conchd did not become healthy within 20s")
}

// checkAdvertised fetches /healthz from the address the files will name, so a
// mismatch between it and where conchd listens fails the run instead of
// leaving files that cannot work.
func (r *runner) checkAdvertised() error {
	if r.advertise == nil {
		return nil
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(r.baseURL() + "/healthz") // #nosec G107 -- the address this run's own conchd was bound to
	if err != nil {
		return fmt.Errorf("conchd does not answer on the advertised %s: %w", r.baseURL(), err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("conchd answered %d on the advertised %s/healthz", resp.StatusCode, r.baseURL())
	}
	return nil
}

// The heartbeat thresholds of a server the suite runs against. Passed to
// conchd as flags, as an operator would; nothing is built into a test binary.
const (
	suiteSessionStale   = "2s"
	suiteSessionOffline = "5s"
)

// serveArgs are the arguments of conchd serve, listening on listen.
func (r *runner) serveArgs(listen string) []string {
	args := []string{"serve", "--listen", listen, "--web-dir", r.webDir, "--data", filepath.Join(r.root, "data")}
	if r.suite {
		args = append(args, "--session-stale", suiteSessionStale, "--session-offline", suiteSessionOffline)
	}
	return args
}

// pause waits for d, or returns the reason the run was interrupted.
func pause(d time.Duration) error {
	select {
	case <-runCtx.Done():
		return runCtx.Err()
	case <-time.After(d):
		return nil
	}
}

// localURL is where this program itself reaches conchd: always plain http on
// the address conchd bound, even when an https:// address is advertised.
func (r *runner) localURL() string { return "http://" + r.addr }

// baseURL is the server's address as a client types it: the advertised URL
// when there is one, else the loopback address conchd chose.
func (r *runner) baseURL() string {
	if r.advertise != nil {
		return r.advertise.url
	}
	return "http://" + r.addr
}

// showServerLog prints the end of the server's log, credentials hidden.
func (r *runner) showServerLog() {
	if r.server == nil {
		return
	}
	data, err := os.ReadFile(r.server.log)
	if err != nil || len(data) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "--- the end of conchd's log ---")
	fmt.Fprintln(os.Stderr, r.redact(tailLines(strings.TrimRight(string(data), "\n"), 40)))
	fmt.Fprintln(os.Stderr, "--- end ---")
}

const realAdapterWarning = `WARNING: --real-adapter. agent-a will run the default Claude Code command
(claude -p ... --dangerously-skip-permissions) with no fixture mode. It runs
as you, on this machine, in the directory you start it from, with every
permission check off: it can read, change and delete what your account can,
and it spends your own model quota. Nothing in this setup limits it. Start it
only in a directory you would let it change. Nothing is started for you, and
agent-b stays a fixture.`

const realCodexWarning = `WARNING: --real-adapter --runtime codex. agent-a will run the default Codex
command (codex exec --json ... --sandbox read-only) with no fixture mode. It
runs as you, on this machine, in the directory you start it from, signed in as
you, and it spends your own model quota. Codex's read-only sandbox lets it read
files and run commands that do not write, but it can read what your account can,
and the command you set in CONCH_ADAPTER_CMD instead is run as written. Start it
only in a directory you would let it read. Nothing is started for you, and
agent-b stays a fixture.`

func (r *runner) realAdapterWarning() string {
	if r.runtime == "codex" {
		return realCodexWarning
	}
	return realAdapterWarning
}

const realAdaptersWarning = `WARNING: --real-adapters. Each real-<runtime> agent will run that runtime's
default command with no fixture mode: Claude Code with every permission check
off, Codex in its read-only sandbox, Ollama as a local model that only writes
text. They run as you, on this machine, in the directory you start them from,
and claude and codex spend your own quota. Nothing is started for you; start
only the ones you mean to, in a directory you would let them read.`

// modelVar is the variable that names the model of a real runtime.
func modelVar(runtime string) string { return "CONCH_P3_MODEL_" + strings.ToUpper(runtime) }

// parseRealAdapters reads the --real-adapters list: comma-separated names from
// the runtimes there are, each once. It returns the names in the order given
// and the model of each from CONCH_P3_MODEL_<RUNTIME>; a runtime whose variable
// is unset or empty is an error naming it, because no model is invented.
func parseRealAdapters(list string, lookup func(string) (string, bool)) ([]string, map[string]string, error) {
	if list == "" {
		return nil, nil, nil
	}
	var names []string
	models := make(map[string]string)
	for _, n := range strings.Split(list, ",") {
		n = strings.TrimSpace(n)
		known := false
		for _, rt := range realRuntimes {
			known = known || rt.name == n
		}
		switch {
		case !known:
			return nil, nil, fmt.Errorf("--real-adapters: %q is not a runtime; use claude, codex or ollama", n)
		case slices.Contains(names, n):
			return nil, nil, fmt.Errorf("--real-adapters: %s is listed twice", n)
		}
		model, _ := lookup(modelVar(n))
		if model == "" {
			return nil, nil, fmt.Errorf("--real-adapters %s: %s is not set; it names the model real-%s runs", n, modelVar(n), n)
		}
		if !safeWord.MatchString(model) || strings.HasPrefix(model, "-") {
			return nil, nil, fmt.Errorf("--real-adapters %s: %s is not a model name (one word, no leading '-')", n, modelVar(n))
		}
		names, models[n] = append(names, n), model
	}
	return names, models, nil
}
