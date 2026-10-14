package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ticketFixture is a member fixture whose server clock is a manual one, so
// the thirty seconds a ticket lives are crossed without sleeping.
type ticketFixture struct {
	*memberFixture
	clock *manualClock
}

func newTicketFixture(t *testing.T) *ticketFixture {
	t.Helper()
	f := &ticketFixture{memberFixture: newMemberFixture(t), clock: newManualClock()}
	f.srv.now = f.clock.Now
	return f
}

// ticketFor asks for a ticket as the holder of token and returns it.
func (f *ticketFixture) ticketFor(t *testing.T, token string) string {
	t.Helper()
	rec := f.do(t, "POST", "/v1/ws-tickets", token, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue ticket = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.WSTicketV1](t, rec).Ticket
}

// seen is what a handler behind the authentication step observed.
type seen struct {
	reached      bool
	caller       store.Principal
	hasCaller    bool
	credentialID int64
	hasCredID    bool
}

// serve sends r through the real authentication step with a handler that only
// records the identity it was given, and returns the response and what the
// handler saw. Routing is the real route table's, so the opt-in is the real one.
func (f *ticketFixture) serve(r *http.Request) (*httptest.ResponseRecorder, seen) {
	var got seen
	h := f.srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		got.caller, got.hasCaller = callerFrom(r.Context())
		got.credentialID, got.hasCredID = credentialIDFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, got
}

func wsTicketRequest(ticket string) *http.Request {
	return httptest.NewRequest("GET", "/v2/ws?channel=alpha&ticket="+url.QueryEscape(ticket), nil)
}

// wellFormedUnknownTicket has the shape of a ticket and was never issued.
func wellFormedUnknownTicket() string {
	return schema.WSTicketPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, schema.WSTicketRandomBytes))
}

func TestCreateWSTicket(t *testing.T) {
	f := newTicketFixture(t)
	tests := []struct {
		name  string
		token string
		path  string
		want  int
		code  string
	}{
		{"human", f.aliceTok, "/v1/ws-tickets", http.StatusCreated, ""},
		{"agent", f.botTok, "/v1/ws-tickets", http.StatusCreated, ""},
		{"operator", f.rootTok, "/v1/ws-tickets", http.StatusCreated, ""},
		{"no credential", "", "/v1/ws-tickets", http.StatusUnauthorized, "unauthenticated"},
		{"bad credential", "conch_" + strings.Repeat("Z", 43), "/v1/ws-tickets", http.StatusUnauthorized, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "POST", tt.path, tt.token, "")
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			if tt.code != "" {
				assertErrorBody(t, rec, tt.want, tt.code)
				return
			}
			got := decodeBody[schema.WSTicketV1](t, rec)
			if !strings.HasPrefix(got.Ticket, schema.WSTicketPrefix) || len(got.Ticket) != schema.WSTicketLength {
				t.Errorf("ticket has prefix/length wrong: %d chars", len(got.Ticket))
			}
			if want := f.clock.Now().Add(30 * time.Second); !got.ExpiresAt.Time().Equal(want) {
				t.Errorf("expires_at = %v, want %v", got.ExpiresAt.Time(), want)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
		})
	}

	t.Run("two tickets differ", func(t *testing.T) {
		if a, b := f.ticketFor(t, f.aliceTok), f.ticketFor(t, f.aliceTok); a == b {
			t.Error("two requests returned the same ticket")
		}
	})

	// A ticket cannot be exchanged for more tickets: the route does not take one.
	t.Run("a ticket does not authenticate the issuing route", func(t *testing.T) {
		tk := f.ticketFor(t, f.aliceTok)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/ws-tickets?ticket="+tk, nil))
		assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
	})
}

func TestCreateWSTicketAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	rec := f.do(t, "POST", "/v1/ws-tickets", "", "")
	assertErrorBody(t, rec, http.StatusBadRequest, schema.ErrorCodeTicketsRequireAuth)
	// A bearer is not read under AuthOff either, so it changes nothing.
	rec = f.do(t, "POST", "/v1/ws-tickets", f.aliceTok, "")
	assertErrorBody(t, rec, http.StatusBadRequest, schema.ErrorCodeTicketsRequireAuth)
}

