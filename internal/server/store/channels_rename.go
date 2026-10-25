package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
)

// Channel names and renames (issue #727, docs/design/channel-rename.md). A
// channel's name is unique by its keys, as a principal's is (rename.go): two
// names that read the same are one name, for creation and rename alike. The
// lookup by name stays by exact spelling. Every write that sets a channel
// name goes through claimChannelNameTx inside its own IMMEDIATE transaction,
// so two writers cannot both take one key. Neither the hidden- nor the guest-
// prefix is reserved for channels: those are rules about people.

// AuditChannelRenamed is the audit action of a channel rename: actor the
// caller, subject the channel, detail from=<old> to=<new>, each quoted.
const AuditChannelRenamed = "channel_renamed"

// AuditChannelRenameRefused counts a manager's refused rename attempt. Its
// detail deliberately carries neither the requested nor the existing name.
const AuditChannelRenameRefused = "channel_rename_refused"

const channelRenameLimitedDetail = " limited=1"

// ErrChannelNameStale is returned by RenameChannel when the name the caller
// believed the channel had is not its name now: another rename came first.
var ErrChannelNameStale = errors.New("store: the channel is no longer called that")

// channelNameKeyMigration is the number of the migration that adds name keys
// to channels: its Go step is fillChannelNameKeys.
const channelNameKeyMigration = 33

// claimChannelNameTx is the one clash check of every write that sets a
// channel's name: it returns name's keys, ErrNameInvalid when they are empty,
// or ErrDuplicate when a channel other than self holds either of them. self
// is the channel being renamed, or 0 on create. It must run in the
// transaction that writes the name, and the keys it returns are the ones that
// row must store.
func claimChannelNameTx(ctx context.Context, tx execer, name string, self int64) (names.Keys, error) {
	keys := names.KeysOf(name)
	if keys.Empty() {
		return names.Keys{}, ErrNameInvalid
	}
	return keys, keyClashTx(ctx, tx, channelClashQuery, name, keys, self)
}

