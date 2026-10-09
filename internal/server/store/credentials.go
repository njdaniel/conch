package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Errors returned by the credential store, in addition to ErrPrincipalNotFound.
var (
	// ErrCredentialInvalid is the single error ResolveCredential returns for a
	// token that is malformed, unknown, expired, or revoked. The cases are
	// deliberately indistinguishable to callers.
	ErrCredentialInvalid = errors.New("store: credential invalid")
	// ErrCredentialNotFound is returned when a credential id does not exist.
	ErrCredentialNotFound = errors.New("store: credential not found")
	// ErrCredentialRevoked is returned when rotating a revoked credential.
	ErrCredentialRevoked = errors.New("store: credential revoked")
	// ErrCredentialExpired is returned when rotating a credential whose expiry
	// has passed: its expiry cannot be carried over and silently dropping it
	// would turn a time-limited credential into a permanent one.
	ErrCredentialExpired = errors.New("store: credential expired")
	// ErrCredentialExpiryPast is returned when a new credential's expiry is
	// not in the future.
	ErrCredentialExpiryPast = errors.New("store: credential expiry must be in the future")
	// ErrPrincipalDisabled is returned when issuing or rotating a credential
	// for a disabled principal (issue #101).
	ErrPrincipalDisabled = errors.New("store: principal disabled")
)

// credentialNow is the clock used for credential creation, expiry, and
// resolution. It is a variable only so tests can control expiry without
// sleeping; production code never reassigns it.
var credentialNow = time.Now

// newCredentialToken returns a fresh token: the "conch_" prefix followed by 32
// bytes from crypto/rand in unpadded base64url.
func newCredentialToken() (string, error) {
	b := make([]byte, schema.CredentialTokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate credential token: %w", err)
	}
	return schema.CredentialTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashCredentialToken returns the lowercase hex SHA-256 of token.
func hashCredentialToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// wellFormedCredentialToken reports whether token has the exact shape
// newCredentialToken produces. It is a cheap pre-check so malformed input
// never reaches the database.
func wellFormedCredentialToken(token string) bool {
	if len(token) != schema.CredentialTokenLength || !strings.HasPrefix(token, schema.CredentialTokenPrefix) {
		return false
	}
	for _, c := range []byte(token[len(schema.CredentialTokenPrefix):]) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func credentialDetail(id int64, label string) string {
	return fmt.Sprintf("credential=%d label=%q", id, label)
}

func msToTimestampPtr(ms sql.NullInt64) *schema.Timestamp {
	if !ms.Valid {
		return nil
	}
	ts := schema.NewTimestamp(time.UnixMilli(ms.Int64))
	return &ts
}

func nullableMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// insertCredentialTx inserts a credential row for token and returns its wire form.
func insertCredentialTx(ctx context.Context, tx execer, principalID int64, label, token string, now time.Time, expiresAt *time.Time) (schema.CredentialV1, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		principalID, label, hashCredentialToken(token), now.UnixMilli(), nullableMillis(expiresAt))
	if err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: insert credential: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: credential id: %w", err)
	}
	c := schema.CredentialV1{
		ID:          id,
		PrincipalID: principalID,
		Label:       label,
		CreatedAt:   schema.NewTimestamp(now),
	}
	if expiresAt != nil {
		ts := schema.NewTimestamp(*expiresAt)
		c.ExpiresAt = &ts
	}
	return c, nil
}

// CreateCredential issues a new credential for an existing principal (human or
// agent) and returns its description plus the plaintext token, which is shown
// exactly once: only its SHA-256 is stored. A credential_created audit event
// is appended in the same transaction; its detail names the credential id and
// label, never the token or hash.
//
// It returns ErrPrincipalNotFound or ErrCredentialExpiryPast without writing.
func (s *Store) CreateCredential(ctx context.Context, actor string, principalID int64, label string, expiresAt *time.Time) (schema.CredentialV1, string, error) {
	now := credentialNow().Truncate(time.Millisecond)
	var exp *time.Time
	if expiresAt != nil {
		e := expiresAt.Truncate(time.Millisecond)
		if !e.After(now) {
			return schema.CredentialV1{}, "", ErrCredentialExpiryPast
		}
		exp = &e
	}
	token, err := newCredentialToken()
	if err != nil {
		return schema.CredentialV1{}, "", err
	}

	var out schema.CredentialV1
	err = s.withImmediateTx(ctx, func(tx execer) error {
		// The disabled check and the insert share one IMMEDIATE transaction
		// with DisablePrincipal's revoke, so a credential can never be
		// issued after (or concurrently with) a disable and survive it.
		var disabledAt sql.NullInt64
		switch err := tx.QueryRowContext(ctx, "SELECT disabled_at FROM principals WHERE id = ?", principalID).Scan(&disabledAt); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPrincipalNotFound
		case err != nil:
			return fmt.Errorf("store: create credential: find principal: %w", err)
		}
		if disabledAt.Valid {
			return ErrPrincipalDisabled
		}
		c, err := insertCredentialTx(ctx, tx, principalID, label, token, now, exp)
		if err != nil {
			return err
		}
		out = c
		return appendAuditEventTx(ctx, tx, actor, "credential_created",
			principalActor(principalID), credentialDetail(c.ID, c.Label), now)
	})
	if err != nil {
		return schema.CredentialV1{}, "", err
	}
	return out, token, nil
}

