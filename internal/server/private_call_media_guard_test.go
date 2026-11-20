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
			checkPrivateCallMediaReferences(t, fset, path, file)
		}
	}
}

func checkPrivateCallMediaReferences(t *testing.T, fset *token.FileSet, path string, file *ast.File) {
	t.Helper()
	mediaFile := filepath.Base(path) == "private_call_media.go"
	for _, decl := range file.Decls {
		fn, _ := decl.(*ast.FuncDecl)
		allowed := mediaFile || path == "voice_presence.go" && (fn == nil ||
			fn.Name.Name == "start" || fn.Name.Name == "runSweep" || fn.Name.Name == "orphanRooms")
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if path != "store/private_call_media.go" && n.Kind == token.STRING &&
					(strings.Contains(n.Value, "call_rooms") || strings.Contains(n.Value, "call_room_holders")) {
					t.Errorf("%s: private call room tables must stay in the private media store", fset.Position(n.Pos()))
				}
			case *ast.Ident:
				switch n.Name {
				case "PrivateCallMediaRooms", "PrivateCallRoomStates", "PreparePrivateCallMedia",
					"GrantPrivateCallMedia", "MarkPrivateCallMediaDeleted", "privateCallRoomStates",
					"privateCallMedia", "callMedia", "runPrivateCallMediaPass", "loopPrivateCallMedia",
					"sweepRetiredPrivateCallRooms", "PrivateCallMediaRoomLive", "PrivateCallMediaHolders",
					"callRoomLocks", "lockPrivateCallRoom", "createPrivateCallRoom":
					if !allowed {
						t.Errorf("%s: %s can expose private call rooms to channel voice", fset.Position(n.Pos()), n.Name)
					}
				}
			case *ast.SelectorExpr:
				if mediaFile && path == "private_call_media.go" {
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
