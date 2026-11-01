package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/coder/websocket"
)

// bigJS is a compressible asset over the compression threshold.
var bigJS = strings.Repeat("console.log('conch');\n", 400)

// largeJS compresses to well over net/http's write buffer, so a response
// carries Content-Length only if the handler sets it.
var largeJS = func() string {
	var b strings.Builder
	for i := range 40000 {
		fmt.Fprintf(&b, "export const v%d = %d;\n", i, i*7919%100003)
	}
	return b.String()
}()

// noiseJS is a .js file gzip cannot shrink.
var noiseJS = func() []byte {
	b := make([]byte, 8192)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}()

func gzFixture() fstest.MapFS {
	m := webFSFixture()
	m["assets/big.js"] = &fstest.MapFile{Data: []byte(bigJS)}
	m["assets/large.js"] = &fstest.MapFile{Data: []byte(largeJS)}
	m["assets/noise.js"] = &fstest.MapFile{Data: noiseJS}
	m["assets/pic.png"] = &fstest.MapFile{Data: []byte(strings.Repeat("P", 4000))}
	m["index.html"] = &fstest.MapFile{Data: []byte("<html>" + strings.Repeat("index ", 400) + "</html>")}
	return m
}

// gzSources serves gzFixture from the embedded-FS path and from --web-dir.
var gzSources = []struct {
	name   string
	server func(t *testing.T) *httptest.Server
}{
	{"embedded", func(t *testing.T) *httptest.Server { return webFSServer(t, AuthRequired, gzFixture()) }},
	{"dir", func(t *testing.T) *httptest.Server {
		dir := t.TempDir()
		for name, f := range gzFixture() {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, f.Data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return webServer(t, AuthRequired, dir)
	}},
}

func TestStaticGzip(t *testing.T) {
	tests := []struct {
		name, path, method, acceptEnc, rng string
		wantGzip                           bool
		vary                               bool
	}{
		{"js with gzip", "/app/assets/big.js", "GET", "gzip", "", true, true},
		{"js among others", "/app/assets/big.js", "GET", "br, gzip;q=0.8", "", true, true},
		{"wildcard", "/app/assets/big.js", "GET", "*", "", true, true},
		{"index", "/app/", "GET", "gzip", "", true, true},
		{"HEAD", "/app/assets/big.js", "HEAD", "gzip", "", true, true},
		{"no header", "/app/assets/big.js", "GET", "", "", false, true},
		{"identity only", "/app/assets/big.js", "GET", "identity", "", false, true},
		{"gzip refused by q=0", "/app/assets/big.js", "GET", "gzip;q=0", "", false, true},
		{"explicit refusal beats wildcard", "/app/assets/big.js", "GET", "gzip;q=0, *;q=1", "", false, true},
		{"wildcard before explicit refusal", "/app/assets/big.js", "GET", "*, gzip;q=0", "", false, true},
		{"refusal with tab", "/app/assets/big.js", "GET", "gzip;\tq=0", "", false, true},
		{"refusal as 0.000", "/app/assets/big.js", "GET", "gzip; q=0.000", "", false, true},
		{"malformed q refuses", "/app/assets/big.js", "GET", "gzip;q=yes", "", false, true},
		{"wildcard refused", "/app/assets/big.js", "GET", "*;q=0", "", false, true},
		{"low but non-zero q", "/app/assets/big.js", "GET", "gzip;q=0.001", "", true, true},
		{"large js has its length", "/app/assets/large.js", "GET", "gzip", "", true, true},
		{"incompressible stays plain", "/app/assets/noise.js", "GET", "gzip", "", false, true},
		{"incompressible stays plain again", "/app/assets/noise.js", "GET", "gzip", "", false, true},
		{"range is served plain", "/app/assets/big.js", "GET", "gzip", "bytes=0-9", false, true},
		{"small file stays plain", "/app/assets/x.js", "GET", "gzip", "", false, true},
		{"png is never compressed", "/app/assets/pic.png", "GET", "gzip", "", false, false},
	}
	for _, src := range gzSources {
		ts := src.server(t)
		for _, tt := range tests {
			t.Run(src.name+"/"+tt.name, func(t *testing.T) {
				req, err := http.NewRequestWithContext(context.Background(), tt.method, ts.URL+tt.path, nil)
				if err != nil {
					t.Fatal(err)
				}
				// Setting the header disables the transport's own gzip handling.
				req.Header.Set("Accept-Encoding", tt.acceptEnc)
				if tt.rng != "" {
					req.Header.Set("Range", tt.rng)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				raw, _ := io.ReadAll(resp.Body)
				if got := resp.Header.Get("Content-Encoding") == "gzip"; got != tt.wantGzip {
					t.Fatalf("gzip = %v, want %v", got, tt.wantGzip)
				}
				if got := strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding"); got != tt.vary {
					t.Errorf("Vary has Accept-Encoding = %v, want %v", got, tt.vary)
				}
				for h, want := range map[string]string{
					"Content-Security-Policy": webCSP,
					"Permissions-Policy":      webPermissionsPolicy,
					"X-Content-Type-Options":  "nosniff",
					"Referrer-Policy":         "no-referrer",
				} {
					if got := resp.Header.Get(h); got != want {
						t.Errorf("%s = %q, want %q", h, got, want)
					}
				}
				if tt.wantGzip && tt.method == "GET" {
					zr, err := gzip.NewReader(bytes.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					plain, _ := io.ReadAll(zr)
					if want, ok := map[string]string{"/app/assets/big.js": bigJS, "/app/assets/large.js": largeJS}[tt.path]; ok && string(plain) != want {
						t.Error("decompressed body differs from the file")
					}
					if len(raw) >= len(plain) {
						t.Errorf("compressed %d >= plain %d", len(raw), len(plain))
					}
					if resp.Header.Get("Content-Length") != fmt.Sprint(len(raw)) {
						t.Errorf("Content-Length %q != %d", resp.Header.Get("Content-Length"), len(raw))
					}
				}
				if !tt.wantGzip && tt.method == "GET" && tt.rng == "" {
					if want, ok := map[string]string{"/app/assets/big.js": bigJS, "/app/assets/noise.js": string(noiseJS)}[tt.path]; ok && string(raw) != want {
						t.Error("plain body differs from the file")
					}
				}
			})
		}
	}
}

// TestGzipNeverOnAPI is the BREACH guard: a client that asks for gzip gets
// no Content-Encoding from anything but the static client.
func TestGzipNeverOnAPI(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, WebFS: gzFixture()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	for _, p := range []string{"/healthz", "/v1/info", "/v1/whoami", "/v1/channels", "/nope"} {
		req, err := http.NewRequestWithContext(context.Background(), "GET", ts.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("%s: Content-Encoding = %q, want none", p, ce)
		}
	}
}

// TestStaticGzipConcurrentCold: many first requests at once for one file all
// get the file, compressed or plain, and the cache ends with one entry.
func TestStaticGzipConcurrentCold(t *testing.T) {
	for _, src := range gzSources {
		t.Run(src.name, func(t *testing.T) {
			ts := src.server(t)
			var wg sync.WaitGroup
			errs := make(chan error, 32)
			for range 32 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, err := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/app/assets/large.js", nil)
					if err != nil {
						errs <- err
						return
					}
					req.Header.Set("Accept-Encoding", "gzip")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						errs <- err
						return
					}
					defer func() { _ = resp.Body.Close() }()
					var body io.Reader = resp.Body
					if resp.Header.Get("Content-Encoding") == "gzip" {
						zr, err := gzip.NewReader(resp.Body)
						if err != nil {
							errs <- err
							return
						}
						body = zr
					}
					got, err := io.ReadAll(body)
					if err != nil {
						errs <- err
						return
					}
					if string(got) != largeJS {
						errs <- fmt.Errorf("body differs from the file (%d bytes)", len(got))
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// TestGzipNeverOnPrivateResponses is the BREACH guard on the answers that
// carry something private, as their signed-in reader gets them: a long
// message read back over REST, a compressible attachment, an MCP tool call,
// and the events socket offered permessage-deflate. None is compressed.
func TestGzipNeverOnPrivateResponses(t *testing.T) {
	f := newFileFixture(t)
	long := strings.Repeat("a private line that compresses well. ", 400)
	res := f.do(t, "POST", "/v1/channels/ops/messages", "ann", "application/json", strings.NewReader(fmt.Sprintf(`{"body":%q}`, long)))
	if res.status != http.StatusCreated {
		t.Fatalf("post = %s", res)
	}
	file := f.mustUpload(t, "ann", "notes.txt", []byte(strings.Repeat("secret notes, very compressible. ", 1500)))

	get := func(path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), "GET", f.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.p("ann").token)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd, *")
		resp, err := http.DefaultClient.Do(req) // #nosec G107 -- a test-local listener
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", path, resp.StatusCode, body)
		}
		if len(body) < 4096 {
			t.Fatalf("%s: body is %d bytes; the guard needs a large answer", path, len(body))
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("%s: Content-Encoding = %q, want none", path, ce)
		}
	}
	get("/v1/channels/ops/messages")
	get(fmt.Sprintf("/v2/files/%d", file.ID))

	// MCP: read the channel back through a tool call.
	call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_channel", "arguments": map[string]any{"channel": "ops"}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", f.url+"/mcp", bytes.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.p("aria").token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd, *")
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- a test-local listener
	if err != nil {
		t.Fatal(err)
	}
	mcpBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(mcpBody), "compresses well") {
		t.Fatalf("mcp read_channel = %d %.300s", resp.StatusCode, mcpBody)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("/mcp: Content-Encoding = %q, want none", ce)
	}

	// The events socket, offered compression by the client, declines it.
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	conn, wsResp, err := websocket.Dial(ctx, f.base+"/v1/events/ws?channel=ops", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + f.p("ann").token}},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if ext := wsResp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Errorf("events socket: Sec-WebSocket-Extensions = %q, want none", ext)
	}
}
