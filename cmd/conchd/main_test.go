package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestRunVersion(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("run version: %v", err)
	}
}

func TestRunNoCommand(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("run with no command: want error, got nil")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if err := run([]string{"frobnicate"}); err == nil {
		t.Fatal("run unknown command: want error, got nil")
	}
}

func TestRunServeRequiresData(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	if err := runServe([]string{"--listen", "127.0.0.1:0"}); err == nil {
		t.Fatal("runServe without --data: want error, got nil")
	}
}

func TestRunServeRejectsInvalidAuthMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
	}{
		{"flag", []string{"--data", "ignored", "--auth", "maybe"}, ""},
		{"empty flag", []string{"--data", "ignored", "--auth="}, ""},
		{"env", []string{"--data", "ignored"}, "sometimes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_DATA", "")
			t.Setenv("CONCHD_AUTH", tt.env)
			err := runServe(append([]string{"--listen", "127.0.0.1:0"}, tt.args...))
			if err == nil || !strings.Contains(err.Error(), "auth") {
				t.Fatalf("err = %v, want an auth mode error", err)
			}
		})
	}
}

func bootstrap(t *testing.T, dir, name string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = runBootstrapOperator([]string{"--data", dir, "--name", name}, &out, &errOut)
	return out.String(), errOut.String(), err
}

// dbCounts returns the audit event, credential and principal counts.
func dbCounts(t *testing.T, dir string) [3]int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	events, err := st.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	creds, principals := 0, 0
	for id := int64(1); id < 20; id++ {
		if _, err := st.PrincipalByID(ctx, id); err != nil {
			continue
		}
		principals++
		c, err := st.ListCredentials(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		creds += len(c)
	}
	return [3]int{len(events), creds, principals}
}

func TestBootstrapOperator(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data") // does not exist yet

	stdout, stderr, err := bootstrap(t, dir, "nick")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	token := strings.TrimSpace(stdout)
	if !strings.HasPrefix(token, schema.CredentialTokenPrefix) || len(token) != schema.CredentialTokenLength || strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout is not exactly one token line")
	}
	if !strings.Contains(stderr, "will not be shown again") || strings.Contains(stderr, token) {
		t.Errorf("stderr = %q, want a one-line notice without the token", stderr)
	}

	// The printed token authenticates against a server on the same data dir.
	st, err := store.Open(context.Background(), filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{AuthMode: server.AuthRequired, Version: "t"}, st)
	ts := httptest.NewServer(srv.Handler())
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var who schema.WhoAmIResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami status %d, err %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if who.Name != "nick" || who.Role != schema.RoleOperator || who.Kind != schema.PrincipalHuman {
		t.Errorf("whoami = %+v", who)
	}
	ts.Close()
	_ = st.Close()

	// A second run is refused and changes nothing.
	before := dbCounts(t, dir)
	for _, name := range []string{"other", "nick"} {
		stdout, _, err := bootstrap(t, dir, name)
		if err == nil || !strings.Contains(err.Error(), "operator already exists") {
			t.Errorf("second bootstrap (%s) err = %v", name, err)
		}
		if stdout != "" {
			t.Errorf("a refused bootstrap printed to stdout: %q", stdout)
		}
	}
	if after := dbCounts(t, dir); after != before {
		t.Errorf("database changed by refused runs: %v -> %v", before, after)
	}
}

func TestBootstrapOperatorNameCollision(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePrincipal(context.Background(), store.PrincipalAgent, "taken"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	before := dbCounts(t, dir)

	stdout, _, err := bootstrap(t, dir, "taken")
	if err == nil || !strings.Contains(err.Error(), "already exists") || stdout != "" {
		t.Errorf("err = %v, stdout = %q", err, stdout)
	}
	if after := dbCounts(t, dir); after != before {
		t.Errorf("database changed: %v -> %v", before, after)
	}
}

func TestBootstrapOperatorUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no data", nil},
		{"no name", []string{"--data", "x"}},
		{"blank name", []string{"--data", "x", "--name", "  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_DATA", "")
			var out, errOut bytes.Buffer
			if err := runBootstrapOperator(tt.args, &out, &errOut); err == nil || out.Len() != 0 {
				t.Errorf("err = %v, stdout = %q", err, out.String())
			}
		})
	}
}
