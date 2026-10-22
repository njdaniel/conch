package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// currentAuthSQL is the SQL condition, over credential c and its
// principal p, that the credential was issued under the principal's present
// authentication generation and has full scope (issue #707). Moving the
// generation on ends every credential issued before it without touching
// their rows. A setup-scoped credential does not meet it: only
// ResolveRequestCredential admits one, for the authentication middleware,
// which lets it reach the few routes that finish setup (issue #709); every
// other check refuses it, which fails closed.
const currentAuthSQL = `(c.auth_generation = p.auth_generation AND c.scope = 'full')`

// credentialRenewStep is how far a renewable credential's expiry must have
// fallen behind before a use moves it (issue #439): at most one write a day
// per credential, however often it is used.
const credentialRenewStep = 24 * time.Hour

// ResolveCredential returns the principal bound to a live bearer token.
//
// Every token problem -- wrong shape, unknown, expired (expires_at <= now),
// idle past its idle bound, revoked, belonging to a disabled principal, or to
// a guest whose visit has ended or passed its deadline (issue #705) -- yields
// the same ErrCredentialInvalid. A malformed token is rejected before any
// database work. A genuine database failure is returned as a different, wrapped error
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
	p, c, err := s.resolveCredential(ctx, token, false)
	return p, c.ID, err
}

// RequestCredential is what the authentication middleware learns of the
// credential a request came with, beside its principal (issue #709).
type RequestCredential struct {
	ID    int64
	Scope schema.CredentialScope
	// Session says it is a sign-in session (signInSessionSQL), not a script
	// credential: the recent-auth gate applies to it.
	Session bool
	// IdleBounded says it has an idle bound, which a REST request moves
	// (NoteGuestUse): a guest's, or an operator's browser session.
	IdleBounded bool
	// RecentAuthUntil is its recent-auth proof as it was read, nil when it
	// has none.
	RecentAuthUntil *time.Time
}

// ResolveRequestCredential is ResolveCredentialDetail for the authentication
// middleware, the one caller that may meet a setup-scoped credential: such a
// credential resolves here (and nowhere else), and the middleware lets it
// reach only the routes that finish setup (design §5.3, review F6). Every
// other caller resolves full scope only, so a setup credential that reached
// one of them by mistake would still be refused.
func (s *Store) ResolveRequestCredential(ctx context.Context, token string) (Principal, RequestCredential, error) {
	return s.resolveCredential(ctx, token, true)
}

func (s *Store) resolveCredential(ctx context.Context, token string, admitSetup bool) (Principal, RequestCredential, error) {
	if !wellFormedCredentialToken(token) {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	want := hashCredentialToken(token)

	var (
		cred                           RequestCredential
		storedHash, scope              string
		expiresAt, revokedAt, disabled sql.NullInt64
		renewMs, recentAuth            sql.NullInt64
		invitedBy, inviteID            sql.NullInt64
		p                              Principal
		kind, role                     string
		createdAt                      int64
		idle                           credentialIdle
		generationCurrent              bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT c.id, c.token_hash, c.expires_at, c.revoked_at, c.renew_ms, p.disabled_at, p.id, p.kind, p.name, p.role, p.created_at,
		        p.invited_by, p.invite_id, p.may_invite, p.voice_allowed, `+idleBoundColumns+`,
		        c.auth_generation = p.auth_generation, c.scope, c.recent_auth_until
		 FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE c.token_hash = ?`, want,
	).Scan(&cred.ID, &storedHash, &expiresAt, &revokedAt, &renewMs, &disabled, &p.ID, &kind, &p.Name, &role, &createdAt,
		&invitedBy, &inviteID, &p.MayInvite, &p.VoiceAllowed, &idle.createdAt, &idle.idleMs, &idle.lastUsedAt,
		&idle.guestState, &idle.guestExpiresAt, &generationCurrent, &scope, &recentAuth)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	if err != nil {
		return Principal{}, RequestCredential{}, fmt.Errorf("store: resolve credential: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(want)) != 1 {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	// The scope check is currentAuthSQL's, with setup let through only when
	// the caller asked for it. A scope this code does not know is refused.
	cred.Scope = schema.CredentialScope(scope)
	scopeOK := cred.Scope == schema.CredentialScopeFull || (admitSetup && cred.Scope == schema.CredentialScopeSetup)
	if revokedAt.Valid || disabled.Valid || !generationCurrent || !scopeOK {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	now := credentialNow()
	if expiresAt.Valid && expiresAt.Int64 <= now.UnixMilli() {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	if !idle.live(now) {
		return Principal{}, RequestCredential{}, ErrCredentialInvalid
	}
	// Every refusal has returned above: only a credential that resolved is
	// ever renewed.
	if renewMs.Valid && expiresAt.Valid {
		s.renewCredential(ctx, cred.ID, expiresAt.Int64, renewMs.Int64, now)
	}
	p.Kind = PrincipalKind(kind)
	p.Role = Role(role)
	p.CreatedAt = time.UnixMilli(createdAt)
	p.InvitedBy = invitedBy.Int64
	p.InviteID = inviteID.Int64
	p.GuestState = schema.GuestState(idle.guestState.String)
	cred.Session = renewMs.Valid || idle.idleMs.Valid
	cred.IdleBounded = idle.idleMs.Valid
	cred.RecentAuthUntil = millisPtr(recentAuth)
	return p, cred, nil
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
// not revoked, has not expired, has not been idle past its idle bound, its
// principal is not disabled, for a guest the visit is active and before its
// deadline (credentialIdle.live), and it is current (currentAuthSQL). It is
// one primary-key read joined to the principal's primary key and is how an
// open connection re-checks the credential it authenticated with. A
// database failure is returned as an error; callers must treat it as "not
// live".
func (s *Store) CredentialLive(ctx context.Context, id int64) (bool, error) {
	var expiresAt, revokedAt, disabledAt sql.NullInt64
	var idle credentialIdle
	var current bool
	err := s.db.QueryRowContext(ctx,
		`SELECT c.expires_at, c.revoked_at, p.disabled_at, `+idleBoundColumns+`, `+currentAuthSQL+`
		 FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE c.id = ?`, id,
	).Scan(&expiresAt, &revokedAt, &disabledAt, &idle.createdAt, &idle.idleMs, &idle.lastUsedAt,
		&idle.guestState, &idle.guestExpiresAt, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: credential liveness: %w", err)
	}
	if revokedAt.Valid || disabledAt.Valid || !current {
		return false, nil
	}
	now := credentialNow()
	if expiresAt.Valid && expiresAt.Int64 <= now.UnixMilli() {
		return false, nil
	}
	return idle.live(now), nil
}
