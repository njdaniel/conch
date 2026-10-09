package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResolveCredential returns the principal bound to a live bearer token.
//
// Every token problem -- wrong shape, unknown, expired (expires_at <= now),
// revoked, or belonging to a disabled principal -- yields the same
// ErrCredentialInvalid. A malformed token is rejected before any database
// work. A genuine database failure is returned as a different, wrapped error
// so callers can tell "bad token" from "store is broken"; callers must fail
// closed on both. The token is never logged or included in any error.
func (s *Store) ResolveCredential(ctx context.Context, token string) (Principal, error) {
	p, _, err := s.ResolveCredentialDetail(ctx, token)
	return p, err
}

// ResolveCredentialDetail is ResolveCredential that also returns the id of the
// credential the token matched, so callers can re-check that one credential
// later (CredentialLive) without a second token lookup.
func (s *Store) ResolveCredentialDetail(ctx context.Context, token string) (Principal, int64, error) {
	if !wellFormedCredentialToken(token) {
		return Principal{}, 0, ErrCredentialInvalid
	}
	want := hashCredentialToken(token)

	var (
		credID                         int64
		storedHash                     string
		expiresAt, revokedAt, disabled sql.NullInt64
		p                              Principal
		kind, role                     string
		createdAt                      int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT c.id, c.token_hash, c.expires_at, c.revoked_at, p.disabled_at, p.id, p.kind, p.name, p.role, p.created_at
		 FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE c.token_hash = ?`, want,
	).Scan(&credID, &storedHash, &expiresAt, &revokedAt, &disabled, &p.ID, &kind, &p.Name, &role, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, 0, ErrCredentialInvalid
	}
	if err != nil {
		return Principal{}, 0, fmt.Errorf("store: resolve credential: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(want)) != 1 {
		return Principal{}, 0, ErrCredentialInvalid
	}
	if revokedAt.Valid || disabled.Valid {
		return Principal{}, 0, ErrCredentialInvalid
	}
	if expiresAt.Valid && expiresAt.Int64 <= credentialNow().UnixMilli() {
		return Principal{}, 0, ErrCredentialInvalid
	}
	p.Kind = PrincipalKind(kind)
	p.Role = Role(role)
	p.CreatedAt = time.UnixMilli(createdAt)
	return p, credID, nil
}

// CredentialLive reports whether credential id is still usable: it exists, is
// not revoked, has not expired, and its principal is not disabled. It is one
// primary-key read joined to the principal's primary key and is how an open
// connection re-checks the credential it authenticated with. A database
// failure is returned as an error; callers must treat it as "not live".
func (s *Store) CredentialLive(ctx context.Context, id int64) (bool, error) {
	var expiresAt, revokedAt, disabledAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT c.expires_at, c.revoked_at, p.disabled_at
		 FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE c.id = ?`, id,
	).Scan(&expiresAt, &revokedAt, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: credential liveness: %w", err)
	}
	if revokedAt.Valid || disabledAt.Valid {
		return false, nil
	}
	if expiresAt.Valid && expiresAt.Int64 <= credentialNow().UnixMilli() {
		return false, nil
	}
	return true, nil
}
