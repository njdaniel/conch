package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func memberIDs(t *testing.T, s *Store, channelID int64) []int64 {
	t.Helper()
	ms, err := s.ListChannelMembers(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, m := range ms {
		ids = append(ids, m.PrincipalID)
	}
	return ids
}

func auditCount(t *testing.T, s *Store, action string) int {
	t.Helper()
	evs, err := s.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestChannelMembersRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateChannel(ctx, "other")
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	bob, _ := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
	adder, _ := s.CreatePrincipal(ctx, PrincipalHuman, "adder")

	if got := memberIDs(t, s, ch.ID); len(got) != 0 {
		t.Fatalf("new channel members = %v, want none", got)
	}
	// Add out of order: listing is ordered by principal id.
	for _, id := range []int64{bob.ID, alice.ID} {
		added, err := s.AddChannelMember(ctx, "principal:9", ch.ID, id, adder.ID)
		if err != nil || !added {
			t.Fatalf("add %d = %v, %v", id, added, err)
		}
	}
	if got, want := memberIDs(t, s, ch.ID), []int64{alice.ID, bob.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
	ms, _ := s.ListChannelMembers(ctx, ch.ID)
	if ms[0].AddedBy != adder.ID || ms[0].CreatedAt.IsZero() {
		t.Errorf("member = %+v, want added_by adder and a timestamp", ms[0])
	}
	if ok, err := s.IsChannelMember(ctx, ch.ID, alice.ID); err != nil || !ok {
		t.Errorf("IsChannelMember(alice) = %v, %v", ok, err)
	}
	if ok, err := s.IsChannelMember(ctx, other.ID, alice.ID); err != nil || ok {
		t.Errorf("IsChannelMember(other channel) = %v, %v, want false", ok, err)
	}
	chs, err := s.ListChannelsForPrincipal(ctx, alice.ID)
	if err != nil || len(chs) != 1 || chs[0].ID != ch.ID {
		t.Errorf("channels for alice = %+v, %v", chs, err)
	}
	if chs, err := s.ListChannelsForPrincipal(ctx, 12345); err != nil || chs == nil || len(chs) != 0 {
		t.Errorf("channels for unknown principal = %#v, %v, want empty non-nil", chs, err)
	}

	removed, err := s.RemoveChannelMember(ctx, "principal:9", ch.ID, alice.ID)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if got, want := memberIDs(t, s, ch.ID), []int64{bob.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("members after remove = %v, want %v", got, want)
	}
	if chs, _ := s.ListChannelsForPrincipal(ctx, alice.ID); len(chs) != 0 {
		t.Errorf("channels for removed alice = %+v", chs)
	}
}

func TestChannelMembersIdempotencyAndErrors(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, _ := s.CreateChannel(ctx, "general")
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")

	tests := []struct {
		name      string
		do        func() (bool, error)
		want      bool
		wantErr   error
		wantAdds  int
		wantRemov int
	}{
		{"add new", func() (bool, error) { return s.AddChannelMember(ctx, "system", ch.ID, alice.ID, 0) }, true, nil, 1, 0},
		{"add again is a no-op", func() (bool, error) { return s.AddChannelMember(ctx, "system", ch.ID, alice.ID, 0) }, false, nil, 1, 0},
		{"remove", func() (bool, error) { return s.RemoveChannelMember(ctx, "system", ch.ID, alice.ID) }, true, nil, 1, 1},
		{"remove again is a no-op", func() (bool, error) { return s.RemoveChannelMember(ctx, "system", ch.ID, alice.ID) }, false, nil, 1, 1},
		{"add unknown channel", func() (bool, error) { return s.AddChannelMember(ctx, "system", 999, alice.ID, 0) }, false, ErrNotFound, 1, 1},
		{"add unknown principal", func() (bool, error) { return s.AddChannelMember(ctx, "system", ch.ID, 999, 0) }, false, ErrPrincipalNotFound, 1, 1},
		{"remove unknown channel", func() (bool, error) { return s.RemoveChannelMember(ctx, "system", 999, alice.ID) }, false, ErrNotFound, 1, 1},
		{"remove unknown principal", func() (bool, error) { return s.RemoveChannelMember(ctx, "system", ch.ID, 999) }, false, ErrPrincipalNotFound, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.do()
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("got %v, %v; want %v, %v", got, err, tt.want, tt.wantErr)
			}
			if n := auditCount(t, s, AuditMemberAdded); n != tt.wantAdds {
				t.Errorf("member_added events = %d, want %d", n, tt.wantAdds)
			}
			if n := auditCount(t, s, AuditMemberRemoved); n != tt.wantRemov {
				t.Errorf("member_removed events = %d, want %d", n, tt.wantRemov)
			}
		})
	}
	// added_by NULL reads back as 0 (unknown).
	_, _ = s.AddChannelMember(ctx, "system", ch.ID, alice.ID, 0)
	if ms, _ := s.ListChannelMembers(ctx, ch.ID); len(ms) != 1 || ms[0].AddedBy != 0 {
		t.Errorf("members = %+v, want added_by unknown", ms)
	}
}

