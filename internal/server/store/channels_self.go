package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Self-join and leave (issue #982, community model I3b, design §2.6 rows 3
// and 4): a member joins a public channel themselves and leaves any channel
// they are in. Each is one transaction and one function, the place I14a's
// directory-signal hook (issue #988) joins it.

// ErrChannelNotJoinable is a self-join the store refuses whoever asks:
// a caller who is not a member of the community (an agent, a guest, an
// ended guest's identity) or one the ban predicate bars from the channel.
// A private channel the caller is not in is ErrNotFound instead, as for a
// channel that does not exist.
var ErrChannelNotJoinable = errors.New("store: channel not joinable")

// ErrChannelLastMember is a leave by the last member of a private channel:
// nobody could reach the channel again, so the leave is refused.
var ErrChannelLastMember = errors.New("store: last member of a private channel")

// SelfJoin is the result of JoinChannel: the channel, the caller's
// membership in it, and whether this call made it (false: the caller was
// already a member and the membership is returned unchanged).
type SelfJoin struct {
	Channel Channel
	Member  ChannelMember
	Added   bool
}

// selfJoinBarredTx is the moderation hook of self-join (issue #982,
// design §8 "Moderation"): whether principalID is barred from joining
// channelID. Self-join is a membership-creating path, so the shared ban
// predicate of moderation M3 (issue #851) runs here, inside the join's
// transaction. Until M3 lands there is no ban, and nobody is barred.
func selfJoinBarredTx(_ context.Context, _ execer, _, _ int64) (bool, error) {
	return false, nil
}

// JoinChannel joins principalID to channelID themselves (POST
// /v1/channels/{channel}/join): in one IMMEDIATE transaction it refuses a
// caller who is not a community member (ErrChannelNotJoinable), returns an
// existing membership unchanged (the route is idempotent, for a private
// channel the caller is in too), answers ErrNotFound for a channel that
// does not exist or is private, calls the ban predicate's hook
// (ErrChannelNotJoinable), and writes the membership with can_decide = 0,
// manages = 0 and no history bound (ADR-013 O12, decision 4), audited
// member_added with the member themselves as actor.
func (s *Store) JoinChannel(ctx context.Context, channelID, principalID int64, now time.Time) (SelfJoin, error) {
	now = now.Truncate(time.Millisecond)
	var res SelfJoin
	err := s.withImmediateTx(ctx, func(tx execer) error {
		member, err := isMemberTx(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if !member {
			return ErrChannelNotJoinable
		}
		ch, err := scanChannel(tx.QueryRowContext(ctx,
			"SELECT "+channelColumns+" FROM channels WHERE id = ?", channelID).Scan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: read channel %d: %w", channelID, err)
		}
		in, err := isChannelMemberTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		if in {
			m, err := channelMemberTx(ctx, tx, channelID, principalID)
			if err != nil {
				return err
			}
			ch.Manages = m.Manages
			res = SelfJoin{Channel: ch, Member: m}
			return nil
		}
		if ch.Visibility != schema.ChannelPublic {
			return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
		}
		barred, err := selfJoinBarredTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		if barred {
			return ErrChannelNotJoinable
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, manages) VALUES (?, ?, ?, ?, 0, 0)",
			channelID, principalID, principalID, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: join %d to channel %d: %w", principalID, channelID, err)
		}
		if err := appendAuditEventTx(ctx, tx, principalActor(principalID), AuditMemberAdded, channelSubject(channelID), memberDetail(principalID), now); err != nil {
			return err
		}
		m, err := channelMemberTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		res = SelfJoin{Channel: ch, Member: m, Added: true}
		return nil
	})
	if err != nil {
		return SelfJoin{}, err
	}
	return res, nil
}

// LeaveChannel removes principalID from channelID at its own asking (POST
// /v1/channels/{channel}/leave): in one IMMEDIATE transaction it answers
// ErrNotFound when the channel does not exist or the caller is not in it,
// refuses the last member of a private channel (ErrChannelLastMember), and
// otherwise removes the membership, its manager grant and its net seats as
// a removal does, audited member_removed with the member themselves as
// actor. The last member of a public channel may leave, and so may a
// member of a default channel. Pending approvals are untouched: a leaver's
// decisions stop counting toward quorum, which counts only the current
// deciders (store/approvals.go), and an approval that can no longer meet
// it expires at its final deadline. The caller must close the principal's
// live subscriptions after this returns.
func (s *Store) LeaveChannel(ctx context.Context, channelID, principalID int64, now time.Time) error {
	now = now.Truncate(time.Millisecond)
	return s.withImmediateTx(ctx, func(tx execer) error {
		var visibility string
		switch err := tx.QueryRowContext(ctx, "SELECT visibility FROM channels WHERE id = ?", channelID).Scan(&visibility); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: read channel %d: %w", channelID, err)
		}
		in, err := isChannelMemberTx(ctx, tx, channelID, principalID)
		if err != nil {
			return err
		}
		if !in {
			return fmt.Errorf("store: channel %d: %w", channelID, ErrNotFound)
		}
		if schema.ChannelVisibility(visibility) != schema.ChannelPublic {
			var others int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND principal_id != ?",
				channelID, principalID).Scan(&others); err != nil {
				return fmt.Errorf("store: count members of channel %d: %w", channelID, err)
			}
			if others == 0 {
				return ErrChannelLastMember
			}
		}
		_, err = removeChannelMemberTx(ctx, tx, principalActor(principalID), channelID, principalID, now)
		return err
	})
}

