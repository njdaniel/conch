package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// Operator administration of invites (issue #438), the store half: the walk
// down the tree of invites, and the invites closed when their maker is
// disabled or loses the right to invite.

// join redeems code as name and returns the new principal.
func (f *inviteFixture) join(code, name string) Principal {
	f.t.Helper()
	res, err := f.redeem(code, name)
	if err != nil {
		f.t.Fatalf("redeem %s: %v", name, err)
	}
	return res.Principal
}

// inviteTree is op -> ada (through op's link, which grants the right to
// invite) -> ben and eve (ada's personal invites) -> cy (ben's) -> dee (cy's),
// each one granted the right by the operator before inviting.
type storeInviteTree struct {
	*inviteFixture
	ada, ben, eve, cy, dee Principal
}

func newStoreInviteTree(t *testing.T) *storeInviteTree {
	t.Helper()
	f := &storeInviteTree{inviteFixture: newInviteFixture(t)}
	yes := true
	link := f.mustCreate(f.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}, MayInvite: &yes})
	f.ada = f.join(link.Code, "ada")
	invite := func(by Principal, name string) Principal {
		t.Helper()
		if _, err := f.s.SetMayInvite(f.ctx, "system", by.ID, true); err != nil {
			t.Fatal(err)
		}
		return f.join(f.mustCreate(by, personal("general")).Code, name)
	}
	f.ben = invite(f.ada, "ben")
	f.eve = invite(f.ada, "eve")
	f.cy = invite(f.ben, "cy")
	f.dee = invite(f.cy, "dee")
	return f
}

func principalIDs(ps []Principal) []int64 {
	out := []int64{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func TestInviteesWalk(t *testing.T) {
	f := newStoreInviteTree(t)
	tests := []struct {
		name  string
		root  Principal
		depth int
		want  []Principal
	}{
		{"direct", f.ada, 1, []Principal{f.ben, f.eve}},
		{"two generations", f.ada, 2, []Principal{f.ben, f.eve, f.cy}},
		{"all, by generation", f.ada, 0, []Principal{f.ben, f.eve, f.cy, f.dee}},
		{"all, negative depth", f.ada, -1, []Principal{f.ben, f.eve, f.cy, f.dee}},
		{"the operator, direct", f.op, 1, []Principal{f.ada}},
		{"the operator, all", f.op, 0, []Principal{f.ada, f.ben, f.eve, f.cy, f.dee}},
		{"part of the way down", f.cy, 0, []Principal{f.dee}},
		{"a leaf", f.dee, 0, nil},
		{"invited by nobody", f.bob, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.s.Invitees(f.ctx, tt.root.ID, tt.depth)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("Invitees returned nil, want an empty slice")
			}
			if g, w := principalIDs(got), principalIDs(tt.want); !slices.Equal(g, w) {
				t.Errorf("invitees = %v, want %v", g, w)
			}
		})
	}
	if _, err := f.s.Invitees(f.ctx, 9999, 0); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown root = %v, want ErrPrincipalNotFound", err)
	}
}

