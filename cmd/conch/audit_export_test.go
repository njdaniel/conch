package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// `conch audit export` against a real conchd (its store, its auth, its
// listener): the command is what main runs, so cli.Run is the entry point
// here, as in main.
func TestAuditExportAgainstRealServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{AuthMode: server.AuthRequired, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test"}, st)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	url := "http://" + srv.Addr()

	boot, err := st.BootstrapOperatorWith(ctx, "root", store.BootstrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, aliceTok, err := st.CreateCredential(ctx, "system", alice.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	run := func(token string, args ...string) (string, error) {
		t.Setenv("CONCH_TOKEN", token)
		var out, errOut bytes.Buffer
		err := cli.Run(ctx, append([]string{"audit", "export", "--server", url}, args...), &out, &errOut, "test")
		return out.String(), err
	}
	parse := func(out string) []schema.AuditEventV1 {
		t.Helper()
		var events []schema.AuditEventV1
		if out == "" {
			return nil
		}
		for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			var e schema.AuditEventV1
			dec := json.NewDecoder(strings.NewReader(line))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&e); err != nil {
				t.Fatalf("line %d does not parse as AuditEventV1: %v: %q", i+1, err, line)
			}
			if err := e.Validate(); err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			events = append(events, e)
		}
		return events
	}

	out, err := run(boot.Token)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	events := parse(out)
	if len(events) < 3 {
		t.Fatalf("exported %d events, want the bootstrap and credential rows at least", len(events))
	}
	var actions []string
	for i, e := range events {
		if i > 0 && e.ID <= events[i-1].ID {
			t.Errorf("event %d does not follow %d", e.ID, events[i-1].ID)
		}
		actions = append(actions, e.Action)
	}
	if !strings.Contains(strings.Join(actions, ","), "operator_bootstrapped") {
		t.Errorf("actions = %v: no operator_bootstrapped", actions)
	}

	// The first export is on the trail, and a second one, resumed from the
	// last id received, carries it and nothing before.
	last := events[len(events)-1].ID
	out, err = run(boot.Token, "--after", strconv.FormatInt(last, 10), "--limit", "5")
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	next := parse(out)
	if len(next) != 1 {
		t.Fatalf("resumed export = %d events (%v), want exactly the first export's own row", len(next), next)
	}
	if next[0].Action != store.AuditExported || next[0].ID <= last {
		t.Errorf("resumed export starts with %+v, want audit_exported after %d", next[0], last)
	}

	// A member is refused: one error line, nonzero, nothing on stdout.
	out, err = run(aliceTok)
	if err == nil {
		t.Fatal("a non-operator's export succeeded")
	}
	if out != "" {
		t.Errorf("a refused export wrote to stdout: %q", out)
	}
	if strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Errorf("refusal is not one line: %q", err)
	}
}
