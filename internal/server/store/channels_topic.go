package store

import (
	"context"
	"fmt"
	"time"
)

// Channel topics (issue #1503, docs/design/channel-topic.md): one optional
// line that says what a channel is for, set by its managers.

// AuditChannelTopicChanged is the audit action of a topic change: actor the
// caller, subject the channel, detail from=<old> to=<new>, each quoted.
const AuditChannelTopicChanged = "channel_topic_changed"

// channelTopicMigration is the number of the migration that adds
// channels.topic.
const channelTopicMigration = 47

// ChannelTopicResult is what a topic change did.
type ChannelTopicResult struct {
	// Channel is the channel as it now is.
	Channel Channel
	// From is the topic before. Changed is false when the new topic was the
	// same string: nothing was written and nothing audited.
	From    string
	Changed bool
}

// SetChannelTopic sets channel id's topic, "" clearing it, in one IMMEDIATE
// transaction, deciding in this order: an unknown channel is ErrNotFound; a
// callerID that is not a channel manager now (the standing rule of a rename:
// an owner, an operator holding manage_channels, an agent granted it, or a
// member holding manages) is ErrNotFound for a non-member and
// ErrManagerRefused otherwise, so a grant revoked after the route's check is
// refused (ADR-010 decision 7); a topic that is already the channel's is no
// change (nothing written, nothing audited). Otherwise the topic is written
// and a channel_topic_changed row records both, quoted. callerID 0 is the
// system, or --auth off, and skips the standing check. An archived channel
// takes the write. topic must already be valid
// (schema.SetChannelTopicRequestV1.Validate); the store does not check.
func (s *Store) SetChannelTopic(ctx context.Context, actor string, callerID, id int64, topic string, now time.Time) (ChannelTopicResult, error) {
	now = now.Truncate(time.Millisecond)
	var out ChannelTopicResult
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out = ChannelTopicResult{}
		ch, err := channelByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if callerID != 0 {
			if _, err := channelRenameStandingTx(ctx, tx, id, callerID); err != nil {
				return err
			}
		}
		out.Channel, out.From = ch, ch.Topic
		if ch.Topic == topic {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE channels SET topic = ? WHERE id = ?", topic, id); err != nil {
			return fmt.Errorf("store: set topic of channel %d: %w", id, err)
		}
		// Quoted, as a rename's names are, so a topic with spaces or "to="
		// in it reads as one field and an invisible character shows as an
		// escape.
		detail := fmt.Sprintf("from=%q to=%q", ch.Topic, topic)
		if err := appendAuditEventTx(ctx, tx, actor, AuditChannelTopicChanged, channelSubject(id), detail, now); err != nil {
			return err
		}
		out.Channel.Topic = topic
		out.Changed = true
		return nil
	})
	if err != nil {
		return ChannelTopicResult{}, err
	}
	return out, nil
}
