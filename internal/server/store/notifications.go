package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/njdaniel/conch/pkg/schema"
)

// Notification preferences (issue #1713, Mobile M11-A;
// docs/design/mobile-notifications.md §6.1).
//
// One row per credential, which is one sign-in: a phone on "mentions" and a
// desk on "all" are two credentials. No row means
// schema.DefaultNotificationPreferences(). The row belongs to the credential
// and ends with it: every site that revokes a credential calls
// endCredentialNotificationsTx in the same transaction
// (TestEveryCredentialRevocationEndsItsNotifications keeps it so), and the
// session sweep deletes the rows of credentials that expired or went idle,
// which are not revocations and so have no site of their own.
//
// This file keeps the preferences. The feed they shape is read in audience.go,
// which is the only code that reads message rows.

// GetNotificationPreferences returns the preferences of credentialID as
// stored, or the defaults when it has written none. Muted is never nil.
func (s *Store) GetNotificationPreferences(ctx context.Context, credentialID int64) (schema.NotificationPreferencesV1, error) {
	return notificationPreferencesOf(ctx, s.db, credentialID)
}

func notificationPreferencesOf(ctx context.Context, q execer, credentialID int64) (schema.NotificationPreferencesV1, error) {
	var policy, muted string
	var preview, quiet int
	err := q.QueryRowContext(ctx,
		`SELECT policy, preview, quiet_when_unreachable, muted FROM notification_preferences WHERE credential_id = ?`,
		credentialID).Scan(&policy, &preview, &quiet, &muted)
	if errors.Is(err, sql.ErrNoRows) {
		return schema.DefaultNotificationPreferences(), nil
	}
	if err != nil {
		return schema.NotificationPreferencesV1{}, fmt.Errorf("store: read notification preferences of credential %d: %w", credentialID, err)
	}
	prefs := schema.NotificationPreferencesV1{
		Policy:               schema.NotificationPolicy(policy),
		Muted:                []int64{},
		Preview:              preview == 1,
		QuietWhenUnreachable: quiet == 1,
	}
	if err := json.Unmarshal([]byte(muted), &prefs.Muted); err != nil || prefs.Muted == nil {
		return schema.NotificationPreferencesV1{}, fmt.Errorf("store: read notification preferences of credential %d: unreadable muted list", credentialID)
	}
	return prefs, nil
}

// NormalizeNotificationPreferences returns prefs as the store keeps them:
// muted deduplicated, ascending, and never nil. pkg/schema has no normaliser
// of its own for these (its Validate refuses a bad list and says the server
// stores a repeated id once), so the rule is written here, once, for the
// stored form and the answer alike. The caller has validated prefs.
func NormalizeNotificationPreferences(prefs schema.NotificationPreferencesV1) schema.NotificationPreferencesV1 {
	muted := slices.Clone(prefs.Muted)
	slices.Sort(muted)
	prefs.Muted = slices.Compact(muted)
	if prefs.Muted == nil {
		prefs.Muted = []int64{}
	}
	return prefs
}