// No invite can make a cycle, but the walk must end on one anyway and name
// nobody twice, nor the root.
func TestInviteesWalkSurvivesACycle(t *testing.T) {
	f := newStoreInviteTree(t)
	if _, err := f.s.db.Exec("UPDATE principals SET invited_by = ? WHERE id = ?", f.dee.ID, f.ada.ID); err != nil {
		t.Fatal(err)
	}
	// And eve invited herself.
	if _, err := f.s.db.Exec("UPDATE principals SET invited_by = ? WHERE id = ?", f.eve.ID, f.eve.ID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		root Principal
		want []Principal
	}{
		{f.ada, []Principal{f.ben, f.cy, f.dee}},
		{f.cy, []Principal{f.dee, f.ada, f.ben}},
		{f.dee, []Principal{f.ada, f.ben, f.cy}},
		{f.eve, []Principal{}},
	}
	for _, tt := range tests {
		t.Run(tt.root.Name, func(t *testing.T) {
			got, err := f.s.Invitees(f.ctx, tt.root.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := principalIDs(got), principalIDs(tt.want); !slices.Equal(g, w) {
				t.Errorf("invitees = %v, want %v", g, w)
			}
		})
	}
}

// closedReasons lists the invite_closed rows as "invite:<id> reason=<why>".
func (f *inviteFixture) closedReasons() []string {
	f.t.Helper()
	var out []string
	for _, e := range f.audit() {
		if e.Action == AuditInviteClosed {
			out = append(out, e.Subject+" "+e.Detail[strings.LastIndex(e.Detail, "reason="):])
		}
	}
	return out
}

func (f *inviteFixture) closedAt(id int64) bool {
	f.t.Helper()
	return f.count("SELECT COUNT(*) FROM invites WHERE id = ? AND closed_at IS NOT NULL", id) == 1
}

func TestCloseInviteReason(t *testing.T) {
	f := newInviteFixture(t)
	own := f.mustCreate(f.alice, personal("general"))
	other := f.mustCreate(f.alice, personal("general"))
	opsOwn := f.mustCreate(f.op, open("general"))
	for _, c := range []struct {
		closer Principal
		id     int64
	}{{f.alice, own.Invite.ID}, {f.op, other.Invite.ID}, {f.op, opsOwn.Invite.ID}} {
		if _, err := f.s.CloseInvite(f.ctx, principalActor(c.closer.ID), c.closer.ID, c.id, f.now); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		fmt.Sprintf("invite:%d reason=creator", own.Invite.ID),
		fmt.Sprintf("invite:%d reason=operator", other.Invite.ID),
		fmt.Sprintf("invite:%d reason=creator", opsOwn.Invite.ID),
	}
	if got := f.closedReasons(); !slices.Equal(got, want) {
		t.Errorf("invite_closed = %v, want %v", got, want)
	}
}

// Disabling a principal closes every invite they made that could still be
// used, in the same transaction, as creator_disabled; a used one is left as
// it is, and enabling them again revives nothing.
func TestDisablePrincipalClosesTheirInvites(t *testing.T) {
	f := newInviteFixture(t)
	open1 := f.mustCreate(f.alice, personal("general"))
	open2 := f.mustCreate(f.alice, personal("lobby"))
	used := f.mustCreate(f.alice, personal("general"))
	f.join(used.Code, "joiner")
	opsLink := f.mustCreate(f.op, open("general"))

	if _, err := f.s.DisablePrincipal(f.ctx, principalActor(f.op.ID), f.alice.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("invite:%d reason=creator_disabled", open1.Invite.ID),
		fmt.Sprintf("invite:%d reason=creator_disabled", open2.Invite.ID),
	}
	if got := f.closedReasons(); !slices.Equal(got, want) {
		t.Errorf("invite_closed = %v, want %v", got, want)
	}
	for _, e := range f.audit() {
		if e.Action == AuditInviteClosed && e.Actor != principalActor(f.op.ID) {
			t.Errorf("invite_closed actor = %q", e.Actor)
		}
	}
	if !f.closedAt(open1.Invite.ID) || !f.closedAt(open2.Invite.ID) || f.closedAt(used.Invite.ID) || f.closedAt(opsLink.Invite.ID) {
		t.Error("the wrong invites were closed")
	}
	// A repeat disable changes nothing; enabling revives nothing.
	if changed, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil || changed {
		t.Fatalf("repeat disable = %v, %v", changed, err)
	}
	if _, err := f.s.EnablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.redeem(open1.Code, "late"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("redeem after enable = %v, want ErrInviteInvalid", err)
	}
	if n := len(f.closedReasons()); n != 2 {
		t.Errorf("invite_closed rows = %d, want 2", n)
	}
}

// An operator's open link is closed when that operator is disabled, too.
func TestDisableOperatorClosesTheirLinks(t *testing.T) {
	f := newInviteFixture(t)
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	link := f.mustCreate(f.bob, open("general"))
	if _, err := f.s.DisablePrincipal(f.ctx, "system", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if !f.closedAt(link.Invite.ID) {
		t.Error("the disabled operator's link is still open")
	}
}

// Disabling the last operator is refused and closes nothing.
func TestDisableLastOperatorClosesNothing(t *testing.T) {
	f := newInviteFixture(t)
	link := f.mustCreate(f.op, open("general"))
	if _, err := f.s.DisablePrincipal(f.ctx, "system", f.op.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("disable = %v, want ErrLastOperator", err)
	}
	if f.closedAt(link.Invite.ID) || len(f.closedReasons()) != 0 {
		t.Error("a refused disable closed an invite")
	}
}

// Taking the right to invite away closes the person's open personal invites;
// an operator's invites stand, whatever their flag says.
func TestSetMayInviteOffClosesPersonalInvites(t *testing.T) {
	f := newInviteFixture(t)
	mine := f.mustCreate(f.alice, personal("general"))
	opsLink := f.mustCreate(f.op, open("general"))
	opsPersonal := f.mustCreate(f.op, personal("general"))

	if _, err := f.s.SetMayInvite(f.ctx, principalActor(f.op.ID), f.alice.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, want := f.closedReasons(), []string{fmt.Sprintf("invite:%d reason=permission_revoked", mine.Invite.ID)}; !slices.Equal(got, want) {
		t.Errorf("invite_closed = %v, want %v", got, want)
	}
	// Given back, the closed invite stays closed.
	if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.redeem(mine.Code, "late"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("redeem after the right came back = %v, want ErrInviteInvalid", err)
	}

	for _, may := range []bool{true, false} {
		if _, err := f.s.SetMayInvite(f.ctx, "system", f.op.ID, may); err != nil {
			t.Fatal(err)
		}
	}
	if f.closedAt(opsLink.Invite.ID) || f.closedAt(opsPersonal.Invite.ID) {
		t.Error("an operator's invites were closed by their flag")
	}
	if n := len(f.closedReasons()); n != 1 {
		t.Errorf("invite_closed rows = %d, want 1", n)
	}
}
