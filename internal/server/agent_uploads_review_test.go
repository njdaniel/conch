package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// What the security review of issue #1798 found (PR #1822), each held by a
// test: how many large /mcp requests the server takes at once, that taking
// the grant away takes effect at once (for files already uploaded, and for an
// upload on its way), and two refusals that had no test.

// countingBody is a request body that counts the bytes read from it.
type countingBody struct {
	r    io.Reader
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// mcpSend sends body to /mcp as the bearer of token and returns the response
// and how many bytes of the body the server read. declared says whether the
// request states its length. It uses no testing.T, so it is safe to call from
// a goroutine.
func mcpSend(srv *Server, token string, body []byte, declared bool) (*httptest.ResponseRecorder, int64) {
	counted := &countingBody{r: bytes.NewReader(body)}
	rec := httptest.NewRecorder()
	// httptest cannot size a reader of this type, so the request has no
	// Content-Length unless it is given one.
	req := httptest.NewRequest(http.MethodPost, "/mcp", counted)
	if declared {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	srv.Handler().ServeHTTP(rec, req)
	return rec, counted.read.Load()
}

// toolError is the schema.Error code of a tool error in an /mcp response, ""
// for a call that worked, and ok false for a response that is not a result.
func toolError(rec *httptest.ResponseRecorder) (code string, ok bool) {
	var response struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Result == nil {
		return "", false
	}
	if !response.Result.IsError {
		return "", true
	}
	var serr schema.Error
	if len(response.Result.Content) == 0 || json.Unmarshal([]byte(response.Result.Content[0].Text), &serr) != nil {
		return "", false
	}
	return serr.Code, true
}

// The server handles a fixed, small number of /mcp requests over 1 MiB at
// once. One more is refused with 503 and Retry-After before its body is read,
// and nothing else is affected: an ordinary request never needs a place, and
// a place is given back however the request that held it ended.
func TestMCPLargeRequestsAreTakenAFewAtATime(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.cfg.MaxUploadBytes = 4 << 20
	f.grant(t, "aria")
	// arlo may post but holds no grant to upload.
	f.manifest(t, "arlo", []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
		schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	if cap(f.srv.mcpLargeSlots) != mcpLargeRequestSlots || mcpLargeRequestSlots != 2 {
		t.Fatalf("the server takes %d large requests at once (the constant says %d), want 2", cap(f.srv.mcpLargeSlots), mcpLargeRequestSlots)
	}
	large := func(tag byte) []byte {
		return toolCall(t, "post_message", postWith(attachment("screen.png", schema.MediaTypePNG, pngOfSize(3<<19, tag))))
	}
	small := toolCall(t, "post_message", map[string]any{"channel": "ops", "body": "words only"})
	aria, arlo := f.p("aria").token, f.p("arlo").token

	// Every place is taken, as by two large requests still being handled.
	for range mcpLargeRequestSlots {
		f.srv.mcpLargeSlots <- struct{}{}
	}
	messages := f.messageCount(t)
	for _, declared := range []bool{true, false} {
		name := "a large request that declares its length"
		// With no declared length the server reads just enough to learn
		// that the request is over the bound, and no more.
		mostRead := int64(0)
		if !declared {
			name, mostRead = "a large request that does not", schema.MaxPostRequestBytes+1
		}
		body := large('a')
		rec, read := mcpSend(f.srv, aria, body, declared)
		var e schema.Error
		if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code != schema.ErrorCodeServerBusy {
			t.Fatalf("%s, with every place taken = %d %s, want 503 %s", name, rec.Code, rec.Body, schema.ErrorCodeServerBusy)
		}
		if rec.Header().Get("Retry-After") != "2" || e.RetryAfterSeconds != 2 {
			t.Errorf("%s: Retry-After = %q, retry_after_seconds = %d; want 2", name, rec.Header().Get("Retry-After"), e.RetryAfterSeconds)
		}
		if read > mostRead {
			t.Errorf("%s: the server read %d of its %d bytes before refusing it, want at most %d", name, read, len(body), mostRead)
		}
		if rec.Header().Get("Connection") != "close" {
			t.Errorf("%s: the refusal does not close the connection; the server would wait for a body it has refused", name)
		}
	}
	// A request within the bound needs no place, with or without a length.
	for _, declared := range []bool{true, false} {
		rec, _ := mcpSend(f.srv, aria, small, declared)
		if code, ok := toolError(rec); !ok || code != "" {
			t.Errorf("an ordinary post with every place taken (declared length %v) = %d %s", declared, rec.Code, rec.Body)
		}
	}
	// An agent with no larger limit is told what it always was: too large.
	if rec, _ := mcpSend(f.srv, arlo, large('b'), true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a large request from an agent without the grant, with every place taken = %d, want 413", rec.Code)
	}
	if got := f.messageCount(t); got != messages+2 || f.countFiles(t) != 0 {
		t.Errorf("with every place taken: %d message(s) and %d file(s) stored, want the 2 ordinary posts and no file", got-messages, f.countFiles(t))
	}
	if len(f.srv.mcpLargeSlots) != mcpLargeRequestSlots {
		t.Fatalf("%d places are taken after refusals, want the %d that were", len(f.srv.mcpLargeSlots), mcpLargeRequestSlots)
	}

	// One place comes free: the next large request takes it, and gives it
	// back when it has been answered, however it ended.
	<-f.srv.mcpLargeSlots
	ended := []struct {
		name     string
		body     []byte
		declared bool
		status   int
		code     string
	}{
		{"posted", large('c'), true, 200, ""},
		{"posted, no declared length", large('d'), false, 200, ""},
		{"refused by the tool", toolCall(t, "post_message", postWith(attachment("anim.gif", "image/gif", pngOfSize(3<<19, 'e')))), true, 200, schema.ErrorCodeUnsupportedMediaType},
		{"refused as not image data", toolCall(t, "post_message", map[string]any{"channel": "ops", "body": strings.Repeat("a", 2<<20)}), true, 413, ""},
		{"refused as not image data, no declared length", toolCall(t, "post_message", map[string]any{"channel": "ops", "body": strings.Repeat("a", 2<<20)}), false, 413, ""},
	}
	for _, tt := range ended {
		rec, _ := mcpSend(f.srv, aria, tt.body, tt.declared)
		code, ok := toolError(rec)
		if rec.Code != tt.status || (tt.status == http.StatusOK && (!ok || code != tt.code)) {
			t.Errorf("a large request %s = %d, tool error %q; want %d, %q (%.200s)", tt.name, rec.Code, code, tt.status, tt.code, rec.Body)
		}
		if got := len(f.srv.mcpLargeSlots); got != mcpLargeRequestSlots-1 {
			t.Fatalf("after a large request %s, %d places are taken, want %d: it did not give its place back", tt.name, got, mcpLargeRequestSlots-1)
		}
	}
	// A request that says it is longer than what arrives.
	rec := httptest.NewRecorder()
	cut := large('f')
	req := httptest.NewRequest(http.MethodPost, "/mcp", io.MultiReader(bytes.NewReader(cut[:len(cut)-100]), errReader{}))
	req.ContentLength = int64(len(cut))
	req.Header.Set("Authorization", "Bearer "+aria)
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a large request cut short = %d, want 400", rec.Code)
	}
	if got := len(f.srv.mcpLargeSlots); got != mcpLargeRequestSlots-1 {
		t.Fatalf("after a large request cut short, %d places are taken, want %d: it did not give its place back", got, mcpLargeRequestSlots-1)
	}
}

// uploadDenial is the audit detail of an upload or an attachment refused for
// want of the grant. The capability is decided from the manifest alone, before
// any channel is looked at, so that refusal names none.
func uploadDenial(f *agentUploadFixture, reason string) string {
	target := fmt.Sprintf("channel:%d", f.ops.ID)
	if reason == denyCapability {
		target = "none"
	}
	return fmt.Sprintf("capability=%s target=%s reason=%s", schema.CapabilityFilesUpload, target, reason)
}

// errReader is a body that fails where the rest of it should be.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// Many large requests at once: each is either handled or told the server is
// busy, what was stored is exactly what was answered as posted, and every
// place is free again at the end.
func TestMCPLargeRequestsTogether(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.cfg.MaxUploadBytes = 4 << 20
	f.srv.setAgentUploadsPerHour(100)
	f.grant(t, "aria")
	const senders = 8
	bodies := make([][]byte, senders)
	for i := range bodies {
		bodies[i] = toolCall(t, "post_message", postWith(attachment("screen.png", schema.MediaTypePNG, pngOfSize(3<<19, byte('a'+i%26)))))
	}
	messages := f.messageCount(t)
	var posted, busy, other atomic.Int64
	var wg sync.WaitGroup
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _ := mcpSend(f.srv, f.p("aria").token, bodies[i], i%2 == 0)
			code, ok := toolError(rec)
			switch {
			case rec.Code == http.StatusServiceUnavailable:
				busy.Add(1)
			case ok && code == "":
				posted.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	if other.Load() != 0 || posted.Load() == 0 || posted.Load()+busy.Load() != senders {
		t.Fatalf("%d posted, %d busy, %d anything else; want every request posted or busy, and at least one posted", posted.Load(), busy.Load(), other.Load())
	}
	if got := int64(f.messageCount(t) - messages); got != posted.Load() || int64(f.countFiles(t)) != posted.Load() {
		t.Errorf("%d message(s) and %d file(s) stored for %d posts answered as posted", got, f.countFiles(t), posted.Load())
	}
	if got := len(f.srv.mcpLargeSlots); got != 0 {
		t.Errorf("%d places are still taken after every request ended", got)
	}
}

// Taking the grant away takes effect at once for what the agent already
// uploaded. A file an agent put by while it held the grant cannot be attached
// after: every post by an agent that names existing files needs the grant in
// that channel at that moment, on every route. The refusal is the one every
// unusable attachment gets, and it is audited as a denial.
func TestAgentAttachingNeedsTheGrantNow(t *testing.T) {
	revocations := []struct {
		name   string
		revoke func(t *testing.T, f *agentUploadFixture)
		reason string
	}{
		{"files.upload taken away", func(t *testing.T, f *agentUploadFixture) {
			f.manifest(t, "aria", []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
				schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionAttach)
		}, denyCapability},
		{"attach taken away", func(t *testing.T, f *agentUploadFixture) {
			f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
		}, denyChannelPermission},
	}
	type attempt struct {
		// refused reports whether the post was refused as an unusable
		// attachment; posted whether it was stored.
		refused, posted bool
		detail          string
	}
	rest := func(t *testing.T, f *agentUploadFixture, path, body string) attempt {
		res := f.do(t, "POST", path, "aria", "application/json", strings.NewReader(body))
		var e schema.Error
		_ = json.Unmarshal(res.body, &e)
		return attempt{refused: res.status == http.StatusBadRequest && e.Code == errInvalidFileAttachment.Code, posted: res.status == http.StatusCreated, detail: res.String()}
	}
	mcp := func(t *testing.T, f *agentUploadFixture, id int64, more map[string]any) attempt {
		args := map[string]any{"channel": "ops", "body": "from the stash",
			"payload": map[string]any{"schema": schema.FileAttachmentV1Name, "data": map[string]any{"file_ids": []int64{id}}}}
		for k, v := range more {
			args[k] = v
		}
		out := f.mcpCall(t, f.p("aria").token, "post_message", args)
		return attempt{refused: out.isError && out.code == errInvalidFileAttachment.Code, posted: out.status == http.StatusOK && !out.isError, detail: out.result() + " " + out.body}
	}
	paths := []struct {
		name    string
		subject string
		// sees can read the message once it is posted.
		sees string
		post func(t *testing.T, f *agentUploadFixture, id, root int64) attempt
	}{
		{"MCP, to the channel", "mcp:post_message", "wes", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt { return mcp(t, f, id, nil) }},
		{"MCP, to a net", "mcp:post_message", "nina", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return mcp(t, f, id, map[string]any{"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}})
		}},
		{"MCP, a whisper", "mcp:post_message", "ann", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return mcp(t, f, id, map[string]any{"audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.p("ann").ID}}})
		}},
		{"MCP, a reply", "mcp:post_message", "wes", func(t *testing.T, f *agentUploadFixture, id, root int64) attempt {
			return mcp(t, f, id, map[string]any{"thread_root_id": root})
		}},
		{"REST v2, to the channel", "POST /v2/channels/{channel}/messages", "wes", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return rest(t, f, "/v2/channels/ops/messages", attachBody("from the stash", "", id))
		}},
		{"REST v2, to a net", "POST /v2/channels/{channel}/messages", "nina", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return rest(t, f, "/v2/channels/ops/messages", attachBody("from the stash", netAudience(f.net1.ID), id))
		}},
		{"REST v2, a whisper", "POST /v2/channels/{channel}/messages", "ann", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return rest(t, f, "/v2/channels/ops/messages", attachBody("from the stash", whisperAudience(f.p("ann").ID), id))
		}},
		{"REST v2, a reply", "POST /v2/channels/{channel}/messages", "wes", func(t *testing.T, f *agentUploadFixture, id, root int64) attempt {
			return rest(t, f, "/v2/channels/ops/messages", attachBody("from the stash", fmt.Sprintf(`"thread_root_id":%d`, root), id))
		}},
		{"REST v1, to the channel", "POST /v1/channels/{channel}/messages", "wes", func(t *testing.T, f *agentUploadFixture, id, _ int64) attempt {
			return rest(t, f, "/v1/channels/ops/messages", attachBody("from the stash", "", id))
		}},
	}
	for _, rv := range revocations {
		for _, p := range paths {
			t.Run(rv.name+"/"+p.name, func(t *testing.T) {
				f := newAgentUploadFixture(t)
				f.grant(t, "aria", schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
				root := f.mustPost(t, "ann", `{"body":"how is it going?"}`)
				// Uploaded with the grant, and not attached.
				data := pngOfSize(64, 's')
				res := f.upload(t, "aria", "stash.png", schema.MediaTypePNG, data)
				var meta schema.FileV1
				if res.status != http.StatusCreated || json.Unmarshal(res.body, &meta) != nil {
					t.Fatalf("upload = %s", res)
				}

				rv.revoke(t, f)
				messages, attached := f.messageCount(t), f.auditCount(t, store.AuditFileAttached)
				got := p.post(t, f, meta.ID, root)
				if !got.refused {
					t.Fatalf("attaching a file uploaded before the grant was taken away = %s, want %s", got.detail, errInvalidFileAttachment.Code)
				}
				if f.messageCount(t) != messages || f.auditCount(t, store.AuditFileAttached) != attached {
					t.Errorf("the refused post stored %d message(s) and attached %d file(s)", f.messageCount(t)-messages, f.auditCount(t, store.AuditFileAttached)-attached)
				}
				if res := f.fetch(t, p.sees, meta.ID); res.status != http.StatusNotFound {
					t.Errorf("%s fetching the file after the refused post = %s, want 404: it was published", p.sees, res)
				}
				d := f.denials(t, "aria")
				if len(d) != 1 || d[0].Subject != p.subject || d[0].Detail != uploadDenial(f, rv.reason) {
					t.Errorf("denials = %+v, want one of files.upload under %s with reason %s", d, p.subject, rv.reason)
				}
				// Only attaching is refused: the agent may still post, and
				// carry a payload of another kind.
				other := map[string]any{"schema": "test.thing.v1", "data": map[string]any{"k": "v"}}
				if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "words", "payload": other}); out.isError {
					t.Errorf("a post with another payload, without the grant = %s", out.result())
				}

				// With the grant back, the same post attaches the same file.
				f.grant(t, "aria", schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
				if got := p.post(t, f, meta.ID, root); !got.posted {
					t.Fatalf("the same post with the grant back = %s, want it posted", got.detail)
				}
				if res := f.fetch(t, p.sees, meta.ID); res.status != http.StatusOK || !bytes.Equal(res.body, data) {
					t.Errorf("%s fetching the file once it is attached = %d", p.sees, res.status)
				}
			})
		}
	}
}

// The grant holds for a file the agent names that is not its own exactly as
// it does without one: the store's refusal, with no audit row. (An agent
// without the grant never reaches the store; see above.)
func TestGrantedAgentStillCannotAttachAnothersFile(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.grant(t, "aria")
	f.grant(t, "arlo")
	upload := func(who string, tag byte) int64 {
		res := f.upload(t, who, "shot.png", schema.MediaTypePNG, pngOfSize(64, tag))
		var meta schema.FileV1
		if res.status != http.StatusCreated || json.Unmarshal(res.body, &meta) != nil {
			t.Fatalf("%s upload = %s", who, res)
		}
		return meta.ID
	}
	tests := map[string]int64{
		"a person's file":      f.mustUpload(t, "ann", "draft.txt", []byte("ann's unshared draft")).ID,
		"another agent's file": upload("arlo", 'o'),
		"a file that is not":   999999,
	}
	messages := f.messageCount(t)
	for name, id := range tests {
		payload := map[string]any{"schema": schema.FileAttachmentV1Name, "data": map[string]any{"file_ids": []int64{id}}}
		if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "x", "payload": payload}); !out.isError || out.code != errInvalidFileAttachment.Code {
			t.Errorf("attaching %s over MCP = %s, want %s", name, out.result(), errInvalidFileAttachment.Code)
		}
		if res := f.do(t, "POST", "/v2/channels/ops/messages", "aria", "application/json", strings.NewReader(attachBody("x", "", id))); res.status != http.StatusBadRequest {
			t.Errorf("attaching %s over REST = %s, want 400", name, res)
		}
	}
	if f.messageCount(t) != messages {
		t.Errorf("refused posts stored %d message(s)", f.messageCount(t)-messages)
	}
	if d := f.denials(t, "aria"); len(d) != 0 {
		t.Errorf("a granted agent's refused attachments were audited as denials: %+v", d)
	}
}