// PutNotificationPreferences replaces the whole preference object of
// credentialID and returns it as stored: normalised. It validates first, so a
// value that schema refuses is never written, and writes only for a
// credential that is live: a request that raced a revocation finds none and
// gets ErrNotFound, rather than leaving a row nothing will ever delete. The
// muted ids are not checked against membership (§5.5).
func (s *Store) PutNotificationPreferences(ctx context.Context, credentialID int64, prefs schema.NotificationPreferencesV1, now time.Time) (schema.NotificationPreferencesV1, error) {
	if err := prefs.Validate(); err != nil {
		return schema.NotificationPreferencesV1{}, err
	}
	prefs = NormalizeNotificationPreferences(prefs)
	muted, err := json.Marshal(prefs.Muted)
	if err != nil {
		return schema.NotificationPreferencesV1{}, fmt.Errorf("store: write notification preferences of credential %d: %w", credentialID, err)
	}
	err = s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO notification_preferences (credential_id, policy, preview, quiet_when_unreachable, muted, updated_at)
			 SELECT c.id, ?, ?, ?, ?, ? FROM credentials c WHERE c.id = ? AND c.revoked_at IS NULL
			 ON CONFLICT (credential_id) DO UPDATE SET
			     policy = excluded.policy, preview = excluded.preview,
			     quiet_when_unreachable = excluded.quiet_when_unreachable,
			     muted = excluded.muted, updated_at = excluded.updated_at`,
			string(prefs.Policy), boolInt(prefs.Preview), boolInt(prefs.QuietWhenUnreachable), string(muted), now.UnixMilli(), credentialID)
		if err != nil {
			return fmt.Errorf("store: write notification preferences of credential %d: %w", credentialID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: write notification preferences of credential %d: %w", credentialID, err)
		} else if n != 1 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return schema.NotificationPreferencesV1{}, err
	}
	return prefs, nil
}

// endCredentialNotificationsTx ends everything a credential holds for phone
// notifications, in the caller's transaction: it deletes the notification
// preferences of credentialID (issue #1713) and ends its active push
// registrations with reason credential_revoked, clearing their targets and
// writing one push_registration_ended row each as actor (issue #1714). It is
// called, next to endCredentialSessionsTx, by every site that sets
// credentials.revoked_at, so there is no moment at which the credential is dead
// and its preferences or its registrations are not. If the revocation is
// refused and the transaction rolls back, everything stays. It is the one
// helper for both, so the structural guard over it holds for the registrations
// too.
func endCredentialNotificationsTx(ctx context.Context, tx execer, actor string, credentialID int64, now time.Time) error {
	// An older migration's Go step (the community migration, 40) ends guest
	// visits through the revocation helpers, before the tables exist. There
	// is then nothing to end: no row can predate its table.
	prefs, err := tableExistsTx(ctx, tx, "notification_preferences")
	if err != nil {
		return fmt.Errorf("store: remove notification preferences of credential %d: %w", credentialID, err)
	}
	if prefs {
		if _, err := tx.ExecContext(ctx, "DELETE FROM notification_preferences WHERE credential_id = ?", credentialID); err != nil {
			return fmt.Errorf("store: remove notification preferences of credential %d: %w", credentialID, err)
		}
	}
	push, err := tableExistsTx(ctx, tx, "push_registrations")
	if err != nil {
		return fmt.Errorf("store: end push registrations of credential %d: %w", credentialID, err)
	}
	if push {
		return endCredentialPushTx(ctx, tx, actor, credentialID, now)
	}
	return nil
}

// tableExistsTx reports whether the database has a table called name.
func tableExistsTx(ctx context.Context, tx execer, name string) (bool, error) {
	var exists int
	switch err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// sweepNotificationPreferencesTx deletes the preferences of credentials that
// are no longer usable though nothing revoked them: expired, or idle past
// their bound. resolve.go refuses such a credential, and nothing else would
// ever remove its row. It returns how many it removed.
func sweepNotificationPreferencesTx(ctx context.Context, tx execer, now time.Time) (int, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM notification_preferences WHERE credential_id IN (
		     SELECT id FROM credentials
		     WHERE (expires_at IS NOT NULL AND expires_at <= ?1)
		        OR (idle_ms IS NOT NULL AND COALESCE(last_used_at, created_at) + idle_ms <= ?1))`,
		now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: sweep notification preferences: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sweep notification preferences: %w", err)
	}
	return int(n), nil
}

// notificationText is the text a summary carries under preview (§6.3): runs
// of whitespace become one space, the ends are trimmed, and a body longer
// than schema.MaxNotificationTextChars characters is cut to its first
// MaxNotificationTextChars-1 and an ellipsis, which is the browser's snippet.
// Whitespace is JavaScript's \s, as schema.Mentions has it, so the two
// clients cut the same text.
func notificationText(body string) string {
	text := strings.Join(strings.FieldsFunc(body, isJSSpace), " ")
	if utf8.RuneCountInString(text) <= schema.MaxNotificationTextChars {
		return text
	}
	runes := []rune(text)
	return string(runes[:schema.MaxNotificationTextChars-1]) + "…"
}

// isJSSpace reports whether r is a JavaScript \s code point.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
