package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// These tests run the real program, as a user does: it is built once, and
// every test runs it with a scratch HOME, XDG_STATE_HOME and root of its own.
// They need Linux (/proc, sh), a Go toolchain and the repository's module
// cache; they build nothing for the web client (CONCH_P3_WEB_DIR points at a
// stub page).

var (
	runnerBin string
	repoDir   string
	goCache   string
	goModules string
	goPath    string
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if runtime.GOOS == "linux" {
		dir, err := os.MkdirTemp("", "p3-local-test-")
		if err != nil {
			panic(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if repoDir, err = moduleRoot(); err != nil {
			panic(err)
		}
		runnerBin = filepath.Join(dir, "p3-local")
		if out, err := exec.Command("go", "build", "-o", runnerBin, ".").CombinedOutput(); err != nil { // #nosec G204 -- the program this test built, or a command it printed
			panic("build the runner: " + err.Error() + "\n" + string(out))
		}
		for _, v := range []struct {
			name string
			dst  *string
		}{{"GOCACHE", &goCache}, {"GOMODCACHE", &goModules}, {"GOPATH", &goPath}} {
			out, err := exec.Command("go", "env", v.name).Output() // #nosec G204 -- the program this test built, or a command it printed
			if err != nil {
				panic(err)
			}
			*v.dst = strings.TrimSpace(string(out))
		}
	}
	return m.Run()
}

// scratch is one test's world.
type scratch struct {
	t     *testing.T
	dir   string
	home  string
	state string
	web   string
	root  string // what the runner will use: state/conch-p3
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the runner's tests need Linux")
	}
	if testing.Short() {
		t.Skip("builds and runs real binaries")
	}
	s := &scratch{t: t, dir: t.TempDir()}
	s.home = filepath.Join(s.dir, "home")
	s.state = filepath.Join(s.dir, "state")
	s.web = filepath.Join(s.dir, "web")
	for _, d := range []string{s.home, s.state, s.web} {
		must(t, os.MkdirAll(d, 0o750))
	}
	must(t, os.WriteFile(filepath.Join(s.web, "index.html"), []byte("<!doctype html><html><body>stub</body></html>\n"), 0o600))
	s.root = filepath.Join(s.state, "conch-p3")
	// Whatever a test does, nothing it started outlives it.
	t.Cleanup(func() {
		cmd := s.command("--reset")
		_ = cmd.Run()
	})
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// env is the runner's environment: the caller's with nothing of Conch's,
// HOME and the state directory in the scratch, and the Go caches where they
// were so that a build is not from nothing.
func (s *scratch) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+s.home, "XDG_STATE_HOME="+s.state, "CONCH_P3_WEB_DIR="+s.web,
		"GOCACHE="+goCache, "GOMODCACHE="+goModules, "GOPATH="+goPath)
	return env
}

func (s *scratch) command(args ...string) *exec.Cmd {
	cmd := exec.Command(runnerBin, args...) // #nosec G204 -- the program this test built
	cmd.Dir = repoDir
	cmd.Env = s.env()
	return cmd
}

// run runs the program to its end and returns what it printed to standard
// output and to standard error.
func (s *scratch) run(args ...string) (stdout, stderr string, err error) {
	cmd := s.command(args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

func (s *scratch) mustRun(args ...string) string {
	s.t.Helper()
	stdout, stderr, err := s.run(args...)
	if err != nil {
		s.t.Fatalf("p3-local %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout, stderr)
	}
	return stdout
}

// sh runs a printed command as a user's shell would.
func (s *scratch) sh(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command+" 2>&1") // #nosec G204 -- a command the runner printed
	cmd.Env = s.env()
	// The deadline must end the command and what it started, or the pipe it
	// holds keeps this call waiting.
	// It is asked to stop, as ctrl-c does, so that an adapter cleans up after
	// itself; WaitDelay is the force.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return signalProcess(cmd.Process.Pid, true) }
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// processesOf lists the live processes that carry run in their environment:
// everything the runner started for that run, builds included.
func processesOf(t *testing.T, runID string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	must(t, err)
	var found []string
	needle := []byte("CONCH_P3_RUN_ID=" + runID + "\x00")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !processRunning(pid) {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ") // #nosec G304 -- a path in this test's scratch or the repository
		if err == nil && bytes.Contains(env, needle) {
			found = append(found, e.Name()+" "+strings.Join(argv(pid), " "))
		}
	}
	return found
}

func runIDOf(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, markerName)) // #nosec G304 -- a path in this test's scratch or the repository
	must(t, err)
	return strings.TrimSpace(strings.TrimPrefix(string(data), markerPrefix))
}

func assertGone(t *testing.T, root, runID string) {
	t.Helper()
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the root %s is still there (err %v)", root, err)
	}
	// Processes need a moment to be reaped by init.
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := processesOf(t, runID)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes of the run are still there: %v", left)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// snapshot is every path under dir with its size and modification time, except
// the Go toolchain's own telemetry directory: `go build` writes there, under
// whatever HOME it is given, and it is not the runner's doing.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	must(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "go" && filepath.Base(filepath.Dir(path)) == ".config" {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if path == filepath.Join(dir, ".config") {
			return nil // its modification time moves when go makes its own directory
		}
		out[path] = info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()
		return nil
	}))
	return out
}

