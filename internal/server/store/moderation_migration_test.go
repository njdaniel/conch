package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// moderationMigration is the schema version migration 45 produces (issue
// #849, docs/design/moderation.md §10.1): channel_bans and message_removals.
const moderationMigration = 45

// TestModerationMigrationFromSchema44: migration 45 adds the two moderation
// tables and nothing else. A database with history keeps every message,
// membership and guest attribution readable, byte for byte as before; the
// messages row is not touched; the new tables start empty; a removal, once
// written, can be neither changed nor deleted; and reopening the migrated
// database is a no-op.
func TestModerationMigrationFromSchema44(t *testing.T) {
	const handle = "guest-a7k4m2q9x3h5t8wz"
	seed := []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, owner, voice_allowed, may_invite, created_at) VALUES
			(1, 'human', 'owner', 'owner', 'owner', 'operator', 1, 1, 0, 1),
			(2, 'human', 'sam', 'sam', 'sam', 'member', 0, 1, 1, 2),
			(3, 'human', '` + handle + `', '` + handle + `', '` + handle + `', 'member', 0, 1, 0, 3)`,
		`INSERT INTO channels (id, name, name_key, name_skel, created_at) VALUES (1, 'general', 'general', 'general', 3)`,
		`INSERT INTO guest_visits (principal_id, handle, label, origin, state, started_at, expires_at, idle_ms)
			VALUES (3, '` + handle + `', 'Pat', 'address', 'active', 4, 9999999999999, 60000)`,
		// The guest was admitted after message 1, so its history bound is 1.
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, history_after) VALUES
			(1, 1, NULL, 4, NULL), (1, 2, 1, 4, NULL), (1, 3, NULL, 6, 1)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old message', 5)`,
		// Posted during the visit: the insert trigger of migration 31 stores
		// the attribution.
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (2, 1, 3, 'hello from a visit', 7)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at, thread_root_id) VALUES (3, 1, 2, 'agreed', 8, 1)`,
	}
	s, path := migrateTo(t, moderationMigration-1, seed)
	ctx := context.Background()

	readable := func(pass int) {
		t.Helper()
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version != moderationMigration {
			t.Errorf("pass %d: user_version = %d, %v; want %d", pass, version, err, moderationMigration)
		}
		// The member sees everything, as before.
		got, err := s.ListVisibleMessages(ctx, 1, Reader{PrincipalID: 1, Scoped: true}, 0, 10)
		if err != nil {
			t.Fatalf("pass %d: ListVisibleMessages: %v", pass, err)
		}
		if len(got) != 3 {
			t.Fatalf("pass %d: the member sees %d messages, want 3", pass, len(got))
		}
		for i, want := range []struct {
			id       int64
			body     string
			root     int64
			guest    bool
			authorID int64
		}{
			{1, "old message", 0, false, 1},
			{2, "hello from a visit", 0, true, 3},
			{3, "agreed", 1, false, 2},
		} {
			m := got[i]
			if m.ID != want.id || m.Body != want.body || m.ThreadRootID != want.root || m.AuthorID != want.authorID || !m.EditedAt.IsZero() || m.Payload != nil {
				t.Errorf("pass %d: message %d = %+v; want id %d, body %q, root %d, author %d, no edit, no payload", pass, i, m, want.id, want.body, want.root, want.authorID)
			}
			if (m.GuestAuthor != nil) != want.guest {
				t.Errorf("pass %d: message %d guest_author = %+v; want present = %v", pass, m.ID, m.GuestAuthor, want.guest)
			}
			if want.guest && m.GuestAuthor != nil {
				if want := (schema.GuestAttributionV1{Handle: handle, Label: "Pat", Tag: schema.GuestTag(handle)}); *m.GuestAuthor != want {
					t.Errorf("pass %d: message %d guest_author = %+v; want %+v", pass, m.ID, *m.GuestAuthor, want)
				}
			}
		}
		// The guest's history bound still holds: after message 1, and a
		// reply only when its root is after the bound too.
		got, err = s.ListVisibleMessages(ctx, 1, Reader{PrincipalID: 3, Scoped: true}, 0, 10)
		if err != nil {
			t.Fatalf("pass %d: the guest's ListVisibleMessages: %v", pass, err)
		}
		if len(got) != 1 || got[0].ID != 2 {
			t.Errorf("pass %d: the guest sees %+v; want message 2 alone", pass, got)
		}
		members, err := s.ListChannelMembers(ctx, 1)
		if err != nil {
			t.Fatalf("pass %d: ListChannelMembers: %v", pass, err)
		}
		if len(members) != 3 {
			t.Fatalf("pass %d: %d members, want 3", pass, len(members))
		}
		for _, m := range members {
			isGuest := m.PrincipalID == 3
			if (m.Guest != nil) != isGuest {
				t.Errorf("pass %d: member %d guest = %+v; want present = %v", pass, m.PrincipalID, m.Guest, isGuest)
			}
			if isGuest && (m.HistoryAfter != 1 || m.Guest == nil || m.Guest.Handle != handle || m.Guest.Label != "Pat") {
				t.Errorf("pass %d: the guest member = %+v; want history_after 1 and its attribution", pass, m)
			}
			if !isGuest && m.HistoryAfter != 0 {
				t.Errorf("pass %d: member %d history_after = %d; want none", pass, m.PrincipalID, m.HistoryAfter)
			}
		}
		// The messages row itself was not rewritten.
		var body string
		var editedAt sql.NullInt64
		if err := s.db.QueryRow("SELECT body, edited_at FROM messages WHERE id = 1").Scan(&body, &editedAt); err != nil || body != "old message" || editedAt.Valid {
			t.Errorf("pass %d: messages row 1 = %q, %v, %v; want the old body and no edited_at", pass, body, editedAt, err)
		}
	}

	readable(1)
	if n := countRows(t, s, "channel_bans"); n != 0 {
		t.Errorf("channel_bans has %d rows after the upgrade, want none", n)
	}
	if n := countRows(t, s, "message_removals"); n != 0 {
		t.Errorf("message_removals has %d rows after the upgrade, want none", n)
	}

	// The tables take the rows the design describes, keyed as it says.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (1, 2, 1, 9)`); err != nil {
		t.Fatalf("a ban: %v", err)
	}
	// The composite primary key refuses a repeat (SQLITE_CONSTRAINT_PRIMARYKEY,
	// not the UNIQUE code isUniqueConstraintErr matches).
	if _, err := s.db.ExecContext(ctx, `INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (1, 2, 1, 10)`); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: channel_bans.channel_id, channel_bans.principal_id") {
		t.Errorf("a second ban of the same principal in the same channel: %v, want the primary key's refusal", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (1, 3, NULL, 11)`); err != nil {
		t.Errorf("a ban with authentication off (no created_by): %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO message_removals (message_id, channel_id, removed_by, removed_at) VALUES (2, 1, 1, 12)`); err != nil {
		t.Fatalf("a removal: %v", err)
	}
	// A removal is permanent: no update, no delete.
	for _, stmt := range []string{
		`UPDATE message_removals SET removed_at = 13 WHERE message_id = 2`,
		`UPDATE message_removals SET removed_by = NULL WHERE message_id = 2`,
		`DELETE FROM message_removals WHERE message_id = 2`,
		`DELETE FROM message_removals`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err == nil || !strings.Contains(err.Error(), "message removals are permanent") {
			t.Errorf("%s: %v, want the trigger's refusal", stmt, err)
		}
	}
	if n := countRows(t, s, "message_removals"); n != 1 {
		t.Errorf("message_removals has %d rows, want the one written", n)
	}
	// A removal names the message's row and never changes it (migration 38's
	// trigger still guards every column but body and edited_at).
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET channel_id = 1, author_id = 1 WHERE id = 2`); err == nil || !strings.Contains(err.Error(), "only a message body and edited_at may change") {
		t.Errorf("rewriting a removed message's row: %v, want the trigger's refusal", err)
	}

	// Reopening the migrated database changes nothing.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	readable(2)
	if n := countRows(t, s, "channel_bans"); n != 2 {
		t.Errorf("channel_bans has %d rows after reopening, want 2", n)
	}
	if n := countRows(t, s, "message_removals"); n != 1 {
		t.Errorf("message_removals has %d rows after reopening, want 1", n)
	}
}

// TestModerationTablesOnAFreshStore: a new database has the moderation tables
// too, empty, and the same triggers.
func TestModerationTablesOnAFreshStore(t *testing.T) {
	s := openFreshTestStore(t)
	for _, table := range []string{"channel_bans", "message_removals"} {
		if n := countRows(t, s, table); n != 0 {
			t.Errorf("%s has %d rows on a fresh store, want none", table, n)
		}
	}
	var triggers int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name IN ('message_removals_no_update', 'message_removals_no_delete')`).Scan(&triggers); err != nil || triggers != 2 {
		t.Errorf("message_removals triggers = %d, %v; want both", triggers, err)
	}
}
