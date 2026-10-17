package voicehelper

import (
	"errors"
	"slices"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestHelperEnv(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/user", "XDG_CONFIG_HOME=/xdg",
		"CONCH_TOKEN=parent", "CONCH_SERVER=https://other.example.org", "CONCH_CONFIG_DIR=/parent",
		"CONCH_VOICE_HELPER=/elsewhere", "CONCH_LATER=x", "RUST_LOG=trace", "RUST_BACKTRACE=1", "RUST_LIB_BACKTRACE=full",
		"conch_lower=kept", "NOEQUALS", "=hidden", "RUST_LOGGING=kept",
	}
	kept := []string{"PATH=/usr/bin", "HOME=/home/user", "XDG_CONFIG_HOME=/xdg", "conch_lower=kept", "RUST_LOGGING=kept"}
	tests := []struct {
		name    string
		profile Profile
		added   []string
	}{
		{name: "stored, with a directory", profile: Profile{ConfigDir: "/cfg/a", Credential: schema.VoiceControlCredentialStored, Token: "never"},
			added: []string{"CONCH_CONFIG_DIR=/cfg/a"}},
		{name: "env, with a directory", profile: Profile{ConfigDir: "/cfg/b", Credential: schema.VoiceControlCredentialEnv, Token: "tok"},
			added: []string{"CONCH_CONFIG_DIR=/cfg/b", "CONCH_TOKEN=tok"}},
		{name: "stored, no directory: the helper resolves the same none", profile: Profile{Credential: schema.VoiceControlCredentialStored}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := helperEnv(parent, tt.profile)
			want := append(slices.Clone(kept), tt.added...)
			if !slices.Equal(got, want) {
				t.Errorf("helperEnv = %q\nwant %q", got, want)
			}
		})
	}
}

func TestProfileCheck(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
		want    error
		ok      bool
	}{
		{name: "stored", profile: Profile{Credential: schema.VoiceControlCredentialStored, ConfigDir: "/cfg"}, ok: true},
		{name: "stored with no directory", profile: Profile{Credential: schema.VoiceControlCredentialStored}, ok: true},
		{name: "env", profile: Profile{Credential: schema.VoiceControlCredentialEnv, Token: "tok"}, ok: true},
		{name: "none", profile: Profile{}, want: ErrNoCredential},
		{name: "env without a token", profile: Profile{Credential: schema.VoiceControlCredentialEnv}, want: ErrNoCredential},
		{name: "a token with a NUL", profile: Profile{Credential: schema.VoiceControlCredentialEnv, Token: "a\x00b"}, want: ErrNoCredential},
		{name: "a relative directory", profile: Profile{Credential: schema.VoiceControlCredentialStored, ConfigDir: "cfg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.profile.check()
			switch {
			case tt.ok && err != nil:
				t.Fatalf("check: %v", err)
			case !tt.ok && err == nil:
				t.Fatal("check accepted it")
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Fatalf("check: %v, want %v", err, tt.want)
			}
		})
	}
}
