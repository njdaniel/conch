package server

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Decision D9 (issue #265): agentManifestCheck refuses a call that names no
// channel, on purpose, and sessions name none. So there is exactly one other
// check, agentSelfCheck, that asks for a capability alone; it is safe only for
// calls that act on the caller's own session. These tests pin both halves:
// the old check is still strict, and the new one is reachable only from the
// three session handlers.

// Whatever it grants, the manifest gate for a channel call stays closed to a
// call that names no channel. If this fails, someone gave agentManifestCheck a
// "no channel" branch; write a separate, named check instead.
func TestAgentManifestCheckStillRefusesNoChannel(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		channelID int64
	}{
		{"no channel", 0},
		{"a negative id", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			// bot's manifest grants every capability, sessions.manage included.
			_, allowed, err := f.srv.agentManifestCheck(ctx, f.bot.ID, "test", schema.CapabilitySessionsManage, c.channelID, schema.ChannelPermissionRead)
			if err != nil || allowed {
				t.Errorf("agentManifestCheck(channel %d) = %v, %v; want refused", c.channelID, allowed, err)
			}
		})
	}
	// While the same agent, with a channel it holds, is allowed.
	if _, allowed, err := f.srv.agentManifestCheck(ctx, f.bot.ID, "test", schema.CapabilitySessionsManage, f.general.ID, schema.ChannelPermissionRead); err != nil || !allowed {
		t.Errorf("agentManifestCheck(general) = %v, %v; want allowed", allowed, err)
	}
}

// agentSelfCheck asks for the capability and nothing else, fails closed, and
// audits what it refuses.
func TestAgentSelfCheck(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	nomanifest, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "nomanifest")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		principal  int64
		capability schema.Capability
		want       bool
		reason     string
	}{
		{"granted", f.bot.ID, schema.CapabilitySessionsManage, true, ""},
		{"not granted", f.mute.ID, schema.CapabilitySessionsManage, false, denyCapability},
		{"no manifest", nomanifest.ID, schema.CapabilitySessionsManage, false, denyNoManifest},
		{"an unknown principal", 999, schema.CapabilitySessionsManage, false, denyNoManifest},
		{"an unknown capability", f.bot.ID, schema.Capability("nonsense"), false, denyCapability},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.auditRows(t, "access_denied"))
			m, allowed, err := f.srv.agentSelfCheck(ctx, tt.principal, "test", tt.capability)
			if err != nil || allowed != tt.want {
				t.Fatalf("agentSelfCheck = %v, %v; want %v", allowed, err, tt.want)
			}
			rows := f.auditRows(t, "access_denied")
			if tt.want {
				if len(rows) != before || !m.Allows(tt.capability) {
					t.Errorf("an allowed call audited or returned a manifest that does not grant: %d rows", len(rows)-before)
				}
				return
			}
			if len(rows) != before+1 || !strings.Contains(rows[len(rows)-1].Detail, "reason="+tt.reason) {
				t.Errorf("denial rows = %+v, want one with reason %s", rows[before:], tt.reason)
			}
		})
	}
}

// guardedCallers maps a guarded function to the only functions that may refer
// to it. A reference that is not a call (passing it as a value) counts: it
// would be a way round the list.
var guardedCallers = map[string][]string{
	"agentSelfCheck": {"agentSelfAllowed"},
	// ownSessionCaller asks for sessions.manage; the adapter's poll for the
	// interventions on its own session's runs asks for runs.manage, the
	// capability that governs runs, and so calls agentSelfAllowed itself
	// (issue #287, decision D9). It names no channel, acts on the caller's own
	// session and reaches nothing of anyone else's, which is all the check is
	// for; the store's statement narrows the rows to the caller's credential.
	"agentSelfAllowed": {"ownSessionCaller", "handleListSessionInterventions"},
	// The routes that act on the caller's own session, and nothing else.
	"ownSessionCaller": {"handleCreateSession", "handleSessionHeartbeat", "handleEndSession"},
}

// referrers returns, for each guarded name, the functions of files that refer
// to it (package-level initializers count as "<package>").
func referrers(files []*ast.File, guarded map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for name := range guarded {
		out[name] = map[string]bool{}
	}
	scan := func(owner string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			var name string
			switch x := n.(type) {
			case *ast.Ident:
				name = x.Name
			case *ast.SelectorExpr:
				name = x.Sel.Name
			default:
				return true
			}
			if set, ok := out[name]; ok {
				set[owner] = true
			}
			return true
		})
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scan(d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				scan("<package>", d)
			}
		}
	}
	return out
}

// violations lists the referrers of each guarded function that are not on its
// list, and the listed ones that do not refer to it (a stale list guards
// nothing).
func violations(files []*ast.File, guarded map[string][]string) []string {
	var bad []string
	got := referrers(files, guarded)
	for name, allowed := range guarded {
		ok := map[string]bool{}
		for _, a := range allowed {
			ok[a] = true
			if !got[name][a] {
				bad = append(bad, a+" is listed for "+name+" but does not call it")
			}
		}
		for who := range got[name] {
			if !ok[who] {
				bad = append(bad, who+" refers to "+name)
			}
		}
	}
	sort.Strings(bad)
	return bad
}

func parseNonTestFiles(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no files")
	}
	return files
}

