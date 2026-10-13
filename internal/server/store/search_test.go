package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDriverHasFTS5 proves the pinned SQLite driver was built with FTS5, which
// message search (issue #22) depends on. If this fails there is no search to
// build without a new dependency or a build tag, neither of which is allowed.
func TestDriverHasFTS5(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE fts_probe USING fts5(body)`); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO fts_probe (body) VALUES ('hello world')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fts_probe WHERE fts_probe MATCH '"hello"'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("match = %d, %v; want 1", n, err)
	}
}

func searchIDs(t *testing.T, s *Store, channelID int64, r Reader, words []string, before int64, limit int) []int64 {
	t.Helper()
	msgs, err := s.SearchVisibleMessages(context.Background(), channelID, r, words, before, limit)
	if err != nil {
		t.Fatalf("SearchVisibleMessages(%q): %v", words, err)
	}
	ids := []int64{}
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestFTSAllWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  string
	}{
		{"none", nil, ""},
		{"one", []string{"alpha"}, `"alpha"`},
		{"several, each quoted, all required", []string{"alpha", "beta"}, `"alpha" "beta"`},
		{"quote doubled", []string{`a"b`}, `"a""b"`},
		{"operators stay inside the string", []string{"NEAR(a", "b)", "OR", "-x", "body:y", "*"}, `"NEAR(a" "b)" "OR" "-x" "body:y" "*"`},
		{"NUL separates words", []string{"a\x00b"}, `"a" "b"`},
		{"only NUL", []string{"\x00"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ftsAllWords(tt.words); got != tt.want {
				t.Errorf("ftsAllWords(%q) = %q, want %q", tt.words, got, tt.want)
			}
		})
	}
}

