package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Notification preferences and the pending feed, through the handlers (issue
// #1713, Mobile M11-A). The visibility of the feed is held by the leak matrix
// (scoped_test.go) and the store's tests; this file holds the routes: their
// access rules, the preference object, the query rules and the limits.

const defaultPrefsJSON = `{"policy":"off","muted":[],"preview":false,"quiet_when_unreachable":false}` + "\n"

var notificationRoutes = []struct{ method, path, body string }{
	{"GET", "/v1/notifications/preferences", ""},
	{"PUT", "/v1/notifications/preferences", `{"policy":"off","muted":[]}`},
	{"GET", "/v1/notifications/pending", ""},
}

func TestNotificationPreferencesRoutes(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits() // this table writes more often than the burst

	t.Run("a fresh credential has the defaults, byte for byte", func(t *testing.T) {
		rec := f.do(t, "GET", "/v1/notifications/preferences", f.aliceTok, "")
		if rec.Code != http.StatusOK || rec.Body.String() != defaultPrefsJSON {
			t.Errorf("GET = %d %q, want 200 %q", rec.Code, rec.Body, defaultPrefsJSON)
		}
	})

	puts := []struct {
		name   string
		body   string
		status int
		want   string // the answer, when 200
	}{
		{"replace, normalised", `{"policy":"all","muted":[9,2,9,5],"preview":true,"quiet_when_unreachable":true}`, 200,
			`{"policy":"all","muted":[2,5,9],"preview":true,"quiet_when_unreachable":true}`},
		{"a switch left out is off", `{"policy":"mentions","muted":[]}`, 200,
			`{"policy":"mentions","muted":[],"preview":false,"quiet_when_unreachable":false}`},
		{"256 mutes", `{"policy":"all","muted":[` + joinIDs(1, 256) + `]}`, 200, ""},
		{"257 mutes", `{"policy":"all","muted":[` + joinIDs(1, 257) + `]}`, 400, ""},
		{"a zero id", `{"policy":"all","muted":[0]}`, 400, ""},
		{"a negative id", `{"policy":"all","muted":[-3]}`, 400, ""},
		{"no policy", `{"muted":[]}`, 400, ""},
		{"an empty policy", `{"policy":"","muted":[]}`, 400, ""},
		{"an unknown policy", `{"policy":"loud","muted":[]}`, 400, ""},
		{"muted missing", `{"policy":"all"}`, 400, ""},
		{"muted null", `{"policy":"all","muted":null}`, 400, ""},
		{"muted a string", `{"policy":"all","muted":"1"}`, 400, ""},
		{"an unknown field", `{"policy":"all","muted":[],"push":true}`, 400, ""},
		{"not JSON", `policy=all`, 400, ""},
		{"two documents", `{"policy":"all","muted":[]}{"policy":"off","muted":[]}`, 400, ""},
		{"an empty body", ``, 400, ""},
	}
	for _, tt := range puts {
		t.Run("PUT "+tt.name, func(t *testing.T) {
			// A known state to be left alone by a refusal.
			if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.aliceTok, `{"policy":"mentions","muted":[7],"preview":true}`); rec.Code != 200 {
				t.Fatalf("setup PUT = %d %s", rec.Code, rec.Body)
			}
			before := f.do(t, "GET", "/v1/notifications/preferences", f.aliceTok, "").Body.String()
			rec := f.do(t, "PUT", "/v1/notifications/preferences", f.aliceTok, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("PUT = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
			after := f.do(t, "GET", "/v1/notifications/preferences", f.aliceTok, "").Body.String()
			if tt.status != 200 {
				assertErrorBody(t, rec, 400, "invalid_request")
				if after != before {
					t.Errorf("a refused PUT changed the stored object: %s -> %s", before, after)
				}
				if strings.Contains(rec.Body.String(), "loud") {
					t.Errorf("the refusal echoes the policy: %s", rec.Body)
				}
				return
			}
			if tt.want != "" && strings.TrimSpace(rec.Body.String()) != tt.want {
				t.Errorf("PUT answered %s, want %s", rec.Body, tt.want)
			}
			if after != rec.Body.String() {
				t.Errorf("GET after PUT = %s, PUT answered %s: the answer is the stored object", after, rec.Body)
			}
		})
	}

	t.Run("preferences belong to the credential", func(t *testing.T) {
		g := newDirectoryFixture(t)
		g.srv.relaxNotificationLimits()
		_, second, err := g.srv.store.CreateCredential(context.Background(), "system", g.alice.ID, "second", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rec := g.do(t, "PUT", "/v1/notifications/preferences", g.aliceTok, `{"policy":"all","muted":[3]}`); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
		if got := g.do(t, "GET", "/v1/notifications/preferences", second, "").Body.String(); got != defaultPrefsJSON {
			t.Errorf("a second credential of the same person reads %s, want the defaults", got)
		}
		// And a different person never sees them.
		if got := g.do(t, "GET", "/v1/notifications/preferences", g.rootTok, "").Body.String(); got != defaultPrefsJSON {
			t.Errorf("another person reads %s, want the defaults", got)
		}
	})

	t.Run("signing out removes them", func(t *testing.T) {
		g := newDirectoryFixture(t)
		creds, err := g.srv.store.ListLiveCredentials(context.Background(), g.alice.ID)
		if err != nil || len(creds) == 0 {
			t.Fatal(creds, err)
		}
		if rec := g.do(t, "PUT", "/v1/notifications/preferences", g.aliceTok, `{"policy":"all","muted":[]}`); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
		if rec := g.do(t, "DELETE", "/v1/credentials/current", g.aliceTok, ""); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
			t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
		}
		got, err := g.srv.store.GetNotificationPreferences(context.Background(), creds[0].ID)
		if err != nil || got.Policy != schema.NotificationPolicyOff {
			t.Errorf("after sign-out: %+v, %v; want the defaults", got, err)
		}
		assertErrorBody(t, g.do(t, "GET", "/v1/notifications/preferences", g.aliceTok, ""), http.StatusUnauthorized, "unauthenticated")
	})
}

func joinIDs(from, to int) string {
	parts := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		parts = append(parts, fmt.Sprint(i))
	}
	return strings.Join(parts, ",")
}

// Who may use the routes: a person with a full credential. An agent gets
// human_required, a guest guest_not_allowed, a setup-scope credential is
// refused by the authentication layer, a dead or absent one is 401.
func TestNotificationRoutesAccess(t *testing.T) {
	f := newDirectoryFixture(t)
	disabled := newDirectoryFixture(t)
	dis, err := disabled.srv.store.DisablePrincipal(context.Background(), "system", disabled.alice.ID)
	if err != nil || !dis {
		t.Fatal(dis, err)
	}
	tests := []struct {
		who    string
		fix    *directoryFixture
		token  string
		status int
		code   string
	}{
		{"member", f, f.aliceTok, 200, ""},
		{"operator", f, f.op2Tok, 200, ""},
		{"owner", f, f.rootTok, 200, ""},
		{"agent", f, f.botTok, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"guest", f, f.guestTok, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"ended guest, token replayed", f, f.endedTok, http.StatusUnauthorized, "unauthenticated"},
		{"disabled person", disabled, disabled.aliceTok, http.StatusUnauthorized, "unauthenticated"},
		{"no credential", f, "", http.StatusUnauthorized, "unauthenticated"},
		{"a token that was never issued", f, "conch_" + strings.Repeat("A", 43), http.StatusUnauthorized, "unauthenticated"},
	}
	for _, tt := range tests {
		for _, r := range notificationRoutes {
			t.Run(tt.who+"/"+r.method+" "+r.path, func(t *testing.T) {
				rec := tt.fix.do(t, r.method, r.path, tt.token, r.body)
				if tt.code == "" {
					if rec.Code != tt.status {
						t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
					}
					return
				}
				assertErrorBody(t, rec, tt.status, tt.code)
			})
		}
	}

	t.Run("a setup-scope credential", func(t *testing.T) {
		o := newOperatorFixture(t)
		setup := o.redeemSetup(t, o.hostSetupToken(t, o.root.ID))
		for _, r := range notificationRoutes {
			assertErrorBody(t, o.send(t, r.method, r.path, setup.Token, r.body, ""), http.StatusForbidden, schema.ErrorCodeSetupIncomplete)
		}
	})
	t.Run("authentication off", func(t *testing.T) {
		off := &authFixture{srv: newTestServerWithConfig(t, Config{AuthMode: AuthOff})}
		for _, r := range notificationRoutes {
			assertErrorBody(t, off.do(t, r.method, r.path, "", r.body), http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth)
		}
	})
	t.Run("the routes are not set-up, previewable or administered", func(t *testing.T) {
		for _, r := range notificationRoutes {
			pattern := r.method + " " + r.path
			if setupScopeRoutes[pattern] {
				t.Errorf("%s is in setupScopeRoutes", pattern)
			}
			for _, rt := range f.srv.routes {
				if rt.pattern == pattern && rt.previewable {
					t.Errorf("%s is previewable", pattern)
				}
			}
			if adminCatalogPatterns[pattern] {
				t.Errorf("%s is in the admin catalog", pattern)
			}
		}
	})
	t.Run("preview-as is refused", func(t *testing.T) {
		for _, r := range notificationRoutes {
			req := fmt.Sprintf("%s %s", r.method, r.path)
			hreq := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
			hreq.Header.Set("Authorization", "Bearer "+f.rootTok)
			hreq.Header.Set(previewHeader, fmt.Sprint(f.alice.ID))
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, hreq)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with Conch-Preview-As = %d %s, want 403", req, rec.Code, rec.Body)
			}
		}
	})
}

