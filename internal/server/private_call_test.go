package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const privateCallTestPair = "/v1/dms/42/calls"

func privateCallHTTPFixture(t *testing.T) (*authFixture, store.Principal, string, string) {
	t.Helper()
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	bob, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "bob")
	if err != nil {
		t.Fatal(err)
	}
	_, bobTok, err := f.srv.store.CreateCredential(ctx, "system", bob.ID, "phone", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, bobSecondTok, err := f.srv.store.CreateCredential(ctx, "system", bob.ID, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	db := privateCallFixtureDB(t, f)
	// The fixture's credentials are minted by "system", which the store
	// records as made by another. The guard (selfMadeCredentialRoutes) would
	// refuse them, so they stand in for sign-ins the people made themselves;
	// TestSelfMadeCredentialRoutesRefuseOthers (provenance_test.go) drives every route here with the other kinds.
	if _, err := db.ExecContext(ctx, "UPDATE credentials SET self_made = 1"); err != nil {
		t.Fatal(err)
	}
	// Conversation 42 is the DM store's own, opened by both people.
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO dm_conversations (id, lo, hi, created_at) VALUES (42, ?, ?, ?)`,
		min(f.alice.ID, bob.ID), max(f.alice.ID, bob.ID), now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.alice.ID, bob.ID} {
		if _, err := db.ExecContext(ctx, `INSERT INTO dm_viewer_state (principal_id, conversation_id, state, updated_at)
			VALUES (?, 42, 'open', ?)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	return f, bob, bobTok, bobSecondTok
}

func privateCallFixtureDB(t *testing.T, f *authFixture) *sql.DB {
	t.Helper()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("missing fixture database path")
	}
	db, err := sql.Open("sqlite", path.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func decodePrivateCallResponse(t *testing.T, rec *httptest.ResponseRecorder, want int) schema.PrivateCallResponseV1 {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
	}
	var out schema.PrivateCallResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	return out
}

func privateCallTestConflict(t *testing.T, rec *httptest.ResponseRecorder, state schema.PrivateCallState) schema.PrivateCallV1 {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	var out schema.PrivateCallConflictV1
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	if out.Call.State != state {
		t.Fatalf("conflict state = %s, want %s", out.Call.State, state)
	}
	return out.Call
}

func privateCallTestCommand(c schema.PrivateCallV1, action string) string {
	return fmt.Sprintf(`{"action_id":%q,"revision":%d}`, action, c.Revision)
}

func TestPrivateCallHTTPCrossedInvites(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	results := privateCallHTTPRace(
		func() *httptest.ResponseRecorder {
			return f.do(t, "POST", privateCallTestPair, f.aliceTok, `{"action_id":"alice"}`)
		},
		func() *httptest.ResponseRecorder {
			return f.do(t, "POST", privateCallTestPair, bobTok, `{"action_id":"bob"}`)
		},
	)
	created := decodePrivateCallResponse(t, results[0], http.StatusCreated)
	glare := decodePrivateCallResponse(t, results[1], http.StatusOK)
	if !created.Call.SelectedHere || !glare.Glare || glare.Call.SelectedHere || glare.Call.ID != created.Call.ID {
		t.Fatalf("crossed invites: %+v, %+v", created, glare)
	}
}

func privateCallHTTPRace(a, b func() *httptest.ResponseRecorder) [2]*httptest.ResponseRecorder {
	var result [2]*httptest.ResponseRecorder
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, send := range []func() *httptest.ResponseRecorder{a, b} {
		wg.Go(func() {
			<-start
			result[i] = send()
		})
	}
	close(start)
	wg.Wait()
	if result[0].Code < result[1].Code {
		result[0], result[1] = result[1], result[0]
	}
	return result
}

func TestPrivateCallHTTPCompetingActions(t *testing.T) {
	for _, test := range []struct {
		name, otherAction string
	}{
		{"two answers", "accept"},
		{"answer races cancel", "cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, bobTok, bobSecondTok := privateCallHTTPFixture(t)
			call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
				f.aliceTok, `{"action_id":"invite"}`), http.StatusCreated).Call
			base := "/v1/calls/" + call.ID + "/"
			otherToken := bobSecondTok
			if test.otherAction == "cancel" {
				otherToken = f.aliceTok
			}
			results := privateCallHTTPRace(
				func() *httptest.ResponseRecorder {
					return f.do(t, "POST", base+"accept", bobTok, privateCallTestCommand(call, "first"))
				},
				func() *httptest.ResponseRecorder {
					return f.do(t, "POST", base+test.otherAction, otherToken, privateCallTestCommand(call, "second"))
				},
			)
			winner := decodePrivateCallResponse(t, results[1], http.StatusOK)
			loser := privateCallTestConflict(t, results[0], winner.Call.State)
			if winner.Call.Revision != 2 || loser.Revision != 2 || loser.SelectedHere && test.otherAction == "accept" {
				t.Fatalf("race: winner %+v, loser %+v", winner, loser)
			}
			if test.otherAction == "accept" && (winner.Call.State != schema.PrivateCallStateAccepted || !winner.Call.SelectedHere) {
				t.Fatalf("selected endpoint: %+v", winner)
			}
		})
	}
}

