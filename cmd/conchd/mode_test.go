package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// CONCHD_MODE (issue #866, ADR-011 item 1): exactly temporary and normal,
// lower case; unset and empty are normal, so every existing server resolves
// as it did; anything else stops serve with an error that names the
// variable.
func TestParseServeArgsMode(t *testing.T) {
	t.Setenv("CONCHD_AUTH", "")
	t.Setenv("CONCHD_TOTP_KEY", "")
	t.Setenv("CONCHD_OPERATOR_TOTP", "")
	tests := []struct {
		name  string
		env   *string
		want  store.Mode
		wants string // a part of the error; "" means none
	}{
		{"unset", nil, store.ModeNormal, ""},
		{"empty", ptr(""), store.ModeNormal, ""},
		{"normal", ptr("normal"), store.ModeNormal, ""},
		{"temporary", ptr("temporary"), store.ModeTemporary, ""},
		{"upper case", ptr("TEMPORARY"), "", "CONCHD_MODE"},
		{"capitalized", ptr("Temporary"), "", "CONCHD_MODE"},
		{"padded", ptr("temporary "), "", "CONCHD_MODE"},
		{"something else", ptr("ephemeral"), "", "CONCHD_MODE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_MODE", "")
			if tt.env == nil {
				if err := os.Unsetenv("CONCHD_MODE"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("CONCHD_MODE", *tt.env)
			}
			opts, err := parseServeArgs([]string{"--data", t.TempDir()})
			if tt.wants != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wants) {
					t.Fatalf("parseServeArgs = %v, want an error naming %s", err, tt.wants)
				}
				return
			}
			if err != nil || opts.mode != tt.want {
				t.Fatalf("parseServeArgs = %q, %v; want %q", opts.mode, err, tt.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// CONCHD_OPERATOR_TOTP (issue #875, ADR-012 items 1 and 2): exactly optional
// and required; unset or empty is required on a normal server and optional on
// a temporary one; required with temporary, and anything else, stops serve
// with an error that names the variable. What the two allow follows.
func TestParseServeArgsOperatorTOTP(t *testing.T) {
	t.Setenv("CONCHD_AUTH", "")
	t.Setenv("CONCHD_TOTP_KEY", "")
	tests := []struct {
		name         string
		mode, totp   *string
		want         store.OperatorTOTP
		passwordOnly bool
		wants        string // a part of the error; "" means none
	}{
		{"both unset", nil, nil, store.OperatorTOTPRequired, false, ""},
		{"normal, empty", ptr("normal"), ptr(""), store.OperatorTOTPRequired, false, ""},
		{"normal, required", nil, ptr("required"), store.OperatorTOTPRequired, false, ""},
		{"normal, optional", nil, ptr("optional"), store.OperatorTOTPOptional, true, ""},
		{"temporary, unset", ptr("temporary"), nil, store.OperatorTOTPOptional, true, ""},
		{"temporary, empty", ptr("temporary"), ptr(""), store.OperatorTOTPOptional, true, ""},
		{"temporary, optional", ptr("temporary"), ptr("optional"), store.OperatorTOTPOptional, true, ""},
		{"temporary, required", ptr("temporary"), ptr("required"), "", false, "CONCHD_OPERATOR_TOTP"},
		{"upper case", nil, ptr("OPTIONAL"), "", false, "CONCHD_OPERATOR_TOTP"},
		{"padded", nil, ptr("optional "), "", false, "CONCHD_OPERATOR_TOTP"},
		{"something else", nil, ptr("off"), "", false, "CONCHD_OPERATOR_TOTP"},
	}
	set := func(t *testing.T, key string, v *string) {
		t.Setenv(key, "")
		if v == nil {
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Setenv(key, *v)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set(t, "CONCHD_MODE", tt.mode)
			set(t, "CONCHD_OPERATOR_TOTP", tt.totp)
			opts, err := parseServeArgs([]string{"--data", t.TempDir()})
			if tt.wants != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wants) {
					t.Fatalf("parseServeArgs = %v, want an error naming %s", err, tt.wants)
				}
				return
			}
			if err != nil || opts.operatorTOTP != tt.want {
				t.Fatalf("parseServeArgs = %q, %v; want %q", opts.operatorTOTP, err, tt.want)
			}
			if got := store.PasswordOnlyAllowed(opts.mode, opts.operatorTOTP); got != tt.passwordOnly {
				t.Errorf("password-only allowed = %v, want %v", got, tt.passwordOnly)
			}
		})
	}
}

// An unknown mode or operator TOTP setting, and temporary with required,
// stop serve before anything is opened or made.
func TestRunServeUnknownModeFailsBeforeDatabase(t *testing.T) {
	for _, tt := range []struct{ mode, totp, wants string }{
		{"ephemeral", "", "CONCHD_MODE"},
		{"", "off", "CONCHD_OPERATOR_TOTP"},
		{"temporary", "required", "CONCHD_OPERATOR_TOTP"},
	} {
		t.Run(tt.wants+"="+tt.mode+tt.totp, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			t.Setenv("CONCHD_TOTP_KEY", "")
			t.Setenv("CONCHD_MODE", tt.mode)
			t.Setenv("CONCHD_OPERATOR_TOTP", tt.totp)
			err := runServe([]string{"--data", dir, "--listen", "127.0.0.1:0"})
			if err == nil || !strings.Contains(err.Error(), tt.wants) {
				t.Fatalf("err = %v, want one naming %s", err, tt.wants)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("data dir was created (stat err %v); serve should fail before opening anything", statErr)
			}
		})
	}
}

