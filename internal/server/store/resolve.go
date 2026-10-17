package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// credentialRenewStep is how far a renewable credential's expiry must have
// fallen behind before a use moves it (issue #439): at most one write a day
// per credential, however often it is used.
const credentialRenewStep = 24 * time.Hour

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
//
// A renewable credential is renewed here, and only once it has resolved: see
// renewCredential.
func (s *Store) ResolveCredentialDetail(ctx context.Context, token string) (Principal, int64, error) {
	if !wellFormedCredentialToken(token) {
		return Principal{}, 0, ErrCredentialInvalid
	}
	want := hashCredentialToken(token)

	var (
		credID                         int64
		storedHash                     string
		expiresAt, revokedAt, disabled sql.NullInt64
		renewMs                        sql.NullInt64
		invitedBy, inviteID            sql.NullInt64
		p                              Principal
		kind, role                     string
		createdAt                      int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT c.id, c.token_hash, c.expires_at, c.revoked_at, c.renew_ms, p.disabled_at, p.id, p.kind, p.name, p.role, p.created_at,
		        p.invited_by, p.invite_id, p.may_invite, p.voice_allowed
		 FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE c.token_hash = ?`, want,
	).Scan(&credID, &storedHash, &expiresAt, &revokedAt, &renewMs, &disabled, &p.ID, &kind, &p.Name, &role, &createdAt,
		&invitedBy, &inviteID, &p.MayInvite, &p.VoiceAllowed)
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
	now := credentialNow()
	if expiresAt.Valid && expiresAt.Int64 <= now.UnixMilli() {
		return Principal{}, 0, ErrCredentialInvalid
	}
	// Every refusal has returned above: only a credential that resolved is
	// ever renewed.
	if renewMs.Valid && expiresAt.Valid {
		s.renewCredential(ctx, credID, expiresAt.Int64, renewMs.Int64, now)
	}
	p.Kind = PrincipalKind(kind)
	p.Role = Role(role)
	p.CreatedAt = time.UnixMilli(createdAt)
	p.InvitedBy = invitedBy.Int64
	p.InviteID = inviteID.Int64
	return p, credID, nil
}

// renewCredential moves a renewable credential's expiry to renewMs past now,
// when it has fallen more than credentialRenewStep behind that (issue #439).
// The caller has just resolved the credential at now, with expiresAt as read.
//
// The update is guarded on the row as it was read: still unrevoked, still
// with that expiry, and that expiry still ahead of now. So a credential that
// was revoked (a disable revokes too) or has expired in between is never
// revived, and of two requests racing to renew one credential only the first
// writes. A failed write costs only the renewal, which the next use tries
// again, so it is logged and not returned: the request has already
// authenticated. The token is not in reach here, so it cannot be logged.
func (s *Store) renewCredential(ctx context.Context, credID, expiresAt, renewMs int64, now time.Time) {
	nowMs := now.UnixMilli()
	if expiresAt-nowMs >= renewMs-credentialRenewStep.Milliseconds() {
		return
	}
	// Not cancelled with the request: the renewal is one short write.
	ctx = context.WithoutCancel(ctx)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET expires_at = ?
		 WHERE id = ? AND revoked_at IS NULL AND renew_ms = ? AND expires_at = ? AND expires_at > ?`,
		nowMs+renewMs, credID, renewMs, expiresAt, nowMs); err != nil {
		slog.WarnContext(ctx, "store: renew credential failed; its next use tries again", "credential", credID, "error", err)
	}
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