func TestSearchVisibleMessages(t *testing.T) {
	f := newAudienceFixture(t)
	a := f.wide(t, f.frank, "Deploy the Red Falcon tonight")
	b := f.wide(t, f.frank, "falcon is red, deploy later")
	c := f.wide(t, f.frank, "unrelated chatter")
	d := f.wide(t, f.frank, "Naïve café ÉCLAIR")
	e := f.wide(t, f.frank, "deployment of falcons")
	// A message in another channel never matches.
	if _, err := f.s.InsertMessage(context.Background(), f.other.ID, f.alice.ID, "deploy falcon elsewhere"); err != nil {
		t.Fatal(err)
	}
	r := Reader{f.frank.ID, true}
	tests := []struct {
		name  string
		words []string
		want  []int64
	}{
		{"one word", []string{"deploy"}, []int64{b, a}},
		{"case-insensitive", []string{"DEPLOY"}, []int64{b, a}},
		{"several words, any order", []string{"red", "falcon"}, []int64{b, a}},
		{"every word required", []string{"deploy", "unrelated"}, []int64{}},
		{"no match", []string{"zebra"}, []int64{}},
		{"no words", nil, []int64{}},
		{"non-ASCII, case-folded", []string{"éclair"}, []int64{d}},
		{"diacritics folded", []string{"naive"}, []int64{d}},
		{"whole words: a plural is another word", []string{"falcons"}, []int64{e}},
		{"unrelated", []string{"chatter"}, []int64{c}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := searchIDs(t, f.s, f.ch.ID, r, tt.words, 0, 50); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchVisibleMessagesPaging(t *testing.T) {
	f := newAudienceFixture(t)
	var all []int64
	for i := 0; i < 7; i++ {
		all = append([]int64{f.wide(t, f.frank, "needle")}, all...)
		f.wide(t, f.frank, "hay")
	}
	r := Reader{f.frank.ID, true}
	var paged []int64
	before := int64(0)
	for i := 0; i < 10; i++ {
		page := searchIDs(t, f.s, f.ch.ID, r, []string{"needle"}, before, 3)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		before = page[len(page)-1]
	}
	if !reflect.DeepEqual(paged, all) {
		t.Errorf("paging by before returned %v, want every match once, newest first: %v", paged, all)
	}
	if _, err := f.s.SearchVisibleMessages(context.Background(), f.ch.ID, r, []string{"x"}, 0, 0); err == nil {
		t.Error("limit 0 was accepted")
	}
}

// A search shows a reader what listing shows them: for every reader and every
// kind of message, the matches are exactly the listed messages containing the
// word, newest first.
func TestSearchMatchesListVisibility(t *testing.T) {
	f := newAudienceFixture(t)
	f.wide(t, f.frank, "needle wide 1")
	f.toNet(t, f.alice, f.n1, "needle net one")
	f.wide(t, f.frank, "hay")
	f.toNet(t, f.erin, f.n2, "needle net two")
	f.whisper(t, f.erin, "needle whisper", f.dave)
	f.wide(t, f.frank, "needle wide 2")
	for _, r := range []Reader{
		{f.alice.ID, true}, {f.dave.ID, true}, {f.bob.ID, true}, {f.erin.ID, true}, {f.frank.ID, true},
		{f.carol.ID, true}, {0, true}, {f.dave.ID, false}, {f.erin.ID, false}, ChannelWideOnly, {9999, true},
	} {
		listed, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, r, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{}
		for i := len(listed) - 1; i >= 0; i-- {
			if strings.Contains(listed[i].Body, "needle") {
				want = append(want, listed[i].ID)
			}
		}
		if got := searchIDs(t, f.s, f.ch.ID, r, []string{"needle"}, 0, 100); !reflect.DeepEqual(got, want) {
			t.Errorf("reader %+v: search = %v, list-and-filter = %v", r, got, want)
		}
	}
}

// A hidden message must not change the page a reader gets: not its content,
// not whether it is full, not the cursor it would be followed by.
func TestSearchHiddenMessageDoesNotShiftPages(t *testing.T) {
	f := newAudienceFixture(t)
	w1 := f.wide(t, f.frank, "needle 1")
	f.toNet(t, f.alice, f.n1, "needle hidden")
	w2 := f.wide(t, f.frank, "needle 2")
	r := Reader{f.frank.ID, true}
	if got, want := searchIDs(t, f.s, f.ch.ID, r, []string{"needle"}, 0, 2), []int64{w2, w1}; !reflect.DeepEqual(got, want) {
		t.Errorf("page of 2 = %v, want %v (the hidden message must not take a slot)", got, want)
	}
	if got, want := searchIDs(t, f.s, f.ch.ID, r, []string{"needle"}, w2, 1), []int64{w1}; !reflect.DeepEqual(got, want) {
		t.Errorf("page before %d = %v, want %v", w2, got, want)
	}
}

// Migration 15 over a database that already holds messages, scoped ones
// included: every one is findable by the reader who may see it afterwards, and
// a message posted after the migration is findable at once.
func TestSearchMigrationFromSchema14(t *testing.T) {
	const pre = 14
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
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'sam', 'member', 2), (3, 'human', 'kim', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4), (1, 3, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old needle', 5)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at, audience_kind) VALUES (2, 1, 1, 'old needle whisper', 6, 'principals')`,
		`INSERT INTO message_recipients (message_id, principal_id) VALUES (2, 1), (2, 2)`,
		`PRAGMA user_version = 14`,
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
	word := []string{"needle"}
	for _, tt := range []struct {
		name string
		r    Reader
		want []int64
	}{
		{"whisper recipient", Reader{2, true}, []int64{2, 1}},
		{"whisper author", Reader{1, true}, []int64{2, 1}},
		{"not a recipient", Reader{3, true}, []int64{1}},
		{"v1 reader", Reader{2, false}, []int64{1}},
	} {
		if got := searchIDs(t, s, 1, tt.r, word, 0, 50); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: after migration got %v, want %v", tt.name, got, tt.want)
		}
	}
	m, err := s.InsertMessage(ctx, 1, 1, "fresh needle")
	if err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, 1, Reader{3, true}, word, 0, 50); !reflect.DeepEqual(got, []int64{m.ID, 1}) {
		t.Errorf("after a post got %v, want the new message first", got)
	}
}