// --link (ADR-011 item 3) prints https://<origin>/app/#/setup/<token> once,
// alone on stdout, in place of the token, for bootstrap and setup; the token
// in it works, and is on stdout only.
func TestOperatorSetupLink(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	ctx := context.Background()
	dir := t.TempDir()
	linkToken := func(t *testing.T, stdout, stderr, origin string) string {
		t.Helper()
		line := strings.TrimSuffix(stdout, "\n")
		prefix := origin + "/app/#/setup/"
		tok, ok := strings.CutPrefix(line, prefix)
		if strings.Count(stdout, "\n") != 1 || !ok || schema.ValidateSetupToken(tok) != nil {
			t.Fatalf("stdout must be the setup link alone, got %q", stdout)
		}
		if strings.Contains(stderr, tok) || !strings.Contains(stderr, "setup link") || !strings.Contains(stderr, "will not be shown again") {
			t.Errorf("stderr %q", stderr)
		}
		return tok
	}
	out, errOut, err := runOp(t, "bootstrap", "--data", dir, "--link", "--origin", "https://conch.example/", "root")
	if err != nil {
		t.Fatal(err)
	}
	tok := linkToken(t, out, errOut, "https://conch.example")
	st := openData(t, dir)
	got, err := st.RedeemSetupToken(ctx, store.HashSetupToken(tok), time.Now())
	if err != nil || got.Principal.Name != "root" {
		t.Fatalf("redeem the link's token = %+v, %v", got, err)
	}
	id := fmt.Sprint(got.Principal.ID)
	out, errOut, err = runOp(t, "setup", id, "--data", dir, "--origin", "https://conch.example:8443", "--link")
	if err != nil {
		t.Fatal(err)
	}
	tok = linkToken(t, out, errOut, "https://conch.example:8443")
	if _, err := st.RedeemSetupToken(ctx, store.HashSetupToken(tok), time.Now()); err != nil {
		t.Errorf("redeem the setup link's token = %v", err)
	}
}

// --link needs --origin, an HTTPS origin and nothing more, and recover does
// not take it. Each refusal prints nothing on stdout and changes nothing:
// recover with --link revokes nothing.
func TestOperatorSetupLinkRefusals(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	ctx := context.Background()
	dir := t.TempDir()
	// A script credential, so a recover that ran would show.
	if err := runBootstrapOperator([]string{"--data", dir, "--name", "root"}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	st := openData(t, dir)
	creds, err := st.ListCredentials(ctx, 1)
	if err != nil || len(creds) != 1 {
		t.Fatalf("the bootstrap's credentials = %v, %v", creds, err)
	}
	before, err := st.ListAuditEvents(ctx, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"--link with no origin", []string{"setup", "--data", dir, "--link", "1"}, "--link needs --origin"},
		{"--origin with no link", []string{"setup", "--data", dir, "--origin", "https://conch.example", "1"}, "only for --link"},
		{"plain http", []string{"setup", "--data", dir, "--link", "--origin", "http://conch.example", "1"}, "https://"},
		{"a path", []string{"setup", "--data", dir, "--link", "--origin", "https://conch.example/app", "1"}, "nothing more"},
		{"a query", []string{"setup", "--data", dir, "--link", "--origin", "https://conch.example?x=1", "1"}, "nothing more"},
		{"a fragment", []string{"setup", "--data", dir, "--link", "--origin", "https://conch.example#x", "1"}, "nothing more"},
		{"a user", []string{"setup", "--data", dir, "--link", "--origin", "https://me@conch.example", "1"}, "nothing more"},
		{"no host", []string{"setup", "--data", dir, "--link", "--origin", "https://", "1"}, "no host"},
		{"not an address", []string{"setup", "--data", dir, "--link", "--origin", "conch.example", "1"}, "https://"},
		{"recover --link", []string{"recover", "--data", dir, "--link", "--origin", "https://conch.example", "1"}, "not offered for recover"},
		{"recover --origin", []string{"recover", "--data", dir, "--origin", "https://conch.example", "1"}, "not offered for recover"},
		{"bootstrap --link with no origin", []string{"bootstrap", "--data", t.TempDir(), "--link", "other"}, "--link needs --origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := runOp(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v, want one containing %q", err, tt.want)
			}
			if out != "" {
				t.Errorf("a refusal printed %q on stdout", out)
			}
		})
	}
	after, err := st.ListAuditEvents(ctx, 0, 100000)
	if err != nil || len(after) != len(before) {
		t.Errorf("the refusals wrote %d audit rows (%v)", len(after)-len(before), err)
	}
	if creds, err := st.ListCredentials(ctx, 1); err != nil || len(creds) != 1 || creds[0].RevokedAt != nil {
		t.Errorf("the operator's credential after the refusals = %+v, %v; want it live", creds, err)
	}
}
