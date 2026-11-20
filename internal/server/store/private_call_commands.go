package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

type privateCallCommand struct {
	actor                   PrivateCallActor
	actionID, event, target string
	now                     time.Time
}

func (cmd privateCallCommand) receipt(ctx context.Context, tx execer) (PrivateCallResult, bool, error) {
	var event, target, outcome string
	var id sql.NullString
	var glare bool
	err := tx.QueryRowContext(ctx, `SELECT event, target, call_id, outcome, glare FROM private_call_actions
		WHERE principal_id = ? AND action_id = ? AND expires_at > ?`,
		cmd.actor.PrincipalID, cmd.actionID, cmd.now.UnixMilli()).Scan(&event, &target, &id, &outcome, &glare)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateCallResult{}, false, nil
	}
	if err != nil {
		return PrivateCallResult{}, false, err
	}
	if event != cmd.event || target != cmd.target {
		return PrivateCallResult{Code: schema.ErrorCodeCallActionConflict}, true, nil
	}
	result := PrivateCallResult{Code: outcome}
	if id.Valid {
		call, err := privateCallByID(ctx, tx, id.String)
		if err != nil {
			return PrivateCallResult{}, false, err
		}
		if _, err := refreshPrivateCall(ctx, tx, &call, cmd.now); err != nil {
			return PrivateCallResult{}, false, err
		}
		result.Call = call.view(cmd.actor)
		result.Glare = glare && call.State == schema.PrivateCallStateRinging
		// The receipt is per person. A second credential cannot claim
		// the first credential's successful accept as its own.
		if event == schema.PrivateCallEventAccept && call.calleeCredential != 0 &&
			call.calleeCredential != cmd.actor.CredentialID {
			result.Code = schema.ErrorCodeCallStateChanged
		}
	}
	result.Replayed = result.Code == ""
	return result, true, nil
}

