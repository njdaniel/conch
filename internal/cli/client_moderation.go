package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

func moderationTarget(channel string, id int64) (string, error) {
	if channel == "" || id <= 0 {
		return "", errors.New("cli: moderation needs a channel and a positive id")
	}
	return strconv.FormatInt(id, 10), nil
}

// KickVoiceParticipant removes a principal from the channel's current voice
// rooms. It does not change membership and the principal may rejoin.
func (c *Client) KickVoiceParticipant(ctx context.Context, channel string, principalID int64) error {
	id, err := moderationTarget(channel, principalID)
	if err != nil {
		return err
	}
	endpoint := c.resolve("v1", "channels", channel, "voice", "participants", id)
	return c.roundTrip(ctx, "remove voice participant", http.MethodDelete, endpoint, nil, nil)
}

// ListChannelBans returns the channel's principal-id-ordered bans.
func (c *Client) ListChannelBans(ctx context.Context, channel string) (schema.ListChannelBansResponseV1, error) {
	if channel == "" {
		return schema.ListChannelBansResponseV1{}, errors.New("cli: ban channel must not be empty")
	}
	endpoint := c.resolve("v1", "channels", channel, "bans")
	var doc channelBansDoc
	if err := c.roundTrip(ctx, "list channel bans", http.MethodGet, endpoint, nil, &doc); err != nil {
		return schema.ListChannelBansResponseV1{}, err
	}
	result := doc.ListChannelBansResponseV1
	if err := result.Validate(); err != nil {
		return schema.ListChannelBansResponseV1{}, fmt.Errorf("cli: invalid channel ban list: %w", err)
	}
	return result, nil
}

// BanChannelMember bans a principal from a channel and removes its membership.
func (c *Client) BanChannelMember(ctx context.Context, channel string, principalID int64) error {
	id, err := moderationTarget(channel, principalID)
	if err != nil {
		return err
	}
	endpoint := c.resolve("v1", "channels", channel, "bans", id)
	return c.roundTrip(ctx, "ban channel member", http.MethodPut, endpoint, nil, nil)
}

// UnbanChannelMember lifts a channel ban without restoring membership.
func (c *Client) UnbanChannelMember(ctx context.Context, channel string, principalID int64) error {
	id, err := moderationTarget(channel, principalID)
	if err != nil {
		return err
	}
	endpoint := c.resolve("v1", "channels", channel, "bans", id)
	return c.roundTrip(ctx, "unban channel member", http.MethodDelete, endpoint, nil, nil)
}

// RemoveMessage replaces a visible message with a permanent tombstone.
func (c *Client) RemoveMessage(ctx context.Context, channel string, messageID int64) error {
	id, err := moderationTarget(channel, messageID)
	if err != nil {
		return err
	}
	endpoint := c.resolve("v2", "channels", channel, "messages", id)
	return c.roundTrip(ctx, "remove message", http.MethodDelete, endpoint, nil, nil)
}
