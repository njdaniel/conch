package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func livePrivateCallHeldBy(ctx context.Context, tx execer, principalID int64, now time.Time) (privateCall, error) {
	c, err := privateCallHeldBy(ctx, tx, principalID)
	if errors.Is(err, ErrPrivateCallNotFound) {
		return privateCall{}, nil
	}
	if err != nil {
		return privateCall{}, err
	}
	if _, err := refreshPrivateCall(ctx, tx, &c, now); err != nil || c.State.Terminal() {
		return privateCall{}, err
	}
	return c, nil
}

// refreshPrivateCall checks current access before deadlines so a revoked
// ring never makes a missed entry. Every caller already holds the write lock.
func refreshPrivateCall(ctx context.Context, tx execer, c *privateCall, now time.Time) (bool, error) {
	if c.State.Terminal() {
		return false, nil
	}
	allowed, err := privateCallStillAllowed(ctx, tx, *c, now)
	if err != nil {
		return false, err
	}
	event := privateCallDeadlineEvent(*c, now)
	if !allowed {
		event = schema.PrivateCallEventRevoked
	}
	if event == "" {
		return false, nil
	}
	return true, applyPrivateCallServerEvent(ctx, tx, c, event, now)
}

func privateCallStillAllowed(ctx context.Context, tx execer, c privateCall, now time.Time) (bool, error) {
	for _, endpoint := range []PrivateCallActor{{c.CallerID, c.callerCredential}, {c.CalleeID, c.calleeCredential}} {
		if allowed, err := privateCallVoiceAllowed(ctx, tx, endpoint.PrincipalID); !allowed || err != nil {
			return false, err
		}
		if endpoint.CredentialID != 0 {
			if live, err := privateCallCredentialLive(ctx, tx, endpoint, now); !live || err != nil {
				return false, err
			}
		}
	}
	// Design §4.1's "revoked" row: no block either way, both standing, DMs on
	// for both and for the server. A preference or a visibility change does
	// not end a call already rung; it refuses the next invite (dmContactTx).
	return dmPairReachableTx(ctx, tx, c.CallerID, c.CalleeID)
}

func privateCallDeadlineEvent(c privateCall, now time.Time) string {
	switch c.State {
	case schema.PrivateCallStateRinging:
		if now.UnixMilli() >= c.deadline.Int64 {
			return schema.PrivateCallEventRingExpired
		}
	case schema.PrivateCallStateAccepted:
		if now.UnixMilli() >= c.deadline.Int64 {
			return schema.PrivateCallEventJoinExpired
		}
	case schema.PrivateCallStateActive:
		graceStart := now.Add(-schema.PrivateCallReconnectGraceSeconds * time.Second).UnixMilli()
		if (c.callerAbsent.Valid && c.callerAbsent.Int64 <= graceStart) ||
			(c.calleeAbsent.Valid && c.calleeAbsent.Int64 <= graceStart) {
			return schema.PrivateCallEventGraceExpired
		}
	}
	return ""
}

func applyPrivateCallServerEvent(ctx context.Context, tx execer, c *privateCall, event string, now time.Time) error {
	row, ok := schema.NextPrivateCall(string(c.State), event)
	if !ok || row.Actor != schema.PrivateCallActorServer {
		return fmt.Errorf("store: invalid server call event %q in state %q", event, c.State)
	}
	return transitionPrivateCall(ctx, tx, c, row, 0, now)
}

