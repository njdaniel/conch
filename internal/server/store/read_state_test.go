package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

var readStateT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func countAfter(t *testing.T, f *audienceFixture, r Reader, after int64, limit int) int {
	t.Helper()
	n, err := f.s.CountVisibleMessagesAfter(context.Background(), f.ch.ID, r, after, limit)
	if err != nil {
		t.Fatalf("CountVisibleMessagesAfter: %v", err)
	}
	return n
}

// The unread count is the visibility function's set, counted: for every kind
// of reader, it equals the length of what ListVisibleMessages returns past the
// mark, and a message the reader cannot see is not in it.
func TestCountVisibleMessagesAfter(t *testing.T) {
	f := newAudienceFixture(t)
	w1 := f.wide(t, f.frank, "wide 1")
	m1, _ := f.toNet(t, f.alice, f.n1, "net one")
	w2 := f.wide(t, f.frank, "wide 2")
	f.toNet(t, f.erin, f.n2, "net two")
	f.whisper(t, f.erin, "whisper", f.dave)
	f.wide(t, f.frank, "wide 3")

	tests := []struct {
		name   string
		reader Reader
		after  int64
		want   int
	}{
		{"net member, from the start", Reader{f.dave.ID, true}, 0, 5},
		{"net monitor", Reader{f.bob.ID, true}, 0, 4},
		{"on both nets", Reader{f.alice.ID, true}, 0, 5},
		{"whisper author", Reader{f.erin.ID, true}, 0, 5},
		{"on no net", Reader{f.frank.ID, true}, 0, 3},
		{"not a member", Reader{f.carol.ID, true}, 0, 3},
		{"no verified reader", Reader{0, true}, 0, 3},
		{"channel-wide only", ChannelWideOnly, 0, 3},
		{"v0/v1 reader of a net member", Reader{f.dave.ID, false}, 0, 3},
		{"past the first message", Reader{f.dave.ID, true}, w1, 4},
		{"past a net message", Reader{f.frank.ID, true}, m1, 2},
		{"past a message that is not theirs", Reader{f.frank.ID, true}, w2 + 1, 1},
		{"past everything", Reader{f.dave.ID, true}, 1 << 40, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countAfter(t, f, tt.reader, tt.after, 1000)
			if got != tt.want {
				t.Errorf("count = %d, want %d", got, tt.want)
			}
			msgs, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, tt.reader, tt.after, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != got {
				t.Errorf("count = %d but the list holds %d", got, len(msgs))
			}
		})
	}

	t.Run("limit", func(t *testing.T) {
		for _, lim := range []struct{ limit, want int }{{1, 1}, {3, 3}, {5, 5}, {6, 5}} {
			if got := countAfter(t, f, Reader{f.dave.ID, true}, 0, lim.limit); got != lim.want {
				t.Errorf("limit %d: count = %d, want %d", lim.limit, got, lim.want)
			}
		}
	})
	t.Run("bad arguments", func(t *testing.T) {
		if _, err := f.s.CountVisibleMessagesAfter(context.Background(), f.ch.ID, ChannelWideOnly, 0, 0); err == nil {
			t.Error("limit 0 was accepted")
		}
		if _, err := f.s.CountVisibleMessagesAfter(context.Background(), f.ch.ID, ChannelWideOnly, -1, 10); err == nil {
			t.Error("a negative cursor was accepted")
		}
	})
	t.Run("removed from the channel", func(t *testing.T) {
		if _, err := f.s.RemoveChannelMember(context.Background(), "system", f.ch.ID, f.dave.ID); err != nil {
			t.Fatal(err)
		}
		if got := countAfter(t, f, Reader{f.dave.ID, true}, 0, 1000); got != 3 {
			t.Errorf("count after removal = %d, want 3", got)
		}
	})
}

