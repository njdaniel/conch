package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// authFixture is a server with an operator ("root", id 1), two members
// ("alice" human, "bot" agent) and a channel "general", each principal holding
// one credential.
type authFixture struct {
	srv      *Server
	root     store.Principal
	alice    store.Principal
	bot      store.Principal
	rootTok  string
	aliceTok string
	botTok   string
}

func newAuthFixture(t *testing.T, mode AuthMode) *authFixture {
	t.Helper()
	srv := newTestServerWithConfig(t, Config{AuthMode: mode})
	ctx := context.Background()
	root, _, rootTok, err := srv.store.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatalf("BootstrapOperator: %v", err)
	}
	f := &authFixture{srv: srv, root: root, rootTok: rootTok}
	mk := func(kind store.PrincipalKind, name string) (store.Principal, string) {
		p, err := srv.store.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatalf("CreatePrincipal %s: %v", name, err)
		}
		_, tok, err := srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatalf("CreateCredential %s: %v", name, err)
		}
		return p, tok
	}
	f.alice, f.aliceTok = mk(store.PrincipalHuman, "alice")
	f.bot, f.botTok = mk(store.PrincipalAgent, "bot")
	// A fourth principal that belongs to nothing, for membership routes.
	if _, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "ghost"); err != nil {
		t.Fatalf("CreatePrincipal ghost: %v", err)
	}
	general, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Since issue #90 a new channel has no members; the route matrix below
	// treats "general" as the channel every fixture caller may use.
	for _, p := range []store.Principal{root, f.alice, f.bot} {
		if _, err := srv.store.AddChannelMember(ctx, "system", general.ID, p.ID, 0); err != nil {
			t.Fatalf("AddChannelMember %s: %v", p.Name, err)
		}
	}
	// Since issue #79 an agent also needs a manifest; the fixture agent may do
	// everything in "general".
	setAgentManifest(t, srv, f.bot.ID, nil, general.ID)
	return f
}

