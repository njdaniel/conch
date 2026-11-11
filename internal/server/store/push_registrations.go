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
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Push registrations (issue #1714, Mobile M11-B;
// docs/design/mobile-notifications.md §5). A registration is an address one
// credential, which is one sign-in, wants woken. It holds no preference (those
// are notification_preferences) and this file sends nothing: the wake is
// #1715's.
//
// The target is write-only. The endpoint, the keys and the device token are
// stored so that a later slice can send, and are read by no route, no audit
// row, no log line and no error: the only things that leave this file are the
// registration's id, its transport, platform and app, its fingerprint, and its
// times. When a registration ends the target columns are cleared in the same
// statement (the table's CHECK demands it), so a row that is no longer
// addressed holds nothing a leak could use (threat T9).
//
// Every registration ends in one of these ways, each with one reason and one
// push_registration_ended audit row on the principal, written in the
// transaction that ends it:
//
//   - self: its own credential deleted it (DeletePushRegistration);
//   - credential_revoked: its credential was revoked, at any of the sites that
//     call endCredentialNotificationsTx;
//   - credential_expired and stale: the session sweep (SweepSessions);
//   - stale again: the server's VAPID key changed (ReconcilePushVAPIDKey);
//   - provider_rejected: #1715.

// PushStaleAfter is how long a registration may go with no fetch and no update
// before the sweep ends it as stale (design §5.8: FCM's "a month" with slack).
const PushStaleAfter = 35 * 24 * time.Hour

// settingPushVAPIDFingerprint is the server_settings key of the fingerprint of
// the VAPID key the webpush registrations were made under.
const settingPushVAPIDFingerprint = "push_vapid_fingerprint"

// ErrPushRegistrationNotFound is returned when the credential has no active
// registration of that id. It is the same for another credential's, an ended
// one and one never issued, so a caller can tell none of them apart.
var ErrPushRegistrationNotFound = errors.New("store: push registration not found")

// ErrTooManyPushRegistrations is returned when the credential already holds
// schema.MaxPushRegistrationsPerCredential active registrations, or its
// principal schema.MaxPushRegistrationsPerPrincipal. The store does not say
// which.
var ErrTooManyPushRegistrations = errors.New("store: too many push registrations")

// PushTarget is a registration's write-only half and the fields around it, as
// a create or a replace request gives them, with the padding of p256dh and
// auth already dropped (schema's Normalized).
type PushTarget struct {
	Transport schema.PushTransport
	Platform  schema.PushPlatform
	App       string
	Endpoint  string
	P256DH    string
	Auth      string
	Provider  schema.PushProvider
	Token     string
}

// Fingerprint is the target's fingerprint: the first 8 bytes of the SHA-256
// of the canonical target, in lowercase hex. The canonical target is the
// transport, then the fields that transport has, each after a NUL. The same
// target always has the same fingerprint, and so it can be compared and not
// read. It is 64 bits of a hash of a long unguessable value: it does not let
// anyone recover the target.
func (t PushTarget) Fingerprint() string {
	var canonical string
	switch t.Transport {
	case schema.PushTransportWebPush:
		canonical = "webpush\x00" + t.Endpoint + "\x00" + t.P256DH + "\x00" + t.Auth
	default:
		canonical = string(t.Transport) + "\x00" + string(t.Provider) + "\x00" + t.Token
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// pushRegistrationColumns are the columns a registration is read from: none of
// the target.
const pushRegistrationColumns = `id, transport, platform, app, target_fingerprint, created_at, last_wake_at, last_fetch_at`

func scanPushRegistration(sc interface{ Scan(dest ...any) error }) (schema.PushRegistrationV1, error) {
	var r schema.PushRegistrationV1
	var transport, platform string
	var created int64
	var wake, fetch sql.NullInt64
	if err := sc.Scan(&r.ID, &transport, &platform, &r.App, &r.TargetFingerprint, &created, &wake, &fetch); err != nil {
		return schema.PushRegistrationV1{}, err
	}
	r.Transport = schema.PushTransport(transport)
	r.Platform = schema.PushPlatform(platform)
	r.CreatedAt = schema.NewTimestamp(time.UnixMilli(created))
	r.LastWakeAt = msToTimestampPtr(wake)
	r.LastFetchAt = msToTimestampPtr(fetch)
	return r, nil
}

// pushDetail is the detail of a registration's audit rows: its id and
// transport, and for an end its reason. Never a target, an app or a
// fingerprint.
func pushDetail(id string, transport string, reason string) string {
	d := "registration=" + id + " transport=" + transport
	if reason != "" {
		d += " reason=" + reason
	}
	return d
}

// CreatePushRegistration registers t for credentialID and returns the
// registration. It refuses with ErrNotFound a credential that is not live
// (a request that raced a revocation must leave no row nothing will end), and
// with ErrTooManyPushRegistrations one at its limit or whose principal is at
// its own. The principal's count is over its active registrations on
// credentials that are live now. The limits, the insert and the
// push_registration_created audit row are one transaction under the write
// lock, so two requests cannot both take the last place.
func (s *Store) CreatePushRegistration(ctx context.Context, credentialID int64, t PushTarget, now time.Time) (schema.PushRegistrationV1, error) {
	now = now.Truncate(time.Millisecond)
	idRaw := make([]byte, schema.PushRegistrationIDBytes)
	if _, err := rand.Read(idRaw); err != nil {
		return schema.PushRegistrationV1{}, fmt.Errorf("store: create push registration: random id: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(idRaw)
	fingerprint := t.Fingerprint()
	var out schema.PushRegistrationV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var principalID int64
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id FROM credentials WHERE id = ? AND revoked_at IS NULL", credentialID).Scan(&principalID); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: create push registration: find credential: %w", err)
		}
		var own, all int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM push_registrations WHERE credential_id = ? AND state = 'active'", credentialID).Scan(&own); err != nil {
			return fmt.Errorf("store: create push registration: count: %w", err)
		}
		if own >= schema.MaxPushRegistrationsPerCredential {
			return ErrTooManyPushRegistrations
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM push_registrations r JOIN credentials c ON c.id = r.credential_id
			 WHERE r.principal_id = ? AND r.state = 'active' AND c.revoked_at IS NULL
			   AND (c.expires_at IS NULL OR c.expires_at > ?)`,
			principalID, now.UnixMilli()).Scan(&all); err != nil {
			return fmt.Errorf("store: create push registration: count: %w", err)
		}
		if all >= schema.MaxPushRegistrationsPerPrincipal {
			return ErrTooManyPushRegistrations
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO push_registrations
			     (id, credential_id, principal_id, transport, platform, app, endpoint, p256dh, auth, provider, token,
			      target_fingerprint, created_at, updated_at, state)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active')`,
			id, credentialID, principalID, string(t.Transport), string(t.Platform), t.App,
			t.Endpoint, t.P256DH, t.Auth, string(t.Provider), t.Token, fingerprint, now.UnixMilli(), now.UnixMilli()); err != nil {
			// The statement's arguments are the target; none is in the error.
			return fmt.Errorf("store: create push registration: insert: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, principalActor(principalID), schema.AuditActionPushRegistrationCreated,
			principalActor(principalID), pushDetail(id, string(t.Transport), ""), now); err != nil {
			return err
		}
		out = schema.PushRegistrationV1{
			ID: id, Transport: t.Transport, Platform: t.Platform, App: t.App,
			TargetFingerprint: fingerprint, CreatedAt: schema.NewTimestamp(now),
		}
		return nil
	})
	if err != nil {
		return schema.PushRegistrationV1{}, err
	}
	return out, nil
}

// GetOwnPushRegistration returns credentialID's active registration id, or
// ErrPushRegistrationNotFound: for another credential's, an ended one and one
// that never existed alike.
func (s *Store) GetOwnPushRegistration(ctx context.Context, credentialID int64, id string) (schema.PushRegistrationV1, error) {
	r, err := scanPushRegistration(s.db.QueryRowContext(ctx,
		"SELECT "+pushRegistrationColumns+" FROM push_registrations WHERE id = ? AND credential_id = ? AND state = 'active'",
		id, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return schema.PushRegistrationV1{}, ErrPushRegistrationNotFound
	}
	if err != nil {
		return schema.PushRegistrationV1{}, fmt.Errorf("store: read push registration: %w", err)
	}
	return r, nil
}

// ListOwnPushRegistrations returns credentialID's active registrations,
// oldest first. It is never nil.
func (s *Store) ListOwnPushRegistrations(ctx context.Context, credentialID int64) ([]schema.PushRegistrationV1, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+pushRegistrationColumns+" FROM push_registrations WHERE credential_id = ? AND state = 'active' ORDER BY created_at, id",
		credentialID)
	if err != nil {
		return nil, fmt.Errorf("store: list push registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.PushRegistrationV1{}
	for rows.Next() {
		r, err := scanPushRegistration(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list push registrations: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list push registrations: %w", err)
	}
	return out, nil
}

// PushCredentials returns which of principalID's credentials hold an active
// registration, for OwnCredentialV1.Push.
func (s *Store) PushCredentials(ctx context.Context, principalID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT credential_id FROM push_registrations WHERE principal_id = ? AND state = 'active'", principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list push credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list push credentials: scan: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list push credentials: %w", err)
	}
	return out, nil
}

// ReplacePushTarget replaces the target and the app of credentialID's active
// registration id with t's, bumps updated_at (which resets the stale clock)
// and returns the registration. The transport and the platform are the
// registration's own and cannot change: a t that names others finds nothing
// to update. It is ErrPushRegistrationNotFound for another credential's, an
// ended one and one that never existed alike.
func (s *Store) ReplacePushTarget(ctx context.Context, credentialID int64, id string, t PushTarget, now time.Time) (schema.PushRegistrationV1, error) {
	now = now.Truncate(time.Millisecond)
	var out schema.PushRegistrationV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE push_registrations SET app = ?, endpoint = ?, p256dh = ?, auth = ?, provider = ?, token = ?,
			        target_fingerprint = ?, updated_at = ?
			 WHERE id = ? AND credential_id = ? AND state = 'active' AND transport = ? AND platform = ?`,
			t.App, t.Endpoint, t.P256DH, t.Auth, string(t.Provider), t.Token, t.Fingerprint(), now.UnixMilli(),
			id, credentialID, string(t.Transport), string(t.Platform))
		if err != nil {
			return fmt.Errorf("store: replace push target: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: replace push target: %w", err)
		} else if n != 1 {
			return ErrPushRegistrationNotFound
		}
		out, err = scanPushRegistration(tx.QueryRowContext(ctx,
			"SELECT "+pushRegistrationColumns+" FROM push_registrations WHERE id = ?", id))
		if err != nil {
			return fmt.Errorf("store: replace push target: read: %w", err)
		}
		return nil
	})
	if err != nil {
		return schema.PushRegistrationV1{}, err
	}
	return out, nil
}

// DeletePushRegistration ends credentialID's active registration id with
// reason self, in one transaction with its audit row. It is
// ErrPushRegistrationNotFound for another credential's, an ended one and one
// that never existed alike.
func (s *Store) DeletePushRegistration(ctx context.Context, credentialID int64, id string, now time.Time) error {
	now = now.Truncate(time.Millisecond)
	return s.withImmediateTx(ctx, func(tx execer) error {
		var principalID int64
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id FROM push_registrations WHERE id = ? AND credential_id = ? AND state = 'active'",
			id, credentialID).Scan(&principalID); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPushRegistrationNotFound
		case err != nil:
			return fmt.Errorf("store: delete push registration: %w", err)
		}
		n, err := expirePushRegistrationsTx(ctx, tx, principalActor(principalID), "id = ?", []any{id},
			schema.PushRegistrationEndSelf, now)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrPushRegistrationNotFound
		}
		return nil
	})
}

// TouchPushFetch records that credentialID read the pending feed at now, on
// each of its active registrations, in one statement. It resets the stale
// clock (design §5.8).
func (s *Store) TouchPushFetch(ctx context.Context, credentialID int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE push_registrations SET last_fetch_at = ? WHERE credential_id = ? AND state = 'active'",
		now.UnixMilli(), credentialID); err != nil {
		return fmt.Errorf("store: record push fetch of credential %d: %w", credentialID, err)
	}
	return nil
}

// expirePushRegistrationsTx ends every active registration that matches
// where, which is a fixed condition written by this package and never request
// text: state expired, the reason, ended_at, the target cleared, and one
// push_registration_ended row each on the registration's principal with actor
// as the actor. The caller holds the write lock. It returns how many it ended.
func expirePushRegistrationsTx(ctx context.Context, tx execer, actor, where string, args []any, reason schema.PushRegistrationEndReason, now time.Time) (int, error) {
	type ending struct {
		id, transport string
		principalID   int64
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id, principal_id, transport FROM push_registrations WHERE state = 'active' AND ("+where+") ORDER BY id", args...) // #nosec G202 -- where is a constant of this package
	if err != nil {
		return 0, fmt.Errorf("store: list push registrations to end: %w", err)
	}
	var found []ending
	for rows.Next() {
		var e ending
		if err := rows.Scan(&e.id, &e.principalID, &e.transport); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: list push registrations to end: scan: %w", err)
		}
		found = append(found, e)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: list push registrations to end: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: list push registrations to end: %w", err)
	}
	for _, e := range found {
		if _, err := tx.ExecContext(ctx,
			`UPDATE push_registrations
			 SET state = 'expired', ended_at = ?, end_reason = ?,
			     endpoint = '', p256dh = '', auth = '', provider = '', token = ''
			 WHERE id = ? AND state = 'active'`,
			now.UnixMilli(), string(reason), e.id); err != nil {
			return 0, fmt.Errorf("store: end push registration: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, schema.AuditActionPushRegistrationEnded,
			principalActor(e.principalID), pushDetail(e.id, e.transport, string(reason)), now); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// endGenerationDeadPushTx ends, as credential_revoked, the active
// registrations of credentials whose authentication generation is no longer
// their principal's, restricted by credWhere (a constant of this package over
// the credentials table, aliased c). Such a credential is dead though
// revoked_at is unset (a bump of the generation, as a restore does), so no
// revocation site ran for it. It skips a database that has no registrations
// table yet.
func endGenerationDeadPushTx(ctx context.Context, tx execer, actor, credWhere string, args []any, now time.Time) error {
	exists, err := tableExistsTx(ctx, tx, "push_registrations")
	if err != nil || !exists {
		return err
	}
	_, err = expirePushRegistrationsTx(ctx, tx, actor,
		`credential_id IN (SELECT c.id FROM credentials c JOIN principals p ON p.id = c.principal_id
		                   WHERE c.auth_generation <> p.auth_generation AND c.`+credWhere+`)`, // #nosec G202 -- credWhere is a constant of this package
		args, schema.PushRegistrationEndCredentialRevoked, now)
	return err
}

// sweepPushRegistrationsTx ends the registrations that nothing else will: those
// of credentials that expired or went idle (reason credential_expired: nobody
// revoked anything), then those with no fetch and no update for PushStaleAfter
// (stale). The order matters for the reason: a registration of an expired
// credential that is also silent is ended as the former. It returns how many
// it ended.
func sweepPushRegistrationsTx(ctx context.Context, tx execer, now time.Time) (int, error) {
	// Credentials a generation bump killed are revoked in effect, and any
	// that a bump's own transaction did not reach (a bump from before this
	// code, a restore of such a database) are ended here.
	if err := endGenerationDeadPushTx(ctx, tx, "system", "id IS NOT NULL", nil, now); err != nil {
		return 0, err
	}
	expired, err := expirePushRegistrationsTx(ctx, tx, "system",
		`credential_id IN (
		     SELECT id FROM credentials
		     WHERE (expires_at IS NOT NULL AND expires_at <= ?1)
		        OR (idle_ms IS NOT NULL AND COALESCE(last_used_at, created_at) + idle_ms <= ?1))`,
		[]any{now.UnixMilli()}, schema.PushRegistrationEndCredentialExpired, now)
	if err != nil {
		return 0, err
	}
	stale, err := expirePushRegistrationsTx(ctx, tx, "system",
		"MAX(updated_at, COALESCE(last_fetch_at, 0)) <= ?",
		[]any{now.Add(-PushStaleAfter).UnixMilli()}, schema.PushRegistrationEndStale, now)
	if err != nil {
		return 0, err
	}
	return expired + stale, nil
}

// endCredentialPushTx ends every active registration of credentialID with
// reason credential_revoked, in the caller's transaction, as the actor that
// revoked the credential. See endCredentialNotificationsTx.
func endCredentialPushTx(ctx context.Context, tx execer, actor string, credentialID int64, now time.Time) error {
	_, err := expirePushRegistrationsTx(ctx, tx, actor, "credential_id = ?", []any{credentialID},
		schema.PushRegistrationEndCredentialRevoked, now)
	return err
}

// ReconcilePushVAPIDKey records fingerprint as the server's VAPID key and, if
// it is not the key the webpush registrations were made under, ends every
// active webpush registration with reason stale, audited, in the same
// transaction (decision 6 on #1116): a Web Push subscription is bound to the
// application server key it was made with, so a lost or replaced key leaves
// every one of them dead. A key never recorded before is a replacement as far
// as any webpush registration that exists is concerned. It returns whether a
// different key was on record, and how many it ended. Called at start, before
// anything is served, and only with webpush on.
func (s *Store) ReconcilePushVAPIDKey(ctx context.Context, fingerprint string, now time.Time) (replaced bool, ended int, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		replaced, ended = false, 0
		var recorded string
		switch err := tx.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", settingPushVAPIDFingerprint).Scan(&recorded); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("store: read push key fingerprint: %w", err)
		case recorded == fingerprint:
			return nil
		default:
			replaced = true
		}
		n, err := expirePushRegistrationsTx(ctx, tx, "system", "transport = 'webpush'", nil,
			schema.PushRegistrationEndStale, now)
		if err != nil {
			return err
		}
		ended = n
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			settingPushVAPIDFingerprint, fingerprint, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: record push key fingerprint: %w", err)
		}
		return nil
	})
	return replaced, ended, err
}
