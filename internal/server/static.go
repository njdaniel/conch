package server

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// webCSP is the Content-Security-Policy of every /app/ response. The page and
// everything it loads come from conchd's own origin; it may not be framed, may
// not set a <base>, embeds no plugin content, and may submit a form only to
// conchd (form-action does not fall back to default-src, so it is spelled out).
const webCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

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
// An unusable directory yields no routes (fail closed). conchd has already
// refused to start in that case, so reaching it here means the directory went
// away in between; that is logged, because /app/ answering 404 with nothing
// said would look like a client that was never configured.
func (s *Server) webRoutes() []route {
	if s.cfg.WebDir == "" {
		return nil
	}
	root, err := os.OpenRoot(s.cfg.WebDir)
	if err != nil {
		slog.Error("web: the web directory cannot be opened; /app/ is not served", "dir", s.cfg.WebDir, "error", err)
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

// open opens name (slash-separated, relative) inside the root. Only a regular
// file or a directory is opened: opening a FIFO would block the request until
// something wrote to it, and a device is not a client file. The Stat comes
// first for that reason, and follows a symlink that stays inside the root.
func (h *staticHandler) open(name string) (*os.File, fs.FileInfo, error) {
	if name == "" {
		return nil, nil, fs.ErrNotExist
	}
	fi, err := h.root.Stat(name)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return nil, nil, fs.ErrNotExist
	}
	f, err := h.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	// Stat again on the open file: the name may have been replaced since.
	if fi, err = f.Stat(); err != nil || (!fi.Mode().IsRegular() && !fi.IsDir()) {
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
