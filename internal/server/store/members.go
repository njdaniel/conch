package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Channel membership (issue #90, ADR-003). A channel's member list is the
// visibility boundary the server enforces when there is an authenticated
// caller. Audit events are written in the same transaction as the change.

// Audit actions written by membership changes.
const (
	// AuditChannelCreated records a new channel (issue #445): actor the
	// creator, subject the channel, detail name=<name> quoted.
	AuditChannelCreated = "channel_created"
	AuditMemberAdded    = "member_added"
	AuditMemberRemoved  = "member_removed"
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
	// HistoryAfter is the membership's history bound (issue #705): the
	// channel's last message id when a guest was admitted, 0 for none.
	HistoryAfter int64
	// Guest is how the member is rendered while it is a guest; nil for
	// everyone else.
	Guest *schema.GuestAttributionV1
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

// CreateChannelAs creates a channel and writes channel_created (issue #445).
// When creatorID is non-zero the creator becomes the channel's only member
// (added_by is the creator) and a member_added event is written. Both rows
// are attributed to actor and written in the creating transaction. With
// creatorID 0 (no authenticated caller) the channel has no members. Names are
// unique (ErrDuplicate).
func (s *Store) CreateChannelAs(ctx context.Context, actor, name string, creatorID int64) (Channel, error) {
	// SQLite stores timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a create response exactly
	// describes what a subsequent read returns.
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
		// Quoted, as a rename's names are, so a name with spaces in it reads
		// as one field.
		if err := appendAuditEventTx(ctx, tx, actor, AuditChannelCreated, channelSubject(id), fmt.Sprintf("name=%q", name), now); err != nil {
			return err
		}
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
		// A guest never decides approvals and every membership it gets has
		// a fresh history bound (issue #705, review F1): the store holds to
		// it whatever the caller asked, and refuses to grant the right.
		guest, err := guestMemberTx(ctx, tx, principalID, canDecide)
		if err != nil {
			return err
		}
		if guest {
			added, err = addGuestMemberTx(ctx, tx, actor, channelID, principalID, addedBy, now)
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
		detail := memberDetail(principalID)
		if !decide {
			// Recorded, so the audit trail says who held the right when.
			detail += " can_decide=0"
		}
		return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), detail, now)
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
// inside the caller's transaction. A guest is never given it.
func setCanDecideTx(ctx context.Context, tx execer, actor string, channelID, principalID int64, canDecide bool, now time.Time) (bool, error) {
	if canDecide {
		if _, err := guestMemberTx(ctx, tx, principalID, &canDecide); err != nil {
			return false, err
		}
	}
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
		removed, err = removeChannelMemberTx(ctx, tx, actor, channelID, principalID, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// removeChannelMemberTx is RemoveChannelMember inside the caller's
// transaction, for a channel and principal known to exist. Hiding a principal
// (issue #445) calls it once per channel, so every removal is audited the
// same way.
func removeChannelMemberTx(ctx context.Context, tx execer, actor string, channelID, principalID int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx,
		"DELETE FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID)
	if err != nil {
		return false, fmt.Errorf("store: remove member %d from channel %d: %w", principalID, channelID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: remove member %d from channel %d: %w", principalID, channelID, err)
	}
	if n == 0 {
		return false, nil
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditMemberRemoved, channelSubject(channelID), memberDetail(principalID), now); err != nil {
		return false, err
	}
	// A net member must be a channel member (ADR-005), so the net seats go
	// in the same transaction.
	return true, removeNetMembershipsTx(ctx, tx, actor, channelID, principalID, now)
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

// guestMemberTx reports whether principalID is a guest, for a membership
// write asking for canDecide (nil: the default). A guest asked to be given
// the right is ErrGuestNotAllowed, and one whose visit has ended is
// ErrGuestEnded: it gets no membership at all.
func guestMemberTx(ctx context.Context, tx execer, principalID int64, canDecide *bool) (bool, error) {
	var state string
	switch err := tx.QueryRowContext(ctx, "SELECT state FROM guest_visits WHERE principal_id = ?", principalID).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read guest state of %d: %w", principalID, err)
	}
	switch schema.GuestState(state) {
	case schema.GuestStateActive:
		if canDecide != nil && *canDecide {
			return true, ErrGuestNotAllowed
		}
		return true, nil
	case schema.GuestStateEnded:
		return true, ErrGuestEnded
	}
	return false, nil
}

// addGuestMemberTx admits guest principalID to channelID unless it is already
// a member, reporting whether it was added.
func addGuestMemberTx(ctx context.Context, tx execer, actor string, channelID, principalID, addedBy int64, now time.Time) (bool, error) {
	var one int
	switch err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
	}
	return true, admitGuestTx(ctx, tx, actor, channelID, principalID, addedBy, now)
}

// ListChannelMembers returns channelID's members, with each principal's name
// and kind, ordered by principal id. It returns an empty, non-nil slice when
// there are none. The join cannot drop a member: principal_id is a foreign
// key, and principals are never deleted.
func (s *Store) ListChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.principal_id, m.added_by, m.created_at, p.name, p.kind, m.can_decide, m.history_after, v.handle, v.label
		 FROM channel_members m
		 JOIN principals p ON p.id = m.principal_id
		 LEFT JOIN guest_visits v ON v.principal_id = m.principal_id AND v.state = 'active'
		 WHERE m.channel_id = ? ORDER BY m.principal_id ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	members := []ChannelMember{}
	for rows.Next() {
		var m ChannelMember
		var addedBy, historyAfter sql.NullInt64
		var handle, label sql.NullString
		var createdAt int64
		var canDecide int
		if err := rows.Scan(&m.PrincipalID, &addedBy, &createdAt, &m.Name, &m.Kind, &canDecide, &historyAfter, &handle, &label); err != nil {
			return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
		}
		m.AddedBy = addedBy.Int64
		m.HistoryAfter = historyAfter.Int64
		if handle.Valid {
			a := guestAttribution(handle.String, label.String)
			m.Guest = &a
		}
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