func (f *authFixture) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) audit(t *testing.T) []store.AuditEvent {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// routeClass is the independently stated expectation for one route, used to
// check the route table rather than derive from it.
type routeClass string

const (
	classExempt routeClass = "exempt"
	classAuth   routeClass = "auth"
	classOp     routeClass = "operator"
	classSelf   routeClass = "operator-or-self"
)

// routeExpectation pins, per route pattern: its class, the request to send,
// and the status an authorized caller gets (the same status AuthOff gives).
type routeExpectation struct {
	class routeClass
	path  string
	body  string
	// allowed is the status an authorized caller gets, which is also what the
	// route returns with AuthOff (whoami excepted: it needs a caller).
	allowed int
}

// routeExpectations must name every pattern in the route table. {id} is the
// operator's principal id (1) so operator-or-self is exercised from a member.
var routeExpectations = map[string]routeExpectation{
	"GET /healthz":      {classExempt, "/healthz", "", 200},
	"GET /v0/ws":        {classAuth, "/v0/ws", "", 400},
	"GET /v1/ws":        {classAuth, "/v1/ws", "", 400},
	"GET /v1/whoami":    {classAuth, "/v1/whoami", "", 200},
	"POST /v0/channels": {classOp, "/v0/channels", `{}`, 400},
	"GET /v1/channels":  {classAuth, "/v1/channels", "", 200},
	// Both membership writes are no-ops in the fixture (alice is already a
	// member; ghost, id 4, is not), so they change nothing and audit nothing.
	"GET /v1/channels/{channel}/members":                   {classAuth, "/v1/channels/general/members", "", 200},
	"PUT /v1/channels/{channel}/members/{principal_id}":    {classOp, "/v1/channels/general/members/2", "", 204},
	"DELETE /v1/channels/{channel}/members/{principal_id}": {classOp, "/v1/channels/general/members/4", "", 204},
	"POST /v0/principals":                                  {classOp, "/v0/principals", `{}`, 400},
	"POST /v0/channels/{channel}/messages":                 {classAuth, "/v0/channels/general/messages", `{}`, 400},
	"GET /v0/channels/{channel}/messages":                  {classAuth, "/v0/channels/general/messages", "", 200},
	"POST /v1/channels/{channel}/messages":                 {classAuth, "/v1/channels/general/messages", `{}`, 400},
	"GET /v1/channels/{channel}/messages":                  {classAuth, "/v1/channels/general/messages", "", 200},
	"PUT /v1/principals/{id}/manifest":                     {classOp, "/v1/principals/1/manifest", `{}`, 400},
	"GET /v1/principals/{id}/manifest":                     {classSelf, "/v1/principals/1/manifest", "", 404},
	"POST /v1/principals/{id}/credentials":                 {classOp, "/v1/principals/1/credentials", `{}`, 400},
	"GET /v1/principals/{id}/credentials":                  {classOp, "/v1/principals/1/credentials", "", 200},
	"POST /v1/credentials/{credential_id}/rotate":          {classOp, "/v1/credentials/9999/rotate", "", 404},
	"DELETE /v1/credentials/{credential_id}":               {classOp, "/v1/credentials/9999", "", 404},
	// Unknown principal / already-enabled ghost: no-ops that write no audit
	// event, so the AuthOff audit count below is unaffected.
	"POST /v1/principals/{id}/disable":                {classOp, "/v1/principals/9999/disable", "", 404},
	"POST /v1/principals/{id}/enable":                 {classOp, "/v1/principals/4/enable", "", 204},
	"POST /v1/principals/{id}/credentials/revoke-all": {classOp, "/v1/principals/9999/credentials/revoke-all", "", 404},
	"POST /v1/hooks":                                  {classOp, "/v1/hooks", `{}`, 400},
	"POST /v1/hooks/{token}":                          {classExempt, "/v1/hooks/nope", `{}`, 404},
	"POST /v1/approvals":                              {classAuth, "/v1/approvals", `{}`, 400},
	"GET /v1/approvals":                               {classAuth, "/v1/approvals", "", 200},
	"POST /v1/approvals/{id}/decisions":               {classAuth, "/v1/approvals/1/decisions", `{}`, 400},
	"/mcp":                                            {classExempt, "/mcp", `{}`, 401},
}

func routeMethod(pattern string) string {
	if method, _, ok := strings.Cut(pattern, " "); ok {
		return method
	}
	return http.MethodPost
}

// TestRouteTable ties the route table to the expectations above and to the mux
// registration, so a route added without an authentication decision fails.
func TestRouteTable(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)

	var got []string
	for _, rt := range f.srv.routes {
		got = append(got, rt.pattern)
		exp, ok := routeExpectations[rt.pattern]
		if !ok {
			t.Errorf("route %q has no entry in routeExpectations: decide its authentication class", rt.pattern)
			continue
		}
		want := map[routeClass]access{classExempt: accessExempt, classAuth: accessAuthenticated, classOp: accessOperator, classSelf: accessOperatorOrSelf}[exp.class]
		if rt.access != want {
			t.Errorf("route %q access = %d, expectation says %s", rt.pattern, rt.access, exp.class)
		}
	}
	var want []string
	for p := range routeExpectations {
		want = append(want, p)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("route table and routeExpectations differ\ntable:\n%s\nexpectations:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Nothing may register on the mux except through the table.
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "mux.Handle"); n != 1 {
		t.Errorf("server.go has %d mux.Handle* calls, want exactly 1 (the loop over the route table)", n)
	}
	if n := strings.Count(string(src), "http.NewServeMux"); n != 1 {
		t.Errorf("server.go has %d http.NewServeMux calls, want 1", n)
	}
}

// TestRoutesRequireAuth walks the real route table under AuthRequired: every
// route not on the explicit exempt list answers 401 without a credential.
func TestRoutesRequireAuth(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	exemptList := map[string]bool{"GET /healthz": true, "POST /v1/hooks/{token}": true, "/mcp": true}

	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		t.Run(rt.pattern, func(t *testing.T) {
			rec := f.do(t, routeMethod(rt.pattern), exp.path, "", exp.body)
			if exemptList[rt.pattern] {
				if rec.Header().Get("WWW-Authenticate") == `Bearer realm="conch"` {
					t.Errorf("exempt route answered with the conch 401: %d %s", rec.Code, rec.Body)
				}
				if rec.Code != exp.allowed {
					t.Errorf("exempt route status = %d, want %d", rec.Code, exp.allowed)
				}
				return
			}
			assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}

	// Unmatched paths and methods are not exempt either: they fail closed.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/nope"}, {"DELETE", "/healthz"}, {"GET", "/"}, {"GET", "/v0/../v1/whoami"},
	} {
		rec := f.do(t, c.method, c.path, "", "")
		assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
	}
}

// TestRoleMatrix sends each route as operator, member and nobody under
// AuthRequired and checks the status of each.
func TestRoleMatrix(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		if exp.class == classExempt {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			method := routeMethod(rt.pattern)
			memberWant := exp.allowed
			if exp.class == classOp || exp.class == classSelf {
				memberWant = http.StatusForbidden
			}
			// The approval REST routes are the human surface: an agent
			// credential is refused there and uses MCP instead (issue #79).
			agentWant := memberWant
			if strings.Contains(rt.pattern, "/v1/approvals") {
				agentWant = http.StatusForbidden
			}
			cases := []struct {
				who   string
				token string
				want  int
			}{
				{"unauthenticated", "", http.StatusUnauthorized},
				{"member human", f.aliceTok, memberWant},
				{"member agent", f.botTok, agentWant},
				{"operator", f.rootTok, exp.allowed},
			}
			for _, c := range cases {
				rec := f.do(t, method, exp.path, c.token, exp.body)
				if rec.Code != c.want {
					t.Errorf("%s: status = %d, want %d; body %s", c.who, rec.Code, c.want, rec.Body)
				}
			}
		})
	}
}

