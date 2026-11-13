package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Agents posting images (issue #1798): the grant, the two ways in, and what
// a refusal leaves behind, which is nothing.

// The smallest things that begin as each type does. Nothing here decodes an
// image, so these are a signature and a few bytes more.
var (
	testPNG  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR a png")
	testJPEG = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00 a jpeg")
	testWebP = []byte("RIFF\x1a\x00\x00\x00WEBPVP8  a webp")
	testGIF  = []byte("GIF89a\x01\x00\x01\x00 a gif, which no agent may upload")
)

// pngOfSize is size bytes that begin as a PNG and are otherwise distinct for
// each tag, so two of them are never stored as one.
func pngOfSize(size int, tag byte) []byte {
	b := bytes.Repeat([]byte{tag}, size)
	copy(b, "\x89PNG\r\n\x1a\n")
	return b
}

// agentUploadFixture is the file fixture with the agents' upload budget on,
// under a clock the test moves.
type agentUploadFixture struct {
	*fileFixture
	clock *manualClock
}

func newAgentUploadFixture(t *testing.T) *agentUploadFixture {
	t.Helper()
	f := &agentUploadFixture{fileFixture: newFileFixture(t), clock: newManualClock()}
	f.srv.now = f.clock.Now
	f.srv.setAgentUploadsPerHour(DefaultAgentUploadsPerHour)
	return f
}

// grant gives agent name the whole grant in ops and everything a post needs:
// every capability, and read, post and attach, plus any extra permissions.
func (f *agentUploadFixture) grant(t *testing.T, name string, extra ...schema.ChannelPermission) {
	t.Helper()
	perms := append([]schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionAttach}, extra...)
	f.manifest(t, name, nil, perms...)
}

// doToken is do for a caller the fixture does not know by name (a guest).
func (f *agentUploadFixture) doToken(t *testing.T, method, path, token, contentType string, body []byte) fileResp {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- a test-local listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fileResp{resp.StatusCode, resp.Header, b}
}

// denials are the access_denied rows of one principal, oldest first.
func (f *agentUploadFixture) denials(t *testing.T, who string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", f.p(who).ID) {
			out = append(out, e)
		}
	}
	return out
}

// attachment is one item of post_message's attachments argument.
func attachment(name, mediaType string, data []byte) map[string]any {
	return map[string]any{"name": name, "media_type": mediaType, "data_base64": base64.StdEncoding.EncodeToString(data)}
}

// postWith is the arguments of a channel-wide post_message to ops that
// carries items.
func postWith(items ...map[string]any) map[string]any {
	return map[string]any{"channel": "ops", "body": "progress", "attachments": items}
}

