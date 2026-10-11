package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// ErrHookNotFound is returned when a hook id does not exist.
var ErrHookNotFound = errors.New("store: hook not found")

// Hook is a webhook hook: an ingest token (stored only as its SHA-256) bound
// to a channel and an attributed principal. It deliberately has no field for
// the token, so no caller can accidentally read or log one back.
type Hook struct {
	ID          int64
	Label       string
	ChannelID   int64
	PrincipalID int64
	CreatedAt   time.Time
}

// hashHookToken returns the lowercase hex SHA-256 of token, the only form in
// which a hook token is stored.
func hashHookToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashPlaintextHooks is migration 10's Go step: it copies every row of the
// pre-hashing hooks table into the new one with its token replaced by the
// token's hash, then drops the old table. Ids are assigned in creation order.
func hashPlaintextHooks(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT token, channel_id, principal_id, created_at FROM hooks_plaintext ORDER BY created_at, rowid")
	if err != nil {
		return fmt.Errorf("read existing hooks: %w", err)
	}
	type old struct {
		hash                 string
		channelID, principal int64
		createdAt            int64
	}
	var found []old
	for rows.Next() {
		var (
			token string
			o     old
		)
		if err := rows.Scan(&token, &o.channelID, &o.principal, &o.createdAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan existing hook: %w", err)
		}
		o.hash = hashHookToken(token)
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read existing hooks: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read existing hooks: %w", err)
	}
	for _, o := range found {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO hooks (token_hash, channel_id, principal_id, created_at) VALUES (?, ?, ?, ?)",
			o.hash, o.channelID, o.principal, o.createdAt); err != nil {
			return fmt.Errorf("copy existing hook: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE hooks_plaintext"); err != nil {
		return fmt.Errorf("drop plaintext hooks: %w", err)
	}
	return nil
}

// hookDetail is the audit detail of a hook event: the hook's id, its channel
// and its label. The ids are decimal and the label is last and quoted the way
// strconv.Quote does it, so the detail is one line whatever the label holds
// and a reader recovers the label with strconv.Unquote. It never contains the
// token or its hash.
func hookDetail(id, channelID int64, label string) string {
	return fmt.Sprintf("hook=%d channel=%d label=%q", id, channelID, label)
}

// CreateHook provisions an unlabeled ingest token for an existing channel and
// principal. Only the token's hash is stored. See CreateHookWithLabel for the
// audit event it writes.
func (s *Store) CreateHook(ctx context.Context, actor, token string, channelID, principalID int64) (Hook, error) {
	return s.CreateHookWithLabel(ctx, actor, token, "", channelID, principalID)
}

// CreateHookWithLabel is CreateHook with an operator-chosen label. A hook is
// a posting credential for its principal, so a hook_created audit event is
// appended in the same transaction as the insert, as credential_created is
// for a bearer credential: actor is who created it, the subject is the hook's
// principal, and the detail names the hook id, channel and label, never the
// token or its hash.
//
// It returns ErrDuplicate for a token that is already stored, and an error
// for an unknown channel or principal; a failed create writes neither a hook
// nor an event.
func (s *Store) CreateHookWithLabel(ctx context.Context, actor, token, label string, channelID, principalID int64) (Hook, error) {
	now := time.Now().Truncate(time.Millisecond)
	var id int64
	err := s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO hooks (token_hash, label, channel_id, principal_id, created_at) VALUES (?, ?, ?, ?, ?)",
			hashHookToken(token), label, channelID, principalID, now.UnixMilli())
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: create hook: %w", ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: create hook: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: create hook: %w", err)
		}
		return appendAuditEventTx(ctx, tx, actor, "hook_created", principalActor(principalID),
			hookDetail(id, channelID, label), now)
	})
	if err != nil {
		return Hook{}, err
	}
	return Hook{ID: id, Label: label, ChannelID: channelID, PrincipalID: principalID, CreatedAt: now}, nil
}

// HookByToken resolves an ingest token. It returns ErrNotFound for a token
// that is unknown or whose hook has been revoked; the two are deliberately
// indistinguishable to callers, as is a malformed token (it is simply hashed
// and not found).
func (s *Store) HookByToken(ctx context.Context, token string) (Hook, error) {
	want := hashHookToken(token)
	var (
		hook       Hook
		storedHash string
		createdAt  int64
		revokedAt  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT id, token_hash, label, channel_id, principal_id, created_at, revoked_at FROM hooks WHERE token_hash = ?", want,
	).Scan(&hook.ID, &storedHash, &hook.Label, &hook.ChannelID, &hook.PrincipalID, &createdAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Hook{}, fmt.Errorf("store: find hook: %w", ErrNotFound)
	}
	if err != nil {
		return Hook{}, fmt.Errorf("store: find hook: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(want)) != 1 || revokedAt.Valid {
		return Hook{}, fmt.Errorf("store: find hook: %w", ErrNotFound)
	}
	hook.CreatedAt = time.UnixMilli(createdAt)
	return hook, nil
}

// ListHooks returns every hook, revoked ones included, newest first (ties by
// id descending). The result is never nil and never carries a token or hash.
func (s *Store) ListHooks(ctx context.Context) ([]schema.HookV1, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT h.id, c.name, h.principal_id, h.label, h.created_at, h.revoked_at
		 FROM hooks h JOIN channels c ON c.id = h.channel_id
		 ORDER BY h.created_at DESC, h.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list hooks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []schema.HookV1{}
	for rows.Next() {
		var (
			h         schema.HookV1
			createdAt int64
			revokedAt sql.NullInt64
		)
		if err := rows.Scan(&h.ID, &h.Channel, &h.Principal, &h.Label, &createdAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("store: list hooks: scan: %w", err)
		}
		h.CreatedAt = schema.NewTimestamp(time.UnixMilli(createdAt))
		h.RevokedAt = msToTimestampPtr(revokedAt)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list hooks: %w", err)
	}
	return out, nil
}

// RevokeHook sets revoked_at on a hook, keeping the row. Revoking an
// already-revoked hook succeeds and changes nothing (the original revoked_at
// stands and no second audit event is written). A hook_revoked audit event is
// appended in the same transaction as the first revocation; its detail names
// the hook id, channel and label, never the token or hash. It returns
// ErrHookNotFound for an unknown id.
func (s *Store) RevokeHook(ctx context.Context, actor string, hookID int64) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		var (
			principalID, channelID int64
			label                  string
			revokedAt              sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id, channel_id, label, revoked_at FROM hooks WHERE id = ?", hookID,
		).Scan(&principalID, &channelID, &label, &revokedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrHookNotFound
		case err != nil:
			return fmt.Errorf("store: revoke hook: read: %w", err)
		}
		if revokedAt.Valid {
			return nil
		}
		now := credentialNow().Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx,
			"UPDATE hooks SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), hookID)
		if err != nil {
			return fmt.Errorf("store: revoke hook: %w", err)
		}
		// No audit event unless this call is the one that revoked the hook.
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: revoke hook: %w", err)
		} else if n != 1 {
			return nil
		}
		return appendAuditEventTx(ctx, tx, actor, "hook_revoked", principalActor(principalID),
			hookDetail(hookID, channelID, label), now)
	})
}
