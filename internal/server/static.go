package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// webCSP is the Content-Security-Policy of every /app/ response. The page and
// everything it loads come from conchd's own origin; it may not be framed, may
// not set a <base>, embeds no plugin content, and may submit a form only to
// conchd (form-action does not fall back to default-src, so it is spelled out).
const webCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// webPermissionsPolicy limits the powers the page may use: its own origin may
// capture a microphone (browser voice, #442) and nothing else; no camera and no
// screen capture until screen share is built.
const webPermissionsPolicy = "microphone=(self), camera=(), display-capture=()"

// webCSPFor is webCSP for a server whose voice is at livekitURL. With voice not
// configured (an empty or unusable URL) it is webCSP unchanged. Otherwise
// connect-src also names the LiveKit origin, as ws(s):// for the signalling
// socket and http(s):// for the requests the SDK makes to the same host when a
// connection fails. Only the origin is named, never a path.
func webCSPFor(livekitURL string) string {
	if livekitURL == "" {
		return webCSP
	}
	u, err := url.Parse(livekitURL)
	if err != nil || u.Host == "" {
		return webCSP
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
	extra := u.Scheme + "://" + u.Host + " " + httpScheme + "://" + u.Host
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
	http.ServeContent(w, r, path.Base(name), fi.ModTime(), f)
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
