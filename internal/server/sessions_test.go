package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Sessions, end to end through the handlers (issue #265). Every SessionV1 and
// SessionRosterV1 a handler sends is decoded and run through its Validate
// here, so a nil list or a wrong state fails the test that produced it.

// sessionFixture is the auth fixture (root the operator, alice and bob humans,
// bot an agent allowed everything, all in "general") with a hand-moved clock
// and the people the session tests need besides: carol, who is in no channel;
// mute, an agent that may read and post but has no sessions.manage; and a
// second credential for alice.
type sessionFixture struct {
	*authFixture
	clock     *manualClock
	general   store.Channel
	bob       store.Principal
	bobTok    string
	carol     store.Principal
	carolTok  string
	mute      store.Principal
	muteTok   string
	aliceTok2 string
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	ctx := context.Background()
	f := &sessionFixture{authFixture: newAuthFixture(t, AuthRequired), clock: newManualClock()}
	f.srv.now = f.clock.Now
	var err error
	if f.general, err = f.srv.store.ChannelByName(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	mk := func(kind store.PrincipalKind, name string, member bool) (store.Principal, string) {
		p, err := f.srv.store.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatalf("CreatePrincipal %s: %v", name, err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatalf("CreateCredential %s: %v", name, err)
		}
		if member {
			if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		return p, tok
	}
	f.bob, f.bobTok = mk(store.PrincipalHuman, "bob", true)
	f.carol, f.carolTok = mk(store.PrincipalHuman, "carol", false)
	f.mute, f.muteTok = mk(store.PrincipalAgent, "mute", true)
	setAgentManifest(t, f.srv, f.mute.ID, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}, f.general.ID)
	_, f.aliceTok2, err = f.srv.store.CreateCredential(ctx, "system", f.alice.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// credentialID is the id of the credential token belongs to.
func (f *sessionFixture) credentialID(t *testing.T, token string) int64 {
	t.Helper()
	_, id, err := f.srv.store.ResolveCredentialDetail(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// validSession decodes rec as a SessionV1 and fails unless it is valid.
func validSession(t *testing.T, rec *httptest.ResponseRecorder) schema.SessionV1 {
	t.Helper()
	doc := decodeBody[schema.SessionV1](t, rec)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the server sent an invalid session: %v\n%s", err, rec.Body)
	}
	return doc
}

// validRoster is validSession for a roster.
func validRoster(t *testing.T, rec *httptest.ResponseRecorder) schema.SessionRosterV1 {
	t.Helper()
	doc := decodeBody[schema.SessionRosterV1](t, rec)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the server sent an invalid roster: %v\n%s", err, rec.Body)
	}
	return doc
}

// create makes a session with token and returns it.
func (f *sessionFixture) create(t *testing.T, token, body string) schema.SessionV1 {
	t.Helper()
	rec := f.do(t, "POST", "/v1/sessions", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", rec.Code, rec.Body)
	}
	return validSession(t, rec)
}

const (
	humanBody = `{"client":"conch-tui"}`
	agentBody = `{"client":"adapter","runtime":"claude-code","model":"opus","host":"build-7","interventions":["pause","cancel"]}`
)

func (f *sessionFixture) roster(t *testing.T, token, channel string) schema.SessionRosterV1 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/sessions?channel="+channel, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions = %d %s", rec.Code, rec.Body)
	}
	return validRoster(t, rec)
}

func sessionPath(id int64, suffix string) string {
	return fmt.Sprintf("/v1/sessions/%d%s", id, suffix)
}

func (f *sessionFixture) auditRows(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestSessionCreate(t *testing.T) {
	tests := []struct {
		name     string
		token    func(f *sessionFixture) string
		body     string
		want     int
		wantCode string
		check    func(t *testing.T, f *sessionFixture, doc schema.SessionV1)
	}{
		{"a human", func(f *sessionFixture) string { return f.aliceTok }, humanBody, 201, "",
			func(t *testing.T, f *sessionFixture, doc schema.SessionV1) {
				if doc.PrincipalID != f.alice.ID || doc.Kind != schema.PrincipalHuman || doc.Name != "alice" || doc.Client != "conch-tui" {
					t.Errorf("session = %+v", doc)
				}
				if doc.State != schema.SessionStateOnline || doc.Status != schema.SessionStatusOnline {
					t.Errorf("state/status = %s/%s, want online/online", doc.State, doc.Status)
				}
				if doc.EndedAt != nil || doc.CurrentRun != 0 || doc.Task != "" || doc.OutcomeUnknown || len(doc.Interventions) != 0 {
					t.Errorf("a new session carries run or end data: %+v", doc)
				}
				if !doc.StartedAt.Time().Equal(f.clock.Now()) || !doc.LastSeenAt.Time().Equal(f.clock.Now()) {
					t.Errorf("started/last seen = %v/%v, want the server clock %v", doc.StartedAt.Time(), doc.LastSeenAt.Time(), f.clock.Now())
				}
			}},
		{"an operator", func(f *sessionFixture) string { return f.rootTok }, humanBody, 201, "", nil},
		{"an agent", func(f *sessionFixture) string { return f.botTok }, agentBody, 201, "",
			func(t *testing.T, f *sessionFixture, doc schema.SessionV1) {
				if doc.Kind != schema.PrincipalAgent || doc.Status != schema.SessionStatusIdle {
					t.Errorf("kind/status = %s/%s, want agent/idle", doc.Kind, doc.Status)
				}
				// The owner is told everything it reported.
				if doc.Runtime != "claude-code" || doc.Model != "opus" || doc.Host != "build-7" ||
					len(doc.Interventions) != 2 || doc.Interventions[0] != schema.InterventionKindPause {
					t.Errorf("agent session = %+v", doc)
				}
			}},
		{"an agent that declares nothing", func(f *sessionFixture) string { return f.botTok }, `{"client":"adapter"}`, 201, "",
			func(t *testing.T, _ *sessionFixture, doc schema.SessionV1) {
				if doc.Interventions == nil {
					t.Error("interventions is nil, want an empty list")
				}
			}},
		{"a human with a runtime", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","runtime":"r"}`, 400, "invalid_request", nil},
		{"a human with a model", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","model":"m"}`, 400, "invalid_request", nil},
		{"a human with a host", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","host":"h"}`, 400, "invalid_request", nil},
		{"a human with interventions", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","interventions":["pause"]}`, 400, "invalid_request", nil},
		{"a human with an empty interventions list", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","interventions":[]}`, 201, "", nil},
		{"no client", func(f *sessionFixture) string { return f.aliceTok }, `{}`, 400, "invalid_request", nil},
		{"a client with a control character", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"a\u001b[2Jb"}`, 400, "invalid_request", nil},
		{"an unknown intervention", func(f *sessionFixture) string { return f.botTok }, `{"client":"x","interventions":["explode"]}`, 400, "invalid_request", nil},
		{"an unknown field", func(f *sessionFixture) string { return f.aliceTok }, `{"client":"x","state":"offline"}`, 400, "invalid_request", nil},
		{"not json", func(f *sessionFixture) string { return f.aliceTok }, `client`, 400, "invalid_request", nil},
		{"an agent without sessions.manage", func(f *sessionFixture) string { return f.muteTok }, `{"client":"x"}`, 403, "forbidden", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSessionFixture(t)
			rec := f.do(t, "POST", "/v1/sessions", tt.token(f), tt.body)
			if tt.wantCode != "" {
				assertErrorBody(t, rec, tt.want, tt.wantCode)
				if n := len(f.auditRows(t, store.AuditSessionStarted)); n != 0 {
					t.Errorf("a refused create wrote %d session_started rows", n)
				}
				return
			}
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			doc := validSession(t, rec)
			if tt.check != nil {
				tt.check(t, f, doc)
			}
		})
	}
}

// A session is audited when it starts, once, with the caller as actor.
func TestSessionStartedAudit(t *testing.T) {
	f := newSessionFixture(t)
	doc := f.create(t, f.aliceTok, humanBody)
	rows := f.auditRows(t, store.AuditSessionStarted)
	if len(rows) != 1 {
		t.Fatalf("session_started rows = %d, want 1", len(rows))
	}
	if rows[0].Actor != fmt.Sprintf("principal:%d", f.alice.ID) || rows[0].Subject != fmt.Sprintf("session:%d", doc.ID) {
		t.Errorf("row = %+v", rows[0])
	}
	if strings.Contains(rows[0].Detail, f.aliceTok) {
		t.Error("the audit row carries the token")
	}
}

// The ownership rule: a session belongs to the credential that created it.
// Another credential of the same principal, an operator, another person and an
// agent get the byte-identical answer a missing id gets, for heartbeat and
// for delete.
func TestSessionOwnership(t *testing.T) {
	f := newSessionFixture(t)
	mine := f.create(t, f.aliceTok, humanBody)
	ended := f.create(t, f.aliceTok, humanBody)
	if rec := f.do(t, "DELETE", sessionPath(ended.ID, ""), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end = %d %s", rec.Code, rec.Body)
	}

	type caller struct{ name, token string }
	others := []caller{
		{"another credential of the same principal", f.aliceTok2},
		{"an operator", f.rootTok},
		{"another person", f.bobTok},
		{"an agent", f.botTok},
	}
	for _, method := range []struct{ name, verb, suffix string }{
		{"heartbeat", "POST", "/heartbeat"},
		{"delete", "DELETE", ""},
	} {
		// What a missing id looks like, from the same caller.
		missing := func(token string) string {
			rec := f.do(t, method.verb, sessionPath(999999, method.suffix), token, "")
			assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeSessionNotFound)
			return rec.Body.String()
		}
		for _, c := range others {
			t.Run(method.name+" by "+c.name, func(t *testing.T) {
				rec := f.do(t, method.verb, sessionPath(mine.ID, method.suffix), c.token, "")
				assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeSessionNotFound)
				if got, want := rec.Body.String(), missing(c.token); got != want {
					t.Errorf("body differs from a missing id's:\n got %s\nwant %s", got, want)
				}
			})
		}
		t.Run(method.name+" on an ended session", func(t *testing.T) {
			rec := f.do(t, method.verb, sessionPath(ended.ID, method.suffix), f.aliceTok, "")
			assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeSessionNotFound)
			if got, want := rec.Body.String(), missing(f.aliceTok); got != want {
				t.Errorf("body differs from a missing id's:\n got %s\nwant %s", got, want)
			}
		})
		for _, id := range []string{"0", "-1", "abc", "1e3", "99999999999999999999"} {
			t.Run(method.name+" with the id "+id, func(t *testing.T) {
				rec := f.do(t, method.verb, "/v1/sessions/"+id+method.suffix, f.aliceTok, "")
				assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeSessionNotFound)
				if got, want := rec.Body.String(), missing(f.aliceTok); got != want {
					t.Errorf("body differs from a missing id's:\n got %s\nwant %s", got, want)
				}
			})
		}
	}
	// None of that touched the session.
	if rec := f.do(t, "POST", sessionPath(mine.ID, "/heartbeat"), f.aliceTok, ""); rec.Code != http.StatusOK {
		t.Errorf("the owner's heartbeat after all that = %d %s", rec.Code, rec.Body)
	}
	if n := len(f.auditRows(t, store.AuditSessionEnded)); n != 1 {
		t.Errorf("session_ended rows = %d, want only the one the owner caused", n)
	}
}

func TestSessionHeartbeatAndEnd(t *testing.T) {
	f := newSessionFixture(t)
	sess := f.create(t, f.aliceTok, humanBody)

	f.clock.Advance(20 * time.Second)
	rec := f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.aliceTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	beat := validSession(t, rec)
	if beat.ID != sess.ID || !beat.LastSeenAt.Time().Equal(f.clock.Now()) || !beat.StartedAt.Time().Equal(sess.StartedAt.Time()) {
		t.Errorf("after a heartbeat: %+v (created %+v)", beat, sess)
	}
	if n := len(f.auditRows(t, store.AuditSessionStarted)) + len(f.auditRows(t, store.AuditSessionEnded)); n != 1 {
		t.Errorf("a heartbeat was audited: %d session rows, want only session_started", n)
	}

	rec = f.do(t, "DELETE", sessionPath(sess.ID, ""), f.aliceTok, "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q, want 204 and no body", rec.Code, rec.Body)
	}
	ends := f.auditRows(t, store.AuditSessionEnded)
	if len(ends) != 1 || !strings.Contains(ends[0].Detail, "cause=client") || ends[0].Subject != fmt.Sprintf("session:%d", sess.ID) {
		t.Errorf("session_ended rows = %+v", ends)
	}
	// Ended is final: a heartbeat does not revive it, and it is not listed.
	assertErrorBody(t, f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.aliceTok, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)
	assertErrorBody(t, f.do(t, "DELETE", sessionPath(sess.ID, ""), f.aliceTok, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)
	if got := f.roster(t, f.aliceTok, "general").Sessions; len(got) != 0 {
		t.Errorf("an ended session is listed: %+v", got)
	}
	if n := len(f.auditRows(t, store.AuditSessionEnded)); n != 1 {
		t.Errorf("session_ended rows = %d after the repeats, want 1", n)
	}
}

// An agent whose manifest lacks sessions.manage is refused on create,
// heartbeat and delete with the standard capability refusal, and each refusal
// is audited as other manifest denials are. A session that was made while it
// had the capability cannot be kept alive or ended once it is gone.
func TestSessionAgentNeedsCapability(t *testing.T) {
	f := newSessionFixture(t)
	sess := f.create(t, f.botTok, agentBody)
	// Revoke the capability: everything but sessions.manage.
	var caps []schema.Capability
	for _, c := range schema.Capabilities() {
		if c != schema.CapabilitySessionsManage {
			caps = append(caps, c)
		}
	}
	setAgentManifest(t, f.srv, f.bot.ID, caps, f.general.ID)

	for _, c := range []struct{ name, method, path, body, pattern string }{
		{"create", "POST", "/v1/sessions", `{"client":"x"}`, "POST /v1/sessions"},
		{"heartbeat", "POST", sessionPath(sess.ID, "/heartbeat"), "", "POST /v1/sessions/{id}/heartbeat"},
		{"delete", "DELETE", sessionPath(sess.ID, ""), "", "DELETE /v1/sessions/{id}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(f.auditRows(t, "access_denied"))
			rec := f.do(t, c.method, c.path, f.botTok, c.body)
			assertErrorBody(t, rec, http.StatusForbidden, errForbidden.Code)
			rows := f.auditRows(t, "access_denied")
			if len(rows) != before+1 {
				t.Fatalf("access_denied rows = %d, want %d", len(rows), before+1)
			}
			last := rows[len(rows)-1]
			if last.Subject != c.pattern || !strings.Contains(last.Detail, "capability=sessions.manage") || !strings.Contains(last.Detail, "reason="+denyCapability) {
				t.Errorf("denial row = %+v", last)
			}
		})
	}
	// The refusals changed nothing: with the capability back, the session lives.
	setAgentManifest(t, f.srv, f.bot.ID, nil, f.general.ID)
	if rec := f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.botTok, ""); rec.Code != http.StatusOK {
		t.Errorf("heartbeat after the capability came back = %d %s", rec.Code, rec.Body)
	}

	// No manifest at all is a refusal too.
	nomanifest, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "nomanifest")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", nomanifest.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/sessions", tok, `{"client":"x"}`), http.StatusForbidden, errForbidden.Code)
}

// The own-session routes spend the capability's budget like every other
// agent call (issue #80), and a refused call spends none.
func TestSessionAgentRateLimit(t *testing.T) {
	f := newSessionFixture(t)
	req := schema.PutAgentManifestRequestV1{
		DisplayName: "bot", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: f.general.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
		RateLimits: []schema.RateLimit{rl(schema.CapabilitySessionsManage, 2, 60)},
	}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.bot.ID, req); err != nil {
		t.Fatal(err)
	}
	sess := f.create(t, f.botTok, agentBody)                                                                 // use 1
	if rec := f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.botTok, ""); rec.Code != http.StatusOK { // use 2
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.botTok, "")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the 429")
	}
	if n := len(f.auditRows(t, store.AuditAgentRateLimited)); n != 1 {
		t.Errorf("agent_rate_limited rows = %d, want 1", n)
	}
	f.clock.Advance(61 * time.Second)
	if rec := f.do(t, "DELETE", sessionPath(sess.ID, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete after the window = %d %s", rec.Code, rec.Body)
	}
}

// At most 16 sessions that have not ended per credential. The bound is the
// credential's: its principal's other credentials have their own.
func TestSessionLimit(t *testing.T) {
	f := newSessionFixture(t)
	var first schema.SessionV1
	for i := range store.MaxLiveSessionsPerCredential {
		s := f.create(t, f.aliceTok, humanBody)
		if i == 0 {
			first = s
		}
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/sessions", f.aliceTok, humanBody), http.StatusTooManyRequests, schema.ErrorCodeTooManySessions)
	if n := len(f.auditRows(t, store.AuditSessionStarted)); n != store.MaxLiveSessionsPerCredential {
		t.Errorf("session_started rows = %d, want %d", n, store.MaxLiveSessionsPerCredential)
	}
	// Another credential of the same principal, and another principal, are not affected.
	f.create(t, f.aliceTok2, humanBody)
	f.create(t, f.bobTok, humanBody)
	// Ending one makes room for one.
	if rec := f.do(t, "DELETE", sessionPath(first.ID, ""), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	f.create(t, f.aliceTok, humanBody)
	assertErrorBody(t, f.do(t, "POST", "/v1/sessions", f.aliceTok, humanBody), http.StatusTooManyRequests, schema.ErrorCodeTooManySessions)
}

// Under --auth off there is no caller to bind a session to: every route
// answers sessions_require_auth, whatever else is wrong with the request.
func TestSessionsRequireAuth(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/sessions", humanBody},
		{"POST", "/v1/sessions", `{}`},
		{"POST", "/v1/sessions/1/heartbeat", ""},
		{"POST", "/v1/sessions/abc/heartbeat", ""},
		{"DELETE", "/v1/sessions/1", ""},
		{"GET", "/v1/sessions?channel=general", ""},
		{"GET", "/v1/sessions", ""},
		{"GET", "/v1/sessions?channel=nosuch", ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			assertErrorBody(t, f.do(t, c.method, c.path, "", c.body), http.StatusBadRequest, schema.ErrorCodeSessionsRequireAuth)
		})
	}
	if n := len(f.audit(t)); n != 8 {
		t.Errorf("audit rows = %d, want the 8 the fixture wrote", n)
	}
}

func TestSessionList(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	// "private" holds only alice; root, the operator, is not in it.
	private, err := f.srv.store.CreateChannel(ctx, "private")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", private.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}

	t.Run("an empty roster is a list", func(t *testing.T) {
		rec := f.do(t, "GET", "/v1/sessions?channel=general", f.aliceTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
			t.Errorf("body = %s, want sessions as []", rec.Body)
		}
		if r := validRoster(t, rec); r.ChannelID != f.general.ID {
			t.Errorf("channel_id = %d, want %d", r.ChannelID, f.general.ID)
		}
	})

	a1 := f.create(t, f.aliceTok, humanBody)
	b1 := f.create(t, f.botTok, agentBody)
	c1 := f.create(t, f.carolTok, humanBody) // carol is in no channel
	a2 := f.create(t, f.aliceTok2, `{"client":"web"}`)

	t.Run("members see the sessions of members, by id", func(t *testing.T) {
		for _, tok := range []string{f.aliceTok, f.bobTok, f.rootTok, f.botTok} {
			r := f.roster(t, tok, "general")
			var ids []int64
			for _, s := range r.Sessions {
				ids = append(ids, s.ID)
			}
			want := []int64{a1.ID, b1.ID, a2.ID}
			if fmt.Sprint(ids) != fmt.Sprint(want) {
				t.Errorf("session ids = %v, want %v (carol's %d must not be listed)", ids, want, c1.ID)
			}
			for _, s := range r.Sessions {
				if s.Name == "" || s.Kind == "" {
					t.Errorf("name/kind are not filled from the principal: %+v", s)
				}
			}
		}
	})

	t.Run("the roster is the channel's current members", func(t *testing.T) {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, f.carol.ID, 0); err != nil {
			t.Fatal(err)
		}
		if got := len(f.roster(t, f.aliceTok, "general").Sessions); got != 4 {
			t.Errorf("after carol joined: %d sessions, want 4", got)
		}
		if _, err := f.srv.store.RemoveChannelMember(ctx, "system", f.general.ID, f.carol.ID); err != nil {
			t.Fatal(err)
		}
		if got := len(f.roster(t, f.aliceTok, "general").Sessions); got != 3 {
			t.Errorf("after carol left: %d sessions, want 3", got)
		}
	})

	t.Run("another channel lists only its own members' sessions", func(t *testing.T) {
		r := f.roster(t, f.aliceTok, "private")
		if len(r.Sessions) != 2 || r.Sessions[0].ID != a1.ID || r.Sessions[1].ID != a2.ID || r.ChannelID != private.ID {
			t.Errorf("private roster = %+v", r)
		}
	})

	// A non-member, an operator who is not a member, and a channel that does
	// not exist all get the one answer.
	unknown := f.do(t, "GET", "/v1/sessions?channel=nosuch", f.aliceTok, "")
	assertErrorBody(t, unknown, http.StatusNotFound, "channel_not_found")
	for _, c := range []struct{ name, token, channel string }{
		{"a non-member", f.carolTok, "general"},
		{"an operator who is not a member", f.rootTok, "private"},
		{"a member of another channel", f.bobTok, "private"},
		{"an agent that is not a member", f.botTok, "private"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/sessions?channel="+c.channel, c.token, "")
			assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
			if rec.Body.String() != unknown.Body.String() {
				t.Errorf("body differs from an unknown channel's:\n got %s\nwant %s", rec.Body, unknown.Body)
			}
		})
	}

	t.Run("a channel is required", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "GET", "/v1/sessions", f.aliceTok, ""), http.StatusBadRequest, "invalid_request")
		assertErrorBody(t, f.do(t, "GET", "/v1/sessions?channel=", f.aliceTok, ""), http.StatusBadRequest, "invalid_request")
	})

	t.Run("an agent needs messages.read with read on the channel", func(t *testing.T) {
		// mute may read and post: it can list. Without the capability it is a
		// member the manifest refuses.
		if got := f.roster(t, f.muteTok, "general"); len(got.Sessions) != 3 {
			t.Errorf("mute lists %d sessions, want 3", len(got.Sessions))
		}
		setAgentManifest(t, f.srv, f.mute.ID, []schema.Capability{schema.CapabilityMessagesPost}, f.general.ID)
		assertErrorBody(t, f.do(t, "GET", "/v1/sessions?channel=general", f.muteTok, ""), http.StatusForbidden, errForbidden.Code)
		// Read on another channel only does not do either.
		setAgentManifest(t, f.srv, f.mute.ID, nil, private.ID)
		assertErrorBody(t, f.do(t, "GET", "/v1/sessions?channel=general", f.muteTok, ""), http.StatusForbidden, errForbidden.Code)
	})

	t.Run("a non-member agent is audited as one", func(t *testing.T) {
		before := len(f.auditRows(t, "access_denied"))
		assertErrorBody(t, f.do(t, "GET", "/v1/sessions?channel=private", f.botTok, ""), http.StatusNotFound, "channel_not_found")
		rows := f.auditRows(t, "access_denied")
		if len(rows) != before+1 || !strings.Contains(rows[len(rows)-1].Detail, "reason="+denyNotMember) {
			t.Errorf("rows = %+v", rows[before:])
		}
	})
}

// The roster shows liveness as it is, hides what has ended, and drops what has
// been silent for more than a day.
func TestSessionListLiveness(t *testing.T) {
	f := newSessionFixture(t)
	live := f.create(t, f.aliceTok, humanBody)
	stale := f.create(t, f.aliceTok2, humanBody)
	ended := f.create(t, f.bobTok, humanBody)
	if rec := f.do(t, "DELETE", sessionPath(ended.ID, ""), f.bobTok, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}

	f.clock.Advance(40 * time.Second)
	f.heartbeat(t, f.aliceTok, live.ID)
	f.srv.sweepSessions(context.Background())
	r := f.roster(t, f.rootTok, "general")
	if len(r.Sessions) != 2 {
		t.Fatalf("roster = %+v, want the live and the stale session", r.Sessions)
	}
	byID := map[int64]schema.SessionV1{}
	for _, s := range r.Sessions {
		byID[s.ID] = s
	}
	if s := byID[live.ID]; s.State != schema.SessionStateOnline || s.Status != schema.SessionStatusOnline {
		t.Errorf("live = %s/%s", s.State, s.Status)
	}
	if s := byID[stale.ID]; s.State != schema.SessionStateStale || s.Status != schema.SessionStatusStale || s.OutcomeUnknown {
		t.Errorf("stale = %+v", s)
	}

	f.clock.Advance(60 * time.Second)
	f.srv.sweepSessions(context.Background())
	if s := f.rosterByID(t, f.rootTok)[stale.ID]; s.State != schema.SessionStateOffline || s.Status != schema.SessionStatusOffline {
		t.Errorf("offline = %+v", s)
	}

	// A day and a second of silence and it is history.
	f.clock.Advance(24*time.Hour + time.Second)
	if got := f.roster(t, f.rootTok, "general").Sessions; len(got) != 0 {
		t.Errorf("after more than 24h of silence: %+v, want none", got)
	}
	// Still there to be revived, though: the row was not deleted.
	f.heartbeat(t, f.aliceTok2, stale.ID)
	if s := f.rosterByID(t, f.rootTok)[stale.ID]; s.State != schema.SessionStateOnline {
		t.Errorf("revived = %+v", s)
	}
}

func (f *sessionFixture) heartbeat(t *testing.T, token string, id int64) schema.SessionV1 {
	t.Helper()
	rec := f.do(t, "POST", sessionPath(id, "/heartbeat"), token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	return validSession(t, rec)
}

func (f *sessionFixture) rosterByID(t *testing.T, token string) map[int64]schema.SessionV1 {
	t.Helper()
	out := map[int64]schema.SessionV1{}
	for _, s := range f.roster(t, token, "general").Sessions {
		out[s.ID] = s
	}
	return out
}

// Runtime and host are for an operator and the session's own principal; the
// model and the declared interventions are shown to every member. Nobody
// else's token, a body that lists a field the viewer may not have, or an
// unrelated agent can find the hidden fields anywhere in the response.
func TestSessionListRedaction(t *testing.T) {
	f := newSessionFixture(t)
	sess := f.create(t, f.botTok, agentBody)
	// A second agent in the channel, to be a viewer of bot's session.
	other, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "other")
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := f.srv.store.CreateCredential(context.Background(), "system", other.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	grantAgent(t, f.srv, other.ID, f.general.ID)

	tests := []struct {
		name   string
		token  string
		hidden bool
	}{
		{"the session's own principal", f.botTok, false},
		{"an operator", f.rootTok, false},
		{"a member", f.aliceTok, true},
		{"another member", f.bobTok, true},
		{"another agent", otherTok, true},
		{"an agent without sessions.manage", f.muteTok, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/sessions?channel=general", tt.token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			r := validRoster(t, rec)
			var got *schema.SessionV1
			for i := range r.Sessions {
				if r.Sessions[i].ID == sess.ID {
					got = &r.Sessions[i]
				}
			}
			if got == nil {
				t.Fatalf("session %d not in the roster", sess.ID)
			}
			if got.Model != "opus" || len(got.Interventions) != 2 {
				t.Errorf("model/interventions = %q/%v, want shown to everyone", got.Model, got.Interventions)
			}
			if tt.hidden {
				if got.Runtime != "" || got.Host != "" {
					t.Errorf("runtime/host = %q/%q, want absent", got.Runtime, got.Host)
				}
				for _, secret := range []string{"claude-code", "build-7", `"runtime"`, `"host"`} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Errorf("the response contains %q: %s", secret, rec.Body)
					}
				}
			} else if got.Runtime != "claude-code" || got.Host != "build-7" {
				t.Errorf("runtime/host = %q/%q, want filled", got.Runtime, got.Host)
			}
		})
	}
}

// Every way a credential or a principal stops being usable ends the sessions
// it should and only those: with one principal holding two credentials and a
// session on each, killing credential A leaves B's session online (P3-04,
// P3-10).
func TestSessionsEndWithTheirCredential(t *testing.T) {
	tests := []struct {
		name string
		// kill ends alice's first credential (or all of them).
		kill func(t *testing.T, f *sessionFixture, credA int64) *httptest.ResponseRecorder
		// bSurvives: whether the second credential's session lives on.
		bSurvives bool
		cause     string
		wantCode  int
	}{
		{"revoke", func(t *testing.T, f *sessionFixture, credA int64) *httptest.ResponseRecorder {
			return f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", credA), f.rootTok, "")
		}, true, "credential_revoked", http.StatusNoContent},
		{"rotate", func(t *testing.T, f *sessionFixture, credA int64) *httptest.ResponseRecorder {
			return f.do(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", credA), f.rootTok, "")
		}, true, "credential_revoked", http.StatusCreated},
		{"revoke-all", func(t *testing.T, f *sessionFixture, _ int64) *httptest.ResponseRecorder {
			return f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.alice.ID), f.rootTok, "")
		}, false, "credential_revoked", http.StatusOK},
		{"disable", func(t *testing.T, f *sessionFixture, _ int64) *httptest.ResponseRecorder {
			return f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.alice.ID), f.rootTok, "")
		}, false, "principal_disabled", http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSessionFixture(t)
			sessA := f.create(t, f.aliceTok, humanBody)
			sessB := f.create(t, f.aliceTok2, humanBody)
			sessBob := f.create(t, f.bobTok, humanBody)
			credA := f.credentialID(t, f.aliceTok)

			if rec := tt.kill(t, f, credA); rec.Code != tt.wantCode {
				t.Fatalf("%s = %d %s", tt.name, rec.Code, rec.Body)
			}

			live := f.rosterByID(t, f.rootTok)
			if _, ok := live[sessA.ID]; ok {
				t.Errorf("credential A's session is still listed")
			}
			if _, ok := live[sessB.ID]; ok != tt.bSurvives {
				t.Errorf("credential B's session listed = %v, want %v", ok, tt.bSurvives)
			}
			if s, ok := live[sessBob.ID]; !ok || s.State != schema.SessionStateOnline {
				t.Errorf("bob's session was touched: %+v", s)
			}
			if tt.bSurvives {
				// Still online: its credential is untouched and it can beat.
				if s := f.heartbeat(t, f.aliceTok2, sessB.ID); s.State != schema.SessionStateOnline {
					t.Errorf("B after the kill: %+v", s)
				}
			}
			// The dead credential's session cannot be revived, by anyone.
			assertErrorBody(t, f.do(t, "POST", sessionPath(sessA.ID, "/heartbeat"), f.aliceTok, ""), http.StatusUnauthorized, "unauthenticated")
			ends := f.auditRows(t, store.AuditSessionEnded)
			want := 1
			if !tt.bSurvives {
				want = 2
			}
			if len(ends) != want {
				t.Fatalf("session_ended rows = %d (%+v), want %d", len(ends), ends, want)
			}
			for _, e := range ends {
				if !strings.Contains(e.Detail, "cause="+tt.cause) {
					t.Errorf("row %+v: want cause=%s", e, tt.cause)
				}
				if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) {
					t.Errorf("row %+v: want the operator as actor", e)
				}
			}
		})
	}
}

// A session that was made through a credential that has since been revoked can
// never be created: the check is inside the transaction that inserts it.
func TestSessionCreateWithDeadCredentialIsRefused(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	credA := f.credentialID(t, f.aliceTok)
	if err := f.srv.store.RevokeCredential(ctx, "system", credA); err != nil {
		t.Fatal(err)
	}
	_, err := f.srv.store.CreateSession(ctx, "system", store.SessionParams{PrincipalID: f.alice.ID, CredentialID: credA, Client: "x"}, f.clock.Now())
	if err == nil {
		t.Fatal("a session was created behind a revoked credential")
	}
	// And a credential is not a way to speak for another principal.
	_, err = f.srv.store.CreateSession(ctx, "system", store.SessionParams{PrincipalID: f.bob.ID, CredentialID: f.credentialID(t, f.aliceTok2), Client: "x"}, f.clock.Now())
	if err == nil {
		t.Fatal("a session was created for a principal that does not own the credential")
	}
}

// The sweeper on an injected clock: online -> stale -> offline -> revived, a
// session_offline row once each time a session goes offline and nothing for
// stale or for a revival, and no row ever deleted.
func TestSessionSweepTransitions(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	sess := f.create(t, f.aliceTok, humanBody)
	steady := f.create(t, f.bobTok, humanBody)

	state := func() map[int64]schema.SessionState {
		out := map[int64]schema.SessionState{}
		for id, s := range f.rosterByID(t, f.rootTok) {
			out[id] = s.State
		}
		return out
	}
	step := func(d time.Duration, wantState schema.SessionState, wantOfflineRows int) {
		t.Helper()
		f.clock.Advance(d)
		f.heartbeat(t, f.bobTok, steady.ID) // bob keeps beating throughout
		f.srv.sweepSessions(ctx)
		if got := state()[sess.ID]; got != wantState {
			t.Fatalf("after +%s: state = %s, want %s", d, got, wantState)
		}
		if got := state()[steady.ID]; got != schema.SessionStateOnline {
			t.Fatalf("after +%s: the session that kept beating is %s", d, got)
		}
		if n := len(f.auditRows(t, store.AuditSessionOffline)); n != wantOfflineRows {
			t.Fatalf("after +%s: session_offline rows = %d, want %d", d, n, wantOfflineRows)
		}
	}

	step(29*time.Second, schema.SessionStateOnline, 0)  // not yet: 30s is the threshold
	step(2*time.Second, schema.SessionStateStale, 0)    // 31s silent
	step(10*time.Second, schema.SessionStateStale, 0)   // sweeping again changes nothing
	step(50*time.Second, schema.SessionStateOffline, 1) // 91s silent
	step(10*time.Second, schema.SessionStateOffline, 1) // and again: still one row

	// A heartbeat on an offline session revives the same row.
	if s := f.heartbeat(t, f.aliceTok, sess.ID); s.ID != sess.ID || s.State != schema.SessionStateOnline || s.Status != schema.SessionStatusOnline {
		t.Fatalf("revived = %+v", s)
	}
	if n := len(f.auditRows(t, store.AuditSessionOffline)); n != 1 {
		t.Errorf("a revival wrote an audit row: %d session_offline rows", n)
	}
	step(5*time.Second, schema.SessionStateOnline, 1)
	// Straight from online to offline in one pass (the server was down): once.
	step(120*time.Second, schema.SessionStateOffline, 2)

	for _, e := range f.auditRows(t, store.AuditSessionOffline) {
		if e.Actor != "system" || e.Subject != fmt.Sprintf("session:%d", sess.ID) {
			t.Errorf("session_offline row = %+v", e)
		}
	}
	// "stale" is never audited, and nothing was ended.
	if n := len(f.auditRows(t, store.AuditSessionEnded)); n != 0 {
		t.Errorf("session_ended rows = %d, want 0", n)
	}
}

// The thresholds come from the configuration, with the defaults for zero.
func TestSessionThresholds(t *testing.T) {
	tests := []struct {
		name                     string
		stale, offline           time.Duration
		wantStale, wantOffline   time.Duration
		wantValidationErrContain string
	}{
		{"defaults", 0, 0, 30 * time.Second, 90 * time.Second, ""},
		{"set", 2 * time.Second, 4 * time.Second, 2 * time.Second, 4 * time.Second, ""},
		{"equal", 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, "shorter"},
		{"stale past offline", 10 * time.Second, 5 * time.Second, 10 * time.Second, 5 * time.Second, "shorter"},
		{"stale alone past the default offline", 100 * time.Second, 0, 100 * time.Second, 90 * time.Second, "shorter"},
		{"negative", -time.Second, 5 * time.Second, -time.Second, 5 * time.Second, "positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, SessionStale: tt.stale, SessionOffline: tt.offline})
			stale, offline := srv.sessionThresholds()
			if stale != tt.wantStale || offline != tt.wantOffline {
				t.Errorf("thresholds = %s/%s, want %s/%s", stale, offline, tt.wantStale, tt.wantOffline)
			}
			err := ValidateSessionThresholds(stale, offline)
			if (err == nil) != (tt.wantValidationErrContain == "") || (err != nil && !strings.Contains(err.Error(), tt.wantValidationErrContain)) {
				t.Errorf("ValidateSessionThresholds = %v, want an error containing %q", err, tt.wantValidationErrContain)
			}
			// Serve refuses what the flags would have refused.
			if tt.wantValidationErrContain != "" {
				if err := srv.Serve(context.Background()); err == nil {
					t.Error("Serve started with thresholds that cannot work")
				}
			}
		})
	}
}

// The sweeper uses the configured thresholds, not the defaults.
func TestSessionSweepUsesConfiguredThresholds(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, SessionStale: 2 * time.Second, SessionOffline: 4 * time.Second})
	clock := newManualClock()
	srv.now = clock.Now
	ctx := context.Background()
	p, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "ann")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := srv.store.CreateChannelAs(ctx, "system", "room", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := srv.store.CreateCredential(ctx, "system", p.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateSession(ctx, "system", store.SessionParams{PrincipalID: p.ID, CredentialID: cred.ID, Client: "x"}, clock.Now()); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		advance time.Duration
		want    schema.SessionState
	}{
		{time.Second, schema.SessionStateOnline},
		{2 * time.Second, schema.SessionStateStale},
		{2 * time.Second, schema.SessionStateOffline},
	} {
		clock.Advance(step.advance)
		srv.sweepSessions(ctx)
		got, err := srv.store.ListChannelSessions(ctx, ch.ID, clock.Now(), time.Hour)
		if err != nil || len(got) != 1 || got[0].State != step.want {
			t.Fatalf("after +%s: %+v, %v; want one session %s", step.advance, got, err, step.want)
		}
	}
}
