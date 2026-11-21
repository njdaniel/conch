package store

// The four cases #2096's security review found no test pinned: each fails
// if its rule in dm_policy.go is removed (the review's mutation check).

import (
	"context"
	"errors"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// a disabled target who shares a channel with the caller.
func TestDMPolicyDisabledChannelMateIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	f.shareChannel(t, "shared", f.alice, f.bob, f.carol)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hi")
	f.disable(t, f.bob)
	f.disable(t, f.carol)
	if err := f.trySend(conv, f.alice, "to a disabled channel-mate"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send to a disabled channel-mate: %v", err)
	}
	if _, err := f.s.OpenDM(ctx, f.alice.ID, f.carol.ID, f.tick()); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("open a new pair with a disabled channel-mate: %v", err)
	}
}

// on hide_members the target knows the caller (the caller joined by
// the target's link) but the caller may not know the target.
func TestDMPolicyCallerMustKnowTargetOnWrite(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	mine := f.send(t, conv, f.alice, "hi")
	f.hideMembers(t)
	if _, err := f.s.db.Exec("UPDATE principals SET invited_by = ? WHERE id = ?", f.bob.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.trySend(conv, f.alice, "to someone I may not know"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send: %v", err)
	}
	if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, mine.ID, "edited", f.tick); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("edit: %v", err)
	}
}

// an ended guest is not found, in the same words.
func TestDMPolicyEndedGuestIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms, ended_at, end_reason)
		VALUES (?, 'g1', 'address', 'ended', 1, 2, 1000, 2, 'end_visit')`, f.carol.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.OpenDM(ctx, f.alice.ID, f.carol.ID, f.tick())
	if !errors.Is(err, ErrPrincipalNotFound) || err.Error() != ErrPrincipalNotFound.Error() {
		t.Errorf("ended guest: %v", err)
	}
}

// a ban ends "shares a channel" for the shared_channel preference.
func TestDMPolicyBanEndsSharingAChannel(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ch, err := f.s.CreateChannel(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{f.alice, f.bob} {
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hi")
	f.prefer(t, f.bob, schema.DMAcceptSharedChannel)
	if err := f.trySend(conv, f.alice, "while sharing"); err != nil {
		t.Fatalf("send while sharing: %v", err)
	}
	if _, err := f.s.db.Exec("INSERT INTO channel_bans (channel_id, principal_id, created_at) VALUES (?, ?, 1)", ch.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.trySend(conv, f.alice, "after a ban"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send after a ban from the only shared channel: %v", err)
	}
}