func TestAuditEventShape(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, _ := s.CreateChannel(ctx, "general")
	adder, _ := s.CreatePrincipal(ctx, PrincipalHuman, "adder")
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	if _, err := s.AddChannelMember(ctx, "principal:5", ch.ID, alice.ID, adder.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveChannelMember(ctx, "principal:5", ch.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.ListAuditEvents(ctx, 0, 100)
	var got []AuditEvent
	for _, e := range evs {
		if e.Action == AuditMemberAdded || e.Action == AuditMemberRemoved {
			got = append(got, e)
		}
	}
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	for _, e := range got {
		if e.Actor != "principal:5" || e.Subject != "channel:1" || e.Detail != "principal=2" {
			t.Errorf("event = %+v", e)
		}
	}
}

func TestCreateChannelAsCreatorMembership(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")

	tests := []struct {
		name        string
		creator     int64
		wantMembers []int64
		wantAudit   int
	}{
		{"with creator", alice.ID, []int64{alice.ID}, 1},
		{"without creator (auth off)", 0, []int64{}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := s.CreateChannelAs(ctx, auditActorFor(tt.creator), tt.name, tt.creator)
			if err != nil {
				t.Fatal(err)
			}
			if got := memberIDs(t, s, ch.ID); !reflect.DeepEqual(got, tt.wantMembers) {
				t.Errorf("members = %v, want %v", got, tt.wantMembers)
			}
			if tt.creator != 0 {
				ms, _ := s.ListChannelMembers(ctx, ch.ID)
				if ms[0].AddedBy != tt.creator {
					t.Errorf("added_by = %d, want creator", ms[0].AddedBy)
				}
			}
		})
	}
	if n := auditCount(t, s, AuditMemberAdded); n != 1 {
		t.Errorf("member_added events = %d, want 1 (creator only)", n)
	}
	if _, err := s.CreateChannelAs(ctx, "system", "with creator", alice.ID); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate = %v, want ErrDuplicate", err)
	}
	if n := auditCount(t, s, AuditMemberAdded); n != 1 {
		t.Errorf("duplicate create wrote an audit event: %d", n)
	}
}

func auditActorFor(id int64) string {
	if id == 0 {
		return "system"
	}
	return principalActor(id)
}

// TestMembershipMigrationFromSchema7 builds a version 7 database with
// principals and channels and checks the upgrade yields full cross membership,
// while a channel or principal created afterwards is not auto-joined.
func TestMembershipMigrationFromSchema7(t *testing.T) {
	const preMembership = 7
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preMembership; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'agent', 'bot', 'member', 2), (3, 'human', 'zed', 'member', 3)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 1), (2, 'ops', 2)`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, ch := range []int64{1, 2} {
		if got, want := memberIDs(t, s, ch), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("channel %d members = %v, want %v", ch, got, want)
		}
		ms, _ := s.ListChannelMembers(ctx, ch)
		for _, m := range ms {
			if m.AddedBy != 0 || m.CreatedAt.IsZero() {
				t.Errorf("backfilled member = %+v, want added_by unknown and a timestamp", m)
			}
		}
	}
	if n := auditCount(t, s, AuditMemberAdded); n != 0 {
		t.Errorf("migration wrote %d member_added events, want 0", n)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
	}

	// Created after the migration: not auto-joined.
	late, err := s.CreatePrincipal(ctx, PrincipalHuman, "late")
	if err != nil {
		t.Fatal(err)
	}
	newCh, err := s.CreateChannel(ctx, "after")
	if err != nil {
		t.Fatal(err)
	}
	if chs, _ := s.ListChannelsForPrincipal(ctx, late.ID); len(chs) != 0 {
		t.Errorf("new principal channels = %+v, want none", chs)
	}
	if got := memberIDs(t, s, newCh.ID); len(got) != 0 {
		t.Errorf("new channel members = %v, want none", got)
	}
	if got := memberIDs(t, s, 1); len(got) != 3 {
		t.Errorf("old channel gained members: %v", got)
	}
}

func TestChannelMembersMigrationFromSchema34(t *testing.T) {
	const preManages = 34
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preManages; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, owner, created_at) VALUES (1, 'human', 'root', 'root', 'root', 'operator', 1, 1), (2, 'human', 'alice', 'alice', 'alice', 'member', 0, 2)`,
		`INSERT INTO channels (id, name, name_key, name_skel, created_at) VALUES (1, 'general', 'general', 'general', 1)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide) VALUES (1, 1, 1, 1, 1), (1, 2, 1, 2, 1)`,
		`PRAGMA user_version = 34`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (%v), want %d", version, err, len(migrations))
	}
	members, err := s.ListChannelMembers(ctx, 1)
	if err != nil || len(members) != 2 {
		t.Fatalf("ListChannelMembers = %+v, %v", members, err)
	}
	for _, m := range members {
		if m.Manages {
			t.Errorf("migrated member %d has Manages=true, want false", m.PrincipalID)
		}
	}
	if _, err := s.db.Exec("UPDATE channel_members SET manages = 2 WHERE channel_id = 1 AND principal_id = 1"); err == nil {
		t.Error("CHECK (manages IN (0, 1)) accepted manages = 2")
	}
}

type managerTestFixture struct {
	s           *Store
	ch1         Channel
	ch2         Channel
	owner       Principal
	operator    Principal
	manager     Principal
	otherMgr    Principal
	sharedMem   Principal
	unsharedMem Principal
	approver    Principal
	fellowMgr   Principal
	agent       Principal
	guest       Principal
	endedGuest  Principal
	disabled    Principal
	hidden      Principal
}