// revokeHeldPrivateCall applies an authority change to the call it affects.
// A nonzero peer or credential narrows revocation to the pair or selected
// endpoint. The authority write and revocation share a lock.
func revokeHeldPrivateCall(ctx context.Context, tx execer, principalID, peerID, credentialID int64, now time.Time) error {
	c, err := privateCallHeldBy(ctx, tx, principalID)
	if errors.Is(err, ErrPrivateCallNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if (peerID != 0 && c.peer(principalID) != peerID) ||
		(credentialID != 0 && c.selectedCredential(principalID) != credentialID) {
		return nil
	}
	return applyPrivateCallServerEvent(ctx, tx, &c, schema.PrivateCallEventRevoked, now)
}

func reconcileHeldPrivateCall(ctx context.Context, tx execer, principalID int64, now time.Time) error {
	_, err := livePrivateCallHeldBy(ctx, tx, principalID, now)
	return err
}

func heldPrivateCallIDs(ctx context.Context, tx execer) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT call_id FROM private_call_holders`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func sweepPrivateCall(ctx context.Context, tx execer, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT sweep_private_call`); err != nil {
		return err
	}
	c, err := privateCallByID(ctx, tx, id)
	if err == nil {
		_, err = refreshPrivateCall(ctx, tx, &c, now)
	}
	if err != nil {
		slog.ErrorContext(ctx, "store: could not sweep private call", "call_id", id, "error", err)
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO sweep_private_call`); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
	}
	_, err = tx.ExecContext(ctx, `RELEASE sweep_private_call`)
	return err
}

// SweepPrivateCalls enforces persisted deadlines and current entitlements
// even while clients make no requests. It also expires old retry receipts.
func (s *Store) SweepPrivateCalls(ctx context.Context, clock func() time.Time) error {
	return s.withPrivateCallTx(ctx, func(tx execer) error {
		now := clock().Truncate(time.Millisecond)
		if err := reconcilePrivateCalls(ctx, tx, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM private_call_actions WHERE expires_at <= ?`, now.UnixMilli())
		return err
	})
}

func reconcilePrivateCalls(ctx context.Context, tx execer, now time.Time) error {
	ids, err := heldPrivateCallIDs(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := sweepPrivateCall(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// RestartPrivateCalls reconciles all live attempts before a server serves.
func (s *Store) RestartPrivateCalls(ctx context.Context, now time.Time) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		return resetPrivateCallsTx(ctx, tx, now, schema.PrivateCallReasonServerRestart)
	})
}

func restorePrivateCallsTx(ctx context.Context, tx execer, now time.Time) error {
	return resetPrivateCallsTx(ctx, tx, now, privateCallReasonRestore)
}

// Startup and restore fail every live attempt under the write lock, even
// if a stored deadline or other live-state field cannot form a valid document.
func resetPrivateCallsTx(ctx context.Context, tx execer, now time.Time, reason schema.PrivateCallReason) error {
	if _, err := tx.ExecContext(ctx, `UPDATE private_calls SET state = 'failed', reason = ?, last_event = ?,
		revision = revision + 1, deadline = NULL, ended_at = MAX(created_at, ?),
		caller_absent_since = NULL, callee_absent_since = NULL
		WHERE state IN ('ringing', 'accepted', 'active')`, reason, schema.PrivateCallEventRestart, now.UnixMilli()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM private_call_holders`)
	return err
}

// ObservePrivateCallEndpoints is the #2032 media poller seam. Submit
// observations in poll order for the attempt revision observed. Each
// credential is the selected credential seen in the room, or zero when
// absent. A late observation cannot resurrect an expired attempt.
func (s *Store) ObservePrivateCallEndpoints(ctx context.Context, id string, revision, callerCredential, calleeCredential int64, clock func() time.Time) error {
	return s.withPrivateCallTx(ctx, func(tx execer) error {
		c, err := privateCallByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Revision != revision || (c.State != schema.PrivateCallStateAccepted && c.State != schema.PrivateCallStateActive) {
			return nil
		}
		now := clock().Truncate(time.Millisecond)
		if changed, err := refreshPrivateCall(ctx, tx, &c, now); changed || err != nil {
			return err
		}
		callerPresent := callerCredential == c.callerCredential
		calleePresent := calleeCredential == c.calleeCredential
		if c.State == schema.PrivateCallStateAccepted {
			if callerPresent && calleePresent {
				return applyPrivateCallServerEvent(ctx, tx, &c, schema.PrivateCallEventJoined, now)
			}
			return nil
		}
		return recordPrivateCallAbsence(ctx, tx, c, callerPresent, calleePresent, now)
	})
}

func recordPrivateCallAbsence(ctx context.Context, tx execer, c privateCall, callerPresent, calleePresent bool, now time.Time) error {
	caller := privateCallAbsence(c.callerAbsent, callerPresent, now)
	callee := privateCallAbsence(c.calleeAbsent, calleePresent, now)
	if caller == c.callerAbsent && callee == c.calleeAbsent {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE private_calls SET caller_absent_since = ?, callee_absent_since = ?
		WHERE id = ? AND revision = ?`, caller, callee, c.ID, c.Revision)
	return err
}

func privateCallAbsence(since sql.NullInt64, present bool, now time.Time) sql.NullInt64 {
	if present {
		return sql.NullInt64{}
	}
	if since.Valid {
		return since
	}
	return privateCallTime(now)
}
