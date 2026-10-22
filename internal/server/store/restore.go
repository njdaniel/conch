package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Restore invalidation (issue #716, docs/design/guests-and-accounts.md §4.2,
// §7.3). A backup can predate a logout, a revocation, a link's use or the end
// of a visit, so a restored database is never served as it was: before the
// server accepts traffic, everything that signs someone in is made unusable,
// and every visit that was active in the backup ends. Sweeping what has
// expired would not be enough: what was revoked after the backup was taken
// is not expired in it.
//
// Invites and guest joining are paused as well: an invite closed after the
// backup was taken is open again in it, joining turned off since is on
// again, and only an operator can tell which. Webhook hooks are
// left as they are and counted for review.
//
// What stays: principals, their passwords, their TOTP secrets, roles,
// memberships, messages and their attribution. Members sign in again with
// their passwords; operators sign in with password and code, or recover on
// the host (conchd operator recover).
//
// The mechanism is the authentication generation (issue #707): moving every
// principal's generation on ends every credential, link, pending login,
// recent-auth proof and setup token issued under the old one, whatever the
// backup says about them. Setup tokens are also deleted, proofs cleared, and
// every TOTP step up to the current one and the window after it marked as
// used, because totp.last_step rolled back with the backup and a code
// accepted after the backup was taken could otherwise be accepted again.

// Audit actions of a restore. Neither carries a secret.
const (
	// AuditRestoreInvalidated is a restored database invalidated (conchd
	// restore): detail what it did, as counts and the TOTP floor.
	AuditRestoreInvalidated = "restore_invalidated"
	// AuditRestoreStarted is the first start of a server on a restored
	// database: detail the TOTP floor it set and how many voice rooms it
	// retired, never their names.
	AuditRestoreStarted = "restore_started"
)

// settingRestorePending is the server_settings key InvalidateRestored
// writes and the first StartRestored removes: the database was restored and
// no server has started on it since.
const settingRestorePending = "restore_pending_start"

// RestoreResult is what InvalidateRestored did.
type RestoreResult struct {
	// Principals are the principals whose authentication generation moved
	// on: every one there is.
	Principals []int64
	// GuestsEnded are the visits that were active and ended with reason
	// restore.
	GuestsEnded []EndedGuest
	// SetupTokens is how many setup tokens were deleted.
	SetupTokens int64
	// TOTPFloor is the highest time step now refused as already used.
	TOTPFloor int64
	// InvitesPaused is true when the restore paused invites; false when they
	// were paused in the backup already. They are paused afterwards either
	// way.
	InvitesPaused bool
	// JoiningPaused is the same for guest joining (joining_paused): no new
	// visit starts from the bare address or an invite until an operator
	// resumes it. guests_enabled is left as the operator set it.
	JoiningPaused bool
	// Hooks is how many webhook hooks are live: their tokens are not
	// sign-ins and a restore leaves them, so they need a review.
	Hooks int
}

// totpFloor is the highest time step a code accepted up to now could be of:
// the current step and the window after it (schema.TOTPWindowSteps). A
// code is accepted only for a step after last_step, so raising last_step
// to this refuses every code that may already have been used, and the first
// code accepted afterwards is one no earlier sign-in could have seen.
func totpFloor(now time.Time) int64 {
	return now.Unix()/schema.TOTPPeriodSeconds + schema.TOTPWindowSteps
}

// raiseTOTPFloorTx marks every TOTP step up to floor as used, for every
// operator, in the caller's transaction.
func raiseTOTPFloorTx(ctx context.Context, tx execer, floor int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE totp SET last_step = ? WHERE last_step < ?", floor, floor); err != nil {
		return fmt.Errorf("store: raise the TOTP floor: %w", err)
	}
	return nil
}

// OnAuthChanged sets the function the store calls, after the commit and
// never inside a transaction, with the principals whose authentication
// generation a restore moved on (InvalidateRestored), so that a server
// holding the store closes what they have open. The other calls that move a
// generation on are followed by their routes, which drop what is open
// themselves. As with OnGuestsEnded there is one such function; set it
// before the store is shared.
func (s *Store) OnAuthChanged(fn func([]int64)) { s.authChanged = fn }

