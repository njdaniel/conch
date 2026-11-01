package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// bigJS is a compressible asset over the compression threshold.
var bigJS = strings.Repeat("console.log('conch');\n", 400)

func gzFixture() fstest.MapFS {
	m := webFSFixture()
	m["assets/big.js"] = &fstest.MapFile{Data: []byte(bigJS)}
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
					if tt.path == "/app/assets/big.js" && string(plain) != bigJS {
						t.Error("decompressed body differs from the file")
					}
					if len(raw) >= len(plain) {
						t.Errorf("compressed %d >= plain %d", len(raw), len(plain))
					}
					if resp.Header.Get("Content-Length") != fmt.Sprint(len(raw)) {
						t.Errorf("Content-Length %q != %d", resp.Header.Get("Content-Length"), len(raw))
					}
				}
				if !tt.wantGzip && tt.method == "GET" && tt.rng == "" && tt.path == "/app/assets/big.js" && string(raw) != bigJS {
					t.Error("plain body differs from the file")
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
