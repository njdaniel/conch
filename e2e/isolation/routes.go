package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/njdaniel/conch/e2e/internal/routetable"
)

// The coverage check. The program reads the route table the server builds
// its mux from (internal/server/routes.go) and fails when a route has neither
// a row in the matrix nor a line in the exempt list: a route added later
// cannot skip the isolation check. A row or an exemption that names a route
// the table no longer has fails too, so the matrix cannot rot.

// coverage returns the routes of the table that have no row and are not
// exempt, and the routes rows or exemptions name that the table does not
// have.
func coverage(table []routetable.Route, rows []row, exempt map[string]string) (missing, stale []string) {
	inTable := map[string]bool{}
	for _, r := range table {
		inTable[r.Text] = true
	}
	covered := map[string]bool{}
	for _, r := range rows {
		covered[r.route] = true
	}
	for route := range exempt {
		covered[route] = true
	}
	for _, r := range table {
		if !covered[r.Text] {
			missing = append(missing, r.Text)
		}
	}
	for route := range covered {
		if !inTable[route] {
			stale = append(stale, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	return missing, stale
}

// checkCoverage reads the table and fails on any gap.
func (h *harness) checkCoverage() ([]routetable.Route, error) {
	table, err := routetable.Read(h.repo)
	if err != nil {
		return nil, err
	}
	missing, stale := coverage(table, append(matrixRows(), agentRows()...), exempt)
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, "routes with no row in the isolation matrix (e2e/isolation/matrix.go) and no exemption: "+strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		problems = append(problems, "rows or exemptions for routes internal/server/routes.go does not have: "+strings.Join(stale, ", "))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return table, nil
}

// checkRoute requires that a row's request is one its route serves, so that a
// row cannot claim a route while asking another.
func (w *world) checkRoute(route string, p probe) error {
	r, err := routetable.Parse(route)
	if err != nil {
		return err
	}
	path, _, _ := strings.Cut(p.path, "?")
	if !r.Serves(p.method, path) {
		return fmt.Errorf("the row's request %s %s is not one %q serves", p.method, path, route)
	}
	return nil
}

// checkCalls requires that every request the run made went to a route of the
// table: the run used the public API only.
func (h *harness) checkCalls(table []routetable.Route) error {
	calls := h.allCalls()
	if bad := routetable.Unlisted(table, calls); len(bad) > 0 {
		return fmt.Errorf("the run requested paths that are not in internal/server/routes.go: %s", strings.Join(bad, ", "))
	}
	return nil
}