// TestAgentSelfCheckCallers fails if any function outside the named handlers
// reaches agentSelfCheck: a new route that wants to use it must be added to
// guardedCallers in a reviewed change, not slip in.
func TestAgentSelfCheckCallers(t *testing.T) {
	if bad := violations(parseNonTestFiles(t), guardedCallers); len(bad) > 0 {
		t.Errorf("the own-session check is reachable from outside its named callers (decision D9):\n  %s", strings.Join(bad, "\n  "))
	}
}

// The guard must be able to fail: a function outside the list, a value
// reference, a method call and a package-level initializer are all caught.
func TestAgentSelfCheckGuardCatchesStrangers(t *testing.T) {
	const src = `package server
var table = map[string]any{"x": agentSelfCheck}
func (s *Server) handleListSessions() { s.agentSelfCheck() }
func handleCreateSession() { ownSessionCaller() }
func sneaky() { f := agentSelfAllowed; f() }
func agentSelfAllowed() { agentSelfCheck() }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Join(violations([]*ast.File{f}, guardedCallers), "\n")
	for _, want := range []string{
		"<package> refers to agentSelfCheck",
		"handleListSessions refers to agentSelfCheck",
		"sneaky refers to agentSelfAllowed",
		"ownSessionCaller is listed for agentSelfAllowed but does not call it",
		"handleSessionHeartbeat is listed for ownSessionCaller but does not call it",
	} {
		if !strings.Contains(bad, want) {
			t.Errorf("the guard missed %q; it reported:\n%s", want, bad)
		}
	}
}

// The handlers on the list are the routes that act on the caller's own
// session, and the session routes are the four of the issue: the list and the
// route table cannot drift apart unnoticed.
func TestOwnSessionRoutesAreTheListedHandlers(t *testing.T) {
	want := map[string]bool{"handleCreateSession": true, "handleSessionHeartbeat": true, "handleEndSession": true}
	for _, h := range guardedCallers["ownSessionCaller"] {
		if !want[h] {
			t.Errorf("%s is on the list but is not an own-session handler", h)
		}
	}
	if len(guardedCallers["ownSessionCaller"]) != len(want) {
		t.Errorf("list = %v, want exactly %v", guardedCallers["ownSessionCaller"], want)
	}
	f := newSessionFixture(t)
	var sessionRoutes []string
	for _, rt := range f.srv.routes {
		if strings.Contains(rt.pattern, "/v1/sessions") {
			sessionRoutes = append(sessionRoutes, rt.pattern)
		}
	}
	sort.Strings(sessionRoutes)
	// Starting a run (issue #266) is under a session's path but is not an
	// own-session call: it is gated by the channel gate with runs.manage
	// (runGate), not by agentSelfCheck.
	// Likewise polling a session's interventions (issue #287): it is under the
	// session's path and acts on the caller's own session, but it asks for
	// runs.manage and so is listed for agentSelfAllowed, not ownSessionCaller.
	wantRoutes := []string{"DELETE /v1/sessions/{id}", "GET /v1/sessions", "GET /v1/sessions/{id}/interventions", "POST /v1/sessions", "POST /v1/sessions/{id}/heartbeat", "POST /v1/sessions/{id}/runs"}
	if strings.Join(sessionRoutes, "|") != strings.Join(wantRoutes, "|") {
		t.Errorf("session routes = %v, want %v", sessionRoutes, wantRoutes)
	}
}

// The sweeper runs on the tick channel it is given, on the server's clock, and
// stops when its context ends. Two ticks are sent so that the second send
// returns only after the first pass is done.
func TestSessionSweeperLoop(t *testing.T) {
	f := newSessionFixture(t)
	sess := f.create(t, f.aliceTok, humanBody)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.runSessionSweeper(ctx, ticks)
	}()

	f.clock.Advance(31 * time.Second)
	ticks <- time.Time{}
	ticks <- time.Time{}
	if s := f.rosterByID(t, f.rootTok)[sess.ID]; s.State != schema.SessionStateStale {
		t.Errorf("after a tick at +31s: %s, want stale", s.State)
	}
	f.clock.Advance(60 * time.Second)
	ticks <- time.Time{}
	ticks <- time.Time{}
	if s := f.rosterByID(t, f.rootTok)[sess.ID]; s.State != schema.SessionStateOffline {
		t.Errorf("after a tick at +91s: %s, want offline", s.State)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper did not stop when its context ended")
	}
}

// startSessionSweeper's stop function returns only when the goroutine is gone.
func TestSessionSweeperStopWaits(t *testing.T) {
	srv := newTestServer(t)
	srv.sessionSweepInterval = time.Millisecond
	stop := srv.startSessionSweeper(context.Background())
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
}

// Serve starts the sweeper (a session that goes silent goes offline with no
// one calling sweepSessions), and returns only after it has stopped.
func TestServeRunsTheSessionSweeper(t *testing.T) {
	f := newSessionFixture(t)
	f.srv.sessionSweepInterval = 5 * time.Millisecond
	sess := f.create(t, f.aliceTok, humanBody)
	f.clock.Advance(2 * time.Minute)
	if err := f.srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.srv.Serve(ctx) }()

	// The only wait in these tests: a real ticker has to fire once.
	deadline := time.Now().Add(10 * time.Second)
	for len(f.auditRows(t, store.AuditSessionOffline)) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Serve never ran the sweeper")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return")
	}
	rows := f.auditRows(t, store.AuditSessionOffline)
	if len(rows) != 1 || rows[0].Subject != fmt.Sprintf("session:%d", sess.ID) {
		t.Errorf("session_offline rows = %+v, want exactly one for session %d", rows, sess.ID)
	}
}
