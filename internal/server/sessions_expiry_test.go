package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
)

// A credential that expires ends its sessions at the next sweep (issue #301):
// the roster stops showing them then, not a day later, and the events sockets
// are told, as they are for a session that went stale.
func TestSweepEndsSessionsOfExpiredCredential(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	// The store judges expiry by the real clock when a credential is made, so
	// the server's clock starts at the real time and moves from there.
	clock := &manualClock{t: time.Now().Truncate(time.Millisecond)}
	f.srv.now = clock.Now
	exp := clock.Now().Add(time.Hour)
	_, tok, err := f.srv.store.CreateCredential(ctx, "system", f.bob.ID, "short", &exp)
	if err != nil {
		t.Fatal(err)
	}
	expiring := f.create(t, tok, humanBody)
	keeper := f.create(t, f.aliceTok, humanBody)

	sub := f.srv.events.subscribe(f.general.ID, f.alice.ID)
	defer sub.cancel()
	clock.Advance(time.Hour - time.Second)
	f.heartbeat(t, tok, expiring.ID)
	f.heartbeat(t, f.aliceTok, keeper.ID)
	f.srv.sweepSessions(ctx)
	if got := f.rosterByID(t, f.rootTok); len(got) != 2 {
		t.Fatalf("before the expiry the roster = %+v, want both sessions", got)
	}
	sub.take() // what the heartbeats owed

	clock.Advance(time.Second)
	f.srv.sweepSessions(ctx)
	got := f.rosterByID(t, f.rootTok)
	if _, ok := got[expiring.ID]; ok || len(got) != 1 {
		t.Fatalf("after the expiry the roster = %+v, want only session %d", got, keeper.ID)
	}
	if rows := f.auditRows(t, store.AuditSessionEnded); len(rows) != 1 || !strings.Contains(rows[0].Detail, "cause=credential_expired") {
		t.Errorf("session_ended rows = %+v, want one with cause credential_expired", rows)
	}
	if roster, _, _ := sub.take(); !roster {
		t.Error("the sweep that ended a session did not notify rosters")
	}
	f.srv.sweepSessions(ctx)
	if rows := f.auditRows(t, store.AuditSessionEnded); len(rows) != 1 {
		t.Errorf("a second sweep wrote more: %+v", rows)
	}
	if roster, _, _ := sub.take(); roster {
		t.Error("a sweep that changed nothing notified rosters")
	}
}
