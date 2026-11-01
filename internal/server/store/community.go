package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community model's store (ADR-013, docs/design/community-model.md,
// issue #979). Migration 40 converts an existing server: its open channels
// become public default channels, every other channel stays private with its
// members (O6), every member is put in every default channel with no history
// bound and no right to decide, voice and link-making are on for every
// member, the joining settings move to open_signup, and whatever guest state
// #1005 left running is ended.

// communityMigration is the migration that converts a server to the
// community model (design §6). It was 37 when the design was written; the
// reactions, moderation, edits and permissions migrations took the numbers
// before it, so it is the next free one at dispatch.
const communityMigration = 40

// AuditCommunityMigrated is the one audit row migration 40 writes for itself
// (design §6 step 10): actor "migration", subject "server", detail the counts
// of what steps 3 to 9 changed. Step 4's rows (End visit's and an invite
// close's own) are written as those paths write them.
const AuditCommunityMigrated = "community_migrated"

// InviteCloseGuestsRemoved is the reason= of the invite_closed row for an
// invite that admitted guests, closed by migration 40 because guests are
// removed for now (design §0.1). It is a store-side reason in the audit
// detail, not a wire value.
const InviteCloseGuestsRemoved = "guests_removed"

// migrationActor is the actor of the rows migration 40 writes.
const migrationActor = "migration"

// The server_settings keys of the community model's joining settings (design
// §3.4, Option B). open_joining and join_voice are deleted by migration 40;
// the open-channel set is channels.is_default.
const (
	settingOpenSignup                     = "open_signup"
	settingMemberLinkDefaultExpirySeconds = "member_link_default_expiry_seconds"
)

// memberPopulation selects §6's population P: every human that is not hidden
// and has no visit active or ended. It is isMemberTx's predicate without the
// enabled test, so a disabled human is converted too and enabling it later
// needs nothing (design §6). Converted guests are in P.
const memberPopulation = `SELECT p.id FROM principals p
	WHERE p.kind = 'human' AND p.hidden_at IS NULL
	  AND NOT EXISTS (SELECT 1 FROM guest_visits g WHERE g.principal_id = p.id AND g.state IN ('active', 'ended'))`

