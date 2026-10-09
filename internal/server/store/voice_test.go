package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
)

// roomNamePattern is "conch-" plus 128 bits in unpadded lower-case base32:
// ceil(128/5) = 26 characters.
var roomNamePattern = regexp.MustCompile(`^conch-[a-z2-7]{26}$`)

func TestNewVoiceRoomName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		name, err := newVoiceRoomName()
		if err != nil {
			t.Fatal(err)
		}
		if !roomNamePattern.MatchString(name) {
			t.Fatalf("name %q does not match %s", name, roomNamePattern)
		}
		if seen[name] {
			t.Fatalf("name %q repeated", name)
		}
		seen[name] = true
	}
}

func TestChannelVoiceRoom(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a, err := s.CreateChannel(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateChannel(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.ChannelVoiceRoom(ctx, a.ID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := s.ChannelVoiceRoom(ctx, a.ID)
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	other, err := s.ChannelVoiceRoom(ctx, b.ID)
	if err != nil {
		t.Fatalf("other channel: %v", err)
	}

	tests := []struct {
		name string
		ok   bool
	}{
		{"name has the conch-<26 base32> shape", roomNamePattern.MatchString(first.RoomName)},
		{"second call returns the same row", again == first},
		{"row names its channel with no net", first.ChannelID == a.ID && first.NetID == 0},
		{"another channel gets a different room", other.RoomName != first.RoomName && other.ID != first.ID},
		{"created_at is set", !first.CreatedAt.IsZero()},
	}
	for _, tt := range tests {
		if !tt.ok {
			t.Errorf("%s: got first=%+v again=%+v other=%+v", tt.name, first, again, other)
		}
	}
	if n, err := s.CountVoiceRooms(ctx); err != nil || n != 2 {
		t.Errorf("CountVoiceRooms = %d, %v; want 2", n, err)
	}
}

func TestChannelVoiceRoomUnknownChannel(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.ChannelVoiceRoom(context.Background(), 9999); err == nil {
		t.Fatal("room for a channel that does not exist was created")
	}
	if n, _ := s.CountVoiceRooms(context.Background()); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

func TestChannelVoiceRoomConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 24
	names := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			room, err := s.ChannelVoiceRoom(ctx, ch.ID)
			names[i], errs[i] = room.RoomName, err
		}()
	}
	wg.Wait()
	for i := range names {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if names[i] != names[0] {
			t.Fatalf("worker %d got room %q, worker 0 got %q", i, names[i], names[0])
		}
	}
	if n, err := s.CountVoiceRooms(ctx); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; want exactly 1", n, err)
	}
}

// TestVoiceRoomsIndexes checks the constraints the table itself enforces, by
// writing around ChannelVoiceRoom: one channel-wide row per channel, one row
// per (channel, net), and unique room names.
func TestVoiceRoomsIndexes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChannel(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	net1, err := s.CreateNet(ctx, "system", ch.ID, "net1", 0)
	if err != nil {
		t.Fatal(err)
	}
	net2, err := s.CreateNet(ctx, "system", ch.ID, "net2", 0)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(channel int64, net any, name string) error {
		_, err := s.db.ExecContext(ctx,
			"INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at) VALUES (?, ?, ?, 1)", channel, net, name)
		return err
	}
	steps := []struct {
		name    string
		channel int64
		net     any
		room    string
		wantErr bool
	}{
		{"channel-wide room", ch.ID, nil, "r1", false},
		{"second channel-wide room for the channel", ch.ID, nil, "r2", true},
		{"channel-wide room of another channel", other.ID, nil, "r3", false},
		{"net room", ch.ID, net1.ID, "r4", false},
		{"second room for the same net", ch.ID, net1.ID, "r5", true},
		{"room for another net", ch.ID, net2.ID, "r6", false},
		{"duplicate room name", other.ID, net1.ID, "r1", true},
		{"unknown channel", 9999, nil, "r7", true},
	}
	for _, st := range steps {
		err := insert(st.channel, st.net, st.room)
		if (err != nil) != st.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", st.name, err, st.wantErr)
		}
	}
}

func TestVoiceRoomsMigrationFromSchema12(t *testing.T) {
	const pre = 12
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
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`PRAGMA user_version = 12`,
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 13 {
		t.Errorf("user_version = %d (%v), want %d (at least 13)", version, err, len(migrations))
	}
	if n, err := s.CountVoiceRooms(ctx); err != nil || n != 0 {
		t.Errorf("CountVoiceRooms after migration = %d, %v; want 0", n, err)
	}
	room, err := s.ChannelVoiceRoom(ctx, 1)
	if err != nil {
		t.Fatalf("ChannelVoiceRoom on a migrated database: %v", err)
	}
	if !roomNamePattern.MatchString(room.RoomName) {
		t.Errorf("room name %q", room.RoomName)
	}
	if _, err := s.ChannelVoiceRoom(ctx, 2); err == nil {
		t.Error("room for a channel that does not exist was created")
	}
}
