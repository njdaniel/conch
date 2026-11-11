package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// SetReaction adds the caller's reaction to a message and returns its whole
// summary. The emoji may be any qualification accepted by the public API.
func (c *Client) SetReaction(ctx context.Context, channel string, messageID int64, emoji string) (schema.ReactionSummaryV1, error) {
	return c.writeReaction(ctx, channel, messageID, emoji, true)
}

// RemoveReaction removes the caller's own reaction. Removing one the caller
// does not hold is an idempotent no-op.
func (c *Client) RemoveReaction(ctx context.Context, channel string, messageID int64, emoji string) (schema.ReactionSummaryV1, error) {
	return c.writeReaction(ctx, channel, messageID, emoji, false)
}

func (c *Client) writeReaction(ctx context.Context, channel string, messageID int64, emoji string, set bool) (schema.ReactionSummaryV1, error) {
	key, err := reactionKey(channel, messageID, emoji)
	if err != nil {
		return schema.ReactionSummaryV1{}, err
	}
	method := http.MethodDelete
	what := "remove reaction"
	if set {
		method = http.MethodPut
		what = "set reaction"
	}
	endpoint := c.resolve("v2", "channels", channel, "messages", strconv.FormatInt(messageID, 10), "reactions", key)
	var result schema.ReactionSummaryV1
	if err := c.roundTrip(ctx, what, method, endpoint, nil, &result); err != nil {
		return schema.ReactionSummaryV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.ReactionSummaryV1{}, fmt.Errorf("cli: invalid reaction summary: %w", err)
	}
	return result, nil
}

// ReactionSummary returns all reactions on one message visible to the caller.
func (c *Client) ReactionSummary(ctx context.Context, channel string, messageID int64) (schema.ReactionSummaryV1, error) {
	if channel == "" || messageID <= 0 {
		return schema.ReactionSummaryV1{}, errors.New("cli: reaction channel must not be empty and message id must be positive")
	}
	endpoint := c.resolve("v2", "channels", channel, "messages", strconv.FormatInt(messageID, 10), "reactions")
	var result schema.ReactionSummaryV1
	if err := c.roundTrip(ctx, "read reactions", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ReactionSummaryV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.ReactionSummaryV1{}, fmt.Errorf("cli: invalid reaction summary: %w", err)
	}
	return result, nil
}

// ReactionSummaries hydrates the reactions of up to 100 distinct messages. An
// omitted id means an empty summary; callers replace, rather than merge, state.
func (c *Client) ReactionSummaries(ctx context.Context, channel string, messageIDs []int64) (schema.ReactionSummariesResponseV1, error) {
	if channel == "" || len(messageIDs) == 0 || len(messageIDs) > schema.MaxReactionBatchMessages {
		return schema.ReactionSummariesResponseV1{}, fmt.Errorf("cli: reaction batch needs 1 to %d message ids and a channel", schema.MaxReactionBatchMessages)
	}
	ids := make([]string, len(messageIDs))
	seen := make(map[int64]bool, len(messageIDs))
	for i, id := range messageIDs {
		if id <= 0 || seen[id] {
			return schema.ReactionSummariesResponseV1{}, errors.New("cli: reaction batch message ids must be positive and distinct")
		}
		seen[id] = true
		ids[i] = strconv.FormatInt(id, 10)
	}
	endpoint := c.resolve("v2", "channels", channel, "reactions")
	endpoint.RawQuery = url.Values{"messages": {strings.Join(ids, ",")}}.Encode()
	var result schema.ReactionSummariesResponseV1
	if err := c.roundTrip(ctx, "read reaction batch", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ReactionSummariesResponseV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.ReactionSummariesResponseV1{}, fmt.Errorf("cli: invalid reaction summaries: %w", err)
	}
	return result, nil
}

// ListReactors returns one principal-id-ordered page for one emoji. after is
// the previous page's next_after, or zero for the first page; limit zero uses
// the server's default.
func (c *Client) ListReactors(ctx context.Context, channel string, messageID int64, emoji string, after int64, limit int) (schema.ListReactorsResponseV1, error) {
	key, err := reactionKey(channel, messageID, emoji)
	if err != nil {
		return schema.ListReactorsResponseV1{}, err
	}
	if after < 0 || limit < 0 || limit > schema.MaxReactorLimit {
		return schema.ListReactorsResponseV1{}, errors.New("cli: reactor after must not be negative and limit must be 0 to 100")
	}
	endpoint := c.resolve("v2", "channels", channel, "messages", strconv.FormatInt(messageID, 10), "reactions", key)
	query := endpoint.Query()
	if after > 0 {
		query.Set("after", strconv.FormatInt(after, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	endpoint.RawQuery = query.Encode()
	var result schema.ListReactorsResponseV1
	if err := c.roundTrip(ctx, "list reactors", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListReactorsResponseV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.ListReactorsResponseV1{}, fmt.Errorf("cli: invalid reactor list: %w", err)
	}
	return result, nil
}

// ModerateReaction removes one principal's reaction, or all of their reactions
// when emoji is empty, through the public channel-manager routes.
func (c *Client) ModerateReaction(ctx context.Context, channel string, messageID, principalID int64, emoji string) (schema.ReactionSummaryV1, error) {
	if channel == "" || messageID <= 0 || principalID <= 0 {
		return schema.ReactionSummaryV1{}, errors.New("cli: moderation needs a channel and positive message and principal ids")
	}
	parts := []string{"v2", "channels", channel, "messages", strconv.FormatInt(messageID, 10)}
	if emoji == "" {
		parts = append(parts, "reactors", strconv.FormatInt(principalID, 10))
	} else {
		key, err := schema.CanonicalReactionKey(emoji)
		if err != nil {
			return schema.ReactionSummaryV1{}, fmt.Errorf("cli: invalid reaction: %w", err)
		}
		parts = append(parts, "reactions", key, strconv.FormatInt(principalID, 10))
	}
	var result schema.ReactionSummaryV1
	if err := c.roundTrip(ctx, "moderate reaction", http.MethodDelete, c.resolve(parts...), nil, &result); err != nil {
		return schema.ReactionSummaryV1{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.ReactionSummaryV1{}, fmt.Errorf("cli: invalid reaction summary: %w", err)
	}
	return result, nil
}

func reactionKey(channel string, messageID int64, emoji string) (string, error) {
	if channel == "" || messageID <= 0 {
		return "", errors.New("cli: reaction channel must not be empty and message id must be positive")
	}
	key, err := schema.CanonicalReactionKey(emoji)
	if err != nil {
		return "", fmt.Errorf("cli: invalid reaction: %w", err)
	}
	return key, nil
}

// ReactionsUnsupported reports whether err is an unversioned 404/405 from a
// server whose mux predates reactions. A schema.Error 404 is an ordinary access
// refusal and deliberately does not match.
func ReactionsUnsupported(err error) bool { return routeMissing(err) }
