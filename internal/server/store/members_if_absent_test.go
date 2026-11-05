package store

import (
	"context"
	"errors"
	"testing"
)

// AddChannelMemberIfAbsent adds by name or id, and never changes a member it
// finds already there: an existing decider keeps deciding, an existing
// non-decider is not made one (issue #1206).
func TestAddChannelMemberIfAbsent(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)
	ch, err := f.s.CreateChannel(ctx, "delta")
	if err != nil {
		t.Fatal(err)
	}
	owner := principalActor(f.owner.ID)

	adds := []struct {
		name      string
		target    MemberTarget
		canDecide *bool
		want      bool
	}{
		{"by id, deciding", MemberTarget{ID: f.sharedMem.ID}, boolPtr(true), true},
		{"by id, not deciding", MemberTarget{ID: f.unsharedMem.ID}, boolPtr(false), false},
		{"by id, decide absent", MemberTarget{ID: f.operator.ID}, nil, false},
	}
	for _, tc := range adds {
		t.Run("new member "+tc.name, func(t *testing.T) {
			cm, added, err := f.s.AddChannelMemberIfAbsent(ctx, owner, ch.ID, f.owner.ID, tc.target, tc.canDecide)
			if err != nil || !added || cm.PrincipalID != tc.target.ID || cm.CanDecide != tc.want || cm.Manages {
				t.Fatalf("AddChannelMemberIfAbsent = %+v, %v, %v; want added with CanDecide=%v, Manages=false", cm, added, err, tc.want)
			}
		})
	}

	// sharedMem now decides and unsharedMem does not. Asking again, with the
	// opposite decide right, changes neither and writes nothing.
	existing := []struct {
		name      string
		target    MemberTarget
		canDecide *bool
		want      bool
	}{
		{"a decider asked not to decide, by id", MemberTarget{ID: f.sharedMem.ID}, boolPtr(false), true},
		{"a decider, by name", MemberTarget{Name: f.sharedMem.Name}, nil, true},
		{"a non-decider asked to decide, by id", MemberTarget{ID: f.unsharedMem.ID}, boolPtr(true), false},
	}
	for _, tc := range existing {
		t.Run("existing member "+tc.name, func(t *testing.T) {
			before := countRows(t, f.s, "audit_events")
			cm, added, err := f.s.AddChannelMemberIfAbsent(ctx, owner, ch.ID, f.owner.ID, tc.target, tc.canDecide)
			if err != nil || added || cm.CanDecide != tc.want {
				t.Fatalf("AddChannelMemberIfAbsent = %+v, %v, %v; want unchanged with CanDecide=%v", cm, added, err, tc.want)
			}
			if countRows(t, f.s, "audit_events") != before {
				t.Error("an add for an existing member wrote an audit event")
			}
		})
	}

	// A manager may not send a decide right, true or false.
	if _, err := f.s.PutChannelMember(ctx, owner, ch.ID, f.manager.ID, f.owner.ID, boolPtr(false), boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	for _, decide := range []bool{true, false} {
		if _, _, err := f.s.AddChannelMemberIfAbsent(ctx, principalActor(f.manager.ID), ch.ID, f.manager.ID, MemberTarget{ID: f.disabled.ID}, boolPtr(decide)); !errors.Is(err, ErrManagerRefused) {
			t.Errorf("manager with can_decide %v = %v, want ErrManagerRefused", decide, err)
		}
	}
	// A plain operator may not set an owner's decide right.
	if _, _, err := f.s.AddChannelMemberIfAbsent(ctx, principalActor(f.operator.ID), f.ch1.ID, f.operator.ID, MemberTarget{ID: f.owner.ID}, boolPtr(false)); !errors.Is(err, ErrOwnerProtected) {
		t.Errorf("operator setting an owner's can_decide = %v, want ErrOwnerProtected", err)
	}

	// By id, every target that may not be added is refused exactly as by name.
	for _, tc := range []struct {
		label string
		id    int64
	}{
		{"unknown id", 99999},
		{"agent", f.agent.ID},
		{"active guest", f.guest.ID},
		{"ended guest", f.endedGuest.ID},
		{"disabled human", f.disabled.ID},
		{"hidden human", f.hidden.ID},
	} {
		t.Run("refused "+tc.label, func(t *testing.T) {
			_, added, err := f.s.AddChannelMemberIfAbsent(ctx, owner, ch.ID, f.owner.ID, MemberTarget{ID: tc.id}, nil)
			if added || !errors.Is(err, ErrNotAddable) || err.Error() != ErrNotAddable.Error() {
				t.Fatalf("AddChannelMemberIfAbsent(%s) = %v, %v; want ErrNotAddable", tc.label, added, err)
			}
		})
	}
}
