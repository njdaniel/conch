package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const testUploadLimit = 1 << 16

// fileFixture is the scoped fixture with uploads switched on and a real
// listener, so bodies stream the way they do in production.
type fileFixture struct {
	*scopedFixture
	url string // http://host of the listener
}

func newFileFixture(t *testing.T) *fileFixture {
	t.Helper()
	f := &fileFixture{scopedFixture: newScopedFixture(t)}
	f.srv.cfg.MaxUploadBytes = testUploadLimit
	f.url = "http" + strings.TrimPrefix(f.base, "ws")
	return f
}

type fileResp struct {
	status int
	header http.Header
	body   []byte
}

func (r fileResp) String() string { return fmt.Sprintf("%d %s", r.status, r.body) }

// do sends a request as who ("" for nobody). A nil body is an empty one.
func (f *fileFixture) do(t *testing.T, method, path, who, contentType string, body io.Reader) fileResp {
	t.Helper()
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequest(method, f.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+f.p(who).token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- a test-local listener
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fileResp{resp.StatusCode, resp.Header, b}
}

func uploadPath(channel, name string) string {
	return "/v2/channels/" + channel + "/files?filename=" + url.QueryEscape(name)
}

func (f *fileFixture) upload(t *testing.T, who, name, contentType string, data []byte) fileResp {
	t.Helper()
	return f.do(t, "POST", uploadPath("ops", name), who, contentType, bytes.NewReader(data))
}

func (f *fileFixture) mustUpload(t *testing.T, who, name string, data []byte) schema.FileV1 {
	t.Helper()
	res := f.upload(t, who, name, "application/octet-stream", data)
	if res.status != http.StatusCreated {
		t.Fatalf("%s upload %q = %s", who, name, res)
	}
	var meta schema.FileV1
	if err := json.Unmarshal(res.body, &meta); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := meta.Validate(); err != nil {
		t.Fatalf("response is not a valid conch.file.v1: %v: %s", err, res.body)
	}
	return meta
}

func attachBody(text, audience string, ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	if audience != "" {
		audience = "," + audience
	}
	return fmt.Sprintf(`{"body":%q,"payload":{"schema":"conch.file_attachment.v1","data":{"file_ids":[%s]}}%s}`,
		text, strings.Join(parts, ","), audience)
}

func (f *fileFixture) fetch(t *testing.T, who string, id int64) fileResp {
	t.Helper()
	return f.do(t, "GET", fmt.Sprintf("/v2/files/%d", id), who, "", nil)
}

func (f *fileFixture) meta(t *testing.T, who string, id int64) fileResp {
	t.Helper()
	return f.do(t, "GET", fmt.Sprintf("/v2/files/%d/meta", id), who, "", nil)
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// regularFiles lists, slash-separated and relative to the data directory,
// every regular file under it. The database is elsewhere in these tests, so
// whatever is listed was written by the upload path.
func (f *fileFixture) regularFiles(t *testing.T) []string {
	t.Helper()
	root := f.srv.cfg.DataDir
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// assertDisk fails unless the data directory holds exactly one file per given
// content, each at files/<sha256[:2]>/<sha256>, and nothing else: no temporary
// file, and nothing created anywhere a caller's input could have pointed.
func (f *fileFixture) assertDisk(t *testing.T, contents ...[]byte) {
	t.Helper()
	want := []string{}
	seen := map[string]bool{}
	for _, c := range contents {
		h := sumHex(c)
		if !seen[h] {
			seen[h] = true
			want = append(want, "files/"+h[:2]+"/"+h)
		}
	}
	sort.Strings(want)
	got := f.regularFiles(t)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("data directory holds\n%s\nwant exactly\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, rel := range want {
		b, err := os.ReadFile(filepath.Join(f.srv.cfg.DataDir, filepath.FromSlash(rel))) // #nosec G304 -- a test path
		if err != nil {
			t.Fatal(err)
		}
		if sumHex(b) != filepath.Base(rel) {
			t.Errorf("%s does not hold the bytes its name says", rel)
		}
	}
}

func (f *fileFixture) countFiles(t *testing.T) int {
	t.Helper()
	n, err := f.srv.store.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFileUploadAndDownloadRoundTrip(t *testing.T) {
	f := newFileFixture(t)
	data := []byte("the plan, byte for byte\x00\xff\n")
	meta := f.mustUpload(t, "ann", "plan.pdf", data)
	if meta.Size != int64(len(data)) || meta.SHA256 != sumHex(data) || meta.Filename != "plan.pdf" ||
		meta.ChannelID != f.ops.ID || meta.UploaderID != f.p("ann").ID {
		t.Fatalf("metadata = %+v", meta)
	}
	f.assertDisk(t, data)

	// Unattached: the uploader reads it, a fellow member does not.
	if got := f.fetch(t, "ann", meta.ID); got.status != 200 || !bytes.Equal(got.body, data) {
		t.Fatalf("uploader fetch = %s", got)
	}
	if got := f.fetch(t, "olga", meta.ID); got.status != 404 {
		t.Fatalf("member fetched an unattached file: %s", got)
	}

	// Attach it to a channel-wide message; now every member reads it.
	if res := f.postV2(t, "ann", attachBody("the plan", "", meta.ID)); res.status != 201 {
		t.Fatalf("attach = %d %s", res.status, res.body)
	}
	for _, who := range []string{"ann", "olga", "root"} {
		if got := f.fetch(t, who, meta.ID); got.status != 200 || !bytes.Equal(got.body, data) {
			t.Errorf("%s fetch = %s", who, got)
		}
		var m schema.FileV1
		if res := f.meta(t, who, meta.ID); res.status != 200 || json.Unmarshal(res.body, &m) != nil || m != meta {
			t.Errorf("%s meta = %s, want %+v", who, res, meta)
		}
	}

	// Audit: one row for the upload (no filename) and one for the attachment.
	var uploaded, attached []store.AuditEvent
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditFileUploaded:
			uploaded = append(uploaded, e)
		case store.AuditFileAttached:
			attached = append(attached, e)
		}
	}
	subject := fmt.Sprintf("file:%d", meta.ID)
	if len(uploaded) != 1 || uploaded[0].Subject != subject || uploaded[0].Actor != fmt.Sprintf("principal:%d", f.p("ann").ID) ||
		!strings.Contains(uploaded[0].Detail, "sha256="+meta.SHA256) || !strings.Contains(uploaded[0].Detail, fmt.Sprintf("size=%d", len(data))) ||
		strings.Contains(uploaded[0].Detail, "plan.pdf") {
		t.Errorf("file_uploaded audit = %+v", uploaded)
	}
	if len(attached) != 1 || attached[0].Subject != subject || !strings.HasPrefix(attached[0].Detail, "message=") {
		t.Errorf("file_attached audit = %+v", attached)
	}
}

func TestFileUploadRefusals(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		limit      int64
		wantStatus int
		wantCode   string
	}{
		{"unauthenticated", "", testUploadLimit, 401, "unauthenticated"},
		{"agent that is a member", "aria", testUploadLimit, 403, "forbidden"},
		{"agent outside the channel", "nomad", testUploadLimit, 403, "forbidden"},
		{"human outside the channel", "nora", testUploadLimit, 404, "channel_not_found"},
		{"uploads disabled", "ann", 0, 403, "uploads_disabled"},
		{"negative limit", "ann", -1, 403, "uploads_disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFileFixture(t)
			f.srv.cfg.MaxUploadBytes = tt.limit
			res := f.upload(t, tt.who, "a.txt", "text/plain", []byte("hello"))
			if res.status != tt.wantStatus {
				t.Fatalf("status = %s, want %d", res, tt.wantStatus)
			}
			var e schema.Error
			if err := json.Unmarshal(res.body, &e); err != nil || e.Code != tt.wantCode {
				t.Errorf("body = %s, want code %q", res.body, tt.wantCode)
			}
			if n := f.countFiles(t); n != 0 {
				t.Errorf("%d file rows after a refused upload", n)
			}
			f.assertDisk(t)
		})
	}
}

