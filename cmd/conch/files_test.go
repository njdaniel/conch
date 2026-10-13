package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// `conch upload` then `conch download` against a real conchd: the bytes round
// trip, and the people who cannot see the message cannot fetch the file.
func TestUploadThenDownloadAgainstRealServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dataDir := t.TempDir()
	srv := server.New(server.Config{AuthMode: server.AuthRequired, DataDir: dataDir, Listen: "127.0.0.1:0", Version: "test",
		MaxUploadBytes: 1 << 20}, st)
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
	ann, olga, nora := mk("ann", true), mk("olga", true), mk("nora", false)

	work := t.TempDir()
	t.Chdir(work)
	run := func(who person, args ...string) (string, string, error) {
		t.Setenv("CONCH_TOKEN", who.token)
		var out, errOut bytes.Buffer
		err := cli.Run(ctx, append([]string{args[0], "--server", url}, args[1:]...), &out, &errOut, "test")
		return strings.TrimSpace(out.String()), errOut.String(), err
	}

	data := append([]byte("binary\x00\xff payload\n"), bytes.Repeat([]byte("0123456789"), 5000)...)
	if err := os.WriteFile(filepath.Join(work, "plan.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := run(ann, "upload", "--message", "here is the plan", "ops", "plan.bin")
	if err != nil || errOut != "" {
		t.Fatalf("upload: out %q, stderr %q, err %v", out, errOut, err)
	}
	id, err := strconv.ParseInt(out, 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("upload printed %q, want a file id", out)
	}

	// One message was posted, carrying the attachment.
	msgs, err := st.ListVisibleMessages(ctx, ch.ID, store.Reader{PrincipalID: ann.ID, Scoped: true}, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "here is the plan" || msgs[0].Payload == nil ||
		msgs[0].Payload.Schema != schema.FileAttachmentV1Name || msgs[0].Audience != nil {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}

	// Another member fetches it to a name of their choosing; the bytes match.
	got := filepath.Join(work, "copy.bin")
	if out, _, err := run(olga, "download", "-o", got, out); err != nil || out != got {
		t.Fatalf("download -o: %q, %v", out, err)
	}
	if b, err := os.ReadFile(got); err != nil || !bytes.Equal(b, data) { // #nosec G304 -- a test path
		t.Errorf("round trip changed the bytes (%d vs %d): %v", len(b), len(data), err)
	}

	// Without -o the stored name is used in the current directory, once.
	if err := os.Remove(filepath.Join(work, "plan.bin")); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run(olga, "download", strconv.FormatInt(id, 10)); err != nil || out != "plan.bin" {
		t.Fatalf("download: %q, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "plan.bin")); err != nil || !bytes.Equal(b, data) { // #nosec G304 -- a test path
		t.Errorf("default-name download changed the bytes: %v", err)
	}
	if _, _, err := run(olga, "download", strconv.FormatInt(id, 10)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second download = %v, want a refusal to overwrite", err)
	}

	// A non-member gets the same refusal as for a file that does not exist.
	_, _, errMissing := run(nora, "download", "-o", filepath.Join(work, "nora.bin"), "9999")
	_, _, errOutsider := run(nora, "download", "-o", filepath.Join(work, "nora.bin"), strconv.FormatInt(id, 10))
	if errMissing == nil || errOutsider == nil || errMissing.Error() != errOutsider.Error() {
		t.Errorf("non-member: %v; unknown file: %v", errOutsider, errMissing)
	}
	if _, err := os.Stat(filepath.Join(work, "nora.bin")); err == nil {
		t.Error("a refused download left a file")
	}

	// An upload into a channel the caller is not in is refused and posts nothing.
	if _, _, err := run(nora, "upload", "ops", "copy.bin"); err == nil {
		t.Error("a non-member uploaded")
	}
	if n, _ := st.CountMessages(ctx, ch.ID); n != 1 {
		t.Errorf("%d messages in the channel, want 1", n)
	}
}
