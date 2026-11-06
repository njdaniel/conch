//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type buildResult struct {
	out string
	err error
}

func TestBuildCLIWith(t *testing.T) {
	vcsOut := "error obtaining VCS status: exit status 128\n\tUse -buildvcs=false to disable VCS stamping.\n"
	boom := errors.New("exit status 1")
	tests := []struct {
		name      string
		results   []buildResult
		wantErr   bool
		wantCalls int
		wantIn    []string
	}{
		{"builds", []buildResult{{"", nil}}, false, 1, nil},
		{"other failure has no diagnosis", []buildResult{{"undefined: x\n", boom}}, true, 1, []string{"undefined: x"}},
		{"vcs failure is not retried, and carries the diagnosis", []buildResult{{vcsOut, boom}}, true, 1,
			[]string{"exit status 128", "issue #886", "DIAG-OUTPUT"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			run := func(string, ...string) (string, error) {
				r := tc.results[calls]
				calls++
				return r.out, r.err
			}
			out, err := buildCLIWith("bin", run, func() string { return "DIAG-OUTPUT\n" })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Errorf("build ran %d times, want %d", calls, tc.wantCalls)
			}
			if !strings.Contains(strings.Join(tc.wantIn, " "), "DIAG-OUTPUT") && strings.Contains(out, "DIAG-OUTPUT") {
				t.Errorf("a non-VCS failure got the VCS diagnosis:\n%s", out)
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
		})
	}
}

func TestGoVCSRoot(t *testing.T) {
	tmp := t.TempDir()
	// Relative paths keep the upward search inside this fixture. Absolute paths
	// would also inspect shared temporary-directory ancestors, where another
	// process can briefly create a .git directory.
	t.Chdir(tmp)
	mk := func(rel string, file bool) {
		p := filepath.Join(tmp, rel)
		if file {
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	mk("main/.git", false)
	mk("main/pkg/sub", false)
	mk("main/.claude/worktrees/wt/.git", true) // linked worktree nested in a checkout
	mk("main/.claude/worktrees/wt/cmd/conch", false)
	mk("loose/wt/.git", true) // linked worktree outside any checkout
	mk("loose/wt/cmd", false)
	tests := []struct{ name, dir, want string }{
		{"plain checkout", "main/pkg/sub", "main"},
		{"nested linked worktree: go uses the enclosing checkout", "main/.claude/worktrees/wt/cmd/conch", "main"},
		{"linked worktree outside any checkout: go finds nothing", "loose/wt/cmd", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := goVCSRoot(tc.dir); got != tc.want {
				t.Errorf("goVCSRoot = %q, want %q", got, tc.want)
			}
		})
	}
}
