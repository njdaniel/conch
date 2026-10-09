package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Channel membership (issue #90, ADR-003). A channel's member list is the
// visibility boundary the server enforces when there is an authenticated
// caller. Audit events are written in the same transaction as the change.

// Audit actions written by membership changes.
const (
	AuditMemberAdded   = "member_added"
	AuditMemberRemoved = "member_removed"
)

// ChannelMember is one row of a channel's member list. AddedBy is 0 when
// unknown (rows backfilled by the upgrade migration, or added with
// authentication off).
type ChannelMember struct {
	PrincipalID int64
	AddedBy     int64
	CreatedAt   time.Time
}

func channelSubject(id int64) string { return fmt.Sprintf("channel:%d", id) }

func memberDetail(principalID int64) string { return fmt.Sprintf("principal=%d", principalID) }

// nullableID maps the "unknown" zero id to SQL NULL.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// CreateChannelAs creates a channel. When creatorID is non-zero the creator
// becomes the channel's only member (added_by is the creator) and a
// member_added event with the given actor is written, all in the creating
// transaction. With creatorID 0 (no authenticated caller) the channel has no
// members. Names are unique (ErrDuplicate).
func (s *Store) CreateChannelAs(ctx context.Context, actor, name string, creatorID int64) (Channel, error) {
	now := time.Now().Truncate(time.Millisecond)
	var ch Channel
	err := s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO channels (name, created_at) VALUES (?, ?)", name, now.UnixMilli())
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: create channel %q: %w", name, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: create channel %q: %w", name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create channel %q: %w", name, err)
		}
		ch = Channel{ID: id, Name: name, CreatedAt: now}
		if creatorID == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (?, ?, ?, ?)",
			id, creatorID, creatorID, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: add creator %d to channel %d: %w", creatorID, id, err)
		}
		return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(id), memberDetail(creatorID), now)
	})
	if err != nil {
		return Channel{}, err
	}
	return ch, nil
}

// checkMemberTargetTx reports ErrNotFound for an unknown channel and
// ErrPrincipalNotFound for an unknown principal.
func checkMemberTargetTx(ctx context.Context, tx execer, channelID, principalID int64) error {
	var one int
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE id = ?", channelID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
	case err != nil:
		return fmt.Errorf("store: check channel %d: %w", channelID, err)
	}
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM principals WHERE id = ?", principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPrincipalNotFound
	case err != nil:
		return fmt.Errorf("store: check principal %d: %w", principalID, err)
	}
	return nil
}

// AddChannelMember makes principalID a member of channelID. addedBy is the
// acting principal, or 0 when unknown. It is idempotent: adding an existing
// member returns added=false and writes nothing, including no audit event. It
// returns ErrNotFound for an unknown channel and ErrPrincipalNotFound for an
// unknown principal.
func (s *Store) AddChannelMember(ctx context.Context, actor string, channelID, principalID, addedBy int64) (added bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if err := checkMemberTargetTx(ctx, tx, channelID, principalID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (channel_id, principal_id) DO NOTHING`,
			channelID, principalID, nullableID(addedBy), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		if n == 0 {
			return nil
		}
		added = true
		return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), memberDetail(principalID), now)
	})
	if err != nil {
		return false, err
	}
	return added, nil
}

// RemoveChannelMember removes principalID from channelID. It is idempotent:
// removing a non-member returns removed=false and writes nothing, including
// no audit event. Errors are as for AddChannelMember. The caller must close
// the principal's live subscriptions after this returns.
func (s *Store) RemoveChannelMember(ctx context.Context, actor string, channelID, principalID int64) (removed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if err := checkMemberTargetTx(ctx, tx, channelID, principalID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"DELETE FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID)
		if err != nil {
			return fmt.Errorf("store: remove member %d from channel %d: %w", principalID, channelID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: remove member %d from channel %d: %w", principalID, channelID, err)
		}
		if n == 0 {
			return nil
		}
		removed = true
		return appendAuditEventTx(ctx, tx, actor, AuditMemberRemoved, channelSubject(channelID), memberDetail(principalID), now)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// IsChannelMember reports whether principalID is a member of channelID.
func (s *Store) IsChannelMember(ctx context.Context, channelID, principalID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
	}
	return true, nil
}

// ListChannelMembers returns channelID's members ordered by principal id. It
// returns an empty, non-nil slice when there are none.
func (s *Store) ListChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT principal_id, added_by, created_at FROM channel_members
		 WHERE channel_id = ? ORDER BY principal_id ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	members := []ChannelMember{}
	for rows.Next() {
		var m ChannelMember
		var addedBy sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&m.PrincipalID, &addedBy, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
		}
		m.AddedBy = addedBy.Int64
		m.CreatedAt = time.UnixMilli(createdAt)
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
	}
	return members, nil
}

// ListChannelsForPrincipal returns the channels principalID is a member of,
// ordered by id ascending. It returns an empty, non-nil slice when there are
// none.
func (s *Store) ListChannelsForPrincipal(ctx context.Context, principalID int64) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.name, c.created_at FROM channels c
		 JOIN channel_members m ON m.channel_id = c.id
		 WHERE m.principal_id = ? ORDER BY c.id ASC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list channels of principal %d: %w", principalID, err)
	}
	defer func() { _ = rows.Close() }()
	channels := []Channel{}
	for rows.Next() {
		var ch Channel
		var createdAt int64
		if err := rows.Scan(&ch.ID, &ch.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list channels of principal %d: %w", principalID, err)
		}
		ch.CreatedAt = time.UnixMilli(createdAt)
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels of principal %d: %w", principalID, err)
	}
	return channels, nil
}