func TestPrivateCallHTTPDeadlineAndHistoryPrivacy(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	now := time.Now()
	f.srv.now = func() time.Time { return now }
	invite := f.do(t, "POST", privateCallTestPair, f.aliceTok, `{"action_id":"one"}`)
	call := decodePrivateCallResponse(t, invite, http.StatusCreated).Call
	base := "/v1/calls/" + call.ID + "/"
	assertErrorBody(t, f.do(t, "POST", base+"accept", f.rootTok, privateCallTestCommand(call, "other")), 404, schema.ErrorCodeCallNotFound)
	assertErrorBody(t, f.do(t, "POST", base+"accept", f.aliceTok, privateCallTestCommand(call, "other")), 403, "forbidden")
	assertErrorBody(t, f.do(t, "GET", privateCallTestPair, f.rootTok, ""), 404, schema.ErrorCodeDMNotFound)
	now = call.RingDeadline.Time()
	privateCallTestConflict(t, f.do(t, "POST", base+"accept", bobTok, privateCallTestCommand(call, "too-late")), schema.PrivateCallStateMissed)
	replay := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
		`{"action_id":"one"}`), http.StatusOK)
	if !replay.Replayed || replay.Call.ID != call.ID || replay.Call.State != schema.PrivateCallStateMissed {
		t.Fatalf("retry: %+v", replay)
	}
	assertErrorBody(t, f.do(t, "POST", base+"cancel", f.aliceTok, privateCallTestCommand(call, "one")), 409, schema.ErrorCodeCallActionConflict)
	for _, test := range []struct {
		token string
		count int
	}{{f.aliceTok, 0}, {bobTok, 1}} {
		rec := f.do(t, "GET", privateCallTestPair, test.token, "")
		var history schema.ListPrivateCallsResponseV1
		if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
			t.Fatal(err)
		}
		if err := history.Validate(); rec.Code != http.StatusOK || err != nil || len(history.Calls) != test.count {
			t.Fatalf("history = %d %s (%v), want %d entries", rec.Code, rec.Body, err, test.count)
		}
		if test.count == 1 && (history.Calls[0].ID != call.ID || history.Calls[0].State != schema.PrivateCallStateMissed) {
			t.Fatalf("missed history: %+v", history)
		}
	}
}

func TestPrivateCallHTTPPermissionLoss(t *testing.T) {
	f, bob, bobTok, _ := privateCallHTTPFixture(t)
	ctx := context.Background()
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"invite"}`), http.StatusCreated).Call
	if _, err := f.srv.store.SetVoiceAllowed(ctx, "system", 0, bob.ID, false); err != nil {
		t.Fatal(err)
	}
	base := "/v1/calls/" + call.ID + "/"
	cancelled := privateCallTestConflict(t, f.do(t, "POST", base+"accept", bobTok,
		privateCallTestCommand(call, "accept")), schema.PrivateCallStateCancelled)
	if cancelled.Reason != "" || cancelled.SelectedHere {
		t.Fatalf("revoked ring: %+v", cancelled)
	}
	unavailable := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"invite-again"}`), http.StatusOK).Call
	if unavailable.State != schema.PrivateCallStateUnavailable || unavailable.Reason != "" {
		t.Fatalf("unavailable: %+v", unavailable)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/calls/"+unavailable.ID+"/decline", bobTok,
		privateCallTestCommand(unavailable, "decline")), 404, schema.ErrorCodeCallNotFound)
	assertErrorBody(t, f.do(t, "POST", privateCallTestPair, bobTok, `{"action_id":"invite-back"}`), 403, schema.ErrorCodeVoiceNotAllowed)
}

