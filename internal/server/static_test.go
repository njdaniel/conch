package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	indexHTML  = "<html>index</html>"
	assetJS    = "console.log('x')"
	secretText = "TOP-SECRET-OUTSIDE"
	dotText    = "DOT-FILE-INSIDE"
)

// webFixture is a built client in <parent>/dist with a secret file beside it
// (outside the served directory) and symlinks, one that stays inside dist and
// two that leave it.
func webFixture(t *testing.T) (dist string) {
	t.Helper()
	parent := t.TempDir()
	dist = filepath.Join(parent, "dist")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dist, "index.html"), indexHTML)
	write(filepath.Join(dist, "assets", "x.js"), assetJS)
	write(filepath.Join(dist, "favicon.svg"), "<svg/>")
	write(filepath.Join(parent, "secret.txt"), secretText)
	// Inside the directory, but never served: names that start with a dot.
	write(filepath.Join(dist, ".env"), dotText)
	write(filepath.Join(dist, ".git", "config"), dotText)
	write(filepath.Join(dist, "assets", ".hidden.js"), dotText)
	// os.Root refuses an absolute link target even when it lands inside, so
	// the inside link is relative, as a build tool would write it.
	for link, target := range map[string]string{
		"inside.js":  filepath.Join("assets", "x.js"),
		"escape.txt": filepath.Join(parent, "secret.txt"),
		// A relative target that leaves the directory, so the refusal is
		// shown to be about leaving, not about the target being absolute.
		"escape-rel.txt": filepath.Join("..", "secret.txt"),
		"escapedir":      parent,
	} {
		if err := os.Symlink(target, filepath.Join(dist, link)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	return dist
}

func webServer(t *testing.T, mode AuthMode, dir string) *httptest.Server {
	t.Helper()
	srv := newTestServerWithConfig(t, Config{AuthMode: mode, WebDir: dir})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// noRedirect keeps the first response so a redirect can be asserted.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func fetch(t *testing.T, method, rawURL string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestStaticServing(t *testing.T) {
	// AuthRequired with no credential: the page loads before sign-in.
	ts := webServer(t, AuthRequired, webFixture(t))

	tests := []struct {
		name      string
		method    string
		path      string
		status    int
		body      string // exact body; "" skips the check
		cache     string
		location  string
		headerSet bool // expects the security headers
	}{
		{"root of app", "GET", "/app/", 200, indexHTML, "no-store", "", true},
		{"index by name", "GET", "/app/index.html", 200, indexHTML, "no-store", "", true},
		{"asset", "GET", "/app/assets/x.js", 200, assetJS, webImmutable, "", true},
		{"other file", "GET", "/app/favicon.svg", 200, "<svg/>", "no-cache", "", true},
		{"route falls back to index", "GET", "/app/channels/general", 200, indexHTML, "no-store", "", true},
		{"directory is not listed", "GET", "/app/assets/", 200, indexHTML, "no-store", "", true},
		{"missing asset is 404", "GET", "/app/assets/missing.js", 404, "", "", "", true},
		{"missing file with extension is 404", "GET", "/app/nope.css", 404, "", "", "", true},
		{"dotted last segment is 404", "GET", "/app/channels/v1.2", 404, "", "", "", true},
		{"dot in earlier segment still falls back", "GET", "/app/v1.2/general", 200, indexHTML, "no-store", "", true},
		{"symlink inside the directory", "GET", "/app/inside.js", 200, assetJS, "no-cache", "", true},
		{"HEAD", "HEAD", "/app/assets/x.js", 200, "", webImmutable, "", true},
		{"bare /app redirects to /app/", "GET", "/app", 301, "", "", "/app/", false},
		{"slash redirects to app", "GET", "/", 302, "", "", "/app/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := fetch(t, tt.method, ts.URL+tt.path)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tt.status, body)
			}
			if tt.body != "" && body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
			if tt.method == "HEAD" && body != "" {
				t.Errorf("HEAD returned a body: %q", body)
			}
			if got := resp.Header.Get("Cache-Control"); tt.cache != "" && got != tt.cache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.cache)
			}
			if tt.location != "" && resp.Header.Get("Location") != tt.location {
				t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), tt.location)
			}
			if tt.headerSet {
				for h, want := range map[string]string{
					"Content-Security-Policy": webCSP,
					"X-Content-Type-Options":  "nosniff",
					"Referrer-Policy":         "no-referrer",
				} {
					if got := resp.Header.Get(h); got != want {
						t.Errorf("%s = %q, want %q", h, got, want)
					}
				}
			}
		})
	}
}

