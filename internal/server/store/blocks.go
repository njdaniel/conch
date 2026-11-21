package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Personal blocks (issue #1832, docs/design/blocks.md). A block is a row
// (owner, blocked): the owner would rather not see the blocked principal's
// messages or be alerted about them. It is the owner's preference and
// nothing else: no membership, visibility or permission predicate reads this
// table, so a block changes nothing anyone can read, and nothing the blocked
// principal can observe. Only three things consult it, all for the owner:
// the owner's own list (ListBlocks), the owner's pending feed
// (ListPendingNotifications) and the owner's push wakes (PushTargets).
// IsBlocked is the question a later feature asks for the owner (direct
// conversations, #2018: a blocked person's new conversation is refused, and
// told only "unavailable").
//
// Nothing here writes an audit row (see migration 51).

var (
	// ErrBlockSelf is returned by PutBlock when the owner names themself.
	ErrBlockSelf = errors.New("store: a principal cannot block themself")
	// ErrBlockLimit is returned by PutBlock when the owner's list already
	// holds schema.MaxBlocks principals and the one named is not among them.
	ErrBlockLimit = errors.New("store: block list is full")
)

// blockColumns is what an entry is read as: the block joined to the blocked
// principal's present name and kind.
const blockColumns = `b.blocked_id, p.name, p.kind, b.created_at
	FROM blocks b JOIN principals p ON p.id = b.blocked_id`

func scanBlock(row interface{ Scan(...any) error }) (schema.BlockV1, error) {
	var b schema.BlockV1
	var kind string
	var at int64
	if err := row.Scan(&b.PrincipalID, &b.Name, &kind, &at); err != nil {
		return schema.BlockV1{}, err
	}
	b.Kind = schema.PrincipalKind(kind)
	b.CreatedAt = schema.NewTimestamp(time.UnixMilli(at))
	return b, nil
}

// ListBlocks returns ownerID's block list, oldest first, never nil. An owner
// with no blocks, or an id that is nobody, gets an empty list.
func (s *Store) ListBlocks(ctx context.Context, ownerID int64) ([]schema.BlockV1, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+blockColumns+`
		WHERE b.owner_id = ? ORDER BY b.created_at, b.blocked_id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("store: list blocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.BlockV1{}
	for rows.Next() {
		b, err := scanBlock(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list blocks: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list blocks: %w", err)
	}
	return out, nil
}

// listedPrincipalSQL is the one statement of "viewer may name this principal
// by id", for a principals row aliased p: not hidden, known to viewer
// (knownPrincipalsSQL, with seeAll the viewer's see_member_directory), and one
// the directory lists for them (a member, an active agent, or an active person
// viewer shares a channel with or invited). A disabled person or agent, or a
// guest viewer shares no channel with, is outside it. The two surfaces that
// answer an id probe, PutBlock and a new-pair OpenDM, both build their
// condition from it, so they answer the same set and cannot drift. The
// returned args fill its placeholders in order. The fragment is built from
// constants and never from a request.
func listedPrincipalSQL(viewer int64, seeAll bool) (cond string, args []any) {
	known, knownArgs, _ := knownPrincipalsSQL(viewer, seeAll)
	cond = "p.hidden_at IS NULL AND (" + known + ")" +
		" AND ((" + memberPredicateSQL + ") OR (p.kind = 'agent' AND p.disabled_at IS NULL) OR (p.disabled_at IS NULL AND " + knowsCond("?") + "))"
	return cond, append(knownArgs, viewer, viewer, viewer)
}

// PutBlock adds blockedID to ownerID's list and returns the entry, with
// created=true when the row is new. It is idempotent: a principal already on
// the list is answered as it is, with its original time, whether or not the
// list is full. It fails with ErrBlockSelf when the two are the same,
// ErrNotFound when blockedID is no principal, a hidden one, one the owner
// may not know (knownPrincipalsSQL, with seeAll the owner's
// see_member_directory), or one the directory does not list for them: a
// disabled person or agent, or a guest the owner shares no channel with. It
// is ListDirectory's rule, so a PUT cannot be used to probe for principals or
// read their names, and
// ErrBlockLimit when the list already holds schema.MaxBlocks others. The
// count and the insert are in one immediate transaction, so two concurrent
// PUTs cannot pass the limit together.
func (s *Store) PutBlock(ctx context.Context, ownerID, blockedID int64, seeAll bool, now time.Time) (entry schema.BlockV1, created bool, err error) {
	if ownerID == blockedID {
		return schema.BlockV1{}, false, ErrBlockSelf
	}
	err = s.withImmediateTx(ctx, func(tx execer) error {
		cond, args := listedPrincipalSQL(ownerID, seeAll)
		var one int
		switch err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM principals p WHERE p.id = ? AND "+cond, // #nosec G202 -- every fragment is a constant
			append([]any{blockedID}, args...)...).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		}
		existing, err := scanBlock(tx.QueryRowContext(ctx, `SELECT `+blockColumns+`
			WHERE b.owner_id = ? AND b.blocked_id = ?`, ownerID, blockedID))
		switch {
		case err == nil:
			entry = existing
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks WHERE owner_id = ?`, ownerID).Scan(&n); err != nil {
			return err
		}
		if n >= schema.MaxBlocks {
			return ErrBlockLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blocks (owner_id, blocked_id, created_at) VALUES (?, ?, ?)`,
			ownerID, blockedID, now.UnixMilli()); err != nil {
			return err
		}
		entry, err = scanBlock(tx.QueryRowContext(ctx, `SELECT `+blockColumns+`
			WHERE b.owner_id = ? AND b.blocked_id = ?`, ownerID, blockedID))
		created = err == nil
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrBlockLimit) {
			return schema.BlockV1{}, false, err
		}
		return schema.BlockV1{}, false, fmt.Errorf("store: put block: %w", err)
	}
	return entry, created, nil
}

// DeleteBlock removes blockedID from ownerID's list and reports whether a row
// was there. Removing a block that does not exist, of anyone or of no one, is
// not an error: the answer is the same either way.
func (s *Store) DeleteBlock(ctx context.Context, ownerID, blockedID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM blocks WHERE owner_id = ? AND blocked_id = ?`, ownerID, blockedID)
	if err != nil {
		return false, fmt.Errorf("store: delete block: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete block: %w", err)
	}
	return n > 0, nil
}

// IsBlocked reports whether ownerID blocks blockedID. It is asked only on the
// owner's behalf; its answer must never reach blockedID.
func (s *Store) IsBlocked(ctx context.Context, ownerID, blockedID int64) (bool, error) {
	var one int
	switch err := s.db.QueryRowContext(ctx, `SELECT 1 FROM blocks WHERE owner_id = ? AND blocked_id = ?`, ownerID, blockedID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: is blocked: %w", err)
	}
	return true, nil
}

// blockedSetOf returns the ids ownerID blocks, for the owner's pending feed.
func blockedSetOf(ctx context.Context, q execer, ownerID int64) (map[int64]struct{}, error) {
	rows, err := q.QueryContext(ctx, `SELECT blocked_id FROM blocks WHERE owner_id = ?`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("store: blocked set: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("store: blocked set: %w", err)
	}
	out := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}
