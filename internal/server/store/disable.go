package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// ErrLastOperator is returned when an action would leave the instance with no
// owner able to sign in: disabling, revoking the credentials of, demoting, or
// clearing the mark of the only enabled owner that can still sign in (issue
// #788, docs/design/delegated-administration.md §5.5). Without this guard the
// instance could only be recovered on the host.
var ErrLastOperator = errors.New("store: this would leave no owner able to sign in")

// liveOperatorCredentialsTx counts the ways an owner could still sign
// in: the live credentials held by enabled owners, leaving out one
// principal's and one credential (pass 0 to leave nothing out), and the
// enabled, unhidden human owners who hold both factors, a password and a
// confirmed TOTP, and so can sign in with them (issue #709), leaving out
// factorsExceptPrincipalID's. Live includes current (currentAuthSQL, issue
// #707): a credential from an old authentication generation signs nobody
// in. It is what the lock-out guards ask: "if this principal, or this
// credential, went away, could an owner still sign in?" A caller that
// ends a principal (a disable or role/mark removal) leaves its factors out;
// one that ends only credentials (a revoke, a sign-out everywhere) keeps
// them, since a password and a code still sign that owner in afterwards.
//
// Where password-only is allowed (the flag StartMode recorded, read in the
// same transaction) a password alone signs an owner in (ADR-011 item 2.1,
// ADR-012 item 3), so an owner with a password counts, with or without a
// confirmed TOTP: the guard counts what the server really allows. Otherwise
// it is as it always was, and either way an owner with no password, holding
// only a setup credential, is not counted.
func liveOperatorCredentialsTx(ctx context.Context, tx execer, exceptPrincipalID, exceptCredentialID, factorsExceptPrincipalID int64, now time.Time) (int, error) {
	passwordOnly, err := storedPasswordOnlyTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM credentials c JOIN principals p ON p.id = c.principal_id
		         WHERE p.role = 'operator' AND p.owner = 1 AND p.disabled_at IS NULL
		           AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > ?)
		           AND `+currentAuthSQL+`
		           AND p.id <> ? AND c.id <> ?)
		      + (SELECT COUNT(*) FROM principals p
		         WHERE p.role = 'operator' AND p.owner = 1 AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL
		           AND p.id <> ?
		           AND EXISTS (SELECT 1 FROM passwords pw WHERE pw.principal_id = p.id)
		           AND (? OR EXISTS (SELECT 1 FROM totp t WHERE t.principal_id = p.id AND t.secret IS NOT NULL AND t.enrolled_at IS NOT NULL)))`,
		now.UnixMilli(), exceptPrincipalID, exceptCredentialID, factorsExceptPrincipalID, passwordOnly,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count live operator credentials: %w", err)
	}
	return n, nil
}

// revokePrincipalCredentialsTx revokes every unrevoked credential of
// principalID, writing one credential_revoked event each with the given
// reason, and returns how many it revoked. Expired credentials are revoked
// too: an expired-but-unrevoked row would resolve again if the clock were set
// back, and "enable revives nothing" must hold regardless. Each credential's
// sessions end with it (issue #265), with the given session cause. The caller
// must hold the write lock (withImmediateTx) so the set it reads is the set it
// revokes.
func revokePrincipalCredentialsTx(ctx context.Context, tx execer, actor string, principalID int64, reason, sessionCause string, now time.Time, ended *[]EndedRun) (int, error) {
	return revokeCredentialsExceptTx(ctx, tx, actor, principalID, 0, reason, sessionCause, now, ended)
}

// revokeCredentialsExceptTx is revokePrincipalCredentialsTx leaving out one
// credential, except (0 leaves out none): a password change or an
// operator's recovery revokes every credential but the one the request came
// with (issue #708).
func revokeCredentialsExceptTx(ctx context.Context, tx execer, actor string, principalID, except int64, reason, sessionCause string, now time.Time, ended *[]EndedRun) (int, error) {
	type live struct {
		id    int64
		label string
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, label FROM credentials
		 WHERE principal_id = ? AND revoked_at IS NULL AND id <> ? ORDER BY id`,
		principalID, except)
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
		if err := endCredentialSessionsTx(ctx, tx, actor, c.id, sessionCause, now, ended); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// principalState reads role, owner and disabled_at of principalID inside tx.
