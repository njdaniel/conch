package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLastOperator is returned when disabling the only enabled operator.
var ErrLastOperator = errors.New("store: cannot disable the last enabled operator")

// revokePrincipalCredentialsTx revokes every live (unrevoked, unexpired)
// credential of principalID, writing one credential_revoked event each with
// the given reason, and returns how many it revoked. The caller must hold the
// write lock (withImmediateTx) so the set it reads is the set it revokes.
func revokePrincipalCredentialsTx(ctx context.Context, tx execer, actor string, principalID int64, reason string, now time.Time) (int, error) {
	type live struct {
		id    int64
		label string
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, label FROM credentials
		 WHERE principal_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`,
		principalID, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: list live credentials: %w", err)
	}
	var found []live
	for rows.Next() {
		var c live
		if err := rows.Scan(&c.id, &c.label); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: scan credential: %w", err)
		}
		found = append(found, c)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: list live credentials: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: list live credentials: %w", err)
	}
	for _, c := range found {
		res, err := tx.ExecContext(ctx,
			"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), c.id)
		if err != nil {
			return 0, fmt.Errorf("store: revoke credential %d: %w", c.id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("store: revoke credential %d: %w", c.id, err)
		} else if n != 1 {
			return 0, fmt.Errorf("store: revoke credential %d: affected %d rows", c.id, n)
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credential_revoked", principalActor(principalID),
			credentialDetail(c.id, c.label)+" reason="+reason, now); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// principalState reads role and disabled_at of principalID inside tx.
func principalState(ctx context.Context, tx execer, principalID int64) (role Role, disabled bool, err error) {
	var (
		r          string
		disabledAt sql.NullInt64
	)
	switch err := tx.QueryRowContext(ctx, "SELECT role, disabled_at FROM principals WHERE id = ?", principalID).Scan(&r, &disabledAt); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, ErrPrincipalNotFound
	case err != nil:
		return "", false, fmt.Errorf("store: find principal: %w", err)
	}
	return Role(r), disabledAt.Valid, nil
}

// DisablePrincipal switches principalID off in one transaction: it sets
// disabled_at, revokes every live credential, and appends principal_disabled
// plus one credential_revoked per credential (all attributed to actor, none
// containing token material). A disabled principal cannot authenticate and
// cannot be issued credentials.
//
// It is idempotent: disabling an already-disabled principal changes nothing,
// writes no audit event, and reports changed == false. It returns
// ErrPrincipalNotFound for an unknown id and ErrLastOperator, writing
// nothing, if the target is the only enabled operator. The guard runs inside
// the same BEGIN IMMEDIATE transaction as the update, so two concurrent
// disables of the last two operators serialize and exactly one succeeds.
func (s *Store) DisablePrincipal(ctx context.Context, actor string, principalID int64) (changed bool, err error) {
	err = s.withImmediateTx(ctx, func(tx execer) error {
		role, disabled, err := principalState(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if disabled {
			return nil
		}
		if role == RoleOperator {
			var others int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM principals WHERE role = 'operator' AND disabled_at IS NULL AND id <> ?", principalID,
			).Scan(&others); err != nil {
				return fmt.Errorf("store: disable principal: count operators: %w", err)
			}
			if others == 0 {
				return ErrLastOperator
			}
		}
		now := credentialNow().Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx,
			"UPDATE principals SET disabled_at = ? WHERE id = ? AND disabled_at IS NULL", now.UnixMilli(), principalID)
		if err != nil {
			return fmt.Errorf("store: disable principal: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: disable principal: %w", err)
		} else if n != 1 {
			return nil
		}
		if err := appendAuditEventTx(ctx, tx, actor, "principal_disabled", principalActor(principalID), "", now); err != nil {
			return err
		}
		if _, err := revokePrincipalCredentialsTx(ctx, tx, actor, principalID, "principal disabled", now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// EnablePrincipal clears disabled_at and appends principal_enabled. It revives
// no credential: the ones revoked by DisablePrincipal stay revoked and new
// ones must be issued. Idempotent: enabling an enabled principal writes
// nothing and reports changed == false. It returns ErrPrincipalNotFound for an
// unknown id.
func (s *Store) EnablePrincipal(ctx context.Context, actor string, principalID int64) (changed bool, err error) {
	err = s.withImmediateTx(ctx, func(tx execer) error {
		_, disabled, err := principalState(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !disabled {
			return nil
		}
		now := credentialNow().Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx,
			"UPDATE principals SET disabled_at = NULL WHERE id = ? AND disabled_at IS NOT NULL", principalID)
		if err != nil {
			return fmt.Errorf("store: enable principal: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: enable principal: %w", err)
		} else if n != 1 {
			return nil
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, "principal_enabled", principalActor(principalID), "", now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RevokeAllCredentials revokes every live credential of principalID in one
// transaction, writing one credential_revoked event each (reason=revoke-all),
// and returns how many it revoked. The principal stays enabled. Revoking when
// none are live returns 0 and writes nothing. It returns ErrPrincipalNotFound
// for an unknown id.
func (s *Store) RevokeAllCredentials(ctx context.Context, actor string, principalID int64) (int, error) {
	var n int
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, _, err := principalState(ctx, tx, principalID); err != nil {
			return err
		}
		var err error
		n, err = revokePrincipalCredentialsTx(ctx, tx, actor, principalID, "revoke-all", credentialNow().Truncate(time.Millisecond))
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