// printedValue finds "  key name value" in the list the runner prints.
func printedValue(t *testing.T, out, key, name string) string {
	t.Helper()
	prefix := "  " + key + " " + name + " "
	if name == "" {
		prefix = "  " + key + " "
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("the output has no %q line", strings.TrimSpace(prefix))
	return ""
}

// -------------------------------------------------------------- the cycle

// TestKeepResetCycle covers the acceptance criteria that need a kept run: the
// printed commands work as printed, the files are private and hold no operator
// credential, reset leaves nothing, a second reset is harmless, the next run
// starts clean, and the whole time nothing under HOME changes (the sentinel).
func TestKeepResetCycle(t *testing.T) {
	s := newScratch(t)
	// The sentinel: a Conch configuration of the user's own, which must not be
	// read, changed, or added to.
	sentinel := filepath.Join(s.home, ".config", "conch")
	must(t, os.MkdirAll(sentinel, 0o700))
	must(t, os.WriteFile(filepath.Join(sentinel, "credentials.json"), []byte(`{"sentinel":true}`), 0o600))
	before := snapshot(t, s.home)

	out := s.mustRun("--keep")
	runID := runIDOf(t, s.root)
	if !strings.Contains(out, "ready.") {
		t.Fatalf("the run did not end ready:\n%s", out)
	}
	address := printedValue(t, out, "address", "")
	if resp, err := http.Get("http://" + address + "/healthz"); err != nil || resp.StatusCode != http.StatusOK { // #nosec G107 -- this test's own server
		t.Fatalf("the server is not up on %s: %v", address, err)
	} else {
		_ = resp.Body.Close()
	}
	if got := processesOf(t, runID); len(got) == 0 {
		t.Fatal("no process of the run is left running after --keep, but the server should be")
	}

	// Files: mode 0600, and the operator's credential only in operator.env.
	opEnv, err := os.ReadFile(filepath.Join(s.root, "operator.env")) // #nosec G304 -- a path in this test's scratch or the repository
	must(t, err)
	token := regexp.MustCompile(`CONCH_TOKEN=(\S+)`).FindStringSubmatch(string(opEnv))
	if token == nil {
		t.Fatal("operator.env holds no CONCH_TOKEN")
	}
	for _, f := range []string{"tui-a.env", "tui-b.env", "tui-a2.env", "agent-a.env", "agent-b.env", "browser.txt", "operator.env"} {
		info, err := os.Stat(filepath.Join(s.root, f))
		must(t, err)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %o", f, info.Mode().Perm())
		}
		data, err := os.ReadFile(filepath.Join(s.root, f)) // #nosec G304 -- a path in this test's scratch or the repository
		must(t, err)
		if has := strings.Contains(string(data), token[1]); has != (f == "operator.env") {
			t.Errorf("%s: holds the operator's credential: %v", f, has)
		}
		if f != "operator.env" && f != "browser.txt" && !strings.Contains(string(data), "CONCH_CONFIG_DIR="+filepath.Join(s.root, "config", strings.TrimSuffix(f, ".env"))) {
			t.Errorf("%s does not set a CONCH_CONFIG_DIR of its own", f)
		}
	}
	// The credentials differ, one per participant (nick-test has two).
	seen := make(map[string]string)
	for _, f := range []string{"tui-a.env", "tui-b.env", "tui-a2.env", "agent-a.env", "agent-b.env"} {
		data, _ := os.ReadFile(filepath.Join(s.root, f)) // #nosec G304 -- a path in this test's scratch or the repository
		tok := regexp.MustCompile(`CONCH_TOKEN=(\S+)`).FindStringSubmatch(string(data))
		if tok == nil {
			t.Fatalf("%s holds no CONCH_TOKEN", f)
		}
		if other, dup := seen[tok[1]]; dup {
			t.Errorf("%s and %s share a credential", f, other)
		}
		seen[tok[1]] = f
	}

	// Two printed commands, run as printed: tui-a sends, agent-a runs.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	printed := printedLines(out)
	tuiA := commandFor(t, printed, "tui-a.env", "/bin/conch")
	if got, err := s.sh(ctx, tuiA+" send general 'sent by the test'"); err != nil {
		t.Fatalf("conch send as tui-a: %v\n%s", err, got)
	}
	tailed, err := s.sh(ctx, tuiA+" search general test")
	if err != nil || !strings.Contains(tailed, "sent by the test") {
		t.Fatalf("the message tui-a sent is not found: %v\n%s", err, tailed)
	}
	whoami, err := s.sh(ctx, commandFor(t, printed, "tui-b.env", "/bin/conch")+" whoami")
	if err != nil || !strings.Contains(whoami, "name: guest-test") {
		t.Fatalf("tui-b is not guest-test: %v\n%s", err, whoami)
	}
	agent := commandFor(t, printed, "agent-a.env", "/bin/conch-adapter")
	agentCtx, stopAgent := context.WithTimeout(ctx, 6*time.Second)
	agentOut, agentErr := s.sh(agentCtx, agent)
	stopAgent()
	if agentCtx.Err() == nil {
		t.Fatalf("agent-a exited by itself within 6s: %v\n%s", agentErr, agentOut)
	}
	if strings.Contains(agentOut, "required") || strings.Contains(agentOut, "unauthorized") {
		t.Fatalf("agent-a did not start:\n%s", agentOut)
	}
	// The adapter was stopped by the deadline, but its session reached the server.
	waitFor(t, 10*time.Second, "the adapter's lock file to go", func() bool {
		_, err := os.Stat(filepath.Join(s.root, "agent-a.lock"))
		return errors.Is(err, fs.ErrNotExist)
	})

	// Reset: nothing is left. Twice in a row is harmless, and the next run
	// starts clean (a new run id, a root without the old files).
	if got := s.mustRun("--reset"); !strings.Contains(got, "reset:") {
		t.Errorf("reset said: %s", got)
	}
	assertGone(t, s.root, runID)
	if got := s.mustRun("--reset"); !strings.Contains(got, "nothing to reset") {
		t.Errorf("a second reset said: %s", got)
	}
	second := s.mustRun("--keep")
	secondID := runIDOf(t, s.root)
	if secondID == runID {
		t.Error("the second run reused the first run's id")
	}
	if strings.Contains(second, "already exists") {
		t.Errorf("the second run did not start clean:\n%s", second)
	}
	if printedValue(t, second, "address", "") == address {
		// Possible by chance, but a new random port makes it unlikely enough to flag.
		t.Log("the second run got the same port as the first")
	}
	s.mustRun("--reset")
	assertGone(t, s.root, secondID)

	if after := snapshot(t, s.home); !sameSnapshot(before, after) {
		t.Errorf("HOME changed during the runs:\nbefore: %v\nafter: %v", before, after)
	}
	data, err := os.ReadFile(filepath.Join(sentinel, "credentials.json")) // #nosec G304 -- a path in this test's scratch or the repository
	if err != nil || string(data) != `{"sentinel":true}` {
		t.Errorf("the sentinel credentials changed: %v %q", err, data)
	}
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// printedLines are the lines of the runner's output that are not comments.
func printedLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines
}