// TestAuthOffUnchanged runs the route table with AuthOff and no credential:
// the statuses are what the handlers gave before authentication existed, no
// route reads a credential, and no role check applies.
func TestAuthOffUnchanged(t *testing.T) {
	for _, mode := range []AuthMode{"", AuthOff} {
		f := newAuthFixture(t, mode)
		for _, rt := range f.srv.routes {
			exp := routeExpectations[rt.pattern]
			t.Run(fmt.Sprintf("%q/%s", mode, rt.pattern), func(t *testing.T) {
				want := exp.allowed
				if rt.pattern == "GET /v1/whoami" {
					want = http.StatusUnauthorized // new route; no caller to report
				}
				rec := f.do(t, routeMethod(rt.pattern), exp.path, "", exp.body)
				if rec.Code != want {
					t.Errorf("status = %d, want %d; body %s", rec.Code, want, rec.Body)
				}
				// A garbage credential is not read at all under AuthOff.
				rec = f.do(t, routeMethod(rt.pattern), exp.path, "not-a-credential", exp.body)
				if rec.Code != want {
					t.Errorf("with garbage credential: status = %d, want %d", rec.Code, want)
				}
			})
		}
		// Bootstrap writes 2 audit events, alice's and bot's credentials 1 each,
		// the fixture's three member_added events, and the fixture agent's
		// manifest_created; no request may add to that (in particular no
		// access_denied).
		if n := len(f.audit(t)); n != 8 {
			t.Errorf("mode %q: audit events = %d, want 8", mode, n)
		}
	}
}