// posted decodes the message a successful post_message returned, and the ids
// of the files it attaches.
func posted(t *testing.T, out mcpOutcome) (schema.MessageV2, []int64) {
	t.Helper()
	if out.status != http.StatusOK || out.isError {
		t.Fatalf("post_message = %s (%s)", out.result(), out.body)
	}
	var response struct {
		Result struct {
			StructuredContent schema.PostMessageResponseV2 `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &response); err != nil {
		t.Fatalf("decode %s: %v", out.body, err)
	}
	msg := response.Result.StructuredContent.Message
	if err := msg.Validate(); err != nil {
		t.Fatalf("the posted message is not a valid conch.message.v2: %v", err)
	}
	if msg.Payload == nil || msg.Payload.Schema != schema.FileAttachmentV1Name {
		t.Fatalf("the posted message carries payload %+v, want a %s", msg.Payload, schema.FileAttachmentV1Name)
	}
	var att schema.FileAttachmentV1
	if err := json.Unmarshal(msg.Payload.Data, &att); err != nil || att.Validate() != nil {
		t.Fatalf("the attachment payload %s is not valid: %v", msg.Payload.Data, err)
	}
	return msg, att.FileIDs
}

// fileMeta reads a file's metadata as who, who must be able to see it.
func (f *agentUploadFixture) fileMeta(t *testing.T, who string, id int64) schema.FileV1 {
	t.Helper()
	res := f.meta(t, who, id)
	if res.status != http.StatusOK {
		t.Fatalf("%s reads the metadata of file %d = %s", who, id, res)
	}
	var meta schema.FileV1
	if err := json.Unmarshal(res.body, &meta); err != nil || meta.Validate() != nil {
		t.Fatalf("metadata %s is not a valid conch.file.v1: %v", res.body, err)
	}
	return meta
}

// assertNothingKept fails unless the refused call left no file row, no
// message, no file on disk (a temporary one included) and no upload audit.
func (f *agentUploadFixture) assertNothingKept(t *testing.T, messagesBefore int) {
	t.Helper()
	if n := f.countFiles(t); n != 0 {
		t.Errorf("%d file row(s) after a refused call", n)
	}
	if got := f.messageCount(t); got != messagesBefore {
		t.Errorf("a refused call stored %d message(s)", got-messagesBefore)
	}
	if n := f.auditCount(t, store.AuditFileUploaded) + f.auditCount(t, store.AuditFileAttached); n != 0 {
		t.Errorf("%d upload audit row(s) after a refused call", n)
	}
	f.assertDisk(t)
}

func TestSniffAgentImage(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want string
	}{
		{"png", testPNG, schema.MediaTypePNG},
		{"jpeg", testJPEG, schema.MediaTypeJPEG},
		{"webp", testWebP, schema.MediaTypeWebP},
		{"only the png signature", []byte("\x89PNG\r\n\x1a\n"), schema.MediaTypePNG},
		{"only the jpeg signature", []byte{0xFF, 0xD8, 0xFF}, schema.MediaTypeJPEG},
		{"gif", testGIF, ""},
		{"text", []byte("hello, world"), ""},
		{"html that names png", []byte("<html>PNG</html>"), ""},
		{"a png signature one byte short", []byte("\x89PNG\r\n\x1a"), ""},
		{"a png signature after a byte", append([]byte{0}, testPNG...), ""},
		{"riff that is a wave file", []byte("RIFF\x1a\x00\x00\x00WAVEfmt "), ""},
		{"riff cut before webp", []byte("RIFF\x1a\x00\x00\x00WEB"), ""},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sniffAgentImage(tt.head[:min(len(tt.head), imageSignatureBytes)]); got != tt.want {
				t.Errorf("sniffAgentImage = %q, want %q", got, tt.want)
			}
		})
	}
}

// The budget is all or nothing for a call that needs several files, refills
// evenly, and gives back what was not stored.
func TestAgentUploadLimiter(t *testing.T) {
	clock := newManualClock()
	l := newAgentUploadLimiter(4)
	take := func(principal int64, n int) bool {
		ok, retry := l.take(principal, n, clock.Now())
		if !ok && retry < time.Second {
			t.Errorf("a refusal says to retry in %s", retry)
		}
		return ok
	}
	if !take(1, 3) {
		t.Fatal("3 of 4 refused")
	}
	if take(1, 2) {
		t.Fatal("2 more taken with 1 left")
	}
	if !take(1, 1) {
		t.Fatal("the refused call spent something: the last file is gone")
	}
	if take(1, 1) {
		t.Fatal("a fifth file taken")
	}
	if !take(2, 4) {
		t.Fatal("another agent shares the budget")
	}
	l.refund(1, 2, clock.Now())
	if !take(1, 2) || take(1, 1) {
		t.Fatal("a refund of 2 did not give back exactly 2")
	}
	// Four an hour is one every fifteen minutes.
	clock.Advance(15 * time.Minute)
	if !take(1, 1) || take(1, 1) {
		t.Fatal("15 minutes did not refill exactly one file")
	}
	// More than the whole budget can never be had, however long it waits.
	clock.Advance(24 * time.Hour)
	if ok, retry := l.take(1, 5, clock.Now()); ok || retry != time.Hour {
		t.Errorf("5 of a budget of 4 = %v, retry %s; want a refusal for the whole window", ok, retry)
	}
	if !take(1, 4) {
		t.Fatal("the impossible request spent something")
	}
	// A budget of zero admits nothing.
	if ok, _ := newAgentUploadLimiter(0).take(1, 1, clock.Now()); ok {
		t.Error("a budget of zero admitted an upload")
	}
}

// The grant matrix on each file route, for an agent: the capability, the
// channel permission and membership, each present or absent. The upload is
// allowed only with all three; without any one of them it is the 403 agents
// always got there. The two routes that fetch refuse an agent whatever it
// holds, for someone else's file and for its own upload.
func TestAgentFileRoutesGrantMatrix(t *testing.T) {
	for _, capability := range []bool{true, false} {
		for _, permission := range []bool{true, false} {
			for _, member := range []bool{true, false} {
				name := fmt.Sprintf("capability=%v permission=%v member=%v", capability, permission, member)
				t.Run(name, func(t *testing.T) {
					f := newAgentUploadFixture(t)
					// aria is in ops; nomad is not.
					who := "aria"
					if !member {
						who = "nomad"
					}
					caps := []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}
					if capability {
						caps = append(caps, schema.CapabilityFilesUpload)
					}
					perms := []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}
					if permission {
						perms = append(perms, schema.ChannelPermissionAttach)
					}
					f.manifest(t, who, caps, perms...)
					// A person's file, attached to a channel-wide message:
					// every member of ops may fetch it.
					theirs := f.mustUpload(t, "ann", "plan.txt", []byte("ann's file"))
					if res := f.do(t, "POST", "/v2/channels/ops/messages", "ann", "application/json", strings.NewReader(attachBody("the plan", "", theirs.ID))); res.status != http.StatusCreated {
						t.Fatalf("ann attaches her file = %s", res)
					}
					rows, uploadsBefore := f.countFiles(t), f.auditCount(t, store.AuditFileUploaded)
					fetchable := []int64{theirs.ID}

					res := f.upload(t, who, "shot.png", schema.MediaTypePNG, testPNG)
					if capability && permission && member {
						if res.status != http.StatusCreated {
							t.Fatalf("the granted upload = %s, want 201", res)
						}
						var meta schema.FileV1
						if err := json.Unmarshal(res.body, &meta); err != nil || meta.Validate() != nil {
							t.Fatalf("the answer %s is not a valid conch.file.v1: %v", res.body, err)
						}
						if meta.UploaderID != f.p(who).ID || meta.ChannelID != f.ops.ID || meta.MediaType != schema.MediaTypePNG ||
							meta.Filename != "shot.png" || meta.Size != int64(len(testPNG)) || meta.SHA256 != sumHex(testPNG) {
							t.Errorf("the stored file = %+v", meta)
						}
						if got := f.countFiles(t); got != rows+1 {
							t.Errorf("file rows = %d, want %d", got, rows+1)
						}
						if d := f.denials(t, who); len(d) != 0 {
							t.Errorf("a granted upload was audited as denied: %+v", d)
						}
						fetchable = append(fetchable, meta.ID)
					} else {
						var e schema.Error
						if res.status != http.StatusForbidden || json.Unmarshal(res.body, &e) != nil || e.Code != errForbidden.Code || e.Message != agentFilesRefusal {
							t.Fatalf("the upload without the whole grant = %s, want the 403 %q %q", res, errForbidden.Code, agentFilesRefusal)
						}
						if got := f.countFiles(t); got != rows {
							t.Errorf("a refused upload made %d file row(s)", got-rows)
						}
						if got := f.auditCount(t, store.AuditFileUploaded); got != uploadsBefore {
							t.Errorf("a refused upload wrote %d file_uploaded row(s)", got-uploadsBefore)
						}
						// The audit row says which part of the grant was
						// missing; the answer does not.
						reason, target := denyCapability, "target=none"
						if capability {
							target = fmt.Sprintf("target=channel:%d", f.ops.ID)
							reason = denyNotMember
							if member {
								reason = denyChannelPermission
							}
						}
						d := f.denials(t, who)
						if len(d) != 1 || d[0].Subject != "POST /v2/channels/{channel}/files" ||
							d[0].Detail != fmt.Sprintf("capability=%s %s reason=%s", schema.CapabilityFilesUpload, target, reason) {
							t.Errorf("denials = %+v, want one of files.upload with %s and reason %s", d, target, reason)
						}
					}

					// Fetching: refused for every agent, for every file.
					before := len(f.denials(t, who))
					for _, id := range fetchable {
						for _, got := range []fileResp{f.fetch(t, who, id), f.meta(t, who, id)} {
							var e schema.Error
							if got.status != http.StatusForbidden || json.Unmarshal(got.body, &e) != nil || e.Code != errForbidden.Code || e.Message != agentFilesRefusal {
								t.Errorf("the agent fetching file %d = %s, want the 403 %q", id, got, agentFilesRefusal)
							}
							if bytes.Contains(got.body, []byte("ann's file")) || bytes.Contains(got.body, testPNG) || bytes.Contains(got.body, []byte("sha256")) {
								t.Errorf("the refusal carries the file: %s", got)
							}
						}
					}
					fetchDenials := f.denials(t, who)[before:]
					if len(fetchDenials) != 2*len(fetchable) {
						t.Fatalf("fetch denials = %+v, want one for each file on each of the two routes", fetchDenials)
					}
					for _, e := range fetchDenials {
						if !strings.HasSuffix(e.Detail, "reason="+denyAgentFiles) {
							t.Errorf("fetch denial = %+v, want reason %s", e, denyAgentFiles)
						}
					}
				})
			}
		}
	}
}

// What is not an agent is as it was: a person uploads any type and has no
// budget, a person outside the channel is told it does not exist, and a guest
// is refused. An agent with no manifest at all, or uploading to a channel
// that does not exist, gets the 403 of every agent without the grant.
func TestAgentUploadGrantLeavesEveryoneElseAsTheyWere(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.setAgentUploadsPerHour(1)
	for i := range 3 {
		if res := f.upload(t, "ann", fmt.Sprintf("notes-%d.txt", i), "text/plain", fmt.Appendf(nil, "notes %d", i)); res.status != http.StatusCreated {
			t.Fatalf("a person's upload %d = %s, want 201", i, res)
		}
	}
	if res := f.upload(t, "ann", "anim.gif", "image/gif", testGIF); res.status != http.StatusCreated {
		t.Errorf("a person's gif = %s, want 201", res)
	}
	if res := f.upload(t, "ann", "not-really.png", schema.MediaTypePNG, []byte("plain text under an image's name")); res.status != http.StatusCreated {
		t.Errorf("a person's mislabelled file = %s, want 201: people's uploads are not sniffed", res)
	}
	if res := f.upload(t, "nora", "a.txt", "text/plain", []byte("x")); res.status != http.StatusNotFound {
		t.Errorf("a person outside the channel = %s, want 404", res)
	}

	g := newGuest(t, f.srv, "", time.Hour, 24*time.Hour, f.ops.ID)
	var e schema.Error
	res := f.doToken(t, "POST", uploadPath("ops", "shot.png"), g.Token, schema.MediaTypePNG, testPNG)
	if res.status != http.StatusForbidden || json.Unmarshal(res.body, &e) != nil || e.Code != schema.ErrorCodeGuestNotAllowed {
		t.Errorf("a guest's upload = %s, want 403 %s", res, schema.ErrorCodeGuestNotAllowed)
	}

	rows := f.countFiles(t)
	f.grant(t, "aria")
	// ghost is an agent in ops that nobody has written a manifest for.
	f.add(t, "ghost", store.PrincipalAgent, true)
	tests := []struct {
		name   string
		who    string
		path   string
		reason string
	}{
		{"a channel that does not exist", "aria", uploadPath("nowhere", "shot.png"), denyNotMember},
		{"no manifest", "ghost", uploadPath("ops", "shot.png"), denyNoManifest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.do(t, "POST", tt.path, tt.who, schema.MediaTypePNG, bytes.NewReader(testPNG))
			var e schema.Error
			if res.status != http.StatusForbidden || json.Unmarshal(res.body, &e) != nil || e.Code != errForbidden.Code || e.Message != agentFilesRefusal {
				t.Fatalf("upload = %s, want the 403 %q", res, agentFilesRefusal)
			}
			d := f.denials(t, tt.who)
			if len(d) != 1 || d[0].Detail != fmt.Sprintf("capability=%s target=none reason=%s", schema.CapabilityFilesUpload, tt.reason) {
				t.Errorf("denials = %+v, want one with reason %s", d, tt.reason)
			}
		})
	}
	if got := f.countFiles(t); got != rows {
		t.Errorf("refused uploads made %d file row(s)", got-rows)
	}
}

// What a granted agent may send to the upload route: three image types, each
// by its declared type and its first bytes. Everything else is refused before
// a byte is kept.
func TestAgentRESTUploadMediaTypes(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		data        []byte
		wantStatus  int
		wantCode    string
	}{
		{"png", "image/png", testPNG, 201, ""},
		{"jpeg", "image/jpeg", testJPEG, 201, ""},
		{"webp", "image/webp", testWebP, 201, ""},
		{"a parameter on the type", "image/png; charset=binary", testPNG, 201, ""},
		{"gif", "image/gif", testGIF, 415, schema.ErrorCodeUnsupportedMediaType},
		{"text", "text/plain", []byte("hello"), 415, schema.ErrorCodeUnsupportedMediaType},
		{"svg", "image/svg+xml", []byte("<svg/>"), 415, schema.ErrorCodeUnsupportedMediaType},
		{"no declared type", "", testPNG, 415, schema.ErrorCodeUnsupportedMediaType},
		{"octet-stream", "application/octet-stream", testPNG, 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared png, jpeg bytes", "image/png", testJPEG, 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared jpeg, png bytes", "image/jpeg", testPNG, 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared webp, gif bytes", "image/webp", testGIF, 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared png, text bytes", "image/png", []byte("<script>alert(1)</script>"), 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared png, no bytes", "image/png", nil, 415, schema.ErrorCodeUnsupportedMediaType},
		{"declared png, too short to tell", "image/png", []byte("\x89PN"), 415, schema.ErrorCodeUnsupportedMediaType},
		{"over the size limit", "image/png", pngOfSize(testUploadLimit+1, 'x'), 413, "file_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			f.srv.setAgentUploadsPerHour(1)
			f.grant(t, "aria")
			res := f.upload(t, "aria", "shot", tt.contentType, tt.data)
			if res.status != tt.wantStatus {
				t.Fatalf("upload = %s, want %d", res, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusCreated {
				f.assertDisk(t, tt.data)
				return
			}
			var e schema.Error
			if err := json.Unmarshal(res.body, &e); err != nil || e.Code != tt.wantCode {
				t.Errorf("body = %s, want code %q", res.body, tt.wantCode)
			}
			if n := f.countFiles(t); n != 0 {
				t.Errorf("%d file row(s) after a refused upload", n)
			}
			f.assertDisk(t)
			// A refused upload is not counted: the one file of the budget
			// is still there.
			if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, testPNG); res.status != http.StatusCreated {
				t.Errorf("an upload after the refused one = %s, want 201: the refusal spent the budget", res)
			}
		})
	}
}

// The budget over REST: so many files an hour for each agent, a 429 with the
// usual shape past it, audited, and a server that gives agents no budget
// takes no upload from them. An upload that fails before its bytes are in
// place is given back.
func TestAgentRESTUploadBudget(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.setAgentUploadsPerHour(2)
	f.grant(t, "aria")
	f.grant(t, "arlo")
	for i := range 2 {
		if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, byte('a'+i))); res.status != http.StatusCreated {
			t.Fatalf("upload %d = %s", i, res)
		}
	}
	rows := f.countFiles(t)
	res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'z'))
	var e schema.Error
	if res.status != http.StatusTooManyRequests || json.Unmarshal(res.body, &e) != nil || e.Code != schema.ErrorCodeRateLimited {
		t.Fatalf("the third upload = %s, want 429 %s", res, schema.ErrorCodeRateLimited)
	}
	// Two an hour is one every thirty minutes.
	if e.RetryAfterSeconds != 1800 || res.header.Get("Retry-After") != "1800" {
		t.Errorf("retry_after_seconds = %d, Retry-After = %q; want 1800", e.RetryAfterSeconds, res.header.Get("Retry-After"))
	}
	if got := f.countFiles(t); got != rows {
		t.Errorf("the refused upload made %d file row(s)", got-rows)
	}
	var limited []store.AuditEvent
	for _, ev := range f.audit(t) {
		if ev.Action == store.AuditAgentRateLimited {
			limited = append(limited, ev)
		}
	}
	if len(limited) != 1 || limited[0].Actor != fmt.Sprintf("principal:%d", f.p("aria").ID) ||
		!strings.Contains(limited[0].Detail, "capability=files.upload") || !strings.Contains(limited[0].Detail, "max=2") {
		t.Errorf("agent_rate_limited rows = %+v", limited)
	}
	// Another agent has its own budget, and a person has none.
	if res := f.upload(t, "arlo", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'r')); res.status != http.StatusCreated {
		t.Errorf("another agent's upload = %s", res)
	}
	if res := f.upload(t, "ann", "a.txt", "text/plain", []byte("a person")); res.status != http.StatusCreated {
		t.Errorf("a person's upload = %s", res)
	}
	f.clock.Advance(30 * time.Minute)
	if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'y')); res.status != http.StatusCreated {
		t.Errorf("an upload after half the window = %s", res)
	}

	// An upload whose bytes could not be put in place is not counted.
	f.clock.Advance(30 * time.Minute)
	f.srv.renameFile = func(_, _ string) error { return fmt.Errorf("injected failure") }
	if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'f')); res.status != http.StatusInternalServerError {
		t.Fatalf("an upload that cannot be placed = %s, want 500", res)
	}
	f.srv.renameFile = os.Rename
	if res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'g')); res.status != http.StatusCreated {
		t.Errorf("the upload after a failed one = %s: the failure spent the budget", res)
	}

	// No budget at all: agents' uploads are off, and only theirs.
	f.srv.setAgentUploadsPerHour(0)
	res = f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, 'h'))
	if res.status != http.StatusForbidden || json.Unmarshal(res.body, &e) != nil || e.Code != "uploads_disabled" {
		t.Errorf("an agent's upload with no budget = %s, want 403 uploads_disabled", res)
	}
	if res := f.upload(t, "ann", "b.txt", "text/plain", []byte("still a person")); res.status != http.StatusCreated {
		t.Errorf("a person's upload with the agents' budget at zero = %s", res)
	}
}

// The manifest's own rate limit for files.upload is spent too, once a call,
// and a call it refuses gives the upload budget back.
func TestAgentUploadManifestRateLimit(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.setAgentUploadsPerHour(2)
	req := schema.PutAgentManifestRequestV1{DisplayName: "aria", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels: []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionAttach}}},
		RateLimits: []schema.RateLimit{{Capability: schema.CapabilityFilesUpload, Max: 1, WindowSeconds: 60}}}
	if _, _, err := f.srv.store.PutAgentManifest(t.Context(), "system", f.p("aria").ID, req); err != nil {
		t.Fatal(err)
	}
	upload := func(tag byte) int {
		return f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, tag)).status
	}
	if got := upload('a'); got != http.StatusCreated {
		t.Fatalf("the first upload = %d", got)
	}
	if got := upload('b'); got != http.StatusTooManyRequests {
		t.Fatalf("the second upload inside the manifest's window = %d, want 429", got)
	}
	// The manifest's window has passed and the budget has refilled almost
	// nothing: the second file of the budget is there only because the
	// refused call gave it back.
	f.clock.Advance(time.Minute)
	if got := upload('c'); got != http.StatusCreated {
		t.Fatalf("the upload after the manifest's window = %d: the refused call kept its file of the budget", got)
	}
	f.clock.Advance(time.Minute)
	if got := upload('d'); got != http.StatusTooManyRequests {
		t.Errorf("a third file in the hour = %d, want 429 from the budget", got)
	}
}

// An agent attaches a file it uploaded over REST as a person does: by naming
// it in the payload of a post, over REST or over MCP. The upload is audited
// with the agent as its actor.
func TestAgentAttachesItsOwnRESTUpload(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.grant(t, "aria")
	upload := func(tag byte) schema.FileV1 {
		res := f.upload(t, "aria", "shot.png", schema.MediaTypePNG, pngOfSize(64, tag))
		var meta schema.FileV1
		if res.status != http.StatusCreated || json.Unmarshal(res.body, &meta) != nil {
			t.Fatalf("upload = %s", res)
		}
		return meta
	}
	overREST, overMCP := upload('a'), upload('b')
	actor := fmt.Sprintf("principal:%d", f.p("aria").ID)
	for _, meta := range []schema.FileV1{overREST, overMCP} {
		found := false
		for _, e := range f.audit(t) {
			if e.Action == store.AuditFileUploaded && e.Subject == fmt.Sprintf("file:%d", meta.ID) {
				found = true
				if e.Actor != actor || !strings.Contains(e.Detail, "sha256="+meta.SHA256) || strings.Contains(e.Detail, "shot.png") {
					t.Errorf("file_uploaded = %+v, want the agent as actor and no file name", e)
				}
			}
		}
		if !found {
			t.Errorf("no file_uploaded row for file %d", meta.ID)
		}
	}
	if res := f.do(t, "POST", "/v2/channels/ops/messages", "aria", "application/json", strings.NewReader(attachBody("over rest", "", overREST.ID))); res.status != http.StatusCreated {
		t.Errorf("the agent attaching its upload over REST = %s", res)
	}
	payload := map[string]any{"schema": schema.FileAttachmentV1Name, "data": map[string]any{"file_ids": []int64{overMCP.ID}}}
	if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "over mcp", "payload": payload}); out.isError {
		t.Errorf("the agent attaching its upload over MCP = %s (%s)", out.result(), out.body)
	}
	for _, meta := range []schema.FileV1{overREST, overMCP} {
		if got := f.fetch(t, "wes", meta.ID); got.status != http.StatusOK {
			t.Errorf("a member fetching the attached file %d = %s", meta.ID, got)
		}
	}
}

// post_message with attachments: the server uploads the images and posts one
// message that carries them. The files are the agent's, audited as its
// uploads, and a member reads them as any attached file.
func TestMCPPostMessageWithAttachments(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.grant(t, "aria")
	messages := f.messageCount(t)

	msg, ids := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(
		attachment("panther.png", schema.MediaTypePNG, testPNG),
		attachment("panther.jpg", schema.MediaTypeJPEG, testJPEG),
		attachment("panther.webp", schema.MediaTypeWebP, testWebP),
	)))
	if msg.AuthorID != f.p("aria").ID || msg.ChannelID != f.ops.ID || msg.Body != "progress" || msg.Audience != nil {
		t.Errorf("the posted message = %+v", msg)
	}
	if got := f.messageCount(t); got != messages+1 {
		t.Errorf("one call stored %d messages, want 1", got-messages)
	}
	want := []struct {
		name, mediaType string
		data            []byte
	}{
		{"panther.png", schema.MediaTypePNG, testPNG},
		{"panther.jpg", schema.MediaTypeJPEG, testJPEG},
		{"panther.webp", schema.MediaTypeWebP, testWebP},
	}
	if len(ids) != len(want) || f.countFiles(t) != len(want) {
		t.Fatalf("the message attaches %v and there are %d file rows, want %d of each", ids, f.countFiles(t), len(want))
	}
	actor := fmt.Sprintf("principal:%d", f.p("aria").ID)
	audit := f.audit(t)
	for i, w := range want {
		// In the order they were given.
		meta := f.fileMeta(t, "wes", ids[i])
		if meta.Filename != w.name || meta.MediaType != w.mediaType || meta.Size != int64(len(w.data)) || meta.SHA256 != sumHex(w.data) ||
			meta.UploaderID != f.p("aria").ID || meta.ChannelID != f.ops.ID {
			t.Errorf("file %d = %+v, want %s %s", i, meta, w.name, w.mediaType)
		}
		got := f.fetch(t, "wes", ids[i])
		if got.status != http.StatusOK || !bytes.Equal(got.body, w.data) {
			t.Errorf("a member fetching %s = %d, %d bytes", w.name, got.status, len(got.body))
		}
		if ct := got.header.Get("Content-Type"); ct != schema.FileDownloadMediaType || got.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s is served as %q; an agent's image is never rendered by the server either", w.name, ct)
		}
		// The agent cannot read back what it posted.
		if res := f.fetch(t, "aria", ids[i]); res.status != http.StatusForbidden {
			t.Errorf("the agent fetching its own attachment = %s, want 403", res)
		}
		subject := fmt.Sprintf("file:%d", ids[i])
		var uploaded, attached int
		for _, e := range audit {
			if e.Subject != subject {
				continue
			}
			switch e.Action {
			case store.AuditFileUploaded:
				uploaded++
				if e.Actor != actor || !strings.Contains(e.Detail, "sha256="+meta.SHA256) || !strings.Contains(e.Detail, fmt.Sprintf("channel=%d", f.ops.ID)) ||
					strings.Contains(e.Detail, w.name) {
					t.Errorf("file_uploaded = %+v, want the agent as actor, the hash, and no file name", e)
				}
			case store.AuditFileAttached:
				attached++
				if e.Actor != actor || e.Detail != fmt.Sprintf("message=%d", msg.ID) {
					t.Errorf("file_attached = %+v", e)
				}
			}
		}
		if uploaded != 1 || attached != 1 {
			t.Errorf("%s has %d file_uploaded and %d file_attached rows, want one of each", w.name, uploaded, attached)
		}
	}
	f.assertDisk(t, testPNG, testJPEG, testWebP)
	if d := f.denials(t, "aria"); len(d) != 3 {
		// Only the three fetches above.
		t.Errorf("denials = %+v", d)
	}

	// The same bytes again are stored once and are another file.
	_, again := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(attachment("same.png", schema.MediaTypePNG, testPNG))))
	if len(again) != 1 || again[0] == ids[0] || f.countFiles(t) != len(want)+1 {
		t.Errorf("posting the same bytes again attaches %v with %d file rows", again, f.countFiles(t))
	}
	f.assertDisk(t, testPNG, testJPEG, testWebP)
}

// Attachments go where the message goes: to a net, to a whisper, or into a
// thread, and only those who can see the message can fetch the images.
func TestMCPPostMessageWithAttachmentsToAnAudience(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.grant(t, "aria", schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	root := f.mustPost(t, "ann", `{"body":"how is it going?"}`)
	tests := []struct {
		name   string
		tag    byte
		args   map[string]any
		sees   string
		cannot string
	}{
		{"to a net", 'n', map[string]any{"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}}, "nina", "wes"},
		{"a whisper", 'w', map[string]any{"audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.p("ann").ID}}}, "ann", "wes"},
		{"a reply", 'r', map[string]any{"thread_root_id": root}, "wes", "nora"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := pngOfSize(64, tt.tag)
			args := postWith(attachment("shot.png", schema.MediaTypePNG, data))
			for k, v := range tt.args {
				args[k] = v
			}
			msg, ids := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", args))
			if len(ids) != 1 {
				t.Fatalf("the message attaches %v", ids)
			}
			if tt.args["audience"] != nil && msg.Audience == nil {
				t.Errorf("the message has no audience: %+v", msg)
			}
			if tt.args["thread_root_id"] != nil && msg.ThreadRootID != root {
				t.Errorf("the message replies to %d, want %d", msg.ThreadRootID, root)
			}
			if got := f.fetch(t, tt.sees, ids[0]); got.status != http.StatusOK || !bytes.Equal(got.body, data) {
				t.Errorf("%s, who can see the message, fetching the image = %d", tt.sees, got.status)
			}
			if got := f.fetch(t, tt.cannot, ids[0]); got.status != http.StatusNotFound {
				t.Errorf("%s, who cannot see the message, fetching the image = %s, want 404", tt.cannot, got)
			}
		})
	}
}

// Every way a post_message with attachments is refused, and what it leaves:
// no file row, no message, no file on disk, no upload audit row.
func TestMCPPostMessageAttachmentRefusals(t *testing.T) {
	png := attachment("shot.png", schema.MediaTypePNG, testPNG)
	half := testUploadLimit/2 + 1
	otherPayload := map[string]any{"schema": "test.thing.v1", "data": map[string]any{"k": "v"}}
	tests := []struct {
		name string
		who  string
		// setup changes the fixture, in which aria and arlo hold the whole
		// grant and nomad holds it without being in the channel.
		setup func(t *testing.T, f *agentUploadFixture)
		args  map[string]any
		// argsFor is args for a case that needs an id of the fixture.
		argsFor  func(f *agentUploadFixture) map[string]any
		wantCode string
		// wantDenial is the reason of the one access_denied row, or "" for
		// a refusal that is not a denial.
		wantDenial string
	}{
		{name: "one item over the size limit", args: postWith(attachment("big.png", schema.MediaTypePNG, pngOfSize(testUploadLimit+1, 'x'))), wantCode: "file_too_large"},
		{name: "the items together over the size limit", args: postWith(
			attachment("a.png", schema.MediaTypePNG, pngOfSize(half, 'a')),
			attachment("b.png", schema.MediaTypePNG, pngOfSize(half, 'b'))), wantCode: "file_too_large"},
		{name: "four items together over the size limit", args: postWith(
			attachment("a.png", schema.MediaTypePNG, pngOfSize(testUploadLimit/4, 'a')),
			attachment("b.png", schema.MediaTypePNG, pngOfSize(testUploadLimit/4, 'b')),
			attachment("c.png", schema.MediaTypePNG, pngOfSize(testUploadLimit/4, 'c')),
			attachment("d.png", schema.MediaTypePNG, pngOfSize(testUploadLimit/4+1, 'd'))), wantCode: "file_too_large"},
		{name: "a gif", args: postWith(attachment("anim.gif", "image/gif", testGIF)), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "text", args: postWith(attachment("notes.txt", "text/plain", []byte("hello"))), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "a media type with a parameter", args: postWith(attachment("shot.png", "image/png; charset=binary", testPNG)), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "declared png, jpeg bytes", args: postWith(attachment("shot.png", schema.MediaTypePNG, testJPEG)), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "declared jpeg, text bytes", args: postWith(attachment("shot.jpg", schema.MediaTypeJPEG, []byte("<script>alert(1)</script>"))), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "declared png, no bytes", args: postWith(attachment("shot.png", schema.MediaTypePNG, nil)), wantCode: schema.ErrorCodeUnsupportedMediaType},
		{name: "more than four items", args: postWith(png, png, png, png, png), wantCode: "invalid_request"},
		{name: "attachments with a payload", args: map[string]any{"channel": "ops", "body": "x", "attachments": []map[string]any{png}, "payload": otherPayload}, wantCode: "invalid_request"},
		{name: "data that is not base64", args: postWith(map[string]any{"name": "shot.png", "media_type": schema.MediaTypePNG, "data_base64": "not base64!"}), wantCode: "invalid_request"},
		{name: "url-safe base64", args: postWith(map[string]any{"name": "shot.png", "media_type": schema.MediaTypePNG, "data_base64": base64.URLEncoding.EncodeToString(append(append([]byte{}, testPNG...), 0xfb, 0xff))}), wantCode: "invalid_request"},
		{name: "a name with a path", args: postWith(attachment("../shot.png", schema.MediaTypePNG, testPNG)), wantCode: "invalid_filename"},
		{name: "no name", args: postWith(attachment("", schema.MediaTypePNG, testPNG)), wantCode: "invalid_filename"},
		{name: "no capability", setup: func(t *testing.T, f *agentUploadFixture) {
			f.manifest(t, "aria", []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
				schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionAttach)
		}, args: postWith(png), wantCode: errForbidden.Code, wantDenial: denyCapability},
		{name: "no attach permission", setup: func(t *testing.T, f *agentUploadFixture) {
			f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
		}, args: postWith(png), wantCode: errForbidden.Code, wantDenial: denyChannelPermission},
		{name: "not a member", who: "nomad", args: postWith(png), wantCode: errChannelNotFound.Code, wantDenial: denyNotMember},
		{name: "a channel that does not exist", args: map[string]any{"channel": "nowhere", "body": "x", "attachments": []map[string]any{png}},
			wantCode: errChannelNotFound.Code, wantDenial: denyNotMember},
		{name: "attach without the permission to post", setup: func(t *testing.T, f *agentUploadFixture) {
			f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionAttach)
		}, args: postWith(png), wantCode: errForbidden.Code, wantDenial: denyChannelPermission},
		{name: "a net the agent is not on", who: "arlo", setup: func(t *testing.T, f *agentUploadFixture) {
			f.grant(t, "arlo", schema.ChannelPermissionPostNet)
		}, argsFor: func(f *agentUploadFixture) map[string]any {
			return map[string]any{"channel": "ops", "body": "x", "attachments": []map[string]any{png},
				"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}}
		}, wantCode: "net_not_found", wantDenial: denyNetNotOn},
		{name: "a thread that does not exist", args: map[string]any{"channel": "ops", "body": "x", "attachments": []map[string]any{png}, "thread_root_id": 999999},
			wantCode: errThreadNotFound.Code},
		{name: "uploads off", setup: func(_ *testing.T, f *agentUploadFixture) { f.srv.cfg.MaxUploadBytes = 0 }, args: postWith(png), wantCode: "uploads_disabled"},
		{name: "no budget for agents", setup: func(_ *testing.T, f *agentUploadFixture) { f.srv.setAgentUploadsPerHour(0) }, args: postWith(png), wantCode: "uploads_disabled"},
		{name: "more items than the budget has", setup: func(_ *testing.T, f *agentUploadFixture) { f.srv.setAgentUploadsPerHour(2) },
			args: postWith(png, png, png), wantCode: schema.ErrorCodeRateLimited},
		{name: "the bytes cannot be put in place", setup: func(_ *testing.T, f *agentUploadFixture) {
			f.srv.renameFile = func(_, _ string) error { return fmt.Errorf("injected failure") }
		}, args: postWith(png), wantCode: errInternal.Code},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			for _, agent := range []string{"aria", "arlo", "nomad"} {
				f.grant(t, agent)
			}
			// Enough for exactly one call of the most a call may carry.
			f.srv.setAgentUploadsPerHour(schema.MaxAgentAttachments)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			who := tt.who
			if who == "" {
				who = "aria"
			}
			args := tt.args
			if tt.argsFor != nil {
				args = tt.argsFor(f)
			}
			messages := f.messageCount(t)
			out := f.mcpCall(t, f.p(who).token, "post_message", args)
			if out.status != http.StatusOK || !out.isError || out.code != tt.wantCode {
				t.Fatalf("post_message = %s, want the tool error %s (%s)", out.result(), tt.wantCode, out.body)
			}
			f.assertNothingKept(t, messages)
			d := f.denials(t, who)
			switch {
			case tt.wantDenial == "" && len(d) != 0:
				t.Errorf("denials = %+v, want none", d)
			case tt.wantDenial != "" && (len(d) != 1 || d[0].Subject != "mcp:post_message" || !strings.HasSuffix(d[0].Detail, "reason="+tt.wantDenial)):
				t.Errorf("denials = %+v, want one with reason %s", d, tt.wantDenial)
			}

			// Whatever refused the call, it spent none of the budget: a
			// call of four images, which is all of it, still goes through.
			f.srv.renameFile = os.Rename
			f.srv.cfg.MaxUploadBytes = testUploadLimit
			if f.srv.cfg.AgentUploadsPerHour != schema.MaxAgentAttachments {
				return
			}
			f.grant(t, "aria")
			_, ids := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(
				attachment("1.png", schema.MediaTypePNG, pngOfSize(32, '1')), attachment("2.png", schema.MediaTypePNG, pngOfSize(32, '2')),
				attachment("3.png", schema.MediaTypePNG, pngOfSize(32, '3')), attachment("4.png", schema.MediaTypePNG, pngOfSize(32, '4')))))
			if len(ids) != schema.MaxAgentAttachments {
				t.Errorf("the call after the refused one attaches %v", ids)
			}
		})
	}
}

// All or nothing: one bad item, wherever it is in the list, and nothing of
// the call is kept, the good items included.
func TestMCPPostMessageAttachmentsAreAllOrNothing(t *testing.T) {
	good := func(tag byte) map[string]any {
		return attachment(fmt.Sprintf("%c.png", tag), schema.MediaTypePNG, pngOfSize(64, tag))
	}
	bad := attachment("anim.gif", "image/gif", testGIF)
	tests := []struct {
		name  string
		items []map[string]any
	}{
		{"the second of two", []map[string]any{good('a'), bad}},
		{"the first of two", []map[string]any{bad, good('a')}},
		{"the last of four", []map[string]any{good('a'), good('b'), good('c'), bad}},
		{"the third of four", []map[string]any{good('a'), good('b'), attachment("c.png", schema.MediaTypePNG, testJPEG), good('d')}},
		{"the second of two, by its size", []map[string]any{good('a'), attachment("big.png", schema.MediaTypePNG, pngOfSize(testUploadLimit, 'x'))}},
		{"the second of two, by its name", []map[string]any{good('a'), attachment("a/b.png", schema.MediaTypePNG, testPNG)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			f.srv.setAgentUploadsPerHour(schema.MaxAgentAttachments)
			f.grant(t, "aria")
			messages := f.messageCount(t)
			out := f.mcpCall(t, f.p("aria").token, "post_message", postWith(tt.items...))
			if !out.isError {
				t.Fatalf("post_message = %s, want a refusal (%s)", out.result(), out.body)
			}
			f.assertNothingKept(t, messages)
			// The good items were not uploaded on the quiet: the whole
			// budget is still there.
			_, ids := posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(good('1'), good('2'), good('3'), good('4'))))
			if len(ids) != schema.MaxAgentAttachments {
				t.Errorf("the call after the refused one attaches %v", ids)
			}
		})
	}
}

// The budget over MCP: each file is one upload, a call that needs more than
// are left takes none, and the refusal is the rate-limit error every tool
// gives, with its delay.
func TestMCPPostMessageAttachmentBudget(t *testing.T) {
	f := newAgentUploadFixture(t)
	f.srv.setAgentUploadsPerHour(3)
	f.grant(t, "aria")
	item := func(tag byte) map[string]any {
		return attachment("shot.png", schema.MediaTypePNG, pngOfSize(64, tag))
	}
	posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('a'), item('b'))))
	messages, rows := f.messageCount(t), f.countFiles(t)

	out := f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('c'), item('d')))
	if !out.isError || out.code != schema.ErrorCodeRateLimited {
		t.Fatalf("two more with one left = %s, want %s (%s)", out.result(), schema.ErrorCodeRateLimited, out.body)
	}
	// Three an hour is one every twenty minutes; the second is a whole one away.
	if !strings.Contains(out.body, `\"retry_after_seconds\":1200`) && !strings.Contains(out.body, `"retry_after_seconds":1200`) {
		t.Errorf("the refusal does not say to retry in 1200 seconds: %s", out.body)
	}
	if f.messageCount(t) != messages || f.countFiles(t) != rows {
		t.Errorf("the refused call stored %d message(s) and %d file(s)", f.messageCount(t)-messages, f.countFiles(t)-rows)
	}
	if n := f.auditCount(t, store.AuditAgentRateLimited); n != 1 {
		t.Errorf("%d agent_rate_limited rows, want 1", n)
	}
	// The refused call took nothing, so the one that was left still is.
	posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('e'))))
	if out := f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('f'))); !out.isError || out.code != schema.ErrorCodeRateLimited {
		t.Errorf("a fourth file in the hour = %s, want %s", out.result(), schema.ErrorCodeRateLimited)
	}
	// A post without attachments is another matter entirely.
	if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "words only"}); out.isError {
		t.Errorf("a post with no attachments, with the upload budget used up = %s", out.result())
	}
	f.clock.Advance(20 * time.Minute)
	posted(t, f.mcpCall(t, f.p("aria").token, "post_message", postWith(item('g'))))
}

