package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	return privateCallContactAllowed(ctx, tx, c.CallerID, c.CalleeID)
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

func eachLivePrivateCall(ctx context.Context, tx execer, fn func(*privateCall) error) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+privateCallColumns+` FROM private_calls c
		WHERE c.id IN (SELECT call_id FROM private_call_holders)`)
	if err != nil {
		return err
	}
	var calls []privateCall
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c, err := scanPrivateCall(rows)
		if err != nil {
			return err
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range calls {
		if err := fn(&calls[i]); err != nil {
			return err
		}
	}
	return nil
}

// SweepPrivateCalls enforces persisted deadlines and current entitlements
// even while clients make no requests. It also expires old retry receipts.
func (s *Store) SweepPrivateCalls(ctx context.Context, clock func() time.Time) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		now := clock().Truncate(time.Millisecond)
		if err := eachLivePrivateCall(ctx, tx, func(c *privateCall) error {
			_, err := refreshPrivateCall(ctx, tx, c, now)
			return err
		}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM private_call_actions WHERE expires_at <= ?`, now.UnixMilli())
		return err
	})
}

// RestartPrivateCalls reconciles all live attempts before a server serves.
func (s *Store) RestartPrivateCalls(ctx context.Context, now time.Time) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		return restartPrivateCallsTx(ctx, tx, now)
	})
}

func restartPrivateCallsTx(ctx context.Context, tx execer, now time.Time) error {
	return eachLivePrivateCall(ctx, tx, func(c *privateCall) error {
		return applyPrivateCallServerEvent(ctx, tx, c, schema.PrivateCallEventRestart, now)
	})
}

func restorePrivateCallsTx(ctx context.Context, tx execer, now time.Time) error {
	return eachLivePrivateCall(ctx, tx, func(c *privateCall) error {
		row, ok := schema.NextPrivateCall(string(c.State), schema.PrivateCallEventRestart)
		if !ok {
			return fmt.Errorf("store: invalid restore in private call state %q", c.State)
		}
		row.Reason = privateCallReasonRestore
		return transitionPrivateCall(ctx, tx, c, row, 0, now)
	})
}

// ObservePrivateCallEndpoints is the #2032 media poller seam. Submit
// observations in poll order for the attempt revision observed. Each
// credential is the selected credential seen in the room, or zero when
// absent. A late observation cannot resurrect an expired attempt.
func (s *Store) ObservePrivateCallEndpoints(ctx context.Context, id string, revision, callerCredential, calleeCredential int64, clock func() time.Time) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
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
