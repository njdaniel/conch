package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

type privateCallFixture struct {
	t           *testing.T
	s           *Store
	a, b, b2, c PrivateCallActor
	now         time.Time
}

func newPrivateCallFixture(t *testing.T, s *Store) *privateCallFixture {
	t.Helper()
	ctx := context.Background()
	f := &privateCallFixture{t: t, s: s, now: time.Now().UTC().Truncate(time.Millisecond)}
	for i, person := range []*PrivateCallActor{&f.a, &f.b, &f.b2, &f.c} {
		if person == &f.b2 {
			person.PrincipalID = f.b.PrincipalID
		} else {
			p, err := s.CreatePrincipal(ctx, PrincipalHuman, fmt.Sprintf("caller%d", i))
			if err != nil {
				t.Fatal(err)
			}
			person.PrincipalID = p.ID
		}
		cred, _, err := s.CreateCredential(ctx, "system", person.PrincipalID, fmt.Sprintf("phone%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		person.CredentialID = cred.ID
		// CreateCredential("system", ...) records a credential made by
		// another; these stand in for sign-ins the people made themselves.
		if _, err := s.db.ExecContext(ctx, "UPDATE credentials SET self_made = 1 WHERE id = ?", cred.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Conversations 1 (a and b) and 2 (c and b) are the DM store's own, each
	// opened by both people, so each sees it.
	for i, person := range []PrivateCallActor{f.a, f.c} {
		lo, hi := min(person.PrincipalID, f.b.PrincipalID), max(person.PrincipalID, f.b.PrincipalID)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO dm_conversations (id, lo, hi, created_at) VALUES (?, ?, ?, ?)`,
			i+1, lo, hi, f.now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{lo, hi} {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO dm_viewer_state (principal_id, conversation_id, state, updated_at)
				VALUES (?, ?, 'open', ?)`, id, i+1, f.now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func (f *privateCallFixture) clock() time.Time { return f.now }

func (f *privateCallFixture) invite(pair int64, actor PrivateCallActor, action string) PrivateCallResult {
	f.t.Helper()
	result, err := f.s.InvitePrivateCall(context.Background(), pair, actor, schema.InvitePrivateCallRequestV1{ActionID: action}, f.clock)
	if err != nil {
		f.t.Error(err)
		return PrivateCallResult{}
	}
	if result.Code == "" {
		if err := result.Validate(); err != nil {
			f.t.Error(err)
		}
	}
	return result
}

func (f *privateCallFixture) act(c schema.PrivateCallV1, actor PrivateCallActor, event, action string) PrivateCallResult {
	f.t.Helper()
	result, err := f.s.ActOnPrivateCall(context.Background(), c.ID, event, actor,
		schema.PrivateCallActionRequestV1{ActionID: action, Revision: c.Revision}, f.clock)
	if err != nil {
		f.t.Error(err)
		return PrivateCallResult{}
	}
	if err := result.Call.Validate(); err != nil {
		f.t.Error(err)
	}
	return result
}

func (f *privateCallFixture) observe(c schema.PrivateCallV1, a, b int64) {
	f.t.Helper()
	if err := f.s.ObservePrivateCallEndpoints(context.Background(), c.ID, c.Revision, a, b, f.clock); err != nil {
		f.t.Fatal(err)
	}
}

func (f *privateCallFixture) sweep() {
	f.t.Helper()
	if err := f.s.SweepPrivateCalls(context.Background(), f.clock); err != nil {
		f.t.Fatal(err)
	}
}

func (f *privateCallFixture) stored(id string) privateCall {
	f.t.Helper()
	c, err := privateCallByID(context.Background(), f.s.db, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestPrivateCallTransitions(t *testing.T) {
	for _, row := range schema.PrivateCallTransitions {
		t.Run(row.From+"/"+row.Event, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			ctx := context.Background()
			voiceOff := func(actor PrivateCallActor) {
				t.Helper()
				if _, err := f.s.SetVoiceAllowed(ctx, "system", 0, actor.PrincipalID, false); err != nil {
					t.Fatal(err)
				}
			}
			if row.Event == schema.PrivateCallEventInviteUnavailable {
				voiceOff(f.b)
			}
			c := f.invite(1, f.a, "invite").Call
			wantRevision := int64(1)
			if row.From != schema.PrivateCallFromNone {
				if row.From == string(schema.PrivateCallStateAccepted) || row.From == string(schema.PrivateCallStateActive) {
					c = f.act(c, f.b, schema.PrivateCallEventAccept, "setup-accept").Call
				}
				if row.From == string(schema.PrivateCallStateActive) {
					f.observe(c, f.a.CredentialID, f.b.CredentialID)
					c = f.stored(c.ID).view(f.a)
				}
				wantRevision = c.Revision + 1
			}
			switch row.Actor {
			case schema.PrivateCallActorCallee:
				f.act(c, f.b, row.Event, "command")
			case schema.PrivateCallActorCaller, schema.PrivateCallActorEither:
				if row.From != schema.PrivateCallFromNone {
					f.act(c, f.a, row.Event, "command")
				}
			case schema.PrivateCallActorServer:
				switch row.Event {
				case schema.PrivateCallEventRingExpired:
					f.now = c.RingDeadline.Time()
					f.sweep()
				case schema.PrivateCallEventJoinExpired:
					f.now = c.JoinDeadline.Time()
					f.sweep()
				case schema.PrivateCallEventJoined:
					f.observe(c, f.a.CredentialID, f.b.CredentialID)
				case schema.PrivateCallEventGraceExpired:
					f.observe(c, 0, f.b.CredentialID)
					f.now = f.now.Add(schema.PrivateCallReconnectGraceSeconds * time.Second)
					f.sweep()
				case schema.PrivateCallEventRevoked:
					voiceOff(f.b)
					f.sweep()
				case schema.PrivateCallEventRestart:
					if err := f.s.RestartPrivateCalls(ctx, f.now); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unhandled server event %q", row.Event)
				}
			}
			got := f.stored(c.ID).view(f.a)
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			if got.State != row.To || got.Reason != row.Reason || got.Revision != wantRevision {
				t.Fatalf("got %s/%s at revision %d, want %s/%s at revision %d", got.State, got.Reason, got.Revision, row.To, row.Reason, wantRevision)
			}
			var holders int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM private_call_holders WHERE call_id = ?`, c.ID).Scan(&holders); err != nil {
				t.Fatal(err)
			}
			wantHolders := 2
			if row.To.Terminal() {
				wantHolders = 0
			}
			if holders != wantHolders {
				t.Fatalf("holders = %d, want %d", holders, wantHolders)
			}
		})
	}
}

func TestPrivateCallReconnectGrace(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	c := f.invite(1, f.a, "invite").Call
	c = f.act(c, f.b, schema.PrivateCallEventAccept, "accept").Call
	f.observe(c, f.a.CredentialID, f.b2.CredentialID)
	if got := f.stored(c.ID); got.State != schema.PrivateCallStateAccepted {
		t.Fatalf("wrong device activated call: %s", got.State)
	}
	f.observe(c, f.a.CredentialID, f.b.CredentialID)
	c = f.stored(c.ID).view(f.a)
	f.observe(c, f.a.CredentialID, 0)
	f.now = f.now.Add((schema.PrivateCallReconnectGraceSeconds - 1) * time.Second)
	f.observe(c, f.a.CredentialID, f.b.CredentialID)
	f.now = f.now.Add(2 * time.Second)
	f.sweep()
	if got := f.stored(c.ID); got.State != schema.PrivateCallStateActive || got.Revision != c.Revision {
		t.Fatalf("rejoined call: %s revision %d", got.State, got.Revision)
	}
	f.observe(c, f.a.CredentialID, 0)
	f.now = f.now.Add((schema.PrivateCallReconnectGraceSeconds - 1) * time.Second)
	f.observe(c, f.a.CredentialID, 0)
	f.now = f.now.Add(time.Second)
	f.observe(c, f.a.CredentialID, f.b.CredentialID)
	if got := f.stored(c.ID); got.State != schema.PrivateCallStateEnded || got.Reason != schema.PrivateCallReasonGone {
		t.Fatalf("late rejoin: %s/%s", got.State, got.Reason)
	}
	if next := f.invite(1, f.b, "fresh"); !next.Created || next.Call.State != schema.PrivateCallStateRinging || next.Call.ID == c.ID {
		t.Fatalf("fresh call after endpoint loss: %+v", next)
	}
}

func TestPrivateCallConcurrentDuplicateAndCompetingInvite(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	results := make([]PrivateCallResult, 3)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, actor := range []PrivateCallActor{f.a, f.a, f.c} {
		wg.Go(func() {
			<-start
			pair := int64(1)
			if actor == f.c {
				pair = 2
			}
			results[i] = f.invite(pair, actor, "invite")
		})
	}
	close(start)
	wg.Wait()
	a, duplicate, other := results[0], results[1], results[2]
	if a.Call.ID == "" || a.Call.ID != duplicate.Call.ID || a.Created == duplicate.Created || a.Replayed == duplicate.Replayed {
		t.Fatalf("duplicate command outcomes: %+v, %+v", a, duplicate)
	}
	if !other.Created || a.Call.State == other.Call.State ||
		(a.Call.State != schema.PrivateCallStateRinging && other.Call.State != schema.PrivateCallStateRinging) {
		t.Fatalf("competing calls: %+v", results)
	}
}

func TestPrivateCallRetryWindowAndAcceptedHolder(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	call := f.invite(1, f.a, "invite").Call
	accepted := f.act(call, f.b, schema.PrivateCallEventAccept, "accept")
	if accepted.Call.State != schema.PrivateCallStateAccepted || !accepted.Call.SelectedHere {
		t.Fatalf("accept: %+v", accepted)
	}
	if retry := f.act(call, f.b, schema.PrivateCallEventAccept, "accept"); !retry.Replayed || retry.Call.Revision != accepted.Call.Revision {
		t.Fatalf("accept retry: %+v", retry)
	}
	if stale := f.act(call, f.b, schema.PrivateCallEventAccept, "other-id"); stale.Code != schema.ErrorCodeCallStateChanged {
		t.Fatalf("accept with a fresh action id and stale revision: %+v", stale)
	}
	if conflict, err := f.s.ActOnPrivateCall(context.Background(), call.ID, schema.PrivateCallEventAccept, f.b,
		schema.PrivateCallActionRequestV1{ActionID: "accept", Revision: accepted.Call.Revision}, f.clock); err != nil || conflict.Code != schema.ErrorCodeCallActionConflict {
		t.Fatalf("accept with a reused action id and different revision: %+v, %v", conflict, err)
	}
	if wrong := f.act(call, f.b2, schema.PrivateCallEventAccept, "accept"); wrong.Code != schema.ErrorCodeCallStateChanged || wrong.Call.SelectedHere {
		t.Fatalf("accept retry on second device: %+v", wrong)
	}
	if busy := f.invite(2, f.c, "busy"); busy.Call.State != schema.PrivateCallStateUnavailable {
		t.Fatalf("accepted callee is not reserved: %+v", busy)
	}
	if busy := f.invite(2, f.b, "from-busy"); busy.Code != schema.ErrorCodeCallInProgress {
		t.Fatalf("accepted caller can invite again: %+v", busy)
	}
	f.now = f.now.Add(schema.PrivateCallActionWindowSeconds * time.Second)
	f.sweep()
	next := f.invite(1, f.a, "invite")
	if !next.Created || next.Call.ID == call.ID || next.Call.State != schema.PrivateCallStateRinging {
		t.Fatalf("invite id reuse at expiry: %+v", next)
	}
}

func TestPrivateCallRefusalDoesNotUseActionID(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	call := f.invite(1, f.a, "invite").Call
	if out := f.act(call, f.b, schema.PrivateCallEventHangup, "hangup"); out.Code != schema.ErrorCodeCallStateChanged {
		t.Fatalf("hangup before accept: %+v", out)
	}
	accepted := f.act(call, f.b, schema.PrivateCallEventAccept, "accept").Call
	if out := f.act(accepted, f.b, schema.PrivateCallEventHangup, "hangup"); out.Code != "" || out.Call.State != schema.PrivateCallStateEnded {
		t.Fatalf("hangup after accept with the same action id: %+v", out)
	}
}

func TestPrivateCallReceiptsArePerCredential(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	call := f.invite(1, f.a, "invite").Call
	accepted := f.act(call, f.b, schema.PrivateCallEventAccept, "device-action").Call
	ended := f.act(accepted, f.b2, schema.PrivateCallEventHangup, "device-action")
	if ended.Code != "" || ended.Call.State != schema.PrivateCallStateEnded || ended.Replayed {
		t.Fatalf("second credential using the same action id: %+v", ended)
	}
	if replay := f.act(call, f.b, schema.PrivateCallEventAccept, "device-action"); !replay.Replayed || replay.Call.ID != ended.Call.ID || replay.Call.State != ended.Call.State {
		t.Fatalf("first credential's receipt: %+v", replay)
	}
}

func TestPrivateCallSelectedCredentialRevocation(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	ctx := context.Background()
	call := f.invite(1, f.a, "invite").Call
	accepted := f.act(call, f.b, schema.PrivateCallEventAccept, "accept").Call
	if err := f.s.RevokeCredential(ctx, "system", f.b2.CredentialID); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if got := f.stored(call.ID); got.State != accepted.State || got.Revision != accepted.Revision {
		t.Fatalf("revoked unselected credential changed the call: %s revision %d", got.State, got.Revision)
	}
	if err := f.s.RevokeCredential(ctx, "system", f.b.CredentialID); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	ended := f.stored(call.ID)
	if ended.State != schema.PrivateCallStateEnded || ended.Reason != "" || ended.Revision != accepted.Revision+1 {
		t.Fatalf("revoked selected credential: %s/%s revision %d", ended.State, ended.Reason, ended.Revision)
	}
}

func TestPrivateCallRepeatLimitCountsUndeliveredAndExpiredInvites(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	ctx := context.Background()
	if _, err := f.s.db.ExecContext(ctx, "INSERT INTO blocks VALUES (?, ?, ?)",
		f.b.PrincipalID, f.a.PrincipalID, f.now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out := f.invite(1, f.a, fmt.Sprintf("unavailable%d", i))
		if out.Call.State != schema.PrivateCallStateUnavailable {
			t.Fatalf("blocked invite: %+v", out)
		}
	}
	if _, err := f.s.db.ExecContext(ctx, "DELETE FROM blocks"); err != nil {
		t.Fatal(err)
	}
	ring := f.invite(1, f.a, "ring").Call
	if ring.State != schema.PrivateCallStateRinging {
		t.Fatalf("third invite: %+v", ring)
	}
	f.now = ring.RingDeadline.Time()
	limited := f.invite(1, f.a, "fourth")
	if limited.Code != schema.ErrorCodeRateLimited || limited.Call.ID != "" || limited.RetryAfter != 10*time.Minute-schema.PrivateCallRingSeconds*time.Second {
		t.Fatalf("fourth invite: %+v", limited)
	}
	if got := f.stored(ring.ID); got.State != schema.PrivateCallStateMissed {
		t.Fatalf("third invite after expiry: %+v", got)
	}
}

// ringAndCancel is one probe cycle: the caller rings, then cancels at once.
func (f *privateCallFixture) ringAndCancel(action string) PrivateCallResult {
	f.t.Helper()
	out := f.invite(1, f.a, "invite-"+action)
	if out.Code != "" {
		return out
	}
	if out.Call.State != schema.PrivateCallStateRinging {
		f.t.Fatalf("invite %s: %+v", action, out)
	}
	if got := f.act(out.Call, f.a, schema.PrivateCallEventCancel, "cancel-"+action); got.Call.State != schema.PrivateCallStateCancelled {
		f.t.Fatalf("cancel %s: %+v", action, got)
	}
	return out
}

// ringAndRevoke rings as caller, lets revoke end the ring by a revocation on
// the next sweep, and checks it is stored cancelled by "revoked".
func (f *privateCallFixture) ringAndRevoke(caller PrivateCallActor, action string, revoke func()) PrivateCallResult {
	f.t.Helper()
	out := f.invite(1, caller, "invite-"+action)
	if out.Code != "" {
		return out
	}
	if out.Call.State != schema.PrivateCallStateRinging {
		f.t.Fatalf("invite %s: %+v", action, out)
	}
	revoke()
	f.sweep()
	got := f.stored(out.Call.ID)
	if got.State != schema.PrivateCallStateCancelled || got.lastEvent != schema.PrivateCallEventRevoked {
		f.t.Fatalf("revoked ring %s: %s by %q", action, got.State, got.lastEvent)
	}
	return out
}

func TestPrivateCallRepeatLimitCountsCallerCancelledRings(t *testing.T) {
	// The security review's probe of #2058: ring, cancel, wait 20 s, again.
	f := newPrivateCallFixture(t, openTestStore(t))
	start := f.now
	const window = 10 * time.Minute
	for i := 0; i < 12; i++ {
		out := f.ringAndCancel(fmt.Sprint(i))
		if i < 3 {
			if !out.Created || out.Code != "" {
				t.Fatalf("cycle %d within the limit: %+v", i, out)
			}
		} else {
			// The third most recent counted attempt (cycle 0) ended at start.
			want := window - f.now.Sub(start)
			if out.Code != schema.ErrorCodeRateLimited || out.Call.ID != "" || out.RetryAfter != want {
				t.Fatalf("cycle %d past the limit: %+v, want rate_limited after %s", i, out, want)
			}
		}
		f.now = f.now.Add(20 * time.Second)
	}
	// Once the oldest counted cancel leaves the window the caller may ring again.
	f.now = start.Add(window + time.Millisecond)
	if out := f.invite(1, f.a, "after-window"); !out.Created || out.Call.State != schema.PrivateCallStateRinging {
		t.Fatalf("invite after the window: %+v", out)
	}
}

// TestPrivateCallRepeatLimitCountsRevokedRings: a ring cancelled by a
// revocation counts toward the repeat limit whoever caused the revocation.
// The caller can cause one at will, by blocking and unblocking the callee or
// signing out the credential that rang, and the stored attempt does not say
// who did (the security review of #2156 measured 90 of 90 such rings
// answered 201 in an hour when they were exempt).
func TestPrivateCallRepeatLimitCountsRevokedRings(t *testing.T) {
	ctx := context.Background()
	block := func(f *privateCallFixture, owner, blocked int64) func() {
		return func() {
			if _, err := f.s.db.ExecContext(ctx, "INSERT INTO blocks VALUES (?, ?, ?)", owner, blocked, f.now.UnixMilli()); err != nil {
				f.t.Fatal(err)
			}
			f.sweep()
			if _, err := f.s.db.ExecContext(ctx, "DELETE FROM blocks"); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name string
		// ring returns the caller for cycle i and the revocation to apply.
		ring func(f *privateCallFixture, i int) (PrivateCallActor, func())
	}{
		{"the caller blocks and unblocks the callee", func(f *privateCallFixture, _ int) (PrivateCallActor, func()) {
			return f.a, block(f, f.a.PrincipalID, f.b.PrincipalID)
		}},
		{"the callee blocks and unblocks the caller", func(f *privateCallFixture, _ int) (PrivateCallActor, func()) {
			return f.a, block(f, f.b.PrincipalID, f.a.PrincipalID)
		}},
		{"the caller signs out the ringing credential", func(f *privateCallFixture, i int) (PrivateCallActor, func()) {
			// A fresh sign-in of the caller's for each ring.
			cred, _, err := f.s.CreateCredential(ctx, "system", f.a.PrincipalID, fmt.Sprintf("signin%d", i), nil)
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.db.ExecContext(ctx, "UPDATE credentials SET self_made = 1 WHERE id = ?", cred.ID); err != nil {
				f.t.Fatal(err)
			}
			caller := PrivateCallActor{PrincipalID: f.a.PrincipalID, CredentialID: cred.ID}
			return caller, func() {
				if err := f.s.RevokeCredential(ctx, "system", cred.ID); err != nil {
					f.t.Fatal(err)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			start := f.now
			const window = 10 * time.Minute
			for i := 0; i < 5; i++ {
				caller, revoke := tt.ring(f, i)
				out := f.ringAndRevoke(caller, fmt.Sprint(i), revoke)
				if i < 3 {
					if !out.Created || out.Code != "" {
						t.Fatalf("ring %d within the limit: %+v", i, out)
					}
				} else if want := window - f.now.Sub(start); out.Code != schema.ErrorCodeRateLimited || out.Call.ID != "" || out.RetryAfter != want {
					t.Fatalf("ring %d past the limit: %+v, want rate_limited after %s", i, out, want)
				}
				f.now = f.now.Add(20 * time.Second)
			}
		})
	}
}

func TestPrivateCallRepeatLimitCountsOutcomesTogether(t *testing.T) {
	outcomes := map[string]func(f *privateCallFixture, n string){
		"cancel": func(f *privateCallFixture, n string) { f.ringAndCancel(n) },
		"decline": func(f *privateCallFixture, n string) {
			call := f.invite(1, f.a, "invite-"+n).Call
			if got := f.act(call, f.b, schema.PrivateCallEventDecline, "decline-"+n); got.Call.State != schema.PrivateCallStateDeclined {
				f.t.Fatalf("decline: %+v", got)
			}
		},
		"missed": func(f *privateCallFixture, n string) {
			call := f.invite(1, f.a, "invite-"+n).Call
			f.now = call.RingDeadline.Time()
			f.sweep()
			if got := f.stored(call.ID); got.State != schema.PrivateCallStateMissed {
				f.t.Fatalf("missed: %s", got.State)
			}
		},
		"unavailable": func(f *privateCallFixture, n string) {
			ctx := context.Background()
			if _, err := f.s.db.ExecContext(ctx, "INSERT INTO blocks VALUES (?, ?, ?)",
				f.b.PrincipalID, f.a.PrincipalID, f.now.UnixMilli()); err != nil {
				f.t.Fatal(err)
			}
			if out := f.invite(1, f.a, "invite-"+n); out.Call.State != schema.PrivateCallStateUnavailable {
				f.t.Fatalf("unavailable: %+v", out)
			}
			if _, err := f.s.db.ExecContext(ctx, "DELETE FROM blocks"); err != nil {
				f.t.Fatal(err)
			}
		},
	}
	for _, tc := range []struct {
		name  string
		steps []string
		limit bool
	}{
		{"three cancels", []string{"cancel", "cancel", "cancel"}, true},
		{"two cancels", []string{"cancel", "cancel"}, false},
		{"three declines", []string{"decline", "decline", "decline"}, true},
		{"three missed", []string{"missed", "missed", "missed"}, true},
		{"three unavailable", []string{"unavailable", "unavailable", "unavailable"}, true},
		{"cancel decline missed", []string{"cancel", "decline", "missed"}, true},
		{"unavailable cancel decline", []string{"unavailable", "cancel", "decline"}, true},
		{"missed cancel unavailable", []string{"missed", "cancel", "unavailable"}, true},
		{"two mixed", []string{"decline", "cancel"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			var first time.Time
			for i, step := range tc.steps {
				outcomes[step](f, fmt.Sprint(i))
				if i == 0 {
					first = f.now
				}
				f.now = f.now.Add(time.Second)
			}
			out := f.invite(1, f.a, "next")
			if !tc.limit {
				if !out.Created || out.Code != "" {
					t.Fatalf("invite below the limit: %+v", out)
				}
				return
			}
			want := 10*time.Minute - f.now.Sub(first)
			if out.Code != schema.ErrorCodeRateLimited || out.Call.ID != "" || out.RetryAfter != want {
				t.Fatalf("invite at the limit: %+v, want rate_limited after %s", out, want)
			}
		})
	}
}

func TestPrivateCallRepeatLimitSkipsRetriesOfRecordedInvite(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	var last PrivateCallResult
	for i := 0; i < 3; i++ {
		last = f.ringAndCancel(fmt.Sprint(i))
		f.now = f.now.Add(time.Second)
	}
	if limited := f.invite(1, f.a, "new-action"); limited.Code != schema.ErrorCodeRateLimited {
		t.Fatalf("a new invite at the limit: %+v", limited)
	}
	retry := f.invite(1, f.a, "invite-2")
	if retry.Code != "" || !retry.Replayed || retry.Call.ID != last.Call.ID {
		t.Fatalf("retry of a recorded invite: %+v, want replay of %s", retry, last.Call.ID)
	}
}

func TestPrivateCallReconcileAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := newTestDBPath(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := newPrivateCallFixture(t, s)
	ring := f.invite(1, f.a, "invite").Call
	accepted := f.act(ring, f.b, schema.PrivateCallEventAccept, "accept").Call
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = openTestStoreAt(t, path)
	f.now = accepted.JoinDeadline.Time().Add(time.Minute)
	if err := f.s.RestartPrivateCalls(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(ring.ID); got.State != schema.PrivateCallStateFailed ||
		got.Reason != schema.PrivateCallReasonServerRestart || got.Revision != accepted.Revision+1 {
		t.Fatalf("restart: %s/%s revision %d", got.State, got.Reason, got.Revision)
	}
	if next := f.invite(1, f.b, "next"); next.Call.State != schema.PrivateCallStateRinging {
		t.Fatalf("holders survived restart: %+v", next)
	}
}

func TestPrivateCallSweepIsolatesMalformedCall(t *testing.T) {
	ctx := context.Background()
	f := newPrivateCallFixture(t, openTestStore(t))
	d, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "fourth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO dm_conversations (id, lo, hi, created_at) VALUES (3, ?, ?, ?)`,
		min(f.c.PrincipalID, d.ID), max(f.c.PrincipalID, d.ID), f.now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	bad := f.invite(1, f.a, "bad").Call
	var good privateCall
	if err := f.s.withImmediateTx(ctx, func(tx execer) error {
		good, err = createPrivateCall(ctx, tx, 3, f.c, d.ID, schema.PrivateCallEventInvite, f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE private_calls SET deadline = deadline - 1 WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}
	f.now = bad.RingDeadline.Time()
	f.sweep()
	if got := f.stored(good.ID); got.State != schema.PrivateCallStateMissed || got.Revision != 2 {
		t.Fatalf("unrelated call after sweep: %+v", got)
	}
	if got := f.stored(bad.ID); got.State != schema.PrivateCallStateRinging || got.Revision != 1 {
		t.Fatalf("malformed call after sweep: %+v", got)
	}
	if err := f.s.RestartPrivateCalls(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(bad.ID); got.State != schema.PrivateCallStateFailed || got.Revision != 2 || got.view(f.a).Validate() != nil {
		t.Fatalf("malformed call after restart: %+v", got)
	}
	if _, err := privateCallHeldBy(ctx, f.s.db, f.a.PrincipalID); !errors.Is(err, ErrPrivateCallNotFound) {
		t.Fatalf("holder after restart: %v", err)
	}
}

func TestInvalidateRestoredPrivateCalls(t *testing.T) {
	for _, state := range []schema.PrivateCallState{
		schema.PrivateCallStateRinging, schema.PrivateCallStateAccepted, schema.PrivateCallStateActive,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			f := newPrivateCallFixture(t, openTestStore(t))
			earlier := f.invite(1, f.a, "earlier").Call
			declined := f.act(earlier, f.b, schema.PrivateCallEventDecline, "decline").Call
			call := f.invite(1, f.a, "invite").Call
			if state != schema.PrivateCallStateRinging {
				call = f.act(call, f.b, schema.PrivateCallEventAccept, "accept").Call
			}
			if state == schema.PrivateCallStateActive {
				f.observe(call, f.a.CredentialID, f.b.CredentialID)
				call = f.stored(call.ID).view(f.a)
			}
			if call.State != state {
				t.Fatalf("setup state = %s, want %s", call.State, state)
			}

			// A failure at the final audit write must roll back the call and
			// its holders along with the rest of the restore.
			if _, err := f.s.db.ExecContext(ctx, `CREATE TRIGGER fail_restore BEFORE INSERT ON audit_events
				WHEN NEW.action = 'restore_invalidated' BEGIN SELECT RAISE(ABORT, 'restore test'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.InvalidateRestored(ctx, f.now); err == nil {
				t.Fatal("restore succeeded with an injected audit failure")
			}
			if got := f.stored(call.ID); got.State != state || got.Revision != call.Revision || got.endedAt.Valid {
				t.Fatalf("restore failure changed the call: %+v", got)
			}
			if held, err := privateCallHeldBy(ctx, f.s.db, f.a.PrincipalID); err != nil || held.ID != call.ID {
				t.Fatalf("restore failure removed the caller's holder: %q, %v", held.ID, err)
			}
			if _, err := f.s.db.ExecContext(ctx, "DROP TRIGGER fail_restore"); err != nil {
				t.Fatal(err)
			}
			if state == schema.PrivateCallStateRinging {
				if _, err := f.s.db.Exec(`UPDATE private_calls SET deadline = deadline - 1 WHERE id = ?`, call.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.s.InvalidateRestored(ctx, f.now); err != nil {
				t.Fatal(err)
			}
			got := f.stored(call.ID)
			if got.State != schema.PrivateCallStateFailed || got.Reason != privateCallReasonRestore ||
				got.Revision != call.Revision+1 || got.deadline.Valid ||
				!got.endedAt.Valid || got.endedAt.Int64 != f.now.UnixMilli() {
				t.Fatalf("restored call: %+v", got)
			}
			var holders int
			if err := f.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM private_call_holders WHERE call_id = ?", call.ID).Scan(&holders); err != nil || holders != 0 {
				t.Fatalf("holders after restore = %d, err = %v", holders, err)
			}
			if view := got.view(f.a); view.Reason != schema.PrivateCallReasonServerRestart || view.Validate() != nil {
				t.Fatalf("restore projection: %+v", view)
			}
			if got := f.stored(declined.ID); got.State != declined.State || got.Revision != declined.Revision {
				t.Fatalf("restore changed a terminal call: %+v", got)
			}
		})
	}
}

func TestPrivateCallMigrationGuardsRows(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	// The pair is stored in its canonical order regardless of who called.
	call := f.invite(1, f.b, "invite").Call
	if call.State != schema.PrivateCallStateRinging {
		t.Fatalf("invite with reversed pair: %+v", call)
	}
	for _, tc := range []struct {
		name, statement, wantError string
		args                       []any
	}{
		{"mismatched pair on insert", `INSERT INTO private_calls
			(id, conversation_id, caller, callee, caller_credential, state, last_event, revision, created_at)
			SELECT 'invalid-attempt-id', 2, caller, callee, caller_credential, state, last_event, revision, created_at
			FROM private_calls WHERE id = ?`, "conversation participants", []any{call.ID}},
		{"mismatched pair on update", `UPDATE private_calls SET conversation_id = 2 WHERE id = ?`, "cannot change its conversation or participants", []any{call.ID}},
		{"unknown state", `UPDATE private_calls SET state = 'unknown' WHERE id = ?`, "CHECK constraint failed", []any{call.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.s.db.Exec(tc.statement, tc.args...); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("database error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestPrivateCallMigrationRejectsLeftoverTable(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.db.Exec(`PRAGMA user_version = 54`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "table private_calls already exists") {
		t.Fatalf("migration 55 with a leftover table: %v", err)
	}
}

func TestPrivateCallHistoryPageWithTiedTimestamps(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	ctx := context.Background()
	seen := make(map[string]bool, schema.MaxPrivateCallHistoryPage+1)
	for i := 0; i <= schema.MaxPrivateCallHistoryPage; i++ {
		// Caller-cancelled attempts count toward the repeat-invite limit
		// (#2112), so a hundred of them at one instant cannot be made through
		// InvitePrivateCall. Build the cancelled rows directly: the page
		// walk below needs only their tied created_at.
		id := fmt.Sprintf("tied-attempt-%03d", i)
		if _, err := f.s.db.ExecContext(ctx, `INSERT INTO private_calls
			(id, conversation_id, caller, callee, caller_credential, state, last_event, revision, created_at, ended_at)
			VALUES (?, 1, ?, ?, ?, 'cancelled', 'cancel', 2, ?, ?)`,
			id, f.a.PrincipalID, f.b.PrincipalID, f.a.CredentialID, f.now.UnixMilli(), f.now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		seen[id] = false
	}
	cursor := ""
	for i, count := range []int{schema.MaxPrivateCallHistoryPage, 1} {
		page, err := f.s.ListPrivateCalls(ctx, 1, f.b, cursor, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if err := page.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(page.Calls) != count || (page.NextCursor == "") != (i == 1) {
			t.Fatalf("page %d: %d calls, cursor %q", i, len(page.Calls), page.NextCursor)
		}
		for _, call := range page.Calls {
			if visited, ok := seen[call.ID]; !ok || visited {
				t.Fatalf("unexpected or duplicate history entry %q", call.ID)
			}
			seen[call.ID] = true
		}
		cursor = page.NextCursor
	}
}