// mcpPostRaw sends one post_message call whose JSON-RPC body the test wrote,
// and reports the HTTP status and the outcome.
func mcpPostRaw(t *testing.T, srv *Server, token string, body []byte, chunked bool) mcpOutcome {
	t.Helper()
	var reader io.Reader = bytes.NewReader(body)
	if chunked {
		// A reader httptest cannot size: the request has no Content-Length.
		reader = io.MultiReader(bytes.NewReader(body))
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	srv.Handler().ServeHTTP(rec, req)
	out := mcpOutcome{status: rec.Code, body: rec.Body.String()}
	if rec.Code != http.StatusOK {
		return out
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	if out.isError = response.Result.IsError; out.isError && len(response.Result.Content) > 0 {
		var serr schema.Error
		if json.Unmarshal([]byte(response.Result.Content[0].Text), &serr) == nil {
			out.code = serr.Code
		}
	}
	return out
}

func toolCall(t *testing.T, tool string, args map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The request cap of /mcp. An agent whose manifest grants files.upload may
// send a post_message of about 2 MiB when the bulk of it is image data; every
// other caller, and every other request of that agent, is held to 1 MiB as
// before.
func TestMCPRequestCapForAttachments(t *testing.T) {
	const uploadLimit = 4 << 20
	big := pngOfSize(3<<19, 'p') // 1.5 MiB, 2 MiB in base64
	text := strings.Repeat("a", 2<<20)
	withImage := postWith(attachment("screen.png", schema.MediaTypePNG, big))
	tests := []struct {
		name string
		who  string
		body func(t *testing.T) []byte
		// chunked sends the request with no Content-Length.
		chunked    bool
		wantStatus int
		wantCode   string // the tool error, or "" for a post that is stored
	}{
		{name: "a granted agent, 2 MiB of image", who: "aria", body: func(t *testing.T) []byte { return toolCall(t, "post_message", withImage) }, wantStatus: 200},
		{name: "a granted agent, 2 MiB of image, no Content-Length", who: "aria", chunked: true,
			body: func(t *testing.T) []byte { return toolCall(t, "post_message", withImage) }, wantStatus: 200},
		{name: "an agent without files.upload, the same request", who: "arlo", body: func(t *testing.T) []byte { return toolCall(t, "post_message", withImage) }, wantStatus: 413},
		{name: "an agent without files.upload, no Content-Length", who: "arlo", chunked: true,
			body: func(t *testing.T) []byte { return toolCall(t, "post_message", withImage) }, wantStatus: 413},
		{name: "an agent with no manifest", who: "ghost", body: func(t *testing.T) []byte { return toolCall(t, "post_message", withImage) }, wantStatus: 413},
		{name: "a granted agent, 2 MiB of text", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", map[string]any{"channel": "ops", "body": text})
		}, wantStatus: 413},
		{name: "a granted agent, 2 MiB of payload", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", map[string]any{"channel": "ops", "body": "x", "payload": map[string]any{"schema": "test.thing.v1", "data": text}})
		}, wantStatus: 413},
		{name: "a granted agent, an image and 2 MiB of text", who: "aria", body: func(t *testing.T) []byte {
			args := postWith(attachment("screen.png", schema.MediaTypePNG, testPNG))
			args["body"] = text
			return toolCall(t, "post_message", args)
		}, wantStatus: 413},
		{name: "a granted agent, 2 MiB in an attachment's name", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", postWith(map[string]any{"name": text, "media_type": schema.MediaTypePNG, "data_base64": "AAAA"}))
		}, wantStatus: 413},
		{name: "a granted agent, another tool", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "read_channel", map[string]any{"channel": "ops", "attachments": []map[string]any{attachment("screen.png", schema.MediaTypePNG, big)}})
		}, wantStatus: 413},
		{name: "a granted agent, a batch", who: "aria", body: func(t *testing.T) []byte {
			return append(append([]byte("["), toolCall(t, "post_message", withImage)...), ']')
		}, wantStatus: 413},
		{name: "a granted agent, the image in a first arguments and the text in a second", who: "aria", body: func(t *testing.T) []byte {
			return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"attachments":[{"data_base64":%q}]},"arguments":{"channel":"ops","body":%q}}}`,
				base64.StdEncoding.EncodeToString(big), text)
		}, wantStatus: 413},
		{name: "a granted agent, more image than one upload may be", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", postWith(attachment("huge.png", schema.MediaTypePNG, pngOfSize(uploadLimit+(1<<20), 'h'))))
		}, wantStatus: 413},
		{name: "a granted agent, one byte more image than one upload may be", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", postWith(attachment("over.png", schema.MediaTypePNG, pngOfSize(uploadLimit+1, 'o'))))
		}, wantStatus: 200, wantCode: "file_too_large"},
		{name: "a granted agent, as much image as one upload may be", who: "aria", body: func(t *testing.T) []byte {
			return toolCall(t, "post_message", postWith(attachment("full.png", schema.MediaTypePNG, pngOfSize(uploadLimit, 'f'))))
		}, wantStatus: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			f.srv.cfg.MaxUploadBytes = uploadLimit
			f.grant(t, "aria")
			// arlo may do everything but upload; ghost is an agent in ops
			// that nobody has written a manifest for.
			f.manifest(t, "arlo", []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
				schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionAttach)
			f.add(t, "ghost", store.PrincipalAgent, true)
			messages := f.messageCount(t)
			body := tt.body(t)
			if len(body) <= schema.MaxPostRequestBytes {
				t.Fatalf("the request is %d bytes; this test is about requests over %d", len(body), schema.MaxPostRequestBytes)
			}
			out := mcpPostRaw(t, f.srv, f.p(tt.who).token, body, tt.chunked)
			if out.status != tt.wantStatus || out.code != tt.wantCode {
				t.Fatalf("a %d-byte request = http %d, tool error %q; want http %d, %q (%.300s)", len(body), out.status, out.code, tt.wantStatus, tt.wantCode, out.body)
			}
			if tt.wantStatus == http.StatusOK && tt.wantCode == "" {
				if got := f.messageCount(t); got != messages+1 || f.countFiles(t) != 1 {
					t.Errorf("the post stored %d message(s) and %d file(s), want one of each", got-messages, f.countFiles(t))
				}
				return
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				var e schema.Error
				if err := json.Unmarshal([]byte(out.body), &e); err != nil || e.Code != "request_too_large" {
					t.Errorf("the 413 body = %s, want request_too_large", out.body)
				}
			}
			f.assertNothingKept(t, messages)
		})
	}
}

// With uploads off for agents nobody has the larger cap, whatever the
// manifest grants.
func TestMCPRequestCapIsTheOldOneWhenAgentsCannotUpload(t *testing.T) {
	tests := []struct {
		name  string
		apply func(f *agentUploadFixture)
	}{
		{"uploads off", func(f *agentUploadFixture) { f.srv.cfg.MaxUploadBytes = 0 }},
		{"no budget for agents", func(f *agentUploadFixture) { f.srv.setAgentUploadsPerHour(0) }},
		{"no data directory", func(f *agentUploadFixture) { f.srv.cfg.DataDir = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentUploadFixture(t)
			f.srv.cfg.MaxUploadBytes = 4 << 20
			f.grant(t, "aria")
			tt.apply(f)
			body := toolCall(t, "post_message", postWith(attachment("screen.png", schema.MediaTypePNG, pngOfSize(3<<19, 'p'))))
			if out := mcpPostRaw(t, f.srv, f.p("aria").token, body, false); out.status != http.StatusRequestEntityTooLarge {
				t.Errorf("a %d-byte request = http %d, want 413", len(body), out.status)
			}
		})
	}
}

func TestMCPOversizeIsAttachments(t *testing.T) {
	data := strings.Repeat("A", 2<<20)
	over := strings.Repeat("b", schema.MaxPostRequestBytes)
	call := func(format string, args ...any) []byte {
		return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+format+`}`, args...)
	}
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{"a post with one large attachment", call(`{"name":"post_message","arguments":{"channel":"ops","body":"x","attachments":[{"name":"a.png","media_type":"image/png","data_base64":%q}]}}`, data), true},
		{"a post with two", call(`{"name":"post_message","arguments":{"attachments":[{"data_base64":%q},{"data_base64":%q}]}}`, data, data), true},
		{"the rest of the request is over the bound", call(`{"name":"post_message","arguments":{"body":%q,"attachments":[{"data_base64":%q}]}}`, over, data), false},
		{"the data is under another key", call(`{"name":"post_message","arguments":{"attachments":[{"data":%q}]}}`, data), false},
		{"the data is the body", call(`{"name":"post_message","arguments":{"body":%q}}`, data), false},
		{"another tool", call(`{"name":"request_approval","arguments":{"attachments":[{"data_base64":%q}]}}`, data), false},
		{"no tool", call(`{"arguments":{"attachments":[{"data_base64":%q}]}}`, data), false},
		{"another method", fmt.Appendf(nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"name":"post_message","arguments":{"attachments":[{"data_base64":%q}]}}}`, data), false},
		{"a batch", append(append([]byte("["), call(`{"name":"post_message","arguments":{"attachments":[{"data_base64":%q}]}}`, data)...), ']'), false},
		{"not json", []byte(data), false},
		{"attachments given twice, the large one first", call(`{"name":"post_message","arguments":{"attachments":[{"data_base64":%q}],"attachments":[]}}`, data), false},
		{"data given twice in one item", call(`{"name":"post_message","arguments":{"attachments":[{"data_base64":%q,"data_base64":"AAAA"}]}}`, data), false},
		{"arguments given twice, the text in the second", call(`{"name":"post_message","arguments":{"attachments":[{"data_base64":%q}]},"arguments":{"body":%q}}`, data, over), false},
		// Only the bytes as they stand in the request are let through: a
		// string that decoding would lengthen is counted at its own length.
		{"data of bytes that are not text", call(`{"name":"post_message","arguments":{"body":%q,"attachments":[{"data_base64":"%s"}]}}`,
			strings.Repeat("b", schema.MaxPostRequestBytes/2), strings.Repeat("\xff", 1<<20)), true},
		{"data of bytes that are not text, and the rest over the bound", call(`{"name":"post_message","arguments":{"body":%q,"attachments":[{"data_base64":"%s"}]}}`,
			over, strings.Repeat("\xff", 1<<20)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.body) <= schema.MaxPostRequestBytes {
				t.Fatalf("the request is only %d bytes", len(tt.body))
			}
			if got := mcpOversizeIsAttachments(tt.body); got != tt.want {
				t.Errorf("mcpOversizeIsAttachments = %v, want %v", got, tt.want)
			}
		})
	}
}