func TestFileUploadUnknownChannelIsTheMembersAnswer(t *testing.T) {
	f := newFileFixture(t)
	nowhere := f.do(t, "POST", uploadPath("nowhere", "a.txt"), "ann", "", strings.NewReader("x"))
	outsider := f.do(t, "POST", uploadPath("ops", "a.txt"), "nora", "", strings.NewReader("x"))
	if nowhere.status != 404 || !bytes.Equal(nowhere.body, outsider.body) {
		t.Errorf("unknown channel %s, non-member %s", nowhere, outsider)
	}
}

func TestFileHostileNames(t *testing.T) {
	long := strings.Repeat("a", 256)
	tests := []struct {
		name     string
		filename string
		ok       bool
	}{
		{"parent traversal", "../x", false},
		{"deep traversal", "../../../etc/passwd", false},
		{"absolute path", "/etc/passwd", false},
		{"backslash", `a\b`, false},
		{"windows drive", `C:\x`, false},
		{"newline", "a\nb", false},
		{"carriage return", "a\rb", false},
		{"NUL", "a\x00b", false},
		{"escape sequence", "a\x1b[2Jb", false},
		{"256 bytes", long, false},
		{"invalid UTF-8", "a\xffb", false},
		{"dot", ".", false},
		{"dotdot", "..", false},
		{"empty", "", false},
		{"bidi override", "a\u202eb", false},
		{"255 bytes", long[:255], true},
		{"leading dot", ".hidden", true},
		{"dots inside", "a..b", true},
		{"unicode", "héllo wörld.txt", true},
		{"quotes and spaces", `a "b" c.txt`, true},
	}
	f := newFileFixture(t)
	data := []byte("content that must land under its hash and nowhere else")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.countFiles(t)
			res := f.upload(t, "ann", tt.filename, "", data)
			if tt.ok {
				if res.status != 201 {
					t.Fatalf("status = %s, want 201", res)
				}
				f.assertDisk(t, data)
				return
			}
			var e schema.Error
			if res.status != 400 || json.Unmarshal(res.body, &e) != nil || e.Code != "invalid_filename" {
				t.Fatalf("status = %s, want 400 invalid_filename", res)
			}
			if f.countFiles(t) != before {
				t.Error("a refused name left a row")
			}
			// Nothing was written for a refused name, and whatever earlier
			// names were accepted left only the hash-named file.
			files := f.regularFiles(t)
			if len(files) > 1 {
				t.Errorf("files on disk: %v", files)
			}
		})
	}
	// The name is never a path: one blob, however many accepted names.
	f.assertDisk(t, data)

	// Two filename parameters, or none, are refused too.
	for _, q := range []string{"", "?filename=a&filename=b"} {
		res := f.do(t, "POST", "/v2/channels/ops/files"+q, "ann", "", strings.NewReader("x"))
		if res.status != 400 {
			t.Errorf("query %q: %s", q, res)
		}
	}
}

