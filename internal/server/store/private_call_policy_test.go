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
