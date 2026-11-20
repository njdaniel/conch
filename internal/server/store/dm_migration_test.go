package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// dmObjectNames lists the tables migration 53 makes.
func dmObjectNames(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND (name LIKE 'dm\_%' ESCAPE '\' OR name LIKE 'direct\_message%' ESCAPE '\')
		  AND name NOT LIKE 'direct\_messages\_fts\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

var wantDMTables = []string{
	"direct_message_removals", "direct_message_revisions", "direct_messages", "direct_messages_fts",
	"dm_conversations", "dm_files", "dm_read_marks", "dm_viewer_state",
}

// dmDropStatements undo migration 51, children before parents. The tests
// that rewind a database to an older version to run a migration on it (the
// topic, community, issued-by and author-removal ones) run them first, as each
// later migration's author has added theirs: the rewind must leave no object
// of a migration the old version has not made.
var dmDropStatements = []string{
	"DROP TABLE direct_messages_fts", // migration 53 (issue #2017)
	"DROP TABLE dm_read_marks",
	"DROP TABLE dm_files",
	"DROP TABLE direct_message_removals",
	"DROP TABLE direct_message_revisions",
	"DROP TABLE direct_messages",
	"DROP TABLE dm_viewer_state",
	"DROP TABLE dm_conversations",
}

// dropDMObjects returns a migrated database to its state before migration 53:
// the migration only adds tables, so removing them (and the triggers that go
// with them) and the version is that state.
func dropDMObjects(t *testing.T, s *Store, version int) {
	t.Helper()
	for _, stmt := range dmDropStatements {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := s.db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from) // #nosec G304 -- a test database this test made
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) // #nosec G304 -- a path in this test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// dmSnapshot is what a reader sees of every DM pair and message, so a reopen
// or a restored copy can be compared whole.
func dmSnapshot(t *testing.T, s *Store, viewers ...Principal) map[int64][]DMMessage {
	t.Helper()
	ctx := context.Background()
	out := map[int64][]DMMessage{}
	for _, v := range viewers {
		convs, err := s.ListDMConversations(ctx, v.ID, schema.DMStateOpen, DMListCursor{}, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			msgs, err := s.ListDMMessagesPage(ctx, v.ID, c.ID, MessagePage{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			out[v.ID*1000+c.ID] = msgs
		}
	}
	return out
}

// The migration is purely additive: a database from before it keeps its
// channels, its messages and its whispers exactly, gains the DM tables empty,
// and then holds DMs; reopening changes nothing, and a restored copy of the
// file (a backup, invalidated as conchd restore does) has the same pairs.
func TestDMMigrationUpgradeReopenAndRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var people [4]Principal
	for i, name := range []string{"alice", "bob", "carol", "dave"} {
		if people[i], err = s.CreatePrincipal(ctx, PrincipalHuman, name); err != nil {
			t.Fatal(err)
		}
	}
	alice, bob, carol, dave := people[0], people[1], people[2], people[3]
	// A channel of two members (a one-on-one the old way), an open message
	// and a whisper. These are what must not move.
	ch, err := s.CreateChannel(ctx, "alice-and-bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{alice, bob, carol} {
		if _, err := s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	open, err := s.InsertMessageV1(ctx, ch.ID, alice.ID, "to the channel", nil)
	if err != nil {
		t.Fatal(err)
	}
	whisper, _, err := s.InsertScopedMessage(ctx, ScopedPost{
		ChannelID: ch.ID, AuthorID: alice.ID, Body: "just us",
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{bob.ID}}.Normalize(alice.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.ListChannelMembers(ctx, ch.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("members before = %v, %v", members, err)
	}
	read := func(s *Store, who Principal) []int64 {
		return visibleIDs(t, s, ch.ID, Reader{PrincipalID: who.ID, Scoped: true})
	}
	beforeBob, beforeCarol := read(s, bob), read(s, carol)
	wantBob := []int64{open.ID, whisper.ID}
	if !reflect.DeepEqual(beforeBob, wantBob) || !reflect.DeepEqual(beforeCarol, []int64{open.ID}) {
		t.Fatalf("seed reads: bob %v carol %v", beforeBob, beforeCarol)
	}

	// Make it a database from before migration 53.
	preDM := len(migrations) - 1
	dropDMObjects(t, s, preDM)
	if got := dmObjectNames(t, s); len(got) != 0 {
		t.Fatalf("after the drop: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Upgrade by opening.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (upgrade): %v", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	if got := dmObjectNames(t, s); !reflect.DeepEqual(got, wantDMTables) {
		t.Fatalf("DM tables = %v, want %v", got, wantDMTables)
	}
	for _, table := range wantDMTables {
		if table == "direct_messages_fts" {
			continue
		}
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows after the upgrade (%v), want none", table, n, err)
		}
	}
	if got := read(s, bob); !reflect.DeepEqual(got, beforeBob) {
		t.Errorf("bob reads %v after the upgrade, want %v", got, beforeBob)
	}
	if got := read(s, carol); !reflect.DeepEqual(got, beforeCarol) {
		t.Errorf("carol reads %v after the upgrade, want %v (the whisper stays whispered)", got, beforeCarol)
	}
	if got, err := s.ListChannelMembers(ctx, ch.ID); err != nil || len(got) != 3 {
		t.Errorf("members after = %v, %v", got, err)
	}

	// The upgraded database holds DMs, and they do not touch the channel.
	step := now
	tick := func() time.Time { step = step.Add(time.Second); return step }
	ab, err := s.OpenDM(ctx, alice.ID, bob.ID, tick())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenDM(ctx, bob.ID, alice.ID, tick()); err != nil {
		t.Fatal(err)
	}
	cd, err := s.OpenDM(ctx, carol.ID, dave.ID, tick())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []DMPost{
		{ConversationID: ab.ID, AuthorID: alice.ID, Body: "dm one", ClientNonce: "a"},
		{ConversationID: ab.ID, AuthorID: bob.ID, Body: "dm two", ClientNonce: "b"},
		{ConversationID: cd.ID, AuthorID: carol.ID, Body: "dm three", ClientNonce: "c"},
	} {
		if _, _, err := s.InsertDMMessage(ctx, p, tick()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := read(s, bob); !reflect.DeepEqual(got, beforeBob) {
		t.Errorf("a DM changed what bob reads in the channel: %v", got)
	}
	var channelMessages int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&channelMessages); err != nil || channelMessages != 2 {
		t.Errorf("messages = %d (%v), want the 2 of the channel", channelMessages, err)
	}
	want := dmSnapshot(t, s, alice, bob, carol, dave)
	if len(want) != 4 {
		t.Fatalf("snapshot = %d conversations-by-viewer, want 4", len(want))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A copy of the file is a backup; restoring it invalidates credentials
	// (InvalidateRestored) and nothing else of a pair or a message.
	restored := filepath.Join(t.TempDir(), "restored.db")
	copyFile(t, path, restored)

	// Reopening is a no-op: the same pairs, messages and ids.
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (reopen): %v", err)
	}
	if got := dmSnapshot(t, again, alice, bob, carol, dave); !reflect.DeepEqual(got, want) {
		t.Errorf("after a reopen:\n got %v\nwant %v", got, want)
	}
	// The same pair opened again is the same conversation.
	if c, err := again.OpenDM(ctx, bob.ID, alice.ID, tick()); err != nil || c.ID != ab.ID {
		t.Errorf("reopen then open = %+v, %v; want conversation %d", c, err, ab.ID)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}

	rs, err := Open(ctx, restored)
	if err != nil {
		t.Fatalf("Open (restored copy): %v", err)
	}
	defer func() { _ = rs.Close() }()
	if _, err := rs.InvalidateRestored(ctx, tick()); err != nil {
		t.Fatalf("InvalidateRestored: %v", err)
	}
	if got := dmSnapshot(t, rs, alice, bob, carol, dave); !reflect.DeepEqual(got, want) {
		t.Errorf("restored copy:\n got %v\nwant %v", got, want)
	}
	if c, err := rs.OpenDM(ctx, dave.ID, carol.ID, tick()); err != nil || c.ID != cd.ID {
		t.Errorf("restored copy opens = %+v, %v; want the same pair and id %d", c, err, cd.ID)
	}
	var pairs int
	if err := rs.db.QueryRow("SELECT COUNT(*) FROM dm_conversations").Scan(&pairs); err != nil || pairs != 2 {
		t.Errorf("restored pairs = %d (%v), want 2", pairs, err)
	}
	// And it still searches: the index came with the file.
	if hits, err := rs.SearchDMMessages(ctx, bob.ID, ab.ID, []string{"two"}, 0, 5); err != nil || len(hits) != 1 {
		t.Errorf("restored search = %v, %v", hits, err)
	}
}

// A migrated-from-nothing database has the DM tables too, and the trigger and
// index text of the migration is what a database in the field gets.
func TestDMMigrationFreshDatabase(t *testing.T) {
	s := openFreshTestStore(t)
	if got := dmObjectNames(t, s); !reflect.DeepEqual(got, wantDMTables) {
		t.Errorf("DM tables = %v, want %v", got, wantDMTables)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'dm_conversations'`).Scan(&n); err != nil || n < 2 {
		t.Errorf("dm_conversations has %d indexes (%v); want the unique pair index and the lookup by hi", n, err)
	}
}
