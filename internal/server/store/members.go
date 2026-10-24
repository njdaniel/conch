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
	// AuditMemberManageGranted and AuditMemberManageRevoked record a change
	// to a member's right to manage the channel (issue #789, ADR-010 §3.2).
	AuditMemberManageGranted = "member_manage_granted"
	AuditMemberManageRevoked = "member_manage_revoked"
	// AuditMemberAddRefused records a refused membership add by name or by a
	// channel manager (issue #790, ADR-010 §3.3): actor the caller, subject
	// the channel, detail empty so no tried name or id is stored.
	AuditMemberAddRefused = "member_add_refused"
)

// ErrNotMember is returned by SetChannelMemberCanDecide and
// SetChannelMemberManages when the principal is not a member of the channel.
// Nothing is written.
var ErrNotMember = errors.New("store: not a member of the channel")

// ErrNotAddable is returned when a membership add names a target that cannot
// be added by that path or actor (issue #789, ADR-010 §3.2, §3.3): for a
// manager, any principal that is unknown, hidden, disabled, an agent, a guest,
// an operator or owner, or outside the manager's channels; for
// AddChannelMemberByName, any name that is not an enabled, non-hidden,
// non-guest human (and within the manager's bound when the actor is a
// manager). Every case returns this one error so a caller cannot tell them
// apart; it matches ErrPrincipalNotFound so handlers map it to 404
// principal_not_found.
var ErrNotAddable error = notAddableError{}

type notAddableError struct{}

func (notAddableError) Error() string        { return "store: principal cannot be added" }
func (notAddableError) Is(target error) bool { return target == ErrPrincipalNotFound }

// ErrManagerRefused is returned when a non-operator actor attempts an
// operator-only membership write (setting can_decide or manages, or acting on
// a channel it does not manage), or when a channel manager attempts to remove
// an operator, owner, agent, or member holding can_decide (issue #789, ADR-010
// §3.2). Mapped to 403 forbidden by the server.
var ErrManagerRefused = errors.New("store: channel manager action not permitted")

// ErrOwnerProtected is returned when a non-owner operator attempts to remove
// an owner from a channel or change an owner's can_decide (issue #789, ADR-010
// §3.1, §4.1 rows 4 and 5). It also matches ErrManagerRefused so a caller
// checking for 403 membership refusals handles both uniformly.
var ErrOwnerProtected error = ownerProtectedError{}

type ownerProtectedError struct{}

func (ownerProtectedError) Error() string {
	return "store: only an owner may remove an owner from a channel or change an owner's approval right"
}

func (ownerProtectedError) Is(target error) bool { return target == ErrManagerRefused }

// ErrManageNotAllowed is returned when manages is set on a principal that
// cannot hold the channel manager grant: an agent ("only a person may manage a
// channel"), or a disabled or hidden principal (issue #789, ADR-010 §3.2). A
// guest target returns ErrGuestNotAllowed (or ErrGuestEnded).
var ErrManageNotAllowed = errors.New("store: principal cannot manage a channel")