func newManagerTestFixture(t *testing.T) managerTestFixture {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)

	owner, _, _, err := s.BootstrapOperator(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := s.CreatePrincipal(ctx, PrincipalHuman, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(ctx, principalActor(owner.ID), owner.ID, operator.ID, RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	operator, _ = s.PrincipalByID(ctx, operator.ID)

	ch1, err := s.CreateChannel(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := s.CreateChannel(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}

	manager, _ := s.CreatePrincipal(ctx, PrincipalHuman, "manager")
	otherMgr, _ := s.CreatePrincipal(ctx, PrincipalHuman, "other-mgr")
	sharedMem, _ := s.CreatePrincipal(ctx, PrincipalHuman, "shared-mem")
	unsharedMem, _ := s.CreatePrincipal(ctx, PrincipalHuman, "unshared-mem")
	approver, _ := s.CreatePrincipal(ctx, PrincipalHuman, "approver")
	fellowMgr, _ := s.CreatePrincipal(ctx, PrincipalHuman, "fellow-mgr")
	agent, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	disabled, err := s.CreatePrincipal(ctx, PrincipalHuman, "disabled-p")
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.CreatePrincipal(ctx, PrincipalHuman, "vanished-p")
	if err != nil {
		t.Fatal(err)
	}

	// Seed ch1: owner, operator, manager (can_decide=0, manages=1), fellowMgr (can_decide=0, manages=1), approver (can_decide=1), agent.
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, owner.ID, owner.ID, boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, operator.ID, owner.ID, boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, manager.ID, owner.ID, boolPtr(false), boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, fellowMgr.ID, owner.ID, boolPtr(false), boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, approver.ID, owner.ID, boolPtr(true), boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch1.ID, agent.ID, owner.ID, boolPtr(false)); err != nil {
		t.Fatal(err)
	}

	// Seed ch2: manager (can_decide=0, manages=0), otherMgr (can_decide=0, manages=1), sharedMem, operator, owner, agent, disabled, hidden.
	for _, pid := range []int64{manager.ID, sharedMem.ID, operator.ID, owner.ID, agent.ID, disabled.ID, hidden.ID} {
		if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch2.ID, pid, owner.ID, boolPtr(false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutChannelMember(ctx, principalActor(owner.ID), ch2.ID, otherMgr.ID, owner.ID, boolPtr(false), boolPtr(true)); err != nil {
		t.Fatal(err)
	}

	g1, err := s.CreateGuest(ctx, "system", NewGuest{
		Label: "visitor", Origin: schema.GuestOriginAddress,
		Channels: []int64{ch2.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.CreateGuest(ctx, "system", NewGuest{
		Label: "ended-visitor", Origin: schema.GuestOriginAddress,
		Channels: []int64{ch2.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndGuestVisit(ctx, principalActor(owner.ID), g2.Principal.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisablePrincipal(ctx, principalActor(owner.ID), disabled.ID); err != nil {
		t.Fatal(err)
	}
	hideRes, err := s.HidePrincipal(ctx, principalActor(owner.ID), hidden.ID, HideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hidden = hideRes.Principal

	return managerTestFixture{
		s:           s,
		ch1:         ch1,
		ch2:         ch2,
		owner:       owner,
		operator:    operator,
		manager:     manager,
		otherMgr:    otherMgr,
		sharedMem:   sharedMem,
		unsharedMem: unsharedMem,
		approver:    approver,
		fellowMgr:   fellowMgr,
		agent:       agent,
		guest:       g1.Principal,
		endedGuest:  g2.Principal,
		disabled:    disabled,
		hidden:      hidden,
	}
}

func TestSetChannelMemberManagesAndPutManages(t *testing.T) {
	ctx := context.Background()

	t.Run("grant, idempotency, revoke, and audit events", func(t *testing.T) {
		f := newManagerTestFixture(t)
		// Add sharedMem to ch1 as plain member first.
		if _, err := f.s.PutChannelMember(ctx, principalActor(f.operator.ID), f.ch1.ID, f.sharedMem.ID, f.operator.ID, boolPtr(false)); err != nil {
			t.Fatal(err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || m {
			t.Fatalf("ManagesChannel before grant = %v, %v; want false", m, err)
		}

		// Operator grants manages via SetChannelMemberManages.
		changed, err := f.s.SetChannelMemberManages(ctx, principalActor(f.operator.ID), f.ch1.ID, f.sharedMem.ID, true, f.operator.ID)
		if err != nil || !changed {
			t.Fatalf("SetChannelMemberManages(true) = %v, %v; want true, nil", changed, err)
		}
		ev, err := f.s.LastAuditEvent(ctx, AuditMemberManageGranted)
		if err != nil || ev.Actor != principalActor(f.operator.ID) || ev.Subject != channelSubject(f.ch1.ID) || ev.Detail != memberDetail(f.sharedMem.ID) {
			t.Errorf("audit event = %+v (%v)", ev, err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || !m {
			t.Errorf("ManagesChannel after grant = %v, %v; want true", m, err)
		}

		// Grant again is a no-op and writes no audit event.
		beforeGrants := auditCount(t, f.s, AuditMemberManageGranted)
		changed, err = f.s.SetChannelMemberManages(ctx, principalActor(f.operator.ID), f.ch1.ID, f.sharedMem.ID, true, f.operator.ID)
		if err != nil || changed {
			t.Fatalf("repeat SetChannelMemberManages(true) = %v, %v; want false, nil", changed, err)
		}
		if got := auditCount(t, f.s, AuditMemberManageGranted); got != beforeGrants {
			t.Errorf("repeat grant wrote audit event: %d != %d", got, beforeGrants)
		}

		// PutChannelMember with nil manages preserves manages=true.
		if _, err := f.s.PutChannelMember(ctx, principalActor(f.operator.ID), f.ch1.ID, f.sharedMem.ID, f.operator.ID, nil, nil); err != nil {
			t.Fatal(err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || !m {
			t.Errorf("ManagesChannel after nil Put = %v, %v; want true", m, err)
		}

		// PutChannelMember with manages=false revokes and audits.
		if _, err := f.s.PutChannelMember(ctx, principalActor(f.owner.ID), f.ch1.ID, f.sharedMem.ID, f.owner.ID, nil, boolPtr(false)); err != nil {
			t.Fatal(err)
		}
		revEv, err := f.s.LastAuditEvent(ctx, AuditMemberManageRevoked)
		if err != nil || revEv.Actor != principalActor(f.owner.ID) || revEv.Subject != channelSubject(f.ch1.ID) || revEv.Detail != memberDetail(f.sharedMem.ID) {
			t.Errorf("revoke audit event = %+v (%v)", revEv, err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || m {
			t.Errorf("ManagesChannel after revoke = %v, %v; want false", m, err)
		}

		// Granting manages on an operator (and an owner) is allowed and recorded.
		for _, target := range []Principal{f.operator, f.owner} {
			changed, err := f.s.SetChannelMemberManages(ctx, principalActor(f.operator.ID), f.ch1.ID, target.ID, true, f.operator.ID)
			if err != nil || !changed {
				t.Errorf("SetChannelMemberManages(operator/owner %d) = %v, %v; want true, nil", target.ID, changed, err)
			}
			if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, target.ID); err != nil || !m {
				t.Errorf("ManagesChannel(%d) = %v, %v; want true", target.ID, m, err)
			}
		}

		// ListChannelMembers populates Role and Manages; ListChannelsForPrincipal populates Manages.
		members, err := f.s.ListChannelMembers(ctx, f.ch1.ID)
		if err != nil {
			t.Fatal(err)
		}
		foundManager, foundOperator := false, false
		for _, cm := range members {
			if cm.PrincipalID == f.manager.ID {
				foundManager = true
				if cm.Role != RoleMember || !cm.Manages || cm.CanDecide {
					t.Errorf("manager member entry = %+v", cm)
				}
			}
			if cm.PrincipalID == f.operator.ID {
				foundOperator = true
				if cm.Role != RoleOperator || !cm.Manages || !cm.CanDecide {
					t.Errorf("operator member entry = %+v", cm)
				}
			}
		}
		if !foundManager || !foundOperator {
			t.Errorf("ListChannelMembers missing expected entries: foundManager=%v foundOperator=%v", foundManager, foundOperator)
		}

		chs, err := f.s.ListChannelsForPrincipal(ctx, f.manager.ID)
		if err != nil || len(chs) != 2 {
			t.Fatalf("ListChannelsForPrincipal(manager) = %+v, %v", chs, err)
		}
		if !chs[0].Manages || chs[1].Manages {
			t.Errorf("ListChannelsForPrincipal Manages = [%v, %v], want [true, false]", chs[0].Manages, chs[1].Manages)
		}

		// Disabling a manager makes ManagesChannel false without losing the column; re-enabling restores it.
		if _, err := f.s.DisablePrincipal(ctx, principalActor(f.owner.ID), f.manager.ID); err != nil {
			t.Fatal(err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.manager.ID); err != nil || m {
			t.Errorf("ManagesChannel for disabled manager = %v, %v; want false", m, err)
		}
		if _, err := f.s.EnablePrincipal(ctx, principalActor(f.owner.ID), f.manager.ID); err != nil {
			t.Fatal(err)
		}
		if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.manager.ID); err != nil || !m {
			t.Errorf("ManagesChannel for re-enabled manager = %v, %v; want true", m, err)
		}
	})

	t.Run("refusals on actor and target", func(t *testing.T) {
		f := newManagerTestFixture(t)
		tests := []struct {
			name      string
			channelID int64
			actorID   int64
			targetID  int64
			usePut    bool
			wantErr   error
		}{
			{"manager actor cannot SetChannelMemberManages", f.ch1.ID, f.manager.ID, f.approver.ID, false, ErrManagerRefused},
			{"manager actor cannot PutChannelMember with manages", f.ch1.ID, f.manager.ID, f.sharedMem.ID, true, ErrManagerRefused},
			{"plain member actor refused", f.ch1.ID, f.approver.ID, f.approver.ID, false, ErrManagerRefused},
			{"guest actor refused", f.ch2.ID, f.guest.ID, f.sharedMem.ID, false, ErrManagerRefused},
			{"agent actor refused", f.ch1.ID, f.agent.ID, f.approver.ID, false, ErrManagerRefused},
			{"disabled actor refused", f.ch2.ID, f.disabled.ID, f.sharedMem.ID, false, ErrManagerRefused},
			{"hidden actor refused", f.ch2.ID, f.hidden.ID, f.sharedMem.ID, false, ErrManagerRefused},
			{"unknown actor refused", f.ch1.ID, 99999, f.approver.ID, false, ErrManagerRefused},
			{"agent target via SetChannelMemberManages", f.ch1.ID, f.operator.ID, f.agent.ID, false, ErrManageNotAllowed},
			{"agent target via PutChannelMember", f.ch1.ID, f.operator.ID, f.agent.ID, true, ErrManageNotAllowed},
			{"active guest target via SetChannelMemberManages", f.ch2.ID, f.operator.ID, f.guest.ID, false, ErrGuestNotAllowed},
			{"active guest target via PutChannelMember", f.ch1.ID, f.operator.ID, f.guest.ID, true, ErrGuestNotAllowed},
			{"ended guest target via PutChannelMember", f.ch1.ID, f.operator.ID, f.endedGuest.ID, true, ErrGuestEnded},
			{"disabled target via SetChannelMemberManages", f.ch2.ID, f.operator.ID, f.disabled.ID, false, ErrManageNotAllowed},
			{"disabled target via PutChannelMember", f.ch1.ID, f.operator.ID, f.disabled.ID, true, ErrManageNotAllowed},
			{"hidden target via PutChannelMember", f.ch1.ID, f.operator.ID, f.hidden.ID, true, ErrManageNotAllowed},
			{"non-member target via SetChannelMemberManages", f.ch1.ID, f.operator.ID, f.unsharedMem.ID, false, ErrNotMember},
			{"unknown target via SetChannelMemberManages", f.ch1.ID, f.operator.ID, 99999, false, ErrPrincipalNotFound},
			{"unknown channel via SetChannelMemberManages", 99999, f.operator.ID, f.approver.ID, false, ErrNotFound},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				beforeAudit := countRows(t, f.s, "audit_events")
				var err error
				if tt.usePut {
					_, err = f.s.PutChannelMember(ctx, principalActor(tt.actorID), tt.channelID, tt.targetID, tt.actorID, nil, boolPtr(true))
				} else {
					_, err = f.s.SetChannelMemberManages(ctx, principalActor(tt.actorID), tt.channelID, tt.targetID, true, tt.actorID)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if afterAudit := countRows(t, f.s, "audit_events"); afterAudit != beforeAudit {
					t.Errorf("refused call wrote %d audit events", afterAudit-beforeAudit)
				}
			})
		}
	})
}

func TestOwnerProtectionOnMembershipWrites(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)
	ch3, err := f.s.CreateChannel(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}

	// Non-owner operator cannot set can_decide on an owner even when the owner is not yet a member of ch3.
	for _, decide := range []bool{false, true} {
		if _, err := f.s.PutChannelMember(ctx, principalActor(f.operator.ID), ch3.ID, f.owner.ID, f.operator.ID, boolPtr(decide)); !errors.Is(err, ErrOwnerProtected) || !errors.Is(err, ErrManagerRefused) {
			t.Errorf("operator PutChannelMember(non-member owner, can_decide=%v) = %v, want ErrOwnerProtected", decide, err)
		}
	}
	// Non-owner operator CAN add an owner with canDecide == nil (default).
	added, err := f.s.PutChannelMember(ctx, principalActor(f.operator.ID), ch3.ID, f.owner.ID, f.operator.ID, nil)
	if err != nil || !added {
		t.Fatalf("operator PutChannelMember(owner, canDecide=nil) = %v, %v; want true, nil", added, err)
	}
	// Once the owner is a member, a non-owner operator cannot change the owner's can_decide or remove the owner.
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.operator.ID), ch3.ID, f.owner.ID, f.operator.ID, boolPtr(false)); !errors.Is(err, ErrOwnerProtected) {
		t.Errorf("operator PutChannelMember(member owner, can_decide=false) = %v, want ErrOwnerProtected", err)
	}
	if _, err := f.s.SetChannelMemberCanDecide(ctx, principalActor(f.operator.ID), ch3.ID, f.owner.ID, false, f.operator.ID); !errors.Is(err, ErrOwnerProtected) {
		t.Errorf("operator SetChannelMemberCanDecide(owner, false) = %v, want ErrOwnerProtected", err)
	}
	if _, err := f.s.RemoveChannelMember(ctx, principalActor(f.operator.ID), ch3.ID, f.owner.ID, f.operator.ID); !errors.Is(err, ErrOwnerProtected) {
		t.Errorf("operator RemoveChannelMember(owner) = %v, want ErrOwnerProtected", err)
	}

	// An owner actor succeeds on all of those.
	if changed, err := f.s.SetChannelMemberCanDecide(ctx, principalActor(f.owner.ID), ch3.ID, f.owner.ID, false, f.owner.ID); err != nil || !changed {
		t.Errorf("owner SetChannelMemberCanDecide(owner, false) = %v, %v; want true, nil", changed, err)
	}
	if removed, err := f.s.RemoveChannelMember(ctx, principalActor(f.owner.ID), ch3.ID, f.owner.ID, f.owner.ID); err != nil || !removed {
		t.Errorf("owner RemoveChannelMember(owner) = %v, %v; want true, nil", removed, err)
	}
}

func TestManagerPutChannelMemberMatrix(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)

	// 1. Manager adds a human member who shares ch2 with the manager -> succeeds with can_decide=0, manages=0.
	added, err := f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, f.sharedMem.ID, f.manager.ID, nil)
	if err != nil || !added {
		t.Fatalf("manager PutChannelMember(sharedMem) = %v, %v; want true, nil", added, err)
	}
	if can, err := f.s.CanDecideApprovals(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || can {
		t.Errorf("CanDecideApprovals(sharedMem) = %v, %v; want false", can, err)
	}
	if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.sharedMem.ID); err != nil || m {
		t.Errorf("ManagesChannel(sharedMem) = %v, %v; want false", m, err)
	}
	ev, err := f.s.LastAuditEvent(ctx, AuditMemberAdded)
	if err != nil || ev.Actor != principalActor(f.manager.ID) || ev.Detail != fmt.Sprintf("principal=%d can_decide=0", f.sharedMem.ID) {
		t.Errorf("manager add audit event = %+v (%v)", ev, err)
	}

	// 2. Adding an already-present member (including an approver or fellow manager) is a no-op and preserves their flags.
	for _, existingID := range []int64{f.sharedMem.ID, f.approver.ID, f.fellowMgr.ID} {
		beforeAudit := countRows(t, f.s, "audit_events")
		added, err := f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, existingID, f.manager.ID, nil)
		if err != nil || added {
			t.Errorf("manager repeat PutChannelMember(%d) = %v, %v; want false, nil", existingID, added, err)
		}
		if afterAudit := countRows(t, f.s, "audit_events"); afterAudit != beforeAudit {
			t.Errorf("repeat PutChannelMember(%d) wrote audit event", existingID)
		}
	}
	if can, _ := f.s.CanDecideApprovals(ctx, f.ch1.ID, f.approver.ID); !can {
		t.Error("manager repeat add reset approver's can_decide")
	}
	if m, _ := f.s.ManagesChannel(ctx, f.ch1.ID, f.fellowMgr.ID); !m {
		t.Error("manager repeat add reset fellowMgr's manages")
	}

	// 3. Every non-addable target returns the identical ErrNotAddable (matching ErrPrincipalNotFound) and writes nothing.
	refusedTargets := []struct {
		name     string
		targetID int64
	}{
		{"human member outside manager's channels", f.unsharedMem.ID},
		{"plain operator in shared channel", f.operator.ID},
		{"owner in shared channel", f.owner.ID},
		{"agent in shared channel", f.agent.ID},
		{"active guest in shared channel", f.guest.ID},
		{"ended guest", f.endedGuest.ID},
		{"disabled human in shared channel", f.disabled.ID},
		{"hidden human in shared channel", f.hidden.ID},
		{"unknown principal ID", 99999},
	}
	// Remove operator, owner, agent from ch1 first so we test adding them when not already in ch1 (though they share ch2).
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch1.ID, f.operator.ID)
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch1.ID, f.owner.ID)
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch1.ID, f.agent.ID)

	for _, rt := range refusedTargets {
		t.Run("target_"+rt.name, func(t *testing.T) {
			beforeAudit := countRows(t, f.s, "audit_events")
			beforeMembers := countRows(t, f.s, "channel_members")
			added, err := f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, rt.targetID, f.manager.ID, nil)
			if added || !errors.Is(err, ErrNotAddable) || !errors.Is(err, ErrPrincipalNotFound) || err.Error() != ErrNotAddable.Error() {
				t.Fatalf("PutChannelMember(%s) = %v, %v; want false, ErrNotAddable", rt.name, added, err)
			}
			if countRows(t, f.s, "audit_events") != beforeAudit || countRows(t, f.s, "channel_members") != beforeMembers {
				t.Errorf("refused PutChannelMember(%s) mutated store", rt.name)
			}
		})
	}

	// 4. Manager passing can_decide or manages is refused with ErrManagerRefused before target lookup.
	for _, flags := range []struct {
		name      string
		canDecide *bool
		manages   *bool
	}{
		{"can_decide true", boolPtr(true), nil},
		{"can_decide false", boolPtr(false), nil},
		{"manages true", nil, boolPtr(true)},
		{"manages false", nil, boolPtr(false)},
	} {
		t.Run("body_"+flags.name, func(t *testing.T) {
			if _, err := f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, 99999, f.manager.ID, flags.canDecide, flags.manages); !errors.Is(err, ErrManagerRefused) {
				t.Errorf("manager PutChannelMember with %s = %v, want ErrManagerRefused", flags.name, err)
			}
		})
	}

	// 5. Actors without manager standing in ch1 are refused with ErrManagerRefused.
	for _, actor := range []struct {
		name string
		id   int64
	}{
		{"manager of ch2 only (member of ch2, not manager of ch1)", f.otherMgr.ID},
		{"plain member of ch1", f.approver.ID},
		{"guest", f.guest.ID},
		{"agent", f.agent.ID},
		{"disabled", f.disabled.ID},
		{"hidden", f.hidden.ID},
		{"unknown", 99999},
	} {
		t.Run("actor_"+actor.name, func(t *testing.T) {
			if _, err := f.s.PutChannelMember(ctx, principalActor(actor.id), f.ch1.ID, f.sharedMem.ID, actor.id, nil); !errors.Is(err, ErrManagerRefused) {
				t.Errorf("PutChannelMember by %s = %v, want ErrManagerRefused", actor.name, err)
			}
		})
	}
}

func TestAddChannelMemberByNameMatrix(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)
	ch3, err := f.s.CreateChannel(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	// Make manager a manager of ch3 as well.
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.owner.ID), ch3.ID, f.manager.ID, f.owner.ID, boolPtr(false), boolPtr(true)); err != nil {
		t.Fatal(err)
	}

	// 1. Manager adds sharedMem by name to ch3 -> added=true, can_decide=0, manages=0.
	cm, added, err := f.s.AddChannelMemberByName(ctx, principalActor(f.manager.ID), ch3.ID, f.manager.ID, f.sharedMem.Name)
	if err != nil || !added || cm.PrincipalID != f.sharedMem.ID || cm.CanDecide || cm.Manages || cm.AddedBy != f.manager.ID {
		t.Fatalf("manager AddChannelMemberByName(sharedMem) = %+v, %v, %v", cm, added, err)
	}

	// 2. Operator adds unsharedMem and operator by name to ch3 -> always inserts can_decide=0, manages=0.
	for _, target := range []Principal{f.unsharedMem, f.operator} {
		cm, added, err := f.s.AddChannelMemberByName(ctx, principalActor(f.owner.ID), ch3.ID, f.owner.ID, target.Name)
		if err != nil || !added || cm.PrincipalID != target.ID || cm.CanDecide || cm.Manages {
			t.Fatalf("owner AddChannelMemberByName(%s) = %+v, %v, %v; want CanDecide=false, Manages=false", target.Name, cm, added, err)
		}
	}
	// Remove unsharedMem from ch3 so it remains outside f.manager's channels for step 4.
	if _, err := f.s.RemoveChannelMember(ctx, "system", ch3.ID, f.unsharedMem.ID); err != nil {
		t.Fatal(err)
	}

	// 3. Idempotent on an existing approver/manager: returns added=false and preserves existing flags.
	if _, err := f.s.SetChannelMemberCanDecide(ctx, principalActor(f.owner.ID), ch3.ID, f.sharedMem.ID, true, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetChannelMemberManages(ctx, principalActor(f.owner.ID), ch3.ID, f.sharedMem.ID, true, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, f.s, "audit_events")
	cm, added, err = f.s.AddChannelMemberByName(ctx, principalActor(f.manager.ID), ch3.ID, f.manager.ID, f.sharedMem.Name)
	if err != nil || added || !cm.CanDecide || !cm.Manages {
		t.Fatalf("repeat AddChannelMemberByName(sharedMem) = %+v, %v, %v; want added=false with CanDecide=true, Manages=true", cm, added, err)
	}
	if countRows(t, f.s, "audit_events") != beforeAudit {
		t.Error("repeat AddChannelMemberByName wrote an audit event")
	}

	// 4. Every non-addable name returns the identical ErrNotAddable.
	refused := []struct {
		label   string
		actorID int64
		name    string
	}{
		{"manager: unknown name", f.manager.ID, "no-such-person"},
		{"manager: unshared member", f.manager.ID, f.unsharedMem.Name},
		{"manager: plain operator", f.manager.ID, f.operator.Name},
		{"manager: owner", f.manager.ID, f.owner.Name},
		{"manager: agent", f.manager.ID, f.agent.Name},
		{"manager: active guest", f.manager.ID, f.guest.Name},
		{"manager: ended guest", f.manager.ID, f.endedGuest.Name},
		{"manager: disabled human", f.manager.ID, f.disabled.Name},
		{"manager: hidden human", f.manager.ID, f.hidden.Name},
		{"operator: unknown name", f.operator.ID, "no-such-person"},
		{"operator: agent", f.operator.ID, f.agent.Name},
		{"operator: active guest", f.operator.ID, f.guest.Name},
		{"operator: ended guest", f.operator.ID, f.endedGuest.Name},
		{"operator: disabled human", f.operator.ID, f.disabled.Name},
		{"operator: hidden human", f.operator.ID, f.hidden.Name},
	}
	for _, tc := range refused {
		t.Run(tc.label, func(t *testing.T) {
			before := countRows(t, f.s, "audit_events")
			_, added, err := f.s.AddChannelMemberByName(ctx, principalActor(tc.actorID), f.ch1.ID, tc.actorID, tc.name)
			if added || !errors.Is(err, ErrNotAddable) || !errors.Is(err, ErrPrincipalNotFound) || err.Error() != ErrNotAddable.Error() {
				t.Fatalf("AddChannelMemberByName(%s) = %v, %v; want false, ErrNotAddable", tc.label, added, err)
			}
			if countRows(t, f.s, "audit_events") != before {
				t.Errorf("refused AddChannelMemberByName(%s) wrote an audit event", tc.label)
			}
		})
	}
}

func TestManagerRemoveChannelMemberMatrix(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)

	// 1. Non-member target is checked FIRST: returns removed=false, err=nil whatever the target's role or existence.
	nonMembers := []struct {
		name string
		id   int64
	}{
		{"unknown principal ID", 99999},
		{"non-member human", f.unsharedMem.ID},
		{"non-member guest", f.guest.ID},
	}
	// Also test a non-member operator, non-member owner, non-member agent, and non-member guest on ch2 after removing them from ch2.
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch2.ID, f.operator.ID)
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch2.ID, f.owner.ID)
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch2.ID, f.agent.ID)
	_, _ = f.s.RemoveChannelMember(ctx, "system", f.ch2.ID, f.guest.ID)
	for _, nm := range append(nonMembers,
		struct {
			name string
			id   int64
		}{"non-member operator", f.operator.ID},
		struct {
			name string
			id   int64
		}{"non-member owner", f.owner.ID},
		struct {
			name string
			id   int64
		}{"non-member agent", f.agent.ID},
	) {
		t.Run("non_member_"+nm.name, func(t *testing.T) {
			before := countRows(t, f.s, "audit_events")
			removed, err := f.s.RemoveChannelMember(ctx, principalActor(f.otherMgr.ID), f.ch2.ID, nm.id, f.otherMgr.ID)
			if err != nil || removed {
				t.Fatalf("manager RemoveChannelMember(%s) = %v, %v; want false, nil", nm.name, removed, err)
			}
			if countRows(t, f.s, "audit_events") != before {
				t.Errorf("non-member remove (%s) wrote audit event", nm.name)
			}
		})
	}

	// 2. Current member in ch1 who is an operator, owner, agent, or holds can_decide=1 -> ErrManagerRefused.
	protectedMembers := []struct {
		name string
		id   int64
	}{
		{"current operator", f.operator.ID},
		{"current owner", f.owner.ID},
		{"current agent", f.agent.ID},
		{"current approver (can_decide=1)", f.approver.ID},
	}
	for _, pm := range protectedMembers {
		t.Run("protected_"+pm.name, func(t *testing.T) {
			before := countRows(t, f.s, "audit_events")
			removed, err := f.s.RemoveChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, pm.id, f.manager.ID)
			if removed || !errors.Is(err, ErrManagerRefused) {
				t.Fatalf("manager RemoveChannelMember(%s) = %v, %v; want false, ErrManagerRefused", pm.name, removed, err)
			}
			if countRows(t, f.s, "audit_events") != before {
				t.Errorf("refused remove (%s) wrote audit event", pm.name)
			}
		})
	}

	// If a manager also holds can_decide=1, another manager (or self) cannot remove them via manager standing.
	if _, err := f.s.SetChannelMemberCanDecide(ctx, principalActor(f.operator.ID), f.ch1.ID, f.fellowMgr.ID, true, f.operator.ID); err != nil {
		t.Fatal(err)
	}
	if removed, err := f.s.RemoveChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, f.fellowMgr.ID, f.manager.ID); removed || !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("manager removing fellow manager who holds can_decide = %v, %v; want false, ErrManagerRefused", removed, err)
	}
	if _, err := f.s.SetChannelMemberCanDecide(ctx, principalActor(f.operator.ID), f.ch1.ID, f.fellowMgr.ID, false, f.operator.ID); err != nil {
		t.Fatal(err)
	}

	// 3. Manager can remove a plain member (can_decide=0), a fellow manager (can_decide=0), and themselves (even as last manager).
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, f.sharedMem.ID, f.manager.ID, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		name string
		id   int64
	}{
		{"plain member", f.sharedMem.ID},
		{"fellow manager", f.fellowMgr.ID},
		{"self as last manager", f.manager.ID},
	} {
		t.Run("allowed_"+target.name, func(t *testing.T) {
			removed, err := f.s.RemoveChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, target.id, f.manager.ID)
			if err != nil || !removed {
				t.Fatalf("manager RemoveChannelMember(%s) = %v, %v; want true, nil", target.name, removed, err)
			}
			if ok, _ := f.s.IsChannelMember(ctx, f.ch1.ID, target.id); ok {
				t.Errorf("%s is still a member of ch1 after removal", target.name)
			}
		})
	}
}

