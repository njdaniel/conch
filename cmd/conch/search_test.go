package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// `conch search` against a real conchd (its store with the FTS index, its auth,
// its listener): what it prints is what the caller could list, nothing more.
func TestSearchAgainstRealServer(t *testing.T) {
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

	if _, err := st.BootstrapOperatorWith(ctx, "root", store.BootstrapOptions{}); err != nil {
		t.Fatal(err)
	}
	ch, err := st.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	type person struct {
		store.Principal
		token string
	}
	mk := func(name string, member bool) person {
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if member {
			if _, err := st.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		return person{p, tok}
	}
	ann, wes, olga, nora := mk("ann", true), mk("wes", true), mk("olga", true), mk("nora", false)
	if _, err := st.InsertMessage(ctx, ch.ID, ann.ID, "Deploy the red falcon"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertScopedMessage(ctx, store.ScopedPost{
		ChannelID: ch.ID, AuthorID: ann.ID, Body: "falcon whisper for wes only",
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{wes.ID}}.Normalize(ann.ID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(ctx, ch.ID, wes.ID, "[whisper:1] falcon lookalike"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(ctx, ch.ID, wes.ID, "something else"); err != nil {
		t.Fatal(err)
	}

	run := func(who person, args ...string) (string, string, error) {
		t.Setenv("CONCH_TOKEN", who.token)
		var out, errOut bytes.Buffer
		err := cli.Run(ctx, append([]string{"search", "--server", url}, args...), &out, &errOut, "test")
		return out.String(), errOut.String(), err
	}
	lines := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}

	// A recipient finds the whisper, newest first, marked as tail marks it.
	out, _, err := run(ann, "ops", "FALCON")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := lines(out)
	if len(got) != 3 || !strings.Contains(got[0], `\[whisper:1] falcon lookalike`) ||
		!strings.Contains(got[1], "[whisper:") || !strings.Contains(got[1], "falcon whisper for wes only") ||
		!strings.HasSuffix(got[2], "Deploy the red falcon") {
		t.Errorf("ann's matches = %q", got)
	}

	// Words are all required, in any order.
	out, _, err = run(wes, "ops", "falcon", "red")
	if err != nil || len(lines(out)) != 1 || !strings.HasSuffix(lines(out)[0], "Deploy the red falcon") {
		t.Errorf("two words: %q, %v", out, err)
	}

	// No match: no output, exit 0.
	out, errOut, err := run(wes, "ops", "zebra")
	if err != nil || out != "" || errOut != "" {
		t.Errorf("no match: out %q, stderr %q, err %v", out, errOut, err)
	}

	// --limit pages, and stderr names the cursor.
	out, errOut, err = run(wes, "--limit", "1", "ops", "falcon")
	if err != nil || len(lines(out)) != 1 || !strings.Contains(errOut, "--before ") {
		t.Errorf("limit 1: out %q, stderr %q, err %v", out, errOut, err)
	}

	// Operators in the words are words: nothing in the data contains them.
	out, _, err = run(wes, "ops", "*", "OR", "falcon")
	if err != nil || out != "" {
		t.Errorf("operators as words: out %q, err %v", out, err)
	}

	// A member who is not a recipient is shown the two channel-wide matches
	// and nothing that says a whisper exists.
	out, errOut, err = run(olga, "ops", "falcon")
	if got := lines(out); err != nil || len(got) != 2 || strings.Contains(out, "whisper for wes") || errOut != "" {
		t.Errorf("outsider: out %q, stderr %q, err %v", out, errOut, err)
	}
	out, errOut, err = run(olga, "--limit", "2", "ops", "falcon")
	if err != nil || len(lines(out)) != 2 || errOut != "" {
		t.Errorf("outsider on a full page: out %q, stderr %q: a hidden message must not produce a cursor (err %v)", out, errOut, err)
	}

	// A non-member gets the unknown-channel answer, on stderr as an error and
	// nothing on stdout.
	out, _, err = run(nora, "ops", "falcon")
	if err == nil || out != "" || !strings.Contains(err.Error(), "channel not found") {
		t.Errorf("non-member: out %q, err %v", out, err)
	}
}