func TestFileUploadLimits(t *testing.T) {
	f := newFileFixture(t)
	f.srv.cfg.MaxUploadBytes = 1000
	ok := bytes.Repeat([]byte("a"), 1000)
	over := bytes.Repeat([]byte("a"), 1001)

	t.Run("exactly at the limit is stored", func(t *testing.T) {
		if res := f.upload(t, "ann", "ok.bin", "", ok); res.status != 201 {
			t.Fatalf("= %s", res)
		}
		f.assertDisk(t, ok)
	})
	startFiles, startRows := f.regularFiles(t), f.countFiles(t)
	untouched := func(t *testing.T) {
		t.Helper()
		if got := f.regularFiles(t); strings.Join(got, "\n") != strings.Join(startFiles, "\n") {
			t.Errorf("files on disk = %v, want %v", got, startFiles)
		}
		if f.countFiles(t) != startRows {
			t.Error("a row was recorded")
		}
	}

	t.Run("one byte over", func(t *testing.T) {
		res := f.upload(t, "ann", "big.bin", "", over)
		if res.status != 413 {
			t.Fatalf("= %s", res)
		}
		untouched(t)
	})
	t.Run("declared length over the limit is refused unread", func(t *testing.T) {
		raw := f.rawConn(t)
		_, _ = fmt.Fprintf(raw, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 1001\r\n\r\nabc",
			uploadPath("ops", "lie.bin"), f.p("ann").token)
		status := rawStatus(t, raw)
		if status != 413 {
			t.Fatalf("status = %d", status)
		}
		untouched(t)
	})
	t.Run("a length that lies low is held to what was declared", func(t *testing.T) {
		// 10 bytes are declared and the rest of the stream is not part of
		// this request: the file is those 10 bytes, never more.
		raw := f.rawConn(t)
		_, _ = fmt.Fprintf(raw, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 10\r\n\r\n%s",
			uploadPath("ops", "short.bin"), f.p("ann").token, strings.Repeat("b", 2000))
		if status := rawStatus(t, raw); status != 201 {
			t.Fatalf("status = %d", status)
		}
		var sizes []int64
		for _, rel := range f.regularFiles(t) {
			st, err := os.Stat(filepath.Join(f.srv.cfg.DataDir, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, st.Size())
		}
		sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
		if len(sizes) != 2 || sizes[0] != 10 || sizes[1] != 1000 {
			t.Errorf("stored sizes = %v, want [10 1000]", sizes)
		}
		startFiles, startRows = f.regularFiles(t), f.countFiles(t)
	})
	t.Run("chunked body with no length", func(t *testing.T) {
		raw := f.rawConn(t)
		_, _ = fmt.Fprintf(raw, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\n\r\n",
			uploadPath("ops", "chunked.bin"), f.p("ann").token)
		for sent := 0; sent < 3000; sent += 500 {
			if _, err := fmt.Fprintf(raw, "%x\r\n%s\r\n", 500, strings.Repeat("c", 500)); err != nil {
				break // the server already answered and closed
			}
		}
		_, _ = fmt.Fprint(raw, "0\r\n\r\n")
		if status := rawStatus(t, raw); status != 413 {
			t.Fatalf("status = %d", status)
		}
		untouched(t)
	})
	t.Run("connection dropped mid-upload", func(t *testing.T) {
		raw := f.rawConn(t)
		_, _ = fmt.Fprintf(raw, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 900\r\n\r\n%s",
			uploadPath("ops", "drop.bin"), f.p("ann").token, strings.Repeat("d", 100))
		// Wait until the server has begun writing, so that the cleanup below
		// is the thing under test and not a race with its start.
		waitFor(t, "a temporary upload file", func() bool { return countTemps(t, f.srv.filesDir()) > 0 })
		_ = raw.Close()
		waitFor(t, "the temporary upload file to be removed", func() bool { return countTemps(t, f.srv.filesDir()) == 0 })
		untouched(t)
	})
}

func (f *fileFixture) rawConn(t *testing.T) net.Conn {
	t.Helper()
	u, err := url.Parse(f.url)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

func rawStatus(t *testing.T, c net.Conn) int {
	t.Helper()
	var status int
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < 4096 && !bytes.Contains(buf, []byte("\r\n")) {
		if _, err := c.Read(one); err != nil {
			t.Fatalf("read status line: %v (have %q)", err, buf)
		}
		buf = append(buf, one...)
	}
	if _, err := fmt.Sscanf(string(buf), "HTTP/1.1 %d", &status); err != nil {
		t.Fatalf("status line %q: %v", buf, err)
	}
	return status
}

func countTemps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), uploadTempPrefix) {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFileSameBytesTwiceMakesTwoRowsAndOneFile(t *testing.T) {
	f := newFileFixture(t)
	data := []byte("identical")
	a := f.mustUpload(t, "ann", "one.txt", data)
	b := f.mustUpload(t, "nina", "two.txt", data)
	if a.ID == b.ID || a.SHA256 != b.SHA256 {
		t.Fatalf("rows %+v %+v", a, b)
	}
	if f.countFiles(t) != 2 {
		t.Errorf("rows = %d, want 2", f.countFiles(t))
	}
	f.assertDisk(t, data)
}

func TestFileDownloadIsNeverRendered(t *testing.T) {
	f := newFileFixture(t)
	tests := []struct {
		name        string
		contentType string
		filename    string
		wantMedia   string
	}{
		{"html", "text/html", "page.html", "text/html"},
		{"html with charset", "text/html; charset=utf-8", "page2.html", "text/html"},
		{"svg", "image/svg+xml", "pic.svg", "image/svg+xml"},
		{"no content type", "", "bare", "application/octet-stream"},
		{"garbage content type", "not a type", "junk", "application/octet-stream"},
		{"javascript", "application/javascript", "x.js", "application/javascript"},
		{"awkward name", "text/plain", "héllo \"quoted\" ; name.txt", "text/plain"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf("<script>alert(%d)</script>", i))
			res := f.upload(t, "ann", tt.filename, tt.contentType, data)
			if res.status != 201 {
				t.Fatalf("upload = %s", res)
			}
			var meta schema.FileV1
			if err := json.Unmarshal(res.body, &meta); err != nil || meta.MediaType != tt.wantMedia {
				t.Fatalf("media_type = %q (%v), want %q", meta.MediaType, err, tt.wantMedia)
			}
			got := f.fetch(t, "ann", meta.ID)
			if got.status != 200 || !bytes.Equal(got.body, data) {
				t.Fatalf("fetch = %s", got)
			}
			if ct := got.header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("Content-Type = %q", ct)
			}
			if v := got.header.Get("X-Content-Type-Options"); v != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", v)
			}
			disp, params, err := mime.ParseMediaType(got.header.Get("Content-Disposition"))
			if err != nil || disp != "attachment" || params["filename"] != tt.filename {
				t.Errorf("Content-Disposition = %q (%v); want attachment with filename %q", got.header.Get("Content-Disposition"), err, tt.filename)
			}
			if cl := got.header.Get("Content-Length"); cl != fmt.Sprint(len(data)) {
				t.Errorf("Content-Length = %q", cl)
			}
		})
	}
}

