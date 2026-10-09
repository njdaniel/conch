package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Nets (issue #115, ADR-005). A net is a named subset of one channel's
// members; a principal on a net is either a member (listens and transmits) or
// a monitor (listens only). This file stores nets and their rosters. It knows
// nothing about messages: scoped posting and reading build on these queries.
// Every change appends its audit event in the same transaction.

// Audit actions written by net changes. The audit subject is "net:<id>".
const (
	AuditNetCreated           = "net_created"
	AuditNetArchived          = "net_archived"
	AuditNetMemberAdded       = "net_member_added"
	AuditNetMemberRoleChanged = "net_member_role_changed"
	AuditNetMemberRemoved     = "net_member_removed"
)

// ErrNotChannelMember is returned when a principal is put on a net without
// being a member of the net's channel.
var ErrNotChannelMember = errors.New("store: principal is not a member of the channel")

// Net is a stored net. CreatedBy is 0 when unknown (created with
// authentication off). ArchivedAt is nil for a live net.
type Net struct {
	ID         int64
	ChannelID  int64
	Name       string
	CreatedBy  int64
	CreatedAt  time.Time
	ArchivedAt *time.Time
}

// NetMember is one principal on a net. AddedBy is 0 when unknown.
type NetMember struct {
	PrincipalID int64
	Role        schema.NetRole
	AddedBy     int64
	CreatedAt   time.Time
}

// NetMemberChange says what PutNetMember did.
type NetMemberChange int

// Outcomes of PutNetMember.
const (
	// NetMemberUnchanged means the principal already had that role; nothing
	// was written and nothing was audited.
	NetMemberUnchanged NetMemberChange = iota
	NetMemberAdded
	NetMemberRoleChanged
)

func netSubject(id int64) string { return fmt.Sprintf("net:%d", id) }

func netDetail(channelID int64, extra string) string {
	d := fmt.Sprintf("channel=%d", channelID)
	if extra != "" {
		d += " " + extra
	}
	return d
}

// liveNetTx resolves the live (non-archived) net named name in channelID.
func liveNetTx(ctx context.Context, tx execer, channelID int64, name string) (Net, error) {
	return scanNet(tx.QueryRowContext(ctx,
		`SELECT id, channel_id, name, created_by, created_at, archived_at FROM nets
		 WHERE channel_id = ? AND name = ? AND archived_at IS NULL`, channelID, name))
}

func scanNet(row interface{ Scan(dest ...any) error }) (Net, error) {
	var n Net
	var createdBy, archivedAt sql.NullInt64
	var createdAt int64
	err := row.Scan(&n.ID, &n.ChannelID, &n.Name, &createdBy, &createdAt, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Net{}, ErrNotFound
	}
	if err != nil {
		return Net{}, fmt.Errorf("store: read net: %w", err)
	}
	n.CreatedBy = createdBy.Int64
	n.CreatedAt = time.UnixMilli(createdAt)
	if archivedAt.Valid {
		t := time.UnixMilli(archivedAt.Int64)
		n.ArchivedAt = &t
	}
	return n, nil
}

// CreateNet creates an empty net named name in channelID and writes net_created.
// createdBy is the acting principal, or 0 when unknown. It returns ErrNotFound
// for an unknown channel and ErrDuplicate when the channel already has a live
// net of that name; the name of an archived net is free again. Name syntax is
// the caller's job (schema.ValidateNetName).
func (s *Store) CreateNet(ctx context.Context, actor string, channelID int64, name string, createdBy int64) (Net, error) {
	now := time.Now().Truncate(time.Millisecond)
	var n Net
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE id = ?", channelID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: check channel %d: %w", channelID, err)
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO nets (channel_id, name, created_by, created_at) VALUES (?, ?, ?, ?)",
			channelID, name, nullableID(createdBy), now.UnixMilli())
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: create net %q in channel %d: %w", name, channelID, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: create net %q in channel %d: %w", name, channelID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create net %q in channel %d: %w", name, channelID, err)
		}
		n = Net{ID: id, ChannelID: channelID, Name: name, CreatedBy: createdBy, CreatedAt: now}
		return appendAuditEventTx(ctx, tx, actor, AuditNetCreated, netSubject(id), netDetail(channelID, "name="+name), now)
	})
	if err != nil {
		return Net{}, err
	}
	return n, nil
}