// InvalidateRestored makes a restored database safe to serve, in one
// IMMEDIATE transaction, with actor "system": every active guest visit ends
// with reason restore (endGuestVisitTx: its memberships go and its
// credential is revoked); every principal's authentication generation moves
// on (bumpAuthGenerationTx: its pending logins are spent, its open links
// closed and its proofs cleared, each audited); every setup token is
// deleted; every recent-auth proof is cleared; every TOTP step up to now and
// the window after it is marked as used; invites are paused (audited
// invites_paused, as an operator's pause is, when they were not paused
// already) and so is guest joining (audited join_settings_changed
// joining_paused=false->true, as PutJoinSettings audits it, when it was not
// paused already); the database is marked restored for StartRestored; and
// restore_invalidated is audited with the counts.
// Either all of it commits or none of it does.
//
// Running it again does it again: every generation moves on once more, and
// no visit is left to end. That is harmless, so a restore that failed or was
// interrupted is simply run again.
//
// It may be called on a database a server is serving. Through the store's
// hooks, a server in this process closes the guests' and principals' sockets
// and media at once (OnGuestsEnded, OnAuthChanged); a server in another
// process refuses every credential on its next REST request and closes open
// sockets within its re-check interval, as after an operator's recovery.
func (s *Store) InvalidateRestored(ctx context.Context, now time.Time) (RestoreResult, error) {
	now = now.Truncate(time.Millisecond)
	var (
		out  RestoreResult
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = RestoreResult{}, nil
		guests, err := idsTx(ctx, tx, "SELECT principal_id FROM guest_visits WHERE state = 'active' ORDER BY principal_id")
		if err != nil {
			return fmt.Errorf("store: restore: list active guests: %w", err)
		}
		for _, id := range guests {
			ended, err := endGuestVisitTx(ctx, tx, hostActor, id, schema.GuestEndRestore, now, &runs)
			if err != nil {
				return err
			}
			out.GuestsEnded = append(out.GuestsEnded, ended)
		}
		if out.Principals, err = idsTx(ctx, tx, "SELECT id FROM principals ORDER BY id"); err != nil {
			return fmt.Errorf("store: restore: list principals: %w", err)
		}
		for _, id := range out.Principals {
			if _, err := bumpAuthGenerationTx(ctx, tx, hostActor, id, "restore", now); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM setup_tokens")
		if err != nil {
			return fmt.Errorf("store: restore: delete setup tokens: %w", err)
		}
		if out.SetupTokens, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("store: restore: delete setup tokens: %w", err)
		}
		// The bumps cleared every proof of a principal's credentials; this
		// says so for every credential there is, whoever's it is.
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET recent_auth_until = NULL WHERE recent_auth_until IS NOT NULL"); err != nil {
			return fmt.Errorf("store: restore: clear proofs: %w", err)
		}
		out.TOTPFloor = totpFloor(now)
		if err := raiseTOTPFloorTx(ctx, tx, out.TOTPFloor); err != nil {
			return err
		}
		paused, err := invitesPausedTx(ctx, tx)
		if err != nil {
			return err
		}
		if !paused {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				settingInvitesPaused, strconv.FormatBool(true), now.UnixMilli()); err != nil {
				return fmt.Errorf("store: restore: pause invites: %w", err)
			}
			if err := appendAuditEventTx(ctx, tx, hostActor, AuditInvitesPaused, "server", "", now); err != nil {
				return err
			}
			out.InvitesPaused = true
		}
		join, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !join.JoiningPaused {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				settingJoiningPaused, strconv.FormatBool(true), now.UnixMilli()); err != nil {
				return fmt.Errorf("store: restore: pause joining: %w", err)
			}
			if err := appendAuditEventTx(ctx, tx, hostActor, AuditJoinSettingsChanged, "server",
				settingJoiningPaused+"=false->true", now); err != nil {
				return err
			}
			out.JoiningPaused = true
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM hooks WHERE revoked_at IS NULL").Scan(&out.Hooks); err != nil {
			return fmt.Errorf("store: restore: count hooks: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			settingRestorePending, strconv.FormatInt(now.UnixMilli(), 10), now.UnixMilli()); err != nil {
			return fmt.Errorf("store: restore: mark restored: %w", err)
		}
		return appendAuditEventTx(ctx, tx, hostActor, AuditRestoreInvalidated, "",
			fmt.Sprintf("principals=%d guests_ended=%d setup_tokens=%d totp_floor_step=%d",
				len(out.Principals), len(out.GuestsEnded), out.SetupTokens, out.TOTPFloor), now)
	})
	if err != nil {
		return RestoreResult{}, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(out.GuestsEnded)
	if len(out.Principals) > 0 && s.authChanged != nil {
		s.authChanged(out.Principals)
	}
	return out, nil
}

// StartRestored is what a server starting on the database does first, before
// it accepts traffic. On a database InvalidateRestored marked and no server
// has started on since, it marks every TOTP step up to now and the window
// after it as used, again, from this start's clock: a code an operator gave
// the old server between the invalidation and this start is refused too, and
// for the first 2 × 30 seconds after the start every code is (design §7.3).
// It also retires every live voice room and drops its holders (issue #782).
// The backup cannot say who joined a room after it was taken, so a room with
// no holders recorded may still have people in it, and LiveKit keeps
// renewing their tokens; a room whose name has an earlier prefix is not
// found by the start's clear (Server.ClearVoiceRooms) either. Retired, every
// such room is deleted in LiveKit by the voice sweep, whatever its prefix,
// and retried from its row until it is gone. The next session for a channel
// creates its room afresh (ChannelVoiceRoom). Then it removes the mark and
// audits restore_started, with the floor and how many rooms it retired, all
// in one IMMEDIATE transaction, and reports true with the floor. On any
// other database it writes nothing and reports false, so an ordinary restart
// never does this.
func (s *Store) StartRestored(ctx context.Context, now time.Time) (floor int64, restored bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		floor, restored = 0, false
		var v string
		switch err := tx.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", settingRestorePending).Scan(&v); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("store: start restored: %w", err)
		}
		floor, restored = totpFloor(now), true
		if err := raiseTOTPFloorTx(ctx, tx, floor); err != nil {
			return err
		}
		// Holders go first: they are found through rooms that are still live.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM voice_room_holders WHERE room_id IN (SELECT id FROM voice_rooms WHERE retired_at IS NULL)`); err != nil {
			return fmt.Errorf("store: start restored: drop voice holders: %w", err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE voice_rooms SET retired_at = ? WHERE retired_at IS NULL`, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: start restored: retire voice rooms: %w", err)
		}
		retired, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: start restored: retire voice rooms: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM server_settings WHERE key = ?", settingRestorePending); err != nil {
			return fmt.Errorf("store: start restored: %w", err)
		}
		return appendAuditEventTx(ctx, tx, hostActor, AuditRestoreStarted, "",
			fmt.Sprintf("totp_floor_step=%d voice_rooms_retired=%d", floor, retired), now)
	})
	if err != nil {
		return 0, false, err
	}
	return floor, restored, nil
}
