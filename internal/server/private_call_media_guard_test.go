package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Only the private media code and the voice poller's lifecycle/sweep may
// touch call rooms. This also protects the REST and socket paths that build
// presence and occupancy from the poller's channel-only room cache.
func TestPrivateCallMediaIsolatedFromChannelVoice(t *testing.T) {
	fset := token.NewFileSet()
	media, err := parser.ParseFile(fset, "private_call_media.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := privateCallMediaDeclarations(media)
	for _, pattern := range []string{"*.go", "store/*.go"} {
		paths, err := filepath.Glob(pattern)
		if err != nil || len(paths) == 0 {
			t.Fatalf("finding Go source %q: %v", pattern, err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			checkPrivateCallMediaReferences(t, fset, path, file, names)
		}
	}
}

func privateCallMediaDeclarations(file *ast.File) map[string]bool {
	names := make(map[string]bool)
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			names[decl.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names[spec.Name.Name] = true
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						names[name.Name] = true
					}
				}
			}
		}
	}
	return names
}

func allowedPrivateCallMediaReferences(path string, decl ast.Decl) bool {
	switch path {
	case "private_call_media.go", "store/private_call_media.go":
		return true
	case "voice_presence.go":
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return false
		}
		receiver, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		name, ok := receiver.X.(*ast.Ident)
		return ok && name.Name == "voicePoller" && (fn.Name.Name == "start" || fn.Name.Name == "runSweep")
	default:
		return false
	}
}

func privateCallMediaAPI(name string) bool {
	_, suffix, hasPrivateCall := strings.Cut(name, "PrivateCall")
	return hasPrivateCall && strings.Contains(suffix, "Media") || strings.Contains(name, "PrivateCallRoom") ||
		name == "ListParticipantsIfRoomExists" || name == "ObservePrivateCallEndpoints"
}

func checkPrivateCallMediaReferences(t *testing.T, fset *token.FileSet, path string, file *ast.File, names map[string]bool) {
	t.Helper()
	channelVoice := strings.HasPrefix(path, "voice_presence") || strings.HasPrefix(path, "voice_occupancy")
	for _, decl := range file.Decls {
		allowed := allowedPrivateCallMediaReferences(path, decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if path != "store/private_call_media.go" && n.Kind == token.STRING &&
					(strings.Contains(n.Value, "call_rooms") || strings.Contains(n.Value, "call_room_holders")) {
					t.Errorf("%s: private call room tables must stay in the private media store", fset.Position(n.Pos()))
				}
			case *ast.Ident:
				if !allowed && (privateCallMediaAPI(n.Name) || channelVoice && names[n.Name]) {
					t.Errorf("%s: %s can expose private call rooms to channel voice", fset.Position(n.Pos()), n.Name)
				}
			case *ast.SelectorExpr:
				if path == "private_call_media.go" {
					switch n.Sel.Name {
					case "rooms", "roomLocked", "applyRoom", "noteSession", "notify", "notifyOccupancy":
						t.Errorf("%s: private media must stay out of the channel presence cache", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
}
