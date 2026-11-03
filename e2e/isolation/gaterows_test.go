package main

import (
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// TestLiteralsOnly: a phrase in a comment, an identifier or a function name
// is not held; one in a string literal, interpreted or raw, is.
func TestLiteralsOnly(t *testing.T) {
	src := "package p\n// a comment says the row of A\nfunc theRowOfA() {}\nvar x = \"the row \" + `of B`\n"
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	lits := literalsOf(file)
	if !slices.Equal(lits, []string{"the row ", "of B"}) {
		t.Fatalf("literalsOf = %q", lits)
	}
	rows := map[string]string{"a.go A": "the row of A", "b.go B": "of B"}
	_, noRow, _ := gateCoverage([]string{"a.go A", "b.go B"}, rows, nil, lits)
	if want := []string{"a.go A -> the row of A"}; !slices.Equal(noRow, want) {
		t.Errorf("noRow = %q, want %q", noRow, want)
	}
}

// TestGateKeysOf: a gate-list element that is not two string literals is an
// error naming the permission and the index; it is never skipped.
func TestGateKeysOf(t *testing.T) {
	for _, tc := range []struct {
		name, list, err string
		keys            []string
	}{
		{"well-formed", `{"a.go", "A"}, {"b.go", "B"}`, "", []string{"a.go A", "b.go B"}},
		{"a name for a literal", `{"a.go", "A"}, {"b.go", fnName}`, "permissionGates[schema.PermX] element 1", nil},
		{"three elements", `{"a.go", "A", "C"}`, "permissionGates[schema.PermX] element 0", nil},
		{"not a composite", `{"a.go", "A"}, pair`, "permissionGates[schema.PermX] element 1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nvar permissionGates = map[schema.Permission][][2]string{\n\tschema.PermX: {" + tc.list + "},\n}\n"
			file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := gateKeysOf(file)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("gateKeysOf: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("gateKeysOf error = %v, want it to contain %q", err, tc.err)
			case !slices.Equal(keys, tc.keys):
				t.Errorf("keys = %q, want %q", keys, tc.keys)
			}
		})
	}
}

// TestRowSourcesExcludeGateRows: the file that holds the phrases is never a
// source of rows.
func TestRowSourcesExcludeGateRows(t *testing.T) {
	if slices.Contains(rowSources, "gaterows.go") {
		t.Fatal("rowSources lists gaterows.go")
	}
}