// ChannelMember is one row of a channel's member list. AddedBy is 0 when
// unknown (rows backfilled by the upgrade migration, or added with
// authentication off). Name, Kind and Role come from the principal row.
// CanDecide is whether the member may decide the channel's approvals (issue
// #450); Manages is whether the member holds the channel manager grant on this
// channel (issue #789). Both matter for human members only, since agents never
// decide or manage.
type ChannelMember struct {
	PrincipalID int64
	AddedBy     int64
	CreatedAt   time.Time
	Name        string
	Kind        PrincipalKind
	Role        Role
	CanDecide   bool
	Manages     bool
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
// unique by their keys (ErrDuplicate: one that reads the same as another
// channel's is taken, issue #727), and a name with empty keys is
// ErrNameInvalid.
func (s *Store) CreateChannelAs(ctx context.Context, actor, name string, creatorID int64) (Channel, error) {
	// SQLite stores timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a create response exactly
	// describes what a subsequent read returns.
	now := time.Now().Truncate(time.Millisecond)
	var ch Channel
	err := s.withImmediateTx(ctx, func(tx execer) error {
		id, err := insertChannelTx(ctx, tx, name, now)
		if err != nil {
			return err
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

// checkChannelExistsTx reports ErrNotFound when channelID does not exist.
func checkChannelExistsTx(ctx context.Context, tx execer, channelID int64) error {
	var one int
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE id = ?", channelID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
	case err != nil:
		return fmt.Errorf("store: check channel %d: %w", channelID, err)
	}
	return nil
}

type memberActorStanding int

const (
	standingOwner memberActorStanding = iota + 1
	standingOperator
	standingManager
)

// memberActorStandingTx re-reads the acting principal under the write lock
// (issue #789, ADR-010 §5.1). An actorID of 0 means an unconstrained system
// or unauthenticated caller and has owner standing. Otherwise the actor must
// be an enabled, non-hidden, non-guest human who is either an operator (with
// or without the owner mark) or a member of channelID holding manages = 1.
func memberActorStandingTx(ctx context.Context, tx execer, channelID, actorID int64) (memberActorStanding, error) {
	if actorID == 0 {
		return standingOwner, nil
	}
	actorP, found, err := queryPrincipal(ctx, tx, actorID)
	if err != nil {
		return 0, err
	}
	if !found || actorP.DisabledAt != nil || actorP.HiddenAt != nil || actorP.Kind != PrincipalHuman || actorP.guestTarget() {
		return 0, ErrManagerRefused
	}
	switch actorP.Role {
	case RoleOperator:
		if actorP.Owner {
			return standingOwner, nil
		}
		return standingOperator, nil
	case RoleMember:
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return 0, err
		}
		var manages int
		switch err := tx.QueryRowContext(ctx,
			"SELECT manages FROM channel_members WHERE channel_id = ? AND principal_id = ?",
			channelID, actorID).Scan(&manages); {
		case errors.Is(err, sql.ErrNoRows):
			return 0, ErrManagerRefused
		case err != nil:
			return 0, fmt.Errorf("store: check manager standing of %d in channel %d: %w", actorID, channelID, err)
		}
		if manages != 1 {
			return 0, ErrManagerRefused
		}
		return standingManager, nil
	default:
		return 0, ErrManagerRefused
	}
}

// checkManagerAddTargetTx checks whether managerID may add targetID to a
// channel (issue #789, ADR-010 §3.2): targetID must be an enabled, non-hidden,
// non-guest human member (not an operator or owner) who already shares at
// least one channel with managerID. Every refused case returns ErrNotAddable.
func checkManagerAddTargetTx(ctx context.Context, tx execer, managerID, targetID int64) error {
	target, found, err := queryPrincipal(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if !found || target.HiddenAt != nil || target.DisabledAt != nil || target.Kind != PrincipalHuman || target.guestTarget() || target.Role != RoleMember || target.Owner {
		return ErrNotAddable
	}
	var shared bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM channel_members m1
			JOIN channel_members m2 ON m2.channel_id = m1.channel_id
			WHERE m1.principal_id = ? AND m2.principal_id = ?
		)`, managerID, targetID).Scan(&shared); err != nil {
		return fmt.Errorf("store: check shared channel between %d and %d: %w", managerID, targetID, err)
	}
	if !shared {
		return ErrNotAddable
	}
	return nil
}

// checkManageTarget verifies that target may hold the channel manager grant
// (issue #789, ADR-010 §3.2). Guest targets are checked first by guestMemberTx.
func checkManageTarget(target Principal) error {
	if target.Kind != PrincipalHuman {
		return fmt.Errorf("%w: only a person may manage a channel", ErrManageNotAllowed)
	}
	if target.DisabledAt != nil || target.HiddenAt != nil {
		return fmt.Errorf("%w: a disabled or hidden principal cannot manage a channel", ErrManageNotAllowed)
	}
	return nil
}

// AddChannelMember makes principalID a member of channelID, with the right
// to decide the channel's approvals. addedBy is recorded in added_by (0 when
// unknown). It is idempotent: adding an existing member returns added=false
// and writes nothing, including no audit event. It returns ErrNotFound for an
// unknown channel and ErrPrincipalNotFound for an unknown principal.
func (s *Store) AddChannelMember(ctx context.Context, actor string, channelID, principalID, addedBy int64) (added bool, err error) {
	return s.putChannelMember(ctx, actor, 0, channelID, principalID, addedBy, nil, nil)
}

// PutChannelMember adds principalID to channelID or updates its membership
// flags (issues #450, #789). addedBy is the acting principal (0 when
// unconstrained/unauthenticated) and is re-checked under the write lock.
// For an operator actor: canDecide nil means the default for a new member
// (true) and no change for an existing one; manages (optional) nil means the
// default for a new member (false) and no change for an existing one. A
// non-owner operator cannot change an owner's can_decide (ErrOwnerProtected).
// For a channel manager actor: both canDecide and manages must be nil
// (ErrManagerRefused otherwise), the target must satisfy the manager-add bound
// (ErrNotAddable otherwise), and a new member is added with can_decide = 0 and
// manages = 0.
func (s *Store) PutChannelMember(ctx context.Context, actor string, channelID, principalID, addedBy int64, canDecide *bool, manages ...*bool) (added bool, err error) {
	var m *bool
	if len(manages) > 0 {
		m = manages[0]
	}
	return s.putChannelMember(ctx, actor, addedBy, channelID, principalID, addedBy, canDecide, m)
}

func (s *Store) putChannelMember(ctx context.Context, actor string, actorID, channelID, principalID, addedBy int64, canDecide, manages *bool) (added bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, actorID)
		if err != nil {
			return err
		}
		if standing == standingManager {
			if canDecide != nil || manages != nil {
				return ErrManagerRefused
			}
			if err := checkManagerAddTargetTx(ctx, tx, actorID, principalID); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, manages) VALUES (?, ?, ?, ?, 0, 0)
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
			return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), memberDetail(principalID)+" can_decide=0", now)
		}
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return err
		}
		target, found, err := queryPrincipal(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPrincipalNotFound
		}
		if standing == standingOperator && target.IsOwner() && canDecide != nil {
			return ErrOwnerProtected
		}
		// A guest never decides approvals or manages channels, and every
		// membership it gets has a fresh history bound (issue #705, #789).
		guest, err := guestMemberTx(ctx, tx, principalID, canDecide, manages)
		if err != nil {
			return err
		}
		if guest {
			added, err = addGuestMemberTx(ctx, tx, actor, channelID, principalID, addedBy, now)
			return err
		}
		if manages != nil && *manages {
			if err := checkManageTarget(target); err != nil {
				return err
			}
		}
		decide := canDecide == nil || *canDecide
		mgr := manages != nil && *manages
		res, err := tx.ExecContext(ctx,
			`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, manages) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (channel_id, principal_id) DO NOTHING`,
			channelID, principalID, nullableID(addedBy), now.UnixMilli(), boolInt(decide), boolInt(mgr))
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", principalID, channelID, err)
		}
		if n == 0 {
			if canDecide != nil {
				if _, err := setCanDecideTx(ctx, tx, actor, channelID, principalID, *canDecide, now); err != nil {
					return err
				}
			}
			if manages != nil {
				if _, err := setManagesTx(ctx, tx, actor, channelID, target, *manages, now); err != nil {
					return err
				}
			}
			return nil
		}
		added = true
		detail := memberDetail(principalID)
		if !decide {
			// Recorded, so the audit trail says who held the right when.
			detail += " can_decide=0"
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), detail, now); err != nil {
			return err
		}
		if mgr {
			if err := appendAuditEventTx(ctx, tx, actor, AuditMemberManageGranted, channelSubject(channelID), memberDetail(principalID), now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return added, nil
}

// AddChannelMemberByName adds the principal with exact name to channelID with
// can_decide = 0 and manages = 0, whoever calls it (issue #789, ADR-010 §3.3).
// addedBy is the acting principal (0 when unconstrained) and is re-checked
// under the write lock. When the named principal is already a member, it
// returns the existing ChannelMember with added=false and writes nothing,
// preserving any existing can_decide or manages flags. Any target that is
// unknown, hidden, disabled, an agent, a guest, or (for a manager actor) an
// operator/owner or outside the manager's channels returns ErrNotAddable.
func (s *Store) AddChannelMemberByName(ctx context.Context, actor string, channelID, addedBy int64, name string) (ChannelMember, bool, error) {
	now := time.Now().Truncate(time.Millisecond)
	var member ChannelMember
	var added bool
	err := s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, addedBy)
		if err != nil {
			return err
		}
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return err
		}
		var targetID int64
		switch err := tx.QueryRowContext(ctx, "SELECT id FROM principals WHERE name = ?", name).Scan(&targetID); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotAddable
		case err != nil:
			return fmt.Errorf("store: find principal by name: %w", err)
		}
		if standing == standingManager {
			if err := checkManagerAddTargetTx(ctx, tx, addedBy, targetID); err != nil {
				return err
			}
		} else {
			target, found, err := queryPrincipal(ctx, tx, targetID)
			if err != nil {
				return err
			}
			if !found || target.HiddenAt != nil || target.DisabledAt != nil || target.Kind != PrincipalHuman || target.guestTarget() {
				return ErrNotAddable
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, manages) VALUES (?, ?, ?, ?, 0, 0)
			 ON CONFLICT (channel_id, principal_id) DO NOTHING`,
			channelID, targetID, nullableID(addedBy), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", targetID, channelID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: add member %d to channel %d: %w", targetID, channelID, err)
		}
		if n > 0 {
			added = true
			if err := appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID), memberDetail(targetID)+" can_decide=0", now); err != nil {
				return err
			}
		}
		member, err = channelMemberTx(ctx, tx, channelID, targetID)
		return err
	})
	if err != nil {
		return ChannelMember{}, false, err
	}
	return member, added, nil
}