// The policy text is part of the contract; pin it character for character.
func TestStaticCSPIsExact(t *testing.T) {
	const want = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	if webCSP != want {
		t.Errorf("webCSP = %q", webCSP)
	}
}

func TestStaticRefusesTraversalAndEscapes(t *testing.T) {
	ts := webServer(t, AuthRequired, webFixture(t))

	paths := []string{
		"/app/../secret.txt",
		"/app/assets/../../secret.txt",
		"/app/%2e%2e/secret.txt",
		"/app/%2e%2e%2fsecret.txt",
		"/app/..%2fsecret.txt",
		"/app/..%2Fsecret.txt",
		"/app/assets/%2e%2e%2f%2e%2e%2fsecret.txt",
		"/app/%2e%2e%5csecret.txt",
		"/app/..%5csecret.txt",
		"/app/%252e%252e/secret.txt",
		"/app/....//secret.txt",
		"/app/escape.txt",           // symlink to a file outside
		"/app/escape-rel.txt",       // the same, by a relative target
		"/app/escapedir/secret.txt", // symlink to the parent directory
		"/app/escapedir/dist/escape.txt",
		"/app/%00",
		"/app/assets/x.js%00.png",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			resp, body := fetch(t, "GET", ts.URL+p)
			if strings.Contains(body, secretText) {
				t.Fatalf("status %d leaked the file outside the directory", resp.StatusCode)
			}
			// Anything with a file extension must not answer 200; an
			// extensionless path may legitimately be the index.
			if resp.StatusCode == 200 && strings.HasSuffix(p, ".txt") {
				t.Errorf("status = 200 for %s", p)
			}
		})
	}
}

// TestStaticNeverServesDotFiles: a file or directory whose name starts with a
// dot is a 404 even though it is inside the directory, and is not answered
// with the index either.
func TestStaticNeverServesDotFiles(t *testing.T) {
	ts := webServer(t, AuthRequired, webFixture(t))
	for _, p := range []string{"/app/.env", "/app/.git/config", "/app/.git/", "/app/assets/.hidden.js", "/app/%2eenv", "/app/.well-known/x"} {
		t.Run(p, func(t *testing.T) {
			resp, body := fetch(t, "GET", ts.URL+p)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
			if strings.Contains(body, dotText) || strings.Contains(body, indexHTML) {
				t.Errorf("body = %q", body)
			}
		})
	}
}

func TestStaticMethods(t *testing.T) {
	ts := webServer(t, AuthOff, webFixture(t))
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(m, func(t *testing.T) {
			resp, _ := fetch(t, m, ts.URL+"/app/")
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
				t.Errorf("Allow = %q", allow)
			}
		})
	}
	// With authentication on, a method the route does not take is not an
	// exempt request at all: it needs a credential like anything unmatched.
	// Nothing is served either way.
	authed := webServer(t, AuthRequired, webFixture(t))
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(m+" with authentication on", func(t *testing.T) {
			resp, body := fetch(t, m, authed.URL+"/app/")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if strings.Contains(body, indexHTML) {
				t.Errorf("body = %q", body)
			}
		})
	}
}