// Everyone who may not fetch a file is told what a file that does not exist is
// told, byte for byte.
func TestFileVisibilityMatrix(t *testing.T) {
	f := newFileFixture(t)
	net1 := netAudience(f.net1.ID)
	whisper := whisperAudience(f.p("nina").ID)

	type stored struct {
		meta schema.FileV1
		data []byte
	}
	post := func(label, audience string) stored {
		data := []byte("secret bytes of " + label)
		meta := f.mustUpload(t, "ann", label+".txt", data)
		if res := f.postV2(t, "ann", attachBody(label, audience, meta.ID)); res.status != 201 {
			t.Fatalf("attach %s = %d %s", label, res.status, res.body)
		}
		return stored{meta, data}
	}
	files := map[string]stored{"wide": post("wide", ""), "net": post("net", net1), "whisper": post("whisper", whisper)}
	unattachedData := []byte("never attached")
	files["unattached"] = stored{f.mustUpload(t, "ann", "loose.txt", unattachedData), unattachedData}

	// who -> file -> may read. Absent means false.
	type access = map[string]bool
	tests := []struct {
		who  string
		want access
	}{
		{"ann", access{"wide": true, "net": true, "whisper": true, "unattached": true}},
		{"nina", access{"wide": true, "net": true, "whisper": true}},
		{"dina", access{"wide": true, "net": true}},
		{"mona", access{"wide": true, "net": true}},
		{"aria", access{"wide": true, "net": true}}, // a member agent: refused below, never given a file
		{"olga", access{"wide": true}},
		{"wes", access{"wide": true}},
		{"root", access{"wide": true}},
		{"nora", access{}},
	}
	check := func(who string, wantFor func(string) bool) {
		t.Helper()
		ref := f.fetch(t, who, 999999)
		refMeta := f.meta(t, who, 999999)
		if ref.status != 404 || refMeta.status != 404 {
			t.Fatalf("%s: reference answers %s / %s", who, ref, refMeta)
		}
		for name, file := range files {
			content, meta := f.fetch(t, who, file.meta.ID), f.meta(t, who, file.meta.ID)
			if wantFor(name) {
				if content.status != 200 || !bytes.Equal(content.body, file.data) {
					t.Errorf("%s fetching %s: %s", who, name, content)
				}
				if meta.status != 200 {
					t.Errorf("%s meta of %s: %s", who, name, meta)
				}
				continue
			}
			for _, pair := range []struct {
				got fileResp
				ref fileResp
			}{{content, ref}, {meta, refMeta}} {
				if pair.got.status != 404 || !bytes.Equal(pair.got.body, pair.ref.body) ||
					pair.got.header.Get("Content-Type") != pair.ref.header.Get("Content-Type") ||
					pair.got.header.Get("Content-Length") != pair.ref.header.Get("Content-Length") {
					t.Errorf("%s fetching %s: %s differs from the answer for a file that does not exist: %s", who, name, pair.got, pair.ref)
				}
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			if tt.who == "aria" {
				// An agent is refused on every file route, for any file.
				for _, file := range files {
					for _, res := range []fileResp{f.fetch(t, "aria", file.meta.ID), f.meta(t, "aria", file.meta.ID)} {
						if res.status != 403 {
							t.Errorf("agent got %s", res)
						}
					}
				}
				return
			}
			check(tt.who, func(name string) bool { return tt.want[name] })
		})
	}
	t.Run("nobody", func(t *testing.T) {
		for _, file := range files {
			if res := f.do(t, "GET", fmt.Sprintf("/v2/files/%d", file.meta.ID), "", "", nil); res.status != 401 {
				t.Errorf("unauthenticated = %s", res)
			}
		}
	})
	t.Run("ids that are not ids", func(t *testing.T) {
		ref := f.fetch(t, "ann", 999999)
		for _, id := range []string{"abc", "0", "-1", "1.5", "9223372036854775808", "%2e%2e", "1%2f2"} {
			got := f.do(t, "GET", "/v2/files/"+id, "ann", "", nil)
			if got.status != 404 || !bytes.Equal(got.body, ref.body) {
				t.Errorf("id %q: %s", id, got)
			}
		}
	})

	// Leaving the channel closes every file that was reachable through it.
	ctx := context.Background()
	for _, who := range []string{"nina", "dina", "ann", "root"} {
		if removed, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.p(who).ID); err != nil || !removed {
			t.Fatalf("remove %s: %v %v", who, removed, err)
		}
		t.Run("removed "+who, func(t *testing.T) {
			check(who, func(string) bool { return false })
		})
	}
	// The channel-wide file stays readable to members who remain.
	if res := f.fetch(t, "olga", files["wide"].meta.ID); res.status != 200 {
		t.Errorf("olga after removals = %s", res)
	}
}