// commandFor finds the printed command that sources envFile and ends in the
// program suffix, exactly, with nothing after it.
func commandFor(t *testing.T, lines []string, envFile, programSuffix string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, "set -a; . ") && strings.Contains(l, "/"+envFile+";") && strings.HasSuffix(l, programSuffix) {
			return l
		}
	}
	t.Fatalf("no printed command sources %s and ends in %s", envFile, programSuffix)
	return ""
}

// ---------------------------------------------------------------- a full run

// TestRunWithoutKeep covers the default mode, end to end: it provisions and
// reads back (the runner fails if that differs), checks each participant, tears
// down and leaves nothing. A root with a space in its path makes the printed
// and sourced quoting do real work.
func TestRunWithoutKeep(t *testing.T) {
	all := []string{"P3-01", "P3-02", "P3-03", "P3-04", "P3-05", "P3-06", "P3-07", "P3-08", "P3-09", "P3-10", "P3-11"}
	for _, tc := range []struct {
		name string
		root func(*scratch) string
		// rows limits the scenarios (CONCH_P3_ROWS); "" runs them all. The
		// root with a space in it only needs to show that every command the
		// suite builds is quoted, which P3-01 does by starting a terminal
		// client and two agents with them.
		rows string
		done []string
	}{
		{"the default root", func(s *scratch) string { return "" }, "", all},
		{"a root with a space in it", func(s *scratch) string { return filepath.Join(s.dir, "a b", "it's here") }, "P3-01", []string{"P3-01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			cmd := s.command()
			if root := tc.root(s); root != "" {
				cmd.Env = append(cmd.Env, "CONCH_P3_ROOT="+root)
				s.root = root
			}
			if tc.rows != "" {
				cmd.Env = append(cmd.Env, "CONCH_P3_ROWS="+tc.rows)
			}
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				t.Fatalf("p3-local: %v\n%s\n%s", err, out.String(), errb.String())
			}
			wants := []string{
				"read back through the API",
				"tui-a signed in as nick-test and sent a message",
				"tui-a2 signed in as nick-test and sent a message",
				"tui-b signed in as guest-test and sent a message",
				"browser-test sent a message",
				"agent-a replied in general",
				"observer-test is refused when it posts",
				"nothing left behind",
				"p3-local: PASS",
			}
			for _, id := range tc.done {
				wants = append(wants, "ok  "+id+" ")
			}
			if tc.rows == "" {
				wants = append(wants, "NOT RUN  P3-13: it runs the real Claude Code command as you; pass --real-adapter to run it")
			}
			for _, want := range wants {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the output lacks %q:\n%s", want, out.String())
				}
			}
			if _, err := os.Lstat(s.root); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the root is still there: %v", err)
			}
		})
	}
}

