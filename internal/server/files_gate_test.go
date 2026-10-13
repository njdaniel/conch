package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// A webhook posts as its principal without being that principal. If its posts
// could attach files, whoever holds the hook's token could take an upload only
// its uploader can read (file ids are sequential, so it is easily named),
// attach it to a channel-wide message, and read it; so could everyone else in
// the channel. Attaching takes the uploader's own verified credential.
func TestWebhookCannotAttachFiles(t *testing.T) {
	f := newFileFixture(t)
	draft := f.mustUpload(t, "ann", "draft.txt", []byte("ann's unshared draft"))
	missing := f.fetch(t, "wes", 999999)

	created := f.authFixture.do(t, http.MethodPost, "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"ops","principal":%d}`, f.p("ann").ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create hook = %d: %s", created.Code, created.Body.String())
	}
	var hook struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &hook); err != nil || hook.Token == "" {
		t.Fatalf("hook token missing from %s (%v)", created.Body.String(), err)
	}
	ingest := func(body string) fileResp {
		return f.do(t, http.MethodPost, "/v1/hooks/"+hook.Token, "", "application/json", strings.NewReader(body))
	}

	before := f.messageCount(t)
	res := ingest(attachBody("from the hook", "", draft.ID))
	// The hook route refuses in its own words, before it hands the request on
	// (postMessage would refuse too; TestUnauthenticatedPostCannotAttachFiles
	// holds that one).
	if res.status != http.StatusBadRequest || errCodeOf(t, string(res.body)) != errFilesNeedAuth.Code || !strings.Contains(string(res.body), "a webhook cannot attach files") {
		t.Fatalf("hook post attaching a file = %s, want 400 %s from the hook route", res, errFilesNeedAuth.Code)
	}
	// With a credential in the request it is still a hook post: the bearer
	// header of an exempt route identifies nobody.
	withBearer := f.do(t, http.MethodPost, "/v1/hooks/"+hook.Token, "ann", "application/json", strings.NewReader(attachBody("from the hook", "", draft.ID)))
	if withBearer.status != http.StatusBadRequest {
		t.Errorf("hook post attaching a file, sent with the uploader's own bearer = %s, want 400", withBearer)
	}
	if got := f.messageCount(t); got != before {
		t.Errorf("the refused hook post stored %d message(s)", got-before)
	}
	// Nobody but the uploader can read it, exactly as before.
	if got := f.fetch(t, "wes", draft.ID); got.status != missing.status || string(got.body) != string(missing.body) {
		t.Errorf("another member's fetch after the hook post = %s, want the no-such-file answer %s", got, missing)
	}
	if got := f.fetch(t, "ann", draft.ID); got.status != http.StatusOK {
		t.Errorf("the uploader's fetch = %s, want 200", got)
	}
	// The file is still unattached: its uploader can attach it herself.
	if res := f.do(t, http.MethodPost, "/v2/channels/ops/messages", "ann", "application/json", strings.NewReader(attachBody("shared now", "", draft.ID))); res.status != http.StatusCreated {
		t.Errorf("the uploader attaching her own file afterwards = %s, want 201", res)
	}
	// The hook itself still posts ordinary messages, with or without a payload
	// of another kind.
	if res := ingest(`{"body":"plain hook message"}`); res.status != http.StatusCreated {
		t.Errorf("an ordinary hook post = %s, want 201", res)
	}
}

// With authentication off the author of a post is whatever its body says, so
// such a post must not attach either: files uploaded while authentication was
// on would otherwise be publishable by anyone who can reach the port.
func TestUnauthenticatedPostCannotAttachFiles(t *testing.T) {
	ctx := context.Background()
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, MaxUploadBytes: testUploadLimit})
	channel, err := srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	people := map[string]store.Principal{}
	for _, name := range []string{"ann", "wes"} {
		p, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.AddChannelMember(ctx, "system", channel.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		people[name] = p
	}
	// A file ann uploaded while authentication was on, never attached.
	draft, err := srv.store.InsertFile(ctx, store.NewFile{ChannelID: channel.ID, UploaderID: people["ann"].ID,
		Filename: "draft.txt", MediaType: "text/plain", Size: 5, SHA256: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		body := fmt.Sprintf(`{"author_id":%d,"body":"x","payload":{"schema":%q,"data":{"file_ids":[%d]}}}`, people["ann"].ID, schema.FileAttachmentV1Name, draft.ID)
		rec := postJSON(t, srv, "/"+version+"/channels/ops/messages", body)
		if rec.Code != http.StatusBadRequest || errCodeOf(t, rec.Body.String()) != errFilesNeedAuth.Code {
			t.Errorf("%s post attaching a file with authentication off = %d %s, want 400 %s", version, rec.Code, rec.Body.String(), errFilesNeedAuth.Code)
		}
	}
	// Still attached to nothing: another member cannot be shown it.
	if _, err := srv.store.FileVisibleTo(ctx, draft.ID, store.Reader{Scoped: true, PrincipalID: people["wes"].ID}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another member's read of the file = %v, want not found", err)
	}
	// An ordinary post with authentication off is unaffected.
	if rec := postJSON(t, srv, "/v1/channels/ops/messages", fmt.Sprintf(`{"author_id":%d,"body":"plain"}`, people["ann"].ID)); rec.Code != http.StatusCreated {
		t.Errorf("an ordinary post = %d %s, want 201", rec.Code, rec.Body.String())
	}
}