// SetChannelMemberCanDecide grants or revokes principalID's right to decide
// channelID's approvals (issue #450, #789). Only an operator may call it;
// passing a non-zero actorID re-checks the actor's operator standing and
// refuses a non-owner operator acting on an owner (ErrOwnerProtected). It
// returns changed=false and writes nothing when the member already has that
// setting, ErrNotMember when the principal is not a member, and the errors of
// AddChannelMember for an unknown channel or principal. A change is audited.
func (s *Store) SetChannelMemberCanDecide(ctx context.Context, actor string, channelID, principalID int64, canDecide bool, actorID ...int64) (changed bool, err error) {
	var callerID int64
	if len(actorID) > 0 {
		callerID = actorID[0]
	}
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, callerID)
		if err != nil {
			return err
		}
		if standing == standingManager {
			return ErrManagerRefused
		}
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return err
		}
		target, found, err := queryPrincipal(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPrincipalNotFound
		}
		if standing == standingOperator && target.IsOwner() {
			return ErrOwnerProtected
		}
		changed, err = setCanDecideTx(ctx, tx, actor, channelID, principalID, canDecide, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// SetChannelMemberManages grants or revokes principalID's channel manager
// grant on channelID (issue #789, ADR-010 §3.2). Only an operator (owner or
// non-owner) may call it; a manager or member actor returns ErrManagerRefused.
// Setting manages on an agent or a disabled/hidden principal returns
// ErrManageNotAllowed; on a guest returns ErrGuestNotAllowed (or
// ErrGuestEnded). It returns changed=false and writes nothing when the member
// already has that setting, ErrNotMember when the principal is not a member,
// and the errors of AddChannelMember for an unknown channel or principal. A
// change writes AuditMemberManageGranted or AuditMemberManageRevoked.
func (s *Store) SetChannelMemberManages(ctx context.Context, actor string, channelID, principalID int64, manages bool, actorID ...int64) (changed bool, err error) {
	var callerID int64
	if len(actorID) > 0 {
		callerID = actorID[0]
	}
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, callerID)
		if err != nil {
			return err
		}
		if standing == standingManager {
			return ErrManagerRefused
		}
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return err
		}
		target, found, err := queryPrincipal(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPrincipalNotFound
		}
		changed, err = setManagesTx(ctx, tx, actor, channelID, target, manages, now)
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
		if _, err := guestMemberTx(ctx, tx, principalID, &canDecide, nil); err != nil {
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

// setManagesTx sets an existing member's channel manager grant and audits a
// change, inside the caller's transaction. An agent, guest, disabled or hidden
// principal is never given it.
func setManagesTx(ctx context.Context, tx execer, actor string, channelID int64, target Principal, manages bool, now time.Time) (bool, error) {
	if manages {
		if _, err := guestMemberTx(ctx, tx, target.ID, nil, &manages); err != nil {
			return false, err
		}
		if err := checkManageTarget(target); err != nil {
			return false, err
		}
	}
	var current int
	switch err := tx.QueryRowContext(ctx,
		"SELECT manages FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, target.ID).Scan(&current); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotMember
	case err != nil:
		return false, fmt.Errorf("store: read manage grant of %d in channel %d: %w", target.ID, channelID, err)
	}
	if (current == 1) == manages {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE channel_members SET manages = ? WHERE channel_id = ? AND principal_id = ?",
		boolInt(manages), channelID, target.ID); err != nil {
		return false, fmt.Errorf("store: set manage grant of %d in channel %d: %w", target.ID, channelID, err)
	}
	action := AuditMemberManageRevoked
	if manages {
		action = AuditMemberManageGranted
	}
	return true, appendAuditEventTx(ctx, tx, actor, action, channelSubject(channelID), memberDetail(target.ID), now)
}

// ManagesChannel reports whether principalID is an enabled, non-hidden,
// non-guest human member of channelID holding manages = 1 (issue #789,
// ADR-010 §3.2).
func (s *Store) ManagesChannel(ctx context.Context, channelID, principalID int64) (bool, error) {
	var manages int
	err := s.db.QueryRowContext(ctx,
		`SELECT m.manages AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL AND NOT `+activeGuestSQL+`
		 FROM channel_members m
		 JOIN principals p ON p.id = m.principal_id
		 WHERE m.channel_id = ? AND m.principal_id = ?`, channelID, principalID).Scan(&manages)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check manage grant of %d in channel %d: %w", principalID, channelID, err)
	}
	return manages == 1, nil
}