// ----------------------------------------------------------------- reset

func TestResetRefusesForeignDirectories(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *scratch)
	}{
		{"a directory without the marker", func(t *testing.T, s *scratch) {
			must(t, os.MkdirAll(s.root, 0o750))
			must(t, os.WriteFile(filepath.Join(s.root, "precious"), []byte("mine"), 0o600))
		}},
		{"a marker that is not ours", func(t *testing.T, s *scratch) {
			must(t, os.MkdirAll(s.root, 0o750))
			must(t, os.WriteFile(filepath.Join(s.root, markerName), []byte("someone else\n"), 0o600))
			must(t, os.WriteFile(filepath.Join(s.root, "precious"), []byte("mine"), 0o600))
		}},
		{"a file", func(t *testing.T, s *scratch) {
			must(t, os.WriteFile(s.root, []byte("mine"), 0o600))
		}},
		{"a link to a directory that has the marker", func(t *testing.T, s *scratch) {
			target := filepath.Join(s.dir, "target")
			must(t, os.MkdirAll(target, 0o750))
			must(t, os.WriteFile(filepath.Join(target, markerName), []byte(markerPrefix+"abc\n"), 0o600))
			must(t, os.WriteFile(filepath.Join(target, "precious"), []byte("mine"), 0o600))
			must(t, os.Symlink(target, s.root))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			tc.setup(t, s)
			stdout, stderr, err := s.run("--reset")
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() == 0 {
				t.Fatalf("reset did not fail: %v\n%s\n%s", err, stdout, stderr)
			}
			if !strings.Contains(stderr, "refusing to reset") {
				t.Errorf("reset did not say why: %s", stderr)
			}
			// Nothing was removed.
			if _, err := os.Lstat(s.root); err != nil {
				t.Errorf("the directory is gone: %v", err)
			}
			if tc.name != "a file" {
				if _, err := os.Stat(filepath.Join(s.root, "precious")); err != nil {
					t.Errorf("a file in it is gone: %v", err)
				}
			}
		})
	}
}

