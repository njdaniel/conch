//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Hostile characters are written by number: an escape typed into source can
// be turned into the character itself by an editor or a tool.
var hostileText = "x" + string(rune(0x1b)) + "[2J" + string(rune(0x1b)) + "]0;pwned" + string(rune(0x07)) +
	string(rune(0x85)) + string(rune(0x202e)) + string(rune(0x2066)) + string(rune(0x2028)) + string(rune(0x7f)) + "y"

// hostileEscaped is hostileText as the plain CLI prints it. It is spelt with
// a backslash and digits, never with a backslash-u escape in source, which a
// tool can turn into the character itself.
var hostileEscaped = `\x1b[2J\x1b]0;pwned\a` + bsl + "u0085" + bsl + "u202e" + bsl + "u2066" + bsl + "u2028" + `\x7fy`

const bsl = string(rune(0x5c))

// unsafeIn names the first thing in s that a terminal would act on, or "".
func unsafeIn(s string) string {
	for _, r := range s {
		switch {
		case r == '\n':
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return string(rune('?')) + " (a control, bidi control or separator)"
		}
	}
	return ""
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A message body with terminal control sequences in it is free text and stays
// stored as sent, but `conch tail` and `conch search` against a real server
// print it as visible escapes and no raw control (issue #234).
func TestTailAndSearchEscapeHostileBodies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{AuthMode: server.AuthOff, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test"}, st)
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
	t.Setenv("CONCH_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := st.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	author, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	body := "falcon " + hostileText

	post := func() {
		t.Helper()
		request, err := json.Marshal(schema.PostMessageRequest{AuthorID: author.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(url+"/v0/channels/general/messages", "application/json", bytes.NewReader(request))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("the server refused the body: %d", resp.StatusCode)
		}
	}
	post() // for search

	// tail: start it, then post, and read what it printed.
	tailCtx, stopTail := context.WithCancel(ctx)
	var tailOut, tailErr lockedBuffer
	tailDone := make(chan error, 1)
	go func() {
		tailDone <- cli.Run(tailCtx, []string{"tail", "--server", url, "general"}, &tailOut, &tailErr, "test")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(tailOut.String(), "pwned") && time.Now().Before(deadline) {
		post()
		time.Sleep(100 * time.Millisecond)
	}
	stopTail()
	<-tailDone

	var searchOut, searchErr bytes.Buffer
	if err := cli.Run(ctx, []string{"search", "--server", url, "general", "falcon"}, &searchOut, &searchErr, "test"); err != nil {
		t.Fatalf("search: %v", err)
	}

	for name, out := range map[string]string{"tail": tailOut.String(), "search": searchOut.String()} {
		if !strings.Contains(out, "falcon x") || !strings.Contains(out, hostileEscaped) {
			t.Errorf("%s printed %q, want the body as visible escapes", name, out)
		}
		if bad := unsafeIn(out); bad != "" {
			t.Errorf("%s printed %s: %q", name, bad, out)
		}
	}
}

// The error line main prints is made safe in one place: a server that puts
// terminal sequences in its error message cannot reach the terminal of anyone
// whose command fails, whichever command it was.
func TestMainErrorLineEscapesServerText(t *testing.T) {
	hostileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(schema.Error{Code: "denied", Message: hostileText})
	}))
	t.Cleanup(hostileServer.Close)

	bin := conchBinary(t)
	home := t.TempDir()
	for _, args := range [][]string{
		{"whoami", "--server", hostileServer.URL},
		{"approvals", "list", "--server", hostileServer.URL},
		{"nets", "list", "--server", hostileServer.URL, "general"},
		{"search", "--server", hostileServer.URL, "general", "x"},
	} {
		cmd := exec.Command(bin, args...) // #nosec G204 -- the binary this test just built
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "CONCH_TOKEN=tok"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			t.Errorf("%v: succeeded against a server that refuses everything", args)
			continue
		}
		if bad := unsafeIn(stdout.String() + stderr.String()); bad != "" {
			t.Errorf("%v printed %s: %q", args, bad, stderr.String())
		}
		if !strings.Contains(stderr.String(), "x"+hostileEscaped) {
			t.Errorf("%v: stderr = %q, want the server's message as visible escapes", args, stderr.String())
		}
	}
}
