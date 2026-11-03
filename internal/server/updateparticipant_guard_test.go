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

// TestUpdateParticipantStaysInThePermissionFunction is the structural half of
// ADR-015 decision 5 (issue #1239, S4): LiveKit replaces a participant's
// permission whole, so a second place that writes one would take down a share
// or, once there is one, a camera. It fails when non-test code in
// internal/server, outside the livekit package that defines the call, names a
// selector UpdateParticipant (a call, or a method value that could be called
// later) anywhere but inside voicePoller.setPublishPermission.
func TestUpdateParticipantStaysInThePermissionFunction(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.ToSlash(path) == "livekit" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		for _, stray := range strayUpdateParticipants(file) {
			t.Errorf("%s: UpdateParticipant is named in %s; every publish permission is written by voicePoller.setPublishPermission (ADR-015 decision 5)", fset.Position(stray.pos), stray.within)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("only %d source files were checked; the walk is not finding the server", checked)
	}
}

// TestUpdateParticipantGuardCatchesAStrayCall proves the guard itself: a
// fixture with a second caller, in a function, in a closure and as a method
// value, is caught each time, and the one function is not.
func TestUpdateParticipantGuardCatchesAStrayCall(t *testing.T) {
	const src = `package server

type voicePoller struct{ s *server }

func (p *voicePoller) setPublishPermission() { p.s.lk.UpdateParticipant() }

func (p *voicePoller) other() { p.s.lk.UpdateParticipant() }

func (p *voicePoller) inClosure() { go func() { p.s.lk.UpdateParticipant() }() }

func (p *voicePoller) asValue() { f := p.s.lk.UpdateParticipant; _ = f }

var pkgLevel = lk.UpdateParticipant

func setPublishPermission() { lk.UpdateParticipant() } // a function of that name that is not the method
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, stray := range strayUpdateParticipants(file) {
		got = append(got, stray.within)
	}
	want := []string{"other", "inClosure", "asValue", "package level", "setPublishPermission"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the guard found strays in %q, want %q", got, want)
	}
}

type strayCall struct {
	pos    token.Pos
	within string
}

// strayUpdateParticipants lists the selectors named UpdateParticipant in file
// that are not inside the method (*voicePoller).setPublishPermission.
func strayUpdateParticipants(file *ast.File) []strayCall {
	var out []strayCall
	for _, decl := range file.Decls {
		within, allowed := "package level", false
		if fn, ok := decl.(*ast.FuncDecl); ok {
			within = fn.Name.Name
			allowed = fn.Name.Name == "setPublishPermission" && isVoicePollerMethod(fn)
		}
		if allowed {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "UpdateParticipant" {
				out = append(out, strayCall{sel.Sel.Pos(), within})
			}
			return true
		})
	}
	return out
}

func isVoicePollerMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "voicePoller"
}
