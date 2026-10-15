package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/njdaniel/conch/pkg/schema"
)

// membersClient carries the adapter's credential on the member list. Like
// the relay's upstream it does not follow a redirect, which would send the
// credential to wherever the redirect points.
var membersClient = &http.Client{
	Timeout:       upstreamTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// members reads GET /v1/channels/{channel}/members with the adapter's own
// credential. The server gives an agent the list on the same grant as reading
// the channel, which the adapter needs anyway.
func (m mcpAdapter) members(ctx context.Context, channel string) ([]schema.ChannelMemberV1, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.server+"/v1/channels/"+url.PathEscape(channel)+"/members", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := membersClient.Do(req) // #nosec G107 -- the operator configures the server address
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("members: server answered %s", resp.Status)
	}
	var list schema.ListChannelMembersResponseV1
	// Bounded like the relay's reads from conchd (maxUpstreamBody).
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("members: decode: %w", err)
	}
	return list.Members, nil
}