func TestInviteAndGuestLeaveManagesZero(t *testing.T) {
	ctx := context.Background()
	f := newManagerTestFixture(t)
	now := time.Now().Truncate(time.Millisecond)

	inv1, err := f.s.CreateInvite(ctx, principalActor(f.owner.ID), f.owner.ID, schema.CreateInviteRequestV1{
		Kind:     schema.InviteKindOpen,
		Channels: []string{f.ch1.Name},
	}, InviteOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	j1, err := f.s.RedeemInvite(ctx, HashInviteCode(inv1.Code), "joiner-token", now)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, j1.Principal.ID); err != nil || m {
		t.Errorf("RedeemInvite joiner ManagesChannel = %v, %v; want false", m, err)
	}

	inv2, err := f.s.CreateInvite(ctx, principalActor(f.owner.ID), f.owner.ID, schema.CreateInviteRequestV1{
		Kind:     schema.InviteKindOpen,
		Channels: []string{f.ch1.Name},
	}, InviteOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := f.s.RedeemInviteWithPassword(ctx, HashInviteCode(inv2.Code), "joiner-pw", testRecord("joiner"), now)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, j2.Principal.ID); err != nil || m {
		t.Errorf("RedeemInviteWithPassword joiner ManagesChannel = %v, %v; want false", m, err)
	}

	// Guest admission (via PutChannelMember) and conversion leave manages = 0.
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.owner.ID), f.ch1.ID, f.guest.ID, f.owner.ID, nil); err != nil {
		t.Fatal(err)
	}
	if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.guest.ID); err != nil || m {
		t.Errorf("guest ManagesChannel = %v, %v; want false", m, err)
	}
	permit, err := f.s.CreateConversionPermit(ctx, principalActor(f.owner.ID), f.owner.ID, f.guest.ID, schema.CreateConversionPermitRequestV1{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ConvertGuest(ctx, principalActor(f.guest.ID), GuestConversion{
		PrincipalID: f.guest.ID,
		Name:        "converted-guest",
		Record:      testRecord("converted"),
		PermitHash:  HashSignInLinkCode(permit.Code),
		Now:         now,
	}); err != nil {
		t.Fatal(err)
	}
	if m, err := f.s.ManagesChannel(ctx, f.ch1.ID, f.guest.ID); err != nil || m {
		t.Errorf("converted guest ManagesChannel = %v, %v; want false", m, err)
	}
}

func TestManagerAddConcurrentWithRevokeManages(t *testing.T) {
	ctx := context.Background()
	for range 10 {
		f := newManagerTestFixture(t)
		var wg sync.WaitGroup
		var addErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, addErr = f.s.PutChannelMember(ctx, principalActor(f.manager.ID), f.ch1.ID, f.sharedMem.ID, f.manager.ID, nil)
		}()
		go func() {
			defer wg.Done()
			_, revokeErr = f.s.SetChannelMemberManages(ctx, principalActor(f.operator.ID), f.ch1.ID, f.manager.ID, false, f.operator.ID)
		}()
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("SetChannelMemberManages(false) error: %v", revokeErr)
		}
		if addErr != nil && !errors.Is(addErr, ErrManagerRefused) {
			t.Fatalf("concurrent manager PutChannelMember error = %v, want nil or ErrManagerRefused", addErr)
		}
		// After the revoke commits, any subsequent add by the former manager must fail with ErrManagerRefused.
		if _, _, err := f.s.AddChannelMemberByName(ctx, principalActor(f.manager.ID), f.ch1.ID, f.manager.ID, f.sharedMem.Name); !errors.Is(err, ErrManagerRefused) {
			t.Fatalf("AddChannelMemberByName after revoke = %v, want ErrManagerRefused", err)
		}
	}
}
