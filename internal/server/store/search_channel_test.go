package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// matchRows counts the rows FTS5 itself returns for a match, before the outer
// statement keeps the channel or the visibility rule: the rows a search has to
// visit.
func matchRows(t *testing.T, s *Store, match string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, match).Scan(&n); err != nil {
		t.Fatalf("match %q: %v", match, err)
	}
	return n
}

// The cost of a search in one channel must not grow with the messages of
// other channels (issue #235). It is counted in rows, not time: the rows FTS5
// returns for the query the search builds are exactly the channel's matches,
// however many messages elsewhere contain the word. The plain words, which is
// what the index was asked before it knew the channel, grow with them.
func TestSearchCostIndependentOfOtherChannels(t *testing.T) {
	ctx := context.Background()
	const inChannel = 7
	for _, other := range []int{0, 40, 400} {
		t.Run(fmt.Sprintf("%d elsewhere", other), func(t *testing.T) {
			f := newAudienceFixture(t)
			for i := 0; i < inChannel; i++ {
				if _, err := f.s.InsertMessage(ctx, f.ch.ID, f.frank.ID, "common word here"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < other; i++ {
				if _, err := f.s.InsertMessage(ctx, f.other.ID, f.alice.ID, "common word there"); err != nil {
					t.Fatal(err)
				}
			}
			words := []string{"common", "word"}
			if got := matchRows(t, f.s, ftsInChannel(f.ch.ID, words)); got != inChannel {
				t.Errorf("channel match returns %d rows, want %d whatever other channels hold", got, inChannel)
			}
			if got := matchRows(t, f.s, ftsAllWords(words)); got != inChannel+other {
				t.Errorf("unscoped match returns %d rows, want %d (the control: the whole corpus)", got, inChannel+other)
			}
			if got := searchIDs(t, f.s, f.ch.ID, Reader{f.frank.ID, true}, words, 0, 50); len(got) != inChannel {
				t.Errorf("search found %d messages, want %d", len(got), inChannel)
			}
		})
	}
}

// A caller's words are confined to the body column: a word that spells the
// channel token or a column filter matches nothing, and the channel clause is
// built from the id alone.
func TestSearchChannelTokenNotReachableFromWords(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	if _, err := f.s.InsertMessage(ctx, f.ch.ID, f.frank.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	token := fmt.Sprintf("c%d", f.ch.ID)
	tests := []struct {
		name  string
		words []string
	}{
		{"the channel token as a word", []string{token}},
		{"a column filter", []string{"fts_channel:" + token}},
		{"a quote closing the phrase", []string{`x" OR fts_channel : "` + token}},
		{"a parenthesis", []string{`hello) OR (fts_channel : ` + token}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := searchIDs(t, f.s, f.ch.ID, Reader{f.frank.ID, true}, tt.words, 0, 50); len(got) != 0 {
				t.Errorf("found %v, want nothing", got)
			}
		})
	}
	if got := ftsInChannel(5, nil); got != "" {
		t.Errorf("ftsInChannel with no words = %q, want empty", got)
	}
	if got, want := ftsInChannel(5, []string{"a", `b"c`}), `fts_channel : "c5" AND body : ("a" "b""c")`; got != want {
		t.Errorf("ftsInChannel = %q, want %q", got, want)
	}
}

// Migration 22 over a database at schema 21 that holds messages in two
// channels, a whisper among them: each is findable in its own channel and not
// in the other, and a message posted afterwards is indexed with its channel.
func TestSearchMigrationFromSchema21(t *testing.T) {
	const pre = 21
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
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'sam', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3), (2, 'ops', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4), (2, 1, NULL, 4), (2, 2, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old needle', 5), (2, 2, 1, 'old needle elsewhere', 6), (3, 2, 1, 'only ops', 7)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at, audience_kind) VALUES (4, 1, 1, 'old needle whisper', 8, 'principals')`,
		`INSERT INTO message_recipients (message_id, principal_id) VALUES (4, 1), (4, 2)`,
		`PRAGMA user_version = 21`,
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
	r := Reader{2, true}
	for _, tt := range []struct {
		name    string
		channel int64
		words   []string
		want    []int64
	}{
		{"first channel", 1, []string{"needle"}, []int64{4, 1}},
		{"second channel", 2, []string{"needle"}, []int64{2}},
		{"a word of the second channel, searched in the first", 1, []string{"ops"}, []int64{}},
	} {
		if got := searchIDs(t, s, tt.channel, r, tt.words, 0, 50); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: after migration got %v, want %v", tt.name, got, tt.want)
		}
	}
	if got := matchRows(t, s, ftsInChannel(2, []string{"needle"})); got != 1 {
		t.Errorf("index rows for the second channel = %d, want 1", got)
	}
	m, err := s.InsertMessage(ctx, 2, 1, "fresh needle")
	if err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, 2, r, []string{"needle"}, 0, 50); !reflect.DeepEqual(got, []int64{m.ID, 2}) {
		t.Errorf("after a post got %v, want the new message first", got)
	}
	if got := searchIDs(t, s, 1, r, []string{"fresh"}, 0, 50); len(got) != 0 {
		t.Errorf("a message of channel 2 found in channel 1: %v", got)
	}
}
