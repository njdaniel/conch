package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The server half of the run rule's structure (issue #266, decision D8), as
// TestMessageReadsStayInTheVisibilityFunction is for messages. The store
// reads runs and activity only in audience.go (TestOnlyAudienceGoReadsRunRows);
// here, outside the store, nothing may name those tables, every store method
// about runs or activity must be one of the known ones, and each is given the
// request's reader, readerFor(...), so the run condition is always asked for
// the caller and never for a principal some other code chose.
//
// Since issue #267 the MCP tools reach the same functions as the REST routes,
// which take the reader from a sessionCall (calls.go): call.reader is allowed
// too, and a sessionCall's reader may only ever be set to readerFor(...) or to
// a.reader(), the MCP scope's reader of its own authenticated agent.

// runStoreMethods are the store's run methods the server may call. Each takes
// a store.Reader, and the call must pass readerFor(...) or call.reader.
var runStoreMethods = map[string]bool{
	"RunVisible": true, "OwnedRun": true, "ListVisibleRuns": true, "ListVisibleActivity": true,
	"RunAnchorAudience": true, "SessionRunViews": true,
	"CreateRun": true, "SetRunState": true, "AppendRunActivity": true,
	// Interventions inherit their run's visibility (issue #287), so they are
	// held to the same rule: the request's reader, always.
	"InterventionVisible": true, "OwnedIntervention": true, "ListVisibleInterventions": true,
	"ListSessionInterventions": true, "CreateIntervention": true, "AckIntervention": true,
}

// runTableInString matches a string that names a run table: run_activity
// anywhere, or runs in a string that is SQL (it holds a statement keyword),
// whatever the shape of the statement: after FROM or JOIN, in a comma join,
// in an UPDATE ... RETURNING.
var (
	runActivityInString = regexp.MustCompile(`(?i)run_activity`)
	runsInString        = regexp.MustCompile(`(?i)\b(runs|interventions)\b`)
	sqlInString         = regexp.MustCompile(`(?i)\b(select|update|insert|delete|replace|from|join|returning)\b`)
)

func namesRunTable(lit string) bool {
	return runActivityInString.MatchString(lit) || (runsInString.MatchString(lit) && sqlInString.MatchString(lit))
}

// checkRunReads returns what file does that the rule forbids.
func checkRunReads(fset *token.FileSet, path string, file *ast.File) []string {
	var bad []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING && namesRunTable(x.Value) {
				bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" names a run table in a string")
			}
		case *ast.CompositeLit:
			if types.ExprString(x.Type) != "sessionCall" {
				return true
			}
			for _, elt := range x.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" builds a sessionCall without field names")
					break
				}
				if v := types.ExprString(kv.Value); types.ExprString(kv.Key) == "reader" && !strings.HasPrefix(v, "readerFor(") && v != "a.reader()" {
					bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" gives a sessionCall the reader "+v)
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "reader" {
					bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" sets a reader field after the fact")
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, name := types.ExprString(sel.X), sel.Sel.Name
			if runStoreMethods[name] {
				// Whatever the receiver: a copy of the store under another
				// name (st := s.store) is still the store.
				for _, arg := range x.Args {
					if a := types.ExprString(arg); strings.HasPrefix(a, "readerFor(") || a == "call.reader" {
						return true
					}
				}
				bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" calls "+name+" without readerFor(...) or call.reader as its reader")
				return true
			}
			// store.RunParams{} and the like are types, not calls; only a
			// method of the store value is a read.
			if recv == "store" || !strings.HasSuffix(recv, ".store") {
				return true
			}
			if strings.Contains(name, "Run") || strings.Contains(name, "Activity") || strings.Contains(name, "Activities") || strings.Contains(name, "Intervention") {
				bad = append(bad, path+": "+fset.Position(x.Pos()).String()+" calls the store's "+name+", which is not a known run method")
			}
		}
		return true
	})
	return bad
}

func TestRunReadsTakeTheRequestReader(t *testing.T) {
	fset := token.NewFileSet()
	checked, calls := 0, 0
	for _, root := range []string{".", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.ToSlash(path) == "store" {
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
			for _, b := range checkRunReads(fset, path, file) {
				t.Error(b)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && runStoreMethods[sel.Sel.Name] {
					calls++
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 10 || calls < len(runStoreMethods) {
		t.Fatalf("checked %d files and %d run calls; the walk is not finding the server", checked, calls)
	}
}

// The guard must be able to fail: a table named in a string, an unknown run
// method, and a known one given another reader are all caught.
func TestRunReadsGuardCatchesStrangers(t *testing.T) {
	const src = `package server
const q = "SELECT task FROM runs WHERE id = ?"
const q2 = "select * from run_activity"
func a(s *Server) { s.store.RunsSince(ctx, 1) }
func b(s *Server) { s.store.RunVisible(ctx, 1, store.Reader{PrincipalID: 9, Scoped: true}) }
func c(s *Server) { s.store.ListVisibleActivity(ctx, 1, store.ChannelWideOnly, 0, 10) }
func d(s *Server) { st := s.store; st.RunVisible(ctx, 1, store.ChannelWideOnly) }
const q3 = "SELECT ru.task FROM sessions s, runs ru"
const q4 = "UPDATE runs SET state = ? RETURNING task"
const q5 = "SELECT body FROM interventions WHERE id = ?"
func e(s *Server) { s.store.InterventionsSince(ctx, 1) }
func f(s *Server) { s.store.InterventionVisible(ctx, 1, store.ChannelWideOnly) }
func g(s *Server) { _ = sessionCall{caller: p, reader: store.Reader{PrincipalID: 9, Scoped: true}} }
func h(s *Server) { _ = sessionCall{p, 1, store.ChannelWideOnly, "", ""} }
func i(call sessionCall) { call.reader = store.ChannelWideOnly }
func ok(s *Server, r *http.Request) { s.store.RunVisible(ctx, 1, readerFor(r, apiV2)); _ = store.RunParams{} }
func ok2(s *Server, r *http.Request, a *agentScope, call sessionCall) { s.store.OwnedRun(ctx, 1, 2, call.reader); _ = sessionCall{reader: readerFor(r, apiV2)}; _ = sessionCall{reader: a.reader()} }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Join(checkRunReads(fset, "x.go", f), "\n")
	for _, want := range []string{
		"x.go:2:", "x.go:3:", "x.go:4:", "x.go:5:", "x.go:6:", "x.go:7:", "x.go:8:", "x.go:9:", "x.go:10:", "x.go:11:", "x.go:12:",
		"x.go:13:", "x.go:14:", "x.go:15:",
		"gives a sessionCall the reader store.Reader",
		"builds a sessionCall without field names",
		"sets a reader field after the fact",
		"calls the store's RunsSince",
		"calls RunVisible without readerFor(...) or call.reader",
		"calls ListVisibleActivity without readerFor(...) or call.reader",
		"calls the store's InterventionsSince",
		"calls InterventionVisible without readerFor(...) or call.reader",
	} {
		if !strings.Contains(bad, want) {
			t.Errorf("the guard missed %q; it reported:\n%s", want, bad)
		}
	}
	for _, allowed := range []string{"x.go:16:", "x.go:17:"} {
		if strings.Contains(bad, allowed) {
			t.Errorf("the guard refused an allowed call:\n%s", bad)
		}
	}
}