func TestFileAttachmentIsRefusedWhole(t *testing.T) {
	f := newFileFixture(t)
	ctx := context.Background()
	other, err := f.srv.store.CreateChannel(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"ann", "nina"} {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", other.ID, f.p(who).ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	mine := f.mustUpload(t, "ann", "mine.txt", []byte("mine"))
	mine2 := f.mustUpload(t, "ann", "mine2.txt", []byte("mine two"))
	theirs := f.mustUpload(t, "nina", "theirs.txt", []byte("theirs"))
	elsewhereRes := f.do(t, "POST", uploadPath("other", "x.txt"), "ann", "", strings.NewReader("elsewhere"))
	var elsewhere schema.FileV1
	if err := json.Unmarshal(elsewhereRes.body, &elsewhere); err != nil || elsewhereRes.status != 201 {
		t.Fatalf("upload to other = %s", elsewhereRes)
	}
	used := f.mustUpload(t, "ann", "used.txt", []byte("used"))
	if res := f.postV2(t, "ann", attachBody("first", "", used.ID)); res.status != 201 {
		t.Fatalf("first attach = %d %s", res.status, res.body)
	}

	over := make([]int64, schema.MaxFileAttachments+1)
	for i := range over {
		over[i] = int64(1000 + i)
	}
	tests := []struct {
		name     string
		body     string
		audience bool
	}{
		{"unknown id", attachBody("x", "", 99999), false},
		{"someone else's file", attachBody("x", "", theirs.ID), false},
		{"another channel's file", attachBody("x", "", elsewhere.ID), false},
		{"already attached", attachBody("x", "", used.ID), false},
		{"one good, one bad", attachBody("x", "", mine.ID, 99999), false},
		{"one good, one someone else's", attachBody("x", "", mine.ID, theirs.ID), false},
		{"eleven ids", attachBody("x", "", over...), false},
		{"no ids", attachBody("x", ""), false},
		{"duplicate ids", attachBody("x", "", mine.ID, mine.ID), false},
		{"zero id", attachBody("x", "", 0), false},
		{"unknown field", `{"body":"x","payload":{"schema":"conch.file_attachment.v1","data":{"file_ids":[1],"extra":1}}}`, false},
		{"not an object", `{"body":"x","payload":{"schema":"conch.file_attachment.v1","data":[1]}}`, false},
		{"net post, bad file", attachBody("x", netAudience(f.net1.ID), theirs.ID), true},
		{"whisper, bad file", attachBody("x", whisperAudience(f.p("nina").ID), 99999), true},
	}
	var first []byte
	before, err := f.srv.store.CountMessages(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.postV2(t, "ann", tt.body)
			var e schema.Error
			if res.status != 400 || json.Unmarshal([]byte(res.body), &e) != nil || e.Code != "invalid_file_attachment" {
				t.Fatalf("= %d %s, want 400 invalid_file_attachment", res.status, res.body)
			}
			if first == nil {
				first = []byte(res.body)
			} else if string(first) != res.body {
				t.Errorf("answer differs between failures:\n%s\n%s", first, res.body)
			}
		})
	}
	if after, _ := f.srv.store.CountMessages(ctx, f.ops.ID); after != before {
		t.Errorf("messages in channel went from %d to %d: a refused post stored something", before, after)
	}
	// Nothing was attached: a fellow member still cannot read "mine".
	if res := f.fetch(t, "olga", mine.ID); res.status != 404 {
		t.Errorf("a refused post attached the file: %s", res)
	}
	// And a good attachment of two files works, once.
	if res := f.postV2(t, "ann", attachBody("both", "", mine.ID, mine2.ID)); res.status != 201 {
		t.Fatalf("attach two = %d %s", res.status, res.body)
	}
	if res := f.postV2(t, "ann", attachBody("again", "", mine.ID)); res.status != 400 {
		t.Errorf("a second attach of one file = %d, want 400", res.status)
	}
	for _, id := range []int64{mine.ID, mine2.ID} {
		if res := f.fetch(t, "olga", id); res.status != 200 {
			t.Errorf("olga fetch %d = %s", id, res)
		}
	}
}