// ListCredentials returns every credential of a principal, revoked and expired
// ones included, newest first (ties by id descending). The result is never
// nil. It returns ErrPrincipalNotFound for an unknown principal.
func (s *Store) ListCredentials(ctx context.Context, principalID int64) ([]schema.CredentialV1, error) {
	var one int
	switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM principals WHERE id = ?", principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrPrincipalNotFound
	case err != nil:
		return nil, fmt.Errorf("store: list credentials: find principal: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, principal_id, label, created_at, expires_at, revoked_at
		 FROM credentials WHERE principal_id = ? ORDER BY created_at DESC, id DESC`, principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []schema.CredentialV1{}
	for rows.Next() {
		var (
			c                    schema.CredentialV1
			createdAt            int64
			expiresAt, revokedAt sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.PrincipalID, &c.Label, &createdAt, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("store: list credentials: scan: %w", err)
		}
		c.CreatedAt = schema.NewTimestamp(time.UnixMilli(createdAt))
		c.ExpiresAt = msToTimestampPtr(expiresAt)
		c.RevokedAt = msToTimestampPtr(revokedAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	return out, nil
}

// RotateCredential issues a new credential for the same principal with the
// same label and expiry, and revokes the old one, in one transaction; a
// credential_rotated audit event is appended in it. The new token is returned
// once. Concurrent rotations of one credential serialize: exactly one wins and
// the rest get ErrCredentialRevoked.
//
// It returns ErrCredentialNotFound, ErrCredentialRevoked, or ErrCredentialExpired
// (the old credential's expiry has passed) without writing anything.
func (s *Store) RotateCredential(ctx context.Context, actor string, credentialID int64) (schema.CredentialV1, string, error) {
	token, err := newCredentialToken()
	if err != nil {
		return schema.CredentialV1{}, "", err
	}
	var out schema.CredentialV1
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var (
			principalID          int64
			label                string
			expiresAt, revokedAt sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id, label, expires_at, revoked_at FROM credentials WHERE id = ?", credentialID,
		).Scan(&principalID, &label, &expiresAt, &revokedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrCredentialNotFound
		case err != nil:
			return fmt.Errorf("store: rotate credential: read: %w", err)
		}
		var disabledAt sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT disabled_at FROM principals WHERE id = ?", principalID).Scan(&disabledAt); err != nil {
			return fmt.Errorf("store: rotate credential: find principal: %w", err)
		}
		if disabledAt.Valid {
			return ErrPrincipalDisabled
		}
		if revokedAt.Valid {
			return ErrCredentialRevoked
		}
		now := credentialNow().Truncate(time.Millisecond)
		var exp *time.Time
		if expiresAt.Valid {
			e := time.UnixMilli(expiresAt.Int64)
			if !e.After(now) {
				return ErrCredentialExpired
			}
			exp = &e
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), credentialID)
		if err != nil {
			return fmt.Errorf("store: rotate credential: revoke old: %w", err)
		}
		// BEGIN IMMEDIATE already serializes this against other writers, so
		// exactly one row changes. Checking anyway means a second credential
		// can never be issued for one revocation even if that ever changes.
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: rotate credential: revoke old: %w", err)
		} else if n != 1 {
			return ErrCredentialRevoked
		}
		c, err := insertCredentialTx(ctx, tx, principalID, label, token, now, exp)
		if err != nil {
			return err
		}
		out = c
		detail := credentialDetail(c.ID, c.Label) + fmt.Sprintf(" replaces=%d", credentialID)
		return appendAuditEventTx(ctx, tx, actor, "credential_rotated", principalActor(principalID), detail, now)
	})
	if err != nil {
		return schema.CredentialV1{}, "", err
	}
	return out, token, nil
}

// RevokeCredential sets revoked_at on a credential, keeping the row. Revoking
// an already-revoked credential succeeds and changes nothing (the original
// revoked_at stands and no second audit event is written). A credential_revoked
// audit event is appended in the same transaction as the first revocation.
// It returns ErrCredentialNotFound for an unknown id.
func (s *Store) RevokeCredential(ctx context.Context, actor string, credentialID int64) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		var (
			principalID int64
			label       string
			revokedAt   sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id, label, revoked_at FROM credentials WHERE id = ?", credentialID,
		).Scan(&principalID, &label, &revokedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrCredentialNotFound
		case err != nil:
			return fmt.Errorf("store: revoke credential: read: %w", err)
		}
		if revokedAt.Valid {
			return nil
		}
		now := credentialNow().Truncate(time.Millisecond)
		// Refuse to revoke the last live credential any enabled operator
		// holds: nobody could sign in afterwards (see ErrLastOperator).
		role, disabled, err := principalState(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if role == RoleOperator && !disabled {
			others, err := liveOperatorCredentialsTx(ctx, tx, 0, credentialID, now)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastOperator
			}
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), credentialID)
		if err != nil {
			return fmt.Errorf("store: revoke credential: %w", err)
		}
		// Same guard as rotation: no audit event unless this call revoked it.
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: revoke credential: %w", err)
		} else if n != 1 {
			return nil
		}
		return appendAuditEventTx(ctx, tx, actor, "credential_revoked",
			principalActor(principalID), credentialDetail(credentialID, label), now)
	})
}
