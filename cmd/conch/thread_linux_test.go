package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// `conch send --reply-to`, `conch thread` and `conch tail` against a real
// conchd (issue #25): a reply is posted, read back as a thread and seen live,
// it has its root's audience, and a message the caller cannot see answers like
// one that does not exist.
func TestThreadsAgainstRealServer(t *testing.T) {
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

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
	mk := func(name string) person {
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		return person{p, tok}
	}
	ann, wes, olga := mk("ann"), mk("wes"), mk("olga")

	run := func(who person, args ...string) (string, error) {
		t.Setenv("CONCH_TOKEN", who.token)
		var out, errOut bytes.Buffer
		err := cli.Run(ctx, append(args[:1:1], append([]string{"--server", url}, args[1:]...)...), &out, &errOut, "test")
		return out.String(), err
	}
	lines := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}

	root, err := st.InsertMessage(ctx, ch.ID, ann.ID, "deploy at noon?")
	if err != nil {
		t.Fatal(err)
	}
	rootID := strconv.FormatInt(root.ID, 10)
	whisper, _, err := st.InsertScopedMessage(ctx, store.ScopedPost{
		ChannelID: ch.ID, AuthorID: ann.ID, Body: "between us",
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{wes.ID}}.Normalize(ann.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	whisperID := strconv.FormatInt(whisper.ID, 10)

	// tail, started first: it must show the reply live, marked with its root.
	tailCtx, stopTail := context.WithCancel(ctx)
	var tailOut, tailErr lockedBuffer
	tailDone := make(chan error, 1)
	t.Setenv("CONCH_TOKEN", olga.token)
	go func() {
		tailDone <- cli.Run(tailCtx, []string{"tail", "--server", url, "ops"}, &tailOut, &tailErr, "test")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for tailOut.String() == "" && time.Now().Before(deadline) {
		// Through the server, so that it is broadcast: a message written to the
		// store reaches no socket.
		if _, err := run(ann, "send", "ops", "ping"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if tailOut.String() == "" {
		t.Fatal("tail never printed anything")
	}

	// send --reply-to posts a reply.
	if _, err := run(wes, "send", "--reply-to", rootID, "ops", "noon works"); err != nil {
		t.Fatalf("send --reply-to: %v", err)
	}
	for !strings.Contains(tailOut.String(), "noon works") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stopTail()
	if err := <-tailDone; err != nil {
		t.Logf("tail ended with %v; stderr %q", err, tailErr.String())
	}
	var replyLine string
	for _, l := range lines(tailOut.String()) {
		if strings.Contains(l, "noon works") {
			replyLine = l
		}
	}
	if !strings.Contains(replyLine, "[reply:"+rootID+"] noon works") {
		t.Errorf("tail printed the reply as %q, want it marked [reply:%s]", replyLine, rootID)
	}

	// thread: the root first, then the reply.
	out, err := run(olga, "thread", "ops", rootID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	got := lines(out)
	if len(got) != 2 || !strings.HasSuffix(got[0], " deploy at noon?") || strings.Contains(got[0], "[reply:") ||
		!strings.Contains(got[1], "[reply:"+rootID+"] noon works") {
		t.Errorf("thread = %q", got)
	}

	// A reply to a whisper reaches the whisper's two people and nobody else.
	if _, err := run(wes, "send", "--reply-to", whisperID, "ops", "got it"); err != nil {
		t.Fatalf("reply to a whisper: %v", err)
	}
	for _, who := range []person{ann, wes} {
		out, err = run(who, "thread", "ops", whisperID)
		got = lines(out)
		if err != nil || len(got) != 2 || !strings.Contains(got[0], "[whisper:") || !strings.Contains(got[1], "[reply:"+whisperID+"] got it") {
			t.Errorf("thread of the whisper for %d = %q (%v)", who.ID, got, err)
		}
	}
	// olga is outside the whisper: her answers are the answers for an id that
	// does not exist, and nothing of the whisper reaches her.
	_, errReply := run(olga, "send", "--reply-to", whisperID, "ops", "can I join?")
	_, errMissing := run(olga, "send", "--reply-to", "9999", "ops", "can I join?")
	if errReply == nil || errMissing == nil || errReply.Error() != errMissing.Error() ||
		!strings.Contains(errReply.Error(), "thread not found") {
		t.Errorf("reply to a hidden whisper = %v, to a missing message = %v; want the same error", errReply, errMissing)
	}
	out, errThread := run(olga, "thread", "ops", whisperID)
	_, errThreadMissing := run(olga, "thread", "ops", "9999")
	if out != "" || errThread == nil || errThreadMissing == nil || errThread.Error() != errThreadMissing.Error() {
		t.Errorf("thread of a hidden whisper: out %q, err %v; missing: %v", out, errThread, errThreadMissing)
	}
}

// --reply-to cannot be combined with --net or --to, and a bad id is refused
// before any request leaves the machine.
func TestSendReplyToFlagErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CONCH_TOKEN", "tok")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"with --net", []string{"send", "--server", "http://127.0.0.1:1", "--reply-to", "3", "--net", "alpha", "ops", "x"}, "--reply-to cannot be combined with --net or --to"},
		{"with --to", []string{"send", "--server", "http://127.0.0.1:1", "--reply-to", "3", "--to", "4", "ops", "x"}, "--reply-to cannot be combined with --net or --to"},
		{"with an empty --to", []string{"send", "--server", "http://127.0.0.1:1", "--reply-to", "3", "--to", "", "ops", "x"}, "--reply-to cannot be combined with --net or --to"},
		{"zero", []string{"send", "--server", "http://127.0.0.1:1", "--reply-to", "0", "ops", "x"}, "--reply-to must be a positive message id"},
		{"negative", []string{"send", "--server", "http://127.0.0.1:1", "--reply-to", "-2", "ops", "x"}, "--reply-to must be a positive message id"},
		{"thread needs a number", []string{"thread", "--server", "http://127.0.0.1:1", "ops", "abc"}, "<id> must be a positive message id"},
		{"thread zero", []string{"thread", "--server", "http://127.0.0.1:1", "ops", "0"}, "<id> must be a positive message id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := cli.Run(context.Background(), tt.args, &out, &errOut, "test")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", out.String())
			}
		})
	}
}
