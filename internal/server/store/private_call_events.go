package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// PrivateCallUpdate identifies a committed revision to be offered to the
// two participants. Delivery must load and authorize the revision again.
type PrivateCallUpdate struct {
	ID                 string
	Revision           int64
	CallerID, CalleeID int64
}

// OnPrivateCallUpdate installs the post-commit callback. Set it before using
// the store concurrently. The callback may run concurrently with later writes.
func (s *Store) OnPrivateCallUpdate(fn func(PrivateCallUpdate)) { s.privateCallUpdate = fn }

type privateCallTx struct {
	execer
	updates []PrivateCallUpdate
}

func (s *Store) withPrivateCallTx(ctx context.Context, fn func(execer) error) error {
	var tx privateCallTx
	if err := s.withImmediateTx(ctx, func(q execer) error {
		tx.execer = q
		return fn(&tx)
	}); err != nil {
		return err
	}
	if s.privateCallUpdate != nil {
		for _, update := range tx.updates {
			s.privateCallUpdate(update)
		}
	}
	return nil
}

func recordPrivateCallUpdate(tx execer, c privateCall) {
	if tx, ok := tx.(*privateCallTx); ok {
		tx.updates = append(tx.updates, PrivateCallUpdate{c.ID, c.Revision, c.CallerID, c.CalleeID})
	}
}

// PrivateCallForDelivery projects the queued revision to one authenticated
// credential. An exact revision check suppresses both late rings and other
// superseded updates, even if callbacks arrive in a different order.
func (s *Store) PrivateCallForDelivery(ctx context.Context, actor PrivateCallActor, id string, revision int64, seen bool, now time.Time) (*schema.PrivateCallV1, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := privateCallByID(ctx, tx, id)
	if errors.Is(err, ErrPrivateCallNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.Revision != revision || c.State == schema.PrivateCallStateUnavailable || c.role(actor.PrincipalID) == "" {
		return nil, nil
	}
	live, err := privateCallRequestLive(ctx, tx, actor, now)
	if !live || err != nil {
		return nil, err
	}
	if c.State.Terminal() {
		// A socket that saw the call or held its selected credential needs
		// the same terminal projection regardless of a later block.
		// Revocations carry the same reasonless outcome for every cause.
		if !seen && c.selectedCredential(actor.PrincipalID) != actor.CredentialID {
			return nil, nil
		}
	} else {
		allowed, err := privateCallStillAllowed(ctx, tx, c, now)
		if !allowed || err != nil || privateCallDeadlineEvent(c, now) != "" {
			return nil, err
		}
		if !seen {
			known, err := dmMayKnowTx(ctx, tx, actor.PrincipalID, c.peer(actor.PrincipalID))
			if !known || err != nil {
				return nil, err
			}
		}
	}
	doc := c.view(actor)
	return &doc, nil
}
