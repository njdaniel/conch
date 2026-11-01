package server

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
)

// webCSP is the Content-Security-Policy of every /app/ response. The page and
// everything it loads come from conchd's own origin; it may not be framed, may
// not set a <base>, embeds no plugin content, and may submit a form only to
// conchd (form-action does not fall back to default-src, so it is spelled out).
// blob: in img-src alone is the attachment previews (issue #762): thumbnails of
// bytes the page fetched itself, never a URL it was given.
const webCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; object-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// webPermissionsPolicy limits the powers the page may use: its own origin may
// capture a microphone (browser voice, #442) and a screen (screen share, #444),
// and nothing else: no camera, and no frame of another origin may do either.
const webPermissionsPolicy = "microphone=(self), camera=(), display-capture=(self)"

// webCSPFor is webCSP for a server whose voice is at livekitURL. With voice not
// configured (an empty or unusable URL) it is webCSP unchanged. Otherwise
// connect-src also names the LiveKit origin, as ws(s):// for the signalling
// socket and http(s):// for the requests the SDK makes to the same host when a
// connection fails. Only the origin is named, never a path. An address whose
// host or port livekit.CheckCSPHost refuses adds nothing.
func webCSPFor(livekitURL string) string {
	if livekitURL == "" {
		return webCSP
	}
	u, err := url.Parse(livekitURL)
	if err != nil || u.Host == "" {
		return webCSP
	}
	// ParseConfig has refused such an address at startup; should one reach
	// here anyway, the page gets the plain policy rather than whatever the
	// address would write into it.
	if livekit.CheckCSPHost(u) != nil {
		return webCSP
	}
	host := u.Hostname()
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	var httpScheme string
	switch u.Scheme {
	case "wss":
		httpScheme = "https"
	case "ws":
		httpScheme = "http"
	default:
		return webCSP
	}
	extra := u.Scheme + "://" + host + " " + httpScheme + "://" + host
	return strings.Replace(webCSP, "connect-src 'self';", "connect-src 'self' "+extra+";", 1)
}

const (
	webIndex     = "index.html"
	webAssetsDir = "assets/"
	webImmutable = "public, max-age=31536000, immutable"
)

// CheckWebDir reports why dir cannot be served at /app/: it must be a
// directory holding a regular index.html. conchd calls it before it starts.
func CheckWebDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("web dir %q: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(webIndex)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("web dir %q has no %s", dir, webIndex)
	case err != nil:
		return fmt.Errorf("web dir %q: %s: %w", dir, webIndex, err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("web dir %q: %s is not a regular file", dir, webIndex)
	}
	return nil
}

// CheckWebFS reports why an embedded client cannot be served at /app/: its
// root must hold a regular index.html. conchd calls it before it starts, as
// it calls CheckWebDir for a directory.
func CheckWebFS(fsys fs.FS) error {
	fi, err := fs.Stat(fsys, webIndex)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("embedded web client has no %s", webIndex)
	case err != nil:
		return fmt.Errorf("embedded web client: %s: %w", webIndex, err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("embedded web client: %s is not a regular file", webIndex)
	}
	return nil
}

// webRoutes returns the routes that serve the web client: /app/ and the
// redirect from /. They need no credential because the page must load before
// sign-in; the API it then calls stays behind its own routes. Neither pattern
// can match an API path: /app/ is a prefix no API route starts with, and
// /{$} matches only "/".
//
// The client comes from WebDir when that is set, otherwise from WebFS (the
// build embedded in the binary), otherwise there are no routes. An unusable
// directory yields no routes (fail closed), never a fall-back to WebFS: the
// operator asked for that directory. conchd has already refused to start in
// that case, so reaching it here means the directory went away in between;
// that is logged, because /app/ answering 404 with nothing said would look
// like a client that was never configured.
func (s *Server) webRoutes() []route {
	var src webSource
	switch {
	case s.cfg.WebDir != "":
		root, err := os.OpenRoot(s.cfg.WebDir)
		if err != nil {
			slog.Error("web: the web directory cannot be opened; /app/ is not served", "dir", s.cfg.WebDir, "error", err)
			return nil
		}
		src = rootSource{root}
	case s.cfg.WebFS != nil:
		src = fsSource{s.cfg.WebFS}
	default:
		return nil
	}
	return []route{
		{pattern: "GET /app/", access: accessExempt, handler: &staticHandler{src: src, csp: webCSPFor(s.cfg.LiveKit.URL)}},
		{pattern: "GET /{$}", access: accessExempt, handler: http.RedirectHandler("/app/", http.StatusFound)},
	}
}

