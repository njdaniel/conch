package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
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