// isMemberTx reports whether principalID is a member of the community
// (design §1.1): a human, enabled, not hidden, with no guest visit active or
// ended. A converted guest is a member; an ended guest is a dead identity,
// never one, as guestTarget already treats it. It is the one store
// predicate for "a member": who may make links, who is in the directory,
// whom a manager may add (I3a to I14a use it). An unknown principal is not
// a member.
func isMemberTx(ctx context.Context, tx execer, principalID int64) (bool, error) {
	var member bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM principals p
			WHERE p.id = ? AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM guest_visits g WHERE g.principal_id = p.id AND g.state IN ('active', 'ended')))`,
		principalID).Scan(&member)
	if err != nil {
		return false, fmt.Errorf("store: is member: %w", err)
	}
	return member, nil
}

// communityCounts are what migration 40's preflight reads and what its
// steps change, logged before and after and kept in its audit row.
type communityCounts struct {
	// Preflight.
	channels, members, boundedMemberships, openInvites, guestInvites, activeVisits int64
	guestsEnabled                                                                  bool
	openJoining                                                                    string
	invitesByKind                                                                  string
	// Steps.
	publicDefaults, visitsEnded, invitesClosed, boundsCleared, membershipsAdded, principalsFlipped, invitesLegacy int64
	openSignup                                                                                                    bool
}

// migrateToCommunity is migration 40's Go step, run in the migration's
// transaction after its DDL (the two channel columns and their triggers, the
// lister column, legacy_history_bounds and invites.link). It runs §6's
// preflight and steps 3 to 11 in order, in this one transaction.
//
// A new database, with no principal, no audit row and no setting, is left as
// its DDL made it, as the permissions migration does: there is nothing to
// convert, the readers' defaults apply (open_signup off, the member link
// expiry 604800), and the trail of a new server starts with what its people
// do rather than with a migration row.
func migrateToCommunity(ctx context.Context, tx *sql.Tx) error {
	var history bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM principals) OR EXISTS (SELECT 1 FROM audit_events)
		     OR EXISTS (SELECT 1 FROM server_settings)`).Scan(&history); err != nil {
		return fmt.Errorf("read whether the database is new: %w", err)
	}
	if !history {
		return nil
	}
	now := credentialNow().Truncate(time.Millisecond)
	ms := now.UnixMilli()
	c, err := communityPreflight(ctx, tx, ms)
	if err != nil {
		return err
	}
	slog.Info("store: converting to the community model (migration 40)",
		"channels", c.channels, "members", c.members, "bounded_memberships", c.boundedMemberships,
		"open_invites", c.openInvites, "open_invites_by_kind", c.invitesByKind,
		"open_guest_invites", c.guestInvites, "active_guest_visits", c.activeVisits,
		"guests_enabled", c.guestsEnabled, "open_joining", c.openJoining)
	if c.guestsEnabled {
		slog.Info("store: guests_enabled is still on; the migration turns it off")
	}

	// Step 3: the open channels become public default channels, each keeping
	// who listed it (the lister rule of ADR-014 §7.3 reads it); every other
	// channel stays private with its members (O6).
	res, err := tx.ExecContext(ctx,
		`UPDATE channels SET visibility = 'public', is_default = 1,
			default_listed_by = (SELECT o.added_by FROM open_channels o WHERE o.channel_id = channels.id)
		 WHERE id IN (SELECT channel_id FROM open_channels)`)
	if err != nil {
		return fmt.Errorf("step 3: mark the default channels: %w", err)
	}
	c.publicDefaults = affected(res)

	// Step 4: no guests (§0.1). Every visit still active is ended through End
	// visit, audited as it audits; every open invite that admits guests is
	// closed. No guest row is deleted.
	if c.visitsEnded, err = endActiveVisitsTx(ctx, tx, now); err != nil {
		return fmt.Errorf("step 4: end the active visits: %w", err)
	}
	if c.invitesClosed, err = closeGuestInvitesTx(ctx, tx, now); err != nil {
		return fmt.Errorf("step 4: close the guest invites: %w", err)
	}

	// Step 5: the history bounds of P are kept in legacy_history_bounds,
	// which nothing reads, and cleared (decision 4). Guest bounds stay.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO legacy_history_bounds (channel_id, principal_id, history_after, cleared_at)
		 SELECT channel_id, principal_id, history_after, ? FROM channel_members
		 WHERE history_after IS NOT NULL AND principal_id IN (`+memberPopulation+`)`, ms); err != nil {
		return fmt.Errorf("step 5: keep the bounds: %w", err)
	}
	if res, err = tx.ExecContext(ctx,
		`UPDATE channel_members SET history_after = NULL
		 WHERE history_after IS NOT NULL AND principal_id IN (`+memberPopulation+`)`); err != nil {
		return fmt.Errorf("step 5: clear the bounds: %w", err)
	}
	c.boundsCleared = affected(res)

	// Step 6: every member of P is in every default channel, with no right to
	// decide and no bound; a row already there is left as it is (O12).
	if res, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, history_after)
		 SELECT c.id, p.id, NULL, ?, 0, NULL FROM channels c CROSS JOIN (`+memberPopulation+`) p
		 WHERE c.is_default = 1`, ms); err != nil {
		return fmt.Errorf("step 6: join the default channels: %w", err)
	}
	c.membershipsAdded = affected(res)

	// Step 7: voice and link-making are on for every member of P, disabled
	// humans included (decisions 1 and 5).
	if res, err = tx.ExecContext(ctx,
		`UPDATE principals SET voice_allowed = 1, may_invite = 1
		 WHERE id IN (`+memberPopulation+`) AND (voice_allowed <> 1 OR may_invite <> 1)`); err != nil {
		return fmt.Errorf("step 7: turn voice and links on: %w", err)
	}
	c.principalsFlipped = affected(res)

	// Step 8's column is in the DDL: every existing invite reads 'legacy'.
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM invites WHERE link = 'legacy'").Scan(&c.invitesLegacy); err != nil {
		return fmt.Errorf("step 8: count the legacy invites: %w", err)
	}

	// Step 9: the joining settings (Option B). open_signup follows the old
	// mode (account and both admitted accounts; guest and off did not), the
	// member link expiry takes its default (O3), guests are off, and the
	// open_joining and join_voice rows go. The guest bounds stay.
	c.openSignup = c.openJoining == string(schema.OpenJoiningAccount) || c.openJoining == string(schema.OpenJoiningBoth)
	for _, kv := range []struct{ k, v string }{
		{settingOpenSignup, strconv.FormatBool(c.openSignup)},
		{settingMemberLinkDefaultExpirySeconds, strconv.FormatInt(schema.DefaultMemberLinkExpirySeconds, 10)},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, kv.k, kv.v, ms); err != nil {
			return fmt.Errorf("step 9: write %s: %w", kv.k, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE server_settings SET value = 'false', updated_at = ? WHERE key = ? AND value <> 'false'", ms, settingGuestsEnabled); err != nil {
		return fmt.Errorf("step 9: turn guests off: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM server_settings WHERE key IN (?, ?)", settingOpenJoining, settingJoinVoice); err != nil {
		return fmt.Errorf("step 9: retire open_joining and join_voice: %w", err)
	}

	// Step 10: one row for the migration itself.
	detail := fmt.Sprintf("public_defaults=%d visits_ended=%d invites_closed=%d bounds_cleared=%d memberships_added=%d principals_flipped=%d invites_legacy=%d open_signup=%t guests_enabled=false",
		c.publicDefaults, c.visitsEnded, c.invitesClosed, c.boundsCleared, c.membershipsAdded, c.principalsFlipped, c.invitesLegacy, c.openSignup)
	if err := appendAuditEventTx(ctx, tx, migrationActor, AuditCommunityMigrated, "server", detail, now); err != nil {
		return err
	}

	// Step 11: the log.
	slog.Info("store: converted to the community model (migration 40)",
		"public_default_channels", c.publicDefaults, "guest_visits_ended", c.visitsEnded,
		"guest_invites_closed", c.invitesClosed, "history_bounds_cleared", c.boundsCleared,
		"default_memberships_added", c.membershipsAdded, "principals_voice_and_links_on", c.principalsFlipped,
		"legacy_invites", c.invitesLegacy, "open_signup", c.openSignup)
	switch schema.OpenJoiningMode(c.openJoining) {
	case schema.OpenJoiningGuest:
		slog.Warn("store: the bare address admitted guests only (open_joining=guest); it now admits nobody (open_signup off)")
	case schema.OpenJoiningBoth:
		slog.Warn("store: the bare address admitted guests and accounts (open_joining=both); it now makes accounts only (open_signup on)")
	}
	if c.publicDefaults == 0 {
		slog.Warn("store: no channel was open, so there is no default channel; a new member would be in nothing until an operator marks one")
	}
	return nil
}

// communityPreflight reads what §6's preflight logs. It blocks nothing.
func communityPreflight(ctx context.Context, tx *sql.Tx, ms int64) (communityCounts, error) {
	var c communityCounts
	open := `closed_at IS NULL AND expires_at > ? AND (max_uses IS NULL OR uses < max_uses)`
	for _, q := range []struct {
		dst  *int64
		sql  string
		args []any
	}{
		{&c.channels, "SELECT COUNT(*) FROM channels", nil},
		{&c.members, "SELECT COUNT(*) FROM (" + memberPopulation + ")", nil},
		{&c.boundedMemberships, "SELECT COUNT(*) FROM channel_members WHERE history_after IS NOT NULL", nil},
		{&c.openInvites, "SELECT COUNT(*) FROM invites WHERE " + open, []any{ms}},
		{&c.guestInvites, "SELECT COUNT(*) FROM invites WHERE join_as IN ('guest', 'both') AND " + open, []any{ms}},
		{&c.activeVisits, "SELECT COUNT(*) FROM guest_visits WHERE state = 'active'", nil},
	} {
		if err := tx.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			return c, fmt.Errorf("preflight: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT kind, join_as, COUNT(*) FROM invites WHERE "+open+" GROUP BY kind, join_as ORDER BY kind, join_as", ms)
	if err != nil {
		return c, fmt.Errorf("preflight: invites by kind: %w", err)
	}
	var by []string
	for rows.Next() {
		var kind, joinAs string
		var n int64
		if err := rows.Scan(&kind, &joinAs, &n); err != nil {
			_ = rows.Close()
			return c, fmt.Errorf("preflight: invites by kind: %w", err)
		}
		by = append(by, fmt.Sprintf("%s/%s=%d", kind, joinAs, n))
	}
	if err := rows.Close(); err != nil {
		return c, fmt.Errorf("preflight: invites by kind: %w", err)
	}
	c.invitesByKind = strings.Join(by, ",")
	c.openJoining = string(schema.DefaultJoinSettings().OpenJoining)
	for _, k := range []string{settingOpenJoining, settingGuestsEnabled} {
		var v string
		switch err := tx.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", k).Scan(&v); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return c, fmt.Errorf("preflight: read %s: %w", k, err)
		case k == settingOpenJoining:
			if schema.OpenJoiningMode(v).Valid() {
				c.openJoining = v
			}
		default:
			b, perr := strconv.ParseBool(v)
			c.guestsEnabled = perr == nil && b
		}
	}
	return c, nil
}

// endActiveVisitsTx ends every guest visit still active through End visit
// (endGuestVisitTx: guest_ended, each membership removed, the credential
// revoked, all audited), reason operator, actor migration. Its guest_visits
// and message_guest_attribution rows are kept. The migration runs before the
// server takes traffic, as a restore's ending does, so nothing is announced.
func endActiveVisitsTx(ctx context.Context, tx execer, now time.Time) (int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT principal_id FROM guest_visits WHERE state = 'active' ORDER BY principal_id")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var runs []EndedRun
	for _, id := range ids {
		if _, err := endGuestVisitTx(ctx, tx, migrationActor, id, schema.GuestEndOperator, now, &runs); err != nil {
			return 0, fmt.Errorf("principal %d: %w", id, err)
		}
	}
	return int64(len(ids)), nil
}

// closeGuestInvitesTx closes every open invite that admits guests
// (join_as guest or both), whatever channels it names, one invite_closed row
// each with reason guests_removed. Its rows are kept.
func closeGuestInvitesTx(ctx context.Context, tx execer, now time.Time) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, uses FROM invites
		 WHERE join_as IN ('guest', 'both') AND closed_at IS NULL AND expires_at > ? AND (max_uses IS NULL OR uses < max_uses)
		 ORDER BY id`, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	type open struct {
		id   int64
		kind schema.InviteKind
		uses int64
	}
	var found []open
	for rows.Next() {
		var o open
		var k string
		if err := rows.Scan(&o.id, &k, &o.uses); err != nil {
			_ = rows.Close()
			return 0, err
		}
		o.kind = schema.InviteKind(k)
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, o := range found {
		if _, err := tx.ExecContext(ctx, "UPDATE invites SET closed_at = ? WHERE id = ? AND closed_at IS NULL", now.UnixMilli(), o.id); err != nil {
			return 0, fmt.Errorf("invite %d: %w", o.id, err)
		}
		if err := appendAuditEventTx(ctx, tx, migrationActor, AuditInviteClosed, inviteSubject(o.id),
			inviteClosedDetail(o.id, o.kind, o.uses, InviteCloseGuestsRemoved), now); err != nil {
			return 0, err
		}
	}
	return int64(len(found)), nil
}

// affected is how many rows a step changed, for its count. A driver that
// cannot say is logged and counted as 0: the count is a report, and the
// step's own error decides whether the migration goes on.
func affected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		slog.Warn("store: migration 40 could not count the rows a step changed", "error", err)
		return 0
	}
	return n
}

// setDefaultChannelTx marks channelID a default channel, listed by
// listedBy (0 for none), or unmarks it. A default channel is always public
// (ADR-013 decision 5): the store makes it so in the same statement, and the
// triggers of migration 40 refuse any row that breaks the rule. Under
// Option B (design §3.4) the joining settings' open_channels is the set of
// default channels; until channel settings ship (I3a) listing a channel
// there is how it becomes public, as migration 40 made every open channel
// public. Unmarking keeps the channel's visibility. It reports whether
// marking it made a private channel public, which the caller audits.
func setDefaultChannelTx(ctx context.Context, tx execer, channelID int64, isDefault bool, listedBy int64) (madePublic bool, err error) {
	if isDefault {
		var was string
		if err := tx.QueryRowContext(ctx, "SELECT visibility FROM channels WHERE id = ?", channelID).Scan(&was); err != nil {
			return false, fmt.Errorf("store: set default channel %d: %w", channelID, err)
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE channels SET visibility = 'public', is_default = 1, default_listed_by = ? WHERE id = ?",
			nullableID(listedBy), channelID)
		madePublic = was != string(schema.ChannelPublic)
	} else {
		_, err = tx.ExecContext(ctx,
			"UPDATE channels SET is_default = 0, default_listed_by = NULL WHERE id = ?", channelID)
	}
	if err != nil {
		return false, fmt.Errorf("store: set default channel %d: %w", channelID, err)
	}
	return madePublic, nil
}