// TestResetThroughALinkedParent is a root whose parent directory is a symlink,
// as a linked ~/.local is: the processes carry the resolved path, and reset must
// still find and stop them before it removes their files.
func TestResetThroughALinkedParent(t *testing.T) {
	s := newScratch(t)
	resolved := filepath.Join(s.dir, "real")
	must(t, os.MkdirAll(resolved, 0o750))
	linked := filepath.Join(s.dir, "linked")
	must(t, os.Symlink(resolved, linked))
	root := filepath.Join(linked, "conch-p3")
	runWith := func(args ...string) (string, error) {
		cmd := s.command(args...)
		cmd.Env = append(cmd.Env, "CONCH_P3_ROOT="+root)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = runWith("--reset") })

	if out, err := runWith("--keep"); err != nil {
		t.Fatalf("p3-local --keep: %v\n%s", err, out)
	}
	runID := runIDOf(t, root)
	if got := processesOf(t, runID); len(got) == 0 {
		t.Fatal("no process of the run is left running after --keep, but the server should be")
	}
	if out, err := runWith("--reset"); err != nil {
		t.Fatalf("p3-local --reset: %v\n%s", err, out)
	}
	assertGone(t, root, runID)
}

func TestRunRefusesAnExistingRoot(t *testing.T) {
	s := newScratch(t)
	must(t, os.MkdirAll(s.root, 0o750))
	must(t, os.WriteFile(filepath.Join(s.root, "precious"), []byte("mine"), 0o600))
	_, stderr, err := s.run("--keep")
	if err == nil || !strings.Contains(stderr, "already exists") {
		t.Fatalf("a run on an existing root: %v\n%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(s.root, "precious")); err != nil {
		t.Errorf("the existing root was touched: %v", err)
	}
}

func TestFlagsAreChecked(t *testing.T) {
	s := newScratch(t)
	for _, args := range [][]string{{"--reset", "--keep"}, {"--reset", "--real-adapter"}, {"extra"}} {
		if _, _, err := s.run(args...); err == nil {
			t.Errorf("p3-local %v succeeded", args)
		}
	}
}

// ------------------------------------------------------------- interrupts

// TestInterrupt interrupts a run at each stage and checks that no process of it
// and no directory is left. CONCH_P3_HOLD makes the run wait at the stage, so
// the signal arrives exactly there; "a build in flight" has no hold and
// interrupts a real, cold `go build`.
func TestInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hold  string
		cold  bool
		alive bool // the server is running at the hold, so the test proves it was stopped
	}{
		{name: "during the build stage", hold: "build"},
		{name: "during provisioning", hold: "provision", alive: true},
		{name: "while running", hold: "check", alive: true},
		{name: "in the middle of go build", cold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			cmd := s.command()
			cmd.Env = s.env()
			if tc.hold != "" {
				cmd.Env = append(cmd.Env, "CONCH_P3_HOLD="+tc.hold)
			}
			if tc.cold {
				cmd.Env = append(cmd.Env, "GOCACHE="+filepath.Join(s.dir, "cold-go-cache"))
			}
			stdout, err := cmd.StdoutPipe()
			must(t, err)
			var errb bytes.Buffer
			cmd.Stderr = &errb
			must(t, cmd.Start())
			lines := make(chan string, 256)
			go func() {
				sc := bufio.NewScanner(stdout)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			want := "holding at " + tc.hold
			if tc.cold {
				want = "== build =="
			}
			waitLine(t, lines, want)
			if tc.cold {
				time.Sleep(1500 * time.Millisecond) // into the compile
			}
			runID := runIDOf(t, s.root)
			if tc.alive {
				if got := processesOf(t, runID); len(got) == 0 {
					t.Fatal("expected the server to be running at this stage")
				}
			}
			must(t, cmd.Process.Signal(os.Interrupt))
			done := make(chan error, 1)
			go func() {
				for range lines { // drain, so the program is never blocked on its output
				}
				done <- cmd.Wait()
			}()
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 130 {
					t.Errorf("exit: %v (want status 130)\n%s", err, errb.String())
				}
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the interrupted run did not exit within 30s")
			}
			assertGone(t, s.root, runID)
		})
	}
}

func waitLine(t *testing.T, lines <-chan string, substr string) {
	t.Helper()
	timeout := time.After(120 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("the output ended before %q", substr)
			}
			if strings.Contains(l, substr) {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %q", substr)
		}
	}
}

// ---------------------------------------------------------- --real-adapter

