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
	// AuditMemberDecideGranted and AuditMemberDecideRevoked record a change
	// to a member's right to decide the channel's approvals (issue #450).
	AuditMemberDecideGranted = "member_decide_granted"
	AuditMemberDecideRevoked = "member_decide_revoked"
)

// ErrNotMember is returned by SetChannelMemberCanDecide when the principal is
// not a member of the channel. Nothing is written.
var ErrNotMember = errors.New("store: not a member of the channel")

// ChannelMember is one row of a channel's member list. AddedBy is 0 when
// unknown (rows backfilled by the upgrade migration, or added with
// authentication off). Name and Kind come from the principal row. CanDecide
// is whether the member may decide the channel's approvals (issue #450); it
// matters for human members only, since agents never decide.
type ChannelMember struct {
	PrincipalID int64
	AddedBy     int64
	CreatedAt   time.Time
	Name        string
	Kind        PrincipalKind
	CanDecide   bool
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

// AddChannelMember makes principalID a member of channelID, with the right
// to decide the channel's approvals. addedBy is the acting principal, or 0
// when unknown. It is idempotent: adding an existing member returns
// added=false and writes nothing, including no audit event. It returns
// ErrNotFound for an unknown channel and ErrPrincipalNotFound for an unknown
// principal.
func (s *Store) AddChannelMember(ctx context.Context, actor string, channelID, principalID, addedBy int64) (added bool, err error) {
	return s.PutChannelMember(ctx, actor, channelID, principalID, addedBy, nil)
}

// PutChannelMember is AddChannelMember with the decide right chosen (issue
// #450). canDecide nil means the default for a new member (true) and no
// change for an existing one. Otherwise a new member is added with it, and
// an existing member's right is set to it, audited only when it changes. It
// all happens in one transaction, so a member added without the right never
// holds it for a moment. added reports whether the principal was new.
func (s *Store) PutChannelMember(ctx context.Context, actor string, channelID, principalID, addedBy int64, canDecide *bool) (added bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if err := checkMemberTargetTx(ctx, tx, channelID, principalID); err != nil {
			return err
		}
		decide := canDecide == nil || *canDecide
		res, err := tx.ExecContext(ctx,
			`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (channel_id, principal_id) DO NOTHING`,
			channelID, principalID, nullableID(addedBy), now.UnixMilli(), boolInt(decide))
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		if n == 0 {
			if canDecide == nil {
				return nil
			}
			_, err := setCanDecideTx(ctx, tx, actor, channelID, principalID, *canDecide, now)
			return err
		}
		added = true
		return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), memberDetail(principalID), now)
	})
	if err != nil {
		return false, err
	}
	return added, nil
}

// SetChannelMemberCanDecide grants or revokes principalID's right to decide
// channelID's approvals (issue #450). It returns changed=false and writes
// nothing when the member already has that setting, ErrNotMember when the
// principal is not a member, and the errors of AddChannelMember for an
// unknown channel or principal. A change is audited.
func (s *Store) SetChannelMemberCanDecide(ctx context.Context, actor string, channelID, principalID int64, canDecide bool) (changed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if err := checkMemberTargetTx(ctx, tx, channelID, principalID); err != nil {
			return err
		}
		changed, err = setCanDecideTx(ctx, tx, actor, channelID, principalID, canDecide, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// setCanDecideTx sets an existing member's decide right and audits a change,
// inside the caller's transaction.
func setCanDecideTx(ctx context.Context, tx execer, actor string, channelID, principalID int64, canDecide bool, now time.Time) (bool, error) {
	var current int
	switch err := tx.QueryRowContext(ctx,
		"SELECT can_decide FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&current); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotMember
	case err != nil:
		return false, fmt.Errorf("store: read decide right of %d in channel %d: %w", principalID, channelID, err)
	}
	if (current == 1) == canDecide {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE channel_members SET can_decide = ? WHERE channel_id = ? AND principal_id = ?",
		boolInt(canDecide), channelID, principalID); err != nil {
		return false, fmt.Errorf("store: set decide right of %d in channel %d: %w", principalID, channelID, err)
	}
	action := AuditMemberDecideRevoked
	if canDecide {
		action = AuditMemberDecideGranted
	}
	return true, appendAuditEventTx(ctx, tx, actor, action, channelSubject(channelID), memberDetail(principalID), now)
}

// CanDecideApprovals reports whether principalID is a member of channelID
// holding the right to decide its approvals (issue #450). A non-member
// cannot.
func (s *Store) CanDecideApprovals(ctx context.Context, channelID, principalID int64) (bool, error) {
	var can int
	err := s.db.QueryRowContext(ctx,
		"SELECT can_decide FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&can)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check decide right of %d in channel %d: %w", principalID, channelID, err)
	}
	return can == 1, nil
}

// DecidingChannels returns the ids of the channels where principalID is a
// member holding the right to decide approvals, as a set.
func (s *Store) DecidingChannels(ctx context.Context, principalID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT channel_id FROM channel_members WHERE principal_id = ? AND can_decide = 1", principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list deciding channels of %d: %w", principalID, err)
	}
	defer func() { _ = rows.Close() }()
	set := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list deciding channels of %d: %w", principalID, err)
		}
		set[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deciding channels of %d: %w", principalID, err)
	}
	return set, nil
}

// RemoveChannelMember removes principalID from channelID. It is idempotent:
// removing a non-member returns removed=false and writes nothing, including
// no audit event. Removing a member also takes them off every net of the
// channel, each removal audited. Errors are as for AddChannelMember. The caller must close
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
		if err := appendAuditEventTx(ctx, tx, actor, AuditMemberRemoved, channelSubject(channelID), memberDetail(principalID), now); err != nil {
			return err
		}
		// A net member must be a channel member (ADR-005), so the net seats go
		// in the same transaction.
		return removeNetMembershipsTx(ctx, tx, actor, channelID, principalID, now)
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

// ListChannelMembers returns channelID's members, with each principal's name
// and kind, ordered by principal id. It returns an empty, non-nil slice when
// there are none. The join cannot drop a member: principal_id is a foreign
// key, and principals are never deleted.
func (s *Store) ListChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.principal_id, m.added_by, m.created_at, p.name, p.kind, m.can_decide FROM channel_members m
		 JOIN principals p ON p.id = m.principal_id
		 WHERE m.channel_id = ? ORDER BY m.principal_id ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	members := []ChannelMember{}
	for rows.Next() {
		var m ChannelMember
		var addedBy sql.NullInt64
		var createdAt int64
		var canDecide int
		if err := rows.Scan(&m.PrincipalID, &addedBy, &createdAt, &m.Name, &m.Kind, &canDecide); err != nil {
			return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
		}
		m.AddedBy = addedBy.Int64
		m.CreatedAt = time.UnixMilli(createdAt)
		m.CanDecide = canDecide == 1
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

// boolInt is the SQLite form of a boolean column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
