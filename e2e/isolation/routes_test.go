package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/e2e/internal/routetable"
)

func table(t *testing.T, texts ...string) []routetable.Route {
	t.Helper()
	var out []routetable.Route
	for _, text := range texts {
		r, err := routetable.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestCoverage: a route of the table with no row and no exemption is
// missing; a row or exemption for a route the table does not have is stale.
func TestCoverage(t *testing.T) {
	rows := []row{{route: "GET /v1/runs/{id}"}, {route: "GET /v1/runs/{id}"}, {route: "POST /v1/reports"}}
	ex := map[string]string{"GET /healthz": "no data"}
	for _, tc := range []struct {
		name           string
		table          []string
		missing, stale []string
	}{
		{"every route covered", []string{"GET /healthz", "GET /v1/runs/{id}", "POST /v1/reports"}, nil, nil},
		{"a route added without a row", []string{"GET /healthz", "GET /v1/runs/{id}", "POST /v1/reports", "POST /v1/sign-in-links/redeem"},
			[]string{"POST /v1/sign-in-links/redeem"}, nil},
		{"a route removed under its row", []string{"GET /healthz", "GET /v1/runs/{id}"}, nil, []string{"POST /v1/reports"}},
		{"an exemption for a route removed", []string{"GET /v1/runs/{id}", "POST /v1/reports"}, nil, []string{"GET /healthz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, stale := coverage(table(t, tc.table...), rows, ex)
			if !slices.Equal(missing, tc.missing) || !slices.Equal(stale, tc.stale) {
				t.Errorf("coverage = missing %q, stale %q; want %q, %q", missing, stale, tc.missing, tc.stale)
			}
		})
	}
}

// TestEveryRouteHasARow is the coverage check against this repository's
// table, so that a route added without a row fails the unit tests as well as
// the program.
func TestEveryRouteHasARow(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	h := &harness{repo: repo}
	if _, err := h.checkCoverage(); err != nil {
		t.Fatal(err)
	}
}

// TestRowsAskTheirRoute: each row's request (with stand-in objects) is one
// its route serves.
func TestRowsAskTheirRoute(t *testing.T) {
	w := &world{tag: "t",
		a: &group{channels: []string{"alpha-t-camp", "alpha-t-den"}, chanIDs: []int64{1, 2}, people: []*member{{id: 3}}, agent: &member{id: 4}, hidden: &member{id: 21, name: "hidden-21"},
			invites: []int64{5, 6}, netName: "alpha-t-net", root: 7},
		b: &group{channels: []string{"bravo-t-yard"}, chanIDs: []int64{8}, people: []*member{{id: 9}}, approval: 10, agentSession: 11},
	}
	w.p = &plaza{channel: "plaza-t-square", chanID: 12, preRoot: 13, preFile: 14, preRun: 15, preIV: 16, preWord: "early-t-x",
		postFile: 17, postRun: 18, pia: &member{id: 19}, nick2: &member{id: 20}}
	w.prober = w.b.people[0]
	for _, r := range allRows() {
		var s subject
		if r.pick != nil {
			s = r.pick(w)
		}
		for _, sub := range []subject{s, w.ghost()} {
			if err := w.checkRoute(r.route, r.probe(w, sub)); err != nil {
				t.Errorf("%s (%s): %v", r.route, r.what, err)
			}
		}
		if (r.own || r.onBehalf) == (r.want != answer{}) {
			t.Errorf("%s (%s): a row either succeeds (own), is bound to the caller (onBehalf) or states its refusal", r.route, r.what)
		}
		if r.noGhost != "" && r.pick == nil {
			t.Errorf("%s (%s): a row that names nothing of A has no ghost to leave out", r.route, r.what)
		}
	}
}

// TestCheckRefusal: the refusal check takes the stated answer and nothing
// else.
func TestCheckRefusal(t *testing.T) {
	want := answer{http.StatusNotFound, "run_not_found"}
	for _, tc := range []struct {
		name string
		got  result
		ok   bool
	}{
		{"the stated refusal", result{status: 404, body: []byte(`{"code":"run_not_found","message":"run not found"}` + "\n")}, true},
		{"another status", result{status: 403, body: []byte(`{"code":"run_not_found","message":"run not found"}`)}, false},
		{"another code", result{status: 404, body: []byte(`{"code":"session_not_found","message":"session not found"}`)}, false},
		{"an error document with more in it", result{status: 404, body: []byte(`{"code":"run_not_found","message":"run not found","channel":"x"}`)}, false},
		{"a message with a number in it", result{status: 404, body: []byte(`{"code":"run_not_found","message":"run not found in channel 3"}`)}, false},
		{"the asked id in the message", result{status: 404, body: []byte(`{"code":"run_not_found","message":"run 42 not found"}`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRefusal(tc.got, want, asked{ids: []kindID{{"run", 42}}})
			if (err == nil) != tc.ok {
				t.Errorf("checkRefusal = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// TestGateCoverage: a gate with a row in neither map is missing; a row whose
// phrase no phase holds, an entry for a gate the list lacks and a gate in
// both maps are stale.
func TestGateCoverage(t *testing.T) {
	gates := []string{"a.go A", "b.go B", "c.go C"}
	source := []string{"the row of A", "x"}
	rows := map[string]string{"a.go A": "the row of A"}
	pending := map[string]string{"b.go B": "#1", "c.go C": "#2"}
	for _, tc := range []struct {
		name                  string
		gates                 []string
		rows, pending         map[string]string
		missing, noRow, stale []string
	}{
		{"every gate has a row or a pending note", gates, rows, pending, nil, nil, nil},
		{"a gate added with no row", append([]string{"d.go D"}, gates...), rows, pending, []string{"d.go D"}, nil, nil},
		{"a row whose phrase is gone", gates, map[string]string{"a.go A": "a phrase nobody wrote"}, pending, nil, []string{"a.go A -> a phrase nobody wrote"}, nil},
		{"a row for a gate removed", gates[1:], rows, pending, nil, nil, []string{"a.go A"}},
		{"a pending note for a gate removed", gates[:1], rows, pending, nil, nil, []string{"b.go B", "c.go C"}},
		{"a gate with a row and a pending note", gates, rows, map[string]string{"a.go A": "#3", "b.go B": "#1", "c.go C": "#2"}, nil, nil, []string{"a.go A (in both maps)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, noRow, stale := gateCoverage(tc.gates, tc.rows, tc.pending, source)
			if !slices.Equal(missing, tc.missing) || !slices.Equal(noRow, tc.noRow) || !slices.Equal(stale, tc.stale) {
				t.Errorf("gateCoverage = missing %q, noRow %q, stale %q; want %q, %q, %q", missing, noRow, stale, tc.missing, tc.noRow, tc.stale)
			}
		})
	}
}

// TestEveryPermissionRouteHasARestrictedRow is the coverage rule of design
// §7.7 item 2 against this repository: every route of class accessPermission
// is read by the restricted phase (which asks each one), and every handler
// check of §7.3, the gates the server's TestEnforcedPermissionsAreHonest
// lists, has a restricted-operator or restricted-member row or a pending
// note. A permission route or a gate added without a row fails here.
func TestEveryPermissionRouteHasARestrictedRow(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	unread, err := permissionRouteCount(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) > 0 {
		t.Errorf("routes of class accessPermission the restricted phase does not read (write each as rt(\"...\", accessPermission, ...).needs(schema.Perm...) on one line): %s", strings.Join(unread, ", "))
	}
	gates, err := gateKeys(repo)
	if err != nil {
		t.Fatal(err)
	}
	source, err := rowLiterals(repo)
	if err != nil {
		t.Fatal(err)
	}
	missing, noRow, stale := gateCoverage(gates, gateRows, pendingGates, source)
	if len(missing) > 0 {
		t.Errorf("handler checks (permissionGates, internal/server/permissions_test.go) with no restricted row in gateRows and no note in pendingGates (e2e/isolation/gaterows.go): %s", strings.Join(missing, ", "))
	}
	if len(noRow) > 0 {
		t.Errorf("gateRows name a phrase that restricted.go and preset.go do not hold: %s", strings.Join(noRow, "; "))
	}
	if len(stale) > 0 {
		t.Errorf("gateRows or pendingGates entries that name no gate, or a gate twice: %s", strings.Join(stale, ", "))
	}
}

// TestHideReadsAreRoutes: every read the hide-members rows scan alice's
// answers of (hiderows.go) is a request its route serves, and the route is
// in the table.
func TestHideReadsAreRoutes(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := routetable.Read(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHideReadsAreRoutes(tbl); err != nil {
		t.Fatal(err)
	}
}
