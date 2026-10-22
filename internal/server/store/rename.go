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
	"github.com/njdaniel/conch/pkg/schema"
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

// ErrNameReserved is returned by every write that sets a name when the name
// is reserved for hidden principals (reservedName, issue #445). The error
// that carries it is ErrDuplicate too: the name is not to be had.
var ErrNameReserved = errors.New("store: names beginning with " + schema.HiddenPrincipalNamePrefix + " are reserved")

// ErrGuestNameReserved is returned by every write that sets a permanent name
// when the name is in the reserved guest namespace (reservedGuestName, issue
// #705): only the store makes a guest's handle. It is ErrNameReserved too,
// and the error that carries it ErrDuplicate, as for the hidden prefix.
var ErrGuestNameReserved error = guestNameReservedError{}

// guestNameReservedError is ErrGuestNameReserved: its own message, and
// ErrNameReserved to errors.Is.
type guestNameReservedError struct{}

func (guestNameReservedError) Error() string {
	return "store: names beginning with " + schema.ReservedGuestNameKeyPrefix + " are reserved for guests"
}

func (guestNameReservedError) Is(target error) bool { return target == ErrNameReserved }

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

// CheckNameFree says, without writing anything, whether name could be
// claimed now by self (0 for a new principal), by claimNameTx's rule, and
// returns its error when not. It is the check the routes that make an
// account with a password (issue #711) make before they hash, so a name
// that is taken costs no Argon2 work; the writing transaction claims it
// again.
func (s *Store) CheckNameFree(ctx context.Context, name string, self int64) error {
	_, err := claimNameTx(ctx, s.db, name, self)
	return err
}

// claimNameTx is the one clash check, used by every path that creates or
// renames a principal: it returns name's keys, ErrNameInvalid when they are
// empty, or ErrDuplicate when a principal other than self holds either of
// them (the folded key against folded keys, the skeletal against skeletal),
// or when the name is reserved for hidden principals (reservedName) or for
// guests (reservedGuestName, with no bypass for an operator). self is
// the principal being renamed, or 0 on create. It must run in the transaction
// that writes the name, and the keys it returns are the ones that row must
// store.
func claimNameTx(ctx context.Context, tx execer, name string, self int64) (names.Keys, error) {
	keys := names.KeysOf(name)
	if keys.Empty() {
		return names.Keys{}, ErrNameInvalid
	}
	if reservedName(keys) {
		// A duplicate as well, so a caller that maps only ErrDuplicate still
		// refuses it as taken; one that knows ErrNameReserved says why.
		return names.Keys{}, fmt.Errorf("store: name %q: %w (%w)", name, ErrNameReserved, ErrDuplicate)
	}
	if reservedGuestName(keys) {
		return names.Keys{}, fmt.Errorf("store: name %q: %w (%w)", name, ErrGuestNameReserved, ErrDuplicate)
	}
	return keys, clashTx(ctx, tx, name, keys, self)
}

// hiddenKeys are the keys of schema.HiddenPrincipalNamePrefix.
var hiddenKeys = names.KeysOf(schema.HiddenPrincipalNamePrefix)

// reservedName reports whether a name with these keys is one only a hide may
// write (issue #445): either key begins with the same key of
// schema.HiddenPrincipalNamePrefix. Comparing keys, not spellings, reserves
// "HIDDEN-12", "hidden-l2" and "hidden-12" spelt with a Cyrillic i with
// "hidden-12", so nobody can take, ahead of a hide, a name that would clash
// with the one it gives.
func reservedName(keys names.Keys) bool {
	return strings.HasPrefix(keys.Folded, hiddenKeys.Folded) || strings.HasPrefix(keys.Skeletal, hiddenKeys.Skeletal)
}

// guestKeys are the keys of schema.ReservedGuestNameKeyPrefix.
var guestKeys = names.KeysOf(schema.ReservedGuestNameKeyPrefix)

// reservedGuestName reports whether a name with these keys is in the guest
// namespace (design §4.1): either key begins with the same key of
// schema.ReservedGuestNameKeyPrefix. As for the hidden prefix, comparing keys
// reserves "Guest-x", "GUESTS" and "guest" spelt with a Cyrillic e, so no
// permanent account can be mistaken for a guest by its name.
func reservedGuestName(keys names.Keys) bool {
	return strings.HasPrefix(keys.Folded, guestKeys.Folded) || strings.HasPrefix(keys.Skeletal, guestKeys.Skeletal)
}

