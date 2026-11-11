package push

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// outboundViolations lists the places in file that would send a request by a
// path other than push.NewClient: a constructed http.Client or http.Transport,
// the package-level client and transport, the package-level request functions,
// or a raw dialer. allowClient and allowDialer say that this file is the one
// that may build them (client.go and dialer.go).
func outboundViolations(fset *token.FileSet, file *ast.File, allowClient, allowDialer bool) []string {
	var out []string
	typeOf := func(e ast.Expr) string {
		if s, ok := e.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok {
				return id.Name + "." + s.Sel.Name
			}
		}
		return ""
	}
	flag := func(pos token.Pos, what string) {
		out = append(out, fset.Position(pos).String()+": "+what)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			switch typeOf(x.Type) {
			case "http.Client", "http.Transport":
				if !allowClient {
					flag(x.Pos(), "builds "+typeOf(x.Type))
				}
			case "net.Dialer", "tls.Dialer":
				if !allowDialer {
					flag(x.Pos(), "builds "+typeOf(x.Type))
				}
			}
		case *ast.ValueSpec:
			// A zero-value http.Client or http.Transport (not a pointer).
			if t := typeOf(x.Type); (t == "http.Client" || t == "http.Transport") && !allowClient {
				flag(x.Pos(), "declares a "+t)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
				if t := typeOf(x.Args[0]); (t == "http.Client" || t == "http.Transport") && !allowClient {
					flag(x.Pos(), "calls new("+t+")")
				}
			}
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name + "." + x.Sel.Name {
			case "http.DefaultClient", "http.DefaultTransport", "http.Get", "http.Post", "http.PostForm", "http.Head":
				flag(x.Pos(), "uses "+id.Name+"."+x.Sel.Name)
			case "net.Dial", "net.DialTimeout", "net.DialTCP", "net.DialUDP", "tls.Dial", "tls.DialWithDialer":
				if !allowDialer {
					flag(x.Pos(), "uses "+id.Name+"."+x.Sel.Name)
				}
			}
		}
		return true
	})
	return out
}

// TestOnlyNewClientSends is the guard behind "every send goes through
// push.NewClient": no non-test file of this package builds an http.Client,
// an http.Transport or a dialer except the two that are the client and the
// dialer, and none uses the default client or the package-level request
// functions. Another client would send without the address pin, the redirect
// rule and the scheme check. It runs the same scan on the server's own push
// files (the check also covers the guard itself, below).
func TestOnlyNewClientSends(t *testing.T) {
	scan := func(dir string, allowed map[string][2]bool) int {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		n := 0
		for _, path := range files {
			base := filepath.Base(path)
			if strings.HasSuffix(base, "_test.go") {
				continue
			}
			if dir != "." && !strings.HasPrefix(base, "push_") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			n++
			allow := allowed[base]
			for _, v := range outboundViolations(fset, file, allow[0], allow[1]) {
				t.Errorf("%s: %s; only push.NewClient may send", path, v)
			}
		}
		return n
	}
	if n := scan(".", map[string][2]bool{"client.go": {true, false}, "dialer.go": {false, true}}); n < 8 {
		t.Fatalf("only %d files scanned; the walk is not finding the package", n)
	}
	if n := scan("..", nil); n < 1 {
		t.Fatal("no server push files scanned")
	}
}

// TestGuardSeesViolations: the scan finds each way of building another
// client, so the guard cannot pass because it looks at nothing.
func TestGuardSeesViolations(t *testing.T) {
	tests := []struct {
		name, src string
		want      int
	}{
		{"client literal", `package p; import "net/http"; var _ = &http.Client{}`, 1},
		{"transport literal", `package p; import "net/http"; var _ = &http.Transport{}`, 1},
		{"new client", `package p; import "net/http"; var _ = new(http.Client)`, 1},
		{"zero value", `package p; import "net/http"; var c http.Client`, 1},
		{"default client", `package p; import "net/http"; var _ = http.DefaultClient`, 1},
		{"package get", `package p; import "net/http"; var _ = http.Get`, 1},
		{"dialer", `package p; import "net"; var _ = &net.Dialer{}`, 1},
		{"net dial", `package p; import "net"; var _ = net.Dial`, 1},
		{"a client pointer in a signature is fine", `package p; import "net/http"; func f(c *http.Client) {}`, 0},
		{"httptest is fine", `package p; import "net/http/httptest"; var _ = httptest.NewTLSServer`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := outboundViolations(fset, file, false, false); len(got) != tt.want {
				t.Errorf("violations = %v, want %d", got, tt.want)
			}
		})
	}
}
