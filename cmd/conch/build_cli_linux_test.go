//go:build linux

package main

import (
	"os"
	"os/exec"
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
	out, err := exec.Command(name, args...).CombinedOutput() // #nosec G204 -- fixed arguments
	return string(out), err
}

// vcsDiagnostics runs the commands go build runs for VCS stamping, from the
// package directory, and returns their combined output (stderr included).
func vcsDiagnostics() string {
	var b strings.Builder
	wd, _ := os.Getwd()
	b.WriteString("cwd: " + wd + "\n")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	for _, args := range [][]string{
		{"rev-parse", "--git-dir", "--git-common-dir", "--show-toplevel"},
		{"status", "--porcelain"},
		{"-c", "log.showsignature=false", "log", "-1", "--format=%H:%ct"},
	} {
		out, err := runCmd("git", args...)
		b.WriteString("$ git " + strings.Join(args, " ") + "\n" + out)
		if err != nil {
			b.WriteString("-> " + err.Error() + "\n")
		}
	}
	return b.String()
}
