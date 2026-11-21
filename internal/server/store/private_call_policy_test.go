package store

import (
	"context"
	"errors"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// A call invite is new contact (ADR-017 Decision 3, issue #2030): it consults
// the contact policy of dm_policy.go inside its own transaction. Alice (f.a)
// invites bob (f.b) in conversation 1 unless a case says otherwise.

func (f *privateCallFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestPrivateCallInviteConsultsTheDMPolicy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, f *privateCallFixture)
		pair      int64
		actor     func(f *privateCallFixture) PrivateCallActor
		wantErr   error
		wantState schema.PrivateCallState // when no error
	}{
		{name: "everything allows it", wantState: schema.PrivateCallStateRinging},
		{name: "the server's switch is off", wantErr: ErrDMsOff, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMServerSettings(ctx, "system", 0, schema.DMServerSettingsV1{DMsEnabled: false}, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the caller's own switch is off", wantErr: ErrDMsOff, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMAllowed(ctx, "system", 0, f.a.PrincipalID, false, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a conversation never issued", pair: 99, wantErr: ErrPrivateCallPair},
		{name: "a conversation the caller is not in", pair: 2, wantErr: ErrPrivateCallPair},
		{name: "the peer is hidden: the same not-found", wantErr: ErrPrivateCallPair, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.db.Exec("UPDATE principals SET hidden_at = ?1, disabled_at = ?1 WHERE id = ?2", f.now.UnixMilli(), f.b.PrincipalID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the callee's switch is off: unavailable", wantState: schema.PrivateCallStateUnavailable, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMAllowed(ctx, "system", 0, f.b.PrincipalID, false, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the callee accepts nobody: unavailable", wantState: schema.PrivateCallStateUnavailable, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMPreferences(ctx, f.b.PrincipalID, schema.DMPreferencesV1{Accept: schema.DMAcceptNobody}, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the callee accepts shared channels only and there is none: unavailable", wantState: schema.PrivateCallStateUnavailable, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMPreferences(ctx, f.b.PrincipalID, schema.DMPreferencesV1{Accept: schema.DMAcceptSharedChannel}, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the callee blocked the caller: unavailable", wantState: schema.PrivateCallStateUnavailable, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.db.Exec("INSERT INTO blocks VALUES (?, ?, ?)", f.b.PrincipalID, f.a.PrincipalID, f.now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the caller blocked the callee: unavailable", wantState: schema.PrivateCallStateUnavailable, setup: func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.db.Exec("INSERT INTO blocks VALUES (?, ?, ?)", f.a.PrincipalID, f.b.PrincipalID, f.now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			if tc.setup != nil {
				tc.setup(t, f)
			}
			// Bob has not seen the conversation: only a ring may show it to him.
			if _, err := f.s.db.Exec("DELETE FROM dm_viewer_state WHERE principal_id = ? AND conversation_id = 1", f.b.PrincipalID); err != nil {
				t.Fatal(err)
			}
			pair := tc.pair
			if pair == 0 {
				pair = 1
			}
			out, err := f.s.InvitePrivateCall(ctx, pair, f.a, schema.InvitePrivateCallRequestV1{ActionID: "invite"}, f.clock)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if n := f.count("SELECT COUNT(*) FROM private_calls"); n != 0 {
					t.Fatalf("a refused invite stored %d attempts", n)
				}
				if n := f.count("SELECT COUNT(*) FROM private_call_actions"); n != 0 {
					t.Fatalf("a refused invite stored %d receipts", n)
				}
				return
			}
			if err != nil || out.Code != "" || out.Call.State != tc.wantState {
				t.Fatalf("invite = %+v, %v; want state %s", out, err, tc.wantState)
			}
			// Only an invite that rings shows the conversation to the callee.
			seen := f.count("SELECT COUNT(*) FROM dm_viewer_state WHERE principal_id = ? AND conversation_id = 1", f.b.PrincipalID)
			if want := map[bool]int{true: 1, false: 0}[tc.wantState == schema.PrivateCallStateRinging]; seen != want {
				t.Fatalf("callee's view rows = %d, want %d", seen, want)
			}
		})
	}
}