// privateCallTestListener lets Serve reach Accept without binding a socket.
type privateCallTestListener struct {
	accepting chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *privateCallTestListener) Accept() (net.Conn, error) {
	close(l.accepting)
	<-l.closed
	return nil, net.ErrClosed
}

func (l *privateCallTestListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *privateCallTestListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeReconcilesAndSweepsPrivateCalls(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	var now atomic.Int64
	now.Store(time.Now().UnixMilli())
	f.srv.now = func() time.Time { return time.UnixMilli(now.Load()) }
	previous := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"before-restart"}`), http.StatusCreated).Call
	now.Store(previous.RingDeadline.Time().UnixMilli())

	ln := &privateCallTestListener{accepting: make(chan struct{}), closed: make(chan struct{})}
	f.srv.ln = ln
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	var serveErr error
	go func() {
		defer close(served)
		serveErr = f.srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
			if serveErr != nil {
				t.Errorf("Serve: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	select {
	case <-ln.accepting:
	case <-served:
		t.Fatal("Serve exited before accepting connections")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
	}

	history := func() []schema.PrivateCallV1 {
		t.Helper()
		rec := f.do(t, "GET", privateCallTestPair, bobTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("history = %d: %s", rec.Code, rec.Body)
		}
		var history schema.ListPrivateCallsResponseV1
		if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
			t.Fatal(err)
		}
		return history.Calls
	}
	calls := history()
	if len(calls) != 1 || calls[0].ID != previous.ID ||
		calls[0].State != schema.PrivateCallStateFailed || calls[0].Reason != schema.PrivateCallReasonServerRestart ||
		calls[0].Revision != previous.Revision+1 {
		t.Fatalf("history when Serve begins accepting: %+v", calls)
	}

	next := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"after-restart"}`), http.StatusCreated).Call
	now.Store(next.RingDeadline.Time().UnixMilli())
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("sweeper did not expire the unanswered invite")
		case <-ticker.C:
			calls := history()
			if len(calls) == 2 {
				if calls[0].ID != next.ID || calls[0].State != schema.PrivateCallStateMissed {
					t.Fatalf("swept history: %+v", calls)
				}
				return
			}
		}
	}
}