// insertChannelTx creates a channel named name, after the clash check, and
// returns its id.
func insertChannelTx(ctx context.Context, tx execer, name string, now time.Time) (int64, error) {
	keys, err := claimChannelNameTx(ctx, tx, name, 0)
	if err != nil {
		return 0, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO channels (name, name_key, name_skel, created_at) VALUES (?, ?, ?, ?)",
		name, keys.Folded, keys.Skeletal, now.UnixMilli())
	if isUniqueConstraintErr(err) {
		// The exact name is held by a channel whose stored keys differ from
		// its keys now: keys computed from other Unicode tables.
		return 0, fmt.Errorf("store: create channel %q: %w", name, ErrDuplicate)
	}
	if err != nil {
		return 0, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	return id, nil
}

// ChannelRenameResult is what a channel rename did.
type ChannelRenameResult struct {
	Channel Channel
	// From is the name before. Changed is false when the new name was the
	// same string: nothing was written and nothing audited.
	From    string
	Changed bool
}

// RenameChannel sets channel id's name, in one IMMEDIATE transaction, deciding
// in this order: an unknown channel is ErrNotFound; a name that is already
// the channel's, the same string, is no rename (nothing written, nothing
// audited, and from not compared, so a retry of a rename that happened is
// answered the same); a from that is not the channel's name now is
// ErrChannelNameStale; a name with empty keys is ErrNameInvalid; a name
// another channel holds either key of is ErrDuplicate (the channel's own old
// keys do not count, so "ops" may become "Ops"). Otherwise the name and its
// keys are written and a channel_renamed row records both names. name must
// already be a valid name (schema.RenameChannelRequestV1.Validate); the
// store does not check.
//
// ChannelRenameOptions carries an authenticated caller and the limits to apply
// if the write transaction finds they are currently a channel manager. The
// zero value is an unconstrained AuthOff rename.
type ChannelRenameOptions struct {
	CallerID int64
	Limits   RenameLimits
}

// Every table that refers to a channel does so by id, so messages, members,
// nets, invites, read marks, files, runs and the voice room stay with it.
// Current standing, limits and audit marking are decided in the transaction,
// so a role or grant change cannot race them.
func (s *Store) RenameChannel(ctx context.Context, actor string, id int64, from, name string, now time.Time, options ...ChannelRenameOptions) (ChannelRenameResult, error) {
	now = now.Truncate(time.Millisecond)
	var opt ChannelRenameOptions
	if len(options) > 0 {
		opt = options[0]
	}
	var out ChannelRenameResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out = ChannelRenameResult{}
		ch, err := channelByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		limitedManager := false
		if opt.CallerID != 0 {
			standing, err := channelRenameStandingTx(ctx, tx, id, opt.CallerID)
			if err != nil {
				return err
			}
			limitedManager = standing == standingManager
		}
		out.Channel, out.From = ch, ch.Name
		if ch.Name == name {
			return nil
		}
		if ch.Name != from {
			return fmt.Errorf("store: rename channel %d: %w", id, ErrChannelNameStale)
		}
		if limitedManager {
			if err := channelRenameLimitTx(ctx, tx, principalActor(opt.CallerID), opt.Limits, now); err != nil {
				return err
			}
		}
		keys, err := claimChannelNameTx(ctx, tx, name, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE channels SET name = ?, name_key = ?, name_skel = ? WHERE id = ?",
			name, keys.Folded, keys.Skeletal, id)
		if isUniqueConstraintErr(err) {
			// As on create: the exact name, under keys from other tables.
			return fmt.Errorf("store: rename channel %d: %w", id, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: rename channel %d: %w", id, err)
		}
		// Quoted, as a principal rename's names are, so a name with spaces
		// or "to=" in it reads as one field and an invisible character
		// shows as an escape. The marker keeps unlimited operator renames
		// out of that person's later manager limits.
		detail := fmt.Sprintf("from=%q to=%q", ch.Name, name)
		if limitedManager {
			detail += channelRenameLimitedDetail
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditChannelRenamed, channelSubject(id), detail, now); err != nil {
			return err
		}
		out.Channel.Name = name
		out.Changed = true
		return nil
	})
	if err != nil {
		return ChannelRenameResult{}, err
	}
	return out, nil
}

func channelRenameStandingTx(ctx context.Context, tx execer, channelID, callerID int64) (memberActorStanding, error) {
	caller, found, err := queryPrincipal(ctx, tx, callerID)
	if err != nil {
		return 0, err
	}
	if !found || caller.DisabledAt != nil || caller.HiddenAt != nil || caller.Kind != PrincipalHuman || caller.guestTarget() {
		return 0, ErrManagerRefused
	}
	if caller.Role == RoleOperator {
		if caller.Owner {
			return standingOwner, nil
		}
		return standingOperator, nil
	}
	if caller.Role != RoleMember {
		return 0, ErrManagerRefused
	}
	var manages int
	switch err := tx.QueryRowContext(ctx, "SELECT manages FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, callerID).Scan(&manages); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNotFound
	case err != nil:
		return 0, fmt.Errorf("store: check channel rename standing of %d in channel %d: %w", callerID, channelID, err)
	}
	if manages != 1 {
		return 0, ErrManagerRefused
	}
	return standingManager, nil
}

