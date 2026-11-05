//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// vcsStatusMarker is what `go build` prints when the version-control command
// it runs to stamp the binary fails. Go discards that command's stderr, so
// the message alone ("exit status 128") says nothing about why.
const vcsStatusMarker = "error obtaining VCS status"

// buildCLI builds the conch command into bin. VCS stamping stays on: the
// binary under test is the one users get, version metadata included.
//
// A VCS-status failure (issue #886: seen once, in a linked worktree, with
// other builds and processes running; its cause is not known) is returned
// with a diagnosis: the git commands go runs, rerun with their stderr. It is
// not retried, so that the next occurrence shows the cause instead of being
// hidden. Any other failure is returned as is.
func buildCLI(bin string) (string, error) {
	return buildCLIWith(bin, runCmd, vcsDiagnostics)
}

func buildCLIWith(bin string, run func(name string, args ...string) (string, error), diagnose func() string) (string, error) {
	out, err := run("go", "build", "-o", bin, ".")
	if err == nil || !strings.Contains(out, vcsStatusMarker) {
		return out, err
	}
	return out + "diagnostics after the failure (issue #886):\n" + diagnose(), err
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput() // #nosec G204 -- every caller passes a literal command
	return string(out), err
}

// goVCSRoot returns the directory go build runs its VCS commands in for a
// package in dir: the nearest ancestor holding a ".git" DIRECTORY. cmd/go
// (1.25) ignores a ".git" file, which is what a linked worktree has, so in a
// worktree nested inside another checkout (".claude/worktrees/<name>") it
// stamps from, and runs git in, the enclosing checkout, not the worktree; in
// a worktree outside any checkout it finds nothing and does not stamp. The
// diagnosis has to look where go looked. It returns "" when go finds nothing.
func goVCSRoot(dir string) string {
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// vcsDiagnostics runs the commands go build runs for VCS stamping, in the
// directory go runs them in (goVCSRoot), and returns their combined output
// (stderr included). It also runs rev-parse in the package directory, which
// differs from go's directory in a linked worktree.
func vcsDiagnostics() string {
	var b strings.Builder
	wd, _ := os.Getwd()
	root := goVCSRoot(wd)
	b.WriteString("cwd: " + wd + "\ngo's VCS root (nearest ancestor with a .git directory): " + root + "\n")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	b.WriteString(gitIn(wd, "rev-parse", "--git-dir", "--git-common-dir", "--show-toplevel"))
	if root == "" {
		return b.String()
	}
	b.WriteString(gitIn(root, "status", "--porcelain"))
	b.WriteString(gitIn(root, "-c", "log.showsignature=false", "log", "-1", "--format=%H:%ct"))
	return b.String()
}

func gitIn(dir string, args ...string) string {
	out, err := runCmd("git", append([]string{"-C", dir}, args...)...)
	s := "$ git -C " + dir + " " + strings.Join(args, " ") + "\n" + out
	if err != nil {
		s += "-> " + err.Error() + "\n"
	}
	return s
}
