package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// selfMadeStandIn is a route that is in selfMadeCredentialRoutes only for
// these tests: the DM and call routes and the per-person stream (#2019,
// #2021, #2030, #2040) do not exist yet, and the guard must be shown to
// work before they do. Each real one, once listed, is driven by the same
// table.
const selfMadeStandIn = "GET /v1/test-only/self-made"

// provenanceFixture is the auth fixture with joiner, a member who joined by
// an invite (a self-made credential, joinerTok), and a token of every other
// kind for the same member.
type provenanceFixture struct {
	*authFixture
	joiner               store.Principal
	joinerTok            string // self-made: the person's own invite redemption
	actAsTok             string // an operator's credential for them (act_as_members)
	linkTok              string // the redemption of a sign-in link an operator made
	migratedTok          string // self-made once, made by another since the migration
	migratedCredentialID int64
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	ctx := context.Background()
	f := &provenanceFixture{authFixture: newAuthFixture(t, AuthRequired)}
	now := time.Now()
	join := func(name string) (store.Principal, string, int64) {
		inv, err := f.srv.store.CreateInvite(ctx, "principal:"+strconv.FormatInt(f.root.ID, 10), f.root.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}
		r, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(inv.Code), name, now)
		if err != nil {
			t.Fatalf("RedeemInvite: %v", err)
		}
		return r.Principal, r.Token, r.Credential.ID
	}
	f.joiner, f.joinerTok, _ = join("joiner")
	_, tok, err := f.srv.store.CreateCredential(ctx, "principal:"+strconv.FormatInt(f.root.ID, 10), f.joiner.ID, "act as", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.actAsTok = tok
	link, err := f.srv.store.CreateSignInLink(ctx, "principal:"+strconv.FormatInt(f.root.ID, 10), f.root.ID, f.joiner.ID, schema.CreateSignInLinkRequestV1{}, now)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := f.srv.store.RedeemSignInLink(ctx, store.HashSignInLinkCode(link.Code), now)
	if err != nil {
		t.Fatal(err)
	}
	f.linkTok = redeemed.Token
	// A credential from before the provenance migration is a row the
	// migration left at 0: the same as a self-made one with its column
	// cleared, which is what is done here, under the store.
	_, f.migratedTok, f.migratedCredentialID = join("veteran")
	path, _ := testDBPaths.Load(f.srv)
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE credentials SET self_made = 0 WHERE id = ?", f.migratedCredentialID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

// guardedStandIn is the stand-in route as guard registers a listed route,
// behind the authentication middleware: 200 "ok" when it runs.
func (f *provenanceFixture) guardedStandIn(t *testing.T) http.Handler {
	t.Helper()
	selfMadeCredentialRoutes[selfMadeStandIn] = true
	t.Cleanup(func() { delete(selfMadeCredentialRoutes, selfMadeStandIn) })
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux := http.NewServeMux()
	mux.Handle(selfMadeStandIn, f.srv.guard(route{pattern: selfMadeStandIn, access: accessAuthenticated, handler: ok}))
	return f.srv.authMiddleware(mux)
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// requestFor is a request to pattern's route, every path parameter 1, with
// token as its bearer.
func requestFor(pattern, token string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	req := httptest.NewRequest(method, pathParam.ReplaceAllString(path, "1"), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func answerCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e schema.Error
	// 426 is a socket route asked for a plain GET by a credential that
	// passed: its answer is text, and it is not a refusal.
	if rec.Code < 400 || rec.Code == http.StatusUpgradeRequired {
		return ""
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("answer %d %q is not an error: %v", rec.Code, rec.Body.String(), err)
	}
	return e.Code
}

// Every route that is a person's own (their DMs, their calls, their stream:
// selfMadeCredentialRoutes, with the stand-in until they exist) takes a
// self-made credential and refuses, with 403 own_sign_in_required, an
// operator's act-as credential, the redemption of a sign-in link an
// operator made, and a credential from before the provenance migration
// (issue #2042). A self-made credential is never refused on that ground;
// what the route then answers is the route's own. An agent is refused first,
// with 403 forbidden audited agents_use_mcp, never own_sign_in_required
// (issue #2098); a guest is a person and gets the person's answers.
func TestSelfMadeCredentialRoutesRefuseOthers(t *testing.T) {
	f := newProvenanceFixture(t)
	guest := f.seedGuest(t, "Pat")
	standIn := f.guardedStandIn(t)
	patterns := []string{selfMadeStandIn}
	for p := range selfMadeCredentialRoutes {
		if p != selfMadeStandIn {
			patterns = append(patterns, p)
		}
	}
	for _, pattern := range patterns {
		serve := f.srv.Handler().ServeHTTP
		if pattern == selfMadeStandIn {
			serve = standIn.ServeHTTP
		}
		for _, tt := range []struct {
			name  string
			token string
			self  bool
			agent bool // refused first, 403 forbidden
		}{
			{"own sign-in", f.joinerTok, true, false},
			{"owner's own bootstrap", f.rootTok, true, false},
			{"act-as credential", f.actAsTok, false, false},
			{"someone else's sign-in link", f.linkTok, false, false},
			{"migrated credential", f.migratedTok, false, false},
			{"agent (credential made by another)", f.botTok, false, true},
			{"guest's own visit", guest.Token, true, false},
		} {
			t.Run(pattern+"/"+tt.name, func(t *testing.T) {
				var auditBefore int64
				if tt.agent {
					var err error
					auditBefore, err = f.srv.store.LastAuditID(t.Context())
					if err != nil {
						t.Fatal(err)
					}
				}
				rec := httptest.NewRecorder()
				serve(rec, requestFor(pattern, tt.token))
				code := answerCode(t, rec)
				if tt.agent {
					if rec.Code != http.StatusForbidden || code != errForbidden.Code {
						t.Fatalf("agent got %d %s (%s); want 403 forbidden", rec.Code, code, rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), "sign in") {
						t.Errorf("the agent was told to sign in: %s", rec.Body.String())
					}
					f.wantAgentDenial(t, auditBefore, f.bot.ID, pattern, denyAgentOnHumanRoute)
					return
				}
				refused := rec.Code == http.StatusForbidden && code == schema.ErrorCodeOwnSignInRequired
				if refused == tt.self {
					t.Fatalf("answer %d %s (%s); want refused %v", rec.Code, code, rec.Body.String(), !tt.self)
				}
				if pattern == selfMadeStandIn && tt.self && (rec.Code != http.StatusOK || rec.Body.String() != "ok") {
					t.Errorf("the stand-in answered %d %q to a self-made credential", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// The routes that must be in selfMadeCredentialRoutes, each added by the
// issue that registers it (#2047's comment: "each adds its routes here in
// the same change"). A route dropped from the list silently opens a person's
// DMs to an operator's credential, so each is named here and
// TestSelfMadeCredentialRoutesRefuseOthers drives it with every token kind.
func TestSelfMadeCredentialRoutesIncludeRequired(t *testing.T) {
	for _, pattern := range []string{
		"GET /v1/events/person/ws",         // the per-person stream (#2040)
		"POST /v1/dms",                     // open or resume (#2019)
		"GET /v1/dms",                      // the inbox
		"GET /v1/dms/{conversation}",       // one conversation
		"PUT /v1/dms/{conversation}/state", // per-viewer archive, hide, reopen
		// The private call routes (#2030).
		"POST /v1/dms/{conversation}/calls",
		"GET /v1/dms/{conversation}/calls",
		"GET /v1/calls/current",
		"POST /v1/calls/{call}/accept",
		"POST /v1/calls/{call}/decline",
		"POST /v1/calls/{call}/cancel",
		"POST /v1/calls/{call}/hangup",
	} {
		if !selfMadeCredentialRoutes[pattern] {
			t.Errorf("%s is not in selfMadeCredentialRoutes", pattern)
		}
	}
}

// Routes outside the list are unchanged: every one of the tokens the list
// refuses still reaches an ordinary route as before.
func TestSelfMadeCredentialOtherRoutesUnchanged(t *testing.T) {
	f := newProvenanceFixture(t)
	for _, tok := range []string{f.joinerTok, f.actAsTok, f.linkTok, f.migratedTok} {
		for _, pattern := range []string{"GET /v1/whoami", "GET /v1/channels"} {
			rec := f.do(t, http.MethodGet, strings.TrimPrefix(pattern, "GET "), tok, "")
			if rec.Code != http.StatusOK {
				t.Errorf("%s answered %d %s", pattern, rec.Code, rec.Body.String())
			}
		}
	}
}

// selfMadeCredential on its own: a ticket-authenticated socket carries only
// the credential's id, and its provenance is read from the store; a
// previewed request is refused whatever the credential; a request with no
// credential, or an id the store does not know, is unauthenticated; under
// AuthOff there are no credentials and nothing is refused.
func TestSelfMadeCredentialCheck(t *testing.T) {
	f := newProvenanceFixture(t)
	ctx := context.Background()
	idOf := func(token string) int64 {
		_, cred, err := f.srv.store.ResolveRequestCredential(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		return cred.ID
	}
	ticket := func(token string) context.Context {
		return withCredentialID(withCaller(ctx, f.joiner), idOf(token))
	}
	for _, tt := range []struct {
		name   string
		srv    *Server
		ctx    context.Context
		status int
		code   string
	}{
		{"ticket for a self-made credential", f.srv, ticket(f.joinerTok), http.StatusOK, ""},
		{"ticket for an act-as credential", f.srv, ticket(f.actAsTok), http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"ticket for someone else's sign-in link", f.srv, ticket(f.linkTok), http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"ticket for a migrated credential", f.srv, ticket(f.migratedTok), http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"credential the store does not know", f.srv, withCredentialID(withCaller(ctx, f.joiner), 1<<40), http.StatusUnauthorized, ""},
		{"previewed, on a self-made credential", f.srv,
			withPreview(withRequestCredential(withCaller(ctx, f.joiner), store.RequestCredential{ID: idOf(f.rootTok), SelfMade: true}),
				previewContext{Actor: f.root, Target: f.joiner}),
			http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"agent, on a ticket", f.srv, withCredentialID(withCaller(ctx, f.bot), idOf(f.botTok)), http.StatusForbidden, errForbidden.Code},
		{"agent, no credential", f.srv, withCaller(ctx, f.bot), http.StatusForbidden, errForbidden.Code},
		{"caller and no credential", f.srv, withCaller(ctx, f.joiner), http.StatusUnauthorized, ""},
		{"no caller", f.srv, ctx, http.StatusUnauthorized, ""},
		{"authentication off", newTestServer(t), ctx, http.StatusOK, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ran := false
			h := tt.srv.requireSelfMadeCredential(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				ran = true
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tt.ctx))
			if rec.Code != tt.status || ran != (tt.status == http.StatusOK) {
				t.Fatalf("answer %d (ran %v), want %d", rec.Code, ran, tt.status)
			}
			if tt.code != "" {
				if got := answerCode(t, rec); got != tt.code {
					t.Errorf("code %q, want %q", got, tt.code)
				}
			}
		})
	}
}

// Every route in selfMadeCredentialRoutes is registered, authenticated
// (an exempt route has no credential to judge), not previewable (a preview
// reads as the member, which these routes exist to prevent) and out of the
// setup scope. Every registered DM and call route is covered unless exempted.
func TestSelfMadeCredentialRoutes(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired})
	// Future operator switch routes may be exempted by their full pattern,
	// with a comment explaining the operator permission that gates each one.
	dmAndCallCredentialExemptions := map[string]bool{}
	for pattern := range srv.routeByPattern {
		path := pattern
		if _, rest, ok := strings.Cut(pattern, " "); ok {
			path = rest
		}
		if (strings.HasPrefix(path, "/v1/dms") || strings.HasPrefix(path, "/v1/calls")) &&
			!dmAndCallCredentialExemptions[pattern] && !selfMadeCredentialRoutes[pattern] {
			t.Errorf("registered DM or call route %s is missing from selfMadeCredentialRoutes", pattern)
		}
	}
	for pattern := range selfMadeCredentialRoutes {
		rt, ok := srv.routeByPattern[pattern]
		switch {
		case !ok:
			t.Errorf("%s is listed but not registered", pattern)
		case rt.access == accessExempt:
			t.Errorf("%s is exempt", pattern)
		case rt.previewable:
			t.Errorf("%s is previewable", pattern)
		case setupScopeRoutes[pattern]:
			t.Errorf("%s is in the setup scope", pattern)
		}
	}
}

// wantAgentDenial checks the request wrote exactly one access_denied row,
// naming the agent, route and reason.
func (f *provenanceFixture) wantAgentDenial(t *testing.T, afterID, agentID int64, subject, reason string) {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(t.Context(), afterID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	denials := slices.DeleteFunc(events, func(e store.AuditEvent) bool { return e.Action != "access_denied" })
	if len(denials) != 1 {
		t.Fatalf("got %d new access_denied rows for %s, want 1: %+v", len(denials), subject, denials)
	}
	actor := "principal:" + strconv.FormatInt(agentID, 10)
	e := denials[0]
	if e.Actor != actor || e.Subject != subject || !slices.Contains(strings.Fields(e.Detail), "reason="+reason) {
		t.Errorf("access_denied actor=%q subject=%q detail=%q, want actor=%q subject=%q reason=%q", e.Actor, e.Subject, e.Detail, actor, subject, reason)
	}
}
