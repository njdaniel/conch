// Package routetable reads the server's route table, internal/server/routes.go,
// for the end-to-end programs that hold themselves to it.
//
// Its input is the repository's routes.go, read as text; its output is the
// routes it lists, each a method (empty for a pattern that takes any) and a
// path whose {name} segments match any one segment. It owns nothing else.
//
// Two programs use it. e2e/p3-local checks that every request it made went to
// a route of the table, so that it can only pass through the public API.
// e2e/isolation checks the other way round: that every route of the table has
// a row in its cross-group matrix, so that a route added later cannot skip
// the isolation check.
package routetable

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Route is one entry of the route table: an optional method and a path whose
// {name} segments match any one segment.
type Route struct {
	// Method is "GET", "POST" and so on, or "" for a pattern that serves
	// every method (/mcp).
	Method string
	// Text is the pattern as routes.go writes it, "GET /v1/runs/{id}".
	Text string
	path *regexp.Regexp
}

// entry finds the patterns of routes.go: the first argument of rt(...) and the
// pattern: field of a literal route.
var entry = regexp.MustCompile(`(?:\brt\(|pattern:\s*)"((?:(?:GET|POST|PUT|DELETE|PATCH|HEAD) )?/[^"]*)"`)

// Read reads the route table of the repository at repo.
func Read(repo string) ([]Route, error) {
	src, err := os.ReadFile(filepath.Join(repo, "internal", "server", "routes.go")) // #nosec G304 -- the repository this run is in
	if err != nil {
		return nil, err
	}
	return FromSource(string(src))
}

// FromSource reads the routes out of the text of a routes.go.
func FromSource(src string) ([]Route, error) {
	var out []Route
	for _, m := range entry.FindAllStringSubmatch(src, -1) {
		r, err := Parse(m[1])
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("internal/server/routes.go has no routes this check can read")
	}
	return out, nil
}

// Parse turns one pattern of the table into a Route.
func Parse(text string) (Route, error) {
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
		return Route{}, err
	}
	return Route{Method: method, Text: text, path: compiled}, nil
}

// Serves reports whether the route serves a request with this method and path.
func (r Route) Serves(method, path string) bool {
	return (r.Method == "" || r.Method == method) && r.path.MatchString(path)
}

// Unlisted is the calls ("METHOD /path") that no route serves.
func Unlisted(routes []Route, calls []string) []string {
	var out []string
	for _, call := range calls {
		method, path, _ := strings.Cut(call, " ")
		ok := false
		for _, r := range routes {
			if r.Serves(method, path) {
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