// webFile is what the handler needs of an open file: http.ServeContent reads
// and seeks it, and the handler closes it and asks it for its info.
type webFile interface {
	io.ReadSeeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// webSource is where the built client lives: a directory on disk (--web-dir)
// or the build embedded in the binary. Names are slash-separated and relative.
// Stat follows a symlink that stays inside the source; confinement is the
// source's, not the handler's.
type webSource interface {
	Stat(name string) (fs.FileInfo, error)
	Open(name string) (webFile, error)
}

// rootSource serves a directory through an os.Root, which refuses ".." and
// any symlink that leaves the directory.
type rootSource struct{ root *os.Root }

func (r rootSource) Stat(name string) (fs.FileInfo, error) { return r.root.Stat(name) }

func (r rootSource) Open(name string) (webFile, error) {
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// fsSource serves an fs.FS, the embedded build. An embed.FS holds no
// symlinks, so there is nothing to confine beyond fs.ValidPath, which every
// fs.FS applies to the name it is given.
type fsSource struct{ fsys fs.FS }

func (f fsSource) Stat(name string) (fs.FileInfo, error) { return fs.Stat(f.fsys, name) }

func (f fsSource) Open(name string) (webFile, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	// http.ServeContent needs to seek. embed and the in-memory test
	// filesystem both do; a source that cannot is refused, not buffered.
	wf, ok := file.(webFile)
	if !ok {
		_ = file.Close()
		return nil, fmt.Errorf("web: %q cannot be served: the file does not seek", name)
	}
	return wf, nil
}

// staticHandler serves a built web client out of a webSource.
type staticHandler struct {
	src webSource
	// csp is the Content-Security-Policy to send; empty means webCSP.
	csp string

	gzMu    sync.Mutex
	gzCache map[string]gzEntry
	gzBytes int
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	csp := h.csp
	if csp == "" {
		csp = webCSP
	}
	hd.Set("Content-Security-Policy", csp)
	hd.Set("Permissions-Policy", webPermissionsPolicy)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")

	rel := strings.TrimPrefix(r.URL.Path, "/app/")
	// The decoded path may carry "..", a backslash or a NUL (from %2e%2e,
	// %5c, %00). None names a client file, and os.Root would refuse the
	// first anyway; answer 404 without touching the disk. A segment that
	// starts with a dot is refused too: a build has no use for one, and a
	// directory given by mistake may hold a .env or a .git.
	if hasUnsafeSegment(rel) {
		http.NotFound(w, r)
		return
	}
	name := path.Clean("/" + rel)[1:] // "" for the root

	f, fi, err := h.open(name)
	switch {
	case err == nil && fi.IsDir():
		// Never a listing: a directory is just a path that names no file.
		// open hands back no file for it.
		f = nil
	case err == nil, errors.Is(err, fs.ErrNotExist):
	default:
		// Includes a symlink that leaves the directory: not served, and
		// not answered with the index either.
		http.NotFound(w, r)
		return
	}
	if f == nil {
		// A path with no file belongs to the client router, unless it
		// looks like a file, so a missing script is not answered with HTML.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = webIndex
		if f, fi, err = h.open(name); err != nil || fi.IsDir() {
			if f != nil {
				_ = f.Close()
			}
			http.NotFound(w, r)
			return
		}
	}
	defer func() { _ = f.Close() }()

	switch {
	case name == webIndex:
		hd.Set("Cache-Control", "no-store")
	case strings.HasPrefix(name, webAssetsDir):
		hd.Set("Cache-Control", webImmutable)
	default:
		hd.Set("Cache-Control", "no-cache")
	}
	if gzippable(name) {
		hd.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) && plainPreconditions(r) {
			if gz, ok := h.gzipped(name, fi, f); ok {
				// Set the type from the name: ServeContent would otherwise
				// sniff it from the compressed bytes.
				hd.Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
				hd.Set("Content-Encoding", "gzip")
				// ServeContent leaves the length out once an encoding is
				// set; the compressed size is known, so say it.
				hd.Set("Content-Length", strconv.Itoa(len(gz)))
				http.ServeContent(w, r, path.Base(name), fi.ModTime(), bytes.NewReader(gz))
				return
			}
			// Compression declined: the read position is unknown.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				http.NotFound(w, r)
				return
			}
		}
	}
	http.ServeContent(w, r, path.Base(name), fi.ModTime(), f)
}