// The feed's query: after, once, a non-negative integer, and nothing else.
func TestPendingNotificationsQuery(t *testing.T) {
	f := newDirectoryFixture(t)
	tests := []struct {
		query  string
		status int
	}{
		{"", 200},
		{"?after=0", 200},
		{"?after=12", 200},
		{"?after=", 400},
		{"?after=abc", 400},
		{"?after=-1", 400},
		{"?after=1.5", 400},
		{"?after=99999999999999999999", 400},
		{"?after=1&after=2", 400},
		{"?after=1&after=1", 400},
		{"?registration=7", 400},
		{"?after=1&registration=7", 400},
		{"?limit=5", 400},
		{"?After=1", 400},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/notifications/pending"+tt.query, f.aliceTok, "")
			if rec.Code != tt.status {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
			if tt.status == 400 {
				assertErrorBody(t, rec, 400, "invalid_request")
			}
		})
	}
}

// The feed end to end: preferences set over the API, messages posted over the
// API, the answer decoded and validated. The empty feed is [] and the cursor
// the one asked from.
func TestPendingNotificationsEndToEnd(t *testing.T) {
	f := newScopedFixture(t)
	empty := f.callREST(t, "GET", "/v1/notifications/pending?after=5", f.p("olga").token, "")
	if empty.status != 200 || empty.body != `{"summaries":[],"cursor":5,"more":false}`+"\n" {
		t.Errorf("an empty feed = %d %q", empty.status, empty.body)
	}
	if res := f.callREST(t, "PUT", "/v1/notifications/preferences", f.p("olga").token, `{"policy":"mentions","muted":[]}`); res.status != 200 {
		t.Fatal(res.body)
	}
	wide := f.mustPost(t, "ann", `{"body":"hello everyone"}`)
	mention := f.mustPost(t, "ann", `{"body":"hello @olga"}`)
	res := f.callREST(t, "GET", "/v1/notifications/pending", f.p("olga").token, "")
	var page schema.ListPendingNotificationsResponseV1
	decodeStrict(t, res.body, &page)
	if err := page.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(page.Summaries) != 1 || page.Summaries[0].MessageID != mention || page.Cursor != mention || page.More {
		t.Fatalf("mentions feed = %+v, want only %d (not %d)", page, mention, wide)
	}
	s := page.Summaries[0]
	if s.Kind != schema.NotificationKindMention || s.ChannelName != "ops" || s.ChannelID != f.ops.ID || s.AuthorID != f.p("ann").ID || s.AuthorName != "ann" || s.Text != "" {
		t.Errorf("summary = %+v", s)
	}
	// The next fetch from the cursor is empty.
	res = f.callREST(t, "GET", fmt.Sprintf("/v1/notifications/pending?after=%d", page.Cursor), f.p("olga").token, "")
	decodeStrict(t, res.body, &page)
	if len(page.Summaries) != 0 || page.Cursor != mention {
		t.Errorf("after the cursor: %+v", page)
	}
}