// An upload over REST is decided when it begins and again when its body has
// arrived. An agent whose grant is taken away in between gets the 403 of an
// agent without one, and no file row. The bytes were already in place: they
// stay, referenced by nothing, and the upload stays counted.
func TestAgentRESTUploadIsDecidedAgainBeforeItsRow(t *testing.T) {
	tests := []struct {
		name   string
		revoke func(f *agentUploadFixture) error
		reason string
		// rejoin says the agent must be put back in the channel afterwards.
		rejoin bool
	}{
		{"attach taken away", func(f *agentUploadFixture) error {
			_, _, err := f.srv.store.PutAgentManifest(t.Context(), "system", f.p("aria").ID, schema.PutAgentManifestRequestV1{
				DisplayName: "aria", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
				Channels: []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}}})
			return err
		}, denyChannelPermission, false},
		{"files.upload taken away", func(f *agentUploadFixture) error {
			_, _, err := f.srv.store.PutAgentManifest(t.Context(), "system", f.p("aria").ID, schema.PutAgentManifestRequestV1{
				DisplayName: "aria", Tier: schema.AgentTierC, Capabilities: []schema.Capability{schema.CapabilityMessagesPost},
				Channels: []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost, schema.ChannelPermissionAttach}}}})
			return err
		}, denyCapability, false},
		{"removed from the channel", func(f *agentUploadFixture) error {
			_, err := f.srv.store.RemoveChannelMember(t.Context(), "system", f.ops.ID, f.p("aria").ID)
			return err
		}, denyNotMember, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			f.srv.setAgentUploadsPerHour(1)
			f.grant(t, "aria")
			// The grant is taken away while the upload is under way: after
			// it was decided, as the bytes are put in place, before the row.
			revoked := make(chan error, 1)
			f.srv.renameFile = func(from, to string) error {
				revoked <- tt.revoke(f)
				return os.Rename(from, to)
			}
			data := pngOfSize(64, 'r')
			res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, data)
			f.srv.renameFile = os.Rename
			select {
			case err := <-revoked:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("the upload never put its bytes in place, so the grant was never taken away")
			}
			var e schema.Error
			if res.status != http.StatusForbidden || json.Unmarshal(res.body, &e) != nil || e.Code != errForbidden.Code || e.Message != agentFilesRefusal {
				t.Fatalf("an upload whose grant was taken away under way = %s, want the 403 %q", res, agentFilesRefusal)
			}
			if n := f.countFiles(t); n != 0 {
				t.Errorf("%d file row(s) for the refused upload", n)
			}
			if n := f.auditCount(t, store.AuditFileUploaded); n != 0 {
				t.Errorf("%d file_uploaded row(s) for the refused upload", n)
			}
			d := f.denials(t, "aria")
			if len(d) != 1 || d[0].Subject != "POST /v2/channels/{channel}/files" || d[0].Detail != uploadDenial(f, tt.reason) {
				t.Errorf("denials = %+v, want one with reason %s", d, tt.reason)
			}
			// The bytes are where they were put, and no row names them.
			f.assertDisk(t, data)
			// And they were counted: with the grant back (and the agent in
			// the channel again), the one file of the budget is gone.
			if tt.rejoin {
				f.join(t, "aria")
			}
			f.grant(t, "aria")
			if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'n')); res.status != http.StatusTooManyRequests {
				t.Errorf("the next upload = %s, want 429: the refused one, whose bytes are stored, must stay counted", res)
			}
		})
	}
}

