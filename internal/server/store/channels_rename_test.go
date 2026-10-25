package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
)

// Channel names and renames (issue #727, docs/design/channel-rename.md).

// The clashing pairs: the exact spelling, case, a Cyrillic letter in a Latin
// name, a zero for an o, and a ligature.
var clashingChannelNames = []struct{ what, held, taken string }{
	{"by spelling", "ops", "ops"},
	{"by case", "Ops", "OPS"},
	{"by look-alike", "paypal", "p\u0430ypal"},
	{"by a digit for a letter", "bob", "b0b"},
	{"by a ligature", "\ufb01nance", "finance"},
}

// storedChannelKeysMatch checks every channel's stored keys against its name
// and returns how many channels there are.
func storedChannelKeysMatch(t *testing.T, s *Store) int {
	t.Helper()
	rows, err := s.db.Query("SELECT id, name, name_key, name_skel FROM channels ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id int64
		var name string
		var folded, skel sql.NullString
		if err := rows.Scan(&id, &name, &folded, &skel); err != nil {
			t.Fatal(err)
		}
		want := names.KeysOf(name)
		if !folded.Valid || !skel.Valid || folded.String != want.Folded || skel.String != want.Skeletal {
			t.Errorf("channel %d %+q: keys %v %v, want %+q", id, name, folded, skel, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// Both ways of creating a channel, and a rename, refuse a name whose key
// another channel holds, and write nothing when they do.
func TestEveryChannelNameWriteRefusesAClash(t *testing.T) {
	ctx := context.Background()
	paths := []struct {
		name string
		// write tries to give a new or existing channel the name taken, in a
		// store where another channel is called held.
		write func(t *testing.T, s *Store, taken string) error
	}{
		{"create", func(_ *testing.T, s *Store, taken string) error {
			_, err := s.CreateChannel(ctx, taken)
			return err
		}},
		{"create as", func(_ *testing.T, s *Store, taken string) error {
			_, err := s.CreateChannelAs(ctx, "system", taken, 0)
			return err
		}},
		{"rename", func(t *testing.T, s *Store, taken string) error {
			ch, err := s.CreateChannel(ctx, "someplace else")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RenameChannel(ctx, "system", ch.ID, ch.Name, taken, time.Now())
			return err
		}},
	}
	for _, path := range paths {
		for _, pair := range clashingChannelNames {
			t.Run(path.name+" "+pair.what, func(t *testing.T) {
				s := openTestStore(t)
				if _, err := s.CreateChannel(ctx, pair.held); err != nil {
					t.Fatal(err)
				}
				before := countRows(t, s, "channels")
				audits := countRows(t, s, "audit_events")
				if err := path.write(t, s, pair.taken); !errors.Is(err, ErrDuplicate) {
					t.Fatalf("error = %v, want ErrDuplicate", err)
				}
				var n int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM channels WHERE name = ?", pair.taken).Scan(&n); err != nil || n != boolCount(pair.held == pair.taken) {
					t.Errorf("%d channels called %q after the refusal (%v)", n, pair.taken, err)
				}
				if got := countRows(t, s, "channels"); got != before+boolCount(path.name == "rename") {
					t.Errorf("%d channels after the refusal, want %d", got, before+boolCount(path.name == "rename"))
				}
				if got := countRows(t, s, "audit_events"); got != audits {
					t.Errorf("the refusal wrote %d audit rows", got-audits)
				}
			})
		}
	}
}

// Every path stores the keys of the name it writes, a name made of nothing a
// reader can see is refused, and a row written without keys is refused by
// the schema itself.
func TestEveryChannelNameWriteStoresTheKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.CreateChannel(ctx, "Zo\u00eb"); err != nil {
		t.Fatal(err)
	}
	as, err := s.CreateChannelAs(ctx, "system", "\u5c71\u7530", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameChannel(ctx, "system", as.ID, as.Name, "\u5c71\u7530 \u592a\u90ce", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := storedChannelKeysMatch(t, s); n != 2 {
		t.Errorf("%d channels, want 2", n)
	}

	invisible := "\u3164"
	if _, err := s.CreateChannel(ctx, invisible); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("create %+q = %v, want ErrNameInvalid", invisible, err)
	}
	if _, err := s.CreateChannelAs(ctx, "system", invisible, 0); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("create as %+q = %v, want ErrNameInvalid", invisible, err)
	}
	if _, err := s.RenameChannel(ctx, "system", as.ID, "\u5c71\u7530 \u592a\u90ce", invisible, time.Now()); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("rename to %+q = %v, want ErrNameInvalid", invisible, err)
	}

	for _, stmt := range []string{
		`INSERT INTO channels (name, created_at) VALUES ('keyless', 1)`,
		`INSERT INTO channels (name, name_key, created_at) VALUES ('keyless', 'keyless', 1)`,
		`INSERT INTO channels (name, name_skel, created_at) VALUES ('keyless', 'keyless', 1)`,
		fmt.Sprintf(`UPDATE channels SET name = 'other', name_key = NULL WHERE id = %d`, as.ID),
		fmt.Sprintf(`UPDATE channels SET name = 'other', name_skel = NULL WHERE id = %d`, as.ID),
	} {
		if _, err := s.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "name_key and name_skel are required") {
			t.Errorf("%s = %v, want refused", stmt, err)
		}
	}
}

