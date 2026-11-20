package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Account notices (issue #2042, child of #2013; Nick's decision B of
// 2026-11-19): what the server tells a member about their own account. The
// one kind so far says a password was set or reset through an enrolment or
// reset link another principal made. act_as_members lets an operator make
// such a link, and whoever opens it chooses the password; the audit log
// records the use, and the member is told here, on a sign-in of their own
// (the server decides that; see provenance.go).
//
// A notice belongs to one principal. Every read and every dismissal names
// that principal and no other, so one member's notices are never visible to
// anyone else and carry only the name of whoever made the link.

// noticePasswordSetByLinkTx records, in the transaction that stores the
// password, that principalID's password was set through a link made by
// makerID. A link the person made themselves (makerID is theirs) or one with
// no maker (0) writes nothing: nobody else was involved.
func noticePasswordSetByLinkTx(ctx context.Context, tx execer, principalID, makerID int64, now time.Time) error {
	if makerID == 0 || makerID == principalID {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_notices (principal_id, kind, by_name, at)
		 VALUES (?, ?, COALESCE((SELECT name FROM principals WHERE id = ?), ''), ?)`,
		principalID, string(schema.AccountNoticeKindPasswordSetByLink), makerID, now.UnixMilli()); err != nil {
		return fmt.Errorf("store: record account notice: %w", err)
	}
	return nil
}

// ListAccountNotices returns principalID's notices they have not dismissed,
// newest first. It reads no other principal's. The result is never nil.
func (s *Store) ListAccountNotices(ctx context.Context, principalID int64) ([]schema.AccountNoticeV1, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, by_name, at FROM account_notices
		 WHERE principal_id = ? AND dismissed_at IS NULL ORDER BY at DESC, id DESC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list account notices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.AccountNoticeV1{}
	for rows.Next() {
		var (
			n  schema.AccountNoticeV1
			at int64
		)
		var kind string
		if err := rows.Scan(&n.ID, &kind, &n.ByName, &at); err != nil {
			return nil, fmt.Errorf("store: list account notices: scan: %w", err)
		}
		n.Kind, n.At = schema.AccountNoticeKind(kind), schema.NewTimestamp(time.UnixMilli(at))
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list account notices: %w", err)
	}
	return out, nil
}

// DismissAccountNotice dismisses notice id for principalID, who must own it:
// another's id, and an id never issued, are ErrNotFound and write nothing,
// so the answer says nothing of whether it exists. One already dismissed is
// a no-op success.
func (s *Store) DismissAccountNotice(ctx context.Context, principalID, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE account_notices SET dismissed_at = ? WHERE id = ? AND principal_id = ? AND dismissed_at IS NULL`,
		now.UnixMilli(), id, principalID)
	if err != nil {
		return fmt.Errorf("store: dismiss account notice: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: dismiss account notice: %w", err)
	}
	if n == 1 {
		return nil
	}
	var one int
	switch err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM account_notices WHERE id = ? AND principal_id = ?", id, principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: dismiss account notice: %w", err)
	}
	return nil
}
