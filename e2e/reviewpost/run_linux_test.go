package reviewpost

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runWorld is everything `run` touches: a repository whose origin is a bare
// repository on disk, a conchd to post to, and stand-ins for claude and gh.
type runWorld struct {
	*postWorld
	// origin is the bare repository; repo is the checkout the script is
	// pointed at (CONCH_REPO).
	origin string
	repo   string
}

// The stand-in session: it does what the real one is asked to do, which is
// write the report at the path named after its branch and nothing else. It
// also records where it ran, what it was given, and whether the lock shared
// with conch-agent was held while it ran.
const sessionWritesReport = `set -eu
printf 'claude\n' >>"$CALLS"
pwd >"$SESSION_DIR/cwd"
for arg in "$@"; do printf '%s\n' "$arg"; done >"$SESSION_DIR/args"
if flock -n "$CONCH_AGENT_LOG_DIR/agent.lock" true; then
    echo free >"$SESSION_DIR/lock"
else
    echo held >"$SESSION_DIR/lock"
fi
branch=$(git branch --show-current)
mkdir -p docs/reports
cp "$FIXTURE" "docs/reports/${branch#principal-review/}-principal-review.md"
echo "the session's own output"
`

func newRunWorld(t *testing.T) *runWorld {
	t.Helper()
	w := &runWorld{postWorld: newPostWorld(t)}
	base := t.TempDir()
	w.origin = filepath.Join(base, "origin.git")
	w.repo = filepath.Join(base, "repo")
	seed := filepath.Join(base, "seed")

	w.git(base, "init", "--quiet", "--bare", "--initial-branch=main", w.origin)
	// A push is seen from the receiving side, so its place in the order of
	// events is recorded by the same file the stubs write to.
	hook := "#!/bin/sh\nwhile read -r old new ref; do printf 'push %s\\n' \"$ref\" >>" + shellQuote(w.env.calls) + "; done\n"
	if err := os.WriteFile(filepath.Join(w.origin, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil { // #nosec G306 -- a git hook must be executable; private test directory
		t.Fatal(err)
	}
	w.git(base, "init", "--quiet", "--initial-branch=main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "a repository to review\n")
	w.git(seed, "add", "README.md")
	w.git(seed, "commit", "--quiet", "-m", "seed")
	w.git(seed, "push", "--quiet", w.origin, "main")
	w.git(base, "clone", "--quiet", w.origin, w.repo)
	// The seed's push is not part of any run.
	if err := os.Remove(w.env.calls); err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(base, "session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.env.vars["CONCH_REPO"] = w.repo
	w.env.vars["SESSION_DIR"] = sessionDir
	w.env.vars["FIXTURE"] = filepath.Join(repoRoot, "e2e", "reviewpost", "testdata", "hostile.md")
	w.env.stub("claude", sessionWritesReport)
	w.env.stub("gh", `printf 'gh %s %s\n' "$1" "$2" >>"$CALLS"
for arg in "$@"; do printf '%s\n' "$arg"; done >"$SESSION_DIR/gh-args"
echo "Creating pull request for the branch into main in njdaniel/conch" >&2
echo "`+prURL+`"
`)
	return w
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// git runs git in dir with the same isolated configuration the script gets.
func (w *runWorld) git(dir string, args ...string) string {
	w.env.t.Helper()
	cmd := exec.Command("git", args...) // #nosec G204 -- git with this test's own arguments
	cmd.Dir = dir
	cmd.Env = w.env.environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.env.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *runWorld) session(name string) string {
	return strings.TrimSpace(readFile(w.env.t, filepath.Join(w.env.vars["SESSION_DIR"], name)))
}

// reviewBranches lists the principal-review branches origin has.
func (w *runWorld) reviewBranches() []string {
	out := w.git(w.origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/principal-review/")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// assertTidy checks what every run must leave behind in the checkout: no
// worktree but the main one, and no local review branch.
func (w *runWorld) assertTidy(t *testing.T) {
	t.Helper()
	if n := strings.Count(w.git(w.repo, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("the checkout has %d worktrees, want only its own", n)
	}
	if out := w.git(w.repo, "branch", "--list", "principal-review/*"); out != "" {
		t.Errorf("local review branches left behind: %s", out)
	}
	entries, err := os.ReadDir(filepath.Join(w.env.logDir, "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("worktree directory left behind: %s", entry.Name())
	}
}

// The success path: lock, worktree and branch, session, then push, PR and
// post in that order, with the summary arriving in the channel.
func TestRunPublishesTheReview(t *testing.T) {
	w := newRunWorld(t)
	w.env.vars["CONCH_REVIEW_MODEL"] = "a-model-for-the-test"
	w.env.vars["CONCH_REVIEW_TIMEOUT"] = "120"
	mainBefore := w.git(w.origin, "rev-parse", "main")

	res := w.env.run(w.cwd, "run")
	if res.code != 0 {
		t.Fatalf("run exited %d\nstderr: %s\nlog: %s", res.code, res.stderr, w.env.logs())
	}

	branches := w.reviewBranches()
	if len(branches) != 1 {
		t.Fatalf("origin has review branches %v, want one", branches)
	}
	branch := branches[0]
	date := strings.TrimPrefix(branch, "principal-review/")
	report := "docs/reports/" + date + "-principal-review.md"

	wantCalls := []string{"claude", "push refs/heads/" + branch, "gh pr create", "conch whoami", "conch send"}
	if got := w.env.callLog(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls = %q, want %q", got, wantCalls)
	}

	// The session ran in the review's own worktree, under the shared lock,
	// with the model and the skill it was told to use.
	if cwd := w.session("cwd"); cwd != filepath.Join(w.env.logDir, "worktrees", "principal-review-"+date) {
		t.Errorf("the session ran in %s", cwd)
	}
	if lock := w.session("lock"); lock != "held" {
		t.Errorf("conch-agent's lock was %s while the session ran, want held", lock)
	}
	args := w.session("args")
	for _, want := range []string{"a-model-for-the-test", ".claude/skills/principal-review/SKILL.md", report, branch} {
		if !strings.Contains(args, want) {
			t.Errorf("the session was not given %q:\n%s", want, args)
		}
	}

	// The branch adds the report and nothing else, and main did not move.
	if changed := w.git(w.origin, "diff", "--name-only", "main", branch); changed != report {
		t.Errorf("the branch changes %q, want only %s", changed, report)
	}
	if got := w.git(w.origin, "show", branch+":"+report); got != strings.TrimSpace(readFile(t, w.env.vars["FIXTURE"])) {
		t.Errorf("the pushed report is not the one the session wrote")
	}
	if after := w.git(w.origin, "rev-parse", "main"); after != mainBefore {
		t.Errorf("main moved from %s to %s", mainBefore, after)
	}

	ghArgs := "\n" + w.session("gh-args") + "\n"
	for _, want := range []string{"\n--head\n" + branch + "\n", "\n--base\nmain\n", "\nRefs #26\n"} {
		if !strings.Contains(ghArgs, want) {
			t.Errorf("gh pr create was not given %q:\n%s", want, ghArgs)
		}
	}

	want := strings.TrimSuffix(readFile(t, filepath.Join("testdata", "hostile.summary")), "\n")
	if got := w.srv.bodies(reviewChannel); len(got) != 1 || got[0] != want {
		t.Errorf("channel holds %q, want the summary with its PR link", got)
	}
	if !strings.Contains(w.env.logs(), "the session's own output") {
		t.Errorf("the session's output is not in the log")
	}
	w.assertTidy(t)
	w.assertNothingRan(t)
	w.env.assertNoSecret(w.token, res)

	// A second run on the same day finds the report on the pushed branch
	// and does nothing.
	again := w.env.run(w.cwd, "run")
	if again.code != 0 {
		t.Fatalf("second run exited %d: %s", again.code, again.stderr)
	}
	if got := w.env.callLog(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("the second run did something: calls = %q", got)
	}
	if !strings.Contains(again.stderr, "already exists on origin/"+branch) {
		t.Errorf("the second run did not say why it did nothing: %s", again.stderr)
	}
	if got := w.srv.bodies(reviewChannel); len(got) != 1 {
		t.Errorf("the second run posted: channel holds %d messages", len(got))
	}
}

// A report for today that is already on main also means there is nothing to
// do. Tomorrow's is added too, so the test holds across midnight.
func TestRunDoesNothingWhenTheReportIsOnMain(t *testing.T) {
	w := newRunWorld(t)
	for _, day := range []time.Time{time.Now(), time.Now().Add(24 * time.Hour)} {
		writeFile(t, filepath.Join(w.repo, "docs", "reports", day.Format("2006-01-02")+"-principal-review.md"), "# done by hand\n")
	}
	w.git(w.repo, "add", "docs")
	w.git(w.repo, "commit", "--quiet", "-m", "reports")
	w.git(w.repo, "push", "--quiet", "origin", "main")
	wantCalls := w.env.callLog() // the push just made

	res := w.env.run(w.cwd, "run")
	if res.code != 0 {
		t.Fatalf("run exited %d: %s", res.code, res.stderr)
	}
	if got := w.env.callLog(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls = %q, want none beyond %q", got, wantCalls)
	}
	if !strings.Contains(res.stderr, "already exists on origin/main") {
		t.Errorf("run did not say why it did nothing: %s", res.stderr)
	}
	if got := w.srv.bodies(reviewChannel); len(got) != 0 {
		t.Errorf("channel holds %q, want nothing", got)
	}
	w.assertTidy(t)
}

// The session failing, in the ways it can: no push, no PR, nothing posted,
// the worktree gone, a nonzero exit and the reason in the log.
func TestRunWhenTheSessionFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		timeout string
		want    string
	}{
		{
			name:    "it exits nonzero",
			session: "printf 'claude\\n' >>\"$CALLS\"\necho 'the session gave up' >&2\nexit 7\n",
			want:    "session failed (exit status 7)",
		},
		{
			name:    "it runs past the timeout",
			session: "printf 'claude\\n' >>\"$CALLS\"\nexec sleep 60\n",
			timeout: "1",
			want:    "session timed out after 1s",
		},
		{
			name:    "it exits 0 without a report",
			session: "printf 'claude\\n' >>\"$CALLS\"\n",
			want:    "session wrote no docs/reports/",
		},
		{
			name: "it commits more than the report",
			session: sessionWritesReport + `echo drive-by >>README.md
git add -A
git commit --quiet -m "the report, and a change nobody asked for"
`,
			want: "the branch changes more than the report",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRunWorld(t)
			w.env.stub("claude", tc.session)
			if tc.timeout != "" {
				w.env.vars["CONCH_REVIEW_TIMEOUT"] = tc.timeout
			}
			res := w.env.run(w.cwd, "run")
			if res.code != 1 {
				t.Errorf("exit status %d, want 1", res.code)
			}
			if got, want := w.env.callLog(), []string{"claude"}; !reflect.DeepEqual(got, want) {
				t.Errorf("calls = %q, want only the session", got)
			}
			if !strings.Contains(w.env.logs(), tc.want) {
				t.Errorf("the log does not give the reason %q:\n%s", tc.want, w.env.logs())
			}
			if branches := w.reviewBranches(); len(branches) != 0 {
				t.Errorf("origin has %v, want no review branch", branches)
			}
			if got := w.srv.bodies(reviewChannel); len(got) != 0 {
				t.Errorf("channel holds %q, want nothing", got)
			}
			w.assertTidy(t)
		})
	}
}

// What can go wrong after a good session. The review must not be lost: until
// the push it is kept as a file, after it the branch stays.
func TestRunWhenPublishingFails(t *testing.T) {
	t.Run("the push is refused", func(t *testing.T) {
		w := newRunWorld(t)
		if err := os.WriteFile(filepath.Join(w.origin, "hooks", "pre-receive"), []byte("#!/bin/sh\necho 'refused by the test' >&2\nexit 1\n"), 0o700); err != nil { // #nosec G306 -- a git hook must be executable; private test directory
			t.Fatal(err)
		}
		res := w.env.run(w.cwd, "run")
		if res.code != 1 {
			t.Errorf("exit status %d, want 1", res.code)
		}
		if got, want := w.env.callLog(), []string{"claude"}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %q, want only the session", got)
		}
		if !strings.Contains(w.env.logs(), "could not push principal-review/") {
			t.Errorf("the log does not give the reason:\n%s", w.env.logs())
		}
		kept, err := filepath.Glob(filepath.Join(w.env.logDir, "*-principal-review.md"))
		if err != nil || len(kept) != 1 {
			t.Fatalf("kept reports = %v (%v), want one", kept, err)
		}
		if readFile(t, kept[0]) != readFile(t, w.env.vars["FIXTURE"]) {
			t.Errorf("the kept report is not the one the session wrote")
		}
		if got := w.srv.bodies(reviewChannel); len(got) != 0 {
			t.Errorf("channel holds %q, want nothing", got)
		}
		w.assertTidy(t)
	})

	t.Run("the PR cannot be opened", func(t *testing.T) {
		w := newRunWorld(t)
		w.env.stub("gh", "printf 'gh %s %s\\n' \"$1\" \"$2\" >>\"$CALLS\"\necho 'gh: not authenticated' >&2\nexit 4\n")
		res := w.env.run(w.cwd, "run")
		if res.code != 1 {
			t.Errorf("exit status %d, want 1", res.code)
		}
		branches := w.reviewBranches()
		if len(branches) != 1 {
			t.Fatalf("origin has review branches %v, want the pushed one to stay", branches)
		}
		wantCalls := []string{"claude", "push refs/heads/" + branches[0], "gh pr create"}
		if got := w.env.callLog(); !reflect.DeepEqual(got, wantCalls) {
			t.Errorf("calls = %q, want %q", got, wantCalls)
		}
		if logs := w.env.logs(); !strings.Contains(logs, "could not open its PR") || !strings.Contains(logs, "gh: not authenticated") {
			t.Errorf("the log does not give the reason:\n%s", logs)
		}
		if got := w.srv.bodies(reviewChannel); len(got) != 0 {
			t.Errorf("channel holds %q, want nothing", got)
		}
		w.assertTidy(t)
	})

	// Conch being down does not lose the review: the branch and the PR are
	// there, the reason is logged, and the exit status is the one for a
	// failed post.
	t.Run("the post fails", func(t *testing.T) {
		w := newRunWorld(t)
		w.srv.stop()
		res := w.env.run(w.cwd, "run")
		w.srv.start()
		if res.code != exitNotPosted {
			t.Errorf("exit status %d, want %d", res.code, exitNotPosted)
		}
		branches := w.reviewBranches()
		if len(branches) != 1 {
			t.Fatalf("origin has review branches %v, want one", branches)
		}
		wantCalls := []string{"claude", "push refs/heads/" + branches[0], "gh pr create", "conch whoami"}
		if got := w.env.callLog(); !reflect.DeepEqual(got, wantCalls) {
			t.Errorf("calls = %q, want %q", got, wantCalls)
		}
		if logs := w.env.logs(); !strings.Contains(logs, "not posted: conchd unreachable at ") || !strings.Contains(logs, prURL) {
			t.Errorf("the log does not give the reason and the PR:\n%s", logs)
		}
		if got := w.srv.bodies(reviewChannel); len(got) != 0 {
			t.Errorf("channel holds %q, want nothing", got)
		}
		w.assertTidy(t)
	})
}

// The lock is conch-agent's. While something else holds it the review does
// not start; it waits as long as it is told to, and runs when the lock is
// released within that time.
func TestRunSharesTheAgentLock(t *testing.T) {
	hold := func(t *testing.T, w *runWorld) *os.File {
		t.Helper()
		if err := os.MkdirAll(w.env.agentLogDir, 0o700); err != nil {
			t.Fatal(err)
		}
		lock, err := os.OpenFile(filepath.Join(w.env.agentLogDir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Close() })
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits an int
			t.Fatal(err)
		}
		return lock
	}

	t.Run("held past the wait", func(t *testing.T) {
		w := newRunWorld(t)
		hold(t, w)
		w.env.vars["CONCH_REVIEW_LOCK_WAIT"] = "1"
		res := w.env.run(w.cwd, "run")
		if res.code != 1 {
			t.Errorf("exit status %d, want 1", res.code)
		}
		if got := w.env.callLog(); len(got) != 0 {
			t.Errorf("calls = %q, want none", got)
		}
		if !strings.Contains(w.env.logs(), "another run still holds") {
			t.Errorf("the log does not give the reason:\n%s", w.env.logs())
		}
		if got := w.srv.bodies(reviewChannel); len(got) != 0 {
			t.Errorf("channel holds %q, want nothing", got)
		}
	})

	t.Run("released within the wait", func(t *testing.T) {
		w := newRunWorld(t)
		lock := hold(t, w)
		w.env.vars["CONCH_REVIEW_LOCK_WAIT"] = "60"
		released := make(chan time.Time, 1)
		go func() {
			time.Sleep(500 * time.Millisecond)
			released <- time.Now()
			_ = lock.Close()
		}()
		res := w.env.run(w.cwd, "run")
		if res.code != 0 {
			t.Fatalf("run exited %d: %s", res.code, res.stderr)
		}
		select {
		case <-released:
		default:
			t.Fatalf("the run finished while the lock was still held")
		}
		if got := w.env.callLog(); len(got) != 5 || got[0] != "claude" {
			t.Errorf("calls = %q, want a full run", got)
		}
		if got := w.srv.bodies(reviewChannel); len(got) != 1 {
			t.Errorf("channel holds %d messages, want the summary", len(got))
		}
	})
}
