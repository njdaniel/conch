package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message reactions (issue #765, docs/design/message-reactions.md §7). A
// reaction is one durable record (message, principal, emoji). Counts are
// always derived from the records; nothing here increments or caches one.
//
// Every statement that names the messages table is in audience.go, the only
// file that may (TestOnlyAudienceGoReadsMessageRows), and every function here
// goes through it first: the reader's message probe, with the one visibility
// condition and the explicit member-and-enabled clause, is the first thing a
// transaction does after its membership check, and the aggregate that reveals
// anything runs only after the probe found the row, in the same transaction.
// This file holds the statements over message_reactions, the net seat read
// and the audit rows.
//
// Reaction rows outlive the reactor's access (leaving a channel, a net, being
// disabled, hidden, ending a guest visit, conversion): nothing here deletes
// them except the owner's own removal and a moderator's.

// Errors of the reaction functions, besides ErrMessageNotFound (audience.go)
// and ErrNotChannelMember (nets.go).
var (
	// ErrReactionNotPermitted is returned by a set when the reader can see
	// the message but may not react to it: no current member seat on the net
	// it was said on (a monitor, or a principal taken off the net since the
	// post).
	ErrReactionNotPermitted = errors.New("store: not permitted to react to this message")
	// ErrTooManyReactions is returned by a set that would exceed
	// schema.MaxReactionKindsPerMessage or
	// schema.MaxReactionsPerPrincipalPerMessage. Nothing is stored.
	ErrTooManyReactions = errors.New("store: too many reactions")
)

// Audit actions of the reaction functions (§11.3). Each is written in the
// transaction that changed a row, and only then: a no-op writes nothing. The
// subject is "message:<id>" and no detail carries a message body or any free
// text.
const (
	AuditReactionSet       = "reaction_set"
	AuditReactionRemoved   = "reaction_removed"
	AuditReactionModerated = "reaction_moderated"
)

// ReactionCount is one emoji on one message.
type ReactionCount struct {
	// Emoji is the canonical key.
	Emoji string
	// Count is how many records hold it; at least 1.
	Count int
	// Me is whether the reader holds it.
	Me bool
	// First is when the earliest record still held with this key was made,
	// which orders the summary.
	First time.Time
}

// ReactionSummary is the whole reaction state of one message for one reader,
// ordered by First, then by key.
type ReactionSummary struct {
	ChannelID int64
	MessageID int64
	Reactions []ReactionCount
	// AsOf is when the summary was read.
	AsOf time.Time
}

// ReactionChange is a write's answer: the message's summary after the write,
// and whether the write changed a row.
type ReactionChange struct {
	Summary ReactionSummary
	Changed bool
}

// Reaction is one record: who reacted with one emoji, and when. Guest is the
// attribution the reactor had when it reacted, never changed afterwards.
type Reaction struct {
	PrincipalID int64
	Emoji       string
	CreatedAt   time.Time
	Guest       *schema.GuestAttributionV1
}

// ReactorPage is one page of the principals holding one emoji on a message,
// by principal id. NextAfter is the id to ask the next page after, or zero
// when there is no further page.
type ReactorPage struct {
	Reactors  []Reaction
	NextAfter int64
}

// checkKey refuses a key that is not canonical. The handlers canonicalize
// before they call, so this is the store's own line of defence: it never
// writes or reads by a key the catalog does not know.
func checkKey(key string) error {
	return schema.ValidateReactionKey(key)
}

// reactionReaderTx is the first step of every reaction transaction: the
// reader must be a verified, Scoped principal who is, now, a member of the
// channel and enabled; otherwise ErrNotChannelMember, the answer the
// handlers' gate gives, so a removal that races the gate is indistinguishable
// from never having been a member.
func reactionReaderTx(ctx context.Context, tx execer, channelID int64, reader Reader) error {
	if !reader.Scoped || reader.PrincipalID <= 0 {
		return ErrNotChannelMember
	}
	member, err := isChannelMemberTx(ctx, tx, channelID, reader.PrincipalID)
	if err != nil {
		return err
	}
	if !member {
		return ErrNotChannelMember
	}
	var one int
	switch err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM principals WHERE id = ? AND disabled_at IS NULL", reader.PrincipalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotChannelMember
	case err != nil:
		return fmt.Errorf("store: check principal %d is enabled: %w", reader.PrincipalID, err)
	}
	return nil
}