// An archived channel takes no upload from an agent, by either way in. Over
// MCP nothing is left behind and the budget is as it was. Over REST the
// refusal is the one a person gets, with the bytes left under their hash and
// the upload counted, as for every upload whose row is refused.
func TestAgentUploadsToAnArchivedChannel(t *testing.T) {
	f := newAgentUploadFixture(t)
	// Four for the call at the end, and one for the REST upload.
	f.srv.setAgentUploadsPerHour(schema.MaxAgentAttachments + 1)
	f.grant(t, "aria", schema.ChannelPermissionPostNet)
	root := f.mustPost(t, "ann", `{"body":"how is it going?"}`)
	settings := func(body string) {
		t.Helper()
		if res := f.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", f.ops.ID), f.rootTok, body); res.status != http.StatusOK {
			t.Fatalf("settings %s = %d %s", body, res.status, res.body)
		}
	}
	settings(`{"archived":true}`)

	messages := f.messageCount(t)
	png := attachment("shot.png", schema.MediaTypePNG, pngOfSize(64, 'z'))
	for name, more := range map[string]map[string]any{
		"to the channel": nil,
		"a reply":        {"thread_root_id": root},
		"to a net":       {"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}},
	} {
		args := postWith(png)
		for k, v := range more {
			args[k] = v
		}
		out := f.mcpCall(t, f.p("aria").token, "post_message", args)
		if !out.isError || out.code != schema.ErrorCodeChannelArchived {
			t.Errorf("attachments %s, in an archived channel = %s, want %s (%s)", name, out.result(), schema.ErrorCodeChannelArchived, out.body)
		}
		f.assertNothingKept(t, messages)
	}

	data := pngOfSize(64, 'y')
	res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, data)
	var e schema.Error
	if res.status != http.StatusConflict || json.Unmarshal(res.body, &e) != nil || e.Code != schema.ErrorCodeChannelArchived {
		t.Errorf("a REST upload to an archived channel = %s, want 409 %s", res, schema.ErrorCodeChannelArchived)
	}
	if n := f.countFiles(t); n != 0 {
		t.Errorf("%d file row(s) in an archived channel", n)
	}
	f.assertDisk(t, data)

	// Restored, the channel takes them again. The three refused calls cost
	// nothing; the REST upload, whose bytes are stored, cost one.
	settings(`{"archived":false}`)
	item := func(tag byte) map[string]any {
		return attachment("shot.png", schema.MediaTypePNG, pngOfSize(32, tag))
	}
	if _, ids := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('1'), item('2'), item('3'), item('4')))); len(ids) != schema.MaxAgentAttachments {
		t.Errorf("the call after the channel was restored attaches %v", ids)
	}
	if out := f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('5'))); !out.isError || out.code != schema.ErrorCodeRateLimited {
		t.Errorf("a sixth file in the hour = %s, want %s: the REST upload to the archived channel must stay counted", out.result(), schema.ErrorCodeRateLimited)
	}
}

