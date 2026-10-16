package main

import (
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
)

func TestParseServeArgsTrustedProxies(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    string // the parsed prefixes, comma-joined
		wantErr bool
	}{
		{"default trusts nobody", nil, "", "", false},
		{"flag", []string{"--trusted-proxies", "127.0.0.1/32,::1/128"}, "", "127.0.0.1/32,::1/128", false},
		{"env", nil, "10.0.0.0/8", "10.0.0.0/8", false},
		{"flag beats env", []string{"--trusted-proxies=192.168.1.0/24"}, "10.0.0.0/8", "192.168.1.0/24", false},
		{"masked", []string{"--trusted-proxies", "10.1.2.3/8"}, "", "10.0.0.0/8", false},
		{"spaces and empty entries", []string{"--trusted-proxies", " 127.0.0.1/32 , ,::1/128"}, "", "127.0.0.1/32,::1/128", false},
		{"bare address", []string{"--trusted-proxies", "127.0.0.1"}, "", "", true},
		{"garbage", []string{"--trusted-proxies", "loopback"}, "", "", true},
		{"mapped prefix", []string{"--trusted-proxies", "::ffff:127.0.0.1/128"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_TRUSTED_PROXIES", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			var got []string
			for _, p := range opts.trustedProxies {
				got = append(got, p.String())
			}
			if strings.Join(got, ",") != tt.want {
				t.Errorf("trustedProxies = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestParseServeArgsInviteAllowance(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		want    int
		wantErr bool
	}{
		{"default", nil, "", store.DefaultPersonalInviteAllowance, false},
		{"flag", []string{"--invite-allowance", "2"}, "", 2, false},
		{"env", nil, "9", 9, false},
		{"flag beats env", []string{"--invite-allowance=3"}, "9", 3, false},
		{"zero", []string{"--invite-allowance", "0"}, "", 0, true},
		{"negative env", nil, "-1", 0, true},
		{"garbage env", nil, "five", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_INVITE_ALLOWANCE", tt.env)
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && opts.inviteAllowance != tt.want {
				t.Errorf("inviteAllowance = %d, want %d", opts.inviteAllowance, tt.want)
			}
		})
	}
}
