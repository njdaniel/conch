package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// A database written before issue #450 keeps today's behaviour: every
// existing membership may decide its channel's approvals after the upgrade.
func TestDecideRightMigrationKeepsEveryExistingMember(t *testing.T) {
	const pre = 23
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'ann', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (10, 'ops', 3), (11, 'side', 4)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (10, 1, 1, 5), (10, 2, 1, 6), (11, 2, 1, 7)`,
		`PRAGMA user_version = 23`,
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
	t.Cleanup(func() { _ = s.Close() })
	for _, m := range []struct{ channel, principal int64 }{{10, 1}, {10, 2}, {11, 2}} {
		can, err := s.CanDecideApprovals(ctx, m.channel, m.principal)
		if err != nil {
			t.Fatal(err)
		}
		if !can {
			t.Errorf("member %d of channel %d lost the right in the upgrade", m.principal, m.channel)
		}
	}
	if can, err := s.CanDecideApprovals(ctx, 11, 1); err != nil || can {
		t.Errorf("a non-member can decide = %v, %v; want false", can, err)
	}
}

// PutChannelMember adds a member with or without the right in one step, and
// SetChannelMemberCanDecide changes it for a member and refuses a
// non-member.
func TestPutChannelMemberWithTheDecideRight(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := s.CreatePrincipal(ctx, PrincipalHuman, "ann")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if added, err := s.PutChannelMember(ctx, "system", ch.ID, ann.ID, 0, &no); err != nil || !added {
		t.Fatalf("add without the right = %v, %v", added, err)
	}
	if can, _ := s.CanDecideApprovals(ctx, ch.ID, ann.ID); can {
		t.Error("a member added without the right holds it")
	}
	if _, err := s.SetChannelMemberCanDecide(ctx, "system", ch.ID, bob.ID, true); !errors.Is(err, ErrNotMember) {
		t.Errorf("grant to a non-member = %v, want ErrNotMember", err)
	}
	if changed, err := s.SetChannelMemberCanDecide(ctx, "system", ch.ID, ann.ID, true); err != nil || !changed {
		t.Fatalf("grant = %v, %v", changed, err)
	}
	if can, _ := s.CanDecideApprovals(ctx, ch.ID, ann.ID); !can {
		t.Error("the grant did not take")
	}
	deciding, err := s.DecidingChannels(ctx, ann.ID)
	if err != nil || !deciding[ch.ID] || len(deciding) != 1 {
		t.Errorf("deciding channels = %v, %v; want only %d", deciding, err, ch.ID)
	}
	members, err := s.ListChannelMembers(ctx, ch.ID)
	if err != nil || len(members) != 1 || !members[0].CanDecide {
		t.Errorf("members = %+v, %v", members, err)
	}
}
