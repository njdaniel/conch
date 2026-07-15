package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Hook maps an opaque ingest token to a channel and attributed principal.
type Hook struct {
	Token       string
	ChannelID   int64
	PrincipalID int64
	CreatedAt   time.Time
}

// CreateHook provisions an ingest token for an existing channel and principal.
func (s *Store) CreateHook(ctx context.Context, token string, channelID, principalID int64) (Hook, error) {
	now := time.Now().Truncate(time.Millisecond)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO hooks (token, channel_id, principal_id, created_at) VALUES (?, ?, ?, ?)",
		token, channelID, principalID, now.UnixMilli())
	if isUniqueConstraintErr(err) {
		return Hook{}, fmt.Errorf("store: create hook: %w", ErrDuplicate)
	}
	if err != nil {
		return Hook{}, fmt.Errorf("store: create hook: %w", err)
	}
	return Hook{Token: token, ChannelID: channelID, PrincipalID: principalID, CreatedAt: now}, nil
}

// HookByToken resolves an ingest token. It returns ErrNotFound for an unknown token.
func (s *Store) HookByToken(ctx context.Context, token string) (Hook, error) {
	var hook Hook
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT token, channel_id, principal_id, created_at FROM hooks WHERE token = ?", token,
	).Scan(&hook.Token, &hook.ChannelID, &hook.PrincipalID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Hook{}, fmt.Errorf("store: find hook: %w", ErrNotFound)
	}
	if err != nil {
		return Hook{}, fmt.Errorf("store: find hook: %w", err)
	}
	hook.CreatedAt = time.UnixMilli(createdAt)
	return hook, nil
}
