package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// runOp runs `conchd operator <args>` and returns stdout, stderr and the
// error.
func runOp(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runOperator(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// setupTokenOf checks that stdout is a setup token alone and returns it.
func setupTokenOf(t *testing.T, stdout string) string {
	t.Helper()
	tok := strings.TrimSuffix(stdout, "\n")
	if strings.Count(stdout, "\n") != 1 || schema.ValidateSetupToken(tok) != nil {
		t.Fatalf("stdout must be the setup token alone, got %q", stdout)
	}
	return tok
}

func openData(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// bootstrap makes the first operator with a setup token and redeems
// nothing; it returns the token.
func TestOperatorBootstrap(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	dir := t.TempDir()
	out, errOut, err := runOp(t, "bootstrap", "--data", dir, "root")
	if err != nil {
		t.Fatal(err)
	}
	tok := setupTokenOf(t, out)
	if strings.Contains(errOut, tok) || !strings.Contains(errOut, "will not be shown again") || !strings.Contains(errOut, "/v1/setup/redeem") {
		t.Errorf("stderr %q", errOut)
	}
	st := openData(t, dir)
	got, err := st.RedeemSetupToken(context.Background(), store.HashSetupToken(tok), time.Now())
	if err != nil || got.Principal.Name != "root" || got.Principal.Role != store.RoleOperator || got.PasswordSet {
		t.Errorf("redeem = %+v, %v", got, err)
	}
	// Flags may come after the name, and a second bootstrap is refused.
	if _, _, err := runOp(t, "bootstrap", "other", "--data", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second bootstrap = %v", err)
	}
}

// setup gives an operator made by bootstrap-operator a token and leaves its
// script credential working; once setup is complete it is refused, and
// recover revokes everything and makes a token that works.
func TestOperatorSetupAndRecover(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	ctx := context.Background()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if err := runBootstrapOperator([]string{"--data", dir, "--name", "root"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	script := strings.TrimSpace(out.String())
	st := openData(t, dir)
	root, err := st.ResolveCredential(ctx, script)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(root.ID)

	stdout, hint, err := runOp(t, "setup", "--data", dir, id)
	if err != nil {
		t.Fatal(err)
	}
	setupTokenOf(t, stdout)
	if !strings.Contains(hint, "conchd operator recover") {
		t.Errorf("setup's stderr does not point at recover for a spent token: %q", hint)
	}
	if _, err := st.ResolveCredential(ctx, script); err != nil {
		t.Errorf("setup revoked the script credential: %v", err)
	}

	// Complete the setup underneath, as the setup flow would.
	if _, err := st.SetPassword(ctx, "system", root.ID, "$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$x", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "conch.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "INSERT INTO totp (principal_id, secret, enrolled_at) VALUES (?, x'01', 1)", root.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runOp(t, "setup", "--data", dir, id); err == nil || !strings.Contains(err.Error(), "operator recover") {
		t.Errorf("setup on a complete operator = %v, want a refusal naming recover", err)
	}

	stdout, stderr, err := runOp(t, "recover", id, "--data", dir)
	if err != nil {
		t.Fatal(err)
	}
	tok := setupTokenOf(t, stdout)
	if !strings.Contains(stderr, "recovered") || strings.Contains(stderr, tok) {
		t.Errorf("stderr %q", stderr)
	}
	if _, err := st.ResolveCredential(ctx, script); err == nil {
		t.Error("the script credential survived recovery")
	}
	if _, err := st.RedeemSetupToken(ctx, store.HashSetupToken(tok), time.Now()); err != nil {
		t.Errorf("the recovery's token = %v", err)
	}
}

// The commands refuse what they cannot do, and change nothing.
func TestOperatorCommandRefusals(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	dir := t.TempDir()
	if _, _, err := runOp(t, "bootstrap", "--data", dir, "root"); err != nil {
		t.Fatal(err)
	}
	st := openData(t, dir)
	member, err := st.CreatePrincipal(context.Background(), store.PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "no command"},
		{"an unknown command", []string{"reset"}, "unknown command"},
		{"setup with no id", []string{"setup", "--data", dir}, "principal id"},
		{"setup with two ids", []string{"setup", "--data", dir, "1", "2"}, "principal id"},
		{"setup with a name", []string{"setup", "--data", dir, "root"}, "not a principal id"},
		{"recover nobody", []string{"recover", "--data", dir, "9999"}, "no principal has id"},
		{"recover a member", []string{"recover", "--data", dir, fmt.Sprint(member.ID)}, "not an operator"},
		{"setup with no data", []string{"setup", "1"}, "--data"},
		{"recover a database that is not there", []string{"recover", "--data", filepath.Join(dir, "nope"), "1"}, "no database"},
		{"bootstrap with no name", []string{"bootstrap", "--data", dir}, "name"},
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
}

// CONCHD_TOTP_KEY is read from the environment only: a good key reaches the
// server's config, none is allowed, and anything else stops serve with an
// error that does not show the value.
func TestParseServeArgsTOTPKey(t *testing.T) {
	t.Setenv("CONCHD_AUTH", "")
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	tests := []struct {
		name, env string
		ok, set   bool
	}{
		{"unset", "", true, false},
		{"a key", good, true, true},
		{"a short one", base64.StdEncoding.EncodeToString([]byte("too short a key")), false, false},
		{"not base64", "correct-horse-battery-staple-key-value", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_TOTP_KEY", tt.env)
			_, err := parseServeArgs([]string{"--data", t.TempDir()})
			if (err == nil) != tt.ok {
				t.Fatalf("parseServeArgs = %v, want ok %v", err, tt.ok)
			}
			if err != nil {
				if strings.Contains(err.Error(), tt.env) || !strings.Contains(err.Error(), "CONCHD_TOTP_KEY") {
					t.Errorf("error %q must name the variable and not the value", err)
				}
				return
			}
			// The resolved options never hold the key; runServe reads it
			// straight into the server's Config.
			k, err := totpKeyFromEnv()
			if err != nil || (k != nil) != tt.set {
				t.Errorf("totpKeyFromEnv = %v, %v; want set %v", k, err, tt.set)
			}
		})
	}
}