// A webhook bound to an agent posts as the agent without being it. It cannot
// attach the agent's own unattached upload: attaching takes the uploader's
// own credential, which the holder of a webhook's token does not have.
func TestAgentBoundWebhookCannotAttachTheAgentsUpload(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.grant(t, "aria")
	data := pngOfSize(64, 'h')
	res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, data)
	var meta schema.FileV1
	if res.status != http.StatusCreated || json.Unmarshal(res.body, &meta) != nil {
		t.Fatalf("upload = %s", res)
	}
	const token = "hook-token-for-the-agents-webhook"
	if _, err := f.srv.store.CreateHook(t.Context(), "system", token, f.ops.ID, f.p("aria").ID); err != nil {
		t.Fatal(err)
	}
	messages := f.messageCount(t)
	res = f.do(t, http.MethodPost, "/v1/hooks/"+token, "", "application/json", strings.NewReader(attachBody("from the hook", "", meta.ID)))
	var e schema.Error
	if res.status != http.StatusBadRequest || json.Unmarshal(res.body, &e) != nil || e.Code != errFilesNeedAuth.Code {
		t.Fatalf("the agent's webhook attaching the agent's upload = %s, want 400 %s", res, errFilesNeedAuth.Code)
	}
	if f.messageCount(t) != messages || f.auditCount(t, store.AuditFileAttached) != 0 {
		t.Errorf("the refused webhook post stored %d message(s) and attached %d file(s)", f.messageCount(t)-messages, f.auditCount(t, store.AuditFileAttached))
	}
	if got := f.fetch(t, "wes", meta.ID); got.status != http.StatusNotFound {
		t.Errorf("a member fetching the file after the webhook's attempt = %s, want 404", got)
	}
	// The webhook still posts words, and the file is still the agent's to
	// attach with its own credential.
	if res := f.do(t, http.MethodPost, "/v1/hooks/"+token, "", "application/json", strings.NewReader(`{"body":"words from the hook"}`)); res.status/100 != 2 {
		t.Errorf("the webhook posting words = %s", res)
	}
	if res := f.do(t, "POST", "/v2/channels/ops/messages", "aria", "application/json", strings.NewReader(attachBody("mine", "", meta.ID))); res.status != http.StatusCreated {
		t.Errorf("the agent attaching its own upload afterwards = %s, want 201", res)
	}
	if got := f.fetch(t, "wes", meta.ID); got.status != http.StatusOK || !bytes.Equal(got.body, data) {
		t.Errorf("a member fetching the file once the agent attached it = %d", got.status)
	}
}
