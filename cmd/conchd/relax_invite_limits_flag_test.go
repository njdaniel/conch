package main

import "testing"

// --test-relax-invite-limits is a flag and nothing else: off by default, and
// not turned on by any environment variable (issue #609).
func TestParseServeArgsRelaxInviteLimits(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{name: "default is off"},
		{name: "flag turns it on", args: []string{"--test-relax-invite-limits"}, want: true},
		{name: "explicitly off", args: []string{"--test-relax-invite-limits=false"}},
		{name: "no environment variable turns it on", env: map[string]string{"CONCHD_TEST_RELAX_INVITE_LIMITS": "1", "CONCHD_RELAX_INVITE_LIMITS": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if err != nil {
				t.Fatal(err)
			}
			if opts.relaxInviteLimits != tt.want {
				t.Errorf("relaxInviteLimits = %v, want %v", opts.relaxInviteLimits, tt.want)
			}
		})
	}
}
