package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// The clashing pairs of the acceptance criteria: case, a Cyrillic letter in a
// Latin name, and a ligature.
var clashingNames = []struct{ held, taken string }{
	{"Alice", "alice"},
	{"paypal", "p\u0430ypal"},
	{"\ufb01ona", "fiona"},
}

// Every path that creates a principal, and a rename, refuses a name whose key
// someone else holds, and writes nothing when it does.
func TestEveryNameWriteRefusesAClash(t *testing.T) {
	ctx := context.Background()
	paths := []struct {
		name string
		// write tries to give a new or existing principal the name taken,
		// in a store where another principal is called held.
		write func(t *testing.T, s *Store, taken string) error
	}{
		{"create", func(t *testing.T, s *Store, taken string) error {
			_, err := s.CreatePrincipal(ctx, PrincipalAgent, taken)
			return err
		}},
		{"bootstrap", func(t *testing.T, s *Store, taken string) error {
			_, err := s.BootstrapOperatorWith(ctx, taken, BootstrapOptions{})
			return err
		}},
		{"redeem", func(t *testing.T, s *Store, taken string) error {
			op, _, _, err := s.BootstrapOperator(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateChannel(ctx, "general"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			inv, err := s.CreateInvite(ctx, principalActor(op.ID), op.ID,
				schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RedeemInvite(ctx, HashInviteCode(inv.Code), taken, now)
			return err
		}},
		{"rename", func(t *testing.T, s *Store, taken string) error {
			p, err := s.CreatePrincipal(ctx, PrincipalHuman, "someone else")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RenamePrincipal(ctx, "system", p.ID, taken, RenameLimits{}, time.Now())
			return err
		}},
	}
	for _, path := range paths {
		for _, pair := range clashingNames {
			t.Run(fmt.Sprintf("%s %q over %q", path.name, pair.taken, pair.held), func(t *testing.T) {
				s := openTestStore(t)
				if _, err := s.CreatePrincipal(ctx, PrincipalHuman, pair.held); err != nil {
					t.Fatal(err)
				}
				before := countRows(t, s, "principals")
				err := path.write(t, s, pair.taken)
				if !errors.Is(err, ErrDuplicate) {
					t.Fatalf("error = %v, want ErrDuplicate", err)
				}
				var n int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE name = ?", pair.taken).Scan(&n); err != nil || n != 0 {
					t.Errorf("%d principals called %q after the refusal (%v)", n, pair.taken, err)
				}
				if path.name != "redeem" && countRows(t, s, "principals") != before+boolCount(path.name == "rename") {
					t.Error("the refusal left a principal behind")
				}
			})
		}
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Every path stores the key of the name it writes, so a later write can see
// it; a name that does not clash is accepted as typed.
func TestEveryNameWriteStoresTheKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	op, _, _, err := s.BootstrapOperator(ctx, "Root")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "Zo\u00eb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	inv, err := s.CreateInvite(ctx, principalActor(op.ID), op.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := s.RedeemInvite(ctx, HashInviteCode(inv.Code), "\u0905\u0930\u094d\u200d\u091c\u0941\u0928", now)
	if err != nil {
		t.Fatalf("a valid Indic name with a joiner was refused: %v", err)
	}
	if _, err := s.RenamePrincipal(ctx, "system", p.ID, "Zoe", RenameLimits{}, now); err != nil {
		t.Fatalf("Zo\u00eb to Zoe: %v", err)
	}
	if n := storedKeysMatch(t, s); n != 3 {
		t.Errorf("%d principals, want 3", n)
	}
	// The plain spelling of the joined name is the same name.
	if _, err := s.CreatePrincipal(ctx, PrincipalHuman, "\u0905\u0930\u094d\u091c\u0941\u0928"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("the plain spelling of %q = %v, want ErrDuplicate", joined.Principal.Name, err)
	}
}

// A row written without a key is refused by the schema itself, so a future
// path that forgets claimNameTx fails loudly instead of leaving a name no
// clash check can see.
func TestPrincipalRowNeedsAKey(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreatePrincipal(context.Background(), PrincipalHuman, "ann")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO principals (kind, name, created_at) VALUES ('human', 'keyless', 1)`,
		`INSERT INTO principals (kind, name, name_key, created_at) VALUES ('human', 'keyless', 'keyless', 1)`,
		`INSERT INTO principals (kind, name, name_skel, created_at) VALUES ('human', 'keyless', 'keyless', 1)`,
		fmt.Sprintf(`UPDATE principals SET name = 'anne', name_key = NULL WHERE id = %d`, p.ID),
		fmt.Sprintf(`UPDATE principals SET name = 'anne', name_skel = NULL WHERE id = %d`, p.ID),
	} {
		if _, err := s.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "name_key and name_skel are required") {
			t.Errorf("%s = %v, want refused", stmt, err)
		}
	}
}

// Two creates of one key at the same moment: SQLite's single writer and the
// in-transaction check let exactly one through.
func TestConcurrentCreatesOfOneKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	variants := []string{"Alice", "alice", "ALICE", "\u0410lice", "al\u0456ce", "aLiCe", "alice\u200e", "Alic\u0435"}
	var wg sync.WaitGroup
	errs := make([]error, len(variants))
	start := make(chan struct{})
	for i, n := range variants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.CreatePrincipal(ctx, PrincipalHuman, n)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrDuplicate):
			t.Errorf("create %q: %v, want success or ErrDuplicate", variants[i], err)
		}
	}
	if ok != 1 || countRows(t, s, "principals") != 1 {
		t.Errorf("%d creates succeeded and %d principals exist; want exactly one", ok, countRows(t, s, "principals"))
	}
}

func TestRenamePrincipal(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ann, err := s.CreatePrincipal(ctx, PrincipalHuman, "ann")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)

	// A new name: stored, keyed, audited with both names.
	res, err := s.RenamePrincipal(ctx, principalActor(ann.ID), ann.ID, "Ann Lee", RenameLimits{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.From != "ann" || res.Principal.Name != "Ann Lee" || res.Principal.ID != ann.ID {
		t.Errorf("result = %+v", res)
	}
	got, err := s.PrincipalByID(ctx, ann.ID)
	if err != nil || got.Name != "Ann Lee" {
		t.Errorf("stored = %+v, %v", got, err)
	}
	last, err := s.LastAuditEvent(ctx, AuditPrincipalRenamed)
	if err != nil {
		t.Fatal(err)
	}
	want := AuditEvent{ID: last.ID, Actor: principalActor(ann.ID), Action: AuditPrincipalRenamed, Subject: principalActor(ann.ID), Detail: `from="ann" to="Ann Lee"`, CreatedAt: now}
	if last.Actor != want.Actor || last.Subject != want.Subject || last.Detail != want.Detail || !last.CreatedAt.Equal(now) {
		t.Errorf("audit = %+v, want %+v", last, want)
	}

	// The person's own key does not stand in their way.
	if _, err := s.RenamePrincipal(ctx, principalActor(ann.ID), ann.ID, "ANN LEE", RenameLimits{}, now); err != nil {
		t.Errorf("a change of case only: %v", err)
	}
	// Someone else's does.
	if _, err := s.RenamePrincipal(ctx, principalActor(ann.ID), ann.ID, "B\u043eb", RenameLimits{}, now); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename to a look-alike of bob = %v, want ErrDuplicate", err)
	}
	// The same string again is no rename: nothing written.
	rows := countRows(t, s, "audit_events")
	if res, err := s.RenamePrincipal(ctx, principalActor(ann.ID), ann.ID, "ANN LEE", RenameLimits{Renames: 1}, now); err != nil || res.Changed {
		t.Errorf("rename to the same name = %+v, %v; want unchanged", res, err)
	}
	if countRows(t, s, "audit_events") != rows {
		t.Error("an unchanged name was audited")
	}
	if _, err := s.RenamePrincipal(ctx, "system", 999, "nobody", RenameLimits{}, now); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown principal = %v, want ErrPrincipalNotFound", err)
	}
	// A hidden account's name (issue #445) is keyed like any other. Only a
	// hide may write it: a rename to it is refused (TestReservedNames).
	hidden := fmt.Sprintf("hidden-%d", bob.ID)
	if _, err := s.HidePrincipal(ctx, "system", bob.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRow("SELECT name_key FROM principals WHERE id = ?", bob.ID).Scan(&key); err != nil || key != names.KeysOf(hidden).Folded {
		t.Errorf("hidden name key = %q, %v; want %q", key, err, names.KeysOf(hidden).Folded)
	}
	// And bob's old name is free again.
	if _, err := s.CreatePrincipal(ctx, PrincipalHuman, "Bob"); err != nil {
		t.Errorf("the freed name: %v", err)
	}
}

// The limits count only the person's own renames (and, for the attempt
// limit, their own refused attempts) in the last day; an operator's rename of
// them counts toward nothing. A refusal says when the next rename is allowed.
func TestRenameLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "n0")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Truncate(time.Millisecond)
	lim := RenameLimits{Renames: 5}
	self := principalActor(p.ID)
	for i := 1; i <= 5; i++ {
		if _, err := s.RenamePrincipal(ctx, self, p.ID, fmt.Sprintf("n%d", i), lim, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("rename %d: %v", i, err)
		}
		if i == 3 {
			// An operator's rename in between counts toward nothing.
			if _, err := s.RenamePrincipal(ctx, "principal:1000", p.ID, "by-op", RenameLimits{}, start.Add(3*time.Minute+time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	at := start.Add(10 * time.Minute)
	_, err = s.RenamePrincipal(ctx, self, p.ID, "n6", lim, at)
	var limited *RenameLimitedError
	if !errors.As(err, &limited) || !errors.Is(err, ErrRenameLimited) {
		t.Fatalf("sixth rename = %v, want a RenameLimitedError", err)
	}
	if want := start.Add(time.Minute).Add(RenameWindow); !limited.Until.Equal(want) {
		t.Errorf("until = %v, want %v (the first rename's time plus the window)", limited.Until, want)
	}
	if got, _ := s.PrincipalByID(ctx, p.ID); got.Name != "n5" {
		t.Errorf("a refused rename changed the name to %q", got.Name)
	}
	// An operator's call has no limits.
	if _, err := s.RenamePrincipal(ctx, "principal:1000", p.ID, "n7", RenameLimits{}, at); err != nil {
		t.Errorf("unlimited rename: %v", err)
	}
	// A limit lowered below what the window holds: two must leave it, so
	// the wait runs to the second rename's time plus the window.
	_, err = s.RenamePrincipal(ctx, self, p.ID, "n6", RenameLimits{Renames: 4}, at)
	if !errors.As(err, &limited) {
		t.Fatalf("rename over a full window = %v, want a RenameLimitedError", err)
	}
	if want := start.Add(2 * time.Minute).Add(RenameWindow); !limited.Until.Equal(want) {
		t.Errorf("until = %v, want %v (the second rename's time plus the window)", limited.Until, want)
	}
	// Once the first rename leaves the window there is room for one more:
	// the two operator renames are not in the count.
	later := start.Add(time.Minute).Add(RenameWindow).Add(time.Millisecond)
	if _, err := s.RenamePrincipal(ctx, self, p.ID, "n8", lim, later); err != nil {
		t.Errorf("rename after the window moved: %v", err)
	}
	// Another principal's renames are not this one's.
	q, err := s.CreatePrincipal(ctx, PrincipalHuman, "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenamePrincipal(ctx, principalActor(q.ID), q.ID, "q1", RenameLimits{Renames: 1}, at); err != nil {
		t.Errorf("another principal's first rename: %v", err)
	}
}

// The attempt limit counts the person's own renames and the refusals
// RecordRenameRefused writes, and the refusal row names no name.
func TestRenameAttemptLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePrincipal(ctx, PrincipalHuman, "target"); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Truncate(time.Millisecond)
	lim := RenameLimits{Renames: 5, Attempts: 4}
	self := principalActor(p.ID)
	if _, err := s.RenamePrincipal(ctx, self, p.ID, "probe-1", lim, start); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if _, err := s.RenamePrincipal(ctx, self, p.ID, "TARGET", lim, at); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("attempt %d = %v, want ErrDuplicate", i, err)
		}
		if err := s.RecordRenameRefused(ctx, p.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	// One rename and three refusals: the fifth attempt is refused, even for
	// a free name, though only one rename has succeeded.
	at := start.Add(10 * time.Minute)
	_, err = s.RenamePrincipal(ctx, self, p.ID, "free", lim, at)
	var limited *RenameLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("attempt over the limit = %v, want a RenameLimitedError", err)
	}
	if want := start.Add(RenameWindow); !limited.Until.Equal(want) {
		t.Errorf("until = %v, want %v", limited.Until, want)
	}
	refused, err := s.LastAuditEvent(ctx, AuditPrincipalRenameRefused)
	if err != nil {
		t.Fatal(err)
	}
	if refused.Actor != self || refused.Subject != self || refused.Detail != "reason=name_taken" || strings.Contains(strings.ToLower(refused.Detail), "target") {
		t.Errorf("refusal row = %+v", refused)
	}
	// Refusals do not count toward the renames limit.
	if _, err := s.RenamePrincipal(ctx, self, p.ID, "free", RenameLimits{Renames: 2}, at); err != nil {
		t.Errorf("second rename with only the renames limit: %v", err)
	}
}

// A name made only of characters nobody can see is refused on every path
// that writes a name, with ErrNameInvalid, and nothing is written.
func TestEveryNameWriteRefusesAnInvisibleName(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"\u3164", "\u3164\u2800\u3164", "\u115f\u1160"} {
		for _, path := range []struct {
			name  string
			write func(s *Store) error
		}{
			{"create", func(s *Store) error { _, err := s.CreatePrincipal(ctx, PrincipalHuman, name); return err }},
			{"bootstrap", func(s *Store) error { _, err := s.BootstrapOperatorWith(ctx, name, BootstrapOptions{}); return err }},
			{"redeem", func(s *Store) error {
				op, _, _, err := s.BootstrapOperator(ctx, "root")
				if err != nil {
					return err
				}
				if _, err := s.CreateChannel(ctx, "general"); err != nil {
					return err
				}
				inv, err := s.CreateInvite(ctx, principalActor(op.ID), op.ID,
					schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, time.Now())
				if err != nil {
					return err
				}
				_, err = s.RedeemInvite(ctx, HashInviteCode(inv.Code), name, time.Now())
				return err
			}},
			{"rename", func(s *Store) error {
				p, err := s.CreatePrincipal(ctx, PrincipalHuman, "visible")
				if err != nil {
					return err
				}
				_, err = s.RenamePrincipal(ctx, principalActor(p.ID), p.ID, name, RenameLimits{}, time.Now())
				return err
			}},
		} {
			t.Run(fmt.Sprintf("%s %+q", path.name, name), func(t *testing.T) {
				s := openTestStore(t)
				if err := path.write(s); !errors.Is(err, ErrNameInvalid) {
					t.Fatalf("error = %v, want ErrNameInvalid", err)
				}
				var n int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE name = ?", name).Scan(&n); err != nil || n != 0 {
					t.Errorf("%d principals with the name (%v)", n, err)
				}
			})
		}
	}
}

// Every statement in the store that changes a principal's name also sets
// both keys, so no path can leave a stale key behind. A source check: the
// triggers refuse a NULL key, but not a stale one.
func TestEveryNameUpdateSetsTheKeys(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	set := regexp.MustCompile(`(?is)UPDATE\s+principals\s+SET\s+([^"` + "`" + `]*)`)
	nameCol := regexp.MustCompile(`(?i)(^|[\s,])name\s*=`)
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 -- this package's own source
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range set.FindAllStringSubmatch(string(src), -1) {
			if !nameCol.MatchString(m[1]) {
				continue
			}
			found++
			if !strings.Contains(m[1], "name_key") || !strings.Contains(m[1], "name_skel") {
				t.Errorf("%s: %q changes a name without setting name_key and name_skel", f, strings.TrimSpace(m[0]))
			}
		}
	}
	if found == 0 {
		t.Fatal("found no UPDATE of a principal's name: the check has stopped checking")
	}
}

// storedKeysMatch checks that every principal's stored keys are its name's,
// and returns how many principals there are.
func storedKeysMatch(t *testing.T, s *Store) int {
	t.Helper()
	rows, err := s.db.Query("SELECT id, name, name_key, name_skel FROM principals ORDER BY id")
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
			t.Errorf("principal %d %+q: keys %v %v, want %+q", id, name, folded, skel, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// A database from before keys with names that clash by case and by
// look-alike letters, and one name nobody can see: the migration keys every
// row, keeps them all, reports the clashing groups and the invisible name,
// and from then on no clashing name can be taken again in any spelling.
func TestNameKeyMigration(t *testing.T) {
	const pre = nameKeyMigration - 1
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
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES
			(1, 'human', 'root', 'operator', 1),
			(2, 'human', 'Alice', 'member', 2),
			(3, 'human', 'alice', 'member', 3),
			(4, 'agent', 'ALICE', 'member', 4),
			(5, 'human', 'bob', 'member', 5),
			(6, 'human', 'B` + "\u043e" + `b', 'member', 6),
			(7, 'human', 'AIice', 'member', 7),
			(8, 'human', '` + "\u3164" + `', 'member', 8)`,
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

	if n := storedKeysMatch(t, s); n != 8 {
		t.Errorf("%d principals after the migration, want all 8 kept", n)
	}
	out := logs.String()
	// AIice shares only the skeletal key with the Alices, and joins their group.
	if !strings.Contains(out, "clash") || !strings.Contains(out, "groups=2") || !strings.Contains(out, "[[2 3 4 7] [5 6]]") {
		t.Errorf("the migration's report = %q, want two groups: [2 3 4 7] and [5 6]", out)
	}
	if !strings.Contains(out, "no character a reader can see") || !strings.Contains(out, "principals=[8]") {
		t.Errorf("the migration's report = %q, want principal 8 named as invisible", out)
	}

	// The pairs keep working, and their keys are taken in every spelling.
	for _, name := range []string{"aLiCe", "AIICE", "b0b"} {
		if _, err := s.CreatePrincipal(ctx, PrincipalHuman, name); !errors.Is(err, ErrDuplicate) {
			t.Errorf("create %q = %v, want ErrDuplicate", name, err)
		}
	}
	if _, err := s.RenamePrincipal(ctx, "system", 2, "Alice Liddell", RenameLimits{}, time.Now()); err != nil {
		t.Errorf("rename to a name of its own: %v", err)
	}
	if _, err := s.RenamePrincipal(ctx, "system", 3, "Alice", RenameLimits{}, time.Now()); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename 3 to Alice while 4 and 7 hold its keys = %v, want ErrDuplicate", err)
	}
	if _, err := s.RenamePrincipal(ctx, "system", 5, "Bob", RenameLimits{}, time.Now()); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename bob to Bob while 6 holds the key = %v, want ErrDuplicate", err)
	}
}
