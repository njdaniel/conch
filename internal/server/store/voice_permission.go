package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
// already stands. A hidden or unknown id is ErrPrincipalNotFound (one answer,
// so the route does not tell them apart), an agent is ErrNotAPerson, and
// forbidOwner makes an owner target ErrOwnerProtected (ADR-010), re-checked
// inside the transaction. It does not touch live media: the caller runs the
// poller's immediate hook after it commits.
func (s *Store) SetVoiceAllowed(ctx context.Context, actor string, principalID int64, allowed bool, forbidOwner bool) (changed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
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
