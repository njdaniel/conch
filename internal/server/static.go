package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// webCSP is the Content-Security-Policy of every /app/ response. The page and
// everything it loads come from conchd's own origin; it may not be framed and
// may not set a <base>.
const webCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'"

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

// webRoutes returns the routes that serve the web client: /app/ and the
// redirect from /. They need no credential because the page must load before
// sign-in; the API it then calls stays behind its own routes. Neither pattern
// can match an API path: /app/ is a prefix no API route starts with, and
// /{$} matches only "/".
//
// An unusable directory yields no routes (fail closed); conchd has already
// refused to start in that case.
func (s *Server) webRoutes() []route {
	if s.cfg.WebDir == "" {
		return nil
	}
	root, err := os.OpenRoot(s.cfg.WebDir)
	if err != nil {
		return nil
	}
	return []route{
		{pattern: "GET /app/", access: accessExempt, handler: &staticHandler{root: root}},
		{pattern: "GET /{$}", access: accessExempt, handler: http.RedirectHandler("/app/", http.StatusFound)},
	}
}

// staticHandler serves a built web client out of an os.Root, which refuses
// ".." and any symlink that leaves the directory, so confinement is the
// standard library's, not a path check of ours.
type staticHandler struct {
	root *os.Root
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", webCSP)
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
		_ = f.Close()
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

// open opens name (slash-separated, relative) inside the root.
func (h *staticHandler) open(name string) (*os.File, fs.FileInfo, error) {
	if name == "" {
		return nil, nil, fs.ErrNotExist
	}
	f, err := h.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
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