func TestPrivateCallAvailabilityRunsTheSameStatementsInEveryClass(t *testing.T) {
	ctx := context.Background()
	want := -1
	for _, reason := range []string{"available", "policy", "voice off", "signed out", "act-as only", "busy"} {
		t.Run(reason, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			var err error
			switch reason {
			case "policy":
				_, err = f.s.SetDMPreferences(ctx, f.b.PrincipalID, schema.DMPreferencesV1{Accept: schema.DMAcceptNobody}, f.now)
			case "voice off":
				_, err = f.s.SetVoiceAllowed(ctx, "system", 0, f.b.PrincipalID, false)
			case "signed out":
				_, err = f.s.db.Exec(`UPDATE credentials SET revoked_at = ? WHERE principal_id = ?`, f.now.UnixMilli(), f.b.PrincipalID)
			case "act-as only":
				_, err = f.s.db.Exec(`UPDATE credentials SET self_made = 0 WHERE principal_id = ?`, f.b.PrincipalID)
			case "busy":
				if call := f.invite(2, f.c, "busy").Call; call.State != schema.PrivateCallStateRinging {
					t.Fatalf("setup call = %+v", call)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.withImmediateTx(ctx, func(tx execer) error {
				q := &countingExecer{execer: tx}
				cmd := privateCallCommand{actor: f.a, now: f.now}
				available, err := cmd.calleeAvailable(ctx, q, f.b.PrincipalID)
				if available != (reason == "available") || err != nil {
					t.Errorf("available = %v, err = %v", available, err)
				}
				if want < 0 {
					want = q.n
				}
				if q.n != want {
					t.Errorf("ran %d statements, eligible callee ran %d", q.n, want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			wantState := schema.PrivateCallStateUnavailable
			if reason == "available" {
				wantState = schema.PrivateCallStateRinging
			}
			if call := f.invite(1, f.a, "invite").Call; call.State != wantState {
				t.Fatalf("invite state = %s, want %s", call.State, wantState)
			}
		})
	}
}

func TestPrivateCallsRecheckRequestCredential(t *testing.T) {
	ctx := context.Background()
	for _, invalid := range []struct {
		name, update string
	}{
		{"revoked", `UPDATE credentials SET revoked_at = created_at WHERE id = ?`},
		{"act-as", `UPDATE credentials SET self_made = 0 WHERE id = ?`},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			earlier := f.invite(1, f.b2, "earlier").Call
			f.act(earlier, f.b2, schema.PrivateCallEventCancel, "cancel")
			call := f.invite(1, f.a, "invite").Call
			// Use an unselected credential for actions and reads so a selected
			// endpoint's revocation cannot mask a missing request check.
			if _, err := f.s.db.Exec(invalid.update, f.b2.CredentialID); err != nil {
				t.Fatal(err)
			}
			if out, err := f.s.InvitePrivateCall(ctx, 1, f.b2, schema.InvitePrivateCallRequestV1{ActionID: "earlier"}, f.clock); !errors.Is(err, ErrPrivateCallPair) {
				t.Errorf("invite replay = %+v, %v; want %v", out, err, ErrPrivateCallPair)
			}
			if out, err := f.s.ActOnPrivateCall(ctx, call.ID, schema.PrivateCallEventAccept, f.b2,
				schema.PrivateCallActionRequestV1{ActionID: "accept", Revision: call.Revision}, f.clock); !errors.Is(err, ErrPrivateCallNotFound) {
				t.Errorf("action = %+v, %v; want %v", out, err, ErrPrivateCallNotFound)
			}
			if out, err := f.s.CurrentPrivateCall(ctx, f.b2, f.clock); !errors.Is(err, ErrPrivateCallNotFound) {
				t.Errorf("current = %+v, %v; want %v", out, err, ErrPrivateCallNotFound)
			}
			if out, err := f.s.ListPrivateCalls(ctx, 1, f.b2, "", f.now); !errors.Is(err, ErrPrivateCallPair) {
				t.Errorf("history = %+v, %v; want %v", out, err, ErrPrivateCallPair)
			}
		})
	}
}

func TestPrivateCallRingPreservesArchiveAndReopensHiddenConversation(t *testing.T) {
	ctx := context.Background()
	for initial, want := range map[schema.DMState]schema.DMState{
		schema.DMStateHidden:   schema.DMStateOpen,
		schema.DMStateArchived: schema.DMStateArchived,
	} {
		t.Run(string(initial), func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			if _, err := f.s.SetDMState(ctx, f.b.PrincipalID, 1, initial, f.now); err != nil {
				t.Fatal(err)
			}
			if call := f.invite(1, f.a, "invite").Call; call.State != schema.PrivateCallStateRinging {
				t.Fatalf("invite = %+v", call)
			}
			var state schema.DMState
			err := f.s.db.QueryRow(`SELECT state FROM dm_viewer_state WHERE principal_id = ? AND conversation_id = 1`, f.b.PrincipalID).Scan(&state)
			if err != nil || state != want {
				t.Fatalf("state = %s, err = %v; want %s", state, err, want)
			}
		})
	}
}

func TestListPrivateCallsExcludesUnavailableAttempts(t *testing.T) {
	ctx := context.Background()
	f := newPrivateCallFixture(t, openTestStore(t))
	ringing := f.invite(1, f.a, "invite").Call
	declined := f.act(ringing, f.b, schema.PrivateCallEventDecline, "decline").Call
	if _, err := f.s.SetVoiceAllowed(ctx, "system", 0, f.b.PrincipalID, false); err != nil {
		t.Fatal(err)
	}
	unavailable := f.invite(1, f.a, "unavailable").Call
	if unavailable.State != schema.PrivateCallStateUnavailable {
		t.Fatalf("invite = %+v", unavailable)
	}
	for _, actor := range []PrivateCallActor{f.a, f.b} {
		history, err := f.s.ListPrivateCalls(ctx, 1, actor, "", f.now)
		if err != nil || len(history.Calls) != 1 || history.Calls[0].ID != declined.ID {
			t.Errorf("history for principal %d = %+v, %v; want only the declined call", actor.PrincipalID, history, err)
		}
		if _, err := f.s.ListPrivateCalls(ctx, 1, actor, unavailable.ID, f.now); !errors.Is(err, ErrPrivateCallCursor) {
			t.Errorf("using unavailable call as cursor: %v, want %v", err, ErrPrivateCallCursor)
		}
	}
}

// A call already ringing ends when the pair stops being reachable (design
// §4.1's "revoked" row), and a preference change alone does not end it: it
// refuses the next invite.
func TestPrivateCallStillAllowedFollowsTheReachabilityRule(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, f *privateCallFixture)
		revoked bool
	}{
		{"a block either way", func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.db.Exec("INSERT INTO blocks VALUES (?, ?, ?)", f.b.PrincipalID, f.a.PrincipalID, f.now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"the server's switch", func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMServerSettings(ctx, "system", 0, schema.DMServerSettingsV1{DMsEnabled: false}, f.now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"the callee's own switch", func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMAllowed(ctx, "system", 0, f.b.PrincipalID, false, f.now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"the callee's preference", func(t *testing.T, f *privateCallFixture) {
			if _, err := f.s.SetDMPreferences(ctx, f.b.PrincipalID, schema.DMPreferencesV1{Accept: schema.DMAcceptNobody}, f.now); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrivateCallFixture(t, openTestStore(t))
			call := f.invite(1, f.a, "invite").Call
			if call.State != schema.PrivateCallStateRinging {
				t.Fatalf("invite: %+v", call)
			}
			tc.change(t, f)
			f.sweep()
			got := f.stored(call.ID)
			if got.State.Terminal() != tc.revoked {
				t.Fatalf("after the change the call is %s/%s, terminal = %v, want %v", got.State, got.Reason, got.State.Terminal(), tc.revoked)
			}
		})
	}
}

// The history of a pair is a read of the pair: the conversation must be
// visible to the reader and its peer known to them, and every other case is
// the not-found of an id never issued.
func TestListPrivateCallsIsAReadOfTheVisiblePair(t *testing.T) {
	ctx := context.Background()
	f := newPrivateCallFixture(t, openTestStore(t))
	if _, err := f.s.ListPrivateCalls(ctx, 1, f.a, "", f.now); err != nil {
		t.Fatalf("a participant: %v", err)
	}
	for name, tc := range map[string]struct {
		pair  int64
		actor PrivateCallActor
	}{
		"a stranger to the pair":      {1, f.c},
		"a conversation never issued": {99, f.a},
	} {
		if _, err := f.s.ListPrivateCalls(ctx, tc.pair, tc.actor, "", f.now); !errors.Is(err, ErrPrivateCallPair) {
			t.Fatalf("%s: err = %v, want ErrPrivateCallPair", name, err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE principals SET hidden_at = ?1, disabled_at = ?1 WHERE id = ?2", f.now.UnixMilli(), f.b.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListPrivateCalls(ctx, 1, f.a, "", f.now); !errors.Is(err, ErrPrivateCallPair) {
		t.Fatalf("a hidden peer: err = %v, want ErrPrivateCallPair", err)
	}
}
