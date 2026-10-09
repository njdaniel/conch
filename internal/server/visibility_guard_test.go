package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestMessageReadsStayInTheVisibilityFunction is the structural half of the
// "one visibility function" rule (issue #116, ADR-005). A scoped message leaks
// the moment any code reads messages without going through
// store.ListVisibleMessages, so this fails when non-test code outside the
// store:
//
//   - imports database/sql or the SQLite driver (the only other way to read the
//     messages table),
//   - names the messages or message_recipients table in a string,
//   - calls a message-listing or message-counting method on anything but
//     ListVisibleMessages, or
//   - calls ListVisibleMessages with a reader that is not either
//     store.ChannelWideOnly or the one request-derived constructor readerFor.
//
// Tests are exempt: they read the log to assert on it.
func TestMessageReadsStayInTheVisibilityFunction(t *testing.T) {
	roots := []string{".", "../../cmd"}
	fset := token.NewFileSet()
	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The store is where messages are read; it has its own guard.
				if filepath.ToSlash(path) == "store" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			checked++
			checkMessageReads(t, fset, path, file)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d source files were checked; the walk is not finding the server", checked)
	}
}

func checkMessageReads(t *testing.T, fset *token.FileSet, path string, file *ast.File) {
	t.Helper()
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if p == "database/sql" || strings.HasPrefix(p, "modernc.org/sqlite") {
			t.Errorf("%s imports %s; messages must be read through the store's visibility function", path, p)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				lit := strings.ToLower(x.Value)
				for _, table := range []string{"from messages", "join messages", "message_recipients", "into messages"} {
					if strings.Contains(lit, table) {
						t.Errorf("%s: %s names a message table in a string", path, fset.Position(x.Pos()))
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			listsMessages := strings.Contains(name, "Message") &&
				(strings.HasPrefix(name, "List") || strings.HasPrefix(name, "Read") || strings.HasPrefix(name, "Get") ||
					strings.HasPrefix(name, "Fetch") || strings.HasPrefix(name, "Query") || strings.HasPrefix(name, "Search") ||
					strings.HasPrefix(name, "Since") || strings.HasPrefix(name, "Recent") ||
					// A count includes scoped messages, so serving one would tell a
					// reader that messages exist which they cannot see.
					strings.HasPrefix(name, "Count"))
			if !listsMessages {
				return true
			}
			if name != "ListVisibleMessages" {
				// The Client types in internal/cli list over HTTP; nothing under
				// the walked roots has such a method, so any other is a store read.
				t.Errorf("%s: %s calls %s; use ListVisibleMessages so the audience filter applies", path, fset.Position(x.Pos()), name)
				return true
			}
			if len(x.Args) < 3 {
				t.Errorf("%s: %s calls ListVisibleMessages with the wrong arguments", path, fset.Position(x.Pos()))
				return true
			}
			switch reader := types.ExprString(x.Args[2]); {
			case reader == "store.ChannelWideOnly", strings.HasPrefix(reader, "readerFor("):
			default:
				t.Errorf("%s: %s passes reader %q to ListVisibleMessages; use readerFor(r, version) or store.ChannelWideOnly", path, fset.Position(x.Pos()), reader)
			}
		}
		return true
	})
}