// ChannelListing is one row of the channel directory (GET
// /v1/directory/channels): a channel, whether the viewer is in it, and,
// on the scope=all listing only, its member count (Members is -1 on the
// member view, which carries no counts).
type ChannelListing struct {
	Channel
	Joined  bool
	Members int64
}

// ChannelDirectory lists the channel directory for viewerID, ordered by id
// ascending: with all unset, every public channel and the private channels
// the viewer is in, with no counts (design §5, "The channel list"); with all
// set (the ?scope=all listing of a holder of manage_channels, R4), every
// channel with its member count. Joined and Manages are the viewer's own
// membership either way; reading the listing reads no channel's content.
// It returns an empty, non-nil slice when there is nothing to list.
func (s *Store) ChannelDirectory(ctx context.Context, viewerID int64, all bool) ([]ChannelListing, error) {
	query := `SELECT c.id, c.name, c.created_at, c.visibility, c.is_default,
	                 m.principal_id IS NOT NULL, COALESCE(m.manages, 0), -1
	          FROM channels c
	          LEFT JOIN channel_members m ON m.channel_id = c.id AND m.principal_id = ?
	          WHERE c.visibility = 'public' OR m.principal_id IS NOT NULL
	          ORDER BY c.id ASC`
	if all {
		query = `SELECT c.id, c.name, c.created_at, c.visibility, c.is_default,
		                m.principal_id IS NOT NULL, COALESCE(m.manages, 0),
		                (SELECT COUNT(*) FROM channel_members n WHERE n.channel_id = c.id)
		         FROM channels c
		         LEFT JOIN channel_members m ON m.channel_id = c.id AND m.principal_id = ?
		         ORDER BY c.id ASC`
	}
	rows, err := s.db.QueryContext(ctx, query, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: channel directory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	listing := []ChannelListing{}
	for rows.Next() {
		var l ChannelListing
		var joined bool
		var manages int
		ch, err := scanChannel(func(dest ...any) error {
			return rows.Scan(append(dest, &joined, &manages, &l.Members)...)
		})
		if err != nil {
			return nil, fmt.Errorf("store: channel directory: %w", err)
		}
		ch.Manages = joined && manages == 1
		l.Channel, l.Joined = ch, joined
		listing = append(listing, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: channel directory: %w", err)
	}
	return listing, nil
}