// The rename itself, case by case in the order RenameChannel decides them.
func TestRenameChannel(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ops, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)

	// A new name: stored with its keys, the same id and creation time, and
	// audited with who, which channel and both names, quoted.
	res, err := s.RenameChannel(ctx, "principal:1", ops.ID, "ops", `ops "to=x"`, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.From != "ops" || res.Channel.ID != ops.ID || res.Channel.Name != `ops "to=x"` || !res.Channel.CreatedAt.Equal(ops.CreatedAt) {
		t.Errorf("result = %+v", res)
	}
	got, err := s.ChannelByID(ctx, ops.ID)
	if err != nil || got.Name != `ops "to=x"` {
		t.Errorf("stored = %+v, %v", got, err)
	}
	last, err := s.LastAuditEvent(ctx, AuditChannelRenamed)
	if err != nil {
		t.Fatal(err)
	}
	if last.Actor != "principal:1" || last.Subject != fmt.Sprintf("channel:%d", ops.ID) || last.Detail != `from="ops" to="ops \"to=x\""` || !last.CreatedAt.Equal(now) {
		t.Errorf("audit = %+v", last)
	}
	if n := auditCount(t, s, AuditChannelRenamed); n != 1 {
		t.Errorf("%d channel_renamed rows, want 1", n)
	}
	// The old name is gone, by spelling and by key: free for a new channel.
	if _, err := s.ChannelByName(ctx, "ops"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name = %v, want ErrNotFound", err)
	}
	if n := storedChannelKeysMatch(t, s); n != 2 {
		t.Errorf("%d channels, want 2", n)
	}

	cur := `ops "to=x"`
	rows := countRows(t, s, "audit_events")
	tests := []struct {
		name       string
		id         int64
		from, to   string
		wantErr    error
		wantChange bool
	}{
		{name: "an unknown channel", id: 9999, from: "x", to: "y", wantErr: ErrNotFound},
		// The same string is no rename, and from is not compared: a retry
		// of a rename that already happened is answered the same.
		{name: "the same string, from the old name", id: ops.ID, from: "ops", to: cur},
		{name: "the same string, from nonsense", id: ops.ID, from: "nonsense", to: cur},
		{name: "a stale from", id: ops.ID, from: "ops", to: "ops-2", wantErr: ErrChannelNameStale},
		{name: "a from that differs only by case", id: ops.ID, from: `OPS "TO=X"`, to: "ops-2", wantErr: ErrChannelNameStale},
		{name: "another channel's name", id: ops.ID, from: cur, to: "general", wantErr: ErrDuplicate},
		{name: "a look-alike of another channel's name", id: ops.ID, from: cur, to: "Gener\u0430l", wantErr: ErrDuplicate},
		{name: "a name nobody can see", id: ops.ID, from: cur, to: "\u3164", wantErr: ErrNameInvalid},
		// The channel's own key does not stand in its way.
		{name: "a change of case only", id: ops.ID, from: cur, to: `OPS "TO=X"`, wantChange: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := s.RenameChannel(ctx, "system", tt.id, tt.from, tt.to, now)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if res.Changed != tt.wantChange {
				t.Errorf("changed = %v, want %v", res.Changed, tt.wantChange)
			}
			if want := rows + boolCount(tt.wantChange); countRows(t, s, "audit_events") != want {
				t.Errorf("audit rows = %d, want %d", countRows(t, s, "audit_events"), want)
			}
		})
	}
}

