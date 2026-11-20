package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// CurrentPrivateCall is the participant's non-terminal attempt, if any.
func (s *Store) CurrentPrivateCall(ctx context.Context, actor PrivateCallActor, clock func() time.Time) (schema.PrivateCallSnapshotV1, error) {
	out := schema.PrivateCallSnapshotV1{Schema: schema.PrivateCallSnapshotSchemaV1}
	err := s.withImmediateTx(ctx, func(tx execer) error {
		now := clock().Truncate(time.Millisecond)
		out.TakenAt = schema.NewTimestamp(now)
		if live, err := privateCallRequestLive(ctx, tx, actor, now); err != nil {
			return err
		} else if !live {
			return ErrPrivateCallNotFound
		}
		c, err := livePrivateCallHeldBy(ctx, tx, actor.PrincipalID, now)
		if c.ID != "" {
			doc := c.view(actor)
			out.Call = &doc
		}
		return err
	})
	return out, err
}

// ListPrivateCalls returns a participant's finished attempts in the pair.
// The cursor is the opaque ID of a visible history entry in this pair.
func (s *Store) ListPrivateCalls(ctx context.Context, conversationID int64, actor PrivateCallActor, cursor string, now time.Time) (schema.ListPrivateCallsResponseV1, error) {
	out := schema.ListPrivateCallsResponseV1{Calls: make([]schema.PrivateCallV1, 0)}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if live, err := privateCallRequestLive(ctx, tx, actor, now); err != nil {
		return out, err
	} else if !live {
		return out, ErrPrivateCallPair
	}
	// Pair membership on its own says nothing about whether this person has
	// seen this conversation. Until the DM visibility check is wired, only a
	// call visible to this viewer can establish that they know the pair.
	var known bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM private_call_pairs p
		WHERE p.conversation_id = ?1 AND ?2 IN (p.lo, p.hi) AND EXISTS (
			SELECT 1 FROM private_calls c WHERE c.conversation_id = p.conversation_id
			AND (c.caller = ?2 OR (c.callee = ?2 AND c.state <> 'unavailable'))))`,
		conversationID, actor.PrincipalID).Scan(&known)
	if err != nil {
		return out, err
	}
	if !known {
		return out, ErrPrivateCallPair
	}
	rows, err := privateCallHistoryRows(ctx, tx, conversationID, actor.PrincipalID, cursor)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(out.Calls) == schema.MaxPrivateCallHistoryPage {
			out.NextCursor = out.Calls[len(out.Calls)-1].ID
			break
		}
		c, err := scanPrivateCall(rows)
		if err != nil {
			return out, err
		}
		out.Calls = append(out.Calls, c.view(actor))
	}
	return out, rows.Err()
}

func privateCallHistoryRows(ctx context.Context, tx execer, conversationID, principalID int64, cursor string) (*sql.Rows, error) {
	const visible = `c.conversation_id = ? AND ? IN (c.caller, c.callee) AND c.ended_at IS NOT NULL
		AND c.state <> 'unavailable' AND (c.state <> 'missed' OR c.callee = ?)`
	var created int64
	if cursor != "" {
		err := tx.QueryRowContext(ctx, `SELECT c.created_at FROM private_calls c WHERE `+visible+` AND c.id = ?`,
			conversationID, principalID, principalID, cursor).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPrivateCallCursor
		}
		if err != nil {
			return nil, err
		}
	}
	return tx.QueryContext(ctx, `SELECT `+privateCallColumns+` FROM private_calls c WHERE `+visible+`
		AND (? = '' OR (c.created_at, c.id) < (?, ?))
		ORDER BY c.created_at DESC, c.id DESC LIMIT ?`,
		conversationID, principalID, principalID, cursor, created, cursor, schema.MaxPrivateCallHistoryPage+1)
}