// TestRealAdapter checks what --real-adapter does and, as important, what it
// does not: the warning is printed, agent-a.env has the default command and no
// fixture mode, agent-b stays a fixture, and the program is started only by
// P3-13, only with the flag, and only when it is on PATH. A stand-in that
// records its use sits first on PATH; the real one is never run.
func TestRealAdapter(t *testing.T) {
	s := newScratch(t)
	bin := filepath.Join(s.dir, "fakebin")
	used := filepath.Join(s.dir, "claude-was-run")
	must(t, os.MkdirAll(bin, 0o750))
	must(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntouch "+used+"\nexit 1\n"), 0o700)) // #nosec G306 -- a test stand-in must be executable

	run := func(args ...string) (string, string) {
		cmd := s.command(args...)
		cmd.Env = append(s.env(), "PATH="+bin+":"+os.Getenv("PATH"))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("p3-local %v: %v\n%s\n%s", args, err, out.String(), errb.String())
		}
		return out.String(), errb.String()
	}

	out, errOut := run("--keep", "--real-adapter")
	if !strings.Contains(errOut, "WARNING: --real-adapter") || !strings.Contains(errOut, "--dangerously-skip-permissions") {
		t.Errorf("the warning is missing from standard error:\n%s", errOut)
	}
	a, err := os.ReadFile(filepath.Join(s.root, "agent-a.env")) // #nosec G304 -- a path in this test's scratch or the repository
	must(t, err)
	b, err := os.ReadFile(filepath.Join(s.root, "agent-b.env")) // #nosec G304 -- a path in this test's scratch or the repository
	must(t, err)
	for _, absent := range []string{"CONCH_ADAPTER_CMD", "CONCH_ADAPTER_FIXTURE", "CONCH_ADAPTER_RUNTIME", "CONCH_ADAPTER_MODEL"} {
		if strings.Contains(string(a), absent) {
			t.Errorf("agent-a.env sets %s; the real adapter runs its default", absent)
		}
	}
	for _, present := range []string{"CONCH_ADAPTER_CMD=", "CONCH_ADAPTER_FIXTURE=1"} {
		if !strings.Contains(string(b), present) {
			t.Errorf("agent-b.env lacks %s", present)
		}
	}
	if strings.Contains(out, "fixtures/approval.sh") {
		t.Error("the fixture variants of agent-a are printed with --real-adapter")
	}
	s.mustRun("--reset")

	// Without --keep the setup check skips agent-a and says so, and the suite
	// runs P3-13: one reply of the adapter's default command, which here is
	// the stand-in. It is started by that row and by nothing else.
	rows := func(env string, args ...string) (string, string) {
		cmd := s.command(args...)
		cmd.Env = append(s.env(), "PATH="+bin+":"+os.Getenv("PATH"), env)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("p3-local %v: %v\n%s\n%s", args, err, out.String(), errb.String())
		}
		return out.String(), errb.String()
	}

	// Without the flag the row is not run, with the program on PATH.
	out, errOut = rows("CONCH_P3_ROWS=P3-13")
	if !strings.Contains(out, "NOT RUN  P3-13") || !strings.Contains(out, "--real-adapter") || !strings.Contains(out, "p3-local: PASS") {
		t.Errorf("the suite without --real-adapter:\n%s\n%s", out, errOut)
	}
	if _, err := os.Stat(used); err == nil {
		t.Fatal("the real claude command was started without --real-adapter")
	}

	// With the flag it is: the stand-in fails, which the row reports as the
	// run's outcome, and nothing falls back to anything else.
	out, errOut = rows("CONCH_P3_ROWS=P3-13", "--real-adapter")
	if !strings.Contains(out, "NOT RUN  agent-a") || !strings.Contains(out, "p3-local: PASS") {
		t.Errorf("the check with --real-adapter:\n%s\n%s", out, errOut)
	}
	if !strings.Contains(out, `P3-13 runtime=claude-code model=sonnet outcome="exit 1" state=failed`) {
		t.Errorf("the row does not report what ran and how it ended:\n%s\n%s", out, errOut)
	}
	if _, err := os.Stat(used); err != nil {
		t.Errorf("the real adapter's command was not started by P3-13: %v", err)
	}
	_ = os.Remove(used)

	// With the flag and no program on PATH the row is skipped and the suite
	// passes.
	empty := filepath.Join(s.dir, "emptybin")
	must(t, os.MkdirAll(empty, 0o750))
	cmd := s.command("--real-adapter")
	cmd.Env = append(s.env(), "PATH="+empty+":"+pathWithout("claude"), "CONCH_P3_ROWS=P3-13")
	var cout, cerr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &cout, &cerr
	if err := cmd.Run(); err != nil {
		t.Fatalf("p3-local --real-adapter without claude: %v\n%s\n%s", err, cout.String(), cerr.String())
	}
	if !strings.Contains(cout.String(), "NOT RUN  P3-13: the claude program is not on PATH") || !strings.Contains(cout.String(), "p3-local: PASS") {
		t.Errorf("without the program on PATH:\n%s\n%s", cout.String(), cerr.String())
	}
	if _, err := os.Stat(used); err == nil {
		t.Fatal("the stand-in ran although it was not on PATH")
	}
}