// The three races of the design's §3.6. Each rename is one IMMEDIATE
// transaction, so SQLite serialises them and the second sees the first.
func TestConcurrentChannelRenames(t *testing.T) {
	ctx := context.Background()
	const n = 8
	// race runs n renames at once and returns what each was answered.
	race := func(rename func(i int) (ChannelRenameResult, error)) ([]ChannelRenameResult, []error) {
		results, errs := make([]ChannelRenameResult, n), make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = rename(i)
			}()
		}
		close(start)
		wg.Wait()
		return results, errs
	}

	t.Run("two channels to one name", func(t *testing.T) {
		s := openTestStore(t)
		chans := make([]Channel, n)
		for i := range chans {
			ch, err := s.CreateChannel(ctx, fmt.Sprintf("room-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			chans[i] = ch
		}
		// One name in several spellings: the same name by its keys.
		spellings := []string{"lobby", "Lobby", "LOBBY", "l\u043ebby", "1obby", "lobb\u0443", "lobby", "Lobby"}
		_, errs := race(func(i int) (ChannelRenameResult, error) {
			return s.RenameChannel(ctx, "system", chans[i].ID, chans[i].Name, spellings[i], time.Now())
		})
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, ErrDuplicate):
				t.Errorf("rename %d to %q = %v, want success or ErrDuplicate", i, spellings[i], err)
			}
		}
		if won != 1 || auditCount(t, s, AuditChannelRenamed) != 1 {
			t.Errorf("%d renames won and %d were audited; want exactly one", won, auditCount(t, s, AuditChannelRenamed))
		}
	})

	t.Run("one channel to two names", func(t *testing.T) {
		s := openTestStore(t)
		ch, err := s.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		_, errs := race(func(i int) (ChannelRenameResult, error) {
			return s.RenameChannel(ctx, "system", ch.ID, "ops", fmt.Sprintf("ops-%d", i), time.Now())
		})
		won := -1
		for i, err := range errs {
			switch {
			case err == nil && won == -1:
				won = i
			case err == nil:
				t.Errorf("renames %d and %d both won", won, i)
			case !errors.Is(err, ErrChannelNameStale):
				t.Errorf("rename %d = %v, want success or ErrChannelNameStale", i, err)
			}
		}
		got, err := s.ChannelByID(ctx, ch.ID)
		if won == -1 || err != nil || got.Name != fmt.Sprintf("ops-%d", won) {
			t.Errorf("the channel is %+v (%v), want the one winner's name (%d)", got, err, won)
		}
		if n := auditCount(t, s, AuditChannelRenamed); n != 1 {
			t.Errorf("%d channel_renamed rows, want 1", n)
		}
	})

	t.Run("one channel to the same name", func(t *testing.T) {
		s := openTestStore(t)
		ch, err := s.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		results, errs := race(func(int) (ChannelRenameResult, error) {
			return s.RenameChannel(ctx, "system", ch.ID, "ops", "ops-new", time.Now())
		})
		changed := 0
		for i, err := range errs {
			if err != nil {
				t.Errorf("rename %d = %v, want success", i, err)
			}
			if results[i].Changed {
				changed++
			}
			if results[i].Channel.Name != "ops-new" {
				t.Errorf("rename %d answered %+v, want the channel as ops-new", i, results[i].Channel)
			}
		}
		if changed != 1 || auditCount(t, s, AuditChannelRenamed) != 1 {
			t.Errorf("%d renames changed the name and %d were audited; want exactly one", changed, auditCount(t, s, AuditChannelRenamed))
		}
	})
}

