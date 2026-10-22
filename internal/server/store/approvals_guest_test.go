package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// addDecidingGuest makes a guest a member of the fixture's channel, then gives
// its membership can_decide = 1 by a raw write: no store path does that for a
// guest (#705 forces 0 and refuses true), so this is what a future path that
// forgot the rule would leave behind.
func (f decideFixture) addDecidingGuest(t *testing.T) Principal {
	t.Helper()
	ctx := context.Background()
	g, err := f.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.channelID}, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, "UPDATE channel_members SET can_decide = 1 WHERE channel_id = ? AND principal_id = ?",
		f.channelID, g.Principal.ID); err != nil {
		t.Fatal(err)
	}
	return g.Principal
}

// Issue #706, review F1: CastMemberDecision reads the guest flag inside the
// deciding transaction, so a guest is refused (ErrNoDecideRight, which the
// server answers decision_not_permitted) and writes nothing, whatever its
// membership row says.
func TestCastMemberDecisionRefusesAGuestInTheTransaction(t *testing.T) {
	f := newDecideFixture(t, 1)
	guest := f.addDecidingGuest(t)
	_, res, err := f.s.CastMemberDecision(context.Background(), f.approvalID, guest.ID, "approve", "fine")
	if !errors.Is(err, ErrNoDecideRight) {
		t.Fatalf("guest's decision err = %v, want ErrNoDecideRight", err)
	}
	if res != nil {
		t.Fatalf("a refused decision resolved the approval: %+v", res)
	}
	if d, a := f.decisionRows(t); d != 0 || a != 0 {
		t.Fatalf("a refused decision wrote %d decisions and %d audit rows", d, a)
	}
	// A member with the right still decides, as before.
	if _, res, err := f.s.CastMemberDecision(context.Background(), f.approvalID, f.ann.ID, "approve", "fine"); err != nil || res == nil {
		t.Fatalf("ann's decision = %v, %v; want it to resolve", res, err)
	}
}

// Quorum never counts a guest: a decision row a guest somehow has does not
// make up the number, and members' decisions count exactly as before.
func TestQuorumNeverCountsAGuest(t *testing.T) {
	f := newDecideFixture(t, 2)
	ctx := context.Background()
	guest := f.addDecidingGuest(t)
	// The row a guest could never cast through the store, written directly.
	if _, err := f.s.db.ExecContext(ctx,
		"INSERT INTO approval_decisions (approval_id, principal_id, option_id, reason, created_at) VALUES (?, ?, 'approve', 'fine', ?)",
		f.approvalID, guest.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, res, err := f.s.CastMemberDecision(ctx, f.approvalID, f.ann.ID, "approve", "fine"); err != nil || res != nil {
		t.Fatalf("ann's decision with only a guest beside it = %v, %v; want pending (quorum 2)", res, err)
	}
	if _, res, err := f.s.CastMemberDecision(ctx, f.approvalID, f.bob.ID, "approve", "fine"); err != nil || res == nil {
		t.Fatalf("bob's decision = %v, %v; want it to resolve with two members", res, err)
	}
}

// A converted guest is a member: the flag is about guests, not about who was
// one. Its membership keeps can_decide 0 (conversion grants nothing), so it
// decides only after an operator grants the right.
func TestAConvertedGuestDecidesOnceGrantedTheRight(t *testing.T) {
	f := newDecideFixture(t, 1)
	ctx := context.Background()
	g, err := f.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.channelID}, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ConvertGuest(ctx, "system", GuestConversion{PrincipalID: g.Principal.ID, Name: "pat", CredentialLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.CastMemberDecision(ctx, f.approvalID, g.Principal.ID, "approve", "fine"); !errors.Is(err, ErrNoDecideRight) {
		t.Fatalf("converted, not yet granted: err = %v, want ErrNoDecideRight", err)
	}
	if _, err := f.s.SetChannelMemberCanDecide(ctx, "system", f.channelID, g.Principal.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, res, err := f.s.CastMemberDecision(ctx, f.approvalID, g.Principal.ID, "approve", "fine"); err != nil || res == nil {
		t.Fatalf("converted and granted: %v, %v; want it to resolve", res, err)
	}
}

// may_invite is a durable right: an operator cannot give it to a guest
// (ErrGuestNotAllowed, nothing written); clearing it is allowed.
func TestSetMayInviteRefusesAGuest(t *testing.T) {
	f := newDecideFixture(t, 1)
	ctx := context.Background()
	g, err := f.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.channelID}, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetMayInvite(ctx, "system", g.Principal.ID, true); !errors.Is(err, ErrGuestNotAllowed) {
		t.Fatalf("SetMayInvite(true) on a guest = %v, want ErrGuestNotAllowed", err)
	}
	var may int
	if err := f.s.db.QueryRowContext(ctx, "SELECT may_invite FROM principals WHERE id = ?", g.Principal.ID).Scan(&may); err != nil || may != 0 {
		t.Fatalf("may_invite = %d (%v), want 0", may, err)
	}
	if _, err := f.s.SetMayInvite(ctx, "system", g.Principal.ID, false); err != nil {
		t.Fatalf("SetMayInvite(false) on a guest = %v, want nil", err)
	}
}