// CanDecideApprovals reports whether principalID is a member of channelID
// holding the right to decide its approvals (issue #450). A non-member
// cannot.
func (s *Store) CanDecideApprovals(ctx context.Context, channelID, principalID int64) (bool, error) {
	var can int
	// A guest never holds the right, whatever its row says (issue #706): the
	// same condition the deciding transaction applies.
	err := s.db.QueryRowContext(ctx,
		`SELECT m.can_decide AND NOT `+activeGuestSQL+` FROM channel_members m
		 JOIN principals p ON p.id = m.principal_id
		 WHERE m.channel_id = ? AND m.principal_id = ?`, channelID, principalID).Scan(&can)
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
// channel, each removal audited. When actorID is provided and non-zero, the
// actor's standing is re-read under the write lock (issue #789, ADR-010 §3.2):
// a non-owner operator cannot remove an owner (ErrOwnerProtected); a channel
// manager checks target membership in channelID FIRST (returning removed=false,
// nil for any non-member regardless of role or whether the principal exists),
// and is refused (ErrManagerRefused) if the current member is an operator, an
// owner, an agent, or holds can_decide = 1. The caller must close the
// principal's live subscriptions after this returns.
func (s *Store) RemoveChannelMember(ctx context.Context, actor string, channelID, principalID int64, actorID ...int64) (removed bool, err error) {
	var callerID int64
	if len(actorID) > 0 {
		callerID = actorID[0]
	}
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, callerID)
		if err != nil {
			return err
		}
		if standing == standingManager {
			var kind, role string
			var owner, canDecide int
			switch err := tx.QueryRowContext(ctx,
				`SELECT p.kind, p.role, p.owner, m.can_decide
				 FROM channel_members m
				 JOIN principals p ON p.id = m.principal_id
				 WHERE m.channel_id = ? AND m.principal_id = ?`,
				channelID, principalID).Scan(&kind, &role, &owner, &canDecide); {
			case errors.Is(err, sql.ErrNoRows):
				removed = false
				return nil
			case err != nil:
				return fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
			}
			if PrincipalKind(kind) != PrincipalHuman || Role(role) == RoleOperator || owner == 1 || canDecide == 1 {
				return ErrManagerRefused
			}
			removed, err = removeChannelMemberTx(ctx, tx, actor, channelID, principalID, now)
			return err
		}
		if err := checkChannelExistsTx(ctx, tx, channelID); err != nil {
			return err
		}
		target, found, err := queryPrincipal(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPrincipalNotFound
		}
		if standing == standingOperator && target.IsOwner() {
			return ErrOwnerProtected
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

// HistoryBounded reports whether principalID reads channelID under a history
// bound (issue #719): it is a guest now, or its membership there carries one
// (history_after, kept through conversion). The bound itself is applied by
// the visibility condition in audience.go; this says only whether a reader
// is held to it, so that a socket can ask that condition about each live
// frame for those readers and spare everyone else the reads. It reads no
// message.
func (s *Store) HistoryBounded(ctx context.Context, channelID, principalID int64) (bool, error) {
	var bounded bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM guest_visits WHERE principal_id = ? AND state = 'active')
		     OR EXISTS (SELECT 1 FROM channel_members
		                WHERE channel_id = ? AND principal_id = ? AND history_after IS NOT NULL)`,
		principalID, channelID, principalID).Scan(&bounded)
	if err != nil {
		return false, fmt.Errorf("store: history bound of %d in channel %d: %w", principalID, channelID, err)
	}
	return bounded, nil
}

// guestMemberTx reports whether principalID is a guest, for a membership
// write asking for canDecide or manages (nil: the default). A guest asked to
// be given either right is ErrGuestNotAllowed, and one whose visit has ended
// is ErrGuestEnded: it gets no membership at all.
func guestMemberTx(ctx context.Context, tx execer, principalID int64, canDecide, manages *bool) (bool, error) {
	var state string
	switch err := tx.QueryRowContext(ctx, "SELECT state FROM guest_visits WHERE principal_id = ?", principalID).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read guest state of %d: %w", principalID, err)
	}
	switch schema.GuestState(state) {
	case schema.GuestStateActive:
		if (canDecide != nil && *canDecide) || (manages != nil && *manages) {
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

func scanChannelMember(scan func(dest ...any) error) (ChannelMember, error) {
	var m ChannelMember
	var addedBy, historyAfter sql.NullInt64
	var handle, label sql.NullString
	var createdAt int64
	var canDecide, manages int
	if err := scan(&m.PrincipalID, &addedBy, &createdAt, &m.Name, &m.Kind, &m.Role, &canDecide, &manages, &historyAfter, &handle, &label); err != nil {
		return ChannelMember{}, err
	}
	m.AddedBy = addedBy.Int64
	m.HistoryAfter = historyAfter.Int64
	if handle.Valid {
		a := guestAttribution(handle.String, label.String)
		m.Guest = &a
	}
	m.CreatedAt = time.UnixMilli(createdAt)
	m.CanDecide = canDecide == 1
	m.Manages = manages == 1
	return m, nil
}

func channelMemberTx(ctx context.Context, tx execer, channelID, principalID int64) (ChannelMember, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT m.principal_id, m.added_by, m.created_at, p.name, p.kind, p.role, m.can_decide, m.manages, m.history_after, v.handle, v.label
		 FROM channel_members m
		 JOIN principals p ON p.id = m.principal_id
		 LEFT JOIN guest_visits v ON v.principal_id = m.principal_id AND v.state = 'active'
		 WHERE m.channel_id = ? AND m.principal_id = ?`, channelID, principalID)
	m, err := scanChannelMember(row.Scan)
	if err != nil {
		return ChannelMember{}, fmt.Errorf("store: read member %d of channel %d: %w", principalID, channelID, err)
	}
	return m, nil
}