// ArchiveNet archives the live net named name in channelID and writes
// net_archived. Its roster is kept. It returns ErrNotFound when the channel has
// no live net of that name, which includes a net that is already archived.
func (s *Store) ArchiveNet(ctx context.Context, actor string, channelID int64, name string) (Net, error) {
	now := time.Now().Truncate(time.Millisecond)
	var n Net
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		if n, err = liveNetTx(ctx, tx, channelID, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE nets SET archived_at = ? WHERE id = ?", now.UnixMilli(), n.ID); err != nil {
			return fmt.Errorf("store: archive net %d: %w", n.ID, err)
		}
		t := now
		n.ArchivedAt = &t
		return appendAuditEventTx(ctx, tx, actor, AuditNetArchived, netSubject(n.ID), netDetail(channelID, "name="+name), now)
	})
	if err != nil {
		return Net{}, err
	}
	return n, nil
}

// NetByName returns the live net named name in channelID, or ErrNotFound. An
// archived net is not found.
func (s *Store) NetByName(ctx context.Context, channelID int64, name string) (Net, error) {
	return liveNetTx(ctx, s.db, channelID, name)
}

func (s *Store) queryNets(ctx context.Context, what, query string, args ...any) ([]Net, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list nets %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	nets := []Net{}
	for rows.Next() {
		n, err := scanNet(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list nets %s: %w", what, err)
		}
		nets = append(nets, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list nets %s: %w", what, err)
	}
	return nets, nil
}

// ListNets returns channelID's live nets ordered by id. It returns an empty,
// non-nil slice when there are none.
func (s *Store) ListNets(ctx context.Context, channelID int64) ([]Net, error) {
	return s.queryNets(ctx, fmt.Sprintf("of channel %d", channelID),
		`SELECT id, channel_id, name, created_by, created_at, archived_at FROM nets
		 WHERE channel_id = ? AND archived_at IS NULL ORDER BY id ASC`, channelID)
}

// ListNetsForPrincipal returns the live nets in channelID that principalID is
// on, as a member or a monitor, ordered by id. It returns an empty, non-nil
// slice when there are none.
func (s *Store) ListNetsForPrincipal(ctx context.Context, channelID, principalID int64) ([]Net, error) {
	return s.queryNets(ctx, fmt.Sprintf("of channel %d for principal %d", channelID, principalID),
		`SELECT n.id, n.channel_id, n.name, n.created_by, n.created_at, n.archived_at FROM nets n
		 JOIN net_members m ON m.net_id = n.id
		 WHERE n.channel_id = ? AND n.archived_at IS NULL AND m.principal_id = ?
		 ORDER BY n.id ASC`, channelID, principalID)
}

// ListNetMembers returns netID's roster ordered by principal id. It returns an
// empty, non-nil slice when nobody is on the net.
func (s *Store) ListNetMembers(ctx context.Context, netID int64) ([]NetMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT principal_id, role, added_by, created_at FROM net_members
		 WHERE net_id = ? ORDER BY principal_id ASC`, netID)
	if err != nil {
		return nil, fmt.Errorf("store: list members of net %d: %w", netID, err)
	}
	defer func() { _ = rows.Close() }()
	members := []NetMember{}
	for rows.Next() {
		var m NetMember
		var addedBy sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&m.PrincipalID, &m.Role, &addedBy, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list members of net %d: %w", netID, err)
		}
		m.AddedBy = addedBy.Int64
		m.CreatedAt = time.UnixMilli(createdAt)
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list members of net %d: %w", netID, err)
	}
	return members, nil
}

// IsNetMember reports whether principalID is on netID with the member role,
// that is, may transmit on it. A monitor is not a member in this sense. It does
// not look at whether the net is archived; callers resolve the net with
// NetByName first.
func (s *Store) IsNetMember(ctx context.Context, netID, principalID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM net_members WHERE net_id = ? AND principal_id = ? AND role = ?",
		netID, principalID, string(schema.NetRoleMember)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check member %d of net %d: %w", principalID, netID, err)
	}
	return true, nil
}

// PutNetMember puts principalID on the live net named netName in channelID with
// the given role, or changes their role. addedBy is the acting principal, or 0
// when unknown. It is idempotent: the same role again returns NetMemberUnchanged
// and writes nothing, including no audit event. It returns ErrNotFound for an
// unknown channel or net, an error for a role outside the vocabulary, and
// ErrNotChannelMember when the principal does not exist or is not a member of
// the channel.
func (s *Store) PutNetMember(ctx context.Context, actor string, channelID int64, netName string, principalID int64, role schema.NetRole, addedBy int64) (NetMemberChange, error) {
	if !role.Valid() {
		return NetMemberUnchanged, fmt.Errorf("store: invalid net role %q", role)
	}
	now := time.Now().Truncate(time.Millisecond)
	change := NetMemberUnchanged
	err := s.withImmediateTx(ctx, func(tx execer) error {
		n, err := liveNetTx(ctx, tx, channelID, netName)
		if err != nil {
			return err
		}
		var one int
		switch err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotChannelMember
		case err != nil:
			return fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
		}
		var current schema.NetRole
		switch err := tx.QueryRowContext(ctx,
			"SELECT role FROM net_members WHERE net_id = ? AND principal_id = ?", n.ID, principalID).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO net_members (net_id, principal_id, role, added_by, created_at) VALUES (?, ?, ?, ?, ?)",
				n.ID, principalID, string(role), nullableID(addedBy), now.UnixMilli()); err != nil {
				return fmt.Errorf("store: add member %d to net %d: %w", principalID, n.ID, err)
			}
			change = NetMemberAdded
			return appendAuditEventTx(ctx, tx, actor, AuditNetMemberAdded, netSubject(n.ID),
				netDetail(channelID, fmt.Sprintf("principal=%d role=%s", principalID, role)), now)
		case err != nil:
			return fmt.Errorf("store: read member %d of net %d: %w", principalID, n.ID, err)
		}
		if current == role {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE net_members SET role = ? WHERE net_id = ? AND principal_id = ?", string(role), n.ID, principalID); err != nil {
			return fmt.Errorf("store: change role of member %d on net %d: %w", principalID, n.ID, err)
		}
		change = NetMemberRoleChanged
		return appendAuditEventTx(ctx, tx, actor, AuditNetMemberRoleChanged, netSubject(n.ID),
			netDetail(channelID, fmt.Sprintf("principal=%d role=%s previous_role=%s", principalID, role, current)), now)
	})
	if err != nil {
		return NetMemberUnchanged, err
	}
	return change, nil
}

// RemoveNetMember takes principalID off the live net named netName in
// channelID and writes net_member_removed. It is idempotent: a principal who is
// not on the net returns removed=false and writes nothing. It returns
// ErrNotFound for an unknown channel or net.
func (s *Store) RemoveNetMember(ctx context.Context, actor string, channelID int64, netName string, principalID int64) (removed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		n, err := liveNetTx(ctx, tx, channelID, netName)
		if err != nil {
			return err
		}
		var role schema.NetRole
		switch err := tx.QueryRowContext(ctx,
			"SELECT role FROM net_members WHERE net_id = ? AND principal_id = ?", n.ID, principalID).Scan(&role); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("store: read member %d of net %d: %w", principalID, n.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM net_members WHERE net_id = ? AND principal_id = ?", n.ID, principalID); err != nil {
			return fmt.Errorf("store: remove member %d from net %d: %w", principalID, n.ID, err)
		}
		removed = true
		return appendAuditEventTx(ctx, tx, actor, AuditNetMemberRemoved, netSubject(n.ID),
			netDetail(channelID, fmt.Sprintf("principal=%d role=%s", principalID, role)), now)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// removeNetMembershipsTx takes principalID off every net of channelID, archived
// nets included, auditing each removal. RemoveChannelMember calls it in its own
// transaction so a principal who leaves a channel never keeps a net seat in it.
func removeNetMembershipsTx(ctx context.Context, tx execer, actor string, channelID, principalID int64, at time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT m.net_id, m.role FROM net_members m JOIN nets n ON n.id = m.net_id
		 WHERE n.channel_id = ? AND m.principal_id = ? ORDER BY m.net_id ASC`, channelID, principalID)
	if err != nil {
		return fmt.Errorf("store: list net seats of principal %d in channel %d: %w", principalID, channelID, err)
	}
	type seat struct {
		netID int64
		role  string
	}
	var seats []seat
	for rows.Next() {
		var st seat
		if err := rows.Scan(&st.netID, &st.role); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list net seats of principal %d in channel %d: %w", principalID, channelID, err)
		}
		seats = append(seats, st)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("store: list net seats of principal %d in channel %d: %w", principalID, channelID, err)
	}
	for _, st := range seats {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM net_members WHERE net_id = ? AND principal_id = ?", st.netID, principalID); err != nil {
			return fmt.Errorf("store: remove principal %d from net %d: %w", principalID, st.netID, err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditNetMemberRemoved, netSubject(st.netID),
			netDetail(channelID, fmt.Sprintf("principal=%d role=%s cause=left_channel", principalID, st.role)), at); err != nil {
			return err
		}
	}
	return nil
}
