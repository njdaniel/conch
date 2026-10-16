package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
)

// Names and renames (issue #441). A principal's name is unique by its keys
// (names.KeysOf: case-folded and reduced to look-alike skeletons, in two
// orders), not by its spelling. Every write that sets a name goes through
// claimNameTx inside its own IMMEDIATE transaction, so two writers cannot
// both take one key: SQLite has one writer, and the second sees the first's
// row.

const (
	// AuditPrincipalRenamed is the audit action of a rename: actor the
	// caller, subject the renamed principal, detail from=<old> to=<new>,
	// each quoted.
	AuditPrincipalRenamed = "principal_renamed"
	// AuditPrincipalRenameRefused is a person's own rename refused because
	// the name, or one that looks the same, is taken: actor and subject the
	// person, detail the reason only, never the name asked for. It is what
	// makes the attempt limit hold across restarts.
	AuditPrincipalRenameRefused = "principal_rename_refused"
)

// RenameWindow is the period RenamePrincipal's limits count over.
const RenameWindow = 24 * time.Hour

// ErrNameInvalid is returned by every write that sets a name when the name's
// keys are empty: it is made only of characters a reader cannot see (Hangul
// fillers, blanks), so it could not be told from any other such name.
var ErrNameInvalid = errors.New("store: the name has no character a reader can see")

// ErrRenameLimited is the error RenamePrincipal returns, as a
// *RenameLimitedError, when the principal has used up its renames.
var ErrRenameLimited = errors.New("store: too many renames")

// RenameLimitedError says when the principal may rename again.
type RenameLimitedError struct {
	Until time.Time
}

func (e *RenameLimitedError) Error() string {
	return fmt.Sprintf("store: too many renames; the next is allowed at %s", e.Until.UTC().Format(time.RFC3339))
}

func (e *RenameLimitedError) Is(target error) bool { return target == ErrRenameLimited }

// RenameLimits bound a person's renames of their own name in a RenameWindow.
// A zero field is no limit; the zero value, no limits at all, is an
// operator's.
type RenameLimits struct {
	// Renames is how many of their own renames may succeed.
	Renames int
	// Attempts is how many of their own renames may be tried: those that
	// succeeded and those refused because the name was taken. It stops a
	// refused rename being a free way to ask whether a name is on the
	// server.
	Attempts int
}

// claimNameTx is the one clash check, used by every path that creates or
// renames a principal: it returns name's keys, ErrNameInvalid when they are
// empty, or ErrDuplicate when a principal other than self holds either of
// them (the folded key against folded keys, the skeletal against skeletal).
// self is the principal being renamed, or 0 on create. It must run in the
// transaction that writes the name, and the keys it returns are the ones that
// row must store.
func claimNameTx(ctx context.Context, tx execer, name string, self int64) (names.Keys, error) {
	keys := names.KeysOf(name)
	if keys.Empty() {
		return names.Keys{}, ErrNameInvalid
	}
	var other int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM principals WHERE (name_key = ? OR name_skel = ?) AND id != ? LIMIT 1",
		keys.Folded, keys.Skeletal, self).Scan(&other)
	switch {
	case err == nil:
		return names.Keys{}, fmt.Errorf("store: name %q: %w", name, ErrDuplicate)
	case errors.Is(err, sql.ErrNoRows):
		return keys, nil
	default:
		return names.Keys{}, fmt.Errorf("store: check name %q: %w", name, err)
	}
}

// RenameResult is what a rename did.
type RenameResult struct {
	Principal Principal
	// From is the name before. Changed is false when the new name was the
	// same string: nothing was written and nothing audited.
	From    string
	Changed bool
}

// RenamePrincipal sets principal id's name, in one IMMEDIATE transaction: the
// new name must not clash with anyone else's (ErrDuplicate; the principal's
// own old keys do not count, so "alice" may become "Alice"), its keys must
// not be empty (ErrNameInvalid), and a principal_renamed row records both
// names. name must already be a valid name
// (schema.RenamePrincipalRequestV1.Validate); the store does not check.
//
// lim bounds the principal's own renames (rows whose actor is the principal
// itself) in the last RenameWindow; past either bound the rename is refused
// with a *RenameLimitedError. An operator renaming someone else passes no
// limits, and that rename counts toward nothing. The counts are of audit
// rows, so the limits need no state of their own and survive a restart.
// Renaming to the very same string is not a rename: it writes nothing, costs
// nothing and is never refused by a limit.
//
// An unknown principal is ErrPrincipalNotFound.
func (s *Store) RenamePrincipal(ctx context.Context, actor string, id int64, name string, lim RenameLimits, now time.Time) (RenameResult, error) {
	now = now.Truncate(time.Millisecond)
	var out RenameResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out = RenameResult{}
		p, err := principalByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out.From = p.Name
		out.Principal = p
		if p.Name == name {
			return nil
		}
		if err := renameLimitTx(ctx, tx, id, lim, now); err != nil {
			return err
		}
		keys, err := claimNameTx(ctx, tx, name, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE principals SET name = ?, name_key = ?, name_skel = ? WHERE id = ?",
			name, keys.Folded, keys.Skeletal, id)
		if isUniqueConstraintErr(err) {
			// The exact name is held by someone whose stored keys differ
			// from its keys now: keys computed from other Unicode tables.
			return fmt.Errorf("store: rename principal %d: %w", id, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: rename principal %d: %w", id, err)
		}
		// Quoted, so a name with spaces or "to=" in it cannot be read as
		// another field, and an invisible character shows as an escape.
		if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalRenamed, principalActor(id),
			fmt.Sprintf("from=%q to=%q", p.Name, name), now); err != nil {
			return err
		}
		out.Principal.Name = name
		out.Changed = true
		return nil
	})
	if err != nil {
		return RenameResult{}, err
	}
	return out, nil
}

