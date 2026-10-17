package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/njdaniel/conch/e2e/internal/routetable"
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

// routePattern is one entry of the route table (e2e/internal/routetable).
type routePattern = routetable.Route

// publicRoutes reads the route table of the repository's server.
func publicRoutes(repo string) ([]routePattern, error) { return routetable.Read(repo) }

func parseRoutePattern(text string) (routePattern, error) { return routetable.Parse(text) }

// unlisted is the requests in seen that no route of the table serves.
func unlisted(routes []routePattern, seen []string) []string {
	return routetable.Unlisted(routes, seen)
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
