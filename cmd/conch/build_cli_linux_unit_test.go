//go:build linux

package main

import (
	"errors"
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
