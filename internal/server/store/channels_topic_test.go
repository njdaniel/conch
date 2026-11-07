package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The channel topic (issue #1503, docs/design/channel-topic.md).

func lastTopicAudit(t *testing.T, s *Store) AuditEvent {
	t.Helper()
	e, err := s.LastAuditEvent(context.Background(), AuditChannelTopicChanged)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSetChannelTopic(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		// before is the topic the channel starts with.
		before, set string
		wantChanged bool
		wantDetail  string
	}{
		{"a first topic", "", "Release planning", true, `from="" to="Release planning"`},
		{"a change", "old", "new", true, `from="old" to="new"`},
		{"a clear", "old", "", true, `from="old" to=""`},
		{"the same string", "same", "same", false, ""},
		{"clearing a clear topic", "", "", false, ""},
		{"quoted so one field", "", `a to="b" c`, true, `from="" to="a to=\"b\" c"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ch, err := s.CreateChannel(ctx, "ops")
			if err != nil {
				t.Fatal(err)
			}
			if tt.before != "" {
				if _, err := s.SetChannelTopic(ctx, "system", 0, ch.ID, tt.before, now); err != nil {
					t.Fatal(err)
				}
			}
			audits := countRows(t, s, "audit_events")
			res, err := s.SetChannelTopic(ctx, "principal:9", 0, ch.ID, tt.set, now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed != tt.wantChanged || res.Channel.Topic != tt.set || res.From != tt.before {
				t.Errorf("result = %+v, want changed %v, topic %q, from %q", res, tt.wantChanged, tt.set, tt.before)
			}
			stored, err := s.ChannelByID(ctx, ch.ID)
			if err != nil || stored.Topic != tt.set || stored.ID != ch.ID || stored.Name != "ops" {
				t.Errorf("stored = %+v, %v", stored, err)
			}
			wantRows := boolCount(tt.wantChanged)
			if got := countRows(t, s, "audit_events") - audits; got != wantRows {
				t.Fatalf("%d audit rows written, want %d", got, wantRows)
			}
			if tt.wantChanged {
				e := lastTopicAudit(t, s)
				if e.Actor != "principal:9" || e.Subject != channelSubject(ch.ID) || e.Detail != tt.wantDetail {
					t.Errorf("audit = %+v, want actor principal:9, subject %s, detail %s", e, channelSubject(ch.ID), tt.wantDetail)
				}
			}
		})
	}
}

func TestSetChannelTopicUnknownChannel(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.SetChannelTopic(context.Background(), "system", 0, 9999, "x", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// The standing is decided in the write's own transaction: a grant revoked
// after the route's check is refused, and nothing is written.
func TestSetChannelTopicStandingRechecked(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := s.CreatePrincipal(ctx, PrincipalHuman, "manager")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := s.CreatePrincipal(ctx, PrincipalHuman, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	if _, err := s.PutChannelMember(ctx, "system", ch.ID, manager.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChannelTopic(ctx, "principal:2", manager.ID, ch.ID, "while a manager", time.Now()); err != nil {
		t.Fatalf("a manager sets the topic: %v", err)
	}
	if _, err := s.SetChannelMemberManages(ctx, "system", ch.ID, manager.ID, false); err != nil {
		t.Fatal(err)
	}
	audits := countRows(t, s, "audit_events")
	if _, err := s.SetChannelTopic(ctx, "principal:2", manager.ID, ch.ID, "after the revoke", time.Now()); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("revoked manager = %v, want ErrManagerRefused", err)
	}
	if _, err := s.SetChannelTopic(ctx, "principal:3", stranger.ID, ch.ID, "x", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member = %v, want ErrNotFound", err)
	}
	if stored, err := s.ChannelByID(ctx, ch.ID); err != nil || stored.Topic != "while a manager" {
		t.Errorf("stored = %+v, %v", stored, err)
	}
	if got := countRows(t, s, "audit_events"); got != audits {
		t.Errorf("the refusals wrote %d audit rows", got-audits)
	}
}

func TestSetChannelTopicOnAnArchivedChannel(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	setArchived(t, s, ch, true)
	res, err := s.SetChannelTopic(ctx, "system", 0, ch.ID, "kept for the record", time.Now())
	if err != nil || !res.Changed || !res.Channel.Archived() {
		t.Fatalf("archived channel: %+v, %v", res, err)
	}
}

// Every channel reader carries the topic: by id, by name, the whole list,
// the caller's channels and both directory views.
func TestChannelTopicInEveryReader(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	member, err := s.CreatePrincipal(ctx, PrincipalHuman, "member")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelMember(ctx, "system", ch.ID, member.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChannelTopic(ctx, "system", 0, ch.ID, "the topic", time.Now()); err != nil {
		t.Fatal(err)
	}
	byName, err := s.ChannelByName(ctx, "ops")
	if err != nil || byName.Topic != "the topic" {
		t.Errorf("ChannelByName = %+v, %v", byName, err)
	}
	all, err := s.ListChannels(ctx)
	if err != nil || findChannel(all, ch.ID).Topic != "the topic" {
		t.Errorf("ListChannels = %+v, %v", all, err)
	}
	mine, err := s.ListChannelsForPrincipal(ctx, member.ID)
	if err != nil || len(mine) != 1 || mine[0].Topic != "the topic" {
		t.Errorf("ListChannelsForPrincipal = %+v, %v", mine, err)
	}
	for _, scopeAll := range []bool{false, true} {
		dir, err := s.ChannelDirectory(ctx, member.ID, scopeAll)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, l := range dir {
			if l.ID == ch.ID {
				got = l.Topic
			}
		}
		if got != "the topic" {
			t.Errorf("ChannelDirectory(all=%v) topic = %q", scopeAll, got)
		}
	}
}

func findChannel(chs []Channel, id int64) Channel {
	for _, c := range chs {
		if c.ID == id {
			return c
		}
	}
	return Channel{}
}

// Migration 47 adds the column to a database that already has channels: they
// read an empty topic, and the migration writes no audit row.
func TestMigrationChannelTopic(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/conch.db"
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE channels DROP COLUMN topic"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", channelTopicMigration-1)); err != nil {
		t.Fatal(err)
	}
	audits := countRows(t, s, "audit_events")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStoreAt(t, path)
	stored, err := s.ChannelByID(ctx, ch.ID)
	if err != nil || stored.Topic != "" || stored.Name != "ops" {
		t.Fatalf("migrated channel = %+v, %v", stored, err)
	}
	if got := countRows(t, s, "audit_events"); got != audits {
		t.Errorf("the migration wrote %d audit rows", got-audits)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d, %v", version, err)
	}
}