// The v1 post route and the unscoped v2 route share the insert, so the same
// rule holds there.
func TestFileAttachmentOnV1Route(t *testing.T) {
	f := newFileFixture(t)
	mine := f.mustUpload(t, "ann", "mine.txt", []byte("mine"))
	body := fmt.Sprintf(`{"body":"x","payload":{"schema":"conch.file_attachment.v1","data":{"file_ids":[%d]}}}`, mine.ID)
	if res := f.callREST(t, "POST", "/v1/channels/ops/messages", f.p("nina").token, body); res.status != 400 {
		t.Errorf("another member attaching ann's file via v1 = %d %s", res.status, res.body)
	}
	if res := f.callREST(t, "POST", "/v1/channels/ops/messages", f.p("ann").token, body); res.status != 201 {
		t.Errorf("uploader attaching via v1 = %d %s", res.status, res.body)
	}
}

// Two posts racing for one file: the database decides, and exactly one wins.
func TestFileConcurrentAttachExactlyOneWins(t *testing.T) {
	f := newFileFixture(t)
	for round := 0; round < 15; round++ {
		meta := f.mustUpload(t, "ann", "race.txt", []byte(fmt.Sprintf("race %d", round)))
		const racers = 4
		statuses := make([]int, racers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				audience := ""
				if i%2 == 1 {
					audience = netAudience(f.net1.ID)
				}
				statuses[i] = f.postV2(t, "ann", attachBody(fmt.Sprintf("racer %d", i), audience, meta.ID)).status
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		for _, s := range statuses {
			switch s {
			case 201:
				created++
			case 400:
			default:
				t.Fatalf("round %d: statuses %v", round, statuses)
			}
		}
		if created != 1 {
			t.Fatalf("round %d: %d posts attached one file, statuses %v", round, created, statuses)
		}
	}
}

// A failure between the temporary file and the final name leaves no row and no
// file; an upload interrupted by a crash leaves a temporary file that the next
// start removes.
func TestFileCrashSafety(t *testing.T) {
	t.Run("failed rename", func(t *testing.T) {
		f := newFileFixture(t)
		f.srv.renameFile = func(_, _ string) error { return fmt.Errorf("injected failure") }
		res := f.upload(t, "ann", "a.txt", "", []byte("doomed"))
		if res.status != 500 {
			t.Fatalf("= %s", res)
		}
		if f.countFiles(t) != 0 {
			t.Error("a row exists for a file that was never placed")
		}
		f.assertDisk(t)
		// The server recovers: with the fault gone the same upload works.
		f.srv.renameFile = os.Rename
		f.mustUpload(t, "ann", "a.txt", []byte("doomed"))
		f.assertDisk(t, []byte("doomed"))
	})
	t.Run("start removes leftover temporary files", func(t *testing.T) {
		f := newFileFixture(t)
		data := []byte("keep me")
		f.mustUpload(t, "ann", "keep.txt", data)
		dir := f.srv.filesDir()
		stale := filepath.Join(dir, uploadTempPrefix+"123456")
		other := filepath.Join(dir, "not-a-temp-file")
		for _, p := range []string{stale, other} {
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		New(Config{DataDir: f.srv.cfg.DataDir, Listen: "127.0.0.1:0"}, f.srv.store)
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale temporary file survived start: %v", err)
		}
		if _, err := os.Stat(other); err != nil {
			t.Errorf("start removed a file that is not a temporary upload: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, sumHex(data)[:2], sumHex(data))); err != nil {
			t.Errorf("start removed a stored file: %v", err)
		}
	})
}

func TestFileUploadsDisabledStillServesExistingFiles(t *testing.T) {
	f := newFileFixture(t)
	data := []byte("before the switch")
	meta := f.mustUpload(t, "ann", "a.txt", data)
	f.srv.cfg.MaxUploadBytes = 0
	var e schema.Error
	res := f.upload(t, "ann", "b.txt", "", []byte("after"))
	if res.status != 403 || json.Unmarshal(res.body, &e) != nil || e.Code != "uploads_disabled" {
		t.Fatalf("upload = %s", res)
	}
	if got := f.fetch(t, "ann", meta.ID); got.status != 200 || !bytes.Equal(got.body, data) {
		t.Errorf("fetch with uploads disabled = %s", got)
	}
}

func TestFileRoutesRefuseAgents(t *testing.T) {
	f := newFileFixture(t)
	meta := f.mustUpload(t, "ann", "a.txt", []byte("x"))
	for _, who := range []string{"aria", "arlo", "nomad"} {
		for _, req := range []struct{ method, path string }{
			{"POST", uploadPath("ops", "a.txt")},
			{"GET", fmt.Sprintf("/v2/files/%d", meta.ID)},
			{"GET", fmt.Sprintf("/v2/files/%d/meta", meta.ID)},
		} {
			res := f.do(t, req.method, req.path, who, "", strings.NewReader("x"))
			if res.status != 403 {
				t.Errorf("%s %s %s = %s", who, req.method, req.path, res)
			}
		}
	}
}