// The clash queries of the two tables whose names are unique by their keys:
// principals (issue #441) and channels (issue #727).
const (
	principalClashQuery = "SELECT id FROM principals WHERE (name_key = ? OR name_skel = ?) AND id != ? LIMIT 1"
	channelClashQuery   = "SELECT id FROM channels WHERE (name_key = ? OR name_skel = ?) AND id != ? LIMIT 1"
)

// clashTx returns ErrDuplicate when a principal other than self holds either
// of keys.
func clashTx(ctx context.Context, tx execer, name string, keys names.Keys, self int64) error {
	return keyClashTx(ctx, tx, principalClashQuery, name, keys, self)
}

// keyClashTx returns ErrDuplicate when query, one of the clash queries above,
// finds a row other than self that holds either of keys.
func keyClashTx(ctx context.Context, tx execer, query, name string, keys names.Keys, self int64) error {
	var other int64
	err := tx.QueryRowContext(ctx, query, keys.Folded, keys.Skeletal, self).Scan(&other)
	switch {
	case err == nil:
		return fmt.Errorf("store: name %q: %w", name, ErrDuplicate)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("store: check name %q: %w", name, err)
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
// own old keys do not count, so "alice" may become "Alice"), nor be one
// reserved for hidden principals (ErrDuplicate, whoever renames), its keys
// must not be empty (ErrNameInvalid), and a principal_renamed row records
// both names. A guest, live or ended, is never renamed (ErrGuestNotHideable). name must already be a valid name
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
		// A guest's name is its handle (issue #705, review F10): neither
		// the guest nor an operator renames it, not even to itself.
		if p.guestTarget() {
			return ErrGuestNotHideable
		}
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
		if err := setNameTx(ctx, tx, actor, p, name, keys, now); err != nil {
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

// setNameTx writes name and the keys a claim returned for it to principal p,
// and appends principal_renamed. It is the one statement that changes a
// principal's name, for a rename and for a hide (issue #445).
func setNameTx(ctx context.Context, tx execer, actor string, p Principal, name string, keys names.Keys, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE principals SET name = ?, name_key = ?, name_skel = ? WHERE id = ?",
		name, keys.Folded, keys.Skeletal, p.ID)
	if isUniqueConstraintErr(err) {
		// The exact name is held by someone whose stored keys differ
		// from its keys now: keys computed from other Unicode tables.
		return fmt.Errorf("store: rename principal %d: %w", p.ID, ErrDuplicate)
	}
	if err != nil {
		return fmt.Errorf("store: rename principal %d: %w", p.ID, err)
	}
	// Quoted, so a name with spaces or "to=" in it cannot be read as
	// another field, and an invisible character shows as an escape.
	return appendAuditEventTx(ctx, tx, actor, AuditPrincipalRenamed, principalActor(p.ID),
		fmt.Sprintf("from=%q to=%q", p.Name, name), now)
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
	all, err := readNamedRows(ctx, tx, "SELECT id, name FROM principals ORDER BY id")
	if err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}
	for _, r := range all {
		keys := names.KeysOf(r.name)
		if _, err := tx.ExecContext(ctx, "UPDATE principals SET name_key = ?, name_skel = ? WHERE id = ?",
			keys.Folded, keys.Skeletal, r.id); err != nil {
			return fmt.Errorf("store name keys of principal %d: %w", r.id, err)
		}
	}
	clashing, blank := nameKeyGroups(all)
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

// namedRow is a row's id and name, as a key migration's Go step reads them.
type namedRow struct {
	id   int64
	name string
}

// readNamedRows reads every (id, name) that query selects, in id order.
func readNamedRows(ctx context.Context, tx *sql.Tx, query string) ([]namedRow, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	var all []namedRow
	for rows.Next() {
		var r namedRow
		if err := rows.Scan(&r.id, &r.name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return all, rows.Err()
}

// nameKeyGroups returns the groups of rows whose names clash (either key
// shared, directly or through another member of the group), each in id order
// and ordered by their first id, and the ids of the rows whose names have
// empty keys. all must be in id order.
func nameKeyGroups(all []namedRow) (clashing [][]int64, blank []int64) {
	// Union-find over ids: two rows are in one group when they share either
	// key.
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
	for _, r := range all {
		parent[r.id] = r.id
		keys := names.KeysOf(r.name)
		if keys.Empty() {
			blank = append(blank, r.id)
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
	for _, ids := range groups {
		if len(ids) > 1 {
			clashing = append(clashing, ids)
		}
	}
	sort.Slice(clashing, func(i, j int) bool { return clashing[i][0] < clashing[j][0] })
	return clashing, blank
}
