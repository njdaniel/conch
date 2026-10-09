package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
		{"duplicate room name", other.ID, nil, "r1", true},
		{"unknown channel", 9999, nil, "r7", true},
		// A net room belongs to its net's own channel, and a net has one room.
		{"a net's room under another channel", other.ID, net2.ID, "r8", true},
		{"a second room for a net under another channel", other.ID, net1.ID, "r9", true},
		{"unknown net", ch.ID, int64(9999), "r10", true},
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

// After the first call a room lookup is a read: it takes no write lock, so a
// member asking for sessions in a loop cannot hold up writers, and it works
// while another connection is in the middle of a write transaction.
func TestChannelVoiceRoomReadsOnceTheRoomExists(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ChannelVoiceRoom(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Hold SQLite's write lock on another connection.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()
	quick, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	again, err := s.ChannelVoiceRoom(quick, ch.ID)
	if err != nil {
		t.Fatalf("lookup while a writer holds the lock: %v", err)
	}
	if again != first {
		t.Errorf("room = %+v, want %+v", again, first)
	}
}

// TestVoiceRoomReaders covers the read helpers the presence poller uses: all
// rooms, the rooms of one channel, and the rooms of the channels a principal
// is a member of.
func TestVoiceRoomReaders(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a, _ := s.CreateChannel(ctx, "alpha")
	b, _ := s.CreateChannel(ctx, "beta")
	c, _ := s.CreateChannel(ctx, "gamma")
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "pat")
	if err != nil {
		t.Fatal(err)
	}
	roomA, _ := s.ChannelVoiceRoom(ctx, a.ID)
	roomB, _ := s.ChannelVoiceRoom(ctx, b.ID)
	roomC, _ := s.ChannelVoiceRoom(ctx, c.ID)
	for _, ch := range []Channel{a, b} {
		if _, err := s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	names := func(rooms []VoiceRoom, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range rooms {
			if r.NetID != 0 || r.CreatedAt.IsZero() {
				t.Errorf("room %+v: net or created_at wrong", r)
			}
			out = append(out, r.RoomName)
		}
		return strings.Join(out, ",")
	}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"all rooms", names(s.ListVoiceRooms(ctx)), strings.Join([]string{roomA.RoomName, roomB.RoomName, roomC.RoomName}, ",")},
		{"one channel", names(s.VoiceRoomsForChannel(ctx, b.ID)), roomB.RoomName},
		{"channel with no room", names(s.VoiceRoomsForChannel(ctx, 9999)), ""},
		{"rooms of a member's channels", names(s.VoiceRoomsForMember(ctx, p.ID)), roomA.RoomName + "," + roomB.RoomName},
		{"principal in no channel", names(s.VoiceRoomsForMember(ctx, 9999)), ""},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Holders, the invariant and rotation (issue #161)

// holderFixture is a channel with a live room and a human member holding a
// session on it under one credential.
type holderFixture struct {
	s    *Store
	ch   Channel
	room VoiceRoom
	p    Principal
	cred int64
	exp  time.Time
}

func newHolderFixture(t *testing.T) holderFixture {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "pat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	cred, _, err := s.CreateCredential(ctx, "system", p.ID, "laptop", &exp)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.ChannelVoiceRoom(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVoiceHolder(ctx, room.ID, p.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	return holderFixture{s: s, ch: ch, room: room, p: p, cred: cred.ID, exp: exp}
}

func TestRecordVoiceHolderOncePerCredential(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	other, _, err := f.s.CreateCredential(ctx, "system", f.p.ID, "phone", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The same triple again, and another credential of the same principal.
	for i := 0; i < 3; i++ {
		if err := f.s.RecordVoiceHolder(ctx, f.room.ID, f.p.ID, f.cred); err != nil {
			t.Fatalf("again %d: %v", i, err)
		}
	}
	if err := f.s.RecordVoiceHolder(ctx, f.room.ID, f.p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.VoiceRoomHolders(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []VoiceHolder{
		{RoomID: f.room.ID, PrincipalID: f.p.ID, CredentialID: f.cred},
		{RoomID: f.room.ID, PrincipalID: f.p.ID, CredentialID: other.ID},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("holders = %+v, want %+v", got, want)
	}

	// Recording again takes no write lock: it is a read.
	conn, err := f.s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()
	quick, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := f.s.RecordVoiceHolder(quick, f.room.ID, f.p.ID, f.cred); err != nil {
		t.Errorf("recording a known holder while a writer holds the lock: %v", err)
	}
}

func TestRecordVoiceHolderRefusals(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	if _, rotated, err := f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved); err != nil || !rotated {
		t.Fatalf("rotate: %v %v", rotated, err)
	}
	tests := []struct {
		name string
		room int64
		want error
	}{
		{"a retired room", f.room.ID, ErrVoiceRoomRetired},
		{"a room that does not exist", 9999, ErrNotFound},
	}
	for _, tt := range tests {
		if err := f.s.RecordVoiceHolder(ctx, tt.room, f.p.ID, f.cred); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	if h, _ := f.s.VoiceRoomHolders(ctx, f.room.ID); len(h) != 0 {
		t.Errorf("a retired room has holders: %+v", h)
	}
}

func TestInvalidVoiceRooms(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		// invalidate changes the store and returns the time to ask at.
		invalidate func(t *testing.T, f holderFixture) time.Time
		want       string // "" means the room is valid
	}{
		{"every holder valid", func(t *testing.T, f holderFixture) time.Time { return time.Now() }, ""},
		{"member removed from the channel", func(t *testing.T, f holderFixture) time.Time {
			if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.p.ID); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, VoiceRotateMemberRemoved},
		{"principal disabled", func(t *testing.T, f holderFixture) time.Time {
			if _, err := f.s.DisablePrincipal(ctx, "system", f.p.ID); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, VoiceRotatePrincipalDisabled},
		{"the credential revoked", func(t *testing.T, f holderFixture) time.Time {
			if err := f.s.RevokeCredential(ctx, "system", f.cred); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, VoiceRotateRevoked},
		{"all credentials revoked", func(t *testing.T, f holderFixture) time.Time {
			if _, err := f.s.RevokeAllCredentials(ctx, "system", f.p.ID); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, VoiceRotateRevoked},
		{"credential expired", func(t *testing.T, f holderFixture) time.Time {
			return f.exp.Add(time.Second)
		}, VoiceRotateExpired},
		{"credential not yet expired", func(t *testing.T, f holderFixture) time.Time {
			return f.exp.Add(-time.Second)
		}, ""},
		{"principal no longer human", func(t *testing.T, f holderFixture) time.Time {
			if _, err := f.s.db.ExecContext(ctx, `UPDATE principals SET kind = 'agent' WHERE id = ?`, f.p.ID); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		}, VoiceRotateNotHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHolderFixture(t)
			at := tt.invalidate(t, f)
			got, err := f.s.InvalidVoiceRooms(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("returned %+v for a valid room", got)
				}
				return
			}
			if len(got) != 1 || got[0].Room.ID != f.room.ID || got[0].Reason != tt.want {
				t.Fatalf("returned %+v, want room %d for %q", got, f.room.ID, tt.want)
			}
			if got[0].Room.RoomName != f.room.RoomName || got[0].Room.ChannelID != f.ch.ID {
				t.Errorf("room = %+v, want the fixture's", got[0].Room)
			}
		})
	}

	t.Run("one invalid holder among valid ones", func(t *testing.T) {
		f := newHolderFixture(t)
		q, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "quinn")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, q.ID, 0); err != nil {
			t.Fatal(err)
		}
		qc, _, err := f.s.CreateCredential(ctx, "system", q.ID, "x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.RecordVoiceHolder(ctx, f.room.ID, q.ID, qc.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.InvalidVoiceRooms(ctx, time.Now()); len(got) != 0 {
			t.Fatalf("both valid, got %+v", got)
		}
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, q.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.InvalidVoiceRooms(ctx, time.Now()); len(got) != 1 {
			t.Fatalf("one holder removed, got %+v", got)
		}
	})

	t.Run("a room with no holders", func(t *testing.T) {
		f := newHolderFixture(t)
		other, err := f.s.CreateChannel(ctx, "empty")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.ChannelVoiceRoom(ctx, other.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := f.s.InvalidVoiceRooms(ctx, time.Now()); err != nil || len(got) != 0 {
			t.Errorf("got %+v, %v; want nothing", got, err)
		}
	})

	t.Run("a retired room is never returned", func(t *testing.T) {
		f := newHolderFixture(t)
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.p.ID); err != nil {
			t.Fatal(err)
		}
		if _, rotated, err := f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved); err != nil || !rotated {
			t.Fatalf("rotate: %v %v", rotated, err)
		}
		if got, err := f.s.InvalidVoiceRooms(ctx, time.Now()); err != nil || len(got) != 0 {
			t.Errorf("got %+v, %v; want nothing", got, err)
		}
	})
}

func TestRotateVoiceRoom(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	untouched, err := f.s.CreateChannel(ctx, "untouched")
	if err != nil {
		t.Fatal(err)
	}
	untouchedRoom, err := f.s.ChannelVoiceRoom(ctx, untouched.ID)
	if err != nil {
		t.Fatal(err)
	}
	neverHad, err := f.s.CreateChannel(ctx, "never-had-a-room")
	if err != nil {
		t.Fatal(err)
	}

	next, rotated, err := f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved)
	if err != nil || !rotated {
		t.Fatalf("rotate = %+v, %v, %v", next, rotated, err)
	}
	if !roomNamePattern.MatchString(next.RoomName) || next.RoomName == f.room.RoomName || next.ChannelID != f.ch.ID {
		t.Errorf("next room = %+v (old %q)", next, f.room.RoomName)
	}
	cur, err := f.s.ChannelVoiceRoom(ctx, f.ch.ID)
	if err != nil || cur.ID != next.ID || cur.RoomName != next.RoomName {
		t.Errorf("ChannelVoiceRoom after rotation = %+v, %v; want %+v", cur, err, next)
	}
	if h, _ := f.s.VoiceRoomHolders(ctx, f.room.ID); len(h) != 0 {
		t.Errorf("the retired room kept its holders: %+v", h)
	}
	if h, _ := f.s.VoiceRoomHolders(ctx, next.ID); len(h) != 0 {
		t.Errorf("the new room has holders: %+v", h)
	}
	retired, err := f.s.ListRetiredVoiceRooms(ctx)
	if err != nil || len(retired) != 1 || retired[0].RoomName != f.room.RoomName || retired[0].RetiredAt.IsZero() {
		t.Errorf("retired = %+v, %v", retired, err)
	}
	live, err := f.s.ListVoiceRooms(ctx)
	if err != nil || len(live) != 2 {
		t.Fatalf("live = %+v, %v; want the new room and the other channel's", live, err)
	}
	for _, r := range live {
		if r.RoomName == f.room.RoomName {
			t.Error("the old room is still listed as live")
		}
		if r.ChannelID == untouched.ID && r.RoomName != untouchedRoom.RoomName {
			t.Error("another channel's room changed")
		}
	}
	// A channel that never had a room is unaffected: nothing was made for it.
	if rooms, _ := f.s.VoiceRoomsForChannel(ctx, neverHad.ID); len(rooms) != 0 {
		t.Errorf("rooms for a channel that never had one: %+v", rooms)
	}
	// Exactly one audit row, with a reason and no room name.
	events, err := f.s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Action != AuditVoiceRoomRotated {
			continue
		}
		n++
		if e.Actor != "system" || e.Subject != "channel:"+strconv.FormatInt(f.ch.ID, 10) || e.Detail != "reason=member_removed" {
			t.Errorf("audit row = %+v", e)
		}
	}
	if n != 1 {
		t.Errorf("voice_room_rotated rows = %d, want 1", n)
	}

	// Rotating the retired room again is a no-op.
	if again, rotated, err := f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved); err != nil || rotated || again.ID != 0 {
		t.Errorf("second rotation = %+v, %v, %v; want a no-op", again, rotated, err)
	}
	if _, _, err := f.s.RotateVoiceRoom(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate of an unknown room: %v", err)
	}
	if n, _ := f.s.CountVoiceRooms(ctx); n != 3 {
		t.Errorf("rows = %d, want 3 (retired, new, other channel)", n)
	}
}

