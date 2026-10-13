package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileReadsGoThroughFileVisibleTo is the structural half of the file
// rule (issue #23). A file is a second way to read what a message carries, so
// the only way server code gets a file's record is store.FileVisibleTo, called
// from files.go with the request's reader (readerFor), and the only code that
// derives a path from a file's hash or opens the bytes is files.go. Another
// file doing either would be a second place where a whisper's attachment could
// reach someone it was not meant for.
func TestFileReadsGoThroughFileVisibleTo(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked, sawVisibleTo := 0, false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var method string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				method = fn.Sel.Name
			case *ast.Ident:
				method = fn.Name
			}
			switch method {
			case "FileVisibleTo":
				sawVisibleTo = true
				if name != "files.go" {
					t.Errorf("%s: %s calls FileVisibleTo; only files.go may", name, fset.Position(call.Pos()))
				}
				if len(call.Args) != 3 || !strings.HasPrefix(types.ExprString(call.Args[2]), "readerFor(") {
					t.Errorf("%s: %s passes a reader that is not readerFor(r, ...)", name, fset.Position(call.Pos()))
				}
			case "blobPath", "storeBlob":
				if name != "files.go" {
					t.Errorf("%s: %s uses %s; only files.go touches file bytes", name, fset.Position(call.Pos()), method)
				}
			}
			return true
		})
	}
	if checked < 10 || !sawVisibleTo {
		t.Fatalf("checked %d files, saw FileVisibleTo=%v: the walk is not finding the file handlers", checked, sawVisibleTo)
	}
}
