package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The two places UpdateParticipant may be named: the one function in the
// server that writes a permission, and the client method that sends it.
var (
	serverPermissionFn = allowedFunc{recv: "voicePoller", name: "setPublishPermission"}
	livekitUpdateFn    = allowedFunc{recv: "Client", name: "UpdateParticipant"}
)

// TestUpdateParticipantStaysInThePermissionFunction is the structural half of
// ADR-015 decision 5 (issue #1239, S4): LiveKit replaces a participant's
// permission whole, so a second place that writes one would take down a share
// or, once there is one, a camera. It fails when non-test code in
// internal/server names a selector UpdateParticipant (a call, or a method
// value that could be called later) anywhere but inside
// voicePoller.setPublishPermission. It also reads the livekit package, where
// the call is defined: there the selector, and the string "UpdateParticipant"
// that names the Twirp method, may occur only inside (*Client).UpdateParticipant,
// so a helper that builds the same request itself cannot escape the guard.
func TestUpdateParticipantStaysInThePermissionFunction(t *testing.T) {
	fset := token.NewFileSet()
	checked, inLiveKit := 0, 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		allowed := serverPermissionFn
		if filepath.ToSlash(filepath.Dir(path)) == "livekit" {
			allowed = livekitUpdateFn
			inLiveKit++
		}
		checked++
		for _, stray := range strayUpdateParticipants(file, allowed) {
			t.Errorf("%s: UpdateParticipant is named in %s; every publish permission is written by voicePoller.setPublishPermission through (*livekit.Client).UpdateParticipant (ADR-015 decision 5)", fset.Position(stray.pos), stray.within)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 || inLiveKit == 0 {
		t.Fatalf("only %d source files were checked, %d of them in livekit; the walk is not finding the server", checked, inLiveKit)
	}
}

// TestUpdateParticipantGuardCatchesAStrayCall proves the guard itself: a
// fixture with a second caller, in a function, in a closure and as a method
// value, is caught each time, and the one function is not. The livekit
// fixture shows a helper that builds the Twirp call itself, or calls the
// method from another, is caught too.
func TestUpdateParticipantGuardCatchesAStrayCall(t *testing.T) {
	tests := []struct {
		name    string
		allowed allowedFunc
		src     string
		want    []string
	}{
		{"server", serverPermissionFn, `package server

type voicePoller struct{ s *server }

func (p *voicePoller) setPublishPermission() { p.s.lk.UpdateParticipant() }

func (p *voicePoller) other() { p.s.lk.UpdateParticipant() }

func (p *voicePoller) inClosure() { go func() { p.s.lk.UpdateParticipant() }() }

func (p *voicePoller) asValue() { f := p.s.lk.UpdateParticipant; _ = f }

var pkgLevel = lk.UpdateParticipant

func setPublishPermission() { lk.UpdateParticipant() } // a function of that name that is not the method
`, []string{"other", "inClosure", "asValue", "package level", "setPublishPermission"}},
		{"livekit", livekitUpdateFn, `package livekit

type Client struct{}

func (c *Client) UpdateParticipant() { c.call(ctx, "UpdateParticipant", g, req, &a) }

// SetCamera builds the Twirp call itself.
func (c *Client) SetCamera() error { return c.call(ctx, "UpdateParticipant", g, req, nil) }

func (c *Client) Wrapper() { c.UpdateParticipant() }

func UpdateParticipant() { c.call(ctx, "UpdateParticipant", g, req, nil) } // not the method

var name = "UpdateParticipantX" // another string is fine
`, []string{"SetCamera", "Wrapper", "UpdateParticipant"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, stray := range strayUpdateParticipants(file, tt.allowed) {
				got = append(got, stray.within)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("the guard found strays in %q, want %q", got, tt.want)
			}
		})
	}
}

type strayCall struct {
	pos    token.Pos
	within string
}

// allowedFunc is the one pointer method that may name UpdateParticipant.
type allowedFunc struct{ recv, name string }

// strayUpdateParticipants lists the selectors named UpdateParticipant, and the
// string literal "UpdateParticipant", in file that are not inside the method
// allowed.
func strayUpdateParticipants(file *ast.File, allowed allowedFunc) []strayCall {
	var out []strayCall
	for _, decl := range file.Decls {
		within := "package level"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			within = fn.Name.Name
			if fn.Name.Name == allowed.name && isPointerMethodOf(fn, allowed.recv) {
				continue
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "UpdateParticipant" {
					out = append(out, strayCall{n.Sel.Pos(), within})
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && (n.Value == `"UpdateParticipant"` || n.Value == "`UpdateParticipant`") {
					out = append(out, strayCall{n.Pos(), within})
				}
			}
			return true
		})
	}
	return out
}

func isPointerMethodOf(fn *ast.FuncDecl, recv string) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == recv
}