func TestMarkRead(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	w1 := f.wide(t, f.frank, "wide 1")
	f.toNet(t, f.alice, f.n1, "net one")
	w2 := f.wide(t, f.frank, "wide 2")
	hidden := f.whisper(t, f.erin, "whisper", f.dave) // frank cannot see this one
	frank := Reader{f.frank.ID, true}
	dave := Reader{f.dave.ID, true}

	mark := func(t *testing.T, r Reader, id int64, at time.Time) ReadMark {
		t.Helper()
		m, err := f.s.MarkRead(ctx, f.ch.ID, r, id, at)
		if err != nil {
			t.Fatalf("MarkRead(%d): %v", id, err)
		}
		return m
	}

	if m := mark(t, frank, 0, readStateT0); m.LastReadID != 0 || m.ChannelID != f.ch.ID {
		t.Errorf("mark 0 on a fresh channel = %+v, want an unmoved mark", m)
	}
	if marks, err := f.s.ReadMarksFor(ctx, f.frank.ID); err != nil || len(marks) != 0 {
		t.Errorf("a mark of 0 stored %v, %v", marks, err)
	}

	m := mark(t, frank, w1, readStateT0)
	if m.LastReadID != w1 || !m.UpdatedAt.Equal(readStateT0) {
		t.Errorf("first mark = %+v, want %d at %v", m, w1, readStateT0)
	}
	// Backward and equal marks change neither the id nor the time.
	later := readStateT0.Add(time.Minute)
	for _, id := range []int64{0, w1 - 1, w1} {
		if m = mark(t, frank, id, later); m.LastReadID != w1 || !m.UpdatedAt.Equal(readStateT0) {
			t.Errorf("mark %d after %d = %+v, want it unchanged", id, w1, m)
		}
	}
	if m = mark(t, frank, w2, later); m.LastReadID != w2 || !m.UpdatedAt.Equal(later) {
		t.Errorf("forward mark = %+v, want %d at %v", m, w2, later)
	}
	// A mark past the newest message frank may see is that message: not the
	// whisper's id, which would say a message he cannot see exists.
	if m = mark(t, frank, hidden+500, later.Add(time.Minute)); m.LastReadID != w2 {
		t.Errorf("mark past the end = %d, want %d (the newest visible)", m.LastReadID, w2)
	}
	// A reader who can see the whisper may mark it.
	if m = mark(t, dave, hidden+500, later); m.LastReadID != hidden {
		t.Errorf("dave's mark past the end = %d, want %d", m.LastReadID, hidden)
	}
	// Marks are per principal.
	marks, err := f.s.ReadMarksFor(ctx, f.dave.ID)
	if err != nil || len(marks) != 1 || marks[f.ch.ID].LastReadID != hidden {
		t.Errorf("dave's marks = %v, %v", marks, err)
	}
	if got := countAfter(t, f, frank, mark(t, frank, 0, later).LastReadID, 1000); got != 0 {
		t.Errorf("frank still has %d unread after reading to the end", got)
	}

	t.Run("refused", func(t *testing.T) {
		if _, err := f.s.MarkRead(ctx, f.ch.ID, ChannelWideOnly, 1, later); err == nil {
			t.Error("a reader with no principal was given a mark")
		}
		if _, err := f.s.MarkRead(ctx, f.ch.ID, frank, -1, later); err == nil {
			t.Error("a negative mark was accepted")
		}
	})
}

// A mark outlives membership, and a mark never reveals what the reader cannot
// see even when the channel holds nothing they can.
func TestMarkReadWithNothingVisible(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	f.whisper(t, f.erin, "only for dave", f.dave)
	m, err := f.s.MarkRead(ctx, f.ch.ID, Reader{f.frank.ID, true}, 99, readStateT0)
	if err != nil || m.LastReadID != 0 {
		t.Errorf("mark = %+v, %v; want 0 with nothing visible", m, err)
	}
}

func TestReadMarksMigrationFromSchema22(t *testing.T) {
	const pre = 22
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
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old', 5), (2, 1, 1, 'older', 6)`,
		`PRAGMA user_version = 22`,
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 23 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM read_marks").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("read_marks rows after migration = %d, %v; want 0", rows, err)
	}
	// Nothing was marked, so every old message is unread; marking works.
	r := Reader{PrincipalID: 1, Scoped: true}
	if n, err := s.CountVisibleMessagesAfter(ctx, 1, r, 0, 1000); err != nil || n != 2 {
		t.Errorf("unread after migration = %d, %v; want 2", n, err)
	}
	m, err := s.MarkRead(ctx, 1, r, 1, readStateT0)
	if err != nil || m.LastReadID != 1 {
		t.Fatalf("MarkRead after migration = %+v, %v", m, err)
	}
	if n, err := s.CountVisibleMessagesAfter(ctx, 1, r, m.LastReadID, 1000); err != nil || n != 1 {
		t.Errorf("unread after marking = %d, %v; want 1", n, err)
	}
}
