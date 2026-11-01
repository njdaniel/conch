package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// AuditVoicePermissionChanged is the audit action of a change to a person's
// voice switch (ADR-013 decision 4, issue #984): subject principal:<id>,
// detail voice_allowed=<bool>.
const AuditVoicePermissionChanged = "voice_permission_changed"

// ErrNotAPerson is returned by SetVoiceAllowed for an agent target: an agent
// never holds voice.
var ErrNotAPerson = errors.New("store: not a person")

// SetVoiceAllowed switches a person's voice on or off, the brake under the
// role's use_voice cell (ADR-014 design §5). It audits voice_permission_changed
// in the same transaction and does nothing, and writes nothing, when the value
// already stands. callerID is the writer whose manage_members is rechecked in
// the transaction (ErrPermissionDenied); 0 means no caller (authentication off). A hidden or unknown id is ErrPrincipalNotFound (one answer,
// so the route does not tell them apart), an agent is ErrNotAPerson, and
// forbidOwner makes an owner target ErrOwnerProtected (ADR-010), re-checked
// inside the transaction. It does not touch live media: the caller runs the
// poller's immediate hook after it commits.
func (s *Store) SetVoiceAllowed(ctx context.Context, actor string, callerID, principalID int64, allowed bool, forbidOwner bool) (changed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		// The caller's authority is asked again under the write lock, before
		// the target is touched (admin-roles §7.1): a caller who lost
		// manage_members since the route guard brakes nobody. callerID 0 is
		// authentication off, which has no caller to ask.
		if callerID != 0 {
			held, err := permittedTx(ctx, tx, callerID, schema.PermManageMembers)
			if err != nil {
				return err
			}
			if !held {
				return &PermissionError{Err: ErrPermissionDenied, Permission: schema.PermManageMembers,
					Reason: "switching a person's voice needs manage_members"}
			}
		}
		var (
			kind   string
			cur    bool
			hidden sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx, "SELECT kind, voice_allowed, hidden_at FROM principals WHERE id = ?", principalID).Scan(&kind, &cur, &hidden); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPrincipalNotFound
		case err != nil:
			return fmt.Errorf("store: set voice_allowed: find principal: %w", err)
		}
		if hidden.Valid {
			return ErrPrincipalNotFound
		}
		if err := ownerProtectedTx(ctx, tx, principalID, forbidOwner); err != nil {
			return err
		}
		if PrincipalKind(kind) != PrincipalHuman {
			return ErrNotAPerson
		}
		if cur == allowed {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE principals SET voice_allowed = ? WHERE id = ?", boolInt(allowed), principalID); err != nil {
			return fmt.Errorf("store: set voice_allowed: %w", err)
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditVoicePermissionChanged, principalActor(principalID),
			fmt.Sprintf("voice_allowed=%t", allowed), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