func (cmd privateCallCommand) remember(ctx context.Context, tx execer, result PrivateCallResult) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO private_call_actions
		(principal_id, action_id, event, target, call_id, outcome, glare, expires_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)
		ON CONFLICT(principal_id, action_id) DO UPDATE SET event = excluded.event, target = excluded.target,
			call_id = excluded.call_id, outcome = excluded.outcome, glare = excluded.glare, expires_at = excluded.expires_at`,
		cmd.actor.PrincipalID, cmd.actionID, cmd.event, cmd.target, result.Call.ID, result.Code, result.Glare,
		cmd.now.Add(schema.PrivateCallActionWindowSeconds*time.Second).UnixMilli())
	return err
}

// InvitePrivateCall serializes both holder checks with creation of the attempt
// and its retry receipt. The clock is sampled after obtaining the write lock.
func (s *Store) InvitePrivateCall(ctx context.Context, conversationID int64, actor PrivateCallActor, req schema.InvitePrivateCallRequestV1, clock func() time.Time) (PrivateCallResult, error) {
	if err := req.Validate(); err != nil {
		return PrivateCallResult{}, err
	}
	var out PrivateCallResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		cmd := privateCallCommand{actor, req.ActionID, schema.PrivateCallEventInvite, strconv.FormatInt(conversationID, 10), clock().Truncate(time.Millisecond)}
		peer, err := privateCallPeer(ctx, tx, conversationID, actor.PrincipalID)
		if err != nil {
			return err
		}
		if live, err := privateCallRequestLive(ctx, tx, actor, cmd.now); err != nil {
			return err
		} else if !live {
			return ErrPrivateCallPair
		}
		if result, found, err := cmd.receipt(ctx, tx); found || err != nil {
			out = result
			return err
		}
		out, err = cmd.invite(ctx, tx, conversationID, peer)
		if err != nil || out.RetryAfter > 0 {
			return err
		}
		return cmd.remember(ctx, tx, out)
	})
	return out, err
}

// Count prior outcomes for this caller and conversation under the same write
// lock as creation. Retries of a recorded action bypass this check.
func (cmd privateCallCommand) inviteRetryAfter(ctx context.Context, tx execer, conversationID int64) (time.Duration, error) {
	const window = 10 * time.Minute
	var ended int64
	err := tx.QueryRowContext(ctx, `SELECT ended_at FROM private_calls
		WHERE conversation_id = ? AND caller = ? AND ended_at > ?
		AND state IN ('unavailable', 'declined', 'missed')
		ORDER BY ended_at DESC LIMIT 1 OFFSET 2`,
		conversationID, cmd.actor.PrincipalID, cmd.now.Add(-window).UnixMilli()).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return time.UnixMilli(ended).Add(window).Sub(cmd.now), nil
}

func (cmd privateCallCommand) invite(ctx context.Context, tx execer, conversationID, peer int64) (PrivateCallResult, error) {
	current, err := livePrivateCallHeldBy(ctx, tx, cmd.actor.PrincipalID, cmd.now)
	if err != nil {
		return PrivateCallResult{}, err
	}
	if allowed, err := privateCallVoiceAllowed(ctx, tx, cmd.actor.PrincipalID); err != nil {
		return PrivateCallResult{}, err
	} else if !allowed {
		return PrivateCallResult{Code: schema.ErrorCodeVoiceNotAllowed}, nil
	}
	if current.ID != "" {
		if current.State == schema.PrivateCallStateRinging && current.ConversationID == conversationID &&
			current.CallerID == peer && current.CalleeID == cmd.actor.PrincipalID {
			return PrivateCallResult{PrivateCallResponseV1: schema.PrivateCallResponseV1{
				Call: current.view(cmd.actor), Glare: true,
			}}, nil
		}
		return PrivateCallResult{Code: schema.ErrorCodeCallInProgress}, nil
	}
	retry, err := cmd.inviteRetryAfter(ctx, tx, conversationID)
	if err != nil {
		return PrivateCallResult{}, err
	}
	if retry > 0 {
		return PrivateCallResult{Code: schema.ErrorCodeRateLimited, RetryAfter: retry}, nil
	}
	event := schema.PrivateCallEventInviteUnavailable
	available, err := cmd.calleeAvailable(ctx, tx, peer)
	if err != nil {
		return PrivateCallResult{}, err
	}
	if available {
		event = schema.PrivateCallEventInvite
	}
	c, err := createPrivateCall(ctx, tx, conversationID, cmd.actor, peer, event, cmd.now)
	return PrivateCallResult{Created: true, PrivateCallResponseV1: schema.PrivateCallResponseV1{Call: c.view(cmd.actor)}}, err
}

func (cmd privateCallCommand) calleeAvailable(ctx context.Context, tx execer, peer int64) (bool, error) {
	if allowed, err := privateCallContactAllowed(ctx, tx, cmd.actor.PrincipalID, peer); !allowed || err != nil {
		return false, err
	}
	if allowed, err := privateCallVoiceAllowed(ctx, tx, peer); !allowed || err != nil {
		return false, err
	}
	if live, err := privateCallCredentialLive(ctx, tx, PrivateCallActor{PrincipalID: peer}, cmd.now); !live || err != nil {
		return false, err
	}
	current, err := livePrivateCallHeldBy(ctx, tx, peer, cmd.now)
	return current.ID == "", err
}

// ActOnPrivateCall applies one caller or callee transition. Policy and timer
// transitions commit even when they make the requested action stale.
func (s *Store) ActOnPrivateCall(ctx context.Context, id, event string, actor PrivateCallActor, req schema.PrivateCallActionRequestV1, clock func() time.Time) (PrivateCallResult, error) {
	if err := req.Validate(); err != nil {
		return PrivateCallResult{}, err
	}
	var out PrivateCallResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		cmd := privateCallCommand{actor, req.ActionID, event, id, clock().Truncate(time.Millisecond)}
		c, err := privateCallByID(ctx, tx, id)
		if err != nil {
			return err
		}
		role := c.role(actor.PrincipalID)
		if live, err := privateCallRequestLive(ctx, tx, actor, cmd.now); err != nil {
			return err
		} else if role == "" || !live {
			return ErrPrivateCallNotFound
		}
		if !privateCallActionActor(event, role) {
			return ErrPrivateCallActor
		}
		if result, found, err := cmd.receipt(ctx, tx); found || err != nil {
			out = result
			return err
		}
		out, err = cmd.act(ctx, tx, &c, req.Revision)
		if err != nil {
			return err
		}
		return cmd.remember(ctx, tx, out)
	})
	return out, err
}

func privateCallActionActor(event string, role schema.PrivateCallRole) bool {
	for _, row := range schema.PrivateCallTransitions {
		if row.Event == event && row.From != schema.PrivateCallFromNone {
			return row.Actor == string(role) || row.Actor == schema.PrivateCallActorEither
		}
	}
	return false
}

func (cmd privateCallCommand) act(ctx context.Context, tx execer, c *privateCall, revision int64) (PrivateCallResult, error) {
	var out PrivateCallResult
	if _, err := refreshPrivateCall(ctx, tx, c, cmd.now); err != nil {
		return out, err
	}
	row, allowed := schema.NextPrivateCall(string(c.State), cmd.event)
	switch {
	case c.lastEvent == cmd.event && (cmd.event != schema.PrivateCallEventAccept || c.calleeCredential == cmd.actor.CredentialID):
		out.Replayed = true
	case !allowed || c.Revision != revision:
		out.Code = schema.ErrorCodeCallStateChanged
	default:
		if err := transitionPrivateCall(ctx, tx, c, row, cmd.actor.CredentialID, cmd.now); err != nil {
			return out, err
		}
	}
	out.Call = c.view(cmd.actor)
	return out, nil
}