// reactionSummaryTx aggregates the reactions of message messageID for
// principalID. The caller has already probed the message in the same
// transaction.
func reactionSummaryTx(ctx context.Context, tx execer, channelID, messageID, principalID int64) ([]ReactionCount, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT x.emoji, COUNT(*), MIN(x.created_at), COALESCE(SUM(x.principal_id = ?), 0)
		 FROM message_reactions x
		 WHERE x.message_id = ? AND x.channel_id = ?
		 GROUP BY x.emoji
		 ORDER BY MIN(x.created_at) ASC, x.emoji ASC`, principalID, messageID, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: summarize reactions of message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	counts := []ReactionCount{}
	for rows.Next() {
		var c ReactionCount
		var first, mine int64
		if err := rows.Scan(&c.Emoji, &c.Count, &first, &mine); err != nil {
			return nil, fmt.Errorf("store: summarize reactions of message %d: %w", messageID, err)
		}
		c.First = time.UnixMilli(first)
		c.Me = mine > 0
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: summarize reactions of message %d: %w", messageID, err)
	}
	return counts, nil
}

// ReactionSummaryOf returns the reactions of message messageID of channelID as
// reader may see them: ErrMessageNotFound when the message does not exist, is
// in another channel or is not visible to reader, and ErrNotChannelMember when
// reader is not now a member of the channel and enabled. The probe and the
// aggregate run in one read transaction, so they cannot disagree. clock is
// read after the aggregate.
func (s *Store) ReactionSummaryOf(ctx context.Context, channelID int64, reader Reader, messageID int64, clock func() time.Time) (ReactionSummary, error) {
	var sum ReactionSummary
	err := s.readTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		if _, err := reactionTargetTx(ctx, tx, channelID, reader, messageID); err != nil {
			return err
		}
		counts, err := reactionSummaryTx(ctx, tx, channelID, messageID, reader.PrincipalID)
		if err != nil {
			return err
		}
		sum = ReactionSummary{ChannelID: channelID, MessageID: messageID, Reactions: counts, AsOf: clock()}
		return nil
	})
	return sum, err
}

// ReactionSummariesOf is the batch read: one summary for each of ids (distinct
// and positive; the caller checked) that is a message of channelID reader may
// see and that holds at least one reaction, ascending by message id. An id
// that is unknown, invisible or unreacted is simply absent, and the three are
// indistinguishable. One statement probes every id and one aggregates over
// those that passed.
func (s *Store) ReactionSummariesOf(ctx context.Context, channelID int64, reader Reader, ids []int64, clock func() time.Time) ([]ReactionSummary, error) {
	out := []ReactionSummary{}
	err := s.readTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		visible, err := reactableMessagesTx(ctx, tx, channelID, reader, ids)
		if err != nil || len(visible) == 0 {
			return err
		}
		list, err := json.Marshal(visible)
		if err != nil {
			return fmt.Errorf("store: encode message ids: %w", err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT x.message_id, x.emoji, COUNT(*), MIN(x.created_at), COALESCE(SUM(x.principal_id = ?), 0)
			 FROM message_reactions x
			 WHERE x.channel_id = ? AND x.message_id IN (SELECT value FROM json_each(?))
			 GROUP BY x.message_id, x.emoji
			 ORDER BY x.message_id ASC, MIN(x.created_at) ASC, x.emoji ASC`,
			reader.PrincipalID, channelID, string(list))
		if err != nil {
			return fmt.Errorf("store: summarize reactions in channel %d: %w", channelID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, first, mine int64
			var c ReactionCount
			if err := rows.Scan(&id, &c.Emoji, &c.Count, &first, &mine); err != nil {
				return fmt.Errorf("store: summarize reactions in channel %d: %w", channelID, err)
			}
			c.First, c.Me = time.UnixMilli(first), mine > 0
			if n := len(out); n == 0 || out[n-1].MessageID != id {
				out = append(out, ReactionSummary{ChannelID: channelID, MessageID: id, Reactions: []ReactionCount{}})
			}
			out[len(out)-1].Reactions = append(out[len(out)-1].Reactions, c)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: summarize reactions in channel %d: %w", channelID, err)
		}
		at := clock()
		for i := range out {
			out[i].AsOf = at
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListReactorsOf returns one page of the principals holding emoji key on
// message messageID, ascending by principal id, after the principal id after
// (zero for the first page), at most limit of them. The message is probed for
// reader in the same transaction. An emoji nobody holds is an empty page.
func (s *Store) ListReactorsOf(ctx context.Context, channelID int64, reader Reader, messageID int64, key string, after int64, limit int) (ReactorPage, error) {
	if err := checkKey(key); err != nil {
		return ReactorPage{}, err
	}
	if limit <= 0 || after < 0 {
		return ReactorPage{}, errors.New("store: list reactors: limit must be positive and the cursor not negative")
	}
	page := ReactorPage{Reactors: []Reaction{}}
	err := s.readTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		if _, err := reactionTargetTx(ctx, tx, channelID, reader, messageID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT x.principal_id, x.created_at, x.guest_handle, x.guest_label
			 FROM message_reactions x
			 WHERE x.message_id = ? AND x.channel_id = ? AND x.emoji = ? AND x.principal_id > ?
			 ORDER BY x.principal_id ASC LIMIT ?`, messageID, channelID, key, after, limit+1)
		if err != nil {
			return fmt.Errorf("store: list reactors of message %d: %w", messageID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			r := Reaction{Emoji: key}
			var at int64
			var handle, label sql.NullString
			if err := rows.Scan(&r.PrincipalID, &at, &handle, &label); err != nil {
				return fmt.Errorf("store: list reactors of message %d: %w", messageID, err)
			}
			r.CreatedAt = time.UnixMilli(at)
			if handle.Valid {
				a := guestAttribution(handle.String, label.String)
				r.Guest = &a
			}
			page.Reactors = append(page.Reactors, r)
		}
		return rows.Err()
	})
	if err != nil {
		return ReactorPage{}, err
	}
	if len(page.Reactors) > limit {
		page.Reactors = page.Reactors[:limit]
		page.NextAfter = page.Reactors[limit-1].PrincipalID
	}
	return page, nil
}

// readTx runs fn in a read-only transaction: one snapshot for the probe and
// the aggregate that follows it.
func (s *Store) readTx(ctx context.Context, fn func(tx execer) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// reactionWrite is the shell of the three writes: one IMMEDIATE transaction
// that checks the reader is still a member and enabled, probes the message for
// the reader (§6.1), runs write, and reads the summary back for the reader.
// The clock is read once, after the write lock is held, so the as_of of two
// answers orders like their commits. write reports whether it changed a row.
func (s *Store) reactionWrite(ctx context.Context, channelID int64, reader Reader, messageID int64, clock func() time.Time,
	write func(tx execer, target ReactionTarget, at time.Time) (bool, error),
) (ReactionChange, error) {
	var out ReactionChange
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		target, err := reactionTargetTx(ctx, tx, channelID, reader, messageID)
		if err != nil {
			return err
		}
		at := clock().Truncate(time.Millisecond)
		changed, err := write(tx, target, at)
		if err != nil {
			return err
		}
		counts, err := reactionSummaryTx(ctx, tx, channelID, messageID, reader.PrincipalID)
		if err != nil {
			return err
		}
		out = ReactionChange{Changed: changed, Summary: ReactionSummary{ChannelID: channelID, MessageID: messageID, Reactions: counts, AsOf: at}}
		return nil
	})
	if err != nil {
		return ReactionChange{}, err
	}
	return out, nil
}

// SetReaction records that reader holds the reaction key on message messageID
// of channelID, and answers the message's summary for reader. Setting one that
// is already held changes nothing (Changed false). key must be canonical.
//
// Refusals, each storing nothing: ErrNotChannelMember (reader is not now a
// member of the channel, or is disabled), ErrMessageNotFound (the message is
// unknown, in another channel or not visible to reader), ErrReactionNotPermitted
// (a net message and reader has no current member seat on its net: a monitor,
// or a principal taken off the net since the post) and ErrTooManyReactions.
// A reaction on a net message is a small transmission into the net, so it asks
// the seat that posting there would; a whisper's recipients are exactly those
// who can see it, which the probe has decided.
func (s *Store) SetReaction(ctx context.Context, channelID int64, reader Reader, messageID int64, key string, clock func() time.Time) (ReactionChange, error) {
	if err := checkKey(key); err != nil {
		return ReactionChange{}, err
	}
	return s.reactionWrite(ctx, channelID, reader, messageID, clock, func(tx execer, target ReactionTarget, at time.Time) (bool, error) {
		if target.Audience != nil && target.Audience.Kind == schema.AudienceKindNet {
			var role string
			switch err := tx.QueryRowContext(ctx,
				`SELECT nm.role FROM nets n JOIN net_members nm ON nm.net_id = n.id
				 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND nm.principal_id = ?`,
				target.Audience.NetID, channelID, reader.PrincipalID).Scan(&role); {
			case errors.Is(err, sql.ErrNoRows):
				return false, ErrReactionNotPermitted
			case err != nil:
				return false, fmt.Errorf("store: read seat on net %d: %w", target.Audience.NetID, err)
			}
			if schema.NetRole(role) != schema.NetRoleMember {
				return false, ErrReactionNotPermitted
			}
		}
		// A guest's reaction carries who it was while it was a guest, read in
		// this transaction and never changed (the message rule).
		var handle, label sql.NullString
		switch err := tx.QueryRowContext(ctx,
			"SELECT handle, label FROM guest_visits WHERE principal_id = ? AND state = 'active'",
			reader.PrincipalID).Scan(&handle, &label); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return false, fmt.Errorf("store: read guest visit of principal %d: %w", reader.PrincipalID, err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO message_reactions (message_id, emoji, principal_id, channel_id, created_at, guest_handle, guest_label)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (message_id, emoji, principal_id) DO NOTHING`,
			messageID, key, reader.PrincipalID, channelID, at.UnixMilli(), handle, label)
		if err != nil {
			return false, fmt.Errorf("store: set reaction on message %d: %w", messageID, err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("store: set reaction on message %d: %w", messageID, err)
		}
		if inserted == 0 {
			return false, nil
		}
		// The limits are checked on the rows as they now stand, in this
		// transaction: the insert is rolled back by the error. Joining a key
		// already on the message adds no kind, so it is always allowed.
		var kinds, mine int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(DISTINCT emoji) FROM message_reactions WHERE message_id = ?", messageID).Scan(&kinds); err != nil {
			return false, fmt.Errorf("store: count reaction kinds of message %d: %w", messageID, err)
		}
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM message_reactions WHERE message_id = ? AND principal_id = ?",
			messageID, reader.PrincipalID).Scan(&mine); err != nil {
			return false, fmt.Errorf("store: count reactions of principal %d: %w", reader.PrincipalID, err)
		}
		if kinds > schema.MaxReactionKindsPerMessage || mine > schema.MaxReactionsPerPrincipalPerMessage {
			return false, ErrTooManyReactions
		}
		return true, appendAuditEventTx(ctx, tx, principalActor(reader.PrincipalID), AuditReactionSet,
			messageSubject(messageID), fmt.Sprintf("channel=%d emoji=%s", channelID, key), at)
	})
}