func TestParseAuthMode(t *testing.T) {
	tests := []struct {
		in      string
		want    AuthMode
		wantErr bool
	}{
		{"off", AuthOff, false},
		{"required", AuthRequired, false},
		{"", "", true},
		{"on", "", true},
		{"Required", "", true},
		{"true", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAuthMode(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseAuthMode(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

// An unrecognized mode that bypasses ParseAuthMode must not open the server.
func TestUnknownAuthModeFailsClosed(t *testing.T) {
	f := newAuthFixture(t, AuthMode("bogus"))
	assertErrorBody(t, f.do(t, "GET", "/v1/channels", "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestCredentialFailureModes(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()

	revoked, revokedTok := mustCredential(t, f, f.alice.ID, nil)
	if err := f.srv.store.RevokeCredential(ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(150 * time.Millisecond)
	_, expiredTok := mustCredential(t, f, f.alice.ID, &soon)
	time.Sleep(300 * time.Millisecond)
	unknownTok := schema.CredentialTokenPrefix + strings.Repeat("A", 43)

	tests := []struct {
		name   string
		header string
		url    string
		cookie string
	}{
		{"missing header", "", "/v1/whoami", ""},
		{"wrong scheme", "Basic " + f.aliceTok, "/v1/whoami", ""},
		{"token without scheme", f.aliceTok, "/v1/whoami", ""},
		{"empty bearer", "Bearer ", "/v1/whoami", ""},
		{"malformed token", "Bearer hello", "/v1/whoami", ""},
		{"truncated token", "Bearer " + f.aliceTok[:20], "/v1/whoami", ""},
		{"padded token", "Bearer " + f.aliceTok + " x", "/v1/whoami", ""},
		{"unknown token", "Bearer " + unknownTok, "/v1/whoami", ""},
		{"expired token", "Bearer " + expiredTok, "/v1/whoami", ""},
		{"revoked token", "Bearer " + revokedTok, "/v1/whoami", ""},
		{"valid token in query only", "", "/v1/whoami?token=" + f.aliceTok + "&access_token=" + f.aliceTok, ""},
		{"valid token in cookie only", "", "/v1/whoami", "conch_token=" + f.aliceTok},
	}
	var first string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if tt.cookie != "" {
				req.Header.Set("Cookie", tt.cookie)
			}
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if first == "" {
				first = rec.Body.String()
			} else if rec.Body.String() != first {
				t.Errorf("body differs from the first failure mode:\n got %s\nwant %s", rec.Body, first)
			}
		})
	}

	// The valid token works in the header, with any scheme case.
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest("GET", "/v1/whoami", nil)
		req.Header.Set("Authorization", scheme+" "+f.aliceTok)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("scheme %q: status = %d", scheme, rec.Code)
		}
	}
	if n := countAction(f.audit(t), "access_denied"); n != 0 {
		t.Errorf("401s wrote %d access_denied audit events, want 0", n)
	}
}

func mustCredential(t *testing.T, f *authFixture, principalID int64, exp *time.Time) (schema.CredentialV1, string) {
	t.Helper()
	c, tok, err := f.srv.store.CreateCredential(context.Background(), "system", principalID, "extra", exp)
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

func countAction(events []store.AuditEvent, action string) int {
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestMiddlewarePassesResolvedPrincipal(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name  string
		token string
		want  store.Principal
	}{
		{"operator", f.rootTok, f.root},
		{"member human", f.aliceTok, f.alice},
		{"member agent", f.botTok, f.bot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got store.Principal
			var seen bool
			marker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, seen = callerFrom(r.Context())
			})
			req := httptest.NewRequest("GET", "/v1/whoami", nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			f.srv.authMiddleware(marker).ServeHTTP(httptest.NewRecorder(), req)
			if !seen || got.ID != tt.want.ID || got.Role != tt.want.Role || got.Kind != tt.want.Kind {
				t.Errorf("principal = %+v (seen %v), want %+v", got, seen, tt.want)
			}
		})
	}
}

// A database failure while resolving a credential is a 500, never an
// authenticated request, and the downstream handler is not reached.
func TestResolveStoreFailureIs500(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	reached := false
	marker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+f.aliceTok) // well-formed, so it reaches the database
	rec := httptest.NewRecorder()
	f.srv.authMiddleware(marker).ServeHTTP(rec, req)
	assertErrorBody(t, rec, http.StatusInternalServerError, "internal_error")
	if reached {
		t.Error("handler reached after a store failure")
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("a 500 must not look like a credential rejection")
	}
}

func TestAuthorBinding(t *testing.T) {
	tests := []struct {
		name   string
		body   func(f *authFixture) string
		status int
		code   string
		author func(f *authFixture) int64
	}{
		{"absent", func(*authFixture) string { return `{"body":"hi"}` }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"zero", func(*authFixture) string { return `{"author_id":0,"body":"hi"}` }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"matching", func(f *authFixture) string { return fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.alice.ID) }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"mismatched", func(f *authFixture) string { return fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.root.ID) }, 403, "author_mismatch", nil},
		{"negative", func(*authFixture) string { return `{"author_id":-1,"body":"hi"}` }, 403, "author_mismatch", nil},
	}
	for _, version := range []string{"v0", "v1"} {
		for _, tt := range tests {
			t.Run(version+"/"+tt.name, func(t *testing.T) {
				f := newAuthFixture(t, AuthRequired)
				rec := f.do(t, "POST", "/"+version+"/channels/general/messages", f.aliceTok, tt.body(f))
				if tt.code != "" {
					assertErrorBody(t, rec, tt.status, tt.code)
					msgs, err := f.srv.store.ListMessages(context.Background(), 1, 0, 10)
					if err != nil || len(msgs) != 0 {
						t.Errorf("mismatched post stored %d messages (%v)", len(msgs), err)
					}
					var denied []store.AuditEvent
					for _, e := range f.audit(t) {
						if e.Action == "access_denied" {
							denied = append(denied, e)
						}
					}
					wantSubject := "POST /" + version + "/channels/{channel}/messages"
					if len(denied) != 1 || denied[0].Actor != fmt.Sprintf("principal:%d", f.alice.ID) ||
						denied[0].Subject != wantSubject || denied[0].Detail != "author_mismatch" {
						t.Errorf("denial audit = %+v, want one event for alice on %q", denied, wantSubject)
					}
					return
				}
				if rec.Code != tt.status {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
				}
				var author int64
				if version == "v0" {
					author = decodeBody[schema.PostMessageResponse](t, rec).Message.AuthorID
				} else {
					author = decodeBody[schema.PostMessageResponseV1](t, rec).Message.AuthorID
				}
				if author != tt.author(f) {
					t.Errorf("stored author = %d, want %d", author, tt.author(f))
				}
			})
		}
	}
}

