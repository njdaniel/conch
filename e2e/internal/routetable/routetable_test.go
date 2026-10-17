package routetable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServes(t *testing.T) {
	var routes []Route
	for _, text := range []string{"GET /healthz", "POST /v1/sessions/{id}/heartbeat", "GET /v2/ws", "/mcp"} {
		r, err := Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, r)
	}
	for _, tc := range []struct {
		call string
		ok   bool
	}{
		{"GET /healthz", true},
		{"POST /healthz", false},
		{"POST /v1/sessions/12/heartbeat", true},
		{"POST /v1/sessions/12/heartbeat/extra", false},
		{"POST /v1/sessions//heartbeat", false},
		{"GET /v2/ws", true},
		{"POST /mcp", true},
		{"GET /v1/test-only", false},
	} {
		t.Run(tc.call, func(t *testing.T) {
			if bad := Unlisted(routes, []string{tc.call}); (len(bad) == 0) != tc.ok {
				t.Errorf("Unlisted = %v, want the call served: %v", bad, tc.ok)
			}
		})
	}
}

func TestFromSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"rt calls and literal routes", `
		rt("GET /healthz", accessExempt, h),
		{pattern: "GET /v2/ws", access: accessAuthenticated},
		rt("/mcp", accessExempt, m),`, []string{"GET /healthz", "GET /v2/ws", "/mcp"}},
		// Text shaped like a route is read wherever it is. For a coverage
		// check that errs the safe way: a phantom route needs a row.
		{"a quoted pattern in a comment is read too", `// rt("GET /nothing") in prose
		rt("DELETE /v1/invites/{id}", a, h),`, []string{"GET /nothing", "DELETE /v1/invites/{id}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromSource(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			var texts []string
			for _, r := range got {
				texts = append(texts, r.Text)
			}
			if len(texts) != len(tc.want) {
				t.Fatalf("read %q, want %q", texts, tc.want)
			}
			for i := range texts {
				if texts[i] != tc.want[i] {
					t.Fatalf("read %q, want %q", texts, tc.want)
				}
			}
		})
	}
	if _, err := FromSource("package server\n"); err == nil {
		t.Error("a file with no routes was read without an error")
	}
}

// TestReadsTheRealTable: the table this repository has is readable and big
// enough to believe the pattern found it.
func TestReadsTheRealTable(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	routes, err := Read(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) < 40 {
		t.Fatalf("read %d routes, want the whole table", len(routes))
	}
}