// RemoveReaction removes reader's own reaction key from message messageID of
// channelID and answers the message's summary for reader. Removing one that is
// not held changes nothing. It needs no right to transmit: a monitor, or a
// principal taken off the net since the post, may take back a mark it made
// (the message must still be visible to it). The refusals are SetReaction's
// except ErrReactionNotPermitted and ErrTooManyReactions.
func (s *Store) RemoveReaction(ctx context.Context, channelID int64, reader Reader, messageID int64, key string, clock func() time.Time) (ReactionChange, error) {
	if err := checkKey(key); err != nil {
		return ReactionChange{}, err
	}
	return s.reactionWrite(ctx, channelID, reader, messageID, clock, func(tx execer, _ ReactionTarget, at time.Time) (bool, error) {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM message_reactions WHERE message_id = ? AND emoji = ? AND principal_id = ? AND channel_id = ?",
			messageID, key, reader.PrincipalID, channelID)
		if err != nil {
			return false, fmt.Errorf("store: remove reaction from message %d: %w", messageID, err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("store: remove reaction from message %d: %w", messageID, err)
		}
		if removed == 0 {
			return false, nil
		}
		return true, appendAuditEventTx(ctx, tx, principalActor(reader.PrincipalID), AuditReactionRemoved,
			messageSubject(messageID), fmt.Sprintf("channel=%d emoji=%s", channelID, key), at)
	})
}