// Under AuthOff the author is still taken from the body, as before.
func TestAuthorFromBodyWhenAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	rec := f.do(t, "POST", "/v0/channels/general/messages", "", fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.bot.ID))
	if rec.Code != 201 || decodeBody[schema.PostMessageResponse](t, rec).Message.AuthorID != f.bot.ID {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, "POST", "/v0/channels/general/messages", "", `{"body":"hi"}`), 400, "invalid_request")
}

func TestWhoAmI(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name  string
		token string
		want  schema.WhoAmIResponseV1
	}{
		{"operator", f.rootTok, schema.WhoAmIResponseV1{ID: f.root.ID, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator}},
		{"member human", f.aliceTok, schema.WhoAmIResponseV1{ID: f.alice.ID, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember}},
		{"member agent", f.botTok, schema.WhoAmIResponseV1{ID: f.bot.ID, Kind: schema.PrincipalAgent, Name: "bot", Role: schema.RoleMember}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/whoami", tt.token, "")
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := decodeBody[schema.WhoAmIResponseV1](t, rec); got != tt.want {
				t.Errorf("whoami = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestOperatorOnlyRoutesDeny checks each operator-only route actually denies a
// member with the forbidden error and records the denial, and that nothing was
// changed by the denied request.
func TestOperatorOnlyRoutesDeny(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		if exp.class != classOp && exp.class != classSelf {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			before := countAction(f.audit(t), "access_denied")
			rec := f.do(t, routeMethod(rt.pattern), exp.path, f.aliceTok, exp.body)
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			var last store.AuditEvent
			for _, e := range f.audit(t) {
				if e.Action == "access_denied" {
					last = e
				}
			}
			if countAction(f.audit(t), "access_denied") != before+1 {
				t.Fatalf("access_denied events did not grow by one")
			}
			if last.Actor != fmt.Sprintf("principal:%d", f.alice.ID) || last.Subject != rt.pattern || last.Detail != "forbidden" {
				t.Errorf("audit = %+v, want actor alice, subject %q, detail forbidden", last, rt.pattern)
			}
		})
	}
	// A denied member cannot have minted anything.
	creds, err := f.srv.store.ListCredentials(context.Background(), f.root.ID)
	if err != nil || len(creds) != 1 {
		t.Errorf("operator credentials = %d (%v), want the one bootstrap credential", len(creds), err)
	}
}

func TestManifestOperatorOrSelf(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	other, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "other")
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := f.srv.store.CreateCredential(context.Background(), "system", other.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	put := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID), f.rootTok,
		`{"display_name":"Bot","tier":"A","capabilities":[],"channels":[],"rate_limits":[]}`)
	if put.Code != 201 && put.Code != 200 {
		t.Fatalf("operator PUT manifest = %d %s", put.Code, put.Body)
	}
	path := fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID)
	tests := []struct {
		name   string
		token  string
		path   string
		status int
	}{
		{"operator reads agent's", f.rootTok, path, 200},
		{"agent reads own", f.botTok, path, 200},
		{"other agent denied", otherTok, path, 403},
		{"other agent denied unknown id", otherTok, "/v1/principals/9999/manifest", 403},
		{"human member denied", f.aliceTok, path, 403},
		{"agent cannot write own", f.botTok, path, 403},
		{"unauthenticated", "", path, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := "GET"
			if strings.HasPrefix(tt.name, "agent cannot write") {
				method = "PUT"
			}
			rec := f.do(t, method, tt.path, tt.token, `{}`)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
}

func TestCreatePrincipalAlwaysMember(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, body := range []string{
		`{"kind":"human","name":"carol"}`,
	} {
		rec := f.do(t, "POST", "/v0/principals", f.rootTok, body)
		if rec.Code != 201 {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		var raw map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["principal"]["role"]; ok {
			t.Error("PrincipalV0 wire shape gained a role field")
		}
		p, err := f.srv.store.PrincipalByID(context.Background(), decodeBody[schema.CreatePrincipalResponse](t, rec).Principal.ID)
		if err != nil || p.Role != store.RoleMember {
			t.Errorf("role = %q (%v), want member", p.Role, err)
		}
	}
	// A client-supplied role is an unknown field and is rejected.
	assertErrorBody(t, f.do(t, "POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"dave","role":"operator"}`), 400, "invalid_request")
}

// Webhook ingest authenticates with its URL token only; an Authorization
// header on it is neither required nor interpreted.
func TestHookIngestExemptUnderRequired(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	rec := f.do(t, "POST", "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"general","principal":%d}`, f.bot.ID))
	if rec.Code != 201 {
		t.Fatalf("create hook = %d %s", rec.Code, rec.Body)
	}
	hook := decodeBody[schema.CreateHookResponse](t, rec).Token
	for _, bearer := range []string{"", f.aliceTok} {
		rec := f.do(t, "POST", "/v1/hooks/"+hook, bearer, `{"body":"from hook"}`)
		if rec.Code != 201 {
			t.Fatalf("ingest (bearer %v) = %d %s", bearer != "", rec.Code, rec.Body)
		}
		if got := decodeBody[schema.PostMessageResponseV1](t, rec).Message.AuthorID; got != f.bot.ID {
			t.Errorf("hook author = %d, want the hook's principal %d", got, f.bot.ID)
		}
	}
}

func TestWebSocketAuth(t *testing.T) {
	for _, version := range []string{"v0", "v1"} {
		t.Run(version, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			base := wsTestServer(t, f.srv)
			url := base + "/" + version + "/ws?channel=general"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			for _, tc := range []struct {
				name   string
				header http.Header
				url    string
			}{
				{"no credential", nil, url},
				{"bad credential", http.Header{"Authorization": {"Bearer nope"}}, url},
				{"token in query", nil, url + "&token=" + f.aliceTok},
			} {
				conn, resp, err := websocket.Dial(ctx, tc.url, &websocket.DialOptions{HTTPHeader: tc.header})
				if err == nil {
					_ = conn.CloseNow()
					t.Fatalf("%s: upgrade succeeded", tc.name)
				}
				if resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("%s: response = %v (%v), want 401", tc.name, resp, err)
				}
				if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
					t.Errorf("%s: WWW-Authenticate = %q", tc.name, got)
				}
				_ = resp.Body.Close()
			}

			// With a credential the upgrade succeeds and messages flow.
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // upgraded
				HTTPHeader: http.Header{"Authorization": {"Bearer " + f.aliceTok}},
			})
			if err != nil {
				t.Fatalf("authenticated dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			httpBase := "http" + strings.TrimPrefix(base, "ws")
			req, _ := http.NewRequestWithContext(ctx, "POST", httpBase+"/"+version+"/channels/general/messages", strings.NewReader(`{"body":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+f.aliceTok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Fatalf("post status = %d", resp.StatusCode)
			}
			var m schema.MessageV0
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				t.Fatalf("read: %v", err)
			}
			if m.Body != "hello" || m.AuthorID != f.alice.ID {
				t.Errorf("message = %+v", m)
			}
		})
	}
}

// syncBuffer collects slog output for the redaction test; slog handlers
// serialize writes, and the test reads only after requests complete.
type syncBuffer struct {
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) { return b.buf.Write(p) }

// TestNoTokenLeaks drives failures and successes and asserts no token (or any
// 20+ character piece of one) appears in logs, response bodies, response
// headers other than a create response's own body, or audit events.
func TestNoTokenLeaks(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newAuthFixture(t, AuthRequired)
	unknown := schema.CredentialTokenPrefix + strings.Repeat("Z", 43)
	secrets := []string{f.rootTok, f.aliceTok, f.botTok, unknown, "tokenlike-garbage-value"}

	var responses []string
	record := func(rec *httptest.ResponseRecorder) {
		responses = append(responses, rec.Body.String())
		for k, v := range rec.Header() {
			responses = append(responses, k+": "+strings.Join(v, ","))
		}
	}
	for _, c := range []struct{ method, path, token, body string }{
		{"GET", "/v1/whoami", unknown, ""},
		{"GET", "/v1/whoami", "tokenlike-garbage-value", ""},
		{"GET", "/v1/whoami", "", ""},
		{"POST", "/v0/channels", f.aliceTok, `{"name":"x"}`},
		{"POST", "/v1/principals/1/credentials", f.aliceTok, `{"label":"x"}`},
		{"GET", "/v1/principals/1/manifest", f.botTok, ""},
		{"POST", "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.root.ID)},
		{"GET", "/v1/whoami", f.aliceTok, ""},
		{"POST", "/v1/hooks/" + f.aliceTok, f.aliceTok, `{}`},
	} {
		record(f.do(t, c.method, c.path, c.token, c.body))
	}
	// A store failure path too.
	_ = f.srv.store.Close() // deliberately breaking the store
	record(f.do(t, "GET", "/v1/whoami", f.aliceTok, ""))

	haystacks := map[string]string{"logs": logs.buf.String(), "responses": strings.Join(responses, "\n")}
	// Audit events are covered by TestNoTokenInAuditEvents.
	for name, hay := range haystacks {
		for _, s := range secrets {
			if strings.Contains(hay, s) {
				t.Errorf("%s contain a token", name)
			}
			for i := 0; i+20 <= len(s) && len(s) > 20; i += 10 {
				if strings.Contains(hay, s[i:i+20]) {
					t.Errorf("%s contain a fragment of a token", name)
				}
			}
		}
	}
}

// TestNoTokenInAuditEvents reads the audit log after denials and failures.
func TestNoTokenInAuditEvents(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	f.do(t, "POST", "/v0/channels", f.aliceTok, `{"name":"x"}`)
	f.do(t, "POST", "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.root.ID))
	f.do(t, "GET", "/v1/whoami", "conch_"+strings.Repeat("Q", 43), "")
	var all strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&all, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	for _, s := range []string{f.rootTok, f.aliceTok, f.botTok, "QQQQQQQQQQ", hashOf(f.aliceTok)} {
		if strings.Contains(all.String(), s) {
			t.Errorf("audit log contains secret material %.8s...", s)
		}
	}
}

func hashOf(token string) string { return sha256Hex(token) }

// Requests that try to reach a protected handler through path or method
// tricks must all get the standard 401 under AuthRequired. This pins the
// middleware's exemption lookup to the mux's own matching.
func TestMuxEdgeCasesRequireAuth(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, c := range []struct{ method, target string }{
		{"GET", "//v1/whoami"},
		{"GET", "/v1//whoami"},
		{"GET", "/v1%2Fwhoami"},
		{"GET", "/healthz/../v1/whoami"},
		{"GET", "/mcp/../v1/whoami"},
		{"GET", "/v1/whoami/"},
		{"GET", "/mcp/"},
		{"GET", "/mcp/v1/whoami"},
		{"GET", "/healthz/"},
		{"HEAD", "/v1/whoami"},
		{"OPTIONS", "/v1/channels"},
		{"POST", "/healthz"},
		{"GET", "/v1/whoami?token=anything"},
		{"GET", "/v1/whoami?access_token=anything"},
	} {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			rec := f.do(t, c.method, c.target, "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}
}

// Running without authentication is a deliberate, loud choice: the server
// says so once at startup, and says nothing of the kind when it is required.
func TestAuthOffWarnsAtStartup(t *testing.T) {
	for _, tt := range []struct {
		mode AuthMode
		want int
	}{{AuthOff, 1}, {"", 1}, {AuthRequired, 0}} {
		logs := captureLogs(t)
		newTestServerWithConfig(t, Config{AuthMode: tt.mode})
		if got := strings.Count(logs.buf.String(), "authentication is OFF"); got != tt.want {
			t.Errorf("mode %q: warnings logged = %d, want %d", tt.mode, got, tt.want)
		}
	}
}
