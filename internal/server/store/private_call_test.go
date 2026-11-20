package store

import (
	"context"
	"fmt"
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
	}
	for i, person := range []PrivateCallActor{f.a, f.c} {
		if err := s.BindPrivateCallConversation(ctx, int64(i+1), person.PrincipalID, f.b.PrincipalID); err != nil {
			t.Fatal(err)
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
	for _, id := range []string{"accept", "other-id"} {
		if retry := f.act(call, f.b, schema.PrivateCallEventAccept, id); !retry.Replayed || retry.Call.Revision != accepted.Call.Revision {
			t.Fatalf("accept retry %q: %+v", id, retry)
		}
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

func TestPrivateCallHistoryPageWithTiedTimestamps(t *testing.T) {
	f := newPrivateCallFixture(t, openTestStore(t))
	ctx := context.Background()
	seen := make(map[string]bool, schema.MaxPrivateCallHistoryPage+1)
	for i := 0; i <= schema.MaxPrivateCallHistoryPage; i++ {
		call := f.invite(1, f.a, fmt.Sprintf("invite%d", i)).Call
		if got := f.act(call, f.b, schema.PrivateCallEventDecline, fmt.Sprintf("decline%d", i)); got.Call.State != schema.PrivateCallStateDeclined {
			t.Fatalf("decline: %+v", got)
		}
		seen[call.ID] = false
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