// ModerateReactions is a moderator's removal (§11.5): it deletes target's
// reaction key from message messageID of channelID, or, when key is empty,
// every reaction of target on it, and answers the message's summary for the
// moderator. reader is the moderator, who must be a current member who can see
// the message, operators included; that the moderator is an operator or a
// manager of the channel is the caller's (the route class). target is any
// principal id: nothing is looked up about it, so an unknown or absent one
// removes nothing, and it is not an account action, so no membership,
// credential or role changes. One audit row, reaction_moderated, is written
// only when rows were removed, naming the moderator, the target and how many.
func (s *Store) ModerateReactions(ctx context.Context, channelID int64, reader Reader, messageID, targetID int64, key string, clock func() time.Time) (ReactionChange, error) {
	if key != "" {
		if err := checkKey(key); err != nil {
			return ReactionChange{}, err
		}
	}
	if targetID <= 0 {
		return ReactionChange{}, errors.New("store: moderate reactions: a target principal is required")
	}
	return s.reactionWrite(ctx, channelID, reader, messageID, clock, func(tx execer, _ ReactionTarget, at time.Time) (bool, error) {
		query := "DELETE FROM message_reactions WHERE message_id = ? AND channel_id = ? AND principal_id = ?"
		args := []any{messageID, channelID, targetID}
		detail := fmt.Sprintf("channel=%d principal=%d", channelID, targetID)
		if key != "" {
			query += " AND emoji = ?"
			args = append(args, key)
			detail += " emoji=" + key
		} else {
			detail += " all"
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return false, fmt.Errorf("store: moderate reactions on message %d: %w", messageID, err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("store: moderate reactions on message %d: %w", messageID, err)
		}
		if removed == 0 {
			return false, nil
		}
		return true, appendAuditEventTx(ctx, tx, principalActor(reader.PrincipalID), AuditReactionModerated,
			messageSubject(messageID), fmt.Sprintf("%s removed=%d", detail, removed), at)
	})
}

func messageSubject(id int64) string { return fmt.Sprintf("message:%d", id) }

// CountReactions returns how many reaction rows the database holds. It exists
// for tests, which assert that a refused write stored nothing; it must never
// be served to a caller.
func (s *Store) CountReactions(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_reactions").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count reactions: %w", err)
	}
	return n, nil
}
