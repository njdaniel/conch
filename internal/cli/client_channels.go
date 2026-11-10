package cli

import (
	"context"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community model's channel, member-link and directory routes (ADR-013,
// issue #994). Each is one request to a public route; the server decides who
// may make it.

// ChannelDirectory lists the public channels and the caller's private ones;
// with all, an operator's listing of every channel with its member count
// (?scope=all, manage_channels).
func (c *Client) ChannelDirectory(ctx context.Context, all bool) (schema.ChannelDirectoryResponseV1, error) {
	return c.channelDirectory(ctx, all, 0)
}

func (c *Client) channelDirectory(ctx context.Context, all bool, previewAs int64) (schema.ChannelDirectoryResponseV1, error) {
	endpoint := c.resolve("v1", "directory", "channels")
	if all {
		endpoint.RawQuery = "scope=all"
	}
	var result channelDirectoryDoc
	var err error
	if previewAs > 0 {
		err = c.roundTripPreview(ctx, "list channels", endpoint, &result, previewAs)
	} else {
		err = c.roundTrip(ctx, "list channels", http.MethodGet, endpoint, nil, &result)
	}
	if err != nil {
		return schema.ChannelDirectoryResponseV1{}, err
	}
	return result.ChannelDirectoryResponseV1, nil
}

// JoinChannel joins a public channel, or says the caller is already in it.
func (c *Client) JoinChannel(ctx context.Context, channel string) (schema.JoinChannelResponseV1, error) {
	endpoint := c.resolve("v1", "channels", channel, "join")
	var result schema.JoinChannelResponseV1
	if err := c.roundTrip(ctx, "join channel", http.MethodPost, endpoint, nil, &result); err != nil {
		return schema.JoinChannelResponseV1{}, err
	}
	return result, nil
}

// LeaveChannel leaves a channel the caller is in.
func (c *Client) LeaveChannel(ctx context.Context, channel string) error {
	endpoint := c.resolve("v1", "channels", channel, "leave")
	return c.roundTrip(ctx, "leave channel", http.MethodPost, endpoint, nil, nil)
}

// CreateChannelV1 makes a channel of the given visibility; the caller is its
// first member and its manager.
func (c *Client) CreateChannelV1(ctx context.Context, request schema.CreateChannelRequestV1) (schema.CreateChannelResponseV1, error) {
	var result schema.CreateChannelResponseV1
	if err := c.roundTrip(ctx, "create channel", http.MethodPost, c.resolve("v1", "channels"), request, &result); err != nil {
		return schema.CreateChannelResponseV1{}, err
	}
	return result, nil
}

// PutChannelSettings changes a channel's visibility or default mark. The
// channel is addressed by id.
func (c *Client) PutChannelSettings(ctx context.Context, id int64, request schema.ChannelSettingsRequestV1) (schema.ChannelSettingsResponseV1, error) {
	endpoint := c.resolve("v1", "channels", strconv.FormatInt(id, 10), "settings")
	var result schema.ChannelSettingsResponseV1
	if err := c.roundTrip(ctx, "change channel settings", http.MethodPut, endpoint, request, &result); err != nil {
		return schema.ChannelSettingsResponseV1{}, err
	}
	return result, nil
}

// DirectoryMembers returns one page of the community's members after the
// member with id after (0 for the first page).
func (c *Client) DirectoryMembers(ctx context.Context, after int64, limit int) (schema.ServerMembersResponseV1, error) {
	return c.directoryMembers(ctx, after, limit, 0)
}

func (c *Client) directoryMembers(ctx context.Context, after int64, limit int, previewAs int64) (schema.ServerMembersResponseV1, error) {
	endpoint := c.resolve("v1", "directory", "members")
	query := endpoint.Query()
	if after > 0 {
		query.Set("after", strconv.FormatInt(after, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	endpoint.RawQuery = query.Encode()
	var result directoryMembersDoc
	var err error
	if previewAs > 0 {
		err = c.roundTripPreview(ctx, "list the community", endpoint, &result, previewAs)
	} else {
		err = c.roundTrip(ctx, "list the community", http.MethodGet, endpoint, nil, &result)
	}
	if err != nil {
		return schema.ServerMembersResponseV1{}, err
	}
	return result.ServerMembersResponseV1, nil
}

// SetVoicePermission turns a person's voice on or off.
func (c *Client) SetVoicePermission(ctx context.Context, id int64, allowed bool) (schema.VoicePermissionResponseV1, error) {
	endpoint := c.resolve("v1", "principals", strconv.FormatInt(id, 10), "voice-permission")
	var result schema.VoicePermissionResponseV1
	if err := c.roundTrip(ctx, "set voice permission", http.MethodPut, endpoint, schema.VoicePermissionRequestV1{VoiceAllowed: allowed}, &result); err != nil {
		return schema.VoicePermissionResponseV1{}, err
	}
	return result, nil
}

// CreateMemberLink makes a v2 member link. The response carries the link's
// code, which the server shows only once.
func (c *Client) CreateMemberLink(ctx context.Context, request schema.CreateInviteRequestV2) (schema.CreateInviteResponseV2, error) {
	var result schema.CreateInviteResponseV2
	if err := c.roundTrip(ctx, "create member link", http.MethodPost, c.resolve("v2", "invites"), request, &result); err != nil {
		return schema.CreateInviteResponseV2{}, err
	}
	return result, nil
}

// ListMemberLinks returns the caller's own member links, or every link for an
// operator, newest first.
func (c *Client) ListMemberLinks(ctx context.Context) (schema.ListInvitesResponseV2, error) {
	var result memberLinksDoc
	if err := c.roundTrip(ctx, "list member links", http.MethodGet, c.resolve("v2", "invites"), nil, &result); err != nil {
		return schema.ListInvitesResponseV2{}, err
	}
	return result.ListInvitesResponseV2, nil
}

// MemberLinkJoiners returns the members who joined through a link.
func (c *Client) MemberLinkJoiners(ctx context.Context, id int64) (schema.InviteJoinersResponseV1, error) {
	endpoint := c.resolve("v2", "invites", strconv.FormatInt(id, 10), "joiners")
	var result joinersDoc
	if err := c.roundTrip(ctx, "list link joiners", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.InviteJoinersResponseV1{}, err
	}
	return result.InviteJoinersResponseV1, nil
}