func TestStaticFlagUnset(t *testing.T) {
	tests := []struct {
		name string
		mode AuthMode
		path string
		want int
	}{
		{"app 404 with auth off", AuthOff, "/app/", 404},
		{"app asset 404 with auth off", AuthOff, "/app/assets/x.js", 404},
		{"root 404 with auth off, as before", AuthOff, "/", 404},
		// Not exempt without the flag: unchanged from before this feature.
		{"app needs a credential under auth", AuthRequired, "/app/", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := webServer(t, tt.mode, "")
			resp, _ := fetch(t, "GET", ts.URL+tt.path)
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if resp.Header.Get("Content-Security-Policy") != "" {
				t.Error("CSP set on a route that does not exist")
			}
		})
	}
}

func TestCheckWebDir(t *testing.T) {
	good := webFixture(t)
	noIndex := t.TempDir()
	indexIsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(indexIsDir, "index.html"), 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		dir     string
		wantErr string
	}{
		{"ok", good, ""},
		{"missing directory", filepath.Join(noIndex, "nope"), "nope"},
		{"not a directory", file, "web dir"},
		{"no index.html", noIndex, "no index.html"},
		{"index.html is a directory", indexIsDir, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckWebDir(tt.dir)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// sampleRequestPath turns a route pattern into a concrete request.
func sampleRequestPath(pattern string) (method, p string) {
	method = http.MethodPost
	if m, rest, ok := strings.Cut(pattern, " "); ok {
		method, pattern = m, rest
	}
	pattern = strings.ReplaceAll(pattern, "{$}", "")
	for {
		i := strings.Index(pattern, "{")
		if i < 0 {
			return method, pattern
		}
		j := strings.Index(pattern[i:], "}")
		pattern = pattern[:i] + "1" + pattern[i+j+1:]
	}
}

// TestWebRoutesShadowNothing walks the real route table of a server with the
// flag set and one without: the web routes are the only addition, they are
// exempt, and every other route still dispatches to the same pattern with the
// same access class. The exemption therefore cannot reach an API or socket
// path.
func TestWebRoutesShadowNothing(t *testing.T) {
	base := newTestServerWithConfig(t, Config{AuthMode: AuthRequired})
	web := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, WebDir: webFixture(t)})

	baseByPattern := map[string]access{}
	for _, rt := range base.routes {
		baseByPattern[rt.pattern] = rt.access
	}
	added := map[string]bool{}
	for _, rt := range web.routes {
		if a, ok := baseByPattern[rt.pattern]; ok {
			if a != rt.access {
				t.Errorf("route %q changed access class from %d to %d", rt.pattern, a, rt.access)
			}
			continue
		}
		added[rt.pattern] = true
		if rt.access != accessExempt {
			t.Errorf("added route %q is not exempt", rt.pattern)
		}
	}
	if len(added) != 2 || !added["GET /app/"] || !added["GET /{$}"] {
		t.Errorf("added routes = %v, want exactly GET /app/ and GET /{$}", added)
	}
	if len(web.routes) != len(base.routes)+2 {
		t.Errorf("web server has %d routes, base %d", len(web.routes), len(base.routes))
	}

	for pattern := range baseByPattern {
		method, p := sampleRequestPath(pattern)
		req := httptest.NewRequest(method, p, nil)
		_, gotWeb := web.mux.Handler(req)
		_, gotBase := base.mux.Handler(req)
		if gotWeb != pattern || gotBase != pattern {
			t.Errorf("%s %s dispatches to %q with the flag, %q without; want %q", method, p, gotWeb, gotBase, pattern)
		}
		// Through the whole handler: the credential requirement is intact.
		if baseByPattern[pattern] != accessExempt {
			rec := httptest.NewRecorder()
			web.Handler().ServeHTTP(rec, httptest.NewRequest(method, p, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without a credential = %d, want 401", method, p, rec.Code)
			}
		}
	}
	// Every route the web patterns do match is under /app/ or exactly "/".
	for _, rt := range web.routes {
		if !added[rt.pattern] {
			continue
		}
		_, p := sampleRequestPath(rt.pattern)
		if p != "/" && !strings.HasPrefix(p, "/app/") {
			t.Errorf("web route %q reaches outside /app/", rt.pattern)
		}
	}
}

func TestParseAllowedOrigins(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"unset", "", nil, false},
		{"one", "http://localhost:5173", []string{"http://localhost:5173"}, false},
		{"two, spaces, case", " http://localhost:5173 , HTTPS://Dev.Example:8443", []string{"http://localhost:5173", "https://dev.example:8443"}, false},
		{"host only", "localhost:5173", nil, true},
		{"star", "*", nil, true},
		{"star among others", "http://a.example,*", nil, true},
		{"scheme star", "https://*", nil, true},
		{"star star", "**", nil, true},
		{"any host any port", "http://*:*", nil, true},
		{"subdomain wildcard", "https://*.example.com", nil, true},
		{"port wildcard", "http://localhost:*", nil, true},
		{"question mark", "http://localhos?:5173", nil, true},
		{"character class", "http://[a-z]ocalhost:5173", nil, true},
		{"IPv6 literal", "http://[::1]:5173", nil, true},
		{"other scheme", "ws://localhost:5173", nil, true},
		{"with a path", "http://localhost:5173/app", nil, true},
		{"trailing slash", "http://localhost:5173/", nil, true},
		{"with a query", "http://localhost:5173?x=1", nil, true},
		{"with user info", "http://u@localhost:5173", nil, true},
		{"empty entry", "http://a.example,,http://b.example", nil, true},
		{"trailing comma", "http://a.example,", nil, true},
		{"bad pattern", "http://[", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAllowedOrigins(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSocketOriginMatrix dials every socket route with a foreign Origin, with
// and without --allowed-origins.
func TestSocketOriginMatrix(t *testing.T) {
	const (
		foreign = "http://evil.example"
		devSrv  = "http://localhost:5173"
	)
	routes := []string{"/v0/ws", "/v1/ws", "/v2/ws", "/v1/voice/ws", "/v1/events/ws"}
	tests := []struct {
		name    string
		allowed []string
		origin  string // "" sends none
		wantOK  bool
	}{
		{"unset, foreign origin refused", nil, foreign, false},
		{"unset, dev origin refused", nil, devSrv, false},
		{"unset, no origin (a CLI) passes", nil, "", true},
		{"set, listed origin passes", []string{devSrv}, devSrv, true},
		{"set, same host under another name refused", []string{devSrv}, "http://127.0.0.1:5173", false},
		{"set, foreign origin still refused", []string{devSrv}, foreign, false},
		{"set, other port refused", []string{devSrv}, "http://localhost:5174", false},
		{"set, other scheme refused", []string{devSrv}, "https://localhost:5173", false},
		{"set, no origin passes", []string{devSrv}, "", true},
	}
	for _, tt := range tests {
		for _, route := range routes {
			t.Run(tt.name+" "+route, func(t *testing.T) {
				// A signed-in member, so the voice socket (which needs one)
				// reaches the upgrade like the others.
				f := newAuthFixture(t, AuthRequired)
				f.srv.cfg.AllowedOrigins = tt.allowed
				base := wsTestServer(t, f.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				opts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + f.aliceTok}}}
				if tt.origin != "" {
					opts.HTTPHeader.Set("Origin", tt.origin)
				}
				conn, resp, err := websocket.Dial(ctx, base+route+"?channel=general", opts) //nolint:bodyclose // Dial closes the body on handshake failure
				if err == nil {
					_ = conn.CloseNow()
				}
				if tt.wantOK && err != nil {
					t.Fatalf("Dial: %v (response %+v)", err, resp)
				}
				if !tt.wantOK {
					if err == nil {
						t.Fatal("Dial succeeded, want refusal")
					}
					if resp == nil || resp.StatusCode != http.StatusForbidden {
						t.Fatalf("response = %+v, want 403", resp)
					}
				}
			})
		}
	}
}