// Compression of the client's static files (issue #1037). Only this handler
// compresses: /app/ serves public files the build produced, never a secret or
// anything built from a request, so a compressed size can reveal nothing
// (BREACH needs a secret and attacker-chosen input in one compressed body).
// No API, WebSocket, MCP or attachment route goes through here; the allowlist
// below is a second guard that keeps a stray file of another type plain.
const (
	gzMinSize  = 1024     // smaller than this is not worth a gzip header
	gzMaxSize  = 16 << 20 // larger is served plain rather than held in memory
	gzMaxCache = 32 << 20 // total compressed bytes kept
	// gzMaxEntries bounds the names remembered; a build has far fewer files.
	gzMaxEntries = 4096
)

// gzipExts are the text types of a built client. Images, fonts and wasm are
// already compressed or are served as they are.
var gzipExts = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".svg": true,
	".json": true, ".map": true, ".txt": true, ".webmanifest": true,
}

// gzippable reports whether name is a text type worth compressing whose
// Content-Type is known from its extension alone.
func gzippable(name string) bool {
	ext := path.Ext(name)
	return gzipExts[strings.ToLower(ext)] && mime.TypeByExtension(ext) != ""
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip: an
// explicit gzip entry decides, and only without one does "*" (RFC 9110
// 12.5.3). A quality of zero, or one that does not parse, refuses.
func acceptsGzip(r *http.Request) bool {
	gzipQ, anyQ := -1.0, -1.0
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(v, ",") {
			tok, params, _ := strings.Cut(part, ";")
			switch strings.ToLower(strings.TrimSpace(tok)) {
			case "gzip", "x-gzip":
				gzipQ = max(gzipQ, quality(params))
			case "*":
				anyQ = max(anyQ, quality(params))
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return anyQ > 0
}

// quality is the q parameter in an Accept-Encoding entry's parameters: 1 when
// absent, 0 when it is not a qvalue as RFC 9110 12.4.2 spells one ("0",
// "0.5", "1.000"; never NaN, an exponent or a bare q).
func quality(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		k, v, hasValue := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		v = strings.TrimSpace(v)
		if !hasValue || !isQValue(v) {
			return 0
		}
		q, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return q
	}
	return 1
}

// isQValue reports whether v is ( "0" [ "." 0*3DIGIT ] ) / ( "1" [ "." 0*3("0") ] ).
func isQValue(v string) bool {
	if v == "" || (v[0] != '0' && v[0] != '1') {
		return false
	}
	if len(v) == 1 {
		return true
	}
	if v[1] != '.' || len(v) > 5 {
		return false
	}
	for _, c := range v[2:] {
		if c < '0' || c > '9' || (v[0] == '1' && c != '0') {
			return false
		}
	}
	return true
}

// plainPreconditions reports whether the request carries none of the headers
// that make ServeContent answer something other than the whole file with its
// own status: a range, or a precondition whose failure is a 412 that would go
// out with the compressed representation's headers and no body. Those are
// rare for a static client and are served plain.
func plainPreconditions(r *http.Request) bool {
	for _, h := range []string{"Range", "If-Range", "If-Match", "If-Unmodified-Since"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// gzEntry is one file's compressed form; nil data means "serve it plain"
// (gzip did not shrink it, or the cache was full).
type gzEntry struct {
	size int64
	mod  time.Time
	data []byte
}

// gzCompress writes src to dst gzipped. A variable so tests can count and
// hold compressions.
var gzCompress = func(dst io.Writer, src io.Reader) error {
	zw, err := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		return err
	}
	return zw.Close()
}

// gzWorkers bounds the compressions running at once. A request that finds
// none free is served plain rather than queued, so a burst of cold requests
// costs at most this many buffers.
var gzWorkers = make(chan struct{}, 2)

// gzipped returns the gzip form of f (read from its start), cached by name
// until the file's size or modification time changes. A file gzip cannot
// shrink, or one past the cache's limits, is remembered as plain, so nothing
// is compressed again on every request.
func (h *staticHandler) gzipped(name string, fi fs.FileInfo, f io.Reader) ([]byte, bool) {
	if fi.Size() < gzMinSize || fi.Size() > gzMaxSize {
		return nil, false
	}
	h.gzMu.Lock()
	e, ok := h.gzCache[name]
	full := len(h.gzCache) >= gzMaxEntries
	h.gzMu.Unlock()
	if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.data, e.data != nil
	}
	if !ok && full {
		// A name the cache cannot remember is served plain, never
		// compressed again on every request.
		return nil, false
	}
	select {
	case gzWorkers <- struct{}{}:
		defer func() { <-gzWorkers }()
	default:
		return nil, false
	}
	var buf bytes.Buffer
	if err := gzCompress(&buf, io.LimitReader(f, gzMaxSize+1)); err != nil {
		return nil, false
	}
	data := buf.Bytes()
	if int64(len(data)) >= fi.Size() {
		data = nil
	}
	h.gzMu.Lock()
	defer h.gzMu.Unlock()
	if h.gzCache == nil {
		h.gzCache = map[string]gzEntry{}
	}
	if old, ok := h.gzCache[name]; ok {
		h.gzBytes -= len(old.data)
		delete(h.gzCache, name)
	}
	if len(h.gzCache) >= gzMaxEntries {
		// Filled by another request meanwhile: answer, remember nothing.
		return data, data != nil
	}
	if h.gzBytes+len(data) > gzMaxCache {
		data = nil
	}
	h.gzCache[name] = gzEntry{size: fi.Size(), mod: fi.ModTime(), data: data}
	h.gzBytes += len(data)
	return data, data != nil
}

// open opens name (slash-separated, relative) inside the source. Only a
// regular file is opened; a directory comes back as its info alone. Opening a
// FIFO would block the request until something wrote to it, and a device is
// not a client file. The Stat comes first for that reason, and follows a
// symlink that stays inside the source.
func (h *staticHandler) open(name string) (webFile, fs.FileInfo, error) {
	if name == "" {
		return nil, nil, fs.ErrNotExist
	}
	fi, err := h.src.Stat(name)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return nil, nil, fs.ErrNotExist
	}
	if fi.IsDir() {
		// Nothing is read from a directory, and an embedded one does not
		// seek, so it is not opened.
		return nil, fi, nil
	}
	f, err := h.src.Open(name)
	if err != nil {
		return nil, nil, err
	}
	// Stat again on the open file: the name may have been replaced since,
	// and a directory is not served from an open file.
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fs.ErrNotExist
		}
		return nil, nil, err
	}
	return f, fi, nil
}

func hasUnsafeSegment(rel string) bool {
	if strings.ContainsAny(rel, "\\\x00") {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
