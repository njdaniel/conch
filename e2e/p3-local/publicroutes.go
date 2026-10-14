package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The suite calls the server through its public routes only: the table in
// internal/server/routes.go, which the server builds its mux from. Every
// request the suite itself makes (not the ones the conch CLI and the TUI make
// as programs of their own) is recorded, and at the end each is matched
// against that table. A request the table does not have fails the suite, so a
// helper that reached for a route made for tests could not pass; conchd has
// no such route to reach.

// requestLog is the set of "METHOD path" the suite has requested.
type requestLog struct {
	mu   sync.Mutex
	seen map[string]bool
}

var requests = &requestLog{seen: make(map[string]bool)}

func (l *requestLog) add(method, path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[method+" "+path] = true
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.seen))
	for k := range l.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loggingTransport records each request before it is sent.
type loggingTransport struct{ next http.RoundTripper }

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	requests.add(req.Method, req.URL.Path)
	return t.next.RoundTrip(req)
}

func newLoggingTransport() http.RoundTripper { return loggingTransport{http.DefaultTransport} }

// routePattern is one entry of the route table: an optional method and a path
// whose {name} segments match any one segment.
type routePattern struct {
	method string
	path   *regexp.Regexp
	text   string
}

var routeEntry = regexp.MustCompile(`(?:\brt\(|pattern:\s*)"((?:(?:GET|POST|PUT|DELETE|PATCH|HEAD) )?/[^"]*)"`)

// publicRoutes reads the route table of the repository's server.
func publicRoutes(repo string) ([]routePattern, error) {
	src, err := os.ReadFile(filepath.Join(repo, "internal", "server", "routes.go")) // #nosec G304 -- the repository this run is in
	if err != nil {
		return nil, err
	}
	var out []routePattern
	for _, m := range routeEntry.FindAllStringSubmatch(string(src), -1) {
		p, err := parseRoutePattern(m[1])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("internal/server/routes.go has no routes this check can read")
	}
	return out, nil
}

func parseRoutePattern(text string) (routePattern, error) {
	method, path := "", text
	if m, p, ok := strings.Cut(text, " "); ok {
		method, path = m, p
	}
	var re strings.Builder
	re.WriteString("^")
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		re.WriteString("/")
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			re.WriteString("[^/]+")
			continue
		}
		re.WriteString(regexp.QuoteMeta(seg))
	}
	// A pattern that ends in a slash matches everything below it, as the mux's
	// does; the table has none, and a call to such a path would be refused here.
	re.WriteString("$")
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return routePattern{}, err
	}
	return routePattern{method: method, path: compiled, text: text}, nil
}

// unlisted is the requests in seen that no route of the table serves.
func unlisted(routes []routePattern, seen []string) []string {
	var out []string
	for _, call := range seen {
		method, path, _ := strings.Cut(call, " ")
		ok := false
		for _, r := range routes {
			if (r.method == "" || r.method == method) && r.path.MatchString(path) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, call)
		}
	}
	return out
}

// checkPublicRoutes fails if the suite has asked for a path that is not in the
// public table.
func (r *runner) checkPublicRoutes() error {
	routes, err := publicRoutes(r.repo)
	if err != nil {
		return err
	}
	seen := requests.all()
	if bad := unlisted(routes, seen); len(bad) != 0 {
		return fmt.Errorf("the suite requested paths that are not in internal/server/routes.go: %s", strings.Join(bad, ", "))
	}
	step("the %d requests the suite made all went to the %d routes of the public table", len(seen), len(routes))
	return nil
}