// A name is never handed out twice: rotate many times and every name, live or
// retired, is different, and the retired names cannot be written back.
func TestRotatedNamesAreNeverReused(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	seen := map[string]bool{f.room.RoomName: true}
	cur := f.room
	for i := 0; i < 10; i++ {
		next, rotated, err := f.s.RotateVoiceRoom(ctx, cur.ID, VoiceRotateMemberRemoved)
		if err != nil || !rotated {
			t.Fatalf("rotation %d: %v %v", i, rotated, err)
		}
		if seen[next.RoomName] {
			t.Fatalf("name %q handed out twice", next.RoomName)
		}
		seen[next.RoomName] = true
		cur = next
	}
	// The table itself refuses a retired name for a new row.
	_, err := f.s.db.ExecContext(ctx,
		"INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at, retired_at) VALUES (?, NULL, ?, 1, 1)", f.ch.ID, f.room.RoomName)
	if err == nil {
		t.Error("a retired room name was inserted again")
	}
	if rooms, _ := f.s.VoiceRoomsForChannel(ctx, f.ch.ID); len(rooms) != 1 || rooms[0].ID != cur.ID {
		t.Errorf("live rooms of the channel = %+v, want only the last", rooms)
	}
}

func TestRotateVoiceRoomConcurrent(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	const workers = 20
	var wg sync.WaitGroup
	results := make([]bool, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i], errs[i] = f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved)
		}()
	}
	wg.Wait()
	won := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i] {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d callers rotated the room, want exactly 1", won)
	}
	if rooms, _ := f.s.VoiceRoomsForChannel(ctx, f.ch.ID); len(rooms) != 1 || rooms[0].ID == f.room.ID {
		t.Errorf("live rooms = %+v, want exactly one new one", rooms)
	}
	if n, _ := f.s.CountVoiceRooms(ctx); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}