func principalState(ctx context.Context, tx execer, principalID int64) (role Role, owner, disabled bool, err error) {
	var (
		r          string
		disabledAt sql.NullInt64
	)
	switch err := tx.QueryRowContext(ctx, "SELECT role, owner, disabled_at FROM principals WHERE id = ?", principalID).Scan(&r, &owner, &disabledAt); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, false, ErrPrincipalNotFound
	case err != nil:
		return "", false, false, fmt.Errorf("store: find principal: %w", err)
	}
	return Role(r), owner, disabledAt.Valid, nil
}

// DisablePrincipal switches principalID off in one transaction: it sets
// disabled_at, revokes every live credential, closes every open invite the
// principal made and every open sign-in link for or by it, and appends
// principal_disabled plus one credential_revoked per credential, one
// invite_closed (reason=creator_disabled) per invite and one
// sign_in_link_closed per link (all attributed to actor, none containing
// token material). It also moves the authentication generation on
// (bumpAuthGenerationTx, audited auth_generation_bumped), which ends the
// principal's pending logins and recent-password proofs. A disabled
// principal cannot authenticate and cannot be issued credentials.
//
// It is idempotent: disabling an already-disabled principal changes nothing,
// writes no audit event, and reports changed == false. It returns
// ErrPrincipalNotFound for an unknown id and ErrLastOperator, writing
// nothing, if the target is the only enabled operator. The guard runs inside
// the same BEGIN IMMEDIATE transaction as the update, so two concurrent
// disables of the last two operators serialize and exactly one succeeds.
func (s *Store) DisablePrincipal(ctx context.Context, actor string, principalID int64, forbidOwner ...bool) (changed bool, err error) {
	var runs []EndedRun
	var guests []EndedGuest
	err = s.withImmediateTx(ctx, func(tx execer) error {
		runs, guests = nil, nil
		now := credentialNow().Truncate(time.Millisecond)
		if changed, err = disablePrincipalTx(ctx, tx, actor, principalID, now, &runs, len(forbidOwner) > 0 && forbidOwner[0]); err != nil {
			return err
		}
		// Disabling a guest ends its visit for good (issue #705): enabling
		// it later revives nothing.
		return endIfGuestTx(ctx, tx, actor, principalID, schema.GuestEndOperator, now, &runs, &guests)
	})
	if err != nil {
		return false, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(guests)
	return changed, nil
}

// disablePrincipalTx is DisablePrincipal inside the caller's IMMEDIATE
// transaction: hiding a principal (issue #445) disables it this way first, in
// its own transaction. The runs its ended sessions failed are added to runs,
// for the caller to announce after the commit.
func disablePrincipalTx(ctx context.Context, tx execer, actor string, principalID int64, now time.Time, runs *[]EndedRun, forbidOwner bool) (bool, error) {
	role, owner, disabled, err := principalState(ctx, tx, principalID)
	if err != nil {
		return false, err
	}
	if err := ownerProtectedTx(ctx, tx, principalID, forbidOwner); err != nil {
		return false, err
	}
	if disabled {
		return false, nil
	}
	if role == RoleOperator && owner {
		// Another enabled owner is not enough: it must also be able to sign in,
		// or nobody could sign in to undo this.
		others, err := liveOperatorCredentialsTx(ctx, tx, principalID, 0, principalID, now)
		if err != nil {
			return false, err
		}
		if others == 0 {
			return false, ErrLastOperator
		}
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE principals SET disabled_at = ? WHERE id = ? AND disabled_at IS NULL", now.UnixMilli(), principalID)
	if err != nil {
		return false, fmt.Errorf("store: disable principal: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("store: disable principal: %w", err)
	} else if n != 1 {
		return false, nil
	}
	if err := appendAuditEventTx(ctx, tx, actor, "principal_disabled", principalActor(principalID), "", now); err != nil {
		return false, err
	}
	// Every session of the principal ends first, whatever credential it
	// was made with, so each is recorded as ended by the disabling and not
	// by the revocations that follow (issue #265).
	if err := endPrincipalSessionsTx(ctx, tx, actor, principalID, SessionEndedPrincipalDisabled, now, runs); err != nil {
		return false, err
	}
	if _, err := revokePrincipalCredentialsTx(ctx, tx, actor, principalID, "principal disabled", SessionEndedPrincipalDisabled, now, runs); err != nil {
		return false, err
	}
	// Redemption already refuses a disabled maker's invites; closing them
	// makes the invite list say so too (issue #438), and enabling the
	// principal again does not revive them.
	if err := closeCreatorInvitesTx(ctx, tx, actor, principalID, "", InviteCloseCreatorDisabled, now); err != nil {
		return false, err
	}
	// Likewise the sign-in links for them or made by them (issue #493).
	if err := closeSignInLinksTx(ctx, tx, actor, principalID, now); err != nil {
		return false, err
	}
	// And whatever else was issued under their present authentication
	// (issue #708): pending logins and recent-password proofs. Last, so the
	// links above are closed for the disable, not for the bump.
	if _, err := bumpAuthGenerationTx(ctx, tx, actor, principalID, "principal_disabled", now); err != nil {
		return false, err
	}
	return true, nil
}

// EnablePrincipal clears disabled_at and appends principal_enabled. It revives
// no credential: the ones revoked by DisablePrincipal stay revoked and new
// ones must be issued. A hidden principal is unhidden too (issue #445):
// hidden_at is cleared and principal_unhidden appended, but its old name and
// its memberships are not restored. Idempotent: enabling an enabled principal
// writes nothing and reports changed == false. It returns
// ErrPrincipalNotFound for an unknown id.
func (s *Store) EnablePrincipal(ctx context.Context, actor string, principalID int64, forbidOwner ...bool) (changed bool, err error) {
	err = s.withImmediateTx(ctx, func(tx execer) error {
		p, err := principalByIDTx(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if err := ownerProtectedTx(ctx, tx, principalID, len(forbidOwner) > 0 && forbidOwner[0]); err != nil {
			return err
		}
		if p.DisabledAt == nil {
			return nil
		}
		now := credentialNow().Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx,
			"UPDATE principals SET disabled_at = NULL, hidden_at = NULL WHERE id = ? AND disabled_at IS NOT NULL", principalID)
		if err != nil {
			return fmt.Errorf("store: enable principal: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: enable principal: %w", err)
		} else if n != 1 {
			return nil
		}
		changed = true
		if err := appendAuditEventTx(ctx, tx, actor, "principal_enabled", principalActor(principalID), "", now); err != nil {
			return err
		}
		if p.HiddenAt == nil {
			return nil
		}
		return appendAuditEventTx(ctx, tx, actor, AuditPrincipalUnhidden, principalActor(principalID), "", now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RevokeAllCredentials revokes every unrevoked credential of principalID in
// one transaction and returns how many it revoked. The principal stays
// enabled. It writes one credential_revoked event per credential
// (reason=revoke-all) and always one credentials_revoked_all event recording
// the operator's action and the count, even when the count is zero. It moves
// the authentication generation on too (issue #708), closing the
// principal's open links and ending its pending logins and recent-password
// proofs, audited auth_generation_bumped and sign_in_link_closed. It
// returns ErrPrincipalNotFound for an unknown id, and ErrLastOperator, writing
// nothing, when the target is an enabled operator and no other enabled
// operator holds a live credential, and no operator, the target included,
// holds a password and a confirmed TOTP.
func (s *Store) RevokeAllCredentials(ctx context.Context, actor string, principalID int64, forbidOwner ...bool) (int, error) {
	var n int
	var runs []EndedRun
	var guests []EndedGuest
	err := s.withImmediateTx(ctx, func(tx execer) error {
		runs, guests = nil, nil
		role, owner, disabled, err := principalState(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if err := ownerProtectedTx(ctx, tx, principalID, len(forbidOwner) > 0 && forbidOwner[0]); err != nil {
			return err
		}
		now := credentialNow().Truncate(time.Millisecond)
		if role == RoleOperator && owner && !disabled {
			// The owner's own password and code survive a sign-out
			// everywhere, so they count (issue #709).
			others, err := liveOperatorCredentialsTx(ctx, tx, principalID, 0, 0, now)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastOperator
			}
		}
		if n, err = revokePrincipalCredentialsTx(ctx, tx, actor, principalID, "revoke-all", SessionEndedByRevocation, now, &runs); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credentials_revoked_all", principalActor(principalID), fmt.Sprintf("count=%d", n), now); err != nil {
			return err
		}
		// Signing out everywhere ends more than credentials (issue #708):
		// open links, pending logins and recent-password proofs too.
		if _, err := bumpAuthGenerationTx(ctx, tx, actor, principalID, "revoke_all", now); err != nil {
			return err
		}
		// For a guest, signing out everywhere is End visit (design B4); an
		// operator's revoke-all ends it too.
		end := schema.GuestEndOperator
		if actor == principalActor(principalID) {
			end = schema.GuestEndVisit
		}
		return endIfGuestTx(ctx, tx, actor, principalID, end, now, &runs, &guests)
	})
	if err != nil {
		return 0, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(guests)
	return n, nil
}