// pathWithout is PATH without any directory that holds a program called name.
func pathWithout(name string) string {
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil { // #nosec G703 -- a directory of this process's own PATH
			continue
		}
		keep = append(keep, dir)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// ---------------------------------------------------------- the document

// TestWalkthroughCommandsArePrinted extracts the fenced commands of
// docs/p3-walkthrough.md and checks each is a line the runner prints, once the
// document's placeholders are filled in from the values the runner prints. It
// also checks the document has a section for each of the thirteen rows.
func TestWalkthroughCommandsArePrinted(t *testing.T) {
	s := newScratch(t)
	doc, err := os.ReadFile(filepath.Join(repoDir, "docs", "p3-walkthrough.md")) // #nosec G304 -- a path in this test's scratch or the repository
	must(t, err)

	for i := 1; i <= 13; i++ {
		heading := "## P3-" + pad2(i) + " "
		if !strings.Contains(string(doc), "\n"+heading) {
			t.Errorf("the walkthrough has no section %q", strings.TrimSpace(heading))
		}
	}

	out := s.mustRun("--keep")
	printed := make(map[string]bool)
	for _, l := range printedLines(out) {
		printed[l] = true
	}
	subst := strings.NewReplacer(
		"<root>", printedValue(t, out, "root", ""),
		"<address>", printedValue(t, out, "address", ""),
		"<webdir>", printedValue(t, out, "webdir", ""),
		"<pid:conchd>", printedValue(t, out, "pid", "conchd"),
	)
	placeholder := regexp.MustCompile(`<(id|credential):([a-z0-9-]+)>`)
	fill := func(line string) string {
		line = placeholder.ReplaceAllStringFunc(line, func(m string) string {
			p := placeholder.FindStringSubmatch(m)
			key := map[string]string{"id": "principal", "credential": "credential"}[p[1]]
			return printedValue(t, out, key, p[2])
		})
		return subst.Replace(line)
	}

	blocks := fencedBlocks(string(doc))
	if len(blocks) < 20 {
		t.Fatalf("found only %d fenced blocks in the walkthrough", len(blocks))
	}
	for _, block := range blocks {
		for _, line := range block {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if got := fill(line); !printed[got] {
				t.Errorf("the walkthrough has a command the runner does not print:\n  %s\n(after substitution)\n  %s", line, got)
			}
		}
	}
}

func pad2(i int) string {
	if i < 10 {
		return "0" + strconv.Itoa(i)
	}
	return strconv.Itoa(i)
}

// fencedBlocks returns the lines of every ```sh block.
func fencedBlocks(doc string) [][]string {
	var blocks [][]string
	var cur []string
	in := false
	for _, l := range strings.Split(doc, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case !in && t == "```sh":
			in, cur = true, nil
		case in && t == "```":
			in = false
			blocks = append(blocks, cur)
		case in:
			cur = append(cur, l)
		}
	}
	return blocks
}

// ------------------------------------------------------------ small pieces