func channelRenameLimitTx(ctx context.Context, tx execer, actor string, lim RenameLimits, now time.Time) error {
	var until time.Time
	for _, c := range []struct {
		limit   int
		attempt bool
	}{
		{lim.Renames, false},
		{lim.Attempts, true},
	} {
		if c.limit <= 0 {
			continue
		}
		where := `FROM audit_events WHERE actor = ? AND created_at > ? AND action = ? AND detail LIKE '% limited=1'`
		args := []any{actor, now.Add(-RenameWindow).UnixMilli(), AuditChannelRenamed}
		if c.attempt {
			where = `FROM audit_events WHERE actor = ? AND created_at > ? AND (action = ? OR (action = ? AND detail LIKE '% limited=1'))`
			args = []any{actor, now.Add(-RenameWindow).UnixMilli(), AuditChannelRenameRefused, AuditChannelRenamed}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+where, args...).Scan(&count); err != nil {
			return fmt.Errorf("store: count channel renames of %s: %w", actor, err)
		}
		if count < c.limit {
			continue
		}
		var freeing int64
		if err := tx.QueryRowContext(ctx, `SELECT created_at `+where+` ORDER BY created_at, id LIMIT 1 OFFSET ?`, append(args, count-c.limit)...).Scan(&freeing); err != nil {
			return fmt.Errorf("store: count channel renames of %s: %w", actor, err)
		}
		if free := time.UnixMilli(freeing).Add(RenameWindow); free.After(until) {
			until = free
		}
	}
	if until.IsZero() {
		return nil
	}
	return &RenameLimitedError{Until: until}
}

// RecordChannelRenameRefused atomically re-checks a manager's limits and
// records a refused attempt without either channel name.
func (s *Store) RecordChannelRenameRefused(ctx context.Context, callerID, channelID int64, lim RenameLimits, now time.Time) error {
	if callerID == 0 {
		return nil
	}
	now = now.Truncate(time.Millisecond)
	return s.withImmediateTx(ctx, func(tx execer) error {
		standing, err := channelRenameStandingTx(ctx, tx, channelID, callerID)
		if err != nil {
			return err
		}
		if standing != standingManager {
			return nil
		}
		if err := channelRenameLimitTx(ctx, tx, principalActor(callerID), lim, now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, principalActor(callerID), AuditChannelRenameRefused, channelSubject(channelID), "", now)
	})
}

// channelByIDTx is ChannelByID inside a transaction.
func channelByIDTx(ctx context.Context, tx execer, id int64) (Channel, error) {
	var ch Channel
	var createdAt int64
	err := tx.QueryRowContext(ctx, "SELECT id, name, created_at FROM channels WHERE id = ?", id).
		Scan(&ch.ID, &ch.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, fmt.Errorf("store: find channel %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: find channel %d: %w", id, err)
	}
	ch.CreatedAt = time.UnixMilli(createdAt)
	return ch, nil
}

// fillChannelNameKeys is the channel name-key migration's Go step: it
// computes both keys for every existing channel, then logs the groups of
// channels whose names clash and the channels whose names have empty keys.
// All are kept as they are: none is renamed, and a later rename of any of
// them must still not take another's key. It writes no audit row.
func fillChannelNameKeys(ctx context.Context, tx *sql.Tx) error {
	all, err := readNamedRows(ctx, tx, "SELECT id, name FROM channels ORDER BY id")
	if err != nil {
		return fmt.Errorf("read channel names: %w", err)
	}
	for _, r := range all {
		keys := names.KeysOf(r.name)
		if _, err := tx.ExecContext(ctx, "UPDATE channels SET name_key = ?, name_skel = ? WHERE id = ?",
			keys.Folded, keys.Skeletal, r.id); err != nil {
			return fmt.Errorf("store name keys of channel %d: %w", r.id, err)
		}
	}
	clashing, blank := nameKeyGroups(all)
	// Ids only, as for principals.
	if len(clashing) > 0 {
		slog.WarnContext(ctx, "store: some existing channel names clash by case or look-alike characters; they are kept as they are, and a rename of any of them to a name that clashes with another is refused",
			"groups", len(clashing), "channels", clashing)
	}
	if len(blank) > 0 {
		slog.WarnContext(ctx, "store: some existing channel names have no character a reader can see; they are kept as they are, and an operator should rename them",
			"channels", blank)
	}
	return nil
}