// TestRedeemWSTicket drives the authentication step on the opted-in route and
// checks who the request becomes, and when it is refused.
func TestRedeemWSTicket(t *testing.T) {
	f := newTicketFixture(t)
	aliceCred, aliceTok := f.newCred(t, f.alice.ID)
	botCred, botTok := f.newCred(t, f.bot.ID)

	tests := []struct {
		name string
		// run does the arranging and returns the request to send.
		run func(t *testing.T) *http.Request
		// wantStatus is 204 when the handler is reached, else 401.
		wantStatus int
		wantCaller int64
		wantCred   int64
	}{
		{
			name: "bound to the credential that asked: human",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(f.ticketFor(t, aliceTok))
			},
			wantStatus: http.StatusNoContent, wantCaller: f.alice.ID, wantCred: aliceCred,
		},
		{
			name: "bound to the credential that asked: agent",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(f.ticketFor(t, botTok))
			},
			wantStatus: http.StatusNoContent, wantCaller: f.bot.ID, wantCred: botCred,
		},
		{
			name: "operator keeps the operator role",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(f.ticketFor(t, f.rootTok))
			},
			wantStatus: http.StatusNoContent, wantCaller: f.root.ID,
		},
		{
			name: "no ticket value",
			run: func(t *testing.T) *http.Request {
				return httptest.NewRequest("GET", "/v2/ws?channel=alpha&ticket=", nil)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "unknown ticket of the right shape",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(wellFormedUnknownTicket())
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong length",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(f.ticketFor(t, aliceTok) + "x")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a bearer in the ticket parameter",
			run: func(t *testing.T) *http.Request {
				return wsTicketRequest(aliceTok)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a bearer in a token parameter",
			run: func(t *testing.T) *http.Request {
				return httptest.NewRequest("GET", "/v2/ws?channel=alpha&token="+aliceTok, nil)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "two ticket parameters",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				return httptest.NewRequest("GET", "/v2/ws?ticket="+tk+"&ticket="+wellFormedUnknownTicket(), nil)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "single use: the second spend is refused",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				if rec, got := f.serve(wsTicketRequest(tk)); rec.Code != http.StatusNoContent || !got.reached {
					t.Fatalf("first spend = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "valid until the instant it expires",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				f.clock.Advance(30*time.Second - time.Millisecond)
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusNoContent, wantCaller: f.alice.ID, wantCred: aliceCred,
		},
		{
			name: "useless after thirty seconds",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				f.clock.Advance(30 * time.Second)
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "header and ticket: the header wins and the ticket stays unspent",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, botTok)
				r := wsTicketRequest(tk)
				r.Header.Set("Authorization", "Bearer "+aliceTok)
				if rec, got := f.serve(r); rec.Code != http.StatusNoContent || got.caller.ID != f.alice.ID || got.credentialID != aliceCred {
					t.Fatalf("header request = %d, caller %d cred %d; want alice", rec.Code, got.caller.ID, got.credentialID)
				}
				return wsTicketRequest(tk) // still the bot's, still good
			},
			wantStatus: http.StatusNoContent, wantCaller: f.bot.ID, wantCred: botCred,
		},
		{
			name: "a bad header with a ticket is refused and the ticket stays unspent",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				r := wsTicketRequest(tk)
				r.Header.Set("Authorization", "Bearer conch_"+strings.Repeat("Z", 43))
				if rec, got := f.serve(r); rec.Code != http.StatusUnauthorized || got.reached {
					t.Fatalf("bad header request = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusNoContent, wantCaller: f.alice.ID, wantCred: aliceCred,
		},
		{
			name: "any Authorization header means the ticket is not read",
			run: func(t *testing.T) *http.Request {
				tk := f.ticketFor(t, aliceTok)
				r := wsTicketRequest(tk)
				r.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
				if rec, got := f.serve(r); rec.Code != http.StatusUnauthorized || got.reached {
					t.Fatalf("basic header request = %d", rec.Code)
				}
				empty := wsTicketRequest(tk)
				empty.Header.Set("Authorization", "")
				if rec, got := f.serve(empty); rec.Code != http.StatusUnauthorized || got.reached {
					t.Fatalf("empty header request = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusNoContent, wantCaller: f.alice.ID, wantCred: aliceCred,
		},
		{
			name: "credential revoked after the ticket was issued",
			run: func(t *testing.T) *http.Request {
				id, tok := f.newCred(t, f.alice.ID)
				tk := f.ticketFor(t, tok)
				if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", id), f.rootTok, ""); rec.Code != 204 {
					t.Fatalf("revoke = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "credential expired after the ticket was issued",
			run: func(t *testing.T) *http.Request {
				exp := time.Now().Add(300 * time.Millisecond)
				_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", f.alice.ID, "short", &exp)
				if err != nil {
					t.Fatal(err)
				}
				tk := f.ticketFor(t, tok)
				time.Sleep(400 * time.Millisecond) // the store's clock is the real one
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "principal disabled after the ticket was issued",
			run: func(t *testing.T) *http.Request {
				p, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "dana")
				if err != nil {
					t.Fatal(err)
				}
				_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", p.ID, "t", nil)
				if err != nil {
					t.Fatal(err)
				}
				tk := f.ticketFor(t, tok)
				if rec := f.do(t, "POST", f.disablePath(p.ID), f.rootTok, ""); rec.Code != 204 {
					t.Fatalf("disable = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "all credentials revoked after the ticket was issued",
			run: func(t *testing.T) *http.Request {
				p, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "erin")
				if err != nil {
					t.Fatal(err)
				}
				_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", p.ID, "t", nil)
				if err != nil {
					t.Fatal(err)
				}
				tk := f.ticketFor(t, tok)
				if rec := f.do(t, "POST", f.revokeAllPath(p.ID), f.rootTok, ""); rec.Code != 200 {
					t.Fatalf("revoke-all = %d", rec.Code)
				}
				return wsTicketRequest(tk)
			},
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, got := f.serve(tt.run(t))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
				if h := rec.Header().Get("WWW-Authenticate"); h != `Bearer realm="conch"` {
					t.Errorf("WWW-Authenticate = %q", h)
				}
				if got.reached {
					t.Error("the handler ran for a refused request")
				}
				return
			}
			if !got.hasCaller || !got.hasCredID {
				t.Fatalf("handler context lacks the caller (%v) or credential id (%v)", got.hasCaller, got.hasCredID)
			}
			if got.caller.ID != tt.wantCaller {
				t.Errorf("caller = %d, want %d", got.caller.ID, tt.wantCaller)
			}
			if tt.wantCred != 0 && got.credentialID != tt.wantCred {
				t.Errorf("credential id = %d, want %d", got.credentialID, tt.wantCred)
			}
			if tt.wantCaller == f.root.ID && got.caller.Role != store.RoleOperator {
				t.Errorf("role = %q, want operator", got.caller.Role)
			}
		})
	}
}

// TestWSTicketOnlyOnOptedInRoutes: the parameter is read only where a route
// asks for it. Everywhere else a request with no header is refused, and the
// ticket is not spent by the attempt.
func TestWSTicketOnlyOnOptedInRoutes(t *testing.T) {
	f := newTicketFixture(t)

	var optedIn []string
	for _, rt := range f.srv.routes {
		if rt.acceptsTicket {
			optedIn = append(optedIn, rt.pattern)
		}
	}
	if len(optedIn) != 2 || optedIn[0] != "GET /v2/ws" || optedIn[1] != "GET /v1/events/ws" {
		t.Errorf("routes that accept a ticket = %v, want only GET /v2/ws and GET /v1/events/ws", optedIn)
	}

	tk := f.ticketFor(t, f.aliceTok)
	for _, rt := range f.srv.routes {
		if rt.acceptsTicket || rt.access == accessExempt {
			continue
		}
		exp := routeExpectations[rt.pattern]
		path := exp.path
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		t.Run(rt.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(routeMethod(rt.pattern), path+sep+"ticket="+tk, strings.NewReader(exp.body))
			f.srv.Handler().ServeHTTP(rec, req)
			assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
		})
	}
	// The explicit case from the issue.
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/channels?ticket="+tk, nil))
	assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")

	// None of those spent it.
	if rec, got := f.serve(wsTicketRequest(tk)); rec.Code != http.StatusNoContent || got.caller.ID != f.alice.ID {
		t.Errorf("ticket after the non-opted attempts = %d, want it still good", rec.Code)
	}
}

// TestWSTicketPerCredentialBound: at most eight unspent tickets per
// credential; the ninth is 429. The bound is per credential, a spent ticket
// frees its place, and so does the passing of thirty seconds.
func TestWSTicketPerCredentialBound(t *testing.T) {
	f := newTicketFixture(t)
	_, otherTok := f.newCred(t, f.alice.ID) // same principal, its own budget

	fill := func(token string) []string {
		var out []string
		for range schema.MaxWSTicketsPerCredential {
			out = append(out, f.ticketFor(t, token))
		}
		return out
	}
	refused := func(token string) {
		t.Helper()
		rec := f.do(t, "POST", "/v1/ws-tickets", token, "")
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeTooManyTickets)
	}

	held := fill(f.aliceTok)
	refused(f.aliceTok)

	t.Run("another credential has its own budget", func(t *testing.T) {
		f.ticketFor(t, otherTok)
		f.ticketFor(t, f.botTok)
	})
	t.Run("spending one frees a place", func(t *testing.T) {
		if rec, _ := f.serve(wsTicketRequest(held[0])); rec.Code != http.StatusNoContent {
			t.Fatalf("spend = %d", rec.Code)
		}
		f.ticketFor(t, f.aliceTok)
		refused(f.aliceTok)
	})
	t.Run("unspent tickets count until they expire", func(t *testing.T) {
		f.clock.Advance(29 * time.Second)
		refused(f.aliceTok)
	})
	t.Run("expiry frees all of them", func(t *testing.T) {
		f.clock.Advance(2 * time.Second)
		fill(f.aliceTok)
		refused(f.aliceTok)
	})
}

// TestWSTicketStoreConcurrentSpend: many goroutines spend one ticket at once
// and exactly one wins. Run under -race, it also covers the map's mutex.
func TestWSTicketStoreConcurrentSpend(t *testing.T) {
	var ts wsTicketStore
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	ticket, _, err := ts.issue(7, 9, now)
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e, ok := ts.redeem(ticket, now.Add(time.Second)); ok {
				if e.credentialID != 9 || e.principalID != 7 {
					t.Errorf("entry = %+v", e)
				}
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d spends succeeded, want exactly 1", wins.Load())
	}
	if len(ts.byHash) != 0 || len(ts.held) != 0 {
		t.Errorf("store not empty after the spend: %d tickets, %d counters", len(ts.byHash), len(ts.held))
	}
}

// TestWSTicketStoreSweeps: expired tickets leave the map, so it cannot grow
// without bound, and the per-credential counter goes with them.
func TestWSTicketStoreSweeps(t *testing.T) {
	var ts wsTicketStore
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for cred := range int64(20) {
		if _, _, err := ts.issue(1, cred, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := ts.redeem(wellFormedUnknownTicket(), now.Add(31*time.Second)); ok {
		t.Fatal("unknown ticket redeemed")
	}
	if len(ts.byHash) != 0 || len(ts.held) != 0 {
		t.Errorf("after expiry: %d tickets, %d counters, want none", len(ts.byHash), len(ts.held))
	}
}

// TestWSTicketStoreDeletesOnFirstLookup: a lookup removes the ticket whatever
// its result, an expired one included, before anything else is decided. The
// sweep is held off here so the lookup is the only thing that can remove it.
func TestWSTicketStoreDeletesOnFirstLookup(t *testing.T) {
	var ts wsTicketStore
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	ticket, _, err := ts.issue(1, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	late := now.Add(45 * time.Second)
	ts.swept = late
	if _, ok := ts.redeem(ticket, late); ok {
		t.Fatal("an expired ticket was redeemed")
	}
	if len(ts.byHash) != 0 || len(ts.held) != 0 {
		t.Errorf("the expired ticket is still held: %d tickets, %d counters", len(ts.byHash), len(ts.held))
	}
}

// openTicketSocket dials path on base with no Authorization header.
func openTicketSocket(ctx context.Context, base, path string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, base+path, nil) //nolint:bodyclose // closed by the caller when refused
}

func TestWSTicketOpensSocket(t *testing.T) {
	f := newTicketFixture(t)
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	refusedWith := func(t *testing.T, path string, want int) {
		t.Helper()
		conn, resp, err := openTicketSocket(ctx, base, path)
		if err == nil {
			_ = conn.CloseNow()
			t.Fatalf("%s: upgrade succeeded, want %d", path, want)
		}
		if resp == nil {
			t.Fatalf("%s: no response (%v)", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status = %d, want %d", path, resp.StatusCode, want)
		}
	}

	t.Run("opens and receives as the issuer", func(t *testing.T) {
		tk := f.ticketFor(t, f.aliceTok)
		conn, _, err := openTicketSocket(ctx, base, "/v2/ws?channel=alpha&ticket="+tk) //nolint:bodyclose // upgraded
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		r := startReader(ctx, conn)
		id := f.postAs(t, f.aliceTok, "alpha", "through a ticket")
		r.waitForID(t, id)
	})

	t.Run("a second upgrade with the same ticket is 401", func(t *testing.T) {
		tk := f.ticketFor(t, f.aliceTok)
		path := "/v2/ws?channel=alpha&ticket=" + tk
		conn, _, err := openTicketSocket(ctx, base, path) //nolint:bodyclose // upgraded
		if err != nil {
			t.Fatalf("first dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		refusedWith(t, path, http.StatusUnauthorized)
	})

	t.Run("a failed first upgrade still spends it", func(t *testing.T) {
		tk := f.ticketFor(t, f.aliceTok)
		refusedWith(t, "/v2/ws?ticket="+tk, http.StatusBadRequest) // no channel
		refusedWith(t, "/v2/ws?channel=alpha&ticket="+tk, http.StatusUnauthorized)
		tk = f.ticketFor(t, f.aliceTok)
		refusedWith(t, "/v2/ws?channel=nosuch&ticket="+tk, http.StatusNotFound)
		refusedWith(t, "/v2/ws?channel=alpha&ticket="+tk, http.StatusUnauthorized)
	})

	t.Run("a ticket does no more than its issuer: a non-member is refused", func(t *testing.T) {
		tk := f.ticketFor(t, f.carolTok)
		refusedWith(t, "/v2/ws?channel=alpha&ticket="+tk, http.StatusNotFound)
	})

	t.Run("an expired ticket is 401", func(t *testing.T) {
		tk := f.ticketFor(t, f.aliceTok)
		f.clock.Advance(30 * time.Second)
		refusedWith(t, "/v2/ws?channel=alpha&ticket="+tk, http.StatusUnauthorized)
	})

	t.Run("the older sockets do not take a ticket", func(t *testing.T) {
		for _, p := range []string{"/v1/ws", "/v0/ws"} {
			tk := f.ticketFor(t, f.aliceTok)
			refusedWith(t, p+"?channel=alpha&ticket="+tk, http.StatusUnauthorized)
		}
	})

	t.Run("a bearer in the query is never read", func(t *testing.T) {
		refusedWith(t, "/v2/ws?channel=alpha&token="+f.aliceTok, http.StatusUnauthorized)
		refusedWith(t, "/v2/ws?channel=alpha&ticket="+f.aliceTok, http.StatusUnauthorized)
		refusedWith(t, "/v2/ws?channel=alpha&access_token="+f.aliceTok, http.StatusUnauthorized)
	})
}

// TestWSTicketSocketClosedWhenCredentialRevoked: a socket opened with a ticket
// is on the same re-check as one opened with a bearer, and is closed with 1008
// when its credential is revoked, while another socket of the same principal
// on another credential stays open.
func TestWSTicketSocketClosedWhenCredentialRevoked(t *testing.T) {
	f := newTicketFixture(t)
	f.srv.credRecheckInterval = 25 * time.Millisecond
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	credA, tokA := f.newCred(t, f.alice.ID)
	_, tokB := f.newCred(t, f.alice.ID)

	open := func(token string) *wsReader {
		tk := f.ticketFor(t, token)
		conn, _, err := openTicketSocket(ctx, base, "/v2/ws?channel=alpha&ticket="+tk) //nolint:bodyclose // upgraded
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		return startReader(ctx, conn)
	}
	a, b := open(tokA), open(tokB)

	time.Sleep(100 * time.Millisecond) // several ticks: still open
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", credA), f.rootTok, ""); rec.Code != 204 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	closeErrContains(t, a.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)

	id := f.postAs(t, tokB, "alpha", "still here")
	b.waitForID(t, id)
	select {
	case <-b.done:
		t.Errorf("socket on the other credential closed: %v", b.err)
	default:
	}
}

// TestWSTicketNeverLogged: a whole cycle of issuing and spending tickets, with
// refusals, leaves no ticket, and no 20-character piece of one, in the log
// output, in any response other than the one that issued it, or in the audit
// log.
func TestWSTicketNeverLogged(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newTicketFixture(t)
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var tickets, responses []string
	issue := func(token string) string {
		tk := f.ticketFor(t, token)
		tickets = append(tickets, tk)
		return tk
	}
	dial := func(path string) {
		conn, resp, err := openTicketSocket(ctx, base, path)
		if err == nil {
			_ = conn.CloseNow()
			return
		}
		if resp != nil {
			responses = append(responses, resp.Status)
			for k, v := range resp.Header {
				responses = append(responses, k+": "+strings.Join(v, ","))
			}
			_ = resp.Body.Close()
		}
	}

	good := issue(f.aliceTok)
	dial("/v2/ws?channel=alpha&ticket=" + good) // opens
	dial("/v2/ws?channel=alpha&ticket=" + good) // spent
	dial("/v2/ws?ticket=" + issue(f.aliceTok))  // 400, still spends it
	dial("/v2/ws?channel=nosuch&ticket=" + issue(f.aliceTok))
	dial("/v2/ws?channel=alpha&ticket=" + issue(f.carolTok)) // not a member
	expired := issue(f.aliceTok)
	f.clock.Advance(time.Minute)
	dial("/v2/ws?channel=alpha&ticket=" + expired)
	revokedID, revokedTok := f.newCred(t, f.alice.ID)
	revoked := issue(revokedTok)
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", revokedID), f.rootTok, ""); rec.Code != 204 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	dial("/v2/ws?channel=alpha&ticket=" + revoked)
	for range schema.MaxWSTicketsPerCredential + 1 { // the 429
		rec := f.do(t, "POST", "/v1/ws-tickets", f.botTok, "")
		if rec.Code == http.StatusCreated {
			tickets = append(tickets, decodeBody[schema.WSTicketV1](t, rec).Ticket)
		} else {
			responses = append(responses, rec.Body.String())
		}
	}
	// A ticket on a route that does not take one.
	rec := f.do(t, "GET", "/v1/channels?ticket="+issue(f.aliceTok), "", "")
	responses = append(responses, rec.Body.String())

	var audit strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&audit, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	haystacks := map[string]string{
		"logs":      logs.buf.String(),
		"responses": strings.Join(responses, "\n"),
		"audit":     audit.String(),
	}
	if len(tickets) < 10 {
		t.Fatalf("only %d tickets were issued; the cycle did not run", len(tickets))
	}
	for name, hay := range haystacks {
		for _, tk := range tickets {
			if strings.Contains(hay, tk) {
				t.Errorf("%s contain a ticket", name)
			}
			// A fragment of the random part, and the hash the server keeps.
			for i := len(schema.WSTicketPrefix); i+20 <= len(tk); i += 10 {
				if strings.Contains(hay, tk[i:i+20]) {
					t.Errorf("%s contain a fragment of a ticket", name)
				}
			}
			if strings.Contains(hay, sha256Hex(tk)) {
				t.Errorf("%s contain the hash of a ticket", name)
			}
		}
		if strings.Contains(hay, schema.WSTicketPrefix) {
			t.Errorf("%s contain the ticket prefix, so something printed a ticket or its URL", name)
		}
	}
	for _, e := range f.audit(t) {
		if strings.Contains(strings.ToLower(e.Action), "ticket") {
			t.Errorf("audit row %q: a ticket must not be audited", e.Action)
		}
	}
}