func TestWebStale(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		dist      bool
		distAge   time.Duration // how long ago index.html was written
		srcAge    time.Duration
		want      bool
		noSrcFile bool
	}{
		{name: "no dist", dist: false, srcAge: time.Hour, want: true},
		{name: "dist newer than src", dist: true, distAge: time.Minute, srcAge: time.Hour, want: false},
		{name: "src newer than dist", dist: true, distAge: time.Hour, srcAge: time.Minute, want: true},
		{name: "no src files", dist: true, distAge: time.Hour, noSrcFile: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dist, src := filepath.Join(dir, "dist"), filepath.Join(dir, "src", "deep")
			must(t, os.MkdirAll(dist, 0o750))
			must(t, os.MkdirAll(src, 0o750))
			if tc.dist {
				p := filepath.Join(dist, "index.html")
				must(t, os.WriteFile(p, []byte("x"), 0o600))
				must(t, os.Chtimes(p, now.Add(-tc.distAge), now.Add(-tc.distAge)))
			}
			if !tc.noSrcFile {
				p := filepath.Join(src, "app.ts")
				must(t, os.WriteFile(p, []byte("x"), 0o600))
				must(t, os.Chtimes(p, now.Add(-tc.srcAge), now.Add(-tc.srcAge)))
			}
			got, err := webStale(dist, filepath.Join(dir, "src"))
			must(t, err)
			if got != tc.want {
				t.Errorf("webStale = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShellQuoting(t *testing.T) {
	for in, want := range map[string]string{
		"/home/n/.local/state/x": "/home/n/.local/state/x",
		"a b":                    "'a b'",
		"it's":                   `'it'\''s'`,
		"":                       "''",
		"$HOME":                  "'$HOME'",
	} {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want []string
		fails     bool
	}{
		{"same order", []string{"a", "b"}, []string{"a", "b"}, false},
		{"other order", []string{"b", "a"}, []string{"a", "b"}, false},
		{"missing", []string{"a"}, []string{"a", "b"}, true},
		{"extra", []string{"a", "b", "c"}, []string{"a", "b"}, true},
		{"both empty", nil, nil, false},
	} {
		if err := sameSet("x", tc.got, tc.want); (err != nil) != tc.fails {
			t.Errorf("%s: sameSet = %v, want failure %v", tc.name, err, tc.fails)
		}
	}
}

// TestCompareManifest shows that the read-back notices each way a stored
// manifest can differ from the one that was put.
func TestCompareManifest(t *testing.T) {
	channels := map[string]int64{chGeneral: 1, chPrivate: 2, chNets: 3}
	want := manifestFor("worker-test", channels)
	stored := func(mutate func(*schema.AgentManifestV1)) schema.AgentManifestV1 {
		m := schema.AgentManifestV1{DisplayName: want.DisplayName, Tier: want.Tier, Capabilities: append([]schema.Capability(nil), want.Capabilities...), Channels: append([]schema.ChannelGrant(nil), want.Channels...)}
		if mutate != nil {
			mutate(&m)
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		mutate func(*schema.AgentManifestV1)
		fails  bool
	}{
		{"identical", nil, false},
		{"a capability less", func(m *schema.AgentManifestV1) { m.Capabilities = m.Capabilities[1:] }, true},
		{"a channel less", func(m *schema.AgentManifestV1) { m.Channels = m.Channels[1:] }, true},
		{"a permission less", func(m *schema.AgentManifestV1) {
			m.Channels = append([]schema.ChannelGrant(nil), m.Channels...)
			m.Channels[0].Permissions = m.Channels[0].Permissions[:1]
		}, true},
		{"another tier", func(m *schema.AgentManifestV1) { m.Tier = schema.AgentTierH }, true},
		{"another name", func(m *schema.AgentManifestV1) { m.DisplayName = "x" }, true},
	} {
		if err := compareManifest("worker-test", stored(tc.mutate), want); (err != nil) != tc.fails {
			t.Errorf("%s: compareManifest = %v, want failure %v", tc.name, err, tc.fails)
		}
	}
}

// TestObserverIsRestricted pins the fixture the issue names: the observer's
// manifest has the two capabilities and the one channel, and the worker's has
// the six capabilities on all three channels.
func TestFixtureManifests(t *testing.T) {
	channels := map[string]int64{chGeneral: 1, chPrivate: 2, chNets: 3}
	obs := manifestFor("observer-test", channels)
	if got := capabilityNames(obs.Capabilities); strings.Join(got, ",") != "messages.read,sessions.manage" || len(obs.Channels) != 1 || obs.Channels[0].ChannelID != 1 {
		t.Errorf("observer-test manifest: %v %v", got, obs.Channels)
	}
	w := manifestFor("worker-test", channels)
	if len(w.Capabilities) != 6 || len(w.Channels) != 3 {
		t.Errorf("worker-test manifest: %v %v", w.Capabilities, w.Channels)
	}
	for _, g := range w.Channels {
		// read, post, and whisper, which answering a whisper takes.
		if len(g.Permissions) != 3 {
			t.Errorf("worker-test grant %v", g)
		}
	}
	if err := w.Validate(); err != nil {
		t.Error(err)
	}
	if err := obs.Validate(); err != nil {
		t.Error(err)
	}
}
