package cli

import (
	"context"
	"fmt"

	"github.com/njdaniel/conch/pkg/schema"
)

const previewAsHeader = "Conch-Preview-As"

func (c *Client) Preview(ctx context.Context, memberID int64) (schema.PreviewResponseV1, error) {
	var result previewDoc
	if err := c.roundTripPreview(ctx, "preview", c.resolve("v1", "preview"), &result, memberID); err != nil {
		return schema.PreviewResponseV1{}, err
	}
	preview := result.PreviewResponseV1
	if err := preview.Validate(); err != nil {
		return schema.PreviewResponseV1{}, fmt.Errorf("cli: invalid preview: %w", err)
	}
	if preview.ID != memberID || preview.Simulation != "" {
		return schema.PreviewResponseV1{}, fmt.Errorf("cli: invalid preview: the answer is not for member %d", memberID)
	}
	return preview, nil
}

func (c *Client) ListChannelsPreview(ctx context.Context, memberID int64) (schema.ListChannelsResponse, error) {
	return c.listChannels(ctx, memberID)
}

func (c *Client) ChannelDirectoryPreview(ctx context.Context, all bool, memberID int64) (schema.ChannelDirectoryResponseV1, error) {
	return c.channelDirectory(ctx, all, memberID)
}

func (c *Client) DirectoryMembersPreview(ctx context.Context, after int64, limit int, memberID int64) (schema.ServerMembersResponseV1, error) {
	return c.directoryMembers(ctx, after, limit, memberID)
}