func TestPrivateCallHTTPAuthentication(t *testing.T) {
	f, bob, bobTok, _ := privateCallHTTPFixture(t)
	off := newAuthFixture(t, AuthOff)
	ctx := context.Background()
	_, issued, err := f.srv.store.CreateCredential(ctx, fmt.Sprintf("principal:%d", f.root.ID), bob.ID, "operator issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.srv.store.CreateSignInLink(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID,
		bob.ID, schema.CreateSignInLinkRequestV1{}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := f.srv.store.RedeemSignInLink(ctx, store.HashSignInLinkCode(link.Code), f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"invite"}`), http.StatusCreated).Call
	attempt := "/v1/calls/" + call.ID + "/"
	for _, route := range []struct {
		method, path string
	}{
		{"GET", privateCallTestPair},
		{"POST", privateCallTestPair},
		{"GET", "/v1/calls/current"},
		{"POST", attempt + "accept"},
		{"POST", attempt + "decline"},
		{"POST", attempt + "cancel"},
		{"POST", attempt + "hangup"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			assertErrorBody(t, f.do(t, route.method, route.path, "", ""),
				http.StatusUnauthorized, "unauthenticated")
			assertErrorBody(t, f.do(t, route.method, route.path, f.botTok, ""),
				http.StatusForbidden, "forbidden")
			assertErrorBody(t, off.do(t, route.method, route.path, "", ""),
				http.StatusBadRequest, schema.ErrorCodeDMRequireAuth)
			for _, token := range []string{issued, redeemed.Token} {
				assertErrorBody(t, f.do(t, route.method, route.path, token, privateCallTestCommand(call, "action")),
					http.StatusForbidden, schema.ErrorCodeOwnSignInRequired)
			}
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("Authorization", "Bearer "+f.rootTok)
			req.Header.Set("Conch-Preview-As", fmt.Sprint(f.alice.ID))
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("preview response = %d: %s", rec.Code, rec.Body)
			}
		})
	}
	decodePrivateCallResponse(t, f.do(t, "POST", attempt+"decline", bobTok,
		privateCallTestCommand(call, "decline")), http.StatusOK)
}

func TestPrivateCallHTTPBlockDuringRingAndHistory(t *testing.T) {
	f, bob, bobTok, _ := privateCallHTTPFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.srv.now = func() time.Time { return now }

	// Bob has not been shown the conversation: only a ring publishes it to
	// him (the DM store's visibility rule), so until then it is, to him, the
	// not-found of a conversation never issued.
	if _, err := privateCallFixtureDB(t, f).ExecContext(ctx, "DELETE FROM dm_viewer_state WHERE principal_id = ? AND conversation_id = 42", bob.ID); err != nil {
		t.Fatal(err)
	}
	unknown := f.do(t, "GET", "/v1/dms/43/calls", bobTok, "")
	assertErrorBody(t, unknown, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	checkUnknown := func() {
		t.Helper()
		rec := f.do(t, "GET", privateCallTestPair, bobTok, "")
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Fatalf("known pair without a visible call: %d %s", rec.Code, rec.Body)
		}
	}
	checkUnknown()
	db := privateCallFixtureDB(t, f)
	setBlock := func(owner, blocked int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "INSERT INTO blocks VALUES (?, ?, ?)", owner, blocked, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	setBlock(bob.ID, f.alice.ID)
	unavailable := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"unavailable"}`), http.StatusOK).Call
	if unavailable.State != schema.PrivateCallStateUnavailable || unavailable.Reason != "" {
		t.Fatalf("blocked invite = %+v", unavailable)
	}
	checkUnknown()
	if _, err := db.ExecContext(ctx, "DELETE FROM blocks"); err != nil {
		t.Fatal(err)
	}
	ring := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"ring"}`), http.StatusCreated).Call
	if rec := f.do(t, "GET", privateCallTestPair, bobTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("after a ring the callee reads the pair's history: %d %s", rec.Code, rec.Body)
	}
	setBlock(f.alice.ID, bob.ID)
	now = ring.RingDeadline.Time()
	rec := f.do(t, "POST", "/v1/calls/"+ring.ID+"/accept", bobTok, privateCallTestCommand(ring, "late"))
	if ended := privateCallTestConflict(t, rec, schema.PrivateCallStateCancelled); ended.Reason != "" {
		t.Fatalf("block reason in call: %+v", ended)
	}
}

func TestPrivateCallHTTPInviteLimits(t *testing.T) {
	t.Run("principal", func(t *testing.T) {
		f, _, _, _ := privateCallHTTPFixture(t)
		now := time.Now()
		f.srv.now = func() time.Time { return now }
		for i := 0; i < 3; i++ {
			assertErrorBody(t, f.do(t, "POST", "/v1/dms/43/calls", f.aliceTok, fmt.Sprintf(`{"action_id":"missing%d"}`, i)),
				http.StatusNotFound, schema.ErrorCodeDMNotFound)
		}
		rec := f.do(t, "POST", privateCallTestPair, f.aliceTok, `{"action_id":"valid"}`)
		assertErrorBody(t, rec, http.StatusTooManyRequests, "rate_limited")
		if got := rec.Header().Get("Retry-After"); got != "20" {
			t.Fatalf("Retry-After = %q, want 20", got)
		}
		now = now.Add(20 * time.Second)
		decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
			`{"action_id":"valid"}`), http.StatusCreated)
	})

	t.Run("pair", func(t *testing.T) {
		f, _, bobTok, _ := privateCallHTTPFixture(t)
		now := time.Now()
		f.srv.now = func() time.Time { return now }
		for i := 0; i < 3; i++ {
			// Refill the principal bucket between attempts.
			call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
				fmt.Sprintf(`{"action_id":"invite%d"}`, i)), http.StatusCreated).Call
			decodePrivateCallResponse(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/decline", bobTok,
				privateCallTestCommand(call, fmt.Sprintf("decline%d", i))), http.StatusOK)
			now = now.Add(20 * time.Second)
		}
		replay := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
			`{"action_id":"invite2"}`), http.StatusOK)
		if !replay.Replayed || replay.Call.State != schema.PrivateCallStateDeclined {
			t.Fatalf("pair limit blocked a replay: %+v", replay)
		}
		rec := f.do(t, "POST", privateCallTestPair, f.aliceTok, `{"action_id":"next"}`)
		assertErrorBody(t, rec, http.StatusTooManyRequests, "rate_limited")
		if got := rec.Header().Get("Retry-After"); got != "540" {
			t.Fatalf("Retry-After = %q, want 540", got)
		}
		now = now.Add(540 * time.Second)
		decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
			`{"action_id":"next"}`), http.StatusCreated)
	})
}
