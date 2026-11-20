package server

import (
	"context"
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
	if err := f.srv.store.BindPrivateCallConversation(ctx, 42, f.alice.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	return f, bob, bobTok, bobSecondTok
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
		f.aliceTok, `{"action_id":"invite-again"}`), http.StatusCreated).Call
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
	f := newAuthFixture(t, AuthRequired)
	off := newAuthFixture(t, AuthOff)
	const attempt = "/v1/calls/abcdefghijklmnopqrstuv/"
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
}