// A database from before channel keys with names that clash by case and by
// look-alike letters, and one name nobody can see: the migration keys every
// channel, keeps them all as they are, writes no audit row, reports the
// clashing groups and the invisible name, and from then on no clashing name
// can be taken again in any spelling.
func TestChannelNameKeyMigration(t *testing.T) {
	const pre = channelNameKeyMigration - 1
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
	seeded := map[int64]string{1: "general", 2: "General", 3: "GENERAL", 4: "ops", 5: "0ps", 6: "\u3164", 7: "lobby"}
	for _, stmt := range []string{
		`INSERT INTO channels (id, name, created_at) VALUES
			(1, 'general', 1),
			(2, 'General', 2),
			(3, 'GENERAL', 3),
			(4, 'ops', 4),
			(5, '0ps', 5),
			(6, '` + "\u3164" + `', 6),
			(7, 'lobby', 7)`,
		fmt.Sprintf(`PRAGMA user_version = %d`, pre),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Writer(&logs), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if n := storedChannelKeysMatch(t, s); n != len(seeded) {
		t.Errorf("%d channels after the migration, want all %d kept", n, len(seeded))
	}
	for id, name := range seeded {
		if ch, err := s.ChannelByID(ctx, id); err != nil || ch.Name != name {
			t.Errorf("channel %d = %+v (%v), want it still called %+q", id, ch, err, name)
		}
	}
	if n := countRows(t, s, "audit_events"); n != 0 {
		t.Errorf("the migration wrote %d audit rows, want none", n)
	}
	out := logs.String()
	if !strings.Contains(out, "channel names clash") || !strings.Contains(out, "groups=2") || !strings.Contains(out, "[[1 2 3] [4 5]]") {
		t.Errorf("the migration's report = %q, want two groups: [1 2 3] and [4 5]", out)
	}
	if !strings.Contains(out, "no character a reader can see") || !strings.Contains(out, "channels=[6]") {
		t.Errorf("the migration's report = %q, want channel 6 named as invisible", out)
	}

	// The clashing names keep working, and their keys are taken in every
	// spelling, on create and on rename.
	for _, name := range []string{"gEnErAl", "OPS", "\u043eps"} {
		if _, err := s.CreateChannel(ctx, name); !errors.Is(err, ErrDuplicate) {
			t.Errorf("create %q = %v, want ErrDuplicate", name, err)
		}
	}
	if _, err := s.RenameChannel(ctx, "system", 1, "general", "general chat", time.Now()); err != nil {
		t.Errorf("rename to a name of its own: %v", err)
	}
	if _, err := s.RenameChannel(ctx, "system", 2, "General", "general", time.Now()); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename 2 to general while 3 holds its keys = %v, want ErrDuplicate", err)
	}
	if _, err := s.RenameChannel(ctx, "system", 4, "ops", "Ops", time.Now()); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename ops to Ops while 5 holds the skeleton = %v, want ErrDuplicate", err)
	}
	if _, err := s.RenameChannel(ctx, "system", 7, "lobby", "LOBBY", time.Now()); err != nil {
		t.Errorf("a change of case only, clashing with nobody: %v", err)
	}
}

func TestRenameChannelManagerLimits(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	opt := ChannelRenameOptions{CallerID: 1, Limits: RenameLimits{Renames: 2, Attempts: 3}}
	grantManager := func(t *testing.T, s *Store, channelID int64) {
		t.Helper()
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, "manager")
		if err != nil {
			t.Fatal(err)
		}
		no, yes := false, true
		if _, err := s.PutChannelMember(ctx, "system", channelID, p.ID, 0, &no, &yes); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("successful renames", func(t *testing.T) {
		s := openTestStore(t)
		ch, err := s.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		grantManager(t, s, ch.ID)
		from := ch.Name
		for i, name := range []string{"one", "two"} {
			if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, from, name, now, opt); err != nil {
				t.Fatalf("rename %d: %v", i+1, err)
			}
			from = name
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, from, "three", now, opt); !errors.Is(err, ErrRenameLimited) {
			t.Fatalf("third rename = %v, want ErrRenameLimited", err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, from, "operator", now); err != nil {
			t.Fatalf("unlimited rename = %v", err)
		}
	})

	t.Run("refused attempts", func(t *testing.T) {
		s := openTestStore(t)
		ch, err := s.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateChannel(ctx, "taken"); err != nil {
			t.Fatal(err)
		}
		grantManager(t, s, ch.ID)
		for i := 0; i < 3; i++ {
			if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "taken", now, opt); !errors.Is(err, ErrDuplicate) {
				t.Fatalf("duplicate %d = %v, want ErrDuplicate", i+1, err)
			}
			if err := s.RecordChannelRenameRefused(ctx, 1, ch.ID, opt.Limits, now); err != nil {
				t.Fatal(err)
			}
		}
		last, err := s.LastAuditEvent(ctx, AuditChannelRenameRefused)
		if err != nil || last.Detail != "" || last.Actor != "principal:1" {
			t.Errorf("refusal audit = %+v, %v", last, err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "taken", now, opt); !errors.Is(err, ErrRenameLimited) {
			t.Fatalf("fourth attempt = %v, want ErrRenameLimited", err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "stale-is-ignored", "ops", now, opt); err != nil {
			t.Fatalf("a no-op past the limit = %v", err)
		}
	})
}

func TestRenameChannelManagerRevokedBeforeWrite(t *testing.T) {
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
	no, yes := false, true
	if _, err := s.PutChannelMember(ctx, "system", ch.ID, manager.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChannelMemberManages(ctx, "system", ch.ID, manager.ID, false); err != nil {
		t.Fatal(err)
	}
	opt := ChannelRenameOptions{CallerID: manager.ID, Limits: RenameLimits{Renames: 5, Attempts: 20}}
	if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "renamed", time.Now(), opt); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("rename with a revoked grant = %v, want ErrManagerRefused", err)
	}
	if err := s.RecordChannelRenameRefused(ctx, manager.ID, ch.ID, opt.Limits, time.Now()); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("refusal with a revoked grant = %v, want ErrManagerRefused", err)
	}
	stored, err := s.ChannelByID(ctx, ch.ID)
	if err != nil || stored.Name != "ops" {
		t.Errorf("stored = %+v, %v", stored, err)
	}
}

func TestChannelRenameRefusalLimitIsAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := s.CreatePrincipal(ctx, PrincipalHuman, "manager")
	if err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	if _, err := s.PutChannelMember(ctx, "system", ch.ID, manager.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	lim := RenameLimits{Attempts: 20}
	now := time.Now().Truncate(time.Millisecond)
	results := make(chan error, 30)
	for range 30 {
		go func() {
			results <- s.RecordChannelRenameRefused(ctx, manager.ID, ch.ID, lim, now)
		}()
	}
	accepted := 0
	for range 30 {
		if err := <-results; err == nil {
			accepted++
		} else if !errors.Is(err, ErrRenameLimited) {
			t.Errorf("refusal = %v, want ErrRenameLimited", err)
		}
	}
	if accepted != lim.Attempts {
		t.Errorf("accepted = %d, want %d", accepted, lim.Attempts)
	}
	if n := auditCount(t, s, AuditChannelRenameRefused); n != lim.Attempts {
		t.Errorf("audit rows = %d, want %d", n, lim.Attempts)
	}
}

func TestRenameChannelCurrentStanding(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Store, Channel, Principal) {
		t.Helper()
		s := openTestStore(t)
		ch, err := s.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		caller, err := s.CreatePrincipal(ctx, PrincipalHuman, "caller")
		if err != nil {
			t.Fatal(err)
		}
		return s, ch, caller
	}
	opt := func(id int64) ChannelRenameOptions {
		return ChannelRenameOptions{CallerID: id, Limits: RenameLimits{Renames: 5, Attempts: 20}}
	}

	t.Run("owner promoted before write", func(t *testing.T) {
		s, ch, caller := setup(t)
		if _, err := s.db.Exec("UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", caller.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "renamed", time.Now(), opt(caller.ID)); err != nil {
			t.Fatalf("owner rename = %v", err)
		}
		last, err := s.LastAuditEvent(ctx, AuditChannelRenamed)
		if err != nil || strings.HasSuffix(last.Detail, channelRenameLimitedDetail) {
			t.Errorf("owner audit = %+v, %v", last, err)
		}
	})

	t.Run("operator demoted to non-member before write", func(t *testing.T) {
		s, ch, caller := setup(t)
		if _, err := s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", caller.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE principals SET role = 'member' WHERE id = ?", caller.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "renamed", time.Now(), opt(caller.ID)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("demoted non-member rename = %v, want ErrNotFound", err)
		}
		if err := s.RecordChannelRenameRefused(ctx, caller.ID, ch.ID, opt(caller.ID).Limits, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("demoted non-member refusal = %v, want ErrNotFound", err)
		}
	})

	t.Run("manager removed before write", func(t *testing.T) {
		s, ch, caller := setup(t)
		no, yes := false, true
		if _, err := s.PutChannelMember(ctx, "system", ch.ID, caller.ID, 0, &no, &yes); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RemoveChannelMember(ctx, "system", ch.ID, caller.ID, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RenameChannel(ctx, "principal:1", ch.ID, "ops", "renamed", time.Now(), opt(caller.ID)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("removed manager rename = %v, want ErrNotFound", err)
		}
		if err := s.RecordChannelRenameRefused(ctx, caller.ID, ch.ID, opt(caller.ID).Limits, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("removed manager refusal = %v, want ErrNotFound", err)
		}
	})
}
