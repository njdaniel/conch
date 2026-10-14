package cli

import (
	"context"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// ListChannelMembers returns the members of channel, each with the name and kind
// the server knows them by.
func (c *Client) ListChannelMembers(ctx context.Context, channel string) (schema.ListChannelMembersResponseV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "members")
	var result schema.ListChannelMembersResponseV1
	if err := c.roundTrip(ctx, "list channel members", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListChannelMembersResponseV1{}, err
	}
	return result, nil
}