// Rotation is all or nothing: when the last step (the audit row) fails, the
// old room is still live with its holders and no new row exists.
func TestRotateVoiceRoomIsAtomic(t *testing.T) {
	ctx := context.Background()
	f := newHolderFixture(t)
	if _, err := f.s.db.ExecContext(ctx, `CREATE TRIGGER refuse_rotation_audit BEFORE INSERT ON audit_events
		WHEN NEW.action = 'voice_room_rotated' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, rotated, err := f.s.RotateVoiceRoom(ctx, f.room.ID, VoiceRotateMemberRemoved); err == nil || rotated {
		t.Fatalf("rotation = %v, %v; want it to fail", rotated, err)
	}
	cur, err := f.s.ChannelVoiceRoom(ctx, f.ch.ID)
	if err != nil || cur.ID != f.room.ID || cur.RoomName != f.room.RoomName {
		t.Errorf("room after a failed rotation = %+v, %v; want the old one", cur, err)
	}
	if h, _ := f.s.VoiceRoomHolders(ctx, f.room.ID); len(h) != 1 {
		t.Errorf("holders after a failed rotation = %+v, want 1", h)
	}
	if n, _ := f.s.CountVoiceRooms(ctx); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	if retired, _ := f.s.ListRetiredVoiceRooms(ctx); len(retired) != 0 {
		t.Errorf("retired = %+v", retired)
	}
}

// A database at version 13 with rooms in it has them all retired by the
// migration: tokens for them may be out, renewed by LiveKit, and nothing
// records who holds them, so no loss of entitlement could ever rotate them.
// Retired, the sweep deletes any LiveKit still has, and the channel's next
// session gets a new room with its holders recorded. The indexes are the
// live-row ones afterwards.
func TestVoiceRoomsMigrationFromSchema13(t *testing.T) {
	const pre = 13
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
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3), (2, 'other', 3)`,
		`INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at) VALUES (1, NULL, 'conch-old1', 5), (2, NULL, 'conch-old2', 6)`,
		`PRAGMA user_version = 13`,
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 14 {
		t.Errorf("user_version = %d (%v), want %d (at least 14)", version, err, len(migrations))
	}
	if live, err := s.ListVoiceRooms(ctx); err != nil || len(live) != 0 {
		t.Fatalf("live rooms after migration = %+v, %v; want none", live, err)
	}
	retired, err := s.ListRetiredVoiceRooms(ctx)
	if err != nil || len(retired) != 2 || retired[0].RoomName != "conch-old1" || retired[1].RoomName != "conch-old2" {
		t.Fatalf("retired rooms after migration = %+v, %v; want both old rooms", retired, err)
	}
	for _, r := range retired {
		if r.RetiredAt.IsZero() || time.Since(r.RetiredAt) > time.Minute || time.Until(r.RetiredAt) > time.Minute {
			t.Errorf("room %d retired at %v, want now", r.ID, r.RetiredAt)
		}
	}
	got, err := s.ChannelVoiceRoom(ctx, 1)
	if err != nil || got.RoomName == "conch-old1" || got.RoomName == "" {
		t.Fatalf("ChannelVoiceRoom = %+v, %v; want a new room, not the retired one", got, err)
	}
	live := []VoiceRoom{got}
	// The indexes now cover live rows only: a second live row is refused, a
	// retired one is fine, and a used name still cannot be reused.
	insert := func(name string, retired any) error {
		_, err := s.db.ExecContext(ctx,
			"INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at, retired_at) VALUES (1, NULL, ?, 1, ?)", name, retired)
		return err
	}
	if err := insert("conch-second", nil); err == nil {
		t.Error("a second live channel-wide room was accepted")
	}
	if err := insert("conch-retired", 7); err != nil {
		t.Errorf("a retired row was refused: %v", err)
	}
	if err := insert("conch-old1", 7); err == nil {
		t.Error("an existing room name was accepted")
	}
	// A rotation works on the migrated data.
	if _, rotated, err := s.RotateVoiceRoom(ctx, live[0].ID, VoiceRotateMemberRemoved); err != nil || !rotated {
		t.Errorf("rotate on a migrated database: %v %v", rotated, err)
	}
}

// PruneRetiredVoiceRooms deletes a retired room's row only when it is older
// than the cut-off and not in the keep list; live rows and rows with a holder
// are never touched.
func TestPruneRetiredVoiceRooms(t *testing.T) {
	ctx := context.Background()
	hour := time.Hour.Milliseconds()
	base := time.Now().UnixMilli()
	cutoff := time.UnixMilli(base - 24*hour)
	tests := []struct {
		name    string
		retired any // retired_at in ms, or nil for a live row
		keep    bool
		holder  bool
		pruned  bool
	}{
		{"retired two days ago, LiveKit no longer lists it", base - 48*hour, false, false, true},
		{"retired two days ago, LiveKit still lists it", base - 48*hour, true, false, false},
		{"retired an hour ago", base - hour, false, false, false},
		{"retired exactly at the cut-off", base - 24*hour, false, false, false},
		{"live", nil, false, false, false},
		{"retired two days ago with a holder still recorded", base - 48*hour, false, true, false},
	}
	s := openTestStore(t)
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "ann")
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := s.CreateCredential(ctx, "system", p.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(tests))
	var keep []int64
	for i, tt := range tests {
		ch, err := s.CreateChannel(ctx, fmt.Sprintf("ch%d", i))
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.db.ExecContext(ctx,
			"INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at, retired_at) VALUES (?, NULL, ?, 1, ?)",
			ch.ID, fmt.Sprintf("conch-prune-%d", i), tt.retired)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if ids[i], err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
		if tt.keep {
			keep = append(keep, ids[i])
		}
		if tt.holder {
			if _, err := s.db.ExecContext(ctx,
				"INSERT INTO voice_room_holders (room_id, principal_id, credential_id, created_at) VALUES (?, ?, ?, 1)", ids[i], p.ID, cred.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	exists := func(id int64) bool {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM voice_rooms WHERE id = ?", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	want := int64(0)
	for _, tt := range tests {
		if tt.pruned {
			want++
		}
	}
	n, err := s.PruneRetiredVoiceRooms(ctx, cutoff, keep)
	if err != nil || n != want {
		t.Fatalf("PruneRetiredVoiceRooms = %d, %v; want %d", n, err, want)
	}
	for i, tt := range tests {
		if got := !exists(ids[i]); got != tt.pruned {
			t.Errorf("%s: pruned = %v, want %v", tt.name, got, tt.pruned)
		}
	}
	// With nothing to keep the statement has no IN list; it must still run.
	if n, err := s.PruneRetiredVoiceRooms(ctx, cutoff, nil); err != nil || n != 1 {
		t.Errorf("second prune with no keep list = %d, %v; want the one LiveKit had still listed", n, err)
	}
	var holders int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM voice_room_holders").Scan(&holders); err != nil || holders != 1 {
		t.Errorf("holder rows = %d, %v; want the one recorded, untouched", holders, err)
	}
}