// The tool says what it takes: the definition agents read names the grant,
// the types, the caps and the all-or-nothing rule.
func TestPostMessageToolDescribesAttachments(t *testing.T) {
	f := newAgentUploadFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+f.p("aria").token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list = %d %s", rec.Code, rec.Body)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Description string `json:"description"`
						Items       struct {
							Required   []string                   `json:"required"`
							Properties map[string]json.RawMessage `json:"properties"`
						} `json:"items"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, tool := range response.Result.Tools {
		if tool.Name != "post_message" {
			continue
		}
		att, ok := tool.InputSchema.Properties["attachments"]
		if !ok {
			t.Fatalf("post_message has no attachments argument: %+v", tool.InputSchema.Properties)
		}
		for _, required := range tool.InputSchema.Required {
			if required == "attachments" {
				t.Error("attachments is required; it must be optional")
			}
		}
		for _, field := range []string{"name", "media_type", "data_base64"} {
			if _, ok := att.Items.Properties[field]; !ok {
				t.Errorf("an attachment has no %s", field)
			}
		}
		if len(att.Items.Required) != 3 {
			t.Errorf("an attachment requires %v, want its three fields", att.Items.Required)
		}
		for where, text := range map[string]string{"the argument": att.Description, "the tool": tool.Description} {
			for _, say := range []string{"files.upload", "attach", "image/png", "image/jpeg", "image/webp", fmt.Sprint(schema.MaxAgentAttachments)} {
				if !strings.Contains(text, say) {
					t.Errorf("the description of %s does not mention %q: %s", where, say, text)
				}
			}
		}
		for _, say := range []string{"All or nothing", "upload limit", "payload", "budget"} {
			if !strings.Contains(att.Description, say) {
				t.Errorf("the description of the argument does not mention %q: %s", say, att.Description)
			}
		}
		return
	}
	t.Fatal("tools/list has no post_message")
}