// RecordRenameRefused appends a principal_rename_refused row for a person
// whose own rename was refused because the name is taken. It is written
// after, and outside, the rename's transaction, which rolled back. The row
// names no name: it counts toward the attempt limit and nothing else.
func (s *Store) RecordRenameRefused(ctx context.Context, id int64, now time.Time) error {
	_, err := s.AppendAuditEventAt(ctx, principalActor(id), AuditPrincipalRenameRefused, principalActor(id),
		"reason=name_taken", now.Truncate(time.Millisecond))
	return err
}

// renameLimitTx refuses a rename when the principal's own renames, or its own
// attempts, in the window ending at now already reach their limit.
func renameLimitTx(ctx context.Context, tx execer, id int64, lim RenameLimits, now time.Time) error {
	var until time.Time
	for _, c := range []struct {
		limit   int
		actions []string
	}{
		{lim.Renames, []string{AuditPrincipalRenamed}},
		{lim.Attempts, []string{AuditPrincipalRenamed, AuditPrincipalRenameRefused}},
	} {
		if c.limit <= 0 {
			continue
		}
		free, full, err := windowFullTx(ctx, tx, principalActor(id), c.actions, c.limit, now)
		if err != nil {
			return fmt.Errorf("store: count renames of principal %d: %w", id, err)
		}
		if full && free.After(until) {
			until = free
		}
	}
	if until.IsZero() {
		return nil
	}
	return &RenameLimitedError{Until: until}
}

// windowFullTx counts the audit rows with one of actions that subject made
// about itself in the RenameWindow ending at now. When they number limit or
// more it reports true, and when the next one fits: the window may hold more
// than limit (an operator's limits were never checked, or a limit was
// lowered), so that is once count-limit+1 of them have left it, at the time
// of the (count-limit+1)th oldest plus the window.
func windowFullTx(ctx context.Context, tx execer, subject string, actions []string, limit int, now time.Time) (time.Time, bool, error) {
	since := now.Add(-RenameWindow).UnixMilli()
	where := `FROM audit_events WHERE subject = ? AND actor = subject AND created_at > ? AND action IN (?` +
		strings.Repeat(", ?", len(actions)-1) + `)`
	args := []any{subject, since}
	for _, a := range actions {
		args = append(args, a)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+where, args...).Scan(&count); err != nil {
		return time.Time{}, false, err
	}
	if count < limit {
		return time.Time{}, false, nil
	}
	var freeing int64
	if err := tx.QueryRowContext(ctx, `SELECT created_at `+where+` ORDER BY created_at, id LIMIT 1 OFFSET ?`,
		append(args, count-limit)...).Scan(&freeing); err != nil {
		return time.Time{}, false, err
	}
	return time.UnixMilli(freeing).Add(RenameWindow), true, nil
}

// principalByIDTx is PrincipalByID inside a transaction, with an unknown
// principal as ErrPrincipalNotFound.
func principalByIDTx(ctx context.Context, tx execer, id int64) (Principal, error) {
	p, found, err := queryPrincipal(ctx, tx, id)
	if err == nil && !found {
		err = fmt.Errorf("store: find principal %d: %w", id, ErrPrincipalNotFound)
	}
	return p, err
}

// nameKeyMigration is the number of the migration that adds the name keys:
// its Go step is fillNameKeys.
const nameKeyMigration = 27

// fillNameKeys is the name-key migration's Go step: it computes both keys for
// every existing principal, then logs the groups of principals whose names
// clash (either key shared, directly or through another member of the group)
// and the principals whose names have empty keys. All are kept as they are:
// none is renamed or refused service, and a later rename of any of them must
// still not take another's key.
func fillNameKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, name FROM principals ORDER BY id")
	if err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}
	type row struct {
		id   int64
		name string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan principal name: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}

	// Union-find over ids: two principals are in one group when they share
	// either key.
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int64) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[max(ra, rb)] = min(ra, rb)
		}
	}
	firstFolded, firstSkel := map[string]int64{}, map[string]int64{}
	var blank []int64
	for _, r := range all {
		parent[r.id] = r.id
		keys := names.KeysOf(r.name)
		if keys.Empty() {
			blank = append(blank, r.id)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE principals SET name_key = ?, name_skel = ? WHERE id = ?",
			keys.Folded, keys.Skeletal, r.id); err != nil {
			return fmt.Errorf("store name keys of principal %d: %w", r.id, err)
		}
		if first, ok := firstFolded[keys.Folded]; ok {
			union(first, r.id)
		} else {
			firstFolded[keys.Folded] = r.id
		}
		if first, ok := firstSkel[keys.Skeletal]; ok {
			union(first, r.id)
		} else {
			firstSkel[keys.Skeletal] = r.id
		}
	}
	groups := map[int64][]int64{}
	for _, r := range all {
		root := find(r.id)
		groups[root] = append(groups[root], r.id)
	}
	var clashing [][]int64
	for _, ids := range groups {
		if len(ids) > 1 {
			clashing = append(clashing, ids)
		}
	}
	sort.Slice(clashing, func(i, j int) bool { return clashing[i][0] < clashing[j][0] })
	// Ids only: the names are in the database, and the log need not repeat
	// them.
	if len(clashing) > 0 {
		slog.WarnContext(ctx, "store: some existing principal names clash by case or look-alike characters; they are kept as they are, and a rename of any of them to a name that clashes with another is refused",
			"groups", len(clashing), "principals", clashing)
	}
	if len(blank) > 0 {
		slog.WarnContext(ctx, "store: some existing principal names have no character a reader can see; they are kept as they are, and an operator should rename them",
			"principals", blank)
	}
	return nil
}