// Over the limit a route answers 429 with Retry-After before the store is
// touched: here the table the handlers use is gone, and a request that reached
// it would be a 500.
func TestNotificationRateLimits(t *testing.T) {
	tests := []struct {
		name          string
		method, path  string
		body          string
		write         bool
		burst         int
		wantRetryRoof int
	}{
		{"read preferences", "GET", "/v1/notifications/preferences", "", false, 3, 60},
		{"the feed", "GET", "/v1/notifications/pending", "", false, 3, 60},
		{"write preferences", "PUT", "/v1/notifications/preferences", `{"policy":"all","muted":[]}`, true, 2, 600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDirectoryFixture(t)
			limit := inviteLimit{name: "test", burst: tt.burst, window: time.Hour}
			relaxed := inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour}
			if tt.write {
				f.srv.setNotificationLimits(limit, relaxed)
			} else {
				f.srv.setNotificationLimits(relaxed, limit)
			}
			for i := range tt.burst {
				if rec := f.do(t, tt.method, tt.path, f.aliceTok, tt.body); rec.Code != 200 {
					t.Fatalf("request %d = %d %s, want 200", i+1, rec.Code, rec.Body)
				}
			}
			// The handlers' table goes away (the credential lookup does not use
			// it): a request that reached the store would be a 500, so only a
			// refusal that comes before the store can answer 429.
			legacyExec(t, f.srv, "DROP TABLE notification_preferences")
			rec := f.do(t, tt.method, tt.path, f.aliceTok, tt.body)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("over the limit = %d %s, want 429", rec.Code, rec.Body)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After")
			}
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		})
	}

	t.Run("buckets are per person and per credential", func(t *testing.T) {
		f := newDirectoryFixture(t)
		_, second, err := f.srv.store.CreateCredential(context.Background(), "system", f.alice.ID, "second", nil)
		if err != nil {
			t.Fatal(err)
		}
		limit := inviteLimit{name: "test", burst: 1, window: time.Hour}
		f.srv.setNotificationLimits(limit, limit)
		// Reads: per credential, so the second sign-in and another person have their own.
		for _, tok := range []string{f.aliceTok, second, f.rootTok} {
			if rec := f.do(t, "GET", "/v1/notifications/pending", tok, ""); rec.Code != 200 {
				t.Fatalf("first read = %d", rec.Code)
			}
		}
		if rec := f.do(t, "GET", "/v1/notifications/pending", f.aliceTok, ""); rec.Code != http.StatusTooManyRequests {
			t.Errorf("second read on one credential = %d", rec.Code)
		}
		// Writes: per person, so both of alice's credentials draw on one bucket.
		if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.aliceTok, `{"policy":"all","muted":[]}`); rec.Code != 200 {
			t.Fatalf("first write = %d", rec.Code)
		}
		if rec := f.do(t, "PUT", "/v1/notifications/preferences", second, `{"policy":"all","muted":[]}`); rec.Code != http.StatusTooManyRequests {
			t.Errorf("a second credential's write = %d, want 429 from the person's bucket", rec.Code)
		}
		if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.rootTok, `{"policy":"all","muted":[]}`); rec.Code != 200 {
			t.Errorf("another person's write = %d", rec.Code)
		}
	})

	t.Run("the defaults", func(t *testing.T) {
		d, r := defaultNotificationWriteLimit(), defaultNotificationReadLimit()
		if d.burst != 10 || d.window != 10*time.Minute || r.burst != 30 || r.window != time.Minute {
			t.Errorf("defaults = %+v, %+v; want writes 10 per 10 minutes, reads 30 per minute", d, r)
		}
	})
}
