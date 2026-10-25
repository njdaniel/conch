package cli

import (
	"context"
	"net/http"
	"strconv"

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

// ChangePrincipalRole sets a principal's role and, when request.Owner is not
// nil, their owner mark.
func (c *Client) ChangePrincipalRole(ctx context.Context, principalID int64, request schema.RoleChangeRequestV1) (schema.RoleChangeResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(principalID, 10), "role")
	var result schema.RoleChangeResponseV1
	if err := c.roundTrip(ctx, "change principal role", http.MethodPut, endpoint, request, &result); err != nil {
		return schema.RoleChangeResponseV1{}, err
	}
	return result, nil
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
