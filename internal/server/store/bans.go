package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Channel entry bans (issue #851, docs/design/moderation.md §6, on the
// storage of issue #849). A ban is a durable (channel, principal) row: the
// principal may not be added to the channel again, whatever credential it
// signs in with. It is not a credential revocation, not server-wide, and it
// identifies nobody but that principal id: a new account or guest visit is a
// different principal.

// Audit actions written by bans (design §3.3).
const (
	// AuditChannelBanAdded records a new ban row: actor the moderator,
	// subject the channel, detail principal=<target>.
	AuditChannelBanAdded = "channel_ban_added"
	// AuditChannelBanRemoved records a lifted ban, in the same shape.
	AuditChannelBanRemoved = "channel_ban_removed"
)

// liveMembers is channel_members without the rows of banned principals, as a
// table expression: the one definition of "member" for every read (design
// §6.2). A ban is checked here, in the same statement that reads the
// membership, so a membership row that outlived a ban (a restored backup, a
// row written before a ban existed) grants nothing, before or after cleanup.
// Only a write that cleans up a membership, or one that must see the raw row
// in order to remove it, reads channel_members itself.
const liveMembers = `(SELECT * FROM channel_members AS lm WHERE NOT EXISTS (SELECT 1 FROM channel_bans AS lb WHERE lb.channel_id = lm.channel_id AND lb.principal_id = lm.principal_id))`

// notBannedSQL is the same predicate for a statement that reads a net seat:
// the seat holder named by principalExpr is not banned from the channel of the
// net aliased n. The expression is a column of the statement, never text of a
// caller's.
func notBannedSQL(principalExpr string) string {
	return "NOT EXISTS (SELECT 1 FROM channel_bans AS nbb WHERE nbb.channel_id = n.channel_id AND nbb.principal_id = " + principalExpr + ")"
}

// ChannelBan is one row of a channel's ban list. CreatedBy is 0 for a ban
// written with authentication off. Name is the principal's current name,
// read at list time.
type ChannelBan struct {
	PrincipalID int64
	Name        string
	CreatedBy   int64
	CreatedAt   time.Time
}

// BanResult is what a ban or unban did, for the caller's follow-up and its
// response. Cleaned is whether a membership or net seat was removed.
type BanResult struct {
	// Changed is whether a ban row was written (ban) or deleted (unban).
	Changed bool
	// Cleaned is whether a residual or current membership or net seat was
	// removed in the same transaction.
	Cleaned bool
}

// channelBannedTx reports whether principalID is banned from channelID.
func channelBannedTx(ctx context.Context, q execer, channelID, principalID int64) (bool, error) {
	var one int
	switch err := q.QueryRowContext(ctx,
		"SELECT 1 FROM channel_bans WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check ban of %d in channel %d: %w", principalID, channelID, err)
	}
	return true, nil
}

// refuseBannedTx is the ban check of a membership write, inside its
// transaction: a banned principal is ErrNotAddable, the uniform answer of an
// add that cannot be made.
func refuseBannedTx(ctx context.Context, tx execer, channelID, principalID int64) error {
	banned, err := channelBannedTx(ctx, tx, channelID, principalID)
	if err != nil {
		return err
	}
	if banned {
		return ErrNotAddable
	}
	return nil
}

// IsChannelBanned reports whether principalID is banned from channelID.
func (s *Store) IsChannelBanned(ctx context.Context, channelID, principalID int64) (bool, error) {
	return channelBannedTx(ctx, s.db, channelID, principalID)
}

// banCreator reads a ban's creator: found=false when there is no ban.
// creator is 0 for a ban written with authentication off.
func banCreatorTx(ctx context.Context, tx execer, channelID, principalID int64) (creator int64, found bool, err error) {
	var by sql.NullInt64
	switch err := tx.QueryRowContext(ctx,
		"SELECT created_by FROM channel_bans WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&by); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("store: read ban of %d in channel %d: %w", principalID, channelID, err)
	}
	return by.Int64, true, nil
}