// ListChannelMembers returns channelID's members, with each principal's name,
// kind and role, ordered by principal id. It returns an empty, non-nil slice
// when there are none. The join cannot drop a member: principal_id is a
// foreign key, and principals are never deleted.
func (s *Store) ListChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.principal_id, m.added_by, m.created_at, p.name, p.kind, p.role, m.can_decide, m.manages, m.history_after, v.handle, v.label
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
		m, err := scanChannelMember(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list members of channel %d: %w", channelID, err)
	}
	return members, nil
}

// ListChannelsForPrincipal returns the channels principalID is a member of,
// ordered by id ascending, with Manages reporting whether principalID holds
// the channel manager grant on each channel. It returns an empty, non-nil
// slice when there are none.
func (s *Store) ListChannelsForPrincipal(ctx context.Context, principalID int64) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.name, c.created_at, m.manages FROM channels c
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
		var manages int
		if err := rows.Scan(&ch.ID, &ch.Name, &createdAt, &manages); err != nil {
			return nil, fmt.Errorf("store: list channels of principal %d: %w", principalID, err)
		}
		ch.CreatedAt = time.UnixMilli(createdAt)
		ch.Manages = manages == 1
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels of principal %d: %w", principalID, err)
	}
	return channels, nil
}

// CheckMemberAddLimit refuses a membership add when callerID has already had
// limit or more refused member-add attempts in the RenameWindow ending at now
// (issue #790, ADR-010 §3.3). When full, it returns a *RenameLimitedError
// naming when the next attempt fits.
func (s *Store) CheckMemberAddLimit(ctx context.Context, callerID int64, limit int, now time.Time) error {
	if callerID <= 0 || limit <= 0 {
		return nil
	}
	now = now.Truncate(time.Millisecond)
	since := now.Add(-RenameWindow).UnixMilli()
	actor := principalActor(callerID)
	const where = `FROM audit_events WHERE actor = ? AND action = ? AND created_at > ?`
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) `+where, actor, AuditMemberAddRefused, since).Scan(&count); err != nil {
		return fmt.Errorf("store: count refused member adds of principal %d: %w", callerID, err)
	}
	if count < limit {
		return nil
	}
	var freeing int64
	if err := s.db.QueryRowContext(ctx, `SELECT created_at `+where+` ORDER BY created_at, id LIMIT 1 OFFSET ?`,
		actor, AuditMemberAddRefused, since, count-limit).Scan(&freeing); err != nil {
		return fmt.Errorf("store: count refused member adds of principal %d: %w", callerID, err)
	}
	return &RenameLimitedError{Until: time.UnixMilli(freeing).Add(RenameWindow)}
}

// RecordMemberAddRefused appends a member_add_refused audit row for callerID
// on channelID, with an empty detail so no tried name or principal id is
// recorded (issue #790, ADR-010 §3.3).
func (s *Store) RecordMemberAddRefused(ctx context.Context, callerID, channelID int64, now time.Time) error {
	_, err := s.AppendAuditEventAt(ctx, principalActor(callerID), AuditMemberAddRefused, channelSubject(channelID),
		"", now.Truncate(time.Millisecond))
	return err
}
