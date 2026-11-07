package cli

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// ListChannelMembers returns the members of channel, each with the name and kind
// the server knows them by.
func (c *Client) ListChannelMembers(ctx context.Context, channel string) (schema.ListChannelMembersResponseV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "members")
	var result channelMembersDoc
	if err := c.roundTrip(ctx, "list channel members", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListChannelMembersResponseV1{}, err
	}
	return result.ListChannelMembersResponseV1, nil
}

// ChannelAuthors names the people who wrote in channel and may no longer be on
// its member list (GET /v1/channels/{channel}/authors?ids=, design §13.5): the
// ids go in batches of at most schema.MaxChannelAuthorIDs. What it returns is
// only what the server named for an id that was asked: a row for another id,
// or a second row for one, is dropped. A batch that fails names nobody; the
// first failure is returned beside what the other batches named, so a caller
// shows #<id> for the rest and still sees why.
func (c *Client) ChannelAuthors(ctx context.Context, channel string, ids []int64) ([]schema.ChannelAuthorV1, error) {
	wanted := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !slices.Contains(wanted, id) {
			wanted = append(wanted, id)
		}
	}
	var named []schema.ChannelAuthorV1
	var first error
	seen := map[int64]bool{}
	for start := 0; start < len(wanted); start += schema.MaxChannelAuthorIDs {
		batch := wanted[start:min(start+schema.MaxChannelAuthorIDs, len(wanted))]
		parts := make([]string, len(batch))
		for i, id := range batch {
			parts[i] = strconv.FormatInt(id, 10)
		}
		endpoint := c.resolve("v1", "channels", channel, "authors")
		query := endpoint.Query()
		query.Set("ids", strings.Join(parts, ","))
		endpoint.RawQuery = query.Encode()
		var result channelAuthorsDoc
		if err := c.roundTrip(ctx, "name channel authors", http.MethodGet, endpoint, nil, &result); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		for _, a := range result.Authors {
			if slices.Contains(batch, a.PrincipalID) && !seen[a.PrincipalID] {
				seen[a.PrincipalID] = true
				named = append(named, a)
			}
		}
	}
	return named, first
}

// ChangePrincipalRole sets a principal's role and, when request.Owner is not
// nil, their owner mark.
func (c *Client) ChangePrincipalRole(ctx context.Context, principalID int64, request schema.RoleChangeRequestV1) (schema.RoleChangeResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "role")
	var result roleChangeDoc
	if err := c.roundTrip(ctx, "change principal role", http.MethodPut, endpoint, request, &result); err != nil {
		return schema.RoleChangeResponseV1{}, err
	}
	return result.RoleChangeResponseV1, nil
}

// AddChannelMember adds the named person to channel and returns the membership
// the server created or already had.
func (c *Client) AddChannelMember(ctx context.Context, channel, name string) (schema.ChannelMemberV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "members")
	request := schema.AddChannelMemberRequestV1{Name: name}
	var result schema.ChannelMemberV1
	if err := c.roundTrip(ctx, "add channel member by name", http.MethodPost, endpoint, request, &result); err != nil {
		return schema.ChannelMemberV1{}, err
	}
	return result, nil
}

// PutChannelMemberManage grants or revokes channel management for principalID.
func (c *Client) PutChannelMemberManage(ctx context.Context, channel string, principalID int64, manages bool) error {
	endpoint := c.resolve("v1", "channels", channel, "members", strconv.FormatInt(principalID, 10))
	return c.roundTrip(ctx, "change channel manager", http.MethodPut, endpoint, schema.PutChannelMemberRequestV1{Manages: &manages}, nil)
}