// cleanMembershipTx removes principalID's membership and net seats of
// channelID, raw: a residual row of a banned principal is removed too. It
// reports whether anything was removed. Each removal is audited as a removal
// by actor is.
func cleanMembershipTx(ctx context.Context, tx execer, actor string, channelID, principalID int64, now time.Time) (bool, error) {
	removed, err := removeChannelMemberTx(ctx, tx, actor, channelID, principalID, now)
	if err != nil {
		return false, err
	}
	// A net seat without a membership is a row no write should have made;
	// it goes too.
	var seats int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM net_members m JOIN nets n ON n.id = m.net_id
		 WHERE n.channel_id = ? AND m.principal_id = ?`, channelID, principalID).Scan(&seats); err != nil {
		return false, fmt.Errorf("store: count net seats of %d in channel %d: %w", principalID, channelID, err)
	}
	if seats > 0 {
		if err := removeNetMembershipsTx(ctx, tx, actor, channelID, principalID, now); err != nil {
			return false, err
		}
		removed = true
	}
	return removed, nil
}

// managerBanTargetTx is a manager's target rule for a ban that does not yet
// exist (design §6.1): the target must be a current member, and a human who
// is not an operator or owner and does not hold can_decide. A member who is
// not eligible is ErrManagerRefused; anyone who is not a member of the
// channel is ErrPrincipalNotFound, so PUT cannot be used to enumerate ids.
// The row is read raw: a manager bans a member as the channel holds them.
func managerBanTargetTx(ctx context.Context, tx execer, channelID, principalID int64) error {
	var kind, role string
	var owner, canDecide int
	switch err := tx.QueryRowContext(ctx,
		`SELECT p.kind, p.role, p.owner, m.can_decide
		 FROM channel_members m JOIN principals p ON p.id = m.principal_id
		 WHERE m.channel_id = ? AND m.principal_id = ?`, channelID, principalID).Scan(&kind, &role, &owner, &canDecide); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrPrincipalNotFound
	case err != nil:
		return fmt.Errorf("store: check ban target %d of channel %d: %w", principalID, channelID, err)
	}
	if PrincipalKind(kind) != PrincipalHuman || Role(role) == RoleOperator || owner == 1 || canDecide == 1 {
		return ErrManagerRefused
	}
	return nil
}

// BanChannelMember bans principalID from channelID and, in the same
// IMMEDIATE transaction, removes their membership and net seats (design
// §6.2). The actor's standing is re-read under the lock (actorID 0 is
// unconstrained):
//
//   - an operator or owner may ban any principal that exists, a non-member
//     included (a pre-ban); a non-owner operator cannot ban an owner
//     (ErrOwnerProtected); an unknown principal is ErrPrincipalNotFound;
//   - a manager may ban a current member who is a human, not an operator or
//     owner and not holding can_decide (ErrManagerRefused otherwise); a
//     principal who is not a member is ErrPrincipalNotFound; and it may repeat
//     only a ban it created (ErrManagerRefused for another's).
//
// A repeat writes no ban row but still removes any residual membership. The
// ban and its cleanup are allowed in an archived channel. The caller closes
// the principal's live subscriptions after this returns, whatever the result.
func (s *Store) BanChannelMember(ctx context.Context, actor string, channelID, principalID, actorID int64) (BanResult, error) {
	now := time.Now().Truncate(time.Millisecond)
	var res BanResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, actorID)
		if err != nil {
			return err
		}
		creator, exists, err := banCreatorTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		if standing == standingManager {
			if exists {
				if creator != actorID {
					return ErrManagerRefused
				}
			} else if err := managerBanTargetTx(ctx, tx, channelID, principalID); err != nil {
				return err
			}
		} else {
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
		}
		if !exists {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (?, ?, ?, ?)",
				channelID, principalID, nullableID(actorID), now.UnixMilli()); err != nil {
				return fmt.Errorf("store: ban %d from channel %d: %w", principalID, channelID, err)
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditChannelBanAdded, channelSubject(channelID), memberDetail(principalID), now); err != nil {
				return err
			}
			res.Changed = true
		}
		res.Cleaned, err = cleanMembershipTx(ctx, tx, actor, channelID, principalID, now)
		return err
	})
	if err != nil {
		return BanResult{}, err
	}
	return res, nil
}

// UnbanChannelMember lifts principalID's ban from channelID (design §6.2).
// Under one IMMEDIATE transaction it deletes any residual membership and net
// seats first, then the ban row, so no committed state has a membership that
// an unban could bring back to life. It restores nothing. An absent ban is a
// no-op that removes no membership and needs no target lookup, whatever the
// actor. A manager may lift only a ban it created (ErrManagerRefused
// otherwise); an operator may lift any but one on an owner it may not touch
// (ErrOwnerProtected); an owner any. The caller closes the principal's live
// subscriptions after this returns.
func (s *Store) UnbanChannelMember(ctx context.Context, actor string, channelID, principalID, actorID int64) (BanResult, error) {
	now := time.Now().Truncate(time.Millisecond)
	var res BanResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := memberActorStandingTx(ctx, tx, channelID, actorID)
		if err != nil {
			return err
		}
		creator, exists, err := banCreatorTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		if !exists {
			if standing != standingManager {
				return checkChannelExistsTx(ctx, tx, channelID)
			}
			return nil
		}
		switch standing {
		case standingManager:
			if creator != actorID {
				return ErrManagerRefused
			}
		case standingOperator:
			target, found, err := queryPrincipal(ctx, tx, principalID)
			if err != nil {
				return err
			}
			if found && target.IsOwner() {
				return ErrOwnerProtected
			}
		}
		res.Cleaned, err = cleanMembershipTx(ctx, tx, actor, channelID, principalID, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM channel_bans WHERE channel_id = ? AND principal_id = ?", channelID, principalID); err != nil {
			return fmt.Errorf("store: lift ban of %d from channel %d: %w", principalID, channelID, err)
		}
		res.Changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditChannelBanRemoved, channelSubject(channelID), memberDetail(principalID), now)
	})
	if err != nil {
		return BanResult{}, err
	}
	return res, nil
}

// ListChannelBans returns channelID's bans ordered by principal id. It
// returns an empty, non-nil slice when there are none. Authorization is the
// caller's. viewerID is who asks (0 for an unconstrained caller). An owner or
// an operator holding manage_channels sees each principal's name; a manager,
// who sees only the principals they may know (see_member_directory, or a
// channel in common), sees "hidden-<id>" for one they may not, so a pre-ban
// of someone outside their view does not tell them the person exists.
func (s *Store) ListChannelBans(ctx context.Context, channelID, viewerID int64) ([]ChannelBan, error) {
	standing, err := memberActorStandingTx(ctx, s.db, channelID, viewerID)
	if err != nil {
		return nil, err
	}
	seeAll := false
	if standing == standingManager {
		if seeAll, err = permittedTx(ctx, s.db, viewerID, schema.PermSeeMemberDirectory); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.principal_id, p.name, b.created_by, b.created_at
		 FROM channel_bans b JOIN principals p ON p.id = b.principal_id
		 WHERE b.channel_id = ? ORDER BY b.principal_id ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: list bans of channel %d: %w", channelID, err)
	}
	var bans []ChannelBan
	for rows.Next() {
		var b ChannelBan
		var by sql.NullInt64
		var at int64
		if err := rows.Scan(&b.PrincipalID, &b.Name, &by, &at); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: list bans of channel %d: %w", channelID, err)
		}
		b.CreatedBy = by.Int64
		b.CreatedAt = time.UnixMilli(at)
		bans = append(bans, b)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("store: list bans of channel %d: %w", channelID, err)
	}
	if bans == nil {
		bans = []ChannelBan{}
	}
	if standing != standingManager {
		return bans, nil
	}
	for i := range bans {
		known, err := mayKnowTx(ctx, s.db, viewerID, bans[i].PrincipalID, seeAll)
		if err != nil {
			return nil, err
		}
		if !known {
			bans[i].Name = fmt.Sprintf("hidden-%d", bans[i].PrincipalID)
		}
	}
	return bans, nil
}
